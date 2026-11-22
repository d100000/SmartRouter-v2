package relay

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/relay/channel/claude"
	"github.com/QuantumNous/new-api/relay/channel/gemini"
	"github.com/QuantumNous/new-api/relay/channel/openai"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type generationReservation struct{ refunded bool }

func (*generationReservation) Reserve(int) error             { return nil }
func (*generationReservation) ReserveBeforeSubmit(int) error { return nil }
func (*generationReservation) GetPreConsumedQuota() int      { return 100 }
func (*generationReservation) Settle(int) error              { return nil }
func (s *generationReservation) Refund(*gin.Context)         { s.refunded = true }
func (s *generationReservation) NeedsRefund() bool           { return !s.refunded }

// A generation is charged when upstream reported usage or produced output;
// only a response with neither is an uncharged empty response.
func TestGenerationSettlementRequiresUsageOrOutput(t *testing.T) {
	require.NoError(t, i18n.Init())
	oldMode, oldTimeout := gin.Mode(), constant.StreamingTimeout
	gin.SetMode(gin.TestMode)
	constant.StreamingTimeout = 1
	t.Cleanup(func() { gin.SetMode(oldMode); constant.StreamingTimeout = oldTimeout })

	// %[1]s is the generated text and %[2]d the upstream input token count.
	const chat = `{"id":"chat_1","model":"gpt-4o-mini","choices":[{"index":0,"message":{"role":"assistant","content":"%[1]s"},"finish_reason":"stop"}],"usage":{"prompt_tokens":%[2]d,"completion_tokens":0}}`
	const response = `{"id":"resp_1","model":"gpt-4o-mini","status":"completed","output":[{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"%[1]s"}]}],"usage":{"input_tokens":%[2]d,"output_tokens":0}}`
	const claudeResponse = `{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4","content":[{"type":"text","text":"%[1]s"}],"stop_reason":"end_turn","usage":{"input_tokens":%[2]d,"output_tokens":0}}`
	const geminiResponse = `{"candidates":[{"content":{"role":"model","parts":[{"text":"%[1]s"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":%[2]d,"candidatesTokenCount":0,"totalTokenCount":%[2]d}}`
	const chatStream = "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"%[1]s\"}}],\"usage\":{\"prompt_tokens\":%[2]d}}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	const responsesStream = "data: {\"type\":\"response.completed\",\"response\":" + response + "}\n\n"
	const claudeStream = "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":%[2]d}}}\n\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"%[1]s\"}}\n\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":0}}\n\ndata: {\"type\":\"message_stop\"}\n\n"
	for _, tc := range []struct {
		name, body string
		stream     bool
		format     types.RelayFormat
		handle     func(*gin.Context, *relaycommon.RelayInfo, *http.Response) (*dto.Usage, *types.NewAPIError)
	}{
		{"chat JSON", chat, false, types.RelayFormatOpenAI, openai.OpenaiHandler},
		{"chat SSE", chatStream, true, types.RelayFormatOpenAI, openai.OaiStreamHandler},
		{"responses JSON", response, false, types.RelayFormatOpenAIResponses, openai.OaiResponsesHandler},
		{"responses SSE", responsesStream, true, types.RelayFormatOpenAIResponses, openai.OaiResponsesStreamHandler},
		{"responses to chat JSON", response, false, types.RelayFormatOpenAI, openai.OaiResponsesToChatHandler},
		{"responses to chat SSE", responsesStream, true, types.RelayFormatOpenAI, openai.OaiResponsesToChatStreamHandler},
		{"buffered responses to chat", responsesStream, true, types.RelayFormatOpenAI, openai.OaiResponsesToChatBufferedStreamHandler},
		{"chat to responses JSON", chat, false, types.RelayFormatOpenAIResponses, openai.OaiChatToResponsesHandler},
		{"chat to responses SSE", chatStream, true, types.RelayFormatOpenAIResponses, openai.OaiChatToResponsesStreamHandler},
		{"claude JSON", claudeResponse, false, types.RelayFormatClaude, func(c *gin.Context, info *relaycommon.RelayInfo, r *http.Response) (*dto.Usage, *types.NewAPIError) {
			return claude.ClaudeHandler(c, r, info)
		}},
		{"claude to responses JSON", claudeResponse, false, types.RelayFormatOpenAIResponses, func(c *gin.Context, info *relaycommon.RelayInfo, r *http.Response) (*dto.Usage, *types.NewAPIError) {
			return claude.ClaudeHandler(c, r, info)
		}},
		{"claude to responses SSE", claudeStream, true, types.RelayFormatOpenAIResponses, func(c *gin.Context, info *relaycommon.RelayInfo, r *http.Response) (*dto.Usage, *types.NewAPIError) {
			return claude.ClaudeResponsesStreamHandler(c, r, info)
		}},
		{"claude SSE", claudeStream, true, types.RelayFormatClaude, func(c *gin.Context, info *relaycommon.RelayInfo, r *http.Response) (*dto.Usage, *types.NewAPIError) {
			return claude.ClaudeStreamHandler(c, r, info)
		}},
		{"gemini to chat JSON", geminiResponse, false, types.RelayFormatOpenAI, gemini.GeminiChatHandler},
		{"gemini to chat SSE", "data: " + geminiResponse + "\n\n", true, types.RelayFormatOpenAI, gemini.GeminiChatStreamHandler},
		{"gemini JSON", geminiResponse, false, types.RelayFormatGemini, gemini.GeminiTextGenerationHandler},
		{"gemini SSE", "data: " + geminiResponse + "\n\n", true, types.RelayFormatGemini, gemini.GeminiTextGenerationStreamHandler},
		{"gemini to responses JSON", geminiResponse, false, types.RelayFormatOpenAIResponses, gemini.GeminiResponsesHandler},
		{"gemini to responses SSE", "data: " + geminiResponse + "\n\n", true, types.RelayFormatOpenAIResponses, gemini.GeminiResponsesStreamHandler},
	} {
		for _, variant := range []struct {
			output      string
			inputTokens int
		}{{"", 0}, {"", 17}, {"hello", 17}, {"hello", 0}} {
			output := variant.output
			t.Run(fmt.Sprintf("%s/output=%q/input=%d", tc.name, output, variant.inputTokens), func(t *testing.T) {
				w := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(w)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
				c.Request.Header.Set("Accept-Language", "en")
				reservation := &generationReservation{}
				info := &relaycommon.RelayInfo{
					ChannelMeta:     &relaycommon.ChannelMeta{ChannelType: constant.ChannelTypeOpenAI, UpstreamModelName: "gpt-4o-mini"},
					OriginModelName: "gpt-4o-mini", RelayMode: relayconstant.RelayModeChatCompletions,
					RelayFormat: tc.format, IsStream: tc.stream, DisablePing: true, StartTime: time.Now(), Billing: reservation,
				}
				info.SetEstimatePromptTokens(1000)
				contentType := "application/json"
				if tc.stream {
					contentType = "text/event-stream"
				}
				resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(fmt.Sprintf(tc.body, output, variant.inputTokens)))}
				usage, apiErr := tc.handle(c, info, resp)
				if output != "" || variant.inputTokens > 0 {
					require.Nil(t, apiErr, "usage or output means the generation ran and is charged")
					require.NotNil(t, usage)
					assert.Positive(t, usage.PromptTokens)
					assert.Contains(t, w.Body.String(), output)
					assert.False(t, reservation.refunded)
					assert.Nil(t, info.StreamFailure)
					return
				}
				require.NotNil(t, apiErr, "neither usage nor output is an empty response")
				assert.Nil(t, usage)
				assert.Equal(t, types.ErrorCodeEmptyResponse, apiErr.GetErrorCode())
				assert.NotContains(t, w.Body.String(), "[DONE]")
				assert.NotContains(t, w.Body.String(), `"type":"response.completed"`)
				assert.NotContains(t, w.Body.String(), `"type":"message_stop"`)
				RefundFailedRequestBilling(c, info, apiErr)
				assert.True(t, reservation.refunded, "release any pre-consumed quota")
				if info.StreamFailure != nil {
					assert.True(t, info.StreamFailure.NoCharge)
					assert.False(t, info.StreamFailure.HasEffectiveOutput)
				}
			})
		}
	}
}

// Reproduced upstream replies that ran (usage or output) but have no visible
// text: each is returned as upstream sent it and charged.
func TestGenerationWithoutVisibleTextIsCharged(t *testing.T) {
	require.NoError(t, i18n.Init())
	oldMode, oldTimeout := gin.Mode(), constant.StreamingTimeout
	gin.SetMode(gin.TestMode)
	constant.StreamingTimeout = 1
	t.Cleanup(func() { gin.SetMode(oldMode); constant.StreamingTimeout = oldTimeout })

	const incomplete = `{"id":"resp_1","model":"gpt-5","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[{"id":"rs_1","type":"reasoning","summary":[]}]%s}`
	const queued = `{"id":"resp_bg","object":"response","model":"gpt-5","status":"queued","background":true,"output":[],"usage":null}`
	const failedStream = "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"status\":\"in_progress\",\"output\":[]}}\n\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"resp_1\",\"status\":\"failed\",\"error\":{\"code\":\"server_error\",\"message\":\"upstream failed\"},\"output\":[],\"usage\":{\"input_tokens\":40,\"output_tokens\":0,\"total_tokens\":40}}}\n\n"
	const claudeThinking = `{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4","content":[{"type":"thinking","thinking":"","signature":"EqQBCkYIBxgCKkA"}],"stop_reason":"max_tokens","usage":{"input_tokens":30,"output_tokens":1024}}`
	const claudeThinkingStream = "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":30}}}\n\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"thinking\",\"thinking\":\"\"}}\n\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"EqQBCkYIBxgCKkA\"}}\n\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"max_tokens\"},\"usage\":{\"output_tokens\":1024}}\n\ndata: {\"type\":\"message_stop\"}\n\n"
	const geminiThoughts = `{"candidates":[{"content":{"role":"model"},"finishReason":"MAX_TOKENS"}],"usageMetadata":{"promptTokenCount":30,"thoughtsTokenCount":1024,"totalTokenCount":1054}}`
	for _, tc := range []struct {
		name, body string
		stream     bool
		format     types.RelayFormat
		visible    string
		failure    bool
		handle     func(*gin.Context, *relaycommon.RelayInfo, *http.Response) (*dto.Usage, *types.NewAPIError)
	}{
		{"responses incomplete reasoning item without usage", fmt.Sprintf(incomplete, ""), false, types.RelayFormatOpenAIResponses, `"type":"reasoning"`, false, openai.OaiResponsesHandler},
		{"responses incomplete reasoning item with usage", fmt.Sprintf(incomplete, `,"usage":{"input_tokens":40,"output_tokens":256,"output_tokens_details":{"reasoning_tokens":256}}`), false, types.RelayFormatOpenAIResponses, `"status":"incomplete"`, false, openai.OaiResponsesHandler},
		{"responses SSE incomplete reasoning item", "data: {\"type\":\"response.incomplete\",\"response\":" + fmt.Sprintf(incomplete, "") + "}\n\n", true, types.RelayFormatOpenAIResponses, `"type":"reasoning"`, false, openai.OaiResponsesStreamHandler},
		{"responses background queued", queued, false, types.RelayFormatOpenAIResponses, `"status":"queued"`, false, openai.OaiResponsesHandler},
		{"responses SSE failed with usage", failedStream, true, types.RelayFormatOpenAIResponses, `"response.failed"`, true, openai.OaiResponsesStreamHandler},
		{"claude hidden thinking hit max tokens", claudeThinking, false, types.RelayFormatClaude, `"stop_reason":"max_tokens"`, false, func(c *gin.Context, info *relaycommon.RelayInfo, r *http.Response) (*dto.Usage, *types.NewAPIError) {
			return claude.ClaudeHandler(c, r, info)
		}},
		{"claude SSE hidden thinking hit max tokens", claudeThinkingStream, true, types.RelayFormatClaude, `"signature_delta"`, false, func(c *gin.Context, info *relaycommon.RelayInfo, r *http.Response) (*dto.Usage, *types.NewAPIError) {
			return claude.ClaudeStreamHandler(c, r, info)
		}},
		{"gemini hidden thoughts hit max tokens", geminiThoughts, false, types.RelayFormatGemini, `"MAX_TOKENS"`, false, gemini.GeminiTextGenerationHandler},
		{"gemini SSE hidden thoughts hit max tokens", "data: " + geminiThoughts + "\n\n", true, types.RelayFormatGemini, `"MAX_TOKENS"`, false, gemini.GeminiTextGenerationStreamHandler},
		{"gemini to chat hidden thoughts hit max tokens", geminiThoughts, false, types.RelayFormatOpenAI, `"finish_reason":"length"`, false, gemini.GeminiChatHandler},
		{"gemini to responses hidden thoughts hit max tokens", geminiThoughts, false, types.RelayFormatOpenAIResponses, `"usage"`, false, gemini.GeminiResponsesHandler},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(w)
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
			c.Request.Header.Set("Accept-Language", "en")
			reservation := &generationReservation{}
			info := &relaycommon.RelayInfo{
				ChannelMeta:     &relaycommon.ChannelMeta{ChannelType: constant.ChannelTypeOpenAI, UpstreamModelName: "gpt-5"},
				OriginModelName: "gpt-5", RelayMode: relayconstant.RelayModeChatCompletions,
				RelayFormat: tc.format, IsStream: tc.stream, DisablePing: true, StartTime: time.Now(), Billing: reservation,
			}
			info.SetEstimatePromptTokens(1000)
			contentType := "application/json"
			if tc.stream {
				contentType = "text/event-stream"
			}
			resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(tc.body))}
			usage, apiErr := tc.handle(c, info, resp)
			require.Nil(t, apiErr)
			require.NotNil(t, usage)
			assert.Positive(t, usage.PromptTokens)
			assert.Contains(t, w.Body.String(), tc.visible)
			assert.False(t, reservation.refunded)
			if tc.failure {
				require.NotNil(t, info.StreamFailure)
				assert.False(t, info.StreamFailure.NoCharge)
				assert.True(t, info.StreamFailure.HasActualUsage)
				assert.Equal(t, 40, usage.PromptTokens)
			} else {
				assert.Nil(t, info.StreamFailure)
			}
		})
	}
}
