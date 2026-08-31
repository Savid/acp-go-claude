package claude

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBuildArgsCurrentSurface(t *testing.T) {
	args := BuildArgs(Options{SessionID: "session", Model: "sonnet", MCPConfigPath: "/tmp/mcp.json", SettingsFile: "overlay.json", ExtraPathDirs: []string{"/tools"}})
	require.Contains(t, args, "--session-id")
	require.Contains(t, args, "--model")
	require.Contains(t, args, "--mcp-config")
	require.Contains(t, args, "--strict-mcp-config")
	require.Contains(t, args, "--settings")
}

func TestBuildEnvUsesAuthorityBaseAndCanonicalOverlay(t *testing.T) {
	authority := &NativeAuthority{NativeEnvironment: func() map[string]string {
		return map[string]string{"PATH": "/native/bin", "BASE": "one", "HOME": "/host/home"}
	}}
	environment := BuildEnv(Options{Cwd: "/work", ClaudeHome: "/native/home", Env: map[string]string{"OVERLAY": "two", "HOME": "/ignored"}, Authority: authority, ExtraPathDirs: []string{"/shim"}})
	require.Contains(t, environment, "BASE=one")
	require.Contains(t, environment, "OVERLAY=two")
	require.Contains(t, environment, "CLAUDE_CONFIG_DIR=/native/home")
	require.Contains(t, environment, "PATH=/shim:/native/bin")
	require.Contains(t, environment, "HOME=/host/home")
}

func TestParseAndCompareClaudeVersion(t *testing.T) {
	version := parseClaudeVersion("Claude Code 2.1.3")
	require.Equal(t, "2.1.3", version)
	require.GreaterOrEqual(t, compareSemver(version, minClaudeVersion), 0)
	require.Empty(t, parseClaudeVersion("not claude"))
}
