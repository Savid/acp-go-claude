//go:build integration

package integration

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	claudeacp "github.com/savid/acp-go-claude"
	"github.com/stretchr/testify/require"
)

func TestClaudeACPAgentBinaryConversation(t *testing.T) {
	requireLiveTokens(t)
	parallelWhenPortableClaudeAuth(t)

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	client := &recordingClient{}
	conn := connectLiveAgentBinary(t, ctx, client, acp.InitializeRequest{})

	session, err := conn.NewSession(ctx, acp.NewSessionRequest{Cwd: t.TempDir(), McpServers: []acp.McpServer{}})
	require.NoError(t, err)

	resp, err := conn.Prompt(ctx, claudeacp.TextPromptRequest(session.SessionId, "turn-binary", "Reply with exactly ACP_BINARY_OK and no punctuation."))
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, resp.StopReason)
	require.Contains(t, client.text(), "ACP_BINARY_OK")

	_, err = conn.CloseSession(ctx, acp.CloseSessionRequest{SessionId: session.SessionId})
	require.NoError(t, err)
}

// TestBinarySmokeInitializeClose exercises the compiled adapter's ACP startup
// and graceful EOF without launching a native harness or reading credentials.
// It proves wrapper wiring and counter production, not native compatibility.
func TestBinarySmokeInitializeClose(t *testing.T) {
	if os.Getenv(envRunIntegration) != "1" {
		t.Skipf("set %s=1 to run compiled adapter smoke", envRunIntegration)
	}
	t.Parallel()
	ctx, cancel := context.WithTimeout(t.Context(), 30*time.Second)
	defer cancel()
	home := t.TempDir()
	cmd := exec.CommandContext(ctx, integrationBinaryPath(t), "-path", filepath.Join(home, "no-native-harness"), "-home", home)
	cmd.Dir = home
	cmd.Env = []string{"HOME=" + home, "USERPROFILE=" + home, "XDG_CONFIG_HOME=" + home, "TMPDIR=" + home, "TEMP=" + home, "TMP=" + home}
	for _, key := range []string{"PATH", "SystemRoot", "SYSTEMROOT", "WINDIR", "GOCOVERDIR"} {
		if value, ok := os.LookupEnv(key); ok {
			cmd.Env = append(cmd.Env, key+"="+value)
		}
	}
	process := startIntegrationProcess(t, cmd)
	conn := acp.NewClientSideConnection(&recordingClient{}, process.stdin, process.stdout)
	response, err := conn.Initialize(ctx, acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber})
	if err != nil {
		t.Fatalf("compiled initialize: %v; stderr: %s", err, process.stderr.String())
	}
	if response.ProtocolVersion != acp.ProtocolVersionNumber || response.AgentInfo == nil {
		t.Fatalf("unexpected initialize response: %#v", response)
	}
	if err := process.stdin.Close(); err != nil {
		t.Fatal(err)
	}
	if err := process.wait(ctx); err != nil {
		t.Fatalf("compiled adapter EOF: %v; stderr: %s", err, process.stderr.String())
	}
}
