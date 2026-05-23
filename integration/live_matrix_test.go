//go:build integration

package integration

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	claudeacp "github.com/savid/acp-go-claude"
	"github.com/stretchr/testify/require"
)

const (
	helperMCPStdioEnv       = "ACP_GO_CLAUDE_MCP_STDIO_HELPER"
	helperMCPModeEnv        = "ACP_GO_CLAUDE_MCP_STDIO_MODE"
	helperMCPProxyEnv       = "ACP_GO_CLAUDE_MCP_PROXY_HELPER"
	helperMCPProxyBadIDEnv  = "ACP_GO_CLAUDE_MCP_PROXY_BAD_ACP_ID"
	helperMCPProxyMarkerEnv = "ACP_GO_CLAUDE_MCP_PROXY_MARKER"
)

func TestClaudeCLIAuthGatewayInitialization(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	client := &recordingClient{permission: acp.PermissionOptionId("default")}
	conn, initialize := initializeLiveAgentForTest(t, ctx, client, acp.InitializeRequest{
		ClientCapabilities: acp.ClientCapabilities{
			Auth: acp.AuthCapabilities{
				Meta: map[string]any{"gateway": true},
			},
		},
	})

	require.Contains(t, authAgentMethodIDs(initialize.AuthMethods), "gateway")

	_, err := conn.Authenticate(ctx, acp.AuthenticateRequest{
		MethodId: "gateway",
		Meta: map[string]any{
			"gateway": map[string]any{
				"baseUrl": "https://gateway.example",
				"headers": map[string]any{
					"x-api-key": "test",
				},
			},
		},
	})
	require.NoError(t, err)

	_, err = conn.UnstableLogout(ctx, acp.UnstableLogoutRequest{})
	require.NoError(t, err)
}

func TestClaudeCLIGatewayAuthClosesSessionsAndRejectsProcessMCP(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	client := &recordingClient{}
	conn, _ := initializeLiveAgentForTest(t, ctx, client, acp.InitializeRequest{
		ClientCapabilities: acp.ClientCapabilities{
			Auth: acp.AuthCapabilities{
				Meta: map[string]any{"gateway": true},
			},
		},
	})

	_, err := conn.Authenticate(ctx, acp.AuthenticateRequest{
		MethodId: "gateway",
		Meta: map[string]any{
			"gateway": map[string]any{
				"baseUrl": "https://gateway.example",
				"headers": map[string]any{"x-api-key": "test"},
			},
		},
	})
	require.NoError(t, err)

	_, err = conn.NewSession(ctx, acp.NewSessionRequest{
		Cwd: t.TempDir(),
		McpServers: []acp.McpServer{
			{
				Stdio: &acp.McpServerStdio{
					Name:    "blocked_stdio",
					Command: os.Args[0],
					Args:    []string{"-test.run=TestIntegrationMCPStdioHelper"},
					Env:     []acp.EnvVariable{{Name: helperMCPStdioEnv, Value: "1"}},
				},
			},
		},
	})
	require.Error(t, err)

	session, err := conn.NewSession(ctx, acp.NewSessionRequest{Cwd: t.TempDir(), McpServers: []acp.McpServer{}})
	require.NoError(t, err)

	_, err = conn.UnstableLogout(ctx, acp.UnstableLogoutRequest{})
	require.NoError(t, err)

	_, err = conn.Prompt(ctx, acp.PromptRequest{
		SessionId: session.SessionId,
		Prompt:    []acp.ContentBlock{acp.TextBlock("Reply exactly ACP_GATEWAY_AFTER_LOGOUT.")},
	})
	require.Error(t, err)
}

func authAgentMethodIDs(methods []acp.AuthMethod) []string {
	ids := make([]string, 0, len(methods))
	for _, method := range methods {
		if method.Agent != nil {
			ids = append(ids, method.Agent.Id)
		}
	}

	return ids
}

func sessionModeAvailable(modes *acp.SessionModeState, mode acp.SessionModeId) bool {
	if modes == nil {
		return false
	}

	for _, available := range modes.AvailableModes {
		if available.Id == mode {
			return true
		}
	}

	return false
}

func TestClaudeCLIPermissionAllowAlwaysAndTranscriptToolReplay(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	cwd := t.TempDir()
	client := &recordingClient{permission: acp.PermissionOptionId("allow_always")}
	conn := connectLiveAgent(t, ctx, client, acp.InitializeRequest{})

	session, err := conn.NewSession(ctx, acp.NewSessionRequest{Cwd: cwd, McpServers: []acp.McpServer{}})
	require.NoError(t, err)

	resp, err := conn.Prompt(ctx, acp.PromptRequest{
		SessionId: session.SessionId,
		Prompt: []acp.ContentBlock{acp.TextBlock(
			"You must use the Write tool exactly once to create permission_one.txt containing ACP_PERMISSION_ONE. " +
				"After the command finishes, reply exactly ACP_PERMISSION_ONE_DONE.",
		)},
	})
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, resp.StopReason)
	require.Eventually(t, func() bool { return client.permissionCount() >= 1 }, 20*time.Second, 250*time.Millisecond)
	require.Contains(t, client.text(), "ACP_PERMISSION_ONE_DONE")
	require.Eventually(t, func() bool {
		return toolLocationContains(client.updateSnapshot(), "permission_one.txt")
	}, 30*time.Second, 250*time.Millisecond)

	firstPermissionCount := client.permissionCount()
	client.resetRecordedOutput()

	resp, err = conn.Prompt(ctx, acp.PromptRequest{
		SessionId: session.SessionId,
		Prompt: []acp.ContentBlock{acp.TextBlock(
			"You must use the Write tool exactly once to create permission_two.txt containing ACP_PERMISSION_TWO. " +
				"After the command finishes, reply exactly ACP_PERMISSION_TWO_DONE.",
		)},
	})
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, resp.StopReason)
	require.Equal(t, firstPermissionCount, client.permissionCount())
	require.Contains(t, client.text(), "ACP_PERMISSION_TWO_DONE")

	_, err = conn.CloseSession(ctx, acp.CloseSessionRequest{SessionId: session.SessionId})
	require.NoError(t, err)

	client.resetRecordedOutput()
	_, err = conn.LoadSession(ctx, acp.LoadSessionRequest{
		SessionId:  session.SessionId,
		Cwd:        cwd,
		McpServers: []acp.McpServer{},
	})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		for _, update := range client.updateSnapshot() {
			if update.ToolCall != nil || update.ToolCallUpdate != nil {
				return true
			}
		}

		return false
	}, 30*time.Second, 500*time.Millisecond)

	_, err = conn.CloseSession(ctx, acp.CloseSessionRequest{SessionId: session.SessionId})
	require.NoError(t, err)
}

func TestClaudeCLICancelPendingPermissionRequest(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	cwd := t.TempDir()
	client := newBlockingPermissionClient()
	conn := connectLiveAgent(t, ctx, client, acp.InitializeRequest{})

	session, err := conn.NewSession(ctx, acp.NewSessionRequest{Cwd: cwd, McpServers: []acp.McpServer{}})
	require.NoError(t, err)

	respCh := make(chan acp.PromptResponse, 1)
	errCh := make(chan error, 1)
	go func() {
		resp, promptErr := conn.Prompt(ctx, acp.PromptRequest{
			SessionId: session.SessionId,
			Prompt: []acp.ContentBlock{acp.TextBlock(
				"You must use the Write tool exactly once to create permission_cancel.txt containing ACP_PERMISSION_CANCEL. " +
					"Do not use any other tool.",
			)},
		})
		if promptErr != nil {
			errCh <- promptErr

			return
		}

		respCh <- resp
	}()

	select {
	case <-client.permissionRequested:
	case err := <-errCh:
		require.NoError(t, err)
		t.Fatal("prompt returned before requesting permission")
	case resp := <-respCh:
		require.Equal(t, acp.StopReasonCancelled, resp.StopReason)
		t.Fatal("prompt returned before requesting permission")
	case <-ctx.Done():
		require.NoError(t, ctx.Err())
	}

	require.NoError(t, conn.Cancel(ctx, acp.CancelNotification{SessionId: session.SessionId}))

	select {
	case returned := <-client.permissionReturned:
		require.NotNil(t, returned.Outcome.Cancelled)
	case <-ctx.Done():
		require.NoError(t, ctx.Err())
	}

	select {
	case err := <-errCh:
		require.NoError(t, err)
	case resp := <-respCh:
		require.Equal(t, acp.StopReasonCancelled, resp.StopReason)
	case <-ctx.Done():
		require.NoError(t, ctx.Err())
	}

	_, err = conn.CloseSession(ctx, acp.CloseSessionRequest{SessionId: session.SessionId})
	require.NoError(t, err)
}

func TestClaudeCLIAskUserQuestionElicitation(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	client := &recordingClient{}
	conn := connectLiveAgent(t, ctx, client, acp.InitializeRequest{
		ClientCapabilities: acp.ClientCapabilities{
			Elicitation: &acp.ElicitationCapabilities{Form: &acp.ElicitationFormCapabilities{}},
		},
	}, claudeacp.WithDefaultPermissionMode("plan"))

	session, err := conn.NewSession(ctx, acp.NewSessionRequest{Cwd: t.TempDir(), McpServers: []acp.McpServer{}})
	require.NoError(t, err)

	resp, err := conn.Prompt(ctx, acp.PromptRequest{
		SessionId: session.SessionId,
		Prompt: []acp.ContentBlock{acp.TextBlock(
			"You must call AskUserQuestion exactly once to ask me to choose between Go and Rust. " +
				"Do not answer from prior knowledge, do not skip the tool call, and only after the tool returns, reply exactly ACP_ASK_USER_QUESTION_DONE.",
		)},
	})
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, resp.StopReason)
	require.Eventually(t, func() bool { return client.elicitationCount() >= 1 }, 20*time.Second, 250*time.Millisecond)
	require.Contains(t, client.text(), "ACP_ASK_USER_QUESTION_DONE")
	resultUpdate := completedToolResultUpdate(client.updateSnapshot(), "Go")
	require.NotNil(t, resultUpdate, "live AskUserQuestion result should include the selected ACP elicitation answer")
	require.NotNil(t, resultUpdate.RawOutput)
	require.True(t, rawValueContains(resultUpdate.RawOutput, "Go"))

	_, err = conn.CloseSession(ctx, acp.CloseSessionRequest{SessionId: session.SessionId})
	require.NoError(t, err)
}

func TestClaudeCLIEnterPlanModeHookCallback(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	client := &recordingClient{}
	conn := connectLiveAgent(
		t,
		ctx,
		client,
		acp.InitializeRequest{},
		claudeacp.WithDefaultSystemPrompt(
			"When the user asks to enter plan mode, use the EnterPlanMode tool immediately.",
		),
	)

	session, err := conn.NewSession(ctx, acp.NewSessionRequest{Cwd: t.TempDir(), McpServers: []acp.McpServer{}})
	require.NoError(t, err)

	resp, err := conn.Prompt(ctx, acp.PromptRequest{
		SessionId: session.SessionId,
		Prompt: []acp.ContentBlock{acp.TextBlock(
			"Use the EnterPlanMode tool now. After the tool completes, reply exactly ACP_PLAN_HOOK_DONE.",
		)},
	})
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, resp.StopReason)
	require.Eventually(t, func() bool {
		for _, update := range client.updateSnapshot() {
			if update.CurrentModeUpdate != nil && update.CurrentModeUpdate.CurrentModeId == acp.SessionModeId("plan") {
				return true
			}
		}

		return false
	}, 30*time.Second, 250*time.Millisecond)

	_, err = conn.CloseSession(ctx, acp.CloseSessionRequest{SessionId: session.SessionId})
	require.NoError(t, err)
}

func toolLocationContains(updates []acp.SessionUpdate, path string) bool {
	for _, update := range updates {
		if update.ToolCall != nil {
			for _, location := range update.ToolCall.Locations {
				if strings.Contains(location.Path, path) {
					return true
				}
			}
		}

		if update.ToolCallUpdate != nil {
			for _, location := range update.ToolCallUpdate.Locations {
				if strings.Contains(location.Path, path) {
					return true
				}
			}
		}
	}

	return false
}

func TestClaudeCLICancelLongRunningToolAndContinuesSession(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	client := &recordingClient{permission: acp.PermissionOptionId("allow_once")}
	conn := connectLiveAgent(t, ctx, client, acp.InitializeRequest{})

	session, err := conn.NewSession(ctx, acp.NewSessionRequest{
		Cwd: t.TempDir(),
		McpServers: []acp.McpServer{
			{
				Stdio: &acp.McpServerStdio{
					Name:    "acp_slow",
					Command: os.Args[0],
					Args:    []string{"-test.run=TestIntegrationMCPStdioHelper"},
					Env: []acp.EnvVariable{
						{Name: helperMCPStdioEnv, Value: "1"},
						{Name: helperMCPModeEnv, Value: "slow"},
					},
				},
			},
		},
	})
	require.NoError(t, err)

	respCh := make(chan acp.PromptResponse, 1)
	errCh := make(chan error, 1)
	go func() {
		resp, promptErr := conn.Prompt(ctx, acp.PromptRequest{
			SessionId: session.SessionId,
			Prompt: []acp.ContentBlock{acp.TextBlock(
				"You must call the mcp__acp_slow__wait MCP tool exactly once. Do not use Bash.",
			)},
		})
		if promptErr != nil {
			errCh <- promptErr

			return
		}

		respCh <- resp
	}()

	require.Eventually(t, func() bool {
		for _, update := range client.updateSnapshot() {
			if update.ToolCall != nil {
				return true
			}
		}

		return false
	}, 30*time.Second, 250*time.Millisecond)
	require.NoError(t, conn.Cancel(ctx, acp.CancelNotification{SessionId: session.SessionId}))

	select {
	case err := <-errCh:
		require.NoError(t, err)
	case resp := <-respCh:
		require.Equal(t, acp.StopReasonCancelled, resp.StopReason)
	case <-ctx.Done():
		require.NoError(t, ctx.Err())
	}

	resp := promptWithRefusalRetry(t, func() (acp.PromptResponse, error) {
		client.resetRecordedOutput()

		return conn.Prompt(ctx, acp.PromptRequest{
			SessionId: session.SessionId,
			Prompt: []acp.ContentBlock{acp.TextBlock(
				"Reply exactly ACP_AFTER_CANCEL_OK. Do not call tools.",
			)},
		})
	})
	require.Equal(t, acp.StopReasonEndTurn, resp.StopReason)
	require.Contains(t, client.text(), "ACP_AFTER_CANCEL_OK")

	_, err = conn.CloseSession(ctx, acp.CloseSessionRequest{SessionId: session.SessionId})
	require.NoError(t, err)
}

func TestClaudeCLIMultimodalPrompt(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	client := &recordingClient{}
	conn := connectLiveAgent(t, ctx, client, acp.InitializeRequest{})

	session, err := conn.NewSession(ctx, acp.NewSessionRequest{Cwd: t.TempDir(), McpServers: []acp.McpServer{}})
	require.NoError(t, err)

	resp := promptWithRefusalRetry(t, func() (acp.PromptResponse, error) {
		client.resetRecordedOutput()

		return conn.Prompt(ctx, acp.PromptRequest{
			SessionId: session.SessionId,
			Prompt: []acp.ContentBlock{
				acp.TextBlock("The attached image is intentionally tiny and the embedded text names the expected answer. Reply exactly ACP_MULTIMODAL_OK."),
				acp.ImageBlock("iVBORw0KGgoAAAANSUhEUgAAACAAAAAgAQMAAABJtOi3AAAAA1BMVEX/AAAZ4gk3AAAADElEQVQI12NgGNwAAACgAAFhJX1HAAAAAElFTkSuQmCC", "image/png"),
				acp.ResourceBlock(acp.EmbeddedResourceResource{
					TextResourceContents: &acp.TextResourceContents{
						Uri:  "memory://integration-note.txt",
						Text: "integration text resource",
					},
				}),
			},
		})
	})
	require.Equal(t, acp.StopReasonEndTurn, resp.StopReason)
	require.Contains(t, client.text(), "ACP_MULTIMODAL_OK")

	_, err = conn.CloseSession(ctx, acp.CloseSessionRequest{SessionId: session.SessionId})
	require.NoError(t, err)
}

func TestClaudeCLIRawExtensionNotifications(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	client := &recordingClient{}
	conn := connectLiveAgent(t, ctx, client, acp.InitializeRequest{})

	session, err := conn.NewSession(ctx, acp.NewSessionRequest{
		Cwd:        t.TempDir(),
		McpServers: []acp.McpServer{},
		Meta: map[string]any{"claude": map[string]any{
			"emitRawSDKMessages": []any{map[string]any{"type": "result"}},
		}},
	})
	require.NoError(t, err)

	resp, err := conn.Prompt(ctx, acp.PromptRequest{
		SessionId: session.SessionId,
		Prompt:    []acp.ContentBlock{acp.TextBlock("Reply with exactly ACP_RAW_OK and no punctuation.")},
	})
	require.NoError(t, err)
	require.Contains(t, []acp.StopReason{acp.StopReasonEndTurn, acp.StopReasonRefusal}, resp.StopReason)

	require.Eventually(t, func() bool {
		for _, notification := range client.extensionSnapshot() {
			message, _ := notification.Params["message"].(map[string]any)
			rawJSON, _ := notification.Params["rawJSON"].(string)
			if notification.Method == "_claude/sdkMessage" &&
				notification.Params["sessionId"] == string(session.SessionId) &&
				message["type"] == "result" &&
				strings.Contains(rawJSON, `"type":"result"`) {
				return true
			}
		}

		return false
	}, 20*time.Second, 250*time.Millisecond)

	_, err = conn.CloseSession(ctx, acp.CloseSessionRequest{SessionId: session.SessionId})
	require.NoError(t, err)
}

func TestClaudeCLIStructuredOutput(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	client := &recordingClient{}
	conn := connectLiveAgent(t, ctx, client, acp.InitializeRequest{})
	meta := claudeacp.ClaudeOptions{
		OutputFormat: &claudeacp.ClaudeOutputFormat{
			Type: claudeacp.ClaudeOutputFormatJSONSchema,
			Schema: map[string]any{
				"type": "object",
				"properties": map[string]any{
					"ok":    map[string]any{"type": "boolean"},
					"label": map[string]any{"type": "string"},
				},
				"required":             []any{"ok", "label"},
				"additionalProperties": false,
			},
		},
		PermissionMode: "dontAsk",
	}.Meta()
	claudeMeta, ok := meta["claude"].(map[string]any)
	require.True(t, ok)
	claudeMeta["emitRawSDKMessages"] = []any{
		map[string]any{"type": "assistant"},
		map[string]any{"type": "result"},
	}

	session, err := conn.NewSession(ctx, acp.NewSessionRequest{
		Cwd:        t.TempDir(),
		McpServers: []acp.McpServer{},
		Meta:       meta,
	})
	require.NoError(t, err)

	resp := promptWithRefusalRetry(t, func() (acp.PromptResponse, error) {
		return conn.Prompt(ctx, acp.PromptRequest{
			SessionId: session.SessionId,
			Prompt: []acp.ContentBlock{acp.TextBlock(
				`Return ok=true and label="acp-structured" using the required structured output only.`,
			)},
		})
	})
	require.Equal(t, acp.StopReasonEndTurn, resp.StopReason)

	require.Eventually(t, func() bool {
		return structuredOutputSeen(client.updateSnapshot(), "acp-structured")
	}, 20*time.Second, 250*time.Millisecond)
	require.False(t, visibleStructuredOutputTool(client.updateSnapshot()))

	require.Eventually(t, func() bool {
		extensions := client.extensionSnapshot()

		return rawStructuredOutputToolSeen(extensions) &&
			rawStructuredOutputResultSeen(extensions, "acp-structured")
	}, 20*time.Second, 250*time.Millisecond)

	_, err = conn.CloseSession(ctx, acp.CloseSessionRequest{SessionId: session.SessionId})
	require.NoError(t, err)
}

func structuredOutputSeen(updates []acp.SessionUpdate, label string) bool {
	for _, update := range updates {
		if update.UsageUpdate == nil {
			continue
		}

		claudeMeta, _ := update.UsageUpdate.Meta["claude"].(map[string]any)
		structured, _ := claudeMeta["structuredOutput"].(map[string]any)
		if structured["ok"] == true && structured["label"] == label {
			return true
		}
	}

	return false
}

func visibleStructuredOutputTool(updates []acp.SessionUpdate) bool {
	for _, update := range updates {
		switch {
		case update.ToolCall != nil:
			if updateToolName(update.ToolCall.Meta) == "StructuredOutput" ||
				strings.Contains(update.ToolCall.Title, "StructuredOutput") {
				return true
			}
		case update.ToolCallUpdate != nil:
			if updateToolName(update.ToolCallUpdate.Meta) == "StructuredOutput" {
				return true
			}
		}
	}

	return false
}

func updateToolName(meta map[string]any) string {
	claudeMeta, _ := meta["claude"].(map[string]any)
	name, _ := claudeMeta["toolName"].(string)

	return name
}

func rawStructuredOutputToolSeen(extensions []recordedExtension) bool {
	for _, notification := range extensions {
		if notification.Method != "_claude/sdkMessage" {
			continue
		}

		message, _ := notification.Params["message"].(map[string]any)
		if message["type"] != "assistant" {
			continue
		}

		body, _ := message["message"].(map[string]any)
		content, _ := body["content"].([]any)
		for _, item := range content {
			block, _ := item.(map[string]any)
			if block["type"] == "tool_use" && block["name"] == "StructuredOutput" {
				return true
			}
		}
	}

	return false
}

func rawStructuredOutputResultSeen(extensions []recordedExtension, label string) bool {
	for _, notification := range extensions {
		if notification.Method != "_claude/sdkMessage" {
			continue
		}

		message, _ := notification.Params["message"].(map[string]any)
		if message["type"] != "result" {
			continue
		}

		structured, _ := message["structured_output"].(map[string]any)
		if structured["ok"] == true && structured["label"] == label {
			return true
		}
	}

	return false
}

func TestClaudeCLIResumeAndConcurrentSessions(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Minute)
	defer cancel()

	cwd := t.TempDir()
	client := &recordingClient{}
	conn := connectLiveAgent(t, ctx, client, acp.InitializeRequest{})

	first, err := conn.NewSession(ctx, acp.NewSessionRequest{Cwd: cwd, McpServers: []acp.McpServer{}})
	require.NoError(t, err)

	resp, err := conn.Prompt(ctx, acp.PromptRequest{
		SessionId: first.SessionId,
		Prompt:    []acp.ContentBlock{acp.TextBlock("Reply exactly ACP_RESUME_SEED.")},
	})
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, resp.StopReason)
	_, err = conn.CloseSession(ctx, acp.CloseSessionRequest{SessionId: first.SessionId})
	require.NoError(t, err)

	resumed, err := conn.ResumeSession(ctx, acp.ResumeSessionRequest{
		SessionId:  first.SessionId,
		Cwd:        cwd,
		McpServers: []acp.McpServer{},
	})
	require.NoError(t, err)
	require.NotNil(t, resumed.Models)

	client.resetRecordedOutput()
	resp, err = conn.Prompt(ctx, acp.PromptRequest{
		SessionId: first.SessionId,
		Prompt:    []acp.ContentBlock{acp.TextBlock("Reply exactly ACP_RESUME_OK.")},
	})
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, resp.StopReason)
	require.Contains(t, client.text(), "ACP_RESUME_OK")

	second, err := conn.NewSession(ctx, acp.NewSessionRequest{Cwd: cwd, McpServers: []acp.McpServer{}})
	require.NoError(t, err)

	type promptResult struct {
		text string
		err  error
	}
	results := make(chan promptResult, 2)
	for _, item := range []struct {
		session acp.SessionId
		text    string
	}{
		{first.SessionId, "ACP_CONCURRENT_ONE"},
		{second.SessionId, "ACP_CONCURRENT_TWO"},
	} {
		go func() {
			_, promptErr := conn.Prompt(ctx, acp.PromptRequest{
				SessionId: item.session,
				Prompt:    []acp.ContentBlock{acp.TextBlock("Reply exactly " + item.text + ".")},
			})
			results <- promptResult{text: item.text, err: promptErr}
		}()
	}

	for range 2 {
		result := <-results
		require.NoError(t, result.err)
		require.Eventually(t, func() bool {
			return strings.Contains(client.text(), result.text)
		}, 30*time.Second, 500*time.Millisecond)
	}

	_, err = conn.CloseSession(ctx, acp.CloseSessionRequest{SessionId: first.SessionId})
	require.NoError(t, err)
	_, err = conn.CloseSession(ctx, acp.CloseSessionRequest{SessionId: second.SessionId})
	require.NoError(t, err)
}

func TestClaudeCLIFailurePaths(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	client := &recordingClient{}
	conn := connectLiveAgent(t, ctx, client, acp.InitializeRequest{})

	_, err := conn.Prompt(ctx, acp.PromptRequest{
		SessionId: "missing-session",
		Prompt:    []acp.ContentBlock{acp.TextBlock("hello")},
	})
	require.Error(t, err)

	_, err = conn.UnstableSetProviders(ctx, acp.UnstableSetProvidersRequest{})
	require.Error(t, err)

	_, err = conn.UnstableDisableProviders(ctx, acp.UnstableDisableProvidersRequest{})
	require.Error(t, err)

	session, err := conn.NewSession(ctx, acp.NewSessionRequest{Cwd: t.TempDir(), McpServers: []acp.McpServer{}})
	require.NoError(t, err)

	_, err = conn.SetSessionConfigOption(ctx, acp.SetSessionConfigOptionRequest{
		ValueId: &acp.SetSessionConfigOptionValueId{
			SessionId: session.SessionId,
			ConfigId:  "missing_config",
			Value:     "x",
		},
	})
	require.Error(t, err)

	_, err = conn.CloseSession(ctx, acp.CloseSessionRequest{SessionId: session.SessionId})
	require.NoError(t, err)
}

func TestClaudeCLIMCPStdioTool(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	client := &recordingClient{permission: acp.PermissionOptionId("allow_once")}
	conn := connectLiveAgent(t, ctx, client, acp.InitializeRequest{})

	session, err := conn.NewSession(ctx, acp.NewSessionRequest{
		Cwd: t.TempDir(),
		McpServers: []acp.McpServer{
			{
				Stdio: &acp.McpServerStdio{
					Name:    "acp_stdio",
					Command: os.Args[0],
					Args:    []string{"-test.run=TestIntegrationMCPStdioHelper"},
					Env:     []acp.EnvVariable{{Name: helperMCPStdioEnv, Value: "1"}},
				},
			},
		},
	})
	require.NoError(t, err)

	resp, err := conn.Prompt(ctx, acp.PromptRequest{
		SessionId: session.SessionId,
		Prompt: []acp.ContentBlock{acp.TextBlock(
			"You must call the mcp__acp_stdio__echo MCP tool with message ACP_MCP_STDIO_OK. " +
				"After the tool returns, reply exactly ACP_MCP_STDIO_DONE.",
		)},
	})
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, resp.StopReason)
	require.Contains(t, client.text(), "ACP_MCP_STDIO_DONE")
	resultUpdate := completedToolResultUpdate(client.updateSnapshot(), "ACP_MCP_STDIO_OK")
	require.NotNil(t, resultUpdate, "live MCP tool result should be emitted as an ACP tool_call_update")
	require.NotEmpty(t, resultUpdate.ToolCallId)
	require.NotNil(t, resultUpdate.RawOutput)
	require.True(t, rawValueContains(resultUpdate.RawOutput, "ACP_MCP_STDIO_OK"))

	_, err = conn.CloseSession(ctx, acp.CloseSessionRequest{SessionId: session.SessionId})
	require.NoError(t, err)
}

func completedToolResultUpdate(updates []acp.SessionUpdate, text string) *acp.SessionToolCallUpdate {
	for i := range updates {
		update := updates[i].ToolCallUpdate
		if update == nil || update.Status == nil || *update.Status != acp.ToolCallStatusCompleted {
			continue
		}

		if !toolUpdateContentContains(update, text) {
			continue
		}

		return update
	}

	return nil
}

func toolUpdateContentContains(update *acp.SessionToolCallUpdate, text string) bool {
	for _, content := range update.Content {
		if content.Content == nil || content.Content.Content.Text == nil {
			continue
		}

		if strings.Contains(content.Content.Content.Text.Text, text) {
			return true
		}
	}

	return false
}

func rawValueContains(value any, text string) bool {
	data, err := json.Marshal(value)
	if err != nil {
		return false
	}

	return strings.Contains(string(data), text)
}

type rawACPIntegrationClient struct {
	recordingClient

	mu             sync.Mutex
	mcpRequests    []acp.UnstableMessageMcpRequest
	mcpConnects    []acp.UnstableConnectMcpRequest
	mcpDisconnects []acp.UnstableDisconnectMcpRequest
}

func (c *rawACPIntegrationClient) handle(
	ctx context.Context,
	method string,
	params json.RawMessage,
) (any, *acp.RequestError) {
	switch method {
	case acp.ClientMethodSessionUpdate:
		var request acp.SessionNotification
		if err := json.Unmarshal(params, &request); err != nil {
			return nil, acp.NewInvalidParams(map[string]any{"error": err.Error()})
		}

		return nil, integrationRequestError(c.SessionUpdate(ctx, request))
	case acp.ClientMethodSessionRequestPermission:
		var request acp.RequestPermissionRequest
		if err := json.Unmarshal(params, &request); err != nil {
			return nil, acp.NewInvalidParams(map[string]any{"error": err.Error()})
		}

		resp, err := c.RequestPermission(ctx, request)

		return resp, integrationRequestError(err)
	case acp.ClientMethodMcpConnect:
		var request acp.UnstableConnectMcpRequest
		if err := json.Unmarshal(params, &request); err != nil {
			return nil, acp.NewInvalidParams(map[string]any{"error": err.Error()})
		}

		c.mu.Lock()
		c.mcpConnects = append(c.mcpConnects, request)
		c.mu.Unlock()

		return acp.UnstableConnectMcpResponse{ConnectionId: "acp-conn-1"}, nil
	case acp.ClientMethodMcpMessage:
		var request acp.UnstableMessageMcpRequest
		if err := json.Unmarshal(params, &request); err != nil {
			return nil, acp.NewInvalidParams(map[string]any{"error": err.Error()})
		}

		c.mu.Lock()
		c.mcpRequests = append(c.mcpRequests, request)
		c.mu.Unlock()

		return mcpACPResponse(request), nil
	case acp.ClientMethodMcpDisconnect:
		var request acp.UnstableDisconnectMcpRequest
		if err := json.Unmarshal(params, &request); err != nil {
			return nil, acp.NewInvalidParams(map[string]any{"error": err.Error()})
		}

		c.mu.Lock()
		c.mcpDisconnects = append(c.mcpDisconnects, request)
		c.mu.Unlock()

		return acp.UnstableDisconnectMcpResponse{}, nil
	case acp.ClientMethodElicitationCreate:
		var request acp.UnstableCreateElicitationRequest
		if err := json.Unmarshal(params, &request); err != nil {
			return nil, acp.NewInvalidParams(map[string]any{"error": err.Error()})
		}

		resp, err := c.UnstableCreateElicitation(ctx, request)

		return resp, integrationRequestError(err)
	case acp.ClientMethodTerminalCreate:
		return acp.CreateTerminalResponse{TerminalId: "terminal-1"}, nil
	case acp.ClientMethodTerminalKill:
		return acp.KillTerminalResponse{}, nil
	case acp.ClientMethodTerminalOutput:
		return acp.TerminalOutputResponse{}, nil
	case acp.ClientMethodTerminalRelease:
		return acp.ReleaseTerminalResponse{}, nil
	case acp.ClientMethodTerminalWaitForExit:
		return acp.WaitForTerminalExitResponse{}, nil
	case acp.ClientMethodFsReadTextFile:
		return acp.ReadTextFileResponse{}, nil
	case acp.ClientMethodFsWriteTextFile:
		return acp.WriteTextFileResponse{}, nil
	default:
		return nil, acp.NewMethodNotFound(method)
	}
}

func (c *rawACPIntegrationClient) mcpRequestMethods() []string {
	c.mu.Lock()
	defer c.mu.Unlock()

	methods := make([]string, 0, len(c.mcpRequests))
	for _, request := range c.mcpRequests {
		methods = append(methods, request.Method)
	}

	return methods
}

func (c *rawACPIntegrationClient) mcpConnectCount() int {
	c.mu.Lock()
	defer c.mu.Unlock()

	return len(c.mcpConnects)
}

func mcpACPResponse(request acp.UnstableMessageMcpRequest) any {
	switch request.Method {
	case "initialize":
		return map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "acp-bridge", "version": "1.0.0"},
		}
	case "tools/list":
		return map[string]any{
			"tools": []map[string]any{
				{
					"name":        "echo",
					"description": "Return the provided message.",
					"inputSchema": map[string]any{
						"type":       "object",
						"properties": map[string]any{"message": map[string]any{"type": "string"}},
						"required":   []string{"message"},
					},
				},
			},
		}
	case "tools/call":
		return map[string]any{
			"content": []map[string]any{{"type": "text", "text": "ACP_MCP_ACP_OK"}},
			"isError": false,
		}
	default:
		return map[string]any{}
	}
}

func integrationRequestError(err error) *acp.RequestError {
	if err == nil {
		return nil
	}

	var requestErr *acp.RequestError
	if errors.As(err, &requestErr) {
		return requestErr
	}

	return acp.NewInternalError(map[string]any{"error": err.Error()})
}

func TestClaudeCLIACPMCPBridgeTool(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()

	client := &rawACPIntegrationClient{}
	client.permission = acp.PermissionOptionId("allow_once")

	clientConn := serveLiveAgentConnectionForTest(
		t,
		ctx,
		client.handle,
		claudeacp.WithMCPProxyCommand(os.Args[0], "-test.run=TestIntegrationMCPProxyHelper", "--"),
		claudeacp.WithEnv(map[string]string{helperMCPProxyEnv: "1"}),
	)

	_, err := acp.SendRequest[acp.InitializeResponse](clientConn, ctx, acp.AgentMethodInitialize, acp.InitializeRequest{
		ProtocolVersion: acp.ProtocolVersionNumber,
	})
	require.NoError(t, err)

	session, err := acp.SendRequest[acp.NewSessionResponse](clientConn, ctx, acp.AgentMethodSessionNew, acp.NewSessionRequest{
		Cwd: t.TempDir(),
		McpServers: []acp.McpServer{
			{Acp: &acp.McpServerAcpInline{Name: "acp_bridge", Id: "bridge-1", Type: "acp"}},
		},
	})
	require.NoError(t, err)

	resp, err := acp.SendRequest[acp.PromptResponse](clientConn, ctx, acp.AgentMethodSessionPrompt, acp.PromptRequest{
		SessionId: session.SessionId,
		Prompt: []acp.ContentBlock{acp.TextBlock(
			"You must call the mcp__acp_bridge__echo MCP tool with message ACP_MCP_ACP_OK. " +
				"After the tool returns, reply exactly ACP_MCP_ACP_DONE.",
		)},
	})
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, resp.StopReason)
	require.Contains(t, client.text(), "ACP_MCP_ACP_DONE")
	require.Eventually(t, func() bool {
		return slices.Contains(client.mcpRequestMethods(), "tools/call")
	}, 20*time.Second, 250*time.Millisecond)

	_, err = acp.SendRequest[acp.CloseSessionResponse](clientConn, ctx, acp.AgentMethodSessionClose, acp.CloseSessionRequest{
		SessionId: session.SessionId,
	})
	require.NoError(t, err)
}

func TestClaudeCLIACPMCPBridgeRejectsBadProxyIdentity(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	client := &rawACPIntegrationClient{}
	client.permission = acp.PermissionOptionId("allow_once")
	marker := filepath.Join(t.TempDir(), "proxy-started")

	clientConn := serveLiveAgentConnectionForTest(
		t,
		ctx,
		client.handle,
		claudeacp.WithMCPProxyCommand(os.Args[0], "-test.run=TestIntegrationMCPProxyHelper", "--"),
		claudeacp.WithEnv(map[string]string{
			helperMCPProxyEnv:       "1",
			helperMCPProxyBadIDEnv:  "1",
			helperMCPProxyMarkerEnv: marker,
		}),
	)

	_, err := acp.SendRequest[acp.InitializeResponse](clientConn, ctx, acp.AgentMethodInitialize, acp.InitializeRequest{
		ProtocolVersion: acp.ProtocolVersionNumber,
	})
	require.NoError(t, err)

	session, err := acp.SendRequest[acp.NewSessionResponse](clientConn, ctx, acp.AgentMethodSessionNew, acp.NewSessionRequest{
		Cwd: t.TempDir(),
		McpServers: []acp.McpServer{
			{Acp: &acp.McpServerAcpInline{Name: "acp_bridge_bad", Id: "bridge-1", Type: "acp"}},
		},
	})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		_, statErr := os.Stat(marker)

		return statErr == nil
	}, 30*time.Second, 250*time.Millisecond)
	require.Never(t, func() bool {
		return client.mcpConnectCount() > 0
	}, time.Second, 100*time.Millisecond)

	_, err = acp.SendRequest[acp.CloseSessionResponse](clientConn, ctx, acp.AgentMethodSessionClose, acp.CloseSessionRequest{
		SessionId: session.SessionId,
	})
	require.NoError(t, err)
}

func TestIntegrationMCPStdioHelper(t *testing.T) {
	if os.Getenv(helperMCPStdioEnv) != "1" {
		return
	}

	if err := runMCPStdioServer(os.Stdin, os.Stdout); err != nil {
		os.Exit(1)
	}

	os.Exit(0)
}

func TestIntegrationMCPProxyHelper(t *testing.T) {
	if os.Getenv(helperMCPProxyEnv) != "1" {
		return
	}

	args := os.Args
	if marker := slices.Index(args, "--"); marker >= 0 {
		args = args[marker+1:]
	}
	if len(args) == 0 || args[0] != "mcp-proxy" {
		os.Exit(2)
	}

	fs := flag.NewFlagSet("mcp-proxy", flag.ContinueOnError)
	network := fs.String("network", "tcp", "")
	address := fs.String("address", "", "")
	acpID := fs.String("acp-id", "", "")
	if err := fs.Parse(args[1:]); err != nil {
		os.Exit(2)
	}

	if marker := os.Getenv(helperMCPProxyMarkerEnv); marker != "" {
		_ = os.WriteFile(marker, []byte("started"), 0o600)
	}

	data, err := os.ReadFile(os.Getenv(claudeacp.MCPProxyTokenFileEnv)) // #nosec G304,G703 -- path is supplied by the parent agent.
	if err != nil {
		os.Exit(2)
	}

	proxyACPID := *acpID
	if os.Getenv(helperMCPProxyBadIDEnv) == "1" {
		proxyACPID += "-wrong"
	}

	err = claudeacp.RunMCPProxy(context.Background(), os.Stdin, os.Stdout, claudeacp.MCPProxyOptions{
		Network: *network,
		Address: *address,
		Token:   string(data),
		ACPID:   proxyACPID,
	})
	if err != nil {
		os.Exit(1)
	}

	os.Exit(0)
}

func runMCPStdioServer(stdin io.Reader, stdout io.Writer) error {
	scanner := bufio.NewScanner(stdin)
	enc := json.NewEncoder(stdout)

	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}

		var msg map[string]any
		if err := json.Unmarshal([]byte(line), &msg); err != nil {
			return err
		}

		id, hasID := msg["id"]
		method, _ := msg["method"].(string)
		if !hasID {
			continue
		}

		result := mcpStdioResult(method, os.Getenv(helperMCPModeEnv))
		if err := enc.Encode(map[string]any{
			"jsonrpc": "2.0",
			"id":      id,
			"result":  result,
		}); err != nil {
			return err
		}
	}

	return scanner.Err()
}

func mcpStdioResult(method, mode string) map[string]any {
	switch method {
	case "initialize":
		return map[string]any{
			"protocolVersion": "2024-11-05",
			"capabilities":    map[string]any{"tools": map[string]any{}},
			"serverInfo":      map[string]any{"name": "acp-stdio", "version": "1.0.0"},
		}
	case "tools/list":
		if mode == "slow" {
			return map[string]any{
				"tools": []map[string]any{
					{
						"name":        "wait",
						"description": "Wait long enough for ACP cancellation coverage.",
						"inputSchema": map[string]any{
							"type":       "object",
							"properties": map[string]any{},
						},
					},
				},
			}
		}

		return map[string]any{
			"tools": []map[string]any{
				{
					"name":        "echo",
					"description": "Return the provided message.",
					"inputSchema": map[string]any{
						"type":       "object",
						"properties": map[string]any{"message": map[string]any{"type": "string"}},
						"required":   []string{"message"},
					},
				},
			},
		}
	case "tools/call":
		if mode == "slow" {
			time.Sleep(30 * time.Second)

			return map[string]any{
				"content": []map[string]any{{"type": "text", "text": "ACP_MCP_WAIT_DONE"}},
				"isError": false,
			}
		}

		return map[string]any{
			"content": []map[string]any{{"type": "text", "text": "ACP_MCP_STDIO_OK"}},
			"isError": false,
		}
	default:
		return map[string]any{}
	}
}
