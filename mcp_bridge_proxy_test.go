package claudeacp

import (
	"context"
	"encoding/json"
	"io"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestRunMCPProxySendsVersionedHello(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(t, err)
	t.Cleanup(func() { _ = ln.Close() })

	helloCh := make(chan mcpProxyHello, 1)
	serverErr := make(chan error, 1)
	go func() {
		conn, acceptErr := ln.Accept()
		if acceptErr != nil {
			serverErr <- acceptErr

			return
		}
		defer conn.Close()

		var hello mcpProxyHello
		if decodeErr := json.NewDecoder(conn).Decode(&hello); decodeErr != nil {
			serverErr <- decodeErr

			return
		}

		helloCh <- hello
		serverErr <- nil
	}()

	errCh := make(chan error, 1)
	go func() {
		errCh <- RunMCPProxy(ctx, strings.NewReader(""), io.Discard, MCPProxyOptions{
			Network: "tcp",
			Address: ln.Addr().String(),
			Token:   "secret",
			ACPID:   "server-1",
		})
	}()

	var hello mcpProxyHello
	select {
	case hello = <-helloCh:
	case <-ctx.Done():
		require.NoError(t, ctx.Err())
	}
	require.Equal(t, mcpProxyVersion, hello.Version)
	require.Equal(t, "secret", hello.Token)
	require.Equal(t, "server-1", hello.ACPID)

	select {
	case err := <-serverErr:
		require.NoError(t, err)
	case <-ctx.Done():
		require.NoError(t, ctx.Err())
	}

	select {
	case err := <-errCh:
		require.NoError(t, err)
	case <-ctx.Done():
		require.NoError(t, ctx.Err())
	}
}
