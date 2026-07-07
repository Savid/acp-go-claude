package claudeacp

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/stretchr/testify/require"
)

func TestSessionPromptDenyInvokeCommands(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		prompt      string
		wantDenied  bool
		wantAlt     string
		wantMessage string
	}{
		{name: "clear", prompt: "/clear now", wantDenied: true, wantAlt: "session/new"},
		{name: "config", prompt: "/config model=sonnet", wantDenied: true, wantAlt: "session/set_config_option"},
		{name: "settings alias", prompt: "/settings model=sonnet", wantDenied: true, wantAlt: "session/set_config_option"},
		{name: "reset alias", prompt: "/reset now", wantDenied: true, wantAlt: "session/new"},
		{name: "new alias", prompt: "/new", wantDenied: true, wantAlt: "session/new"},
		{name: "leading whitespace bypass", prompt: " /clear now", wantMessage: " /clear now"},
		{name: "leading whitespace alias bypass", prompt: " /reset", wantMessage: " /reset"},
		{name: "exact name only", prompt: "/clearly now", wantMessage: "/clearly now"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()
			session, transport, cleanup := newPromptFlowSession(t)
			defer cleanup()
			session.availableCommands = []claude.SlashCommand{
				{Name: "clear", Aliases: []string{"reset", "new"}},
				{Name: "config", Aliases: []string{"settings"}},
			}

			before := len(transport.Sent())
			resp, err := session.Prompt(ctx, TextPromptRequest(session.id, tc.prompt))
			if tc.wantDenied {
				require.Empty(t, resp)
				requireInvalidCommandAlternative(t, err, tc.wantAlt)
				require.Len(t, transport.Sent(), before)

				return
			}

			require.NoError(t, err)
			require.Equal(t, acp.StopReasonEndTurn, resp.StopReason)
			require.Equal(t, tc.wantMessage, lastSentUserText(t, transport))
		})
	}
}

func TestSuppressedCommandsPassThroughAsText(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	session, transport, cleanup := newPromptFlowSession(t)
	defer cleanup()
	session.availableCommands = []claude.SlashCommand{
		{Name: "cost", Description: "suppressed status"},
		{Name: "heapdump", Description: "suppressed diagnostic"},
	}

	for _, prompt := range []string{"/cost", "/heapdump"} {
		resp, err := session.Prompt(ctx, TextPromptRequest(session.id, prompt))
		require.NoError(t, err)
		require.Equal(t, acp.StopReasonEndTurn, resp.StopReason)
		require.Equal(t, prompt, lastSentUserText(t, transport))
	}
}

func TestMCPPromptRewriteUsesAdvertisedCommandsSnapshot(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	session, transport, cleanup := newPromptFlowSession(t)
	defer cleanup()
	session.availableCommands = []claude.SlashCommand{{Name: "server:name (MCP)", Description: "Run MCP command"}}

	resp, err := session.Prompt(ctx, TextPromptRequest(session.id, "/mcp:server:name args"))
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, resp.StopReason)
	require.Equal(t, "/mcp:server:name args", lastSentUserText(t, transport))

	session.advertisedCommands = []acp.AvailableCommand{{Name: "mcp:server:name"}}
	resp, err = session.Prompt(ctx, TextPromptRequest(session.id, "/mcp:server:name args"))
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, resp.StopReason)
	require.Equal(t, "/server:name (MCP) args", lastSentUserText(t, transport))
}

func TestCommandTurnExclusivity(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	session, _, cleanup := newPromptFlowSession(t)
	defer cleanup()
	// A command acquires the whole turn exclusively, so with a spare slot free
	// it is still refused while another prompt holds a slot.
	session.turn = make(chan struct{}, 2)
	session.availableCommands = []claude.SlashCommand{{Name: "compact"}}
	session.turn <- struct{}{}

	_, err := session.Prompt(ctx, TextPromptRequest(session.id, "/compact now"))
	requireBackpressureLimit(t, err, "session_prompt")

	_, err = session.Prompt(ctx, TextPromptRequest(session.id, "/clear now"))
	requireBackpressureLimit(t, err, "session_prompt")
}

func TestPromptPoisonAfterAcquireTurn(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	session, _, cleanup := newPromptFlowSession(t)
	defer cleanup()
	session.turnAcquiredHook = func(int) {
		session.mu.Lock()
		session.poisonCause = "poisoned after prompt acquire"
		session.mu.Unlock()
	}

	_, err := session.Prompt(ctx, TextPromptRequest(session.id, "hello"))
	require.ErrorContains(t, err, "poisoned after prompt acquire")
}

func TestAvailableCommandsEmptyClear(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	session, _, cleanup := newPromptFlowSession(t)
	defer cleanup()
	conn, ok := session.agent.connection().(*recordingAgentClient)
	require.True(t, ok)

	session.availableCommands = []claude.SlashCommand{{Name: "help", Description: "Help"}}
	require.NoError(t, session.emitAvailableCommandsUpdate(ctx, true))

	session.availableCommands = nil
	require.NoError(t, session.emitAvailableCommandsUpdate(ctx, false))
	require.NoError(t, session.emitAvailableCommandsUpdate(ctx, false))

	updates := availableCommandUpdates(conn.Updates())
	require.Len(t, updates, 2)
	require.Len(t, updates[0].AvailableCommands, 1)
	require.Empty(t, updates[1].AvailableCommands)
}

func TestNativeAliasesAreNotAdvertised(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	session, _, cleanup := newPromptFlowSession(t)
	defer cleanup()
	session.availableCommands = []claude.SlashCommand{
		{Name: "help", Description: "Help", Aliases: []string{"h"}},
		{Name: "clear", Description: "Clear", Aliases: []string{"reset", "new"}},
		{Name: "config", Description: "Config", Aliases: []string{"settings"}},
	}

	require.NoError(t, session.emitAvailableCommandsUpdate(ctx, true))

	conn, ok := session.agent.connection().(*recordingAgentClient)
	require.True(t, ok)
	updates := availableCommandUpdates(conn.Updates())
	require.Len(t, updates, 1)
	require.Equal(t, []string{"help"}, availableCommandNames(updates[0].AvailableCommands))
}

func TestReloadSkillsRediscoveryEmitsDynamicCommandUpdate(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	session, transport, cleanup := newPromptFlowSession(t)
	defer cleanup()
	require.False(t, session.commandAdvertised(""))
	session.availableCommands = []claude.SlashCommand{{Name: commandReloadSkills, Description: "Reload skills"}}
	transport.initialize = map[string]any{
		"commands": []any{
			map[string]any{"name": commandReloadSkills, "description": "Reload skills"},
			map[string]any{"name": "deploy", "description": "Deploy", "argumentHint": "[env]"},
		},
	}

	resp, err := session.Prompt(ctx, TextPromptRequest(session.id, "/"+commandReloadSkills))
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, resp.StopReason)

	conn, ok := session.agent.connection().(*recordingAgentClient)
	require.True(t, ok)
	updates := availableCommandUpdates(conn.Updates())
	require.Len(t, updates, 1)
	require.Equal(t, []string{commandReloadSkills, "deploy"}, availableCommandNames(updates[0].AvailableCommands))
	require.Equal(t, "[env]", updates[0].AvailableCommands[1].Input.Unstructured.Hint)
}

func TestReloadPluginsRediscoveryEmitsDynamicCommandUpdate(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	session, transport, cleanup := newPromptFlowSession(t)
	defer cleanup()
	session.availableCommands = []claude.SlashCommand{
		{Name: commandReloadPlugins, Description: "Reload plugins"},
		{Name: "old", Description: "Old command"},
	}
	require.NoError(t, session.emitAvailableCommandsUpdate(ctx, true))

	transport.initialize = map[string]any{
		"commands": []any{
			map[string]any{"name": commandReloadPlugins, "description": "Reload plugins"},
			map[string]any{"name": "deploy", "description": "Deploy", "argumentHint": "[env]"},
		},
	}

	resp, err := session.Prompt(ctx, TextPromptRequest(session.id, "/"+commandReloadPlugins))
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, resp.StopReason)

	conn, ok := session.agent.connection().(*recordingAgentClient)
	require.True(t, ok)
	updates := availableCommandUpdates(conn.Updates())
	require.Len(t, updates, 2)
	require.Equal(t, []string{commandReloadPlugins, "old"}, availableCommandNames(updates[0].AvailableCommands))
	require.Equal(t, []string{commandReloadPlugins, "deploy"}, availableCommandNames(updates[1].AvailableCommands))
	require.Equal(t, "[env]", updates[1].AvailableCommands[1].Input.Unstructured.Hint)
}

func TestReloadSkillsRefreshErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		queryMsgs []map[string]any
	}{
		{
			name: "result",
			queryMsgs: []map[string]any{{
				"type":        "result",
				"subtype":     "success",
				"is_error":    false,
				"stop_reason": "end_turn",
			}},
		},
		{
			name: "system idle",
			queryMsgs: []map[string]any{{
				"type":      "system",
				"subtype":   systemSubtypeSessionStateChanged,
				systemState: systemStateIdle,
			}},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()
			session, transport, cleanup := newPromptFlowSession(t)
			defer cleanup()
			session.availableCommands = []claude.SlashCommand{{Name: commandReloadSkills, Description: "Reload skills"}}
			transport.queryMsgs = tc.queryMsgs
			transport.controlErr = map[string]error{"initialize": errors.New("refresh failed")}

			_, err := session.Prompt(ctx, TextPromptRequest(session.id, "/"+commandReloadSkills))
			require.ErrorContains(t, err, "refresh claude control initialize")
		})
	}
}

func TestInvariantGuardPoisonsSession(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		queryMsgs []map[string]any
		wantCause string
	}{
		{
			name: "conversation reset frame",
			queryMsgs: []map[string]any{
				{"type": "conversation_reset"},
				{"type": "transcript_mirror", "filePath": "/tmp/ignored.jsonl", "entries": []any{map[string]any{"type": "user"}}},
			},
			wantCause: "conversation_reset",
		},
		{
			name: "native session id drift",
			queryMsgs: []map[string]any{
				{
					"type":       "assistant",
					"session_id": "native-other",
					"message":    map[string]any{"content": []any{map[string]any{"type": "text", "text": "wrong session"}}},
				},
			},
			wantCause: "native session_id drift",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()
			session, transport, cleanup := newPromptFlowSession(t)
			defer cleanup()
			transport.queryMsgs = tc.queryMsgs
			session.availableCommands = []claude.SlashCommand{{Name: "help", Description: "Help"}}
			home := t.TempDir()
			projects := filepath.Join(home, "projects")
			session.mirror = &sessionMirror{
				log:         session.agent.log,
				store:       &faultSessionStore{SessionStore: NewInMemorySessionStore(), appendErr: errors.New("mirror should not append after poison")},
				projectsDir: projects,
			}
			session.agent.mu.Lock()
			session.agent.sessions[session.id] = session
			session.agent.mu.Unlock()

			require.NoError(t, session.emitAvailableCommandsUpdate(ctx, true))

			_, err := session.Prompt(ctx, TextPromptRequest(session.id, "hello"))
			require.ErrorContains(t, err, tc.wantCause)

			conn, ok := session.agent.connection().(*recordingAgentClient)
			require.True(t, ok)
			updates := availableCommandUpdates(conn.Updates())
			require.Len(t, updates, 2)
			require.Len(t, updates[0].AvailableCommands, 1)
			require.Empty(t, updates[1].AvailableCommands)

			_, err = session.Prompt(ctx, TextPromptRequest(session.id, "again"))
			require.ErrorContains(t, err, tc.wantCause)

			_, err = session.agent.SetSessionConfigOption(ctx, SetModelRequest(session.id, "opus"))
			require.ErrorContains(t, err, tc.wantCause)
		})
	}
}

func requireInvalidCommandAlternative(t *testing.T, err error, alternative string) {
	t.Helper()

	require.Error(t, err)
	var reqErr *acp.RequestError
	require.True(t, errors.As(err, &reqErr), "error = %T %[1]v", err)
	require.Equal(t, -32602, reqErr.Code)
	data, ok := reqErr.Data.(map[string]any)
	require.True(t, ok)
	require.Equal(t, alternative, data["alternative"])
	require.Contains(t, data[jsonFieldMessage], alternative)
}

func requireBackpressureLimit(t *testing.T, err error, limit string) {
	t.Helper()

	require.Error(t, err)
	var reqErr *acp.RequestError
	require.True(t, errors.As(err, &reqErr), "error = %T %[1]v", err)
	require.Equal(t, -32600, reqErr.Code)
	data, ok := reqErr.Data.(map[string]any)
	require.True(t, ok)
	require.Equal(t, limit, data["limit"])
}

func lastSentUserText(t *testing.T, transport *fakeClaudeTransport) string {
	t.Helper()

	sent := transport.Sent()
	require.NotEmpty(t, sent)
	for index := len(sent) - 1; index >= 0; index-- {
		payload, ok := sent[index].(map[string]any)
		if !ok || payload["type"] != claude.MessageTypeUser {
			continue
		}

		message, ok := payload["message"].(map[string]any)
		require.True(t, ok)
		content, ok := message["content"].([]map[string]any)
		require.True(t, ok)
		require.NotEmpty(t, content)
		text, _ := content[0]["text"].(string)

		return text
	}

	require.Fail(t, "no user payload sent")

	return ""
}

func availableCommandUpdates(notifications []acp.SessionNotification) []acp.SessionAvailableCommandsUpdate {
	updates := make([]acp.SessionAvailableCommandsUpdate, 0, len(notifications))
	for _, notification := range notifications {
		if notification.Update.AvailableCommandsUpdate != nil {
			updates = append(updates, *notification.Update.AvailableCommandsUpdate)
		}
	}

	return updates
}

func availableCommandNames(commands []acp.AvailableCommand) []string {
	names := make([]string, 0, len(commands))
	for _, command := range commands {
		names = append(names, command.Name)
	}

	return names
}
