package claude

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestOrdinaryEnvironmentIsTheSanitizedAmbientCapture proves omission carries
// the operator's own environment minus exactly the values no child may inherit,
// and that a configured ambient credential still reaches the child so a host
// that scopes its worker environment keeps deciding what the harness sees.
func TestOrdinaryEnvironmentIsTheSanitizedAmbientCapture(t *testing.T) {
	original := ordinaryEnviron
	t.Cleanup(func() { ordinaryEnviron = original })

	ordinaryEnviron = func() []string {
		return []string{
			"PATH=/usr/bin",
			"ANTHROPIC_API_KEY=ambient-key",
			"GOTRACEBACK=crash",
			"CLAUDE_CODE_CUSTOM_OAUTH_URL=https://example.invalid",
			"TERM_PROGRAM=iTerm",
			envClaudeCodeNested + "=1",
			privateAdapterEnvPrefix + "MODE=guardian",
			"NUL_VALUE=bad\x00value",
			"=empty-key",
			"malformed-entry",
		}
	}

	require.Equal(t, map[string]string{
		"PATH":              "/usr/bin",
		"ANTHROPIC_API_KEY": "ambient-key",
	}, OrdinaryEnvironment())
}

// TestOrdinaryLaunchEnvironmentIsNotHeldToPolicyPathRules proves ordinary
// execution keeps the operator's own search path, including relative entries a
// hardened replacement policy would be refused for.
func TestOrdinaryLaunchEnvironmentIsNotHeldToPolicyPathRules(t *testing.T) {
	ordinary := Options{OrdinaryEnvironment: map[string]string{envSearchPath: "relative:/usr/bin"}}
	require.Equal(t, []string{
		envSearchPath + "=relative:/usr/bin",
		"CLAUDE_CODE_ENTRYPOINT=acp-go-claude",
	}, BuildEnv(ordinary))

	hardened := Options{ProcessIsolation: &ProcessIsolation{
		UID: 64251, GID: 64252,
		BaseEnvironment:     map[string]string{envSearchPath: "relative:/usr/bin"},
		StandaloneOwnerID:   "acp-go-claude-tests",
		StandaloneStateRoot: "/var/lib/acp-go-claude-tests",
	}}
	require.Nil(t, BuildEnv(hardened))
}

func TestResolveOrdinaryExecutableUsesAmbientRules(t *testing.T) {
	original := ordinaryLookPath
	t.Cleanup(func() { ordinaryLookPath = original })

	_, err := resolveOrdinaryExecutable("  ", nil)
	require.ErrorContains(t, err, "empty")

	ordinaryLookPath = func(name string, _ []string) (string, error) {
		if filepath.Base(name) != "claude" {
			return "", exec.ErrNotFound
		}

		return "/resolved/" + name, nil
	}

	resolved, err := resolveOrdinaryExecutable("dir/claude", nil)
	require.NoError(t, err)
	require.Equal(t, "/resolved/dir/claude", resolved)

	_, err = resolveOrdinaryExecutable("dir/missing", nil)
	require.ErrorContains(t, err, "resolve executable")

	// A relative PATH entry is ordinary here; an empty one is skipped.
	resolved, err = resolveOrdinaryExecutable("claude", []string{
		envSearchPath + "=" + string(os.PathListSeparator) + "relative",
	})
	require.NoError(t, err)
	require.Equal(t, "/resolved/"+filepath.Join("relative", "claude"), resolved)

	_, err = resolveOrdinaryExecutable("missing", []string{envSearchPath + "=/usr/bin"})
	require.ErrorIs(t, err, exec.ErrNotFound)

	_, err = resolveLaunchExecutable(Options{}, "missing", nil)
	require.ErrorIs(t, err, exec.ErrNotFound)
	_, err = resolveLaunchExecutable(
		Options{ProcessIsolation: &ProcessIsolation{}}, "relative/tool", nil,
	)
	require.ErrorContains(t, err, "not absolute")

	// Discovery answers the base-environment verdict before it resolves
	// anything, so an invalid explicit policy never reaches a search path.
	_, err = Discover(t.Context(), "claude", Options{ProcessIsolation: &ProcessIsolation{}})
	require.ErrorContains(t, err, "must be nonzero")
	require.ErrorContains(t, validateProcessIsolation(nil), "process isolation is required")
}

func TestOrdinaryLaunchPreparationRequiresACommand(t *testing.T) {
	_, err := prepareOrdinaryLaunch(nil, processLaunchOptions{})
	require.ErrorContains(t, err, "command is unavailable")

	launch, err := prepareOrdinaryLaunch(exec.Command("true"), processLaunchOptions{})
	require.NoError(t, err)
	require.True(t, launch.ordinary)

	_, err = startOrdinaryBoundary(nil)
	require.ErrorContains(t, err, "launch is unavailable")

	failing := &processTreeCommand{cmd: exec.Command(filepath.Join(t.TempDir(), "missing")), ordinary: true}
	_, err = startOrdinaryBoundary(failing)
	require.Error(t, err)

	require.ErrorContains(t, (*ordinaryBoundary)(nil).complete(time.Second), "boundary is unavailable")
	require.ErrorContains(t, (*ordinaryBoundary)(nil).wait(), "boundary is unavailable")
	require.Nil(t, (*ordinaryBoundary)(nil).observeExit())
}

// TestOrdinaryBoundaryEscalatesAndReportsIncompleteness proves the directly
// owned boundary asks the process group to stop, escalates to a kill, and
// reports an unreaped child as an incomplete boundary rather than success.
func TestOrdinaryBoundaryEscalatesAndReportsIncompleteness(t *testing.T) {
	originalTerminate, originalKill := processTerminate, processKill
	t.Cleanup(func() { processTerminate, processKill = originalTerminate, originalKill })

	want := errors.New("signal refused")

	processTerminate = func(*exec.Cmd) (bool, error) { return false, want }
	require.ErrorContains(t, newStalledOrdinaryBoundary().complete(time.Millisecond), want.Error())

	processTerminate = func(*exec.Cmd) (bool, error) { return true, nil }
	processKill = func(*exec.Cmd) (bool, error) { return false, want }
	require.ErrorContains(t, newStalledOrdinaryBoundary().complete(time.Millisecond), want.Error())

	processKill = func(*exec.Cmd) (bool, error) { return true, nil }

	stalled := newStalledOrdinaryBoundary()
	err := stalled.complete(0)
	require.ErrorIs(t, err, ErrProcessContainmentIncomplete)
	require.ErrorContains(t, err, "not reaped")
	require.Equal(t, err, stalled.complete(time.Second), "completion is memoized")

	reaped := newStalledOrdinaryBoundary()
	close(reaped.waiter.done)
	require.NoError(t, reaped.complete(time.Second))
	require.NoError(t, reaped.wait())
	require.NotNil(t, reaped.observeExit())
}

// newStalledOrdinaryBoundary builds a boundary whose direct child never reports
// its exit, so escalation and the incomplete verdict are both reachable without
// depending on a real process's timing.
func newStalledOrdinaryBoundary() *ordinaryBoundary {
	return &ordinaryBoundary{waiter: &commandWait{done: make(chan struct{})}}
}
