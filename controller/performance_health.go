package controller

import (
	"errors"
	"fmt"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	perfmetrics "github.com/QuantumNous/new-api/pkg/perf_metrics"
	"github.com/QuantumNous/new-api/setting/ratio_setting"

	"github.com/gin-gonic/gin"
	"github.com/samber/lo"
)

const performanceHealthDefinitionVersion = "request-v1"

type performanceHealthModel struct {
	ModelName           string                         `json:"model_name"`
	CompanyID           string                         `json:"company_id"`
	CompanyName         string                         `json:"company_name"`
	GroupID             string                         `json:"group_id,omitempty"`
	GroupName           string                         `json:"group_name,omitempty"`
	RequestCount        int64                          `json:"request_count"`
	SuccessRate         float64                        `json:"success_rate"`
	AvgLatencyMs        int64                          `json:"avg_latency_ms"`
	AvgTps              float64                        `json:"avg_tps"`
	RecentSuccessSeries []perfmetrics.SuccessRatePoint `json:"recent_success_series,omitempty"`
}

type performanceHealthCompany struct {
	ID               string  `json:"id"`
	Name             string  `json:"name"`
	Status           string  `json:"status"`
	RequestCount     int64   `json:"request_count"`
	SuccessRate      float64 `json:"success_rate"`
	AvgLatencyMs     int64   `json:"avg_latency_ms"`
	ModelCount       int     `json:"model_count"`
	TerminalCoverage float64 `json:"terminal_coverage"`
}

type performanceHealthSummary struct {
	Summary             *perfmetrics.Summary                      `json:"summary"`
	WindowStart         int64                                     `json:"window_start"`
	WindowEnd           int64                                     `json:"window_end"`
	Models              []performanceHealthModel                  `json:"models"`
	Companies           []performanceHealthCompany                `json:"companies"`
	RecentSuccessSeries map[string][]perfmetrics.SuccessRatePoint `json:"recent_success_series,omitempty"`
	TerminalCoverage    float64                                   `json:"terminal_coverage"`
	DataFreshness       int64                                     `json:"data_freshness"`
	DefinitionVersion   string                                    `json:"definition_version"`
	ConfigVersion       int64                                     `json:"config_version"`
}

type performanceHealthOwnership struct{ CompanyID, CompanyName, GroupID, GroupName string }

func GetPerformanceHealthSummary(c *gin.Context) {
	hours, ok := parsePerformanceHealthHours(c.Query("hours"))
	if !ok {
		common.ApiErrorStatus(c, http.StatusBadRequest, errors.New("hours must be between 1 and 720"))
		return
	}
	config, err := model.GetPerformanceHealthConfig()
	if err != nil {
		performanceHealthError(c, err)
		return
	}
	result, err := queryPerformanceHealthSummary(hours, config, c.Query("company_id"), c.Query("group_id"), c.Query("model"))
	if err != nil {
		performanceHealthError(c, err)
		return
	}
	common.ApiSuccess(c, result)
}

func GetPerformanceHealthTimeseries(c *gin.Context) {
	hours, ok := parsePerformanceHealthHours(c.Query("hours"))
	if !ok {
		common.ApiErrorStatus(c, http.StatusBadRequest, errors.New("hours must be between 1 and 720"))
		return
	}
	config, err := model.GetPerformanceHealthConfig()
	if err != nil {
		performanceHealthError(c, err)
		return
	}
	modelName := strings.TrimSpace(c.Query("model"))
	if modelName != "" {
		result, err := perfmetrics.Query(perfmetrics.QueryParams{Model: modelName, Hours: hours, AllowedGroups: append(lo.Keys(ratio_setting.GetGroupRatioCopy()), "auto")})
		if err != nil {
			performanceHealthError(c, err)
			return
		}
		common.ApiSuccess(c, gin.H{"scope": "model", "model": modelName, "series": result.Series, "window_start": result.WindowStart, "window_end": result.WindowEnd, "definition_version": performanceHealthDefinitionVersion})
		return
	}
	result, err := queryPerformanceHealthSummary(hours, config, c.Query("company_id"), c.Query("group_id"), "")
	if err != nil {
		performanceHealthError(c, err)
		return
	}
	common.ApiSuccess(c, gin.H{"scope": "site", "series": result.RecentSuccessSeries, "window_start": result.WindowStart, "window_end": result.WindowEnd, "definition_version": performanceHealthDefinitionVersion, "data_freshness": result.DataFreshness})
}

func GetPerformanceHealthCompanies(c *gin.Context) {
	config, err := model.GetPerformanceHealthConfig()
	if err != nil {
		performanceHealthError(c, err)
		return
	}
	common.ApiSuccess(c, gin.H{"companies": config.Companies, "config_version": config.Version})
}

func PutPerformanceHealthConfig(c *gin.Context) {
	var config model.PerformanceHealthConfig
	if err := common.DecodeJson(c.Request.Body, &config); err != nil {
		performanceHealthError(c, err)
		return
	}
	saved, err := model.SavePerformanceHealthConfig(config)
	if err != nil {
		performanceHealthError(c, err)
		return
	}
	recordManageAudit(c, "performance_health.config.update", map[string]any{"config_version": saved.Version, "company_count": len(saved.Companies)})
	common.ApiSuccess(c, saved)
}

func GetPerformanceHealthGroups(c *gin.Context) {
	config, index, err := findPerformanceHealthCompany(strings.TrimSpace(c.Param("company_id")))
	if err != nil {
		performanceHealthError(c, err)
		return
	}
	common.ApiSuccess(c, gin.H{"company_id": config.Companies[index].ID, "groups": config.Companies[index].Groups, "config_version": config.Version})
}

func queryPerformanceHealthSummary(hours int, config model.PerformanceHealthConfig, companyID, groupID, modelFilter string) (*performanceHealthSummary, error) {
	allowedGroups := append(lo.Keys(ratio_setting.GetGroupRatioCopy()), "auto")
	first, err := perfmetrics.QuerySummaryAll(hours, allowedGroups)
	if err != nil {
		return nil, err
	}
	modelNames := make([]string, 0, len(first.Models))
	for _, item := range first.Models {
		modelNames = append(modelNames, item.ModelName)
	}
	slices.Sort(modelNames)
	if len(modelNames) > perfmetrics.MaxRecentModels {
		modelNames = modelNames[:perfmetrics.MaxRecentModels]
	}
	result := first
	if len(modelNames) > 0 {
		result, err = perfmetrics.QuerySummaryAll(hours, allowedGroups, modelNames...)
		if err != nil {
			return nil, err
		}
	}
	ownership := buildPerformanceHealthOwnership(config)
	models := make([]performanceHealthModel, 0, len(result.Models))
	for _, item := range result.Models {
		owner := ownership[item.ModelName]
		if companyID != "" && owner.CompanyID != companyID {
			continue
		}
		if groupID != "" && owner.GroupID != groupID {
			continue
		}
		if modelFilter != "" && item.ModelName != modelFilter {
			continue
		}
		models = append(models, performanceHealthModel{ModelName: item.ModelName, CompanyID: owner.CompanyID, CompanyName: owner.CompanyName, GroupID: owner.GroupID, GroupName: owner.GroupName, RequestCount: item.RequestCount, SuccessRate: item.SuccessRate, AvgLatencyMs: item.AvgLatencyMs, AvgTps: item.AvgTps, RecentSuccessSeries: item.RecentSuccessSeries})
	}
	return &performanceHealthSummary{Summary: result.Summary, WindowStart: result.WindowStart, WindowEnd: result.WindowEnd, Models: models, Companies: aggregatePerformanceHealthCompanies(config, models), RecentSuccessSeries: result.RecentSuccessSeries, TerminalCoverage: 100, DataFreshness: time.Now().Unix(), DefinitionVersion: performanceHealthDefinitionVersion, ConfigVersion: config.Version}, nil
}

func buildPerformanceHealthOwnership(config model.PerformanceHealthConfig) map[string]performanceHealthOwnership {
	result := make(map[string]performanceHealthOwnership)
	for _, company := range config.Companies {
		for _, group := range company.Groups {
			for _, modelName := range group.Models {
				result[modelName] = performanceHealthOwnership{company.ID, company.Name, group.ID, group.Name}
			}
		}
	}
	return result
}

func aggregatePerformanceHealthCompanies(config model.PerformanceHealthConfig, models []performanceHealthModel) []performanceHealthCompany {
	type accumulator struct {
		company          model.PerformanceCompany
		requests         int64
		success, latency float64
		models           int
	}
	acc := make(map[string]*accumulator)
	for _, company := range config.Companies {
		acc[company.ID] = &accumulator{company: company}
	}
	for _, item := range models {
		id := item.CompanyID
		if id == "" {
			id = "unassigned"
		}
		entry := acc[id]
		if entry == nil {
			entry = &accumulator{company: model.PerformanceCompany{ID: id, Name: "Unassigned", Status: "active"}}
			acc[id] = entry
		}
		entry.requests += item.RequestCount
		entry.success += item.SuccessRate * float64(item.RequestCount)
		entry.latency += float64(item.AvgLatencyMs * item.RequestCount)
		entry.models++
	}
	result := make([]performanceHealthCompany, 0, len(acc))
	for _, entry := range acc {
		if entry.requests == 0 && entry.company.ID != "unassigned" {
			continue
		}
		rate, latency := 0.0, int64(0)
		if entry.requests > 0 {
			rate = entry.success / float64(entry.requests)
			latency = int64(entry.latency / float64(entry.requests))
		}
		result = append(result, performanceHealthCompany{ID: entry.company.ID, Name: entry.company.Name, Status: entry.company.Status, RequestCount: entry.requests, SuccessRate: rate, AvgLatencyMs: latency, ModelCount: entry.models, TerminalCoverage: 100})
	}
	slices.SortFunc(result, func(a, b performanceHealthCompany) int {
		if a.RequestCount == b.RequestCount {
			return strings.Compare(a.Name, b.Name)
		}
		if a.RequestCount > b.RequestCount {
			return -1
		}
		return 1
	})
	return result
}

func findPerformanceHealthCompany(companyID string) (model.PerformanceHealthConfig, int, error) {
	config, err := model.GetPerformanceHealthConfig()
	if err != nil {
		return model.PerformanceHealthConfig{}, -1, err
	}
	for index, company := range config.Companies {
		if company.ID == companyID {
			return config, index, nil
		}
	}
	return model.PerformanceHealthConfig{}, -1, fmt.Errorf("company %s not found", companyID)
}

func parsePerformanceHealthHours(raw string) (int, bool) {
	if raw == "" {
		return 24, true
	}
	hours, err := strconv.Atoi(raw)
	return hours, err == nil && hours >= 1 && hours <= 24*30
}
func performanceHealthError(c *gin.Context, err error) {
	common.ApiErrorStatus(c, http.StatusBadRequest, err)
}

func CreatePerformanceHealthCompany(c *gin.Context) {
	var company model.PerformanceCompany
	if err := common.DecodeJson(c.Request.Body, &company); err != nil {
		performanceHealthError(c, err)
		return
	}
	if strings.TrimSpace(company.ID) == "" {
		company.ID = fmt.Sprintf("company-%d", time.Now().UnixNano())
	}
	if company.Status == "" {
		company.Status = "active"
	}
	config, err := model.GetPerformanceHealthConfig()
	if err != nil {
		performanceHealthError(c, err)
		return
	}
	for _, existing := range config.Companies {
		if existing.ID == company.ID {
			common.ApiErrorStatus(c, http.StatusConflict, fmt.Errorf("company %s already exists", company.ID))
			return
		}
	}
	config.Companies = append(config.Companies, company)
	saved, err := model.SavePerformanceHealthConfig(config)
	if err != nil {
		performanceHealthError(c, err)
		return
	}
	recordManageAudit(c, "performance_health.company.create", map[string]any{"company_id": company.ID})
	common.ApiSuccess(c, gin.H{"company": company, "config_version": saved.Version})
}

func UpdatePerformanceHealthCompany(c *gin.Context) {
	companyID := strings.TrimSpace(c.Param("company_id"))
	var patch model.PerformanceCompany
	if err := common.DecodeJson(c.Request.Body, &patch); err != nil {
		performanceHealthError(c, err)
		return
	}
	config, index, err := findPerformanceHealthCompany(companyID)
	if err != nil {
		performanceHealthError(c, err)
		return
	}
	patch.ID = companyID
	if patch.Name == "" {
		patch.Name = config.Companies[index].Name
	}
	if patch.Status == "" {
		patch.Status = config.Companies[index].Status
	}
	if patch.Groups == nil {
		patch.Groups = config.Companies[index].Groups
	}
	config.Companies[index] = patch
	saved, err := model.SavePerformanceHealthConfig(config)
	if err != nil {
		performanceHealthError(c, err)
		return
	}
	recordManageAudit(c, "performance_health.company.update", map[string]any{"company_id": companyID})
	common.ApiSuccess(c, gin.H{"company": patch, "config_version": saved.Version})
}

func DeletePerformanceHealthCompany(c *gin.Context) {
	companyID := strings.TrimSpace(c.Param("company_id"))
	config, index, err := findPerformanceHealthCompany(companyID)
	if err != nil {
		performanceHealthError(c, err)
		return
	}
	config.Companies = append(config.Companies[:index], config.Companies[index+1:]...)
	saved, err := model.SavePerformanceHealthConfig(config)
	if err != nil {
		performanceHealthError(c, err)
		return
	}
	recordManageAudit(c, "performance_health.company.delete", map[string]any{"company_id": companyID})
	common.ApiSuccess(c, gin.H{"config_version": saved.Version})
}

func CreatePerformanceHealthGroup(c *gin.Context) {
	companyID := strings.TrimSpace(c.Param("company_id"))
	var group model.PerformanceModelGroup
	if err := common.DecodeJson(c.Request.Body, &group); err != nil {
		performanceHealthError(c, err)
		return
	}
	if strings.TrimSpace(group.ID) == "" {
		group.ID = fmt.Sprintf("group-%d", time.Now().UnixNano())
	}
	config, index, err := findPerformanceHealthCompany(companyID)
	if err != nil {
		performanceHealthError(c, err)
		return
	}
	for _, company := range config.Companies {
		for _, existing := range company.Groups {
			if existing.ID == group.ID {
				common.ApiErrorStatus(c, http.StatusConflict, fmt.Errorf("model group %s already exists", group.ID))
				return
			}
		}
	}
	config.Companies[index].Groups = append(config.Companies[index].Groups, group)
	saved, err := model.SavePerformanceHealthConfig(config)
	if err != nil {
		performanceHealthError(c, err)
		return
	}
	recordManageAudit(c, "performance_health.group.create", map[string]any{"company_id": companyID, "group_id": group.ID})
	common.ApiSuccess(c, gin.H{"group": group, "config_version": saved.Version})
}

func UpdatePerformanceHealthGroup(c *gin.Context) {
	groupID := strings.TrimSpace(c.Param("group_id"))
	var patch model.PerformanceModelGroup
	if err := common.DecodeJson(c.Request.Body, &patch); err != nil {
		performanceHealthError(c, err)
		return
	}
	config, companyIndex, groupIndex, err := findPerformanceHealthGroup(groupID)
	if err != nil {
		performanceHealthError(c, err)
		return
	}
	patch.ID = groupID
	if patch.Name == "" {
		patch.Name = config.Companies[companyIndex].Groups[groupIndex].Name
	}
	if patch.Models == nil {
		patch.Models = config.Companies[companyIndex].Groups[groupIndex].Models
	}
	config.Companies[companyIndex].Groups[groupIndex] = patch
	saved, err := model.SavePerformanceHealthConfig(config)
	if err != nil {
		performanceHealthError(c, err)
		return
	}
	recordManageAudit(c, "performance_health.group.update", map[string]any{"group_id": groupID})
	common.ApiSuccess(c, gin.H{"group": patch, "config_version": saved.Version})
}

func DeletePerformanceHealthGroup(c *gin.Context) {
	groupID := strings.TrimSpace(c.Param("group_id"))
	config, companyIndex, groupIndex, err := findPerformanceHealthGroup(groupID)
	if err != nil {
		performanceHealthError(c, err)
		return
	}
	groups := config.Companies[companyIndex].Groups
	config.Companies[companyIndex].Groups = append(groups[:groupIndex], groups[groupIndex+1:]...)
	saved, err := model.SavePerformanceHealthConfig(config)
	if err != nil {
		performanceHealthError(c, err)
		return
	}
	recordManageAudit(c, "performance_health.group.delete", map[string]any{"group_id": groupID})
	common.ApiSuccess(c, gin.H{"config_version": saved.Version})
}

func findPerformanceHealthGroup(groupID string) (model.PerformanceHealthConfig, int, int, error) {
	config, err := model.GetPerformanceHealthConfig()
	if err != nil {
		return model.PerformanceHealthConfig{}, -1, -1, err
	}
	for companyIndex, company := range config.Companies {
		for groupIndex, group := range company.Groups {
			if group.ID == groupID {
				return config, companyIndex, groupIndex, nil
			}
		}
	}
	return model.PerformanceHealthConfig{}, -1, -1, fmt.Errorf("model group %s not found", groupID)
}
