package claudeacp

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/stretchr/testify/require"
)

type containmentTestLock struct{}

func (containmentTestLock) Duplicate() (*os.File, error) { return nil, errors.New("unavailable") }

func TestContainmentModeAndValidationAcrossPlatforms(t *testing.T) {
	originalGOOS := runtimeGOOS
	t.Cleanup(func() { runtimeGOOS = originalGOOS })

	runtimeGOOS = "linux"
	if got := containmentMode(Options{}); got != RuntimeContainmentSharedIdentity {
		t.Fatalf("Linux mode = %q", got)
	}
	if got := containmentMode(Options{ProcessIsolation: &ProcessIsolation{UID: 64251, GID: 64252}}); got != RuntimeContainmentAuthoritative {
		t.Fatalf("Linux explicit mode = %q", got)
	}
	if got := containmentMode(Options{DarwinBestEffortContainment: true}); got != RuntimeContainmentUnavailable {
		t.Fatalf("off-Darwin opt-in mode = %q", got)
	}
	if err := validateContainmentOptions(Options{DarwinBestEffortContainment: true}); err == nil {
		t.Fatal("off-Darwin opt-in was accepted")
	}

	runtimeGOOS = "windows"
	if got := containmentMode(Options{}); got != RuntimeContainmentUnavailable {
		t.Fatalf("Windows mode = %q", got)
	}

	runtimeGOOS = "darwin"
	if got := containmentMode(Options{DarwinBestEffortContainment: true}); got != RuntimeContainmentBestEffort {
		t.Fatalf("Darwin opt-in mode = %q", got)
	}
	if got := containmentMode(Options{}); got != RuntimeContainmentUnavailable {
		t.Fatalf("Darwin default mode = %q", got)
	}
	if err := validateContainmentOptions(Options{Env: map[string]string{"acp_go_claude_internal_bad": "value"}}); err == nil {
		t.Fatal("reserved private environment was accepted")
	}
	if err := validateContainmentOptions(Options{}); err != nil {
		t.Fatal(err)
	}

	runtimeGOOS = "freebsd"
	if got := containmentMode(Options{}); got != RuntimeContainmentUnavailable {
		t.Fatalf("unsupported mode = %q", got)
	}
}

// TestContainmentModeReportsASharedAgentIdentity proves the reported boundary
// names what the launch actually proves. A supervisor that runs the agent under
// its own identity still proves whole-tree lifecycle, so it is not best-effort
// and not unavailable; what it does not prove is a credential boundary between
// itself and the agent, so it must not keep calling itself authoritative.
func TestContainmentModeReportsASharedAgentIdentity(t *testing.T) {
	originalGOOS, originalUID := runtimeGOOS, containmentEffectiveUID
	t.Cleanup(func() { runtimeGOOS, containmentEffectiveUID = originalGOOS, originalUID })

	runtimeGOOS = "linux"
	containmentEffectiveUID = func() int { return 1000 }

	shared := Options{ProcessIsolation: &ProcessIsolation{UID: 1000, GID: 1000}}
	require.Equal(t, RuntimeContainmentSharedIdentity, containmentMode(shared))
	require.True(t, RuntimeContainmentSharedIdentity.provesWholeTreeLifecycle())
	require.False(t, sharedProcessIdentity(nil))

	// An omitted policy launches the native tree as this process's own
	// identity, so the shared report is the only truthful one — root included.
	require.Equal(t, RuntimeContainmentSharedIdentity, containmentMode(Options{}))
	require.Equal(
		t,
		RuntimeContainmentAuthoritative,
		containmentMode(Options{ProcessIsolation: &ProcessIsolation{UID: 64251, GID: 64252}}),
	)

	containmentEffectiveUID = func() int { return 0 }
	require.Equal(t, RuntimeContainmentAuthoritative, containmentMode(shared))
	require.Equal(t, RuntimeContainmentSharedIdentity, containmentMode(Options{}))

	containmentEffectiveUID = func() int { return 1000 }
	runtimeGOOS = "darwin"
	require.Equal(t, RuntimeContainmentUnavailable, containmentMode(shared))
}

// TestSharedIdentityAgentKeepsItsLifecycleSurfaces proves the new mode is
// reported to the embedding and still admits every surface a proven tree is
// allowed: the descendant inventory and the runtime generation both belong to
// the lifecycle boundary, which a shared identity does not weaken.
func TestSharedIdentityAgentKeepsItsLifecycleSurfaces(t *testing.T) {
	originalGOOS, originalUID := runtimeGOOS, containmentEffectiveUID
	t.Cleanup(func() { runtimeGOOS, containmentEffectiveUID = originalGOOS, originalUID })

	runtimeGOOS = "linux"
	containmentEffectiveUID = func() int { return 1000 }

	var observed RuntimeContainmentMode

	agent := NewAgent(
		WithProcessIsolation(ProcessIsolation{UID: 1000, GID: 1000}),
		WithScratchDir(t.TempDir()),
		WithRuntimeResourceHooks(RuntimeResourceHooks{
			ObserveContainment: func(_ context.Context, mode RuntimeContainmentMode) { observed = mode },
		}),
	)
	require.Equal(t, RuntimeContainmentSharedIdentity, observed)
	require.Equal(t, RuntimeContainmentSharedIdentity, agent.ContainmentMode())
	require.True(t, agent.descendantProcesses.authoritative)

	generation, err := agent.prepareDiscoveryGeneration(t.Context())
	require.NoError(t, err)
	require.NoError(t, generation.Release(true))
}

func TestStandaloneIsolationDefaultsAndFencesDurableHome(t *testing.T) {
	const stateRoot = "/var/lib/acp-go-claude"
	isolation := ProcessIsolation{StandaloneOwnerID: "deployment-1", StandaloneStateRoot: stateRoot}

	defaulted := NewAgent(WithProcessIsolation(isolation))
	require.Equal(t, stateRoot, defaulted.options.Home)

	mismatched := NewAgent(WithHome("/var/lib/other"), WithProcessIsolation(isolation))
	_, err := mismatched.Initialize(t.Context(), acp.InitializeRequest{})
	require.ErrorContains(t, err, "WithHome must equal ProcessIsolation.StandaloneStateRoot")

	borrowed := Options{Home: "/unchanged", ProcessIsolation: &ProcessIsolation{IdentityLock: containmentTestLock{}}}
	require.NoError(t, normalizeStandaloneHome(&borrowed))
	require.Equal(t, "/unchanged", borrowed.Home)
}

func TestAgentRejectsManagedRootEnvironmentOverrides(t *testing.T) {
	for _, key := range []string{
		claudeConfigDirEnv, homeEnv, "XDG_CACHE_HOME", xdgConfigHomeEnv,
		"XDG_DATA_HOME", "XDG_RUNTIME_DIR", "XDG_STATE_HOME",
	} {
		t.Run(key, func(t *testing.T) {
			agent := NewAgent(WithEnv(map[string]string{key: "/attacker-selected"}))
			_, err := agent.Initialize(t.Context(), acp.InitializeRequest{})
			require.ErrorContains(t, err, "managed by the process isolation policy")
		})
	}
}

func TestAgentContainmentModeObservationAndWarning(t *testing.T) {
	if got := (*Agent)(nil).ContainmentMode(); got != RuntimeContainmentUnavailable {
		t.Fatalf("nil agent mode = %q", got)
	}

	originalGOOS := runtimeGOOS
	t.Cleanup(func() { runtimeGOOS = originalGOOS })
	runtimeGOOS = "darwin"

	var observed []RuntimeContainmentMode
	var logs bytes.Buffer
	agent := NewAgent(
		WithDarwinBestEffortContainment(),
		WithLogger(slog.New(slog.NewJSONHandler(&logs, nil))),
		WithRuntimeResourceHooks(RuntimeResourceHooks{
			ObserveContainment: func(_ context.Context, mode RuntimeContainmentMode) {
				observed = append(observed, mode)
			},
		}),
	)
	if agent.ContainmentMode() != RuntimeContainmentBestEffort {
		t.Fatalf("mode = %q", agent.ContainmentMode())
	}
	if len(observed) != 1 || observed[0] != RuntimeContainmentBestEffort {
		t.Fatalf("observations = %v", observed)
	}
	if !strings.Contains(logs.String(), `"containment":"best_effort"`) || !strings.Contains(logs.String(), "escaped descendants may survive") {
		t.Fatalf("warning = %q", logs.String())
	}

	runtimeGOOS = "linux"
	invalid := NewAgent(WithDarwinBestEffortContainment())
	if _, err := invalid.Initialize(t.Context(), acp.InitializeRequest{}); err == nil || !strings.Contains(err.Error(), "supported only on darwin") {
		t.Fatalf("off-Darwin initialization error = %v", err)
	}
}

func TestPrepareDarwinGenerationResources(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("Darwin generation registry is platform-specific")
	}

	originalMkdir := mkdirDarwinGeneration
	originalRemove := removeDarwinGeneration
	originalChmod := chmodDarwinGeneration
	t.Cleanup(func() {
		mkdirDarwinGeneration = originalMkdir
		removeDarwinGeneration = originalRemove
		chmodDarwinGeneration = originalChmod
	})

	if generation, err := (*Agent)(nil).prepareDarwinGeneration(t.Context(), RuntimeResourcePrompt); err == nil || generation != nil {
		t.Fatalf("nil agent generation=%v err=%v", generation, err)
	}
	if generation, err := NewAgent().prepareDarwinGeneration(t.Context(), RuntimeResourcePrompt); err == nil || generation != nil {
		t.Fatalf("authoritative agent generation=%v err=%v", generation, err)
	}

	want := errors.New("resource")
	newAgent := func(parent string, reserve func(context.Context, RuntimeResourceKind) (func(), error)) *Agent {
		original := runtimeGOOS
		runtimeGOOS = "darwin"
		agent := NewAgent(
			WithScratchDir(parent),
			WithDarwinBestEffortContainment(),
			WithRuntimeResourceHooks(RuntimeResourceHooks{ReserveScratchRoot: reserve}),
		)
		runtimeGOOS = original

		return agent
	}

	agent := newAgent(t.TempDir(), func(context.Context, RuntimeResourceKind) (func(), error) { return nil, want })
	if _, err := agent.prepareDarwinGeneration(t.Context(), RuntimeResourcePrompt); !errors.Is(err, want) {
		t.Fatalf("reserve error = %v", err)
	}

	reserved := 0
	released := 0
	reserve := func(context.Context, RuntimeResourceKind) (func(), error) {
		reserved++

		return func() { released++ }, nil
	}
	fileParent := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(fileParent, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := newAgent(fileParent, reserve).prepareDarwinGeneration(t.Context(), RuntimeResourcePrompt); err == nil || reserved != 1 || released != 1 {
		t.Fatalf("parent error=%v reserved=%d released=%d", err, reserved, released)
	}

	parent := t.TempDir()
	mkdirDarwinGeneration = func(string, string) (string, error) { return "", want }
	if _, err := newAgent(parent, reserve).prepareDarwinGeneration(t.Context(), RuntimeResourcePrompt); !errors.Is(err, want) || released != 2 {
		t.Fatalf("mkdir error=%v released=%d", err, released)
	}
	mkdirDarwinGeneration = originalMkdir

	chmodDarwinGeneration = func(string, os.FileMode) error { return want }
	if _, err := newAgent(parent, reserve).prepareDarwinGeneration(t.Context(), RuntimeResourcePrompt); !errors.Is(err, want) || released != 3 {
		t.Fatalf("chmod error=%v released=%d", err, released)
	}
	chmodDarwinGeneration = originalChmod

	registry := filepath.Join(parent, "acp-go-claude-containment")
	if err := os.WriteFile(registry, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := newAgent(parent, reserve).prepareDarwinGeneration(t.Context(), RuntimeResourcePrompt); err == nil || released != 4 {
		t.Fatalf("record error=%v released=%d", err, released)
	}
	removeDarwinGeneration = func(string) error { return want }
	if _, err := newAgent(parent, reserve).prepareDarwinGeneration(t.Context(), RuntimeResourcePrompt); err == nil || released != 4 {
		t.Fatalf("record/remove error=%v released=%d", err, released)
	}
	removeDarwinGeneration = originalRemove
	if err := os.Remove(registry); err != nil {
		t.Fatal(err)
	}

	generation, err := newAgent(parent, reserve).prepareDarwinGeneration(t.Context(), RuntimeResourcePrompt)
	if err != nil {
		t.Fatal(err)
	}
	beforeRelease := released
	releaseErr := generation.Release(false)
	if releaseErr != nil || released != beforeRelease {
		t.Fatalf("incomplete release error=%v releases=%d", releaseErr, released)
	}
	removeDarwinGeneration = func(string) error { return want }
	releaseErr = generation.Release(true)
	if !errors.Is(releaseErr, want) || released != beforeRelease {
		t.Fatalf("failed complete release error=%v releases=%d", releaseErr, released)
	}
	removeDarwinGeneration = originalRemove

	generation, err = newAgent(parent, reserve).prepareDarwinGeneration(t.Context(), RuntimeResourcePrompt)
	if err != nil {
		t.Fatal(err)
	}
	releaseErr = generation.Release(true)
	if releaseErr != nil || released != beforeRelease+1 {
		t.Fatalf("complete release error=%v releases=%d", releaseErr, released)
	}
}

func TestPrepareUsageGenerationResources(t *testing.T) {
	originalMkdir := mkdirDarwinGeneration
	originalRemove := removeDarwinGeneration
	originalChmod := chmodDarwinGeneration
	t.Cleanup(func() {
		mkdirDarwinGeneration = originalMkdir
		removeDarwinGeneration = originalRemove
		chmodDarwinGeneration = originalChmod
	})

	if generation, err := (*Agent)(nil).prepareDiscoveryGeneration(t.Context()); err == nil || generation != nil {
		t.Fatalf("nil agent generation=%v err=%v", generation, err)
	}

	bestEffort := NewAgent(WithScratchDir(t.TempDir()))
	bestEffort.containmentMode = RuntimeContainmentBestEffort
	generation, err := bestEffort.prepareDiscoveryGeneration(t.Context())
	if runtime.GOOS == "darwin" {
		require.NoError(t, err)
		require.NoError(t, generation.Release(true))
	} else {
		require.Nil(t, generation)
		require.ErrorIs(t, err, ErrProcessContainmentIncomplete)
	}

	unavailable := NewAgent()
	unavailable.containmentMode = RuntimeContainmentUnavailable
	_, err = unavailable.prepareDiscoveryGeneration(t.Context())
	require.ErrorIs(t, err, ErrProcessContainmentIncomplete)

	wantErr := errors.New("usage generation failure")
	authoritative := func(parent string, reserve func(context.Context, RuntimeResourceKind) (func(), error)) *Agent {
		agent := NewAgent(
			WithScratchDir(parent),
			WithRuntimeResourceHooks(RuntimeResourceHooks{ReserveScratchRoot: reserve}),
		)
		agent.containmentMode = RuntimeContainmentAuthoritative

		return agent
	}
	_, err = authoritative(t.TempDir(), func(context.Context, RuntimeResourceKind) (func(), error) {
		return nil, wantErr
	}).prepareDiscoveryGeneration(t.Context())
	require.ErrorIs(t, err, wantErr)

	releases := 0
	reserve := func(context.Context, RuntimeResourceKind) (func(), error) {
		return func() { releases++ }, nil
	}
	fileParent := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(fileParent, nil, 0o600))
	_, err = authoritative(fileParent, reserve).prepareDiscoveryGeneration(t.Context())
	require.Error(t, err)
	require.Equal(t, 1, releases)

	parent := t.TempDir()
	mkdirDarwinGeneration = func(string, string) (string, error) { return "", wantErr }
	_, err = authoritative(parent, reserve).prepareDiscoveryGeneration(t.Context())
	require.ErrorIs(t, err, wantErr)
	require.Equal(t, 2, releases)
	mkdirDarwinGeneration = originalMkdir

	chmodDarwinGeneration = func(string, os.FileMode) error { return wantErr }
	_, err = authoritative(parent, reserve).prepareDiscoveryGeneration(t.Context())
	require.ErrorIs(t, err, wantErr)
	require.Equal(t, 3, releases)
	chmodDarwinGeneration = originalChmod

	generation, err = authoritative(parent, reserve).prepareDiscoveryGeneration(t.Context())
	require.NoError(t, err)
	require.NoError(t, generation.Release(false))
	require.Equal(t, 3, releases)
	removeDarwinGeneration = func(string) error { return wantErr }
	require.ErrorIs(t, generation.Release(true), wantErr)
	require.Equal(t, 3, releases)
	removeDarwinGeneration = originalRemove

	generation, err = authoritative(parent, reserve).prepareDiscoveryGeneration(t.Context())
	require.NoError(t, err)
	require.NoError(t, generation.Release(true))
	require.Equal(t, 4, releases)
}

// TestAgentSessionDefaultsToOrdinaryExecution proves omitting
// WithProcessIsolation is the ordinary default: session establishment
// succeeds, and every native launch is handed a clone of the one
// current-identity capture — the ambient environment included — rather than an
// isolation policy.
func TestAgentSessionDefaultsToOrdinaryExecution(t *testing.T) {
	t.Setenv("ACP_GO_CLAUDE_TEST_CANARY", "ambient-canary")

	agent, _, _ := newFakeLifecycleAgent(t, nil)
	t.Cleanup(func() { require.NoError(t, agent.Close()) })

	resp, err := agent.NewSession(context.Background(), NewSessionRequest(t.TempDir()))
	require.NoError(t, err, "NewSession without isolation")
	require.NotEmpty(t, resp.SessionId)

	isolation := agent.claudeIsolation()
	require.NotNil(t, isolation)
	require.True(t, isolation.Implicit)
	require.Equal(t, int64(os.Geteuid()), int64(isolation.UID))
	require.Equal(t, int64(os.Getegid()), int64(isolation.GID))
	require.Equal(t, "ambient-canary", isolation.BaseEnvironment["ACP_GO_CLAUDE_TEST_CANARY"])
	require.Nil(t, isolation.IdentityLock)
	require.Nil(t, isolation.AuthorityDomain)
	require.Empty(t, isolation.StandaloneOwnerID)
	require.Empty(t, isolation.StandaloneStateRoot)

	isolation.BaseEnvironment["ACP_GO_CLAUDE_TEST_CANARY"] = "mutated"
	require.Equal(t, "ambient-canary", agent.claudeIsolation().BaseEnvironment["ACP_GO_CLAUDE_TEST_CANARY"],
		"implicit capture must be cloned, not shared")

	require.Nil(t, (&Agent{}).claudeIsolation())
}

// TestExplicitProcessIsolationPreservesPolicy proves supplying
// WithProcessIsolation stays explicit hardening: the policy reaches native
// launches verbatim with no ambient environment mixed in, and an invalid
// policy fails before any native process can spawn, with no ordinary-mode
// fallback.
func TestExplicitProcessIsolationPreservesPolicy(t *testing.T) {
	t.Setenv("ACP_GO_CLAUDE_TEST_CANARY", "ambient-canary")

	agent := NewAgent(WithProcessIsolation(ProcessIsolation{
		UID: 64251, GID: 64252,
		BaseEnvironment:     map[string]string{"PATH": "/policy/bin", "USER": "acp"},
		StandaloneOwnerID:   "acp-go-claude-tests",
		StandaloneStateRoot: "/var/lib/acp-go-claude-tests",
	}))
	t.Cleanup(func() { require.NoError(t, agent.Close()) })

	isolation := agent.claudeIsolation()
	require.NotNil(t, isolation)
	require.False(t, isolation.Implicit)
	require.Equal(t, uint32(64251), isolation.UID)
	require.Equal(t, uint32(64252), isolation.GID)
	require.NotContains(t, isolation.BaseEnvironment, "ACP_GO_CLAUDE_TEST_CANARY")
	require.Equal(t, "/policy/bin", isolation.BaseEnvironment["PATH"])
	require.Nil(t, agent.implicitIsolation, "explicit policy must not capture an implicit fallback")
	require.Equal(t, "/var/lib/acp-go-claude-tests", agent.options.Home,
		"standalone state root still names the managed home")

	invalid := NewAgent(WithProcessIsolation(ProcessIsolation{UID: 0, GID: 0}))
	t.Cleanup(func() { require.NoError(t, invalid.Close()) })
	invalid.newClaudeClient = func(*slog.Logger, claude.Options) *claude.Client {
		t.Fatal("an invalid explicit policy must fail before any native client exists")

		return nil
	}
	_, err := invalid.NewSession(context.Background(), NewSessionRequest(t.TempDir()))
	require.Error(t, err, "invalid explicit policy must fail session establishment closed")
}
