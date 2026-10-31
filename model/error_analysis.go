package model

import (
	"context"
	"fmt"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/constant"
	"github.com/tidwall/gjson"
	"gorm.io/gorm"
)

const ErrorAnalysisTaskType = "error_analysis"

func SaveErrorAnalysisOption(key, value string, version int64) error {
	return DB.Transaction(func(tx *gorm.DB) error {
		var old Option
		err := lockForUpdate(tx).Where(map[string]any{"key": key}).Take(&old).Error
		if err == gorm.ErrRecordNotFound {
			if version != 0 {
				return common.NewMessage("Analysis configuration changed; reload before saving")
			}
			return tx.Create(&Option{Key: key, Value: value}).Error
		}
		if err != nil {
			return err
		}
		if gjson.Get(old.Value, "version").Int() != version {
			return common.NewMessage("Analysis configuration changed; reload before saving")
		}
		result := tx.Model(&Option{}).Where(map[string]any{"key": key, "value": old.Value}).Update("value", value)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected != 1 {
			return common.NewMessage("Analysis configuration changed; reload before saving")
		}
		return nil
	})
}

var failureSecretPattern = regexp.MustCompile(`(?i)(sk-[a-z0-9_-]+|bearer\s+\S+|[a-z0-9._%+-]+@[a-z0-9.-]+\.[a-z]{2,}|https?://\S+)`)

func SafeFailureFact(value string, limit int) string {
	value = failureSecretPattern.ReplaceAllString(value, "[redacted]")
	characters := []rune(value)
	if len(characters) > limit {
		return string(characters[:limit])
	}
	return value
}

// ErrorAnalysisFilter uses a half-open time range, so adjacent daily reports
// never count a boundary request twice. Queries deliberately avoid JSON SQL.
type ErrorAnalysisFilter struct {
	RequestID string `json:"request_id,omitempty" form:"request_id"`
	Start     int64  `json:"start" form:"start"`
	End       int64  `json:"end" form:"end"`
	Model     string `json:"model" form:"model"`
	Channel   int    `json:"channel" form:"channel"`
}

func (f *ErrorAnalysisFilter) Normalize(now time.Time) error {
	f.RequestID = strings.TrimSpace(f.RequestID)
	if f.RequestID != "" {
		if len(f.RequestID) > 64 || strings.ContainsAny(f.RequestID, "\r\n\x00") {
			return common.NewMessage("Invalid analysis time range or filters")
		}
		// An exact indexed lookup intentionally searches all retained records,
		// including earlier attempts on other channels and older requests.
		f.Start, f.End, f.Channel, f.Model = 0, 0, 0, ""
		return nil
	}
	if f.End == 0 {
		f.End = now.Unix()
	}
	if f.Start == 0 {
		f.Start = f.End - 86400
	}
	if f.Start < 0 || f.End <= f.Start || f.End-f.Start > 31*86400 || f.End > now.Unix()+60 || f.Channel < 0 || len(f.Model) > 200 {
		return common.NewMessage("Invalid analysis time range or filters")
	}
	return nil
}

type FailureDiagnostic struct {
	Version         int    `json:"version"`
	Protocol        string `json:"protocol,omitempty"`
	Method          string `json:"method,omitempty"`
	Path            string `json:"path,omitempty"`
	Node            string `json:"node,omitempty"`
	UpstreamModel   string `json:"upstream_model,omitempty"`
	ChannelType     int    `json:"channel_type,omitempty"`
	ElapsedMS       int64  `json:"elapsed_ms"`
	FirstResponseMS int64  `json:"first_response_ms,omitempty"`
	ReceivedChunks  int    `json:"received_chunks"`
	SentChunks      int    `json:"sent_chunks"`
	HasOutput       *bool  `json:"has_output,omitempty"`
	ExpectsTerminal bool   `json:"expects_terminal"`
	ResponseStatus  string `json:"response_status,omitempty"`
	EndReason       string `json:"end_reason,omitempty"`
	ErrorCode       string `json:"error_code,omitempty"`
	ErrorType       string `json:"error_type,omitempty"`
	StatusCode      int    `json:"status_code,omitempty"`
	StreamErrors    int    `json:"stream_errors"`
	RetryCount      int    `json:"retry_count"`
	FinishReason    string `json:"finish_reason,omitempty"`
}

type FailureLog struct {
	ID                int                      `json:"id"`
	RequestID         string                   `json:"request_id"`
	UpstreamRequestID string                   `json:"upstream_request_id"`
	CreatedAt         int64                    `json:"created_at"`
	Model             string                   `json:"model"`
	Channel           int                      `json:"channel"`
	IsStream          bool                     `json:"is_stream"`
	CompletionTokens  int                      `json:"completion_tokens"`
	Attempts          int                      `json:"attempts"`
	Categories        []string                 `json:"categories"`
	Diagnostic        FailureDiagnostic        `json:"diagnostic"`
	LogType           int                      `json:"log_type"`
	PromptTokens      int                      `json:"prompt_tokens"`
	Quota             int                      `json:"quota"`
	RecordedQuota     int64                    `json:"recorded_quota"`
	UsageSource       string                   `json:"usage_source,omitempty"`
	NoCharge          *bool                    `json:"no_charge,omitempty"`
	ErrorMessage      string                   `json:"error_message,omitempty"`
	ErrorTruncated    bool                     `json:"error_truncated,omitempty"`
	Evidence          *RelaySettlementEvidence `json:"evidence,omitempty"`
	Records           []FailureLog             `json:"records,omitempty"`
	RecordsTruncated  bool                     `json:"records_truncated,omitempty"`
}

type FailureCounts struct {
	Requests              int `json:"requests"`
	Affected              int `json:"affected"`
	ZeroOutput            int `json:"zero_output"`
	Interrupted           int `json:"interrupted"`
	Errors                int `json:"errors"`
	ClientCancelled       int `json:"client_cancelled"`
	UnknownOutput         int `json:"unknown_output"`
	Recovered             int `json:"recovered"`
	SettlementRisk        int `json:"settlement_risk"`
	UpstreamUsageUnbilled int `json:"upstream_usage_unbilled"`
	ChargedZeroOutput     int `json:"charged_zero_output"`
}

type FailureDistribution struct {
	Name string `json:"name"`
	FailureCounts
}

type ErrorAnalysisSnapshot struct {
	Filter ErrorAnalysisFilter `json:"filter"`
	FailureCounts
	TotalLogRows          int64                 `json:"total_log_rows"`
	ScannedLogRows        int                   `json:"scanned_log_rows"`
	Truncated             bool                  `json:"truncated"`
	LogRecordingEnabled   bool                  `json:"log_recording_enabled"`
	ErrorRecordingEnabled bool                  `json:"error_recording_enabled"`
	Models                []FailureDistribution `json:"models"`
	Channels              []FailureDistribution `json:"channels"`
	Reasons               []FailureDistribution `json:"reasons"`
	Timeline              []FailureDistribution `json:"timeline"`
	Logs                  []FailureLog          `json:"logs"`
}

func isGenerationPath(path string) bool {
	return strings.HasSuffix(path, "/chat/completions") || strings.HasSuffix(path, "/completions") || strings.HasSuffix(path, "/responses") || strings.HasSuffix(path, "/messages") || strings.Contains(path, ":generateContent") || strings.Contains(path, ":streamGenerateContent")
}

// ClassifyFailureLog includes bounded, redacted error/usage evidence. It never
// includes request bodies, account identities, credentials or arbitrary headers.
func ClassifyFailureLog(log Log) FailureLog {
	root := gjson.Parse(log.Other)
	d := FailureDiagnostic{}
	_ = common.UnmarshalJsonStr(root.Get("admin_info.failure_diagnostic").Raw, &d)
	if d.Path == "" {
		d.Path = root.Get("request_path").String()
	}
	if d.EndReason == "" {
		d.EndReason = root.Get("stream_status.end_reason").String()
	}
	if d.ResponseStatus == "" {
		d.ResponseStatus = root.Get("stream_status.response_status").String()
	}
	if d.ErrorCode == "" {
		d.ErrorCode = root.Get("error_code").String()
	}
	if d.ErrorType == "" {
		d.ErrorType = root.Get("error_type").String()
	}
	if d.StatusCode == 0 {
		d.StatusCode = int(root.Get("status_code").Int())
	}
	if d.ElapsedMS == 0 {
		d.ElapsedMS = int64(log.UseTime) * 1000
	}
	if failure := root.Get("stream_failure"); failure.Exists() {
		d.ErrorCode = failure.Get("code").String()
		d.ErrorType = failure.Get("type").String()
		if output := failure.Get("has_output"); output.Exists() {
			value := output.Bool()
			d.HasOutput = &value
		}
	}
	// Empty responses rejected before writing a body have no writer observation.
	// Their explicit error code is evidence of zero output, including JSON calls.
	if d.HasOutput == nil && d.ErrorCode == "empty_response" {
		output := false
		d.HasOutput = &output
	}
	d.ErrorCode, d.ErrorType = SafeFailureFact(d.ErrorCode, 100), SafeFailureFact(d.ErrorType, 100)
	d.UpstreamModel, d.Node = SafeFailureFact(d.UpstreamModel, 200), SafeFailureFact(d.Node, 100)
	d.Path, _, _ = strings.Cut(d.Path, "?")
	d.Path = SafeFailureFact(d.Path, 200)
	entry := FailureLog{ID: log.Id, RequestID: SafeFailureFact(log.RequestId, 64), UpstreamRequestID: SafeFailureFact(log.UpstreamRequestId, 128), CreatedAt: log.CreatedAt, Model: SafeFailureFact(log.ModelName, 200), Channel: log.ChannelId, IsStream: log.IsStream, CompletionTokens: log.CompletionTokens, Attempts: 1, Categories: []string{}, Diagnostic: d}
	entry.LogType, entry.PromptTokens, entry.Quota = log.Type, log.PromptTokens, log.Quota
	if log.Type == LogTypeConsume {
		entry.RecordedQuota = int64(log.Quota)
	}
	entry.UsageSource = SafeFailureFact(root.Get("stream_failure.usage_source").String(), 80)
	if entry.UsageSource == "" && root.Get("admin_info.local_count_tokens").Bool() {
		entry.UsageSource = "local_estimate"
	}
	if entry.UsageSource == "" {
		entry.UsageSource = SafeFailureFact(root.Get("admin_info.usage_billing_path").String(), 80)
	}
	if raw := root.Get("admin_info.settlement_evidence"); raw.Exists() {
		_ = common.UnmarshalJsonStr(raw.Raw, &entry.Evidence)
		if entry.UsageSource == "" && entry.Evidence != nil {
			entry.UsageSource = entry.Evidence.UsageSource
		}
	}
	if noCharge := root.Get("stream_failure.no_charge"); noCharge.Exists() {
		value := noCharge.Bool()
		entry.NoCharge = &value
	}
	if log.Type == LogTypeError {
		entry.ErrorMessage, entry.ErrorTruncated = SafeFailureDetail(log.Content, 32*1024)
	}
	if message := root.Get("stream_failure.message").String(); message != "" {
		entry.ErrorMessage, entry.ErrorTruncated = SafeFailureDetail(message, 32*1024)
	}
	cancelled := d.EndReason == "client_gone" || d.ResponseStatus == "cancelled"
	if cancelled {
		entry.Categories = append(entry.Categories, "client_cancelled")
	}
	if !cancelled && (log.Type == LogTypeError || d.ResponseStatus == "failed" || root.Get("stream_failure").Exists()) {
		entry.Categories = append(entry.Categories, "error")
	}
	interrupted := false
	if log.IsStream && !cancelled {
		switch d.EndReason {
		case "timeout", "scanner_error", "panic", "ping_fail":
			interrupted = true
		case "eof":
			// Adaptors turn a missing terminal event into bad_response before
			// persisting the log. Preserve that transport failure even though
			// the response outcome has already been changed to failed.
			interrupted = d.ExpectsTerminal && (d.ResponseStatus == "" || d.ResponseStatus == "failed" && d.ErrorCode == "bad_response")
		case "handler_stop":
			interrupted = d.ExpectsTerminal && d.ResponseStatus == ""
		}
	}
	if interrupted {
		entry.Categories = append(entry.Categories, "interrupted")
	}
	if !cancelled && isGenerationPath(d.Path) {
		if d.HasOutput != nil && !*d.HasOutput {
			entry.Categories = append(entry.Categories, "zero_output")
		} else if d.HasOutput == nil && log.Type == LogTypeConsume && log.CompletionTokens == 0 {
			entry.Categories = append(entry.Categories, "unknown_output")
		}
	}
	if log.Type == LogTypeConsume && log.Quota > 0 && d.HasOutput != nil && !*d.HasOutput {
		entry.Categories = append(entry.Categories, "charged_zero_output")
	}
	if e := entry.Evidence; e != nil {
		if e.Local != nil && (e.Local.State == "failed" || e.Local.Error != "") {
			entry.Categories = append(entry.Categories, "settlement_risk")
		}
		upstreamUsage := false
		for _, attempt := range e.Upstream {
			upstreamUsage = upstreamUsage || HasReportedUsage(attempt.Usage)
		}
		if upstreamUsage && (log.Type == LogTypeError || log.Type == LogTypeConsume && log.Quota == 0) {
			entry.Categories = append(entry.Categories, "upstream_usage_unbilled")
		}
		if log.Type == LogTypeConsume && log.Quota > 0 {
			down := e.Downstream
			if down != nil && (down.WriteFailed || down.Failed || down.StatusCode >= 400 || !down.CaptureIncomplete && len(down.Usage) == 0 && !down.Completed) || d.ResponseStatus == "failed" || root.Get("stream_failure").Exists() {
				if !slices.Contains(entry.Categories, "settlement_risk") {
					entry.Categories = append(entry.Categories, "settlement_risk")
				}
			}
		}
	}
	if log.Type == LogTypeConsume && log.Quota > 0 && (d.ResponseStatus == "failed" || root.Get("stream_failure").Exists() || interrupted) && !slices.Contains(entry.Categories, "settlement_risk") {
		entry.Categories = append(entry.Categories, "settlement_risk")
	}
	return entry
}

func (c *FailureCounts) add(entry FailureLog) {
	c.Requests++
	affected := false
	for _, kind := range entry.Categories {
		switch kind {
		case "zero_output":
			c.ZeroOutput++
			affected = true
		case "interrupted":
			c.Interrupted++
			affected = true
		case "error":
			c.Errors++
			affected = true
		case "client_cancelled":
			c.ClientCancelled++
		case "unknown_output":
			c.UnknownOutput++
		case "recovered":
			c.Recovered++
		case "settlement_risk":
			c.SettlementRisk++
			affected = true
		case "upstream_usage_unbilled":
			c.UpstreamUsageUnbilled++
			affected = true
		case "charged_zero_output":
			c.ChargedZeroOutput++
			affected = true
		}
	}
	if affected {
		c.Affected++
	}
}

// LoadErrorAnalysis scans at most 50k recent rows with an explicit coverage
// warning. Request IDs deduplicate relay retries; legacy rows remain distinct.
// LOG_DB can be separate (including ClickHouse); no cross-database joins occur.
func LoadErrorAnalysis(ctx context.Context, filter ErrorAnalysisFilter) (*ErrorAnalysisSnapshot, error) {
	query := LOG_DB.WithContext(ctx).Model(&Log{}).Where("type IN ?", []int{LogTypeConsume, LogTypeError})
	if filter.RequestID != "" {
		query = query.Where("request_id = ?", filter.RequestID)
	} else {
		query = query.Where("created_at >= ? AND created_at < ?", filter.Start, filter.End)
	}
	if filter.Model != "" {
		query = query.Where("model_name = ?", filter.Model)
	}
	if filter.Channel > 0 {
		query = query.Where("channel_id = ?", filter.Channel)
	}
	result := &ErrorAnalysisSnapshot{Filter: filter, LogRecordingEnabled: common.LogConsumeEnabled, ErrorRecordingEnabled: constant.ErrorLogEnabled, Logs: []FailureLog{}}
	if err := query.Count(&result.TotalLogRows).Error; err != nil {
		return nil, err
	}
	order := "created_at desc, id desc"
	if common.UsingLogDatabase(common.DatabaseTypeClickHouse) {
		order = "created_at desc, request_id desc"
	}
	rows, err := query.Select("id", "created_at", "type", "model_name", "channel_id", "request_id", "upstream_request_id", "prompt_tokens", "completion_tokens", "quota", "content", "use_time", "is_stream", "other").Order(order).Limit(50000).Rows()
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	entries := make([]FailureLog, 0, min(result.TotalLogRows, 50000))
	indices := map[string]int{}
	consumed := map[string]bool{}
	errored := map[string]bool{}
	bytesScanned := 0
	for rows.Next() {
		var log Log
		if err := LOG_DB.ScanRows(rows, &log); err != nil {
			return nil, err
		}
		bytesScanned += len(log.Other) + len(log.Content)
		if bytesScanned > 64*1024*1024 {
			break
		}
		result.ScannedLogRows++
		// MySQL's default collation can equate IDs differing only by case.
		// Keep the indexed candidate query, then enforce an exact identifier
		// match consistently across all log database engines.
		if filter.RequestID != "" && log.RequestId != filter.RequestID {
			continue
		}
		key := log.RequestId
		if key == "" {
			key = fmt.Sprintf("legacy:%d:%d", log.CreatedAt, log.Id)
		}
		errored[key] = errored[key] || log.Type == LogTypeError
		classified := ClassifyFailureLog(log)
		if index, exists := indices[key]; exists {
			entry := &entries[index]
			attempts, recordedQuota := entry.Attempts+1, entry.RecordedQuota+classified.RecordedQuota
			records := entry.Records
			if len(records) < 50 {
				records = append(records, classified)
			}
			categories := entry.Categories
			// Consumption is the final request outcome even when ClickHouse
			// returns rows with equal timestamps in an arbitrary order.
			if log.Type == LogTypeConsume && !consumed[key] {
				*entry = classified
			}
			entry.Attempts, entry.RecordedQuota, entry.Records = attempts, recordedQuota, records
			entry.RecordsTruncated = attempts > len(records)
			// A successful retry must not erase financial risk on failed attempts.
			for _, kind := range append(slices.Clone(categories), classified.Categories...) {
				if (kind == "settlement_risk" || kind == "upstream_usage_unbilled" || kind == "charged_zero_output") && !slices.Contains(entry.Categories, kind) {
					entry.Categories = append(entry.Categories, kind)
				}
			}
			consumed[key] = consumed[key] || log.Type == LogTypeConsume
			continue
		}
		indices[key] = len(entries)
		consumed[key] = log.Type == LogTypeConsume
		classified.Records = []FailureLog{classified}
		entries = append(entries, classified)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	result.Truncated = int64(result.ScannedLogRows) < result.TotalLogRows
	for key, index := range indices {
		entry := &entries[index]
		if consumed[key] && errored[key] && !slices.Contains(entry.Categories, "error") && !slices.Contains(entry.Categories, "interrupted") && !slices.Contains(entry.Categories, "zero_output") && !slices.Contains(entry.Categories, "client_cancelled") {
			entry.Categories = append(entry.Categories, "recovered")
		}
	}
	dimensions := []map[string]*FailureDistribution{{}, {}, {}, {}}
	for hour := filter.Start - filter.Start%3600; hour < filter.End; hour += 3600 {
		key := strconv.FormatInt(hour, 10)
		dimensions[3][key] = &FailureDistribution{Name: key}
	}
	for _, entry := range entries {
		result.FailureCounts.add(entry)
		reason := entry.Diagnostic.ErrorCode
		if reason == "" {
			reason = entry.Diagnostic.EndReason
		}
		if reason == "" {
			reason = strconv.Itoa(entry.Diagnostic.StatusCode)
		}
		keys := []string{entry.Model, strconv.Itoa(entry.Channel), reason, strconv.FormatInt(entry.CreatedAt-entry.CreatedAt%3600, 10)}
		for i, key := range keys {
			if dimensions[i][key] == nil {
				dimensions[i][key] = &FailureDistribution{Name: key}
			}
			dimensions[i][key].add(entry)
		}
		if len(entry.Categories) > 0 || filter.RequestID != "" {
			result.Logs = append(result.Logs, entry)
		}
	}
	outputs := []*[]FailureDistribution{&result.Models, &result.Channels, &result.Reasons, &result.Timeline}
	for i, values := range dimensions {
		*outputs[i] = []FailureDistribution{}
		for _, value := range values {
			*outputs[i] = append(*outputs[i], *value)
		}
		slices.SortFunc(*outputs[i], func(a, b FailureDistribution) int {
			if a.Affected != b.Affected {
				return b.Affected - a.Affected
			}
			return strings.Compare(a.Name, b.Name)
		})
		if i != 3 && len(*outputs[i]) > 100 {
			*outputs[i] = (*outputs[i])[:100]
		}
	}
	slices.SortFunc(result.Timeline, func(a, b FailureDistribution) int { return strings.Compare(a.Name, b.Name) })
	return result, nil
}
