package service

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

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
		{"rate limit mentioning tokens", 429, `{"error":{"message":"token limit per minute exceeded"}}`, ""},
		{"server error mentioning context", 503, `{"error":{"message":"context window service unavailable"}}`, ""},
		{"unrelated invalid request", 400, `{"error":{"code":"invalid_model","message":"unknown model"}}`, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			assert.Equal(t, tc.want, classifyAnalysisSizeError(tc.status, []byte(tc.body)))
		})
	}
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
	db := setupErrorAnalysisWorkflowDB(t)
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
}

func TestAnalysisEvidencePrunedAfterRetention(t *testing.T) {
	db := setupErrorAnalysisWorkflowDB(t)
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
}
