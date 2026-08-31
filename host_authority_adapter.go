package claudeacp

import (
	"context"
	"reflect"

	"github.com/savid/acp-go-claude/internal/claude"
)

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

func (p *providerAuth) claudeAuthority() *claude.NativeAuthority {
	base := p.agent.claudeAuthority()
	if base == nil {
		return nil
	}

	authority := *base
	authority.PrepareNativeTree = func(ctx context.Context, root string) error {
		if root != p.home.path {
			return base.PrepareNativeTree(ctx, root)
		}

		p.nativeTreeMu.Lock()
		defer p.nativeTreeMu.Unlock()

		if !p.nativeTreePrepared {
			if err := base.PrepareNativeTree(ctx, root); err != nil {
				return err
			}

			p.nativeTreePrepared = true
		}

		p.nativeTreeUsers++

		return nil
	}
	authority.ReclaimNativeTree = func(ctx context.Context, root string) error {
		if root != p.home.path {
			return base.ReclaimNativeTree(ctx, root)
		}

		p.nativeTreeMu.Lock()
		defer p.nativeTreeMu.Unlock()

		if !p.nativeTreePrepared || p.nativeTreeUsers <= 0 {
			return ErrContainmentIncomplete
		}

		p.nativeTreeUsers--
		if p.nativeTreeUsers > 0 {
			return nil
		}

		if err := base.ReclaimNativeTree(ctx, root); err != nil {
			return err
		}

		p.nativeTreePrepared = false

		return nil
	}

	return &authority
}

func (p *providerAuth) reclaimIdleNativeHome(ctx context.Context) error {
	base := p.agent.claudeAuthority()
	if base == nil {
		return nil
	}

	p.nativeTreeMu.Lock()
	defer p.nativeTreeMu.Unlock()

	if p.nativeTreeUsers > 0 {
		return ErrNativeTreeBusy
	}

	if !p.nativeTreePrepared {
		return nil
	}

	if err := base.ReclaimNativeTree(ctx, p.home.path); err != nil {
		return err
	}

	p.nativeTreePrepared = false

	return nil
}
