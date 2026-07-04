package claudeacp

import (
	"context"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/savid/acp-go-claude/internal/mapper"
	"github.com/stretchr/testify/require"
)

func TestPromptResultAndLocalCommandHelpers(t *testing.T) {
	t.Parallel()

	require.True(t, workflowTaskNotificationResultCompletesPrompt(nil))
	require.False(t, workflowTaskNotificationResultCompletesPrompt(mapper.NewWorkflowTracker()))
	require.NoError(t, promptResultError(nil, ""))
	require.NoError(t, promptResultError(&claude.ResultMessage{IsError: true, StopReason: stopReasonMaxTokens}, ""))
	require.NoError(t, promptResultError(&claude.ResultMessage{IsError: false}, ""))
	require.Error(t, promptResultError(&claude.ResultMessage{Result: "Please run /login first"}, ""))
	err := promptResultError(&claude.ResultMessage{IsError: true, Subtype: "error", Result: "failed", Errors: []string{"one"}}, "kind")
	require.Error(t, err)

	require.True(t, fatalClaudeProcessError(claude.ErrMessageStreamClosed))
	require.True(t, fatalClaudeProcessError(claude.ErrProcessExited))
	require.True(t, fatalClaudeProcessError(claude.ErrClientNotStarted))
	require.False(t, fatalClaudeProcessError(context.Canceled))
	require.True(t, localOnlySlashCommand([]acp.ContentBlock{acp.TextBlock(" /context now")}))
	require.True(t, localOnlySlashCommand([]acp.ContentBlock{acp.TextBlock("/extra-usage")}))
	require.True(t, localOnlySlashCommand([]acp.ContentBlock{acp.TextBlock("/heapdump")}))
	require.False(t, localOnlySlashCommand([]acp.ContentBlock{acp.TextBlock("/help")}))
	require.Equal(t, "", firstPromptText([]acp.ContentBlock{{}}))
	require.Equal(t, "", firstPromptToken("   "))
	require.Equal(t, "/context", firstPromptToken(" /context now"))
	agent := NewAgent()
	session, cleanup := newStartedAgentSessionForTest(t, agent, "session-1")
	defer cleanup()
	require.ErrorIs(t, session.interruptAfterEmitError(context.Background(), context.Canceled), context.Canceled)
}

func TestStreamUsageAndContextHelpers(t *testing.T) {
	t.Parallel()

	session := &agentSession{model: "sonnet", contextWindowSize: 100}
	start := &claude.StreamEventMessage{EventType: streamEventMessageStart, Event: map[string]any{
		"message": map[string]any{
			"model": "claude-sonnet-4-5-1m",
			"usage": map[string]any{"input_tokens": 1, "output_tokens": 2, "cache_read_input_tokens": 3, "cache_creation_input_tokens": 4, "reasoning_output_tokens": 5},
		},
	}}
	updates, snapshot, known, total := session.streamUsageUpdates(start, usageSnapshot{}, false, 0)
	require.True(t, known)
	require.Equal(t, 15, total)
	require.Len(t, updates, 1)
	require.Equal(t, largeContextWindow, updates[0].UsageUpdate.Size)
	require.Equal(t, "claude-sonnet-4-5-1m", streamModel(start))

	updates, snapshot, known, total = session.streamUsageUpdates(&claude.StreamEventMessage{
		EventType: streamEventMessageDelta,
		Event:     map[string]any{"usage": map[string]any{"output_tokens": 7, "cache_read_input_tokens": nil}},
	}, snapshot, known, total)
	require.True(t, known)
	require.Equal(t, 17, total)
	require.Len(t, updates, 1)
	require.Equal(t, 0, snapshot.cacheReadTokens)

	updates, _, _, total = session.streamUsageUpdates(&claude.StreamEventMessage{EventType: "other"}, snapshot, known, total)
	require.Nil(t, updates)
	require.Equal(t, 17, total)
	updates, snapshot, known, total = session.streamUsageUpdates(&claude.StreamEventMessage{
		EventType: streamEventMessageDelta,
		Event:     map[string]any{"usage": map[string]any{"output_tokens": 7}},
	}, snapshot, known, total)
	require.Nil(t, updates)
	require.True(t, known)
	require.Equal(t, 17, total)
	_, ok := streamUsageSnapshot(&claude.StreamEventMessage{EventType: streamEventMessageStart, Event: map[string]any{"message": map[string]any{}}}, usageSnapshot{}, false)
	require.False(t, ok)
	_, ok = streamUsageSnapshot(&claude.StreamEventMessage{EventType: streamEventMessageDelta}, usageSnapshot{}, false)
	require.False(t, ok)
	require.Equal(t, "", streamModel(nil))
	require.Equal(t, "", streamModel(&claude.StreamEventMessage{EventType: streamEventMessageDelta}))

	meta := streamUsageMeta(snapshot)
	require.NotEmpty(t, meta[claudeMetaKey])
	require.Equal(t, 17, snapshot.total())
	require.Equal(t, usageSnapshot{inputTokens: 3}, usageSnapshotFromMap(map[string]any{"input_tokens": 3}))
	require.Equal(t, usageSnapshot{inputTokens: 9}, (usageSnapshot{}).patch(map[string]any{"input_tokens": int64(9)}))

	contextUpdates := session.contextUsageUpdates(&claude.ContextUsage{TotalTokens: 4, MaxTokens: 200})
	require.Len(t, contextUpdates, 1)
	require.Equal(t, 200, session.currentContextWindow())
	require.Nil(t, session.contextUsageUpdates(nil))
	require.Nil(t, session.contextUsageUpdates(&claude.ContextUsage{}))
	require.Equal(t, largeContextWindow, session.liveContextWindow("opus-1m"))
	require.Equal(t, 200, session.liveContextWindow("sonnet"))
	require.Equal(t, largeContextWindow, contextWindowForModel("claude-sonnet-1m"))
	require.Equal(t, defaultContextWindow, contextWindowForModel("claude-sonnet"))
	require.True(t, modelHasLargeContext("claude-sonnet-4-5-1m"))
	require.False(t, modelHasLargeContext("claude-sonnet"))
	require.Equal(t, map[string]any{"a": "b"}, mapValue(map[string]any{"a": "b"}))
	require.Nil(t, mapValue("bad"))
	require.Equal(t, "x", stringValue("x"))
	require.Equal(t, "", stringValue(1))
	require.Equal(t, 7, intValue(7))
	require.Equal(t, 8, intValue(int64(8)))
	require.Equal(t, 9, intValue(float64(9)))
	require.Equal(t, 0, intValue("9"))
	value, present := intField(map[string]any{"x": nil}, "x")
	require.True(t, present)
	require.Zero(t, value)
	_, present = intField(nil, "x")
	require.False(t, present)
}

func TestPromptMirrorAndObservationBranches(t *testing.T) {
	ctx := context.Background()
	agent := NewAgent()
	session, cleanup := newStartedAgentSessionForTest(t, agent, "session-1")
	defer cleanup()
	session.mirror = newSessionMirror(agent.log, nil, t.TempDir())
	handled, err := session.handleSessionMirror(ctx, &claude.AssistantMessage{})
	require.NoError(t, err)
	require.False(t, handled)
	handled, err = session.handleSessionMirror(ctx, &claude.TranscriptMirrorMessage{})
	require.NoError(t, err)
	require.True(t, handled)

	state := &promptLoopState{}
	require.NoError(t, session.observePromptMessage(ctx, &claude.StreamEventMessage{ParentToolUseID: "parent"}, state))
	observeAssistantMessage(&claude.AssistantMessage{ParentToolUseID: "parent", ErrorKind: "ignored", Model: "ignored"}, state)
	require.Empty(t, state.lastAssistantModel)
	require.NoError(t, session.observePromptMessage(ctx, &claude.AssistantMessage{ErrorKind: "kind", Model: "<synthetic>"}, state))
	require.Equal(t, "kind", state.lastAssistantErrorKind)

	session.recordWorkflowFrameErrors(ctx, nil)
	(&agentSession{}).recordWorkflowFrameErrors(ctx, mapper.NewWorkflowTracker())

	transport := newFakeClaudeTransport()
	client := claude.NewClient(nil, claude.Options{}, transport)
	require.NoError(t, client.Start(ctx))
	defer func() { _ = client.Close() }()
	drainSession := &agentSession{agent: agent, id: "drain", client: client, mirror: newSessionMirror(agent.log, nil, t.TempDir())}
	transport.messages <- map[string]any{"type": "transcript_mirror", "filePath": "/tmp/outside.jsonl", "entries": []any{map[string]any{"type": "user"}}}
	require.NoError(t, drainSession.drainSessionMirror(ctx, mapper.ToolUpdateOptions{Workflow: mapper.NewWorkflowTracker()}))
}

func TestResultUsageHelpers(t *testing.T) {
	t.Parallel()

	session := &agentSession{model: "claude-sonnet", contextWindowSize: 100}
	require.Nil(t, session.resultUsageUpdates(nil, nil, ""))
	require.Nil(t, session.resultUsageUpdates(&claude.ResultMessage{}, nil, ""))

	cost := 0.02
	updates := session.resultUsageUpdates(&claude.ResultMessage{
		TotalCostUSD:     &cost,
		Origin:           map[string]any{"kind": originKindTaskNotification},
		StructuredOutput: map[string]any{"ok": true},
		Usage:            &claude.Usage{InputTokens: 1, OutputTokens: 2, CachedInputTokens: 3, CacheCreationInputTokens: 4, ReasoningOutputTokens: 5},
		ModelUsage: map[string]claude.ModelUsage{
			"claude-sonnet-long": {ContextWindow: 123},
		},
	}, nil, "claude-sonnet")
	require.Len(t, updates, 1)
	require.Equal(t, 15, updates[0].UsageUpdate.Used)
	require.Equal(t, 123, updates[0].UsageUpdate.Size)
	claudeMeta, ok := updates[0].UsageUpdate.Meta[claudeMetaKey].(map[string]any)
	require.True(t, ok)
	require.Equal(t, map[string]any{"ok": true}, claudeMeta[structuredOutputMetaKey])
	origin, ok := claudeMeta[rawMessageOriginKey].(map[string]any)
	require.True(t, ok)
	require.Equal(t, originKindTaskNotification, origin["kind"])

	updates = session.resultUsageUpdates(&claude.ResultMessage{Usage: &claude.Usage{InputTokens: 1}}, &claude.ContextUsage{TotalTokens: 8, MaxTokens: 456}, "claude-sonnet")
	require.Equal(t, 8, updates[0].UsageUpdate.Used)
	require.Equal(t, 456, updates[0].UsageUpdate.Size)

	usage, ok := matchingModelUsage(map[string]claude.ModelUsage{"abc": {InputTokens: 1}}, "")
	require.False(t, ok)
	require.Zero(t, usage)
	usage, ok = matchingModelUsage(map[string]claude.ModelUsage{"abc": {InputTokens: 1}}, "abc")
	require.True(t, ok)
	require.Equal(t, 1, usage.InputTokens)
	usage, ok = matchingModelUsage(map[string]claude.ModelUsage{"abcdef": {InputTokens: 2}}, "abcxyz")
	require.True(t, ok)
	require.Equal(t, 2, usage.InputTokens)
	require.Equal(t, 2, commonPrefixLength("abc", "abd"))
}
