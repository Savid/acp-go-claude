package claudeacp

import (
	"testing"

	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/stretchr/testify/require"
)

func TestSessionModelsUseEffectiveNativeEnvironment(t *testing.T) {
	t.Setenv(envAnthropicModel, "adapter-model")
	t.Setenv(envClaudeModelConfig, "invalid adapter config")

	for _, test := range []struct {
		name    string
		base    map[string]string
		overlay map[string]string
		model   string
		models  int
	}{
		{name: "authority", base: map[string]string{envAnthropicModel: "opus", envClaudeModelConfig: `{"availableModels":["opus"]}`}, model: "opus", models: 2},
		{name: "adapter excluded", base: map[string]string{}, model: "sonnet", models: 2},
		{name: "empty override", base: map[string]string{envAnthropicModel: "opus", envClaudeModelConfig: "invalid authority config"}, overlay: map[string]string{envAnthropicModel: "", envClaudeModelConfig: ""}, model: "sonnet", models: 2},
	} {
		t.Run(test.name, func(t *testing.T) {
			authority := residualCallbackAuthority()
			authority.environment = func() map[string]string { return cloneStringMap(test.base) }
			agent, _, _ := newFakeLifecycleAgent(t, newFakeClaudeTransport(), WithHostAuthority(authority), WithEnv(test.overlay))
			t.Cleanup(func() { require.NoError(t, agent.Close()) })
			response, err := agent.NewSession(t.Context(), NewSessionRequest(t.TempDir()))
			require.NoError(t, err)
			session, err := agent.session(response.SessionId)
			require.NoError(t, err)
			require.Equal(t, test.model, session.currentModel())
			require.Len(t, session.availableModels, test.models)
			if test.name == "authority" {
				require.Equal(t, modelDefault, session.availableModels[0].Value)
				require.Equal(t, "opus", session.availableModels[1].Value)
			}
		})
	}
}

func TestEffectiveNativeEnvironmentPreservesOverlayPrecedence(t *testing.T) {
	previousPlatform := claude.Platform
	t.Cleanup(func() { claude.Platform = previousPlatform })
	for _, platform := range []string{"linux", "windows"} {
		t.Run(platform, func(t *testing.T) {
			claude.Platform = platform
			agent := NewAgent(WithHostAuthority(newFakeHostAuthority()))
			overlay := mergeEnv(map[string]string{"token": "static"}, map[string]string{"TOKEN": ""})
			environment := agent.effectiveNativeEnvironment(overlay)
			require.Empty(t, environment["TOKEN"])
			if platform == "windows" {
				require.NotContains(t, environment, "token")
				require.True(t, providerAuthSettingsContentConfigured([]byte(`{"env":{"anthropic_api_key":"configured"}}`)))
			} else {
				require.Equal(t, "static", environment["token"])
				require.False(t, providerAuthSettingsContentConfigured([]byte(`{"env":{"anthropic_api_key":"configured"}}`)))
			}
			require.NoError(t, agent.Close())
		})
	}
}
