package claude

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestOrdinaryEnvironmentIsSanitizedAmbientCapture(t *testing.T) {
	require.Equal(t, map[string]string{
		"PATH":              "/usr/bin",
		"ANTHROPIC_API_KEY": "ambient-key",
	}, OrdinaryEnvironment([]string{
		"PATH=/usr/bin",
		"ANTHROPIC_API_KEY=ambient-key",
		"GOTRACEBACK=crash",
		"CLAUDE_CODE_CUSTOM_OAUTH_URL=https://example.invalid",
		"TERM_PROGRAM=terminal",
		envClaudeCodeNested + "=1",
		privateAdapterEnvPrefix + "MODE=private",
		"NUL_VALUE=bad\x00value",
		"=empty-key",
		"malformed-entry",
	}))
}

func TestOrdinaryEnvironmentUsesPlatformKeyIdentity(t *testing.T) {
	lower := strings.ToLower(envClaudeCodeNested)
	captured := OrdinaryEnvironment([]string{lower + "=1"})
	if EnvironmentKey(lower) == EnvironmentKey(envClaudeCodeNested) {
		require.Empty(t, captured)
	} else {
		require.Equal(t, map[string]string{lower: "1"}, captured)
	}
}

func TestResolveOrdinaryExecutableRefusesMissingSelectorAndStaticPath(t *testing.T) {
	_, err := resolveOrdinaryExecutable("  ", nil)
	require.ErrorContains(t, err, "empty")

	_, err = resolveOrdinaryExecutable("definitely-not-an-acp-go-claude-command", []string{"PATH=" + t.TempDir()})
	require.Error(t, err)
}
