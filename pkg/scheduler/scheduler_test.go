package scheduler

import (
	"fmt"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

type schedulerFixture struct {
	engine    *Engine
	now       time.Time
	key       Key
	candidate Candidate
}

func newSchedulerFixture() *schedulerFixture {
	f := &schedulerFixture{engine: New(), now: time.Date(2026, 9, 23, 12, 0, 0, 0, time.UTC),
		key:       Key{Group: "default", Model: "example-model"},
		candidate: Candidate{ID: 1, Name: "Primary", Status: 1, Weight: 10, Capacity: 100}}
	f.engine.now = func() time.Time { return f.now }
	f.engine.random = func() float64 { return 0 }
	return f
}

func (f *schedulerFixture) dispatch(t *testing.T, candidate Candidate, stream bool, outcome Outcome) {
	t.Helper()
	r := f.engine.BeginRequest(f.key, "", false)
	a, err := f.engine.ReserveCandidate(r, candidate, stream)
	require.NoError(t, err)
	f.engine.StartAttempt(a)
	f.engine.FinishAttempt(a, outcome)
	f.engine.EndRequest(r)
}

func latency(value time.Duration) *time.Duration { return &value }

func TestActivationOriginalRequestLifecycleAndIdleReset(t *testing.T) {
	f := newSchedulerFixture()
	probe := f.engine.BeginRequest(f.key, "probe", true)
	a, err := f.engine.ReserveCandidate(probe, f.candidate, true)
	require.NoError(t, err)
	f.engine.StartAttempt(a)
	f.engine.FinishAttempt(a, Outcome{Success: true})
	f.engine.EndRequest(probe)
	assert.Zero(t, f.engine.Snapshot(f.key, []Candidate{f.candidate}, true).Summary.Requests30m)

	invalid := f.engine.BeginRequest(f.key, "user-error", true)
	f.engine.TouchRequest(invalid)
	f.engine.EndRequest(invalid)
	assert.Zero(t, f.engine.Snapshot(f.key, []Candidate{f.candidate}, true).Summary.Requests30m, "client errors do not count as effective original requests")

	for i := range 49 {
		r := f.engine.BeginRequest(f.key, fmt.Sprintf("request-%d", i), true)
		f.engine.ConfirmRequest(r)
		f.engine.ConfirmRequest(r)
		f.engine.EndRequest(r)
	}
	assert.False(t, f.engine.IsActive(f.key))
	duplicate := f.engine.BeginRequest(f.key, "request-0", false)
	f.engine.EndRequest(duplicate)
	assert.False(t, f.engine.IsActive(f.key), "the same original request cannot count twice")
	last := f.engine.BeginRequest(f.key, "request-49", false)
	f.engine.EndRequest(last)
	assert.True(t, f.engine.IsActive(f.key))
	assert.False(t, f.engine.IsActive(Key{Group: "other", Model: f.key.Model}))
	assert.False(t, f.engine.IsActive(Key{Group: f.key.Group, Model: "other"}))

	f.now = f.now.Add(20 * time.Minute)
	keepAlive := f.engine.BeginRequest(f.key, "later", false)
	f.engine.EndRequest(keepAlive)
	f.now = f.now.Add(20 * time.Minute)
	assert.True(t, f.engine.IsActive(f.key), "falling below 50 rolling requests must not deactivate")
	assert.Equal(t, 1, f.engine.Snapshot(f.key, []Candidate{f.candidate}, true).ActivationRequests)

	pending := f.engine.BeginRequest(f.key, "long-stream", false)
	reservation, err := f.engine.ReserveCandidate(pending, f.candidate, true)
	require.NoError(t, err)
	f.engine.StartAttempt(reservation)
	f.now = f.now.Add(31 * time.Minute)
	assert.True(t, f.engine.IsActive(f.key), "a running stream prevents idle reset")
	f.engine.FinishAttempt(reservation, Outcome{Success: true, TTFT: latency(time.Second)})
	f.engine.EndRequest(pending)
	assert.False(t, f.engine.IsActive(f.key))
	snapshot := f.engine.Snapshot(f.key, []Candidate{f.candidate}, true)
	assert.Zero(t, snapshot.ActivationRequests)
	assert.EqualValues(t, 1, snapshot.Summary.Attempts30m, "reset preserves observed history")
	assert.Equal(t, "cold", snapshot.Channels[0].RouteState)
	assert.InDelta(t, 0.5, snapshot.Channels[0].QualityScore/snapshot.Channels[0].HealthScore, 1e-9,
		"the old attempt completing in the same second as idle reset is not new learning")
	f.dispatch(t, f.candidate, true, Outcome{Success: true, TTFT: latency(6 * time.Second)})
	f.now = f.now.Add(time.Minute)
	fresh := f.engine.Snapshot(f.key, []Candidate{f.candidate}, true).Channels[0]
	assert.InDelta(t, 1.0/3, fresh.QualityScore/fresh.HealthScore, 1e-9,
		"a new attempt in the reset second contributes to only the new learning epoch")
	assert.EqualValues(t, 2, fresh.Window30m.Attempts)

}

func TestAttemptAccountingMissingLatencyAndTransportSeparation(t *testing.T) {
	f := newSchedulerFixture()
	second := Candidate{ID: 2, Name: "Fallback", Status: 1, Weight: 10, Capacity: 100}
	r := f.engine.BeginRequest(f.key, "logical-request", true)
	firstAttempt, err := f.engine.ReserveCandidate(r, f.candidate, true)
	require.NoError(t, err)
	f.engine.ConfirmRequest(r)
	f.engine.StartAttempt(firstAttempt)
	f.engine.FinishAttempt(firstAttempt, Outcome{ChannelFailure: true})
	secondAttempt, err := f.engine.ReserveCandidate(r, second, false)
	require.NoError(t, err)
	f.engine.StartAttempt(secondAttempt, true)
	f.engine.FinishAttempt(secondAttempt, Outcome{Success: true, TTFT: latency(2 * time.Second)})
	f.engine.FinishAttempt(secondAttempt, Outcome{Success: true, TTFT: latency(time.Millisecond)})
	f.engine.EndRequest(r)
	f.engine.EndRequest(r)

	f.dispatch(t, second, false, Outcome{Success: true, TTFT: latency(10 * time.Second)})
	f.dispatch(t, second, true, Outcome{Success: true})
	f.dispatch(t, second, true, Outcome{}) // client cancellation/user error is neutral
	provisional := f.engine.BeginRequest(f.key, "invalid-local-request", true)
	_, err = f.engine.ReserveCandidate(provisional, second, true)
	require.NoError(t, err)
	f.engine.EndRequest(provisional) // never dispatched: release without a metric

	stream := f.engine.Snapshot(f.key, []Candidate{f.candidate, second}, true)
	assert.EqualValues(t, 4, stream.Summary.Attempts30m)
	assert.EqualValues(t, 3, stream.Summary.Successes30m)
	assert.Zero(t, stream.Summary.InFlight)
	require.NotNil(t, stream.Channels[0].SuccessRate30m)
	assert.Zero(t, *stream.Channels[0].SuccessRate30m, "retry success must not erase the first failure")
	assert.Nil(t, stream.Channels[0].AvgTTFTMS5m)
	require.NotNil(t, stream.Channels[1].AvgTTFTMS5m)
	assert.Equal(t, 2000.0, *stream.Channels[1].AvgTTFTMS5m)
	assert.EqualValues(t, 1, stream.Channels[1].Window5m.LatencySamples)
	assert.EqualValues(t, 4, stream.Channels[1].Dispatches30m)
	assert.Equal(t, 1.0, *stream.Channels[1].HealthAttainment, "missing latency must not become zero or a failed observation")
	nonStream := f.engine.Snapshot(f.key, []Candidate{second}, false)
	assert.Equal(t, 10000.0, *nonStream.Channels[0].AvgTTFTMS5m)
	assert.Zero(t, *nonStream.Channels[0].HealthAttainment)
	assert.NotNil(t, stream.Trend[len(stream.Trend)-1].InFlight)
	assert.Equal(t, 1, *stream.Trend[len(stream.Trend)-1].InFlight)
	assert.Nil(t, stream.Trend[0].InFlight)
}

func TestStaticInitializationAndConfigReplacement(t *testing.T) {
	f := newSchedulerFixture()
	primary := f.candidate
	primary.Priority = 1000
	primary.Weight = 30
	secondary := Candidate{ID: 2, Name: "Secondary", Status: 1, Priority: 1000, Weight: 10}
	fallback := Candidate{ID: 3, Name: "Fallback", Status: 1, Priority: 1, Weight: 9999}
	candidates := []Candidate{primary, secondary, fallback}
	snapshot := f.engine.Snapshot(f.key, candidates, false)
	assert.Equal(t, 0.75, snapshot.Channels[0].SelectionProbability)
	assert.Equal(t, 0.25, snapshot.Channels[1].SelectionProbability)
	assert.Zero(t, snapshot.Channels[2].SelectionProbability)
	weight := 10.0
	require.NoError(t, f.engine.ReplaceOverrides(f.key, map[int]Override{1: {Weight: &weight}}))
	weight = 9999 // a caller cannot mutate saved settings through a pointer
	snapshot = f.engine.Snapshot(f.key, candidates, false)
	assert.Equal(t, 0.5, snapshot.Channels[0].SelectionProbability)
	require.NoError(t, f.engine.ReplaceOverrides(f.key, nil))
	assert.Equal(t, 0.75, f.engine.Snapshot(f.key, candidates, false).Channels[0].SelectionProbability)
	primary.Weight, secondary.Weight = 0, 0
	snapshot = f.engine.Snapshot(f.key, []Candidate{primary, secondary}, false)
	assert.Equal(t, 0.5, snapshot.Channels[0].SelectionProbability)
	assert.Equal(t, 0.5, snapshot.Channels[1].SelectionProbability)

	f.dispatch(t, primary, true, Outcome{Success: true, TTFT: latency(4 * time.Second)})
	snapshot = f.engine.Snapshot(f.key, []Candidate{primary}, true)
	assert.Zero(t, *snapshot.Channels[0].HealthAttainment)
	config := f.engine.GetConfig(f.key)
	config.LatencyTargetMS = 5000
	require.NoError(t, f.engine.SetConfig(f.key, config))
	snapshot = f.engine.Snapshot(f.key, []Candidate{primary}, true)
	assert.Equal(t, 1.0, *snapshot.Channels[0].HealthAttainment, "target changes must reevaluate historical observations")
	assert.Equal(t, 4000.0, *snapshot.Channels[0].AvgTTFTMS5m)
	config.LatencyTargetMS = math.NaN()
	assert.ErrorIs(t, f.engine.SetConfig(f.key, config), ErrInvalidConfig)
}

func TestDynamicHealthImmediatePenaltyAndEvidenceBasedRecovery(t *testing.T) {
	f := newSchedulerFixture()
	for range 100 {
		f.dispatch(t, f.candidate, true, Outcome{Success: true, TTFT: latency(time.Second)})
	}
	before := f.engine.Snapshot(f.key, []Candidate{f.candidate}, true).Channels[0]
	require.True(t, f.engine.IsActive(f.key))
	for range 40 {
		f.dispatch(t, f.candidate, true, Outcome{ChannelFailure: true})
	}
	after := f.engine.Snapshot(f.key, []Candidate{f.candidate}, true).Channels[0]
	assert.Less(t, after.EffectiveWeight, before.EffectiveWeight, "errors take effect without waiting for the minute refresh")
	assert.Greater(t, after.EffectiveWeight, 0.0)
	assert.GreaterOrEqual(t, after.HealthScore, 1.0)
	assert.True(t, after.CanRecover)
	assert.Equal(t, after.EffectiveWeight, f.engine.Snapshot(f.key, []Candidate{f.candidate}, true).Channels[0].EffectiveWeight, "read/selection must not apply the same penalty repeatedly")

	f.now = f.now.Add(6 * time.Minute)
	withoutNewEvidence := f.engine.Snapshot(f.key, []Candidate{f.candidate}, true).Channels[0]
	assert.LessOrEqual(t, withoutNewEvidence.HealthScore, after.HealthScore+1e-9)
	f.dispatch(t, f.candidate, true, Outcome{Success: true, TTFT: latency(time.Second)})
	f.now = f.now.Add(time.Minute)
	withEvidence := f.engine.Snapshot(f.key, []Candidate{f.candidate}, true).Channels[0]
	assert.Greater(t, withEvidence.HealthScore, withoutNewEvidence.HealthScore)
	assert.LessOrEqual(t, withEvidence.HealthScore, withoutNewEvidence.HealthScore+2+1e-9, "one fresh success permits at most two health points of recovery")

	disabled := f.candidate
	disabled.Status = 2
	f.engine.ClearPenalty(f.key, disabled.ID)
	disabledSnapshot := f.engine.Snapshot(f.key, []Candidate{disabled}, true).Channels[0]
	assert.Equal(t, "disabled", disabledSnapshot.RouteState)
	assert.Zero(t, disabledSnapshot.SelectionProbability)
	assert.False(t, disabledSnapshot.CanRecover)
	assert.EqualValues(t, 141, disabledSnapshot.Window30m.Attempts, "manual recalculation must preserve failure history")
}

func TestLongWindowDegradationKeepsRecoveryActionAfterShortErrorsExpire(t *testing.T) {
	f := newSchedulerFixture()
	for range 60 {
		f.dispatch(t, f.candidate, true, Outcome{Success: true, TTFT: latency(time.Second)})
	}
	for range 40 {
		f.dispatch(t, f.candidate, true, Outcome{ChannelFailure: true})
	}
	f.now = f.now.Add(6 * time.Minute)
	f.engine.ClearPenalty(f.key, f.candidate.ID)
	row := f.engine.Snapshot(f.key, []Candidate{f.candidate}, true).Channels[0]
	require.Zero(t, row.Window5m.Failures, "all recent errors have naturally expired")
	require.EqualValues(t, 40, row.Window30m.Failures)
	assert.Equal(t, "degraded", row.RouteState)
	assert.True(t, row.CanRecover, "long-window degradation must keep the administrator recalculation action available")
	require.NotNil(t, row.SuccessRate30m)
	assert.Equal(t, 0.6, *row.SuccessRate30m)
	assert.InDelta(t, math.Pow(79.0/120, 4)*100, row.HealthScore, 1e-9,
		"clearing the recovery ceiling must retain the current low-health baseline")
	f.engine.ClearPenalty(f.key, f.candidate.ID)
	recalculated := f.engine.Snapshot(f.key, []Candidate{f.candidate}, true).Channels[0]
	assert.Equal(t, row.Window30m, recalculated.Window30m, "recalculation must not erase any error history")
	assert.Equal(t, row.HealthScore, recalculated.HealthScore, "manual action alone cannot fabricate recovery")
	assert.True(t, recalculated.CanRecover)

	for _, state := range []string{"disabled", "cooling", "ineligible"} {
		t.Run(state, func(t *testing.T) {
			candidate := f.candidate
			switch state {
			case "disabled":
				candidate.Status = 2
			case "cooling":
				candidate.CooldownUntil = f.now.Add(time.Minute)
			case "ineligible":
				candidate.Excluded = true
			}
			unavailable := f.engine.Snapshot(f.key, []Candidate{candidate}, true).Channels[0]
			assert.Equal(t, state, unavailable.RouteState)
			assert.False(t, unavailable.CanRecover)
			assert.Zero(t, unavailable.SelectionProbability)
		})
	}

	limited := f.candidate
	limited.Capacity = 1
	request := f.engine.BeginRequest(f.key, "capacity-held", false)
	_, err := f.engine.ReserveCandidate(request, limited, true)
	require.NoError(t, err)
	defer f.engine.EndRequest(request)
	saturated := f.engine.Snapshot(f.key, []Candidate{limited}, true).Channels[0]
	assert.Equal(t, "saturated", saturated.RouteState)
	assert.True(t, saturated.CanRecover, "recalculating health does not bypass the capacity gate")
	assert.Zero(t, saturated.SelectionProbability)
}

func TestRetryRankingExclusionAndMonotonicDynamicTransition(t *testing.T) {
	f := newSchedulerFixture()
	slow := f.candidate
	slow.Priority = 100
	fast := Candidate{ID: 2, Name: "Fast", Status: 1, Priority: 1, Weight: 1, Capacity: 100}
	for range 50 {
		f.dispatch(t, slow, true, Outcome{Success: true, TTFT: latency(20 * time.Second)})
		f.dispatch(t, fast, true, Outcome{Success: true, TTFT: latency(time.Second)})
	}
	r := f.engine.BeginRequest(f.key, "retry", false)
	a, err := f.engine.SelectAndReserve(r, []Candidate{slow, fast}, true, true)
	require.NoError(t, err)
	assert.Equal(t, fast.ID, a.Candidate.ID, "dynamic retry ranks across static priority tiers")
	f.engine.StartAttempt(a)
	f.engine.FinishAttempt(a, Outcome{ChannelFailure: true})
	a, err = f.engine.SelectAndReserve(r, []Candidate{slow, fast}, true, true)
	require.NoError(t, err)
	assert.Equal(t, slow.ID, a.Candidate.ID)
	f.engine.StartAttempt(a)
	f.engine.FinishAttempt(a, Outcome{Success: true})
	_, err = f.engine.SelectAndReserve(r, []Candidate{slow, fast}, true, true)
	assert.ErrorIs(t, err, ErrNoEligibleChannel)
	pinned, err := f.engine.ReserveCandidate(r, fast, true)
	require.NoError(t, err, "fixed-channel retry policy belongs to the host")
	f.engine.FinishAttempt(pinned, Outcome{})
	f.engine.EndRequest(r)

	f.now = f.now.Add(20 * time.Minute)
	keepAlive := f.engine.BeginRequest(f.key, "keep-alive", false)
	f.engine.EndRequest(keepAlive)
	f.now = f.now.Add(11 * time.Minute)
	snapshot := f.engine.Snapshot(f.key, []Candidate{slow, fast}, true)
	assert.True(t, snapshot.Active)
	assert.Greater(t, snapshot.Channels[1].EffectiveWeight, 0.0, "sample expiry cannot restore static priority gating")
}

func TestSharedCapacityAtomicReservationAndPoolChanges(t *testing.T) {
	f := newSchedulerFixture()
	candidate := f.candidate
	candidate.Capacity, candidate.CapacityKey = 1, "shared-upstream"
	otherKey := Key{Group: "other", Model: "other-model"}
	otherChannel := Candidate{ID: 2, Name: "Same upstream", Status: 1, Capacity: 10, CapacityKey: "shared-upstream"}
	f.engine.Snapshot(f.key, []Candidate{candidate}, false)
	f.engine.Snapshot(otherKey, []Candidate{otherChannel}, false)
	var wg sync.WaitGroup
	results := make(chan *Attempt, 2)
	requests := []*Request{f.engine.BeginRequest(f.key, "one", false), f.engine.BeginRequest(otherKey, "two", false)}
	for i := range 2 {
		wg.Go(func() {
			ch := candidate
			if i == 1 {
				ch = otherChannel
			}
			a, err := f.engine.ReserveCandidate(requests[i], ch, true)
			if err == nil {
				results <- a
			}
		})
	}
	wg.Wait()
	close(results)
	reserved := make([]*Attempt, 0, 1)
	for result := range results {
		reserved = append(reserved, result)
	}
	require.Len(t, reserved, 1, "groups/channels sharing a pool must atomically honor its strictest capacity")
	snapshot := f.engine.Snapshot(otherKey, []Candidate{otherChannel}, true)
	assert.Equal(t, 1, snapshot.Channels[0].Capacity)
	assert.Equal(t, 1, snapshot.Channels[0].InFlight)
	assert.Equal(t, "saturated", snapshot.Channels[0].RouteState)
	assert.Zero(t, snapshot.Channels[0].SelectionProbability)

	winningAttempt := reserved[0]
	require.NoError(t, f.engine.SetOverride(winningAttempt.request.Key, winningAttempt.Candidate.ID, Override{CapacityKey: "new-pool"}))
	f.engine.FinishAttempt(winningAttempt, Outcome{})
	for _, request := range requests {
		f.engine.EndRequest(request)
	}
	assert.Zero(t, f.engine.pools["shared-upstream"].inFlight, "changing settings must release the originally reserved pool")
	assert.Zero(t, f.engine.pools["new-pool"].inFlight)

	candidate.Status = 2
	r := f.engine.BeginRequest(f.key, "disabled", false)
	_, err := f.engine.ReserveCandidate(r, candidate, false)
	assert.ErrorIs(t, err, ErrNoEligibleChannel)
	candidate.Status = 1
	candidate.CooldownUntil = f.now.Add(time.Minute)
	_, err = f.engine.ReserveCandidate(r, candidate, false)
	assert.ErrorIs(t, err, ErrNoEligibleChannel)
	f.engine.EndRequest(r)
}

func TestExplorationRequiresHealthyRecentEvidence(t *testing.T) {
	for _, healthy := range []bool{true, false} {
		t.Run(fmt.Sprintf("healthy-alternative-%v", healthy), func(t *testing.T) {
			f := newSchedulerFixture()
			alternative := Candidate{ID: 2, Name: "Alternative", Status: 1, Weight: 10, Capacity: 100}
			for range 100 {
				f.dispatch(t, f.candidate, true, Outcome{Success: true, TTFT: latency(time.Millisecond)})
				if healthy {
					f.dispatch(t, alternative, true, Outcome{Success: true, TTFT: latency(40 * time.Second)})
				} else {
					f.dispatch(t, alternative, true, Outcome{ChannelFailure: true})
				}
			}
			snapshot := f.engine.Snapshot(f.key, []Candidate{f.candidate, alternative}, true)
			if healthy {
				assert.InDelta(t, 0.9, snapshot.Channels[0].SelectionProbability, 1e-9)
				assert.InDelta(t, 0.1, snapshot.Channels[1].SelectionProbability, 1e-9)
			} else {
				assert.Greater(t, snapshot.Channels[0].SelectionProbability, 0.9)
				assert.Less(t, snapshot.Channels[1].SelectionProbability, 0.1)
				assert.Greater(t, snapshot.Channels[1].SelectionProbability, 0.0)
			}
		})
	}
}

func TestLatencyConfidenceDoesNotBorrowOtherTransportSamples(t *testing.T) {
	f := newSchedulerFixture()
	for range 10 {
		f.dispatch(t, f.candidate, true, Outcome{Success: true, TTFT: latency(20 * time.Second)})
	}
	f.now = f.now.Add(6 * time.Minute)
	f.dispatch(t, f.candidate, true, Outcome{Success: true, TTFT: latency(time.Second)})
	before := f.engine.Snapshot(f.key, []Candidate{f.candidate}, true).Channels[0]
	for range 100 {
		f.dispatch(t, f.candidate, false, Outcome{Success: true, TTFT: latency(time.Millisecond)})
	}
	f.now = f.now.Add(time.Minute)
	after := f.engine.Snapshot(f.key, []Candidate{f.candidate}, true).Channels[0]
	assert.InDelta(t, before.QualityScore/before.HealthScore, after.QualityScore/after.HealthScore, 1e-9,
		"non-streaming traffic may change success confidence but cannot change the streaming latency score")
	assert.EqualValues(t, 1, after.Window5m.LatencySamples)
	assert.Equal(t, 1000.0, *after.AvgTTFTMS5m)
}

func TestConfiguredSharedPoolLimitsApplyBeforeFirstGroupRequest(t *testing.T) {
	f := newSchedulerFixture()
	lowKey := Key{Group: "unused-group", Model: "unused-model"}
	lowChannel := Candidate{ID: 2, Name: "Idle shared upstream", Status: 1}
	lowCapacity, highCapacity := 1, 100
	require.NoError(t, f.engine.SetOverride(lowKey, lowChannel.ID, Override{Capacity: &lowCapacity, CapacityKey: "configured-shared"}))
	require.NoError(t, f.engine.SetOverride(f.key, f.candidate.ID, Override{Capacity: &highCapacity, CapacityKey: "configured-shared"}))
	f.engine.RegisterCandidates(lowKey, []Candidate{lowChannel})
	f.engine.RegisterCandidates(f.key, []Candidate{f.candidate})

	r := f.engine.BeginRequest(f.key, "first", false)
	a, err := f.engine.ReserveCandidate(r, f.candidate, true)
	require.NoError(t, err)
	assert.Equal(t, 1, a.Candidate.Capacity, "an unvisited group must still constrain its configured shared pool")
	another := f.engine.BeginRequest(f.key, "second", false)
	_, err = f.engine.ReserveCandidate(another, f.candidate, true)
	assert.ErrorIs(t, err, ErrNoEligibleChannel)
	assert.False(t, f.engine.IsActive(lowKey))

	f.engine.RegisterCandidates(lowKey, nil)
	require.NoError(t, f.engine.SetConfig(lowKey, DefaultConfig()))
	require.NoError(t, f.engine.ReplaceOverrides(lowKey, nil))
	b, err := f.engine.ReserveCandidate(another, f.candidate, true)
	require.NoError(t, err, "removed metadata must not be resurrected by a later config refresh")
	assert.Equal(t, highCapacity, b.Candidate.Capacity)
	f.engine.StartAttempt(a)
	f.engine.FinishAttempt(a, Outcome{Success: true, TTFT: latency(time.Second)})
	f.engine.RegisterCandidates(f.key, nil)
	f.engine.FinishAttempt(b, Outcome{})
	f.engine.EndRequest(r)
	f.engine.EndRequest(another)
	assert.Zero(t, f.engine.pools["configured-shared"].inFlight, "removing membership must not lose the outstanding reservation pool")
	assert.Empty(t, f.engine.pools["configured-shared"].members)
	f.engine.RegisterCandidates(f.key, []Candidate{f.candidate})
	assert.EqualValues(t, 1, f.engine.Snapshot(f.key, []Candidate{f.candidate}, true).Summary.Attempts30m,
		"removing and restoring metadata preserves observed history")
}

func TestDashboardExcludedAbilitiesRemainVisibleWithoutRoutingProbability(t *testing.T) {
	f := newSchedulerFixture()
	excluded := Candidate{ID: 2, Name: "Disabled ability", Status: 1, Priority: 1000, Weight: 9999, Excluded: true}
	snapshot := f.engine.Snapshot(f.key, []Candidate{f.candidate, excluded}, true)
	require.Len(t, snapshot.Channels, 2)
	assert.Equal(t, 1, snapshot.Channels[1].Status, "ability eligibility must not rewrite channel status")
	assert.Equal(t, "ineligible", snapshot.Channels[1].RouteState)
	assert.False(t, snapshot.Channels[1].CanRecover)
	assert.Zero(t, snapshot.Channels[1].SelectionProbability)
	assert.Equal(t, 1.0, snapshot.Channels[0].SelectionProbability)
	assert.Equal(t, 1, snapshot.Summary.EligibleChannels)
	r := f.engine.BeginRequest(f.key, "excluded", false)
	_, err := f.engine.ReserveCandidate(r, excluded, true)
	assert.ErrorIs(t, err, ErrNoEligibleChannel)
	a, err := f.engine.SelectAndReserve(r, []Candidate{f.candidate, excluded}, true, false)
	require.NoError(t, err)
	assert.Equal(t, f.candidate.ID, a.Candidate.ID)
	f.engine.EndRequest(r)
}

func TestRecentBusinessKeysUseRealStartsAndExpireAtThirtyMinutes(t *testing.T) {
	f := newSchedulerFixture()
	startedAt := f.now
	metadataOnly := Key{Group: "metadata", Model: "configured-only"}
	require.NoError(t, f.engine.SetConfig(metadataOnly, DefaultConfig()))
	f.engine.Snapshot(Key{Group: "metadata", Model: "viewed-only"}, []Candidate{f.candidate}, true)
	assert.Empty(t, f.engine.RecentBusinessKeys())

	first := f.engine.BeginRequest(f.key, "older-business", true)
	f.engine.TouchRequest(first)
	f.engine.EndRequest(first)
	f.now = f.now.Add(time.Second)
	newer := Key{Group: "default", Model: "z-model"}
	tied := Key{Group: "default", Model: "a-model"}
	newerRequest := f.engine.BeginRequest(newer, "newer-business", true)
	f.engine.TouchRequest(newerRequest)
	tiedRequest := f.engine.BeginRequest(tied, "tied-business", false)
	f.engine.EndRequest(tiedRequest)
	assert.Equal(t, []Key{tied, newer, f.key}, f.engine.RecentBusinessKeys(),
		"real starts are visible before confirmation/completion and ties sort deterministically")

	f.now = startedAt.Add(20 * time.Minute)
	for _, key := range []Key{f.key, {Group: "probes", Model: "probe-only"}} {
		probe := f.engine.BeginRequest(key, "probe-"+key.Model, true)
		attempt, err := f.engine.ReserveCandidate(probe, f.candidate, true)
		require.NoError(t, err)
		f.engine.StartAttempt(attempt)
		f.engine.FinishAttempt(attempt, Outcome{Success: true, TTFT: latency(time.Second)})
		f.engine.EndRequest(probe)
	}
	f.engine.TouchRequest(newerRequest) // retrying one original request is not a new start
	assert.Equal(t, []Key{tied, newer, f.key}, f.engine.RecentBusinessKeys(),
		"a successful probe neither creates business activity nor extends existing activity")

	f.now = startedAt.Add(30 * time.Minute)
	assert.Equal(t, []Key{tied, newer}, f.engine.RecentBusinessKeys(), "a start exactly 30 minutes old has expired")
	f.now = f.now.Add(time.Second)
	assert.Empty(t, f.engine.RecentBusinessKeys(), "an unfinished request does not turn its old start into fresh traffic")
	f.engine.EndRequest(newerRequest)
}
