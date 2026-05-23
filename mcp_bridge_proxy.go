package claudeacp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// RunMCPProxy runs the stdio shim launched by Claude for ACP-transport MCP.
// It is intended for the acp-go-claude mcp-proxy subcommand, not as a
// general library API.
func RunMCPProxy(ctx context.Context, stdin io.Reader, stdout io.Writer, options MCPProxyOptions) error {
	conn, err := mcpDialContext(ctx, options.Network, options.Address)
	if err != nil {
		return fmt.Errorf("connect MCP proxy bridge: %w", err)
	}
	defer conn.Close()

	if err := json.NewEncoder(conn).Encode(mcpProxyHello{
		Version: mcpProxyVersion,
		Token:   options.Token,
		ACPID:   options.ACPID,
	}); err != nil {
		return fmt.Errorf("send MCP proxy hello: %w", err)
	}

	errCh := make(chan error, 2)
	go proxyCopy(errCh, conn, stdin)
	go proxyCopy(errCh, stdout, conn)

	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-errCh:
		if errors.Is(err, io.EOF) {
			return nil
		}

		return err
	}
}

func proxyCopy(errCh chan<- error, dst io.Writer, src io.Reader) {
	scanner := bufio.NewScanner(src)
	scanner.Buffer(make([]byte, 0, mcpProxyInitialBuf), mcpProxyMaxBuf)

	for scanner.Scan() {
		line := append([]byte(nil), scanner.Bytes()...)
		line = append(line, '\n')

		if _, err := dst.Write(line); err != nil {
			errCh <- err

			return
		}
	}

	if err := scanner.Err(); err != nil {
		errCh <- err

		return
	}

	errCh <- io.EOF
}
