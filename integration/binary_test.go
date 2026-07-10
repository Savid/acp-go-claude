//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/require"
)

func TestClaudeACPAgentBinaryConversation(t *testing.T) {
	requireLiveTokens(t)
	parallelWhenPortableClaudeAuth(t)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	client := &recordingClient{}
	conn := connectLiveAgentBinary(t, ctx, client, acp.InitializeRequest{})

	session, err := conn.NewSession(ctx, acp.NewSessionRequest{Cwd: t.TempDir(), McpServers: []acp.McpServer{}})
	require.NoError(t, err)

	resp, err := conn.Prompt(ctx, acp.PromptRequest{
		SessionId: session.SessionId,
		Prompt:    []acp.ContentBlock{acp.TextBlock("Reply with exactly ACP_BINARY_OK and no punctuation.")},
	})
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, resp.StopReason)
	require.Contains(t, client.text(), "ACP_BINARY_OK")

	_, err = conn.CloseSession(ctx, acp.CloseSessionRequest{SessionId: session.SessionId})
	require.NoError(t, err)
}
