package controller

import (
	"context"
	"crypto/tls"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestSupplierAsyncFactsRemainOwnerScoped(t *testing.T) {
	_, _ = setupSecurityEnrollmentTest(t)
	require.NoError(t, model.DB.AutoMigrate(&model.Channel{}, &model.SupplierAccount{}, &model.OverviewAttemptEvent{}, &model.OverviewCollectionState{}))
	require.NoError(t, model.AutoMigrateChannelLimitSchema(model.DB))
	now := time.Now().Unix()
	for i, event := range []model.OverviewAttemptEvent{
		{CallStage: "task_submit", Success: true, TaskStatus: "running", LatencyMS: 20},
		{CallStage: "task_submit", Success: true, TaskStatus: "failed", LatencyMS: 40, TaskDurationMS: common.GetPointer(int64(800)), Credits: common.GetPointer("1234567890.123456789"), CreditUsageSource: "upstream_usage"},
		{CallStage: "task_submit", Failure: true, UpstreamStatusCode: 429, LatencyMS: 30},
		{CallStage: "task_submit", TaskStatus: "unknown", LatencyMS: 50},
		{CallStage: "realtime_generation", Success: true, LatencyMS: 100, Credits: common.GetPointer("0.000000001")},
	} {
		event.ID, event.SupplierReference, event.AuthorizedSupplierID = fmt.Sprint("async-fact-", i), fmt.Sprint("local-", i), "owner-a"
		event.ChannelID, event.ModelName, event.StartedAt, event.FinishedAt = 1, "model-a", now, now
		require.NoError(t, model.DB.Create(&event).Error)
	}
	require.NoError(t, model.DB.Create(&model.OverviewAttemptEvent{ID: "foreign-task", SupplierReference: "foreign-reference", AuthorizedSupplierID: "owner-b", ChannelID: 2, CallStage: "task_submit", TaskStatus: "succeeded", Success: true, StartedAt: now, Credits: common.GetPointer("999")}).Error)
	response := supplierTestRequest(http.MethodGet, "/api/supplier/overview", "", "owner-a", 0, GetSupplierOverview)
	var overview struct {
		Success bool
		Data    supplierOverviewMetrics
	}
	require.NoError(t, common.Unmarshal(response.Body.Bytes(), &overview))
	require.True(t, overview.Success, response.Body.String())
	assert.EqualValues(t, 5, overview.Data.RequestCount)
	assert.EqualValues(t, 1, overview.Data.SuccessCount, "accepted submissions are not successful generations")
	assert.Zero(t, overview.Data.FailureCount)
	assert.EqualValues(t, 1, overview.Data.RealtimeGenerationCount)
	require.NotNil(t, overview.Data.AverageLatency)
	assert.Equal(t, float64(100), *overview.Data.AverageLatency)
	assert.EqualValues(t, 4, overview.Data.Async.SubmittedCount)
	assert.EqualValues(t, 2, overview.Data.Async.AcceptedCount)
	assert.EqualValues(t, 1, overview.Data.Async.RejectedCount)
	assert.EqualValues(t, 1, overview.Data.Async.UnknownSubmissionCount)
	assert.EqualValues(t, 1, overview.Data.Async.RunningCount)
	assert.EqualValues(t, 1, overview.Data.Async.FailedCount)
	assert.EqualValues(t, 1, overview.Data.Async.UnknownCount)
	assert.Zero(t, overview.Data.Async.SucceededCount)
	require.NotNil(t, overview.Data.Credits)
	assert.Equal(t, "1234567890.12345679", *overview.Data.Credits)
	assert.EqualValues(t, 2, overview.Data.CreditUsageKnownCount)
	logs := supplierTestRequest(http.MethodGet, "/api/supplier/logs?task_status=failed", "", "owner-a", 0, GetSupplierLogs)
	var envelope struct {
		Success bool
		Data    struct {
			Items []supplierLogResponse
			Total int
		}
	}
	require.NoError(t, common.Unmarshal(logs.Body.Bytes(), &envelope))
	require.True(t, envelope.Success, logs.Body.String())
	require.Len(t, envelope.Data.Items, 1)
	assert.Equal(t, "success", envelope.Data.Items[0].Status)
	assert.Equal(t, "failed", envelope.Data.Items[0].TaskStatus)
	assert.Equal(t, "async_task", envelope.Data.Items[0].Protocol)
	exported := supplierTestRequest(http.MethodGet, "/api/supplier/logs/export?stage=task_submit", "", "owner-a", 0, ExportSupplierLogs)
	assert.Contains(t, exported.Body.String(), "task_completed_at")
	assert.Contains(t, exported.Body.String(), "1234567890.123456789")
	assert.NotContains(t, exported.Body.String(), "foreign-reference")
}

func TestSupplierPluginAuthorizationCannotExpandFromChannelType(t *testing.T) {
	account := model.SupplierAccount{AllowedTypes: "[1,61]", AllowedModels: "test"}
	assert.True(t, account.Allows(constant.ChannelTypeOpenAI, []string{"test"}))
	assert.False(t, account.Allows(constant.ChannelTypeTaskPlugin, []string{"test"}))
	assert.False(t, account.AllowsTemplate("plugin:kling", []string{"test"}))
	account.AllowedTemplates = `["plugin:kling"]`
	assert.True(t, account.AllowsTemplate("plugin:kling", []string{"test"}))
	assert.False(t, account.AllowsTemplate("plugin:sora", []string{"test"}))
	assert.False(t, account.AllowsTemplate("plugin:kling", []string{"foreign-model"}))
	assert.False(t, account.Allows(constant.ChannelTypeOpenAI, []string{"test"}))
}

func TestSupplierAsyncValidationWaitsForActualCompletion(t *testing.T) {
	user, _ := setupSecurityEnrollmentTest(t)
	require.NoError(t, model.DB.AutoMigrate(&model.Channel{}, &model.Ability{}, &model.SupplierAccount{}, &model.SupplierChannelDraft{}, &model.Task{}, &model.OverviewAttemptEvent{}))
	require.NoError(t, model.AutoMigrateUpstreamCredentialSchema(model.DB))
	require.NoError(t, model.AutoMigrateChannelLimitSchema(model.DB))
	t.Setenv("SUPPLIER_CREDENTIAL_KEYS", `{"v1":"`+base64.StdEncoding.EncodeToString([]byte("01234567890123456789012345678901"))+`"}`)
	t.Setenv("SUPPLIER_CREDENTIAL_ACTIVE_KEY", "v1")
	previousRatios := ratio_setting.ModelRatio2JSONString()
	require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(`{"kling-v3":1}`))
	previousAdaptor := service.GetTaskAdaptorFunc
	service.GetTaskAdaptorFunc = func(platform constant.TaskPlatform) service.TaskPollingAdaptor { return relay.GetTaskAdaptor(platform) }
	t.Cleanup(func() {
		require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(previousRatios))
		service.GetTaskAdaptorFunc = previousAdaptor
	})
	account := model.SupplierAccount{SupplierID: "probe-owner", SupplyEnabled: true, AllowedTemplates: `["plugin:kling"]`, AllowedModels: "kling-v3", Group: "default", MaxChannels: 10, PoolWeight: 1, CostRatio: common.GetPointer(0.0)}
	require.NoError(t, model.DB.Create(&model.UpstreamSupplier{ID: account.SupplierID, Name: "Probe owner"}).Error)
	require.NoError(t, model.SaveSupplierAccount(&account, 0))
	secret, err := model.EncryptSupplierSecret(account.SupplierID, "probe-private-key")
	require.NoError(t, err)
	draft := model.SupplierChannelDraft{SupplierID: account.SupplierID, Type: constant.ChannelTypeTaskPlugin, TemplateID: "plugin:kling", Name: "async probe", BaseURL: "https://1.1.1.1", Secret: secret, Models: "kling-v3"}
	channel, err := model.SaveSupplierChannelDraft(&draft, user.Id, 0, 0)
	require.NoError(t, err)
	require.NoError(t, model.EnsureChannelLimitTracking(channel.Id, true, time.Now()))
	require.NoError(t, model.DB.Model(channel).Update("supplier_paused", true).Error)
	require.NoError(t, model.DB.Model(&draft).Update("status", "pending").Error)
	client := service.GetSupplierHTTPClient()
	transport := client.Transport
	t.Cleanup(func() { client.Transport = transport })
	submits, polls, completed := 0, 0, false
	client.Transport = schedulingTokenTransport(func(request *http.Request) (*http.Response, error) {
		body := `{"code":0,"data":{"task_id":"vendor-probe-id","task_status":"submitted"}}`
		if request.Method == http.MethodPost {
			submits++
		} else {
			polls++
			if completed {
				body = `{"code":0,"data":{"task_id":"vendor-probe-id","task_status":"succeed","task_result":{"videos":[{"url":"https://cdn.example/test.mp4","duration":"5"}]},"final_unit_deduction":"1"}}`
			}
		}
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}, nil
	})
	staleDraft := draft
	for iteration := range 2 {
		validateSupplierChannelDraft(channel.Id, account.SupplierID, draft.Version, user.Id)
		require.NoError(t, model.DB.First(&draft, "channel_id = ?", channel.Id).Error)
		assert.Equal(t, "waiting", draft.Status)
		stored, err := model.GetChannelById(channel.Id, false)
		require.NoError(t, err)
		assert.False(t, stored.SupplierValidated, "acceptance does not prove generation capability")
		assert.True(t, stored.SupplierPaused)
		if iteration == 0 {
			// A worker can hold an old empty probe map when its lease expires.
			// Reclaiming the same version must not submit the paid task twice.
			require.NoError(t, model.DB.Model(&draft).Update("status", "testing").Error)
			candidate := *channel
			candidate.Key, candidate.BaseURL, candidate.Models = "probe-private-key", &draft.BaseURL, draft.Models
			_ = supplierTaskProbe(context.Background(), &staleDraft, &candidate, user.Id, "kling-v3", 0)
			assert.Equal(t, 1, submits)
			require.NoError(t, model.DB.Model(&draft).Update("status", "waiting").Error)
		}
	}
	assert.Equal(t, 1, submits, "repeated validation resumes the accepted task")
	assert.Positive(t, polls)
	completed = true
	validateSupplierChannelDraft(channel.Id, account.SupplierID, draft.Version, user.Id)
	require.NoError(t, model.DB.First(&draft, "channel_id = ?", channel.Id).Error)
	assert.Equal(t, "validated", draft.Status)
	stored, err := model.GetChannelById(channel.Id, false)
	require.NoError(t, err)
	assert.True(t, stored.SupplierValidated)
	assert.True(t, stored.SupplierPaused, "validation cannot clear a supplier pause")
	assert.Equal(t, 1, submits)
	var task model.Task
	require.NoError(t, model.DB.Where("channel_id = ?", channel.Id).First(&task).Error)
	assert.Zero(t, task.Quota)
	require.NotNil(t, task.PrivateData.ChannelAttempt)
	assert.True(t, task.PrivateData.ChannelAttempt.Encrypted)
	assert.NotContains(t, task.PrivateData.ChannelAttempt.Connection, "probe-private-key")
	assert.Empty(t, task.PrivateData.Key)
	var testUser model.User
	require.NoError(t, model.DB.First(&testUser, user.Id).Error)
	assert.Equal(t, user.Quota, testUser.Quota, "controlled probes never charge the supplier's consumer wallet")
}

func TestSupplierPendingConnectionSurvivesMetadataEdits(t *testing.T) {
	user, _ := setupSecurityEnrollmentTest(t)
	previousSecure := common.SessionCookieSecure
	common.SessionCookieSecure = true
	t.Cleanup(func() { common.SessionCookieSecure = previousSecure })
	require.NoError(t, model.DB.AutoMigrate(&model.Channel{}, &model.Ability{}, &model.SupplierAccount{}, &model.SupplierChannelDraft{}))
	require.NoError(t, model.AutoMigrateUpstreamCredentialSchema(model.DB))
	require.NoError(t, model.AutoMigrateChannelLimitSchema(model.DB))
	t.Setenv("SUPPLIER_CREDENTIAL_KEYS", `{"v1":"`+base64.StdEncoding.EncodeToString([]byte("01234567890123456789012345678901"))+`"}`)
	t.Setenv("SUPPLIER_CREDENTIAL_ACTIVE_KEY", "v1")
	account := model.SupplierAccount{SupplierID: "pending-owner", SupplyEnabled: true, AllowedTypes: "[1]", AllowedModels: "model-a", Group: "default", MaxChannels: 10, PoolWeight: 1}
	require.NoError(t, model.DB.Create(&model.UpstreamSupplier{ID: account.SupplierID, Name: "Pending owner"}).Error)
	require.NoError(t, model.SaveSupplierAccount(&account, 0))
	channel := model.Channel{OwnerSupplierID: account.SupplierID, Name: "live", Type: 1, Key: "live-private-key", BaseURL: common.GetPointer("https://1.1.1.1/live"), Models: "model-a", SupplierValidated: true, SupplierVersion: 1, Status: common.ChannelStatusEnabled, Group: "default"}
	require.NoError(t, channel.Insert())
	require.NoError(t, model.EnsureChannelLimitTracking(channel.Id, true, time.Now()))
	secret, err := model.EncryptSupplierSecret(account.SupplierID, "pending-private-key")
	require.NoError(t, err)
	draft := model.SupplierChannelDraft{ChannelID: channel.Id, SupplierID: account.SupplierID, Type: 1, TemplateID: "1", BaseURL: "https://1.1.1.1/pending", Models: "model-a", Name: "pending", Secret: secret, ProbeTaskIDs: `{"model-a:0":"existing-paid-probe"}`}
	updated, err := model.SaveSupplierChannelDraft(&draft, user.Id, 1, 0)
	require.NoError(t, err)
	require.NoError(t, model.DB.Model(&draft).Update("status", "waiting").Error)
	draft.Status = "waiting"
	view := supplierChannelDTO(updated, &draft, "Pending owner")
	assert.Equal(t, draft.BaseURL, view.BaseURL)
	assert.True(t, view.HasValidatedConfig)
	body := fmt.Sprintf(`{"name":"renamed","template_id":"1","base_url":%q,"models":["model-a"],"priority":7,"version":%d}`, draft.BaseURL, updated.SupplierVersion)
	response := supplierTestRequest(http.MethodPut, "/channel", body, account.SupplierID, channel.Id, func(c *gin.Context) { c.Request.TLS = &tls.ConnectionState{}; SaveSupplierChannel(c) })
	assert.Contains(t, response.Body.String(), `"success":true`)
	assert.NotContains(t, response.Body.String(), "private-key")
	require.NoError(t, model.DB.First(&draft, "channel_id = ?", channel.Id).Error)
	assert.Equal(t, "waiting", draft.Status)
	assert.EqualValues(t, 3, draft.Version)
	assert.Equal(t, `{"model-a:0":"existing-paid-probe"}`, draft.ProbeTaskIDs)
	decrypted, err := model.DecryptSupplierSecret(account.SupplierID, draft.Secret)
	require.NoError(t, err)
	assert.Equal(t, "pending-private-key", decrypted)
	live, err := model.GetChannelById(channel.Id, true)
	require.NoError(t, err)
	assert.Equal(t, "live-private-key", live.Key)
	assert.Equal(t, "https://1.1.1.1/live", live.GetBaseURL())
	assert.Equal(t, "renamed", live.Name)
}

func TestSupplierConnectionChangesRequireValidatedEditor(t *testing.T) {
	_, _ = setupSecurityEnrollmentTest(t)
	require.NoError(t, model.DB.AutoMigrate(&model.Channel{}, &model.Ability{}))
	require.NoError(t, model.AutoMigrateUpstreamCredentialSchema(model.DB))
	require.NoError(t, model.AutoMigrateChannelLimitSchema(model.DB))
	t.Setenv("SUPPLIER_CREDENTIAL_KEYS", `{"v1":"`+base64.StdEncoding.EncodeToString([]byte("01234567890123456789012345678901"))+`"}`)
	t.Setenv("SUPPLIER_CREDENTIAL_ACTIVE_KEY", "v1")
	require.NoError(t, model.DB.Create(&model.UpstreamSupplier{ID: "supplier-a", Name: "A"}).Error)
	setting, header, mapping := `{"force_format":true}`, `{"X-Provider":"old"}`, `{"test-model":"old-model"}`
	channel := model.Channel{Name: "validated", Type: 1, OwnerSupplierID: "supplier-a", Key: "secret-one\nsecret-two", Models: "test-model", Group: "default", Tag: common.GetPointer("supplier-tag"), SupplierValidated: true, SupplierVersion: 1, Setting: &setting, HeaderOverride: &header, ModelMapping: &mapping, Other: "old", OtherSettings: `{"old":true}`, ChannelInfo: model.ChannelInfo{IsMultiKey: true, MultiKeySize: 2}}
	require.NoError(t, channel.Insert())
	for _, change := range []string{`"settings":"{}"`, `"other":""`, `"setting":"{}"`, `"header_override":"{}"`, `"model_mapping":"{}"`, `"models":"another-model"`} {
		body := fmt.Sprintf(`{"id":%d,"type":1,"models":"test-model",%s}`, channel.Id, change)
		response := supplierTestRequest("PUT", "/api/channel/", body, "", 0, func(c *gin.Context) {
			c.Set("role", common.RoleRootUser)
			UpdateChannel(c)
		})
		assert.Contains(t, response.Body.String(), "Use the supplier channel editor", change)
	}
	for _, action := range []string{"delete_key", "delete_disabled_keys"} {
		response := supplierTestRequest("POST", "/api/channel/multi_key/manage", fmt.Sprintf(`{"channel_id":%d,"action":%q,"key_index":0}`, channel.Id, action), "", 0, func(c *gin.Context) {
			c.Set("role", common.RoleRootUser)
			ManageMultiKeys(c)
		})
		assert.Contains(t, response.Body.String(), "Use the supplier channel editor")
	}
	stored, err := model.GetChannelById(channel.Id, true)
	require.NoError(t, err)
	assert.Equal(t, channel.Key, stored.Key)
	assert.Equal(t, channel.OtherSettings, stored.OtherSettings)
	assert.Equal(t, channel.Setting, stored.Setting)
	assert.Equal(t, channel.HeaderOverride, stored.HeaderOverride)
	assert.Equal(t, channel.ModelMapping, stored.ModelMapping)
	assert.EqualValues(t, 1, stored.SupplierVersion)
	assert.ErrorIs(t, model.EditChannelByTag("supplier-tag", nil, nil, nil, nil, nil, nil, nil, common.GetPointer("{}")), model.ErrSupplierChannelOperation)
	_, err = fetchChannelUpstreamModelIDs(stored)
	assert.ErrorIs(t, err, model.ErrSupplierChannelOperation)
	_, err = updateChannelBalance(stored)
	assert.ErrorIs(t, err, model.ErrSupplierChannelOperation)
	_, err = GetResponseBody(http.MethodGet, "https://1.1.1.1", stored, nil)
	assert.ErrorIs(t, err, model.ErrSupplierChannelOperation)
	_, err = fetchInferenceStatus(context.Background(), stored)
	assert.ErrorIs(t, err, model.ErrSupplierChannelOperation)
	_, _, err = service.RefreshCodexChannelCredential(context.Background(), stored.Id, service.CodexCredentialRefreshOptions{})
	assert.ErrorIs(t, err, model.ErrSupplierChannelOperation)
	for _, handler := range []gin.HandlerFunc{OllamaPullModel, OllamaPullModelStream, OllamaDeleteModel, OllamaVersion, GetCodexChannelUsage} {
		response := supplierTestRequest("POST", "/utility", fmt.Sprintf(`{"channel_id":%d,"model_name":"test-model"}`, stored.Id), "", stored.Id, handler)
		assert.Contains(t, response.Body.String(), model.ErrSupplierChannelOperation.Error())
		assert.NotContains(t, response.Body.String(), "secret-one")
	}
	assert.ErrorIs(t, updateChannelUpstreamModelSettings(stored, stored.GetOtherSettings(), true), model.ErrSupplierChannelOperation)
	assert.ErrorIs(t, (&model.Channel{Id: channel.Id}).Delete(), model.ErrSupplierChannelOperation)
	_, err = model.BatchDeleteChannels([]int{channel.Id})
	assert.ErrorIs(t, err, model.ErrSupplierChannelOperation)
	require.NoError(t, model.DB.Model(&model.Channel{}).Where("id = ?", channel.Id).Update("status", common.ChannelStatusManuallyDisabled).Error)
	_, err = model.DeleteDisabledChannel()
	assert.ErrorIs(t, err, model.ErrSupplierChannelOperation)
	_, err = model.DeleteChannelByStatus(common.ChannelStatusManuallyDisabled)
	assert.ErrorIs(t, err, model.ErrSupplierChannelOperation)
	stored, err = model.GetChannelById(channel.Id, true)
	require.NoError(t, err)
	assert.Equal(t, header, *stored.HeaderOverride)
	assert.Equal(t, channel.Key, stored.Key)
}

func TestSupplierDraftValidationCanProbeNewPausedChannel(t *testing.T) {
	user, _ := setupSecurityEnrollmentTest(t)
	previousRatios := ratio_setting.ModelRatio2JSONString()
	t.Cleanup(func() { require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(previousRatios)) })
	require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(`{"gpt-4o-mini":1}`))
	require.NoError(t, model.DB.AutoMigrate(&model.Channel{}, &model.Ability{}, &model.SupplierAccount{}, &model.SupplierChannelDraft{}))
	require.NoError(t, model.AutoMigrateUpstreamCredentialSchema(model.DB))
	require.NoError(t, model.AutoMigrateChannelLimitSchema(model.DB))
	t.Setenv("SUPPLIER_CREDENTIAL_KEYS", `{"v1":"`+base64.StdEncoding.EncodeToString([]byte("01234567890123456789012345678901"))+`"}`)
	t.Setenv("SUPPLIER_CREDENTIAL_ACTIVE_KEY", "v1")
	require.NoError(t, model.DB.Create(&model.UpstreamSupplier{ID: "supplier-a", Name: "A"}).Error)
	account := model.SupplierAccount{SupplierID: "supplier-a", SupplyEnabled: true, AllowedTypes: "[1]", AllowedModels: "gpt-4o-mini", Group: "default", MaxChannels: 10, PoolWeight: 1}
	require.NoError(t, model.SaveSupplierAccount(&account, 0))
	secret, err := model.EncryptSupplierSecret(account.SupplierID, "draft-secret")
	require.NoError(t, err)
	draft := model.SupplierChannelDraft{SupplierID: account.SupplierID, Type: 1, Name: "new", BaseURL: "https://1.1.1.1/v1", Secret: secret, Models: "gpt-4o-mini"}
	channel, err := model.SaveSupplierChannelDraft(&draft, user.Id, 0, 0)
	require.NoError(t, err)
	require.NoError(t, model.EnsureChannelLimitTracking(channel.Id, true, time.Now()))
	require.NoError(t, model.DB.Model(channel).Update("supplier_paused", true).Error)
	require.NoError(t, model.DB.Model(&draft).Update("status", "pending").Error)
	client := service.GetSupplierHTTPClient()
	previousTransport := client.Transport
	t.Cleanup(func() { client.Transport = previousTransport })
	sent := 0
	client.Transport = schedulingTokenTransport(func(request *http.Request) (*http.Response, error) {
		sent++
		assert.Equal(t, "Bearer draft-secret", request.Header.Get("Authorization"))
		return &http.Response{StatusCode: http.StatusUnauthorized, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"invalid key draft-secret","type":"invalid_request_error","code":"invalid_api_key"}}`))}, nil
	})
	validateSupplierChannelDraft(channel.Id, account.SupplierID, draft.Version, user.Id)
	assert.Equal(t, 1, sent, "the draft probe must reach its upstream without enabling the live channel")
	stored, err := model.GetChannelById(channel.Id, false)
	require.NoError(t, err)
	assert.False(t, stored.SupplierValidated)
	assert.True(t, stored.SupplierPaused)
	require.NoError(t, model.DB.First(&draft, "channel_id = ?", channel.Id).Error)
	assert.Equal(t, "failed", draft.Status)
	// A completed version and a copied channel must both schedule a real probe.
	require.NoError(t, model.PublishSupplierChannelDraft(channel.Id, account.SupplierID, draft.Version, []string{"gpt-4o-mini"}))
	for _, copyChannel := range []bool{false, true} {
		id, version := channel.Id, draft.Version
		if copyChannel {
			id, err = model.CopySupplierChannel(channel.Id, account.SupplierID, draft.Version, user.Id, "copy")
			require.NoError(t, err)
			version = 1
		}
		status, err := model.QueueSupplierChannelValidation(id, account.SupplierID, version)
		require.NoError(t, err)
		assert.Equal(t, "pending", status)
		before := sent
		validateSupplierChannelDraft(id, account.SupplierID, version, user.Id)
		assert.Equal(t, before+1, sent)
		_, err = model.QueueSupplierChannelValidation(id, account.SupplierID, version+1)
		assert.Error(t, err)
		stored, err := model.GetChannelById(id, false)
		require.NoError(t, err)
		assert.True(t, stored.SupplierValidated, "a failed re-test must retain the working version")
		assert.True(t, stored.SupplierPaused)
	}

	t.Run("image model validation uses its image endpoint", func(t *testing.T) {
		previousPrices := ratio_setting.ModelPrice2JSONString()
		t.Cleanup(func() { require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(previousPrices)) })
		require.NoError(t, ratio_setting.UpdateModelPriceByJSONString(`{"dall-e-3":0.04}`))
		account.AllowedModels = "gpt-4o-mini,dall-e-3"
		require.NoError(t, model.SaveSupplierAccount(&account, account.Revision))
		imageDraft := model.SupplierChannelDraft{SupplierID: account.SupplierID, Type: 1, Name: "image", BaseURL: "https://1.1.1.1", Secret: secret, Models: "dall-e-3"}
		imageChannel, err := model.SaveSupplierChannelDraft(&imageDraft, user.Id, 0, 0)
		require.NoError(t, err)
		require.NoError(t, model.EnsureChannelLimitTracking(imageChannel.Id, true, time.Now()))
		require.NoError(t, model.DB.Model(&imageDraft).Update("status", "pending").Error)
		previousTransport := client.Transport
		t.Cleanup(func() { client.Transport = previousTransport })
		imageSent := false
		client.Transport = schedulingTokenTransport(func(request *http.Request) (*http.Response, error) {
			imageSent = true
			assert.Equal(t, "/v1/images/generations", request.URL.Path)
			return &http.Response{StatusCode: http.StatusUnauthorized, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"error":{"message":"invalid key"}}`))}, nil
		})
		validateSupplierChannelDraft(imageChannel.Id, account.SupplierID, imageDraft.Version, user.Id)
		assert.True(t, imageSent)
	})

	probe := *channel
	probe.SupplierValidated, probe.SupplierPaused, probe.Models, probe.Key, probe.BaseURL = true, false, draft.Models, "draft-secret", &draft.BaseURL
	result := testChannel(nil, &probe, user.Id, draft.Models, "", false)
	require.Error(t, result.localErr)
	assert.Equal(t, "Channel test failed", result.localErr.Error())
	require.NotNil(t, result.newAPIError)
	assert.NotContains(t, result.newAPIError.Error(), "draft-secret")
	assert.Equal(t, http.StatusUnauthorized, result.newAPIError.StatusCode)
}

func TestSupplierIsolationReadModelsAndLimitUpdates(t *testing.T) {
	previousSecure := common.SessionCookieSecure
	common.SessionCookieSecure = true
	t.Cleanup(func() { common.SessionCookieSecure = previousSecure })
	_, _ = setupSecurityEnrollmentTest(t)
	require.NoError(t, model.DB.AutoMigrate(&model.Channel{}, &model.Ability{}, &model.SupplierAccount{}, &model.SupplierChannelDraft{}, &model.OverviewAttemptEvent{}, &model.OverviewCollectionState{}))
	require.NoError(t, model.AutoMigrateUpstreamCredentialSchema(model.DB))
	require.NoError(t, model.AutoMigrateChannelLimitSchema(model.DB))
	t.Setenv("SUPPLIER_CREDENTIAL_KEYS", `{"v1":"`+base64.StdEncoding.EncodeToString([]byte("01234567890123456789012345678901"))+`"}`)
	t.Setenv("SUPPLIER_CREDENTIAL_ACTIVE_KEY", "v1")
	now := time.Now()
	channels := make([]model.Channel, 2)
	for i, owner := range []string{"supplier-a", "supplier-b"} {
		require.NoError(t, model.DB.Create(&model.UpstreamSupplier{ID: owner, Name: owner}).Error)
		account := model.SupplierAccount{SupplierID: owner, SupplyEnabled: true, AllowedTypes: "[1]", AllowedModels: "test-model", Group: "default", MaxChannels: 10, PoolWeight: 1}
		require.NoError(t, model.SaveSupplierAccount(&account, 0))
		channels[i] = model.Channel{Name: owner + "-channel", Type: 1, OwnerSupplierID: owner, Key: "never-return-upstream-secret", Models: "test-model", Group: "default", SupplierValidated: true, SupplierVersion: 1, Status: common.ChannelStatusEnabled}
		require.NoError(t, channels[i].Insert())
		require.NoError(t, model.EnsureChannelLimitTracking(channels[i].Id, true, now))
		require.NoError(t, model.DB.Create(&model.OverviewAttemptEvent{ID: fmt.Sprintf("request:%d", i), RequestID: "global-private-request-id", AuthorizedSupplierID: owner, SupplierReference: owner + "-reference", ChannelID: channels[i].Id, ChannelName: channels[i].Name, StartedAt: now.Unix(), FinishedAt: now.Unix(), ModelName: "test-model", Success: true, InputTokens: 10, UsageSource: "upstream", RevenueQuota: 999}).Error)
	}
	// A procurement label or a channel's current identity does not grant access
	// to historical facts that lack the immutable authorized-owner snapshot.
	require.NoError(t, model.DB.Create(&model.OverviewAttemptEvent{ID: "legacy", SupplierID: "supplier-a", ChannelID: channels[0].Id, StartedAt: now.Unix(), ModelName: "legacy-private"}).Error)
	for _, test := range []struct {
		name, path string
		handler    gin.HandlerFunc
	}{
		{"list", "/api/supplier/channels", ListSupplierChannels},
		{"logs", "/api/supplier/logs", GetSupplierLogs},
		{"export", "/api/supplier/logs/export", ExportSupplierLogs},
		{"overview", "/api/supplier/overview", GetSupplierOverview},
	} {
		t.Run(test.name, func(t *testing.T) {
			response := supplierTestRequest("GET", test.path, "", "supplier-a", 0, test.handler)
			body := response.Body.String()
			assert.Equal(t, 200, response.Code)
			assert.NotContains(t, body, "supplier-b")
			assert.NotContains(t, body, "never-return")
			assert.NotContains(t, body, "global-private-request-id")
			assert.NotContains(t, body, "legacy-private")
			assert.NotContains(t, body, "revenue_quota")
			if test.name != "export" {
				assert.Contains(t, body, `"success":true`)
			}
		})
	}
	t.Run("supplier scoped request counts and error categories", func(t *testing.T) {
		bucket := now.Add(-time.Hour).Truncate(time.Hour).Unix()
		require.NoError(t, model.DB.Model(&model.OverviewAttemptEvent{}).Where("id = ?", "request:0").Update("started_at", bucket).Error)
		cases := []struct {
			reference, requestID, category string
			status                         int
			failure                        bool
		}{
			{"request-timeout", "global-private-request-id", "timeout", 408, true},
			{"gateway-timeout", "private-timeout-id", "timeout", 504, true},
			{"rate-limit", "private-rate-id", "rate_limited", 429, true},
			{"server-error", "private-retry-id", "upstream_error", 500, true},
			{"server-retry", "private-retry-id", "upstream_error", 503, true},
			{"bad-request", "private-client-id", "request_error", 400, true},
			{"unknown-failure", "private-unknown-id", "unknown", 0, true},
			{"unknown-outcome", "", "unknown", 0, false},
			{"unfinished-response", "private-unfinished-id", "unknown", 200, false},
			{"authentication-error", "private-auth-id", "auth_error", 401, true},
			{"authorization-error", "private-authorization-id", "auth_error", 403, true},
		}
		for i, tc := range cases {
			event := model.OverviewAttemptEvent{ID: "category:" + tc.reference, RequestID: tc.requestID, AuthorizedSupplierID: "supplier-a", SupplierReference: tc.reference,
				ChannelID: channels[0].Id, ChannelName: channels[0].Name, StartedAt: bucket + int64(i) + 1, ModelName: "test-model", Failure: tc.failure, UpstreamStatusCode: tc.status, LatencyMS: 3600000}
			require.NoError(t, model.DB.Create(&event).Error)
		}
		require.NoError(t, model.DB.Model(&model.OverviewAttemptEvent{}).Where("id = ?", "request:0").Updates(map[string]any{"image_count": 2, "image_usage_source": "upstream_payload", "call_stage": "image", "upstream_request_id": "vendor-rid-a"}).Error)
		require.NoError(t, model.DB.Model(&model.OverviewAttemptEvent{}).Where("id = ?", "request:1").Update("upstream_request_id", "vendor-rid-b").Error)
		require.NoError(t, model.DB.Model(&model.OverviewAttemptEvent{}).Where("id = ?", "category:server-error").Updates(map[string]any{"image_count": 3, "image_usage_source": "upstream_usage", "call_stage": "image"}).Error)
		require.NoError(t, model.DB.Model(&model.OverviewAttemptEvent{}).Where("id = ?", "category:bad-request").Updates(map[string]any{"audio_seconds": 1.25, "audio_usage_source": "upstream_usage", "call_stage": "audio"}).Error)
		require.NoError(t, model.DB.Model(&model.OverviewAttemptEvent{}).Where("id = ?", "category:unfinished-response").Updates(map[string]any{"audio_seconds": 2.5, "audio_usage_source": "media_metadata", "call_stage": "audio"}).Error)
		require.NoError(t, model.DB.Create(&model.OverviewAttemptEvent{ID: "foreign-category", RequestID: "private-foreign-id", AuthorizedSupplierID: "supplier-b", SupplierReference: "supplier-b-private-reference", ChannelID: channels[1].Id, StartedAt: now.Unix(), UpstreamStatusCode: 504, Failure: true}).Error)
		response := supplierTestRequest("GET", "/api/supplier/overview", "", "supplier-a", 0, GetSupplierOverview)
		var overview struct {
			Success bool
			Data    struct {
				supplierOverviewMetrics
				Channels []supplierOverviewMetrics `json:"channels"`
				Trend    []supplierOverviewMetrics `json:"trend"`
			}
		}
		require.NoError(t, common.Unmarshal(response.Body.Bytes(), &overview))
		require.True(t, overview.Success, response.Body.String())
		require.Len(t, overview.Data.Channels, 1)
		require.Len(t, overview.Data.Trend, 1)
		for _, metrics := range []supplierOverviewMetrics{overview.Data.supplierOverviewMetrics, overview.Data.Channels[0], overview.Data.Trend[0]} {
			assert.EqualValues(t, 12, metrics.RequestCount)
			assert.EqualValues(t, 9, metrics.LogicalRequestCount, "retries collapse only inside this supplier's filtered facts; absent IDs remain unknown")
			require.NotNil(t, metrics.LastCalledAt)
			assert.Equal(t, bucket+11, *metrics.LastCalledAt, "another supplier's later call must not change this timestamp")
			assert.EqualValues(t, 2, metrics.TimeoutCount)
			assert.EqualValues(t, 1, metrics.RateLimitedCount)
			assert.EqualValues(t, 2, metrics.AuthErrorCount)
			assert.EqualValues(t, 2, metrics.UpstreamErrorCount)
			assert.EqualValues(t, 1, metrics.RequestErrorCount)
			assert.EqualValues(t, 1, metrics.UnknownErrorCount, "in-flight and unknown outcomes are not counted as failures")
			assert.EqualValues(t, 2, metrics.UnknownCount, "long latency and missing terminal outcome are not known timeouts")
			require.NotNil(t, metrics.ImageCount)
			assert.EqualValues(t, 5, *metrics.ImageCount)
			require.NotNil(t, metrics.AudioSeconds)
			assert.Equal(t, 3.75, *metrics.AudioSeconds)
			assert.EqualValues(t, 2, metrics.ImageUsageKnownCount)
			assert.EqualValues(t, 2, metrics.AudioUsageKnownCount)
		}
		assert.NotContains(t, response.Body.String(), "private-")
		response = supplierTestRequest("GET", "/api/supplier/logs?page_size=100", "", "supplier-a", 0, GetSupplierLogs)
		var logs struct {
			Success bool
			Data    struct {
				Items []supplierLogResponse
				Total int
			}
		}
		require.NoError(t, common.Unmarshal(response.Body.Bytes(), &logs))
		require.True(t, logs.Success, response.Body.String())
		assert.Equal(t, 12, logs.Data.Total)
		categories := map[string]string{}
		for _, row := range logs.Data.Items {
			categories[row.Reference] = row.ErrorCategory
			switch row.Reference {
			case "supplier-a-reference":
				require.NotNil(t, row.ImageCount)
				assert.EqualValues(t, 2, *row.ImageCount)
				assert.Equal(t, "upstream_payload", row.ImageUsageSource)
				assert.Equal(t, "vendor-rid-a", row.UpstreamRequestID)
				assert.NotEqual(t, row.Reference, row.UpstreamRequestID, "vendor and supplier-local identifiers are independent")
			case "unfinished-response":
				require.NotNil(t, row.AudioSeconds)
				assert.Equal(t, 2.5, *row.AudioSeconds)
				assert.Equal(t, "media_metadata", row.AudioUsageSource)
			case "unknown-outcome":
				assert.Nil(t, row.ImageCount)
				assert.Nil(t, row.AudioSeconds)
				assert.Empty(t, row.UpstreamRequestID, "missing vendor identity must not be replaced by a platform request ID")
			}
		}
		assert.Equal(t, "none", categories["supplier-a-reference"])
		for _, tc := range cases {
			assert.Equal(t, tc.category, categories[tc.reference])
		}
		assert.NotContains(t, response.Body.String(), "private-")
		assert.NotContains(t, response.Body.String(), "global-private-request-id")
		assert.NotContains(t, response.Body.String(), "vendor-rid-b")
		exported := supplierTestRequest("GET", "/api/supplier/logs/export", "", "supplier-a", 0, ExportSupplierLogs)
		assert.Contains(t, exported.Body.String(), "error_category")
		assert.Contains(t, exported.Body.String(), ",rate_limited,")
		assert.NotContains(t, exported.Body.String(), "private-")
		assert.Contains(t, exported.Body.String(), "upstream_request_id")
		assert.Contains(t, exported.Body.String(), "vendor-rid-a")
		assert.NotContains(t, exported.Body.String(), "vendor-rid-b")
		empty := supplierTestRequest("GET", "/api/supplier/overview?model=no-activity", "", "supplier-a", 0, GetSupplierOverview)
		require.NoError(t, common.Unmarshal(empty.Body.Bytes(), &overview))
		require.True(t, overview.Success)
		assert.Zero(t, overview.Data.LogicalRequestCount)
		assert.Nil(t, overview.Data.LastCalledAt)
		assert.Nil(t, overview.Data.ImageCount)
		assert.Nil(t, overview.Data.AudioSeconds)
		assert.Zero(t, overview.Data.ImageUsageKnownCount)
		assert.Zero(t, overview.Data.AudioUsageKnownCount)
	})
	foreign := supplierTestRequest("GET", "/api/supplier/channels/2", "", "supplier-a", channels[1].Id, GetSupplierChannel)
	missing := supplierTestRequest("GET", "/api/supplier/channels/99999", "", "supplier-a", 99999, GetSupplierChannel)
	assert.JSONEq(t, missing.Body.String(), foreign.Body.String())
	viewer := supplierTestRequest("GET", "/api/channel/read-only/", "", "", 0, ListSupplierChannels)
	assert.Contains(t, viewer.Body.String(), "supplier-a")
	assert.Contains(t, viewer.Body.String(), "supplier-b")
	assert.NotContains(t, viewer.Body.String(), "never-return")
	for _, test := range []struct {
		body     string
		success  bool
		rpm, tpm int64
	}{
		{`{"revision":1,"rpm":5,"tpm":100}`, true, 5, 100},
		{`{"revision":2,"rpm":null}`, true, 0, 100},
		{`{"revision":3,"tpm":0}`, false, 0, 100},
		{`{"revision":3,"supplier_id":"supplier-b"}`, false, 0, 100},
		{`{"revision":2,"tpm":null}`, false, 0, 100},
	} {
		response := supplierTestRequest("PUT", "/limits", test.body, "supplier-a", channels[0].Id, UpdateSupplierChannelLimits)
		assert.Contains(t, response.Body.String(), fmt.Sprintf(`"success":%t`, test.success))
		state, err := model.GetChannelLimitState(channels[0].Id, now)
		require.NoError(t, err)
		assert.Equal(t, test.rpm, state.Config.RPM)
		assert.Equal(t, test.tpm, state.Config.TPM)
	}
	response := supplierTestRequest("PUT", "/limits", `{"revision":1,"rpm":1}`, "supplier-a", channels[1].Id, UpdateSupplierChannelLimits)
	assert.Contains(t, response.Body.String(), `"success":false`)
	t.Run("channel and nested limit updates are atomic", func(t *testing.T) {
		before, err := model.GetChannelById(channels[0].Id, true)
		require.NoError(t, err)
		for _, limits := range []string{`{"revision":3,"tpm":0}`, `{"revision":1,"tpm":200}`} {
			body := fmt.Sprintf(`{"name":"must-roll-back","template_id":"1","base_url":%q,"models":["test-model"],"remark":"must-roll-back","priority":10,"version":1,"limits":%s}`, before.GetBaseURL(), limits)
			response := supplierTestRequest("PUT", "/channel", body, "supplier-a", before.Id, func(c *gin.Context) {
				c.Request.TLS = &tls.ConnectionState{}
				SaveSupplierChannel(c)
			})
			assert.Contains(t, response.Body.String(), `"success":false`)
			after, err := model.GetChannelById(before.Id, true)
			require.NoError(t, err)
			assert.Equal(t, before.Name, after.Name)
			assert.Equal(t, before.SupplierVersion, after.SupplierVersion)
			assert.Equal(t, before.Key, after.Key)
			var drafts int64
			require.NoError(t, model.DB.Model(&model.SupplierChannelDraft{}).Where("channel_id = ?", before.Id).Count(&drafts).Error)
			assert.Zero(t, drafts)
			state, err := model.GetChannelLimitState(before.Id, now)
			require.NoError(t, err)
			assert.EqualValues(t, 3, state.Config.Revision)
			assert.EqualValues(t, 100, state.Config.TPM)
		}
	})

	t.Run("publishing cannot clear a pause or accept a stale test", func(t *testing.T) {
		secret, err := model.EncryptSupplierSecret("supplier-a", "replacement-secret")
		require.NoError(t, err)
		draft := model.SupplierChannelDraft{ChannelID: channels[0].Id, SupplierID: "supplier-a", Type: 1, Name: "revised", BaseURL: "https://example.com", Secret: secret, Models: "test-model"}
		updated, err := model.SaveSupplierChannelDraft(&draft, 1, 1, 50)
		require.NoError(t, err)
		before, err := model.GetChannelById(updated.Id, true)
		require.NoError(t, err)
		assert.Equal(t, "never-return-upstream-secret", before.Key)
		assert.Error(t, model.PublishSupplierChannelDraft(updated.Id, "supplier-a", 1, []string{"test-model"}))
		require.NoError(t, model.DB.Model(&model.Channel{}).Where("id = ?", updated.Id).Updates(map[string]any{"supplier_paused": true, "status": common.ChannelStatusManuallyDisabled}).Error)
		require.NoError(t, model.PublishSupplierChannelDraft(updated.Id, "supplier-a", updated.SupplierVersion, []string{"test-model"}))
		after, err := model.GetChannelById(updated.Id, true)
		require.NoError(t, err)
		assert.True(t, after.SupplierPaused)
		assert.Equal(t, common.ChannelStatusManuallyDisabled, after.Status)
		assert.Equal(t, "replacement-secret", after.Key)
		assert.False(t, model.SupplierChannelAvailable(after))
		copiedID, err := model.CopySupplierChannel(updated.Id, "supplier-a", updated.SupplierVersion, 1, "copy")
		require.NoError(t, err)
		copied, err := model.GetChannelById(copiedID, true)
		require.NoError(t, err)
		assert.Equal(t, "supplier-a", copied.OwnerSupplierID)
		assert.True(t, copied.SecretsWriteOnly)
		assert.True(t, copied.SupplierPaused)
		copiedLimits, err := model.GetChannelLimitState(copiedID, now)
		require.NoError(t, err)
		assert.EqualValues(t, 100, copiedLimits.Config.TPM)
		assert.Zero(t, copiedLimits.DailyUsedNano)
		account, err := model.GetSupplierAccount("supplier-a")
		require.NoError(t, err)
		ratio := 0.5
		account.CostRatio = &ratio
		require.NoError(t, model.SaveSupplierAccount(account, account.Revision))
		ready, err := model.GetChannelLimitState(updated.Id, now)
		require.NoError(t, err)
		assert.True(t, ready.Config.BudgetReady)
		assert.EqualValues(t, 100, ready.Config.TPM)
	})
}

func supplierTestRequest(method, path, body, owner string, id int, handler gin.HandlerFunc) *httptest.ResponseRecorder {
	response := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(response)
	c.Request = httptest.NewRequest(method, path, strings.NewReader(body))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Set("id", 1)
	c.Set("role", common.RoleCommonUser)
	if owner != "" {
		c.Set("role", common.RoleSupplierUser)
		c.Set("supplier_id", owner)
	}
	if id > 0 {
		c.Params = gin.Params{{Key: "id", Value: fmt.Sprint(id)}}
	}
	handler(c)
	return response
}

func TestSupplierReadModelReportsEveryBlockingReasonAndPoolSettings(t *testing.T) {
	_, _ = setupSecurityEnrollmentTest(t)
	require.NoError(t, model.DB.AutoMigrate(&model.Channel{}, &model.SupplierAccount{}, &model.SupplierChannelDraft{}))
	require.NoError(t, model.AutoMigrateUpstreamCredentialSchema(model.DB))
	require.NoError(t, model.AutoMigrateChannelLimitSchema(model.DB))
	account := model.SupplierAccount{SupplierID: "supplier-state", SupplyEnabled: true, AllowedTypes: "[1]", AllowedModels: "model-a", Group: "default", MaxChannels: 10, PoolPriority: 81, PoolWeight: 22}
	require.NoError(t, model.SaveSupplierAccount(&account, 0))
	channel := model.Channel{Name: "state", Type: 1, Models: "model-a", OwnerSupplierID: account.SupplierID, SupplierValidated: true, Status: common.ChannelStatusEnabled, Priority: common.GetPointer(int64(1)), Weight: common.GetPointer(uint(1))}
	require.NoError(t, model.DB.Create(&channel).Error)
	active := supplierChannelDTO(&channel, nil, "Supplier")
	assert.Equal(t, "available", active.Status)
	assert.Empty(t, active.BlockingReasons)
	assert.EqualValues(t, 81, active.PlatformPriority)
	assert.Equal(t, 22, active.Weight)

	now := time.Now()
	require.NoError(t, model.EnsureChannelLimitTracking(channel.Id, true, now))
	limit, err := model.ParseChannelLimitUSD("10")
	require.NoError(t, err)
	_, err = model.UpdateChannelLimits(channel.Id, 1, model.ChannelLimitSettings{DailyUSDNano: limit, ReconcileSeconds: model.ChannelLimitDefaultReconcile}, true, now)
	require.NoError(t, err)
	state, err := model.GetChannelLimitState(channel.Id, now)
	require.NoError(t, err)
	require.NoError(t, model.DB.Model(&model.ChannelDailyBudget{}).Where("channel_id = ? AND budget_day = ?", channel.Id, state.BudgetDay).Updates(map[string]any{"used_usd_nano": limit, "exhausted": true}).Error)
	require.NoError(t, model.DB.Model(&account).Updates(map[string]any{"allowed_models": "model-b", "supply_enabled": false}).Error)
	channel.Status, channel.SupplierPaused = common.ChannelStatusManuallyDisabled, true
	blocked := supplierChannelDTO(&channel, nil, "Supplier")
	assert.ElementsMatch(t, []string{"manual_paused", "paused", "supply_paused", "authorization_removed", "daily_exhausted"}, blocked.BlockingReasons)
	assert.Equal(t, "daily_exhausted", blocked.Status)

	// New authorized drafts must not be treated as revoked merely because the
	// active channel does not have a published model list yet.
	channel.SupplierValidated, channel.Models = false, ""
	draft := model.SupplierChannelDraft{Type: 1, Models: "model-b", Status: "pending"}
	pending := supplierChannelDTO(&channel, &draft, "Supplier")
	assert.Contains(t, pending.BlockingReasons, "pending")
	assert.NotContains(t, pending.BlockingReasons, "authorization_removed")
}

func TestSupplierCredentialTransportRequiresVerifiedHTTPS(t *testing.T) {
	previousSecure := common.SessionCookieSecure
	t.Cleanup(func() { common.SessionCookieSecure = previousSecure })
	for _, tc := range []struct {
		name, peer, proxies, proto string
		tls, secure, allowed       bool
	}{
		{"direct TLS", "198.51.100.1:443", "none", "", true, true, true},
		{"secure cookies required", "198.51.100.1:443", "none", "", true, false, false},
		{"cookie flag cannot allow plaintext", "127.0.0.1:80", "", "", false, true, false},
		{"public spoof", "198.51.100.1:80", "", "https", false, true, false},
		{"trusted proxy", "127.0.0.1:80", "127.0.0.1", "https", false, true, true},
		{"trust disabled", "127.0.0.1:80", "none", "https", false, true, false},
		{"mixed protocol chain", "127.0.0.1:80", "", "https,http", false, true, false},
		{"invalid trust configuration", "127.0.0.1:80", "127.0.0.1,bad", "https", false, true, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("TRUSTED_PROXIES", tc.proxies)
			common.SessionCookieSecure = tc.secure
			response := supplierTestRequest("POST", "/channel", "{}", "supplier-a", 0, func(c *gin.Context) {
				c.Request.RemoteAddr = tc.peer
				c.Request.Header.Set("X-Forwarded-Proto", tc.proto)
				if tc.tls {
					c.Request.TLS = &tls.ConnectionState{}
				}
				// Invalid JSON stops after the transport guard, before any database access.
				c.Request.Body = io.NopCloser(strings.NewReader("{"))
				SaveSupplierChannel(c)
			})
			assert.Equal(t, !tc.allowed, strings.Contains(response.Body.String(), "Supplier credentials require HTTPS and secure session cookies"))
		})
	}
}

func TestSupplierChannelOwnerFilterBeforePagination(t *testing.T) {
	_, _ = setupSecurityEnrollmentTest(t)
	require.NoError(t, model.DB.AutoMigrate(&model.Channel{}, &model.Ability{}, &model.ChannelFundingIncident{}))
	require.NoError(t, model.AutoMigrateUpstreamCredentialSchema(model.DB))
	for i, owner := range []string{"supplier-a", "supplier-b", ""} {
		channel := model.Channel{Name: fmt.Sprintf("scope-%d", i), Type: 1, OwnerSupplierID: owner, Models: "test-model", Group: "default", Tag: common.GetPointer("shared-tag"), Status: common.ChannelStatusEnabled}
		require.NoError(t, model.DB.Create(&channel).Error)
	}
	for _, handler := range []gin.HandlerFunc{GetAllChannels, SearchChannels} {
		for _, tagMode := range []bool{false, true} {
			for _, owner := range []string{"supplier-a", "supplier-b", "unassigned"} {
				response := supplierTestRequest("GET", fmt.Sprintf("/api/channel/?owner_supplier_id=%s&tag_mode=%t&page_size=1", owner, tagMode), "", "", 0, handler)
				var envelope struct {
					Success bool `json:"success"`
					Data    struct {
						Items      []model.Channel  `json:"items"`
						Total      int64            `json:"total"`
						TypeCounts map[string]int64 `json:"type_counts"`
					} `json:"data"`
				}
				require.NoError(t, common.Unmarshal(response.Body.Bytes(), &envelope))
				require.True(t, envelope.Success, response.Body.String())
				require.Len(t, envelope.Data.Items, 1)
				want := owner
				if owner == "unassigned" {
					want = ""
				}
				assert.Equal(t, want, envelope.Data.Items[0].OwnerSupplierID)
				assert.EqualValues(t, 1, envelope.Data.Total)
				assert.EqualValues(t, 1, envelope.Data.TypeCounts["1"])
			}
		}
	}
}
