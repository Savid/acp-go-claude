package claude

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
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

	path, err = Discover(context.Background(), "", map[string]string{EnvClaudeCodeExecutable: "/env/claude"})
	require.NoError(t, err)
	require.Equal(t, "/env/claude", path)
}

func TestDiscoverFromProcessEnv(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	t.Setenv(EnvClaudeCodeExecutable, "/process/claude")

	path, err := Discover(context.Background(), "", nil)
	require.NoError(t, err)
	require.Equal(t, "/process/claude", path)
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
