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
	// A recovery probe holds its half-open gate until its first useful output
	// or for at most this long, so one slow stream cannot hold a channel out.
	probeWindow = 45 * time.Second
	// maxSeenRequests bounds per-dimension request deduplication state; beyond
	// it only the last seenRequestsBusyWindow of request IDs is kept.
	maxSeenRequests        = 100_000
	seenRequestsBusyWindow = 5 * time.Minute
	// Idle dimensions and unregistered channel state are reclaimed at most this often.
	sweepInterval = time.Minute
)

type dimension struct {
	key       Key
	channelID int
}

// capacityPool caches the strictest positive member capacity, so admission
// never scans the members of a pool shared by many group/model dimensions.
type capacityPool struct {
	inFlight int
	members  map[dimension]int
	limits   map[int]int
	minLimit int
}

func newCapacityPool() *capacityPool {
	return &capacityPool{members: make(map[dimension]int), limits: make(map[int]int)}
}

func (p *capacityPool) setMember(member dimension, limit int) {
	if previous, ok := p.members[member]; ok {
		if previous == limit {
			return
		}
		p.dropLimit(previous)
	}
	p.members[member] = limit
	if limit > 0 {
		p.limits[limit]++
		if p.minLimit == 0 || limit < p.minLimit {
			p.minLimit = limit
		}
	}
}

func (p *capacityPool) removeMember(member dimension) {
	if p == nil {
		return
	}
	previous, ok := p.members[member]
	if !ok {
		return
	}
	delete(p.members, member)
	p.dropLimit(previous)
}

func (p *capacityPool) dropLimit(limit int) {
	if limit <= 0 {
		return
	}
	if p.limits[limit]--; p.limits[limit] > 0 {
		return
	}
	delete(p.limits, limit)
	if limit != p.minLimit {
		return
	}
	p.minLimit = 0
	for value := range p.limits {
		if p.minLimit == 0 || value < p.minLimit {
			p.minLimit = value
		}
	}
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
	registered        bool
	outcomeVersion    uint64
	windowCache       [4]windowCache
	candidate         Candidate
	pool              string
	defaultPool       string
	buckets           map[int64]*secondBucket
	learningBuckets   map[int64]*secondBucket
	dispatches        map[int64]int64
	inFlight          int
	lastScore         time.Time
	scoreVersion      uint64
	baseSuccess       float64
	baseHealth        float64
	latencyScore      [2]float64
	recoveryLimit     float64
	learningSuccesses int64
	learningEpoch     uint64
	learningSince     time.Time
	cooldown          cooldownState
	healthBuckets     []healthBucket
	shortHealthCursor int
	longHealthCursor  int
	healthLong        Window
	healthShort       Window
}

type keyState struct {
	loadPeaks           map[int64]int
	capacitySkips       map[int64]int64
	capacityExhausted   map[int64]int64
	lastCapacityWarning time.Time
	cooldownSuppressed  map[int64]int64
	config              Config
	overrides           map[int]Override
	channels            map[int]*channelState
	requests            map[int64]int64
	seenRequests        map[string]time.Time
	activation          []time.Time
	active              bool
	learningSamples     int64
	lastBusiness        time.Time
	businessInFlight    int
	inFlight            int
	lastPrune           time.Time
	// openRequests counts Requests that have not ended. A dimension with an
	// open request or reservation is never evicted.
	openRequests int
	lastUsed     time.Time
}

func New() *Engine {
	return &Engine{
		now:    time.Now,
		random: rand.Float64,
		states: make(map[Key]*keyState),
		pools:  make(map[string]*capacityPool),

		channelCooldowns: make(map[int]*channelCooldown),
	}
}

func (e *Engine) state(key Key, now time.Time) *keyState {
	s := e.states[key]
	if s == nil {
		s = &keyState{
			config: DefaultConfig(), overrides: make(map[int]Override), loadPeaks: make(map[int64]int),
			capacitySkips: make(map[int64]int64), capacityExhausted: make(map[int64]int64), cooldownSuppressed: make(map[int64]int64),
			channels: make(map[int]*channelState), requests: make(map[int64]int64),
			seenRequests: make(map[string]time.Time),
		}
		e.states[key] = s
	}
	s.lastUsed = now
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
		for second := range s.capacitySkips {
			if second < cutoff {
				delete(s.capacitySkips, second)
			}
		}
		for second := range s.capacityExhausted {
			if second < cutoff {
				delete(s.capacityExhausted, second)
			}
		}
		for second := range s.cooldownSuppressed {
			if second < cutoff {
				delete(s.cooldownSuppressed, second)
			}
		}
		// Deduplication only needs a request's own lifetime. Keep the full
		// window normally, but bound memory on very busy dimensions.
		seenCutoff := now.Add(-longWindow)
		if len(s.seenRequests) > maxSeenRequests {
			seenCutoff = now.Add(-seenRequestsBusyWindow)
		}
		for id, at := range s.seenRequests {
			if at.Before(seenCutoff) {
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
	e.maybeSweep(now)
	e.state(key, now).openRequests++
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
	if s := e.states[r.Key]; s != nil {
		s.openRequests--
		if !r.businessAt.IsZero() {
			s.businessInFlight--
		}
	}
	e.state(r.Key, now)
}

// maybeSweep reclaims state left by request-supplied model names, removed
// memberships and deleted channels. It runs at most once per sweepInterval.
// Dimensions with registered members belong to the published membership and
// stay; other dimensions are evicted after a full statistics window without
// use, open requests or reservations, so late completions never find them gone.
func (e *Engine) maybeSweep(now time.Time) {
	if !e.lastSweep.IsZero() && now.Sub(e.lastSweep) < sweepInterval {
		return
	}
	e.lastSweep = now
	cutoff := now.Add(-longWindow)
	for key, s := range e.states {
		if s.openRequests > 0 || s.inFlight > 0 || s.businessInFlight > 0 {
			continue
		}
		registered := false
		for id, ch := range s.channels {
			if ch.registered {
				registered = true
				continue
			}
			if ch.inFlight > 0 || ch.cooldown.halfOpen || ch.cooldown.endedAt.After(cutoff) || ch.hasSamplesSince(cutoff.Unix()) {
				continue
			}
			delete(s.channels, id)
		}
		if !registered && s.lastUsed.Before(cutoff) {
			delete(e.states, key)
		}
	}
	for name, pool := range e.pools {
		if pool.inFlight == 0 && len(pool.members) == 0 {
			delete(e.pools, name)
		}
	}
	for id, wide := range e.channelCooldowns {
		if wide.halfOpen || wide.probeInFlight || wide.endedAt.After(cutoff) {
			continue
		}
		recent := false
		for _, at := range wide.modelFailures {
			recent = recent || now.Sub(at) <= channelCooldownWindow
		}
		if !recent {
			delete(e.channelCooldowns, id)
		}
	}
}

func (ch *channelState) hasSamplesSince(cutoff int64) bool {
	for second := range ch.buckets {
		if second >= cutoff {
			return true
		}
	}
	for second := range ch.dispatches {
		if second >= cutoff {
			return true
		}
	}
	return false
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

func (config Config) valid() bool {
	return config.SuccessTarget > 0 && config.SuccessTarget <= 1 && isFinite(config.SuccessTarget) &&
		config.LatencyTargetMS > 0 && isFinite(config.LatencyTargetMS) && config.DefaultCapacity >= 0
}

func (e *Engine) SetConfig(key Key, config Config) error {
	if !config.valid() {
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

// Membership contains a complete generation prepared without holding either routing engine's lock.
type Membership struct {
	Candidates map[Key][]Candidate
	Metadata   []Candidate
}

// RoutingConfig applies to one group/model. Channel overrides do not alter its supplier pools.
type RoutingConfig struct {
	Config    Config
	Overrides map[int]Override
}

// PublishRoutingMembership installs channel and supplier routing as a single generation.
// Both engines retain their existing attempts, capacity leases and learning history.
func (e *Engine) PublishRoutingMembership(channels Membership, poolEngine *Engine, pools Membership, settings map[Key]RoutingConfig) error {
	if poolEngine == nil || e == poolEngine {
		return ErrInvalidConfig
	}
	for _, entry := range settings {
		if !entry.Config.valid() {
			return ErrInvalidConfig
		}
		for id, override := range entry.Overrides {
			if err := validateOverride(id, override); err != nil {
				return err
			}
		}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	poolEngine.mu.Lock()
	defer poolEngine.mu.Unlock()
	for _, engine := range []*Engine{e, poolEngine} {
		now := engine.now()
		for key, entry := range settings {
			state := engine.state(key, now)
			for _, channel := range state.channels {
				channel.lastScore = time.Time{}
			}
			state.config = entry.Config
			if engine != e {
				// Supplier pools carry their own capacity (0 = unlimited); a
				// group/model's per-channel default must not cap a whole pool.
				state.config.DefaultCapacity = 0
			}
			clear(state.overrides)
			if engine == e {
				for id, override := range entry.Overrides {
					state.overrides[id] = cloneOverride(override)
				}
			}
		}
	}
	e.replaceCandidateMembership(channels.Candidates, channels.Metadata)
	poolEngine.replaceCandidateMembership(pools.Candidates, pools.Metadata)
	return nil
}

// ReplaceCandidateMembership reconciles an authoritative complete metadata
// snapshot atomically. Never pass a request's filtered candidates here. Removed
// memberships release their limits, while leases retain their original pools.
func (e *Engine) ReplaceCandidateMembership(candidates map[Key][]Candidate, channelMetadata ...Candidate) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.replaceCandidateMembership(candidates, channelMetadata)
}

func (e *Engine) replaceCandidateMembership(candidates map[Key][]Candidate, channelMetadata []Candidate) {
	now := e.now()
	e.maybeSweep(now)
	for key, s := range e.states {
		if _, present := candidates[key]; !present {
			// Unregistering is not a use: an abandoned dimension must still
			// age out while new generations keep being published.
			used := s.lastUsed
			e.replaceCandidates(key, nil, now)
			s.lastUsed = used
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
		e.pools[ch.pool].removeMember(dimension{key: key, channelID: id})
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
	ch := s.channels[candidate.ID]
	if e.membershipManaged {
		if ch == nil || !ch.registered {
			candidate.Excluded = true
			return ch, candidate
		}
		// Request candidates can be stale or filtered. They must not change the
		// authoritative membership, channel status, or shared capacity pool.
		// Publication and configuration changes already registered this member.
		excluded := candidate.Excluded
		candidate = e.admissionCandidate(s, ch, ch.candidate, e.now())
		candidate.Excluded = candidate.Excluded || excluded
		return ch, candidate
	}
	if ch != nil && ch.registered && ch.candidate == candidate {
		return ch, e.admissionCandidate(s, ch, candidate, e.now())
	}
	return e.registerCandidate(key, s, candidate)
}

// admissionCandidate applies dimension settings, cooldown and the shared
// pool's strictest capacity to an already registered member.
func (e *Engine) admissionCandidate(s *keyState, ch *channelState, candidate Candidate, now time.Time) Candidate {
	candidate = e.configuredCandidate(s, ch, candidate, now)
	if pool := e.pools[candidate.CapacityKey]; pool != nil && pool.minLimit > 0 {
		candidate.Capacity = pool.minLimit
	}
	return candidate
}

// pool returns the named capacity pool, creating it for an exceptional pinned
// reservation or after an idle sweep removed an empty pool.
func (e *Engine) pool(name string) *capacityPool {
	pool := e.pools[name]
	if pool == nil {
		pool = newCapacityPool()
		e.pools[name] = pool
	}
	return pool
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
	candidate = e.configuredCandidate(s, ch, candidate, e.now())
	member := dimension{key: key, channelID: candidate.ID}
	if ch.pool != "" && ch.pool != candidate.CapacityKey {
		e.pools[ch.pool].removeMember(member)
	}
	ch.pool = candidate.CapacityKey
	pool := e.pool(ch.pool)
	if candidate.Status == 1 && !candidate.Excluded {
		pool.setMember(member, candidate.Capacity)
	} else {
		pool.removeMember(member)
	}
	if pool.minLimit > 0 {
		candidate.Capacity = pool.minLimit
	}
	return ch, candidate
}

func (e *Engine) configuredCandidate(s *keyState, ch *channelState, candidate Candidate, now time.Time) Candidate {
	candidate.probeBlocked, candidate.wideGuard = false, false
	// Like the channel-wide guard, a dimension's own cooldown or probe lease
	// never removes its last viable route. Alternatives can be disabled, removed
	// or filled after the cooldown started, so this is decided at admission.
	if (ch.cooldown.until.After(now) || ch.cooldown.probeBusy(now)) && e.hasAlternative(s, candidate.ID, now) {
		if ch.cooldown.until.After(candidate.CooldownUntil) {
			candidate.CooldownUntil = ch.cooldown.until
		}
		candidate.probeBlocked = ch.cooldown.probeBusy(now)
	}
	if wide := e.channelCooldowns[candidate.ID]; wide != nil && wide.halfOpen && e.hasAlternative(s, candidate.ID, now) {
		candidate.wideGuard = true
		if wide.until.After(candidate.CooldownUntil) {
			candidate.CooldownUntil = wide.until
		}
		candidate.probeBlocked = candidate.probeBlocked || wide.probeBusy(now)
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
		if ch.defaultPool == "" {
			ch.defaultPool = fmt.Sprintf("channel:%d", candidate.ID)
		}
		candidate.CapacityKey = ch.defaultPool
	}
	return candidate
}

func poolCapacity(pool *capacityPool) int {
	if pool == nil {
		return 0
	}
	return pool.minLimit
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
		candidate = e.configuredCandidate(s, ch, canonical, now)
		e.pool(candidate.CapacityKey)
	} else {
		_, candidate = e.resolveCandidate(r.Key, s, candidate)
	}
	if !e.eligible(candidate, now) {
		if e.available(candidate, now) {
			r.capacityBlocked.Store(true)
		}
		r.countCooldownSkip(s.channels[candidate.ID], candidate, now)
		return nil, ErrNoEligibleChannel
	}
	return e.reserve(r, candidate, stream, now), nil
}

// TakeCapacityBlock consumes a capacity observation from this selection.
func (r *Request) TakeCapacityBlock() bool {
	return r != nil && r.capacityBlocked.Swap(false)
}

// RecordCapacityOutcome records the final routing decision for a dimension
// where capacity blocked a candidate at either the channel or supplier layer.
// It returns true on the first exhaustion in the current minute for logging.
func (e *Engine) RecordCapacityOutcome(key Key, exhausted bool) bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	s := e.state(key, now)
	if exhausted {
		s.capacityExhausted[now.Unix()]++
		minute := now.Truncate(time.Minute)
		if s.lastCapacityWarning != minute {
			s.lastCapacityWarning = minute
			return true
		}
	} else {
		s.capacitySkips[now.Unix()]++
	}
	return false
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
	ch := s.channels[candidate.ID]
	ch.inFlight++
	e.pool(a.pool).inFlight++
	a.cooldownGeneration = ch.cooldown.generation
	// A waived last-viable route may admit traffic while a probe is still
	// busy; only one attempt at a time owns each probe gate.
	if wide := e.channelCooldowns[candidate.ID]; candidate.wideGuard && !wide.probeBusy(now) {
		wide.claimProbe(a, now, &a.wideProbe)
	}
	if ch.cooldown.halfOpen && !ch.cooldown.probeBusy(now) {
		ch.cooldown.claimProbe(a, now, &a.probe)
	}
	return a
}

// available ignores capacity: the candidate could serve if its pool had room.
func (e *Engine) available(candidate Candidate, now time.Time) bool {
	return !candidate.Excluded && candidate.Status == 1 && !candidate.CooldownUntil.After(now) && !candidate.probeBlocked
}

func (e *Engine) eligible(candidate Candidate, now time.Time) bool {
	if !e.available(candidate, now) {
		return false
	}
	pool := e.pool(candidate.CapacityKey)
	capacity := pool.minLimit
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
	a.request.dispatched, a.request.lastPriority = true, a.Candidate.Priority
	if !a.request.businessAt.IsZero() {
		e.states[a.request.Key].channels[a.Candidate.ID].dispatches[a.StartedAt.Unix()]++
	}
}

// RetrySameTier lets the request use again the candidates it already tried in
// the priority tier of its latest attempt, and returns their IDs. The host
// calls it when no untried candidate is eligible: with nowhere else to go,
// the remaining retries stay in that tier instead of ending the request.
// Every candidate of the tier is then tried once more before any is tried a
// third time.
func (e *Engine) RetrySameTier(r *Request, candidates []Candidate) []int {
	e.mu.Lock()
	defer e.mu.Unlock()
	if r == nil || r.engine != e || r.ended || !r.dispatched {
		return nil
	}
	var again []int
	for _, candidate := range candidates {
		if r.attempted[candidate.ID] && candidate.Priority == r.lastPriority {
			delete(r.attempted, candidate.ID)
			again = append(again, candidate.ID)
		}
	}
	return again
}

// RetryAgain lets the request use the given candidates again and returns
// those it had tried.
func (e *Engine) RetryAgain(r *Request, ids []int) []int {
	e.mu.Lock()
	defer e.mu.Unlock()
	if r == nil || r.engine != e || r.ended {
		return nil
	}
	var again []int
	for _, id := range ids {
		if r.attempted[id] {
			delete(r.attempted, id)
			again = append(again, id)
		}
	}
	return again
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
	if pool := e.pools[a.pool]; pool != nil {
		pool.inFlight--
	}
	// An open request keeps its dimension and reserved channel from eviction;
	// these guards only keep an unexpected late completion from panicking.
	s := e.states[r.Key]
	if s == nil {
		return channelObservation{}, false
	}
	ch := s.channels[a.Candidate.ID]
	minute := now.Truncate(time.Minute).Unix()
	s.loadPeaks[minute] = max(s.loadPeaks[minute], s.inFlight)
	s.inFlight--
	if ch == nil {
		return channelObservation{}, false
	}
	ch.inFlight--
	if a.probe {
		// Any end of the probe admits the next one unless it cooled again.
		ch.cooldown.releaseProbe(a)
	}
	if a.wideProbe {
		e.channelCooldowns[a.Candidate.ID].releaseProbe(a)
	}
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
	e.recordCooldownOutcome(a, s, ch, outcome, now)
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
