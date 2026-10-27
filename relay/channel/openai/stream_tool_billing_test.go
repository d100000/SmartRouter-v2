package openai

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"testing/iotest"
	"testing/synctest"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/i18n"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestObserveStreamChoicesDedupesFunctionCallNames(t *testing.T) {
	info := &relaycommon.RelayInfo{StreamStatus: relaycommon.NewStreamStatus()}
	seen := make(map[string]struct{})
	var names []string

	chunks := []string{
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"c1","type":"function","function":{"name":"get_weather","arguments":""}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"{\"q\":"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"x\"}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"c2","type":"function","function":{"name":"get_time","arguments":""}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"{}"}}]}}]}`,
	}
	for _, chunk := range chunks {
		observeStreamChoices(info, chunk, seen, &names)
	}

	require.Len(t, names, 2)
	assert.Equal(t, []string{"get_weather", "get_time"}, names)
	assert.Empty(t, info.StreamStatus.ResponseOutcome(), "tool call deltas carry no finish reason")

	observeStreamChoices(info, `{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`, seen, &names)
	assert.Equal(t, "completed", info.StreamStatus.ResponseOutcome())
	assert.False(t, info.PerformanceBusinessRejection)

	filtered := &relaycommon.RelayInfo{StreamStatus: relaycommon.NewStreamStatus()}
	observeStreamChoices(filtered, `{"choices":[{"index":0,"delta":{},"finish_reason":"content_filter"}]}`, map[string]struct{}{}, &names)
	assert.True(t, filtered.PerformanceBusinessRejection)
}

func TestOaiStreamHandlerRejectsUnbillableFailures(t *testing.T) {
	for _, test := range []struct {
		name        string
		contentType string
		body        string
		code        types.ErrorCode
		message     string
	}{
		{name: "error with done", body: "data: {\"error\":{\"message\":\"context too long\",\"type\":\"invalid_request_error\",\"code\":\"context_length_exceeded\"}}\n\ndata: [DONE]\n\n", code: "context_length_exceeded", message: "context too long"},
		{name: "error without type", body: "data: {\"error\":{\"message\":\"context too long\",\"code\":\"context_length_exceeded\"}}\n\n", code: "context_length_exceeded", message: "context too long"},
		{name: "named flat error", body: "event: error\ndata: {\"message\":\"upstream refused\",\"code\":\"rejected\"}\n\n", code: "rejected", message: "upstream refused"},
		{name: "named string error", body: "event: error\ndata: \"upstream refused\"\n\n", code: types.ErrorCodeBadResponse, message: "upstream refused"},
		{name: "named empty error", body: "event: error\n\n", code: types.ErrorCodeBadResponse},
		{name: "string envelope", body: "data: {\"error\":\"upstream refused\"}\n\n", code: types.ErrorCodeBadResponse, message: "upstream refused"},
		{name: "only done", body: "data: [DONE]\n\n", code: types.ErrorCodeEmptyResponse},
		{name: "empty eof", code: types.ErrorCodeBadResponse},
		{name: "role only", body: "data: {\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\ndata: [DONE]\n\n", code: types.ErrorCodeEmptyResponse},
		{name: "finish only", body: "data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", code: types.ErrorCodeEmptyResponse},
		{name: "whitespace only", body: "data: {\"choices\":[{\"delta\":{\"content\":\"   \"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", code: types.ErrorCodeEmptyResponse},
		{name: "heartbeats only", body: ": ping\n\nevent: heartbeat\ndata: {\"type\":\"heartbeat\"}\n\ndata: [DONE]\n\n", code: types.ErrorCodeEmptyResponse},
		{name: "zero usage", body: "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":0,\"completion_tokens\":0}}\n\ndata: [DONE]\n\n", code: types.ErrorCodeEmptyResponse},
		{name: "estimated canonical usage", body: "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":1325020,\"billing_usage\":{\"source\":\"oai_chat\",\"semantic\":\"openai\",\"estimated\":true,\"openai_usage\":{\"prompt_tokens\":1325020}}}}\n\ndata: [DONE]\n\n", code: types.ErrorCodeEmptyResponse},
		{name: "local usage source", body: "data: {\"usage\":{\"prompt_tokens\":1325020,\"usage_source\":\"local\"}}\n\ndata: [DONE]\n\n", code: types.ErrorCodeEmptyResponse},
		{name: "estimated canonical source", body: "data: {\"usage\":{\"billing_usage\":{\"source\":\"oai_chat_estimated\",\"semantic\":\"openai\",\"openai_usage\":{\"prompt_tokens\":1325020}}}}\n\ndata: [DONE]\n\n", code: types.ErrorCodeEmptyResponse},
		{name: "negative usage", body: "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":-1}}\n\ndata: [DONE]\n\n", code: types.ErrorCodeEmptyResponse},
		{name: "invalid event", body: "data: not-json\n\n", code: types.ErrorCodeBadResponseBody},
		{name: "json error without type", contentType: "application/json", body: `{"error":{"message":"context too long","code":"context_length_exceeded"}}`, code: "context_length_exceeded", message: "context too long"},
		{name: "json error incidental choices", contentType: "application/json", body: `{"error":{"message":"upstream refused"},"choices":[{"message":{"role":"assistant","content":"not delivered"}}]}`, code: types.ErrorCodeBadResponse, message: "upstream refused"},
		{name: "invalid json response", contentType: "application/json", body: `{`, code: types.ErrorCodeBadResponseBody},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, response, info, recorder := setupChatBillingStream(t, io.NopCloser(strings.NewReader(test.body)))
			if test.contentType != "" {
				response.Header.Set("Content-Type", test.contentType)
			}
			usage, apiErr := OaiStreamHandler(ctx, info, response)
			require.NotNil(t, apiErr)
			assert.Nil(t, usage)
			assert.Equal(t, test.code, apiErr.GetErrorCode())
			assert.True(t, types.IsSkipRetryError(apiErr))
			if test.message != "" {
				assert.Contains(t, apiErr.Error(), test.message)
			}
			require.NotNil(t, info.StreamFailure)
			assert.True(t, info.StreamFailure.NoCharge)
			assert.False(t, info.StreamFailure.HasActualUsage)
			assert.False(t, info.StreamFailure.HasEffectiveOutput)
			assert.True(t, info.StreamStatus.ResponseFailed())
			assert.True(t, info.StreamStatus.HasErrors())
			assert.NotContains(t, recorder.Body.String(), "[DONE]")
			assert.NotContains(t, recorder.Body.String(), `"prompt_tokens":1325020`)
		})
	}
}

func TestOaiStreamHandlerKeepsOutputAndRealUsageBillable(t *testing.T) {
	for _, test := range []struct {
		name        string
		body        string
		contentType string
		prompt      int
		completion  int
		actualUsage bool
		failure     bool
		visible     string
	}{
		{name: "text without usage", body: "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\ndata: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", prompt: 1325020, completion: -1},
		{name: "reasoning only", body: "data: {\"choices\":[{\"delta\":{\"reasoning_content\":\"thinking\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", prompt: 1325020, completion: -1},
		{name: "refusal only", body: "data: {\"choices\":[{\"delta\":{\"refusal\":\"I cannot help\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", prompt: 1325020, completion: -1},
		{name: "refusal with usage", body: "data: {\"choices\":[{\"delta\":{\"refusal\":\"I cannot help\"},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":17,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n", prompt: 17, completion: 2, actualUsage: true, visible: "I cannot help"},
		{name: "legacy function", body: "data: {\"choices\":[{\"delta\":{\"function_call\":{\"name\":\"weather\",\"arguments\":\"{}\"}},\"finish_reason\":\"function_call\"}]}\n\ndata: [DONE]\n\n", prompt: 1325020, completion: -1},
		{name: "tool only", body: "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"id\":\"call1\",\"type\":\"function\",\"function\":{\"name\":\"weather\",\"arguments\":\"{}\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n", prompt: 1325020, completion: -1},
		{name: "custom tool", body: "data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"id\":\"call1\",\"type\":\"custom\",\"custom\":{\"name\":\"python\",\"input\":\"print(1)\"}}]},\"finish_reason\":\"tool_calls\"}]}\n\ndata: [DONE]\n\n", prompt: 1325020, completion: -1},
		{name: "audio payload", body: "data: {\"choices\":[{\"delta\":{\"audio\":{\"data\":\"aGVsbG8=\"}},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n", prompt: 1325020},
		{name: "input usage only", body: "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":17,\"completion_tokens\":0,\"total_tokens\":17}}\n\ndata: [DONE]\n\n", prompt: 17, actualUsage: true},
		{name: "canonical cache only", body: "data: {\"usage\":{\"billing_usage\":{\"source\":\"claude_messages\",\"semantic\":\"anthropic\",\"claude_usage\":{\"input_tokens\":0,\"output_tokens\":0,\"cache_read_input_tokens\":17}}}}\n\ndata: [DONE]\n\n", actualUsage: true},
		{name: "JSON response with output and usage", contentType: "application/json", body: `{"choices":[{"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":17,"completion_tokens":2}}`, prompt: 17, completion: 2, actualUsage: true, visible: "hello"},
		{name: "earlier usage retained", body: "data: {\"choices\":[],\"usage\":{\"prompt_tokens\":17,\"completion_tokens\":2}}\n\ndata: {\"choices\":[]}\n\ndata: {\"choices\":[]}\n\ndata: [DONE]\n\n", prompt: 17, completion: 2, actualUsage: true},
		{name: "actual replaces estimate", body: "data: {\"usage\":{\"prompt_tokens\":1325020,\"billing_usage\":{\"source\":\"oai_chat\",\"semantic\":\"openai\",\"estimated\":true,\"openai_usage\":{\"prompt_tokens\":1325020}}}}\n\ndata: {\"usage\":{\"prompt_tokens\":17,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n", prompt: 17, completion: 2, actualUsage: true},
		{name: "partial output error", body: "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\ndata: {\"error\":{\"message\":\"upstream failed\",\"code\":\"upstream_failed\"}}\n\n", prompt: 1325020, completion: -1, failure: true},
		{name: "actual usage error", body: "data: {\"usage\":{\"prompt_tokens\":17,\"completion_tokens\":0}}\n\ndata: {\"error\":{\"message\":\"upstream failed\"}}\n\n", prompt: 17, actualUsage: true, failure: true},
		{name: "json actual usage error", contentType: "application/json", body: `{"usage":{"prompt_tokens":17},"error":{"message":"upstream failed"}}`, prompt: 17, actualUsage: true, failure: true},
		{name: "partial premature eof", body: "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"}}]}\n\n", prompt: 1325020, completion: -1, failure: true},
		{name: "finish without done", body: "data: {\"choices\":[{\"delta\":{\"content\":\"hello\"},\"finish_reason\":\"stop\"}]}\n\n", prompt: 1325020, completion: -1},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, response, info, recorder := setupChatBillingStream(t, io.NopCloser(strings.NewReader(test.body)))
			if test.contentType != "" {
				response.Header.Set("Content-Type", test.contentType)
			}
			usage, apiErr := OaiStreamHandler(ctx, info, response)
			require.Nil(t, apiErr)
			require.NotNil(t, usage)
			assert.Equal(t, test.prompt, usage.PromptTokens)
			if test.completion >= 0 {
				assert.Equal(t, test.completion, usage.CompletionTokens)
			} else {
				assert.Positive(t, usage.CompletionTokens)
			}
			assert.Equal(t, !test.actualUsage, common.GetContextKeyBool(ctx, constant.ContextKeyLocalCountTokens))
			if test.visible != "" {
				assert.Contains(t, recorder.Body.String(), test.visible)
			}
			if test.failure {
				require.NotNil(t, info.StreamFailure)
				assert.False(t, info.StreamFailure.NoCharge)
				assert.Equal(t, test.actualUsage, info.StreamFailure.HasActualUsage)
				assert.True(t, info.StreamStatus.ResponseFailed())
				assert.Contains(t, recorder.Body.String(), `"error"`)
				assert.NotContains(t, recorder.Body.String(), "[DONE]")
			} else {
				assert.Nil(t, info.StreamFailure)
			}
		})
	}
}

func TestOaiStreamHandlerUnbillableTransportFailures(t *testing.T) {
	for _, name := range []string{"scanner error", "timeout", "client cancelled"} {
		t.Run(name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				var body io.ReadCloser
				if name == "scanner error" {
					body = io.NopCloser(iotest.ErrReader(errors.New("upstream read failed")))
				} else {
					reader, writer := io.Pipe()
					t.Cleanup(func() { _ = writer.Close() })
					body = reader
				}
				ctx, response, info, recorder := setupChatBillingStream(t, body)
				if name == "client cancelled" {
					requestCtx, cancel := context.WithCancel(ctx.Request.Context())
					cancel()
					ctx.Request = ctx.Request.WithContext(requestCtx)
				}
				usage, apiErr := OaiStreamHandler(ctx, info, response)
				require.NotNil(t, apiErr)
				assert.Nil(t, usage)
				require.NotNil(t, info.StreamFailure)
				assert.True(t, info.StreamFailure.NoCharge)
				assert.True(t, info.StreamStatus.ResponseFailed())
				assert.NotContains(t, recorder.Body.String(), "[DONE]")
			})
		})
	}
}

func TestOaiStreamHandlerBoundsErrorDiagnostics(t *testing.T) {
	ctx, response, info, _ := setupChatBillingStream(t, io.NopCloser(strings.NewReader("event: error\ndata: {\"message\":\"upstream refused\",\"code\":{\"secret\":\"never-store-this\"},\"type\":\"sk-secret-token\"}\n\n")))
	_, apiErr := OaiStreamHandler(ctx, info, response)
	require.NotNil(t, apiErr)
	assert.Equal(t, types.ErrorCodeBadResponse, apiErr.GetErrorCode())
	assert.Equal(t, "upstream_error", apiErr.ToOpenAIError().Type)
	assert.NotContains(t, apiErr.Error(), "never-store-this")
	assert.NotContains(t, info.StreamStatus.Summary(), "never-store-this")
	assert.NotContains(t, info.StreamStatus.Summary(), "sk-secret-token")
}

func TestOaiStreamHandlerDoesNotChargeDiscardedExtensions(t *testing.T) {
	for _, test := range []struct {
		name           string
		format         types.RelayFormat
		forceFormat    bool
		thinkToContent bool
	}{
		{name: "forced format", format: types.RelayFormatOpenAI, forceFormat: true},
		{name: "thinking conversion", format: types.RelayFormatOpenAI, thinkToContent: true},
		{name: "Claude conversion", format: types.RelayFormatClaude},
		{name: "Gemini conversion", format: types.RelayFormatGemini},
	} {
		t.Run(test.name, func(t *testing.T) {
			ctx, response, info, recorder := setupChatBillingStream(t, io.NopCloser(strings.NewReader("data: {\"choices\":[{\"delta\":{\"refusal\":\"I cannot help\"},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n")))
			info.RelayFormat = test.format
			info.ChannelSetting.ForceFormat = test.forceFormat
			info.ChannelSetting.ThinkingToContent = test.thinkToContent
			usage, apiErr := OaiStreamHandler(ctx, info, response)
			require.NotNil(t, apiErr)
			assert.Nil(t, usage)
			require.NotNil(t, info.StreamFailure)
			assert.True(t, info.StreamFailure.NoCharge)
			assert.NotContains(t, recorder.Body.String(), "I cannot help")
		})
	}
}

func setupChatBillingStream(t *testing.T, body io.ReadCloser) (*gin.Context, *http.Response, *relaycommon.RelayInfo, *httptest.ResponseRecorder) {
	t.Helper()
	require.NoError(t, i18n.Init())
	previousMode, previousTimeout := gin.Mode(), constant.StreamingTimeout
	gin.SetMode(gin.TestMode)
	constant.StreamingTimeout = 1
	t.Cleanup(func() {
		gin.SetMode(previousMode)
		constant.StreamingTimeout = previousTimeout
	})
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	ctx.Request.Header.Set("Accept-Language", "en")
	info := &relaycommon.RelayInfo{
		ChannelMeta: &relaycommon.ChannelMeta{ChannelType: constant.ChannelTypeOpenAI, UpstreamModelName: "gpt-4o-mini"},
		RelayFormat: types.RelayFormatOpenAI, RelayMode: relayconstant.RelayModeChatCompletions,
		OriginModelName: "gpt-4o-mini", IsStream: true, DisablePing: true, StartTime: time.Now(),
	}
	info.SetEstimatePromptTokens(1325020)
	response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: body}
	return ctx, response, info, recorder
}
