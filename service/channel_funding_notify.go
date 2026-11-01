package service

import (
	"context"
	"errors"
	"fmt"
	"html"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/i18n"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/setting/operation_setting"
	"github.com/QuantumNous/new-api/setting/system_setting"
	"gorm.io/gorm"
)

const ChannelFundingNotifyTaskType = "channel_funding_notify"

type fundingNotifyHandler struct{}

func (fundingNotifyHandler) Type() string { return ChannelFundingNotifyTaskType }
func (fundingNotifyHandler) Enabled() bool {
	return operation_setting.GetChannelFundingSetting().Enabled
}
func (fundingNotifyHandler) Interval() time.Duration { return time.Minute }
func (fundingNotifyHandler) NewPayload() any         { return nil }
func (fundingNotifyHandler) Run(ctx context.Context, task *model.SystemTask, runnerID string) {
	err := RunChannelFundingNotifications(ctx)
	status, detail := model.SystemTaskStatusSucceeded, ""
	if err != nil {
		status, detail = model.SystemTaskStatusFailed, err.Error()
	}
	_ = model.FinishSystemTask(task.TaskID, runnerID, status, nil, detail)
}

func init() { RegisterSystemTaskHandler(fundingNotifyHandler{}) }

// RunChannelFundingNotifications is separate from probes so a slow upstream
// probe never delays an initial notification. A DB task lease serializes runs.
func RunChannelFundingNotifications(ctx context.Context) error {
	setting := operation_setting.GetChannelFundingSetting()
	if !setting.Enabled {
		return nil
	}
	if err := model.QueueFundingMaintenance(max(1, setting.ReminderHours)); err != nil {
		return err
	}
	now := common.GetTimestamp()
	if err := model.DB.Model(&model.ChannelFundingDelivery{}).Where("status = ? AND attempts >= ? AND next_attempt_at <= ?", "sending", 5, now).Updates(map[string]any{"status": "failed", "last_error": "Delivery result unavailable after worker lease expired"}).Error; err != nil {
		return err
	}
	var deliveries []model.ChannelFundingDelivery
	if err := model.DB.Where("status NOT IN ? AND attempts < ? AND next_attempt_at <= ?", []string{"sent", "cancelled"}, 5, now).Order("id").Limit(200).Find(&deliveries).Error; err != nil {
		return err
	}
	groups := make(map[string][]model.ChannelFundingDelivery)
	order := []string{}
	for _, delivery := range deliveries {
		key := fmt.Sprintf("%d:%d:%s", delivery.UserID, delivery.ChannelID, delivery.Kind)
		if _, exists := groups[key]; !exists {
			order = append(order, key)
		}
		groups[key] = append(groups[key], delivery)
	}
	for _, key := range order {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !operation_setting.GetChannelFundingSetting().Enabled {
			return nil
		}
		group := groups[key]
		claimed, err := model.ClaimFundingDeliveries(group[0].ChannelID, group[0].UserID, group[0].Kind, common.GetTimestamp())
		if err != nil {
			return err
		}
		if len(claimed) == 0 {
			continue
		}
		// Re-read subscriptions and event state after claiming, immediately
		// before delivery. Cancellation only touches this attempt generation.
		var user model.User
		err = model.DB.Select("id", "email", "role", "status", "setting").First(&user, group[0].UserID).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		userSetting := user.GetSetting()
		user.Email = strings.TrimSpace(user.Email)
		userSetting.NotificationEmail = strings.TrimSpace(userSetting.NotificationEmail)
		subscribed := err == nil && user.Role >= common.RoleAdminUser && user.Status == common.UserStatusEnabled && (userSetting.ChannelNotifyEnabled != nil && *userSetting.ChannelNotifyEnabled || userSetting.ChannelNotifyEnabled == nil && user.Role == common.RoleRootUser)
		channel, err := model.GetChannelById(group[0].ChannelID, false)
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if err != nil {
			channel = nil
		}
		activeClaims := make([]model.ChannelFundingDelivery, 0, len(claimed))
		rows := make([]model.ChannelFundingIncident, 0, len(claimed))
		for _, delivery := range claimed {
			var row model.ChannelFundingIncident
			err := model.DB.First(&row, delivery.IncidentID).Error
			if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
			valid := err == nil && row.ActiveKey != nil && row.State == "confirmed"
			if delivery.Kind == "recovery" {
				valid = err == nil && row.State == "resolved" && operation_setting.GetChannelFundingSetting().RecoveryNotify
			}
			if !valid || !subscribed || channel == nil {
				if err := model.FinishFundingDelivery(delivery.ID, delivery.Attempts, "cancelled", "", 0, false); err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
					return err
				}
				continue
			}
			var owned int64
			if err := model.DB.Model(&model.ChannelFundingDelivery{}).Where("id = ? AND attempts = ? AND status = ? AND next_attempt_at > ?", delivery.ID, delivery.Attempts, "sending", common.GetTimestamp()).Count(&owned).Error; err != nil {
				return err
			}
			if owned == 0 {
				continue
			}
			activeClaims = append(activeClaims, delivery)
			rows = append(rows, row)
		}
		if len(activeClaims) == 0 {
			continue
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if !operation_setting.GetChannelFundingSetting().Enabled {
			return nil
		}
		deliveryStatus, detail := "sent", ""
		if !fundingNotificationDestination(user.Email, userSetting) {
			deliveryStatus, detail = "missing_address", "Notification destination is not configured"
		} else {
			title, content := renderFundingNotification(channel, rows, group[0].Kind, userSetting.Language)
			err := NotifyUser(user.Id, user.Email, userSetting, dto.NewNotify(fmt.Sprintf("channel_funding_%d_%s", channel.Id, group[0].Kind), title, content, nil))
			if err != nil {
				deliveryStatus, detail = "failed", "Notification delivery failed"
				if errors.Is(err, ErrNotificationLimited) {
					deliveryStatus, detail = "limited", "Notification rate limit reached"
				}
			}
		}
		for _, delivery := range activeClaims {
			backoff := []int64{60, 300, 900, 3600, 3600}
			current := operation_setting.GetChannelFundingSetting()
			if err := model.FinishFundingDelivery(delivery.ID, delivery.Attempts, deliveryStatus, detail, common.GetTimestamp()+backoff[min(max(delivery.Attempts-1, 0), 4)], current.Enabled && current.RecoveryNotify); err != nil {
				return err
			}
		}
	}
	return nil
}

func fundingNotificationDestination(email string, setting dto.UserSetting) bool {
	switch setting.NotifyType {
	case "", dto.NotifyTypeEmail:
		return strings.TrimSpace(setting.NotificationEmail) != "" || strings.TrimSpace(email) != ""
	case dto.NotifyTypeWebhook:
		return setting.WebhookUrl != ""
	case dto.NotifyTypeBark:
		return setting.BarkUrl != ""
	case dto.NotifyTypeGotify:
		return setting.GotifyUrl != "" && setting.GotifyToken != ""
	}
	return false
}

func renderFundingNotification(channel *model.Channel, rows []model.ChannelFundingIncident, kind, lang string) (string, string) {
	params := map[string]any{"Name": html.EscapeString(channel.Name), "Id": channel.Id, "Count": len(rows)}
	subjectKey := "funding.notice_subject"
	if kind == "recovery" {
		subjectKey = "funding.recovery_subject"
	}
	subject := i18n.Translate(lang, subjectKey, params)
	var content strings.Builder
	currentBindings := make(map[string]model.UpstreamCredentialSnapshot)
	for index := range max(1, channel.ChannelInfo.MultiKeySize) {
		snapshot := channel.GetUpstreamCredentialSnapshot(index)
		if snapshot.BindingID != "" {
			currentBindings[snapshot.BindingID] = snapshot
		}
	}
	for _, row := range rows {
		alias := row.KeyAlias
		if alias == "" {
			index := row.KeyIndex
			if snapshot, ok := currentBindings[row.BindingID]; ok && snapshot.CredentialVersionID == row.CredentialVersionID && snapshot.BindingActivatedAt == row.BindingActivatedAt {
				index = snapshot.KeyIndex
			}
			alias = fmt.Sprintf("#%d", index+1)
		}
		category := i18n.Translate(lang, "funding."+row.Category)
		params := map[string]any{"Event": row.ID, "Key": html.EscapeString(alias), "Reason": html.EscapeString(category), "Evidence": html.EscapeString(row.Evidence), "Time": time.Unix(row.FirstSeen, 0).UTC().Format(time.RFC3339), "Disabled": i18n.Translate(lang, "funding.disabled_no"), "Probe": i18n.Translate(lang, "funding.probe_"+row.ProbeStatus)}
		if row.AutoDisabled {
			params["Disabled"] = i18n.Translate(lang, "funding.disabled_yes")
		}
		content.WriteString(i18n.Translate(lang, "funding.notice_entry", params))
		content.WriteString("<br/>")
	}
	link := strings.TrimRight(system_setting.ServerAddress, "/") + "/channels?filter=" + fmt.Sprint(channel.Id)
	content.WriteString(i18n.Translate(lang, "funding.manage_link", map[string]any{"Link": html.EscapeString(link)}))
	return subject, content.String()
}
