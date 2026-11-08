package service

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"sync"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/scheduler"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

// Pool identities outlive channel membership. Deleting or adding a channel
// cannot reset the supplier's health samples, ramp or shared capacity.
var supplierRouting = struct {
	sync.Mutex
	engine        *scheduler.Engine
	channelEngine *scheduler.Engine
	identities    map[int]string
}{identities: make(map[int]string)}

type supplierRoutingPool struct {
	candidate scheduler.Candidate
	channels  []*model.Channel
}

// Synchronize all dimensions from authoritative metadata. A supplier capacity
// change must replace the old value even in a model that is currently idle.
// Request-filtered candidates must never become the pool's membership source.
func syncSupplierRoutingMembership(channels []*model.Channel, channelSets map[scheduler.Key][]scheduler.Candidate, entries []SchedulingConfig) (map[scheduler.Key][]scheduler.Candidate, error) {
	byChannel := make(map[int]scheduler.Candidate, len(channels))
	metadata := make(map[int]scheduler.Candidate)
	for _, channel := range channels {
		pool, err := supplierRoutingPoolForChannel(channel)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		byChannel[channel.Id] = pool.candidate
		metadata[pool.candidate.ID] = pool.candidate
	}
	sets := make(map[scheduler.Key][]scheduler.Candidate, len(channelSets))
	for key, members := range channelSets {
		pools := make(map[int]scheduler.Candidate)
		for _, member := range members {
			pool, ok := byChannel[member.ID]
			if !ok {
				continue
			}
			pool.Excluded = member.Excluded || member.Status != common.ChannelStatusEnabled
			if previous, exists := pools[pool.ID]; exists {
				pool.Excluded = pool.Excluded && previous.Excluded
			}
			pools[pool.ID] = pool
		}
		for _, pool := range pools {
			sets[key] = append(sets[key], pool)
		}
	}
	supplierRouting.Lock()
	engine := supplierRouting.engine
	supplierRouting.Unlock()
	if engine == nil {
		return sets, nil
	}
	keys := make(map[scheduler.Key]bool, len(sets))
	for key := range sets {
		keys[key] = true
	}
	for _, key := range engine.Keys() {
		keys[key] = true
	}
	for key := range keys {
		config := scheduler.DefaultConfig()
		for _, entry := range entries {
			if entry.Group == key.Group && entry.Model == key.Model {
				config = scheduler.Config{Enabled: entry.Enabled, SuccessTarget: entry.TargetSuccessRate, LatencyTargetMS: entry.TargetTTFTMS, DefaultCapacity: entry.DefaultCapacity}
				break
			}
		}
		if err := engine.SetConfig(key, config); err != nil {
			return nil, err
		}
	}
	canonical := make([]scheduler.Candidate, 0, len(metadata))
	for _, pool := range metadata {
		canonical = append(canonical, pool)
	}
	engine.ReplaceCandidateMembership(sets, canonical...)
	return sets, nil
}

func supplierRoutingPoolForChannel(channel *model.Channel) (*supplierRoutingPool, error) {
	key := fmt.Sprintf("channel:%d", channel.Id)
	priority, weight := channel.GetPriority(), float64(channel.GetWeight())
	capacity := 0
	if channel.OwnerSupplierID != "" {
		key = "supplier:" + channel.OwnerSupplierID
		account, err := model.GetSupplierAccount(channel.OwnerSupplierID)
		if err != nil {
			return nil, err
		}
		priority, weight = account.PoolPriority, float64(account.PoolWeight)
		capacity = account.PoolCapacity
	}
	supplierRouting.Lock()
	defer supplierRouting.Unlock()
	if supplierRouting.engine == nil || supplierRouting.channelEngine != scheduler.Default {
		supplierRouting.engine, supplierRouting.channelEngine = scheduler.New(), scheduler.Default
		supplierRouting.identities = make(map[int]string)
	}
	digest := sha256.Sum256([]byte(key))
	id := int(binary.BigEndian.Uint64(digest[:8]) & uint64(^uint(0)>>1))
	if id == 0 {
		id = 1
	}
	if identity, exists := supplierRouting.identities[id]; exists && identity != key {
		return nil, errors.New("supplier routing identity collision")
	}
	supplierRouting.identities[id] = key
	return &supplierRoutingPool{candidate: scheduler.Candidate{ID: id, Status: common.ChannelStatusEnabled, Priority: priority, Weight: weight, Capacity: capacity, CapacityKey: key}, channels: []*model.Channel{channel}}, nil
}

func supplierPoolRequest(state *schedulingRequest, key scheduler.Key) *scheduler.Request {
	if state.poolEngine == nil {
		supplierRouting.Lock()
		state.poolEngine = supplierRouting.engine
		supplierRouting.Unlock()
		state.poolRequests = make(map[scheduler.Key]*scheduler.Request)
	}
	config := scheduler.DefaultConfig()
	schedulingConfigState.Lock()
	for _, entry := range schedulingConfigState.entries {
		if entry.Group == key.Group && entry.Model == key.Model {
			config = scheduler.Config{Enabled: entry.Enabled, SuccessTarget: entry.TargetSuccessRate, LatencyTargetMS: entry.TargetTTFTMS, DefaultCapacity: entry.DefaultCapacity}
			break
		}
	}
	schedulingConfigState.Unlock()
	// Configuration was validated by SyncSchedulingConfig before selection.
	_ = state.poolEngine.SetConfig(key, config)
	request := state.poolRequests[key]
	if request == nil {
		request = state.poolEngine.BeginRequest(key, state.id, true)
		state.poolRequests[key] = request
	}
	return request
}

func supplierChannelEligible(c *gin.Context, channel *model.Channel) bool {
	if !model.SupplierChannelAvailable(channel) {
		return false
	}
	if channel.OwnerSupplierID == "" || c == nil {
		return true
	}
	state := schedulingState(c)
	state.mu.Lock()
	defer state.mu.Unlock()
	return !state.sentSuppliers[channel.OwnerSupplierID]
}

// MarkSupplierRequestSent belongs at the final dispatch gate, after local
// capacity and quota reservations. A refused provisional candidate does not
// consume the supplier's one actual attempt within a logical request.
func MarkSupplierRequestSent(c *gin.Context, channel *model.Channel) bool {
	if channel == nil || channel.OwnerSupplierID == "" {
		return true
	}
	state := schedulingState(c)
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.sentSuppliers[channel.OwnerSupplierID] {
		return false
	}
	if state.sentSuppliers == nil {
		state.sentSuppliers = make(map[string]bool)
	}
	state.sentSuppliers[channel.OwnerSupplierID] = true
	return true
}

// ReleaseSupplierRequestSent rolls back only a successful local claim whose
// durable dispatch transition failed before any upstream transmission. A caller
// that did not acquire the claim must never release it.
func ReleaseSupplierRequestSent(c *gin.Context, channel *model.Channel) {
	if channel == nil || channel.OwnerSupplierID == "" || c == nil {
		return
	}
	state := schedulingState(c)
	state.mu.Lock()
	defer state.mu.Unlock()
	delete(state.sentSuppliers, channel.OwnerSupplierID)
}

// The platform engine sees one candidate per supplier, independent of the
// number or internal priorities of its channels. Both engines keep the existing
// health/latency/load scoring and capacity reservation semantics.
func selectSupplierPoolChannel(c *gin.Context, state *schedulingRequest, request *scheduler.Request, channels []*model.Channel, stream, retry bool) (*model.Channel, error) {
	pools := make(map[int]*supplierRoutingPool)
	for _, channel := range channels {
		if !supplierChannelEligible(c, channel) || slices.Contains(c.GetStringSlice("use_channel"), fmt.Sprint(channel.Id)) {
			continue
		}
		pool, err := supplierRoutingPoolForChannel(channel)
		if err != nil {
			return nil, err
		}
		if existing := pools[pool.candidate.ID]; existing != nil {
			existing.channels = append(existing.channels, channel)
		} else {
			pools[pool.candidate.ID] = pool
		}
	}
	if len(pools) == 0 {
		return nil, nil
	}
	poolRequest := supplierPoolRequest(state, request.Key)
	for len(pools) > 0 {
		candidates := make([]scheduler.Candidate, 0, len(pools))
		for _, pool := range pools {
			candidates = append(candidates, pool.candidate)
		}
		slices.SortFunc(candidates, func(a, b scheduler.Candidate) int { return a.ID - b.ID })
		poolAttempt, err := state.poolEngine.SelectAndReserve(poolRequest, candidates, stream, retry)
		if errors.Is(err, scheduler.ErrNoEligibleChannel) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		pool := pools[poolAttempt.Candidate.ID]
		members := make([]scheduler.Candidate, 0, len(pool.channels))
		byID := make(map[int]*model.Channel, len(pool.channels))
		for _, channel := range pool.channels {
			members = append(members, SchedulerCandidate(channel))
			byID[channel.Id] = channel
		}
		attempt, err := scheduler.Default.SelectAndReserve(request, members, stream, retry)
		if err != nil {
			state.poolEngine.FinishAttempt(poolAttempt, scheduler.Outcome{})
			if errors.Is(err, scheduler.ErrNoEligibleChannel) {
				delete(pools, pool.candidate.ID)
				continue
			}
			return nil, err
		}
		state.mu.Lock()
		state.attempt, state.currentRequest = attempt, request
		state.poolAttempt, state.poolRequest = poolAttempt, poolRequest
		state.started, state.stream, state.ttft, state.pending = false, stream, nil, nil
		state.lineParsed, state.eventOutcome, state.writtenOutcome = false, relaycommon.ResponseOutcomeUnknown, relaycommon.ResponseOutcomeUnknown
		state.mu.Unlock()
		return byID[attempt.Candidate.ID], nil
	}
	return nil, nil
}
