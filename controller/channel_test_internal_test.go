package controller

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	relaytypes "github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/config"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestGetChannelDefaultBaseURLsUsesBuiltInDefaults(t *testing.T) {
	originalBaseURLs := constant.ChannelBaseURLs
	constant.ChannelBaseURLs = append([]string(nil), originalBaseURLs...)
	constant.ChannelBaseURLs[constant.ChannelTypeDeepSeek] = "https://deepseek.server.example"
	t.Cleanup(func() {
		constant.ChannelBaseURLs = originalBaseURLs
	})

	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/channel/default_base_urls", nil)
	GetChannelDefaultBaseURLs(c)

	require.Equal(t, http.StatusOK, recorder.Code)
	var response struct {
		Success bool           `json:"success"`
		Data    map[int]string `json:"data"`
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	require.True(t, response.Success)
	assert.Equal(t, "https://deepseek.server.example", response.Data[constant.ChannelTypeDeepSeek])
	assert.Equal(t, "https://api.openai.com", response.Data[constant.ChannelTypeOpenAI])
	assert.NotContains(t, response.Data, constant.ChannelTypeAzure)
	assert.NotContains(t, response.Data, constant.ChannelTypeNewAPI)
	assert.NotContains(t, response.Data, constant.ChannelTypeTaskPlugin)
}

func TestValidateChannelProxy(t *testing.T) {
	tests := []struct {
		name    string
		proxy   string
		wantErr bool
	}{
		{name: "empty"},
		{name: "http", proxy: "http://proxy.example:8080"},
		{name: "https", proxy: "https://proxy.example:8443"},
		{name: "socks5", proxy: "socks5://proxy.example"},
		{name: "socks5h", proxy: "socks5h://proxy.example:1080/"},
		{name: "unsupported", proxy: "ftp://proxy.example", wantErr: true},
		{name: "path", proxy: "socks5://proxy.example:1080/path", wantErr: true},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			setting, err := common.Marshal(dto.ChannelSettings{Proxy: test.proxy})
			require.NoError(t, err)
			channel := &model.Channel{
				Type:    constant.ChannelTypeOpenAI,
				Setting: common.GetPointer(string(setting)),
			}

			err = validateChannel(channel, false)

			if test.wantErr {
				require.ErrorContains(t, err, "invalid channel proxy")
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestValidateChannelRequiresNewAPIBaseURL(t *testing.T) {
	tests := []struct {
		name    string
		baseURL *string
		wantErr bool
	}{
		{name: "missing", wantErr: true},
		{name: "blank", baseURL: common.GetPointer("  "), wantErr: true},
		{name: "configured", baseURL: common.GetPointer("https://new-api.example")},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			channel := &model.Channel{
				Type:    constant.ChannelTypeNewAPI,
				BaseURL: test.baseURL,
			}

			err := validateChannel(channel, false)

			if test.wantErr {
				require.ErrorContains(t, err, "New API channel base URL cannot be empty")
				return
			}
			require.NoError(t, err)
		})
	}
}

func TestNewAPIChannelRegistration(t *testing.T) {
	apiType, ok := common.ChannelType2APIType(constant.ChannelTypeNewAPI)

	require.True(t, ok)
	assert.Equal(t, constant.APITypeNewAPI, apiType)
	assert.Equal(t, "New API", constant.GetChannelTypeName(constant.ChannelTypeNewAPI))
	require.Greater(t, len(constant.ChannelBaseURLs), constant.ChannelTypeNewAPI)
	assert.Empty(t, constant.ChannelBaseURLs[constant.ChannelTypeNewAPI])
}

func TestResponsesCompactChannelSupport(t *testing.T) {
	tests := []struct {
		name        string
		channelType int
		apiType     int
		want        bool
	}{
		{name: "OpenAI", channelType: constant.ChannelTypeOpenAI, apiType: constant.APITypeOpenAI, want: true},
		{name: "Azure", channelType: constant.ChannelTypeAzure, apiType: constant.APITypeOpenAI, want: true},
		{name: "Codex", channelType: constant.ChannelTypeCodex, apiType: constant.APITypeCodex, want: true},
		{name: "Advanced Custom", channelType: constant.ChannelTypeAdvancedCustom, apiType: constant.APITypeAdvancedCustom, want: true},
		{name: "Sub2API", channelType: constant.ChannelTypeSub2API, apiType: constant.APITypeSub2API, want: true},
		{name: "New API", channelType: constant.ChannelTypeNewAPI, apiType: constant.APITypeNewAPI, want: true},
		{name: "Anthropic", channelType: constant.ChannelTypeAnthropic, apiType: constant.APITypeAnthropic, want: false},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			assert.Equal(t, test.want, common.SupportsResponsesCompact(test.channelType, test.apiType))
		})
	}
}

func TestMultiprotocolGatewayEndpointTypes(t *testing.T) {
	want := []constant.EndpointType{
		constant.EndpointTypeOpenAI,
		constant.EndpointTypeOpenAIResponse,
		constant.EndpointTypeOpenAIResponseCompact,
		constant.EndpointTypeAnthropic,
		constant.EndpointTypeGemini,
		constant.EndpointTypeOpenAIAlphaSearch,
	}

	assert.Equal(t, want, common.GetEndpointTypesByChannelType(constant.ChannelTypeNewAPI, "gpt-5"))
	assert.Equal(t, want, common.GetEndpointTypesByChannelType(constant.ChannelTypeSub2API, "gpt-5"))
}

func TestCopyChannelRejectsInvalidLegacyProxySettings(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	settingBytes, err := common.Marshal(dto.ChannelSettings{
		Proxy: "socks5://proxy.example/legacy-path",
	})
	require.NoError(t, err)
	setting := string(settingBytes)
	origin := &model.Channel{
		Type:    constant.ChannelTypeOpenAI,
		Name:    "legacy proxy channel",
		Key:     "test-key",
		Models:  "gpt-test",
		Group:   "default",
		Setting: &setting,
	}
	require.NoError(t, db.Create(origin).Error)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: fmt.Sprintf("%d", origin.Id)}}
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/channel/copy", nil)

	CopyChannel(ctx)

	assert.Contains(t, recorder.Body.String(), "invalid channel settings")
	var channelCount int64
	require.NoError(t, db.Model(&model.Channel{}).Count(&channelCount).Error)
	assert.Equal(t, int64(1), channelCount)
}

func TestDeleteChannelResetsProxyCacheWhenPreReadFails(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Log{}, &model.AuditLog{}))
	require.NoError(t, model.AutoMigrateUpstreamCredentialSchema(db))
	service.ResetProxyClientCache()
	t.Cleanup(service.ResetProxyClientCache)

	proxyURL := "http://proxy.example:8080"
	beforeDelete, err := service.GetHttpClientWithProxy(proxyURL)
	require.NoError(t, err)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Params = gin.Params{{Key: "id", Value: "999999"}}
	ctx.Request = httptest.NewRequest(http.MethodDelete, "/api/channel/999999", nil)

	DeleteChannel(ctx)

	assert.Contains(t, recorder.Body.String(), `"success":true`)
	afterDelete, err := service.GetHttpClientWithProxy(proxyURL)
	require.NoError(t, err)
	assert.NotSame(t, beforeDelete, afterDelete)
}

func TestDeleteChannelBatchReportsAndAuditsActualDeletedCount(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.Log{}, &model.AuditLog{}))
	require.NoError(t, model.AutoMigrateUpstreamCredentialSchema(db))
	channel := &model.Channel{Name: "existing", Key: "test-key"}
	require.NoError(t, db.Create(channel).Error)

	requestBody, err := common.Marshal(ChannelBatch{Ids: []int{channel.Id, 999999}})
	require.NoError(t, err)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodDelete, "/api/channel/batch", bytes.NewReader(requestBody))
	ctx.Request.Header.Set("Content-Type", "application/json")

	DeleteChannelBatch(ctx)

	var response struct {
		Success bool  `json:"success"`
		Data    int64 `json:"data"`
	}
	require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
	assert.True(t, response.Success)
	assert.Equal(t, int64(1), response.Data)

	var auditLog model.AuditLog
	require.NoError(t, db.Order("id desc").First(&auditLog).Error)
	var auditData struct {
		Operation struct {
			Params map[string]any `json:"params"`
		} `json:"op"`
	}
	encodedAudit, err := common.Marshal(auditLog.Other)
	require.NoError(t, err)
	require.NoError(t, common.Unmarshal(encodedAudit, &auditData))
	assert.Equal(t, float64(1), auditData.Operation.Params["count"])
}

func TestSettleTestQuotaUsesTieredBilling(t *testing.T) {
	info := &relaycommon.RelayInfo{
		TieredBillingSnapshot: &billingexpr.BillingSnapshot{
			BillingMode:   "tiered_expr",
			ExprString:    `param("stream") == true ? tier("stream", p * 3) : tier("base", p * 2)`,
			ExprHash:      billingexpr.ExprHashString(`param("stream") == true ? tier("stream", p * 3) : tier("base", p * 2)`),
			GroupRatio:    1,
			EstimatedTier: "stream",
			QuotaPerUnit:  common.QuotaPerUnit,
			ExprVersion:   1,
		},
		BillingRequestInput: &billingexpr.RequestInput{
			Body: []byte(`{"stream":true}`),
		},
	}

	quota, result := settleTestQuota(info, types.PriceData{
		ModelRatio:      1,
		CompletionRatio: 2,
	}, &dto.Usage{
		PromptTokens: 1000,
	})

	require.Equal(t, 1500, quota)
	require.NotNil(t, result)
	require.Equal(t, "stream", result.MatchedTier)
}

func TestBuildTestLogOtherInjectsTieredInfo(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())

	info := &relaycommon.RelayInfo{
		TieredBillingSnapshot: &billingexpr.BillingSnapshot{
			BillingMode: "tiered_expr",
			ExprString:  `tier("base", p * 2)`,
		},
		ChannelMeta: &relaycommon.ChannelMeta{},
	}
	priceData := types.PriceData{
		GroupRatioInfo: types.GroupRatioInfo{GroupRatio: 1},
	}
	usage := &dto.Usage{
		PromptTokensDetails: dto.InputTokenDetails{
			CachedTokens: 12,
		},
	}

	requestRules := []billingexpr.RequestRuleTrace{{
		Cond:       `param("service_tier") == "fast"`,
		Multiplier: 2,
		Matched:    true,
	}}
	other := buildTestLogOther(ctx, info, priceData, usage, &billingexpr.TieredResult{
		MatchedTier:  "base",
		RequestRules: requestRules,
	})

	fields := other.Snapshot()
	require.Equal(t, "tiered_expr", fields["billing_mode"])
	require.Equal(t, "base", fields["matched_tier"])
	require.Equal(t, requestRules, fields["request_rules"])
	require.NotEmpty(t, fields["expr_b64"])
}

func TestResolveChannelTestUserIDUsesRequestUser(t *testing.T) {
	gin.SetMode(gin.TestMode)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Set("id", 2)

	userID, err := resolveChannelTestUserID(ctx)

	require.NoError(t, err)
	require.Equal(t, 2, userID)
}

func TestSelectChannelsForAutomaticTestPassiveRecoveryOnlyUsesAutoDisabled(t *testing.T) {
	channels := []*model.Channel{
		{Id: 1, Status: common.ChannelStatusEnabled},
		{Id: 2, Status: common.ChannelStatusAutoDisabled},
		{Id: 3, Status: common.ChannelStatusManuallyDisabled},
	}

	selected := selectChannelsForAutomaticTest(channels, operation_setting.ChannelTestModePassiveRecovery)

	require.Len(t, selected, 1)
	require.Equal(t, 2, selected[0].Id)
}

func TestSelectChannelsForAutomaticTestScheduledSkipsManualDisabled(t *testing.T) {
	channels := []*model.Channel{
		{Id: 1, Status: common.ChannelStatusEnabled},
		{Id: 2, Status: common.ChannelStatusAutoDisabled},
		{Id: 3, Status: common.ChannelStatusManuallyDisabled},
	}

	selected := selectChannelsForAutomaticTest(channels, operation_setting.ChannelTestModeScheduledAll)

	require.Len(t, selected, 2)
	require.Equal(t, 1, selected[0].Id)
	require.Equal(t, 2, selected[1].Id)
}

func TestSelectChannelsForAutomaticTestAutoBanOnlyUsesEligibleChannels(t *testing.T) {
	autoBanEnabled := 1
	autoBanDisabled := 0
	channels := []*model.Channel{
		{Id: 1, Status: common.ChannelStatusEnabled, AutoBan: &autoBanEnabled},
		{Id: 2, Status: common.ChannelStatusEnabled, AutoBan: &autoBanDisabled},
		{Id: 3, Status: common.ChannelStatusAutoDisabled, AutoBan: &autoBanEnabled},
		{Id: 4, Status: common.ChannelStatusManuallyDisabled, AutoBan: &autoBanEnabled},
		{Id: 5, Status: common.ChannelStatusEnabled},
	}

	selected := selectChannelsForAutomaticTest(channels, operation_setting.ChannelTestModeAutoBanOnly)

	require.Len(t, selected, 2)
	require.Equal(t, 1, selected[0].Id)
	require.Equal(t, 3, selected[1].Id)
}

func TestRunChannelTestWorkersHonorsConfiguredConcurrency(t *testing.T) {
	originalInterval := common.RequestInterval
	common.RequestInterval = 0
	t.Cleanup(func() { common.RequestInterval = originalInterval })

	channels := []*model.Channel{
		{Id: 1, Status: common.ChannelStatusEnabled},
		{Id: 2, Status: common.ChannelStatusEnabled},
		{Id: 3, Status: common.ChannelStatusEnabled},
		{Id: 4, Status: common.ChannelStatusEnabled},
	}
	started := make(chan struct{}, len(channels))
	release := make(chan struct{})
	var active atomic.Int32
	var maxActive atomic.Int32
	progress := make([]int, 0, len(channels)+1)
	summaryResult := make(chan channelTestSummary, 1)

	go func() {
		summaryResult <- runChannelTestWorkers(
			context.Background(),
			channels,
			2,
			func(_ context.Context, _ *model.Channel) channelTestSummary {
				current := active.Add(1)
				defer active.Add(-1)
				for {
					observed := maxActive.Load()
					if current <= observed || maxActive.CompareAndSwap(observed, current) {
						break
					}
				}
				started <- struct{}{}
				<-release
				return channelTestSummary{Tested: 1, Succeeded: 1}
			},
			func(processed, _ int) {
				progress = append(progress, processed)
			},
		)
	}()

	<-started
	<-started
	select {
	case <-started:
		t.Fatal("started more channel tests than the configured concurrency")
	default:
	}
	close(release)

	summary := <-summaryResult

	assert.Equal(t, int32(2), maxActive.Load())
	assert.Equal(t, channelTestSummary{Tested: 4, Succeeded: 4}, summary)
	assert.Equal(t, []int{0, 1, 2, 3, 4}, progress)
}

func TestRunChannelTestWorkersStopsAfterCancellation(t *testing.T) {
	originalInterval := common.RequestInterval
	common.RequestInterval = 0
	t.Cleanup(func() { common.RequestInterval = originalInterval })

	ctx, cancel := context.WithCancel(context.Background())
	channels := []*model.Channel{
		{Id: 1, Status: common.ChannelStatusEnabled},
		{Id: 2, Status: common.ChannelStatusEnabled},
		{Id: 3, Status: common.ChannelStatusEnabled},
		{Id: 4, Status: common.ChannelStatusEnabled},
	}
	started := make(chan struct{}, len(channels))
	progress := make([]int, 0, 1)
	summaryResult := make(chan channelTestSummary, 1)

	go func() {
		summaryResult <- runChannelTestWorkers(
			ctx,
			channels,
			2,
			func(ctx context.Context, _ *model.Channel) channelTestSummary {
				started <- struct{}{}
				<-ctx.Done()
				return channelTestSummary{Tested: 1, Succeeded: 1}
			},
			func(processed, _ int) {
				progress = append(progress, processed)
			},
		)
	}()

	<-started
	<-started
	cancel()

	summary := <-summaryResult

	select {
	case <-started:
		t.Fatal("started another channel test after cancellation")
	default:
	}
	assert.Equal(t, channelTestSummary{Tested: 2, Succeeded: 2}, summary)
	assert.Equal(t, []int{0}, progress)
}

func TestTestAllChannelsRejectsExistingActiveTask(t *testing.T) {
	db := setupModelListControllerTestDB(t)
	require.NoError(t, db.AutoMigrate(&model.SystemTask{}, &model.SystemTaskLock{}))

	existing, err := model.CreateSystemTask(model.SystemTaskTypeChannelTest, nil, nil)
	require.NoError(t, err)

	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/api/channel/test", nil)

	TestAllChannels(ctx)

	require.Equal(t, http.StatusConflict, recorder.Code)
	require.Contains(t, recorder.Body.String(), existing.TaskID)
	require.Contains(t, recorder.Body.String(), "A channel test task is already running or queued")
}

func TestFundingRecoveryAndAdminOperations(t *testing.T) {
	db := modelManagementDB(t, "sqlite", "")
	require.NoError(t, db.AutoMigrate(&model.ChannelFundingIncident{}, &model.ChannelFundingDelivery{}, &model.SystemTask{}, &model.SystemTaskLock{}, &model.Log{}))
	previousFunding := operation_setting.GetChannelFundingSetting()
	t.Cleanup(func() {
		require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{"channel_funding_setting.enabled": fmt.Sprint(previousFunding.Enabled)}))
	})
	require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{"channel_funding_setting.enabled": "true"}))
	disable, enable := common.AutomaticDisableChannelEnabled, common.AutomaticEnableChannelEnabled
	common.AutomaticDisableChannelEnabled, common.AutomaticEnableChannelEnabled = true, true
	t.Cleanup(func() { common.AutomaticDisableChannelEnabled, common.AutomaticEnableChannelEnabled = disable, enable })
	service.InitHttpClient()
	var requests atomic.Int32
	var funded atomic.Bool
	funded.Store(true)
	var actualKey atomic.Value
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests.Add(1)
		actualKey.Store(r.Header.Get("Authorization"))
		assert.Equal(t, "/user/balance", r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		_, _ = fmt.Fprintf(w, `{"is_available":%t,"balance_infos":[{"currency":"USD","total_balance":"2"}]}`, funded.Load())
	}))
	t.Cleanup(server.Close)
	for _, tc := range []struct {
		name                          string
		autoEnable, manual, available bool
		source                        string
		wantState, wantProbe          string
		wantStatus                    int
	}{
		{"owned recovery", true, false, true, "http", "resolved", "recovered", 1},
		{"enable off", false, false, true, "http", "resolved", "recovered", 3},
		{"manual disable", true, true, true, "http", "confirmed", "paused", 2},
		{"still empty", true, false, false, "http", "confirmed", "failed", 3},
	} {
		t.Run(tc.name, func(t *testing.T) {
			common.AutomaticEnableChannelEnabled = tc.autoEnable
			funded.Store(tc.available)
			channel := model.Channel{Name: tc.name, Type: constant.ChannelTypeDeepSeek, Key: "probe-key", BaseURL: &server.URL, Status: 1, Models: "deepseek-chat", Group: "default"}
			require.NoError(t, channel.Insert())
			snapshot := channel.GetUpstreamCredentialSnapshot(0)
			identity := types.ChannelFundingAttempt{ChannelID: channel.Id, ChannelType: channel.Type, BindingID: snapshot.BindingID, VersionID: snapshot.CredentialVersionID, BindingActivatedAt: snapshot.BindingActivatedAt, Model: "deepseek-chat", Endpoint: "/v1/chat/completions", Source: tc.source, StartedAt: time.Now().Add(-time.Minute).UnixMilli()}
			event, err := model.RecordChannelFundingFailure(identity, "balance_insufficient", "test", "insufficient_balance", "balance", 402, true, 10)
			require.NoError(t, err)
			require.NoError(t, db.Model(event).Update("last_failure_at", time.Now().Add(-time.Second).UnixMilli()).Error)
			if tc.manual {
				require.True(t, model.UpdateChannelStatus(channel.Id, "", 2, "manual"))
				require.NoError(t, db.Model(event).Update("next_probe_at", common.GetTimestamp()).Error)
			}
			before := requests.Load()
			probeID := event.ID
			if tc.manual {
				probeID = 0 // Scheduled checks must respect the operator's stop.
			}
			_, err = runFundingProbes(context.Background(), probeID)
			require.NoError(t, err)
			require.NoError(t, db.First(event, event.ID).Error)
			assert.Equal(t, tc.wantState, event.State)
			assert.Equal(t, tc.wantProbe, event.ProbeStatus)
			stored, err := model.GetChannelById(channel.Id, true)
			require.NoError(t, err)
			assert.Equal(t, tc.wantStatus, stored.Status)
			if tc.manual {
				assert.Equal(t, before, requests.Load())
				_, err = runFundingProbes(context.Background(), event.ID)
				require.NoError(t, err)
				require.NoError(t, db.First(event, event.ID).Error)
				assert.Equal(t, "resolved", event.State)
				stored, err = model.GetChannelById(channel.Id, true)
				require.NoError(t, err)
				assert.Equal(t, common.ChannelStatusManuallyDisabled, stored.Status, "an explicit probe must not undo a manual stop")
			} else {
				assert.Equal(t, before+1, requests.Load())
				assert.Equal(t, "Bearer probe-key", actualKey.Load())
			}
			if !tc.available {
				assert.InDelta(t, time.Now().Add(20*time.Minute).Unix(), event.NextProbeAt, 2)
			}
		})
	}
	// A task-only event cannot trigger a generated image/video job.
	channel := model.Channel{Name: "task only", Type: constant.ChannelTypeTaskPlugin, Key: "task-key", Status: 1, Models: "video", Group: "default"}
	require.NoError(t, channel.Insert())
	snapshot := channel.GetUpstreamCredentialSnapshot(0)
	identity := types.ChannelFundingAttempt{ChannelID: channel.Id, ChannelType: channel.Type, BindingID: snapshot.BindingID, VersionID: snapshot.CredentialVersionID, BindingActivatedAt: snapshot.BindingActivatedAt, Model: "video", Endpoint: "/v1/videos", Source: "task_submit", StartedAt: time.Now().UnixMilli()}
	event, err := model.RecordChannelFundingFailure(identity, "balance_insufficient", "test", "insufficient_balance", "balance", 402, true, 10)
	require.NoError(t, err)
	before := requests.Load()
	_, err = runFundingProbes(context.Background(), event.ID)
	require.NoError(t, err)
	require.NoError(t, db.First(event, event.ID).Error)
	assert.Equal(t, "manual_required", event.ProbeStatus)
	assert.Zero(t, event.NextProbeAt)
	assert.Equal(t, before, requests.Load())
	// Explicit operations require matching event/channel/revision and never enable
	// a channel merely because its marker was dismissed.
	engine := gin.New()
	engine.Use(func(c *gin.Context) { c.Set("id", 99) })
	engine.GET("/api/channel/:id/funding", GetChannelFunding)
	engine.POST("/api/channel/:id/funding/dismiss", OperateChannelFunding)
	engine.POST("/api/channel/:id/funding/probe", OperateChannelFunding)
	for _, revision := range []int64{event.Revision + 1, event.Revision} {
		body, _ := common.Marshal(map[string]any{"incident_id": event.ID, "revision": revision, "reason": "reviewed"})
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/channel/%d/funding/dismiss", channel.Id), bytes.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		engine.ServeHTTP(recorder, request)
		if revision == event.Revision {
			assert.Equal(t, http.StatusOK, recorder.Code)
		} else {
			assert.Equal(t, http.StatusConflict, recorder.Code)
		}
	}
	stored, err := model.GetChannelById(channel.Id, true)
	require.NoError(t, err)
	assert.Equal(t, 3, stored.Status)
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/channel/%d/funding", channel.Id), nil))
	assert.Equal(t, http.StatusOK, recorder.Code)
	assert.NotContains(t, recorder.Body.String(), "task-key")
	assert.Contains(t, recorder.Body.String(), "dismissed")
}

func TestFundingTextProbeAndSeparateLogDatabase(t *testing.T) {
	for _, kind := range []string{"sqlite", "mysql", "postgres"} {
		t.Run(kind, func(t *testing.T) {
			dsn := os.Getenv("TEST_FUNDING_MYSQL_DSN")
			if kind == "postgres" {
				dsn = os.Getenv("TEST_FUNDING_POSTGRES_DSN")
			}
			if kind != "sqlite" && dsn == "" {
				t.Skip("funding database DSN is not configured")
			}
			db := modelManagementDB(t, kind, dsn)
			require.NoError(t, db.AutoMigrate(&model.ChannelFundingIncident{}, &model.ChannelFundingDelivery{}, &model.SystemTask{}, &model.SystemTaskLock{}, &model.Log{}))
			logDB, _ := newAuditTestDatabase(t, kind, dsn)
			require.NoError(t, logDB.AutoMigrate(&model.Log{}))
			model.LOG_DB = logDB
			common.SetLogDatabaseType(common.DatabaseType(kind))
			previousFunding := operation_setting.GetChannelFundingSetting()
			previousLogs, previousEnable := common.LogConsumeEnabled, common.AutomaticEnableChannelEnabled
			common.LogConsumeEnabled, common.AutomaticEnableChannelEnabled = true, true
			t.Cleanup(func() {
				common.LogConsumeEnabled, common.AutomaticEnableChannelEnabled = previousLogs, previousEnable
				require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{"channel_funding_setting.enabled": fmt.Sprint(previousFunding.Enabled)}))
			})
			require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{"channel_funding_setting.enabled": "true"}))
			require.NoError(t, ratio_setting.UpdateModelRatioByJSONString(`{"gpt-4o-mini":1}`))
			root := model.User{Username: "funding-probe-root", Role: common.RoleRootUser, Status: common.UserStatusEnabled, Group: "default", Quota: 100000, AffCode: "funding-probe"}
			require.NoError(t, db.Create(&root).Error)
			service.InitHttpClient()
			var response atomic.Value
			response.Store(`{"object":"chat.completion","choices":[{"message":{"role":"assistant","content":"unfinished"},"finish_reason":null}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`)
			var requests atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				requests.Add(1)
				assert.Equal(t, "Bearer funding-disabled-key", r.Header.Get("Authorization"))
				assert.Equal(t, "/v1/chat/completions", r.URL.Path)
				var request struct {
					Stream    bool `json:"stream"`
					MaxTokens int  `json:"max_tokens"`
				}
				assert.NoError(t, common.DecodeJson(r.Body, &request))
				assert.False(t, request.Stream)
				assert.Equal(t, 16, request.MaxTokens)
				w.Header().Set("Content-Type", "application/json")
				_, _ = fmt.Fprint(w, response.Load())
			}))
			t.Cleanup(server.Close)
			channel := model.Channel{Name: "funding text", Type: constant.ChannelTypeOpenAI, Key: "funding-disabled-key\nhealthy-key", BaseURL: &server.URL, Status: common.ChannelStatusEnabled, Models: "gpt-4o-mini", Group: "default", ChannelInfo: model.ChannelInfo{IsMultiKey: true, MultiKeySize: 2}}
			require.NoError(t, channel.Insert())
			snapshot := channel.GetUpstreamCredentialSnapshot(0)
			identity := types.ChannelFundingAttempt{ChannelID: channel.Id, ChannelType: channel.Type, BindingID: snapshot.BindingID, VersionID: snapshot.CredentialVersionID, BindingActivatedAt: snapshot.BindingActivatedAt, Model: "gpt-4o-mini", Endpoint: "/v1/chat/completions", Source: "http", StartedAt: time.Now().Add(-time.Minute).UnixMilli()}
			event, err := model.RecordChannelFundingFailure(identity, "balance_insufficient", "test", "insufficient_balance", "insufficient balance", 429, true, 10)
			require.NoError(t, err)
			require.NoError(t, db.Model(event).Update("last_failure_at", time.Now().Add(-time.Second).UnixMilli()).Error)
			_, err = runFundingProbes(context.Background(), event.ID)
			require.NoError(t, err)
			require.NoError(t, db.First(event, event.ID).Error)
			assert.Equal(t, "confirmed", event.State, "HTTP 200 with an unfinished completion must not recover")
			stored, err := model.GetChannelById(channel.Id, true)
			require.NoError(t, err)
			assert.Equal(t, common.ChannelStatusAutoDisabled, stored.ChannelInfo.MultiKeyStatusList[0])
			response.Store(`{"object":"chat.completion","choices":[{"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":1,"total_tokens":4}}`)
			_, err = runFundingProbes(context.Background(), event.ID)
			require.NoError(t, err)
			require.NoError(t, db.First(event, event.ID).Error)
			assert.Equal(t, "resolved", event.State)
			assert.EqualValues(t, 2, requests.Load())
			stored, err = model.GetChannelById(channel.Id, true)
			require.NoError(t, err)
			assert.NotEqual(t, common.ChannelStatusAutoDisabled, stored.ChannelInfo.MultiKeyStatusList[0])
			var logs []model.Log
			require.NoError(t, logDB.Find(&logs).Error)
			require.Len(t, logs, 2)
			assert.Contains(t, logs[0].Other, `"test_purpose":"channel_funding_recovery"`)
			assert.Contains(t, logs[0].Other, snapshot.BindingID)
			assert.NotContains(t, logs[0].Other, "funding-disabled-key")
			var mainLogs int64
			require.NoError(t, db.Model(&model.Log{}).Count(&mainLogs).Error)
			assert.Zero(t, mainLogs)
		})
	}
	for _, tc := range []struct {
		name   string
		format relaytypes.RelayFormat
		body   string
		want   bool
	}{
		{"Responses completed", relaytypes.RelayFormatOpenAIResponses, `{"object":"response","status":"completed"}`, true},
		{"Responses failed", relaytypes.RelayFormatOpenAIResponses, `{"object":"response","status":"failed"}`, false},
		{"Claude completed", relaytypes.RelayFormatClaude, `{"type":"message","content":[],"stop_reason":"end_turn"}`, true},
		{"Claude incomplete", relaytypes.RelayFormatClaude, `{"type":"message","content":[],"stop_reason":null}`, false},
		{"Gemini completed", relaytypes.RelayFormatGemini, `{"candidates":[{"finishReason":"STOP"}]}`, true},
		{"Gemini incomplete", relaytypes.RelayFormatGemini, `{"candidates":[{"content":{}}]}`, false},
		{"error envelope", relaytypes.RelayFormatOpenAIResponses, `{"object":"response","status":"completed","error":{"message":"insufficient balance"}}`, false},
	} {
		t.Run(tc.name, func(t *testing.T) { assert.Equal(t, tc.want, fundingProbeCompleted([]byte(tc.body), tc.format)) })
	}
}

func TestChannelFundingFiltersAndTagPagination(t *testing.T) {
	db := modelManagementDB(t, "sqlite", "")
	require.NoError(t, db.AutoMigrate(&model.ChannelFundingIncident{}, &model.ChannelFundingDelivery{}))
	for i, tag := range []string{"alpha", "alpha", "beta", "gamma"} {
		channel := model.Channel{Name: fmt.Sprintf("funding-list-%d", i), Type: 1, Key: "list-key", Status: 1, Models: "text-model", Group: "default", Tag: common.GetPointer(tag)}
		if i == 0 {
			channel.Key = "list-a\nlist-b\nlist-c"
			channel.ChannelInfo = model.ChannelInfo{IsMultiKey: true, MultiKeySize: 3}
		}
		require.NoError(t, channel.Insert())
		if i == 0 || i == 2 {
			snapshot := channel.GetUpstreamCredentialSnapshot(0)
			identity := types.ChannelFundingAttempt{ChannelID: channel.Id, ChannelType: 1, BindingID: snapshot.BindingID, VersionID: snapshot.CredentialVersionID, BindingActivatedAt: snapshot.BindingActivatedAt, Model: "text-model", Endpoint: "/v1/chat/completions", Source: "http", StartedAt: time.Now().UnixMilli()}
			_, err := model.RecordChannelFundingFailure(identity, "balance_insufficient", "test", "insufficient_balance", "insufficient balance", 429, false, 10)
			require.NoError(t, err)
		}
	}
	for _, handler := range []struct {
		name string
		fn   gin.HandlerFunc
	}{{"list", GetAllChannels}, {"search", SearchChannels}} {
		for _, tc := range []struct {
			name, query      string
			wantTotal, items int
			keys, affected   int
			tag              string
		}{
			{"tag all", "tag_mode=true&p=1&page_size=1", 3, 2, 4, 1, "alpha"},
			{"tag funded", "tag_mode=true&p=1&page_size=1&funding_status=balance_insufficient", 2, 1, 3, 1, "alpha"},
			{"tag funded next", "tag_mode=true&p=2&page_size=1&funding_status=balance_insufficient", 2, 1, 1, 1, "beta"},
			{"channels funded", "p=1&page_size=1&funding_status=balance_insufficient", 2, 1, 0, 0, ""},
			{"no marker", "p=1&page_size=20&funding_status=none", 2, 2, 0, 0, ""},
			{"different category", "p=1&page_size=20&funding_status=quota_limited", 0, 0, 0, 0, ""},
		} {
			t.Run(handler.name+"/"+tc.name, func(t *testing.T) {
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				c.Request = httptest.NewRequest(http.MethodGet, "/api/channel/?"+tc.query, nil)
				handler.fn(c)
				var response struct {
					Success bool `json:"success"`
					Data    struct {
						Items []*model.Channel                       `json:"items"`
						Total int                                    `json:"total"`
						Tags  map[string]model.ChannelFundingSummary `json:"funding_tags"`
					} `json:"data"`
				}
				require.NoError(t, common.Unmarshal(recorder.Body.Bytes(), &response))
				require.True(t, response.Success, recorder.Body.String())
				assert.Equal(t, tc.wantTotal, response.Data.Total)
				assert.Len(t, response.Data.Items, tc.items)
				assert.NotContains(t, recorder.Body.String(), "list-key")
				assert.NotContains(t, recorder.Body.String(), "list-a")
				if tc.tag != "" {
					assert.Equal(t, tc.keys, response.Data.Tags[tc.tag].TotalKeys)
					assert.Equal(t, tc.affected, response.Data.Tags[tc.tag].AffectedKeys)
				}
			})
		}
	}
}

func TestMidjourneyFundingPollKeepsCredentialAndTaskScope(t *testing.T) {
	db := modelManagementDB(t, "sqlite", "")
	require.NoError(t, db.AutoMigrate(&model.Midjourney{}, &model.ChannelFundingIncident{}, &model.ChannelFundingDelivery{}, &model.SystemTask{}, &model.SystemTaskLock{}))
	previous := operation_setting.GetChannelFundingSetting()
	previousDisable := common.AutomaticDisableChannelEnabled
	common.AutomaticDisableChannelEnabled = true
	require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{"channel_funding_setting.enabled": "true"}))
	t.Cleanup(func() {
		common.AutomaticDisableChannelEnabled = previousDisable
		require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{"channel_funding_setting.enabled": fmt.Sprint(previous.Enabled)}))
	})
	service.InitHttpClient()
	requests := make(chan string, 2)
	now := time.Now().UnixMilli()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		key := r.Header.Get("mj-api-secret")
		requests <- key
		var request struct {
			IDs []string `json:"ids"`
		}
		require.NoError(t, common.DecodeJson(r.Body, &request))
		assert.Equal(t, []string{"same-upstream-id"}, request.IDs)
		w.Header().Set("Content-Type", "application/json")
		if key == "mj-funding-key" {
			w.WriteHeader(http.StatusPaymentRequired)
			_, _ = fmt.Fprint(w, `{"error":{"code":"insufficient_balance","message":"insufficient balance"}}`)
			return
		}
		_, _ = fmt.Fprintf(w, `[{"id":"same-upstream-id","status":"IN_PROGRESS","progress":"50%%","submitTime":%d}]`, now)
	}))
	t.Cleanup(server.Close)
	channel := model.Channel{Name: "MJ key scopes", Type: constant.ChannelTypeMidjourney, BaseURL: &server.URL, Key: "mj-funding-key\nmj-healthy-key", Status: common.ChannelStatusEnabled, Models: "mj_imagine", Group: "default", ChannelInfo: model.ChannelInfo{IsMultiKey: true}}
	require.NoError(t, channel.Insert())
	for index, key := range channel.GetKeys() {
		snapshot := channel.GetUpstreamCredentialSnapshot(index)
		identity := types.ChannelFundingAttempt{ChannelID: channel.Id, ChannelType: channel.Type, BindingID: snapshot.BindingID, VersionID: snapshot.CredentialVersionID, BindingActivatedAt: snapshot.BindingActivatedAt, KeyIndex: index, StartedAt: now, Source: "task_submit"}
		encoded, err := common.Marshal(identity)
		require.NoError(t, err)
		task := model.Midjourney{MjId: "same-upstream-id", ChannelId: channel.Id, PollingKey: key, FundingAttemptData: string(encoded), SubmitTime: now, Status: "IN_PROGRESS", Progress: "10%", Code: 1}
		require.NoError(t, db.Create(&task).Error)
		public, err := common.Marshal(task)
		require.NoError(t, err)
		assert.NotContains(t, string(public), key)
	}
	summary := runMidjourneyTaskUpdateOnce(context.Background(), nil)
	assert.Equal(t, 2, summary.UnfinishedTasks)
	assert.ElementsMatch(t, []string{"mj-funding-key", "mj-healthy-key"}, []string{<-requests, <-requests})
	var tasks []model.Midjourney
	require.NoError(t, db.Order("id").Find(&tasks).Error)
	require.Len(t, tasks, 2)
	assert.Equal(t, "10%", tasks[0].Progress)
	assert.Equal(t, "50%", tasks[1].Progress)
	assert.Equal(t, "IN_PROGRESS", tasks[0].Status)
	assert.Equal(t, "IN_PROGRESS", tasks[1].Status)
	var incidents []model.ChannelFundingIncident
	require.NoError(t, db.Where("channel_id = ?", channel.Id).Find(&incidents).Error)
	require.Len(t, incidents, 1)
	assert.Equal(t, channel.GetUpstreamCredentialSnapshot(0).BindingID, incidents[0].BindingID)
}
