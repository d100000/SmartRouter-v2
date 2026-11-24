package scheduler

import (
	"slices"
	"time"
)

const (
	cooldownStreakFailures = 3
	// The streak is consecutive: any success resets it. The window only
	// bounds how far apart those failures may be, so a dimension with a
	// request every minute or less can still cool a dead channel.
	cooldownStreakWindow = 10 * time.Minute
	cooldownMaxDuration  = 5 * time.Minute
	// A channel entering cooldown in this many distinct models within
	// channelCooldownWindow cools in every dimension.
	channelCooldownModels = 2
	channelCooldownWindow = time.Minute
	// A preferred session binding yields to a healthy alternative only while
	// its channel shows this many consecutive failed requests, the latest
	// within bindingFailureWindow. One isolated error never moves a session.
	bindingFailureStreak = 2
	bindingFailureWindow = time.Minute
)

// Probe results of a half-open channel after its cooldown expired.
const (
	ProbeRecovered = "recovered"
	ProbeFailed    = "failed"
)

// availabilityCircuit records one outage and owns its single recovery lease.
type availabilityCircuit struct {
	generation                uint64
	level                     int
	startedAt, until, endedAt time.Time
	duration                  time.Duration
	skipped, probes           int64
	probeResult               string
	halfOpen, probeInFlight   bool
	// probeAttempt owns the gate since probeSince. After probeWindow without
	// first output another attempt may take over the probe.
	probeAttempt *Attempt
	probeSince   time.Time
}

// probeBusy reports whether a recovery probe still holds the half-open gate.
func (c *availabilityCircuit) probeBusy(now time.Time) bool {
	return c.probeInFlight && now.Sub(c.probeSince) < probeWindow
}

// claimProbe hands the gate to a new attempt. An expired previous probe
// becomes an ordinary attempt and can no longer resolve this circuit.
func (c *availabilityCircuit) claimProbe(a *Attempt, now time.Time, flag *bool) {
	if previous := c.probeAttempt; previous != nil && previous != a {
		if flag == &a.probe {
			previous.probe = false
		} else {
			previous.wideProbe = false
		}
	}
	c.probeAttempt, c.probeSince, c.probeInFlight = a, now, true
	*flag = true
}

func (c *availabilityCircuit) releaseProbe(a *Attempt) {
	if c.probeAttempt == a {
		c.probeAttempt, c.probeInFlight = nil, false
	}
}

func (c *availabilityCircuit) open(now time.Time) {
	if !c.halfOpen {
		c.startedAt, c.endedAt = now, time.Time{}
		c.level, c.skipped, c.probes, c.probeResult = 0, 0, 0, ""
	}
	c.duration = min(failureCooldown<<min(c.level, 8), cooldownMaxDuration)
	c.level++
	c.generation++
	c.until = now.Add(c.duration)
	c.halfOpen = true
}

func (c *availabilityCircuit) close(now time.Time) {
	c.halfOpen = false
	c.endedAt = now
}

// cooldownState tracks the independent streak for one group, model and channel.
type cooldownState struct {
	availabilityCircuit
	streakSince    time.Time
	streakRequests []string
	lastFailure    time.Time
}

type channelCooldown struct {
	availabilityCircuit
	modelFailures map[string]time.Time
}

// hasAlternative reports whether another member of this group/model could
// serve now. A cooldown never removes the last available channel.
func (e *Engine) hasAlternative(s *keyState, channelID int, now time.Time) bool {
	for id, other := range s.channels {
		if id == channelID || !other.registered {
			continue
		}
		candidate := other.candidate
		if candidate.Excluded || candidate.Status != 1 || candidate.CooldownUntil.After(now) ||
			other.cooldown.until.After(now) || other.cooldown.probeBusy(now) {
			continue
		}
		if wide := e.channelCooldowns[id]; wide != nil && (wide.until.After(now) || wide.probeBusy(now)) {
			continue
		}
		pool := e.pools[other.pool]
		if pool == nil {
			continue
		}
		if capacity := poolCapacity(pool); capacity > 0 && pool.inFlight >= capacity {
			continue
		}
		return true
	}
	return false
}

// startCooldown opens the circuit unless this is the last viable channel.
func (e *Engine) startCooldown(key Key, s *keyState, ch *channelState, channelID int, now time.Time) {
	cooldown := &ch.cooldown
	cooldown.streakRequests = nil
	if !e.hasAlternative(s, channelID, now) {
		s.cooldownSuppressed[now.Unix()]++
		if cooldown.halfOpen {
			// A failed probe is now the last viable route. Let it carry traffic.
			cooldown.close(now)
		}
		return
	}
	cooldown.open(now)
	wide := e.channelCooldowns[channelID]
	if wide == nil {
		wide = &channelCooldown{modelFailures: make(map[string]time.Time)}
		e.channelCooldowns[channelID] = wide
	}
	if !wide.halfOpen {
		wide.modelFailures[key.Model] = now
		for model, at := range wide.modelFailures {
			if now.Sub(at) > channelCooldownWindow {
				delete(wide.modelFailures, model)
			}
		}
		if len(wide.modelFailures) >= channelCooldownModels {
			wide.open(now)
			clear(wide.modelFailures)
		}
	}
}

// ObserveFirstOutput resolves a recovery probe as soon as it delivers useful
// output instead of holding the half-open gate for the whole stream. The
// attempt's final outcome still counts toward health and the failure streak.
func (e *Engine) ObserveFirstOutput(a *Attempt) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if a == nil || a.request == nil || a.request.engine != e || a.finished || !a.started || a.request.businessAt.IsZero() {
		return
	}
	now := e.now()
	if a.wideProbe {
		wide := e.channelCooldowns[a.Candidate.ID]
		wide.releaseProbe(a)
		wide.probes++
		wide.probeResult = ProbeRecovered
		wide.close(now)
		a.wideProbe = false
	}
	s := e.states[a.request.Key]
	if !a.probe || s == nil || s.channels[a.Candidate.ID] == nil {
		return
	}
	cooldown := &s.channels[a.Candidate.ID].cooldown
	cooldown.releaseProbe(a)
	a.probe = false
	if a.cooldownGeneration != cooldown.generation {
		return
	}
	cooldown.probes++
	cooldown.probeResult = ProbeRecovered
	cooldown.lastFailure = time.Time{}
	cooldown.streakRequests = nil
	cooldown.close(now)
}

// recordCooldownOutcome advances the availability guard for a completed
// business attempt. Only a success resets the streak and the backoff level;
// failures that do not indicate unavailability neither advance nor reset it.
func (e *Engine) recordCooldownOutcome(a *Attempt, s *keyState, ch *channelState, outcome Outcome, now time.Time) {
	cooldown := &ch.cooldown
	if a.wideProbe {
		wide := e.channelCooldowns[a.Candidate.ID]
		if outcome.Success {
			wide.probes++
			wide.probeResult = ProbeRecovered
			wide.close(now)
		} else if outcome.CooldownFailure && outcome.ChannelFailure {
			wide.probes++
			wide.probeResult = ProbeFailed
			wide.open(now)
		}
	}
	// Completions of requests admitted before the circuit opened still count
	// as health samples, but cannot change a later outage or its probe.
	if a.cooldownGeneration != cooldown.generation {
		return
	}
	if outcome.Success {
		cooldown.streakRequests = nil
		cooldown.lastFailure = time.Time{}
		if a.probe {
			cooldown.probes++
			cooldown.probeResult = ProbeRecovered
			cooldown.close(now)
		}
		return
	}
	if !outcome.ChannelFailure || !outcome.CooldownFailure {
		return
	}
	cooldown.lastFailure = now
	if a.probe {
		cooldown.probes++
		cooldown.probeResult = ProbeFailed
		e.startCooldown(a.request.Key, s, ch, a.Candidate.ID, now)
		return
	}
	if len(cooldown.streakRequests) == 0 || now.Sub(cooldown.streakSince) > cooldownStreakWindow {
		cooldown.streakSince, cooldown.streakRequests = now, nil
	}
	// Retries of one request cannot cool a channel on their own: a request
	// that fails everywhere must not remove every channel from service.
	if slices.Contains(cooldown.streakRequests, a.request.ID) {
		return
	}
	cooldown.streakRequests = append(cooldown.streakRequests, a.request.ID)
	if len(cooldown.streakRequests) >= cooldownStreakFailures {
		e.startCooldown(a.request.Key, s, ch, a.Candidate.ID, now)
	}
}

// countCooldownSkip records one request bypassing a cooling or probing
// channel, at most once per request and channel.
func (r *Request) countCooldownSkip(ch *channelState, candidate Candidate, now time.Time) {
	if r == nil || ch == nil || !candidate.CooldownUntil.After(now) && !candidate.probeBlocked || r.cooldownSkipped[candidate.ID] {
		return
	}
	if r.cooldownSkipped == nil {
		r.cooldownSkipped = make(map[int]bool)
	}
	r.cooldownSkipped[candidate.ID] = true
	if candidate.wideGuard {
		r.engine.channelCooldowns[candidate.ID].skipped++
	}
	if ch.cooldown.halfOpen {
		ch.cooldown.skipped++
	}
}

// BindingAdvisable reports whether a preferred session should still try its
// bound channel. It yields when that channel is failing consecutive requests
// for availability and another member is healthy and has room.
func (e *Engine) BindingAdvisable(key Key, channelID int) bool {
	return len(e.HealthyBindingAlternatives(key, channelID)) == 0
}

// HealthyBindingAlternatives identifies healthy peers for a recently failing
// binding. The host still applies request constraints and reserves both the
// supplier and channel before bypassing the bound channel.
func (e *Engine) HealthyBindingAlternatives(key Key, channelID int) map[int]bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	s := e.state(key, now)
	ch := s.channels[channelID]
	if ch == nil || len(ch.cooldown.streakRequests) < bindingFailureStreak ||
		ch.cooldown.lastFailure.IsZero() || now.Sub(ch.cooldown.lastFailure) > bindingFailureWindow {
		return nil
	}
	var healthy map[int]bool
	for id, other := range s.channels {
		if id == channelID || !other.registered {
			continue
		}
		if _, ok := e.healthyBindingTarget(s, other, now); ok {
			if healthy == nil {
				healthy = make(map[int]bool)
			}
			healthy[id] = true
		}
	}
	return healthy
}

// healthyBindingTarget reports a member that can take a session now and has
// recent successes at the target rate, with its configured candidate.
func (e *Engine) healthyBindingTarget(s *keyState, ch *channelState, now time.Time) (Candidate, bool) {
	candidate := e.configuredCandidate(s, ch, ch.candidate, now)
	if !e.eligible(candidate, now) {
		return candidate, false
	}
	e.refreshScore(ch, s.config, now)
	_, short := channelWindows(ch, now, true, false, s.config.LatencyTargetMS)
	shortSuccess := float64(short.Successes+19) / float64(short.Attempts+20)
	return candidate, short.Successes > 0 && ch.baseSuccess >= s.config.SuccessTarget && shortSuccess >= s.config.SuccessTarget
}

// HigherPriorityAlternatives reports whether a session's bound channel sits
// below another routable priority tier (demoted) and which members of higher
// tiers are healthy right now. A session that failed over to a lower tier may
// return to them once the host decides its binding has gone idle. The host
// tracks that idle time for every demoted binding, also while no higher tier
// is healthy, so a session in use through an outage is not mistaken for an
// idle one when the higher tier recovers.
// Dimensions with soft priority never ask a session to move for priority.
func (e *Engine) HigherPriorityAlternatives(key Key, channelID int) (healthy map[int]bool, demoted bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	now := e.now()
	s := e.state(key, now)
	ch := s.channels[channelID]
	if ch == nil || !ch.registered || s.config.SoftPriority {
		return nil, false
	}
	bound := ch.candidate.Priority
	for id, other := range s.channels {
		if id == channelID || !other.registered || other.candidate.Excluded || other.candidate.Priority <= bound {
			continue
		}
		demoted = true
		if _, ok := e.healthyBindingTarget(s, other, now); ok {
			if healthy == nil {
				healthy = make(map[int]bool)
			}
			healthy[id] = true
		}
	}
	return healthy, demoted
}
