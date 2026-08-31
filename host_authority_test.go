package claudeacp

import (
	"context"
	"errors"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

type fakeHostAuthority struct {
	mu      sync.Mutex
	events  []string
	prepare error
	reclaim error
	start   func(context.Context, NativeRequest) (NativeProcess, error)
}

func newFakeHostAuthority() *fakeHostAuthority { return &fakeHostAuthority{} }

func (a *fakeHostAuthority) NativeEnvironment() map[string]string {
	return map[string]string{"PATH": "/usr/bin:/bin", "USER": "native"}
}

func (a *fakeHostAuthority) PrepareNativeTree(_ context.Context, root string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.events = append(a.events, "prepare:"+root)

	return a.prepare
}

func (a *fakeHostAuthority) ReclaimNativeTree(_ context.Context, root string) error {
	a.mu.Lock()
	defer a.mu.Unlock()
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
}

func TestMaterializedResidenceWrapsUncertainReclaim(t *testing.T) {
	root := t.TempDir() + "/native"
	authority := newFakeHostAuthority()
	authority.reclaim = errors.New("broker disconnected")
	residence := &materializedSession{configDir: root}
	require.NoError(t, residence.prepare(t.Context(), authority, root))
	require.ErrorIs(t, residence.Close(), ErrContainmentIncomplete)
}
