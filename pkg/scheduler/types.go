// Package scheduler maintains instance-local, concurrency-safe routing statistics.
// It does not change channel enablement, retry eligibility, or provider billing.
package scheduler

import (
	"errors"
	"sync"
	"time"
)

var (
	ErrNoEligibleChannel = errors.New("scheduler: no eligible channel")
	ErrInvalidRequest    = errors.New("scheduler: request has ended or is invalid")
	ErrInvalidConfig     = errors.New("scheduler: invalid configuration")
)

type Key struct {
	Group string `json:"group"`
	Model string `json:"model"`
}

type Candidate struct {
	ID            int       `json:"channel_id"`
	Name          string    `json:"name"`
	Status        int       `json:"status"`
	Excluded      bool      `json:"excluded,omitempty"`
	Priority      int64     `json:"priority"`
	Weight        float64   `json:"weight"`
	Capacity      int       `json:"capacity"`
	CapacityKey   string    `json:"capacity_key,omitempty"`
	CooldownUntil time.Time `json:"cooldown_until,omitempty"`
}

type Config struct {
	Enabled         bool    `json:"enabled"`
	SuccessTarget   float64 `json:"target_success_rate"`
	LatencyTargetMS float64 `json:"target_ttft_ms"`
	DefaultCapacity int     `json:"default_capacity"`
}

func DefaultConfig() Config {
	return Config{Enabled: true, SuccessTarget: 0.95, LatencyTargetMS: 3000, DefaultCapacity: 100}
}

type Override struct {
	Weight      *float64 `json:"weight,omitempty"`
	Capacity    *int     `json:"capacity,omitempty"`
	CapacityKey string   `json:"capacity_key,omitempty"`
}

// Request represents one logical request, including all of its retries.
// Its exported fields are immutable. All mutation belongs to its Engine.
type Request struct {
	Key        Key
	ID         string
	engine     *Engine
	businessAt time.Time
	confirmed  bool
	ended      bool
	attempted  map[int]bool
	attempts   map[*Attempt]bool
}

// Attempt holds a capacity reservation until FinishAttempt or EndRequest.
// StartAttempt must be called immediately before sending to the upstream.
// In particular, a streaming reservation must live until its stream ends.
type Attempt struct {
	Candidate Candidate
	StartedAt time.Time
	request   *Request
	pool      string
	stream    bool
	started   bool
	finished  bool
}

type Outcome struct {
	Success        bool
	ChannelFailure bool
	TTFT           *time.Duration
}

type Window struct {
	Attempts       int64   `json:"attempts"`
	Successes      int64   `json:"successes"`
	Failures       int64   `json:"failures"`
	LatencySamples int64   `json:"latency_samples"`
	LatencySumMS   float64 `json:"latency_sum_ms"`
	HealthySamples int64   `json:"healthy_samples"`
	HealthSamples  int64   `json:"health_samples"`
}

type ChannelSnapshot struct {
	ChannelID            int      `json:"channel_id"`
	Name                 string   `json:"name"`
	Status               int      `json:"status"`
	RouteState           string   `json:"route_state"`
	TrafficShare         float64  `json:"traffic_share"`
	EffectiveWeight      float64  `json:"effective_weight"`
	ConfiguredWeight     float64  `json:"configured_weight"`
	Priority             int64    `json:"priority"`
	Dispatches30m        int64    `json:"dispatches_30m"`
	SuccessRate30m       *float64 `json:"success_rate_30m"`
	SuccessRate5m        *float64 `json:"success_rate_5m"`
	InFlight             int      `json:"in_flight"`
	Capacity             int      `json:"capacity"`
	CapacityKey          string   `json:"capacity_key"`
	HealthAttainment     *float64 `json:"health_attainment"`
	HealthScore          float64  `json:"health_score"`
	AvgTTFTMS5m          *float64 `json:"avg_ttft_ms_5m"`
	QualityScore         float64  `json:"quality_score"`
	SelectionProbability float64  `json:"selection_probability"`
	CanRecover           bool     `json:"can_recover"`
	Window30m            Window   `json:"window_30m"`
	Window5m             Window   `json:"window_5m"`
}

type Summary struct {
	Requests30m      int64    `json:"requests_30m"`
	Attempts30m      int64    `json:"attempts_30m"`
	Successes30m     int64    `json:"successes_30m"`
	Attempts5m       int64    `json:"attempts_5m"`
	Successes5m      int64    `json:"successes_5m"`
	LatencySamples5m int64    `json:"latency_samples_5m"`
	LatencySumMS5m   float64  `json:"latency_sum_ms_5m"`
	SuccessRate30m   *float64 `json:"success_rate_30m"`
	SuccessRate5m    *float64 `json:"success_rate_5m"`
	AvgTTFTMS5m      *float64 `json:"avg_ttft_ms_5m"`
	HealthyChannels  int      `json:"healthy_channels"`
	EligibleChannels int      `json:"eligible_channels"`
	DegradedChannels int      `json:"degraded_channels"`
	InFlight         int      `json:"in_flight"`
	TopTrafficShare  float64  `json:"top_traffic_share"`
}

type TrendPoint struct {
	Timestamp   int64    `json:"timestamp"`
	Requests    int64    `json:"requests"`
	Attempts    int64    `json:"attempts"`
	SuccessRate *float64 `json:"success_rate"`
	AvgTTFTMS   *float64 `json:"avg_ttft_ms"`
	InFlight    *int     `json:"in_flight"`
}

type Snapshot struct {
	Key                Key               `json:"key"`
	Scope              string            `json:"scope"`
	Config             Config            `json:"config"`
	Active             bool              `json:"active"`
	ActivationRequests int               `json:"activation_requests"`
	Summary            Summary           `json:"summary"`
	Trend              []TrendPoint      `json:"trend"`
	Channels           []ChannelSnapshot `json:"channels"`
}

type Engine struct {
	mu       sync.Mutex
	now      func() time.Time
	random   func() float64
	states   map[Key]*keyState
	pools    map[string]*capacityPool
	sequence uint64
}

var Default = New()
