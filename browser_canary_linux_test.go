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
	browserCanaryUID     = 4242
	browserCanaryGID     = 4242
	browserCanaryScratch = "/canary/scratch"
	browserCanaryNative  = "/usr/local/bin/claude"
	browserCanaryState   = "/var/lib/acp-go-claude-browser-canary"
)

var browserCanaryLaunchers = []string{"open", "xdg-open", "x-www-browser", "www-browser", "sensible-browser"}

// TestRealNativeBrowserContainment drives the real Claude subscription login
// through the production ACP extension dispatcher. The container entrypoint
// traces this test's process tree and independently proves the native child
// execed only a production-generated browser shim, never an image decoy.
func TestRealNativeBrowserContainment(t *testing.T) {
	if os.Getenv("ACP_GO_CLAUDE_BROWSER_CANARY") != "1" {
		t.Fatal("real-native browser canary was selected without its required execution gate")
	}
	require.Equal(t, 0, os.Getuid(), "the canary must start as root so production isolation can enter uid 4242")
	require.FileExists(t, browserCanaryNative)

	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()

	require.NoError(t, os.MkdirAll(browserCanaryScratch, 0o711))
	require.NoError(t, os.Chmod(browserCanaryScratch, 0o711))
	authRoot := browserCanaryOwnedDir(t, "/home/canary/provider-auth-browser-canary", 0o700)
	stateRoot := browserCanaryOwnedDir(t, browserCanaryState, 0o700)

	agent := NewAgent(
		WithExecutablePath(browserCanaryNative),
		WithHome(stateRoot),
		WithScratchDir(browserCanaryScratch),
		WithProviderAuthRoot(authRoot),
		WithLogger(slog.New(slog.DiscardHandler)),
		WithEnv(map[string]string{
			providerAuthEnvAnthropicAPIKey:  "",
			providerAuthEnvAnthropicToken:   "",
			providerAuthEnvClaudeOAuthToken: "",
		}),
		WithProcessIsolation(ProcessIsolation{
			UID:                 browserCanaryUID,
			GID:                 browserCanaryGID,
			StandaloneOwnerID:   "claude-browser-canary",
			StandaloneStateRoot: stateRoot,
			BaseEnvironment: map[string]string{
				"BROWSER":         "/canary/decoys/open",
				homeEnv:           "/home/canary",
				"LOGNAME":         "canary",
				"PATH":            "/canary/decoys:/usr/local/bin:/usr/bin:/bin",
				"USER":            "canary",
				"XDG_CACHE_HOME":  "/home/canary/.cache",
				xdgConfigHomeEnv:  "/home/canary/.config",
				"XDG_DATA_HOME":   "/home/canary/.local/share",
				"XDG_RUNTIME_DIR": "/home/canary/.run",
				"XDG_STATE_HOME":  "/home/canary/.local/state",
			},
		}),
	)
	require.Equal(t, RuntimeContainmentAuthoritative, agent.ContainmentMode())

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

func browserCanaryOwnedDir(t *testing.T, path string, mode os.FileMode) string {
	t.Helper()
	require.NoError(t, os.Mkdir(path, mode))
	require.NoError(t, os.Chown(path, browserCanaryUID, browserCanaryGID))
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
