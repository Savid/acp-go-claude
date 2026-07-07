package claudeacp

import (
	"bytes"
	"context"
	"errors"
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
	_, resumeErr := NewAgent(WithSessionStore(NewInMemorySessionStore())).
		ResumeSession(ctx, ResumeSessionRequest(sessionID, cwd))
	requireUnknownSession(t, resumeErr)

	_, loadErr := NewAgent(WithSessionStore(NewInMemorySessionStore())).
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
	missing, _, _ := newFakeLifecycleAgent(t, missingTransport)
	_, err = missing.ResumeSession(ctx, ResumeSessionRequest(sessionID, cwd))
	requireUnknownSession(t, err)

	startErrTransport := newFakeClaudeTransport()
	startErrTransport.startErr = errors.New("start failed")
	startErrAgent, _, _ := newFakeLifecycleAgent(t, startErrTransport)
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

	resumed, _, _ := newFakeLifecycleAgent(t, newFakeClaudeTransport())
	resp, err := resumed.ResumeSession(ctx, ResumeSessionRequest(sessionID, cwd))
	require.NoError(t, err)
	require.NotEmpty(t, resp.ConfigOptions)
	require.NoError(t, resumed.Close())

	emitFail, emitConn, _ := newFakeLifecycleAgent(t, newFakeClaudeTransport())
	emitConn.sessionUpdateErr = errors.New("resume update failed")
	resp, err = emitFail.ResumeSession(ctx, ResumeSessionRequest("22222222-2222-4222-8222-222222222222", cwd))
	require.NoError(t, err)
	require.NotEmpty(t, resp.ConfigOptions)

	backpressure, _, _ := newFakeLifecycleAgent(t, newFakeClaudeTransport(), WithConcurrencyLimits(ConcurrencyLimits{MaxActiveSessions: 1}))
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
	_, err = NewAgent(WithHome(badHome)).LoadSession(ctx, LoadSessionRequest(sessionID, cwd))
	require.Error(t, err)

	_, err = NewAgent(WithHome(t.TempDir())).LoadSession(ctx, LoadSessionRequest(sessionID, cwd))
	requireUnknownSession(t, err)

	store := NewInMemorySessionStore()
	require.NoError(t, store.Append(ctx, SessionKey{SessionID: string(sessionID)}, []SessionStoreEntry{
		[]byte(`{"type":"user","cwd":"` + filepath.ToSlash(cwd) + `","message":{"content":"hello"}}`),
	}))
	loaded, _, _ := newFakeLifecycleAgent(t, newFakeClaudeTransport(), WithSessionStore(store))
	resp, err := loaded.LoadSession(ctx, LoadSessionRequest(sessionID, cwd))
	require.NoError(t, err)
	require.NotEmpty(t, resp.ConfigOptions)
	require.NoError(t, loaded.Close())

	nativeHome := t.TempDir()
	writeNativeTranscript(t, nativeHome, cwd, "99999999-9999-4999-8999-999999999999")
	nativeLoaded, _, _ := newFakeLifecycleAgent(t, newFakeClaudeTransport(), WithHome(nativeHome))
	resp, err = nativeLoaded.LoadSession(ctx, LoadSessionRequest("99999999-9999-4999-8999-999999999999", cwd))
	require.NoError(t, err)
	require.NotEmpty(t, resp.ConfigOptions)
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
	envSkip, _, _ := newFakeLifecycleAgent(t, newFakeClaudeTransport(), WithSessionStore(envSkipStore), WithEnv(map[string]string{"CLAUDE_CONFIG_DIR": envNativeHome}))
	_, err = envSkip.LoadSession(ctx, LoadSessionRequest("88888888-8888-4888-8888-888888888888", cwd))
	requireUnknownSession(t, err)

	activeStore := NewInMemorySessionStore()
	require.NoError(t, activeStore.Append(ctx, SessionKey{SessionID: string(sessionID)}, []SessionStoreEntry{[]byte(`{"type":"system"}`)}))
	active, _, _ := newFakeLifecycleAgent(t, newFakeClaudeTransport(), WithSessionStore(activeStore))
	active.sessions[sessionID] = &agentSession{
		agent:        active,
		id:           sessionID,
		cwd:          cwd,
		fingerprint:  sessionStartFingerprint(sessionStart{Cwd: cwd, ResumeID: string(sessionID)}),
		materialized: &materializedSession{mainPath: filepath.Join(t.TempDir(), "missing.jsonl")},
		client:       claude.NewClient(nil, claude.Options{}, newFakeClaudeTransport()),
		turn:         make(chan struct{}, sessionTurnCapacity),
	}
	_, err = active.LoadSession(ctx, LoadSessionRequest(sessionID, cwd))
	require.ErrorContains(t, err, "open transcript")

	previousCopy := copyClaudeConfigFiles
	copyClaudeConfigFiles = func(dst string, _ string, _ map[string]string) error {
		projectKey, keyErr := projectKeyForDirectory(cwd)
		require.NoError(t, keyErr)

		return os.Remove(filepath.Join(dst, "projects", projectKey, string(sessionID)+".jsonl"))
	}
	t.Cleanup(func() { copyClaudeConfigFiles = previousCopy })
	startedReplayErr, _, _ := newFakeLifecycleAgent(t, newFakeClaudeTransport(), WithSessionStore(store))
	_, err = startedReplayErr.LoadSession(ctx, LoadSessionRequest(sessionID, cwd))
	require.ErrorContains(t, err, "open transcript")
	require.Empty(t, startedReplayErr.sessions)
	copyClaudeConfigFiles = previousCopy

	emptyUpdateStore := NewInMemorySessionStore()
	require.NoError(t, emptyUpdateStore.Append(ctx, SessionKey{SessionID: "44444444-4444-4444-8444-444444444444"}, []SessionStoreEntry{
		[]byte(`{"type":"system","subtype":"init"}`),
	}))
	emitFail, emitConn, _ := newFakeLifecycleAgent(t, newFakeClaudeTransport(), WithSessionStore(emptyUpdateStore))
	emitConn.sessionUpdateErr = errors.New("load update failed")
	loadResp, err := emitFail.LoadSession(ctx, LoadSessionRequest("44444444-4444-4444-8444-444444444444", cwd))
	require.NoError(t, err)
	require.NotEmpty(t, loadResp.ConfigOptions)
}

func TestListPromptCloseAndDeleteEdgeBranches(t *testing.T) {
	ctx := context.Background()
	cwd := t.TempDir()
	sessionID := acp.SessionId("55555555-5555-4555-8555-555555555555")

	_, err := NewAgent().ListSessions(ctx, ListSessionsRequest(WithListSessionsCwd("relative")))
	require.ErrorContains(t, err, "absolute")

	badHome := string([]byte{0})
	_, err = NewAgent(WithHome(badHome)).ListSessions(ctx, ListSessionsRequest())
	require.Error(t, err)

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
	require.Len(t, nativeResp.Sessions, 1)
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
	_, err = fatalAgent.Prompt(ctx, TextPromptRequest(sessionID, "hello"))
	require.Error(t, err)
	require.Empty(t, fatalAgent.sessions)

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
