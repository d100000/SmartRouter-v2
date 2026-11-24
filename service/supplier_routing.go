package service

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/scheduler"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/gin-gonic/gin"
	"gorm.io/gorm"
)

type supplierRoutingPool struct {
	candidate scheduler.Candidate
	channels  []*model.Channel
}

func supplierPoolKey(channel *model.Channel) string {
	if channel.OwnerSupplierID != "" {
		return "supplier:" + channel.OwnerSupplierID
	}
	return fmt.Sprintf("channel:%d", channel.Id)
}

// Build the complete supplier layer before either engine publishes the generation.
func buildSupplierMembership(channels []*model.Channel, channelSets map[scheduler.Key][]scheduler.Candidate, accounts map[string]model.SupplierAccount) (scheduler.Membership, map[string]scheduler.Candidate, error) {
	byChannel := make(map[int]scheduler.Candidate, len(channels))
	byKey := make(map[string]scheduler.Candidate)
	identities := make(map[int]string)
	pools := scheduler.Membership{Candidates: make(map[scheduler.Key][]scheduler.Candidate, len(channelSets))}
	for _, channel := range channels {
		key := supplierPoolKey(channel)
		candidate, exists := byKey[key]
		if !exists {
			candidate = scheduler.Candidate{Status: common.ChannelStatusEnabled, Priority: channel.GetPriority(), Weight: float64(channel.GetWeight()), CapacityKey: key}
			if channel.OwnerSupplierID != "" {
				account, ok := accounts[channel.OwnerSupplierID]
				if !ok {
					continue
				}
				candidate.Priority, candidate.Weight, candidate.Capacity = account.PoolPriority, float64(account.PoolWeight), account.PoolCapacity
			}
			digest := sha256.Sum256([]byte(key))
			candidate.ID = max(1, int(binary.BigEndian.Uint64(digest[:8])&uint64(^uint(0)>>1)))
			if other, found := identities[candidate.ID]; found && other != key {
				return scheduler.Membership{}, nil, errors.New("supplier routing identity collision")
			}
			identities[candidate.ID] = key
			byKey[key] = candidate
			pools.Metadata = append(pools.Metadata, candidate)
		}
		byChannel[channel.Id] = candidate
	}
	for key, members := range channelSets {
		candidates := make(map[int]scheduler.Candidate)
		for _, member := range members {
			pool, ok := byChannel[member.ID]
			if !ok {
				continue
			}
			pool.Excluded = member.Excluded || member.Status != common.ChannelStatusEnabled
			if previous, exists := candidates[pool.ID]; exists {
				pool.Excluded = pool.Excluded && previous.Excluded
			}
			candidates[pool.ID] = pool
		}
		for _, candidate := range candidates {
			pools.Candidates[key] = append(pools.Candidates[key], candidate)
		}
	}
	return pools, byKey, nil
}

func supplierRoutingPoolForChannel(channel *model.Channel) (*supplierRoutingPool, error) {
	schedulingConfigState.Lock()
	candidate, ok := schedulingConfigState.poolByKey[supplierPoolKey(channel)]
	schedulingConfigState.Unlock()
	if !ok {
		return nil, gorm.ErrRecordNotFound
	}
	return &supplierRoutingPool{candidate: candidate, channels: []*model.Channel{channel}}, nil
}

func supplierPoolRequest(state *schedulingRequest, key scheduler.Key) *scheduler.Request {
	if state.poolEngine == nil {
		schedulingConfigState.Lock()
		state.poolEngine = schedulingConfigState.poolEngine
		schedulingConfigState.Unlock()
		state.poolRequests = make(map[scheduler.Key]*scheduler.Request)
	}
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
	again, _ := state.retryState(request.Key)
	for _, channel := range channels {
		if !supplierChannelEligible(c, channel) || !again[channel.Id] && slices.Contains(c.GetStringSlice("use_channel"), fmt.Sprint(channel.Id)) {
			continue
		}
		pool, err := supplierRoutingPoolForChannel(channel)
		if errors.Is(err, gorm.ErrRecordNotFound) {
			continue
		}
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
