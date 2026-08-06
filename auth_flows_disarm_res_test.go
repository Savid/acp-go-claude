package claudeacp

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestAuthFlowStopCompleterIsIdempotent proves that disarming a login flow's
// completer twice disarms it once and returns quietly the second time. Several
// terminal paths — cancellation, supersession, completion and teardown — each
// disarm the flow they are ending, and more than one of them can run for the
// same flow. Closing an already-closed channel panics, which would take the
// whole agent process down rather than end one login, so the second disarm has
// to observe the closed channel instead of closing it again.
func TestAuthFlowStopCompleterIsIdempotent(t *testing.T) {
	flow := &authFlow{disarm: make(chan struct{})}

	flow.stopCompleter()
	select {
	case <-flow.disarm:
	default:
		require.Fail(t, "the first disarm must close the completer channel")
	}

	require.NotPanics(t, flow.stopCompleter, "a second disarm must not close the channel again")
	select {
	case <-flow.disarm:
	default:
		require.Fail(t, "the completer channel must stay closed")
	}
}
