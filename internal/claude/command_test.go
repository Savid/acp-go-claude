package claude

import (
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"

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
		MCPConfigPath:           "/tmp/acp-go-claude-mcp.json",
		JSONSchema: map[string]any{
			"type":       "object",
			"properties": map[string]any{"ok": map[string]any{"type": "boolean"}},
		},
	})

	require.Equal(t, []string{
		cliArgOutputFormat, "stream-json",
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
		"--mcp-config", "/tmp/acp-go-claude-mcp.json",
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

	args := BuildArgs(Options{SettingsFile: "/tmp/home/custom.settings.json"})

	index := slices.Index(args, "--settings")
	require.GreaterOrEqual(t, index, 0)
	require.Less(t, index+1, len(args))
	require.Equal(t, "/tmp/home/custom.settings.json", args[index+1])
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

	options := withTestProcessIsolation(Options{
		ClaudeHome: "/tmp/claude-home",
		Cwd:        "/repo",
		Env: map[string]string{
			"CLAUDE_CONFIG_DIR": "/override/claude-home",
			"CLAUDECODE":        "explicit-nested",
			"HOME":              "/override/home",
			"PWD":               "/override/cwd",
			"X_TEST":            "1",
			"XDG_CONFIG_HOME":   "/override/xdg-config",
		},
	})
	options.ProcessIsolation.BaseEnvironment["HOME"] = "/managed/home"
	options.ProcessIsolation.BaseEnvironment["XDG_CONFIG_HOME"] = "/managed/xdg-config"
	env := BuildEnv(options)

	require.Equal(t, 1, countEnvKey(env, "CLAUDE_CONFIG_DIR"))
	require.Equal(t, 1, countEnvKey(env, "CLAUDE_CODE_ENTRYPOINT"))
	require.Equal(t, 1, countEnvKey(env, "PWD"))
	require.Equal(t, 0, countEnvKey(env, "CLAUDECODE"))
	require.Contains(t, env, "CLAUDE_CONFIG_DIR=/tmp/claude-home")
	require.Contains(t, env, "CLAUDE_CODE_ENTRYPOINT=acp-go-claude")
	require.Contains(t, env, "HOME=/managed/home")
	require.Contains(t, env, "PWD=/repo")
	require.Contains(t, env, "X_TEST=1")
	require.Contains(t, env, "XDG_CONFIG_HOME=/managed/xdg-config")
	require.NotContains(t, env, "CLAUDE_CONFIG_DIR=/override/claude-home")
	require.NotContains(t, env, "HOME=/override/home")
	require.NotContains(t, env, "XDG_CONFIG_HOME=/override/xdg-config")
}

func TestBuildEnvPrependsExtraPathDirs(t *testing.T) {
	separator := string(os.PathListSeparator)

	t.Setenv(envSearchPath, "/usr/bin"+separator+"/bin")

	env := BuildEnv(withTestProcessIsolation(Options{ExtraPathDirs: []string{"/session/bin", "/shared/bin"}}))

	require.Equal(t, 1, countEnvKey(env, envSearchPath))
	require.Contains(t, env, envSearchPath+"=/session/bin"+separator+"/shared/bin"+separator+"/usr/bin"+separator+"/bin")
}

// TestBuildEnvExtraPathDirsOutrankAnOverriddenPath pins the precedence between
// the two ways a PATH reaches the child: an explicit Env override replaces the
// inherited value, and the extra dirs still lead it.
func TestBuildEnvExtraPathDirsOutrankAnOverriddenPath(t *testing.T) {
	separator := string(os.PathListSeparator)

	t.Setenv(envSearchPath, "/inherited/bin")

	env := BuildEnv(withTestProcessIsolation(Options{
		Env:           map[string]string{envSearchPath: "/override/bin"},
		ExtraPathDirs: []string{"/session/bin"},
	}))

	require.Equal(t, 1, countEnvKey(env, envSearchPath))
	require.Contains(t, env, envSearchPath+"=/session/bin"+separator+"/override/bin")
}

func TestBuildEnvExtraPathDirsWithoutInheritedPath(t *testing.T) {
	env := BuildEnv(Options{
		ProcessIsolation: &ProcessIsolation{UID: 1, GID: 2, BaseEnvironment: map[string]string{"GOOD": "1"}, StandaloneOwnerID: "test-owner", StandaloneStateRoot: "/var/lib/acp-go-test"},
		ExtraPathDirs:    []string{"/session/bin"},
	})

	require.Contains(t, env, envSearchPath+"=/session/bin")
}

func TestBuildEnvUsesOnlyPolicyEntries(t *testing.T) {
	env := BuildEnv(Options{ProcessIsolation: &ProcessIsolation{
		UID: 1, GID: 2, BaseEnvironment: map[string]string{"GOOD": "1"},
		StandaloneOwnerID: "test-owner", StandaloneStateRoot: "/var/lib/acp-go-test",
	}})

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

	path, err := Discover(context.Background(), "/bin/sh", withTestProcessIsolation(Options{}))
	require.NoError(t, err)
	require.Equal(t, "/bin/sh", path)
}

func TestDiscoverRejectsMissingPolicy(t *testing.T) {
	_, err := Discover(t.Context(), "/bin/sh", struct{}{})
	require.ErrorContains(t, err, "process isolation is required")
}

func TestDiscoverCancelledExplicitPath(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := Discover(ctx, "/bin/sh", withTestProcessIsolation(Options{}))
	require.ErrorIs(t, err, context.Canceled)

	_, err = Discover(ctx, "", withTestProcessIsolation(Options{}))
	require.ErrorIs(t, err, context.Canceled)
}

func TestDiscoverMissingFromPath(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	_, err := Discover(context.Background(), "", withTestProcessIsolation(Options{}))
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
	options := platformTestTransportOptions(t, Options{})
	require.NoError(t, validateClaudeVersion(context.Background(), current, options))

	old := writeShellScript(t, filepath.Join(dir, "old"), "#!/bin/sh\necho '1.9.9 (Claude Code)'\n")
	require.ErrorContains(t, validateClaudeVersion(context.Background(), old, options), "too old")

	unparsable := writeShellScript(t, filepath.Join(dir, "bad"), "#!/bin/sh\necho 'no version'\n")
	require.ErrorContains(t, validateClaudeVersion(context.Background(), unparsable, options), "could not parse")

	failing := writeShellScript(t, filepath.Join(dir, "fail"), "#!/bin/sh\nexit 1\n")
	require.Error(t, validateClaudeVersion(context.Background(), failing, options))
}

func TestContainedClaudeOutputSurvivesWaitBeforeRead(t *testing.T) {
	const (
		helperEnv = "CLAUDE_TEST_CONTAINED_OUTPUT_HELPER"
		sentinel  = "contained-output-survived"
	)
	if os.Getenv(helperEnv) == "1" {
		_, err := io.WriteString(os.Stdout, sentinel)
		require.NoError(t, err)

		return
	}

	originalPrepare := processPrepareContained
	originalStart := processStartContained
	originalWait := processWaitContained
	originalQuiesce := processBoundaryComplete
	originalClose := processContainmentClose
	t.Cleanup(func() {
		processPrepareContained = originalPrepare
		processStartContained = originalStart
		processWaitContained = originalWait
		processBoundaryComplete = originalQuiesce
		processContainmentClose = originalClose
	})

	processPrepareContained = func(command *exec.Cmd, _ processLaunchOptions) (*processTreeCommand, error) {
		return &processTreeCommand{cmd: command}, nil
	}
	processStartContained = func(launch *processTreeCommand) (*processContainment, error) {
		require.NoError(t, launch.cmd.Start())
		require.NoError(t, launch.cmd.Wait())

		return &processContainment{}, nil
	}
	processWaitContained = func(*processContainment, *exec.Cmd) error { return nil }
	processBoundaryComplete = func(*processContainment, time.Duration) error { return nil }
	processContainmentClose = func(*processContainment) error { return nil }
	t.Setenv(helperEnv, "1")

	output, err := containedClaudeOutput(
		t.Context(),
		os.Args[0],
		[]string{"-test.run=^TestContainedClaudeOutputSurvivesWaitBeforeRead$"},
		withTestProcessIsolation(Options{Cwd: t.TempDir()}),
		nil,
		"contained output regression",
	)
	require.NoError(t, err)
	require.Contains(t, string(output), sentinel)
}

func TestValidateClaudeVersionContainmentFailureBranches(t *testing.T) {
	want := errors.New("version seam")
	releases := 0
	err := validateClaudeVersion(t.Context(), "/bin/sh", withTestProcessIsolation(Options{
		AcquireVersionDiscovery: func(context.Context) (func(), error) { return nil, want },
	}))
	require.ErrorIs(t, err, want)
	err = validateClaudeVersion(t.Context(), "/bin/sh", withTestProcessIsolation(Options{
		AcquireVersionDiscovery: func(context.Context) (func(), error) { return nil, nil }, //nolint:nilnil // Invalid callback result under test.
	}))
	require.ErrorContains(t, err, "nil release")

	err = validateClaudeVersion(t.Context(), "/bin/sh", withTestProcessIsolation(Options{
		DarwinBestEffort: true,
		AcquireVersionDiscovery: func(context.Context) (func(), error) {
			return func() { releases++ }, nil
		},
	}))
	require.ErrorIs(t, err, ErrProcessContainmentIncomplete)
	require.Zero(t, releases)

	_, err = containedClaudeVersionOutput(t.Context(), "/bin/sh", withTestProcessIsolation(Options{
		DarwinBestEffort: true,
		PrepareDarwinVersionGeneration: func(context.Context) (*DarwinGeneration, error) {
			return nil, want
		},
	}))
	require.ErrorIs(t, err, want)

	originalGetwd := processGetwd
	originalPrepare := processPrepareContained
	originalStart := processStartContained
	t.Cleanup(func() {
		processGetwd = originalGetwd
		processPrepareContained = originalPrepare
		processStartContained = originalStart
	})

	finished := 0
	generationOptions := withTestProcessIsolation(Options{
		DarwinBestEffort: true,
		PrepareDarwinVersionGeneration: func(context.Context) (*DarwinGeneration, error) {
			return &DarwinGeneration{RecordFinished: func(bool) error {
				finished++

				return nil
			}}, nil
		},
	})
	processGetwd = func() (string, error) { return "", want }
	_, err = containedClaudeVersionOutput(t.Context(), "/bin/sh", generationOptions)
	require.ErrorIs(t, err, want)
	require.Equal(t, 1, finished)
	processGetwd = originalGetwd

	processPrepareContained = func(*exec.Cmd, processLaunchOptions) (*processTreeCommand, error) { return nil, want }
	_, err = containedClaudeVersionOutput(t.Context(), "/bin/sh", withTestProcessIsolation(Options{Cwd: t.TempDir()}))
	require.ErrorIs(t, err, want)
	observedUnavailable := 0
	processPrepareContained = func(*exec.Cmd, processLaunchOptions) (*processTreeCommand, error) {
		return nil, ErrProcessContainmentIncomplete
	}
	_, err = containedClaudeVersionOutput(t.Context(), "/bin/sh", withTestProcessIsolation(Options{
		Cwd: t.TempDir(),
		ObserveProcessInventory: func(context.Context, func() (int, bool)) {
			observedUnavailable++
		},
	}))
	require.ErrorIs(t, err, ErrProcessContainmentIncomplete)
	require.Equal(t, 1, observedUnavailable)

	processPrepareContained = func(command *exec.Cmd, _ processLaunchOptions) (*processTreeCommand, error) {
		command.Stdout = io.Discard

		return &processTreeCommand{cmd: command}, nil
	}
	_, err = containedClaudeVersionOutput(t.Context(), "/bin/sh", withTestProcessIsolation(Options{Cwd: t.TempDir()}))
	require.ErrorContains(t, err, "capture claude version output")

	processPrepareContained = func(command *exec.Cmd, _ processLaunchOptions) (*processTreeCommand, error) {
		return &processTreeCommand{cmd: command}, nil
	}
	processStartContained = func(*processTreeCommand) (*processContainment, error) { return nil, want }
	_, err = containedClaudeVersionOutput(t.Context(), "/bin/sh", withTestProcessIsolation(Options{Cwd: t.TempDir()}))
	require.ErrorIs(t, err, want)
	processStartContained = func(*processTreeCommand) (*processContainment, error) {
		return nil, ErrProcessContainmentIncomplete
	}
	_, err = containedClaudeVersionOutput(t.Context(), "/bin/sh", withTestProcessIsolation(Options{
		Cwd: t.TempDir(),
		ObserveProcessInventory: func(context.Context, func() (int, bool)) {
			observedUnavailable++
		},
	}))
	require.ErrorIs(t, err, ErrProcessContainmentIncomplete)
	require.Equal(t, 2, observedUnavailable)

	_, err = containedClaudeOutput(
		t.Context(), "/bin/sh", nil, Options{ProcessIsolation: &ProcessIsolation{}}, nil, "invalid environment",
	)
	require.ErrorContains(t, err, "invalid process isolation")

	originalPipe := commandPipe
	t.Cleanup(func() { commandPipe = originalPipe })
	commandPipe = func() (*os.File, *os.File, error) { return nil, nil, want }
	_, err = containedClaudeOutput(t.Context(), "/bin/sh", nil, withTestProcessIsolation(Options{Cwd: t.TempDir()}), nil, "pipe failure")
	require.ErrorIs(t, err, want)
	commandPipe = originalPipe

	processPrepareContained = originalPrepare
	processStartContained = originalStart
	if runtime.GOOS != "windows" {
		dir := t.TempDir()
		hanging := writeShellScript(t, filepath.Join(dir, "hanging"), "#!/bin/sh\nsleep 30\n")
		ctx, cancel := context.WithTimeout(t.Context(), 20*time.Millisecond)
		defer cancel()
		_, err = containedClaudeVersionOutput(ctx, hanging, platformTestTransportOptions(t, Options{Cwd: dir}))
		require.ErrorIs(t, err, context.DeadlineExceeded)
	}
}

func TestObserveAuxiliaryBoundaryCompleteBranches(t *testing.T) {
	inventories := 0
	completed := 0
	options := Options{
		ObserveProcessInventory: func(context.Context, func() (int, bool)) { inventories++ },
		ObserveBoundaryComplete: func(context.Context) { completed++ },
	}
	observeAuxiliaryBoundaryComplete(options, errors.New("cleanup failed"))
	require.Equal(t, 1, inventories)
	require.Zero(t, completed)

	observeAuxiliaryBoundaryComplete(Options{}, errors.New("cleanup failed"))
	observeAuxiliaryBoundaryComplete(options, nil)
	require.Equal(t, 1, inventories)
	require.Equal(t, 1, completed)
}

func TestDiscoverFromPath(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses executable mode bits")
	}

	binDir := t.TempDir()
	path := filepath.Join(binDir, "claude")
	require.NoError(t, os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o755))
	t.Setenv("PATH", binDir)

	found, err := Discover(context.Background(), "", withTestProcessIsolation(Options{}))
	require.NoError(t, err)
	require.Equal(t, path, found)
}
