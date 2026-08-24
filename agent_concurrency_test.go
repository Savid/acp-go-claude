package claudeacp

import (
	"context"
	"fmt"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/require"
)

func TestConcurrencyLimitBranches(t *testing.T) {
	t.Parallel()

	require.ErrorContains(t, validateConcurrencyLimits(ConcurrencyLimits{MaxActiveSessions: -1}), "active sessions")
	require.ErrorContains(t, validateConcurrencyLimits(ConcurrencyLimits{MaxConcurrentClientCalls: -1}), "client calls")
	require.NoError(t, validateConcurrencyLimits(ConcurrencyLimits{}))

	require.NoError(t, validateConcurrencyLimits(ConcurrencyLimits{
		MaxActiveSessions: 64, MaxConcurrentClientCalls: 32,
	}))

	defaults := NewAgent()
	require.Equal(t, defaultMaxActiveSessions, defaults.maxActiveSessions())
	require.Equal(t, defaultMaxClientCalls, defaults.maxConcurrentClientCalls())

	custom := NewAgent(WithConcurrencyLimits(ConcurrencyLimits{
		MaxActiveSessions: 2, MaxConcurrentClientCalls: 3,
	}))
	require.Equal(t, 2, custom.maxActiveSessions())
	require.Equal(t, 3, custom.maxConcurrentClientCalls())

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	custom.clientCalls = make(chan struct{})
	release, err := custom.acquireClientCall(cancelled)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, release)
}

func TestClientCallCapacityBackpressuresExactlyWhenFull(t *testing.T) {
	t.Parallel()

	for _, limit := range []int{1, defaultMaxClientCalls, defaultMaxClientCalls + 8} {
		t.Run(fmt.Sprintf("capacity_%d", limit), func(t *testing.T) {
			t.Parallel()

			agent := NewAgent(WithConcurrencyLimits(ConcurrencyLimits{MaxConcurrentClientCalls: limit}))
			releases := make([]func(), 0, limit)
			for range limit {
				release, err := agent.acquireClientCall(t.Context())
				require.NoError(t, err)
				releases = append(releases, release)
			}

			release, err := agent.acquireClientCall(t.Context())
			require.Nil(t, release)

			var requestErr *acp.RequestError
			require.ErrorAs(t, err, &requestErr)
			require.Equal(t, -32600, requestErr.Code)
			require.Equal(t, map[string]any{
				jsonFieldError: "backpressure",
				"limit":        "client_calls",
			}, requestErr.Data)

			releases[0]()
			replacement, err := agent.acquireClientCall(t.Context())
			require.NoError(t, err)
			replacement()
			for _, held := range releases[1:] {
				held()
			}
		})
	}
}
