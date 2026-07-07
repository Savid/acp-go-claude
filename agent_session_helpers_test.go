package claudeacp

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/stretchr/testify/require"
)

func TestAgentSessionRemovalAndStoreStartBranches(t *testing.T) {
	ctx := context.Background()
	agent := NewAgent(WithConcurrencyLimits(ConcurrencyLimits{MaxActiveSessions: 1}))
	session, cleanup := newStartedAgentSessionForTest(t, agent, "session-1")
	defer cleanup()
	agent.sessions[session.id] = session

	agent.removeSession(ctx, session.id, session)
	require.Nil(t, agent.sessions[session.id])
	agent.removeSession(ctx, "missing", nil)

	closedAgent := NewAgent()
	closedAgent.closed = true
	closedSession, closedCleanup := newStartedAgentSessionForTest(t, closedAgent, "closed")
	defer closedCleanup()
	require.ErrorIs(t, closedAgent.storeStartedSession(ctx, closedSession), errAgentClosed)

	limitAgent := NewAgent(WithConcurrencyLimits(ConcurrencyLimits{MaxActiveSessions: 1}))
	first, firstCleanup := newStartedAgentSessionForTest(t, limitAgent, "first")
	defer firstCleanup()
	require.NoError(t, limitAgent.storeStartedSession(ctx, first))
	second, secondCleanup := newStartedAgentSessionForTest(t, limitAgent, "second")
	defer secondCleanup()
	require.Error(t, limitAgent.storeStartedSession(ctx, second))

	replacementAgent := NewAgent()
	oldSession, oldCleanup := newStartedAgentSessionForTest(t, replacementAgent, "same")
	defer oldCleanup()
	newSession, newCleanup := newStartedAgentSessionForTest(t, replacementAgent, "same")
	defer newCleanup()
	require.NoError(t, replacementAgent.storeStartedSession(ctx, oldSession))
	require.NoError(t, replacementAgent.storeStartedSession(ctx, newSession))
	require.Equal(t, newSession, replacementAgent.sessions["same"])
}

func TestAgentSessionHelperBranches(t *testing.T) {
	t.Parallel()

	httpServer := acp.McpServer{Http: &acp.McpServerHttpInline{Name: "http"}}
	sseServer := acp.McpServer{Sse: &acp.McpServerSseInline{Name: "sse"}}
	acpServer := acp.McpServer{Acp: &acp.McpServerAcpInline{Name: "acp"}}
	stdioServer := acp.McpServer{Stdio: &acp.McpServerStdio{Name: "stdio"}}
	require.Equal(t, "http", mcpServerName(httpServer))
	require.Equal(t, "sse", mcpServerName(sseServer))
	require.Equal(t, "acp", mcpServerName(acpServer))
	require.Equal(t, "stdio", mcpServerName(stdioServer))
	require.Equal(t, "", mcpServerName(acp.McpServer{}))

	fingerprint := sessionStartFingerprint(sessionStart{Cwd: "/tmp/project", McpServers: []acp.McpServer{sseServer, httpServer}})
	require.NotEmpty(t, fingerprint)
	require.Contains(t, jsonFingerprint(map[string]any{"bad": func() {}}), "marshal-error")

	agent := NewAgent()
	_, err := agent.mcpConfigForStart(sessionStart{McpServers: []acp.McpServer{sseServer}})
	require.Error(t, err)
	_, err = agent.mcpConfigForStart(sessionStart{McpServers: []acp.McpServer{acpServer}})
	require.Error(t, err)
	_, err = agent.mcpConfigForStart(sessionStart{McpServers: []acp.McpServer{{}}})
	require.Error(t, err)
	config, err := agent.mcpConfigForStart(sessionStart{McpServers: []acp.McpServer{stdioServer, httpServer}})
	require.NoError(t, err)
	require.NotEmpty(t, config)
	closeSessionStartResources(&materializedSession{})
}

func TestNativeSessionBlockedAndStoreHasSession(t *testing.T) {
	ctx := context.Background()
	sessionID := acp.SessionId("11111111-1111-4111-8111-111111111111")
	store := NewInMemorySessionStore()
	agent := NewAgent(WithSessionStore(store))

	require.True(t, NewAgent().nativeSessionFallbackEnabled())
	blocked, err := NewAgent().nativeSessionBlocked(ctx, sessionID)
	require.NoError(t, err)
	require.False(t, blocked)

	require.False(t, agent.storeHasSession(ctx, string(sessionID), "/tmp/project"))
	blocked, err = agent.nativeSessionBlocked(ctx, sessionID)
	require.NoError(t, err)
	require.True(t, blocked)

	require.NoError(t, store.Append(ctx, SessionKey{SessionID: string(sessionID)}, []SessionStoreEntry{[]byte(`{"type":"user"}`)}))
	require.True(t, agent.storeHasSession(ctx, string(sessionID), "/tmp/project"))
	blocked, err = agent.nativeSessionBlocked(ctx, sessionID)
	require.NoError(t, err)
	require.False(t, blocked)

	agent.deleted[sessionID] = struct{}{}
	blocked, err = agent.nativeSessionBlocked(ctx, sessionID)
	require.NoError(t, err)
	require.True(t, blocked)

	errAgent := NewAgent(WithSessionStore(&faultSessionStore{SessionStore: NewInMemorySessionStore(), loadErr: errors.New("load failed")}))
	blocked, err = errAgent.nativeSessionBlocked(ctx, sessionID)
	require.ErrorContains(t, err, "load failed")
	require.False(t, blocked)
	require.False(t, errAgent.storeHasSession(ctx, string(sessionID), "/tmp/project"))
}

func TestListStoreSessionsTitleAndPaginationBranches(t *testing.T) {
	ctx := context.Background()
	cwd := "/tmp/project"
	other := "/tmp/other"
	ids := []string{
		"11111111-1111-4111-8111-111111111111",
		"22222222-2222-4222-8222-222222222222",
		"33333333-3333-4333-8333-333333333333",
		"44444444-4444-4444-8444-444444444444",
	}
	store := NewInMemorySessionStore()
	require.NoError(t, store.Append(ctx, SessionKey{SessionID: ids[0]}, []SessionStoreEntry{[]byte(`{"aiTitle":" AI title "}`)}))
	require.NoError(t, store.Append(ctx, SessionKey{SessionID: ids[1]}, []SessionStoreEntry{[]byte(`{"customTitle":" Custom title "}`)}))
	require.NoError(t, store.Append(ctx, SessionKey{SessionID: ids[2]}, []SessionStoreEntry{[]byte(`{"type":"user","message":{"content":"hello from string"}}`)}))
	require.NoError(t, store.Append(ctx, SessionKey{SessionID: ids[3]}, []SessionStoreEntry{[]byte(`{"type":"user","message":{"content":[{"type":"text","text":"hello from block"}]}}`)}))

	summaries := []SessionSummary{
		{SessionID: "not-a-uuid", UpdatedAtUnixMilli: 1},
		{SessionID: ids[0], UpdatedAtUnixMilli: 10, Cwd: cwd, Meta: map[string]any{"a": "b"}},
		{SessionID: ids[1], UpdatedAtUnixMilli: 20, Cwd: cwd},
		{SessionID: ids[2], UpdatedAtUnixMilli: 30, Cwd: cwd},
		{SessionID: ids[3], UpdatedAtUnixMilli: 40, Cwd: other},
	}
	agent := NewAgent(WithSessionStore(&faultSessionStore{SessionStore: store, listSessions: summaries}))
	infos, err := agent.listStoreSessions(ctx, ListSessionsRequest(WithListSessionsCwd(cwd)))
	require.NoError(t, err)
	require.Len(t, infos, 3)
	require.Equal(t, "AI title", *infos[0].Title)
	require.Equal(t, "Custom title", *infos[1].Title)
	require.Equal(t, "hello from string", *infos[2].Title)
	require.Equal(t, "b", infos[0].Meta["a"])

	infos, err = agent.listStoreSessions(ctx, ListSessionsRequest())
	require.NoError(t, err)
	require.Len(t, infos, 4)
	require.Equal(t, "hello from block", *infos[3].Title)
	require.Equal(t, ids[0], storeSessionTitle(ids[0], []SessionStoreEntry{[]byte(`not-json`)}))
	require.Equal(t, "", firstStoreUserPrompt(map[string]any{"type": "assistant"}))
	require.Equal(t, "", firstStoreUserPrompt(map[string]any{"type": "user", "message": map[string]any{"content": []any{map[string]any{"type": "image"}}}}))

	errAgent := NewAgent(WithSessionStore(&faultSessionStore{SessionStore: store, listSessionsErr: errors.New("list failed")}))
	_, err = errAgent.listStoreSessions(ctx, ListSessionsRequest())
	require.ErrorContains(t, err, "list session store")
	loadErrAgent := NewAgent(WithSessionStore(&faultSessionStore{SessionStore: store, listSessions: summaries[:2], loadErr: errors.New("load failed")}))
	_, err = loadErrAgent.listStoreSessions(ctx, ListSessionsRequest())
	require.ErrorContains(t, err, "load failed")

	sessions := make([]acp.SessionInfo, listSessionsPageSize+1)
	for i := range sessions {
		sessions[i].SessionId = acp.SessionId(fmt.Sprintf("s-%02d", i))
	}
	page, next, err := paginateSessionInfos(sessions, nil)
	require.NoError(t, err)
	require.Len(t, page, listSessionsPageSize)
	require.NotNil(t, next)
	offset, err := decodeListCursor(next)
	require.NoError(t, err)
	require.Equal(t, listSessionsPageSize, offset)
	last, next, err := paginateSessionInfos(sessions, next)
	require.NoError(t, err)
	require.Len(t, last, 1)
	require.Nil(t, next)
	badCursor := "%%%"
	_, _, err = paginateSessionInfos(sessions, &badCursor)
	require.Error(t, err)
	past := encodeListCursor(len(sessions) + 1)
	_, _, err = paginateSessionInfos(sessions, &past)
	require.Error(t, err)
	negative := base64.RawURLEncoding.EncodeToString([]byte("-1"))
	_, err = decodeListCursor(&negative)
	require.Error(t, err)
	notNumber := base64.RawURLEncoding.EncodeToString([]byte("nope"))
	_, err = decodeListCursor(&notNumber)
	require.Error(t, err)
	require.Equal(t, "MA", encodeListCursor(0))
}

func TestPromptResultForObserverBranches(t *testing.T) {
	t.Parallel()

	result := promptResultForObserver(acp.PromptResponse{Usage: &acp.Usage{
		InputTokens:       1,
		OutputTokens:      2,
		TotalTokens:       3,
		CachedReadTokens:  acp.Ptr(4),
		CachedWriteTokens: acp.Ptr(5),
		ThoughtTokens:     acp.Ptr(6),
	}}, errors.New("prompt failed"), "sonnet")
	require.Equal(t, "sonnet", result.Model)
	require.Equal(t, 1, result.InputTokens)
	require.Equal(t, 5, result.CachedWriteTokens)
	require.Equal(t, 6, result.ThoughtTokens)
	require.ErrorContains(t, result.Err, "prompt failed")
}

func newStartedAgentSessionForTest(t *testing.T, agent *Agent, id acp.SessionId) (*agentSession, func()) {
	t.Helper()

	transport := newFakeClaudeTransport()
	client := claude.NewClient(nil, claude.Options{}, transport)
	require.NoError(t, client.Start(context.Background()))

	session := &agentSession{
		agent:         agent,
		id:            id,
		cwd:           t.TempDir(),
		client:        client,
		turn:          make(chan struct{}, sessionTurnCapacity),
		closeTurnWait: defaultSessionCloseTurnWait,
	}

	return session, func() { _ = client.Close() }
}
