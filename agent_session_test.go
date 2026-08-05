package claudeacp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/stretchr/testify/require"
)

func TestUnknownSessionInvalidParams(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	cwd := t.TempDir()
	sessionID := acp.SessionId("11111111-1111-4111-8111-111111111111")

	// Session-scoped methods that resolve an active session id.
	_, sessionErr := NewAgent().session("does-not-exist")
	requireUnknownSession(t, sessionErr)

	// Resume and load against an id that is not active and not in the store.
	_, resumeErr := NewAgent().
		ResumeSession(ctx, ResumeSessionRequest(sessionID, cwd))
	requireUnknownSession(t, resumeErr)

	_, loadErr := NewAgent().
		LoadSession(ctx, LoadSessionRequest(sessionID, cwd))
	requireUnknownSession(t, loadErr)
}

func TestNewSessionEdgeBranches(t *testing.T) {
	ctx := context.Background()

	_, err := NewAgent().NewSession(ctx, NewSessionRequest("relative"))
	require.ErrorContains(t, err, "absolute")

	previousUUIDRandom := uuidRandom
	uuidRandom = bytes.NewBuffer(nil)
	t.Cleanup(func() { uuidRandom = previousUUIDRandom })
	_, err = NewAgent().NewSession(ctx, NewSessionRequest(t.TempDir()))
	require.ErrorContains(t, err, "read random uuid")
	uuidRandom = previousUUIDRandom

	agent, conn, _ := newFakeLifecycleAgent(t, newFakeClaudeTransport())
	conn.sessionUpdateErr = errors.New("optional update failed")
	resp, err := agent.NewSession(ctx, NewSessionRequest(t.TempDir()))
	require.NoError(t, err)
	require.Contains(t, agent.sessions, resp.SessionId)
	require.Same(t, agent.store, agent.sessions[resp.SessionId].mirror.store)
}

func TestResumeSessionEdgeBranches(t *testing.T) {
	ctx := context.Background()
	cwd := t.TempDir()
	sessionID := acp.SessionId("11111111-1111-4111-8111-111111111111")

	_, err := NewAgent().ResumeSession(ctx, ResumeSessionRequest(sessionID, cwd, WithSessionMeta(map[string]any{
		claudeMetaKey: map[string]any{metaOptionsKey: "bad"},
	})))
	require.Error(t, err)

	_, err = NewAgent().ResumeSession(ctx, ResumeSessionRequest(sessionID, "relative"))
	require.ErrorContains(t, err, "absolute")

	loadErrAgent := NewAgent(WithSessionStore(&faultSessionStore{SessionStore: NewInMemorySessionStore(), loadErr: errors.New("load failed")}))
	_, err = loadErrAgent.ResumeSession(ctx, ResumeSessionRequest(sessionID, cwd))
	require.ErrorContains(t, err, "load failed")

	_, err = NewAgent(WithSessionStore(NewInMemorySessionStore())).ResumeSession(ctx, ResumeSessionRequest(sessionID, cwd))
	requireUnknownSession(t, err)

	closed := NewAgent(WithHome(t.TempDir()))
	require.NoError(t, closed.Close())
	_, err = closed.ResumeSession(ctx, ResumeSessionRequest(sessionID, cwd))
	require.ErrorIs(t, err, errAgentClosed)

	missingTransport := newFakeClaudeTransport()
	missingTransport.startErr = claude.ErrSessionNotFound
	missingStore := NewInMemorySessionStore()
	require.NoError(t, missingStore.Append(ctx, SessionKey{SessionID: string(sessionID)}, []SessionStoreEntry{[]byte(`{"type":"user"}`)}))
	missing, _, _ := newFakeLifecycleAgent(t, missingTransport, WithSessionStore(missingStore))
	_, err = missing.ResumeSession(ctx, ResumeSessionRequest(sessionID, cwd))
	requireUnknownSession(t, err)

	startErrTransport := newFakeClaudeTransport()
	startErrTransport.startErr = errors.New("start failed")
	startErrAgent, _, _ := newFakeLifecycleAgent(t, startErrTransport, WithSessionStore(missingStore))
	_, err = startErrAgent.ResumeSession(ctx, ResumeSessionRequest(sessionID, cwd))
	require.ErrorContains(t, err, "start failed")

	active, activeConn, _ := newFakeLifecycleAgent(t, newFakeClaudeTransport())
	newResp, err := active.NewSession(ctx, NewSessionRequest(cwd))
	require.NoError(t, err)
	activeConn.sessionUpdateErr = errors.New("active update failed")
	_, err = active.ResumeSession(ctx, ResumeSessionRequest(newResp.SessionId, cwd))
	require.NoError(t, err)
	activeConn.sessionUpdateErr = nil
	require.NoError(t, active.Close())

	resumed, _, _ := newFakeLifecycleAgent(t, newFakeClaudeTransport(), WithSessionStore(missingStore))
	resp, err := resumed.ResumeSession(ctx, ResumeSessionRequest(sessionID, cwd))
	require.NoError(t, err)
	require.NotEmpty(t, resp.ConfigOptions)
	require.NoError(t, resumed.Close())

	emitStore := NewInMemorySessionStore()
	require.NoError(t, emitStore.Append(ctx, SessionKey{SessionID: "22222222-2222-4222-8222-222222222222"}, []SessionStoreEntry{[]byte(`{"type":"user"}`)}))
	emitFail, emitConn, _ := newFakeLifecycleAgent(t, newFakeClaudeTransport(), WithSessionStore(emitStore))
	emitConn.sessionUpdateErr = errors.New("resume update failed")
	resp, err = emitFail.ResumeSession(ctx, ResumeSessionRequest("22222222-2222-4222-8222-222222222222", cwd))
	require.NoError(t, err)
	require.NotEmpty(t, resp.ConfigOptions)

	backpressureStore := NewInMemorySessionStore()
	require.NoError(t, backpressureStore.Append(ctx, SessionKey{SessionID: "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"}, []SessionStoreEntry{[]byte(`{"type":"user"}`)}))
	backpressure, _, _ := newFakeLifecycleAgent(t, newFakeClaudeTransport(), WithSessionStore(backpressureStore), WithConcurrencyLimits(ConcurrencyLimits{MaxActiveSessions: 1}))
	existing, cleanup := newStartedAgentSessionForTest(t, backpressure, "existing")
	defer cleanup()
	backpressure.sessions[existing.id] = existing
	_, err = backpressure.ResumeSession(ctx, ResumeSessionRequest("aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa", cwd))
	require.ErrorContains(t, err, "active_sessions")
}

func TestLoadSessionEdgeBranches(t *testing.T) {
	ctx := context.Background()
	cwd := t.TempDir()
	sessionID := acp.SessionId("33333333-3333-4333-8333-333333333333")

	_, err := NewAgent().LoadSession(ctx, LoadSessionRequest(sessionID, cwd, WithSessionMeta(map[string]any{
		claudeMetaKey: map[string]any{metaOptionsKey: "bad"},
	})))
	require.Error(t, err)

	_, err = NewAgent().LoadSession(ctx, LoadSessionRequest(sessionID, "relative"))
	require.ErrorContains(t, err, "absolute")

	loadErrAgent := NewAgent(WithSessionStore(&faultSessionStore{SessionStore: NewInMemorySessionStore(), loadErr: errors.New("load failed")}))
	_, err = loadErrAgent.LoadSession(ctx, LoadSessionRequest(sessionID, cwd))
	require.ErrorContains(t, err, "load failed")

	_, err = NewAgent(WithSessionStore(NewInMemorySessionStore())).LoadSession(ctx, LoadSessionRequest(sessionID, cwd))
	requireUnknownSession(t, err)

	badHome := string([]byte{0})
	badHomeStore := NewInMemorySessionStore()
	require.NoError(t, badHomeStore.Append(ctx, SessionKey{SessionID: string(sessionID)}, []SessionStoreEntry{[]byte(`{"type":"user"}`)}))
	_, err = NewAgent(WithHome(badHome), WithSessionStore(badHomeStore)).LoadSession(ctx, LoadSessionRequest(sessionID, cwd))
	require.Error(t, err)

	_, err = NewAgent(WithHome(t.TempDir())).LoadSession(ctx, LoadSessionRequest(sessionID, cwd))
	requireUnknownSession(t, err)

	store := NewInMemorySessionStore()
	require.NoError(t, store.Append(ctx, SessionKey{SessionID: string(sessionID)}, []SessionStoreEntry{
		[]byte(`{"type":"user","cwd":"` + filepath.ToSlash(cwd) + `","message":{"content":"hello"}}`),
	}))
	countedStore := &mainLoadCountingStore{SessionStore: store}
	loaded, _, _ := newFakeLifecycleAgent(t, newFakeClaudeTransport(), WithSessionStore(countedStore))
	resp, err := loaded.LoadSession(ctx, LoadSessionRequest(sessionID, cwd))
	require.NoError(t, err)
	require.NotEmpty(t, resp.ConfigOptions)
	require.Equal(t, 1, countedStore.loads)
	require.NoError(t, loaded.Close())

	nativeHome := t.TempDir()
	nativeID := acp.SessionId("99999999-9999-4999-8999-999999999999")
	nativePath := writeNativeTranscript(t, nativeHome, cwd, nativeID)
	nativeLoaded, _, _ := newFakeLifecycleAgent(t, newFakeClaudeTransport(), WithHome(nativeHome))
	_, err = nativeLoaded.LoadSession(ctx, LoadSessionRequest(nativeID, cwd))
	requireUnknownSession(t, err)
	require.NoFileExists(t, nativePath)
	nativePath = writeNativeTranscript(t, nativeHome, cwd, nativeID)
	_, err = nativeLoaded.ResumeSession(ctx, ResumeSessionRequest(nativeID, cwd))
	requireUnknownSession(t, err)
	require.NoFileExists(t, nativePath)
	require.NoError(t, nativeLoaded.Close())

	closedStore := NewInMemorySessionStore()
	require.NoError(t, closedStore.Append(ctx, SessionKey{SessionID: "77777777-7777-4777-8777-777777777777"}, []SessionStoreEntry{[]byte(`{"type":"system"}`)}))
	closed := NewAgent(WithHome(t.TempDir()), WithSessionStore(closedStore))
	require.NoError(t, closed.Close())
	_, err = closed.LoadSession(ctx, LoadSessionRequest("77777777-7777-4777-8777-777777777777", cwd))
	require.ErrorIs(t, err, errAgentClosed)

	missingTransport := newFakeClaudeTransport()
	missingTransport.startErr = claude.ErrSessionNotFound
	missingStart, _, _ := newFakeLifecycleAgent(t, missingTransport, WithSessionStore(store))
	_, err = missingStart.LoadSession(ctx, LoadSessionRequest(sessionID, cwd))
	requireUnknownSession(t, err)

	genericTransport := newFakeClaudeTransport()
	genericTransport.startErr = errors.New("load start failed")
	genericStart, _, _ := newFakeLifecycleAgent(t, genericTransport, WithSessionStore(store))
	_, err = genericStart.LoadSession(ctx, LoadSessionRequest(sessionID, cwd))
	require.ErrorContains(t, err, "load start failed")

	backpressure, _, _ := newFakeLifecycleAgent(t, newFakeClaudeTransport(), WithSessionStore(store), WithConcurrencyLimits(ConcurrencyLimits{MaxActiveSessions: 1}))
	existing, cleanup := newStartedAgentSessionForTest(t, backpressure, "existing-load")
	defer cleanup()
	backpressure.sessions[existing.id] = existing
	_, err = backpressure.LoadSession(ctx, LoadSessionRequest(sessionID, cwd))
	require.ErrorContains(t, err, "active_sessions")

	envNativeHome := t.TempDir()
	projectKey, err := projectKeyForDirectory(cwd)
	require.NoError(t, err)
	envNativePath := filepath.Join(envNativeHome, "projects", projectKey, "88888888-8888-4888-8888-888888888888.jsonl")
	require.NoError(t, os.MkdirAll(filepath.Dir(envNativePath), 0o755))
	require.NoError(t, os.WriteFile(envNativePath, []byte("{}\n"), 0o600))
	envSkipStore := NewInMemorySessionStore()
	require.NoError(t, envSkipStore.Append(ctx, SessionKey{SessionID: "88888888-8888-4888-8888-888888888888"}, []SessionStoreEntry{[]byte(`{"type":"system"}`)}))
	envSkip, _, _ := newFakeLifecycleAgent(t, newFakeClaudeTransport(), WithSessionStore(envSkipStore), WithHome(envNativeHome))
	_, err = envSkip.LoadSession(ctx, LoadSessionRequest("88888888-8888-4888-8888-888888888888", cwd))
	require.NoError(t, err)
	require.NotNil(t, envSkip.sessions["88888888-8888-4888-8888-888888888888"].materialized)

	emptyUpdateStore := NewInMemorySessionStore()
	require.NoError(t, emptyUpdateStore.Append(ctx, SessionKey{SessionID: "44444444-4444-4444-8444-444444444444"}, []SessionStoreEntry{
		[]byte(`{"type":"system","subtype":"init"}`),
	}))
	emitFail, emitConn, _ := newFakeLifecycleAgent(t, newFakeClaudeTransport(), WithSessionStore(emptyUpdateStore))
	emitConn.sessionUpdateErr = errors.New("load update failed")
	loadResp, err := emitFail.LoadSession(ctx, LoadSessionRequest("44444444-4444-4444-8444-444444444444", cwd))
	require.NoError(t, err)
	require.NotEmpty(t, loadResp.ConfigOptions)

	replayErrStore := NewInMemorySessionStore()
	require.NoError(t, replayErrStore.Append(ctx, SessionKey{SessionID: "55555555-5555-4555-8555-555555555555"}, []SessionStoreEntry{
		[]byte(`{"type":"user","message":{"content":"replay me"}}`),
	}))
	replayErrAgent, replayErrConn, _ := newFakeLifecycleAgent(t, newFakeClaudeTransport(), WithSessionStore(replayErrStore))
	replayErrConn.sessionUpdateErr = errors.New("replay update failed")
	_, err = replayErrAgent.LoadSession(ctx, LoadSessionRequest("55555555-5555-4555-8555-555555555555", cwd))
	require.ErrorContains(t, err, "replay update failed")
	require.Empty(t, replayErrAgent.sessions)
}

func TestActiveSessionMissingStoreLoadPreservesNativeTranscriptAndPrompt(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	cwd := t.TempDir()
	transport := newFakeClaudeTransport()
	agent, _, _ := newFakeLifecycleAgent(t, transport, WithHome(home))

	newResp, err := agent.NewSession(ctx, NewSessionRequest(cwd))
	require.NoError(t, err)
	active := agent.sessions[newResp.SessionId]
	nativePath := writeNativeTranscript(t, home, cwd, newResp.SessionId)

	_, err = agent.LoadSession(ctx, LoadSessionRequest(newResp.SessionId, cwd))
	requireUnknownSession(t, err)
	require.Same(t, active, agent.sessions[newResp.SessionId])
	require.FileExists(t, nativePath)
	require.Zero(t, transport.CloseCalls())

	promptResp, err := agent.Prompt(ctx, TextPromptRequest(newResp.SessionId, "after-failed-load", "still bound"))
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, promptResp.StopReason)
	require.Same(t, active, agent.sessions[newResp.SessionId])
	require.FileExists(t, nativePath)

	require.NoError(t, agent.Close())
}

func TestListPromptCloseAndDeleteEdgeBranches(t *testing.T) {
	ctx := context.Background()
	cwd := t.TempDir()
	sessionID := acp.SessionId("55555555-5555-4555-8555-555555555555")

	_, err := NewAgent().ListSessions(ctx, ListSessionsRequest(WithListSessionsCwd("relative")))
	require.ErrorContains(t, err, "absolute")

	badHome := string([]byte{0})
	badHomeResp, err := NewAgent(WithHome(badHome)).ListSessions(ctx, ListSessionsRequest())
	require.NoError(t, err)
	require.Empty(t, badHomeResp.Sessions)

	cursor := "bad"
	_, err = NewAgent(WithSessionStore(NewInMemorySessionStore())).ListSessions(ctx, acp.ListSessionsRequest{Cursor: &cursor})
	require.Error(t, err)
	require.False(t, sessionMatchesListFilters(&agentSession{cwd: cwd}, ListSessionsRequest(WithListSessionsCwd(filepath.Join(cwd, "other")))))

	filterAgent := NewAgent(WithSessionStore(NewInMemorySessionStore()))
	filterSession, filterCleanup := newStartedAgentSessionForTest(t, filterAgent, "filter")
	defer filterCleanup()
	filterSession.cwd = filepath.Join(cwd, "mismatch")
	filterAgent.sessions[filterSession.id] = filterSession
	filtered, err := filterAgent.ListSessions(ctx, ListSessionsRequest(WithListSessionsCwd(cwd)))
	require.NoError(t, err)
	require.Empty(t, filtered.Sessions)

	listErrAgent := NewAgent(WithSessionStore(&faultSessionStore{SessionStore: NewInMemorySessionStore(), listSessionsErr: errors.New("list failed")}))
	_, err = listErrAgent.ListSessions(ctx, ListSessionsRequest())
	require.ErrorContains(t, err, "list failed")

	listStore := NewInMemorySessionStore()
	dupID := "66666666-6666-4666-8666-666666666666"
	require.NoError(t, listStore.Append(ctx, SessionKey{SessionID: dupID}, []SessionStoreEntry{[]byte(`{"type":"user","message":{"content":"stored"}}`)}))
	listAgent := NewAgent(WithHome(t.TempDir()), WithSessionStore(&faultSessionStore{
		SessionStore: listStore,
		listSessions: []SessionSummary{
			{SessionID: dupID, UpdatedAtUnixMilli: 1, Cwd: ""},
			{SessionID: "not-a-uuid", UpdatedAtUnixMilli: 2},
		},
	}))
	activeSession, activeCleanup := newStartedAgentSessionForTest(t, listAgent, acp.SessionId(dupID))
	defer activeCleanup()
	activeSession.cwd = cwd
	listAgent.sessions[activeSession.id] = activeSession
	listResp, err := listAgent.ListSessions(ctx, ListSessionsRequest(WithListSessionsCwd(cwd)))
	require.NoError(t, err)
	require.Len(t, listResp.Sessions, 1)
	require.Equal(t, acp.SessionId(dupID), listResp.Sessions[0].SessionId)

	nativeHome := t.TempDir()
	nativeID := acp.SessionId("77777777-7777-4777-8777-777777777777")
	writeNativeTranscript(t, nativeHome, cwd, nativeID)
	nativeListAgent := NewAgent(WithHome(nativeHome))
	nativeResp, err := nativeListAgent.ListSessions(ctx, ListSessionsRequest(WithListSessionsCwd(cwd)))
	require.NoError(t, err)
	require.Empty(t, nativeResp.Sessions)
	activeNative, activeNativeCleanup := newStartedAgentSessionForTest(t, nativeListAgent, nativeID)
	defer activeNativeCleanup()
	activeNative.cwd = cwd
	nativeListAgent.sessions[nativeID] = activeNative
	nativeResp, err = nativeListAgent.ListSessions(ctx, ListSessionsRequest(WithListSessionsCwd(cwd)))
	require.NoError(t, err)
	require.Len(t, nativeResp.Sessions, 1)

	storeOnlyID := "88888888-8888-4888-8888-888888888888"
	storeOnly := NewInMemorySessionStore()
	require.NoError(t, storeOnly.Append(ctx, SessionKey{SessionID: storeOnlyID}, []SessionStoreEntry{[]byte(`{"type":"user","message":{"content":"store only"}}`)}))
	storeOnlyResp, err := NewAgent(WithSessionStore(storeOnly)).ListSessions(ctx, ListSessionsRequest(WithListSessionsCwd(cwd)))
	require.NoError(t, err)
	require.Len(t, storeOnlyResp.Sessions, 1)

	fatalAgent := NewAgent()
	fatalAgent.sessions[sessionID] = &agentSession{
		agent:  fatalAgent,
		id:     sessionID,
		cwd:    cwd,
		turn:   make(chan struct{}, 1),
		client: claude.NewClient(nil, claude.Options{}, newFakeClaudeTransport()),
	}
	_, err = fatalAgent.Prompt(ctx, TextPromptRequest(sessionID, "test-turn", "hello"))
	require.Error(t, err)
	// A native turn failure leaves the session addressable and retriable: it is
	// never removed from the map, so a follow-up prompt cannot return the
	// unknown-session error.
	require.Contains(t, fatalAgent.sessions, sessionID)

	_, err = NewAgent().CloseSession(ctx, acp.CloseSessionRequest{SessionId: sessionID})
	require.Error(t, err)

	deleteErrAgent := NewAgent(WithSessionStore(&faultSessionStore{SessionStore: NewInMemorySessionStore(), deleteErr: errors.New("delete failed")}))
	_, err = deleteErrAgent.UnstableDeleteSession(ctx, DeleteSessionRequest(sessionID))
	require.ErrorContains(t, err, "delete failed")

	previousDelete := deleteNativeTranscript
	t.Cleanup(func() { deleteNativeTranscript = previousDelete })
	deleteNativeTranscript = func(context.Context, string, string) error { return nil }
	successDelete := NewAgent(WithSessionStore(NewInMemorySessionStore()))
	_, err = successDelete.UnstableDeleteSession(ctx, DeleteSessionRequest(sessionID))
	require.NoError(t, err)

	activeDelete, _, _ := newFakeLifecycleAgent(t, newFakeClaudeTransport(), WithSessionStore(NewInMemorySessionStore()))
	closeTransport := newFakeClaudeTransport()
	closeClient := claude.NewClient(nil, claude.Options{}, closeTransport)
	require.NoError(t, closeClient.Start(ctx))
	closeTransport.closeErr = errors.New("close failed")
	activeDelete.sessions[sessionID] = &agentSession{
		agent:         activeDelete,
		id:            sessionID,
		cwd:           cwd,
		client:        closeClient,
		turn:          make(chan struct{}, 1),
		closeTurnWait: defaultSessionCloseTurnWait,
	}
	_, err = activeDelete.UnstableDeleteSession(ctx, DeleteSessionRequest(sessionID))
	require.ErrorContains(t, err, "close failed")
}

func TestSessionMapCleanupCloseErrors(t *testing.T) {
	ctx := context.Background()
	agent := NewAgent(WithConcurrencyLimits(ConcurrencyLimits{MaxActiveSessions: 1}))
	closeErr := errors.New("close failed")

	removedSession, removedCleanup := newStartedAgentSessionForTest(t, agent, "removed")
	defer removedCleanup()
	removedSession.client = startedClientWithCloseError(t, closeErr)
	agent.sessions[removedSession.id] = removedSession
	agent.removeSession(ctx, removedSession.id, removedSession)

	closedAgent := NewAgent()
	closedAgent.closed = true
	rejected, rejectedCleanup := newStartedAgentSessionForTest(t, closedAgent, "rejected")
	defer rejectedCleanup()
	rejected.client = startedClientWithCloseError(t, closeErr)
	require.ErrorIs(t, closedAgent.storeStartedSession(ctx, rejected), errAgentClosed)

	limitAgent := NewAgent(WithConcurrencyLimits(ConcurrencyLimits{MaxActiveSessions: 1}))
	first, firstCleanup := newStartedAgentSessionForTest(t, limitAgent, "first")
	defer firstCleanup()
	require.NoError(t, limitAgent.storeStartedSession(ctx, first))
	backpressured, backpressuredCleanup := newStartedAgentSessionForTest(t, limitAgent, "second")
	defer backpressuredCleanup()
	backpressured.client = startedClientWithCloseError(t, closeErr)
	require.Error(t, limitAgent.storeStartedSession(ctx, backpressured))

	replacementAgent := NewAgent()
	oldSession, oldCleanup := newStartedAgentSessionForTest(t, replacementAgent, "same")
	defer oldCleanup()
	oldSession.client = startedClientWithCloseError(t, closeErr)
	newSession, newCleanup := newStartedAgentSessionForTest(t, replacementAgent, "same")
	defer newCleanup()
	require.NoError(t, replacementAgent.storeStartedSession(ctx, oldSession))
	require.NoError(t, replacementAgent.storeStartedSession(ctx, newSession))
}

func newFakeLifecycleAgent(t *testing.T, transport *fakeClaudeTransport, opts ...Option) (*Agent, *recordingAgentClient, *fakeClaudeTransport) {
	t.Helper()

	if transport == nil {
		transport = newFakeClaudeTransport()
	}

	options := append([]Option{WithHome(t.TempDir())}, opts...)
	agent := NewAgent(options...)
	conn := newRecordingAgentClient()
	agent.setConnection(conn)
	installFakeClaudeClient(agent, transport)

	return agent, conn, transport
}

func startedClientWithCloseError(t *testing.T, closeErr error) *claude.Client {
	t.Helper()

	transport := newFakeClaudeTransport()
	client := claude.NewClient(nil, claude.Options{}, &closeErrTransport{Transport: transport, err: closeErr})
	require.NoError(t, client.Start(context.Background()))

	return client
}

type closeErrTransport struct {
	claude.Transport
	err error
}

func (t *closeErrTransport) Close() error {
	return t.err
}

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

func TestStoredSessionEntries(t *testing.T) {
	ctx := context.Background()
	sessionID := acp.SessionId("11111111-1111-4111-8111-111111111111")
	store := NewInMemorySessionStore()
	agent := NewAgent(WithSessionStore(store))

	_, err := NewAgent().storedSessionEntries(ctx, sessionID)
	requireUnknownSession(t, err)
	_, err = agent.storedSessionEntries(ctx, sessionID)
	requireUnknownSession(t, err)

	require.NoError(t, store.Append(ctx, SessionKey{SessionID: string(sessionID)}, []SessionStoreEntry{[]byte(`{"type":"user"}`)}))
	entries, err := agent.storedSessionEntries(ctx, sessionID)
	require.NoError(t, err)
	require.Len(t, entries, 1)

	agent.deleted[sessionID] = struct{}{}
	previousDeleteNativeTranscript := deleteNativeTranscript
	deleteNativeTranscript = func(context.Context, string, string) error { return errors.New("cleanup failed") }
	t.Cleanup(func() { deleteNativeTranscript = previousDeleteNativeTranscript })
	_, err = agent.storedSessionEntries(ctx, sessionID)
	requireUnknownSession(t, err)
	deleteNativeTranscript = previousDeleteNativeTranscript

	errAgent := NewAgent(WithSessionStore(&faultSessionStore{SessionStore: NewInMemorySessionStore(), loadErr: errors.New("load failed")}))
	_, err = errAgent.storedSessionEntries(ctx, sessionID)
	require.ErrorContains(t, err, "load failed")
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

type mainLoadCountingStore struct {
	SessionStore
	loads int
}

func (s *mainLoadCountingStore) Load(ctx context.Context, key SessionKey) ([]SessionStoreEntry, error) {
	if key.Subpath == SessionStoreMainSubpath {
		s.loads++
	}

	return s.SessionStore.Load(ctx, key)
}

func TestStartSessionEdgeBranches(t *testing.T) {
	ctx := context.Background()
	cwd := t.TempDir()
	sessionID := acp.SessionId("12121212-1212-4212-8212-121212121212")

	homeFile := filepath.Join(t.TempDir(), "not-dir")
	require.NoError(t, os.WriteFile(homeFile, []byte("x"), 0o600))
	_, err := NewAgent(WithHome(homeFile)).startSession(ctx, sessionID, sessionStart{Cwd: cwd})
	require.ErrorContains(t, err, "not a directory")

	unauthorizedResume, _, _ := newFakeLifecycleAgent(t, newFakeClaudeTransport())
	_, err = unauthorizedResume.startSession(ctx, sessionID, sessionStart{Cwd: cwd, ResumeID: string(sessionID)})
	requireUnknownSession(t, err)

	blockedScratch := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(blockedScratch, []byte("x"), 0o600))
	materializeErrAgent, _, _ := newFakeLifecycleAgent(t, newFakeClaudeTransport(), WithScratchDir(blockedScratch))
	_, err = materializeErrAgent.startSession(ctx, sessionID, sessionStart{
		Cwd:          cwd,
		ResumeID:     string(sessionID),
		StoreEntries: []SessionStoreEntry{[]byte(`{"type":"user"}`)},
	})
	require.ErrorContains(t, err, "create scratch parent dir")

	imageListErrAgent, _, _ := newFakeLifecycleAgent(
		t,
		newFakeClaudeTransport(),
		WithSessionStore(&faultSessionStore{
			SessionStore:   NewInMemorySessionStore(),
			listSubkeysErr: errors.New("image list failed"),
		}),
	)
	_, err = imageListErrAgent.startSession(ctx, sessionID, sessionStart{
		Cwd:          cwd,
		ResumeID:     string(sessionID),
		StoreEntries: []SessionStoreEntry{[]byte(`{"type":"user"}`)},
	})
	require.ErrorContains(t, err, "list image artifacts")

	missingArtifactAgent, _, _ := newFakeLifecycleAgent(
		t,
		newFakeClaudeTransport(),
		WithSessionStore(NewInMemorySessionStore()),
	)
	_, err = missingArtifactAgent.startSession(ctx, sessionID, sessionStart{
		Cwd:      cwd,
		ResumeID: string(sessionID),
		StoreEntries: []SessionStoreEntry{[]byte(
			`{"type":"assistant","message":{"content":[{"type":"image","source":{"type":"acp_artifact","artifact_key":"missing"}}]}}`,
		)},
	})
	requireImageOutputError(t, err, imageOutputStorageFailed)

	originalMaterializeMkdirTemp := materializeMkdirTemp
	materializeMkdirTemp = func(string, string) (string, error) {
		return "", errors.New("materialize temp failed")
	}
	materializeTempErrAgent, _, _ := newFakeLifecycleAgent(
		t,
		newFakeClaudeTransport(),
		WithSessionStore(NewInMemorySessionStore()),
	)
	_, err = materializeTempErrAgent.startSession(ctx, sessionID, sessionStart{
		Cwd:          cwd,
		ResumeID:     string(sessionID),
		StoreEntries: []SessionStoreEntry{[]byte(`{"type":"user"}`)},
	})
	require.ErrorContains(t, err, "materialize temp failed")
	materializeMkdirTemp = originalMaterializeMkdirTemp

	forkStore := NewInMemorySessionStore()
	png := outputFixtureBase64(t, "valid.png")
	decoded, err := base64.StdEncoding.DecodeString(png)
	require.NoError(t, err)
	forkArtifact := storedImageArtifact{
		Version:     imageArtifactVersion,
		Identity:    "agent:message:0",
		Fingerprint: imageFingerprint(decoded),
		MimeType:    "image/png",
		Data:        png,
		CreatedAt:   imageArtifactNow().UnixMilli(),
	}
	forkRaw, err := json.Marshal(forkArtifact)
	require.NoError(t, err)
	require.NoError(t, forkStore.Append(ctx, SessionKey{
		SessionID: "parent",
		Subpath:   imageArtifactKey(forkArtifact.Identity, forkArtifact.Fingerprint),
	}, []SessionStoreEntry{forkRaw}))
	failingForkStore := &faultSessionStore{
		SessionStore: forkStore,
		appendErr:    errors.New("fork image append failed"),
	}
	forkAgent, _, _ := newFakeLifecycleAgent(
		t,
		newFakeClaudeTransport(),
		WithSessionStore(failingForkStore),
	)
	_, err = forkAgent.startSession(ctx, sessionID, sessionStart{
		Cwd:          cwd,
		ResumeID:     "parent",
		StoreEntries: []SessionStoreEntry{[]byte(`{"type":"user"}`)},
		ForkSession:  true,
	})
	require.ErrorContains(t, err, "store forked image artifact")

	modelConfigErr, _, _ := newFakeLifecycleAgent(t, newFakeClaudeTransport(), WithEnv(map[string]string{envClaudeModelConfig: "[]"}))
	_, err = modelConfigErr.startSession(ctx, sessionID, sessionStart{Cwd: cwd})
	require.ErrorContains(t, err, envClaudeModelConfig)

	settingsErrTransport := newFakeClaudeTransport()
	settingsErrTransport.controlErr = map[string]error{"get_settings": errors.New("settings failed")}
	settingsErrAgent, _, _ := newFakeLifecycleAgent(t, settingsErrTransport)
	session, err := settingsErrAgent.startSession(ctx, sessionID, sessionStart{Cwd: cwd})
	require.NoError(t, err)
	require.False(t, session.fastModeKnown)
	require.NoError(t, session.Close(ctx))

	allowlistTransport := newFakeClaudeTransport()
	allowlistAgent, _, _ := newFakeLifecycleAgent(t, allowlistTransport, WithEnv(map[string]string{
		envClaudeModelConfig: `{"availableModels":["opus"]}`,
	}))
	session, err = allowlistAgent.startSession(ctx, sessionID, sessionStart{Cwd: cwd})
	require.NoError(t, err)
	require.Len(t, session.availableModels, 2)
	require.Equal(t, modelDefault, session.availableModels[0].Value)
	require.Equal(t, "opus", session.availableModels[1].Value)
	require.NoError(t, session.Close(ctx))

	settingsCwd := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(settingsCwd, settingsDirName), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(settingsCwd, settingsDirName, settingsFileName), []byte(`{
		"permissions": {"defaultMode": "acceptEdits"},
		"effortLevel": "medium"
	}`), 0o600))
	discoveredAgent, _, _ := newFakeLifecycleAgent(t, newFakeClaudeTransport())
	session, err = discoveredAgent.startSession(ctx, sessionID, sessionStart{Cwd: settingsCwd})
	require.NoError(t, err)
	require.Equal(t, modeAcceptEdits, session.mode)
	require.NoError(t, session.Close(ctx))

	metaAgent, _, _ := newFakeLifecycleAgent(t, newFakeClaudeTransport())
	session, err = metaAgent.startSession(ctx, sessionID, sessionStart{Cwd: cwd, MetaOptions: ClaudeOptions{PermissionMode: string(modePlan)}})
	require.NoError(t, err)
	require.Equal(t, modePlan, session.mode)
	require.NoError(t, session.Close(ctx))

	previousGeteuid := osGeteuid
	previousSandbox, hadSandbox := os.LookupEnv("IS_SANDBOX")
	osGeteuid = func() int { return 0 }
	require.NoError(t, os.Unsetenv("IS_SANDBOX"))
	t.Cleanup(func() {
		osGeteuid = previousGeteuid
		if hadSandbox {
			require.NoError(t, os.Setenv("IS_SANDBOX", previousSandbox))
		} else {
			require.NoError(t, os.Unsetenv("IS_SANDBOX"))
		}
	})
	bypassAgent, _, _ := newFakeLifecycleAgent(t, newFakeClaudeTransport(), WithClaudeDefaultPermissionMode(permissionModeBypassPermissions))
	session, err = bypassAgent.startSession(ctx, sessionID, sessionStart{Cwd: cwd})
	require.NoError(t, err)
	require.Equal(t, modeDefault, session.mode)
	require.NoError(t, session.Close(ctx))
	osGeteuid = previousGeteuid
	if hadSandbox {
		require.NoError(t, os.Setenv("IS_SANDBOX", previousSandbox))
	} else {
		require.NoError(t, os.Unsetenv("IS_SANDBOX"))
	}

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	permissionErrAgent, _, _ := newFakeLifecycleAgent(t, newFakeClaudeTransport())
	_, err = permissionErrAgent.startSession(cancelled, sessionID, sessionStart{Cwd: cwd})
	require.ErrorIs(t, err, context.Canceled)

	setModelErrTransport := newFakeClaudeTransport()
	setModelErrTransport.controlErr = map[string]error{"set_model": errors.New("set model failed")}
	setModelErrAgent, _, _ := newFakeLifecycleAgent(t, setModelErrTransport, WithEnv(map[string]string{envAnthropicModel: "opus"}))
	_, err = setModelErrAgent.startSession(ctx, sessionID, sessionStart{Cwd: cwd})
	require.ErrorContains(t, err, "set model failed")

	reconcileTransport := newFakeClaudeTransport()
	reconcileTransport.settings = map[string]any{
		"applied":   map[string]any{"model": "sonnet", "effort": "medium"},
		"effective": map[string]any{"fastMode": true},
	}
	reconcileAgent, _, _ := newFakeLifecycleAgent(t, reconcileTransport)
	session, err = reconcileAgent.startSession(ctx, sessionID, sessionStart{Cwd: cwd})
	require.NoError(t, err)
	require.Equal(t, effortHigh, session.effort)
	require.NoError(t, session.Close(ctx))

	modeClampTransport := newFakeClaudeTransport()
	modeClampTransport.initialize = map[string]any{
		"models": []any{
			map[string]any{"value": "opus", "displayName": "Opus"},
		},
	}
	modeClampAgent, _, _ := newFakeLifecycleAgent(t, modeClampTransport, WithClaudeDefaultPermissionMode("auto"))
	session, err = modeClampAgent.startSession(ctx, sessionID, sessionStart{Cwd: cwd, MetaOptions: ClaudeOptions{Model: "opus"}})
	require.NoError(t, err)
	require.Equal(t, modeDefault, session.mode)
	require.NoError(t, session.Close(ctx))

	modeApplyErrTransport := newFakeClaudeTransport()
	modeApplyErrTransport.initialize = modeClampTransport.initialize
	modeApplyErrTransport.controlErr = map[string]error{"set_permission_mode": errors.New("set default mode failed")}
	modeApplyErrAgent, _, _ := newFakeLifecycleAgent(t, modeApplyErrTransport, WithClaudeDefaultPermissionMode("auto"))
	_, err = modeApplyErrAgent.startSession(ctx, sessionID, sessionStart{Cwd: cwd, MetaOptions: ClaudeOptions{Model: "opus"}})
	require.ErrorContains(t, err, "set default mode failed")
}

func TestStartSessionIsolationFailureBranches(t *testing.T) {
	ctx := t.Context()
	cwd := t.TempDir()
	sessionID := acp.SessionId("34343434-3434-4434-8434-343434343434")
	uid, gid := uint32(os.Geteuid()), uint32(os.Getegid())
	if uid == 0 {
		uid = 1
	}
	if gid == 0 {
		gid = 1
	}
	isolation := ProcessIsolation{UID: uid, GID: gid, BaseEnvironment: map[string]string{"PATH": "/usr/bin:/bin"}}

	originalEnsure := sessionEnsureScratchParent
	originalHandoff := sessionHandoffGeneratedNativeTree
	originalValidate := sessionValidateNativeOwnedDirectory
	originalMkdirTemp := materializeMkdirTemp
	originalCopy := copyClaudeConfigFiles
	t.Cleanup(func() {
		sessionEnsureScratchParent = originalEnsure
		sessionHandoffGeneratedNativeTree = originalHandoff
		sessionValidateNativeOwnedDirectory = originalValidate
		materializeMkdirTemp = originalMkdirTemp
		copyClaudeConfigFiles = originalCopy
	})
	restore := func() {
		sessionEnsureScratchParent = originalEnsure
		sessionHandoffGeneratedNativeTree = originalHandoff
		sessionValidateNativeOwnedDirectory = originalValidate
		materializeMkdirTemp = originalMkdirTemp
		copyClaudeConfigFiles = originalCopy
	}
	start := func(options []Option, request sessionStart) error {
		agent, _, _ := newFakeLifecycleAgent(t, newFakeClaudeTransport(), options...)
		_, err := agent.startSession(ctx, sessionID, request)

		return err
	}
	authAgent := newAuthAgent(t)
	authAgent.setConnection(newRecordingAgentClient())
	installFakeClaudeClient(authAgent, newFakeClaudeTransport())
	authSession, err := authAgent.startSession(ctx, sessionID, sessionStart{Cwd: cwd})
	require.NoError(t, err)
	require.NoError(t, authSession.Close(ctx))

	sessionValidateNativeOwnedDirectory = func(string, *ProcessIsolation) error { return errors.New("validate native home") }
	err = start(nil, sessionStart{Cwd: cwd})
	require.ErrorContains(t, err, "validate native home")
	restore()

	sessionEnsureScratchParent = func(string) (string, error) { return "", errors.New("isolated scratch") }
	err = start([]Option{WithProcessIsolation(isolation)}, sessionStart{Cwd: cwd})
	require.ErrorContains(t, err, "isolated scratch")
	restore()

	materializeMkdirTemp = func(string, string) (string, error) { return "", errors.New("isolated home") }
	err = start([]Option{WithProcessIsolation(isolation)}, sessionStart{Cwd: cwd})
	require.ErrorContains(t, err, "create isolated Claude home")
	restore()

	copyClaudeConfigFiles = func(string, string, claude.Options) error { return errors.New("copy isolated home") }
	err = start([]Option{WithProcessIsolation(isolation)}, sessionStart{Cwd: cwd})
	require.ErrorContains(t, err, "copy isolated home")
	restore()

	copyClaudeConfigFiles = func(string, string, claude.Options) error { return nil }
	sessionHandoffGeneratedNativeTree = func(string, *ProcessIsolation) error { return errors.New("handoff isolated home") }
	err = start([]Option{WithProcessIsolation(isolation)}, sessionStart{Cwd: cwd})
	require.ErrorContains(t, err, "handoff isolated home")
	restore()

	handoffs := 0
	copyClaudeConfigFiles = func(string, string, claude.Options) error { return nil }
	sessionHandoffGeneratedNativeTree = func(string, *ProcessIsolation) error {
		handoffs++
		if handoffs == 2 {
			return errors.New("handoff MCP config")
		}

		return nil
	}
	err = start([]Option{WithProcessIsolation(isolation)}, sessionStart{
		Cwd: cwd,
		McpServers: []acp.McpServer{{
			Stdio: &acp.McpServerStdio{Name: "fixture", Command: "/bin/true"},
		}},
	})
	require.ErrorContains(t, err, "handoff MCP config")
	require.Equal(t, 2, handoffs)
}

func TestSessionCloseReportsMCPConfigRemovalError(t *testing.T) {
	agent := NewAgent()
	session, cleanup := newStartedAgentSessionForTest(t, agent, "session-close-mcp")
	defer cleanup()
	parentFile := filepath.Join(t.TempDir(), "not-a-directory")
	require.NoError(t, os.WriteFile(parentFile, []byte("x"), 0o600))
	session.mcpConfigDir = filepath.Join(parentFile, "mcp")
	require.Error(t, session.Close(t.Context()))
}
