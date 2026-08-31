package claude

import (
	"context"
	"errors"
	"fmt"
	"os"
	"reflect"
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
			return nil, err
		}

		if !validInterfaceValue(process) || !validInterfaceValue(process.Stdin()) || !validInterfaceValue(process.Stdout()) || !validInterfaceValue(process.Stderr()) {
			return nil, authorityUnavailable(options.Authority)
		}

		return process, nil
	}

	return startOrdinaryNative(executable, arguments, environment, cwd)
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
