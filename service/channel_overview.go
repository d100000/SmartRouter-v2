package service

import (
	"math"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	perfmetrics "github.com/QuantumNous/new-api/pkg/perf_metrics"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
)

func PrepareOverviewAttempt(c *gin.Context, info *relaycommon.RelayInfo, channel *model.Channel) {
	if info.IsChannelTest {
		return
	}
	index := 0
	if channel.ChannelInfo.IsMultiKey {
		index = common.GetContextKeyInt(c, constant.ContextKeyChannelMultiKeyIndex)
	}
	snapshot := channel.GetUpstreamCredentialSnapshot(index)
	requestID := info.RequestId
	if requestID == "" {
		requestID = c.GetString(common.RequestIdKey)
	}
	if requestID == "" {
		requestID = common.NewRequestId()
	}
	attempt := &relaycommon.OverviewAttempt{EventID: requestID + ":" + strconv.Itoa(info.RetryIndex), RequestID: requestID, ChannelID: channel.Id, ChannelName: channel.Name, GroupName: common.GetContextKeyString(c, constant.ContextKeyUsingGroup), ModelName: info.OriginModelName, CredentialID: snapshot.CredentialID, CredentialName: snapshot.Alias, CredentialVersionID: snapshot.CredentialVersionID, BindingID: snapshot.BindingID, SupplierID: snapshot.SupplierID, SupplierName: snapshot.SupplierName, OwnershipVersionID: snapshot.OwnershipVersionID, CostVersionID: snapshot.CostVersionID, CostSource: snapshot.CostSource, BillingSource: info.BillingSource, CredentialCostVersionID: snapshot.CredentialCostVersionID, ChannelCostVersionID: snapshot.ChannelCostVersionID, Tags: append([]string(nil), snapshot.Tags...), QuotaPerUnit: common.QuotaPerUnit}
	if snapshot.EffectiveCostRatio != nil {
		value := *snapshot.EffectiveCostRatio
		attempt.CostRatio = &value
	}
	if channel.OwnerSupplierID != "" {
		if limit := info.ChannelLimitAttempt; limit != nil && limit.ChannelID == channel.Id {
			attempt.CostRatio, attempt.CostSource, attempt.CostVersionID = limit.CostRatio, limit.CostSource, limit.CostVersionID
			attempt.CredentialCostVersionID, attempt.ChannelCostVersionID = limit.CredentialCostVersionID, limit.ChannelCostVersionID
		} else if account, err := model.GetSupplierAccount(channel.OwnerSupplierID); err == nil {
			if cost, err := model.GetChannelProcurementSnapshot(channel, index, account); err == nil {
				attempt.CostRatio, attempt.CostSource, attempt.CostVersionID = cost.EffectiveCostRatio, cost.CostSource, cost.CostVersionID
				attempt.CredentialCostVersionID, attempt.ChannelCostVersionID = cost.CredentialCostVersionID, cost.ChannelCostVersionID
			} else {
				attempt.CostRatio = nil
			}
		} else {
			attempt.CostRatio = nil
		}
	}
	attempt.AuthorizedSupplierID = channel.OwnerSupplierID
	attempt.SupplierReference = uuid.NewString()
	attempt.IsStream, attempt.CallStage = info.IsStream, "inference"
	switch info.RelayMode {
	case relayconstant.RelayModeEmbeddings:
		attempt.CallStage = "embedding"
	case relayconstant.RelayModeRerank:
		attempt.CallStage = "rerank"
	case relayconstant.RelayModeImagesGenerations, relayconstant.RelayModeImagesEdits:
		attempt.CallStage = "image"
	case relayconstant.RelayModeAudioSpeech, relayconstant.RelayModeAudioTranscription, relayconstant.RelayModeAudioTranslation:
		attempt.CallStage = "audio"
	}
	if info.TaskRelayInfo != nil {
		attempt.CallStage = "task_submit"
	}
	info.AnalyticsBilling = nil
	info.AnalyticsMediaUsage = relaycommon.OverviewMediaUsage{}
	info.AnalyticsRealtimeRevenueQuota = 0
	info.AnalyticsRevenueUnitAmbiguous = false
	info.OverviewAttempt = attempt
	info.OverviewAttempts = append(info.OverviewAttempts, attempt)
}

// Transport calls this after local request/header validation, immediately
// before dispatch. No secret, user prompt, client token, or key preview enters
// the accounting event. A local validation failure is not an upstream call.
func MarkOverviewUpstreamSent(info *relaycommon.RelayInfo) {
	if info == nil || info.OverviewAttempt == nil || info.OverviewAttempt.Sent {
		return
	}
	attempt := info.OverviewAttempt
	attempt.Sent, attempt.StartedAt = true, time.Now().UTC()
	if upstream := info.GetUpstreamModelName(); upstream != "" {
		attempt.ModelName = upstream
	}
	event := overviewEvent(attempt)
	event.Status = "pending"
	if err := model.BeginOverviewAttempt(&event); err != nil {
		common.SysError("channel overview capture failed: " + err.Error())
	}
	attempt.DispatchedAt = time.Now()
}

func FinishOverviewAttempt(c *gin.Context, info *relaycommon.RelayInfo, apiErr *types.NewAPIError) {
	if info == nil || info.OverviewAttempt == nil {
		return
	}
	attempt := info.OverviewAttempt
	if !attempt.Sent || attempt.Finished {
		return
	}
	attempt.Finished = true
	attempt.LatencyMS = max(0, time.Since(attempt.DispatchedAt).Milliseconds())
	if info.IsStream && info.FirstResponseTime.After(attempt.DispatchedAt) {
		value := info.FirstResponseTime.Sub(attempt.DispatchedAt).Milliseconds()
		attempt.FirstTokenMS = &value
	}
	state := schedulingState(c)
	state.mu.Lock()
	classification := state.lastOutcome
	state.mu.Unlock()
	if classification == "" {
		classification = perfmetrics.ClassifyRelayOutcome(c.Request.Context(), info, apiErr)
	}
	attempt.Success = classification == perfmetrics.OutcomeSuccess
	attempt.Failure = classification == perfmetrics.OutcomeFailure
	if apiErr != nil {
		attempt.UpstreamStatusCode = apiErr.StatusCode
	}
	attempt.UpstreamRequestID = safeOverviewUpstreamRequestID(c, info)
	if info.AnalyticsBilling != nil {
		copy := *info.AnalyticsBilling
		copy.RevenueUnitAmbiguous = info.AnalyticsRevenueUnitAmbiguous
		attempt.Billing = &copy
	}
	if info.AnalyticsMediaUsage.ImageCount != nil || info.AnalyticsMediaUsage.AudioSeconds != nil {
		if attempt.Billing == nil {
			attempt.Billing = &relaycommon.OverviewBillingSnapshot{}
		}
		attempt.Billing.OverviewMediaUsage = info.AnalyticsMediaUsage
	}
}

// Vendor correlation identifiers are untrusted response data. Keep only a
// bounded identifier, excluding credentials and the platform's global request
// identity; an omitted value is safer than storing a modified secret or body.
func safeOverviewUpstreamRequestID(c *gin.Context, info *relaycommon.RelayInfo) string {
	if c == nil || info == nil {
		return ""
	}
	value := c.GetString(common.UpstreamRequestIdKey)
	if len(value) == 0 || len(value) > 128 {
		return ""
	}
	for index := range len(value) {
		ch := value[index]
		alphaNumeric := ch >= 'a' && ch <= 'z' || ch >= 'A' && ch <= 'Z' || ch >= '0' && ch <= '9'
		if !alphaNumeric && (index == 0 || ch != '_' && ch != '-' && ch != '.' && ch != ':') {
			return ""
		}
	}
	forbidden := []string{info.TokenKey, common.GetContextKeyString(c, constant.ContextKeyTokenKey), common.GetContextKeyString(c, constant.ContextKeyChannelKey), info.RequestId, c.GetString(common.RequestIdKey)}
	if info.ChannelMeta != nil {
		forbidden = append(forbidden, info.ApiKey)
	}
	if info.OverviewAttempt != nil {
		forbidden = append(forbidden, info.OverviewAttempt.RequestID)
	}
	if c.Request != nil {
		authorization := strings.TrimSpace(c.Request.Header.Get("Authorization"))
		if scheme, credential, ok := strings.Cut(authorization, " "); ok && strings.EqualFold(scheme, "Bearer") {
			authorization = strings.TrimSpace(credential)
		}
		forbidden = append(forbidden, authorization, c.Request.Header.Get("X-Api-Key"), c.Request.Header.Get("X-Goog-Api-Key"), c.Query("key"))
	}
	for _, secret := range forbidden {
		if secret != "" && strings.Contains(value, secret) {
			return ""
		}
	}
	return value
}

// Costs are attributed to every real attempt; income belongs only to the
// attempt that settled it. Completeness covers the whole request, including
// retries outside the object selected on the report page.
func CompleteOverviewRelay(info *relaycommon.RelayInfo) {
	if info == nil {
		return
	}
	events := make([]model.OverviewAttemptEvent, 0, len(info.OverviewAttempts))
	complete := true
	incomeConfirmed := false
	for _, attempt := range info.OverviewAttempts {
		if !attempt.Sent {
			continue
		}
		event := overviewEvent(attempt)
		event.UpstreamRequestID = attempt.UpstreamRequestID
		event.UpstreamStatusCode = attempt.UpstreamStatusCode
		event.Status = "ready"
		event.FinishedAt = time.Now().Unix()
		event.Success, event.Failure, event.LatencyMS = attempt.Success, attempt.Failure, attempt.LatencyMS
		if attempt.CostRatio != nil && *attempt.CostRatio == 0 {
			event.CostKnown = true
		}
		if attempt.Billing != nil {
			billing := attempt.Billing
			event.ImageCount, event.AudioSeconds = billing.ImageCount, billing.AudioSeconds
			event.ImageUsageSource, event.AudioUsageSource = billing.ImageUsageSource, billing.AudioUsageSource
			event.InputTokens, event.OutputTokens, event.UsageSource = billing.InputTokens, billing.OutputTokens, billing.UsageSource
			event.CacheUsageKnown = billing.CacheUsageKnown
			event.CacheInputTokens = billing.CacheInputTokens
			event.CacheReadTokens = billing.CacheReadTokens
			event.CacheWriteTokens = billing.CacheWriteTokens
			event.BaseQuota = billing.BaseQuota
			if billing.QuotaPerUnit > 0 {
				event.QuotaPerUnit = billing.QuotaPerUnit
			}
			event.RawRevenueQuota = billing.RevenueQuota
			event.RevenueUnitAmbiguous = billing.RevenueUnitAmbiguous
			if !billing.RevenueUnitAmbiguous {
				event.RevenueQuota = billing.RevenueQuota
			} else {
				complete = false
			}
			if billing.RevenueConfirmed && !billing.RevenueUnitAmbiguous {
				incomeConfirmed = true
			}
			if billing.BaseQuota != nil && attempt.CostRatio != nil && !math.IsNaN(*billing.BaseQuota) && !math.IsInf(*billing.BaseQuota, 0) && *billing.BaseQuota >= 0 && !math.IsNaN(*attempt.CostRatio) && !math.IsInf(*attempt.CostRatio, 0) && *attempt.CostRatio >= 0 {
				cost := decimal.NewFromFloat(*billing.BaseQuota).Mul(decimal.NewFromFloat(*attempt.CostRatio))
				value := cost.InexactFloat64()
				if value >= 0 && value <= float64(common.MaxQuota) && !math.IsNaN(value) && !math.IsInf(value, 0) {
					// Procurement analysis retains fractions of one quota unit;
					// this does not change the user's integer billing amount.
					event.CostQuota = value
					event.CostKnown = true
				}
			}
			if !billing.RevenueConfirmed {
				complete = false
			}
		}
		if event.QuotaPerUnit <= 0 {
			complete = false
		}
		if !event.CostKnown {
			complete = false
		}
		events = append(events, event)
	}
	if len(events) == 0 {
		return
	}
	// Final request count is recorded once even when no income was confirmed.
	events[len(events)-1].IsFinal = true
	events[len(events)-1].RevenueConfirmed = incomeConfirmed
	complete = complete && incomeConfirmed
	for i := range events {
		events[i].RequestComplete = complete
	}
	if err := model.CompleteOverviewRequest(events); err != nil {
		common.SysError("channel overview completion failed: " + err.Error())
	}
}

// Cache analysis uses actual upstream counts, never the local token estimate or
// the input categories adjusted for pricing. In particular, cache-write TTLs
// change prices but do not multiply the number of tokens observed here.
func captureOverviewCacheUsage(snapshot *relaycommon.OverviewBillingSnapshot, info *relaycommon.RelayInfo, usage *dto.Usage, localEstimate bool) {
	if snapshot == nil || usage == nil || localEstimate || (usage.BillingUsage != nil && usage.BillingUsage.Estimated) {
		return
	}
	if usage.BillingUsage != nil {
		usage = effectiveBillingUsage(usage)
	} else if usageSemanticFromUsage(info, usage) != dto.BillingUsageSemanticAnthropic {
		// The same native normalizer handles Responses' input_tokens_details
		// and compatible providers' prompt_cache_hit_tokens aliases.
		if native := dto.NewOpenAIChatBillingUsage(usage); native != nil {
			usage, _ = native.CanonicalUsage()
		}
	}
	input, read := int64(usage.PromptTokens), int64(usage.PromptTokensDetails.CachedTokens)
	creation := usage.PromptTokensDetails
	for _, count := range []int{usage.PromptTokens, creation.CachedTokens, creation.CachedCreationTokens, creation.CacheWriteTokens, creation.CacheCreationInputTokens, usage.ClaudeCacheCreation5mTokens, usage.ClaudeCacheCreation1hTokens} {
		if count < 0 || int64(count) > int64(common.MaxQuota) {
			return
		}
	}
	write := int64(creation.CacheCreationTokensTotal())
	if detail := creation.CacheWriteTokensDetails; detail != nil {
		if detail.TTL30mTokens < 0 || detail.TTL12hTokens < 0 || int64(detail.TTL30mTokens) > int64(common.MaxQuota) || int64(detail.TTL12hTokens) > int64(common.MaxQuota) {
			return
		}
		split := int64(detail.TTL30mTokens) + int64(detail.TTL12hTokens)
		if write > 0 && split > write {
			return
		}
		write = max(write, split)
	}
	if usageSemanticFromUsage(info, usage) == dto.BillingUsageSemanticAnthropic {
		split := int64(usage.ClaudeCacheCreation5mTokens) + int64(usage.ClaudeCacheCreation1hTokens)
		if write > 0 && split > write {
			return
		}
		write = max(write, split)
		input += read + write
	}
	if input > int64(common.MaxQuota) || write < 0 || write > int64(common.MaxQuota) || read > input {
		return
	}
	// An empty usage object carries no evidence that input tokens were measured.
	if input == 0 && (usage.CompletionTokens <= 0 || write > 0) {
		return
	}
	snapshot.CacheUsageKnown = true
	snapshot.CacheInputTokens, snapshot.CacheReadTokens, snapshot.CacheWriteTokens = input, read, write
}

func overviewEvent(attempt *relaycommon.OverviewAttempt) model.OverviewAttemptEvent {
	tags, _ := common.Marshal(attempt.Tags)
	return model.OverviewAttemptEvent{CallStage: attempt.CallStage, IsStream: attempt.IsStream, FirstTokenMS: attempt.FirstTokenMS, AuthorizedSupplierID: attempt.AuthorizedSupplierID, SupplierReference: attempt.SupplierReference, ID: attempt.EventID, RequestID: attempt.RequestID, Day: attempt.StartedAt.UTC().Format("2006-01-02"), StartedAt: attempt.StartedAt.Unix(), ChannelID: attempt.ChannelID, ChannelName: attempt.ChannelName, GroupName: attempt.GroupName, ModelName: attempt.ModelName, CredentialID: attempt.CredentialID, CredentialName: attempt.CredentialName, CredentialVersionID: attempt.CredentialVersionID, BindingID: attempt.BindingID, SupplierID: attempt.SupplierID, SupplierName: attempt.SupplierName, OwnershipVersionID: attempt.OwnershipVersionID, CostVersionID: attempt.CostVersionID, CostSource: attempt.CostSource, CostRatio: attempt.CostRatio, CredentialCostVersionID: attempt.CredentialCostVersionID, ChannelCostVersionID: attempt.ChannelCostVersionID, TagsJSON: string(tags), BillingSource: attempt.BillingSource, QuotaPerUnit: attempt.QuotaPerUnit}
}
