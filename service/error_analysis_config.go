package service

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/QuantumNous/new-api/common"
	"github.com/QuantumNous/new-api/model"
	"gorm.io/gorm"
)

// The suffix also excludes this option from the generic options API.
const errorAnalysisConfigOption = "ErrorAnalysisConfigSecret"

type ErrorAnalysisConfig struct {
	Version             int64  `json:"version"`
	Enabled             bool   `json:"enabled"`
	Endpoint            string `json:"endpoint"`
	Model               string `json:"model"`
	Timezone            string `json:"timezone"`
	Hour                int    `json:"hour"`
	MaxAttempts         int    `json:"max_attempts"`
	TimeoutSeconds      int    `json:"timeout_seconds"`
	IdleSeconds         int    `json:"idle_seconds"`
	SampleLimit         int    `json:"sample_limit"`
	ReportLanguage      string `json:"report_language"`
	APIKeyConfigured    bool   `json:"api_key_configured"`
	EncryptedKey        string `json:"encrypted_key,omitempty"`
	ContextWindowTokens int    `json:"context_window_tokens"`
	ChunkInputTokens    int    `json:"chunk_input_tokens"`
	MaxOutputTokens     int    `json:"max_output_tokens"`
	SafetyMarginTokens  int    `json:"safety_margin_tokens"`
	MaxCalls            int    `json:"max_calls"`
}

func GetErrorAnalysisConfig() (ErrorAnalysisConfig, error) {
	config := ErrorAnalysisConfig{Endpoint: "https://api.bob-api.com/v1/chat/completions", Model: "deepseek-v4.1-flash", Timezone: "Asia/Shanghai", Hour: 3, MaxAttempts: 3, TimeoutSeconds: 600, IdleSeconds: 60, SampleLimit: 60, ReportLanguage: "zh", ContextWindowTokens: 32768, ChunkInputTokens: 8000, MaxOutputTokens: 2048, SafetyMarginTokens: 4096, MaxCalls: 100}
	var option model.Option
	err := model.DB.Where(map[string]any{"key": errorAnalysisConfigOption}).Take(&option).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return config, nil
	}
	if err != nil {
		return config, err
	}
	if err := common.UnmarshalJsonStr(option.Value, &config); err != nil {
		return config, common.NewMessage("Invalid error analysis configuration")
	}
	config = normalizeAnalysisBudgets(config)
	config.APIKeyConfigured = config.EncryptedKey != ""
	return config, nil
}

func validateErrorAnalysisConfig(config ErrorAnalysisConfig) error {
	config = normalizeAnalysisBudgets(config)
	u, err := url.Parse(config.Endpoint)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || !strings.HasSuffix(u.Path, "/chat/completions") || len(config.Endpoint) > 1000 {
		return common.NewMessage("Use an HTTPS chat completions endpoint without credentials or query parameters")
	}
	if strings.TrimSpace(config.Model) == "" || len(config.Model) > 200 || config.Hour < 0 || config.Hour > 23 || config.MaxAttempts < 1 || config.MaxAttempts > 5 || config.TimeoutSeconds < 30 || config.TimeoutSeconds > 600 || config.IdleSeconds < 10 || config.IdleSeconds > 600 || config.SampleLimit < 1 || config.SampleLimit > 200 {
		return common.NewMessage("Invalid error analysis configuration")
	}
	if config.ContextWindowTokens < 4096 || config.ContextWindowTokens > 2000000 || config.ChunkInputTokens < 256 || config.ChunkInputTokens > 2000000 || config.MaxOutputTokens < 128 || config.MaxOutputTokens > 32768 || config.SafetyMarginTokens < 256 || config.SafetyMarginTokens >= config.ContextWindowTokens || config.MaxOutputTokens+config.SafetyMarginTokens+512 >= config.ContextWindowTokens || config.MaxCalls < 1 || config.MaxCalls > 1000 {
		return common.NewMessage("Invalid analysis token budget")
	}
	if _, err := time.LoadLocation(config.Timezone); err != nil {
		return common.NewMessage("Invalid analysis timezone")
	}
	switch config.ReportLanguage {
	case "zh", "zh-TW", "en", "fr", "ja", "ru", "vi":
	default:
		return common.NewMessage("Invalid report language")
	}
	return nil
}

func analysisKeyCipher() (cipher.AEAD, error) {
	secret := os.Getenv("CRYPTO_SECRET")
	if secret == "" {
		secret = os.Getenv("SESSION_SECRET")
	}
	if len(secret) < 16 {
		return nil, common.NewMessage("Set a persistent CRYPTO_SECRET of at least 16 characters before saving an analysis API key")
	}
	key := sha256.Sum256([]byte(secret + ":error-analysis"))
	block, err := aes.NewCipher(key[:])
	if err != nil {
		return nil, err
	}
	return cipher.NewGCMWithRandomNonce(block)
}

func analysisAPIKey(config ErrorAnalysisConfig) (string, error) {
	if config.EncryptedKey == "" {
		return "", common.NewMessage("Configure an analysis API key first")
	}
	aead, err := analysisKeyCipher()
	if err != nil {
		return "", err
	}
	data, err := base64.StdEncoding.DecodeString(config.EncryptedKey)
	if err != nil {
		return "", common.NewMessage("Unable to decrypt analysis API key; save the key again")
	}
	clear, err := aead.Open(nil, nil, data, []byte(errorAnalysisConfigOption))
	if err != nil {
		return "", common.NewMessage("Unable to decrypt analysis API key; save the key again")
	}
	return string(clear), nil
}

func SaveErrorAnalysisConfig(config ErrorAnalysisConfig, key string, clearKey bool) error {
	config = normalizeAnalysisBudgets(config)
	if err := validateErrorAnalysisConfig(config); err != nil {
		return err
	}
	old, err := GetErrorAnalysisConfig()
	if err != nil {
		return err
	}
	if old.Version != config.Version {
		return common.NewMessage("Analysis configuration changed; reload before saving")
	}
	config.EncryptedKey = old.EncryptedKey
	if clearKey {
		config.EncryptedKey = ""
	}
	if key != "" {
		if len(key) > 4096 || strings.ContainsAny(key, "\r\n") {
			return common.NewMessage("Invalid analysis API key")
		}
		aead, err := analysisKeyCipher()
		if err != nil {
			return err
		}
		config.EncryptedKey = base64.StdEncoding.EncodeToString(aead.Seal(nil, nil, []byte(key), []byte(errorAnalysisConfigOption)))
	}
	if config.Enabled && config.EncryptedKey == "" {
		return common.NewMessage("Configure an analysis API key first")
	}
	config.Version++
	config.APIKeyConfigured = config.EncryptedKey != ""
	raw, err := common.Marshal(config)
	if err != nil {
		return err
	}
	// Compare the stored version in application code under the existing row
	// lock helper, keeping SQLite/MySQL/PostgreSQL behavior identical.
	return model.SaveErrorAnalysisOption(errorAnalysisConfigOption, string(raw), old.Version)
}
