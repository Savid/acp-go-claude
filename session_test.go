package claudeacp

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"

	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	acpcore "github.com/savid/acp-go-core"

	"github.com/savid/acp-go-core/wire"
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
	rows, err := loadEntries(t.Context(), store, acpcore.SessionKey{SessionID: string(session.SessionId)})
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
	created, err := h.conn.NewSession(h.ctx(), wire.NewSessionRequest(cwd))
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
	_, err = h.conn.LoadSession(h.ctx(), wire.LoadSessionRequest(created.SessionId, cwd))
	require.NoError(t, err)
	require.Contains(t, agentText(h.rec.snapshot()), "native reply")
	_, err = h.conn.CloseSession(h.ctx(), acp.CloseSessionRequest{SessionId: created.SessionId})
	require.NoError(t, err)
	require.NoError(t, os.Remove(path))
	_, err = h.conn.ResumeSession(h.ctx(), wire.ResumeSessionRequest(created.SessionId, cwd))
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
	response, err := h.prompt(session.SessionId, "again", promptMeta(3))
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, response.StopReason)
	// The generation the crash ended took its incarnation with it, so the
	// relaunch publishes on a new stream rather than the dead one.
	require.Equal(t, 2, lifecycleIncarnations(h.rec.snapshot()), "a lost generation never fenced its incarnation")
	require.True(t, reduceAll(t, session.SessionId, h.rec.snapshot()).Settled())
}

// The incarnation ends with the generation that produced it, not with the
// turn: claude leaves after its turn already settled, so the relaunch opens a
// new incarnation instead of publishing on the stream the dead process owned.
func TestNativeExitAfterASettledTurnFencesTheIncarnation(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.initialize(withLifecycle())
	session := h.newSession()

	response, err := h.prompt(session.SessionId, "EXIT", promptMeta(1))
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, response.StopReason)

	next, err := h.prompt(session.SessionId, "HELLO", promptMeta(2))
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, next.StopReason)
	require.Equal(t, 2, lifecycleIncarnations(h.rec.snapshot()), "a generation that ended after its turn never fenced its incarnation")
	require.True(t, reduceAll(t, session.SessionId, h.rec.snapshot()).Settled())
}

func TestConfigurationAndEnvironment(t *testing.T) {
	t.Parallel()
	home := t.TempDir()
	dump := filepath.Join(t.TempDir(), "env")
	store := acpcore.NewInMemorySessionStore()
	h := newHarness(t, WithHome(home), WithSessionStore(store))
	h.initialize()
	session := h.newSession(WithSessionClaudeOptions(NewClaudeOptions(WithClaudeEnv(map[string]string{"PATH": "/usr/bin::/bin", "HOME": "/tmp/session-home", "ACP_GO_CLAUDE_INTERNAL_TEST": "drop", "ACP_GO_OTHER_INTERNAL_TEST": "keep", "ACP_GO_CLAUDE_TEST_ENV_DUMP": dump}), WithClaudeExtraPathDirs("/tmp/extra"))))
	env, err := os.ReadFile(dump)
	require.NoError(t, err)
	require.Contains(t, string(env), "PATH=/tmp/extra:/usr/bin:/bin")
	require.Contains(t, string(env), "HOME=/tmp/session-home")
	require.Contains(t, string(env), "ACP_GO_OTHER_INTERNAL_TEST=keep")
	_, err = h.prompt(session.SessionId, "hello", nil)
	require.NoError(t, err)
	_, err = h.conn.SetSessionConfigOption(h.ctx(), SetModelRequest(session.SessionId, "haiku"))
	require.NoError(t, err)
	rows, err := loadEntries(t.Context(), store, acpcore.SessionKey{SessionID: string(session.SessionId), Subpath: "config"})
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
	rows, err := loadEntries(t.Context(), store, acpcore.SessionKey{SessionID: string(session.SessionId), Subpath: "config"})
	require.NoError(t, err)
	require.Len(t, rows, 1)
	var record sessionRecord
	require.NoError(t, json.Unmarshal(rows[0], &record))
	require.Equal(t, claude.SessionPath(filepath.Join(record.Cwd, "native-home"), record.Cwd, string(session.SessionId)), record.SessionFile)
	require.FileExists(t, record.SessionFile)
}

func TestLateDialogAfterCancellationIsRefused(t *testing.T) {
	t.Parallel()
	for _, state := range []string{"cancelled", "timed out", "closed", "disconnected"} {
		t.Run(state, func(t *testing.T) {
			t.Parallel()
			s := &session{runtime: &runtime{}, turn: &turn{}}
			switch state {
			case "cancelled":
				s.turn.cancelled = true
			case "timed out":
				s.turn.timedOut = true
			case "closed":
				s.closing = true
			case "disconnected":
				s.runtime = nil
			}
			ctx, cancel := context.WithCancelCause(t.Context())
			defer cancel(nil)
			release := s.registerDialog("late-native-request", cancel)
			require.ErrorIs(t, context.Cause(ctx), errDialogCancelled)
			release()
			s.callbacks.Wait()
		})
	}
}

func TestEnvironmentMergeOrderAndShadowing(t *testing.T) {
	t.Setenv("ACP_GO_CLAUDE_TEST_PROCESS_ONLY", "process")
	t.Setenv("ACP_GO_CLAUDE_TEST_AGENT_WINS", "process")
	t.Setenv("ACP_GO_CLAUDE_TEST_SESSION_WINS", "process")

	dump := filepath.Join(t.TempDir(), "env")
	base, shadow := t.TempDir(), t.TempDir()
	require.NoError(t, os.Symlink(os.Args[0], filepath.Join(base, "claude")))
	require.NoError(t, os.WriteFile(filepath.Join(shadow, "claude"), []byte("#!/bin/sh\nexit 3\n"), 0o700))

	h := newHarness(t,
		WithExecutablePath(""),
		WithEnv(map[string]string{
			fakeClaudeEnv:                     "1",
			"PATH":                            base,
			"ACP_GO_CLAUDE_TEST_AGENT_WINS":   "agent",
			"ACP_GO_CLAUDE_TEST_SESSION_WINS": "agent",
		}),
	)
	h.initialize()
	h.newSession(WithSessionClaudeOptions(NewClaudeOptions(
		WithClaudeEnv(map[string]string{
			"ACP_GO_CLAUDE_TEST_SESSION_WINS": "session",
			"ACP_GO_CLAUDE_TEST_EMPTY":        "",
			"ACP_GO_CLAUDE_TEST_ENV_DUMP":     dump,
		}),
		WithClaudeExtraPathDirs(shadow),
	)))

	env, err := os.ReadFile(dump)
	require.NoError(t, err, "the planted binary shadowed the harness on the session PATH")

	entries := strings.Split(string(env), "\n")
	require.Contains(t, entries, "ACP_GO_CLAUDE_TEST_PROCESS_ONLY=process", "a process variable must reach the harness unchanged")
	require.Contains(t, entries, "ACP_GO_CLAUDE_TEST_AGENT_WINS=agent", "WithEnv overrides the process environment")
	require.Contains(t, entries, "ACP_GO_CLAUDE_TEST_SESSION_WINS=session", "session env overrides WithEnv")
	require.Contains(t, entries, "ACP_GO_CLAUDE_TEST_EMPTY=", "an empty-valued key reaches the harness as KEY=")
	require.Contains(t, entries, "PATH="+shadow+string(os.PathListSeparator)+base)
}

func TestTwoSessionsKeepDistinctEnvironments(t *testing.T) {
	t.Parallel()

	first, second := filepath.Join(t.TempDir(), "first"), filepath.Join(t.TempDir(), "second")
	h := newHarness(t)
	h.initialize()

	for dump, value := range map[string]string{first: "one", second: "two"} {
		h.newSession(WithSessionClaudeOptions(NewClaudeOptions(WithClaudeEnv(map[string]string{
			"ACP_GO_CLAUDE_TEST_SESSION_VALUE": value,
			"ACP_GO_CLAUDE_TEST_ENV_DUMP":      dump,
		}))))
	}

	for dump, value := range map[string]string{first: "one", second: "two"} {
		env, err := os.ReadFile(dump)
		require.NoError(t, err)
		entries := strings.Split(string(env), "\n")
		require.Contains(t, entries, "ACP_GO_CLAUDE_TEST_SESSION_VALUE="+value)
		require.NotContains(t, strings.Join(entries, "\n"), "ACP_GO_CLAUDE_TEST_SESSION_VALUE="+map[string]string{"one": "two", "two": "one"}[value])
	}
}

func TestRequestedPermissionModeIsTheStoredCarrier(t *testing.T) {
	t.Parallel()

	store := acpcore.NewInMemorySessionStore()
	argv := filepath.Join(t.TempDir(), "argv")
	h := newHarness(t, WithSessionStore(store), WithEnv(map[string]string{fakeClaudeEnv: "1", fakeClaudeEnvArgvDump: argv}))
	h.initialize()

	session := h.newSession(WithSessionClaudeOptions(NewClaudeOptions(WithClaudePermissionMode(permissionModePlan))))

	args, err := os.ReadFile(argv)
	require.NoError(t, err)
	require.Contains(t, strings.Split(string(args), "\n"), permissionModePlan, "the requested mode never reached the child")

	rows, err := loadEntries(t.Context(), store, acpcore.SessionKey{SessionID: string(session.SessionId), Subpath: "config"})
	require.NoError(t, err)
	require.Len(t, rows, 1)

	var record sessionRecord
	require.NoError(t, json.Unmarshal(rows[0], &record))
	require.Equal(t, permissionModePlan, record.PermissionMode, "the carrier a relaunch is built from lost the requested mode")
}

func TestNativeCancelResolvesAPendingDialog(t *testing.T) {
	t.Parallel()

	s := &session{runtime: &runtime{}, id: "dialog-session", nativeID: "dialog-native"}
	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)

	release := s.registerDialog("native-request", cancel)
	defer release()

	s.handleEvent(t.Context(), s.runtime, claude.Event{Type: "control_cancel_request", RequestID: "native-request", SessionID: "dialog-native"})
	require.ErrorIs(t, context.Cause(ctx), errDialogCancelled)
}
