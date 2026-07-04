package claudeacp

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/stretchr/testify/require"
)

func TestNewAgentDefaultClientAndCloseBranches(t *testing.T) {
	t.Parallel()

	agent := NewAgent()
	client := agent.newClaudeClient(nil, claude.Options{})
	require.NotNil(t, client)

	transport := newFakeClaudeTransport()
	sessionClient := claude.NewClient(nil, claude.Options{}, transport)
	require.NoError(t, sessionClient.Start(context.Background()))
	transport.closeErr = errors.New("close failed")

	session := &agentSession{
		agent:         agent,
		id:            "session-1",
		client:        sessionClient,
		turn:          make(chan struct{}, 1),
		closeTurnWait: defaultSessionCloseTurnWait,
	}
	agent.sessions[session.id] = session
	agent.permissionCache[session.id] = map[string]string{"Read": "allow"}
	agent.deleted[session.id] = struct{}{}
	agent.setConnection(newRecordingAgentClient())

	err := agent.Close()
	require.ErrorContains(t, err, "close failed")
	require.Empty(t, agent.sessions)
	require.Empty(t, agent.permissionCache)
	require.Empty(t, agent.deleted)
	require.True(t, agent.closed)
}

func TestServeDoneAndCloseErrorBranches(t *testing.T) {
	require.NoError(t, Serve(context.Background(), bytes.NewBuffer(nil), io.Discard))

	previous := newServeAgent
	t.Cleanup(func() { newServeAgent = previous })

	agent := NewAgent()
	transport := newFakeClaudeTransport()
	sessionClient := claude.NewClient(nil, claude.Options{}, transport)
	require.NoError(t, sessionClient.Start(context.Background()))
	transport.closeErr = errors.New("close failed")
	agent.sessions["session-1"] = &agentSession{
		agent:         agent,
		id:            acp.SessionId("session-1"),
		client:        sessionClient,
		turn:          make(chan struct{}, 1),
		closeTurnWait: defaultSessionCloseTurnWait,
	}
	newServeAgent = func(...Option) *Agent { return agent }

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	require.ErrorIs(t, Serve(ctx, &blockingReader{}, io.Discard), context.Canceled)
}
