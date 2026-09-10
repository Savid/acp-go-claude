package claudeacp

import (
	"errors"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/savid/acp-go-claude/internal/claude"
)

func TestSessionInitializationBeforePublication(t *testing.T) {
	store := NewInMemorySessionStore()
	agent, _, _ := newFakeLifecycleAgent(t, newFakeClaudeTransport(), WithSessionStore(store))
	cwd := t.TempDir()
	opened, err := agent.NewSession(t.Context(), NewSessionRequest(cwd))
	require.NoError(t, err)
	stored, err := agent.storedSession(t.Context(), opened.SessionId)
	require.NoError(t, err)
	require.Empty(t, stored.Entries)

	recovered, _, _ := newFakeLifecycleAgent(t, newFakeClaudeTransport(), WithSessionStore(store))
	var launch claude.Options
	newClient := recovered.newClaudeClient
	recovered.newClaudeClient = func(log *slog.Logger, options claude.Options) *claude.Client {
		launch = options

		return newClient(log, options)
	}
	_, err = recovered.ResumeSession(t.Context(), ResumeSessionRequest(opened.SessionId, cwd))
	require.NoError(t, err)
	require.Equal(t, string(opened.SessionId), launch.SessionID)
	require.Empty(t, launch.ResumeID)
}

func TestSessionInitializationFailureDoesNotPublish(t *testing.T) {
	want := errors.New("durable store unavailable")
	store := &faultSessionStore{SessionStore: NewInMemorySessionStore(), appendErr: want}
	agent, _, _ := newFakeLifecycleAgent(t, newFakeClaudeTransport(), WithSessionStore(store))
	opened, err := agent.NewSession(t.Context(), NewSessionRequest(t.TempDir()))
	require.ErrorIs(t, err, want)
	require.Empty(t, opened.SessionId)
	require.Empty(t, agent.sessions)
}
