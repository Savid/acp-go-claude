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
	if got := containmentMode(Options{}); got != RuntimeContainmentSharedIdentity {
		t.Fatalf("Windows mode = %q", got)
	}

	runtimeGOOS = "darwin"
	if got := containmentMode(Options{DarwinBestEffortContainment: true}); got != RuntimeContainmentBestEffort {
		t.Fatalf("Darwin opt-in mode = %q", got)
	}
	if got := containmentMode(Options{}); got != RuntimeContainmentSharedIdentity {
		t.Fatalf("Darwin default mode = %q", got)
	}
	if err := validateContainmentOptions(Options{Env: map[string]string{"acp_go_claude_internal_bad": "value"}}); err == nil {
		t.Fatal("reserved private environment was accepted")
	}
	if err := validateContainmentOptions(Options{}); err != nil {
		t.Fatal(err)
	}

	runtimeGOOS = "freebsd"
	if got := containmentMode(Options{}); got != RuntimeContainmentSharedIdentity {
		t.Fatalf("FreeBSD omission mode = %q", got)
	}
	if got := containmentMode(Options{ProcessIsolation: &ProcessIsolation{UID: 1, GID: 2}}); got != RuntimeContainmentUnavailable {
		t.Fatalf("FreeBSD explicit mode = %q", got)
	}
}

// TestOrdinaryExecutionIsPortableAcrossSupportedPlatforms proves omission is the
// ordinary default everywhere the adapter otherwise runs, and that an explicitly
// supplied hardened policy stays a strict Linux selection that fails closed
// rather than degrading to shared identity or best effort.
func TestOrdinaryExecutionIsPortableAcrossSupportedPlatforms(t *testing.T) {
	originalGOOS := runtimeGOOS
	t.Cleanup(func() { runtimeGOOS = originalGOOS })

	explicit := Options{ProcessIsolation: &ProcessIsolation{UID: 64251, GID: 64252}}

	for _, platform := range []string{"linux", "darwin", "windows", "freebsd", "openbsd"} {
		t.Run(platform, func(t *testing.T) {
			runtimeGOOS = platform

			require.Equal(t, RuntimeContainmentSharedIdentity, containmentMode(Options{}))
			require.NoError(t, validateContainmentOptions(Options{}))
			require.False(t, RuntimeContainmentSharedIdentity.provesWholeTreeLifecycle())

			if platform == "linux" {
				require.Equal(t, RuntimeContainmentAuthoritative, containmentMode(explicit))

				return
			}

			require.Equal(t, RuntimeContainmentUnavailable, containmentMode(explicit))
		})
	}
}

// TestDarwinBestEffortAndProcessIsolationAreMutuallyExclusive proves the two
// explicit options are refused together at construction: an explicit hardened
// identity policy cannot be downgraded to a process-group boundary, so the
// combination never reaches a native spawn.
func TestDarwinBestEffortAndProcessIsolationAreMutuallyExclusive(t *testing.T) {
	originalGOOS := runtimeGOOS
	t.Cleanup(func() { runtimeGOOS = originalGOOS })

	runtimeGOOS = "darwin"

	combined := Options{
		DarwinBestEffortContainment: true,
		ProcessIsolation:            &ProcessIsolation{UID: 64251, GID: 64252},
	}
	require.True(t, containmentOptionsConflict(combined))
	require.Equal(t, RuntimeContainmentUnavailable, containmentMode(combined))
	require.ErrorContains(t, validateContainmentOptions(combined), "cannot be combined")

	agent := NewAgent(
		WithDarwinBestEffortContainment(),
		WithProcessIsolation(ProcessIsolation{
			UID: 64251, GID: 64252,
			BaseEnvironment:     map[string]string{"PATH": "/policy/bin"},
			StandaloneOwnerID:   "acp-go-claude-tests",
			StandaloneStateRoot: "/var/lib/acp-go-claude-tests",
		}),
	)
	t.Cleanup(func() { require.NoError(t, agent.Close()) })

	agent.newClaudeClient = func(*slog.Logger, claude.Options) *claude.Client {
		t.Fatal("the refused combination must never construct a native client")

		return nil
	}

	require.Equal(t, RuntimeContainmentUnavailable, agent.ContainmentMode())

	_, err := agent.Initialize(t.Context(), acp.InitializeRequest{})
	require.ErrorContains(t, err, "cannot be combined")

	_, err = agent.NewSession(t.Context(), NewSessionRequest(t.TempDir()))
	require.Error(t, err)
}

// TestContainmentModeReportsASharedAgentIdentity proves shared identity has
// exactly one source — an omitted policy — and that an explicitly supplied one
// never degrades into it, not even when its ids name the very identity the
// caller already holds.
func TestContainmentModeReportsASharedAgentIdentity(t *testing.T) {
	originalGOOS := runtimeGOOS
	t.Cleanup(func() { runtimeGOOS = originalGOOS })

	runtimeGOOS = "linux"

	// An omitted policy launches native work as this process's own identity, so
	// the shared report is the only truthful one — root included.
	require.Equal(t, RuntimeContainmentSharedIdentity, containmentMode(Options{}))
	require.False(t, RuntimeContainmentSharedIdentity.provesWholeTreeLifecycle())
	require.True(t, RuntimeContainmentAuthoritative.provesWholeTreeLifecycle())

	// A supplied policy naming the caller's own identity is still a supplied
	// policy: it selects the strict boundary, which then refuses it.
	callerIdentity := Options{ProcessIsolation: &ProcessIsolation{
		UID: uint32(os.Geteuid()), GID: uint32(os.Getegid()),
	}}
	require.Equal(t, RuntimeContainmentAuthoritative, containmentMode(callerIdentity))

	runtimeGOOS = "darwin"
	require.Equal(t, RuntimeContainmentUnavailable, containmentMode(callerIdentity))
	require.Equal(t, RuntimeContainmentSharedIdentity, containmentMode(Options{}))
}

// TestSharedIdentityAgentPublishesNoDescendantInventory proves the ordinary
// mode is reported to the embedding and deliberately publishes nothing it
// cannot prove: no provider-descendant snapshot, including a terminal zero, and
// no whole-tree claim. It still gets its own scratch generation, which is
// adapter bookkeeping rather than containment evidence.
func TestSharedIdentityAgentPublishesNoDescendantInventory(t *testing.T) {
	originalGOOS := runtimeGOOS
	t.Cleanup(func() { runtimeGOOS = originalGOOS })

	runtimeGOOS = "linux"

	var (
		observed  RuntimeContainmentMode
		snapshots []int
	)

	agent := NewAgent(
		WithScratchDir(t.TempDir()),
		WithRuntimeResourceHooks(RuntimeResourceHooks{
			ObserveContainment: func(_ context.Context, mode RuntimeContainmentMode) { observed = mode },
			ObserveProcessSnapshot: func(_ context.Context, _ RuntimeProcessKind, count int) {
				snapshots = append(snapshots, count)
			},
		}),
	)
	require.Equal(t, RuntimeContainmentSharedIdentity, observed)
	require.Equal(t, RuntimeContainmentSharedIdentity, agent.ContainmentMode())
	require.False(t, agent.descendantProcesses.authoritative)

	source := agent.descendantProcesses.newSource()
	source.started(t.Context(), func() (int, bool) { return 3, true })
	source.completed(t.Context())
	require.Empty(t, snapshots, "ordinary execution publishes no descendant sample, including zero")

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
// WithProcessIsolation is the ordinary default: session establishment succeeds
// as the current identity, no ProcessIsolation value is manufactured, the
// launch carries the sanitized ambient environment, and the agent reports
// shared identity with no descendant inventory and no whole-tree claim.
func TestAgentSessionDefaultsToOrdinaryExecution(t *testing.T) {
	t.Setenv("ACP_GO_CLAUDE_TEST_CANARY", "ambient-canary")

	var (
		launched  []claude.Options
		snapshots []int
	)

	agent, _, transport := newFakeLifecycleAgent(t, nil,
		WithScratchDir(t.TempDir()),
		WithRuntimeResourceHooks(RuntimeResourceHooks{
			ObserveProcessSnapshot: func(_ context.Context, _ RuntimeProcessKind, count int) {
				snapshots = append(snapshots, count)
			},
		}),
	)
	t.Cleanup(func() { require.NoError(t, agent.Close()) })

	agent.newClaudeClient = func(log *slog.Logger, options claude.Options) *claude.Client {
		launched = append(launched, options)

		return claude.NewClient(log, options, transport)
	}

	resp, err := agent.NewSession(context.Background(), NewSessionRequest(t.TempDir()))
	require.NoError(t, err, "NewSession without isolation")
	require.NotEmpty(t, resp.SessionId)

	require.Len(t, launched, 1)
	require.Nil(t, launched[0].ProcessIsolation,
		"omission must not manufacture a ProcessIsolation value")
	require.Equal(t, "ambient-canary", launched[0].OrdinaryEnvironment["ACP_GO_CLAUDE_TEST_CANARY"])

	// The one capture is cloned per launch, so a mutated launch environment
	// cannot reach the next one.
	launched[0].OrdinaryEnvironment["ACP_GO_CLAUDE_TEST_CANARY"] = "mutated"
	require.Equal(t, "ambient-canary", agent.ordinaryEnvironment()["ACP_GO_CLAUDE_TEST_CANARY"])

	require.Nil(t, agent.claudeIsolation())
	require.Equal(t, RuntimeContainmentSharedIdentity, agent.ContainmentMode())
	require.False(t, agent.ContainmentMode().provesWholeTreeLifecycle())
	require.False(t, agent.descendantProcesses.authoritative)
	require.Empty(t, snapshots, "ordinary execution publishes no provider-descendant sample")

	require.Nil(t, (&Agent{}).claudeIsolation())
	require.Nil(t, (&Agent{}).ordinaryEnvironment())
}

// TestExplicitProcessIsolationPreservesPolicy proves supplying
// WithProcessIsolation stays a strict selection: the policy reaches the launch
// verbatim with no ambient environment mixed in and no ordinary capture beside
// it, every platform that cannot apply it reports the boundary as unavailable
// rather than degrading to shared identity or best effort, and an invalid
// policy refuses session establishment before any spawn without retrying
// ordinary execution. That the unavailable verdict also stops the spawn is
// proven at the selector itself, in
// internal/claude.TestDarwinLaunchFailsClosedForExplicitProcessIsolation and
// the platform selector tests beside it.
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
	require.Equal(t, uint32(64251), isolation.UID)
	require.Equal(t, uint32(64252), isolation.GID)
	require.NotContains(t, isolation.BaseEnvironment, "ACP_GO_CLAUDE_TEST_CANARY")
	require.Equal(t, "/policy/bin", isolation.BaseEnvironment["PATH"])
	require.Nil(t, agent.ordinaryEnvironment(),
		"an explicit policy must not capture an ordinary ambient fallback")
	require.Equal(t, "/var/lib/acp-go-claude-tests", agent.options.Home,
		"standalone state root still names the managed home")

	// The structurally valid policy above is refused before any spawn on every
	// platform that cannot apply it, and the refusal never becomes shared
	// identity or best effort.
	originalGOOS := runtimeGOOS
	t.Cleanup(func() { runtimeGOOS = originalGOOS })

	for _, platform := range []string{"darwin", "windows", "freebsd", "openbsd"} {
		runtimeGOOS = platform
		require.Equal(t, RuntimeContainmentUnavailable, containmentMode(agent.options))
	}

	runtimeGOOS = originalGOOS

	invalid := NewAgent(WithProcessIsolation(ProcessIsolation{UID: 0, GID: 0}))
	t.Cleanup(func() { require.NoError(t, invalid.Close()) })
	require.NotNil(t, invalid.claudeIsolation())
	require.Nil(t, invalid.ordinaryEnvironment())
	transport := claude.NewProcessTransport(nil, claude.Options{
		CLIPath:             "/bin/true",
		Cwd:                 t.TempDir(),
		ProcessIsolation:    invalid.claudeIsolation(),
		OrdinaryEnvironment: map[string]string{"ACP_GO_CLAUDE_TEST_CANARY": "fallback"},
	})
	require.ErrorContains(t, transport.Start(t.Context()), "process isolation uid and gid must be nonzero")
}
