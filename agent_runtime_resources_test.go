package claudeacp

import (
	"context"
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

func TestRuntimeResourceHooks(t *testing.T) {
	options := Options{}
	WithRuntimeResourceHooks(RuntimeResourceHooks{AcquireNativeRoot: func(context.Context, RuntimeResourceKind) (func(), error) {
		return func() {}, nil
	}})(&options)
	require.NotNil(t, options.RuntimeResourceHooks.AcquireNativeRoot)

	release, err := acquireNativeRoot(t.Context(), RuntimeResourceHooks{}, RuntimeResourceSession)
	require.NoError(t, err)
	release()

	wantErr := errors.New("full")
	_, err = reserveScratchRoot(t.Context(), RuntimeResourceHooks{ReserveScratchRoot: func(context.Context, RuntimeResourceKind) (func(), error) {
		return nil, wantErr
	}}, RuntimeResourceSession)
	require.ErrorIs(t, err, wantErr)

	_, err = acquireNativeRoot(t.Context(), RuntimeResourceHooks{AcquireNativeRoot: func(context.Context, RuntimeResourceKind) (func(), error) {
		return nil, nil //nolint:nilnil // A nil release is the invalid hook result under test.
	}}, RuntimeResourcePrompt)
	require.ErrorContains(t, err, "nil release")

	releases := 0
	release, err = acquireNativeRoot(t.Context(), RuntimeResourceHooks{AcquireNativeRoot: func(_ context.Context, kind RuntimeResourceKind) (func(), error) {
		require.Equal(t, RuntimeResourceDiscovery, kind)

		return func() { releases++ }, nil
	}}, RuntimeResourceDiscovery)
	require.NoError(t, err)
	release()
	release()
	require.Equal(t, 1, releases)
}

func TestSessionResourceAdmissionFailsBeforeNativeStart(t *testing.T) {
	wantErr := errors.New("resource exhausted")
	agent, _, _ := newFakeLifecycleAgent(t, newFakeClaudeTransport(), WithRuntimeResourceHooks(RuntimeResourceHooks{
		ReserveScratchRoot: func(context.Context, RuntimeResourceKind) (func(), error) { return nil, wantErr },
	}))
	_, err := agent.NewSession(t.Context(), NewSessionRequest(t.TempDir()))
	require.ErrorIs(t, err, wantErr)

	scratchReleases := 0
	agent, _, _ = newFakeLifecycleAgent(t, newFakeClaudeTransport(), WithRuntimeResourceHooks(RuntimeResourceHooks{
		ReserveScratchRoot: func(context.Context, RuntimeResourceKind) (func(), error) {
			return func() { scratchReleases++ }, nil
		},
		AcquireNativeRoot: func(context.Context, RuntimeResourceKind) (func(), error) { return nil, wantErr },
	}))
	_, err = agent.NewSession(t.Context(), NewSessionRequest(t.TempDir()))
	require.ErrorIs(t, err, wantErr)
	require.Equal(t, 1, scratchReleases)
}

func TestSessionStartRetainsNativeAdmissionWhenQuiescenceIsUnproven(t *testing.T) {
	transport := newFakeClaudeTransport()
	transport.startErr = claude.ErrProcessContainmentIncomplete

	scratch := t.TempDir()
	nativeReleases, scratchReleases := 0, 0
	agent, _, _ := newFakeLifecycleAgent(t, transport, WithScratchDir(scratch), WithRuntimeResourceHooks(RuntimeResourceHooks{
		ReserveScratchRoot: func(context.Context, RuntimeResourceKind) (func(), error) {
			return func() { scratchReleases++ }, nil
		},
		AcquireNativeRoot: func(context.Context, RuntimeResourceKind) (func(), error) {
			return func() { nativeReleases++ }, nil
		},
	}))

	_, err := agent.NewSession(t.Context(), NewSessionRequest(
		t.TempDir(),
		WithSessionMCPServers(StdioMCPServer("test", "test-server", nil, nil)),
	))
	require.ErrorIs(t, err, claude.ErrProcessContainmentIncomplete)
	require.Zero(t, nativeReleases)
	require.Zero(t, scratchReleases)
	entries, readErr := os.ReadDir(scratch)
	require.NoError(t, readErr)
	require.NotEmpty(t, entries, "unproven process tree lost its adapter-owned MCP root")
}

func TestSessionCloseReleasesNativeAdmissionOnlyWhenTreeIsProven(t *testing.T) {
	t.Run("ordinary close error releases", func(t *testing.T) {
		releases := 0
		closeErr := errors.New("ordinary close error")
		session := closeErrorAgentSession(t, closeErr, func() { releases++ })

		err := session.Close(t.Context())
		require.ErrorIs(t, err, closeErr)
		require.Equal(t, 1, releases)
	})

	t.Run("unproven tree retains", func(t *testing.T) {
		releases := 0
		closeErr := errors.Join(errors.New("shutdown failed"), claude.ErrProcessContainmentIncomplete)
		session := closeErrorAgentSession(t, closeErr, func() { releases++ })

		err := session.Close(t.Context())
		require.ErrorIs(t, err, claude.ErrProcessContainmentIncomplete)
		require.Zero(t, releases)
	})
}

func TestSessionCloseScratchReleaseProofBoundaries(t *testing.T) {
	originalRemoveAll := sessionRemoveAll
	t.Cleanup(func() { sessionRemoveAll = originalRemoveAll })

	newRoots := func(t *testing.T) (string, *materializedSession) {
		t.Helper()
		root := t.TempDir()
		mcp := filepath.Join(root, "mcp")
		materializedRoot := filepath.Join(root, "materialized")
		require.NoError(t, os.Mkdir(mcp, 0o700))
		require.NoError(t, os.Mkdir(materializedRoot, 0o700))

		return mcp, &materializedSession{configDir: materializedRoot}
	}

	t.Run("ordinary close error deletes roots and releases both admissions", func(t *testing.T) {
		mcp, materialized := newRoots(t)
		closeErr := errors.New("ordinary close error")
		nativeReleases, scratchReleases := 0, 0
		session := closeErrorAgentSession(t, closeErr, func() { nativeReleases++ })
		session.mcpConfigDir = mcp
		session.materialized = materialized
		session.scratchRootRelease = func() { scratchReleases++ }

		err := session.Close(t.Context())
		require.ErrorIs(t, err, closeErr)
		require.Equal(t, 1, nativeReleases)
		require.Equal(t, 1, scratchReleases)
		require.NoDirExists(t, mcp)
		require.NoDirExists(t, materialized.configDir)
	})

	t.Run("unproven tree retains roots and both admissions", func(t *testing.T) {
		mcp, materialized := newRoots(t)
		nativeReleases, scratchReleases := 0, 0
		session := closeErrorAgentSession(t, claude.ErrProcessContainmentIncomplete, func() { nativeReleases++ })
		session.mcpConfigDir = mcp
		session.materialized = materialized
		session.scratchRootRelease = func() { scratchReleases++ }

		err := session.Close(t.Context())
		require.ErrorIs(t, err, claude.ErrProcessContainmentIncomplete)
		require.Zero(t, nativeReleases)
		require.Zero(t, scratchReleases)
		require.DirExists(t, mcp)
		require.DirExists(t, materialized.configDir)
	})

	t.Run("deletion failure releases native and retains scratch", func(t *testing.T) {
		mcp, materialized := newRoots(t)
		deleteErr := errors.New("delete MCP root")
		sessionRemoveAll = func(path string) error {
			require.Equal(t, mcp, path)

			return deleteErr
		}
		t.Cleanup(func() { sessionRemoveAll = originalRemoveAll })
		nativeReleases, scratchReleases := 0, 0
		session := closeErrorAgentSession(t, nil, func() { nativeReleases++ })
		session.mcpConfigDir = mcp
		session.materialized = materialized
		session.scratchRootRelease = func() { scratchReleases++ }

		err := session.Close(t.Context())
		require.ErrorIs(t, err, deleteErr)
		require.Equal(t, 1, nativeReleases)
		require.Zero(t, scratchReleases)
		require.DirExists(t, mcp)
		require.NoDirExists(t, materialized.configDir)
	})
}

func closeErrorAgentSession(t *testing.T, closeErr error, nativeRelease func()) *agentSession {
	t.Helper()

	client := claude.NewClient(nil, claude.Options{}, &closeErrTransport{Transport: newFakeClaudeTransport(), err: closeErr})
	require.NoError(t, client.Start(t.Context()))

	return &agentSession{
		agent:             NewAgent(),
		id:                "session-1",
		client:            client,
		turn:              make(chan struct{}, sessionTurnCapacity),
		nativeRootRelease: nativeRelease,
	}
}

func TestClaudeRelaunchReleasesOldTreeBeforeFreshAdmission(t *testing.T) {
	acquires := 0
	releases := 0
	agent := NewAgent(WithHome(t.TempDir()), WithRuntimeResourceHooks(RuntimeResourceHooks{
		AcquireNativeRoot: func(context.Context, RuntimeResourceKind) (func(), error) {
			acquires++
			if acquires == 2 {
				require.Equal(t, 1, releases, "replacement admitted before the old tree was released")
			}

			return func() { releases++ }, nil
		},
	}))
	agent.setConnection(newRecordingAgentClient())
	agent.newClaudeClient = func(log *slog.Logger, options claude.Options) *claude.Client {
		require.NotNil(t, options.ObserveProcessInventory)
		require.NotNil(t, options.ObserveBoundaryComplete)

		return claude.NewClient(log, options, newFakeClaudeTransport())
	}

	response, err := agent.NewSession(t.Context(), NewSessionRequest(t.TempDir()))
	require.NoError(t, err)
	session := agent.sessions[response.SessionId]
	require.NoError(t, session.client.Close())

	require.NoError(t, session.ensureClientAlive(t.Context()))
	require.Equal(t, 2, acquires)
	require.Equal(t, 1, releases)
	require.NoError(t, session.Close(t.Context()))
	require.Equal(t, 2, releases)
}

func TestClaudeFirstMCPPromptRefreshesFixedRegistryOnce(t *testing.T) {
	acquires := 0
	releases := 0
	var (
		launchOptions []claude.Options
		transports    []*fakeClaudeTransport
	)
	agent := NewAgent(
		WithHome(t.TempDir()),
		WithScratchDir(t.TempDir()),
		WithDefaultModel("sonnet"),
		WithRuntimeResourceHooks(RuntimeResourceHooks{
			AcquireNativeRoot: func(context.Context, RuntimeResourceKind) (func(), error) {
				acquires++

				return func() { releases++ }, nil
			},
		}),
	)
	agent.setConnection(newRecordingAgentClient())
	agent.newClaudeClient = func(log *slog.Logger, options claude.Options) *claude.Client {
		transport := newFakeClaudeTransport()
		launchOptions = append(launchOptions, options)
		transports = append(transports, transport)

		return claude.NewClient(log, options, transport)
	}

	response, err := agent.NewSession(t.Context(), NewSessionRequest(
		t.TempDir(),
		WithSessionMCPServers(
			HTTPMCPServer("wagie", "http://127.0.0.1:1/mcp", map[string]string{"Authorization": "Bearer test"}),
			StdioMCPServer("playwright", "playwright-mcp", []string{"--headless"}, nil),
		),
	))
	require.NoError(t, err)
	require.Len(t, launchOptions, 1)
	require.NotEmpty(t, launchOptions[0].MCPConfigPath)
	require.NotNil(t, launchOptions[0].AcquireVersionDiscovery)
	require.NotNil(t, launchOptions[0].PrepareDarwinVersionGeneration)
	generation, err := launchOptions[0].PrepareDarwinGeneration(t.Context())
	require.Error(t, err)
	require.Nil(t, generation)
	discoveryRelease, err := launchOptions[0].AcquireVersionDiscovery(t.Context())
	require.NoError(t, err)
	discoveryRelease()
	generation, err = launchOptions[0].PrepareDarwinVersionGeneration(t.Context())
	require.Error(t, err)
	require.Nil(t, generation)

	prompt := TextPromptRequest(response.SessionId, "turn-1", "hello")
	result, err := agent.Prompt(t.Context(), prompt)
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, result.StopReason)
	require.Len(t, launchOptions, 2)
	require.Equal(t, launchOptions[0].MCPConfigPath, launchOptions[1].MCPConfigPath)
	require.Empty(t, launchOptions[1].ResumeID, "a new pre-prompt session must keep its session-id launch semantics")
	require.False(t, launchOptions[1].ForkSession)
	require.Equal(t, "sonnet", launchOptions[1].Model)
	require.Equal(t, 3, acquires)
	require.Equal(t, 2, releases, "the provisional tree must be released before replacement admission")
	require.Len(t, transports, 2)

	result, err = agent.Prompt(t.Context(), TextPromptRequest(response.SessionId, "turn-2", "again"))
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, result.StopReason)
	require.Len(t, launchOptions, 2, "the authorized registry refresh must occur only once")

	_, err = agent.CloseSession(t.Context(), acp.CloseSessionRequest{SessionId: response.SessionId})
	require.NoError(t, err)
	require.Equal(t, 3, releases)
}

func TestClaudeCancelledMCPRefreshUnwindsAndRetries(t *testing.T) {
	acquires := 0
	releases := 0
	launches := 0
	agent := NewAgent(
		WithHome(t.TempDir()),
		WithScratchDir(t.TempDir()),
		WithRuntimeResourceHooks(RuntimeResourceHooks{
			AcquireNativeRoot: func(context.Context, RuntimeResourceKind) (func(), error) {
				acquires++

				return func() { releases++ }, nil
			},
		}),
	)
	agent.setConnection(newRecordingAgentClient())
	agent.newClaudeClient = func(log *slog.Logger, options claude.Options) *claude.Client {
		launches++
		transport := newFakeClaudeTransport()
		if launches == 2 {
			transport.startErr = context.Canceled
		}

		return claude.NewClient(log, options, transport)
	}

	response, err := agent.NewSession(t.Context(), NewSessionRequest(
		t.TempDir(),
		WithSessionMCPServers(HTTPMCPServer("wagie", "http://127.0.0.1:1/mcp", nil)),
	))
	require.NoError(t, err)

	_, err = agent.Prompt(t.Context(), TextPromptRequest(response.SessionId, "turn-1", "hello"))
	requireTurnFailure(t, err, -32603, failureCauseTransport, context.Canceled.Error())
	require.Equal(t, 2, acquires)
	require.Equal(t, 2, releases, "both the provisional and failed replacement admissions must unwind")

	result, err := agent.Prompt(t.Context(), TextPromptRequest(response.SessionId, "turn-2", "retry"))
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, result.StopReason)
	require.Equal(t, 3, launches)
	require.Equal(t, 3, acquires)
	require.Equal(t, 2, releases)

	_, err = agent.CloseSession(t.Context(), acp.CloseSessionRequest{SessionId: response.SessionId})
	require.NoError(t, err)
	require.Equal(t, 3, releases)
}

func TestClaudeRelaunchFailsClosed(t *testing.T) {
	t.Run("previous tree proof fails", func(t *testing.T) {
		closeErr := errors.Join(errors.New("close failed"), claude.ErrProcessContainmentIncomplete)
		releases := 0
		session := &agentSession{
			agent:             NewAgent(),
			id:                "session-1",
			client:            deadClaudeClient(t, closeErr),
			canRelaunch:       true,
			nativeRootRelease: func() { releases++ },
		}

		err := session.ensureClientAlive(t.Context())
		require.ErrorIs(t, err, claude.ErrProcessContainmentIncomplete)
		require.Zero(t, releases)
		require.False(t, session.canRelaunch)
	})

	t.Run("ordinary previous close error releases and relaunches", func(t *testing.T) {
		closeErr := errors.New("close failed after proof")
		acquires, releases := 0, 0
		agent := NewAgent(WithRuntimeResourceHooks(RuntimeResourceHooks{
			AcquireNativeRoot: func(context.Context, RuntimeResourceKind) (func(), error) {
				acquires++

				return func() { releases++ }, nil
			},
		}))
		agent.newClaudeClient = func(log *slog.Logger, options claude.Options) *claude.Client {
			return claude.NewClient(log, options, newFakeClaudeTransport())
		}
		session := &agentSession{
			agent:             agent,
			id:                "session-1",
			client:            deadClaudeClient(t, closeErr),
			canRelaunch:       true,
			nativeRootRelease: func() { releases++ },
		}

		err := session.ensureClientAlive(t.Context())
		require.ErrorIs(t, err, closeErr)
		require.Equal(t, 1, acquires)
		require.Equal(t, 1, releases)
		require.True(t, session.client.Alive())
		require.NoError(t, session.Close(t.Context()))
		require.Equal(t, 2, releases)
	})

	t.Run("replacement admission fails", func(t *testing.T) {
		admissionErr := errors.New("full")
		releases := 0
		agent := NewAgent(WithRuntimeResourceHooks(RuntimeResourceHooks{
			AcquireNativeRoot: func(context.Context, RuntimeResourceKind) (func(), error) {
				return nil, admissionErr
			},
		}))
		session := &agentSession{
			agent:             agent,
			id:                "session-1",
			client:            deadClaudeClient(t, nil),
			canRelaunch:       true,
			nativeRootRelease: func() { releases++ },
		}

		err := session.ensureClientAlive(t.Context())
		require.ErrorIs(t, err, admissionErr)
		require.Equal(t, 1, releases)
	})

	t.Run("replacement quiescence is unproven", func(t *testing.T) {
		releases := 0
		agent := NewAgent(WithRuntimeResourceHooks(RuntimeResourceHooks{
			AcquireNativeRoot: func(context.Context, RuntimeResourceKind) (func(), error) {
				return func() { releases++ }, nil
			},
		}))
		agent.newClaudeClient = func(log *slog.Logger, options claude.Options) *claude.Client {
			transport := newFakeClaudeTransport()
			transport.startErr = claude.ErrProcessContainmentIncomplete

			return claude.NewClient(log, options, transport)
		}
		session := &agentSession{
			agent:             agent,
			id:                "session-1",
			client:            deadClaudeClient(t, nil),
			canRelaunch:       true,
			nativeRootRelease: func() { releases++ },
		}

		err := session.ensureClientAlive(t.Context())
		require.ErrorIs(t, err, claude.ErrProcessContainmentIncomplete)
		require.Equal(t, 1, releases, "replacement admission must remain held")
		require.False(t, session.canRelaunch)
	})

	for _, tc := range []struct {
		name        string
		outputStyle string
		effort      string
	}{
		{name: "replacement output style fails", outputStyle: "concise"},
		{name: "replacement effort fails", effort: "high"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			releases := 0
			agent := NewAgent(WithRuntimeResourceHooks(RuntimeResourceHooks{
				AcquireNativeRoot: func(context.Context, RuntimeResourceKind) (func(), error) {
					return func() { releases++ }, nil
				},
			}))
			agent.newClaudeClient = func(log *slog.Logger, options claude.Options) *claude.Client {
				transport := newFakeClaudeTransport()
				transport.controlErr = map[string]error{"apply_flag_settings": errors.New("reapply failed")}

				return claude.NewClient(log, options, transport)
			}
			session := &agentSession{
				agent:             agent,
				id:                "session-1",
				client:            deadClaudeClient(t, nil),
				canRelaunch:       true,
				nativeRootRelease: func() { releases++ },
				outputStyle:       tc.outputStyle,
				effort:            tc.effort,
			}

			err := session.ensureClientAlive(t.Context())
			require.ErrorContains(t, err, "reapply failed")
			require.Equal(t, 2, releases)
		})
	}
}

func TestClaudeCancelQuarantinesUnprovenTreeAdmission(t *testing.T) {
	releases := 0
	session := closeErrorAgentSession(
		t,
		errors.Join(errors.New("containment probe failed"), claude.ErrProcessContainmentIncomplete),
		func() { releases++ },
	)
	session.cancel = func() {}
	session.canRelaunch = true

	err := session.Cancel(t.Context())
	require.ErrorIs(t, err, claude.ErrProcessContainmentIncomplete)
	require.Zero(t, releases, "an unproven native tree must retain its admission")
	require.False(t, session.canRelaunch, "an unproven native tree must disable lazy relaunch")

	// Close is deliberately repeatable, but the original proof failure remains
	// sticky so a later cleanup cannot accidentally release the quarantined slot.
	err = session.Close(t.Context())
	require.ErrorIs(t, err, claude.ErrProcessContainmentIncomplete)
	require.Zero(t, releases)

	interruptSession := closeErrorAgentSession(
		t,
		errors.Join(errors.New("containment probe failed"), claude.ErrProcessContainmentIncomplete),
		nil,
	)
	interruptSession.cancel = func() {}
	originalErr := errors.New("emit failed")
	err = interruptSession.interruptAfterEmitError(t.Context(), originalErr)
	require.ErrorIs(t, err, originalErr)
	require.ErrorIs(t, err, claude.ErrProcessContainmentIncomplete)
}

func deadClaudeClient(t *testing.T, closeErr error) *claude.Client {
	t.Helper()

	transport := newFakeClaudeTransport()
	client := claude.NewClient(nil, claude.Options{}, &closeErrTransport{Transport: transport, err: closeErr})
	require.NoError(t, client.Start(t.Context()))
	transport.errs <- errors.New("process exited")
	require.Eventually(t, func() bool { return !client.Alive() }, time.Second, time.Millisecond)

	return client
}
