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
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

var errMessagesAlreadyStarted = errors.New("claude transport messages already started")

// ErrProcessContainmentIncomplete means the selected native containment
// boundary did not complete.
var ErrProcessContainmentIncomplete = errors.New("claude process containment incomplete")

const transportErrorBuffer = 8
const defaultMaxJSONLineBytes = 10 * 1024 * 1024
const processWaitTimedOutMessage = "wait for claude process after kill timed out"

// stderrTailLines bounds the ring of recent stderr lines kept so a process exit
// error can carry the real cause.
const stderrTailLines = 20

var (
	processCommand                 = newProcessCommand
	processPrepareContained        = prepareProcessTreeCommand
	processStartContained          = startContainedProcess
	processWaitContained           = waitContainedProcess
	processAfterDecode             = func() {}
	processGetwd                   = os.Getwd
	processTerminate               = terminateProcess
	processKill                    = killProcess
	processContainmentOwnsShutdown = func(tree *processContainment) bool { return tree.ownsShutdown() }
	processContainmentQuiesce      = func(tree *processContainment, timeout time.Duration) error { return tree.quiesce(timeout) }
	processContainmentClose        = func(tree *processContainment) error { return tree.close() }
	maxJSONLineBytes               = defaultMaxJSONLineBytes
	processShutdownWaitDelay       = 5 * time.Second
	processExitGracePeriod         = 2 * time.Second
)

func waitContainedProcess(tree *processContainment, cmd *exec.Cmd) error { return tree.wait(cmd) }

// claudeVersionProbe fails fast when the discovered Claude CLI is too old. It is
// a variable so tests can substitute the probe.
var claudeVersionProbe = validateClaudeVersion

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

	cmd      *exec.Cmd
	tree     *processContainment
	waitTree *processContainment
	stdin    io.WriteCloser
	stdout   io.ReadCloser
	stderr   io.ReadCloser

	mu              sync.Mutex
	closeOnce       sync.Once
	closeErr        error
	stdinOnce       sync.Once
	stdinErr        error
	stderrWG        sync.WaitGroup
	closed          bool
	messagesStarted bool
	waitOnce        sync.Once
	waitErr         error

	stderrMu       sync.Mutex
	stderrTail     []string
	malformedLines atomic.Uint64
}

// appendStderr records one stderr line into the bounded tail ring.
func (t *ProcessTransport) appendStderr(line string) {
	t.stderrMu.Lock()
	defer t.stderrMu.Unlock()

	t.stderrTail = append(t.stderrTail, line)
	if len(t.stderrTail) > stderrTailLines {
		t.stderrTail = t.stderrTail[len(t.stderrTail)-stderrTailLines:]
	}
}

// StderrTail returns the most recent stderr lines joined by newlines.
func (t *ProcessTransport) StderrTail() string {
	t.stderrMu.Lock()
	defer t.stderrMu.Unlock()

	return strings.Join(t.stderrTail, "\n")
}

// processExitError builds a ProcessExitError enriched with the process exit
// status and the captured stderr tail.
func (t *ProcessTransport) processExitError(err error) error {
	exit := &ProcessExitError{ExitCode: -1, StderrTail: t.StderrTail(), Err: err}

	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		exit.ExitCode = exitErr.ExitCode()
	}

	return exit
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
func (t *ProcessTransport) Start(ctx context.Context) (returnErr error) {
	path, err := Discover(ctx, t.options.CLIPath, t.options)
	if err != nil {
		return err
	}

	if probeErr := claudeVersionProbe(ctx, path, t.options); probeErr != nil {
		return probeErr
	}

	generation := t.options.Generation
	if generation == nil && t.options.DarwinBestEffort && t.options.PrepareDarwinGeneration != nil {
		generation, err = t.options.PrepareDarwinGeneration(ctx)
		if err != nil {
			return err
		}
	}

	generationOwnedByTree := false
	defer func() {
		if generation != nil && !generationOwnedByTree {
			returnErr = errors.Join(returnErr, generation.finish(true))
		}
	}()

	args := BuildArgs(t.options)
	cmd := processCommand(path, args...)
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
	if cmd.Env == nil {
		return errors.New("build Claude process environment: invalid process isolation")
	}

	if cmd.Stdin != nil {
		return errors.New("create stdin pipe: exec: Stdin already set")
	}

	if cmd.Stdout != nil {
		return errors.New("create stdout pipe: exec: Stdout already set")
	}

	if cmd.Stderr != nil {
		return errors.New("create stderr pipe: exec: Stderr already set")
	}

	launch, err := processPrepareContained(cmd, processLaunchOptions{
		DarwinBestEffort: t.options.DarwinBestEffort,
		Generation:       generation,
		Isolation:        t.options.ProcessIsolation,
	})
	if err != nil {
		if errors.Is(err, ErrProcessContainmentIncomplete) && t.options.ObserveProcessInventory != nil {
			t.options.ObserveProcessInventory(ctx, unavailableProcessInventory)
		}

		return fmt.Errorf("prepare claude containment: %w", err)
	}

	cmd = launch.cmd

	stdin, err := cmd.StdinPipe()
	if err != nil {
		launch.close()

		return fmt.Errorf("create stdin pipe: %w", err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()

		launch.close()

		return fmt.Errorf("create stdout pipe: %w", err)
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()

		launch.close()

		return fmt.Errorf("create stderr pipe: %w", err)
	}

	tree, err := processStartContained(launch)
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()
		_ = stderr.Close()

		if errors.Is(err, ErrProcessContainmentIncomplete) && t.options.ObserveProcessInventory != nil {
			t.options.ObserveProcessInventory(ctx, unavailableProcessInventory)
		}

		return fmt.Errorf("start claude: %w", err)
	}

	t.cmd = cmd
	t.tree = tree
	t.waitTree = tree
	generationOwnedByTree = true
	t.stdin = stdin
	t.stdout = stdout
	t.stderr = stderr

	if t.options.ObserveProcessInventory != nil {
		t.options.ObserveProcessInventory(ctx, tree.processSnapshot)
	}

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
				t.sendError(errs, t.processExitError(err))
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
					// A single malformed line from a live process must not tear
					// the session down: skip it, count it, and keep scanning.
					count := t.malformedLines.Add(1)
					if t.log != nil {
						t.log.DebugContext(ctx, "skip malformed claude json line",
							slog.Int("bytes", len(line)),
							slog.Uint64("malformed_lines", count),
						)
					}
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
			// os/exec closes the parent ends of its own pipes inside Wait, so the
			// transport must have already given up its stdin handle by then;
			// otherwise Close would report a second close of the same descriptor
			// as a shutdown failure.
			_ = t.closeStdin()

			if t.waitTree != nil {
				t.waitErr = t.waitTree.wait(t.cmd)
			} else {
				t.waitErr = t.cmd.Wait()
			}
		}
	})

	return t.waitErr
}

// closeStdin closes the Claude stdin pipe exactly once and memoizes the result.
// Both Close and wait reach it, and either one may run first.
func (t *ProcessTransport) closeStdin() error {
	t.stdinOnce.Do(func() {
		t.mu.Lock()
		defer t.mu.Unlock()

		if t.stdin == nil {
			return
		}

		t.stdinErr = t.stdin.Close()
	})

	return t.stdinErr
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
		if trimmed := strings.TrimRight(line, "\r\n"); trimmed != "" {
			t.appendStderr(trimmed)
			t.log.Debug("claude stderr", slog.String("line", trimmed))
		}

		if err != nil {
			return
		}
	}
}

// Close terminates the Claude process.
func (t *ProcessTransport) Close() error {
	t.closeOnce.Do(func() {
		t.mu.Lock()
		t.closed = true
		stdinClosed := t.stdin != nil
		t.mu.Unlock()

		t.closeErr = t.closeStdin()

		t.mu.Lock()
		if t.stderr != nil {
			_ = t.stderr.Close()
		}
		t.mu.Unlock()

		if t.cmd != nil && t.cmd.Process != nil {
			if err := t.shutdownProcess(stdinClosed); err != nil {
				t.closeErr = errors.Join(t.closeErr, err)
			}
		}

		t.stderrWG.Wait()
	})

	return t.closeErr
}

func configureProcessCommand(cmd *exec.Cmd) {
	cmd.WaitDelay = processShutdownWaitDelay
	configureProcessCommandPlatform(cmd)
}

// shutdownProcess escalates process shutdown: stdin EOF → SIGTERM → SIGKILL.
// The initial grace window lets the CLI exit on its own after stdin closes so
// in-flight cleanup (e.g. MCP session termination) completes instead of being
// cut short by a signal. The window is skipped when there was no stdin to
// close, since the process then has no EOF cue to exit on.
func (t *ProcessTransport) shutdownProcess(stdinClosed bool) error {
	waitErr := make(chan error, 1)
	go func() { waitErr <- t.wait() }()

	if stdinClosed && processExitGracePeriod > 0 {
		timer := time.NewTimer(processExitGracePeriod)
		defer timer.Stop()

		select {
		case err := <-waitErr:
			if expectedShutdownProcessExit(err, true) {
				err = nil
			}

			return errors.Join(err, t.quiesceProcessTree())
		case <-timer.C:
		}
	}

	if t.tree != nil && processContainmentOwnsShutdown(t.tree) {
		containmentErr := t.quiesceProcessTree()

		return errors.Join(containmentErr, t.waitForShutdown(waitErr, true))
	}

	shutdown := false

	var closeErr error

	if terminated, err := processTerminate(t.cmd); err != nil {
		closeErr = errors.Join(closeErr, err)
	} else {
		shutdown = terminated
	}

	if err := t.waitForShutdown(waitErr, shutdown); err != nil {
		closeErr = errors.Join(closeErr, err)
	}

	return errors.Join(closeErr, t.quiesceProcessTree())
}

func (t *ProcessTransport) quiesceProcessTree() error {
	if t.tree == nil {
		// ProcessTransport.Start always installs containment. A nil tree is only
		// possible for package-internal tests that construct a transport around
		// an already-started command.
		return nil
	}

	if err := processContainmentQuiesce(t.tree, processShutdownWaitDelay); err != nil {
		if t.options.ObserveProcessInventory != nil {
			t.options.ObserveProcessInventory(context.Background(), unavailableProcessInventory)
		}

		return fmt.Errorf("%w: %v", ErrProcessContainmentIncomplete, err)
	}

	if t.options.ObserveProcessQuiesced != nil {
		t.options.ObserveProcessQuiesced(context.Background())
	}

	if err := processContainmentClose(t.tree); err != nil {
		return fmt.Errorf("close Claude process containment: %w", err)
	}

	t.tree = nil

	return nil
}

func unavailableProcessInventory() (int, bool) { return 0, false }

func (t *ProcessTransport) waitForShutdown(waitErr <-chan error, shutdown bool) error {
	if processShutdownWaitDelay <= 0 {
		err := <-waitErr
		if expectedShutdownProcessExit(err, shutdown) {
			return nil
		}

		return err
	}

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
		if t.tree != nil {
			closeErr = errors.Join(closeErr, t.quiesceProcessTree())
			shutdown = true
		} else if killed, err := processKill(t.cmd); err != nil {
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
