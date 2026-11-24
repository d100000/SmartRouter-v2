package service

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"github.com/QuantumNous/new-api/tokenkit"
	"github.com/tidwall/gjson"
)

type errorAnalysisPayload struct {
	model.ErrorAnalysisFilter
	WorkflowVersion int `json:"workflow_version"`
}

type AnalysisReportSection struct {
	ID          string   `json:"id"`
	Title       string   `json:"title"`
	Content     string   `json:"content"`
	EvidenceIDs []string `json:"evidence_ids,omitempty"`
}

// normalizeAnalysisBudgets keeps previously stored configurations readable.
// The context setting is an operator limit, not a claim about provider capacity.
func normalizeAnalysisBudgets(config ErrorAnalysisConfig) ErrorAnalysisConfig {
	if config.ContextWindowTokens == 0 {
		config.ContextWindowTokens = defaultAnalysisContextWindow
	}
	if config.ChunkInputTokens == 0 {
		config.ChunkInputTokens = defaultAnalysisChunkInput
	}
	if config.MaxOutputTokens == 0 {
		config.MaxOutputTokens = defaultAnalysisMaxOutput
	}
	if config.SafetyMarginTokens == 0 {
		config.SafetyMarginTokens = defaultAnalysisSafetyMargin
	}
	if config.MaxCalls == 0 {
		config.MaxCalls = defaultAnalysisMaxCalls
	}
	return config
}

func NormalizeErrorAnalysisFilter(filter *model.ErrorAnalysisFilter) error {
	if filter.RequestID == "" && filter.Start == 0 && filter.End == 0 {
		config, err := GetErrorAnalysisConfig()
		if err != nil {
			return err
		}
		location, err := time.LoadLocation(config.Timezone)
		if err != nil {
			return common.NewMessage("Invalid analysis timezone")
		}
		now := time.Now().In(location)
		end := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, location)
		filter.Start, filter.End = end.AddDate(0, 0, -7).Unix(), end.Unix()
	}
	return filter.Normalize(time.Now())
}

func analysisTokenCount(modelName, text string) int {
	count := tokenkit.Count(modelName, text)
	// Unknown/non-GPT tokenizers are estimates. Keep headroom in addition to
	// the operator's reserve, and still handle a provider-side overflow.
	if tokenkit.ModelFamily(modelName) != tokenkit.FamilyOpenAI {
		count += (count + 3) / 4
	}
	return count
}

func analysisEvidenceBudget(config ErrorAnalysisConfig, system string) int {
	return min(config.ChunkInputTokens, config.ContextWindowTokens-config.MaxOutputTokens-config.SafetyMarginTokens-analysisTokenCount(config.Model, system)-64)
}

// completeErrorAnalysis consumes a bounded non-streaming response. A silent
// model is not an idle worker: only the whole request deadline applies here.
func completeErrorAnalysis(parent context.Context, client *http.Client, config ErrorAnalysisConfig, key, system, input string, attempt *AnalysisAttempt) (string, error) {
	config = normalizeAnalysisBudgets(config)
	attempt.InputTokens = analysisTokenCount(config.Model, system) + analysisTokenCount(config.Model, input) + 64
	attempt.MaxOutputTokens, attempt.TimeoutSeconds = config.MaxOutputTokens, config.TimeoutSeconds
	if analysisTokenCount(config.Model, input) > analysisEvidenceBudget(config, system) {
		return "", &analysisFailure{Code: "analysis_context_limit"}
	}
	started := time.Now()
	attempt.StartedAt = started.Unix()
	defer func() {
		attempt.FinishedAt = time.Now().Unix()
		attempt.DurationMs = time.Since(started).Milliseconds()
	}()
	ctx, cancel := context.WithTimeout(parent, time.Duration(config.TimeoutSeconds)*time.Second)
	defer cancel()
	body, err := common.Marshal(map[string]any{
		"model": config.Model, "stream": false, analysisTokenParam(config.Model): config.MaxOutputTokens,
		"messages": []map[string]string{{"role": "system", "content": system}, {"role": "user", "content": input}},
	})
	if err != nil {
		return "", &analysisFailure{Code: "analysis_input_error"}
	}
	request, err := http.NewRequestWithContext(ctx, http.MethodPost, config.Endpoint, bytes.NewReader(body))
	if err != nil {
		return "", &analysisFailure{Code: "analysis_configuration_error"}
	}
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	response, err := client.Do(request)
	if err != nil {
		return "", analysisTransportFailure(parent, ctx, false)
	}
	defer response.Body.Close()
	attempt.StatusCode = response.StatusCode
	const responseLimit = 2 * 1024 * 1024
	data, err := io.ReadAll(io.LimitReader(response.Body, responseLimit+1))
	if err != nil {
		return "", analysisTransportFailure(parent, ctx, false)
	}
	if len(data) > responseLimit {
		return "", &analysisFailure{Code: "analysis_response_too_large"}
	}
	var completion struct {
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"choices"`
	}
	decodeErr := common.Unmarshal(data, &completion)
	providerError := gjson.GetBytes(data, "error")
	hasProviderError := providerError.Exists() && providerError.Type != gjson.Null
	if response.StatusCode != http.StatusOK || hasProviderError {
		failure := &analysisFailure{Code: "analysis_http_error", Status: response.StatusCode, Retryable: response.StatusCode == 408 || response.StatusCode == 429 || response.StatusCode >= 500}
		switch response.StatusCode {
		case 401, 403:
			failure.Code = "analysis_authentication_failed"
		case 429:
			failure.Code = "analysis_rate_limited"
		case 400, 404, 422:
			failure.Code = "analysis_invalid_request"
		}
		switch classifyAnalysisSizeError(response.StatusCode, data) {
		case "input":
			failure.Code, failure.Retryable = "analysis_context_limit", false
		case "output":
			failure.Code, failure.Retryable = "analysis_output_limit", false
		}
		// Respect bounded Retry-After without retaining provider-controlled text.
		if seconds, err := strconv.Atoi(response.Header.Get("Retry-After")); err == nil && seconds > 0 {
			failure.RetryAfter = time.Duration(min(seconds, 120)) * time.Second
		}
		return "", failure
	}
	if decodeErr != nil || len(completion.Choices) != 1 {
		return "", &analysisFailure{Code: "analysis_invalid_response", Retryable: true}
	}
	choice := completion.Choices[0]
	switch choice.FinishReason {
	case "stop":
		attempt.FinishReason = "stop"
	case "length":
		attempt.FinishReason = "length"
		return "", &analysisFailure{Code: "analysis_incomplete_report"}
	default:
		attempt.FinishReason = "unknown"
		return "", &analysisFailure{Code: "analysis_incomplete_report"}
	}
	content := strings.TrimSpace(choice.Message.Content)
	if content == "" {
		return "", &analysisFailure{Code: "analysis_empty_report", Retryable: true}
	}
	if key != "" {
		content = strings.ReplaceAll(content, key, "[redacted]")
	}
	return content, nil
}

// Size errors are worded differently by every provider and gateway, and the
// error may be an object, a bare string or a top-level message. Only the
// classification is kept; provider text never reaches diagnostics.
var (
	analysisOutputLimitMarkers = []string{"max_tokens", "max_completion_tokens", "max_output_tokens", "maximum output", "output token"}
	analysisInputLimitMarkers  = []string{
		"context_length", "context length", "context_window", "context window", "maximum context",
		"prompt is too long", "prompt too long", "prompt_too_long", "input is too long", "input too long", "input_too_long",
		"too many tokens", "too_many_tokens", "too many input tokens", "request too large", "request_too_large",
		"reduce the length", "exceeds the maximum", "token limit", "string_above_max_length",
		"上下文", "超出最大", "过长", "太长",
	}
)

func classifyAnalysisSizeError(status int, data []byte) string {
	if status == http.StatusTooManyRequests || status >= 500 {
		return "" // Rate and capacity limits are retried, never split.
	}
	root := gjson.ParseBytes(data)
	parts := []string{root.Get("message").String()}
	if node := root.Get("error"); node.IsObject() {
		parts = append(parts, node.Get("code").String(), node.Get("type").String(), node.Get("message").String())
	} else if node.Exists() {
		parts = append(parts, node.String())
	}
	if !root.Exists() || !root.IsObject() {
		// Proxies often answer an oversized body with plain text or HTML.
		parts = append(parts, string(data[:min(len(data), 2048)]))
	}
	text := strings.ToLower(strings.Join(parts, " "))
	for _, marker := range analysisOutputLimitMarkers {
		if strings.Contains(text, marker) {
			return "output"
		}
	}
	if status == http.StatusRequestEntityTooLarge {
		return "input"
	}
	for _, marker := range analysisInputLimitMarkers {
		if strings.Contains(text, marker) {
			return "input"
		}
	}
	return ""
}

func newAnalysisCompletionClient(timeout time.Duration) *http.Client {
	client := newAnalysisHTTPClient()
	// Non-streaming providers may not return headers until generation finishes.
	client.Transport.(*http.Transport).ResponseHeaderTimeout = timeout
	return client
}
