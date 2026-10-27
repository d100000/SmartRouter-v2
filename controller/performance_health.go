package controller

import (
	"context"
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
	"github.com/QuantumNous/new-api/setting/perf_metrics_setting"

	"github.com/gin-gonic/gin"
)

const performanceHealthDefinitionVersion = "request-v1"

type performanceHealthModel struct {
	ModelName              string                         `json:"model_name"`
	CompanyID              string                         `json:"company_id"`
	CompanyName            string                         `json:"company_name"`
	GroupID                string                         `json:"group_id,omitempty"`
	GroupName              string                         `json:"group_name,omitempty"`
	RequestCount           int64                          `json:"request_count"`
	SuccessRate            *float64                       `json:"success_rate"`
	AvgLatencyMs           *int64                         `json:"avg_latency_ms"`
	AvgTps                 *float64                       `json:"avg_tps"`
	OutputMetricsAvailable bool                           `json:"output_metrics_available"`
	EffectiveSLO           *float64                       `json:"effective_slo"`
	MinSamples             int64                          `json:"min_samples"`
	MonitoringEnabled      bool                           `json:"monitoring_enabled"`
	Configured             bool                           `json:"configured"`
	RecentSuccessSeries    []perfmetrics.SuccessRatePoint `json:"recent_success_series,omitempty"`
}

type performanceHealthCompany struct {
	ID               string   `json:"id"`
	Name             string   `json:"name"`
	Status           string   `json:"status"`
	RequestCount     int64    `json:"request_count"`
	SuccessRate      *float64 `json:"success_rate"`
	AvgLatencyMs     *int64   `json:"avg_latency_ms"`
	ModelCount       int      `json:"model_count"`
	TerminalCoverage float64  `json:"terminal_coverage"`
}

type performanceHealthSummary struct {
	MetricCapabilities        map[string]bool                           `json:"metric_capabilities"`
	Summary                   *perfmetrics.Summary                      `json:"summary"`
	WindowStart               int64                                     `json:"window_start"`
	WindowEnd                 int64                                     `json:"window_end"`
	Models                    []performanceHealthModel                  `json:"models"`
	Companies                 []performanceHealthCompany                `json:"companies"`
	RecentSuccessSeries       map[string][]perfmetrics.SuccessRatePoint `json:"recent_success_series,omitempty"`
	TerminalCoverage          float64                                   `json:"terminal_coverage"`
	TerminalCoverageAvailable bool                                      `json:"terminal_coverage_available"`
	DataFreshness             int64                                     `json:"data_freshness"`
	DefinitionVersion         string                                    `json:"definition_version"`
	ConfigVersion             int64                                     `json:"config_version"`
	GeneratedAt               int64                                     `json:"generated_at"`
	WindowAlignment           string                                    `json:"window_alignment"`
	BucketSeconds             int64                                     `json:"bucket_seconds"`
	CurrentBucketPartial      bool                                      `json:"current_bucket_partial"`
	LastSampleAt              *int64                                    `json:"last_sample_at"`
	PersistedThrough          *int64                                    `json:"persisted_through"`
	CollectorHeartbeatAt      *int64                                    `json:"collector_heartbeat_at"`
	Collection                perfmetrics.CollectionHealth              `json:"collection"`
}

type performanceHealthOwnership struct {
	CompanyID, CompanyName, GroupID, GroupName string
	EffectiveSLO                               *float64
	MinSamples                                 int64
	MonitoringEnabled                          bool
}

type performanceHealthCacheRow struct {
	ChannelID           int     `json:"channel_id"`
	ChannelName         string  `json:"channel_name"`
	ModelName           string  `json:"model_name"`
	RequestCount        int64   `json:"request_count"`
	TokenUsed           int64   `json:"token_used"`
	CacheReadTokens     int64   `json:"cache_read_tokens"`
	CacheCreationTokens int64   `json:"cache_creation_tokens"`
	CacheReadRatio      float64 `json:"cache_read_ratio"`
	CacheWriteRatio     float64 `json:"cache_write_ratio"`
}

func GetPerformanceHealthCacheRatios(c *gin.Context) {
	hours, ok := parsePerformanceHealthHours(c.Query("hours"))
	if !ok {
		common.ApiErrorStatus(c, http.StatusBadRequest, errors.New("hours must be between 1 and 720"))
		return
	}
	endTs := time.Now().Unix()
	startTs := endTs - int64(hours)*3600
	summaries, err := model.GetCacheRatioSummary(startTs, endTs)
	if err != nil {
		performanceHealthError(c, err)
		return
	}
	channelIDs := make([]int, 0, len(summaries))
	seenChannels := make(map[int]struct{}, len(summaries))
	for _, summary := range summaries {
		if summary.ChannelID <= 0 {
			continue
		}
		if _, seen := seenChannels[summary.ChannelID]; seen {
			continue
		}
		seenChannels[summary.ChannelID] = struct{}{}
		channelIDs = append(channelIDs, summary.ChannelID)
	}
	channelNames, err := model.GetChannelNamesByIds(channelIDs)
	if err != nil {
		performanceHealthError(c, err)
		return
	}
	rows := make([]performanceHealthCacheRow, 0, len(summaries))
	for _, summary := range summaries {
		// TokenUsed already includes cache-read tokens for the normal OpenAI-like
		// usage semantics recorded in quota_data. Adding CacheReadTokens again
		// would double-count reads and understate the cache share.
		readDenominator := summary.TokenUsed
		writeDenominator := summary.TokenUsed + summary.CacheCreationTokens
		channelName := channelNames[summary.ChannelID]
		if channelName == "" {
			if summary.ChannelID > 0 {
				channelName = fmt.Sprintf("Channel #%d", summary.ChannelID)
			} else {
				channelName = "Unknown channel"
			}
		}
		row := performanceHealthCacheRow{
			ChannelID:           summary.ChannelID,
			ChannelName:         channelName,
			ModelName:           summary.ModelName,
			RequestCount:        summary.RequestCount,
			TokenUsed:           summary.TokenUsed,
			CacheReadTokens:     summary.CacheReadTokens,
			CacheCreationTokens: summary.CacheCreationTokens,
		}
		if readDenominator > 0 {
			row.CacheReadRatio = float64(summary.CacheReadTokens) / float64(readDenominator) * 100
		}
		if writeDenominator > 0 {
			row.CacheWriteRatio = float64(summary.CacheCreationTokens) / float64(writeDenominator) * 100
		}
		rows = append(rows, row)
	}
	common.ApiSuccess(c, gin.H{
		"available":          len(rows) > 0,
		"source":             "quota_data",
		"definition_version": "cache-token-share-v1",
		"window_start":       startTs,
		"window_end":         endTs,
		"rows":               rows,
	})
}

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
	result, err := queryPerformanceHealthSummary(hours, config, c.Query("company_id"), c.Query("group_id"), c.Query("model"), c.Query("include_presets") == "true")
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
		result, err := perfmetrics.Query(perfmetrics.QueryParams{Model: modelName, Hours: hours, AllowedGroups: nil})
		if err != nil {
			performanceHealthError(c, err)
			return
		}
		collection, err := perfmetrics.CollectionStatus(c.Request.Context(), result.WindowStart)
		if err != nil {
			common.SysError(common.LogText("failed to read performance metric collection health: %s", err.Error()))
			performanceHealthError(c, common.NewMessage("Unable to load collection health"))
			return
		}
		common.ApiSuccess(c, gin.H{"scope": "model", "model": modelName, "series": result.Series,
			"window_start": result.WindowStart, "window_end": result.WindowEnd, "definition_version": performanceHealthDefinitionVersion,
			"generated_at": time.Now().Unix(), "bucket_seconds": perf_metrics_setting.GetBucketSeconds(), "window_alignment": "hour", "current_bucket_partial": true,
			"last_sample_at": collection.LastSampleAt, "persisted_through": nil, "collector_heartbeat_at": collection.CollectorHeartbeatAt, "collection": collection})
		return
	}
	result, err := queryPerformanceHealthSummary(hours, config, c.Query("company_id"), c.Query("group_id"), "", false)
	if err != nil {
		performanceHealthError(c, err)
		return
	}
	common.ApiSuccess(c, gin.H{"scope": "site", "series": result.RecentSuccessSeries, "window_start": result.WindowStart, "window_end": result.WindowEnd, "definition_version": performanceHealthDefinitionVersion, "data_freshness": result.DataFreshness, "generated_at": result.GeneratedAt, "bucket_seconds": result.BucketSeconds, "window_alignment": result.WindowAlignment, "current_bucket_partial": true, "last_sample_at": result.LastSampleAt, "persisted_through": nil, "collector_heartbeat_at": result.CollectorHeartbeatAt, "collection": result.Collection})
}

func GetPerformanceHealthCompanies(c *gin.Context) {
	config, err := model.GetPerformanceHealthConfig()
	if err != nil {
		performanceHealthError(c, err)
		return
	}
	common.ApiSuccess(c, gin.H{
		"companies": config.Companies, "version": config.Version, "config_version": config.Version,
		"updated_at": config.UpdatedAt, "preset_version": config.PresetVersion,
		"excluded_models": config.ExcludedModels, "excluded_companies": config.ExcludedCompanies, "excluded_groups": config.ExcludedGroups,
	})
}

func PutPerformanceHealthConfig(c *gin.Context) {
	var request struct {
		model.PerformanceHealthConfig
		ExpectedConfigVersion *int64 `json:"expected_config_version"`
	}
	if err := common.DecodeJson(c.Request.Body, &request); err != nil {
		performanceHealthError(c, err)
		return
	}
	if request.ExpectedConfigVersion == nil || *request.ExpectedConfigVersion < 1 {
		performanceHealthError(c, common.NewMessage("Reload the configuration before saving changes"))
		return
	}
	request.Version = *request.ExpectedConfigVersion
	saved, err := model.SavePerformanceHealthConfig(request.PerformanceHealthConfig)
	if err != nil {
		performanceHealthError(c, err)
		return
	}
	recordManageAudit(c, "performance_health.config.update", map[string]any{"config_version": saved.Version, "company_count": len(saved.Companies)})
	common.ApiSuccess(c, saved)
}

func GetPerformanceHealthPresets(c *gin.Context) {
	common.ApiSuccess(c, model.GetPerformanceHealthPresets())
}

func PreviewPerformanceHealthPresets(c *gin.Context) {
	preview, err := model.PreviewPerformanceHealthPresets()
	if err != nil {
		performanceHealthError(c, err)
		return
	}
	common.ApiSuccess(c, preview)
}

func ApplyPerformanceHealthPresets(c *gin.Context) {
	var request struct {
		ExpectedConfigVersion int64  `json:"expected_config_version"`
		PreviewToken          string `json:"preview_token"`
	}
	if err := common.DecodeJson(c.Request.Body, &request); err != nil {
		performanceHealthError(c, err)
		return
	}
	if request.ExpectedConfigVersion < 1 || request.PreviewToken == "" {
		performanceHealthError(c, common.NewMessage("Preview preset additions before applying them"))
		return
	}
	saved, err := model.ApplyPerformanceHealthPresets(request.ExpectedConfigVersion, request.PreviewToken)
	if err != nil {
		performanceHealthError(c, err)
		return
	}
	recordManageAudit(c, "performance_health.presets.apply", map[string]any{"config_version": saved.Version, "preset_version": saved.PresetVersion})
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

func queryPerformanceHealthSummary(hours int, config model.PerformanceHealthConfig, companyID, groupID, modelFilter string, includePresets bool) (*performanceHealthSummary, error) {
	result, err := perfmetrics.QuerySummaryAll(hours, nil)
	if err != nil {
		return nil, err
	}
	configured, err := model.GetConfiguredModelChannels()
	if err != nil {
		return nil, err
	}
	ownership := buildPerformanceHealthOwnership(config)
	byName := make(map[string]performanceHealthModel, len(result.Models)+len(configured))
	for _, item := range result.Models {
		var outputTps *float64
		if item.OutputMetricsAvailable {
			outputTps = &item.AvgTps
		}
		byName[item.ModelName] = performanceHealthModel{
			ModelName: item.ModelName, RequestCount: item.RequestCount, OutputMetricsAvailable: item.OutputMetricsAvailable,
			SuccessRate: &item.SuccessRate, AvgLatencyMs: &item.AvgLatencyMs, AvgTps: outputTps,
			RecentSuccessSeries: item.RecentSuccessSeries,
		}
	}
	for name := range configured {
		if _, exists := byName[name]; !exists {
			byName[name] = performanceHealthModel{ModelName: name}
		}
	}
	if includePresets {
		for name := range ownership {
			if _, exists := byName[name]; !exists {
				byName[name] = performanceHealthModel{ModelName: name}
			}
		}
	}
	models := make([]performanceHealthModel, 0, len(byName))
	recentSeries := make(map[string][]perfmetrics.SuccessRatePoint, len(result.Models))
	for name, item := range byName {
		owner, assigned := ownership[name]
		if companyID != "" && owner.CompanyID != companyID {
			continue
		}
		if groupID != "" && owner.GroupID != groupID {
			continue
		}
		if modelFilter != "" && name != modelFilter {
			continue
		}
		item.CompanyID, item.CompanyName = owner.CompanyID, owner.CompanyName
		item.GroupID, item.GroupName = owner.GroupID, owner.GroupName
		item.EffectiveSLO, item.MinSamples = owner.EffectiveSLO, owner.MinSamples
		item.MonitoringEnabled = owner.MonitoringEnabled
		if !assigned {
			item.MinSamples = 100
			item.MonitoringEnabled = true
		}
		_, item.Configured = configured[name]
		models = append(models, item)
		recentSeries[name] = item.RecentSuccessSeries
	}
	slices.SortFunc(models, func(a, b performanceHealthModel) int {
		if a.RequestCount > b.RequestCount {
			return -1
		}
		if a.RequestCount < b.RequestCount {
			return 1
		}
		return strings.Compare(a.ModelName, b.ModelName)
	})
	statusCtx, cancelStatus := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelStatus()
	collection, err := perfmetrics.CollectionStatus(statusCtx, result.WindowStart)
	if err != nil {
		common.SysError(common.LogText("failed to read performance metric collection health: %s", err.Error()))
		return nil, common.NewMessage("Unable to load collection health")
	}
	now := time.Now().Unix()
	return &performanceHealthSummary{
		Summary: result.Summary, WindowStart: result.WindowStart, WindowEnd: result.WindowEnd,
		Models: models, Companies: aggregatePerformanceHealthCompanies(config, models), RecentSuccessSeries: recentSeries,
		TerminalCoverageAvailable: false, DataFreshness: now, GeneratedAt: now,
		WindowAlignment: "hour", BucketSeconds: 3600, CurrentBucketPartial: true,
		LastSampleAt: collection.LastSampleAt, PersistedThrough: nil,
		CollectorHeartbeatAt: collection.CollectorHeartbeatAt, Collection: collection,
		MetricCapabilities: map[string]bool{"durable_terminal_events": true, "task_terminal_recovery": true, "output_tps": true, "attempts": false, "retry_recovery": false, "latency_percentiles": false, "terminal_coverage": false},
		DefinitionVersion:  performanceHealthDefinitionVersion, ConfigVersion: config.Version,
	}, nil
}

func buildPerformanceHealthOwnership(config model.PerformanceHealthConfig) map[string]performanceHealthOwnership {
	result := make(map[string]performanceHealthOwnership)
	for _, company := range config.Companies {
		for _, group := range company.Groups {
			enabled := company.Status == "active" && group.Enabled
			var effectiveSLO *float64
			if enabled {
				groupConfirmed := group.SLOConfirmed == nil && group.SLO > 0 || group.SLOConfirmed != nil && *group.SLOConfirmed
				companyConfirmed := company.SLOConfirmed == nil && company.SLO > 0 || company.SLOConfirmed != nil && *company.SLOConfirmed
				if groupConfirmed {
					effectiveSLO = &group.SLO
				} else if companyConfirmed {
					effectiveSLO = &company.SLO
				}
			}
			for _, modelName := range group.Models {
				result[modelName] = performanceHealthOwnership{
					CompanyID: company.ID, CompanyName: company.Name, GroupID: group.ID, GroupName: group.Name,
					EffectiveSLO: effectiveSLO, MinSamples: group.MinSamples, MonitoringEnabled: enabled,
				}
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
		entry.models++
		if item.RequestCount <= 0 || item.SuccessRate == nil || item.AvgLatencyMs == nil {
			continue
		}
		entry.requests += item.RequestCount
		entry.success += *item.SuccessRate * float64(item.RequestCount)
		entry.latency += float64(*item.AvgLatencyMs) * float64(item.RequestCount)
	}
	result := make([]performanceHealthCompany, 0, len(acc))
	for _, entry := range acc {
		row := performanceHealthCompany{
			ID: entry.company.ID, Name: entry.company.Name, Status: entry.company.Status,
			RequestCount: entry.requests, ModelCount: entry.models,
		}
		if entry.requests > 0 {
			rate := entry.success / float64(entry.requests)
			latency := int64(entry.latency / float64(entry.requests))
			row.SuccessRate, row.AvgLatencyMs = &rate, &latency
		}
		result = append(result, row)
	}
	slices.SortFunc(result, func(a, b performanceHealthCompany) int {
		if a.RequestCount > b.RequestCount {
			return -1
		}
		if a.RequestCount < b.RequestCount {
			return 1
		}
		return strings.Compare(a.Name, b.Name)
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
	if errors.Is(err, model.ErrPerformanceHealthConflict) {
		common.ApiErrorStatus(c, http.StatusConflict, common.NewMessage("Configuration changed. Reload before saving again."))
		return
	}
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
	config, index, err := findPerformanceHealthCompany(companyID)
	if err != nil {
		performanceHealthError(c, err)
		return
	}
	patch := config.Companies[index]
	if err := common.DecodeJson(c.Request.Body, &patch); err != nil {
		performanceHealthError(c, err)
		return
	}
	patch.ID = companyID
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
	config, companyIndex, groupIndex, err := findPerformanceHealthGroup(groupID)
	if err != nil {
		performanceHealthError(c, err)
		return
	}
	patch := config.Companies[companyIndex].Groups[groupIndex]
	if err := common.DecodeJson(c.Request.Body, &patch); err != nil {
		performanceHealthError(c, err)
		return
	}
	patch.ID = groupID
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
