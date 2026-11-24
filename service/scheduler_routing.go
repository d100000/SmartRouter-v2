package service

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"maps"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	"github.com/QuantumNous/new-api/model"
	perfmetrics "github.com/QuantumNous/new-api/pkg/perf_metrics"
	"github.com/QuantumNous/new-api/pkg/scheduler"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"gorm.io/gorm"
)

const schedulerContextKey = "scheduling_request"
const unsentCredentialRetryChannelKey = "unsent_credential_retry_channel"

type schedulingRequest struct {
	mu                 sync.Mutex
	id                 string
	requests           map[scheduler.Key]*scheduler.Request
	poolEngine         *scheduler.Engine
	poolRequests       map[scheduler.Key]*scheduler.Request
	poolRequest        *scheduler.Request
	poolAttempt        *scheduler.Attempt
	capacityBlocks     []scheduler.Key
	capacityUnresolved bool
	sentSuppliers      map[string]bool
	coldPriorities     map[scheduler.Key]int64
	// retryAgain holds channels the request may use again although it already
	// tried them; sameTier marks keys whose retries stay in the tier of the
	// latest attempt. See retrySameTier.
	retryAgain     map[int]bool
	sameTier       map[scheduler.Key]bool
	currentRequest *scheduler.Request
	attempt        *scheduler.Attempt
	confirmed      bool
	started        bool
	// dispatched counts attempts actually sent upstream. Only after one does
	// selection rank candidates as a retry; a local refusal is not a retry.
	dispatched                int
	stream                    bool
	ttft                      *time.Duration
	pending                   []byte
	lineParsed                bool
	eventOutcome              relaycommon.ResponseOutcome
	writtenOutcome            relaycommon.ResponseOutcome
	output                    bool
	writeFailedBeforeTerminal bool
	lastOutcome               perfmetrics.Outcome
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
	priority := channel.GetPriority()
	if channel.OwnerSupplierID != "" {
		priority = channel.SupplierPriority
	}
	return scheduler.Candidate{ID: channel.Id, Name: channel.Name, Status: channel.Status, Priority: priority, Weight: float64(channel.GetWeight())}
}

func schedulerRequestForGroup(c *gin.Context, group, modelName string) (*schedulingRequest, *scheduler.Request) {
	state := schedulingState(c)
	key := SchedulingKey(group, modelName)
	request := state.requests[key]
	if request == nil {
		request = scheduler.Default.BeginRequest(key, state.id, true)
		state.requests[key] = request
	}
	return state, request
}

func schedulerSelectChannel(c *gin.Context, group, modelName string, retry int, filters []dto.ChannelFilter) (*model.Channel, error) {
	return selectSchedulingCandidates(c, scheduler.Key{Group: group, Model: modelName}, retry, filters, nil)
}

// allowed restricts normal routing to healthy alternatives for a preferred
// binding. Supplier pool and channel reservations still run in their usual order.
// key carries the request's own model name: candidates and filters are
// resolved for it, while the statistics live in its SchedulingKey dimension.
func selectSchedulingCandidates(c *gin.Context, key scheduler.Key, retry int, filters []dto.ChannelFilter, allowed map[int]bool) (selected *model.Channel, selectionErr error) {
	state := schedulingState(c)
	var request *scheduler.Request
	defer func() { state.observeCapacity(request, selected != nil, selectionErr) }()
	if err := ensureSchedulingConfig(false); err != nil {
		return nil, err
	}
	channels, err := model.GetSatisfiedChannelCandidates(key.Group, key.Model, filters)
	if err != nil || len(channels) == 0 {
		return nil, err
	}
	withSuppliers := slices.ContainsFunc(channels, func(channel *model.Channel) bool { return channel.OwnerSupplierID != "" })
	if allowed != nil {
		channels = slices.DeleteFunc(channels, func(channel *model.Channel) bool { return !allowed[channel.Id] })
		if len(channels) == 0 {
			return nil, nil
		}
	}
	_, request = schedulerRequestForGroup(c, key.Group, key.Model)
	credentialRetryChannel := c.GetInt(unsentCredentialRetryChannelKey)
	c.Set(unsentCredentialRetryChannelKey, 0)
	state.mu.Lock()
	// Plugin preparation can reject a provisional candidate. Release its
	// lease before choosing again, including when candidates share capacity.
	state.releaseUnstartedLease()
	retried := state.dispatched > 0
	state.mu.Unlock()
	stream := common.GetContextKeyBool(c, constant.ContextKeyIsStream)
	for {
		if withSuppliers {
			selected, err = selectSupplierPoolChannel(c, state, request, channels, stream, retried)
		} else {
			selected, err = selectChannelRound(c, state, request, channels, retry, stream, retried, credentialRetryChannel)
		}
		// With no untried channel left, the remaining retries go back to the
		// tier of the latest attempt. A restricted choice among healthy
		// alternatives of a session binding never does.
		if selected != nil || err != nil || allowed != nil || !state.retrySameTier(c, request, channels, withSuppliers) {
			return selected, err
		}
	}
}

// selectChannelRound chooses among the channels the request has not used.
func selectChannelRound(c *gin.Context, state *schedulingRequest, request *scheduler.Request, channels []*model.Channel, retry int, stream, retried bool, credentialRetryChannel int) (*model.Channel, error) {
	var err error
	again, sameTier := state.retryState(request.Key)
	active := scheduler.Default.IsActive(request.Key)
	priorities := make([]int64, 0, len(channels))
	tiers := make(map[int64][]scheduler.Candidate)
	candidates := make([]scheduler.Candidate, 0, len(channels))
	byID := make(map[int]*model.Channel, len(channels))
	for _, channel := range channels {
		if !supplierChannelEligible(c, channel) {
			continue
		}
		priority := channel.GetPriority()
		if _, seen := tiers[priority]; !seen {
			tiers[priority] = nil
			priorities = append(priorities, priority)
		}
		if channel.Id != credentialRetryChannel && !again[channel.Id] && slices.Contains(c.GetStringSlice("use_channel"), fmt.Sprint(channel.Id)) {
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
		attempt, err = scheduler.Default.SelectAndReserve(request, candidates, stream, retried)
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
				// Once retries stay in the latest tier, it is not skipped.
				if priority < previous || sameTier && priority == previous {
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

// retryState returns the channels the request may use again and whether the
// retries of this key stay in the tier of its latest attempt.
func (state *schedulingRequest) retryState(key scheduler.Key) (map[int]bool, bool) {
	state.mu.Lock()
	defer state.mu.Unlock()
	return maps.Clone(state.retryAgain), state.sameTier[key]
}

// retrySameTier is called when no untried channel can take the request. The
// request then has retries left but nowhere new to send them: every channel
// of the current tier was tried and no lower tier can take it. The channels
// already tried in the tier of the latest attempt become selectable again, so
// the configured number of retries is used instead of the request ending
// early. A single channel is retried; several are each tried again before any
// is tried a third time. It reports whether any channel was made selectable.
//
// Channels refused before dispatch (a local limit) are not included: they
// were never sent to, and asking again would only repeat the refusal.
func (state *schedulingRequest) retrySameTier(c *gin.Context, request *scheduler.Request, channels []*model.Channel, withSuppliers bool) bool {
	var again []int
	if withSuppliers {
		// The platform engine chooses a supplier pool, the channel engine a
		// channel inside it. The tier is the one of the latest pool.
		state.mu.Lock()
		poolEngine, poolRequest := state.poolEngine, state.poolRequests[request.Key]
		state.mu.Unlock()
		if poolEngine == nil || poolRequest == nil {
			return false
		}
		members := map[int][]int{}
		var pools []scheduler.Candidate
		for _, channel := range channels {
			if !supplierChannelEligible(c, channel) {
				continue
			}
			pool, err := supplierRoutingPoolForChannel(channel)
			if err != nil {
				continue
			}
			if _, seen := members[pool.candidate.ID]; !seen {
				pools = append(pools, pool.candidate)
			}
			members[pool.candidate.ID] = append(members[pool.candidate.ID], channel.Id)
		}
		var tried []int
		for _, id := range poolEngine.RetrySameTier(poolRequest, pools) {
			tried = append(tried, members[id]...)
		}
		again = scheduler.Default.RetryAgain(request, tried)
	} else {
		candidates := make([]scheduler.Candidate, 0, len(channels))
		for _, channel := range channels {
			if supplierChannelEligible(c, channel) {
				candidates = append(candidates, SchedulerCandidate(channel))
			}
		}
		again = scheduler.Default.RetrySameTier(request, candidates)
	}
	if len(again) == 0 {
		return false
	}
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.retryAgain == nil {
		state.retryAgain, state.sameTier = map[int]bool{}, map[scheduler.Key]bool{}
	}
	for _, id := range again {
		state.retryAgain[id] = true
	}
	state.sameTier[request.Key] = true
	return true
}

// refusedBeforeDispatch withdraws a channel from the ones the request may use
// again: it declined the request locally and was not sent to.
func (state *schedulingRequest) refusedBeforeDispatch(channelID int) {
	state.mu.Lock()
	defer state.mu.Unlock()
	delete(state.retryAgain, channelID)
}

// releaseUnstartedLease returns a reservation that was never dispatched.
// Undispatched reservations never count as attempts or health failures.
// The caller holds state.mu.
func (state *schedulingRequest) releaseUnstartedLease() {
	if state.attempt == nil || state.started {
		return
	}
	scheduler.Default.FinishAttempt(state.attempt, scheduler.Outcome{})
	state.attempt, state.currentRequest = nil, nil
	if state.poolAttempt != nil {
		state.poolEngine.FinishAttempt(state.poolAttempt, scheduler.Outcome{})
		state.poolAttempt, state.poolRequest = nil, nil
	}
}

// Accumulate pressure across tiers, groups and supplier pools. A route
// through another group turns earlier full pools into successful bypasses.
func (state *schedulingRequest) observeCapacity(request *scheduler.Request, admitted bool, selectionErr error) {
	state.mu.Lock()
	defer state.mu.Unlock()
	if request != nil {
		channelBlocked := request.TakeCapacityBlock()
		poolBlocked := state.poolRequests[request.Key].TakeCapacityBlock()
		if channelBlocked || poolBlocked {
			state.capacityUnresolved = true
			// A preferred binding may visit its group before auto routing
			// tries earlier groups and then comes back to it.
			if i := slices.Index(state.capacityBlocks, request.Key); i >= 0 {
				state.capacityBlocks = slices.Delete(state.capacityBlocks, i, i+1)
			}
			state.capacityBlocks = append(state.capacityBlocks, request.Key)
		}
	}
	if selectionErr != nil && !errors.Is(selectionErr, scheduler.ErrNoEligibleChannel) {
		state.capacityBlocks = nil
		state.capacityUnresolved = false
		return
	}
	if admitted {
		state.capacityUnresolved = false
	}
}

// ReserveSchedulingChannel applies the same availability/capacity gates to
// fixed channels and strict session bindings without choosing a substitute.
func ReserveSchedulingChannel(c *gin.Context, channel *model.Channel, group, modelName string) (reservationErr error) {
	state := schedulingState(c)
	var request *scheduler.Request
	defer func() { state.observeCapacity(request, reservationErr == nil, reservationErr) }()
	// Callers wrap this error for API clients; never expose database or
	// configuration details through it.
	defer func() { reservationErr = clientSafeSchedulingError(c, reservationErr) }()
	defer func() {
		// A Realtime connection only carries generations, and each generation
		// reserves capacity on its own context. Admitting the socket checks
		// availability and capacity but must not hold a slot while it idles.
		if reservationErr == nil && c.Request != nil && c.Request.URL.Path == "/v1/realtime" && IsRelayWebSocketRequest(c) {
			state.mu.Lock()
			state.releaseUnstartedLease()
			state.mu.Unlock()
		}
	}()
	if err := ensureSchedulingConfig(false); err != nil {
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
	if apiErr := ChannelAvailabilityError(c, fresh); apiErr != nil {
		return apiErr
	}
	channel = fresh
	if !supplierChannelEligible(c, channel) {
		return scheduler.ErrNoEligibleChannel
	}
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
	_, request = schedulerRequestForGroup(c, group, modelName)
	state.mu.Lock()
	if state.attempt != nil && state.currentRequest == request && state.attempt.Candidate.ID == channel.Id && !state.started {
		state.mu.Unlock()
		return nil
	}
	previous, previousPool, previousPoolEngine := state.attempt, state.poolAttempt, state.poolEngine
	state.attempt, state.poolAttempt, state.poolRequest = nil, nil, nil
	state.mu.Unlock()
	if previous != nil {
		scheduler.Default.FinishAttempt(previous, scheduler.Outcome{})
	}
	if previousPool != nil {
		previousPoolEngine.FinishAttempt(previousPool, scheduler.Outcome{})
	}
	stream := common.GetContextKeyBool(c, constant.ContextKeyIsStream)
	if channel.OwnerSupplierID != "" {
		pool, err := supplierRoutingPoolForChannel(channel)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return scheduler.ErrNoEligibleChannel
		}
		if err != nil {
			return err
		}
		poolRequest := supplierPoolRequest(state, request.Key)
		poolAttempt, err := state.poolEngine.ReserveCandidate(poolRequest, pool.candidate, stream)
		if err != nil {
			return err
		}
		state.mu.Lock()
		state.poolAttempt, state.poolRequest = poolAttempt, poolRequest
		state.mu.Unlock()
	}
	attempt, err := scheduler.Default.ReserveCandidate(request, SchedulerCandidate(channel), stream)
	if err != nil {
		state.mu.Lock()
		poolAttempt, poolEngine := state.poolAttempt, state.poolEngine
		state.poolAttempt, state.poolRequest = nil, nil
		state.mu.Unlock()
		if poolAttempt != nil {
			poolEngine.FinishAttempt(poolAttempt, scheduler.Outcome{})
		}
		return err
	}
	state.mu.Lock()
	state.attempt, state.started, state.stream, state.ttft, state.pending = attempt, false, stream, nil, nil
	state.lineParsed, state.eventOutcome, state.writtenOutcome = false, relaycommon.ResponseOutcomeUnknown, relaycommon.ResponseOutcomeUnknown
	state.currentRequest = request
	state.mu.Unlock()
	return nil
}

// PrepareUnsentPluginCredentialRetry permits another enabled key on the same
// channel only when the rejected key required login and no request was sent.
// The normal retry budget and channel reservations still apply; the failed key
// has already been disabled by the credential resolver.
func PrepareUnsentPluginCredentialRetry(c *gin.Context, info *relaycommon.RelayInfo, apiErr *types.NewAPIError) {
	if info != nil && info.ChannelIsMultiKey && info.ChannelLimitAttempt != nil && !info.ChannelLimitAttempt.Sent && !info.ChannelLimitAttempt.PriorPhysicalRequestSent && errors.Is(apiErr, ErrPluginCredentialNeedsLogin) {
		c.Set(unsentCredentialRetryChannelKey, info.ChannelId)
	}
}

// StartSchedulingAttempt runs after local validation/billing and immediately
// before the upstream attempt. A reservation alone never activates learning.
func StartSchedulingAttempt(c *gin.Context, info *relaycommon.RelayInfo) {
	if info.ChannelLimitAttempt != nil && !info.ChannelLimitAttempt.Sent {
		return
	}
	state := schedulingState(c)
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.attempt == nil || state.started {
		return
	}
	state.coldPriorities[state.currentRequest.Key] = state.attempt.Candidate.Priority
	scheduler.Default.TouchRequest(state.currentRequest)
	state.stream = info.IsStream
	state.lastOutcome = ""
	scheduler.Default.StartAttempt(state.attempt, info.IsStream)
	state.dispatched++
	if state.poolAttempt != nil {
		state.poolEngine.TouchRequest(state.poolRequest)
		state.poolEngine.StartAttempt(state.poolAttempt, info.IsStream)
	}
	state.started = true
}

// FinalRelayOutcome applies the same delivery evidence to request metrics as
// to scheduling. It evaluates the current request, never a cached prior attempt.
func FinalRelayOutcome(c *gin.Context, info *relaycommon.RelayInfo, apiErr *types.NewAPIError) perfmetrics.Outcome {
	if c == nil {
		return perfmetrics.ClassifyRelayOutcome(context.Background(), info, apiErr)
	}
	state := schedulingState(c)
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.finalRelayOutcome(c, info, apiErr)
}

// The caller holds state.mu; response writer observations stay consistent.
func (state *schedulingRequest) finalRelayOutcome(c *gin.Context, info *relaycommon.RelayInfo, apiErr *types.NewAPIError) perfmetrics.Outcome {
	classificationContext := context.Background()
	if c.Request != nil {
		classificationContext = c.Request.Context()
	}
	// HTTP clients can close as soon as the complete non-stream response
	// arrives, while settlement is still finishing. Successful completion
	// with no short/failed writes is not a client-aborted channel attempt.
	if info != nil && !info.IsStream && apiErr == nil && state.output && !state.writeFailedBeforeTerminal {
		classificationContext = context.WithoutCancel(classificationContext)
	}
	classification := perfmetrics.ClassifyRelayOutcome(classificationContext, info, apiErr)
	if state.writeFailedBeforeTerminal || info != nil && info.IsStream && classificationContext.Err() != nil && state.writtenOutcome == relaycommon.ResponseOutcomeUnknown {
		classification = perfmetrics.OutcomeIgnored
	} else if info != nil && info.IsStream && state.writtenOutcome != relaycommon.ResponseOutcomeUnknown && apiErr == nil && !info.PerformanceBusinessRejection && (classificationContext.Err() == nil || errors.Is(classificationContext.Err(), context.Canceled)) {
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
	return classification
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
		classification := state.finalRelayOutcome(c, info, apiErr)
		state.lastOutcome = classification
		outcome.Success = classification == perfmetrics.OutcomeSuccess
		outcome.ChannelFailure = classification == perfmetrics.OutcomeFailure
		if outcome.ChannelFailure {
			outcome.CooldownFailure = availabilityFailure(info, apiErr)
		}
		outcome.TTFT = state.ttft
		if classification != perfmetrics.OutcomeIgnored && !state.confirmed {
			scheduler.Default.ConfirmRequest(state.currentRequest)
			state.confirmed = true
		}
	}
	scheduler.Default.FinishAttempt(state.attempt, outcome)
	if state.poolAttempt != nil {
		if state.started && state.lastOutcome != perfmetrics.OutcomeIgnored {
			state.poolEngine.ConfirmRequest(state.poolRequest)
		}
		state.poolEngine.FinishAttempt(state.poolAttempt, outcome)
		state.poolAttempt, state.poolRequest = nil, nil
	}
	state.attempt, state.started = nil, false
}

// availabilityFailure narrows a classified channel failure to evidence of
// transient upstream unavailability. A retained stream can carry the actual
// protocol error even though its handler returned nil after writing output.
func availabilityFailure(info *relaycommon.RelayInfo, apiErr *types.NewAPIError) bool {
	var stream relaycommon.StreamOutcome
	if info != nil {
		stream = info.StreamStatus.OutcomeSnapshot()
		if apiErr == nil && info.StreamFailure != nil {
			apiErr = info.StreamFailure.Error
		}
	}
	code, kind, status := stream.ErrorCode, stream.ErrorType, stream.ErrorStatus
	if apiErr != nil {
		// Interpret the original error even if a host wrapper changed its status.
		for {
			inner, ok := errors.AsType[*types.NewAPIError](apiErr.Unwrap())
			if !ok || inner == apiErr {
				break
			}
			apiErr = inner
		}
		switch RelayErrorSource(apiErr) {
		case "local", "channel":
			return false
		case "transport":
			return true
		}
		code, kind, status = string(apiErr.GetErrorCode()), apiErr.ToOpenAIError().Type, apiErr.StatusCode
	} else if stream.Response != relaycommon.ResponseOutcomeFailed {
		if info != nil && info.StreamStatus != nil && errors.Is(info.StreamStatus.EndError, context.DeadlineExceeded) {
			return true
		}
		switch stream.EndReason {
		case relaycommon.StreamEndReasonTimeout, relaycommon.StreamEndReasonScannerErr:
			return true
		case relaycommon.StreamEndReasonEOF:
			return stream.ExpectsTerminal && stream.Response == relaycommon.ResponseOutcomeUnknown
		}
		return false
	}
	// Some adapters synthesize a 502 for any SSE error envelope. A specific
	// credential or configuration code takes precedence over that status.
	for _, value := range []string{code, kind} {
		switch strings.ToLower(value) {
		case "invalid_api_key", "api_key_invalid", "api_key_expired", "api_key_service_blocked",
			"invalid_authentication", "authentication_error", "unauthenticated", "permission_denied",
			"permission_error", "authorization_error", "access_denied", "model_not_found", "insufficient_quota":
			return false
		}
	}
	for _, value := range []string{code, kind} {
		switch strings.ToLower(value) {
		case "server_error", "internal_error", "service_unavailable", "overloaded_error", "server_is_overloaded",
			"rate_limit_exceeded", "rate_limit_error", "resource_exhausted":
			return true
		}
	}
	return status == http.StatusTooManyRequests || status == http.StatusRequestTimeout || status/100 == 5
}

func EndSchedulingRequest(c *gin.Context) {
	value, ok := c.Get(schedulerContextKey)
	if !ok {
		return
	}
	state := value.(*schedulingRequest)
	state.mu.Lock()
	defer state.mu.Unlock()
	// All routes have now been tried. Attribute a terminal rejection once,
	// to the last dimension where capacity prevented a reservation.
	blocked := state.capacityBlocks
	state.capacityBlocks = nil
	if !state.capacityUnresolved {
		for _, key := range blocked {
			scheduler.Default.RecordCapacityOutcome(key, false)
		}
	} else if len(blocked) > 0 && ((c.Writer != nil && c.Writer.Status() == http.StatusServiceUnavailable) || IsRelayWebSocketRequest(c)) {
		key := blocked[len(blocked)-1]
		if scheduler.Default.RecordCapacityOutcome(key, true) {
			logger.LogWarn(c, common.LogText("scheduling capacity exhausted: group=%s model=%s", key.Group, key.Model))
		}
	}
	state.capacityUnresolved = false
	for _, request := range state.requests {
		scheduler.Default.EndRequest(request)
	}
	for _, request := range state.poolRequests {
		state.poolEngine.EndRequest(request)
	}
	state.poolAttempt = nil
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
	if _, exists := c.Get(failureObservationKey); !exists {
		c.Set(failureObservationKey, &failureOutputObservation{})
	}
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
		if value, ok := w.ctx.Get(failureObservationKey); ok {
			observation := value.(*failureOutputObservation)
			observation.mu.Lock()
			observation.writeFailed = true
			observation.mu.Unlock()
		}
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
		observeFailureOutput(w.ctx, body[:n])
		observeSchedulingOutput(w.ctx, body[:n], w.ResponseWriter.Status())
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
	status := http.StatusOK
	if c.Writer != nil {
		status = c.Writer.Status()
	}
	observeSchedulingOutput(c, body, status)
}

func observeSchedulingOutput(c *gin.Context, body []byte, status int) {
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
		useful = status < http.StatusBadRequest
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
		// A recovery probe is resolved by its first useful output; the rest
		// of the stream no longer holds the channel's half-open gate.
		scheduler.Default.ObserveFirstOutput(state.attempt)
		if state.poolAttempt != nil {
			state.poolEngine.ObserveFirstOutput(state.poolAttempt)
		}
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
