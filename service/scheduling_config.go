package service

import (
	"errors"
	"fmt"
	"math"
	"slices"
	"strings"
	"sync"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/pkg/scheduler"
	"github.com/QuantumNous/new-api/setting/ratio_setting"
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

var schedulingConfigState = struct {
	sync.Mutex
	raw     string
	entries []SchedulingConfig
}{}

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
	if config.DefaultCapacity < 1 || config.DefaultCapacity > 100000 {
		return errors.New("default_capacity must be between 1 and 100000")
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

// SyncSchedulingConfig applies only changed option snapshots, so the routing
// hot path never reads the database or resets the scheduler's learning state.
func SyncSchedulingConfig() error {
	schedulingConfigState.Lock()
	defer schedulingConfigState.Unlock()
	return syncSchedulingConfigLocked()
}

func syncSchedulingConfigLocked() error {
	common.OptionMapRWMutex.RLock()
	raw := common.OptionMap[schedulingOptionKey]
	common.OptionMapRWMutex.RUnlock()
	if raw == schedulingConfigState.raw {
		return nil
	}
	entries := []SchedulingConfig{}
	if raw != "" {
		if len(raw) > 4<<20 {
			return errors.New("scheduling configuration exceeds 4 MiB")
		}
		if err := common.UnmarshalJsonStr(raw, &entries); err != nil {
			return fmt.Errorf("invalid scheduling configuration: %w", err)
		}
	}
	seen := make(map[scheduler.Key]bool, len(entries))
	for _, entry := range entries {
		if err := ValidateSchedulingConfig(entry); err != nil {
			return err
		}
		key := scheduler.Key{Group: entry.Group, Model: entry.Model}
		if seen[key] {
			return errors.New("duplicate group/model scheduling configuration")
		}
		seen[key] = true
	}
	// Register every configured dimension before allowing another selection.
	// Otherwise an unvisited group with a lower shared-pool limit would not
	// constrain traffic arriving through a group that was already active.
	channels, err := model.GetAllChannels(0, -1, false, true)
	if err != nil {
		return fmt.Errorf("unable to load scheduling capacity members: %w", err)
	}
	candidateSets := make(map[scheduler.Key][]scheduler.Candidate)
	allEntries := append(append([]SchedulingConfig(nil), entries...), schedulingConfigState.entries...)
	for _, entry := range allEntries {
		key := scheduler.Key{Group: entry.Group, Model: entry.Model}
		if _, exists := candidateSets[key]; exists {
			continue
		}
		candidates := make([]scheduler.Candidate, 0)
		for _, channel := range channels {
			if slices.Contains(channel.GetGroups(), key.Group) && (slices.Contains(channel.GetModels(), key.Model) || slices.Contains(channel.GetModels(), ratio_setting.RoutingMatchModelName(key.Model))) {
				candidates = append(candidates, SchedulerCandidate(channel))
			}
		}
		candidateSets[key] = candidates
	}
	for _, previous := range schedulingConfigState.entries {
		key := scheduler.Key{Group: previous.Group, Model: previous.Model}
		if !seen[key] {
			if err := scheduler.Default.SetConfig(key, scheduler.Config{Enabled: true, SuccessTarget: .95, LatencyTargetMS: 3000, DefaultCapacity: 100}); err != nil {
				return err
			}
			if err := scheduler.Default.ReplaceOverrides(key, nil); err != nil {
				return err
			}
			scheduler.Default.RegisterCandidates(key, candidateSets[key])
		}
	}
	for _, entry := range entries {
		key := scheduler.Key{Group: entry.Group, Model: entry.Model}
		if err := scheduler.Default.SetConfig(key, scheduler.Config{Enabled: entry.Enabled, SuccessTarget: entry.TargetSuccessRate, LatencyTargetMS: entry.TargetTTFTMS, DefaultCapacity: entry.DefaultCapacity}); err != nil {
			return err
		}
		overrides := make(map[int]scheduler.Override, len(entry.ChannelOverrides))
		for _, override := range entry.ChannelOverrides {
			overrides[override.ChannelID] = scheduler.Override{Weight: override.Weight, Capacity: override.Capacity, CapacityKey: override.CapacityKey}
		}
		if err := scheduler.Default.ReplaceOverrides(key, overrides); err != nil {
			return err
		}
		scheduler.Default.RegisterCandidates(key, candidateSets[key])
	}
	schedulingConfigState.entries = entries
	schedulingConfigState.raw = raw
	return nil
}

func GetSchedulingConfig(key scheduler.Key) SchedulingConfig {
	schedulingConfigState.Lock()
	defer schedulingConfigState.Unlock()
	for _, entry := range schedulingConfigState.entries {
		if entry.Group == key.Group && entry.Model == key.Model {
			return entry
		}
	}
	return SchedulingConfig{Group: key.Group, Model: key.Model, Enabled: true, TargetSuccessRate: .95, TargetTTFTMS: 3000, DefaultCapacity: 100, ChannelOverrides: []SchedulingChannelOverride{}}
}

func SaveSchedulingConfig(config SchedulingConfig) error {
	if err := ValidateSchedulingConfig(config); err != nil {
		return err
	}
	schedulingConfigState.Lock()
	defer schedulingConfigState.Unlock()
	if err := syncSchedulingConfigLocked(); err != nil {
		return err
	}
	entries := append([]SchedulingConfig(nil), schedulingConfigState.entries...)
	found := false
	for index, entry := range entries {
		if entry.Group == config.Group && entry.Model == config.Model {
			entries[index] = config
			found = true
			break
		}
	}
	if !found {
		entries = append(entries, config)
	}
	encoded, err := common.Marshal(entries)
	if err != nil {
		return err
	}
	if len(encoded) > 4<<20 {
		return errors.New("scheduling configuration exceeds 4 MiB")
	}
	// Bulk options propagates database failures before changing the live cache.
	if err := model.UpdateOptionsBulk(map[string]string{schedulingOptionKey: string(encoded)}); err != nil {
		return err
	}
	return syncSchedulingConfigLocked()
}
