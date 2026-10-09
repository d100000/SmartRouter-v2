package service

import (
	"math"
	"strconv"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	perfmetrics "github.com/QuantumNous/new-api/pkg/perf_metrics"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
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
	info.AnalyticsBilling = nil
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
	state := schedulingState(c)
	state.mu.Lock()
	classification := state.lastOutcome
	state.mu.Unlock()
	if classification == "" {
		classification = perfmetrics.ClassifyRelayOutcome(c.Request.Context(), info, apiErr)
	}
	attempt.Success = classification == perfmetrics.OutcomeSuccess
	attempt.Failure = classification == perfmetrics.OutcomeFailure
	if info.AnalyticsBilling != nil {
		copy := *info.AnalyticsBilling
		copy.RevenueUnitAmbiguous = info.AnalyticsRevenueUnitAmbiguous
		attempt.Billing = &copy
	}
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
		event.Status = "ready"
		event.FinishedAt = time.Now().Unix()
		event.Success, event.Failure, event.LatencyMS = attempt.Success, attempt.Failure, attempt.LatencyMS
		if attempt.CostRatio != nil && *attempt.CostRatio == 0 {
			event.CostKnown = true
		}
		if attempt.Billing != nil {
			billing := attempt.Billing
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

func overviewEvent(attempt *relaycommon.OverviewAttempt) model.OverviewAttemptEvent {
	tags, _ := common.Marshal(attempt.Tags)
	return model.OverviewAttemptEvent{ID: attempt.EventID, RequestID: attempt.RequestID, Day: attempt.StartedAt.UTC().Format("2006-01-02"), StartedAt: attempt.StartedAt.Unix(), ChannelID: attempt.ChannelID, ChannelName: attempt.ChannelName, GroupName: attempt.GroupName, ModelName: attempt.ModelName, CredentialID: attempt.CredentialID, CredentialName: attempt.CredentialName, CredentialVersionID: attempt.CredentialVersionID, BindingID: attempt.BindingID, SupplierID: attempt.SupplierID, SupplierName: attempt.SupplierName, OwnershipVersionID: attempt.OwnershipVersionID, CostVersionID: attempt.CostVersionID, CostSource: attempt.CostSource, CostRatio: attempt.CostRatio, CredentialCostVersionID: attempt.CredentialCostVersionID, ChannelCostVersionID: attempt.ChannelCostVersionID, TagsJSON: string(tags), BillingSource: attempt.BillingSource, QuotaPerUnit: attempt.QuotaPerUnit}
}
