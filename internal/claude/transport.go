package claude

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"sync"
	"sync/atomic"
	"time"
)

var errEventsAlreadyStarted = errors.New("claude transport events already started")

const defaultMaxJSONLineBytes = 10 * 1024 * 1024

var (
	maxJSONLineBytes         = defaultMaxJSONLineBytes
	processExitGracePeriod   = 2 * time.Second
	processShutdownWaitDelay = 5 * time.Second
	claudeVersionProbe       = validateClaudeVersion
)

type Transport interface {
	Start(context.Context) error
	Send(context.Context, any) error
	Events(context.Context) <-chan TransportEvent
	Close() error
}

type TransportEvent struct {
	Message map[string]any
	Err     error
}

type ProcessTransport struct {
	log     *slog.Logger
	options Options
	process NativeProcess
	stdin   io.WriteCloser
	stdout  io.ReadCloser
	stderr  io.ReadCloser

	mu             sync.Mutex
	closeOnce      sync.Once
	closeErr       error
	stdinOnce      sync.Once
	stdinErr       error
	eventsStarted  bool
	closed         bool
	waitOnce       sync.Once
	waitDone       chan struct{}
	waitResult     NativeResult
	waitErr        error
	stderrDone     chan struct{}
	stderrErr      error
	eventsDone     chan struct{}
	malformedLines atomic.Uint64
}

var _ Transport = (*ProcessTransport)(nil)

func NewProcessTransport(log *slog.Logger, options Options) *ProcessTransport {
	if log == nil {
		log = slog.Default()
	}

	return &ProcessTransport{log: log, options: options}
}

func (t *ProcessTransport) Start(ctx context.Context) error {
	if err := claudeVersionProbe(ctx, t.options); err != nil {
		return err
	}

	process, err := startNative(ctx, t.options, t.options.CLIPath, BuildArgs(t.options))
	if err != nil {
		return fmt.Errorf("start claude: %w", err)
	}

	t.process = process
	t.stdin = process.Stdin()
	t.stdout = process.Stdout()
	t.stderr = process.Stderr()
	t.mu.Lock()
	t.waitDone = make(chan struct{})
	t.mu.Unlock()

	return nil
}

func (t *ProcessTransport) Send(ctx context.Context, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return errClaudePayloadMarshal
	}

	if len(data)+1 > maxJSONLineBytes {
		return fmt.Errorf("claude stdin json line exceeds %d bytes", maxJSONLineBytes)
	}

	t.mu.Lock()
	defer t.mu.Unlock()

	if t.stdin == nil {
		return errors.New("claude transport is not started")
	}

	select {
	case <-ctx.Done():
		return ctx.Err()
	default:
	}

	reset := installWriteDeadline(ctx, t.stdin)
	defer reset()

	if _, err := t.stdin.Write(append(data, '\n')); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}

		return errClaudeStdinWrite
	}

	return nil
}

func (t *ProcessTransport) Events(ctx context.Context) <-chan TransportEvent {
	t.mu.Lock()
	if t.eventsStarted {
		t.mu.Unlock()

		return closedTransportEvents(errEventsAlreadyStarted)
	}

	if t.closed {
		t.mu.Unlock()

		return closedTransportEvents(ErrClientClosed)
	}

	t.eventsStarted = true
	t.eventsDone = make(chan struct{})

	if t.stderr != nil {
		t.stderrDone = make(chan struct{})

		stderrDone := t.stderrDone
		go func() {
			defer close(stderrDone)
			defer func() {
				if recovered := recover(); recovered != nil {
					t.mu.Lock()
					t.stderrErr = errClaudeTransportFailure
					t.mu.Unlock()
					handleClaudeGoroutinePanic(ctx, t.log, "stderr drain", nil, recovered)
				}
			}()

			t.drainStderr()
		}()
	}

	eventsDone := t.eventsDone
	t.mu.Unlock()

	events := make(chan TransportEvent, 1)
	// #nosec G118 -- terminal Wait deliberately outlives event-delivery cancellation.
	go func() {
		defer close(events)
		defer close(eventsDone)

		terminal, deliver := t.scanEvents(ctx, events)

		result, waitErr := t.wait(context.Background())
		if waitErr == nil && result.ExitCode != 0 {
			waitErr = &ProcessExitError{ExitCode: result.ExitCode}
		}

		t.mu.Lock()
		stderrDone := t.stderrDone
		stderr := t.stderr
		t.mu.Unlock()

		if stderrDone != nil {
			select {
			case <-stderrDone:
			case <-time.After(nativeOutputDrainDelay):
				terminal = errors.Join(terminal, closeNativeStream(stderr))

				select {
				case <-stderrDone:
				case <-time.After(nativeOutputDrainDelay):
					terminal = errors.Join(terminal, errClaudeTransportFailure)
				}
			}
		}

		t.mu.Lock()
		stderrErr := t.stderrErr
		t.mu.Unlock()

		terminal = errors.Join(terminal, waitErr, stderrErr)

		if ctx.Err() != nil {
			deliver = false
		}

		if terminal != nil && deliver {
			select {
			case events <- TransportEvent{Err: terminal}:
			case <-ctx.Done():
			}
		}
	}()

	return events
}

func (t *ProcessTransport) scanEvents(ctx context.Context, events chan<- TransportEvent) (terminal error, deliver bool) {
	deliver = true

	defer func() {
		if recovered := recover(); recovered != nil {
			terminal = errors.Join(terminal, errClaudeStdoutReaderPanic)

			handleClaudeGoroutinePanic(ctx, t.log, "stdout reader", nil, recovered)
		}
	}()

	scanner := bufio.NewScanner(t.stdout)

	initialBuffer := 64 * 1024
	if maxJSONLineBytes < initialBuffer {
		initialBuffer = maxJSONLineBytes
	}

	scanner.Buffer(make([]byte, 0, initialBuffer), maxJSONLineBytes)

	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}

		var message map[string]any
		if err := json.Unmarshal(line, &message); err != nil {
			count := t.malformedLines.Add(1)
			if t.log != nil {
				t.log.DebugContext(ctx, "skip malformed claude json line",
					slog.Int("bytes", len(line)), slog.Uint64("malformed_lines", count))
			}

			continue
		}

		message[rawJSONInternalKey] = string(line)

		if deliver {
			select {
			case events <- TransportEvent{Message: message}:
			case <-ctx.Done():
				deliver = false
			}
		}
	}

	if scanner.Err() != nil {
		terminal = errClaudeStdoutRead
	}

	return terminal, deliver
}

func closedTransportEvents(err error) <-chan TransportEvent {
	events := make(chan TransportEvent, 1)
	events <- TransportEvent{Err: err}

	close(events)

	return events
}

func (t *ProcessTransport) wait(ctx context.Context) (NativeResult, error) {
	if t.process == nil {
		return NativeResult{}, nil
	}

	t.mu.Lock()
	if t.waitDone == nil {
		t.waitDone = make(chan struct{})
	}

	done := t.waitDone
	t.mu.Unlock()

	t.waitOnce.Do(func() {
		go func() {
			t.waitResult, t.waitErr = t.process.Wait(context.Background())

			close(done)
		}()
	})

	select {
	case <-done:
		return t.waitResult, t.waitErr
	case <-ctx.Done():
		return NativeResult{}, ctx.Err()
	}
}

func (t *ProcessTransport) closeStdin() error {
	t.stdinOnce.Do(func() {
		if t.stdin != nil {
			t.stdinErr = t.stdin.Close()
			if errors.Is(t.stdinErr, os.ErrClosed) {
				t.stdinErr = nil
			}
		}
	})

	return t.stdinErr
}

func (t *ProcessTransport) Close() error {
	t.closeOnce.Do(func() {
		t.mu.Lock()
		t.closed = true
		t.mu.Unlock()

		t.closeErr = t.closeStdin()
		if t.process != nil {
			grace, cancelGrace := context.WithTimeout(context.Background(), processExitGracePeriod)
			_, waitErr := t.wait(grace)

			cancelGrace()

			if errors.Is(waitErr, context.DeadlineExceeded) || errors.Is(waitErr, context.Canceled) {
				revokeCtx, cancelRevoke := context.WithTimeout(context.Background(), processShutdownWaitDelay)
				revokeErr := t.process.Revoke(revokeCtx)

				cancelRevoke()

				terminalCtx, cancelTerminal := context.WithTimeout(context.Background(), processShutdownWaitDelay)
				_, waitErr = t.wait(terminalCtx)

				cancelTerminal()

				if errors.Is(waitErr, context.DeadlineExceeded) || errors.Is(waitErr, context.Canceled) {
					waitErr = containmentIncomplete(t.options.Authority, "wait for revoked Claude process", waitErr)
				}

				t.closeErr = errors.Join(t.closeErr, revokeErr)
			}

			if waitErr != nil && t.options.Authority != nil &&
				!errors.Is(waitErr, t.options.Authority.ContainmentIncomplete) {
				waitErr = containmentIncomplete(t.options.Authority, "wait for Claude process", waitErr)
			}

			t.closeErr = errors.Join(t.closeErr, waitErr)
		}

		t.mu.Lock()
		eventsDone := t.eventsDone
		stdout := t.stdout
		stderr := t.stderr
		stderrDone := t.stderrDone
		t.mu.Unlock()

		if eventsDone != nil {
			select {
			case <-eventsDone:
			case <-time.After(nativeOutputDrainDelay):
				if stdout != nil {
					t.closeErr = errors.Join(t.closeErr, closeNativeStream(stdout))
				}
			}
		}

		if stderr != nil {
			t.closeErr = errors.Join(t.closeErr, closeNativeStream(stderr))
		}

		if stderrDone != nil {
			select {
			case <-stderrDone:
			case <-time.After(nativeOutputDrainDelay):
				t.closeErr = errors.Join(t.closeErr, errClaudeTransportFailure)
			}
		}
	})

	return t.closeErr
}

func closeNativeStream(stream io.Closer) error {
	err := stream.Close()
	if errors.Is(err, os.ErrClosed) {
		return nil
	}

	return err
}

type writeDeadliner interface{ SetWriteDeadline(time.Time) error }

func installWriteDeadline(ctx context.Context, writer io.Writer) func() {
	deadliner, ok := writer.(writeDeadliner)
	if !ok {
		return func() {}
	}

	if deadline, ok := ctx.Deadline(); ok {
		_ = deadliner.SetWriteDeadline(deadline)
	}

	stop := context.AfterFunc(ctx, func() { _ = deadliner.SetWriteDeadline(time.Now()) })

	return func() { stop(); _ = deadliner.SetWriteDeadline(time.Time{}) }
}

func (t *ProcessTransport) drainStderr() {
	if t.stderr != nil {
		_, _ = io.Copy(io.Discard, t.stderr)
	}
}
