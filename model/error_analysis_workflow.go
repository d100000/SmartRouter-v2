package model

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"maps"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/tidwall/gjson"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

const ErrorAnalysisWorkflowVersion = 2

var ErrErrorAnalysisCheckpointChanged = errors.New("analysis_checkpoint_changed")
var ErrErrorAnalysisResumeUnavailable = errors.New("analysis_resume_unavailable")
var ErrErrorAnalysisSourceChanged = errors.New("analysis_source_changed")

// ErrorAnalysisArtifact keeps checkpoints, bounded request evidence and model
// results separate from the small SystemTask state sent to browser subscribers.
// Request identifiers are hashed into Name so database collations cannot merge
// case-sensitive request IDs. LongText remains portable across all main DBs.
type ErrorAnalysisArtifact struct {
	ID        int64    `json:"id" gorm:"primaryKey;index:idx_error_analysis_artifact_page,priority:3"`
	TaskID    string   `json:"task_id" gorm:"type:varchar(64);not null;uniqueIndex:idx_error_analysis_artifact_identity,priority:1;index:idx_error_analysis_artifact_page,priority:1"`
	Kind      string   `json:"kind" gorm:"type:varchar(32);not null;uniqueIndex:idx_error_analysis_artifact_identity,priority:2;index:idx_error_analysis_artifact_page,priority:2"`
	Name      string   `json:"name" gorm:"type:varchar(64);not null;uniqueIndex:idx_error_analysis_artifact_identity,priority:3"`
	Data      LongText `json:"data"`
	UpdatedAt int64    `json:"updated_at" gorm:"bigint"`
}

type ErrorAnalysisScanProgress struct {
	Version               int                 `json:"version"`
	Filter                ErrorAnalysisFilter `json:"filter"`
	PreviousFilter        ErrorAnalysisFilter `json:"previous_filter"`
	Comparison            bool                `json:"comparison"`
	Source                string              `json:"source"`
	SourceFingerprint     string              `json:"source_fingerprint"`
	Timezone              string              `json:"timezone"`
	StartedAt             int64               `json:"started_at"`
	Generation            string              `json:"generation"`
	HighWaterID           int64               `json:"high_water_id"`
	CursorID              int64               `json:"cursor_id"`
	ScannedRows           int64               `json:"scanned_rows"`
	TotalRows             int64               `json:"total_rows"`
	TotalKnown            bool                `json:"total_known"`
	CurrentLogRows        int64               `json:"current_log_rows"`
	PreviousLogRows       int64               `json:"previous_log_rows"`
	Stable                bool                `json:"stable"`
	Done                  bool                `json:"done"`
	Warnings              []string            `json:"warnings"`
	LogRecordingEnabled   bool                `json:"log_recording_enabled"`
	ErrorRecordingEnabled bool                `json:"error_recording_enabled"`
}

type ErrorAnalysisCoverage struct {
	ScannedRows int64    `json:"scanned_rows"`
	TotalRows   int64    `json:"total_rows"`
	TotalKnown  bool     `json:"total_known"`
	Complete    bool     `json:"complete"`
	Stable      bool     `json:"stable"`
	Source      string   `json:"source"`
	Warnings    []string `json:"warnings"`
}

type ErrorAnalysisAggregationProgress struct {
	CursorID          int64 `json:"cursor_id"`
	ProcessedRequests int64 `json:"processed_requests"`
	TotalRequests     int64 `json:"total_requests"`
	GroupCount        int   `json:"group_count"`
	Done              bool  `json:"done"`
}

type ErrorAnalysisWorkflowSnapshot struct {
	Timezone            string                 `json:"timezone"`
	Current             *ErrorAnalysisSnapshot `json:"current"`
	Previous            *ErrorAnalysisSnapshot `json:"previous,omitempty"`
	Coverage            ErrorAnalysisCoverage  `json:"coverage"`
	ComparisonAvailable bool                   `json:"comparison_available"`
	ComparisonReason    string                 `json:"comparison_reason,omitempty"`
	GroupCount          int                    `json:"group_count"`
}

type ErrorAnalysisGroup struct {
	ID                 string        `json:"id"`
	Reason             string        `json:"reason"`
	Model              string        `json:"model"`
	Channel            int           `json:"channel"`
	Categories         []string      `json:"categories"`
	Current            FailureCounts `json:"current"`
	Previous           FailureCounts `json:"previous"`
	FirstSeen          int64         `json:"first_seen"`
	LastSeen           int64         `json:"last_seen"`
	Samples            []FailureLog  `json:"samples"`
	PreviousSamples    []FailureLog  `json:"previous_samples"`
	EvidenceIncomplete bool          `json:"evidence_incomplete"`
}

type errorAnalysisRequestFact struct {
	Entry    FailureLog `json:"entry"`
	Consumed bool       `json:"consumed"`
	Errored  bool       `json:"errored"`
	SortKey  string     `json:"sort_key"`
}

func GetErrorAnalysisArtifact(taskID, kind, name string, value any) (bool, error) {
	var artifact ErrorAnalysisArtifact
	err := DB.Where("task_id = ? AND kind = ? AND name = ?", taskID, kind, name).Take(&artifact).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, common.UnmarshalJsonStr(string(artifact.Data), value)
}

func ListErrorAnalysisArtifacts(taskID, kind string, afterID int64, limit int) ([]ErrorAnalysisArtifact, error) {
	artifacts := []ErrorAnalysisArtifact{}
	err := DB.Where("task_id = ? AND kind = ? AND id > ?", taskID, kind, afterID).
		Order("id asc").Limit(min(max(limit, 1), 1000)).Find(&artifacts).Error
	return artifacts, err
}

func SaveErrorAnalysisArtifact(taskID, runnerID, kind, name string, value any) error {
	artifact, err := newErrorAnalysisArtifact(taskID, kind, name, value)
	if err != nil {
		return err
	}
	return withErrorAnalysisLease(context.Background(), taskID, runnerID, func(tx *gorm.DB) error {
		return upsertErrorAnalysisArtifacts(tx, []ErrorAnalysisArtifact{artifact})
	})
}

func newErrorAnalysisArtifact(taskID, kind, name string, value any) (ErrorAnalysisArtifact, error) {
	if taskID == "" || len(taskID) > 64 || kind == "" || len(kind) > 32 || name == "" || len(name) > 64 {
		return ErrorAnalysisArtifact{}, errors.New("analysis_invalid_artifact")
	}
	data, err := common.Marshal(value)
	if err != nil {
		return ErrorAnalysisArtifact{}, err
	}
	if len(data) > 8*1024*1024 {
		return ErrorAnalysisArtifact{}, errors.New("analysis_artifact_too_large")
	}
	return ErrorAnalysisArtifact{TaskID: taskID, Kind: kind, Name: name, Data: LongText(data), UpdatedAt: common.GetTimestamp()}, nil
}

func upsertErrorAnalysisArtifacts(tx *gorm.DB, artifacts []ErrorAnalysisArtifact) error {
	if len(artifacts) == 0 {
		return nil
	}
	for start := 0; start < len(artifacts); {
		end, bytes := start, 0
		for end < len(artifacts) && end-start < 100 {
			if end > start && bytes+len(artifacts[end].Data) > 512*1024 {
				break
			}
			bytes += len(artifacts[end].Data)
			end++
		}
		batch := artifacts[start:end]
		if err := tx.Clauses(clause.OnConflict{
			Columns:   []clause.Column{{Name: "task_id"}, {Name: "kind"}, {Name: "name"}},
			DoUpdates: clause.AssignmentColumns([]string{"data", "updated_at"}),
		}).Create(&batch).Error; err != nil {
			return err
		}
		start = end
	}
	return nil
}

// Every artifact mutation serializes with lease transfer. The final expiry
// check also prevents a slow transaction from committing after its lease died.
func withErrorAnalysisLease(ctx context.Context, taskID, runnerID string, fn func(*gorm.DB) error) error {
	return DB.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var lease SystemTaskLock
		err := lockForUpdate(tx).Where("type = ? AND task_id = ? AND locked_by = ? AND locked_until >= ?", ErrorAnalysisTaskType, taskID, runnerID, common.GetTimestamp()).Take(&lease).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrSystemTaskLockLost
		}
		if err != nil {
			return err
		}
		var task SystemTask
		err = lockForUpdate(tx).Where("task_id = ? AND type = ? AND status = ? AND locked_by = ?", taskID, ErrorAnalysisTaskType, SystemTaskStatusRunning, runnerID).Take(&task).Error
		if errors.Is(err, gorm.ErrRecordNotFound) {
			return ErrSystemTaskLockLost
		}
		if err != nil {
			return err
		}
		if gjson.Get(task.Payload, "workflow_version").Int() != ErrorAnalysisWorkflowVersion {
			return errors.New("analysis_invalid_workflow")
		}
		if err := fn(tx); err != nil {
			return err
		}
		if lease.LockedUntil < common.GetTimestamp() {
			return ErrSystemTaskLockLost
		}
		return nil
	})
}

func errorAnalysisLogQuery(ctx context.Context, scan ErrorAnalysisScanProgress) *gorm.DB {
	query := LOG_DB.WithContext(ctx).Model(&Log{}).Where("type IN ?", []int{LogTypeConsume, LogTypeError})
	if scan.Filter.RequestID != "" {
		query = query.Where("request_id = ?", scan.Filter.RequestID)
	} else {
		start := scan.Filter.Start
		if scan.Comparison {
			start = scan.PreviousFilter.Start
		}
		query = query.Where("created_at >= ? AND created_at < ?", start, scan.Filter.End)
	}
	if scan.Filter.Model != "" {
		query = query.Where("model_name = ?", scan.Filter.Model)
	}
	if scan.Filter.Channel > 0 {
		query = query.Where("channel_id = ?", scan.Filter.Channel)
	}
	return query
}

// CollectErrorAnalysisPage persists request facts and their cursor together.
// SQL stores use a frozen ID high-water mark. ClickHouse has no unique event
// ID, so one streaming SELECT owns its read snapshot; an interrupted collection
// restarts only collection, never resumes by an ambiguous timestamp/request ID.
func CollectErrorAnalysisPage(ctx context.Context, taskID, runnerID string, filter ErrorAnalysisFilter, batchSize int) (*ErrorAnalysisScanProgress, error) {
	batchSize = min(max(batchSize, 1), 1000)
	var scan ErrorAnalysisScanProgress
	found, err := GetErrorAnalysisArtifact(taskID, "scan", "checkpoint", &scan)
	if err != nil {
		return nil, err
	}
	if found && scan.Done {
		return &scan, nil
	}
	// Match InitLogDB/chooseDB's configuration precedence. Only the digest is
	// persisted: DSNs can contain credentials and must never enter task state,
	// error messages or logs. Completed snapshots no longer need their source.
	source := os.Getenv("LOG_SQL_DSN")
	if source == "" {
		source = os.Getenv("SQL_DSN")
	}
	if source == "" || strings.HasPrefix(source, "local") {
		source = common.SQLitePath
	}
	fingerprint := fmt.Sprintf("%x", sha256.Sum256([]byte(string(common.LogDatabaseType())+"\x00"+source)))
	if found && scan.SourceFingerprint != fingerprint {
		return nil, ErrErrorAnalysisSourceChanged
	}
	clickHouse := common.UsingLogDatabase(common.DatabaseTypeClickHouse)
	if !found || clickHouse {
		if found {
			filter = scan.Filter
		} else if err := filter.Normalize(time.Now()); err != nil {
			return nil, err
		}
		scan = ErrorAnalysisScanProgress{Version: ErrorAnalysisWorkflowVersion, Filter: filter, StartedAt: common.GetTimestamp(), Generation: common.GetRandomString(24), Stable: true, Source: "id_watermark", SourceFingerprint: fingerprint, Timezone: "UTC", Warnings: []string{}, LogRecordingEnabled: common.LogConsumeEnabled, ErrorRecordingEnabled: constant.ErrorLogEnabled}
		configs, err := ListErrorAnalysisArtifacts(taskID, "config", 0, 1)
		if err != nil {
			return nil, err
		}
		if len(configs) > 0 {
			if timezone := gjson.Get(string(configs[0].Data), "timezone").String(); timezone != "" {
				if _, err := time.LoadLocation(timezone); err != nil {
					return nil, errors.New("analysis_invalid_timezone")
				}
				scan.Timezone = timezone
			}
		}
		if filter.RequestID == "" {
			previousStart := filter.Start - (filter.End - filter.Start)
			location, _ := time.LoadLocation(scan.Timezone)
			start, end := time.Unix(filter.Start, 0).In(location), time.Unix(filter.End, 0).In(location)
			startDay := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, location)
			endDay := time.Date(end.Year(), end.Month(), end.Day(), 0, 0, 0, 0, location)
			if start.Equal(startDay) && end.Equal(endDay) {
				// Whole-day windows compare the same number of calendar days.
				// A DST transition can make seven days differ from 7*24 hours.
				startDate := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, time.UTC)
				endDate := time.Date(end.Year(), end.Month(), end.Day(), 0, 0, 0, 0, time.UTC)
				days := int(endDate.Sub(startDate) / (24 * time.Hour))
				previousStart = start.AddDate(0, 0, -days).Unix()
			}
			if previousStart >= 0 {
				scan.Comparison = true
				scan.PreviousFilter = filter
				scan.PreviousFilter.Start, scan.PreviousFilter.End = previousStart, filter.Start
			}
		}
		if filter.Channel != 0 || filter.Model != "" {
			scan.Warnings = append(scan.Warnings, "cross_filter_retry_evidence_incomplete")
		}
		if clickHouse {
			scan.Source = "clickhouse_single_query"
			var engine string
			if err := LOG_DB.WithContext(ctx).Raw("SELECT engine FROM system.tables WHERE database = currentDatabase() AND name = 'logs'").Scan(&engine).Error; err != nil || !strings.HasSuffix(engine, "MergeTree") {
				scan.Stable = false
				scan.Warnings = append(scan.Warnings, "clickhouse_snapshot_engine_unverified")
			}
		} else {
			scan.TotalKnown = true
			if err := errorAnalysisLogQuery(ctx, scan).Select("COALESCE(MAX(id), 0)").Scan(&scan.HighWaterID).Error; err != nil {
				return nil, err
			}
			if err := errorAnalysisLogQuery(ctx, scan).Where("id <= ?", scan.HighWaterID).Count(&scan.TotalRows).Error; err != nil {
				return nil, err
			}
		}
		checkpoint, err := newErrorAnalysisArtifact(taskID, "scan", "checkpoint", scan)
		if err != nil {
			return nil, err
		}
		err = withErrorAnalysisLease(ctx, taskID, runnerID, func(tx *gorm.DB) error {
			// No aggregation runs before collection is complete. A ClickHouse
			// restart replaces only the uncompleted source snapshot and facts.
			if clickHouse {
				if err := tx.Where("task_id = ? AND kind IN ?", taskID, []string{"request", "scan"}).Delete(&ErrorAnalysisArtifact{}).Error; err != nil {
					return err
				}
			}
			return upsertErrorAnalysisArtifacts(tx, []ErrorAnalysisArtifact{checkpoint})
		})
		if err != nil {
			return nil, err
		}
	}
	query := errorAnalysisLogQuery(ctx, scan).Select("id", "created_at", "type", "model_name", "channel_id", "request_id", "upstream_request_id", "prompt_tokens", "completion_tokens", "quota", "content", "use_time", "is_stream", "other")
	if !clickHouse {
		query = query.Where("id > ? AND id <= ?", scan.CursorID, scan.HighWaterID).Order("id asc").Limit(batchSize)
	}
	rows, err := query.Rows()
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	page := make([]Log, 0, batchSize)
	pageBytes := 0
	byteLimited := false
	for rows.Next() {
		var log Log
		if err := LOG_DB.ScanRows(rows, &log); err != nil {
			return nil, err
		}
		page = append(page, log)
		pageBytes += len(log.Content) + len(log.Other)
		if clickHouse && (len(page) >= batchSize || pageBytes >= 4*1024*1024) {
			if err := persistErrorAnalysisLogPage(ctx, taskID, runnerID, &scan, page, false); err != nil {
				return nil, err
			}
			page, pageBytes = page[:0], 0
		} else if !clickHouse && pageBytes >= 4*1024*1024 {
			byteLimited = true
			break
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	done := clickHouse || !byteLimited && len(page) < batchSize
	if err := persistErrorAnalysisLogPage(ctx, taskID, runnerID, &scan, page, done); err != nil {
		return nil, err
	}
	return &scan, nil
}

func persistErrorAnalysisLogPage(ctx context.Context, taskID, runnerID string, scan *ErrorAnalysisScanProgress, logs []Log, done bool) error {
	next := *scan
	next.Warnings = slices.Clone(scan.Warnings)
	keys := make([]string, len(logs))
	for i := range logs {
		log := &logs[i]
		next.ScannedRows++
		next.CursorID = max(next.CursorID, int64(log.Id))
		if next.Filter.RequestID != "" && next.Filter.RequestID != log.RequestId {
			continue
		}
		if next.Filter.RequestID == "" && log.CreatedAt < next.Filter.Start {
			next.PreviousLogRows++
		} else {
			next.CurrentLogRows++
		}
		identity := "request:" + log.RequestId
		if log.RequestId == "" {
			identity = fmt.Sprintf("legacy:%d:%d", log.CreatedAt, log.Id)
			if scan.Source == "clickhouse_single_query" {
				identity = fmt.Sprintf("legacy:%s:%d", scan.Generation, next.ScannedRows)
			}
		}
		keys[i] = fmt.Sprintf("%x", sha256.Sum256([]byte(identity)))
	}
	next.Done = done
	if next.Source == "clickhouse_single_query" && done {
		next.TotalRows = next.ScannedRows
		next.TotalKnown = true
	} else if next.Source != "clickhouse_single_query" && done && next.ScannedRows != next.TotalRows {
		next.Stable = false
		next.Warnings = append(next.Warnings, "source_rows_changed_during_scan")
	}
	err := withErrorAnalysisLease(ctx, taskID, runnerID, func(tx *gorm.DB) error {
		var stored ErrorAnalysisArtifact
		if err := tx.Where("task_id = ? AND kind = ? AND name = ?", taskID, "scan", "checkpoint").Take(&stored).Error; err != nil {
			return err
		}
		var current ErrorAnalysisScanProgress
		if err := common.UnmarshalJsonStr(string(stored.Data), &current); err != nil {
			return err
		}
		if current.Generation != scan.Generation || current.ScannedRows != scan.ScannedRows || current.Done {
			return ErrErrorAnalysisCheckpointChanged
		}
		facts := map[string]errorAnalysisRequestFact{}
		if len(keys) > 0 {
			var existing []ErrorAnalysisArtifact
			if err := tx.Where("task_id = ? AND kind = ? AND name IN ?", taskID, "request", keys).Find(&existing).Error; err != nil {
				return err
			}
			for _, artifact := range existing {
				var fact errorAnalysisRequestFact
				if err := common.UnmarshalJsonStr(string(artifact.Data), &fact); err != nil {
					return err
				}
				facts[artifact.Name] = fact
			}
		}
		for i, log := range logs {
			// SQL's candidate lookup can be case-insensitive on MySQL.
			if next.Filter.RequestID != "" && next.Filter.RequestID != log.RequestId {
				continue
			}
			entry := ClassifyFailureLog(log)
			compactErrorAnalysisEvidence(&entry)
			fact := facts[keys[i]]
			mergeErrorAnalysisRequest(&fact, entry)
			facts[keys[i]] = fact
		}
		artifacts := make([]ErrorAnalysisArtifact, 0, len(facts)+1)
		for name, fact := range facts {
			artifact, err := newErrorAnalysisArtifact(taskID, "request", name, fact)
			if err != nil {
				return err
			}
			artifacts = append(artifacts, artifact)
		}
		checkpoint, err := newErrorAnalysisArtifact(taskID, "scan", "checkpoint", next)
		if err != nil {
			return err
		}
		if err := upsertErrorAnalysisArtifacts(tx, append(artifacts, checkpoint)); err != nil {
			return err
		}
		var task SystemTask
		if err := tx.Where("task_id = ?", taskID).Take(&task).Error; err != nil {
			return err
		}
		state := map[string]any{}
		if err := task.DecodeState(&state); err != nil {
			return err
		}
		state["version"], state["scanned_rows"], state["total_rows"], state["total_known"] = ErrorAnalysisWorkflowVersion, next.ScannedRows, next.TotalRows, next.TotalKnown
		state["updated_at"] = common.GetTimestamp()
		encoded, err := common.Marshal(state)
		if err != nil {
			return err
		}
		return tx.Model(&task).Updates(map[string]any{"state": string(encoded), "updated_at": common.GetTimestamp()}).Error
	})
	if err == nil {
		*scan = next
	}
	return err
}

func compactErrorAnalysisEvidence(entry *FailureLog) {
	entry.ErrorTruncated = entry.ErrorTruncated || len([]rune(entry.ErrorMessage)) > 1024
	entry.ErrorMessage = SafeFailureFact(entry.ErrorMessage, 1024)
	if entry.Evidence == nil {
		return
	}
	if len(entry.Evidence.Upstream) > 5 {
		entry.Evidence.Upstream = entry.Evidence.Upstream[len(entry.Evidence.Upstream)-5:]
		entry.Evidence.AttemptsTruncated = true
	}
	for i := range entry.Evidence.Upstream {
		attempt := &entry.Evidence.Upstream[i]
		attempt.ErrorTruncated = attempt.ErrorTruncated || len([]rune(attempt.ErrorBody)) > 1024
		attempt.ErrorBody = SafeFailureFact(attempt.ErrorBody, 1024)
		usageBytes := 0
		for _, key := range slices.Sorted(maps.Keys(attempt.Usage)) {
			value := attempt.Usage[key]
			if len(value) > 2048 || usageBytes+len(key)+len(value) > 4096 {
				delete(attempt.Usage, key)
				attempt.CaptureIncomplete = true
			} else {
				usageBytes += len(key) + len(value)
			}
		}
	}
	if downstream := entry.Evidence.Downstream; downstream != nil {
		usageBytes := 0
		for _, key := range slices.Sorted(maps.Keys(downstream.Usage)) {
			value := downstream.Usage[key]
			if len(value) > 2048 || usageBytes+len(key)+len(value) > 4096 {
				delete(downstream.Usage, key)
				downstream.CaptureIncomplete = true
			} else {
				usageBytes += len(key) + len(value)
			}
		}
	}
}

func mergeErrorAnalysisRequest(fact *errorAnalysisRequestFact, entry FailureLog) {
	encoded, _ := common.Marshal(entry)
	sortKey := fmt.Sprintf("%020d:%020d:%x", entry.CreatedAt, entry.ID, sha256.Sum256(encoded))
	previous := fact.Entry
	replace := previous.Attempts == 0 || entry.LogType == LogTypeConsume && !fact.Consumed || ((entry.LogType == LogTypeConsume) == fact.Consumed && sortKey > fact.SortKey)
	if replace {
		fact.Entry, fact.SortKey = entry, sortKey
	}
	fact.Consumed = fact.Consumed || entry.LogType == LogTypeConsume
	fact.Errored = fact.Errored || entry.LogType == LogTypeError
	fact.Entry.Attempts = previous.Attempts + 1
	fact.Entry.RecordedQuota = previous.RecordedQuota + entry.RecordedQuota
	records := append(previous.Records, entry)
	slices.SortFunc(records, func(a, b FailureLog) int {
		if a.CreatedAt != b.CreatedAt {
			if a.CreatedAt > b.CreatedAt {
				return -1
			}
			return 1
		}
		return b.ID - a.ID
	})
	fact.Entry.Records = records[:min(len(records), 8)]
	fact.Entry.RecordsTruncated = previous.RecordsTruncated || fact.Entry.Attempts > len(fact.Entry.Records)
	for _, category := range append(slices.Clone(previous.Categories), entry.Categories...) {
		if (category == "settlement_risk" || category == "upstream_usage_unbilled" || category == "charged_zero_output") && !slices.Contains(fact.Entry.Categories, category) {
			fact.Entry.Categories = append(fact.Entry.Categories, category)
		}
	}
}

func finalizedErrorAnalysisRequest(fact errorAnalysisRequestFact) FailureLog {
	entry := fact.Entry
	if fact.Consumed && fact.Errored && !slices.Contains(entry.Categories, "error") && !slices.Contains(entry.Categories, "interrupted") && !slices.Contains(entry.Categories, "zero_output") && !slices.Contains(entry.Categories, "client_cancelled") && !slices.Contains(entry.Categories, "recovered") {
		entry.Categories = append(slices.Clone(entry.Categories), "recovered")
	}
	return entry
}

func GetErrorAnalysisRequestEvidence(taskID, requestID string) (*FailureLog, error) {
	if requestID == "" {
		return nil, errors.New("analysis_request_evidence_unavailable")
	}
	name := fmt.Sprintf("%x", sha256.Sum256([]byte("request:"+requestID)))
	var fact errorAnalysisRequestFact
	found, err := GetErrorAnalysisArtifact(taskID, "request", name, &fact)
	if err != nil || !found {
		return nil, err
	}
	entry := finalizedErrorAnalysisRequest(fact)
	return &entry, nil
}

var errorAnalysisVariablePattern = regexp.MustCompile(`(?i)\b[0-9a-f]{8}-[0-9a-f-]{27,}\b|\b[0-9]+\b`)

func AggregateErrorAnalysisPage(ctx context.Context, taskID, runnerID string, batchSize, sampleLimit int) (*ErrorAnalysisAggregationProgress, error) {
	batchSize = min(max(batchSize, 1), 500)
	sampleLimit = min(max(sampleLimit, 1), 8)
	var scan ErrorAnalysisScanProgress
	found, err := GetErrorAnalysisArtifact(taskID, "scan", "checkpoint", &scan)
	if err != nil {
		return nil, err
	}
	if !found || !scan.Done {
		return nil, errors.New("analysis_collection_incomplete")
	}
	location, err := time.LoadLocation(scan.Timezone)
	if err != nil {
		return nil, errors.New("analysis_invalid_timezone")
	}
	var progress ErrorAnalysisAggregationProgress
	if _, err := GetErrorAnalysisArtifact(taskID, "aggregate", "checkpoint", &progress); err != nil {
		return nil, err
	}
	if progress.Done {
		return &progress, nil
	}
	if err := DB.WithContext(ctx).Model(&ErrorAnalysisArtifact{}).Where("task_id = ? AND kind = ?", taskID, "request").Count(&progress.TotalRequests).Error; err != nil {
		return nil, err
	}
	page, err := ListErrorAnalysisArtifacts(taskID, "request", progress.CursorID, batchSize)
	if err != nil {
		return nil, err
	}
	beforeCursor := progress.CursorID
	err = withErrorAnalysisLease(ctx, taskID, runnerID, func(tx *gorm.DB) error {
		var stored ErrorAnalysisArtifact
		err := tx.Where("task_id = ? AND kind = ? AND name = ?", taskID, "aggregate", "checkpoint").Take(&stored).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if err == nil && gjson.Get(string(stored.Data), "cursor_id").Int() != beforeCursor {
			return ErrErrorAnalysisCheckpointChanged
		}
		counts := map[string]*FailureCounts{}
		distributions := map[string]*FailureDistribution{}
		groups := map[string]*ErrorAnalysisGroup{}
		for _, artifact := range page {
			var fact errorAnalysisRequestFact
			if err := common.UnmarshalJsonStr(string(artifact.Data), &fact); err != nil {
				return err
			}
			entry := finalizedErrorAnalysisRequest(fact)
			period := "current"
			if scan.Comparison && entry.CreatedAt < scan.Filter.Start {
				period = "previous"
			}
			if counts[period] == nil {
				counts[period] = &FailureCounts{}
				var previous ErrorAnalysisArtifact
				err := tx.Where("task_id = ? AND kind = ? AND name = ?", taskID, "counts", period).Take(&previous).Error
				if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
					return err
				}
				if err == nil {
					if err := common.UnmarshalJsonStr(string(previous.Data), counts[period]); err != nil {
						return err
					}
				}
			}
			counts[period].add(entry)
			reason := entry.Diagnostic.ErrorCode
			if reason == "" {
				reason = entry.Diagnostic.EndReason
			}
			if reason == "" {
				reason = strconv.Itoa(entry.Diagnostic.StatusCode)
			}
			day := time.Unix(entry.CreatedAt, 0).In(location)
			day = time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, location)
			for dimension, value := range map[string]string{"model": entry.Model, "channel": strconv.Itoa(entry.Channel), "reason": reason, "timeline": strconv.FormatInt(day.Unix(), 10)} {
				kind := "dist_" + period + "_" + dimension
				name := fmt.Sprintf("%x", sha256.Sum256([]byte(value)))
				identity := kind + ":" + name
				if distributions[identity] == nil {
					distributions[identity] = &FailureDistribution{Name: value}
					var previous ErrorAnalysisArtifact
					err := tx.Where("task_id = ? AND kind = ? AND name = ?", taskID, kind, name).Take(&previous).Error
					if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
						return err
					}
					if err == nil {
						if err := common.UnmarshalJsonStr(string(previous.Data), distributions[identity]); err != nil {
							return err
						}
					}
				}
				distributions[identity].add(entry)
			}
			if len(entry.Categories) > 0 {
				categories := slices.Clone(entry.Categories)
				slices.Sort(categories)
				fingerprint := errorAnalysisVariablePattern.ReplaceAllString(entry.ErrorMessage, "#")
				fingerprint = strings.Join(strings.Fields(fingerprint), " ")
				name := fmt.Sprintf("%x", sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d\x00%s\x00%s\x00%s", entry.Model, entry.Channel, strings.Join(categories, ","), reason, fingerprint))))
				if groups[name] == nil {
					groups[name] = &ErrorAnalysisGroup{ID: name, Reason: reason, Model: entry.Model, Channel: entry.Channel, Categories: categories, FirstSeen: entry.CreatedAt, LastSeen: entry.CreatedAt, Samples: []FailureLog{}, PreviousSamples: []FailureLog{}}
					var previous ErrorAnalysisArtifact
					err := tx.Where("task_id = ? AND kind = ? AND name = ?", taskID, "group", name).Take(&previous).Error
					if errors.Is(err, gorm.ErrRecordNotFound) {
						progress.GroupCount++
					} else if err != nil {
						return err
					} else if err := common.UnmarshalJsonStr(string(previous.Data), groups[name]); err != nil {
						return err
					}
				}
				group := groups[name]
				group.FirstSeen = min(group.FirstSeen, entry.CreatedAt)
				group.LastSeen = max(group.LastSeen, entry.CreatedAt)
				group.EvidenceIncomplete = group.EvidenceIncomplete || entry.ErrorTruncated || entry.RecordsTruncated
				for _, record := range append([]FailureLog{entry}, entry.Records...) {
					group.EvidenceIncomplete = group.EvidenceIncomplete || record.ErrorTruncated
					if evidence := record.Evidence; evidence != nil {
						group.EvidenceIncomplete = group.EvidenceIncomplete || evidence.AttemptsTruncated
						if evidence.Downstream != nil {
							group.EvidenceIncomplete = group.EvidenceIncomplete || evidence.Downstream.CaptureIncomplete
						}
						for _, attempt := range evidence.Upstream {
							group.EvidenceIncomplete = group.EvidenceIncomplete || attempt.ErrorTruncated || attempt.CaptureIncomplete
						}
					}
				}
				if period == "previous" {
					group.Previous.add(entry)
					group.PreviousSamples = retainErrorAnalysisSamples(group.PreviousSamples, entry, min(sampleLimit, 2))
				} else {
					group.Current.add(entry)
					group.Samples = retainErrorAnalysisSamples(group.Samples, entry, sampleLimit)
				}
			}
			progress.CursorID = artifact.ID
			progress.ProcessedRequests++
		}
		progress.Done = len(page) < batchSize
		artifacts := make([]ErrorAnalysisArtifact, 0, len(counts)+len(distributions)+len(groups)+1)
		for period, value := range counts {
			artifact, err := newErrorAnalysisArtifact(taskID, "counts", period, value)
			if err != nil {
				return err
			}
			artifacts = append(artifacts, artifact)
		}
		for identity, value := range distributions {
			kind, name, _ := strings.Cut(identity, ":")
			artifact, err := newErrorAnalysisArtifact(taskID, kind, name, value)
			if err != nil {
				return err
			}
			artifacts = append(artifacts, artifact)
		}
		for name, value := range groups {
			artifact, err := newErrorAnalysisArtifact(taskID, "group", name, value)
			if err != nil {
				return err
			}
			artifacts = append(artifacts, artifact)
		}
		checkpoint, err := newErrorAnalysisArtifact(taskID, "aggregate", "checkpoint", progress)
		if err != nil {
			return err
		}
		return upsertErrorAnalysisArtifacts(tx, append(artifacts, checkpoint))
	})
	return &progress, err
}

func retainErrorAnalysisSamples(samples []FailureLog, entry FailureLog, limit int) []FailureLog {
	if len(samples) < limit {
		return append(samples, entry)
	}
	// Retain early examples and the latest example so a large repeated group
	// cannot hide its current behavior behind only its oldest requests.
	if entry.CreatedAt >= samples[len(samples)-1].CreatedAt {
		samples[len(samples)-1] = entry
	}
	return samples
}

func LoadErrorAnalysisWorkflowSnapshot(taskID string) (*ErrorAnalysisWorkflowSnapshot, error) {
	var scan ErrorAnalysisScanProgress
	found, err := GetErrorAnalysisArtifact(taskID, "scan", "checkpoint", &scan)
	if err != nil || !found {
		return nil, err
	}
	var aggregation ErrorAnalysisAggregationProgress
	if _, err := GetErrorAnalysisArtifact(taskID, "aggregate", "checkpoint", &aggregation); err != nil {
		return nil, err
	}
	result := &ErrorAnalysisWorkflowSnapshot{
		Timezone:   scan.Timezone,
		Coverage:   ErrorAnalysisCoverage{ScannedRows: scan.ScannedRows, TotalRows: scan.TotalRows, TotalKnown: scan.TotalKnown, Complete: scan.Done && aggregation.Done && scan.Stable, Stable: scan.Stable, Source: scan.Source, Warnings: slices.Clone(scan.Warnings)},
		GroupCount: aggregation.GroupCount,
	}
	location, err := time.LoadLocation(scan.Timezone)
	if err != nil {
		return nil, err
	}
	for _, period := range []string{"current", "previous"} {
		if period == "previous" && !scan.Comparison {
			continue
		}
		filter, rows := scan.Filter, scan.CurrentLogRows
		if period == "previous" {
			filter, rows = scan.PreviousFilter, scan.PreviousLogRows
		}
		snapshot := &ErrorAnalysisSnapshot{Filter: filter, TotalLogRows: rows, ScannedLogRows: int(rows), Truncated: !result.Coverage.Complete, LogRecordingEnabled: scan.LogRecordingEnabled, ErrorRecordingEnabled: scan.ErrorRecordingEnabled, Logs: []FailureLog{}}
		if _, err := GetErrorAnalysisArtifact(taskID, "counts", period, &snapshot.FailureCounts); err != nil {
			return nil, err
		}
		for dimension, target := range map[string]*[]FailureDistribution{"model": &snapshot.Models, "channel": &snapshot.Channels, "reason": &snapshot.Reasons, "timeline": &snapshot.Timeline} {
			*target = []FailureDistribution{}
			var cursor int64
			other := FailureDistribution{Name: "other"}
			for {
				artifacts, err := ListErrorAnalysisArtifacts(taskID, "dist_"+period+"_"+dimension, cursor, 500)
				if err != nil {
					return nil, err
				}
				for _, artifact := range artifacts {
					var distribution FailureDistribution
					if err := common.UnmarshalJsonStr(string(artifact.Data), &distribution); err != nil {
						return nil, err
					}
					*target = append(*target, distribution)
					cursor = artifact.ID
					if dimension != "timeline" && len(*target) > 100 {
						slices.SortFunc(*target, compareErrorAnalysisDistribution)
						dropped := (*target)[100]
						addErrorAnalysisCounts(&other.FailureCounts, dropped.FailureCounts)
						*target = (*target)[:100]
					}
				}
				if len(artifacts) < 500 {
					break
				}
			}
			if other.Requests > 0 {
				*target = append(*target, other)
			}
			if dimension == "timeline" {
				present := map[string]bool{}
				for _, bucket := range *target {
					present[bucket.Name] = true
				}
				day := time.Unix(filter.Start, 0).In(location)
				day = time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, location)
				for filter.RequestID == "" && day.Unix() < filter.End {
					name := strconv.FormatInt(day.Unix(), 10)
					if !present[name] {
						*target = append(*target, FailureDistribution{Name: name})
					}
					day = day.AddDate(0, 0, 1)
				}
				slices.SortFunc(*target, func(a, b FailureDistribution) int { return strings.Compare(a.Name, b.Name) })
			} else {
				slices.SortFunc(*target, compareErrorAnalysisDistribution)
			}
		}
		if period == "current" {
			result.Current = snapshot
		} else {
			result.Previous = snapshot
		}
	}
	result.ComparisonAvailable = result.Coverage.Complete && result.Previous != nil && result.Previous.Requests > 0
	if !result.ComparisonAvailable {
		result.ComparisonReason = "previous_period_unavailable"
		if !result.Coverage.Complete {
			result.ComparisonReason = "analysis_coverage_incomplete"
		}
	}
	return result, nil
}

func compareErrorAnalysisDistribution(a, b FailureDistribution) int {
	if a.Affected != b.Affected {
		return b.Affected - a.Affected
	}
	return strings.Compare(a.Name, b.Name)
}

func addErrorAnalysisCounts(to *FailureCounts, from FailureCounts) {
	to.Requests += from.Requests
	to.Affected += from.Affected
	to.ZeroOutput += from.ZeroOutput
	to.Interrupted += from.Interrupted
	to.Errors += from.Errors
	to.ClientCancelled += from.ClientCancelled
	to.UnknownOutput += from.UnknownOutput
	to.Recovered += from.Recovered
	to.SettlementRisk += from.SettlementRisk
	to.UpstreamUsageUnbilled += from.UpstreamUsageUnbilled
	to.ChargedZeroOutput += from.ChargedZeroOutput
}

// ResumeErrorAnalysisTask grants one additional configured call budget only
// after an explicit resume. Automatic lease recovery never replenishes it.
func ResumeErrorAnalysisTask(taskID string) (*SystemTask, error) {
	var task SystemTask
	err := DB.Transaction(func(tx *gorm.DB) error {
		var lease SystemTaskLock
		err := lockForUpdate(tx).Where("type = ?", ErrorAnalysisTaskType).Take(&lease).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if err == nil && lease.LockedUntil >= common.GetTimestamp() {
			return errors.New("analysis_already_active")
		}
		if err := lockForUpdate(tx).Where("task_id = ? AND type = ?", taskID, ErrorAnalysisTaskType).Take(&task).Error; err != nil {
			return err
		}
		if task.Status != SystemTaskStatusFailed || gjson.Get(task.Payload, "workflow_version").Int() != ErrorAnalysisWorkflowVersion {
			return ErrErrorAnalysisResumeUnavailable
		}
		var config ErrorAnalysisArtifact
		if err := tx.Where("task_id = ? AND kind = ? AND name = ?", taskID, "config", "snapshot").Take(&config).Error; err != nil {
			if errors.Is(err, gorm.ErrRecordNotFound) {
				return ErrErrorAnalysisResumeUnavailable
			}
			return err
		}
		var active int64
		if err := tx.Model(&SystemTask{}).Where("type = ? AND task_id <> ? AND status IN ?", ErrorAnalysisTaskType, taskID, activeSystemTaskStatuses()).Count(&active).Error; err != nil {
			return err
		}
		if active > 0 {
			return errors.New("analysis_already_active")
		}
		state := map[string]any{}
		if err := task.DecodeState(&state); err != nil {
			return err
		}
		if canResume, ok := state["can_resume"].(bool); !ok || !canResume {
			return ErrErrorAnalysisResumeUnavailable
		}
		used := max(0, gjson.Get(string(task.State), "calls_used").Int())
		budget := gjson.Get(string(config.Data), "max_calls").Int()
		if budget < 1 || budget > 10000 {
			budget = 100
		}
		if used > 1<<53-1-budget {
			return errors.New("analysis_call_budget_exhausted")
		}
		state["can_resume"], state["phase"], state["max_calls"], state["updated_at"] = false, "collecting", used+budget, common.GetTimestamp()
		encoded, err := common.Marshal(state)
		if err != nil {
			return err
		}
		if err := tx.Model(&task).Updates(map[string]any{"status": SystemTaskStatusPending, "active_key": ErrorAnalysisTaskType, "locked_by": "", "error": "", "state": string(encoded), "updated_at": common.GetTimestamp()}).Error; err != nil {
			return err
		}
		if lease.TaskID == taskID {
			if err := tx.Where("type = ? AND task_id = ? AND locked_by = ? AND locked_until < ?", ErrorAnalysisTaskType, taskID, lease.LockedBy, common.GetTimestamp()).Delete(&SystemTaskLock{}).Error; err != nil {
				return err
			}
		}
		return tx.Where("task_id = ?", taskID).Take(&task).Error
	})
	if err != nil {
		return nil, err
	}
	return &task, nil
}

// recoverExpiredErrorAnalysisTask handles only workflow v2 jobs that have
// persisted their configuration. Other task types retain the old lease policy.
func recoverExpiredErrorAnalysisTask(taskID string) (bool, error) {
	var candidate SystemTask
	err := DB.Where("task_id = ?", taskID).Take(&candidate).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	if candidate.Type != ErrorAnalysisTaskType || gjson.Get(candidate.Payload, "workflow_version").Int() != ErrorAnalysisWorkflowVersion {
		return false, nil
	}
	handled := false
	err = DB.Transaction(func(tx *gorm.DB) error {
		var lease SystemTaskLock
		err := lockForUpdate(tx).Where("type = ?", ErrorAnalysisTaskType).Take(&lease).Error
		if err != nil && !errors.Is(err, gorm.ErrRecordNotFound) {
			return err
		}
		if err == nil && lease.TaskID != taskID {
			return nil
		}
		if err == nil && lease.LockedUntil >= common.GetTimestamp() {
			handled = true
			return nil
		}
		var task SystemTask
		if err := lockForUpdate(tx).Where("task_id = ?", taskID).Take(&task).Error; err != nil {
			return err
		}
		if task.Status != SystemTaskStatusRunning {
			handled = true
			return nil
		}
		var configCount int64
		if err := tx.Model(&ErrorAnalysisArtifact{}).Where("task_id = ? AND kind = ? AND name = ?", taskID, "config", "snapshot").Count(&configCount).Error; err != nil {
			return err
		}
		if configCount == 0 {
			return nil
		}
		if err := tx.Model(&task).Updates(map[string]any{"status": SystemTaskStatusPending, "active_key": ErrorAnalysisTaskType, "locked_by": "", "error": "", "updated_at": common.GetTimestamp()}).Error; err != nil {
			return err
		}
		if lease.TaskID == taskID {
			if err := tx.Where("type = ? AND task_id = ? AND locked_by = ? AND locked_until < ?", ErrorAnalysisTaskType, taskID, lease.LockedBy, common.GetTimestamp()).Delete(&SystemTaskLock{}).Error; err != nil {
				return err
			}
		}
		handled = true
		return nil
	})
	return handled, err
}
