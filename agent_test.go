package claudeacp

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
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
	transport.closeErr = errors.New("close failed")

	session := &agentSession{
		agent:         agent,
		id:            "session-1",
		client:        sessionClient,
		turn:          make(chan struct{}, 1),
		closeTurnWait: defaultSessionCloseTurnWait,
	}
	agent.sessions[session.id] = session
	agent.permissionCache[session.id] = map[string]string{"Read": "allow"}
	agent.deleted[session.id] = struct{}{}
	agent.setConnection(newRecordingAgentClient())

	err := agent.Close()
	require.ErrorContains(t, err, "close failed")
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
	transport.closeErr = errors.Join(errors.New("close failed"), claude.ErrProcessTreeUnproven)
	agent.sessions["session-1"] = &agentSession{
		agent:         agent,
		id:            acp.SessionId("session-1"),
		client:        sessionClient,
		turn:          make(chan struct{}, 1),
		closeTurnWait: defaultSessionCloseTurnWait,
	}
	newServeAgent = func(...Option) *Agent { return agent }

	// EOF input resolves conn.Done first; the deferred agent close must preserve
	// the stronger process-tree proof failure.
	err := Serve(context.Background(), bytes.NewBuffer(nil), io.Discard)
	require.ErrorIs(t, err, ErrProcessTreeUnproven)
	require.ErrorIs(t, ErrProcessTreeUnproven, claude.ErrProcessTreeUnproven)

	// A context that is already cancelled fails the pre-select guard before an
	// agent is even constructed.
	preCancelled, cancelEarly := context.WithCancel(context.Background())
	cancelEarly()
	require.ErrorIs(t, Serve(preCancelled, &blockingReader{}, io.Discard), context.Canceled)

	// A context cancelled while Serve is blocked resolves through the select.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	require.ErrorIs(t, Serve(ctx, &blockingReader{}, io.Discard), context.DeadlineExceeded)
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
	require.ErrorContains(t, err, "unsupported option")
	_, err = agent.SetSessionConfigOption(ctx, SetConfigOptionRequest(newResp.SessionId, configMode, "bad"))
	require.ErrorContains(t, err, "unsupported mode")

	listResp, err := agent.ListSessions(ctx, ListSessionsRequest(WithListSessionsCwd(session.cwd)))
	require.NoError(t, err)
	require.Len(t, listResp.Sessions, 1)

	resumeResp, err := agent.ResumeSession(ctx, ResumeSessionRequest(newResp.SessionId, session.cwd, WithSessionRawEvents(true)))
	require.NoError(t, err)
	require.NotEmpty(t, resumeResp.ConfigOptions)

	require.NoError(t, agent.Cancel(ctx, acp.CancelNotification{SessionId: newResp.SessionId}))
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
	require.ErrorContains(t, err, "close failed")

	closed := NewAgent(WithHome(t.TempDir()))
	require.NoError(t, closed.Close())
	_, err = closed.NewSession(ctx, NewSessionRequest(t.TempDir()))
	require.ErrorIs(t, err, errAgentClosed)

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
