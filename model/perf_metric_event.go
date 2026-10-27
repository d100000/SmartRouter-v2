package model

import (
	"context"
	"errors"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/setting/perf_metrics_setting"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// PerfMetricEvent is a durable terminal observation in the primary database.
// It contains numeric measurements and bounded dimensions, never request or
// response bodies. ID is stable across retries and asynchronous task polling.
type PerfMetricEvent struct {
	ID                string `gorm:"type:varchar(96);primaryKey"`
	ModelName         string `gorm:"size:128"`
	Group             string `gorm:"column:group;size:64"`
	BucketTs          int64
	SampleAt          int64 `gorm:"index:idx_perf_event_sample"`
	AcceptedAt        int64 `gorm:"index:idx_perf_event_projection,priority:2;index:idx_perf_event_accepted"`
	Success           bool
	LatencyMs         int64
	TtftMs            int64
	HasTtft           bool
	OutputTokens      int64
	OutputTokensKnown bool
	GenerationMs      int64
	ProjectedAt       int64 `gorm:"index:idx_perf_event_projection,priority:1"`
}

func (PerfMetricEvent) TableName() string {
	return "perf_metric_events"
}

type PerfMetricCollectionStatus struct {
	PendingEvents         int64
	PendingTaskEvents     int64
	FailedTaskEvents      int64
	OldestPendingAt       int64
	OldestPendingSampleAt int64
	OldestPendingTaskAt   int64
	LastAcceptedAt        int64
	LastProjectedAt       int64
	LastSampleAt          int64
}

// AcceptPerfMetricEvent acknowledges a terminal sample only after INSERT has
// committed. The caller may pass its own transaction to make task completion
// and metric acceptance atomic. A replay never changes the accepted payload.
func AcceptPerfMetricEvent(ctx context.Context, db *gorm.DB, event *PerfMetricEvent) error {
	if db == nil {
		return errors.New("performance metric database is unavailable")
	}
	if event == nil {
		return errors.New("performance metric event is required")
	}
	event.ModelName = strings.TrimSpace(event.ModelName)
	event.Group = strings.TrimSpace(event.Group)
	if event.ID == "" || event.ModelName == "" || utf8.RuneCountInString(event.ID) > 96 || utf8.RuneCountInString(event.ModelName) > 128 || utf8.RuneCountInString(event.Group) > 64 {
		return errors.New("performance metric event has invalid dimensions")
	}
	if event.SampleAt <= 0 || event.BucketTs < 0 || event.BucketTs > event.SampleAt || event.LatencyMs < 0 || event.TtftMs < 0 || event.OutputTokens < 0 || event.GenerationMs < 0 {
		return errors.New("performance metric event has invalid measurements")
	}
	if cutoff := perf_metrics_setting.GetRetentionCutoff(time.Now()); cutoff > 0 && event.SampleAt < cutoff {
		return nil
	}
	if event.AcceptedAt <= 0 {
		event.AcceptedAt = time.Now().Unix()
	}
	event.ProjectedAt = 0
	return db.WithContext(ctx).Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "id"}}, DoNothing: true}).Create(event).Error
}

var errPerfMetricClaimChanged = errors.New("performance metric projection claim changed")

// ProjectPerfMetricEvents claims and folds terminal observations in one
// transaction. Its first transactional statement is a write, avoiding SQLite's
// deferred-read lock upgrade race. Any worker can replay after a process dies.
func ProjectPerfMetricEvents(ctx context.Context, db *gorm.DB) (int, error) {
	if db == nil {
		return 0, errors.New("performance metric database is unavailable")
	}
	var events []PerfMetricEvent
	if err := db.WithContext(ctx).Where("projected_at = ?", 0).Order("accepted_at ASC, id ASC").Limit(100).Find(&events).Error; err != nil {
		return 0, err
	}
	if len(events) == 0 {
		return 0, nil
	}
	projected := 0
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		ids := make([]string, len(events))
		for i := range events {
			ids[i] = events[i].ID
		}
		claim := tx.Model(&PerfMetricEvent{}).Where("id IN ? AND projected_at = ?", ids, 0).Update("projected_at", time.Now().Unix())
		if claim.Error != nil {
			return claim.Error
		}
		if claim.RowsAffected == 0 {
			return nil
		}
		if claim.RowsAffected != int64(len(events)) {
			return errPerfMetricClaimChanged
		}
		type dimension struct {
			model, group string
			bucket       int64
		}
		rows := make(map[dimension]*PerfMetric, len(events))
		keys := make([]dimension, 0, len(events))
		for _, event := range events {
			key := dimension{model: event.ModelName, group: event.Group, bucket: event.BucketTs}
			row := rows[key]
			if row == nil {
				row = &PerfMetric{ModelName: event.ModelName, Group: event.Group, BucketTs: event.BucketTs}
				rows[key] = row
				keys = append(keys, key)
			}
			row.RequestCount++
			if event.Success {
				row.SuccessCount++
			}
			row.TotalLatencyMs += event.LatencyMs
			if event.HasTtft {
				row.TtftSumMs += event.TtftMs
				row.TtftCount++
			}
			if event.OutputTokensKnown && event.GenerationMs > 0 {
				row.OutputTokens += event.OutputTokens
				row.GenerationMs += event.GenerationMs
			}
			row.LastSampleAt = max(row.LastSampleAt, event.SampleAt)
		}
		// Consistent aggregate lock order also covers batches with multiple
		// models/groups, reducing deadlocks between independently running nodes.
		slices.SortFunc(keys, func(a, b dimension) int {
			if order := strings.Compare(a.model, b.model); order != 0 {
				return order
			}
			if order := strings.Compare(a.group, b.group); order != 0 {
				return order
			}
			if a.bucket < b.bucket {
				return -1
			}
			if a.bucket > b.bucket {
				return 1
			}
			return 0
		})
		for _, key := range keys {
			if err := upsertPerfMetric(tx, rows[key]); err != nil {
				return err
			}
		}
		projected = len(events)
		return nil
	})
	if errors.Is(err, errPerfMetricClaimChanged) {
		return 0, nil
	}
	return projected, err
}

func GetPerfMetricCollectionStatus(ctx context.Context, db *gorm.DB) (PerfMetricCollectionStatus, error) {
	var status PerfMetricCollectionStatus
	if db == nil {
		return status, errors.New("performance metric database is unavailable")
	}
	if err := db.WithContext(ctx).Model(&PerfMetricEvent{}).Where("projected_at = ?", 0).
		Select("COUNT(*) AS pending_events, COALESCE(MIN(accepted_at), 0) AS oldest_pending_at, COALESCE(MIN(sample_at), 0) AS oldest_pending_sample_at").
		Scan(&status).Error; err != nil {
		return status, err
	}
	// Each MAX uses its own leading index; polling collection health must not
	// scan every completed event when retention is configured to keep history.
	for _, value := range []struct {
		column string
		target *int64
	}{
		{"accepted_at", &status.LastAcceptedAt}, {"projected_at", &status.LastProjectedAt}, {"sample_at", &status.LastSampleAt},
	} {
		if err := db.WithContext(ctx).Model(&PerfMetricEvent{}).Select("COALESCE(MAX(" + value.column + "), 0)").Scan(value.target).Error; err != nil {
			return status, err
		}
	}
	var aggregateSampleAt int64
	if err := db.WithContext(ctx).Model(&PerfMetric{}).Select("COALESCE(MAX(last_sample_at), 0)").Scan(&aggregateSampleAt).Error; err != nil {
		return status, err
	}
	status.LastSampleAt = max(status.LastSampleAt, aggregateSampleAt)
	var taskStatus struct {
		PendingTaskEvents   int64
		FailedTaskEvents    int64
		OldestPendingTaskAt int64
	}
	if err := db.WithContext(ctx).Model(&Task{}).Where("performance_sample_pending = ?", true).
		Select("COUNT(*) AS pending_task_events, COALESCE(SUM(CASE WHEN performance_sample_error IS NOT NULL AND performance_sample_error <> '' THEN 1 ELSE 0 END), 0) AS failed_task_events, COALESCE(MIN(finish_time), 0) AS oldest_pending_task_at").Scan(&taskStatus).Error; err != nil {
		return status, err
	}
	status.PendingTaskEvents = taskStatus.PendingTaskEvents
	status.FailedTaskEvents = taskStatus.FailedTaskEvents
	status.OldestPendingTaskAt = taskStatus.OldestPendingTaskAt
	return status, nil
}

// Pending observations are retained even beyond the normal metric history
// window. They remain diagnosable and recoverable until projection succeeds.
func DeletePerfMetricEventsBefore(ctx context.Context, db *gorm.DB, cutoff int64) error {
	if db == nil {
		return errors.New("performance metric database is unavailable")
	}
	if cutoff <= 0 {
		return nil
	}
	return db.WithContext(ctx).Where("projected_at > ? AND sample_at < ?", 0, cutoff).Delete(&PerfMetricEvent{}).Error
}
