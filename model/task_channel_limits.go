package model

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"time"

	"github.com/QuantumNous/new-api/common"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"gorm.io/gorm"
)

// The version write obtains the same primary-row lock on all three engines.
// Observation and pending marker commit with the business status transition.
func (task *Task) updateChannelTaskWithStatus(from TaskStatus) (bool, error) {
	won := false
	err := DB.Transaction(func(tx *gorm.DB) error {
		result := tx.Model(&Task{}).Where("id = ? AND status = ? AND COALESCE(procurement_version, 0) = ?", task.ID, from, task.ProcurementVersion).
			UpdateColumn("procurement_version", gorm.Expr("COALESCE(procurement_version, 0) + 1"))
		if result.Error != nil || result.RowsAffected == 0 {
			return result.Error
		}
		var current Task
		if err := tx.Select("procurement_version").First(&current, task.ID).Error; err != nil {
			return err
		}
		task.ProcurementVersion, task.ProcurementPending = current.ProcurementVersion, true
		task.ProcurementUnknown = task.PrivateData.ChannelAttempt.NeedsReconciliation()
		task.ProcurementNextPoll = time.Now().Add(time.Minute).Unix()
		if err := tx.Model(task).Where("status = ?", from).Select("*").Updates(task).Error; err != nil {
			return err
		}
		won = true
		return nil
	})
	return won, err
}

// ApplyChannelTaskObservation updates the original submit observation rather
// than creating synthetic attempts for queries or task completion. The ledger
// settles first with this same monotonic version; crash retries are idempotent.
func ApplyChannelTaskObservation(ctx context.Context, snapshot *relaycommon.TaskChannelSnapshot, version int64) error {
	if snapshot == nil || snapshot.Overview == nil {
		return nil
	}
	observation := snapshot.Observation
	return DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		id := snapshot.Overview.EventID
		if err := tx.Model(&OverviewAttemptEvent{}).Where("id = ?", id).
			UpdateColumn("task_observation_version", gorm.Expr("COALESCE(task_observation_version, 0)")).Error; err != nil {
			return err
		}
		var previous OverviewAttemptEvent
		if err := lockForUpdate(tx).First(&previous, "id = ?", id).Error; err != nil {
			return err
		}
		if previous.TaskObservationVersion < version {
			changes := map[string]any{"task_status": observation.Status, "task_observed_at": observation.ObservedAt,
				"task_completed_at": observation.CompletedAt, "task_observation_version": version}
			if observation.CompletedAt != nil {
				changes["task_duration_ms"] = max(int64(0), *observation.CompletedAt*1000-snapshot.Overview.StartedAt.UnixMilli())
			}
			if observation.ImageCount != nil {
				changes["image_count"], changes["image_usage_source"] = observation.ImageCount, "upstream_usage"
			}
			if observation.VideoSeconds != nil {
				changes["video_seconds"], changes["video_usage_source"] = observation.VideoSeconds, "upstream_usage"
			}
			if observation.AudioSeconds != nil {
				changes["audio_seconds"], changes["audio_usage_source"] = observation.AudioSeconds, "upstream_usage"
			}
			if observation.Credits != nil {
				changes["credits"], changes["credit_usage_source"] = observation.Credits, "upstream_usage"
			}
			if observation.CostUSDNano != nil {
				cost := float64(*observation.CostUSDNano) / float64(ChannelLimitUSDScale) * snapshot.QuotaPerUnit
				changes["cost_known"], changes["cost_quota"] = true, cost
				if previous.Status == "completed" {
					identity, err := common.Marshal([]any{previous.Day, previous.ChannelID, previous.GroupName, previous.ModelName, previous.CredentialID, previous.SupplierID, previous.OwnershipVersionID, previous.CostVersionID, previous.QuotaPerUnit})
					if err != nil {
						return err
					}
					hash := sha256.Sum256(identity)
					delta := cost
					if previous.CostKnown {
						delta -= previous.CostQuota
					}
					aggregates := map[string]any{"cost": gorm.Expr("cost + ?", delta)}
					if !previous.CostKnown {
						aggregates["unknown_cost_attempts"] = gorm.Expr("unknown_cost_attempts - 1")
					}
					if previous.RequestComplete {
						aggregates["complete_cost"] = gorm.Expr("complete_cost + ?", delta)
					}
					if err := tx.Model(&OverviewDaily{}).Where("id = ?", hex.EncodeToString(hash[:])).Updates(aggregates).Error; err != nil {
						return err
					}
				}
			}
			if err := tx.Model(&OverviewAttemptEvent{}).Where("id = ?", id).Updates(changes).Error; err != nil {
				return err
			}
		}
		return nil
	})
}
