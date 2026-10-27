package perfmetrics

type Store interface {
	Record(sample Sample)
	Query(params QueryParams) (QueryResult, error)
}

type Sample struct {
	EventID           string
	OccurredAt        int64
	OutputTokensKnown bool
	Model             string
	Group             string
	LatencyMs         int64
	TtftMs            int64
	HasTtft           bool
	Success           bool
	OutputTokens      int64
	GenerationMs      int64
}

type QueryParams struct {
	Model string
	Group string
	Hours int
	// AllowedGroups restricts both the per-group results and the model summary;
	// nil allows every group.
	AllowedGroups []string
}

// Summary is the request-weighted aggregate over every bucket in the window.
type Summary struct {
	OutputMetricsAvailable bool    `json:"output_metrics_available"`
	AvgLatencyMs           int64   `json:"avg_latency_ms"`
	SuccessRate            float64 `json:"success_rate"`
	AvgTps                 float64 `json:"avg_tps"`
}

type BucketPoint struct {
	OutputMetricsAvailable bool    `json:"output_metrics_available"`
	Ts                     int64   `json:"ts"`
	AvgTtftMs              int64   `json:"avg_ttft_ms"`
	AvgLatencyMs           int64   `json:"avg_latency_ms"`
	SuccessRate            float64 `json:"success_rate"`
	AvgTps                 float64 `json:"avg_tps"`
}

type GroupResult struct {
	OutputMetricsAvailable bool          `json:"output_metrics_available"`
	Group                  string        `json:"group"`
	AvgTtftMs              int64         `json:"avg_ttft_ms"`
	AvgLatencyMs           int64         `json:"avg_latency_ms"`
	SuccessRate            float64       `json:"success_rate"`
	AvgTps                 float64       `json:"avg_tps"`
	Series                 []BucketPoint `json:"series"`
}

type QueryResult struct {
	ModelName    string        `json:"model_name"`
	SeriesSchema string        `json:"series_schema"`
	Summary      *Summary      `json:"summary"`
	Series       []BucketPoint `json:"series"`
	WindowStart  int64         `json:"window_start"`
	WindowEnd    int64         `json:"window_end"`
	Groups       []GroupResult `json:"groups"`
}

type SuccessRatePoint struct {
	Ts          int64   `json:"ts"`
	SuccessRate float64 `json:"success_rate"`
}

type ModelSummary struct {
	OutputMetricsAvailable bool               `json:"output_metrics_available"`
	ModelName              string             `json:"model_name"`
	AvgLatencyMs           int64              `json:"avg_latency_ms"`
	SuccessRate            float64            `json:"success_rate"`
	AvgTps                 float64            `json:"avg_tps"`
	RecentSuccessSeries    []SuccessRatePoint `json:"recent_success_series,omitempty"`
	RequestCount           int64              `json:"-"`
}

type SummaryAllResult struct {
	Summary     *Summary       `json:"summary"`
	WindowStart int64          `json:"window_start"`
	WindowEnd   int64          `json:"window_end"`
	Models      []ModelSummary `json:"models"`
	// RecentSuccessSeries contains up to 24 nonempty hours from the last 7 days,
	// including requested models with no traffic in the summary window.
	RecentSuccessSeries map[string][]SuccessRatePoint `json:"recent_success_series,omitempty"`
}

type bucketKey struct {
	model    string
	group    string
	bucketTs int64
}

type counters struct {
	requestCount   int64
	successCount   int64
	totalLatencyMs int64
	ttftSumMs      int64
	ttftCount      int64
	outputTokens   int64
	generationMs   int64
}
