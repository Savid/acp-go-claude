//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/require"
)

func TestClaudeCLIACPConversation(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cwd := t.TempDir()
	client := &recordingClient{}
	clientConn := connectLiveAgent(t, ctx, client, acp.InitializeRequest{})

	providers, err := clientConn.UnstableListProviders(ctx, acp.UnstableListProvidersRequest{})
	require.NoError(t, err)
	require.Len(t, providers.Providers, 1)
	require.Equal(t, "claude-code", providers.Providers[0].Id)

	nes, err := clientConn.UnstableStartNes(ctx, acp.UnstableStartNesRequest{})
	require.NoError(t, err)
	require.NotEmpty(t, nes.SessionId)

	err = clientConn.UnstableDidOpenDocument(ctx, acp.UnstableDidOpenDocumentNotification{
		SessionId:  nes.SessionId,
		Uri:        "file:///repo/main.go",
		LanguageId: "go",
		Text:       "package main\n",
		Version:    1,
	})
	require.NoError(t, err)

	suggestion, err := clientConn.UnstableSuggestNes(ctx, acp.UnstableSuggestNesRequest{
		SessionId:   nes.SessionId,
		Uri:         "file:///repo/main.go",
		Version:     1,
		TriggerKind: acp.UnstableNesTriggerKindManual,
		Position:    acp.UnstablePosition{Line: 0, Character: 0},
	})
	require.NoError(t, err)
	require.NotNil(t, suggestion.Suggestions)

	_, err = clientConn.UnstableCloseNes(ctx, acp.UnstableCloseNesRequest{SessionId: nes.SessionId})
	require.NoError(t, err)

	session, err := clientConn.NewSession(ctx, acp.NewSessionRequest{Cwd: cwd, McpServers: []acp.McpServer{}})
	require.NoError(t, err)
	require.NotEmpty(t, session.SessionId)
	require.NotNil(t, session.Models)
	require.NotEmpty(t, session.Models.CurrentModelId)
	require.NotEmpty(t, session.Models.AvailableModels)
	require.NotEmpty(t, session.ConfigOptions)
	claudeMeta, ok := session.Meta["claude"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, string(session.Models.CurrentModelId), claudeMeta["modelId"])
	require.Contains(t, sessionModelIDs(session.Models.AvailableModels), session.Models.CurrentModelId)
	outputStyle := findSelectConfig(t, session.ConfigOptions, "output_style")
	require.NotEmpty(t, outputStyle.CurrentValue)

	require.Eventually(t, func() bool {
		return client.commandCount() > 0
	}, 30*time.Second, 500*time.Millisecond)

	if sessionModeAvailable(session.Modes, acp.SessionModeId("auto")) {
		_, err = clientConn.SetSessionMode(ctx, acp.SetSessionModeRequest{
			SessionId: session.SessionId,
			ModeId:    acp.SessionModeId("auto"),
		})
		require.NoError(t, err)
	}

	_, err = clientConn.SetSessionConfigOption(ctx, acp.SetSessionConfigOptionRequest{
		ValueId: &acp.SetSessionConfigOptionValueId{
			SessionId: session.SessionId,
			ConfigId:  "output_style",
			Value:     outputStyle.CurrentValue,
		},
	})
	require.NoError(t, err)

	if effort := selectConfig(session.ConfigOptions, "effort"); effort != nil {
		require.NotEmpty(t, selectConfigValues(effort))
		require.Contains(t, selectConfigValues(effort), effort.CurrentValue)

		_, err = clientConn.SetSessionConfigOption(ctx, acp.SetSessionConfigOptionRequest{
			ValueId: &acp.SetSessionConfigOptionValueId{
				SessionId: session.SessionId,
				ConfigId:  "effort",
				Value:     effort.CurrentValue,
			},
		})
		require.NoError(t, err)
	}

	if fastMode := booleanConfig(session.ConfigOptions, "fast_mode"); fastMode != nil {
		_, err = clientConn.SetSessionConfigOption(ctx, acp.SetSessionConfigOptionRequest{
			Boolean: &acp.SetSessionConfigOptionBoolean{
				SessionId: session.SessionId,
				ConfigId:  "fast_mode",
				Value:     fastMode.CurrentValue,
			},
		})
		require.NoError(t, err)
	}

	messageID := "22222222-2222-4222-8222-222222222222"
	resp, err := clientConn.Prompt(ctx, acp.PromptRequest{
		SessionId: session.SessionId,
		MessageId: &messageID,
		Prompt: []acp.ContentBlock{
			acp.TextBlock("Reply with exactly ACP_OK and no punctuation."),
		},
	})
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, resp.StopReason)
	require.Equal(t, &messageID, resp.UserMessageId)
	require.NotNil(t, resp.Usage)
	require.Positive(t, resp.Usage.TotalTokens)
	require.Contains(t, client.text(), "ACP_OK")
	require.Eventually(t, func() bool {
		usage := client.latestUsage()

		return usage != nil && usage.Cost != nil && usage.Cost.Currency == "USD" && usage.Used > 0 && usage.Size > 0
	}, 30*time.Second, 500*time.Millisecond)

	_, err = clientConn.CloseSession(ctx, acp.CloseSessionRequest{SessionId: session.SessionId})
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		list, listErr := clientConn.ListSessions(ctx, acp.ListSessionsRequest{Cwd: &cwd})
		if listErr != nil {
			return false
		}

		for _, listed := range list.Sessions {
			if listed.SessionId == session.SessionId {
				return true
			}
		}

		return false
	}, 30*time.Second, 500*time.Millisecond)

	client.clear()

	fork, err := clientConn.UnstableForkSession(ctx, acp.UnstableForkSessionRequest{
		SessionId:  session.SessionId,
		Cwd:        cwd,
		McpServers: []acp.UnstableMcpServer{},
	})
	require.NoError(t, err)
	require.NotEmpty(t, fork.SessionId)
	require.NotEqual(t, session.SessionId, fork.SessionId)
	require.NotNil(t, fork.Models)
	require.NotEmpty(t, fork.Models.AvailableModels)
	require.NotEmpty(t, fork.ConfigOptions)

	resp, err = clientConn.Prompt(ctx, acp.PromptRequest{
		SessionId: fork.SessionId,
		Prompt: []acp.ContentBlock{
			acp.TextBlock("Reply with exactly ACP_FORK_OK and no punctuation."),
		},
	})
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, resp.StopReason)
	require.Contains(t, client.text(), "ACP_FORK_OK")

	_, err = clientConn.CloseSession(ctx, acp.CloseSessionRequest{SessionId: fork.SessionId})
	require.NoError(t, err)

	client.clear()

	_, err = clientConn.LoadSession(ctx, acp.LoadSessionRequest{
		SessionId:  session.SessionId,
		Cwd:        cwd,
		McpServers: []acp.McpServer{},
	})
	require.NoError(t, err)

	require.Contains(t, client.text(), "Reply with exactly ACP_OK")
	require.Contains(t, client.text(), "ACP_OK")

	_, err = clientConn.CloseSession(ctx, acp.CloseSessionRequest{SessionId: session.SessionId})
	require.NoError(t, err)
}
