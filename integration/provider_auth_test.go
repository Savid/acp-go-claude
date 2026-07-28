//go:build integration

package integration

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	claudeacp "github.com/savid/acp-go-claude"
	"github.com/stretchr/testify/require"
)

const envRunAttended = "ACP_GO_CLAUDE_RUN_ATTENDED"

// requireRunAttended gates the tier whose flows need a human at the provider.
// Once the gate is set the tier never skips: an unanswered prompt is a failure,
// because a silently green attended suite is worse than a red one.
func requireRunAttended(t *testing.T) {
	t.Helper()

	if os.Getenv(envRunIntegration) != "1" || os.Getenv(envRunAttended) != "1" {
		t.Skipf("set %s=1 and %s=1 to run attended provider-auth tests", envRunIntegration, envRunAttended)
	}
}

type authMethodsWire struct {
	Providers map[string][]struct {
		ID    string `json:"id"`
		Type  string `json:"type"`
		Label string `json:"label"`
	} `json:"providers"`
	Generation string `json:"generation"`
}

type authAuthorizeWire struct {
	Interaction   string `json:"interaction"`
	URL           string `json:"url"`
	Message       string `json:"message"`
	CallbackInput string `json:"callbackInput"`
	FlowID        string `json:"flowId"`
	FlowExpiresAt int64  `json:"flowExpiresAt"`
}

type authStatusWire struct {
	FlowID string `json:"flowId"`
	State  string `json:"state"`
	Reason string `json:"reason"`
}

func callAuthLeg(t *testing.T, ctx context.Context, conn *acp.ClientSideConnection, method string, params any, out any) error {
	t.Helper()

	raw, err := conn.CallExtension(ctx, method, params)
	if err != nil {
		return err
	}

	if out == nil {
		return nil
	}

	require.NoError(t, json.Unmarshal(raw, out))

	return nil
}

// TestAttendedProviderAuthLoginCompletes drives the hosted paste-back login end
// to end against the real CLI. The operator opens the relayed URL and pastes
// the `<code>#<state>` value back before the flow's deadline.
func TestAttendedProviderAuthLoginCompletes(t *testing.T) {
	requireRunAttended(t)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	client := &recordingClient{}
	authRoot := t.TempDir()
	conn := serveLiveAgentForTest(t, ctx, client, claudeacp.WithProviderAuthRoot(authRoot))

	response, err := conn.Initialize(ctx, acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber})
	require.NoError(t, err)

	vendor, ok := response.AgentCapabilities.Meta["claude"].(map[string]any)
	require.True(t, ok)
	require.Contains(t, vendor, "providerAuth")

	session, err := conn.NewSession(ctx, acp.NewSessionRequest{Cwd: t.TempDir(), McpServers: []acp.McpServer{}})
	require.NoError(t, err)

	sessionID := string(session.SessionId)

	var methods authMethodsWire
	require.NoError(t, callAuthLeg(t, ctx, conn, claudeacp.AuthMethodsMethod,
		map[string]any{"sessionId": sessionID}, &methods))
	require.Len(t, methods.Providers, 1)

	entries := methods.Providers["anthropic"]
	require.Len(t, entries, 1)

	var authorization authAuthorizeWire
	require.NoError(t, callAuthLeg(t, ctx, conn, claudeacp.AuthAuthorizeMethod, map[string]any{
		"sessionId":          sessionID,
		"providerId":         "anthropic",
		"connectionId":       "attended-connection",
		"methodsGeneration":  methods.Generation,
		"method":             entries[0].ID,
		"authorizeRequestId": "attended-request",
	}, &authorization))

	require.Equal(t, "callback", authorization.Interaction)
	require.Equal(t, "code", authorization.CallbackInput)
	require.True(t, strings.HasPrefix(authorization.URL, "https://claude.com/"))

	t.Logf("open this url, approve, and paste the <code>#<state> value on stdin before %s:\n  %s",
		time.UnixMilli(authorization.FlowExpiresAt).Format(time.RFC3339), authorization.URL)

	pasted := waitForAttendedPaste(t, time.UnixMilli(authorization.FlowExpiresAt))

	require.NoError(t, callAuthLeg(t, ctx, conn, claudeacp.AuthCallbackMethod, map[string]any{
		"sessionId":  sessionID,
		"providerId": "anthropic",
		"method":     entries[0].ID,
		"flowId":     authorization.FlowID,
		"input":      pasted,
	}, nil))

	var status authStatusWire
	require.NoError(t, callAuthLeg(t, ctx, conn, claudeacp.AuthStatusMethod, map[string]any{
		"sessionId":  sessionID,
		"providerId": "anthropic",
		"flowId":     authorization.FlowID,
	}, &status))
	require.Equal(t, "authenticated", status.State, "reason %q", status.Reason)

	assertLedgerIsValuesFree(t, authRoot, authorization)
}

// waitForAttendedPaste reads the operator's pasted value from stdin. It fails
// rather than skips when the human does not answer inside the flow's own
// deadline: a silently green attended suite is worse than a red one.
func waitForAttendedPaste(t *testing.T, deadline time.Time) string {
	t.Helper()

	answered := make(chan string, 1)

	go func() {
		scanner := bufio.NewScanner(os.Stdin)
		if scanner.Scan() {
			answered <- strings.TrimSpace(scanner.Text())

			return
		}

		close(answered)
	}()

	select {
	case value := <-answered:
		require.NotEmpty(t, value, "stdin carried no pasted value")

		return value
	case <-time.After(time.Until(deadline)):
		t.Fatal("no pasted value arrived on stdin before the flow deadline")

		return ""
	}
}

// assertLedgerIsValuesFree walks every ledger entry and fails on any presented
// value: the ledger records slot identity and provenance only.
func assertLedgerIsValuesFree(t *testing.T, authRoot string, authorization authAuthorizeWire) {
	t.Helper()

	banned := []string{authorization.URL, authorization.Message}

	require.NoError(t, filepath.WalkDir(authRoot, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil || entry.IsDir() {
			return walkErr
		}

		contents, err := os.ReadFile(path)
		if err != nil {
			return err
		}

		for _, value := range banned {
			if value != "" && strings.Contains(string(contents), value) {
				t.Fatalf("ledger entry %s carries presented value %q", path, value)
			}
		}

		return nil
	}))
}
