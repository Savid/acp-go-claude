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
	"os/exec"
	"sync"
	"time"
)

var errMessagesAlreadyStarted = errors.New("claude transport messages already started")

const transportErrorBuffer = 8
const defaultMaxJSONLineBytes = 10 * 1024 * 1024
const processWaitTimedOutMessage = "wait for claude process after kill timed out"

var (
	processCommandContext    = exec.CommandContext
	processAfterDecode       = func() {}
	processGetwd             = os.Getwd
	processTerminate         = terminateProcess
	processKill              = killProcess
	maxJSONLineBytes         = defaultMaxJSONLineBytes
	processShutdownWaitDelay = 5 * time.Second
)

// Transport is the JSON-line transport used by the Claude control protocol.
type Transport interface {
	Start(ctx context.Context) error
	Send(ctx context.Context, payload any) error
	Messages(ctx context.Context) (<-chan map[string]any, <-chan error)
	Close() error
}

// ProcessTransport starts and owns a Claude CLI subprocess.
type ProcessTransport struct {
	log     *slog.Logger
	options Options

	cmd    *exec.Cmd
	stdin  io.WriteCloser
	stdout io.ReadCloser
	stderr io.ReadCloser

	mu              sync.Mutex
	closeOnce       sync.Once
	stderrWG        sync.WaitGroup
	closed          bool
	messagesStarted bool
	waitOnce        sync.Once
	waitErr         error
}

var _ Transport = (*ProcessTransport)(nil)

// NewProcessTransport creates a subprocess transport.
func NewProcessTransport(log *slog.Logger, options Options) *ProcessTransport {
	if log == nil {
		log = slog.Default()
	}

	return &ProcessTransport{log: log, options: options}
}

// Start launches the Claude CLI.
func (t *ProcessTransport) Start(ctx context.Context) error {
	path, err := Discover(ctx, t.options.CLIPath, t.options.Env)
	if err != nil {
		return err
	}

	args := BuildArgs(t.options)
	cmd := processCommandContext(ctx, path, args...)
	configureProcessCommand(cmd)

	cmd.Dir = t.options.Cwd
	if cmd.Dir == "" {
		cmd.Dir, err = processGetwd()
		if err != nil {
			return fmt.Errorf("get working directory: %w", err)
		}
	}

	envOptions := t.options
	envOptions.Cwd = cmd.Dir
	cmd.Env = BuildEnv(envOptions)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return fmt.Errorf("create stdin pipe: %w", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return fmt.Errorf("create stdout pipe: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		return fmt.Errorf("create stderr pipe: %w", err)
	}

	if err := cmd.Start(); err != nil {
		return fmt.Errorf("start claude: %w", err)
	}

	t.cmd = cmd
	t.stdin = stdin
	t.stdout = stdout
	t.stderr = stderr

	return nil
}

// Send writes one JSON line to Claude stdin.
func (t *ProcessTransport) Send(ctx context.Context, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("marshal payload: %w", err)
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

	resetDeadline := installWriteDeadline(ctx, t.stdin)
	defer resetDeadline()

	if _, err := t.stdin.Write(append(data, '\n')); err != nil {
		if ctxErr := ctx.Err(); ctxErr != nil {
			return ctxErr
		}

		return fmt.Errorf("write claude stdin: %w", err)
	}

	return nil
}

// Messages reads Claude stdout as line-delimited JSON maps.
func (t *ProcessTransport) Messages(ctx context.Context) (<-chan map[string]any, <-chan error) {
	t.mu.Lock()
	if t.messagesStarted {
		t.mu.Unlock()

		return closedMessageChannels(errMessagesAlreadyStarted)
	}

	if t.closed {
		t.mu.Unlock()

		return closedMessageChannels(ErrClientClosed)
	}

	t.messagesStarted = true

	drainStderr := t.stderr != nil

	if drainStderr {
		t.stderrWG.Add(1)
	}
	t.mu.Unlock()

	messages := make(chan map[string]any)
	errs := make(chan error, transportErrorBuffer)

	if drainStderr {
		go func() {
			defer t.recoverStderrDrain(ctx)
			defer t.stderrWG.Done()

			t.drainStderr()
		}()
	}

	go func() {
		defer close(messages)
		defer close(errs)
		defer func() {
			if err := t.wait(); err != nil {
				t.sendError(errs, fmt.Errorf("%w: %w", ErrProcessExited, err))
			}
		}()
		defer t.recoverStdoutReader(ctx, errs)

		scanner := bufio.NewScanner(t.stdout)
		scanner.Buffer(make([]byte, 0, 64*1024), maxJSONLineBytes)

		for scanner.Scan() {
			select {
			case <-ctx.Done():
				t.sendError(errs, ctx.Err())

				return
			default:
			}

			line := bytes.TrimSpace(scanner.Bytes())
			if len(line) > 0 && line[0] == '{' {
				var msg map[string]any
				if err := json.Unmarshal(line, &msg); err != nil {
					t.sendError(errs, fmt.Errorf("decode claude json line: %w", err))
				} else {
					msg[rawJSONInternalKey] = string(line)

					processAfterDecode()

					select {
					case messages <- msg:
					case <-ctx.Done():
						t.sendError(errs, ctx.Err())

						return
					}
				}
			}
		}

		if err := scanner.Err(); err != nil {
			t.sendError(errs, fmt.Errorf("read claude stdout: %w", err))
		}
	}()

	return messages, errs
}

func closedMessageChannels(err error) (<-chan map[string]any, <-chan error) {
	messages := make(chan map[string]any)
	errs := make(chan error, 1)

	close(messages)
	sendTransportError(errs, err)
	close(errs)

	return messages, errs
}

func (t *ProcessTransport) recoverStderrDrain(ctx context.Context) {
	handleClaudeGoroutinePanic(ctx, t.log, "stderr drain", func(any) { _ = t.Close() }, recover())
}

func (t *ProcessTransport) recoverStdoutReader(ctx context.Context, errs chan<- error) {
	handleClaudeGoroutinePanic(ctx, t.log, "stdout reader", func(recovered any) {
		t.sendError(errs, fmt.Errorf("claude stdout reader panic: %v", recovered))
		_ = t.Close()
	}, recover())
}

func (t *ProcessTransport) wait() error {
	t.waitOnce.Do(func() {
		if t.cmd != nil {
			t.waitErr = t.cmd.Wait()
		}
	})

	return t.waitErr
}

func (t *ProcessTransport) sendError(errs chan<- error, err error) {
	if sendTransportError(errs, err) {
		return
	}

	log := t.log
	if log == nil {
		log = slog.Default()
	}

	log.Debug("dropped claude transport error", slog.String(keyError, err.Error()))
}

func sendTransportError(errs chan<- error, err error) bool {
	if err == nil {
		return true
	}

	select {
	case errs <- err:
		return true
	default:
		return false
	}
}

type writeDeadliner interface {
	SetWriteDeadline(time.Time) error
}

func installWriteDeadline(ctx context.Context, writer io.Writer) func() {
	deadliner, ok := writer.(writeDeadliner)
	if !ok {
		return func() {}
	}

	if deadline, ok := ctx.Deadline(); ok {
		_ = deadliner.SetWriteDeadline(deadline)
	}

	stop := context.AfterFunc(ctx, func() {
		_ = deadliner.SetWriteDeadline(time.Now())
	})

	return func() {
		stop()

		_ = deadliner.SetWriteDeadline(time.Time{})
	}
}

func (t *ProcessTransport) drainStderr() {
	if t.stderr == nil {
		return
	}

	reader := bufio.NewReader(t.stderr)
	for {
		line, err := reader.ReadString('\n')
		if line != "" {
			t.log.Debug("claude stderr", slog.String("line", string(bytes.TrimRight([]byte(line), "\r\n"))))
		}

		if err != nil {
			return
		}
	}
}

// Close terminates the Claude process.
func (t *ProcessTransport) Close() error {
	var closeErr error

	t.closeOnce.Do(func() {
		t.mu.Lock()
		t.closed = true

		if t.stdin != nil {
			closeErr = t.stdin.Close()
		}

		if t.stderr != nil {
			_ = t.stderr.Close()
		}
		t.mu.Unlock()

		if t.cmd != nil && t.cmd.Process != nil {
			shutdown := false

			if terminated, err := processTerminate(t.cmd); err != nil {
				closeErr = errors.Join(closeErr, err)
			} else {
				shutdown = terminated
			}

			if err := t.waitForShutdown(shutdown); err != nil {
				closeErr = errors.Join(closeErr, err)
			}
		}

		t.stderrWG.Wait()
	})

	return closeErr
}

func configureProcessCommand(cmd *exec.Cmd) {
	cmd.WaitDelay = processShutdownWaitDelay
	configureProcessCommandPlatform(cmd)
}

func (t *ProcessTransport) waitForShutdown(shutdown bool) error {
	if processShutdownWaitDelay <= 0 {
		err := t.wait()
		if expectedShutdownProcessExit(err, shutdown) {
			return nil
		}

		return err
	}

	waitErr := make(chan error, 1)
	go func() { waitErr <- t.wait() }()

	timer := time.NewTimer(processShutdownWaitDelay)
	defer timer.Stop()

	select {
	case err := <-waitErr:
		if expectedShutdownProcessExit(err, shutdown) {
			return nil
		}

		return err
	case <-timer.C:
		closeErr := t.closeStdout()
		if killed, err := processKill(t.cmd); err != nil {
			closeErr = errors.Join(closeErr, err)
		} else {
			shutdown = shutdown || killed
		}

		return waitForProcessExit(waitErr, shutdown, closeErr, processShutdownWaitDelay)
	}
}

func waitForProcessExit(waitErr <-chan error, shutdown bool, closeErr error, timeout time.Duration) error {
	select {
	case err := <-waitErr:
		if expectedShutdownProcessExit(err, shutdown) {
			return closeErr
		}

		return errors.Join(closeErr, err)
	case <-time.After(timeout):
		return errors.Join(closeErr, errors.New(processWaitTimedOutMessage))
	}
}

func (t *ProcessTransport) closeStdout() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if t.stdout == nil {
		return nil
	}

	return t.stdout.Close()
}

func expectedShutdownProcessExit(err error, shutdown bool) bool {
	if err == nil {
		return true
	}

	if !shutdown {
		return false
	}

	if errors.Is(err, context.Canceled) || errors.Is(err, exec.ErrWaitDelay) {
		return true
	}

	var exitErr *exec.ExitError

	return errors.As(err, &exitErr)
}
