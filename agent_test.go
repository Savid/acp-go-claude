package claudeacp

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/stretchr/testify/require"
)

func TestNewAgentDefaultClientAndCloseBranches(t *testing.T) {
	t.Parallel()

	agent := NewAgent()
	client := agent.newClaudeClient(nil, claude.Options{})
	require.NotNil(t, client)

	transport := newFakeClaudeTransport()
	sessionClient := claude.NewClient(nil, claude.Options{}, transport)
	require.NoError(t, sessionClient.Start(context.Background()))
	closeErr := errors.New("close failed")
	transport.closeErr = closeErr

	session := &agentSession{
		agent:  agent,
		id:     "session-1",
		client: sessionClient,
		turn:   make(chan struct{}, 1),
	}
	agent.sessions[session.id] = session
	agent.permissionCache[session.id] = map[string]string{"Read": "allow"}
	agent.deleted[session.id] = struct{}{}
	agent.setConnection(newRecordingAgentClient())

	err := agent.Close()
	require.ErrorIs(t, err, closeErr)
	require.Empty(t, agent.sessions)
	require.Empty(t, agent.permissionCache)
	require.Empty(t, agent.deleted)
	require.True(t, agent.closed)
}

func TestServeDoneAndCloseErrorBranches(t *testing.T) {
	require.NoError(t, Serve(context.Background(), bytes.NewBuffer(nil), io.Discard))

	previous := newServeAgent
	t.Cleanup(func() { newServeAgent = previous })

	agent := NewAgent()
	transport := newFakeClaudeTransport()
	sessionClient := claude.NewClient(nil, claude.Options{}, transport)
	require.NoError(t, sessionClient.Start(context.Background()))
	transport.closeErr = errors.Join(errors.New("close failed"), claude.ErrProcessContainmentIncomplete)
	agent.sessions["session-1"] = &agentSession{
		agent:  agent,
		id:     acp.SessionId("session-1"),
		client: sessionClient,
		turn:   make(chan struct{}, 1),
	}
	newServeAgent = func(...Option) *Agent { return agent }

	// EOF input resolves conn.Done first; the deferred agent close must preserve
	// the stronger process-tree proof failure.
	err := Serve(context.Background(), bytes.NewBuffer(nil), io.Discard)
	require.ErrorIs(t, err, ErrProcessContainmentIncomplete)
	require.ErrorIs(t, ErrProcessContainmentIncomplete, claude.ErrProcessContainmentIncomplete)

	// A context that is already cancelled fails the pre-select guard before an
	// agent is even constructed.
	preCancelled, cancelEarly := context.WithCancel(context.Background())
	cancelEarly()
	require.ErrorIs(t, Serve(preCancelled, &blockingReader{}, io.Discard), context.Canceled)

	// A context cancelled while Serve is blocked resolves through the select,
	// but the deferred Close error takes precedence over that loop result.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	err = Serve(ctx, &blockingReader{}, io.Discard)
	require.ErrorIs(t, err, ErrProcessContainmentIncomplete)
	require.NotErrorIs(t, err, context.DeadlineExceeded)
}

type blockingCloseTransport struct {
	claude.Transport
	entered chan struct{}
	release chan struct{}
	err     error
	calls   atomic.Int32
	once    sync.Once
}

type blockingStartTransport struct {
	claude.Transport
	entered chan struct{}
	release chan struct{}
	err     error
	once    sync.Once
}

func (t *blockingStartTransport) Start(context.Context) error {
	t.once.Do(func() { close(t.entered) })
	<-t.release

	return t.err
}

func (t *blockingCloseTransport) Close() error {
	t.calls.Add(1)
	t.once.Do(func() { close(t.entered) })
	<-t.release

	return t.err
}

func TestAgentCloseSingleflightAndStickyContainment(t *testing.T) {
	blocked := &blockingCloseTransport{
		Transport: newFakeClaudeTransport(),
		entered:   make(chan struct{}),
		release:   make(chan struct{}),
		err:       errors.Join(errors.New("cleanup failed"), claude.ErrProcessContainmentIncomplete),
	}
	client := claude.NewClient(nil, claude.Options{}, blocked)
	require.NoError(t, client.Start(t.Context()))

	agent := NewAgent()
	agent.sessions["session-1"] = &agentSession{
		agent: agent, id: "session-1", client: client, turn: make(chan struct{}, 1),
	}

	results := make(chan error, 2)
	go func() { results <- agent.Close() }()
	<-blocked.entered
	go func() { results <- agent.Close() }()
	close(blocked.release)

	for range 2 {
		require.ErrorIs(t, <-results, claude.ErrProcessContainmentIncomplete)
	}
	require.Equal(t, int32(1), blocked.calls.Load())
	require.ErrorIs(t, agent.Close(), claude.ErrProcessContainmentIncomplete)
}

func TestCloseAndServeJoinAdmittedIncompleteSessionConstruction(t *testing.T) {
	previous := newServeAgent
	t.Cleanup(func() { newServeAgent = previous })

	transport := &blockingStartTransport{
		Transport: newFakeClaudeTransport(),
		entered:   make(chan struct{}),
		release:   make(chan struct{}),
		err:       claude.ErrProcessContainmentIncomplete,
	}
	agent := NewAgent(
		WithHome(t.TempDir()),
		WithScratchDir(t.TempDir()),
		WithLogger(slog.New(slog.DiscardHandler)),
	)
	agent.newClaudeClient = func(log *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(log, options, transport)
	}

	newSessionErr := make(chan error, 1)
	go func() {
		_, err := agent.NewSession(context.Background(), NewSessionRequest(t.TempDir()))
		newSessionErr <- err
	}()
	<-transport.entered

	serveCreated := make(chan struct{})
	newServeAgent = func(...Option) *Agent {
		close(serveCreated)

		return agent
	}
	serveCtx, cancelServe := context.WithCancel(context.Background())
	input, inputWriter := io.Pipe()
	t.Cleanup(func() {
		_ = input.Close()
		_ = inputWriter.Close()
	})
	serveErr := make(chan error, 1)
	go func() { serveErr <- Serve(serveCtx, input, io.Discard) }()
	<-serveCreated
	cancelServe()

	closeErr := make(chan error, 1)
	go func() { closeErr <- agent.Close() }()
	require.Eventually(t, func() bool {
		agent.mu.Lock()
		defer agent.mu.Unlock()

		return agent.closed
	}, time.Second, time.Millisecond)

	select {
	case err := <-closeErr:
		t.Fatalf("Close returned before admitted construction published containment: %v", err)
	default:
	}
	select {
	case err := <-serveErr:
		t.Fatalf("Serve returned before admitted construction published containment: %v", err)
	default:
	}

	close(transport.release)
	require.ErrorIs(t, <-newSessionErr, claude.ErrProcessContainmentIncomplete)
	require.ErrorIs(t, <-closeErr, claude.ErrProcessContainmentIncomplete)
	gotServeErr := <-serveErr
	require.ErrorIs(t, gotServeErr, claude.ErrProcessContainmentIncomplete)
	require.NotErrorIs(t, gotServeErr, context.Canceled)
	require.ErrorIs(t, agent.Close(), claude.ErrProcessContainmentIncomplete)
}

func TestCloseAndServeJoinAdmittedIncompletePromptRelaunch(t *testing.T) {
	previous := newServeAgent
	t.Cleanup(func() { newServeAgent = previous })

	transport := &blockingStartTransport{
		Transport: newFakeClaudeTransport(),
		entered:   make(chan struct{}),
		release:   make(chan struct{}),
		err:       claude.ErrProcessContainmentIncomplete,
	}
	agent := NewAgent(WithLogger(slog.New(slog.DiscardHandler)))
	agent.newClaudeClient = func(log *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(log, options, transport)
	}
	session := &agentSession{
		agent:       agent,
		id:          "session-1",
		client:      deadClaudeClient(t, nil),
		turn:        make(chan struct{}, sessionTurnCapacity),
		canRelaunch: true,
	}
	agent.sessions[session.id] = session

	relaunchErr := make(chan error, 1)
	go func() { relaunchErr <- session.ensureClientAlive(context.Background()) }()
	<-transport.entered

	serveCreated := make(chan struct{})
	newServeAgent = func(...Option) *Agent {
		close(serveCreated)

		return agent
	}
	serveCtx, cancelServe := context.WithCancel(context.Background())
	input, inputWriter := io.Pipe()
	t.Cleanup(func() {
		_ = input.Close()
		_ = inputWriter.Close()
	})
	serveErr := make(chan error, 1)
	go func() { serveErr <- Serve(serveCtx, input, io.Discard) }()
	<-serveCreated
	cancelServe()

	closeErr := make(chan error, 1)
	go func() { closeErr <- agent.Close() }()
	require.Eventually(t, func() bool {
		agent.mu.Lock()
		defer agent.mu.Unlock()

		return agent.closed
	}, time.Second, time.Millisecond)

	select {
	case err := <-closeErr:
		t.Fatalf("Close returned before prompt relaunch unwound: %v", err)
	default:
	}
	select {
	case err := <-serveErr:
		t.Fatalf("Serve returned before prompt relaunch unwound: %v", err)
	default:
	}

	close(transport.release)
	require.ErrorIs(t, <-relaunchErr, ErrProcessContainmentIncomplete)
	require.ErrorIs(t, <-closeErr, ErrProcessContainmentIncomplete)
	require.ErrorIs(t, <-serveErr, ErrProcessContainmentIncomplete)
	require.ErrorIs(t, agent.Close(), ErrProcessContainmentIncomplete)
}

// TestFailedCloseRetainsItsSessionAndItsContainmentSurvivesTerminalServe pins
// both halves of a close that could not contain its tree. The id keeps naming
// the session, because what is still running behind it is exactly what this
// close failed to finish, and the containment failure the agent recorded is
// terminal for the agent: Serve and every later Close report it too.
func TestFailedCloseRetainsItsSessionAndItsContainmentSurvivesTerminalServe(t *testing.T) {
	previous := newServeAgent
	t.Cleanup(func() { newServeAgent = previous })

	agent := NewAgent()
	session := closeErrorAgentSession(t, claude.ErrProcessContainmentIncomplete, nil)
	session.agent = agent
	agent.sessions["session-1"] = session

	_, err := agent.CloseSession(t.Context(), acp.CloseSessionRequest{SessionId: "session-1"})
	require.ErrorIs(t, err, claude.ErrProcessContainmentIncomplete)
	require.Contains(t, agent.sessions, acp.SessionId("session-1"),
		"a close that contained nothing keeps the id its work is still addressable through")

	newServeAgent = func(...Option) *Agent { return agent }
	err = Serve(t.Context(), bytes.NewBuffer(nil), io.Discard)
	require.ErrorIs(t, err, claude.ErrProcessContainmentIncomplete)
	require.ErrorIs(t, agent.Close(), claude.ErrProcessContainmentIncomplete)
}

func TestAgentLifecycleWithFakeClaude(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	agent := NewAgent(WithHome(t.TempDir()), WithDefaultModel("sonnet"))
	conn := newRecordingAgentClient()
	agent.setConnection(conn)
	transport := newFakeClaudeTransport()
	installFakeClaudeClient(agent, transport)

	newResp, err := agent.NewSession(ctx, NewSessionRequest(t.TempDir(), WithSessionRawEvents(true)))
	require.NoError(t, err)
	require.NotEmpty(t, newResp.SessionId)
	require.NotEmpty(t, newResp.ConfigOptions)
	require.NotEmpty(t, newResp.Meta)

	session := agent.sessions[newResp.SessionId]
	require.Equal(t, "sonnet", session.currentModel())
	require.NotNil(t, agent.activeSessionForStart(newResp.SessionId, sessionStart{Cwd: session.cwd, RawMessages: rawMessageConfig{All: true}}))

	messageID := "message-1"
	promptRequest := TextPromptRequest(newResp.SessionId, "turn-1", "hello")
	promptRequest.MessageId = &messageID
	promptRequest.Meta["trace"] = "prompt"
	promptResp, err := agent.Prompt(ctx, promptRequest)
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, promptResp.StopReason)
	require.NotNil(t, promptResp.Usage)
	require.Equal(t, messageID, *promptResp.UserMessageId)
	require.NotEmpty(t, conn.Updates())
	require.NotEmpty(t, conn.Extensions())

	_, err = agent.SetSessionConfigOption(ctx, SetModelRequest(newResp.SessionId, "opus"))
	require.NoError(t, err)
	_, err = agent.SetSessionConfigOption(ctx, SetConfigOptionRequest(newResp.SessionId, configMode, acp.SessionConfigValueId(modeDefault)))
	require.NoError(t, err)
	_, err = agent.SetSessionConfigOption(ctx, SetConfigOptionRequest(newResp.SessionId, configOutputStyle, "concise"))
	require.NoError(t, err)
	_, err = agent.SetSessionConfigOption(ctx, SetConfigOptionRequest(newResp.SessionId, configEffort, effortHigh))
	require.NoError(t, err)
	_, err = agent.SetSessionConfigOption(ctx, SetConfigOptionRequest(newResp.SessionId, "bad", "x"))
	requireExactUnsupportedField(t, err, "configId")
	_, err = agent.SetSessionConfigOption(ctx, SetConfigOptionRequest(newResp.SessionId, configMode, "bad"))
	requireExactUnsupportedField(t, err, jsonFieldValue)

	listResp, err := agent.ListSessions(ctx, ListSessionsRequest(WithListSessionsCwd(session.cwd)))
	require.NoError(t, err)
	require.Len(t, listResp.Sessions, 1)

	resumeResp, err := agent.ResumeSession(ctx, ResumeSessionRequest(newResp.SessionId, session.cwd, WithSessionRawEvents(true)))
	require.NoError(t, err)
	require.NotEmpty(t, resumeResp.ConfigOptions)

	// The turn has already settled, so no nonce can authorize a cancel: the
	// idle session refuses it and stays open for the close that follows.
	requireExactUnsupportedField(t, agent.Cancel(ctx, acp.CancelNotification{SessionId: newResp.SessionId}), routeMetaKey)
	_, err = agent.CloseSession(ctx, acp.CloseSessionRequest{SessionId: newResp.SessionId})
	require.NoError(t, err)
	_, err = agent.Prompt(ctx, TextPromptRequest(newResp.SessionId, "test-turn", "after close"))
	require.Error(t, err)
}

func TestAgentLifecycleErrors(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	agent := NewAgent(WithHome(t.TempDir()), WithConcurrencyLimits(ConcurrencyLimits{MaxActiveSessions: 1}))
	agent.setConnection(newRecordingAgentClient())
	var transports []*fakeClaudeTransport
	agent.newClaudeClient = func(log *slog.Logger, options claude.Options) *claude.Client {
		transport := newFakeClaudeTransport()
		transports = append(transports, transport)

		return claude.NewClient(log, options, transport)
	}

	first, err := agent.NewSession(ctx, NewSessionRequest(t.TempDir()))
	require.NoError(t, err)
	_, err = agent.NewSession(ctx, NewSessionRequest(t.TempDir()))
	require.ErrorContains(t, err, "active_sessions")

	transports[0].closeErr = errors.New("close failed")
	_, err = agent.CloseSession(ctx, acp.CloseSessionRequest{SessionId: first.SessionId})
	require.ErrorContains(t, err, "claude transport failed")
	require.NotContains(t, err.Error(), "close failed")

	closed := NewAgent(WithHome(t.TempDir()))
	require.NoError(t, closed.Close())
	_, err = closed.NewSession(ctx, NewSessionRequest(t.TempDir()))
	requireAgentClosedRefusal(t, err)

	startFail := NewAgent(WithHome(t.TempDir()))
	startFail.setConnection(newRecordingAgentClient())
	failTransport := newFakeClaudeTransport()
	failTransport.startErr = errors.New("start failed")
	installFakeClaudeClient(startFail, failTransport)
	_, err = startFail.NewSession(ctx, NewSessionRequest(t.TempDir()))
	require.ErrorContains(t, err, "start failed")

	require.True(t, missingClaudeSessionError(claude.ErrSessionNotFound))
	require.True(t, missingClaudeSessionError(claude.ErrQueryClosed))
	require.False(t, missingClaudeSessionError(errors.New("other")))
}

func TestServeAndExtensionMethods(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	err := Serve(ctx, &blockingReader{}, io.Discard)
	require.ErrorIs(t, err, context.Canceled)

	agent := NewAgent()
	_, err = agent.Authenticate(context.Background(), acp.AuthenticateRequest{MethodId: "claude"})
	require.Error(t, err)
	_, err = agent.Logout(context.Background(), acp.LogoutRequest{})
	require.NoError(t, err)
	_, err = agent.HandleExtensionMethod(context.Background(), "unknown", nil)
	require.Error(t, err)
}

type blockingReader struct{}

func (*blockingReader) Read([]byte) (int, error) {
	select {}
}
