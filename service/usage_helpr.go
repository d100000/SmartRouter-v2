package service

import (
	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/tokenkit"
	"github.com/gin-gonic/gin"
)

//func GetPromptTokens(textRequest dto.GeneralOpenAIRequest, relayMode int) (int, error) {
//	switch relayMode {
//	case constant.RelayModeChatCompletions:
//		return CountTokenMessages(textRequest.Messages, textRequest.Model)
//	case constant.RelayModeCompletions:
//		return CountTokenInput(textRequest.Prompt, textRequest.Model), nil
//	case constant.RelayModeModerations:
//		return CountTokenInput(textRequest.Input, textRequest.Model), nil
//	}
//	return 0, errors.New("unknown relay mode")
//}

func ResponseText2Usage(c *gin.Context, responseText string, modeName string, promptTokens int) *dto.Usage {
	common.SetContextKey(c, constant.ContextKeyLocalCountTokens, true)
	usage := &dto.Usage{}
	usage.PromptTokens = promptTokens
	usage.CompletionTokens = tokenkit.Estimate(modeName, responseText)
	usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	return usage
}

func ValidUsage(usage *dto.Usage) bool {
	return usage != nil && (usage.PromptTokens != 0 || usage.CompletionTokens != 0)
}

// FillMissingPromptTokens runs only after the upstream response has ended.
// Native billing usage wins over converted fields, including Claude's valid
// zero uncached input when its entire prompt came from cache.
func FillMissingPromptTokens(info *relaycommon.RelayInfo, usage *dto.Usage) bool {
	if usage == nil {
		return false
	}
	if native, ok := usageFromBillingUsage(usage); ok {
		if native.PromptTokens != 0 || native.UsageSemantic == "anthropic" &&
			(native.PromptTokensDetails.CachedTokens != 0 || native.PromptTokensDetails.CacheCreationTokensTotal() != 0) {
			if usage.PromptTokens == 0 && native.PromptTokens > 0 {
				usage.PromptTokens = native.PromptTokens
				usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
				return true
			}
			return false
		}
	}
	if usage.PromptTokens != 0 || usage.UsageSemantic == "anthropic" &&
		(usage.PromptTokensDetails.CachedTokens != 0 || usage.PromptTokensDetails.CacheCreationTokensTotal() != 0) {
		return false
	}
	promptTokens := usage.InputTokens
	if promptTokens == 0 && usage.TotalTokens > usage.CompletionTokens {
		promptTokens = usage.TotalTokens - usage.CompletionTokens
	}
	estimated := promptTokens == 0
	if estimated {
		promptTokens = info.GetFallbackPromptTokens()
	}
	if promptTokens <= 0 {
		return false
	}
	usage.PromptTokens = promptTokens
	usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	if estimated {
		usage.UsageSource = "local"
	}
	// Settlement prefers the native sidecar. Keep its cache and modality
	// details and update the missing input there as well as in the display DTO.
	if billing := dto.CloneBillingUsage(usage.BillingUsage); billing != nil {
		billing.Estimated = billing.Estimated || estimated
		switch {
		case billing.OpenAIUsage != nil:
			billing.OpenAIUsage.PromptTokens = promptTokens
			billing.OpenAIUsage.InputTokens = promptTokens
			billing.OpenAIUsage.TotalTokens = promptTokens + max(billing.OpenAIUsage.CompletionTokens, billing.OpenAIUsage.OutputTokens)
		case billing.ClaudeUsage != nil:
			billing.ClaudeUsage.InputTokens = promptTokens
		case billing.GeminiUsageMetadata != nil:
			metadata := billing.GeminiUsageMetadata
			metadata.PromptTokenCount = max(promptTokens-metadata.ToolUsePromptTokenCount, 0)
			metadata.TotalTokenCount = promptTokens + metadata.CandidatesTokenCount + metadata.ThoughtsTokenCount
		}
		usage.BillingUsage = billing
	}
	return true
}

// CompleteTextUsage fills only missing quantities after a converted response
// has finished. Preserve native usage and cache details used by settlement.
func CompleteTextUsage(c *gin.Context, info *relaycommon.RelayInfo, usage *dto.Usage, output string) *dto.Usage {
	if usage == nil {
		usage = &dto.Usage{}
	}
	if usage.CompletionTokens == 0 {
		usage.CompletionTokens = usage.OutputTokens
		if native, ok := usageFromBillingUsage(usage); ok && native.CompletionTokens > 0 {
			usage.CompletionTokens = native.CompletionTokens
		}
		if usage.CompletionTokens == 0 && output != "" {
			usage.CompletionTokens = tokenkit.Estimate(info.UpstreamModelName, output)
			usage.BillingUsage = dto.CloneBillingUsageWithEstimatedCompletion(usage.BillingUsage, usage.CompletionTokens)
			common.SetContextKey(c, constant.ContextKeyLocalCountTokens, true)
		}
	}
	FillMissingPromptTokens(info, usage)
	if usage.UsageSource == "local" {
		common.SetContextKey(c, constant.ContextKeyLocalCountTokens, true)
	}
	usage.TotalTokens = usage.PromptTokens + usage.CompletionTokens
	return usage
}
