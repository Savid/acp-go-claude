package main

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/rogpeppe/go-internal/testscript"
	claudeacp "github.com/savid/acp-go-claude"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

func TestMain(m *testing.M) {
	testscript.Main(goleakTestMain{m: m}, map[string]func(){
		"acp-go-claude": testscriptACPGoClaude,
	})
}

type goleakTestMain struct {
	m *testing.M
}

func (m goleakTestMain) Run() int {
	code := m.m.Run()
	if code != 0 {
		return code
	}

	if err := goleak.Find(); err != nil {
		_, _ = fmt.Fprintf(os.Stderr, "goleak: Errors on successful test run: %v\n", err)

		return 1
	}

	return 0
}

func testscriptACPGoClaude() {
	os.Exit(run(context.Background(), os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func TestRunPassesOptions(t *testing.T) {
	originalServe := serve
	originalAgentVersion := agentVersion
	t.Cleanup(func() { serve = originalServe })
	t.Cleanup(func() { agentVersion = originalAgentVersion })

	var got claudeacp.Options
	serve = func(
		_ context.Context,
		_ io.Reader,
		_ io.Writer,
		opts ...claudeacp.Option,
	) error {
		for _, opt := range opts {
			opt(&got)
		}

		return nil
	}
	agentVersion = func() string { return "v9.8.7" }

	code := run(
		context.Background(),
		[]string{
			"-claude", "/bin/claude",
			"-claude-home", "/tmp/claude",
			"-model", "sonnet",
			"-bare",
			"-hide-claude-auth",
			"-debug",
		},
		bytes.NewBuffer(nil),
		bytes.NewBuffer(nil),
		bytes.NewBuffer(nil),
	)

	require.Equal(t, 0, code)
	require.Equal(t, "v9.8.7", got.AgentVersion)
	require.Equal(t, "/bin/claude", got.ClaudePath)
	require.Equal(t, "/tmp/claude", got.ClaudeHome)
	require.Equal(t, "sonnet", got.DefaultModel)
	require.True(t, got.BareMode)
	require.True(t, got.HideClaudeAuth)
	require.NotNil(t, got.Logger)
	require.IsType(t, &slog.Logger{}, got.Logger)
}

func TestMainFunctionReturnsNormallyOnZero(t *testing.T) {
	originalServe := serve
	originalRunMCPProxy := runMCPProxy
	originalRunClaudeCLI := runClaudeCLI
	originalExit := exit
	originalArgs := os.Args
	t.Cleanup(func() {
		serve = originalServe
		runMCPProxy = originalRunMCPProxy
		runClaudeCLI = originalRunClaudeCLI
		exit = originalExit
		os.Args = originalArgs
	})

	serve = func(context.Context, io.Reader, io.Writer, ...claudeacp.Option) error {
		return nil
	}

	exit = func(code int) {
		t.Fatalf("exit called for successful main with code %d", code)
	}
	os.Args = []string{"acp-go-claude"}

	main()
}

func TestMainFunctionExitsNonZero(t *testing.T) {
	originalServe := serve
	originalExit := exit
	originalArgs := os.Args
	t.Cleanup(func() {
		serve = originalServe
		exit = originalExit
		os.Args = originalArgs
	})

	serve = func(context.Context, io.Reader, io.Writer, ...claudeacp.Option) error {
		return errors.New("boom")
	}

	var gotCode int
	exit = func(code int) {
		gotCode = code
	}
	os.Args = []string{"acp-go-claude"}

	main()

	require.Equal(t, 1, gotCode)
}

func TestMCPProxyScripts(t *testing.T) {
	testscript.Run(t, testscript.Params{
		Dir:                 "testdata/script",
		RequireExplicitExec: true,
	})
}

func TestRunCLIBranch(t *testing.T) {
	originalRunClaudeCLI := runClaudeCLI
	t.Cleanup(func() { runClaudeCLI = originalRunClaudeCLI })

	runClaudeCLI = func(context.Context, []string, io.Reader, io.Writer, io.Writer) int {
		return 17
	}

	code := run(context.Background(), []string{"--cli", "auth", "login"}, bytes.NewBuffer(nil), bytes.NewBuffer(nil), bytes.NewBuffer(nil))

	require.Equal(t, 17, code)
}

func TestRunCLI(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses shell scripts")
	}

	script := filepath.Join(t.TempDir(), "claude")
	require.NoError(t, os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s|%s|%s' \"$1\" \"$2\" \"$CLAUDE_CONFIG_DIR\"\n"), 0o755))

	var stdout bytes.Buffer
	code := runCLI(
		context.Background(),
		[]string{"-claude", script, "-claude-home", "/tmp/claude-home", "auth", "login"},
		bytes.NewBuffer(nil),
		&stdout,
		bytes.NewBuffer(nil),
	)

	require.Equal(t, 0, code)
	require.Equal(t, "auth|login|/tmp/claude-home", stdout.String())
}

func TestRunCLIDiscoversClaude(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses shell scripts")
	}

	binDir := t.TempDir()
	script := filepath.Join(binDir, "claude")
	require.NoError(t, os.WriteFile(script, []byte("#!/bin/sh\nprintf discovered\n"), 0o755))
	t.Setenv("PATH", binDir)

	var stdout bytes.Buffer
	code := runCLI(context.Background(), nil, bytes.NewBuffer(nil), &stdout, bytes.NewBuffer(nil))

	require.Equal(t, 0, code)
	require.Equal(t, "discovered", stdout.String())
}

func TestRunCLIUsesClaudeExecutableEnv(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses shell scripts")
	}

	script := filepath.Join(t.TempDir(), "claude-env")
	require.NoError(t, os.WriteFile(script, []byte("#!/bin/sh\nprintf env-path\n"), 0o755))
	t.Setenv("CLAUDE_CODE_EXECUTABLE", script)

	var stdout bytes.Buffer
	code := runCLI(context.Background(), nil, bytes.NewBuffer(nil), &stdout, bytes.NewBuffer(nil))

	require.Equal(t, 0, code)
	require.Equal(t, "env-path", stdout.String())
}

func TestRunCLIErrors(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses shell scripts")
	}

	var stderr bytes.Buffer
	code := runCLI(context.Background(), []string{"-unknown"}, bytes.NewBuffer(nil), bytes.NewBuffer(nil), &stderr)
	require.Equal(t, 2, code)
	require.Contains(t, stderr.String(), "flag provided but not defined")

	stderr.Reset()
	t.Setenv("PATH", t.TempDir())
	code = runCLI(context.Background(), nil, bytes.NewBuffer(nil), bytes.NewBuffer(nil), &stderr)
	require.Equal(t, 1, code)
	require.Contains(t, stderr.String(), "find claude in PATH")

	stderr.Reset()
	script := filepath.Join(t.TempDir(), "claude")
	require.NoError(t, os.WriteFile(script, []byte("#!/bin/sh\nexit 7\n"), 0o755))
	code = runCLI(context.Background(), []string{"-claude", script}, bytes.NewBuffer(nil), bytes.NewBuffer(nil), &stderr)
	require.Equal(t, 7, code)
	require.Contains(t, stderr.String(), "exit status 7")

	stderr.Reset()
	code = runCLI(context.Background(), []string{"-claude", t.TempDir()}, bytes.NewBuffer(nil), bytes.NewBuffer(nil), &stderr)
	require.Equal(t, 1, code)
	require.Contains(t, stderr.String(), cliCommandName)
}

func TestRunCLIForwardsSignals(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses POSIX signals")
	}

	dir := t.TempDir()
	script := filepath.Join(dir, "claude")
	forwarded := filepath.Join(dir, "forwarded")
	ready := filepath.Join(dir, "ready")
	require.NoError(t, os.WriteFile(script, []byte(`#!/bin/sh
trap 'printf forwarded > "$1"; exit 0' TERM HUP INT
printf ready > "$2"
while :; do sleep 1; done
`), 0o755))

	done := make(chan int, 1)
	go func() {
		done <- runCLI(
			context.Background(),
			[]string{"-claude", script, forwarded, ready},
			bytes.NewBuffer(nil),
			bytes.NewBuffer(nil),
			bytes.NewBuffer(nil),
		)
	}()

	require.Eventually(t, func() bool {
		_, err := os.Stat(ready)

		return err == nil
	}, time.Second, 10*time.Millisecond)
	time.Sleep(50 * time.Millisecond)
	require.NoError(t, syscall.Kill(syscall.Getpid(), syscall.SIGTERM))

	select {
	case code := <-done:
		require.Equal(t, 0, code)
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for forwarded signal")
	}

	content, err := os.ReadFile(forwarded)
	require.NoError(t, err)
	require.Equal(t, "forwarded", string(content))
}

func TestCommandExitCodeSignalsAndFallback(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses POSIX signals")
	}

	err := exec.Command("sh", "-c", "kill -TERM $$").Run()
	var exitErr *exec.ExitError
	require.ErrorAs(t, err, &exitErr)
	require.Equal(t, 128+int(syscall.SIGTERM), commandExitCode(exitErr))

	err = exec.Command("sh", "-c", "exit 7").Run()
	require.ErrorAs(t, err, &exitErr)
	require.Zero(t, signalExitCode(exitErr))
	require.Equal(t, 1, commandExitCode(errors.New("failed")))
	require.Equal(t, 130, signalCode(os.Interrupt))
	require.Equal(t, 1, signalCode(testSignal("custom")))
	require.Nil(t, pendingSignal(make(chan os.Signal, 1)))

	signals := make(chan os.Signal, 1)
	signals <- os.Interrupt
	require.Equal(t, os.Interrupt, pendingSignal(signals))
}

type testSignal string

func (s testSignal) Signal() {}

func (s testSignal) String() string {
	return string(s)
}

func TestRunProxy(t *testing.T) {
	originalRunMCPProxy := runMCPProxy
	t.Cleanup(func() { runMCPProxy = originalRunMCPProxy })

	var got claudeacp.MCPProxyOptions
	runMCPProxy = func(_ context.Context, _ io.Reader, _ io.Writer, options claudeacp.MCPProxyOptions) error {
		got = options

		return nil
	}

	tokenFile := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(tokenFile, []byte(" file-secret\n"), 0o600))
	t.Setenv(claudeacp.MCPProxyTokenFileEnv, tokenFile)
	code := run(
		context.Background(),
		[]string{"mcp-proxy", "-network", "tcp", "-address", "127.0.0.1:1", "-acp-id", "server-1"},
		bytes.NewBuffer(nil),
		bytes.NewBuffer(nil),
		bytes.NewBuffer(nil),
	)

	require.Equal(t, 0, code)
	require.Equal(t, "tcp", got.Network)
	require.Equal(t, "127.0.0.1:1", got.Address)
	require.Equal(t, "file-secret", got.Token)
	require.Equal(t, "server-1", got.ACPID)
	require.Empty(t, readTokenFile(""))
	require.Empty(t, readTokenFile(filepath.Join(t.TempDir(), "missing")))
}

func TestRunProxyValidation(t *testing.T) {
	originalRunMCPProxy := runMCPProxy
	t.Cleanup(func() { runMCPProxy = originalRunMCPProxy })

	runMCPProxy = func(context.Context, io.Reader, io.Writer, claudeacp.MCPProxyOptions) error {
		t.Fatal("proxy should not run")

		return nil
	}

	var stderr bytes.Buffer
	code := run(context.Background(), []string{"mcp-proxy", "-address", "127.0.0.1:1"}, bytes.NewBuffer(nil), bytes.NewBuffer(nil), &stderr)

	require.Equal(t, 2, code)
	require.Contains(t, stderr.String(), "required")
}

func TestRunProxyHandlesFlagAndRuntimeErrors(t *testing.T) {
	originalRunMCPProxy := runMCPProxy
	t.Cleanup(func() { runMCPProxy = originalRunMCPProxy })

	runMCPProxy = func(context.Context, io.Reader, io.Writer, claudeacp.MCPProxyOptions) error {
		return errors.New("proxy failed")
	}

	tokenFile := filepath.Join(t.TempDir(), "token")
	require.NoError(t, os.WriteFile(tokenFile, []byte("secret"), 0o600))
	t.Setenv(claudeacp.MCPProxyTokenFileEnv, tokenFile)

	var stderr bytes.Buffer
	code := run(
		context.Background(),
		[]string{"mcp-proxy", "-network", "tcp", "-address", "127.0.0.1:1", "-acp-id", "server-1"},
		bytes.NewBuffer(nil),
		bytes.NewBuffer(nil),
		&stderr,
	)
	require.Equal(t, 1, code)
	require.Contains(t, stderr.String(), "proxy failed")

	code = run(context.Background(), []string{"mcp-proxy", "-unknown"}, bytes.NewBuffer(nil), bytes.NewBuffer(nil), &stderr)
	require.Equal(t, 2, code)

	code = run(context.Background(), []string{"mcp-proxy", "-token", "secret"}, bytes.NewBuffer(nil), bytes.NewBuffer(nil), &stderr)
	require.Equal(t, 2, code)
}

func TestRunHandlesErrors(t *testing.T) {
	originalServe := serve
	t.Cleanup(func() { serve = originalServe })

	serve = func(context.Context, io.Reader, io.Writer, ...claudeacp.Option) error {
		return errors.New("boom")
	}

	var stderr bytes.Buffer
	code := run(context.Background(), nil, bytes.NewBuffer(nil), bytes.NewBuffer(nil), &stderr)

	require.Equal(t, 1, code)
	require.Contains(t, stderr.String(), "acp-go-claude: boom")
}

func TestRunHandlesSignals(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses POSIX signals")
	}

	originalServe := serve
	t.Cleanup(func() { serve = originalServe })

	started := make(chan struct{})
	serve = func(ctx context.Context, _ io.Reader, _ io.Writer, _ ...claudeacp.Option) error {
		close(started)
		<-ctx.Done()

		return ctx.Err()
	}

	done := make(chan int, 1)
	go func() {
		done <- run(context.Background(), nil, bytes.NewBuffer(nil), bytes.NewBuffer(nil), bytes.NewBuffer(nil))
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("serve did not start")
	}

	require.NoError(t, syscall.Kill(syscall.Getpid(), syscall.SIGTERM))

	select {
	case code := <-done:
		require.Equal(t, signalCode(syscall.SIGTERM), code)
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for run to exit after signal")
	}
}

func TestRunHandlesFlagErrors(t *testing.T) {
	originalServe := serve
	t.Cleanup(func() { serve = originalServe })

	serve = func(context.Context, io.Reader, io.Writer, ...claudeacp.Option) error {
		t.Fatal("serve should not be called")

		return nil
	}

	var stderr bytes.Buffer
	code := run(context.Background(), []string{"-unknown"}, bytes.NewBuffer(nil), bytes.NewBuffer(nil), &stderr)

	require.Equal(t, 2, code)
	require.Contains(t, stderr.String(), "flag provided but not defined")
}
