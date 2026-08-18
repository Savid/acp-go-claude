package claudeacp

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/savid/acp-go-claude/internal/mapper"
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
			resp, err := session.Prompt(ctx, TextPromptRequest(session.id, "test-turn", tc.prompt))
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
		resp, err := session.Prompt(ctx, TextPromptRequest(session.id, "test-turn", prompt))
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

	resp, err := session.Prompt(ctx, TextPromptRequest(session.id, "test-turn", "/mcp:server:name args"))
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, resp.StopReason)
	require.Equal(t, "/mcp:server:name args", lastSentUserText(t, transport))

	session.advertisedCommands = []acp.AvailableCommand{{Name: "mcp:server:name"}}
	resp, err = session.Prompt(ctx, TextPromptRequest(session.id, "test-turn", "/mcp:server:name args"))
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, resp.StopReason)
	require.Equal(t, "/server:name (MCP) args", lastSentUserText(t, transport))
}

func TestPromptPublishesDurableTerminalAssistantIdentity(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	session, transport, cleanup := newPromptFlowSession(t)
	defer cleanup()
	transport.queryMsgs = []map[string]any{
		{
			"type": "assistant",
			"uuid": "22222222-2222-4222-8222-222222222222",
			"message": map[string]any{
				"stop_reason": "tool_use",
				"content": []any{map[string]any{
					"type": "text", "text": "before tool",
				}},
			},
		},
		{
			"type": "assistant",
			"uuid": "33333333-3333-4333-8333-333333333333",
			"message": map[string]any{
				"stop_reason": "end_turn",
				"content": []any{map[string]any{
					"type": "text", "text": "done",
				}},
			},
		},
		{"type": "result", "subtype": "success", "is_error": false, "stop_reason": "end_turn"},
	}

	resp, err := session.Prompt(ctx, TextPromptRequest(session.id, "test-turn", "hello"))
	require.NoError(t, err)
	responseClaudeMeta, ok := resp.Meta[claudeMetaKey].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "33333333-3333-4333-8333-333333333333", responseClaudeMeta["messageId"])

	conn, ok := session.agent.connection().(*recordingAgentClient)
	require.True(t, ok)
	var messages []*acp.SessionUpdateAgentMessageChunk
	for _, notification := range conn.Updates() {
		if notification.Update.AgentMessageChunk != nil {
			messages = append(messages, notification.Update.AgentMessageChunk)
		}
	}
	require.Len(t, messages, 2)
	require.Empty(t, messages[0].Meta)
	terminalClaudeMeta, ok := messages[1].Meta[claudeMetaKey].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "33333333-3333-4333-8333-333333333333", terminalClaudeMeta["messageId"])

	var checkpointNotification *acp.SessionNotification
	for _, notification := range conn.Updates() {
		claudeMeta, metaOK := notification.Meta[claudeMetaKey].(map[string]any)
		if metaOK && claudeMeta["messageId"] != nil {
			notificationCopy := notification
			checkpointNotification = &notificationCopy
		}
	}
	require.NotNil(t, checkpointNotification)
	require.NotNil(t, checkpointNotification.Update.SessionInfoUpdate)
	checkpointClaudeMeta, ok := checkpointNotification.Meta[claudeMetaKey].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "33333333-3333-4333-8333-333333333333", checkpointClaudeMeta["messageId"])
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

	_, err := session.Prompt(ctx, TextPromptRequest(session.id, "test-turn", "/compact now"))
	requireBackpressureLimit(t, err, "session_prompt")

	_, err = session.Prompt(ctx, TextPromptRequest(session.id, "test-turn", "/clear now"))
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

	_, err := session.Prompt(ctx, TextPromptRequest(session.id, "test-turn", "hello"))
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

	resp, err := session.Prompt(ctx, TextPromptRequest(session.id, "test-turn", "/"+commandReloadSkills))
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

	resp, err := session.Prompt(ctx, TextPromptRequest(session.id, "test-turn", "/"+commandReloadPlugins))
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

			_, err := session.Prompt(ctx, TextPromptRequest(session.id, "test-turn", "/"+commandReloadSkills))
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

			_, err := session.Prompt(ctx, TextPromptRequest(session.id, "test-turn", "hello"))
			require.ErrorContains(t, err, tc.wantCause)

			conn, ok := session.agent.connection().(*recordingAgentClient)
			require.True(t, ok)
			updates := availableCommandUpdates(conn.Updates())
			require.Len(t, updates, 2)
			require.Len(t, updates[0].AvailableCommands, 1)
			require.Empty(t, updates[1].AvailableCommands)

			_, err = session.Prompt(ctx, TextPromptRequest(session.id, "test-turn", "again"))
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

func TestSessionPromptFlowEdgeBranches(t *testing.T) {
	ctx := context.Background()

	t.Run("query send error", func(t *testing.T) {
		session, transport, cleanup := newPromptFlowSession(t)
		defer cleanup()
		transport.sendErr = errors.New("query failed")
		_, err := session.Prompt(ctx, TextPromptRequest(session.id, "test-turn", "hello"))
		require.ErrorContains(t, err, "query failed")
	})

	t.Run("acquire turn error", func(t *testing.T) {
		session, _, cleanup := newPromptFlowSession(t)
		defer cleanup()
		session.turn <- struct{}{}
		_, err := session.Prompt(ctx, TextPromptRequest(session.id, "test-turn", "hello"))
		require.Error(t, err)
	})

	t.Run("prompt mapping error", func(t *testing.T) {
		session, _, cleanup := newPromptFlowSession(t)
		defer cleanup()
		_, err := session.Prompt(ctx, PromptRequest(session.id, "test-turn", acp.AudioBlock("abc", "audio/wav")))
		var reqErr *acp.RequestError
		require.ErrorAs(t, err, &reqErr)
		require.Equal(t, -32602, reqErr.Code)
		require.Equal(t, map[string]any{"error": "unsupported", "field": "prompt"}, reqErr.Data)
	})

	t.Run("receive after turn cancellation", func(t *testing.T) {
		session, transport, cleanup := newPromptFlowSession(t)
		defer cleanup()
		promptCtx, cancel := context.WithCancel(ctx)
		transport.queryMsgs = nil
		transport.onQuery = cancel
		resp, err := session.Prompt(promptCtx, TextPromptRequest(session.id, "test-turn", "hello"))
		require.NoError(t, err)
		require.Equal(t, acp.StopReasonCancelled, resp.StopReason)
	})

	t.Run("raw emit error does not abort turn", func(t *testing.T) {
		session, _, cleanup := newPromptFlowSession(t)
		defer cleanup()
		session.rawMessages = rawMessageConfig{All: true}
		conn, ok := session.agent.connection().(*recordingAgentClient)
		require.True(t, ok)
		conn.extensionErr = errors.New("raw failed")
		resp, err := session.Prompt(ctx, TextPromptRequest(session.id, "test-turn", "hello"))
		require.NoError(t, err)
		require.Equal(t, acp.StopReasonEndTurn, resp.StopReason)
	})

	t.Run("stream read error answers pending permissions cancelled", func(t *testing.T) {
		session, transport, cleanup := newPromptFlowSession(t)
		defer cleanup()

		permissionCancelled := make(chan struct{}, 1)
		session.permissionCancel = map[string]*permissionRequestCancel{
			"tool": {cancel: func() {
				select {
				case permissionCancelled <- struct{}{}:
				default:
				}
			}},
		}

		transport.queryMsgs = nil
		transport.onQuery = func() { transport.errs <- errors.New("stream failed") }

		_, err := session.Prompt(ctx, TextPromptRequest(session.id, "test-turn", "hello"))
		// The real transport cause is surfaced in the uniform failure, never a
		// bare stream-closed sentinel.
		requireTurnFailure(t, err, -32603, failureCauseTransport, "stream failed")

		select {
		case <-permissionCancelled:
		case <-time.After(time.Second):
			t.Fatal("pending permission was not answered cancelled on stream read error")
		}
	})

	t.Run("empty mirror frame is handled", func(t *testing.T) {
		session, transport, cleanup := newPromptFlowSession(t)
		defer cleanup()
		transport.queryMsgs = []map[string]any{
			{"type": "transcript_mirror", "filePath": "/tmp/ignored.jsonl"},
			{"type": "result", "subtype": "success", "is_error": false, "stop_reason": "end_turn"},
		}
		resp, err := session.Prompt(ctx, TextPromptRequest(session.id, "test-turn", "hello"))
		require.NoError(t, err)
		require.Equal(t, acp.StopReasonEndTurn, resp.StopReason)
	})

	t.Run("mirror append error interrupts", func(t *testing.T) {
		session, transport, cleanup := newPromptFlowSession(t)
		defer cleanup()
		home := t.TempDir()
		projects := filepath.Join(home, "projects")
		session.mirror = &sessionMirror{
			log:         session.agent.log,
			store:       &faultSessionStore{SessionStore: NewInMemorySessionStore(), appendErr: errors.New("append failed")},
			projectsDir: projects,
		}
		transport.queryMsgs = []map[string]any{{
			"type":     "transcript_mirror",
			"filePath": filepath.Join(projects, "project", "11111111-1111-4111-8111-111111111111.jsonl"),
			"entries":  []any{map[string]any{"type": "user"}},
		}}
		_, err := session.Prompt(ctx, TextPromptRequest(session.id, "test-turn", "hello"))
		requireStoreCommitFailure(t, err, errSessionMirrorAppend.Error())
	})

	t.Run("stream usage update error interrupts", func(t *testing.T) {
		session, _, cleanup := newPromptFlowSession(t)
		defer cleanup()
		conn, ok := session.agent.connection().(*recordingAgentClient)
		require.True(t, ok)
		conn.sessionUpdateErr = errors.New("usage failed")
		_, err := session.Prompt(ctx, TextPromptRequest(session.id, "test-turn", "hello"))
		require.ErrorContains(t, err, "usage failed")
	})

	t.Run("message side effect error interrupts", func(t *testing.T) {
		session, transport, cleanup := newPromptFlowSession(t)
		defer cleanup()
		conn, ok := session.agent.connection().(*recordingAgentClient)
		require.True(t, ok)
		conn.sessionUpdateErr = errors.New("compact failed")
		transport.queryMsgs = []map[string]any{{
			"type":    "system",
			"subtype": systemStatus,
			"status":  systemStatusCompacting,
		}}
		_, err := session.Prompt(ctx, TextPromptRequest(session.id, "test-turn", "hello"))
		require.ErrorContains(t, err, "compact failed")
	})

	t.Run("mapped update error interrupts", func(t *testing.T) {
		session, transport, cleanup := newPromptFlowSession(t)
		defer cleanup()
		conn, ok := session.agent.connection().(*recordingAgentClient)
		require.True(t, ok)
		conn.sessionUpdateErr = errors.New("mapped update failed")
		transport.queryMsgs = []map[string]any{{
			"type": "assistant",
			"message": map[string]any{"content": []any{
				map[string]any{"type": "text", "text": "mapped"},
			}},
		}}
		_, err := session.Prompt(ctx, TextPromptRequest(session.id, "test-turn", "hello"))
		require.ErrorContains(t, err, "mapped update failed")
	})

	t.Run("hook update error interrupts", func(t *testing.T) {
		session, transport, cleanup := newPromptFlowSession(t)
		defer cleanup()
		conn, ok := session.agent.connection().(*recordingAgentClient)
		require.True(t, ok)
		conn.sessionUpdateErr = errors.New("hook failed")
		conn.failUpdateAfter = 2
		transport.queryMsgs = []map[string]any{
			{
				"type": "assistant",
				"message": map[string]any{"content": []any{
					map[string]any{"type": "tool_use", "id": "tool-1", "name": "Edit", "input": map[string]any{"file_path": "/tmp/a.go"}},
				}},
			},
			{
				"type":              "system",
				"subtype":           systemSubtypeHookResponse,
				systemHookEventName: systemHookPostToolUse,
				systemToolUseID:     "tool-1",
				systemToolResponse: map[string]any{
					"filePath": "/tmp/a.go",
					"structuredPatch": []any{
						map[string]any{"newStart": 1, "lines": []any{"-old", "+new"}},
					},
				},
			},
		}
		_, err := session.Prompt(ctx, TextPromptRequest(session.id, "test-turn", "hello"))
		require.ErrorContains(t, err, "hook failed")
	})

	t.Run("finish result loop controls", func(t *testing.T) {
		session, transport, cleanup := newPromptFlowSession(t)
		defer cleanup()
		transport.queryMsgs = []map[string]any{
			{"type": "result", "subtype": "success", "is_error": false, "stop_reason": "end_turn"},
			{"type": "result", "subtype": "success", "is_error": false, "stop_reason": "end_turn"},
		}
		previousFinish := finishPromptResultCall
		calls := 0
		finishPromptResultCall = func(
			s *agentSession,
			turnCtx context.Context,
			interruptCtx context.Context,
			params acp.PromptRequest,
			result *claude.ResultMessage,
			state *promptLoopState,
			toolUpdateOptions mapper.ToolUpdateOptions,
			localOnlyCommand bool,
		) (acp.PromptResponse, bool, error) {
			calls++
			if calls == 1 {
				return acp.PromptResponse{}, false, nil
			}

			return previousFinish(s, turnCtx, interruptCtx, params, result, state, toolUpdateOptions, localOnlyCommand)
		}
		t.Cleanup(func() { finishPromptResultCall = previousFinish })
		resp, err := session.Prompt(ctx, TextPromptRequest(session.id, "test-turn", "hello"))
		require.NoError(t, err)
		require.Equal(t, acp.StopReasonEndTurn, resp.StopReason)
		require.Equal(t, 2, calls)
		finishPromptResultCall = previousFinish

		errorSession, errorTransport, errorCleanup := newPromptFlowSession(t)
		defer errorCleanup()
		errorTransport.queryMsgs = []map[string]any{{"type": "result", "subtype": "success", "is_error": false}}
		finishPromptResultCall = func(
			*agentSession,
			context.Context,
			context.Context,
			acp.PromptRequest,
			*claude.ResultMessage,
			*promptLoopState,
			mapper.ToolUpdateOptions,
			bool,
		) (acp.PromptResponse, bool, error) {
			return acp.PromptResponse{}, false, errors.New("finish failed")
		}
		_, err = errorSession.Prompt(ctx, TextPromptRequest(errorSession.id, "test-turn", "hello"))
		require.ErrorContains(t, err, "finish failed")
		finishPromptResultCall = previousFinish
	})

	t.Run("system idle completion", func(t *testing.T) {
		session, transport, cleanup := newPromptFlowSession(t)
		defer cleanup()
		transport.queryMsgs = []map[string]any{{
			"type":      "system",
			"subtype":   systemSubtypeSessionStateChanged,
			systemState: systemStateIdle,
		}}
		resp, err := session.Prompt(ctx, TextPromptRequest(session.id, "test-turn", "hello"))
		require.NoError(t, err)
		require.Equal(t, acp.StopReasonEndTurn, resp.StopReason)
	})

	t.Run("system idle canceled completion", func(t *testing.T) {
		session, transport, cleanup := newPromptFlowSession(t)
		defer cleanup()
		transport.queryMsgs = []map[string]any{{
			"type":      "system",
			"subtype":   systemSubtypeSessionStateChanged,
			systemState: systemStateIdle,
		}}
		transport.onQuery = func() {
			session.mu.Lock()
			session.turnCancelled = true
			session.mu.Unlock()
		}
		resp, err := session.Prompt(ctx, TextPromptRequest(session.id, "test-turn", "hello"))
		require.NoError(t, err)
		require.Equal(t, acp.StopReasonCancelled, resp.StopReason)
	})

	t.Run("system idle live info error", func(t *testing.T) {
		session, transport, cleanup := newPromptFlowSession(t)
		defer cleanup()
		conn, ok := session.agent.connection().(*recordingAgentClient)
		require.True(t, ok)
		conn.sessionUpdateErr = errors.New("idle update failed")
		transport.queryMsgs = []map[string]any{{
			"type":      "system",
			"subtype":   systemSubtypeSessionStateChanged,
			systemState: systemStateIdle,
		}}
		_, err := session.Prompt(ctx, TextPromptRequest(session.id, "test-turn", "hello"))
		require.ErrorContains(t, err, "idle update failed")
	})

	t.Run("trailing mirror frame fails the commit boundary", func(t *testing.T) {
		session, transport, cleanup := newPromptFlowSession(t)
		defer cleanup()
		home := t.TempDir()
		projects := filepath.Join(home, "projects")
		session.mirror = &sessionMirror{
			log:         session.agent.log,
			store:       &faultSessionStore{SessionStore: NewInMemorySessionStore(), appendErr: errors.New("drain append failed")},
			projectsDir: projects,
		}
		transport.queryMsgs = []map[string]any{
			{
				"type":      "system",
				"subtype":   systemSubtypeSessionStateChanged,
				systemState: systemStateIdle,
			},
			{
				"type":     "transcript_mirror",
				"filePath": filepath.Join(projects, "project", "11111111-1111-4111-8111-111111111111.jsonl"),
				"entries":  []any{map[string]any{"type": "user"}},
			},
		}
		_, err := session.Prompt(ctx, TextPromptRequest(session.id, "test-turn", "hello"))
		requireStoreCommitFailure(t, err, errSessionMirrorAppend.Error())
	})

	t.Run("local command emits result text", func(t *testing.T) {
		session, transport, cleanup := newPromptFlowSession(t)
		defer cleanup()
		transport.queryMsgs = []map[string]any{{
			"type":        "result",
			"subtype":     "success",
			"is_error":    false,
			"stop_reason": "end_turn",
			"result":      "context text",
			"usage":       map[string]any{"input_tokens": 1},
		}}
		resp, err := session.Prompt(ctx, TextPromptRequest(session.id, "test-turn", "/context"))
		require.NoError(t, err)
		require.Equal(t, acp.StopReasonEndTurn, resp.StopReason)
		conn, ok := session.agent.connection().(*recordingAgentClient)
		require.True(t, ok)
		require.NotEmpty(t, conn.Updates())
	})
}

func TestFinishPromptResultEdges(t *testing.T) {
	ctx := context.Background()
	session, transport, cleanup := newPromptFlowSession(t)
	defer cleanup()

	state := &promptLoopState{}
	tracker := mapper.NewWorkflowTracker()
	updates := mapper.MessageToUpdatesWithOptions(&claude.SystemMessage{
		Subtype: "task_started",
		Raw: map[string]any{
			"task_id":     "task-1",
			"tool_use_id": "workflow-1",
			"summary":     "running",
		},
	}, mapper.ToolUpdateOptions{Workflow: tracker})
	require.NotNil(t, updates)

	resp, done, err := session.finishPromptResult(ctx, ctx, TextPromptRequest(session.id, "test-turn", "hello"), &claude.ResultMessage{
		Origin: map[string]any{"kind": originKindTaskNotification},
		Usage:  &claude.Usage{InputTokens: 1},
	}, state, mapper.ToolUpdateOptions{Workflow: tracker}, false)
	require.NoError(t, err)
	require.False(t, done)
	require.Empty(t, resp)

	session.logUnknownStopReason(ctx, &claude.ResultMessage{StopReason: "future"})

	transport.controlErr = map[string]error{"get_context_usage": errors.New("usage failed")}
	_, done, err = session.finishPromptResult(ctx, ctx, TextPromptRequest(session.id, "test-turn", "hello"), &claude.ResultMessage{
		Usage: &claude.Usage{InputTokens: 1},
	}, &promptLoopState{}, mapper.ToolUpdateOptions{}, false)
	require.NoError(t, err)
	require.True(t, done)
	transport.controlErr = nil

	conn, ok := session.agent.connection().(*recordingAgentClient)
	require.True(t, ok)
	conn.sessionUpdateErr = errors.New("result usage failed")
	_, _, err = session.finishPromptResult(ctx, ctx, TextPromptRequest(session.id, "test-turn", "hello"), &claude.ResultMessage{
		Usage: &claude.Usage{InputTokens: 1},
	}, &promptLoopState{}, mapper.ToolUpdateOptions{}, false)
	require.ErrorContains(t, err, "result usage failed")
	conn.sessionUpdateErr = nil

	conn.sessionUpdateErr = errors.New("native identity failed")
	_, err = session.finishPromptSystemIdle(
		ctx,
		ctx,
		TextPromptRequest(session.id, "test-turn", "hello"),
		&promptLoopState{lastAssistantMessageID: "33333333-3333-4333-8333-333333333333"},
		"",
	)
	require.ErrorContains(t, err, "native identity failed")
	conn.sessionUpdateErr = nil

	_, _, err = session.finishPromptResult(ctx, ctx, TextPromptRequest(session.id, "test-turn", "hello"), &claude.ResultMessage{
		IsError: true,
		Subtype: "error",
		Result:  "failed",
	}, &promptLoopState{}, mapper.ToolUpdateOptions{}, false)
	require.Error(t, err)

	transport.context = map[string]any{}
	conn.sessionUpdateErr = errors.New("result native identity failed")
	_, _, err = session.finishPromptResult(
		ctx,
		ctx,
		TextPromptRequest(session.id, "test-turn", "hello"),
		&claude.ResultMessage{},
		&promptLoopState{lastAssistantMessageID: "33333333-3333-4333-8333-333333333333"},
		mapper.ToolUpdateOptions{},
		false,
	)
	require.ErrorContains(t, err, "result native identity failed")
	conn.sessionUpdateErr = nil

	transport.context = map[string]any{}
	conn.sessionUpdateErr = errors.New("local result failed")
	_, _, err = session.finishPromptResult(ctx, ctx, TextPromptRequest(session.id, "test-turn", "/context"), &claude.ResultMessage{
		Result: "local text",
	}, &promptLoopState{}, mapper.ToolUpdateOptions{}, true)
	require.ErrorContains(t, err, "local result failed")
	conn.sessionUpdateErr = nil

	transport.context = map[string]any{}
	conn.sessionUpdateErr = errors.New("live info failed")
	_, _, err = session.finishPromptResult(ctx, ctx, TextPromptRequest(session.id, "test-turn", "hello"), &claude.ResultMessage{}, &promptLoopState{}, mapper.ToolUpdateOptions{}, false)
	require.ErrorContains(t, err, "live info failed")
	conn.sessionUpdateErr = nil
}

func TestFinishPromptResultEmitErrorBranches(t *testing.T) {
	ctx := context.Background()

	localSession, localTransport, localCleanup := newPromptFlowSession(t)
	defer localCleanup()
	localTransport.context = map[string]any{}
	localSession.agent.setConnection(newFailingSessionUpdateClient(errors.New("local result failed")))
	_, _, err := localSession.finishPromptResult(ctx, ctx, TextPromptRequest(localSession.id, "test-turn", "/context"), &claude.ResultMessage{
		Result: "local text",
	}, &promptLoopState{}, mapper.ToolUpdateOptions{}, true)
	require.ErrorContains(t, err, "local result failed")

	liveSession, liveTransport, liveCleanup := newPromptFlowSession(t)
	defer liveCleanup()
	liveTransport.context = map[string]any{}
	liveSession.agent.setConnection(newFailingSessionUpdateClient(errors.New("live info failed")))
	_, _, err = liveSession.finishPromptResult(ctx, ctx, TextPromptRequest(liveSession.id, "test-turn", "hello"), &claude.ResultMessage{}, &promptLoopState{}, mapper.ToolUpdateOptions{}, false)
	require.ErrorContains(t, err, "live info failed")
}

type failingSessionUpdateClient struct {
	*recordingAgentClient
	err error
}

func newFailingSessionUpdateClient(err error) *failingSessionUpdateClient {
	return &failingSessionUpdateClient{recordingAgentClient: newRecordingAgentClient(), err: err}
}

func (c *failingSessionUpdateClient) SessionUpdate(context.Context, acp.SessionNotification) error {
	return c.err
}

func TestPromptHelperRemainingBranches(t *testing.T) {
	ctx := context.Background()
	session, _, cleanup := newPromptFlowSession(t)
	defer cleanup()

	require.NoError(t, session.observePromptMessage(ctx, &claude.StreamEventMessage{
		EventType: streamEventMessageStart,
		Event: map[string]any{"message": map[string]any{
			"model": "claude-sonnet-1m",
			"usage": map[string]any{"input_tokens": 1},
		}},
	}, &promptLoopState{}))
	// The context window is never fabricated from the model name: it stays the
	// harness-reported window seeded on the session.
	require.Equal(t, 200000, session.currentContextWindow())

	tracker := mapper.NewWorkflowTracker()
	_ = mapper.MessageToUpdatesWithOptions(&claude.SystemMessage{Subtype: "task_progress", Raw: map[string]any{}}, mapper.ToolUpdateOptions{Workflow: tracker})
	session.recordWorkflowFrameErrors(ctx, tracker)

	snapshot, ok := streamUsageSnapshot(&claude.StreamEventMessage{
		EventType: streamEventMessageDelta,
		Event:     map[string]any{"usage": map[string]any{"cache_creation_input_tokens": 2, "reasoning_output_tokens": 3}},
	}, usageSnapshot{}, false)
	require.True(t, ok)
	require.Equal(t, 5, snapshot.total())
	require.Equal(t, usageSnapshot{cacheCreationTokens: 4, reasoningOutputToken: 5}, (usageSnapshot{}).patch(map[string]any{
		"cache_creation_input_tokens": 4,
		"reasoning_output_tokens":     5,
	}))

	transport := newFakeClaudeTransport()
	transport.controlErr = map[string]error{"get_context_usage": errors.New("usage failed")}
	client := claude.NewClient(nil, claude.Options{}, transport)
	require.NoError(t, client.Start(ctx))
	usageErrSession := &agentSession{agent: session.agent, id: "usage", client: client}
	usageErrSession.emitCurrentUsageUpdate(ctx)
	require.NoError(t, client.Close())

	// An unknown context window is 0, never a fabricated default.
	unknownWindow := (&agentSession{model: "sonnet"}).currentContextWindow()
	require.Equal(t, 0, unknownWindow)
}

func newPromptFlowSession(t *testing.T) (*agentSession, *fakeClaudeTransport, func()) {
	t.Helper()

	transport := newFakeClaudeTransport()
	agent, conn, _ := newFakeLifecycleAgent(t, transport)
	agent.setConnection(conn)
	client := claude.NewClient(agent.log, claude.Options{}, transport)
	require.NoError(t, client.Start(context.Background()))

	session := &agentSession{
		agent:             agent,
		id:                "prompt-session",
		cwd:               t.TempDir(),
		model:             "sonnet",
		client:            client,
		turn:              make(chan struct{}, sessionTurnCapacity),
		contextWindowSize: 200000,
		mirror:            newSessionMirror(agent.log, nil, t.TempDir(), nil),
	}

	return session, transport, func() { _ = client.Close() }
}

func TestPromptResultAndLocalCommandHelpers(t *testing.T) {
	t.Parallel()

	require.True(t, workflowTaskNotificationResultCompletesPrompt(nil))
	require.False(t, workflowTaskNotificationResultCompletesPrompt(mapper.NewWorkflowTracker()))
	require.NoError(t, providerTurnFailure(nil))
	require.NoError(t, providerTurnFailure(&claude.ResultMessage{IsError: true, StopReason: stopReasonMaxTokens}))
	require.NoError(t, providerTurnFailure(&claude.ResultMessage{IsError: false}))
	require.Error(t, providerTurnFailure(&claude.ResultMessage{Result: "Please run /login first"}))
	err := providerTurnFailure(&claude.ResultMessage{IsError: true, Subtype: "error", Result: "failed", Errors: []string{"one"}})
	require.Error(t, err)

	require.True(t, isNativeProcessExit(claude.ErrMessageStreamClosed))
	require.True(t, isNativeProcessExit(claude.ErrProcessExited))
	require.True(t, isNativeProcessExit(claude.ErrClientNotStarted))
	require.False(t, isNativeProcessExit(context.Canceled))
	require.False(t, localOnlySlashCommand([]acp.ContentBlock{acp.TextBlock(" /context now")}))
	require.True(t, localOnlySlashCommand([]acp.ContentBlock{acp.TextBlock("/extra-usage")}))
	require.True(t, localOnlySlashCommand([]acp.ContentBlock{acp.TextBlock("/heapdump")}))
	require.False(t, localOnlySlashCommand([]acp.ContentBlock{acp.TextBlock("/help")}))
	require.Equal(t, "", firstPromptText([]acp.ContentBlock{{}}))
	require.Equal(t, "hello", firstPromptText([]acp.ContentBlock{acp.TextBlock("hello")}))
	require.Equal(t, "", firstPromptToken("   "))
	require.Equal(t, "", firstPromptToken(" /context now"))
	require.Equal(t, "/context", firstPromptToken("/context now"))
	agent := NewAgent()
	session, cleanup := newStartedAgentSessionForTest(t, agent, "session-1")
	defer cleanup()
	require.ErrorIs(t, session.interruptAfterEmitError(context.Background(), context.Canceled), context.Canceled)
}

func TestStreamUsageAndContextHelpers(t *testing.T) {
	t.Parallel()

	session := &agentSession{model: "sonnet", contextWindowSize: 100}
	start := &claude.StreamEventMessage{EventType: streamEventMessageStart, Event: map[string]any{
		"message": map[string]any{
			"model": "claude-sonnet-4-5-1m",
			"usage": map[string]any{"input_tokens": 1, "output_tokens": 2, "cache_read_input_tokens": 3, "cache_creation_input_tokens": 4, "reasoning_output_tokens": 5},
		},
	}}
	updates, snapshot, known, total := session.streamUsageUpdates(start, usageSnapshot{}, false, 0)
	require.True(t, known)
	require.Equal(t, 15, total)
	require.Len(t, updates, 1)
	// usage_update.size is the harness-reported window (contextWindowSize), never
	// fabricated from the model name.
	require.Equal(t, 100, updates[0].UsageUpdate.Size)
	require.Equal(t, "claude-sonnet-4-5-1m", streamModel(start))

	updates, snapshot, known, total = session.streamUsageUpdates(&claude.StreamEventMessage{
		EventType: streamEventMessageDelta,
		Event:     map[string]any{"usage": map[string]any{"output_tokens": 7, "cache_read_input_tokens": nil}},
	}, snapshot, known, total)
	require.True(t, known)
	require.Equal(t, 17, total)
	require.Len(t, updates, 1)
	require.Equal(t, 0, snapshot.cacheReadTokens)

	updates, _, _, total = session.streamUsageUpdates(&claude.StreamEventMessage{EventType: "other"}, snapshot, known, total)
	require.Nil(t, updates)
	require.Equal(t, 17, total)
	updates, snapshot, known, total = session.streamUsageUpdates(&claude.StreamEventMessage{
		EventType: streamEventMessageDelta,
		Event:     map[string]any{"usage": map[string]any{"output_tokens": 7}},
	}, snapshot, known, total)
	require.Nil(t, updates)
	require.True(t, known)
	require.Equal(t, 17, total)
	_, ok := streamUsageSnapshot(&claude.StreamEventMessage{EventType: streamEventMessageStart, Event: map[string]any{"message": map[string]any{}}}, usageSnapshot{}, false)
	require.False(t, ok)
	_, ok = streamUsageSnapshot(&claude.StreamEventMessage{EventType: streamEventMessageDelta}, usageSnapshot{}, false)
	require.False(t, ok)
	require.Equal(t, "", streamModel(nil))
	require.Equal(t, "", streamModel(&claude.StreamEventMessage{EventType: streamEventMessageDelta}))

	meta := streamUsageMeta(snapshot)
	require.NotEmpty(t, meta[claudeMetaKey])
	require.Equal(t, 17, snapshot.total())
	require.Equal(t, usageSnapshot{inputTokens: 3}, usageSnapshotFromMap(map[string]any{"input_tokens": 3}))
	require.Equal(t, usageSnapshot{inputTokens: 9}, (usageSnapshot{}).patch(map[string]any{"input_tokens": int64(9)}))

	contextUpdates := session.contextUsageUpdates(&claude.ContextUsage{TotalTokens: 4, MaxTokens: 200})
	require.Len(t, contextUpdates, 1)
	require.Equal(t, 200, session.currentContextWindow())
	require.Nil(t, session.contextUsageUpdates(nil))
	require.Nil(t, session.contextUsageUpdates(&claude.ContextUsage{}))
	// An unknown context window is reported as 0, never fabricated from the model
	// name.
	require.Equal(t, 0, (&agentSession{model: "claude-sonnet-1m"}).currentContextWindow())
	require.True(t, modelHasLargeContext("claude-sonnet-4-5-1m"))
	require.False(t, modelHasLargeContext("claude-sonnet"))
	require.Equal(t, map[string]any{"a": "b"}, mapValue(map[string]any{"a": "b"}))
	require.Nil(t, mapValue("bad"))
	require.Equal(t, "x", stringValue("x"))
	require.Equal(t, "", stringValue(1))
	require.Equal(t, 7, intValue(7))
	require.Equal(t, 8, intValue(int64(8)))
	require.Equal(t, 9, intValue(float64(9)))
	require.Equal(t, 0, intValue("9"))
	value, present := intField(map[string]any{"x": nil}, "x")
	require.True(t, present)
	require.Zero(t, value)
	_, present = intField(nil, "x")
	require.False(t, present)
}

func TestPromptMirrorAndObservationBranches(t *testing.T) {
	ctx := context.Background()
	agent := NewAgent()
	session, cleanup := newStartedAgentSessionForTest(t, agent, "session-1")
	defer cleanup()
	session.mirror = newSessionMirror(agent.log, nil, t.TempDir(), nil)
	require.NoError(t, session.appendSessionMirror(ctx, &claude.TranscriptMirrorMessage{}))
	require.NoError(t, (&agentSession{agent: agent}).appendSessionMirror(ctx, &claude.TranscriptMirrorMessage{}))

	state := &promptLoopState{}
	require.NoError(t, session.observePromptMessage(ctx, &claude.StreamEventMessage{ParentToolUseID: "parent"}, state))
	observeAssistantMessage(&claude.AssistantMessage{ParentToolUseID: "parent", ErrorKind: "ignored", Model: "ignored"}, state)
	require.Empty(t, state.lastAssistantModel)
	require.NoError(t, session.observePromptMessage(ctx, &claude.AssistantMessage{ErrorKind: "kind", Model: "<synthetic>"}, state))
	require.Empty(t, state.lastAssistantModel)

	session.recordWorkflowFrameErrors(ctx, nil)
	(&agentSession{}).recordWorkflowFrameErrors(ctx, mapper.NewWorkflowTracker())

	// A frame whose path falls outside the Claude projects dir is logged and
	// dropped rather than written to a key it does not name.
	outside := &agentSession{agent: agent, id: "outside", mirror: newSessionMirror(agent.log, NewInMemorySessionStore(), t.TempDir(), nil)}
	require.NoError(t, outside.appendSessionMirror(ctx, &claude.TranscriptMirrorMessage{
		FilePath: "/tmp/outside.jsonl",
		Entries:  []SessionStoreEntry{[]byte(`{"type":"user"}`)},
	}))
}

func TestResultUsageHelpers(t *testing.T) {
	t.Parallel()

	session := &agentSession{model: "claude-sonnet", contextWindowSize: 100}
	require.Nil(t, session.resultUsageUpdates(nil, nil, ""))
	require.Nil(t, session.resultUsageUpdates(&claude.ResultMessage{}, nil, ""))

	cost := 0.02
	updates := session.resultUsageUpdates(&claude.ResultMessage{
		TotalCostUSD:     &cost,
		Origin:           map[string]any{"kind": originKindTaskNotification},
		StructuredOutput: map[string]any{"ok": true},
		Usage:            &claude.Usage{InputTokens: 1, OutputTokens: 2, CachedInputTokens: 3, CacheCreationInputTokens: 4, ReasoningOutputTokens: 5},
		ModelUsage: map[string]claude.ModelUsage{
			"claude-sonnet-long": {ContextWindow: 123},
		},
	}, nil, "claude-sonnet")
	require.Len(t, updates, 1)
	require.Equal(t, 15, updates[0].UsageUpdate.Used)
	require.Equal(t, 123, updates[0].UsageUpdate.Size)
	claudeMeta, ok := updates[0].UsageUpdate.Meta[claudeMetaKey].(map[string]any)
	require.True(t, ok)
	require.Equal(t, map[string]any{"ok": true}, claudeMeta[structuredOutputMetaKey])
	origin, ok := claudeMeta[rawMessageOriginKey].(map[string]any)
	require.True(t, ok)
	require.Equal(t, originKindTaskNotification, origin["kind"])

	updates = session.resultUsageUpdates(&claude.ResultMessage{Usage: &claude.Usage{InputTokens: 1}}, &claude.ContextUsage{TotalTokens: 8, MaxTokens: 456}, "claude-sonnet")
	require.Equal(t, 8, updates[0].UsageUpdate.Used)
	require.Equal(t, 456, updates[0].UsageUpdate.Size)

	usage, ok := matchingModelUsage(map[string]claude.ModelUsage{"abc": {InputTokens: 1}}, "")
	require.False(t, ok)
	require.Zero(t, usage)
	usage, ok = matchingModelUsage(map[string]claude.ModelUsage{"abc": {InputTokens: 1}}, "abc")
	require.True(t, ok)
	require.Equal(t, 1, usage.InputTokens)
	usage, ok = matchingModelUsage(map[string]claude.ModelUsage{"abcdef": {InputTokens: 2}}, "abcxyz")
	require.True(t, ok)
	require.Equal(t, 2, usage.InputTokens)
	require.Equal(t, 2, commonPrefixLength("abc", "abd"))
}

// requireStoreCommitFailure asserts a turn failed at its own durability boundary
// rather than reporting a native failure it did not have.
func requireStoreCommitFailure(t *testing.T, err error, message string) {
	t.Helper()

	var reqErr *acp.RequestError

	require.ErrorAs(t, err, &reqErr)
	require.Equal(t, -32603, reqErr.Code)

	data, ok := reqErr.Data.(map[string]any)
	require.True(t, ok)
	require.Equal(t, "claude_store_commit_failed", data[jsonFieldError])
	require.Contains(t, data[jsonFieldMessage], message)
}
