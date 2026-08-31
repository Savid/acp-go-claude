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

func startNative(ctx context.Context, options Options, executable string, arguments []string) (NativeProcess, error) {
	if executable == "" {
		executable = defaultCLIExecutable
	}

	cwd := options.Cwd
	if cwd == "" {
		var err error

		cwd, err = os.Getwd()
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
			if errors.Is(err, options.Authority.Unavailable) || errors.Is(err, options.Authority.ContainmentIncomplete) {
				return nil, err
			}

			return nil, containmentIncomplete(options, "start native process", err)
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
	done := make(chan error, 1)

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	go func() { done <- process.Revoke(ctx) }()

	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	}
}

func boundedNativeWait(process NativeProcess, timeout time.Duration) (NativeResult, error) {
	type waitResult struct {
		result NativeResult
		err    error
	}

	done := make(chan waitResult, 1)

	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	go func() {
		result, err := process.Wait(ctx)
		done <- waitResult{result: result, err: err}
	}()

	select {
	case waited := <-done:
		return waited.result, waited.err
	case <-ctx.Done():
		return NativeResult{}, ctx.Err()
	}
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
