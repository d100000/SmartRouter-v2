package scheduler

import (
	"fmt"
	"math"
	"math/rand/v2"
	"slices"
	"time"
)

const (
	activationThreshold = 50
	longWindow          = 30 * time.Minute
	shortWindow         = 5 * time.Minute
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

type channelState struct {
	registered      bool
	outcomeVersion  uint64
	windowCache     [4]windowCache
	candidate       Candidate
	pool            string
	buckets         map[int64]*secondBucket
	learningBuckets map[int64]*secondBucket
	dispatches      map[int64]int64
	inFlight        int
	lastScore       time.Time
	baseSuccess     float64
	baseHealth      float64
	latencyScore    [2]float64
	recoveryLimit   float64
	successSerial   int64
	recoverySerial  int64
	learningSince   time.Time
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
			ch.learningBuckets = make(map[int64]*secondBucket)
			ch.lastScore = time.Time{}
			ch.recoveryLimit = 1
			ch.recoverySerial = ch.successSerial
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
	s := e.state(key, e.now())
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

func (e *Engine) registerCandidate(key Key, s *keyState, candidate Candidate) (*channelState, Candidate) {
	ch := s.channels[candidate.ID]
	if ch == nil {
		ch = &channelState{buckets: make(map[int64]*secondBucket), learningBuckets: make(map[int64]*secondBucket), dispatches: make(map[int64]int64), recoveryLimit: 1}
		s.channels[candidate.ID] = ch
	}
	ch.registered = true
	ch.candidate = candidate
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
	pool.members[member] = candidate.Capacity
	candidate.Capacity = poolCapacity(pool)
	return ch, candidate
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
	_, candidate = e.registerCandidate(r.Key, s, candidate)
	if !e.eligible(candidate, now) {
		return nil, ErrNoEligibleChannel
	}
	return e.reserve(r, candidate, stream, now), nil
}

func (e *Engine) reserve(r *Request, candidate Candidate, stream bool, now time.Time) *Attempt {
	a := &Attempt{Candidate: candidate, StartedAt: now, request: r, pool: candidate.CapacityKey, stream: stream}
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
	e.states[a.request.Key].channels[a.Candidate.ID].dispatches[a.StartedAt.Unix()]++
}

func (e *Engine) FinishAttempt(a *Attempt, outcome Outcome) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if a == nil || a.request == nil || a.request.engine != e {
		return
	}
	e.finishAttempt(a, outcome, e.now())
}

func (e *Engine) finishAttempt(a *Attempt, outcome Outcome, now time.Time) {
	if a.finished {
		return
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
	if !a.started || (!outcome.Success && !outcome.ChannelFailure) {
		return
	}
	second := now.Unix()
	bucket := ch.buckets[second]
	learning := ch.learningBuckets[second]
	if bucket == nil {
		bucket = &secondBucket{}
		ch.buckets[second] = bucket
		learning = bucket
		ch.learningBuckets[second] = learning
	} else if learning == nil {
		// A reset can share a second with the last completed old attempt.
		// Retain its history while giving new learning a genuinely empty epoch.
		learning = &secondBucket{}
		ch.learningBuckets[second] = learning
	}
	ch.outcomeVersion++
	s.learningSamples++
	streamIndex := 0
	if a.stream {
		streamIndex = 1
	}
	bucket.recordOutcome(outcome, streamIndex)
	if learning != bucket {
		learning.recordOutcome(outcome, streamIndex)
	}
	if outcome.Success {
		ch.successSerial++
	} else {
		// Errors impose an immediate ceiling from current samples, never a
		// multiplicative discount on an already discounted previous weight.
		long, short := channelWindows(ch, now, true, a.stream, s.config.LatencyTargetMS)
		_, health := successHealth(long, short)
		ch.recoveryLimit = min(ch.recoveryLimit, health)
		ch.recoverySerial = ch.successSerial
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
		ch.recoveryLimit = 1
		ch.recoverySerial = ch.successSerial
		ch.lastScore = time.Time{}
	}
}
