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

func TestGenerationSettlementRequiresOutput(t *testing.T) {
	require.NoError(t, i18n.Init())
	oldMode, oldTimeout := gin.Mode(), constant.StreamingTimeout
	gin.SetMode(gin.TestMode)
	constant.StreamingTimeout = 1
	t.Cleanup(func() { gin.SetMode(oldMode); constant.StreamingTimeout = oldTimeout })

	const chat = `{"id":"chat_1","model":"gpt-4o-mini","choices":[{"index":0,"message":{"role":"assistant","content":"%s"},"finish_reason":"stop"}],"usage":{"prompt_tokens":17,"completion_tokens":0}}`
	const response = `{"id":"resp_1","model":"gpt-4o-mini","status":"completed","output":[{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"%s"}]}],"usage":{"input_tokens":17,"output_tokens":0}}`
	const claudeResponse = `{"id":"msg_1","type":"message","role":"assistant","model":"claude-sonnet-4","content":[{"type":"text","text":"%s"}],"stop_reason":"end_turn","usage":{"input_tokens":17,"output_tokens":0}}`
	const geminiResponse = `{"candidates":[{"content":{"role":"model","parts":[{"text":"%s"}]},"finishReason":"STOP"}],"usageMetadata":{"promptTokenCount":17,"candidatesTokenCount":0,"totalTokenCount":17}}`
	const chatStream = "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"%s\"}}],\"usage\":{\"prompt_tokens\":17}}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}]}\n\ndata: [DONE]\n\n"
	const responsesStream = "data: {\"type\":\"response.completed\",\"response\":" + response + "}\n\n"
	const claudeStream = "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"content\":[],\"usage\":{\"input_tokens\":17}}}\n\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"%s\"}}\n\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":{\"output_tokens\":0}}\n\ndata: {\"type\":\"message_stop\"}\n\n"
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
		for _, output := range []string{"", "hello"} {
			t.Run(fmt.Sprintf("%s/output=%q", tc.name, output), func(t *testing.T) {
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
				resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(fmt.Sprintf(tc.body, output)))}
				usage, apiErr := tc.handle(c, info, resp)
				if output != "" {
					require.Nil(t, apiErr)
					require.NotNil(t, usage)
					assert.Contains(t, w.Body.String(), output)
					assert.False(t, reservation.refunded)
					return
				}
				require.NotNil(t, apiErr, "usage without output must not enter successful settlement")
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
