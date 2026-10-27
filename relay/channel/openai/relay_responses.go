package openai

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/logger"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func OaiResponsesHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	defer service.CloseResponseBodyGracefully(resp)

	// read response body
	var responsesResponse dto.OpenAIResponsesResponse
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeReadResponseBodyFailed, http.StatusInternalServerError)
	}
	err = common.Unmarshal(responseBody, &responsesResponse)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}
	if oaiError := responsesResponse.GetOpenAIError(); oaiError != nil && oaiError.Type != "" {
		return nil, types.WithOpenAIError(*oaiError, resp.StatusCode)
	}

	info.ObserveResponseModel(responsesResponse.Model)
	responseBody = rewriteSGLangResponsesCreatedAt(info, responseBody, "created_at", responsesResponse.CreatedAt)

	// 写入新的 response body
	service.IOCopyBytesGracefully(c, resp, responseBody)

	// compute usage
	usage := &dto.Usage{}
	service.ApplyResponsesUsage(usage, responsesResponse.Usage)
	// Count actual tool invocations from Output (not tool declarations).
	for _, output := range responsesResponse.Output {
		switch output.Type {
		case dto.BuildInCallWebSearchCall:
			info.CountBillableToolCall(dto.BuildInCallWebSearchCall, "")
		case dto.BuildInCallFileSearchCall:
			info.CountBillableToolCall(dto.BuildInCallFileSearchCall, "")
		case dto.BuildInCallFunctionCall:
			info.CountBillableToolCall(dto.BuildInCallFunctionCall, output.Name)
		}
	}
	info.ApplyVendorToolUsage(responseBody)

	imageCounter := &relaycommon.ImageGenerationCallCounter{}
	for i := range responsesResponse.Output {
		imageCounter.Observe(&responsesResponse.Output[i], &i)
	}
	imageCounter.Commit(info)

	return usage, nil
}

func OaiResponsesStreamHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	if resp == nil || resp.Body == nil {
		logger.LogError(c, common.LogText("invalid response or response body"))
		return nil, types.NewError(fmt.Errorf("invalid response"), types.ErrorCodeBadResponse)
	}

	defer service.CloseResponseBodyGracefully(resp)

	accumulator := service.NewResponsesUsageAccumulator(info)
	var startBuffer *helper.StreamStartBuffer
	if info.ResponsesRetryEligible && strings.HasPrefix(resp.Header.Get("Content-Type"), "text/event-stream") {
		startBuffer = &helper.StreamStartBuffer{}
	}
	var streamErr *types.NewAPIError
	var retryable, protocolFailure, hasActualUsage, hasOutput, errorDelivered bool
	// Initialization is observed only when the attempt is retained. A discarded
	// attempt never touches the billing accumulator or calls Finish/settlement.
	var initialization []dto.ResponsesStreamResponse

	helper.StreamScannerHandler(c, resp, info, func(data string, sr *helper.StreamResult) {

		// 检查当前数据是否包含 completed 状态和 usage 信息
		var streamResponse dto.ResponsesStreamResponse
		if err := common.UnmarshalJsonStr(data, &streamResponse); err != nil {
			logger.LogError(c, common.LogText("failed to unmarshal stream response: %s", err.Error()))
			streamErr = types.NewOpenAIError(errors.New(i18n.T(c, i18n.MsgRelayStreamInvalidResponse)), types.ErrorCodeBadResponseBody, http.StatusBadGateway, types.ErrOptionWithSkipRetry())
			_ = startBuffer.Flush(c)
			sr.Error(err)
			return
		}
		if streamResponse.Response != nil {
			data = string(rewriteSGLangResponsesCreatedAt(info, []byte(data), "response.created_at", streamResponse.Response.CreatedAt))
		}
		frameErr := responsesStreamError(c, data, sr.Event)
		knownEvent := sr.Event == "" || sr.Event == streamResponse.Type || sr.Event == "error" && streamResponse.Type == ""
		noActivity := startBuffer.CanRetry(c) && knownEvent && responsesEventHasNoActivity(data)
		knownFailure := streamResponse.Type == "error" || streamResponse.Type == "response.error" || streamResponse.Type == "response.failed" || sr.Event == "error" && streamResponse.Type == ""
		if frameErr != nil && knownFailure && frameErr.GetErrorCode() == "server_is_overloaded" && noActivity &&
			!hasActualUsage && !hasOutput && !info.StreamStatus.HasErrors() && startBuffer.Discard(c) {
			streamErr = types.WithOpenAIError(frameErr.ToOpenAIError(), http.StatusServiceUnavailable)
			retryable = true
			initialization = nil
			sr.Stop(streamErr)
			return
		}
		if frameErr == nil && noActivity && streamResponse.Response != nil &&
			(streamResponse.Type == "response.created" || streamResponse.Type == "response.queued" || streamResponse.Type == "response.in_progress") {
			buffered, err := startBuffer.Append(c, streamResponse.Type, data)
			if err != nil {
				sr.Stop(err)
				return
			}
			if buffered {
				initialization = append(initialization, streamResponse)
				return
			}
		}
		if err := startBuffer.Flush(c); err != nil {
			sr.Stop(err)
			return
		}
		for i := range initialization {
			accumulator.Observe(&initialization[i], nil)
		}
		initialization = nil
		if streamResponse.Response != nil {
			hasActualUsage = hasActualUsage || dto.HasOpenAIUsageTokens(streamResponse.Response.Usage)
		}
		hasOutput = hasOutput || responsesEventHasOutput(&streamResponse)
		accumulator.Observe(&streamResponse, common.StringToByteSlice(data))
		if frameErr != nil {
			streamErr = frameErr
			protocolFailure = true
			// A retained stream keeps its original protocol error exactly once.
			// Returning an uncharged failure still drives the controller refund.
			if err := helper.ResponseChunkData(c, streamResponse, data); err == nil {
				errorDelivered = true
			} else {
				sr.Error(err)
			}
			sr.Stop(streamErr)
			return
		}
		if err := helper.ResponseChunkData(c, streamResponse, data); err != nil {
			sr.Stop(err)
			return
		}
		if (streamResponse.Type == "response.completed" || streamResponse.Type == "response.done") &&
			streamResponse.Response != nil && info.StreamStatus.ResponseOutcome() == string(relaycommon.ResponseOutcomeCompleted) {
			sr.Done()
		}
	}, startBuffer)

	common.SetContextKey(c, constant.ContextKeyResponseStreamStatus, info.StreamStatus)
	info.StreamStatus.RequireTerminal()
	for i := range initialization {
		accumulator.Observe(&initialization[i], nil)
	}
	if streamErr == nil && (!info.StreamStatus.IsNormalEnd() || info.StreamStatus.HasErrors() ||
		info.StreamStatus.ResponseOutcome() == "") {
		streamErr = types.NewOpenAIError(errors.New(i18n.T(c, i18n.MsgRelayStreamInterrupted, map[string]any{"Reason": string(info.StreamStatus.EndReason)})), types.ErrorCodeBadResponse, http.StatusBadGateway, types.ErrOptionWithSkipRetry())
	}
	if streamErr != nil {
		if protocolFailure || retryable {
			markOpenAIStreamFailure(c, info, streamErr, hasActualUsage, hasOutput)
		} else {
			// A transport interruption is not a protocol response.failed event.
			// Keep the original completion/cancellation outcome for health metrics.
			sanitizeStreamError(c, streamErr)
			info.StreamFailure = &relaycommon.StreamFailureInfo{Error: streamErr, HasActualUsage: hasActualUsage,
				HasEffectiveOutput: hasOutput, NoCharge: !hasActualUsage && !hasOutput}
			if !info.StreamStatus.HasErrors() {
				info.StreamStatus.RecordError(streamErr.Error())
			}
		}
		info.StreamFailure.RetryableOverload = retryable
		info.StreamFailure.ErrorDelivered = errorDelivered
		info.StreamFailure.RetryAfter = responsesRetryAfter(resp.Header.Get("Retry-After"))
		if info.StreamFailure.NoCharge {
			return nil, streamErr
		}
	}
	return accumulator.Finish(), nil
}

func rewriteSGLangResponsesCreatedAt(info *relaycommon.RelayInfo, payload []byte, path string, createdAt dto.IntValue) []byte {
	if info.GetChannelType() != constant.ChannelTypeSGLang {
		return payload
	}
	if !gjson.GetBytes(payload, path).Exists() {
		return payload
	}
	patched, err := sjson.SetBytes(payload, path, int(createdAt))
	if err != nil {
		return payload
	}
	return patched
}
