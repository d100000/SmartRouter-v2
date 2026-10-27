package perfmetrics

import (
	"context"
	"time"

	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/perf_metrics_setting"
)

type CollectionHealth struct {
	Enabled                 bool   `json:"enabled"`
	Availability            string `json:"availability"`
	PendingEvents           int64  `json:"pending_events"`
	OldestPendingAt         *int64 `json:"oldest_pending_at"`
	LastSampleAt            *int64 `json:"last_sample_at"`
	LastAcceptedAt          *int64 `json:"last_accepted_at"`
	LastProjectedAt         *int64 `json:"last_projected_at"`
	PersistedThrough        *int64 `json:"persisted_through"`
	CollectorHeartbeatAt    *int64 `json:"collector_heartbeat_at"`
	UnpersistedEvents       int64  `json:"unpersisted_events"`
	OldestUnpersistedAt     *int64 `json:"oldest_unpersisted_at"`
	DroppedEvents           int64  `json:"dropped_events"`
	QuarantinedEvents       int64  `json:"quarantined_events"`
	StaleCollectors         int64  `json:"stale_collectors"`
	LocalUnpersistedEvents  int64  `json:"local_unpersisted_events"`
	Storage                 string `json:"storage"`
	TargetVisibilitySeconds int64  `json:"target_visibility_seconds"`
}

// CollectionStatus separates durable acceptance, projection lag and the risk
// that has not reached SQL. A maximum sample timestamp is deliberately not
// advertised as a complete persisted-through watermark: events can arrive late.
func CollectionStatus(ctx context.Context, windowStart int64) (CollectionHealth, error) {
	status, err := model.GetPerfMetricCollectionStatus(ctx, model.DB)
	if err != nil {
		return CollectionHealth{}, err
	}
	rows, err := model.GetPerfMetricCollectors(ctx, model.DB)
	if err != nil {
		return CollectionHealth{}, err
	}
	now := time.Now().Unix()
	health := CollectionHealth{Enabled: perf_metrics_setting.GetSetting().Enabled,
		Availability: "ready", Storage: "database_outbox", TargetVisibilitySeconds: 60,
		PendingEvents: status.PendingEvents + status.PendingTaskEvents, QuarantinedEvents: status.FailedTaskEvents,
		LastSampleAt:    positiveTimestamp(status.LastSampleAt),
		LastAcceptedAt:  positiveTimestamp(status.LastAcceptedAt),
		LastProjectedAt: positiveTimestamp(status.LastProjectedAt)}
	oldestPending := status.OldestPendingAt
	if status.OldestPendingTaskAt > 0 && (oldestPending == 0 || status.OldestPendingTaskAt < oldestPending) {
		oldestPending = status.OldestPendingTaskAt
	}
	health.OldestPendingAt = positiveTimestamp(oldestPending)
	p := collector
	p.mu.Lock()
	localID := p.id
	health.LocalUnpersistedEvents = int64(len(p.pending))
	health.UnpersistedEvents = health.LocalUnpersistedEvents
	health.DroppedEvents = p.dropped
	oldestUnpersisted := int64(0)
	for _, event := range p.pending {
		if oldestUnpersisted == 0 || event.SampleAt < oldestUnpersisted {
			oldestUnpersisted = event.SampleAt
		}
	}
	p.mu.Unlock()
	lastHeartbeat := int64(0)
	for _, row := range rows {
		lastHeartbeat = max(lastHeartbeat, row.HeartbeatAt)
		if row.ID == localID {
			continue // The live local queue is newer than its heartbeat snapshot.
		}
		if row.HeartbeatAt < windowStart && row.UnpersistedEvents == 0 && row.DroppedEvents == 0 {
			continue
		}
		health.UnpersistedEvents += row.UnpersistedEvents
		health.DroppedEvents += row.DroppedEvents
		if row.OldestUnpersistedAt > 0 && (oldestUnpersisted == 0 || row.OldestUnpersistedAt < oldestUnpersisted) {
			oldestUnpersisted = row.OldestUnpersistedAt
		}
		if row.StoppedAt == 0 && now-row.HeartbeatAt > 90 {
			health.StaleCollectors++
		}
	}
	health.OldestUnpersistedAt = positiveTimestamp(oldestUnpersisted)
	health.CollectorHeartbeatAt = positiveTimestamp(lastHeartbeat)
	if !health.Enabled {
		health.Availability = "disabled"
	}
	if health.Enabled && (lastHeartbeat == 0 || now-lastHeartbeat > 60) || oldestPending > 0 && now-oldestPending > 60 {
		health.Availability = "delayed"
	}
	if health.UnpersistedEvents > 0 || health.DroppedEvents > 0 || health.StaleCollectors > 0 || status.FailedTaskEvents > 0 {
		health.Availability = "partial"
	}
	return health, nil
}

func positiveTimestamp(value int64) *int64 {
	if value <= 0 {
		return nil
	}
	return &value
}
