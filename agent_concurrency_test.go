package claudeacp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestConcurrencyLimitBranches(t *testing.T) {
	t.Parallel()

	require.ErrorContains(t, validateConcurrencyLimits(ConcurrencyLimits{MaxActiveSessions: -1}), "active sessions")
	require.ErrorContains(t, validateConcurrencyLimits(ConcurrencyLimits{MaxConcurrentClientCalls: -1}), "client calls")
	require.NoError(t, validateConcurrencyLimits(ConcurrencyLimits{}))

	// The remaining limits may be raised above their defaults.
	require.NoError(t, validateConcurrencyLimits(ConcurrencyLimits{MaxActiveSessions: 64, MaxConcurrentClientCalls: 32}))

	defaults := NewAgent()
	require.Equal(t, defaultMaxActiveSessions, defaults.maxActiveSessions())
	require.Equal(t, defaultMaxConcurrentClientCalls, defaults.maxConcurrentClientCalls())

	custom := NewAgent(WithConcurrencyLimits(ConcurrencyLimits{
		MaxActiveSessions:        2,
		MaxConcurrentClientCalls: 4,
	}))
	require.Equal(t, 2, custom.maxActiveSessions())
	require.Equal(t, 4, custom.maxConcurrentClientCalls())

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	custom.clientCalls = make(chan struct{})
	release, err := custom.acquireClientCall(cancelled)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, release)
}
