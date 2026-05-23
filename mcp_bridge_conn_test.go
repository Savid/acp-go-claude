package claudeacp

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestMCPBridgeHandleConnRejectsUnsupportedVersion(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	agent := NewAgent()
	agent.setConnection(&stubAgentClient{})
	bridge := &mcpSessionBridge{
		agent: agent,
		token: "secret",
		conns: make(map[*mcpBridgeConn]struct{}),
	}

	left, right := net.Pipe()
	done := make(chan struct{})
	go func() {
		bridge.handleConn(ctx, left)
		close(done)
	}()

	_, err := right.Write([]byte(`{"version":999,"token":"secret","acpId":"server-1"}` + "\n"))
	require.NoError(t, err)

	select {
	case <-done:
	case <-ctx.Done():
		require.NoError(t, ctx.Err())
	}

	bridge.mu.Lock()
	require.Empty(t, bridge.conns)
	bridge.mu.Unlock()
	require.NoError(t, right.Close())
}
