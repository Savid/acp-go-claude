package claudeacp

import (
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/stretchr/testify/require"
)

func TestAgentModelMetaAndOptions(t *testing.T) {
	t.Parallel()

	available := []claude.AvailableModelInfo{
		{
			Value:                 "claude-sonnet-4-5",
			DisplayName:           "Sonnet",
			Description:           "balanced",
			SupportedEffortLevels: []string{effortLow, effortHigh, effortHigh, ""},
			SupportsAutoMode:      true,
		},
		{
			Value:       "claude-opus-4-5-1m",
			DisplayName: "Opus 1m",
			Description: "1 million token context",
		},
		{Value: "claude-opus-4-5-1m", DisplayName: "Duplicate Opus"},
		{Value: ""},
	}
	session := &agentSession{
		model:                 "claude-sonnet-4-5",
		availableModels:       available,
		outputStyle:           "default",
		availableOutputStyles: []string{"default", "", "concise", "concise"},
		mode:                  modeAuto,
		effort:                effortHigh,
		fastMode:              true,
		fastModeKnown:         true,
	}

	meta := sessionResponseMeta(session)
	claudeMeta, ok := meta[claudeMetaKey].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "claude-sonnet-4-5", claudeMeta[claudeModelMetaModelIDKey])
	require.Equal(t, effortHigh, claudeMeta[claudeModelMetaVariantKey])
	require.Equal(t, []string{effortLow, effortHigh}, claudeMeta[claudeModelMetaAvailableVariantsKey])
	require.Nil(t, claudeModelVariantMeta("", available, effortLow))

	infoMeta, ok := claudeModelInfoMeta(available[0])[claudeMetaKey].(map[string]any)
	require.True(t, ok)
	require.Equal(t, []string{effortLow, effortHigh}, infoMeta[claudeModelMetaSupportedEffortKey])
	require.Equal(t, true, infoMeta[claudeModelMetaSupportsAutoModeKey])
	require.Equal(t, defaultContextWindow, infoMeta[claudeModelMetaContextWindowKey])
	require.Nil(t, claudeModelInfoMeta(claude.AvailableModelInfo{Value: "unknown"}))
	require.Equal(t, largeContextWindow, modelContextWindowHint(available[1]))
	require.Equal(t, largeContextWindow, modelContextWindowHint(claude.AvailableModelInfo{Description: "has 1m context"}))
	require.Equal(t, largeContextWindow, modelContextWindowHint(claude.AvailableModelInfo{Value: modelTokenOpus}))
	require.Equal(t, claudeModelFamilyHaiku, modelFamily(claude.AvailableModelInfo{Value: "claude-haiku"}))
	require.Equal(t, claudeModelFamilyOpus, modelFamily(claude.AvailableModelInfo{DisplayName: "Opus"}))
	require.Equal(t, "", modelFamily(claude.AvailableModelInfo{Value: "custom"}))
	require.Equal(t, "value display description", modelHintText(claude.AvailableModelInfo{Value: "Value", DisplayName: "Display", Description: "Description"}))
	_, ok = availableModelInfo("missing", available)
	require.False(t, ok)
	foundInfo, ok := availableModelInfo(available[0].Value, available)
	require.True(t, ok)
	require.Equal(t, available[0].Value, foundInfo.Value)
	require.Equal(t, []string{"a", "b"}, nonEmptyModelStrings([]string{"", "a", "a", "b"}))
	require.Equal(t, acp.SessionConfigOptionCategoryModel, *configCategory(acp.SessionConfigOptionCategoryModel))

	options := configOptions(modeAuto, "claude-sonnet-4-5", available, "default", []string{"default", "concise"}, effortHigh, true, true)
	require.Len(t, options, 4)
	require.Equal(t, configModel, options[0].Select.Id)
	require.Equal(t, configMode, options[1].Select.Id)
	require.Equal(t, configOutputStyle, options[2].Select.Id)
	require.Equal(t, configEffort, options[3].Select.Id)

	unstable := unstableConfigOptions(modeAuto, "claude-sonnet-4-5", available, "default", []string{"default"}, effortHigh, true, true)
	require.Len(t, unstable, 4)
	require.Equal(t, configTypeSelect, unstable[0].Select.Type)

	require.Len(t, configSelectOptions("custom", available), 3)
	require.Len(t, configSelectOptions("claude-sonnet-4-5", available), 2)
	require.Len(t, outputStyleSelectOptions("verbose", []string{"default", "", "default"}), 2)
	require.Contains(t, modeSelectOptions("claude-sonnet-4-5", available), acp.SessionConfigSelectOption{Name: modeNameAuto, Value: acp.SessionConfigValueId(modeAuto)})
	require.Nil(t, effortSelectOptions("missing", available, effortHigh))
	require.Contains(t, effortSelectOptions("claude-sonnet-4-5", available, effortMedium), acp.SessionConfigSelectOption{Name: "Medium", Value: acp.SessionConfigValueId(effortMedium)})
	require.Equal(t, []string{effortLow, effortHigh, effortHigh, ""}, effortLevelsForModel("claude-sonnet-4-5", available))
	require.Equal(t, "Extra High", effortDisplayName(effortXHigh))
	require.Equal(t, "Low", effortDisplayName(effortLow))
	require.Equal(t, "Medium", effortDisplayName(effortMedium))
	require.Equal(t, "High", effortDisplayName(effortHigh))
	require.Equal(t, "Max", effortDisplayName(effortMax))
	require.Equal(t, "custom", effortDisplayName("custom"))
	require.Equal(t, "Sonnet", modelDisplayName(available[0]))
	require.Equal(t, "fallback", modelDisplayName(claude.AvailableModelInfo{Value: "fallback"}))
	require.Nil(t, stringPtrIfNotEmpty(""))
	require.Equal(t, "x", *stringPtrIfNotEmpty("x"))
}
