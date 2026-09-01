package claudeacp

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/stretchr/testify/require"
)

func residualCallbackAuthority() *callbackHostAuthority {
	return &callbackHostAuthority{
		environment: func() map[string]string { return map[string]string{"PATH": "/bin"} },
		prepare:     func(context.Context, string) error { return nil },
		reclaim:     func(context.Context, string) error { return nil },
		start:       func(context.Context, NativeRequest) (NativeProcess, error) { return valueNativeProcess{}, nil },
	}
}

func residualProviderAuth(authority HostAuthority) *providerAuth {
	agent := NewAgent(WithHostAuthority(authority))

	return &providerAuth{agent: agent, home: providerAuthHome{path: "/provider-home"}}
}

func TestProviderAuthClaudeAuthorityResidualBranches(t *testing.T) {
	require.Nil(t, (&providerAuth{agent: NewAgent()}).claudeAuthority())

	t.Run("delegates other roots", func(t *testing.T) {
		events := []string{}
		authority := residualCallbackAuthority()
		authority.prepare = func(_ context.Context, root string) error {
			events = append(events, "prepare:"+root)

			return nil
		}
		authority.reclaim = func(_ context.Context, root string) error {
			events = append(events, "reclaim:"+root)

			return nil
		}
		wrapped := residualProviderAuth(authority).claudeAuthority()
		require.NoError(t, wrapped.PrepareNativeTree(t.Context(), "/other"))
		require.NoError(t, wrapped.ReclaimNativeTree(t.Context(), "/other"))
		require.Equal(t, []string{"prepare:/other", "reclaim:/other"}, events)
	})

	t.Run("access cancellation", func(t *testing.T) {
		broker := residualProviderAuth(residualCallbackAuthority())
		require.NoError(t, broker.takeNativeHomeAccess(t.Context()))
		wrapped := broker.claudeAuthority()
		require.ErrorIs(t, wrapped.PrepareNativeTree(residualCanceledContext(), broker.home.path), context.Canceled)
		require.ErrorIs(t, wrapped.ReclaimNativeTree(residualCanceledContext(), broker.home.path), context.Canceled)
		broker.releaseNativeHomeAccess()
	})

	t.Run("opaque tree", func(t *testing.T) {
		broker := residualProviderAuth(residualCallbackAuthority())
		broker.nativeTreeOpaque = errors.New("opaque")
		wrapped := broker.claudeAuthority()
		require.ErrorIs(t, wrapped.PrepareNativeTree(t.Context(), broker.home.path), ErrContainmentIncomplete)
		require.ErrorIs(t, wrapped.ReclaimNativeTree(t.Context(), broker.home.path), ErrContainmentIncomplete)
	})

	t.Run("prepare failure and reuse", func(t *testing.T) {
		authority := residualCallbackAuthority()
		authority.prepare = func(context.Context, string) error { return errors.New("prepare refused") }
		broker := residualProviderAuth(authority)
		wrapped := broker.claudeAuthority()
		require.ErrorContains(t, wrapped.PrepareNativeTree(t.Context(), broker.home.path), "prepare refused")
		require.NotNil(t, broker.nativeTreeOpaque)

		broker = residualProviderAuth(residualCallbackAuthority())
		wrapped = broker.claudeAuthority()
		require.NoError(t, wrapped.PrepareNativeTree(t.Context(), broker.home.path))
		require.NoError(t, wrapped.PrepareNativeTree(t.Context(), broker.home.path))
		require.Equal(t, 2, broker.nativeTreeUsers)
		require.NoError(t, wrapped.ReclaimNativeTree(t.Context(), broker.home.path))
		require.Equal(t, 1, broker.nativeTreeUsers)
		require.True(t, broker.nativeTreePrepared)
		require.NoError(t, wrapped.ReclaimNativeTree(t.Context(), broker.home.path))
		require.False(t, broker.nativeTreePrepared)
	})

	t.Run("reclaim failures", func(t *testing.T) {
		broker := residualProviderAuth(residualCallbackAuthority())
		wrapped := broker.claudeAuthority()
		require.ErrorIs(t, wrapped.ReclaimNativeTree(t.Context(), broker.home.path), ErrContainmentIncomplete)

		authority := residualCallbackAuthority()
		authority.reclaim = func(context.Context, string) error { return errors.New("reclaim refused") }
		broker = residualProviderAuth(authority)
		broker.nativeTreePrepared = true
		wrapped = broker.claudeAuthority()
		require.ErrorIs(t, wrapped.ReclaimNativeTree(t.Context(), broker.home.path), ErrContainmentIncomplete)
	})
}

func TestProviderAuthIdleReclaimResidualBranches(t *testing.T) {
	require.NoError(t, (&providerAuth{agent: NewAgent()}).reclaimIdleNativeHome(t.Context()))

	broker := residualProviderAuth(residualCallbackAuthority())
	require.NoError(t, broker.takeNativeHomeAccess(t.Context()))
	require.ErrorIs(t, broker.reclaimIdleNativeHome(residualCanceledContext()), context.Canceled)
	broker.releaseNativeHomeAccess()

	broker.nativeTreeOpaque = errors.New("opaque")
	require.ErrorIs(t, broker.reclaimIdleNativeHome(t.Context()), ErrContainmentIncomplete)
	broker.nativeTreeOpaque = nil
	broker.nativeTreeUsers = 1
	require.ErrorIs(t, broker.reclaimIdleNativeHome(t.Context()), ErrNativeTreeBusy)
	broker.nativeTreeUsers = 0
	require.NoError(t, broker.reclaimIdleNativeHome(t.Context()))

	for _, failure := range []error{ErrNativeTreeBusy, ErrContainmentIncomplete, errors.New("reclaim refused")} {
		authority := residualCallbackAuthority()
		authority.reclaim = func(context.Context, string) error { return failure }
		broker = residualProviderAuth(authority)
		broker.nativeTreePrepared = true
		err := broker.reclaimIdleNativeHome(t.Context())
		if errors.Is(failure, ErrNativeTreeBusy) {
			require.ErrorIs(t, err, ErrNativeTreeBusy)
		} else {
			require.ErrorIs(t, err, ErrContainmentIncomplete)
		}
	}

	broker = residualProviderAuth(residualCallbackAuthority())
	broker.nativeTreePrepared = true
	require.NoError(t, broker.reclaimIdleNativeHome(t.Context()))
	require.False(t, broker.nativeTreePrepared)
}

func TestProviderAuthReadAdmissionResidualBranches(t *testing.T) {
	broker := residualProviderAuth(residualCallbackAuthority())
	require.NoError(t, broker.takeNativeHomeAccess(t.Context()))
	_, err := broker.admitNativeHomeRead(residualCanceledContext())
	require.ErrorIs(t, err, context.Canceled)
	broker.releaseNativeHomeAccess()

	broker.nativeTreeOpaque = errors.New("opaque")
	_, err = broker.admitNativeHomeRead(t.Context())
	require.ErrorIs(t, err, ErrContainmentIncomplete)
	broker.nativeTreeOpaque = nil
	broker.nativeTreePrepared = true
	_, err = broker.admitNativeHomeRead(t.Context())
	require.ErrorIs(t, err, ErrNativeTreeBusy)
	broker.nativeTreePrepared = false
	release, err := broker.admitNativeHomeRead(t.Context())
	require.NoError(t, err)
	release()
}

func TestMaterializedPrepareResidualBranches(t *testing.T) {
	authority := residualCallbackAuthority()
	materialized := &materializedSession{}
	require.NoError(t, materialized.prepare(t.Context(), authority, "", "/root"))
	require.True(t, materialized.owns("/root"))

	for _, failure := range []error{ErrContainmentIncomplete, ErrHostAuthorityUnavailable, errors.New("prepare refused")} {
		authority := residualCallbackAuthority()
		authority.prepare = func(context.Context, string) error { return failure }
		materialized := &materializedSession{}
		err := materialized.prepare(t.Context(), authority, "/root")
		if errors.Is(failure, ErrHostAuthorityUnavailable) {
			require.ErrorIs(t, err, ErrHostAuthorityUnavailable)
		} else {
			require.ErrorIs(t, err, ErrContainmentIncomplete)
		}
		require.Contains(t, materialized.opaque, "/root")
	}
}

func TestRateLimitResidenceResidualFailures(t *testing.T) {
	original := materializeMkdirTemp
	t.Cleanup(func() { materializeMkdirTemp = original })

	authority := residualCallbackAuthority()
	agent := NewAgent(WithHostAuthority(authority), WithScratchDir(t.TempDir()), WithHome(t.TempDir()))
	materializeMkdirTemp = func(string, string) (string, error) { return "", errors.New("mkdir refused") }
	_, err := agent.handleRateLimits(t.Context(), json.RawMessage(`{}`))
	require.ErrorContains(t, err, "mkdir refused")

	materializeMkdirTemp = original
	authority.prepare = func(context.Context, string) error { return ErrContainmentIncomplete }
	_, err = agent.handleRateLimits(t.Context(), json.RawMessage(`{}`))
	require.ErrorIs(t, err, ErrContainmentIncomplete)
}

func TestSessionLifecycleResidualBranches(t *testing.T) {
	agent := NewAgent()
	agent.closed = true
	session := &agentSession{agent: agent, mcpRefreshPending: true, canRelaunch: true}
	require.Error(t, session.refreshMCPRegistry(t.Context()))

	cancelled := false
	session = &agentSession{cancel: func() { cancelled = true }}
	session.fenceAuthorityFailure()
	require.True(t, cancelled)
	require.True(t, session.closing)
}

func TestAgentAndSessionMapResidualBranches(t *testing.T) {
	closed := NewAgent()
	closed.closed = true
	_, err := closed.Prompt(t.Context(), TextPromptRequest("missing", "turn", "hello"))
	require.Error(t, err)

	authAgent := newAuthAgent(t)
	require.NoError(t, authAgent.Close())

	agent := NewAgent()
	session, cleanup := newStartedAgentSessionForTest(t, agent, "deleted")
	defer cleanup()
	agent.deleted[session.id] = struct{}{}
	require.Error(t, agent.storeStartedSession(t.Context(), session))

	hosted := NewAgent(WithHostAuthority(residualCallbackAuthority()))
	called := false
	originalDelete := deleteNativeTranscript
	deleteNativeTranscript = func(context.Context, string, string) error {
		called = true

		return nil
	}
	t.Cleanup(func() { deleteNativeTranscript = originalDelete })
	hosted.retryDeleteNativeTranscript(t.Context(), "session")
	require.False(t, called)
}

func TestAuthorityFailureFanoutLogsCloseFailure(t *testing.T) {
	agent := NewAgent()
	session, cleanup := newStartedAgentSessionForTest(t, agent, "failure")
	defer cleanup()
	session.client = startedClientWithCloseError(t, errors.New("close refused"))
	done := make(chan struct{})
	agent.closeAuthorityFailedSessions(map[acp.SessionId]*agentSession{session.id: session}, done)
	<-done
}

func TestProviderAuthAdmissionTimeoutResidualBranches(t *testing.T) {
	broker, sessionID := newAuthBroker(t)
	release := holdAuthSlot(t, broker)

	flow := &authFlow{
		id:        "flow",
		sessionID: sessionID,
		method: authCatalogMethod{
			Type: authMethodTypeOAuth,
		},
	}
	_, cause := broker.mintPresentation(residualCanceledContext(), flow)
	require.Equal(t, authCauseTimeout, cause)

	_, err := broker.inventory(residualCanceledContext(), authParams(t, map[string]any{
		"sessionId": string(sessionID),
	}))
	requireAuthFailed(t, err, authCauseTimeout)
	_ = release
}

func TestReplacementConfigurationStoreResidualBranches(t *testing.T) {
	newSession := func(agent *Agent) *agentSession {
		return &agentSession{id: "replacement", agent: agent, mirror: &sessionMirror{}}
	}

	base := NewInMemorySessionStore()
	store := &faultSessionStore{SessionStore: base, listSubkeysErr: errors.New("list refused")}
	agent := NewAgent(WithSessionStore(store))
	require.ErrorContains(t, agent.persistReplacementConfiguration(t.Context(), newSession(agent), nil), "list session store subkeys")

	store = &faultSessionStore{SessionStore: base, listSubkeys: []string{"child"}, loadSubpathErr: errors.New("load refused")}
	agent = NewAgent(WithSessionStore(store))
	require.ErrorContains(t, agent.persistReplacementConfiguration(t.Context(), newSession(agent), nil), "load session store subkey")

	require.NoError(t, base.Append(t.Context(), SessionKey{SessionID: "replacement", Subpath: "child"}, []SessionStoreEntry{json.RawMessage(`{"type":"child"}`)}))
	store = &faultSessionStore{SessionStore: base, listSubkeys: []string{"child"}, replaceErr: errors.New("replace refused")}
	agent = NewAgent(WithSessionStore(store))
	require.ErrorContains(t, agent.persistReplacementConfiguration(t.Context(), newSession(agent), nil), "replace session configuration")
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

func TestAgentCloseRetriesBusySession(t *testing.T) {
	authority := residualCallbackAuthority()
	reclaims := 0
	authority.reclaim = func(context.Context, string) error {
		reclaims++
		if reclaims == 1 {
			return ErrNativeTreeBusy
		}

		return nil
	}
	agent := NewAgent(WithHostAuthority(authority))
	session, cleanup := newStartedAgentSessionForTest(t, agent, "busy-close")
	defer cleanup()
	root := t.TempDir()
	session.materialized = &materializedSession{configDir: root, authority: authority, prepared: []string{root}}
	agent.sessions[session.id] = session
	require.NoError(t, agent.Close())
	require.Equal(t, 2, reclaims)
}

func TestRateLimitConfigCopyResidualFailure(t *testing.T) {
	originalCopy := copyClaudeConfigFiles
	copyClaudeConfigFiles = func(string, string, claude.Options) error { return errors.New("copy refused") }
	t.Cleanup(func() { copyClaudeConfigFiles = originalCopy })

	agent := NewAgent(
		WithHostAuthority(residualCallbackAuthority()),
		WithScratchDir(t.TempDir()),
		WithHome(t.TempDir()),
	)
	_, err := agent.handleRateLimits(t.Context(), json.RawMessage(`{}`))
	require.ErrorContains(t, err, "copy refused")
}

func TestProviderAuthReadAdmissionRetainedFailure(t *testing.T) {
	broker, _ := newAuthBroker(t)
	pending := true
	child := &fakeAuthLogin{closeErr: errors.New("cleanup refused"), cleanupPending: &pending}
	handle := &authLoginHandle{login: child, agent: broker.agent, owner: broker}
	broker.retainLogin(handle)
	_, err := broker.admitNativeHomeRead(t.Context())
	require.ErrorContains(t, err, "cleanup refused")
}

func TestStartSessionManagedResidualFailures(t *testing.T) {
	t.Run("provider auth admission", func(t *testing.T) {
		agent := newAuthAgent(t)
		pending := true
		child := &fakeAuthLogin{closeErr: errors.New("cleanup refused"), cleanupPending: &pending}
		handle := &authLoginHandle{login: child, agent: agent, owner: agent.providerAuth}
		agent.providerAuth.retainLogin(handle)
		_, err := agent.startSession(t.Context(), "auth-admission", sessionStart{Cwd: t.TempDir()})
		require.ErrorContains(t, err, "cleanup refused")
	})

	t.Run("provider injection and read release", func(t *testing.T) {
		agent := newAuthAgent(t, WithExecutablePath(filepath.Join(t.TempDir(), "missing-claude")))
		_, err := agent.startSession(t.Context(), "auth-injection", sessionStart{Cwd: t.TempDir()})
		require.Error(t, err)
	})

	t.Run("isolated home creation", func(t *testing.T) {
		originalMkdirTemp := materializeMkdirTemp
		materializeMkdirTemp = func(string, string) (string, error) { return "", errors.New("home refused") }
		t.Cleanup(func() { materializeMkdirTemp = originalMkdirTemp })
		agent := NewAgent(
			WithHostAuthority(residualCallbackAuthority()),
			WithScratchDir(t.TempDir()),
			WithHome(t.TempDir()),
		)
		_, err := agent.startSession(t.Context(), "home-failure", sessionStart{Cwd: t.TempDir()})
		require.ErrorContains(t, err, "create isolated Claude home")
	})

	t.Run("config copy", func(t *testing.T) {
		originalCopy := copyClaudeConfigFiles
		copyClaudeConfigFiles = func(string, string, claude.Options) error { return errors.New("copy refused") }
		t.Cleanup(func() { copyClaudeConfigFiles = originalCopy })
		agent := NewAgent(
			WithHostAuthority(residualCallbackAuthority()),
			WithScratchDir(t.TempDir()),
			WithHome(t.TempDir()),
		)
		_, err := agent.startSession(t.Context(), "copy-failure", sessionStart{Cwd: t.TempDir()})
		require.ErrorContains(t, err, "copy refused")
	})

	t.Run("authority preparation", func(t *testing.T) {
		authority := residualCallbackAuthority()
		authority.prepare = func(context.Context, string) error { return ErrContainmentIncomplete }
		agent := NewAgent(
			WithHostAuthority(authority),
			WithScratchDir(t.TempDir()),
			WithHome(t.TempDir()),
		)
		_, err := agent.startSession(t.Context(), "prepare-failure", sessionStart{Cwd: t.TempDir()})
		require.ErrorIs(t, err, ErrContainmentIncomplete)
	})
}

func residualCanceledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	return ctx
}
