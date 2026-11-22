package service

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/scheduler"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/setting"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func setupChannelSelectAutoGroupsTest(t *testing.T) *gorm.DB {
	t.Helper()

	originalDB := model.DB
	originalMemoryCacheEnabled := common.MemoryCacheEnabled
	originalRetryTimes := common.RetryTimes
	originalAutoGroups := setting.AutoGroups2JsonString()
	originalUsableGroups := setting.UserUsableGroups2JSONString()
	originalGroupRatios := ratio_setting.GroupRatio2JSONString()
	originalMaxTokenAutoGroups := setting.GetMaxTokenAutoGroups()

	dsn := fmt.Sprintf("file:%s?mode=memory&cache=shared", strings.ReplaceAll(t.Name(), "/", "_"))
	db, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{})
	require.NoError(t, err)
	require.NoError(t, db.AutoMigrate(&model.Channel{}, &model.Ability{}))
	require.NoError(t, model.AutoMigrateUpstreamCredentialSchema(db))
	require.NoError(t, model.AutoMigrateChannelLimitSchema(db))
	model.DB = db
	common.MemoryCacheEnabled = true
	common.RetryTimes = 0

	require.NoError(t, setting.UpdateAutoGroupsByJsonString(`[]`))
	require.NoError(t, setting.UpdateUserUsableGroupsByJSONString(`{"default":"Default","vip":"VIP"}`))
	require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(`{"default":1,"vip":2}`))
	require.NoError(t, setting.UpdateMaxTokenAutoGroups("2"))

	t.Cleanup(func() {
		model.DB = originalDB
		common.MemoryCacheEnabled = originalMemoryCacheEnabled
		common.RetryTimes = originalRetryTimes
		require.NoError(t, setting.UpdateAutoGroupsByJsonString(originalAutoGroups))
		require.NoError(t, setting.UpdateUserUsableGroupsByJSONString(originalUsableGroups))
		require.NoError(t, ratio_setting.UpdateGroupRatioByJSONString(originalGroupRatios))
		require.NoError(t, setting.UpdateMaxTokenAutoGroups(fmt.Sprintf("%d", originalMaxTokenAutoGroups)))

		if originalMemoryCacheEnabled && originalDB != nil &&
			originalDB.Migrator().HasTable(&model.Channel{}) && originalDB.Migrator().HasTable(&model.Ability{}) {
			model.InitChannelCache()
		}
		sqlDB, err := db.DB()
		if err == nil {
			require.NoError(t, sqlDB.Close())
		}
	})

	return db
}

func createChannelSelectAutoGroupsChannel(t *testing.T, db *gorm.DB, id int, group, modelName string) {
	t.Helper()
	priority := int64(0)
	weight := uint(100)
	require.NoError(t, db.Create(&model.Channel{
		Id:       id,
		Type:     constant.ChannelTypeOpenAI,
		Key:      fmt.Sprintf("key-%d", id),
		Status:   common.ChannelStatusEnabled,
		Name:     fmt.Sprintf("channel-%d", id),
		Weight:   &weight,
		Models:   modelName,
		Group:    group,
		Priority: &priority,
	}).Error)
	require.NoError(t, db.Create(&model.Ability{
		Group:     group,
		Model:     modelName,
		ChannelId: id,
		Enabled:   true,
		Priority:  &priority,
		Weight:    weight,
	}).Error)
}

func TestCacheGetRandomSatisfiedChannelUsesTokenAutoGroupsWhenGlobalAutoIsEmpty(t *testing.T) {
	db := setupChannelSelectAutoGroupsTest(t)
	const modelName = "auto-groups-runtime-model"
	createChannelSelectAutoGroupsChannel(t, db, 2101, "vip", modelName)
	createChannelSelectAutoGroupsChannel(t, db, 2102, "default", modelName)
	model.InitChannelCache()

	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	defer EndSchedulingRequest(ctx)
	common.SetContextKey(ctx, constant.ContextKeyUserGroup, "default")
	common.SetContextKey(ctx, constant.ContextKeyTokenAutoGroups, []string{"vip", "default"})
	common.SetContextKey(ctx, constant.ContextKeyTokenCrossGroupRetry, true)

	retry := 0
	param := &RetryParam{
		Ctx:         ctx,
		TokenGroup:  "auto",
		ModelName:   modelName,
		RequestPath: "/v1/chat/completions",
		Retry:       &retry,
	}

	first, selectedGroup, err := CacheGetRandomSatisfiedChannel(param)
	require.NoError(t, err)
	require.NotNil(t, first)
	assert.Equal(t, 2101, first.Id)
	assert.Equal(t, "vip", selectedGroup)
	assert.Equal(t, "vip", common.GetContextKeyString(ctx, constant.ContextKeyAutoGroup))
	assert.Empty(t, setting.GetAutoGroups(), "the selection must not depend on the global Auto list")

	param.IncreaseRetry()
	second, selectedGroup, err := CacheGetRandomSatisfiedChannel(param)
	require.NoError(t, err)
	require.NotNil(t, second)
	assert.Equal(t, 2102, second.Id)
	assert.Equal(t, "default", selectedGroup)
	assert.Equal(t, "default", common.GetContextKeyString(ctx, constant.ContextKeyAutoGroup))
}

func routingTestContext(t *testing.T) *gin.Context {
	t.Helper()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	common.SetContextKey(c, constant.ContextKeyUserGroup, "default")
	t.Cleanup(func() { EndSchedulingRequest(c) })
	return c
}

func TestAutoGroupLocalAdmissionAndCapacityOutcomes(t *testing.T) {
	db := setupChannelSelectAutoGroupsTest(t)
	const name = "auto-capacity"
	setSchedulingTestConfig(t, []SchedulingConfig{
		{Group: "vip", Model: name, Enabled: true, TargetSuccessRate: .95, TargetTTFTMS: 3000, DefaultCapacity: 1},
		{Group: "default", Model: name, Enabled: true, TargetSuccessRate: .95, TargetTTFTMS: 3000, DefaultCapacity: 1},
	})
	for i, group := range []string{"vip", "vip", "default"} {
		createChannelSelectAutoGroupsChannel(t, db, 6001+i, group, name)
	}
	require.NoError(t, db.Model(&model.Channel{}).Where("id = ?", 6001).Update("priority", 100).Error)
	model.InitChannelCache()
	require.NoError(t, SyncSchedulingConfig())
	autoContext := func() *gin.Context {
		c := routingTestContext(t)
		common.SetContextKey(c, constant.ContextKeyTokenAutoGroups, []string{"vip", "default"})
		common.SetContextKey(c, constant.ContextKeyTokenCrossGroupRetry, true)
		return c
	}
	selectAuto := func(c *gin.Context) (*model.Channel, string, *ChannelSelectError) {
		// The relay constructs a new retry parameter after Distribute has reserved a channel.
		return SelectChannelForRequest(c, name, &RetryParam{Ctx: c, TokenGroup: "auto", ModelName: name})
	}
	first := autoContext()
	selected, _, selectErr := selectAuto(first)
	require.Nil(t, selectErr)
	require.Equal(t, 6001, selected.Id)
	AppendUsedChannel(first, selected.Id)
	FinishSchedulingAttempt(first, nil, types.NewOpenAIError(errors.New("local limit"), types.ErrorCode("channel_limit_unavailable"), http.StatusServiceUnavailable))
	first.Set("channel_local_refusals", 1)
	selected, group, selectErr := selectAuto(first)
	require.Nil(t, selectErr)
	require.Equal(t, "vip", group)
	require.Equal(t, 6002, selected.Id)
	EndSchedulingRequest(first)

	for _, id := range []int{6001, 6002} {
		channel, err := model.CacheGetChannelForRouting(id)
		require.NoError(t, err)
		require.NoError(t, ReserveSchedulingChannel(routingTestContext(t), channel, "vip", name))
	}
	fallback := autoContext()
	selected, group, selectErr = selectAuto(fallback)
	require.Nil(t, selectErr)
	require.Equal(t, "default", group)
	require.Equal(t, 6003, selected.Id)
	exhausted := autoContext() // The successful fallback still holds the last lease.
	selected, _, selectErr = selectAuto(exhausted)
	require.Nil(t, selected)
	require.NotNil(t, selectErr)
	require.Equal(t, http.StatusServiceUnavailable, selectErr.StatusCode)
	exhausted.AbortWithStatus(selectErr.StatusCode)
	EndSchedulingRequest(exhausted)
	// A preparatory selection can fail before an HTTP handler accepts the
	// request. Its unwritten success status must not look like a capacity 503.
	preflight := autoContext()
	_, _, selectErr = selectAuto(preflight)
	require.NotNil(t, selectErr)
	preflight.Status(http.StatusNoContent)
	EndSchedulingRequest(preflight)
	EndSchedulingRequest(fallback)
	vip := scheduler.Default.Snapshot(scheduler.Key{Group: "vip", Model: name}, nil, false).Summary
	assert.EqualValues(t, 1, vip.CapacitySkips30m)
	assert.Zero(t, vip.CapacityExhausted30m)
	last := scheduler.Default.Snapshot(scheduler.Key{Group: "default", Model: name}, nil, false).Summary
	assert.Zero(t, last.CapacitySkips30m)
	assert.EqualValues(t, 1, last.CapacityExhausted30m)
}

func TestCapacityCountsFinalRoutingOutcomeAcrossRetries(t *testing.T) {
	for _, succeeds := range []bool{true, false} {
		t.Run(fmt.Sprintf("retry_succeeds_%t", succeeds), func(t *testing.T) {
			db := setupChannelSelectAutoGroupsTest(t)
			const name = "capacity-retries"
			setSchedulingTestConfig(t, []SchedulingConfig{{Group: "default", Model: name, Enabled: true, TargetSuccessRate: .95, TargetTTFTMS: 3000, DefaultCapacity: 1}})
			for _, id := range []int{6110, 6111, 6112} {
				createChannelSelectAutoGroupsChannel(t, db, id, "default", name)
			}
			model.InitChannelCache()
			require.NoError(t, SyncSchedulingConfig())
			var lastLease *gin.Context
			for _, id := range []int{6110, 6112} {
				channel, err := model.CacheGetChannelForRouting(id)
				require.NoError(t, err)
				lastLease = routingTestContext(t)
				require.NoError(t, ReserveSchedulingChannel(lastLease, channel, "default", name))
			}
			common.RetryTimes = 1
			request := routingTestContext(t)
			param := &RetryParam{Ctx: request, TokenGroup: "default", ModelName: name}
			channel, _, selectErr := SelectChannelForRequest(request, name, param)
			require.Nil(t, selectErr)
			require.Equal(t, 6111, channel.Id)
			info := &relaycommon.RelayInfo{OriginModelName: name, UsingGroup: "default"}
			AppendUsedChannel(request, channel.Id)
			StartSchedulingAttempt(request, info)
			FinishSchedulingAttempt(request, info, types.NewOpenAIError(errors.New("upstream failed"), types.ErrorCodeBadResponseStatusCode, http.StatusBadGateway))
			key := scheduler.Key{Group: "default", Model: name}
			assert.Zero(t, scheduler.Default.Snapshot(key, nil, false).Summary.CapacitySkips30m, "an intermediate selection cannot settle the request")
			if succeeds {
				EndSchedulingRequest(lastLease)
			}
			param.IncreaseRetry()
			channel, _, selectErr = SelectChannelForRequest(request, name, param)
			if succeeds {
				require.Nil(t, selectErr)
				require.Equal(t, 6112, channel.Id)
				StartSchedulingAttempt(request, info)
				FinishSchedulingAttempt(request, info, nil)
			} else {
				require.NotNil(t, selectErr)
				require.Equal(t, http.StatusServiceUnavailable, selectErr.StatusCode)
				request.AbortWithStatus(selectErr.StatusCode)
			}
			EndSchedulingRequest(request)
			EndSchedulingRequest(request)
			summary := scheduler.Default.Snapshot(key, nil, false).Summary
			if succeeds {
				assert.EqualValues(t, 1, summary.CapacitySkips30m)
				assert.Zero(t, summary.CapacityExhausted30m)
			} else {
				assert.Zero(t, summary.CapacitySkips30m)
				assert.EqualValues(t, 1, summary.CapacityExhausted30m)
			}
		})
	}
}

func TestSchedulingAttemptsPreserveFailuresAndMeasureUsefulOutput(t *testing.T) {
	db := setupChannelSelectAutoGroupsTest(t)
	common.RetryTimes = 2
	previousEngine := scheduler.Default
	scheduler.Default = scheduler.New()
	t.Cleanup(func() { scheduler.Default = previousEngine })
	const modelName = "scheduler-attempt-model"
	createChannelSelectAutoGroupsChannel(t, db, 3101, "default", modelName)
	createChannelSelectAutoGroupsChannel(t, db, 3102, "default", modelName)
	createChannelSelectAutoGroupsChannel(t, db, 3103, "default", modelName)
	require.NoError(t, db.Model(&model.Channel{}).Where("id = ?", 3101).Update("priority", 100).Error)
	require.NoError(t, db.Model(&model.Channel{}).Where("id = ?", 3103).Update("priority", 50).Error)
	model.InitChannelCache()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
	ObserveSchedulingResponse(c)
	defer EndSchedulingRequest(c)
	param := &RetryParam{Ctx: c, TokenGroup: "default", ModelName: modelName, Retry: common.GetPointer(0)}
	first, _, err := SelectChannelForRequest(c, modelName, param)
	require.Nil(t, err)
	require.NotNil(t, first)
	assert.Equal(t, 3101, first.Id, "cold start retains native priority")
	info := &relaycommon.RelayInfo{UsingGroup: "default", OriginModelName: modelName, IsStream: true}
	StartSchedulingAttempt(c, info)
	AppendUsedChannel(c, first.Id)
	FinishSchedulingAttempt(c, info, types.NewOpenAIError(errors.New("upstream down"), types.ErrorCodeDoRequestFailed, 502))
	key := scheduler.Key{Group: "default", Model: modelName}
	for index := range 49 {
		request := scheduler.Default.BeginRequest(key, fmt.Sprintf("activation-%d", index), false)
		scheduler.Default.EndRequest(request)
	}
	require.True(t, scheduler.Default.IsActive(key))
	model.CacheUpdateChannelStatus(3103, common.ChannelStatusManuallyDisabled)
	param.SetRetry(1)
	second, _, err := SelectChannelForRequest(c, modelName, param)
	require.Nil(t, err)
	require.NotNil(t, second)
	assert.Equal(t, 3102, second.Id, "retry excludes prior attempts and a newly disabled higher-priority channel")
	StartSchedulingAttempt(c, info)
	AppendUsedChannel(c, second.Id)
	_, writeErr := c.Writer.WriteString(": heartbeat\n\ndata: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n")
	require.NoError(t, writeErr)
	assert.Nil(t, schedulingState(c).ttft)
	assert.False(t, SchedulingRetryAllowed(c), "a heartbeat commits the response even without useful first output")
	_, writeErr = c.Writer.WriteString("data: {\"choices\":[{\"delta\":{\"content\":")
	require.NoError(t, writeErr)
	assert.Nil(t, schedulingState(c).ttft)
	_, writeErr = c.Writer.WriteString("\"answer\"}}]}\n\n")
	require.NoError(t, writeErr)
	require.NotNil(t, schedulingState(c).ttft)
	assert.False(t, SchedulingRetryAllowed(c), "a response cannot be replayed after output")
	FinishSchedulingAttempt(c, info, nil)
	EndSchedulingRequest(c)
	channels := []scheduler.Candidate{SchedulerCandidate(first), SchedulerCandidate(second)}
	snapshot := scheduler.Default.Snapshot(key, channels, true)
	assert.Equal(t, int64(50), snapshot.Summary.Requests30m, "retry does not increment original requests")
	assert.Equal(t, int64(2), snapshot.Summary.Attempts30m)
	assert.Equal(t, int64(1), snapshot.Summary.Successes30m)
	assert.Zero(t, snapshot.Summary.InFlight)
	assert.Equal(t, int64(1), snapshot.Channels[0].Window5m.Failures)
	assert.Equal(t, int64(1), snapshot.Channels[1].Window5m.LatencySamples)
	nonStream := scheduler.Default.Snapshot(key, channels, false)
	assert.Zero(t, nonStream.Channels[1].Window5m.LatencySamples, "StartAttempt uses actual stream mode")
}

func TestSchedulingReservationsAndClientCancellationReleaseSharedCapacity(t *testing.T) {
	db := setupChannelSelectAutoGroupsTest(t)
	previousEngine := scheduler.Default
	scheduler.Default = scheduler.New()
	t.Cleanup(func() { scheduler.Default = previousEngine })
	const modelName = "scheduler-capacity-model"
	createChannelSelectAutoGroupsChannel(t, db, 3201, "default", modelName)
	require.NoError(t, db.Model(&model.Channel{}).Where("id = ?", 3201).Update("group", "default,vip").Error)
	require.NoError(t, db.Create(&model.Ability{Group: "vip", Model: modelName, ChannelId: 3201, Enabled: true, Weight: 100}).Error)
	model.InitChannelCache()
	key := scheduler.Key{Group: "default", Model: modelName}
	capacity := 1
	require.NoError(t, scheduler.Default.SetOverride(key, 3201, scheduler.Override{Capacity: &capacity}))
	channel, lookupErr := model.CacheGetChannelForRouting(3201)
	require.NoError(t, lookupErr)
	first, _ := gin.CreateTestContext(httptest.NewRecorder())
	first.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	defer EndSchedulingRequest(first)
	require.NoError(t, ReserveSchedulingChannel(first, channel, "default", modelName))
	second, _ := gin.CreateTestContext(httptest.NewRecorder())
	second.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	defer EndSchedulingRequest(second)
	assert.ErrorIs(t, ReserveSchedulingChannel(second, channel, "vip", modelName), scheduler.ErrNoEligibleChannel)
	EndSchedulingRequest(first)
	unused := scheduler.Default.Snapshot(key, []scheduler.Candidate{SchedulerCandidate(channel)}, false)
	assert.Zero(t, unused.Summary.Requests30m)
	assert.Zero(t, unused.Channels[0].Dispatches30m)
	require.NoError(t, ReserveSchedulingChannel(second, channel, "vip", modelName))
	info := &relaycommon.RelayInfo{UsingGroup: "vip", OriginModelName: modelName}
	StartSchedulingAttempt(second, info)
	ctx, cancel := context.WithCancel(second.Request.Context())
	second.Request = second.Request.WithContext(ctx)
	cancel()
	FinishSchedulingAttempt(second, info, types.NewOpenAIError(context.Canceled, types.ErrorCodeDoRequestFailed, 502))
	EndSchedulingRequest(second)
	cancelled := scheduler.Default.Snapshot(scheduler.Key{Group: "vip", Model: modelName}, []scheduler.Candidate{SchedulerCandidate(channel)}, false)
	assert.Zero(t, cancelled.Summary.InFlight)
	assert.Zero(t, cancelled.Summary.Attempts30m, "caller cancellation is not a channel fault")
	assert.Zero(t, cancelled.Summary.Requests30m, "caller cancellation does not count towards activation")
	assert.Nil(t, cancelled.Summary.AvgTTFTMS5m, "missing first output is not zero milliseconds")
}

func TestSchedulingCompleteResponseSurvivesClientClosingAfterRead(t *testing.T) {
	db := setupChannelSelectAutoGroupsTest(t)
	previousEngine := scheduler.Default
	scheduler.Default = scheduler.New()
	t.Cleanup(func() { scheduler.Default = previousEngine })
	const modelName = "scheduler-completed-response"
	createChannelSelectAutoGroupsChannel(t, db, 3301, "default", modelName)
	model.InitChannelCache()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(ctx)
	ObserveSchedulingResponse(c)
	defer EndSchedulingRequest(c)
	channel, err := model.CacheGetChannelForRouting(3301)
	require.NoError(t, err)
	require.NoError(t, ReserveSchedulingChannel(c, channel, "default", modelName))
	info := &relaycommon.RelayInfo{UsingGroup: "default", OriginModelName: modelName, OverviewAttempt: &relaycommon.OverviewAttempt{Sent: true, DispatchedAt: time.Now()}}
	StartSchedulingAttempt(c, info)
	_, err = c.Writer.WriteString(`{"choices":[{"message":{"content":"complete"}}]}`)
	require.NoError(t, err)
	cancel()
	FinishSchedulingAttempt(c, info, nil)
	FinishOverviewAttempt(c, info, nil)
	assert.True(t, info.OverviewAttempt.Success, "daily health must preserve a complete response after the caller closes")
	assert.False(t, info.OverviewAttempt.Failure)
	snapshot := scheduler.Default.Snapshot(scheduler.Key{Group: "default", Model: modelName}, []scheduler.Candidate{SchedulerCandidate(channel)}, false)
	assert.Equal(t, int64(1), snapshot.Summary.Successes30m)
	assert.Equal(t, int64(1), snapshot.Summary.Requests30m)
	assert.Zero(t, snapshot.Summary.InFlight)
}

func TestSchedulingColdOverrideRetainsNativeRetryPriority(t *testing.T) {
	db := setupChannelSelectAutoGroupsTest(t)
	common.RetryTimes = 1
	previousEngine := scheduler.Default
	scheduler.Default = scheduler.New()
	t.Cleanup(func() { scheduler.Default = previousEngine })
	const modelName = "scheduler-cold-priority"
	for _, id := range []int{3401, 3402, 3403} {
		createChannelSelectAutoGroupsChannel(t, db, id, "default", modelName)
	}
	require.NoError(t, db.Model(&model.Channel{}).Where("id IN ?", []int{3401, 3402}).Update("priority", 100).Error)
	model.InitChannelCache()
	common.OptionMapRWMutex.Lock()
	previousRaw := common.OptionMap[schedulingOptionKey]
	if common.OptionMap == nil {
		common.OptionMap = make(map[string]string)
	}
	common.OptionMap[schedulingOptionKey] = `[{"group":"default","model":"scheduler-cold-priority","enabled":true,"target_success_rate":0.95,"target_ttft_ms":3000,"default_capacity":100,"channel_overrides":[{"channel_id":3401,"weight":200}]}]`
	common.OptionMapRWMutex.Unlock()
	t.Cleanup(func() {
		common.OptionMapRWMutex.Lock()
		common.OptionMap[schedulingOptionKey] = previousRaw
		common.OptionMapRWMutex.Unlock()
		require.NoError(t, SyncSchedulingConfig())
	})
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	defer EndSchedulingRequest(c)
	param := &RetryParam{Ctx: c, TokenGroup: "default", ModelName: modelName, Retry: common.GetPointer(0)}
	first, _, selectErr := SelectChannelForRequest(c, modelName, param)
	require.Nil(t, selectErr)
	require.NotNil(t, first)
	assert.Equal(t, int64(100), first.GetPriority())
	info := &relaycommon.RelayInfo{UsingGroup: "default", OriginModelName: modelName}
	StartSchedulingAttempt(c, info)
	AppendUsedChannel(c, first.Id)
	FinishSchedulingAttempt(c, info, types.NewOpenAIError(errors.New("upstream down"), types.ErrorCodeDoRequestFailed, 502))
	param.SetRetry(1)
	second, _, selectErr := SelectChannelForRequest(c, modelName, param)
	require.Nil(t, selectErr)
	require.NotNil(t, second)
	assert.Equal(t, 3403, second.Id, "cold retry moves to the next configured priority even when the previous tier has remaining candidates")
}

func setSchedulingTestConfig(t *testing.T, entries []SchedulingConfig) {
	t.Helper()
	encoded, err := common.Marshal(entries)
	require.NoError(t, err)
	previousEngine := scheduler.Default
	scheduler.Default = scheduler.New()
	common.OptionMapRWMutex.Lock()
	previousRaw := common.OptionMap[schedulingOptionKey]
	if common.OptionMap == nil {
		common.OptionMap = make(map[string]string)
	}
	common.OptionMap[schedulingOptionKey] = string(encoded)
	common.OptionMapRWMutex.Unlock()
	t.Cleanup(func() {
		scheduler.Default = previousEngine
		common.OptionMapRWMutex.Lock()
		common.OptionMap[schedulingOptionKey] = previousRaw
		common.OptionMapRWMutex.Unlock()
	})
}

func TestSupplierRoutingScopesPriorityAndRetriesToIndependentPools(t *testing.T) {
	db := setupChannelSelectAutoGroupsTest(t)
	require.NoError(t, db.AutoMigrate(&model.SupplierAccount{}))
	const modelName = "supplier-routing-model"
	setSchedulingTestConfig(t, []SchedulingConfig{{Group: "default", Model: modelName, Enabled: false, TargetSuccessRate: .95, TargetTTFTMS: 3000, DefaultCapacity: 100}})
	for _, account := range []model.SupplierAccount{
		{SupplierID: "supplier-a", SupplyEnabled: true, AllowedTypes: "[1]", AllowedModels: modelName, Group: "default", PoolPriority: 20, PoolWeight: 1},
		{SupplierID: "supplier-b", SupplyEnabled: true, AllowedTypes: "[1]", AllowedModels: modelName, Group: "default", PoolPriority: 10, PoolWeight: 1},
	} {
		require.NoError(t, db.Create(&account).Error)
	}
	for _, channel := range []struct {
		id       int
		owner    string
		priority int64
	}{{7101, "supplier-a", 100}, {7102, "supplier-a", 10}, {7103, "supplier-b", 999999}} {
		createChannelSelectAutoGroupsChannel(t, db, channel.id, "default", modelName)
		require.NoError(t, db.Model(&model.Channel{}).Where("id = ?", channel.id).Updates(map[string]any{"owner_supplier_id": channel.owner, "supplier_priority": channel.priority, "supplier_validated": true}).Error)
	}
	model.InitChannelCache()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	defer EndSchedulingRequest(c)
	first, err := schedulerSelectChannel(c, "default", modelName, 0, nil)
	require.NoError(t, err)
	require.NotNil(t, first)
	assert.Equal(t, 7101, first.Id, "the supplier's internal priority must not overtake the administrator's platform priority")
	// Local admission has not sent anything. Another channel in A remains a
	// valid choice and the pool receives no failed upstream health sample.
	AppendUsedChannel(c, first.Id)
	FinishSchedulingAttempt(c, &relaycommon.RelayInfo{}, nil)
	second, err := schedulerSelectChannel(c, "default", modelName, 1, nil)
	require.NoError(t, err)
	require.NotNil(t, second)
	assert.Equal(t, 7102, second.Id)
	state := schedulingState(c)
	pool, err := supplierRoutingPoolForChannel(second)
	require.NoError(t, err)
	snapshot := state.poolEngine.Snapshot(scheduler.Key{Group: "default", Model: modelName}, []scheduler.Candidate{pool.candidate}, false)
	require.Len(t, snapshot.Channels, 1)
	assert.Zero(t, snapshot.Channels[0].Window30m.Attempts)
	require.True(t, MarkSupplierRequestSent(c, second))
	assert.False(t, MarkSupplierRequestSent(c, first))
	StartSchedulingAttempt(c, &relaycommon.RelayInfo{})
	FinishSchedulingAttempt(c, &relaycommon.RelayInfo{}, types.NewErrorWithStatusCode(errors.New("upstream failed"), types.ErrorCodeBadResponse, http.StatusBadGateway))
	third, err := schedulerSelectChannel(c, "default", modelName, 2, nil)
	require.NoError(t, err)
	require.NotNil(t, third)
	assert.Equal(t, 7103, third.Id, "one real dispatch to A excludes every A channel for this logical request")
	assert.Error(t, ReserveSchedulingChannel(c, first, "default", modelName))
	assert.NotNil(t, ChannelAvailabilityError(c, first))
	// Adding a channel preserves the platform identity and its observations.
	added := *second
	added.Id, added.SupplierPriority = 7104, 999999999
	newPool, err := supplierRoutingPoolForChannel(&added)
	require.NoError(t, err)
	assert.Equal(t, pool.candidate.ID, newPool.candidate.ID)
	snapshot = state.poolEngine.Snapshot(scheduler.Key{Group: "default", Model: modelName}, []scheduler.Candidate{newPool.candidate}, false)
	assert.Equal(t, int64(1), snapshot.Channels[0].Window30m.Attempts)
}

func TestSupplierPoolWeightDoesNotGrowWithChannelCount(t *testing.T) {
	db := setupChannelSelectAutoGroupsTest(t)
	require.NoError(t, db.AutoMigrate(&model.SupplierAccount{}))
	const modelName = "supplier-pool-weight-model"
	setSchedulingTestConfig(t, []SchedulingConfig{{Group: "default", Model: modelName, Enabled: false, TargetSuccessRate: .95, TargetTTFTMS: 3000, DefaultCapacity: 100}})
	for _, owner := range []string{"supplier-a", "supplier-b"} {
		require.NoError(t, db.Create(&model.SupplierAccount{SupplierID: owner, SupplyEnabled: true, AllowedTypes: "[1]", AllowedModels: modelName, Group: "default", PoolWeight: 1}).Error)
	}
	for _, channel := range []struct {
		id    int
		owner string
	}{{7201, "supplier-a"}, {7202, "supplier-a"}, {7203, "supplier-a"}, {7204, "supplier-b"}} {
		createChannelSelectAutoGroupsChannel(t, db, channel.id, "default", modelName)
		require.NoError(t, db.Model(&model.Channel{}).Where("id = ?", channel.id).Updates(map[string]any{"owner_supplier_id": channel.owner, "supplier_validated": true}).Error)
	}
	model.InitChannelCache()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	defer EndSchedulingRequest(c)
	selected, err := schedulerSelectChannel(c, "default", modelName, 0, nil)
	require.NoError(t, err)
	require.NotNil(t, selected)
	a, err := supplierRoutingPoolForChannel(&model.Channel{OwnerSupplierID: "supplier-a"})
	require.NoError(t, err)
	b, err := supplierRoutingPoolForChannel(&model.Channel{OwnerSupplierID: "supplier-b"})
	require.NoError(t, err)
	snapshot := schedulingState(c).poolEngine.Snapshot(scheduler.Key{Group: "default", Model: modelName}, []scheduler.Candidate{a.candidate, b.candidate}, false)
	require.Len(t, snapshot.Channels, 2)
	for _, channel := range snapshot.Channels {
		assert.InDelta(t, .5, channel.SelectionProbability, 1e-12)
	}
	// A live supply suspension takes effect even with preheated channel cache.
	require.NoError(t, db.Model(&model.SupplierAccount{}).Where("supplier_id = ?", "supplier-a").Update("supply_enabled", false).Error)
	FinishSchedulingAttempt(c, &relaycommon.RelayInfo{}, nil)
	selected, err = schedulerSelectChannel(c, "default", modelName, 0, nil)
	require.NoError(t, err)
	require.NotNil(t, selected)
	assert.Equal(t, "supplier-b", selected.OwnerSupplierID)
}

func TestSupplierPoolCapacityChangesApplyAcrossIdleModels(t *testing.T) {
	db := setupChannelSelectAutoGroupsTest(t)
	require.NoError(t, db.AutoMigrate(&model.SupplierAccount{}))
	setSchedulingTestConfig(t, []SchedulingConfig{
		{Group: "default", Model: "supplier-model-a", Enabled: false, TargetSuccessRate: .95, TargetTTFTMS: 3000, DefaultCapacity: 100},
		{Group: "default", Model: "supplier-model-b", Enabled: false, TargetSuccessRate: .95, TargetTTFTMS: 3000, DefaultCapacity: 100},
	})
	require.NoError(t, db.Create(&model.SupplierAccount{SupplierID: "supplier-a", SupplyEnabled: true, AllowedTypes: "[1]", AllowedModels: "supplier-model-a,supplier-model-b", Group: "default", PoolWeight: 1, PoolCapacity: 1}).Error)
	for i, name := range []string{"supplier-model-a", "supplier-model-b"} {
		createChannelSelectAutoGroupsChannel(t, db, 7301+i, "default", name)
		require.NoError(t, db.Model(&model.Channel{}).Where("id = ?", 7301+i).Updates(map[string]any{"owner_supplier_id": "supplier-a", "supplier_validated": true}).Error)
	}
	model.InitChannelCache()
	idle, _ := gin.CreateTestContext(httptest.NewRecorder())
	selected, err := schedulerSelectChannel(idle, "default", "supplier-model-b", 0, nil)
	require.NoError(t, err)
	require.NotNil(t, selected)
	EndSchedulingRequest(idle)
	first, _ := gin.CreateTestContext(httptest.NewRecorder())
	defer EndSchedulingRequest(first)
	selected, err = schedulerSelectChannel(first, "default", "supplier-model-a", 0, nil)
	require.NoError(t, err)
	require.NotNil(t, selected)
	require.NoError(t, db.Model(&model.SupplierAccount{}).Where("supplier_id = ?", "supplier-a").Update("pool_capacity", 2).Error)
	model.InitChannelCache()
	require.NoError(t, SyncSchedulingConfig())
	second, _ := gin.CreateTestContext(httptest.NewRecorder())
	defer EndSchedulingRequest(second)
	selected, err = schedulerSelectChannel(second, "default", "supplier-model-a", 0, nil)
	require.NoError(t, err)
	require.NotNil(t, selected, "the idle model must no longer retain the old capacity of one")
	require.NoError(t, db.Model(&model.SupplierAccount{}).Where("supplier_id = ?", "supplier-a").Update("pool_capacity", 1).Error)
	model.InitChannelCache()
	require.NoError(t, SyncSchedulingConfig())
	third, _ := gin.CreateTestContext(httptest.NewRecorder())
	defer EndSchedulingRequest(third)
	selected, err = schedulerSelectChannel(third, "default", "supplier-model-b", 0, nil)
	require.NoError(t, err)
	assert.Nil(t, selected, "decreasing capacity must retain both outstanding leases")
	third.AbortWithStatus(http.StatusServiceUnavailable)
	EndSchedulingRequest(third)
	summary := scheduler.Default.Snapshot(scheduler.Key{Group: "default", Model: "supplier-model-b"}, nil, false).Summary
	assert.EqualValues(t, 1, summary.CapacityExhausted30m, "supplier pool saturation is visible on the model dashboard")
	assert.Zero(t, summary.CapacitySkips30m)
}

func TestSchedulingColdFallbackSkipsUnavailableTiers(t *testing.T) {
	for _, tc := range []struct {
		name     string
		busy     []int
		disabled []int
		used     []string
		want     int64
	}{
		{name: "top full", busy: []int{4101, 4102}, want: 50},
		{name: "consecutive empty tiers", busy: []int{4101, 4102, 4103, 4104}, want: 0},
		{name: "disabled and attempted", disabled: []int{4101}, used: []string{"4102"}, want: 50},
		{name: "all unavailable", busy: []int{4101, 4102, 4103, 4104, 4105}, want: -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := setupChannelSelectAutoGroupsTest(t)
			const modelName = "cold-fallback"
			setSchedulingTestConfig(t, []SchedulingConfig{{Group: "default", Model: modelName, Enabled: true, TargetSuccessRate: .95, TargetTTFTMS: 3000, DefaultCapacity: 1}})
			for i, priority := range []int64{100, 100, 50, 50, 0} {
				id := 4101 + i
				createChannelSelectAutoGroupsChannel(t, db, id, "default", modelName)
				require.NoError(t, db.Model(&model.Channel{}).Where("id = ?", id).Update("priority", priority).Error)
			}
			for _, id := range tc.disabled {
				require.NoError(t, db.Model(&model.Channel{}).Where("id = ?", id).Update("status", common.ChannelStatusManuallyDisabled).Error)
			}
			model.InitChannelCache()
			require.NoError(t, SyncSchedulingConfig())
			key := scheduler.Key{Group: "default", Model: modelName}
			for _, id := range tc.busy {
				channel, err := model.CacheGetChannelForRouting(id)
				require.NoError(t, err)
				request := scheduler.Default.BeginRequest(key, fmt.Sprint(id), true)
				_, err = scheduler.Default.ReserveCandidate(request, SchedulerCandidate(channel), false)
				require.NoError(t, err)
				defer scheduler.Default.EndRequest(request)
			}
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			c.Set("use_channel", tc.used)
			defer EndSchedulingRequest(c)
			channel, err := schedulerSelectChannel(c, "default", modelName, 0, nil)
			require.NoError(t, err)
			if tc.want == -1 {
				assert.Nil(t, channel)
				return
			}
			require.NotNil(t, channel)
			assert.Equal(t, tc.want, channel.GetPriority())
			assert.Equal(t, tc.used, c.GetStringSlice("use_channel"), "skipping a tier does not create an upstream attempt")
			assert.Zero(t, scheduler.Default.Snapshot(key, []scheduler.Candidate{SchedulerCandidate(channel)}, false).Summary.Attempts30m)
		})
	}
}

func TestSchedulingColdRetryAdvancesAfterFallbackAndPinsStayFixed(t *testing.T) {
	db := setupChannelSelectAutoGroupsTest(t)
	common.RetryTimes = 2
	const modelName = "fallback-retry"
	setSchedulingTestConfig(t, []SchedulingConfig{{Group: "default", Model: modelName, Enabled: true, TargetSuccessRate: .95, TargetTTFTMS: 3000, DefaultCapacity: 1}})
	for i, priority := range []int64{100, 50, 50, 0} {
		id := 4201 + i
		createChannelSelectAutoGroupsChannel(t, db, id, "default", modelName)
		require.NoError(t, db.Model(&model.Channel{}).Where("id = ?", id).Update("priority", priority).Error)
	}
	model.InitChannelCache()
	busy, _ := gin.CreateTestContext(httptest.NewRecorder())
	busy.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	defer EndSchedulingRequest(busy)
	top, err := model.CacheGetChannelForRouting(4201)
	require.NoError(t, err)
	require.NoError(t, ReserveSchedulingChannel(busy, top, "default", modelName))
	pinned, _ := gin.CreateTestContext(httptest.NewRecorder())
	pinned.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	defer EndSchedulingRequest(pinned)
	GetChannelConstraints(pinned).AddPin(dto.ChannelPin{ChannelId: top.Id, Source: dto.PinSourceToken})
	selected, _, selectErr := SelectChannelForRequest(pinned, modelName, &RetryParam{Ctx: pinned, TokenGroup: "default", ModelName: modelName})
	assert.Nil(t, selected)
	require.NotNil(t, selectErr)
	assert.Equal(t, http.StatusServiceUnavailable, selectErr.StatusCode)

	affinity := operation_setting.GetChannelAffinitySetting()
	previousAffinity := *affinity
	t.Cleanup(func() { *affinity = previousAffinity })
	policy, err := model.BuildRequestPolicy(map[string]string{
		"channel_affinity_setting.enabled":      "true",
		"channel_affinity_setting.session_mode": "strict",
		"channel_affinity_setting.rules":        `[{"name":"full-channel","session_mode":"inherit","model_regex":[".*"],"key_sources":[{"type":"request_header","key":"X-Session"}]}]`,
	})
	require.NoError(t, err)
	*affinity = policy.Affinity
	strict, _ := gin.CreateTestContext(httptest.NewRecorder())
	strict.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	strict.Request.Header.Set("X-Session", t.Name())
	defer EndSchedulingRequest(strict)
	_, found := GetPreferredChannelByAffinity(strict, modelName, "default")
	require.False(t, found)
	require.Equal(t, "strict", RequestPolicy(strict).SessionMode)
	RecordChannelAffinity(strict, top.Id)
	t.Cleanup(func() { ClearCurrentChannelAffinityCache(strict) })
	selected, _, selectErr = SelectChannelForRequest(strict, modelName, &RetryParam{Ctx: strict, TokenGroup: "default", ModelName: modelName})
	assert.Nil(t, selected, "strict sessions cannot escape a full bound channel")
	require.NotNil(t, selectErr)
	assert.Equal(t, http.StatusServiceUnavailable, selectErr.StatusCode)

	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	defer EndSchedulingRequest(c)
	first, err := schedulerSelectChannel(c, "default", modelName, 0, nil)
	require.NoError(t, err)
	require.NotNil(t, first)
	assert.Equal(t, int64(50), first.GetPriority())
	info := &relaycommon.RelayInfo{UsingGroup: "default", OriginModelName: modelName}
	StartSchedulingAttempt(c, info)
	AppendUsedChannel(c, first.Id)
	FinishSchedulingAttempt(c, info, types.NewOpenAIError(errors.New("upstream unavailable"), types.ErrorCodeBadResponseStatusCode, 502))
	second, err := schedulerSelectChannel(c, "default", modelName, 1, nil)
	require.NoError(t, err)
	require.NotNil(t, second)
	assert.Equal(t, 4204, second.Id, "retry advances to the next tier, not an untried peer of the fallback")
}

func TestSchedulingMembershipRefreshPreservesOutstandingLeases(t *testing.T) {
	for _, mutation := range []string{"remove group", "remove model", "delete channel", "switch pool", "change capacity"} {
		t.Run(mutation, func(t *testing.T) {
			db := setupChannelSelectAutoGroupsTest(t)
			const modelName = "membership-model"
			entries := []SchedulingConfig{
				{Group: "default", Model: modelName, Enabled: true, TargetSuccessRate: .95, TargetTTFTMS: 3000, DefaultCapacity: 10, ChannelOverrides: []SchedulingChannelOverride{{ChannelID: 4301, CapacityKey: "shared"}}},
				{Group: "vip", Model: modelName, Enabled: true, TargetSuccessRate: .95, TargetTTFTMS: 3000, DefaultCapacity: 1, ChannelOverrides: []SchedulingChannelOverride{{ChannelID: 4302, CapacityKey: "shared"}}},
			}
			setSchedulingTestConfig(t, entries)
			createChannelSelectAutoGroupsChannel(t, db, 4301, "default", modelName)
			createChannelSelectAutoGroupsChannel(t, db, 4302, "vip", modelName)
			model.InitChannelCache()
			require.NoError(t, SyncSchedulingConfig())
			primary, err := model.CacheGetChannelForRouting(4301)
			require.NoError(t, err)
			old, err := model.CacheGetChannelForRouting(4302)
			require.NoError(t, err)
			oldRequest := scheduler.Default.BeginRequest(scheduler.Key{Group: "vip", Model: modelName}, "old", true)
			oldAttempt, err := scheduler.Default.ReserveCandidate(oldRequest, SchedulerCandidate(old), false)
			require.NoError(t, err)
			scheduler.Default.StartAttempt(oldAttempt)
			defer scheduler.Default.EndRequest(oldRequest)
			key := scheduler.Key{Group: "default", Model: modelName}
			request := scheduler.Default.BeginRequest(key, "new", true)
			defer scheduler.Default.EndRequest(request)
			_, err = scheduler.Default.ReserveCandidate(request, SchedulerCandidate(primary), false)
			assert.ErrorIs(t, err, scheduler.ErrNoEligibleChannel)
			switch mutation {
			case "remove group":
				old.Group = "other"
				require.NoError(t, old.Update())
			case "remove model":
				old.Models = "other-model"
				require.NoError(t, old.Update())
			case "delete channel":
				require.NoError(t, old.Delete())
			case "switch pool", "change capacity":
				if mutation == "switch pool" {
					entries[1].ChannelOverrides[0].CapacityKey = "new-pool"
				} else {
					entries[1].DefaultCapacity = 10
				}
				encoded, encodeErr := common.Marshal(entries)
				require.NoError(t, encodeErr)
				common.OptionMapRWMutex.Lock()
				common.OptionMap[schedulingOptionKey] = string(encoded)
				common.OptionMapRWMutex.Unlock()
			}
			// CRUD versions trigger reconciliation even before a controller's
			// next cache publication, without re-saving scheduling settings.
			require.NoError(t, SyncSchedulingConfig())
			current, err := scheduler.Default.ReserveCandidate(request, SchedulerCandidate(primary), false)
			require.NoError(t, err)
			assert.Equal(t, 10, current.Candidate.Capacity)
			snapshot := scheduler.Default.Snapshot(key, []scheduler.Candidate{SchedulerCandidate(primary)}, false)
			assert.Equal(t, 2, snapshot.Channels[0].InFlight, "the moved/deleted request retains its original pool lease")
			scheduler.Default.FinishAttempt(oldAttempt, scheduler.Outcome{Success: true})
			snapshot = scheduler.Default.Snapshot(key, []scheduler.Candidate{SchedulerCandidate(primary)}, false)
			assert.Equal(t, 1, snapshot.Channels[0].InFlight)
			scheduler.Default.FinishAttempt(current, scheduler.Outcome{})
			snapshot = scheduler.Default.Snapshot(key, []scheduler.Candidate{SchedulerCandidate(primary)}, false)
			assert.Zero(t, snapshot.Channels[0].InFlight)
		})
	}
}

func TestSchedulingMembershipSameChannelGroupRemovalAndCachedAliases(t *testing.T) {
	db := setupChannelSelectAutoGroupsTest(t)
	const modelName = "gpt-4-gizmo-*"
	setSchedulingTestConfig(t, []SchedulingConfig{
		{Group: "default", Model: modelName, Enabled: true, TargetSuccessRate: .95, TargetTTFTMS: 3000, DefaultCapacity: 10},
		{Group: "vip", Model: modelName, Enabled: true, TargetSuccessRate: .95, TargetTTFTMS: 3000, DefaultCapacity: 1},
	})
	createChannelSelectAutoGroupsChannel(t, db, 4401, "default", modelName)
	channel, err := model.GetChannelById(4401, true)
	require.NoError(t, err)
	channel.Group = "default,vip"
	require.NoError(t, channel.Update())
	model.InitChannelCache()
	require.NoError(t, SyncSchedulingConfig())
	key := scheduler.Key{Group: "default", Model: modelName}
	old := scheduler.Default.BeginRequest(key, "old", true)
	defer scheduler.Default.EndRequest(old)
	_, err = scheduler.Default.ReserveCandidate(old, SchedulerCandidate(channel), false)
	require.NoError(t, err)
	channel.Group = "default"
	require.NoError(t, channel.Update())
	model.InitChannelCache()
	require.NoError(t, SyncSchedulingConfig())
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	defer EndSchedulingRequest(c)
	selected, err := schedulerSelectChannel(c, "default", "gpt-4-gizmo-example", 0, nil)
	require.NoError(t, err)
	require.NotNil(t, selected, "removing vip releases its old limit; unconfigured normalized aliases still route")
	assert.Equal(t, 4401, selected.Id)
	assert.Equal(t, 10, schedulingState(c).attempt.Candidate.Capacity)

	queries := 0
	callback := "scheduling_membership_query_count"
	require.NoError(t, db.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) { queries++ }))
	t.Cleanup(func() { require.NoError(t, db.Callback().Query().Remove(callback)) })
	require.NoError(t, SyncSchedulingConfig())
	require.NoError(t, SyncSchedulingConfig(scheduler.Key{Group: "default", Model: "gpt-4-gizmo-another"}))
	assert.Zero(t, queries, "stable metadata and new aliases use the complete cached snapshot, not database scans")
}

func TestSchedulingStaticWeightUnaffectedByCapacityOverride(t *testing.T) {
	db := setupChannelSelectAutoGroupsTest(t)
	const modelName = "static-weight"
	entries := []SchedulingConfig{{Group: "default", Model: modelName, Enabled: true, TargetSuccessRate: .95, TargetTTFTMS: 3000, DefaultCapacity: 10}}
	setSchedulingTestConfig(t, entries)
	var candidates []scheduler.Candidate
	for index, weight := range []uint{1, 9, 0} {
		id := 4501 + index
		createChannelSelectAutoGroupsChannel(t, db, id, "default", modelName)
		require.NoError(t, db.Model(&model.Channel{}).Where("id = ?", id).Update("weight", weight).Error)
		require.NoError(t, db.Model(&model.Ability{}).Where("channel_id = ?", id).Update("weight", weight).Error)
		channel, err := model.GetChannelById(id, true)
		require.NoError(t, err)
		candidates = append(candidates, SchedulerCandidate(channel))
	}
	for _, cached := range []bool{false, true} {
		common.MemoryCacheEnabled = cached
		model.InitChannelCache()
		for _, override := range []bool{false, true} {
			entries[0].ChannelOverrides = nil
			if override {
				entries[0].ChannelOverrides = []SchedulingChannelOverride{{ChannelID: 4501, Capacity: common.GetPointer(3)}}
			}
			encoded, err := common.Marshal(entries)
			require.NoError(t, err)
			common.OptionMapRWMutex.Lock()
			common.OptionMap[schedulingOptionKey] = string(encoded)
			common.OptionMapRWMutex.Unlock()
			require.NoError(t, SyncSchedulingConfig())
			snapshot := scheduler.Default.Snapshot(scheduler.Key{Group: "default", Model: modelName}, candidates, false)
			require.Len(t, snapshot.Channels, 3)
			assert.InDelta(t, .1, snapshot.Channels[0].SelectionProbability, 1e-9)
			assert.InDelta(t, .9, snapshot.Channels[1].SelectionProbability, 1e-9)
			assert.Zero(t, snapshot.Channels[2].SelectionProbability)
		}
	}
}

func TestSchedulingTransientCooldownIsEnforcedByHostRouting(t *testing.T) {
	for _, tc := range []struct {
		name       string
		lastStatus int
		cancelled  bool
		wantID     int
	}{
		{name: "three transient failures", lastStatus: 503, wantID: 4602},
		{name: "parameter error excluded", lastStatus: 400, wantID: 4601},
		{name: "client cancellation excluded", lastStatus: 502, cancelled: true, wantID: 4601},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := setupChannelSelectAutoGroupsTest(t)
			const modelName = "host-cooldown"
			setSchedulingTestConfig(t, nil)
			createChannelSelectAutoGroupsChannel(t, db, 4601, "default", modelName)
			createChannelSelectAutoGroupsChannel(t, db, 4602, "default", modelName)
			require.NoError(t, db.Model(&model.Channel{}).Where("id = ?", 4601).Update("priority", 100).Error)
			model.InitChannelCache()
			channel, err := model.CacheGetChannelForRouting(4601)
			require.NoError(t, err)
			for index := range 3 {
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				ctx, cancel := context.WithCancel(context.Background())
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil).WithContext(ctx)
				require.NoError(t, ReserveSchedulingChannel(c, channel, "default", modelName))
				info := &relaycommon.RelayInfo{UsingGroup: "default", OriginModelName: modelName}
				StartSchedulingAttempt(c, info)
				status := 502
				if index == 2 {
					status = tc.lastStatus
					if tc.cancelled {
						cancel()
					}
				}
				FinishSchedulingAttempt(c, info, types.NewOpenAIError(errors.New("upstream failure"), types.ErrorCodeBadResponseStatusCode, status))
				EndSchedulingRequest(c)
				cancel()
			}
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			defer EndSchedulingRequest(c)
			selected, err := schedulerSelectChannel(c, "default", modelName, 0, nil)
			require.NoError(t, err)
			require.NotNil(t, selected)
			assert.Equal(t, tc.wantID, selected.Id)
			if tc.wantID == 4602 {
				assert.ErrorIs(t, ReserveSchedulingChannel(c, channel, "default", modelName), scheduler.ErrNoEligibleChannel, "fixed reservations cannot bypass cooldown")
			}
		})
	}
}

func TestSchedulingColdRetryDoesNotSkipTierAfterPreviousIsDisabled(t *testing.T) {
	db := setupChannelSelectAutoGroupsTest(t)
	const modelName = "retry-disabled-tier"
	setSchedulingTestConfig(t, nil)
	for index, priority := range []int64{100, 50, 0} {
		createChannelSelectAutoGroupsChannel(t, db, 4701+index, "default", modelName)
		require.NoError(t, db.Model(&model.Channel{}).Where("id = ?", 4701+index).Update("priority", priority).Error)
	}
	model.InitChannelCache()
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	defer EndSchedulingRequest(c)
	first, err := schedulerSelectChannel(c, "default", modelName, 0, nil)
	require.NoError(t, err)
	require.NotNil(t, first)
	assert.Equal(t, 4701, first.Id)
	info := &relaycommon.RelayInfo{UsingGroup: "default", OriginModelName: modelName}
	StartSchedulingAttempt(c, info)
	AppendUsedChannel(c, first.Id)
	FinishSchedulingAttempt(c, info, types.NewOpenAIError(errors.New("upstream down"), types.ErrorCodeBadResponseStatusCode, 502))
	require.NoError(t, db.Model(&model.Channel{}).Where("id = ?", first.Id).Update("status", common.ChannelStatusAutoDisabled).Error)
	model.InitChannelCache()
	next, err := schedulerSelectChannel(c, "default", modelName, 1, nil)
	require.NoError(t, err)
	require.NotNil(t, next)
	assert.Equal(t, 4702, next.Id, "removing the previous tier does not consume the following tier too")
}

func TestSchedulingHonorsSessionAffinityAndAdmission(t *testing.T) {
	const boundID, fallbackID = 4801, 4802
	for _, tc := range []struct {
		name    string
		active  bool
		mode    string
		full    bool
		refused bool
		wantID  int
	}{
		{name: "active prefer keeps binding", active: true, mode: "prefer", wantID: boundID},
		{name: "active strict keeps binding", active: true, mode: "strict", wantID: boundID},
		{name: "active prefer full falls back", active: true, mode: "prefer", full: true, wantID: fallbackID},
		{name: "cold prefer full falls back", mode: "prefer", full: true, wantID: fallbackID},
		{name: "active strict full refuses", active: true, mode: "strict", full: true},
		{name: "active prefer skips locally refused binding", active: true, mode: "prefer", refused: true, wantID: fallbackID},
	} {
		t.Run(tc.name, func(t *testing.T) {
			db := setupChannelSelectAutoGroupsTest(t)
			const modelName = "scheduler-session-affinity"
			setSchedulingTestConfig(t, []SchedulingConfig{{Group: "default", Model: modelName, Enabled: true, TargetSuccessRate: .95, TargetTTFTMS: 3000, DefaultCapacity: 1}})
			createChannelSelectAutoGroupsChannel(t, db, boundID, "default", modelName)
			createChannelSelectAutoGroupsChannel(t, db, fallbackID, "default", modelName)
			require.NoError(t, db.Model(&model.Channel{}).Where("id = ?", fallbackID).Update("priority", 100).Error)
			model.InitChannelCache()

			affinity := operation_setting.GetChannelAffinitySetting()
			previousAffinity := *affinity
			t.Cleanup(func() { *affinity = previousAffinity })
			*affinity = operation_setting.ChannelAffinitySetting{
				Enabled: true, DefaultTTLSeconds: 3600,
				Rules: []operation_setting.ChannelAffinityRule{{
					Name: t.Name(), ModelRegex: []string{".*"}, SessionMode: tc.mode,
					KeySources: []operation_setting.ChannelAffinityKeySource{{Type: "request_header", Key: "X-Session"}},
				}},
			}
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			c.Request.Header.Set("X-Session", t.Name())
			defer EndSchedulingRequest(c)
			_, found := GetPreferredChannelByAffinity(c, modelName, "default")
			require.False(t, found)
			RecordChannelAffinity(c, boundID)
			t.Cleanup(func() { ClearCurrentChannelAffinityCache(c) })
			key := scheduler.Key{Group: "default", Model: modelName}
			require.NoError(t, SyncSchedulingConfig(key))
			if tc.active {
				for i := range 50 {
					request := scheduler.Default.BeginRequest(key, fmt.Sprintf("activation-%d", i), false)
					scheduler.Default.EndRequest(request)
				}
			}
			require.Equal(t, tc.active, scheduler.Default.IsActive(key))

			busy, _ := gin.CreateTestContext(httptest.NewRecorder())
			busy.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			defer EndSchedulingRequest(busy)
			bound, err := model.CacheGetChannelForRouting(boundID)
			require.NoError(t, err)
			if tc.full {
				require.NoError(t, ReserveSchedulingChannel(busy, bound, "default", modelName))
			}
			param := &RetryParam{Ctx: c, TokenGroup: "default", ModelName: modelName}
			if tc.refused {
				first, _, selectErr := SelectChannelForRequest(c, modelName, param)
				require.Nil(t, selectErr)
				require.NotNil(t, first)
				require.Equal(t, boundID, first.Id)
				AppendUsedChannel(c, boundID)
				FinishSchedulingAttempt(c, nil, types.NewOpenAIError(errors.New("local admission refused"), types.ErrorCode("channel_limit_unavailable"), http.StatusServiceUnavailable))
				c.Set("channel_local_refusals", 1)
			}
			selected, group, selectErr := SelectChannelForRequest(c, modelName, param)
			if tc.wantID == 0 {
				assert.Nil(t, selected)
				require.NotNil(t, selectErr)
				assert.Equal(t, http.StatusServiceUnavailable, selectErr.StatusCode)
				c.AbortWithStatus(selectErr.StatusCode)
			} else {
				require.Nil(t, selectErr)
				require.NotNil(t, selected)
				assert.Equal(t, tc.wantID, selected.Id)
				assert.Equal(t, "default", group)
				info, marked := c.Get(ginKeyChannelAffinityLogInfo)
				if tc.wantID == boundID {
					require.True(t, marked)
					assert.Equal(t, boundID, info.(map[string]any)["channel_id"])
					assert.Equal(t, 1, c.GetInt(ginKeyChannelAffinityRetryOffset))
				} else {
					assert.Equal(t, tc.refused, marked, "only a previously admitted binding may be marked")
					assert.Zero(t, c.GetInt(ginKeyChannelAffinityRetryOffset), "fallback must not consume the affinity retry offset")
				}
			}
			EndSchedulingRequest(c)
			EndSchedulingRequest(busy)
			snapshot := scheduler.Default.Snapshot(key, []scheduler.Candidate{SchedulerCandidate(bound)}, false)
			assert.Zero(t, snapshot.Summary.InFlight, "selection and fallback must release all reservations")
			assert.Zero(t, snapshot.Summary.Attempts30m, "local reservations are not upstream attempts")
			if tc.full && tc.mode == "strict" {
				assert.EqualValues(t, 1, snapshot.Summary.CapacityExhausted30m)
				assert.Zero(t, snapshot.Summary.CapacitySkips30m)
			} else if tc.full {
				assert.EqualValues(t, 1, snapshot.Summary.CapacitySkips30m)
				assert.Zero(t, snapshot.Summary.CapacityExhausted30m)
			}
		})
	}
}

func bindRoutingTestSession(t *testing.T, name, mode string, bound int) func() *gin.Context {
	t.Helper()
	affinity := operation_setting.GetChannelAffinitySetting()
	old := *affinity
	t.Cleanup(func() { *affinity = old })
	*affinity = operation_setting.ChannelAffinitySetting{
		Enabled: true, SwitchOnSuccess: true,
		Rules: []operation_setting.ChannelAffinityRule{{
			Name: t.Name(), ModelRegex: []string{".*"}, SessionMode: mode,
			KeySources: []operation_setting.ChannelAffinityKeySource{{Type: "request_header", Key: "X-Session"}},
		}},
	}
	newRequest := func() *gin.Context {
		c := routingTestContext(t)
		c.Request.Header.Set("X-Session", t.Name())
		return c
	}
	seed := newRequest()
	_, found := GetPreferredChannelByAffinity(seed, name, "default")
	require.False(t, found)
	RecordChannelAffinity(seed, bound)
	t.Cleanup(func() { ClearCurrentChannelAffinityCache(seed) })
	return newRequest
}

func TestSessionBindingSurvivesRequestFilter(t *testing.T) {
	db := setupChannelSelectAutoGroupsTest(t)
	const name, bound, alternate = "filtered-session", 6010, 6011
	setSchedulingTestConfig(t, nil)
	for _, id := range []int{bound, alternate} {
		createChannelSelectAutoGroupsChannel(t, db, id, "default", name)
	}
	require.NoError(t, db.Model(&model.Channel{}).Where("id = ?", alternate).Update("setting", `{"responses_websocket_enabled":true}`).Error)
	model.InitChannelCache()
	require.NoError(t, SyncSchedulingConfig())
	for _, mode := range []string{"prefer", "strict"} {
		t.Run(mode, func(t *testing.T) {
			newRequest := bindRoutingTestSession(t, name, mode, bound)
			filtered := newRequest()
			GetChannelConstraints(filtered).AddFilter(dto.ChannelFilter{Kind: dto.FilterResponsesWebSocket})
			selected, _, err := SelectChannelForRequest(filtered, name, &RetryParam{Ctx: filtered, TokenGroup: "default", ModelName: name})
			if mode == "strict" {
				require.NotNil(t, err)
				require.Equal(t, http.StatusServiceUnavailable, err.StatusCode)
			} else {
				require.Nil(t, err)
				require.Equal(t, alternate, selected.Id)
				RecordChannelAffinity(filtered, selected.Id)
			}
			next := newRequest()
			selected, _, err = SelectChannelForRequest(next, name, &RetryParam{Ctx: next, TokenGroup: "default", ModelName: name})
			require.Nil(t, err)
			assert.Equal(t, bound, selected.Id)
			assert.Equal(t, bound, next.GetInt(ginKeyChannelAffinityUsedChannel))
		})
	}
}

func TestSessionHealthBypassRequiresReservableSupplier(t *testing.T) {
	db := setupChannelSelectAutoGroupsTest(t)
	const name, other, bound, healthy, sibling = "health-session", "other-model", 6100, 6101, 6102
	setSchedulingTestConfig(t, nil)
	require.NoError(t, db.AutoMigrate(&model.SupplierAccount{}))
	for priority, supplier := range []string{"healthy", "preferred"} {
		require.NoError(t, db.Create(&model.SupplierAccount{SupplierID: supplier, SupplyEnabled: true, AllowedTypes: "[1]", AllowedModels: name + "," + other, Group: "default", PoolWeight: 1, PoolPriority: int64(priority), PoolCapacity: 1}).Error)
	}
	for _, row := range []struct {
		id              int
		supplier, model string
	}{{bound, "preferred", name}, {healthy, "healthy", name}, {sibling, "healthy", other}} {
		createChannelSelectAutoGroupsChannel(t, db, row.id, "default", row.model)
		require.NoError(t, db.Model(&model.Channel{}).Where("id = ?", row.id).Updates(map[string]any{"owner_supplier_id": row.supplier, "supplier_validated": true}).Error)
	}
	require.NoError(t, db.Model(&model.Channel{}).Where("id = ?", bound).Update("setting", `{"responses_websocket_enabled":true}`).Error)
	model.InitChannelCache()
	require.NoError(t, SyncSchedulingConfig())
	for _, id := range []int{healthy, bound} {
		channel, err := model.CacheGetChannelForRouting(id)
		require.NoError(t, err)
		c := routingTestContext(t)
		require.NoError(t, ReserveSchedulingChannel(c, channel, "default", name))
		info := &relaycommon.RelayInfo{OriginModelName: name, UsingGroup: "default"}
		StartSchedulingAttempt(c, info)
		var failure *types.NewAPIError
		if id == bound {
			failure = types.NewOpenAIError(errors.New("upstream error"), types.ErrorCodeBadResponseStatusCode, http.StatusInternalServerError)
		}
		FinishSchedulingAttempt(c, info, failure)
		EndSchedulingRequest(c)
	}
	newRequest := bindRoutingTestSession(t, name, "prefer", bound)
	c := newRequest()
	selected, _, selectErr := SelectChannelForRequest(c, name, &RetryParam{Ctx: c, TokenGroup: "default", ModelName: name})
	require.Nil(t, selectErr)
	require.Equal(t, healthy, selected.Id)
	RecordChannelAffinity(c, selected.Id)
	EndSchedulingRequest(c)
	filtered := newRequest()
	GetChannelConstraints(filtered).AddFilter(dto.ChannelFilter{Kind: dto.FilterResponsesWebSocket})
	selected, _, selectErr = SelectChannelForRequest(filtered, name, &RetryParam{Ctx: filtered, TokenGroup: "default", ModelName: name})
	require.Nil(t, selectErr)
	require.Equal(t, bound, selected.Id, "the healthy supplier has no channel serving this request")
	assert.Equal(t, bound, filtered.GetInt(ginKeyChannelAffinityUsedChannel))
	EndSchedulingRequest(filtered)
	// Only the supplier pool is full. The healthy channel has no active attempt.
	busy, err := model.CacheGetChannelForRouting(sibling)
	require.NoError(t, err)
	require.NoError(t, ReserveSchedulingChannel(routingTestContext(t), busy, "default", other))
	c = newRequest()
	selected, _, selectErr = SelectChannelForRequest(c, name, &RetryParam{Ctx: c, TokenGroup: "default", ModelName: name})
	require.Nil(t, selectErr)
	require.Equal(t, bound, selected.Id)
	assert.Equal(t, bound, c.GetInt(ginKeyChannelAffinityUsedChannel))
}

func TestSchedulingMetadataReloadFailureKeepsRouting(t *testing.T) {
	db := setupChannelSelectAutoGroupsTest(t)
	setSchedulingTestConfig(t, nil)
	require.NoError(t, db.AutoMigrate(&model.SupplierAccount{}))
	const name = "scheduler-metadata-reload"
	createChannelSelectAutoGroupsChannel(t, db, 7401, "default", name)
	createChannelSelectAutoGroupsChannel(t, db, 7402, "default", name)
	// Cold routing always prefers this channel while its ability is enabled.
	require.NoError(t, db.Model(&model.Channel{}).Where("id = ?", 7402).Update("priority", 100).Error)
	model.InitChannelCache()
	require.NoError(t, SyncSchedulingConfig())

	// Leave the native channel enabled while revoking just this model ability.
	require.NoError(t, db.Model(&model.Ability{}).Where("channel_id = ?", 7402).Update("enabled", false).Error)
	createChannelSelectAutoGroupsChannel(t, db, 7403, "new-group", name)
	createChannelSelectAutoGroupsChannel(t, db, 7404, "supplier-group", name)
	require.NoError(t, db.Model(&model.Channel{}).Where("id = ?", 7404).Update("owner_supplier_id", "new-supplier").Error)
	model.InitChannelCache()

	started := make(chan struct{})
	release := make(chan struct{})
	var signal, unblock sync.Once
	defer unblock.Do(func() { close(release) })
	const callback = "block_supplier_membership"
	require.NoError(t, db.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "supplier_accounts" {
			signal.Do(func() { close(started) })
			<-release
			tx.AddError(errors.New("supplier read offline"))
		}
	}))
	selectOnce := func(group string) *model.Channel {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		defer EndSchedulingRequest(c)
		selected, err := schedulerSelectChannel(c, group, name, 0, nil)
		require.NoError(t, err)
		return selected
	}
	// Selection must serve the old complete snapshot even when the supplier
	// query has not returned; the revoked ability is immediately ineligible.
	first := make(chan error, 1)
	go func() { first <- ensureSchedulingConfig(false) }()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("metadata refresh did not reach supplier query")
	}
	select {
	case err := <-first:
		require.NoError(t, err)
	case <-time.After(5 * time.Second):
		t.Fatal("the request that scheduled the refresh is blocked on the supplier query")
	}
	selected := selectOnce("default")
	require.NotNil(t, selected)
	assert.Equal(t, 7401, selected.Id)
	schedulingConfigState.Lock()
	work := schedulingConfigState.refresh
	schedulingConfigState.Unlock()
	require.NotNil(t, work)
	assert.Nil(t, selectOnce("new-group"))
	unblock.Do(func() { close(release) })
	<-work.done
	require.Error(t, work.err)
	assert.Nil(t, selectOnce("new-group"), "a failed supplier read must publish neither layer")
	selected = selectOnce("default")
	require.NotNil(t, selected)
	assert.Equal(t, 7401, selected.Id)
	require.NoError(t, db.Callback().Query().Remove(callback))
	require.NoError(t, SyncSchedulingConfig())
	selected = selectOnce("new-group")
	require.NotNil(t, selected)
	assert.Equal(t, 7403, selected.Id, "complete publication admits the new native channel")
}

func TestAvailabilityFailureClassification(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  *types.NewAPIError
		want bool
	}{
		{"local plugin failure", types.NewErrorWithStatusCode(errors.New("plugin"), types.ErrorCodeRequestPluginFailed, 500), false},
		{"credential in synthesized 502", types.InitOpenAIError("invalid_api_key", 502), false},
		{"server error", types.NewOpenAIError(errors.New("internal"), types.ErrorCodeBadResponseStatusCode, http.StatusInternalServerError), true},
		{"overloaded", types.NewOpenAIError(errors.New("overloaded"), types.ErrorCodeBadResponseStatusCode, http.StatusServiceUnavailable), true},
		{"rate limited", types.NewOpenAIError(errors.New("slow down"), types.ErrorCodeBadResponseStatusCode, http.StatusTooManyRequests), true},
		{"connection failed", types.NewError(errors.New("dial"), types.ErrorCodeDoRequestFailed), true},
		{"invalid credential", types.NewOpenAIError(errors.New("bad key"), types.ErrorCodeBadResponseStatusCode, http.StatusUnauthorized), false},
		{"channel configuration", types.NewError(errors.New("no key"), types.ErrorCodeChannelNoAvailableKey), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, availabilityFailure(nil, tc.err))
		})
	}
	scanner := relaycommon.NewStreamStatus()
	scanner.SetEndReason(relaycommon.StreamEndReasonScannerErr, errors.New("read failed"))
	credential := relaycommon.NewStreamStatus()
	credential.MarkFailed("invalid_api_key", "authentication_error", http.StatusBadGateway)
	overload := relaycommon.NewStreamStatus()
	overload.MarkFailed("overloaded_error", "", 0)
	deadline := relaycommon.NewStreamStatus()
	deadline.SetEndReason(relaycommon.StreamEndReasonClientGone, context.DeadlineExceeded)
	for _, tc := range []struct {
		name   string
		stream *relaycommon.StreamStatus
		want   bool
	}{
		{"interrupted stream", scanner, true},
		{"in-band credential rejection", credential, false},
		{"in-band overload", overload, true},
		{"upstream read deadline", deadline, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, availabilityFailure(&relaycommon.RelayInfo{IsStream: true, StreamStatus: tc.stream}, nil))
		})
	}
}

// A channel refused locally before dispatch is excluded, but the next pick
// remains a weighted draw: only an upstream dispatch makes selection rank
// candidates as a retry, which always takes the top weight.
func TestLocalRefusalKeepsWeightedSelection(t *testing.T) {
	db := setupChannelSelectAutoGroupsTest(t)
	const modelName = "scheduler-local-refusal"
	setSchedulingTestConfig(t, nil)
	for _, id := range []int{7501, 7502, 7503} {
		createChannelSelectAutoGroupsChannel(t, db, id, "default", modelName)
	}
	model.InitChannelCache()
	key := scheduler.Key{Group: "default", Model: modelName}
	require.NoError(t, SyncSchedulingConfig(key))
	for i := range 50 {
		scheduler.Default.EndRequest(scheduler.Default.BeginRequest(key, fmt.Sprintf("activation-%d", i), false))
	}
	require.True(t, scheduler.Default.IsActive(key))
	chosen := make(map[int]bool)
	for range 40 {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		AppendUsedChannel(c, 7501)
		c.Set("channel_local_refusals", 1)
		selected, err := schedulerSelectChannel(c, "default", modelName, 0, nil)
		require.NoError(t, err)
		require.NotNil(t, selected)
		assert.NotEqual(t, 7501, selected.Id, "the refused channel is excluded")
		chosen[selected.Id] = true
		EndSchedulingRequest(c)
	}
	assert.Len(t, chosen, 2, "equal remaining channels are drawn by weight, not ranked")
}

func TestSchedulingConfigFailuresKeepServingLastSnapshot(t *testing.T) {
	db := setupChannelSelectAutoGroupsTest(t)
	require.NoError(t, db.AutoMigrate(&model.Option{}))
	const name = "stale-scheduling-config"
	key := scheduler.Key{Group: "default", Model: name}
	setSchedulingTestConfig(t, nil)
	createChannelSelectAutoGroupsChannel(t, db, 7601, "default", name)
	model.InitChannelCache()
	setRaw := func(raw string) {
		common.OptionMapRWMutex.Lock()
		common.OptionMap[schedulingOptionKey] = raw
		common.OptionMapRWMutex.Unlock()
	}
	selectOnce := func() *ChannelSelectError {
		c := routingTestContext(t)
		defer EndSchedulingRequest(c)
		_, _, err := SelectChannelForRequest(c, name, &RetryParam{Ctx: c, TokenGroup: "default", ModelName: name})
		return err
	}

	// Without any published snapshot the request fails, with a generic message.
	setRaw(`{"not": "a list"`)
	selectErr := selectOnce()
	require.NotNil(t, selectErr)
	assert.Equal(t, http.StatusServiceUnavailable, selectErr.StatusCode)
	assert.Equal(t, channelSelectionUnavailable, selectErr.Message, "parse errors are logged, never returned to clients")

	setRaw("[]")
	require.NoError(t, SyncSchedulingConfig())
	require.Nil(t, selectOnce())
	// A later invalid value keeps routing on the last published snapshot.
	setRaw(`[{"group":"default","model":"` + name + `","target_success_rate":5}]`)
	require.Nil(t, selectOnce())
	require.Error(t, SyncSchedulingConfig(), "the administrative barrier still reports the failure")
	status := GetSchedulingConfigStatus()
	assert.True(t, status.Stale)
	assert.NotEmpty(t, status.Error)
	assert.Positive(t, status.Failures)
	require.Nil(t, selectOnce(), "requests during the retry backoff still route")

	// The administrator can replace the invalid value. A blank channel override
	// is dropped and an entry equal to the defaults is removed.
	saved, syncErr, err := SaveSchedulingConfig(SchedulingConfig{Group: "default", Model: name, Enabled: true, TargetSuccessRate: .95, TargetTTFTMS: 3000, DefaultCapacity: 2,
		ChannelOverrides: []SchedulingChannelOverride{{ChannelID: 7601}}})
	require.NoError(t, err)
	require.NoError(t, syncErr)
	assert.Empty(t, saved.ChannelOverrides)
	assert.False(t, GetSchedulingConfigStatus().Stale)
	assert.Equal(t, 2, GetSchedulingConfig(key).DefaultCapacity)
	saved.DefaultCapacity = 0
	_, syncErr, err = SaveSchedulingConfig(saved)
	require.NoError(t, err)
	require.NoError(t, syncErr)
	common.OptionMapRWMutex.RLock()
	assert.Equal(t, "[]", common.OptionMap[schedulingOptionKey])
	common.OptionMapRWMutex.RUnlock()

	// A reload failure after a durable save is a pending sync, not a failed save.
	const callback = "fail_scheduling_channel_reload"
	require.NoError(t, db.Callback().Query().Before("gorm:query").Register(callback, func(tx *gorm.DB) {
		if tx.Statement.Table == "channels" {
			tx.AddError(errors.New("channel read offline"))
		}
	}))
	saved.DefaultCapacity = 3
	_, syncErr, err = SaveSchedulingConfig(saved)
	require.NoError(t, err)
	require.Error(t, syncErr)
	assert.Equal(t, 0, GetSchedulingConfig(key).DefaultCapacity, "routing keeps the previous snapshot until the reload succeeds")
	require.Nil(t, selectOnce())
	require.NoError(t, db.Callback().Query().Remove(callback))
	require.NoError(t, SyncSchedulingConfig())
	assert.Equal(t, 3, GetSchedulingConfig(key).DefaultCapacity)
}

func TestSchedulingAliasesNeedCandidatesAndExpire(t *testing.T) {
	db := setupChannelSelectAutoGroupsTest(t)
	const pattern = "gpt-4-gizmo-*"
	setSchedulingTestConfig(t, nil)
	createChannelSelectAutoGroupsChannel(t, db, 7701, "default", pattern)
	model.InitChannelCache()
	require.NoError(t, SyncSchedulingConfig())
	route := func(modelName string) *model.Channel {
		c := routingTestContext(t)
		defer EndSchedulingRequest(c)
		selected, err := schedulerSelectChannel(c, "default", modelName, 0, nil)
		require.NoError(t, err)
		return selected
	}
	aliases := func() map[scheduler.Key]time.Time {
		schedulingConfigState.Lock()
		defer schedulingConfigState.Unlock()
		return maps.Clone(schedulingConfigState.aliases)
	}
	assert.Nil(t, route("no-such-model"))
	assert.NotContains(t, scheduler.Default.Keys(), scheduler.Key{Group: "default", Model: "no-such-model"}, "a model without candidates creates no routing state")
	old := scheduler.Key{Group: "default", Model: "gpt-4-gizmo-old"}
	require.NotNil(t, route(old.Model))
	require.Contains(t, aliases(), old)

	schedulingConfigState.Lock()
	schedulingConfigState.aliases[old] = time.Now().Add(-schedulingAliasTTL)
	schedulingConfigState.aliasSweep = time.Time{}
	schedulingConfigState.Unlock()
	fresh := scheduler.Key{Group: "default", Model: "gpt-4-gizmo-fresh"}
	require.NotNil(t, route(fresh.Model))
	current := aliases()
	assert.NotContains(t, current, old, "an alias unused for the statistics window expires")
	assert.Contains(t, current, fresh)
	schedulingConfigState.Lock()
	_, retained := schedulingConfigState.candidates[old]
	schedulingConfigState.Unlock()
	assert.False(t, retained)
	require.NotNil(t, route(old.Model), "an expired alias is registered again on its next use")
}

// An alias routes with its normalized model's settings, so a shared capacity
// key configured for the pattern also limits traffic for concrete names.
func TestSchedulingAliasInheritsNormalizedConfig(t *testing.T) {
	db := setupChannelSelectAutoGroupsTest(t)
	const pattern = "gpt-4-gizmo-*"
	one := 1
	setSchedulingTestConfig(t, []SchedulingConfig{{Group: "default", Model: pattern, Enabled: true, TargetSuccessRate: .95, TargetTTFTMS: 3000,
		ChannelOverrides: []SchedulingChannelOverride{{ChannelID: 7711, Capacity: &one, CapacityKey: "shared-account"}}}})
	createChannelSelectAutoGroupsChannel(t, db, 7711, "default", pattern)
	model.InitChannelCache()
	require.NoError(t, SyncSchedulingConfig())
	channel, err := model.CacheGetChannelForRouting(7711)
	require.NoError(t, err)

	holder := routingTestContext(t)
	defer EndSchedulingRequest(holder)
	require.NoError(t, ReserveSchedulingChannel(holder, channel, "default", "gpt-4-gizmo-first"))
	second := routingTestContext(t)
	defer EndSchedulingRequest(second)
	assert.ErrorIs(t, ReserveSchedulingChannel(second, channel, "default", "gpt-4-gizmo-second"), scheduler.ErrNoEligibleChannel,
		"another alias of the same pattern shares the configured capacity key")
}

func TestRealtimeConnectionDoesNotHoldGenerationCapacity(t *testing.T) {
	db := setupChannelSelectAutoGroupsTest(t)
	const name = "realtime-capacity"
	setSchedulingTestConfig(t, []SchedulingConfig{{Group: "default", Model: name, Enabled: true, TargetSuccessRate: .95, TargetTTFTMS: 3000, DefaultCapacity: 1}})
	createChannelSelectAutoGroupsChannel(t, db, 7801, "default", name)
	model.InitChannelCache()
	require.NoError(t, SyncSchedulingConfig())
	channel, err := model.CacheGetChannelForRouting(7801)
	require.NoError(t, err)
	connect := func() *gin.Context {
		c := routingTestContext(t)
		c.Request = httptest.NewRequest(http.MethodGet, "/v1/realtime?model="+name, nil)
		c.Request.Header.Set("Upgrade", "websocket")
		return c
	}
	// Distribute selects the channel, then the relay reserves it again.
	sockets := []*gin.Context{connect(), connect()}
	for _, socket := range sockets {
		selected, _, selectErr := SelectChannelForRequest(socket, name, &RetryParam{Ctx: socket, TokenGroup: "default", ModelName: name})
		require.Nil(t, selectErr)
		require.Equal(t, 7801, selected.Id)
		require.NoError(t, ReserveSchedulingChannel(socket, channel, "default", name))
	}
	key := scheduler.Key{Group: "default", Model: name}
	assert.Zero(t, scheduler.Default.Snapshot(key, nil, true).Summary.InFlight, "idle sockets hold no capacity")

	// Each generation runs on its own context and reserves the slot.
	generation := routingTestContext(t)
	require.NoError(t, ReserveSchedulingChannel(generation, channel, "default", name))
	assert.ErrorIs(t, ReserveSchedulingChannel(routingTestContext(t), channel, "default", name), scheduler.ErrNoEligibleChannel, "a concurrent generation respects capacity")
	EndSchedulingRequest(generation)
	require.NoError(t, ReserveSchedulingChannel(routingTestContext(t), channel, "default", name))
}
