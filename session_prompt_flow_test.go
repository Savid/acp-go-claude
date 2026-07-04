package claudeacp

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/savid/acp-go-claude/internal/mapper"
	"github.com/stretchr/testify/require"
)

func TestSessionPromptFlowEdgeBranches(t *testing.T) {
	ctx := context.Background()

	t.Run("query send error", func(t *testing.T) {
		session, transport, cleanup := newPromptFlowSession(t)
		defer cleanup()
		transport.sendErr = errors.New("query failed")
		_, err := session.Prompt(ctx, TextPromptRequest(session.id, "hello"))
		require.ErrorContains(t, err, "query failed")
	})

	t.Run("acquire turn error", func(t *testing.T) {
		session, _, cleanup := newPromptFlowSession(t)
		defer cleanup()
		session.turn <- struct{}{}
		_, err := session.Prompt(ctx, TextPromptRequest(session.id, "hello"))
		require.Error(t, err)
	})

	t.Run("prompt mapping error", func(t *testing.T) {
		session, _, cleanup := newPromptFlowSession(t)
		defer cleanup()
		_, err := session.Prompt(ctx, acp.PromptRequest{SessionId: session.id, Prompt: []acp.ContentBlock{acp.AudioBlock("abc", "audio/wav")}})
		require.Error(t, err)
	})

	t.Run("receive after turn cancellation", func(t *testing.T) {
		session, transport, cleanup := newPromptFlowSession(t)
		defer cleanup()
		promptCtx, cancel := context.WithCancel(ctx)
		transport.queryMsgs = nil
		transport.onQuery = cancel
		resp, err := session.Prompt(promptCtx, TextPromptRequest(session.id, "hello"))
		require.NoError(t, err)
		require.Equal(t, acp.StopReasonCancelled, resp.StopReason)
	})

	t.Run("raw emit error interrupts", func(t *testing.T) {
		session, _, cleanup := newPromptFlowSession(t)
		defer cleanup()
		session.rawMessages = rawMessageConfig{All: true}
		conn, ok := session.agent.connection().(*recordingAgentClient)
		require.True(t, ok)
		conn.extensionErr = errors.New("raw failed")
		_, err := session.Prompt(ctx, TextPromptRequest(session.id, "hello"))
		require.ErrorContains(t, err, "raw failed")
	})

	t.Run("empty mirror frame is handled", func(t *testing.T) {
		session, transport, cleanup := newPromptFlowSession(t)
		defer cleanup()
		transport.queryMsgs = []map[string]any{
			{"type": "transcript_mirror", "filePath": "/tmp/ignored.jsonl"},
			{"type": "result", "subtype": "success", "is_error": false, "stop_reason": "end_turn"},
		}
		resp, err := session.Prompt(ctx, TextPromptRequest(session.id, "hello"))
		require.NoError(t, err)
		require.Equal(t, acp.StopReasonEndTurn, resp.StopReason)
	})

	t.Run("mirror append error interrupts", func(t *testing.T) {
		session, transport, cleanup := newPromptFlowSession(t)
		defer cleanup()
		home := t.TempDir()
		projects := filepath.Join(home, "projects")
		session.mirror = &sessionMirror{
			log:         session.agent.log,
			store:       &faultSessionStore{SessionStore: NewInMemorySessionStore(), appendErr: errors.New("append failed")},
			projectsDir: projects,
		}
		transport.queryMsgs = []map[string]any{{
			"type":     "transcript_mirror",
			"filePath": filepath.Join(projects, "project", "11111111-1111-4111-8111-111111111111.jsonl"),
			"entries":  []any{map[string]any{"type": "user"}},
		}}
		_, err := session.Prompt(ctx, TextPromptRequest(session.id, "hello"))
		require.ErrorIs(t, err, errSessionMirrorAppend)
	})

	t.Run("stream usage update error interrupts", func(t *testing.T) {
		session, _, cleanup := newPromptFlowSession(t)
		defer cleanup()
		conn, ok := session.agent.connection().(*recordingAgentClient)
		require.True(t, ok)
		conn.sessionUpdateErr = errors.New("usage failed")
		_, err := session.Prompt(ctx, TextPromptRequest(session.id, "hello"))
		require.ErrorContains(t, err, "usage failed")
	})

	t.Run("message side effect error interrupts", func(t *testing.T) {
		session, transport, cleanup := newPromptFlowSession(t)
		defer cleanup()
		conn, ok := session.agent.connection().(*recordingAgentClient)
		require.True(t, ok)
		conn.sessionUpdateErr = errors.New("compact failed")
		transport.queryMsgs = []map[string]any{{
			"type":    "system",
			"subtype": systemStatus,
			"status":  systemStatusCompacting,
		}}
		_, err := session.Prompt(ctx, TextPromptRequest(session.id, "hello"))
		require.ErrorContains(t, err, "compact failed")
	})

	t.Run("mapped update error interrupts", func(t *testing.T) {
		session, transport, cleanup := newPromptFlowSession(t)
		defer cleanup()
		conn, ok := session.agent.connection().(*recordingAgentClient)
		require.True(t, ok)
		conn.sessionUpdateErr = errors.New("mapped update failed")
		transport.queryMsgs = []map[string]any{{
			"type": "assistant",
			"message": map[string]any{"content": []any{
				map[string]any{"type": "text", "text": "mapped"},
			}},
		}}
		_, err := session.Prompt(ctx, TextPromptRequest(session.id, "hello"))
		require.ErrorContains(t, err, "mapped update failed")
	})

	t.Run("hook update error interrupts", func(t *testing.T) {
		session, transport, cleanup := newPromptFlowSession(t)
		defer cleanup()
		conn, ok := session.agent.connection().(*recordingAgentClient)
		require.True(t, ok)
		conn.sessionUpdateErr = errors.New("hook failed")
		conn.failUpdateAfter = 2
		transport.queryMsgs = []map[string]any{
			{
				"type": "assistant",
				"message": map[string]any{"content": []any{
					map[string]any{"type": "tool_use", "id": "tool-1", "name": "Edit", "input": map[string]any{"file_path": "/tmp/a.go"}},
				}},
			},
			{
				"type":              "system",
				"subtype":           systemSubtypeHookResponse,
				systemHookEventName: systemHookPostToolUse,
				systemToolUseID:     "tool-1",
				systemToolResponse: map[string]any{
					"filePath": "/tmp/a.go",
					"structuredPatch": []any{
						map[string]any{"newStart": 1, "lines": []any{"-old", "+new"}},
					},
				},
			},
		}
		_, err := session.Prompt(ctx, TextPromptRequest(session.id, "hello"))
		require.ErrorContains(t, err, "hook failed")
	})

	t.Run("finish result loop controls", func(t *testing.T) {
		session, transport, cleanup := newPromptFlowSession(t)
		defer cleanup()
		transport.queryMsgs = []map[string]any{
			{"type": "result", "subtype": "success", "is_error": false, "stop_reason": "end_turn"},
			{"type": "result", "subtype": "success", "is_error": false, "stop_reason": "end_turn"},
		}
		previousFinish := finishPromptResultCall
		calls := 0
		finishPromptResultCall = func(
			s *agentSession,
			turnCtx context.Context,
			interruptCtx context.Context,
			params acp.PromptRequest,
			result *claude.ResultMessage,
			state *promptLoopState,
			toolUpdateOptions mapper.ToolUpdateOptions,
			localOnlyCommand bool,
		) (acp.PromptResponse, bool, error) {
			calls++
			if calls == 1 {
				return acp.PromptResponse{}, false, nil
			}

			return previousFinish(s, turnCtx, interruptCtx, params, result, state, toolUpdateOptions, localOnlyCommand)
		}
		t.Cleanup(func() { finishPromptResultCall = previousFinish })
		resp, err := session.Prompt(ctx, TextPromptRequest(session.id, "hello"))
		require.NoError(t, err)
		require.Equal(t, acp.StopReasonEndTurn, resp.StopReason)
		require.Equal(t, 2, calls)
		finishPromptResultCall = previousFinish

		errorSession, errorTransport, errorCleanup := newPromptFlowSession(t)
		defer errorCleanup()
		errorTransport.queryMsgs = []map[string]any{{"type": "result", "subtype": "success", "is_error": false}}
		finishPromptResultCall = func(
			*agentSession,
			context.Context,
			context.Context,
			acp.PromptRequest,
			*claude.ResultMessage,
			*promptLoopState,
			mapper.ToolUpdateOptions,
			bool,
		) (acp.PromptResponse, bool, error) {
			return acp.PromptResponse{}, false, errors.New("finish failed")
		}
		_, err = errorSession.Prompt(ctx, TextPromptRequest(errorSession.id, "hello"))
		require.ErrorContains(t, err, "finish failed")
		finishPromptResultCall = previousFinish
	})

	t.Run("system idle completion", func(t *testing.T) {
		session, transport, cleanup := newPromptFlowSession(t)
		defer cleanup()
		transport.queryMsgs = []map[string]any{{
			"type":      "system",
			"subtype":   systemSubtypeSessionStateChanged,
			systemState: systemStateIdle,
		}}
		resp, err := session.Prompt(ctx, TextPromptRequest(session.id, "hello"))
		require.NoError(t, err)
		require.Equal(t, acp.StopReasonEndTurn, resp.StopReason)
	})

	t.Run("system idle canceled completion", func(t *testing.T) {
		session, transport, cleanup := newPromptFlowSession(t)
		defer cleanup()
		transport.queryMsgs = []map[string]any{{
			"type":      "system",
			"subtype":   systemSubtypeSessionStateChanged,
			systemState: systemStateIdle,
		}}
		transport.onQuery = func() {
			session.mu.Lock()
			session.turnCancelled = true
			session.mu.Unlock()
		}
		resp, err := session.Prompt(ctx, TextPromptRequest(session.id, "hello"))
		require.NoError(t, err)
		require.Equal(t, acp.StopReasonCancelled, resp.StopReason)
	})

	t.Run("system idle live info error", func(t *testing.T) {
		session, transport, cleanup := newPromptFlowSession(t)
		defer cleanup()
		conn, ok := session.agent.connection().(*recordingAgentClient)
		require.True(t, ok)
		conn.sessionUpdateErr = errors.New("idle update failed")
		transport.queryMsgs = []map[string]any{{
			"type":      "system",
			"subtype":   systemSubtypeSessionStateChanged,
			systemState: systemStateIdle,
		}}
		_, err := session.Prompt(ctx, TextPromptRequest(session.id, "hello"))
		require.ErrorContains(t, err, "idle update failed")
	})

	t.Run("system idle drain error", func(t *testing.T) {
		session, transport, cleanup := newPromptFlowSession(t)
		defer cleanup()
		home := t.TempDir()
		projects := filepath.Join(home, "projects")
		session.mirror = &sessionMirror{
			log:         session.agent.log,
			store:       &faultSessionStore{SessionStore: NewInMemorySessionStore(), appendErr: errors.New("drain append failed")},
			projectsDir: projects,
		}
		transport.queryMsgs = []map[string]any{
			{
				"type":      "system",
				"subtype":   systemSubtypeSessionStateChanged,
				systemState: systemStateIdle,
			},
			{
				"type":     "transcript_mirror",
				"filePath": filepath.Join(projects, "project", "11111111-1111-4111-8111-111111111111.jsonl"),
				"entries":  []any{map[string]any{"type": "user"}},
			},
		}
		_, err := session.Prompt(ctx, TextPromptRequest(session.id, "hello"))
		require.ErrorIs(t, err, errSessionMirrorAppend)
	})

	t.Run("local command emits result text", func(t *testing.T) {
		session, transport, cleanup := newPromptFlowSession(t)
		defer cleanup()
		transport.queryMsgs = []map[string]any{{
			"type":        "result",
			"subtype":     "success",
			"is_error":    false,
			"stop_reason": "end_turn",
			"result":      "context text",
			"usage":       map[string]any{"input_tokens": 1},
		}}
		resp, err := session.Prompt(ctx, TextPromptRequest(session.id, "/context"))
		require.NoError(t, err)
		require.Equal(t, acp.StopReasonEndTurn, resp.StopReason)
		conn, ok := session.agent.connection().(*recordingAgentClient)
		require.True(t, ok)
		require.NotEmpty(t, conn.Updates())
	})
}

func TestFinishPromptResultAndDrainEdges(t *testing.T) {
	ctx := context.Background()
	session, transport, cleanup := newPromptFlowSession(t)
	defer cleanup()

	state := &promptLoopState{}
	tracker := mapper.NewWorkflowTracker()
	updates := mapper.MessageToUpdatesWithOptions(&claude.SystemMessage{
		Subtype: "task_started",
		Raw: map[string]any{
			"task_id":     "task-1",
			"tool_use_id": "workflow-1",
			"summary":     "running",
		},
	}, mapper.ToolUpdateOptions{Workflow: tracker})
	require.NotNil(t, updates)

	resp, done, err := session.finishPromptResult(ctx, ctx, TextPromptRequest(session.id, "hello"), &claude.ResultMessage{
		Origin: map[string]any{"kind": originKindTaskNotification},
		Usage:  &claude.Usage{InputTokens: 1},
	}, state, mapper.ToolUpdateOptions{Workflow: tracker}, false)
	require.NoError(t, err)
	require.False(t, done)
	require.Empty(t, resp)

	session.logUnknownStopReason(ctx, &claude.ResultMessage{StopReason: "future"})

	transport.controlErr = map[string]error{"get_context_usage": errors.New("usage failed")}
	_, done, err = session.finishPromptResult(ctx, ctx, TextPromptRequest(session.id, "hello"), &claude.ResultMessage{
		Usage: &claude.Usage{InputTokens: 1},
	}, &promptLoopState{}, mapper.ToolUpdateOptions{}, false)
	require.NoError(t, err)
	require.True(t, done)
	transport.controlErr = nil

	conn, ok := session.agent.connection().(*recordingAgentClient)
	require.True(t, ok)
	conn.sessionUpdateErr = errors.New("result usage failed")
	_, _, err = session.finishPromptResult(ctx, ctx, TextPromptRequest(session.id, "hello"), &claude.ResultMessage{
		Usage: &claude.Usage{InputTokens: 1},
	}, &promptLoopState{}, mapper.ToolUpdateOptions{}, false)
	require.ErrorContains(t, err, "result usage failed")
	conn.sessionUpdateErr = nil

	_, _, err = session.finishPromptResult(ctx, ctx, TextPromptRequest(session.id, "hello"), &claude.ResultMessage{
		IsError: true,
		Subtype: "error",
		Result:  "failed",
	}, &promptLoopState{}, mapper.ToolUpdateOptions{}, false)
	require.Error(t, err)

	transport.context = map[string]any{}
	conn.sessionUpdateErr = errors.New("local result failed")
	_, _, err = session.finishPromptResult(ctx, ctx, TextPromptRequest(session.id, "/context"), &claude.ResultMessage{
		Result: "local text",
	}, &promptLoopState{}, mapper.ToolUpdateOptions{}, true)
	require.ErrorContains(t, err, "local result failed")
	conn.sessionUpdateErr = nil

	transport.context = map[string]any{}
	conn.sessionUpdateErr = errors.New("live info failed")
	_, _, err = session.finishPromptResult(ctx, ctx, TextPromptRequest(session.id, "hello"), &claude.ResultMessage{}, &promptLoopState{}, mapper.ToolUpdateOptions{}, false)
	require.ErrorContains(t, err, "live info failed")
	conn.sessionUpdateErr = nil

	home := t.TempDir()
	projects := filepath.Join(home, "projects")
	session.mirror = &sessionMirror{
		log:         session.agent.log,
		store:       &faultSessionStore{SessionStore: NewInMemorySessionStore(), appendErr: errors.New("finish drain failed")},
		projectsDir: projects,
	}
	transport.messages <- map[string]any{
		"type":     "transcript_mirror",
		"filePath": filepath.Join(projects, "project", "11111111-1111-4111-8111-111111111111.jsonl"),
		"entries":  []any{map[string]any{"type": "user"}},
	}
	_, _, err = session.finishPromptResult(ctx, ctx, TextPromptRequest(session.id, "hello"), &claude.ResultMessage{}, &promptLoopState{}, mapper.ToolUpdateOptions{}, false)
	require.ErrorIs(t, err, errSessionMirrorAppend)

	transport.messages <- map[string]any{"type": "assistant", "message": map[string]any{"content": []any{
		map[string]any{"type": "text", "text": "drained"},
	}}}
	conn.sessionUpdateErr = errors.New("drain failed")
	err = session.drainSessionMirror(ctx, mapper.ToolUpdateOptions{Workflow: mapper.NewWorkflowTracker()})
	require.ErrorContains(t, err, "drain failed")

	transport.errs <- errors.New("receive failed")
	err = session.drainSessionMirror(ctx)
	require.Error(t, err)

	timeoutSession, _, timeoutCleanup := newPromptFlowSession(t)
	defer timeoutCleanup()
	previousDrain := sessionMirrorDrainTimeout
	sessionMirrorDrainTimeout = time.Nanosecond
	t.Cleanup(func() { sessionMirrorDrainTimeout = previousDrain })
	require.NoError(t, timeoutSession.drainSessionMirror(ctx))

	sessionMirrorDrainTimeout = previousDrain
	rawDrain, rawTransport, rawCleanup := newPromptFlowSession(t)
	defer rawCleanup()
	rawDrain.rawMessages = rawMessageConfig{All: true}
	rawConn, ok := rawDrain.agent.connection().(*recordingAgentClient)
	require.True(t, ok)
	rawConn.extensionErr = errors.New("raw drain failed")
	rawTransport.messages <- map[string]any{"type": "assistant", "message": map[string]any{"content": []any{
		map[string]any{"type": "text", "text": "raw"},
	}}}
	err = rawDrain.drainSessionMirror(ctx)
	require.ErrorContains(t, err, "raw drain failed")

	noWorkflow, noWorkflowTransport, noWorkflowCleanup := newPromptFlowSession(t)
	defer noWorkflowCleanup()
	sessionMirrorDrainTimeout = previousDrain
	noWorkflowTransport.messages <- map[string]any{"type": "assistant", "message": map[string]any{"content": []any{
		map[string]any{"type": "text", "text": "ignored"},
	}}}
	require.NoError(t, noWorkflow.drainSessionMirror(ctx))

	poisonMirror, _, poisonCleanup := newPromptFlowSession(t)
	defer poisonCleanup()
	poisonMirror.mu.Lock()
	poisonMirror.poisonCause = "mirror poisoned"
	poisonMirror.mu.Unlock()
	handled, err := poisonMirror.handleSessionMirror(ctx, &claude.AssistantMessage{})
	require.False(t, handled)
	require.ErrorContains(t, err, "mirror poisoned")

	resetDrain, resetTransport, resetCleanup := newPromptFlowSession(t)
	defer resetCleanup()
	resetTransport.messages <- map[string]any{"type": "conversation_reset"}
	err = resetDrain.drainSessionMirror(ctx)
	require.ErrorContains(t, err, "conversation_reset")
}

func TestFinishPromptResultEmitErrorBranches(t *testing.T) {
	ctx := context.Background()

	localSession, localTransport, localCleanup := newPromptFlowSession(t)
	defer localCleanup()
	localTransport.context = map[string]any{}
	localSession.agent.setConnection(newFailingSessionUpdateClient(errors.New("local result failed")))
	_, _, err := localSession.finishPromptResult(ctx, ctx, TextPromptRequest(localSession.id, "/context"), &claude.ResultMessage{
		Result: "local text",
	}, &promptLoopState{}, mapper.ToolUpdateOptions{}, true)
	require.ErrorContains(t, err, "local result failed")

	liveSession, liveTransport, liveCleanup := newPromptFlowSession(t)
	defer liveCleanup()
	liveTransport.context = map[string]any{}
	liveSession.agent.setConnection(newFailingSessionUpdateClient(errors.New("live info failed")))
	_, _, err = liveSession.finishPromptResult(ctx, ctx, TextPromptRequest(liveSession.id, "hello"), &claude.ResultMessage{}, &promptLoopState{}, mapper.ToolUpdateOptions{}, false)
	require.ErrorContains(t, err, "live info failed")
}

type failingSessionUpdateClient struct {
	*recordingAgentClient
	err error
}

func newFailingSessionUpdateClient(err error) *failingSessionUpdateClient {
	return &failingSessionUpdateClient{recordingAgentClient: newRecordingAgentClient(), err: err}
}

func (c *failingSessionUpdateClient) SessionUpdate(context.Context, acp.SessionNotification) error {
	return c.err
}

func TestPromptHelperRemainingBranches(t *testing.T) {
	ctx := context.Background()
	session, _, cleanup := newPromptFlowSession(t)
	defer cleanup()

	require.NoError(t, session.observePromptMessage(ctx, &claude.StreamEventMessage{
		EventType: streamEventMessageStart,
		Event: map[string]any{"message": map[string]any{
			"model": "claude-sonnet-1m",
			"usage": map[string]any{"input_tokens": 1},
		}},
	}, &promptLoopState{}))
	require.Equal(t, largeContextWindow, session.currentContextWindow())

	tracker := mapper.NewWorkflowTracker()
	_ = mapper.MessageToUpdatesWithOptions(&claude.SystemMessage{Subtype: "task_progress", Raw: map[string]any{}}, mapper.ToolUpdateOptions{Workflow: tracker})
	session.recordWorkflowFrameErrors(ctx, tracker)

	snapshot, ok := streamUsageSnapshot(&claude.StreamEventMessage{
		EventType: streamEventMessageDelta,
		Event:     map[string]any{"usage": map[string]any{"cache_creation_input_tokens": 2, "reasoning_output_tokens": 3}},
	}, usageSnapshot{}, false)
	require.True(t, ok)
	require.Equal(t, 5, snapshot.total())
	require.Equal(t, usageSnapshot{cacheCreationTokens: 4, reasoningOutputToken: 5}, (usageSnapshot{}).patch(map[string]any{
		"cache_creation_input_tokens": 4,
		"reasoning_output_tokens":     5,
	}))

	transport := newFakeClaudeTransport()
	transport.controlErr = map[string]error{"get_context_usage": errors.New("usage failed")}
	client := claude.NewClient(nil, claude.Options{}, transport)
	require.NoError(t, client.Start(ctx))
	usageErrSession := &agentSession{agent: session.agent, id: "usage", client: client}
	usageErrSession.emitCurrentUsageUpdate(ctx)
	require.NoError(t, client.Close())

	defaultWindow := (&agentSession{model: "sonnet"}).currentContextWindow()
	require.Equal(t, defaultContextWindow, defaultWindow)
}

func newPromptFlowSession(t *testing.T) (*agentSession, *fakeClaudeTransport, func()) {
	t.Helper()

	transport := newFakeClaudeTransport()
	agent, conn, _ := newFakeLifecycleAgent(t, transport)
	agent.setConnection(conn)
	client := claude.NewClient(agent.log, claude.Options{}, transport)
	require.NoError(t, client.Start(context.Background()))

	session := &agentSession{
		agent:             agent,
		id:                "prompt-session",
		cwd:               t.TempDir(),
		model:             "sonnet",
		client:            client,
		turn:              make(chan struct{}, agent.maxConcurrentPrompts()),
		contextWindowSize: 200000,
		mirror:            newSessionMirror(agent.log, nil, t.TempDir()),
		closeTurnWait:     defaultSessionCloseTurnWait,
	}

	return session, transport, func() { _ = client.Close() }
}
