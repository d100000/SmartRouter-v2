package model

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
)

const PerformanceHealthPresetVersion = "2026-10-27.1"

var performanceHealthPresetMu sync.RWMutex
var performanceHealthPresetModels = map[string][]string{
	"moonshot":  {"kimi-k3", "kimi-k2.5", "kimi-k2-thinking"},
	"openai":    {"gpt-4.1", "gpt-6.1-sol", "gpt-5.6-sol", "gpt-6-astra", "gpt-6-sol", "gpt-6-luna", "gpt-5.6-terra", "gpt-5.6-luna"},
	"anthropic": {"claude-sonnet-4-6", "claude-opus-4-8"},
}

type PerformanceHealthPresetPreview struct {
	Config                PerformanceHealthConfig `json:"config"`
	ExpectedConfigVersion int64                   `json:"expected_config_version"`
	PreviewToken          string                  `json:"preview_token"`
	AddedCompanies        []string                `json:"added_companies"`
	AddedGroups           []string                `json:"added_groups"`
	AddedModels           []string                `json:"added_models"`
	Conflicts             []string                `json:"conflicts"`
	SkippedModels         []string                `json:"skipped_models"`
}

// SetPerformanceHealthPresetModels is called with each adaptor's model catalog
// during host startup. Keeping registration here avoids model/adaptor cycles and
// a second copy of the full provider catalogs.
func SetPerformanceHealthPresetModels(companyKey string, models []string) error {
	if companyKey != "moonshot" && companyKey != "openai" && companyKey != "anthropic" {
		return common.NewMessage("Unknown performance health preset company")
	}
	performanceHealthPresetMu.Lock()
	defer performanceHealthPresetMu.Unlock()
	performanceHealthPresetModels[companyKey] = normalizeModelNames(append(slices.Clone(performanceHealthPresetModels[companyKey]), models...))
	return nil
}

func GetPerformanceHealthPresets() PerformanceHealthConfig {
	performanceHealthPresetMu.RLock()
	defer performanceHealthPresetMu.RUnlock()
	config := PerformanceHealthConfig{Version: 1, PresetVersion: PerformanceHealthPresetVersion, Companies: []PerformanceCompany{}, ExcludedModels: []string{}, ExcludedCompanies: []string{}, ExcludedGroups: []string{}}
	for _, preset := range []struct{ key, name string }{{"moonshot", "Kimi"}, {"openai", "Openai"}, {"anthropic", "Claude"}} {
		unconfirmed := false
		config.Companies = append(config.Companies, PerformanceCompany{
			ID: preset.key, Name: preset.name, PresetKey: preset.key, Status: "active", SLO: 99.9, SLOConfirmed: &unconfirmed,
			Groups: []PerformanceModelGroup{{ID: preset.key + "-default", PresetKey: preset.key, Name: "Default models", Models: normalizeModelNames(performanceHealthPresetModels[preset.key]), Enabled: true, SLO: 99.9, SLOConfirmed: &unconfirmed, MinSamples: 100}},
		})
	}
	return config
}

func performanceHealthCompanyPresetKey(company PerformanceCompany) string {
	if company.PresetKey == "moonshot" || company.PresetKey == "openai" || company.PresetKey == "anthropic" {
		return company.PresetKey
	}
	switch strings.ToLower(strings.TrimSpace(company.Name)) {
	case "kimi", "moonshot", "moonshot ai", "月之暗面":
		return "moonshot"
	case "openai":
		return "openai"
	case "claude", "anthropic":
		return "anthropic"
	default:
		return ""
	}
}

// performanceHealthModelCandidates combines exact catalogs with persisted model
// names. Family matching is anchored and is only proposed for explicit preview;
// unknown aliases such as codex-auto-review remain unassigned.
func performanceHealthModelCandidates(db *gorm.DB, presets PerformanceHealthConfig) (map[string]string, error) {
	candidates := make(map[string]string)
	for _, company := range presets.Companies {
		for _, group := range company.Groups {
			for _, name := range group.Models {
				candidates[name] = company.PresetKey
			}
		}
	}
	known := make(map[string]struct{})
	if db.Migrator().HasTable(&Channel{}) {
		var channels []Channel
		if err := db.Select("models").Find(&channels).Error; err != nil {
			return nil, err
		}
		for _, channel := range channels {
			for name := range strings.SplitSeq(channel.Models, ",") {
				if name = strings.TrimSpace(name); name != "" {
					known[name] = struct{}{}
				}
			}
		}
	}
	if db.Migrator().HasTable(&PerfMetric{}) {
		var names []string
		if err := db.Model(&PerfMetric{}).Distinct("model_name").Pluck("model_name", &names).Error; err != nil {
			return nil, err
		}
		for _, name := range names {
			if name = strings.TrimSpace(name); name != "" {
				known[name] = struct{}{}
			}
		}
	}
	var records []Model
	if db.Migrator().HasTable(&Model{}) {
		if err := db.Order("id").Find(&records).Error; err != nil {
			return nil, err
		}
		for _, record := range records {
			if record.NameRule == NameRuleExact {
				known[record.ModelName] = struct{}{}
			}
		}
	}
	var vendors []Vendor
	if db.Migrator().HasTable(&Vendor{}) {
		if err := db.Find(&vendors).Error; err != nil {
			return nil, err
		}
	}
	vendorKeys := make(map[int]string, len(vendors))
	for _, vendor := range vendors {
		vendorKeys[vendor.Id] = performanceHealthCompanyPresetKey(PerformanceCompany{Name: vendor.Name})
	}
	names := make([]string, 0, len(known))
	for name := range known {
		names = append(names, name)
	}
	slices.Sort(names)
	resolved := resolveModelMetadata(records, names)
	for _, name := range names {
		if record := resolved[name]; record != nil && record.VendorID > 0 {
			candidates[name] = vendorKeys[record.VendorID]
			continue
		}
		if _, exact := candidates[name]; exact {
			continue
		}
		lower := strings.ToLower(name)
		switch {
		case strings.HasPrefix(lower, "kimi-"), strings.HasPrefix(lower, "moonshot-v1-"):
			candidates[name] = "moonshot"
		case strings.HasPrefix(lower, "gpt-"), strings.HasPrefix(lower, "o1-"), lower == "o1", strings.HasPrefix(lower, "o3-"), lower == "o3", strings.HasPrefix(lower, "o4-"):
			candidates[name] = "openai"
		case strings.HasPrefix(lower, "claude-"):
			candidates[name] = "anthropic"
		default:
			candidates[name] = ""
		}
	}
	return candidates, nil
}

func PreviewPerformanceHealthPresets() (PerformanceHealthPresetPreview, error) {
	current, err := GetPerformanceHealthConfig()
	if err != nil {
		return PerformanceHealthPresetPreview{}, err
	}
	return previewPerformanceHealthPresets(DB, current)
}

func previewPerformanceHealthPresets(db *gorm.DB, current PerformanceHealthConfig) (PerformanceHealthPresetPreview, error) {
	config, err := clonePerformanceHealthConfig(current)
	if err != nil {
		return PerformanceHealthPresetPreview{}, err
	}
	presets := GetPerformanceHealthPresets()
	candidates, err := performanceHealthModelCandidates(db, presets)
	if err != nil {
		return PerformanceHealthPresetPreview{}, err
	}
	result := PerformanceHealthPresetPreview{Config: config, ExpectedConfigVersion: current.Version, AddedCompanies: []string{}, AddedGroups: []string{}, AddedModels: []string{}, Conflicts: []string{}, SkippedModels: []string{}}
	companyByKey := make(map[string][]int)
	modelOwners := make(map[string]string)
	groupIDs := make(map[string]struct{})
	for i, company := range result.Config.Companies {
		if key := performanceHealthCompanyPresetKey(company); key != "" {
			companyByKey[key] = append(companyByKey[key], i)
		}
		for _, group := range company.Groups {
			groupIDs[group.ID] = struct{}{}
			for _, modelName := range group.Models {
				modelOwners[modelName] = company.ID
			}
		}
	}
	names := make([]string, 0, len(candidates))
	for name := range candidates {
		names = append(names, name)
	}
	slices.Sort(names)
	for _, preset := range presets.Companies {
		key := preset.PresetKey
		indices := companyByKey[key]
		if len(indices) > 1 {
			result.Conflicts = append(result.Conflicts, preset.Name)
			continue
		}
		if slices.Contains(result.Config.ExcludedCompanies, key) {
			continue
		}
		index := -1
		if len(indices) == 1 {
			index = indices[0]
		} else {
			id := preset.ID
			for _, company := range result.Config.Companies {
				if company.ID == id {
					id = key + "-preset"
					break
				}
			}
			for suffix := 1; ; suffix++ {
				used := false
				for _, company := range result.Config.Companies {
					if company.ID == id {
						used = true
						break
					}
				}
				if !used {
					break
				}
				id = fmt.Sprintf("%s-preset-%d", key, suffix)
			}
			preset.ID, preset.Groups = id, []PerformanceModelGroup{}
			result.Config.Companies = append(result.Config.Companies, preset)
			index = len(result.Config.Companies) - 1
			result.AddedCompanies = append(result.AddedCompanies, preset.Name)
		}
		company := &result.Config.Companies[index]
		if company.Status == "disabled" {
			for _, name := range names {
				if candidates[name] == key {
					result.SkippedModels = append(result.SkippedModels, name)
				}
			}
			continue
		}
		company.PresetKey = key
		groupIndex := -1
		for i, group := range company.Groups {
			if group.PresetKey == key || group.ID == key+"-default" || group.ID == company.ID+"-default" {
				groupIndex = i
				break
			}
		}
		if groupIndex == -1 {
			if slices.Contains(result.Config.ExcludedGroups, key+":default") || slices.Contains(result.Config.ExcludedGroups, key+"-default") || slices.Contains(result.Config.ExcludedGroups, company.ID+"-default") {
				continue
			}
			id := key + "-default"
			for suffix := 1; ; suffix++ {
				if _, exists := groupIDs[id]; !exists {
					break
				}
				id = fmt.Sprintf("%s-default-%d", key, suffix)
			}
			unconfirmed := false
			company.Groups = append(company.Groups, PerformanceModelGroup{ID: id, PresetKey: key, Name: "Default models", Models: []string{}, Enabled: true, SLO: company.SLO, SLOConfirmed: &unconfirmed, MinSamples: 100})
			groupIDs[id] = struct{}{}
			groupIndex = len(company.Groups) - 1
			result.AddedGroups = append(result.AddedGroups, company.Name+" / Default models")
		}
		group := &company.Groups[groupIndex]
		if !group.Enabled {
			continue
		}
		for _, name := range names {
			if candidates[name] != key {
				continue
			}
			if owner, assigned := modelOwners[name]; assigned {
				if owner != company.ID {
					result.Conflicts = append(result.Conflicts, name)
				}
				continue
			}
			if slices.Contains(result.Config.ExcludedModels, name) {
				result.SkippedModels = append(result.SkippedModels, name)
				continue
			}
			group.Models = append(group.Models, name)
			modelOwners[name] = company.ID
			result.AddedModels = append(result.AddedModels, name)
		}
		group.Models = normalizeModelNames(group.Models)
	}
	for _, name := range names {
		if candidates[name] == "" {
			result.SkippedModels = append(result.SkippedModels, name)
		}
	}
	result.Config.PresetVersion = PerformanceHealthPresetVersion
	result.Conflicts = normalizeModelNames(result.Conflicts)
	result.SkippedModels = normalizeModelNames(result.SkippedModels)
	result.AddedModels = normalizeModelNames(result.AddedModels)
	if err := validatePerformanceHealthConfig(result.Config); err != nil {
		return PerformanceHealthPresetPreview{}, err
	}
	encoded, err := common.Marshal(result)
	if err != nil {
		return PerformanceHealthPresetPreview{}, err
	}
	result.PreviewToken = fmt.Sprintf("%x", sha256.Sum256(encoded))
	return result, nil
}

func ApplyPerformanceHealthPresets(expectedVersion int64, previewToken string) (PerformanceHealthConfig, error) {
	if DB == nil {
		return PerformanceHealthConfig{}, common.NewMessage("Performance health database is unavailable")
	}
	performanceHealthConfigMu.Lock()
	defer performanceHealthConfigMu.Unlock()
	var saved PerformanceHealthConfig
	change := func(tx *gorm.DB) error {
		var option Option
		if err := lockForUpdate(tx).Where(map[string]any{"key": PerformanceHealthConfigOption}).Take(&option).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrPerformanceHealthConflict
			}
			return err
		}
		current, err := decodePerformanceHealthConfig(option.Value)
		if err != nil {
			return err
		}
		if current.Version != expectedVersion {
			return ErrPerformanceHealthConflict
		}
		preview, err := previewPerformanceHealthPresets(tx, current)
		if err != nil {
			return err
		}
		if previewToken == "" || previewToken != preview.PreviewToken {
			return ErrPerformanceHealthConflict
		}
		before, err := common.Marshal(current)
		if err != nil {
			return err
		}
		after, err := common.Marshal(preview.Config)
		if err != nil {
			return err
		}
		if bytes.Equal(before, after) {
			saved = current
			return nil
		}
		saved, err = savePerformanceHealthConfig(tx, option, current, preview.Config)
		return err
	}
	var err error
	if common.UsingMainDatabase(common.DatabaseTypeSQLite) {
		err = change(DB)
	} else {
		err = DB.Transaction(change)
	}
	if err != nil {
		return PerformanceHealthConfig{}, err
	}
	cachePerformanceHealthConfig(saved)
	return saved, nil
}

func protectPerformanceHealthRemovals(current PerformanceHealthConfig, next *PerformanceHealthConfig) {
	companyIDs, groupIDs, modelNames := make(map[string]struct{}), make(map[string]struct{}), make(map[string]struct{})
	next.ExcludedCompanies = normalizeModelNames(append(slices.Clone(current.ExcludedCompanies), next.ExcludedCompanies...))
	next.ExcludedGroups = normalizeModelNames(append(slices.Clone(current.ExcludedGroups), next.ExcludedGroups...))
	next.ExcludedModels = normalizeModelNames(append(slices.Clone(current.ExcludedModels), next.ExcludedModels...))
	for _, company := range next.Companies {
		companyIDs[company.ID] = struct{}{}
		if key := performanceHealthCompanyPresetKey(company); key != "" {
			next.ExcludedCompanies = slices.DeleteFunc(next.ExcludedCompanies, func(value string) bool { return value == key })
		}
		for _, group := range company.Groups {
			groupIDs[group.ID] = struct{}{}
			next.ExcludedGroups = slices.DeleteFunc(next.ExcludedGroups, func(value string) bool { return value == group.ID })
			for _, name := range group.Models {
				modelNames[name] = struct{}{}
			}
		}
	}
	for _, company := range current.Companies {
		if _, remains := companyIDs[company.ID]; !remains {
			if key := performanceHealthCompanyPresetKey(company); key != "" {
				next.ExcludedCompanies = append(next.ExcludedCompanies, key)
			}
		}
		for _, group := range company.Groups {
			if _, remains := groupIDs[group.ID]; !remains {
				next.ExcludedGroups = append(next.ExcludedGroups, group.ID)
				key := group.PresetKey
				if key == "" && (group.ID == performanceHealthCompanyPresetKey(company)+"-default" || group.ID == company.ID+"-default") {
					key = performanceHealthCompanyPresetKey(company)
				}
				if key != "" {
					next.ExcludedGroups = append(next.ExcludedGroups, key+":default")
				}
			}
			for _, name := range group.Models {
				if _, remains := modelNames[name]; !remains {
					next.ExcludedModels = append(next.ExcludedModels, name)
				}
			}
		}
	}
	next.ExcludedCompanies = normalizeModelNames(next.ExcludedCompanies)
	next.ExcludedGroups = normalizeModelNames(next.ExcludedGroups)
	next.ExcludedModels = normalizeModelNames(next.ExcludedModels)
	next.ExcludedModels = slices.DeleteFunc(next.ExcludedModels, func(name string) bool { _, assigned := modelNames[name]; return assigned })
}
