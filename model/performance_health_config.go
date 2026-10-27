package model

import (
	"errors"
	"math"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

// PerformanceHealthConfigOption stores the dashboard's administrative
// company/model ownership map in the existing options table.
const PerformanceHealthConfigOption = "PerformanceHealthConfig"

type PerformanceHealthConfig struct {
	Version           int64                `json:"version"`
	UpdatedAt         int64                `json:"updated_at"`
	Companies         []PerformanceCompany `json:"companies"`
	PresetVersion     string               `json:"preset_version,omitempty"`
	ExcludedModels    []string             `json:"excluded_models"`
	ExcludedCompanies []string             `json:"excluded_companies"`
	ExcludedGroups    []string             `json:"excluded_groups"`
}

type PerformanceCompany struct {
	ID           string                  `json:"id"`
	Name         string                  `json:"name"`
	Status       string                  `json:"status"`
	SLO          float64                 `json:"slo"`
	SLOConfirmed *bool                   `json:"slo_confirmed,omitempty"`
	PresetKey    string                  `json:"preset_key,omitempty"`
	Owner        string                  `json:"owner,omitempty"`
	Note         string                  `json:"note,omitempty"`
	Groups       []PerformanceModelGroup `json:"groups"`
}

type PerformanceModelGroup struct {
	ID           string   `json:"id"`
	PresetKey    string   `json:"preset_key,omitempty"`
	Name         string   `json:"name"`
	Models       []string `json:"models"`
	Enabled      bool     `json:"enabled"`
	SLO          float64  `json:"slo"`
	SLOConfirmed *bool    `json:"slo_confirmed,omitempty"`
	P95LatencyMs int64    `json:"p95_latency_ms"`
	MinSamples   int64    `json:"min_samples"`
}

var performanceHealthConfigMu sync.Mutex

var ErrPerformanceHealthConflict = common.NewMessage("Performance health configuration changed; reload before saving")

func GetPerformanceHealthConfig() (PerformanceHealthConfig, error) {
	if DB == nil {
		return PerformanceHealthConfig{}, common.NewMessage("Performance health database is unavailable")
	}
	var option Option
	if err := DB.Where(map[string]any{"key": PerformanceHealthConfigOption}).Take(&option).Error; err != nil {
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return decodePerformanceHealthConfig("")
		}
		return PerformanceHealthConfig{}, err
	}
	return decodePerformanceHealthConfig(option.Value)
}

func decodePerformanceHealthConfig(raw string) (PerformanceHealthConfig, error) {
	if strings.TrimSpace(raw) == "" {
		return PerformanceHealthConfig{Version: 1, Companies: []PerformanceCompany{}, ExcludedModels: []string{}, ExcludedCompanies: []string{}, ExcludedGroups: []string{}}, nil
	}
	var config PerformanceHealthConfig
	if err := common.UnmarshalJsonStr(raw, &config); err != nil {
		return PerformanceHealthConfig{}, common.NewMessage("Performance health configuration is invalid")
	}
	if err := validatePerformanceHealthConfig(config); err != nil {
		return PerformanceHealthConfig{}, err
	}
	if config.Version == 0 {
		config.Version = 1
	}
	if config.Companies == nil {
		config.Companies = []PerformanceCompany{}
	}
	config.ExcludedModels = normalizeModelNames(config.ExcludedModels)
	config.ExcludedCompanies = normalizeModelNames(config.ExcludedCompanies)
	config.ExcludedGroups = normalizeModelNames(config.ExcludedGroups)
	return config, nil
}

// InitPerformanceHealthConfig seeds an absent option once at startup. Existing
// configuration, including a deliberately empty saved configuration, is intact.
func InitPerformanceHealthConfig() error {
	if DB == nil {
		return common.NewMessage("Performance health database is unavailable")
	}
	performanceHealthConfigMu.Lock()
	defer performanceHealthConfigMu.Unlock()
	var count int64
	if err := DB.Model(&Option{}).Where(map[string]any{"key": PerformanceHealthConfigOption}).Count(&count).Error; err != nil {
		return err
	}
	if count == 0 {
		initial, err := decodePerformanceHealthConfig("")
		if err != nil {
			return err
		}
		preview, err := previewPerformanceHealthPresets(DB, initial)
		if err != nil {
			return err
		}
		initial = preview.Config
		initial.UpdatedAt = time.Now().Unix()
		raw, err := common.Marshal(initial)
		if err != nil {
			return err
		}
		// The insert is the first statement in its write transaction. In
		// SQLite a prior read in the same deferred transaction cannot safely
		// upgrade to a writer when another process is initializing too.
		if err := DB.Clauses(clause.OnConflict{Columns: []clause.Column{{Name: "key"}}, DoNothing: true}).Create(&Option{Key: PerformanceHealthConfigOption, Value: string(raw)}).Error; err != nil {
			return err
		}
	}
	config, err := GetPerformanceHealthConfig()
	if err != nil {
		return err
	}
	cachePerformanceHealthConfig(config)
	return nil
}

// SavePerformanceHealthConfig treats Version as the editor's expected version.
// Row locking plus a value CAS protects independently running instances; the
// mutex also serializes SQLite writers within the current process.
func SavePerformanceHealthConfig(config PerformanceHealthConfig) (PerformanceHealthConfig, error) {
	if DB == nil {
		return PerformanceHealthConfig{}, common.NewMessage("Performance health database is unavailable")
	}
	cloned, err := clonePerformanceHealthConfig(config)
	if err != nil {
		return PerformanceHealthConfig{}, err
	}
	if err := validatePerformanceHealthConfig(cloned); err != nil {
		return PerformanceHealthConfig{}, err
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
		saved, err = savePerformanceHealthConfig(tx, option, current, cloned)
		return err
	}
	if common.UsingMainDatabase(common.DatabaseTypeSQLite) {
		// A single-row compare-and-set is the commit boundary. Reading
		// before that write transaction avoids SQLite lock-upgrade races.
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

func savePerformanceHealthConfig(tx *gorm.DB, option Option, current, config PerformanceHealthConfig) (PerformanceHealthConfig, error) {
	if config.Version != current.Version || current.Version == math.MaxInt64 {
		return PerformanceHealthConfig{}, ErrPerformanceHealthConflict
	}
	// Older clients omit preset metadata. Preserve its confirmation state so
	// saving an unrelated field cannot activate an unconfirmed template SLO.
	companies := make(map[string]PerformanceCompany, len(current.Companies))
	groups := make(map[string]PerformanceModelGroup)
	for _, company := range current.Companies {
		companies[company.ID] = company
		for _, group := range company.Groups {
			groups[group.ID] = group
		}
	}
	for i := range config.Companies {
		company := &config.Companies[i]
		previous := companies[company.ID]
		if company.PresetKey == "" {
			company.PresetKey = previous.PresetKey
		}
		if company.SLOConfirmed == nil {
			company.SLOConfirmed = previous.SLOConfirmed
		}
		for j := range company.Groups {
			group := &company.Groups[j]
			previous := groups[group.ID]
			if group.PresetKey == "" {
				group.PresetKey = previous.PresetKey
			}
			if group.SLOConfirmed == nil {
				group.SLOConfirmed = previous.SLOConfirmed
			}
		}
	}
	protectPerformanceHealthRemovals(current, &config)
	if config.PresetVersion == "" {
		config.PresetVersion = current.PresetVersion
	}
	config.Version = current.Version + 1
	config.UpdatedAt = time.Now().Unix()
	if config.Companies == nil {
		config.Companies = []PerformanceCompany{}
	}
	if err := validatePerformanceHealthConfig(config); err != nil {
		return PerformanceHealthConfig{}, err
	}
	raw, err := common.Marshal(config)
	if err != nil {
		return PerformanceHealthConfig{}, err
	}
	result := tx.Model(&Option{}).Where(map[string]any{"key": PerformanceHealthConfigOption, "value": option.Value}).Update("value", string(raw))
	if result.Error != nil {
		return PerformanceHealthConfig{}, result.Error
	}
	if result.RowsAffected != 1 {
		return PerformanceHealthConfig{}, ErrPerformanceHealthConflict
	}
	return config, nil
}

func clonePerformanceHealthConfig(config PerformanceHealthConfig) (PerformanceHealthConfig, error) {
	raw, err := common.Marshal(config)
	if err != nil {
		return PerformanceHealthConfig{}, err
	}
	var clone PerformanceHealthConfig
	if err := common.Unmarshal(raw, &clone); err != nil {
		return PerformanceHealthConfig{}, err
	}
	return clone, nil
}

func cachePerformanceHealthConfig(config PerformanceHealthConfig) {
	raw, err := common.Marshal(config)
	if err != nil {
		return
	}
	common.OptionMapRWMutex.Lock()
	if common.OptionMap == nil {
		common.OptionMap = make(map[string]string)
	}
	common.OptionMap[PerformanceHealthConfigOption] = string(raw)
	common.OptionMapRWMutex.Unlock()
}

func validatePerformanceHealthConfig(config PerformanceHealthConfig) error {
	if len(config.Companies) > 100 {
		return common.NewMessage("Performance health supports at most 100 companies")
	}
	companyIDs := make(map[string]struct{}, len(config.Companies))
	groupIDs := make(map[string]struct{})
	modelOwners := make(map[string]string)
	for i := range config.Companies {
		company := &config.Companies[i]
		company.ID = strings.TrimSpace(company.ID)
		company.Name = strings.TrimSpace(company.Name)
		company.Status = strings.TrimSpace(company.Status)
		if company.Status == "" {
			company.Status = "active"
		}
		if company.ID == "" || company.Name == "" {
			return common.NewMessage("Company ID and name are required")
		}
		if utf8.RuneCountInString(company.ID) > 128 || utf8.RuneCountInString(company.Name) > 128 {
			return common.NewMessage("Company ID and name must not exceed 128 characters")
		}
		if company.Status != "active" && company.Status != "disabled" {
			return common.NewMessage("Company status must be active or disabled")
		}
		if math.IsNaN(company.SLO) || math.IsInf(company.SLO, 0) || company.SLO < 0 || company.SLO > 100 {
			return common.NewMessage("Company SLO must be between 0 and 100")
		}
		if _, exists := companyIDs[company.ID]; exists {
			return common.NewMessage("Duplicate company ID: {{id}}", map[string]any{"id": company.ID})
		}
		companyIDs[company.ID] = struct{}{}
		if len(company.Groups) > 100 {
			return common.NewMessage("Company {{company}} supports at most 100 groups", map[string]any{"company": company.ID})
		}
		if company.Groups == nil {
			company.Groups = []PerformanceModelGroup{}
		}
		for j := range company.Groups {
			group := &company.Groups[j]
			group.ID = strings.TrimSpace(group.ID)
			group.Name = strings.TrimSpace(group.Name)
			if group.ID == "" || group.Name == "" {
				return common.NewMessage("Model group ID and name are required")
			}
			if utf8.RuneCountInString(group.ID) > 128 || utf8.RuneCountInString(group.Name) > 128 {
				return common.NewMessage("Model group ID and name must not exceed 128 characters")
			}
			if _, exists := groupIDs[group.ID]; exists {
				return common.NewMessage("Duplicate model group ID: {{id}}", map[string]any{"id": group.ID})
			}
			groupIDs[group.ID] = struct{}{}
			if math.IsNaN(group.SLO) || math.IsInf(group.SLO, 0) || group.SLO < 0 || group.SLO > 100 {
				return common.NewMessage("Model group {{group}} SLO must be between 0 and 100", map[string]any{"group": group.ID})
			}
			if group.P95LatencyMs < 0 || group.P95LatencyMs > 24*60*60*1000 {
				return common.NewMessage("Model group {{group}} has an invalid P95 latency threshold", map[string]any{"group": group.ID})
			}
			if group.MinSamples < 0 || group.MinSamples > 1_000_000_000 {
				return common.NewMessage("Model group {{group}} has an invalid minimum sample count", map[string]any{"group": group.ID})
			}
			group.Models = normalizeModelNames(group.Models)
			for _, modelName := range group.Models {
				if utf8.RuneCountInString(modelName) > 128 {
					return common.NewMessage("Model group {{group}} contains a model name longer than 128 characters", map[string]any{"group": group.ID})
				}
				if owner, exists := modelOwners[modelName]; exists {
					return common.NewMessage("Model {{model}} is assigned to both {{first}} and {{second}}", map[string]any{"model": modelName, "first": owner, "second": group.ID})
				}
				modelOwners[modelName] = group.ID
			}
		}
	}
	return nil
}

func normalizeModelNames(names []string) []string {
	result := make([]string, 0, len(names))
	for _, name := range names {
		name = strings.TrimSpace(name)
		if name != "" && !slices.Contains(result, name) {
			result = append(result, name)
		}
	}
	slices.Sort(result)
	return result
}
