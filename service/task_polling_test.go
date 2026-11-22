package service

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	taskdto "github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	hosttypes "github.com/QuantumNous/new-api/types"
	"github.com/bytedance/gopkg/util/gopool"
	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
	"gorm.io/gorm/schema"
)

type taskPollingFetchAdaptor struct {
	mu           sync.Mutex
	initInfo     *relaycommon.RelayInfo
	taskIDs      []string
	fetched      chan string
	blockTaskID  string
	blockStarted chan struct{}
	releaseBlock chan struct{}
	blockOnce    sync.Once
}

type batchPollingAdaptor struct {
	taskPollingFetchAdaptor
	batchCalls int
	batchIDs   []string
	results    map[string]*BatchTaskResult
}

func (a *batchPollingAdaptor) FetchMode() string { return "batch" }
func (a *batchPollingAdaptor) FetchBatchTasks(_ string, _ string, tasks []*model.Task, _ string) (*http.Response, error) {
	a.batchCalls++
	a.batchIDs = a.batchIDs[:0]
	for _, task := range tasks {
		a.batchIDs = append(a.batchIDs, task.GetUpstreamTaskID())
	}
	return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(bytes.NewReader([]byte(`{}`)))}, nil
}
func (a *batchPollingAdaptor) ParseBatchResult(_ []*model.Task, _ *http.Response, _ []byte) (map[string]*BatchTaskResult, error) {
	if a.results != nil {
		return a.results, nil
	}
	results := make(map[string]*BatchTaskResult, len(a.batchIDs))
	for _, taskID := range a.batchIDs {
		results[taskID] = &BatchTaskResult{TaskInfo: relaycommon.TaskInfo{TaskID: taskID, Status: model.TaskStatusInProgress, Url: "https://example.com/result"}}
	}
	return results, nil
}

func (a *taskPollingFetchAdaptor) Init(info *relaycommon.RelayInfo) {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.initInfo = info
}
func (a *taskPollingFetchAdaptor) initChannelMeta() *relaycommon.ChannelMeta {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.initInfo == nil {
		return nil
	}
	return a.initInfo.ChannelMeta
}

func (a *taskPollingFetchAdaptor) FetchTask(_ string, _ string, task *model.Task, _ string) (*http.Response, error) {
	taskID := ""
	if task != nil {
		taskID = task.GetUpstreamTaskID()
	}
	if taskID == a.blockTaskID && a.releaseBlock != nil {
		a.blockOnce.Do(func() {
			if a.blockStarted != nil {
				close(a.blockStarted)
			}
		})
		<-a.releaseBlock
	}

	a.mu.Lock()
	a.taskIDs = append(a.taskIDs, taskID)
	a.mu.Unlock()
	if a.fetched != nil {
		select {
		case a.fetched <- taskID:
		default:
		}
	}

	response := taskdto.TaskResponse[model.Task]{
		Code: taskdto.TaskSuccessCode,
		Data: model.Task{
			TaskID:   taskID,
			Status:   model.TaskStatusInProgress,
			Progress: "30%",
		},
	}
	responseBody, err := common.Marshal(response)
	if err != nil {
		return nil, err
	}
	return &http.Response{
		StatusCode: http.StatusOK,
		Body:       io.NopCloser(bytes.NewReader(responseBody)),
	}, nil
}

func (a *taskPollingFetchAdaptor) ParseTaskResult(*model.Task, *http.Response, []byte) (*relaycommon.TaskInfo, error) {
	return &relaycommon.TaskInfo{Status: model.TaskStatusInProgress}, nil
}

func (a *taskPollingFetchAdaptor) AdjustBillingOnComplete(_ *model.Task, _ *relaycommon.TaskInfo) int {
	return 0
}

func (a *taskPollingFetchAdaptor) fetchCount() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	return len(a.taskIDs)
}

func (a *taskPollingFetchAdaptor) fetchedTaskIDs() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]string(nil), a.taskIDs...)
}

func TestRedactVideoResponseBodyPreservesPollingPayloadShape(t *testing.T) {
	rawVideo := strings.Repeat("a", 300)
	body, err := common.Marshal(map[string]any{
		"done": true,
		"name": "operations/provider-task",
		"response": map[string]any{
			"bytesBase64Encoded": "secret-bytes",
			"video":              rawVideo,
			"videos": []any{
				map[string]any{
					"bytesBase64Encoded": "other-secret-bytes",
					"mimeType":           "video/mp4",
					"uri":                "https://media.example/video.mp4",
				},
			},
		},
	})
	require.NoError(t, err)

	var stored map[string]any
	require.NoError(t, common.Unmarshal(redactVideoResponseBody(body), &stored))
	assert.Equal(t, true, stored["done"])
	assert.Equal(t, "operations/provider-task", stored["name"])
	response, ok := stored["response"].(map[string]any)
	require.True(t, ok)
	assert.NotContains(t, response, "bytesBase64Encoded")
	assert.Equal(t, strings.Repeat("a", 256)+"...", response["video"])
	videos, ok := response["videos"].([]any)
	require.True(t, ok)
	require.Len(t, videos, 1)
	video, ok := videos[0].(map[string]any)
	require.True(t, ok)
	assert.NotContains(t, video, "bytesBase64Encoded")
	assert.Equal(t, "video/mp4", video["mimeType"])
	assert.Equal(t, "https://media.example/video.mp4", video["uri"])
}

func TestRedactVideoResponseBodyDropsInteractionInlineVideo(t *testing.T) {
	body := []byte(`{"id":"int_1","status":"completed","steps":[{"type":"model_output","content":[{"type":"video","mime_type":"video/mp4","data":"` + strings.Repeat("a", 1024) + `"},{"type":"text","text":"done"}]}]}`)

	var stored map[string]any
	require.NoError(t, common.Unmarshal(redactVideoResponseBody(body), &stored))
	content := stored["steps"].([]any)[0].(map[string]any)["content"].([]any)
	assert.Equal(t, map[string]any{"type": "video", "mime_type": "video/mp4"}, content[0])
	assert.Equal(t, "done", content[1].(map[string]any)["text"])
}

func TestRedactVideoResponseBodyKeepsUntouchedBytes(t *testing.T) {
	body := []byte(`{"name":"op","response":{"videos":[{"uri":"u","bytesBase64Encoded":"x"}],"durationSeconds":8.0,"bytesBase64Encoded":"y"},"done":true}`)

	assert.Equal(t, `{"name":"op","response":{"videos":[{"uri":"u"}],"durationSeconds":8.0},"done":true}`, string(redactVideoResponseBody(body)))
}

func seedTaskPollingChannel(t *testing.T, id int, disableSleep bool) {
	t.Helper()
	ch := &model.Channel{
		Id:     id,
		Type:   constant.ChannelTypeKling,
		Name:   "polling_channel",
		Key:    "sk-test",
		Status: common.ChannelStatusEnabled,
	}
	if disableSleep {
		ch.SetOtherSettings(dto.ChannelOtherSettings{DisableTaskPollingSleep: true})
	}
	require.NoError(t, model.DB.Create(ch).Error)
}

func seedPollingTask(t *testing.T, channelID int, publicID string, upstreamID string) *model.Task {
	t.Helper()
	task := &model.Task{
		TaskID:    publicID,
		Platform:  constant.TaskPlatform("kling"),
		UserId:    1,
		ChannelId: channelID,
		Action:    constant.TaskActionImageToVideo,
		Status:    model.TaskStatusInProgress,
		Progress:  "30%",
		CreatedAt: time.Now().Unix(),
		UpdatedAt: time.Now().Unix(),
		PrivateData: model.TaskPrivateData{
			UpstreamTaskID: upstreamID,
		},
	}
	require.NoError(t, model.DB.Create(task).Error)
	return task
}

func TestUpdateVideoTasksDefaultSleepWaitsBetweenTasks(t *testing.T) {
	truncate(t)

	const channelID = 101
	seedTaskPollingChannel(t, channelID, false)
	first := seedPollingTask(t, channelID, "task_public_1", "upstream_1")
	second := seedPollingTask(t, channelID, "task_public_2", "upstream_2")

	adaptor := &taskPollingFetchAdaptor{}
	previousFactory := GetTaskAdaptorFunc
	GetTaskAdaptorFunc = func(constant.TaskPlatform) TaskPollingAdaptor { return adaptor }
	t.Cleanup(func() { GetTaskAdaptorFunc = previousFactory })

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := UpdateVideoTasks(ctx, constant.TaskPlatform("kling"), map[int][]string{
		channelID: {
			first.GetUpstreamTaskID(),
			second.GetUpstreamTaskID(),
		},
	}, map[string]*model.Task{
		first.GetUpstreamTaskID():  first,
		second.GetUpstreamTaskID(): second,
	})

	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.Equal(t, 1, adaptor.fetchCount())
}

func TestPollingPassesExecutingChannelTypeToAdaptor(t *testing.T) {
	truncate(t)
	const channelID = 112
	baseURL := "https://gateway.example"
	gateway := &model.Channel{Id: channelID, Type: constant.ChannelTypeNewAPI, Name: "gateway", Key: "sk-gateway", Status: common.ChannelStatusEnabled, BaseURL: &baseURL}
	gateway.HeaderOverride = common.GetPointer(`{"X-API-Key":"{api_key}"}`)
	gateway.SetOtherSettings(dto.ChannelOtherSettings{DisableTaskPollingSleep: true})
	require.NoError(t, model.DB.Create(gateway).Error)
	task := seedPollingTask(t, channelID, "task_gateway", "upstream_gateway")
	taskChannels := map[int][]string{channelID: {task.GetUpstreamTaskID()}}
	tasks := map[string]*model.Task{task.GetUpstreamTaskID(): task}

	perTask := &taskPollingFetchAdaptor{}
	previousFactory := GetTaskAdaptorFunc
	GetTaskAdaptorFunc = func(constant.TaskPlatform) TaskPollingAdaptor { return perTask }
	t.Cleanup(func() { GetTaskAdaptorFunc = previousFactory })
	require.NoError(t, UpdateVideoTasks(context.Background(), constant.TaskPlatform("kling"), taskChannels, tasks))
	meta := perTask.initChannelMeta()
	require.NotNil(t, meta, "per-task polling initializes the adaptor with channel metadata")
	assert.Equal(t, constant.ChannelTypeNewAPI, meta.ChannelType, "the adaptor derives the upstream kind from the channel type")
	assert.Equal(t, channelID, meta.ChannelId)
	assert.Equal(t, baseURL, meta.ChannelBaseUrl)
	assert.Equal(t, map[string]any{"X-API-Key": "{api_key}"}, meta.HeadersOverride)

	batch := &batchPollingAdaptor{}
	require.NoError(t, UpdateBatchTasks(context.Background(), batch, taskChannels, tasks))
	meta = batch.initChannelMeta()
	require.NotNil(t, meta, "batch polling initializes the adaptor with channel metadata")
	assert.Equal(t, constant.ChannelTypeNewAPI, meta.ChannelType)
	assert.Equal(t, channelID, meta.ChannelId)
	assert.Equal(t, baseURL, meta.ChannelBaseUrl)
	assert.Equal(t, map[string]any{"X-API-Key": "{api_key}"}, meta.HeadersOverride)
}

func TestDispatchPlatformUpdateUsesFetchMode(t *testing.T) {
	truncate(t)
	const channelID = 109
	seedTaskPollingChannel(t, channelID, true)
	task := seedPollingTask(t, channelID, "task_batch", "upstream_batch")
	taskChannels := map[int][]string{channelID: {task.GetUpstreamTaskID()}}
	tasks := map[string]*model.Task{task.GetUpstreamTaskID(): task}

	batch := &batchPollingAdaptor{}
	previousFactory := GetTaskAdaptorFunc
	GetTaskAdaptorFunc = func(constant.TaskPlatform) TaskPollingAdaptor { return batch }
	DispatchPlatformUpdate(context.Background(), "batch-plugin", taskChannels, tasks)
	assert.Equal(t, 1, batch.batchCalls)
	assert.Equal(t, 0, batch.fetchCount())
	var persisted model.Task
	require.NoError(t, model.DB.First(&persisted, task.ID).Error)
	assert.Equal(t, "https://example.com/result", persisted.GetResultURL())

	perTask := &taskPollingFetchAdaptor{}
	GetTaskAdaptorFunc = func(constant.TaskPlatform) TaskPollingAdaptor { return perTask }
	DispatchPlatformUpdate(context.Background(), "per-task-plugin", taskChannels, tasks)
	assert.Equal(t, 1, perTask.fetchCount())

	GetTaskAdaptorFunc = func(constant.TaskPlatform) TaskPollingAdaptor { return nil }
	assert.NotPanics(t, func() { DispatchPlatformUpdate(context.Background(), "missing-plugin", taskChannels, tasks) })
	GetTaskAdaptorFunc = previousFactory
}

func TestUpdateBatchTasksSettlesTieredUsageForTerminalStates(t *testing.T) {
	testCases := []struct {
		name        string
		status      model.TaskStatus
		units       float64
		actualQuota int
	}{
		{name: "success with usage", status: model.TaskStatusSuccess, units: 3, actualQuota: 3_000},
		{name: "failure with usage", status: model.TaskStatusFailure, units: 3, actualQuota: 0},
		{name: "success with zero usage", status: model.TaskStatusSuccess, units: 0, actualQuota: 0},
		{name: "failure with zero usage", status: model.TaskStatusFailure, units: 0, actualQuota: 0},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			truncate(t)

			const userID, tokenID, channelID = 41, 41, 141
			const initialQuota, preConsumedQuota = 10_000, 5_000
			const tokenRemain = 8_000
			seedUser(t, userID, initialQuota)
			seedToken(t, tokenID, userID, "sk-batch-tiered", tokenRemain)
			seedTaskPollingChannel(t, channelID, true)

			expression := `tier("actual", u("units"))`
			task := makeTask(userID, channelID, preConsumedQuota, tokenID, BillingSourceWallet, 0)
			task.TaskID = "task_batch_tiered_" + string(testCase.status)
			task.Platform = "batch-plugin"
			task.PrivateData.UpstreamTaskID = "upstream_batch_tiered_" + string(testCase.status)
			task.SetData(map[string]any{"provider_payload": "must-be-preserved"})
			task.PrivateData.BillingContext.TieredSnapshot = &billingexpr.BillingSnapshot{
				ExprString:       expression,
				ExprHash:         billingexpr.ExprHashString(expression),
				GroupRatio:       1,
				QuotaPerUnit:     1_000,
				ExprVersion:      1,
				TaskUsageBilling: true,
			}
			require.NoError(t, model.DB.Create(task).Error)

			upstreamID := task.GetUpstreamTaskID()
			reason := ""
			if testCase.status == model.TaskStatusFailure {
				reason = "upstream failed"
			}
			result := &BatchTaskResult{TaskInfo: relaycommon.TaskInfo{
				TaskID:     upstreamID,
				Status:     string(testCase.status),
				Reason:     reason,
				UsageFacts: map[string]any{"units": testCase.units},
			}}
			adaptor := &batchPollingAdaptor{results: map[string]*BatchTaskResult{upstreamID: result}}
			taskIDs := []string{upstreamID}
			taskMap := map[string]*model.Task{upstreamID: task}

			require.NoError(t, UpdateBatchTasks(context.Background(), adaptor, map[int][]string{channelID: taskIDs}, taskMap))

			var persisted model.Task
			require.NoError(t, model.DB.First(&persisted, task.ID).Error)
			assert.Equal(t, testCase.status, persisted.Status)
			assert.Equal(t, testCase.actualQuota, persisted.Quota)
			var persistedData map[string]any
			require.NoError(t, common.Unmarshal(persisted.Data, &persistedData))
			assert.Equal(t, "must-be-preserved", persistedData["provider_payload"])
			assert.Equal(t, initialQuota+(preConsumedQuota-testCase.actualQuota), getUserQuota(t, userID))
			assert.Equal(t, tokenRemain+(preConsumedQuota-testCase.actualQuota), getTokenRemainQuota(t, tokenID))
			assert.Equal(t, int64(1), countLogs(t))
			if testCase.status == model.TaskStatusFailure {
				log := getLastLog(t)
				require.NotNil(t, log)
				assert.Equal(t, model.LogTypeRefund, log.Type)
			}

			// A duplicate terminal response must not settle the same task twice.
			require.NoError(t, UpdateBatchTasks(context.Background(), adaptor, map[int][]string{channelID: taskIDs}, taskMap))
			assert.Equal(t, initialQuota+(preConsumedQuota-testCase.actualQuota), getUserQuota(t, userID))
			assert.Equal(t, int64(1), countLogs(t))
		})
	}
}

func TestUpdateBatchTasksRefundsFailedTieredTask(t *testing.T) {
	truncate(t)

	const userID, tokenID, channelID = 43, 43, 143
	const initialQuota, preConsumedQuota, tokenRemain = 10_000, 5_000, 8_000
	seedUser(t, userID, initialQuota)
	seedToken(t, tokenID, userID, "sk-batch-tiered-refund", tokenRemain)
	seedTaskPollingChannel(t, channelID, true)

	expression := `tier("actual", u("units"))`
	task := makeTask(userID, channelID, preConsumedQuota, tokenID, BillingSourceWallet, 0)
	task.TaskID = "task_batch_tiered_refund"
	task.Platform = "batch-plugin"
	task.PrivateData.UpstreamTaskID = "upstream_batch_tiered_refund"
	task.PrivateData.BillingContext.TieredSnapshot = &billingexpr.BillingSnapshot{
		ExprString:       expression,
		ExprHash:         billingexpr.ExprHashString(expression),
		GroupRatio:       1,
		QuotaPerUnit:     1_000,
		ExprVersion:      1,
		TaskUsageBilling: true,
		UsageFacts:       map[string]any{"units": float64(5)},
		EstimatedTier:    "actual",
	}
	require.NoError(t, model.DB.Create(task).Error)

	upstreamID := task.GetUpstreamTaskID()
	adaptor := &batchPollingAdaptor{results: map[string]*BatchTaskResult{
		upstreamID: {TaskInfo: relaycommon.TaskInfo{
			TaskID:     upstreamID,
			Status:     model.TaskStatusFailure,
			Reason:     "upstream failed",
			UsageFacts: map[string]any{"units": float64(5)},
		}},
	}}
	require.NoError(t, UpdateBatchTasks(context.Background(), adaptor, map[int][]string{channelID: {upstreamID}}, map[string]*model.Task{upstreamID: task}))

	var persisted model.Task
	require.NoError(t, model.DB.First(&persisted, task.ID).Error)
	assert.EqualValues(t, model.TaskStatusFailure, persisted.Status)
	assert.Zero(t, persisted.Quota)
	assert.Equal(t, initialQuota+preConsumedQuota, getUserQuota(t, userID))
	assert.Equal(t, tokenRemain+preConsumedQuota, getTokenRemainQuota(t, tokenID))

	log := getLastLog(t)
	require.NotNil(t, log)
	assert.Equal(t, model.LogTypeRefund, log.Type)
	assert.Equal(t, preConsumedQuota, log.Quota)
	var other map[string]any
	require.NoError(t, common.UnmarshalJsonStr(log.Other, &other))
	assert.Equal(t, "tiered_expr", other["billing_mode"])
	assert.Equal(t, "actual", other["matched_tier"])
	facts, ok := other["usage_facts"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, map[string]any{"units": float64(5)}, facts)
}

func TestUpdateBatchTasksRefundsFailedTaskWithoutUsageSettlement(t *testing.T) {
	truncate(t)

	const userID, tokenID, channelID = 42, 42, 142
	const initialQuota, preConsumedQuota, tokenRemain = 10_000, 4_000, 7_000
	seedUser(t, userID, initialQuota)
	seedToken(t, tokenID, userID, "sk-batch-refund", tokenRemain)
	seedTaskPollingChannel(t, channelID, true)

	task := makeTask(userID, channelID, preConsumedQuota, tokenID, BillingSourceWallet, 0)
	task.TaskID = "task_batch_refund"
	task.Platform = "batch-plugin"
	task.Properties.OriginModelName = "missing-batch-token-price"
	task.PrivateData.UpstreamTaskID = "upstream_batch_refund"
	task.PrivateData.BillingContext.OriginModelName = "missing-batch-token-price"
	require.NoError(t, model.DB.Create(task).Error)

	upstreamID := task.GetUpstreamTaskID()
	adaptor := &batchPollingAdaptor{results: map[string]*BatchTaskResult{
		upstreamID: {TaskInfo: relaycommon.TaskInfo{TaskID: upstreamID, Status: model.TaskStatusFailure, Reason: "upstream failed", TotalTokens: 123}},
	}}
	require.NoError(t, UpdateBatchTasks(context.Background(), adaptor, map[int][]string{channelID: {upstreamID}}, map[string]*model.Task{upstreamID: task}))

	assert.Equal(t, initialQuota+preConsumedQuota, getUserQuota(t, userID))
	assert.Equal(t, tokenRemain+preConsumedQuota, getTokenRemainQuota(t, tokenID))
	log := getLastLog(t)
	require.NotNil(t, log)
	assert.Equal(t, model.LogTypeRefund, log.Type)
}

func TestUpdateVideoTasksCanSkipPollingSleepPerChannel(t *testing.T) {
	truncate(t)

	const channelID = 102
	seedTaskPollingChannel(t, channelID, true)
	first := seedPollingTask(t, channelID, "task_public_3", "upstream_3")
	second := seedPollingTask(t, channelID, "task_public_4", "upstream_4")

	adaptor := &taskPollingFetchAdaptor{}
	previousFactory := GetTaskAdaptorFunc
	GetTaskAdaptorFunc = func(constant.TaskPlatform) TaskPollingAdaptor { return adaptor }
	t.Cleanup(func() { GetTaskAdaptorFunc = previousFactory })

	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()

	err := UpdateVideoTasks(ctx, constant.TaskPlatform("kling"), map[int][]string{
		channelID: {
			first.GetUpstreamTaskID(),
			second.GetUpstreamTaskID(),
		},
	}, map[string]*model.Task{
		first.GetUpstreamTaskID():  first,
		second.GetUpstreamTaskID(): second,
	})

	require.NoError(t, err)
	assert.Equal(t, 2, adaptor.fetchCount())
}

func TestUpdateVideoTasksDefaultSleepDoesNotBlockOtherChannels(t *testing.T) {
	truncate(t)

	const firstChannelID = 201
	const secondChannelID = 202
	seedTaskPollingChannel(t, firstChannelID, false)
	seedTaskPollingChannel(t, secondChannelID, false)
	firstChannelFirst := seedPollingTask(t, firstChannelID, "task_public_5", "upstream_a_1")
	firstChannelSecond := seedPollingTask(t, firstChannelID, "task_public_6", "upstream_a_2")
	secondChannelFirst := seedPollingTask(t, secondChannelID, "task_public_7", "upstream_b_1")
	secondChannelSecond := seedPollingTask(t, secondChannelID, "task_public_8", "upstream_b_2")

	adaptor := &taskPollingFetchAdaptor{}
	previousFactory := GetTaskAdaptorFunc
	GetTaskAdaptorFunc = func(constant.TaskPlatform) TaskPollingAdaptor { return adaptor }
	t.Cleanup(func() { GetTaskAdaptorFunc = previousFactory })

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	err := UpdateVideoTasks(ctx, constant.TaskPlatform("kling"), map[int][]string{
		firstChannelID: {
			firstChannelFirst.GetUpstreamTaskID(),
			firstChannelSecond.GetUpstreamTaskID(),
		},
		secondChannelID: {
			secondChannelFirst.GetUpstreamTaskID(),
			secondChannelSecond.GetUpstreamTaskID(),
		},
	}, map[string]*model.Task{
		firstChannelFirst.GetUpstreamTaskID():   firstChannelFirst,
		firstChannelSecond.GetUpstreamTaskID():  firstChannelSecond,
		secondChannelFirst.GetUpstreamTaskID():  secondChannelFirst,
		secondChannelSecond.GetUpstreamTaskID(): secondChannelSecond,
	})

	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.ElementsMatch(t, []string{"upstream_a_1", "upstream_b_1"}, adaptor.fetchedTaskIDs())
}

func TestUpdateVideoTasksSlowChannelDoesNotBlockOtherChannels(t *testing.T) {
	truncate(t)

	const slowChannelID = 251
	const fastChannelID = 252
	seedTaskPollingChannel(t, slowChannelID, false)
	seedTaskPollingChannel(t, fastChannelID, true)
	slowTask := seedPollingTask(t, slowChannelID, "task_public_slow", "upstream_slow_1")
	fastFirst := seedPollingTask(t, fastChannelID, "task_public_fast_1", "upstream_fast_parallel_1")
	fastSecond := seedPollingTask(t, fastChannelID, "task_public_fast_2", "upstream_fast_parallel_2")

	slowID, fastFirstID, fastSecondID := slowTask.GetUpstreamTaskID(), fastFirst.GetUpstreamTaskID(), fastSecond.GetUpstreamTaskID()
	adaptor := &taskPollingFetchAdaptor{
		fetched:      make(chan string, 4),
		blockTaskID:  slowID,
		blockStarted: make(chan struct{}),
		releaseBlock: make(chan struct{}),
	}
	var releaseOnce sync.Once
	releaseBlockedTask := func() {
		releaseOnce.Do(func() {
			close(adaptor.releaseBlock)
		})
	}
	previousFactory := GetTaskAdaptorFunc
	GetTaskAdaptorFunc = func(constant.TaskPlatform) TaskPollingAdaptor { return adaptor }
	done := make(chan struct{})
	t.Cleanup(func() {
		releaseBlockedTask()
		<-done
		GetTaskAdaptorFunc = previousFactory
	})
	var pollErr error
	gopool.Go(func() {
		defer close(done)
		pollErr = UpdateVideoTasks(context.Background(), constant.TaskPlatform("kling"), map[int][]string{
			slowChannelID: {
				slowID,
			},
			fastChannelID: {
				fastFirstID,
				fastSecondID,
			},
		}, map[string]*model.Task{
			slowID:       slowTask,
			fastFirstID:  fastFirst,
			fastSecondID: fastSecond,
		})
	})

	select {
	case <-adaptor.blockStarted:
	case <-time.After(500 * time.Millisecond):
		t.Fatal("slow channel did not start blocking")
	}

	for _, want := range []string{fastFirstID, fastSecondID} {
		select {
		case got := <-adaptor.fetched:
			require.Equal(t, want, got)
		case <-time.After(500 * time.Millisecond):
			t.Fatalf("fast channel did not fetch %s while slow channel was blocked", want)
		}
	}

	releaseBlockedTask()
	<-done
	require.NoError(t, pollErr)
	assert.ElementsMatch(t, []string{
		slowID,
		fastFirstID,
		fastSecondID,
	}, adaptor.fetchedTaskIDs())
}

func TestUpdateVideoTasksMixedChannelSleepSettings(t *testing.T) {
	truncate(t)

	const sleepyChannelID = 301
	const fastChannelID = 302
	seedTaskPollingChannel(t, sleepyChannelID, false)
	seedTaskPollingChannel(t, fastChannelID, true)
	sleepyFirst := seedPollingTask(t, sleepyChannelID, "task_public_9", "upstream_sleepy_1")
	sleepySecond := seedPollingTask(t, sleepyChannelID, "task_public_10", "upstream_sleepy_2")
	fastFirst := seedPollingTask(t, fastChannelID, "task_public_11", "upstream_fast_1")
	fastSecond := seedPollingTask(t, fastChannelID, "task_public_12", "upstream_fast_2")

	adaptor := &taskPollingFetchAdaptor{}
	previousFactory := GetTaskAdaptorFunc
	GetTaskAdaptorFunc = func(constant.TaskPlatform) TaskPollingAdaptor { return adaptor }
	t.Cleanup(func() { GetTaskAdaptorFunc = previousFactory })

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	err := UpdateVideoTasks(ctx, constant.TaskPlatform("kling"), map[int][]string{
		sleepyChannelID: {
			sleepyFirst.GetUpstreamTaskID(),
			sleepySecond.GetUpstreamTaskID(),
		},
		fastChannelID: {
			fastFirst.GetUpstreamTaskID(),
			fastSecond.GetUpstreamTaskID(),
		},
	}, map[string]*model.Task{
		sleepyFirst.GetUpstreamTaskID():  sleepyFirst,
		sleepySecond.GetUpstreamTaskID(): sleepySecond,
		fastFirst.GetUpstreamTaskID():    fastFirst,
		fastSecond.GetUpstreamTaskID():   fastSecond,
	})

	require.ErrorIs(t, err, context.DeadlineExceeded)
	assert.ElementsMatch(t, []string{"upstream_sleepy_1", "upstream_fast_1", "upstream_fast_2"}, adaptor.fetchedTaskIDs())
}

func TestUpdateSunoTasksStalePollsRefundExactlyOnce(t *testing.T) {
	truncate(t)

	const userID, tokenID, channelID = 401, 401, 401
	const initialUserQuota, initialTokenQuota, taskQuota = 10_000, 6_000, 2_500
	const publicTaskID, upstreamTaskID = "suno_public_refund_once", "suno_upstream_refund_once"

	seedUser(t, userID, initialUserQuota)
	seedToken(t, tokenID, userID, "sk-suno-refund-once", initialTokenQuota)
	baseURL := "https://suno.invalid"
	require.NoError(t, model.DB.Create(&model.Channel{
		Id:      channelID,
		Type:    constant.ChannelTypeSunoAPI,
		Name:    "suno_refund_once",
		Key:     "sk-suno-channel",
		Status:  common.ChannelStatusEnabled,
		BaseURL: &baseURL,
	}).Error)

	task := makeTask(userID, channelID, taskQuota, tokenID, BillingSourceWallet, 0)
	task.TaskID = publicTaskID
	task.Platform = constant.TaskPlatformSuno
	task.Status = model.TaskStatusInProgress
	task.Progress = "50%"
	task.SubmitTime = time.Now().Unix()
	task.PrivateData.UpstreamTaskID = upstreamTaskID
	require.NoError(t, model.DB.Create(task).Error)

	var firstPollTask model.Task
	var staleSecondPollTask model.Task
	require.NoError(t, model.DB.First(&firstPollTask, task.ID).Error)
	require.NoError(t, model.DB.First(&staleSecondPollTask, task.ID).Error)

	adaptor := &batchPollingAdaptor{results: map[string]*BatchTaskResult{
		upstreamTaskID: {TaskInfo: relaycommon.TaskInfo{
			TaskID: upstreamTaskID,
			Status: model.TaskStatusFailure,
			Reason: "upstream failed",
		}},
	}}
	previousFactory := GetTaskAdaptorFunc
	GetTaskAdaptorFunc = func(constant.TaskPlatform) TaskPollingAdaptor { return adaptor }
	t.Cleanup(func() { GetTaskAdaptorFunc = previousFactory })

	require.NoError(t, updateBatchTasks(context.Background(), adaptor, channelID, []string{upstreamTaskID}, map[string]*model.Task{
		upstreamTaskID: &firstPollTask,
	}))
	require.NoError(t, updateBatchTasks(context.Background(), adaptor, channelID, []string{upstreamTaskID}, map[string]*model.Task{
		upstreamTaskID: &staleSecondPollTask,
	}))

	var reloaded model.Task
	require.NoError(t, model.DB.First(&reloaded, task.ID).Error)
	assert.EqualValues(t, model.TaskStatusFailure, reloaded.Status)
	assert.Zero(t, reloaded.Quota)
	assert.Equal(t, initialUserQuota+taskQuota, getUserQuota(t, userID))
	assert.Equal(t, initialTokenQuota+taskQuota, getTokenRemainQuota(t, tokenID))
	assert.Equal(t, int64(1), countLogs(t))
}

func TestRunTaskPollingOnceDoesNotRefundHistoricalFailedTask(t *testing.T) {
	truncate(t)

	const userID, initialQuota, taskQuota = 402, 10_000, 1_200
	seedUser(t, userID, initialQuota)

	task := makeTask(userID, 0, taskQuota, 0, BillingSourceWallet, 0)
	task.TaskID = "historical_failed_already_refunded"
	task.Status = model.TaskStatusFailure
	task.Progress = "100%"
	task.SubmitTime = time.Now().Add(-90 * 24 * time.Hour).Unix()
	task.UpdatedAt = time.Now().Add(-time.Minute).Unix()
	require.NoError(t, model.DB.Create(task).Error)

	previousFactory := GetTaskAdaptorFunc
	GetTaskAdaptorFunc = func(constant.TaskPlatform) TaskPollingAdaptor {
		return &taskPollingFetchAdaptor{}
	}
	t.Cleanup(func() { GetTaskAdaptorFunc = previousFactory })

	summary := RunTaskPollingOnce(context.Background(), nil)

	assert.Zero(t, summary.UnfinishedTasks)
	assert.Equal(t, initialQuota, getUserQuota(t, userID))
	assert.Equal(t, taskQuota, getTaskQuota(t, task.ID))
	assert.Equal(t, int64(0), countLogs(t))
}

func TestSweepTimedOutTasksHonorsRefundRolloutBoundary(t *testing.T) {
	truncate(t)

	const (
		userID          = 403
		initialQuota    = 10_000
		legacyTaskQuota = 1_800
		modernTaskQuota = 1_200
	)
	seedUser(t, userID, initialQuota)

	legacyTask := makeTask(userID, 0, legacyTaskQuota, 0, BillingSourceWallet, 0)
	legacyTask.TaskID = "legacy_timeout_without_refund"
	legacyTask.Progress = "50%"
	legacyTask.SubmitTime = 1771718399 // 2026-02-21 23:59:59 UTC
	require.NoError(t, model.DB.Create(legacyTask).Error)

	modernTask := makeTask(userID, 0, modernTaskQuota, 0, BillingSourceWallet, 0)
	modernTask.TaskID = "modern_timeout_with_refund"
	modernTask.Progress = "50%"
	modernTask.SubmitTime = 1771718400 // 2026-02-22 00:00:00 UTC
	require.NoError(t, model.DB.Create(modernTask).Error)

	previousTimeout := constant.TaskTimeoutMinutes
	constant.TaskTimeoutMinutes = 1
	t.Cleanup(func() { constant.TaskTimeoutMinutes = previousTimeout })

	sweepTimedOutTasks(context.Background())

	var reloadedLegacy model.Task
	var reloadedModern model.Task
	require.NoError(t, model.DB.First(&reloadedLegacy, legacyTask.ID).Error)
	require.NoError(t, model.DB.First(&reloadedModern, modernTask.ID).Error)
	assert.EqualValues(t, model.TaskStatusFailure, reloadedLegacy.Status)
	assert.EqualValues(t, model.TaskStatusFailure, reloadedModern.Status)
	assert.True(t, reloadedLegacy.PerformanceSamplePending, "timeout telemetry is saved with the terminal CAS")
	assert.True(t, reloadedModern.PerformanceSamplePending)
	assert.Zero(t, reloadedLegacy.Quota)
	assert.Zero(t, reloadedModern.Quota)
	assert.Contains(t, reloadedLegacy.FailReason, "old system")
	assert.Equal(t, "Task timed out", reloadedModern.FailReason)
	assert.Equal(t, initialQuota+modernTaskQuota, getUserQuota(t, userID))
	assert.Equal(t, int64(1), countLogs(t))
}

// TASK_TIMEOUT_MINUTES defaults to 72 hours, the longest a vendor keeps a
// task running (Ark can be configured to 72h, Vidu off-peak runs 48h).
func TestSweepTimedOutTasksAtTheDefaultDeadline(t *testing.T) {
	truncate(t)

	const userID, initialQuota, taskQuota = 404, 10_000, 1_500
	seedUser(t, userID, initialQuota)
	running := makeTask(userID, 0, taskQuota, 0, BillingSourceWallet, 0)
	running.TaskID = "running_for_60_hours"
	running.Progress = "50%"
	running.SubmitTime = time.Now().Add(-60 * time.Hour).Unix()
	require.NoError(t, model.DB.Create(running).Error)
	stuck := makeTask(userID, 0, taskQuota, 0, BillingSourceWallet, 0)
	stuck.TaskID = "running_for_73_hours"
	stuck.Progress = "50%"
	stuck.SubmitTime = time.Now().Add(-73 * time.Hour).Unix()
	require.NoError(t, model.DB.Create(stuck).Error)

	previousTimeout := constant.TaskTimeoutMinutes
	constant.TaskTimeoutMinutes = 4320
	t.Cleanup(func() { constant.TaskTimeoutMinutes = previousTimeout })

	sweepTimedOutTasks(context.Background())

	var reloadedRunning, reloadedStuck model.Task
	require.NoError(t, model.DB.First(&reloadedRunning, running.ID).Error)
	require.NoError(t, model.DB.First(&reloadedStuck, stuck.ID).Error)
	assert.NotEqualValues(t, model.TaskStatusFailure, reloadedRunning.Status)
	assert.Equal(t, taskQuota, reloadedRunning.Quota)
	assert.EqualValues(t, model.TaskStatusFailure, reloadedStuck.Status)
	assert.Equal(t, "Task timed out", reloadedStuck.FailReason)
	assert.Equal(t, initialQuota+taskQuota, getUserQuota(t, userID))
}

type scriptedPollingAdaptor struct {
	statusCode int
	body       []byte
	fetchErr   error
	parse      *relaycommon.TaskInfo
	parseErr   error
	adjust     int
}

func (a *scriptedPollingAdaptor) Init(*relaycommon.RelayInfo) {}
func (a *scriptedPollingAdaptor) FetchTask(string, string, *model.Task, string) (*http.Response, error) {
	if a.fetchErr != nil {
		return nil, a.fetchErr
	}
	code := a.statusCode
	if code == 0 {
		code = http.StatusOK
	}
	body := a.body
	if body == nil {
		body = []byte(`{}`)
	}
	return &http.Response{StatusCode: code, Body: io.NopCloser(bytes.NewReader(body))}, nil
}
func (a *scriptedPollingAdaptor) ParseTaskResult(*model.Task, *http.Response, []byte) (*relaycommon.TaskInfo, error) {
	if a.parseErr != nil {
		return nil, a.parseErr
	}
	if a.parse != nil {
		return a.parse, nil
	}
	return &relaycommon.TaskInfo{Status: model.TaskStatusInProgress}, nil
}
func (a *scriptedPollingAdaptor) AdjustBillingOnComplete(*model.Task, *relaycommon.TaskInfo) int {
	return a.adjust
}

type scriptedBatchPollingAdaptor struct {
	scriptedPollingAdaptor
	results map[string]*BatchTaskResult
}

func (a *scriptedBatchPollingAdaptor) FetchMode() string { return "batch" }
func (a *scriptedBatchPollingAdaptor) FetchBatchTasks(string, string, []*model.Task, string) (*http.Response, error) {
	return a.FetchTask("", "", nil, "")
}
func (a *scriptedBatchPollingAdaptor) ParseBatchResult([]*model.Task, *http.Response, []byte) (map[string]*BatchTaskResult, error) {
	if a.parseErr != nil {
		return nil, a.parseErr
	}
	return a.results, nil
}

func TestUpdateVideoSingleTaskPollClassification(t *testing.T) {
	testCases := []struct {
		name          string
		statusCode    int
		fetchErr      error
		parse         *relaycommon.TaskInfo
		parseErr      error
		priorFailures int
		priorState    string
		maxFailures   int
		wantStatus    model.TaskStatus
		wantFailures  int
		wantRefund    bool
		wantReason    string
		wantState     string
		wantUnchanged bool
	}{
		{
			name:          "404 fails immediately and refunds",
			statusCode:    http.StatusNotFound,
			wantStatus:    model.TaskStatusFailure,
			wantRefund:    true,
			wantReason:    "Upstream task not found",
			wantUnchanged: false,
		},
		{
			name:          "401 increments without changing status",
			statusCode:    http.StatusUnauthorized,
			wantStatus:    model.TaskStatusInProgress,
			wantFailures:  1,
			wantUnchanged: true,
		},
		{
			name:          "429 reaches threshold and refunds",
			statusCode:    http.StatusTooManyRequests,
			priorFailures: 2,
			maxFailures:   3,
			wantStatus:    model.TaskStatusFailure,
			wantFailures:  3,
			wantRefund:    true,
			wantReason:    "Failed to query the upstream task status",
		},
		{
			name:          "UNKNOWN increments",
			statusCode:    http.StatusOK,
			parse:         &relaycommon.TaskInfo{Status: model.TaskStatusUnknown, Reason: "weird"},
			wantStatus:    model.TaskStatusInProgress,
			wantFailures:  1,
			wantUnchanged: true,
		},
		{
			name:          "valid 2xx resets the failure counter",
			statusCode:    http.StatusOK,
			parse:         &relaycommon.TaskInfo{Status: model.TaskStatusInProgress},
			priorFailures: 5,
			wantStatus:    model.TaskStatusInProgress,
			wantFailures:  0,
		},
		{
			name:       "omit state preserves previous plugin state",
			statusCode: http.StatusOK,
			parse:      &relaycommon.TaskInfo{Status: model.TaskStatusInProgress},
			priorState: `{"req_key":"keep"}`,
			wantStatus: model.TaskStatusInProgress,
			wantState:  `{"req_key":"keep"}`,
		},
		{
			name:       "returned state replaces plugin state",
			statusCode: http.StatusOK,
			parse: &relaycommon.TaskInfo{
				Status:      model.TaskStatusInProgress,
				PluginState: []byte(`{"req_key":"new"}`),
			},
			priorState: `{"req_key":"old"}`,
			wantStatus: model.TaskStatusInProgress,
			wantState:  `{"req_key":"new"}`,
		},
		{
			name:          "other 4xx non-terminal is unrecognized",
			statusCode:    http.StatusBadRequest,
			parse:         &relaycommon.TaskInfo{Status: model.TaskStatusInProgress},
			wantStatus:    model.TaskStatusInProgress,
			wantFailures:  1,
			wantUnchanged: true,
		},
	}

	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			truncate(t)
			const userID, tokenID, channelID = 510, 510, 510
			const initialQuota, preConsumed, tokenRemain = 10_000, 4_000, 7_000
			seedUser(t, userID, initialQuota)
			seedToken(t, tokenID, userID, "sk-poll-class", tokenRemain)
			ch := &model.Channel{Id: channelID, Type: constant.ChannelTypeKling, Name: "poll", Key: "sk-test", Status: common.ChannelStatusEnabled}

			if testCase.maxFailures > 0 {
				previous := constant.TaskPollMaxFailures
				constant.TaskPollMaxFailures = testCase.maxFailures
				t.Cleanup(func() { constant.TaskPollMaxFailures = previous })
			}

			task := makeTask(userID, channelID, preConsumed, tokenID, BillingSourceWallet, 0)
			task.TaskID = "task_poll_class"
			task.PrivateData.UpstreamTaskID = "upstream_poll_class"
			task.PrivateData.PollFailures = testCase.priorFailures
			if testCase.priorState != "" {
				task.PrivateData.PluginState = []byte(testCase.priorState)
			}
			require.NoError(t, model.DB.Create(task).Error)

			adaptor := &scriptedPollingAdaptor{statusCode: testCase.statusCode, fetchErr: testCase.fetchErr, parse: testCase.parse, parseErr: testCase.parseErr}
			require.NoError(t, updateVideoSingleTask(context.Background(), adaptor, ch, task.GetUpstreamTaskID(), map[string]*model.Task{
				task.GetUpstreamTaskID(): task,
			}))

			var persisted model.Task
			require.NoError(t, model.DB.First(&persisted, task.ID).Error)
			assert.EqualValues(t, testCase.wantStatus, persisted.Status)
			assert.Equal(t, testCase.wantFailures, persisted.PrivateData.PollFailures)
			if testCase.wantUnchanged {
				assert.Empty(t, persisted.FailReason)
			}
			if testCase.wantReason != "" {
				assert.Equal(t, testCase.wantReason, persisted.FailReason)
			}
			if testCase.wantState != "" {
				assert.JSONEq(t, testCase.wantState, string(persisted.PrivateData.PluginState))
			}
			if testCase.wantRefund {
				assert.Equal(t, initialQuota+preConsumed, getUserQuota(t, userID))
				assert.Equal(t, tokenRemain+preConsumed, getTokenRemainQuota(t, tokenID))
				assert.Zero(t, persisted.Quota)
				log := getLastLog(t)
				require.NotNil(t, log)
				assert.Equal(t, model.LogTypeRefund, log.Type)
			} else {
				assert.Equal(t, initialQuota, getUserQuota(t, userID))
				assert.Equal(t, tokenRemain, getTokenRemainQuota(t, tokenID))
			}
		})
	}
}

func TestUpdateBatchTasksPollClassification(t *testing.T) {
	testCases := []struct {
		name         string
		statusCode   int
		resultStatus model.TaskStatus
		wantStatus   model.TaskStatus
		wantFailures int
		wantRefund   bool
		wantReason   string
	}{
		{
			name:       "404 fails the batch and refunds",
			statusCode: http.StatusNotFound,
			wantStatus: model.TaskStatusFailure,
			wantRefund: true,
			wantReason: "Upstream task not found",
		},
		{
			name:         "401 increments every task",
			statusCode:   http.StatusUnauthorized,
			wantStatus:   model.TaskStatusInProgress,
			wantFailures: 1,
		},
		{
			name:         "UNKNOWN increments",
			statusCode:   http.StatusOK,
			resultStatus: model.TaskStatusUnknown,
			wantStatus:   model.TaskStatusInProgress,
			wantFailures: 1,
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			truncate(t)
			const userID, tokenID, channelID = 610, 610, 610
			const initialQuota, preConsumed, tokenRemain = 10_000, 4_000, 7_000
			seedUser(t, userID, initialQuota)
			seedToken(t, tokenID, userID, "sk-batch-class", tokenRemain)
			seedTaskPollingChannel(t, channelID, true)

			task := makeTask(userID, channelID, preConsumed, tokenID, BillingSourceWallet, 0)
			task.TaskID = "task_batch_class"
			task.PrivateData.UpstreamTaskID = "upstream_batch_class"
			require.NoError(t, model.DB.Create(task).Error)
			upstreamID := task.GetUpstreamTaskID()

			adaptor := &scriptedBatchPollingAdaptor{
				scriptedPollingAdaptor: scriptedPollingAdaptor{statusCode: testCase.statusCode},
			}
			if testCase.resultStatus != "" {
				adaptor.results = map[string]*BatchTaskResult{
					upstreamID: {TaskInfo: relaycommon.TaskInfo{TaskID: upstreamID, Status: string(testCase.resultStatus), Reason: "weird"}},
				}
			}

			require.NoError(t, UpdateBatchTasks(context.Background(), adaptor, map[int][]string{channelID: {upstreamID}}, map[string]*model.Task{upstreamID: task}))

			var persisted model.Task
			require.NoError(t, model.DB.First(&persisted, task.ID).Error)
			assert.EqualValues(t, testCase.wantStatus, persisted.Status)
			assert.Equal(t, testCase.wantFailures, persisted.PrivateData.PollFailures)
			if testCase.wantReason != "" {
				assert.Equal(t, testCase.wantReason, persisted.FailReason)
			}
			if testCase.wantRefund {
				assert.Equal(t, initialQuota+preConsumed, getUserQuota(t, userID))
				assert.Zero(t, persisted.Quota)
			} else {
				assert.Equal(t, initialQuota, getUserQuota(t, userID))
			}
		})
	}
}

func TestAsyncTaskChannelProcurementDatabaseMatrix(t *testing.T) {
	for _, dialect := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			var driver gorm.Dialector
			switch dialect {
			case "sqlite":
				driver = sqlite.Open(":memory:")
			case "mysql":
				if os.Getenv("TEST_MYSQL_DSN") == "" {
					t.Skip("TEST_MYSQL_DSN not configured")
				}
				driver = mysql.Open(os.Getenv("TEST_MYSQL_DSN"))
			case "postgres":
				if os.Getenv("TEST_POSTGRES_DSN") == "" {
					t.Skip("TEST_POSTGRES_DSN not configured")
				}
				driver = postgres.New(postgres.Config{DSN: os.Getenv("TEST_POSTGRES_DSN"), PreferSimpleProtocol: true})
			}
			db, err := gorm.Open(driver, &gorm.Config{NamingStrategy: schema.NamingStrategy{TablePrefix: "async_procurement_test_"}, Logger: logger.Default.LogMode(logger.Silent)})
			require.NoError(t, err)
			sqlDB, err := db.DB()
			require.NoError(t, err)
			sqlDB.SetMaxOpenConns(1)
			oldDB, oldType, oldCache, oldQuota := model.DB, common.MainDatabaseType(), common.MemoryCacheEnabled, common.QuotaPerUnit
			model.DB, common.MemoryCacheEnabled, common.QuotaPerUnit = db, false, 500000
			common.SetMainDatabaseType(common.DatabaseType(dialect))
			schemas := []any{&model.Channel{}, &model.SupplierAccount{}, &model.Task{}, &model.ChannelLimitConfig{}, &model.ChannelLimitAttempt{}, &model.ChannelDailyBudget{}, &model.ChannelLimitEvent{}, &model.OverviewAttemptEvent{}, &model.OverviewDaily{}, &model.OverviewCollectionState{}}
			t.Cleanup(func() {
				require.NoError(t, db.Migrator().DropTable(schemas...))
				model.DB, common.MemoryCacheEnabled, common.QuotaPerUnit = oldDB, oldCache, oldQuota
				common.SetMainDatabaseType(oldType)
				require.NoError(t, sqlDB.Close())
			})
			for range 2 {
				require.NoError(t, db.AutoMigrate(schemas...))
			}
			t.Setenv("SUPPLIER_CREDENTIAL_ACTIVE_KEY", "test")
			t.Setenv("SUPPLIER_CREDENTIAL_KEYS", `{"test":"`+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{7}, 32))+`"}`)
			baseURL := "https://api.example.com"
			costRatio := 1.0
			channel := &model.Channel{Type: constant.ChannelTypeOpenAI, Name: "frozen-task", Key: "secret-before-rotation", BaseURL: &baseURL, Models: "video-model", Status: common.ChannelStatusEnabled, SecretsWriteOnly: true, CostRatio: &costRatio}
			require.NoError(t, db.Create(channel).Error)
			pricingSnapshot, err := common.Marshal([]model.UpstreamCredentialSnapshot{{ChannelID: channel.Id, KeyIndex: 0, Active: true, EffectiveCostRatio: &costRatio, CostVersionID: "cost-v1", CredentialID: "original-credential", CredentialVersionID: "credential-v1"}})
			require.NoError(t, err)
			channel.UpstreamCredentialSnapshotData = string(pricingSnapshot)
			require.NoError(t, db.Model(channel).Update("upstream_credential_snapshot_data", channel.UpstreamCredentialSnapshotData).Error)
			require.NoError(t, model.EnsureChannelLimitTracking(channel.Id, true, time.Now()))
			state, err := model.GetChannelLimitState(channel.Id, time.Now())
			require.NoError(t, err)
			_, err = model.UpdateChannelLimits(channel.Id, state.Config.Revision, model.ChannelLimitSettings{RPM: 10, TPM: 20, DailyUSDNano: 10 * model.ChannelLimitUSDScale, ReconcileSeconds: 300}, true, time.Now())
			require.NoError(t, err)
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/videos", strings.NewReader(`{"model":"video-model","seconds":6}`))
			info := &relaycommon.RelayInfo{OriginModelName: "video-model", TaskRelayInfo: &relaycommon.TaskRelayInfo{Action: constant.TaskActionTextToVideo}, ChannelMeta: &relaycommon.ChannelMeta{ChannelId: channel.Id, ChannelType: channel.Type, ChannelBaseUrl: baseURL, ApiKey: channel.Key}, PriceData: hosttypes.PriceData{UsePrice: true, ModelPrice: 1}, TaskUsageUnits: map[string]string{"seconds": "second", "units": "credit"}}
			require.NoError(t, PrepareTaskChannelAttempt(info, channel))
			PrepareOverviewAttempt(c, info, channel)
			require.NoError(t, BeforeTaskChannelUpstream(c, info, c.Request))
			MarkOverviewUpstreamSent(info)
			c.Set("task_submit_http_status", http.StatusAccepted)
			FinishTaskChannelSubmission(c, info, true, nil, nil)
			require.NotNil(t, info.TaskChannelSnapshot)
			assert.True(t, info.TaskChannelSnapshot.Encrypted, "write-only protection survives platform ownership")
			assert.NotContains(t, info.TaskChannelSnapshot.Connection, "secret-before-rotation")
			task := model.InitTask(constant.TaskPlatform("video"), info)
			task.PrivateData.UpstreamTaskID = "vendor-task"
			task.Status = model.TaskStatusSubmitted
			task.Quota = 0
			require.NoError(t, task.Insert())
			require.NoError(t, FlushTaskChannelObservation(context.Background(), task))
			var event model.OverviewAttemptEvent
			require.NoError(t, db.First(&event, "id = ?", info.OverviewAttempt.EventID).Error)
			assert.True(t, event.Success, "accepted submission is recorded independently")
			assert.Equal(t, "accepted", event.TaskStatus)
			state, err = model.GetChannelLimitState(channel.Id, time.Now())
			require.NoError(t, err)
			assert.EqualValues(t, 1, state.RPMUsed)
			assert.Zero(t, state.TPMReserved, "media seconds never occupy TPM")
			assert.EqualValues(t, model.ChannelLimitUSDScale, state.DailyHeldNano)
			channel.Key = "new-key"
			channel.Status = common.ChannelStatusManuallyDisabled
			replacementURL := "https://new.example.com"
			channel.BaseURL = &replacementURL
			require.NoError(t, db.Save(channel).Error)
			frozen, err := ResolveTaskChannelConnection(task)
			require.NoError(t, err)
			assert.Equal(t, "secret-before-rotation", frozen.Key)
			assert.Equal(t, baseURL, frozen.GetBaseURL())
			oldStatus := task.Status
			task.Status = model.TaskStatusFailure
			ObserveTaskChannelResult(task, nil, false)
			won, err := task.UpdateWithStatus(oldStatus)
			require.NoError(t, err)
			require.True(t, won)
			require.NoError(t, FlushTaskChannelObservation(context.Background(), task))
			state, err = model.GetChannelLimitState(channel.Id, time.Now())
			require.NoError(t, err)
			assert.EqualValues(t, model.ChannelLimitUSDScale, state.DailyHeldNano, "local failure/refund cannot release upstream procurement")
			var stale model.Task
			require.NoError(t, db.First(&stale, task.ID).Error)
			result := &relaycommon.TaskInfo{Status: model.TaskStatusSuccess, UsageFacts: map[string]any{"seconds": 6.0, "units": 1.25}}
			ObserveTaskChannelResult(task, result, true)
			task.Status = model.TaskStatusSuccess
			won, err = task.UpdateWithStatus(model.TaskStatusFailure)
			require.NoError(t, err)
			require.True(t, won)
			require.NoError(t, FlushTaskChannelObservation(context.Background(), task))
			require.NoError(t, SettleTaskChannelSnapshot(context.Background(), task.PrivateData.ChannelAttempt, task.ProcurementVersion, time.Now()))
			state, err = model.GetChannelLimitState(channel.Id, time.Now())
			require.NoError(t, err)
			assert.EqualValues(t, model.ChannelLimitUSDScale, state.DailyUsedNano)
			assert.Zero(t, state.DailyHeldNano)
			ObserveTaskChannelResult(&stale, nil, false)
			stale.Status = model.TaskStatusFailure
			won, err = stale.UpdateWithStatus(model.TaskStatusFailure)
			require.NoError(t, err)
			assert.False(t, won, "stale poll cannot overwrite confirmed completion")
			require.NoError(t, db.First(&event, "id = ?", info.OverviewAttempt.EventID).Error)
			assert.Equal(t, "succeeded", event.TaskStatus)
			require.NotNil(t, event.VideoSeconds)
			assert.Equal(t, 6.0, *event.VideoSeconds)
			require.NotNil(t, event.Credits)
			assert.Equal(t, "1.25", *event.Credits)
			assert.EqualValues(t, 1, state.RPMUsed, "polling and terminal corrections never create another generation")
			adaptor := &batchPollingAdaptor{results: map[string]*BatchTaskResult{"vendor-task": {TaskInfo: relaycommon.TaskInfo{Status: model.TaskStatusSuccess, UsageFacts: map[string]any{"seconds": 9.0}}}}}
			version := task.ProcurementVersion
			require.NoError(t, pollCredentialBatch(context.Background(), adaptor, frozen, frozen.Key, []*model.Task{task}))
			assert.Greater(t, task.ProcurementVersion, version, "usage-only terminal correction persists a new outbox version")
			require.NoError(t, db.First(&event, "id = ?", info.OverviewAttempt.EventID).Error)
			require.NotNil(t, event.VideoSeconds)
			assert.Equal(t, 9.0, *event.VideoSeconds)
			adaptor.results["vendor-task"].TaskInfo.Status = model.TaskStatusInProgress
			require.NoError(t, pollCredentialBatch(context.Background(), adaptor, frozen, frozen.Key, []*model.Task{task}))
			require.NoError(t, failTaskFromPoll(context.Background(), adaptor, task, task.Status, "Upstream task not found"))
			assert.Equal(t, model.TaskStatus(model.TaskStatusSuccess), task.Status, "late stale status or query expiry cannot revoke terminal success")
		})
	}
}

func TestTaskProcurementEstimateAndUnknownCompletion(t *testing.T) {
	protected := &relaycommon.RelayInfo{ChannelLimitAttempt: &relaycommon.ChannelLimitRuntime{WriteOnlyCredentials: true}, ChannelMeta: &relaycommon.ChannelMeta{ApiKey: "secret"}}
	redacted := RedactTaskChannelPayload(protected, []byte(`{"echo":"\u0073ecret","exact":9007199254740993,"other":"unchanged"}`))
	assert.Equal(t, `{"echo":"[REDACTED]","exact":9007199254740993,"other":"unchanged"}`, string(redacted), "JSON string redaction preserves numbers exactly")
	failure := ProtectTaskChannelError(protected, &taskdto.TaskError{Code: "rejected", StatusCode: 429, NoRetry: true, Message: "secret", Error: errors.New("secret"), Data: map[string]any{"key": "secret"}})
	assert.Equal(t, "Upstream request failed", failure.Message)
	assert.Equal(t, "rejected", failure.Code)
	assert.Equal(t, 429, failure.StatusCode)
	assert.True(t, failure.NoRetry)
	assert.Nil(t, failure.Data)
	costRatio := 0.5
	info := &relaycommon.RelayInfo{ChannelLimitAttempt: &relaycommon.ChannelLimitRuntime{CostRatio: &costRatio}, TaskRelayInfo: &relaycommon.TaskRelayInfo{}, TieredBillingSnapshot: &billingexpr.BillingSnapshot{TaskUsageBilling: true, EstimatedQuotaBeforeGroup: 2.4 * 500000, EstimatedQuotaAfterGroup: 0, GroupRatio: 0, QuotaPerUnit: 500000}}
	budget, err := channelLimitProcurementBudget(info, nil, nil)
	require.NoError(t, err)
	assert.EqualValues(t, 1200000000, budget, "free customer group must retain procurement estimate")
	snapshot := &relaycommon.TaskChannelSnapshot{CostRatio: &costRatio, QuotaPerUnit: 500000, ModelPrice: 2.4, UsePrice: true}
	ObserveTaskChannelSnapshot(snapshot, &relaycommon.TaskInfo{Status: model.TaskStatusSuccess, Unconfirmed: true}, true)
	assert.Equal(t, "unknown", snapshot.Observation.Status)
	assert.Nil(t, snapshot.Observation.CostUSDNano)
	ObserveTaskChannelSnapshot(snapshot, &relaycommon.TaskInfo{Status: model.TaskStatusInProgress, Progress: "10%"}, true)
	first := snapshot.Observation.ObservedAt
	ObserveTaskChannelSnapshot(snapshot, nil, false)
	ObserveTaskChannelSnapshot(snapshot, &relaycommon.TaskInfo{Status: model.TaskStatusInProgress, Progress: "10%"}, true)
	assert.Equal(t, "unknown", snapshot.Observation.Status, "repeated old progress must not restart reconciliation")
	ObserveTaskChannelSnapshot(snapshot, &relaycommon.TaskInfo{Status: model.TaskStatusInProgress, Progress: "20%"}, true)
	assert.Equal(t, "running", snapshot.Observation.Status)
	assert.GreaterOrEqual(t, snapshot.Observation.ObservedAt, first)
	text := &relaycommon.TaskChannelSnapshot{TPMApplicable: true, UsageUnits: map[string]string{"input_tokens": "credit", "prompt_tokens": "token", "output_tokens": "token"}}
	ObserveTaskChannelSnapshot(text, &relaycommon.TaskInfo{Status: model.TaskStatusSuccess, UsageFacts: map[string]any{"input_tokens": 1000, "prompt_tokens": 2, "output_tokens": 3}}, true)
	require.NotNil(t, text.Observation.Tokens)
	assert.EqualValues(t, 5, *text.Observation.Tokens, "named aliases still require declared token units")
	for _, expression := range []string{`tier("flat", 3)`, `tier("duration", u("seconds") * 3)`} {
		t.Run(expression, func(t *testing.T) {
			price := &billingexpr.BillingSnapshot{TaskUsageBilling: true, ExprString: expression, ExprHash: billingexpr.ExprHashString(expression), QuotaPerUnit: 500000, GroupRatio: 0, UsageFacts: map[string]any{"seconds": 10.0}}
			snapshot := &relaycommon.TaskChannelSnapshot{Expression: price, CostRatio: &costRatio, QuotaPerUnit: 500000}
			ObserveTaskChannelSnapshot(snapshot, &relaycommon.TaskInfo{Status: model.TaskStatusSuccess}, true)
			ObserveTaskChannelSnapshot(snapshot, &relaycommon.TaskInfo{Status: model.TaskStatusInProgress}, true)
			assert.Equal(t, "succeeded", snapshot.Observation.Status, "late stale running status must not revoke terminal evidence")
			// Without completion facts the customer settlement evaluates the frozen
			// expression on the submit facts; procurement records that same amount.
			want := int64(1500000000)
			if strings.Contains(expression, `u(`) {
				want = 15000000000
			}
			require.NotNil(t, snapshot.Observation.CostUSDNano)
			assert.EqualValues(t, want, *snapshot.Observation.CostUSDNano)
			assert.False(t, snapshot.NeedsReconciliation())
		})
	}
	for _, tc := range []struct {
		name, path, body string
		output           *int64
	}{
		{"responses final output", "/v1/responses", `{"model":"gpt-4o-mini","input":"Hello","max_output_tokens":5}`, common.GetPointer(int64(5))},
		{"explicit zero", "/v1/responses", `{"model":"gpt-4o-mini","input":"Hello","max_output_tokens":0}`, common.GetPointer(int64(0))},
		{"chat final output", "/v1/chat/completions", `{"model":"gpt-4o-mini","messages":[{"role":"user","content":"Hello"}],"max_completion_tokens":9}`, common.GetPointer(int64(9))},
		{"unseen conversation", "/v1/responses", `{"model":"gpt-4o-mini","input":"Hello","previous_response_id":"old-response","max_output_tokens":5}`, nil},
		{"native lookalike", "/native/submit", `{"model":"gpt-4o-mini","input":"Hello","max_output_tokens":5}`, nil},
		{"missing output bound", "/v1/responses", `{"model":"gpt-4o-mini","input":"Hello"}`, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/client/submit", strings.NewReader(`{"max_tokens":1}`))
			request := httptest.NewRequest(http.MethodPost, "https://provider.example"+tc.path, strings.NewReader(tc.body))
			request.Header.Set("Content-Type", "application/json")
			info := &relaycommon.RelayInfo{OriginModelName: "gpt-4o-mini", ChannelMeta: &relaycommon.ChannelMeta{}, TaskRelayInfo: &relaycommon.TaskRelayInfo{}, ChannelLimitAttempt: &relaycommon.ChannelLimitRuntime{TPMApplicable: true}}
			input, output, total, err := taskChannelTokenBudget(c, info, request)
			require.NoError(t, err)
			if tc.output == nil {
				assert.Nil(t, total)
			} else {
				require.NotNil(t, input)
				require.NotNil(t, output)
				require.NotNil(t, total)
				assert.Positive(t, *input)
				assert.Equal(t, *tc.output, *output)
				assert.Equal(t, *input+*tc.output, *total)
			}
			remaining, err := io.ReadAll(request.Body)
			require.NoError(t, err)
			assert.Equal(t, tc.body, string(remaining), "counting must preserve the actual upstream request")
		})
	}
	for _, tc := range []struct {
		name, media, key string
		video, audio     bool
	}{
		{"untyped duration", "", "seconds", false, false},
		{"video duration", "video", "seconds", true, false},
		{"audio duration", "audio", "seconds", false, true},
		{"explicit audio", "", "audio_seconds", false, true},
		{"explicit video", "", "video_seconds", true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot := &relaycommon.TaskChannelSnapshot{MediaKind: tc.media, UsageUnits: map[string]string{tc.key: "second"}}
			ObserveTaskChannelSnapshot(snapshot, &relaycommon.TaskInfo{Status: model.TaskStatusSuccess, UsageFacts: map[string]any{tc.key: 6.0}}, true)
			assert.Equal(t, tc.video, snapshot.Observation.VideoSeconds != nil)
			assert.Equal(t, tc.audio, snapshot.Observation.AudioSeconds != nil)
		})
	}
}

// A task reports once, when it reaches its final state: not per poll, not
// when another poller lost the race, never when no instance may report it.
func TestTaskReportsOnceAtTheFinalState(t *testing.T) {
	truncate(t)
	const userID, tokenID, channelID = 520, 520, 520
	seedUser(t, userID, 10_000)
	seedToken(t, tokenID, userID, "sk-poll-report", 7_000)
	ch := &model.Channel{Id: channelID, Type: constant.ChannelTypeKling, Name: "poll", Key: "sk-test", Status: common.ChannelStatusEnabled}

	var reported []string
	previous := ReportTaskFunc
	ReportTaskFunc = func(task *model.Task) { reported = append(reported, task.TaskID+":"+string(task.Status)) }
	t.Cleanup(func() { ReportTaskFunc = previous })

	newTask := func(id string, plugins bool) *model.Task {
		task := makeTask(userID, channelID, 1_000, tokenID, BillingSourceWallet, 0)
		task.TaskID = id
		task.PrivateData.UpstreamTaskID = "upstream_" + id
		if plugins {
			task.PrivateData.RequestPlugins = &model.TaskRequestPlugins{Instances: []string{"audit"}}
		}
		require.NoError(t, model.DB.Create(task).Error)
		return task
	}
	reload := func(task *model.Task) *model.Task {
		var stored model.Task
		require.NoError(t, model.DB.First(&stored, task.ID).Error)
		return &stored
	}
	poll := func(task *model.Task, status model.TaskStatus) {
		adaptor := &scriptedPollingAdaptor{parse: &relaycommon.TaskInfo{Status: string(status), Url: "https://cdn.example.com/v.mp4"}}
		require.NoError(t, updateVideoSingleTask(context.Background(), adaptor, ch, task.GetUpstreamTaskID(), map[string]*model.Task{
			task.GetUpstreamTaskID(): task,
		}))
	}

	task := newTask("report_once", true)
	stale := reload(task)
	poll(task, model.TaskStatusInProgress)
	poll(reload(task), model.TaskStatusInProgress)
	assert.Empty(t, reported)

	poll(reload(task), model.TaskStatusSuccess)
	assert.Equal(t, []string{"report_once:SUCCESS"}, reported)
	// Another poller that read the row before it finished loses the update.
	poll(stale, model.TaskStatusSuccess)
	assert.Len(t, reported, 1)

	plain := newTask("no_plugins", false)
	poll(plain, model.TaskStatusSuccess)
	assert.Len(t, reported, 1)

	failed := newTask("fails", true)
	poll(failed, model.TaskStatusFailure)
	assert.Equal(t, "fails:FAILURE", reported[1])

	previousTimeout := constant.TaskTimeoutMinutes
	constant.TaskTimeoutMinutes = 1
	t.Cleanup(func() { constant.TaskTimeoutMinutes = previousTimeout })
	timedOut := newTask("times_out", true)
	require.NoError(t, model.DB.Model(&model.Task{}).Where("id = ?", timedOut.ID).Update("submit_time", time.Now().Add(-time.Hour).Unix()).Error)
	sweepTimedOutTasks(context.Background())
	sweepTimedOutTasks(context.Background())
	assert.Equal(t, []string{"report_once:SUCCESS", "fails:FAILURE", "times_out:FAILURE"}, reported)
}

func TestTaskCredentialLookupFailuresRespectRefundThreshold(t *testing.T) {
	truncate(t)
	const userID, tokenID, channelID = 521, 521, 521
	seedUser(t, userID, 10_000)
	seedToken(t, tokenID, userID, "sk-credential-poll", 7_000)
	previous := constant.TaskPollMaxFailures
	constant.TaskPollMaxFailures = 2
	t.Cleanup(func() { constant.TaskPollMaxFailures = previous })
	task := makeTask(userID, channelID, 1_000, tokenID, BillingSourceWallet, 0)
	task.TaskID = "credential_lookup_failure"
	task.PrivateData.UpstreamTaskID = "upstream-credential"
	require.NoError(t, model.DB.Create(task).Error)
	channel := &model.Channel{Id: channelID, Type: constant.ChannelTypeTaskPlugin, Key: "old", Status: common.ChannelStatusEnabled}
	adaptor := &scriptedPollingAdaptor{fetchErr: errors.New("task credential connection changed")}
	poll := func(task *model.Task) {
		require.NoError(t, updateVideoSingleTask(t.Context(), adaptor, channel, task.GetUpstreamTaskID(), map[string]*model.Task{task.GetUpstreamTaskID(): task}))
	}
	poll(task)
	assert.NotEqualValues(t, model.TaskStatusFailure, task.Status)
	assert.Equal(t, 1, task.PrivateData.PollFailures)
	assert.Equal(t, 10_000, getUserQuota(t, userID), "a credential lookup failure is not an upstream terminal failure")
	var stale model.Task
	require.NoError(t, model.DB.First(&stale, task.ID).Error)
	poll(task)
	assert.EqualValues(t, model.TaskStatusFailure, task.Status)
	assert.Equal(t, 11_000, getUserQuota(t, userID))
	assert.Equal(t, 8_000, getTokenRemainQuota(t, tokenID))
	poll(&stale)
	assert.Equal(t, 11_000, getUserQuota(t, userID), "a stale poll cannot refund twice")
	assert.EqualValues(t, 1, countLogs(t))
}

// A refunded FAILURE whose vendor finishes later is charged back exactly what
// the refund returned and then settles as a normal SUCCESS would. Stale
// pollers cannot charge twice and a later FAILURE cannot refund the result.
// The claims are conditional updates, so this runs on every database.
func TestRefundedFailureLateSuccessIsChargedOnceDatabaseMatrix(t *testing.T) {
	for _, dialect := range []string{"sqlite", "mysql", "postgres"} {
		for _, source := range []string{BillingSourceWallet, BillingSourceSubscription} {
			t.Run(dialect+"/"+source, func(t *testing.T) {
				var driver gorm.Dialector
				switch dialect {
				case "sqlite":
					driver = sqlite.Open(":memory:")
				case "mysql":
					if os.Getenv("TEST_MYSQL_DSN") == "" {
						t.Skip("TEST_MYSQL_DSN not configured")
					}
					driver = mysql.Open(os.Getenv("TEST_MYSQL_DSN"))
				case "postgres":
					if os.Getenv("TEST_POSTGRES_DSN") == "" {
						t.Skip("TEST_POSTGRES_DSN not configured")
					}
					driver = postgres.New(postgres.Config{DSN: os.Getenv("TEST_POSTGRES_DSN"), PreferSimpleProtocol: true})
				}
				db, err := gorm.Open(driver, &gorm.Config{NamingStrategy: schema.NamingStrategy{TablePrefix: "task_recharge_test_"}, Logger: logger.Default.LogMode(logger.Silent)})
				require.NoError(t, err)
				sqlDB, err := db.DB()
				require.NoError(t, err)
				sqlDB.SetMaxOpenConns(1)
				oldDB, oldLog, oldMain, oldLogType := model.DB, model.LOG_DB, common.MainDatabaseType(), common.LogDatabaseType()
				model.DB, model.LOG_DB = db, db
				common.SetDatabaseTypes(common.DatabaseType(dialect), common.DatabaseType(dialect))
				schemas := []any{&model.Task{}, &model.User{}, &model.Token{}, &model.Log{}, &model.Channel{}, &model.UserSubscription{}}
				t.Cleanup(func() {
					require.NoError(t, db.Migrator().DropTable(schemas...))
					model.DB, model.LOG_DB = oldDB, oldLog
					common.SetDatabaseTypes(oldMain, oldLogType)
					require.NoError(t, sqlDB.Close())
				})
				for range 2 {
					require.NoError(t, db.AutoMigrate(schemas...))
				}

				const userID, tokenID, channelID, subID = 701, 701, 701, 701
				const walletQuota, tokenRemain, preConsumed, actual = 10_000, 7_000, 3_000, 2_000
				const subTotal, subUsed int64 = 100_000, 50_000
				seedUser(t, userID, walletQuota)
				seedToken(t, tokenID, userID, "sk-late-success", tokenRemain)
				seedChannel(t, channelID)
				seedChargedAccounting(t, userID, channelID, tokenID, preConsumed, 1)
				subscriptionID := 0
				if source == BillingSourceSubscription {
					seedSubscription(t, subID, userID, subTotal, subUsed)
					subscriptionID = subID
				}
				// funded reports the funding source balance relative to its
				// state after the submission charge.
				funded := func() int {
					if source == BillingSourceSubscription {
						return int(subUsed - getSubscriptionUsed(t, subID))
					}
					return getUserQuota(t, userID) - walletQuota
				}
				task := makeTask(userID, channelID, preConsumed, tokenID, source, subscriptionID)
				task.TaskID = "late_success_" + source
				task.PrivateData.UpstreamTaskID = "upstream_late_success"
				require.NoError(t, model.DB.Create(task).Error)
				ch := &model.Channel{Id: channelID, Type: constant.ChannelTypeKling, Key: "sk-test", Status: common.ChannelStatusEnabled}
				reload := func() *model.Task {
					var stored model.Task
					require.NoError(t, model.DB.First(&stored, task.ID).Error)
					return &stored
				}
				poll := func(task *model.Task, adaptor *scriptedPollingAdaptor) {
					require.NoError(t, updateVideoSingleTask(context.Background(), adaptor, ch, task.GetUpstreamTaskID(), map[string]*model.Task{task.GetUpstreamTaskID(): task}))
				}

				poll(task, &scriptedPollingAdaptor{statusCode: http.StatusNotFound})
				stored := reload()
				require.EqualValues(t, model.TaskStatusFailure, stored.Status)
				assert.Zero(t, stored.Quota)
				assert.Equal(t, preConsumed, stored.RefundedQuota)
				assert.Equal(t, preConsumed, funded())

				first, second := reload(), reload()
				success := &scriptedPollingAdaptor{parse: &relaycommon.TaskInfo{Status: model.TaskStatusSuccess}, adjust: actual}
				poll(first, success)
				poll(second, success)

				stored = reload()
				assert.EqualValues(t, model.TaskStatusSuccess, stored.Status)
				assert.Equal(t, actual, stored.Quota, "the late success settles from its result exactly as a normal success")
				assert.Zero(t, stored.RefundedQuota)
				assert.Equal(t, preConsumed-actual, funded(), "charged back once, then settled to the actual cost")
				assert.Equal(t, tokenRemain+preConsumed-actual, getTokenRemainQuota(t, tokenID))
				usedQuota, requestCount := getUserUsageAccounting(t, userID)
				assert.Equal(t, actual, usedQuota)
				assert.Equal(t, 1, requestCount)
				assert.Equal(t, int64(actual), getChannelUsedQuota(t, channelID))
				var logs []model.Log
				require.NoError(t, model.LOG_DB.Order("id").Find(&logs).Error)
				require.Len(t, logs, 3)
				assert.Equal(t, []int{model.LogTypeRefund, model.LogTypeConsume, model.LogTypeRefund}, []int{logs[0].Type, logs[1].Type, logs[2].Type})
				assert.Equal(t, []int{preConsumed, preConsumed, preConsumed - actual}, []int{logs[0].Quota, logs[1].Quota, logs[2].Quota})

				poll(reload(), &scriptedPollingAdaptor{parse: &relaycommon.TaskInfo{Status: model.TaskStatusFailure, Reason: "late failure"}})
				stored = reload()
				assert.EqualValues(t, model.TaskStatusSuccess, stored.Status, "a delivered result is never revoked")
				assert.Equal(t, actual, stored.Quota)
				assert.Equal(t, preConsumed-actual, funded())
				assert.EqualValues(t, 3, countLogs(t))
			})
		}
	}
}

func TestTaskFailureLateSuccessBilling(t *testing.T) {
	const userID, tokenID, channelID = 711, 711, 711
	const walletQuota, tokenRemain, preConsumed, actual = 10_000, 7_000, 3_000, 2_000
	ch := &model.Channel{Id: channelID, Type: constant.ChannelTypeKling, Key: "sk-test", Status: common.ChannelStatusEnabled}
	setup := func(t *testing.T, quota int) *model.Task {
		truncate(t)
		seedUser(t, userID, walletQuota)
		seedToken(t, tokenID, userID, "sk-late-billing", tokenRemain)
		seedChannel(t, channelID)
		seedChargedAccounting(t, userID, channelID, tokenID, preConsumed, 1)
		task := makeTask(userID, channelID, quota, tokenID, BillingSourceWallet, 0)
		task.TaskID = "late_billing"
		task.Status = model.TaskStatusFailure
		task.Progress = "100%"
		task.PrivateData.UpstreamTaskID = "upstream_late_billing"
		require.NoError(t, model.DB.Create(task).Error)
		return task
	}
	reload := func(t *testing.T, task *model.Task) *model.Task {
		var stored model.Task
		require.NoError(t, model.DB.First(&stored, task.ID).Error)
		return &stored
	}
	success := &scriptedPollingAdaptor{parse: &relaycommon.TaskInfo{Status: model.TaskStatusSuccess}, adjust: actual}

	t.Run("an unrefunded failure settles normally", func(t *testing.T) {
		// The refund of this FAILURE failed, so it still charges preConsumed.
		task := setup(t, preConsumed)
		require.NoError(t, updateVideoSingleTask(context.Background(), success, ch, task.GetUpstreamTaskID(), map[string]*model.Task{task.GetUpstreamTaskID(): task}))
		stored := reload(t, task)
		assert.EqualValues(t, model.TaskStatusSuccess, stored.Status)
		assert.Equal(t, actual, stored.Quota)
		assert.Equal(t, walletQuota+preConsumed-actual, getUserQuota(t, userID))
		log := getLastLog(t)
		require.NotNil(t, log)
		assert.Equal(t, model.LogTypeRefund, log.Type)
		assert.Equal(t, preConsumed-actual, log.Quota)
		assert.EqualValues(t, 1, countLogs(t))
	})

	t.Run("a refund racing a late success is withdrawn", func(t *testing.T) {
		task := setup(t, preConsumed)
		refunder := reload(t, task)
		require.NoError(t, updateVideoSingleTask(context.Background(), success, ch, task.GetUpstreamTaskID(), map[string]*model.Task{task.GetUpstreamTaskID(): task}))
		assert.False(t, RefundTaskQuota(context.Background(), refunder, "Task failed"))
		stored := reload(t, task)
		assert.EqualValues(t, model.TaskStatusSuccess, stored.Status)
		assert.Equal(t, actual, stored.Quota)
		assert.Zero(t, stored.RefundedQuota)
		assert.Equal(t, walletQuota+preConsumed-actual, getUserQuota(t, userID))
		assert.Equal(t, tokenRemain+preConsumed-actual, getTokenRemainQuota(t, tokenID))
		assert.EqualValues(t, 1, countLogs(t))
	})

	t.Run("a refunded failure never resumes", func(t *testing.T) {
		task := setup(t, preConsumed)
		require.True(t, RefundTaskQuota(context.Background(), task, "Task failed"))
		running := &scriptedPollingAdaptor{parse: &relaycommon.TaskInfo{Status: model.TaskStatusInProgress}}
		require.NoError(t, updateVideoSingleTask(context.Background(), running, ch, task.GetUpstreamTaskID(), map[string]*model.Task{task.GetUpstreamTaskID(): reload(t, task)}))
		stored := reload(t, task)
		assert.EqualValues(t, model.TaskStatusFailure, stored.Status)
		assert.Equal(t, preConsumed, stored.RefundedQuota)
		assert.Equal(t, walletQuota+preConsumed, getUserQuota(t, userID))
	})
}

// A client fetch triggers a poll of its own task. Its upstream errors are not
// the background poller's and neither count nor fail the running task.
func TestClientTriggeredPollErrorsDoNotCountAsPollFailures(t *testing.T) {
	const userID, channelID, preConsumed = 721, 721, 3_000
	testCases := []struct {
		name    string
		adaptor scriptedPollingAdaptor
		batch   bool
	}{
		{name: "transport error", adaptor: scriptedPollingAdaptor{fetchErr: errors.New("connection reset")}},
		{name: "429", adaptor: scriptedPollingAdaptor{statusCode: http.StatusTooManyRequests}},
		{name: "5xx", adaptor: scriptedPollingAdaptor{statusCode: http.StatusBadGateway}},
		{name: "404", adaptor: scriptedPollingAdaptor{statusCode: http.StatusNotFound}},
		{name: "unrecognized", adaptor: scriptedPollingAdaptor{parse: &relaycommon.TaskInfo{Status: model.TaskStatusUnknown}}},
		{name: "batch 429", adaptor: scriptedPollingAdaptor{statusCode: http.StatusTooManyRequests}, batch: true},
		{name: "batch transport error", adaptor: scriptedPollingAdaptor{fetchErr: errors.New("connection reset")}, batch: true},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			truncate(t)
			seedUser(t, userID, 10_000)
			seedTaskPollingChannel(t, channelID, true)
			previous := constant.TaskPollMaxFailures
			constant.TaskPollMaxFailures = 1
			t.Cleanup(func() { constant.TaskPollMaxFailures = previous })
			task := makeTask(userID, channelID, preConsumed, 0, BillingSourceWallet, 0)
			task.TaskID = "client_poll"
			task.PrivateData.UpstreamTaskID = "upstream_client_poll"
			require.NoError(t, model.DB.Create(task).Error)
			ctx := WithClientTaskPoll(context.Background())
			ch := &model.Channel{Id: channelID, Type: constant.ChannelTypeKling, Key: "sk-test", Status: common.ChannelStatusEnabled}
			adaptor := testCase.adaptor
			if testCase.batch {
				require.NoError(t, pollCredentialBatch(ctx, &scriptedBatchPollingAdaptor{scriptedPollingAdaptor: adaptor}, ch, ch.Key, []*model.Task{task}))
			} else {
				require.NoError(t, updateVideoSingleTask(ctx, &adaptor, ch, task.GetUpstreamTaskID(), map[string]*model.Task{task.GetUpstreamTaskID(): task}))
			}
			var stored model.Task
			require.NoError(t, model.DB.First(&stored, task.ID).Error)
			assert.EqualValues(t, model.TaskStatusInProgress, stored.Status)
			assert.Zero(t, stored.PrivateData.PollFailures)
			assert.Equal(t, preConsumed, stored.Quota)
			assert.Equal(t, 10_000, getUserQuota(t, userID))
			assert.EqualValues(t, 0, countLogs(t))
		})
	}
}

// A legacy task without a saved key polls with its channel's current key. A
// sealed supplier secret is opened first; one this node cannot open yields no
// key, so the sealed value is never sent upstream.
func TestTaskPollingKeyOpensSealedSupplierSecret(t *testing.T) {
	t.Setenv("SUPPLIER_CREDENTIAL_KEYS", `{"v1":"`+base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{9}, 32))+`"}`)
	t.Setenv("SUPPLIER_CREDENTIAL_ACTIVE_KEY", "v1")
	sealed, err := model.EncryptSupplierSecret("supplier-1", "vendor-secret")
	require.NoError(t, err)
	require.NotEqual(t, "vendor-secret", sealed)

	assert.Equal(t, "plain-key", TaskPollingKey(&model.Channel{Key: "plain-key"}, "", nil))
	assert.Equal(t, "saved-key", TaskPollingKey(&model.Channel{Key: sealed, OwnerSupplierID: "supplier-1"}, "saved-key", nil))
	assert.Equal(t, "vendor-secret", TaskPollingKey(&model.Channel{Key: sealed, OwnerSupplierID: "supplier-1", SecretsWriteOnly: true}, "", nil))
	assert.Empty(t, TaskPollingKey(&model.Channel{Key: sealed, OwnerSupplierID: "another-supplier", SecretsWriteOnly: true}, "", nil))
}

// Refund bookkeeping that still owes the funding source a charge is retried by
// the pollers without polling the vendor, and a settled quota survives a stale
// task copy. The claims and markers are conditional updates, so this runs on
// every database.
func TestPendingRefundBillingDatabaseMatrix(t *testing.T) {
	for _, dialect := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(dialect, func(t *testing.T) {
			var driver gorm.Dialector
			switch dialect {
			case "sqlite":
				driver = sqlite.Open(":memory:")
			case "mysql":
				if os.Getenv("TEST_MYSQL_DSN") == "" {
					t.Skip("TEST_MYSQL_DSN not configured")
				}
				driver = mysql.Open(os.Getenv("TEST_MYSQL_DSN"))
			case "postgres":
				if os.Getenv("TEST_POSTGRES_DSN") == "" {
					t.Skip("TEST_POSTGRES_DSN not configured")
				}
				driver = postgres.New(postgres.Config{DSN: os.Getenv("TEST_POSTGRES_DSN"), PreferSimpleProtocol: true})
			}
			db, err := gorm.Open(driver, &gorm.Config{NamingStrategy: schema.NamingStrategy{TablePrefix: "refund_repair_test_"}, Logger: logger.Default.LogMode(logger.Silent)})
			require.NoError(t, err)
			sqlDB, err := db.DB()
			require.NoError(t, err)
			sqlDB.SetMaxOpenConns(1)
			oldDB, oldLog, oldMain, oldLogType := model.DB, model.LOG_DB, common.MainDatabaseType(), common.LogDatabaseType()
			model.DB, model.LOG_DB = db, db
			common.SetDatabaseTypes(common.DatabaseType(dialect), common.DatabaseType(dialect))
			schemas := []any{&model.Task{}, &model.Midjourney{}, &model.User{}, &model.Token{}, &model.Log{}, &model.Channel{}, &model.UserSubscription{}}
			t.Cleanup(func() {
				require.NoError(t, db.Migrator().DropTable(schemas...))
				model.DB, model.LOG_DB = oldDB, oldLog
				common.SetDatabaseTypes(oldMain, oldLogType)
				require.NoError(t, sqlDB.Close())
			})
			for range 2 {
				require.NoError(t, db.AutoMigrate(schemas...))
			}
			// allowWalletWrites lets that many wallet writes through and fails
			// the rest; a negative value lets every write through.
			allowWalletWrites := -1
			require.NoError(t, db.Callback().Update().Before("gorm:update").Register("test:wallet_outage", func(tx *gorm.DB) {
				if allowWalletWrites < 0 || !strings.HasSuffix(tx.Statement.Table, "users") {
					return
				}
				if allowWalletWrites == 0 {
					_ = tx.AddError(errors.New("wallet unavailable"))
					return
				}
				allowWalletWrites--
			}))
			ctx := context.Background()
			seedWallet := func(t *testing.T, id, quota int) {
				name := "refund_repair_" + strconv.Itoa(id)
				user := &model.User{Id: id, Username: name, AffCode: name, Quota: quota, Status: common.UserStatusEnabled}
				require.NoError(t, db.Create(user).Error)
			}
			reloadTask := func(id int64) *model.Task {
				var stored model.Task
				require.NoError(t, db.First(&stored, id).Error)
				return &stored
			}

			t.Run("a stale channel task copy cannot write back the pre-settlement quota", func(t *testing.T) {
				const userID, channelID = 801, 801
				seedWallet(t, userID, 10_000)
				seedChannel(t, channelID)
				task := makeTask(userID, channelID, 3_000, 0, BillingSourceWallet, 0)
				task.TaskID = "stale_copy"
				task.PrivateData.ChannelAttempt = &relaycommon.TaskChannelSnapshot{ChannelID: channelID}
				require.NoError(t, task.Insert())

				winner := reloadTask(task.ID)
				winner.Status, winner.Progress = model.TaskStatusSuccess, "100%"
				won, err := winner.UpdateWithStatus(model.TaskStatusInProgress)
				require.NoError(t, err)
				require.True(t, won)
				stale := reloadTask(task.ID)
				RecalculateTaskQuota(ctx, winner, 2_000, common.NewMessage("Adjusted by the channel adaptor"))
				require.Equal(t, 2_000, reloadTask(task.ID).Quota)

				stale.FailReason = "stale write"
				won, err = stale.UpdateWithStatus(model.TaskStatusSuccess)
				require.NoError(t, err)
				assert.False(t, won, "a copy read before the quota write loses its CAS")
				assert.Equal(t, 2_000, reloadTask(task.ID).Quota)

				won, err = winner.UpdateWithStatus(model.TaskStatusSuccess)
				require.NoError(t, err)
				assert.True(t, won, "the settling copy keeps writing")
				assert.Equal(t, 2_000, reloadTask(task.ID).Quota)
			})

			t.Run("a failed task re-charge is retried by the poller", func(t *testing.T) {
				const userID, channelID, subID = 802, 802, 802
				const refunded = 3_000
				seedWallet(t, userID, 10_000)
				seedChannel(t, channelID)
				seedSubscription(t, subID, userID, 100_000, 99_000)
				task := makeTask(userID, channelID, 0, 0, BillingSourceSubscription, subID)
				task.TaskID = "recharge_retry"
				task.Status, task.Progress, task.RefundedQuota = model.TaskStatusSuccess, "100%", refunded
				task.PrivateData.UpstreamTaskID = "upstream_recharge_retry"
				require.NoError(t, db.Create(task).Error)

				assert.False(t, RechargeRefundedTaskQuota(ctx, reloadTask(task.ID)), "the subscription cannot absorb the charge")
				assert.Equal(t, refunded, reloadTask(task.ID).RefundedQuota)
				assert.True(t, model.HasUnfinishedSyncTasks(), "the pending re-charge keeps the poller running")
				assert.Empty(t, model.GetAllUnFinishSyncTasks(10), "the vendor is not polled again")

				require.NoError(t, db.Model(&model.UserSubscription{}).Where("id = ?", subID).Update("amount_total", 200_000).Error)
				RetryPendingTaskRefundBilling(ctx)
				RetryPendingTaskRefundBilling(ctx)
				stored := reloadTask(task.ID)
				assert.EqualValues(t, model.TaskStatusSuccess, stored.Status)
				assert.Equal(t, refunded, stored.Quota)
				assert.Zero(t, stored.RefundedQuota)
				assert.EqualValues(t, 99_000+refunded, getSubscriptionUsed(t, subID), "charged exactly once")
				assert.False(t, model.HasUnfinishedSyncTasks())
			})

			t.Run("a lost task refund whose withdrawal failed is taken back by the poller", func(t *testing.T) {
				const userID, channelID, quota = 803, 803, 3_000
				seedWallet(t, userID, 10_000)
				seedChannel(t, channelID)
				task := makeTask(userID, channelID, quota, 0, BillingSourceWallet, 0)
				task.TaskID = "withdrawal_retry"
				task.Status, task.Progress = model.TaskStatusSuccess, "100%"
				require.NoError(t, db.Create(task).Error)
				stale := reloadTask(task.ID)

				allowWalletWrites = 1
				assert.False(t, RefundTaskQuota(ctx, reloadTask(task.ID), "late failure"), "a SUCCESS wins over the refund claim")
				allowWalletWrites = -1
				stored := reloadTask(task.ID)
				assert.Equal(t, quota, stored.Quota)
				assert.Equal(t, quota, stored.RefundWithdrawalQuota)
				assert.Equal(t, 10_000+quota, getUserQuota(t, userID))
				assert.True(t, model.HasUnfinishedSyncTasks())

				// Change a field: MySQL reports zero affected rows for an
				// identical write, which callers already treat as a lost CAS.
				stale.Progress = "99%"
				won, err := stale.UpdateWithStatus(model.TaskStatusSuccess)
				require.NoError(t, err)
				require.True(t, won)
				assert.Equal(t, quota, reloadTask(task.ID).RefundWithdrawalQuota, "a task write never clears the pending withdrawal")

				RetryPendingTaskRefundBilling(ctx)
				RetryPendingTaskRefundBilling(ctx)
				stored = reloadTask(task.ID)
				assert.Zero(t, stored.RefundWithdrawalQuota)
				assert.Equal(t, quota, stored.Quota)
				assert.Equal(t, 10_000, getUserQuota(t, userID), "withdrawn exactly once")
				assert.False(t, model.HasUnfinishedSyncTasks())
			})

			t.Run("Midjourney re-charge and withdrawal failures are retried by the poller", func(t *testing.T) {
				const userID, channelID, quota = 804, 804, 20
				seedWallet(t, userID, 1_000)
				seedChannel(t, channelID)
				recharge := &model.Midjourney{UserId: userID, ChannelId: channelID, MjId: "mj-recharge", Action: constant.MjActionImagine,
					Status: "SUCCESS", Progress: "100%", RefundedQuota: quota}
				withdrawal := &model.Midjourney{UserId: userID, ChannelId: channelID, MjId: "mj-withdrawal", Action: constant.MjActionImagine,
					Status: "SUCCESS", Progress: "100%", Quota: quota}
				require.NoError(t, recharge.Insert())
				require.NoError(t, withdrawal.Insert())

				allowWalletWrites = 0
				assert.False(t, RechargeRefundedMidjourneyQuota(ctx, recharge))
				allowWalletWrites = 1
				assert.False(t, RefundMidjourneyQuota(ctx, withdrawal, "late failure"))
				allowWalletWrites = -1
				assert.Equal(t, quota, getMidjourneyTask(t, recharge.Id).RefundedQuota)
				assert.Equal(t, quota, getMidjourneyTask(t, withdrawal.Id).RefundWithdrawalQuota)
				assert.Equal(t, 1_000+quota, getUserQuota(t, userID))
				assert.True(t, model.HasUnfinishedMidjourneyTasks())
				assert.Empty(t, model.GetAllUnFinishTasks(), "the vendor is not polled again")

				RetryPendingMidjourneyRefundBilling(ctx)
				RetryPendingMidjourneyRefundBilling(ctx)
				assert.Zero(t, getMidjourneyTask(t, recharge.Id).RefundedQuota)
				assert.Equal(t, quota, getMidjourneyTask(t, recharge.Id).Quota)
				assert.Zero(t, getMidjourneyTask(t, withdrawal.Id).RefundWithdrawalQuota)
				assert.Equal(t, 1_000-quota, getUserQuota(t, userID), "each owed charge is applied exactly once")
				assert.False(t, model.HasUnfinishedMidjourneyTasks())
			})
		})
	}
}
