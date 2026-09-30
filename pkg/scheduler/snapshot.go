package scheduler

import "time"

func ratio(numerator, denominator int64) *float64 {
	if denominator == 0 {
		return nil
	}
	value := float64(numerator) / float64(denominator)
	return &value
}

func meanLatency(sum float64, samples int64) *float64 {
	if samples == 0 {
		return nil
	}
	value := sum / float64(samples)
	return &value
}

// Snapshot computes probabilities from the exact same candidate evaluation as
// selection. Historical success metrics are observed, unsmoothed rates; health
// and routing scores use the smoothed learning model.
func (e *Engine) Snapshot(key Key, candidates []Candidate, stream bool) Snapshot {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	s := e.state(key, now)
	minute := now.Truncate(time.Minute).Unix()
	s.loadPeaks[minute] = max(s.loadPeaks[minute], s.inFlight)
	result := Snapshot{
		Key: key, Scope: "instance", Config: s.config, Active: s.active && s.config.Enabled,
		ActivationRequests: len(s.activation), Channels: make([]ChannelSnapshot, 0, len(candidates)),
		Trend: make([]TrendPoint, 0, 30),
	}
	result.Phase = "cold"
	if result.Active {
		result.Phase = "transition"
		if s.learningSamples >= 100 {
			result.Phase = "dynamic"
		}
	}
	evaluated := e.evaluate(key, s, candidates, now, stream, nil)
	byID := make(map[int]evaluatedCandidate, len(evaluated))
	for _, item := range evaluated {
		byID[item.candidate.ID] = item
	}
	cutoff := now.Add(-longWindow).Unix()
	seen := make(map[int]bool, len(candidates))
	var totalDispatches int64
	for _, candidate := range candidates {
		if candidate.ID <= 0 || seen[candidate.ID] {
			continue
		}
		seen[candidate.ID] = true
		ch, resolved := e.resolveCandidate(key, s, candidate)
		if ch == nil {
			ch = &channelState{recoveryLimit: 1}
		}
		if !ch.registered {
			// Historical/disabled rows remain visible without creating capacity
			// members from a dashboard or a stale request candidate list.
			resolved = configuredCandidate(s, ch, resolved)
		}
		e.refreshScore(ch, s.config, now)
		pool := e.pools[resolved.CapacityKey]
		if pool == nil {
			pool = &capacityPool{}
		}
		if capacity := poolCapacity(pool); capacity > 0 {
			resolved.Capacity = capacity
		}
		long, short := channelWindows(ch, now, false, stream, s.config.LatencyTargetMS)
		row := ChannelSnapshot{
			ChannelID: resolved.ID, Name: resolved.Name, Status: resolved.Status,
			ConfiguredWeight: resolved.Weight, Priority: resolved.Priority,
			InFlight: pool.inFlight, Capacity: resolved.Capacity, CapacityKey: resolved.CapacityKey,
			SuccessRate30m: ratio(long.Successes, long.Attempts), SuccessRate5m: ratio(short.Successes, short.Attempts),
			AvgTTFTMS5m:      meanLatency(short.LatencySumMS, short.LatencySamples),
			HealthAttainment: ratio(short.HealthySamples, short.HealthSamples),
			Window30m:        long, Window5m: short,
			HealthBaseline: ch.baseHealth * 100, RecoveryLimit: ch.recoveryLimit * 100,
			RampSuccesses: ch.learningSuccesses, RampProgress: min(1, float64(ch.learningSuccesses)/channelRampSuccesses),
		}
		if resolved.CooldownUntil.After(now) {
			until := resolved.CooldownUntil
			row.CooldownUntil = &until
		}
		for second, count := range ch.dispatches {
			if second >= cutoff {
				row.Dispatches30m += count
			}
		}
		totalDispatches += row.Dispatches30m
		streamIndex := 0
		if stream {
			streamIndex = 1
		}
		health := max(0.01, min(ch.baseHealth, ch.recoveryLimit))
		row.HealthScore = health * 100
		row.QualityScore = ch.latencyScore[streamIndex] * health * 100
		row.CanRecover = result.Active && (ch.baseSuccess < s.config.SuccessTarget || short.Failures > 0 || ch.recoveryLimit+0.000001 < ch.baseHealth)
		switch {
		case resolved.Status != 1:
			row.RouteState = "disabled"
			row.CanRecover = false
		case resolved.CooldownUntil.After(now):
			row.RouteState = "cooling"
			row.CanRecover = false
		case resolved.Excluded:
			row.RouteState = "ineligible"
			row.CanRecover = false
		case resolved.Capacity > 0 && pool.inFlight >= resolved.Capacity:
			row.RouteState = "saturated"
		case !result.Active:
			row.RouteState = "cold"
		case row.CanRecover || ch.baseSuccess < s.config.SuccessTarget:
			row.RouteState = "degraded"
		default:
			row.RouteState = "eligible"
		}
		if item, ok := byID[resolved.ID]; ok {
			row.RampLimited = item.rampLimited
			row.EffectiveWeight = item.weight
			row.SelectionProbability = item.probability
			row.HealthScore = item.health * 100
			row.QualityScore = item.latency * item.health * 100
			result.Summary.EligibleChannels++
			if item.healthy {
				result.Summary.HealthyChannels++
			}
		}
		if row.RouteState == "degraded" {
			result.Summary.DegradedChannels++
		}
		result.Summary.Attempts30m += long.Attempts
		result.Summary.Successes30m += long.Successes
		result.Summary.Attempts5m += short.Attempts
		result.Summary.Successes5m += short.Successes
		result.Summary.LatencySamples5m += short.LatencySamples
		result.Summary.LatencySumMS5m += short.LatencySumMS
		result.Channels = append(result.Channels, row)
	}
	for i := range result.Channels {
		if totalDispatches > 0 {
			result.Channels[i].TrafficShare = float64(result.Channels[i].Dispatches30m) / float64(totalDispatches)
		}
		result.Summary.TopTrafficShare = max(result.Summary.TopTrafficShare, result.Channels[i].TrafficShare)
	}
	for second, count := range s.requests {
		if second >= cutoff {
			result.Summary.Requests30m += count
		}
	}
	result.Summary.InFlight = s.inFlight
	result.Summary.SuccessRate30m = ratio(result.Summary.Successes30m, result.Summary.Attempts30m)
	result.Summary.SuccessRate5m = ratio(result.Summary.Successes5m, result.Summary.Attempts5m)
	result.Summary.AvgTTFTMS5m = meanLatency(result.Summary.LatencySumMS5m, result.Summary.LatencySamples5m)
	streamIndex := 0
	if stream {
		streamIndex = 1
	}
	minuteStart := now.Truncate(time.Minute).Add(-29 * time.Minute).Unix()
	for i := range 30 {
		start := minuteStart + int64(i)*60
		point := TrendPoint{Timestamp: start}
		var successes, latencySamples int64
		latencySum := 0.0
		for second := start; second < start+60; second++ {
			point.Requests += s.requests[second]
			for channelID := range seen {
				ch := s.channels[channelID]
				if ch == nil {
					continue
				}
				if bucket := ch.buckets[second]; bucket != nil {
					point.Attempts += bucket.attempts
					successes += bucket.successes
					latencySamples += bucket.latency[streamIndex].count
					latencySum += bucket.latency[streamIndex].sumMS
				}
			}
		}
		point.SuccessRate = ratio(successes, point.Attempts)
		point.AvgTTFTMS = meanLatency(latencySum, latencySamples)
		// These are observed minute peaks, not reconstructed historical
		// instantaneous values. Unsampled minutes remain nil.
		if peak, ok := s.loadPeaks[start]; ok {
			point.InFlight = &peak
		}
		result.Trend = append(result.Trend, point)
	}
	return result
}
