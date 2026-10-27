package perfmetrics

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/setting/config"
	"github.com/QuantumNous/new-api/setting/perf_metrics_setting"
	"github.com/alicebob/miniredis/v2"
	"github.com/go-redis/redis/v8"
	"github.com/samber/lo"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"gorm.io/gorm"
)

func TestClassifyRelayOutcome(t *testing.T) {
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	expired, cancelDeadline := context.WithDeadline(context.Background(), time.Unix(1, 0))
	defer cancelDeadline()
	for _, tc := range []struct {
		name string
		ctx  context.Context
		err  *types.NewAPIError
		want Outcome
	}{
		{"success", context.Background(), nil, OutcomeSuccess},
		{"upstream bad request", context.Background(), types.InitOpenAIError("unknown", 400), OutcomeIgnored},
		{"upstream credentials returned as 400", context.Background(), types.InitOpenAIError("invalid_api_key", 400), OutcomeFailure},
		{"wrapped credentials keep their cause", context.Background(), types.NewErrorWithStatusCode(types.InitOpenAIError("invalid_api_key", 400), types.ErrorCodeInvalidRequest, 400), OutcomeFailure},
		{"upstream context limit returned as 500", context.Background(), types.InitOpenAIError("context_length_exceeded", 500), OutcomeIgnored},
		{"local rate limit", context.Background(), types.NewErrorWithStatusCode(errors.New("limited"), types.ErrorCodeInvalidRequest, 429), OutcomeIgnored},
		{"upstream rate limit", context.Background(), types.InitOpenAIError("rate_limit_exceeded", 429), OutcomeFailure},
		{"local quota", context.Background(), types.NewError(errors.New("quota"), types.ErrorCodeInsufficientUserQuota), OutcomeIgnored},
		{"local violation fee", context.Background(), types.NewError(errors.New("csam"), types.ErrorCodeViolationFeeGrokCSAM), OutcomeIgnored},
		{"upstream quota", context.Background(), types.InitOpenAIError("insufficient_quota", 429), OutcomeFailure},
		{"upstream gateway quota", context.Background(), types.InitOpenAIError(types.ErrorCodeInsufficientUserQuota, 403), OutcomeFailure},
		{"unavailable channel", context.Background(), types.NewErrorWithStatusCode(errors.New("disabled"), types.ErrorCodeGetChannelFailed, 403), OutcomeFailure},
		{"empty upstream response", context.Background(), types.NewError(errors.New("empty"), types.ErrorCodeEmptyResponse), OutcomeFailure},
		{"network failure", context.Background(), types.NewOpenAIError(errors.New("connection refused"), types.ErrorCodeDoRequestFailed, 500), OutcomeFailure},
		{"client cancellation", canceled, types.NewOpenAIError(errors.New("context canceled"), types.ErrorCodeDoRequestFailed, 500), OutcomeIgnored},
		{"whole request deadline", expired, types.NewOpenAIError(context.DeadlineExceeded, types.ErrorCodeDoRequestFailed, 504), OutcomeIgnored},
		{"upstream deadline", context.Background(), types.NewOpenAIError(context.DeadlineExceeded, types.ErrorCodeDoRequestFailed, 504), OutcomeFailure},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, ClassifyRelayOutcome(tc.ctx, &relaycommon.RelayInfo{}, tc.err))
		})
	}
	assert.Equal(t, OutcomeIgnored, ClassifyRelayOutcome(context.Background(), &relaycommon.RelayInfo{PerformanceBusinessRejection: true}, nil))
}

func TestStreamOutcomeClassification(t *testing.T) {
	for _, tc := range []struct {
		name string
		mark func(*relaycommon.StreamStatus)
		end  relaycommon.StreamEndReason
		want Outcome
	}{
		{"completed", (*relaycommon.StreamStatus).MarkCompleted, relaycommon.StreamEndReasonEOF, OutcomeSuccess},
		{"business rejection", func(s *relaycommon.StreamStatus) { s.MarkFailed("context_length_exceeded", "", 0) }, relaycommon.StreamEndReasonEOF, OutcomeIgnored},
		{"service error", func(s *relaycommon.StreamStatus) { s.MarkFailed("server_error", "", 0) }, relaycommon.StreamEndReasonEOF, OutcomeFailure},
		{"error after completion", func(s *relaycommon.StreamStatus) { s.MarkCompleted(); s.MarkFailed("", "server_error", 0) }, relaycommon.StreamEndReasonEOF, OutcomeFailure},
		{"output limit", func(s *relaycommon.StreamStatus) { s.MarkIncomplete("max_output_tokens") }, relaycommon.StreamEndReasonEOF, OutcomeSuccess},
		{"content filter", func(s *relaycommon.StreamStatus) { s.MarkIncomplete("content_filter") }, relaycommon.StreamEndReasonEOF, OutcomeIgnored},
		{"client cancel", (*relaycommon.StreamStatus).MarkCancelled, relaycommon.StreamEndReasonEOF, OutcomeIgnored},
		{"client gone", nil, relaycommon.StreamEndReasonClientGone, OutcomeIgnored},
		{"timeout", nil, relaycommon.StreamEndReasonTimeout, OutcomeFailure},
		{"missing terminal", nil, relaycommon.StreamEndReasonEOF, OutcomeFailure},
		{"done marker without terminal", nil, relaycommon.StreamEndReasonDone, OutcomeSuccess},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream := relaycommon.NewStreamStatus()
			stream.RequireTerminal()
			if tc.mark != nil {
				tc.mark(stream)
			}
			stream.SetEndReason(tc.end, nil)
			assert.Equal(t, tc.want, ClassifyRelayOutcome(context.Background(), &relaycommon.RelayInfo{StreamStatus: stream}, nil))
		})
	}
}

func TestClientCancellationDuringUpstreamRead(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stream := relaycommon.NewStreamStatus()
	stream.RequireTerminal()
	stream.SetEndReason(relaycommon.StreamEndReasonScannerErr, errors.New("reader closed"))
	assert.Equal(t, OutcomeIgnored, ClassifyRelayOutcome(ctx, &relaycommon.RelayInfo{StreamStatus: stream}, nil))

	deadline := relaycommon.NewStreamStatus()
	deadline.SetEndReason(relaycommon.StreamEndReasonClientGone, context.DeadlineExceeded)
	assert.Equal(t, OutcomeFailure, ClassifyRelayOutcome(context.Background(), &relaycommon.RelayInfo{StreamStatus: deadline}, nil))

	expired, stop := context.WithDeadline(context.Background(), time.Unix(1, 0))
	defer stop()
	for _, tc := range []struct {
		name string
		ctx  context.Context
		end  relaycommon.StreamEndReason
		want Outcome
	}{
		{"delivered completion followed by cancellation", ctx, relaycommon.StreamEndReasonDone, OutcomeSuccess},
		{"parsed completion with interrupted delivery", ctx, relaycommon.StreamEndReasonClientGone, OutcomeIgnored},
		{"whole request deadline remains excluded", expired, relaycommon.StreamEndReasonDone, OutcomeIgnored},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stream := relaycommon.NewStreamStatus()
			stream.MarkCompleted()
			stream.SetEndReason(tc.end, nil)
			assert.Equal(t, tc.want, ClassifyRelayOutcome(tc.ctx, &relaycommon.RelayInfo{IsStream: true, StreamStatus: stream}, nil))
		})
	}
}

func TestPerformanceWindowIncludesCurrentHour(t *testing.T) {
	now := time.Date(2026, 9, 14, 12, 37, 0, 0, time.UTC)
	start, end := queryWindow(now, 24)
	assert.Equal(t, time.Date(2026, 9, 13, 13, 0, 0, 0, time.UTC).Unix(), start)
	assert.Equal(t, now.Unix(), end)
}

func TestHourlySuccessSeriesWeightsSmallerBuckets(t *testing.T) {
	points := recentSuccessSeries(map[int64]counters{
		3600: {requestCount: 100, successCount: 100},
		3900: {requestCount: 1},
		7200: {requestCount: 1, successCount: 1},
	})
	assert.Equal(t, []SuccessRatePoint{{Ts: 3600, SuccessRate: 99.01}, {Ts: 7200, SuccessRate: 100}}, points)
}

func TestRecentHistoryRejectsOversizedModelLists(t *testing.T) {
	_, err := QuerySummaryAll(24, nil, make([]string, 51)...)
	require.EqualError(t, err, "Invalid parameters")
}

func TestRetentionCutoffPreservesInvalidSavedValues(t *testing.T) {
	previous := perf_metrics_setting.GetSetting().RetentionDays
	t.Cleanup(func() {
		config.UpdateConfigFromMap(config.GlobalConfig.Get("perf_metrics_setting"), map[string]string{"retention_days": strconv.Itoa(previous)})
	})
	now := time.Date(2026, 10, 27, 12, 30, 0, 0, time.UTC)
	for _, tc := range []struct {
		name string
		days int
		want int64
	}{
		{"unlimited", 0, 0},
		{"legacy negative", -1, 0},
		{"seven days", 7, time.Date(2026, 10, 20, 12, 30, 0, 0, time.UTC).Unix()},
		{"overflowing legacy value", math.MaxInt, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config.UpdateConfigFromMap(config.GlobalConfig.Get("perf_metrics_setting"), map[string]string{"retention_days": strconv.Itoa(tc.days)})
			assert.Equal(t, tc.want, perf_metrics_setting.GetRetentionCutoff(now))
			assert.Equal(t, tc.days, perf_metrics_setting.GetSetting().RetentionDays)
		})
	}
}

func performanceDatabaseFixture(t *testing.T, dialect, dsn string) *gorm.DB {
	t.Helper()
	WaitForPendingWrites()
	previousCollector := collector
	collector = &collectorProcess{id: "collector:test", pending: make(map[string]model.PerfMetricEvent)}
	previousDB, previousLog := model.DB, model.LOG_DB
	previousPath, previousMaster, previousRedis := common.SQLitePath, common.IsMasterNode, common.RedisEnabled
	previousMainType, previousLogType := common.MainDatabaseType(), common.LogDatabaseType()
	previousSetting := perf_metrics_setting.GetSetting()
	config.UpdateConfigFromMap(config.GlobalConfig.Get("perf_metrics_setting"), map[string]string{"enabled": "true", "bucket_time": "hour", "retention_days": "0"})
	common.SQLitePath, common.IsMasterNode, common.RedisEnabled = filepath.Join(t.TempDir(), "performance.db"), false, false
	if dialect == "sqlite" {
		dsn = "local"
	}
	t.Setenv("SQL_DSN", dsn)
	t.Setenv("LOG_SQL_DSN", "")
	require.NoError(t, model.InitDB())
	db := model.DB
	model.LOG_DB = db
	sqlDB, err := db.DB()
	require.NoError(t, err)
	sqlDB.SetMaxOpenConns(1)
	require.NoError(t, db.Migrator().DropTable(&model.PerfMetric{}, &model.PerfMetricEvent{}, &model.PerfMetricCollector{}, &model.Task{}, &model.Option{}))
	require.NoError(t, db.AutoMigrate(&model.PerfMetric{}, &model.PerfMetricEvent{}, &model.PerfMetricCollector{}, &model.Task{}, &model.Option{}))
	t.Cleanup(func() {
		WaitForPendingWrites()
		require.NoError(t, db.Migrator().DropTable(&model.PerfMetric{}, &model.PerfMetricEvent{}, &model.PerfMetricCollector{}, &model.Task{}, &model.Option{}))
		require.NoError(t, sqlDB.Close())
		model.DB, model.LOG_DB = previousDB, previousLog
		common.SQLitePath, common.IsMasterNode, common.RedisEnabled = previousPath, previousMaster, previousRedis
		common.SetDatabaseTypes(previousMainType, previousLogType)
		collector = previousCollector
		config.UpdateConfigFromMap(config.GlobalConfig.Get("perf_metrics_setting"), map[string]string{"enabled": strconv.FormatBool(previousSetting.Enabled), "bucket_time": previousSetting.BucketTime, "retention_days": strconv.Itoa(previousSetting.RetentionDays)})
	})
	return db
}

func TestRecordTaskResultSamplesTerminalTasks(t *testing.T) {
	performanceDatabaseFixture(t, "sqlite", "")
	now := time.Now().Unix()
	tokens, zero := int64(500), int64(0)
	for i, tc := range []struct {
		name                       string
		status                     model.TaskStatus
		safe                       *int64
		completion                 int
		duration                   bool
		wantPresent, wantAvailable bool
		wantTPS                    float64
	}{
		{"total differs from completion", model.TaskStatusSuccess, &tokens, 500, true, true, true, 5},
		{"only total is not output", model.TaskStatusSuccess, nil, 0, true, true, false, 0},
		{"unverified completion counter", model.TaskStatusSuccess, nil, 17, true, true, false, 0},
		{"known zero output", model.TaskStatusSuccess, &zero, 0, true, true, true, 0},
		{"missing generation time", model.TaskStatusSuccess, &tokens, 500, false, true, false, 0},
		{"failed task", model.TaskStatusFailure, &tokens, 500, true, true, false, 0},
		{"nonterminal task", model.TaskStatusInProgress, &tokens, 500, true, false, false, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			task := model.Task{ID: int64(i + 1), Status: tc.status, Group: "a", SubmitTime: now - 120, FinishTime: now, Properties: model.Properties{OriginModelName: tc.name}}
			if tc.duration {
				task.StartTime = now - 100
			}
			result := relaycommon.TaskInfo{TotalTokens: 5000, CompletionTokens: tc.completion, PerformanceOutputTokens: tc.safe}
			RecordTaskResult(&task, &result)
			require.NoError(t, Flush(context.Background()))
			// Replayed poll replies use the same primary-key event identity.
			RecordTaskResult(&task, &result)
			require.NoError(t, Flush(context.Background()))
			summary, err := QuerySummaryAll(24, nil)
			require.NoError(t, err)
			byName := lo.KeyBy(summary.Models, func(value ModelSummary) string { return value.ModelName })
			row, present := byName[tc.name]
			assert.Equal(t, tc.wantPresent, present)
			if !tc.wantPresent {
				return
			}
			assert.Equal(t, int64(1), row.RequestCount)
			assert.Equal(t, tc.wantAvailable, row.OutputMetricsAvailable)
			assert.Equal(t, tc.wantTPS, row.AvgTps)
		})
	}
}

func TestPerformanceAggregationAndFlush(t *testing.T) {
	WaitForPendingWrites()
	for _, dialect := range []struct{ name, env string }{
		{"sqlite", ""}, {"mysql", "TEST_PERF_MYSQL_DSN"}, {"postgres", "TEST_PERF_POSTGRES_DSN"},
	} {
		t.Run(dialect.name, func(t *testing.T) {
			dsn := ""
			if dialect.env != "" {
				dsn = os.Getenv(dialect.env)
				if dsn == "" {
					t.Skip("isolated test database DSN is not configured")
				}
			}
			db := performanceDatabaseFixture(t, dialect.name, dsn)
			versionQuery := "SELECT version()"
			if dialect.name == "sqlite" {
				versionQuery = "SELECT sqlite_version()"
			}
			var version string
			require.NoError(t, db.Raw(versionQuery).Scan(&version).Error)
			t.Logf("database version: %s", version)

			now := time.Now()
			start, _ := queryWindow(now, 24)
			hour := now.Unix() - now.Unix()%3600 - 3600
			// Historical counters remain usable without reclassification or migration.
			for _, row := range []model.PerfMetric{
				{ModelName: "test-model", Group: "a", BucketTs: hour, RequestCount: 100, SuccessCount: 100, TotalLatencyMs: 100000, TtftCount: 100, TtftSumMs: 10000, OutputTokens: 200, GenerationMs: 40000},
				{ModelName: "test-model", Group: "inactive", BucketTs: hour, RequestCount: 100},
				{ModelName: "test-model", Group: "a", BucketTs: start - 3600, RequestCount: 100},
			} {
				require.NoError(t, model.UpsertPerfMetric(&row))
			}
			groups := []string{"a", "b"}

			RecordRelayResult(context.Background(), &relaycommon.RelayInfo{OriginModelName: "test-model", UsingGroup: "b", StartTime: now}, types.InitOpenAIError("unknown", 400))
			businessRejected, err := QuerySummaryAll(24, groups)
			require.NoError(t, err)
			require.NotNil(t, businessRejected.Summary)
			assert.Equal(t, 100.0, businessRejected.Summary.SuccessRate)

			Record(Sample{EventID: "history:failure", Model: "test-model", Group: "b", OccurredAt: hour, LatencyMs: 2000})
			require.NoError(t, Flush(context.Background()))
			before, err := Query(QueryParams{Model: "test-model", Hours: 24, AllowedGroups: groups})
			require.NoError(t, err)
			require.NotNil(t, before.Summary)
			assert.Equal(t, Summary{SuccessRate: 99.01, AvgLatencyMs: 1009, AvgTps: 5, OutputMetricsAvailable: true}, *before.Summary)
			assert.Equal(t, start, before.WindowStart)
			require.Len(t, before.Series, 1)
			assert.Equal(t, hour, before.Series[0].Ts)
			assert.InDelta(t, 99.01, before.Series[0].SuccessRate, 0.01)
			require.Len(t, before.Groups, 2)

			summary, err := QuerySummaryAll(24, groups)
			require.NoError(t, err)
			assert.Equal(t, before.Summary, summary.Summary)
			require.Len(t, summary.Models, 1)
			assert.Equal(t, 99.01, summary.Models[0].SuccessRate)
			assert.Equal(t, 99.01, summary.Models[0].RecentSuccessSeries[0].SuccessRate)
			encoded, err := common.Marshal(summary)
			require.NoError(t, err)
			assert.NotContains(t, string(encoded), "request_count")
			assert.NotContains(t, string(encoded), "success_count")

			require.NoError(t, Flush(context.Background()))
			require.NoError(t, Flush(context.Background()))
			after, err := Query(QueryParams{Model: "test-model", Hours: 24, AllowedGroups: groups})
			require.NoError(t, err)
			assert.Equal(t, before.Summary, after.Summary)
			assert.Equal(t, before.Series, after.Series)

			RecordRelayResult(context.Background(), &relaycommon.RelayInfo{OriginModelName: "test-model", UsingGroup: "a", StartTime: now}, types.InitOpenAIError("context_length_exceeded", 400))
			after, err = Query(QueryParams{Model: "test-model", Hours: 24, AllowedGroups: groups})
			require.NoError(t, err)
			assert.Equal(t, before.Summary, after.Summary)
			onlyA, err := Query(QueryParams{Model: "test-model", Group: "a", Hours: 24, AllowedGroups: groups})
			require.NoError(t, err)
			assert.Equal(t, 100.0, onlyA.Summary.SuccessRate)
			empty, err := Query(QueryParams{Model: "missing", Hours: 24, AllowedGroups: groups})
			require.NoError(t, err)
			assert.Nil(t, empty.Summary)
			assert.Empty(t, empty.Series)

			require.NoError(t, model.UpsertPerfMetric(&model.PerfMetric{ModelName: "second-model", Group: "a", BucketTs: hour, RequestCount: 1}))
			combined, err := QuerySummaryAll(24, groups)
			require.NoError(t, err)
			assert.Equal(t, 98.04, combined.Summary.SuccessRate)
			assert.Equal(t, 99.01, combined.Models[0].SuccessRate)

			t.Run("recent models are summarized over their status sample hours", func(t *testing.T) {
				for i := range 26 {
					require.NoError(t, model.UpsertPerfMetric(&model.PerfMetric{
						ModelName: "history-model", Group: "a", BucketTs: hour - int64(i+1)*6*3600,
						RequestCount: 1, SuccessCount: 1,
					}))
				}
				for _, row := range []model.PerfMetric{
					{ModelName: "history-model", Group: "a", BucketTs: hour + 60, RequestCount: 100, SuccessCount: 100},
					{ModelName: "history-model", Group: "inactive", BucketTs: hour + 120, RequestCount: 100},
					{ModelName: "history-model", Group: "a", BucketTs: hour + 2*3600, RequestCount: 1},
					{ModelName: "history-idle", Group: "a", BucketTs: hour - 3*24*3600, RequestCount: 1},
					{ModelName: "history-hidden", Group: "inactive", BucketTs: hour - 3*24*3600, RequestCount: 1},
				} {
					require.NoError(t, model.UpsertPerfMetric(&row))
				}
				require.NoError(t, db.Create(&model.PerfMetric{ModelName: "history-model", Group: "a", BucketTs: hour - 3600}).Error)
				Record(Sample{EventID: "history:model-failure", Model: "history-model", Group: "b", OccurredAt: hour + 120})
				Record(Sample{EventID: "history:recent-success", Model: "history-hot", Group: "a", OccurredAt: hour, Success: true})
				Record(Sample{EventID: "history:hidden-failure", Model: "history-hidden", Group: "inactive", OccurredAt: hour})
				require.NoError(t, Flush(context.Background()))

				window, err := QuerySummaryAll(24, groups)
				require.NoError(t, err)
				history, err := QuerySummaryAll(24, groups, "history-model", "history-idle", "history-hidden", "history-hot", "missing")
				require.NoError(t, err)
				assert.Equal(t, window.WindowStart, history.WindowStart)
				summaries := lo.KeyBy(history.Models, func(m ModelSummary) string { return m.ModelName })
				assert.Equal(t, int64(124), summaries["history-model"].RequestCount)
				assert.Equal(t, 99.19, summaries["history-model"].SuccessRate)
				assert.Equal(t, int64(1), summaries["history-idle"].RequestCount)
				assert.Equal(t, 0.0, summaries["history-idle"].SuccessRate)
				assert.NotContains(t, summaries, "history-hidden")
				points := history.RecentSuccessSeries["history-model"]
				require.Len(t, points, 24)
				expected := make([]SuccessRatePoint, 0, 24)
				for i := 23; i > 0; i-- {
					expected = append(expected, SuccessRatePoint{Ts: hour - int64(i)*6*3600, SuccessRate: 100})
				}
				expected = append(expected, SuccessRatePoint{Ts: hour, SuccessRate: 99.01})
				assert.Equal(t, expected, points)
				assert.Equal(t, []SuccessRatePoint{{Ts: hour - 3*24*3600, SuccessRate: 0}}, history.RecentSuccessSeries["history-idle"])
				assert.Equal(t, []SuccessRatePoint{{Ts: hour, SuccessRate: 100}}, history.RecentSuccessSeries["history-hot"])
				assert.Empty(t, history.RecentSuccessSeries["history-hidden"])
				assert.Empty(t, history.RecentSuccessSeries["missing"])

				denied, err := QuerySummaryAll(24, []string{}, "history-model", "history-hidden")
				require.NoError(t, err)
				assert.Empty(t, denied.RecentSuccessSeries)
				require.NoError(t, Flush(context.Background()))
				persisted, err := QuerySummaryAll(24, groups, "history-model", "history-hot")
				require.NoError(t, err)
				assert.Equal(t, points, persisted.RecentSuccessSeries["history-model"])
				assert.Equal(t, history.RecentSuccessSeries["history-hot"], persisted.RecentSuccessSeries["history-hot"])
			})

			t.Run("recent history stops at the 7 day boundary even with fewer than 24 samples", func(t *testing.T) {
				historyStart, _ := queryWindow(now, 7*24)
				for _, row := range []model.PerfMetric{
					{ModelName: "history-boundary", Group: "a", BucketTs: historyStart, RequestCount: 1, SuccessCount: 1},
					{ModelName: "history-boundary", Group: "a", BucketTs: historyStart - 3600, RequestCount: 1},
					{ModelName: "history-expired", Group: "a", BucketTs: historyStart - 3600, RequestCount: 1},
				} {
					require.NoError(t, model.UpsertPerfMetric(&row))
				}
				Record(Sample{EventID: "history:expired", Model: "history-hot-expired", Group: "a", OccurredAt: historyStart - 3600, Success: true})
				Record(Sample{EventID: "history:boundary", Model: "history-hot-boundary", Group: "a", OccurredAt: historyStart, Success: true})
				require.NoError(t, Flush(context.Background()))

				history, err := QuerySummaryAll(24, groups, "history-boundary", "history-expired", "history-hot-expired", "history-hot-boundary")
				require.NoError(t, err)
				assert.Equal(t, []SuccessRatePoint{{Ts: historyStart, SuccessRate: 100}}, history.RecentSuccessSeries["history-boundary"])
				assert.Equal(t, []SuccessRatePoint{{Ts: historyStart, SuccessRate: 100}}, history.RecentSuccessSeries["history-hot-boundary"])
				assert.Empty(t, history.RecentSuccessSeries["history-expired"])
				assert.Empty(t, history.RecentSuccessSeries["history-hot-expired"])
			})
		})
	}
}

func TestCurrentBucketDurabilityAndRetry(t *testing.T) {
	for _, dialect := range []struct{ name, env string }{{"sqlite", ""}, {"mysql", "TEST_PERF_MYSQL_DSN"}, {"postgres", "TEST_PERF_POSTGRES_DSN"}} {
		t.Run(dialect.name, func(t *testing.T) {
			dsn := ""
			if dialect.env != "" {
				dsn = os.Getenv(dialect.env)
				if dsn == "" {
					t.Skip("isolated test database DSN is not configured")
				}
			}
			db := performanceDatabaseFixture(t, dialect.name, dsn)
			now := time.Now().Unix()
			sample := Sample{EventID: "relay:current", OccurredAt: now, Model: "current-model", Group: "retired", Success: true, LatencyMs: 1000}
			Record(sample)
			before, err := Query(QueryParams{Model: "current-model", Hours: 24})
			require.NoError(t, err)
			assert.Nil(t, before.Summary, "queries never add unprojected local or Redis counters")
			health, err := CollectionStatus(context.Background(), now-86400)
			require.NoError(t, err)
			assert.Equal(t, int64(1), health.PendingEvents)
			require.NotNil(t, health.LastSampleAt)
			assert.Equal(t, now, *health.LastSampleAt)
			// A new read-node process has no writer memory, yet can project the accepted sample.
			WaitForPendingWrites()
			collector = &collectorProcess{id: "collector:reader", pending: make(map[string]model.PerfMetricEvent)}
			require.NoError(t, Flush(context.Background()))
			after, err := Query(QueryParams{Model: "current-model", Hours: 24})
			require.NoError(t, err)
			require.NotNil(t, after.Summary)
			assert.Equal(t, 100.0, after.Summary.SuccessRate)
			all, err := QuerySummaryAll(24, nil)
			require.NoError(t, err)
			require.Len(t, all.Models, 1)
			assert.Equal(t, int64(1), all.Models[0].RequestCount)

			forced := errors.New("metric event database write rejected")
			const callback = "test:perf-accept-reject"
			require.NoError(t, db.Callback().Create().Before("gorm:create").Register(callback, func(tx *gorm.DB) {
				if tx.Statement.Table == "perf_metric_events" {
					tx.AddError(forced)
				}
			}))
			sample.EventID = "relay:retry"
			Record(sample)
			health, err = CollectionStatus(context.Background(), now-86400)
			require.NoError(t, err)
			assert.Equal(t, "partial", health.Availability)
			assert.Equal(t, int64(1), health.UnpersistedEvents)
			db.Callback().Create().Remove(callback)
			config.UpdateConfigFromMap(config.GlobalConfig.Get("perf_metrics_setting"), map[string]string{"enabled": "false"})
			require.NoError(t, Flush(context.Background()), "disabling new samples does not abandon an already accepted retry")
			health, err = CollectionStatus(context.Background(), now-86400)
			require.NoError(t, err)
			assert.Zero(t, health.UnpersistedEvents)
			assert.Zero(t, health.PendingEvents)
			assert.Equal(t, "disabled", health.Availability)
			config.UpdateConfigFromMap(config.GlobalConfig.Get("perf_metrics_setting"), map[string]string{"enabled": "true"})
			Record(sample)
			require.NoError(t, Flush(context.Background()))
			all, err = QuerySummaryAll(24, nil)
			require.NoError(t, err)
			require.Len(t, all.Models, 1)
			assert.Equal(t, int64(2), all.Models[0].RequestCount, "unknown acknowledgement retries never double count")
			// A legacy administrator value must not overflow into deletion.
			oldBucket := now - 366*86400
			require.NoError(t, model.UpsertPerfMetric(&model.PerfMetric{ModelName: "keep-old-history", Group: "retired", BucketTs: oldBucket, RequestCount: 17}))
			config.UpdateConfigFromMap(config.GlobalConfig.Get("perf_metrics_setting"), map[string]string{"retention_days": strconv.Itoa(math.MaxInt)})
			collector.nextMaintenance = time.Time{} // Make this shutdown exercise the cleanup deadline.
			sample.EventID = "relay:shutdown"
			Record(sample)
			require.NoError(t, Shutdown(context.Background()))
			all, err = QuerySummaryAll(24, nil)
			require.NoError(t, err)
			require.Len(t, all.Models, 1)
			assert.Equal(t, int64(3), all.Models[0].RequestCount, "normal shutdown projects the current partial bucket")
			var old model.PerfMetric
			require.NoError(t, db.Where("model_name = ?", "keep-old-history").First(&old).Error)
			assert.Equal(t, int64(17), old.RequestCount)
			// An exhausted maintenance budget must keep history. A later valid
			// cleanup deletes only expired buckets, preserving current counters.
			canceled, cancel := context.WithCancel(context.Background())
			cancel()
			cutoff := now - 7*86400
			require.ErrorIs(t, model.DeletePerfMetricsBeforeContext(canceled, cutoff), context.Canceled)
			require.NoError(t, db.Where("model_name = ?", "keep-old-history").First(&old).Error)
			require.NoError(t, model.DeletePerfMetricsBeforeContext(context.Background(), cutoff))
			require.ErrorIs(t, db.Where("model_name = ?", "keep-old-history").First(&old).Error, gorm.ErrRecordNotFound)
			all, err = QuerySummaryAll(24, nil)
			require.NoError(t, err)
			require.Len(t, all.Models, 1)
			assert.Equal(t, int64(3), all.Models[0].RequestCount)
		})
	}
}

func TestRedisFailureDoesNotAffectDurableCollection(t *testing.T) {
	performanceDatabaseFixture(t, "sqlite", "")
	server := miniredis.RunT(t)
	client := redis.NewClient(&redis.Options{Addr: server.Addr()})
	previousEnabled, previousClient := common.RedisEnabled, common.RDB
	common.RedisEnabled, common.RDB = true, client
	t.Cleanup(func() {
		common.RedisEnabled, common.RDB = previousEnabled, previousClient
		require.NoError(t, client.Close())
	})
	server.Close()
	Record(Sample{EventID: "relay:redis-unavailable", Model: "redis-independent", Success: true})
	require.NoError(t, Flush(context.Background()))
	summary, err := QuerySummaryAll(24, nil)
	require.NoError(t, err)
	require.Len(t, summary.Models, 1)
	assert.Equal(t, int64(1), summary.Models[0].RequestCount)
	assert.Equal(t, 100.0, summary.Models[0].SuccessRate)
}

func TestRecordNormalizesBlankTrafficGroupToDefault(t *testing.T) {
	performanceDatabaseFixture(t, "sqlite", "")
	Record(Sample{EventID: "relay:whitespace", Model: " spaced-model ", Group: "  ", Success: true})
	require.NoError(t, Flush(context.Background()))
	result, err := Query(QueryParams{Model: "spaced-model", Group: "default", Hours: 24})
	require.NoError(t, err)
	require.NotNil(t, result.Summary, "blank traffic groups must remain visible in the default group")
	assert.Equal(t, 100.0, result.Summary.SuccessRate)
}
