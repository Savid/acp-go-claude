package claudeacp

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/savid/acp-go-claude/internal/mapper"
	"github.com/stretchr/testify/require"
)

func TestSessionEmitAndReplayErrorBranches(t *testing.T) {
	t.Parallel()

	session := &Session{
		agent:       NewAgent(WithClaudeHome(t.TempDir())),
		id:          "session-1",
		rawMessages: rawMessageConfig{All: true},
	}

	require.NoError(t, session.emitUpdates(context.Background(), nil))
	require.NoError(t, session.emitOptionalUpdates(context.Background(), []acp.SessionUpdate{acp.UpdateAgentMessageText("hello")}))
	require.Error(t, session.emitUpdates(context.Background(), []acp.SessionUpdate{acp.UpdateAgentMessageText("hello")}))
	require.NoError(t, session.emitRawClaudeMessage(context.Background(), &claude.SystemMessage{
		Raw: map[string]any{"type": "system"},
	}))
	require.Error(t, session.replayTranscript(context.Background(), "/missing/transcript.jsonl"))
}

func TestSessionEmitUpdatesSkipsAfterAgentClose(t *testing.T) {
	t.Parallel()

	agent := NewAgent(WithClaudeHome(t.TempDir()))
	agent.setConnection(&stubAgentClient{})
	session := &Session{agent: agent, id: "session-1"}

	require.NoError(t, agent.Close())
	require.ErrorIs(t, session.emitUpdates(context.Background(), []acp.SessionUpdate{acp.UpdateAgentMessageText("closed")}), errAgentClosed)
	require.NoError(t, session.emitOptionalUpdates(context.Background(), []acp.SessionUpdate{acp.UpdateAgentMessageText("closed")}))
	require.NoError(t, session.emitRawClaudeMessage(context.Background(), &claude.SystemMessage{
		Raw: map[string]any{"type": "system"},
	}))
}

type blockingSessionUpdateClient struct {
	stubAgentClient

	once    sync.Once
	started chan struct{}
	release chan struct{}
}

func (c *blockingSessionUpdateClient) SessionUpdate(ctx context.Context, update acp.SessionNotification) error {
	c.once.Do(func() { close(c.started) })

	select {
	case <-c.release:
	case <-ctx.Done():
		return ctx.Err()
	}

	return c.stubAgentClient.SessionUpdate(ctx, update)
}

func TestSessionEmitUpdatesDoesNotHoldAgentLockDuringWrite(t *testing.T) {
	t.Parallel()

	agent := NewAgent(WithClaudeHome(t.TempDir()))
	client := &blockingSessionUpdateClient{
		started: make(chan struct{}),
		release: make(chan struct{}),
	}
	agent.setConnection(client)

	session := &Session{agent: agent, id: "session-1"}
	emitDone := make(chan error, 1)
	go func() {
		emitDone <- session.emitUpdates(context.Background(), []acp.SessionUpdate{acp.UpdateAgentMessageText("blocked")})
	}()

	select {
	case <-client.started:
	case <-time.After(time.Second):
		t.Fatal("session update did not start")
	}

	closeDone := make(chan error, 1)
	go func() {
		closeDone <- agent.Close()
	}()

	select {
	case err := <-closeDone:
		require.NoError(t, err)
	case <-time.After(time.Second):
		t.Fatal("agent close blocked on session update write")
	}

	close(client.release)
	require.NoError(t, <-emitDone)
}

func TestSessionReplayTranscriptEmitsTruncationWarning(t *testing.T) {
	t.Parallel()

	const replayUpdateLimit = 10000

	agent := NewAgent(WithClaudeHome(t.TempDir()))
	client := &stubAgentClient{}
	agent.setConnection(client)
	session := &Session{agent: agent, id: "session-1"}

	lines := make([]string, replayUpdateLimit+1)
	for i := range lines {
		lines[i] = `{"type":"user","uuid":"22222222-2222-4222-8222-222222222222","cwd":"/repo","message":{"content":"hello"}}`
	}

	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	require.NoError(t, os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600))

	require.NoError(t, session.replayTranscript(context.Background(), path))

	updates := client.recordedUpdates()
	require.Len(t, updates, replayUpdateLimit+1)
	require.Contains(t, updates[0].Update.AgentMessageChunk.Content.Text.Text, "transcript replay was truncated")
	require.Equal(t, "hello", updates[1].Update.UserMessageChunk.Content.Text.Text)
}

func TestSessionCloseJoinsClientAndBridgeErrors(t *testing.T) {
	t.Parallel()

	clientErr := errors.New("client close failed")
	bridgeErr := errors.New("bridge close failed")
	fake := newAgentFakeTransport()
	fake.closeErr = clientErr
	session := &Session{
		agent:  NewAgent(WithClaudeHome(t.TempDir())),
		client: claude.NewClient(nil, claude.Options{}, fake),
		mcpBridge: &mcpSessionBridge{
			agent: NewAgent(),
			done:  make(chan struct{}),
			conns: make(map[*mcpBridgeConn]struct{}),
			ln:    closeErrorListener{err: bridgeErr},
		},
	}

	err := session.Close(context.Background())
	require.ErrorIs(t, err, clientErr)
	require.ErrorIs(t, err, bridgeErr)
	require.True(t, fake.isClosed())
}

func TestSessionCloseCancelsAndWaitsForActivePrompt(t *testing.T) {
	t.Parallel()

	fake := newAgentFakeTransport()
	fake.suppressResult = true
	agent := NewAgent(WithClaudeHome(t.TempDir()))
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(nil, options, fake)
	}

	resp, err := agent.NewSession(context.Background(), acp.NewSessionRequest{Cwd: "/repo", McpServers: []acp.McpServer{}})
	require.NoError(t, err)
	session, err := agent.session(resp.SessionId)
	require.NoError(t, err)

	promptDone := make(chan error, 1)
	go func() {
		_, promptErr := session.Prompt(context.Background(), acp.PromptRequest{
			SessionId: resp.SessionId,
			Prompt:    []acp.ContentBlock{acp.TextBlock("hello")},
		})
		promptDone <- promptErr
	}()

	require.Eventually(t, func() bool {
		return len(sentUserPayloads(fake)) == 1
	}, time.Second, 10*time.Millisecond)

	require.NoError(t, session.Close(context.Background()))

	select {
	case <-promptDone:
	case <-time.After(time.Second):
		t.Fatal("session close returned before prompt goroutine exited")
	}
}

func TestSessionCloseReportsTurnWaitTimeout(t *testing.T) {
	t.Parallel()

	fake := newAgentFakeTransport()
	session := &Session{
		agent:         NewAgent(WithClaudeHome(t.TempDir())),
		client:        claude.NewClient(nil, claude.Options{}, fake),
		turn:          make(chan struct{}, 1),
		closeTurnWait: 10 * time.Millisecond,
	}
	session.turn <- struct{}{}

	err := session.Close(context.Background())
	require.ErrorIs(t, err, context.DeadlineExceeded)
	require.True(t, fake.isClosed())
}

func TestSessionCloseHonorsCanceledContext(t *testing.T) {
	t.Parallel()

	fake := newAgentFakeTransport()
	session := &Session{
		agent:  NewAgent(WithClaudeHome(t.TempDir())),
		client: claude.NewClient(nil, claude.Options{}, fake),
		turn:   make(chan struct{}, 1),
	}
	session.turn <- struct{}{}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	err := session.Close(ctx)
	require.ErrorIs(t, err, context.Canceled)
	require.True(t, fake.isClosed())
}

func TestSessionPromptCancellationAndInputErrors(t *testing.T) {
	t.Parallel()

	fake := newAgentFakeTransport()
	fake.suppressResult = true

	agent := NewAgent(WithClaudeHome(t.TempDir()))
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(nil, options, fake)
	}

	sessionResp, err := agent.NewSession(context.Background(), acp.NewSessionRequest{Cwd: "/repo", McpServers: []acp.McpServer{}})
	require.NoError(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan acp.PromptResponse, 1)
	errs := make(chan error, 1)
	go func() {
		resp, promptErr := agent.Prompt(ctx, acp.PromptRequest{
			SessionId: sessionResp.SessionId,
			Prompt:    []acp.ContentBlock{acp.TextBlock("hello")},
		})
		if promptErr != nil {
			errs <- promptErr

			return
		}

		done <- resp
	}()

	require.Eventually(t, func() bool {
		return len(sentUserPayloads(fake)) == 1
	}, time.Second, 10*time.Millisecond)
	cancel()

	select {
	case promptErr := <-errs:
		require.NoError(t, promptErr)
	case resp := <-done:
		require.Equal(t, acp.StopReasonCancelled, resp.StopReason)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for cancelled prompt")
	}

	_, err = agent.Prompt(context.Background(), acp.PromptRequest{
		SessionId: sessionResp.SessionId,
		Prompt:    []acp.ContentBlock{acp.AudioBlock("abc", "audio/wav")},
	})
	require.Error(t, err)
}

func TestSessionPromptEmitsLiveSessionInfoUpdate(t *testing.T) {
	t.Parallel()

	fake := newAgentFakeTransport()
	agent := NewAgent(WithClaudeHome(t.TempDir()))
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(nil, options, fake)
	}

	client := &recordingACPClient{}
	_ = connectAgentForTest(t, agent, client)

	sessionResp, err := agent.NewSession(context.Background(), acp.NewSessionRequest{
		Cwd:        "/repo",
		McpServers: []acp.McpServer{},
	})
	require.NoError(t, err)

	_, err = agent.Prompt(context.Background(), acp.PromptRequest{
		SessionId: sessionResp.SessionId,
		Prompt:    []acp.ContentBlock{acp.TextBlock("  hello\n\tworld  "), acp.ImageBlock("abc", "image/png")},
	})
	require.NoError(t, err)

	var infoUpdate *acp.SessionSessionInfoUpdate
	require.Eventually(t, func() bool {
		for _, notification := range client.recordedUpdates() {
			if notification.Update.SessionInfoUpdate != nil {
				infoUpdate = notification.Update.SessionInfoUpdate

				return true
			}
		}

		return false
	}, time.Second, 10*time.Millisecond)

	require.NotNil(t, infoUpdate.Title)
	require.Equal(t, "hello world", *infoUpdate.Title)
	require.NotNil(t, infoUpdate.UpdatedAt)
	_, err = time.Parse(time.RFC3339, *infoUpdate.UpdatedAt)
	require.NoError(t, err)

	list, err := agent.ListSessions(context.Background(), acp.ListSessionsRequest{})
	require.NoError(t, err)
	require.Len(t, list.Sessions, 1)
	require.Equal(t, "hello world", *list.Sessions[0].Title)
	require.Equal(t, *infoUpdate.UpdatedAt, *list.Sessions[0].UpdatedAt)
}

func TestLiveSessionTitleFromPrompt(t *testing.T) {
	t.Parallel()

	require.Equal(t, "", liveSessionTitleFromPrompt(nil))
	require.Equal(t, "", liveSessionTitleFromPrompt([]acp.ContentBlock{acp.ImageBlock("abc", "image/png")}))
	require.Equal(t, "second title", liveSessionTitleFromPrompt([]acp.ContentBlock{
		acp.TextBlock(" \n\t "),
		acp.TextBlock(" second   title "),
	}))

	longTitle := strings.Repeat("a", liveSessionTitleMaxRunes+10)
	title := liveSessionTitleFromPrompt([]acp.ContentBlock{acp.TextBlock(longTitle)})
	require.Len(t, []rune(title), liveSessionTitleMaxRunes)
	require.True(t, strings.HasSuffix(title, "..."))
}

func TestSessionTurnQueueInitializes(t *testing.T) {
	t.Parallel()

	session := &Session{}
	first := session.turnQueue()
	second := session.turnQueue()

	require.NotNil(t, first)
	require.Equal(t, first, second)
}

func TestSessionWorkContextBranches(t *testing.T) {
	t.Parallel()

	session := &Session{}
	ctx := context.Background()
	workCtx, cancel := session.sessionWorkContext(ctx)
	cancel()
	require.Equal(t, ctx, workCtx)
	require.NoError(t, workCtx.Err())

	turnDone := make(chan struct{})
	session = &Session{turnDone: turnDone}
	workCtx, cancel = session.sessionWorkContext(context.Background())
	cancel()
	require.Eventually(t, func() bool {
		return workCtx.Err() == context.Canceled
	}, time.Second, 10*time.Millisecond)
}

func TestDescribeAlwaysAllow(t *testing.T) {
	t.Parallel()

	require.Equal(t, "Always Allow all Bash", describeAlwaysAllow(nil, "Bash"))
	require.Equal(t, "Always Allow Bash(npm test:*)", describeAlwaysAllow([]map[string]any{{
		jsonFieldType:            permissionUpdateAddRules,
		permissionUpdateBehavior: claude.BehaviorAllow,
		permissionUpdateRules: []any{
			map[string]any{permissionUpdateToolName: "Bash", permissionUpdateRuleContent: "npm test:*"},
		},
	}}, "Bash"))
	require.Equal(t, "Always Allow all Read", describeAlwaysAllow([]map[string]any{{
		jsonFieldType:            permissionUpdateAddRules,
		permissionUpdateBehavior: claude.BehaviorAllow,
		permissionUpdateRules: []any{
			map[string]any{permissionUpdateToolName: "Read"},
		},
	}}, "Read"))
	require.Equal(t, "Always Allow Bash(git status), Bash(git diff:*) and access to /tmp/work", describeAlwaysAllow([]map[string]any{
		{
			jsonFieldType:            permissionUpdateAddRules,
			permissionUpdateBehavior: claude.BehaviorAllow,
			permissionUpdateRules: []any{
				map[string]any{permissionUpdateToolName: "Bash", permissionUpdateRuleContent: "git status"},
				map[string]any{permissionUpdateToolName: "Bash", permissionUpdateRuleContent: "git diff:*"},
			},
		},
		{
			jsonFieldType:               permissionUpdateAddDirs,
			permissionUpdateDirectories: []any{"/tmp/work"},
		},
	}, "Bash"))
	require.Equal(t, "Always Allow all Bash", describeAlwaysAllow([]map[string]any{{
		jsonFieldType:            permissionUpdateAddRules,
		permissionUpdateBehavior: claude.BehaviorDeny,
		permissionUpdateRules: []any{
			map[string]any{permissionUpdateToolName: "Bash", permissionUpdateRuleContent: "rm -rf:*"},
		},
	}}, "Bash"))
	require.Equal(t, "Always Allow all Bash", describeAlwaysAllow([]map[string]any{{
		jsonFieldType:            permissionUpdateAddRules,
		permissionUpdateBehavior: claude.BehaviorAllow,
		permissionUpdateRules: []any{
			map[string]any{permissionUpdateToolName: ""},
		},
	}}, "Bash"))
}

func TestSessionPromptErrorBranches(t *testing.T) {
	t.Parallel()

	t.Run("queued prompt cancellation", func(t *testing.T) {
		t.Parallel()

		fake := newAgentFakeTransport()
		agent := NewAgent(WithClaudeHome(t.TempDir()))
		agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
			return claude.NewClient(nil, options, fake)
		}

		sessionResp, err := agent.NewSession(context.Background(), acp.NewSessionRequest{Cwd: "/repo", McpServers: []acp.McpServer{}})
		require.NoError(t, err)

		session, err := agent.session(sessionResp.SessionId)
		require.NoError(t, err)

		turn := session.turnQueue()
		turn <- struct{}{}

		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		resp, err := agent.Prompt(ctx, acp.PromptRequest{
			SessionId: sessionResp.SessionId,
			Prompt:    []acp.ContentBlock{acp.TextBlock("hello")},
		})
		require.NoError(t, err)
		require.Equal(t, acp.StopReasonCancelled, resp.StopReason)
		require.Empty(t, sentUserPayloads(fake))

		<-turn
	})

	t.Run("receive error", func(t *testing.T) {
		t.Parallel()

		fake := newAgentFakeTransport()
		fake.systemMessages = []map[string]any{{"type": "assistant"}}
		agent := NewAgent(WithClaudeHome(t.TempDir()))
		agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
			return claude.NewClient(nil, options, fake)
		}

		sessionResp, err := agent.NewSession(context.Background(), acp.NewSessionRequest{Cwd: "/repo", McpServers: []acp.McpServer{}})
		require.NoError(t, err)

		_, err = agent.Prompt(context.Background(), acp.PromptRequest{
			SessionId: sessionResp.SessionId,
			Prompt:    []acp.ContentBlock{acp.TextBlock("hello")},
		})
		require.Error(t, err)
	})

	t.Run("fatal receive error removes session", func(t *testing.T) {
		t.Parallel()

		fake := newAgentFakeTransport()
		fake.setSuppressResult(true)
		agent := NewAgent(WithClaudeHome(t.TempDir()))
		agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
			return claude.NewClient(nil, options, fake)
		}

		sessionResp, err := agent.NewSession(context.Background(), acp.NewSessionRequest{Cwd: "/repo", McpServers: []acp.McpServer{}})
		require.NoError(t, err)

		fake.errs <- fmt.Errorf("%w: exit status 7", claude.ErrProcessExited)

		_, err = agent.Prompt(context.Background(), acp.PromptRequest{
			SessionId: sessionResp.SessionId,
			Prompt:    []acp.ContentBlock{acp.TextBlock("hello")},
		})
		var reqErr *acp.RequestError
		require.ErrorAs(t, err, &reqErr)
		require.Equal(t, -32603, reqErr.Code)
		require.True(t, fake.isClosed())

		_, err = agent.Prompt(context.Background(), acp.PromptRequest{
			SessionId: sessionResp.SessionId,
			Prompt:    []acp.ContentBlock{acp.TextBlock("again")},
		})
		require.Error(t, err)
	})

	t.Run("usage update error", func(t *testing.T) {
		t.Parallel()

		fake := newAgentFakeTransport()
		agent := NewAgent(WithClaudeHome(t.TempDir()))
		agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
			return claude.NewClient(nil, options, fake)
		}

		sessionResp, err := agent.NewSession(context.Background(), acp.NewSessionRequest{Cwd: "/repo", McpServers: []acp.McpServer{}})
		require.NoError(t, err)
		attachFailingConnection(agent)

		_, err = agent.Prompt(context.Background(), acp.PromptRequest{
			SessionId: sessionResp.SessionId,
			Prompt:    []acp.ContentBlock{acp.TextBlock("hello")},
		})
		require.Error(t, err)
		require.NotEmpty(t, sentControlRequests(fake, "interrupt"))
	})

	t.Run("stream usage observe error", func(t *testing.T) {
		t.Parallel()

		fake := newAgentFakeTransport()
		fake.systemMessages = []map[string]any{
			{
				"type": "stream_event",
				"event": map[string]any{
					"type": "message_delta",
					"usage": map[string]any{
						"input_tokens": float64(1),
					},
				},
			},
		}
		agent := NewAgent(WithClaudeHome(t.TempDir()))
		agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
			return claude.NewClient(nil, options, fake)
		}

		sessionResp, err := agent.NewSession(context.Background(), acp.NewSessionRequest{Cwd: "/repo", McpServers: []acp.McpServer{}})
		require.NoError(t, err)
		attachFailingConnection(agent)

		_, err = agent.Prompt(context.Background(), acp.PromptRequest{
			SessionId: sessionResp.SessionId,
			Prompt:    []acp.ContentBlock{acp.TextBlock("hello")},
		})
		require.Error(t, err)
		require.NotEmpty(t, sentControlRequests(fake, "interrupt"))
	})

	t.Run("message update error", func(t *testing.T) {
		t.Parallel()

		fake := newAgentFakeTransport()
		fake.assistantText = "hello"
		agent := NewAgent(WithClaudeHome(t.TempDir()))
		agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
			return claude.NewClient(nil, options, fake)
		}

		sessionResp, err := agent.NewSession(context.Background(), acp.NewSessionRequest{Cwd: "/repo", McpServers: []acp.McpServer{}})
		require.NoError(t, err)
		attachFailingConnection(agent)

		_, err = agent.Prompt(context.Background(), acp.PromptRequest{
			SessionId: sessionResp.SessionId,
			Prompt:    []acp.ContentBlock{acp.TextBlock("hello")},
		})
		require.Error(t, err)
		require.NotEmpty(t, sentControlRequests(fake, "interrupt"))
	})

	t.Run("side effect error", func(t *testing.T) {
		t.Parallel()

		fake := newAgentFakeTransport()
		fake.systemMessages = []map[string]any{
			{
				"type":           "system",
				"subtype":        elicitationComplete,
				"elicitation_id": "elicitation-1",
			},
		}
		agent := NewAgent(WithClaudeHome(t.TempDir()))
		agent.clientCapabilities = acp.ClientCapabilities{
			Elicitation: &acp.ElicitationCapabilities{Url: &acp.ElicitationUrlCapabilities{}},
		}
		agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
			return claude.NewClient(nil, options, fake)
		}

		sessionResp, err := agent.NewSession(context.Background(), acp.NewSessionRequest{Cwd: "/repo", McpServers: []acp.McpServer{}})
		require.NoError(t, err)
		attachFailingConnection(agent)

		_, err = agent.Prompt(context.Background(), acp.PromptRequest{
			SessionId: sessionResp.SessionId,
			Prompt:    []acp.ContentBlock{acp.TextBlock("hello")},
		})
		require.Error(t, err)
		require.NotEmpty(t, sentControlRequests(fake, "interrupt"))
	})

	t.Run("local command result update error", func(t *testing.T) {
		t.Parallel()

		fake := newAgentFakeTransport()
		fake.resultText = "context output"
		agent := NewAgent(WithClaudeHome(t.TempDir()))
		agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
			return claude.NewClient(nil, options, fake)
		}

		agent.setConnection(&stubAgentClient{updateErr: errors.New("update failed"), updateErrAfter: 1})

		sessionResp, err := agent.NewSession(context.Background(), acp.NewSessionRequest{Cwd: "/repo", McpServers: []acp.McpServer{}})
		require.NoError(t, err)

		_, err = agent.Prompt(context.Background(), acp.PromptRequest{
			SessionId: sessionResp.SessionId,
			Prompt:    []acp.ContentBlock{acp.TextBlock(localCommandContext)},
		})
		require.Error(t, err)
		require.NotEmpty(t, sentControlRequests(fake, "interrupt"))
	})

	t.Run("hook response update error", func(t *testing.T) {
		t.Parallel()

		fake := newAgentFakeTransport()
		fake.systemMessages = []map[string]any{
			{
				"type": "assistant",
				"message": map[string]any{
					"content": []any{
						map[string]any{
							"type":  "tool_use",
							"id":    "plan-1",
							"name":  enterPlanModeTool,
							"input": map[string]any{},
						},
					},
				},
			},
			{
				"type":              "system",
				"subtype":           systemSubtypeHookResponse,
				systemHookEventName: systemHookPostToolUse,
				systemToolUseID:     "plan-1",
			},
		}
		agent := NewAgent(WithClaudeHome(t.TempDir()))
		agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
			return claude.NewClient(nil, options, fake)
		}

		agent.setConnection(&stubAgentClient{updateErr: errors.New("update failed"), updateErrAfter: 1})

		sessionResp, err := agent.NewSession(context.Background(), acp.NewSessionRequest{Cwd: "/repo", McpServers: []acp.McpServer{}})
		require.NoError(t, err)

		_, err = agent.Prompt(context.Background(), acp.PromptRequest{
			SessionId: sessionResp.SessionId,
			Prompt:    []acp.ContentBlock{acp.TextBlock("hello")},
		})
		require.Error(t, err)
		require.NotEmpty(t, sentControlRequests(fake, "interrupt"))
	})

	t.Run("result session info update error", func(t *testing.T) {
		t.Parallel()

		fake := newAgentFakeTransport()
		agent := NewAgent(WithClaudeHome(t.TempDir()))
		agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
			return claude.NewClient(nil, options, fake)
		}

		sessionResp, err := agent.NewSession(context.Background(), acp.NewSessionRequest{Cwd: "/repo", McpServers: []acp.McpServer{}})
		require.NoError(t, err)

		agent.setConnection(&stubAgentClient{updateErr: errors.New("update failed"), updateErrAfter: 1})

		_, err = agent.Prompt(context.Background(), acp.PromptRequest{
			SessionId: sessionResp.SessionId,
			Prompt:    []acp.ContentBlock{acp.TextBlock("hello")},
		})
		require.Error(t, err)
		require.NotEmpty(t, sentControlRequests(fake, "interrupt"))
	})

	t.Run("system idle session info update error", func(t *testing.T) {
		t.Parallel()

		fake := newAgentFakeTransport()
		fake.systemMessages = []map[string]any{
			{
				"type":       "system",
				"subtype":    systemSubtypeSessionStateChanged,
				systemState:  systemStateIdle,
				"session_id": "session-1",
			},
		}
		agent := NewAgent(WithClaudeHome(t.TempDir()))
		agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
			return claude.NewClient(nil, options, fake)
		}

		sessionResp, err := agent.NewSession(context.Background(), acp.NewSessionRequest{Cwd: "/repo", McpServers: []acp.McpServer{}})
		require.NoError(t, err)

		agent.setConnection(&stubAgentClient{updateErr: errors.New("update failed")})

		_, err = agent.Prompt(context.Background(), acp.PromptRequest{
			SessionId: sessionResp.SessionId,
			Prompt:    []acp.ContentBlock{acp.TextBlock("hello")},
		})
		require.Error(t, err)
		require.NotEmpty(t, sentControlRequests(fake, "interrupt"))
	})
}

func TestSessionRawClaudeMessages(t *testing.T) {
	t.Parallel()

	fake := newAgentFakeTransport()
	fake.systemMessages = []map[string]any{
		{"type": "system", "subtype": "compact_boundary"},
	}
	agent := NewAgent(WithClaudeHome(t.TempDir()))
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(nil, options, fake)
	}

	client := &recordingACPClient{}
	_ = connectAgentForTest(t, agent, client)

	sessionResp, err := agent.NewSession(context.Background(), acp.NewSessionRequest{
		Cwd:        "/repo",
		McpServers: []acp.McpServer{},
		Meta: map[string]any{claudeMetaKey: map[string]any{
			emitRawSDKMessagesKey: []any{
				map[string]any{rawMessageTypeKey: "system", rawMessageSubtypeKey: "compact_boundary"},
				map[string]any{rawMessageTypeKey: "result"},
			},
		}},
	})
	require.NoError(t, err)

	resp, err := agent.Prompt(context.Background(), acp.PromptRequest{
		SessionId: sessionResp.SessionId,
		Prompt:    []acp.ContentBlock{acp.TextBlock("hello")},
	})
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, resp.StopReason)

	require.Eventually(t, func() bool {
		return len(client.recordedExtensions()) == 2
	}, time.Second, 10*time.Millisecond)

	extensions := client.recordedExtensions()
	require.Equal(t, rawClaudeSDKMessageMethod, extensions[0].Method)
	require.Equal(t, string(sessionResp.SessionId), extensions[0].Params[acpFieldSessionID])
	firstMessage, ok := extensions[0].Params[jsonFieldMessage].(map[string]any)
	require.True(t, ok)
	secondMessage, ok := extensions[1].Params[jsonFieldMessage].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "system", firstMessage[rawMessageTypeKey])
	require.Equal(t, "result", secondMessage[rawMessageTypeKey])
}

func TestSessionRawClaudeMessageError(t *testing.T) {
	t.Parallel()

	fake := newAgentFakeTransport()
	fake.assistantText = "hello"
	agent := NewAgent(WithClaudeHome(t.TempDir()))
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(nil, options, fake)
	}

	sessionResp, err := agent.NewSession(context.Background(), acp.NewSessionRequest{
		Cwd:        "/repo",
		McpServers: []acp.McpServer{},
		Meta: map[string]any{claudeMetaKey: map[string]any{
			emitRawSDKMessagesKey: true,
		}},
	})
	require.NoError(t, err)
	attachFailingConnection(agent)

	_, err = agent.Prompt(context.Background(), acp.PromptRequest{
		SessionId: sessionResp.SessionId,
		Prompt:    []acp.ContentBlock{acp.TextBlock("hello")},
	})
	require.Error(t, err)
}

func TestSessionTaskNotificationResultsDoNotEndPrompt(t *testing.T) {
	t.Parallel()

	fake := newAgentFakeTransport()
	fake.systemMessages = []map[string]any{
		{
			"type":           "result",
			"subtype":        "success",
			"stop_reason":    "max_tokens",
			"total_cost_usd": 0.02,
			"origin":         map[string]any{"kind": "task-notification"},
			"usage": map[string]any{
				"input_tokens":  100,
				"output_tokens": 50,
			},
		},
	}
	agent := NewAgent(WithClaudeHome(t.TempDir()))
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(nil, options, fake)
	}

	client := &recordingACPClient{}
	_ = connectAgentForTest(t, agent, client)

	sessionResp, err := agent.NewSession(context.Background(), acp.NewSessionRequest{Cwd: "/repo", McpServers: []acp.McpServer{}})
	require.NoError(t, err)

	resp, err := agent.Prompt(context.Background(), acp.PromptRequest{
		SessionId: sessionResp.SessionId,
		Prompt:    []acp.ContentBlock{acp.TextBlock("hello")},
	})
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, resp.StopReason)
	require.NotNil(t, resp.Usage)
	require.Equal(t, 108, resp.Usage.InputTokens)
	require.Equal(t, 53, resp.Usage.OutputTokens)
	require.Equal(t, 163, resp.Usage.TotalTokens)

	require.Eventually(t, func() bool {
		return len(client.recordedUpdates()) == 3
	}, time.Second, 10*time.Millisecond)

	updates := client.recordedUpdates()
	require.Len(t, updates, 3)
	require.Equal(t, map[string]any{
		usageMetaKey: map[string]any{
			"inputTokens":       float64(100),
			"outputTokens":      float64(50),
			"cachedReadTokens":  float64(0),
			"cachedWriteTokens": float64(0),
			"thoughtTokens":     float64(0),
			"totalTokens":       float64(150),
		},
		rawMessageOriginKey: map[string]any{"kind": "task-notification"},
	}, updates[0].Update.UsageUpdate.Meta[claudeMetaKey])
	require.Equal(t, map[string]any{
		usageMetaKey: map[string]any{
			"inputTokens":       float64(8),
			"outputTokens":      float64(3),
			"cachedReadTokens":  float64(2),
			"cachedWriteTokens": float64(0),
			"thoughtTokens":     float64(0),
			"totalTokens":       float64(13),
		},
		"modelUsage": map[string]any{
			"claude-test": map[string]any{
				"inputTokens":       float64(0),
				"outputTokens":      float64(0),
				"cachedReadTokens":  float64(0),
				"cachedWriteTokens": float64(0),
				"contextWindow":     float64(200000),
			},
		},
	}, updates[1].Update.UsageUpdate.Meta[claudeMetaKey])
	require.NotNil(t, updates[2].Update.SessionInfoUpdate)
}

func TestSessionPromptResultErrors(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name        string
		messages    []map[string]any
		wantCode    int
		wantData    map[string]any
		wantStop    acp.StopReason
		wantNoError bool
	}{
		{
			name: "success is_error returns internal error",
			messages: []map[string]any{
				{
					"type":        "result",
					"subtype":     "success",
					"is_error":    true,
					"stop_reason": "end_turn",
					"result":      "Something went wrong",
				},
			},
			wantCode: -32603,
			wantData: map[string]any{
				"subtype": "success",
				"result":  "Something went wrong",
			},
		},
		{
			name: "error_during_execution is_error returns internal error",
			messages: []map[string]any{
				{
					"type":        "result",
					"subtype":     "error_during_execution",
					"is_error":    true,
					"stop_reason": "end_turn",
					"errors":      []any{"tool failed"},
				},
			},
			wantCode: -32603,
			wantData: map[string]any{
				"subtype": "error_during_execution",
				"errors":  []string{"tool failed"},
			},
		},
		{
			name: "error max turns is_error returns internal error",
			messages: []map[string]any{
				{
					"type":     "result",
					"subtype":  "error_max_turns",
					"is_error": true,
					"errors":   []any{"too many turns"},
				},
			},
			wantCode: -32603,
			wantData: map[string]any{
				"subtype": "error_max_turns",
				"errors":  []string{"too many turns"},
			},
		},
		{
			name: "login text returns auth required",
			messages: []map[string]any{
				{
					"type":     "result",
					"subtype":  "success",
					"is_error": false,
					"result":   "Please run /login",
				},
			},
			wantCode: -32000,
			wantData: map[string]any{jsonFieldError: "Please run /login"},
		},
		{
			name: "assistant error kind is preserved",
			messages: []map[string]any{
				{
					"type":  "assistant",
					"error": "rate_limit",
					"message": map[string]any{
						"model":   "claude-test",
						"content": []any{},
					},
				},
				{
					"type":        "result",
					"subtype":     "success",
					"is_error":    true,
					"stop_reason": "end_turn",
					"result":      "limit reached",
				},
			},
			wantCode: -32603,
			wantData: map[string]any{
				"subtype":   "success",
				"result":    "limit reached",
				"errorKind": "rate_limit",
			},
		},
		{
			name: "is_error max_tokens remains normal response",
			messages: []map[string]any{
				{
					"type":        "result",
					"subtype":     "error_during_execution",
					"is_error":    true,
					"stop_reason": "max_tokens",
					"errors":      []any{"token limit"},
				},
			},
			wantStop:    acp.StopReasonMaxTokens,
			wantNoError: true,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			fake := newAgentFakeTransport()
			fake.systemMessages = tt.messages
			agent := NewAgent(WithClaudeHome(t.TempDir()))
			agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
				return claude.NewClient(nil, options, fake)
			}

			sessionResp, err := agent.NewSession(context.Background(), acp.NewSessionRequest{
				Cwd:        "/repo",
				McpServers: []acp.McpServer{},
			})
			require.NoError(t, err)

			resp, err := agent.Prompt(context.Background(), acp.PromptRequest{
				SessionId: sessionResp.SessionId,
				Prompt:    []acp.ContentBlock{acp.TextBlock("hello")},
			})
			if tt.wantNoError {
				require.NoError(t, err)
				require.Equal(t, tt.wantStop, resp.StopReason)

				return
			}

			var reqErr *acp.RequestError
			require.ErrorAs(t, err, &reqErr)
			require.Equal(t, tt.wantCode, reqErr.Code)
			require.Equal(t, tt.wantData, reqErr.Data)
		})
	}
}

func TestSessionLogsUnknownStopReason(t *testing.T) {
	t.Parallel()

	var logs bytes.Buffer
	fake := newAgentFakeTransport()
	fake.systemMessages = []map[string]any{
		{
			"type":           "result",
			"subtype":        "success",
			"stop_reason":    "future_stop",
			"total_cost_usd": 0.01,
		},
	}
	agent := NewAgent(
		WithClaudeHome(t.TempDir()),
		WithLogger(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))),
	)
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(nil, options, fake)
	}

	sessionResp, err := agent.NewSession(context.Background(), acp.NewSessionRequest{
		Cwd:        "/repo",
		McpServers: []acp.McpServer{},
	})
	require.NoError(t, err)

	resp, err := agent.Prompt(context.Background(), acp.PromptRequest{
		SessionId: sessionResp.SessionId,
		Prompt:    []acp.ContentBlock{acp.TextBlock("hello")},
	})

	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, resp.StopReason)
	require.Contains(t, logs.String(), "unknown Claude stop reason")
	require.Contains(t, logs.String(), "future_stop")
}

func TestSessionStreamUsageUpdates(t *testing.T) {
	t.Parallel()

	fake := newAgentFakeTransport()
	fake.systemMessages = []map[string]any{
		{
			"type":               "stream_event",
			"parent_tool_use_id": nil,
			"event": map[string]any{
				"type": "message_start",
				"message": map[string]any{
					"model": "claude-opus-4-6-1m",
					"usage": map[string]any{
						"input_tokens":                100,
						"output_tokens":               nil,
						"cache_read_input_tokens":     20,
						"cache_creation_input_tokens": 10,
					},
				},
			},
		},
		{
			"type":               "stream_event",
			"parent_tool_use_id": nil,
			"event": map[string]any{
				"type": "message_delta",
				"usage": map[string]any{
					"output_tokens": 50,
				},
			},
		},
		{
			"type":               "stream_event",
			"parent_tool_use_id": nil,
			"event": map[string]any{
				"type": "message_delta",
				"usage": map[string]any{
					"output_tokens": 50,
				},
			},
		},
		{
			"type":               "stream_event",
			"parent_tool_use_id": "subagent-1",
			"event": map[string]any{
				"type": "message_start",
				"message": map[string]any{
					"model": "claude-haiku",
					"usage": map[string]any{"input_tokens": 999},
				},
			},
		},
		{
			"type":        "result",
			"subtype":     "success",
			"stop_reason": "end_turn",
			"usage": map[string]any{
				"input_tokens":                100,
				"output_tokens":               50,
				"cache_read_input_tokens":     20,
				"cache_creation_input_tokens": 10,
			},
			"modelUsage": map[string]any{
				"claude-opus-4-6-1m": map[string]any{
					"cacheCreationInputTokens": 10,
					"contextWindow":            1000000,
				},
			},
		},
	}

	agent := NewAgent(WithClaudeHome(t.TempDir()))
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(nil, options, fake)
	}

	client := &recordingACPClient{}
	_ = connectAgentForTest(t, agent, client)

	sessionResp, err := agent.NewSession(context.Background(), acp.NewSessionRequest{
		Cwd:        "/repo",
		McpServers: []acp.McpServer{},
	})
	require.NoError(t, err)

	resp, err := agent.Prompt(context.Background(), acp.PromptRequest{
		SessionId: sessionResp.SessionId,
		Prompt:    []acp.ContentBlock{acp.TextBlock("hello")},
	})
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, resp.StopReason)
	require.NotNil(t, resp.Usage)
	require.Equal(t, 180, resp.Usage.TotalTokens)
	require.Equal(t, 10, *resp.Usage.CachedWriteTokens)

	var usageUpdates []*acp.SessionUsageUpdate
	require.Eventually(t, func() bool {
		updates := client.recordedUpdates()
		usageUpdates = make([]*acp.SessionUsageUpdate, 0, len(updates))
		for _, update := range updates {
			if update.Update.UsageUpdate != nil {
				usageUpdates = append(usageUpdates, update.Update.UsageUpdate)
			}
		}

		return len(usageUpdates) == 3
	}, time.Second, 10*time.Millisecond)

	require.Len(t, usageUpdates, 3)
	require.Equal(t, 130, usageUpdates[0].Used)
	require.Equal(t, largeContextWindow, usageUpdates[0].Size, "%#v", usageUpdates)
	require.Nil(t, usageUpdates[0].Cost)
	require.Equal(t, map[string]any{
		usageMetaKey: map[string]any{
			"inputTokens":       float64(100),
			"outputTokens":      float64(0),
			"cachedReadTokens":  float64(20),
			"cachedWriteTokens": float64(10),
			"thoughtTokens":     float64(0),
			"totalTokens":       float64(130),
		},
	}, usageUpdates[0].Meta[claudeMetaKey])
	require.Equal(t, 180, usageUpdates[1].Used)
	require.Equal(t, largeContextWindow, usageUpdates[1].Size)
	require.Nil(t, usageUpdates[1].Cost)
	require.Equal(t, map[string]any{
		usageMetaKey: map[string]any{
			"inputTokens":       float64(100),
			"outputTokens":      float64(50),
			"cachedReadTokens":  float64(20),
			"cachedWriteTokens": float64(10),
			"thoughtTokens":     float64(0),
			"totalTokens":       float64(180),
		},
	}, usageUpdates[1].Meta[claudeMetaKey])
	require.Equal(t, 180, usageUpdates[2].Used)
	require.Equal(t, largeContextWindow, usageUpdates[2].Size)
	require.Nil(t, usageUpdates[2].Cost)
	require.Equal(t, map[string]any{
		usageMetaKey: map[string]any{
			"inputTokens":       float64(100),
			"outputTokens":      float64(50),
			"cachedReadTokens":  float64(20),
			"cachedWriteTokens": float64(10),
			"thoughtTokens":     float64(0),
			"totalTokens":       float64(180),
		},
		"modelUsage": map[string]any{
			"claude-opus-4-6-1m": map[string]any{
				"inputTokens":       float64(0),
				"outputTokens":      float64(0),
				"cachedReadTokens":  float64(0),
				"cachedWriteTokens": float64(10),
				"contextWindow":     float64(1000000),
			},
		},
	}, usageUpdates[2].Meta[claudeMetaKey])
}

func TestSessionLocalOnlySlashCommandForwardsResult(t *testing.T) {
	t.Parallel()

	fake := newAgentFakeTransport()
	fake.systemMessages = []map[string]any{
		{
			"type":        "result",
			"subtype":     "success",
			"stop_reason": "end_turn",
			"result":      "context output",
		},
	}
	agent := NewAgent(WithClaudeHome(t.TempDir()))
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(nil, options, fake)
	}

	client := &recordingACPClient{}
	_ = connectAgentForTest(t, agent, client)

	sessionResp, err := agent.NewSession(context.Background(), acp.NewSessionRequest{
		Cwd:        "/repo",
		McpServers: []acp.McpServer{},
	})
	require.NoError(t, err)

	resp, err := agent.Prompt(context.Background(), acp.PromptRequest{
		SessionId: sessionResp.SessionId,
		Prompt:    []acp.ContentBlock{acp.TextBlock("/context")},
	})
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, resp.StopReason)

	require.Eventually(t, func() bool {
		for _, update := range client.recordedUpdates() {
			if update.Update.AgentMessageChunk != nil &&
				update.Update.AgentMessageChunk.Content.Text.Text == "context output" {
				return true
			}
		}

		return false
	}, time.Second, 10*time.Millisecond)

	require.True(t, localOnlySlashCommand([]acp.ContentBlock{acp.ImageBlock("abc", "image/png"), acp.TextBlock("/heapdump now")}))
	require.True(t, localOnlySlashCommand([]acp.ContentBlock{acp.TextBlock("   "), acp.TextBlock("/context")}))
	require.False(t, localOnlySlashCommand([]acp.ContentBlock{acp.TextBlock("/compact")}))
}

func TestSessionSystemStatusMessages(t *testing.T) {
	t.Parallel()

	fake := newAgentFakeTransport()
	fake.systemMessages = []map[string]any{
		{
			"type":       "system",
			"subtype":    systemStatus,
			systemStatus: systemStatusCompacting,
		},
		{
			"type":    "system",
			"subtype": systemSubtypeCompactBoundary,
		},
		{
			"type":          "system",
			"subtype":       systemSubtypeLocalCommandOutput,
			systemContent:   "local output",
			systemStatus:    "ignored",
			"session_id":    "session-1",
			"irrelevantKey": "irrelevant value",
		},
	}
	agent := NewAgent(WithClaudeHome(t.TempDir()))
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(nil, options, fake)
	}

	client := &recordingACPClient{}
	_ = connectAgentForTest(t, agent, client)

	sessionResp, err := agent.NewSession(context.Background(), acp.NewSessionRequest{
		Cwd:        "/repo",
		McpServers: []acp.McpServer{},
	})
	require.NoError(t, err)

	resp, err := agent.Prompt(context.Background(), acp.PromptRequest{
		SessionId: sessionResp.SessionId,
		Prompt:    []acp.ContentBlock{acp.TextBlock("hello")},
	})
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, resp.StopReason)

	var updates []acp.SessionNotification
	require.Eventually(t, func() bool {
		updates = client.recordedUpdates()

		return len(updates) >= 5
	}, time.Second, 10*time.Millisecond)

	var texts []string
	var reset *acp.SessionUsageUpdate
	for _, notification := range updates {
		update := notification.Update
		if update.AgentMessageChunk != nil {
			texts = append(texts, update.AgentMessageChunk.Content.Text.Text)
		}

		if update.UsageUpdate != nil && update.UsageUpdate.Used == 0 {
			reset = update.UsageUpdate
		}
	}

	require.Contains(t, texts, compactingMessageText)
	require.Contains(t, texts, compactingCompleteMessageText)
	require.Contains(t, texts, "local output")
	require.NotNil(t, reset)
	require.Equal(t, defaultContextWindow, reset.Size)
}

func TestSessionSystemIdleFinishesPrompt(t *testing.T) {
	t.Parallel()

	fake := newAgentFakeTransport()
	fake.systemMessages = []map[string]any{
		{
			"type":        "system",
			"subtype":     systemSubtypeSessionStateChanged,
			systemState:   systemStateIdle,
			"session_id":  "session-1",
			"other_field": "ignored",
		},
	}
	agent := NewAgent(WithClaudeHome(t.TempDir()))
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(nil, options, fake)
	}

	sessionResp, err := agent.NewSession(context.Background(), acp.NewSessionRequest{
		Cwd:        "/repo",
		McpServers: []acp.McpServer{},
	})
	require.NoError(t, err)

	resp, err := agent.Prompt(context.Background(), acp.PromptRequest{
		SessionId: sessionResp.SessionId,
		Prompt:    []acp.ContentBlock{acp.TextBlock("hello")},
	})
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, resp.StopReason)
	require.Nil(t, resp.Usage)

	require.True(t, promptFinishedBySystemIdle(&claude.SystemMessage{
		Subtype: systemSubtypeSessionStateChanged,
		Raw:     map[string]any{systemState: systemStateIdle},
	}))
	require.False(t, promptFinishedBySystemIdle(&claude.SystemMessage{
		Subtype: systemSubtypeSessionStateChanged,
		Raw:     map[string]any{systemState: "busy"},
	}))
}

func TestSessionSystemIdleFinishesCancelledPrompt(t *testing.T) {
	t.Parallel()

	fake := newAgentFakeTransport()
	fake.suppressResult = true
	agent := NewAgent(WithClaudeHome(t.TempDir()))
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(nil, options, fake)
	}

	sessionResp, err := agent.NewSession(context.Background(), acp.NewSessionRequest{
		Cwd:        "/repo",
		McpServers: []acp.McpServer{},
	})
	require.NoError(t, err)

	done := make(chan acp.PromptResponse, 1)
	errs := make(chan error, 1)
	go func() {
		resp, promptErr := agent.Prompt(context.Background(), acp.PromptRequest{
			SessionId: sessionResp.SessionId,
			Prompt:    []acp.ContentBlock{acp.TextBlock("hello")},
		})
		if promptErr != nil {
			errs <- promptErr

			return
		}

		done <- resp
	}()

	require.Eventually(t, func() bool {
		return len(sentUserPayloads(fake)) == 1
	}, time.Second, 10*time.Millisecond)
	require.NoError(t, agent.Cancel(context.Background(), acp.CancelNotification{SessionId: sessionResp.SessionId}))
	fake.incoming <- map[string]any{
		"type":       "system",
		"subtype":    systemSubtypeSessionStateChanged,
		systemState:  systemStateIdle,
		"session_id": "session-1",
	}

	select {
	case promptErr := <-errs:
		require.NoError(t, promptErr)
	case resp := <-done:
		require.Equal(t, acp.StopReasonCancelled, resp.StopReason)
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for cancelled system idle")
	}
}

func TestSessionHookResponseSideEffects(t *testing.T) {
	t.Parallel()

	fake := newAgentFakeTransport()
	fake.systemMessages = []map[string]any{
		{
			"type": "assistant",
			"message": map[string]any{
				"content": []any{
					map[string]any{
						"type":  "tool_use",
						"id":    "edit-1",
						"name":  "Edit",
						"input": map[string]any{"file_path": "/repo/a.go"},
					},
					map[string]any{
						"type":  "tool_use",
						"id":    "plan-1",
						"name":  enterPlanModeTool,
						"input": map[string]any{},
					},
				},
			},
		},
		{
			"type":              "system",
			"subtype":           systemSubtypeHookResponse,
			systemHookEventName: systemHookPostToolUse,
			systemToolUseID:     "edit-1",
			systemToolResponse: map[string]any{
				"filePath": "/repo/a.go",
				"structuredPatch": []any{
					map[string]any{"newStart": 2, "lines": []any{"-old", "+new"}},
				},
			},
		},
		{
			"type":              "system",
			"subtype":           systemSubtypeHookResponse,
			systemHookEventName: systemHookPostToolUse,
			systemToolUseID:     "plan-1",
		},
	}
	agent := NewAgent(WithClaudeHome(t.TempDir()))
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(nil, options, fake)
	}

	client := &recordingACPClient{}
	_ = connectAgentForTest(t, agent, client)

	sessionResp, err := agent.NewSession(context.Background(), acp.NewSessionRequest{
		Cwd:        "/repo",
		McpServers: []acp.McpServer{},
	})
	require.NoError(t, err)

	resp, err := agent.Prompt(context.Background(), acp.PromptRequest{
		SessionId: sessionResp.SessionId,
		Prompt:    []acp.ContentBlock{acp.TextBlock("hello")},
	})
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, resp.StopReason)

	var updates []acp.SessionNotification
	require.Eventually(t, func() bool {
		updates = client.recordedUpdates()

		return len(updates) >= 5
	}, time.Second, 10*time.Millisecond)

	var diffUpdate *acp.SessionToolCallUpdate
	var modeUpdate *acp.SessionCurrentModeUpdate
	for _, notification := range updates {
		if notification.Update.ToolCallUpdate != nil && notification.Update.ToolCallUpdate.ToolCallId == "edit-1" {
			if len(notification.Update.ToolCallUpdate.Content) > 0 {
				diffUpdate = notification.Update.ToolCallUpdate
			}
		}

		if notification.Update.CurrentModeUpdate != nil {
			modeUpdate = notification.Update.CurrentModeUpdate
		}
	}

	require.NotNil(t, diffUpdate)
	require.Equal(t, "/repo/a.go", diffUpdate.Content[0].Diff.Path)
	require.Equal(t, "old", *diffUpdate.Content[0].Diff.OldText)
	require.Equal(t, "new", diffUpdate.Content[0].Diff.NewText)
	require.Contains(t, diffUpdate.Meta, claudeMetaKey)
	require.NotNil(t, modeUpdate)
	require.Equal(t, modePlan, modeUpdate.CurrentModeId)

	session, err := agent.session(sessionResp.SessionId)
	require.NoError(t, err)
	require.Equal(t, modePlan, session.mode)
}

func TestSessionHookResponseIgnoredMessages(t *testing.T) {
	t.Parallel()

	session := &Session{}
	options := mapper.ToolUpdateOptions{}
	require.NoError(t, session.emitHookResponseUpdates(context.Background(), &claude.AssistantMessage{}, options))
	require.NoError(t, session.emitHookResponseUpdates(context.Background(), &claude.SystemMessage{
		Subtype: "other",
	}, options))
	require.NoError(t, session.emitHookResponseUpdates(context.Background(), &claude.SystemMessage{
		Subtype: systemSubtypeHookResponse,
		Raw:     map[string]any{systemHookEventName: "PreToolUse"},
	}, options))
	require.NoError(t, session.emitHookResponseUpdates(context.Background(), &claude.SystemMessage{
		Subtype: systemSubtypeHookResponse,
		Raw:     map[string]any{systemHookEventName: systemHookPostToolUse},
	}, options))

	session.markHookHandled("handled")
	require.NoError(t, session.emitHookResponseUpdates(context.Background(), &claude.SystemMessage{
		Subtype: systemSubtypeHookResponse,
		Raw: map[string]any{
			systemHookEventName: systemHookPostToolUse,
			systemToolUseID:     "handled",
		},
	}, options))
}

func TestSessionHandledHooksAreCapped(t *testing.T) {
	t.Parallel()

	session := &Session{}
	session.markHookHandled("hook-0")
	session.markHookHandled("hook-0")

	for i := 1; i <= maxHandledHooks; i++ {
		session.markHookHandled(fmt.Sprintf("hook-%d", i))
	}

	require.False(t, session.hookHandled("hook-0"))
	require.True(t, session.hookHandled("hook-1"))
	require.True(t, session.hookHandled(fmt.Sprintf("hook-%d", maxHandledHooks)))
	require.Len(t, session.handledHooks, maxHandledHooks)
	require.Len(t, session.handledHookOrder, maxHandledHooks)
}

func TestSessionHookCallbackSideEffects(t *testing.T) {
	t.Parallel()

	fake := newAgentFakeTransport()
	agent := NewAgent(WithClaudeHome(t.TempDir()))
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(nil, options, fake)
	}

	client := &recordingACPClient{}
	_ = connectAgentForTest(t, agent, client)

	sessionResp, err := agent.NewSession(context.Background(), acp.NewSessionRequest{
		Cwd:        "/repo",
		McpServers: []acp.McpServer{},
	})
	require.NoError(t, err)

	session, err := agent.session(sessionResp.SessionId)
	require.NoError(t, err)
	baselineUpdates := len(client.recordedUpdates())

	resp, err := session.handleHookCallback(context.Background(), claude.HookRequest{
		EventName: systemHookPostToolUse,
		ToolName:  "Edit",
	})
	require.NoError(t, err)
	require.True(t, resp.Continue)

	resp, err = session.handleHookCallback(context.Background(), claude.HookRequest{
		EventName: "PreToolUse",
		ToolName:  "Edit",
		ToolUseID: "ignored",
	})
	require.NoError(t, err)
	require.True(t, resp.Continue)
	require.Len(t, client.recordedUpdates(), baselineUpdates)

	resp, err = session.handleHookCallback(context.Background(), claude.HookRequest{
		EventName: systemHookPostToolUse,
		ToolName:  "Read",
		ToolUseID: "read-callback",
	})
	require.NoError(t, err)
	require.True(t, resp.Continue)

	resp, err = session.handleHookCallback(context.Background(), claude.HookRequest{
		EventName: systemHookPostToolUse,
		ToolName:  "Edit",
		ToolUseID: "edit-empty-callback",
	})
	require.NoError(t, err)
	require.True(t, resp.Continue)

	resp, err = session.handleHookCallback(context.Background(), claude.HookRequest{
		EventName:    systemHookPostToolUse,
		ToolName:     "Write",
		ToolUseID:    "write-empty-callback",
		ToolResponse: map[string]any{},
	})
	require.NoError(t, err)
	require.True(t, resp.Continue)
	require.Len(t, client.recordedUpdates(), baselineUpdates)

	toolResponse := map[string]any{
		"filePath": "/repo/a.go",
		"structuredPatch": []any{
			map[string]any{"newStart": 2, "lines": []any{"-old", "+new"}},
		},
	}
	resp, err = session.handleHookCallback(context.Background(), claude.HookRequest{
		EventName:    systemHookPostToolUse,
		ToolName:     "Edit",
		ToolUseID:    "edit-callback",
		ToolResponse: toolResponse,
	})
	require.NoError(t, err)
	require.True(t, resp.Continue)

	resp, err = session.handleHookCallback(context.Background(), claude.HookRequest{
		EventName: systemHookPostToolUse,
		ToolName:  enterPlanModeTool,
		ToolUseID: "plan-callback",
	})
	require.NoError(t, err)
	require.True(t, resp.Continue)

	resp, err = session.handleHookCallback(context.Background(), claude.HookRequest{
		EventName: systemHookPostToolUse,
		ToolName:  "Edit",
		ToolUseID: "edit-callback",
	})
	require.NoError(t, err)
	require.True(t, resp.Continue)

	var updates []acp.SessionNotification
	require.Eventually(t, func() bool {
		updates = client.recordedUpdates()

		return len(updates) == baselineUpdates+2
	}, time.Second, 10*time.Millisecond)

	var diffUpdate *acp.SessionToolCallUpdate
	var modeUpdate *acp.SessionCurrentModeUpdate
	for _, notification := range updates {
		if notification.Update.ToolCallUpdate != nil && notification.Update.ToolCallUpdate.ToolCallId == "edit-callback" {
			diffUpdate = notification.Update.ToolCallUpdate
		}

		if notification.Update.CurrentModeUpdate != nil {
			modeUpdate = notification.Update.CurrentModeUpdate
		}
	}

	require.NotNil(t, diffUpdate)
	require.Equal(t, "/repo/a.go", diffUpdate.Content[0].Diff.Path)
	require.Equal(t, "old", *diffUpdate.Content[0].Diff.OldText)
	require.Equal(t, "new", diffUpdate.Content[0].Diff.NewText)
	require.Contains(t, diffUpdate.Meta, claudeMetaKey)
	require.NotNil(t, modeUpdate)
	require.Equal(t, modePlan, modeUpdate.CurrentModeId)
	require.Equal(t, modePlan, session.mode)
}

func TestSessionHookCallbackUpdateErrors(t *testing.T) {
	t.Parallel()

	updateErr := errors.New("session update failed")
	agent := NewAgent(WithClaudeHome(t.TempDir()))
	agent.setConnection(&stubAgentClient{updateErr: updateErr})
	session := &Session{agent: agent, id: "session-1"}

	_, err := session.handleHookCallback(context.Background(), claude.HookRequest{
		EventName: systemHookPostToolUse,
		ToolName:  enterPlanModeTool,
		ToolUseID: "plan-callback",
	})
	require.ErrorIs(t, err, updateErr)

	_, err = session.handleHookCallback(context.Background(), claude.HookRequest{
		EventName: systemHookPostToolUse,
		ToolName:  "Write",
		ToolUseID: "write-callback",
		ToolResponse: map[string]any{
			"filePath": "/repo/a.go",
			"structuredPatch": []any{
				map[string]any{"newStart": 2, "lines": []any{"+new"}},
			},
		},
	})
	require.ErrorIs(t, err, updateErr)
}

func TestSessionUsageHelpers(t *testing.T) {
	t.Parallel()

	require.Empty(t, resultOriginKind(nil))
	require.Nil(t, mergeUsage(nil, nil))
	require.Nil(t, cloneUsage(nil))
	require.Nil(t, optionalIntSum(nil, nil))
	require.NoError(t, promptResultError(nil, ""))
	require.True(t, modelHasLargeContext("claude-opus-4-6-1m"))
	require.True(t, modelHasLargeContext("opus[1m]"))
	require.False(t, modelHasLargeContext("claude-sonnet-4-6"))
	require.Equal(t, largeContextWindow, contextWindowForModel("opus[1m]"))
	require.False(t, fatalClaudeProcessError(nil))
	require.True(t, fatalClaudeProcessError(claude.ErrClientNotStarted))
	require.Equal(t, "", firstPromptToken("   "))
	require.Equal(t, "hello", firstPromptToken("hello world"))

	state := &promptLoopState{lastAssistantErrorKind: "old", lastAssistantModel: "old-model"}
	observeAssistantMessage(&claude.AssistantMessage{
		ParentToolUseID: "parent",
		ErrorKind:       "new",
		Model:           "new-model",
	}, state)
	require.Equal(t, "old", state.lastAssistantErrorKind)
	require.Equal(t, "old-model", state.lastAssistantModel)

	cachedRead := 2
	cachedWrite := 3
	thought := 4
	total := &acp.Usage{
		InputTokens:       1,
		OutputTokens:      2,
		TotalTokens:       10,
		CachedReadTokens:  &cachedRead,
		CachedWriteTokens: &cachedWrite,
		ThoughtTokens:     &thought,
	}
	cloned := cloneUsage(total)
	require.NotSame(t, total.CachedReadTokens, cloned.CachedReadTokens)
	require.Equal(t, 2, *cloned.CachedReadTokens)

	nextCachedRead := 5
	merged := mergeUsage(total, &acp.Usage{
		InputTokens:      7,
		OutputTokens:     8,
		TotalTokens:      20,
		CachedReadTokens: &nextCachedRead,
	})
	require.Equal(t, 8, merged.InputTokens)
	require.Equal(t, 10, merged.OutputTokens)
	require.Equal(t, 30, merged.TotalTokens)
	require.Equal(t, 7, *merged.CachedReadTokens)
	require.Equal(t, 3, *merged.CachedWriteTokens)
	require.Equal(t, 4, *merged.ThoughtTokens)

	session := &Session{model: "claude-sonnet-4-6"}
	require.Equal(t, defaultContextWindow, session.currentContextWindow())
	session.setContextWindowSize(123)
	require.Equal(t, 123, session.currentContextWindow())
	require.Equal(t, largeContextWindow, (&Session{}).liveContextWindow("opus[1m]"))

	updates, next, known, usedTotal := session.streamUsageUpdates(&claude.StreamEventMessage{
		EventType: streamEventMessageStart,
		Event:     map[string]any{"message": map[string]any{}},
	}, usageSnapshot{}, false, 0)
	require.Nil(t, updates)
	require.False(t, known)
	require.Zero(t, usedTotal)
	require.Equal(t, usageSnapshot{}, next)

	updates, next, known, usedTotal = session.streamUsageUpdates(&claude.StreamEventMessage{
		EventType: streamEventMessageDelta,
		Event:     map[string]any{"usage": map[string]any{"input_tokens": 1}},
	}, usageSnapshot{}, false, 0)
	require.Len(t, updates, 1)
	require.True(t, known)
	require.Equal(t, 1, usedTotal)
	require.Equal(t, 1, next.inputTokens)

	updates, next, known, usedTotal = session.streamUsageUpdates(&claude.StreamEventMessage{
		EventType: streamEventMessageDelta,
		Event:     map[string]any{"usage": map[string]any{"input_tokens": 1}},
	}, next, true, 1)
	require.Nil(t, updates)
	require.True(t, known)
	require.Equal(t, 1, usedTotal)
	next, known = streamUsageSnapshot(&claude.StreamEventMessage{
		EventType: streamEventMessageDelta,
		Event:     map[string]any{},
	}, next, true)
	require.False(t, known)
	next, known = streamUsageSnapshot(&claude.StreamEventMessage{EventType: "other"}, next, true)
	require.False(t, known)

	snapshot := usageSnapshot{}.patch(map[string]any{
		"input_tokens":                1,
		"output_tokens":               int64(2),
		"cache_read_input_tokens":     float64(3),
		"cache_creation_input_tokens": 5,
		"reasoning_output_tokens":     6,
	})
	require.Equal(t, 17, snapshot.total())

	require.Nil(t, session.resultUsageUpdates(nil, nil, ""))
	require.Equal(t, 9, session.resultUsageUpdates(&claude.ResultMessage{}, &claude.ContextUsage{TotalTokens: 9}, "")[0].UsageUpdate.Used)
	structuredUpdates := session.resultUsageUpdates(&claude.ResultMessage{
		Origin:           map[string]any{"kind": "task-notification"},
		StructuredOutput: map[string]any{"ok": true},
	}, nil, "")
	require.Len(t, structuredUpdates, 1)
	require.Equal(t, map[string]any{
		rawMessageOriginKey:     map[string]any{"kind": "task-notification"},
		structuredOutputMetaKey: map[string]any{"ok": true},
	}, structuredUpdates[0].UsageUpdate.Meta[claudeMetaKey])
	usageUpdates := session.resultUsageUpdates(&claude.ResultMessage{
		Usage: &claude.Usage{
			InputTokens:              1,
			OutputTokens:             2,
			CachedInputTokens:        3,
			CacheCreationInputTokens: 4,
			ReasoningOutputTokens:    5,
		},
		ModelUsage: map[string]claude.ModelUsage{
			"claude-haiku": {
				InputTokens:              6,
				OutputTokens:             7,
				CacheReadInputTokens:     8,
				CacheCreationInputTokens: 9,
				ContextWindow:            10,
			},
		},
	}, nil, "claude-haiku")
	require.Len(t, usageUpdates, 1)
	require.Equal(t, map[string]any{
		usageMetaKey: map[string]any{
			"inputTokens":       1,
			"outputTokens":      2,
			"cachedReadTokens":  3,
			"cachedWriteTokens": 4,
			"thoughtTokens":     5,
			"totalTokens":       15,
		},
		"modelUsage": map[string]any{
			"claude-haiku": map[string]any{
				"inputTokens":       6,
				"outputTokens":      7,
				"cachedReadTokens":  8,
				"cachedWriteTokens": 9,
				"contextWindow":     10,
			},
		},
	}, usageUpdates[0].UsageUpdate.Meta[claudeMetaKey])
	require.Nil(t, session.contextUsageUpdates(nil))
	require.Nil(t, session.contextUsageUpdates(&claude.ContextUsage{TotalTokens: 9}))
	contextUpdates := session.contextUsageUpdates(&claude.ContextUsage{TotalTokens: 9, MaxTokens: 1000})
	require.Len(t, contextUpdates, 1)
	require.Equal(t, 9, contextUpdates[0].UsageUpdate.Used)
	require.Equal(t, 1000, contextUpdates[0].UsageUpdate.Size)
	require.Equal(t, 1000, session.updateContextWindow(&claude.ResultMessage{}, &claude.ContextUsage{MaxTokens: 1000}, ""))
	require.Equal(t, 1000, session.currentContextWindow())
	usage, ok := matchingModelUsage(map[string]claude.ModelUsage{
		"claude-sonnet": {ContextWindow: 1},
	}, "claude-sonnet-4-6")
	require.True(t, ok)
	require.Equal(t, 1, usage.ContextWindow)
	require.Equal(t, 2, commonPrefixLength("abc", "abd"))
	require.Equal(t, 3, commonPrefixLength("abc", "abcde"))
	require.Equal(t, 0, intValue("bad"))
	_, ok = intField(nil, "x")
	require.False(t, ok)
}

func TestSessionMessageSideEffectsNoops(t *testing.T) {
	t.Parallel()

	session := &Session{}
	require.NoError(t, session.emitMessageSideEffects(context.Background(), &claude.SystemMessage{
		Subtype: systemStatus,
		Raw:     map[string]any{systemStatus: "running"},
	}))
	require.NoError(t, session.emitMessageSideEffects(context.Background(), &claude.SystemMessage{
		Subtype: systemSubtypeLocalCommandOutput,
		Raw:     map[string]any{},
	}))
}

func TestSessionPermissionDirectBranches(t *testing.T) {
	t.Parallel()

	session := &Session{
		agent: NewAgent(WithClaudeHome(t.TempDir())),
		id:    "session-1",
		permissionRules: map[string]string{
			"Allowed": claude.BehaviorAllow,
			"Denied":  claude.BehaviorDeny,
		},
	}

	decision, err := session.handlePermission(context.Background(), claude.PermissionRequest{
		ToolName: "Allowed",
		Input:    map[string]any{"file_path": "/tmp/a"},
	})
	require.NoError(t, err)
	require.Equal(t, claude.BehaviorAllow, decision.Behavior)
	require.Equal(t, map[string]any{"file_path": "/tmp/a"}, decision.UpdatedInput)

	decision, err = session.handlePermission(context.Background(), claude.PermissionRequest{ToolName: "Denied"})
	require.NoError(t, err)
	require.Equal(t, claude.BehaviorDeny, decision.Behavior)
	require.Contains(t, decision.Message, "saved ACP permission rule")

	decision, err = session.handlePermission(context.Background(), claude.PermissionRequest{ToolName: "Unknown"})
	require.NoError(t, err)
	require.Equal(t, claude.BehaviorDeny, decision.Behavior)
	require.Contains(t, decision.Message, "unavailable")

	session.setPermissionRule(context.Background(), "", claude.BehaviorAllow)
	require.NotContains(t, session.clonePermissionRules(), "")
}

func TestSessionCancelMarksActiveTurnAndInterruptsClaude(t *testing.T) {
	previousFallback := sessionCancelFallbackTimeout
	sessionCancelFallbackTimeout = 10 * time.Millisecond
	t.Cleanup(func() {
		sessionCancelFallbackTimeout = previousFallback
	})

	fake := newAgentFakeTransport()
	agent := NewAgent(WithClaudeHome(t.TempDir()))
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(nil, options, fake)
	}

	resp, err := agent.NewSession(context.Background(), acp.NewSessionRequest{Cwd: "/repo", McpServers: []acp.McpServer{}})
	require.NoError(t, err)

	session, err := agent.session(resp.SessionId)
	require.NoError(t, err)

	called := make(chan struct{})
	var calledOnce sync.Once
	turnDone := make(chan struct{})
	session.mu.Lock()
	session.cancel = func() {
		calledOnce.Do(func() { close(called) })
	}
	session.turnDone = turnDone
	permissionCancelled := false
	session.permissionCancel = map[string]*permissionRequestCancel{
		"tool-1": {cancel: func() { permissionCancelled = true }},
	}
	session.mu.Unlock()
	t.Cleanup(func() { close(turnDone) })

	require.NoError(t, session.Cancel(context.Background()))
	require.True(t, permissionCancelled)
	require.True(t, session.wasTurnCancelled())
	require.NotEmpty(t, sentControlRequests(fake, "interrupt"))

	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("turn context was not cancelled after interrupt fallback")
	}
}

func TestSessionPermissionClientOutcomes(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name       string
		option     acp.PermissionOptionId
		want       string
		message    string
		persisted  bool
		permission map[string]string
	}{
		{
			name:    "reject once",
			option:  permissionRejectOnce,
			want:    claude.BehaviorDeny,
			message: permissionRejectedMessage,
		},
		{
			name:       "reject always",
			option:     permissionRejectAlways,
			want:       claude.BehaviorDeny,
			message:    permissionRejectedMessage,
			persisted:  true,
			permission: map[string]string{"Read": claude.BehaviorDeny},
		},
		{
			name:    "unknown",
			option:  "unexpected",
			want:    claude.BehaviorDeny,
			message: "Unknown permission option",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			agent := NewAgent(WithClaudeHome(t.TempDir()))
			client := &recordingACPClient{permission: tc.option}
			_ = connectAgentForTest(t, agent, client)

			session := &Session{
				agent:           agent,
				id:              "session-1",
				permissionRules: map[string]string{},
			}

			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()

			decision, err := session.handlePermission(ctx, claude.PermissionRequest{
				ToolName:  "Read",
				ToolUseID: "tool-1",
				Input:     map[string]any{"file_path": "/tmp/a"},
				Raw:       map[string]any{acpFieldRaw: true},
			})
			require.NoError(t, err)
			require.Equal(t, tc.want, decision.Behavior)
			require.Contains(t, decision.Message, tc.message)

			if tc.persisted {
				require.Equal(t, tc.permission, session.clonePermissionRules())
				require.NotEmpty(t, decision.UpdatedPermissions)
			}
		})
	}
}

func TestSessionPermissionCancelledAndPersistErrors(t *testing.T) {
	t.Parallel()

	blockingHome := filepath.Join(t.TempDir(), "claude-home")
	require.NoError(t, os.WriteFile(blockingHome, []byte("file"), 0o600))

	agent := NewAgent(WithClaudeHome(blockingHome))
	client := &recordingACPClient{permissionCancelled: true}
	_ = connectAgentForTest(t, agent, client)

	session := &Session{
		agent: agent,
		id:    "session-1",
	}
	session.setPermissionRule(context.Background(), "Write", claude.BehaviorAllow)
	require.Equal(t, map[string]string{"Write": claude.BehaviorAllow}, session.clonePermissionRules())

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	decision, err := session.handlePermission(ctx, claude.PermissionRequest{
		ToolName: "Read",
		Input:    map[string]any{"file_path": "/tmp/a"},
	})
	require.NoError(t, err)
	require.Equal(t, claude.BehaviorDeny, decision.Behavior)
	require.Contains(t, decision.Message, "cancelled")

	requests := client.recordedPermissions()
	require.Len(t, requests, 1)
	require.Equal(t, acp.ToolCallId("Read"), requests[0].ToolCall.ToolCallId)
}

func TestSessionPermissionRequestContextCancelledWithTurn(t *testing.T) {
	t.Parallel()

	started := make(chan struct{}, 1)
	block := make(chan struct{})
	client := &recordingACPClient{
		permissionStarted: started,
		permissionBlock:   block,
	}
	agent := NewAgent(WithClaudeHome(t.TempDir()))
	_ = connectAgentForTest(t, agent, client)
	fake := newAgentFakeTransport()
	claudeClient := claude.NewClient(nil, claude.Options{}, fake)
	require.NoError(t, claudeClient.Start(context.Background()))
	t.Cleanup(func() { require.NoError(t, claudeClient.Close()) })

	session := &Session{
		agent:           agent,
		id:              "session-1",
		client:          claudeClient,
		permissionRules: map[string]string{},
	}

	type permissionResult struct {
		decision claude.PermissionDecision
		err      error
	}
	done := make(chan permissionResult, 1)

	go func() {
		decision, err := session.handlePermission(context.Background(), claude.PermissionRequest{
			ToolName:  "Write",
			ToolUseID: "tool-1",
			Input:     map[string]any{"file_path": "/tmp/a"},
		})
		done <- permissionResult{decision: decision, err: err}
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for permission request")
	}

	require.NoError(t, session.Cancel(context.Background()))

	select {
	case result := <-done:
		require.NoError(t, result.err)
		require.Equal(t, claude.BehaviorDeny, result.decision.Behavior)
		require.True(t, result.decision.Interrupt)
		require.Contains(t, result.decision.Message, "cancelled")
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for permission cancellation")
	}
}

func TestSessionPermissionRequestContextBranches(t *testing.T) {
	t.Parallel()

	session := &Session{}

	ctx, finish := session.permissionRequestContext(context.Background(), "")
	require.NoError(t, ctx.Err())
	finish()

	session.mu.Lock()
	session.turnCancelled = true
	session.mu.Unlock()

	ctx, finish = session.permissionRequestContext(context.Background(), "tool-1")
	require.ErrorIs(t, ctx.Err(), context.Canceled)
	finish()

	session.mu.Lock()
	require.Empty(t, session.permissionCancel)
	session.mu.Unlock()

	require.True(t, permissionRequestCancelled(context.Canceled))
	require.False(t, permissionRequestCancelled(errors.New("other")))
	require.False(t, permissionRequestCancelled(acp.NewInternalError(nil)))
}

func TestSessionSetPermissionRulePersistsInMutationOrder(t *testing.T) {
	oldSavePermissionRules := savePermissionRules
	firstEntered := make(chan struct{})
	releaseFirst := make(chan struct{})
	secondEntered := make(chan struct{})

	var (
		mu        sync.Mutex
		snapshots []map[string]string
	)

	savePermissionRules = func(_ context.Context, _ string, _ acp.SessionId, rules map[string]string) error {
		mu.Lock()
		snapshot := make(map[string]string)
		maps.Copy(snapshot, rules)
		snapshots = append(snapshots, snapshot)
		call := len(snapshots)
		mu.Unlock()

		if call == 1 {
			close(firstEntered)
			<-releaseFirst
		} else {
			close(secondEntered)
		}

		return nil
	}
	t.Cleanup(func() {
		savePermissionRules = oldSavePermissionRules
		select {
		case <-firstEntered:
			select {
			case <-releaseFirst:
			default:
				close(releaseFirst)
			}
		default:
		}
	})

	session := &Session{
		agent: NewAgent(WithClaudeHome(t.TempDir())),
		id:    "session-1",
	}

	firstDone := make(chan struct{})
	go func() {
		defer close(firstDone)
		session.setPermissionRule(context.Background(), "Read", claude.BehaviorAllow)
	}()

	<-firstEntered

	secondDone := make(chan struct{})
	go func() {
		defer close(secondDone)
		session.setPermissionRule(context.Background(), "Write", claude.BehaviorDeny)
	}()

	select {
	case <-secondEntered:
		t.Fatal("second permission save started before first save finished")
	case <-time.After(50 * time.Millisecond):
	}

	close(releaseFirst)
	<-firstDone
	<-secondDone

	mu.Lock()
	defer mu.Unlock()
	require.Equal(t, []map[string]string{
		{"Read": claude.BehaviorAllow},
		{"Read": claude.BehaviorAllow, "Write": claude.BehaviorDeny},
	}, snapshots)
}

func TestSessionExitPlanModePermissionAllow(t *testing.T) {
	t.Parallel()

	agent := NewAgent(WithClaudeHome(t.TempDir()))
	client := &recordingACPClient{permission: acp.PermissionOptionId(modeAuto)}
	_ = connectAgentForTest(t, agent, client)

	session := &Session{
		agent: agent,
		id:    "session-1",
		cwd:   "/repo",
		model: "claude-opus-4-6",
		mode:  modePlan,
		availableModels: []claude.AvailableModelInfo{
			{Value: "claude-opus-4-6", SupportsAutoMode: true},
		},
	}

	decision, err := session.handlePermission(context.Background(), claude.PermissionRequest{
		ToolName:  exitPlanModeTool,
		ToolUseID: "tool-1",
		Title:     "Review plan",
		Input:     map[string]any{"plan": "Implement it"},
		Raw:       map[string]any{"raw": true},
	})
	require.NoError(t, err)
	require.Equal(t, claude.BehaviorAllow, decision.Behavior)
	require.Equal(t, []map[string]any{permissionModeUpdate(modeAuto)}, decision.UpdatedPermissions)

	requests := client.recordedPermissions()
	require.Len(t, requests, 1)
	require.Equal(t, "Review plan", *requests[0].ToolCall.Title)
	require.Contains(t, requests[0].Options, acp.PermissionOption{
		OptionId: acp.PermissionOptionId(modeAuto),
		Name:     `Yes, and use "auto" mode`,
		Kind:     acp.PermissionOptionKindAllowAlways,
	})
	require.Equal(t, acp.ToolKindSwitchMode, *requests[0].ToolCall.Kind)
	require.Equal(t, "Implement it", requests[0].ToolCall.Content[0].Content.Content.Text.Text)

	require.Eventually(t, func() bool {
		return len(client.recordedUpdates()) == 1
	}, time.Second, 10*time.Millisecond)

	updates := client.recordedUpdates()
	require.Len(t, updates, 1)
	require.Equal(t, modeAuto, updates[0].Update.CurrentModeUpdate.CurrentModeId)
}

func TestSessionExitPlanModePermissionDenyBranches(t *testing.T) {
	t.Parallel()

	session := &Session{agent: NewAgent(WithClaudeHome(t.TempDir())), id: "session-1"}
	decision, err := session.handlePermission(context.Background(), claude.PermissionRequest{ToolName: exitPlanModeTool})
	require.NoError(t, err)
	require.Equal(t, claude.BehaviorDeny, decision.Behavior)
	require.Contains(t, decision.Message, "unavailable")

	for _, tc := range []struct {
		name       string
		permission acp.PermissionOptionId
		cancelled  bool
		message    string
	}{
		{name: "cancelled", cancelled: true, message: "cancelled"},
		{name: "keep planning", permission: acp.PermissionOptionId(modePlan), message: "rejected"},
		{name: "unavailable auto", permission: acp.PermissionOptionId(modeAuto), message: "rejected"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			agent := NewAgent(WithClaudeHome(t.TempDir()))
			client := &recordingACPClient{permission: tc.permission, permissionCancelled: tc.cancelled}
			_ = connectAgentForTest(t, agent, client)

			session := &Session{
				agent: agent,
				id:    "session-1",
				model: "claude-haiku-4-5",
				availableModels: []claude.AvailableModelInfo{
					{Value: "claude-haiku-4-5"},
				},
			}

			decision, err := session.handlePermission(context.Background(), claude.PermissionRequest{
				ToolName: exitPlanModeTool,
				Input:    map[string]any{"plan": "Implement it"},
			})
			require.NoError(t, err)
			require.Equal(t, claude.BehaviorDeny, decision.Behavior)
			require.Contains(t, decision.Message, tc.message)
		})
	}
}

func TestSessionExitPlanModePermissionErrors(t *testing.T) {
	t.Parallel()

	agent := NewAgent(WithClaudeHome(t.TempDir()))
	client := &recordingACPClient{permission: acp.PermissionOptionId(modeDefault), permissionErr: errors.New("permission failed")}
	_ = connectAgentForTest(t, agent, client)

	session := &Session{agent: agent, id: "session-1"}
	_, err := session.handlePermission(context.Background(), claude.PermissionRequest{ToolName: exitPlanModeTool})
	require.Error(t, err)

	client.setPermissionErr(nil)
	attachFailingConnection(agent)
	_, err = session.handlePermission(context.Background(), claude.PermissionRequest{ToolName: exitPlanModeTool})
	require.Error(t, err)

	updateAgent := NewAgent(WithClaudeHome(t.TempDir()))
	updateAgent.setConnection(&stubAgentClient{
		permission: acp.PermissionOptionId(modeDefault),
		updateErr:  errors.New("update failed"),
	})
	updateSession := &Session{agent: updateAgent, id: "session-1"}
	_, err = updateSession.handlePermission(context.Background(), claude.PermissionRequest{ToolName: exitPlanModeTool})
	require.Error(t, err)
}

func TestSessionPersistPermissionRulesLogsSaveError(t *testing.T) {
	oldSavePermissionRules := savePermissionRules
	t.Cleanup(func() {
		savePermissionRules = oldSavePermissionRules
	})

	savePermissionRules = func(context.Context, string, acp.SessionId, map[string]string) error {
		return errors.New("save failed")
	}

	agent := NewAgent(WithClaudeHome(t.TempDir()))
	session := &Session{
		agent:           agent,
		id:              "session-1",
		permissionRules: map[string]string{"Read": claude.BehaviorAllow},
	}
	session.persistPermissionRules(context.Background())

	_, ok := agent.cachedPermissionRules("session-1")
	require.False(t, ok)
}

func TestSessionElicitationDirectBranches(t *testing.T) {
	t.Parallel()

	session := &Session{
		agent: NewAgent(WithClaudeHome(t.TempDir())),
		id:    "session-1",
	}

	for _, request := range []claude.ElicitationRequest{
		{Mode: claude.ElicitationModeForm},
		{Mode: claude.ElicitationModeURL, URL: "https://example.com"},
		{Mode: "future"},
	} {
		response, err := session.handleElicitation(context.Background(), request)
		require.NoError(t, err)
		require.Equal(t, claude.ElicitationActionDecline, response.Action)
	}

	require.Equal(t, claude.ElicitationActionCancel, claudeElicitationResponse(
		acp.UnstableCreateElicitationResponse{Cancel: &acp.UnstableCreateElicitationCancel{}},
	).Action)
	require.Equal(t, claude.ElicitationActionDecline, claudeElicitationResponse(acp.UnstableCreateElicitationResponse{}).Action)
	require.Equal(t, acp.UnstableElicitationSchemaTypeObject, elicitationSchema(nil).Type)
}

func TestSessionElicitationCapabilityAndErrorBranches(t *testing.T) {
	t.Parallel()

	agent := NewAgent(WithClaudeHome(t.TempDir()))
	client := &recordingACPClient{}
	_ = connectAgentForTest(t, agent, client)
	session := &Session{agent: agent, id: "session-1"}

	agent.clientCapabilities = acp.ClientCapabilities{
		Elicitation: &acp.ElicitationCapabilities{Url: &acp.ElicitationUrlCapabilities{}},
	}
	response, err := session.handleElicitation(context.Background(), claude.ElicitationRequest{Mode: claude.ElicitationModeForm})
	require.NoError(t, err)
	require.Equal(t, claude.ElicitationActionDecline, response.Action)

	agent.clientCapabilities = acp.ClientCapabilities{
		Elicitation: &acp.ElicitationCapabilities{Form: &acp.ElicitationFormCapabilities{}},
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = session.handleElicitation(canceled, claude.ElicitationRequest{Mode: claude.ElicitationModeForm})
	var reqErr *acp.RequestError
	require.ErrorAs(t, err, &reqErr)
	require.Equal(t, -32800, reqErr.Code)

	for _, request := range []claude.ElicitationRequest{
		{Mode: claude.ElicitationModeURL, URL: "https://example.com"},
		{Mode: claude.ElicitationModeURL},
		{Mode: "future"},
	} {
		response, err = session.handleElicitation(context.Background(), request)
		require.NoError(t, err)
		require.Equal(t, claude.ElicitationActionDecline, response.Action)
	}

	agent.clientCapabilities = acp.ClientCapabilities{
		Elicitation: &acp.ElicitationCapabilities{Url: &acp.ElicitationUrlCapabilities{}},
	}
	client.setElicitationErr(errors.New("elicitation failed"))
	_, err = session.handleElicitation(context.Background(), claude.ElicitationRequest{
		Mode:          claude.ElicitationModeURL,
		ElicitationID: "existing-id",
		URL:           "https://example.com",
	})
	require.Error(t, err)
}

func TestSessionURLElicitationUUIDError(t *testing.T) {
	random := uuidRandom
	uuidRandom = errReader{err: errors.New("random failed")}
	t.Cleanup(func() {
		uuidRandom = random
	})

	agent := NewAgent(WithClaudeHome(t.TempDir()))
	agent.clientCapabilities = acp.ClientCapabilities{
		Elicitation: &acp.ElicitationCapabilities{Url: &acp.ElicitationUrlCapabilities{}},
	}
	_ = connectAgentForTest(t, agent, &recordingACPClient{})

	session := &Session{agent: agent, id: "session-1"}
	_, err := session.handleElicitation(context.Background(), claude.ElicitationRequest{
		Mode: claude.ElicitationModeURL,
		URL:  "https://example.com",
	})
	require.Error(t, err)
}

func TestSessionAskUserQuestionDirectBranches(t *testing.T) {
	t.Parallel()

	agent := NewAgent(WithClaudeHome(t.TempDir()))
	agent.clientCapabilities = acp.ClientCapabilities{
		Elicitation: &acp.ElicitationCapabilities{Form: &acp.ElicitationFormCapabilities{}},
	}
	client := &recordingACPClient{}
	_ = connectAgentForTest(t, agent, client)

	session := &Session{agent: agent, id: "session-1"}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	decision, err := session.handleAskUserQuestion(ctx, claude.PermissionRequest{Input: nil})
	require.NoError(t, err)
	require.Equal(t, claude.BehaviorDeny, decision.Behavior)
	require.Contains(t, decision.Message, "parse error")

	decision, err = session.handleAskUserQuestion(ctx, claude.PermissionRequest{Input: map[string]any{}})
	require.NoError(t, err)
	require.Equal(t, claude.BehaviorDeny, decision.Behavior)
	require.Contains(t, decision.Message, "missing questions")

	client.setElicitationResponse(acp.UnstableCreateElicitationResponse{
		Decline: &acp.UnstableCreateElicitationDecline{},
	})
	decision, err = session.handleAskUserQuestion(ctx, claude.PermissionRequest{
		ToolUseID: "tool-1",
		Input: map[string]any{
			askFieldQuestions: []any{map[string]any{askFieldQuestion: "Pick one"}},
		},
	})
	require.NoError(t, err)
	require.Equal(t, claude.BehaviorDeny, decision.Behavior)
	require.Contains(t, decision.Message, "declined")

	client.setElicitationResponse(acp.UnstableCreateElicitationResponse{
		Accept: &acp.UnstableCreateElicitationAccept{Content: map[string]any{"missing": "answer"}},
	})
	decision, err = session.handleAskUserQuestion(ctx, claude.PermissionRequest{
		Input: map[string]any{
			askFieldQuestions: []any{map[string]any{askFieldID: "q1", askFieldQuestion: "Pick one"}},
		},
	})
	require.NoError(t, err)
	require.Equal(t, claude.BehaviorDeny, decision.Behavior)
	require.Contains(t, decision.Message, "invalid response")

	client.setElicitationErr(errors.New("elicitation failed"))
	_, err = session.handleAskUserQuestion(ctx, claude.PermissionRequest{
		Input: map[string]any{
			askFieldQuestions: []any{map[string]any{askFieldQuestion: "Pick one"}},
		},
	})
	require.Error(t, err)

	decision, err = session.handleAskUserQuestion(ctx, claude.PermissionRequest{
		Input: map[string]any{
			askFieldQuestions: []any{"bad"},
		},
	})
	require.NoError(t, err)
	require.Equal(t, claude.BehaviorDeny, decision.Behavior)
	require.Contains(t, decision.Message, "no parseable")

	require.Nil(t, answerStrings(""))
}

func TestSessionURLElicitationDirectBranches(t *testing.T) {
	t.Parallel()

	agent := NewAgent(WithClaudeHome(t.TempDir()))
	agent.clientCapabilities = acp.ClientCapabilities{
		Elicitation: &acp.ElicitationCapabilities{Url: &acp.ElicitationUrlCapabilities{}},
	}
	client := &recordingACPClient{
		elicitationResponse: acp.UnstableCreateElicitationResponse{
			Accept: &acp.UnstableCreateElicitationAccept{Content: map[string]any{"ok": true}},
		},
	}
	_ = connectAgentForTest(t, agent, client)

	session := &Session{agent: agent, id: "session-1"}

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	response, err := session.handleElicitation(ctx, claude.ElicitationRequest{
		Mode:          claude.ElicitationModeURL,
		ElicitationID: "existing-id",
		Message:       "Authenticate",
		URL:           "https://example.com/auth",
	})
	require.NoError(t, err)
	require.Equal(t, claude.ElicitationActionAccept, response.Action)

	require.Eventually(t, func() bool {
		requests := client.recordedElicitations()

		return len(requests) == 1 && requests[0].Url != nil && requests[0].Url.ElicitationId == "existing-id"
	}, time.Second, 10*time.Millisecond)
}

func TestSessionMessageSideEffectNoops(t *testing.T) {
	t.Parallel()

	session := &Session{
		agent: NewAgent(WithClaudeHome(t.TempDir())),
		id:    "session-1",
	}

	require.NoError(t, session.emitMessageSideEffects(context.Background(), &claude.AssistantMessage{}))
	require.NoError(t, session.emitMessageSideEffects(context.Background(), &claude.SystemMessage{Subtype: "other"}))
	require.NoError(t, session.emitMessageSideEffects(context.Background(), &claude.SystemMessage{Subtype: elicitationComplete}))
	require.NoError(t, session.emitMessageSideEffects(context.Background(), &claude.SystemMessage{
		Subtype: elicitationComplete,
		Raw:     map[string]any{"elicitation_id": ""},
	}))
	require.Empty(t, elicitationIDFromSystem(&claude.SystemMessage{}))

	agent := NewAgent(WithClaudeHome(t.TempDir()))
	agent.clientCapabilities = acp.ClientCapabilities{
		Elicitation: &acp.ElicitationCapabilities{Url: &acp.ElicitationUrlCapabilities{}},
	}
	_ = connectAgentForTest(t, agent, &recordingACPClient{})
	session = &Session{agent: agent, id: "session-1"}
	require.NoError(t, session.emitMessageSideEffects(context.Background(), &claude.SystemMessage{
		Subtype: elicitationComplete,
	}))

	agent.setConnection(nil)
	require.NoError(t, session.emitMessageSideEffects(context.Background(), &claude.SystemMessage{
		Subtype: elicitationComplete,
		Raw:     map[string]any{"elicitation_id": "from-raw"},
	}))
}
