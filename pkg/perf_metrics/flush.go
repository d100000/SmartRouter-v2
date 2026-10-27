package perfmetrics

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/setting/perf_metrics_setting"
)

const projectionInterval = 5 * time.Second
const maxRetryEvents = 10000

type collectorProcess struct {
	mu              sync.Mutex
	flushMu         sync.Mutex
	active          sync.WaitGroup
	pending         map[string]model.PerfMetricEvent
	id              string
	started         bool
	closed          bool
	stop            chan struct{}
	done            chan struct{}
	lastErrorAt     int64
	lastSuccessAt   int64
	dropped         int64
	nextMaintenance time.Time
}

var collector = &collectorProcess{pending: make(map[string]model.PerfMetricEvent)}

// Init starts a projection worker on every node, including read-only dashboard
// nodes. Accepted events remain in SQL when any particular process disappears.
func Init() {
	collector.mu.Lock()
	defer collector.mu.Unlock()
	if collector.started {
		return
	}
	collector.id = "collector:" + common.NewRequestId()
	collector.stop, collector.done = make(chan struct{}), make(chan struct{})
	collector.started = true
	go collector.run()
}

func (p *collectorProcess) run() {
	defer close(p.done)
	ticker := time.NewTicker(projectionInterval)
	defer ticker.Stop()
	for {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		if err := Flush(ctx); err != nil {
			common.SysError(common.LogText("performance metric projection failed: %s", err.Error()))
		}
		cancel()
		select {
		case <-p.stop:
			return
		case <-ticker.C:
		}
	}
}

func Record(sample Sample) {
	sample.Model = strings.TrimSpace(sample.Model)
	sample.Group = strings.TrimSpace(sample.Group)
	sample.EventID = strings.TrimSpace(sample.EventID)
	if !perf_metrics_setting.GetSetting().Enabled || sample.Model == "" {
		return
	}
	if sample.EventID == "" {
		sample.EventID = "relay:" + common.NewRequestId()
	}
	if sample.OccurredAt <= 0 {
		sample.OccurredAt = time.Now().Unix()
	}
	if sample.Group == "" {
		sample.Group = "default"
	}
	// Keep dimensions bounded before attempting SQL. Invalid values cannot
	// become a permanently poisonous retry queue entry.
	if utf8.RuneCountInString(sample.Model) > 128 || utf8.RuneCountInString(sample.Group) > 64 || len(sample.EventID) > 96 {
		collector.mu.Lock()
		collector.dropped++
		collector.lastErrorAt = time.Now().Unix()
		collector.mu.Unlock()
		return
	}
	event := model.PerfMetricEvent{
		ID: sample.EventID, ModelName: sample.Model, Group: sample.Group,
		BucketTs: bucketStart(sample.OccurredAt), SampleAt: sample.OccurredAt,
		Success: sample.Success, LatencyMs: max(0, sample.LatencyMs),
		HasTtft: sample.HasTtft && sample.TtftMs >= 0, TtftMs: max(0, sample.TtftMs),
		OutputTokens: max(0, sample.OutputTokens), OutputTokensKnown: sample.OutputTokensKnown,
		GenerationMs: max(0, sample.GenerationMs),
	}
	recordEvent(event)
}

func recordEvent(event model.PerfMetricEvent) {
	p := collector
	p.mu.Lock()
	if p.closed {
		p.dropped++
		p.lastErrorAt = time.Now().Unix()
		p.mu.Unlock()
		return
	}
	p.active.Add(1)
	p.mu.Unlock()
	defer p.active.Done()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	err := model.AcceptPerfMetricEvent(ctx, model.DB, &event)
	cancel()
	p.mu.Lock()
	defer p.mu.Unlock()
	if err == nil {
		delete(p.pending, event.ID)
		p.lastSuccessAt = time.Now().Unix()
		return
	}
	previousError := p.lastErrorAt
	p.lastErrorAt = time.Now().Unix()
	event.AcceptedAt = 0
	if len(p.pending) < maxRetryEvents {
		p.pending[event.ID] = event
	} else if _, exists := p.pending[event.ID]; !exists {
		p.dropped++
	}
	if p.lastErrorAt-previousError >= 10 {
		common.SysError(common.LogText("performance metric acceptance failed; unconfirmed events retained for retry: %s", err.Error()))
	}
}

// WaitForPendingWrites waits for in-flight acceptance. It does not claim that
// failed in-memory retries are durable; Flush/Shutdown report that separately.
func WaitForPendingWrites() {
	collector.active.Wait()
}

// Flush retries unconfirmed samples, stages task markers, then projects durable
// events. Projection and retry continue even when new collection is disabled.
func Flush(ctx context.Context) error {
	p := collector
	p.flushMu.Lock()
	defer p.flushMu.Unlock()
	p.mu.Lock()
	pending := make([]model.PerfMetricEvent, 0, len(p.pending))
	for _, event := range p.pending {
		pending = append(pending, event)
	}
	p.mu.Unlock()
	var firstErr error
	for _, event := range pending {
		if err := model.AcceptPerfMetricEvent(ctx, model.DB, &event); err != nil {
			firstErr = err
			break
		}
		p.mu.Lock()
		delete(p.pending, event.ID)
		p.lastSuccessAt = time.Now().Unix()
		p.mu.Unlock()
	}
	for ctx.Err() == nil {
		staged, stageErr := model.StagePerfMetricTaskEvents(ctx, model.DB)
		projected, projectErr := model.ProjectPerfMetricEvents(ctx, model.DB)
		if stageErr != nil {
			firstErr = stageErr
		}
		if projectErr != nil {
			firstErr = projectErr
		}
		if stageErr != nil || projectErr != nil || staged+projected == 0 {
			break
		}
	}
	if firstErr == nil {
		firstErr = ctx.Err()
	}
	if firstErr == nil && time.Now().After(p.nextMaintenance) {
		cutoff := perf_metrics_setting.GetRetentionCutoff(time.Now())
		if cutoff > 0 {
			if err := model.DeletePerfMetricsBeforeContext(ctx, cutoff); err != nil {
				firstErr = err
			}
			if firstErr == nil {
				firstErr = model.DeletePerfMetricEventsBefore(ctx, model.DB, cutoff)
			}
		}
		p.nextMaintenance = time.Now().Add(time.Duration(perf_metrics_setting.GetFlushIntervalMinutes()) * time.Minute)
	}
	// A busy projection must not consume the heartbeat's own deadline.
	heartbeatCtx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := p.saveHeartbeat(heartbeatCtx, false); firstErr == nil {
		firstErr = err
	}
	return firstErr
}

func (p *collectorProcess) saveHeartbeat(ctx context.Context, stopped bool) error {
	p.mu.Lock()
	if p.id == "" {
		p.mu.Unlock()
		return nil
	}
	state := model.PerfMetricCollector{ID: p.id, HeartbeatAt: time.Now().Unix(), UnpersistedEvents: int64(len(p.pending)), DroppedEvents: p.dropped, LastErrorAt: p.lastErrorAt, LastSuccessAt: p.lastSuccessAt}
	for _, event := range p.pending {
		if state.OldestUnpersistedAt == 0 || event.SampleAt < state.OldestUnpersistedAt {
			state.OldestUnpersistedAt = event.SampleAt
		}
	}
	if stopped {
		state.StoppedAt = state.HeartbeatAt
	}
	p.mu.Unlock()
	return model.SavePerfMetricCollector(ctx, model.DB, state)
}

// Shutdown runs after HTTP producers have drained and before DB shutdown.
// Committed outbox rows need not finish projection here: another node/restart
// can project them. An outstanding unconfirmed retry returns an explicit error.
func Shutdown(ctx context.Context) error {
	p := collector
	p.mu.Lock()
	p.closed = true
	if p.started {
		select {
		case <-p.stop:
		default:
			close(p.stop)
		}
	}
	done := p.done
	p.mu.Unlock()
	if done != nil {
		select {
		case <-done:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	producersDone := make(chan struct{})
	go func() { p.active.Wait(); close(producersDone) }()
	select {
	case <-producersDone:
	case <-ctx.Done():
		return ctx.Err()
	}
	err := Flush(ctx)
	p.mu.Lock()
	unconfirmed := len(p.pending)
	p.mu.Unlock()
	if unconfirmed > 0 {
		return errors.New("performance metric shutdown has unconfirmed samples")
	}
	if heartbeatErr := p.saveHeartbeat(ctx, true); err == nil {
		err = heartbeatErr
	}
	return err
}
