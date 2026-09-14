//go:build integration

package integration

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

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
	_, err = h.prompt(session.SessionId, "The project slug is apricot-orbit. Please acknowledge the project slug.", promptMeta(1))
	require.NoError(t, err)
	require.Contains(t, agentText(h.rec.snapshot()), "apricot-orbit")
	_, err = h.conn.CloseSession(h.ctx(), acp.CloseSessionRequest{SessionId: session.SessionId})
	require.NoError(t, err)
	h.stop()
	command := exec.CommandContext(t.Context(), "claude", "--print", "--resume", string(session.SessionId), "--model", model, "--setting-sources=", "--output-format", "json", "We chose cobalt-lantern as the release label. Please confirm both the project slug and release label.")
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
	_, err = restored.prompt(session.SessionId, "What project slug and release label did we choose?", promptMeta(2))
	require.NoError(t, err)
	text := agentText(restored.rec.snapshot())
	require.GreaterOrEqual(t, strings.Count(text, "cobalt-lantern"), 2)
}

func TestNativeCallbacksPathAndCancellation(t *testing.T) {
	if os.Getenv("ACP_GO_CLAUDE_RUN_LIVE_TOKENS") != "1" {
		t.Skip("live token gate")
	}
	home, cwd := nativeHome(t), t.TempDir()
	directories := []string{t.TempDir(), t.TempDir()}
	for index, dir := range directories {
		script := "#!/bin/sh\nprintf '%s\\n' 'marker-" + strconv.Itoa(index) + "' \"$PATH\"\n"
		require.NoError(t, os.WriteFile(filepath.Join(dir, "acpgogo-native-probe"), []byte(script), 0700))
	}
	h := newHarness(t, claudeacp.WithExecutablePath("claude"), claudeacp.WithEnv(nil), claudeacp.WithHome(home), claudeacp.WithDefaultModel("haiku"), claudeacp.WithClaudeSettingSources([]string{}))
	var questions atomic.Int32
	h.rec.elicit = func(acp.UnstableCreateElicitationRequest) (acp.UnstableCreateElicitationResponse, error) {
		questions.Add(1)
		return acp.UnstableCreateElicitationResponse{Accept: &acp.UnstableCreateElicitationAccept{Content: map[string]any{"q0": "cobalt"}}}, nil
	}
	h.initialize(withLifecycle(), withFormElicitation())
	session, err := h.conn.NewSession(h.ctx(), claudeacp.NewSessionRequest(cwd, claudeacp.WithSessionRawEvents(true), claudeacp.WithSessionClaudeOptions(claudeacp.NewClaudeOptions(claudeacp.WithClaudePermissionMode("default"), claudeacp.WithClaudeExtraPathDirs(directories[0])))))
	require.NoError(t, err)
	response, err := h.prompt(session.SessionId, "Use the Write tool to create proof.txt in the current directory containing exactly native-proof. Do not use any other tool. Reply DONE when finished.", promptMeta(1))
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, response.StopReason)
	data, err := os.ReadFile(filepath.Join(cwd, "proof.txt"))
	require.NoError(t, err)
	require.Equal(t, "native-proof", strings.TrimSpace(string(data)))
	h.rec.mu.Lock()
	permissions := len(h.rec.permissions)
	raw := len(h.rec.raw)
	h.rec.mu.Unlock()
	require.Positive(t, permissions)
	require.Positive(t, raw)
	_, err = h.prompt(session.SessionId, "Use AskUserQuestion to ask exactly one question: Pick a color. Offer two choices, amber and cobalt. After receiving my answer, reply with the chosen color. Do not use any other tool.", promptMeta(2))
	require.NoError(t, err)
	require.EqualValues(t, 1, questions.Load())
	for index, dir := range directories {
		if index > 0 {
			_, err = h.conn.CloseSession(h.ctx(), acp.CloseSessionRequest{SessionId: session.SessionId})
			require.NoError(t, err)
			_, err = h.conn.ResumeSession(h.ctx(), claudeacp.ResumeSessionRequest(session.SessionId, cwd, claudeacp.WithSessionRawEvents(true), claudeacp.WithSessionClaudeOptions(claudeacp.NewClaudeOptions(claudeacp.WithClaudeExtraPathDirs(dir)))))
			require.NoError(t, err)
		}
		before := len(h.rec.snapshot())
		_, err = h.prompt(session.SessionId, "Use the Bash tool to run the exact command acpgogo-native-probe. Do not set PATH, use an absolute command path, or run other commands. Reply DONE after it runs.", promptMeta(index+3))
		require.NoError(t, err)
		output := toolText(h.rec.snapshot()[before:])
		require.Contains(t, output, "marker-"+strconv.Itoa(index))
		require.Contains(t, output, dir+string(os.PathListSeparator))
		if index > 0 {
			require.NotContains(t, output, directories[0])
		}
	}
	done := make(chan acp.PromptResponse, 1)
	failed := make(chan error, 1)
	go func() {
		response, promptErr := h.prompt(session.SessionId, "Use the Bash tool to run exactly: printf started > sleep-started; sleep 60. Do not use a timeout or run it in the background. Reply DONE when it finishes.", promptMeta(5))
		done <- response
		failed <- promptErr
	}()
	require.Eventually(t, func() bool { _, statErr := os.Stat(filepath.Join(cwd, "sleep-started")); return statErr == nil }, 45*time.Second, 25*time.Millisecond)
	require.NoError(t, h.conn.Cancel(h.ctx(), acp.CancelNotification{SessionId: session.SessionId}))
	require.NoError(t, <-failed)
	require.Equal(t, acp.StopReasonCancelled, (<-done).StopReason)
	_, err = h.conn.UnstableDeleteSession(h.ctx(), acp.UnstableDeleteSessionRequest{SessionId: session.SessionId})
	require.NoError(t, err)
	list, err := h.conn.ListSessions(h.ctx(), acp.ListSessionsRequest{})
	require.NoError(t, err)
	require.Empty(t, list.Sessions)
}

func toolText(updates []acp.SessionNotification) string {
	var text strings.Builder
	for _, update := range updates {
		if tool := update.Update.ToolCallUpdate; tool != nil {
			for _, item := range tool.Content {
				if item.Content != nil && item.Content.Content.Text != nil {
					text.WriteString(item.Content.Content.Text.Text)
				}
			}
		}
	}
	return text.String()
}
