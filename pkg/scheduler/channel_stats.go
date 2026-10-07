package scheduler

import (
	"cmp"
	"slices"
	"sync"
	"time"
)

const (
	recentChannelWindow  = 10 * time.Minute
	channelHistoryWindow = time.Hour
	channelLatencyWindow = 5 * time.Minute
)

type ChannelHistoryStat struct {
	StartTime   int64    `json:"start_time"`
	EndTime     int64    `json:"end_time"`
	Requests    int64    `json:"requests"`
	Successes   int64    `json:"successes"`
	SuccessRate *float64 `json:"success_rate"`
}

type RecentChannelStat struct {
	ChannelID         int                  `json:"channel_id"`
	Requests          int64                `json:"requests"`
	Successes         int64                `json:"successes"`
	SuccessRate       *float64             `json:"success_rate"`
	Requests1h        int64                `json:"requests_1h"`
	Successes1h       int64                `json:"successes_1h"`
	SuccessRate1h     *float64             `json:"success_rate_1h"`
	History           []ChannelHistoryStat `json:"history"`
	TTFTSamples5m     int64                `json:"ttft_samples_5m"`
	TTFTSumMS5m       float64              `json:"ttft_sum_ms_5m"`
	AvgTTFTMS5m       *float64             `json:"avg_ttft_ms_5m"`
	ResponseSamples5m int64                `json:"response_samples_5m"`
	ResponseSumMS5m   float64              `json:"response_sum_ms_5m"`
	AvgResponseMS5m   *float64             `json:"avg_response_ms_5m"`
}

type RecentChannelStats struct {
	Ready                bool                `json:"ready"`
	RefreshedAt          int64               `json:"refreshed_at"`
	CollectedSince       int64               `json:"collected_since"`
	WindowSeconds        int64               `json:"window_seconds"`
	HistoryWindowSeconds int64               `json:"history_window_seconds"`
	BucketSeconds        int64               `json:"bucket_seconds"`
	LatencyWindowSeconds int64               `json:"latency_window_seconds"`
	Scope                string              `json:"scope"`
	Items                []RecentChannelStat `json:"items"`
}

// Channel observations have no model/group dimension or latency histograms.
// Their lock is never held with Engine.mu, so dashboard reads cannot stall routing.
type channelObservations struct {
	mu               sync.Mutex
	refreshMu        sync.Mutex
	channels         map[int]map[int64]*channelObservationBucket
	collectedSince   int64
	initialized      bool
	version          uint64
	publishedVersion uint64
}

type channelObservationBucket struct {
	attempts     int64
	successes    int64
	latencyCount [2]int64
	latencySumMS [2]float64
}

type channelObservation struct {
	channelID int
	second    int64
	success   bool
	stream    bool
	latencyMS float64
}

// recordChannelObservation is called only after releasing the routing lock.
// A completed attempt adds one observation even when its learning epoch expired.
func (e *Engine) recordChannelObservation(observation channelObservation) {
	stats := &e.channelObservations
	stats.mu.Lock()
	defer stats.mu.Unlock()
	if !stats.initialized {
		stats.collectedSince, stats.initialized = observation.second, true
	} else {
		stats.collectedSince = min(stats.collectedSince, observation.second)
	}
	if stats.channels == nil {
		stats.channels = make(map[int]map[int64]*channelObservationBucket)
	}
	buckets := stats.channels[observation.channelID]
	if buckets == nil {
		buckets = make(map[int64]*channelObservationBucket)
		stats.channels[observation.channelID] = buckets
	}
	bucket := buckets[observation.second]
	if bucket == nil {
		bucket = &channelObservationBucket{}
		buckets[observation.second] = bucket
	}
	bucket.attempts++
	if observation.success {
		bucket.successes++
		if observation.latencyMS > 0 {
			streamIndex := 0
			if observation.stream {
				streamIndex = 1
			}
			bucket.latencyCount[streamIndex]++
			bucket.latencySumMS[streamIndex] += observation.latencyMS
		}
	}
	stats.version++
}

// RecentChannelStats reads the last published snapshot without aggregating.
func (e *Engine) RecentChannelStats() RecentChannelStats {
	return cloneRecentChannelStats(e.recentChannelStats.Load(), nil)
}

// FreshRecentChannelStats includes completed observations at the current second.
// Concurrent readers share a rebuild; a new completion invalidates it even within
// that second. Time also invalidates it so quiet channels expire normally.
func (e *Engine) FreshRecentChannelStats(channelIDs ...int) RecentChannelStats {
	e.RefreshRecentChannelStats()
	return cloneRecentChannelStats(e.recentChannelStats.Load(), channelIDs)
}

func cloneRecentChannelStats(cached *RecentChannelStats, channelIDs []int) RecentChannelStats {
	if cached == nil {
		return RecentChannelStats{
			WindowSeconds: int64(recentChannelWindow / time.Second), HistoryWindowSeconds: int64(channelHistoryWindow / time.Second),
			BucketSeconds: int64(recentChannelWindow / time.Second), LatencyWindowSeconds: int64(channelLatencyWindow / time.Second),
			Scope: "instance", Items: []RecentChannelStat{},
		}
	}
	selected := make(map[int]bool, len(channelIDs))
	for _, id := range channelIDs {
		selected[id] = true
	}
	result := *cached
	capacity := len(cached.Items)
	if len(selected) > 0 {
		capacity = min(capacity, len(selected))
	}
	result.Items = make([]RecentChannelStat, 0, capacity)
	for _, row := range cached.Items {
		if len(selected) > 0 && !selected[row.ChannelID] {
			continue
		}
		for _, rate := range []**float64{&row.SuccessRate, &row.SuccessRate1h, &row.AvgTTFTMS5m, &row.AvgResponseMS5m} {
			if *rate != nil {
				value := **rate
				*rate = &value
			}
		}
		row.History = slices.Clone(row.History)
		for i := range row.History {
			if rate := row.History[i].SuccessRate; rate != nil {
				value := *rate
				row.History[i].SuccessRate = &value
			}
		}
		result.Items = append(result.Items, row)
	}
	return result
}

// RefreshRecentChannelStats aggregates instance-local, completed effective attempts.
// It never visits routing state, database logs, or learning windows. Background
// refresh also removes expired buckets and inactive channels when traffic stops.
func (e *Engine) RefreshRecentChannelStats() {
	stats := &e.channelObservations
	stats.refreshMu.Lock()
	defer stats.refreshMu.Unlock()
	now := e.now().Unix()
	stats.mu.Lock()
	if !stats.initialized {
		stats.collectedSince, stats.initialized = now, true
	}
	if cached := e.recentChannelStats.Load(); cached != nil && cached.RefreshedAt == now && stats.publishedVersion == stats.version {
		stats.mu.Unlock()
		return
	}
	windowSeconds := int64(recentChannelWindow / time.Second)
	historySeconds := int64(channelHistoryWindow / time.Second)
	latencySeconds := int64(channelLatencyWindow / time.Second)
	cutoff := now - historySeconds
	result := &RecentChannelStats{
		Ready: true, RefreshedAt: now, CollectedSince: stats.collectedSince,
		WindowSeconds: windowSeconds, HistoryWindowSeconds: historySeconds,
		BucketSeconds: windowSeconds, LatencyWindowSeconds: latencySeconds,
		Scope: "instance", Items: make([]RecentChannelStat, 0, len(stats.channels)),
	}
	for id, buckets := range stats.channels {
		row := RecentChannelStat{ChannelID: id, History: make([]ChannelHistoryStat, 6)}
		for i := range row.History {
			row.History[i].StartTime = cutoff + int64(i)*windowSeconds
			row.History[i].EndTime = cutoff + int64(i+1)*windowSeconds
		}
		for second, bucket := range buckets {
			if second <= cutoff {
				delete(buckets, second)
				continue
			}
			if second > now {
				continue
			}
			row.Requests1h += bucket.attempts
			row.Successes1h += bucket.successes
			segment := &row.History[(second-cutoff-1)/windowSeconds]
			segment.Requests += bucket.attempts
			segment.Successes += bucket.successes
			if second > now-windowSeconds {
				row.Requests += bucket.attempts
				row.Successes += bucket.successes
			}
			if second > now-latencySeconds {
				row.ResponseSamples5m += bucket.latencyCount[0]
				row.ResponseSumMS5m += bucket.latencySumMS[0]
				row.TTFTSamples5m += bucket.latencyCount[1]
				row.TTFTSumMS5m += bucket.latencySumMS[1]
			}
		}
		if len(buckets) == 0 {
			delete(stats.channels, id)
		}
		if row.Requests1h > 0 {
			result.Items = append(result.Items, row)
		}
	}
	stats.publishedVersion = stats.version
	stats.mu.Unlock()
	for i := range result.Items {
		row := &result.Items[i]
		row.SuccessRate = ratio(row.Successes, row.Requests)
		row.SuccessRate1h = ratio(row.Successes1h, row.Requests1h)
		row.AvgTTFTMS5m = meanLatency(row.TTFTSumMS5m, row.TTFTSamples5m)
		row.AvgResponseMS5m = meanLatency(row.ResponseSumMS5m, row.ResponseSamples5m)
		for j := range row.History {
			row.History[j].SuccessRate = ratio(row.History[j].Successes, row.History[j].Requests)
		}
	}
	slices.SortFunc(result.Items, func(a, b RecentChannelStat) int { return cmp.Compare(a.ChannelID, b.ChannelID) })
	e.recentChannelStats.Store(result)
}
