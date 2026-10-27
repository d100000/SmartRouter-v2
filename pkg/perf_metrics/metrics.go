package perfmetrics

import (
	"context"
	"math"
	"sort"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	relaycommon "github.com/QuantumNous/new-api/relay/common"
	"github.com/QuantumNous/new-api/relaykit/types"
	"github.com/QuantumNous/new-api/setting/perf_metrics_setting"
)

// seriesSchema is a stable client cache/schema marker. Do not change it when
// hiding fields or making response-only privacy hardening changes.
const seriesSchema = "dbcd0a3c01b55203"

// MaxRecentModels bounds the historical lookups made by one summary request.
const MaxRecentModels = 50
const recentSuccessSampleLimit = 24
const recentSuccessHistoryHours = 7 * 24

// RecordRelayResult remains the context-only entry point for callers without
// delivery state. HTTP/Responses callers use the shared delivery-aware outcome.
func RecordRelayResult(ctx context.Context, info *relaycommon.RelayInfo, apiErr *types.NewAPIError) {
	RecordRelayOutcome(ctx, info, ClassifyRelayOutcome(ctx, info, apiErr))
}

func RecordRelayOutcome(_ context.Context, info *relaycommon.RelayInfo, outcome Outcome) {
	if info == nil || outcome == OutcomeIgnored {
		return
	}
	if info.PerformanceEventID == "" {
		info.PerformanceEventID = "relay:" + common.NewRequestId()
	}
	now := time.Now()
	latencyMs := int64(0)
	if !info.StartTime.IsZero() && now.After(info.StartTime) {
		latencyMs = now.Sub(info.StartTime).Milliseconds()
	}
	hasTtft := info.IsStream && info.HasSendResponse()
	ttftMs, generationMs := int64(0), latencyMs
	if hasTtft {
		ttftMs = info.FirstResponseTime.Sub(info.StartTime).Milliseconds()
		generationMs = now.Sub(info.FirstResponseTime).Milliseconds()
	}
	Record(Sample{
		EventID: info.PerformanceEventID, OccurredAt: now.Unix(),
		Model: info.OriginModelName, Group: info.UsingGroup,
		LatencyMs: latencyMs, TtftMs: ttftMs, HasTtft: hasTtft,
		Success:           outcome == OutcomeSuccess,
		OutputTokens:      info.PerformanceOutputTokens,
		OutputTokensKnown: info.PerformanceOutputTokens > 0,
		GenerationMs:      generationMs,
	})
}

// RecordTaskResult is retained for integrations outside the polling pipeline.
// The production task path persists a pending marker with its terminal CAS and
// stages the same task:<primary key> identity without a crash window.
func RecordTaskResult(task *model.Task, result *relaycommon.TaskInfo) {
	if task == nil || !perf_metrics_setting.GetSetting().Enabled {
		return
	}
	copy := *task
	if result != nil && result.PerformanceOutputTokens != nil {
		copy.PrivateData.PerformanceOutputTokens = result.PerformanceOutputTokens
	}
	if event := copy.PerformanceMetricEvent(); event != nil {
		recordEvent(*event)
	}
}

// queryWindow covers the current partial hour plus the preceding complete
// hours, so every reader shares the same hourly buckets.
func queryWindow(now time.Time, hours int) (int64, int64) {
	if hours <= 0 {
		hours = 24
	}
	hours = min(hours, 24*30)
	endTs := now.Unix()
	return endTs - endTs%3600 - int64(hours-1)*3600, endTs
}

func Query(params QueryParams) (QueryResult, error) {
	startTs, endTs := queryWindow(time.Now(), params.Hours)
	allowedGroups := allowedGroupSet(params.AllowedGroups)

	merged := map[bucketKey]counters{}
	rows, err := model.GetPerfMetrics(params.Model, params.Group, startTs, endTs)
	if err != nil {
		return QueryResult{}, err
	}
	for _, row := range rows {
		if allowedGroups != nil {
			if _, ok := allowedGroups[row.Group]; !ok {
				continue
			}
		}
		mergeCounters(merged, bucketKey{
			model:    row.ModelName,
			group:    row.Group,
			bucketTs: row.BucketTs,
		}, counters{
			requestCount:   row.RequestCount,
			successCount:   row.SuccessCount,
			totalLatencyMs: row.TotalLatencyMs,
			ttftSumMs:      row.TtftSumMs,
			ttftCount:      row.TtftCount,
			outputTokens:   row.OutputTokens,
			generationMs:   row.GenerationMs,
		})
	}

	result := buildQueryResult(params.Model, merged)
	result.WindowStart, result.WindowEnd = startTs, endTs
	return result, nil
}

func QuerySummaryAll(hours int, groups []string, recentModels ...string) (SummaryAllResult, error) {
	if len(recentModels) > MaxRecentModels {
		return SummaryAllResult{}, common.NewMessage("Invalid parameters")
	}
	now := time.Now()
	startTs, endTs := queryWindow(now, hours)
	recentStartTs, _ := queryWindow(now, recentSuccessHistoryHours)
	queryStartTs := startTs

	recentBuckets := make(map[string]map[int64]counters, len(recentModels))
	for _, name := range recentModels {
		if name != "" {
			recentBuckets[name] = map[int64]counters{}
		}
	}
	if len(recentBuckets) > 0 {
		buckets, err := model.GetRecentPerfMetricSuccessBucketsAll(recentModels, recentStartTs, endTs, groups, recentSuccessSampleLimit)
		if err != nil {
			return SummaryAllResult{}, err
		}
		for _, bucket := range buckets {
			queryStartTs = min(queryStartTs, bucket.BucketTs)
			mergeModelBucket(recentBuckets, bucket.ModelName, bucket.BucketTs, counters{
				requestCount: bucket.RequestCount,
				successCount: bucket.SuccessCount,
			})
		}
	}

	rows, err := model.GetPerfMetricsSummaryBucketsAll(queryStartTs, endTs, groups)
	if err != nil {
		return SummaryAllResult{}, err
	}

	modelBuckets := map[string]map[int64]counters{}
	for _, row := range rows {
		value := counters{
			requestCount:   row.RequestCount,
			successCount:   row.SuccessCount,
			totalLatencyMs: row.TotalLatencyMs,
			outputTokens:   row.OutputTokens,
			generationMs:   row.GenerationMs,
		}
		mergeModelBucket(modelBuckets, row.ModelName, row.BucketTs, value)
	}

	recentSeries := make(map[string][]SuccessRatePoint, len(recentBuckets))
	for name, buckets := range recentBuckets {
		points := recentSuccessSeries(buckets)
		if len(points) == 0 {
			continue
		}
		recentSeries[name] = points[max(0, len(points)-recentSuccessSampleLimit):]
	}

	// Requested models are summarized over the same hours as their status samples.
	totals := map[string]counters{}
	for name, buckets := range modelBuckets {
		fromTs := startTs
		if points, ok := recentSeries[name]; ok {
			fromTs = points[0].Ts
		}
		for bucketTs, value := range buckets {
			if bucketTs < fromTs {
				delete(buckets, bucketTs)
				continue
			}
			mergeModelTotals(totals, name, value)
		}
	}

	all := counters{}
	models := make([]ModelSummary, 0, len(totals))
	for name, total := range totals {
		if total.requestCount == 0 {
			continue
		}
		all.requestCount += total.requestCount
		all.successCount += total.successCount
		all.totalLatencyMs += total.totalLatencyMs
		all.outputTokens += total.outputTokens
		all.generationMs += total.generationMs
		avgLatency := total.totalLatencyMs / total.requestCount
		successRate := float64(total.successCount) / float64(total.requestCount) * 100
		avgTps := 0.0
		if total.generationMs > 0 {
			avgTps = float64(total.outputTokens) / (float64(total.generationMs) / 1000.0)
		}
		models = append(models, ModelSummary{
			ModelName:              name,
			AvgLatencyMs:           avgLatency,
			SuccessRate:            math.Round(successRate*100) / 100,
			AvgTps:                 math.Round(avgTps*100) / 100,
			OutputMetricsAvailable: total.generationMs > 0,
			RecentSuccessSeries:    recentSuccessSeries(modelBuckets[name]),
			RequestCount:           total.requestCount,
		})
	}
	sort.Slice(models, func(i, j int) bool {
		return models[i].RequestCount > models[j].RequestCount
	})

	return SummaryAllResult{
		Summary: summarize(all), WindowStart: startTs, WindowEnd: endTs,
		Models: models, RecentSuccessSeries: recentSeries,
	}, nil
}

func mergeModelTotals(totals map[string]counters, modelName string, value counters) {
	if value.requestCount == 0 {
		return
	}
	current := totals[modelName]
	current.requestCount += value.requestCount
	current.successCount += value.successCount
	current.totalLatencyMs += value.totalLatencyMs
	current.ttftSumMs += value.ttftSumMs
	current.ttftCount += value.ttftCount
	current.outputTokens += value.outputTokens
	current.generationMs += value.generationMs
	totals[modelName] = current
}

func mergeModelBucket(modelBuckets map[string]map[int64]counters, modelName string, bucketTs int64, value counters) {
	if value.requestCount == 0 {
		return
	}
	if _, ok := modelBuckets[modelName]; !ok {
		modelBuckets[modelName] = map[int64]counters{}
	}
	current := modelBuckets[modelName][bucketTs]
	current.requestCount += value.requestCount
	current.successCount += value.successCount
	current.totalLatencyMs += value.totalLatencyMs
	current.ttftSumMs += value.ttftSumMs
	current.ttftCount += value.ttftCount
	current.outputTokens += value.outputTokens
	current.generationMs += value.generationMs
	modelBuckets[modelName][bucketTs] = current
}

func recentSuccessSeries(buckets map[int64]counters) []SuccessRatePoint {
	if len(buckets) == 0 {
		return nil
	}
	hourly := map[int64]counters{}
	for ts, value := range buckets {
		hourTs := ts - ts%3600
		merged := hourly[hourTs]
		merged.requestCount += value.requestCount
		merged.successCount += value.successCount
		hourly[hourTs] = merged
	}
	timestamps := make([]int64, 0, len(hourly))
	for hourTs, value := range hourly {
		if value.requestCount == 0 {
			continue
		}
		timestamps = append(timestamps, hourTs)
	}
	if len(timestamps) == 0 {
		return nil
	}
	sort.Slice(timestamps, func(i, j int) bool {
		return timestamps[i] < timestamps[j]
	})
	points := make([]SuccessRatePoint, 0, len(timestamps))
	for _, hourTs := range timestamps {
		points = append(points, SuccessRatePoint{
			Ts:          hourTs,
			SuccessRate: math.Round(successRate(hourly[hourTs])*100) / 100,
		})
	}
	return points
}

func allowedGroupSet(groups []string) map[string]struct{} {
	if groups == nil {
		return nil
	}
	allowed := make(map[string]struct{}, len(groups))
	for _, group := range groups {
		allowed[group] = struct{}{}
	}
	return allowed
}

func bucketStart(ts int64) int64 {
	bucketSeconds := perf_metrics_setting.GetBucketSeconds()
	if bucketSeconds <= 0 {
		bucketSeconds = 3600
	}
	return ts - (ts % bucketSeconds)
}

func mergeCounters(merged map[bucketKey]counters, key bucketKey, value counters) {
	if value.requestCount == 0 {
		return
	}
	current := merged[key]
	current.requestCount += value.requestCount
	current.successCount += value.successCount
	current.totalLatencyMs += value.totalLatencyMs
	current.ttftSumMs += value.ttftSumMs
	current.ttftCount += value.ttftCount
	current.outputTokens += value.outputTokens
	current.generationMs += value.generationMs
	merged[key] = current
}

func buildQueryResult(modelName string, merged map[bucketKey]counters) QueryResult {
	groupBuckets := map[string]map[int64]counters{}
	modelBuckets := map[string]map[int64]counters{}
	for key, value := range merged {
		if value.requestCount == 0 {
			continue
		}
		if _, ok := groupBuckets[key.group]; !ok {
			groupBuckets[key.group] = map[int64]counters{}
		}
		groupBuckets[key.group][key.bucketTs] = value
		mergeModelBucket(modelBuckets, modelName, key.bucketTs, value)
	}

	groups := make([]string, 0, len(groupBuckets))
	for group := range groupBuckets {
		groups = append(groups, group)
	}
	sort.Strings(groups)

	all := counters{}
	results := make([]GroupResult, 0, len(groups))
	for _, group := range groups {
		buckets := groupBuckets[group]
		timestamps := make([]int64, 0, len(buckets))
		for ts := range buckets {
			timestamps = append(timestamps, ts)
		}
		sort.Slice(timestamps, func(i, j int) bool {
			return timestamps[i] < timestamps[j]
		})

		total := counters{}
		series := make([]BucketPoint, 0, len(timestamps))
		for _, ts := range timestamps {
			value := buckets[ts]
			total.requestCount += value.requestCount
			total.successCount += value.successCount
			total.totalLatencyMs += value.totalLatencyMs
			total.ttftSumMs += value.ttftSumMs
			total.ttftCount += value.ttftCount
			total.outputTokens += value.outputTokens
			total.generationMs += value.generationMs
			series = append(series, bucketPoint(ts, value))
		}
		all.requestCount += total.requestCount
		all.successCount += total.successCount
		all.totalLatencyMs += total.totalLatencyMs
		all.outputTokens += total.outputTokens
		all.generationMs += total.generationMs

		results = append(results, GroupResult{
			Group:                  group,
			OutputMetricsAvailable: total.generationMs > 0,
			AvgTtftMs:              avg(total.ttftSumMs, total.ttftCount),
			AvgLatencyMs:           avg(total.totalLatencyMs, total.requestCount),
			SuccessRate:            successRate(total),
			AvgTps:                 avgTps(total),
			Series:                 series,
		})
	}

	modelSeries := make([]BucketPoint, 0, len(modelBuckets[modelName]))
	for ts, value := range modelBuckets[modelName] {
		modelSeries = append(modelSeries, bucketPoint(ts, value))
	}
	sort.Slice(modelSeries, func(i, j int) bool {
		return modelSeries[i].Ts < modelSeries[j].Ts
	})

	return QueryResult{
		ModelName:    modelName,
		SeriesSchema: seriesSchema,
		Summary:      summarize(all),
		Series:       modelSeries,
		Groups:       results,
	}
}

// summarize returns nil when nothing was sampled, so clients can show
// "no data" instead of a fabricated 0% success rate.
func summarize(total counters) *Summary {
	if total.requestCount <= 0 {
		return nil
	}
	return &Summary{
		AvgLatencyMs:           avg(total.totalLatencyMs, total.requestCount),
		SuccessRate:            math.Round(successRate(total)*100) / 100,
		AvgTps:                 math.Round(avgTps(total)*100) / 100,
		OutputMetricsAvailable: total.generationMs > 0,
	}
}

func bucketPoint(ts int64, value counters) BucketPoint {
	return BucketPoint{
		Ts:                     ts,
		OutputMetricsAvailable: value.generationMs > 0,
		AvgTtftMs:              avg(value.ttftSumMs, value.ttftCount),
		AvgLatencyMs:           avg(value.totalLatencyMs, value.requestCount),
		SuccessRate:            successRate(value),
		AvgTps:                 avgTps(value),
	}
}

func avg(sum int64, count int64) int64 {
	if count <= 0 {
		return 0
	}
	return sum / count
}

func successRate(value counters) float64 {
	if value.requestCount <= 0 {
		return 0
	}
	return float64(value.successCount) / float64(value.requestCount) * 100
}

func avgTps(value counters) float64 {
	if value.outputTokens <= 0 || value.generationMs <= 0 {
		return 0
	}
	return float64(value.outputTokens) / (float64(value.generationMs) / 1000)
}
