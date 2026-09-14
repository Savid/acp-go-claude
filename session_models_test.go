package claudeacp

import (
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/stretchr/testify/require"
)

func TestConfigCatalogAndReadback(t *testing.T) {
	t.Parallel()
	h := newHarness(t, WithConfiguredModels([]string{"haiku", "host-model"}))
	h.initialize()
	session := h.newSession()
	model := session.ConfigOptions[0].Select
	require.Equal(t, configModel, model.Id)
	values := *model.Options.Ungrouped
	ids := make([]acp.SessionConfigValueId, 0, len(values))
	for _, value := range values {
		ids = append(ids, value.Value)
	}
	require.Equal(t, []acp.SessionConfigValueId{"default", "haiku", "host-model"}, ids)
	for _, option := range session.ConfigOptions {
		require.Equal(t, "select", option.Select.Type)
		require.Contains(t, []acp.SessionConfigOptionCategory{acp.SessionConfigOptionCategoryModel, acp.SessionConfigOptionCategoryMode, acp.SessionConfigOptionCategoryThoughtLevel, "model_config"}, *option.Select.Category)
	}
	for _, selection := range []struct {
		id    acp.SessionConfigId
		value string
	}{{configModel, "future-model"}, {configMode, permissionModePlan}, {configModel, "default"}, {configEffort, "high"}, {configOutputStyle, "concise"}} {
		response, err := h.conn.SetSessionConfigOption(h.ctx(), SetConfigOptionRequest(session.SessionId, selection.id, acp.SessionConfigValueId(selection.value)))
		require.NoError(t, err)
		found := false
		for _, option := range response.ConfigOptions {
			if option.Select.Id == selection.id {
				require.Equal(t, acp.SessionConfigValueId(selection.value), option.Select.CurrentValue)
				found = true
			}
		}
		require.True(t, found)
	}
}

func TestModelCatalogPreservesNativeIdentity(t *testing.T) {
	t.Parallel()
	values := modelSelectOptions("current", []claude.Model{{Value: "one", DisplayName: "One", SupportedEffortLevels: []string{"high"}}, {Value: "one"}}, []string{"one", "listed"})
	require.Len(t, values, 3)
	require.Equal(t, acp.SessionConfigValueId("one"), values[0].Value)
	require.Equal(t, "One", values[0].Name)
	require.Equal(t, acp.SessionConfigValueId("listed"), values[1].Value)
	require.Equal(t, acp.SessionConfigValueId("current"), values[2].Value)
	meta, ok := values[0].Meta[vendor].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "one", meta["modelId"])
}
