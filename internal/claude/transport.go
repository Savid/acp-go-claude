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
	stdinOnce      sync.Once
	stdinErr       error
	eventsStarted  bool
	closed         bool
	waitFlight     *nativeWaitFlight
	eventsCancel   context.CancelFunc
	stderrDone     chan struct{}
	stderrErr      error
	eventsDone     chan struct{}
	malformedLines atomic.Uint64

	closeMu        sync.Mutex
	closeFlight    *transportCloseFlight
	closeSettled   bool
	processSettled bool
	revokeSettled  bool
	outputsSettled bool
	outputErr      error
}

type transportCloseFlight struct {
	done chan struct{}
	err  error
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
	t.waitFlight = t.newWaitFlight()
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
	eventsCtx, cancelEvents := context.WithCancel(ctx)
	t.eventsCancel = cancelEvents
	waitFlight := t.waitFlightLocked()

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
					handleClaudeGoroutinePanic(eventsCtx, t.log, "stderr drain", nil, recovered)
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
		defer cancelEvents()

		terminal, deliver := t.scanEvents(eventsCtx, events)

		var result NativeResult

		var waitErr error
		if waitFlight != nil {
			result, waitErr = waitFlight.wait(context.Background())
		}

		if waitErr == nil && (result.Revoked || result.Signal != 0 || result.ExitCode != 0) {
			waitErr = &ProcessExitError{ExitCode: result.ExitCode, Signal: result.Signal, Revoked: result.Revoked}
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

		if eventsCtx.Err() != nil {
			deliver = false
		}

		if terminal != nil && deliver {
			select {
			case events <- TransportEvent{Err: terminal}:
			case <-eventsCtx.Done():
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
	t.mu.Lock()
	flight := t.waitFlightLocked()
	t.mu.Unlock()

	if flight == nil {
		return NativeResult{}, nil
	}

	return flight.wait(ctx)
}

func (t *ProcessTransport) waitFlightLocked() *nativeWaitFlight {
	if t.process != nil && t.waitFlight == nil {
		t.waitFlight = t.newWaitFlight()
	}

	return t.waitFlight
}

func (t *ProcessTransport) newWaitFlight() *nativeWaitFlight {
	return newNativeWaitFlight(t.process, containmentIncomplete(t.options, "wait for Claude process", errNativeProcessWaitPanic))
}

func (t *ProcessTransport) cancelWaitFlight(cause error) (NativeResult, error) {
	t.mu.Lock()
	flight := t.waitFlight
	t.mu.Unlock()

	if flight == nil {
		return NativeResult{}, nil
	}

	return t.cancelExactWaitFlight(flight, cause)
}

func (t *ProcessTransport) cancelExactWaitFlight(flight *nativeWaitFlight, cause error) (NativeResult, error) {
	if flight == nil {
		return NativeResult{}, nil
	}

	result, err := flight.cancelAndJoin(cause)

	t.mu.Lock()
	if t.waitFlight == flight {
		t.waitFlight = nil
	}
	t.mu.Unlock()

	return result, err
}

func (t *ProcessTransport) clearExactWaitFlight(flight *nativeWaitFlight) {
	t.mu.Lock()
	if t.waitFlight == flight {
		t.waitFlight = nil
	}
	t.mu.Unlock()
}

func (t *ProcessTransport) closeWait(ctx context.Context) (*nativeWaitFlight, NativeResult, error) {
	t.mu.Lock()
	flight := t.waitFlightLocked()
	t.mu.Unlock()

	if flight == nil {
		return nil, NativeResult{}, nil
	}

	result, err := flight.wait(ctx)

	return flight, result, err
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
	t.closeMu.Lock()
	if t.closeSettled {
		t.closeMu.Unlock()

		return nil
	}

	if flight := t.closeFlight; flight != nil {
		t.closeMu.Unlock()
		<-flight.done

		return flight.err
	}

	flight := &transportCloseFlight{done: make(chan struct{})}
	t.closeFlight = flight
	t.closeMu.Unlock()

	flight.err = t.closeAttempt()

	t.closeMu.Lock()
	if flight.err == nil {
		t.closeSettled = true
	}

	if t.closeFlight == flight {
		t.closeFlight = nil
	}

	close(flight.done)
	t.closeMu.Unlock()

	return flight.err
}

func (t *ProcessTransport) closeAttempt() error {
	t.mu.Lock()
	t.closed = true
	t.mu.Unlock()

	closeErr := t.closeStdin()
	if t.process != nil && !t.processSettled {
		grace, cancelGrace := context.WithTimeout(context.Background(), processExitGracePeriod)
		flight, _, waitErr := t.closeWait(grace)

		cancelGrace()

		if waitErr != nil && (t.options.Authority != nil || errors.Is(waitErr, context.DeadlineExceeded) || errors.Is(waitErr, context.Canceled)) {
			initialWaitErr := waitErr
			terminalWon := false

			if errors.Is(waitErr, context.DeadlineExceeded) || errors.Is(waitErr, context.Canceled) {
				incomplete := containmentIncomplete(t.options, "wait for Claude process before revoke", waitErr)
				_, joinedErr := t.cancelExactWaitFlight(flight, incomplete)
				terminalWon = joinedErr == nil
				initialWaitErr = independentWaitError(joinedErr, incomplete)
			} else {
				t.clearExactWaitFlight(flight)
			}

			if terminalWon {
				waitErr = nil
			} else {
				var revokeErr error
				if !t.revokeSettled {
					revokeErr = boundedNativeRevoke(t.process, processShutdownWaitDelay)
					if revokeErr == nil {
						t.revokeSettled = true
					}
				}

				terminalCtx, cancelTerminal := context.WithTimeout(context.Background(), processShutdownWaitDelay)
				terminalFlight, _, terminalErr := t.closeWait(terminalCtx)

				cancelTerminal()

				if errors.Is(terminalErr, context.DeadlineExceeded) || errors.Is(terminalErr, context.Canceled) {
					incomplete := containmentIncomplete(t.options, "wait for revoked Claude process", terminalErr)
					_, terminalErr = t.cancelExactWaitFlight(terminalFlight, incomplete)
				} else if terminalErr != nil {
					t.clearExactWaitFlight(terminalFlight)
				}

				waitErr = terminalErr
				closeErr = errors.Join(closeErr, initialWaitErr, revokeErr)
			}
		}

		if waitErr == nil {
			t.processSettled = true
		} else {
			t.clearExactWaitFlight(flight)

			if t.options.Authority != nil && !errors.Is(waitErr, t.options.Authority.ContainmentIncomplete) {
				waitErr = containmentIncomplete(t.options, "wait for Claude process", waitErr)
			}
		}

		closeErr = errors.Join(closeErr, waitErr)
	}

	return errors.Join(closeErr, t.settleOutputWorkers())
}

func independentWaitError(err error, ownedCancelCause error) error {
	if err == nil || err == ownedCancelCause {
		return nil
	}

	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		var independent error
		for _, cause := range joined.Unwrap() {
			independent = errors.Join(independent, independentWaitError(cause, ownedCancelCause))
		}

		return independent
	}

	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		return independentWaitError(wrapped.Unwrap(), ownedCancelCause)
	}

	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return nil
	}

	return err
}

func (t *ProcessTransport) settleOutputWorkers() error {
	if t.outputsSettled {
		return t.outputErr
	}

	t.mu.Lock()
	eventsDone := t.eventsDone
	eventsCancel := t.eventsCancel
	stdout := t.stdout
	stderr := t.stderr
	stderrDone := t.stderrDone
	t.mu.Unlock()

	eventsJoined := eventsDone == nil
	if eventsDone != nil {
		select {
		case <-eventsDone:
			eventsJoined = true
		case <-time.After(nativeOutputDrainDelay):
		}
	}

	if !eventsJoined && eventsCancel != nil {
		eventsCancel()
	}

	if stdout != nil {
		t.outputErr = errors.Join(t.outputErr, closeNativeStream(stdout))
	}

	if stderr != nil {
		t.outputErr = errors.Join(t.outputErr, closeNativeStream(stderr))
	}

	if eventsDone != nil && !eventsJoined {
		<-eventsDone
	}

	if stderrDone != nil {
		<-stderrDone
	}

	t.mu.Lock()
	t.outputErr = errors.Join(t.outputErr, t.stderrErr)
	t.mu.Unlock()
	t.outputsSettled = true

	return t.outputErr
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
