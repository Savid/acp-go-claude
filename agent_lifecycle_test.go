package claudeacp

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/stretchr/testify/require"
)

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
	promptResp, err := agent.Prompt(ctx, acp.PromptRequest{
		SessionId: newResp.SessionId,
		MessageId: &messageID,
		Prompt:    []acp.ContentBlock{acp.TextBlock("hello")},
		Meta:      map[string]any{"trace": "prompt"},
	})
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
	_, err = agent.Prompt(ctx, TextPromptRequest(newResp.SessionId, "after close"))
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
