package model

import (
	"context"
	"errors"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// PerfMetricCollector records a process's unconfirmed collection risk. A boot
// ID is never reused, so a restarted process cannot clear a crashed one's risk.
// No database errors, request identifiers or credentials are persisted here.
type PerfMetricCollector struct {
	ID                  string `gorm:"type:varchar(96);primaryKey"`
	HeartbeatAt         int64  `gorm:"index"`
	StoppedAt           int64
	UnpersistedEvents   int64
	OldestUnpersistedAt int64
	DroppedEvents       int64
	LastErrorAt         int64
	LastSuccessAt       int64
}

func SavePerfMetricCollector(ctx context.Context, db *gorm.DB, state PerfMetricCollector) error {
	if db == nil {
		return errors.New("performance metric database is unavailable")
	}
	return db.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "id"}},
		DoUpdates: clause.AssignmentColumns([]string{"heartbeat_at", "stopped_at", "unpersisted_events", "oldest_unpersisted_at", "dropped_events", "last_error_at", "last_success_at"}),
	}).Create(&state).Error
}

func GetPerfMetricCollectors(ctx context.Context, db *gorm.DB) ([]PerfMetricCollector, error) {
	if db == nil {
		return nil, errors.New("performance metric database is unavailable")
	}
	var rows []PerfMetricCollector
	// Clean exits with no outstanding risk need not stay on the active surface.
	// Unclean exits and any reported risk remain visible instead of silently
	// becoming healthy just because their heartbeat stopped.
	err := db.WithContext(ctx).Where("stopped_at = ? OR unpersisted_events > ? OR dropped_events > ?", 0, 0, 0).Find(&rows).Error
	return rows, err
}
