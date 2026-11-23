package model

import (
	"crypto/sha256"
	"errors"
	"fmt"
	"math/rand"
	"slices"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/QuantumNous/new-api/dto"
	"github.com/QuantumNous/new-api/logger"
	kitdto "github.com/QuantumNous/new-api/relaykit/dto"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"gorm.io/gorm"
)

var group2model2channels map[string]map[string][]int // enabled channel
var channelsIDM map[int]*Channel                     // all channels include disabled
// channel2advancedCustomConfig caches parsed Advanced Custom (type 58) configs so
// path-aware selection avoids re-parsing JSON per request. Refreshed on full sync.
var channel2advancedCustomConfig map[int]*kitdto.AdvancedCustomConfig
var channelSyncLock sync.RWMutex

// channelCacheReloadLock serializes full cache publication.
var channelCacheReloadLock sync.Mutex

// channelStatusGeneration numbers the status changes cached by
// CacheUpdateChannelStatus. channelStatusChanges, guarded by channelSyncLock,
// keeps the last one of each channel until a reload that read after it.
var (
	channelStatusGeneration atomic.Uint64
	channelStatusChanges    = map[int]cachedStatusChange{}
)

type cachedStatusChange struct {
	generation uint64
	status     int
	// info is the key-level state of a full entry update (per-key disables);
	// nil for a status-only change.
	info *ChannelInfo
}

// RoutingMetadataVersion changes after channel CRUD and cache publication.
// Scheduling reconciles complete membership once per version, never from a
// request-filtered candidate list or a full database scan on every request.
var routingMetadataVersion atomic.Uint64

func RoutingMetadataVersion() uint64 { return routingMetadataVersion.Load() }

func invalidateRoutingMetadata() { routingMetadataVersion.Add(1) }

// routingMetadataFingerprint identifies the routing fields published by the
// last full cache reload, so an unchanged periodic reload keeps the version.
var routingMetadataFingerprint [sha256.Size]byte

// routingFingerprint covers every channel and ability field that scheduling
// membership reads. Rows are ordered so database order cannot change it.
func routingFingerprint(channels []*Channel, abilities []*Ability, accounts []SupplierAccount) [sha256.Size]byte {
	hash := sha256.New()
	sorted := slices.Clone(channels)
	slices.SortFunc(sorted, func(a, b *Channel) int { return a.Id - b.Id })
	for _, channel := range sorted {
		fmt.Fprintf(hash, "c|%d|%q|%d|%q|%q|%d|%d|%q|%d\n", channel.Id, channel.Name, channel.Status, channel.Group, channel.Models,
			channel.GetPriority(), channel.GetWeight(), channel.OwnerSupplierID, channel.SupplierPriority)
	}
	rows := make([]string, 0, len(abilities))
	for _, ability := range abilities {
		priority := int64(0)
		if ability.Priority != nil {
			priority = *ability.Priority
		}
		rows = append(rows, fmt.Sprintf("a|%q|%q|%d|%t|%d|%d\n", ability.Group, ability.Model, ability.ChannelId, ability.Enabled, priority, ability.Weight))
	}
	slices.Sort(rows)
	for _, row := range rows {
		hash.Write([]byte(row))
	}
	// Supplier pools route by account priority, weight and capacity.
	slices.SortFunc(accounts, func(a, b SupplierAccount) int { return strings.Compare(a.SupplierID, b.SupplierID) })
	for _, account := range accounts {
		fmt.Fprintf(hash, "s|%q|%t|%d|%d|%d|%d\n", account.SupplierID, account.SupplyEnabled, account.PoolPriority, account.PoolWeight, account.PoolCapacity, account.Revision)
	}
	var fingerprint [sha256.Size]byte
	copy(fingerprint[:], hash.Sum(nil))
	return fingerprint
}

// GetSatisfiedChannelCandidates returns every enabled matching channel across
// priorities. Copies keep scheduling snapshots independent of cache updates.
func GetSatisfiedChannelCandidates(group, modelName string, filters []dto.ChannelFilter) ([]*Channel, error) {
	var channels []*Channel
	if common.MemoryCacheEnabled {
		channelSyncLock.RLock()
		defer channelSyncLock.RUnlock()
		ids, _ := filterCandidateIDs(group2model2channels[group][modelName], modelName, filters)
		if len(ids) == 0 {
			ids, _ = filterCandidateIDs(group2model2channels[group][ratio_setting.RoutingMatchModelName(modelName)], modelName, filters)
		}
		for _, id := range ids {
			if channel := channelsIDM[id]; channel != nil && channel.Status == common.ChannelStatusEnabled {
				copy := *channel
				channels = append(channels, &copy)
			}
		}
		return channels, nil
	}
	// Abilities are authoritative for uncached routing. Channel group/model
	// metadata alone must never revive a disabled or absent ability. Like the
	// memory cache, retry with the routing name when the exact name finds none.
	names := []string{modelName}
	if normalized := ratio_setting.RoutingMatchModelName(modelName); normalized != "" && normalized != modelName {
		names = append(names, normalized)
	}
	for _, abilityModel := range names {
		var abilities []Ability
		if err := DB.Where(map[string]any{"group": group, "model": abilityModel, "enabled": true}).Find(&abilities).Error; err != nil {
			return nil, err
		}
		if len(abilities) == 0 {
			continue
		}
		ids := make([]int, 0, len(abilities))
		byID := make(map[int]Ability, len(abilities))
		for _, ability := range abilities {
			ids = append(ids, ability.ChannelId)
			byID[ability.ChannelId] = ability
		}
		var all []*Channel
		if err := DB.Where("id IN ?", ids).Find(&all).Error; err != nil {
			return nil, err
		}
		for _, channel := range all {
			if channel.Status != common.ChannelStatusEnabled || !channel.HasPublishedConfiguration() || channel.credentialUnavailable {
				continue
			}
			if ok, _ := ChannelSatisfiesFilters(channel, modelName, filters); ok {
				ability := byID[channel.Id]
				channel.Priority, channel.Weight = ability.Priority, &ability.Weight
				channels = append(channels, channel)
			}
		}
		if len(channels) > 0 {
			break
		}
	}
	return channels, nil
}

func InitChannelCache() {
	_ = ReloadChannelCache()
}

// ReloadChannelCache preserves the previous routing cache on a failed read and
// reports the failure so node synchronization can retry its revision.
func ReloadChannelCache() error {
	channelCacheReloadLock.Lock()
	defer channelCacheReloadLock.Unlock()
	if !common.MemoryCacheEnabled {
		defer invalidateRoutingMetadata()
		InvalidatePricingCache()
		rebuildTaskAliasView()
		return nil
	}
	newChannelId2channel := make(map[int]*Channel)
	newChannel2advancedCustomConfig := make(map[int]*kitdto.AdvancedCustomConfig)
	var channels []*Channel
	// A status change cached after this point may be missing from the rows read.
	readGeneration := channelStatusGeneration.Load()
	// A failed read must not publish an empty cache: keep serving the last
	// complete one until a later reload succeeds.
	if err := DB.Find(&channels).Error; err != nil {
		common.SysError(common.LogText("failed to sync channels from database, keeping the previous cache: %s", err.Error()))
		return err
	}
	for _, channel := range channels {
		if channel.credentialUnavailable {
			// Only this channel stops routing on this node; the stored
			// credential and status stay as they are.
			common.SysError(common.LogText("channel #%d credential cannot be decrypted on this node, disabling it in the channel cache", channel.Id))
			channel.Status = common.ChannelStatusManuallyDisabled
		}
		// The cache keeps write-only credentials sealed. A channel that
		// cannot be sealed must leave routing before the routes are built.
		if err := channel.sealSupplierCredential(); err != nil {
			channel.Key, channel.Keys, channel.Status = "", nil, common.ChannelStatusManuallyDisabled
		}
		channel.compiledAvailability = channel.availabilityPlan()
		newChannelId2channel[channel.Id] = channel
		if channel.Type == constant.ChannelTypeAdvancedCustom {
			if config := channel.GetOtherSettings().AdvancedCustom; config != nil {
				newChannel2advancedCustomConfig[channel.Id] = config
			}
		}
	}
	var abilities []*Ability
	if err := DB.Find(&abilities).Error; err != nil {
		common.SysError(common.LogText("failed to sync abilities from database, keeping the previous cache: %s", err.Error()))
		return err
	}
	var accounts []SupplierAccount
	suppliersKnown := true
	if slices.ContainsFunc(channels, func(channel *Channel) bool { return channel.OwnerSupplierID != "" }) {
		suppliersKnown = DB.Find(&accounts).Error == nil
	}
	fingerprint := routingFingerprint(channels, abilities, accounts)
	type abilityKey struct {
		group, model string
		channel      int
	}
	enabled := make(map[abilityKey]bool, len(abilities))
	for _, ability := range abilities {
		if ability.Enabled {
			enabled[abilityKey{ability.Group, ability.Model, ability.ChannelId}] = true
		}
	}
	newGroup2model2channels := make(map[string]map[string][]int)
	for _, channel := range channels {
		if channel.Status != common.ChannelStatusEnabled || !channel.HasPublishedConfiguration() {
			continue
		}
		models := channel.GetModels()
		for _, group := range channel.GetGroups() {
			for _, modelName := range models {
				if !enabled[abilityKey{group, modelName, channel.Id}] {
					continue
				}
				if newGroup2model2channels[group] == nil {
					newGroup2model2channels[group] = make(map[string][]int)
				}
				newGroup2model2channels[group][modelName] = append(newGroup2model2channels[group][modelName], channel.Id)
			}
		}
	}

	// sort by priority
	for group, model2channels := range newGroup2model2channels {
		for model, channels := range model2channels {
			sort.Slice(channels, func(i, j int) bool {
				return newChannelId2channel[channels[i]].GetPriority() > newChannelId2channel[channels[j]].GetPriority()
			})
			newGroup2model2channels[group][model] = channels
		}
	}

	channelSyncLock.Lock()
	// A disable cached after the read above may be missing from the rows: keep
	// it, so this reload cannot route to the channel again. A later enable is
	// left to the next reload.
	for id, change := range channelStatusChanges {
		if change.generation <= readGeneration {
			delete(channelStatusChanges, id)
			continue
		}
		channel := newChannelId2channel[id]
		if channel == nil {
			continue
		}
		if change.info != nil {
			channel.ChannelInfo = *change.info
		}
		if change.status == common.ChannelStatusEnabled {
			continue
		}
		channel.Status = change.status
		for _, model2channels := range newGroup2model2channels {
			for model, ids := range model2channels {
				model2channels[model] = slices.DeleteFunc(ids, func(channelID int) bool { return channelID == id })
			}
		}
	}
	group2model2channels = newGroup2model2channels
	//channelsIDM = newChannelId2channel
	for i, channel := range newChannelId2channel {
		// Sealed keys stay sealed in the cache; a request opens its own copy.
		if channel.ChannelInfo.IsMultiKey && !strings.HasPrefix(channel.Key, supplierSecretPrefix) {
			channel.Keys = channel.GetKeys()
		}
		if channel.ChannelInfo.IsMultiKey && channel.ChannelInfo.MultiKeyMode == constant.MultiKeyModePolling {
			if oldChannel, ok := channelsIDM[i]; ok {
				// 存在旧的渠道，如果是多key且轮询，保留轮询索引信息
				if oldChannel.ChannelInfo.IsMultiKey && oldChannel.ChannelInfo.MultiKeyMode == constant.MultiKeyModePolling {
					channel.ChannelInfo.MultiKeyPollingIndex = oldChannel.ChannelInfo.MultiKeyPollingIndex
				}
			}
		}
	}
	channelsIDM = newChannelId2channel
	channel2advancedCustomConfig = newChannel2advancedCustomConfig
	channelSyncLock.Unlock()
	if !suppliersKnown || fingerprint != routingMetadataFingerprint {
		routingMetadataFingerprint = fingerprint
		invalidateRoutingMetadata()
	}
	// Lock ordering: InvalidatePricingCache acquires updatePricingLock, and
	// GetPricing (holding updatePricingLock) nests channelSyncLock.RLock via
	// loadPricingAdvancedCustomConfigs. channelSyncLock MUST be released before
	// invalidating the pricing cache, otherwise the reversed order deadlocks.
	InvalidatePricingCache()
	rebuildTaskAliasView()
	common.SysLog(common.LogText("channels synced from database"))
	return nil
}

func SyncChannelCache(frequency int) {
	for {
		time.Sleep(time.Duration(frequency) * time.Second)
		common.SysLog(common.LogText("syncing channels from database"))
		InitChannelCache()
	}
}

func GetRandomSatisfiedChannel(
	group string,
	model string,
	retry int,
	filters []dto.ChannelFilter,
) (*Channel, error) {
	// if memory cache is disabled, get channel directly from database
	if !common.MemoryCacheEnabled {
		return GetChannel(group, model, retry, filters)
	}

	channelSyncLock.RLock()
	defer channelSyncLock.RUnlock()

	// First, try to find channels with the exact model name.
	channels, _ := filterCandidateIDs(group2model2channels[group][model], model, filters)

	// If no channels found, try to find channels with the normalized model name.
	if len(channels) == 0 {
		normalizedModel := ratio_setting.RoutingMatchModelName(model)
		channels, _ = filterCandidateIDs(group2model2channels[group][normalizedModel], model, filters)
	}

	if len(channels) == 0 {
		return nil, nil
	}

	if len(channels) == 1 {
		if channel, ok := channelsIDM[channels[0]]; ok {
			return channel, nil
		}
		return nil, fmt.Errorf("database consistency error: channel #%d does not exist, please contact the administrator", channels[0])
	}

	uniquePriorities := make(map[int]bool)
	for _, channelId := range channels {
		if channel, ok := channelsIDM[channelId]; ok {
			uniquePriorities[int(channel.GetPriority())] = true
		} else {
			return nil, fmt.Errorf("database consistency error: channel #%d does not exist, please contact the administrator", channelId)
		}
	}
	var sortedUniquePriorities []int
	for priority := range uniquePriorities {
		sortedUniquePriorities = append(sortedUniquePriorities, priority)
	}
	sort.Sort(sort.Reverse(sort.IntSlice(sortedUniquePriorities)))

	if retry >= len(uniquePriorities) {
		retry = len(uniquePriorities) - 1
	}
	targetPriority := int64(sortedUniquePriorities[retry])

	// get the priority for the given retry number
	var targetChannels []*Channel
	for _, channelId := range channels {
		if channel, ok := channelsIDM[channelId]; ok {
			if channel.GetPriority() == targetPriority {
				targetChannels = append(targetChannels, channel)
			}
		} else {
			return nil, fmt.Errorf("database consistency error: channel #%d does not exist, please contact the administrator", channelId)
		}
	}

	if len(targetChannels) == 0 {
		return nil, errors.New(fmt.Sprintf("no channel found, group: %s, model: %s, priority: %d", group, model, targetPriority))
	}

	weights := make([]uint, len(targetChannels))
	for i, channel := range targetChannels {
		weights[i] = uint(channel.GetWeight())
	}
	return targetChannels[configuredWeightIndex(weights, rand.Float64())], nil
}

func CacheGetChannel(id int) (*Channel, error) {
	if !common.MemoryCacheEnabled {
		return GetChannelById(id, true)
	}
	channelSyncLock.RLock()
	defer channelSyncLock.RUnlock()

	c, ok := channelsIDM[id]
	if !ok {
		return nil, fmt.Errorf("channel #%d no longer exists", id)
	}
	return c, nil
}

// CacheGetChannelForRouting takes a stable status/configuration snapshot so a
// concurrent disable cannot race a scheduler's initial eligibility check.
func CacheGetChannelForRouting(id int) (*Channel, error) {
	if !common.MemoryCacheEnabled {
		return GetChannelById(id, true)
	}
	channelSyncLock.RLock()
	defer channelSyncLock.RUnlock()
	channel := channelsIDM[id]
	if channel == nil {
		return nil, fmt.Errorf("channel %d no longer exists: %w", id, gorm.ErrRecordNotFound)
	}
	copy := *channel
	return &copy, nil
}

func CacheGetChannelInfo(id int) (*ChannelInfo, error) {
	if !common.MemoryCacheEnabled {
		var channel Channel
		if err := DB.Select("id", "channel_info").First(&channel, "id = ?", id).Error; err != nil {
			return nil, err
		}
		return &channel.ChannelInfo, nil
	}
	channelSyncLock.RLock()
	defer channelSyncLock.RUnlock()

	c, ok := channelsIDM[id]
	if !ok {
		return nil, fmt.Errorf("channel #%d no longer exists", id)
	}
	return &c.ChannelInfo, nil
}

// advanceCachedPollingIndex replaces the polling cursor of the cached channel
// with advance(cursor). The caller holds the channel's polling lock, and the
// cache lock keeps readers copying the entry from seeing a torn update. It
// reports false when the channel is no longer cached.
func advanceCachedPollingIndex(id int, advance func(cursor int) int) bool {
	channelSyncLock.Lock()
	defer channelSyncLock.Unlock()
	cached, ok := channelsIDM[id]
	if !ok {
		return false
	}
	cached.ChannelInfo.MultiKeyPollingIndex = advance(cached.ChannelInfo.MultiKeyPollingIndex)
	return true
}

// cachedChannelStatusIs reports whether routing on this node already uses
// status for the channel. Without the memory cache routing reads the database.
func cachedChannelStatusIs(id int, status int) bool {
	if !common.MemoryCacheEnabled {
		return true
	}
	channelSyncLock.RLock()
	defer channelSyncLock.RUnlock()
	channel, ok := channelsIDM[id]
	return ok && channel.Status == status
}

func CacheUpdateChannelStatus(id int, status int) {
	defer invalidateRoutingMetadata()
	if !common.MemoryCacheEnabled {
		return
	}
	channelSyncLock.Lock()
	defer channelSyncLock.Unlock()
	channelStatusChanges[id] = cachedStatusChange{generation: channelStatusGeneration.Add(1), status: status}
	if channel, ok := channelsIDM[id]; ok {
		channel.Status = status
	}
	if status != common.ChannelStatusEnabled {
		// delete the channel from group2model2channels
		for group, model2channels := range group2model2channels {
			for model, channels := range model2channels {
				for i, channelId := range channels {
					if channelId == id {
						// remove the channel from the slice
						group2model2channels[group][model] = append(channels[:i], channels[i+1:]...)
						break
					}
				}
			}
		}
	}
}

func CacheUpdateChannel(channel *Channel) {
	defer invalidateRoutingMetadata()
	if !common.MemoryCacheEnabled {
		return
	}
	channelSyncLock.Lock()
	if channel == nil {
		channelSyncLock.Unlock()
		return
	}

	if channelsIDM == nil {
		channelsIDM = make(map[int]*Channel)
	}
	if oldChannel, ok := channelsIDM[channel.Id]; ok {
		logger.LogDebug(nil, "CacheUpdateChannel before: id=%d, name=%s, status=%d, polling_index=%d", channel.Id, channel.Name, channel.Status, oldChannel.ChannelInfo.MultiKeyPollingIndex)
	}
	copy := *channel
	copy.compiledAvailability = channel.availabilityPlan()
	if err := copy.sealSupplierCredential(); err != nil || copy.credentialUnavailable {
		copy.Key, copy.Keys, copy.Status = "", nil, common.ChannelStatusManuallyDisabled
	}
	if oldChannel, ok := channelsIDM[channel.Id]; ok && copy.ChannelInfo.IsMultiKey && copy.ChannelInfo.MultiKeyMode == constant.MultiKeyModePolling &&
		oldChannel.ChannelInfo.IsMultiKey && oldChannel.ChannelInfo.MultiKeyMode == constant.MultiKeyModePolling {
		// The cursor lives only in the cache; a row loaded from the database
		// carries a stale one.
		copy.ChannelInfo.MultiKeyPollingIndex = oldChannel.ChannelInfo.MultiKeyPollingIndex
	}
	// A reload that read its rows before this update must not drop it.
	info := copy.ChannelInfo
	channelStatusChanges[channel.Id] = cachedStatusChange{generation: channelStatusGeneration.Add(1), status: copy.Status, info: &info}
	channelsIDM[channel.Id] = &copy
	if channel2advancedCustomConfig == nil {
		channel2advancedCustomConfig = make(map[int]*kitdto.AdvancedCustomConfig)
	}
	delete(channel2advancedCustomConfig, channel.Id)
	if channel.Type == constant.ChannelTypeAdvancedCustom {
		if config := channel.GetOtherSettings().AdvancedCustom; config != nil {
			channel2advancedCustomConfig[channel.Id] = config
		}
	}
	logger.LogDebug(nil, "CacheUpdateChannel after: id=%d, name=%s, status=%d, polling_index=%d", channel.Id, channel.Name, channel.Status, channel.ChannelInfo.MultiKeyPollingIndex)
	// Lock ordering: do NOT hold channelSyncLock while calling
	// InvalidatePricingCache. GetPricing acquires updatePricingLock first and then
	// channelSyncLock.RLock (via loadPricingAdvancedCustomConfigs); acquiring
	// updatePricingLock while holding channelSyncLock would be an AB-BA deadlock.
	channelSyncLock.Unlock()
	InvalidatePricingCache()
}
