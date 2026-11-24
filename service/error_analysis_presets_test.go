package service

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

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
	setupErrorAnalysisWorkflowDB(t)
	t.Setenv("CRYPTO_SECRET", "model-list-test-secret-not-for-production")
	config, err := GetErrorAnalysisConfig()
	require.NoError(t, err)
	require.NoError(t, SaveErrorAnalysisConfig(config, "saved-key", false))
	_, err = ListAnalysisModels(context.Background(), "https://elsewhere.example/v1/chat/completions", "")
	require.EqualError(t, err, "Enter the API key for this endpoint")
	_, err = ListAnalysisModels(context.Background(), "http://insecure.example/v1/chat/completions", "key")
	require.Error(t, err)
}
