package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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
		"channel_affinity_setting.rules":        `[{"name":"full-channel","model_regex":[".*"],"key_sources":[{"type":"request_header","key":"X-Session"}]}]`,
	})
	require.NoError(t, err)
	*affinity = policy.Affinity
	strict, _ := gin.CreateTestContext(httptest.NewRecorder())
	strict.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	strict.Request.Header.Set("X-Session", t.Name())
	defer EndSchedulingRequest(strict)
	_, found := GetPreferredChannelByAffinity(strict, modelName, "default")
	require.False(t, found)
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
