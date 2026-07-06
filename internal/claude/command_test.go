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

func TestBuildArgs(t *testing.T) {
	t.Parallel()

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
		AddDirs:                 []string{"/repo", ""},
		MCPConfigJSON:           `{"mcpServers":{}}`,
		JSONSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"ok": map[string]any{"type": "boolean"}},
		},
	})

	require.Equal(t, []string{
		"--output-format", "stream-json",
		"--input-format", "stream-json",
		"--include-partial-messages",
		"--verbose",
		"--include-hook-events",
		"--session-mirror",
		"--bare",
		"--permission-mode", "default",
		"--allow-dangerously-skip-permissions",
		"--permission-prompt-tool", "stdio",
		"--model", "claude-test",
		"--system-prompt", "system",
		"--json-schema", `{"properties":{"ok":{"type":"boolean"}},"type":"object"}`,
		"--mcp-config", `{"mcpServers":{}}`,
		"--strict-mcp-config",
		"--setting-sources=user,project,local",
		"--add-dir", "/repo",
		"--session-id", "session-1",
	}, args)
}

func TestBuildArgsInvalidJSONSchemaFallsBackToEmptyObject(t *testing.T) {
	t.Parallel()

	args := BuildArgs(Options{JSONSchema: map[string]any{"bad": func() {}}})

	require.Contains(t, args, "--json-schema")
	require.Contains(t, args, "{}")
}

func TestBuildArgsSettingSources(t *testing.T) {
	t.Parallel()

	require.NotContains(t, BuildArgs(Options{}), "--setting-sources=")
	require.Contains(t, BuildArgs(Options{SettingSources: []string{}}), "--setting-sources=")
	require.Contains(t, BuildArgs(Options{SettingSources: []string{"project"}}), "--setting-sources=project")
}

func TestBuildArgsSettingsFile(t *testing.T) {
	t.Parallel()

	require.NotContains(t, BuildArgs(Options{}), "--settings")

	args := BuildArgs(Options{SettingsFile: "/tmp/home/wagie.settings.json"})

	index := slices.Index(args, "--settings")
	require.GreaterOrEqual(t, index, 0)
	require.Less(t, index+1, len(args))
	require.Equal(t, "/tmp/home/wagie.settings.json", args[index+1])
}

func TestBuildArgsResumeTakesPrecedenceOverSessionID(t *testing.T) {
	t.Parallel()

	args := BuildArgs(Options{SessionID: "new", ResumeID: "old", ForkSession: true})

	require.Contains(t, args, "--resume")
	require.Contains(t, args, "old")
	require.Contains(t, args, "--fork-session")
	require.Contains(t, args, "--session-id")
	require.Contains(t, args, "new")
}

func TestBuildEnv(t *testing.T) {
	t.Setenv("CLAUDE_CONFIG_DIR", "/process/claude-home")
	t.Setenv("CLAUDE_CODE_ENTRYPOINT", "process-entrypoint")
	t.Setenv("CLAUDECODE", "nested")
	t.Setenv("PWD", "/process/cwd")

	env := BuildEnv(Options{
		ClaudeHome: "/tmp/claude-home",
		Cwd:        "/repo",
		Env: map[string]string{
			"CLAUDE_CONFIG_DIR": "/override/claude-home",
			"CLAUDECODE":        "explicit-nested",
			"PWD":               "/override/cwd",
			"X_TEST":            "1",
		},
	})

	require.Equal(t, 1, countEnvKey(env, "CLAUDE_CONFIG_DIR"))
	require.Equal(t, 1, countEnvKey(env, "CLAUDE_CODE_ENTRYPOINT"))
	require.Equal(t, 1, countEnvKey(env, "PWD"))
	require.Equal(t, 0, countEnvKey(env, "CLAUDECODE"))
	require.Contains(t, env, "CLAUDE_CONFIG_DIR=/override/claude-home")
	require.Contains(t, env, "CLAUDE_CODE_ENTRYPOINT=acp-go-claude")
	require.Contains(t, env, "PWD=/repo")
	require.Contains(t, env, "X_TEST=1")
}

func TestBuildEnvSkipsInvalidProcessEntries(t *testing.T) {
	originalEnviron := commandEnviron
	t.Cleanup(func() {
		commandEnviron = originalEnviron
	})

	commandEnviron = func() []string {
		return []string{"", "BROKEN", "=empty", "GOOD=1"}
	}

	env := BuildEnv(Options{})

	require.Contains(t, env, "GOOD=1")
	require.NotContains(t, env, "")
	require.NotContains(t, env, "BROKEN")
	require.NotContains(t, env, "=empty")
}

func countEnvKey(env []string, key string) int {
	count := 0
	prefix := key + "="
	for _, item := range env {
		if strings.HasPrefix(item, prefix) {
			count++
		}
	}

	return count
}

func TestDiscover(t *testing.T) {
	t.Parallel()

	path, err := Discover(context.Background(), "/custom/claude", nil)
	require.NoError(t, err)
	require.Equal(t, "/custom/claude", path)
}

func TestDiscoverCancelledExplicitPath(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := Discover(ctx, "/custom/claude", nil)
	require.ErrorIs(t, err, context.Canceled)

	_, err = Discover(ctx, "", nil)
	require.ErrorIs(t, err, context.Canceled)
}

func TestDiscoverMissingFromPath(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	_, err := Discover(context.Background(), "", nil)
	require.Error(t, err)
}

func TestParseClaudeVersion(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
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
		require.Equal(t, tc.want, parseClaudeVersion(tc.output), tc.output)
	}
}

func TestCompareSemver(t *testing.T) {
	t.Parallel()

	require.Equal(t, -1, compareSemver("1.9.9", "2.0.0"))
	require.Equal(t, 1, compareSemver("2.1.0", "2.0.9"))
	require.Equal(t, 0, compareSemver("2.0.0", "2.0.0"))
	require.Equal(t, 1, compareSemver("2.1.201", "2.0.0"))
	require.Equal(t, 0, compareSemver("2.0", "2.0.0"))
	require.Equal(t, -1, compareSemver("2", "2.0.1"))
}

func TestValidateClaudeVersion(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses /bin/sh scripts")
	}

	dir := t.TempDir()

	current := writeShellScript(t, filepath.Join(dir, "current"), "#!/bin/sh\necho '2.1.201 (Claude Code)'\n")
	require.NoError(t, validateClaudeVersion(context.Background(), current))

	old := writeShellScript(t, filepath.Join(dir, "old"), "#!/bin/sh\necho '1.9.9 (Claude Code)'\n")
	require.ErrorContains(t, validateClaudeVersion(context.Background(), old), "too old")

	unparseable := writeShellScript(t, filepath.Join(dir, "bad"), "#!/bin/sh\necho 'no version'\n")
	require.ErrorContains(t, validateClaudeVersion(context.Background(), unparseable), "could not parse")

	failing := writeShellScript(t, filepath.Join(dir, "fail"), "#!/bin/sh\nexit 1\n")
	require.Error(t, validateClaudeVersion(context.Background(), failing))
}

func TestDiscoverFromPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses executable mode bits")
	}

	binDir := t.TempDir()
	path := filepath.Join(binDir, "claude")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755))
	t.Setenv("PATH", binDir)

	found, err := Discover(context.Background(), "", nil)
	require.NoError(t, err)
	require.Equal(t, path, found)
}
