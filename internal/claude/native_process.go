package claude

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"reflect"
	"time"
)

var (
	errNativeProcessWaitPanic   = errors.New("native process wait panicked")
	errNativeProcessRevokePanic = errors.New("native process revoke panicked")
)

// nativeWaitFlight owns one cancellable observation of a host-owned process.
// The context stays local to its goroutine; completion publishes result and err
// before closing done, so every waiter reads them without another lock.
type nativeWaitFlight struct {
	done   chan struct{}
	cancel context.CancelCauseFunc
	result NativeResult
	err    error
}

func newNativeWaitFlight(process NativeProcess, panicErr error) *nativeWaitFlight {
	ctx, cancel := context.WithCancelCause(context.Background())
	flight := &nativeWaitFlight{done: make(chan struct{}), cancel: cancel}

	go func() {
		defer close(flight.done)

		flight.result, flight.err = callNativeWait(ctx, process)
		if errors.Is(flight.err, errNativeProcessWaitPanic) && panicErr != nil {
			flight.err = panicErr
		}

		if flight.err != nil && ctx.Err() != nil && errors.Is(flight.err, ctx.Err()) {
			if cause := context.Cause(ctx); cause != nil {
				flight.err = errors.Join(cause, flight.err)
			}
		}
	}()

	return flight
}

func (f *nativeWaitFlight) wait(ctx context.Context) (NativeResult, error) {
	select {
	case <-f.done:
		return f.result, f.err
	case <-ctx.Done():
		return NativeResult{}, ctx.Err()
	}
}

func (f *nativeWaitFlight) cancelAndJoin(cause error) (NativeResult, error) {
	f.cancel(cause)
	<-f.done

	return f.result, f.err
}

// nativeGetwd names where a launch with no explicit working directory starts.
// It is a seam because a process cannot portably be made to lose its own
// working directory: POSIX lets the directory be unlinked underneath it, darwin
// still answers the unlinked path from its name cache, and Windows refuses the
// unlink outright.
var nativeGetwd = os.Getwd

func startNative(ctx context.Context, options Options, executable string, arguments []string) (NativeProcess, error) {
	if executable == "" {
		executable = defaultCLIExecutable
	}

	cwd := options.Cwd
	if cwd == "" {
		var err error

		cwd, err = nativeGetwd()
		if err != nil {
			return nil, fmt.Errorf("get native working directory: %w", err)
		}
	}

	envOptions := options
	envOptions.Cwd = cwd

	environment := append([]string(nil), options.PreparedEnvironment...)
	if environment == nil {
		environment = BuildEnv(envOptions)
	}

	if environment == nil {
		if options.Authority != nil {
			return nil, authorityUnavailable(options.Authority)
		}

		return nil, errors.New("build native environment")
	}

	request := NativeRequest{Executable: executable, Arguments: append([]string(nil), arguments...), Environment: append([]string(nil), environment...), WorkingDirectory: cwd}

	if options.Authority != nil {
		if options.Authority.StartNative == nil {
			return nil, authorityUnavailable(options.Authority)
		}

		process, err := options.Authority.StartNative(ctx, request)
		if err != nil {
			// An error return from StartNative proves that no child exists. Preserve
			// the authority's own classification so callers can distinguish a normal
			// admission refusal from explicit authority or containment ambiguity.
			return nil, err
		}

		if !validInterfaceValue(process) {
			return nil, containmentIncomplete(options, "start native process", authorityUnavailable(options.Authority))
		}

		stdin := process.Stdin()
		stdout := process.Stdout()

		stderr := process.Stderr()
		if !validInterfaceValue(stdin) || !validInterfaceValue(stdout) || !validInterfaceValue(stderr) {
			return nil, settleUnusableNativeProcess(process, stdin, stdout, stderr, options)
		}

		return process, nil
	}

	return startOrdinaryNative(executable, arguments, environment, cwd)
}

func settleUnusableNativeProcess(
	process NativeProcess,
	stdin io.WriteCloser,
	stdout io.ReadCloser,
	stderr io.ReadCloser,
	options Options,
) error {
	var streamErr error

	for _, stream := range []io.Closer{stdin, stdout, stderr} {
		if validInterfaceValue(stream) {
			streamErr = errors.Join(streamErr, stream.Close())
		}
	}

	revokeErr := boundedNativeRevoke(process, processShutdownWaitDelay)

	_, waitErr := boundedNativeWait(process, processShutdownWaitDelay)
	if waitErr == nil {
		return errors.Join(authorityUnavailable(options.Authority), streamErr)
	}

	return containmentIncomplete(
		options,
		"settle unusable native process",
		errors.Join(authorityUnavailable(options.Authority), streamErr, revokeErr, waitErr),
	)
}

func boundedNativeRevoke(process NativeProcess, timeout time.Duration) error {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	return callNativeRevoke(ctx, process)
}

func boundedNativeWait(process NativeProcess, timeout time.Duration) (NativeResult, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	return callNativeWait(ctx, process)
}

func callNativeWait(ctx context.Context, process NativeProcess) (result NativeResult, err error) {
	defer func() {
		if recover() != nil {
			result = NativeResult{}
			err = errNativeProcessWaitPanic
		}
	}()

	return process.Wait(ctx)
}

func callNativeRevoke(ctx context.Context, process NativeProcess) (err error) {
	defer func() {
		if recover() != nil {
			err = errNativeProcessRevokePanic
		}
	}()

	return process.Revoke(ctx)
}

func validInterfaceValue(value any) bool {
	if value == nil {
		return false
	}

	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return !reflected.IsNil()
	default:
		return true
	}
}

func authorityUnavailable(authority *NativeAuthority) error {
	if authority != nil && authority.Unavailable != nil {
		return authority.Unavailable
	}

	return errors.New("host authority unavailable")
}

func containmentIncomplete(options Options, operation string, cause error) error {
	marker := options.ContainmentIncomplete
	if options.Authority != nil && options.Authority.ContainmentIncomplete != nil {
		marker = options.Authority.ContainmentIncomplete
	}

	if marker != nil {
		return fmt.Errorf("%w: %s: %w", marker, operation, cause)
	}

	return fmt.Errorf("native containment incomplete: %s: %w", operation, cause)
}
