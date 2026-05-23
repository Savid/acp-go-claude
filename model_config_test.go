package claudeacp

import (
	"testing"

	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/stretchr/testify/require"
)

func TestParseModelConfigErrorsAndEmpty(t *testing.T) {
	t.Parallel()

	config, ok, err := parseModelConfig("  ")
	require.NoError(t, err)
	require.False(t, ok)
	require.Empty(t, config)

	_, _, err = parseModelConfig(`[]`)
	require.Error(t, err)

	_, _, err = parseModelConfig(`{"modelOverrides":[]}`)
	require.Error(t, err)

	_, _, err = parseModelConfig(`{"modelOverrides":{"opus":4}}`)
	require.Error(t, err)

	_, _, err = parseModelConfig(`{"availableModels":{}}`)
	require.Error(t, err)

	_, _, err = parseModelConfig(`{"availableModels":[4]}`)
	require.Error(t, err)

	config, ok, err = parseModelConfig(`{"modelOverrides":{}}`)
	require.NoError(t, err)
	require.False(t, ok)
	require.Empty(t, config)

	config, ok, err = modelConfigFromEnv(map[string]string{
		envClaudeModelConfig: `{"availableModels":["opus"]}`,
	})
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, []string{"opus"}, config.AvailableModels)
}

func TestApplyAvailableModelsAllowlist(t *testing.T) {
	t.Parallel()

	available := []claude.AvailableModelInfo{
		{
			Value:                 "claude-opus-4-6",
			DisplayName:           "Opus",
			Description:           "Large model",
			SupportedEffortLevels: []string{"low"},
			SupportsAutoMode:      true,
		},
	}

	copied := applyAvailableModelsAllowlist(available, nil)
	require.Equal(t, available, copied)
	copied[0].Value = "changed"
	require.Equal(t, "claude-opus-4-6", available[0].Value)

	filtered := applyAvailableModelsAllowlist(available, []string{" ", "default", "opus", "opus", "custom"})
	require.Equal(t, []claude.AvailableModelInfo{
		{Value: modelDefault, DisplayName: "Default"},
		{
			Value:                 "opus",
			DisplayName:           "Opus",
			Description:           "Large model",
			SupportedEffortLevels: []string{"low"},
			SupportsAutoMode:      true,
		},
		{Value: "custom", DisplayName: "custom"},
	}, filtered)
}

func TestResolveModelPreferenceAndScoring(t *testing.T) {
	t.Parallel()

	models := []claude.AvailableModelInfo{
		{
			Value:                 "claude-opus-4-6-1m",
			DisplayName:           "Opus 1m",
			SupportedEffortLevels: []string{"low"},
		},
		{Value: "claude-sonnet-4-5", DisplayName: "Sonnet"},
	}

	require.Nil(t, resolveModelPreference(models, " "))
	require.Equal(t, "claude-sonnet-4-5", resolveModelPreference(models, "Sonnet").Value)
	require.Equal(t, "claude-opus-4-6-1m", resolveModelPreference(models, "OPUS").Value)
	require.Nil(t, resolveModelPreference(models, modelDefault))
	require.Equal(t, "claude-opus-4-6-1m", resolveModelPreference(models, "Claude OpusPlan [1m]").Value)

	tokens, contextHint := tokenizeModelPreference("Claude OpusPlan best 123 [1m]")
	require.Equal(t, []string{"opus", "1m"}, tokens)
	require.Equal(t, "1m", contextHint)
	require.Equal(t, 4, scoreModelMatch(models[0], []string{"opus", "1m", "missing"}, "1m"))
}
