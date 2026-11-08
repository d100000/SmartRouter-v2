package controller

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/service"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type fundingTaskPayload struct {
	IncidentID int64 `json:"incident_id,omitempty"`
}
type channelFundingHandler struct{}

func (channelFundingHandler) Type() string { return model.ChannelFundingTaskType }
func (channelFundingHandler) Enabled() bool {
	return operation_setting.GetChannelFundingSetting().Enabled
}
func (channelFundingHandler) Interval() time.Duration { return time.Minute }
func (channelFundingHandler) NewPayload() any         { return nil }
func (channelFundingHandler) Run(ctx context.Context, task *model.SystemTask, runnerID string) {
	var payload fundingTaskPayload
	err := task.DecodePayload(&payload)
	var count int
	if err == nil {
		count, err = runFundingProbes(ctx, payload.IncidentID)
	}
	status, detail := model.SystemTaskStatusSucceeded, ""
	if err != nil {
		status, detail = model.SystemTaskStatusFailed, err.Error()
	}
	_ = model.FinishSystemTask(task.TaskID, runnerID, status, map[string]any{"tested": count}, detail)
}

func runFundingProbes(ctx context.Context, incidentID int64) (int, error) {
	setting := operation_setting.GetChannelFundingSetting()
	if !setting.Enabled {
		return 0, nil
	}
	query := model.DB.Where("active_key IS NOT NULL AND probe_lease_until <= ?", common.GetTimestamp())
	if incidentID > 0 {
		query = query.Where("id = ?", incidentID)
	} else {
		query = query.Where("next_probe_at > 0 AND next_probe_at <= ?", common.GetTimestamp())
	}
	var rows []model.ChannelFundingIncident
	if err := query.Order("next_probe_at, id").Limit(50).Find(&rows).Error; err != nil {
		return 0, err
	}
	count := 0
	for _, row := range rows {
		if ctx.Err() != nil {
			return count, ctx.Err()
		}
		if !operation_setting.GetChannelFundingSetting().Enabled {
			break
		}
		token := common.GetRandomString(32)
		channel, binding, err := model.ClaimChannelFundingProbe(row.ID, row.Revision, token)
		if errors.Is(err, model.ErrFundingConflict) || errors.Is(err, gorm.ErrRecordNotFound) {
			continue
		}
		if err != nil {
			return count, err
		}
		probeStatus, detail := "failed", "Upstream recovery check failed"
		startedAt := time.Now().UnixMilli()
		probeCtx, cancel := context.WithTimeout(ctx, 45*time.Second)
		keyDisabledManually := channel.ChannelInfo.IsMultiKey && channel.ChannelInfo.MultiKeyStatusList[binding.KeyIndex] == common.ChannelStatusManuallyDisabled
		success := false
		switch {
		case channel.OwnerSupplierID != "" || channel.SecretsWriteOnly:
			probeStatus, detail = "manual_required", "No reliable automatic recovery check is available"
		case incidentID == 0 && (channel.Status == common.ChannelStatusManuallyDisabled || keyDisabledManually):
			probeStatus, detail = "paused", "Channel or key was disabled manually"
		case !channel.ScheduleAllowsAt(time.Now()):
			probeStatus, detail = "paused", "Channel is outside its availability schedule"
		case channel.Type == constant.ChannelTypeDeepSeek && row.Category == "balance_insufficient":
			// Only this provider's documented availability flag proves that the
			// account balance restriction is lifted. Do not infer quota recovery
			// from a positive wallet balance at unrelated providers.
			keys := channel.GetKeys()
			if binding.KeyIndex < len(keys) {
				success = probeDeepSeekFunding(probeCtx, channel, keys[binding.KeyIndex])
				count++
			}
		default:
			endpoint := ""
			switch row.Endpoint {
			case "/v1/chat/completions":
				endpoint = string(constant.EndpointTypeOpenAI)
			case "/v1/messages":
				endpoint = string(constant.EndpointTypeAnthropic)
			case "/v1/responses":
				endpoint = string(constant.EndpointTypeOpenAIResponse)
			default:
				if strings.HasPrefix(row.Endpoint, "/v1beta/models/") && (strings.HasSuffix(row.Endpoint, ":generateContent") || strings.HasSuffix(row.Endpoint, ":streamGenerateContent")) {
					endpoint = string(constant.EndpointTypeGemini)
				}
			}
			// Do not replay arbitrary custom routes or provider task creation.
			if endpoint == "" || row.Model == "" || strings.HasPrefix(row.Source, "task") || channel.Type == constant.ChannelTypeTaskPlugin || channel.Type == constant.ChannelTypeAdvancedCustom || len(channel.GetParamOverride()) > 0 {
				probeStatus, detail = "manual_required", "No reliable automatic recovery check is available"
			} else if userID, resolveErr := resolveChannelTestUserID(nil); resolveErr != nil {
				probeStatus, detail = "blocked", "Channel test user is unavailable"
			} else {
				result := testChannel(probeCtx, channel, userID, row.Model, endpoint, false, binding.KeyIndex)
				count++
				success = result.localErr == nil && result.newAPIError == nil
				if result.newAPIError != nil {
					service.ProcessChannelFundingError(result.context, result.newAPIError)
				}
			}
		}
		cancel()
		if ctx.Err() != nil {
			return count, ctx.Err()
		}
		setting = operation_setting.GetChannelFundingSetting()
		if !setting.Enabled {
			err := model.DB.Model(&model.ChannelFundingIncident{}).Where("id = ? AND probe_token = ? AND active_key IS NOT NULL", row.ID, token).Updates(map[string]any{"probe_lease_until": 0, "probe_token": "", "probe_status": "paused", "probe_detail": "Channel funding monitoring is disabled", "last_probe_at": common.GetTimestamp()}).Error
			return count, err
		}
		if success {
			_, enable := common.ChannelAutomationSwitches()
			err := model.ResolveChannelFundingProbe(row.ID, row.Revision, startedAt, enable, setting.RecoveryNotify, token)
			if err == nil {
				continue
			}
			if !errors.Is(err, model.ErrFundingConflict) {
				return count, err
			}
			probeStatus, detail = "pending", "A newer failure was observed"
		}
		minutes := min(60, max(1, setting.ProbeMinutes)*(1<<min(row.ProbeAttempts+1, 6)))
		next := common.GetTimestamp() + int64(minutes)*60
		if probeStatus == "manual_required" {
			next = 0
		}
		updates := map[string]any{"probe_lease_until": 0, "probe_token": "", "probe_status": probeStatus, "probe_detail": detail, "last_probe_at": common.GetTimestamp(), "probe_attempts": row.ProbeAttempts + 1, "next_probe_at": next}
		if err := model.DB.Model(&model.ChannelFundingIncident{}).Where("id = ? AND probe_token = ? AND active_key IS NOT NULL", row.ID, token).Updates(updates).Error; err != nil {
			return count, err
		}
	}
	return count, nil
}

func probeDeepSeekFunding(ctx context.Context, channel *model.Channel, key string) bool {
	baseURL := channel.GetBaseURL()
	if baseURL == "" {
		baseURL = constant.GetChannelBaseURL(channel.Type)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, strings.TrimRight(baseURL, "/")+"/user/balance", nil)
	if err != nil {
		return false
	}
	headers, err := buildFetchModelsHeaders(channel, key)
	if err != nil {
		return false
	}
	req.Header = headers
	settings := channel.GetSetting()
	client, err := service.GetHttpClientWithProxySettings(settings.Proxy, settings)
	if err != nil {
		return false
	}
	response, err := client.Do(req)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return false
	}
	body, err := io.ReadAll(io.LimitReader(response.Body, 32<<10))
	var balance DeepSeekUsageResponse
	return err == nil && common.Unmarshal(body, &balance) == nil && balance.IsAvailable
}

func GetChannelFunding(c *gin.Context) {
	id, err := strconv.Atoi(c.Param("id"))
	if err != nil || id <= 0 {
		common.ApiErrorT(c, "Invalid channel ID")
		return
	}
	incidents := make([]model.ChannelFundingIncident, 0)
	if err := model.DB.Where("channel_id = ?", id).Order("id desc").Limit(100).Find(&incidents).Error; err != nil {
		common.ApiError(c, err)
		return
	}
	deliveries := make([]model.ChannelFundingDelivery, 0)
	if err := model.DB.Where("channel_id = ?", id).Order("id desc").Limit(200).Find(&deliveries).Error; err != nil {
		common.ApiError(c, err)
		return
	}
	var bindings []model.ChannelCredentialBinding
	if err := model.DB.Where("channel_id = ? AND active = ?", id, true).Find(&bindings).Error; err != nil {
		common.ApiError(c, err)
		return
	}
	byID := make(map[string]model.ChannelCredentialBinding, len(bindings))
	for _, binding := range bindings {
		byID[binding.ID] = binding
	}
	for i := range incidents {
		if binding, ok := byID[incidents[i].BindingID]; ok && incidents[i].ActiveKey != nil && binding.CredentialVersionID == incidents[i].CredentialVersionID && binding.ActivatedAt == incidents[i].BindingActivatedAt {
			incidents[i].KeyIndex = binding.KeyIndex
		}
	}
	common.ApiSuccess(c, gin.H{"incidents": incidents, "deliveries": deliveries, "monitoring_enabled": operation_setting.GetChannelFundingSetting().Enabled})
}

func OperateChannelFunding(c *gin.Context) {
	id, parseErr := strconv.Atoi(c.Param("id"))
	var request struct {
		IncidentID int64  `json:"incident_id"`
		Revision   int64  `json:"revision"`
		Reason     string `json:"reason"`
	}
	if parseErr != nil || id <= 0 || c.ShouldBindJSON(&request) != nil || request.IncidentID <= 0 || request.Revision <= 0 {
		common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Invalid parameters"))
		return
	}
	var incident model.ChannelFundingIncident
	if model.DB.Where("id = ? AND channel_id = ? AND revision = ? AND active_key IS NOT NULL", request.IncidentID, id, request.Revision).First(&incident).Error != nil {
		common.ApiErrorStatus(c, http.StatusConflict, common.NewMessage(model.ErrFundingConflict.Error()))
		return
	}
	if strings.HasSuffix(c.FullPath(), "/probe") {
		channel, err := model.GetChannelById(id, false)
		if err != nil {
			common.ApiError(c, err)
			return
		}
		if channel.OwnerSupplierID != "" || channel.SecretsWriteOnly {
			common.ApiError(c, model.ErrSupplierChannelOperation)
			return
		}
		if !operation_setting.GetChannelFundingSetting().Enabled {
			common.ApiErrorStatus(c, http.StatusConflict, common.NewMessage("Channel funding monitoring is disabled"))
			return
		}
		if incident.ProbeLeaseUntil > common.GetTimestamp() {
			common.ApiErrorStatus(c, http.StatusConflict, common.NewMessage("A funding recovery check is already running"))
			return
		}
		task, created, err := service.EnqueueSystemTask(model.ChannelFundingTaskType, fundingTaskPayload{IncidentID: incident.ID})
		if err != nil {
			common.ApiError(c, err)
			return
		}
		if !created {
			common.ApiErrorStatus(c, http.StatusConflict, common.NewMessage("A funding recovery check is already running"))
			return
		}
		recordManageAudit(c, "channel.funding_probe", map[string]any{"channel_id": id, "event_id": incident.ID})
		c.JSON(http.StatusAccepted, gin.H{"success": true, "data": gin.H{"task_id": task.TaskID}})
		return
	}
	reason := strings.TrimSpace(request.Reason)
	if reason == "" || len([]rune(reason)) > 500 {
		common.ApiErrorStatus(c, http.StatusBadRequest, common.NewMessage("Enter a reason of 1 to 500 characters"))
		return
	}
	reason, _ = model.SafeFailureDetail(reason, 2000)
	if err := model.ResolveChannelFunding(incident.ID, request.Revision, 0, false, false, c.GetInt("id"), reason); err != nil {
		common.ApiErrorStatus(c, http.StatusConflict, common.NewMessage(model.ErrFundingConflict.Error()))
		return
	}
	recordManageAudit(c, "channel.funding_dismiss", map[string]any{"channel_id": id, "event_id": incident.ID})
	common.ApiSuccess(c, nil)
}
