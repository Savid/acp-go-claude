package claudeacp

import (
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/require"
)

func TestSessionCloseCurrentBoundaryIsIdempotent(t *testing.T) {
	transport := newFakeClaudeTransport()
	agent, _, _ := newFakeLifecycleAgent(t, transport)
	sessionID := acp.SessionId("session-close-current")
	session := newSessionForTransport(t, agent, sessionID, transport)
	agent.sessions[sessionID] = session

	_, err := agent.CloseSession(t.Context(), acp.CloseSessionRequest{SessionId: sessionID})
	require.NoError(t, err)
	require.Equal(t, 1, transport.CloseCalls())

	_, err = agent.CloseSession(t.Context(), acp.CloseSessionRequest{SessionId: sessionID})
	require.Error(t, err)
	require.Equal(t, 1, transport.CloseCalls())
}
