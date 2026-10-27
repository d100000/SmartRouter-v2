package openai

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/relay/channel/openrouter"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relay/helper"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/tokenkit"

	"github.com/gin-gonic/gin"
)

func sendStreamData(c *gin.Context, info *relaycommon.RelayInfo, data string, forceFormat bool, thinkToContent bool) error {
	if data == "" {
		return nil
	}

	if !forceFormat && !thinkToContent {
		return helper.StringData(c, data)
	}

	var lastStreamResponse dto.ChatCompletionsStreamResponse
	if err := common.UnmarshalJsonStr(data, &lastStreamResponse); err != nil {
		return err
	}

	if !thinkToContent {
		return helper.ObjectData(c, lastStreamResponse)
	}

	hasThinkingContent := false
	hasContent := false
	var thinkingContent strings.Builder
	for _, choice := range lastStreamResponse.Choices {
		if len(choice.Delta.GetReasoningContent()) > 0 {
			hasThinkingContent = true
			thinkingContent.WriteString(choice.Delta.GetReasoningContent())
		}
		if len(choice.Delta.GetContentString()) > 0 {
			hasContent = true
		}
	}

	// Handle think to content conversion
	if info.ThinkingContentInfo.IsFirstThinkingContent {
		if hasThinkingContent {
			response := lastStreamResponse.Copy()
			for i := range response.Choices {
				// send `think` tag with thinking content
				response.Choices[i].Delta.SetContentString("<think>\n" + thinkingContent.String())
				response.Choices[i].Delta.ReasoningContent = nil
				response.Choices[i].Delta.Reasoning = nil
			}
			info.ThinkingContentInfo.IsFirstThinkingContent = false
			info.ThinkingContentInfo.HasSentThinkingContent = true
			return helper.ObjectData(c, response)
		}
	}

	if lastStreamResponse.Choices == nil || len(lastStreamResponse.Choices) == 0 {
		return helper.ObjectData(c, lastStreamResponse)
	}

	// Process each choice
	for i, choice := range lastStreamResponse.Choices {
		// Handle transition from thinking to content
		// only send `</think>` tag when previous thinking content has been sent
		if hasContent && !info.ThinkingContentInfo.SendLastThinkingContent && info.ThinkingContentInfo.HasSentThinkingContent {
			response := lastStreamResponse.Copy()
			for j := range response.Choices {
				response.Choices[j].Delta.SetContentString("\n</think>\n")
				response.Choices[j].Delta.ReasoningContent = nil
				response.Choices[j].Delta.Reasoning = nil
			}
			info.ThinkingContentInfo.SendLastThinkingContent = true
			helper.ObjectData(c, response)
		}

		// Convert reasoning content to regular content if any
		if len(choice.Delta.GetReasoningContent()) > 0 {
			lastStreamResponse.Choices[i].Delta.SetContentString(choice.Delta.GetReasoningContent())
			lastStreamResponse.Choices[i].Delta.ReasoningContent = nil
			lastStreamResponse.Choices[i].Delta.Reasoning = nil
		} else if !hasThinkingContent && !hasContent {
			// flush thinking content
			lastStreamResponse.Choices[i].Delta.ReasoningContent = nil
			lastStreamResponse.Choices[i].Delta.Reasoning = nil
		}
	}

	return helper.ObjectData(c, lastStreamResponse)
}

func OaiStreamHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	if resp == nil || resp.Body == nil {
		logger.LogError(c, common.LogText("invalid response or response body"))
		return nil, types.NewOpenAIError(fmt.Errorf("invalid response"), types.ErrorCodeBadResponse, http.StatusInternalServerError)
	}

	defer service.CloseResponseBodyGracefully(resp)
	// Some compatible gateways return a JSON error body after accepting a
	// streaming request. Do not let the SSE scanner silently discard that body.
	if strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "json") {
		limit := helper.DefaultMaxScannerBufferSize
		if constant.StreamScannerMaxBufferMB > 0 {
			limit = constant.StreamScannerMaxBufferMB << 20
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, int64(limit)+1))
		var frame openAIStreamFrame
		var apiErr *types.NewAPIError
		switch {
		case err != nil:
			apiErr = types.NewOpenAIError(fmt.Errorf("%s: %w", i18n.T(c, i18n.MsgRelayStreamInvalidResponse), err), types.ErrorCodeReadResponseBodyFailed, http.StatusBadGateway, types.ErrOptionWithSkipRetry())
		case len(body) > limit:
			apiErr = types.NewOpenAIError(errors.New(i18n.T(c, i18n.MsgRelayStreamInvalidResponse)), types.ErrorCodeBadResponseBody, http.StatusBadGateway, types.ErrOptionWithSkipRetry())
		case common.Unmarshal(body, &frame) != nil:
			apiErr = types.NewOpenAIError(errors.New(i18n.T(c, i18n.MsgRelayStreamInvalidResponse)), types.ErrorCodeBadResponseBody, http.StatusBadGateway, types.ErrOptionWithSkipRetry())
		default:
			apiErr = frame.streamError(c, "")
		}
		hasActualUsage := streamHasActualUsage(frame.Usage)
		hasOutput := frame.hasJSONOutput(info)
		if apiErr == nil && !hasActualUsage && !hasOutput {
			apiErr = types.NewOpenAIError(errors.New(i18n.T(c, i18n.MsgRelayStreamEmpty)), types.ErrorCodeEmptyResponse, http.StatusBadGateway, types.ErrOptionWithSkipRetry())
		}
		if apiErr != nil {
			// A JSON error envelope is delivered as an error, not as the
			// incidental choices it might contain. Only real usage survives it.
			markOpenAIStreamFailure(c, info, apiErr, hasActualUsage, false)
			if info.StreamFailure.NoCharge {
				return nil, apiErr
			}
			helper.SetEventStreamHeaders(c)
			if info.RelayFormat == types.RelayFormatClaude {
				_ = helper.ClaudeData(c, dto.ClaudeResponse{Type: "error", Error: apiErr.ToClaudeError(common.UpstreamAddresses(c)...)})
			} else {
				_ = helper.ObjectData(c, gin.H{"error": apiErr.ToOpenAIError(common.UpstreamAddresses(c)...)})
			}
			applyUsagePostProcessing(info, frame.Usage, body)
			return frame.Usage, nil
		}
		service.CloseResponseBodyGracefully(resp)
		resp.Body = io.NopCloser(bytes.NewReader(body))
		return OpenaiHandler(c, info, resp)
	}

	model := info.UpstreamModelName
	var responseId string
	var createAt int64 = 0
	var systemFingerprint string
	var containStreamUsage bool
	var responseTextBuilder strings.Builder
	var toolCount int
	var usage = &dto.Usage{}
	var lastStreamData string
	var usageFrame string
	var streamErr *types.NewAPIError
	var hasActualUsage, hasNonTextOutput, lastHasOutput bool
	seenStreamToolCalls := make(map[string]struct{})
	var streamFunctionCallNames []string

	helper.StreamScannerHandler(c, resp, info, func(data string, sr *helper.StreamResult) {
		if lastStreamData != "" {
			if err := HandleStreamFormat(c, info, lastStreamData, info.ChannelSetting.ForceFormat, info.ChannelSetting.ThinkingToContent); err != nil {
				lastStreamData = ""
				streamErr = types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusBadGateway, types.ErrOptionWithSkipRetry())
				sanitizeStreamError(c, streamErr)
				sr.Stop(streamErr)
				return
			}
		}
		lastStreamData = ""
		lastHasOutput = false
		if strings.EqualFold(sr.Event, "ping") || strings.EqualFold(sr.Event, "heartbeat") {
			return
		}
		var frame openAIStreamFrame
		if err := common.UnmarshalJsonStr(data, &frame); err != nil && !strings.EqualFold(sr.Event, "error") {
			streamErr = types.NewOpenAIError(errors.New(i18n.T(c, i18n.MsgRelayStreamInvalidResponse)), types.ErrorCodeBadResponseBody, http.StatusBadGateway, types.ErrOptionWithSkipRetry())
		} else {
			if strings.EqualFold(sr.Event, "error") && frame.Message == "" {
				_ = common.UnmarshalJsonStr(data, &frame.Message)
			}
			actualUsage := streamHasActualUsage(frame.Usage)
			if actualUsage || !hasActualUsage && streamHasUsage(frame.Usage) {
				if actualUsage && !hasActualUsage {
					usage = &dto.Usage{}
				}
				usage = dto.MergeUsageNonZero(usage, frame.Usage)
				containStreamUsage = true
				usageFrame = data
				hasActualUsage = hasActualUsage || actualUsage
			}
			streamErr = frame.streamError(c, sr.Event)
		}
		if streamErr != nil {
			sanitizeStreamError(c, streamErr)
			sr.Stop(streamErr)
			return
		}
		lastStreamData = data
		observeStreamChoices(info, data, seenStreamToolCalls, &streamFunctionCallNames)
		textStart := responseTextBuilder.Len()
		if err := processTokenData(info, data, &responseTextBuilder, &toolCount); err != nil {
			lastStreamData = ""
			streamErr = types.NewOpenAIError(errors.New(i18n.T(c, i18n.MsgRelayStreamInvalidResponse)), types.ErrorCodeBadResponseBody, http.StatusBadGateway, types.ErrOptionWithSkipRetry())
			sr.Stop(streamErr)
			return
		}
		if info.RelayFormat == types.RelayFormatOpenAI && !info.ChannelSetting.ForceFormat && !info.ChannelSetting.ThinkingToContent {
			lastHasOutput = frame.appendAdditionalOutput(&responseTextBuilder)
			hasNonTextOutput = lastHasOutput || hasNonTextOutput
		}
		lastHasOutput = lastHasOutput || strings.TrimSpace(responseTextBuilder.String()[textStart:]) != ""
	})

	info.StreamStatus.RequireTerminal()

	// The last normal payload can carry the finish marker and response metadata.
	shouldSendLastResp := true
	if lastStreamData != "" {
		if err := handleLastResponse(lastStreamData, &responseId, &createAt, &systemFingerprint, &model,
			info, &shouldSendLastResp); err != nil && streamErr == nil {
			streamErr = types.NewOpenAIError(errors.New(i18n.T(c, i18n.MsgRelayStreamInvalidResponse)), types.ErrorCodeBadResponseBody, http.StatusBadGateway, types.ErrOptionWithSkipRetry())
		}
	}
	if lastHasOutput && info.RelayFormat == types.RelayFormatOpenAI && !info.ChannelSetting.ForceFormat && !info.ChannelSetting.ThinkingToContent {
		shouldSendLastResp = true
	}

	hasOutput := hasNonTextOutput || strings.TrimSpace(responseTextBuilder.String()) != ""
	outcome := info.StreamStatus.OutcomeSnapshot()
	if streamErr == nil && (!info.StreamStatus.IsNormalEnd() || info.StreamStatus.HasErrors() ||
		outcome.EndReason == relaycommon.StreamEndReasonEOF && outcome.Response != relaycommon.ResponseOutcomeCompleted) {
		streamErr = types.NewOpenAIError(errors.New(i18n.T(c, i18n.MsgRelayStreamInterrupted, map[string]any{"Reason": string(outcome.EndReason)})), types.ErrorCodeBadResponse, http.StatusBadGateway, types.ErrOptionWithSkipRetry())
	}
	if streamErr == nil && !hasActualUsage && !hasOutput {
		streamErr = types.NewOpenAIError(errors.New(i18n.T(c, i18n.MsgRelayStreamEmpty)), types.ErrorCodeEmptyResponse, http.StatusBadGateway, types.ErrOptionWithSkipRetry())
	}
	if streamErr != nil {
		markOpenAIStreamFailure(c, info, streamErr, hasActualUsage, hasOutput)
		if info.StreamFailure.NoCharge {
			return nil, streamErr
		}
	}

	if info.RelayFormat == types.RelayFormatOpenAI && shouldSendLastResp && lastStreamData != "" {
		if err := sendStreamData(c, info, lastStreamData, info.ChannelSetting.ForceFormat, info.ChannelSetting.ThinkingToContent); err != nil && streamErr == nil {
			streamErr = types.NewOpenAIError(err, types.ErrorCodeBadResponse, http.StatusBadGateway, types.ErrOptionWithSkipRetry())
			markOpenAIStreamFailure(c, info, streamErr, hasActualUsage, hasOutput)
		}
	}

	if !containStreamUsage {
		usage = service.ResponseText2Usage(c, responseTextBuilder.String(), info.UpstreamModelName, info.GetEstimatePromptTokens())
		usage.CompletionTokens += toolCount * 7
	}
	if usageFrame == "" {
		usageFrame = lastStreamData
	}

	applyUsagePostProcessing(info, usage, common.StringToByteSlice(usageFrame))

	for _, name := range streamFunctionCallNames {
		info.CountBillableToolCall(dto.BuildInCallFunctionCall, name)
	}

	if streamErr != nil {
		if info.RelayFormat == types.RelayFormatClaude {
			_ = helper.ClaudeData(c, dto.ClaudeResponse{Type: "error", Error: streamErr.ToClaudeError(common.UpstreamAddresses(c)...)})
		} else {
			_ = helper.ObjectData(c, gin.H{"error": streamErr.ToOpenAIError(common.UpstreamAddresses(c)...)})
		}
	} else {
		HandleFinalResponse(c, info, lastStreamData, responseId, createAt, model, systemFingerprint, usage, containStreamUsage)
	}

	return usage, nil
}

type openAIStreamFrame struct {
	Error      json.RawMessage `json:"error"`
	Type       string          `json:"type"`
	Message    string          `json:"message"`
	Code       any             `json:"code"`
	StatusCode int             `json:"status_code"`
	Usage      *dto.Usage      `json:"usage"`
	Choices    []struct {
		Text    string          `json:"text"`
		Message json.RawMessage `json:"message"`
		Delta   struct {
			Refusal      string                `json:"refusal"`
			FunctionCall *dto.FunctionResponse `json:"function_call"`
			Audio        struct {
				Data       string `json:"data"`
				Transcript string `json:"transcript"`
			} `json:"audio"`
			ToolCalls []struct {
				Custom struct {
					Name  string `json:"name"`
					Input string `json:"input"`
				} `json:"custom"`
			} `json:"tool_calls"`
		} `json:"delta"`
	} `json:"choices"`
}

func (frame *openAIStreamFrame) streamError(c *gin.Context, event string) *types.NewAPIError {
	hasError := len(frame.Error) > 0 && string(frame.Error) != "null" && string(frame.Error) != "false"
	if !hasError && !strings.EqualFold(event, "error") && frame.Type != "error" {
		return nil
	}
	upstreamError := types.OpenAIError{Message: frame.Message, Type: frame.Type, Code: frame.Code}
	status := frame.StatusCode
	if hasError {
		var details struct {
			Message    string `json:"message"`
			Type       string `json:"type"`
			Code       any    `json:"code"`
			Status     int    `json:"status"`
			StatusCode int    `json:"status_code"`
		}
		if err := common.Unmarshal(frame.Error, &details); err == nil {
			upstreamError = types.OpenAIError{Message: details.Message, Type: details.Type, Code: details.Code}
			status = max(details.Status, details.StatusCode)
		} else {
			_ = common.Unmarshal(frame.Error, &upstreamError.Message)
		}
	}
	if strings.TrimSpace(upstreamError.Message) == "" {
		upstreamError.Message = i18n.T(c, i18n.MsgRelayStreamUpstreamError)
	}
	upstreamError.Code = streamErrorIdentifier(upstreamError.Code, string(types.ErrorCodeBadResponse))
	upstreamError.Type = streamErrorIdentifier(upstreamError.Type, "upstream_error")
	if status < http.StatusBadRequest || status > 599 {
		status = http.StatusBadGateway
	}
	return types.WithOpenAIError(upstreamError, status, types.ErrOptionWithSkipRetry())
}

func streamErrorIdentifier(value any, fallback string) string {
	var identifier string
	switch scalar := value.(type) {
	case string:
		identifier = scalar
	case float64:
		identifier = fmt.Sprint(scalar)
	default:
		return fallback
	}
	if identifier == "" || len(identifier) > 64 || strings.HasPrefix(strings.ToLower(identifier), "sk-") || strings.HasPrefix(strings.ToLower(identifier), "sk_") {
		return fallback
	}
	for _, char := range identifier {
		if char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || char == '_' || char == '.' || char == '-' || char == ':' {
			continue
		}
		return fallback
	}
	return identifier
}

func (frame *openAIStreamFrame) appendAdditionalOutput(text *strings.Builder) bool {
	hasOutput := false
	for _, choice := range frame.Choices {
		text.WriteString(choice.Delta.Refusal)
		text.WriteString(choice.Delta.Audio.Transcript)
		if strings.TrimSpace(choice.Delta.Audio.Data) != "" {
			hasOutput = true
		}
		if choice.Delta.FunctionCall != nil {
			text.WriteString(choice.Delta.FunctionCall.Name)
			text.WriteString(choice.Delta.FunctionCall.Arguments)
		}
		for _, tool := range choice.Delta.ToolCalls {
			text.WriteString(tool.Custom.Name)
			text.WriteString(tool.Custom.Input)
		}
	}
	return hasOutput
}

func (frame *openAIStreamFrame) hasJSONOutput(info *relaycommon.RelayInfo) bool {
	for _, choice := range frame.Choices {
		var message dto.Message
		var extra struct {
			Refusal      string                `json:"refusal"`
			FunctionCall *dto.FunctionResponse `json:"function_call"`
			Audio        struct {
				Data       string `json:"data"`
				Transcript string `json:"transcript"`
			} `json:"audio"`
		}
		_ = common.Unmarshal(choice.Message, &message)
		_ = common.Unmarshal(choice.Message, &extra)
		if strings.TrimSpace(choice.Text+message.StringContent()+message.GetReasoningContent()) != "" || len(message.ParseToolCalls()) > 0 {
			return true
		}
		if info.RelayFormat != types.RelayFormatOpenAI || info.ChannelSetting.ForceFormat || info.ChannelSetting.ThinkingToContent {
			continue
		}
		if strings.TrimSpace(extra.Refusal+extra.Audio.Data+extra.Audio.Transcript) != "" || extra.FunctionCall != nil && strings.TrimSpace(extra.FunctionCall.Name+extra.FunctionCall.Arguments) != "" {
			return true
		}
	}
	return false
}

func streamHasUsage(usage *dto.Usage) bool {
	if usage == nil {
		return false
	}
	if canonical, ok := usage.BillingUsage.CanonicalUsage(); ok {
		usage = canonical
	}
	// The merge drops invalid negative counters and preserves positive cache,
	// audio and reasoning details, including usage with no ordinary tokens.
	return dto.HasOpenAIUsageTokens(dto.MergeUsageNonZero(nil, usage))
}

func streamHasActualUsage(usage *dto.Usage) bool {
	if usage == nil {
		return false
	}
	source := strings.ToLower(usage.UsageSource)
	if source == "local" || strings.Contains(source, "estimat") {
		return false
	}
	if usage.BillingUsage != nil {
		source = strings.ToLower(usage.BillingUsage.Source)
		if usage.BillingUsage.Estimated || source == "local" || strings.Contains(source, "estimat") {
			return false
		}
	}
	if canonical, ok := usage.BillingUsage.CanonicalUsage(); ok {
		usage = canonical
	}
	if usage.PromptTokens < 0 || usage.CompletionTokens < 0 || usage.InputTokens < 0 || usage.OutputTokens < 0 || usage.TotalTokens < 0 {
		return false
	}
	return streamHasUsage(usage)
}

func sanitizeStreamError(c *gin.Context, apiErr *types.NewAPIError) {
	message := apiErr.MaskSensitiveError(common.UpstreamAddresses(c)...)
	if len(message) > common.LocalLogContentLimit {
		message = strings.ToValidUTF8(message[:common.LocalLogContentLimit], "")
	}
	apiErr.SetMessage(message)
}

func markOpenAIStreamFailure(c *gin.Context, info *relaycommon.RelayInfo, apiErr *types.NewAPIError, actualUsage, hasOutput bool) {
	sanitizeStreamError(c, apiErr)
	info.StreamFailure = &relaycommon.StreamFailureInfo{
		Error: apiErr, HasActualUsage: actualUsage, HasEffectiveOutput: hasOutput,
		NoCharge: !actualUsage && !hasOutput,
	}
	if info.StreamStatus == nil {
		info.StreamStatus = relaycommon.NewStreamStatus()
		info.StreamStatus.SetEndReason(relaycommon.StreamEndReasonHandlerStop, apiErr)
	}
	info.StreamStatus.MarkFailed(string(apiErr.GetErrorCode()), apiErr.ToOpenAIError().Type, apiErr.StatusCode)
	if !info.StreamStatus.HasErrors() {
		info.StreamStatus.RecordError(apiErr.Error())
	}
	logger.LogError(c, common.LogText("stream request failed: code=%s, no_charge=%t, actual_usage=%t, effective_output=%t, message=%s", apiErr.GetErrorCode(), info.StreamFailure.NoCharge, actualUsage, hasOutput, apiErr.Error()))
}

// observeStreamChoices collects billable function call names and records the
// finish reason facts used by health sampling from one parsed chunk.
func observeStreamChoices(info *relaycommon.RelayInfo, data string, seen map[string]struct{}, names *[]string) {
	var streamResponse dto.ChatCompletionsStreamResponse
	if err := common.UnmarshalJsonStr(data, &streamResponse); err != nil {
		return
	}
	for _, choice := range streamResponse.Choices {
		if choice.FinishReason != nil && *choice.FinishReason != "" {
			if *choice.FinishReason == constant.FinishReasonContentFilter {
				info.PerformanceBusinessRejection = true
			}
			info.StreamStatus.MarkCompleted()
		}
		for i, tc := range choice.Delta.ToolCalls {
			name := strings.TrimSpace(tc.Function.Name)
			if name == "" {
				continue
			}
			toolIdx := i
			if tc.Index != nil {
				toolIdx = *tc.Index
			}
			fallbackKey := fmt.Sprintf("index\x00%d\x00%d\x00%s", choice.Index, toolIdx, name)
			activeKey := fmt.Sprintf("active\x00%d\x00%d\x00%s", choice.Index, toolIdx, name)
			callID := strings.TrimSpace(tc.ID)
			if callID != "" {
				idKey := fmt.Sprintf("id\x00%d\x00%s", choice.Index, callID)
				if _, ok := seen[idKey]; ok {
					continue
				}
				seen[idKey] = struct{}{}
				seen[activeKey] = struct{}{}
				if _, delayedID := seen[fallbackKey]; delayedID {
					delete(seen, fallbackKey)
					continue
				}
			} else {
				if _, ok := seen[fallbackKey]; ok {
					continue
				}
				if _, ok := seen[activeKey]; ok {
					continue
				}
				seen[fallbackKey] = struct{}{}
				seen[activeKey] = struct{}{}
			}
			*names = append(*names, name)
		}
	}
}

func OpenaiHandler(c *gin.Context, info *relaycommon.RelayInfo, resp *http.Response) (*dto.Usage, *types.NewAPIError) {
	defer service.CloseResponseBodyGracefully(resp)

	var simpleResponse dto.OpenAITextResponse
	responseBody, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeReadResponseBodyFailed, http.StatusInternalServerError)
	}
	logger.LogDebug(c, "upstream response body: %s", responseBody)
	// Unmarshal to simpleResponse
	if info.ChannelType == constant.ChannelTypeOpenRouter && info.ChannelOtherSettings.IsOpenRouterEnterprise() {
		// 尝试解析为 openrouter enterprise
		var enterpriseResponse openrouter.OpenRouterEnterpriseResponse
		err = common.Unmarshal(responseBody, &enterpriseResponse)
		if err != nil {
			return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
		}
		if enterpriseResponse.Success {
			responseBody = enterpriseResponse.Data
		} else {
			logger.LogError(c, common.LogText("openrouter enterprise response success=false, data: %s", enterpriseResponse.Data))
			return nil, types.NewOpenAIError(fmt.Errorf("openrouter response success=false"), types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
		}
	}

	err = common.Unmarshal(responseBody, &simpleResponse)
	if err != nil {
		return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
	}

	if oaiError := simpleResponse.GetOpenAIError(); oaiError != nil && oaiError.Type != "" {
		return nil, types.WithOpenAIError(*oaiError, resp.StatusCode)
	}

	info.ObserveResponseModel(simpleResponse.Model)
	for _, choice := range simpleResponse.Choices {
		if choice.FinishReason == constant.FinishReasonContentFilter {
			info.PerformanceBusinessRejection = true
			common.SetContextKey(c, constant.ContextKeyAdminRejectReason, "openai_finish_reason=content_filter")
			break
		}
	}

	for _, choice := range simpleResponse.Choices {
		for _, tc := range choice.Message.ParseToolCalls() {
			info.CountBillableToolCall(dto.BuildInCallFunctionCall, tc.Function.Name)
		}
	}
	info.ApplyVendorToolUsage(responseBody)

	forceFormat := false
	if info.ChannelSetting.ForceFormat {
		forceFormat = true
	}

	usageModified := false
	if simpleResponse.Usage.PromptTokens == 0 {
		common.SetContextKey(c, constant.ContextKeyLocalCountTokens, true)
		completionTokens := simpleResponse.Usage.CompletionTokens
		if completionTokens == 0 {
			for _, choice := range simpleResponse.Choices {
				ctkm := tokenkit.Count(info.UpstreamModelName, choice.Message.StringContent()+choice.Message.GetReasoningContent())
				completionTokens += ctkm
			}
		}
		fallbackUsage := &dto.Usage{
			PromptTokens:     info.GetEstimatePromptTokens(),
			CompletionTokens: completionTokens,
			TotalTokens:      info.GetEstimatePromptTokens() + completionTokens,
		}
		simpleResponse.Usage = *fallbackUsage
		usageModified = true
	}

	applyUsagePostProcessing(info, &simpleResponse.Usage, responseBody)

	switch info.RelayFormat {
	case types.RelayFormatOpenAI:
		if usageModified {
			var bodyMap map[string]any
			err = common.Unmarshal(responseBody, &bodyMap)
			if err != nil {
				return nil, types.NewOpenAIError(err, types.ErrorCodeBadResponseBody, http.StatusInternalServerError)
			}
			bodyMap["usage"] = simpleResponse.Usage
			responseBody, _ = common.Marshal(bodyMap)
		}
		if forceFormat {
			responseBody, err = common.Marshal(simpleResponse)
			if err != nil {
				return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
			}
		} else {
			break
		}
	case types.RelayFormatClaude:
		convertResult, err := service.ConvertResponse(c, info, types.RelayFormatClaude, &simpleResponse)
		if err != nil {
			return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
		}
		claudeRespStr, err := common.Marshal(convertResult.Value)
		if err != nil {
			return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
		}
		responseBody = claudeRespStr
	case types.RelayFormatGemini:
		convertResult, err := service.ConvertResponse(c, info, types.RelayFormatGemini, &simpleResponse)
		if err != nil {
			return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
		}
		geminiRespStr, err := common.Marshal(convertResult.Value)
		if err != nil {
			return nil, types.NewError(err, types.ErrorCodeBadResponseBody)
		}
		responseBody = geminiRespStr
	}

	service.IOCopyBytesGracefully(c, resp, responseBody)

	return &simpleResponse.Usage, nil
}
