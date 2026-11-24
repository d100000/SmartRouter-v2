package service

import (
	"context"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/glebarez/sqlite"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"gorm.io/driver/mysql"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
)

// forEachAnalysisDatabase runs a test on SQLite and, when their disposable
// DSNs are set, on MySQL (TEST_ANALYSIS_MYSQL_DSN) and PostgreSQL
// (TEST_ANALYSIS_POSTGRES_DSN). The tables are dropped and recreated.
func forEachAnalysisDatabase(t *testing.T, test func(t *testing.T, db *gorm.DB)) {
	dialects := map[string]func(t *testing.T) gorm.Dialector{
		"sqlite": func(t *testing.T) gorm.Dialector { return sqlite.Open(filepath.Join(t.TempDir(), "analysis.db")) },
	}
	if dsn := os.Getenv("TEST_ANALYSIS_MYSQL_DSN"); dsn != "" {
		dialects["mysql"] = func(*testing.T) gorm.Dialector { return mysql.Open(dsn) }
	}
	if dsn := os.Getenv("TEST_ANALYSIS_POSTGRES_DSN"); dsn != "" {
		dialects["postgres"] = func(*testing.T) gorm.Dialector { return postgres.Open(dsn) }
	}
	for name, open := range dialects {
		t.Run(name, func(t *testing.T) {
			test(t, setupErrorAnalysisWorkflowDB(t, open(t)))
		})
	}
}

func TestAnalysisSizeErrorsAreRecognised(t *testing.T) {
	for _, tc := range []struct {
		name   string
		status int
		body   string
		want   string
	}{
		{"OpenAI context code", 400, `{"error":{"code":"context_length_exceeded","message":"x"}}`, "input"},
		{"Anthropic wording", 400, `{"error":{"type":"invalid_request_error","message":"prompt is too long: 250000 tokens > 200000 maximum"}}`, "input"},
		{"error as a bare string", 400, `{"error":"Input is too long for the selected model"}`, "input"},
		{"top-level message", 400, `{"message":"This model's maximum context length is 131072 tokens"}`, "input"},
		{"proxy rejects the body", 413, `<html><body>413 Request Entity Too Large</body></html>`, "input"},
		{"Chinese gateway wording", 400, `{"error":{"message":"请求内容超出最大上下文长度"}}`, "input"},
		{"output parameter too large", 400, `{"error":{"message":"max_tokens is too large: 65536. This model supports at most 32768 completion tokens"}}`, "output"},
		{"output limit worded as unsupported", 400, `{"error":{"message":"max_tokens 65536 is not supported: the maximum is 32768"}}`, "output"},
		{"output limit name rejected", 400, `{"error":{"message":"Unsupported parameter: 'max_tokens' is not supported with this model. Use 'max_completion_tokens' instead.","param":"max_tokens"}}`, "parameter"},
		{"rate limit mentioning tokens", 429, `{"error":{"message":"token limit per minute exceeded"}}`, ""},
		{"request above the per-minute allowance", 429, `{"error":{"message":"Request too large for this model on tokens per min (TPM): Limit 30000, Requested 45000."}}`, "input"},
		{"quota error mentioning a token limit", 403, `{"error":{"message":"You exceeded your current quota: token limit reached"}}`, ""},
		{"gateway reports the overflow as a server error", 500, `{"error":{"code":"context_length_exceeded","message":"upstream error"}}`, "input"},
		{"server error mentioning context", 503, `{"error":{"message":"context window service unavailable"}}`, ""},
		{"unrelated invalid request", 400, `{"error":{"code":"invalid_model","message":"unknown model"}}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, classifyAnalysisRequestError(tc.status, []byte(tc.body), "max_tokens"))
		})
	}
}

func TestAnalysisCompletionSwitchesTheOutputLimitName(t *testing.T) {
	var sent []string
	client := &http.Client{Transport: analysisTestTransport(func(request *http.Request) (*http.Response, error) {
		body, err := io.ReadAll(request.Body)
		require.NoError(t, err)
		reply, status := `{"choices":[{"message":{"content":"done"},"finish_reason":"stop"}],"error":{}}`, 200
		if gjson.GetBytes(body, "max_tokens").Exists() {
			sent = append(sent, "max_tokens")
			reply, status = `{"error":{"message":"Unsupported parameter: 'max_tokens' is not supported with this model. Use 'max_completion_tokens' instead."}}`, 400
		} else {
			require.EqualValues(t, 4096, gjson.GetBytes(body, "max_completion_tokens").Int())
			sent = append(sent, "max_completion_tokens")
		}
		return &http.Response{StatusCode: status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(reply)), Request: request}, nil
	})}
	config := ErrorAnalysisConfig{Endpoint: "https://analysis.example/v1/chat/completions", Model: "unlisted-reasoning-model", TimeoutSeconds: 30, MaxOutputTokens: 4096}
	content, err := completeErrorAnalysis(context.Background(), client, config, "key", "system", "input", &AnalysisAttempt{})
	require.NoError(t, err)
	assert.Equal(t, "done", content, "an empty error member on a successful answer is not a failure")
	assert.Equal(t, []string{"max_tokens", "max_completion_tokens"}, sent, "a model without a preset is retried once with the other name")
}

func TestAnalysisCompletionMapsSizeErrors(t *testing.T) {
	for _, tc := range []struct {
		name, body, code string
		status           int
	}{
		{"string error", `{"error":"prompt is too long"}`, "analysis_context_limit", 400},
		{"payload too large", `Request Entity Too Large`, "analysis_context_limit", 413},
		{"output limit", `{"error":{"message":"max_completion_tokens exceeds the model limit"}}`, "analysis_output_limit", 400},
		{"error inside a 200 response", `{"error":{"code":"context_length_exceeded"}}`, "analysis_context_limit", 200},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := &http.Client{Transport: analysisTestTransport(func(request *http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: tc.status, Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(tc.body)), Request: request}, nil
			})}
			_, err := completeErrorAnalysis(context.Background(), client, ErrorAnalysisConfig{Endpoint: "https://analysis.example/v1/chat/completions", Model: "test", TimeoutSeconds: 30}, "key", "system", "input", &AnalysisAttempt{})
			var failure *analysisFailure
			require.ErrorAs(t, err, &failure)
			assert.Equal(t, tc.code, failure.Code)
			assert.False(t, failure.Retryable)
		})
	}
}

func TestAnalysisDefaultsUpgradeOnlyUntouchedValues(t *testing.T) {
	stored := ErrorAnalysisConfig{ContextWindowTokens: 32768, ChunkInputTokens: 8000, MaxOutputTokens: 4096, SafetyMarginTokens: 4096, MaxCalls: 100, Timezone: "Asia/Shanghai"}
	upgraded := upgradeAnalysisDefaults(stored)
	assert.Equal(t, defaultAnalysisContextWindow, upgraded.ContextWindowTokens)
	assert.Equal(t, defaultAnalysisChunkInput, upgraded.ChunkInputTokens)
	assert.Equal(t, 4096, upgraded.MaxOutputTokens, "an operator's own output budget is kept")
	assert.Equal(t, defaultAnalysisMaxCalls, upgraded.MaxCalls)
	assert.Equal(t, defaultAnalysisTimezone, upgraded.Timezone)
	assert.Equal(t, errorAnalysisDefaultsRevision, upgraded.DefaultsRevision)
	require.NoError(t, validateErrorAnalysisConfig(ErrorAnalysisConfig{Endpoint: "https://a.example/v1/chat/completions", Model: "m", Timezone: upgraded.Timezone, MaxAttempts: 1, TimeoutSeconds: 60, IdleSeconds: 60, SampleLimit: 10, ReportLanguage: "zh", ContextWindowTokens: upgraded.ContextWindowTokens, ChunkInputTokens: upgraded.ChunkInputTokens, MaxOutputTokens: defaultAnalysisMaxOutput, SafetyMarginTokens: upgraded.SafetyMarginTokens, MaxCalls: upgraded.MaxCalls}))

	small := upgradeAnalysisDefaults(ErrorAnalysisConfig{ContextWindowTokens: 16384, ChunkInputTokens: 8000, MaxOutputTokens: 2048, SafetyMarginTokens: 4096, MaxCalls: 100, Timezone: "UTC"})
	assert.Equal(t, []int{16384, 8000, 2048}, []int{small.ContextWindowTokens, small.ChunkInputTokens, small.MaxOutputTokens}, "budgets that would no longer fit an operator's own context window stay as they are")

	again := upgradeAnalysisDefaults(ErrorAnalysisConfig{DefaultsRevision: errorAnalysisDefaultsRevision, ContextWindowTokens: 32768, Timezone: "Asia/Shanghai"})
	assert.Equal(t, 32768, again.ContextWindowTokens, "a value chosen after the upgrade is never changed again")
	assert.Equal(t, "Asia/Shanghai", again.Timezone)
}

func TestAnalysisEvidenceSendsTheRepresentativeAttemptOnce(t *testing.T) {
	first := model.FailureLog{ID: 1, RequestID: "r", CreatedAt: 10, LogType: model.LogTypeError, Channel: 3, ErrorMessage: "first"}
	last := model.FailureLog{ID: 2, RequestID: "r", CreatedAt: 11, LogType: model.LogTypeConsume, Channel: 4, ErrorMessage: ""}
	entry := last
	entry.Attempts = 2
	entry.Records = []model.FailureLog{last, first}
	compactAnalysisEvidence(&entry)
	require.Len(t, entry.Records, 1)
	assert.Equal(t, 1, entry.Records[0].ID)

	single := first
	single.Records = []model.FailureLog{first}
	compactAnalysisEvidence(&single)
	assert.Empty(t, single.Records, "a request with one attempt needs no retry list")
}

func TestAnalysisTruncatedOutputGetsMoreRoomInsteadOfSplitting(t *testing.T) {
	forEachAnalysisDatabase(t, func(t *testing.T, db *gorm.DB) {
		t.Setenv("CRYPTO_SECRET", "output-room-test-secret-not-for-production")
		config, err := GetErrorAnalysisConfig()
		require.NoError(t, err)
		config.MaxAttempts, config.Timezone = 1, "UTC"
		require.NoError(t, SaveErrorAnalysisConfig(config, "room-key", false))
		start := time.Date(2024, 1, 8, 0, 0, 0, 0, time.UTC).Unix()
		require.NoError(t, db.Create(&[]model.Log{
			{RequestId: "room-A", Type: model.LogTypeError, CreatedAt: start + 1, ModelName: "test-model", Other: `{"error_code":"timeout"}`},
			{RequestId: "room-B", Type: model.LogTypeError, CreatedAt: start + 2, ModelName: "test-model", Other: `{"error_code":"timeout"}`},
		}).Error)
		var groupBudgets []int
		client := &http.Client{Transport: analysisTestTransport(func(request *http.Request) (*http.Response, error) {
			var payload struct {
				MaxTokens int `json:"max_tokens"`
				Messages  []struct {
					Content string `json:"content"`
				} `json:"messages"`
			}
			require.NoError(t, common.DecodeJson(request.Body, &payload))
			input := payload.Messages[1].Content
			if gjson.Get(input, "samples").IsArray() {
				groupBudgets = append(groupBudgets, payload.MaxTokens)
				if len(groupBudgets) == 1 {
					return analysisWorkflowTestResponse(t, request, "unfinished", "length")
				}
				return analysisWorkflowTestResponse(t, request, analysisFinding{Title: "Timeouts", Facts: []string{"two timeouts"}, EvidenceIDs: []string{"room-A", "room-B"}}, "stop")
			}
			if gjson.Parse(input).IsArray() {
				return analysisWorkflowTestResponse(t, request, analysisFinding{Title: "Timeouts", Facts: []string{"two timeouts"}, EvidenceIDs: []string{"room-A"}}, "stop")
			}
			return analysisWorkflowTestResponse(t, request, "Check the timeout channel.", "stop")
		})}
		task, _, err := StartErrorAnalysis(model.ErrorAnalysisFilter{Start: start, End: start + 86400})
		require.NoError(t, err)
		claimed, ok, err := model.ClaimSystemTask(task.ID, model.ErrorAnalysisTaskType, "room-worker", time.Now().Unix()+600)
		require.NoError(t, err)
		require.True(t, ok)
		(errorAnalysisHandler{client: client}).Run(context.Background(), claimed, "room-worker")
		done, err := model.GetSystemTaskByTaskID(task.TaskID)
		require.NoError(t, err)
		assert.Equal(t, model.SystemTaskStatusSucceeded, done.Status, done.Error)
		assert.Equal(t, []int{defaultAnalysisMaxOutput, 2 * defaultAnalysisMaxOutput}, groupBudgets, "the same group is sent again with twice the output budget, not split")
	})
}

func TestAnalysisEvidencePrunedAfterRetention(t *testing.T) {
	forEachAnalysisDatabase(t, func(t *testing.T, db *gorm.DB) {
		now := time.Now().Unix()
		old := now - 40*86400
		tasks := []model.SystemTask{
			{TaskID: "old-done", Type: model.ErrorAnalysisTaskType, Status: model.SystemTaskStatusSucceeded, CreatedAt: old, UpdatedAt: old},
			{TaskID: "old-failed", Type: model.ErrorAnalysisTaskType, Status: model.SystemTaskStatusFailed, CreatedAt: old, UpdatedAt: old},
			{TaskID: "recent-done", Type: model.ErrorAnalysisTaskType, Status: model.SystemTaskStatusSucceeded, CreatedAt: now, UpdatedAt: now},
		}
		require.NoError(t, db.Create(&tasks).Error)
		for _, task := range tasks {
			for _, kind := range []string{"config", "request", "completion", "group", "finding"} {
				require.NoError(t, db.Create(&model.ErrorAnalysisArtifact{TaskID: task.TaskID, Kind: kind, Name: kind, Data: "{}"}).Error)
			}
		}
		removed, err := model.PruneErrorAnalysisEvidence(context.Background(), now-30*86400)
		require.NoError(t, err)
		assert.Equal(t, int64(2+4), removed)
		kinds := func(taskID string) []string {
			var values []string
			require.NoError(t, db.Model(&model.ErrorAnalysisArtifact{}).Where("task_id = ?", taskID).Order("kind").Pluck("kind", &values).Error)
			return values
		}
		assert.Equal(t, []string{"config", "finding", "group"}, kinds("old-done"), "a finished report keeps its findings")
		assert.Equal(t, []string{"config"}, kinds("old-failed"), "a failed run collects again if resumed")
		assert.Len(t, kinds("recent-done"), 5)
	})
}

func TestAnalysisModelPresetsMatchGatewayNames(t *testing.T) {
	for id, want := range map[string]string{
		"deepseek-flash":                  "deepseek-v4.1-flash",
		"deepseek-v4.1-flash":             "deepseek-v4.1-flash",
		"DeepSeek-V4.1-Flash-1017":        "deepseek-v4.1-flash",
		"deepseek-v4-flash":               "deepseek-v4.1-flash",
		"deepseek/deepseek-v4-pro":        "deepseek-v4-pro",
		"deepseek-v4-pro-0813":            "deepseek-v4-pro",
		"kimi-k3":                         "kimi-k3",
		"moonshotai/kimi-k3":              "kimi-k3",
		"kimi-k3.1-pro":                   "kimi-k3.1-pro",
		"kimi-k3.1-pro-highspeed":         "kimi-k3.1-pro",
		"gpt-6.2-sol":                     "gpt-6.2-sol",
		"openai/gpt-6.2-sol":              "gpt-6.2-sol",
		"claude-opus-5-5":                 "claude-opus-5.5",
		"claude-opus-5-5-20260922":        "claude-opus-5.5",
		"anthropic/claude-opus-5.5":       "claude-opus-5.5",
		"gpt-6.2-sol@reasoning_effort:hi": "gpt-6.2-sol",
	} {
		preset := MatchAnalysisModelPreset(id)
		if assert.NotNil(t, preset, id) {
			assert.Equal(t, want, preset.ID, id)
		}
	}
	for _, id := range []string{"", "gpt-6.2-astra", "claude-opus-5", "kimi-k2.8", "deepseek-v3.2", "gpt-6.2-solar", "kimi-k30"} {
		assert.Nil(t, MatchAnalysisModelPreset(id), id)
	}
}

func TestAnalysisPresetsFitTheConfigurationLimits(t *testing.T) {
	for _, preset := range AnalysisModelPresets() {
		config := ErrorAnalysisConfig{Endpoint: "https://a.example/v1/chat/completions", Model: preset.ID, Timezone: "UTC", MaxAttempts: 1, TimeoutSeconds: 60, IdleSeconds: 60, SampleLimit: 10, ReportLanguage: "zh",
			ContextWindowTokens: preset.ContextWindow, ChunkInputTokens: preset.InputBudget, MaxOutputTokens: preset.OutputReserve, SafetyMarginTokens: defaultAnalysisSafetyMargin, MaxCalls: 100, MaxConcurrency: preset.Concurrency}
		require.NoError(t, validateErrorAnalysisConfig(config), preset.ID)
		assert.Contains(t, []string{"max_tokens", "max_completion_tokens"}, preset.TokenParam)
		assert.LessOrEqual(t, preset.InputBudget, preset.ContextWindow/4, "%s keeps to the effective part of its window", preset.ID)
	}
	assert.Equal(t, "Kimi K3.1 Pro", analysisPresetName("moonshot/kimi-k3.1-pro-highspeed"), "the settings page shows the preset by name")
	assert.Empty(t, analysisPresetName("some-private-model"))
	assert.Equal(t, "max_completion_tokens", analysisTokenParam("gpt-6.2-sol"))
	assert.Equal(t, "max_tokens", analysisTokenParam("some-private-model"))
	assert.Equal(t, 4, analysisConcurrency(ErrorAnalysisConfig{Model: "deepseek-flash"}))
	assert.Equal(t, 3, analysisConcurrency(ErrorAnalysisConfig{Model: "deepseek-flash", MaxConcurrency: 3}))
	assert.Equal(t, 2, analysisConcurrency(ErrorAnalysisConfig{Model: "unknown"}))
}

func TestAnalysisModelListMarksPresetsFirst(t *testing.T) {
	options := parseAnalysisModelList([]byte(`{"object":"list","data":[
		{"id":"zeta-local"},
		{"id":"gpt-6.2-sol"},
		{"id":"deepseek-flash"},
		{"id":"gpt-6.2-sol"},
		{"id":"router/other","context_length":200000,"top_provider":{"max_completion_tokens":4096}},
		{"id":""}
	]}`))
	require.Len(t, options, 4)
	assert.Equal(t, []string{"deepseek-flash", "gpt-6.2-sol", "router/other", "zeta-local"}, []string{options[0].ID, options[1].ID, options[2].ID, options[3].ID})
	assert.Equal(t, "deepseek-v4.1-flash", options[0].Preset.ID)
	assert.Nil(t, options[2].Preset)

	contextWindow, input, output := SuggestedAnalysisBudget(options[2])
	assert.Equal(t, []int{200000, 50000, 4096}, []int{contextWindow, input, output}, "a reported context suggests a quarter of it")
	contextWindow, input, output = SuggestedAnalysisBudget(options[3])
	assert.Equal(t, []int{65536, 32768, 8192}, []int{contextWindow, input, output}, "nothing known: conservative defaults")
	contextWindow, input, output = SuggestedAnalysisBudget(options[0])
	assert.Equal(t, []int{1_000_000, 131_072, 16_384}, []int{contextWindow, input, output})
}

func TestAnalysisModelListNeverSendsTheSavedKeyElsewhere(t *testing.T) {
	forEachAnalysisDatabase(t, func(t *testing.T, db *gorm.DB) {
		t.Setenv("CRYPTO_SECRET", "model-list-test-secret-not-for-production")
		config, err := GetErrorAnalysisConfig()
		require.NoError(t, err)
		require.NoError(t, SaveErrorAnalysisConfig(config, "saved-key", false))
		_, err = ListAnalysisModels(context.Background(), "https://elsewhere.example/v1/chat/completions", "")
		require.EqualError(t, err, "Enter the API key for this endpoint")
		_, err = ListAnalysisModels(context.Background(), "http://insecure.example/v1/chat/completions", "key")
		require.Error(t, err)
	})
}

type packTestProvider struct {
	mu        sync.Mutex
	groupSent map[string]int
	packSizes []int
	finals    int
	// breakPacks returns a pack answer that omits one group.
	breakPacks bool
	// refuse answers a single group of this reason with a server error.
	refuse string
}

func (p *packTestProvider) client(t *testing.T) *http.Client {
	return &http.Client{Transport: analysisTestTransport(func(request *http.Request) (*http.Response, error) {
		var payload struct {
			Messages []struct {
				Content string `json:"content"`
			} `json:"messages"`
		}
		require.NoError(t, common.DecodeJson(request.Body, &payload))
		input := gjson.Parse(payload.Messages[1].Content)
		p.mu.Lock()
		defer p.mu.Unlock()
		finding := func(group gjson.Result) map[string]any {
			id := group.Get("samples.0.request_id").String()
			return map[string]any{"group_id": group.Get("id").String(), "title": group.Get("reason").String() + " failures", "facts": []string{"seen on " + group.Get("channels.#").String() + " channels"}, "evidence_ids": []string{id}}
		}
		switch {
		case input.Get("groups").IsArray():
			groups := input.Get("groups").Array()
			p.packSizes = append(p.packSizes, len(groups))
			out := []any{}
			for i, group := range groups {
				if p.breakPacks && i == len(groups)-1 {
					continue
				}
				out = append(out, finding(group))
			}
			return analysisWorkflowTestResponse(t, request, map[string]any{"findings": out}, "stop")
		case input.Get("samples").IsArray():
			if p.refuse != "" && input.Get("reason").String() == p.refuse {
				return &http.Response{StatusCode: http.StatusInternalServerError, Header: http.Header{}, Body: io.NopCloser(strings.NewReader(`{}`)), Request: request}, nil
			}
			p.groupSent[input.Get("reason").String()]++
			result := finding(input)
			delete(result, "group_id")
			return analysisWorkflowTestResponse(t, request, result, "stop")
		default:
			p.finals++
			return analysisWorkflowTestResponse(t, request, map[string]any{
				"summary":  "Three kinds of failure; timeouts dominate.",
				"priority": map[string]any{"title": "Timeouts first", "facts": []string{"timeouts lead"}},
				"actions":  []string{"Check channel 1 timeouts", "Compare rate limits"},
				"limits":   []string{"No upstream status page was available."},
			}, "stop")
		}
	})}
}

func runPackTestAnalysis(t *testing.T, db *gorm.DB, configure func(*ErrorAnalysisConfig), provider *packTestProvider) (*model.SystemTask, AnalysisResult) {
	t.Helper()
	t.Setenv("CRYPTO_SECRET", "pack-test-secret-not-for-production")
	config, err := GetErrorAnalysisConfig()
	require.NoError(t, err)
	config.MaxAttempts, config.Timezone = 1, "UTC"
	if configure != nil {
		configure(&config)
	}
	require.NoError(t, SaveErrorAnalysisConfig(config, "pack-key", false))
	start := time.Date(2024, 1, 8, 0, 0, 0, 0, time.UTC).Unix()
	task, _, err := StartErrorAnalysis(model.ErrorAnalysisFilter{Start: start, End: start + 86400})
	require.NoError(t, err)
	claimed, ok, err := model.ClaimSystemTask(task.ID, model.ErrorAnalysisTaskType, "pack-worker", time.Now().Unix()+600)
	require.NoError(t, err)
	require.True(t, ok)
	(errorAnalysisHandler{client: provider.client(t)}).Run(context.Background(), claimed, "pack-worker")
	done, err := model.GetSystemTaskByTaskID(task.TaskID)
	require.NoError(t, err)
	if provider.refuse != "" {
		return done, AnalysisResult{}
	}
	require.Equal(t, model.SystemTaskStatusSucceeded, done.Status, done.Error)
	var result AnalysisResult
	require.NoError(t, common.UnmarshalJsonStr(string(done.Result), &result))
	return done, result
}

func seedPackTestLogs(t *testing.T, db *gorm.DB) {
	start := time.Date(2024, 1, 8, 0, 0, 0, 0, time.UTC).Unix()
	logs := []model.Log{
		// One failure on two channels, differing only in variable parts.
		{RequestId: "t-1", Type: model.LogTypeError, CreatedAt: start + 1, ModelName: "m", ChannelId: 1, Other: `{"error_code":"timeout"}`, Content: "upstream timed out after 30000 ms (req_8f3a2b9c1d)"},
		{RequestId: "t-2", Type: model.LogTypeError, CreatedAt: start + 2, ModelName: "m", ChannelId: 2, Other: `{"error_code":"timeout"}`, Content: "upstream timed out after 31250 ms (req_0c9d8e7f6a)"},
		{RequestId: "t-3", Type: model.LogTypeError, CreatedAt: start + 3, ModelName: "m", ChannelId: 1, Other: `{"error_code":"timeout"}`, Content: "upstream timed out after 29990 ms (req_11aa22bb33)"},
		{RequestId: "r-1", Type: model.LogTypeError, CreatedAt: start + 4, ModelName: "m", ChannelId: 3, Other: `{"error_code":"rate_limited"}`, Content: "429 too many requests"},
		{RequestId: "b-1", Type: model.LogTypeError, CreatedAt: start + 5, ModelName: "n", ChannelId: 3, Other: `{"error_code":"bad_response"}`, Content: "invalid JSON in upstream body"},
	}
	for i := range 20 {
		logs = append(logs, model.Log{RequestId: "ok-" + string(rune('a'+i)), Type: model.LogTypeConsume, CreatedAt: start + 10 + int64(i), ModelName: "m", ChannelId: 1, CompletionTokens: 5})
	}
	require.NoError(t, db.Create(&logs).Error)
}

func TestAnalysisGroupsTheSameFailureAcrossChannels(t *testing.T) {
	forEachAnalysisDatabase(t, func(t *testing.T, db *gorm.DB) {
		seedPackTestLogs(t, db)
		provider := &packTestProvider{groupSent: map[string]int{}}
		done, result := runPackTestAnalysis(t, db, nil, provider)

		var groups []model.ErrorAnalysisArtifact
		require.NoError(t, db.Where("task_id = ? AND kind = ?", done.TaskID, "group").Find(&groups).Error)
		require.Len(t, groups, 3, "timeouts on two channels form one group")
		assert.Equal(t, []int{3}, provider.packSizes, "the three small groups share one call")
		assert.Empty(t, provider.groupSent)
		assert.Equal(t, 1, provider.finals, "summary, priority, steps and limits come from one call")
		assert.Equal(t, 2, result.Run.Calls)

		require.GreaterOrEqual(t, len(result.Sections), 5)
		assert.Equal(t, "priority", result.Sections[0].ID)
		timeout := result.Sections[1]
		assert.Equal(t, "issue-1", timeout.ID, "the most affected group is listed first")
		assert.Equal(t, "timeout failures", timeout.Title)
		assert.Equal(t, 3, timeout.Affected)
		assert.ElementsMatch(t, []string{"1", "2"}, timeout.Channels)
		assert.Equal(t, "critical", timeout.Severity, "3 of 25 requests is above 5%")
		assert.Equal(t, "critical", result.Severity)
		assert.Equal(t, "Three kinds of failure; timeouts dominate.", result.Summary)
		assert.Contains(t, result.Report, "1. Check channel 1 timeouts")
		assert.Contains(t, result.Report, "No upstream status page was available.")
		assert.Equal(t, 25, result.Snapshot.Requests)

		var stored int64
		require.NoError(t, db.Model(&model.ErrorAnalysisArtifact{}).Where("task_id = ? AND kind = ?", done.TaskID, "request").Count(&stored).Error)
		assert.EqualValues(t, 5, stored, "only failed requests are stored; the 20 clean ones are counted")
	})
}

func TestAnalysisPackThatLosesAGroupIsSplit(t *testing.T) {
	forEachAnalysisDatabase(t, func(t *testing.T, db *gorm.DB) {
		seedPackTestLogs(t, db)
		provider := &packTestProvider{groupSent: map[string]int{}, breakPacks: true}
		_, result := runPackTestAnalysis(t, db, nil, provider)
		assert.Equal(t, []int{3, 2}, provider.packSizes, "the incomplete pack of three is retried as halves of one and two")
		assert.Equal(t, 3, len(provider.groupSent), "the halves keep losing a group, so every group falls back to its own call")
		for _, section := range result.Sections {
			if section.ID != "priority" && section.ID != "actions" && section.ID != "limits" {
				assert.NotEmpty(t, section.Title)
			}
		}
	})
}

func TestAnalysisFindingsOfASplitPackSurviveALaterFailure(t *testing.T) {
	forEachAnalysisDatabase(t, func(t *testing.T, db *gorm.DB) {
		seedPackTestLogs(t, db)
		// The pack of three loses a group and is split. The timeout group
		// then gets its finding; the provider fails the rate-limit group.
		provider := &packTestProvider{groupSent: map[string]int{}, breakPacks: true, refuse: "rate_limited"}
		paused, _ := runPackTestAnalysis(t, db, nil, provider)
		require.Equal(t, model.SystemTaskStatusFailed, paused.Status)
		var state AnalysisState
		require.NoError(t, paused.DecodeState(&state))
		require.True(t, state.CanResume)
		assert.GreaterOrEqual(t, state.CompletedChunks, 1, "the finished group counts as progress")

		provider.refuse = ""
		_, err := ResumeErrorAnalysis(paused.TaskID)
		require.NoError(t, err)
		claimed, ok, err := model.ClaimSystemTask(paused.ID, model.ErrorAnalysisTaskType, "pack-resume", time.Now().Unix()+600)
		require.NoError(t, err)
		require.True(t, ok)
		(errorAnalysisHandler{client: provider.client(t)}).Run(context.Background(), claimed, "pack-resume")
		done, err := model.GetSystemTaskByTaskID(paused.TaskID)
		require.NoError(t, err)
		require.Equal(t, model.SystemTaskStatusSucceeded, done.Status, done.Error)
		assert.Equal(t, 1, provider.groupSent["timeout"], "the stored finding is not asked for again")
		threes := 0
		for _, size := range provider.packSizes {
			if size == 3 {
				threes++
			}
		}
		assert.Equal(t, 1, threes, "the resumed run packs only the groups still missing a finding")
		var result AnalysisResult
		require.NoError(t, common.UnmarshalJsonStr(string(done.Result), &result))
		assert.Greater(t, result.Run.Calls, state.CallsUsed, "the run summary counts the calls of both runs")
	})
}

func TestAnalysisLargeGroupsRunInParallel(t *testing.T) {
	forEachAnalysisDatabase(t, func(t *testing.T, db *gorm.DB) {
		seedPackTestLogs(t, db)
		provider := &packTestProvider{groupSent: map[string]int{}}
		_, result := runPackTestAnalysis(t, db, func(config *ErrorAnalysisConfig) {
			// An output reserve too small for two findings gives every group its
			// own call.
			config.MaxOutputTokens, config.MaxConcurrency = 1024, 3
		}, provider)
		assert.Empty(t, provider.packSizes)
		assert.Equal(t, map[string]int{"timeout": 1, "rate_limited": 1, "bad_response": 1}, provider.groupSent)
		assert.Equal(t, 4, result.Run.Calls)
	})
}

func TestAnalysisRequestWithALaterRowIsStoredNotCounted(t *testing.T) {
	forEachAnalysisDatabase(t, func(t *testing.T, db *gorm.DB) {
		start := time.Date(2024, 1, 8, 0, 0, 0, 0, time.UTC).Unix()
		logs := []model.Log{{RequestId: "late", Type: model.LogTypeConsume, CreatedAt: start + 1, ModelName: "m", ChannelId: 1, CompletionTokens: 3}}
		for i := range 4 {
			logs = append(logs, model.Log{RequestId: "fill-" + string(rune('a'+i)), Type: model.LogTypeConsume, CreatedAt: start + 2, ModelName: "m", ChannelId: 1, CompletionTokens: 1})
		}
		// The failed attempt of "late" is written after its successful row,
		// several pages later. Its first row must be stored, not counted, so
		// the two rows end up as one request whatever happens in between.
		logs = append(logs, model.Log{RequestId: "late", Type: model.LogTypeError, CreatedAt: start + 3, ModelName: "m", ChannelId: 2, Other: `{"error_code":"timeout"}`})
		require.NoError(t, db.Create(&logs).Error)
		task, err := model.CreateSystemTask(model.ErrorAnalysisTaskType, errorAnalysisPayload{ErrorAnalysisFilter: model.ErrorAnalysisFilter{Start: start, End: start + 86400}, WorkflowVersion: 2}, AnalysisState{Version: 2})
		require.NoError(t, err)
		_, ok, err := model.ClaimSystemTask(task.ID, model.ErrorAnalysisTaskType, "late-worker", time.Now().Unix()+600)
		require.NoError(t, err)
		require.True(t, ok)
		filter := model.ErrorAnalysisFilter{Start: start, End: start + 86400}
		for {
			progress, err := model.CollectErrorAnalysisPage(context.Background(), task.TaskID, "late-worker", filter, 2)
			require.NoError(t, err)
			if progress.Done {
				break
			}
		}
		for {
			progress, err := model.AggregateErrorAnalysisPage(context.Background(), task.TaskID, "late-worker", 500, 8)
			require.NoError(t, err)
			if progress.Done {
				break
			}
		}
		snapshot, err := model.LoadErrorAnalysisWorkflowSnapshot(task.TaskID)
		require.NoError(t, err)
		assert.Equal(t, 5, snapshot.Current.Requests, "the late row joins its request instead of adding one")
		var stored int64
		require.NoError(t, db.Model(&model.ErrorAnalysisArtifact{}).Where("task_id = ? AND kind = ?", task.TaskID, "request").Count(&stored).Error)
		assert.EqualValues(t, 1, stored, "the four clean requests are only counted")
		assert.Equal(t, 1, snapshot.Current.Recovered)
		evidence, err := model.GetErrorAnalysisRequestEvidence(task.TaskID, "late")
		require.NoError(t, err)
		require.NotNil(t, evidence)
		assert.Equal(t, 2, evidence.Attempts)
	})
}
