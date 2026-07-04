package claudeacp

import (
	"context"
	"errors"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/stretchr/testify/require"
)

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

func TestAgentPermissionRuleLoadBranches(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	sessionID := acp.SessionId("session-1")
	agent := NewAgent(WithHome(t.TempDir()))
	_, ok := agent.cachedPermissionRules(sessionID)
	require.False(t, ok)

	require.NoError(t, savePermissionRules(ctx, agent.options.Home, sessionID, map[string]string{"Read": claude.BehaviorAllow}))
	rules, err := agent.permissionRulesForSession(ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"Read": claude.BehaviorAllow}, rules)

	explicit, err := agent.permissionRulesForStart(ctx, "next", sessionStart{PermissionRules: map[string]string{"Write": claude.BehaviorDeny}})
	require.NoError(t, err)
	require.Equal(t, map[string]string{"Write": claude.BehaviorDeny}, explicit)

	badHome := string([]byte{0})
	loadErrAgent := NewAgent(WithHome(badHome))
	_, err = loadErrAgent.loadPermissionRules(ctx, sessionID)
	require.ErrorContains(t, err, "load permission rules")
	loadErrAgent.cachePermissionRules(sessionID, map[string]string{"Bash": claude.BehaviorAllow})
	rules, err = loadErrAgent.loadPermissionRules(ctx, sessionID)
	require.NoError(t, err)
	require.Equal(t, map[string]string{"Bash": claude.BehaviorAllow}, rules)
	rules["Bash"] = claude.BehaviorDeny
	cached, ok := loadErrAgent.cachedPermissionRules(sessionID)
	require.True(t, ok)
	require.Equal(t, claude.BehaviorAllow, cached["Bash"])
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
	decision, err = session.handlePermission(context.Background(), claude.PermissionRequest{ToolName: "Bash"})
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

			decision, err = session.handlePermission(context.Background(), claude.PermissionRequest{ToolName: "Bash", ToolUseID: "tool-1", Input: map[string]any{"command": "true"}})
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
	decision, err = session.handlePermission(context.Background(), claude.PermissionRequest{ToolName: "Bash"})
	require.NoError(t, err)
	require.Equal(t, permissionCancelledMessage, decision.Message)

	conn = newRecordingAgentClient()
	conn.permissionErr = errors.New("permission failed")
	agent.setConnection(conn)
	_, err = session.handlePermission(context.Background(), claude.PermissionRequest{ToolName: "Bash"})
	require.ErrorContains(t, err, "permission failed")
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
	decision, err = session.handleExitPlanMode(context.Background(), claude.PermissionRequest{ToolName: exitPlanModeTool, ToolUseID: "exit-1", Input: map[string]any{"plan": "done"}})
	require.NoError(t, err)
	require.Equal(t, claude.BehaviorAllow, decision.Behavior)
	require.Equal(t, modeDefault, session.mode)
	require.NotEmpty(t, conn.Updates())

	conn.permission = acp.PermissionOptionId(modePlan)
	session.mode = modePlan
	decision, err = session.handleExitPlanMode(context.Background(), claude.PermissionRequest{ToolName: exitPlanModeTool})
	require.NoError(t, err)
	require.Equal(t, claude.BehaviorDeny, decision.Behavior)
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
	decision, err = session.handlePermission(ctx, claude.PermissionRequest{ToolName: exitPlanModeTool, ToolUseID: "exit-1", Title: "Exit plan"})
	require.NoError(t, err)
	require.Equal(t, claude.BehaviorAllow, decision.Behavior)

	conn = newRecordingAgentClient()
	conn.permissionErr = errors.New("exit permission failed")
	agent.setConnection(conn)
	_, err = session.handleExitPlanMode(ctx, claude.PermissionRequest{ToolName: exitPlanModeTool})
	require.ErrorContains(t, err, "exit permission failed")

	conn = newRecordingAgentClient()
	conn.nilPermission = true
	agent.setConnection(conn)
	decision, err = session.handleExitPlanMode(ctx, claude.PermissionRequest{ToolName: exitPlanModeTool})
	require.NoError(t, err)
	require.Equal(t, permissionCancelledMessage, decision.Message)

	conn = newRecordingAgentClient()
	conn.permission = acp.PermissionOptionId(modeDefault)
	conn.sessionUpdateErr = errors.New("config update failed")
	agent.setConnection(conn)
	_, err = session.handleExitPlanMode(ctx, claude.PermissionRequest{ToolName: exitPlanModeTool})
	require.ErrorContains(t, err, "config update failed")

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
