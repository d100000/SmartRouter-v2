package relay

import (
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
	"github.com/QuantumNous/new-api/relay/channel/claude"
	"github.com/QuantumNous/new-api/relay/channel/cloudflare"
	"github.com/QuantumNous/new-api/relay/channel/cohere"
	"github.com/QuantumNous/new-api/relay/channel/dify"
	"github.com/QuantumNous/new-api/relay/channel/gemini"
	"github.com/QuantumNous/new-api/relay/channel/openai"
	"github.com/QuantumNous/new-api/relay/channel/tencent"
	"github.com/QuantumNous/new-api/relay/channel/xai"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type observedTokenRequest struct {
	dto.GeneralOpenAIRequest
	counts int
}

func (r *observedTokenRequest) GetTokenCountMeta() *types.TokenCountMeta {
	r.counts++
	return r.GeneralOpenAIRequest.GetTokenCountMeta()
}

type tokenUsageReader struct {
	io.Reader
	t       *testing.T
	request *observedTokenRequest
}

func (r tokenUsageReader) Read(p []byte) (int, error) {
	assert.Zero(r.t, r.request.counts, "input must not be tokenized while reading the upstream response")
	return r.Reader.Read(p)
}

func responseUsageHandler(handle func(*gin.Context, *http.Response, *relaycommon.RelayInfo) (any, *types.NewAPIError)) func(*gin.Context, *relaycommon.RelayInfo, *http.Response) (*dto.Usage, *types.NewAPIError) {
	return func(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
		usage, err := handle(c, resp, info)
		if err != nil {
			return nil, err
		}
		return usage.(*dto.Usage), nil
	}
}

func TestResponseUsageWithoutPrecount(t *testing.T) {
	require.NoError(t, i18n.Init())
	previousCount, previousTimeout := constant.CountToken, constant.StreamingTimeout
	constant.CountToken, constant.StreamingTimeout = false, 1
	t.Cleanup(func() { constant.CountToken, constant.StreamingTimeout = previousCount, previousTimeout })

	const chat = `{"id":"chat_1","model":"gpt-4o-mini","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":%s}`
	const response = `{"id":"resp_1","model":"gpt-4o-mini","status":"completed","output":[{"id":"msg_1","type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]}],"usage":%s}`
	const claudeResponse = `{"id":"msg_1","type":"message","role":"assistant","model":"gpt-4o-mini","content":[{"type":"text","text":"hello"}],"stop_reason":"end_turn","usage":%s}`
	const geminiResponse = `{"candidates":[{"content":{"role":"model","parts":[{"text":"hello"}]},"finishReason":"STOP"}],"usageMetadata":%s}`
	const chatStream = "data: {\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\ndata: {\"choices\":[{\"index\":0,\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":%s}\n\ndata: [DONE]\n\n"
	const responseStream = "data: {\"type\":\"response.completed\",\"response\":" + response + "}\n\n"
	const claudeStream = "data: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"type\":\"message\",\"role\":\"assistant\",\"model\":\"gpt-4o-mini\",\"content\":[]}}\n\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"hello\"}}\n\ndata: {\"type\":\"content_block_stop\",\"index\":0}\n\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"},\"usage\":%s}\n\ndata: {\"type\":\"message_stop\"}\n\n"

	for _, provider := range []struct {
		name, body, usage string
		stream            bool
		format            types.RelayFormat
		handle            func(*gin.Context, *relaycommon.RelayInfo, *http.Response) (*dto.Usage, *types.NewAPIError)
	}{
		{"chat JSON", chat, `{"prompt_tokens":%d,"completion_tokens":%d}`, false, types.RelayFormatOpenAI, openai.OpenaiHandler},
		{"chat SSE", chatStream, `{"prompt_tokens":%d,"completion_tokens":%d}`, true, types.RelayFormatOpenAI, openai.OaiStreamHandler},
		{"responses JSON", response, `{"input_tokens":%d,"output_tokens":%d}`, false, types.RelayFormatOpenAIResponses, openai.OaiResponsesHandler},
		{"responses SSE", responseStream, `{"input_tokens":%d,"output_tokens":%d}`, true, types.RelayFormatOpenAIResponses, openai.OaiResponsesStreamHandler},
		{"responses to chat JSON", response, `{"input_tokens":%d,"output_tokens":%d}`, false, types.RelayFormatOpenAI, openai.OaiResponsesToChatHandler},
		{"responses to chat SSE", responseStream, `{"input_tokens":%d,"output_tokens":%d}`, true, types.RelayFormatOpenAI, openai.OaiResponsesToChatStreamHandler},
		{"chat to responses JSON", chat, `{"prompt_tokens":%d,"completion_tokens":%d}`, false, types.RelayFormatOpenAIResponses, openai.OaiChatToResponsesHandler},
		{"chat to responses SSE", chatStream, `{"prompt_tokens":%d,"completion_tokens":%d}`, true, types.RelayFormatOpenAIResponses, openai.OaiChatToResponsesStreamHandler},
		{"Claude JSON", claudeResponse, `{"input_tokens":%d,"output_tokens":%d}`, false, types.RelayFormatClaude, func(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
			return claude.ClaudeHandler(c, resp, info)
		}},
		{"Claude SSE", claudeStream, `{"input_tokens":%d,"output_tokens":%d}`, true, types.RelayFormatClaude, func(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
			return claude.ClaudeStreamHandler(c, resp, info)
		}},
		{"Gemini JSON", geminiResponse, `{"promptTokenCount":%d,"candidatesTokenCount":%d}`, false, types.RelayFormatGemini, gemini.GeminiTextGenerationHandler},
		{"Gemini SSE", "data: " + geminiResponse + "\n\n", `{"promptTokenCount":%d,"candidatesTokenCount":%d}`, true, types.RelayFormatGemini, gemini.GeminiTextGenerationStreamHandler},
		{"Cloudflare JSON", chat, `{"prompt_tokens":%d,"completion_tokens":%d}`, false, types.RelayFormatOpenAI, responseUsageHandler((&cloudflare.Adaptor{}).DoResponse)},
		{"Cloudflare SSE", chatStream, `{"prompt_tokens":%d,"completion_tokens":%d}`, true, types.RelayFormatOpenAI, responseUsageHandler((&cloudflare.Adaptor{}).DoResponse)},
		{"xAI JSON", chat, `{"prompt_tokens":%d,"completion_tokens":%d}`, false, types.RelayFormatOpenAI, responseUsageHandler((&xai.Adaptor{}).DoResponse)},
		{"xAI SSE", chatStream, `{"prompt_tokens":%d,"completion_tokens":%d}`, true, types.RelayFormatOpenAI, responseUsageHandler((&xai.Adaptor{}).DoResponse)},
		{"Tencent JSON", `{"Response":{"Choices":[{"Message":{"Content":"hello"},"FinishReason":"stop"}],"Usage":%s}}`, `{"PromptTokens":%d,"CompletionTokens":%d}`, false, types.RelayFormatOpenAI, responseUsageHandler((&tencent.Adaptor{}).DoResponse)},
		{"Tencent SSE", "data: {\"Choices\":[{\"Delta\":{\"Content\":\"hello\"},\"FinishReason\":\"stop\"}],\"Usage\":%s}\n\n", `{"PromptTokens":%d,"CompletionTokens":%d}`, true, types.RelayFormatOpenAI, responseUsageHandler((&tencent.Adaptor{}).DoResponse)},
		{"Cohere JSON", `{"text":"hello","meta":{"billed_units":%s}}`, `{"input_tokens":%d,"output_tokens":%d}`, false, types.RelayFormatOpenAI, responseUsageHandler((&cohere.Adaptor{}).DoResponse)},
		{"Dify JSON", `{"answer":"hello","metadata":{"usage":%s}}`, `{"prompt_tokens":%d,"completion_tokens":%d}`, false, types.RelayFormatOpenAI, responseUsageHandler((&dify.Adaptor{}).DoResponse)},
		{"Dify SSE", "data: {\"event\":\"message\",\"answer\":\"hello\"}\n\ndata: {\"event\":\"message_end\",\"metadata\":{\"usage\":%s}}\n\ndata: [DONE]\n\n", `{"prompt_tokens":%d,"completion_tokens":%d}`, true, types.RelayFormatOpenAI, responseUsageHandler((&dify.Adaptor{}).DoResponse)},
	} {
		for _, tc := range []struct {
			name          string
			input, output int
		}{
			{"actual usage", 17, 13},
			{"missing input", 0, 13},
			{"missing output", 17, 0},
			{"missing usage", 0, 0},
		} {
			t.Run(provider.name+"/"+tc.name, func(t *testing.T) {
				c, _ := gin.CreateTestContext(httptest.NewRecorder())
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
				common.SetContextKey(c, constant.ContextKeyOriginalModel, "gpt-4o-mini")
				request := &observedTokenRequest{GeneralOpenAIRequest: dto.GeneralOpenAIRequest{Model: "gpt-4o-mini", Prompt: "hello"}}
				info := &relaycommon.RelayInfo{
					ChannelMeta:     &relaycommon.ChannelMeta{ChannelType: constant.ChannelTypeOpenAI, UpstreamModelName: "gpt-4o-mini"},
					OriginModelName: "gpt-4o-mini", RelayMode: relayconstant.RelayModeChatCompletions,
					RelayFormat: provider.format, IsStream: provider.stream, DisablePing: true, StartTime: time.Now(), Request: request,
				}
				estimate, err := service.EstimateRequestToken(c, nil, info)
				require.NoError(t, err)
				assert.Zero(t, estimate)
				assert.Zero(t, info.GetEstimatePromptTokens())
				assert.Zero(t, request.counts)
				contentType := "application/json"
				if provider.stream {
					contentType = "text/event-stream"
				}
				body := fmt.Sprintf(provider.body, fmt.Sprintf(provider.usage, tc.input, tc.output))
				resp := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(tokenUsageReader{strings.NewReader(body), t, request})}
				usage, apiErr := provider.handle(c, info, resp)
				require.Nil(t, apiErr)
				require.NotNil(t, usage)
				wantInput, wantOutput := tc.input, tc.output
				if wantInput == 0 {
					wantInput = 1
					if provider.format == types.RelayFormatOpenAI {
						wantInput = 4
					}
					assert.Equal(t, 1, request.counts)
					assert.Equal(t, wantInput, info.GetFallbackPromptTokens())
					assert.Equal(t, 1, request.counts, "settlement must reuse the fallback")
				} else {
					assert.Zero(t, request.counts, "actual input usage must avoid local counting")
				}
				if wantOutput == 0 {
					wantOutput = 1
				}
				assert.Equal(t, wantInput, usage.PromptTokens)
				assert.Equal(t, wantOutput, usage.CompletionTokens)
				assert.Equal(t, wantInput+wantOutput, usage.TotalTokens)
				if provider.name == "responses SSE" {
					if tc.input == 0 || tc.output == 0 {
						require.NotNil(t, usage.BillingUsage, "native stream estimates must carry their source into settlement")
						assert.True(t, usage.BillingUsage.Estimated)
					}
					service.CaptureChannelTestLimitUsage(c, info, usage, nil)
					require.NotNil(t, info.AnalyticsBilling)
					if tc.input == 0 || tc.output == 0 {
						assert.Equal(t, "local_estimate", info.AnalyticsBilling.UsageSource)
						assert.Nil(t, info.AnalyticsBilling.BaseQuota, "estimated output must not settle actual supplier cost")
					} else {
						assert.Equal(t, "upstream", info.AnalyticsBilling.UsageSource)
						assert.NotNil(t, info.AnalyticsBilling.BaseQuota)
					}
				}
				if native, ok := usage.BillingUsage.CanonicalUsage(); ok {
					assert.Equal(t, wantInput, native.PromptTokens, "native settlement input must match")
					assert.Equal(t, wantOutput, native.CompletionTokens, "native settlement output must match")
				}
			})
		}
	}
}

func TestDeferredInputPreservesNativeUsage(t *testing.T) {
	for _, tc := range []struct {
		name   string
		usage  *dto.Usage
		input  int
		cached int
	}{
		{"Claude cache-only input", &dto.Usage{CompletionTokens: 3, BillingUsage: dto.NewClaudeMessagesBillingUsage(&dto.ClaudeUsage{OutputTokens: 3, CacheReadInputTokens: 17})}, 0, 17},
		{"Responses input alias", &dto.Usage{InputTokens: 17, CompletionTokens: 3, BillingUsage: dto.NewOpenAIResponsesBillingUsage(&dto.Usage{InputTokens: 17, OutputTokens: 3, InputTokensDetails: &dto.InputTokenDetails{CachedTokens: 5}})}, 17, 5},
		{"input derived from upstream total", &dto.Usage{TotalTokens: 20, CompletionTokens: 3, BillingUsage: dto.NewOpenAIChatBillingUsage(&dto.Usage{TotalTokens: 20, CompletionTokens: 3, PromptTokensDetails: dto.InputTokenDetails{CachedTokens: 5}})}, 17, 5},
	} {
		t.Run(tc.name, func(t *testing.T) {
			info := &relaycommon.RelayInfo{ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "gpt-4o-mini"}}
			info.SetPromptTokenFallback(func() int {
				t.Error("available native input must not trigger counting")
				return 999
			})
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			usage := service.CompleteTextUsage(c, info, tc.usage, "hello")
			assert.Equal(t, tc.input, usage.PromptTokens)
			assert.Equal(t, 3, usage.CompletionTokens)
			native, ok := usage.BillingUsage.CanonicalUsage()
			require.True(t, ok)
			assert.Equal(t, tc.input, native.PromptTokens)
			assert.Equal(t, tc.cached, native.PromptTokensDetails.CachedTokens)
			assert.False(t, usage.BillingUsage.Estimated)
		})
	}
}

func TestDeferredInputOnInterruptedResponses(t *testing.T) {
	previous := constant.CountToken
	constant.CountToken = false
	t.Cleanup(func() { constant.CountToken = previous })
	for _, output := range []string{"", "hello"} {
		t.Run(fmt.Sprintf("output=%q", output), func(t *testing.T) {
			c, _ := gin.CreateTestContext(httptest.NewRecorder())
			c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			common.SetContextKey(c, constant.ContextKeyOriginalModel, "gpt-4o-mini")
			request := &observedTokenRequest{GeneralOpenAIRequest: dto.GeneralOpenAIRequest{Prompt: "hello"}}
			info := &relaycommon.RelayInfo{Request: request, RelayFormat: types.RelayFormatOpenAIResponses, StreamStatus: relaycommon.NewStreamStatus(), ChannelMeta: &relaycommon.ChannelMeta{UpstreamModelName: "gpt-4o-mini"}}
			_, err := service.EstimateRequestToken(c, nil, info)
			require.NoError(t, err)
			accumulator := service.NewResponsesUsageAccumulator(info)
			if output != "" {
				accumulator.Observe(&dto.ResponsesStreamResponse{Type: "response.output_text.delta", Delta: output}, nil)
			}
			accumulator.Observe(&dto.ResponsesStreamResponse{Type: "response.failed"}, nil)
			assert.Zero(t, request.counts)
			usage := accumulator.Finish()
			if output == "" {
				assert.Zero(t, usage.TotalTokens, "failure before output must not charge")
				assert.Zero(t, request.counts)
			} else {
				assert.Equal(t, 1, usage.PromptTokens)
				assert.Equal(t, 1, usage.CompletionTokens)
				assert.Same(t, usage, accumulator.Finish())
				assert.Equal(t, 1, request.counts)
			}
		})
	}
}

func TestResponsesUsagePreservesReportedZeroOutput(t *testing.T) {
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	info := &relaycommon.RelayInfo{RelayFormat: types.RelayFormatOpenAIResponses, StreamStatus: relaycommon.NewStreamStatus()}
	info.SetPromptTokenFallback(func() int {
		t.Fatal("reported input must not trigger local counting")
		return 0
	})
	accumulator := service.NewResponsesUsageAccumulator(info)
	accumulator.Observe(&dto.ResponsesStreamResponse{Type: "response.completed", Response: &dto.OpenAIResponsesResponse{
		Usage: &dto.Usage{InputTokens: 17, OutputTokens: 0, TotalTokens: 17},
	}}, nil)
	usage := accumulator.Finish()
	assert.Equal(t, 17, usage.PromptTokens)
	assert.Zero(t, usage.CompletionTokens, "a response without generated content must preserve zero output")
	assert.Equal(t, 17, usage.TotalTokens)
	service.CaptureChannelTestLimitUsage(c, info, usage, nil)
	require.NotNil(t, info.AnalyticsBilling)
	assert.Equal(t, "upstream", info.AnalyticsBilling.UsageSource)
	assert.NotNil(t, info.AnalyticsBilling.BaseQuota)
}

func TestDeferredInputKeepsTextWhenImageCannotBeMeasured(t *testing.T) {
	previousCount, previousMedia := constant.CountToken, constant.GetMediaToken
	constant.CountToken, constant.GetMediaToken = false, true
	t.Cleanup(func() { constant.CountToken, constant.GetMediaToken = previousCount, previousMedia })
	c, _ := gin.CreateTestContext(httptest.NewRecorder())
	c.Request = httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	t.Cleanup(func() { service.CleanupFileSources(c) })
	common.SetContextKey(c, constant.ContextKeyOriginalModel, "gpt-4o")
	request := &observedTokenRequest{}
	require.NoError(t, common.Unmarshal([]byte(`{"model":"gpt-4o","messages":[{"role":"user","content":[{"type":"text","text":"hello"},{"type":"image_url","image_url":{"url":"data:image/png;base64,aW52YWxpZA==","detail":"high"}}]}]}`), &request.GeneralOpenAIRequest))
	info := &relaycommon.RelayInfo{Request: request, RelayFormat: types.RelayFormatOpenAI, IsStream: true}
	_, err := service.EstimateRequestToken(c, nil, info)
	require.NoError(t, err)
	assert.Zero(t, request.counts)
	usage := &dto.Usage{CompletionTokens: 1}
	require.True(t, service.FillMissingPromptTokens(info, usage))
	assert.Equal(t, 774, usage.PromptTokens, "9 text/framing tokens plus the 765-token unknown-size image estimate")
	assert.Equal(t, 775, usage.TotalTokens)
	assert.Equal(t, 1, request.counts)
	_, err = service.CountRequestToken(c, request.GetTokenCountMeta(), info)
	require.Error(t, err, "an explicit counting request must still report unreadable media")
}
