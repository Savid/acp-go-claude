package claudeacp

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

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
	requireExactUnsupportedField(t, err, jsonFieldCwd)

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

func TestLocalSessionStartSettlesEveryEstablishmentFailure(t *testing.T) {
	newLocalAgent := func(options ...Option) *Agent {
		agent := NewAgent(options...)
		agent.setConnection(&localAgentConnection{agent: agent, hooks: &postResponseHooks{}})

		return agent
	}

	previous := uuidRandom
	uuidRandom = bytes.NewReader(nil)
	agent := newLocalAgent(WithHome(t.TempDir()), WithScratchDir(t.TempDir()))
	_, err := agent.startSession(t.Context(), "session-install-failure", sessionStart{Cwd: t.TempDir()})
	uuidRandom = previous
	require.ErrorContains(t, err, "read random uuid")

	rootErr := errors.New("native root refused")
	agent = newLocalAgent(
		WithHome(t.TempDir()),
		WithScratchDir(t.TempDir()),
		WithRuntimeResourceHooks(RuntimeResourceHooks{
			AcquireNativeRoot: func(context.Context, RuntimeResourceKind) (func(), error) {
				return nil, rootErr
			},
		}),
	)
	_, err = agent.startSession(t.Context(), "session-root-failure", sessionStart{Cwd: t.TempDir()})
	require.ErrorIs(t, err, rootErr)
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
	requireExactUnsupportedField(t, err, jsonFieldCwd)

	loadErrAgent := NewAgent(WithSessionStore(&faultSessionStore{SessionStore: NewInMemorySessionStore(), loadErr: errors.New("load failed")}))
	_, err = loadErrAgent.ResumeSession(ctx, ResumeSessionRequest(sessionID, cwd))
	require.ErrorContains(t, err, "load failed")

	_, err = NewAgent(WithSessionStore(NewInMemorySessionStore())).ResumeSession(ctx, ResumeSessionRequest(sessionID, cwd))
	requireUnknownSession(t, err)

	closed := NewAgent(WithHome(t.TempDir()))
	require.NoError(t, closed.Close())
	_, err = closed.ResumeSession(ctx, ResumeSessionRequest(sessionID, cwd))
	requireAgentClosedRefusal(t, err)

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
	require.ErrorContains(t, err, "active update failed")
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
	_, err = emitFail.ResumeSession(ctx, ResumeSessionRequest("22222222-2222-4222-8222-222222222222", cwd))
	require.ErrorContains(t, err, "resume update failed")

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
	requireExactUnsupportedField(t, err, jsonFieldCwd)

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
	requireAgentClosedRefusal(t, err)

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
	_, err = emitFail.LoadSession(ctx, LoadSessionRequest("44444444-4444-4444-8444-444444444444", cwd))
	require.ErrorContains(t, err, "load update failed")

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
	requireExactUnsupportedField(t, err, jsonFieldCwd)

	badHome := string([]byte{0})
	badHomeResp, err := NewAgent(WithHome(badHome)).ListSessions(ctx, ListSessionsRequest())
	require.NoError(t, err)
	require.Empty(t, badHomeResp.Sessions)

	cursor := "bad"
	_, err = NewAgent(WithSessionStore(NewInMemorySessionStore())).ListSessions(ctx, acp.ListSessionsRequest{Cursor: &cursor})
	require.Error(t, err)
	require.False(t, sessionMatchesListFilters(&agentSession{cwd: cwd}, ListSessionsRequest(WithListSessionsCwd(filepath.Join(cwd, "other")))))
	// An empty cwd is an absent filter, never a filter that matches nothing.
	require.True(t, sessionMatchesListFilters(&agentSession{cwd: cwd}, ListSessionsRequest(WithListSessionsCwd(""))))

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
	require.ErrorContains(t, err, "session delete tombstone failed")

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
		agent:  activeDelete,
		id:     sessionID,
		cwd:    cwd,
		client: closeClient,
		turn:   make(chan struct{}, 1),
	}
	_, err = activeDelete.UnstableDeleteSession(ctx, DeleteSessionRequest(sessionID))
	require.ErrorContains(t, err, "claude transport failed")
}

// newSessionForTransport builds one started session over transport, so a test can
// observe that session's own native containment.
func newSessionForTransport(
	t *testing.T,
	agent *Agent,
	sessionID acp.SessionId,
	transport claude.Transport,
) *agentSession {
	t.Helper()

	client := claude.NewClient(nil, claude.Options{}, transport)
	require.NoError(t, client.Start(context.Background()))
	t.Cleanup(func() { _ = client.Close() })

	return &agentSession{
		agent:  agent,
		id:     sessionID,
		cwd:    t.TempDir(),
		client: client,
		turn:   make(chan struct{}, sessionTurnCapacity),
	}
}

// newBusySessionAgent registers one session whose single turn slot is already
// held, so every close of it must wait at the settlement barrier.
func newBusySessionAgent(
	t *testing.T,
	sessionID acp.SessionId,
	options ...Option,
) (*Agent, *agentSession, *fakeClaudeTransport) {
	t.Helper()

	agent, _, _ := newFakeLifecycleAgent(t, newFakeClaudeTransport(), options...)

	transport := newFakeClaudeTransport()
	session := newSessionForTransport(t, agent, sessionID, transport)
	agent.sessions[sessionID] = session
	session.turn <- struct{}{}

	return agent, session, transport
}

// requireCancelledCloseRefusal pins the wire answer for a barrier wait the caller
// itself withdrew. Nothing failed internally and the caller no longer wants an
// answer, so the raw error travels undressed and the dispatcher answers the one
// code a withdrawn request has: -32800.
func requireCancelledCloseRefusal(t *testing.T, ctx context.Context, err error) {
	t.Helper()

	require.ErrorIs(t, err, errSessionCloseUnsettled)
	require.ErrorIs(t, err, context.Canceled)

	var reqErr *acp.RequestError

	require.NotErrorAs(t, err, &reqErr, "a withdrawn request is not a typed refusal")

	mapped := requestError(ctx, err)
	require.Equal(t, -32800, mapped.Code)
	require.Equal(t, "Request cancelled", mapped.Message)
}

// requireExpiredCloseRefusal pins the wire answer for a barrier wait that ran out
// without a cancel. The request was well formed and nothing failed internally: it
// is a retryable refusal that names itself, and its message carries the
// barrier-wait error rather than anything the native process said.
func requireExpiredCloseRefusal(t *testing.T, ctx context.Context, err error) {
	t.Helper()

	var reqErr *acp.RequestError

	require.ErrorAs(t, err, &reqErr)
	require.Equal(t, -32600, reqErr.Code)
	require.Equal(t, "Invalid request", reqErr.Message)

	data, ok := reqErr.Data.(map[string]any)
	require.True(t, ok, "the refusal names itself")
	require.Equal(t, "claude_session_close_unsettled", data[jsonFieldError])
	require.Equal(t, "session close did not reach its settlement barrier", data[jsonFieldMessage])
	mapped := requestError(ctx, err)
	require.Equal(t, -32603, mapped.Code)
	mappedData, ok := mapped.Data.(map[string]any)
	require.True(t, ok)
	require.Equal(t, "request_deadline_exceeded", mappedData[jsonFieldError])
	require.Equal(t, "deadline", mappedData["class"])
}

// TestCloseSessionKeepsAnUnsettledSessionAddressable proves removal follows the
// settlement barrier and not the request: a close whose caller expired contained
// nothing, so the id keeps the live session and the next close settles it.
//
// The session it refuses is a real one: a turn is running and its cancel is
// registered, which is the only shape under which a native teardown hoisted ahead
// of the barrier would actually fire. "Contained nothing" has to be asserted on
// the session that could have been contained, or it asserts nothing at all.
func TestCloseSessionKeepsAnUnsettledSessionAddressable(t *testing.T) {
	sessionID := acp.SessionId("session-busy")
	agent, session, transport := newBusySessionAgent(t, sessionID)

	turnCtx, cancelTurn := context.WithCancel(context.Background())
	defer cancelTurn()

	session.mu.Lock()
	session.cancel = cancelTurn
	session.turnNonce = "nonce-1"
	session.mu.Unlock()

	expired, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := agent.CloseSession(expired, acp.CloseSessionRequest{SessionId: sessionID})
	requireCancelledCloseRefusal(t, expired, err)
	require.Zero(t, transport.CloseCalls(), "an unsettled close contains nothing")
	require.True(t, session.client.Alive(), "a refused close leaves the native process it never contained")
	require.Zero(t, interruptCalls(transport), "a refused close reaches no rung of the native teardown")
	require.Error(t, turnCtx.Err(), "the turn the close is waiting on is still asked to wind down")

	held, lookupErr := agent.session(sessionID)
	require.NoError(t, lookupErr, "the work still running behind the id stays addressable")
	require.Same(t, session, held)

	<-session.turn

	_, err = agent.CloseSession(context.Background(), acp.CloseSessionRequest{SessionId: sessionID})
	require.NoError(t, err)
	require.Equal(t, 1, transport.CloseCalls())
	_, lookupErr = agent.session(sessionID)
	require.Error(t, lookupErr, "a settled close removes the id")
}

// TestFailedCloseRetriesItsBoundaryOnTheNextClose pins what a close that reached
// its boundary and failed a rung of it leaves behind. The rung here is the
// deletion of the roots the session owns: it fails, so the close reports that
// failure, the id keeps naming the session, and nothing about the attempt is
// memoized. The next close retakes the boundary, completes the rung, and only
// then does the id stop resolving — dropping it on the first failure would leave
// the host holding the only name for what this adapter had not finished.
//
// Each admission is still returned exactly once across both closes: the native
// root on the close that proved containment, the scratch reservation on the
// close that finished deleting what it reserved.
func TestFailedCloseRetriesItsBoundaryOnTheNextClose(t *testing.T) {
	previous := sessionRemoveAll
	t.Cleanup(func() { sessionRemoveAll = previous })

	sessionID := acp.SessionId("session-retry")
	agent, _, _ := newFakeLifecycleAgent(t, newFakeClaudeTransport())
	session := newSessionForTransport(t, agent, sessionID, newFakeClaudeTransport())
	agent.sessions[sessionID] = session

	mcp := filepath.Join(t.TempDir(), "mcp")
	require.NoError(t, os.Mkdir(mcp, 0o700))
	session.mcpConfigDir = mcp

	nativeReleases, scratchReleases := 0, 0
	session.nativeRootRelease = func() { nativeReleases++ }
	session.scratchRootRelease = func() { scratchReleases++ }

	removeErr := errors.New("delete MCP root")
	sessionRemoveAll = func(string) error { return removeErr }

	_, err := agent.CloseSession(t.Context(), acp.CloseSessionRequest{SessionId: sessionID})
	require.ErrorIs(t, err, removeErr)

	held, lookupErr := agent.session(sessionID)
	require.NoError(t, lookupErr, "a close that failed a rung of its boundary keeps its id addressable")
	require.Same(t, session, held)
	require.Equal(t, 1, nativeReleases, "containment completed, so that admission is already back")
	require.Zero(t, scratchReleases, "the reservation survives the deletion the close still owes it")

	sessionRemoveAll = previous

	_, err = agent.CloseSession(t.Context(), acp.CloseSessionRequest{SessionId: sessionID})
	require.NoError(t, err, "the next close retakes the boundary rather than replaying a memoized failure")
	require.NoDirExists(t, mcp, "the rung the first close failed is the one the retry completes")
	require.Equal(t, 1, nativeReleases, "an admission returned twice would credit work that was never admitted")
	require.Equal(t, 1, scratchReleases)

	_, lookupErr = agent.session(sessionID)
	require.Error(t, lookupErr, "a completed boundary removes the id")
}

// cancelAfterDeleteStore withdraws the delete request the instant its tombstone is
// durable. It reproduces the crash window the ordering exists for: everything
// after the tombstone fails, and the test can then ask what a host still sees.
type cancelAfterDeleteStore struct {
	SessionStore

	cancel context.CancelFunc
}

func (s *cancelAfterDeleteStore) Delete(ctx context.Context, key SessionKey) error {
	err := s.SessionStore.Delete(ctx, key)
	s.cancel()

	return err
}

// TestDeleteSessionTombstonesBeforeItTearsAnythingDown proves delete writes its
// durable tombstone first and hides the id with it. Everything after that is
// teardown: when it fails the id is already deleted everywhere a host can look,
// the error is reported rather than swallowed, and the instance stays behind the
// tombstone so the next delete finishes what this one started.
func TestDeleteSessionTombstonesBeforeItTearsAnythingDown(t *testing.T) {
	ctx := context.Background()
	sessionID := acp.SessionId("session-busy")
	key := SessionKey{SessionID: string(sessionID)}
	backing := NewInMemorySessionStore()
	require.NoError(t, backing.Append(ctx, key, []SessionStoreEntry{
		[]byte(`{"type":"user","message":{"content":"live"}}`),
	}))

	withdrawn, cancel := context.WithCancel(ctx)
	defer cancel()

	store := &cancelAfterDeleteStore{SessionStore: backing, cancel: cancel}
	agent, session, transport := newBusySessionAgent(t, sessionID, WithSessionStore(store))

	previousDelete := deleteNativeTranscript
	t.Cleanup(func() { deleteNativeTranscript = previousDelete })

	transcriptDeletes := 0
	deleteNativeTranscript = func(context.Context, string, string) error {
		transcriptDeletes++

		return nil
	}

	_, err := agent.UnstableDeleteSession(withdrawn, DeleteSessionRequest(sessionID))
	require.ErrorIs(t, err, errSessionCloseUnsettled, "a teardown that did not finish is reported")
	require.Zero(t, transport.CloseCalls(), "the barrier admitted no teardown")

	// The tombstone landed before any of that, and the id is hidden everywhere a
	// host can address it.
	entries, err := backing.Load(ctx, key)
	require.NoError(t, err)
	require.Empty(t, entries, "the tombstone is durable before teardown runs")

	agent.mu.Lock()
	_, tombstoned := agent.deleted[sessionID]
	agent.mu.Unlock()
	require.True(t, tombstoned)

	_, lookupErr := agent.session(sessionID)
	requireUnknownSession(t, lookupErr)

	listResp, err := agent.ListSessions(ctx, ListSessionsRequest())
	require.NoError(t, err)
	require.Empty(t, listResp.Sessions)

	// The instance is still held, so the delete that follows can finish the
	// teardown this one abandoned.
	agent.mu.Lock()
	require.Same(t, session, agent.sessions[sessionID])
	agent.mu.Unlock()

	<-session.turn

	_, err = agent.UnstableDeleteSession(ctx, DeleteSessionRequest(sessionID))
	require.NoError(t, err)
	require.Equal(t, 1, transport.CloseCalls())
	require.Equal(t, 2, transcriptDeletes)

	agent.mu.Lock()
	require.NotContains(t, agent.sessions, sessionID, "the settled teardown evicts the instance")
	agent.mu.Unlock()

	entries, err = backing.Load(ctx, key)
	require.NoError(t, err)
	require.Empty(t, entries, "no write behind the tombstone recreates the row")
}

// TestRemoveSessionEvictsOnlyASettledSession proves the internal cleanup path
// obeys the same rule: it closes first and evicts only once that close settled.
func TestRemoveSessionEvictsOnlyASettledSession(t *testing.T) {
	sessionID := acp.SessionId("session-busy")
	agent, session, transport := newBusySessionAgent(t, sessionID)

	expired, cancel := context.WithCancel(context.Background())
	cancel()

	agent.removeSession(expired, sessionID, session)
	require.Contains(t, agent.sessions, sessionID)
	require.Zero(t, transport.CloseCalls())

	<-session.turn

	agent.removeSession(context.Background(), sessionID, session)
	require.NotContains(t, agent.sessions, sessionID)
	require.Equal(t, 1, transport.CloseCalls())
}

// TestStoreStartedSessionRefusesAnInstallOverAnUnsettledSession proves a same-id
// install cannot orphan the instance it replaces: while the replaced session is
// still running a turn its id keeps naming it, and the replacement that could not
// take the id is torn down instead of being left running beside it.
func TestStoreStartedSessionRefusesAnInstallOverAnUnsettledSession(t *testing.T) {
	sessionID := acp.SessionId("session-busy")
	agent, previous, previousTransport := newBusySessionAgent(t, sessionID)

	replacementTransport := newFakeClaudeTransport()
	replacement := newSessionForTransport(t, agent, sessionID, replacementTransport)

	expired, cancel := context.WithCancel(context.Background())
	cancel()

	err := agent.storeStartedSession(expired, replacement)
	requireCancelledCloseRefusal(t, expired, err)
	require.Same(t, previous, agent.sessions[sessionID], "the replaced instance keeps its id")
	require.Zero(t, previousTransport.CloseCalls(), "the replaced instance settled nothing")
	require.Equal(t, 1, replacementTransport.CloseCalls(), "the refused replacement is contained")

	// Once the turn settles, the install lands: the replaced instance is contained
	// first and the id then names the session taking its place.
	<-previous.turn

	admitted := newSessionForTransport(t, agent, sessionID, newFakeClaudeTransport())
	require.NoError(t, agent.storeStartedSession(context.Background(), admitted))
	require.Equal(t, 1, previousTransport.CloseCalls())
	require.Same(t, admitted, agent.sessions[sessionID])
}

// TestStoreStartedSessionRefusesAnInstallIntoAnAgentClosedMidReplacement proves
// the settled replaced instance leaves the map and the replacement is contained
// when the agent closes during that settlement.
func TestStoreStartedSessionRefusesAnInstallIntoAnAgentClosedMidReplacement(t *testing.T) {
	sessionID := acp.SessionId("session-1")
	agent, _, _ := newFakeLifecycleAgent(t, newFakeClaudeTransport())

	hooked := &closeHookTransport{Transport: newFakeClaudeTransport()}
	previous := newSessionForTransport(t, agent, sessionID, hooked)
	agent.sessions[sessionID] = previous

	hooked.onClose = func() {
		agent.mu.Lock()
		agent.closed = true
		agent.mu.Unlock()
	}

	replacementTransport := newFakeClaudeTransport()
	replacement := newSessionForTransport(t, agent, sessionID, replacementTransport)

	require.ErrorIs(t, agent.storeStartedSession(context.Background(), replacement), errAgentClosed)
	require.Equal(t, 1, replacementTransport.CloseCalls(), "the refused replacement is contained")

	agent.mu.Lock()
	_, present := agent.sessions[sessionID]
	agent.mu.Unlock()
	require.False(t, present, "a settled instance no closed agent can serve leaves the map")
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
	return errors.Join(t.err, t.Transport.Close())
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
		agent:  agent,
		id:     id,
		cwd:    t.TempDir(),
		client: client,
		turn:   make(chan struct{}, sessionTurnCapacity),
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
	require.ErrorContains(t, err, "claude control request failed")

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
	require.ErrorContains(t, err, "claude control request failed")
}

func TestStartSessionIsolationFailureBranches(t *testing.T) {
	ctx := t.Context()
	cwd := t.TempDir()
	sessionID := acp.SessionId("34343434-3434-4434-8434-343434343434")
	uid, gid := testIsolationIdentity()
	isolation := ProcessIsolation{UID: uid, GID: gid, BaseEnvironment: map[string]string{"PATH": "/usr/bin:/bin"}}
	// Every isolated branch below reaches its own seam only if the native-owned
	// home check ahead of it passes. That check is real on Linux, so the home has
	// to be one the isolated identity genuinely owns rather than a t.TempDir leaf.
	isolatedOptions := []Option{WithHome(testNativeOwnedHome(t)), WithProcessIsolation(isolation)}

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
	err = start(isolatedOptions, sessionStart{Cwd: cwd})
	require.ErrorContains(t, err, "isolated scratch")
	restore()

	materializeMkdirTemp = func(string, string) (string, error) { return "", errors.New("isolated home") }
	err = start(isolatedOptions, sessionStart{Cwd: cwd})
	require.ErrorContains(t, err, "create isolated Claude home")
	restore()

	copyClaudeConfigFiles = func(string, string, claude.Options) error { return errors.New("copy isolated home") }
	err = start(isolatedOptions, sessionStart{Cwd: cwd})
	require.ErrorContains(t, err, "copy isolated home")
	restore()

	copyClaudeConfigFiles = func(string, string, claude.Options) error { return nil }
	sessionHandoffGeneratedNativeTree = func(string, *ProcessIsolation) error { return errors.New("handoff isolated home") }
	err = start(isolatedOptions, sessionStart{Cwd: cwd})
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
	err = start(isolatedOptions, sessionStart{
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

// gatedLoadStore holds one store read open, so a test can land a delete in the
// exact window a load has already passed its tombstone check and has not yet
// installed anything.
type gatedLoadStore struct {
	SessionStore

	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (s *gatedLoadStore) Load(ctx context.Context, key SessionKey) ([]SessionStoreEntry, error) {
	entries, err := s.SessionStore.Load(ctx, key)
	s.once.Do(func() { close(s.entered) })
	<-s.release

	return entries, err
}

// TestLoadSessionRacingDeleteInstallsNothingBehindTheTombstone proves the
// tombstone is re-read where an id becomes live, not only where a load begins.
// The load passes its tombstone check, the delete lands while the native process
// is starting, and the instance that start produced is refused rather than
// registered: an install behind a tombstone is a live native session no host
// could ever address again, because session, load, resume, fork and close all
// answer unknown-session for that id.
func TestLoadSessionRacingDeleteInstallsNothingBehindTheTombstone(t *testing.T) {
	ctx := context.Background()
	sessionID := acp.SessionId("22222222-2222-4222-8222-222222222222")
	key := SessionKey{SessionID: string(sessionID)}
	cwd := t.TempDir()

	backing := NewInMemorySessionStore()
	require.NoError(t, backing.Append(ctx, key, []SessionStoreEntry{
		[]byte(`{"type":"user","message":{"content":"live"}}`),
	}))

	gate := &gatedLoadStore{SessionStore: backing, entered: make(chan struct{}), release: make(chan struct{})}
	transport := newFakeClaudeTransport()
	agent, _, _ := newFakeLifecycleAgent(t, transport, WithSessionStore(gate))

	var startedMu sync.Mutex
	started := []*claude.Client{}

	agent.newClaudeClient = func(log *slog.Logger, options claude.Options) *claude.Client {
		client := claude.NewClient(log, options, transport)

		startedMu.Lock()
		started = append(started, client)
		startedMu.Unlock()

		return client
	}

	previousDelete := deleteNativeTranscript
	t.Cleanup(func() { deleteNativeTranscript = previousDelete })
	deleteNativeTranscript = func(context.Context, string, string) error { return nil }

	loadDone := make(chan error, 1)

	go func() {
		_, loadErr := agent.LoadSession(ctx, LoadSessionRequest(sessionID, cwd))
		loadDone <- loadErr
	}()

	<-gate.entered

	_, err := agent.UnstableDeleteSession(ctx, DeleteSessionRequest(sessionID))
	require.NoError(t, err, "the delete of an id the map does not hold succeeds")

	close(gate.release)

	requireUnknownSession(t, <-loadDone)

	// The native start really did happen — this is the window, not a load that
	// failed early — and what it produced is contained rather than published.
	startedMu.Lock()
	require.Len(t, started, 1, "the load started exactly one native process")
	require.False(t, started[0].Alive(), "the process the refused install started is not left running")
	startedMu.Unlock()

	require.Equal(t, 1, transport.CloseCalls(), "the refused install is torn down exactly once")

	agent.mu.Lock()
	require.NotContains(t, agent.sessions, sessionID, "nothing is registered behind the tombstone")
	require.True(t, agent.isDeletedLocked(sessionID), "the tombstone is intact")
	agent.mu.Unlock()

	entries, err := backing.Load(ctx, key)
	require.NoError(t, err)
	require.Empty(t, entries, "the racing load recreated no row")
}

// tombstoningTransport writes the agent's tombstone for an id while that id's
// current instance is being closed, which is the window a same-id install opens
// when it steps aside to settle the instance it is replacing.
type tombstoningTransport struct {
	claude.Transport

	agent     *Agent
	sessionID acp.SessionId
}

func (t *tombstoningTransport) Close() error {
	t.agent.mu.Lock()
	t.agent.deleted[t.sessionID] = struct{}{}
	t.agent.mu.Unlock()

	return t.Transport.Close()
}

// TestStoreStartedSessionRefusesAReplacementBehindALateTombstone covers the
// other side of the same fence. A same-id install leaves the lock to settle the
// instance it replaces, so a delete can land in that gap too; the replacement is
// torn down rather than installed, and the settled instance leaves the map.
func TestStoreStartedSessionRefusesAReplacementBehindALateTombstone(t *testing.T) {
	ctx := context.Background()
	sessionID := acp.SessionId("session-replaced")
	agent, _, _ := newFakeLifecycleAgent(t, newFakeClaudeTransport())

	previousTransport := newFakeClaudeTransport()
	previous := newSessionForTransport(t, agent, sessionID, &tombstoningTransport{
		Transport: previousTransport,
		agent:     agent,
		sessionID: sessionID,
	})
	agent.sessions[sessionID] = previous

	replacementTransport := newFakeClaudeTransport()
	replacement := newSessionForTransport(t, agent, sessionID, replacementTransport)

	requireUnknownSession(t, agent.storeStartedSession(ctx, replacement))

	require.Equal(t, 1, previousTransport.CloseCalls(), "the replaced instance settled")
	require.Equal(t, 1, replacementTransport.CloseCalls(), "the replacement is contained, not published")

	agent.mu.Lock()
	require.NotContains(t, agent.sessions, sessionID, "the id names nothing behind its tombstone")
	agent.mu.Unlock()
}

// TestResumeSessionNeverReusesAnInstanceBehindItsTombstone proves the reuse path
// reads the tombstone rather than the map. Delete writes its tombstone before it
// touches the instance behind it, so there is a real window in which the map
// still holds a session whose close has not even begun; handing that instance to
// a load, resume or fork would un-delete a session the host was told is gone.
// The probe runs from inside the delete's own cancel, which is the window.
func TestResumeSessionNeverReusesAnInstanceBehindItsTombstone(t *testing.T) {
	ctx := context.Background()
	sessionID := acp.SessionId("session-busy")
	cwd := t.TempDir()

	withdrawn, cancel := context.WithCancel(ctx)
	defer cancel()

	store := &cancelAfterDeleteStore{SessionStore: NewInMemorySessionStore(), cancel: cancel}
	agent, session, _ := newBusySessionAgent(t, sessionID, WithSessionStore(store))

	previousDelete := deleteNativeTranscript
	t.Cleanup(func() { deleteNativeTranscript = previousDelete })
	deleteNativeTranscript = func(context.Context, string, string) error { return nil }

	start := sessionStart{Cwd: cwd, ResumeID: string(sessionID)}
	session.fingerprint = sessionStartFingerprint(start)
	require.Same(t, session, agent.activeSessionForStart(sessionID, start), "the instance is reusable before the delete")

	// The delete's cancel runs after the tombstone is durable and before the
	// close that would latch the instance's terminal state, so this is the whole
	// window the fence exists for.
	reused := make(chan *agentSession, 1)

	var probeOnce sync.Once

	session.mu.Lock()
	session.cancel = func() {
		probeOnce.Do(func() { reused <- agent.activeSessionForStart(sessionID, start) })
	}
	session.mu.Unlock()

	_, err := agent.UnstableDeleteSession(withdrawn, DeleteSessionRequest(sessionID))
	require.ErrorIs(t, err, errSessionCloseUnsettled, "the barrier refused the teardown, so the instance is held")

	require.Nil(t, <-reused, "a tombstoned id names nothing the map still holds")

	agent.mu.Lock()
	require.Same(t, session, agent.sessions[sessionID], "the instance stays for the next delete to retry")
	agent.mu.Unlock()

	require.Nil(t, agent.activeSessionForStart(sessionID, start))

	_, resumeErr := agent.ResumeSession(ctx, ResumeSessionRequest(sessionID, cwd))
	requireUnknownSession(t, resumeErr)
}

// expireAfterDeleteStore holds the delete until its caller's deadline has passed,
// so the teardown behind a durable tombstone meets a barrier wait that expires on
// a clock rather than on a cancel.
type expireAfterDeleteStore struct {
	SessionStore

	once sync.Once
}

func (s *expireAfterDeleteStore) Delete(ctx context.Context, key SessionKey) error {
	err := s.SessionStore.Delete(ctx, key)
	s.once.Do(func() { <-ctx.Done() })

	return err
}

// TestDeleteSessionNamesItsUnreachedTeardownBoundary pins the wire answer for a
// delete whose teardown never took the settlement barrier. It is the same
// unreached boundary session/close reports, so it is not an internal failure: a
// deadline gets the family's named invalid-request refusal, under a name of its
// own because the deletion itself already happened.
func TestDeleteSessionNamesItsUnreachedTeardownBoundary(t *testing.T) {
	ctx := context.Background()
	sessionID := acp.SessionId("session-busy")
	key := SessionKey{SessionID: string(sessionID)}
	backing := NewInMemorySessionStore()
	require.NoError(t, backing.Append(ctx, key, []SessionStoreEntry{
		[]byte(`{"type":"user","message":{"content":"live"}}`),
	}))

	agent, session, transport := newBusySessionAgent(t, sessionID,
		WithSessionStore(&expireAfterDeleteStore{SessionStore: backing}))

	previousDelete := deleteNativeTranscript
	t.Cleanup(func() { deleteNativeTranscript = previousDelete })
	deleteNativeTranscript = func(context.Context, string, string) error { return nil }

	expiring, cancel := context.WithTimeout(ctx, time.Millisecond)
	defer cancel()

	_, err := agent.UnstableDeleteSession(expiring, DeleteSessionRequest(sessionID))
	requireExpiredDeleteRefusal(t, expiring, err)
	require.Zero(t, transport.CloseCalls(), "the barrier admitted no teardown")

	// The refusal is a refusal of the teardown alone: the deletion the host asked
	// for is durable, the id is hidden, and the instance stays for the retry.
	entries, err := backing.Load(ctx, key)
	require.NoError(t, err)
	require.Empty(t, entries)

	agent.mu.Lock()
	require.Same(t, session, agent.sessions[sessionID])
	agent.mu.Unlock()

	_, lookupErr := agent.session(sessionID)
	requireUnknownSession(t, lookupErr)

	<-session.turn

	_, err = agent.UnstableDeleteSession(ctx, DeleteSessionRequest(sessionID))
	require.NoError(t, err, "the next delete finishes the teardown this one refused")
	require.Equal(t, 1, transport.CloseCalls())
}

// requireExpiredDeleteRefusal pins delete's answer for a barrier wait that ran
// out without a cancel: the request was well formed and nothing failed
// internally, so it is a retryable refusal that names itself and carries the
// barrier-wait error as its message.
func requireExpiredDeleteRefusal(t *testing.T, ctx context.Context, err error) {
	t.Helper()

	var reqErr *acp.RequestError

	require.ErrorAs(t, err, &reqErr)
	require.Equal(t, -32600, reqErr.Code)
	require.Equal(t, "Invalid request", reqErr.Message)

	data, ok := reqErr.Data.(map[string]any)
	require.True(t, ok, "the refusal names itself")
	require.Equal(t, "claude_session_delete_unsettled", data[jsonFieldError])
	require.Equal(t, "session delete teardown did not reach its settlement barrier", data[jsonFieldMessage])
	mapped := requestError(ctx, err)
	require.Equal(t, -32603, mapped.Code)
	mappedData, ok := mapped.Data.(map[string]any)
	require.True(t, ok)
	require.Equal(t, "request_deadline_exceeded", mappedData[jsonFieldError])
	require.Equal(t, "deadline", mappedData["class"])
}

// ladderProbeTransport reports the state of the shutdown ladder at the moment a
// native teardown rung actually reaches the process.
type ladderProbeTransport struct {
	claude.Transport

	observe func(rung string)
}

func (t *ladderProbeTransport) Send(ctx context.Context, payload any) error {
	if request, ok := payload.(claude.ControlRequest); ok {
		if subtype, _ := request.Request["subtype"].(string); subtype == "interrupt" {
			t.observe("interrupt")
		}
	}

	return t.Transport.Send(ctx, payload)
}

func (t *ladderProbeTransport) Close() error {
	t.observe("close")

	return t.Transport.Close()
}

// ladderObservation records what the earlier rungs had done by the time one
// native teardown rung fired.
type ladderObservation struct {
	rung             string
	admissionClosed  bool
	providerAuthDone bool
}

// TestCloseSessionRunsTheEarlyLadderBeforeAnyNativeTeardown pins the fixed
// positions in the shutdown ladder. Nothing native is touched until the session
// has stopped accepting prompts and the pending provider-auth flows have been
// cancelled: an interrupt hoisted ahead of those rungs answers a pending flow
// from a process that is already being torn down, and it tears that process down
// while the session is still admitting the prompt that would relaunch it.
func TestCloseSessionRunsTheEarlyLadderBeforeAnyNativeTeardown(t *testing.T) {
	sessionID := acp.SessionId("session-ladder")
	agent, _, _ := newFakeLifecycleAgent(t, newFakeClaudeTransport(), WithProviderAuthRoot(t.TempDir()))
	require.NotNil(t, agent.providerAuth, "the provider-auth rung exists on this fixture, so its position can be asserted")

	probe := &ladderProbeTransport{Transport: newFakeClaudeTransport()}
	session := newSessionForTransport(t, agent, sessionID, probe)
	agent.sessions[sessionID] = session

	var (
		mu       sync.Mutex
		observed []ladderObservation
	)

	probe.observe = func(rung string) {
		agent.providerAuth.mu.Lock()
		_, providerAuthDone := agent.providerAuth.closedSessions[sessionID]
		agent.providerAuth.mu.Unlock()

		mu.Lock()
		defer mu.Unlock()

		observed = append(observed, ladderObservation{
			rung:             rung,
			admissionClosed:  session.isClosing(),
			providerAuthDone: providerAuthDone,
		})
	}

	// A registered turn cancel is what makes a hoisted native abort fire at all,
	// so the session this close is asked to contain is one that could be.
	_, cancelTurn := context.WithCancel(context.Background())
	defer cancelTurn()

	session.mu.Lock()
	session.cancel = cancelTurn
	session.turnNonce = "nonce-1"
	session.mu.Unlock()

	_, err := agent.CloseSession(context.Background(), acp.CloseSessionRequest{SessionId: sessionID})
	require.NoError(t, err)

	mu.Lock()
	defer mu.Unlock()

	require.Equal(t, []ladderObservation{{
		rung:             "close",
		admissionClosed:  true,
		providerAuthDone: true,
	}}, observed, "the containment boundary is the only rung that reaches the process, and it runs last")
}

// TestDeleteWhoseTombstoneNeverLandedIsToldApartFromAnUnsettledTeardown pins the
// two refusals a delete can carry. Both can reach the wire as -32800 — a caller
// that withdrew its own request has no other honest answer — but they report
// opposite states: the unsettled teardown already deleted the session and owes
// only the cleanup behind it, while a failed tombstone deleted nothing and left
// the id listable, loadable, and resumable. A host that read the second as the
// first would stop naming a session that is still there.
func TestDeleteWhoseTombstoneNeverLandedIsToldApartFromAnUnsettledTeardown(t *testing.T) {
	sessionID := acp.SessionId("session-undeleted")
	key := SessionKey{SessionID: string(sessionID)}

	t.Run("the caller withdrew the request", func(t *testing.T) {
		ctx := context.Background()
		backing := NewInMemorySessionStore()
		require.NoError(t, backing.Append(ctx, key, []SessionStoreEntry{[]byte(`{"type":"user"}`)}))

		agent := NewAgent(WithHome(t.TempDir()), WithSessionStore(backing))

		withdrawn, cancel := context.WithCancel(ctx)
		cancel()

		_, err := agent.UnstableDeleteSession(withdrawn, DeleteSessionRequest(sessionID))
		require.ErrorIs(t, err, context.Canceled)

		mapped := requestError(withdrawn, err)
		require.Equal(t, -32800, mapped.Code)

		data, ok := mapped.Data.(map[string]any)
		require.True(t, ok)
		require.Equal(t, "request_cancelled", data[jsonFieldError])

		// Nothing was deleted: the id is not hidden and its rows are still there.
		agent.mu.Lock()
		_, hidden := agent.deleted[sessionID]
		agent.mu.Unlock()
		require.False(t, hidden, "a delete that wrote no tombstone hides nothing")

		entries, loadErr := backing.Load(ctx, key)
		require.NoError(t, loadErr)
		require.Len(t, entries, 1, "the session the delete never deleted is still stored")

		summaries, listErr := backing.ListSessions(ctx)
		require.NoError(t, listErr)
		require.Len(t, summaries, 1, "and still listable")
	})

	t.Run("the store itself refused the write", func(t *testing.T) {
		agent := NewAgent(WithHome(t.TempDir()), WithSessionStore(&faultSessionStore{
			SessionStore: NewInMemorySessionStore(),
			deleteErr:    errors.New("tombstone write failed"),
		}))

		_, err := agent.UnstableDeleteSession(context.Background(), DeleteSessionRequest(sessionID))

		var reqErr *acp.RequestError

		require.ErrorAs(t, err, &reqErr)
		require.Equal(t, -32603, reqErr.Code)

		data, ok := reqErr.Data.(map[string]any)
		require.True(t, ok)
		require.Equal(t, "claude_session_delete_untombstoned", data[jsonFieldError])
		require.Equal(t, "session delete tombstone failed", data[jsonFieldMessage])
	})
}

// TestConcurrentInstallAndDeleteNeverResurrectTheSession drives the race the
// install-lock re-check exists for. A load or resume decides its id is
// addressable, then spends a native process launch getting there; a delete
// landing inside that window writes a durable tombstone and hides the id, so the
// instance about to be published would be one nothing could ever name again —
// unclosable, unlistable, and holding a live native process.
//
// The verdict of the race is legitimately either way: a load that finishes first
// is a session the delete then deletes, and a delete that lands first refuses the
// install. What is never either way is the end state. However the two interleave,
// the tombstone is final: the id resolves to nothing, nothing is listable under
// it, the store holds no row for it, and no instance is left in the active map.
func TestConcurrentInstallAndDeleteNeverResurrectTheSession(t *testing.T) {
	ctx := context.Background()
	cwd := t.TempDir()

	for _, testCase := range []struct {
		name    string
		install func(*Agent, acp.SessionId) error
	}{
		{
			name: "session/load",
			install: func(agent *Agent, sessionID acp.SessionId) error {
				_, err := agent.LoadSession(ctx, LoadSessionRequest(sessionID, cwd))

				return err
			},
		},
		{
			name: "session/resume",
			install: func(agent *Agent, sessionID acp.SessionId) error {
				_, err := agent.ResumeSession(ctx, ResumeSessionRequest(sessionID, cwd))

				return err
			},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			sessionID := acp.SessionId("11111111-1111-4111-8111-111111111111")
			key := SessionKey{SessionID: string(sessionID)}

			store := NewInMemorySessionStore()
			require.NoError(t, store.Append(ctx, key, []SessionStoreEntry{
				[]byte(`{"type":"user","cwd":"` + filepath.ToSlash(cwd) + `","message":{"content":"hello"}}`),
			}))

			agent, _, _ := newFakeLifecycleAgent(t, newFakeClaudeTransport(), WithSessionStore(store))
			t.Cleanup(func() { _ = agent.Close() })

			start := make(chan struct{})
			installed := make(chan error, 1)
			deleted := make(chan error, 1)

			go func() {
				<-start
				installed <- testCase.install(agent, sessionID)
			}()

			go func() {
				<-start

				_, err := agent.UnstableDeleteSession(ctx, DeleteSessionRequest(sessionID))
				deleted <- err
			}()

			close(start)

			require.NoError(t, <-deleted)

			// Either verdict is legitimate: the install either finished before the
			// tombstone or was refused by it.
			if installErr := <-installed; installErr != nil {
				requireUnknownSession(t, installErr)
			}

			// The tombstone outranks whichever order the two actually took.
			_, lookupErr := agent.session(sessionID)
			requireUnknownSession(t, lookupErr)

			agent.mu.Lock()
			_, resident := agent.sessions[sessionID]
			agent.mu.Unlock()
			require.False(t, resident, "no instance survives behind the tombstone")

			entries, loadErr := store.Load(ctx, key)
			require.NoError(t, loadErr)
			require.Empty(t, entries, "no durable row survives behind the tombstone")

			summaries, listErr := store.ListSessions(ctx)
			require.NoError(t, listErr)
			require.Empty(t, summaries, "a deleted session is never listable again")

			listed, err := agent.ListSessions(ctx, ListSessionsRequest())
			require.NoError(t, err)
			require.Empty(t, listed.Sessions)
		})
	}
}

// TestDeleteLandingInsideANativeStartRefusesTheInstall drives the same race with
// the window held open deliberately. The delete lands after the install decided
// its id was addressable and before it has anything to publish, which is the one
// interleaving a scheduler is unlikely to produce on its own and the exact one
// the re-check under the install lock exists for.
func TestDeleteLandingInsideANativeStartRefusesTheInstall(t *testing.T) {
	ctx := context.Background()
	cwd := t.TempDir()
	sessionID := acp.SessionId("11111111-1111-4111-8111-111111111111")
	key := SessionKey{SessionID: string(sessionID)}

	store := NewInMemorySessionStore()
	require.NoError(t, store.Append(ctx, key, []SessionStoreEntry{
		[]byte(`{"type":"user","cwd":"` + filepath.ToSlash(cwd) + `","message":{"content":"hello"}}`),
	}))

	transport := newFakeClaudeTransport()
	agent, _, _ := newFakeLifecycleAgent(t, transport, WithSessionStore(store))
	t.Cleanup(func() { _ = agent.Close() })

	deletes := 0
	agent.newClaudeClient = func(log *slog.Logger, options claude.Options) *claude.Client {
		if deletes == 0 {
			deletes++

			_, err := agent.UnstableDeleteSession(ctx, DeleteSessionRequest(sessionID))
			require.NoError(t, err)
		}

		return claude.NewClient(log, options, transport)
	}

	_, err := agent.LoadSession(ctx, LoadSessionRequest(sessionID, cwd))
	requireUnknownSession(t, err)
	require.Equal(t, 1, deletes, "the delete really landed inside the native start")

	agent.mu.Lock()
	_, resident := agent.sessions[sessionID]
	agent.mu.Unlock()
	require.False(t, resident, "the instance nothing could ever name is torn down, not installed")

	require.Positive(t, transport.CloseCalls(), "and its native process is contained")

	_, lookupErr := agent.session(sessionID)
	requireUnknownSession(t, lookupErr)

	entries, loadErr := store.Load(ctx, key)
	require.NoError(t, loadErr)
	require.Empty(t, entries, "nothing the refused install did reaches the store")
}
