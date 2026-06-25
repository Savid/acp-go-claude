//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	claudeacp "github.com/savid/acp-go-claude"
	"github.com/stretchr/testify/require"
)

const claudeSessionSetGoalMethod = "_claude/session/setGoal"

func TestClaudeGoalExtensionLive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	client := &recordingClient{}
	conn, init := initializeLiveAgentForTest(t, ctx, client, acp.InitializeRequest{})

	goals := claudeGoalMap(t, init.AgentCapabilities.Meta)
	require.Equal(t, "session", goals["scope"])
	require.Equal(t, claudeSessionSetGoalMethod, goals["setMethod"])
	require.Equal(t, "session_info_update._meta.claude.goal", goals["state"])

	session, err := conn.NewSession(ctx, claudeacp.NewSessionRequest(
		t.TempDir(),
		claudeacp.WithSessionGoal(claudeacp.ClaudeGoal{
			Objective:           "integration initial goal",
			CompletionCondition: "initial condition",
		}),
	))
	require.NoError(t, err)
	require.Equal(t, "integration initial goal", claudeGoalMap(t, session.Meta)["objective"])

	raw, err := conn.CallExtension(ctx, claudeSessionSetGoalMethod, claudeacp.SetGoalRequest(
		session.SessionId,
		claudeacp.ClaudeGoal{
			Objective:           "integration updated goal",
			CompletionCondition: "updated condition",
			Status:              claudeacp.ClaudeGoalStatusBlocked,
			Reason:              "waiting on integration",
		},
	))
	require.NoError(t, err)
	require.Equal(t, "integration updated goal", extensionGoalMap(t, raw)["objective"])
	require.Eventually(t, func() bool {
		goal, ok := latestGoalUpdate(client)

		return ok && goal != nil && goal["objective"] == "integration updated goal" &&
			goal["status"] == claudeacp.ClaudeGoalStatusBlocked
	}, 30*time.Second, 250*time.Millisecond)

	raw, err = conn.CallExtension(ctx, claudeSessionSetGoalMethod, claudeacp.ClearGoalRequest(session.SessionId))
	require.NoError(t, err)
	require.Nil(t, extensionGoalValue(t, raw))
	require.Eventually(t, func() bool {
		goal, ok := latestGoalUpdate(client)

		return ok && goal == nil
	}, 30*time.Second, 250*time.Millisecond)

	_, err = conn.CloseSession(ctx, acp.CloseSessionRequest{SessionId: session.SessionId})
	require.NoError(t, err)
}

func TestClaudeGoalClearLocalCommandLive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	client := &recordingClient{}
	conn := connectLiveAgent(t, ctx, client, acp.InitializeRequest{})

	session, err := conn.NewSession(ctx, claudeacp.NewSessionRequest(
		t.TempDir(),
		claudeacp.WithSessionGoal(claudeacp.ClaudeGoal{Objective: "integration clear goal"}),
	))
	require.NoError(t, err)

	_, err = conn.Prompt(ctx, claudeacp.TextPromptRequest(session.SessionId, "/goal clear"))
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		goal, ok := latestGoalUpdate(client)

		return ok && goal == nil
	}, 30*time.Second, 250*time.Millisecond)

	_, err = conn.CloseSession(ctx, acp.CloseSessionRequest{SessionId: session.SessionId})
	require.NoError(t, err)
}

func TestClaudeNativeGoalMirrorLive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	client := &recordingClient{}
	conn := connectLiveAgent(t, ctx, client, acp.InitializeRequest{})

	session, err := conn.NewSession(ctx, claudeacp.NewSessionRequest(
		t.TempDir(),
		claudeacp.WithSessionClaudeOptions(claudeacp.NewClaudeOptions(
			claudeacp.WithClaudePermissionMode("default"),
		)),
	))
	require.NoError(t, err)

	_, err = conn.Prompt(ctx, claudeacp.TextPromptRequest(
		session.SessionId,
		"/goal The next assistant response must be exactly ACP_NATIVE_GOAL_DONE with no punctuation.",
	))
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		goal, ok := latestGoalUpdate(client)

		if !ok || goal == nil {
			return false
		}

		status := goal["status"]
		return goal["objective"] == "The next assistant response must be exactly ACP_NATIVE_GOAL_DONE with no punctuation." &&
			(status == claudeacp.ClaudeGoalStatusActive || status == claudeacp.ClaudeGoalStatusCompleted)
	}, 30*time.Second, 250*time.Millisecond)
	require.Contains(t, client.text(), "ACP_NATIVE_GOAL_DONE")

	_, err = conn.Prompt(ctx, claudeacp.TextPromptRequest(session.SessionId, "/goal clear"))
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		goal, ok := latestGoalUpdate(client)

		return ok && goal == nil
	}, 30*time.Second, 250*time.Millisecond)

	_, err = conn.CloseSession(ctx, acp.CloseSessionRequest{SessionId: session.SessionId})
	require.NoError(t, err)
}

func TestClaudeGoalSetDuringPendingPermissionLive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	client := newBlockingPermissionClient()
	conn := connectLiveAgent(t, ctx, client, acp.InitializeRequest{}, permissionGateOptions()...)

	session, err := conn.NewSession(ctx, claudeacp.NewSessionRequest(t.TempDir()))
	require.NoError(t, err)

	respCh := make(chan acp.PromptResponse, 1)
	errCh := make(chan error, 1)
	go func() {
		resp, promptErr := conn.Prompt(ctx, claudeacp.TextPromptRequest(
			session.SessionId,
			"You must use the Write tool exactly once to create goal_midturn.txt containing ACP_GOAL_MIDTURN. Do not use any other tool.",
		))
		if promptErr != nil {
			errCh <- promptErr

			return
		}

		respCh <- resp
	}()

	select {
	case <-client.permissionRequested:
	case err := <-errCh:
		require.NoError(t, err)
		t.Fatal("prompt returned before requesting permission")
	case resp := <-respCh:
		require.Equal(t, acp.StopReasonCancelled, resp.StopReason)
		t.Fatal("prompt returned before requesting permission")
	case <-ctx.Done():
		require.NoError(t, ctx.Err())
	}

	raw, err := conn.CallExtension(ctx, claudeSessionSetGoalMethod, claudeacp.SetGoalRequest(
		session.SessionId,
		claudeacp.ClaudeGoal{Objective: "integration midturn goal"},
	))
	require.NoError(t, err)
	require.Equal(t, "integration midturn goal", extensionGoalMap(t, raw)["objective"])
	require.Eventually(t, func() bool {
		goal, ok := latestGoalUpdate(&client.recordingClient)

		return ok && goal != nil && goal["objective"] == "integration midturn goal"
	}, 30*time.Second, 250*time.Millisecond)

	require.NoError(t, conn.Cancel(ctx, acp.CancelNotification{SessionId: session.SessionId}))

	select {
	case returned := <-client.permissionReturned:
		require.NotNil(t, returned.Outcome.Cancelled)
	case <-ctx.Done():
		require.NoError(t, ctx.Err())
	}

	select {
	case err := <-errCh:
		require.NoError(t, err)
	case resp := <-respCh:
		require.Equal(t, acp.StopReasonCancelled, resp.StopReason)
	case <-ctx.Done():
		require.NoError(t, ctx.Err())
	}

	_, err = conn.CloseSession(ctx, acp.CloseSessionRequest{SessionId: session.SessionId})
	require.NoError(t, err)
}

func claudeGoalMap(t *testing.T, meta map[string]any) map[string]any {
	t.Helper()

	claudeMeta, ok := meta["claude"].(map[string]any)
	require.True(t, ok)

	goals, ok := claudeMeta["goals"].(map[string]any)
	if ok {
		return goals
	}

	goal, ok := claudeMeta["goal"].(map[string]any)
	require.True(t, ok)

	return goal
}

func extensionGoalMap(t *testing.T, raw json.RawMessage) map[string]any {
	t.Helper()

	goal, ok := extensionGoalValue(t, raw).(map[string]any)
	require.True(t, ok)

	return goal
}

func extensionGoalValue(t *testing.T, raw json.RawMessage) any {
	t.Helper()

	var response map[string]any
	require.NoError(t, json.Unmarshal(raw, &response))

	return response["goal"]
}

func latestGoalUpdate(client *recordingClient) (map[string]any, bool) {
	updates := client.updateSnapshot()
	for index := len(updates) - 1; index >= 0; index-- {
		info := updates[index].SessionInfoUpdate
		if info == nil || info.Meta == nil {
			continue
		}

		claudeMeta, _ := info.Meta["claude"].(map[string]any)
		if claudeMeta == nil {
			continue
		}

		value, ok := claudeMeta["goal"]
		if !ok {
			continue
		}

		if value == nil {
			return nil, true
		}

		goal, _ := value.(map[string]any)

		return goal, goal != nil
	}

	return nil, false
}
