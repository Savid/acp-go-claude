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
	_, _, err = parseModelConfig(`{bad`)
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
		envClaudeModelConfig: `{"modelOverrides":{"opus":"claude-opus"},"availableModels":["opus"]}`,
	})
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, map[string]string{"opus": "claude-opus"}, config.ModelOverrides)
	require.Equal(t, []string{"opus"}, config.AvailableModels)
}

func TestApplyAvailableModelsAllowlist(t *testing.T) {
	t.Parallel()

	available := []claude.AvailableModelInfo{
		{Value: modelDefault, DisplayName: "Built In Default"},
		{
			Value:                 "opus",
			ResolvedModel:         "claude-opus-4-6",
			DisplayName:           "Opus",
			Description:           "Large model",
			SupportedEffortLevels: []string{"low"},
			SupportsAutoMode:      true,
		},
	}

	copied := applyAvailableModelsAllowlist(available, nil)
	require.Equal(t, available, copied)
	copied[0].Value = "changed"
	require.Equal(t, modelDefault, available[0].Value)

	filtered := applyAvailableModelsAllowlist(available, []string{" ", "default", "opus", "opus", "custom"})
	require.Equal(t, []claude.AvailableModelInfo{
		{Value: modelDefault, DisplayName: "Built In Default"},
		{
			Value:                 "opus",
			ResolvedModel:         "claude-opus-4-6",
			DisplayName:           "Opus",
			Description:           "Large model",
			SupportedEffortLevels: []string{"low"},
			SupportsAutoMode:      true,
		},
		{Value: "custom", DisplayName: "custom"},
	}, filtered)
	require.Equal(t, claude.AvailableModelInfo{Value: modelDefault, DisplayName: modeNameDefault}, defaultModelInfo(nil))
}

func TestResolveModelPreferencePreservesExactIdentity(t *testing.T) {
	t.Parallel()
	models := []claude.AvailableModelInfo{
		{Value: "fable", ResolvedModel: "claude-fable-5", DisplayName: "Fable 5", SupportedEffortLevels: []string{"low"}},
		{Value: "claude-fable-5-1", ResolvedModel: "claude-fable-5-1", DisplayName: "Claude Fable 5.1"},
	}
	for _, value := range []string{"fable", "claude-fable-5", "claude-fable-5-1"} {
		resolved := resolveModelPreference(models, value)
		require.NotNil(t, resolved)
		require.Equal(t, value, resolved.Value)
	}
	for _, value := range []string{"", "Fable 5", "FABLE", "claude-fable-5-2", "fable-5", "default", "best"} {
		require.Nil(t, resolveModelPreference(models, value), value)
	}
	resolved := resolveModelPreference(models, "claude-fable-5")
	resolved.SupportedEffortLevels[0] = "changed"
	require.Equal(t, []string{"low"}, models[0].SupportedEffortLevels)
}

func TestModelCatalogRestrictionsCannotWidenConfiguredChoices(t *testing.T) {
	t.Parallel()
	catalog := []claude.AvailableModelInfo{
		{Value: modelDefault, DisplayName: "Default"},
		{Value: "fable", ResolvedModel: "claude-fable-5-1"},
		{Value: "claude-fable-5-1", ContextWindow: 1000000},
		{Value: "claude-opus-5"},
		{Value: "denied", ResolvedModel: "claude-denied", Disabled: true},
	}
	for _, test := range []struct {
		name       string
		configured []string
		native     any
		want       []string
	}{
		{"intersection", []string{"claude-fable-5-1"}, []any{"claude-fable-5-1", "claude-opus-5"}, []string{modelDefault, "claude-fable-5-1"}},
		{"empty configured", []string{}, []any{"claude-opus-5"}, []string{modelDefault}},
		{"empty native", []string{"claude-fable-5-1"}, []any{}, []string{modelDefault}},
		{"invalid native", nil, "invalid", []string{modelDefault}},
		{"exact native alias", []string{"claude-fable-5-1"}, []any{"fable"}, []string{modelDefault, "claude-fable-5-1"}},
		{"version prefix", []string{"claude-fable-5-1"}, []any{"claude-fable-5"}, []string{modelDefault, "claude-fable-5-1"}},
		{"unknown explicit ID", []string{"deployment-id"}, []any{"deployment-id"}, []string{modelDefault, "deployment-id"}},
		{"native refusal", []string{"claude-denied"}, []any{"claude-denied"}, []string{modelDefault}},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			models := reconcileSessionModels(catalog, test.configured, &claude.SettingsSnapshot{
				Effective: map[string]any{settingsFieldAvailableModels: test.native},
			}, nil)
			var values []string
			for _, option := range configSelectOptions(modelDefault, models) {
				values = append(values, string(option.Value))
			}
			require.Equal(t, test.want, values)
			require.True(t, claude.ModelDisabled("claude-denied", models))
		})
	}
}

func TestModelCatalogOverridesDescribeDispatchedTarget(t *testing.T) {
	t.Parallel()
	catalog := []claude.AvailableModelInfo{
		{Value: modelDefault, ResolvedModel: "claude-sonnet-5", ContextWindow: 1000000},
		{Value: "sonnet", ResolvedModel: "claude-sonnet-5", ContextWindow: 1000000, SupportsAutoMode: true, SupportedEffortLevels: []string{"high"}},
		{Value: "claude-fable-5-1", DisplayName: "Fable 5.1", ContextWindow: 2000000, SupportedEffortLevels: []string{"low"}},
		{Value: "blocked", ResolvedModel: "claude-blocked", Disabled: true},
	}
	for _, alias := range []string{modelDefault, "sonnet"} {
		models := reconcileSessionModels(catalog, nil, nil, map[string]string{alias: "claude-fable-5-1"})
		info, ok := availableModelInfo(alias, models)
		require.True(t, ok)
		require.Equal(t, "claude-fable-5-1", info.ResolvedModel)
		require.Equal(t, "Fable 5.1", info.DisplayName)
		require.Equal(t, int64(2000000), info.ContextWindow)
		require.Equal(t, []string{"low"}, info.SupportedEffortLevels)
		require.False(t, info.SupportsAutoMode)
	}
	models := reconcileSessionModels(catalog, nil, nil, map[string]string{"sonnet": "unknown-target"})
	info, ok := availableModelInfo("sonnet", models)
	require.True(t, ok)
	require.Zero(t, info.ContextWindow)
	require.Empty(t, info.SupportedEffortLevels)
	require.False(t, info.SupportsAutoMode)
	models = reconcileSessionModels(catalog, nil, nil, map[string]string{"sonnet": "claude-blocked"})
	require.True(t, claude.ModelDisabled("sonnet", models))
	models = reconcileSessionModels(catalog, nil, &claude.SettingsSnapshot{
		Effective: map[string]any{settingsFieldAvailableModels: []any{"sonnet"}},
	}, map[string]string{"sonnet": "claude-fable-5-1"})
	require.True(t, claude.ModelDisabled("sonnet", models), "an override cannot evade a native allowlist")
}
