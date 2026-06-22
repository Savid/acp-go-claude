//go:build integration

package integration

import (
	"context"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	claudeacp "github.com/savid/acp-go-claude"
	"github.com/stretchr/testify/require"
)

func TestClaudeWorkflowCapabilityLive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	client := &recordingClient{}
	_, init := initializeLiveAgentForTest(t, ctx, client, acp.InitializeRequest{})

	workflows := claudeWorkflowCapabilityMap(t, init.AgentCapabilities.Meta)
	require.Equal(t, "session", workflows["scope"])
	require.Equal(t, true, workflows["updates"])
	require.Equal(t, "think", workflows["toolKind"])
	require.Equal(t, "tool_call_update._meta.claude.workflow", workflows["metadataPath"])
	require.Equal(t, "_claude/sdkMessage", workflows["rawEvents"])
}

func TestClaudeWorkflowMappedLive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Minute)
	defer cancel()

	requireClaudeWorkflowSnapshotVersion(t, ctx)

	client := &recordingClient{}
	conn := connectLiveAgent(t, ctx, client, acp.InitializeRequest{})

	session, err := conn.NewSession(ctx, claudeacp.NewSessionRequest(
		t.TempDir(),
		claudeacp.WithSessionClaudeOptions(claudeacp.NewClaudeOptions(
			claudeacp.WithClaudePermissionMode("bypassPermissions"),
		)),
	))
	require.NoError(t, err)
	requireBypassPermissionMode(t, session.ConfigOptions)

	resp, err := conn.Prompt(ctx, claudeacp.TextPromptRequest(session.SessionId, workflowLivePrompt()))
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, resp.StopReason)
	require.Contains(t, client.text(), "ACP_WORKFLOW_DONE")

	var finalWorkflow map[string]any
	require.Eventually(t, func() bool {
		for _, update := range client.updateSnapshot() {
			toolUpdate := update.ToolCallUpdate
			if toolUpdate == nil || toolUpdate.Status == nil || *toolUpdate.Status != acp.ToolCallStatusCompleted {
				continue
			}

			workflow := workflowMetaFromToolUpdate(toolUpdate)
			if workflow == nil || workflow["status"] != "completed" {
				continue
			}

			finalWorkflow = workflow

			return true
		}

		return false
	}, 30*time.Second, 250*time.Millisecond)

	require.Equal(t, float64(2), finalWorkflow["completedAgents"])
	require.Equal(t, float64(0), finalWorkflow["activeAgents"])
	require.Equal(t, float64(0), finalWorkflow["failedAgents"])
	require.Len(t, workflowList(t, finalWorkflow["phases"]), 2)

	agents := workflowList(t, finalWorkflow["agents"])
	require.Len(t, agents, 2)
	require.Equal(t, float64(1), agents[0]["index"])
	require.Equal(t, "done", agents[0]["state"])
	require.Contains(t, agents[0]["resultPreview"], "ACP_WF_ALPHA")
	require.Equal(t, float64(2), agents[1]["index"])
	require.Equal(t, "done", agents[1]["state"])
	require.Contains(t, agents[1]["resultPreview"], "ACP_WF_BETA")
	require.NotContains(t, finalWorkflow, "outputContent")

	_, err = conn.CloseSession(ctx, acp.CloseSessionRequest{SessionId: session.SessionId})
	require.NoError(t, err)
}

func TestClaudeWorkflowPermissionGateLive(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	requireClaudeWorkflowSnapshotVersion(t, ctx)

	client := newBlockingPermissionClient()
	conn := connectLiveAgent(t, ctx, client, acp.InitializeRequest{})

	session, err := conn.NewSession(ctx, claudeacp.NewSessionRequest(t.TempDir()))
	require.NoError(t, err)

	respCh := make(chan acp.PromptResponse, 1)
	errCh := make(chan error, 1)
	go func() {
		resp, promptErr := conn.Prompt(ctx, claudeacp.TextPromptRequest(session.SessionId, workflowLivePrompt()))
		if promptErr != nil {
			errCh <- promptErr

			return
		}

		respCh <- resp
	}()

	select {
	case <-client.permissionRequested:
	case err := <-errCh:
		if len(client.permissionSnapshot()) == 0 {
			t.Skipf("Claude did not surface a Workflow permission gate in this local configuration: %v", err)
		}
		require.NoError(t, err)
	case <-respCh:
		if len(client.permissionSnapshot()) == 0 {
			t.Skip("Claude did not surface a Workflow permission gate in this local configuration")
		}
	case <-time.After(90 * time.Second):
		t.Fatal("timed out waiting for Workflow permission gate")
	}

	permissions := client.permissionSnapshot()
	require.NotEmpty(t, permissions)
	require.NotNil(t, permissions[0].ToolCall.Title)
	require.Equal(t, "Workflow", *permissions[0].ToolCall.Title)
	require.NotNil(t, permissions[0].ToolCall.Kind)
	require.Equal(t, acp.ToolKindThink, *permissions[0].ToolCall.Kind)

	require.NoError(t, conn.Cancel(ctx, acp.CancelNotification{SessionId: session.SessionId}))
	select {
	case returned := <-client.permissionReturned:
		require.NotNil(t, returned.Outcome.Cancelled)
	case <-time.After(30 * time.Second):
		t.Fatal("Workflow permission request did not return after cancel")
	}

	select {
	case err := <-errCh:
		require.NoError(t, err)
	case resp := <-respCh:
		require.Equal(t, acp.StopReasonCancelled, resp.StopReason)
	case <-time.After(30 * time.Second):
		t.Fatal("Workflow prompt did not finish after cancel")
	}

	_, err = conn.CloseSession(ctx, acp.CloseSessionRequest{SessionId: session.SessionId})
	require.NoError(t, err)
}

func requireClaudeWorkflowSnapshotVersion(t *testing.T, ctx context.Context) {
	t.Helper()

	out, err := exec.CommandContext(ctx, integrationClaudePath(t), "--version").CombinedOutput()
	require.NoError(t, err, string(out))
	if !strings.Contains(string(out), "2.1.154") {
		t.Skipf("workflow probes are pinned to claude 2.1.154; got %s", strings.TrimSpace(string(out)))
	}
}

func requireBypassPermissionMode(t *testing.T, options []acp.SessionConfigOption) {
	t.Helper()

	modeConfig := selectConfig(options, "mode")
	if selectConfigValueAvailable(modeConfig, "bypass_permissions") {
		return
	}

	t.Skip("bypassPermissions mode is not available; skipping workflow live prompt to avoid default-mode false pass")
}

func claudeWorkflowCapabilityMap(t *testing.T, meta map[string]any) map[string]any {
	t.Helper()

	claudeMeta, ok := meta["claude"].(map[string]any)
	require.True(t, ok)

	workflows, ok := claudeMeta["workflows"].(map[string]any)
	require.True(t, ok)

	return workflows
}

func workflowMetaFromToolUpdate(update *acp.SessionToolCallUpdate) map[string]any {
	if update == nil || update.Meta == nil {
		return nil
	}

	claudeMeta, _ := update.Meta["claude"].(map[string]any)
	if claudeMeta == nil {
		return nil
	}

	workflow, _ := claudeMeta["workflow"].(map[string]any)

	return workflow
}

func workflowList(t *testing.T, value any) []map[string]any {
	t.Helper()

	items, ok := value.([]any)
	require.True(t, ok)

	mapped := make([]map[string]any, 0, len(items))
	for _, item := range items {
		value, ok := item.(map[string]any)
		require.True(t, ok)
		mapped = append(mapped, value)
	}

	return mapped
}

func workflowLivePrompt() string {
	return `Use the Workflow tool exactly once with a tiny inline workflow that has two phases and two agents.
The first agent must return ACP_WF_ALPHA.
The second agent must return ACP_WF_BETA.
After the workflow completes, end with exactly ACP_WORKFLOW_DONE.

The workflow script should be equivalent to:

export const meta = {
  name: 'acp-tiny-two-phase',
  description: 'Tiny two-phase workflow with two agents returning fixed tokens',
  phases: [
    { title: 'Alpha', detail: 'first agent returns ACP_WF_ALPHA' },
    { title: 'Beta', detail: 'second agent returns ACP_WF_BETA' },
  ],
}

phase('Alpha')
const alpha = await agent('Return exactly this text and nothing else: ACP_WF_ALPHA', { label: 'alpha' })
log(` + "`" + `alpha returned: ${alpha}` + "`" + `)

phase('Beta')
const beta = await agent('Return exactly this text and nothing else: ACP_WF_BETA', { label: 'beta' })
log(` + "`" + `beta returned: ${beta}` + "`" + `)

return { alpha, beta }`
}
