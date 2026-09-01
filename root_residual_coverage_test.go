package claudeacp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

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

func TestRateLimitScratchParentResidualFailure(t *testing.T) {
	occupied := filepath.Join(t.TempDir(), "occupied")
	require.NoError(t, os.WriteFile(occupied, []byte("not a directory"), 0o600))
	agent := NewAgent(
		WithHostAuthority(residualCallbackAuthority()),
		WithScratchDir(occupied),
		WithHome(t.TempDir()),
	)
	_, err := agent.handleRateLimits(t.Context(), json.RawMessage(`{}`))
	require.ErrorContains(t, err, "create scratch parent dir")
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

func TestSessionRetirementResidualBranches(t *testing.T) {
	t.Run("unsettled", func(t *testing.T) {
		agent, session, _ := newBusySessionAgent(t, "busy")
		ctx, cancel := context.WithCancel(t.Context())
		session.mu.Lock()
		session.cancel = cancel
		session.mu.Unlock()
		err := agent.retireSessionPredecessor(ctx, session.id, session)
		require.ErrorIs(t, err, errSessionCloseUnsettled)
		<-session.turn
	})

	t.Run("close failure", func(t *testing.T) {
		agent := NewAgent()
		session, cleanup := newStartedAgentSessionForTest(t, agent, "failed")
		defer cleanup()
		session.client = startedClientWithCloseError(t, errors.New("close refused"))
		require.Error(t, agent.retireSessionPredecessor(t.Context(), session.id, session))
	})
}

func TestCloseSessionUnsettledResidualBranch(t *testing.T) {
	agent, session, _ := newBusySessionAgent(t, "busy-close")
	ctx, cancel := context.WithCancel(t.Context())
	session.mu.Lock()
	session.cancel = cancel
	session.mu.Unlock()
	_, err := agent.CloseSession(ctx, acp.CloseSessionRequest{SessionId: session.id})
	require.ErrorIs(t, err, errSessionCloseUnsettled)
	<-session.turn
}

func TestResumeAndLoadResidualBranches(t *testing.T) {
	carrierOptions := func(value string) ClaudeOptions {
		return ClaudeOptions{Env: map[string]string{"TOKEN": value}}
	}

	for _, method := range []string{"resume", "load"} {
		t.Run(method+" retirement", func(t *testing.T) {
			agent, session, _ := newBusySessionAgent(t, acp.SessionId(method+"-busy"))
			oldOptions := carrierOptions("old")
			session.configuration = configurationFromOptions(oldOptions)
			session.fingerprint = sessionStartFingerprint(sessionStart{Cwd: session.cwd, MetaOptions: oldOptions})
			ctx, cancel := context.WithCancel(t.Context())
			session.mu.Lock()
			session.cancel = cancel
			session.mu.Unlock()
			if method == "resume" {
				_, err := agent.ResumeSession(ctx, ResumeSessionRequest(session.id, session.cwd,
					WithSessionMeta(carrierOptions("new").Meta())))
				require.ErrorIs(t, err, errSessionCloseUnsettled)
			} else {
				_, err := agent.LoadSession(ctx, LoadSessionRequest(session.id, session.cwd,
					WithSessionMeta(carrierOptions("new").Meta())))
				require.ErrorIs(t, err, errSessionCloseUnsettled)
			}
			<-session.turn
		})
	}

	t.Run("stored resume mismatch", func(t *testing.T) {
		store := NewInMemorySessionStore()
		id := acp.SessionId("stored-mismatch")
		oldOptions := carrierOptions("old")
		require.NoError(t, store.Append(t.Context(), SessionKey{SessionID: string(id)}, testStoredSessionEntries(t, oldOptions)))
		agent := NewAgent(WithSessionStore(store))
		_, err := agent.ResumeSession(t.Context(), ResumeSessionRequest(id, t.TempDir(),
			WithSessionMeta(carrierOptions("new").Meta())))
		require.Error(t, err)
	})

	t.Run("active load mismatch", func(t *testing.T) {
		agent, _, _ := newFakeLifecycleAgent(t, newFakeClaudeTransport())
		created, err := agent.NewSession(t.Context(), NewSessionRequest(t.TempDir()))
		require.NoError(t, err)
		_, err = agent.LoadSession(t.Context(), LoadSessionRequest(created.SessionId, t.TempDir()))
		require.Error(t, err)
		require.NoError(t, agent.Close())
	})

	for _, phase := range []string{"replay", "usage", "success"} {
		t.Run("active load "+phase, func(t *testing.T) {
			store := NewInMemorySessionStore()
			agent, conn, _ := newFakeLifecycleAgent(t, newFakeClaudeTransport(), WithSessionStore(store))
			cwd := t.TempDir()
			created, err := agent.NewSession(t.Context(), NewSessionRequest(cwd))
			require.NoError(t, err)
			entries := testStoredSessionEntries(t, ClaudeOptions{})
			if phase == "replay" {
				entries = testStoredSessionEntries(t, ClaudeOptions{}, json.RawMessage(
					`{"type":"user","message":{"content":"replay me"}}`,
				))
			}
			require.NoError(t, store.Replace(t.Context(), SessionKey{SessionID: string(created.SessionId)}, []SessionStoreReplacement{{
				Key: SessionKey{SessionID: string(created.SessionId)}, Entries: entries,
			}}))
			if phase != "success" {
				conn.sessionUpdateErr = errors.New(phase + " update refused")
			}
			_, err = agent.LoadSession(t.Context(), LoadSessionRequest(created.SessionId, cwd))
			if phase == "success" {
				require.NoError(t, err)
			} else {
				require.ErrorContains(t, err, "update refused")
			}
			conn.sessionUpdateErr = nil
			require.NoError(t, agent.Close())
		})
	}
}

func TestResumeAndLoadRefuseClosureAfterLifecycleAdmission(t *testing.T) {
	for _, method := range []string{"resume", "load"} {
		t.Run(method, func(t *testing.T) {
			agent := NewAgent()
			id := acp.SessionId(method + "-closed-after-admission")
			flight := &sessionLifecycleFlight{admission: make(chan struct{}, 1)}
			agent.lifecycleFlights[id] = flight

			done := make(chan error, 1)
			go func() {
				if method == "resume" {
					_, err := agent.ResumeSession(t.Context(), ResumeSessionRequest(id, t.TempDir()))
					done <- err

					return
				}

				_, err := agent.LoadSession(t.Context(), LoadSessionRequest(id, t.TempDir()))
				done <- err
			}()

			require.Eventually(t, func() bool {
				agent.mu.Lock()
				defer agent.mu.Unlock()

				return flight.waiters == 1
			}, time.Second, time.Millisecond)

			agent.mu.Lock()
			agent.closed = true
			agent.mu.Unlock()
			flight.admission <- struct{}{}

			requireAgentClosedRefusal(t, <-done)
		})
	}
}

func TestStartSessionImageAndEstablishmentResidualBranches(t *testing.T) {
	t.Run("image scratch", func(t *testing.T) {
		original := imageScratchMkdirTemp
		imageScratchMkdirTemp = func(string, string) (string, error) { return "", errors.New("image scratch refused") }
		t.Cleanup(func() { imageScratchMkdirTemp = original })
		agent := NewAgent(WithScratchDir(t.TempDir()), WithHome(t.TempDir()))
		_, err := agent.startSession(t.Context(), "image-failure", sessionStart{Cwd: t.TempDir()})
		require.ErrorContains(t, err, "image scratch refused")
	})

	t.Run("managed auth release on copy failure", func(t *testing.T) {
		originalCopy := copyClaudeConfigFiles
		copyClaudeConfigFiles = func(string, string, claude.Options) error { return errors.New("copy refused") }
		t.Cleanup(func() { copyClaudeConfigFiles = originalCopy })
		agent := newAuthAgent(t, WithHostAuthority(residualCallbackAuthority()))
		_, err := agent.startSession(t.Context(), "release-failure", sessionStart{Cwd: t.TempDir()})
		require.ErrorContains(t, err, "copy refused")
	})

	t.Run("local start failure settles gate", func(t *testing.T) {
		transport := newFakeClaudeTransport()
		transport.startErr = errors.New("start refused")
		agent := NewAgent(WithScratchDir(t.TempDir()), WithHome(t.TempDir()))
		agent.setConnection(&localAgentConnection{agent: agent, hooks: &postResponseHooks{}})
		installFakeClaudeClient(agent, transport)
		_, err := agent.startSession(t.Context(), "local-start-failure", sessionStart{Cwd: t.TempDir()})
		require.ErrorContains(t, err, "start refused")
	})
}

func TestRefreshMCPRegistryAndRelaunchResidualBranches(t *testing.T) {
	t.Run("prompt refresh failure", func(t *testing.T) {
		agent, _, _ := newFakeLifecycleAgent(t, newFakeClaudeTransport())
		session := newSessionForTransport(t, agent, "prompt-refresh", newFakeClaudeTransport())
		session.mcpRefreshPending = true
		session.canRelaunch = true
		session.client = startedClientWithCloseError(t, ErrContainmentIncomplete)
		_, err := session.Prompt(t.Context(), TextPromptRequest(session.id, "refresh-turn", "hello"))
		require.ErrorContains(t, err, ErrContainmentIncomplete.Error())
	})

	t.Run("successful refresh", func(t *testing.T) {
		first := newFakeClaudeTransport()
		second := newFakeClaudeTransport()
		agent, _, _ := newFakeLifecycleAgent(t, first)
		agent.newClaudeClient = func(log *slog.Logger, options claude.Options) *claude.Client {
			return claude.NewClient(log, options, second)
		}
		session := newSessionForTransport(t, agent, "refresh", first)
		session.mcpRefreshPending = true
		session.canRelaunch = true
		require.NoError(t, session.refreshMCPRegistry(t.Context()))
		require.False(t, session.mcpRefreshPending)
		require.NoError(t, session.Close(t.Context()))
	})

	for _, phase := range []string{"containment", "output style", "effort"} {
		t.Run(phase, func(t *testing.T) {
			first := newFakeClaudeTransport()
			second := newFakeClaudeTransport()
			agent, _, _ := newFakeLifecycleAgent(t, first)
			session := newSessionForTransport(t, agent, acp.SessionId("relaunch-"+phase), first)
			session.canRelaunch = true
			if phase == "containment" {
				session.client = startedClientWithCloseError(t, ErrContainmentIncomplete)
			} else {
				if phase == "output style" {
					session.outputStyle = "concise"
					second.controlErr = map[string]error{}
					second.controlErr["apply_flag_settings"] = errors.New("style refused")
				} else {
					session.effort = "high"
					second.controlErr = map[string]error{}
					second.controlErr["apply_flag_settings"] = errors.New("effort refused")
				}
				agent.newClaudeClient = func(log *slog.Logger, options claude.Options) *claude.Client {
					return claude.NewClient(log, options, second)
				}
			}
			err := session.relaunchClient(t.Context(), session.client, session.clientOptions)
			require.Error(t, err)
		})
	}
}

func residualCanceledContext() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	return ctx
}
