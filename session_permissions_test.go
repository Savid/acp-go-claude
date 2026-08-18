package claudeacp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"sync"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/savid/acp-go-claude/internal/lifecycle"
	"github.com/savid/acp-go-claude/internal/mapper"
	"github.com/stretchr/testify/require"
)

const permissionControlTurnNonce = "permission-turn-0123456789abcdef"

type blockingExitPlanClient struct {
	*recordingAgentClient
	entered chan struct{}
	release chan struct{}
}

func (c *blockingExitPlanClient) RequestPermission(
	ctx context.Context,
	_ acp.RequestPermissionRequest,
) (acp.RequestPermissionResponse, error) {
	close(c.entered)
	select {
	case <-c.release:
		return acp.RequestPermissionResponse{
			Outcome: acp.NewRequestPermissionOutcomeSelected(acp.PermissionOptionId(modeDefault)),
		}, nil
	case <-ctx.Done():
		return acp.RequestPermissionResponse{}, ctx.Err()
	}
}

func activatePermissionControlTurn(t *testing.T, session *agentSession, turnNonce string) context.Context {
	t.Helper()

	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	session.mu.Lock()
	session.cancel = cancel
	session.turnNonce = turnNonce
	session.mu.Unlock()

	return withTurnRoute(ctx, turnNonce)
}

func TestPermissionRulePersistenceHelpers(t *testing.T) {
	var savedRules map[string]string
	previousSave := savePermissionRules
	savePermissionRules = func(_ context.Context, _ string, _ acp.SessionId, rules map[string]string) error {
		savedRules = cloneStringMap(rules)

		return nil
	}
	t.Cleanup(func() { savePermissionRules = previousSave })

	agent := NewAgent()
	session := &agentSession{
		agent:           agent,
		id:              "session-1",
		permissionRules: map[string]string{"Read": claude.BehaviorAllow},
	}
	agent.sessions[session.id] = session

	rules, err := agent.permissionRulesForSession(context.Background(), session.id)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"Read": claude.BehaviorAllow}, rules)
	rules["Read"] = claude.BehaviorDeny
	got, ok := session.permissionRule("Read")
	require.True(t, ok)
	require.Equal(t, claude.BehaviorAllow, got)

	session.setPermissionRule(context.Background(), "", claude.BehaviorDeny)
	session.setPermissionRule(context.Background(), "Write", claude.BehaviorDeny)
	require.Equal(t, map[string]string{"Read": claude.BehaviorAllow, "Write": claude.BehaviorDeny}, savedRules)
	cached, ok := agent.cachedPermissionRules(session.id)
	require.True(t, ok)
	require.Equal(t, savedRules, cached)
	cached["Write"] = "changed"
	cached, _ = agent.cachedPermissionRules(session.id)
	require.Equal(t, claude.BehaviorDeny, cached["Write"])

	session.permissionRules["Write"] = claude.BehaviorAllow
	session.persistPermissionRules(context.Background())
	require.Equal(t, claude.BehaviorAllow, savedRules["Write"])

	savePermissionRules = func(context.Context, string, acp.SessionId, map[string]string) error {
		return errors.New("save failed")
	}
	session.permissionRules = nil
	session.setPermissionRule(context.Background(), "Edit", claude.BehaviorAllow)
	session.persistPermissionRules(context.Background())
	require.Equal(t, claude.BehaviorAllow, session.permissionRules["Edit"])
}

func TestPermissionHelperBranches(t *testing.T) {
	t.Parallel()

	require.True(t, permissionRequestCancelled(context.Canceled))
	require.True(t, permissionRequestCancelled(acp.NewRequestCancelled(nil)))
	require.False(t, permissionRequestCancelled(errors.New("boom")))

	session := &agentSession{permissionRules: map[string]string{}, turnCancelled: true}
	ctx, finish := session.permissionRequestContext(context.Background(), "")
	require.NotNil(t, ctx)
	finish()

	ctx, finish = session.permissionRequestContext(context.Background(), "tool")
	select {
	case <-ctx.Done():
	default:
		t.Fatal("turn-cancelled permission context was not canceled")
	}
	finish()
	require.Empty(t, session.permissionCancel)

	suggestions := []map[string]any{{
		jsonFieldType:               permissionUpdateAddRules,
		permissionUpdateBehavior:    claude.BehaviorAllow,
		permissionUpdateDestination: permissionUpdateLocalSettings,
		permissionUpdateRules: []any{
			map[string]any{permissionUpdateToolName: workflowTool + "(deploy)"},
		},
	}}
	normalized := permissionSuggestionsForAllowAlways(workflowTool, suggestions, permissionUpdate("Write", claude.BehaviorAllow))
	require.Equal(t, permissionUpdateSession, normalized[0][permissionUpdateDestination])
	require.Equal(t, permissionUpdateLocalSettings, suggestions[0][permissionUpdateDestination])
	require.Equal(t, suggestions, permissionSuggestionsOrFallback(suggestions, nil))
	require.Len(t, permissionSuggestionsOrFallback(nil, permissionUpdate("Read", claude.BehaviorAllow)), 1)
	require.Equal(t, "Always Allow all Bash", describeAlwaysAllow(nil, "Bash"))
	require.Equal(t, "Always Allow all Read, Write(file.txt) and access to /tmp, /repo", describeAlwaysAllow([]map[string]any{
		{
			jsonFieldType:            permissionUpdateAddRules,
			permissionUpdateBehavior: claude.BehaviorAllow,
			permissionUpdateRules: []any{
				map[string]any{permissionUpdateToolName: "Read"},
				map[string]any{permissionUpdateToolName: "Write", permissionUpdateRuleContent: "file.txt"},
			},
		},
		{
			jsonFieldType: permissionUpdateAddDirs,
			permissionUpdateDirectories: []any{
				"/tmp",
				"",
				"/repo",
			},
		},
	}, "Bash"))
	require.Equal(t, "Always Allow all Bash", describeAlwaysAllow([]map[string]any{{jsonFieldType: permissionUpdateAddRules}}, "Bash"))
	require.True(t, workflowPermissionRuleName(workflowTool+"(x)"))
	require.False(t, workflowPermissionRuleName("Write"))
	require.Equal(t, []map[string]any{{"a": "b"}}, permissionRuleMaps([]any{map[string]any{"a": "b"}, "skip"}))
	require.Equal(t, []map[string]any{{"a": "b"}}, permissionRuleMaps([]map[string]any{{"a": "b"}}))
	require.Equal(t, []map[string]any{{"a": "b"}}, mapSliceAny([]any{map[string]any{"a": "b"}, nil}))
	require.Equal(t, []string{"a"}, clonePermissionSuggestionValue([]string{"a"}))
	require.Equal(t, []any{"a"}, clonePermissionSuggestionValue([]any{"a"}))
	require.Equal(t, []map[string]any{{"a": "b"}}, clonePermissionSuggestionValue([]map[string]any{{"a": "b"}}))
	require.Equal(t, "x", clonePermissionSuggestionValue("x"))
	require.Equal(t, map[string]any{jsonFieldType: permissionUpdateSetMode, jsonFieldMode: string(modePlan), permissionUpdateDestination: permissionUpdateSession}, permissionModeUpdate(modePlan))
}

func TestHandlePermissionBranches(t *testing.T) {
	agent := NewAgent()
	session := &agentSession{
		agent:           agent,
		id:              "session-1",
		cwd:             "/tmp/project",
		mode:            modeDefault,
		model:           "sonnet",
		permissionRules: map[string]string{"Read": claude.BehaviorAllow, "Write": claude.BehaviorDeny},
	}

	decision, err := session.handlePermission(context.Background(), claude.PermissionRequest{ToolName: "Read", Input: map[string]any{"file_path": "/tmp/a"}})
	require.NoError(t, err)
	require.Equal(t, claude.BehaviorAllow, decision.Behavior)
	decision, err = session.handlePermission(context.Background(), claude.PermissionRequest{ToolName: "Write"})
	require.NoError(t, err)
	require.Equal(t, claude.BehaviorDeny, decision.Behavior)
	require.Contains(t, decision.Message, "saved")
	decision, err = session.handlePermission(context.Background(), claude.PermissionRequest{ToolName: "Bash", ToolUseID: "bash-cancelled"})
	require.NoError(t, err)
	require.Equal(t, "ACP client is unavailable", decision.Message)
	decision, err = session.handlePermission(context.Background(), claude.PermissionRequest{ToolName: askUserQuestionTool})
	require.NoError(t, err)
	require.Equal(t, claude.BehaviorDeny, decision.Behavior)

	previousSave := savePermissionRules
	savePermissionRules = func(context.Context, string, acp.SessionId, map[string]string) error { return nil }
	t.Cleanup(func() { savePermissionRules = previousSave })

	for _, tc := range []struct {
		name       string
		selected   acp.PermissionOptionId
		want       string
		wantUpdate bool
	}{
		{name: "allow once", selected: permissionAllowOnce, want: claude.BehaviorAllow},
		{name: "allow always", selected: permissionAllowAlways, want: claude.BehaviorAllow, wantUpdate: true},
		{name: "reject once", selected: permissionRejectOnce, want: claude.BehaviorDeny},
		{name: "reject always", selected: permissionRejectAlways, want: claude.BehaviorDeny, wantUpdate: true},
		{name: "unknown", selected: "unknown", want: claude.BehaviorDeny},
	} {
		t.Run(tc.name, func(t *testing.T) {
			conn := newRecordingAgentClient()
			conn.permission = tc.selected
			agent.setConnection(conn)
			session.permissionRules = map[string]string{}

			decision, err = session.handlePermission(context.Background(), claude.PermissionRequest{ToolName: "Bash", ToolUseID: "tool-1", Input: map[string]any{jsonFieldCommand: "true"}})
			require.NoError(t, err)
			require.Equal(t, tc.want, decision.Behavior)
			if tc.wantUpdate {
				require.NotEmpty(t, decision.UpdatedPermissions)
			}
		})
	}

	conn := newRecordingAgentClient()
	conn.nilPermission = true
	agent.setConnection(conn)
	decision, err = session.handlePermission(context.Background(), claude.PermissionRequest{ToolName: "Bash", ToolUseID: "bash-cancelled"})
	require.NoError(t, err)
	require.Equal(t, permissionCancelledMessage, decision.Message)

	conn = newRecordingAgentClient()
	conn.permissionErr = errors.New("permission failed")
	agent.setConnection(conn)
	_, err = session.handlePermission(context.Background(), claude.PermissionRequest{ToolName: "Bash", ToolUseID: "bash-error"})
	require.ErrorContains(t, err, "permission failed")

	decision, err = session.handlePermission(context.Background(), claude.PermissionRequest{ToolName: "Bash"})
	require.NoError(t, err)
	require.Contains(t, decision.Message, "missing its native tool-use ID")

	conn = newRecordingAgentClient()
	conn.sessionUpdateErr = errors.New("pending update failed")
	agent.setConnection(conn)
	_, err = session.handlePermission(context.Background(), claude.PermissionRequest{ToolName: "Bash", ToolUseID: "bash-update-error"})
	require.ErrorContains(t, err, "pending update failed")
}

func TestExitPlanModePermission(t *testing.T) {
	t.Parallel()

	available := []claude.AvailableModelInfo{{Value: "sonnet", SupportsAutoMode: true}}
	options := exitPlanModeOptions("sonnet", available)
	require.NotEmpty(t, options)
	require.True(t, exitPlanModeSelectionAllows(modeDefault, options))
	require.False(t, exitPlanModeSelectionAllows(modePlan, options))
	require.False(t, exitPlanModeSelectionAllows(modeAuto, nil))

	agent := NewAgent()
	session := &agentSession{agent: agent, id: "session-1", cwd: "/tmp/project", mode: modePlan, model: "sonnet", availableModels: available}
	decision, err := session.handleExitPlanMode(context.Background(), claude.PermissionRequest{ToolName: exitPlanModeTool})
	require.NoError(t, err)
	require.Equal(t, "ACP client is unavailable", decision.Message)

	conn := newRecordingAgentClient()
	conn.permission = acp.PermissionOptionId(modeDefault)
	agent.setConnection(conn)
	turnCtx := activatePermissionControlTurn(t, session, permissionControlTurnNonce)

	pendingErrorConn := newRecordingAgentClient()
	pendingErrorConn.sessionUpdateErr = errors.New("pending exit update failed")
	agent.setConnection(pendingErrorConn)
	_, err = session.handleExitPlanMode(turnCtx, claude.PermissionRequest{ToolName: exitPlanModeTool, ToolUseID: "exit-pending-error"})
	require.ErrorContains(t, err, "pending exit update failed")
	agent.setConnection(conn)

	decision, err = session.handleExitPlanMode(turnCtx, claude.PermissionRequest{ToolName: exitPlanModeTool, ToolUseID: "exit-1", Input: map[string]any{"plan": "done"}})
	require.NoError(t, err)
	require.Equal(t, claude.BehaviorAllow, decision.Behavior)
	require.Equal(t, modeDefault, session.mode)
	require.NotEmpty(t, conn.Updates())

	conn.permission = acp.PermissionOptionId(modePlan)
	session.mode = modePlan
	decision, err = session.handleExitPlanMode(turnCtx, claude.PermissionRequest{ToolName: exitPlanModeTool, ToolUseID: "exit-rejected"})
	require.NoError(t, err)
	require.Equal(t, claude.BehaviorDeny, decision.Behavior)
}

func TestPermissionLifecycleActionFailures(t *testing.T) {
	t.Run("announcement failure", func(t *testing.T) {
		session, conn, _, turnCtx := newLifecycleActionSession(t, permissionControlTurnNonce)
		failLifecycleAction(session, conn, lifecycle.ActionPending,
			errors.New("action announcement delivery"))

		_, err := session.handlePermission(turnCtx, claude.PermissionRequest{
			ToolName: "Bash", ToolUseID: "bash-announce", Input: map[string]any{jsonFieldCommand: "true"},
		})
		require.ErrorContains(t, err, "action announcement delivery")
	})

	t.Run("resolution failure fails after the answer", func(t *testing.T) {
		session, conn, _, turnCtx := newLifecycleActionSession(t, permissionControlTurnNonce)
		conn.permission = permissionAllowOnce
		failLifecycleAction(session, conn, lifecycle.ActionAccepted,
			errors.New("action resolution delivery"))

		decision, err := session.handlePermission(turnCtx, claude.PermissionRequest{
			ToolName: "Bash", ToolUseID: "bash-resolve", Input: map[string]any{jsonFieldCommand: "true"},
		})
		require.ErrorContains(t, err, "action resolution delivery")
		require.Equal(t, claude.PermissionDecision{}, decision)
	})
}

func TestExitPlanModeLifecycleActionFailures(t *testing.T) {
	newSession := func(t *testing.T) (*agentSession, *recordingAgentClient, context.Context) {
		t.Helper()
		session, conn, _, _ := newLifecycleActionSession(t, permissionControlTurnNonce)
		session.mode = modePlan
		session.model = "sonnet"

		return session, conn, activatePermissionControlTurn(t, session, permissionControlTurnNonce)
	}

	t.Run("announcement failure", func(t *testing.T) {
		session, conn, turnCtx := newSession(t)
		failLifecycleAction(session, conn, lifecycle.ActionPending,
			errors.New("action announcement delivery"))

		_, err := session.handleExitPlanMode(turnCtx, claude.PermissionRequest{ToolName: exitPlanModeTool, ToolUseID: "exit-announce"})
		require.ErrorContains(t, err, "action announcement delivery")
	})

	t.Run("resolution failure fails after the answer", func(t *testing.T) {
		session, conn, turnCtx := newSession(t)
		conn.permission = acp.PermissionOptionId(modeDefault)
		failLifecycleAction(session, conn, lifecycle.ActionAccepted,
			errors.New("action resolution delivery"))

		decision, err := session.handleExitPlanMode(turnCtx, claude.PermissionRequest{
			ToolName: exitPlanModeTool, ToolUseID: "exit-resolve", Input: map[string]any{"plan": "done"},
		})
		require.ErrorContains(t, err, "action resolution delivery")
		require.Equal(t, claude.PermissionDecision{}, decision)
	})
}

func TestExitPlanModeControlCallbackRequiresExactActiveRoute(t *testing.T) {
	newSession := func() (*agentSession, *recordingAgentClient) {
		agent := NewAgent()
		conn := newRecordingAgentClient()
		conn.permission = acp.PermissionOptionId(modeDefault)
		agent.setConnection(conn)

		return &agentSession{
			agent: agent,
			id:    "session-1",
			mode:  modePlan,
			model: "sonnet",
		}, conn
	}
	request := claude.PermissionRequest{ToolName: exitPlanModeTool, ToolUseID: "exit-1"}

	t.Run("current route", func(t *testing.T) {
		session, conn := newSession()
		turnCtx := activatePermissionControlTurn(t, session, permissionControlTurnNonce)

		decision, err := session.handleExitPlanMode(turnCtx, request)
		require.NoError(t, err)
		require.Equal(t, claude.BehaviorAllow, decision.Behavior)
		updates := conn.Updates()
		require.Len(t, updates, 2)
		require.Equal(t, acp.ToolCallId("exit-1"), updates[0].Update.ToolCall.ToolCallId)
		require.Equal(t, acp.ToolCallStatusPending, updates[0].Update.ToolCall.Status)
		require.Equal(t, turnRouteMeta(permissionControlTurnNonce), updates[0].Meta)
		require.NotNil(t, updates[1].Update.ConfigOptionUpdate)
	})

	t.Run("missing callback route", func(t *testing.T) {
		session, conn := newSession()
		activatePermissionControlTurn(t, session, permissionControlTurnNonce)

		decision, err := session.handleExitPlanMode(t.Context(), request)
		require.NoError(t, err)
		require.Equal(t, claude.BehaviorDeny, decision.Behavior)
		require.Contains(t, decision.Message, "outside the active turn")
		require.Equal(t, modePlan, session.mode)
		require.Empty(t, conn.Updates())
	})

	t.Run("stale callback route", func(t *testing.T) {
		session, conn := newSession()
		activatePermissionControlTurn(t, session, permissionControlTurnNonce)

		decision, err := session.handleExitPlanMode(
			withTurnRoute(t.Context(), "stale-turn-0123456789abcdef"),
			request,
		)
		require.NoError(t, err)
		require.Equal(t, claude.BehaviorDeny, decision.Behavior)
		require.Equal(t, modePlan, session.mode)
		require.Empty(t, conn.Updates())
	})

	t.Run("callback becomes stale while awaiting permission", func(t *testing.T) {
		agent := NewAgent()
		conn := &blockingExitPlanClient{
			recordingAgentClient: newRecordingAgentClient(),
			entered:              make(chan struct{}),
			release:              make(chan struct{}),
		}
		agent.setConnection(conn)
		session := &agentSession{agent: agent, id: "session-1", mode: modePlan, model: "sonnet"}
		turnCtx := activatePermissionControlTurn(t, session, "turn-old-0123456789abcdef")
		decisionCh := make(chan claude.PermissionDecision, 1)
		errCh := make(chan error, 1)

		go func() {
			decision, err := session.handleExitPlanMode(turnCtx, request)
			decisionCh <- decision
			errCh <- err
		}()

		select {
		case <-conn.entered:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for ExitPlanMode permission request")
		}

		session.mu.Lock()
		session.turnNonce = "turn-new-0123456789abcdef"
		session.cancel = func() {}
		session.mu.Unlock()
		close(conn.release)

		require.NoError(t, <-errCh)
		require.Equal(t, claude.BehaviorDeny, (<-decisionCh).Behavior)
		require.Equal(t, modePlan, session.mode)
		updates := conn.Updates()
		require.Len(t, updates, 1)
		require.Equal(t, acp.ToolCallId("exit-1"), updates[0].Update.ToolCall.ToolCallId)
	})

	t.Run("outside turn", func(t *testing.T) {
		session, conn := newSession()

		decision, err := session.handleExitPlanMode(
			withTurnRoute(t.Context(), permissionControlTurnNonce),
			request,
		)
		require.NoError(t, err)
		require.Equal(t, claude.BehaviorDeny, decision.Behavior)
		require.Equal(t, modePlan, session.mode)
		require.Empty(t, conn.Updates())
	})

	t.Run("cancelled callback", func(t *testing.T) {
		session, conn := newSession()
		turnCtx := activatePermissionControlTurn(t, session, permissionControlTurnNonce)
		cancelledCtx, cancel := context.WithCancel(turnCtx)
		cancel()

		decision, err := session.handleExitPlanMode(cancelledCtx, request)
		require.NoError(t, err)
		require.Equal(t, claude.BehaviorDeny, decision.Behavior)
		require.Equal(t, modePlan, session.mode)
		require.Empty(t, conn.Updates())
	})
}

func TestPermissionAdditionalEdgeBranches(t *testing.T) {
	ctx := context.Background()
	available := []claude.AvailableModelInfo{{Value: "sonnet", SupportsAutoMode: true}}
	agent := NewAgent()
	session := &agentSession{
		agent:           agent,
		id:              "session-1",
		cwd:             "/tmp/project",
		mode:            modePlan,
		model:           "sonnet",
		availableModels: available,
		permissionRules: map[string]string{},
	}

	conn := newRecordingAgentClient()
	conn.permissionErr = acp.NewRequestCancelled(nil)
	agent.setConnection(conn)
	session.turnCancelled = true
	decision, err := session.handlePermission(ctx, claude.PermissionRequest{ToolName: "Bash", ToolUseID: "bash-1"})
	require.NoError(t, err)
	require.True(t, decision.Interrupt)
	require.Equal(t, permissionCancelledMessage, decision.Message)
	session.turnCancelled = false

	conn = newRecordingAgentClient()
	conn.permission = acp.PermissionOptionId(modeDefault)
	agent.setConnection(conn)
	turnCtx := activatePermissionControlTurn(t, session, permissionControlTurnNonce)
	decision, err = session.handleExitPlanMode(turnCtx, claude.PermissionRequest{ToolName: exitPlanModeTool})
	require.NoError(t, err)
	require.Contains(t, decision.Message, "missing its native tool-use ID")

	decision, err = session.handlePermission(turnCtx, claude.PermissionRequest{ToolName: exitPlanModeTool, ToolUseID: "exit-1", Title: "Exit plan"})
	require.NoError(t, err)
	require.Equal(t, claude.BehaviorAllow, decision.Behavior)

	conn = newRecordingAgentClient()
	conn.permissionErr = errors.New("exit permission failed")
	agent.setConnection(conn)
	_, err = session.handleExitPlanMode(turnCtx, claude.PermissionRequest{ToolName: exitPlanModeTool, ToolUseID: "exit-error"})
	require.ErrorContains(t, err, "exit permission failed")

	conn = newRecordingAgentClient()
	conn.nilPermission = true
	agent.setConnection(conn)
	decision, err = session.handleExitPlanMode(turnCtx, claude.PermissionRequest{ToolName: exitPlanModeTool, ToolUseID: "exit-cancelled"})
	require.NoError(t, err)
	require.Equal(t, permissionCancelledMessage, decision.Message)

	conn = newRecordingAgentClient()
	conn.permission = acp.PermissionOptionId(modeDefault)
	conn.sessionUpdateErr = errors.New("config update failed")
	conn.failUpdateAfter = 2
	agent.setConnection(conn)
	_, err = session.handleExitPlanMode(turnCtx, claude.PermissionRequest{ToolName: exitPlanModeTool, ToolUseID: "exit-update-error"})
	require.ErrorContains(t, err, "config update failed")

	conn = newRecordingAgentClient()
	agent.setConnection(conn)
	require.NoError(t, session.emitPendingToolCall(ctx, "Read", "title-fallback", "", nil, nil))
	require.Equal(t, "Read File", conn.Updates()[0].Update.ToolCall.Title)

	nonWorkflow := map[string]any{
		jsonFieldType:               permissionUpdateAddRules,
		permissionUpdateBehavior:    claude.BehaviorAllow,
		permissionUpdateDestination: permissionUpdateLocalSettings,
		permissionUpdateRules: []any{
			map[string]any{permissionUpdateToolName: "Read"},
		},
	}
	require.Equal(t, permissionUpdateLocalSettings, normalizeWorkflowPermissionSuggestion(nonWorkflow)[permissionUpdateDestination])
	require.Equal(t, permissionUpdateSession, normalizeWorkflowPermissionSuggestion(map[string]any{
		jsonFieldType:               permissionUpdateSetMode,
		permissionUpdateDestination: permissionUpdateSession,
	})[permissionUpdateDestination])
	require.Equal(t, "Always Allow all Bash", describeAlwaysAllow([]map[string]any{{
		jsonFieldType:            permissionUpdateAddRules,
		permissionUpdateBehavior: claude.BehaviorAllow,
		permissionUpdateRules:    []any{map[string]any{}},
	}}, "Bash"))
}

type callbackOrderWireClient struct {
	recordingClient

	permissionOption acp.PermissionOptionId
}

func (c *callbackOrderWireClient) RequestPermission(
	_ context.Context,
	_ acp.RequestPermissionRequest,
) (acp.RequestPermissionResponse, error) {
	return acp.RequestPermissionResponse{
		Outcome: acp.NewRequestPermissionOutcomeSelected(c.permissionOption),
	}, nil
}

func (c *callbackOrderWireClient) UnstableCreateElicitation(
	_ context.Context,
	_ acp.UnstableCreateElicitationRequest,
) (acp.UnstableCreateElicitationResponse, error) {
	response := acp.NewUnstableCreateElicitationResponseAccept()
	response.Accept.Content = map[string]any{"q": "Yes"}

	return response, nil
}

type callbackOrderWireRecorder struct {
	writer io.Writer

	mu       sync.Mutex
	messages []json.RawMessage
}

// Write records successful agent-to-client frames before the next frame can be
// sent. The ACP SDK dispatches notifications and requests on different client
// goroutines, so callback handler order is not evidence of sender wire order.
func (r *callbackOrderWireRecorder) Write(p []byte) (int, error) {
	n, err := r.writer.Write(p)
	if err != nil || n != len(p) {
		return n, err
	}

	line := bytes.TrimSpace(p)
	if len(line) > 0 {
		r.mu.Lock()
		r.messages = append(r.messages, append(json.RawMessage(nil), line...))
		r.mu.Unlock()
	}

	return n, nil
}

func (r *callbackOrderWireRecorder) Order(t *testing.T) []string {
	t.Helper()

	r.mu.Lock()
	messages := append([]json.RawMessage(nil), r.messages...)
	r.mu.Unlock()

	order := make([]string, 0, len(messages))
	for _, message := range messages {
		var envelope struct {
			Method string          `json:"method"`
			Params json.RawMessage `json:"params"`
		}
		require.NoError(t, json.Unmarshal(message, &envelope))

		switch envelope.Method {
		case acp.ClientMethodSessionUpdate:
			var notification acp.SessionNotification
			require.NoError(t, json.Unmarshal(envelope.Params, &notification))
			switch {
			case notification.Update.ToolCall != nil:
				order = append(order, "tool_call:"+string(notification.Update.ToolCall.ToolCallId))
			case notification.Update.ToolCallUpdate != nil:
				order = append(order, "tool_call_update:"+string(notification.Update.ToolCallUpdate.ToolCallId))
			default:
				order = append(order, "session_update")
			}
		case acp.ClientMethodSessionRequestPermission:
			var request acp.RequestPermissionRequest
			require.NoError(t, json.Unmarshal(envelope.Params, &request))
			order = append(order, "permission:"+string(request.ToolCall.ToolCallId))
		case acp.ClientMethodElicitationCreate:
			var request acp.UnstableCreateElicitationRequest
			require.NoError(t, json.Unmarshal(envelope.Params, &request))
			var meta map[string]any
			if request.Form != nil {
				meta = request.Form.Meta
			} else if request.Url != nil {
				meta = request.Url.Meta
			}
			route, _ := meta[routeMetaKey].(map[string]any)
			toolCallID, _ := route["toolCallId"].(string)
			order = append(order, "elicitation:"+toolCallID)
		}
	}

	return order
}

func newCallbackOrderWireSession(
	t *testing.T,
	permissionOption acp.PermissionOptionId,
) (*agentSession, *callbackOrderWireRecorder, context.Context) {
	t.Helper()

	clientToAgentReader, clientToAgentWriter := io.Pipe()
	agentToClientReader, agentToClientWriter := io.Pipe()
	t.Cleanup(func() {
		_ = clientToAgentReader.Close()
		_ = clientToAgentWriter.Close()
		_ = agentToClientReader.Close()
		_ = agentToClientWriter.Close()
	})

	agent := NewAgent()
	wireClient := &callbackOrderWireClient{permissionOption: permissionOption}
	peer := acp.NewClientSideConnection(wireClient, clientToAgentWriter, agentToClientReader)
	wire := &callbackOrderWireRecorder{writer: agentToClientWriter}
	local := newLocalAgentConnection(agent, wire, clientToAgentReader)
	agent.setConnection(local)

	_, err := peer.Initialize(t.Context(), acp.InitializeRequest{ClientCapabilities: acp.ClientCapabilities{
		Elicitation: &acp.ElicitationCapabilities{
			Form: &acp.ElicitationFormCapabilities{},
			Url:  &acp.ElicitationUrlCapabilities{},
		},
	}})
	require.NoError(t, err)

	session := &agentSession{
		agent:           agent,
		id:              "session-wire",
		cwd:             "/tmp/project",
		mode:            modePlan,
		model:           "sonnet",
		permissionRules: map[string]string{},
	}
	turnCtx := activatePermissionControlTurn(t, session, permissionControlTurnNonce)

	return session, wire, turnCtx
}

func TestPermissionCallbacksPublishExactPendingToolOnACPWire(t *testing.T) {
	t.Run("general permission", func(t *testing.T) {
		session, wire, turnCtx := newCallbackOrderWireSession(t, permissionAllowOnce)
		decision, err := session.handlePermission(turnCtx, claude.PermissionRequest{
			ToolName: "Bash", ToolUseID: "bash-wire", Input: map[string]any{jsonFieldCommand: "true"},
		})
		require.NoError(t, err)
		require.Equal(t, claude.BehaviorAllow, decision.Behavior)
		require.Equal(t, []string{"tool_call:bash-wire", "permission:bash-wire"}, wire.Order(t))

		updates := mapper.MessageToUpdatesWithOptions(&claude.AssistantMessage{
			Content: []claude.ContentBlock{claude.ToolUseBlock{ID: "bash-wire", Name: "Bash", Input: map[string]any{jsonFieldCommand: "true"}}},
		}, mapper.ToolUpdateOptions{ToolUses: make(map[string]claude.ToolUseBlock)})
		require.NoError(t, session.emitUpdates(turnCtx, updates))
		require.Equal(t, []string{
			"tool_call:bash-wire", "permission:bash-wire", "tool_call_update:bash-wire",
		}, wire.Order(t))
	})

	t.Run("ExitPlanMode", func(t *testing.T) {
		session, wire, turnCtx := newCallbackOrderWireSession(t, acp.PermissionOptionId(modeDefault))
		decision, err := session.handleExitPlanMode(turnCtx, claude.PermissionRequest{
			ToolName: exitPlanModeTool, ToolUseID: "exit-wire", Input: map[string]any{"plan": "ship"},
		})
		require.NoError(t, err)
		require.Equal(t, claude.BehaviorAllow, decision.Behavior)
		require.Equal(t, []string{
			"tool_call:exit-wire", "permission:exit-wire", "session_update",
		}, wire.Order(t))

		updates := mapper.MessageToUpdatesWithOptions(&claude.AssistantMessage{
			Content: []claude.ContentBlock{claude.ToolUseBlock{ID: "exit-wire", Name: exitPlanModeTool, Input: map[string]any{"plan": "ship"}}},
		}, mapper.ToolUpdateOptions{ToolUses: make(map[string]claude.ToolUseBlock)})
		require.NoError(t, session.emitUpdates(turnCtx, updates))
		require.Equal(t, "tool_call_update:exit-wire", wire.Order(t)[3])
	})
}
