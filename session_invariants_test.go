package claudeacp

import (
	"context"
	"testing"

	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/stretchr/testify/require"
)

func TestNativeSessionInvariantNoops(t *testing.T) {
	t.Parallel()

	session := &agentSession{id: "session-1"}
	require.NoError(t, session.checkNativeSessionInvariant(context.Background(), nil))
	require.NoError(t, session.checkNativeSessionInvariant(context.Background(), &claude.AssistantMessage{
		Raw: map[string]any{"session_id": "session-1"},
	}))
	require.NoError(t, session.checkNativeSessionInvariant(context.Background(), &claude.AssistantMessage{
		Raw: map[string]any{},
	}))
}
