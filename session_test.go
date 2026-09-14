package claudeacp

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	acpcore "github.com/savid/acp-go-core"
	"github.com/stretchr/testify/require"
)

func TestPromptStreamsOnceAndCommits(t *testing.T) {
	t.Parallel()
	store := acpcore.NewInMemorySessionStore()
	h := newHarness(t, WithSessionStore(store))
	init := h.initialize(withLifecycle())
	require.Empty(t, init.AuthMethods)
	session := h.newSession()
	result, err := h.prompt(session.SessionId, "hello", promptMeta(1))
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, result.StopReason)
	require.Equal(t, "reply: hello", agentText(h.rec.snapshot()))
	require.Equal(t, []string{"lifecycle_snapshot", "prompt_accepted", "state_update:running", "state_update:idle"}, eventTypes(lifecycleEvents(h.rec.snapshot())))
	rows, err := store.Load(t.Context(), acpcore.SessionKey{SessionID: string(session.SessionId)})
	require.NoError(t, err)
	require.Len(t, rows, 2)
	require.Equal(t, 15, result.Usage.TotalTokens)
	state := reduceAll(t, session.SessionId, h.rec.snapshot())
	require.True(t, state.Settled())
}
func TestNativeContinuationAndHydration(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	cwd := t.TempDir()
	store := acpcore.NewInMemorySessionStore()
	h := newHarness(t, WithHome(home), WithSessionStore(store))
	h.initialize()
	created, err := h.conn.NewSession(h.ctx(), NewSessionRequest(cwd))
	require.NoError(t, err)
	listed, err := h.conn.ListSessions(h.ctx(), acp.ListSessionsRequest{Cwd: &cwd})
	require.NoError(t, err)
	require.Len(t, listed.Sessions, 1)
	require.Equal(t, created.SessionId, listed.Sessions[0].SessionId)
	_, err = h.prompt(created.SessionId, "first", nil)
	require.NoError(t, err)
	_, err = h.conn.CloseSession(h.ctx(), acp.CloseSessionRequest{SessionId: created.SessionId})
	require.NoError(t, err)
	canonical, err := filepath.EvalSymlinks(cwd)
	require.NoError(t, err)
	path := claude.SessionPath(home, canonical, string(created.SessionId))
	f := fakeClaude{id: string(created.SessionId), path: path}
	f.row("user", "native")
	f.row("assistant", "native reply")
	_, err = h.conn.LoadSession(h.ctx(), LoadSessionRequest(created.SessionId, cwd))
	require.NoError(t, err)
	require.Contains(t, agentText(h.rec.snapshot()), "native reply")
	_, err = h.conn.CloseSession(h.ctx(), acp.CloseSessionRequest{SessionId: created.SessionId})
	require.NoError(t, err)
	require.NoError(t, os.Remove(path))
	_, err = h.conn.ResumeSession(h.ctx(), ResumeSessionRequest(created.SessionId, cwd))
	require.NoError(t, err)
	rows, err := claude.ReadRows(path)
	require.NoError(t, err)
	require.Len(t, rows, 4)
}
func TestPermissionAndElicitationLifecycle(t *testing.T) {
	t.Parallel()
	for _, prompt := range []string{"PERMISSION", "ELICIT", "QUESTION"} {
		t.Run(prompt, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t)
			h.initialize(withLifecycle(), withFormElicitation())
			h.rec.elicit = func(acp.UnstableCreateElicitationRequest) (acp.UnstableCreateElicitationResponse, error) {
				return acp.UnstableCreateElicitationResponse{Accept: &acp.UnstableCreateElicitationAccept{Content: map[string]any{"color": "blue", "q0": "blue"}}}, nil
			}
			session := h.newSession()
			_, err := h.prompt(session.SessionId, prompt, promptMeta(1))
			require.NoError(t, err)
			kinds := eventTypes(lifecycleEvents(h.rec.snapshot()))
			require.Contains(t, kinds, "action_update:pending")
			require.Contains(t, kinds, "action_update:accepted")
			require.Equal(t, "state_update:idle", kinds[len(kinds)-1])
			state := reduceAll(t, session.SessionId, h.rec.snapshot())
			require.True(t, state.Settled())
		})
	}
}
func TestCancelAndProcessFailure(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.initialize(withLifecycle())
	session := h.newSession()
	done := make(chan error, 1)
	go func() {
		result, err := h.prompt(session.SessionId, "BLOCK", promptMeta(1))
		if err == nil && result.StopReason != acp.StopReasonCancelled {
			err = context.Canceled
		}
		done <- err
	}()
	h.rec.waitFor(t, func(updates []acp.SessionNotification) bool { return len(lifecycleEvents(updates)) >= 3 })
	require.NoError(t, h.conn.Cancel(h.ctx(), acp.CancelNotification{SessionId: session.SessionId}))
	select {
	case err := <-done:
		require.NoError(t, err)
	case <-time.After(testTimeout):
		t.Fatal("cancel did not settle")
	}
	_, err := h.prompt(session.SessionId, "CRASH", promptMeta(2))
	require.Equal(t, "claude_turn_failed", requestErrorData(t, err)["error"])
	_, err = h.prompt(session.SessionId, "again", promptMeta(3))
	require.NoError(t, err)
}
func TestConfigurationAndEnvironment(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	dump := filepath.Join(t.TempDir(), "env")
	store := acpcore.NewInMemorySessionStore()
	h := newHarness(t, WithHome(home), WithSessionStore(store))
	h.initialize()
	session := h.newSession(WithSessionClaudeOptions(NewClaudeOptions(WithClaudeEnv(map[string]string{"PATH": "/usr/bin::/bin", "HOME": "/tmp/session-home", "ACP_GO_CLAUDE_INTERNAL_TEST": "drop", "ACP_GO_PI_INTERNAL_TEST": "keep", "ACP_GO_CLAUDE_TEST_ENV_DUMP": dump}), WithClaudeExtraPathDirs("/tmp/extra"))))
	env, err := os.ReadFile(dump)
	require.NoError(t, err)
	require.Contains(t, string(env), "PATH=/tmp/extra:/usr/bin:/bin")
	require.Contains(t, string(env), "HOME=/tmp/session-home")
	require.Contains(t, string(env), "ACP_GO_PI_INTERNAL_TEST=keep")
	_, err = h.prompt(session.SessionId, "hello", nil)
	require.NoError(t, err)
	_, err = h.conn.SetSessionConfigOption(h.ctx(), SetModelRequest(session.SessionId, "haiku"))
	require.NoError(t, err)
	rows, err := store.Load(t.Context(), acpcore.SessionKey{SessionID: string(session.SessionId), Subpath: "config"})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	var record sessionRecord
	require.NoError(t, json.Unmarshal(rows[0], &record))
	require.Equal(t, "haiku", record.Model)
}

func TestRelativeNativeHomeUsesSessionCwd(t *testing.T) {
	t.Parallel()
	store := acpcore.NewInMemorySessionStore()
	h := newHarness(t, WithHome(""), WithSessionStore(store), WithEnv(map[string]string{fakeClaudeEnv: "1", claude.EnvConfigDir: "native-home"}))
	h.initialize()
	session := h.newSession()
	_, err := h.prompt(session.SessionId, "relative-home", nil)
	require.NoError(t, err)
	rows, err := store.Load(t.Context(), acpcore.SessionKey{SessionID: string(session.SessionId), Subpath: "config"})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	var record sessionRecord
	require.NoError(t, json.Unmarshal(rows[0], &record))
	require.Equal(t, claude.SessionPath(filepath.Join(record.Cwd, "native-home"), record.Cwd, string(session.SessionId)), record.SessionFile)
	require.FileExists(t, record.SessionFile)
}
