package claudeacp

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProviderAuthClaudeAuthorityResidualBranches(t *testing.T) {
	require.Nil(t, (&providerAuth{agent: NewAgent()}).claudeAuthority())

	t.Run("delegates other roots", func(t *testing.T) {
		events := []string{}
		authority := residualCallbackAuthority()
		authority.prepare = func(_ context.Context, root string) error {
			events = append(events, "prepare:"+root)

			return nil
		}
		authority.reclaim = func(_ context.Context, root string) error {
			events = append(events, "reclaim:"+root)

			return nil
		}
		wrapped := residualProviderAuth(authority).claudeAuthority()
		require.NoError(t, wrapped.PrepareNativeTree(t.Context(), "/other"))
		require.NoError(t, wrapped.ReclaimNativeTree(t.Context(), "/other"))
		require.Equal(t, []string{"prepare:/other", "reclaim:/other"}, events)
	})

	t.Run("access cancellation", func(t *testing.T) {
		broker := residualProviderAuth(residualCallbackAuthority())
		require.NoError(t, broker.takeNativeHomeAccess(t.Context()))
		wrapped := broker.claudeAuthority()
		require.ErrorIs(t, wrapped.PrepareNativeTree(residualCanceledContext(), broker.home.path), context.Canceled)
		require.ErrorIs(t, wrapped.ReclaimNativeTree(residualCanceledContext(), broker.home.path), context.Canceled)
		broker.releaseNativeHomeAccess()
	})

	t.Run("opaque tree", func(t *testing.T) {
		broker := residualProviderAuth(residualCallbackAuthority())
		broker.nativeTreeOpaque = errors.New("opaque")
		wrapped := broker.claudeAuthority()
		require.ErrorIs(t, wrapped.PrepareNativeTree(t.Context(), broker.home.path), ErrContainmentIncomplete)
		require.ErrorIs(t, wrapped.ReclaimNativeTree(t.Context(), broker.home.path), ErrContainmentIncomplete)
	})

	t.Run("prepare failure and reuse", func(t *testing.T) {
		authority := residualCallbackAuthority()
		authority.prepare = func(context.Context, string) error { return errors.New("prepare refused") }
		broker := residualProviderAuth(authority)
		wrapped := broker.claudeAuthority()
		require.ErrorContains(t, wrapped.PrepareNativeTree(t.Context(), broker.home.path), "prepare refused")
		require.NotNil(t, broker.nativeTreeOpaque)

		broker = residualProviderAuth(residualCallbackAuthority())
		wrapped = broker.claudeAuthority()
		require.NoError(t, wrapped.PrepareNativeTree(t.Context(), broker.home.path))
		require.NoError(t, wrapped.PrepareNativeTree(t.Context(), broker.home.path))
		require.Equal(t, 2, broker.nativeTreeUsers)
		require.NoError(t, wrapped.ReclaimNativeTree(t.Context(), broker.home.path))
		require.Equal(t, 1, broker.nativeTreeUsers)
		require.True(t, broker.nativeTreePrepared)
		require.NoError(t, wrapped.ReclaimNativeTree(t.Context(), broker.home.path))
		require.False(t, broker.nativeTreePrepared)
	})

	t.Run("reclaim failures", func(t *testing.T) {
		broker := residualProviderAuth(residualCallbackAuthority())
		wrapped := broker.claudeAuthority()
		require.ErrorIs(t, wrapped.ReclaimNativeTree(t.Context(), broker.home.path), ErrContainmentIncomplete)

		authority := residualCallbackAuthority()
		authority.reclaim = func(context.Context, string) error { return errors.New("reclaim refused") }
		broker = residualProviderAuth(authority)
		broker.nativeTreePrepared = true
		wrapped = broker.claudeAuthority()
		require.ErrorIs(t, wrapped.ReclaimNativeTree(t.Context(), broker.home.path), ErrContainmentIncomplete)
	})
}

func TestProviderAuthIdleReclaimResidualBranches(t *testing.T) {
	require.NoError(t, (&providerAuth{agent: NewAgent()}).reclaimIdleNativeHome(t.Context()))

	broker := residualProviderAuth(residualCallbackAuthority())
	require.NoError(t, broker.takeNativeHomeAccess(t.Context()))
	require.ErrorIs(t, broker.reclaimIdleNativeHome(residualCanceledContext()), context.Canceled)
	broker.releaseNativeHomeAccess()

	broker.nativeTreeOpaque = errors.New("opaque")
	require.ErrorIs(t, broker.reclaimIdleNativeHome(t.Context()), ErrContainmentIncomplete)
	broker.nativeTreeOpaque = nil
	broker.nativeTreeUsers = 1
	require.ErrorIs(t, broker.reclaimIdleNativeHome(t.Context()), ErrNativeTreeBusy)
	broker.nativeTreeUsers = 0
	require.NoError(t, broker.reclaimIdleNativeHome(t.Context()))

	for _, failure := range []error{ErrNativeTreeBusy, ErrContainmentIncomplete, errors.New("reclaim refused")} {
		authority := residualCallbackAuthority()
		authority.reclaim = func(context.Context, string) error { return failure }
		broker = residualProviderAuth(authority)
		broker.nativeTreePrepared = true
		err := broker.reclaimIdleNativeHome(t.Context())
		if errors.Is(failure, ErrNativeTreeBusy) {
			require.ErrorIs(t, err, ErrNativeTreeBusy)
		} else {
			require.ErrorIs(t, err, ErrContainmentIncomplete)
		}
	}

	broker = residualProviderAuth(residualCallbackAuthority())
	broker.nativeTreePrepared = true
	require.NoError(t, broker.reclaimIdleNativeHome(t.Context()))
	require.False(t, broker.nativeTreePrepared)
}
