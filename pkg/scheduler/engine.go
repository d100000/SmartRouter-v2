package scheduler

import (
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"time"
)

const (
	activationThreshold     = 50
	longWindow              = 30 * time.Minute
	shortWindow             = 5 * time.Minute
	channelRampSuccesses    = 20
	channelRampInitialShare = 0.05
	failureCooldown         = 15 * time.Second
)

type dimension struct {
	key       Key
	channelID int
}

type capacityPool struct {
	inFlight int
	members  map[dimension]int
}

type latencyBucket struct {
	count       int64
	sumMS       float64
	histogramMS map[int64]int64
}

type secondBucket struct {
	attempts       int64
	successes      int64
	failures       int64
	streamFailures [2]int64
	latency        [2]latencyBucket
}

type windowCache struct {
	valid    bool
	second   int64
	epoch    int64
	targetMS float64
	version  uint64
	long     Window
	short    Window
}

type healthBucket struct {
	second    int64
	attempts  int64
	successes int64
}

type channelState struct {
	registered         bool
	outcomeVersion     uint64
	windowCache        [4]windowCache
	candidate          Candidate
	pool               string
	buckets            map[int64]*secondBucket
	learningBuckets    map[int64]*secondBucket
	dispatches         map[int64]int64
	inFlight           int
	lastScore          time.Time
	scoreVersion       uint64
	baseSuccess        float64
	baseHealth         float64
	latencyScore       [2]float64
	recoveryLimit      float64
	learningSuccesses  int64
	learningEpoch      uint64
	learningSince      time.Time
	cooldownUntil      time.Time
	failureStreak      int
	failureStreakSince time.Time
	healthBuckets      []healthBucket
	shortHealthCursor  int
	longHealthCursor   int
	healthLong         Window
	healthShort        Window
}

type keyState struct {
	loadPeaks        map[int64]int
	config           Config
	overrides        map[int]Override
	channels         map[int]*channelState
	requests         map[int64]int64
	seenRequests     map[string]time.Time
	activation       []time.Time
	active           bool
	learningSamples  int64
	lastBusiness     time.Time
	businessInFlight int
	inFlight         int
	lastPrune        time.Time
}

func New() *Engine {
	return &Engine{
		now:    time.Now,
		random: rand.Float64,
		states: make(map[Key]*keyState),
		pools:  make(map[string]*capacityPool),
	}
}

func (e *Engine) state(key Key, now time.Time) *keyState {
	s := e.states[key]
	if s == nil {
		s = &keyState{
			config: DefaultConfig(), overrides: make(map[int]Override), loadPeaks: make(map[int64]int),
			channels: make(map[int]*channelState), requests: make(map[int64]int64),
			seenRequests: make(map[string]time.Time),
		}
		e.states[key] = s
	}
	if !s.lastBusiness.IsZero() && now.Sub(s.lastBusiness) >= longWindow && s.businessInFlight == 0 && s.inFlight == 0 {
		s.active = false
		s.learningSamples = 0
		s.activation = nil
		s.lastBusiness = time.Time{}
		for _, ch := range s.channels {
			// Keep the dashboard history. Only the current learning epoch resets.
			ch.learningSince = now
			ch.learningEpoch++
			ch.learningBuckets = make(map[int64]*secondBucket)
			ch.healthBuckets = nil
			ch.shortHealthCursor, ch.longHealthCursor = 0, 0
			ch.healthLong, ch.healthShort = Window{}, Window{}
			ch.learningSuccesses = 0
			ch.lastScore = time.Time{}
			ch.recoveryLimit = 1
		}
	}
	if s.lastPrune.IsZero() || now.Sub(s.lastPrune) >= time.Minute {
		cutoff := now.Add(-longWindow).Unix()
		for minute := range s.loadPeaks {
			if minute < cutoff {
				delete(s.loadPeaks, minute)
			}
		}
		for second := range s.requests {
			if second < cutoff {
				delete(s.requests, second)
			}
		}
		for id, at := range s.seenRequests {
			if at.Before(now.Add(-longWindow)) {
				delete(s.seenRequests, id)
			}
		}
		for _, ch := range s.channels {
			for second := range ch.buckets {
				if second < cutoff {
					delete(ch.buckets, second)
				}
			}
			for second := range ch.learningBuckets {
				if second < cutoff {
					delete(ch.learningBuckets, second)
				}
			}
			for second := range ch.dispatches {
				if second < cutoff {
					delete(ch.dispatches, second)
				}
			}
		}
		s.lastPrune = now
	}
	cutoff := now.Add(-longWindow)
	for len(s.activation) > 0 && s.activation[0].Before(cutoff) {
		s.activation = s.activation[1:]
	}
	return s
}

func (e *Engine) BeginRequest(key Key, id string, probe bool) *Request {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	e.state(key, now)
	if id == "" {
		e.sequence++
		id = fmt.Sprintf("scheduler-%d", e.sequence)
	}
	r := &Request{Key: key, ID: id, engine: e, attempted: make(map[int]bool), attempts: make(map[*Attempt]bool)}
	if !probe {
		e.confirmRequest(r, now)
	}
	return r
}

// TouchRequest marks the start of a real business request without yet counting
// it toward activation. The host confirms only success or a channel failure;
// client mistakes and cancellations can then stay outside effective traffic.
func (e *Engine) TouchRequest(r *Request) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if r == nil || r.engine != e || r.ended || !r.businessAt.IsZero() {
		return
	}
	e.touchRequest(r, e.now())
}

func (e *Engine) touchRequest(r *Request, now time.Time) {
	if !r.businessAt.IsZero() {
		return
	}
	s := e.state(r.Key, now)
	r.businessAt = now
	s.businessInFlight++
	// A duplicate tracking handle is not a new original business request.
	if _, duplicate := s.seenRequests[r.ID]; !duplicate {
		s.lastBusiness = now
	}
}

// ConfirmRequest counts one effective original request using its business-start
// timestamp. It is safe to call again after a successful retry.
func (e *Engine) ConfirmRequest(r *Request) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if r == nil || r.engine != e || r.ended || r.confirmed {
		return
	}
	e.confirmRequest(r, e.now())
}

func (e *Engine) confirmRequest(r *Request, now time.Time) {
	e.touchRequest(r, now)
	s := e.state(r.Key, now)
	r.confirmed = true
	if _, duplicate := s.seenRequests[r.ID]; duplicate {
		return
	}
	at := r.businessAt
	s.seenRequests[r.ID] = at
	s.requests[at.Unix()]++
	if at.Before(now.Add(-longWindow)) {
		return
	}
	s.activation = append(s.activation, at)
	// Completions can arrive out of order; retain the 50 most recent starts.
	slices.SortFunc(s.activation, func(a, b time.Time) int { return a.Compare(b) })
	if len(s.activation) > activationThreshold {
		s.activation = s.activation[len(s.activation)-activationThreshold:]
	}
	if len(s.activation) >= activationThreshold {
		s.active = true
	}
}

func (e *Engine) EndRequest(r *Request) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if r == nil || r.engine != e || r.ended {
		return
	}
	now := e.now()
	for a := range r.attempts {
		e.finishAttempt(a, Outcome{}, now)
	}
	r.ended = true
	if !r.businessAt.IsZero() {
		e.states[r.Key].businessInFlight--
	}
	e.state(r.Key, now)
}

func (e *Engine) IsActive(key Key) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.state(key, e.now())
	return s.config.Enabled && s.active
}

func (e *Engine) Keys() []Key {
	e.mu.Lock()
	defer e.mu.Unlock()
	keys := make([]Key, 0, len(e.states))
	for key := range e.states {
		keys = append(keys, key)
	}
	slices.SortFunc(keys, func(a, b Key) int {
		if a.Group < b.Group {
			return -1
		}
		if a.Group > b.Group {
			return 1
		}
		if a.Model < b.Model {
			return -1
		}
		if a.Model > b.Model {
			return 1
		}
		return 0
	})
	return keys
}

// RecentBusinessKeys returns dimensions with business requests started in the
// last 30 minutes, newest first. Reading this list never refreshes scores or
// resets learning. Metadata-only dimensions and untouched probes are excluded.
func (e *Engine) RecentBusinessKeys() []Key {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	keys := make([]Key, 0, len(e.states))
	for key, state := range e.states {
		if !state.lastBusiness.IsZero() && now.Sub(state.lastBusiness) < longWindow {
			keys = append(keys, key)
		}
	}
	slices.SortFunc(keys, func(a, b Key) int {
		if recency := e.states[b].lastBusiness.Compare(e.states[a].lastBusiness); recency != 0 {
			return recency
		}
		if a.Group < b.Group {
			return -1
		}
		if a.Group > b.Group {
			return 1
		}
		if a.Model < b.Model {
			return -1
		}
		if a.Model > b.Model {
			return 1
		}
		return 0
	})
	return keys
}

func (e *Engine) GetConfig(key Key) Config {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.state(key, e.now()).config
}

func (e *Engine) SetConfig(key Key, config Config) error {
	if config.SuccessTarget <= 0 || config.SuccessTarget > 1 || !isFinite(config.SuccessTarget) ||
		config.LatencyTargetMS <= 0 || !isFinite(config.LatencyTargetMS) || config.DefaultCapacity < 0 {
		return ErrInvalidConfig
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.state(key, e.now())
	s.config = config
	for _, ch := range s.channels {
		ch.lastScore = time.Time{}
		if ch.registered {
			e.registerCandidate(key, s, ch.candidate)
		}
	}
	return nil
}

func (e *Engine) SetOverride(key Key, channelID int, override Override) error {
	if err := validateOverride(channelID, override); err != nil {
		return err
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.state(key, e.now())
	s.overrides[channelID] = cloneOverride(override)
	if ch := s.channels[channelID]; ch != nil && ch.registered {
		e.registerCandidate(key, s, ch.candidate)
	}
	return nil
}

// ReplaceOverrides atomically replaces all dimension-specific overrides,
// including removal of previously configured overrides.
func (e *Engine) ReplaceOverrides(key Key, overrides map[int]Override) error {
	for id, override := range overrides {
		if err := validateOverride(id, override); err != nil {
			return err
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.state(key, e.now())
	s.overrides = make(map[int]Override, len(overrides))
	for id, override := range overrides {
		s.overrides[id] = cloneOverride(override)
	}
	for _, ch := range s.channels {
		if ch.registered {
			e.registerCandidate(key, s, ch.candidate)
		}
	}
	return nil
}

// RegisterCandidates replaces the metadata membership for one group/model.
// It neither scores nor selects channels. Register all configured dimensions
// before routing so a shared pool honors limits even for idle groups.
// A nil list removes membership while retaining metrics and outstanding leases.
func (e *Engine) RegisterCandidates(key Key, candidates []Candidate) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.replaceCandidates(key, candidates, e.now())
}

// ReplaceCandidateMembership reconciles an authoritative complete metadata
// snapshot atomically. Never pass a request's filtered candidates here. Removed
// memberships release their limits, while leases retain their original pools.
func (e *Engine) ReplaceCandidateMembership(candidates map[Key][]Candidate, channelMetadata ...Candidate) {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	for key := range e.states {
		if _, present := candidates[key]; !present {
			e.replaceCandidates(key, nil, now)
		}
	}
	for key, members := range candidates {
		e.replaceCandidates(key, members, now)
	}
	e.channelMetadata = make(map[int]Candidate)
	for _, members := range candidates {
		for _, candidate := range members {
			if candidate.ID > 0 {
				e.channelMetadata[candidate.ID] = candidate
			}
		}
	}
	// Channels with no current model/group ability may still be pinned by an
	// original task. Their metadata establishes status without a pool member.
	for _, candidate := range channelMetadata {
		if candidate.ID > 0 {
			e.channelMetadata[candidate.ID] = candidate
		}
	}
	e.membershipManaged = true
}

func (e *Engine) replaceCandidates(key Key, candidates []Candidate, now time.Time) {
	s := e.state(key, now)
	present := make(map[int]bool, len(candidates))
	for _, candidate := range candidates {
		if candidate.ID > 0 {
			present[candidate.ID] = true
		}
	}
	for id, ch := range s.channels {
		if !ch.registered || present[id] {
			continue
		}
		delete(e.pools[ch.pool].members, dimension{key: key, channelID: id})
		ch.pool = ""
		ch.registered = false
		// A returning member must establish fresh channel-level confidence.
		// Keep historical outcomes, any recovery ceiling, and fault cooldown.
		ch.learningSince = now
		ch.learningEpoch++
		ch.learningBuckets = make(map[int64]*secondBucket)
		ch.healthBuckets = nil
		ch.shortHealthCursor, ch.longHealthCursor = 0, 0
		ch.healthLong, ch.healthShort = Window{}, Window{}
		ch.learningSuccesses = 0
		ch.lastScore = time.Time{}
	}
	for _, candidate := range candidates {
		if candidate.ID > 0 {
			e.registerCandidate(key, s, candidate)
		}
	}
}

func cloneOverride(o Override) Override {
	if o.Weight != nil {
		value := *o.Weight
		o.Weight = &value
	}
	if o.Capacity != nil {
		value := *o.Capacity
		o.Capacity = &value
	}
	return o
}

func validateOverride(channelID int, override Override) error {
	if channelID <= 0 || (override.Weight != nil && (*override.Weight < 0 || !isFinite(*override.Weight))) ||
		(override.Capacity != nil && *override.Capacity < 0) {
		return ErrInvalidConfig
	}
	return nil
}

func isFinite(value float64) bool { return !math.IsNaN(value) && !math.IsInf(value, 0) }

func (e *Engine) resolveCandidate(key Key, s *keyState, candidate Candidate) (*channelState, Candidate) {
	if e.membershipManaged {
		ch := s.channels[candidate.ID]
		if ch == nil || !ch.registered {
			candidate.Excluded = true
			return ch, candidate
		}
		// Request candidates can be stale or filtered. They must not change the
		// authoritative membership, channel status, or shared capacity pool.
		excluded := candidate.Excluded
		candidate = ch.candidate
		ch, candidate = e.registerCandidate(key, s, candidate)
		candidate.Excluded = candidate.Excluded || excluded
		return ch, candidate
	}
	return e.registerCandidate(key, s, candidate)
}

func (e *Engine) registerCandidate(key Key, s *keyState, candidate Candidate) (*channelState, Candidate) {
	ch := s.channels[candidate.ID]
	if ch == nil {
		ch = &channelState{buckets: make(map[int64]*secondBucket), learningBuckets: make(map[int64]*secondBucket), dispatches: make(map[int64]int64), recoveryLimit: 1}
		s.channels[candidate.ID] = ch
	} else if !ch.registered || (ch.candidate.Status == 1 && !ch.candidate.Excluded) != (candidate.Status == 1 && !candidate.Excluded) {
		// Re-enabling an unavailable member requires fresh ramp evidence, while
		// its observed health history and any fault cooldown remain intact.
		ch.learningSuccesses = 0
		ch.learningEpoch++
	}
	ch.registered = true
	ch.candidate = candidate
	candidate = configuredCandidate(s, ch, candidate)
	member := dimension{key: key, channelID: candidate.ID}
	if ch.pool != "" && ch.pool != candidate.CapacityKey {
		delete(e.pools[ch.pool].members, member)
	}
	ch.pool = candidate.CapacityKey
	pool := e.pools[ch.pool]
	if pool == nil {
		pool = &capacityPool{members: make(map[dimension]int)}
		e.pools[ch.pool] = pool
	}
	if candidate.Status == 1 && !candidate.Excluded {
		pool.members[member] = candidate.Capacity
	} else {
		delete(pool.members, member)
	}
	if capacity := poolCapacity(pool); capacity > 0 {
		candidate.Capacity = capacity
	}
	return ch, candidate
}

func configuredCandidate(s *keyState, ch *channelState, candidate Candidate) Candidate {
	if ch.cooldownUntil.After(candidate.CooldownUntil) {
		candidate.CooldownUntil = ch.cooldownUntil
	}
	if candidate.Capacity <= 0 {
		candidate.Capacity = s.config.DefaultCapacity
	}
	if override, ok := s.overrides[candidate.ID]; ok {
		if override.Weight != nil {
			candidate.Weight = *override.Weight
		}
		if override.Capacity != nil {
			candidate.Capacity = *override.Capacity
		}
		if override.CapacityKey != "" {
			candidate.CapacityKey = override.CapacityKey
		}
	}
	if candidate.CapacityKey == "" {
		candidate.CapacityKey = fmt.Sprintf("channel:%d", candidate.ID)
	}
	return candidate
}

func poolCapacity(pool *capacityPool) int {
	capacity := 0
	for _, limit := range pool.members {
		if limit > 0 && (capacity == 0 || limit < capacity) {
			capacity = limit
		}
	}
	return capacity
}

// ReserveCandidate preserves externally selected fixed/static routing. Unlike
// SelectAndReserve, it permits retrying a pinned channel when the host permits it.
func (e *Engine) ReserveCandidate(r *Request, candidate Candidate, stream bool) (*Attempt, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if r == nil || r.engine != e || r.ended {
		return nil, ErrInvalidRequest
	}
	now := e.now()
	s := e.state(r.Key, now)
	ch := s.channels[candidate.ID]
	if e.membershipManaged && (ch == nil || !ch.registered || ch.candidate.Excluded) {
		// Native fixed/origin-task routing can pin a channel outside its
		// current model/group ability. Honor the latest channel status while
		// avoiding a phantom capacity member from that exceptional request.
		canonical, exists := e.channelMetadata[candidate.ID]
		if !exists {
			return nil, ErrNoEligibleChannel
		}
		canonical.Excluded = candidate.Excluded
		if ch == nil {
			ch = &channelState{buckets: make(map[int64]*secondBucket), learningBuckets: make(map[int64]*secondBucket), dispatches: make(map[int64]int64), recoveryLimit: 1}
			s.channels[candidate.ID] = ch
		}
		candidate = configuredCandidate(s, ch, canonical)
		if e.pools[candidate.CapacityKey] == nil {
			e.pools[candidate.CapacityKey] = &capacityPool{members: make(map[dimension]int)}
		}
	} else {
		_, candidate = e.resolveCandidate(r.Key, s, candidate)
	}
	if !e.eligible(candidate, now) {
		return nil, ErrNoEligibleChannel
	}
	return e.reserve(r, candidate, stream, now), nil
}

func (e *Engine) reserve(r *Request, candidate Candidate, stream bool, now time.Time) *Attempt {
	a := &Attempt{Candidate: candidate, StartedAt: now, request: r, pool: candidate.CapacityKey, stream: stream,
		learningEpoch: e.states[r.Key].channels[candidate.ID].learningEpoch}
	r.attempts[a] = true
	// A reservation may fail local validation. Mark a channel attempted only
	// when StartAttempt actually dispatches it upstream.
	s := e.states[r.Key]
	s.inFlight++
	minute := now.Truncate(time.Minute).Unix()
	s.loadPeaks[minute] = max(s.loadPeaks[minute], s.inFlight)
	s.channels[candidate.ID].inFlight++
	e.pools[a.pool].inFlight++
	return a
}

func (e *Engine) eligible(candidate Candidate, now time.Time) bool {
	if candidate.Excluded || candidate.Status != 1 || candidate.CooldownUntil.After(now) {
		return false
	}
	pool := e.pools[candidate.CapacityKey]
	capacity := poolCapacity(pool)
	if candidate.Capacity > 0 && (capacity == 0 || candidate.Capacity < capacity) {
		capacity = candidate.Capacity
	}
	return capacity == 0 || pool.inFlight < capacity
}

func (e *Engine) StartAttempt(a *Attempt, stream ...bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if a == nil || a.request == nil || a.request.engine != e || a.finished || a.started {
		return
	}
	if len(stream) > 0 {
		a.stream = stream[0]
	}
	a.started = true
	a.StartedAt = e.now()
	a.request.attempted[a.Candidate.ID] = true
	if !a.request.businessAt.IsZero() {
		e.states[a.request.Key].channels[a.Candidate.ID].dispatches[a.StartedAt.Unix()]++
	}
}

func (e *Engine) FinishAttempt(a *Attempt, outcome Outcome) {
	e.mu.Lock()
	if a == nil || a.request == nil || a.request.engine != e {
		e.mu.Unlock()
		return
	}
	observation, accepted := e.finishAttempt(a, outcome, e.now())
	e.mu.Unlock()
	if accepted {
		e.recordChannelObservation(observation)
	}
}

func (e *Engine) finishAttempt(a *Attempt, outcome Outcome, now time.Time) (channelObservation, bool) {
	if a.finished {
		return channelObservation{}, false
	}
	a.finished = true
	r := a.request
	delete(r.attempts, a)
	s := e.states[r.Key]
	ch := s.channels[a.Candidate.ID]
	minute := now.Truncate(time.Minute).Unix()
	s.loadPeaks[minute] = max(s.loadPeaks[minute], s.inFlight)
	s.inFlight--
	ch.inFlight--
	e.pools[a.pool].inFlight--
	if !a.started || r.businessAt.IsZero() || (!outcome.Success && !outcome.ChannelFailure) {
		return channelObservation{}, false
	}
	second := now.Unix()
	observation := channelObservation{channelID: a.Candidate.ID, second: second, success: outcome.Success, stream: a.stream}
	if outcome.Success && outcome.TTFT != nil && *outcome.TTFT > 0 {
		observation.latencyMS = float64(*outcome.TTFT) / float64(time.Millisecond)
	}
	bucket := ch.buckets[second]
	if bucket == nil {
		bucket = &secondBucket{}
		ch.buckets[second] = bucket
	}
	ch.outcomeVersion++
	streamIndex := 0
	if a.stream {
		streamIndex = 1
	}
	bucket.recordOutcome(outcome, streamIndex)
	if outcome.Success || !outcome.CooldownFailure {
		ch.failureStreak = 0
	} else {
		if ch.failureStreak == 0 || now.Sub(ch.failureStreakSince) > time.Minute {
			ch.failureStreak, ch.failureStreakSince = 0, now
		}
		ch.failureStreak++
		if ch.failureStreak >= 3 {
			ch.cooldownUntil = now.Add(failureCooldown)
			ch.failureStreak = 0
		}
	}
	// Old leases still release normally and retain history, but cannot establish
	// confidence for a new membership/learning epoch after metadata changed.
	if a.learningEpoch != ch.learningEpoch {
		return observation, true
	}
	learning := ch.learningBuckets[second]
	if learning == nil {
		learning = &secondBucket{}
		ch.learningBuckets[second] = learning
	}
	learning.recordOutcome(outcome, streamIndex)
	// Within a second, advance already-computed aggregates in constant time.
	// Otherwise the next lookup rebuilds once for window expiry; completions
	// and dashboard reads do not repeatedly rescan all 30 minutes of samples.
	for i := range ch.windowCache {
		cache := &ch.windowCache[i]
		if !cache.valid || cache.second != second || cache.epoch != ch.learningSince.UnixNano() || cache.version+1 != ch.outcomeVersion {
			continue
		}
		cache.long.recordOutcome(outcome, i%2 == streamIndex, cache.targetMS)
		cache.short.recordOutcome(outcome, i%2 == streamIndex, cache.targetMS)
		cache.version = ch.outcomeVersion
	}
	s.learningSamples++
	ch.advanceHealth(now)
	if len(ch.healthBuckets) == 0 || ch.healthBuckets[len(ch.healthBuckets)-1].second != second {
		ch.healthBuckets = append(ch.healthBuckets, healthBucket{second: second})
	}
	healthBucket := &ch.healthBuckets[len(ch.healthBuckets)-1]
	healthBucket.attempts++
	if outcome.Success {
		healthBucket.successes++
	}
	ch.healthLong.recordOutcome(outcome, false, 0)
	ch.healthShort.recordOutcome(outcome, false, 0)
	_, health := successHealth(ch.healthLong, ch.healthShort)
	if outcome.Success {
		ch.learningSuccesses++
		// Consume each success once, at completion. Reads and score refreshes
		// cannot discard, duplicate, or defer recovery credit until after a failure.
		ch.recoveryLimit = min(health, ch.recoveryLimit+0.02)
	} else {
		// Errors impose an immediate ceiling from current samples, never a
		// multiplicative discount on an already discounted previous weight.
		ch.recoveryLimit = min(ch.recoveryLimit, health)
	}
	return observation, true
}

func (window *Window) recordOutcome(outcome Outcome, matchingTransport bool, targetMS float64) {
	window.Attempts++
	if !outcome.Success {
		window.Failures++
		if matchingTransport {
			window.HealthSamples++
		}
		return
	}
	window.Successes++
	if !matchingTransport || outcome.TTFT == nil || *outcome.TTFT <= 0 {
		return
	}
	latencyMS := float64(*outcome.TTFT) / float64(time.Millisecond)
	window.LatencySamples++
	window.LatencySumMS += latencyMS
	window.HealthSamples++
	if math.Ceil(latencyMS) <= targetMS {
		window.HealthySamples++
	}
}

func (bucket *secondBucket) recordOutcome(outcome Outcome, streamIndex int) {
	bucket.attempts++
	if !outcome.Success {
		bucket.failures++
		bucket.streamFailures[streamIndex]++
		return
	}
	bucket.successes++
	if outcome.TTFT == nil || *outcome.TTFT <= 0 {
		return
	}
	latency := &bucket.latency[streamIndex]
	latency.count++
	latency.sumMS += float64(*outcome.TTFT) / float64(time.Millisecond)
	if latency.histogramMS == nil {
		latency.histogramMS = make(map[int64]int64)
	}
	latency.histogramMS[int64(math.Ceil(float64(*outcome.TTFT)/float64(time.Millisecond)))]++
}

// ClearPenalty removes the gradual-recovery ceiling and recalculates from the
// current windows. It does not erase failures, enable a channel, or end cooldown.
func (e *Engine) ClearPenalty(key Key, channelID int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	s := e.state(key, e.now())
	if ch := s.channels[channelID]; ch != nil {
		ch.advanceHealth(e.now())
		ch.recoveryLimit = 1
		ch.lastScore = time.Time{}
	}
}
