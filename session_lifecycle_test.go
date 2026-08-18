package claudeacp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/stretchr/testify/require"
)

func TestSessionLifecycleBranches(t *testing.T) {
	ctx := context.Background()
	agent := NewAgent()
	session, cleanup := newStartedAgentSessionForTest(t, agent, "session-1")
	defer cleanup()

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	session.turn = make(chan struct{}, 1)
	session.turn <- struct{}{}
	release, err := session.acquireTurn(cancelled)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, release)

	// A second prompt to a busy session (its single turn slot taken) is refused
	// with session_prompt backpressure; per-session turn capacity is fixed at 1.
	session.turn = make(chan struct{}, sessionTurnCapacity)
	session.turn <- struct{}{}
	release, err = session.acquireTurn(ctx)
	requireBackpressureLimit(t, err, "session_prompt")
	require.Nil(t, release)

	session.turn = make(chan struct{}, 2)
	release, err = session.acquireExclusiveTurn(ctx)
	require.NoError(t, err)
	require.Len(t, session.turn, 2)
	release()
	require.Empty(t, session.turn)

	partialCtx, partialCancel := context.WithCancel(ctx)
	session.turn = make(chan struct{}, 2)
	session.turnAcquiredHook = func(acquired int) {
		if acquired == 1 {
			partialCancel()
			session.turn <- struct{}{}
		}
	}
	release, err = session.acquireExclusiveTurn(partialCtx)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, release)
	<-session.turn
	require.Empty(t, session.turn)
	session.turnAcquiredHook = nil

	innerCancelCtx := &nthDoneContext{
		done:       make(chan struct{}),
		closeAfter: 3,
	}
	session.turn = make(chan struct{}, 2)
	session.turnAcquiredHook = func(acquired int) {
		if acquired == 1 {
			session.turn <- struct{}{}
		}
	}
	release, err = session.acquireExclusiveTurn(innerCancelCtx)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, release)
	<-session.turn
	require.Empty(t, session.turn)
	session.turnAcquiredHook = nil

	session.turn = make(chan struct{})
	release, err = session.acquireExclusiveTurn(cancelled)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, release)

	session.turn = nil
	require.NotNil(t, session.turnQueue())

	permissionCancelled := false
	session.permissionCancel = map[string]*permissionRequestCancel{
		"p1": {cancel: func() { permissionCancelled = true }},
	}
	cancels := session.cancelPermissionRequestsLocked()
	require.Len(t, cancels, 1)
	cancels[0]()
	require.True(t, permissionCancelled)
	require.Empty(t, session.permissionCancel)

	closeSession, closeCleanup := newStartedAgentSessionForTest(t, agent, "close")
	defer closeCleanup()
	closeSession.turn = make(chan struct{}, 1)
	closeSession.turn <- struct{}{}

	waitCtx, stopWaiting := context.WithTimeout(ctx, time.Millisecond)
	defer stopWaiting()

	err = closeSession.Close(waitCtx)
	require.Error(t, err)
}

func TestSessionCancelAndCloseEdgeBranches(t *testing.T) {
	ctx := context.Background()
	agent := NewAgent()
	session, cleanup := newStartedAgentSessionForTest(t, agent, "cancel")
	defer cleanup()

	permissionCancelled := false
	turnCancelled := false
	session.cancel = func() { turnCancelled = true }
	session.permissionCancel = map[string]*permissionRequestCancel{
		"tool": {cancel: func() { permissionCancelled = true }},
	}
	require.NoError(t, session.Cancel(ctx))
	require.True(t, permissionCancelled)
	require.True(t, session.turnCancelled)
	// Cancel cancels the local turn context synchronously, before the native
	// interrupt resolves.
	require.True(t, turnCancelled)

	notStarted := &agentSession{client: claude.NewClient(nil, claude.Options{}, newFakeClaudeTransport())}
	require.ErrorIs(t, notStarted.Cancel(ctx), claude.ErrClientNotStarted)
	require.ErrorIs(t, (&agentSession{cancel: func() {}}).Cancel(ctx), claude.ErrClientNotStarted)
	require.NoError(t, (&agentSession{}).closeNativeClient(nil))
	require.NoError(t, (&agentSession{mcpRefreshPending: true}).refreshMCPRegistry(ctx))

	closeSession, closeCleanup := newStartedAgentSessionForTest(t, agent, "close-edge")
	defer closeCleanup()
	cancelled := false
	closeSession.cancel = func() { cancelled = true }
	closeSession.materialized = &materializedSession{configDir: string([]byte{0})}
	err := closeSession.Close(ctx)
	require.Error(t, err)
	require.True(t, cancelled)

	closeErrSession, closeErrCleanup := newStartedAgentSessionForTest(t, agent, "close-error")
	defer closeErrCleanup()
	closeErrSession.client = claude.NewClient(nil, claude.Options{}, &closeErrTransport{Transport: newFakeClaudeTransport(), err: errors.New("close failed")})
	require.NoError(t, closeErrSession.client.Start(ctx))
	err = closeErrSession.Close(ctx)
	require.ErrorContains(t, err, "close failed")
}

func TestSessionCancelInterruptDetachedFromCallerContext(t *testing.T) {
	agent := NewAgent()
	session, cleanup := newStartedAgentSessionForTest(t, agent, "detached-interrupt")
	defer cleanup()

	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()

	// The native interrupt runs under a bounded background-derived context, so a
	// cancelled caller context does not abort it.
	require.NoError(t, session.Cancel(cancelledCtx))
}

func TestSessionCancelClosesNativeClientBeforeReturn(t *testing.T) {
	agent := NewAgent()
	transport := newFakeClaudeTransport()
	client := claude.NewClient(nil, claude.Options{}, transport)
	require.NoError(t, client.Start(t.Context()))
	t.Cleanup(func() { _ = client.Close() })
	session := &agentSession{agent: agent, id: "contained-cancel", client: client}
	session.cancel = func() {}

	require.NoError(t, session.Cancel(t.Context()))
	require.Equal(t, 1, transport.CloseCalls())
	require.False(t, session.client.Alive(), "Cancel returned before the native client was closed")
}

func TestSessionCancelCancelsPendingElicitations(t *testing.T) {
	ctx := context.Background()
	agent := NewAgent()
	session, cleanup := newStartedAgentSessionForTest(t, agent, "elicit-cancel")
	defer cleanup()

	permissionCancelled := false
	elicitationCancelled := false
	session.permissionCancel = map[string]*permissionRequestCancel{
		"tool": {cancel: func() { permissionCancelled = true }},
	}
	session.elicitationCancel = map[int64]*elicitationRequestCancel{
		1: {cancel: func() { elicitationCancelled = true }},
	}

	require.NoError(t, session.Cancel(ctx))
	require.True(t, permissionCancelled)
	require.True(t, elicitationCancelled)
	require.True(t, session.turnCancelled)
	require.Empty(t, session.permissionCancel)
	require.Empty(t, session.elicitationCancel)
}

func TestSessionCloseResolvesPendingInteractionsFirst(t *testing.T) {
	ctx := context.Background()
	agent := NewAgent()
	session, cleanup := newStartedAgentSessionForTest(t, agent, "close-pending")
	defer cleanup()

	permissionCancelled := false
	elicitationCancelled := false
	session.permissionCancel = map[string]*permissionRequestCancel{
		"tool": {cancel: func() { permissionCancelled = true }},
	}
	session.elicitationCancel = map[int64]*elicitationRequestCancel{
		1: {cancel: func() { elicitationCancelled = true }},
	}

	require.NoError(t, session.Close(ctx))
	require.True(t, permissionCancelled)
	require.True(t, elicitationCancelled)
	require.True(t, session.turnCancelled)
	require.Empty(t, session.permissionCancel)
	require.Empty(t, session.elicitationCancel)
}

func TestRegisterElicitationCancellation(t *testing.T) {
	ctx := context.Background()
	agent := NewAgent()
	session, cleanup := newStartedAgentSessionForTest(t, agent, "elicit-register")
	defer cleanup()

	elicitationCtx, finish := session.registerElicitation(ctx)
	require.NoError(t, elicitationCtx.Err())
	require.Len(t, session.elicitationCancel, 1)

	require.NoError(t, session.Cancel(ctx))
	require.Error(t, elicitationCtx.Err())
	require.Empty(t, session.elicitationCancel)

	finish()

	// A registration made after the turn is already cancelled is cancelled at once.
	preCancelledCtx, preFinish := session.registerElicitation(ctx)
	defer preFinish()
	require.Error(t, preCancelledCtx.Err())
}

type nthDoneContext struct {
	done       chan struct{}
	closeAfter int
	calls      int
	once       sync.Once
}

func (c *nthDoneContext) Deadline() (time.Time, bool) {
	return time.Time{}, false
}

func (c *nthDoneContext) Done() <-chan struct{} {
	c.calls++
	if c.calls >= c.closeAfter {
		c.once.Do(func() { close(c.done) })
	}

	return c.done
}

func (c *nthDoneContext) Err() error {
	select {
	case <-c.done:
		return context.Canceled
	default:
		return nil
	}
}

func (c *nthDoneContext) Value(any) any {
	return nil
}

const processContainmentHelperArg = "--acp-go-claude-containment-helper"

var actualProcessFixtureUID atomic.Uint32

// TestCancelContainsActualNativeDescendants exercises the complete adapter
// cancellation path against a real OS process tree. The protocol stand-in
// deliberately acknowledges interrupt without stopping a setsid tool process
// that ignores INT and TERM; only transport containment can make this test pass.
func TestProcessIsolationActualCancelContainsNativeDescendants(t *testing.T) {
	fixture := newActualProcessFixture(t)
	agent, sessionID := fixture.agent, fixture.session.id

	promptResult := make(chan struct {
		response acp.PromptResponse
		err      error
	}, 1)
	go func() {
		response, promptErr := agent.Prompt(
			context.Background(),
			TextPromptRequest(sessionID, "turn-cancel", "run the long tool"),
		)
		promptResult <- struct {
			response acp.PromptResponse
			err      error
		}{response: response, err: promptErr}
	}()

	childPID := fixture.waitForChild(t)
	require.NoError(t, agent.Cancel(t.Context(), CancelRequest(sessionID, "turn-cancel")))
	require.False(t, unixProcessExists(childPID),
		"session/cancel returned while the native tool descendant was still alive")
	fixture.assertNoDelayedSideEffect(t)

	select {
	case result := <-promptResult:
		require.NoError(t, result.err)
		require.Equal(t, acp.StopReasonCancelled, result.response.StopReason)
	case <-time.After(5 * time.Second):
		t.Fatal("cancelled prompt did not settle")
	}

	fixture.assertLazyResume(t, "turn-resume")
}

func TestProcessIsolationActualTimeoutContainsNativeDescendants(t *testing.T) {
	fixture := newActualProcessFixture(t, WithTurnTimeout(500*time.Millisecond))

	result := make(chan error, 1)
	go func() {
		_, err := fixture.agent.Prompt(
			context.Background(),
			TextPromptRequest(fixture.session.id, "turn-timeout", "run the long tool"),
		)
		result <- err
	}()

	childPID := fixture.waitForChild(t)
	select {
	case err := <-result:
		requireTurnFailure(t, err, -32603, failureCauseTimeout, "")
	case <-time.After(5 * time.Second):
		t.Fatal("timed-out prompt did not settle")
	}
	require.False(t, unixProcessExists(childPID),
		"timeout settled while the native tool descendant was still alive")
	fixture.assertNoDelayedSideEffect(t)
	fixture.assertLazyResume(t, "turn-after-timeout")
}

func TestProcessIsolationActualParentCancelContainsNativeDescendants(t *testing.T) {
	fixture := newActualProcessFixture(t)
	promptCtx, cancelPrompt := context.WithCancel(context.Background())

	result := make(chan struct {
		response acp.PromptResponse
		err      error
	}, 1)
	go func() {
		response, err := fixture.agent.Prompt(
			promptCtx,
			TextPromptRequest(fixture.session.id, "turn-parent-cancel", "run the long tool"),
		)
		result <- struct {
			response acp.PromptResponse
			err      error
		}{response: response, err: err}
	}()

	childPID := fixture.waitForChild(t)
	cancelPrompt()

	select {
	case settled := <-result:
		require.NoError(t, settled.err)
		require.Equal(t, acp.StopReasonCancelled, settled.response.StopReason)
	case <-time.After(5 * time.Second):
		t.Fatal("parent-cancelled prompt did not settle")
	}
	require.False(t, unixProcessExists(childPID),
		"parent cancellation settled while the native tool descendant was still alive")
	fixture.assertNoDelayedSideEffect(t)
	fixture.assertLazyResume(t, "turn-after-parent-cancel")
}

type actualProcessFixture struct {
	agent        *Agent
	session      *agentSession
	pidFile      string
	argsFile     string
	triggerFile  string
	sentinelFile string
}

func newActualProcessFixture(t *testing.T, agentOptions ...Option) actualProcessFixture {
	t.Helper()
	if runtime.GOOS != "linux" {
		t.Skip("authoritative detached-descendant containment requires Linux")
	}
	if os.Geteuid() != 0 {
		t.Skip("authoritative detached-descendant containment requires root")
	}
	selfNamespace, selfErr := os.Readlink("/proc/self/ns/pid")
	initNamespace, initErr := os.Readlink("/proc/1/ns/pid")
	if selfErr != nil || initErr != nil || selfNamespace != initNamespace ||
		selfNamespace != "pid:[4026531836]" && os.Getpid() != 1 {
		t.Skip("authoritative detached-descendant containment requires the initial PID namespace")
	}

	uid := 64300 + actualProcessFixtureUID.Add(1)
	gid := uid
	dir := createActualProcessStateRoot(t, uid, gid)
	pidFile := filepath.Join(dir, "child.pid")
	launchFile := filepath.Join(dir, "launch-count")
	argsFile := filepath.Join(dir, "launch-args")
	triggerFile := filepath.Join(dir, "delayed-trigger")
	sentinelFile := filepath.Join(dir, "delayed-sentinel")
	executable, err := os.Executable()
	require.NoError(t, err)
	executablePayload, err := os.ReadFile(executable)
	require.NoError(t, err)
	helperExecutable := filepath.Join(dir, "claude-containment-helper.test")
	require.NoError(t, os.WriteFile(helperExecutable, executablePayload, 0o700))
	require.NoError(t, os.Chown(helperExecutable, int(uid), int(gid)))

	wrapper := filepath.Join(dir, "claude")
	wrapperBody := fmt.Sprintf(
		"#!/bin/sh\nexec %s -test.run '^TestClaudeProcessContainmentHelper$' -- %s \"$@\"\n",
		strconv.Quote(helperExecutable), processContainmentHelperArg,
	)
	require.NoError(t, os.WriteFile(wrapper, []byte(wrapperBody), 0o700))
	require.NoError(t, os.Chown(wrapper, int(uid), int(gid)))

	agent := NewAgent(agentOptions...)
	agent.setConnection(newRecordingAgentClient())
	sessionID := acp.SessionId("11111111-1111-4111-8111-111111111111")
	options := claude.Options{
		CLIPath: wrapper, Cwd: dir, SessionID: string(sessionID),
		ProcessIsolation: &claude.ProcessIsolation{
			UID: uid, GID: gid, BaseEnvironment: map[string]string{"PATH": "/usr/bin:/bin"},
			StandaloneOwnerID:   "claude-session-containment-" + strconv.FormatUint(uint64(uid), 10),
			StandaloneStateRoot: dir,
		},
		Env: map[string]string{
			"ACP_TEST_CHILD_PID":    pidFile,
			"ACP_TEST_LAUNCH_COUNT": launchFile,
			"ACP_TEST_LAUNCH_ARGS":  argsFile,
			"ACP_TEST_TRIGGER":      triggerFile,
			"ACP_TEST_SENTINEL":     sentinelFile,
		},
	}
	client := agent.newClaudeClient(agent.log, options)
	require.NoError(t, client.Start(t.Context()))

	session := &agentSession{
		agent:             agent,
		id:                sessionID,
		cwd:               dir,
		client:            client,
		clientOptions:     options,
		canRelaunch:       true,
		turn:              make(chan struct{}, sessionTurnCapacity),
		mirror:            newSessionMirror(agent.log, nil, dir, nil),
		contextWindowSize: 200000,
	}
	agent.mu.Lock()
	agent.sessions[sessionID] = session
	agent.mu.Unlock()
	t.Cleanup(func() { _ = session.Close(context.Background()) })

	return actualProcessFixture{
		agent: agent, session: session, pidFile: pidFile,
		argsFile: argsFile, triggerFile: triggerFile, sentinelFile: sentinelFile,
	}
}

func createActualProcessStateRoot(t *testing.T, uid, gid uint32) string {
	t.Helper()
	base := "/var/lib/acp-go-claude-session-test"
	require.NoError(t, os.MkdirAll(base, 0o711))
	require.NoError(t, os.Chmod(base, 0o711))
	info, err := os.Stat(base)
	require.NoError(t, err)
	stat, ok := info.Sys().(*syscall.Stat_t)
	require.True(t, ok)
	require.True(t, info.IsDir())
	require.Equal(t, os.FileMode(0o711), info.Mode().Perm())
	require.Equal(t, uint32(0), stat.Uid)
	require.Equal(t, uint32(0), stat.Gid)

	dir := filepath.Join(base, strconv.FormatUint(uint64(uid), 10))
	require.NoError(t, os.MkdirAll(dir, 0o700))
	require.NoError(t, os.Chown(dir, int(uid), int(gid)))
	require.NoError(t, os.Chmod(dir, 0o700))
	for _, name := range []string{"child.pid", "launch-count", "launch-args", "delayed-trigger", "delayed-sentinel", "claude", "claude-containment-helper.test"} {
		err = os.Remove(filepath.Join(dir, name))
		require.True(t, err == nil || errors.Is(err, os.ErrNotExist), "remove stale fixture file %q: %v", name, err)
	}

	return dir
}

func (f actualProcessFixture) waitForChild(t *testing.T) int {
	t.Helper()
	require.Eventually(t, func() bool {
		_, statErr := os.Stat(f.pidFile)

		return statErr == nil
	}, 5*time.Second, 10*time.Millisecond)
	raw, err := os.ReadFile(f.pidFile)
	require.NoError(t, err)
	fields := strings.Fields(string(raw))
	require.Len(t, fields, 6)
	values := make([]int, len(fields))
	for index, field := range fields {
		values[index], err = strconv.Atoi(field)
		require.NoError(t, err)
	}
	childPID, childPGID, childSID := values[0], values[1], values[2]
	rootPID, rootPGID, rootSID := values[3], values[4], values[5]
	require.True(t, unixProcessExists(childPID))
	require.Equal(t, childPID, childPGID, "setsid child must lead its own process group")
	require.Equal(t, childPID, childSID, "setsid child must lead its own session")
	require.NotEqual(t, rootPID, childPID)
	require.NotEqual(t, rootPGID, childPGID)
	require.NotEqual(t, rootSID, childSID)

	return childPID
}

func (f actualProcessFixture) assertNoDelayedSideEffect(t *testing.T) {
	t.Helper()
	require.NoError(t, os.WriteFile(f.triggerFile, []byte("settled"), 0o600))
	time.Sleep(500 * time.Millisecond)
	require.NoFileExists(t, f.sentinelFile,
		"a detached native descendant performed a delayed side effect after settlement")
}

func (f actualProcessFixture) assertLazyResume(t *testing.T, turnNonce string) {
	t.Helper()
	response, err := f.agent.Prompt(
		t.Context(),
		TextPromptRequest(f.session.id, turnNonce, "continue after containment"),
	)
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, response.StopReason)

	launchArgs, err := os.ReadFile(f.argsFile)
	require.NoError(t, err)
	lastLaunch := strings.TrimSpace(string(launchArgs))
	require.Contains(t, lastLaunch, "--resume "+string(f.session.id))
	require.NotContains(t, lastLaunch, "--fork-session")
}

func unixProcessExists(pid int) bool {
	return exec.Command("sh", "-c", "kill -0 \"$1\"", "sh", strconv.Itoa(pid)).Run() == nil
}

// TestClaudeProcessContainmentHelper is executed through a shell wrapper as a
// provider-free Claude control-protocol stand-in. Its first launch leaks an
// interrupt-ignoring native tool descendant; its resumed launch completes.
func TestClaudeProcessContainmentHelper(t *testing.T) {
	args := helperArgumentsAfterSeparator(os.Args)
	if len(args) == 0 || args[0] != processContainmentHelperArg {
		return
	}
	args = args[1:]

	if len(args) == 1 && args[0] == "--version" {
		fmt.Println("2.1.210 (Claude Code)")
		os.Exit(0)
	}

	launch := incrementProcessTestLaunch(os.Getenv("ACP_TEST_LAUNCH_COUNT"))
	_ = os.WriteFile(os.Getenv("ACP_TEST_LAUNCH_ARGS"), []byte(strings.Join(args, " ")), 0o600)

	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var message map[string]any
		if json.Unmarshal(scanner.Bytes(), &message) != nil {
			continue
		}

		switch message["type"] {
		case "control_request":
			request, _ := message["request"].(map[string]any)
			subtype, _ := request["subtype"].(string)
			payload := map[string]any{}

			switch subtype {
			case "initialize":
				payload = map[string]any{
					"models": []any{map[string]any{"value": "sonnet", "displayName": "Sonnet"}},
				}
			case "get_context_usage":
				payload = map[string]any{"totalTokens": 1, "maxTokens": 200000}
			}

			_ = encoder.Encode(map[string]any{
				"type": "control_response",
				"response": map[string]any{
					"request_id": message["request_id"],
					"subtype":    "success",
					"response":   payload,
				},
			})
		case "user":
			if launch == 1 {
				_ = startDetachedContainmentChild(
					os.Args[0],
					os.Getenv("ACP_TEST_CHILD_PID"),
					os.Getenv("ACP_TEST_TRIGGER"),
					os.Getenv("ACP_TEST_SENTINEL"),
				)

				continue
			}

			_ = encoder.Encode(map[string]any{
				"type":        "result",
				"subtype":     "success",
				"is_error":    false,
				"stop_reason": "end_turn",
			})
		}
	}

	os.Exit(0)
}

func helperArgumentsAfterSeparator(args []string) []string {
	for index, arg := range args {
		if arg == "--" {
			return args[index+1:]
		}
	}

	return nil
}

func incrementProcessTestLaunch(path string) int {
	launch := 0
	if raw, err := os.ReadFile(path); err == nil {
		launch, _ = strconv.Atoi(strings.TrimSpace(string(raw)))
	}

	launch++
	_ = os.WriteFile(path, []byte(strconv.Itoa(launch)), 0o600)

	return launch
}

// closeHookTransport runs a hook at the moment the native transport is torn
// down, which is how these tests linearize a same-id install against a close
// that is already running.
type closeHookTransport struct {
	claude.Transport

	onClose func()
}

func (t *closeHookTransport) Close() error {
	if t.onClose != nil {
		t.onClose()
	}

	return t.Transport.Close()
}

func newCloseStateSession(t *testing.T, transport claude.Transport) *agentSession {
	t.Helper()

	client := claude.NewClient(nil, claude.Options{}, transport)
	require.NoError(t, client.Start(t.Context()))

	return &agentSession{
		agent:  NewAgent(),
		id:     "session-1",
		client: client,
		turn:   make(chan struct{}, sessionTurnCapacity),
	}
}

// TestSessionCloseWaitsForTheInFlightTurnBeforeTeardown proves the barrier is a
// real wait placed ahead of the native teardown: while a turn holds the
// session's only slot the process is not closed, and close still latches its
// terminal state immediately so no other door can start work behind it.
func TestSessionCloseWaitsForTheInFlightTurnBeforeTeardown(t *testing.T) {
	transport := newFakeClaudeTransport()
	session := newCloseStateSession(t, transport)

	// A turn is in flight: it holds the session's single turn slot.
	session.turn <- struct{}{}

	closed := make(chan error, 1)

	go func() { closed <- session.Close(context.Background()) }()

	require.Eventually(t, session.isClosing, time.Second, time.Millisecond)
	require.Never(t, func() bool { return transport.CloseCalls() > 0 }, 100*time.Millisecond, 5*time.Millisecond)

	<-session.turn

	require.NoError(t, <-closed)
	require.Equal(t, 1, transport.CloseCalls())

	// The second caller observes the first result and re-runs no teardown.
	require.NoError(t, session.Close(context.Background()))
	require.Equal(t, 1, transport.CloseCalls())
}

// TestSessionCloseWaitIsBoundedOnlyByItsCaller proves the settlement latch has no
// deadline of its own: a busy session's close waits for that turn to settle and
// reports the caller's own bound rather than the spurious prompt-admission
// backpressure error a non-blocking barrier produced.
func TestSessionCloseWaitIsBoundedOnlyByItsCaller(t *testing.T) {
	transport := newFakeClaudeTransport()
	session := newCloseStateSession(t, transport)

	// The turn never finishes, so only the caller's bound can end the wait.
	session.turn <- struct{}{}

	ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
	defer cancel()

	err := session.Close(ctx)
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.ErrorContains(t, err, "await the in-flight Claude turn")

	var reqErr *acp.RequestError

	require.NotErrorAs(t, err, &reqErr, "an abandoned close wait is not prompt backpressure")
}

// TestClosedSessionRefusesEveryDoor proves close is terminal: once it has begun,
// a prompt, a relaunch, an MCP registry refresh and a reuse by load, resume or
// fork are all refused, and no replacement native process is started.
func TestClosedSessionRefusesEveryDoor(t *testing.T) {
	created := 0
	acquires := 0
	releases := 0

	agent := NewAgent(WithHome(t.TempDir()), WithRuntimeResourceHooks(RuntimeResourceHooks{
		AcquireNativeRoot: func(context.Context, RuntimeResourceKind) (func(), error) {
			acquires++

			return func() { releases++ }, nil
		},
	}))
	agent.setConnection(newRecordingAgentClient())
	agent.newClaudeClient = func(log *slog.Logger, options claude.Options) *claude.Client {
		created++

		return claude.NewClient(log, options, newFakeClaudeTransport())
	}

	start := sessionStart{Cwd: t.TempDir()}

	response, err := agent.NewSession(t.Context(), NewSessionRequest(start.Cwd))
	require.NoError(t, err)

	session := agent.sessions[response.SessionId]
	require.NotNil(t, session)
	require.NoError(t, session.Close(t.Context()))
	require.Equal(t, 1, created)

	_, err = session.Prompt(t.Context(), TextPromptRequest(session.id, "test-turn", "hello"))
	requireUnknownSession(t, err)

	requireUnknownSession(t, session.relaunchClient(t.Context(), nil, nil, session.clientOptions))

	session.mu.Lock()
	session.mcpRefreshPending = true
	session.mu.Unlock()
	requireUnknownSession(t, session.refreshMCPRegistry(t.Context()))

	require.Nil(t, agent.activeSessionForStart(response.SessionId, start))

	// No door started a replacement process, and the admission the session held
	// was returned exactly once.
	require.Equal(t, 1, created)
	require.Equal(t, acquires, releases)
}

// TestPromptRefusesACloseThatBeganDuringAdmission drives the check that lives
// in the section publishing the turn. The close begins after prompt admission
// has already passed its own check, so only the section that installs the turn
// can still refuse it.
func TestPromptRefusesACloseThatBeganDuringAdmission(t *testing.T) {
	session, _, cleanup := newPromptFlowSession(t)
	defer cleanup()

	session.turnAcquiredHook = func(int) { session.beginClose() }

	_, err := session.Prompt(t.Context(), TextPromptRequest(session.id, "test-turn", "hello"))
	requireUnknownSession(t, err)

	session.mu.Lock()
	require.Nil(t, session.cancel)
	session.mu.Unlock()
}

// TestRelaunchThatLosesToCloseLeavesNoNativeProcess proves the relaunch check
// inside the client-swap section both refuses and cleans up: the replacement
// that was already started is torn down and its admission returned.
func TestRelaunchThatLosesToCloseLeavesNoNativeProcess(t *testing.T) {
	acquires := 0
	releases := 0
	replacement := newFakeClaudeTransport()

	agent := NewAgent(WithHome(t.TempDir()), WithRuntimeResourceHooks(RuntimeResourceHooks{
		AcquireNativeRoot: func(context.Context, RuntimeResourceKind) (func(), error) {
			acquires++

			return func() { releases++ }, nil
		},
	}))
	agent.setConnection(newRecordingAgentClient())
	agent.newClaudeClient = func(log *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(log, options, newFakeClaudeTransport())
	}

	response, err := agent.NewSession(t.Context(), NewSessionRequest(t.TempDir()))
	require.NoError(t, err)

	session := agent.sessions[response.SessionId]
	require.NotNil(t, session)
	require.NoError(t, session.client.Close())

	// The close begins while the relaunch is in flight: the replacement exists
	// and has been started, but has not been published to the session yet.
	agent.newClaudeClient = func(log *slog.Logger, options claude.Options) *claude.Client {
		session.beginClose()

		return claude.NewClient(log, options, replacement)
	}

	requireUnknownSession(t, session.ensureClientAlive(t.Context()))
	require.Equal(t, 1, replacement.CloseCalls())
	require.Equal(t, acquires, releases)

	session.mu.Lock()
	require.Nil(t, session.nativeRootRelease)
	session.mu.Unlock()
}

// TestCloseNeverEvictsAReplacementUnderTheSameID proves removal is
// pointer-conditional. A same-id install lands while the close is tearing the
// old process down; the closer must leave the live session in the map, and the
// active-session gauge is decremented in the same branch as the map delete, so
// leaving the map alone leaves the gauge alone too.
func TestCloseNeverEvictsAReplacementUnderTheSameID(t *testing.T) {
	agent, conn, _ := newFakeLifecycleAgent(t, nil)
	agent.setConnection(conn)

	response, err := agent.NewSession(t.Context(), NewSessionRequest(t.TempDir()))
	require.NoError(t, err)

	stale := agent.sessions[response.SessionId]
	require.NotNil(t, stale)

	replacement := &agentSession{agent: agent, id: response.SessionId}

	hooked := &closeHookTransport{Transport: newFakeClaudeTransport()}
	hooked.onClose = func() {
		agent.mu.Lock()
		agent.sessions[response.SessionId] = replacement
		agent.mu.Unlock()
	}

	stale.mu.Lock()
	stale.client = claude.NewClient(nil, claude.Options{}, hooked)
	stale.mu.Unlock()
	require.NoError(t, stale.client.Start(t.Context()))

	_, err = agent.CloseSession(t.Context(), acp.CloseSessionRequest{SessionId: response.SessionId})
	require.NoError(t, err)

	agent.mu.Lock()
	require.Same(t, replacement, agent.sessions[response.SessionId])
	agent.mu.Unlock()

	// Dropping the live instance removes it exactly once; repeating the drop is
	// a no-op rather than a second removal.
	agent.dropSession(t.Context(), response.SessionId, replacement)
	agent.dropSession(t.Context(), response.SessionId, replacement)

	agent.mu.Lock()
	_, present := agent.sessions[response.SessionId]
	agent.mu.Unlock()
	require.False(t, present)
}

// TestDeleteSessionRemovesOnlyItsOwnInstance holds session/delete to the same
// pointer-conditional rule as session/close.
func TestDeleteSessionRemovesOnlyItsOwnInstance(t *testing.T) {
	agent, conn, _ := newFakeLifecycleAgent(t, nil)
	agent.setConnection(conn)

	response, err := agent.NewSession(t.Context(), NewSessionRequest(t.TempDir()))
	require.NoError(t, err)

	stale := agent.sessions[response.SessionId]
	require.NotNil(t, stale)

	replacement := &agentSession{agent: agent, id: response.SessionId}

	hooked := &closeHookTransport{Transport: newFakeClaudeTransport()}
	hooked.onClose = func() {
		agent.mu.Lock()
		agent.sessions[response.SessionId] = replacement
		agent.mu.Unlock()
	}

	stale.mu.Lock()
	stale.client = claude.NewClient(nil, claude.Options{}, hooked)
	stale.mu.Unlock()
	require.NoError(t, stale.client.Start(t.Context()))

	_, err = agent.UnstableDeleteSession(t.Context(), acp.UnstableDeleteSessionRequest{SessionId: response.SessionId})
	require.NoError(t, err)

	agent.mu.Lock()
	require.Same(t, replacement, agent.sessions[response.SessionId])
	agent.mu.Unlock()
}
