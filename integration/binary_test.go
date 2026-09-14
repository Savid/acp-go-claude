//go:build integration

package integration

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/coder/acp-go-sdk"
	claudeacp "github.com/savid/acp-go-claude"
	acpcore "github.com/savid/acp-go-core"
	"github.com/stretchr/testify/require"
)

func nativeHome(t *testing.T) string {
	t.Helper()
	home := t.TempDir()
	if source := os.Getenv("ACP_GO_CLAUDE_HOME"); source != "" {
		data, err := os.ReadFile(filepath.Join(source, ".credentials.json"))
		require.NoError(t, err, "native credentials source")
		require.NoError(t, os.WriteFile(filepath.Join(home, ".credentials.json"), data, 0o600))
	}
	return home
}
func TestNativeSmoke(t *testing.T) {
	if os.Getenv("ACP_GO_CLAUDE_RUN_INTEGRATION") != "1" {
		t.Skip("native integration gate")
	}
	h := newHarness(t, claudeacp.WithExecutablePath("claude"), claudeacp.WithEnv(nil), claudeacp.WithHome(nativeHome(t)), claudeacp.WithClaudeSettingSources([]string{}))
	h.initialize(withLifecycle())
	session := h.newSession()
	require.NotEmpty(t, session.SessionId)
	require.NotEmpty(t, session.ConfigOptions)
	config, err := h.conn.SetSessionConfigOption(h.ctx(), claudeacp.SetConfigOptionRequest(session.SessionId, "effort", "high"))
	require.NoError(t, err)
	require.NotEmpty(t, config.ConfigOptions)
	_, err = h.conn.CloseSession(h.ctx(), acp.CloseSessionRequest{SessionId: session.SessionId})
	require.NoError(t, err)
}
func TestNativeContinuation(t *testing.T) {
	if os.Getenv("ACP_GO_CLAUDE_RUN_LIVE_TOKENS") != "1" {
		t.Skip("live token gate")
	}
	home := nativeHome(t)
	cwd := t.TempDir()
	store := acpcore.NewInMemorySessionStore()
	model := os.Getenv("ACP_GO_CLAUDE_MODEL")
	if model == "" {
		model = "haiku"
	}
	h := newHarness(t, claudeacp.WithExecutablePath("claude"), claudeacp.WithEnv(nil), claudeacp.WithHome(home), claudeacp.WithSessionStore(store), claudeacp.WithDefaultModel(model), claudeacp.WithClaudeSettingSources([]string{}))
	h.initialize(withLifecycle())
	session, err := h.conn.NewSession(h.ctx(), claudeacp.NewSessionRequest(cwd))
	require.NoError(t, err)
	_, err = h.prompt(session.SessionId, "Remember the code word apricot-orbit. Reply with only that code word. Do not use tools.", promptMeta(1))
	require.NoError(t, err)
	require.Contains(t, agentText(h.rec.snapshot()), "apricot-orbit")
	_, err = h.conn.CloseSession(h.ctx(), acp.CloseSessionRequest{SessionId: session.SessionId})
	require.NoError(t, err)
	h.stop()
	command := exec.CommandContext(t.Context(), "claude", "--print", "--resume", string(session.SessionId), "--model", model, "--setting-sources=", "--output-format", "json", "Remember the second code word cobalt-lantern. Reply only with both code words. Do not use tools.")
	command.Dir = cwd
	command.Env = append(os.Environ(), "CLAUDE_CONFIG_DIR="+home)
	data, err := command.Output()
	require.NoError(t, err, "native resume")
	var result struct {
		Result    string `json:"result"`
		IsError   bool   `json:"is_error"`   //nolint:tagliatelle // Claude uses this native wire spelling.
		SessionID string `json:"session_id"` //nolint:tagliatelle // Claude uses this native wire spelling.
	}
	require.NoError(t, json.Unmarshal(data, &result))
	require.False(t, result.IsError)
	require.Equal(t, string(session.SessionId), result.SessionID)
	require.Contains(t, result.Result, "apricot-orbit")
	require.Contains(t, result.Result, "cobalt-lantern")
	restored := newHarness(t, claudeacp.WithExecutablePath("claude"), claudeacp.WithEnv(nil), claudeacp.WithHome(home), claudeacp.WithSessionStore(store), claudeacp.WithDefaultModel(model), claudeacp.WithClaudeSettingSources([]string{}))
	restored.initialize(withLifecycle())
	_, err = restored.conn.LoadSession(restored.ctx(), claudeacp.LoadSessionRequest(session.SessionId, cwd))
	require.NoError(t, err)
	replay := agentText(restored.rec.snapshot())
	require.Contains(t, replay, "apricot-orbit")
	require.Contains(t, replay, "cobalt-lantern")
	_, err = restored.prompt(session.SessionId, "Reply only with the two code words. Do not use tools.", promptMeta(2))
	require.NoError(t, err)
	text := agentText(restored.rec.snapshot())
	require.GreaterOrEqual(t, strings.Count(text, "cobalt-lantern"), 2)
}
