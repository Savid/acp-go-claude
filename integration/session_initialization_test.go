//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	claudeacp "github.com/savid/acp-go-claude"
	"github.com/stretchr/testify/require"
)

func TestClaudeCLIColdResumeBeforeFirstPrompt(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()

	cwd := t.TempDir()
	store := claudeacp.NewInMemorySessionStore()
	connect := func(store claudeacp.SessionStore) *acp.ClientSideConnection {
		pipes := serveLiveAgentInRuntimeForTest(t, ctx, emptyClaudeRuntime(t), claudeacp.WithSessionStore(store))
		conn := acp.NewClientSideConnection(&recordingClient{}, pipes.clientInput, pipes.agentOutput)
		_, err := conn.Initialize(ctx, acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber})
		require.NoError(t, err)

		return conn
	}
	conn := connect(store)
	opened, err := conn.NewSession(ctx, claudeacp.NewSessionRequest(cwd))
	require.NoError(t, err)

	key := claudeacp.SessionKey{SessionID: string(opened.SessionId)}
	entries, err := store.Load(ctx, key)
	require.NoError(t, err)
	require.Len(t, entries, 1, "initialization must be durable before prompting or closing")
	snapshot := claudeacp.NewInMemorySessionStore()
	require.NoError(t, snapshot.Append(ctx, key, entries))

	_, err = conn.CloseSession(ctx, acp.CloseSessionRequest{SessionId: opened.SessionId})
	require.NoError(t, err)

	recovered := connect(snapshot)
	_, err = recovered.ResumeSession(ctx, claudeacp.ResumeSessionRequest(opened.SessionId, cwd))
	require.NoError(t, err)
	_, err = recovered.LoadSession(ctx, claudeacp.LoadSessionRequest(opened.SessionId, cwd))
	require.NoError(t, err)
	_, err = recovered.CloseSession(ctx, acp.CloseSessionRequest{SessionId: opened.SessionId})
	require.NoError(t, err)
	rows, err := snapshot.Load(ctx, key)
	require.NoError(t, err)
	require.Equal(t, entries, rows, "recovery must not invent a conversation")
}
