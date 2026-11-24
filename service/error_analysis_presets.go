package service

import (
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/tidwall/gjson"
)

// AnalysisModelPreset describes how the error analysis should use a known
// model. Context and output limits are the provider's published figures; the
// input budget, output reserve and concurrency are this module's operating
// defaults. Long-context benchmarks find effective context well below the
// advertised window, so a call uses 128K-200K even on 1M-token models.
//
// Sources, as of 2026-11: DeepSeek pricing page (V4.1 Flash, V4 Pro), Kimi
// platform docs (K3, K3.1 Pro), OpenAI model page (GPT-6.2 Sol, which bills
// requests over 272K input tokens at long-context rates), Claude docs (Opus 5.5).
type AnalysisModelPreset struct {
	ID            string `json:"id"`
	Name          string `json:"name"`
	ContextWindow int    `json:"context_window"`
	// MaxOutput is the provider's output ceiling; 0 means not published.
	MaxOutput     int    `json:"max_output"`
	InputBudget   int    `json:"input_budget"`
	OutputReserve int    `json:"output_reserve"`
	Concurrency   int    `json:"concurrency"`
	Thinking      string `json:"thinking"`
	// TokenParam is the request field that limits output for this model.
	TokenParam string `json:"token_param"`
	pattern    *regexp.Regexp
}

// Ordered from specific to general: kimi-k3.1-pro must match before kimi-k3.
var analysisModelPresets = []AnalysisModelPreset{
	{ID: "deepseek-v4.1-flash", Name: "DeepSeek V4.1 Flash", ContextWindow: 1_000_000, MaxOutput: 384_000, InputBudget: 131_072, OutputReserve: 16_384, Concurrency: 4, Thinking: "default_on", TokenParam: "max_tokens",
		pattern: regexp.MustCompile(`^deepseek-(flash|v4[.-]1-flash|v4-flash)(\b|[-.]|$)`)},
	{ID: "deepseek-v4-pro", Name: "DeepSeek V4 Pro", ContextWindow: 1_000_000, MaxOutput: 384_000, InputBudget: 163_840, OutputReserve: 16_384, Concurrency: 2, Thinking: "default_on", TokenParam: "max_tokens",
		pattern: regexp.MustCompile(`^deepseek-v4-pro(\b|[-.]|$)`)},
	{ID: "kimi-k3.1-pro", Name: "Kimi K3.1 Pro", ContextWindow: 1_048_576, InputBudget: 163_840, OutputReserve: 16_384, Concurrency: 2, Thinking: "effort", TokenParam: "max_completion_tokens",
		pattern: regexp.MustCompile(`^kimi-k3[.-]1-pro(\b|[-.]|$)`)},
	{ID: "kimi-k3", Name: "Kimi K3", ContextWindow: 1_048_576, MaxOutput: 131_072, InputBudget: 131_072, OutputReserve: 16_384, Concurrency: 2, Thinking: "always", TokenParam: "max_completion_tokens",
		pattern: regexp.MustCompile(`^kimi-k3($|-)`)},
	{ID: "gpt-6.2-sol", Name: "GPT-6.2 Sol", ContextWindow: 1_050_000, MaxOutput: 128_000, InputBudget: 204_800, OutputReserve: 16_384, Concurrency: 2, Thinking: "reasoning", TokenParam: "max_completion_tokens",
		pattern: regexp.MustCompile(`^gpt-6[.-]2-sol(\b|[-.]|$)`)},
	{ID: "claude-opus-5.5", Name: "Claude Opus 5.5", ContextWindow: 1_000_000, MaxOutput: 128_000, InputBudget: 204_800, OutputReserve: 16_384, Concurrency: 2, Thinking: "adaptive", TokenParam: "max_tokens",
		pattern: regexp.MustCompile(`^claude-opus-5[.-]5(\b|[-.]|$)`)},
}

var (
	analysisModelDateSuffix = regexp.MustCompile(`-(\d{8}|\d{4}|latest)$`)
	analysisModelModifiers  = regexp.MustCompile(`@.*$`)
)

// normalizeAnalysisModelID removes what gateways add around a model name:
// letter case, vendor prefixes ("openai/gpt-6.2-sol"), new-api modifiers
// ("@key:value") and a trailing snapshot date.
func normalizeAnalysisModelID(id string) string {
	id = strings.ToLower(strings.TrimSpace(id))
	id = analysisModelModifiers.ReplaceAllString(id, "")
	if index := strings.LastIndex(id, "/"); index >= 0 {
		id = id[index+1:]
	}
	id = strings.TrimSuffix(id, ":free")
	for {
		trimmed := analysisModelDateSuffix.ReplaceAllString(id, "")
		if trimmed == id {
			return id
		}
		id = trimmed
	}
}

// MatchAnalysisModelPreset returns the preset for a model ID, or nil.
func MatchAnalysisModelPreset(id string) *AnalysisModelPreset {
	normalized := normalizeAnalysisModelID(id)
	if normalized == "" {
		return nil
	}
	for i := range analysisModelPresets {
		if analysisModelPresets[i].pattern.MatchString(normalized) {
			preset := analysisModelPresets[i]
			return &preset
		}
	}
	return nil
}

// AnalysisModelPresets lists the built-in presets for the settings page.
func AnalysisModelPresets() []AnalysisModelPreset {
	return slices.Clone(analysisModelPresets)
}

func analysisPresetName(modelName string) string {
	if preset := MatchAnalysisModelPreset(modelName); preset != nil {
		return preset.Name
	}
	return ""
}

func analysisTokenParam(modelName string) string {
	if preset := MatchAnalysisModelPreset(modelName); preset != nil {
		return preset.TokenParam
	}
	return "max_tokens"
}

// analysisConcurrency is the number of evidence packs analysed at once.
func analysisConcurrency(config ErrorAnalysisConfig) int {
	if config.MaxConcurrency > 0 {
		return min(config.MaxConcurrency, 8)
	}
	if preset := MatchAnalysisModelPreset(config.Model); preset != nil {
		return preset.Concurrency
	}
	return 2
}

type AnalysisModelOption struct {
	ID            string               `json:"id"`
	Preset        *AnalysisModelPreset `json:"preset,omitempty"`
	ContextLength int                  `json:"context_length,omitempty"`
	MaxOutput     int                  `json:"max_output,omitempty"`
}

type analysisModelListEntry struct {
	at      time.Time
	options []AnalysisModelOption
}

var analysisModelListCache = struct {
	sync.Mutex
	entries map[string]analysisModelListEntry
}{entries: map[string]analysisModelListEntry{}}

// ListAnalysisModels asks the configured endpoint for its models. A blank key
// uses the saved key only for the saved endpoint, so a key is never sent to a
// host the administrator has not saved it for. Results are cached ten minutes.
func ListAnalysisModels(ctx context.Context, endpoint, key string) ([]AnalysisModelOption, error) {
	endpoint = strings.TrimSpace(endpoint)
	probe := ErrorAnalysisConfig{Endpoint: endpoint, Model: "probe", Timezone: "UTC", MaxAttempts: 1, TimeoutSeconds: 30, IdleSeconds: 10, SampleLimit: 1, ReportLanguage: "en"}
	if err := validateErrorAnalysisConfig(probe); err != nil {
		return nil, err
	}
	if key == "" {
		saved, err := GetErrorAnalysisConfig()
		if err != nil {
			return nil, err
		}
		if saved.Endpoint != endpoint {
			return nil, common.NewMessage("Enter the API key for this endpoint")
		}
		if key, err = analysisAPIKey(saved); err != nil {
			return nil, err
		}
	}
	if len(key) > 4096 || strings.ContainsAny(key, "\r\n") {
		return nil, common.NewMessage("Invalid analysis API key")
	}
	cacheKey := fmt.Sprintf("%x", sha256.Sum256([]byte(endpoint+"\x00"+key)))
	analysisModelListCache.Lock()
	if entry, ok := analysisModelListCache.entries[cacheKey]; ok && time.Since(entry.at) < 10*time.Minute {
		analysisModelListCache.Unlock()
		return slices.Clone(entry.options), nil
	}
	analysisModelListCache.Unlock()

	modelsURL := strings.TrimSuffix(endpoint, "/chat/completions") + "/models"
	ctx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, modelsURL, nil)
	if err != nil {
		return nil, common.NewMessage("Unable to fetch models. Check the endpoint and API key.")
	}
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("Accept", "application/json")
	client := newAnalysisHTTPClient()
	defer client.CloseIdleConnections()
	response, err := client.Do(request)
	if err != nil {
		return nil, common.NewMessage("Unable to fetch models. Check the endpoint and API key.")
	}
	defer response.Body.Close()
	data, err := io.ReadAll(io.LimitReader(response.Body, 4<<20))
	if err != nil || response.StatusCode != http.StatusOK || !gjson.ValidBytes(data) {
		return nil, common.NewMessage("Unable to fetch models. Check the endpoint and API key.")
	}
	options := parseAnalysisModelList(data)
	analysisModelListCache.Lock()
	analysisModelListCache.entries[cacheKey] = analysisModelListEntry{at: time.Now(), options: options}
	for name, entry := range analysisModelListCache.entries {
		if time.Since(entry.at) >= 10*time.Minute {
			delete(analysisModelListCache.entries, name)
		}
	}
	analysisModelListCache.Unlock()
	return slices.Clone(options), nil
}

// parseAnalysisModelList reads OpenAI-style {"data":[{"id":...}]} lists. Some
// gateways (OpenRouter and its clones) also report context_length and
// top_provider.max_completion_tokens; those fill in unrecognised models.
func parseAnalysisModelList(data []byte) []AnalysisModelOption {
	root := gjson.ParseBytes(data)
	items := root.Get("data")
	if !items.IsArray() {
		items = root.Get("models")
	}
	seen := map[string]bool{}
	options := []AnalysisModelOption{}
	items.ForEach(func(_, item gjson.Result) bool {
		id := strings.TrimSpace(item.Get("id").String())
		if id == "" && item.Type == gjson.String {
			id = strings.TrimSpace(item.String())
		}
		if id == "" || len(id) > 200 || seen[id] {
			return true
		}
		seen[id] = true
		option := AnalysisModelOption{ID: id, Preset: MatchAnalysisModelPreset(id)}
		option.ContextLength = int(max(item.Get("context_length").Int(), item.Get("top_provider.context_length").Int()))
		option.MaxOutput = int(item.Get("top_provider.max_completion_tokens").Int())
		options = append(options, option)
		return len(options) < 2000
	})
	order := map[string]int{}
	for i, preset := range analysisModelPresets {
		order[preset.ID] = i
	}
	slices.SortStableFunc(options, func(a, b AnalysisModelOption) int {
		switch {
		case a.Preset != nil && b.Preset == nil:
			return -1
		case a.Preset == nil && b.Preset != nil:
			return 1
		case a.Preset != nil && order[a.Preset.ID] != order[b.Preset.ID]:
			return order[a.Preset.ID] - order[b.Preset.ID]
		}
		return strings.Compare(a.ID, b.ID)
	})
	return options
}

// SuggestedAnalysisBudget gives the settings an unrecognised model should
// start from: a quarter of a reported context (at most 128K), otherwise a
// conservative 32K input and 8K output.
func SuggestedAnalysisBudget(option AnalysisModelOption) (contextWindow, input, output int) {
	if option.Preset != nil {
		return option.Preset.ContextWindow, option.Preset.InputBudget, option.Preset.OutputReserve
	}
	if option.ContextLength >= 16_384 {
		output = 8_192
		if option.MaxOutput > 0 {
			output = min(output, option.MaxOutput)
		}
		return option.ContextLength, min(option.ContextLength/4, 131_072), output
	}
	return 65_536, 32_768, 8_192
}
