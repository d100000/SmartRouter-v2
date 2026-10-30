package model

import (
	"context"
	"crypto/sha256"
	"fmt"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm/clause"
)

// SystemResourcePoint uses null for unavailable readings, including the first
// CPU/network sample, which has no counter baseline yet. Rates are bytes/second.
type SystemResourcePoint struct {
	Timestamp      int64    `json:"timestamp"`
	CPUPercent     *float64 `json:"cpu_percent"`
	MemoryPercent  *float64 `json:"memory_percent"`
	MemoryUsed     *uint64  `json:"memory_used"`
	MemoryTotal    *uint64  `json:"memory_total"`
	DiskPercent    *float64 `json:"disk_percent"`
	DiskUsed       *uint64  `json:"disk_used"`
	DiskTotal      *uint64  `json:"disk_total"`
	NetworkReceive *float64 `json:"network_receive"`
	NetworkSend    *float64 `json:"network_send"`
}

// One sample per node/minute. The fixed ASCII digest keeps the composite key
// within MySQL 5.7's index limit even with an utf8mb4 database default.
type SystemResourceSample struct {
	NodeKey string `gorm:"type:varchar(64);primaryKey"`
	Minute  int64  `gorm:"primaryKey;autoIncrement:false;index"`
	Payload string `gorm:"type:text;not null"`
}

func SaveSystemResourceSample(ctx context.Context, node string, point SystemResourcePoint) error {
	payload, err := common.Marshal(point)
	if err != nil {
		return err
	}
	row := SystemResourceSample{
		NodeKey: fmt.Sprintf("%x", sha256.Sum256([]byte(node))),
		Minute:  point.Timestamp / 60 * 60,
		Payload: string(payload),
	}
	return DB.WithContext(ctx).Clauses(clause.OnConflict{
		Columns:   []clause.Column{{Name: "node_key"}, {Name: "minute"}},
		DoUpdates: clause.AssignmentColumns([]string{"payload"}),
	}).Create(&row).Error
}

func ListSystemResourceSamples(ctx context.Context, node string, from, to int64) ([]SystemResourcePoint, error) {
	var rows []SystemResourceSample
	err := DB.WithContext(ctx).
		Where("node_key = ? AND minute >= ? AND minute <= ?", fmt.Sprintf("%x", sha256.Sum256([]byte(node))), from/60*60, to).
		Order("minute ASC").Limit(1441).Find(&rows).Error
	if err != nil {
		return nil, err
	}
	points := make([]SystemResourcePoint, 0, len(rows))
	for _, row := range rows {
		var point SystemResourcePoint
		if err := common.UnmarshalJsonStr(row.Payload, &point); err != nil {
			return nil, err
		}
		if point.Timestamp >= from && point.Timestamp <= to {
			points = append(points, point)
		}
	}
	return points, nil
}

func DeleteExpiredSystemResourceSamples(ctx context.Context, before int64) error {
	return DB.WithContext(ctx).Where("minute < ?", before/60*60).Delete(&SystemResourceSample{}).Error
}
