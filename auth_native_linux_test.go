//go:build linux && browsercanary

package claudeacp

import (
	"context"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/require"
)

const (
	browserCanaryScratch = "/canary/scratch"
	browserCanaryNative  = "/usr/local/bin/claude"
)

var browserCanaryLaunchers = []string{"open", "xdg-open", "x-www-browser", "www-browser", "sensible-browser"}

func TestRealNativeBrowserLaunchIsNeutralized(t *testing.T) {
	if os.Getenv("ACP_GO_CLAUDE_BROWSER_CANARY") != "1" {
		t.Fatal("real-native browser canary was selected without its required execution gate")
	}
	require.FileExists(t, browserCanaryNative)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	require.NoError(t, os.MkdirAll(browserCanaryScratch, 0o700))
	authRoot := browserCanaryDir(t, "/canary/provider-auth")
	stateRoot := browserCanaryDir(t, "/canary/claude-home")

	agent := NewAgent(
		WithExecutablePath(browserCanaryNative),
		WithHome(stateRoot),
		WithScratchDir(browserCanaryScratch),
		WithProviderAuthRoot(authRoot),
		WithLogger(slog.New(slog.DiscardHandler)),
		WithEnv(map[string]string{
			"BROWSER":                       "/canary/decoys/open",
			"PATH":                          "/canary/decoys:/usr/local/bin:/usr/bin:/bin",
			providerAuthEnvAnthropicAPIKey:  "",
			providerAuthEnvAnthropicToken:   "",
			providerAuthEnvClaudeOAuthToken: "",
		}),
	)

	sessionID := acp.SessionId("browser-canary-session")
	agent.sessions[sessionID] = &agentSession{agent: agent, id: sessionID}

	enumerated, err := agent.HandleExtensionMethod(ctx, AuthMethodsMethod, browserCanaryParams(t, map[string]any{
		"sessionId": string(sessionID),
	}))
	require.NoError(t, err)
	methods, ok := enumerated.(authMethodsResult)
	require.True(t, ok)
	require.Equal(t, authMethodLogin, methods.Providers[authProviderID][0].ID)

	authorized, err := agent.HandleExtensionMethod(ctx, AuthAuthorizeMethod, browserCanaryParams(t, map[string]any{
		"sessionId":          string(sessionID),
		"providerId":         authProviderID,
		"connectionId":       "browser-canary-connection",
		"methodsGeneration":  methods.Generation,
		authFieldMethod:      authMethodLogin,
		"authorizeRequestId": "browser-canary-request",
	}))
	require.NoError(t, err)
	flow, ok := authorized.(authAuthorizeResult)
	require.True(t, ok)
	require.NotEmpty(t, flow.FlowID)
	require.True(t, strings.HasPrefix(flow.URL, "https://claude.com/"))

	shim := requireLiveBrowserCanaryShim(t, "acp-go-claude-browser-shim-")
	for _, name := range browserCanaryLaunchers {
		require.FileExists(t, filepath.Join(shim, name))
	}
	require.Eventually(t, func() bool {
		for _, name := range browserCanaryLaunchers {
			info, statErr := os.Stat(filepath.Join(shim, name))
			if statErr != nil {
				continue
			}
			stat, statOK := info.Sys().(*syscall.Stat_t)
			if statOK && stat.Atim.Nano() > stat.Mtim.Nano() {
				return true
			}
		}

		return false
	}, 10*time.Second, 10*time.Millisecond, "native login did not execute a generated browser launcher")

	_, err = agent.HandleExtensionMethod(ctx, AuthCancelMethod, browserCanaryParams(t, map[string]any{
		"sessionId":  string(sessionID),
		"providerId": authProviderID,
		"flowId":     flow.FlowID,
	}))
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		matches, globErr := filepath.Glob(filepath.Join(browserCanaryScratch, "acp-go-claude-browser-shim-*"))
		return globErr == nil && len(matches) == 0
	}, 10*time.Second, 25*time.Millisecond, "provider-auth cancellation left its browser shim behind")
}

func browserCanaryParams(t *testing.T, value map[string]any) json.RawMessage {
	t.Helper()
	raw, err := json.Marshal(value)
	require.NoError(t, err)

	return raw
}

func browserCanaryDir(t *testing.T, path string) string {
	t.Helper()
	require.NoError(t, os.MkdirAll(path, 0o700))
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(path)) })

	return path
}

func requireLiveBrowserCanaryShim(t *testing.T, prefix string) string {
	t.Helper()

	var matches []string
	require.Eventually(t, func() bool {
		var err error
		matches, err = filepath.Glob(filepath.Join(browserCanaryScratch, prefix+"*"))
		return err == nil && len(matches) == 1
	}, 10*time.Second, 25*time.Millisecond, "production auth created no unique live browser shim")

	return matches[0]
}
