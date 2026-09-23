package service

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/scheduler"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/setting"
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
	info := &relaycommon.RelayInfo{UsingGroup: "default", OriginModelName: modelName}
	StartSchedulingAttempt(c, info)
	_, err = c.Writer.WriteString(`{"choices":[{"message":{"content":"complete"}}]}`)
	require.NoError(t, err)
	cancel()
	FinishSchedulingAttempt(c, info, nil)
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
