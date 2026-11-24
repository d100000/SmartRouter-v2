package scheduler

import (
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// These tests pin what the scheduler is for: it distributes traffic by
// observed quality while configured priority, configured weight and session
// stickiness keep their meaning.

type trafficSim struct {
	engine *Engine
	now    time.Time
	seed   uint64
	count  int
}

func newTrafficSim() *trafficSim {
	s := &trafficSim{engine: New(), now: time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC), seed: 7}
	s.engine.now = func() time.Time { return s.now }
	s.engine.random = s.random
	return s
}

func (s *trafficSim) random() float64 {
	s.seed = s.seed*6364136223846793005 + 1442695040888963407
	return float64(s.seed>>11) / float64(1<<53)
}

// request runs one logical request with up to three attempts and returns the
// channels it tried in order.
func (s *trafficSim) request(key Key, candidates []Candidate, fails func(id int) bool) []int {
	s.count++
	r := s.engine.BeginRequest(key, fmt.Sprintf("request-%d", s.count), true)
	defer s.engine.EndRequest(r)
	var tried []int
	for try := range 3 {
		a, err := s.engine.SelectAndReserve(r, candidates, false, try > 0)
		if err != nil {
			break
		}
		tried = append(tried, a.Candidate.ID)
		s.engine.TouchRequest(r)
		s.engine.StartAttempt(a)
		s.engine.ConfirmRequest(r)
		if fails(a.Candidate.ID) {
			s.engine.FinishAttempt(a, Outcome{ChannelFailure: true, CooldownFailure: true})
			continue
		}
		s.engine.FinishAttempt(a, Outcome{Success: true, TTFT: latency(500 * time.Millisecond)})
		break
	}
	return tried
}

// run sends requests at a fixed interval and counts first attempts per
// channel over the last tail requests.
func (s *trafficSim) run(key Key, candidates []Candidate, requests, tail int, interval time.Duration, fails func(id int) bool) map[int]int {
	first := make(map[int]int)
	for i := range requests {
		s.now = s.now.Add(interval)
		if tried := s.request(key, candidates, fails); len(tried) > 0 && i >= requests-tail {
			first[tried[0]]++
		}
	}
	return first
}

func never(int) bool { return false }

var goalsKey = Key{Group: "default", Model: "goals"}

func TestStrictPriorityHoldsAfterActivation(t *testing.T) {
	candidates := []Candidate{
		{ID: 1, Status: 1, Priority: 10, Weight: 1},
		{ID: 2, Status: 1, Priority: 5, Weight: 1},
		{ID: 3, Status: 1, Priority: 0, Weight: 1},
	}
	s := newTrafficSim()
	s.engine.RegisterCandidates(goalsKey, candidates)
	first := s.run(goalsKey, candidates, 3000, 1000, 200*time.Millisecond, never)
	require.True(t, s.engine.IsActive(goalsKey))
	assert.Equal(t, map[int]int{1: 1000}, first, "healthy higher tiers keep every first attempt after activation")

	// The top tier degrades to 40% success without ever failing three
	// requests in a row: the next tier takes most of the traffic, the lowest none.
	degraded := s.run(goalsKey, candidates, 3000, 1000, 200*time.Millisecond, func(id int) bool { return id == 1 && s.random() < 0.6 })
	assert.Greater(t, degraded[2], 700, "the next tier carries what the degraded tier cannot")
	assert.Less(t, degraded[1], 300)
	assert.Zero(t, degraded[3], "the lowest tier stays idle while a healthy tier is above it")

	recovered := s.run(goalsKey, candidates, 6000, 1000, 200*time.Millisecond, never)
	assert.Greater(t, recovered[1], 950, "traffic returns to the top tier once it is healthy again")
}

func TestStrictPriorityFallsThroughUnavailableTier(t *testing.T) {
	primary := Candidate{ID: 1, Status: 1, Priority: 10, Weight: 1}
	backup := Candidate{ID: 2, Status: 1, Priority: 0, Weight: 1}
	candidates := []Candidate{primary, backup}
	s := newTrafficSim()
	s.engine.RegisterCandidates(goalsKey, candidates)
	s.run(goalsKey, candidates, 1000, 0, 200*time.Millisecond, never)
	require.True(t, s.engine.IsActive(goalsKey))

	// A retry excludes the tried primary and must reach the backup.
	tried := s.request(goalsKey, candidates, func(id int) bool { return id == 1 })
	assert.Equal(t, []int{1, 2}, tried)

	disabled := primary
	disabled.Status = 2
	s.engine.RegisterCandidates(goalsKey, []Candidate{disabled, backup})
	first := s.run(goalsKey, []Candidate{disabled, backup}, 100, 100, 200*time.Millisecond, never)
	assert.Equal(t, map[int]int{2: 100}, first, "a disabled tier hands everything to the next one")

	limited := primary
	limited.Capacity = 1
	s.engine.RegisterCandidates(goalsKey, []Candidate{limited, backup})
	holder := s.engine.BeginRequest(goalsKey, "holder", true)
	_, err := s.engine.ReserveCandidate(holder, limited, false)
	require.NoError(t, err)
	first = s.run(goalsKey, []Candidate{limited, backup}, 100, 100, 200*time.Millisecond, never)
	assert.Equal(t, map[int]int{2: 100}, first, "a full tier hands everything to the next one")
	s.engine.EndRequest(holder)
}

func TestNewHighPriorityChannelRampsBeforeTakingOver(t *testing.T) {
	backup := Candidate{ID: 2, Status: 1, Priority: 0, Weight: 1}
	s := newTrafficSim()
	s.engine.RegisterCandidates(goalsKey, []Candidate{backup})
	s.run(goalsKey, []Candidate{backup}, 1000, 0, 200*time.Millisecond, never)
	require.True(t, s.engine.IsActive(goalsKey))

	primary := Candidate{ID: 1, Status: 1, Priority: 10, Weight: 1}
	candidates := []Candidate{primary, backup}
	s.engine.RegisterCandidates(goalsKey, candidates)
	rows := s.engine.Snapshot(goalsKey, candidates, false).Channels
	assert.InDelta(t, 0.05, rows[0].SelectionProbability, 1e-9, "an unproven channel starts with its ramp allowance, whatever its priority")

	first := s.run(goalsKey, candidates, 2000, 1000, 200*time.Millisecond, never)
	assert.Equal(t, map[int]int{1: 1000}, first, "a proven high-priority channel takes the whole tier's traffic")
}

func TestSoftPriorityKeepsEarlierSpread(t *testing.T) {
	candidates := []Candidate{{ID: 1, Status: 1, Priority: 10, Weight: 1}, {ID: 2, Status: 1, Priority: 0, Weight: 1}}
	s := newTrafficSim()
	config := DefaultConfig()
	config.SoftPriority = true
	require.NoError(t, s.engine.SetConfig(goalsKey, config))
	s.engine.RegisterCandidates(goalsKey, candidates)
	first := s.run(goalsKey, candidates, 3000, 1000, 200*time.Millisecond, never)
	assert.InDelta(t, 667, first[1], 60, "soft priority is a two-to-one preference between equal channels")
	assert.InDelta(t, 333, first[2], 60)
}

func TestConfiguredWeightKeepsShapingTheSplit(t *testing.T) {
	candidates := []Candidate{{ID: 1, Status: 1, Weight: 9}, {ID: 2, Status: 1, Weight: 1}}
	s := newTrafficSim()
	s.engine.RegisterCandidates(goalsKey, candidates)
	first := s.run(goalsKey, candidates, 4000, 1000, 200*time.Millisecond, never)
	require.True(t, s.engine.IsActive(goalsKey))
	assert.InDelta(t, 900, first[1], 40, "equal quality keeps the configured nine-to-one split")

	// Quality still moves traffic: the heavier channel starts failing.
	failing := s.run(goalsKey, candidates, 3000, 1000, 200*time.Millisecond, func(id int) bool { return id == 1 && s.random() < 0.5 })
	assert.Greater(t, failing[2], 500, "a failing channel loses share despite its configured weight")

	learned := newTrafficSim()
	config := DefaultConfig()
	config.LearnedWeightOnly = true
	require.NoError(t, learned.engine.SetConfig(goalsKey, config))
	learned.engine.RegisterCandidates(goalsKey, candidates)
	first = learned.run(goalsKey, candidates, 4000, 1000, 200*time.Millisecond, never)
	assert.InDelta(t, 500, first[1], 60, "learned weights only: the configured ratio fades out")
}

func TestZeroWeightBesideWeightedPeerStaysIdle(t *testing.T) {
	candidates := []Candidate{{ID: 1, Status: 1, Weight: 10}, {ID: 2, Status: 1, Weight: 0}}
	s := newTrafficSim()
	s.engine.RegisterCandidates(goalsKey, candidates)
	first := s.run(goalsKey, candidates, 2000, 1000, 200*time.Millisecond, never)
	assert.Equal(t, map[int]int{1: 1000}, first)
	tried := s.request(goalsKey, candidates, func(id int) bool { return id == 1 })
	assert.Equal(t, []int{1, 2}, tried, "a zero-weight channel still serves as the retry of its tier")
}

func TestStickyBindingSurvivesBackgroundErrors(t *testing.T) {
	candidates := []Candidate{{ID: 1, Status: 1, Weight: 1}, {ID: 2, Status: 1, Weight: 1}}
	s := newTrafficSim()
	s.engine.RegisterCandidates(goalsKey, candidates)
	bypassed, samples := 0, 0
	for i := range 5 * 3600 {
		s.now = s.now.Add(200 * time.Millisecond)
		s.request(goalsKey, candidates, func(int) bool { return s.random() < 0.01 })
		if i > 5*600 {
			samples++
			if !s.engine.BindingAdvisable(goalsKey, 1) {
				bypassed++
			}
		}
	}
	assert.Less(t, float64(bypassed)/float64(samples), 0.02, "a 1%% background error rate must not move bound sessions")
}

func TestLowTrafficDimensionCoolsDeadPrimary(t *testing.T) {
	primary := Candidate{ID: 1, Status: 1, Priority: 10, Weight: 1}
	backup := Candidate{ID: 2, Status: 1, Priority: 0, Weight: 1}
	s := newTrafficSim()
	s.engine.RegisterCandidates(goalsKey, []Candidate{primary, backup})
	hitDead, total := 0, 0
	for elapsed := time.Duration(0); elapsed < time.Hour; elapsed += 40 * time.Second {
		s.now = s.now.Add(40 * time.Second)
		s.count++
		total++
		r := s.engine.BeginRequest(goalsKey, fmt.Sprintf("request-%d", s.count), true)
		// The cold-start host walks the tiers from the top.
		if a, err := s.engine.SelectAndReserve(r, []Candidate{primary}, false, false); err == nil {
			hitDead++
			s.engine.TouchRequest(r)
			s.engine.StartAttempt(a)
			s.engine.ConfirmRequest(r)
			s.engine.FinishAttempt(a, Outcome{ChannelFailure: true, CooldownFailure: true})
		}
		a, err := s.engine.SelectAndReserve(r, []Candidate{backup}, false, false)
		require.NoError(t, err)
		s.engine.TouchRequest(r)
		s.engine.StartAttempt(a)
		s.engine.ConfirmRequest(r)
		s.engine.FinishAttempt(a, Outcome{Success: true, TTFT: latency(500 * time.Millisecond)})
		s.engine.EndRequest(r)
	}
	require.False(t, s.engine.IsActive(goalsKey), "45 requests in 30 minutes stay below the activation threshold")
	assert.Less(t, hitDead, total/3, "a dead primary cools even when requests are 40 seconds apart")
	assert.Greater(t, hitDead, 3, "recovery probes still reach the primary")
}

func TestRetriesSpreadOverHealthyPeers(t *testing.T) {
	candidates := []Candidate{{ID: 1, Status: 1, Weight: 1}, {ID: 2, Status: 1, Weight: 1}, {ID: 3, Status: 1, Weight: 1}, {ID: 4, Status: 1, Weight: 1}}
	s := newTrafficSim()
	s.engine.RegisterCandidates(goalsKey, candidates)
	s.run(goalsKey, candidates, 2000, 0, 100*time.Millisecond, never)
	retries := make(map[int]int)
	total := 0
	for range 3000 {
		s.now = s.now.Add(100 * time.Millisecond)
		tried := s.request(goalsKey, candidates, func(id int) bool { return id == 1 || s.random() < 0.03 })
		for _, id := range tried[1:] {
			retries[id]++
			total++
		}
	}
	require.Greater(t, total, 50)
	for id, count := range retries {
		assert.Less(t, float64(count)/float64(total), 0.5, "channel %d must not absorb most retries", id)
	}
}

func TestAbandonedDimensionAgesOutWhilePublicationsContinue(t *testing.T) {
	s := newTrafficSim()
	pools := New()
	pools.now = s.engine.now
	members := []Candidate{{ID: 1, Status: 1, Weight: 1}, {ID: 2, Status: 1, Weight: 1}}
	membership := Membership{Candidates: map[Key][]Candidate{goalsKey: members}, Metadata: members}
	empty := Membership{Candidates: map[Key][]Candidate{}}
	require.NoError(t, s.engine.PublishRoutingMembership(membership, pools, empty, nil))
	abandoned := Key{Group: "default", Model: "goals@x:1"}
	s.engine.RegisterCandidates(abandoned, members)
	s.engine.RegisterCandidates(abandoned, nil)
	for range 6 {
		s.now = s.now.Add(10 * time.Minute)
		require.NoError(t, s.engine.PublishRoutingMembership(membership, pools, empty, nil))
	}
	s.engine.mu.Lock()
	_, kept := s.engine.states[abandoned]
	_, live := s.engine.states[goalsKey]
	s.engine.mu.Unlock()
	assert.False(t, kept, "publishing a generation is not a use of an unregistered dimension")
	assert.True(t, live)
}

func TestHigherPriorityAlternativesForDemotedBinding(t *testing.T) {
	primary := Candidate{ID: 1, Status: 1, Priority: 10, Weight: 1}
	peer := Candidate{ID: 3, Status: 1, Priority: 0, Weight: 1}
	backup := Candidate{ID: 2, Status: 1, Priority: 0, Weight: 1}
	candidates := []Candidate{primary, backup, peer}
	s := newTrafficSim()
	s.engine.RegisterCandidates(goalsKey, candidates)
	healthy, demoted := s.engine.HigherPriorityAlternatives(goalsKey, backup.ID)
	assert.Empty(t, healthy, "a tier without recent successes is not a target")
	assert.True(t, demoted, "but the binding is below it all the same")
	s.run(goalsKey, candidates, 200, 0, 200*time.Millisecond, never)
	healthy, demoted = s.engine.HigherPriorityAlternatives(goalsKey, backup.ID)
	assert.Equal(t, map[int]bool{1: true}, healthy, "peers of the same tier are not a reason to move")
	assert.True(t, demoted)
	healthy, demoted = s.engine.HigherPriorityAlternatives(goalsKey, primary.ID)
	assert.Empty(t, healthy)
	assert.False(t, demoted, "the highest tier has nothing above it")
	healthy, demoted = s.engine.HigherPriorityAlternatives(goalsKey, 99)
	assert.Empty(t, healthy)
	assert.False(t, demoted)

	cooling := primary
	cooling.CooldownUntil = s.now.Add(time.Minute)
	s.engine.RegisterCandidates(goalsKey, []Candidate{cooling, backup, peer})
	healthy, demoted = s.engine.HigherPriorityAlternatives(goalsKey, backup.ID)
	assert.Empty(t, healthy, "a cooling tier is not a target")
	assert.True(t, demoted, "a cooling tier is still above the binding")
	s.engine.RegisterCandidates(goalsKey, candidates)

	config := DefaultConfig()
	config.SoftPriority = true
	require.NoError(t, s.engine.SetConfig(goalsKey, config))
	healthy, demoted = s.engine.HigherPriorityAlternatives(goalsKey, backup.ID)
	assert.Empty(t, healthy, "soft priority never moves a session for priority")
	assert.False(t, demoted)
}

func TestStrictPriorityCapGivesALowerTierOnlyItsSpill(t *testing.T) {
	candidates := []Candidate{{ID: 1, Status: 1, Priority: 10, Weight: 1}, {ID: 2, Status: 1, Priority: 0, Weight: 1}}
	s := newTrafficSim()
	s.engine.RegisterCandidates(goalsKey, candidates)
	s.run(goalsKey, candidates, 2000, 0, 200*time.Millisecond, never)
	require.True(t, s.engine.IsActive(goalsKey))

	// The top tier fails one request in ten: mostly healthy, at times just
	// below the level at which the backup starts to receive its spill.
	smallSpills := 0
	for range 6000 {
		s.now = s.now.Add(200 * time.Millisecond)
		s.request(goalsKey, candidates, func(id int) bool { return id == 1 && s.random() < 0.10 })
		s.engine.mu.Lock()
		evaluated, _ := s.engine.evaluate(goalsKey, s.engine.state(goalsKey, s.now), candidates, s.now, false, nil)
		s.engine.mu.Unlock()
		total, backup := 0.0, evaluatedCandidate{}
		for _, item := range evaluated {
			total += item.weight
			if item.candidate.ID == 2 {
				backup = item
			}
		}
		if len(evaluated) < 2 || total <= 0 {
			continue // The top tier is cooling: the backup carries everything.
		}
		share := backup.weight / total
		if share > 0 && share < 0.1 {
			smallSpills++
		}
		require.InDelta(t, share, backup.probability, 1e-9, "the 90%% cap must not hand a lower tier more than its spill")
	}
	assert.Positive(t, smallSpills, "the scenario must include spills smaller than the cap's tenth")
}
