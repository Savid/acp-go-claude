package claudeacp

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/savid/acp-go-claude/internal/lifecycle"
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
	}{
		{
			name: "conversation reset frame",
			queryMsgs: []map[string]any{
				{"type": "conversation_reset"},
				{"type": "transcript_mirror", "filePath": "/tmp/ignored.jsonl", "entries": []any{map[string]any{"type": "user"}}},
			},
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
			require.ErrorContains(t, err, "native session invariant failed")

			conn, ok := session.agent.connection().(*recordingAgentClient)
			require.True(t, ok)
			updates := availableCommandUpdates(conn.Updates())
			require.Len(t, updates, 2)
			require.Len(t, updates[0].AvailableCommands, 1)
			require.Empty(t, updates[1].AvailableCommands)

			_, err = session.Prompt(ctx, TextPromptRequest(session.id, "test-turn", "again"))
			require.ErrorContains(t, err, "native session invariant failed")

			_, err = session.agent.SetSessionConfigOption(ctx, SetModelRequest(session.id, "opus"))
			require.ErrorContains(t, err, "native session invariant failed")
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
		requireTurnFailure(t, err, -32603, failureCauseTransport, nativeTransportFailureMessage)
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
		transport.onQuery = func() {
			incarnation := session.currentNativeIncarnation()
			session.mu.Lock()
			entry := session.permissionCancel["tool"]
			entry.owner = lifecycleInteractionOwner{incarnation: incarnation, route: "test-turn"}
			session.mu.Unlock()
			transport.errs <- errors.New("stream failed")
		}

		_, err := session.Prompt(ctx, TextPromptRequest(session.id, "test-turn", "hello"))
		// The real transport cause is surfaced in the uniform failure, never a
		// bare stream-closed sentinel.
		requireTurnFailure(t, err, -32603, failureCauseTransport, nativeTransportFailureMessage)

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
		requireStoreCommitFailure(t, err)
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

	t.Run("foreground mirror failure prevents idle commit", func(t *testing.T) {
		session, transport, cleanup := newPromptFlowSession(t)
		defer cleanup()
		home := t.TempDir()
		projects := filepath.Join(home, "projects")
		session.mirror = &sessionMirror{
			log:         session.agent.log,
			store:       &faultSessionStore{SessionStore: NewInMemorySessionStore(), appendErr: errors.New("prefix append failed")},
			projectsDir: projects,
		}
		transport.queryMsgs = []map[string]any{
			{
				"type":     "transcript_mirror",
				"filePath": filepath.Join(projects, "project", "11111111-1111-4111-8111-111111111111.jsonl"),
				"entries":  []any{map[string]any{"type": "user"}},
			},
			{
				"type":      "system",
				"subtype":   systemSubtypeSessionStateChanged,
				systemState: systemStateIdle,
			},
		}
		_, err := session.Prompt(ctx, TextPromptRequest(session.id, "test-turn", "hello"))
		requireStoreCommitFailure(t, err)
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
	require.Error(t, usageErrSession.emitCurrentUsageUpdate(ctx))
	require.NoError(t, client.Close())

	// An unknown context window is 0, never a fabricated default.
	unknownWindow := (&agentSession{model: "sonnet"}).currentContextWindow()
	require.Equal(t, 0, unknownWindow)
}

func TestPromptDispatchAndPredispatchDrainRejectEveryStaleOwner(t *testing.T) {
	session, _, _, cleanup := newNegotiatedPromptFlowSession(t)
	defer cleanup()
	require.NoError(t, session.serveNativePump(t.Context(), session.currentClient()))
	incarnation := session.currentNativeIncarnation()
	stream := session.lifecycleStream()
	sink := newNativeTurnSink("prompt", incarnation)

	admission := &controlCallbackAdmission{session: session, route: "callback", done: make(chan struct{})}
	session.callbackAdmissions = map[*controlCallbackAdmission]struct{}{admission: {}}
	_, err := session.dispatchPrompt(t.Context(), stream, lifecycle.Submission{}, "prompt", incarnation, sink, func() error { return nil })
	requireBackpressureLimit(t, err, "session_foreground")
	delete(session.callbackAdmissions, admission)

	session.closing = true
	_, err = session.dispatchPrompt(t.Context(), stream, lifecycle.Submission{}, "prompt", incarnation, sink, func() error { return nil })
	require.Error(t, err)
	session.closing = false

	_, err = session.dispatchPrompt(t.Context(), stream, lifecycle.Submission{}, "prompt", nil, sink, func() error { return nil })
	require.Error(t, err)
	_, err = session.dispatchPrompt(t.Context(), stream, lifecycle.Submission{}, "prompt", incarnation, nil, func() error { return nil })
	require.ErrorIs(t, err, errPromptPreDispatchFrame)

	predispatch := newNativeTurnSink("prompt", incarnation)
	predispatch.admit(nativeOwnedFrame{route: "prompt", message: &claude.AssistantMessage{}})
	_, err = session.dispatchPrompt(t.Context(), stream, lifecycle.Submission{}, "prompt", incarnation, predispatch, func() error { return nil })
	require.ErrorIs(t, err, errPromptPreDispatchFrame)

	require.Error(t, session.drainBeforePromptDispatch(t.Context(), nil, incarnation))
	require.Error(t, session.drainBeforePromptDispatch(t.Context(), sink, nil))
	session.clearAutonomousRoute(incarnation)
	require.Error(t, session.drainBeforePromptDispatch(t.Context(), sink, incarnation))
}

func TestPredispatchDrainProjectsEveryAdmittedAutonomousFrame(t *testing.T) {
	t.Run("projection failure", func(t *testing.T) {
		session, _, _, cleanup := newNegotiatedPromptFlowSession(t)
		defer cleanup()
		require.NoError(t, session.serveNativePump(t.Context(), session.currentClient()))
		incarnation := session.currentNativeIncarnation()
		incarnation.superviseOnce.Do(func() {})
		session.rawMessages = rawMessageConfig{All: true}
		session.agent.setConnection(nil)
		sink := newNativeTurnSink("prompt", incarnation)
		sink.admit(nativeOwnedFrame{route: session.autonomousRoute(), message: &claude.AssistantMessage{
			Raw: map[string]any{"type": claude.MessageTypeAssistant},
		}})

		require.ErrorIs(t, session.drainBeforePromptDispatch(t.Context(), sink, incarnation), errACPConnectionNotAttached)
	})

	t.Run("conversation frame", func(t *testing.T) {
		session, _, _, cleanup := newNegotiatedPromptFlowSession(t)
		defer cleanup()
		require.NoError(t, session.serveNativePump(t.Context(), session.currentClient()))
		incarnation := session.currentNativeIncarnation()
		sink := newNativeTurnSink("prompt", incarnation)
		sink.admit(nativeOwnedFrame{route: session.autonomousRoute(), message: &claude.AssistantMessage{
			Content: []claude.ContentBlock{claude.TextBlock{Text: "before prompt"}},
			Raw:     map[string]any{"type": claude.MessageTypeAssistant},
		}})

		require.NoError(t, session.drainBeforePromptDispatch(t.Context(), sink, incarnation))
		require.NotNil(t, session.excursion)
	})

	t.Run("mapping failure is latched before dispatch", func(t *testing.T) {
		session, _, conn, cleanup := newNegotiatedPromptFlowSession(t)
		defer cleanup()
		require.NoError(t, session.serveNativePump(t.Context(), session.currentClient()))
		incarnation := session.currentNativeIncarnation()
		incarnation.superviseOnce.Do(func() {})
		session.agent.setConnection(&selectiveUpdateFailureClient{
			recordingAgentClient: conn,
			err:                  errors.New("autonomous update failed"),
			fail: func(notification acp.SessionNotification) bool {
				return notification.Update.AgentMessageChunk != nil
			},
		})
		sink := newNativeTurnSink("prompt", incarnation)
		sink.admit(nativeOwnedFrame{route: session.autonomousRoute(), message: &claude.AssistantMessage{
			Content: []claude.ContentBlock{claude.TextBlock{Text: "before prompt"}},
			Raw:     map[string]any{"type": claude.MessageTypeAssistant},
		}})

		require.Error(t, session.drainBeforePromptDispatch(t.Context(), sink, incarnation))
		require.Error(t, session.autonomousFailureError())
	})
}

func TestPromptRejectsStateChangesAfterItsSinkIsAttached(t *testing.T) {
	startBlocked := func(t *testing.T) (*agentSession, *fakeClaudeTransport, *recordingAgentClient, *nativeTurnSink, chan error, func()) {
		t.Helper()
		session, transport, conn, cleanup := newNegotiatedPromptFlowSession(t)
		t.Cleanup(cleanup)
		require.NoError(t, session.serveNativePump(t.Context(), session.currentClient()))
		pump := session.nativePumpHandle()
		pump.mu.Lock()
		attached := pump.attachedSignalLocked()
		pump.mu.Unlock()
		session.cancelMu.Lock()
		done := make(chan error, 1)
		go func() {
			_, err := session.Prompt(context.Background(), lifecyclePromptRequest(session.id, "blocked-turn", "hello"))
			done <- err
		}()
		<-attached
		pump.mu.Lock()
		sink := pump.sink
		pump.mu.Unlock()
		require.NotNil(t, sink)

		return session, transport, conn, sink, done, session.cancelMu.Unlock
	}

	t.Run("close latch", func(t *testing.T) {
		session, _, _, _, done, release := startBlocked(t)
		session.mu.Lock()
		session.closing = true
		session.mu.Unlock()
		release()
		require.Error(t, <-done)
	})

	t.Run("projection failure", func(t *testing.T) {
		session, _, _, sink, done, release := startBlocked(t)
		session.rawMessages = rawMessageConfig{All: true}
		session.agent.setConnection(nil)
		sink.admit(nativeOwnedFrame{route: session.autonomousRoute(), message: &claude.AssistantMessage{
			Raw: map[string]any{"type": claude.MessageTypeAssistant},
		}})
		release()
		require.ErrorIs(t, <-done, errACPConnectionNotAttached)
	})

	t.Run("autonomous excursion conflict", func(t *testing.T) {
		session, _, _, sink, done, release := startBlocked(t)
		sink.admit(nativeOwnedFrame{route: session.autonomousRoute(), message: &claude.AssistantMessage{
			Content: []claude.ContentBlock{claude.TextBlock{Text: "between turns"}},
			Raw:     map[string]any{"type": claude.MessageTypeAssistant},
		}})
		release()
		err := <-done
		requireBackpressureLimit(t, err, "session_foreground")
	})
}

func TestPromptRejectsTheCurrentIncarnationsLatchedAutonomousFailure(t *testing.T) {
	session, transport, _, cleanup := newNegotiatedPromptFlowSession(t)
	defer cleanup()
	session.mu.Lock()
	session.autonomousErr = errors.New("autonomous projection failed")
	session.autonomousClient = session.client
	session.mu.Unlock()

	_, err := session.Prompt(t.Context(), lifecyclePromptRequest(session.id, "failed-autonomous", "hello"))
	require.Error(t, err)
	require.Zero(t, sentUserFrames(transport))
}

func TestPromptTurnAdmissionIsAtomicWithTerminalSessionState(t *testing.T) {
	session := &agentSession{}
	release, err := session.acquirePromptTurn(t.Context(), false)
	require.NoError(t, err)
	require.NotNil(t, session.turn)
	release()

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	session.turn <- struct{}{}
	release, err = session.acquirePromptTurn(cancelled, false)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, release)
	<-session.turn

	session.turn = make(chan struct{})
	release, err = session.acquirePromptTurn(cancelled, true)
	require.ErrorIs(t, err, context.Canceled)
	require.Nil(t, release)

	session.turn = make(chan struct{}, 2)
	release, err = session.acquirePromptTurn(t.Context(), true)
	require.NoError(t, err)
	require.Len(t, session.turn, 2)
	release()
	require.Empty(t, session.turn)
}

func newPromptFlowSession(t *testing.T) (*agentSession, *fakeClaudeTransport, func()) {
	t.Helper()

	transport := newFakeClaudeTransport()
	agent, conn, _ := newFakeLifecycleAgent(t, transport)
	agent.setConnection(conn)

	session := &agentSession{
		agent:             agent,
		id:                "prompt-session",
		cwd:               t.TempDir(),
		model:             "sonnet",
		turn:              make(chan struct{}, sessionTurnCapacity),
		contextWindowSize: 200000,
		mirror:            newSessionMirror(agent.log, nil, t.TempDir(), nil),
	}
	client := claude.NewClient(agent.log, claude.Options{
		PermissionHandler:  session.handlePermission,
		ElicitationHandler: session.handleElicitation,
		HookHandler:        session.handleHookCallback,
	}, transport)
	client.SetControlHandlerAdmission(session.admitControlCallback)
	session.client = client
	require.NoError(t, client.Start(context.Background()))

	return session, transport, func() {
		session.stopNativePump()
		_ = client.Close()
	}
}

func newNegotiatedPromptFlowSession(t *testing.T) (*agentSession, *fakeClaudeTransport, *recordingAgentClient, func()) {
	t.Helper()

	session, transport, cleanup := newPromptFlowSession(t)
	conn, ok := session.agent.connection().(*recordingAgentClient)
	require.True(t, ok)
	_, err := session.agent.Initialize(t.Context(), acp.InitializeRequest{Meta: lifecycleOfferMeta(1)})
	require.NoError(t, err)

	return session, transport, conn, cleanup
}

func lifecyclePromptRequest(sessionID acp.SessionId, turnNonce, text string) acp.PromptRequest {
	request := TextPromptRequest(sessionID, turnNonce, text)
	request.Meta = withLifecycleMeta(request.Meta, map[string]any{
		lifecycle.MetaKey: map[string]any{
			"version": 1,
			"submission": map[string]any{
				"submissionId": "submission-1",
				"clientNonce":  "client-1",
			},
		},
	})

	return request
}

func sentUserFrames(transport *fakeClaudeTransport) int {
	frames := 0
	for _, payload := range transport.Sent() {
		if msg, ok := payload.(map[string]any); ok && msg["type"] == claude.MessageTypeUser {
			frames++
		}
	}

	return frames
}

func TestPromptRefusesAMissingCorrelationValue(t *testing.T) {
	session, transport, _, cleanup := newNegotiatedPromptFlowSession(t)
	defer cleanup()

	_, err := session.Prompt(t.Context(), TextPromptRequest(session.id, "test-turn", "hello"))
	requireRequestError(t, err, -32602, lifecycle.MetaPath)
	require.Zero(t, sentUserFrames(transport), "a refused correlation value never reaches the harness")
}

func TestPromptRouteGenerationAndRotationFailuresWriteNoUnownedTurn(t *testing.T) {
	t.Run("route generation", func(t *testing.T) {
		session, _, _, cleanup := newNegotiatedPromptFlowSession(t)
		defer cleanup()
		_, err := session.Prompt(t.Context(), lifecyclePromptRequest(session.id, "first-turn", "hello"))
		require.NoError(t, err)

		previous := uuidRandom
		uuidRandom = errReader{err: errors.New("route generation failed")}
		t.Cleanup(func() { uuidRandom = previous })
		_, err = session.Prompt(t.Context(), lifecyclePromptRequest(session.id, "second-turn", "again"))
		require.ErrorContains(t, err, "route generation failed")
	})

	t.Run("route rotation", func(t *testing.T) {
		session, transport, _, cleanup := newNegotiatedPromptFlowSession(t)
		defer cleanup()
		_, err := session.Prompt(t.Context(), lifecyclePromptRequest(session.id, "first-turn", "hello"))
		require.NoError(t, err)
		incarnation := session.currentNativeIncarnation()
		transport.onSend = func(payload any) {
			message, ok := payload.(map[string]any)
			if ok && message["type"] == claude.MessageTypeUser {
				session.clearAutonomousRoute(incarnation)
			}
		}

		_, err = session.Prompt(t.Context(), lifecyclePromptRequest(session.id, "second-turn", "again"))
		require.Error(t, err)
	})
}

func TestExactFailureWhilePromptWaitsReportsTheLatchedCommitFailure(t *testing.T) {
	session, transport, _, cleanup := newNegotiatedPromptFlowSession(t)
	defer cleanup()
	transport.queryMsgs = nil
	querySent := make(chan struct{})
	transport.onQuery = func() { close(querySent) }

	promptErr := make(chan error, 1)
	go func() {
		_, err := session.Prompt(context.Background(),
			lifecyclePromptRequest(session.id, "failed-turn", "hello"))
		promptErr <- err
	}()
	<-querySent

	incarnation := session.currentNativeIncarnation()
	pump := session.nativePumpHandle()
	commitErr := errors.New("prompt mirror commit failed")
	pump.recordCommitError(commitErr)
	incarnation.failed.Store(true)
	pump.recordIncarnationEnd(incarnation, errors.New("native stream lost"))
	incarnation.signalMirrorReady()

	err := <-promptErr
	require.ErrorIs(t, err, commitErr)
	require.NotContains(t, err.Error(), commitErr.Error())
}

// TestPromptFailsWhenTheOpeningSnapshotCannotBeDelivered proves an incarnation
// the host was never told about does not stay live: the frame is never written
// and the process the snapshot could not name is contained.
func TestPromptFailsWhenTheOpeningSnapshotCannotBeDelivered(t *testing.T) {
	session, transport, conn, cleanup := newNegotiatedPromptFlowSession(t)
	defer cleanup()

	conn.sessionUpdateErr = errors.New("snapshot delivery")
	_, err := session.Prompt(t.Context(), lifecyclePromptRequest(session.id, "test-turn", "hello"))
	require.ErrorContains(t, err, "lifecycle delivery failed")
	require.Zero(t, sentUserFrames(transport))
	require.Equal(t, 1, transport.CloseCalls(), "an unannounced incarnation is contained, not left running")

	pump := session.nativePumpHandle()
	pump.mu.Lock()
	defer pump.mu.Unlock()
	require.Nil(t, pump.client, "the pump serves nothing behind a failed opening snapshot")
	require.Nil(t, pump.stop)
}

// TestPromptFailsWhenAcceptanceCannotBeDelivered proves the one lifecycle failure
// this adapter cannot foresee is contained rather than abandoned: the harness has
// the frame, no event covers the turn it opened, so the turn is contained before
// the failure returns.
func TestPromptFailsWhenAcceptanceCannotBeDelivered(t *testing.T) {
	session, transport, conn, cleanup := newNegotiatedPromptFlowSession(t)
	defer cleanup()

	session.agent.setConnection(&lifecycleEventFailingClient{
		recordingAgentClient: conn,
		eventType:            lifecycle.EventPromptAccepted,
		err:                  errors.New("acceptance delivery"),
	})
	_, err := session.Prompt(t.Context(), lifecyclePromptRequest(session.id, "test-turn", "hello"))
	require.ErrorContains(t, err, "lifecycle delivery failed")
	require.Equal(t, 1, sentUserFrames(transport), "the native dispatcher accepted the frame first")
	require.Positive(t, transport.CloseCalls(), "native work no lifecycle event covers is contained")
}

// TestPromptWritesNoFrameBehindALatchedLifecycleStream proves the acceptance
// linearization point preflights the stream: a stream that already lost an event
// can announce nothing, so the harness is never given work the host will never
// hear about.
func TestPromptWritesNoFrameBehindALatchedLifecycleStream(t *testing.T) {
	session, transport, conn, cleanup := newNegotiatedPromptFlowSession(t)
	defer cleanup()

	ctx := t.Context()

	// The incarnation opens and is served, so the refusal below can only come from
	// the dispatch preflight rather than from the pump.
	require.NoError(t, session.serveNativePump(ctx, session.currentClient()))

	// One emission the host never received latches the stream. It carries no
	// native frame of its own, so the only work the harness can be given below is
	// the prompt's.
	stream := session.lifecycleStream()
	conn.sessionUpdateErr = errors.New("host disconnected")
	_, err := stream.dispatch(
		ctx, lifecycle.Submission{SubmissionID: "s", ClientNonce: "c"}, "latch", func() error { return nil })
	require.ErrorContains(t, err, "lifecycle delivery failed")

	conn.sessionUpdateErr = nil
	closeCalls := transport.CloseCalls()

	_, err = session.Prompt(ctx, lifecyclePromptRequest(session.id, "test-turn", "hello"))
	require.ErrorContains(t, err, "lifecycle delivery failed", "the latched stream refuses the turn")
	require.Zero(t, sentUserFrames(transport), "no frame is written behind a stream that cannot announce it")
	require.Equal(t, closeCalls, transport.CloseCalls(), "a refusal before dispatch contains nothing")
}

type lifecycleEventFailingClient struct {
	*recordingAgentClient
	eventType lifecycle.EventType
	state     lifecycle.ForegroundState
	err       error
}

func (c *lifecycleEventFailingClient) SessionUpdate(ctx context.Context, notification acp.SessionNotification) error {
	if envelope, ok := notification.Meta[lifecycle.MetaKey].(map[string]any); ok {
		if event, ok := envelope["event"].(map[string]any); ok {
			if event["type"] == string(c.eventType) && (c.state == "" || event["state"] == string(c.state)) {
				return c.err
			}
		}
	}

	return c.recordingAgentClient.SessionUpdate(ctx, notification)
}

func TestPromptFailsWhenTheTerminalIdleCannotBeDelivered(t *testing.T) {
	session, _, conn, cleanup := newNegotiatedPromptFlowSession(t)
	defer cleanup()

	session.agent.setConnection(&lifecycleEventFailingClient{
		recordingAgentClient: conn,
		eventType:            lifecycle.EventStateUpdate,
		state:                lifecycle.ForegroundIdle,
		err:                  errors.New("terminal idle delivery"),
	})

	_, err := session.Prompt(t.Context(), lifecyclePromptRequest(session.id, "test-turn", "hello"))
	require.ErrorContains(t, err, "lifecycle delivery failed")
}

func TestSettleTurnLifecyclePropagatesIncarnationLossFailure(t *testing.T) {
	ctx := t.Context()
	session, conn, stream := newLifecycleStreamTestSession(t)
	require.NoError(t, stream.incarnate(ctx))
	_, err := stream.dispatch(ctx, lifecycle.Submission{SubmissionID: "s", ClientNonce: "c"}, "nonce", func() error { return nil })
	require.NoError(t, err)

	conn.sessionUpdateErr = errors.New("loss delivery")
	_, err = session.settleTurnLifecycle(ctx, stream, "another-turn", acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil)
	require.ErrorContains(t, err, "lifecycle delivery failed")

	session.stopNativePump()
}

func TestPromptResultAndLocalCommandHelpers(t *testing.T) {
	t.Parallel()

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
func requireStoreCommitFailure(t *testing.T, err error) {
	t.Helper()

	var reqErr *acp.RequestError

	require.ErrorAs(t, err, &reqErr)
	require.Equal(t, -32603, reqErr.Code)

	data, ok := reqErr.Data.(map[string]any)
	require.True(t, ok)
	require.Equal(t, "claude_store_commit_failed", data[jsonFieldError])
	require.Equal(t, "session store commit failed", data[jsonFieldMessage])
}

func TestInterruptAfterEmitContainmentResidualBranch(t *testing.T) {
	transport := newFakeClaudeTransport()
	client := claude.NewClient(nil, claude.Options{}, &closeErrTransport{
		Transport: transport,
		err:       ErrContainmentIncomplete,
	})
	require.NoError(t, client.Start(t.Context()))
	session := &agentSession{
		agent:       NewAgent(),
		client:      client,
		cancel:      func() {},
		canRelaunch: true,
	}
	err := session.interruptAfterEmitError(t.Context(), errors.New("emit refused"))
	require.ErrorIs(t, err, ErrContainmentIncomplete)
}
