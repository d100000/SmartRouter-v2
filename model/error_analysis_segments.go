package model

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"

	"github.com/tidwall/gjson"
	"gorm.io/gorm"
)

// A report is assembled from segments. Each whole local day read from an SQL
// log store is collected and aggregated once, under a name of its own, and
// every later report that covers the same day with the same filters reuses
// it as long as the day's log rows have not changed. A daily report over the
// last 7 days and the 7 before therefore reads one new day, not fourteen.
//
// Part of a day at either end of a custom range, an exact request lookup and
// every ClickHouse range are segments of their own that only this report uses.
//
// A request is merged within its segment. One whose log rows fall on both
// sides of midnight is counted once in each day.

// errorAnalysisScope separates the task that holds the lease from the name
// its collected artifacts are stored under.
type errorAnalysisScope struct {
	Task string
	ID   string
	// Whole marks a segment: one range without a comparison period. The
	// remaining fields carry the report's progress for the task's state.
	Whole         bool
	ScannedBefore int64
	TotalRows     int64
	TotalKnown    bool
}

// errorAnalysisSegmentVersion is part of a shared day's name. Raise it when
// classification, grouping or the stored shapes change, so days collected by
// older code are not reused.
const errorAnalysisSegmentVersion = 1

// errorAnalysisScopeRegistry lists every segment scope with the time a report
// last used it. Removal of unused scopes reads only this list.
const errorAnalysisScopeRegistry = "error-analysis-scopes"

type ErrorAnalysisSegment struct {
	Scope    string `json:"scope"`
	Start    int64  `json:"start"`
	End      int64  `json:"end"`
	Previous bool   `json:"previous,omitempty"`
	// Shared segments are whole days other reports can reuse; Reused ones
	// were already collected when this report was planned.
	Shared bool `json:"shared,omitempty"`
	Reused bool `json:"reused,omitempty"`
	// Rows is the number of log rows in the range when the report was
	// planned. It is unknown for ClickHouse.
	Rows int64 `json:"rows"`
}

type ErrorAnalysisPlan struct {
	Version               int                    `json:"version"`
	Filter                ErrorAnalysisFilter    `json:"filter"`
	PreviousFilter        ErrorAnalysisFilter    `json:"previous_filter"`
	Comparison            bool                   `json:"comparison"`
	Timezone              string                 `json:"timezone"`
	Segments              []ErrorAnalysisSegment `json:"segments"`
	TotalRows             int64                  `json:"total_rows"`
	TotalKnown            bool                   `json:"total_known"`
	LogRecordingEnabled   bool                   `json:"log_recording_enabled"`
	ErrorRecordingEnabled bool                   `json:"error_recording_enabled"`
	Merged                bool                   `json:"merged"`
}

// errorAnalysisSourceFingerprint identifies the log database. It matches
// InitLogDB/chooseDB's configuration precedence. Only the digest is persisted:
// DSNs can contain credentials and must never enter task state, error
// messages or logs.
func errorAnalysisSourceFingerprint() string {
	source := os.Getenv("LOG_SQL_DSN")
	if source == "" {
		source = os.Getenv("SQL_DSN")
	}
	if source == "" || strings.HasPrefix(source, "local") {
		source = common.SQLitePath
	}
	return fmt.Sprintf("%x", sha256.Sum256([]byte(string(common.LogDatabaseType())+"\x00"+source)))
}

// errorAnalysisTaskTimezone is the timezone of the configuration the task was
// started with, UTC when it has none.
func errorAnalysisTaskTimezone(taskID string) (string, error) {
	configs, err := ListErrorAnalysisArtifacts(taskID, "config", 0, 1)
	if err != nil {
		return "", err
	}
	if len(configs) > 0 {
		if timezone := gjson.Get(string(configs[0].Data), "timezone").String(); timezone != "" {
			if _, err := time.LoadLocation(timezone); err != nil {
				return "", errors.New("analysis_invalid_timezone")
			}
			return timezone, nil
		}
	}
	return "UTC", nil
}

// errorAnalysisPreviousStart is the start of the period a range is compared
// with, which ends where the range starts.
func errorAnalysisPreviousStart(filter ErrorAnalysisFilter, location *time.Location) int64 {
	start, end := time.Unix(filter.Start, 0).In(location), time.Unix(filter.End, 0).In(location)
	startDay := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, location)
	endDay := time.Date(end.Year(), end.Month(), end.Day(), 0, 0, 0, 0, location)
	if start.Equal(startDay) && end.Equal(endDay) {
		// Whole-day windows compare the same number of calendar days.
		// A DST transition can make seven days differ from 7*24 hours.
		startDate := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, time.UTC)
		endDate := time.Date(end.Year(), end.Month(), end.Day(), 0, 0, 0, 0, time.UTC)
		days := int(endDate.Sub(startDate) / (24 * time.Hour))
		return start.AddDate(0, 0, -days).Unix()
	}
	return filter.Start - (filter.End - filter.Start)
}

// PrepareErrorAnalysisSegments returns the task's plan, creating it on the
// first call. It returns nil for a task that started collecting before plans
// existed; such a task finishes with CollectErrorAnalysisPage as before.
func PrepareErrorAnalysisSegments(ctx context.Context, taskID, runnerID string, filter ErrorAnalysisFilter) (*ErrorAnalysisPlan, error) {
	var plan ErrorAnalysisPlan
	found, err := GetErrorAnalysisArtifact(taskID, "plan", "segments", &plan)
	if err != nil {
		return nil, err
	}
	if found {
		return &plan, nil
	}
	var legacy ErrorAnalysisScanProgress
	if found, err = GetErrorAnalysisArtifact(taskID, "scan", "checkpoint", &legacy); err != nil || found {
		return nil, err
	}
	if err := filter.Normalize(time.Now()); err != nil {
		return nil, err
	}
	timezone, err := errorAnalysisTaskTimezone(taskID)
	if err != nil {
		return nil, err
	}
	location, _ := time.LoadLocation(timezone)
	clickHouse := common.UsingLogDatabase(common.DatabaseTypeClickHouse)
	plan = ErrorAnalysisPlan{Version: errorAnalysisSegmentVersion, Filter: filter, Timezone: timezone, Segments: []ErrorAnalysisSegment{}, TotalKnown: !clickHouse, LogRecordingEnabled: common.LogConsumeEnabled, ErrorRecordingEnabled: constant.ErrorLogEnabled}
	if filter.RequestID == "" {
		if previousStart := errorAnalysisPreviousStart(filter, location); previousStart >= 0 {
			plan.Comparison = true
			plan.PreviousFilter = filter
			plan.PreviousFilter.Start, plan.PreviousFilter.End = previousStart, filter.Start
		}
	}
	fingerprint := errorAnalysisSourceFingerprint()
	add := func(start, end int64, previous, shared bool) {
		segment := ErrorAnalysisSegment{Start: start, End: end, Previous: previous, Shared: shared}
		if shared {
			segment.Scope = fmt.Sprintf("eaday%x", sha256.Sum256([]byte(fmt.Sprintf("%d\x00%s\x00%s\x00%d\x00%d\x00%s\x00%d", errorAnalysisSegmentVersion, fingerprint, timezone, start, end, filter.Model, filter.Channel))))[:45]
		} else {
			segment.Scope = fmt.Sprintf("easeg%x", sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%d\x00%t", taskID, start, end, previous))))[:45]
		}
		plan.Segments = append(plan.Segments, segment)
	}
	split := func(start, end int64, previous bool) {
		if filter.RequestID != "" || clickHouse {
			add(start, end, previous, false)
			return
		}
		for at := start; at < end; {
			local := time.Unix(at, 0).In(location)
			day := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, location)
			next := day.AddDate(0, 0, 1).Unix()
			add(at, min(next, end), previous, at == day.Unix() && next <= end)
			at = min(next, end)
		}
	}
	if plan.Comparison {
		split(plan.PreviousFilter.Start, plan.PreviousFilter.End, true)
	}
	split(filter.Start, filter.End, false)

	// A shared day is reused when it was read completely and its rows are
	// the ones read then: the same number of rows and the same newest row.
	var stale []string
	for i := range plan.Segments {
		segment := &plan.Segments[i]
		if clickHouse {
			continue
		}
		var newest int64
		query := errorAnalysisLogQuery(ctx, ErrorAnalysisScanProgress{Filter: ErrorAnalysisFilter{RequestID: filter.RequestID, Start: segment.Start, End: segment.End, Model: filter.Model, Channel: filter.Channel}})
		if err := query.Select("COUNT(*), COALESCE(MAX(id), 0)").Row().Scan(&segment.Rows, &newest); err != nil {
			return nil, err
		}
		plan.TotalRows += segment.Rows
		if !segment.Shared {
			continue
		}
		var scan ErrorAnalysisScanProgress
		var aggregation ErrorAnalysisAggregationProgress
		scanned, err := GetErrorAnalysisArtifact(segment.Scope, "scan", "checkpoint", &scan)
		if err != nil {
			return nil, err
		}
		if _, err := GetErrorAnalysisArtifact(segment.Scope, "aggregate", "checkpoint", &aggregation); err != nil {
			return nil, err
		}
		segment.Reused = scanned && scan.Done && scan.Stable && aggregation.Done && scan.TotalRows == segment.Rows && scan.HighWaterID == newest
		if scanned && !segment.Reused {
			stale = append(stale, segment.Scope)
		}
	}
	artifacts := make([]ErrorAnalysisArtifact, 0, len(plan.Segments)+1)
	for _, segment := range plan.Segments {
		// Writing the entry again records that a report uses the scope now.
		artifact, err := newErrorAnalysisArtifact(errorAnalysisScopeRegistry, "scope", segment.Scope, map[string]any{"start": segment.Start, "end": segment.End, "shared": segment.Shared})
		if err != nil {
			return nil, err
		}
		artifacts = append(artifacts, artifact)
	}
	artifact, err := newErrorAnalysisArtifact(taskID, "plan", "segments", plan)
	if err != nil {
		return nil, err
	}
	err = withErrorAnalysisLease(ctx, taskID, runnerID, func(tx *gorm.DB) error {
		// A day whose rows changed, or that an earlier report left unfinished,
		// is read again from the start.
		if len(stale) > 0 {
			if err := tx.Where("task_id IN ?", stale).Delete(&ErrorAnalysisArtifact{}).Error; err != nil {
				return err
			}
		}
		return upsertErrorAnalysisArtifacts(tx, append(artifacts, artifact))
	})
	if err != nil {
		return nil, err
	}
	return &plan, nil
}

// CollectErrorAnalysisSegmentPage reads one page of a segment. scannedBefore
// is the number of rows the report's other segments already hold.
func CollectErrorAnalysisSegmentPage(ctx context.Context, taskID, runnerID string, plan *ErrorAnalysisPlan, index, batchSize int, scannedBefore int64) (*ErrorAnalysisScanProgress, error) {
	segment := plan.Segments[index]
	filter := plan.Filter
	filter.Start, filter.End = segment.Start, segment.End
	scope := errorAnalysisScope{Task: taskID, ID: segment.Scope, Whole: true, ScannedBefore: scannedBefore, TotalRows: plan.TotalRows, TotalKnown: plan.TotalKnown}
	return collectErrorAnalysisPage(ctx, scope, runnerID, filter, batchSize)
}

func AggregateErrorAnalysisSegmentPage(ctx context.Context, taskID, runnerID string, plan *ErrorAnalysisPlan, index, batchSize, sampleLimit int) (*ErrorAnalysisAggregationProgress, error) {
	return aggregateErrorAnalysisPage(ctx, errorAnalysisScope{Task: taskID, ID: plan.Segments[index].Scope, Whole: true}, runnerID, batchSize, sampleLimit)
}

var errorAnalysisDimensionNames = []string{"model", "channel", "reason", "timeline"}

// MergeErrorAnalysisSegments writes the report's counts, distributions and
// groups from its segments, in the shapes a single scan produces, so the rest
// of the analysis reads one report. It replaces what an interrupted merge
// left behind.
func MergeErrorAnalysisSegments(ctx context.Context, taskID, runnerID string, sampleLimit int) error {
	sampleLimit = min(max(sampleLimit, 1), 8)
	var plan ErrorAnalysisPlan
	found, err := GetErrorAnalysisArtifact(taskID, "plan", "segments", &plan)
	if err != nil {
		return err
	}
	if !found {
		return errors.New("analysis_collection_incomplete")
	}
	if plan.Merged {
		return nil
	}
	scan := ErrorAnalysisScanProgress{Version: ErrorAnalysisWorkflowVersion, Filter: plan.Filter, PreviousFilter: plan.PreviousFilter, Comparison: plan.Comparison, Timezone: plan.Timezone, SourceFingerprint: errorAnalysisSourceFingerprint(), StartedAt: common.GetTimestamp(), Stable: true, TotalKnown: true, Done: true, Warnings: []string{}, LogRecordingEnabled: plan.LogRecordingEnabled, ErrorRecordingEnabled: plan.ErrorRecordingEnabled}
	aggregation := ErrorAnalysisAggregationProgress{Done: true}
	scopes := map[bool][]string{}
	order := map[string]int{}
	previous := map[string]bool{}
	for i, segment := range plan.Segments {
		var part ErrorAnalysisScanProgress
		var counted ErrorAnalysisAggregationProgress
		scanned, err := GetErrorAnalysisArtifact(segment.Scope, "scan", "checkpoint", &part)
		if err != nil {
			return err
		}
		if _, err := GetErrorAnalysisArtifact(segment.Scope, "aggregate", "checkpoint", &counted); err != nil {
			return err
		}
		if !scanned || !part.Done || !counted.Done {
			return errors.New("analysis_collection_incomplete")
		}
		scan.Source = part.Source
		scan.Stable = scan.Stable && part.Stable
		scan.TotalKnown = scan.TotalKnown && part.TotalKnown
		scan.ScannedRows += part.ScannedRows
		scan.TotalRows += part.TotalRows
		if segment.Previous {
			scan.PreviousLogRows += part.CurrentLogRows
		} else {
			scan.CurrentLogRows += part.CurrentLogRows
		}
		for _, warning := range part.Warnings {
			if !slices.Contains(scan.Warnings, warning) {
				scan.Warnings = append(scan.Warnings, warning)
			}
		}
		aggregation.ProcessedRequests += counted.ProcessedRequests
		aggregation.TotalRequests += counted.TotalRequests
		scopes[segment.Previous] = append(scopes[segment.Previous], segment.Scope)
		order[segment.Scope], previous[segment.Scope] = i, segment.Previous
	}
	plan.Merged = true
	return withErrorAnalysisLease(ctx, taskID, runnerID, func(tx *gorm.DB) error {
		kinds := []string{"scan", "aggregate", "counts", "group"}
		for _, period := range []string{"current", "previous"} {
			for _, dimension := range errorAnalysisDimensionNames {
				kinds = append(kinds, "dist_"+period+"_"+dimension)
			}
		}
		if err := tx.Where("task_id = ? AND kind IN ?", taskID, kinds).Delete(&ErrorAnalysisArtifact{}).Error; err != nil {
			return err
		}
		var pending []ErrorAnalysisArtifact
		write := func(kind, name string, value any) error {
			artifact, err := newErrorAnalysisArtifact(taskID, kind, name, value)
			if err != nil {
				return err
			}
			if pending = append(pending, artifact); len(pending) < 100 {
				return nil
			}
			batch := pending
			pending = nil
			return upsertErrorAnalysisArtifacts(tx, batch)
		}
		for _, old := range []bool{false, true} {
			period := "current"
			if old {
				period = "previous"
			}
			if len(scopes[old]) == 0 {
				continue
			}
			err := eachErrorAnalysisArtifactName(tx, scopes[old], "counts", func(name string, rows []ErrorAnalysisArtifact) error {
				if name != "current" {
					return nil
				}
				var total FailureCounts
				for _, row := range rows {
					var counts FailureCounts
					if err := common.UnmarshalJsonStr(string(row.Data), &counts); err != nil {
						return err
					}
					addErrorAnalysisCounts(&total, counts)
				}
				return write("counts", period, total)
			})
			if err != nil {
				return err
			}
			for _, dimension := range errorAnalysisDimensionNames {
				err := eachErrorAnalysisArtifactName(tx, scopes[old], "dist_current_"+dimension, func(name string, rows []ErrorAnalysisArtifact) error {
					var total FailureDistribution
					for _, row := range rows {
						var distribution FailureDistribution
						if err := common.UnmarshalJsonStr(string(row.Data), &distribution); err != nil {
							return err
						}
						total.Name = distribution.Name
						addErrorAnalysisCounts(&total.FailureCounts, distribution.FailureCounts)
					}
					return write("dist_"+period+"_"+dimension, name, total)
				})
				if err != nil {
					return err
				}
			}
		}
		all := append(slices.Clone(scopes[true]), scopes[false]...)
		err := eachErrorAnalysisArtifactName(tx, all, "group", func(name string, rows []ErrorAnalysisArtifact) error {
			// Oldest segment first, so samples are retained as one scan
			// through the whole period would retain them.
			slices.SortFunc(rows, func(a, b ErrorAnalysisArtifact) int { return order[a.TaskID] - order[b.TaskID] })
			merged := ErrorAnalysisGroup{ID: name, Samples: []FailureLog{}, PreviousSamples: []FailureLog{}}
			channels, models := map[string]*ErrorAnalysisShare{}, map[string]*ErrorAnalysisShare{}
			addShares := func(target map[string]*ErrorAnalysisShare, shares []ErrorAnalysisShare, old bool) {
				for _, share := range shares {
					if target[share.Name] == nil {
						target[share.Name] = &ErrorAnalysisShare{Name: share.Name}
					}
					if old {
						target[share.Name].Previous += share.Current
					} else {
						target[share.Name].Current += share.Current
					}
				}
			}
			for i, row := range rows {
				var group ErrorAnalysisGroup
				if err := common.UnmarshalJsonStr(string(row.Data), &group); err != nil {
					return err
				}
				if i == 0 {
					merged.Reason, merged.Categories, merged.Template = group.Reason, group.Categories, group.Template
					merged.FirstSeen, merged.LastSeen = group.FirstSeen, group.LastSeen
				}
				merged.FirstSeen, merged.LastSeen = min(merged.FirstSeen, group.FirstSeen), max(merged.LastSeen, group.LastSeen)
				merged.EvidenceIncomplete = merged.EvidenceIncomplete || group.EvidenceIncomplete
				old := previous[row.TaskID]
				if old {
					addErrorAnalysisCounts(&merged.Previous, group.Current)
				} else {
					addErrorAnalysisCounts(&merged.Current, group.Current)
				}
				for _, sample := range group.Samples {
					if old {
						merged.PreviousSamples = retainErrorAnalysisSamples(merged.PreviousSamples, sample, min(sampleLimit, 2))
					} else {
						merged.Samples = retainErrorAnalysisSamples(merged.Samples, sample, sampleLimit)
					}
				}
				addShares(channels, group.Channels, old)
				addShares(models, group.Models, old)
			}
			var topChannel string
			merged.Channels, topChannel = rankErrorAnalysisShares(channels)
			merged.Models, merged.Model = rankErrorAnalysisShares(models)
			if channel, err := strconv.Atoi(topChannel); err == nil {
				merged.Channel = channel
			}
			aggregation.GroupCount++
			return write("group", name, merged)
		})
		if err != nil {
			return err
		}
		if err := write("scan", "checkpoint", scan); err != nil {
			return err
		}
		if err := write("aggregate", "checkpoint", aggregation); err != nil {
			return err
		}
		if err := write("plan", "segments", plan); err != nil {
			return err
		}
		return upsertErrorAnalysisArtifacts(tx, pending)
	})
}

// rankErrorAnalysisShares orders shares as addErrorAnalysisShare does and
// folds those beyond the limit into "other". It also returns the leading name.
func rankErrorAnalysisShares(shares map[string]*ErrorAnalysisShare) ([]ErrorAnalysisShare, string) {
	var other *ErrorAnalysisShare
	result := make([]ErrorAnalysisShare, 0, len(shares))
	for _, share := range shares {
		if share.Name == "other" {
			other = share
			continue
		}
		result = append(result, *share)
	}
	slices.SortFunc(result, func(a, b ErrorAnalysisShare) int {
		if a.Current != b.Current {
			return b.Current - a.Current
		}
		if a.Previous != b.Previous {
			return b.Previous - a.Previous
		}
		return strings.Compare(a.Name, b.Name)
	})
	limit := errorAnalysisShareLimit
	if other != nil || len(result) > limit {
		if other == nil {
			other = &ErrorAnalysisShare{Name: "other"}
		}
		limit--
		for _, share := range result[min(limit, len(result)):] {
			other.Current += share.Current
			other.Previous += share.Previous
		}
		result = append(result[:min(limit, len(result))], *other)
	}
	top := ""
	if len(result) > 0 {
		top = result[0].Name
	}
	return result, top
}

// eachErrorAnalysisArtifactName calls fn once for every name of the kind, with
// the artifacts that carry it in the given scopes.
func eachErrorAnalysisArtifactName(tx *gorm.DB, scopes []string, kind string, fn func(name string, rows []ErrorAnalysisArtifact) error) error {
	const pageSize = 500
	after := ""
	for {
		var page []ErrorAnalysisArtifact
		if err := tx.Where("task_id IN ? AND kind = ? AND name > ?", scopes, kind, after).Order("name asc, id asc").Limit(pageSize).Find(&page).Error; err != nil {
			return err
		}
		end := len(page)
		if end == pageSize {
			// The last name may continue on the next page.
			for last := page[end-1].Name; end > 0 && page[end-1].Name == last; end-- {
			}
			if end == 0 {
				return errors.New("analysis_internal_error")
			}
		}
		for i := 0; i < end; {
			j := i
			for j < end && page[j].Name == page[i].Name {
				j++
			}
			if err := fn(page[i].Name, page[i:j]); err != nil {
				return err
			}
			after = page[i].Name
			i = j
		}
		if len(page) < pageSize {
			return nil
		}
	}
}

// PruneErrorAnalysisScopes removes segment scopes no report needs. With a
// cutoff, a scope goes when no report has used it since; without one, when no
// report that still exists refers to it. Evidence of a report older than the
// cutoff disappears with its scopes, as it does for a single scan.
func PruneErrorAnalysisScopes(ctx context.Context, cutoff int64) (int64, error) {
	var referenced map[string]bool
	var removed, cursor int64
	for {
		var entries []ErrorAnalysisArtifact
		if err := DB.WithContext(ctx).Select("id", "name", "updated_at").Where("task_id = ? AND kind = ? AND id > ?", errorAnalysisScopeRegistry, "scope", cursor).Order("id asc").Limit(200).Find(&entries).Error; err != nil {
			return removed, err
		}
		for _, entry := range entries {
			cursor = entry.ID
			if cutoff > 0 && entry.UpdatedAt >= cutoff {
				continue
			}
			if cutoff <= 0 {
				if entry.UpdatedAt > common.GetTimestamp()-3600 {
					continue
				}
				if referenced == nil {
					referenced = map[string]bool{}
					var tasks []string
					if err := DB.WithContext(ctx).Model(&SystemTask{}).Where("type = ?", ErrorAnalysisTaskType).Pluck("task_id", &tasks).Error; err != nil {
						return removed, err
					}
					for _, taskID := range tasks {
						var plan ErrorAnalysisPlan
						if _, err := GetErrorAnalysisArtifact(taskID, "plan", "segments", &plan); err != nil {
							return removed, err
						}
						for _, segment := range plan.Segments {
							referenced[segment.Scope] = true
						}
					}
				}
				if referenced[entry.Name] {
					continue
				}
			}
			for {
				var ids []int64
				if err := DB.WithContext(ctx).Model(&ErrorAnalysisArtifact{}).Where("task_id = ?", entry.Name).Order("id asc").Limit(1000).Pluck("id", &ids).Error; err != nil {
					return removed, err
				}
				if len(ids) == 0 {
					break
				}
				result := DB.WithContext(ctx).Where("id IN ?", ids).Delete(&ErrorAnalysisArtifact{})
				if result.Error != nil {
					return removed, result.Error
				}
				removed += result.RowsAffected
			}
			if err := DB.WithContext(ctx).Where("id = ?", entry.ID).Delete(&ErrorAnalysisArtifact{}).Error; err != nil {
				return removed, err
			}
		}
		if len(entries) < 200 {
			return removed, nil
		}
	}
}
