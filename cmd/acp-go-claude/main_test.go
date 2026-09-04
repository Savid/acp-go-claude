package main

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	claudeacp "github.com/savid/acp-go-claude"
	"github.com/stretchr/testify/require"
)

func TestRunPassesCurrentFlags(t *testing.T) {
	originalServe := serve
	originalVersion := agentVersion
	t.Cleanup(func() {
		serve = originalServe
		agentVersion = originalVersion
	})
	agentVersion = func() string { return "v1.2.3" }
	var got claudeacp.Options
	serve = func(_ context.Context, _ io.Reader, _ io.Writer, options ...claudeacp.Option) error {
		for _, option := range options {
			option(&got)
		}

		return nil
	}
	code := run(t.Context(), []string{
		"-path", "/bin/claude",
		"-home", "/tmp/home",
		"-scratch-dir", "/tmp/scratch",
		"-provider-auth-root", "/tmp/auth",
		"-provider-auth-direct-home", "/tmp/home",
		"-model", "sonnet",
		"-claude-bare",
		"-claude-permission-mode", "plan",
		"-claude-system-prompt", "system",
		"-claude-hide-auth",
		"-debug",
	}, bytes.NewReader(nil), io.Discard, io.Discard)
	require.Zero(t, code)
	require.Equal(t, "v1.2.3", got.AgentVersion)
	require.Equal(t, "/bin/claude", got.ExecutablePath)
	require.Equal(t, "/tmp/home", got.Home)
	require.Equal(t, "/tmp/scratch", got.ScratchDir)
	require.Equal(t, "/tmp/auth", got.ProviderAuthRoot)
	require.Equal(t, "/tmp/home", got.ProviderAuthDirectHome)
	require.Equal(t, "sonnet", got.DefaultModel)
	require.True(t, got.BareMode)
	require.Equal(t, "plan", got.DefaultPermissionMode)
	require.Equal(t, "system", got.DefaultSystemPrompt)
	require.True(t, got.HideAuth)
	require.NotNil(t, got.Logger)
}

func TestRunVersionAndUnknownFlag(t *testing.T) {
	originalVersion := agentVersion
	t.Cleanup(func() { agentVersion = originalVersion })
	agentVersion = func() string { return "test-version" }
	var output bytes.Buffer
	require.Zero(t, run(t.Context(), []string{"-version"}, bytes.NewReader(nil), &output, io.Discard))
	require.Equal(t, "test-version\n", output.String())
	require.Equal(t, 2, run(t.Context(), []string{"-removed-flag"}, bytes.NewReader(nil), io.Discard, io.Discard))
}

func TestRunPassesSeedAndSettingsFlags(t *testing.T) {
	originalServe := serve
	t.Cleanup(func() { serve = originalServe })

	hostFile := filepath.Join(t.TempDir(), "seed-settings.json")
	require.NoError(t, os.WriteFile(hostFile, []byte(`{"model":"opus"}`), 0o600))

	var got claudeacp.Options
	serve = func(_ context.Context, _ io.Reader, _ io.Writer, options ...claudeacp.Option) error {
		for _, option := range options {
			option(&got)
		}

		return nil
	}

	code := run(t.Context(), []string{
		"-home", "/tmp/claude",
		"-seed-file", "settings.json=" + hostFile,
		"-claude-settings-file", "custom.settings.json",
	}, bytes.NewReader(nil), io.Discard, io.Discard)
	require.Zero(t, code)
	require.Equal(t, map[string]string{"settings.json": `{"model":"opus"}`}, got.SeedFiles)
	require.Equal(t, "custom.settings.json", got.SettingsFile)
}

func TestSeedFileFlagCurrentBehavior(t *testing.T) {
	t.Parallel()

	var empty seedFileFlag
	require.Empty(t, empty.String())

	hostFile := filepath.Join(t.TempDir(), "host.json")
	require.NoError(t, os.WriteFile(hostFile, []byte("contents"), 0o600))

	var value seedFileFlag
	require.NoError(t, value.Set("a.json="+hostFile))
	require.NoError(t, value.Set("b.json="+hostFile))
	require.Equal(t, "a.json,b.json", value.String())
	require.Equal(t, "contents", value.files["a.json"])
	require.Error(t, value.Set("missing-separator"))
	require.Error(t, value.Set("=/only/host"))
	require.Error(t, value.Set("rel="))
	require.Error(t, value.Set("rel="+filepath.Join(t.TempDir(), "missing")))
}

func TestRunCurrentErrorBranches(t *testing.T) {
	originalServe := serve
	originalShutdown := shutdownOpenTelemetry
	t.Cleanup(func() {
		serve = originalServe
		shutdownOpenTelemetry = originalShutdown
	})

	serve = func(context.Context, io.Reader, io.Writer, ...claudeacp.Option) error {
		return errors.New("serve failed")
	}
	shutdownOpenTelemetry = func(context.Context, func(context.Context) error) error { return nil }
	var stderr bytes.Buffer
	require.Equal(t, 1, run(t.Context(), nil, bytes.NewReader(nil), io.Discard, &stderr))
	require.Contains(t, stderr.String(), "serve failed")

	serve = func(ctx context.Context, _ io.Reader, _ io.Writer, _ ...claudeacp.Option) error { return ctx.Err() }
	shutdownOpenTelemetry = func(context.Context, func(context.Context) error) error { return errors.New("shutdown failed") }
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	stderr.Reset()
	require.Equal(t, 1, run(ctx, nil, bytes.NewReader(nil), io.Discard, &stderr))
	require.Contains(t, stderr.String(), "shutdown OpenTelemetry")
}

func TestPendingSignalAndSignalCodeCurrentBehavior(t *testing.T) {
	signals := make(chan os.Signal, 1)
	require.Nil(t, pendingSignal(signals))
	signals <- syscall.SIGTERM
	require.Equal(t, syscall.SIGTERM, pendingSignal(signals))
	require.Equal(t, 128+int(syscall.SIGTERM), signalCode(syscall.SIGTERM))
	require.Equal(t, 1, signalCode(fakeSignal("fake")))
}

func TestMainCurrentExitBranch(t *testing.T) {
	originalServe := serve
	originalExit := exit
	originalArgs := os.Args
	t.Cleanup(func() {
		serve = originalServe
		exit = originalExit
		os.Args = originalArgs
	})

	serve = func(context.Context, io.Reader, io.Writer, ...claudeacp.Option) error {
		return errors.New("serve failed")
	}
	os.Args = []string{"acp-go-claude"}
	exitCode := -1
	exit = func(code int) { exitCode = code }

	main()
	require.Equal(t, 1, exitCode)
}

type fakeSignal string

func (s fakeSignal) String() string { return string(s) }
func (s fakeSignal) Signal()        {}
