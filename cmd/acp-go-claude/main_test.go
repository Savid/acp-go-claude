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

func TestRunPassesContractFlags(t *testing.T) {
	originalServe := serve
	originalAgentVersion := agentVersion
	t.Cleanup(func() {
		serve = originalServe
		agentVersion = originalAgentVersion
	})

	var got claudeacp.Options
	serve = func(_ context.Context, _ io.Reader, _ io.Writer, opts ...claudeacp.Option) error {
		for _, opt := range opts {
			opt(&got)
		}

		return nil
	}
	agentVersion = func() string { return "v1.2.3" }

	code := run(context.Background(), []string{
		"-path", "/bin/claude",
		"-home", "/tmp/claude",
		"-model", "sonnet",
		"-claude-bare",
		"-claude-permission-mode", "plan",
		"-claude-system-prompt", "system",
		"-claude-hide-auth",
		"-debug",
	}, bytes.NewBuffer(nil), bytes.NewBuffer(nil), bytes.NewBuffer(nil))

	require.Equal(t, 0, code)
	require.Equal(t, "v1.2.3", got.AgentVersion)
	require.Equal(t, "/bin/claude", got.ExecutablePath)
	require.Equal(t, "/tmp/claude", got.Home)
	require.Equal(t, "sonnet", got.DefaultModel)
	require.True(t, got.BareMode)
	require.Equal(t, "plan", got.DefaultPermissionMode)
	require.Equal(t, "system", got.DefaultSystemPrompt)
	require.True(t, got.HideAuth)
	require.NotNil(t, got.Logger)
}

func TestRunPassesSeedAndSettingsFlags(t *testing.T) {
	originalServe := serve
	t.Cleanup(func() { serve = originalServe })

	hostFile := filepath.Join(t.TempDir(), "seed-settings.json")
	require.NoError(t, os.WriteFile(hostFile, []byte(`{"model":"opus"}`), 0o600))

	var got claudeacp.Options
	serve = func(_ context.Context, _ io.Reader, _ io.Writer, opts ...claudeacp.Option) error {
		for _, opt := range opts {
			opt(&got)
		}

		return nil
	}

	code := run(context.Background(), []string{
		"-home", "/tmp/claude",
		"-seed-file", "settings.json=" + hostFile,
		"-settings-file", "custom.settings.json",
	}, bytes.NewBuffer(nil), bytes.NewBuffer(nil), bytes.NewBuffer(nil))

	require.Equal(t, 0, code)
	require.Equal(t, map[string]string{"settings.json": `{"model":"opus"}`}, got.SeedFiles)
	require.Equal(t, "custom.settings.json", got.SettingsFile)
}

func TestSeedFileFlag(t *testing.T) {
	t.Parallel()

	var empty seedFileFlag
	require.Empty(t, empty.String())

	hostFile := filepath.Join(t.TempDir(), "host.json")
	require.NoError(t, os.WriteFile(hostFile, []byte("contents"), 0o600))

	var flag seedFileFlag
	require.NoError(t, flag.Set("a.json="+hostFile))
	require.NoError(t, flag.Set("b.json="+hostFile))
	require.Equal(t, "a.json,b.json", flag.String())
	require.Equal(t, "contents", flag.files["a.json"])

	require.ErrorContains(t, flag.Set("missing-separator"), "expected <relpath>=<hostpath>")
	require.ErrorContains(t, flag.Set("=/only/host"), "expected <relpath>=<hostpath>")
	require.ErrorContains(t, flag.Set("rel="), "expected <relpath>=<hostpath>")
	require.ErrorContains(t, flag.Set("rel="+filepath.Join(t.TempDir(), "does-not-exist")), "read seed file")
}

func TestRunVersion(t *testing.T) {
	originalAgentVersion := agentVersion
	t.Cleanup(func() { agentVersion = originalAgentVersion })
	agentVersion = func() string { return "v9.9.9" }

	var stdout bytes.Buffer
	code := run(context.Background(), []string{"-version"}, bytes.NewBuffer(nil), &stdout, bytes.NewBuffer(nil))

	require.Equal(t, 0, code)
	require.Equal(t, "v9.9.9\n", stdout.String())
}

func TestRunErrorBranches(t *testing.T) {
	originalServe := serve
	originalShutdown := shutdownOpenTelemetry
	originalAgentVersion := agentVersion
	t.Cleanup(func() {
		serve = originalServe
		shutdownOpenTelemetry = originalShutdown
		agentVersion = originalAgentVersion
	})
	agentVersion = func() string { return "v1.2.3" }

	code := run(context.Background(), []string{"-bad"}, bytes.NewBuffer(nil), bytes.NewBuffer(nil), bytes.NewBuffer(nil))
	require.Equal(t, 2, code)

	serve = func(context.Context, io.Reader, io.Writer, ...claudeacp.Option) error {
		return errors.New("serve failed")
	}
	shutdownOpenTelemetry = func(context.Context, func(context.Context) error) error { return nil }
	var stderr bytes.Buffer
	code = run(context.Background(), nil, bytes.NewBuffer(nil), bytes.NewBuffer(nil), &stderr)
	require.Equal(t, 1, code)
	require.Contains(t, stderr.String(), "serve failed")

	serve = func(ctx context.Context, _ io.Reader, _ io.Writer, _ ...claudeacp.Option) error {
		return ctx.Err()
	}
	shutdownOpenTelemetry = func(context.Context, func(context.Context) error) error { return errors.New("shutdown failed") }
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	stderr.Reset()
	code = run(ctx, nil, bytes.NewBuffer(nil), bytes.NewBuffer(nil), &stderr)
	require.Equal(t, 1, code)
	require.Contains(t, stderr.String(), "shutdown OpenTelemetry")
}

func TestPendingSignalAndSignalCode(t *testing.T) {
	signals := make(chan os.Signal, 1)
	require.Nil(t, pendingSignal(signals))
	signals <- syscall.SIGTERM
	require.Equal(t, syscall.SIGTERM, pendingSignal(signals))
	require.Equal(t, 128+int(syscall.SIGTERM), signalCode(syscall.SIGTERM))
	require.Equal(t, 1, signalCode(fakeSignal("fake")))
}

func TestRunReturnsSignalCode(t *testing.T) {
	originalServe := serve
	originalShutdown := shutdownOpenTelemetry
	t.Cleanup(func() {
		serve = originalServe
		shutdownOpenTelemetry = originalShutdown
	})

	serve = func(ctx context.Context, _ io.Reader, _ io.Writer, _ ...claudeacp.Option) error {
		require.NoError(t, syscall.Kill(os.Getpid(), syscall.SIGTERM))
		<-ctx.Done()

		return ctx.Err()
	}
	shutdownOpenTelemetry = func(context.Context, func(context.Context) error) error { return nil }

	code := run(context.Background(), nil, bytes.NewBuffer(nil), bytes.NewBuffer(nil), bytes.NewBuffer(nil))
	require.Equal(t, 128+int(syscall.SIGTERM), code)
}

func TestMainExitBranch(t *testing.T) {
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

func (s fakeSignal) Signal() {}
