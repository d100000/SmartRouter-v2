package scheduler

import (
	"math"
	"slices"
	"time"
)

type evaluatedCandidate struct {
	candidate   Candidate
	weight      float64
	probability float64
	health      float64
	latency     float64
	healthy     bool
	mature      bool
	rampLimited bool
}

func channelWindows(ch *channelState, now time.Time, learning, stream bool, targetMS float64) (Window, Window) {
	cacheIndex := 0
	if stream {
		cacheIndex++
	}
	if learning {
		cacheIndex += 2
	}
	cache := &ch.windowCache[cacheIndex]
	if cache.valid && cache.second == now.Unix() && cache.epoch == ch.learningSince.UnixNano() &&
		cache.targetMS == targetMS && cache.version == ch.outcomeVersion {
		return cache.long, cache.short
	}
	long, short := Window{}, Window{}
	longCutoff := now.Add(-longWindow).Unix()
	shortCutoff := now.Add(-shortWindow).Unix()
	buckets := ch.buckets
	if learning {
		buckets = ch.learningBuckets
	}
	streamIndex := 0
	if stream {
		streamIndex = 1
	}
	for second, bucket := range buckets {
		if second < longCutoff {
			continue
		}
		latency := bucket.latency[streamIndex]
		window := Window{
			Attempts: bucket.attempts, Successes: bucket.successes, Failures: bucket.failures,
			LatencySamples: latency.count, LatencySumMS: latency.sumMS,
			HealthSamples: latency.count + bucket.streamFailures[streamIndex],
		}
		for latencyMS, count := range latency.histogramMS {
			if float64(latencyMS) <= targetMS {
				window.HealthySamples += count
			}
		}
		addWindow(&long, window)
		if second >= shortCutoff {
			addWindow(&short, window)
		}
	}
	*cache = windowCache{valid: true, second: now.Unix(), epoch: ch.learningSince.UnixNano(),
		targetMS: targetMS, version: ch.outcomeVersion, long: long, short: short}
	return long, short
}

func addWindow(dst *Window, src Window) {
	dst.Attempts += src.Attempts
	dst.Successes += src.Successes
	dst.Failures += src.Failures
	dst.LatencySamples += src.LatencySamples
	dst.LatencySumMS += src.LatencySumMS
	dst.HealthySamples += src.HealthySamples
	dst.HealthSamples += src.HealthSamples
}

// The beta prior is 19 successes and one failure: a weak 95% prior prevents
// small samples from producing either perfect health or a zero weight.
func successHealth(long, short Window) (float64, float64) {
	longSuccess := float64(long.Successes+19) / float64(long.Attempts+20)
	shortSuccess := float64(short.Successes+19) / float64(short.Attempts+20)
	alpha := 0.6 * float64(short.Attempts) / float64(short.Attempts+20)
	success := (1-alpha)*longSuccess + alpha*shortSuccess
	error5 := float64(short.Failures+1) / float64(short.Attempts+20)
	return success, max(0.01, min(math.Pow(success, 4), 1-error5))
}

func latencyQuality(long, short Window, targetMS float64) float64 {
	// Missing observations are neutral, never a fabricated zero-millisecond
	// response. The two transport modes have independent latency samples.
	longScore := 0.5
	if long.LatencySamples > 0 {
		longScore = targetMS / (targetMS + long.LatencySumMS/float64(long.LatencySamples))
	}
	if short.LatencySamples == 0 {
		return longScore
	}
	shortScore := targetMS / (targetMS + short.LatencySumMS/float64(short.LatencySamples))
	// Only observed latency samples in this transport mode establish
	// confidence; unrelated traffic and missing TTFT cannot add confidence.
	alpha := 0.6 * float64(short.LatencySamples) / float64(short.LatencySamples+20)
	return (1-alpha)*longScore + alpha*shortScore
}

// advanceHealth applies expiry boundaries in their original order, even when
// nobody selected or viewed this channel between boundaries. Expiry can lower
// the recovery ceiling, but only a new success or ClearPenalty can raise it.
// Each bucket is visited twice total, with no historical rescans per outcome.
func (ch *channelState) advanceHealth(now time.Time) {
	for {
		shortExpiry, longExpiry := int64(math.MaxInt64), int64(math.MaxInt64)
		if ch.shortHealthCursor < len(ch.healthBuckets) {
			shortExpiry = ch.healthBuckets[ch.shortHealthCursor].second + int64(shortWindow/time.Second) + 1
		}
		if ch.longHealthCursor < len(ch.healthBuckets) {
			longExpiry = ch.healthBuckets[ch.longHealthCursor].second + int64(longWindow/time.Second) + 1
		}
		expiry := min(shortExpiry, longExpiry)
		if expiry > now.Unix() {
			break
		}
		// Short and long expirations at the same second are simultaneous;
		// do not create an artificial baseline between those two removals.
		if shortExpiry == expiry {
			bucket := ch.healthBuckets[ch.shortHealthCursor]
			ch.healthShort.Attempts -= bucket.attempts
			ch.healthShort.Successes -= bucket.successes
			ch.healthShort.Failures -= bucket.attempts - bucket.successes
			ch.shortHealthCursor++
		}
		if longExpiry == expiry {
			bucket := ch.healthBuckets[ch.longHealthCursor]
			ch.healthLong.Attempts -= bucket.attempts
			ch.healthLong.Successes -= bucket.successes
			ch.healthLong.Failures -= bucket.attempts - bucket.successes
			ch.longHealthCursor++
		}
		_, health := successHealth(ch.healthLong, ch.healthShort)
		ch.recoveryLimit = min(ch.recoveryLimit, health)
	}
	if ch.longHealthCursor > 0 && (ch.longHealthCursor >= 256 || ch.longHealthCursor == len(ch.healthBuckets)) {
		retained := copy(ch.healthBuckets, ch.healthBuckets[ch.longHealthCursor:])
		ch.healthBuckets = ch.healthBuckets[:retained]
		ch.shortHealthCursor -= ch.longHealthCursor
		ch.longHealthCursor = 0
	}
}

func (e *Engine) refreshScore(ch *channelState, config Config, now time.Time) {
	if !ch.lastScore.IsZero() && ch.lastScore.Unix() == now.Unix() && ch.scoreVersion == ch.outcomeVersion {
		return
	}
	ch.advanceHealth(now)
	long, short := channelWindows(ch, now, true, false, config.LatencyTargetMS)
	ch.baseSuccess, ch.baseHealth = successHealth(ch.healthLong, ch.healthShort)
	ch.latencyScore[0] = latencyQuality(long, short, config.LatencyTargetMS)
	streamLong, streamShort := channelWindows(ch, now, true, true, config.LatencyTargetMS)
	ch.latencyScore[1] = latencyQuality(streamLong, streamShort, config.LatencyTargetMS)
	ch.lastScore = now
	ch.scoreVersion = ch.outcomeVersion
}

func (e *Engine) evaluate(key Key, s *keyState, candidates []Candidate, now time.Time, stream bool, exclude map[int]bool) []evaluatedCandidate {
	// Register the entire batch first, so members sharing a capacity pool all
	// see the strictest configured capacity before selection or reservation.
	registered := make([]Candidate, 0, len(candidates))
	seen := make(map[int]bool, len(candidates))
	for _, candidate := range candidates {
		if candidate.ID <= 0 || seen[candidate.ID] {
			continue
		}
		seen[candidate.ID] = true
		_, candidate = e.resolveCandidate(key, s, candidate)
		registered = append(registered, candidate)
	}
	result := make([]evaluatedCandidate, 0, len(registered))
	for _, candidate := range registered {
		if exclude[candidate.ID] || !e.eligible(candidate, now) {
			continue
		}
		ch := s.channels[candidate.ID]
		e.refreshScore(ch, s.config, now)
		_, short := channelWindows(ch, now, true, stream, s.config.LatencyTargetMS)
		streamIndex := 0
		if stream {
			streamIndex = 1
		}
		pool := e.pools[candidate.CapacityKey]
		candidate.Capacity = poolCapacity(pool)
		loadCapacity := candidate.Capacity
		if loadCapacity == 0 {
			loadCapacity = 100
		}
		health := max(0.01, min(ch.baseHealth, ch.recoveryLimit))
		// Re-read the short-window error bound at selection time. New errors
		// reduce recoveryLimit synchronously in FinishAttempt as well.
		error5 := float64(short.Failures+1) / float64(short.Attempts+20)
		health = max(0.01, min(health, 1-error5))
		latency := ch.latencyScore[streamIndex]
		weight := latency * health / (1 + float64(pool.inFlight)/float64(loadCapacity))
		shortSuccess := float64(short.Successes+19) / float64(short.Attempts+20)
		result = append(result, evaluatedCandidate{
			candidate: candidate, weight: weight,
			health: health, latency: latency,
			healthy: short.Successes > 0 && ch.baseSuccess >= s.config.SuccessTarget && shortSuccess >= s.config.SuccessTarget,
			mature:  ch.learningSuccesses >= channelRampSuccesses,
		})
	}
	if len(result) == 0 {
		return result
	}
	maxPriority := result[0].candidate.Priority
	for _, item := range result {
		maxPriority = max(maxPriority, item.candidate.Priority)
	}
	staticTotal := 0.0
	staticCount := 0
	for _, item := range result {
		if item.candidate.Priority == maxPriority {
			staticTotal += max(0, item.candidate.Weight)
			staticCount++
		}
	}
	active := s.config.Enabled && s.active
	transition := min(1, max(0.01, float64(s.learningSamples)/100))
	for i := range result {
		item := &result[i]
		staticShare := 0.0
		if item.candidate.Priority == maxPriority {
			if staticTotal > 0 {
				staticShare = max(0, item.candidate.Weight) / staticTotal
			} else {
				staticShare = 1 / float64(staticCount)
			}
		}
		if !active {
			item.weight = staticShare
		} else {
			// Priority defines a tier, never an additive score. The normalized
			// configured ratio fades as 100 real attempt samples accumulate.
			item.weight *= transition + (1-transition)*staticShare*float64(len(result))
		}
	}
	total := 0.0
	for _, item := range result {
		total += item.weight
	}
	if total <= 0 {
		return nil
	}
	top := 0
	for i := range result {
		result[i].probability = result[i].weight / total
	}
	if active {
		matureHealthyWeight := 0.0
		newChannels := 0
		for _, item := range result {
			if item.mature && item.healthy {
				matureHealthyWeight += item.weight
			} else if !item.mature {
				newChannels++
			}
		}
		if newChannels > 0 && matureHealthyWeight > 0 {
			excess := 0.0
			for i := range result {
				item := &result[i]
				if item.mature {
					continue
				}
				// All zero-sample members share a 5% starting budget. Each
				// successful business attempt adds 4.75 percentage points of
				// headroom, with the guard removed after 20 successes. This is
				// an upper bound, never a floor for a failing new channel.
				progress := float64(s.channels[item.candidate.ID].learningSuccesses) / channelRampSuccesses
				limit := channelRampInitialShare/float64(newChannels) + (1-channelRampInitialShare)*progress
				item.rampLimited = true
				if item.probability > limit {
					excess += item.probability - limit
					item.probability = limit
				}
			}
			for i := range result {
				item := &result[i]
				if item.mature && item.healthy {
					item.probability += excess * item.weight / matureHealthyWeight
				}
				// Retries rank the same effective weights with ramp protection.
				item.weight = item.probability * total
			}
		}
	}
	for i := range result {
		if result[i].probability > result[top].probability {
			top = i
		}
	}
	if active && result[top].probability > 0.9 {
		healthyAlternativeWeight := 0.0
		for i, item := range result {
			if i != top && item.healthy && !item.rampLimited {
				healthyAlternativeWeight += item.weight
			}
		}
		if healthyAlternativeWeight > 0 {
			excess := result[top].probability - 0.9
			result[top].probability = 0.9
			for i := range result {
				if i != top && result[i].healthy && !result[i].rampLimited {
					result[i].probability += excess * result[i].weight / healthyAlternativeWeight
				}
			}
		}
	}
	return result
}

// SelectAndReserve reads fresh eligibility and load under the same lock used to
// reserve capacity. Retries rank all remaining candidates by effective weight.
func (e *Engine) SelectAndReserve(r *Request, candidates []Candidate, stream, retry bool) (*Attempt, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if r == nil || r.engine != e || r.ended {
		return nil, ErrInvalidRequest
	}
	now := e.now()
	s := e.state(r.Key, now)
	evaluated := e.evaluate(r.Key, s, candidates, now, stream, r.attempted)
	if len(evaluated) == 0 {
		return nil, ErrNoEligibleChannel
	}
	if retry && s.active && s.config.Enabled {
		slices.SortFunc(evaluated, func(a, b evaluatedCandidate) int {
			if a.weight > b.weight {
				return -1
			}
			if a.weight < b.weight {
				return 1
			}
			return a.candidate.ID - b.candidate.ID
		})
		return e.reserve(r, evaluated[0].candidate, stream, now), nil
	}
	threshold := e.random()
	selected := evaluated[len(evaluated)-1].candidate
	for _, item := range evaluated {
		threshold -= item.probability
		if threshold < 0 {
			selected = item.candidate
			break
		}
	}
	return e.reserve(r, selected, stream, now), nil
}
