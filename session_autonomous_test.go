package claudeacp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/savid/acp-go-claude/internal/lifecycle"
	"github.com/savid/acp-go-claude/internal/mapper"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

const (
	autonomousToolSentinel      = "SENTINEL-TOOL-RESULT"
	autonomousAssistantSentinel = "SENTINEL-ASSISTANT"
)

// taskStartedFrame is the native frame that proves one task's identity: the task
// id that names it and the tool use it renders as.
func taskStartedFrame() map[string]any {
	return namedTaskStartedFrame("task-1", "tool-1")
}

func namedTaskStartedFrame(taskID, toolUseID string) map[string]any {
	return map[string]any{
		"type":          "system",
		"subtype":       "task_started",
		"task_id":       taskID,
		"tool_use_id":   toolUseID,
		"workflow_name": "background",
		"description":   "background work",
	}
}

// taskNotificationFrame is the completion the harness reports for that same
// task. It names the task alone, so a consumer that lost the correlation at the
// prompt boundary could not resolve it.
func taskNotificationFrame() map[string]any {
	return namedTaskNotificationFrame("task-1")
}

func namedTaskNotificationFrame(taskID string) map[string]any {
	return map[string]any{
		"type":    "system",
		"subtype": "task_notification",
		"task_id": taskID,
		"status":  "completed",
		"summary": "background work finished",
	}
}

// taskNotificationResultFrame is the terminal the harness attributes to a task
// notification rather than to a submission. Its origin is the harness's
// SDKMessageOrigin vocabulary — a kind and nothing else — so the result names
// no task and borrows no task owner.
func taskNotificationResultFrame() map[string]any {
	frame := resultFrame()
	frame["origin"] = map[string]any{"kind": originKindTaskNotification}

	return frame
}

// systemIdleFrame is the state frame the harness closes work with when it sends
// no result at all.
func systemIdleFrame() map[string]any {
	return map[string]any{
		"type":      "system",
		"subtype":   systemSubtypeSessionStateChanged,
		systemState: systemStateIdle,
	}
}

// requireForegroundBusy asserts the refusal a prompt gets while an agent-origin
// turn holds the session's foreground. It is retryable by construction: the
// excursion settles on its own native terminal and the same prompt then runs.
func requireForegroundBusy(t *testing.T, err error) {
	t.Helper()

	var reqErr *acp.RequestError
	require.ErrorAs(t, err, &reqErr)
	require.Equal(t, -32600, reqErr.Code)

	data, ok := reqErr.Data.(map[string]any)
	require.True(t, ok, "the refusal names the limit it hit")
	require.Equal(t, "backpressure", data[jsonFieldError])
	require.Equal(t, "session_foreground", data["limit"])
}

// blockingPermissionClient holds every permission request until the test
// releases it or the adapter cancels it, which is how a pending action is kept
// pending across another call.
type blockingPermissionClient struct {
	*recordingAgentClient

	entered chan struct{}
	release chan struct{}
}

// blockingCallbackClient records and parks either callback family after the
// adapter has emitted its owned pending content. Tests use the entered channels
// as semantic barriers rather than relying on scheduler timing.
type blockingCallbackClient struct {
	*recordingPermissionClient

	permissionEntered   chan struct{}
	elicitationEntered  chan struct{}
	permissionCanceled  chan struct{}
	elicitationCanceled chan struct{}
	release             chan struct{}
}

func newBlockingCallbackClient(conn *recordingAgentClient) *blockingCallbackClient {
	return &blockingCallbackClient{
		recordingPermissionClient: &recordingPermissionClient{recordingAgentClient: conn},
		permissionEntered:         make(chan struct{}, 1),
		elicitationEntered:        make(chan struct{}, 1),
		permissionCanceled:        make(chan struct{}, 1),
		elicitationCanceled:       make(chan struct{}, 1),
		release:                   make(chan struct{}),
	}
}

func (c *blockingCallbackClient) RequestPermission(
	ctx context.Context,
	request acp.RequestPermissionRequest,
	action actionWireAdmission,
) (acp.RequestPermissionResponse, error) {
	if err := action.publishPending(); err != nil {
		return acp.RequestPermissionResponse{}, err
	}

	c.mu.Lock()
	c.requests = append(c.requests, request)
	c.mu.Unlock()
	if err := action.observeWrite(ctx, actionWireIdentity{
		method:    acp.ClientMethodSessionRequestPermission,
		requestID: "blocking-permission-" + action.actionID,
	}); err != nil {
		return acp.RequestPermissionResponse{}, err
	}

	c.permissionEntered <- struct{}{}

	select {
	case <-c.release:
		return acp.RequestPermissionResponse{
			Outcome: acp.NewRequestPermissionOutcomeSelected(permissionAllowOnce),
		}, nil
	case <-ctx.Done():
		c.permissionCanceled <- struct{}{}

		return acp.RequestPermissionResponse{}, ctx.Err()
	}
}

func (c *blockingCallbackClient) CreateElicitation(
	ctx context.Context,
	request acp.UnstableCreateElicitationRequest,
	_ elicitationScope,
	action actionWireAdmission,
) (acp.UnstableCreateElicitationResponse, error) {
	if err := action.publishPending(); err != nil {
		return acp.UnstableCreateElicitationResponse{}, err
	}

	c.mu.Lock()
	c.elicitations = append(c.elicitations, request)
	c.mu.Unlock()
	if err := action.observeWrite(ctx, actionWireIdentity{
		method:    acp.ClientMethodElicitationCreate,
		requestID: "blocking-elicitation-" + action.actionID,
	}); err != nil {
		return acp.UnstableCreateElicitationResponse{}, err
	}

	c.elicitationEntered <- struct{}{}

	select {
	case <-c.release:
		response := acp.NewUnstableCreateElicitationResponseAccept()
		response.Accept.Content = map[string]any{"approved": true}

		return response, nil
	case <-ctx.Done():
		c.elicitationCanceled <- struct{}{}

		return acp.UnstableCreateElicitationResponse{}, ctx.Err()
	}
}

func pushControlCallback(transport *fakeClaudeTransport, kind lifecycle.ActionKind, requestID, toolCallID string) {
	request := map[string]any{
		"tool_use_id": toolCallID,
	}

	switch kind {
	case lifecycle.ActionPermission:
		request["subtype"] = "can_use_tool"
		request["tool_name"] = "Write"
		request["input"] = map[string]any{"file_path": "/tmp/x", "content": "y"}
	case lifecycle.ActionElicitation:
		request["subtype"] = "elicitation"
		request["mode"] = claude.ElicitationModeForm
		request["message"] = "Approve?"
		request["requested_schema"] = map[string]any{
			"type":       "object",
			"properties": map[string]any{"approved": map[string]any{"type": "boolean"}},
		}
	}

	transport.messages <- map[string]any{
		"type":       "control_request",
		"request_id": requestID,
		"request":    request,
	}
}

func newBlockingPermissionClient(conn *recordingAgentClient) *blockingPermissionClient {
	return &blockingPermissionClient{
		recordingAgentClient: conn,
		entered:              make(chan struct{}, 1),
		release:              make(chan struct{}),
	}
}

func (c *blockingPermissionClient) RequestPermission(
	ctx context.Context,
	_ acp.RequestPermissionRequest,
	action actionWireAdmission,
) (acp.RequestPermissionResponse, error) {
	if err := action.publishPending(); err != nil {
		return acp.RequestPermissionResponse{}, err
	}

	if err := action.observeWrite(ctx, actionWireIdentity{
		method:    acp.ClientMethodSessionRequestPermission,
		requestID: "blocking-permission-" + action.actionID,
	}); err != nil {
		return acp.RequestPermissionResponse{}, err
	}
	select {
	case c.entered <- struct{}{}:
	default:
	}

	select {
	case <-c.release:
		return acp.RequestPermissionResponse{
			Outcome: acp.NewRequestPermissionOutcomeSelected(permissionAllowOnce),
		}, nil
	case <-ctx.Done():
		return acp.RequestPermissionResponse{}, ctx.Err()
	}
}

func toolResultFrame(sentinel string) map[string]any {
	return map[string]any{
		"type": "user",
		"message": map[string]any{
			"role": "user",
			"content": []any{map[string]any{
				"type":        "tool_result",
				"tool_use_id": "tool-1",
				"content":     sentinel,
			}},
		},
	}
}

func assistantFrame(sentinel string) map[string]any {
	return map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"model":       "sonnet",
			"stop_reason": "end_turn",
			"content":     []any{map[string]any{"type": "text", "text": sentinel}},
		},
	}
}

func resultFrame() map[string]any {
	return map[string]any{
		"type":        "result",
		"subtype":     "success",
		"is_error":    false,
		"stop_reason": "end_turn",
		"usage":       map[string]any{"input_tokens": 3, "output_tokens": 4},
	}
}

// pushNativeFrames feeds frames to the session's reader the way the harness
// would, with no prompt in flight.
func pushNativeFrames(transport *fakeClaudeTransport, frames ...map[string]any) {
	for _, frame := range frames {
		transport.messages <- frame
	}
}

// notificationEvents reports, in delivery order, one decoded lifecycle event per
// notification that carried one, beside the index of the notification it rode.
type notificationEvent struct {
	index int
	event map[string]any
}

func lifecycleNotificationEvents(t *testing.T, conn *recordingAgentClient) []notificationEvent {
	t.Helper()

	events := []notificationEvent{}

	for index, update := range conn.Updates() {
		envelope, ok := update.Meta[lifecycle.MetaKey].(map[string]any)
		if !ok {
			continue
		}

		events = append(events, notificationEvent{index: index, event: requireAnyMap(t, envelope["event"])})
	}

	return events
}

// findLifecycleEvent reports the first event at or after `from` that matches, and
// the notification index it rode.
func findLifecycleEvent(
	t *testing.T,
	conn *recordingAgentClient,
	from int,
	match func(map[string]any) bool,
) (map[string]any, int, bool) {
	t.Helper()

	for _, entry := range lifecycleNotificationEvents(t, conn) {
		if entry.index >= from && match(entry.event) {
			return entry.event, entry.index, true
		}
	}

	return nil, 0, false
}

func transitionMatcher(state lifecycle.ForegroundState, cause lifecycle.Cause) func(map[string]any) bool {
	return func(event map[string]any) bool {
		return event["type"] == string(lifecycle.EventStateUpdate) &&
			event["state"] == string(state) && event["cause"] == string(cause)
	}
}

// sentinelNotificationIndex reports the index of the first notification whose
// wire form carries the sentinel.
func sentinelNotificationIndex(t *testing.T, conn *recordingAgentClient, sentinel string) (int, bool) {
	t.Helper()

	for index, update := range conn.Updates() {
		encoded, err := json.Marshal(update)
		require.NoError(t, err)

		if strings.Contains(string(encoded), sentinel) {
			return index, true
		}
	}

	return 0, false
}

func awaitLifecycleEvent(t *testing.T, conn *recordingAgentClient, match func(map[string]any) bool) {
	t.Helper()

	awaitAgentUpdates(t, conn, func() bool {
		_, _, found := findLifecycleEvent(t, conn, 0, match)

		return found
	})
}

func awaitAgentUpdates(t *testing.T, conn *recordingAgentClient, ready func() bool) {
	t.Helper()

	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()

	for !ready() {
		select {
		case <-conn.UpdatesChanged():
		case <-timer.C:
			t.Fatal("timed out waiting for an acknowledged session update")
		}
	}
}

// TestBetweenPromptWorkOpensOneAgentOriginTurn is the incident: a foreground
// prompt returns, the harness keeps working, and everything it produces after
// that return reaches the host under one fresh agent-origin turn.
func TestBetweenPromptWorkOpensOneAgentOriginTurn(t *testing.T) {
	session, transport, conn, cleanup := newNegotiatedPromptFlowSession(t)
	defer cleanup()

	// The prompt itself starts the task, so the correlation the between-prompt
	// frames depend on is established inside the foreground turn.
	transport.queryMsgs = []map[string]any{taskStartedFrame(), resultFrame()}

	response, err := session.Prompt(t.Context(), lifecyclePromptRequest(session.id, "test-turn", "hello"))
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, response.StopReason)

	promptIdle, promptIdleIndex, found := findLifecycleEvent(t, conn, 0,
		transitionMatcher(lifecycle.ForegroundIdle, lifecycle.CauseSubmission))
	require.True(t, found, "the foreground prompt settles its own turn")

	promptTurnID, _ := promptIdle["turnId"].(string)
	require.NotEmpty(t, promptTurnID)

	beforeExcursion := len(conn.Updates())

	pushNativeFrames(transport,
		taskNotificationFrame(),
		toolResultFrame(autonomousToolSentinel),
		assistantFrame(autonomousAssistantSentinel),
		resultFrame(),
	)

	awaitLifecycleEvent(t, conn, transitionMatcher(lifecycle.ForegroundIdle, lifecycle.CauseActivity))

	running, runningIndex, found := findLifecycleEvent(t, conn, beforeExcursion,
		transitionMatcher(lifecycle.ForegroundRunning, lifecycle.CauseActivity))
	require.True(t, found, "the excursion opens an agent-origin turn")
	require.Greater(t, runningIndex, promptIdleIndex, "the excursion follows the prompt it outlived")

	agentTurnID, _ := running["turnId"].(string)
	require.NotEmpty(t, agentTurnID)
	require.NotEqual(t, promptTurnID, agentTurnID, "the excursion is a turn of its own")

	idle, idleIndex, found := findLifecycleEvent(t, conn, runningIndex,
		transitionMatcher(lifecycle.ForegroundIdle, lifecycle.CauseActivity))
	require.True(t, found)
	require.Equal(t, agentTurnID, idle["turnId"])
	require.Equal(t, "success", idle["outcome"])
	require.Equal(t, "end_turn", idle["stopReason"])

	// The excursion is announced by its running transition and by nothing else: a
	// turn nobody submitted carries no acceptance.
	for _, entry := range lifecycleNotificationEvents(t, conn) {
		if entry.index <= promptIdleIndex {
			continue
		}

		require.NotEqual(t, string(lifecycle.EventPromptAccepted), entry.event["type"],
			"the agent-origin turn carries no submission")
	}

	toolIndex, found := sentinelNotificationIndex(t, conn, autonomousToolSentinel)
	require.True(t, found, "the between-prompt tool result reaches the host")
	require.Greater(t, toolIndex, runningIndex)
	require.Less(t, toolIndex, idleIndex)

	assistantIndex, found := sentinelNotificationIndex(t, conn, autonomousAssistantSentinel)
	require.True(t, found, "the between-prompt assistant text reaches the host")
	require.Greater(t, assistantIndex, toolIndex, "native order is preserved")
	require.Less(t, assistantIndex, idleIndex)

	require.Equal(t, []string{agentTurnID}, agentTurnIDs(t, conn, beforeExcursion),
		"one excursion is exactly one turn")
	require.Equal(t, 1, countLifecycleEvents(t, conn, beforeExcursion,
		transitionMatcher(lifecycle.ForegroundIdle, lifecycle.CauseActivity)),
		"the excursion settles exactly once")
}

// TestTaskNotificationResultSettlesTheExcursionItRead pins the between-prompt
// terminal against the harness's real emission order: each notification's
// autonomous turn ends on a result whose origin is `{"kind":"task-notification"}`
// and nothing follows that result until further work. A result that arrives
// before any excursion is open ends nothing; the one that arrives with the
// excursion open settles it, whether or not another tracked task is still live —
// background liveness is not a foreground.
func TestTaskNotificationResultSettlesTheExcursionItRead(t *testing.T) {
	session, transport, conn, cleanup := newNegotiatedPromptFlowSession(t)
	defer cleanup()

	transport.queryMsgs = []map[string]any{
		namedTaskStartedFrame("task-1", "tool-1"),
		namedTaskStartedFrame("task-2", "tool-2"),
		resultFrame(),
	}
	_, err := session.Prompt(t.Context(), lifecyclePromptRequest(session.id, "task-parent", "hello"))
	require.NoError(t, err)

	before := len(conn.Updates())
	pushNativeFrames(transport,
		namedTaskNotificationFrame("task-1"),
		taskNotificationResultFrame(),
		assistantFrame("task-one-result-processed"),
	)
	awaitAgentUpdates(t, conn, func() bool {
		_, found := sentinelNotificationIndex(t, conn, "task-one-result-processed")

		return found
	})
	require.Zero(t, countLifecycleEvents(t, conn, before,
		transitionMatcher(lifecycle.ForegroundIdle, lifecycle.CauseActivity)),
		"a result read before any excursion was open ends nothing")
	require.Len(t, agentTurnIDs(t, conn, before), 1)

	pushNativeFrames(transport,
		namedTaskNotificationFrame("task-2"),
		taskNotificationResultFrame(),
	)
	awaitLifecycleEvent(t, conn, transitionMatcher(lifecycle.ForegroundIdle, lifecycle.CauseActivity))
	idle, _, found := findLifecycleEvent(t, conn, before,
		transitionMatcher(lifecycle.ForegroundIdle, lifecycle.CauseActivity))
	require.True(t, found)
	require.Equal(t, "success", idle["outcome"],
		"the task-notification result settles the open excursion even with a task still tracked")
	require.Equal(t, 1, countLifecycleEvents(t, conn, before,
		transitionMatcher(lifecycle.ForegroundIdle, lifecycle.CauseActivity)))
	require.Len(t, agentTurnIDs(t, conn, before), 1)
	require.Nil(t, session.excursion, "nothing holds the foreground after the settle")
}

// TestStackedTaskNotificationsSettleOneExcursionEach is the incident: one task
// fires two notifications, each runs its own autonomous API turn, and each turn
// ends on a result whose origin is `{"kind":"task-notification"}` — the only
// terminal the harness emits between prompts. Each notification's reply reaches
// the host under its own settled agent-origin turn, a transcript-mirror batch
// that journals the autonomous turn into the prompt-written file is store state,
// and the foreground is free for the next prompt as soon as the last result is
// read.
func TestStackedTaskNotificationsSettleOneExcursionEach(t *testing.T) {
	session, transport, conn, cleanup := newNegotiatedPromptFlowSession(t)
	defer cleanup()

	const transcriptPath = "/native/projects/project/prompt-session.jsonl"

	promptAssistant := assistantFrame("prompt-reply")
	promptAssistant["uuid"] = "prompt-assistant-1"
	transport.queryMsgs = []map[string]any{
		taskStartedFrame(),
		promptAssistant,
		{
			"type":     claude.MessageTypeMirror,
			"filePath": transcriptPath,
			"entries": []any{
				map[string]any{"type": "assistant", "uuid": "prompt-assistant-1"},
			},
		},
		resultFrame(),
	}

	_, err := session.Prompt(t.Context(), lifecyclePromptRequest(session.id, "arming-turn", "arm the monitor"))
	require.NoError(t, err)

	before := len(conn.Updates())

	replyOne := assistantFrame("SENTINEL-STACKED-REPLY-1")
	replyOne["uuid"] = "notification-assistant-1"
	pushNativeFrames(transport,
		taskNotificationFrame(),
		map[string]any{
			"type":   "user",
			"uuid":   "notification-user-1",
			"origin": map[string]any{"kind": originKindTaskNotification},
			"message": map[string]any{
				"role":    "user",
				"content": []any{map[string]any{"type": "text", "text": "<task-notification>fired</task-notification>"}},
			},
		},
		replyOne,
		map[string]any{
			"type":     claude.MessageTypeMirror,
			"filePath": transcriptPath,
			"entries": []any{
				map[string]any{"type": "user", "uuid": "notification-user-1"},
				map[string]any{"type": "assistant", "uuid": "notification-assistant-1"},
			},
		},
		taskNotificationResultFrame(),
	)

	awaitLifecycleEvent(t, conn, transitionMatcher(lifecycle.ForegroundIdle, lifecycle.CauseActivity))
	firstIdle, firstIdleIndex, found := findLifecycleEvent(t, conn, before,
		transitionMatcher(lifecycle.ForegroundIdle, lifecycle.CauseActivity))
	require.True(t, found, "the first notification's result settles its excursion")
	require.Equal(t, "success", firstIdle["outcome"])
	replyOneIndex, found := sentinelNotificationIndex(t, conn, "SENTINEL-STACKED-REPLY-1")
	require.True(t, found)
	require.Less(t, replyOneIndex, firstIdleIndex, "the reply precedes its own terminal idle")

	// The second notification of the same task arrives after the first
	// autonomous turn settled, and runs as a fresh agent-origin turn.
	replyTwo := assistantFrame("SENTINEL-STACKED-REPLY-2")
	replyTwo["uuid"] = "notification-assistant-2"
	pushNativeFrames(transport,
		taskNotificationFrame(),
		map[string]any{
			"type":   "user",
			"uuid":   "notification-user-2",
			"origin": map[string]any{"kind": originKindTaskNotification},
			"message": map[string]any{
				"role":    "user",
				"content": []any{map[string]any{"type": "text", "text": "<task-notification>completed</task-notification>"}},
			},
		},
		replyTwo,
		taskNotificationResultFrame(),
	)

	awaitAgentUpdates(t, conn, func() bool {
		return countLifecycleEvents(t, conn, before,
			transitionMatcher(lifecycle.ForegroundIdle, lifecycle.CauseActivity)) == 2
	})

	_, found = sentinelNotificationIndex(t, conn, "SENTINEL-STACKED-REPLY-2")
	require.True(t, found, "the second autonomous reply reaches the host")

	require.Len(t, agentTurnIDs(t, conn, before), 2,
		"each notification's autonomous turn is its own settled agent-origin turn")
	require.Equal(t, 2, countLifecycleEvents(t, conn, before,
		transitionMatcher(lifecycle.ForegroundIdle, lifecycle.CauseActivity)),
		"each excursion settles exactly once")

	incarnation := session.currentNativeIncarnation()
	require.NotNil(t, incarnation)
	require.False(t, incarnation.failed.Load(), "nothing was contained")
	require.NoError(t, session.autonomousFailureError())

	transport.queryMsgs = []map[string]any{resultFrame()}
	_, err = session.Prompt(t.Context(), lifecyclePromptRequest(session.id, "after-turn", "hello again"))
	require.NoError(t, err, "the session keeps serving prompts after the stacked notifications")
}

func TestBackgroundTaskFramesKeepPromptOriginWhileAnotherPromptRuns(t *testing.T) {
	session, transport, conn, cleanup := newNegotiatedPromptFlowSession(t)
	defer cleanup()
	session.rawMessages = rawMessageConfig{All: true}

	const (
		routeA = "origin-a"
		routeB = "current-b"
	)
	transport.queryMsgs = []map[string]any{taskStartedFrame(), resultFrame()}
	_, err := session.Prompt(t.Context(), lifecyclePromptRequest(session.id, routeA, "start task"))
	require.NoError(t, err)

	transport.queryMsgs = nil
	beforeB := len(conn.Updates())
	bDone := make(chan error, 1)
	go func() {
		_, promptErr := session.Prompt(t.Context(), lifecyclePromptRequest(session.id, routeB, "foreground b"))
		bDone <- promptErr
	}()
	awaitAgentUpdates(t, conn, func() bool {
		_, _, found := findLifecycleEvent(t, conn, beforeB,
			transitionMatcher(lifecycle.ForegroundRunning, lifecycle.CauseSubmission))

		return found
	})

	beforeTaskEnd := len(conn.Updates())
	beforeRaw := len(conn.Extensions())
	parentAssistant := assistantFrame("parent-tagged-assistant")
	parentAssistant["uuid"] = "parent-assistant-a"
	parentAssistant["parent_tool_use_id"] = "tool-1"
	parentStream := map[string]any{
		"type":               "stream_event",
		"uuid":               "parent-stream-a",
		"parent_tool_use_id": "tool-1",
		"event": map[string]any{
			"type":  streamEventMessageDelta,
			"usage": map[string]any{"output_tokens": 9},
		},
	}
	parentUser := toolResultFrame("parent-tagged-user")
	parentUser["uuid"] = "parent-user-a"
	parentUser["parent_tool_use_id"] = "tool-1"
	parentMirror := map[string]any{
		"type":     claude.MessageTypeMirror,
		"filePath": "/native/session/subagents/agent-a.jsonl",
		"entries": []any{
			map[string]any{"type": "assistant", "uuid": "parent-assistant-a"},
		},
	}
	taskResult := taskNotificationResultFrame()
	taskResult["total_cost_usd"] = 1.25
	pushNativeFrames(transport,
		taskNotificationFrame(), parentAssistant, parentStream, parentUser, parentMirror, taskResult,
	)

	awaitAgentUpdates(t, conn, func() bool {
		for _, notification := range conn.Updates()[beforeTaskEnd:] {
			if notification.Update.UsageUpdate != nil && notification.Update.UsageUpdate.Cost != nil {
				return true
			}
		}

		return false
	})

	// Every frame the task identifies retains A. The task-notification result is
	// the one frame that identifies nothing — the harness's origin is a kind
	// alone — so its usage and cost ride the foreground that read it.
	for _, notification := range conn.Updates()[beforeTaskEnd:] {
		update := notification.Update
		if update.ToolCallUpdate == nil && update.UsageUpdate == nil && update.AgentMessageChunk == nil {
			continue
		}

		route := requireAnyMap(t, notification.Meta[routeMetaKey])
		if update.UsageUpdate != nil && update.UsageUpdate.Cost != nil {
			require.Equal(t, routeB, route[routeFieldTurn], "the id-less result rides the foreground that read it")

			continue
		}

		require.Equal(t, routeA, route[routeFieldTurn], "task typed state and usage retain A")
		require.NotEqual(t, routeB, route[routeFieldTurn])
	}

	rawEvents := decodeRawEvents(t, conn)
	require.GreaterOrEqual(t, len(rawEvents), beforeRaw+6)

	resultRawSeen := false

	for _, event := range rawEvents[beforeRaw:] {
		route := requireAnyMap(t, event.Meta[routeMetaKey])
		if payload, ok := event.Event["type"].(string); ok && payload == claude.MessageTypeResult {
			resultRawSeen = true

			require.Equal(t, routeB, route[routeFieldTurn], "the id-less result raw frame rides the foreground")

			continue
		}

		require.Equal(t, routeA, route[routeFieldTurn], "task raw frames retain A")
	}

	require.True(t, resultRawSeen, "the task-notification result raw frame reaches the host")

	select {
	case err := <-bDone:
		require.NoError(t, err)
		t.Fatal("a task-notification result ended B")
	default:
	}

	pushNativeFrames(transport, resultFrame())
	require.NoError(t, <-bDone)
}

func TestTaskStartedInPromptWaitsForOrdinaryNativeTerminal(t *testing.T) {
	session, transport, conn, cleanup := newNegotiatedPromptFlowSession(t)
	defer cleanup()

	const route = "task-owner-b"
	transport.queryMsgs = nil
	before := len(conn.Updates())
	promptDone := make(chan error, 1)
	go func() {
		_, promptErr := session.Prompt(t.Context(), lifecyclePromptRequest(session.id, route, "start task"))
		promptDone <- promptErr
	}()

	waitForUpdate := func(match func([]acp.SessionNotification) bool) {
		t.Helper()

		for !match(conn.Updates()[before:]) {
			select {
			case <-conn.UpdatesChanged():
			case <-t.Context().Done():
				t.Fatal("context ended while waiting for the deterministic update barrier")
			}
		}
	}
	waitForUpdate(func(_ []acp.SessionNotification) bool {
		_, _, found := findLifecycleEvent(t, conn, before,
			transitionMatcher(lifecycle.ForegroundRunning, lifecycle.CauseSubmission))

		return found
	})

	pushNativeFrames(transport, taskStartedFrame(), taskNotificationFrame(), taskNotificationResultFrame())
	waitForUpdate(func(updates []acp.SessionNotification) bool {
		for _, notification := range updates {
			if notification.Update.UsageUpdate != nil {
				return true
			}
		}

		return false
	})

	for _, notification := range conn.Updates()[before:] {
		if notification.Update.ToolCall == nil && notification.Update.ToolCallUpdate == nil &&
			notification.Update.UsageUpdate == nil {
			continue
		}

		meta := requireAnyMap(t, notification.Meta[routeMetaKey])
		require.Equal(t, route, meta[routeFieldTurn])
	}

	select {
	case promptErr := <-promptDone:
		require.NoError(t, promptErr)
		t.Fatal("the task terminal notification finished its owning prompt")
	default:
	}

	pushNativeFrames(transport, resultFrame())
	require.NoError(t, <-promptDone)
}

func countLifecycleEvents(
	t *testing.T,
	conn *recordingAgentClient,
	from int,
	match func(map[string]any) bool,
) int {
	t.Helper()

	count := 0

	for _, entry := range lifecycleNotificationEvents(t, conn) {
		if entry.index >= from && match(entry.event) {
			count++
		}
	}

	return count
}

// recordingPermissionClient records the permission requests the adapter sent, so
// a test can read the lifecycle correlation the request actually carried.
type recordingPermissionClient struct {
	*recordingAgentClient

	mu       sync.Mutex
	requests []acp.RequestPermissionRequest
}

func (c *recordingPermissionClient) RequestPermission(
	ctx context.Context,
	request acp.RequestPermissionRequest,
	action actionWireAdmission,
) (acp.RequestPermissionResponse, error) {
	c.mu.Lock()
	c.requests = append(c.requests, request)
	c.mu.Unlock()

	return c.recordingAgentClient.RequestPermission(ctx, request, action)
}

func (c *recordingPermissionClient) Requests() []acp.RequestPermissionRequest {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]acp.RequestPermissionRequest(nil), c.requests...)
}

// TestPostTurnPermissionOwnsTheAgentTurnItOpened proves the control-callback
// half: a permission the harness raises after the prompt returned reaches the
// host, opens the excursion that owns it, and blocks that excursion's cycle
// rather than borrowing the route of the prompt that is over.
func TestPostTurnPermissionOwnsTheAgentTurnItOpened(t *testing.T) {
	session, transport, conn, cleanup := newNegotiatedPromptFlowSession(t)
	defer cleanup()

	permissions := &recordingPermissionClient{recordingAgentClient: conn}
	permissions.permission = permissionAllowOnce
	session.agent.setConnection(permissions)

	_, err := session.Prompt(t.Context(), lifecyclePromptRequest(session.id, "test-turn", "hello"))
	require.NoError(t, err)

	beforeExcursion := len(conn.Updates())

	// The route the prompt bound has been handed back to the incarnation's own,
	// so the callback the harness raises now is admitted through real ownership.
	route := session.autonomousRoute()
	require.NotEmpty(t, route)

	decision, err := handlePermissionThroughAdmissionForTest(t, session, t.Context(), route, claude.PermissionRequest{
		ToolName:  "Write",
		ToolUseID: "tool-write",
		Input:     map[string]any{"file_path": "/tmp/x", "content": "y"},
	})
	require.NoError(t, err)
	require.Equal(t, claude.BehaviorAllow, decision.Behavior)

	requests := permissions.Requests()
	require.Len(t, requests, 1, "the callback reaches the ACP host")

	running, runningIndex, found := findLifecycleEvent(t, conn, beforeExcursion,
		transitionMatcher(lifecycle.ForegroundRunning, lifecycle.CauseActivity))
	require.True(t, found, "a callback for work nobody submitted opens the turn that owns it")

	agentTurnID, _ := running["turnId"].(string)
	require.NotEmpty(t, agentTurnID)

	pending, pendingIndex, found := findLifecycleEvent(t, conn, runningIndex, func(event map[string]any) bool {
		if event["type"] != string(lifecycle.EventActionUpdate) {
			return false
		}

		return requireAnyMap(t, event["action"])["state"] == string(lifecycle.ActionPending)
	})
	require.True(t, found)
	require.Greater(t, pendingIndex, runningIndex, "the turn opens before the action it holds")

	action := requireAnyMap(t, pending["action"])
	require.Equal(t, "permission", action["kind"])
	require.Equal(t, true, action["blocksForeground"])
	require.Equal(t, map[string]any{"type": "turn", "id": agentTurnID}, requireAnyMap(t, action["owner"]))

	correlation := requireAnyMap(t, requests[0].Meta[lifecycle.MetaKey])
	require.Equal(t, map[string]any{"type": "turn", "id": agentTurnID},
		requireAnyMap(t, requireAnyMap(t, correlation["action"])["owner"]),
		"the request names the turn that owns it")

	_, blockedIndex, found := findLifecycleEvent(t, conn, pendingIndex,
		transitionMatcher(lifecycle.ForegroundRequiresAction, lifecycle.CauseActivity))
	require.True(t, found, "the blocked cycle is the excursion's own")
	require.Greater(t, blockedIndex, pendingIndex)

	contentIndex, found := pendingToolCallNotificationIndex(conn, beforeExcursion, "tool-write")
	require.True(t, found, "the pending permission content reaches the host")
	require.Less(t, contentIndex, pendingIndex,
		"ordinary pending tool content precedes lifecycle action visibility")

	_, releasedIndex, found := findLifecycleEvent(t, conn, blockedIndex,
		transitionMatcher(lifecycle.ForegroundRunning, lifecycle.CauseActivity))
	require.True(t, found, "answering the last blocker releases the cycle")
	require.Greater(t, releasedIndex, blockedIndex)

	// The frames that follow belong to the turn the callback already opened, and
	// the native result settles that same turn once.
	pushNativeFrames(transport, assistantFrame(autonomousAssistantSentinel), resultFrame())
	awaitLifecycleEvent(t, conn, transitionMatcher(lifecycle.ForegroundIdle, lifecycle.CauseActivity))

	idle, _, found := findLifecycleEvent(t, conn, releasedIndex,
		transitionMatcher(lifecycle.ForegroundIdle, lifecycle.CauseActivity))
	require.True(t, found)
	require.Equal(t, agentTurnID, idle["turnId"], "the pump adopts the turn the callback opened")
	require.Equal(t, []string{agentTurnID}, agentTurnIDs(t, conn, beforeExcursion),
		"the callback and the frames that followed it are one excursion")
}

// TestPostTurnElicitationAnnouncesOwnershipBeforeContent pins the same ordering
// for native MCP elicitation: an autonomous callback opens and blocks its own
// turn before the pending tool rendering is published, then its native terminal
// settles that exact excursion.
func TestPostTurnElicitationAnnouncesOwnershipBeforeContent(t *testing.T) {
	session, transport, conn, cleanup := newNegotiatedPromptFlowSession(t)
	defer cleanup()

	session.agent.clientCapabilities.Elicitation = &acp.ElicitationCapabilities{
		Form: &acp.ElicitationFormCapabilities{},
	}

	_, err := session.Prompt(t.Context(), lifecyclePromptRequest(session.id, "first-turn", "hello"))
	require.NoError(t, err)

	beforeExcursion := len(conn.Updates())
	route := session.autonomousRoute()
	require.NotEmpty(t, route)

	response, err := handleElicitationThroughAdmissionForTest(t, session, t.Context(), route, claude.ElicitationRequest{
		Mode:            claude.ElicitationModeForm,
		ToolUseID:       "tool-elicit",
		Message:         "Choose",
		RequestedSchema: map[string]any{"type": "object", "properties": map[string]any{}},
	})
	require.NoError(t, err)
	require.Equal(t, claude.ElicitationActionAccept, response.Action)

	running, runningIndex, found := findLifecycleEvent(t, conn, beforeExcursion,
		transitionMatcher(lifecycle.ForegroundRunning, lifecycle.CauseActivity))
	require.True(t, found)
	agentTurnID, _ := running["turnId"].(string)
	require.NotEmpty(t, agentTurnID)

	pending, pendingIndex, found := findLifecycleEvent(t, conn, runningIndex, func(event map[string]any) bool {
		if event["type"] != string(lifecycle.EventActionUpdate) {
			return false
		}

		action := requireAnyMap(t, event["action"])

		return action["kind"] == string(lifecycle.ActionElicitation) &&
			action["state"] == string(lifecycle.ActionPending)
	})
	require.True(t, found)
	require.Equal(t, map[string]any{"type": "turn", "id": agentTurnID},
		requireAnyMap(t, requireAnyMap(t, pending["action"])["owner"]))

	_, _, found = findLifecycleEvent(t, conn, pendingIndex,
		transitionMatcher(lifecycle.ForegroundRequiresAction, lifecycle.CauseActivity))
	require.True(t, found)

	contentIndex, found := pendingToolCallNotificationIndex(conn, beforeExcursion, "tool-elicit")
	require.True(t, found)
	require.Less(t, contentIndex, pendingIndex,
		"ordinary pending tool content precedes lifecycle action visibility")

	pushNativeFrames(transport, resultFrame())
	awaitLifecycleEvent(t, conn, transitionMatcher(lifecycle.ForegroundIdle, lifecycle.CauseActivity))

	idle, _, found := findLifecycleEvent(t, conn, contentIndex,
		transitionMatcher(lifecycle.ForegroundIdle, lifecycle.CauseActivity))
	require.True(t, found)
	require.Equal(t, agentTurnID, idle["turnId"])
}

// agentTurnIDs reports the distinct turns every agent-origin transition named,
// which is how many excursions the stream actually opened. A blocking action
// resolving emits a running transition of its own, so counting transitions counts
// the wrong thing.
func agentTurnIDs(t *testing.T, conn *recordingAgentClient, from int) []string {
	t.Helper()

	seen := map[string]struct{}{}
	turns := []string{}

	for _, entry := range lifecycleNotificationEvents(t, conn) {
		if entry.index < from || entry.event["cause"] != string(lifecycle.CauseActivity) {
			continue
		}

		turnID, _ := entry.event["turnId"].(string)
		if _, already := seen[turnID]; turnID == "" || already {
			continue
		}

		seen[turnID] = struct{}{}
		turns = append(turns, turnID)
	}

	return turns
}

// TestStaleCallbackRouteFailsClosed pins the residual case: a callback carrying
// a route no live owner answers for announces no action, emits no content and
// reaches no host request surface.
func TestStaleCallbackRouteFailsClosed(t *testing.T) {
	session, _, conn, cleanup := newNegotiatedPromptFlowSession(t)
	defer cleanup()

	permissions := &recordingPermissionClient{recordingAgentClient: conn}
	permissions.permission = permissionAllowOnce
	session.agent.setConnection(permissions)

	_, err := session.Prompt(t.Context(), lifecyclePromptRequest(session.id, "test-turn", "hello"))
	require.NoError(t, err)

	beforeCallback := len(conn.Updates())

	_, finish, admitted := session.admitControlCallback(t.Context(), "a-route-nothing-holds")
	require.False(t, admitted)
	finish()

	require.Zero(t, countLifecycleEvents(t, conn, beforeCallback, func(map[string]any) bool { return true }),
		"a callback with no owner announces nothing")

	require.Empty(t, permissions.Requests())
	_, found := pendingToolCallNotificationIndex(conn, beforeCallback, "tool-stale")
	require.False(t, found)
}

// TestPromptDuringAnOpenExcursionIsRefused proves the handoff at the other
// boundary. A prompt may run while background activity is live, but an
// agent-origin turn holding the foreground is not background: the prompt is
// refused before it is attached or dispatched, the excursion stays open and
// owned, and the retry after its native terminal proceeds as a turn of its own.
func TestPromptDuringAnOpenExcursionIsRefused(t *testing.T) {
	session, transport, conn, cleanup := newNegotiatedPromptFlowSession(t)
	defer cleanup()

	_, err := session.Prompt(t.Context(), lifecyclePromptRequest(session.id, "first-turn", "hello"))
	require.NoError(t, err)

	beforeExcursion := len(conn.Updates())

	// No result follows, so the excursion is still open when the next prompt
	// arrives.
	pushNativeFrames(transport, assistantFrame(autonomousAssistantSentinel))
	awaitLifecycleEvent(t, conn, transitionMatcher(lifecycle.ForegroundRunning, lifecycle.CauseActivity))

	running, runningIndex, _ := findLifecycleEvent(t, conn, beforeExcursion,
		transitionMatcher(lifecycle.ForegroundRunning, lifecycle.CauseActivity))
	agentTurnID, _ := running["turnId"].(string)
	require.NotEmpty(t, agentTurnID)

	beforeRefusal := len(conn.Updates())
	dispatchedFrames := sentUserFrames(transport)

	_, err = session.Prompt(t.Context(), lifecyclePromptRequest(session.id, "second-turn", "again"))
	requireForegroundBusy(t, err)

	require.Equal(t, dispatchedFrames, sentUserFrames(transport),
		"a refused prompt writes no frame to the harness")
	require.Zero(t, countLifecycleEvents(t, conn, beforeRefusal, func(event map[string]any) bool {
		return event["type"] == string(lifecycle.EventPromptAccepted)
	}), "a refused prompt is never accepted")
	require.Zero(t, countLifecycleEvents(t, conn, runningIndex,
		transitionMatcher(lifecycle.ForegroundIdle, lifecycle.CauseActivity)),
		"the excursion the prompt found open is left open")

	// The excursion's own native terminal settles it, and only then does a retry
	// take the foreground.
	pushNativeFrames(transport, resultFrame())
	awaitLifecycleEvent(t, conn, transitionMatcher(lifecycle.ForegroundIdle, lifecycle.CauseActivity))

	idle, idleIndex, found := findLifecycleEvent(t, conn, runningIndex,
		transitionMatcher(lifecycle.ForegroundIdle, lifecycle.CauseActivity))
	require.True(t, found)
	require.Equal(t, agentTurnID, idle["turnId"])
	require.Equal(t, "success", idle["outcome"])

	_, err = session.Prompt(t.Context(), lifecyclePromptRequest(session.id, "third-turn", "again"))
	require.NoError(t, err, "a retry after the excursion settles proceeds")

	accepted, acceptedIndex, found := findLifecycleEvent(t, conn, idleIndex, func(event map[string]any) bool {
		return event["type"] == string(lifecycle.EventPromptAccepted)
	})
	require.True(t, found)
	require.Greater(t, acceptedIndex, idleIndex)
	require.NotEqual(t, agentTurnID, accepted["turnId"], "the retry is a turn of its own")

	sentinelIndex, found := sentinelNotificationIndex(t, conn, autonomousAssistantSentinel)
	require.True(t, found)
	require.Less(t, sentinelIndex, acceptedIndex, "excursion content is never attributed to a later prompt")
}

// TestPromptDuringAnExcursionLeavesItsPendingActionOwned pins the half that made
// pre-emption wrong: the excursion the prompt is refused for may be blocked on a
// permission nobody has answered. Refusing the prompt leaves that action pending
// and owned, and the host's eventual answer still releases the cycle it blocked.
func TestPromptDuringAnExcursionLeavesItsPendingActionOwned(t *testing.T) {
	session, transport, conn, cleanup := newNegotiatedPromptFlowSession(t)
	defer cleanup()

	permissions := newBlockingPermissionClient(conn)
	session.agent.setConnection(permissions)

	_, err := session.Prompt(t.Context(), lifecyclePromptRequest(session.id, "first-turn", "hello"))
	require.NoError(t, err)

	beforeExcursion := len(conn.Updates())
	route := session.autonomousRoute()
	require.NotEmpty(t, route)

	decisions := make(chan claude.PermissionDecision, 1)

	go func() {
		decision, permissionErr := handlePermissionThroughAdmissionForTest(t, session, context.Background(), route,
			claude.PermissionRequest{
				ToolName:  "Write",
				ToolUseID: "tool-write",
				Input:     map[string]any{"file_path": "/tmp/x", "content": "y"},
			})
		require.NoError(t, permissionErr)
		decisions <- decision
	}()

	<-permissions.entered
	awaitLifecycleEvent(t, conn, transitionMatcher(lifecycle.ForegroundRequiresAction, lifecycle.CauseActivity))

	blocked, blockedIndex, _ := findLifecycleEvent(t, conn, beforeExcursion,
		transitionMatcher(lifecycle.ForegroundRequiresAction, lifecycle.CauseActivity))
	agentTurnID, _ := blocked["turnId"].(string)
	require.NotEmpty(t, agentTurnID)

	_, err = session.Prompt(t.Context(), lifecyclePromptRequest(session.id, "second-turn", "again"))
	requireForegroundBusy(t, err)

	require.Zero(t, countLifecycleEvents(t, conn, blockedIndex, func(event map[string]any) bool {
		if event["type"] != string(lifecycle.EventActionUpdate) {
			return false
		}

		state, _ := requireAnyMap(t, event["action"])["state"].(string)

		return state != string(lifecycle.ActionPending)
	}), "a refused prompt terminalizes nobody's pending action")

	close(permissions.release)
	require.Equal(t, claude.BehaviorAllow, (<-decisions).Behavior)

	accepted, acceptedIndex, found := findLifecycleEvent(t, conn, blockedIndex, func(event map[string]any) bool {
		if event["type"] != string(lifecycle.EventActionUpdate) {
			return false
		}

		return requireAnyMap(t, event["action"])["state"] == string(lifecycle.ActionAccepted)
	})
	require.True(t, found, "the action the excursion owned is answered by its own host reply")
	require.Equal(t, map[string]any{"type": "turn", "id": agentTurnID},
		requireAnyMap(t, requireAnyMap(t, accepted["action"])["owner"]))

	_, releasedIndex, found := findLifecycleEvent(t, conn, acceptedIndex,
		transitionMatcher(lifecycle.ForegroundRunning, lifecycle.CauseActivity))
	require.True(t, found, "answering the last blocker releases the excursion's cycle")

	pushNativeFrames(transport, resultFrame())
	awaitLifecycleEvent(t, conn, transitionMatcher(lifecycle.ForegroundIdle, lifecycle.CauseActivity))

	idle, _, found := findLifecycleEvent(t, conn, releasedIndex,
		transitionMatcher(lifecycle.ForegroundIdle, lifecycle.CauseActivity))
	require.True(t, found)
	require.Equal(t, agentTurnID, idle["turnId"])

	_, err = session.Prompt(t.Context(), lifecyclePromptRequest(session.id, "third-turn", "again"))
	require.NoError(t, err)
}

// TestCloseTerminalizesOpenAutonomousState proves close settles the autonomous
// foreground it owns before publishing quiescence and fencing the stream.
func TestCloseTerminalizesOpenAutonomousState(t *testing.T) {
	ctx := t.Context()
	session, conn, stream := newAuthoritativeLifecycleStreamTestSession(t)
	session.turn = make(chan struct{}, sessionTurnCapacity)
	session.client = claude.NewClient(session.agent.log, claude.Options{}, newFakeClaudeTransport())
	require.NoError(t, session.client.Start(ctx))

	require.NoError(t, stream.incarnate(ctx))

	turnID, err := stream.openAgentTurn(ctx, "autonomous-route")
	require.NoError(t, err)
	require.NotEmpty(t, turnID)

	require.NoError(t, session.Close(ctx))

	idle, idleIndex, found := findLifecycleEvent(t, conn, 0,
		transitionMatcher(lifecycle.ForegroundIdle, lifecycle.CauseActivity))
	require.True(t, found, "the open agent-origin turn is settled by the close")
	require.Equal(t, turnID, idle["turnId"])
	require.Equal(t, "cancelled", idle["outcome"])

	_, quiescenceIndex, found := findLifecycleEvent(t, conn, idleIndex, func(event map[string]any) bool {
		return event["type"] == string(lifecycle.EventQuiescenceUpdate)
	})
	require.True(t, found, "a boundary with nothing live behind it states its fact")
	require.Greater(t, quiescenceIndex, idleIndex)
}

// TestExcursionAndPromptNeverShareTheForeground drives frames at the reader while
// prompts run, which is the shape the race detector is pointed at: the foreground
// is one owner at a time, so the stream's own reducer never sees two open turns.
func TestExcursionAndPromptNeverShareTheForeground(t *testing.T) {
	session, transport, conn, cleanup := newNegotiatedPromptFlowSession(t)
	defer cleanup()

	_, err := session.Prompt(t.Context(), lifecyclePromptRequest(session.id, "warm-turn", "hello"))
	require.NoError(t, err)

	done := make(chan struct{})

	go func() {
		defer close(done)

		for range 8 {
			pushNativeFrames(transport, assistantFrame(autonomousAssistantSentinel), resultFrame())
		}
	}()

	for turn := range 4 {
		_, err := session.Prompt(t.Context(),
			lifecyclePromptRequest(session.id, "race-turn-"+string(rune('a'+turn)), "hello"))
		if err != nil {
			// A prompt that lands while an excursion holds the foreground is
			// refused as busy rather than pre-empting it. That is the only failure
			// this race may produce, and it is retryable by construction.
			requireForegroundBusy(t, err)
		}
	}

	<-done

	// Every emission passes the same reducer the canonical vectors drive, so a
	// foreground two turns claimed would have failed the emission that claimed it.
	// The stream is still live and still emitting, which is the assertion.
	require.NoError(t, session.lifecycleStream().incarnate(t.Context()))
	require.NotEmpty(t, lifecycleNotificationEvents(t, conn))
}

type selectiveUpdateFailureClient struct {
	*recordingAgentClient
	err  error
	fail func(acp.SessionNotification) bool
}

func (c *selectiveUpdateFailureClient) SessionUpdate(
	ctx context.Context,
	notification acp.SessionNotification,
) error {
	if c.fail(notification) {
		return c.err
	}

	return c.recordingAgentClient.SessionUpdate(ctx, notification)
}

func openAutonomousMapTestExcursion(
	t *testing.T,
) (*agentSession, *recordingAgentClient, *sessionStream, string) {
	t.Helper()

	session, conn, stream := newLifecycleStreamTestSession(t)
	require.NoError(t, stream.incarnate(t.Context()))
	route := "autonomous-map-route"
	session.setAutonomousRoute(route, nil)
	turnID, err := stream.openAgentTurn(t.Context(), route)
	require.NoError(t, err)
	require.NotEmpty(t, turnID)
	session.excursion = &agentExcursion{turnID: turnID}

	return session, conn, stream, route
}

func pendingToolCallNotificationIndex(
	conn *recordingAgentClient,
	from int,
	toolCallID acp.ToolCallId,
) (int, bool) {
	for index, notification := range conn.Updates() {
		if index < from || notification.Update.ToolCall == nil {
			continue
		}
		if notification.Update.ToolCall.ToolCallId == toolCallID {
			return index, true
		}
	}

	return 0, false
}

func TestCancelAndCloseContainWrittenPermissionAndElicitation(t *testing.T) {
	for _, operation := range []struct {
		name string
		run  func(*agentSession) error
	}{
		{name: "cancel", run: func(session *agentSession) error { return session.Cancel(context.Background()) }},
		{name: "close", run: func(session *agentSession) error { return session.Close(context.Background()) }},
	} {
		for _, action := range []struct {
			name     string
			kind     lifecycle.ActionKind
			entered  func(*blockingCallbackClient) <-chan struct{}
			canceled func(*blockingCallbackClient) <-chan struct{}
		}{
			{name: "permission", kind: lifecycle.ActionPermission,
				entered:  func(c *blockingCallbackClient) <-chan struct{} { return c.permissionEntered },
				canceled: func(c *blockingCallbackClient) <-chan struct{} { return c.permissionCanceled }},
			{name: "elicitation", kind: lifecycle.ActionElicitation,
				entered:  func(c *blockingCallbackClient) <-chan struct{} { return c.elicitationEntered },
				canceled: func(c *blockingCallbackClient) <-chan struct{} { return c.elicitationCanceled }},
		} {
			t.Run(operation.name+"/"+action.name, func(t *testing.T) {
				session, transport, conn, cleanup := newNegotiatedPromptFlowSession(t)
				defer cleanup()

				session.agent.clientCapabilities.Elicitation = &acp.ElicitationCapabilities{
					Form: &acp.ElicitationFormCapabilities{},
				}
				_, err := session.Prompt(t.Context(), lifecyclePromptRequest(session.id, "first-turn", "hello"))
				require.NoError(t, err)

				callbacks := newBlockingCallbackClient(conn)
				session.agent.setConnection(callbacks)
				pushControlCallback(transport, action.kind, operation.name+"-"+action.name, "tool-"+operation.name+"-"+action.name)
				<-action.entered(callbacks)

				done := make(chan error, 1)
				go func() { done <- operation.run(session) }()
				<-action.canceled(callbacks)
				require.NoError(t, <-done)

				session.mu.Lock()
				require.Empty(t, session.permissionCancel)
				require.Empty(t, session.elicitationCancel)
				session.mu.Unlock()
				if action.kind == lifecycle.ActionPermission {
					require.Len(t, callbacks.Requests(), 1, "the exact permission request crossed its write boundary")
				} else {
					require.Len(t, callbacks.Elicitations(), 1, "the exact elicitation request crossed its write boundary")
				}
			})
		}
	}
}

// TestAutonomousSystemIdleFrameSettlesBehindItsOwnContent proves an ordinary
// native idle is the excursion terminal and remains ordered behind every update
// carried by that same frame sequence.
func TestAutonomousSystemIdleFrameSettlesBehindItsOwnContent(t *testing.T) {
	session, transport, conn, cleanup := newNegotiatedPromptFlowSession(t)
	defer cleanup()

	transport.queryMsgs = []map[string]any{taskStartedFrame(), resultFrame()}

	_, err := session.Prompt(t.Context(), lifecyclePromptRequest(session.id, "first-turn", "hello"))
	require.NoError(t, err)

	beforeExcursion := len(conn.Updates())
	transport.queryMsgs = []map[string]any{resultFrame()}

	pushNativeFrames(transport,
		namedTaskNotificationFrame("task-1"),
		assistantFrame(autonomousAssistantSentinel),
		systemIdleFrame(),
	)

	awaitLifecycleEvent(t, conn, transitionMatcher(lifecycle.ForegroundIdle, lifecycle.CauseActivity))

	idle, idleIndex, found := findLifecycleEvent(t, conn, beforeExcursion,
		transitionMatcher(lifecycle.ForegroundIdle, lifecycle.CauseActivity))
	require.True(t, found, "a state frame that idles the harness settles the excursion")
	require.Equal(t, "success", idle["outcome"])
	require.Equal(t, "end_turn", idle["stopReason"])
	require.Equal(t, 1, countLifecycleEvents(t, conn, beforeExcursion,
		transitionMatcher(lifecycle.ForegroundIdle, lifecycle.CauseActivity)))

	sentinelIndex, found := sentinelNotificationIndex(t, conn, autonomousAssistantSentinel)
	require.True(t, found)
	require.Less(t, sentinelIndex, idleIndex, "the excursion's content precedes its terminal idle")

	// Nothing the excursion owned lands behind its own end.
	require.Equal(t, idleIndex, len(conn.Updates())-1,
		"the terminal idle is the excursion's last word")
}

// TestTaskNotificationResultTerminalizesTheDelayedExcursion is the monitor
// incident: a prompt arms a background task and settles; the task fires later;
// the harness wakes, narrates, and closes that autonomous turn with a result
// whose origin is `{"kind":"task-notification"}` — and then emits nothing else.
// That result must settle the excursion: the narration ends in a terminal idle
// with a success outcome, and the next prompt runs instead of being refused by
// a foreground nothing will ever release.
func TestTaskNotificationResultTerminalizesTheDelayedExcursion(t *testing.T) {
	session, transport, conn, cleanup := newNegotiatedPromptFlowSession(t)
	defer cleanup()

	transport.queryMsgs = []map[string]any{taskStartedFrame(), resultFrame()}

	_, err := session.Prompt(t.Context(), lifecyclePromptRequest(session.id, "arming-turn", "arm the monitor"))
	require.NoError(t, err)

	beforeExcursion := len(conn.Updates())

	pushNativeFrames(transport,
		taskNotificationFrame(),
		assistantFrame(autonomousAssistantSentinel),
		taskNotificationResultFrame(),
	)

	awaitLifecycleEvent(t, conn, transitionMatcher(lifecycle.ForegroundIdle, lifecycle.CauseActivity))

	idle, idleIndex, found := findLifecycleEvent(t, conn, beforeExcursion,
		transitionMatcher(lifecycle.ForegroundIdle, lifecycle.CauseActivity))
	require.True(t, found, "the task-notification result settles the excursion")
	require.Equal(t, "success", idle["outcome"])
	require.Equal(t, "end_turn", idle["stopReason"])
	require.Equal(t, 1, countLifecycleEvents(t, conn, beforeExcursion,
		transitionMatcher(lifecycle.ForegroundIdle, lifecycle.CauseActivity)))

	sentinelIndex, found := sentinelNotificationIndex(t, conn, autonomousAssistantSentinel)
	require.True(t, found)
	require.Less(t, sentinelIndex, idleIndex, "the narration precedes its terminal idle")

	// The foreground is free again: the next prompt runs instead of being
	// refused with a session_foreground backpressure error.
	transport.queryMsgs = []map[string]any{resultFrame()}
	_, err = session.Prompt(t.Context(), lifecyclePromptRequest(session.id, "follow-up", "and now?"))
	require.NoError(t, err, "prompt C proceeds once the delayed excursion settled")
}

func TestSessionCloseJoinsQueuedAutonomousRetirementWorker(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	session, transport, _, cleanup := newNegotiatedPromptFlowSession(t)
	defer cleanup()
	require.NoError(t, session.serveNativePump(t.Context(), session.currentClient()))

	incarnation := session.currentNativeIncarnation()
	require.NotNil(t, incarnation)

	// The worker owns a producer admission before it starts its goroutine.
	// Parking it on the exact serve lock makes its queued lifetime deterministic.
	session.pumpServeMu.Lock()
	serveLocked := true
	defer func() {
		if serveLocked {
			session.pumpServeMu.Unlock()
		}
	}()
	session.failNativeIncarnation(t.Context(), incarnation, errNativeReceiveExited, "test_barrier")
	session.beginClose()

	closeStarted := make(chan struct{})
	closeDone := make(chan error, 1)
	go func() {
		close(closeStarted)
		closeDone <- session.Close(t.Context())
	}()
	<-closeStarted

	select {
	case closeErr := <-closeDone:
		require.NoError(t, closeErr)
		t.Fatal("session close returned with its retirement worker still queued")
	default:
	}
	require.Zero(t, transport.CloseCalls(), "carrier teardown waits behind the queued worker")

	session.pumpServeMu.Unlock()
	serveLocked = false
	require.NoError(t, <-closeDone)
	require.Positive(t, transport.CloseCalls())
}

// TestStaleCallbackOnALatchedStreamFailsClosed pins the residual ordering inside
// the stream. Ownership is resolved before emittability, so a callback whose
// route names no live owner is refused for that reason before any action,
// ordinary content or host request can inherit an unrelated stream failure.
func TestStaleCallbackOnALatchedStreamFailsClosed(t *testing.T) {
	session, _, conn, cleanup := newNegotiatedPromptFlowSession(t)
	defer cleanup()

	permissions := &recordingPermissionClient{recordingAgentClient: conn}
	permissions.permission = permissionAllowOnce
	session.agent.setConnection(permissions)

	_, err := session.Prompt(t.Context(), lifecyclePromptRequest(session.id, "first-turn", "hello"))
	require.NoError(t, err)

	stream := session.lifecycleStream()
	stream.mu.Lock()
	stream.lost = lifecycleViolationError("an earlier emission never reached the host")
	stream.mu.Unlock()

	beforeCallback := len(conn.Updates())

	_, finish, admitted := session.admitControlCallback(t.Context(), "a-route-nothing-holds")
	require.False(t, admitted)
	finish()

	require.Zero(t, countLifecycleEvents(t, conn, beforeCallback, func(map[string]any) bool { return true }),
		"a callback with no owner announces nothing")

	require.Empty(t, permissions.Requests(), "a stale route reaches no host permission surface")

	// A route that really does name a live owner is entitled to the latch, and
	// reports it.
	_, err = handlePermissionThroughAdmissionForTest(t, session, t.Context(), session.autonomousRoute(),
		claude.PermissionRequest{ToolName: "Write", ToolUseID: "tool-owned", Input: map[string]any{}})
	require.Error(t, err, "the owner of a latched stream is told the stream is latched")
}

// TestRelaunchDuringAnExcursionRetiresIt covers the other race at the incarnation
// boundary: a replacement process arriving while an agent-origin turn is open
// retires that turn with the incarnation that owned it, and the session comes
// back with no excursion, a fresh route, and a prompt path that works.
func TestRelaunchDuringAnExcursionRetiresIt(t *testing.T) {
	session, transport, conn, cleanup := newNegotiatedPromptFlowSession(t)
	defer cleanup()

	_, err := session.Prompt(t.Context(), lifecyclePromptRequest(session.id, "first-turn", "hello"))
	require.NoError(t, err)

	beforeExcursion := len(conn.Updates())
	firstRoute := session.autonomousRoute()

	pushNativeFrames(transport, assistantFrame(autonomousAssistantSentinel))
	awaitLifecycleEvent(t, conn, transitionMatcher(lifecycle.ForegroundRunning, lifecycle.CauseActivity))

	running, runningIndex, _ := findLifecycleEvent(t, conn, beforeExcursion,
		transitionMatcher(lifecycle.ForegroundRunning, lifecycle.CauseActivity))
	agentTurnID, _ := running["turnId"].(string)
	require.NotEmpty(t, agentTurnID)

	replacementTransport := newFakeClaudeTransport()
	replacement := claude.NewClient(session.agent.log, claude.Options{}, replacementTransport)
	require.NoError(t, replacement.Start(t.Context()))
	defer func() { _ = replacement.Close() }()

	t.Cleanup(func() { _ = replacement.Close() })

	session.mu.Lock()
	session.client = replacement
	session.mu.Unlock()

	require.NoError(t, session.serveNativePump(t.Context(), replacement))

	idle, _, found := findLifecycleEvent(t, conn, runningIndex,
		transitionMatcher(lifecycle.ForegroundIdle, lifecycle.CauseActivity))
	require.True(t, found, "a retired incarnation does not leave an agent-origin turn open")
	require.Equal(t, agentTurnID, idle["turnId"])
	require.Equal(t, "failed", idle["outcome"])

	require.NotEqual(t, firstRoute, session.autonomousRoute(), "the route is retired with its incarnation")
	_, finishRetired, admitted := session.admitControlCallback(t.Context(), firstRoute)
	require.False(t, admitted)
	finishRetired()

	release := session.takeForeground()
	require.Nil(t, session.excursion, "the replacement inherits no excursion")
	release()

	_, err = session.Prompt(t.Context(), lifecyclePromptRequest(session.id, "second-turn", "again"))
	require.NoError(t, err, "the replacement incarnation serves prompts normally")
}

func TestAutonomousProjectionStageFailuresAreReturned(t *testing.T) {
	tests := []struct {
		name  string
		frame func(*agentSession) claude.Message
		fail  func(acp.SessionNotification) bool
	}{
		{
			name: "side effect",
			frame: func(*agentSession) claude.Message {
				return &claude.SystemMessage{Subtype: systemStatus, Raw: map[string]any{systemStatus: systemStatusCompacting}}
			},
			fail: func(notification acp.SessionNotification) bool {
				return notification.Update.AgentMessageChunk != nil
			},
		},
		{
			name: "hook response",
			frame: func(session *agentSession) claude.Message {
				options := session.sessionToolUpdateOptions()
				options.ToolUses["hook-tool"] = claude.ToolUseBlock{ID: "hook-tool", Name: "Edit"}

				return &claude.SystemMessage{Subtype: systemSubtypeHookResponse, Raw: map[string]any{
					systemHookEventName: systemHookPostToolUse,
					systemToolUseID:     "hook-tool",
					systemToolResponse: map[string]any{
						"filePath": "/tmp/file", "structuredPatch": []any{map[string]any{"lines": []any{"+x"}}},
					},
				}}
			},
			fail: func(notification acp.SessionNotification) bool {
				return notification.Update.ToolCallUpdate != nil
			},
		},
		{
			name: "typed content",
			frame: func(*agentSession) claude.Message {
				return &claude.AssistantMessage{Content: []claude.ContentBlock{
					claude.TextBlock{Text: "typed failure"},
				}}
			},
			fail: func(notification acp.SessionNotification) bool {
				return notification.Update.AgentMessageChunk != nil
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			session, conn, _, route := openAutonomousMapTestExcursion(t)
			failure := errors.New(test.name + " failed")
			session.agent.setConnection(&selectiveUpdateFailureClient{
				recordingAgentClient: conn,
				err:                  failure,
				fail:                 test.fail,
			})

			err := session.mapAutonomousFrame(withTurnRoute(t.Context(), route), test.frame(session))
			if test.name == "activity" {
				require.ErrorContains(t, err, "lifecycle delivery failed")
				require.NotContains(t, err.Error(), failure.Error())
			} else {
				require.ErrorContains(t, err, failure.Error())
			}
		})
	}
}

func TestCausalBackgroundProjectionReturnsEachOwnedStageFailure(t *testing.T) {
	tests := []struct {
		name  string
		frame func(*agentSession) claude.Message
		fail  func(acp.SessionNotification) bool
	}{
		{
			name: "side effect",
			frame: func(*agentSession) claude.Message {
				return &claude.SystemMessage{Subtype: systemStatus, Raw: map[string]any{systemStatus: systemStatusCompacting}}
			},
			fail: func(notification acp.SessionNotification) bool { return notification.Update.AgentMessageChunk != nil },
		},
		{
			name: "hook response",
			frame: func(session *agentSession) claude.Message {
				options := session.sessionToolUpdateOptions()
				options.ToolUses["hook-tool"] = claude.ToolUseBlock{ID: "hook-tool", Name: "Edit"}

				return &claude.SystemMessage{Subtype: systemSubtypeHookResponse, Raw: map[string]any{
					systemHookEventName: systemHookPostToolUse,
					systemToolUseID:     "hook-tool",
					systemToolResponse: map[string]any{
						"filePath": "/tmp/file", "structuredPatch": []any{map[string]any{"lines": []any{"+x"}}},
					},
				}}
			},
			fail: func(notification acp.SessionNotification) bool { return notification.Update.ToolCallUpdate != nil },
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			session, conn, _ := newLifecycleStreamTestSession(t)
			failure := errors.New(test.name + " failed")
			session.agent.setConnection(&selectiveUpdateFailureClient{
				recordingAgentClient: conn,
				err:                  failure,
				fail:                 test.fail,
			})

			require.Error(t, session.mapCausalBackgroundFrame(t.Context(), test.frame(session)))
		})
	}
}

func TestAutonomousExcursionOpeningAndUsageFailuresStayAtTheirExactStage(t *testing.T) {
	session, _, stream := newLifecycleStreamTestSession(t)
	require.NoError(t, stream.incarnate(t.Context()))
	stream.lost = errors.New("opening failed")
	err := session.mapAutonomousFrame(t.Context(), &claude.AssistantMessage{
		Content: []claude.ContentBlock{claude.TextBlock{Text: "work"}},
	})
	require.ErrorContains(t, err, "opening failed")

	session, conn, _, _ := openAutonomousMapTestExcursion(t)
	usageErr := errors.New("usage failed")
	session.agent.setConnection(&selectiveUpdateFailureClient{
		recordingAgentClient: conn,
		err:                  usageErr,
		fail: func(notification acp.SessionNotification) bool {
			return notification.Update.UsageUpdate != nil
		},
	})
	err = session.mapAutonomousFrame(t.Context(), &claude.StreamEventMessage{
		EventType: streamEventMessageStart,
		Event: map[string]any{"message": map[string]any{
			"model": "opus", "usage": map[string]any{"input_tokens": 1, "output_tokens": 2},
		}},
	})
	require.ErrorIs(t, err, usageErr)

	session, conn, _, _ = openAutonomousMapTestExcursion(t)
	session.agent.setConnection(&selectiveUpdateFailureClient{
		recordingAgentClient: conn,
		err:                  usageErr,
		fail: func(notification acp.SessionNotification) bool {
			return notification.Update.UsageUpdate != nil
		},
	})
	incarnation := &nativeIncarnation{lost: make(chan struct{}), mirrorReady: make(chan struct{})}
	incarnation.superviseOnce.Do(func() {})
	session.observeAutonomousFrame(t.Context(), incarnation, &claude.StreamEventMessage{
		EventType: streamEventMessageStart,
		Event: map[string]any{"message": map[string]any{
			"model": "opus", "usage": map[string]any{"input_tokens": 3},
		}},
	})
	require.True(t, incarnation.failed.Load())
}

func TestAutonomousFrameAndSettlementEdgeStates(t *testing.T) {
	session, conn, stream := newLifecycleStreamTestSession(t)
	incarnation := &nativeIncarnation{}
	otherIncarnation := &nativeIncarnation{}
	require.False(t, session.rotateAutonomousRoute(nil, "route"))
	require.False(t, session.rotateAutonomousRoute(incarnation, ""))
	session.setAutonomousRoute("route", incarnation)
	_, owned := session.autonomousRouteExact(nil)
	require.False(t, owned)
	session.clearAutonomousRoute(nil)
	session.clearAutonomousRoute(otherIncarnation)
	require.Equal(t, "route", session.autonomousRoute())
	require.False(t, session.rotateAutonomousRoute(otherIncarnation, "other"))
	session.observeAutonomousFrame(t.Context(), nil, &claude.AssistantMessage{})
	incarnation.failed.Store(true)
	session.observeAutonomousFrame(t.Context(), incarnation, &claude.AssistantMessage{})
	incarnation.failed.Store(false)
	session.failNativeIncarnation(t.Context(), nil, errors.New("failure"), "test")
	session.failNativeIncarnation(t.Context(), incarnation, nil, "test")
	session.failAutonomousFrame(t.Context(), nil, errors.New("failure"))
	sealedIncarnation := &nativeIncarnation{}
	session.producers.seal()
	session.failNativeIncarnation(t.Context(), sealedIncarnation, errors.New("sealed"), "test")
	require.True(t, sealedIncarnation.failed.Load())
	require.NoError(t, autonomousStreamError(nil))

	client := claude.NewClient(nil, claude.Options{}, newFakeClaudeTransport())
	session.client = client
	session.autonomousClient = client
	session.autonomousErr = errors.New("retained")
	session.clearAutonomousFailure(client)
	require.Error(t, session.autonomousErr)
	session.clearAutonomousFailure(claude.NewClient(nil, claude.Options{}, newFakeClaudeTransport()))
	require.NoError(t, session.autonomousErr)

	closed := autonomousStreamError(errors.New("provider-store-tool-user-secret"))
	require.Error(t, closed)
	require.NotContains(t, closed.Error(), "provider-store-tool-user-secret")
	require.NoError(t, session.mapAutonomousFrame(t.Context(), &claude.ResultMessage{}))
	require.NoError(t, session.mapAutonomousFrame(t.Context(), &claude.SystemMessage{Subtype: "unknown"}))
	require.Nil(t, session.excursion)

	require.True(t, autonomousFrameCarriesWork(nil, []acp.SessionUpdate{{}}))
	require.True(t, autonomousFrameCarriesWork(&claude.StreamEventMessage{
		EventType: streamEventMessageStart,
		Event:     map[string]any{"message": map[string]any{"usage": map[string]any{}}},
	}, nil))
	require.False(t, autonomousFrameCarriesWork(&claude.StreamEventMessage{EventType: "other"}, nil))
	require.False(t, autonomousFrameCarriesWork(&claude.AssistantMessage{}, nil))
	require.True(t, autonomousFrameCarriesWork(&claude.SystemMessage{Subtype: systemStatus}, nil))
	require.False(t, autonomousFrameCarriesWork(&claude.SystemMessage{Subtype: "unknown"}, nil))
	require.NoError(t, session.settleExcursion(t.Context(), nil, mapper.ToolUpdateOptions{}))

	session.excursion = &agentExcursion{}
	require.NoError(t, session.observeExcursionFrame(t.Context(), &claude.AssistantMessage{
		Model: syntheticModelName, MessageID: "assistant-id",
	}))
	require.Equal(t, "assistant-id", session.excursion.lastAssistantMessageID)
	require.Empty(t, session.excursion.lastAssistantModel)
	require.NoError(t, session.observeExcursionFrame(t.Context(), &claude.StreamEventMessage{ParentToolUseID: "parent"}))
	require.NoError(t, session.observeExcursionFrame(t.Context(), &claude.StreamEventMessage{
		EventType: streamEventMessageStart,
		Event: map[string]any{"message": map[string]any{
			"model": "opus", "usage": map[string]any{"input_tokens": 1, "output_tokens": 2},
		}},
	}))
	require.Equal(t, "opus", session.excursion.lastAssistantModel)
	require.True(t, session.excursion.streamKnown)

	taskSession, _, taskStream, _ := openAutonomousMapTestExcursion(t)
	require.NoError(t, taskSession.settleExcursion(t.Context(), &claude.ResultMessage{
		Origin: map[string]any{"kind": originKindTaskNotification},
	}, mapper.ToolUpdateOptions{}))
	require.Nil(t, taskSession.excursion,
		"a task-notification result is the excursion's native terminal")
	require.Empty(t, taskStream.agentTurnID())

	require.NoError(t, stream.incarnate(t.Context()))
	session.setAutonomousRoute("settlement-route", nil)
	turnID, err := stream.openAgentTurn(t.Context(), "settlement-route")
	require.NoError(t, err)
	session.excursion = &agentExcursion{turnID: turnID}
	failure := errors.New("result usage failed")
	session.agent.setConnection(&selectiveUpdateFailureClient{
		recordingAgentClient: conn,
		err:                  failure,
		fail: func(notification acp.SessionNotification) bool {
			return notification.Update.UsageUpdate != nil
		},
	})
	err = session.settleExcursion(t.Context(), &claude.ResultMessage{Usage: &claude.Usage{InputTokens: 1}}, mapper.ToolUpdateOptions{})
	require.ErrorIs(t, err, failure)

	session, conn, _, _ = openAutonomousMapTestExcursion(t)
	session.excursion.lastAssistantMessageID = "assistant-id"
	identityFailure := errors.New("identity failed")
	session.agent.setConnection(&selectiveUpdateFailureClient{
		recordingAgentClient: conn,
		err:                  identityFailure,
		fail: func(notification acp.SessionNotification) bool {
			_, lifecycleCarrier := notification.Meta[lifecycle.MetaKey]

			return notification.Update.SessionInfoUpdate != nil && !lifecycleCarrier
		},
	})
	require.ErrorIs(t, session.settleExcursion(t.Context(), nil, mapper.ToolUpdateOptions{}), identityFailure)

	session, _, _, commitRoute := openAutonomousMapTestExcursion(t)
	require.NotEmpty(t, commitRoute)
	commitFailure := errors.New("excursion commit failed")
	session.mirror = newSessionMirror(session.agent.log, &faultSessionStore{
		SessionStore: NewInMemorySessionStore(), appendErr: commitFailure,
	}, t.TempDir(), session)
	session.nativePumpHandle().recordCommitError(commitFailure)
	require.ErrorContains(t, session.settleExcursion(t.Context(), nil, mapper.ToolUpdateOptions{}), "session store commit failed")
}

func TestLoadRefusesAnExactContainedIncarnation(t *testing.T) {
	id := acp.SessionId("contained-load")
	cwd := t.TempDir()
	store := NewInMemorySessionStore()
	require.NoError(t, store.Append(t.Context(), SessionKey{SessionID: string(id)}, []SessionStoreEntry{
		[]byte(`{"type":"user"}`),
	}))
	agent := NewAgent(WithSessionStore(store))
	client := claude.NewClient(nil, claude.Options{}, newFakeClaudeTransport())
	session := &agentSession{
		agent: agent, id: id, cwd: cwd, client: client,
		autonomousErr: errors.New("contained"), autonomousClient: client,
	}
	session.fingerprint = sessionStartFingerprint(sessionStart{Cwd: cwd, ResumeID: string(id)})
	agent.sessions[id] = session

	_, err := agent.LoadSession(t.Context(), acp.LoadSessionRequest{SessionId: id, Cwd: cwd})
	require.ErrorContains(t, err, "claude_autonomous_stream_failed")
}

func TestContainedIncarnationCannotRetireItsReplacement(t *testing.T) {
	session, _, _, cleanup := newNegotiatedPromptFlowSession(t)
	defer cleanup()
	require.NoError(t, session.serveNativePump(t.Context(), session.currentClient()))
	incarnation := session.currentNativeIncarnation()
	pump := session.nativePumpHandle()

	session.pumpServeMu.Lock()
	session.failNativeIncarnation(t.Context(), incarnation, errors.New("projection failed"), "test")
	replacement := &nativeIncarnation{}
	pump.mu.Lock()
	pump.incarnation = replacement
	pump.mu.Unlock()
	session.pumpServeMu.Unlock()

	require.NoError(t, session.producers.closeAndWait(t.Context()))
	require.True(t, incarnation.failed.Load())
	require.Same(t, replacement, session.nativePumpHandle().incarnation)
}
