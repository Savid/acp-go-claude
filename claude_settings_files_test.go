package claudeacp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/stretchr/testify/require"
)

func TestResolveClaudeSettingsFile(t *testing.T) {
	t.Parallel()

	dir := filepath.FromSlash("/tmp/claude-home")

	resolved, err := resolveClaudeSettingsFile(dir, "custom.settings.json")
	require.NoError(t, err)
	require.Equal(t, filepath.Join(dir, "custom.settings.json"), resolved)

	nested, err := resolveClaudeSettingsFile(dir, "overlays/custom.json")
	require.NoError(t, err)
	require.Equal(t, filepath.Join(dir, "overlays", "custom.json"), nested)
}

func TestResolveClaudeSettingsFileRejectsUnsafePaths(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		key  string
	}{
		{name: "empty", key: ""},
		{name: "absolute", key: "/etc/passwd"},
		{name: "parent escape", key: "../evil.json"},
		{name: "current dir", key: "./custom.json"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			resolved, err := resolveClaudeSettingsFile(filepath.FromSlash("/tmp/claude-home"), tc.key)
			require.Empty(t, resolved)

			field := settingsFileOptionField
			if tc.key != "" {
				field = fmt.Sprintf("%s[%q]", settingsFileOptionField, tc.key)
			}

			requireExactUnsupportedField(t, err, field)
		})
	}
}

func TestLoadDiscoveredSettingsMergeOrder(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()
	managed := filepath.Join(t.TempDir(), "managed-settings.json")

	previousManagedPath := managedSettingsPath
	managedSettingsPath = func() string { return managed }
	t.Cleanup(func() { managedSettingsPath = previousManagedPath })

	require.NoError(t, os.WriteFile(filepath.Join(home, settingsFileName), []byte(`{
		"model": "claude-user",
		"effortLevel": "low",
		"availableModels": ["claude-haiku-4-5", "claude-opus-4-7[1m]"],
		"permissions": {"defaultMode": "acceptEdits"},
		"env": {"FROM_USER": "yes", "OVERRIDE": "user"}
	}`), 0o600))

	require.NoError(t, os.MkdirAll(filepath.Join(cwd, settingsDirName), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(cwd, settingsDirName, settingsFileName), []byte(`{
		"model": "claude-project",
		"availableModels": ["claude-opus-4-7[1m]", "claude-sonnet-4-6"],
		"permissions": {"defaultMode": "auto"},
		"env": {"OVERRIDE": "project"}
	}`), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(cwd, settingsDirName, settingsLocalFileName), []byte(`{
		"effortLevel": "high",
		"permissions": {"defaultMode": "dontAsk"}
	}`), 0o600))
	require.NoError(t, os.WriteFile(managed, []byte(`{
		"model": "claude-managed",
		"availableModels": ["claude-managed"],
		"env": {"FROM_MANAGED": "yes"}
	}`), 0o600))

	settings := loadDiscoveredSettings(context.Background(), cwd, home, nil)
	require.Equal(t, "claude-managed", settings.Model)
	require.Equal(t, "high", settings.Effort)
	require.Equal(t, "dontAsk", settings.PermissionMode)
	require.True(t, settings.HasAvailableModels)
	require.Equal(t, []string{
		"claude-haiku-4-5",
		"claude-opus-4-7[1m]",
		"claude-sonnet-4-6",
		"claude-managed",
	}, settings.AvailableModels)
	require.Equal(t, map[string]string{
		"FROM_USER":    "yes",
		"OVERRIDE":     "project",
		"FROM_MANAGED": "yes",
	}, settings.Env)
}

func TestLoadDiscoveredSettingsIgnoresInvalidFiles(t *testing.T) {
	home := t.TempDir()
	cwd := t.TempDir()

	previousManagedPath := managedSettingsPath
	managedSettingsPath = func() string { return filepath.Join(t.TempDir(), "missing.json") }
	t.Cleanup(func() { managedSettingsPath = previousManagedPath })

	require.NoError(t, os.WriteFile(filepath.Join(home, settingsFileName), []byte(`{bad`), 0o600))
	require.NoError(t, os.MkdirAll(filepath.Join(cwd, settingsDirName), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(cwd, settingsDirName, settingsFileName), []byte(`{
		"availableModels": [],
		"permissions": {"defaultMode": 7},
		"env": {"GOOD": "yes", "BAD": 1}
	}`), 0o600))

	settings := loadDiscoveredSettings(context.Background(), cwd, home, nil)
	require.Empty(t, settings.Model)
	require.Empty(t, settings.PermissionMode)
	require.True(t, settings.HasAvailableModels)
	require.Empty(t, settings.AvailableModels)
	require.Equal(t, map[string]string{"GOOD": "yes"}, settings.Env)
}

func TestLoadDiscoveredSettingsStopsOnContextCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	settings := loadDiscoveredSettings(ctx, t.TempDir(), t.TempDir(), slog.New(slog.DiscardHandler))
	require.Empty(t, settings.Model)

	logger := slog.New(slog.DiscardHandler)
	_, ok := loadSettingsFile(context.Background(), t.TempDir(), logger)
	require.False(t, ok)
	invalid := filepath.Join(t.TempDir(), "settings.json")
	require.NoError(t, os.WriteFile(invalid, []byte(`{bad`), 0o600))
	_, ok = loadSettingsFile(context.Background(), invalid, logger)
	require.False(t, ok)
}

func TestSettingsHelpers(t *testing.T) {
	require.Equal(t, absTestPath("home", "claude", "settings.json"), userSettingsPath(absTestPath("home", "claude")))
	t.Setenv("CLAUDE_CONFIG_DIR", absTestPath("env", "claude"))
	require.Equal(t, absTestPath("env", "claude", "settings.json"), userSettingsPath(""))
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	previousUserHomeDir := userHomeDir
	userHomeDir = func() (string, error) { return absTestPath("home", "user"), nil }
	require.Equal(t, absTestPath("home", "user", ".claude", "settings.json"), userSettingsPath(""))
	userHomeDir = previousUserHomeDir

	home := t.TempDir()
	canonicalHome, err := filepath.EvalSymlinks(home)
	require.NoError(t, err)
	canonical, err := canonicalClaudeHome(filepath.Join(home, "."))
	require.NoError(t, err)
	require.Equal(t, canonicalHome, canonical)
	canonical, err = canonicalClaudeHome("")
	require.NoError(t, err)
	require.Empty(t, canonical)
	_, err = canonicalClaudeHome(filepath.Join(t.TempDir(), "missing"))
	require.Error(t, err)
	notDir := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(notDir, []byte("settings"), 0o600))
	_, err = canonicalClaudeHome(notDir)
	require.Error(t, err)
	previousAbs := filepathAbs
	filepathAbs = func(string) (string, error) { return "", errors.New("abs failed") }
	_, err = canonicalClaudeHome("relative")
	require.Error(t, err)
	filepathAbs = previousAbs
	previousEvalSymlinks := filepathEvalSymlinks
	filepathEvalSymlinks = func(string) (string, error) { return "", errors.New("eval failed") }
	_, err = canonicalClaudeHome(home)
	require.Error(t, err)
	filepathEvalSymlinks = previousEvalSymlinks
	t.Cleanup(func() {
		filepathAbs = previousAbs
		filepathEvalSymlinks = previousEvalSymlinks
	})

	userHomeDir = func() (string, error) { return "", errors.New("home failed") }
	require.Empty(t, userSettingsPath(""))
	userHomeDir = previousUserHomeDir
	t.Cleanup(func() { userHomeDir = previousUserHomeDir })

	require.Equal(t, map[string]string{"A": "override", "B": "base"}, mergeEnv(
		map[string]string{"A": "base", "B": "base"},
		map[string]string{"A": "override"},
	))
	logs := new(bytes.Buffer)
	logger := slog.New(slog.NewTextHandler(logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	require.Equal(t, map[string]string{
		"GOOD_1":      "yes",
		"https_proxy": "",
		"1_DIGIT":     "digit",
		"WITH-DASH":   "dash",
	}, stringMapSetting(context.Background(), map[string]any{
		"env": map[string]any{
			"GOOD_1":      "yes",
			"https_proxy": "",
			"1_DIGIT":     "digit",
			"WITH-DASH":   "dash",
			"BAD=EQUALS":  "equals",
			"BAD_NUL":     "bad\x00value",
		},
	}, "env", logger))
	require.Contains(t, logs.String(), "ignoring invalid settings env entry")
	require.Contains(t, logs.String(), "BAD=EQUALS")
	require.Contains(t, logs.String(), "BAD_NUL")
	require.NotContains(t, logs.String(), "equals\"")

	allowlist, ok := settingsAvailableModelAllowlist(
		modelConfig{AvailableModels: []string{"opus", "sonnet"}},
		true,
		discoveredSettings{AvailableModels: []string{"sonnet", "haiku"}, HasAvailableModels: true},
	)
	require.True(t, ok)
	require.Equal(t, []string{"opus", "sonnet", "haiku"}, allowlist)
	allowlist, ok = settingsAvailableModelAllowlist(
		modelConfig{AvailableModels: []string{"opus", "opus"}},
		true,
		discoveredSettings{},
	)
	require.True(t, ok)
	require.Equal(t, []string{"opus"}, allowlist)
	require.Equal(t, "b", firstNonEmptyString("", "b", "c"))
	require.NotEmpty(t, defaultManagedSettingsPath())

	previousGOOS := claude.Platform
	t.Cleanup(func() { claude.Platform = previousGOOS })
	claude.Platform = "darwin"
	require.Contains(t, defaultManagedSettingsPath(), "/Library/Application Support/")
	claude.Platform = "windows"
	require.Contains(t, defaultManagedSettingsPath(), `C:\Program Files`)
	claude.Platform = "linux"
	require.Equal(t, "/etc/claude-code/managed-settings.json", defaultManagedSettingsPath())
	_, ok = loadSettingsFile(context.Background(), "", nil)
	require.False(t, ok)
}
