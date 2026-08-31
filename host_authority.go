package claudeacp

import (
	"context"
	"errors"
	"io"
	"reflect"

	"github.com/savid/acp-go-claude/internal/claude"
)

var (
	ErrHostAuthorityUnavailable = errors.New("host authority unavailable")
	ErrContainmentIncomplete    = errors.New("native containment incomplete")
	ErrNativeTreeBusy           = errors.New("native tree has live lease processes")
)

type HostAuthority interface {
	NativeEnvironment() map[string]string
	PrepareNativeTree(context.Context, string) error
	ReclaimNativeTree(context.Context, string) error
	StartNative(context.Context, NativeRequest) (NativeProcess, error)
}

type NativeRequest struct {
	Executable       string
	Arguments        []string
	Environment      []string
	WorkingDirectory string
}

type NativeProcess interface {
	Stdin() io.WriteCloser
	Stdout() io.ReadCloser
	Stderr() io.ReadCloser
	Wait(context.Context) (NativeResult, error)
	Revoke(context.Context) error
}

type NativeResult struct {
	ExitCode int
	Signal   int
	Revoked  bool
}

func validHostAuthority(authority HostAuthority) bool {
	if authority == nil {
		return false
	}

	value := reflect.ValueOf(authority)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return !value.IsNil()
	default:
		return true
	}
}

func (a *Agent) claudeAuthority() *claude.NativeAuthority {
	if a == nil || !a.options.hostAuthoritySet || !validHostAuthority(a.options.HostAuthority) {
		return nil
	}

	authority := a.options.HostAuthority

	return &claude.NativeAuthority{
		Unavailable:           ErrHostAuthorityUnavailable,
		ContainmentIncomplete: ErrContainmentIncomplete,
		TreeBusy:              ErrNativeTreeBusy,
		NativeEnvironment:     authority.NativeEnvironment,
		PrepareNativeTree:     authority.PrepareNativeTree,
		ReclaimNativeTree:     authority.ReclaimNativeTree,
		StartNative: func(ctx context.Context, request claude.NativeRequest) (claude.NativeProcess, error) {
			process, err := authority.StartNative(ctx, NativeRequest{
				Executable:       request.Executable,
				Arguments:        append([]string(nil), request.Arguments...),
				Environment:      append([]string(nil), request.Environment...),
				WorkingDirectory: request.WorkingDirectory,
			})
			if err != nil {
				return nil, err
			}

			if !validNativeProcess(process) {
				return nil, ErrHostAuthorityUnavailable
			}

			return nativeProcessAdapter{NativeProcess: process}, nil
		},
	}
}

func validNativeProcess(process NativeProcess) bool {
	if process == nil {
		return false
	}

	value := reflect.ValueOf(process)
	switch value.Kind() {
	case reflect.Chan, reflect.Func, reflect.Interface, reflect.Map, reflect.Pointer, reflect.Slice:
		return !value.IsNil()
	default:
		return true
	}
}

type nativeProcessAdapter struct {
	NativeProcess
}

func (p nativeProcessAdapter) Wait(ctx context.Context) (claude.NativeResult, error) {
	result, err := p.NativeProcess.Wait(ctx)

	return claude.NativeResult(result), err
}
