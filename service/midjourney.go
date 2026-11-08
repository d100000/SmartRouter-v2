package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	relayconstant "github.com/QuantumNous/new-api/relay/constant"
	"github.com/QuantumNous/new-api/setting"
	hosttypes "github.com/QuantumNous/new-api/types"

	"github.com/gin-gonic/gin"
)

func CovertMjpActionToModelName(mjAction string) string {
	modelName := "mj_" + strings.ToLower(mjAction)
	if mjAction == constant.MjActionSwapFace {
		modelName = "swap_face"
	}
	return modelName
}

// PrepareMidjourneyTaskBilling sets the durable refund marker before the task is inserted.
func PrepareMidjourneyTaskBilling(relayInfo *relaycommon.RelayInfo, task *model.Midjourney, quota int, shouldBill bool) (bool, error) {
	if task == nil {
		return false, errors.New("Midjourney task is nil")
	}
	task.Quota = 0
	task.TokenId = 0
	task.BillingChannelId = 0
	if !shouldBill {
		return false, nil
	}
	if relayInfo == nil {
		return false, errors.New("relay info is nil")
	}
	if quota < 0 {
		return false, errors.New("quota cannot be negative")
	}
	if relayInfo.BillingSource == BillingSourceSubscription {
		return false, errors.New("legacy Midjourney billing does not support subscriptions")
	}

	task.Quota = quota
	task.BillingChannelId = task.ChannelId
	if relayInfo.ChannelMeta != nil && relayInfo.ChannelId > 0 {
		task.BillingChannelId = relayInfo.ChannelId
	}
	return true, nil
}

// SettleMidjourneyTaskBilling charges a persisted legacy task and records the applied stages.
func SettleMidjourneyTaskBilling(relayInfo *relaycommon.RelayInfo, task *model.Midjourney, prepared bool) (bool, error) {
	if !prepared {
		return false, nil
	}
	if relayInfo == nil {
		return false, errors.New("relay info is nil")
	}
	if task == nil || task.Id == 0 {
		return false, errors.New("Midjourney task must be persisted before billing")
	}

	result, billingErr := postConsumeQuotaWithResult(relayInfo, task.Quota, 0, true)
	if !result.FundingApplied {
		task.Quota = 0
		task.TokenId = 0
		task.BillingChannelId = 0
		if updateErr := task.UpdateBillingState(); updateErr != nil {
			return false, errors.Join(billingErr, fmt.Errorf("clear Midjourney billing state: %w", updateErr))
		}
		return false, billingErr
	}

	task.TokenId = 0
	if result.TokenApplied {
		task.TokenId = relayInfo.TokenId
	}
	if updateErr := task.UpdateBillingState(); updateErr != nil {
		return true, errors.Join(billingErr, fmt.Errorf("update Midjourney billing state: %w", updateErr))
	}
	return true, billingErr
}

// RefundMidjourneyQuota reverses every accounting element recorded for a billed legacy task.
func RefundMidjourneyQuota(ctx context.Context, task *model.Midjourney, reason string) bool {
	quota := task.Quota
	if quota == 0 {
		return true
	}

	if err := model.IncreaseUserQuota(task.UserId, quota, false); err != nil {
		logger.LogWarn(ctx, common.LogText("failed to refund Midjourney user quota, task %s: %s", task.MjId, err.Error()))
		return false
	}

	if task.TokenId > 0 {
		tokenKey := resolveTokenKey(ctx, task.TokenId, task.MjId)
		if tokenKey != "" {
			if err := model.IncreaseTokenQuota(task.TokenId, tokenKey, quota); err != nil {
				logger.LogWarn(ctx, common.LogText("failed to refund Midjourney token quota, task %s: %s", task.MjId, err.Error()))
			}
		}
	}

	billingChannelId := task.GetBillingChannelId()
	model.UpdateUserUsedQuota(task.UserId, -quota)
	model.UpdateChannelUsedQuota(billingChannelId, -quota)
	other := model.NewLogOther()
	other.SetPublic("task_id", task.MjId)
	other.SetPublic("reason", reason)
	model.RecordTaskBillingLog(model.RecordTaskBillingLogParams{
		UserId:    task.UserId,
		LogType:   model.LogTypeRefund,
		ChannelId: billingChannelId,
		ModelName: CovertMjpActionToModelName(task.Action),
		Quota:     quota,
		TokenId:   task.TokenId,
		Other:     other,
	})

	task.Quota = 0
	if err := task.UpdateBillingState(); err != nil {
		logger.LogError(ctx, common.LogText("Midjourney refund succeeded but clearing the quota failed, task %s: %s", task.MjId, err.Error()))
	}
	return true
}

func GetMjRequestModel(relayMode int, midjRequest *dto.MidjourneyRequest) (string, *dto.MidjourneyResponse, bool) {
	action := ""
	if relayMode == relayconstant.RelayModeMidjourneyAction {
		// plus request
		err := CoverPlusActionToNormalAction(midjRequest)
		if err != nil {
			return "", err, false
		}
		action = midjRequest.Action
	} else {
		switch relayMode {
		case relayconstant.RelayModeMidjourneyImagine:
			action = constant.MjActionImagine
		case relayconstant.RelayModeMidjourneyVideo:
			action = constant.MjActionVideo
		case relayconstant.RelayModeMidjourneyEdits:
			action = constant.MjActionEdits
		case relayconstant.RelayModeMidjourneyDescribe:
			action = constant.MjActionDescribe
		case relayconstant.RelayModeMidjourneyBlend:
			action = constant.MjActionBlend
		case relayconstant.RelayModeMidjourneyShorten:
			action = constant.MjActionShorten
		case relayconstant.RelayModeMidjourneyChange:
			action = midjRequest.Action
		case relayconstant.RelayModeMidjourneyModal:
			action = constant.MjActionModal
		case relayconstant.RelayModeSwapFace:
			action = constant.MjActionSwapFace
		case relayconstant.RelayModeMidjourneyUpload:
			action = constant.MjActionUpload
		case relayconstant.RelayModeMidjourneySimpleChange:
			params := ConvertSimpleChangeParams(midjRequest.Content)
			if params == nil {
				return "", MidjourneyErrorWrapper(constant.MjRequestError, "invalid_request"), false
			}
			action = params.Action
		case relayconstant.RelayModeMidjourneyTaskFetch, relayconstant.RelayModeMidjourneyTaskFetchByCondition, relayconstant.RelayModeMidjourneyNotify:
			return "", nil, true
		default:
			return "", MidjourneyErrorWrapper(constant.MjRequestError, "unknown_relay_action"), false
		}
	}
	modelName := CovertMjpActionToModelName(action)
	return modelName, nil, true
}

func CoverPlusActionToNormalAction(midjRequest *dto.MidjourneyRequest) *dto.MidjourneyResponse {
	// "customId": "MJ::JOB::upsample::2::3dbbd469-36af-4a0f-8f02-df6c579e7011"
	customId := midjRequest.CustomId
	if customId == "" {
		return MidjourneyErrorWrapper(constant.MjRequestError, "custom_id_is_required")
	}
	splits := strings.Split(customId, "::")
	var action string
	if splits[1] == "JOB" {
		action = splits[2]
	} else {
		action = splits[1]
	}

	if action == "" {
		return MidjourneyErrorWrapper(constant.MjRequestError, "unknown_action")
	}
	if strings.Contains(action, "upsample") {
		index, err := strconv.Atoi(splits[3])
		if err != nil {
			return MidjourneyErrorWrapper(constant.MjRequestError, "index_parse_failed")
		}
		midjRequest.Index = index
		midjRequest.Action = constant.MjActionUpscale
	} else if strings.Contains(action, "variation") {
		midjRequest.Index = 1
		if action == "variation" {
			index, err := strconv.Atoi(splits[3])
			if err != nil {
				return MidjourneyErrorWrapper(constant.MjRequestError, "index_parse_failed")
			}
			midjRequest.Index = index
			midjRequest.Action = constant.MjActionVariation
		} else if action == "low_variation" {
			midjRequest.Action = constant.MjActionLowVariation
		} else if action == "high_variation" {
			midjRequest.Action = constant.MjActionHighVariation
		}
	} else if strings.Contains(action, "pan") {
		midjRequest.Action = constant.MjActionPan
		midjRequest.Index = 1
	} else if strings.Contains(action, "reroll") {
		midjRequest.Action = constant.MjActionReRoll
		midjRequest.Index = 1
	} else if action == "Outpaint" {
		midjRequest.Action = constant.MjActionZoom
		midjRequest.Index = 1
	} else if action == "CustomZoom" {
		midjRequest.Action = constant.MjActionCustomZoom
		midjRequest.Index = 1
	} else if action == "Inpaint" {
		midjRequest.Action = constant.MjActionInPaint
		midjRequest.Index = 1
	} else {
		return MidjourneyErrorWrapper(constant.MjRequestError, "unknown_action:"+customId)
	}
	return nil
}

func ConvertSimpleChangeParams(content string) *dto.MidjourneyRequest {
	split := strings.Split(content, " ")
	if len(split) != 2 {
		return nil
	}

	action := strings.ToLower(split[1])
	changeParams := &dto.MidjourneyRequest{}
	changeParams.TaskId = split[0]

	if action[0] == 'u' {
		changeParams.Action = "UPSCALE"
	} else if action[0] == 'v' {
		changeParams.Action = "VARIATION"
	} else if action == "r" {
		changeParams.Action = "REROLL"
		return changeParams
	} else {
		return nil
	}

	index, err := strconv.Atoi(action[1:2])
	if err != nil || index < 1 || index > 4 {
		return nil
	}
	changeParams.Index = index
	return changeParams
}

// RecordMidjourneyPolicyResponse distinguishes accepted tasks from errors inside
// HTTP 200 responses. Submissions are single-attempt: an ambiguous transport
// failure must never create a duplicate task on another channel.
func RecordMidjourneyPolicyResponse(c *gin.Context, response *dto.MidjourneyResponseWithStatusCode, requestErr error) bool {
	if response == nil {
		response = MidjourneyErrorWithStatusCodeWrapper(constant.MjErrorUnknown, "empty_response", http.StatusBadGateway)
	}
	accepted := requestErr == nil && response.StatusCode == http.StatusOK && (response.Response.Code == 1 || response.Response.Code == 21 || response.Response.Code == 22)
	if accepted {
		properties, _ := response.Response.Properties.(map[string]any)
		if properties["status"] != "FAILURE" {
			return true
		}
	}
	state := RequestPolicy(c)
	event := PolicyEvent{ChannelID: c.GetInt("channel_id"), Status: response.StatusCode, ErrorCode: strconv.Itoa(response.Response.Code), ErrorSource: "upstream", Decision: PolicyDecision{Action: "failure", Reason: "upstream_failure", Source: "upstream"}}
	state.AddEvent(event)
	event.Decision, event.Health = PolicyDecision{Action: "stop", Reason: "non_retryable_error", Source: "system"}, "unchanged"
	if accepted {
		event.Decision.Reason = "task_accepted"
	}
	state.AddEvent(event)
	return false
}

func DoMidjourneyHttpRequest(c *gin.Context, timeout time.Duration, fullRequestURL string, infos ...*relaycommon.RelayInfo) (outcome *dto.MidjourneyResponseWithStatusCode, responseBody []byte, resultErr error) {
	var info *relaycommon.RelayInfo
	protected := false
	if len(infos) > 0 {
		info = infos[0]
	}
	if info != nil && info.TaskRelayInfo == nil {
		info.TaskRelayInfo = &relaycommon.TaskRelayInfo{}
	}
	defer func() {
		if protected && resultErr != nil {
			resultErr = errors.New("upstream request failed")
		}
		if info == nil || info.ChannelLimitAttempt == nil || !info.ChannelLimitAttempt.Sent {
			return
		}
		accepted := resultErr == nil && outcome != nil && outcome.StatusCode >= 200 && outcome.StatusCode < 300 && (outcome.Response.Code == 1 || outcome.Response.Code == 21 || outcome.Response.Code == 22)
		var result *relaycommon.TaskInfo
		if accepted {
			result = &relaycommon.TaskInfo{Status: "NOT_START"}
			if properties, ok := outcome.Response.Properties.(map[string]any); ok {
				if status, ok := properties["status"].(string); ok {
					result.Status = status
				}
			}
		}
		FinishTaskChannelSubmission(c, info, accepted, result, resultErr)
	}()
	var nullBytes []byte
	//var requestBody io.Reader
	//requestBody = c.Request.Body
	// read request body to json, delete accountFilter and notifyHook
	var mapResult map[string]any
	// if get request, no need to read request body
	if c.Request.Method != "GET" {
		err := common.DecodeJson(c.Request.Body, &mapResult)
		if err != nil {
			return MidjourneyErrorWithStatusCodeWrapper(constant.MjErrorUnknown, "read_request_body_failed", http.StatusInternalServerError), nullBytes, err
		}
		if !setting.MjAccountFilterEnabled {
			delete(mapResult, "accountFilter")
		}
		if !setting.MjNotifyEnabled {
			delete(mapResult, "notifyHook")
		}
		//req, err := http.NewRequest(c.Request.Method, fullRequestURL, requestBody)
		// make new request with mapResult
	}
	if setting.MjModeClearEnabled {
		if prompt, ok := mapResult["prompt"].(string); ok {
			prompt = strings.Replace(prompt, "--fast", "", -1)
			prompt = strings.Replace(prompt, "--relax", "", -1)
			prompt = strings.Replace(prompt, "--turbo", "", -1)

			mapResult["prompt"] = prompt
		}
	}
	reqBody, err := common.Marshal(mapResult)
	if err != nil {
		return MidjourneyErrorWithStatusCodeWrapper(constant.MjErrorUnknown, "marshal_request_body_failed", http.StatusInternalServerError), nullBytes, err
	}
	req, err := http.NewRequest(c.Request.Method, fullRequestURL, strings.NewReader(string(reqBody)))
	if err != nil {
		return MidjourneyErrorWithStatusCodeWrapper(constant.MjErrorUnknown, "create_request_failed", http.StatusInternalServerError), nullBytes, err
	}
	ctx, cancel := context.WithTimeout(c.Request.Context(), timeout)
	// 使用带有超时的 context 创建新的请求
	req = req.WithContext(ctx)
	req.Header.Set("Content-Type", c.Request.Header.Get("Content-Type"))
	req.Header.Set("Accept", c.Request.Header.Get("Accept"))
	auth := common.GetContextKeyString(c, constant.ContextKeyChannelKey)
	if auth != "" {
		auth = strings.TrimPrefix(auth, "Bearer ")
		req.Header.Set("mj-api-secret", auth)
	}
	defer cancel()
	var selected *model.Channel
	if info != nil {
		if info.LockedChannel != nil {
			selected, _ = info.LockedChannel.(*model.Channel)
		} else {
			selected, err = model.GetChannelById(info.ChannelId, true)
			if err != nil {
				return MidjourneyErrorWithStatusCodeWrapper(constant.MjErrorUnknown, "channel_unavailable", http.StatusServiceUnavailable), nil, err
			}
		}
		if info.TaskRelayInfo == nil {
			info.TaskRelayInfo = &relaycommon.TaskRelayInfo{}
		}
		if err = PrepareTaskChannelAttempt(info, selected); err != nil {
			return MidjourneyErrorWithStatusCodeWrapper(constant.MjErrorUnknown, "channel_limit_unavailable", http.StatusServiceUnavailable), nil, err
		}
		if selected == nil {
			return MidjourneyErrorWithStatusCodeWrapper(constant.MjErrorUnknown, "channel_unavailable", http.StatusServiceUnavailable), nil, model.ErrChannelAttemptState
		}
		PrepareOverviewAttempt(c, info, selected)
	} else if value, ok := c.Get("midjourney_query_channel"); ok {
		selected, _ = value.(*model.Channel)
		if selected == nil {
			return MidjourneyErrorWithStatusCodeWrapper(constant.MjErrorUnknown, "channel_unavailable", http.StatusServiceUnavailable), nil, model.ErrChannelAttemptState
		}
	} else {
		// A legacy caller without the durable submit runtime cannot bypass
		// supplier isolation or configured admission. Existing-task queries
		// supply their frozen connection through the explicit branch above.
		selected, err = model.GetChannelById(c.GetInt("channel_id"), true)
		if err != nil {
			return MidjourneyErrorWithStatusCodeWrapper(constant.MjErrorUnknown, "channel_unavailable", http.StatusServiceUnavailable), nil, err
		}
		state, stateErr := model.GetChannelLimitState(selected.Id, time.Now())
		if stateErr != nil {
			return MidjourneyErrorWithStatusCodeWrapper(constant.MjErrorUnknown, "channel_limit_unavailable", http.StatusServiceUnavailable), nil, stateErr
		}
		if selected.OwnerSupplierID != "" || selected.SecretsWriteOnly || state.Config.RPM > 0 || state.Config.TPM > 0 || state.Config.DailyUSDNano > 0 {
			return MidjourneyErrorWithStatusCodeWrapper(constant.MjErrorUnknown, "protocol_limit_unsupported", http.StatusServiceUnavailable), nil, &model.ChannelLimitError{Reason: "protocol_limit_unsupported"}
		}
	}
	client := GetHttpClient()
	protected = selected != nil && (selected.OwnerSupplierID != "" || selected.SecretsWriteOnly)
	if protected {
		if err = ValidateSupplierURL(fullRequestURL); err != nil {
			return MidjourneyErrorWithStatusCodeWrapper(constant.MjErrorUnknown, "upstream_target_unavailable", http.StatusServiceUnavailable), nil, err
		}
		client = GetSupplierHTTPClient()
	}
	if info != nil {
		if err = BeforeTaskChannelUpstream(c, info, req); err != nil {
			return MidjourneyErrorWithStatusCodeWrapper(constant.MjErrorUnknown, "channel_limit_unavailable", http.StatusServiceUnavailable), nil, err
		}
	}
	if info != nil {
		MarkOverviewUpstreamSent(info)
	}
	resp, err := client.Do(req)
	if err != nil {
		if protected {
			err = errors.New("upstream request failed")
		}
		common.SysLog(common.LogText("do request failed: %s", err.Error()))
		return MidjourneyErrorWithStatusCodeWrapper(constant.MjErrorUnknown, "do_request_failed", http.StatusInternalServerError), nullBytes, err
	}
	defer CloseResponseBodyGracefully(resp)
	statusCode := resp.StatusCode
	c.Set("task_submit_http_status", statusCode)
	//if statusCode != 200  {
	//	return MidjourneyErrorWithStatusCodeWrapper(constant.MjErrorUnknown, "bad_response_status_code", statusCode), nullBytes, nil
	//}
	err = req.Body.Close()
	if err != nil {
		return MidjourneyErrorWithStatusCodeWrapper(constant.MjErrorUnknown, "close_request_body_failed", statusCode), nullBytes, err
	}
	err = c.Request.Body.Close()
	if err != nil {
		return MidjourneyErrorWithStatusCodeWrapper(constant.MjErrorUnknown, "close_request_body_failed", statusCode), nullBytes, err
	}
	var midjResponse dto.MidjourneyResponse
	var midjourneyUploadsResponse dto.MidjourneyUploadResponse
	responseBody, err = io.ReadAll(io.LimitReader(resp.Body, 8<<20+1))
	if len(responseBody) > 8<<20 {
		err = errors.New("upstream response exceeds limit")
	}
	if err != nil {
		return MidjourneyErrorWithStatusCodeWrapper(constant.MjErrorUnknown, "read_response_body_failed", statusCode), nullBytes, err
	}
	ObserveChannelFundingJSON(ChannelFundingAttempt(c), statusCode, responseBody)
	// Upstream payloads can echo credentials and are never written to diagnostics.
	if protected && len(responseBody) > 0 {
		responseBody, err = redactMidjourneySupplierPayload(responseBody, auth)
		if err != nil {
			return MidjourneyErrorWithStatusCodeWrapper(constant.MjErrorUnknown, "unmarshal_response_body_failed", statusCode), nil, errors.New("upstream response invalid")
		}
	}
	if len(responseBody) == 0 {
		return MidjourneyErrorWithStatusCodeWrapper(constant.MjErrorUnknown, "empty_response_body", statusCode), responseBody, nil
	} else {
		err = common.Unmarshal(responseBody, &midjResponse)
		if err != nil {
			err2 := common.Unmarshal(responseBody, &midjourneyUploadsResponse)
			if err2 != nil {
				return MidjourneyErrorWithStatusCodeWrapper(constant.MjErrorUnknown, "unmarshal_response_body_failed", statusCode), responseBody, err
			}
		}
	}
	if midjResponse.Code != 1 && midjResponse.Code != 21 && midjResponse.Code != 22 {
		c.Set("task_submit_rejected", true)
		RecordChannelFundingError(ChannelFundingAttempt(c), statusCode, fmt.Sprint(midjResponse.Code), "", midjResponse.Description)
	}
	//for k, v := range resp.Header {
	//	c.Writer.Header().Set(k, v[0])
	//}
	return &dto.MidjourneyResponseWithStatusCode{
		StatusCode: statusCode,
		Response:   midjResponse,
	}, responseBody, nil
}

// Redact JSON strings before they enter task storage, client responses or
// error logs. RawMessage preserves numeric IDs and quantities exactly, while
// decoding strings also catches credentials echoed with JSON escape sequences.
func redactMidjourneySupplierPayload(data []byte, secret string) ([]byte, error) {
	switch common.GetJsonType(data) {
	case "object":
		var values map[string]common.RawMessage
		if err := common.Unmarshal(data, &values); err != nil {
			return nil, err
		}
		clean := make(map[string]common.RawMessage, len(values))
		for key, value := range values {
			redacted, err := redactMidjourneySupplierPayload(value, secret)
			if err != nil {
				return nil, err
			}
			if secret != "" {
				key = strings.ReplaceAll(key, secret, "[redacted]")
			}
			clean[key] = redacted
		}
		return common.Marshal(clean)
	case "array":
		var values []common.RawMessage
		if err := common.Unmarshal(data, &values); err != nil {
			return nil, err
		}
		for i, value := range values {
			redacted, err := redactMidjourneySupplierPayload(value, secret)
			if err != nil {
				return nil, err
			}
			values[i] = redacted
		}
		return common.Marshal(values)
	case "string":
		var value string
		if err := common.Unmarshal(data, &value); err != nil {
			return nil, err
		}
		if secret != "" {
			value = strings.ReplaceAll(value, secret, "[redacted]")
		}
		return common.Marshal(value)
	default:
		var scalar common.RawMessage
		if err := common.Unmarshal(data, &scalar); err != nil {
			return nil, err
		}
		return scalar, nil
	}
}

// AttachMidjourneyChannelAttempt freezes the submit-time procurement identity.
// Legacy task consumers never receive this encrypted connection snapshot.
func AttachMidjourneyChannelAttempt(info *relaycommon.RelayInfo, task *model.Midjourney) error {
	if info == nil || info.TaskChannelSnapshot == nil {
		return nil
	}
	snapshot := info.TaskChannelSnapshot
	if task.Status == "SUCCESS" || task.Status == "FAILURE" {
		result := &relaycommon.TaskInfo{Status: task.Status, Progress: task.Progress}
		if task.ImageUrl != "" && task.Action != constant.MjActionVideo {
			snapshot.UsageUnits = map[string]string{"image_count": "count"}
			result.UsageFacts = map[string]any{"image_count": 1}
		}
		ObserveTaskChannelSnapshot(snapshot, result, true)
	}
	if err := task.SetChannelAttempt(snapshot); err != nil {
		return err
	}
	task.ProcurementVersion, task.ProcurementPending = 1, true
	task.ProcurementReconcile = snapshot.NeedsReconciliation() && task.MjId != ""
	return nil
}

func FlushMidjourneyChannelObservation(ctx context.Context, task *model.Midjourney) error {
	if !task.ProcurementPending {
		return nil
	}
	snapshot, err := task.ChannelAttempt()
	if err != nil || snapshot == nil {
		return err
	}
	if err := SettleTaskChannelSnapshot(ctx, snapshot, task.ProcurementVersion, time.Now()); err != nil {
		return err
	}
	result := model.DB.WithContext(ctx).Model(&model.Midjourney{}).Where("id = ? AND procurement_version = ?", task.Id, task.ProcurementVersion).Update("procurement_pending", false)
	if result.Error == nil && result.RowsAffected > 0 {
		task.ProcurementPending = false
	}
	return result.Error
}

func ResolveMidjourneyTaskChannel(task *model.Midjourney) (*model.Channel, error) {
	snapshot, err := task.ChannelAttempt()
	if err != nil {
		return nil, err
	}
	if snapshot != nil {
		return ResolveTaskChannelSnapshot(snapshot)
	}
	channel, err := model.CacheGetChannel(task.ChannelId)
	if err != nil {
		return nil, err
	}
	var identity hosttypes.ChannelFundingAttempt
	_ = common.UnmarshalJsonStr(task.FundingAttemptData, &identity)
	copy := *channel
	copy.Key = TaskPollingKey(channel, task.PollingKey, &identity)
	copy.Keys, copy.ChannelInfo = nil, model.ChannelInfo{}
	return &copy, nil
}

// RefreshMidjourneyTask accepts terminal evidence only from the frozen
// upstream. Pauses and today's exhausted budget affect new submissions only.
func RefreshMidjourneyTask(ctx context.Context, task *model.Midjourney) error {
	if err := FlushMidjourneyChannelObservation(ctx, task); err != nil {
		return err
	}
	if (task.Status == "SUCCESS" || task.Status == "FAILURE") && task.Progress == "100%" && !task.ProcurementReconcile {
		return nil
	}
	snapshot, err := task.ChannelAttempt()
	if err != nil {
		return err
	}
	channel, err := ResolveMidjourneyTaskChannel(task)
	if err != nil {
		return markMidjourneyResultUnknown(ctx, task, snapshot, err)
	}
	body, err := common.Marshal(map[string]any{"ids": []string{task.MjId}})
	if err != nil {
		return err
	}
	requestURL := strings.TrimRight(channel.GetBaseURL(), "/") + "/mj/task/list-by-condition"
	client := GetHttpClient()
	if channel.OwnerSupplierID != "" || channel.SecretsWriteOnly {
		if err := ValidateSupplierURL(requestURL); err != nil {
			return markMidjourneyResultUnknown(ctx, task, snapshot, err)
		}
		client = GetSupplierHTTPClient()
	}
	queryCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(queryCtx, http.MethodPost, requestURL, strings.NewReader(string(body)))
	if err != nil {
		return markMidjourneyResultUnknown(ctx, task, snapshot, err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("mj-api-secret", channel.Key)
	response, err := client.Do(req)
	if err != nil {
		return markMidjourneyResultUnknown(ctx, task, snapshot, errors.New("upstream task query failed"))
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 8<<20+1))
	if err != nil || len(data) > 8<<20 {
		return markMidjourneyResultUnknown(ctx, task, snapshot, errors.New("upstream task response unavailable"))
	}
	var identity hosttypes.ChannelFundingAttempt
	_ = common.UnmarshalJsonStr(task.FundingAttemptData, &identity)
	funding := FundingTaskAttempt(channel, channel.Key, &model.Task{PrivateData: model.TaskPrivateData{FundingAttempt: &identity}})
	ObserveChannelFundingJSON(funding, response.StatusCode, data)
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return markMidjourneyResultUnknown(ctx, task, snapshot, errors.New("upstream task result unconfirmed"))
	}
	if channel.OwnerSupplierID != "" || channel.SecretsWriteOnly {
		data, err = redactMidjourneySupplierPayload(data, channel.Key)
		if err != nil {
			return markMidjourneyResultUnknown(ctx, task, snapshot, errors.New("upstream task response invalid"))
		}
	}
	var items []dto.MidjourneyDto
	if common.Unmarshal(data, &items) != nil {
		return markMidjourneyResultUnknown(ctx, task, snapshot, errors.New("upstream task response invalid"))
	}
	var result *dto.MidjourneyDto
	for i := range items {
		if items[i].MjId == task.MjId {
			if result != nil {
				return markMidjourneyResultUnknown(ctx, task, snapshot, errors.New("upstream task response ambiguous"))
			}
			result = &items[i]
		}
	}
	if result == nil {
		return markMidjourneyResultUnknown(ctx, task, snapshot, errors.New("upstream task result unconfirmed"))
	}
	switch result.Status {
	case "SUCCESS", "FAILURE", "IN_PROGRESS", "NOT_START", "SUBMITTED", "QUEUED":
	default:
		return markMidjourneyResultUnknown(ctx, task, snapshot, errors.New("upstream task result unrecognized"))
	}
	previous := task.Status
	observed := &relaycommon.TaskInfo{Status: result.Status, Progress: result.Progress}
	if snapshot != nil {
		if result.Status == "SUCCESS" && result.ImageUrl != "" && task.Action != constant.MjActionVideo {
			snapshot.UsageUnits = map[string]string{"image_count": "count"}
			observed.UsageFacts = map[string]any{"image_count": 1}
		}
		ObserveTaskChannelSnapshot(snapshot, observed, true)
		if err := task.SetChannelAttempt(snapshot); err != nil {
			return err
		}
	}
	task.Code, task.Status, task.Progress = 1, result.Status, result.Progress
	task.PromptEn, task.State, task.StartTime, task.FinishTime = result.PromptEn, result.State, result.StartTime, result.FinishTime
	task.ImageUrl, task.VideoUrl, task.FailReason = result.ImageUrl, result.VideoUrl, result.FailReason
	if result.Properties != nil {
		raw, err := common.Marshal(result.Properties)
		if err != nil {
			return err
		}
		task.Properties = string(raw)
	}
	if result.Buttons != nil {
		raw, err := common.Marshal(result.Buttons)
		if err != nil {
			return err
		}
		task.Buttons = string(raw)
	}
	if result.VideoUrls != nil {
		raw, err := common.Marshal(result.VideoUrls)
		if err != nil {
			return err
		}
		task.VideoUrls = string(raw)
	}
	task.ProcurementReconcile = snapshot != nil && snapshot.NeedsReconciliation()
	if result.Status == "SUCCESS" || result.Status == "FAILURE" {
		task.Progress = "100%"
	}
	// A customer timeout does not establish upstream failure or a purchase refund.
	localTimeout := task.Status != "SUCCESS" && task.Status != "FAILURE" && task.SubmitTime > 0 && time.Now().UnixMilli()-task.SubmitTime > int64(time.Hour/time.Millisecond)
	if localTimeout {
		task.Status, task.Progress, task.FailReason = "FAILURE", "100%", "Upstream task timed out (over 1 hour)"
		task.ProcurementReconcile = snapshot != nil
		if snapshot != nil {
			ObserveTaskChannelSnapshot(snapshot, nil, false)
			if err := task.SetChannelAttempt(snapshot); err != nil {
				return err
			}
		}
	}
	won, err := task.UpdateWithStatus(previous)
	if err != nil || !won {
		return err
	}
	if task.Status == "FAILURE" && previous != "FAILURE" {
		RefundMidjourneyQuota(ctx, task, "Composition failed")
	}
	return FlushMidjourneyChannelObservation(ctx, task)
}

func markMidjourneyResultUnknown(ctx context.Context, task *model.Midjourney, snapshot *relaycommon.TaskChannelSnapshot, cause error) error {
	if snapshot == nil {
		return cause
	}
	ObserveTaskChannelSnapshot(snapshot, nil, false)
	if err := task.SetChannelAttempt(snapshot); err != nil {
		return err
	}
	task.ProcurementReconcile = task.MjId != ""
	won, err := task.UpdateWithStatus(task.Status)
	if err != nil {
		return err
	}
	if won {
		if err := FlushMidjourneyChannelObservation(ctx, task); err != nil {
			return err
		}
	}
	return cause
}
