package claudeacp

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	acpcore "github.com/savid/acp-go-core"
	"github.com/savid/acp-go-core/wire"
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
	for _, state := range []string{"cancelled", "closed", "disconnected"} {
		t.Run(state, func(t *testing.T) {
			t.Parallel()
			s := &session{runtime: &runtime{}, turn: &turn{}}
			switch state {
			case "cancelled":
				s.turn.cancelled = true
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

// A launch that completes after the session began closing binds nothing and
// answers that the session is gone; the process it started goes through the
// shutdown ladder like any other generation.
func TestLaunchAfterCloseBindsNothing(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.initialize()
	sessionID := h.newSession().SessionId

	h.agent.mu.Lock()
	s := h.agent.sessions[sessionID]
	h.agent.mu.Unlock()

	require.NoError(t, s.close(h.ctx()))

	_, err := s.launch(h.ctx(), "")
	require.Equal(t, wire.UnknownSession(), err)

	s.mu.Lock()
	defer s.mu.Unlock()
	require.Nil(t, s.runtime)
}

func TestCloseJoinsFirstMirrorAndFencesOpening(t *testing.T) {
	t.Parallel()
	for _, agentClose := range []bool{false, true} {
		name := "close_session"
		if agentClose {
			name = "close_agent"
		}
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			store := &commitBarrier{SessionStore: acpcore.NewInMemorySessionStore(), entered: make(chan acpcore.SessionKey, 1), release: make(chan struct{})}
			release := sync.OnceFunc(func() { close(store.release) })
			t.Cleanup(release)
			store.block.Store(true)
			a := NewAgent(testOptions(t, WithSessionStore(store))...)
			t.Cleanup(func() { _ = a.Close() })
			rec := newRecorder()
			a.attach(rec, nil)
			request := acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber}
			withLifecycle()(&request)
			_, err := a.Initialize(t.Context(), request)
			require.NoError(t, err)
			cwd := t.TempDir()
			created := make(chan error, 1)
			go func() {
				_, createErr := a.NewSession(t.Context(), wire.NewSessionRequest(cwd))
				created <- createErr
			}()
			var key acpcore.SessionKey
			select {
			case key = <-store.entered:
			case <-time.After(testTimeout):
				t.Fatal("creation did not reach its first mirror")
			}
			s, err := a.session(t.Context(), acp.SessionId(key.SessionID))
			require.NoError(t, err)
			s.mu.Lock()
			rt := s.runtime
			s.mu.Unlock()
			closed := make(chan error, 1)
			go func() {
				if agentClose {
					closed <- a.Close()

					return
				}
				_, err := a.CloseSession(t.Context(), acp.CloseSessionRequest{SessionId: acp.SessionId(key.SessionID)})
				closed <- err
			}()
			require.Eventually(t, func() bool {
				s.mu.Lock()
				defer s.mu.Unlock()

				return s.closing
			}, testTimeout, time.Millisecond)
			select {
			case err := <-closed:
				t.Fatalf("close returned while the first mirror was blocked: %v", err)
			case <-time.After(100 * time.Millisecond):
			}
			release()
			select {
			case err := <-closed:
				require.NoError(t, err)
			case <-time.After(testTimeout):
				t.Fatal("close did not join creation")
			}
			select {
			case err := <-created:
				require.Error(t, err, "a closing session must refuse its opening publication")
			case <-time.After(testTimeout):
				t.Fatal("creation did not release its gate before cleanup")
			}
			require.False(t, s.lc.Active())
			before := len(rec.snapshot())
			require.Error(t, s.openStream(t.Context(), rt))
			require.Len(t, rec.snapshot(), before, "closed session published commands or a lifecycle snapshot")
			require.False(t, s.lc.Active())
		})
	}
}

func TestOpeningRejectsReplacedNativeGeneration(t *testing.T) {
	t.Parallel()
	a := NewAgent(testOptions(t)...)
	t.Cleanup(func() { _ = a.Close() })
	rec := newRecorder()
	a.attach(rec, nil)
	request := acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber}
	withLifecycle()(&request)
	_, err := a.Initialize(t.Context(), request)
	require.NoError(t, err)
	created, err := a.NewSession(t.Context(), wire.NewSessionRequest(t.TempDir()))
	require.NoError(t, err)
	s, err := a.session(t.Context(), created.SessionId)
	require.NoError(t, err)
	s.mu.Lock()
	stale := s.runtime
	s.mu.Unlock()
	transport, meta := prepareOpeningResponse(t)
	a.attach(rec, transport)
	require.NoError(t, a.scheduleOpen(transport.RequestContext(t.Context(), meta), s))
	s.stopRuntime(t.Context(), stale)
	require.False(t, s.lc.Active())
	fresh, err := s.ensureRuntime(t.Context())
	require.NoError(t, err)
	require.NotSame(t, stale, fresh)
	require.True(t, s.lc.Active())
	before := len(rec.snapshot())
	require.Error(t, s.openStream(t.Context(), stale))
	require.Len(t, rec.snapshot(), before, "stale deferred opening published on the replacement generation")
	require.True(t, s.lc.Active(), "stale opening fenced the replacement stream")
	finishOpeningResponse(t, transport, s.id)
	require.Len(t, rec.snapshot(), before, "stale hook published on the replacement generation")
	current, err := a.session(t.Context(), s.id)
	require.NoError(t, err)
	require.Same(t, s, current)
	require.True(t, s.lc.Active(), "stale hook closed the replacement stream")
}

// openingCallbackClient exercises a synchronous embedded callback into admission.
type openingCallbackClient struct {
	*recorder
	agent *Agent
}

func (c *openingCallbackClient) SessionUpdate(ctx context.Context, notification acp.SessionNotification) error {
	if err := c.agent.Cancel(ctx, acp.CancelNotification{SessionId: notification.SessionId}); err != nil {
		return err
	}

	return c.recorder.SessionUpdate(ctx, notification)
}

func TestOpeningAllowsSynchronousSessionCallback(t *testing.T) {
	t.Parallel()
	a := NewAgent(testOptions(t)...)
	t.Cleanup(func() { _ = a.Close() })
	rec := newRecorder()
	a.attach(&openingCallbackClient{recorder: rec, agent: a}, nil)
	request := acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber}
	withLifecycle()(&request)
	_, err := a.Initialize(t.Context(), request)
	require.NoError(t, err)
	_, err = a.NewSession(t.Context(), wire.NewSessionRequest(t.TempDir()))
	require.NoError(t, err)
	require.NotEmpty(t, rec.snapshot())
}

func prepareOpeningResponse(t *testing.T) (*wire.Transport, map[string]any) {
	t.Helper()
	transport := wire.NewTransport(strings.NewReader("{\"jsonrpc\":\"2.0\",\"id\":1,\"method\":\"session/new\",\"params\":{}}\n"), io.Discard)
	t.Cleanup(transport.Close)
	transport.Start()
	inbound, err := io.ReadAll(transport.Reader())
	require.NoError(t, err)

	var frame struct {
		Params acp.NewSessionRequest `json:"params"`
	}

	require.NoError(t, json.Unmarshal(inbound, &frame))

	return transport, frame.Params.Meta
}

func finishOpeningResponse(t *testing.T, transport *wire.Transport, id acp.SessionId) {
	t.Helper()
	_, err := transport.Writer().Write([]byte("{\"jsonrpc\":\"2.0\",\"id\":1,\"result\":{}}\n"))
	require.NoError(t, err)
	ctx, cancel := context.WithTimeout(t.Context(), testTimeout)
	defer cancel()
	require.NoError(t, transport.AwaitSession(ctx, id))
}

func TestDeferredOpeningFailureDetachesSession(t *testing.T) {
	t.Parallel()
	a := NewAgent(testOptions(t, WithConcurrencyLimits(ConcurrencyLimits{MaxActiveSessions: 1}))...)
	t.Cleanup(func() { _ = a.Close() })
	request := acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber}
	withLifecycle()(&request)
	_, err := a.Initialize(t.Context(), request)
	require.NoError(t, err)
	transport, meta := prepareOpeningResponse(t)
	a.attach(&firstOpenFailureClient{recorder: newRecorder()}, transport)
	newRequest := wire.NewSessionRequest(t.TempDir())
	newRequest.Meta = meta
	created, err := a.NewSession(t.Context(), newRequest)
	require.NoError(t, err)
	a.mu.Lock()
	s := a.sessions[created.SessionId]
	a.mu.Unlock()
	require.NotNil(t, s)
	finishOpeningResponse(t, transport, created.SessionId)
	require.False(t, s.lc.Active())
	a.mu.Lock()
	_, installed := a.sessions[created.SessionId]
	a.mu.Unlock()
	require.False(t, installed, "failed deferred publication retained the active slot")
	s.mu.Lock()
	closed := s.closing
	s.mu.Unlock()
	require.True(t, closed)
}
