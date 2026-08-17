//go:build !unix

package claude

import (
	"os"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestNonUnixLaunchSelectorTakesOrdinaryOrRefuses proves the launch selector on
// a platform with no Unix process-boundary primitives makes exactly two
// decisions and never falls back between them: an omitted policy selects
// ordinary execution, and any explicitly configured boundary is refused with
// nothing started and nothing handed back to start.
func TestNonUnixLaunchSelectorTakesOrdinaryOrRefuses(t *testing.T) {
	ordinary := newProcessCommand("unused-executable")
	configureProcessCommand(ordinary)

	launch, err := prepareProcessTreeCommand(ordinary, processLaunchOptions{})
	require.NoError(t, err)
	require.True(t, launch.ordinary, "an omitted policy selects ordinary execution")
	require.Same(t, ordinary, launch.cmd)
	require.Nil(t, ordinary.Process, "selecting a launch starts nothing")

	for name, options := range map[string]processLaunchOptions{
		"explicit isolation": {Isolation: &ProcessIsolation{UID: 64251, GID: 64252}},
		"darwin best effort": {DarwinBestEffort: true},
	} {
		t.Run(name, func(t *testing.T) {
			refused := newProcessCommand("unused-executable")
			configureProcessCommand(refused)

			rejected, refusalErr := prepareProcessTreeCommand(refused, options)
			require.Nil(t, rejected, "a refused boundary hands back nothing to start")
			require.ErrorIs(t, refusalErr, ErrProcessContainmentIncomplete)
			require.Contains(t, refusalErr.Error(), runtime.GOOS,
				"the refusal names the platform that cannot apply the boundary")
			require.Nil(t, refused.Process, "a refused boundary starts nothing")
		})
	}
}

// TestNonUnixOrdinaryBoundaryRunsAndCompletesADirectChild executes the ordinary
// boundary this platform actually selects: a real child starts, is waited on
// exactly once, publishes no descendant inventory, and leaves shutdown to the
// caller's own termination ladder.
func TestNonUnixOrdinaryBoundaryRunsAndCompletesADirectChild(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("fixture needs the Windows command interpreter")
	}

	interpreter := os.Getenv("COMSPEC")
	if interpreter == "" {
		interpreter = "cmd.exe"
	}

	command := newProcessCommand(interpreter, "/c", "exit 0")
	configureProcessCommand(command)

	command.Dir = t.TempDir()
	command.Env = BuildEnv(Options{OrdinaryEnvironment: OrdinaryEnvironment(), Cwd: command.Dir})
	require.NotNil(t, command.Env)

	launch, err := prepareProcessTreeCommand(command, processLaunchOptions{})
	require.NoError(t, err)
	require.True(t, launch.ordinary)

	tree, err := processStartContained(launch)
	require.NoError(t, err)
	require.False(t, processContainmentOwnsShutdown(tree),
		"the ordinary boundary leaves shutdown to the caller's ladder")

	count, exact := tree.processSnapshot()
	require.False(t, exact, "ordinary execution proves no descendant inventory")
	require.Zero(t, count)

	require.NoError(t, processWaitContained(tree, launch.cmd))
	require.NoError(t, processBoundaryComplete(tree, time.Second))
	require.NoError(t, processContainmentClose(tree))
	require.NotNil(t, command.ProcessState, "the direct child is reaped exactly once")
}

// TestNonUnixContainmentAnswersAnAbsentBoundary proves the boundary reports its
// own absence instead of faulting, the way every other platform's does.
func TestNonUnixContainmentAnswersAnAbsentBoundary(t *testing.T) {
	require.ErrorIs(t, (*processContainment)(nil).complete(time.Second), ErrProcessContainmentIncomplete)
	require.ErrorIs(t, (*processContainment)(nil).wait(nil), ErrProcessContainmentIncomplete)
	require.ErrorContains(t, (&processContainment{}).complete(time.Second), "boundary is unavailable")
	require.ErrorContains(t, (&processContainment{}).wait(nil), "boundary is unavailable")
}
