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

// startAuthFlowForTest drives methods plus authorize against a live agent and
// returns the minted presentation beside the method it names.
func startAuthFlowForTest(
	t *testing.T,
	ctx context.Context,
	conn *acp.ClientSideConnection,
	sessionID string,
	connectionID string,
	requestID string,
) (authAuthorizeWire, string) {
	t.Helper()

	var methods authMethodsWire
	require.NoError(t, callAuthLeg(t, ctx, conn, claudeacp.AuthMethodsMethod,
		map[string]any{"sessionId": sessionID}, &methods))
	require.Len(t, methods.Providers, 1)

	entries := methods.Providers["anthropic"]
	require.Equal(t, []struct {
		ID    string `json:"id"`
		Type  string `json:"type"`
		Label string `json:"label"`
	}{
		{ID: "login", Type: "oauth", Label: "Claude subscription"},
		{ID: "setup-token", Type: "api", Label: "Claude setup token"},
		{ID: "api-key", Type: "api", Label: "Anthropic API key"},
	}, entries)

	var authorization authAuthorizeWire
	require.NoError(t, callAuthLeg(t, ctx, conn, claudeacp.AuthAuthorizeMethod, map[string]any{
		"sessionId":          sessionID,
		"providerId":         "anthropic",
		"connectionId":       connectionID,
		"methodsGeneration":  methods.Generation,
		"method":             entries[0].ID,
		"authorizeRequestId": requestID,
	}, &authorization))

	require.Equal(t, "callback", authorization.Interaction)
	require.Equal(t, "code", authorization.CallbackInput)
	require.True(t, strings.HasPrefix(authorization.URL, "https://claude.com/"))

	return authorization, entries[0].ID
}

func liveAuthSession(t *testing.T, ctx context.Context, conn *acp.ClientSideConnection) string {
	t.Helper()

	response, err := conn.Initialize(ctx, acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber})
	require.NoError(t, err)

	vendor, ok := response.AgentCapabilities.Meta["claude"].(map[string]any)
	require.True(t, ok)
	require.Contains(t, vendor, "providerAuth")

	session, err := conn.NewSession(ctx, acp.NewSessionRequest{Cwd: t.TempDir(), McpServers: []acp.McpServer{}})
	require.NoError(t, err)

	return string(session.SessionId)
}

func authFlowState(t *testing.T, ctx context.Context, conn *acp.ClientSideConnection, sessionID string, flowID string) authStatusWire {
	t.Helper()

	var status authStatusWire
	require.NoError(t, callAuthLeg(t, ctx, conn, claudeacp.AuthStatusMethod, map[string]any{
		"sessionId":  sessionID,
		"providerId": "anthropic",
		"flowId":     flowID,
	}, &status))

	return status
}

// TestAttendedProviderAuthLoginCompletes drives the hosted paste-back login end
// to end against the real CLI. The operator opens the relayed URL and pastes
// the `<code>#<state>` value back before the flow's deadline.
//
// It runs against a config dir proved empty first. Seeded with the operator's
// own credential — as every other live test here is, so its turns can run — the
// assertion below would hold for any pasted value at all, because `auth status`
// answers for the config dir rather than for the flow.
func TestAttendedProviderAuthLoginCompletes(t *testing.T) {
	requireRunAttended(t)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()

	runtime := emptyClaudeRuntime(t)
	requireClaudeHomeHoldsNoCredential(t, runtime.home)

	client := &recordingClient{}
	authRoot := t.TempDir()
	pipes := serveLiveAgentInRuntimeForTest(t, ctx, runtime, claudeacp.WithProviderAuthRoot(authRoot))
	conn := acp.NewClientSideConnection(client, pipes.clientInput, pipes.agentOutput)

	sessionID := liveAuthSession(t, ctx, conn)
	authorization, methodID := startAuthFlowForTest(t, ctx, conn, sessionID, "attended-connection", "attended-request")

	t.Logf("open this url, approve, and paste the <code>#<state> value on stdin before %s:\n  %s",
		time.UnixMilli(authorization.FlowExpiresAt).Format(time.RFC3339), authorization.URL)

	pasted := waitForAttendedPaste(t, time.UnixMilli(authorization.FlowExpiresAt))

	require.NoError(t, callAuthLeg(t, ctx, conn, claudeacp.AuthCallbackMethod, map[string]any{
		"sessionId":  sessionID,
		"providerId": "anthropic",
		"method":     methodID,
		"flowId":     authorization.FlowID,
		"input":      pasted,
	}, nil))

	status := authFlowState(t, ctx, conn, sessionID, authorization.FlowID)
	require.Equal(t, "authenticated", status.State, "reason %q", status.Reason)

	assertLedgerIsValuesFree(t, authRoot, authorization)
}

// TestProviderAuthRejectedCodeDoesNotComplete drives a provider refusal without
// spending model tokens or requiring an operator.
func TestProviderAuthRejectedCodeDoesNotComplete(t *testing.T) {
	if os.Getenv(envRunIntegration) != "1" {
		t.Skipf("set %s=1 to run provider-auth integration tests", envRunIntegration)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	runtime := emptyClaudeRuntime(t)
	requireClaudeHomeHoldsNoCredential(t, runtime.home)

	client := &recordingClient{}
	pipes := serveLiveAgentInRuntimeForTest(t, ctx, runtime, claudeacp.WithProviderAuthRoot(t.TempDir()))
	conn := acp.NewClientSideConnection(client, pipes.clientInput, pipes.agentOutput)

	sessionID := liveAuthSession(t, ctx, conn)
	authorization, methodID := startAuthFlowForTest(t, ctx, conn, sessionID, "rejected-connection", "rejected-request")

	// A value shaped like the harness's `<code>#<state>` and issued by nobody.
	err := callAuthLeg(t, ctx, conn, claudeacp.AuthCallbackMethod, map[string]any{
		"sessionId":  sessionID,
		"providerId": "anthropic",
		"method":     methodID,
		"flowId":     authorization.FlowID,
		"input":      "not-an-authorization-code#not-a-state",
	}, nil)
	require.Error(t, err, "the leg accepted a value no provider issued")

	status := authFlowState(t, ctx, conn, sessionID, authorization.FlowID)
	require.NotEqual(t, "authenticated", status.State,
		"a rejected authorization code was reported as a completed login")
	require.Equal(t, "failed", status.State, "reason %q", status.Reason)
}

// TestProviderAuthRejectedCodeNeverCompletesOnAPopulatedConfigDir pins the
// direct login child's nonzero exit against an already logged-in home. The
// resident account cannot answer for a value the provider rejected.
func TestProviderAuthRejectedCodeNeverCompletesOnAPopulatedConfigDir(t *testing.T) {
	if os.Getenv(envRunIntegration) != "1" {
		t.Skipf("set %s=1 to run provider-auth integration tests", envRunIntegration)
	}

	source, _ := integrationClaudeSourceHome(t)
	if !portableClaudeAuthAvailable(t, source) {
		t.Skip("no portable Claude credential is available for the populated-home provider-auth test")
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	runtime := isolatedClaudeRuntime(t)
	runtime.env = emptyClaudeCredentialEnv()
	require.NoError(t, os.RemoveAll(filepath.Join(runtime.home, "settings.json")))
	if !claudeHomeLoggedInWithoutCredentialEnv(t, runtime.home) {
		t.Skip("the isolated Claude home has no durable credential independent of static environment or settings auth")
	}

	client := &recordingClient{}
	pipes := serveLiveAgentInRuntimeForTest(t, ctx, runtime, claudeacp.WithProviderAuthRoot(t.TempDir()))
	conn := acp.NewClientSideConnection(client, pipes.clientInput, pipes.agentOutput)

	sessionID := liveAuthSession(t, ctx, conn)
	authorization, methodID := startAuthFlowForTest(
		t,
		ctx,
		conn,
		sessionID,
		"populated-rejected-connection",
		"populated-rejected-request",
	)

	err := callAuthLeg(t, ctx, conn, claudeacp.AuthCallbackMethod, map[string]any{
		"sessionId":  sessionID,
		"providerId": "anthropic",
		"method":     methodID,
		"flowId":     authorization.FlowID,
		"input":      "not-an-authorization-code#not-a-state",
	}, nil)
	require.Error(t, err, "the leg accepted a value no provider issued")

	status := authFlowState(t, ctx, conn, sessionID, authorization.FlowID)
	require.Equal(t, "failed", status.State, "reason %q", status.Reason)
	require.Equal(t, "provider_refused", status.Reason)
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
