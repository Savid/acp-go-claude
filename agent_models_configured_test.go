package claudeacp

import (
	"testing"

	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/stretchr/testify/require"
)

func TestConfiguredModelsRefuseMalformedIDs(t *testing.T) {
	for _, tc := range []struct {
		name string
		ids  []string
		want string
	}{
		{name: "empty", ids: []string{""}, want: "is not a model id"},
		{name: "surrounding space", ids: []string{" vendor/qwen"}, want: "is not a model id"},
		{name: "duplicate", ids: []string{"vendor/qwen", "vendor/qwen"}, want: "listed twice"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.ErrorContains(t, NewAgent(WithConfiguredModels(tc.ids)).configurationErr, tc.want)
		})
	}

	require.NoError(t, NewAgent(WithConfiguredModels([]string{"vendor/qwen", "vendor/deepseek"})).configurationErr)
}

// TestHostListedModelsArePublishedOnEveryRoute pins the host-listed entry: it
// follows the native rows on a first-party route and on a gateway route alike,
// while dispatch identity still withholds a host-listed Anthropic identity
// where Claude's own names are withheld.
func TestHostListedModelsArePublishedOnEveryRoute(t *testing.T) {
	native := []any{
		map[string]any{"value": "sonnet", "resolvedModel": "claude-sonnet-5", "displayName": "Sonnet"},
		map[string]any{"value": "vendor/qwen", "displayName": "Qwen", "supportedEffortLevels": []any{"low", "high"}},
		map[string]any{"value": "vendor/old", "displayName": "Old", "disabled": true},
	}
	hostListed := []string{"vendor/deepseek", "vendor/qwen", "vendor/old", "claude-opus-5"}

	for _, tc := range []struct {
		name        string
		environment map[string]string
		want        []string
		withheld    []string
	}{
		{
			name: "first party",
			want: []string{"sonnet", "vendor/qwen", "vendor/deepseek", "claude-opus-5"},
		},
		{
			name:        "gateway route",
			environment: map[string]string{"ANTHROPIC_BASE_URL": "http://127.0.0.1:4000"},
			want:        []string{"vendor/qwen", "vendor/deepseek"},
			withheld:    []string{"claude-opus-5"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			transport := newFakeClaudeTransport()
			transport.initialize["models"] = native
			transport.environment = tc.environment
			agent, _, _ := newFakeLifecycleAgent(t, transport, WithConfiguredModels(hostListed))

			resp, err := agent.NewSession(t.Context(), NewSessionRequest(t.TempDir()))
			require.NoError(t, err)

			values := catalogModelValues(t, resp.ConfigOptions)
			for _, id := range tc.want {
				require.Contains(t, values, id)
			}

			for _, id := range tc.withheld {
				require.NotContains(t, values, id)
			}

			// A row Claude refused stays refused however the host lists it.
			require.NotContains(t, values, "vendor/old")

			for _, option := range *catalogModelOption(t, resp.ConfigOptions).Options.Ungrouped {
				switch option.Value {
				case "vendor/qwen":
					require.Equal(t, "Qwen", option.Name, "the native row stands and the host entry adds nothing")
					require.NotEmpty(t, option.Meta)
				case "vendor/deepseek":
					require.Equal(t, "vendor/deepseek", option.Name)
					require.Empty(t, option.Meta, "a host-listed row carries no invented facts")
				}
			}
		})
	}
}

func TestAppendHostListedModelsKeepsNativeOrderAndRefusals(t *testing.T) {
	native := []claude.AvailableModelInfo{
		{Value: "sonnet", ResolvedModel: "claude-sonnet-5"},
		{Value: "vendor/old", Disabled: true},
	}

	models := appendHostListedModels(native, []string{"Sonnet", "vendor/old", "vendor/new"}, false)
	require.Equal(t, []string{"sonnet", "vendor/old", "vendor/new"}, modelValues(models))
	require.True(t, models[1].Disabled)

	withheld := appendHostListedModels(native, []string{"opus", "claude-haiku-4-5", "vendor/new"}, true)
	require.Equal(t, []string{"sonnet", "vendor/old", "vendor/new"}, modelValues(withheld))

	require.Equal(t, native, appendHostListedModels(native, nil, true))
}

func modelValues(models []claude.AvailableModelInfo) []string {
	values := make([]string, 0, len(models))
	for _, model := range models {
		values = append(values, model.Value)
	}

	return values
}
