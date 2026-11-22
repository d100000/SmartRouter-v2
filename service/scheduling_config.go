package service

import (
	"database/sql"
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/scheduler"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
	"gorm.io/gorm"
)

const schedulingOptionKey = "SchedulingConfig"

// SchedulingConfig is persisted using the existing options store. Learning and
// concurrency state are deliberately instance-local and never stored as config.
type SchedulingConfig struct {
	Group             string                      `json:"group"`
	Model             string                      `json:"model"`
	Enabled           bool                        `json:"enabled"`
	TargetSuccessRate float64                     `json:"target_success_rate"`
	TargetTTFTMS      float64                     `json:"target_ttft_ms"`
	DefaultCapacity   int                         `json:"default_capacity"`
	ChannelOverrides  []SchedulingChannelOverride `json:"channel_overrides"`
}

type SchedulingChannelOverride struct {
	ChannelID   int      `json:"channel_id"`
	Weight      *float64 `json:"weight"`
	Capacity    *int     `json:"capacity"`
	CapacityKey string   `json:"capacity_key,omitempty"`
}

type schedulingSource struct {
	raw      string
	database *gorm.DB
	engine   *scheduler.Engine
}

type schedulingRefresh struct {
	source schedulingSource
	done   chan struct{}
	err    error
}

var schedulingConfigState = struct {
	sync.Mutex
	schedulingSource
	entries         []SchedulingConfig
	settings        map[scheduler.Key]scheduler.RoutingConfig
	metadataVersion uint64
	candidates      map[scheduler.Key][]scheduler.Candidate
	poolCandidates  map[scheduler.Key][]scheduler.Candidate
	poolByKey       map[string]scheduler.Candidate
	poolEngine      *scheduler.Engine
	refresh         *schedulingRefresh
	failedRefresh   *schedulingRefresh
	failures        int
	failedAt        time.Time
	retryAt         time.Time
	// aliases are concrete request models routed with their normalized
	// model's membership, keyed to their last use. Unused aliases expire.
	aliases    map[scheduler.Key]time.Time
	aliasSweep time.Time
}{}

// schedulingAliasTTL matches the statistics window: an alias unused this long
// has no in-flight work or windowed history left to preserve.
const schedulingAliasTTL = 30 * time.Minute

// SchedulingConfigStatus reports whether routing is serving an older snapshot
// because the latest configuration or metadata failed to load.
type SchedulingConfigStatus struct {
	Published bool   `json:"published"`
	Stale     bool   `json:"stale"`
	Error     string `json:"error,omitempty"`
	FailedAt  int64  `json:"failed_at,omitempty"`
	RetryAt   int64  `json:"retry_at,omitempty"`
	Failures  int    `json:"failures,omitempty"`
}

var schedulingSaveLock sync.Mutex

func ValidateSchedulingConfig(config SchedulingConfig) error {
	if strings.TrimSpace(config.Group) == "" || strings.TrimSpace(config.Model) == "" || config.Group == "auto" || len(config.Group) > 128 || len(config.Model) > 512 {
		return errors.New("a concrete group and model are required")
	}
	if math.IsNaN(config.TargetSuccessRate) || math.IsInf(config.TargetSuccessRate, 0) || config.TargetSuccessRate <= 0 || config.TargetSuccessRate > 1 {
		return errors.New("target_success_rate must be between 0 (exclusive) and 1")
	}
	if math.IsNaN(config.TargetTTFTMS) || math.IsInf(config.TargetTTFTMS, 0) || config.TargetTTFTMS < 1 || config.TargetTTFTMS > 3600000 {
		return errors.New("target_ttft_ms must be between 1 and 3600000")
	}
	// Zero leaves concurrency unlimited; explicit limits refuse new requests.
	if config.DefaultCapacity < 0 || config.DefaultCapacity > 100000 {
		return errors.New("default_capacity must be between 0 and 100000")
	}
	if len(config.ChannelOverrides) > 10000 {
		return errors.New("too many channel overrides")
	}
	seen := make(map[int]bool, len(config.ChannelOverrides))
	for _, override := range config.ChannelOverrides {
		if override.ChannelID <= 0 || seen[override.ChannelID] {
			return errors.New("channel overrides require unique positive channel IDs")
		}
		seen[override.ChannelID] = true
		if override.Weight != nil && (math.IsNaN(*override.Weight) || math.IsInf(*override.Weight, 0) || *override.Weight < 0 || *override.Weight > 1000000) {
			return errors.New("channel weight must be between 0 and 1000000")
		}
		if override.Capacity != nil && (*override.Capacity < 1 || *override.Capacity > 100000) {
			return errors.New("channel capacity must be between 1 and 100000")
		}
		if len(override.CapacityKey) > 128 || strings.ContainsAny(override.CapacityKey, "\r\n\x00") {
			return errors.New("invalid shared capacity key")
		}
	}
	return nil
}

// SyncSchedulingConfig is the synchronous publication barrier for administrative
// reads and writes. Traffic with a usable snapshot requests the same work asynchronously.
// Requested keys are registered like routed request models.
func SyncSchedulingConfig(requestedKeys ...scheduler.Key) error {
	if err := ensureSchedulingConfig(true); err != nil {
		return err
	}
	for _, key := range requestedKeys {
		registerSchedulingKey(key, 0)
	}
	return nil
}

// GetSchedulingConfigStatus exposes the last load failure to administrators.
func GetSchedulingConfigStatus() SchedulingConfigStatus {
	state := &schedulingConfigState
	state.Lock()
	defer state.Unlock()
	status := SchedulingConfigStatus{Published: state.poolEngine != nil}
	if state.failedRefresh == nil {
		return status
	}
	status.Stale = status.Published
	// Admins see a category; the driver or parse detail stays in the server log.
	status.Error = "invalid_configuration"
	if strings.HasPrefix(state.failedRefresh.err.Error(), "unable to load") {
		status.Error = "database_unavailable"
	}
	status.FailedAt, status.RetryAt, status.Failures = state.failedAt.Unix(), state.retryAt.Unix(), state.failures
	return status
}

// ensureSchedulingConfig keeps traffic on the last published snapshot while a
// changed configuration or metadata generation loads, and while a failed load
// retries with backoff. Only a process with no published snapshot for the
// current database and engine waits for, or fails with, the load.
func ensureSchedulingConfig(wait bool) error {
	for {
		common.OptionMapRWMutex.RLock()
		source := schedulingSource{raw: common.OptionMap[schedulingOptionKey], database: model.DB, engine: scheduler.Default}
		common.OptionMapRWMutex.RUnlock()
		if source.database == nil || source.engine == nil {
			return errors.New("scheduling is not initialized")
		}
		state := &schedulingConfigState
		state.Lock()
		version := model.RoutingMetadataVersion()
		current := state.schedulingSource == source && version == state.metadataVersion
		// A changed configuration briefly routes with the previous one. That is
		// preferable to failing every request while it loads or stays invalid.
		warm := state.poolEngine != nil && state.engine == source.engine && state.database == source.database
		if !current {
			work := state.refresh
			backoff := state.failedRefresh != nil && state.failedRefresh.source == source && time.Now().Before(state.retryAt)
			if !wait && !warm && work == nil && backoff {
				err := state.failedRefresh.err
				state.Unlock()
				return err
			}
			if (work == nil || work.source != source) && (wait || !warm || !backoff) {
				work = &schedulingRefresh{source: source, done: make(chan struct{})}
				state.refresh = work
				go work.run(version)
			}
			if wait || !warm {
				state.Unlock()
				<-work.done
				if work.err != nil {
					return work.err
				}
				continue
			}
		}
		state.Unlock()
		return nil
	}
}

// registerSchedulingKey gives a concrete request model its normalized model's
// membership. Call it only after the request found routable candidates, so
// arbitrary model names cannot create routing state. A positive channelID
// additionally requires that channel to be a routable member of the alias.
func registerSchedulingKey(key scheduler.Key, channelID int) {
	state := &schedulingConfigState
	state.Lock()
	defer state.Unlock()
	if state.poolEngine == nil {
		return
	}
	now := time.Now()
	if _, alias := state.aliases[key]; alias {
		state.aliases[key] = now
		return
	}
	if _, known := state.candidates[key]; known {
		return
	}
	normalized := scheduler.Key{Group: key.Group, Model: ratio_setting.RoutingMatchModelName(key.Model)}
	candidates := state.candidates[normalized]
	if len(candidates) == 0 || channelID > 0 && !slices.ContainsFunc(candidates, func(candidate scheduler.Candidate) bool { return candidate.ID == channelID && !candidate.Excluded }) {
		return
	}
	if state.aliases == nil {
		state.aliases = make(map[scheduler.Key]time.Time)
	}
	state.aliases[key] = now
	state.candidates[key] = candidates
	// An alias routes with its normalized model's configuration: targets,
	// overrides and shared capacity keys must apply to its traffic too.
	if settings, configured := state.settings[normalized]; configured {
		_ = state.engine.SetConfig(key, settings.Config)
		_ = state.engine.ReplaceOverrides(key, settings.Overrides)
		poolConfig := settings.Config
		poolConfig.DefaultCapacity = 0
		_ = state.poolEngine.SetConfig(key, poolConfig)
	}
	state.engine.RegisterCandidates(key, candidates)
	pools := state.poolCandidates[normalized]
	state.poolCandidates[key] = pools
	state.poolEngine.RegisterCandidates(key, pools)
	if now.Sub(state.aliasSweep) < time.Minute {
		return
	}
	state.aliasSweep = now
	for alias, used := range state.aliases {
		if now.Sub(used) < schedulingAliasTTL {
			continue
		}
		delete(state.aliases, alias)
		delete(state.candidates, alias)
		delete(state.poolCandidates, alias)
		// Unregistered dimensions are evicted by the engines once idle.
		state.engine.RegisterCandidates(alias, nil)
		state.poolEngine.RegisterCandidates(alias, nil)
	}
}

type schedulingSnapshot struct {
	entries   []SchedulingConfig
	settings  map[scheduler.Key]scheduler.RoutingConfig
	channels  scheduler.Membership
	pools     scheduler.Membership
	poolByKey map[string]scheduler.Candidate
}

func (work *schedulingRefresh) run(version uint64) {
	snapshot, err := loadSchedulingSnapshot(work.source)
	state := &schedulingConfigState
	state.Lock()
	defer state.Unlock()
	defer close(work.done)
	if state.refresh != work {
		return
	}
	state.refresh = nil
	// A concurrent save or CRUD may have superseded rows read in this generation.
	common.OptionMapRWMutex.RLock()
	currentRaw := common.OptionMap[schedulingOptionKey]
	common.OptionMapRWMutex.RUnlock()
	if currentRaw != work.source.raw {
		return
	}
	if latest := model.RoutingMetadataVersion(); latest != version {
		next := &schedulingRefresh{source: work.source, done: make(chan struct{})}
		state.refresh = next
		go next.run(latest)
		return
	}
	if err == nil {
		poolEngine := state.poolEngine
		if poolEngine == nil || state.engine != work.source.engine || state.database != work.source.database {
			poolEngine = scheduler.New()
		} else {
			// Removed persisted settings revert to defaults. Unmanaged engine keys
			// may also have programmatic overrides; leave those intact.
			for _, previous := range state.entries {
				key := scheduler.Key{Group: previous.Group, Model: previous.Model}
				if _, present := snapshot.settings[key]; !present {
					snapshot.settings[key] = scheduler.RoutingConfig{Config: scheduler.DefaultConfig()}
				}
			}
			// Recently used aliases keep their membership in the new generation;
			// expired ones are dropped from it and later evicted by the engines.
			for key, used := range state.aliases {
				normalized := scheduler.Key{Group: key.Group, Model: ratio_setting.RoutingMatchModelName(key.Model)}
				if _, already := snapshot.channels.Candidates[key]; already || time.Since(used) >= schedulingAliasTTL || len(snapshot.channels.Candidates[normalized]) == 0 {
					delete(state.aliases, key)
					continue
				}
				snapshot.channels.Candidates[key] = snapshot.channels.Candidates[normalized]
				snapshot.pools.Candidates[key] = snapshot.pools.Candidates[normalized]
				if settings, configured := snapshot.settings[normalized]; configured {
					snapshot.settings[key] = settings
				}
			}
		}
		err = work.source.engine.PublishRoutingMembership(snapshot.channels, poolEngine, snapshot.pools, snapshot.settings)
		if err == nil {
			if state.poolEngine != poolEngine {
				clear(state.aliases)
			}
			state.schedulingSource, state.metadataVersion = work.source, version
			state.candidates, state.poolCandidates = snapshot.channels.Candidates, snapshot.pools.Candidates
			state.poolEngine, state.poolByKey = poolEngine, snapshot.poolByKey
			state.entries, state.settings = snapshot.entries, snapshot.settings
			state.failedRefresh, state.failures, state.retryAt = nil, 0, time.Time{}
			return
		}
	}
	work.err = err
	if state.failedRefresh == nil || state.failedRefresh.source != work.source {
		state.failures = 0
	}
	state.failedRefresh = work
	state.failures++
	state.failedAt = time.Now()
	delay := min(time.Duration(1<<min(state.failures-1, 5))*time.Second, 30*time.Second)
	state.retryAt = state.failedAt.Add(delay)
	common.SysError(common.LogText("scheduling metadata reload failed, keeping the previous membership: %s", err.Error()))
	if state.engine == work.source.engine && state.database == work.source.database {
		// Retry a warm generation even when the next request arrives after recovery.
		time.AfterFunc(delay, work.retry)
	}
}

func (work *schedulingRefresh) retry() {
	state := &schedulingConfigState
	state.Lock()
	defer state.Unlock()
	if state.failedRefresh != work || state.refresh != nil {
		return
	}
	common.OptionMapRWMutex.RLock()
	raw := common.OptionMap[schedulingOptionKey]
	common.OptionMapRWMutex.RUnlock()
	if raw != work.source.raw {
		return
	}
	next := &schedulingRefresh{source: work.source, done: make(chan struct{})}
	state.refresh = next
	go next.run(model.RoutingMetadataVersion())
}

func loadSchedulingSnapshot(source schedulingSource) (*schedulingSnapshot, error) {
	snapshot := &schedulingSnapshot{settings: make(map[scheduler.Key]scheduler.RoutingConfig)}
	if source.raw != "" {
		if len(source.raw) > 4<<20 {
			return nil, errors.New("scheduling configuration exceeds 4 MiB")
		}
		if err := common.UnmarshalJsonStr(source.raw, &snapshot.entries); err != nil {
			return nil, fmt.Errorf("invalid scheduling configuration: %w", err)
		}
	}
	for _, entry := range snapshot.entries {
		if err := ValidateSchedulingConfig(entry); err != nil {
			return nil, err
		}
		key := scheduler.Key{Group: entry.Group, Model: entry.Model}
		if _, duplicate := snapshot.settings[key]; duplicate {
			return nil, errors.New("duplicate group/model scheduling configuration")
		}
		overrides := make(map[int]scheduler.Override, len(entry.ChannelOverrides))
		for _, override := range entry.ChannelOverrides {
			overrides[override.ChannelID] = scheduler.Override{Weight: override.Weight, Capacity: override.Capacity, CapacityKey: override.CapacityKey}
		}
		snapshot.settings[key] = scheduler.RoutingConfig{
			Config:    scheduler.Config{Enabled: entry.Enabled, SuccessTarget: entry.TargetSuccessRate, LatencyTargetMS: entry.TargetTTFTMS, DefaultCapacity: entry.DefaultCapacity},
			Overrides: overrides,
		}
	}
	var channels []*model.Channel
	var abilities []model.Ability
	accounts := make(map[string]model.SupplierAccount)
	// Keep channels, abilities and supplier settings on one database snapshot,
	// including when a writer on another node changes them during the reads.
	options := &sql.TxOptions{Isolation: sql.LevelRepeatableRead, ReadOnly: true}
	if source.database.Dialector.Name() == "sqlite" {
		options = &sql.TxOptions{ReadOnly: true}
	}
	err := source.database.Transaction(func(tx *gorm.DB) error {

		if err := tx.Select("id", "name", "status", "group", "models", "priority", "weight", "owner_supplier_id", "supplier_priority").Find(&channels).Error; err != nil {
			return fmt.Errorf("unable to load scheduling capacity members: %w", err)
		}

		if err := tx.Select("group", "model", "channel_id").Where("enabled = ?", true).Find(&abilities).Error; err != nil {
			return fmt.Errorf("unable to load scheduling ability members: %w", err)
		}

		if slices.ContainsFunc(channels, func(ch *model.Channel) bool { return ch.OwnerSupplierID != "" }) {
			var rows []model.SupplierAccount
			if err := tx.Select("supplier_id", "pool_priority", "pool_weight", "pool_capacity").Find(&rows).Error; err != nil {
				return fmt.Errorf("unable to load scheduling supplier pools: %w", err)
			}
			for _, row := range rows {
				accounts[row.SupplierID] = row
			}
		}
		return nil
	}, options)
	if err != nil {
		if !strings.HasPrefix(err.Error(), "unable to load") {
			err = fmt.Errorf("unable to load scheduling metadata: %w", err)
		}
		return nil, err
	}
	enabled := make(map[scheduler.Key]map[int]bool)
	for _, ability := range abilities {
		key := scheduler.Key{Group: ability.Group, Model: ability.Model}
		if enabled[key] == nil {
			enabled[key] = make(map[int]bool)
		}
		enabled[key][ability.ChannelId] = true
	}
	candidates := make(map[scheduler.Key][]scheduler.Candidate)
	for _, channel := range channels {
		canonical := SchedulerCandidate(channel)
		snapshot.channels.Metadata = append(snapshot.channels.Metadata, canonical)
		models := channel.GetModels()
		for _, group := range channel.GetGroups() {
			for _, name := range models {
				key := scheduler.Key{Group: group, Model: name}
				member := canonical
				member.Excluded = !enabled[key][channel.Id]
				candidates[key] = append(candidates[key], member)
			}
		}
	}
	for key := range snapshot.settings {
		if _, ok := candidates[key]; !ok {
			candidates[key] = nil
		}
	}
	// Exact models may fall back to their normalized model when a request
	// filter removes every exact candidate. Keep both memberships in the pool.
	for key, members := range candidates {
		normalized := scheduler.Key{Group: key.Group, Model: ratio_setting.RoutingMatchModelName(key.Model)}
		if normalized == key {
			continue
		}
		merged := slices.Clone(members)
		for _, alternative := range candidates[normalized] {
			i := slices.IndexFunc(merged, func(member scheduler.Candidate) bool { return member.ID == alternative.ID })
			if i < 0 {
				merged = append(merged, alternative)
			} else {
				merged[i].Excluded = merged[i].Excluded && alternative.Excluded
			}
		}
		candidates[key] = merged
	}
	snapshot.channels.Candidates = candidates
	snapshot.pools, snapshot.poolByKey, err = buildSupplierMembership(channels, candidates, accounts)
	if err != nil {
		return nil, err
	}
	return snapshot, nil
}

func GetSchedulingConfig(key scheduler.Key) SchedulingConfig {
	schedulingConfigState.Lock()
	defer schedulingConfigState.Unlock()
	for _, entry := range schedulingConfigState.entries {
		if entry.Group == key.Group && entry.Model == key.Model {
			return entry
		}
	}
	return SchedulingConfig{Group: key.Group, Model: key.Model, Enabled: true, TargetSuccessRate: .95, TargetTTFTMS: 3000, ChannelOverrides: []SchedulingChannelOverride{}}
}

// SaveSchedulingConfig persists one group/model entry and publishes it. Blank
// channel overrides are dropped, and an entry equal to the defaults is removed,
// so administrators can clear overrides from the same form. It returns the
// normalized entry. A non-nil syncErr means the value is persisted but this
// instance has not published it yet; routing keeps the previous snapshot while
// the load retries.
func SaveSchedulingConfig(config SchedulingConfig) (saved SchedulingConfig, syncErr error, err error) {
	if err := ValidateSchedulingConfig(config); err != nil {
		return config, nil, err
	}
	config.ChannelOverrides = slices.DeleteFunc(slices.Clone(config.ChannelOverrides), func(override SchedulingChannelOverride) bool {
		return override.Weight == nil && override.Capacity == nil && override.CapacityKey == ""
	})
	if config.ChannelOverrides == nil {
		config.ChannelOverrides = []SchedulingChannelOverride{}
	}
	defaults := config.Enabled && config.TargetSuccessRate == .95 && config.TargetTTFTMS == 3000 && config.DefaultCapacity == 0 && len(config.ChannelOverrides) == 0
	schedulingSaveLock.Lock()
	defer schedulingSaveLock.Unlock()
	// An invalid persisted value must not block the administrator from
	// replacing it; edit the last configuration that was actually published.
	if err := SyncSchedulingConfig(); err != nil && !GetSchedulingConfigStatus().Published {
		return config, nil, err
	}
	schedulingConfigState.Lock()
	entries := slices.Clone(schedulingConfigState.entries)
	schedulingConfigState.Unlock()
	index := slices.IndexFunc(entries, func(entry SchedulingConfig) bool { return entry.Group == config.Group && entry.Model == config.Model })
	switch {
	case index >= 0 && defaults:
		entries = slices.Delete(entries, index, index+1)
	case index >= 0:
		entries[index] = config
	case !defaults:
		entries = append(entries, config)
	}
	encoded, err := common.Marshal(entries)
	if err != nil {
		return config, nil, err
	}
	if len(encoded) > 4<<20 {
		return config, nil, errors.New("scheduling configuration exceeds 4 MiB")
	}
	// Bulk options propagates database failures before changing the live cache.
	if err := model.UpdateOptionsBulk(map[string]string{schedulingOptionKey: string(encoded)}); err != nil {
		return config, nil, err
	}
	if err := SyncSchedulingConfig(); err != nil {
		common.SysError(common.LogText("scheduling configuration saved but not yet applied: %s", err.Error()))
		return config, err, nil
	}
	return config, nil, nil
}
