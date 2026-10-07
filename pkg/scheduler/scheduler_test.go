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
	primary.Priority = math.MaxInt64
	primary.Weight = 30
	secondary := Candidate{ID: 2, Name: "Secondary", Status: 1, Priority: math.MaxInt64, Weight: 10}
	fallback := Candidate{ID: 3, Name: "Fallback", Status: 1, Priority: math.MinInt64, Weight: 9999}
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

func TestMaturePriorityPreference(t *testing.T) {
	for _, tc := range []struct {
		name            string
		highPriority    int64
		lowPriority     int64
		failures        int
		lowFailures     int
		highLatency     time.Duration
		heldReservation bool
		reverse         bool
		expectedLeader  int
		noBonus         bool
		belowTarget     bool
	}{
		{name: "equal-quality", highPriority: 100, lowPriority: 1, expectedLeader: 1},
		{name: "negative-priorities", highPriority: -1, lowPriority: -100, expectedLeader: 1},
		{name: "signed-extremes", highPriority: math.MaxInt64, lowPriority: math.MinInt64, expectedLeader: 1},
		{name: "reversed-candidate-order", highPriority: math.MaxInt64, lowPriority: math.MinInt64, reverse: true, expectedLeader: 1},
		{name: "same-priority", highPriority: 100, lowPriority: 100, noBonus: true},
		{name: "success-within-three-points", highPriority: 100, lowPriority: 1, failures: 2, expectedLeader: 1},
		{name: "success-near-three-point-boundary", highPriority: 100, lowPriority: 1, failures: 3, expectedLeader: 1},
		{name: "success-outside-three-points", highPriority: 100, lowPriority: 1, failures: 4, expectedLeader: 2, noBonus: true},
		{name: "same-success-below-target", highPriority: 100, lowPriority: 1, failures: 25, lowFailures: 25, expectedLeader: 1, belowTarget: true},
		{name: "latency-overrides-priority", highPriority: 100, lowPriority: 1, highLatency: 20 * time.Second, expectedLeader: 2},
		{name: "load-and-latency-override-priority", highPriority: 100, lowPriority: 1, highLatency: 3 * time.Second, heldReservation: true, expectedLeader: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newSchedulerFixture()
			high := f.candidate
			high.Priority, high.Capacity = tc.highPriority, 2
			low := Candidate{ID: 2, Name: "Lower priority", Status: 1, Priority: tc.lowPriority, Weight: 1, Capacity: 100}
			highLatency := tc.highLatency
			if highLatency == 0 {
				highLatency = time.Second
			}
			for range 100 {
				f.dispatch(t, high, true, Outcome{Success: true, TTFT: latency(highLatency)})
				f.dispatch(t, low, true, Outcome{Success: true, TTFT: latency(time.Second)})
			}
			for range tc.failures {
				f.dispatch(t, high, true, Outcome{ChannelFailure: true})
			}
			for range tc.lowFailures {
				f.dispatch(t, low, true, Outcome{ChannelFailure: true})
			}
			if tc.heldReservation {
				request := f.engine.BeginRequest(f.key, "in-flight", false)
				_, err := f.engine.ReserveCandidate(request, high, true)
				require.NoError(t, err)
				defer f.engine.EndRequest(request)
			}
			controlLow := low
			controlLow.Priority = high.Priority
			control := f.engine.Snapshot(f.key, []Candidate{high, controlLow}, true)
			candidates := []Candidate{high, low}
			if tc.reverse {
				candidates = []Candidate{low, high}
			}
			snapshot := f.engine.Snapshot(f.key, candidates, true)
			require.Equal(t, "dynamic", snapshot.Phase)
			require.Len(t, snapshot.Channels, 2)
			rows := make(map[int]ChannelSnapshot, 2)
			probability := 0.0
			for _, row := range snapshot.Channels {
				rows[row.ChannelID] = row
				assert.False(t, math.IsNaN(row.EffectiveWeight) || math.IsInf(row.EffectiveWeight, 0))
				assert.False(t, math.IsNaN(row.SelectionProbability) || math.IsInf(row.SelectionProbability, 0))
				assert.Positive(t, row.SelectionProbability)
				probability += row.SelectionProbability
			}
			assert.InDelta(t, 1, probability, 1e-9)
			assert.InDelta(t, control.Channels[1].EffectiveWeight, rows[low.ID].EffectiveWeight, 1e-9)
			assert.GreaterOrEqual(t, rows[high.ID].EffectiveWeight, control.Channels[0].EffectiveWeight)
			assert.LessOrEqual(t, rows[high.ID].EffectiveWeight, 2*control.Channels[0].EffectiveWeight+1e-9)
			if tc.noBonus {
				assert.InDelta(t, control.Channels[0].EffectiveWeight, rows[high.ID].EffectiveWeight, 1e-9)
				assert.InDelta(t, control.Channels[0].SelectionProbability, rows[high.ID].SelectionProbability, 1e-9)
			} else if tc.failures == tc.lowFailures {
				assert.InDelta(t, 2*control.Channels[0].EffectiveWeight, rows[high.ID].EffectiveWeight, 1e-9)
			}
			if tc.expectedLeader == 0 {
				assert.InDelta(t, .5, rows[high.ID].SelectionProbability, 1e-9)
			} else {
				assert.Greater(t, rows[tc.expectedLeader].SelectionProbability, .5)
			}
			if tc.failures == tc.lowFailures && highLatency == time.Second && !tc.noBonus {
				assert.InDelta(t, 2.0/3, rows[high.ID].SelectionProbability, 1e-9, "comparable mature channels retain a bounded two-to-one preference")
			}
			if tc.belowTarget {
				assert.Zero(t, snapshot.Summary.HealthyChannels)
				assert.InDelta(t, rows[high.ID].HealthBaseline, rows[low.ID].HealthBaseline, 1e-9)
				assert.InDelta(t, .5, control.Channels[0].SelectionProbability, 1e-9)
				for _, row := range snapshot.Channels {
					require.NotNil(t, row.SuccessRate5m)
					assert.InDelta(t, .8, *row.SuccessRate5m, 1e-9)
					assert.Less(t, *row.SuccessRate5m, snapshot.Config.SuccessTarget)
					assert.InDelta(t, row.HealthBaseline, row.HealthScore, 1e-9, "stable failures leave no extra recovery ceiling")
				}
			}

			cumulative := 0.0
			for _, row := range snapshot.Channels {
				sample := cumulative + row.SelectionProbability/2
				f.engine.random = func() float64 { return sample }
				request := f.engine.BeginRequest(f.key, "", false)
				attempt, err := f.engine.SelectAndReserve(request, candidates, true, false)
				require.NoError(t, err)
				assert.Equal(t, row.ChannelID, attempt.Candidate.ID, "selection must use the displayed probability intervals")
				if tc.heldReservation && row.ChannelID == high.ID {
					full := f.engine.Snapshot(f.key, []Candidate{high, low}, true)
					assert.Equal(t, "saturated", full.Channels[0].RouteState)
					assert.Zero(t, full.Channels[0].SelectionProbability, "priority cannot bypass a full capacity pool")
				}
				f.engine.EndRequest(request)
				cumulative += row.SelectionProbability
			}
			expectedRetry := high.ID
			if rows[low.ID].EffectiveWeight > rows[high.ID].EffectiveWeight {
				expectedRetry = low.ID
			}
			request := f.engine.BeginRequest(f.key, "retry", false)
			attempt, err := f.engine.SelectAndReserve(request, candidates, true, true)
			require.NoError(t, err)
			assert.Equal(t, expectedRetry, attempt.Candidate.ID, "retry ranks the same effective weights shown in the snapshot")
			f.engine.EndRequest(request)
			if tc.name == "signed-extremes" {
				high.CooldownUntil = f.now.Add(time.Minute)
				cooling := f.engine.Snapshot(f.key, []Candidate{high, low}, true)
				assert.Equal(t, "cooling", cooling.Channels[0].RouteState)
				assert.Zero(t, cooling.Channels[0].SelectionProbability, "priority cannot bypass a cooling channel")
			}
		})
	}
}

func TestPriorityCannotBypassResidualRecoveryPenalty(t *testing.T) {
	f := newSchedulerFixture()
	high := f.candidate
	high.Priority = math.MaxInt64
	low := Candidate{ID: 2, Name: "Recovered", Status: 1, Priority: math.MinInt64, Weight: 1, Capacity: 100}
	for range 100 {
		f.dispatch(t, high, true, Outcome{Success: true, TTFT: latency(time.Second)})
		f.dispatch(t, low, true, Outcome{Success: true, TTFT: latency(time.Second)})
	}
	for range 40 {
		f.dispatch(t, high, true, Outcome{ChannelFailure: true})
	}
	f.now = f.now.Add(20 * time.Minute)
	keepAlive := f.engine.BeginRequest(f.key, "keep-alive", false)
	f.engine.EndRequest(keepAlive)
	f.now = f.now.Add(11 * time.Minute)
	for _, candidate := range []Candidate{high, low} {
		f.dispatch(t, candidate, true, Outcome{Success: true, TTFT: latency(time.Second)})
	}
	controlLow := low
	controlLow.Priority = high.Priority
	control := f.engine.Snapshot(f.key, []Candidate{high, controlLow}, true)
	snapshot := f.engine.Snapshot(f.key, []Candidate{high, low}, true)
	require.Equal(t, "dynamic", snapshot.Phase)
	assert.Equal(t, snapshot.Channels[0].Window30m, snapshot.Channels[1].Window30m, "both channels now have the same observed success and latency")
	assert.Less(t, snapshot.Channels[0].HealthScore, snapshot.Channels[0].HealthBaseline)
	assert.InDelta(t, control.Channels[0].EffectiveWeight, snapshot.Channels[0].EffectiveWeight, 1e-9)
	assert.InDelta(t, control.Channels[0].SelectionProbability, snapshot.Channels[0].SelectionProbability, 1e-9,
		"expired failures cannot grant priority while the recovery ceiling still applies")
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
			f.candidate.Priority = math.MaxInt64
			alternative := Candidate{ID: 2, Name: "Alternative", Status: 1, Priority: math.MinInt64, Weight: 10, Capacity: 100}
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

func TestRecentChannelStatsUseCompletedAttemptsAndPublishedSnapshots(t *testing.T) {
	f := newSchedulerFixture()
	initial := f.engine.RecentChannelStats()
	assert.False(t, initial.Ready)
	assert.Zero(t, initial.RefreshedAt)
	assert.EqualValues(t, 600, initial.WindowSeconds)
	assert.Equal(t, "instance", initial.Scope)
	require.NotNil(t, initial.Items)
	assert.Empty(t, initial.Items)

	f.engine.RefreshRecentChannelStats()
	assert.True(t, f.engine.RecentChannelStats().Ready)
	assert.Empty(t, f.engine.RecentChannelStats().Items, "a ready empty window is not pending")

	fallback := Candidate{ID: 2, Name: "Fallback", Status: 1, Weight: 10, Capacity: 100}
	r := f.engine.BeginRequest(f.key, "retried-request", false)
	failed, err := f.engine.ReserveCandidate(r, f.candidate, true)
	require.NoError(t, err)
	f.engine.StartAttempt(failed)
	f.engine.FinishAttempt(failed, Outcome{ChannelFailure: true})
	retried, err := f.engine.ReserveCandidate(r, fallback, true)
	require.NoError(t, err)
	f.engine.StartAttempt(retried)
	f.engine.FinishAttempt(retried, Outcome{Success: true})
	f.engine.FinishAttempt(retried, Outcome{Success: true}) // duplicate completion
	f.engine.EndRequest(r)

	f.key = Key{Group: "other", Model: "other-model"}
	f.dispatch(t, f.candidate, false, Outcome{Success: true})
	f.dispatch(t, f.candidate, true, Outcome{}) // cancellation / client error
	pending := f.engine.BeginRequest(f.key, "pending", false)
	unfinished, err := f.engine.ReserveCandidate(pending, fallback, true)
	require.NoError(t, err)
	f.engine.StartAttempt(unfinished)
	assert.Empty(t, f.engine.RecentChannelStats().Items, "request completion never forces synchronous cache refresh")

	f.engine.RefreshRecentChannelStats()
	stats := f.engine.RecentChannelStats()
	assert.Equal(t, f.now.Unix(), stats.RefreshedAt)
	require.Len(t, stats.Items, 2)
	assert.Equal(t, 1, stats.Items[0].ChannelID)
	assert.EqualValues(t, 2, stats.Items[0].Requests, "groups, models and stream modes are combined")
	assert.EqualValues(t, 1, stats.Items[0].Successes)
	require.NotNil(t, stats.Items[0].SuccessRate)
	assert.Equal(t, 0.5, *stats.Items[0].SuccessRate)
	assert.Equal(t, 2, stats.Items[1].ChannelID)
	assert.EqualValues(t, 1, stats.Items[1].Requests, "successful retry is a separate attempt; pending attempts are excluded")
	assert.EqualValues(t, 1, stats.Items[1].Successes)
	require.NotNil(t, stats.Items[1].SuccessRate)
	assert.Equal(t, 1.0, *stats.Items[1].SuccessRate)
	f.engine.EndRequest(pending)

	stats.Items[0].Requests = 999
	*stats.Items[0].SuccessRate = 0
	assert.EqualValues(t, 2, f.engine.RecentChannelStats().Items[0].Requests, "callers cannot alter the cache")
	assert.Equal(t, 0.5, *f.engine.RecentChannelStats().Items[0].SuccessRate)

	f.now = f.now.Add(9*time.Minute + 59*time.Second)
	f.engine.ClearPenalty(f.key, f.candidate.ID)
	f.engine.RefreshRecentChannelStats()
	assert.Len(t, f.engine.RecentChannelStats().Items, 2, "recalculating weights preserves observed history")
	f.now = f.now.Add(time.Second)
	assert.Len(t, f.engine.RecentChannelStats().Items, 2, "reading does not aggregate even when samples have expired")
	f.engine.RefreshRecentChannelStats()
	expiredTenMinute := f.engine.RecentChannelStats()
	require.Len(t, expiredTenMinute.Items, 2)
	assert.Zero(t, expiredTenMinute.Items[0].Requests, "an attempt exactly ten minutes old has expired from the legacy window")
	assert.Nil(t, expiredTenMinute.Items[0].SuccessRate)
	assert.EqualValues(t, 2, expiredTenMinute.Items[0].Requests1h, "the independent hour history is retained")

	r = f.engine.BeginRequest(f.key, "long-stream", false)
	longStream, err := f.engine.ReserveCandidate(r, f.candidate, true)
	require.NoError(t, err)
	f.engine.StartAttempt(longStream)
	f.now = f.now.Add(31 * time.Minute)
	f.engine.FinishAttempt(longStream, Outcome{Success: true})
	f.engine.EndRequest(r) // triggers idle learning reset after the long stream
	f.engine.RefreshRecentChannelStats()
	reset := f.engine.RecentChannelStats()
	require.Len(t, reset.Items, 2)
	assert.EqualValues(t, 1, reset.Items[0].Requests, "idle reset retains attempts in their completion window")
	assert.EqualValues(t, 3, reset.Items[0].Requests1h)
	require.NotNil(t, reset.Items[0].SuccessRate)
	assert.Equal(t, 1.0, *reset.Items[0].SuccessRate)
}

func TestRecentChannelHistoryRollingBoundaries(t *testing.T) {
	for _, tc := range []struct {
		age       time.Duration
		segment   int
		inTenMin  bool
		inFiveMin bool
	}{
		{time.Hour, -1, false, false},
		{time.Hour - time.Second, 0, false, false},
		{50 * time.Minute, 0, false, false},
		{50*time.Minute - time.Second, 1, false, false},
		{40 * time.Minute, 1, false, false},
		{40*time.Minute - time.Second, 2, false, false},
		{30 * time.Minute, 2, false, false},
		{30*time.Minute - time.Second, 3, false, false},
		{20 * time.Minute, 3, false, false},
		{20*time.Minute - time.Second, 4, false, false},
		{10 * time.Minute, 4, false, false},
		{10*time.Minute - time.Second, 5, true, false},
		{5 * time.Minute, 5, true, false},
		{5*time.Minute - time.Second, 5, true, true},
		{0, 5, true, true},
	} {
		t.Run(tc.age.String(), func(t *testing.T) {
			f := newSchedulerFixture()
			start := f.now
			initial := f.engine.FreshRecentChannelStats()
			assert.True(t, initial.Ready)
			assert.Equal(t, start.Unix(), initial.CollectedSince)
			assert.EqualValues(t, 3600, initial.HistoryWindowSeconds)
			assert.EqualValues(t, 600, initial.BucketSeconds)
			assert.EqualValues(t, 300, initial.LatencyWindowSeconds)
			f.now = start.Add(time.Hour - tc.age)
			f.dispatch(t, f.candidate, true, Outcome{Success: true, TTFT: latency(2 * time.Second)})
			f.now = start.Add(time.Hour)
			stats := f.engine.FreshRecentChannelStats()
			assert.Equal(t, f.now.Unix(), stats.RefreshedAt)
			assert.Equal(t, start.Unix(), stats.CollectedSince)
			if tc.segment < 0 {
				assert.Empty(t, stats.Items, "an attempt exactly one hour old expires even without new traffic")
				return
			}
			require.Len(t, stats.Items, 1)
			row := stats.Items[0]
			assert.EqualValues(t, 1, row.Requests1h)
			require.NotNil(t, row.SuccessRate1h)
			assert.Equal(t, 1.0, *row.SuccessRate1h)
			require.Len(t, row.History, 6)
			for i, segment := range row.History {
				assert.Equal(t, start.Unix()+int64(i)*600, segment.StartTime)
				assert.Equal(t, start.Unix()+int64(i+1)*600, segment.EndTime)
				if i == tc.segment {
					assert.EqualValues(t, 1, segment.Requests)
					require.NotNil(t, segment.SuccessRate)
					assert.Equal(t, 1.0, *segment.SuccessRate)
				} else {
					assert.Zero(t, segment.Requests)
					assert.Nil(t, segment.SuccessRate, "no observations differs from zero percent success")
				}
			}
			if tc.inTenMin {
				assert.EqualValues(t, 1, row.Requests)
			} else {
				assert.Zero(t, row.Requests)
				assert.Nil(t, row.SuccessRate)
			}
			if tc.inFiveMin {
				assert.EqualValues(t, 1, row.TTFTSamples5m)
				assert.Equal(t, 2000.0, row.TTFTSumMS5m)
				require.NotNil(t, row.AvgTTFTMS5m)
				assert.Equal(t, 2000.0, *row.AvgTTFTMS5m)
			} else {
				assert.Zero(t, row.TTFTSamples5m)
				assert.Nil(t, row.AvgTTFTMS5m)
			}
		})
	}
}

func TestRecentChannelLatencyAggregationFilteringAndSnapshotIsolation(t *testing.T) {
	f := newSchedulerFixture()
	f.engine.FreshRecentChannelStats()
	f.dispatch(t, f.candidate, true, Outcome{Success: true, TTFT: latency(time.Second)})
	first := f.engine.FreshRecentChannelStats()
	require.Len(t, first.Items, 1)
	assert.EqualValues(t, 1, first.Items[0].Requests)
	f.key = Key{Group: "vip", Model: "other-model"}
	for _, duration := range []time.Duration{3 * time.Second, 5 * time.Second} {
		f.dispatch(t, f.candidate, true, Outcome{Success: true, TTFT: latency(duration)})
	}
	for _, duration := range []time.Duration{2 * time.Second, 10 * time.Second} {
		f.dispatch(t, f.candidate, false, Outcome{Success: true, TTFT: latency(duration)})
	}
	f.dispatch(t, f.candidate, true, Outcome{ChannelFailure: true, TTFT: latency(20 * time.Second)})
	for _, duration := range []*time.Duration{nil, latency(0), latency(-time.Second)} {
		f.dispatch(t, f.candidate, true, Outcome{Success: true, TTFT: duration})
	}
	failed := Candidate{ID: 2, Status: 1, Weight: 10, Capacity: 100}
	f.dispatch(t, failed, false, Outcome{ChannelFailure: true})
	stats := f.engine.FreshRecentChannelStats(1, 1, 999)
	require.Len(t, stats.Items, 1)
	row := stats.Items[0]
	assert.EqualValues(t, 9, row.Requests, "a completion invalidates a snapshot even in the same second")
	assert.EqualValues(t, 8, row.Successes)
	assert.EqualValues(t, 3, row.TTFTSamples5m)
	assert.Equal(t, 9000.0, row.TTFTSumMS5m)
	require.NotNil(t, row.AvgTTFTMS5m)
	assert.Equal(t, 3000.0, *row.AvgTTFTMS5m, "model averages are weighted by their sample counts")
	assert.EqualValues(t, 2, row.ResponseSamples5m)
	assert.Equal(t, 12000.0, row.ResponseSumMS5m)
	require.NotNil(t, row.AvgResponseMS5m)
	assert.Equal(t, 6000.0, *row.AvgResponseMS5m, "non-stream response latency remains separate from stream TTFT")
	assert.Empty(t, f.engine.FreshRecentChannelStats(999).Items)
	all := f.engine.FreshRecentChannelStats()
	require.Len(t, all.Items, 2)
	require.NotNil(t, all.Items[1].SuccessRate1h)
	assert.Zero(t, *all.Items[1].SuccessRate1h)
	assert.Nil(t, all.Items[1].AvgTTFTMS5m)
	assert.Nil(t, all.Items[1].AvgResponseMS5m)
	stats.Items[0].Requests = 999
	stats.Items[0].History[5].Requests = 999
	for _, rate := range []*float64{stats.Items[0].SuccessRate, stats.Items[0].SuccessRate1h, stats.Items[0].AvgTTFTMS5m, stats.Items[0].AvgResponseMS5m, stats.Items[0].History[5].SuccessRate} {
		require.NotNil(t, rate)
		*rate = 999
	}
	assert.Equal(t, all, f.engine.FreshRecentChannelStats(), "callers cannot alter cached history, rates, or latency values")
}

func TestRecentChannelHistorySurvivesMembershipChangesAndIdleLearningReset(t *testing.T) {
	f := newSchedulerFixture()
	f.engine.ReplaceCandidateMembership(map[Key][]Candidate{f.key: {f.candidate}})
	r := f.engine.BeginRequest(f.key, "removed-before-completion", false)
	a, err := f.engine.ReserveCandidate(r, f.candidate, true)
	require.NoError(t, err)
	f.engine.StartAttempt(a)
	f.engine.ReplaceCandidateMembership(map[Key][]Candidate{})
	f.engine.FinishAttempt(a, Outcome{Success: true, TTFT: latency(time.Second)})
	f.engine.FinishAttempt(a, Outcome{Success: true, TTFT: latency(time.Second)})
	f.engine.EndRequest(r)
	stats := f.engine.FreshRecentChannelStats()
	require.Len(t, stats.Items, 1)
	assert.EqualValues(t, 1, stats.Items[0].Requests1h, "removed membership and an old learning epoch retain one completed observation")
	f.now = f.now.Add(31 * time.Minute)
	assert.False(t, f.engine.IsActive(f.key))
	stats = f.engine.FreshRecentChannelStats()
	require.Len(t, stats.Items, 1)
	assert.EqualValues(t, 1, stats.Items[0].Requests1h, "learning/history pruning at thirty minutes does not erase hour observations")
	assert.Zero(t, stats.Items[0].Requests)
	assert.Nil(t, stats.Items[0].AvgTTFTMS5m)
	f.now = f.now.Add(29 * time.Minute)
	f.engine.RefreshRecentChannelStats() // background cleanup works without any new request
	assert.Empty(t, f.engine.RecentChannelStats().Items)
	assert.Empty(t, f.engine.FreshRecentChannelStats().Items)
}

func TestChannelObservationLockDoesNotHoldRoutingCapacity(t *testing.T) {
	f := newSchedulerFixture()
	r := f.engine.BeginRequest(f.key, "finishing", false)
	candidate := f.candidate
	candidate.Capacity = 1
	a, err := f.engine.ReserveCandidate(r, candidate, true)
	require.NoError(t, err)
	f.engine.StartAttempt(a)
	finishing := make(chan struct{})
	allowFinish := make(chan struct{})
	var clockOnce sync.Once
	f.engine.now = func() time.Time {
		clockOnce.Do(func() {
			close(finishing) // FinishAttempt is inside the routing critical section.
			<-allowFinish
		})
		return f.now
	}
	f.engine.channelObservations.mu.Lock()
	var releaseObservations sync.Once
	var workers sync.WaitGroup
	t.Cleanup(func() {
		releaseObservations.Do(f.engine.channelObservations.mu.Unlock)
		workers.Wait()
	})
	workers.Go(func() { f.engine.FinishAttempt(a, Outcome{Success: true}) })
	<-finishing
	close(allowFinish)
	reserved := make(chan error, 1)
	workers.Go(func() {
		next := f.engine.BeginRequest(f.key, "next", false)
		_, reserveErr := f.engine.ReserveCandidate(next, candidate, true)
		f.engine.EndRequest(next)
		reserved <- reserveErr
	})
	select {
	case reserveErr := <-reserved:
		require.NoError(t, reserveErr, "a blocked observation writer must already release routing capacity")
	case <-time.After(5 * time.Second):
		t.Fatal("observation reading blocked routing")
	}
	releaseObservations.Do(f.engine.channelObservations.mu.Unlock)
	workers.Wait()
	f.engine.EndRequest(r)
	stats := f.engine.FreshRecentChannelStats()
	require.Len(t, stats.Items, 1)
	assert.EqualValues(t, 1, stats.Items[0].Requests)
	assert.Zero(t, f.engine.Snapshot(f.key, []Candidate{candidate}, true).Summary.InFlight)
}

func TestConcurrentChannelObservationsAndUIReadsPreserveAccountingAndSelection(t *testing.T) {
	f := newSchedulerFixture()
	other := f.candidate
	other.ID = 2
	other.Priority = 10
	var attempts []*Attempt
	var requests []*Request
	for _, candidate := range []Candidate{f.candidate, other} {
		r := f.engine.BeginRequest(f.key, "", false)
		a, err := f.engine.ReserveCandidate(r, candidate, true)
		require.NoError(t, err)
		f.engine.StartAttempt(a)
		requests = append(requests, r)
		attempts = append(attempts, a)
	}
	var workers sync.WaitGroup
	for range 2 {
		workers.Go(func() { f.engine.FinishAttempt(attempts[0], Outcome{Success: true, TTFT: latency(time.Second)}) })
	}
	workers.Go(func() { f.engine.FinishAttempt(attempts[1], Outcome{ChannelFailure: true}) })
	for range 2 {
		workers.Go(func() {
			stats := f.engine.FreshRecentChannelStats()
			for i := range stats.Items {
				stats.Items[i].History[5].Requests = 999 // Readers own independent snapshots.
			}
		})
	}
	workers.Wait()
	for _, r := range requests {
		f.engine.EndRequest(r)
	}
	stats := f.engine.FreshRecentChannelStats()
	require.Len(t, stats.Items, 2)
	assert.EqualValues(t, 1, stats.Items[0].Requests)
	assert.EqualValues(t, 1, stats.Items[0].TTFTSamples5m)
	assert.EqualValues(t, 1, stats.Items[1].Requests)
	assert.Zero(t, stats.Items[1].Successes)
	candidates := []Candidate{f.candidate, other}
	before := f.engine.Snapshot(f.key, candidates, true)
	f.engine.FreshRecentChannelStats()
	f.engine.RefreshRecentChannelStats()
	assert.Equal(t, before, f.engine.Snapshot(f.key, candidates, true), "UI reads must not alter probabilities, recovery, maturity, or routing samples")
}

func TestRecoverySurvivesOccasionalFailuresRegardlessOfReadTiming(t *testing.T) {
	var finalHealth []float64
	for _, failureFirst := range []bool{false, true} {
		for _, readEveryCompletion := range []bool{false, true} {
			t.Run(fmt.Sprintf("failure-first-%v-frequent-reads-%v", failureFirst, readEveryCompletion), func(t *testing.T) {
				f := newSchedulerFixture()
				for range 100 {
					f.dispatch(t, f.candidate, true, Outcome{ChannelFailure: true})
				}
				assert.Equal(t, 1.0, f.engine.Snapshot(f.key, []Candidate{f.candidate}, true).Channels[0].HealthScore)
				// This reproduces the reported 32-minute pattern, including a
				// failure completing after 99 successes in every minute.
				for range 32 {
					f.now = f.now.Add(time.Minute)
					for attempt := range 100 {
						failed := (!failureFirst && attempt == 99) || (failureFirst && attempt == 0)
						f.dispatch(t, f.candidate, true, Outcome{Success: !failed, ChannelFailure: failed})
						if readEveryCompletion {
							f.engine.Snapshot(f.key, []Candidate{f.candidate}, true)
						}
					}
				}
				row := f.engine.Snapshot(f.key, []Candidate{f.candidate}, true).Channels[0]
				require.NotNil(t, row.SuccessRate30m)
				assert.InDelta(t, .99, *row.SuccessRate30m, 1e-9)
				assert.Greater(t, row.HealthScore, 95.0)
				finalHealth = append(finalHealth, row.HealthScore)
			})
		}
	}
	for _, health := range finalHealth[1:] {
		assert.InDelta(t, finalHealth[0], health, 1e-9, "dashboard frequency and incidental completion order cannot erase earned recovery")
	}
}

func TestRecoveryNeedsNewSuccessAfterErrorExpiry(t *testing.T) {
	f := newSchedulerFixture()
	for range 100 {
		f.dispatch(t, f.candidate, true, Outcome{ChannelFailure: true})
	}
	for range 4 {
		f.now = f.now.Add(8 * time.Minute)
		request := f.engine.BeginRequest(f.key, "", false)
		f.engine.EndRequest(request)
		row := f.engine.Snapshot(f.key, []Candidate{f.candidate}, true).Channels[0]
		assert.InDelta(t, 1, row.HealthScore, 1e-9, "time and reads alone cannot restore health")
	}
	row := f.engine.Snapshot(f.key, []Candidate{f.candidate}, true).Channels[0]
	assert.Zero(t, row.Window30m.Failures)
	assert.Greater(t, row.HealthBaseline, row.HealthScore)
	f.dispatch(t, f.candidate, true, Outcome{Success: true})
	row = f.engine.Snapshot(f.key, []Candidate{f.candidate}, true).Channels[0]
	assert.InDelta(t, 3, row.HealthScore, 1e-9, "one success consumes its two-point contribution immediately")
	assert.Equal(t, row.HealthScore, f.engine.Snapshot(f.key, []Candidate{f.candidate}, true).Channels[0].HealthScore)
	f.engine.ClearPenalty(f.key, f.candidate.ID)
	cleared := f.engine.Snapshot(f.key, []Candidate{f.candidate}, true).Channels[0]
	assert.Equal(t, cleared.HealthBaseline, cleared.HealthScore)
	assert.Equal(t, row.Window30m, cleared.Window30m)
}

func TestNewChannelRampInMatureDimension(t *testing.T) {
	f := newSchedulerFixture()
	for range 100 {
		f.dispatch(t, f.candidate, true, Outcome{Success: true, TTFT: latency(20 * time.Second)})
	}
	newcomer := Candidate{ID: 2, Name: "New", Status: 1, Priority: -10, Weight: 0, Capacity: 100}
	candidates := []Candidate{f.candidate, newcomer}
	snapshot := f.engine.Snapshot(f.key, candidates, true)
	assert.Equal(t, "dynamic", snapshot.Phase)
	assert.InDelta(t, .05, snapshot.Channels[1].SelectionProbability, 1e-9)
	assert.True(t, snapshot.Channels[1].RampLimited)
	assert.Zero(t, snapshot.Channels[1].RampSuccesses)
	assert.Nil(t, snapshot.Channels[1].AvgTTFTMS5m)
	previous := snapshot.Channels[1].SelectionProbability
	for successes := range 20 {
		f.dispatch(t, newcomer, true, Outcome{Success: true, TTFT: latency(time.Millisecond)})
		snapshot = f.engine.Snapshot(f.key, candidates, true)
		row := snapshot.Channels[1]
		assert.GreaterOrEqual(t, row.SelectionProbability, previous)
		assert.InDelta(t, 1, snapshot.Channels[0].SelectionProbability+row.SelectionProbability, 1e-9)
		assert.EqualValues(t, successes+1, row.RampSuccesses)
		assert.Equal(t, successes < 19, row.RampLimited)
		previous = row.SelectionProbability
	}
	assert.Greater(t, previous, .75, "bounded priority preference still gives a substantially faster mature channel most traffic")

	// Removing and returning a channel starts its own ramp while retaining
	// past dashboard outcomes, and does not reset the mature group.
	f.engine.RegisterCandidates(f.key, []Candidate{f.candidate})
	f.engine.RegisterCandidates(f.key, candidates)
	snapshot = f.engine.Snapshot(f.key, candidates, true)
	assert.True(t, snapshot.Active)
	assert.EqualValues(t, 20, snapshot.Channels[1].Window30m.Successes)
	assert.Zero(t, snapshot.Channels[1].RampSuccesses)
	assert.InDelta(t, .05, snapshot.Channels[1].SelectionProbability, 1e-9)
}

func TestPriorityCannotBenefitUnprovenNewChannel(t *testing.T) {
	f := newSchedulerFixture()
	for range 100 {
		f.dispatch(t, f.candidate, true, Outcome{Success: true, TTFT: latency(20 * time.Second)})
	}
	newcomer := Candidate{ID: 2, Name: "New", Status: 1, Priority: math.MaxInt64, Weight: 0, Capacity: 100}
	for _, successes := range []int{0, 19} {
		for range successes {
			f.dispatch(t, newcomer, true, Outcome{Success: true, TTFT: latency(time.Millisecond)})
		}
		controlNewcomer := newcomer
		controlNewcomer.Priority = f.candidate.Priority
		control := f.engine.Snapshot(f.key, []Candidate{f.candidate, controlNewcomer}, true)
		snapshot := f.engine.Snapshot(f.key, []Candidate{f.candidate, newcomer}, true)
		assert.EqualValues(t, successes, snapshot.Channels[1].RampSuccesses)
		assert.True(t, snapshot.Channels[1].RampLimited)
		assert.InDelta(t, control.Channels[1].EffectiveWeight, snapshot.Channels[1].EffectiveWeight, 1e-9,
			"priority cannot reward a new channel before it proves successful business routing")
		assert.InDelta(t, control.Channels[1].SelectionProbability, snapshot.Channels[1].SelectionProbability, 1e-9)
		if successes == 0 {
			assert.InDelta(t, .05, snapshot.Channels[1].SelectionProbability, 1e-9)
		}
	}
}

func TestNewChannelRampSharesInitialBudgetAndKeepsFallbackAvailable(t *testing.T) {
	f := newSchedulerFixture()
	for range 100 {
		f.dispatch(t, f.candidate, true, Outcome{Success: true, TTFT: latency(time.Millisecond)})
	}
	newcomer := Candidate{ID: 2, Name: "New", Status: 1, Weight: 0, Capacity: 1}
	other := Candidate{ID: 3, Name: "Other", Status: 1, Weight: 0, Capacity: 100}
	candidates := []Candidate{f.candidate, newcomer, other}
	snapshot := f.engine.Snapshot(f.key, candidates, true)
	assert.InDelta(t, .05, snapshot.Channels[1].SelectionProbability+snapshot.Channels[2].SelectionProbability, 1e-9)
	assert.InDelta(t, .025, snapshot.Channels[1].SelectionProbability, 1e-9)
	for range 40 {
		f.dispatch(t, newcomer, true, Outcome{ChannelFailure: true})
	}
	snapshot = f.engine.Snapshot(f.key, candidates, true)
	assert.Less(t, snapshot.Channels[1].SelectionProbability, .025, "ramp cap cannot turn into a failing channel's traffic floor")
	assert.Zero(t, snapshot.Channels[1].RampSuccesses)

	unavailable := f.candidate
	unavailable.Status = 2
	other.CooldownUntil = f.now.Add(time.Minute)
	snapshot = f.engine.Snapshot(f.key, []Candidate{unavailable, newcomer, other}, true)
	assert.Equal(t, 1.0, snapshot.Channels[1].SelectionProbability, "no healthy mature alternative means the ramp cannot deny service")
	assert.False(t, snapshot.Channels[1].RampLimited)
	request := f.engine.BeginRequest(f.key, "fallback", false)
	attempt, err := f.engine.SelectAndReserve(request, []Candidate{unavailable, newcomer, other}, true, false)
	require.NoError(t, err)
	assert.Equal(t, newcomer.ID, attempt.Candidate.ID)
	full := f.engine.Snapshot(f.key, []Candidate{unavailable, newcomer, other}, true)
	assert.Equal(t, "saturated", full.Channels[1].RouteState)
	assert.Zero(t, full.Channels[1].SelectionProbability)
	f.engine.EndRequest(request)
}

func TestFailureCooldownUsesQualifiedFailuresAndSurvivesAdministrativeActions(t *testing.T) {
	f := newSchedulerFixture()
	for _, outcome := range []Outcome{
		{ChannelFailure: true, CooldownFailure: true},
		{Success: true},
		{ChannelFailure: true, CooldownFailure: true},
		{ChannelFailure: true}, // an ordinary failure breaks the availability streak
		{ChannelFailure: true, CooldownFailure: true},
		{}, // ignored client mistakes/cancellations cannot be the third failure
		{ChannelFailure: true, CooldownFailure: true},
	} {
		f.dispatch(t, f.candidate, true, outcome)
		assert.NotEqual(t, "cooling", f.engine.Snapshot(f.key, []Candidate{f.candidate}, true).Channels[0].RouteState)
	}
	f.dispatch(t, f.candidate, true, Outcome{ChannelFailure: true, CooldownFailure: true})
	row := f.engine.Snapshot(f.key, []Candidate{f.candidate}, true).Channels[0]
	assert.Equal(t, "cooling", row.RouteState)
	require.NotNil(t, row.CooldownUntil)
	assert.Equal(t, f.now.Add(15*time.Second), *row.CooldownUntil)
	f.engine.ClearPenalty(f.key, f.candidate.ID)
	f.engine.RegisterCandidates(f.key, nil)
	f.engine.RegisterCandidates(f.key, []Candidate{f.candidate})
	request := f.engine.BeginRequest(f.key, "cooling", false)
	_, err := f.engine.SelectAndReserve(request, []Candidate{f.candidate}, true, true)
	assert.ErrorIs(t, err, ErrNoEligibleChannel)
	_, err = f.engine.ReserveCandidate(request, f.candidate, true)
	assert.ErrorIs(t, err, ErrNoEligibleChannel)
	assert.Equal(t, "cooling", f.engine.Snapshot(f.key, []Candidate{f.candidate}, true).Channels[0].RouteState)
	f.now = f.now.Add(15 * time.Second)
	attempt, err := f.engine.ReserveCandidate(request, f.candidate, true)
	require.NoError(t, err, "expiry permits fresh business evidence without a background probe")
	f.engine.StartAttempt(attempt)
	f.engine.FinishAttempt(attempt, Outcome{Success: true})
	f.engine.EndRequest(request)
	f.candidate.Status = 2
	f.now = f.now.Add(31 * time.Minute)
	row = f.engine.Snapshot(f.key, []Candidate{f.candidate}, true).Channels[0]
	assert.Equal(t, "disabled", row.RouteState, "idle reset and cooldown expiry cannot enable a channel")
	assert.Zero(t, row.SelectionProbability)
}

func TestFailureCooldownStreakExpiresAndIdleResetPreservesExternalCooldown(t *testing.T) {
	f := newSchedulerFixture()
	for range 2 {
		f.dispatch(t, f.candidate, true, Outcome{ChannelFailure: true, CooldownFailure: true})
	}
	f.now = f.now.Add(time.Minute + time.Second)
	f.dispatch(t, f.candidate, true, Outcome{ChannelFailure: true, CooldownFailure: true})
	assert.NotEqual(t, "cooling", f.engine.Snapshot(f.key, []Candidate{f.candidate}, true).Channels[0].RouteState)
	f.candidate.CooldownUntil = f.now.Add(time.Hour)
	f.now = f.now.Add(31 * time.Minute)
	row := f.engine.Snapshot(f.key, []Candidate{f.candidate}, true).Channels[0]
	assert.Equal(t, "cooling", row.RouteState)
	assert.Zero(t, row.SelectionProbability)
}

func TestAuthoritativeMembershipRemovalPoolChangeAndOldLeases(t *testing.T) {
	f := newSchedulerFixture()
	vip := Key{Group: "vip", Model: f.key.Model}
	otherModel := Key{Group: "default", Model: "removed-model"}
	high := f.candidate
	high.Capacity = 10
	low := high
	low.Capacity = 1
	for _, removedKey := range []Key{vip, otherModel} {
		t.Run(removedKey.Group+"-"+removedKey.Model, func(t *testing.T) {
			f.engine.ReplaceCandidateMembership(map[Key][]Candidate{f.key: {high}, removedKey: {low}})
			request := f.engine.BeginRequest(removedKey, "old", false)
			old, err := f.engine.ReserveCandidate(request, low, true)
			require.NoError(t, err)
			f.engine.StartAttempt(old)
			f.engine.ReplaceCandidateMembership(map[Key][]Candidate{f.key: {high}})
			newRequest := f.engine.BeginRequest(f.key, "current", false)
			current, err := f.engine.ReserveCandidate(newRequest, high, true)
			require.NoError(t, err, "removed group/model must no longer impose its capacity of one")
			assert.Equal(t, 10, current.Candidate.Capacity)
			_, err = f.engine.SelectAndReserve(request, []Candidate{low}, true, false)
			assert.ErrorIs(t, err, ErrNoEligibleChannel, "stale candidate lists cannot resurrect removed metadata")
			f.engine.ReplaceCandidateMembership(map[Key][]Candidate{f.key: {high}, removedKey: {low}})
			f.engine.FinishAttempt(old, Outcome{Success: true})
			row := f.engine.Snapshot(removedKey, []Candidate{low}, true).Channels[0]
			assert.Zero(t, row.RampSuccesses, "old-epoch completions retain history but cannot mature a returned channel")
			assert.GreaterOrEqual(t, row.Window30m.Successes, int64(1))
			f.engine.EndRequest(request)
			f.engine.EndRequest(newRequest)
		})
	}

	f.engine.ReplaceCandidateMembership(map[Key][]Candidate{f.key: {high}})
	request := f.engine.BeginRequest(f.key, "pool-change", false)
	old, err := f.engine.ReserveCandidate(request, high, true)
	require.NoError(t, err)
	moved := high
	moved.CapacityKey = "replacement-pool"
	f.engine.ReplaceCandidateMembership(map[Key][]Candidate{f.key: {moved}})
	staleRequest := f.engine.BeginRequest(f.key, "stale", false)
	current, err := f.engine.ReserveCandidate(staleRequest, high, true)
	require.NoError(t, err)
	assert.Equal(t, "replacement-pool", current.Candidate.CapacityKey, "stale metadata cannot change the canonical shared pool")
	f.engine.ReplaceCandidateMembership(nil) // deleted channel
	_, err = f.engine.ReserveCandidate(staleRequest, high, true)
	assert.ErrorIs(t, err, ErrNoEligibleChannel)
	f.engine.FinishAttempt(old, Outcome{})
	f.engine.FinishAttempt(old, Outcome{})
	f.engine.EndRequest(request)
	f.engine.EndRequest(staleRequest)
	assert.Zero(t, f.engine.pools[old.pool].inFlight)
	assert.Zero(t, f.engine.pools[current.pool].inFlight)
	assert.Empty(t, f.engine.pools[old.pool].members)
	assert.Empty(t, f.engine.pools[current.pool].members)
}

func TestPinnedReservationOutsideAbilityDoesNotCreateMembership(t *testing.T) {
	f := newSchedulerFixture()
	f.engine.ReplaceCandidateMembership(map[Key][]Candidate{f.key: {f.candidate}})
	outside := Key{Group: "original-task-group", Model: "original-task-model"}
	request := f.engine.BeginRequest(outside, "origin-task", false)
	attempt, err := f.engine.ReserveCandidate(request, f.candidate, true)
	require.NoError(t, err, "native origin-task/fixed channel bindings can bypass current ability membership")
	assert.Len(t, f.engine.pools[attempt.pool].members, 1, "a fixed reservation cannot become a global capacity member")
	_, err = f.engine.SelectAndReserve(request, []Candidate{f.candidate}, true, false)
	assert.ErrorIs(t, err, ErrNoEligibleChannel, "the fixed reservation does not authorize ordinary routing in its group/model")
	f.engine.EndRequest(request)
	f.candidate.Status = 2
	f.engine.ReplaceCandidateMembership(map[Key][]Candidate{f.key: {f.candidate}})
	request = f.engine.BeginRequest(outside, "disabled-origin-task", false)
	stale := f.candidate
	stale.Status = 1
	_, err = f.engine.ReserveCandidate(request, stale, true)
	assert.ErrorIs(t, err, ErrNoEligibleChannel, "fixed routing must still honor current channel disablement")
	f.engine.EndRequest(request)
}

func TestProbesDoNotSupplyBusinessLearningOrCooldownEvidence(t *testing.T) {
	f := newSchedulerFixture()
	for range 2 {
		f.dispatch(t, f.candidate, true, Outcome{ChannelFailure: true, CooldownFailure: true})
	}
	for _, outcome := range []Outcome{{Success: true}, {ChannelFailure: true, CooldownFailure: true}} {
		probe := f.engine.BeginRequest(f.key, "probe", true)
		attempt, err := f.engine.ReserveCandidate(probe, f.candidate, true)
		require.NoError(t, err)
		f.engine.StartAttempt(attempt)
		f.engine.FinishAttempt(attempt, outcome)
		f.engine.EndRequest(probe)
	}
	snapshot := f.engine.Snapshot(f.key, []Candidate{f.candidate}, true)
	assert.EqualValues(t, 2, snapshot.Summary.Requests30m)
	assert.EqualValues(t, 2, snapshot.Summary.Attempts30m)
	assert.EqualValues(t, 2, snapshot.Channels[0].Dispatches30m)
	assert.Zero(t, snapshot.Channels[0].RampSuccesses)
	assert.NotEqual(t, "cooling", snapshot.Channels[0].RouteState)
	f.dispatch(t, f.candidate, true, Outcome{ChannelFailure: true, CooldownFailure: true})
	assert.Equal(t, "cooling", f.engine.Snapshot(f.key, []Candidate{f.candidate}, true).Channels[0].RouteState,
		"manual probe success cannot erase a real business failure streak")
}

func TestIdleLearningResetPreservesFreshFailureCooldown(t *testing.T) {
	f := newSchedulerFixture()
	var requests []*Request
	var attempts []*Attempt
	for range 3 {
		request := f.engine.BeginRequest(f.key, "", false)
		attempt, err := f.engine.ReserveCandidate(request, f.candidate, true)
		require.NoError(t, err)
		f.engine.StartAttempt(attempt)
		requests = append(requests, request)
		attempts = append(attempts, attempt)
	}
	f.now = f.now.Add(31 * time.Minute)
	for i, attempt := range attempts {
		f.engine.FinishAttempt(attempt, Outcome{ChannelFailure: true, CooldownFailure: true})
		f.engine.EndRequest(requests[i])
	}
	snapshot := f.engine.Snapshot(f.key, []Candidate{f.candidate}, true)
	assert.False(t, snapshot.Active)
	assert.Zero(t, snapshot.ActivationRequests)
	assert.Equal(t, "cooling", snapshot.Channels[0].RouteState)
	assert.Zero(t, snapshot.Channels[0].SelectionProbability)
	assert.EqualValues(t, 3, snapshot.Summary.Attempts30m, "idle reset preserves failure history")
}

func TestAuthoritativeDashboardKeepsUnavailableHistoryWithoutMembership(t *testing.T) {
	f := newSchedulerFixture()
	f.dispatch(t, f.candidate, true, Outcome{Success: true})
	f.engine.ReplaceCandidateMembership(nil)
	disabled := f.candidate
	disabled.Status = 2
	unknown := Candidate{ID: 2, Name: "Disabled ability", Status: 1, Excluded: true}
	snapshot := f.engine.Snapshot(f.key, []Candidate{disabled, unknown}, true)
	require.Len(t, snapshot.Channels, 2)
	assert.Equal(t, "disabled", snapshot.Channels[0].RouteState)
	assert.Equal(t, "ineligible", snapshot.Channels[1].RouteState)
	assert.EqualValues(t, 1, snapshot.Channels[0].Window30m.Successes)
	assert.EqualValues(t, 1, snapshot.Summary.Successes30m)
	assert.Zero(t, snapshot.Summary.EligibleChannels)
	assert.Empty(t, f.engine.pools["channel:1"].members, "dashboard inspection cannot restore removed capacity limits")

	f.engine.ReplaceCandidateMembership(map[Key][]Candidate{f.key: {f.candidate, unknown}})
	filtered := f.candidate
	filtered.Excluded = true
	f.engine.Snapshot(f.key, []Candidate{filtered}, true)
	row := f.engine.Snapshot(f.key, []Candidate{f.candidate}, true).Channels[0]
	assert.Equal(t, 1.0, row.SelectionProbability, "request filters cannot mutate authoritative ability membership")
}

func TestDimensionTransitionFadesConfiguredWeightAndReenabledChannelRamps(t *testing.T) {
	f := newSchedulerFixture()
	second := Candidate{ID: 2, Name: "Initially low weight", Status: 1, Weight: 1, Capacity: 100}
	for range 25 {
		f.dispatch(t, f.candidate, true, Outcome{Success: true, TTFT: latency(time.Second)})
		f.dispatch(t, second, true, Outcome{Success: true, TTFT: latency(time.Second)})
	}
	candidates := []Candidate{f.candidate, second}
	transition := f.engine.Snapshot(f.key, candidates, true)
	assert.Equal(t, "transition", transition.Phase)
	assert.Greater(t, transition.Channels[0].SelectionProbability, .5)
	assert.Less(t, transition.Channels[0].SelectionProbability, 10.0/11)
	for range 25 {
		f.dispatch(t, f.candidate, true, Outcome{Success: true, TTFT: latency(time.Second)})
		f.dispatch(t, second, true, Outcome{Success: true, TTFT: latency(time.Second)})
	}
	mature := f.engine.Snapshot(f.key, candidates, true)
	assert.Equal(t, "dynamic", mature.Phase)
	assert.InDelta(t, .5, mature.Channels[0].SelectionProbability, 1e-9, "configured weight is not a permanent dynamic multiplier")
	second.Status = 2
	f.engine.RegisterCandidates(f.key, []Candidate{f.candidate, second})
	second.Status = 1
	f.engine.RegisterCandidates(f.key, []Candidate{f.candidate, second})
	row := f.engine.Snapshot(f.key, []Candidate{f.candidate, second}, true).Channels[1]
	assert.Zero(t, row.RampSuccesses)
	assert.True(t, row.RampLimited)
	assert.InDelta(t, .05, row.SelectionProbability, 1e-9)
	assert.EqualValues(t, 50, row.Window30m.Successes, "reenabling retains historical outcomes")
}

func TestExpiryCannotReboundHealthAfterOlderSuccessesExpire(t *testing.T) {
	var results []float64
	for _, observeDip := range []bool{false, true} {
		t.Run(fmt.Sprintf("observe-dip-%v", observeDip), func(t *testing.T) {
			f := newSchedulerFixture()
			for range 100 {
				f.dispatch(t, f.candidate, true, Outcome{Success: true})
			}
			f.now = f.now.Add(4 * time.Minute)
			for range 10 {
				f.dispatch(t, f.candidate, true, Outcome{ChannelFailure: true})
			}
			before := f.engine.Snapshot(f.key, []Candidate{f.candidate}, true).Channels[0]
			// The old successes leave the short window before the newer errors.
			f.now = f.now.Add(2 * time.Minute)
			if observeDip {
				dip := f.engine.Snapshot(f.key, []Candidate{f.candidate}, true).Channels[0]
				assert.Less(t, dip.HealthScore, before.HealthScore)
			}
			f.now = f.now.Add(4 * time.Minute)
			row := f.engine.Snapshot(f.key, []Candidate{f.candidate}, true).Channels[0]
			assert.Zero(t, row.Window5m.Failures)
			assert.Less(t, row.HealthScore, before.HealthScore, "error expiry cannot undo the intervening loss of success evidence")
			results = append(results, row.HealthScore)
		})
	}
	assert.InDelta(t, results[0], results[1], 1e-9, "expiry penalties must not depend on observing the intermediate dip")
}

func TestFixedChannelWithoutCurrentAbilitiesRemainsPinnable(t *testing.T) {
	f := newSchedulerFixture()
	f.engine.ReplaceCandidateMembership(nil, f.candidate)
	request := f.engine.BeginRequest(f.key, "original-task", false)
	attempt, err := f.engine.ReserveCandidate(request, f.candidate, false)
	require.NoError(t, err)
	assert.Empty(t, f.engine.pools[attempt.pool].members)
	f.engine.EndRequest(request)

	excluded := f.candidate
	excluded.Excluded = true
	f.engine.ReplaceCandidateMembership(map[Key][]Candidate{f.key: {excluded}}, f.candidate)
	request = f.engine.BeginRequest(f.key, "excluded-ability", false)
	attempt, err = f.engine.ReserveCandidate(request, f.candidate, false)
	require.NoError(t, err, "a host-authorized fixed channel can bypass an excluded ability")
	assert.Empty(t, f.engine.pools[attempt.pool].members)
	_, err = f.engine.SelectAndReserve(request, []Candidate{f.candidate}, false, false)
	assert.ErrorIs(t, err, ErrNoEligibleChannel, "native pins must not restore ordinary ability eligibility")
	f.engine.EndRequest(request)
}
