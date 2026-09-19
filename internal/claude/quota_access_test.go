package claude

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestQuotaAccessUsesOnlyEffectiveSetupToken(t *testing.T) {
	env := make([]string, 1, 5)
	env[0] = "CLAUDE_CODE_OAUTH_TOKEN=fixture-token"
	a := quotaAccess(env, false)
	require.NotNil(t, a)
	require.Nil(t, quotaAccess(env, true))
	for _, extra := range []string{"ANTHROPIC_API_KEY=key", "CLAUDE_CODE_USE_VERTEX=1", "ANTHROPIC_BASE_URL=https://other.example", "ANTHROPIC_SMALL_FAST_MODEL=claude-fable-5-1"} {
		require.Nil(t, quotaAccess(append(env, extra), false), extra)
	}
	b := quotaAccess([]string{"CLAUDE_CODE_OAUTH_TOKEN=another-token"}, false)
	require.NotEqual(t, a.Key(), b.Key())
	require.True(t, quotaSettingsMatch(json.RawMessage(`{"env":{"CLAUDE_CODE_OAUTH_TOKEN":"fixture-token"}}`), env))
	for _, raw := range []string{`null`, `{"apiKeyHelper":"credential-command"}`, `{"forceLoginMethod":"gateway"}`, `{"env":{"CLAUDE_CODE_OAUTH_TOKEN":"another-token"}}`} {
		require.False(t, quotaSettingsMatch(json.RawMessage(raw), env), raw)
	}
}
