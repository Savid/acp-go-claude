package claudeacp

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/stretchr/testify/require"
)

type fakeHostAuthority struct {
	mu      sync.Mutex
	events  []string
	prepare error
	reclaim error
	start   func(context.Context, NativeRequest) (NativeProcess, error)
	hidden  map[string]string
	settled bool
}

type callbackHostAuthority struct {
	environment func() map[string]string
	prepare     func(context.Context, string) error
	reclaim     func(context.Context, string) error
	start       func(context.Context, NativeRequest) (NativeProcess, error)
}

func (a *callbackHostAuthority) NativeEnvironment() map[string]string {
	return a.environment()
}

func (a *callbackHostAuthority) PrepareNativeTree(ctx context.Context, root string) error {
	return a.prepare(ctx, root)
}

func (a *callbackHostAuthority) ReclaimNativeTree(ctx context.Context, root string) error {
	return a.reclaim(ctx, root)
}

func (a *callbackHostAuthority) StartNative(ctx context.Context, request NativeRequest) (NativeProcess, error) {
	return a.start(ctx, request)
}

func newFakeHostAuthority() *fakeHostAuthority {
	return &fakeHostAuthority{}
}

func (a *fakeHostAuthority) NativeEnvironment() map[string]string {
	return map[string]string{"PATH": "/usr/bin:/bin", "USER": "native"}
}

func (a *fakeHostAuthority) PrepareNativeTree(_ context.Context, root string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, "prepare:"+root)
	if a.prepare == nil && a.hidden != nil {
		hidden := root + ".authority"
		if err := os.Rename(root, hidden); err != nil {
			return err
		}
		a.hidden[root] = hidden
	}

	return a.prepare
}

func (a *fakeHostAuthority) ReclaimNativeTree(_ context.Context, root string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	if hidden := a.hidden[root]; hidden != "" {
		if !a.settled {
			return ErrNativeTreeBusy
		}
		if err := os.Rename(hidden, root); err != nil {
			return err
		}
		delete(a.hidden, root)
	}
	a.events = append(a.events, "reclaim:"+root)

	return a.reclaim
}

func (a *fakeHostAuthority) StartNative(ctx context.Context, request NativeRequest) (NativeProcess, error) {
	a.mu.Lock()
	a.events = append(a.events, "start:"+request.Executable)
	start := a.start
	a.mu.Unlock()
	if start == nil {
		return nil, ErrHostAuthorityUnavailable
	}

	return start(ctx, request)
}

func (a *fakeHostAuthority) snapshot() []string {
	a.mu.Lock()
	defer a.mu.Unlock()

	return append([]string(nil), a.events...)
}

type fakeNativeInput struct{ bytes.Buffer }

func (*fakeNativeInput) Close() error { return nil }

type valueHostAuthority struct{}

func (valueHostAuthority) NativeEnvironment() map[string]string {
	return map[string]string{"PATH": "/bin"}
}
func (valueHostAuthority) PrepareNativeTree(context.Context, string) error { return nil }
func (valueHostAuthority) ReclaimNativeTree(context.Context, string) error { return nil }
func (valueHostAuthority) StartNative(context.Context, NativeRequest) (NativeProcess, error) {
	return valueNativeProcess{}, nil
}

type valueNativeProcess struct{}

func (valueNativeProcess) Stdin() io.WriteCloser                      { return &fakeNativeInput{} }
func (valueNativeProcess) Stdout() io.ReadCloser                      { return io.NopCloser(bytes.NewReader(nil)) }
func (valueNativeProcess) Stderr() io.ReadCloser                      { return io.NopCloser(bytes.NewReader(nil)) }
func (valueNativeProcess) Wait(context.Context) (NativeResult, error) { return NativeResult{}, nil }
func (valueNativeProcess) Revoke(context.Context) error               { return nil }

type fakeNativeProcess struct {
	authority *fakeHostAuthority
	stdout    io.ReadCloser
	waitFor   <-chan struct{}
}

func (*fakeNativeProcess) Stdin() io.WriteCloser   { return &fakeNativeInput{} }
func (p *fakeNativeProcess) Stdout() io.ReadCloser { return p.stdout }
func (*fakeNativeProcess) Stderr() io.ReadCloser   { return io.NopCloser(bytes.NewReader(nil)) }
func (p *fakeNativeProcess) Wait(ctx context.Context) (NativeResult, error) {
	if p.waitFor != nil {
		select {
		case <-p.waitFor:
		case <-ctx.Done():
			return NativeResult{}, ctx.Err()
		}
	}
	p.authority.mu.Lock()
	p.authority.settled = true
	p.authority.events = append(p.authority.events, "wait:terminal")
	p.authority.mu.Unlock()

	return NativeResult{}, nil
}
func (*fakeNativeProcess) Revoke(context.Context) error { return nil }

func newAuthStatusProcess(authority *fakeHostAuthority, waitFor <-chan struct{}) NativeProcess {
	return &fakeNativeProcess{
		authority: authority,
		stdout:    io.NopCloser(bytes.NewBufferString(`{"loggedIn":true}`)),
		waitFor:   waitFor,
	}
}

func TestHostAuthorityManagedLaunchTrace(t *testing.T) {
	root := filepath.Join(t.TempDir(), "native")
	require.NoError(t, os.Mkdir(root, 0o700))

	authority := newFakeHostAuthority()
	authority.hidden = make(map[string]string)
	authority.start = func(context.Context, NativeRequest) (NativeProcess, error) {
		return newAuthStatusProcess(authority, nil), nil
	}
	agent := NewAgent(WithHostAuthority(authority))

	account, code, err := claude.AuthStatus(t.Context(), claude.Options{
		CLIPath: "claude", ClaudeHome: root, Authority: agent.claudeAuthority(),
	})
	require.NoError(t, err)
	require.Zero(t, code)
	require.True(t, account.LoggedIn)
	require.Equal(t, []string{"prepare:" + root, "start:claude", "wait:terminal", "reclaim:" + root}, authority.snapshot())
}

func TestHostAuthorityPreparedTreeExclusivity(t *testing.T) {
	root := filepath.Join(t.TempDir(), "native")
	require.NoError(t, os.Mkdir(root, 0o700))

	release := make(chan struct{})
	started := make(chan struct{})
	authority := newFakeHostAuthority()
	authority.hidden = make(map[string]string)
	authority.start = func(context.Context, NativeRequest) (NativeProcess, error) {
		close(started)

		return newAuthStatusProcess(authority, release), nil
	}
	agent := NewAgent(WithHostAuthority(authority))

	done := make(chan error, 1)
	go func() {
		_, _, err := claude.AuthStatus(context.Background(), claude.Options{
			CLIPath: "claude", ClaudeHome: root, Authority: agent.claudeAuthority(),
		})
		done <- err
	}()

	<-started
	_, err := os.Stat(root)
	require.ErrorIs(t, err, os.ErrNotExist)
	close(release)
	require.NoError(t, <-done)
	require.DirExists(t, root)
}

func TestHostAuthorityReclaimPrecedesRemoval(t *testing.T) {
	root := filepath.Join(t.TempDir(), "native")
	require.NoError(t, os.Mkdir(root, 0o700))

	authority := newFakeHostAuthority()
	authority.hidden = make(map[string]string)
	authority.start = func(context.Context, NativeRequest) (NativeProcess, error) {
		return newAuthStatusProcess(authority, nil), nil
	}
	agent := NewAgent(WithHostAuthority(authority))
	residence := &materializedSession{configDir: root}
	require.NoError(t, residence.prepare(t.Context(), authority, root))

	_, _, err := claude.AuthStatus(t.Context(), claude.Options{
		CLIPath: "claude", ClaudeHome: root, Authority: agent.claudeAuthority(), TreePrepared: true,
	})
	require.NoError(t, err)
	_, err = os.Stat(root)
	require.ErrorIs(t, err, os.ErrNotExist)
	require.NoError(t, residence.Close())
	_, err = os.Stat(root)
	require.ErrorIs(t, err, os.ErrNotExist)
	require.Equal(t, []string{"prepare:" + root, "start:claude", "wait:terminal", "reclaim:" + root}, authority.snapshot())
}

func TestHostAuthorityNoOrdinaryFallback(t *testing.T) {
	denied := errors.New("authority denied launch")
	authority := newFakeHostAuthority()
	authority.start = func(context.Context, NativeRequest) (NativeProcess, error) { return nil, denied }
	agent := NewAgent(WithHostAuthority(authority))

	_, _, err := claude.AuthStatus(t.Context(), claude.Options{
		CLIPath: "must-not-direct-launch", Authority: agent.claudeAuthority(),
	})
	require.ErrorIs(t, err, denied)
}

func TestHostAuthorityContractAndNilFailClosed(t *testing.T) {
	require.EqualError(t, ErrHostAuthorityUnavailable, "host authority unavailable")
	require.EqualError(t, ErrContainmentIncomplete, "native containment incomplete")
	require.EqualError(t, ErrNativeTreeBusy, "native tree has live lease processes")

	var typedNil *fakeHostAuthority
	agent := NewAgent(WithHostAuthority(typedNil))
	require.ErrorIs(t, agent.configurationErr, ErrHostAuthorityUnavailable)
	require.Nil(t, agent.providerAuth)
}

func TestHostAuthorityInterfaceValidationAndUnconfiguredAdapters(t *testing.T) {
	t.Parallel()

	require.False(t, validHostAuthority(nil))
	require.True(t, validHostAuthority(valueHostAuthority{}))
	require.False(t, validNativeProcess(nil))
	require.True(t, validNativeProcess(valueNativeProcess{}))
	require.Nil(t, (*Agent)(nil).claudeAuthority())
	require.Nil(t, NewAgent().claudeAuthority())

	broker := &providerAuth{agent: NewAgent()}
	require.Nil(t, broker.claudeAuthority())
	require.NoError(t, broker.reclaimIdleNativeHome(t.Context()))
}

func TestHostAuthorityLateEnvironmentPanicReturnsUnavailableBase(t *testing.T) {
	panicEnvironment := false
	authority := &callbackHostAuthority{
		environment: func() map[string]string {
			if panicEnvironment {
				panic("late environment panic")
			}

			return map[string]string{"PATH": "/bin"}
		},
		prepare: func(context.Context, string) error { return nil },
		reclaim: func(context.Context, string) error { return nil },
		start:   func(context.Context, NativeRequest) (NativeProcess, error) { return valueNativeProcess{}, nil },
	}
	agent := NewAgent(WithHostAuthority(authority))
	require.NoError(t, agent.configurationErr)

	panicEnvironment = true
	require.Nil(t, agent.claudeAuthority().NativeEnvironment())
}

func TestHostAuthorityRejectsReservedAndManagedEnvironmentKeys(t *testing.T) {
	t.Parallel()

	for _, key := range []string{privateAdapterEnvPrefix + "SECRET", "HOME", "XDG_CONFIG_HOME"} {
		agent := NewAgent(WithEnv(map[string]string{key: "value"}))
		require.Error(t, agent.configurationErr, key)
	}
}

func TestHostAuthorityNativeEnvironmentPanicFailsConstructionBeforeMutation(t *testing.T) {
	prepareCalls := 0
	startCalls := 0
	authority := &callbackHostAuthority{
		environment: func() map[string]string { panic("environment panic") },
		prepare: func(context.Context, string) error {
			prepareCalls++

			return nil
		},
		reclaim: func(context.Context, string) error { return nil },
		start: func(context.Context, NativeRequest) (NativeProcess, error) {
			startCalls++

			return nil, errors.New("unexpected start")
		},
	}

	agent := NewAgent(WithHostAuthority(authority))
	require.ErrorIs(t, agent.configurationErr, ErrHostAuthorityUnavailable)
	require.NotErrorIs(t, agent.configurationErr, ErrContainmentIncomplete)
	require.Zero(t, prepareCalls)
	require.Zero(t, startCalls)
}

func TestHostAuthorityNilNativeEnvironmentFailsConstructionBeforeMutation(t *testing.T) {
	prepareCalls := 0
	authority := &callbackHostAuthority{
		environment: func() map[string]string { return nil },
		prepare: func(context.Context, string) error {
			prepareCalls++

			return nil
		},
	}

	agent := NewAgent(WithHostAuthority(authority))
	require.ErrorIs(t, agent.configurationErr, ErrHostAuthorityUnavailable)
	require.NotErrorIs(t, agent.configurationErr, ErrContainmentIncomplete)
	require.Zero(t, prepareCalls)
}

func TestHostAuthorityAmbiguousCallbacksRetainPreparedTree(t *testing.T) {
	tests := []struct {
		name  string
		start func(context.Context, NativeRequest) (NativeProcess, error)
	}{
		{
			name: "panic",
			start: func(context.Context, NativeRequest) (NativeProcess, error) {
				panic("start panic")
			},
		},
		{
			name: "nil",
			start: func(context.Context, NativeRequest) (NativeProcess, error) {
				return nil, nil //nolint:nilnil // invalid successful callback is under test.
			},
		},
		{
			name: "typed nil",
			start: func(context.Context, NativeRequest) (NativeProcess, error) {
				var process *fakeNativeProcess

				return process, nil
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var events []string
			authority := &callbackHostAuthority{
				environment: func() map[string]string { return map[string]string{"PATH": "/bin"} },
				prepare: func(context.Context, string) error {
					events = append(events, "prepare")

					return nil
				},
				reclaim: func(context.Context, string) error {
					events = append(events, "reclaim")

					return nil
				},
				start: func(ctx context.Context, request NativeRequest) (NativeProcess, error) {
					events = append(events, "start")

					return tt.start(ctx, request)
				},
			}
			agent := NewAgent(WithHostAuthority(authority))

			_, _, err := claude.AuthStatus(t.Context(), claude.Options{
				CLIPath: "claude", ClaudeHome: t.TempDir(), Authority: agent.claudeAuthority(),
			})
			require.ErrorIs(t, err, ErrHostAuthorityUnavailable)
			require.ErrorIs(t, err, ErrContainmentIncomplete)
			require.Equal(t, []string{"prepare", "start"}, events)
		})
	}
}

func TestHostAuthorityPrepareAndReclaimPanicsAreContainmentAmbiguous(t *testing.T) {
	t.Run("prepare", func(t *testing.T) {
		startCalls := 0
		reclaimCalls := 0
		authority := &callbackHostAuthority{
			environment: func() map[string]string { return map[string]string{"PATH": "/bin"} },
			prepare:     func(context.Context, string) error { panic("prepare panic") },
			reclaim: func(context.Context, string) error {
				reclaimCalls++

				return nil
			},
			start: func(context.Context, NativeRequest) (NativeProcess, error) {
				startCalls++

				return nil, errors.New("unexpected start")
			},
		}
		agent := NewAgent(WithHostAuthority(authority))

		_, _, err := claude.AuthStatus(t.Context(), claude.Options{
			CLIPath: "claude", ClaudeHome: t.TempDir(), Authority: agent.claudeAuthority(),
		})
		require.ErrorIs(t, err, ErrHostAuthorityUnavailable)
		require.ErrorIs(t, err, ErrContainmentIncomplete)
		require.Zero(t, startCalls)
		require.Zero(t, reclaimCalls)
	})

	t.Run("reclaim", func(t *testing.T) {
		authority := &callbackHostAuthority{
			environment: func() map[string]string { return map[string]string{"PATH": "/bin"} },
			prepare:     func(context.Context, string) error { return nil },
			reclaim:     func(context.Context, string) error { panic("reclaim panic") },
		}
		authority.start = func(context.Context, NativeRequest) (NativeProcess, error) {
			return newAuthStatusProcess(&fakeHostAuthority{}, nil), nil
		}
		agent := NewAgent(WithHostAuthority(authority))

		_, _, err := claude.AuthStatus(t.Context(), claude.Options{
			CLIPath: "claude", ClaudeHome: t.TempDir(), Authority: agent.claudeAuthority(),
		})
		require.ErrorIs(t, err, ErrHostAuthorityUnavailable)
		require.ErrorIs(t, err, ErrContainmentIncomplete)
	})

	t.Run("reclaim failure", func(t *testing.T) {
		failure := errors.New("reclaim response lost")
		authority := &callbackHostAuthority{
			environment: func() map[string]string { return map[string]string{"PATH": "/bin"} },
			prepare:     func(context.Context, string) error { return nil },
			reclaim:     func(context.Context, string) error { return failure },
		}
		authority.start = func(context.Context, NativeRequest) (NativeProcess, error) {
			return newAuthStatusProcess(&fakeHostAuthority{}, nil), nil
		}
		agent := NewAgent(WithHostAuthority(authority))

		_, _, err := claude.AuthStatus(t.Context(), claude.Options{
			CLIPath: "claude", ClaudeHome: t.TempDir(), Authority: agent.claudeAuthority(),
		})
		require.ErrorIs(t, err, failure)
		require.ErrorIs(t, err, ErrContainmentIncomplete)
		require.NotErrorIs(t, err, ErrHostAuthorityUnavailable)
	})
}

func TestHostAuthorityOrdinaryStartRefusalRemainsOrdinary(t *testing.T) {
	refusal := errors.New("admission refused")
	reclaimed := 0
	authority := &callbackHostAuthority{
		environment: func() map[string]string { return map[string]string{"PATH": "/bin"} },
		prepare:     func(context.Context, string) error { return nil },
		reclaim: func(context.Context, string) error {
			reclaimed++

			return nil
		},
		start: func(context.Context, NativeRequest) (NativeProcess, error) { return nil, refusal },
	}
	agent := NewAgent(WithHostAuthority(authority))

	_, _, err := claude.AuthStatus(t.Context(), claude.Options{
		CLIPath: "claude", ClaudeHome: t.TempDir(), Authority: agent.claudeAuthority(),
	})
	require.ErrorIs(t, err, refusal)
	require.NotErrorIs(t, err, ErrHostAuthorityUnavailable)
	require.NotErrorIs(t, err, ErrContainmentIncomplete)
	require.Equal(t, 1, reclaimed)
}

func TestAmbiguousRuntimeRetainsEveryMaterializedResource(t *testing.T) {
	root := t.TempDir()
	reclaimCalls := 0
	removeCalls := 0
	materialized := &materializedSession{
		configDir: root,
		prepared:  []string{root},
		authority: &callbackHostAuthority{
			reclaim: func(context.Context, string) error {
				reclaimCalls++

				return nil
			},
		},
	}
	originalRemove := sessionRemoveAll
	sessionRemoveAll = func(string) error {
		removeCalls++

		return nil
	}
	t.Cleanup(func() { sessionRemoveAll = originalRemove })

	err := finalizeSessionRuntimeResources(
		errors.Join(ErrHostAuthorityUnavailable, ErrContainmentIncomplete),
		filepath.Join(root, "mcp"), filepath.Join(root, "images"), materialized,
	)
	require.ErrorIs(t, err, ErrHostAuthorityUnavailable)
	require.ErrorIs(t, err, ErrContainmentIncomplete)
	require.Zero(t, reclaimCalls)
	require.Zero(t, removeCalls)
	require.Equal(t, []string{root}, materialized.prepared)
}

func TestMaterializedResidenceReclaimsBeforeRemovalAndBusyFailsClosed(t *testing.T) {
	root := t.TempDir() + "/native"
	authority := newFakeHostAuthority()
	residence := &materializedSession{configDir: root}
	require.NoError(t, residence.prepare(t.Context(), authority, root))

	originalRemove := materializeRemoveAll
	removed := false
	materializeRemoveAll = func(path string) error {
		removed = true
		require.Equal(t, root, path)

		return nil
	}
	t.Cleanup(func() { materializeRemoveAll = originalRemove })
	require.NoError(t, residence.Close())
	require.True(t, removed)
	require.Equal(t, []string{"prepare:" + root, "reclaim:" + root}, authority.events)

	removed = false
	authority.events = nil
	authority.reclaim = ErrNativeTreeBusy
	busy := &materializedSession{configDir: root}
	require.NoError(t, busy.prepare(t.Context(), authority, root))
	require.ErrorIs(t, busy.Close(), ErrNativeTreeBusy)
	require.False(t, removed)

	authority.reclaim = nil
	require.NoError(t, busy.Close())
	require.True(t, removed)
	require.Equal(t, []string{"prepare:" + root, "reclaim:" + root, "reclaim:" + root}, authority.events)
}

func TestMaterializedResidenceWrapsUncertainReclaim(t *testing.T) {
	root := t.TempDir() + "/native"
	authority := newFakeHostAuthority()
	authority.reclaim = errors.New("broker disconnected")
	residence := &materializedSession{configDir: root}
	require.NoError(t, residence.prepare(t.Context(), authority, root))
	require.ErrorIs(t, residence.Close(), ErrContainmentIncomplete)
}

func TestMaterializedResidenceRetainsFailedPrepareWithoutPathAccess(t *testing.T) {
	root := filepath.Join(t.TempDir(), "native")
	authority := newFakeHostAuthority()
	authority.prepare = errors.New("prepare response lost")
	residence := &materializedSession{configDir: root}

	removed := false
	originalRemove := materializeRemoveAll
	materializeRemoveAll = func(string) error {
		removed = true

		return nil
	}
	t.Cleanup(func() { materializeRemoveAll = originalRemove })

	require.ErrorIs(t, residence.prepare(t.Context(), authority, root), ErrContainmentIncomplete)
	require.ErrorIs(t, residence.Close(), ErrContainmentIncomplete)
	require.False(t, removed)
	require.Equal(t, []string{"prepare:" + root}, authority.events)
}

func TestMaterializedResidenceRetainsMutatedRootAfterPreparePanic(t *testing.T) {
	root := filepath.Join(t.TempDir(), "native")
	require.NoError(t, os.MkdirAll(root, 0o700))
	marker := filepath.Join(root, "mutated")
	authority := &callbackHostAuthority{
		prepare: func(context.Context, string) error {
			require.NoError(t, os.WriteFile(marker, []byte("host mutation"), 0o600))
			panic("prepare response lost")
		},
	}
	residence := &materializedSession{configDir: root}

	err := residence.prepare(t.Context(), authority, root)
	require.ErrorIs(t, err, ErrHostAuthorityUnavailable)
	require.ErrorIs(t, err, ErrContainmentIncomplete)
	require.Empty(t, residence.prepared)
	require.Contains(t, residence.opaque, root)

	err = residence.Close()
	require.ErrorIs(t, err, ErrContainmentIncomplete)
	require.FileExists(t, marker)
}

func TestMaterializedResidenceRetainsRootAfterReclaimPanic(t *testing.T) {
	root := filepath.Join(t.TempDir(), "native")
	require.NoError(t, os.MkdirAll(root, 0o700))
	authority := &callbackHostAuthority{
		prepare: func(context.Context, string) error { return nil },
		reclaim: func(context.Context, string) error { panic("reclaim response lost") },
	}
	residence := &materializedSession{configDir: root}
	require.NoError(t, residence.prepare(t.Context(), authority, root))

	err := residence.Close()
	require.ErrorIs(t, err, ErrHostAuthorityUnavailable)
	require.ErrorIs(t, err, ErrContainmentIncomplete)
	require.Equal(t, []string{root}, residence.prepared)
	require.DirExists(t, root)
}

func TestMaterializedResidenceBoundsReclaimWithDetachedContext(t *testing.T) {
	previousTimeout := materializedNativeTreeReclaimTimeout
	materializedNativeTreeReclaimTimeout = time.Nanosecond
	t.Cleanup(func() { materializedNativeTreeReclaimTimeout = previousTimeout })

	root := filepath.Join(t.TempDir(), "native")
	require.NoError(t, os.MkdirAll(root, 0o700))
	callbackObservedDeadline := false
	authority := &callbackHostAuthority{
		prepare: func(context.Context, string) error { return nil },
		reclaim: func(ctx context.Context, _ string) error {
			_, callbackObservedDeadline = ctx.Deadline()
			<-ctx.Done()

			return ctx.Err()
		},
	}
	residence := &materializedSession{configDir: root}
	require.NoError(t, residence.prepare(t.Context(), authority, root))

	err := residence.Close()
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.ErrorIs(t, err, ErrContainmentIncomplete)
	require.True(t, callbackObservedDeadline)
	require.Equal(t, []string{root}, residence.prepared)
	require.DirExists(t, root)
}

func TestProviderAuthHomeReadExcludesNativePreparation(t *testing.T) {
	authority := newFakeHostAuthority()
	agent := NewAgent(WithHostAuthority(authority))
	broker := &providerAuth{agent: agent, home: providerAuthHome{path: filepath.Join(t.TempDir(), "home")}}

	release, err := broker.admitNativeHomeRead(t.Context())
	require.NoError(t, err)

	prepareDone := make(chan error, 1)
	go func() {
		prepareDone <- broker.claudeAuthority().PrepareNativeTree(t.Context(), broker.home.path)
	}()

	select {
	case prepareErr := <-prepareDone:
		require.Failf(t, "prepare crossed active home read", "error: %v", prepareErr)
	default:
	}

	release()
	require.NoError(t, <-prepareDone)

	_, err = broker.admitNativeHomeRead(t.Context())
	require.ErrorIs(t, err, ErrNativeTreeBusy)
}
