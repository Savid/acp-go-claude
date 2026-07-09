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

func TestAvailableCommandUpdateHelperBranches(t *testing.T) {
	t.Parallel()

	left := []acp.AvailableCommand{{
		Name:        "help",
		Description: "Help",
		Input: &acp.AvailableCommandInput{
			Unstructured: &acp.UnstructuredCommandInput{Hint: "[topic]"},
		},
	}}
	require.True(t, availableCommandsEqual(left, cloneAvailableCommands(left)))
	require.False(t, availableCommandsEqual(left, nil))
	require.False(t, availableCommandsEqual(left, []acp.AvailableCommand{{Name: "other", Description: "Help"}}))
	require.False(t, availableCommandsEqual(left, []acp.AvailableCommand{{Name: "help", Description: "Other"}}))
	require.False(t, availableCommandsEqual(left, []acp.AvailableCommand{{
		Name:        "help",
		Description: "Help",
		Input: &acp.AvailableCommandInput{
			Unstructured: &acp.UnstructuredCommandInput{Hint: "[other]"},
		},
	}}))
	require.Empty(t, availableCommandHint(acp.AvailableCommand{}))
	require.Equal(t, "[topic]", availableCommandHint(left[0]))

	agent := NewAgent()
	conn := newRecordingAgentClient()
	agent.setConnection(conn)
	session := &agentSession{agent: agent, id: "session-1", availableCommands: []claude.SlashCommand{{Name: "help"}}}
	require.NoError(t, session.emitAvailableCommandsUpdate(context.Background(), false))
	require.NoError(t, session.emitAvailableCommandsUpdate(context.Background(), false))
	require.Len(t, availableCommandUpdates(conn.Updates()), 1)

	conn.sessionUpdateErr = errors.New("clear failed")
	require.ErrorContains(t, session.emitClearAvailableCommandsUpdate(context.Background()), "clear failed")
}

func TestPoisonBranches(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cancelled := make(chan struct{})
	agent := NewAgent()
	conn := newRecordingAgentClient()
	conn.sessionUpdateErr = errors.New("clear failed")
	agent.setConnection(conn)
	session := &agentSession{
		agent:              agent,
		id:                 "session-1",
		cancel:             func() { close(cancelled) },
		advertisedCommands: []acp.AvailableCommand{{Name: "help"}},
	}

	err := session.poison(ctx, "first cause")
	require.ErrorContains(t, err, "first cause")
	require.ErrorContains(t, session.poisonedError(), "first cause")
	select {
	case <-cancelled:
	default:
		t.Fatal("poison did not cancel active turn")
	}

	err = session.poison(ctx, "second cause")
	require.ErrorContains(t, err, "first cause")

	nilAgent := &agentSession{id: "session-2"}
	require.ErrorContains(t, nilAgent.poison(ctx, "nil agent cause"), "nil agent cause")
}

func TestPoisonBeforeAdvertisementEmitsNoCommandUpdate(t *testing.T) {
	t.Parallel()

	agent := NewAgent()
	conn := newRecordingAgentClient()
	agent.setConnection(conn)
	session := &agentSession{agent: agent, id: "session-1"}

	err := session.poison(context.Background(), "native reset before advertisement")
	require.ErrorContains(t, err, "native reset before advertisement")
	require.Empty(t, availableCommandUpdates(conn.Updates()))
}

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

func TestRawAndUsageUpdateHelpers(t *testing.T) {
	t.Parallel()

	agent := NewAgent()
	session := &agentSession{agent: agent, id: "session-1", rawMessages: rawMessageConfig{All: true}}
	session.emitRawClaudeMessage(context.Background(), &claude.SystemMessage{Raw: map[string]any{"type": "system"}})
	conn := newRecordingAgentClient()
	agent.setConnection(conn)
	session.emitRawClaudeMessage(context.Background(), &claude.SystemMessage{Raw: map[string]any{"type": "system"}})
	require.Len(t, conn.Extensions(), 1)
	require.Equal(t, RawEventMethod, conn.Extensions()[0].method)

	// A raw-event emit failure never aborts the turn: it returns nil and is
	// recorded on the internal observer hook.
	conn.extensionErr = errors.New("extension failed")
	session.emitRawClaudeMessage(context.Background(), &claude.SystemMessage{Raw: map[string]any{"type": "system"}})
	conn.extensionErr = nil
	// Oversized events are emitted as a marker (consuming a sequence), never dropped.
	before := len(conn.Extensions())
	huge := &claude.SystemMessage{Raw: map[string]any{"type": "system", "data": strings.Repeat("x", rawEventMaxBytes)}}
	session.emitRawClaudeMessage(context.Background(), huge)
	require.Len(t, conn.Extensions(), before+1)

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

func TestSessionUpdateEdgeBranches(t *testing.T) {
	ctx := context.Background()
	agent := NewAgent()
	session := &agentSession{agent: agent, id: "session-1", cwd: "/tmp/project", contextWindowSize: 100}

	require.NoError(t, session.emitOptionalUpdates(ctx, nil))
	info := session.sessionInfo("session-1")
	require.Equal(t, "session-1", *info.Title)
	session.rawMessages = rawMessageConfig{All: true}
	agent.closed = true
	session.emitRawClaudeMessage(ctx, &claude.SystemMessage{Raw: map[string]any{"type": "system"}})
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
