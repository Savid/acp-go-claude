package claude

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type controlTurnTestContextKey struct{}

func admitControlCallbacksForTest(client *Client) {
	client.SetControlHandlerAdmission(func(ctx context.Context, _ string) (context.Context, func(), bool) {
		return ctx, func() {}, true
	})
}

func startClientForTest(t *testing.T, client *Client) {
	t.Helper()

	require.NoError(t, client.Start(context.Background()))
	t.Cleanup(func() { require.NoError(t, client.Close()) })
}

func TestClientStartAndQuery(t *testing.T) {
	t.Parallel()

	require.NotNil(t, NewClient(nil, Options{}, nil))

	transport := newFakeTransport()
	client := NewClient(nil, Options{SessionID: "session-1", ControlHandlerTimeout: 2 * time.Second}, transport)

	go autoRespondInitialize(transport)
	startClientForTest(t, client)
	require.Equal(t, 2*time.Second, client.activeController().handlerTimeout)

	require.NoError(t, client.Query(context.Background(), "turn-1", []map[string]any{{"type": "text", "text": "hi"}}))
	require.Eventually(t, func() bool {
		return len(transport.sentPayloads()) >= 2
	}, time.Second, 10*time.Millisecond)

	query, ok := transport.sentPayloads()[1].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "user", query["type"])
	require.Equal(t, "session-1", query["session_id"])
}

func TestClientQueryAfterClose(t *testing.T) {
	t.Parallel()

	transport := newFakeTransport()
	client := NewClient(nil, Options{}, transport)

	require.NoError(t, client.Close())
	err := client.Query(context.Background(), "turn-1", []map[string]any{{"type": "text", "text": "hi"}})

	require.ErrorIs(t, err, ErrClientClosed)
	require.True(t, transport.isClosed())
	require.Empty(t, transport.sentPayloads())
}

func TestClientCloseRetriesIncompleteTransportAndMemoizesSuccess(t *testing.T) {
	t.Parallel()

	transport := newFakeTransport()
	want := errors.New("close failed")
	transport.closeErr = want
	client := NewClient(nil, Options{}, transport)

	require.ErrorIs(t, client.Close(), want)
	transport.closeErr = nil
	require.NoError(t, client.Close())
	require.NoError(t, client.Close())
	require.Equal(t, 2, transport.closeCalls())
}

func TestClientCloseIsIdempotent(t *testing.T) {
	t.Parallel()

	transport := newFakeTransport()
	client := NewClient(nil, Options{}, transport)

	require.NoError(t, client.Close())
	require.NoError(t, client.Close())
	require.Equal(t, 1, transport.closeCalls())
}

func TestClientCloseObserversJoinPublishedAttempt(t *testing.T) {
	t.Parallel()

	want := errors.New("published close result")
	done := make(chan struct{})
	close(done)

	client := NewClient(nil, Options{}, newFakeTransport())
	client.transportCloseFlight = &clientCloseFlight{done: done, err: want}
	require.ErrorIs(t, client.closeTransport(), want)

	client.closeFlight = &clientCloseFlight{done: done, err: want}
	require.ErrorIs(t, client.Close(), want)
}

func TestClientCloseBoundsBlockedAdmittedPrefixAndReportsAbort(t *testing.T) {
	transport := newFakeTransport()
	client := NewClient(nil, Options{}, transport)

	go autoRespondInitialize(transport)
	require.NoError(t, client.Start(context.Background()))

	controller := client.activeController()
	for index := range cap(controller.messages) + 1 {
		transport.sendMessage(map[string]any{"type": "assistant", "index": index})
	}
	require.Eventually(t, func() bool {
		return len(controller.messages) == cap(controller.messages)
	}, time.Second, time.Millisecond)

	started := time.Now()
	err := client.Close()
	require.ErrorContains(t, err, string(ControllerDataTeardownAbort))
	require.Less(t, time.Since(started), 6*time.Second)
	select {
	case <-controller.Done():
	default:
		t.Fatal("controller did not join after bounded admitted-prefix abort")
	}
}

func TestClientCloseWaitsForRouterAfterResponsePanicAndDataDrain(t *testing.T) {
	transport := newFakeTransport()
	client := NewClient(slog.New(slog.DiscardHandler), Options{}, transport)
	runCtx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	controller := NewController(client.log, clientControllerTransport{
		Transport: transport,
		close:     client.closeTransport,
	})
	controller.Start(runCtx)
	client.stateMu.Lock()
	client.controller = controller
	client.cancel = cancel
	client.stateMu.Unlock()

	for index := range cap(controller.messages) {
		controller.messages <- map[string]any{"type": "blocked", "number": index}
	}
	transport.sendMessage(map[string]any{"type": "assistant", "number": cap(controller.messages)})
	transport.mu.Lock()
	transport.panicSend = true
	transport.mu.Unlock()
	transport.sendMessage(map[string]any{
		keyType:      controlRequestType,
		keyRequestID: "panic-response",
		keyRequest:   map[string]any{keySubtype: "missing"},
	})
	<-transport.closeSig

	closeDone := make(chan error, 1)
	go func() { closeDone <- client.Close() }()
	for !client.isClosed() {
		select {
		case <-t.Context().Done():
			t.Fatal("context ended before Client.Close entered its wait")
		default:
			runtime.Gosched()
		}
	}

	select {
	case closeErr := <-closeDone:
		require.NoError(t, closeErr)
		t.Fatal("Client.Close returned before the router joined its admitted data prefix")
	default:
	}
	select {
	case <-controller.Done():
		t.Fatal("router published Done before the admitted data prefix drained")
	default:
	}

	for range cap(controller.messages) + 1 {
		<-controller.Messages()
	}
	require.NoError(t, <-closeDone)
	select {
	case <-controller.Done():
	default:
		t.Fatal("Client.Close returned before controller Done")
	}
	require.ErrorIs(t, controller.LastError(), errControlResponseWrite)
}

func TestClientControlCallbackContextSnapshotsExactQueryTurn(t *testing.T) {
	t.Parallel()

	handledTurn := ""
	client := NewClient(nil, Options{
		PermissionHandler: func(ctx context.Context, _ PermissionRequest) (PermissionDecision, error) {
			handledTurn, _ = ctx.Value(controlTurnTestContextKey{}).(string)

			return PermissionDecision{Behavior: BehaviorAllow}, nil
		},
	}, newFakeTransport())
	client.SetControlHandlerAdmission(func(ctx context.Context, turnNonce string) (context.Context, func(), bool) {
		return context.WithValue(ctx, controlTurnTestContextKey{}, turnNonce), func() {}, true
	})

	require.NoError(t, client.Query(t.Context(), "turn-old", "old prompt"))
	_, err := client.handleCanUseTool(t.Context(), &ControlRequest{Request: map[string]any{}})
	require.NoError(t, err)
	require.Equal(t, "turn-old", handledTurn)
	oldCallbackCtx, finishOld, admitted := client.controlHandlerContext(t.Context())
	require.True(t, admitted)
	finishOld()
	require.Equal(t, "turn-old", oldCallbackCtx.Value(controlTurnTestContextKey{}))

	require.NoError(t, client.Query(t.Context(), "turn-current", "current prompt"))
	client.HandOffQuery("turn-old", "")
	currentCallbackCtx, finishCurrent, admitted := client.controlHandlerContext(t.Context())
	require.True(t, admitted)
	finishCurrent()
	require.Equal(t, "turn-current", currentCallbackCtx.Value(controlTurnTestContextKey{}))
	require.Equal(t, "turn-old", oldCallbackCtx.Value(controlTurnTestContextKey{}))

	client.AdoptControlRoute("turn-adopted")
	adoptedCtx, finishAdopted, admitted := client.controlHandlerContext(t.Context())
	require.True(t, admitted)
	finishAdopted()
	require.Equal(t, "turn-adopted", adoptedCtx.Value(controlTurnTestContextKey{}))

	client.HandOffQuery("turn-adopted", "")
	outsideCtx, finishOutside, admitted := client.controlHandlerContext(t.Context())
	require.True(t, admitted)
	finishOutside()
	require.Equal(t, "", outsideCtx.Value(controlTurnTestContextKey{}))
}

func TestClientControlCallbackAdmissionRejectionFailsClosed(t *testing.T) {
	t.Parallel()

	client := NewClient(nil, Options{}, newFakeTransport())
	client.SetControlHandlerAdmission(func(ctx context.Context, _ string) (context.Context, func(), bool) {
		return ctx, func() {}, false
	})

	permission, err := client.handleCanUseTool(t.Context(), &ControlRequest{Request: map[string]any{
		"input": map[string]any{"path": "/tmp/a"},
	}})
	require.NoError(t, err)
	require.Equal(t, BehaviorDeny, permission[keyBehavior])

	elicitation, err := client.handleElicitation(t.Context(), &ControlRequest{Request: map[string]any{}})
	require.NoError(t, err)
	require.Equal(t, ElicitationActionDecline, elicitation["action"])

	hook, err := client.handleHookCallback(t.Context(), &ControlRequest{Request: map[string]any{}})
	require.NoError(t, err)
	require.Equal(t, false, hook["continue"])
}

func TestClientStartErrors(t *testing.T) {
	t.Parallel()

	startErr := errors.New("start failed")
	client := NewClient(nil, Options{}, startErrorTransport{err: startErr})
	require.ErrorIs(t, client.Start(context.Background()), startErr)

	transport := newFakeTransport()
	client = NewClient(nil, Options{InitializeTimeout: time.Millisecond}, transport)
	err := client.Start(context.Background())
	require.Error(t, err)
	require.True(t, transport.isClosed())
}

func TestClientStartAfterClose(t *testing.T) {
	t.Parallel()

	transport := newFakeTransport()
	client := NewClient(nil, Options{}, transport)

	require.NoError(t, client.Close())

	err := client.Start(context.Background())
	require.ErrorIs(t, err, ErrClientClosed)
	require.True(t, transport.isClosed())
}

func TestClientStartCloseRaceAfterTransportStart(t *testing.T) {
	t.Parallel()

	transport := newStartHookTransport()
	client := NewClient(nil, Options{}, transport)
	transport.hook = func() {
		require.NoError(t, client.Close())
	}

	err := client.Start(context.Background())
	require.ErrorIs(t, err, ErrClientClosed)
	require.True(t, transport.isClosed())
}

func TestClientStartContextCancellationClosesTransport(t *testing.T) {
	t.Parallel()

	transport := newFakeTransport()
	client := NewClient(nil, Options{}, transport)
	ctx, cancel := context.WithCancel(context.Background())

	done := make(chan error, 1)
	go func() {
		done <- client.Start(ctx)
	}()

	require.Eventually(t, func() bool {
		return len(transport.sentPayloads()) == 1
	}, time.Second, 10*time.Millisecond)
	cancel()

	err := <-done
	require.ErrorIs(t, err, context.Canceled)
	require.True(t, transport.isClosed())
}

func TestClientStartLogsTransportCloseError(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	transport := newFakeTransport()
	const sentinel = "opaque-close-failure"
	transport.closeErr = errors.New(sentinel)
	client := NewClient(
		slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug})),
		Options{InitializeTimeout: time.Millisecond},
		transport,
	)

	err := client.Start(context.Background())

	require.Error(t, err)
	require.True(t, transport.isClosed())
	require.Contains(t, logs.String(), "close Claude transport failed")
	require.Contains(t, logs.String(), "initialize failed")
	require.NotContains(t, logs.String(), sentinel)
	require.NotContains(t, err.Error(), sentinel)
}

func TestClientCapturesInitializeInfo(t *testing.T) {
	t.Parallel()

	transport := newFakeTransport()
	client := NewClient(nil, Options{}, transport)

	go autoRespondInitializeWithResponse(transport, map[string]any{
		"commands": []any{
			map[string]any{
				"name":         "debug",
				"description":  "Debug session",
				"argumentHint": "[issue]",
				"aliases":      []any{"dbg", 1},
			},
			map[string]any{"description": "missing name"},
			"bad",
		},
		"models": []any{
			map[string]any{
				"value":                 "default",
				"displayName":           "Default",
				"description":           "Recommended",
				"supportsEffort":        true,
				"supportedEffortLevels": []any{"low", "medium"},
				"supportsAutoMode":      true,
				"supportsFastMode":      true,
			},
			map[string]any{"displayName": "missing value"},
			"bad",
		},
		"output_style":            "default",
		"available_output_styles": []any{"default", 1, "Explanatory"},
	})
	startClientForTest(t, client)

	info := client.InitializeInfo()
	require.Equal(t, []SlashCommand{
		{Name: "debug", Description: "Debug session", ArgumentHint: "[issue]", Aliases: []string{"dbg"}},
	}, info.Commands)
	require.Equal(t, []AvailableModelInfo{
		{
			Value:                 "default",
			DisplayName:           "Default",
			Description:           "Recommended",
			SupportedEffortLevels: []string{"low", "medium"},
			SupportsAutoMode:      true,
		},
	}, info.Models)
	require.Equal(t, "default", info.OutputStyle)
	require.Equal(t, []string{"default", "Explanatory"}, info.AvailableOutputStyles)

	info.Commands[0].Name = "mutated"
	info.Commands[0].Aliases[0] = "mutated"
	require.Equal(t, "debug", client.InitializeInfo().Commands[0].Name)
	require.Equal(t, []string{"dbg"}, client.InitializeInfo().Commands[0].Aliases)
}

func TestClientRefreshInitializeInfo(t *testing.T) {
	t.Parallel()

	transport := newFakeTransport()
	client := NewClient(nil, Options{}, transport)

	go autoRespondInitializeWithResponse(transport, map[string]any{
		"commands": []any{map[string]any{"name": "old"}},
	})
	startClientForTest(t, client)

	go respondToControlRequestAfter(transport, "initialize", 1, map[string]any{
		"commands": []any{map[string]any{"name": "new", "description": "New command"}},
	})
	info, err := client.RefreshInitializeInfo(context.Background())
	require.NoError(t, err)
	require.Equal(t, []SlashCommand{{Name: "new", Description: "New command"}}, info.Commands)
	require.Equal(t, info.Commands, client.InitializeInfo().Commands)

	stopped := client.activeController()
	require.NoError(t, client.closeTransport())
	<-stopped.Done()
	_, err = client.RefreshInitializeInfo(context.Background())
	require.ErrorContains(t, err, "refresh claude control initialize")

	unstarted := NewClient(nil, Options{}, newFakeTransport())
	_, err = unstarted.RefreshInitializeInfo(context.Background())
	require.ErrorIs(t, err, ErrClientNotStarted)
}

func TestClientStartRegistersHooks(t *testing.T) {
	t.Parallel()

	callbackIDs := []string{"acp_post_tool_use"}
	transport := newFakeTransport()
	client := NewClient(nil, Options{
		Hooks: Hooks{
			"": {
				{HookCallbackIDs: []string{"ignored"}},
			},
			HookEventPostToolUse: {
				{HookCallbackIDs: nil},
				{Matcher: "*", HookCallbackIDs: callbackIDs, TimeoutSeconds: 30},
			},
			"Other": {
				{HookCallbackIDs: []string{"other"}},
			},
		},
	}, transport)
	admitControlCallbacksForTest(client)

	go autoRespondInitialize(transport)
	startClientForTest(t, client)
	callbackIDs[0] = "mutated"

	payloads := transport.sentPayloads()
	require.NotEmpty(t, payloads)
	initReq, ok := payloads[0].(ControlRequest)
	require.True(t, ok)
	hooks, ok := initReq.Request["hooks"].(map[string]any)
	require.True(t, ok)
	require.NotContains(t, hooks, "")

	postToolUse, ok := hooks[HookEventPostToolUse].([]map[string]any)
	require.True(t, ok)
	require.Equal(t, []map[string]any{
		{
			"matcher":       "*",
			keyHookCallback: []string{"acp_post_tool_use"},
			"timeout":       30,
		},
	}, postToolUse)
	require.Equal(t, []map[string]any{
		{keyHookCallback: []string{"other"}},
	}, hooks["Other"])
}

func TestClientReceive(t *testing.T) {
	t.Parallel()

	transport := newFakeTransport()
	client := NewClient(nil, Options{}, transport)

	go autoRespondInitialize(transport)
	startClientForTest(t, client)

	transport.sendMessage(map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"content": []any{map[string]any{"type": "text", "text": "hello"}},
		},
	})

	msg, err := client.Receive(context.Background())
	require.NoError(t, err)
	require.Equal(t, MessageTypeAssistant, msg.ClaudeType())

	transport.sendMessage(map[string]any{"type": "assistant"})

	_, err = client.Receive(context.Background())
	require.Error(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = client.Receive(ctx)
	require.ErrorIs(t, err, context.Canceled)

	close(transport.events)
	_, err = client.Receive(context.Background())
	require.Error(t, err)
}

func TestClientHandlesPermissionRequest(t *testing.T) {
	t.Parallel()

	transport := newFakeTransport()
	client := NewClient(nil, Options{
		PermissionHandler: func(_ context.Context, request PermissionRequest) (PermissionDecision, error) {
			require.Equal(t, "Read", request.ToolName)
			require.Equal(t, []map[string]any{
				{
					"type":        "addRules",
					"behavior":    "allow",
					"destination": "session",
					"rules":       []any{map[string]any{"toolName": "Read"}},
				},
			}, request.Suggestions)

			return PermissionDecision{Behavior: BehaviorAllow, UpdatedInput: request.Input}, nil
		},
	}, transport)
	admitControlCallbacksForTest(client)

	go autoRespondInitialize(transport)
	startClientForTest(t, client)

	transport.sendMessage(map[string]any{
		"type":       "control_request",
		"request_id": "perm-1",
		"request": map[string]any{
			"subtype":     "can_use_tool",
			"tool_name":   "Read",
			"tool_use_id": "tool-1",
			"input":       map[string]any{"file_path": "/tmp/a"},
			"suggestions": []any{
				map[string]any{
					"type":        "addRules",
					"behavior":    "allow",
					"destination": "session",
					"rules":       []any{map[string]any{"toolName": "Read"}},
				},
			},
		},
	})

	require.Eventually(t, func() bool {
		return len(transport.sentPayloads()) >= 2
	}, time.Second, 10*time.Millisecond)

	resp, ok := transport.sentPayloads()[1].(ControlResponse)
	require.True(t, ok)
	require.Equal(t, "success", resp.Response["subtype"])

	payload, ok := resp.Response["response"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, BehaviorAllow, payload["behavior"])
}

func TestClientControlMethods(t *testing.T) {
	t.Parallel()

	transport := newFakeTransport()
	client := NewClient(nil, Options{}, transport)

	go autoRespondInitialize(transport)
	startClientForTest(t, client)

	go respondToControlRequest(transport, "interrupt")
	require.NoError(t, client.Interrupt(context.Background()))

	go respondToControlRequest(transport, "set_permission_mode")
	require.NoError(t, client.SetPermissionMode(context.Background(), "plan"))

	go respondToControlRequest(transport, "set_model")
	require.NoError(t, client.SetModel(context.Background(), "claude-test"))

	go respondToControlRequest(transport, "apply_flag_settings")
	require.NoError(t, client.SetOutputStyle(context.Background(), "Explanatory"))

	go respondToControlRequestAfter(transport, "apply_flag_settings", len(transport.sentPayloads()), map[string]any{})
	require.NoError(t, client.SetFastMode(context.Background(), true))

	go respondToControlRequestWithResponse(transport, "get_context_usage", map[string]any{
		"totalTokens": float64(42),
		"maxTokens":   float64(200000),
		"model":       "claude-test",
	})
	usage, err := client.GetContextUsage(context.Background())
	require.NoError(t, err)
	require.Equal(t, 42, usage.TotalTokens)
	require.Equal(t, 200000, usage.MaxTokens)

	go respondToControlRequestWithResponse(transport, "get_settings", map[string]any{
		"applied": map[string]any{
			"model":  "claude-test",
			"effort": "low",
		},
		"effective": map[string]any{
			"fastMode": true,
		},
	})
	settings, err := client.GetSettings(context.Background())
	require.NoError(t, err)
	require.Equal(t, "claude-test", settings.Applied.Model)
	require.Equal(t, "low", settings.Applied.Effort)
	require.NotNil(t, settings.FastMode)
	require.True(t, *settings.FastMode)
}

func TestClientControlMethodSendErrors(t *testing.T) {
	t.Parallel()

	transport := newFakeTransport()
	client := NewClient(nil, Options{}, transport)

	go autoRespondInitialize(transport)
	startClientForTest(t, client)

	const sentinel = "opaque-send-failure"
	sendErr := errors.New(sentinel)
	transport.sendErr = sendErr

	require.ErrorIs(t, client.Interrupt(context.Background()), errClaudeTransportFailure)
	require.ErrorIs(t, client.SetPermissionMode(context.Background(), "plan"), errClaudeTransportFailure)
	require.ErrorIs(t, client.SetModel(context.Background(), "claude-test"), errClaudeTransportFailure)
	require.ErrorIs(t, client.ApplyFlagSettings(context.Background(), map[string]any{"x": true}), errClaudeTransportFailure)

	_, err := client.GetSettings(context.Background())
	require.ErrorIs(t, err, errClaudeTransportFailure)
	require.NotContains(t, err.Error(), sentinel)

	_, err = client.GetContextUsage(context.Background())
	require.ErrorIs(t, err, errClaudeTransportFailure)
	require.NotContains(t, err.Error(), sentinel)
}

func TestClientControlMethodsBeforeStart(t *testing.T) {
	t.Parallel()

	client := NewClient(nil, Options{}, newFakeTransport())

	require.Error(t, client.Interrupt(context.Background()))
	require.Error(t, client.SetPermissionMode(context.Background(), "plan"))
	require.Error(t, client.SetModel(context.Background(), "claude-test"))
	require.Error(t, client.SetOutputStyle(context.Background(), "default"))
	require.Error(t, client.SetEffort(context.Background(), "low"))
	require.Error(t, client.SetFastMode(context.Background(), true))
	require.Error(t, client.ApplyFlagSettings(context.Background(), nil))
	_, err := client.GetSettings(context.Background())
	require.Error(t, err)
	_, err = client.GetContextUsage(context.Background())
	require.Error(t, err)

	_, err = client.Receive(context.Background())
	require.Error(t, err)
}

func TestParseContextUsageDefaults(t *testing.T) {
	t.Parallel()

	usage := parseContextUsage(nil)
	require.NotNil(t, usage)
	require.Zero(t, usage.TotalTokens)
	require.Zero(t, usage.MaxTokens)
}

func TestParseSettingsDefaults(t *testing.T) {
	t.Parallel()

	settings := parseSettings(map[string]any{
		"effective": map[string]any{
			"fastMode": false,
		},
	})
	require.NotNil(t, settings.FastMode)
	require.False(t, *settings.FastMode)
	require.Empty(t, settings.Applied.Model)

	require.Empty(t, parseSettings(nil).Applied.Model)
}

func TestClientClose(t *testing.T) {
	t.Parallel()

	require.NoError(t, (&Client{}).Close())

	transport := newFakeTransport()
	client := NewClient(nil, Options{}, transport)

	require.NoError(t, client.Close())
	require.True(t, transport.isClosed())

	transport = newFakeTransport()
	client = NewClient(nil, Options{}, transport)
	go autoRespondInitialize(transport)
	startClientForTest(t, client)
	require.NoError(t, client.Close())
	require.True(t, transport.isClosed())
}

func TestClientUnsupportedElicitation(t *testing.T) {
	t.Parallel()

	client := NewClient(nil, Options{}, newFakeTransport())
	payload, err := client.handleElicitation(context.Background(), &ControlRequest{})

	require.NoError(t, err)
	require.Equal(t, "decline", payload["action"])
}

func TestClientConfiguredCallbackDefaultsRemainFailClosed(t *testing.T) {
	t.Parallel()

	client := NewClient(nil, Options{}, newFakeTransport())
	admitControlCallbacksForTest(client)

	permission, err := client.handleCanUseTool(t.Context(), &ControlRequest{Request: map[string]any{
		"input": map[string]any{"path": "/tmp/current"},
	}})
	require.NoError(t, err)
	require.Equal(t, BehaviorDeny, permission[keyBehavior])
	require.Equal(t, "permission handler is not configured", permission[keyMessage])

	elicitation, err := client.handleElicitation(t.Context(), &ControlRequest{Request: map[string]any{}})
	require.NoError(t, err)
	require.Equal(t, ElicitationActionDecline, elicitation["action"])

	hook, err := client.handleHookCallback(t.Context(), &ControlRequest{Request: map[string]any{}})
	require.NoError(t, err)
	require.Equal(t, true, hook["continue"])

	client.SetControlHandlerAdmission(nil)
	permission, err = client.handleCanUseTool(t.Context(), &ControlRequest{Request: map[string]any{}})
	require.NoError(t, err)
	require.Equal(t, BehaviorDeny, permission[keyBehavior])
	require.Equal(t, "permission callback no longer has a live owner", permission[keyMessage])
}

func TestClientHandleCanUseToolRequiresAdmission(t *testing.T) {
	t.Parallel()

	client := NewClient(nil, Options{}, newFakeTransport())
	payload, err := client.handleCanUseTool(context.Background(), &ControlRequest{
		Request: map[string]any{
			"tool_name": "Read",
			"input":     map[string]any{"file_path": "/tmp/a"},
		},
	})
	require.NoError(t, err)
	require.Equal(t, BehaviorDeny, payload[keyBehavior])
	require.Equal(t, "permission callback no longer has a live owner", payload[keyMessage])

	handlerErr := errors.New("permission failed")
	client = NewClient(nil, Options{
		PermissionHandler: func(context.Context, PermissionRequest) (PermissionDecision, error) {
			return PermissionDecision{}, handlerErr
		},
	}, newFakeTransport())
	admitControlCallbacksForTest(client)
	_, err = client.handleCanUseTool(context.Background(), &ControlRequest{Request: map[string]any{}})
	require.ErrorIs(t, err, handlerErr)
	require.ErrorContains(t, err, "permission handler")
}

func TestClientHandlesElicitationRequest(t *testing.T) {
	t.Parallel()

	transport := newFakeTransport()
	client := NewClient(nil, Options{
		ElicitationHandler: func(_ context.Context, request ElicitationRequest) (ElicitationResponse, error) {
			require.Equal(t, "server-1", request.MCPServerName)
			require.Equal(t, "Approve access?", request.Message)
			require.Equal(t, ElicitationModeForm, request.Mode)
			require.Equal(t, []any{"approved"}, request.RequestedSchema["required"])

			return ElicitationResponse{
				Action:  ElicitationActionAccept,
				Content: map[string]any{"approved": true},
			}, nil
		},
	}, transport)
	admitControlCallbacksForTest(client)

	go autoRespondInitialize(transport)
	startClientForTest(t, client)

	transport.sendMessage(map[string]any{
		"type":       "control_request",
		"request_id": "elicitation-1",
		"request": map[string]any{
			"subtype":         "elicitation",
			"mcp_server_name": "server-1",
			"message":         "Approve access?",
			"mode":            "form",
			"requested_schema": map[string]any{
				"type":     "object",
				"required": []any{"approved"},
				"properties": map[string]any{
					"approved": map[string]any{"type": "boolean"},
				},
			},
		},
	})

	require.Eventually(t, func() bool {
		return len(transport.sentPayloads()) >= 2
	}, time.Second, 10*time.Millisecond)

	resp, ok := transport.sentPayloads()[1].(ControlResponse)
	require.True(t, ok)
	require.Equal(t, "success", resp.Response["subtype"])

	payload, ok := resp.Response["response"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, ElicitationActionAccept, payload["action"])
	require.Equal(t, map[string]any{"approved": true}, payload["content"])
}

func TestClientHandlesHookCallback(t *testing.T) {
	t.Parallel()

	transport := newFakeTransport()
	client := NewClient(nil, Options{
		HookHandler: func(_ context.Context, request HookRequest) (HookResponse, error) {
			require.Equal(t, HookEventPostToolUse, request.EventName)
			require.Equal(t, "Edit", request.ToolName)
			require.Equal(t, "tool-1", request.ToolUseID)
			require.Equal(t, map[string]any{"filePath": "/tmp/a.go"}, request.ToolResponse)

			return HookResponse{Continue: true}, nil
		},
	}, transport)
	admitControlCallbacksForTest(client)

	go autoRespondInitialize(transport)
	startClientForTest(t, client)

	transport.sendMessage(map[string]any{
		"type":       "control_request",
		"request_id": "hook-1",
		"request": map[string]any{
			"subtype":     "hook_callback",
			"callback_id": "acp_post_tool_use",
			"input": map[string]any{
				"hook_event_name": HookEventPostToolUse,
				"tool_name":       "Edit",
				"tool_use_id":     "tool-1",
				"tool_input":      map[string]any{"file_path": "/tmp/a.go"},
				"tool_response":   map[string]any{"filePath": "/tmp/a.go"},
			},
		},
	})

	require.Eventually(t, func() bool {
		return len(transport.sentPayloads()) >= 2
	}, time.Second, 10*time.Millisecond)

	resp, ok := transport.sentPayloads()[1].(ControlResponse)
	require.True(t, ok)
	require.Equal(t, "success", resp.Response["subtype"])

	payload, ok := resp.Response["response"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, true, payload["continue"])
}

func TestClientElicitationHandlerError(t *testing.T) {
	t.Parallel()

	handlerErr := errors.New("elicitation failed")
	client := NewClient(nil, Options{
		ElicitationHandler: func(context.Context, ElicitationRequest) (ElicitationResponse, error) {
			return ElicitationResponse{}, handlerErr
		},
	}, newFakeTransport())
	admitControlCallbacksForTest(client)

	_, err := client.handleElicitation(context.Background(), &ControlRequest{Request: map[string]any{}})
	require.ErrorIs(t, err, handlerErr)
}

func TestClientElicitationHandlerReceivesContext(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	client := NewClient(nil, Options{
		ElicitationHandler: func(ctx context.Context, _ ElicitationRequest) (ElicitationResponse, error) {
			return ElicitationResponse{}, ctx.Err()
		},
	}, newFakeTransport())
	admitControlCallbacksForTest(client)

	_, err := client.handleElicitation(ctx, &ControlRequest{Request: map[string]any{}})
	require.ErrorIs(t, err, context.Canceled)
}

func TestElicitationHelpers(t *testing.T) {
	t.Parallel()

	require.Equal(t, ElicitationModeForm, ElicitationRequest{}.requestedMode())
	require.Equal(t, ElicitationModeURL, ElicitationRequest{URL: "https://example.com"}.requestedMode())
	require.Equal(t, "custom", ElicitationRequest{Mode: "custom", URL: "https://example.com"}.requestedMode())

	defaultPayload := ElicitationResponse{}.toPayload()
	require.Equal(t, ElicitationActionDecline, defaultPayload["action"])
	require.NotContains(t, defaultPayload, "content")
}

func TestParseInitializeInfoDefaults(t *testing.T) {
	t.Parallel()

	require.Empty(t, parseInitializeInfo(nil).Models)
}

func TestClientHookCallbackRequiresAdmission(t *testing.T) {
	t.Parallel()

	client := NewClient(nil, Options{}, newFakeTransport())
	payload, err := client.handleHookCallback(context.Background(), &ControlRequest{Request: map[string]any{}})
	require.NoError(t, err)
	require.Equal(t, false, payload["continue"])
	require.Equal(t, false, HookResponse{}.toPayload()["continue"])

	handlerErr := errors.New("hook failed")
	client = NewClient(nil, Options{
		HookHandler: func(context.Context, HookRequest) (HookResponse, error) {
			return HookResponse{}, handlerErr
		},
	}, newFakeTransport())
	admitControlCallbacksForTest(client)
	_, err = client.handleHookCallback(context.Background(), &ControlRequest{Request: map[string]any{}})
	require.ErrorIs(t, err, handlerErr)
}

func autoRespondInitialize(transport *fakeTransport) {
	autoRespondInitializeWithResponse(transport, map[string]any{})
}

func autoRespondInitializeWithResponse(transport *fakeTransport, response map[string]any) {
	for {
		payloads := transport.sentPayloads()
		if len(payloads) == 0 {
			time.Sleep(time.Millisecond)

			continue
		}

		req, ok := payloads[0].(ControlRequest)
		if ok {
			transport.sendMessage(map[string]any{
				"type": "control_response",
				"response": map[string]any{
					"subtype":    "success",
					"request_id": req.RequestID,
					"response":   response,
				},
			})
		}

		return
	}
}

func respondToControlRequest(transport *fakeTransport, subtype string) {
	respondToControlRequestWithResponse(transport, subtype, map[string]any{})
}

func respondToControlRequestWithResponse(transport *fakeTransport, subtype string, response map[string]any) {
	respondToControlRequestAfter(transport, subtype, 0, response)
}

func respondToControlRequestAfter(transport *fakeTransport, subtype string, after int, response map[string]any) {
	for {
		payloads := transport.sentPayloads()
		for _, payload := range payloads[after:] {
			req, ok := payload.(ControlRequest)
			if !ok || req.Request[keySubtype] != subtype {
				continue
			}

			transport.sendMessage(map[string]any{
				keyType: controlResponseType,
				keyResponse: map[string]any{
					keySubtype:   responseSubtypeSuccess,
					keyRequestID: req.RequestID,
					keyResponse:  response,
				},
			})

			return
		}

		time.Sleep(time.Millisecond)
	}
}

type startErrorTransport struct {
	err error
}

type startHookTransport struct {
	*fakeTransport

	hook func()
}

func newStartHookTransport() *startHookTransport {
	return &startHookTransport{fakeTransport: newFakeTransport()}
}

func (t *startHookTransport) Start(context.Context) error {
	if t.hook != nil {
		t.hook()
	}

	return nil
}

func (t startErrorTransport) Start(context.Context) error {
	return t.err
}

func (t startErrorTransport) Send(context.Context, any) error {
	return nil
}

func (t startErrorTransport) Events(context.Context) <-chan TransportEvent {
	events := make(chan TransportEvent)
	close(events)

	return events
}

func (t startErrorTransport) Close() error {
	return nil
}
