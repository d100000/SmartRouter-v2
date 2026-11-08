package service

import (
	"bytes"
	"errors"
	"io"
	"math"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/billingexpr"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/shopspring/decimal"
	"github.com/tidwall/gjson"
)

// Limit support follows transports with durable dispatch and terminal hooks.
func ValidateChannelLimitSupport(channel *model.Channel, settings model.ChannelLimitSettings) error {
	if settings.RPM == 0 && settings.TPM == 0 && settings.DailyUSDNano == 0 {
		return nil
	}
	if channel == nil {
		return model.ErrChannelAttemptState
	}
	if channelUsesMeteredHTTP(channel.Type) || channel.Type == constant.ChannelTypeMidjourney || channel.Type == constant.ChannelTypeMidjourneyPlus {
		return nil
	}
	if channel.Type == constant.ChannelTypeTaskPlugin {
		if _, ok := jsplugin.DefaultRegistry.Get(channel.GetSetting().TaskPluginKey); ok {
			return nil
		}
	}
	if _, ok := jsplugin.DefaultRegistry.GetByChannelType(channel.Type); ok {
		return nil
	}
	return &model.ChannelLimitError{Reason: "protocol_limit_unsupported"}
}

func channelUsesMeteredHTTP(channelType int) bool {
	// These adaptors dispatch synchronously through relay/channel.doRequest.
	// Do not infer support from the fallback API type: task and SDK channels
	// can share an OpenAI-facing route while bypassing that transport.
	switch channelType {
	case constant.ChannelTypeOpenAI, constant.ChannelTypeAzure, constant.ChannelTypeOpenAIMax,
		constant.ChannelTypeOhMyGPT, constant.ChannelTypeCustom, constant.ChannelTypeAILS,
		constant.ChannelTypeAIProxy, constant.ChannelTypeAPI2GPT, constant.ChannelTypeAIGC2D,
		constant.ChannelType360, constant.ChannelTypeFastGPT, constant.ChannelTypeLingYiWanWu,
		constant.ChannelTypeAnthropic, constant.ChannelTypeGemini, constant.ChannelTypeVertexAi,
		constant.ChannelTypeOpenRouter, constant.ChannelTypeDeepSeek, constant.ChannelTypeSiliconFlow,
		constant.ChannelTypeMoonshot, constant.ChannelTypePerplexity, constant.ChannelTypeMistral,
		constant.ChannelTypeXinference, constant.ChannelTypeXai, constant.ChannelTypeCodex,
		constant.ChannelTypeAdvancedCustom, constant.ChannelTypeSub2API, constant.ChannelTypeNewAPI,
		constant.ChannelTypeVLLM, constant.ChannelTypeSGLang, constant.ChannelTypeSubmodel:
		return true
	}
	return false
}

var liveChannelLimitAttempts sync.Map
var channelLimitLeaseLoop sync.Once

func StartChannelLimitLeaseWorker() {
	channelLimitLeaseLoop.Do(func() { go maintainChannelLimitLeases() })
}

func maintainChannelLimitLeases() {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for now := range ticker.C {
		liveChannelLimitAttempts.Range(func(key, value any) bool {
			if err := model.RenewChannelAttemptLease(value.(int), key.(string), now); err != nil {
				common.SysError("channel attempt lease update failed: " + err.Error())
			}
			return true
		})
	}
}

func PrepareChannelLimitAttempt(info *relaycommon.RelayInfo, channel *model.Channel) error {
	if info == nil || channel == nil {
		return model.ErrChannelAttemptState
	}
	info.ChannelLimitAttempt = nil
	state, err := model.GetChannelLimitState(channel.Id, time.Now())
	if err != nil {
		return err
	}
	if err := ValidateChannelLimitSupport(channel, state.Config.ChannelLimitSettings); err != nil {
		return err
	}
	if info.TaskRelayInfo != nil {
		return PrepareTaskChannelAttempt(info, channel)
	}
	if !channelUsesMeteredHTTP(channel.Type) {
		if channel.OwnerSupplierID != "" || state.Config.RPM > 0 || state.Config.TPM > 0 || state.Config.DailyUSDNano > 0 {
			return &model.ChannelLimitError{Reason: "protocol_limit_unsupported"}
		}
		return nil
	}
	return PrepareChannelEventAttempt(info, channel)
}

// PrepareChannelEventAttempt freezes a new business attempt. Opening a socket
// does not reserve capacity; each response.create uses a separate runtime.
func PrepareChannelEventAttempt(info *relaycommon.RelayInfo, channel *model.Channel) error {
	if info == nil || channel == nil {
		return model.ErrChannelAttemptState
	}
	info.ChannelLimitAttempt = nil
	var err error
	index := 0
	if info.ChannelMeta != nil && info.ChannelIsMultiKey {
		index = info.ChannelMultiKeyIndex
	}
	attempt := &relaycommon.ChannelLimitRuntime{ID: uuid.NewString(), ChannelID: channel.Id, SupplierID: channel.OwnerSupplierID, TemplateID: model.SupplierChannelTemplateID(channel), TPMApplicable: true, WriteOnlyCredentials: channel.SecretsWriteOnly || channel.OwnerSupplierID != ""}
	var account *model.SupplierAccount
	if channel.OwnerSupplierID != "" {
		account, err = model.GetSupplierAccount(channel.OwnerSupplierID)
		if err != nil {
			return err
		}
		attempt.MaxOutputTokens = account.MaxOutputTokens
		attempt.MaxInputTokens = account.MaxInputTokens
	}
	snapshot, err := model.GetChannelProcurementSnapshot(channel, index, account)
	if err != nil {
		return err
	}
	attempt.CostRatio, attempt.CostSource, attempt.CostVersionID = snapshot.EffectiveCostRatio, snapshot.CostSource, snapshot.CostVersionID
	attempt.CredentialCostVersionID, attempt.ChannelCostVersionID = snapshot.CredentialCostVersionID, snapshot.ChannelCostVersionID
	switch info.RelayMode {
	case relayconstant.RelayModeImagesGenerations, relayconstant.RelayModeImagesEdits,
		relayconstant.RelayModeAudioSpeech, relayconstant.RelayModeAudioTranscription,
		relayconstant.RelayModeAudioTranslation, relayconstant.RelayModeVideoSubmit:
		// Image count, duration and credits are not text-token quantities.
		attempt.TPMApplicable = false
	}
	if info.TaskRelayInfo != nil {
		attempt.TPMApplicable = false
	}
	info.ChannelLimitAttempt = attempt
	return nil
}

// BeforeChannelUpstream is for dispatch paths whose final request is already
// represented by info.Request. HTTP transports should pass the actual request
// to BeforeChannelUpstreamRequest so overrides cannot bypass an output budget.
func BeforeChannelUpstream(c *gin.Context, info *relaycommon.RelayInfo) error {
	return BeforeChannelUpstreamRequest(c, info, nil)
}

func checkChannelLimitDispatchState(info *relaycommon.RelayInfo) error {
	attempt := info.ChannelLimitAttempt
	channel, err := model.GetChannelById(attempt.ChannelID, false)
	if err != nil {
		return err
	}
	if channel.OwnerSupplierID != attempt.SupplierID || !info.IsChannelTest &&
		(channel.Status != common.ChannelStatusEnabled || !channel.ScheduleAllowsAt(time.Now()) || !model.SupplierChannelAvailable(channel)) {
		return &model.ChannelLimitError{Reason: "channel_unavailable"}
	}
	if attempt.SupplierID != "" {
		if _, available := SupplierTemplateByID(attempt.TemplateID); !available {
			return &model.ChannelLimitError{Reason: "channel_unavailable"}
		}
		account, err := model.GetSupplierAccount(attempt.SupplierID)
		if err != nil || !account.SupplyEnabled || !account.AllowsTemplate(attempt.TemplateID, []string{info.OriginModelName}) {
			return &model.ChannelLimitError{Reason: "channel_unavailable"}
		}
	}
	return nil
}

func BeforeChannelUpstreamRequest(c *gin.Context, info *relaycommon.RelayInfo, request *http.Request) error {
	if info == nil {
		return model.ErrChannelAttemptState
	}
	if info.TaskRelayInfo != nil {
		return BeforeTaskChannelUpstream(c, info, request)
	}
	if info.ChannelLimitAttempt == nil {
		// Polling and utility transports do not originate a new business attempt.
		// Never permit them to bypass a configured channel limit or supplier policy.
		if info.ChannelId <= 0 {
			return nil
		}
		channel, err := model.GetChannelById(info.ChannelId, false)
		if err != nil {
			return err
		}
		state, err := model.GetChannelLimitState(channel.Id, time.Now())
		if err != nil {
			return err
		}
		if channel.OwnerSupplierID != "" || state.Config.RPM > 0 || state.Config.TPM > 0 || state.Config.DailyUSDNano > 0 {
			return &model.ChannelLimitError{Reason: "protocol_limit_unsupported"}
		}
		return nil
	}
	attempt := info.ChannelLimitAttempt
	if attempt.Sent {
		// Reusing a transport request ID for another send would evade RPM.
		return model.ErrChannelAttemptDuplicate
	}
	if err := checkChannelLimitDispatchState(info); err != nil {
		return err
	}
	now := time.Now()
	state, err := model.GetChannelLimitState(attempt.ChannelID, now)
	if err != nil {
		return err
	}
	if info.RelayFormat == types.RelayFormatOpenAIRealtime || info.RelayMode == relayconstant.RelayModeRealtime || request != nil && strings.EqualFold(request.Header.Get("Upgrade"), "websocket") {
		return CheckChannelEventConnection(info)
	}
	var body []byte
	needsEstimate := attempt.TPMApplicable && (state.Config.TPM > 0 || state.Config.DailyUSDNano > 0)
	if needsEstimate && request != nil && request.Body != nil && (strings.Contains(request.Header.Get("Content-Type"), "json") || request.Header.Get("Content-Type") == "") {
		reader := request.Body
		if request.GetBody != nil {
			reader, err = request.GetBody()
			if err != nil {
				return err
			}
		}
		maxBytes := int64(constant.MaxRequestBodyMB) * 1024 * 1024
		if maxBytes <= 0 {
			maxBytes = 128 * 1024 * 1024
		}
		body, err = io.ReadAll(io.LimitReader(reader, maxBytes+1))
		_ = reader.Close()
		if request.GetBody == nil {
			request.Body = io.NopCloser(bytes.NewReader(body))
			request.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
		}
		if err != nil || int64(len(body)) > maxBytes {
			return model.ErrChannelLimitAccounting
		}
	} else if needsEstimate && request == nil && info.Request != nil {
		body, err = common.Marshal(info.Request)
		if err != nil {
			return err
		}
	}
	prompt, output, budget := channelLimitTokenBudget(c, info, body)
	return AdmitChannelEvent(c, info, prompt, output, budget)
}

// CheckChannelEventConnection verifies the owner and current supply state,
// without treating a WebSocket handshake as a generated response.
func CheckChannelEventConnection(info *relaycommon.RelayInfo) error {
	if info == nil || info.ChannelLimitAttempt == nil {
		return model.ErrChannelAttemptState
	}
	return checkChannelLimitDispatchState(info)
}

// AdmitChannelEvent uses caller-supplied trusted bounds for a single generated
// response. Unknown bounds remain nil and never become an implicit zero.
func AdmitChannelEvent(c *gin.Context, info *relaycommon.RelayInfo, prompt, output, budget *int64) error {
	if info == nil || info.ChannelLimitAttempt == nil {
		return model.ErrChannelAttemptState
	}
	attempt := info.ChannelLimitAttempt
	if attempt.Sent {
		return model.ErrChannelAttemptDuplicate
	}
	if err := checkChannelLimitDispatchState(info); err != nil {
		return err
	}
	for _, bound := range []*int64{prompt, output, budget} {
		if bound != nil && (*bound < 0 || *bound > model.ChannelLimitMaxValue) {
			return model.ErrChannelLimitAccounting
		}
	}
	var err error
	var costBudget *int64
	if attempt.CostRatio != nil {
		if *attempt.CostRatio == 0 {
			zero := int64(0)
			costBudget = &zero
		} else if amount, err := channelLimitProcurementBudget(info, prompt, output); err == nil {
			costBudget = &amount
		}
	}
	quotaPerUnit := common.QuotaPerUnit
	if info.TieredBillingSnapshot != nil {
		quotaPerUnit = info.TieredBillingSnapshot.QuotaPerUnit
	}
	// Persist pricing rules only. Never serialize RelayInfo, request input, or
	// the extensible task usage facts into the procurement ledger.
	var expressionSnapshot map[string]any
	if snapshot := info.TieredBillingSnapshot; snapshot != nil {
		expressionSnapshot = map[string]any{
			"expression": snapshot.ExprString, "hash": snapshot.ExprHash,
			"version": snapshot.ExprVersion, "billing_mode": snapshot.BillingMode,
			"quota_per_unit": snapshot.QuotaPerUnit,
		}
	}
	pricingSnapshot, err := common.Marshal(map[string]any{"price": info.PriceData, "other_ratios": info.PriceData.OtherRatios(), "expression": expressionSnapshot,
		"cost_source": attempt.CostSource, "cost_version_id": attempt.CostVersionID,
		"credential_cost_version_id": attempt.CredentialCostVersionID, "channel_cost_version_id": attempt.ChannelCostVersionID})
	if err != nil {
		return err
	}
	now := time.Now()
	_, err = model.ReserveChannelAttempt(model.ChannelAttemptReservation{
		ID: attempt.ID, ChannelID: attempt.ChannelID, TPMApplicable: attempt.TPMApplicable,
		TokenBudget: budget, CostBudgetNano: costBudget,
		CostRatio: attempt.CostRatio, QuotaPerUnit: quotaPerUnit, PricingSnapshot: string(pricingSnapshot),
	}, now)
	if err != nil {
		return err
	}
	// Tokenization or remote input inspection can outlive a pause/configuration
	// change. Recheck at the dispatch boundary, releasing our unsent permit.
	if err := checkChannelLimitDispatchState(info); err != nil {
		_ = model.ReleaseChannelAttempt(attempt.ChannelID, attempt.ID, time.Now())
		return err
	}
	if !MarkSupplierRequestSent(c, &model.Channel{Id: attempt.ChannelID, OwnerSupplierID: attempt.SupplierID}) {
		_ = model.ReleaseChannelAttempt(attempt.ChannelID, attempt.ID, now)
		return &model.ChannelLimitError{Reason: "supplier_attempt_already_sent"}
	}
	if err := model.MarkChannelAttemptSent(attempt.ChannelID, attempt.ID, time.Now()); err != nil {
		ReleaseSupplierRequestSent(c, &model.Channel{Id: attempt.ChannelID, OwnerSupplierID: attempt.SupplierID})
		_ = model.ReleaseChannelAttempt(attempt.ChannelID, attempt.ID, now)
		return err
	}
	attempt.Sent = true
	StartSchedulingAttempt(c, info)
	liveChannelLimitAttempts.Store(attempt.ID, attempt.ChannelID)
	return nil
}

// A nil bound is unknown. It is deliberately distinct from an explicit zero
// output bound and does not block a channel whose applicable limits are off.
func channelLimitTokenBudget(c *gin.Context, info *relaycommon.RelayInfo, body []byte) (*int64, *int64, *int64) {
	if !info.ChannelLimitAttempt.TPMApplicable || len(body) == 0 || !gjson.ValidBytes(body) {
		return nil, nil, nil
	}
	// A stored conversation's unseen context cannot be counted from this
	// outbound body. Fixed per-request pricing can still be budgeted separately.
	if gjson.GetBytes(body, "previous_response_id").String() != "" || gjson.GetBytes(body, "conversation").Type != gjson.Null {
		return nil, nil, nil
	}
	var parsed dto.Request
	switch info.GetFinalRequestRelayFormat() {
	case types.RelayFormatClaude:
		if !gjson.GetBytes(body, "messages").IsArray() {
			return nil, nil, nil
		}
		parsed = &dto.ClaudeRequest{}
	case types.RelayFormatGemini:
		if info.RelayMode == relayconstant.RelayModeEmbeddings {
			if !gjson.GetBytes(body, "content").Exists() && !gjson.GetBytes(body, "requests").IsArray() {
				return nil, nil, nil
			}
			parsed = &dto.GeminiEmbeddingRequest{}
		} else {
			if !gjson.GetBytes(body, "contents").IsArray() {
				return nil, nil, nil
			}
			parsed = &dto.GeminiChatRequest{}
		}
	case types.RelayFormatOpenAIResponses, types.RelayFormatOpenAIResponsesCompaction:
		if !gjson.GetBytes(body, "input").Exists() {
			return nil, nil, nil
		}
		parsed = &dto.OpenAIResponsesRequest{}
	default:
		if info.RelayMode == relayconstant.RelayModeRerank {
			if !gjson.GetBytes(body, "query").Exists() || !gjson.GetBytes(body, "documents").IsArray() {
				return nil, nil, nil
			}
			parsed = &dto.RerankRequest{}
		} else {
			if !gjson.GetBytes(body, "messages").IsArray() && !gjson.GetBytes(body, "prompt").Exists() && !gjson.GetBytes(body, "input").Exists() {
				return nil, nil, nil
			}
			parsed = &dto.GeneralOpenAIRequest{}
		}
	}
	if common.Unmarshal(body, parsed) != nil {
		return nil, nil, nil
	}
	countInfo := *info
	countInfo.RelayFormat = info.GetFinalRequestRelayFormat()
	count, err := CountRequestToken(c, parsed.GetTokenCountMeta(), &countInfo)
	if err != nil || count < 0 || int64(count) > int64(common.MaxQuota) {
		return nil, nil, nil
	}
	input := int64(count)
	batches := int64(1)
	if info.GetFinalRequestRelayFormat() == types.RelayFormatOpenAI {
		field := "prompt"
		if info.RelayMode == relayconstant.RelayModeEmbeddings || info.RelayMode == relayconstant.RelayModeModerations {
			field = "input"
		}
		if values := gjson.GetBytes(body, field); values.IsArray() {
			batchInput, batchCount, err := channelLimitTextBatchBudget(c, &countInfo, values.Array())
			if err != nil {
				return nil, nil, nil
			}
			input, batches = max(input, batchInput), batchCount
		}
	}
	var output *int64
	switch info.RelayMode {
	case relayconstant.RelayModeEmbeddings, relayconstant.RelayModeRerank, relayconstant.RelayModeModerations:
		zero := int64(0)
		output = &zero
	default:
		bound := int64(-1)
		for _, path := range []string{"max_tokens", "max_completion_tokens", "max_output_tokens", "generationConfig.maxOutputTokens", "generation_config.max_output_tokens"} {
			value := gjson.GetBytes(body, path)
			if !value.Exists() {
				continue
			}
			if value.Type != gjson.Number || value.Num < 0 || math.Trunc(value.Num) != value.Num || value.Num > float64(common.MaxQuota) {
				return &input, nil, nil
			}
			bound = max(bound, value.Int())
		}
		if bound < 0 && info.ChannelLimitAttempt.MaxOutputTokens > 0 {
			bound = info.ChannelLimitAttempt.MaxOutputTokens
		}
		if bound >= 0 {
			choices := int64(1)
			for _, path := range []string{"n", "best_of", "generationConfig.candidateCount", "generation_config.candidate_count"} {
				value := gjson.GetBytes(body, path)
				if !value.Exists() {
					continue
				}
				if value.Type != gjson.Number || value.Num < 1 || math.Trunc(value.Num) != value.Num || value.Num > float64(common.MaxQuota) {
					return &input, nil, nil
				}
				choices = max(choices, value.Int())
			}
			if choices > model.ChannelLimitMaxValue/batches {
				return &input, nil, nil
			}
			choices *= batches
			if bound > model.ChannelLimitMaxValue/choices {
				return &input, nil, nil
			}
			bound *= choices
			output = &bound
		}
	}
	if output == nil || *output > model.ChannelLimitMaxValue-input {
		return &input, output, nil
	}
	total := input + *output
	return &input, output, &total
}

// Text endpoints accept a batch of strings or token-ID sequences; a flat
// token-ID array is one prompt. Counting their JSON/string representation
// would discard token IDs or merge independent prompts across a boundary.
func channelLimitTextBatchBudget(c *gin.Context, info *relaycommon.RelayInfo, values []gjson.Result) (int64, int64, error) {
	if len(values) == 0 {
		return 0, 0, model.ErrChannelLimitAccounting
	}
	var input int64
	batches := int64(len(values))
	kind := values[0].Type
	nested := values[0].IsArray()
	if kind == gjson.Number {
		batches = 1
	}
	for _, value := range values {
		if value.Type != kind || value.IsArray() != nested {
			return 0, 0, model.ErrChannelLimitAccounting
		}
		var tokens int64
		switch {
		case kind == gjson.String:
			count, err := CountRequestToken(c, &types.TokenCountMeta{CombineText: value.Str}, info)
			if err != nil || count < 0 {
				return 0, 0, model.ErrChannelLimitAccounting
			}
			tokens = int64(count)
		case kind == gjson.Number:
			if value.Num < 0 || math.Trunc(value.Num) != value.Num || value.Num > float64(common.MaxQuota) {
				return 0, 0, model.ErrChannelLimitAccounting
			}
			tokens = 1
		case nested:
			ids := value.Array()
			if len(ids) == 0 {
				return 0, 0, model.ErrChannelLimitAccounting
			}
			for _, id := range ids {
				if id.Type != gjson.Number || id.Num < 0 || math.Trunc(id.Num) != id.Num || id.Num > float64(common.MaxQuota) {
					return 0, 0, model.ErrChannelLimitAccounting
				}
			}
			tokens = int64(len(ids))
		default:
			return 0, 0, model.ErrChannelLimitAccounting
		}
		if tokens > model.ChannelLimitMaxValue-input {
			return 0, 0, model.ErrChannelLimitAccounting
		}
		input += tokens
	}
	return input, batches, nil
}

func channelLimitProcurementBudget(info *relaycommon.RelayInfo, prompt, output *int64) (int64, error) {
	attempt := info.ChannelLimitAttempt
	if attempt == nil || attempt.CostRatio == nil {
		return 0, model.ErrChannelLimitAccounting
	}
	price := info.PriceData
	quotaPerUnit := common.QuotaPerUnit
	var base float64
	if snapshot := info.TieredBillingSnapshot; snapshot != nil {
		request := billingexpr.RequestInput{Usage: snapshot.UsageFacts}
		if info.BillingRequestInput != nil {
			request = *info.BillingRequestInput
			if snapshot.TaskUsageBilling {
				request.Usage = snapshot.UsageFacts
			}
		}
		var upperQuota float64
		var err error
		if snapshot.TaskUsageBilling {
			// Daily procurement uses the validated submission estimate. It is
			// independent of customer group discounts; completion can exceed it
			// and records the full delta without truncating in-flight work.
			upperQuota = snapshot.EstimatedQuotaBeforeGroup
			if upperQuota < 0 || math.IsNaN(upperQuota) || math.IsInf(upperQuota, 0) || snapshot.QuotaPerUnit <= 0 {
				err = model.ErrChannelLimitAccounting
			}
		} else {
			upperQuota, err = channelLimitExpressionUpperQuota(snapshot, prompt, output, request)
		}
		if err != nil {
			return 0, err
		}
		base, quotaPerUnit = price.ApplyOtherRatiosToFloat(upperQuota), snapshot.QuotaPerUnit
	} else if price.UsePrice {
		base = price.ApplyOtherRatiosToFloat(price.ModelPrice * quotaPerUnit)
	} else if info.TaskRelayInfo != nil {
		// Match the legacy task request estimate directly, before discounts.
		base = price.ApplyOtherRatiosToFloat(price.ModelRatio / 2 * quotaPerUnit)
	} else {
		if prompt == nil || output == nil {
			return 0, model.ErrChannelLimitAccounting
		}
		// Keep the frozen legacy model ratios, including explicit zero pricing;
		// do not divide the customer reservation by its group discount.
		inputPrice := max(1, price.CacheRatio, price.CacheCreationRatio, price.CacheCreation5mRatio, price.CacheCreation1hRatio, price.CacheCreation12hRatio, price.ImageRatio, price.AudioRatio)
		base = decimal.NewFromInt(*prompt).Mul(decimal.NewFromFloat(inputPrice)).
			Add(decimal.NewFromInt(*output).Mul(decimal.NewFromFloat(max(price.CompletionRatio, price.AudioRatio*price.AudioCompletionRatio)))).
			Mul(decimal.NewFromFloat(price.ModelRatio)).InexactFloat64()
		base = price.ApplyOtherRatiosToFloat(base)
	}
	return model.ProcurementCostUSDNano(base, *attempt.CostRatio, quotaPerUnit)
}

func FinishChannelLimitAttempt(c *gin.Context, info *relaycommon.RelayInfo, apiErr *types.NewAPIError) error {
	if info == nil || info.ChannelLimitAttempt == nil {
		return nil
	}
	attempt := info.ChannelLimitAttempt
	if !attempt.Sent || attempt.Finished {
		return nil
	}
	liveChannelLimitAttempts.Delete(attempt.ID)
	if info.TaskRelayInfo != nil && apiErr == nil {
		// The task host persists the attempt ID and settles on its actual terminal
		// event. A successful submission is not successful task completion.
		return nil
	}
	var tokens, cost *int64
	if billing := info.AnalyticsBilling; billing != nil {
		if billing.UsageSource != "" && billing.UsageSource != "local_estimate" {
			input := billing.InputTokens
			if billing.CacheUsageKnown {
				input = billing.CacheInputTokens
			}
			if input >= 0 && billing.OutputTokens >= 0 && input <= model.ChannelLimitMaxValue-billing.OutputTokens {
				total := input + billing.OutputTokens
				tokens = &total
			}
		}
		if billing.BaseQuota != nil && attempt.CostRatio != nil {
			if amount, err := model.ProcurementCostUSDNano(*billing.BaseQuota, *attempt.CostRatio, billing.QuotaPerUnit); err == nil {
				cost = &amount
			}
		}
	}
	if attempt.CostRatio != nil && *attempt.CostRatio == 0 {
		zero := int64(0)
		cost = &zero
	}
	now := time.Now()
	unknown := apiErr != nil && (apiErr.GetErrorCode() == types.ErrorCodeDoRequestFailed || apiErr.GetErrorCode() == types.ErrorCodeReadResponseBodyFailed || apiErr.GetErrorCode() == types.ErrorCodeChannelResponseTimeExceeded)
	if c != nil && c.Request != nil && c.Request.Context().Err() != nil {
		unknown = true
	}
	if info.StreamStatus != nil {
		outcome := info.StreamStatus.OutcomeSnapshot()
		if outcome.Response == relaycommon.ResponseOutcomeUnknown && (outcome.ExpectsTerminal || outcome.EndReason != relaycommon.StreamEndReasonDone && outcome.EndReason != relaycommon.StreamEndReasonEOF) {
			unknown = true
		}
	}
	var err error
	if unknown {
		observed := int64(0)
		if tokens != nil {
			observed = *tokens
		}
		if cost != nil {
			err = model.MarkChannelAttemptUnknown(attempt.ChannelID, attempt.ID, observed, now, *cost)
		} else {
			err = model.MarkChannelAttemptUnknown(attempt.ChannelID, attempt.ID, observed, now)
		}
	} else {
		err = model.SettleChannelAttempt(model.ChannelAttemptSettlement{ID: attempt.ID, ChannelID: attempt.ChannelID, Version: 1, Tokens: tokens, CostUSDNano: cost}, now)
	}
	if err == nil {
		attempt.Finished = true
	}
	return err
}

// ChannelLimitRelayError preserves a machine-readable refusal without exposing
// another supplier's identity, budget, or usage through the inference API.
func ChannelLimitRelayError(err error, contexts ...*gin.Context) *types.NewAPIError {
	status := http.StatusServiceUnavailable
	var limited *model.ChannelLimitError
	if errors.As(err, &limited) && (limited.Reason == "rpm_limited" || limited.Reason == "tpm_limited" || limited.Reason == "daily_exhausted" || limited.Reason == "budget_reserved") {
		status = http.StatusTooManyRequests
	}
	var ctx *gin.Context
	if len(contexts) > 0 {
		ctx = contexts[0]
	}
	message := "Upstream channel capacity is currently unavailable"
	if i18n.Init() == nil {
		message = i18n.T(ctx, i18n.MsgRelayChannelLimitUnavailable)
	}
	return types.NewErrorWithStatusCode(err, types.ErrorCode("channel_limit_unavailable"), status, types.ErrOptionWithHideErrMsg(message))
}

// Channel tests consume upstream supply but never a consumer's wallet. Use the
// same frozen price rules and normalized upstream usage to settle their limits.
func CaptureChannelTestLimitUsage(c *gin.Context, info *relaycommon.RelayInfo, usage *dto.Usage, tieredResult *billingexpr.TieredResult) {
	if info == nil || usage == nil {
		return
	}
	summary := calculateTextQuotaSummary(c, info, effectiveBillingUsage(usage))
	snapshot := &relaycommon.OverviewBillingSnapshot{InputTokens: int64(summary.PromptTokens), OutputTokens: int64(summary.CompletionTokens), UsageSource: "upstream", QuotaPerUnit: summary.QuotaPerUnit}
	info.AnalyticsBilling = snapshot
	if usage.BillingUsage != nil && usage.BillingUsage.Estimated || common.GetContextKeyBool(c, constant.ContextKeyLocalCountTokens) {
		snapshot.UsageSource = "local_estimate"
		return
	}
	captureOverviewCacheUsage(snapshot, info, usage, false)
	base := summary.BaseQuota.InexactFloat64()
	if frozen := info.TieredBillingSnapshot; frozen != nil {
		if tieredResult == nil || tieredResult.Clamp != nil {
			return
		}
		base, snapshot.QuotaPerUnit = info.PriceData.ApplyOtherRatiosToFloat(tieredResult.ActualQuotaBeforeGroup), frozen.QuotaPerUnit
	}
	if info.QuotaClamp == nil && base >= 0 && !math.IsNaN(base) && !math.IsInf(base, 0) {
		snapshot.BaseQuota = &base
	}
}
