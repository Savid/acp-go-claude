package claudeacp

import (
	"context"
	"errors"
	"fmt"
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
		NativeEnvironment: func() (environment map[string]string) {
			defer func() {
				if recover() != nil {
					environment = nil
				}
			}()

			return authority.NativeEnvironment()
		},
		PrepareNativeTree: func(ctx context.Context, root string) (err error) {
			defer func() {
				if recovered := recover(); recovered != nil {
					err = authorityCallbackAmbiguous("prepare native tree", recovered)
				}
			}()

			return authority.PrepareNativeTree(ctx, root)
		},
		ReclaimNativeTree: func(ctx context.Context, root string) (err error) {
			defer func() {
				if recovered := recover(); recovered != nil {
					err = authorityCallbackAmbiguous("reclaim native tree", recovered)
				}
			}()

			err = authority.ReclaimNativeTree(ctx, root)
			if err != nil && !errors.Is(err, ErrNativeTreeBusy) && !errors.Is(err, ErrContainmentIncomplete) {
				return fmt.Errorf("%w: reclaim native tree: %w", ErrContainmentIncomplete, err)
			}

			return err
		},
		StartNative: func(ctx context.Context, request claude.NativeRequest) (claude.NativeProcess, error) {
			return guardedStartNative(authority, ctx, request)
		},
	}
}

func guardedStartNative(
	authority HostAuthority,
	ctx context.Context,
	request claude.NativeRequest,
) (process claude.NativeProcess, err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			process = nil
			err = authorityCallbackAmbiguous("start native process", recovered)
		}
	}()

	nativeProcess, err := authority.StartNative(ctx, NativeRequest{
		Executable:       request.Executable,
		Arguments:        append([]string(nil), request.Arguments...),
		Environment:      append([]string(nil), request.Environment...),
		WorkingDirectory: request.WorkingDirectory,
	})
	if err != nil {
		return nil, err
	}

	if !validNativeProcess(nativeProcess) {
		return nil, errors.Join(ErrHostAuthorityUnavailable, ErrContainmentIncomplete)
	}

	return nativeProcessAdapter{NativeProcess: nativeProcess}, nil
}

func authorityCallbackAmbiguous(operation string, recovered any) error {
	return fmt.Errorf("%w: %w: %s callback panicked: %T", ErrHostAuthorityUnavailable, ErrContainmentIncomplete, operation, recovered)
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

		if err := p.takeNativeHomeAccess(ctx); err != nil {
			return err
		}
		defer p.releaseNativeHomeAccess()

		p.nativeTreeMu.Lock()
		defer p.nativeTreeMu.Unlock()

		if p.nativeTreeOpaque != nil {
			return errors.Join(ErrContainmentIncomplete, p.nativeTreeOpaque)
		}

		if !p.nativeTreePrepared {
			if err := base.PrepareNativeTree(ctx, root); err != nil {
				p.nativeTreeOpaque = err
				p.agent.recordContainmentError(errors.Join(ErrContainmentIncomplete, err))

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

		if err := p.takeNativeHomeAccess(ctx); err != nil {
			return err
		}
		defer p.releaseNativeHomeAccess()

		p.nativeTreeMu.Lock()
		defer p.nativeTreeMu.Unlock()

		if p.nativeTreeOpaque != nil {
			return errors.Join(ErrContainmentIncomplete, p.nativeTreeOpaque)
		}

		if !p.nativeTreePrepared {
			return ErrContainmentIncomplete
		}

		if p.nativeTreeUsers > 0 {
			p.nativeTreeUsers--
		}

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

	if err := p.takeNativeHomeAccess(ctx); err != nil {
		return err
	}
	defer p.releaseNativeHomeAccess()

	p.nativeTreeMu.Lock()
	defer p.nativeTreeMu.Unlock()

	if p.nativeTreeOpaque != nil {
		return errors.Join(ErrContainmentIncomplete, p.nativeTreeOpaque)
	}

	if p.nativeTreeUsers > 0 {
		return ErrNativeTreeBusy
	}

	if !p.nativeTreePrepared {
		return nil
	}

	if err := base.ReclaimNativeTree(ctx, p.home.path); err != nil {
		if errors.Is(err, ErrNativeTreeBusy) {
			return err
		}

		p.agent.recordContainmentError(err)

		return err
	}

	p.nativeTreePrepared = false

	return nil
}

func (p *providerAuth) takeNativeHomeAccess(ctx context.Context) error {
	p.nativeTreeMu.Lock()
	if p.nativeHomeAccess == nil {
		p.nativeHomeAccess = make(chan struct{}, 1)
		p.nativeHomeAccess <- struct{}{}
	}

	access := p.nativeHomeAccess
	p.nativeTreeMu.Unlock()

	select {
	case <-access:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

func (p *providerAuth) releaseNativeHomeAccess() {
	p.nativeHomeAccess <- struct{}{}
}

func (p *providerAuth) admitNativeHomeRead(ctx context.Context) (func(), error) {
	if err := p.retryRetainedLogins(); err != nil && !errors.Is(err, ErrNativeTreeBusy) {
		return nil, err
	}

	if err := p.takeNativeHomeAccess(ctx); err != nil {
		return nil, err
	}

	p.nativeTreeMu.Lock()
	opaque := p.nativeTreeOpaque
	prepared := p.nativeTreePrepared || p.nativeTreeUsers > 0
	p.nativeTreeMu.Unlock()

	if opaque != nil {
		p.releaseNativeHomeAccess()

		return nil, errors.Join(ErrContainmentIncomplete, opaque)
	}

	if prepared {
		p.releaseNativeHomeAccess()

		return nil, ErrNativeTreeBusy
	}

	return p.releaseNativeHomeAccess, nil
}
