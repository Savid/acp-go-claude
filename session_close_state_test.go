package claudeacp

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/stretchr/testify/require"
)

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
		agent:         NewAgent(),
		id:            "session-1",
		client:        client,
		turn:          make(chan struct{}, sessionTurnCapacity),
		closeTurnWait: defaultSessionCloseTurnWait,
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

// TestSessionCloseBoundReportsTheWaitRatherThanBackpressure proves a busy
// session's close is no longer answered with the spurious prompt-admission
// backpressure error that the old non-blocking barrier produced.
func TestSessionCloseBoundReportsTheWaitRatherThanBackpressure(t *testing.T) {
	transport := newFakeClaudeTransport()
	session := newCloseStateSession(t, transport)
	session.closeTurnWait = time.Millisecond

	// The turn never finishes, so only the bound can end the wait.
	session.turn <- struct{}{}

	err := session.Close(context.Background())
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.ErrorContains(t, err, "await the in-flight Claude turn")

	var reqErr *acp.RequestError

	require.NotErrorAs(t, err, &reqErr, "a bounded close wait is not prompt backpressure")
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
