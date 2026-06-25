package claude

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type bufferWriteCloser struct {
	*bytes.Buffer

	closed bool
}

func (w *bufferWriteCloser) Close() error {
	w.closed = true

	return nil
}

type lockedWriteCloser struct {
	mu sync.Mutex

	data   bytes.Buffer
	closed bool
}

type errorWriteCloser struct{}

type errorReadCloser struct {
	err error
}

type deadlineWriteCloser struct {
	*bufferWriteCloser

	mu        sync.Mutex
	deadlines []time.Time
}

func (w *deadlineWriteCloser) SetWriteDeadline(deadline time.Time) error {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.deadlines = append(w.deadlines, deadline)

	return nil
}

func (w *deadlineWriteCloser) recordedDeadlines() []time.Time {
	w.mu.Lock()
	defer w.mu.Unlock()

	return append([]time.Time(nil), w.deadlines...)
}

type cancelDeadlineWriteCloser struct {
	started  chan struct{}
	deadline chan time.Time
	once     sync.Once
}

func (w errorWriteCloser) Write([]byte) (int, error) {
	return 0, errors.New("write failed")
}

func (w errorWriteCloser) Close() error {
	return nil
}

func (r errorReadCloser) Read([]byte) (int, error) {
	return 0, r.err
}

func (r errorReadCloser) Close() error {
	return nil
}

func (w *cancelDeadlineWriteCloser) Write([]byte) (int, error) {
	w.once.Do(func() {
		close(w.started)
	})
	<-w.deadline

	return 0, errors.New("write deadline exceeded")
}

func (w *cancelDeadlineWriteCloser) Close() error {
	return nil
}

func (w *cancelDeadlineWriteCloser) SetWriteDeadline(deadline time.Time) error {
	if !deadline.IsZero() {
		select {
		case w.deadline <- deadline:
		default:
		}
	}

	return nil
}

func (w *lockedWriteCloser) Write(data []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.data.Write(data)
}

func (w *lockedWriteCloser) Close() error {
	w.mu.Lock()
	defer w.mu.Unlock()

	w.closed = true

	return nil
}

func (w *lockedWriteCloser) isClosed() bool {
	w.mu.Lock()
	defer w.mu.Unlock()

	return w.closed
}

func TestProcessTransportSend(t *testing.T) {
	t.Parallel()

	stdin := &bufferWriteCloser{Buffer: &bytes.Buffer{}}
	transport := &ProcessTransport{stdin: stdin}

	require.NoError(t, transport.Send(context.Background(), map[string]any{"type": "user"}))
	require.JSONEq(t, `{"type":"user"}`, strings.TrimSpace(stdin.String()))
}

func TestProcessTransportSendErrors(t *testing.T) {
	t.Parallel()

	require.Error(t, (&ProcessTransport{}).Send(context.Background(), map[string]any{}))

	transport := &ProcessTransport{stdin: &bufferWriteCloser{Buffer: &bytes.Buffer{}}}
	require.Error(t, transport.Send(context.Background(), map[string]any{"bad": func() {}}))

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	require.ErrorIs(t, transport.Send(ctx, map[string]any{}), context.Canceled)

	transport = &ProcessTransport{stdin: errorWriteCloser{}}
	require.Error(t, transport.Send(context.Background(), map[string]any{}))
}

func TestProcessTransportSendRejectsOversizedPayload(t *testing.T) {
	oldMaxJSONLineBytes := maxJSONLineBytes
	maxJSONLineBytes = 12
	t.Cleanup(func() { maxJSONLineBytes = oldMaxJSONLineBytes })

	transport := &ProcessTransport{stdin: &bufferWriteCloser{Buffer: &bytes.Buffer{}}}
	err := transport.Send(context.Background(), map[string]any{"type": "oversized"})

	require.Error(t, err)
	require.Contains(t, err.Error(), "claude stdin json line exceeds")
}

func TestProcessTransportSendUsesContextWriteDeadline(t *testing.T) {
	t.Parallel()

	stdin := &deadlineWriteCloser{bufferWriteCloser: &bufferWriteCloser{Buffer: &bytes.Buffer{}}}
	transport := &ProcessTransport{stdin: stdin}
	deadline := time.Now().Add(time.Minute)
	ctx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()

	require.NoError(t, transport.Send(ctx, map[string]any{"type": "user"}))

	deadlines := stdin.recordedDeadlines()
	require.Len(t, deadlines, 2)
	require.Equal(t, deadline, deadlines[0])
	require.True(t, deadlines[1].IsZero())
}

func TestProcessTransportSendContextCancelUnblocksDeadlineWriter(t *testing.T) {
	t.Parallel()

	stdin := &cancelDeadlineWriteCloser{
		started:  make(chan struct{}),
		deadline: make(chan time.Time, 1),
	}
	transport := &ProcessTransport{stdin: stdin}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)

	go func() {
		done <- transport.Send(ctx, map[string]any{"type": "user"})
	}()

	<-stdin.started
	cancel()

	require.ErrorIs(t, <-done, context.Canceled)
}

func TestProcessTransportMessages(t *testing.T) {
	t.Parallel()

	transport := &ProcessTransport{
		stdout: io.NopCloser(strings.NewReader("noise\n{\"type\":\"assistant\"}\n{\"bad\"\n{\"type\":\"result\"}\n")),
	}
	messages, errs := transport.Messages(context.Background())

	var got []map[string]any
	for msg := range messages {
		got = append(got, msg)
	}

	require.Len(t, got, 2)
	require.Equal(t, MessageTypeAssistant, got[0][keyType])
	require.Equal(t, MessageTypeResult, got[1][keyType])

	err := <-errs
	require.Error(t, err)
	require.Contains(t, err.Error(), "decode claude json line")
}

func TestProcessTransportRecoverStderrDrainClosesTransport(t *testing.T) {
	t.Parallel()

	stdin := &bufferWriteCloser{Buffer: &bytes.Buffer{}}
	transport := &ProcessTransport{
		log:   slog.New(slog.DiscardHandler),
		stdin: stdin,
	}

	func() {
		defer transport.recoverStderrDrain(context.Background())

		panic("boom")
	}()

	require.True(t, stdin.closed)
}

func TestProcessTransportRecoverStdoutReaderReportsAndClosesTransport(t *testing.T) {
	t.Parallel()

	stdin := &bufferWriteCloser{Buffer: &bytes.Buffer{}}
	errs := make(chan error, 1)
	transport := &ProcessTransport{
		log:   slog.New(slog.DiscardHandler),
		stdin: stdin,
	}

	func() {
		defer transport.recoverStdoutReader(context.Background(), errs)

		panic("boom")
	}()

	require.ErrorContains(t, <-errs, "claude stdout reader panic: boom")
	require.True(t, stdin.closed)
}

func TestProcessTransportMessagesRecoversStdoutPanic(t *testing.T) {
	afterDecode := processAfterDecode
	processAfterDecode = func() { panic("boom") }
	t.Cleanup(func() { processAfterDecode = afterDecode })

	stdin := &bufferWriteCloser{Buffer: &bytes.Buffer{}}
	transport := &ProcessTransport{
		log:    slog.New(slog.DiscardHandler),
		stdin:  stdin,
		stdout: io.NopCloser(strings.NewReader("{\"type\":\"assistant\"}\n")),
	}
	messages, errs := transport.Messages(context.Background())

	for range messages {
	}

	require.ErrorContains(t, <-errs, "claude stdout reader panic: boom")
	require.True(t, stdin.closed)
}

func TestProcessTransportMessagesIsSingleUse(t *testing.T) {
	t.Parallel()

	transport := &ProcessTransport{
		stdout: io.NopCloser(strings.NewReader("{\"type\":\"assistant\"}\n")),
	}
	firstMessages, firstErrs := transport.Messages(context.Background())
	secondMessages, secondErrs := transport.Messages(context.Background())

	_, ok := <-secondMessages
	require.False(t, ok)
	require.ErrorIs(t, <-secondErrs, errMessagesAlreadyStarted)

	var got []map[string]any
	for msg := range firstMessages {
		got = append(got, msg)
	}
	for range firstErrs {
	}

	require.Len(t, got, 1)
}

func TestProcessTransportMessagesAfterClose(t *testing.T) {
	t.Parallel()

	transport := &ProcessTransport{
		stdout: io.NopCloser(strings.NewReader("{\"type\":\"assistant\"}\n")),
	}
	require.NoError(t, transport.Close())

	messages, errs := transport.Messages(context.Background())
	_, ok := <-messages
	require.False(t, ok)
	require.ErrorIs(t, <-errs, ErrClientClosed)
}

func TestProcessTransportMessagesContextCancelled(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	transport := &ProcessTransport{
		stdout: io.NopCloser(strings.NewReader("{\"type\":\"assistant\"}\n")),
	}
	messages, errs := transport.Messages(ctx)

	var got []map[string]any
	for msg := range messages {
		got = append(got, msg)
	}

	require.Empty(t, got)
	require.ErrorIs(t, <-errs, context.Canceled)
}

func TestProcessTransportMessagesCancelledWhileSending(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	afterDecode := processAfterDecode
	processAfterDecode = cancel
	t.Cleanup(func() {
		processAfterDecode = afterDecode
	})

	transport := &ProcessTransport{
		stdout: io.NopCloser(strings.NewReader("{\"type\":\"assistant\"}\n")),
	}
	messages, errs := transport.Messages(ctx)

	require.ErrorIs(t, <-errs, context.Canceled)
	_, ok := <-messages
	require.False(t, ok)
}

func TestProcessTransportMessagesContextCancelWaitsForProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses /bin/sh")
	}

	cmd := exec.Command("sh", "-c", "exit 0")
	require.NoError(t, cmd.Start())

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	transport := &ProcessTransport{
		cmd:    cmd,
		stdout: io.NopCloser(strings.NewReader("{\"type\":\"assistant\"}\n")),
	}
	messages, errs := transport.Messages(ctx)
	for range messages {
	}
	for range errs {
	}

	require.NotNil(t, cmd.ProcessState)
}

func TestProcessTransportMessagesReadAndWaitErrors(t *testing.T) {
	t.Parallel()

	transport := &ProcessTransport{
		stdout: errorReadCloser{err: errors.New("read failed")},
	}
	messages, errs := transport.Messages(context.Background())
	for range messages {
	}
	err := <-errs
	require.Error(t, err)
	require.Contains(t, err.Error(), "read claude stdout")

	cmd := exec.Command("sh", "-c", "exit 7")
	require.NoError(t, cmd.Start())

	transport = &ProcessTransport{
		cmd:    cmd,
		stdout: io.NopCloser(strings.NewReader("")),
	}
	messages, errs = transport.Messages(context.Background())
	for range messages {
	}

	err = <-errs
	require.Error(t, err)
	require.Contains(t, err.Error(), "claude exited")
}

func TestProcessTransportMessagesReadAndWaitErrorsDoNotBlock(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses /bin/sh")
	}

	cmd := exec.Command("sh", "-c", "exit 7")
	require.NoError(t, cmd.Start())

	transport := &ProcessTransport{
		cmd:    cmd,
		stdout: io.NopCloser(strings.NewReader("{bad\n")),
	}
	messages, errs := transport.Messages(context.Background())
	for range messages {
	}

	var got []error
	for err := range errs {
		got = append(got, err)
	}

	require.Len(t, got, 2)
	require.Contains(t, got[0].Error(), "decode claude json line")
	require.Contains(t, got[1].Error(), "claude exited")
}

func TestProcessTransportMessagesPreservesBurstErrors(t *testing.T) {
	t.Parallel()

	transport := &ProcessTransport{
		stdout: io.NopCloser(strings.NewReader("{bad\n{worse\n{nope\n{still-bad\n")),
	}
	messages, errs := transport.Messages(context.Background())
	for range messages {
	}

	var got []error
	for err := range errs {
		got = append(got, err)
	}

	require.Len(t, got, 4)
	for _, err := range got {
		require.Contains(t, err.Error(), "decode claude json line")
	}
}

func TestProcessTransportMessagesAcceptsLargeJSONLines(t *testing.T) {
	t.Parallel()

	large := strings.Repeat("x", maxJSONLineBytes/2)
	transport := &ProcessTransport{
		stdout: io.NopCloser(strings.NewReader(`{"type":"assistant","text":"` + large + `"}`)),
	}
	messages, errs := transport.Messages(context.Background())

	msg := <-messages
	require.Equal(t, MessageTypeAssistant, msg[keyType])
	require.Len(t, msg["text"], len(large))

	_, ok := <-messages
	require.False(t, ok)
	require.Empty(t, errs)
}

func TestSendTransportErrorIgnoresNil(t *testing.T) {
	t.Parallel()

	errs := make(chan error, 1)
	require.True(t, sendTransportError(errs, nil))

	require.Empty(t, errs)
}

func TestProcessTransportLogsDroppedErrors(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	transport := &ProcessTransport{
		log: slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
	}
	errs := make(chan error, 1)
	errs <- errors.New("first")

	transport.sendError(errs, errors.New("second"))

	require.Contains(t, logs.String(), "dropped claude transport error")
	require.Contains(t, logs.String(), "second")

	(&ProcessTransport{}).sendError(errs, errors.New("third"))
}

func TestProcessTransportDrainStderr(t *testing.T) {
	t.Parallel()

	(&ProcessTransport{}).drainStderr()

	var logs bytes.Buffer
	transport := &ProcessTransport{
		log:    slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		stderr: io.NopCloser(strings.NewReader("first\nsecond\n")),
	}

	transport.drainStderr()

	require.Contains(t, logs.String(), "first")
	require.Contains(t, logs.String(), "second")
}

func TestProcessTransportMessagesRejectsOversizedLine(t *testing.T) {
	t.Parallel()

	transport := &ProcessTransport{
		stdout: io.NopCloser(strings.NewReader(`{"type":"` + strings.Repeat("x", maxJSONLineBytes+1) + `"}` + "\n")),
	}
	messages, errs := transport.Messages(context.Background())

	for range messages {
		t.Fatal("oversized line should not produce a message")
	}

	var gotErr error
	for err := range errs {
		gotErr = err
	}
	require.Error(t, gotErr)
	require.Contains(t, gotErr.Error(), "read claude stdout")
}

func TestProcessTransportCloseStopsStderrDrain(t *testing.T) {
	t.Parallel()

	stderrReader, stderrWriter := io.Pipe()
	t.Cleanup(func() { _ = stderrWriter.Close() })

	transport := &ProcessTransport{
		stdout: io.NopCloser(strings.NewReader("")),
		stderr: stderrReader,
	}
	messages, errs := transport.Messages(context.Background())
	for range messages {
	}
	for range errs {
	}

	closed := make(chan error, 1)
	go func() {
		closed <- transport.Close()
	}()

	select {
	case err := <-closed:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for transport close")
	}
}

func TestProcessTransportClose(t *testing.T) {
	t.Parallel()

	stdin := &lockedWriteCloser{}
	transport := &ProcessTransport{stdin: stdin}

	require.NoError(t, transport.Close())
	require.True(t, stdin.isClosed())
	require.NoError(t, transport.Close())
}

func TestProcessTransportCloseReapsProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses /bin/sh")
	}

	cmd := exec.Command("sh", "-c", "sleep 30")
	require.NoError(t, cmd.Start())

	transport := &ProcessTransport{cmd: cmd}
	require.NoError(t, transport.Close())
	require.NotNil(t, cmd.ProcessState)
}

func TestProcessTransportCloseSendsTermBeforeKill(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses /bin/sh")
	}

	dir := t.TempDir()
	marker := filepath.Join(dir, "term")
	ready := filepath.Join(dir, "ready")
	script := writeShellScript(t, filepath.Join(dir, "claude"), `#!/bin/sh
trap 'printf term > "$TERM_MARK"; exit 0' TERM
printf ready > "$READY_MARK"
while :; do :; done
`)
	transport := NewProcessTransport(nil, Options{
		CLIPath: script,
		Env:     map[string]string{"READY_MARK": ready, "TERM_MARK": marker},
	})

	require.NoError(t, transport.Start(context.Background()))
	require.Eventually(t, func() bool {
		_, err := os.Stat(ready)

		return err == nil
	}, 5*time.Second, 10*time.Millisecond)
	require.NoError(t, transport.Close())

	data, err := os.ReadFile(marker)
	require.NoError(t, err)
	require.Equal(t, "term", string(data))
}

func TestProcessTransportCloseKillsAfterTermTimeout(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses /bin/sh")
	}

	oldWaitDelay := processShutdownWaitDelay
	processShutdownWaitDelay = 20 * time.Millisecond
	t.Cleanup(func() { processShutdownWaitDelay = oldWaitDelay })

	dir := t.TempDir()
	script := writeShellScript(t, filepath.Join(dir, "claude"), `#!/bin/sh
trap '' TERM
while :; do sleep 1; done
`)
	transport := NewProcessTransport(nil, Options{CLIPath: script})

	require.NoError(t, transport.Start(context.Background()))
	require.NoError(t, transport.Close())
	require.NotNil(t, transport.cmd.ProcessState)
}

func TestProcessTransportCloseReportsTerminateError(t *testing.T) {
	oldTerminate := processTerminate
	processTerminate = func(*exec.Cmd) (bool, error) {
		return false, errors.New("terminate failed")
	}
	t.Cleanup(func() { processTerminate = oldTerminate })

	cmd := exec.Command("sh", "-c", "exit 0")
	cmd.Process = &os.Process{Pid: os.Getpid()}
	transport := &ProcessTransport{cmd: cmd}

	err := transport.Close()
	require.Error(t, err)
	require.Contains(t, err.Error(), "terminate failed")
}

func TestProcessTransportCloseReportsKillError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses /bin/sh")
	}

	oldWaitDelay := processShutdownWaitDelay
	processShutdownWaitDelay = 20 * time.Millisecond
	t.Cleanup(func() { processShutdownWaitDelay = oldWaitDelay })

	oldKill := processKill
	processKill = func(*exec.Cmd) (bool, error) {
		return false, errors.New("kill failed")
	}
	t.Cleanup(func() { processKill = oldKill })

	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	script := writeShellScript(t, filepath.Join(dir, "claude"), `#!/bin/sh
trap '' TERM
printf ready > "$READY_MARK"
while :; do sleep 1; done
`)
	transport := NewProcessTransport(nil, Options{
		CLIPath: script,
		Env:     map[string]string{"READY_MARK": ready},
	})

	require.NoError(t, transport.Start(context.Background()))
	require.Eventually(t, func() bool {
		_, err := os.Stat(ready)

		return err == nil
	}, 5*time.Second, 10*time.Millisecond)

	err := transport.Close()
	require.Error(t, err)
	require.Contains(t, err.Error(), "kill failed")

	processKill = oldKill
	_, _ = killProcess(transport.cmd)
	_ = transport.wait()
}

func TestProcessTransportCloseWithoutWaitDelay(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses /bin/sh")
	}

	oldWaitDelay := processShutdownWaitDelay
	processShutdownWaitDelay = 0
	t.Cleanup(func() { processShutdownWaitDelay = oldWaitDelay })

	dir := t.TempDir()
	script := writeShellScript(t, filepath.Join(dir, "claude"), `#!/bin/sh
trap 'exit 0' TERM
while :; do sleep 1; done
`)
	transport := NewProcessTransport(nil, Options{CLIPath: script})

	require.NoError(t, transport.Start(context.Background()))
	require.NoError(t, transport.Close())
}

func TestProcessTransportWaitForShutdownWithoutWaitDelayReturnsWaitError(t *testing.T) {
	oldWaitDelay := processShutdownWaitDelay
	processShutdownWaitDelay = 0
	t.Cleanup(func() { processShutdownWaitDelay = oldWaitDelay })

	waitErr := errors.New("wait failed")
	transport := &ProcessTransport{}
	transport.waitOnce.Do(func() {
		transport.waitErr = waitErr
	})

	require.ErrorIs(t, transport.waitForShutdown(false), waitErr)
}

func TestWaitForProcessExitTimeout(t *testing.T) {
	err := waitForProcessExit(make(chan error), true, errors.New("close failed"), time.Millisecond)

	require.Error(t, err)
	require.Contains(t, err.Error(), "close failed")
	require.Contains(t, err.Error(), processWaitTimedOutMessage)
}

func TestWaitForProcessExitWaitError(t *testing.T) {
	waitErr := make(chan error, 1)
	waitErr <- errors.New("wait failed")

	err := waitForProcessExit(waitErr, false, errors.New("close failed"), time.Second)

	require.Error(t, err)
	require.Contains(t, err.Error(), "close failed")
	require.Contains(t, err.Error(), "wait failed")
}

func TestProcessTransportCloseStdoutNil(t *testing.T) {
	t.Parallel()

	require.NoError(t, (&ProcessTransport{}).closeStdout())
}

func TestProcessTransportCloseKillError(t *testing.T) {
	t.Parallel()

	cmd := exec.Command("sh", "-c", "exit 0")
	require.NoError(t, cmd.Start())
	require.NoError(t, cmd.Wait())

	transport := &ProcessTransport{cmd: cmd}
	require.Error(t, transport.Close())
	require.False(t, expectedShutdownProcessExit(errors.New("not an exit error"), true))
	require.True(t, expectedShutdownProcessExit(context.Canceled, true))
	require.True(t, expectedShutdownProcessExit(exec.ErrWaitDelay, true))
}

func writeShellScript(t *testing.T, path string, script string) string {
	t.Helper()

	require.NoError(t, os.WriteFile(path, []byte(script), 0o700))

	return path
}

func TestProcessTransportStartMissingBinary(t *testing.T) {
	t.Parallel()

	transport := NewProcessTransport(nil, Options{CLIPath: "/definitely/not/claude"})

	require.Error(t, transport.Start(context.Background()))
}

func TestProcessTransportStartDiscoveryError(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	transport := NewProcessTransport(nil, Options{})
	require.Error(t, transport.Start(context.Background()))
}

func TestProcessTransportStartBadWorkingDirectory(t *testing.T) {
	t.Parallel()

	transport := NewProcessTransport(nil, Options{
		CLIPath: "/bin/sh",
		Cwd:     filepath.Join(t.TempDir(), "missing"),
	})

	require.Error(t, transport.Start(context.Background()))
}

func TestProcessTransportStartSetupErrors(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses /bin/sh")
	}

	commandContext := processCommandContext
	getwd := processGetwd
	t.Cleanup(func() {
		processCommandContext = commandContext
		processGetwd = getwd
	})

	processGetwd = func() (string, error) {
		return "", errors.New("getwd failed")
	}
	transport := NewProcessTransport(nil, Options{CLIPath: "/bin/sh"})
	require.Error(t, transport.Start(context.Background()))
	processGetwd = getwd

	processCommandContext = func(ctx context.Context, name string, arg ...string) *exec.Cmd {
		cmd := commandContext(ctx, name, arg...)
		cmd.Stdin = strings.NewReader("")

		return cmd
	}
	transport = NewProcessTransport(nil, Options{CLIPath: "/bin/sh", Cwd: t.TempDir()})
	require.Error(t, transport.Start(context.Background()))

	processCommandContext = func(ctx context.Context, name string, arg ...string) *exec.Cmd {
		cmd := commandContext(ctx, name, arg...)
		cmd.Stdout = io.Discard

		return cmd
	}
	transport = NewProcessTransport(nil, Options{CLIPath: "/bin/sh", Cwd: t.TempDir()})
	require.Error(t, transport.Start(context.Background()))

	processCommandContext = func(ctx context.Context, name string, arg ...string) *exec.Cmd {
		cmd := commandContext(ctx, name, arg...)
		cmd.Stderr = io.Discard

		return cmd
	}
	transport = NewProcessTransport(nil, Options{CLIPath: "/bin/sh", Cwd: t.TempDir()})
	require.Error(t, transport.Start(context.Background()))
}

func TestProcessTransportStart(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := filepath.Join(dir, "fake-claude")
	require.NoError(t, os.WriteFile(script, []byte("#!/bin/sh\nwhile true; do sleep 1; done\n"), 0o755))

	transport := NewProcessTransport(nil, Options{
		CLIPath: script,
		Cwd:     dir,
	})
	require.NoError(t, transport.Start(context.Background()))
	require.Equal(t, dir, transport.cmd.Dir)
	require.NotNil(t, transport.stdin)
	require.NotNil(t, transport.stdout)
	require.NotNil(t, transport.stderr)
	require.NoError(t, transport.Close())

	transport = NewProcessTransport(nil, Options{CLIPath: script})
	require.NoError(t, transport.Start(context.Background()))
	require.NotEmpty(t, transport.cmd.Dir)
	require.NoError(t, transport.Close())
}
