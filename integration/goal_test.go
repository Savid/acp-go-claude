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

const pinnedGoalProbeClaudeVersion = "2.1.200"

func TestClaudeGoalCommandLiveProbe(t *testing.T) {
	requireLiveTokens(t)
	parallelWhenPortableClaudeAuth(t)

	claudePath := integrationClaudePath(t)
	versionBytes, err := exec.Command(claudePath, "--version").Output() // #nosec G204 -- integration probes configured local Claude.
	require.NoError(t, err)
	version := strings.TrimSpace(string(versionBytes))
	if !strings.Contains(version, pinnedGoalProbeClaudeVersion) {
		t.Skipf("goal probe is pinned to Claude Code %s; local version is %q", pinnedGoalProbeClaudeVersion, version)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	client := &recordingClient{}
	conn := connectLiveAgent(t, ctx, client, acp.InitializeRequest{})
	session, err := conn.NewSession(ctx, claudeacp.NewSessionRequest(t.TempDir(), claudeacp.WithSessionRawEvents(true)))
	require.NoError(t, err)

	resp := promptWithRefusalRetry(t, func() (acp.PromptResponse, error) {
		return conn.Prompt(ctx, claudeacp.TextPromptRequest(
			session.SessionId,
			"turn-goal",
			"/goal Reply exactly ACP_GOAL_PROBE_OK and no punctuation when this goal is satisfied.",
		))
	})
	require.Equal(t, acp.StopReasonEndTurn, resp.StopReason)
	require.Contains(t, client.text(), "ACP_GOAL_PROBE_OK")
	require.Equal(t, 1, rawClaudeResultCount(client.extensionSnapshot()))
}

func rawClaudeResultCount(extensions []recordedExtension) int {
	count := 0
	for _, extension := range extensions {
		if extension.Method != claudeacp.RawEventMethod {
			continue
		}

		event, _ := extension.Params["event"].(map[string]any)
		if event["type"] == "result" {
			count++
		}
	}

	return count
}
