package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/model"
	perfmetrics "github.com/QuantumNous/new-api/pkg/perf_metrics"
	"github.com/QuantumNous/new-api/pkg/scheduler"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const schedulerContextKey = "scheduling_request"

type schedulingRequest struct {
	mu                        sync.Mutex
	id                        string
	requests                  map[scheduler.Key]*scheduler.Request
	coldPriorities            map[scheduler.Key]int64
	currentRequest            *scheduler.Request
	attempt                   *scheduler.Attempt
	confirmed                 bool
	started                   bool
	stream                    bool
	ttft                      *time.Duration
	pending                   []byte
	lineParsed                bool
	eventOutcome              relaycommon.ResponseOutcome
	writtenOutcome            relaycommon.ResponseOutcome
	output                    bool
	writeFailedBeforeTerminal bool
}

func schedulingState(c *gin.Context) *schedulingRequest {
	if value, ok := c.Get(schedulerContextKey); ok {
		return value.(*schedulingRequest)
	}
	state := &schedulingRequest{id: common.GetUUID(), requests: make(map[scheduler.Key]*scheduler.Request), coldPriorities: make(map[scheduler.Key]int64)}
	c.Set(schedulerContextKey, state)
	return state
}

// SchedulerCandidate exposes routing configuration without credentials.
func SchedulerCandidate(channel *model.Channel) scheduler.Candidate {
	return scheduler.Candidate{ID: channel.Id, Name: channel.Name, Status: channel.Status, Priority: channel.GetPriority(), Weight: float64(channel.GetWeight())}
}

func schedulerRequestForGroup(c *gin.Context, group, modelName string) (*schedulingRequest, *scheduler.Request) {
	state := schedulingState(c)
	key := scheduler.Key{Group: group, Model: modelName}
	request := state.requests[key]
	if request == nil {
		request = scheduler.Default.BeginRequest(key, state.id, true)
		state.requests[key] = request
	}
	return state, request
}

func schedulerSelectChannel(c *gin.Context, group, modelName string, retry int, filters []dto.ChannelFilter) (*model.Channel, error) {
	if err := SyncSchedulingConfig(scheduler.Key{Group: group, Model: modelName}); err != nil {
		return nil, err
	}
	channels, err := model.GetSatisfiedChannelCandidates(group, modelName, filters)
	if err != nil || len(channels) == 0 {
		return nil, err
	}
	state, request := schedulerRequestForGroup(c, group, modelName)
	stream := common.GetContextKeyBool(c, constant.ContextKeyIsStream)
	active := scheduler.Default.IsActive(request.Key)
	priorities := make([]int64, 0, len(channels))
	tiers := make(map[int64][]scheduler.Candidate)
	candidates := make([]scheduler.Candidate, 0, len(channels))
	byID := make(map[int]*model.Channel, len(channels))
	for _, channel := range channels {
		priority := channel.GetPriority()
		if _, seen := tiers[priority]; !seen {
			tiers[priority] = nil
			priorities = append(priorities, priority)
		}
		if slices.Contains(c.GetStringSlice("use_channel"), fmt.Sprint(channel.Id)) {
			continue
		}
		candidate := SchedulerCandidate(channel)
		candidates = append(candidates, candidate)
		tiers[priority] = append(tiers[priority], candidate)
		byID[channel.Id] = channel
	}
	if len(candidates) == 0 {
		return nil, nil
	}
	var attempt *scheduler.Attempt
	if active {
		attempt, err = scheduler.Default.SelectAndReserve(request, candidates, stream, retry > 0 || len(c.GetStringSlice("use_channel")) > 0)
	} else {
		slices.Sort(priorities)
		slices.Reverse(priorities)
		start := min(max(retry, 0), len(priorities)-1)
		state.mu.Lock()
		previous, hasPrevious := state.coldPriorities[request.Key]
		state.mu.Unlock()
		if hasPrevious && retry > 0 {
			// A dispatched fallback advances the next retry past its tier.
			// At the final tier preserve native retry clamping, while the
			// engine and request trail still exclude already attempted IDs.
			next := len(priorities) - 1
			for index, priority := range priorities {
				if priority < previous {
					next = index
					break
				}
			}
			start = next
		}
		for _, priority := range priorities[start:] {
			// Eligibility and reservation are atomic. Empty/full/cooling
			// tiers consume no upstream attempt and immediately fall through.
			attempt, err = scheduler.Default.SelectAndReserve(request, tiers[priority], stream, false)
			if !errors.Is(err, scheduler.ErrNoEligibleChannel) {
				break
			}
		}
	}
	if err != nil {
		if errors.Is(err, scheduler.ErrNoEligibleChannel) {
			return nil, nil
		}
		return nil, err
	}
	state.mu.Lock()
	state.attempt, state.started, state.stream, state.ttft, state.pending = attempt, false, stream, nil, nil
	state.lineParsed, state.eventOutcome, state.writtenOutcome = false, relaycommon.ResponseOutcomeUnknown, relaycommon.ResponseOutcomeUnknown
	state.currentRequest = request
	state.mu.Unlock()
	return byID[attempt.Candidate.ID], nil
}

// ReserveSchedulingChannel applies the same availability/capacity gates to
// fixed channels and strict session bindings without choosing a substitute.
func ReserveSchedulingChannel(c *gin.Context, channel *model.Channel, group, modelName string) error {
	if err := SyncSchedulingConfig(); err != nil {
		return err
	}
	if channel == nil || channel.Status != common.ChannelStatusEnabled {
		return scheduler.ErrNoEligibleChannel
	}
	fresh, err := model.CacheGetChannelForRouting(channel.Id)
	if err != nil {
		return err
	}
	if fresh.Status != common.ChannelStatusEnabled {
		return scheduler.ErrNoEligibleChannel
	}
	channel = fresh
	if group == "" || group == "auto" {
		group = common.GetContextKeyString(c, constant.ContextKeyAutoGroup)
		if group == "" {
			group = common.GetContextKeyString(c, constant.ContextKeyUsingGroup)
		}
		if group == "auto" {
			groups := GetRequestAutoGroups(c, common.GetContextKeyString(c, constant.ContextKeyUserGroup))
			for _, candidateGroup := range groups {
				if model.IsChannelEnabledForGroupModel(candidateGroup, modelName, channel.Id) {
					group = candidateGroup
					break
				}
			}
			if group == "auto" && len(groups) > 0 {
				group = groups[0]
			}
			if group != "auto" {
				common.SetContextKey(c, constant.ContextKeyAutoGroup, group)
			}
		}
	}
	if err := SyncSchedulingConfig(scheduler.Key{Group: group, Model: modelName}); err != nil {
		return err
	}
	state, request := schedulerRequestForGroup(c, group, modelName)
	if state.attempt != nil && state.currentRequest == request && state.attempt.Candidate.ID == channel.Id && !state.started {
		return nil
	}
	if state.attempt != nil {
		scheduler.Default.FinishAttempt(state.attempt, scheduler.Outcome{})
	}
	stream := common.GetContextKeyBool(c, constant.ContextKeyIsStream)
	attempt, err := scheduler.Default.ReserveCandidate(request, SchedulerCandidate(channel), stream)
	if err != nil {
		return err
	}
	state.mu.Lock()
	state.attempt, state.started, state.stream, state.ttft, state.pending = attempt, false, stream, nil, nil
	state.lineParsed, state.eventOutcome, state.writtenOutcome = false, relaycommon.ResponseOutcomeUnknown, relaycommon.ResponseOutcomeUnknown
	state.currentRequest = request
	state.mu.Unlock()
	return nil
}

// StartSchedulingAttempt runs after local validation/billing and immediately
// before the upstream attempt. A reservation alone never activates learning.
func StartSchedulingAttempt(c *gin.Context, info *relaycommon.RelayInfo) {
	state := schedulingState(c)
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.attempt == nil || state.started {
		return
	}
	state.coldPriorities[state.currentRequest.Key] = state.attempt.Candidate.Priority
	scheduler.Default.TouchRequest(state.currentRequest)
	state.stream = info.IsStream
	scheduler.Default.StartAttempt(state.attempt, info.IsStream)
	state.started = true
}

func FinishSchedulingAttempt(c *gin.Context, info *relaycommon.RelayInfo, apiErr *types.NewAPIError) {
	state := schedulingState(c)
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.attempt == nil {
		return
	}
	outcome := scheduler.Outcome{}
	if state.started {
		classificationContext := c.Request.Context()
		// HTTP clients can close as soon as the complete non-stream response
		// arrives, while settlement is still finishing. Successful completion
		// with no short/failed writes is not a client-aborted channel attempt.
		if info != nil && !info.IsStream && apiErr == nil && state.output && !state.writeFailedBeforeTerminal {
			classificationContext = context.WithoutCancel(classificationContext)
		}
		classification := perfmetrics.ClassifyRelayOutcome(classificationContext, info, apiErr)
		if state.writeFailedBeforeTerminal {
			classification = perfmetrics.OutcomeIgnored
		} else if info != nil && info.IsStream && state.writtenOutcome != relaycommon.ResponseOutcomeUnknown && apiErr == nil && !info.PerformanceBusinessRejection {
			stream := info.StreamStatus.OutcomeSnapshot()
			// The caller may close after receiving the complete terminal SSE
			// event, before upstream EOF or quota settlement. Only terminal
			// output observed by our writer can override that late cancellation;
			// parsing a terminal upstream event alone is insufficient.
			if state.writtenOutcome == relaycommon.ResponseOutcomeFailed && stream.Response == relaycommon.ResponseOutcomeFailed {
				// Preserve the existing channel-vs-business error classification.
				classification = perfmetrics.ClassifyRelayOutcome(context.WithoutCancel(classificationContext), info, apiErr)
			} else if state.writtenOutcome == relaycommon.ResponseOutcomeCompleted && stream.Response == relaycommon.ResponseOutcomeCompleted && !stream.HasErrors {
				switch stream.EndReason {
				case relaycommon.StreamEndReasonDone, relaycommon.StreamEndReasonEOF, relaycommon.StreamEndReasonClientGone:
					classification = perfmetrics.OutcomeSuccess
				}
			}
		}
		outcome.Success = classification == perfmetrics.OutcomeSuccess
		outcome.ChannelFailure = classification == perfmetrics.OutcomeFailure
		if outcome.ChannelFailure && c.Request.Context().Err() == nil && apiErr != nil {
			outcome.CooldownFailure = apiErr.GetErrorCode() == types.ErrorCodeDoRequestFailed || apiErr.StatusCode == 429 || apiErr.StatusCode == 502 || apiErr.StatusCode == 503 || apiErr.StatusCode == 504
		}
		outcome.TTFT = state.ttft
		if classification != perfmetrics.OutcomeIgnored && !state.confirmed {
			scheduler.Default.ConfirmRequest(state.currentRequest)
			state.confirmed = true
		}
	}
	scheduler.Default.FinishAttempt(state.attempt, outcome)
	state.attempt, state.started = nil, false
}

func EndSchedulingRequest(c *gin.Context) {
	value, ok := c.Get(schedulerContextKey)
	if !ok {
		return
	}
	state := value.(*schedulingRequest)
	state.mu.Lock()
	defer state.mu.Unlock()
	for _, request := range state.requests {
		scheduler.Default.EndRequest(request)
	}
	state.attempt = nil
}

// SchedulingRetryAllowed protects output already sent to the caller and the
// overall request deadline in addition to the existing native retry policy.
func SchedulingRetryAllowed(c *gin.Context) bool {
	return SchedulingRetryStopReason(c) == ""
}

// SchedulingRetryStopReason is deliberately compact for the existing policy
// failure log; it never includes candidate details or request content.
func SchedulingRetryStopReason(c *gin.Context) string {
	if c.Request != nil {
		if err := c.Request.Context().Err(); err != nil {
			if errors.Is(err, context.DeadlineExceeded) {
				return "request_deadline_exceeded"
			}
			return "client_cancelled"
		}
	}
	if c.Writer != nil && c.Writer.Written() && c.Writer.Status() != 101 {
		return "response_started"
	}
	state := schedulingState(c)
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.output {
		return "response_started"
	}
	return ""
}

type schedulingResponseWriter struct {
	gin.ResponseWriter
	ctx *gin.Context
}

func ObserveSchedulingResponse(c *gin.Context) {
	if _, ok := c.Writer.(*schedulingResponseWriter); !ok && c.Writer != nil {
		c.Writer = &schedulingResponseWriter{ResponseWriter: c.Writer, ctx: c}
	}
}

func (w *schedulingResponseWriter) Unwrap() http.ResponseWriter {
	return w.ResponseWriter
}

func (w *schedulingResponseWriter) Write(body []byte) (int, error) {
	n, err := w.ResponseWriter.Write(body)
	if err != nil || n < len(body) {
		state := schedulingState(w.ctx)
		state.mu.Lock()
		// Once the complete terminal has been written successfully, a failed
		// trailing heartbeat or metadata write cannot undo that known result.
		// Earlier failures remain sticky, including a failure on the terminal.
		if state.writtenOutcome == relaycommon.ResponseOutcomeUnknown {
			state.writeFailedBeforeTerminal = true
		}
		state.mu.Unlock()
	}
	if n > 0 {
		ObserveSchedulingOutput(w.ctx, body[:n])
	}
	return n, err
}

func (w *schedulingResponseWriter) WriteString(body string) (int, error) {
	return w.Write([]byte(body))
}

// ObserveSchedulingOutput samples the first useful payload actually emitted.
// Heartbeats, role-only deltas, empty chunks and terminal metadata are excluded.
// The bounded partial-line buffer supports split SSE writes without retaining
// an entire response or any request content in the scheduler.
func ObserveSchedulingOutput(c *gin.Context, body []byte) {
	value, exists := c.Get(schedulerContextKey)
	if !exists || len(body) == 0 {
		return
	}
	state := value.(*schedulingRequest)
	state.mu.Lock()
	defer state.mu.Unlock()
	state.output = true
	if !state.started || state.attempt == nil {
		return
	}
	useful := false
	if !state.stream {
		useful = c.Writer == nil || c.Writer.Status() < 400
	} else if len(state.pending) == 0 && !state.lineParsed && gjson.ValidBytes(body) {
		useful = meaningfulSchedulingPayload(body)
	} else {
		if state.lineParsed {
			// A full JSON data line can be written separately from its newline.
			// Retain only its classification, including for large final events.
			line, rest, found := bytes.Cut(body, []byte{'\n'})
			if len(bytes.TrimSuffix(line, []byte{'\r'})) != 0 {
				state.eventOutcome = relaycommon.ResponseOutcomeUnknown
			}
			if !found {
				return
			}
			state.lineParsed = false
			body = rest
		}
		state.pending = append(state.pending, body...)
		for {
			line, rest, found := bytes.Cut(state.pending, []byte{'\n'})
			if !found {
				break
			}
			state.pending = rest
			useful = state.observeSSELine(line) || useful
		}
		if data, ok := bytes.CutPrefix(state.pending, []byte("data:")); ok && gjson.ValidBytes(bytes.TrimSpace(data)) {
			useful = state.observeSSELine(state.pending) || useful
			state.pending, state.lineParsed = nil, true
		} else if len(state.pending) > 256*1024 {
			state.pending = nil
			state.eventOutcome = relaycommon.ResponseOutcomeUnknown
		}
	}
	if useful && state.ttft == nil {
		latency := time.Since(state.attempt.StartedAt)
		state.ttft = &latency
	}
}

// observeSSELine commits a terminal only after its blank-line delimiter was
// written. A partial terminal cannot turn cancellation into a health sample.
func (state *schedulingRequest) observeSSELine(line []byte) bool {
	line = bytes.TrimSuffix(line, []byte{'\r'})
	if len(line) == 0 {
		if state.eventOutcome == relaycommon.ResponseOutcomeFailed || state.writtenOutcome == relaycommon.ResponseOutcomeUnknown {
			state.writtenOutcome = state.eventOutcome
		}
		state.eventOutcome = relaycommon.ResponseOutcomeUnknown
		return false
	}
	data, ok := bytes.CutPrefix(line, []byte("data:"))
	if !ok {
		return false
	}
	data = bytes.TrimSpace(data)
	state.eventOutcome = relaycommon.ResponseOutcomeUnknown
	if !gjson.ValidBytes(data) {
		return false
	}
	root := gjson.ParseBytes(data)
	kind, status := root.Get("type").String(), root.Get("response.status").String()
	switch {
	case kind == "response.failed", kind == "response.error", kind == "error":
		state.eventOutcome = relaycommon.ResponseOutcomeFailed
	case (kind == "response.completed" || kind == "response.done") && (status == "" || status == "completed") && root.Get("error").Type == gjson.Null && root.Get("response.error").Type == gjson.Null:
		state.eventOutcome = relaycommon.ResponseOutcomeCompleted
	}
	return state.ttft == nil && meaningfulSchedulingPayload(data)
}

func meaningfulSchedulingPayload(body []byte) bool {
	if !gjson.ValidBytes(body) {
		return false
	}
	root := gjson.ParseBytes(body)
	for _, path := range []string{"choices.#.delta.content", "choices.#.delta.reasoning_content", "choices.#.text", "choices.#.delta.tool_calls.#.function.arguments", "choices.#.delta.audio.data", "candidates.#.content.parts.#.text", "candidates.#.content.parts.#.inlineData.data", "delta.text", "delta.thinking", "delta.partial_json", "text", "audio"} {
		if nonemptySchedulingValue(root.Get(path)) {
			return true
		}
	}
	typeName := root.Get("type").String()
	if strings.HasSuffix(typeName, ".delta") && nonemptySchedulingValue(root.Get("delta")) && root.Get("delta").Type == gjson.String {
		return true
	}
	if strings.HasSuffix(typeName, ".completed") && (nonemptySchedulingValue(root.Get("b64_json")) || nonemptySchedulingValue(root.Get("url"))) {
		return true
	}
	return false
}

func nonemptySchedulingValue(value gjson.Result) bool {
	if value.Type == gjson.String {
		return value.String() != ""
	}
	if value.IsArray() {
		for _, item := range value.Array() {
			if nonemptySchedulingValue(item) {
				return true
			}
		}
	}
	return false
}
