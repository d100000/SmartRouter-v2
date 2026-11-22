package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/logger"
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
	"gorm.io/gorm"
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

// channelUsesMeteredHTTP reports channel types whose every billable relay
// send is admitted into the procurement ledger right before it leaves: through
// relay/channel.doRequest, or through an explicit BeforeChannelUpstream call
// where the adaptor bypasses that transport (AWS Bedrock SDK, Xunfei and
// VolcEngine TTS WebSockets). Task submissions are admitted by the task path.
// A new channel type must be added here or listed as non-billable in the
// coverage test; a configured channel's cost is always recorded.
func channelUsesMeteredHTTP(channelType int) bool {
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
		constant.ChannelTypeVLLM, constant.ChannelTypeSGLang, constant.ChannelTypeSubmodel, constant.ChannelTypeTaskPlugin,
		constant.ChannelTypeAli, constant.ChannelTypeVolcEngine, constant.ChannelTypeZhipu, constant.ChannelTypeZhipu_v4,
		constant.ChannelTypeTencent, constant.ChannelTypeBaidu, constant.ChannelTypeBaiduV2, constant.ChannelTypeCohere,
		constant.ChannelTypeMiniMax, constant.ChannelTypeJina, constant.ChannelCloudflare, constant.ChannelTypeMokaAI,
		constant.ChannelTypeReplicate, constant.ChannelTypeOllama, constant.ChannelTypePaLM, constant.ChannelTypeDify,
		constant.ChannelTypeCoze, constant.ChannelTypeJimeng, constant.ChannelTypeAws, constant.ChannelTypeXunfei,
		// Task channel types: a non-task relay routed to them uses the
		// OpenAI-compatible adaptor and therefore doRequest.
		constant.ChannelTypeMidjourney, constant.ChannelTypeMidjourneyPlus, constant.ChannelTypeSunoAPI,
		constant.ChannelTypeKling, constant.ChannelTypeVidu, constant.ChannelTypeDoubaoVideo, constant.ChannelTypeSora:
		return true
	}
	return false
}

func channelLimitUpstreamStatusKey(attemptID string) string {
	return "channel_limit_upstream_status:" + attemptID
}

// ObserveChannelLimitUpstreamStatus records the HTTP status the upstream
// answered for the current physical request. It distinguishes a request the
// upstream rejected (no work ran) from one whose outcome is unknown.
func ObserveChannelLimitUpstreamStatus(c *gin.Context, info *relaycommon.RelayInfo, status int) {
	if c == nil || info == nil || info.ChannelLimitAttempt == nil || !info.ChannelLimitAttempt.Sent || status <= 0 {
		return
	}
	c.Set(channelLimitUpstreamStatusKey(info.ChannelLimitAttempt.ID), status)
}

func channelLimitUpstreamRejected(c *gin.Context, attempt *relaycommon.ChannelLimitRuntime) bool {
	if c == nil {
		return false
	}
	status := c.GetInt(channelLimitUpstreamStatusKey(attempt.ID))
	return status > 0 && (status < http.StatusOK || status >= http.StatusMultipleChoices)
}

var liveChannelLimitAttempts sync.Map
var channelLimitLeaseLoop sync.Once

func StartChannelLimitLeaseWorker() {
	channelLimitLeaseLoop.Do(func() { go maintainChannelLimitLeases() })
}

func maintainChannelLimitLeases() {
	ticker := time.NewTicker(15 * time.Second)
	defer ticker.Stop()
	for range ticker.C {
		byChannel := make(map[int][]string)
		liveChannelLimitAttempts.Range(func(key, value any) bool {
			channelID := value.(int)
			byChannel[channelID] = append(byChannel[channelID], key.(string))
			return true
		})
		workers := 4
		if common.UsingMainDatabase(common.DatabaseTypeSQLite) {
			workers = 1
		}
		slots := make(chan struct{}, workers)
		var renewals sync.WaitGroup
		for channelID, ids := range byChannel {
			slots <- struct{}{}
			renewals.Go(func() {
				defer func() { <-slots }()
				for batch := range slices.Chunk(ids, 128) {
					if err := model.RenewChannelAttemptLeases(channelID, batch, time.Time{}); err != nil {
						common.SysError("channel attempt lease update failed: " + err.Error())
					}
				}
			})
		}
		renewals.Wait()
	}
}

func PrepareChannelLimitAttempt(info *relaycommon.RelayInfo, channel *model.Channel) error {
	if info == nil || channel == nil {
		return model.ErrChannelAttemptState
	}
	info.ChannelLimitAttempt = nil
	// Every request records its procurement cost, whether or not a limit is
	// configured: the ledger is the channel's cost evidence, not only a limiter.
	config, err := model.GetChannelLimitConfig(channel.Id)
	if err != nil {
		return err
	}
	if err := ValidateChannelLimitSupport(channel, config.ChannelLimitSettings); err != nil {
		return err
	}
	// A realtime handshake has no RPM charge. Generations are admitted individually.
	if info.RelayFormat != types.RelayFormatOpenAIRealtime && info.RelayMode != relayconstant.RelayModeRealtime {
		if err := model.CheckChannelRPM(config, time.Time{}); err != nil {
			return err
		}
	}
	if info.TaskRelayInfo != nil {
		return PrepareTaskChannelAttempt(info, channel)
	}
	if !channelUsesMeteredHTTP(channel.Type) {
		if channel.OwnerSupplierID != "" || config.RPM > 0 || config.TPM > 0 || config.DailyUSDNano > 0 {
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

// PrepareNextPluginRelayAttempt gives each physical request of a provider
// chain its own ledger record. An intermediate step (authentication, polling,
// a rejected credential that is refreshed and resent) carries no billable work
// of its own: it settles at zero and releases its hold. The logical request's
// whole cost settles on its final step, so the records sum to what ran.
// Later steps are recorded but never refused for capacity, because the first
// step already ran upstream. Frozen procurement rules and the logical
// request's supplier claim survive the transition.
func PrepareNextPluginRelayAttempt(info *relaycommon.RelayInfo) error {
	if info == nil || info.ChannelLimitAttempt == nil || !info.ChannelLimitAttempt.Sent {
		return nil
	}
	previous := info.ChannelLimitAttempt
	if !previous.Finished {
		zero := int64(0)
		if err := model.SettleChannelAttempt(model.ChannelAttemptSettlement{ID: previous.ID, ChannelID: previous.ChannelID, Version: 1, Tokens: &zero, CostUSDNano: &zero}, time.Time{}); err != nil {
			return err
		}
		previous.Finished = true
		liveChannelLimitAttempts.Delete(previous.ID)
	}
	next := *previous
	next.ID, next.Sent, next.Finished = uuid.NewString(), false, false
	next.PriorPhysicalRequestSent = true
	info.ChannelLimitAttempt = &next
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
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return &model.ChannelLimitError{Reason: "channel_unavailable"}
	}
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
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
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
		config, err := model.GetChannelLimitConfig(channel.Id)
		if err != nil {
			return err
		}
		if channel.OwnerSupplierID != "" || config.RPM > 0 || config.TPM > 0 || config.DailyUSDNano > 0 {
			return &model.ChannelLimitError{Reason: "protocol_limit_unsupported"}
		}
		return nil
	}
	attempt := info.ChannelLimitAttempt
	if attempt.Sent {
		// Reusing a transport request ID for another send would evade RPM.
		return model.ErrChannelAttemptDuplicate
	}
	if info.RelayFormat == types.RelayFormatOpenAIRealtime || info.RelayMode == relayconstant.RelayModeRealtime || request != nil && strings.EqualFold(request.Header.Get("Upgrade"), "websocket") {
		return CheckChannelEventConnection(info)
	}
	config, err := model.GetChannelLimitConfig(attempt.ChannelID)
	if err != nil {
		return err
	}
	var body []byte
	needsEstimate := attempt.TPMApplicable && (config.TPM > 0 || config.DailyUSDNano > 0)
	// Fail fast before a slow token estimate. Without one, the recheck in
	// AdmitChannelEvent right before dispatch is sufficient.
	if needsEstimate {
		if err := checkChannelLimitDispatchState(info); err != nil {
			return err
		}
	}
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
	// Tokenization or remote input inspection can outlive a pause/configuration
	// change. Recheck at dispatch before committing the attempt.
	if err := checkChannelLimitDispatchState(info); err != nil {
		return err
	}
	claimedSupplier := false
	if !attempt.SupplierRequestClaimed {
		if !MarkSupplierRequestSent(c, &model.Channel{Id: attempt.ChannelID, OwnerSupplierID: attempt.SupplierID}) {
			return &model.ChannelLimitError{Reason: "supplier_attempt_already_sent"}
		}
		claimedSupplier = attempt.SupplierID != ""
	}
	if err := model.DispatchChannelAttempt(model.ChannelAttemptReservation{
		ID: attempt.ID, ChannelID: attempt.ChannelID, TPMApplicable: attempt.TPMApplicable,
		TokenBudget: budget, CostBudgetNano: costBudget,
		CostRatio: attempt.CostRatio, QuotaPerUnit: quotaPerUnit, PricingSnapshot: string(pricingSnapshot),
		FollowUp: attempt.PriorPhysicalRequestSent,
	}); err != nil {
		if claimedSupplier {
			ReleaseSupplierRequestSent(c, &model.Channel{Id: attempt.ChannelID, OwnerSupplierID: attempt.SupplierID})
		}
		return err
	}
	attempt.SupplierRequestClaimed = attempt.SupplierRequestClaimed || claimedSupplier
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
		// TPM records upstream-reported tokens only; a local estimate is not usage.
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
	settle := func(tokens, cost *int64) error {
		return model.SettleChannelAttempt(model.ChannelAttemptSettlement{ID: attempt.ID, ChannelID: attempt.ChannelID, Version: 1, Tokens: tokens, CostUSDNano: cost}, time.Time{})
	}
	var err error
	switch {
	case cost != nil:
		// The customer settlement computed this request's amount, so it ran at
		// this cost even if the client then disconnected or a later step failed.
		err = settle(tokens, cost)
	case apiErr != nil && !attempt.PriorPhysicalRequestSent && channelLimitUpstreamRejected(c, attempt):
		// The upstream rejected the request with no usage: nothing ran.
		zero := int64(0)
		err = settle(&zero, &zero)
	case attempt.CostRatio == nil:
		// No procurement price is configured, so no later evidence can price
		// this request either. Settle it (the cost stays counted as unknown)
		// rather than leaving it in reconciliation, also after a disconnect.
		err = settle(tokens, nil)
	default:
		// The request reached the upstream but no amount was computed for it.
		// Its cost is unknown and stays counted until a correction settles it.
		observed := int64(0)
		if tokens != nil {
			observed = *tokens
		}
		err = model.MarkChannelAttemptUnknown(attempt.ChannelID, attempt.ID, observed, time.Time{})
	}
	if err == nil {
		attempt.Finished = true
	}
	return err
}

// channelAdmissionError exposes only the localized public message to clients.
// Its cause remains available to host code for retry decisions.
type channelAdmissionError struct {
	message string
	cause   error
}

func (e *channelAdmissionError) Error() string { return e.message }
func (e *channelAdmissionError) Unwrap() error { return e.cause }

// CheckChannelRPM checks a candidate before building a generation or submission.
// The actual send still performs atomic admission against the current limits.
func CheckChannelRPM(channelID int) error {
	config, err := model.GetChannelLimitConfig(channelID)
	if err != nil {
		return err
	}
	return model.CheckChannelRPM(config, time.Time{})
}

func LocalChannelLimitError(err error) *types.NewAPIError {
	var apiErr *types.NewAPIError
	if !errors.As(err, &apiErr) || apiErr == nil || apiErr.GetErrorType() != types.ErrorTypeNewAPIError || apiErr.GetErrorCode() != "channel_limit_unavailable" {
		return nil
	}
	if _, local := apiErr.Err.(*channelAdmissionError); !local {
		return nil
	}
	return apiErr
}

// RememberAttemptFailure keeps the error from the attempt that was retried.
// A later candidate may be declined locally without producing an upstream result.
func (p *RetryParam) RememberAttemptFailure(apiErr *types.NewAPIError) {
	p.priorAttemptError = apiErr
	p.priorAttemptAddresses = common.UpstreamAddresses(p.Ctx)
	p.Ctx.Set("channel_rpm_retry_inapplicable", true)
}

// ClientErrorAfterRefusal selects the error to present after accounting has
// finished for the last candidate. An earlier provider error must be masked
// using that provider's addresses.
func (p *RetryParam) ClientErrorAfterRefusal(final *types.NewAPIError) (*types.NewAPIError, []string) {
	if p == nil || LocalChannelLimitError(final) == nil || p.LocalRefusal == nil {
		return final, nil
	}
	if p.priorAttemptError != nil {
		return p.priorAttemptError, p.priorAttemptAddresses
	}
	return p.LocalRefusal, nil
}

// TryAnotherChannelAfterRefusal handles a candidate declined before dispatch.
// A bound session may retain its preferred channel even if this call uses another one.
func (p *RetryParam) TryAnotherChannelAfterRefusal(channelID int, locked bool, apiErr *types.NewAPIError) bool {
	c := p.Ctx
	PreserveChannelAffinityAfterLocalRefusal(c, channelID)
	refusals := c.GetInt("channel_local_refusals") + 1
	c.Set("channel_local_refusals", refusals)
	var limited *model.ChannelLimitError
	if errors.As(apiErr, &limited) && limited.Reason == "rpm_limited" && limited.RetryAt > 0 {
		deadline := limited.RetryDeadline()
		if earliest := c.GetTime("channel_rpm_retry_at"); earliest.IsZero() || deadline.Before(earliest) {
			c.Set("channel_rpm_retry_at", deadline)
		}
	} else {
		c.Set("channel_rpm_retry_inapplicable", true)
	}
	// A known RPM retry time applies only when every candidate was RPM-limited.
	// Preserve a 503 over capacity refusals, and an untimed capacity refusal
	// over RPM, regardless of the order in which candidates were visited.
	var previous *model.ChannelLimitError
	if p.LocalRefusal == nil || apiErr.StatusCode >= http.StatusInternalServerError ||
		(errors.As(p.LocalRefusal, &previous) && previous.Reason == "rpm_limited") {
		p.LocalRefusal = apiErr
	}
	_, pinned, _ := GetChannelConstraints(c).ResolvedPin()
	return !locked && !pinned && RequestPolicy(c).SessionMode != "strict" && refusals < 512 && SchedulingRetryAllowed(c)
}

// WriteChannelRetryAfter is called only when rendering the final HTTP error.
func WriteChannelRetryAfter(c *gin.Context, err error) {
	apiErr := LocalChannelLimitError(err)
	var limited *model.ChannelLimitError
	if c.Writer.Written() || c.GetBool("channel_rpm_retry_inapplicable") || apiErr == nil || apiErr.StatusCode != http.StatusTooManyRequests ||
		!errors.As(apiErr, &limited) || limited.Reason != "rpm_limited" || limited.RetryAt <= 0 {
		return
	}
	deadline := limited.RetryDeadline()
	if earliest := c.GetTime("channel_rpm_retry_at"); !earliest.IsZero() && earliest.Before(deadline) {
		deadline = earliest
	}
	remaining := time.Until(deadline)
	c.Header("Retry-After", strconv.FormatInt(max(1, int64((remaining+time.Second-1)/time.Second)), 10))
}

// channelLimitInfrastructureError hides a ledger or database failure from the
// client. It is not a refusal by the channel, so callers do not try another
// candidate: that would repeat the failing query for every channel.
type channelLimitInfrastructureError struct {
	message string
	cause   error
}

func (e *channelLimitInfrastructureError) Error() string { return e.message }
func (e *channelLimitInfrastructureError) Unwrap() error { return e.cause }

// IsChannelLimitInfrastructureError reports an error produced by
// ChannelLimitRelayError for a failure other than the channel's own refusal.
func IsChannelLimitInfrastructureError(err error) bool {
	var failure *channelLimitInfrastructureError
	return errors.As(err, &failure)
}

// channelLimitRefusal reports whether err is this channel's own admission
// decision, which another candidate channel may not share.
func channelLimitRefusal(err error) bool {
	var limited *model.ChannelLimitError
	return errors.As(err, &limited) || errors.Is(err, model.ErrChannelAttemptDuplicate) || errors.Is(err, model.ErrChannelLimitAccounting)
}

func channelLimitCapacityReason(reason string) bool {
	return reason == "rpm_limited" || reason == "tpm_limited" || reason == "daily_exhausted" || reason == "budget_reserved"
}

const channelLimitRefusalLogInterval = 10 * time.Second

type channelLimitRefusalKey struct {
	channelID int
	reason    string
}

// Capacity refusals can occur for every request on a saturated channel.
// Log each channel and reason at most once per interval.
var channelLimitRefusalLog = struct {
	sync.Mutex
	last map[channelLimitRefusalKey]time.Time
}{last: make(map[channelLimitRefusalKey]time.Time)}

func shouldLogChannelLimitRefusal(channelID int, reason string) bool {
	now := time.Now()
	key := channelLimitRefusalKey{channelID: channelID, reason: reason}
	channelLimitRefusalLog.Lock()
	defer channelLimitRefusalLog.Unlock()
	if last, found := channelLimitRefusalLog.last[key]; found && now.Sub(last) < channelLimitRefusalLogInterval {
		return false
	}
	if len(channelLimitRefusalLog.last) >= 4096 {
		clear(channelLimitRefusalLog.last)
	}
	channelLimitRefusalLog.last[key] = now
	return true
}

// ChannelLimitRelayError preserves a machine-readable refusal without exposing
// another supplier's identity, budget, or usage through the inference API.
// The actual cause is written to the server log here.
func ChannelLimitRelayError(err error, contexts ...*gin.Context) *types.NewAPIError {
	var ctx *gin.Context
	if len(contexts) > 0 {
		ctx = contexts[0]
	}
	channelID := 0
	if ctx != nil {
		channelID = ctx.GetInt("channel_id")
	}
	var logCtx context.Context = context.Background()
	if ctx != nil {
		logCtx = ctx
	}
	i18nReady := i18n.Init() == nil
	if !channelLimitRefusal(err) {
		logger.LogError(logCtx, common.LogText("channel limit check failed: channel #%d: %s", channelID, err.Error()))
		message := "The service is temporarily unavailable. Please try again later"
		if i18nReady {
			message = i18n.T(ctx, i18n.MsgRelayChannelLimitCheckFailed)
		}
		return types.NewErrorWithStatusCode(&channelLimitInfrastructureError{message: message, cause: err}, types.ErrorCode("channel_limit_unavailable"), http.StatusServiceUnavailable, types.ErrOptionWithSkipRetry())
	}
	status := http.StatusServiceUnavailable
	reason := "accounting_unavailable"
	if errors.Is(err, model.ErrChannelAttemptDuplicate) {
		reason = "attempt_duplicate"
	}
	var limited *model.ChannelLimitError
	if errors.As(err, &limited) {
		reason = limited.Reason
	}
	if channelLimitCapacityReason(reason) {
		status = http.StatusTooManyRequests
		if shouldLogChannelLimitRefusal(channelID, reason) {
			logger.LogInfo(logCtx, common.LogText("channel limit refused request: channel #%d, reason %s", channelID, reason))
		}
	} else {
		logger.LogWarn(logCtx, common.LogText("channel limit refused request: channel #%d, reason %s", channelID, reason))
	}
	message := "Upstream channel capacity is currently unavailable"
	if i18nReady {
		message = i18n.T(ctx, i18n.MsgRelayChannelLimitUnavailable)
	}
	return types.NewErrorWithStatusCode(&channelAdmissionError{message: message, cause: err}, types.ErrorCode("channel_limit_unavailable"), status)
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
	// Like a relay settlement, an estimated quantity still prices the request;
	// UsageSource keeps it apart from upstream-reported usage.
	if usage.BillingUsage != nil && usage.BillingUsage.Estimated || common.GetContextKeyBool(c, constant.ContextKeyLocalCountTokens) {
		snapshot.UsageSource = "local_estimate"
	} else {
		captureOverviewCacheUsage(snapshot, info, usage, false)
	}
	base := summary.BaseQuota.InexactFloat64()
	if !summary.hasBillableUsage() {
		base = 0
	}
	if frozen := info.TieredBillingSnapshot; frozen != nil && tieredResult != nil {
		if tieredResult.Clamp != nil {
			return
		}
		base, snapshot.QuotaPerUnit = info.PriceData.ApplyOtherRatiosToFloat(tieredResult.ActualQuotaBeforeGroup), frozen.QuotaPerUnit
	}
	if info.QuotaClamp == nil && base >= 0 && !math.IsNaN(base) && !math.IsInf(base, 0) {
		snapshot.BaseQuota = &base
	}
}
