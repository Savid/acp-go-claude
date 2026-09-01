package claude

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBuildArgsExactCurrentSurface(t *testing.T) {
	args := BuildArgs(Options{
		SessionID:               "session-1",
		Model:                   "claude-test",
		SystemText:              "system",
		PermissionMode:          "default",
		PermissionPromptTool:    "stdio",
		AllowSkipPermissionsArg: true,
		SessionMirror:           true,
		Bare:                    true,
		SettingSources:          []string{"user", "project", "local"},
		SettingsFile:            "/tmp/settings.json",
		AddDirs:                 []string{"/repo", ""},
		MCPConfigPath:           "/tmp/mcp.json",
		JSONSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"ok": map[string]any{"type": "boolean"}},
		},
	})

	require.Equal(t, []string{
		"--output-format", "stream-json", "--input-format", "stream-json",
		"--include-partial-messages", "--verbose", "--include-hook-events",
		"--session-mirror", "--bare", "--permission-mode", "default",
		"--allow-dangerously-skip-permissions", "--permission-prompt-tool", "stdio",
		"--model", "claude-test", "--system-prompt", "system",
		"--json-schema", `{"properties":{"ok":{"type":"boolean"}},"type":"object"}`,
		"--mcp-config", "/tmp/mcp.json", "--strict-mcp-config",
		"--setting-sources=user,project,local", "--settings", "/tmp/settings.json",
		"--add-dir", "/repo", "--session-id", "session-1",
	}, args)
}

func TestBuildArgsCurrentEdgeCases(t *testing.T) {
	require.Contains(t, BuildArgs(Options{JSONSchema: map[string]any{"bad": func() {}}}), "{}")
	require.NotContains(t, BuildArgs(Options{}), "--setting-sources=")
	require.Contains(t, BuildArgs(Options{SettingSources: []string{}}), "--setting-sources=")

	args := BuildArgs(Options{SessionID: "new", ResumeID: "old", ForkSession: true})
	require.Equal(t, []string{"--resume", "old", "--fork-session", "--session-id", "new"}, args[len(args)-5:])

	withoutBypass := BuildArgs(Options{PermissionMode: "bypassPermissions", AllowSkipPermissionsArg: true})
	require.NotContains(t, withoutBypass, "--allow-dangerously-skip-permissions")
}

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

func TestBuildEnvRejectsUnavailableAndMalformedBases(t *testing.T) {
	t.Parallel()

	require.Nil(t, BuildEnv(Options{Authority: &NativeAuthority{}}))
	require.Nil(t, BuildEnv(Options{OrdinaryEnvironment: nil}))

	for _, environment := range []map[string]string{
		{"": "value"},
		{"BAD=KEY": "value"},
		{"BAD\x00KEY": "value"},
		{"KEY": "bad\x00value"},
	} {
		require.Nil(t, BuildEnv(Options{OrdinaryEnvironment: environment}))
	}
}

func TestBuildEnvUsesOnlySelectedBaseAndProtectsManagedRoots(t *testing.T) {
	separator := string(os.PathListSeparator)
	environment := BuildEnv(Options{
		Cwd:                 "/work",
		ClaudeHome:          "/native/home",
		OrdinaryEnvironment: map[string]string{"PATH": "/base/bin", "HOME": "/base/home", "BASE": "one"},
		Env: map[string]string{
			"PATH":              "/overlay/bin",
			"HOME":              "/ignored/home",
			"XDG_CONFIG_HOME":   "/ignored/xdg",
			"CLAUDE_CONFIG_DIR": "/ignored/claude",
			"OVERLAY":           "two",
			"CLAUDECODE":        "nested",
		},
		ExtraPathDirs: []string{"/shim"},
	})

	require.Contains(t, environment, "BASE=one")
	require.Contains(t, environment, "OVERLAY=two")
	require.Contains(t, environment, "HOME=/base/home")
	require.Contains(t, environment, "CLAUDE_CONFIG_DIR=/native/home")
	require.Contains(t, environment, "PWD=/work")
	require.Contains(t, environment, "PATH=/shim"+separator+"/overlay/bin")
	require.Zero(t, countEnvironmentKey(environment, "CLAUDECODE"))
	require.Equal(t, 1, countEnvironmentKey(environment, "CLAUDE_CONFIG_DIR"))
	require.NotContains(t, environment, "XDG_CONFIG_HOME=/ignored/xdg")
}

func TestBuildEnvPlatformEnvironmentIdentity(t *testing.T) {
	environment := BuildEnv(Options{
		ClaudeHome:          "/native/home",
		OrdinaryEnvironment: map[string]string{"PATH": "/base/bin", "home": "/base/lower"},
		Env:                 map[string]string{"claude_config_dir": "/overlay/lower"},
	})
	folded := EnvironmentKey("home") == EnvironmentKey("HOME")
	require.Equal(t, !folded, slices.Contains(environment, EnvironmentKey("home")+"=/base/lower"))
	require.Equal(t, !folded, slices.Contains(environment, EnvironmentKey("claude_config_dir")+"=/overlay/lower"))
	require.True(t, managedRootEnvKey("CLAUDE_CONFIG_DIR"))
	require.Equal(t, folded, managedRootEnvKey("claude_config_dir"))
}

func countEnvironmentKey(environment []string, key string) int {
	count := 0
	for _, entry := range environment {
		candidate, _, ok := strings.Cut(entry, "=")
		if ok && EnvironmentKey(candidate) == EnvironmentKey(key) {
			count++
		}
	}

	return count
}

func TestParseAndCompareClaudeVersion(t *testing.T) {
	version := parseClaudeVersion("Claude Code 2.1.3")
	require.Equal(t, "2.1.3", version)
	require.GreaterOrEqual(t, compareSemver(version, minClaudeVersion), 0)
	require.Empty(t, parseClaudeVersion("not claude"))
}

func TestClaudeVersionParsingAndComparisonMatrix(t *testing.T) {
	for _, test := range []struct {
		output string
		want   string
	}{
		{"2.1.201 (Claude Code)", "2.1.201"},
		{"claude 2.0.0", "2.0.0"},
		{"1.2.3-beta.1", "1.2.3"},
		{"1.2.3+build.5", "1.2.3"},
		{"", ""},
		{"no version here", ""},
	} {
		require.Equal(t, test.want, parseClaudeVersion(test.output), test.output)
	}

	require.Negative(t, compareSemver("1.9.9", "2.0.0"))
	require.Positive(t, compareSemver("2.1.0", "2.0.9"))
	require.Zero(t, compareSemver("2.0", "2.0.0"))
}

func TestValidateClaudeVersionOrdinaryBoundary(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses shell scripts")
	}

	dir := t.TempDir()
	options := Options{Cwd: dir, OrdinaryEnvironment: OrdinaryEnvironment()}

	options.CLIPath = writeShellScript(t, filepath.Join(dir, "current"), "#!/bin/sh\nprintf '2.1.201 (Claude Code)\\n'\n")
	require.NoError(t, validateClaudeVersion(context.Background(), options))

	options.CLIPath = writeShellScript(t, filepath.Join(dir, "old"), "#!/bin/sh\nprintf '1.9.9 (Claude Code)\\n'\n")
	require.ErrorContains(t, validateClaudeVersion(context.Background(), options), "older")

	options.CLIPath = writeShellScript(t, filepath.Join(dir, "bad"), "#!/bin/sh\nprintf 'no version\\n'\n")
	require.ErrorContains(t, validateClaudeVersion(context.Background(), options), "parse")

	options.CLIPath = writeShellScript(t, filepath.Join(dir, "failure"), "#!/bin/sh\nexit 7\n")
	require.ErrorContains(t, validateClaudeVersion(context.Background(), options), "exited 7")

	options.CLIPath = filepath.Join(dir, "missing")
	require.ErrorContains(t, validateClaudeVersion(context.Background(), options), "probe claude version")
}
