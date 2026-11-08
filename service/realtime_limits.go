package service

import (
	"math"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// A connection can reference context unavailable in the current frame. Only
// an operator-supplied complete input bound can cover that history.
func AdmitResponsesWebSocketEvent(c *gin.Context, info *relaycommon.RelayInfo, payload []byte) error {
	prompt, output, total := channelLimitTokenBudget(c, info, payload)
	if gjson.GetBytes(payload, "previous_response_id").String() != "" || gjson.GetBytes(payload, "conversation").Exists() {
		prompt, output, total = RealtimeGenerationBounds(info, gjson.GetBytes(payload, "max_output_tokens"))
	}
	return AdmitChannelEvent(c, info, prompt, output, total)
}

// Realtime inputs can contain audio and accumulated conversation state. They
// are not safely estimated from the latest message, audio duration or price.
// Unknown bounds stay nil; an RPM-only channel remains usable.
func RealtimeGenerationBounds(info *relaycommon.RelayInfo, outputValue gjson.Result) (prompt, output, total *int64) {
	if info.ChannelLimitAttempt.MaxInputTokens > 0 {
		value := info.ChannelLimitAttempt.MaxInputTokens
		prompt = &value
	}
	if outputValue.Type == gjson.Number && outputValue.Num >= 0 && math.Trunc(outputValue.Num) == outputValue.Num && outputValue.Num <= float64(common.MaxQuota) {
		value := outputValue.Int()
		output = &value
	} else if (!outputValue.Exists() || outputValue.String() == "inf") && info.ChannelLimitAttempt.MaxOutputTokens > 0 {
		value := info.ChannelLimitAttempt.MaxOutputTokens
		output = &value
	}
	if prompt != nil && output != nil && *prompt <= model.ChannelLimitMaxValue-*output {
		value := *prompt + *output
		total = &value
	}
	return
}

func realtimeTokenParams(info *relaycommon.RelayInfo, usage *dto.RealtimeUsage) billingexpr.TokenParams {
	vars := map[string]bool{}
	if snapshot := info.TieredBillingSnapshot; snapshot != nil {
		vars = billingexpr.UsedVarsByHash(snapshot.ExprString, snapshot.ExprHash)
	}
	return BuildTieredTokenParams(&dto.Usage{PromptTokens: usage.InputTokens, CompletionTokens: usage.OutputTokens, TotalTokens: usage.TotalTokens, PromptTokensDetails: usage.InputTokenDetails, CompletionTokenDetails: usage.OutputTokenDetails}, false, vars)
}

// A probe records upstream usage and procurement cost without charging a
// supplier's (non-consumer) account or producing customer consumption logs.
func CaptureRealtimeChannelTestUsage(c *gin.Context, info *relaycommon.RelayInfo, usage *dto.RealtimeUsage) {
	ok, _, result := TryTieredSettle(info, realtimeTokenParams(info, usage))
	quota := QuotaInfo{InputDetails: TokenDetails{ImageTokens: usage.InputTokenDetails.ImageTokens, TextTokens: usage.InputTokenDetails.TextTokens, AudioTokens: usage.InputTokenDetails.AudioTokens}, OutputDetails: TokenDetails{TextTokens: usage.OutputTokenDetails.TextTokens, AudioTokens: usage.OutputTokenDetails.AudioTokens}, ModelName: info.OriginModelName, UsePrice: info.PriceData.UsePrice, ModelPrice: info.PriceData.ModelPrice, ModelRatio: info.PriceData.ModelRatio, GroupRatio: 1, QuotaPerUnit: common.QuotaPerUnit, FrozenPrice: &info.PriceData}
	snapshot := &relaycommon.OverviewBillingSnapshot{InputTokens: int64(usage.InputTokenDetails.TextTokens), OutputTokens: int64(usage.OutputTokenDetails.TextTokens), UsageSource: "upstream_realtime", QuotaPerUnit: common.QuotaPerUnit}
	captureAudioBillingSnapshot(info, quota, snapshot, ok, result)
}

// Duration-based ASR follows the existing transcription billing convention:
// 1000 billing units per minute, rounded up to the observed whole second.
// Those units are never presented as measured text tokens or charged to TPM.
func SettleRealtimeTranscriptionDuration(c *gin.Context, info *relaycommon.RelayInfo, seconds float64) bool {
	quantity, clamp := common.QuotaRoundChecked(math.Ceil(seconds) / 60 * 1000)
	if clamp != nil {
		info.QuotaClamp = clamp
		return false
	}
	info.ObserveAudioSeconds(seconds, "upstream_usage")
	PostWssConsumeQuota(c, info, info.OriginModelName, &dto.RealtimeUsage{InputTokens: quantity, TotalTokens: quantity, InputTokenDetails: dto.InputTokenDetails{TextTokens: quantity}}, nil)
	if snapshot := info.AnalyticsBilling; snapshot != nil {
		snapshot.InputTokens, snapshot.OutputTokens = 0, 0
		snapshot.UsageSource = ""
	}
	return true
}

// Validation probes record duration procurement without consuming a customer wallet.
func CaptureRealtimeTranscriptionTestDuration(c *gin.Context, info *relaycommon.RelayInfo, seconds float64) {
	quantity, clamp := common.QuotaRoundChecked(math.Ceil(seconds) / 60 * 1000)
	if clamp != nil {
		info.QuotaClamp = clamp
		return
	}
	info.ObserveAudioSeconds(seconds, "upstream_usage")
	CaptureRealtimeChannelTestUsage(c, info, &dto.RealtimeUsage{InputTokens: quantity, TotalTokens: quantity, InputTokenDetails: dto.InputTokenDetails{TextTokens: quantity}})
	if snapshot := info.AnalyticsBilling; snapshot != nil {
		snapshot.InputTokens, snapshot.OutputTokens = 0, 0
		snapshot.UsageSource = ""
	}
}
