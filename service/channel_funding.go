package service

import (
	"errors"
	"fmt"
	"net/http"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	relaytypes "github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	hosttypes "github.com/QuantumNous/new-api/types"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"gorm.io/gorm"
)

const fundingAttemptKey = "channel_funding_attempt"

var fundingAccountLimitMessage = regexp.MustCompile(`(?:reached|exceeded|exhausted) (?:your |its |the |their |our )?(?:configured |maximum )?(?:monthly (?:spending|spend)|account spending|organization spending|project spending) limit|(?:monthly (?:spending|spend)|account spending|organization spending|project spending) limit (?:has been |is )?(?:reached|exceeded|exhausted)|账户消费额度已用尽|账号消费额度已用尽|已达到月度消费上限`)

type FundingAttempt struct {
	Identity  hosttypes.ChannelFundingAttempt
	secret    string
	mu        sync.Mutex
	recorded  bool
	observed  bool
	confirmed bool
	rawStatus int
}

type FundingClassification struct{ Category, Rule, Code, Message string }

func BeginChannelFundingAttempt(c *gin.Context, channel *model.Channel, key string, index int, modelName string) {
	c.Set(fundingAttemptKey, (*FundingAttempt)(nil))
	if !operation_setting.GetChannelFundingSetting().Enabled {
		return
	}
	snapshot := channel.GetUpstreamCredentialSnapshot(index)
	if snapshot.BindingID == "" {
		return
	}
	keys := []string{channel.Key}
	if channel.ChannelInfo.IsMultiKey {
		keys = channel.GetKeys()
	}
	if index < 0 || index >= len(keys) || keys[index] != key {
		return
	}
	endpoint := ""
	if c.Request != nil && c.Request.URL != nil {
		endpoint = c.Request.URL.Path
	}
	attempt := &FundingAttempt{Identity: hosttypes.ChannelFundingAttempt{ChannelID: channel.Id, ChannelType: channel.Type, BindingID: snapshot.BindingID, BindingActivatedAt: snapshot.BindingActivatedAt, VersionID: snapshot.CredentialVersionID, KeyIndex: index, KeyAlias: snapshot.Alias, Model: modelName, Endpoint: endpoint, RequestID: c.GetString(common.RequestIdKey), StartedAt: time.Now().UnixMilli(), Source: "http"}, secret: key}
	if c.GetString("task_plugin_key") != "" || channel.Type == constant.ChannelTypeTaskPlugin || strings.Contains(endpoint, "/mj/") {
		attempt.Identity.Source = "task_submit"
	}
	c.Set(fundingAttemptKey, attempt)
}

func ChannelFundingAttempt(c *gin.Context) *FundingAttempt {
	if c == nil {
		return nil
	}
	value, _ := c.Get(fundingAttemptKey)
	attempt, _ := value.(*FundingAttempt)
	return attempt
}

// ClassifyChannelFundingError only accepts protocol error fields, never output
// text or request bodies. A generic quota error does not establish account debt.
func ClassifyChannelFundingError(channelType, status int, code, kind, message string) FundingClassification {
	code, kind = strings.ToLower(strings.TrimSpace(code)), strings.ToLower(strings.TrimSpace(kind))
	lower := strings.ToLower(strings.TrimSpace(message))
	result := FundingClassification{Code: code, Message: message}
	if status == http.StatusUnauthorized || status == http.StatusForbidden && (code == "permission_denied" || code == "invalid_api_key") {
		return result
	}
	switch code {
	case "insufficient_user_quota", "pre_consume_token_quota_failed", "do_request_failed", "rate_limit_exceeded", "rate_limit_error", "slow_down", "invalid_api_key", "authentication_error":
		return result
	}
	if kind == "new_api_error" {
		return result
	}
	switch kind {
	case "authentication_error", "permission_error", "permission_denied", "authorization_error", "invalid_api_key":
		return result
	}
	if status == http.StatusRequestTimeout || status == http.StatusGatewayTimeout || status == 499 {
		return result
	}
	switch code {
	case "credit_balance_exhausted", "insufficient_balance", "insufficient_balance_error", "balance_insufficient", "insufficient_credits":
		result.Category, result.Rule = "balance_insufficient", "balance_code"
	case "organization_spend_limit_exceeded", "org_spend_limit_exceeded", "project_spend_limit_exceeded", "organization_usage_limit_exceeded", "org_usage_limit_exceeded", "billing_hard_limit_reached", "enforced_spend_limit_reached":
		result.Category, result.Rule = "quota_limited", "account_limit_code"
	}
	if result.Category == "" && channelType == constant.ChannelTypeDeepSeek && status == http.StatusPaymentRequired {
		result.Category, result.Rule = "balance_insufficient", "deepseek_payment_required"
	}
	for _, phrase := range []string{"insufficient balance", "balance is insufficient", "credit balance is too low", "credit balance too low", "account balance is insufficient", "account has insufficient balance", "余额不足", "账户欠费", "账号欠费", "账户已欠费"} {
		if result.Category == "" && strings.Contains(lower, phrase) {
			result.Category, result.Rule = "balance_insufficient", "explicit_balance_message"
			break
		}
	}
	if result.Category == "" && fundingAccountLimitMessage.MatchString(lower) {
		result.Category, result.Rule = "quota_limited", "explicit_account_limit_message"
	}
	if kind == "rate_limit_error" && result.Category != "quota_limited" && result.Rule != "balance_code" {
		result.Category, result.Rule = "", ""
		return result
	}
	for _, phrase := range []string{"rate limit", "too many requests", "per minute", "per second", "per_minute", "per_second", "perminute", "persecond", "请求过于频繁"} {
		if strings.Contains(lower, phrase) {
			result.Category, result.Rule = "", ""
			return result
		}
	}
	if result.Category == "" && (code == "insufficient_quota" || kind == "insufficient_quota" || kind == "billing_error" || status == http.StatusPaymentRequired || strings.Contains(lower, "quota") || strings.Contains(lower, "billing")) {
		result.Category, result.Rule = "suspected", "ambiguous_funding_error"
	}
	if result.Category == "" {
		return result
	}
	for _, phrase := range []string{"fewer max_tokens", "reduce max_tokens", "requires more credits", "for this request", "for the request", "for requested", "requested tokens", "token budget", "request budget", "single request", "per-model", "per model", "model quota", "quota for model", "model budget", "model spending", "model-specific", "模型额度", "模型配额", "单次请求", "not due to", "not caused by"} {
		if strings.Contains(lower, phrase) {
			result.Category, result.Rule = "suspected", "request_or_model_budget"
			break
		}
	}
	return result
}

// ObserveChannelFundingJSON reads only recognized error envelopes. Successful
// task snapshots and model output are never searched for financial keywords.
func ObserveChannelFundingJSON(attempt *FundingAttempt, status int, body []byte) bool {
	if attempt == nil || !operation_setting.GetChannelFundingSetting().Enabled {
		return false
	}
	var code, kind, message string
	if gjson.ValidBytes(body) {
		root := gjson.ParseBytes(body)
		errBody := root.Get("error")
		if !errBody.Exists() || errBody.Type == gjson.Null {
			errBody = root.Get("response.error")
		}
		if !errBody.Exists() || errBody.Type == gjson.Null {
			errBody = root.Get("response.status_details.error")
		}
		if errBody.IsObject() {
			code, kind, message = errBody.Get("code").String(), errBody.Get("type").String(), errBody.Get("message").String()
			if detail := errBody.Get("details.error_code").String(); detail != "" {
				code = detail
			}
		} else if errBody.Type == gjson.String {
			message = errBody.String()
		} else if status >= 400 || root.Get("type").String() == "error" {
			code, kind = root.Get("code").String(), root.Get("type").String()
			for _, key := range []string{"message", "msg", "error_msg", "detail", "description"} {
				if message = root.Get(key).String(); message != "" {
					break
				}
			}
		}
	}
	return RecordChannelFundingError(attempt, status, code, kind, message)
}

func RecordChannelFundingError(attempt *FundingAttempt, status int, code, kind, message string) bool {
	setting := operation_setting.GetChannelFundingSetting()
	if attempt == nil || !setting.Enabled {
		return false
	}
	attempt.mu.Lock()
	defer attempt.mu.Unlock()
	if attempt.recorded {
		return attempt.confirmed
	}
	classified := ClassifyChannelFundingError(attempt.Identity.ChannelType, status, code, kind, message)
	if classified.Category == "" {
		return false
	}
	attempt.observed = true
	if attempt.secret == "" {
		if channel, err := model.GetChannelById(attempt.Identity.ChannelID, true); err == nil {
			keys := []string{channel.Key}
			if channel.ChannelInfo.IsMultiKey {
				keys = channel.GetKeys()
			}
			for index, key := range keys {
				snapshot := channel.GetUpstreamCredentialSnapshot(index)
				if snapshot.BindingID == attempt.Identity.BindingID && snapshot.CredentialVersionID == attempt.Identity.VersionID && snapshot.BindingActivatedAt == attempt.Identity.BindingActivatedAt {
					attempt.secret = key
					break
				}
			}
		}
	}
	identity := attempt.Identity
	if attempt.secret != "" {
		classified.Message = strings.ReplaceAll(classified.Message, attempt.secret, "[redacted]")
		classified.Code = strings.ReplaceAll(classified.Code, strings.ToLower(attempt.secret), "[redacted]")
		identity.KeyAlias = strings.ReplaceAll(identity.KeyAlias, attempt.secret, "[redacted]")
	}
	classified.Message, _ = model.SafeFailureDetail(classified.Message, 2048)
	identity.RequestID = model.SafeFailureFact(identity.RequestID, 128)
	identity.KeyAlias = model.SafeFailureFact(identity.KeyAlias, 100)
	disable, _ := common.ChannelAutomationSwitches()
	row, err := model.RecordChannelFundingFailure(identity, classified.Category, classified.Rule, model.SafeFailureFact(classified.Code, 128), classified.Message, status, disable, max(1, setting.ProbeMinutes))
	if err != nil {
		if !errors.Is(err, gorm.ErrRecordNotFound) && !errors.Is(err, model.ErrFundingConflict) {
			common.SysError(fmt.Sprintf("channel funding observation failed: channel_id=%d error=%v", attempt.Identity.ChannelID, err))
		}
		return false
	}
	attempt.recorded, attempt.confirmed = true, row.State == "confirmed"
	if row.AutoDisabled && shouldCloseActiveWebSocketsAfterDisable(row.ChannelID) {
		CloseActiveWebSocketsForChannel(row.ChannelID, ChannelDisabledCloseReason)
	}
	if row.NotificationQueued {
		_, _, _ = EnqueueSystemTask(ChannelFundingNotifyTaskType, nil)
	}
	return attempt.confirmed
}

func ProcessChannelFundingError(c *gin.Context, err *relaytypes.NewAPIError) bool {
	attempt := ChannelFundingAttempt(c)
	if attempt == nil || err == nil {
		return false
	}
	if err.GetErrorType() == relaytypes.ErrorTypeNewAPIError {
		attempt.mu.Lock()
		defer attempt.mu.Unlock()
		return attempt.observed
	}
	upstream := err.ToOpenAIError(common.UpstreamAddresses(c)...)
	attempt.mu.Lock()
	status := attempt.rawStatus
	attempt.mu.Unlock()
	// A configured downstream status mapping is not upstream evidence. SDK
	// paths without a raw response can still use explicit provider error fields.
	RecordChannelFundingError(attempt, status, fmt.Sprint(upstream.Code), upstream.Type, upstream.Message)
	attempt.mu.Lock()
	defer attempt.mu.Unlock()
	return attempt.observed
}

func ObserveChannelFundingSuccess(attempt *FundingAttempt) {
	setting := operation_setting.GetChannelFundingSetting()
	if attempt == nil || !setting.Enabled {
		return
	}
	attempt.mu.Lock()
	recorded := attempt.observed
	attempt.mu.Unlock()
	if recorded {
		return
	}
	var rows []model.ChannelFundingIncident
	if model.DB.Where("binding_id = ? AND credential_version_id = ? AND binding_activated_at = ? AND active_key IS NOT NULL AND model = ? AND endpoint = ?", attempt.Identity.BindingID, attempt.Identity.VersionID, attempt.Identity.BindingActivatedAt, attempt.Identity.Model, attempt.Identity.Endpoint).Find(&rows).Error != nil {
		return
	}
	for _, row := range rows {
		_, enable := common.ChannelAutomationSwitches()
		_ = model.ResolveChannelFunding(row.ID, row.Revision, attempt.Identity.StartedAt, enable, setting.RecoveryNotify, 0, "Successful upstream request")
	}
}

// EOF and HTTP 200 alone do not prove an inference completed. In particular,
// an interrupted stream or an empty success envelope cannot restore a key.
func ObserveChannelFundingResponseSuccess(c *gin.Context) {
	attempt := ChannelFundingAttempt(c)
	value, ok := c.Get(upstreamEvidenceKey)
	if attempt == nil || !ok {
		return
	}
	capture := value.(*upstreamEvidenceCapture)
	capture.mu.Lock()
	var body *upstreamEvidenceBody
	if len(capture.attempts) > 0 {
		body = capture.attempts[len(capture.attempts)-1]
	}
	capture.mu.Unlock()
	if body == nil || body.funding != attempt {
		return
	}
	body.mu.Lock()
	completed := body.evidence.StatusCode >= 200 && body.evidence.StatusCode < 300 && body.parser.completed && body.parser.fundingOutput && !body.parser.failed && !body.parser.incomplete && len(body.parser.pending) == 0 && len(body.parser.event) == 0
	body.mu.Unlock()
	if completed {
		ObserveChannelFundingSuccess(attempt)
	}
}

// FundingTaskAttempt uses the identity frozen at submission for terminal task
// failures, or the current actual polling credential for HTTP polling failures.
func FundingTaskAttempt(channel *model.Channel, key string, task *model.Task) *FundingAttempt {
	if channel == nil || !operation_setting.GetChannelFundingSetting().Enabled {
		return nil
	}
	keys := []string{channel.Key}
	if channel.ChannelInfo.IsMultiKey {
		keys = channel.GetKeys()
	}
	matched := -1
	for index, candidate := range keys {
		if candidate != key {
			continue
		}
		if task != nil && task.PrivateData.FundingAttempt != nil {
			identity := task.PrivateData.FundingAttempt
			snapshot := channel.GetUpstreamCredentialSnapshot(index)
			if snapshot.BindingID == identity.BindingID && snapshot.CredentialVersionID == identity.VersionID && snapshot.BindingActivatedAt == identity.BindingActivatedAt {
				matched = index
				break
			}
		}
		if matched != -1 {
			// Without a binding snapshot, equal material in multiple slots is
			// ambiguous. Do not disable an arbitrary slot.
			matched = -2
		} else {
			matched = index
		}
	}
	if matched >= 0 {
		index := matched
		snapshot := channel.GetUpstreamCredentialSnapshot(index)
		if snapshot.BindingID == "" {
			return nil
		}
		identity := hosttypes.ChannelFundingAttempt{ChannelID: channel.Id, ChannelType: channel.Type, BindingID: snapshot.BindingID, BindingActivatedAt: snapshot.BindingActivatedAt, VersionID: snapshot.CredentialVersionID, KeyIndex: index, KeyAlias: snapshot.Alias, Source: "task_poll", StartedAt: time.Now().UnixMilli()}
		if task != nil {
			identity.Model, identity.RequestID = task.Properties.OriginModelName, task.TaskID
		}
		return &FundingAttempt{Identity: identity, secret: key}
	}
	return nil
}

func ObserveTaskFundingFailure(task *model.Task, reason, code string) {
	if task.PrivateData.FundingAttempt == nil {
		return
	}
	attempt := &FundingAttempt{Identity: *task.PrivateData.FundingAttempt, secret: task.PrivateData.Key}
	attempt.Identity.Source, attempt.Identity.RequestID = "task_result", task.TaskID
	RecordChannelFundingError(attempt, http.StatusOK, code, "", reason)
}

func ObserveMidjourneyFundingFailure(task *model.Midjourney) {
	var identity hosttypes.ChannelFundingAttempt
	if task.FundingAttemptData == "" || common.UnmarshalJsonStr(task.FundingAttemptData, &identity) != nil {
		return
	}
	identity.Source, identity.RequestID = "task_result", task.MjId
	RecordChannelFundingError(&FundingAttempt{Identity: identity}, http.StatusOK, "", "", task.FailReason)
}
