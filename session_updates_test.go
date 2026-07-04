package claudeacp

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/stretchr/testify/require"
)

func TestSessionUpdateEmitAndInfoHelpers(t *testing.T) {
	t.Parallel()

	agent := NewAgent()
	session := &agentSession{agent: agent, id: "session-1", cwd: "/tmp/project", additionalDirectories: []string{"/tmp/extra"}}
	require.NoError(t, session.emitUpdates(context.Background(), nil))
	require.ErrorIs(t, session.emitUpdates(context.Background(), []acp.SessionUpdate{acp.UpdateAgentMessageText("x")}), errACPConnectionNotAttached)
	require.NoError(t, session.emitOptionalUpdates(context.Background(), []acp.SessionUpdate{acp.UpdateAgentMessageText("x")}))

	conn := newRecordingAgentClient()
	agent.setConnection(conn)
	require.NoError(t, session.emitUpdates(context.Background(), []acp.SessionUpdate{acp.UpdateAgentMessageText("x")}))
	require.Len(t, conn.Updates(), 1)

	conn.sessionUpdateErr = errors.New("update failed")
	require.ErrorContains(t, session.emitOptionalUpdates(context.Background(), []acp.SessionUpdate{acp.UpdateAgentMessageText("x")}), "update failed")
	conn.sessionUpdateErr = nil

	require.NoError(t, session.emitLiveSessionInfoUpdate(context.Background(), []acp.ContentBlock{acp.TextBlock("  hello   world  ")}))
	info := session.sessionInfo(session.id)
	require.Equal(t, "hello world", *info.Title)
	require.Equal(t, "/tmp/project", info.Cwd)
	info.AdditionalDirectories[0] = "changed"
	require.Equal(t, "/tmp/extra", session.additionalDirectories[0])
	require.Equal(t, "hello world", liveSessionTitleFromPrompt([]acp.ContentBlock{acp.TextBlock("hello world")}))
	require.Equal(t, "", liveSessionTitleFromPrompt([]acp.ContentBlock{{}}))
	require.Equal(t, "", normalizeLiveSessionTitle("   "))
	require.True(t, strings.HasSuffix(normalizeLiveSessionTitle(strings.Repeat("x", liveSessionTitleMaxRunes+10)), "..."))

	agent.closed = true
	require.ErrorIs(t, session.emitUpdates(context.Background(), []acp.SessionUpdate{acp.UpdateAgentMessageText("x")}), errAgentClosed)
	require.NoError(t, session.emitOptionalUpdates(context.Background(), []acp.SessionUpdate{acp.UpdateAgentMessageText("x")}))
}

func TestRawAndUsageUpdateHelpers(t *testing.T) {
	t.Parallel()

	agent := NewAgent()
	session := &agentSession{agent: agent, id: "session-1", rawMessages: rawMessageConfig{All: true}}
	require.NoError(t, session.emitRawClaudeMessage(context.Background(), &claude.SystemMessage{Raw: map[string]any{"type": "system"}}))
	conn := newRecordingAgentClient()
	agent.setConnection(conn)
	require.NoError(t, session.emitRawClaudeMessage(context.Background(), &claude.SystemMessage{Raw: map[string]any{"type": "system"}}))
	require.Len(t, conn.Extensions(), 1)
	require.Equal(t, RawEventMethod, conn.Extensions()[0].method)

	conn.extensionErr = errors.New("extension failed")
	require.ErrorContains(t, session.emitRawClaudeMessage(context.Background(), &claude.SystemMessage{Raw: map[string]any{"type": "system"}}), "extension failed")
	conn.extensionErr = nil
	huge := &claude.SystemMessage{Raw: map[string]any{"type": "system", "data": strings.Repeat("x", rawEventMaxBytes)}}
	require.NoError(t, session.emitRawClaudeMessage(context.Background(), huge))

	require.Equal(t, "", resultOriginKind(nil))
	require.Equal(t, originKindTaskNotification, resultOriginKind(&claude.ResultMessage{Origin: map[string]any{"kind": originKindTaskNotification}}))

	require.Nil(t, mergeUsage(nil, nil))
	left := &acp.Usage{InputTokens: 1, OutputTokens: 2, TotalTokens: 3, CachedReadTokens: acp.Ptr(4)}
	right := &acp.Usage{InputTokens: 10, OutputTokens: 20, TotalTokens: 30, CachedReadTokens: acp.Ptr(5), CachedWriteTokens: acp.Ptr(5), ThoughtTokens: acp.Ptr(6)}
	merged := mergeUsage(left, right)
	require.Equal(t, 11, merged.InputTokens)
	require.Equal(t, 9, *merged.CachedReadTokens)
	require.Equal(t, 5, *merged.CachedWriteTokens)
	require.Equal(t, 6, *merged.ThoughtTokens)
	cloned := cloneUsage(left)
	cloned.InputTokens = 99
	*cloned.CachedReadTokens = 99
	require.Equal(t, 1, left.InputTokens)
	require.Equal(t, 4, *left.CachedReadTokens)
	require.Nil(t, cloneUsage(nil))
	require.Nil(t, optionalIntSum(nil, nil))
	require.Equal(t, 4, *optionalIntSum(acp.Ptr(4), nil))
	require.Equal(t, 5, *optionalIntSum(nil, acp.Ptr(5)))
}

func TestSystemMessageSideEffects(t *testing.T) {
	t.Parallel()

	agent := NewAgent()
	conn := newRecordingAgentClient()
	agent.setConnection(conn)
	agent.clientCapabilities.Elicitation = &acp.ElicitationCapabilities{Url: &acp.ElicitationUrlCapabilities{}}
	session := &agentSession{agent: agent, id: "session-1", contextWindowSize: 100}

	require.NoError(t, session.emitMessageSideEffects(context.Background(), &claude.SystemMessage{Subtype: systemStatus, Raw: map[string]any{systemStatus: systemStatusCompacting}}))
	require.NoError(t, session.emitMessageSideEffects(context.Background(), &claude.SystemMessage{Subtype: systemStatus, Raw: map[string]any{systemStatus: "idle"}}))
	require.NoError(t, session.emitMessageSideEffects(context.Background(), &claude.SystemMessage{Subtype: systemSubtypeCompactBoundary, Raw: map[string]any{}}))
	require.NoError(t, session.emitMessageSideEffects(context.Background(), &claude.SystemMessage{Subtype: systemSubtypeLocalCommandOutput, Raw: map[string]any{systemContent: "output"}}))
	require.NoError(t, session.emitMessageSideEffects(context.Background(), &claude.SystemMessage{Subtype: systemSubtypeLocalCommandOutput, Raw: map[string]any{}}))
	require.NoError(t, session.emitMessageSideEffects(context.Background(), &claude.SystemMessage{Subtype: elicitationComplete, Raw: map[string]any{"elicitation_id": "e1"}}))
	require.Len(t, conn.Completions(), 1)
	require.NoError(t, session.emitMessageSideEffects(context.Background(), &claude.AssistantMessage{}))
	require.NoError(t, session.emitMessageSideEffects(context.Background(), &claude.SystemMessage{Subtype: "other", Raw: map[string]any{}}))
}
