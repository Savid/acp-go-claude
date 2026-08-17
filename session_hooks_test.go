package claudeacp

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/savid/acp-go-claude/internal/mapper"
	"github.com/stretchr/testify/require"
)

func TestHookHelpers(t *testing.T) {
	t.Parallel()

	agent := NewAgent()
	conn := newRecordingAgentClient()
	agent.setConnection(conn)
	session := &agentSession{agent: agent, id: "session-1", mode: modeDefault, model: "sonnet"}

	resp, err := session.handleHookCallback(context.Background(), claude.HookRequest{EventName: "Other"})
	require.NoError(t, err)
	require.True(t, resp.Continue)
	resp, err = session.handleHookCallback(context.Background(), claude.HookRequest{EventName: systemHookPostToolUse})
	require.NoError(t, err)
	require.True(t, resp.Continue)
	require.False(t, session.hookHandled("missing"))

	resp, err = session.handleHookCallback(context.Background(), claude.HookRequest{EventName: systemHookPostToolUse, ToolName: enterPlanModeTool, ToolUseID: "plan-1"})
	require.NoError(t, err)
	require.True(t, resp.Continue)
	require.Equal(t, modePlan, session.mode)
	require.True(t, session.hookHandled("plan-1"))

	for i := range maxHandledHooks + 1 {
		session.markHookHandled(fmt.Sprintf("tool-%d", i))
	}
	require.False(t, session.hookHandled("tool-0"))
	session.markHookHandled("duplicate")
	orderLen := len(session.handledHookOrder)
	session.markHookHandled("duplicate")
	require.Len(t, session.handledHookOrder, orderLen)

	conn.sessionUpdateErr = errors.New("update failed")
	err = session.handlePostToolUseHook(context.Background(), "plan-error", enterPlanModeTool, nil)
	require.ErrorContains(t, err, "update failed")
	conn.sessionUpdateErr = nil

	require.NoError(t, session.handlePostToolUseHook(context.Background(), "edit-1", "Read", map[string]any{}))
	require.NoError(t, session.handlePostToolUseHook(context.Background(), "edit-1", "Edit", nil))
	require.NoError(t, session.handlePostToolUseHook(context.Background(), "edit-1", "Edit", map[string]any{}))
	require.NoError(t, session.handlePostToolUseHook(context.Background(), "edit-2", "Write", map[string]any{
		"filePath": "/tmp/file.txt",
		"structuredPatch": []any{
			map[string]any{"newStart": 2, "lines": []any{"-old", "+new"}},
		},
	}))
	require.True(t, session.hookHandled("edit-2"))

	system := &claude.SystemMessage{Subtype: systemSubtypeSessionStateChanged, Raw: map[string]any{systemState: systemStateIdle}}
	require.True(t, promptFinishedBySystemIdle(system))
	require.False(t, promptFinishedBySystemIdle(&claude.SystemMessage{Subtype: "other", Raw: map[string]any{}}))
	require.Equal(t, systemStateIdle, systemString(system, systemState))
	require.Nil(t, systemMap(system, "missing"))
	require.Equal(t, "e1", elicitationIDFromSystem(&claude.SystemMessage{Raw: map[string]any{"elicitation_id": "e1"}}))
	require.NoError(t, session.emitHookResponseUpdates(context.Background(), &claude.SystemMessage{Subtype: systemSubtypeHookResponse, Raw: map[string]any{systemHookEventName: systemHookPostToolUse, systemToolUseID: "hook-1", systemToolResponse: map[string]any{"filePath": "/tmp/file.txt", "structuredPatch": []any{map[string]any{"lines": []any{"+new"}}}}}}, mapper.ToolUpdateOptions{ToolUses: map[string]claude.ToolUseBlock{"hook-1": {Name: "Edit"}}}))
}
