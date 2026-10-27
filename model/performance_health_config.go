package model

import (
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/QuantumNous/new-api/common"
)

// PerformanceHealthConfigOption stores the dashboard's administrative
// company/model ownership map in the existing options table.
const PerformanceHealthConfigOption = "PerformanceHealthConfig"

type PerformanceHealthConfig struct {
	Version   int64                `json:"version"`
	UpdatedAt int64                `json:"updated_at"`
	Companies []PerformanceCompany `json:"companies"`
}

type PerformanceCompany struct {
	ID     string                  `json:"id"`
	Name   string                  `json:"name"`
	Status string                  `json:"status"`
	SLO    float64                 `json:"slo"`
	Owner  string                  `json:"owner,omitempty"`
	Note   string                  `json:"note,omitempty"`
	Groups []PerformanceModelGroup `json:"groups"`
}

type PerformanceModelGroup struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Models       []string `json:"models"`
	Enabled      bool     `json:"enabled"`
	SLO          float64  `json:"slo"`
	P95LatencyMs int64    `json:"p95_latency_ms"`
	MinSamples   int64    `json:"min_samples"`
}

var performanceHealthConfigMu sync.Mutex

func GetPerformanceHealthConfig() (PerformanceHealthConfig, error) {
	common.OptionMapRWMutex.RLock()
	raw := common.OptionMap[PerformanceHealthConfigOption]
	common.OptionMapRWMutex.RUnlock()
	if strings.TrimSpace(raw) == "" {
		return PerformanceHealthConfig{Version: 1, Companies: []PerformanceCompany{}}, nil
	}
	var config PerformanceHealthConfig
	if err := common.UnmarshalJsonStr(raw, &config); err != nil {
		return PerformanceHealthConfig{}, fmt.Errorf("decode performance health config: %w", err)
	}
	if err := validatePerformanceHealthConfig(config); err != nil {
		return PerformanceHealthConfig{}, err
	}
	if config.Version == 0 {
		config.Version = 1
	}
	return config, nil
}

func SavePerformanceHealthConfig(config PerformanceHealthConfig) (PerformanceHealthConfig, error) {
	if err := validatePerformanceHealthConfig(config); err != nil {
		return PerformanceHealthConfig{}, err
	}
	config.Version++
	if config.Version <= 0 {
		config.Version = 1
	}
	config.UpdatedAt = time.Now().Unix()
	if config.Companies == nil {
		config.Companies = []PerformanceCompany{}
	}
	raw, err := common.Marshal(config)
	if err != nil {
		return PerformanceHealthConfig{}, err
	}
	performanceHealthConfigMu.Lock()
	defer performanceHealthConfigMu.Unlock()
	if err := UpdateOption(PerformanceHealthConfigOption, string(raw)); err != nil {
		return PerformanceHealthConfig{}, err
	}
	return config, nil
}

func validatePerformanceHealthConfig(config PerformanceHealthConfig) error {
	if len(config.Companies) > 100 {
		return errors.New("performance health supports at most 100 companies")
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
			return errors.New("company id and name are required")
		}
		if utf8.RuneCountInString(company.ID) > 128 || utf8.RuneCountInString(company.Name) > 128 {
			return errors.New("company id and name must not exceed 128 characters")
		}
		if company.Status != "active" && company.Status != "disabled" {
			return errors.New("company status must be active or disabled")
		}
		if company.SLO < 0 || company.SLO > 100 {
			return errors.New("company SLO must be between 0 and 100")
		}
		if _, exists := companyIDs[company.ID]; exists {
			return fmt.Errorf("duplicate company id: %s", company.ID)
		}
		companyIDs[company.ID] = struct{}{}
		if len(company.Groups) > 100 {
			return fmt.Errorf("company %s supports at most 100 groups", company.ID)
		}
		for j := range company.Groups {
			group := &company.Groups[j]
			group.ID = strings.TrimSpace(group.ID)
			group.Name = strings.TrimSpace(group.Name)
			if group.ID == "" || group.Name == "" {
				return errors.New("model group id and name are required")
			}
			if utf8.RuneCountInString(group.ID) > 128 || utf8.RuneCountInString(group.Name) > 128 {
				return errors.New("model group id and name must not exceed 128 characters")
			}
			if _, exists := groupIDs[group.ID]; exists {
				return fmt.Errorf("duplicate model group id: %s", group.ID)
			}
			groupIDs[group.ID] = struct{}{}
			if group.SLO < 0 || group.SLO > 100 {
				return fmt.Errorf("model group %s SLO must be between 0 and 100", group.ID)
			}
			if group.P95LatencyMs < 0 || group.P95LatencyMs > 24*60*60*1000 {
				return fmt.Errorf("model group %s has invalid P95 latency threshold", group.ID)
			}
			if group.MinSamples < 0 || group.MinSamples > 1_000_000_000 {
				return fmt.Errorf("model group %s has invalid minimum sample count", group.ID)
			}
			group.Models = normalizeModelNames(group.Models)
			for _, modelName := range group.Models {
				if owner, exists := modelOwners[modelName]; exists {
					return fmt.Errorf("model %s is assigned to both %s and %s", modelName, owner, group.ID)
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
