package controller

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"testing/synctest"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/middleware"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relay"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/billing_setting"
	"github.com/QuantumNous/new-api/setting/config"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/bytedance/gopkg/util/gopool"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

const retryCreatedEvent = `{"type":"response.created","response":{"id":"failed-response","status":"in_progress","output":[],"usage":null}}`
const retryOverloadEvent = `{"type":"response.failed","response":{"id":"failed-response","status":"failed","error":{"code":"server_is_overloaded","type":"service_unavailable_error","message":"Upstream temporarily overloaded"},"output":[],"usage":null}}`
const retryCompletedEvent = `{"type":"response.completed","response":{"id":"successful-response","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}],"usage":{"input_tokens":7,"output_tokens":3,"total_tokens":10}}}`

func TestResponsesOverloadKeepsRetryBoundary(t *testing.T) {
	previousTimeout := constant.StreamingTimeout
	constant.StreamingTimeout = 30
	t.Cleanup(func() { constant.StreamingTimeout = previousTimeout })
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-test","input":"hello","stream":true}`))
	var settings dto.ChannelSettings
	require.NoError(t, common.UnmarshalJsonStr(`{"responses_overload_retry_enabled":true}`, &settings))
	common.SetContextKey(c, constant.ContextKeyChannelSetting, settings)
	common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenAI)
	common.SetContextKey(c, constant.ContextKeyChannelBaseUrl, "https://provider.invalid")
	req := &dto.OpenAIResponsesRequest{Model: "gpt-test", Input: []byte(`"hello"`), Stream: common.GetPointer(true)}
	info := &relaycommon.RelayInfo{Request: req, OriginModelName: req.Model, IsStream: true, DisablePing: true,
		RelayMode: relayconstant.RelayModeResponses, RelayFormat: types.RelayFormatOpenAIResponses, StartTime: time.Now()}
	adaptor, _, closer, apiErr := relay.PrepareResponsesRequest(c, info, req)
	require.Nil(t, apiErr)
	defer closer.Close()
	response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}, "X-Codex-Turn-State": {"failed-turn"}},
		Body: io.NopCloser(strings.NewReader("data: " + retryCreatedEvent + "\n\ndata: " + retryOverloadEvent + "\n\n"))}
	_, apiErr = adaptor.DoResponse(c, response, info)
	require.NotNil(t, apiErr, "explicit overload must return to the retry controller before settlement")
	assert.False(t, c.Writer.Written(), "neither initialization nor error events may reach the client")
	assert.Empty(t, recorder.Body.String())
	assert.Empty(t, recorder.Header().Get("X-Codex-Turn-State"))
	require.NotNil(t, info.StreamFailure)
	assert.True(t, info.StreamFailure.NoCharge)
}

func TestResponsesRetryStartupBoundaries(t *testing.T) {
	require.NoError(t, i18n.Init())
	settings := operation_setting.GetGeneralSetting()
	previousEnabled, previousInterval, previousTimeout := settings.PingIntervalEnabled, settings.PingIntervalSeconds, constant.StreamingTimeout
	settings.PingIntervalEnabled, settings.PingIntervalSeconds, constant.StreamingTimeout = true, 1, 30
	t.Cleanup(func() {
		settings.PingIntervalEnabled, settings.PingIntervalSeconds, constant.StreamingTimeout = previousEnabled, previousInterval, previousTimeout
	})
	for _, tc := range []struct {
		name, first, event              string
		delay                           time.Duration
		cancel, committed, retry, usage bool
	}{
		{name: "heartbeats cannot commit buffered initialization", delay: 2 * time.Second, retry: true},
		{name: "buffer expiry commits initialization and prevents retry", delay: 11 * time.Second, committed: true},
		{name: "client cancellation closes upstream without retry", delay: 2 * time.Second, cancel: true},
		{name: "already committed response cannot retry", committed: true},
		{name: "buffer size limit commits rather than drops metadata", first: strings.Replace(retryCreatedEvent, "failed-response", strings.Repeat("x", 65<<10), 1), committed: true},
		{name: "malformed event closes window", first: "invalid JSON", committed: true},
		{name: "completed state cannot masquerade as initialization", first: strings.Replace(retryCreatedEvent, "in_progress", "completed", 1), committed: true},
		{name: "unknown SSE name closes the retry window", event: "provider.unknown", committed: true},
		{name: "cache creation alias is actual usage", first: strings.Replace(retryCreatedEvent, `"usage":null`, `"usage":{"input_tokens_details":{"cache_creation_input_tokens":5}}`, 1), usage: true, committed: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				requestContext, cancel := context.WithCancel(context.Background())
				defer cancel()
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(requestContext)
				common.SetContextKey(c, constant.ContextKeyChannelSetting, dto.ChannelSettings{ResponsesOverloadRetryEnabled: true})
				common.SetContextKey(c, constant.ContextKeyChannelType, constant.ChannelTypeOpenAI)
				req := &dto.OpenAIResponsesRequest{Model: "gpt-test", Input: []byte(`"hello"`), Stream: common.GetPointer(true)}
				info := &relaycommon.RelayInfo{Request: req, OriginModelName: req.Model, IsStream: true,
					RelayMode: relayconstant.RelayModeResponses, RelayFormat: types.RelayFormatOpenAIResponses, StartTime: time.Now()}
				adaptor, _, closer, apiErr := relay.PrepareResponsesRequest(c, info, req)
				require.Nil(t, apiErr)
				defer closer.Close()
				if tc.name == "already committed response cannot retry" {
					_, err := c.Writer.Write([]byte(": PING\n\n"))
					require.NoError(t, err)
				}
				reader, writer := io.Pipe()
				defer reader.Close()
				go func() {
					defer writer.Close()
					first := tc.first
					if first == "" {
						first = retryCreatedEvent
					}
					if tc.event != "" {
						if _, err := fmt.Fprintf(writer, "event: %s\n", tc.event); err != nil {
							return
						}
					}
					if _, err := fmt.Fprintf(writer, "data: %s\n\n", first); err != nil {
						return
					}
					if tc.delay > 0 {
						<-time.After(tc.delay)
					}
					if tc.cancel {
						cancel()
						return
					}
					_, _ = fmt.Fprintf(writer, "data: %s\n\n", retryOverloadEvent)
				}()
				response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: reader}
				_, apiErr = adaptor.DoResponse(c, response, info)
				if tc.usage {
					require.Nil(t, apiErr)
				} else {
					require.NotNil(t, apiErr)
				}
				require.NotNil(t, info.StreamFailure)
				assert.Equal(t, tc.retry, info.StreamFailure.RetryableOverload)
				assert.Equal(t, tc.usage, info.StreamFailure.HasActualUsage)
				assert.Equal(t, !tc.usage, info.StreamFailure.NoCharge)
				assert.Equal(t, tc.committed, c.Writer.Written())
				if tc.retry {
					assert.Empty(t, recorder.Body.String())
				}
				_, err := reader.Read(make([]byte, 1))
				assert.ErrorIs(t, err, io.ErrClosedPipe, "the previous upstream must be closed before returning for retry")
			})
		})
	}
}

func TestResponsesRetryPreservesPolicyAndCancellation(t *testing.T) {
	for _, tc := range []struct {
		name, policyReason, reason string
		retried, cancel            bool
		delay                      time.Duration
	}{
		{name: "strict session", policyReason: "strict_session", reason: "strict_session"},
		{name: "pinned channel", policyReason: "pinned_channel", reason: "pinned_channel"},
		{name: "global policy", policyReason: "status_not_retryable", reason: "status_not_retryable"},
		{name: "second attempt exhausted", retried: true, reason: "stream_retry_limit"},
		{name: "Retry-After cannot be shortened", delay: time.Minute, reason: "stream_retry_deadline"},
		{name: "client cancels during backoff", delay: time.Second, cancel: true, reason: "client_cancelled"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(ctx)
				apiErr := types.NewOpenAIError(errors.New("overloaded"), "server_is_overloaded", http.StatusServiceUnavailable)
				info := &relaycommon.RelayInfo{StartTime: time.Now(), ChannelMeta: &relaycommon.ChannelMeta{}, StreamFailure: &relaycommon.StreamFailureInfo{Error: apiErr, NoCharge: true, RetryableOverload: true, RetryAfter: tc.delay}}
				if tc.retried {
					info.StreamRetryCount = 1
				}
				decision := service.PolicyDecision{Action: "retry", Reason: "retry_status_matched", Source: "global"}
				if tc.policyReason != "" {
					decision = service.PolicyDecision{Action: "stop", Reason: tc.policyReason, Source: "global"}
				}
				if tc.cancel {
					go func() { <-time.After(time.Millisecond); cancel() }()
				}
				result := service.PrepareStreamOverloadRetry(c, info, apiErr, decision)
				assert.Equal(t, "stop", result.Action)
				assert.Equal(t, tc.reason, result.Reason)
				require.Len(t, info.StreamRetryHistory, 1)
				assert.Equal(t, tc.reason, info.StreamRetryHistory[0].Reason)
			})
		})
	}
}

func TestResponsesSafeRetryBillingDatabaseMatrix(t *testing.T) {
	require.NoError(t, i18n.Init())
	for _, dialect := range []struct{ name, env string }{{"sqlite", ""}, {"mysql", "TEST_MYSQL_DSN"}, {"postgres", "TEST_POSTGRES_DSN"}} {
		t.Run(dialect.name, func(t *testing.T) {
			if dialect.env != "" && os.Getenv(dialect.env) == "" {
				t.Skip(dialect.env + " not configured")
			}
			db := modelManagementDB(t, dialect.name, os.Getenv(dialect.env))
			require.NoError(t, db.AutoMigrate(&model.Token{}, &model.Log{}, &model.OverviewAttemptEvent{}))
			logDB, _ := newAuditTestDatabase(t, dialect.name, os.Getenv(dialect.env))
			require.NoError(t, logDB.AutoMigrate(&model.Log{}))
			logSQL, err := logDB.DB()
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, logSQL.Close()) })
			model.LOG_DB = logDB
			disableRelayStreamTestMetrics(t)
			previousRetries, previousDisable, previousBatch := common.RetryTimes, common.AutomaticDisableChannelEnabled, common.BatchUpdateEnabled
			previousLogs, previousExport, previousQuota, previousTimeout := common.LogConsumeEnabled, common.DataExportEnabled, common.QuotaPerUnit, constant.StreamingTimeout
			previousErrorLogs, previousTrustQuota := constant.ErrorLogEnabled, operation_setting.GetQuotaSetting().TrustQuotaUSD
			constant.ErrorLogEnabled, operation_setting.GetQuotaSetting().TrustQuotaUSD = false, 10
			common.RetryTimes, common.AutomaticDisableChannelEnabled, common.BatchUpdateEnabled = 4, false, false
			common.LogConsumeEnabled, common.DataExportEnabled, common.QuotaPerUnit, constant.StreamingTimeout = true, false, 500000, 30
			t.Cleanup(func() {
				common.RetryTimes, common.AutomaticDisableChannelEnabled, common.BatchUpdateEnabled = previousRetries, previousDisable, previousBatch
				common.LogConsumeEnabled, common.DataExportEnabled, common.QuotaPerUnit, constant.StreamingTimeout = previousLogs, previousExport, previousQuota, previousTimeout
				constant.ErrorLogEnabled, operation_setting.GetQuotaSetting().TrustQuotaUSD = previousErrorLogs, previousTrustQuota
			})
			require.NoError(t, config.GlobalConfig.LoadFromDB(map[string]string{"group_ratio_setting.group_ratio": `{"default":1}`}))
			service.InitHttpClient()
			client := service.GetHttpClient()
			previousTransport := client.Transport
			t.Cleanup(func() { client.Transport = previousTransport })
			t.Cleanup(func() {
				// Wallet writes finish before their asynchronous cache updates.
				// Join both before restoring the shared database/cache fixture.
				require.Eventually(t, func() bool { return gopool.WorkerCount() == 0 }, 5*time.Second, time.Millisecond)
			})
			historical := model.Log{RequestId: "preserved-history", Type: model.LogTypeConsume, Quota: 17225260, PromptTokens: 1325020, Other: `{"manual":"keep"}`}
			require.NoError(t, logDB.Create(&historical).Error)
			for index, tc := range []struct {
				name, prefix, failure, extra, override, retryAfter, expression                    string
				disabled, failAgain, httpFailure, noGlobalRetry, passthrough, trusted, noFallback bool
				wantAttempts, wantQuota                                                           int
			}{
				{name: "overload then success settles once", wantAttempts: 2, wantQuota: 500},
				{name: "trusted wallet settles success once", trusted: true, wantAttempts: 2, wantQuota: 500},
				{name: "manual token prices remain frozen across attempts", expression: `tier("manual", p * 26 + c * 136)`, wantAttempts: 2, wantQuota: 295},
				{name: "second overload stops despite global budget", failAgain: true, wantAttempts: 2},
				{name: "HTTP failure on second attempt cannot start a third", httpFailure: true, wantAttempts: 2},
				{name: "channel defaults off", disabled: true, wantAttempts: 1},
				{name: "global retries disabled", noGlobalRetry: true, wantAttempts: 1},
				{name: "no fallback preserves original overload and refunds", noFallback: true, wantAttempts: 1},
				{name: "plain EOF never retries", failure: "EOF", wantAttempts: 1},
				{name: "message alone is insufficient", failure: strings.ReplaceAll(retryOverloadEvent, "server_is_overloaded", "server_error"), wantAttempts: 1},
				{name: "usage on failed event prevents replay", failure: strings.ReplaceAll(retryOverloadEvent, `"usage":null`, `"usage":{"input_tokens":7,"output_tokens":0}`), wantAttempts: 1, wantQuota: 500},
				{name: "duplicate response fields cannot hide usage", failure: strings.TrimSuffix(retryOverloadEvent, "}") + `,"response":{"status":"failed","error":{"code":"server_is_overloaded"},"usage":{"input_tokens":7}}}`, wantAttempts: 1, wantQuota: 500},
				{name: "zero usage object is not proof of rejection", failure: strings.ReplaceAll(retryOverloadEvent, `"usage":null`, `"usage":{"input_tokens":0,"output_tokens":0}`), wantAttempts: 1},
				{name: "delivered output prevents retry", prefix: `{"type":"response.output_text.delta","delta":"hello"}`, wantAttempts: 1, wantQuota: 500},
				{name: "unpriced function activity prevents retry", prefix: `{"type":"response.output_item.added","item":{"type":"function_call","id":"call-1","name":"unpriced","arguments":""}}`, wantAttempts: 1, wantQuota: 500},
				{name: "unknown event closes retry window", prefix: `{"type":"provider.unknown","value":"extension"}`, wantAttempts: 1},
				{name: "conversation excludes retry", extra: `,"conversation":"conv-state"`, wantAttempts: 1},
				{name: "previous response excludes retry", extra: `,"previous_response_id":"resp-parent"`, wantAttempts: 1},
				{name: "hosted tools exclude retry", extra: `,"tools":[{"type":"web_search"}]`, wantAttempts: 1},
				{name: "client function declarations allow retry", extra: `,"tools":[{"type":"function","name":"local_tool","parameters":{"type":"object"}}]`, wantAttempts: 2, wantQuota: 500},
				{name: "parameter override cannot inject background execution", override: `{"background":true}`, wantAttempts: 1},
				{name: "parameter override cannot inject MCP", override: `{"tools":[{"type":"mcp","server_url":"https://tools.invalid"}]}`, wantAttempts: 1},
				{name: "stored input references exclude retry", override: `{"input":[{"type":"item_reference","id":"msg-stored"}]}`, wantAttempts: 1},
				{name: "stored file references exclude retry", override: `{"input":[{"role":"user","content":[{"type":"input_file","file_id":"file-stored"}]}]}`, wantAttempts: 1},
				{name: "passthrough remains excluded", passthrough: true, wantAttempts: 1},
				{name: "Retry-After beyond budget stops", retryAfter: "60", wantAttempts: 1},
				{name: "oversized Retry-After must not become an immediate retry", retryAfter: "99999999999999999999999", wantAttempts: 1},
			} {
				t.Run(tc.name, func(t *testing.T) {
					modelName := fmt.Sprintf("safe-stream-%d", index)
					expression := tc.expression
					if expression == "" {
						expression = `tier("manual", fixed(0.001))`
					}
					withTieredBillingConfig(t, map[string]string{modelName: "tiered_expr"}, map[string]string{modelName: expression})
					initialQuota := 1000000
					if tc.trusted {
						initialQuota = 100000000
					}
					user := model.User{Username: fmt.Sprintf("retry-user-%d", index), AffCode: fmt.Sprintf("retry-aff-%d", index), Group: "default", Quota: initialQuota}
					require.NoError(t, db.Create(&user).Error)
					token := model.Token{UserId: user.Id, Key: fmt.Sprintf("retry-token-%d", index), RemainQuota: initialQuota}
					require.NoError(t, db.Create(&token).Error)
					settings := `{"proxy":"","system_prompt":"manual prompt","responses_overload_retry_enabled":true}`
					if tc.disabled {
						settings = `{"system_prompt":"manual prompt"}`
					}
					if tc.passthrough {
						settings = `{"responses_overload_retry_enabled":true,"pass_through_body_enabled":true}`
					}
					channel := model.Channel{Name: "retry-channel", Type: 1, Key: "upstream-secret-for-test", BaseURL: common.GetPointer("https://retry.invalid"), Models: modelName, Group: "default", Status: common.ChannelStatusEnabled, Setting: &settings, ParamOverride: common.GetPointer(tc.override)}
					require.NoError(t, db.Create(&channel).Error)
					require.NoError(t, channel.AddAbilities(db))
					fallback := channel
					fallback.Id = 0
					fallback.Name = "fallback-channel"
					if !tc.noFallback {
						require.NoError(t, db.Create(&fallback).Error)
						require.NoError(t, fallback.AddAbilities(db))
					}
					model.InitChannelCache()
					common.RetryTimes = 4
					if tc.noGlobalRetry {
						common.RetryTimes = 0
					}
					attempts := 0
					var upstreamWallets []int
					client.Transport = schedulingTokenTransport(func(request *http.Request) (*http.Response, error) {
						attempts++
						var wallet model.User
						require.NoError(t, db.First(&wallet, user.Id).Error)
						upstreamWallets = append(upstreamWallets, wallet.Quota)
						status := http.StatusOK
						body := "data: " + retryCreatedEvent + "\n\n"
						if tc.prefix != "" {
							body += "data: " + tc.prefix + "\n\n"
						}
						failure := tc.failure
						if failure == "" {
							failure = retryOverloadEvent
						}
						if failure != "EOF" {
							body += "data: " + failure + "\n\n"
						}
						turn := "failed-turn"
						if attempts > 1 && !tc.failAgain {
							body, turn = "data: "+retryCompletedEvent+"\n\n", "successful-turn"
							if tc.httpFailure {
								status, body = http.StatusBadGateway, `{"error":{"message":"unavailable"}}`
							}
						}
						return &http.Response{StatusCode: status, Request: request, Header: http.Header{"Content-Type": {"text/event-stream"}, "X-Codex-Turn-State": {turn}, "Retry-After": {tc.retryAfter}, common.RequestIdKey: {fmt.Sprintf("upstream-%d", attempts)}}, Body: io.NopCloser(strings.NewReader(body))}, nil
					})
					recorder := httptest.NewRecorder()
					c, _ := gin.CreateTestContext(recorder)
					c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(fmt.Sprintf(`{"model":%q,"input":"hello","stream":true%s}`, modelName, tc.extra)))
					c.Request.Header.Set("Content-Type", "application/json")
					c.Set(common.RequestIdKey, fmt.Sprintf("safe-retry-%d", index))
					c.Set("id", user.Id)
					c.Set("username", user.Username)
					c.Set("group", "default")
					c.Set("token_group", "default")
					c.Set("token_id", token.Id)
					c.Set("token_key", token.Key)
					c.Set("token_name", "retry-token")
					c.Set("token_quota", initialQuota)
					common.SetContextKey(c, constant.ContextKeyUserQuota, initialQuota)
					common.SetContextKey(c, constant.ContextKeyUserSetting, dto.UserSetting{BillingPreference: "wallet_only"})
					common.SetContextKey(c, constant.ContextKeyUsingGroup, "default")
					common.SetContextKey(c, constant.ContextKeyRequestStartTime, time.Now())
					common.SetContextKey(c, constant.ContextKeyOriginalModel, modelName)
					require.Nil(t, middleware.SetupContextForSelectedChannel(c, &channel, modelName))
					defer service.EndSchedulingRequest(c)
					Relay(c, types.RelayFormatOpenAIResponses)
					assert.Equal(t, tc.wantAttempts, attempts, recorder.Body.String())
					if tc.noFallback {
						assert.Equal(t, http.StatusServiceUnavailable, recorder.Code)
						assert.Contains(t, recorder.Body.String(), "server_is_overloaded")
					}
					if attempts == 2 {
						require.Len(t, upstreamWallets, 2)
						assert.Equal(t, upstreamWallets[0], upstreamWallets[1], "an unbilled attempt must not settle or reserve quota again")
					}
					if tc.trusted {
						assert.Equal(t, initialQuota, upstreamWallets[0], "trusted wallets bypass pre-consumption")
					}
					var savedUser model.User
					require.Eventually(t, func() bool {
						return db.First(&savedUser, user.Id).Error == nil && savedUser.Quota == initialQuota-tc.wantQuota
					}, 5*time.Second, 10*time.Millisecond, "settlement/refund must preserve the wallet")
					var savedToken model.Token
					require.Eventually(t, func() bool {
						return db.First(&savedToken, token.Id).Error == nil && savedToken.RemainQuota == initialQuota-tc.wantQuota
					}, 5*time.Second, 10*time.Millisecond)
					assert.Equal(t, tc.wantQuota, savedUser.UsedQuota)
					assert.Equal(t, tc.wantQuota, savedToken.UsedQuota)
					var logs []model.Log
					require.NoError(t, logDB.Where("request_id = ?", c.GetString(common.RequestIdKey)).Order("id").Find(&logs).Error)
					consumed := 0
					for _, log := range logs {
						assert.NotContains(t, log.Other, channel.Key)
						if log.Type == model.LogTypeConsume {
							consumed++
							assert.Equal(t, tc.wantQuota, log.Quota)
						}
					}
					if tc.wantQuota == 0 {
						assert.Zero(t, consumed)
					} else {
						assert.Equal(t, 1, consumed)
					}
					if tc.wantAttempts == 2 && !tc.failAgain && !tc.httpFailure {
						assert.Equal(t, "text/event-stream", recorder.Header().Get("Content-Type"))
						assert.Equal(t, "no-cache", recorder.Header().Get("Cache-Control"))
						assert.Equal(t, "no", recorder.Header().Get("X-Accel-Buffering"))
						assert.Equal(t, "keep-alive", recorder.Header().Get("Connection"))
						assert.NotContains(t, recorder.Body.String(), "failed-response")
						assert.Contains(t, recorder.Body.String(), "successful-response")
						assert.Equal(t, "successful-turn", recorder.Header().Get("X-Codex-Turn-State"))
						require.Len(t, logs, 2)
						assert.Contains(t, logs[0].Other, `"no_charge":true`)
						assert.Contains(t, logs[1].Other, `"recovered":true`)
						assert.Contains(t, logs[1].Other, `"upstream_request_id":"upstream-1"`, "the recovered row must retain the rejected attempt's upstream ID")
						assert.Equal(t, "upstream-2", logs[1].UpstreamRequestId)
					}
					if tc.failAgain || tc.httpFailure {
						require.Len(t, logs, 2, "both failures must be durable even with optional error logging disabled")
						assert.Contains(t, logs[1].Other, `"stream_retry_limit"`)
						assert.Contains(t, logs[1].Other, `"no_charge":true`)
					}
					configured, exists := billing_setting.GetBillingExpr(modelName)
					assert.True(t, exists)
					assert.Equal(t, expression, configured)
					var savedChannel model.Channel
					require.NoError(t, db.First(&savedChannel, channel.Id).Error)
					assert.Equal(t, settings, *savedChannel.Setting, "retry must not change manual channel settings")
					assert.Equal(t, !tc.disabled, savedChannel.GetSetting().ResponsesOverloadRetryEnabled)
				})
			}
			var preserved model.Log
			require.NoError(t, logDB.First(&preserved, historical.Id).Error)
			assert.Equal(t, historical, preserved)
		})
	}
}
