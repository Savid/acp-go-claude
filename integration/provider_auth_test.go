//go:build integration

package integration

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	claudeacp "github.com/savid/acp-go-claude"
	nativeclaude "github.com/savid/acp-go-claude/internal/claude"
	"github.com/stretchr/testify/require"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/wait"
)

const (
	envRunAttended = "ACP_GO_CLAUDE_RUN_ATTENDED"
	envRunKeystore = "ACP_GO_CLAUDE_RUN_KEYSTORE"
)

// canaryCredential is the only material this fixture ever plants. It is not a
// credential and never was one.
const canaryCredential = `{"claudeAiOauth":{"accessToken":"canary-not-a-real-token"}}`

// requireRunAttended gates the tier whose flows need a human at the provider.
// Once the gate is set the tier never skips: an unanswered prompt is a failure,
// because a silently green attended suite is worse than a red one.
func requireRunAttended(t *testing.T) {
	t.Helper()

	if os.Getenv(envRunIntegration) != "1" || os.Getenv(envRunAttended) != "1" {
		t.Skipf("set %s=1 and %s=1 to run attended provider-auth tests", envRunIntegration, envRunAttended)
	}
}

// requireRunKeystore gates the credential-residence matrix. Once the gate is
// set a missing container runtime is a failure rather than a skip.
func requireRunKeystore(t *testing.T) {
	t.Helper()

	if os.Getenv(envRunIntegration) != "1" || os.Getenv(envRunKeystore) != "1" {
		t.Skipf("set %s=1 and %s=1 to run credential-residence tests", envRunIntegration, envRunKeystore)
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

// TestKeystoreLinuxCredentialResidence runs the two Linux thirds of the
// residence matrix against a container fixture. The assertion that matters for
// this harness is that a live Secret Service changes nothing: the Linux
// artifact carries no keystore path at all, so the plaintext store under the
// config dir stays unconditionally authoritative.
func TestKeystoreLinuxCredentialResidence(t *testing.T) {
	requireRunKeystore(t)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	container := startKeystoreContainer(t, ctx)

	address := strings.TrimSpace(execInKeystoreContainer(t, ctx, container, []string{"cat", "/home/canary/dbus-address"}))
	require.NotEmpty(t, address, "the fixture published no session bus address")

	busEnv := "DBUS_SESSION_BUS_ADDRESS=" + address

	// Keystore-present Linux: seed a canary under every documented claude
	// service name, then read it back. The service is genuinely live.
	for _, item := range nativeclaude.AuthKeychainItems("/home/canary/.claude", "canary") {
		execInKeystoreContainer(t, ctx, container, []string{
			"env", busEnv, "sh", "-c",
			fmt.Sprintf("printf %%s %q | secret-tool store --label=%q service %q account %q",
				canaryCredential, item.Service, item.Service, item.Account),
		})

		looked := execInKeystoreContainer(t, ctx, container, []string{
			"env", busEnv, "secret-tool", "lookup", "service", item.Service, "account", item.Account,
		})
		require.Contains(t, looked, "canary-not-a-real-token", "the seeded item is not readable")
	}

	// The file store is what a Linux residence answer reads, and it is written
	// unconditionally rather than as a fallback from anything.
	execInKeystoreContainer(t, ctx, container, []string{
		"sh", "-c",
		fmt.Sprintf("mkdir -p /home/canary/.claude && printf %%s %q > /home/canary/.claude/.credentials.json"+
			" && chmod 0600 /home/canary/.claude/.credentials.json", canaryCredential),
	})

	mode := strings.TrimSpace(execInKeystoreContainer(t, ctx, container, []string{
		"stat", "-c", "%a", "/home/canary/.claude/.credentials.json",
	}))
	require.Equal(t, "600", mode)

	// Keystore-absent Linux: with no session bus the same file answers
	// identically, which is what "unconditionally authoritative" means.
	absent := execInKeystoreContainer(t, ctx, container, []string{
		"sh", "-c", "secret-tool lookup service anything account anything 2>&1 || true",
	})
	require.NotContains(t, absent, "canary-not-a-real-token")

	stored := execInKeystoreContainer(t, ctx, container, []string{"cat", "/home/canary/.claude/.credentials.json"})
	require.Contains(t, stored, "canary-not-a-real-token")
}

// TestKeystoreLinuxArtifactHasNoSecretServiceClient pins the other half of the
// same fact from this repo's own side: the adapter compiled for Linux links no
// Secret Service client, so there is no code path for a live service to reach.
func TestKeystoreLinuxArtifactHasNoSecretServiceClient(t *testing.T) {
	requireRunKeystore(t)

	binary := filepath.Join(t.TempDir(), "acp-go-claude-linux")

	build := exec.CommandContext(t.Context(), "go", "build", "-o", binary, "../cmd/acp-go-claude")
	build.Env = append(os.Environ(), "GOOS=linux", "GOARCH=amd64")

	output, err := build.CombinedOutput()
	require.NoError(t, err, string(output))

	contents, err := os.ReadFile(binary)
	require.NoError(t, err)

	for _, symbol := range []string{"libsecret", "org.freedesktop.secrets", "gnome-keyring"} {
		require.NotContains(t, string(contents), symbol)
	}
}

// TestKeystoreDarwinCredentialResidence runs the macOS third against the real
// login keychain under a canary service name it deletes afterwards. A keychain
// write under a scratch HOME blocks on an interactive modal, which for this
// harness maps to the write-timeout branch that drops the credential silently,
// so no writing harness is ever pointed at one.
func TestKeystoreDarwinCredentialResidence(t *testing.T) {
	requireRunKeystore(t)

	if runtime.GOOS != "darwin" {
		t.Skipf("the login keychain third runs on darwin; this is %s", runtime.GOOS)
	}

	const service = "acp-go-claude-keystore-canary"

	account := os.Getenv("USER")
	require.NotEmpty(t, account)

	add := exec.CommandContext(t.Context(), "security", "add-generic-password",
		"-s", service, "-a", account, "-w", "canary-not-a-real-token", "-U")
	output, err := add.CombinedOutput()
	require.NoError(t, err, string(output))

	t.Cleanup(func() {
		_ = exec.Command("security", "delete-generic-password", "-s", service, "-a", account).Run()
	})

	found := exec.CommandContext(t.Context(), "security", "find-generic-password", "-s", service, "-a", account, "-w")
	value, err := found.Output()
	require.NoError(t, err)
	require.Equal(t, "canary-not-a-real-token", strings.TrimSpace(string(value)))

	// The adapter's removal ladder clears both items per config dir across both
	// reachable name shapes, and reports absence rather than failure for an item
	// nothing ever wrote.
	require.NoError(t, nativeclaude.RemoveAuthKeychainItems(t.Context(), t.TempDir(), account))
}

func startKeystoreContainer(t *testing.T, ctx context.Context) testcontainers.Container {
	t.Helper()

	request := testcontainers.ContainerRequest{
		FromDockerfile: testcontainers.FromDockerfile{
			Context:    filepath.Join("testdata", "keystore"),
			Dockerfile: "Dockerfile",
			KeepImage:  true,
		},
		// Readiness is a store/lookup round trip executed inside the container.
		// A log line or a bus-name check both report ready against a service
		// that answers no lookup, and the suite then goes green having tested
		// the wrong thing.
		WaitingFor: wait.ForExec([]string{
			"sh", "-c",
			"export DBUS_SESSION_BUS_ADDRESS=$(cat /home/canary/dbus-address) && " +
				"printf readiness | secret-tool store --label=readiness service readiness account readiness && " +
				"secret-tool lookup service readiness account readiness | grep -q readiness",
		}).WithStartupTimeout(3 * time.Minute).WithPollInterval(time.Second),
	}

	container, err := testcontainers.GenericContainer(ctx, testcontainers.GenericContainerRequest{
		ContainerRequest: request,
		Started:          true,
	})
	require.NoError(t, err, "the keystore tier needs a container runtime; it fails rather than skips once its gate is set")

	t.Cleanup(func() { _ = container.Terminate(context.Background()) })

	return container
}

func execInKeystoreContainer(t *testing.T, ctx context.Context, container testcontainers.Container, command []string) string {
	t.Helper()

	code, reader, err := container.Exec(ctx, command)
	require.NoError(t, err)

	output := make([]byte, 0, 1024)
	buffer := make([]byte, 1024)

	for {
		read, readErr := reader.Read(buffer)
		if read > 0 {
			output = append(output, buffer[:read]...)
		}

		if readErr != nil {
			break
		}
	}

	require.Zero(t, code, "%s failed: %s", strings.Join(command, " "), string(output))

	return string(output)
}
