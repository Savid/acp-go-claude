package claude

import (
	"bufio"
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

func platformTestTransportOptions(t *testing.T, options Options) Options {
	t.Helper()
	options.AcquireUsageDiscovery = func(context.Context) (func(), error) {
		return func() {}, nil
	}
	options.PrepareUsageGeneration = func(context.Context) (*DarwinGeneration, error) {
		root, err := os.MkdirTemp(t.TempDir(), "acp-go-claude-runtime-")
		if err != nil {
			return nil, err
		}

		return &DarwinGeneration{
			RuntimeID:   strings.Repeat("a", 32),
			ScratchRoot: root,
			Release: func(complete bool) error {
				if !complete {
					return nil
				}

				return os.RemoveAll(root)
			},
		}, nil
	}
	if runtime.GOOS != "darwin" {
		return options
	}

	options.DarwinBestEffort = true
	options.PrepareDarwinGeneration = func(context.Context) (*DarwinGeneration, error) {
		root, err := os.MkdirTemp(t.TempDir(), "acp-go-claude-runtime-")
		if err != nil {
			return nil, err
		}

		return &DarwinGeneration{
			RuntimeID:   strings.Repeat("a", 32),
			ScratchRoot: root,
			Release: func(complete bool) error {
				if !complete {
					return nil
				}

				return os.RemoveAll(root)
			},
		}, nil
	}
	options.PrepareDarwinVersionGeneration = options.PrepareDarwinGeneration

	return options
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

	// The malformed line is skipped and counted; it never tears the stream down
	// nor surfaces as an error, so both valid frames are delivered.
	require.Len(t, got, 2)
	require.Equal(t, MessageTypeAssistant, got[0][keyType])
	require.Equal(t, MessageTypeResult, got[1][keyType])
	require.Equal(t, uint64(1), transport.malformedLines.Load())

	_, ok := <-errs
	require.False(t, ok)
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

	// The malformed line is skipped (not surfaced); only the non-zero process
	// exit is reported, and it carries the real cause.
	require.Len(t, got, 1)
	require.Contains(t, got[0].Error(), "claude exited")
	require.ErrorIs(t, got[0], ErrProcessExited)
	require.Equal(t, uint64(1), transport.malformedLines.Load())
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

	// A burst of malformed lines is skipped and counted, never surfaced as
	// errors that would kill a live session.
	require.Empty(t, got)
	require.Equal(t, uint64(4), transport.malformedLines.Load())
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

func TestProcessTransportShutdownGraceReturnsUnexpectedWaitError(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses /bin/sh")
	}

	oldGrace := processExitGracePeriod
	processExitGracePeriod = time.Second
	t.Cleanup(func() { processExitGracePeriod = oldGrace })

	cmd := exec.Command("sh", "-c", "exit 0")
	require.NoError(t, cmd.Start())
	require.NoError(t, cmd.Wait())

	transport := &ProcessTransport{cmd: cmd}
	require.ErrorContains(t, transport.shutdownProcess(true), "Wait")
}

func TestProcessTransportCloseSendsTermBeforeKill(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses /bin/sh")
	}

	oldGrace := processExitGracePeriod
	processExitGracePeriod = 20 * time.Millisecond
	t.Cleanup(func() { processExitGracePeriod = oldGrace })

	dir := t.TempDir()
	marker := filepath.Join(dir, "term")
	ready := filepath.Join(dir, "ready")
	script := writeShellScript(t, filepath.Join(dir, "claude"), `#!/bin/sh
trap 'printf term > "$TERM_MARK"; exit 0' TERM
printf ready > "$READY_MARK"
while :; do :; done
`)
	transport := NewProcessTransport(nil, platformTestTransportOptions(t, Options{
		CLIPath: script,
		Env:     map[string]string{"READY_MARK": ready, "TERM_MARK": marker},
	}))

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
	processShutdownWaitDelay = 2 * time.Second
	t.Cleanup(func() { processShutdownWaitDelay = oldWaitDelay })

	oldGrace := processExitGracePeriod
	processExitGracePeriod = 20 * time.Millisecond
	t.Cleanup(func() { processExitGracePeriod = oldGrace })

	dir := t.TempDir()
	script := writeShellScript(t, filepath.Join(dir, "claude"), `#!/bin/sh
trap '' TERM
while :; do sleep 1; done
`)
	transport := NewProcessTransport(nil, platformTestTransportOptions(t, Options{CLIPath: script}))

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

	oldGrace := processExitGracePeriod
	processExitGracePeriod = 20 * time.Millisecond
	t.Cleanup(func() { processExitGracePeriod = oldGrace })

	oldKill := processKill
	processKill = func(*exec.Cmd) (bool, error) {
		return false, errors.New("kill failed")
	}
	t.Cleanup(func() { processKill = oldKill })

	cmd := exec.Command("sh", "-c", "trap '' TERM; while :; do sleep 1; done")
	ready, pipeErr := cmd.StdoutPipe()
	require.NoError(t, pipeErr)
	cmd.Args = []string{"sh", "-c", "trap '' TERM; echo ready; while :; do sleep 1; done"}
	require.NoError(t, cmd.Start())
	line, readErr := bufio.NewReader(ready).ReadString('\n')
	require.NoError(t, readErr)
	require.Equal(t, "ready\n", line)
	transport := &ProcessTransport{cmd: cmd}

	err := transport.Close()
	require.Error(t, err)
	require.Contains(t, err.Error(), "kill failed")

	processKill = oldKill
	_, _ = killProcess(cmd)
	_ = transport.wait()
}

func TestProcessTransportCloseWithoutWaitDelay(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses /bin/sh")
	}

	oldWaitDelay := processShutdownWaitDelay
	processShutdownWaitDelay = 0
	t.Cleanup(func() { processShutdownWaitDelay = oldWaitDelay })

	oldGrace := processExitGracePeriod
	processExitGracePeriod = 20 * time.Millisecond
	t.Cleanup(func() { processExitGracePeriod = oldGrace })

	dir := t.TempDir()
	script := writeShellScript(t, filepath.Join(dir, "claude"), `#!/bin/sh
trap 'exit 0' TERM
while :; do sleep 1; done
`)
	transport := NewProcessTransport(nil, platformTestTransportOptions(t, Options{CLIPath: script}))

	require.NoError(t, transport.Start(context.Background()))
	require.NoError(t, transport.Close())
}

func TestProcessTransportWaitForShutdownWithoutWaitDelayReturnsWaitError(t *testing.T) {
	oldWaitDelay := processShutdownWaitDelay
	processShutdownWaitDelay = 0
	t.Cleanup(func() { processShutdownWaitDelay = oldWaitDelay })

	waitErr := errors.New("wait failed")
	transport := &ProcessTransport{}
	waitCh := make(chan error, 1)
	waitCh <- waitErr

	require.ErrorIs(t, transport.waitForShutdown(waitCh, false), waitErr)
}

func TestProcessTransportWaitForShutdownRecordsSuccessfulKill(t *testing.T) {
	oldWaitDelay := processShutdownWaitDelay
	processShutdownWaitDelay = time.Millisecond
	t.Cleanup(func() { processShutdownWaitDelay = oldWaitDelay })

	oldKill := processKill
	processKill = func(*exec.Cmd) (bool, error) { return true, nil }
	t.Cleanup(func() { processKill = oldKill })

	transport := &ProcessTransport{cmd: &exec.Cmd{}}
	err := transport.waitForShutdown(make(chan error), false)
	require.ErrorContains(t, err, processWaitTimedOutMessage)
}

func TestProcessTransportCloseWaitsForVoluntaryExitBeforeTerm(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses /bin/sh")
	}

	terminated := make(chan struct{}, 1)
	oldTerminate := processTerminate
	processTerminate = func(cmd *exec.Cmd) (bool, error) {
		terminated <- struct{}{}

		return terminateProcess(cmd)
	}
	t.Cleanup(func() { processTerminate = oldTerminate })

	dir := t.TempDir()
	script := writeShellScript(t, filepath.Join(dir, "claude"), `#!/bin/sh
cat >/dev/null
exit 0
`)
	transport := NewProcessTransport(nil, platformTestTransportOptions(t, Options{CLIPath: script}))

	require.NoError(t, transport.Start(context.Background()))
	require.NoError(t, transport.Close())
	require.NotNil(t, transport.cmd.ProcessState)

	select {
	case <-terminated:
		t.Fatal("process exiting on stdin EOF should not be signalled")
	default:
	}
}

func TestProcessTransportCloseSkipsExitGraceWithoutStdin(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses /bin/sh")
	}

	cmd := exec.Command("sh", "-c", "sleep 30")
	require.NoError(t, cmd.Start())

	transport := &ProcessTransport{cmd: cmd}

	start := time.Now()
	require.NoError(t, transport.Close())
	require.Less(t, time.Since(start), processExitGracePeriod)
	require.NotNil(t, cmd.ProcessState)
}

func TestProcessTransportShutdownExpiresVoluntaryExitGrace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses sleep and Unix termination")
	}

	originalGrace := processExitGracePeriod
	processExitGracePeriod = time.Millisecond
	t.Cleanup(func() { processExitGracePeriod = originalGrace })

	command := exec.Command("sh", "-c", "sleep 30")
	require.NoError(t, command.Start())
	transport := &ProcessTransport{cmd: command}
	require.NoError(t, transport.shutdownProcess(true))
	require.NotNil(t, command.ProcessState)
}

func TestProcessTransportContainedTreeOwnsShutdown(t *testing.T) {
	originalOwnsShutdown := processContainmentOwnsShutdown
	originalQuiesce := processContainmentQuiesce
	originalClose := processContainmentClose
	t.Cleanup(func() {
		processContainmentOwnsShutdown = originalOwnsShutdown
		processContainmentQuiesce = originalQuiesce
		processContainmentClose = originalClose
	})

	processContainmentOwnsShutdown = func(*processContainment) bool { return true }
	processContainmentQuiesce = func(*processContainment, time.Duration) error { return nil }
	processContainmentClose = func(*processContainment) error { return nil }

	transport := &ProcessTransport{tree: &processContainment{}}
	require.NoError(t, transport.shutdownProcess(false))
	require.Nil(t, transport.tree)
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

func TestProcessTransportStartProbesVersion(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses /bin/sh scripts")
	}

	// Restore the real probe that TestMain disables for the other process tests.
	probe := claudeVersionProbe
	claudeVersionProbe = validateClaudeVersion
	t.Cleanup(func() { claudeVersionProbe = probe })

	dir := t.TempDir()
	acquires := 0
	releases := 0
	withDiscoveryAdmission := func(options Options) Options {
		options.AcquireVersionDiscovery = func(context.Context) (func(), error) {
			acquires++

			return func() { releases++ }, nil
		}

		return platformTestTransportOptions(t, options)
	}

	oldScript := writeShellScript(t, filepath.Join(dir, "old-claude"),
		"#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo '1.0.0 (Claude Code)'; exit 0; fi\ncat >/dev/null\n")
	oldTransport := NewProcessTransport(nil, withDiscoveryAdmission(Options{CLIPath: oldScript, Cwd: dir}))
	require.ErrorContains(t, oldTransport.Start(context.Background()), "too old")
	require.Equal(t, 1, acquires)
	require.Equal(t, 1, releases)

	currentScript := writeShellScript(t, filepath.Join(dir, "current-claude"),
		"#!/bin/sh\nif [ \"$1\" = \"--version\" ]; then echo '2.1.0 (Claude Code)'; exit 0; fi\ncat >/dev/null\n")
	currentTransport := NewProcessTransport(nil, withDiscoveryAdmission(Options{CLIPath: currentScript, Cwd: dir}))
	require.NoError(t, currentTransport.Start(context.Background()))
	require.Equal(t, 2, acquires)
	require.Equal(t, 2, releases)
	require.NoError(t, currentTransport.Close())
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
		CLIPath:          "/bin/sh",
		DarwinBestEffort: true,
		Cwd:              filepath.Join(t.TempDir(), "missing"),
	})

	require.Error(t, transport.Start(context.Background()))
}

func TestProcessTransportStartSetupErrors(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses /bin/sh")
	}

	commandContext := processCommandContext
	prepareContained := processPrepareContained
	startContained := processStartContained
	getwd := processGetwd
	t.Cleanup(func() {
		processCommandContext = commandContext
		processPrepareContained = prepareContained
		processStartContained = startContained
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

	processCommandContext = commandContext
	var inventories []func() (int, bool)
	processPrepareContained = func(*exec.Cmd, processLaunchOptions) (*processTreeCommand, error) {
		return nil, errors.Join(ErrProcessContainmentIncomplete, errors.New("prepare failed"))
	}
	transport = NewProcessTransport(nil, Options{
		CLIPath: "/bin/sh",
		Cwd:     t.TempDir(),
		ObserveProcessInventory: func(_ context.Context, inventory func() (int, bool)) {
			inventories = append(inventories, inventory)
		},
	})
	require.ErrorIs(t, transport.Start(context.Background()), ErrProcessContainmentIncomplete)
	require.Len(t, inventories, 1)
	inventories = nil

	for _, test := range []struct {
		name  string
		shape func(*exec.Cmd)
	}{
		{name: "prepared-stdin", shape: func(cmd *exec.Cmd) { cmd.Stdin = strings.NewReader("") }},
		{name: "prepared-stdout", shape: func(cmd *exec.Cmd) { cmd.Stdout = io.Discard }},
		{name: "prepared-stderr", shape: func(cmd *exec.Cmd) { cmd.Stderr = io.Discard }},
	} {
		t.Run(test.name, func(t *testing.T) {
			processPrepareContained = func(*exec.Cmd, processLaunchOptions) (*processTreeCommand, error) {
				prepared := exec.Command("/bin/sh")
				test.shape(prepared)

				return &processTreeCommand{cmd: prepared}, nil
			}
			preparedTransport := NewProcessTransport(nil, Options{CLIPath: "/bin/sh", Cwd: t.TempDir()})
			require.Error(t, preparedTransport.Start(context.Background()))
		})
	}

	processPrepareContained = prepareContained
	processStartContained = func(*processTreeCommand) (*processContainment, error) {
		return nil, errors.Join(ErrProcessContainmentIncomplete, errors.New("containment cleanup failed"))
	}
	transport = NewProcessTransport(nil, Options{
		CLIPath: "/bin/sh",
		Cwd:     t.TempDir(),
		ObserveProcessInventory: func(_ context.Context, inventory func() (int, bool)) {
			inventories = append(inventories, inventory)
		},
	})
	require.ErrorIs(t, transport.Start(context.Background()), ErrProcessContainmentIncomplete)
	require.Len(t, inventories, 1)
	_, exact := inventories[0]()
	require.False(t, exact)
}

func TestProcessTransportStartDarwinGenerationFailureBranches(t *testing.T) {
	want := errors.New("generation")
	transport := NewProcessTransport(nil, Options{
		CLIPath:          "/bin/sh",
		DarwinBestEffort: true,
		PrepareDarwinGeneration: func(context.Context) (*DarwinGeneration, error) {
			return nil, want
		},
	})
	require.ErrorIs(t, transport.Start(t.Context()), want)

	transport = NewProcessTransport(nil, Options{
		CLIPath:          "/bin/sh",
		Cwd:              filepath.Join(t.TempDir(), "missing"),
		DarwinBestEffort: true,
		PrepareDarwinGeneration: func(context.Context) (*DarwinGeneration, error) {
			return &DarwinGeneration{Release: func(bool) error { return want }}, nil
		},
	})
	require.ErrorIs(t, transport.Start(t.Context()), want)

	originalPrepare := processPrepareContained
	originalStart := processStartContained
	t.Cleanup(func() {
		processPrepareContained = originalPrepare
		processStartContained = originalStart
	})
	processPrepareContained = func(command *exec.Cmd, _ processLaunchOptions) (*processTreeCommand, error) {
		return &processTreeCommand{cmd: command}, nil
	}
	processStartContained = func(*processTreeCommand) (*processContainment, error) {
		return nil, errors.Join(ErrProcessContainmentIncomplete, want)
	}
	var inventories []func() (int, bool)
	transport = NewProcessTransport(nil, Options{
		CLIPath: "/bin/sh",
		ObserveProcessInventory: func(_ context.Context, inventory func() (int, bool)) {
			inventories = append(inventories, inventory)
		},
	})
	require.ErrorIs(t, transport.Start(t.Context()), ErrProcessContainmentIncomplete)
	require.Len(t, inventories, 1)
	_, exact := inventories[0]()
	require.False(t, exact)
}

func TestProcessTransportStart(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	script := filepath.Join(dir, "fake-claude")
	require.NoError(t, os.WriteFile(script, []byte("#!/bin/sh\ncat >/dev/null\n"), 0o755))

	var (
		snapshotCount     int
		snapshotAvailable bool
		snapshotCalls     int
		quiesced          int
	)
	transport := NewProcessTransport(nil, platformTestTransportOptions(t, Options{
		CLIPath: script,
		Cwd:     dir,
		ObserveProcessInventory: func(_ context.Context, inventory func() (int, bool)) {
			snapshotCount, snapshotAvailable = inventory()
			snapshotCalls++
		},
		ObserveProcessQuiesced: func(context.Context) { quiesced++ },
	}))
	require.NoError(t, transport.Start(context.Background()))
	require.Equal(t, 1, snapshotCalls)
	require.Zero(t, snapshotCount)
	require.False(t, snapshotAvailable)
	require.Equal(t, dir, transport.cmd.Dir)
	require.NotNil(t, transport.stdin)
	require.NotNil(t, transport.stdout)
	require.NotNil(t, transport.stderr)
	require.NoError(t, transport.Close())
	require.Equal(t, 1, quiesced)

	transport = NewProcessTransport(nil, platformTestTransportOptions(t, Options{CLIPath: script}))
	require.NoError(t, transport.Start(context.Background()))
	require.NotEmpty(t, transport.cmd.Dir)
	require.NoError(t, transport.Close())
}
