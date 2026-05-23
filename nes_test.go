package claudeacp

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/stretchr/testify/require"
)

type nesScriptTransport struct {
	messages chan map[string]any
	errs     chan error

	queryMessages []map[string]any
	startErr      error
	userSendErr   error
	closeErr      error
}

func newNESScriptTransport(queryMessages ...map[string]any) *nesScriptTransport {
	return &nesScriptTransport{
		messages:      make(chan map[string]any, 16),
		errs:          make(chan error, 1),
		queryMessages: queryMessages,
	}
}

func (t *nesScriptTransport) Start(context.Context) error {
	return t.startErr
}

func (t *nesScriptTransport) Send(_ context.Context, payload any) error {
	switch typed := payload.(type) {
	case claude.ControlRequest:
		t.messages <- map[string]any{
			"type": "control_response",
			"response": map[string]any{
				"subtype":    "success",
				"request_id": typed.RequestID,
				"response":   map[string]any{},
			},
		}
	case map[string]any:
		if typed["type"] == "user" {
			if t.userSendErr != nil {
				return t.userSendErr
			}

			for _, msg := range t.queryMessages {
				t.messages <- msg
			}
		}
	}

	return nil
}

func (t *nesScriptTransport) Messages(context.Context) (<-chan map[string]any, <-chan error) {
	return t.messages, t.errs
}

func (t *nesScriptTransport) Close() error {
	return t.closeErr
}

func TestNESSessionCloneAndClaudeOptions(t *testing.T) {
	t.Parallel()

	workspaceURI := "file:///repo"
	start := acp.UnstableStartNesRequest{
		Meta: map[string]any{
			"key": "value",
			"nested": map[string]any{
				"items": []any{
					map[string]any{"name": "first"},
				},
			},
			"nilSlice": []any(nil),
		},
		Repository: &acp.UnstableNesRepository{
			Name:      "repo",
			Owner:     "owner",
			RemoteUrl: "https://example.com/repo.git",
		},
		WorkspaceUri: &workspaceURI,
		WorkspaceFolders: []acp.UnstableWorkspaceFolder{
			{Name: "repo", Uri: "file:///repo"},
			{Name: "extra", Uri: "file:///extra"},
			{Name: "bad", Uri: "https://example.com/not-local"},
		},
	}

	session := newNESSession(start)
	start.Meta["key"] = "changed"
	startNested, ok := start.Meta["nested"].(map[string]any)
	require.True(t, ok)
	startItems, ok := startNested["items"].([]any)
	require.True(t, ok)
	startItem, ok := startItems[0].(map[string]any)
	require.True(t, ok)
	startItem["name"] = "changed"
	start.Repository.Name = "changed"
	*start.WorkspaceUri = "file:///changed"
	start.WorkspaceFolders[0].Uri = "file:///changed"

	require.Equal(t, "value", session.start.Meta["key"])
	sessionNested, ok := session.start.Meta["nested"].(map[string]any)
	require.True(t, ok)
	sessionItems, ok := sessionNested["items"].([]any)
	require.True(t, ok)
	sessionItem, ok := sessionItems[0].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "first", sessionItem["name"])
	nilSlice, ok := session.start.Meta["nilSlice"].([]any)
	require.True(t, ok)
	require.Nil(t, nilSlice)
	require.Equal(t, "repo", session.start.Repository.Name)
	require.Equal(t, "file:///repo", *session.start.WorkspaceUri)
	require.Equal(t, "file:///repo", session.start.WorkspaceFolders[0].Uri)
	require.Nil(t, cloneAnyMap(nil))
	require.Nil(t, cloneAnySlice(nil))

	agent := NewAgent(
		WithClaudePath("/bin/claude"),
		WithClaudeHome("/tmp/claude"),
		WithDefaultModel("claude-test"),
		WithInitializeTimeout(time.Second),
		WithControlHandlerTimeout(2*time.Second),
	)
	options := agent.nesClaudeOptions(session, "nes-1")

	require.Equal(t, "/bin/claude", options.CLIPath)
	require.Equal(t, "/repo", options.Cwd)
	require.Equal(t, []string{"/extra"}, options.AddDirs)
	require.Equal(t, "/tmp/claude", options.ClaudeHome)
	require.Equal(t, "claude-test", options.Model)
	require.Equal(t, "nes-1", options.SessionID)
	require.Equal(t, string(modePlan), options.PermissionMode)
	require.Equal(t, nesSystemPrompt, options.SystemText)
	require.Equal(t, time.Second, options.InitializeTimeout)
	require.Equal(t, 2*time.Second, options.ControlHandlerTimeout)

	decision, err := options.PermissionHandler(context.Background(), claude.PermissionRequest{})
	require.NoError(t, err)
	require.Equal(t, claude.BehaviorDeny, decision.Behavior)
}

func TestNESWorkspaceHelpers(t *testing.T) {
	t.Parallel()

	workspaceURI := "https://example.com/repo"
	start := acp.UnstableStartNesRequest{
		WorkspaceUri: &workspaceURI,
		WorkspaceFolders: []acp.UnstableWorkspaceFolder{
			{Uri: "https://example.com/skip"},
			{Uri: "file:///repo"},
			{Uri: "file:///repo"},
			{Uri: "file:///extra"},
		},
	}

	require.Equal(t, "/repo", nesWorkspacePath(start))
	require.Equal(t, []string{"/repo", "/extra"}, nesWorkspaceAddDirs(start, ""))
	require.Equal(t, []string{"/extra"}, nesWorkspaceAddDirs(start, "/repo"))
	require.Equal(t, "/repo", fileURIToPath("file://localhost/repo"))
	require.Equal(t, "C:/repo", fileURIToPath("file:///C:/repo"))
	require.Empty(t, fileURIToPath("https://example.com/repo"))
	require.Empty(t, fileURIToPath("%zz"))
	require.Empty(t, fileURIToPath("file://example.com/repo"))
	require.Empty(t, nesWorkspacePath(acp.UnstableStartNesRequest{}))
}

func TestNESDocumentForRequest(t *testing.T) {
	t.Parallel()

	agent := NewAgent()
	sessionID := acp.SessionId("nes-1")
	agent.documents[sessionID] = map[string]documentState{
		"file:///repo/main.go": {
			URI:        "file:///repo/main.go",
			LanguageID: "go",
			Text:       "package main\n",
			Version:    7,
		},
	}

	document := agent.nesDocumentForRequest(acp.UnstableSuggestNesRequest{
		SessionId: sessionID,
		Uri:       "file:///repo/main.go",
		Version:   8,
	})
	require.Equal(t, 7, document.Version)
	require.Equal(t, "package main\n", document.Text)

	document = agent.nesDocumentForRequest(acp.UnstableSuggestNesRequest{
		SessionId: sessionID,
		Uri:       "file:///repo/other.go",
		Version:   3,
		Context: &acp.UnstableNesSuggestContext{
			RecentFiles: []acp.UnstableNesRecentFile{
				{Uri: "file:///repo/other.go", LanguageId: "go", Text: "package other\n"},
			},
		},
	})
	require.Equal(t, "package other\n", document.Text)
	require.Equal(t, 3, document.Version)

	document = agent.nesDocumentForRequest(acp.UnstableSuggestNesRequest{
		Uri:     "file:///repo/missing.go",
		Version: 1,
	})
	require.Equal(t, "file:///repo/missing.go", document.URI)
	require.Empty(t, document.Text)
}

func TestNESPrompt(t *testing.T) {
	t.Parallel()

	session := newNESSession(acp.UnstableStartNesRequest{})
	prompt, err := nesPrompt(session, documentState{
		URI:        "file:///repo/main.go",
		LanguageID: "go",
		Text:       "package main\n",
		Version:    1,
	}, acp.UnstableSuggestNesRequest{
		SessionId:   "nes-1",
		Uri:         "file:///repo/main.go",
		Version:     1,
		TriggerKind: acp.UnstableNesTriggerKindManual,
		Position:    acp.UnstablePosition{Line: 0, Character: 0},
	})
	require.NoError(t, err)
	require.Contains(t, prompt, "ACP NES suggestions")
	require.Contains(t, prompt, "package main")

	_, err = nesPrompt(session, documentState{}, acp.UnstableSuggestNesRequest{
		Context: &acp.UnstableNesSuggestContext{
			Meta: map[string]any{"bad": func() {}},
		},
	})
	require.Error(t, err)
}

func TestCollectNESSuggestionText(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	client := startNESClient(t, newNESScriptTransport(
		assistantMessage(`{"suggestions":[]}`),
		resultMessage("ignored", false, nil),
	))
	require.NoError(t, client.Query(ctx, []map[string]any{{jsonFieldType: jsonFieldText, jsonFieldText: "suggest"}}))
	text, err := collectNESSuggestionText(ctx, client)
	require.NoError(t, err)
	require.Equal(t, `{"suggestions":[]}`, text)

	client = startNESClient(t, newNESScriptTransport(resultMessage(`{"suggestions":[]}`, false, nil)))
	require.NoError(t, client.Query(ctx, []map[string]any{{jsonFieldType: jsonFieldText, jsonFieldText: "suggest"}}))
	text, err = collectNESSuggestionText(ctx, client)
	require.NoError(t, err)
	require.Equal(t, `{"suggestions":[]}`, text)

	client = startNESClient(t, newNESScriptTransport(resultMessage("", true, []string{"failed"})))
	require.NoError(t, client.Query(ctx, []map[string]any{{jsonFieldType: jsonFieldText, jsonFieldText: "suggest"}}))
	_, err = collectNESSuggestionText(ctx, client)
	require.ErrorContains(t, err, "failed")

	_, err = collectNESSuggestionText(ctx, claude.NewClient(nil, claude.Options{}, nil))
	require.ErrorContains(t, err, "receive Claude NES suggestion response")
}

func TestParseNESSuggestions(t *testing.T) {
	t.Parallel()

	params := acp.UnstableSuggestNesRequest{
		SessionId: "nes-1",
		Uri:       "file:///repo/main.go",
		Version:   4,
	}

	suggestions, err := parseNESSuggestions(`prefix {"suggestions":[{"kind":"edit","edits":[{"range":{"start":{"line":0,"character":0},"end":{"line":0,"character":0}},"newText":"hi"}]}]} suffix`, params)
	require.NoError(t, err)
	require.Len(t, suggestions, 1)
	require.NotEmpty(t, suggestions[0].Edit.Id)
	require.Equal(t, params.Uri, suggestions[0].Edit.Uri)

	suggestions, err = parseNESSuggestions("```json\n[{\"kind\":\"jump\",\"position\":{\"line\":1,\"character\":2}}]\n```", params)
	require.NoError(t, err)
	require.Len(t, suggestions, 1)
	require.NotNil(t, suggestions[0].Jump)
	require.Equal(t, params.Uri, suggestions[0].Jump.Uri)

	suggestions = normalizeNESSuggestions([]acp.UnstableNesSuggestion{
		acp.NewUnstableNesSuggestionRename("", "", acp.UnstablePosition{Line: 2, Character: 3}, ""),
		acp.NewUnstableNesSuggestionRename("", "", acp.UnstablePosition{Line: 2, Character: 3}, "nextName"),
		acp.NewUnstableNesSuggestionSearchAndReplace("", "", "", "replace"),
		acp.NewUnstableNesSuggestionSearchAndReplace("", "", "old", "new"),
		{},
		{
			Edit: &acp.UnstableNesSuggestionEdit{
				Edits: []acp.UnstableNesTextEdit{{
					Range: acp.UnstableRange{},
				}},
			},
			Jump: &acp.UnstableNesSuggestionJump{},
		},
		{
			Edit: &acp.UnstableNesSuggestionEdit{},
		},
	}, params)
	require.Len(t, suggestions, 2)
	require.NotNil(t, suggestions[0].Rename)
	require.NotNil(t, suggestions[1].SearchAndReplace)

	_, err = parseNESSuggestions("", params)
	require.Error(t, err)
	_, err = parseNESSuggestions("{bad", params)
	require.Error(t, err)

	require.Equal(t, `{"suggestions":[]}`, extractNESJSON(`{"suggestions":[]}`))
	require.Equal(t, `[{"kind":"jump"}]`, extractNESJSON(`noise [{"kind":"jump"}]`))
	require.Equal(t, `[1]`, extractNESJSON(`noise {broken [1]`))
	require.Empty(t, extractNESJSON("no json here"))
}

func TestSuggestNESErrorPaths(t *testing.T) {
	t.Parallel()

	agent := NewAgent()
	params := acp.UnstableSuggestNesRequest{
		SessionId: "nes-1",
		Uri:       "file:///repo/main.go",
		Version:   1,
	}

	_, err := agent.suggestNES(context.Background(), nil, params)
	require.Error(t, err)

	session := newNESSession(acp.UnstableStartNesRequest{})
	_, err = agent.suggestNES(context.Background(), session, acp.UnstableSuggestNesRequest{
		SessionId: "nes-1",
		Uri:       "file:///repo/main.go",
		Context: &acp.UnstableNesSuggestContext{
			Meta: map[string]any{"bad": func() {}},
		},
	})
	require.Error(t, err)

	closedSession := newNESSession(acp.UnstableStartNesRequest{})
	closedSession.close()
	_, err = agent.suggestNES(context.Background(), closedSession, params)
	require.ErrorIs(t, err, context.Canceled)

	startErr := errors.New("start failed")
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		transport := newNESScriptTransport()
		transport.startErr = startErr

		return claude.NewClient(nil, options, transport)
	}
	_, err = agent.suggestNES(context.Background(), session, params)
	require.ErrorIs(t, err, startErr)

	sendErr := errors.New("send failed")
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		transport := newNESScriptTransport()
		transport.userSendErr = sendErr

		return claude.NewClient(nil, options, transport)
	}
	_, err = agent.suggestNES(context.Background(), session, params)
	require.ErrorIs(t, err, sendErr)

	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(nil, options, newNESScriptTransport(resultMessage("", true, []string{"failed"})))
	}
	_, err = agent.suggestNES(context.Background(), session, params)
	require.ErrorContains(t, err, "failed")

	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(nil, options, newNESScriptTransport(
			assistantMessage("not json"),
			resultMessage("", false, nil),
		))
	}
	suggestions, err := agent.suggestNES(context.Background(), session, params)
	require.NoError(t, err)
	require.Empty(t, suggestions)

	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		transport := newNESScriptTransport(
			assistantMessage(`{"suggestions":[]}`),
			resultMessage("", false, nil),
		)
		transport.closeErr = errors.New("close failed")

		return claude.NewClient(nil, options, transport)
	}
	suggestions, err = agent.suggestNES(context.Background(), session, params)
	require.NoError(t, err)
	require.Empty(t, suggestions)

	start, err := agent.UnstableStartNes(context.Background(), acp.UnstableStartNesRequest{})
	require.NoError(t, err)
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		transport := newNESScriptTransport()
		transport.startErr = startErr

		return claude.NewClient(nil, options, transport)
	}
	_, err = agent.UnstableSuggestNes(context.Background(), acp.UnstableSuggestNesRequest{
		SessionId: start.SessionId,
		Uri:       "file:///repo/main.go",
	})
	require.ErrorIs(t, err, startErr)
}

func TestNESSessionSuggestLimitAndClose(t *testing.T) {
	t.Parallel()

	session := newNESSession(acp.UnstableStartNesRequest{})

	releaseFirst, err := session.acquireSuggest(context.Background())
	require.NoError(t, err)
	releasedFirst := false
	defer func() {
		if !releasedFirst {
			releaseFirst()
		}
	}()

	releaseSecond, err := session.acquireSuggest(context.Background())
	require.NoError(t, err)
	defer releaseSecond()

	blocked := make(chan error, 1)
	go func() {
		release, acquireErr := session.acquireSuggest(context.Background())
		if acquireErr == nil {
			release()
		}

		blocked <- acquireErr
	}()

	select {
	case blockedErr := <-blocked:
		require.NoError(t, blockedErr)
		t.Fatal("third NES suggestion acquired a slot before one was released")
	case <-time.After(20 * time.Millisecond):
	}

	canceledCtx, cancelAcquire := context.WithCancel(context.Background())
	cancelAcquire()
	_, err = session.acquireSuggest(canceledCtx)
	require.ErrorIs(t, err, context.Canceled)

	closingSession := newNESSession(acp.UnstableStartNesRequest{})
	closeReleaseFirst, err := closingSession.acquireSuggest(context.Background())
	require.NoError(t, err)
	defer closeReleaseFirst()

	closeReleaseSecond, err := closingSession.acquireSuggest(context.Background())
	require.NoError(t, err)
	defer closeReleaseSecond()

	closedAcquire := make(chan error, 1)
	go func() {
		release, acquireErr := closingSession.acquireSuggest(context.Background())
		if acquireErr == nil {
			release()
		}

		closedAcquire <- acquireErr
	}()

	select {
	case acquireErr := <-closedAcquire:
		require.NoError(t, acquireErr)
		t.Fatal("blocked NES suggestion acquired a slot before the session was closed")
	case <-time.After(20 * time.Millisecond):
	}

	closingSession.close()
	require.ErrorIs(t, <-closedAcquire, context.Canceled)

	releaseFirst()
	releasedFirst = true
	require.NoError(t, <-blocked)

	turnCtx, cancel := session.suggestionContext(context.Background())
	defer cancel()

	session.close()
	require.Eventually(t, func() bool {
		return turnCtx.Err() != nil
	}, time.Second, 10*time.Millisecond)
	require.ErrorIs(t, turnCtx.Err(), context.Canceled)

	_, err = session.acquireSuggest(context.Background())
	require.ErrorIs(t, err, context.Canceled)
}

func TestUnstableCloseNesCancelsSession(t *testing.T) {
	t.Parallel()

	agent := NewAgent()
	start, err := agent.UnstableStartNes(context.Background(), acp.UnstableStartNesRequest{})
	require.NoError(t, err)

	session := agent.nesSession(start.SessionId)
	require.NotNil(t, session)

	turnCtx, cancel := session.suggestionContext(context.Background())
	defer cancel()

	_, err = agent.UnstableCloseNes(context.Background(), acp.UnstableCloseNesRequest{
		SessionId: start.SessionId,
	})
	require.NoError(t, err)
	require.Eventually(t, func() bool {
		return turnCtx.Err() != nil
	}, time.Second, 10*time.Millisecond)
	require.Nil(t, agent.nesSession(start.SessionId))
}

func TestNESSuggestionStorageAndDecisions(t *testing.T) {
	t.Parallel()

	agent := NewAgent()
	sessionID := acp.SessionId("nes-1")
	suggestion := acp.NewUnstableNesSuggestionJump("jump-1", "file:///repo/main.go", acp.UnstablePosition{})

	agent.storeNESSuggestions(sessionID, []acp.UnstableNesSuggestion{suggestion})
	require.Error(t, agent.recordNESDecision(sessionID, "jump-1", nesDecisionAccepted, nil))

	agent.nesSessions[sessionID] = newNESSession(acp.UnstableStartNesRequest{})
	agent.storeNESSuggestions(sessionID, nil)
	agent.storeNESSuggestions(sessionID, []acp.UnstableNesSuggestion{{}, suggestion})
	require.Equal(t, "jump-1", nesSuggestionID(suggestion))
	require.Equal(t, "rename-1", nesSuggestionID(acp.NewUnstableNesSuggestionRename(
		"rename-1",
		"file:///repo/main.go",
		acp.UnstablePosition{},
		"nextName",
	)))
	require.Equal(t, "search-1", nesSuggestionID(acp.NewUnstableNesSuggestionSearchAndReplace(
		"search-1",
		"file:///repo/main.go",
		"old",
		"new",
	)))
	require.Empty(t, nesSuggestionID(acp.UnstableNesSuggestion{}))

	require.Error(t, agent.recordNESDecision(sessionID, "missing", nesDecisionAccepted, nil))
	require.NoError(t, agent.recordNESDecision(sessionID, "jump-1", nesDecisionAccepted, nil))
	require.Len(t, agent.nesSessions[sessionID].decisions, 1)
}

func startNESClient(t *testing.T, transport *nesScriptTransport) *claude.Client {
	t.Helper()

	client := claude.NewClient(nil, claude.Options{}, transport)
	require.NoError(t, client.Start(context.Background()))

	return client
}

func assistantMessage(text string) map[string]any {
	return map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"content": []any{
				map[string]any{jsonFieldType: jsonFieldText, jsonFieldText: text},
			},
		},
	}
}

func resultMessage(result string, isError bool, errors []string) map[string]any {
	return map[string]any{
		"type":        "result",
		"subtype":     "success",
		"is_error":    isError,
		"result":      result,
		"stop_reason": "end_turn",
		"errors":      errors,
	}
}
