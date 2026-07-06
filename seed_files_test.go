package claudeacp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/stretchr/testify/require"
)

func readSeedManifest(t *testing.T, dir string) []string {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(dir, seedManifestFileName))
	require.NoError(t, err)

	var entries []string
	require.NoError(t, json.Unmarshal(data, &entries))

	return entries
}

func TestWithSeedFilesClonesInput(t *testing.T) {
	t.Parallel()

	source := map[string]string{"settings.json": `{"model":"opus"}`}

	options := applyOptions([]Option{WithSeedFiles(source)})
	require.Equal(t, source, options.SeedFiles)

	// Mutating the caller map must not affect the stored options.
	source["settings.json"] = "mutated"
	source["extra.json"] = "added"

	require.Equal(t, `{"model":"opus"}`, options.SeedFiles["settings.json"])
	require.NotContains(t, options.SeedFiles, "extra.json")

	// Nil input yields a nil map, not a panic.
	require.Nil(t, applyOptions([]Option{WithSeedFiles(nil)}).SeedFiles)
}

func TestWriteSeedFilesWritesUnderDir(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	files := map[string]string{
		"settings.json":            `{"env":{"ANTHROPIC_BASE_URL":"https://proxy.example"}}`,
		"agents/reviewer/AGENT.md": "seeded verbatim\n",
	}

	require.NoError(t, writeSeedFiles(dir, files))

	for name, want := range files {
		target := filepath.Join(dir, filepath.FromSlash(name))

		got, err := os.ReadFile(target)
		require.NoError(t, err)
		require.Equal(t, want, string(got))

		info, err := os.Stat(target)
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o600), info.Mode().Perm())
	}

	parentInfo, err := os.Stat(filepath.Join(dir, "agents", "reviewer"))
	require.NoError(t, err)
	require.True(t, parentInfo.IsDir())
	require.Equal(t, os.FileMode(0o700), parentInfo.Mode().Perm())
}

func TestWriteSeedFilesEmptyInputNoop(t *testing.T) {
	t.Parallel()

	require.NoError(t, writeSeedFiles(t.TempDir(), nil))
	require.NoError(t, writeSeedFiles("", nil))
}

func TestWriteSeedFilesRejectsEmptyDir(t *testing.T) {
	t.Parallel()

	err := writeSeedFiles("", map[string]string{"settings.json": "{}"})
	requireExactUnsupportedField(t, err, seedFilesOptionField)
}

func TestWriteSeedFilesRejectsUnsafePaths(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name string
		key  string
	}{
		{name: "empty", key: ""},
		{name: "absolute", key: "/etc/passwd"},
		{name: "parent escape", key: "../evil.json"},
		{name: "nested parent escape", key: "a/../../evil.json"},
		{name: "current dir", key: "./settings.json"},
		{name: "backslash root", key: `\evil.json`},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			dir := t.TempDir()
			err := writeSeedFiles(dir, map[string]string{tc.key: "{}"})

			field := seedFilesOptionField
			if tc.key != "" {
				field = fmt.Sprintf("%s[%q]", seedFilesOptionField, tc.key)
			}

			requireExactUnsupportedField(t, err, field)

			entries, readErr := os.ReadDir(dir)
			require.NoError(t, readErr)
			require.Empty(t, entries, "no files should be written when a key is rejected")
		})
	}
}

func TestStartSessionSeedsFilesUnderHome(t *testing.T) {
	ctx := context.Background()
	cwd := t.TempDir()
	home := t.TempDir()
	sessionID := acp.SessionId("13131313-1313-4313-8313-131313131313")

	seedContent := `{"env":{"ANTHROPIC_BASE_URL":"https://proxy.example"}}`
	agent, _, _ := newFakeLifecycleAgent(t, newFakeClaudeTransport(),
		WithHome(home),
		WithSeedFiles(map[string]string{"settings.json": seedContent}),
	)

	session, err := agent.startSession(ctx, sessionID, sessionStart{Cwd: cwd})
	require.NoError(t, err)

	got, err := os.ReadFile(filepath.Join(home, "settings.json"))
	require.NoError(t, err)
	require.Equal(t, seedContent, string(got))

	require.NoError(t, session.Close(ctx))
}

func TestStartSessionSeedFilesRequireExplicitHome(t *testing.T) {
	ctx := context.Background()
	cwd := t.TempDir()
	sessionID := acp.SessionId("14141414-1414-4414-8414-141414141414")

	// WithHome("") clears the default temp home set by newFakeLifecycleAgent.
	agent, _, _ := newFakeLifecycleAgent(t, newFakeClaudeTransport(),
		WithHome(""),
		WithSeedFiles(map[string]string{"settings.json": "{}"}),
	)

	session, err := agent.startSession(ctx, sessionID, sessionStart{Cwd: cwd})
	require.Nil(t, session)
	requireExactUnsupportedField(t, err, seedFilesOptionField)
}

func TestWriteSeedFilesRecordsManifest(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	require.NoError(t, writeSeedFiles(dir, map[string]string{
		"settings.json":            "{}",
		"agents/reviewer/AGENT.md": "seed\n",
	}))

	require.Equal(t, []string{"agents/reviewer/AGENT.md", "settings.json"}, readSeedManifest(t, dir))
}

func TestWriteSeedFilesReseedIdenticalNoBackup(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	files := map[string]string{"settings.json": `{"model":"opus"}`}

	require.NoError(t, writeSeedFiles(dir, files))
	require.NoError(t, writeSeedFiles(dir, files))

	_, err := os.Stat(filepath.Join(dir, "settings.json"+seedBackupSuffix))
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestWriteSeedFilesReseedChangedBacksUp(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	target := filepath.Join(dir, "settings.json")

	require.NoError(t, writeSeedFiles(dir, map[string]string{"settings.json": `{"model":"opus"}`}))
	require.NoError(t, writeSeedFiles(dir, map[string]string{"settings.json": `{"model":"sonnet"}`}))

	updated, err := os.ReadFile(target)
	require.NoError(t, err)
	require.Equal(t, `{"model":"sonnet"}`, string(updated))

	backup, err := os.ReadFile(target + seedBackupSuffix)
	require.NoError(t, err)
	require.Equal(t, `{"model":"opus"}`, string(backup))
}

func TestWriteSeedFilesRejectsUnmanagedPreexisting(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	target := filepath.Join(dir, "settings.json")

	// Operator-authored file the wrapper never wrote (not in any manifest).
	require.NoError(t, os.WriteFile(target, []byte("operator owned"), 0o600))

	err := writeSeedFiles(dir, map[string]string{"settings.json": "{}"})
	requireExactUnsupportedField(t, err, fmt.Sprintf("%s[%q]", seedFilesOptionField, "settings.json"))

	// The operator file is untouched and no manifest is created.
	got, readErr := os.ReadFile(target)
	require.NoError(t, readErr)
	require.Equal(t, "operator owned", string(got))

	_, manifestErr := os.Stat(filepath.Join(dir, seedManifestFileName))
	require.ErrorIs(t, manifestErr, os.ErrNotExist)
}

func TestWriteSeedFilesManifestSurvivesAcrossPasses(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// First pass seeds settings.json and records it as managed.
	require.NoError(t, writeSeedFiles(dir, map[string]string{"settings.json": `{"model":"opus"}`}))
	require.Equal(t, []string{"settings.json"}, readSeedManifest(t, dir))

	// Second pass changes the managed file: the guard treats it as owned and
	// updates it (keeping a backup) instead of failing closed.
	require.NoError(t, writeSeedFiles(dir, map[string]string{"settings.json": `{"model":"sonnet"}`}))

	backup, err := os.ReadFile(filepath.Join(dir, "settings.json"+seedBackupSuffix))
	require.NoError(t, err)
	require.Equal(t, `{"model":"opus"}`, string(backup))
	require.Equal(t, []string{"settings.json"}, readSeedManifest(t, dir))
}

func TestStartSessionResolvesSettingsFileUnderHome(t *testing.T) {
	ctx := context.Background()
	cwd := t.TempDir()
	home := t.TempDir()
	sessionID := acp.SessionId("15151515-1515-4515-8515-151515151515")

	agent, _, _ := newFakeLifecycleAgent(t, newFakeClaudeTransport(),
		WithHome(home),
		WithClaudeSettingsFile("wagie.settings.json"),
	)

	var captured claude.Options
	transport := newFakeClaudeTransport()
	agent.newClaudeClient = func(log *slog.Logger, options claude.Options) *claude.Client {
		captured = options

		return claude.NewClient(log, options, transport)
	}

	session, err := agent.startSession(ctx, sessionID, sessionStart{Cwd: cwd})
	require.NoError(t, err)
	require.Equal(t, filepath.Join(home, "wagie.settings.json"), captured.SettingsFile)

	require.NoError(t, session.Close(ctx))
}

func TestStartSessionSettingsFileRequiresExplicitHome(t *testing.T) {
	ctx := context.Background()
	cwd := t.TempDir()
	sessionID := acp.SessionId("16161616-1616-4616-8616-161616161616")

	// WithHome("") clears the default temp home set by newFakeLifecycleAgent.
	agent, _, _ := newFakeLifecycleAgent(t, newFakeClaudeTransport(),
		WithHome(""),
		WithClaudeSettingsFile("wagie.settings.json"),
	)

	session, err := agent.startSession(ctx, sessionID, sessionStart{Cwd: cwd})
	require.Nil(t, session)
	requireExactUnsupportedField(t, err, settingsFileOptionField)
}

func TestPrepareSeededClaudeConfigWriteError(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	agent := NewAgent(WithHome(home), WithSeedFiles(map[string]string{"../evil.json": "{}"}))

	resolved, err := agent.prepareSeededClaudeConfig(home, home)
	require.Empty(t, resolved)
	requireExactUnsupportedField(t, err, fmt.Sprintf("%s[%q]", seedFilesOptionField, "../evil.json"))
}

// TestWriteSeedFilesIOErrorBranches injects failures into the materialize file
// primitives to exercise the guard's error paths. Subtests mutate package-level
// vars, so they must not run in parallel.
func TestWriteSeedFilesIOErrorBranches(t *testing.T) {
	origRead := materializeReadFile
	origWrite := materializeWriteFile
	origStat := materializeStat
	origMkdir := materializeMkdirAll
	t.Cleanup(func() {
		materializeReadFile = origRead
		materializeWriteFile = origWrite
		materializeStat = origStat
		materializeMkdirAll = origMkdir
	})

	reset := func() {
		materializeReadFile = origRead
		materializeWriteFile = origWrite
		materializeStat = origStat
		materializeMkdirAll = origMkdir
	}

	failWriteSuffix := func(suffix string) {
		materializeWriteFile = func(path string, data []byte, perm os.FileMode) error {
			if strings.HasSuffix(path, suffix) {
				return errors.New("boom")
			}

			return origWrite(path, data, perm)
		}
	}

	t.Run("stat error", func(t *testing.T) {
		reset()
		materializeStat = func(string) (os.FileInfo, error) { return nil, errors.New("stat boom") }

		err := writeSeedFiles(t.TempDir(), map[string]string{"settings.json": "{}"})
		require.ErrorContains(t, err, "stat seed file")
	})

	t.Run("manifest read error", func(t *testing.T) {
		reset()
		materializeReadFile = func(path string) ([]byte, error) {
			if strings.HasSuffix(path, seedManifestFileName) {
				return nil, errors.New("read boom")
			}

			return origRead(path)
		}

		err := writeSeedFiles(t.TempDir(), map[string]string{"settings.json": "{}"})
		require.ErrorContains(t, err, "read seed manifest")
	})

	t.Run("manifest decode error", func(t *testing.T) {
		reset()
		dir := t.TempDir()
		require.NoError(t, os.WriteFile(filepath.Join(dir, seedManifestFileName), []byte("not json"), 0o600))

		err := writeSeedFiles(dir, map[string]string{"settings.json": "{}"})
		require.ErrorContains(t, err, "decode seed manifest")
	})

	t.Run("managed read error", func(t *testing.T) {
		reset()
		dir := t.TempDir()
		require.NoError(t, writeSeedFiles(dir, map[string]string{"settings.json": `{"a":1}`}))

		materializeReadFile = func(path string) ([]byte, error) {
			if strings.HasSuffix(path, "settings.json") {
				return nil, errors.New("read boom")
			}

			return origRead(path)
		}

		err := writeSeedFiles(dir, map[string]string{"settings.json": `{"a":2}`})
		require.ErrorContains(t, err, "read managed seed file")
	})

	t.Run("backup write error", func(t *testing.T) {
		reset()
		dir := t.TempDir()
		require.NoError(t, writeSeedFiles(dir, map[string]string{"settings.json": `{"a":1}`}))

		failWriteSuffix(seedBackupSuffix)

		err := writeSeedFiles(dir, map[string]string{"settings.json": `{"a":2}`})
		require.ErrorContains(t, err, "back up managed seed file")
	})

	t.Run("mkdir error", func(t *testing.T) {
		reset()
		materializeMkdirAll = func(string, os.FileMode) error { return errors.New("mkdir boom") }

		err := writeSeedFiles(t.TempDir(), map[string]string{"settings.json": "{}"})
		require.ErrorContains(t, err, "create seed file directory")
	})

	t.Run("target write error", func(t *testing.T) {
		reset()
		failWriteSuffix("settings.json")

		err := writeSeedFiles(t.TempDir(), map[string]string{"settings.json": "{}"})
		require.ErrorContains(t, err, "write seed file")
	})

	t.Run("manifest write error", func(t *testing.T) {
		reset()
		failWriteSuffix(seedManifestFileName)

		err := writeSeedFiles(t.TempDir(), map[string]string{"settings.json": "{}"})
		require.ErrorContains(t, err, "write seed manifest")
	})
}
