package claudeacp

import (
	"context"
	"errors"
	"log/slog"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/stretchr/testify/require"
)

func TestSessionConfigStateHelpers(t *testing.T) {
	t.Parallel()

	session := &agentSession{
		mode:                  modeDefault,
		model:                 "sonnet",
		availableModels:       []claude.AvailableModelInfo{{Value: "sonnet", SupportedEffortLevels: []string{effortLow, effortHigh}}},
		outputStyle:           "default",
		availableOutputStyles: []string{"default"},
		effort:                effortLow,
		fastMode:              true,
		fastModeKnown:         true,
	}

	session.setMode(modePlan)
	mode, model, available := session.modeInfo()
	require.Equal(t, modePlan, mode)
	require.Equal(t, "sonnet", model)
	available[0].Value = "changed"
	require.Equal(t, "sonnet", session.availableModels[0].Value)

	session.setOutputStyle("concise")
	session.setEffort(effortHigh)
	mode, model, available, style, styles, effort, fast, known := session.configInfo()
	require.Equal(t, modePlan, mode)
	require.Equal(t, "sonnet", model)
	require.Equal(t, "concise", style)
	require.Equal(t, []string{"default"}, styles)
	require.Equal(t, effortHigh, effort)
	require.True(t, fast)
	require.True(t, known)
	available[0].Value = "changed"
	styles[0] = "changed"
	require.Equal(t, "sonnet", session.availableModels[0].Value)
	require.Equal(t, "default", session.availableOutputStyles[0])

	require.Len(t, sessionConfigOptions(session), 4)
	require.Len(t, sessionUnstableConfigOptions(session), 4)

	agent := NewAgent()
	// The two ways to miss the value-id variant fault different members: a
	// request that decoded to nothing carried no value, while a boolean payload
	// chose the wrong discriminator.
	_, err := agent.SetSessionConfigOption(t.Context(), acp.SetSessionConfigOptionRequest{})
	requireExactUnsupportedField(t, err, jsonFieldValue)
	_, err = agent.SetSessionConfigOption(t.Context(), acp.SetSessionConfigOptionRequest{
		Boolean: &acp.SetSessionConfigOptionBoolean{SessionId: "missing", ConfigId: configModel, Value: true},
	})
	requireExactUnsupportedField(t, err, jsonFieldType)
	_, err = agent.SetSessionConfigOption(t.Context(), SetConfigOptionRequest("missing", configModel, "sonnet"))
	require.Error(t, err)
}

func TestSetSessionConfigValueBranches(t *testing.T) {
	ctx := context.Background()
	sessionID := acp.SessionId("session-1")

	t.Run("backpressure", func(t *testing.T) {
		agent, session, _, _, cleanup := newStartedConfigTestSession(t, sessionID)
		defer cleanup()
		session.turn = make(chan struct{}, 1)
		session.turn <- struct{}{}

		_, err := agent.SetSessionConfigOption(ctx, SetModelRequest(sessionID, "sonnet"))
		require.Error(t, err)
	})

	t.Run("unavailable mode", func(t *testing.T) {
		agent, _, _, _, cleanup := newStartedConfigTestSession(t, sessionID)
		defer cleanup()

		_, err := agent.SetSessionConfigOption(ctx, SetConfigOptionRequest(sessionID, configMode, acp.SessionConfigValueId(modeAuto)))
		requireExactUnsupportedField(t, err, jsonFieldValue)
	})

	for _, tc := range []struct {
		name    string
		request acp.SetSessionConfigOptionRequest
	}{
		{name: "model set error", request: SetModelRequest(sessionID, "opus")},
		{name: "mode set error", request: SetConfigOptionRequest(sessionID, configMode, acp.SessionConfigValueId(modePlan))},
		{name: "output style error", request: SetConfigOptionRequest(sessionID, configOutputStyle, acp.SessionConfigValueId("concise"))},
		{name: "effort error", request: SetConfigOptionRequest(sessionID, configEffort, acp.SessionConfigValueId(effortHigh))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			agent, _, transport, _, cleanup := newStartedConfigTestSession(t, sessionID)
			defer cleanup()
			transport.sendErr = errors.New("send failed")

			_, err := agent.SetSessionConfigOption(ctx, tc.request)
			require.ErrorContains(t, err, "send failed")
		})
	}

	t.Run("emit update error", func(t *testing.T) {
		agent, _, _, conn, cleanup := newStartedConfigTestSession(t, sessionID)
		defer cleanup()
		conn.sessionUpdateErr = errors.New("update failed")

		_, err := agent.SetSessionConfigOption(ctx, SetConfigOptionRequest(sessionID, configOutputStyle, acp.SessionConfigValueId("concise")))
		require.ErrorContains(t, err, "update failed")
	})

	t.Run("clear effort", func(t *testing.T) {
		_, session, _, _, cleanup := newStartedConfigTestSession(t, sessionID)
		defer cleanup()

		require.NoError(t, session.applyEffort(ctx, ""))
	})
}

func newStartedConfigTestSession(
	t *testing.T,
	sessionID acp.SessionId,
) (*Agent, *agentSession, *fakeClaudeTransport, *recordingAgentClient, func()) {
	t.Helper()

	transport := newFakeClaudeTransport()
	client := claude.NewClient(slog.Default(), claude.Options{}, transport)
	require.NoError(t, client.Start(context.Background()))

	agent := NewAgent()
	conn := newRecordingAgentClient()
	agent.setConnection(conn)

	session := &agentSession{
		agent:                 agent,
		id:                    sessionID,
		cwd:                   t.TempDir(),
		model:                 "sonnet",
		availableModels:       []claude.AvailableModelInfo{{Value: "sonnet", SupportedEffortLevels: []string{effortLow, effortHigh}}, {Value: "opus", SupportedEffortLevels: []string{effortHigh}}},
		outputStyle:           "default",
		availableOutputStyles: []string{"default", "concise"},
		mode:                  modeDefault,
		effort:                effortLow,
		client:                client,
		turn:                  make(chan struct{}, sessionTurnCapacity),
	}
	agent.sessions[sessionID] = session

	return agent, session, transport, conn, func() { require.NoError(t, client.Close()) }
}
