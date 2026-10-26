package model

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestValidatePerformanceHealthConfigRejectsOverlappingModels(t *testing.T) {
	config := PerformanceHealthConfig{Companies: []PerformanceCompany{
		{ID: "openai", Name: "OpenAI", Groups: []PerformanceModelGroup{{ID: "general", Name: "General", Models: []string{"gpt-4.1"}}}},
		{ID: "backup", Name: "Backup", Groups: []PerformanceModelGroup{{ID: "fallback", Name: "Fallback", Models: []string{"gpt-4.1"}}}},
	}}

	err := validatePerformanceHealthConfig(config)
	require.ErrorContains(t, err, "assigned to both")
}

func TestValidatePerformanceHealthConfigNormalizesModelsAndDefaultsStatus(t *testing.T) {
	config := PerformanceHealthConfig{Companies: []PerformanceCompany{{
		ID:     "openai",
		Name:   "OpenAI",
		Groups: []PerformanceModelGroup{{ID: "general", Name: "General", Models: []string{" gpt-4.1 ", "gpt-4.1", ""}}},
	}}}

	require.NoError(t, validatePerformanceHealthConfig(config))
	require.Equal(t, "active", config.Companies[0].Status)
	require.Equal(t, []string{"gpt-4.1"}, config.Companies[0].Groups[0].Models)
}
