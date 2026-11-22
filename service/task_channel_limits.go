package service

import (
	"bytes"
	"context"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	taskdto "github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/jsplugin"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
	"github.com/shopspring/decimal"
	"github.com/tidwall/gjson"
	"gorm.io/gorm"
)

// ProtectedChannelCredentials follows the credential protection lineage even
// after a supplier channel is copied into platform ownership.
func ProtectedChannelCredentials(info *relaycommon.RelayInfo) bool {
	return info != nil && (info.ChannelLimitAttempt != nil && (info.ChannelLimitAttempt.SupplierID != "" || info.ChannelLimitAttempt.WriteOnlyCredentials) || info.TaskChannelSnapshot != nil && (info.TaskChannelSnapshot.SupplierID != "" || info.TaskChannelSnapshot.Encrypted))
}

func TaskChannelSensitiveError(info *relaycommon.RelayInfo, err error) error {
	if err == nil || !ProtectedChannelCredentials(info) {
		return err
	}
	return errors.New("Upstream request failed")
}

func ProtectTaskChannelError(info *relaycommon.RelayInfo, failure *taskdto.TaskError) *taskdto.TaskError {
	if failure == nil || !ProtectedChannelCredentials(info) {
		return failure
	}
	copy := *failure
	copy.Message, copy.Error = "Upstream request failed", errors.New("Upstream request failed")
	if local := LocalChannelLimitError(failure.Error); local != nil {
		copy.Message, copy.Error = local.Error(), local
	}
	copy.Data = nil
	return &copy
}

func protectedTask(task *model.Task) bool {
	return task != nil && task.PrivateData.ChannelAttempt != nil && (task.PrivateData.ChannelAttempt.Encrypted || task.PrivateData.ChannelAttempt.SupplierID != "")
}

// RedactTaskChannelPayload preserves JSON number precision and string
// escaping. Plain-text replies are redacted without interpreting their data.
func RedactTaskChannelPayload(info *relaycommon.RelayInfo, payload []byte) []byte {
	if !ProtectedChannelCredentials(info) || info.ChannelMeta == nil || info.ApiKey == "" {
		return payload
	}
	if gjson.ValidBytes(payload) {
		return RedactUpstreamWebSocketCredential(payload, info.ApiKey)
	}
	return bytes.ReplaceAll(payload, []byte(info.ApiKey), []byte("[REDACTED]"))
}

func PrepareTaskChannelAttempt(info *relaycommon.RelayInfo, channel *model.Channel) error {
	if err := PrepareChannelEventAttempt(info, channel); err != nil {
		return err
	}
	info.ChannelLimitAttempt.TPMApplicable = false
	info.TaskChannelSnapshot = nil
	return nil
}

// TaskChannelUsageBounds uses the same host-owned bounds as plugin billing.
// A submission estimate is never treated as the maximum completion quantity.
func TaskChannelUsageBounds(units map[string]string) map[string]float64 {
	bounds := make(map[string]float64, len(units))
	for key, unit := range units {
		switch unit {
		case "count":
			bounds[key] = 128
		case "second":
			bounds[key] = 3600
		case "token", "credit":
			bounds[key] = float64(common.MaxQuota)
		}
	}
	return bounds
}

func BeforeTaskChannelUpstream(c *gin.Context, info *relaycommon.RelayInfo, request *http.Request) error {
	if info != nil && info.ChannelMeta != nil && info.ChannelId <= 0 && info.ChannelLimitAttempt == nil {
		return nil
	}
	if info == nil || info.ChannelLimitAttempt == nil || info.ChannelMeta == nil {
		return model.ErrChannelAttemptState
	}
	attempt := info.ChannelLimitAttempt
	meta := *info.ChannelMeta
	data, err := common.Marshal(meta)
	if err != nil {
		return err
	}
	snapshot := &relaycommon.TaskChannelSnapshot{AttemptID: attempt.ID, ChannelID: attempt.ChannelID, SupplierID: attempt.SupplierID,
		TemplateID: attempt.TemplateID, IsChannelTest: info.IsChannelTest, Connection: string(data), CostRatio: attempt.CostRatio,
		QuotaPerUnit: common.QuotaPerUnit, ModelPrice: info.PriceData.ModelPrice, ModelRatio: info.PriceData.ModelRatio, UsePrice: info.PriceData.UsePrice,
		OtherRatios: info.PriceData.OtherRatios(), UsageUnits: info.TaskUsageUnits, Expression: info.TieredBillingSnapshot,
		Overview: info.OverviewAttempt, TPMApplicable: false}
	// Generic duration facts have no media meaning by themselves. Freeze the
	// host protocol/action meaning so audio cannot become video after reload.
	if endpoint, ok := c.Get(jsplugin.ContextKeyPinnedEndpoint); ok {
		if pinned, ok := endpoint.(jsplugin.PinnedEndpoint); ok {
			switch pinned.Protocol {
			case "openai_video":
				snapshot.MediaKind = "video"
			case "openai_image":
				snapshot.MediaKind = "image"
			case "openai_responses":
				snapshot.MediaKind = "text"
			}
		}
	}
	if snapshot.MediaKind == "" && info.TaskRelayInfo != nil {
		switch constant.NormalizeTaskAction(info.Action) {
		case constant.TaskActionTextToVideo, constant.TaskActionImageToVideo, constant.TaskActionFirstTailToVideo, constant.TaskActionReferenceToVideo:
			snapshot.MediaKind = "video"
		}
		if constant.TaskPlatform(c.GetString("platform")) == constant.TaskPlatformSuno {
			snapshot.MediaKind = "audio"
		}
	}
	var protection model.Channel
	if err := model.DB.Select("id", "secrets_write_only").First(&protection, attempt.ChannelID).Error; err != nil {
		return err
	}
	if attempt.SupplierID != "" || protection.SecretsWriteOnly {
		snapshot.Connection, err = model.EncryptSupplierSecret(attempt.SupplierID, snapshot.Connection)
		if err != nil {
			return err
		}
		snapshot.Encrypted = true
	}
	if info.TieredBillingSnapshot != nil {
		snapshot.QuotaPerUnit = info.TieredBillingSnapshot.QuotaPerUnit
	}
	if info.OverviewAttempt != nil {
		snapshot.CredentialID, snapshot.CredentialVersionID, snapshot.BindingID = info.OverviewAttempt.CredentialID, info.OverviewAttempt.CredentialVersionID, info.OverviewAttempt.BindingID
	}
	// Only named text input/output facts have TPM semantics. Vendor video
	// tokens and credits are procurement quantities, never text TPM.
	textInput := info.TaskUsageUnits["input_tokens"] == "token" || info.TaskUsageUnits["prompt_tokens"] == "token"
	textOutput := info.TaskUsageUnits["output_tokens"] == "token" || info.TaskUsageUnits["completion_tokens"] == "token"
	var input, output, total *int64
	if textInput && textOutput {
		attempt.TPMApplicable, snapshot.TPMApplicable = true, true
		input, output, total, err = taskChannelTokenBudget(c, info, request)
		if err != nil {
			return err
		}
		if total == nil && attempt.MaxInputTokens > 0 && attempt.MaxOutputTokens > 0 && attempt.MaxInputTokens <= model.ChannelLimitMaxValue-attempt.MaxOutputTokens {
			input, output = &attempt.MaxInputTokens, &attempt.MaxOutputTokens
			value := *input + *output
			total = &value
		}
	}
	if err := AdmitChannelEvent(c, info, input, output, total); err != nil {
		return err
	}
	info.TaskChannelSnapshot = snapshot
	return nil
}

// Only the final standard protocol body can provide request-derived text
// bounds. Native plugin quantities and submit estimates are not upper bounds.
func taskChannelTokenBudget(c *gin.Context, info *relaycommon.RelayInfo, request *http.Request) (input, output, total *int64, err error) {
	if request == nil || request.URL == nil || request.Body == nil || request.Method != http.MethodPost {
		return
	}
	mediaType, _, parseErr := mime.ParseMediaType(request.Header.Get("Content-Type"))
	if parseErr != nil || mediaType != "application/json" {
		return
	}
	copy := *info
	switch {
	case strings.HasSuffix(request.URL.Path, "/chat/completions"):
		copy.FinalRequestRelayFormat, copy.RelayMode = types.RelayFormatOpenAI, relayconstant.RelayModeChatCompletions
	case strings.HasSuffix(request.URL.Path, "/responses"):
		copy.FinalRequestRelayFormat, copy.RelayMode = types.RelayFormatOpenAIResponses, relayconstant.RelayModeResponses
	default:
		return
	}
	reader := request.Body
	if request.GetBody != nil {
		reader, err = request.GetBody()
		if err != nil {
			return
		}
	}
	maxBytes := int64(constant.MaxRequestBodyMB) * 1024 * 1024
	if maxBytes <= 0 {
		maxBytes = 128 * 1024 * 1024
	}
	body, readErr := io.ReadAll(io.LimitReader(reader, maxBytes+1))
	_ = reader.Close()
	if request.GetBody == nil {
		request.Body = io.NopCloser(bytes.NewReader(body))
		request.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(body)), nil }
	}
	if readErr != nil || int64(len(body)) > maxBytes {
		return nil, nil, nil, model.ErrChannelLimitAccounting
	}
	input, output, total = channelLimitTokenBudget(c, &copy, body)
	return
}

// ResolveTaskChannelSnapshot returns a private connection copy, independent
// of channel edits, key rotation, pauses and budget exhaustion after submit.
func ResolveTaskChannelSnapshot(snapshot *relaycommon.TaskChannelSnapshot) (*model.Channel, error) {
	if snapshot == nil {
		return nil, model.ErrChannelAttemptState
	}
	data := snapshot.Connection
	if snapshot.Encrypted {
		var err error
		data, err = model.DecryptSupplierSecret(snapshot.SupplierID, data)
		if err != nil {
			return nil, err
		}
	}
	var meta relaycommon.ChannelMeta
	if common.UnmarshalJsonStr(data, &meta) != nil || meta.ChannelId != snapshot.ChannelID || meta.ApiKey == "" {
		return nil, model.ErrChannelAttemptState
	}
	channel := &model.Channel{Id: snapshot.ChannelID, Type: meta.ChannelType, Key: meta.ApiKey, BaseURL: &meta.ChannelBaseUrl, OwnerSupplierID: snapshot.SupplierID, SecretsWriteOnly: snapshot.Encrypted}
	channel.SetSetting(meta.ChannelSetting)
	channel.SetOtherSettings(meta.ChannelOtherSettings)
	if len(meta.HeadersOverride) > 0 {
		value, err := common.Marshal(meta.HeadersOverride)
		if err != nil {
			return nil, err
		}
		channel.HeaderOverride = common.GetPointer(string(value))
	}
	if snapshot.SupplierID != "" {
		if !snapshot.Encrypted {
			return nil, model.ErrChannelAttemptState
		}
		if err := ValidateSupplierURL(channel.GetBaseURL()); err != nil {
			return nil, err
		}
	}
	return channel, nil
}

func ResolveTaskChannelConnection(task *model.Task) (*model.Channel, error) {
	if task.PrivateData.ChannelAttempt != nil {
		return ResolveTaskChannelSnapshot(task.PrivateData.ChannelAttempt)
	}
	channel, err := model.CacheGetChannel(task.ChannelId)
	if err != nil {
		return nil, err
	}
	if channel.CredentialUnavailable() {
		// A credential this node cannot open must never poll upstream.
		return nil, model.ErrChannelAttemptState
	}
	copy := *channel
	copy.Key = TaskPollingKey(channel, task.PrivateData.Key, task.PrivateData.FundingAttempt)
	copy.Keys, copy.ChannelInfo = nil, model.ChannelInfo{}
	return &copy, nil
}

// ObserveTaskChannelSnapshot changes only the durable procurement observation.
// The caller commits it together with its task CAS before flushing the outbox.
func ObserveTaskChannelSnapshot(snapshot *relaycommon.TaskChannelSnapshot, result *relaycommon.TaskInfo, authoritative bool) {
	if snapshot == nil {
		return
	}
	status := "unknown"
	if result != nil && result.Unconfirmed {
		authoritative = false
	}
	if authoritative && result != nil {
		switch model.TaskStatus(result.Status) {
		case model.TaskStatusSubmitted, model.TaskStatusNotStart:
			status = "accepted"
		case model.TaskStatusQueued:
			status = "queued"
		case model.TaskStatusInProgress:
			status = "running"
		case model.TaskStatusSuccess:
			status = "succeeded"
		case model.TaskStatusFailure:
			status = "failed"
		default:
			authoritative = false
		}
	}
	previous := snapshot.Observation
	if previous.Authoritative && previous.CompletedAt != nil && (!authoritative || status != "succeeded" && status != "failed") {
		return
	}
	// Repeated old status is not evidence of progress and cannot prolong an
	// unknown-result deadline after a crash or an unresponsive provider.
	progress := ""
	if result != nil {
		progress = result.Progress
	}
	if authoritative && status != "succeeded" && status != "failed" {
		if snapshot.LastConfirmedStatus == status && snapshot.LastConfirmedProgress == progress {
			return
		}
		snapshot.LastConfirmedStatus, snapshot.LastConfirmedProgress = status, progress
	}
	if previous.Status == status && previous.Progress == progress && status != "succeeded" && status != "failed" {
		return
	}
	observation := relaycommon.TaskChannelObservation{Status: status, Authoritative: authoritative, ObservedAt: time.Now().Unix(), Progress: progress}
	if status == "succeeded" || status == "failed" {
		observation.CompletedAt = common.GetPointer(observation.ObservedAt)
		if previous.Status == status && previous.Authoritative && previous.CompletedAt != nil {
			observation.CompletedAt = previous.CompletedAt
		}
	}
	if authoritative && result != nil {
		facts := result.UsageFacts
		for key, unit := range snapshot.UsageUnits {
			value, exists := facts[key]
			if !exists {
				continue
			}
			encoded, err := common.Marshal(value)
			if err != nil {
				continue
			}
			quantity, err := decimal.NewFromString(string(encoded))
			if err != nil || quantity.IsNegative() {
				continue
			}
			maximum, ok := TaskChannelUsageBounds(map[string]string{key: unit})[key]
			if !ok || quantity.GreaterThan(decimal.NewFromFloat(maximum)) {
				continue
			}
			switch {
			case key == "image_count" && unit == "count" && quantity.Equal(quantity.Truncate(0)):
				observation.ImageCount = common.GetPointer(quantity.IntPart())
			case (key == "video_seconds" || key == "seconds" && snapshot.MediaKind == "video") && unit == "second":
				v, _ := quantity.Float64()
				observation.VideoSeconds = &v
			case (key == "audio_seconds" || key == "seconds" && snapshot.MediaKind == "audio") && unit == "second":
				v, _ := quantity.Float64()
				observation.AudioSeconds = &v
			case unit == "credit":
				if observation.Credits == nil {
					observation.Credits = common.GetPointer(quantity.String())
				} else {
					sum, _ := decimal.NewFromString(*observation.Credits)
					observation.Credits = common.GetPointer(sum.Add(quantity).String())
				}
			}
		}
		if snapshot.TPMApplicable {
			var input, output *int64
			for _, pair := range []struct {
				keys   []string
				target **int64
			}{{[]string{"input_tokens", "prompt_tokens"}, &input}, {[]string{"output_tokens", "completion_tokens"}, &output}} {
				for _, key := range pair.keys {
					if snapshot.UsageUnits[key] != "token" {
						continue
					}
					raw, exists := facts[key]
					if !exists {
						continue
					}
					data, _ := common.Marshal(raw)
					v, err := decimal.NewFromString(string(data))
					if err == nil && !v.IsNegative() && v.Equal(v.Truncate(0)) && v.LessThanOrEqual(decimal.NewFromInt(int64(common.MaxQuota))) {
						*pair.target = common.GetPointer(v.IntPart())
						break
					}
				}
			}
			if input != nil && output != nil {
				observation.Tokens = common.GetPointer(*input + *output)
			}
		}
		observation.CostUSDNano = taskChannelProcurementCost(snapshot, status, result)
		if status == "failed" && observation.Tokens == nil && snapshot.TPMApplicable {
			observation.Tokens = common.GetPointer(int64(0))
		}
	}
	snapshot.Observation = observation
}

// taskChannelProcurementCost is the procurement amount of a task observation:
// the amount the customer task settlement charges before the group ratio, at
// the frozen submit price, times the channel cost ratio. A failed task costs
// nothing unless the vendor reports usage for it. Nil means not yet known.
func taskChannelProcurementCost(snapshot *relaycommon.TaskChannelSnapshot, status string, result *relaycommon.TaskInfo) *int64 {
	if snapshot.CostRatio == nil {
		return nil
	}
	if *snapshot.CostRatio == 0 {
		return common.GetPointer(int64(0))
	}
	var facts map[string]any
	tokens := 0
	if result != nil {
		facts = result.UsageFacts
		tokens = result.TotalTokens
		if tokens == 0 {
			tokens = result.CompletionTokens
		}
	}
	otherRatios := func(base float64) float64 {
		for _, ratio := range snapshot.OtherRatios {
			base *= ratio
		}
		return base
	}
	var base float64
	switch {
	case snapshot.Expression != nil && (len(facts) > 0 || status == "succeeded"):
		// The customer settlement evaluates the frozen expression on the submit
		// facts merged with any completion facts, and keeps the reservation
		// when evaluation fails.
		settlement, _, err := EvaluateTaskCompletionUsage(snapshot.Expression, facts)
		switch {
		case err == nil && settlement.Clamp != nil:
			return nil
		case err == nil:
			base = settlement.ActualQuotaBeforeGroup
		case status == "succeeded":
			base = snapshot.Expression.EstimatedQuotaBeforeGroup
		default:
			base = 0
		}
	case status == "failed" && snapshot.Expression == nil && !snapshot.UsePrice && tokens > 0 && int64(tokens) <= int64(common.MaxQuota):
		base = otherRatios(float64(tokens) * snapshot.ModelRatio)
	case status == "failed":
		base = 0
	case status != "succeeded":
		return nil
	case snapshot.UsePrice:
		base = otherRatios(snapshot.ModelPrice * snapshot.QuotaPerUnit)
	case tokens > 0 && int64(tokens) <= int64(common.MaxQuota):
		base = otherRatios(float64(tokens) * snapshot.ModelRatio)
	default:
		// Without token usage the customer keeps the per-request reservation,
		// half the model ratio per call.
		base = otherRatios(snapshot.ModelRatio / 2 * snapshot.QuotaPerUnit)
	}
	cost, err := model.ProcurementCostUSDNano(base, *snapshot.CostRatio, snapshot.QuotaPerUnit)
	if err != nil {
		return nil
	}
	return &cost
}

func ObserveTaskChannelResult(task *model.Task, result *relaycommon.TaskInfo, authoritative bool) {
	ObserveTaskChannelSnapshot(task.PrivateData.ChannelAttempt, result, authoritative)
}

// SettleTaskChannelSnapshot has no model.Task dependency, so legacy task
// stores can use exactly the same ledger and observation projection.
func SettleTaskChannelSnapshot(ctx context.Context, snapshot *relaycommon.TaskChannelSnapshot, version int64, now time.Time) error {
	if snapshot == nil {
		return nil
	}
	observation := snapshot.Observation
	var err error
	if observation.Authoritative && (observation.Status == "succeeded" || observation.Status == "failed") {
		err = model.SettleChannelAttempt(model.ChannelAttemptSettlement{ID: snapshot.AttemptID, ChannelID: snapshot.ChannelID, Version: version, Tokens: observation.Tokens, CostUSDNano: observation.CostUSDNano}, now)
	} else if observation.Status == "unknown" {
		observed := int64(0)
		if observation.Tokens != nil {
			observed = *observation.Tokens
		}
		if observation.CostUSDNano != nil {
			err = model.MarkChannelAttemptUnknown(snapshot.ChannelID, snapshot.AttemptID, observed, now, *observation.CostUSDNano)
		} else {
			err = model.MarkChannelAttemptUnknown(snapshot.ChannelID, snapshot.AttemptID, observed, now)
		}
	} else if observation.ObservedAt > 0 {
		if now.IsZero() {
			now, err = model.ChannelLimitDatabaseTime(model.DB.WithContext(ctx))
			if err != nil {
				return err
			}
		}
		// ObservedAt comes from this gateway's clock, now from the database.
		// Tolerate a gateway running slightly ahead of the database.
		if age := now.Unix() - observation.ObservedAt; age >= -taskObservationClockSkewSeconds && age < 60 {
			err = model.ConfirmChannelAttemptRunning(snapshot.ChannelID, snapshot.AttemptID, 0, time.Unix(observation.ObservedAt, 0))
			if errors.Is(err, model.ErrChannelAttemptState) {
				err = nil
			}
		}
	}
	if errors.Is(err, gorm.ErrRecordNotFound) {
		// The attempt was pruned after its retention window; a correction
		// that late cannot change any active limit or budget.
		common.SysLog(common.LogText("task procurement correction skipped for pruned channel attempt %s (channel #%d)", snapshot.AttemptID, snapshot.ChannelID))
		err = nil
	}
	if err != nil {
		return err
	}
	return model.ApplyChannelTaskObservation(ctx, snapshot, version)
}

const taskObservationClockSkewSeconds = 5

func FlushTaskChannelObservation(ctx context.Context, task *model.Task) error {
	if task.PrivateData.ChannelAttempt == nil || !task.ProcurementPending {
		return nil
	}
	// Read the committed outbox. A caller that lost its CAS may still hold an
	// edited observation in memory, which must never reach the ledger.
	var committed model.Task
	err := model.DB.WithContext(ctx).Where("id = ? AND procurement_version = ? AND procurement_pending = ?", task.ID, task.ProcurementVersion, true).First(&committed).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return nil
	}
	if err != nil {
		return err
	}
	if err := SettleTaskChannelSnapshot(ctx, committed.PrivateData.ChannelAttempt, committed.ProcurementVersion, time.Time{}); err != nil {
		return err
	}
	result := model.DB.WithContext(ctx).Model(&model.Task{}).Where("id = ? AND procurement_version = ?", task.ID, task.ProcurementVersion).Update("procurement_pending", false)
	if result.Error == nil && result.RowsAffected > 0 {
		task.ProcurementPending = false
	}
	return result.Error
}

func FinishTaskChannelSubmission(c *gin.Context, info *relaycommon.RelayInfo, accepted bool, result *relaycommon.TaskInfo, submitErr error) {
	attempt := info.ChannelLimitAttempt
	if attempt == nil || !attempt.Sent {
		return
	}
	liveChannelLimitAttempts.Delete(attempt.ID)
	var apiErr *types.NewAPIError
	if submitErr != nil {
		apiErr = types.NewError(submitErr, types.ErrorCodeBadResponse)
	}
	FinishOverviewAttempt(c, info, apiErr)
	if info.OverviewAttempt != nil {
		info.OverviewAttempt.Success, info.OverviewAttempt.Failure = accepted, !accepted && (c.GetInt("task_submit_http_status") >= 300 || c.GetBool("task_submit_rejected"))
		info.OverviewAttempt.UpstreamStatusCode = c.GetInt("task_submit_http_status")
	}
	CompleteOverviewRelay(info)
	snapshot := info.TaskChannelSnapshot
	if snapshot == nil {
		_ = model.MarkChannelAttemptUnknown(attempt.ChannelID, attempt.ID, 0, time.Time{})
		return
	}
	if accepted {
		if result != nil && !result.Unconfirmed {
			snapshot.OtherRatios = info.PriceData.OtherRatios()
		}
		if result == nil {
			result = &relaycommon.TaskInfo{Status: string(model.TaskStatusSubmitted)}
		}
		ObserveTaskChannelSnapshot(snapshot, result, true)
	} else if c.GetInt("task_submit_http_status") >= http.StatusMultipleChoices || c.GetBool("task_submit_rejected") {
		// The vendor rejected the submission: no work runs and nothing is owed.
		ObserveTaskChannelSnapshot(snapshot, &relaycommon.TaskInfo{Status: string(model.TaskStatusFailure)}, true)
		_ = SettleTaskChannelSnapshot(context.WithoutCancel(c.Request.Context()), snapshot, 1, time.Time{})
	} else {
		ObserveTaskChannelSnapshot(snapshot, nil, false)
		_ = SettleTaskChannelSnapshot(context.WithoutCancel(c.Request.Context()), snapshot, 1, time.Time{})
	}
	attempt.Finished = true
}

// SettleUnpersistedTaskChannelAttempt settles an accepted submission that will
// never get a task row (client cancellation after submit, a reservation or
// insert failure), so no polling can settle it later. A terminal immediate
// result settles with its cost. Otherwise the vendor keeps the job: the
// attempt is marked unknown, holding the frozen submit price as its lower
// bound, instead of being left to lapse without any record.
func SettleUnpersistedTaskChannelAttempt(c *gin.Context, info *relaycommon.RelayInfo, immediate *relaycommon.TaskInfo) {
	if info == nil || info.ChannelLimitAttempt == nil || !info.ChannelLimitAttempt.Sent {
		return
	}
	attempt := info.ChannelLimitAttempt
	ctx := context.WithoutCancel(c.Request.Context())
	snapshot := info.TaskChannelSnapshot
	var err error
	switch {
	case snapshot == nil:
		err = model.MarkChannelAttemptUnknown(attempt.ChannelID, attempt.ID, 0, time.Time{})
	case immediate != nil && !immediate.Unconfirmed && (model.TaskStatus(immediate.Status) == model.TaskStatusSuccess || model.TaskStatus(immediate.Status) == model.TaskStatusFailure):
		ObserveTaskChannelSnapshot(snapshot, immediate, true)
		err = SettleTaskChannelSnapshot(ctx, snapshot, 2, time.Time{})
	default:
		snapshot.Observation = relaycommon.TaskChannelObservation{Status: "unknown", ObservedAt: time.Now().Unix(),
			CostUSDNano: taskChannelProcurementCost(snapshot, "succeeded", nil)}
		err = SettleTaskChannelSnapshot(ctx, snapshot, 2, time.Time{})
	}
	if err != nil {
		common.SysError(common.LogText("task procurement settlement without a task row failed: channel #%d, attempt %s: %s", attempt.ChannelID, attempt.ID, err.Error()))
	}
	attempt.Finished = true
}

var taskChannelWorker sync.Once

// PollTaskChannelOnce is shared by validation probes and late-result recovery.
// It does not select a new channel or require a consumer API key.
func PollTaskChannelOnce(ctx context.Context, task *model.Task) error {
	if task == nil || GetTaskAdaptorFunc == nil {
		return model.ErrChannelAttemptState
	}
	adaptor := GetTaskAdaptorFunc(task.Platform)
	if adaptor == nil {
		return model.ErrChannelAttemptState
	}
	channel, err := ResolveTaskChannelConnection(task)
	if err != nil {
		return recordPollFailure(ctx, adaptor, task, task.Status, pollClassTransport, 0, "task connection unavailable")
	}
	if batch, ok := adaptor.(BatchTaskPollingAdaptor); ok && batch.FetchMode() == "batch" {
		return pollCredentialBatch(ctx, batch, channel, channel.Key, []*model.Task{task})
	}
	return updateVideoSingleTask(ctx, adaptor, channel, task.TaskID, map[string]*model.Task{task.TaskID: task})
}

func StartTaskChannelLimitWorker() {
	taskChannelWorker.Do(func() {
		pollSlots := make(chan struct{}, 4)
		go func() {
			ticker := time.NewTicker(5 * time.Second)
			defer ticker.Stop()
			for range ticker.C {
				var tasks []model.Task
				if model.DB.Where("procurement_pending = ?", true).Order("id ASC").Limit(200).Find(&tasks).Error != nil {
					continue
				}
				for i := range tasks {
					if err := FlushTaskChannelObservation(context.Background(), &tasks[i]); err != nil {
						common.SysError("task procurement reconciliation failed")
					}
				}
				// A consumer timeout or refund must not prevent later procurement
				// corrections. Claims are persistent and shared across instances.
				var unresolved []model.Task
				now := time.Now().Unix()
				if model.DB.Where("procurement_unknown = ? AND procurement_next_poll <= ? AND status IN ?", true, now, []model.TaskStatus{model.TaskStatusFailure, model.TaskStatusSuccess}).Order("procurement_next_poll ASC").Limit(4).Find(&unresolved).Error != nil {
					continue
				}
				for i := range unresolved {
					select {
					case pollSlots <- struct{}{}:
					default:
						continue
					}
					task := &unresolved[i]
					claim := model.DB.Model(&model.Task{}).Where("id = ? AND procurement_version = ? AND procurement_next_poll = ?", task.ID, task.ProcurementVersion, task.ProcurementNextPoll).Update("procurement_next_poll", now+180)
					if claim.Error != nil || claim.RowsAffected != 1 {
						<-pollSlots
						continue
					}
					go func(task *model.Task) {
						defer func() { <-pollSlots }()
						ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
						defer cancel()
						_ = PollTaskChannelOnce(ctx, task)
					}(task)
				}
			}
		}()
	})
}

// FrozenTaskUsageUnits exposes only validated, declared unit metadata.
func FrozenTaskUsageUnits(plugin *jsplugin.LoadedPlugin, models ...string) map[string]string {
	if plugin == nil {
		return nil
	}
	schema, _ := plugin.Meta.UsageForModels(models...)
	units := make(map[string]string, len(schema))
	for key, field := range schema {
		if field.Type == "number" {
			units[key] = field.Unit
		}
	}
	return units
}
