package model

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	hosttypes "github.com/QuantumNous/new-api/types"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const ChannelFundingTaskType = "channel_funding"

var ErrFundingConflict = errors.New("Funding event changed. Refresh and try again")

type ChannelFundingIncident struct {
	NotificationQueued  bool    `json:"-" gorm:"-"`
	BindingActivatedAt  int64   `json:"-" gorm:"bigint"`
	ResolvedAfter       int64   `json:"-" gorm:"bigint"`
	ID                  int64   `json:"id" gorm:"primaryKey"`
	ChannelID           int     `json:"channel_id" gorm:"index"`
	BindingID           string  `json:"binding_id" gorm:"type:varchar(36);index"`
	CredentialVersionID string  `json:"credential_version_id" gorm:"type:varchar(36)"`
	ActiveKey           *string `json:"-" gorm:"type:varchar(80);uniqueIndex"`
	Revision            int64   `json:"revision"`
	State               string  `json:"state" gorm:"type:varchar(20);index"`
	Category            string  `json:"category" gorm:"type:varchar(32)"`
	Rule                string  `json:"rule" gorm:"type:varchar(100)"`
	KeyIndex            int     `json:"key_index"`
	KeyAlias            string  `json:"key_alias" gorm:"type:varchar(100)"`
	Model               string  `json:"model" gorm:"type:varchar(255)"`
	Endpoint            string  `json:"endpoint" gorm:"type:varchar(255)"`
	Source              string  `json:"source" gorm:"type:varchar(32)"`
	RequestID           string  `json:"request_id" gorm:"type:varchar(128)"`
	StatusCode          int     `json:"status_code"`
	ErrorCode           string  `json:"error_code" gorm:"type:varchar(128)"`
	Evidence            string  `json:"evidence" gorm:"type:text"`
	FirstSeen           int64   `json:"first_seen" gorm:"bigint"`
	LastSeen            int64   `json:"last_seen" gorm:"bigint"`
	LastFailureAt       int64   `json:"-" gorm:"bigint"`
	Hits                int64   `json:"hits" gorm:"bigint"`
	AutoDisabled        bool    `json:"auto_disabled"`
	DisableReason       string  `json:"-" gorm:"type:varchar(128)"`
	NextProbeAt         int64   `json:"next_probe_at" gorm:"bigint;index"`
	ProbeAttempts       int     `json:"probe_attempts"`
	ProbeStatus         string  `json:"probe_status" gorm:"type:varchar(32)"`
	ProbeDetail         string  `json:"probe_detail" gorm:"type:text"`
	LastProbeAt         int64   `json:"last_probe_at" gorm:"bigint"`
	ProbeLeaseUntil     int64   `json:"-" gorm:"bigint"`
	ProbeToken          string  `json:"-" gorm:"type:varchar(64)"`
	ResolvedAt          int64   `json:"resolved_at" gorm:"bigint;index"`
	ResolvedBy          int     `json:"resolved_by"`
	Resolution          string  `json:"resolution" gorm:"type:text"`
}

type ChannelFundingDelivery struct {
	ID            int64  `json:"id" gorm:"primaryKey"`
	IncidentID    int64  `json:"incident_id" gorm:"index"`
	ChannelID     int    `json:"channel_id" gorm:"index"`
	UserID        int    `json:"user_id"`
	DedupKey      string `json:"-" gorm:"type:varchar(128);uniqueIndex"`
	Kind          string `json:"kind" gorm:"type:varchar(20)"`
	Status        string `json:"status" gorm:"type:varchar(24);index"`
	Attempts      int    `json:"attempts"`
	NextAttemptAt int64  `json:"next_attempt_at" gorm:"bigint;index"`
	CreatedAt     int64  `json:"created_at" gorm:"bigint"`
	SentAt        int64  `json:"sent_at" gorm:"bigint"`
	LastError     string `json:"last_error" gorm:"type:text"`
}

type ChannelFundingSummary struct {
	Status       string `json:"status"`
	AffectedKeys int    `json:"affected_keys"`
	TotalKeys    int    `json:"total_keys"`
	LastSeen     int64  `json:"last_seen"`
}

// RecordChannelFundingFailure serializes identity validation, incident creation,
// disabling and outbox insertion on the channel row across gateway instances.
func RecordChannelFundingFailure(attempt hosttypes.ChannelFundingAttempt, category, rule, code, evidence string, status int, disable bool, probeMinutes int) (*ChannelFundingIncident, error) {
	lock := GetChannelPollingLock(attempt.ChannelID)
	lock.Lock()
	defer lock.Unlock()
	var incident ChannelFundingIncident
	var channel Channel
	err := DB.Transaction(func(tx *gorm.DB) error {
		if common.UsingMainDatabase(common.DatabaseTypeSQLite) {
			// Acquire SQLite's write reservation before the first read. A
			// deferred read transaction cannot safely upgrade across processes.
			if err := tx.Model(&Channel{}).Where("id = ?", attempt.ChannelID).UpdateColumn("id", gorm.Expr("id")).Error; err != nil {
				return err
			}
		}
		if err := lockForUpdate(tx).First(&channel, attempt.ChannelID).Error; err != nil {
			return err
		}
		var binding ChannelCredentialBinding
		if err := tx.Where("id = ? AND channel_id = ? AND credential_version_id = ? AND active = ?", attempt.BindingID, attempt.ChannelID, attempt.VersionID, true).First(&binding).Error; err != nil {
			return err
		}
		if binding.ActivatedAt != attempt.BindingActivatedAt {
			return ErrFundingConflict
		}
		active := attempt.BindingID + ":" + attempt.VersionID
		err := tx.Where("active_key = ?", active).First(&incident).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		now := time.Now()
		newIncident := incident.ID == 0
		wasConfirmed := incident.State == "confirmed"
		if newIncident {
			var previous ChannelFundingIncident
			if err := tx.Where("binding_id = ? AND credential_version_id = ? AND active_key IS NULL", attempt.BindingID, attempt.VersionID).Order("id desc").Limit(1).Find(&previous).Error; err != nil {
				return err
			}
			// Ignore in-flight requests from before a recovery or manual dismissal.
			if previous.ID != 0 && attempt.StartedAt <= previous.ResolvedAfter {
				return ErrFundingConflict
			}
			incident = ChannelFundingIncident{ChannelID: channel.Id, BindingID: binding.ID, BindingActivatedAt: binding.ActivatedAt, CredentialVersionID: binding.CredentialVersionID, ActiveKey: &active, FirstSeen: now.Unix(), NextProbeAt: now.Add(time.Duration(probeMinutes) * time.Minute).Unix(), ProbeStatus: "pending"}
		}
		incident.Revision++
		incident.Hits++
		incident.LastSeen, incident.LastFailureAt = now.Unix(), now.UnixMilli()
		if !wasConfirmed || category != "suspected" {
			incident.Category, incident.Rule, incident.ErrorCode, incident.Evidence = category, rule, code, evidence
			incident.State = "confirmed"
			if category == "suspected" {
				incident.State = "suspected"
			}
			incident.Model, incident.Endpoint, incident.Source, incident.RequestID, incident.StatusCode = attempt.Model, attempt.Endpoint, attempt.Source, attempt.RequestID, status
		}
		incident.KeyIndex, incident.KeyAlias = binding.KeyIndex, attempt.KeyAlias
		if err := tx.Save(&incident).Error; err != nil {
			return err
		}
		if disable && channel.GetAutoBan() && incident.State == "confirmed" && channel.Status != common.ChannelStatusManuallyDisabled {
			keyStatus := channel.Status
			if channel.ChannelInfo.IsMultiKey {
				keyStatus = channel.ChannelInfo.MultiKeyStatusList[binding.KeyIndex]
				if keyStatus == 0 {
					keyStatus = common.ChannelStatusEnabled
				}
			}
			if keyStatus == common.ChannelStatusEnabled {
				keys := channelCredentialKeys(&channel)
				if binding.KeyIndex >= len(keys) {
					return ErrFundingConflict
				}
				incident.DisableReason = fmt.Sprintf("Upstream funding restriction (event #%d)", incident.ID)
				if channel.ChannelInfo.IsMultiKey {
					updateMultiKeyStatusAtIndex(&channel, binding.KeyIndex, common.ChannelStatusAutoDisabled, incident.DisableReason)
				} else {
					channel.Status = common.ChannelStatusAutoDisabled
					info := channel.GetOtherInfo()
					info["status_reason"], info["status_time"] = incident.DisableReason, now.Unix()
					channel.SetOtherInfo(info)
				}
				if err := channel.saveStatusState(tx); err != nil {
					return err
				}
				if err := tx.Model(&Ability{}).Where("channel_id = ?", channel.Id).Update("enabled", channel.Status == common.ChannelStatusEnabled).Error; err != nil {
					return err
				}
				incident.AutoDisabled = true
				if err := tx.Save(&incident).Error; err != nil {
					return err
				}
			}
		}
		if incident.State == "confirmed" && !wasConfirmed {
			previous := tx.Model(&ChannelFundingIncident{}).Select("id").Where("binding_id = ? AND credential_version_id = ? AND id < ?", incident.BindingID, incident.CredentialVersionID, incident.ID)
			if err := cancelFundingDeliveries(tx.Model(&ChannelFundingDelivery{}).Where("incident_id IN (?) AND kind = ?", previous, "recovery")); err != nil {
				return err
			}
			incident.NotificationQueued = true
			return enqueueFundingDeliveries(tx, &incident, "initial", 0)
		}
		return nil
	})
	if err == nil {
		invalidateRoutingMetadata()
		CacheUpdateChannel(&channel)
		CacheUpdateChannelStatus(channel.Id, channel.Status)
	}
	return &incident, err
}

func enqueueFundingDeliveries(tx *gorm.DB, incident *ChannelFundingIncident, kind string, slot int64) error {
	var users []User
	if err := tx.Select("id", "role", "setting").Where("role >= ? AND status = ?", common.RoleAdminUser, common.UserStatusEnabled).Find(&users).Error; err != nil {
		return err
	}
	for _, user := range users {
		setting := user.GetSetting()
		if setting.ChannelNotifyEnabled != nil && !*setting.ChannelNotifyEnabled || setting.ChannelNotifyEnabled == nil && user.Role != common.RoleRootUser {
			continue
		}
		if kind == "recovery" {
			var count int64
			if err := tx.Model(&ChannelFundingDelivery{}).Where("incident_id = ? AND user_id = ? AND kind <> ? AND status = ?", incident.ID, user.Id, "recovery", "sent").Count(&count).Error; err != nil {
				return err
			}
			if count == 0 {
				continue
			}
		}
		if kind == "reminder" {
			var count int64
			if err := tx.Model(&ChannelFundingDelivery{}).Where("incident_id = ? AND user_id = ? AND kind <> ? AND status NOT IN ? AND attempts < ?", incident.ID, user.Id, "recovery", []string{"sent", "cancelled"}, 5).Count(&count).Error; err != nil {
				return err
			}
			if count > 0 {
				continue
			}
		}
		now := common.GetTimestamp()
		row := ChannelFundingDelivery{IncidentID: incident.ID, ChannelID: incident.ChannelID, UserID: user.Id, DedupKey: fmt.Sprintf("%d:%d:%s:%d", incident.ID, user.Id, kind, slot), Kind: kind, Status: "pending", CreatedAt: now, NextAttemptAt: now}
		if err := tx.Clauses(clause.OnConflict{DoNothing: true}).Create(&row).Error; err != nil {
			return err
		}
	}
	return nil
}

// ResolveChannelFunding only clears the exact generation that was observed.
// A successful request that started before the last failure cannot recover it.
func ResolveChannelFunding(id, revision, startedAt int64, enable, notify bool, userID int, reason string) error {
	return resolveChannelFunding(id, revision, startedAt, enable, notify, userID, reason, "")
}

func ResolveChannelFundingProbe(id, revision, startedAt int64, enable, notify bool, token string) error {
	if token == "" {
		return ErrFundingConflict
	}
	return resolveChannelFunding(id, revision, startedAt, enable, notify, 0, "Recovery probe succeeded", token)
}

// ClaimChannelFundingProbe freezes the binding and current administrative
// status in the same transaction as the probe lease.
func ClaimChannelFundingProbe(id, revision int64, token string) (*Channel, *ChannelCredentialBinding, error) {
	var initial ChannelFundingIncident
	if err := DB.First(&initial, id).Error; err != nil {
		return nil, nil, err
	}
	var channel Channel
	var binding ChannelCredentialBinding
	err := DB.Transaction(func(tx *gorm.DB) error {
		if common.UsingMainDatabase(common.DatabaseTypeSQLite) {
			if err := tx.Model(&Channel{}).Where("id = ?", initial.ChannelID).UpdateColumn("id", gorm.Expr("id")).Error; err != nil {
				return err
			}
		}
		if err := lockForUpdate(tx).First(&channel, initial.ChannelID).Error; err != nil {
			return err
		}
		var row ChannelFundingIncident
		if err := lockForUpdate(tx).First(&row, id).Error; err != nil {
			return err
		}
		now := common.GetTimestamp()
		if row.ActiveKey == nil || row.Revision != revision || row.ProbeLeaseUntil > now || token == "" {
			return ErrFundingConflict
		}
		if err := tx.Where("id = ? AND channel_id = ? AND credential_version_id = ? AND activated_at = ? AND active = ?", row.BindingID, row.ChannelID, row.CredentialVersionID, row.BindingActivatedAt, true).First(&binding).Error; err != nil {
			return ErrFundingConflict
		}
		snapshot := channel.GetUpstreamCredentialSnapshot(binding.KeyIndex)
		if snapshot.BindingID != row.BindingID || snapshot.CredentialVersionID != row.CredentialVersionID || snapshot.BindingActivatedAt != row.BindingActivatedAt {
			return ErrFundingConflict
		}
		return tx.Model(&row).Updates(map[string]any{"probe_lease_until": now + 90, "probe_token": token, "probe_status": "running"}).Error
	})
	return &channel, &binding, err
}

func resolveChannelFunding(id, revision, startedAt int64, enable, notify bool, userID int, reason, probeToken string) error {
	var initial ChannelFundingIncident
	if err := DB.First(&initial, id).Error; err != nil {
		return err
	}
	lock := GetChannelPollingLock(initial.ChannelID)
	lock.Lock()
	defer lock.Unlock()
	var channel Channel
	err := DB.Transaction(func(tx *gorm.DB) error {
		if common.UsingMainDatabase(common.DatabaseTypeSQLite) {
			if err := tx.Model(&Channel{}).Where("id = ?", initial.ChannelID).UpdateColumn("id", gorm.Expr("id")).Error; err != nil {
				return err
			}
		}
		if err := lockForUpdate(tx).First(&channel, initial.ChannelID).Error; err != nil {
			return err
		}
		var row ChannelFundingIncident
		if err := lockForUpdate(tx).First(&row, id).Error; err != nil {
			return err
		}
		if row.Revision != revision || row.ActiveKey == nil || userID == 0 && startedAt <= row.LastFailureAt {
			return ErrFundingConflict
		}
		if probeToken != "" && (row.ProbeToken != probeToken || row.ProbeLeaseUntil <= common.GetTimestamp()) {
			return ErrFundingConflict
		}
		var binding ChannelCredentialBinding
		if err := tx.Where("id = ? AND credential_version_id = ? AND active = ?", row.BindingID, row.CredentialVersionID, true).First(&binding).Error; err != nil {
			return ErrFundingConflict
		}
		if binding.ActivatedAt != row.BindingActivatedAt {
			return ErrFundingConflict
		}
		if userID == 0 && enable && row.AutoDisabled && channel.Status != common.ChannelStatusManuallyDisabled {
			owned := channel.Status == common.ChannelStatusAutoDisabled && channel.GetOtherInfo()["status_reason"] == row.DisableReason
			if channel.ChannelInfo.IsMultiKey {
				owned = (channel.Status == common.ChannelStatusEnabled || channel.GetOtherInfo()["status_reason"] == ChannelStatusReasonAllKeysDisabled) && channel.ChannelInfo.MultiKeyStatusList[binding.KeyIndex] == common.ChannelStatusAutoDisabled && channel.ChannelInfo.MultiKeyDisabledReason[binding.KeyIndex] == row.DisableReason
			}
			if owned {
				keys := channelCredentialKeys(&channel)
				if binding.KeyIndex >= len(keys) {
					return ErrFundingConflict
				}
				if channel.ChannelInfo.IsMultiKey {
					updateMultiKeyStatusAtIndex(&channel, binding.KeyIndex, common.ChannelStatusEnabled, "")
				} else {
					channel.Status = common.ChannelStatusEnabled
					info := channel.GetOtherInfo()
					info["status_reason"], info["status_time"] = "", common.GetTimestamp()
					channel.SetOtherInfo(info)
				}
				if err := channel.saveStatusState(tx); err != nil {
					return err
				}
				if err := tx.Model(&Ability{}).Where("channel_id = ?", channel.Id).Update("enabled", channel.Status == common.ChannelStatusEnabled).Error; err != nil {
					return err
				}
			}
		}
		row.ActiveKey, row.State, row.ResolvedAt, row.ResolvedBy, row.Resolution = nil, "resolved", common.GetTimestamp(), userID, reason
		row.ResolvedAfter = time.Now().UnixMilli()
		row.Revision++
		row.ProbeLeaseUntil = 0
		row.ProbeStatus = "recovered"
		row.NextProbeAt, row.LastProbeAt, row.ProbeToken = 0, common.GetTimestamp(), ""
		if userID != 0 {
			row.State, row.ProbeStatus = "dismissed", "cancelled"
		}
		if err := tx.Save(&row).Error; err != nil {
			return err
		}
		if err := cancelFundingDeliveries(tx.Model(&ChannelFundingDelivery{}).Where("incident_id = ?", id)); err != nil {
			return err
		}
		if userID == 0 && notify {
			return enqueueFundingDeliveries(tx, &row, "recovery", 0)
		}
		return nil
	})
	if err == nil {
		invalidateRoutingMetadata()
		CacheUpdateChannel(&channel)
		CacheUpdateChannelStatus(channel.Id, channel.Status)
	}
	return err
}

func fundingOwnedDisable(tx *gorm.DB, channel *Channel, usingKey string) (bool, error) {
	owned := strings.HasPrefix(fmt.Sprint(channel.GetOtherInfo()["status_reason"]), "Upstream funding restriction (event #")
	for _, reason := range channel.ChannelInfo.MultiKeyDisabledReason {
		owned = owned || strings.HasPrefix(reason, "Upstream funding restriction (event #")
	}
	if !owned {
		return false, nil
	}
	var incidents []ChannelFundingIncident
	if err := tx.Where("channel_id = ? AND active_key IS NOT NULL AND auto_disabled = ?", channel.Id, true).Find(&incidents).Error; err != nil {
		return false, err
	}
	var bindings []ChannelCredentialBinding
	if err := tx.Where("channel_id = ? AND active = ?", channel.Id, true).Find(&bindings).Error; err != nil {
		return false, err
	}
	keys := channelCredentialKeys(channel)
	for _, row := range incidents {
		for _, binding := range bindings {
			if binding.KeyIndex < 0 || binding.KeyIndex >= len(keys) || usingKey != "" && keys[binding.KeyIndex] != usingKey {
				continue
			}
			if binding.ID == row.BindingID && binding.CredentialVersionID == row.CredentialVersionID && binding.ActivatedAt == row.BindingActivatedAt {
				return true, nil
			}
		}
	}
	return false, nil
}

// A send can finish after recovery cancelled an in-flight delivery. Acknowledge
// actual delivery and then queue recovery, preserving the recipient's ordering.
func FinishFundingDelivery(id int64, attempt int, status, detail string, nextAt int64, recoveryNotify bool) error {
	var initial ChannelFundingDelivery
	if err := DB.First(&initial, id).Error; err != nil {
		return err
	}
	return DB.Transaction(func(tx *gorm.DB) error {
		if common.UsingMainDatabase(common.DatabaseTypeSQLite) {
			if err := tx.Model(&ChannelFundingIncident{}).Where("id = ?", initial.IncidentID).UpdateColumn("id", gorm.Expr("id")).Error; err != nil {
				return err
			}
		}
		var incident ChannelFundingIncident
		if err := lockForUpdate(tx).First(&incident, initial.IncidentID).Error; err != nil {
			return err
		}
		updates := map[string]any{"status": status, "last_error": detail, "next_attempt_at": nextAt}
		query := tx.Model(&ChannelFundingDelivery{}).Where("id = ? AND attempts = ? AND status <> ?", id, attempt, "sent")
		if status == "sent" {
			updates["sent_at"] = common.GetTimestamp()
		} else {
			query = query.Where("status = ?", "sending")
		}
		result := query.Updates(updates)
		if result.Error != nil {
			return result.Error
		}
		if status != "sent" && result.RowsAffected == 0 {
			// A cancelled send has now returned without acceptance. Release its
			// ordering lease without resurrecting the cancelled notification.
			return tx.Model(&ChannelFundingDelivery{}).Where("id = ? AND attempts = ? AND status = ?", id, attempt, "cancelled").Update("next_attempt_at", 0).Error
		}
		if result.RowsAffected > 0 && status == "sent" && initial.Kind != "recovery" && incident.State == "resolved" && recoveryNotify {
			return enqueueFundingDeliveries(tx, &incident, "recovery", 0)
		}
		return nil
	})
}

// Keep an in-flight send's lease until its acknowledgement or expiry. Removing
// it immediately could send a recovery before the original fault email.
func cancelFundingDeliveries(query *gorm.DB) error {
	if err := query.Session(&gorm.Session{}).Where("status NOT IN ?", []string{"sent", "cancelled", "sending"}).Updates(map[string]any{"status": "cancelled", "next_attempt_at": 0}).Error; err != nil {
		return err
	}
	return query.Session(&gorm.Session{}).Where("status = ?", "sending").Update("status", "cancelled").Error
}

// ClaimFundingDeliveries claims the whole recipient/channel group atomically.
// Parallel runners cannot split one batch into several emails, and a stale
// worker can acknowledge only the attempt number it actually claimed.
func ClaimFundingDeliveries(channelID, userID int, kind string, now int64) ([]ChannelFundingDelivery, error) {
	var rows []ChannelFundingDelivery
	err := DB.Transaction(func(tx *gorm.DB) error {
		if common.UsingMainDatabase(common.DatabaseTypeSQLite) {
			if err := tx.Model(&Channel{}).Where("id = ?", channelID).UpdateColumn("id", gorm.Expr("id")).Error; err != nil {
				return err
			}
		}
		var channel Channel
		if err := lockForUpdate(tx).Select("id").First(&channel, channelID).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return cancelFundingDeliveries(tx.Model(&ChannelFundingDelivery{}).Where("channel_id = ?", channelID))
			}
			return err
		}
		var sending int64
		if err := tx.Model(&ChannelFundingDelivery{}).Where("channel_id = ? AND user_id = ? AND status IN ? AND next_attempt_at > ?", channelID, userID, []string{"sending", "cancelled"}, now).Count(&sending).Error; err != nil || sending > 0 {
			return err
		}
		if err := tx.Where("channel_id = ? AND user_id = ? AND kind = ? AND status NOT IN ? AND attempts < ? AND next_attempt_at <= ?", channelID, userID, kind, []string{"sent", "cancelled"}, 5, now).Order("id").Limit(200).Find(&rows).Error; err != nil {
			return err
		}
		for i := range rows {
			row := &rows[i]
			result := tx.Model(&ChannelFundingDelivery{}).Where("id = ? AND attempts = ? AND status NOT IN ?", row.ID, row.Attempts, []string{"sent", "cancelled"}).Updates(map[string]any{"status": "sending", "attempts": row.Attempts + 1, "next_attempt_at": now + 300})
			if result.Error != nil {
				return result.Error
			}
			if result.RowsAffected != 1 {
				return ErrFundingConflict
			}
			row.Attempts++
			row.Status, row.NextAttemptAt = "sending", now+300
		}
		return nil
	})
	return rows, err
}

func ApplyChannelFundingFilter(query *gorm.DB, value string) *gorm.DB {
	if value == "" || value == "all" {
		return query
	}
	sub := DB.Model(&ChannelFundingIncident{}).Select("channel_id").Where("active_key IS NOT NULL")
	switch value {
	case "balance_insufficient", "quota_limited":
		sub = sub.Where("state = ? AND category = ?", "confirmed", value)
	case "suspected":
		sub = sub.Where("state = ?", value)
	case "none":
		return query.Where("id NOT IN (?)", sub)
	default:
		return query.Where("1 = 0")
	}
	return query.Where("id IN (?)", sub)
}

func AttachChannelFunding(channels []*Channel) error {
	if len(channels) == 0 {
		return nil
	}
	ids := make([]int, 0, len(channels))
	for _, channel := range channels {
		ids = append(ids, channel.Id)
	}
	var rows []ChannelFundingIncident
	if err := DB.Where("channel_id IN ? AND active_key IS NOT NULL", ids).Find(&rows).Error; err != nil {
		return err
	}
	byID := make(map[int]*ChannelFundingSummary)
	for _, row := range rows {
		summary := byID[row.ChannelID]
		if summary == nil {
			summary = &ChannelFundingSummary{Status: row.Category}
			byID[row.ChannelID] = summary
		}
		summary.AffectedKeys++
		summary.LastSeen = max(summary.LastSeen, row.LastSeen)
		if row.Category == "balance_insufficient" || summary.Status == "suspected" {
			summary.Status = row.Category
		}
	}
	for _, channel := range channels {
		channel.Funding = byID[channel.Id]
		if channel.Funding != nil {
			channel.Funding.TotalKeys = 1
			if channel.ChannelInfo.IsMultiKey {
				channel.Funding.TotalKeys = max(1, channel.ChannelInfo.MultiKeySize)
			}
		}
	}
	return nil
}

// ChannelFundingTags aggregates the same filtered channels returned in tag
// mode, so pagination, key totals and badges share the server's result scope.
func ChannelFundingTags(channels []*Channel) map[string]ChannelFundingSummary {
	result := make(map[string]ChannelFundingSummary)
	for _, channel := range channels {
		tag := channel.GetTag()
		summary := result[tag]
		keys := 1
		if channel.ChannelInfo.IsMultiKey {
			keys = max(1, channel.ChannelInfo.MultiKeySize)
		}
		summary.TotalKeys += keys
		if funding := channel.Funding; funding != nil {
			summary.AffectedKeys += funding.AffectedKeys
			summary.LastSeen = max(summary.LastSeen, funding.LastSeen)
			if summary.Status == "" || summary.Status == "suspected" || funding.Status == "balance_insufficient" {
				summary.Status = funding.Status
			}
		}
		result[tag] = summary
	}
	return result
}

func QueueFundingMaintenance(reminderHours int) error {
	now := common.GetTimestamp()
	// Detached bindings are historical events, never recovery evidence.
	activeBindings := DB.Model(&ChannelCredentialBinding{}).Select("1").Joins("JOIN channels ON channels.id = channel_credential_bindings.channel_id").Where("channel_credential_bindings.id = channel_funding_incidents.binding_id AND channel_credential_bindings.credential_version_id = channel_funding_incidents.credential_version_id AND channel_credential_bindings.activated_at = channel_funding_incidents.binding_activated_at AND channel_credential_bindings.active = ?", true)
	var detached []int
	if err := DB.Model(&ChannelFundingIncident{}).Where("active_key IS NOT NULL AND NOT EXISTS (?)", activeBindings).Distinct("channel_id").Pluck("channel_id", &detached).Error; err != nil {
		return err
	}
	for _, channelID := range detached {
		if err := DB.Transaction(func(tx *gorm.DB) error {
			if common.UsingMainDatabase(common.DatabaseTypeSQLite) {
				if err := tx.Model(&ChannelFundingIncident{}).Where("channel_id = ?", channelID).UpdateColumn("id", gorm.Expr("id")).Error; err != nil {
					return err
				}
			}
			var channel Channel
			err := lockForUpdate(tx).First(&channel, channelID).Error
			if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
				return err
			}
			var snapshots []UpstreamCredentialSnapshot
			if err == nil {
				for index := range channelCredentialKeys(&channel) {
					snapshots = append(snapshots, channel.GetUpstreamCredentialSnapshot(index))
				}
			}
			return ArchiveDetachedChannelFunding(tx, channelID, snapshots)
		}); err != nil {
			return err
		}
	}
	var rows []ChannelFundingIncident
	if err := DB.Where("state = ? AND active_key IS NOT NULL AND first_seen <= ?", "confirmed", now-int64(reminderHours)*3600).Find(&rows).Error; err != nil {
		return err
	}
	for _, row := range rows {
		if err := DB.Transaction(func(tx *gorm.DB) error {
			if common.UsingMainDatabase(common.DatabaseTypeSQLite) {
				if err := tx.Model(&ChannelFundingIncident{}).Where("id = ?", row.ID).UpdateColumn("id", gorm.Expr("id")).Error; err != nil {
					return err
				}
			}
			var current ChannelFundingIncident
			if err := lockForUpdate(tx).First(&current, row.ID).Error; err != nil {
				return err
			}
			if current.ActiveKey == nil || current.State != "confirmed" {
				return nil
			}
			return enqueueFundingDeliveries(tx, &current, "reminder", (now-current.FirstSeen)/(int64(max(1, reminderHours))*3600))
		}); err != nil {
			return err
		}
	}
	cutoff := now - 90*24*3600
	old := DB.Model(&ChannelFundingIncident{}).Select("id").Where("active_key IS NULL AND resolved_at < ?", cutoff)
	if err := DB.Where("incident_id IN (?)", old).Delete(&ChannelFundingDelivery{}).Error; err != nil {
		return err
	}
	return DB.Where("active_key IS NULL AND resolved_at < ?", cutoff).Delete(&ChannelFundingIncident{}).Error
}

// ArchiveDetachedChannelFunding runs with the channel edit transaction. Stable
// procurement identities may be reused after removal, but old attempts may not.
func ArchiveDetachedChannelFunding(tx *gorm.DB, channelID int, snapshots []UpstreamCredentialSnapshot) error {
	var rows []ChannelFundingIncident
	if err := tx.Where("channel_id = ? AND active_key IS NOT NULL", channelID).Find(&rows).Error; err != nil {
		return err
	}
	current := make(map[string]UpstreamCredentialSnapshot, len(snapshots))
	for _, snapshot := range snapshots {
		current[snapshot.BindingID] = snapshot
	}
	for _, row := range rows {
		snapshot, ok := current[row.BindingID]
		if ok && snapshot.CredentialVersionID == row.CredentialVersionID && snapshot.BindingActivatedAt == row.BindingActivatedAt {
			continue
		}
		if err := tx.Model(&row).Updates(map[string]any{"active_key": nil, "state": "dismissed", "resolved_at": common.GetTimestamp(), "resolved_after": time.Now().UnixMilli(), "revision": gorm.Expr("revision + 1"), "resolution": "Credential removed or replaced", "probe_status": "cancelled", "next_probe_at": 0, "probe_lease_until": 0, "probe_token": ""}).Error; err != nil {
			return err
		}
		if err := cancelFundingDeliveries(tx.Model(&ChannelFundingDelivery{}).Where("incident_id = ?", row.ID)); err != nil {
			return err
		}
	}
	return nil
}
