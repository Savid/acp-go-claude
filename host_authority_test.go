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
