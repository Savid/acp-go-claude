package claudeacp

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/savid/acp-go-claude/internal/mapper"
	"github.com/stretchr/testify/require"
)

func TestSessionUpdateEdgeBranches(t *testing.T) {
	ctx := context.Background()
	agent := NewAgent()
	session := &agentSession{agent: agent, id: "session-1", cwd: "/tmp/project", contextWindowSize: 100}

	require.NoError(t, session.emitOptionalUpdates(ctx, nil))
	info := session.sessionInfo("session-1")
	require.Equal(t, "session-1", *info.Title)
	session.rawMessages = rawMessageConfig{All: true}
	agent.closed = true
	require.NoError(t, session.emitRawClaudeMessage(ctx, &claude.SystemMessage{Raw: map[string]any{"type": "system"}}))
	agent.closed = false

	cloned := cloneUsage(&acp.Usage{CachedWriteTokens: acp.Ptr(1), ThoughtTokens: acp.Ptr(2)})
	*cloned.CachedWriteTokens = 3
	*cloned.ThoughtTokens = 4
	require.Equal(t, 1, *cloneUsage(&acp.Usage{CachedWriteTokens: acp.Ptr(1)}).CachedWriteTokens)

	transcript := filepath.Join(t.TempDir(), "transcript.jsonl")
	require.NoError(t, os.WriteFile(transcript, []byte(`{"type":"assistant","message":{"content":"done"}}`+"\n"), 0o600))
	conn := newRecordingAgentClient()
	agent.setConnection(conn)
	require.NoError(t, session.replayTranscript(ctx, transcript))
	require.NotEmpty(t, conn.Updates())
	require.Error(t, session.replayTranscript(ctx, filepath.Join(t.TempDir(), "missing.jsonl")))

	truncatedTranscript := filepath.Join(t.TempDir(), "truncated.jsonl")
	var lines strings.Builder
	for range 10001 {
		lines.WriteString(`{"type":"user","message":{"content":"hello"}}` + "\n")
	}
	require.NoError(t, os.WriteFile(truncatedTranscript, []byte(lines.String()), 0o600))
	require.NoError(t, session.replayTranscript(ctx, truncatedTranscript))

	agent.clientCapabilities.Elicitation = nil
	require.NoError(t, session.emitElicitationComplete(ctx, &claude.SystemMessage{Raw: map[string]any{"elicitation_id": "e1"}}))
	agent.clientCapabilities.Elicitation = &acp.ElicitationCapabilities{Url: &acp.ElicitationUrlCapabilities{}}
	agent.setConnection(nil)
	require.NoError(t, session.emitElicitationComplete(ctx, &claude.SystemMessage{Raw: map[string]any{"elicitation_id": "e1"}}))
	agent.setConnection(conn)
	require.NoError(t, session.emitElicitationComplete(ctx, &claude.SystemMessage{Raw: map[string]any{}}))

	require.NoError(t, session.emitHookResponseUpdates(ctx, &claude.AssistantMessage{}, mapper.ToolUpdateOptions{}))
	require.NoError(t, session.emitHookResponseUpdates(ctx, &claude.SystemMessage{Subtype: systemSubtypeHookResponse, Raw: map[string]any{systemHookEventName: "pre_tool_use"}}, mapper.ToolUpdateOptions{}))
	require.NoError(t, session.emitHookResponseUpdates(ctx, &claude.SystemMessage{Subtype: systemSubtypeHookResponse, Raw: map[string]any{systemHookEventName: systemHookPostToolUse}}, mapper.ToolUpdateOptions{}))
	session.markHookHandled("tool-1")
	require.NoError(t, session.emitHookResponseUpdates(ctx, &claude.SystemMessage{Subtype: systemSubtypeHookResponse, Raw: map[string]any{systemHookEventName: systemHookPostToolUse, systemToolUseID: "tool-1"}}, mapper.ToolUpdateOptions{}))

	conn.sessionUpdateErr = errors.New("hook update failed")
	err := session.emitHookResponseUpdates(ctx, &claude.SystemMessage{Subtype: systemSubtypeHookResponse, Raw: map[string]any{
		systemHookEventName: systemHookPostToolUse,
		systemToolUseID:     "tool-2",
		systemToolResponse: map[string]any{
			"filePath": "/tmp/a.go",
			"structuredPatch": []any{
				map[string]any{"newStart": 1, "lines": []any{"-old", "+new"}},
			},
		},
	}}, mapper.ToolUpdateOptions{ToolUses: map[string]claude.ToolUseBlock{"tool-2": {ID: "tool-2", Name: "Edit"}}})
	require.ErrorContains(t, err, "hook update failed")
}
