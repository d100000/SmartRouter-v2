package scheduler

import (
	"cmp"
	"slices"
	"time"
)

const recentChannelWindow = 10 * time.Minute

type RecentChannelStat struct {
	ChannelID   int      `json:"channel_id"`
	Requests    int64    `json:"requests"`
	Successes   int64    `json:"successes"`
	SuccessRate *float64 `json:"success_rate"`
}

type RecentChannelStats struct {
	Ready         bool                `json:"ready"`
	RefreshedAt   int64               `json:"refreshed_at"`
	WindowSeconds int64               `json:"window_seconds"`
	Scope         string              `json:"scope"`
	Items         []RecentChannelStat `json:"items"`
}

// RecentChannelStats reads only the published snapshot. Copying its small rows
// keeps callers from mutating the cache without visiting routing state or buckets.
func (e *Engine) RecentChannelStats() RecentChannelStats {
	cached := e.recentChannelStats.Load()
	if cached == nil {
		return RecentChannelStats{WindowSeconds: int64(recentChannelWindow / time.Second), Scope: "instance", Items: []RecentChannelStat{}}
	}
	result := *cached
	result.Items = slices.Clone(cached.Items)
	for i := range result.Items {
		if rate := result.Items[i].SuccessRate; rate != nil {
			value := *rate
			result.Items[i].SuccessRate = &value
		}
	}
	return result
}

// RefreshRecentChannelStats is background-only. It combines completed effective
// attempts across groups, models and transports, independently of logging and
// learning epochs. A failed attempt remains a failure when its retry succeeds.
func (e *Engine) RefreshRecentChannelStats() {
	e.mu.Lock()
	now := e.now().Unix()
	keys := make([]Key, 0, len(e.states))
	for key := range e.states {
		keys = append(keys, key)
	}
	e.mu.Unlock()
	cutoff := now - int64(recentChannelWindow/time.Second)
	byChannel := make(map[int]RecentChannelStat)
	for _, key := range keys {
		// Release the routing lock between dimensions so refreshing a large
		// installation does not hold it for the complete aggregation.
		e.mu.Lock()
		for id, channel := range e.states[key].channels {
			row := byChannel[id]
			for second, bucket := range channel.buckets {
				if second <= cutoff || second > now {
					continue
				}
				row.Requests += bucket.attempts
				row.Successes += bucket.successes
			}
			if row.Requests > 0 {
				row.ChannelID = id
				byChannel[id] = row
			}
		}
		e.mu.Unlock()
	}
	result := &RecentChannelStats{
		Ready: true, RefreshedAt: now, WindowSeconds: int64(recentChannelWindow / time.Second),
		Scope: "instance", Items: make([]RecentChannelStat, 0, len(byChannel)),
	}
	for _, row := range byChannel {
		row.SuccessRate = ratio(row.Successes, row.Requests)
		result.Items = append(result.Items, row)
	}
	slices.SortFunc(result.Items, func(a, b RecentChannelStat) int { return cmp.Compare(a.ChannelID, b.ChannelID) })
	e.recentChannelStats.Store(result)
}
