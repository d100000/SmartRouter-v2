package controller

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/billing_setting"

	"github.com/gin-gonic/gin"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestProcessChannelErrorUsesSnapshotWithoutLeakingChannelMetadata(t *testing.T) {
	gin.SetMode(gin.TestMode)
	previousDB, previousLogDB := model.DB, model.LOG_DB
	previousRedisEnabled := common.RedisEnabled
	previousMainDatabaseType := common.MainDatabaseType()
	previousLogDatabaseType := common.LogDatabaseType()
	previousErrorLogEnabled := constant.ErrorLogEnabled

	database, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	require.NoError(t, err)
	sqlDB, err := database.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	require.NoError(t, database.AutoMigrate(&model.User{}, &model.Log{}))
	model.DB, model.LOG_DB = database, database
	common.RedisEnabled = false
	common.SetDatabaseTypes(common.DatabaseTypeSQLite, common.DatabaseTypeSQLite)
	constant.ErrorLogEnabled = true
	t.Cleanup(func() {
		model.DB, model.LOG_DB = previousDB, previousLogDB
		common.RedisEnabled = previousRedisEnabled
		common.SetDatabaseTypes(previousMainDatabaseType, previousLogDatabaseType)
		constant.ErrorLogEnabled = previousErrorLogEnabled
		require.NoError(t, sqlDB.Close())
	})

	require.NoError(t, database.Create(&model.User{Id: 7, Username: "log-owner", Group: "default"}).Error)
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ctx.Set("id", 7)
	ctx.Set("username", "log-owner")
	ctx.Set("token_name", "test-token")
	ctx.Set("token_id", 11)
	ctx.Set("original_model", "gpt-test")
	ctx.Set("group", "default")
	ctx.Set("channel_id", 202)
	ctx.Set("channel_name", "mutable-context-channel")
	ctx.Set("channel_type", 9)
	ctx.Set("use_channel", []string{"101"})
	common.SetContextKey(ctx, constant.ContextKeyRequestStartTime, time.Now().Add(-time.Second))

	channelSnapshot := types.ChannelError{
		ChannelId:   101,
		ChannelType: 1,
		ChannelName: "snapshot-channel",
		AutoBan:     false,
	}
	apiErr := types.NewOpenAIError(errors.New("upstream failed"), types.ErrorCodeBadResponseStatusCode, http.StatusBadGateway)

	processChannelError(ctx, channelSnapshot, apiErr, nil)

	var stored model.Log
	require.NoError(t, database.First(&stored).Error)
	assert.Equal(t, channelSnapshot.ChannelId, stored.ChannelId)
	storedOther, err := common.StrToMap(stored.Other)
	require.NoError(t, err)
	assert.Equal(t, float64(http.StatusBadGateway), storedOther["status_code"])
	for _, key := range []string{"channel_id", "channel_name", "channel_type"} {
		assert.NotContains(t, storedOther, key)
	}
	adminInfo, ok := storedOther["admin_info"].(map[string]any)
	require.True(t, ok)
	assert.Equal(t, []any{"101"}, adminInfo["use_channel"])

	logs, total, err := model.GetUserLogs(7, model.LogFilter{Type: model.LogTypeError}, 0, 10)
	require.NoError(t, err)
	require.Equal(t, int64(1), total)
	require.Len(t, logs, 1)
	assert.Equal(t, channelSnapshot.ChannelId, logs[0].ChannelId)
	assert.Empty(t, logs[0].ChannelName)
	userOther, err := common.StrToMap(logs[0].Other)
	require.NoError(t, err)
	assert.NotContains(t, userOther, "admin_info")
	for _, key := range []string{"channel_id", "channel_name", "channel_type"} {
		assert.NotContains(t, userOther, key)
	}
}

// Exercise the real compatible adaptor, pricing snapshot, wallet/token billing
// session and log writers. The existing isolated dialect fixture also allows
// the same regression to run against real MySQL and PostgreSQL databases.
func TestChatStreamFailureBillingAndLogs(t *testing.T) {
	gin.SetMode(gin.TestMode)
	require.NoError(t, i18n.Init())
	db, dialect := openTaskDialectDatabase(t, &model.User{}, &model.Token{}, &model.Channel{}, &model.Log{})
	previousDB, previousLogDB := model.DB, model.LOG_DB
	previousMain, previousLog := common.MainDatabaseType(), common.LogDatabaseType()
	previousRedis, previousMemory, previousBatch := common.RedisEnabled, common.MemoryCacheEnabled, common.BatchUpdateEnabled
	previousConsume, previousExport, previousError := common.LogConsumeEnabled, common.DataExportEnabled, constant.ErrorLogEnabled
	previousQuotaUnit, previousTimeout := common.QuotaPerUnit, constant.StreamingTimeout
	model.DB, model.LOG_DB = db, db
	common.SetDatabaseTypes(dialect, dialect)
	common.RedisEnabled, common.MemoryCacheEnabled, common.BatchUpdateEnabled = false, false, false
	common.LogConsumeEnabled, common.DataExportEnabled, constant.ErrorLogEnabled = true, false, false
	common.QuotaPerUnit, constant.StreamingTimeout = 500000, 30
	t.Cleanup(func() {
		model.DB, model.LOG_DB = previousDB, previousLogDB
		common.SetDatabaseTypes(previousMain, previousLog)
		common.RedisEnabled, common.MemoryCacheEnabled, common.BatchUpdateEnabled = previousRedis, previousMemory, previousBatch
		common.LogConsumeEnabled, common.DataExportEnabled, constant.ErrorLogEnabled = previousConsume, previousExport, previousError
		common.QuotaPerUnit, constant.StreamingTimeout = previousQuotaUnit, previousTimeout
	})
	service.InitHttpClient()
	client := service.GetHttpClient()
	previousTransport := client.Transport
	t.Cleanup(func() { client.Transport = previousTransport })

	const modelName = "manual-stream-price"
	const expression = `tier("manual", p * 26 + c * 136)`
	historicalLog := model.Log{RequestId: "historical-empty-stream", Type: model.LogTypeConsume, ModelName: modelName, PromptTokens: 1325020, Quota: 17225260,
		Other: `{"admin_info":{"usage_billing_path":"local"},"stream_status":{"end_reason":"done","status":"ok"}}`}
	require.NoError(t, db.Create(&historicalLog).Error)
	for index, tc := range []struct {
		name       string
		body       string
		expression string
		provider   int
		trusted    bool
		noCharge   bool
		failed     bool
		wantQuota  int
	}{
		{name: "empty stream with trusted wallet", body: "data: [DONE]\n\n", trusted: true, noCharge: true, failed: true},
		{name: "upstream error refunds reservation", body: "data: {\"error\":{\"message\":\"context too long\",\"code\":\"context_length_exceeded\"}}\n\ndata: [DONE]\n\n", noCharge: true, failed: true},
		{name: "fixed price empty stream is not charged", body: "data: [DONE]\n\n", expression: `tier("manual", fixed(0.25))`, noCharge: true, failed: true},
		{name: "upstream input usage without output remains billable", body: "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":0,\"total_tokens\":5}}\n\ndata: [DONE]\n\n", wantQuota: 65},
		{name: "valid output preserves manual fixed price", body: "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", expression: `tier("manual", fixed(0.25))`, wantQuota: 125000},
		{name: "failed stream with real usage preserves charge and error marker", body: "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":0,\"total_tokens\":5}}\n\ndata: {\"error\":{\"message\":\"upstream stopped\",\"type\":\"server_error\"}}\n\n", failed: true, wantQuota: 65},
		{name: "native Claude empty EOF refunds reservation", provider: constant.ChannelTypeAnthropic, noCharge: true, failed: true},
		{name: "native Claude empty message stop is not billed", provider: constant.ChannelTypeAnthropic, body: "data: {\"type\":\"message_stop\"}\n\n", noCharge: true, failed: true},
		{name: "native Claude stream error is marked uncharged", provider: constant.ChannelTypeAnthropic, body: "data: {\"type\":\"error\",\"error\":{\"type\":\"invalid_request_error\",\"message\":\"context too long\"}}\n\n", noCharge: true, failed: true},
		{name: "native Claude genuine input usage remains billable", provider: constant.ChannelTypeAnthropic, body: "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_test\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":5,\"output_tokens\":0}}}\n\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":0}}\n\ndata: {\"type\":\"message_stop\"}\n\n", wantQuota: 65},
		{name: "native Claude start only is uncharged", provider: constant.ChannelTypeAnthropic, body: "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_empty\",\"role\":\"assistant\",\"content\":[]}}\n\n", noCharge: true, failed: true},
		{name: "native Claude cache only usage preserves pricing", provider: constant.ChannelTypeAnthropic, body: "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_cached\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":0,\"output_tokens\":0,\"cache_read_input_tokens\":5}}}\n\ndata: {\"type\":\"message_stop\"}\n\n", wantQuota: 65},
		{name: "native Claude partial text failure keeps price and marker", provider: constant.ChannelTypeAnthropic, body: "data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hello\"}}\n\n", expression: `tier("manual", fixed(0.25))`, failed: true, wantQuota: 125000},
		{name: "native Gemini empty EOF refunds reservation", provider: constant.ChannelTypeGemini, noCharge: true, failed: true},
		{name: "native Gemini finish only is not billed", provider: constant.ChannelTypeGemini, body: "data: {\"candidates\":[{\"content\":{\"parts\":[]},\"finishReason\":\"STOP\"}]}\n\n", noCharge: true, failed: true},
		{name: "native Gemini error is marked uncharged", provider: constant.ChannelTypeGemini, body: "data: {\"error\":{\"code\":400,\"message\":\"context too long\",\"status\":\"INVALID_ARGUMENT\"}}\n\n", noCharge: true, failed: true},
		{name: "native Gemini genuine input usage remains billable", provider: constant.ChannelTypeGemini, body: "data: {\"candidates\":[{\"content\":{\"parts\":[]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":5,\"candidatesTokenCount\":0,\"totalTokenCount\":5}}\n\n", wantQuota: 65},
		{name: "native Gemini zero usage is not actual usage", provider: constant.ChannelTypeGemini, body: "data: {\"candidates\":[{\"content\":{\"parts\":[]},\"finishReason\":\"STOP\"}],\"usageMetadata\":{\"promptTokenCount\":0,\"candidatesTokenCount\":0,\"totalTokenCount\":0}}\n\n", noCharge: true, failed: true},
		{name: "native Gemini partial text failure keeps price and marker", provider: constant.ChannelTypeGemini, body: "data: {\"candidates\":[{\"content\":{\"parts\":[{\"text\":\"hello\"}]}}]}\n\n", expression: `tier("manual", fixed(0.25))`, failed: true, wantQuota: 125000},
		{name: "native Gemini generated image preserves fixed price", provider: constant.ChannelTypeGemini, body: "data: {\"candidates\":[{\"content\":{\"parts\":[{\"inlineData\":{\"mimeType\":\"image/png\",\"data\":\"iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mP8/x8AAwMCAO+jP1kAAAAASUVORK5CYII=\"}}]},\"finishReason\":\"STOP\"}]}\n\n", expression: `tier("manual", fixed(0.25))`, wantQuota: 125000},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pricing := tc.expression
			if pricing == "" {
				pricing = expression
			}
			withTieredBillingConfig(t, map[string]string{modelName: "tiered_expr"}, map[string]string{modelName: pricing})
			const initialQuota = 50000000
			user := model.User{Username: fmt.Sprintf("stream_user_%d", index), AffCode: fmt.Sprintf("stream_aff_%d", index), Group: "default", Quota: initialQuota}
			require.NoError(t, db.Create(&user).Error)
			token := model.Token{UserId: user.Id, Key: fmt.Sprintf("stream_token_%d", index), Name: "stream regression", RemainQuota: initialQuota}
			require.NoError(t, db.Create(&token).Error)
			provider := tc.provider
			if provider == 0 {
				provider = constant.ChannelTypeOpenAI
			}
			channel := model.Channel{Name: "stream regression", Type: provider, Status: common.ChannelStatusEnabled}
			require.NoError(t, db.Create(&channel).Error)
			client.Transport = schedulingTokenTransport(func(request *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(tc.body)), Request: request}, nil
			})
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"manual-stream-price","stream":true}`))
			ctx.Set(common.RequestIdKey, fmt.Sprintf("stream-billing-%d", index))
			ctx.Set("id", user.Id)
			ctx.Set("username", user.Username)
			ctx.Set("token_name", token.Name)
			ctx.Set("token_id", token.Id)
			ctx.Set("token_quota", initialQuota)
			ctx.Set("group", "default")
			ctx.Set("use_channel", []string{fmt.Sprint(channel.Id)})
			common.SetContextKey(ctx, constant.ContextKeyOriginalModel, modelName)
			common.SetContextKey(ctx, constant.ContextKeyChannelId, channel.Id)
			common.SetContextKey(ctx, constant.ContextKeyChannelType, channel.Type)
			common.SetContextKey(ctx, constant.ContextKeyChannelBaseUrl, "https://stream-provider.invalid")
			common.SetContextKey(ctx, constant.ContextKeyChannelKey, "regression-upstream-key")
			common.SetContextKey(ctx, constant.ContextKeyRequestStartTime, time.Now())
			common.SetContextKey(ctx, constant.ContextKeyUserSetting, dto.UserSetting{Language: "en"})
			info := &relaycommon.RelayInfo{UserId: user.Id, TokenId: token.Id, TokenKey: token.Key, UserGroup: "default", UsingGroup: "default", OriginModelName: modelName,
				RelayMode: relayconstant.RelayModeChatCompletions, RelayFormat: types.RelayFormatOpenAI, IsStream: true, DisablePing: true, StartTime: time.Now(), FirstResponseTime: time.Now(), ForcePreConsume: !tc.trusted,
				UserSetting: dto.UserSetting{BillingPreference: "wallet_only"}, Request: &dto.GeneralOpenAIRequest{Model: modelName, Stream: common.GetPointer(true), Messages: []dto.Message{{Role: "user", Content: "test"}}}}
			info.InitChannelMeta(ctx)
			info.SetEstimatePromptTokens(1325020)
			_, priceErr := helper.ModelPriceHelper(ctx, info, 1325020, &types.TokenCountMeta{})
			require.NoError(t, priceErr)
			require.Nil(t, service.PreConsumeBilling(ctx, info.PriceData.QuotaToPreConsume, info))
			if tc.trusted {
				assert.Zero(t, info.Billing.GetPreConsumedQuota())
			} else {
				assert.Positive(t, info.Billing.GetPreConsumedQuota())
			}
			apiErr := relay.TextHelper(ctx, info)
			if tc.noCharge {
				require.NotNil(t, apiErr)
				assert.True(t, types.IsSkipRetryError(apiErr))
				processChannelError(ctx, *types.NewChannelError(channel.Id, channel.Type, channel.Name, false, "", false), apiErr, info)
				refunded := make(chan struct{}, 1)
				callback := fmt.Sprintf("test:stream-refund-%d", index)
				require.NoError(t, db.Callback().Update().After("gorm:commit_or_rollback_transaction").Register(callback, func(tx *gorm.DB) {
					if _, ok := tx.Statement.Model.(*model.Token); ok && tx.Error == nil {
						select {
						case refunded <- struct{}{}:
						default:
						}
					}
				}))
				relay.RefundFailedRequestBilling(ctx, info, apiErr)
				if !tc.trusted {
					select {
					case <-refunded:
					case <-time.After(5 * time.Second):
						t.Fatal("wallet/token refund did not complete")
					}
				}
				require.NoError(t, db.Callback().Update().Remove(callback))
			} else {
				require.Nil(t, apiErr)
			}

			var updatedUser model.User
			var updatedToken model.Token
			require.NoError(t, db.First(&updatedUser, user.Id).Error)
			require.NoError(t, db.First(&updatedToken, token.Id).Error)
			assert.Equal(t, initialQuota-tc.wantQuota, updatedUser.Quota)
			assert.Equal(t, tc.wantQuota, updatedUser.UsedQuota)
			assert.Equal(t, initialQuota-tc.wantQuota, updatedToken.RemainQuota)
			assert.Equal(t, tc.wantQuota, updatedToken.UsedQuota)
			storedExpression, configured := billing_setting.GetBillingExpr(modelName)
			assert.True(t, configured)
			assert.Equal(t, pricing, storedExpression, "manual pricing must stay unchanged")
			var logs []model.Log
			require.NoError(t, db.Where("request_id = ?", ctx.GetString(common.RequestIdKey)).Find(&logs).Error)
			require.Len(t, logs, 1)
			assert.Equal(t, tc.wantQuota, logs[0].Quota)
			if tc.noCharge {
				assert.Equal(t, model.LogTypeError, logs[0].Type)
				assert.Zero(t, logs[0].PromptTokens)
			} else {
				assert.Equal(t, model.LogTypeConsume, logs[0].Type)
			}
			var other map[string]any
			require.NoError(t, common.UnmarshalJsonStr(logs[0].Other, &other))
			if tc.failed {
				failure, ok := other["stream_failure"].(map[string]any)
				require.True(t, ok)
				assert.Equal(t, tc.noCharge, failure["no_charge"])
				assert.NotEmpty(t, failure["message"])
				assert.NotContains(t, logs[0].Other, "regression-upstream-key")
			} else {
				assert.NotContains(t, other, "stream_failure")
			}
		})
	}
	var unchangedHistoricalLog model.Log
	require.NoError(t, db.First(&unchangedHistoricalLog, historicalLog.Id).Error)
	assert.Equal(t, historicalLog, unchangedHistoricalLog, "historical charges and diagnostics must not be rewritten")
}
