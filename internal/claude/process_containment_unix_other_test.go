//go:build unix && !linux && !darwin

package claude

import (
	"os/exec"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestUnixOtherLaunchSelectorTakesOrdinaryOrRefuses proves the launch selector
// on a Unix platform that carries neither the Linux supervisor nor the Darwin
// best-effort boundary makes exactly two decisions and never falls back between
// them: an omitted policy selects ordinary execution, and any explicitly
// configured boundary is refused with nothing started and nothing handed back
// to start.
func TestUnixOtherLaunchSelectorTakesOrdinaryOrRefuses(t *testing.T) {
	ordinary := newProcessCommand("/bin/sh", "-c", "exit 0")
	configureProcessCommand(ordinary)

	launch, err := prepareProcessTreeCommand(ordinary, processLaunchOptions{})
	require.NoError(t, err)
	require.True(t, launch.ordinary, "an omitted policy selects ordinary execution")
	require.Same(t, ordinary, launch.cmd)
	require.Nil(t, ordinary.SysProcAttr.Credential, "ordinary execution applies no credential")
	require.Nil(t, ordinary.Process, "selecting a launch starts nothing")

	for name, options := range map[string]processLaunchOptions{
		"explicit isolation": {Isolation: &ProcessIsolation{UID: 64251, GID: 64252}},
		"darwin best effort": {DarwinBestEffort: true},
	} {
		t.Run(name, func(t *testing.T) {
			refused := newProcessCommand("/bin/sh", "-c", "exit 0")
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

// TestUnixOtherOrdinaryBoundaryRunsAndStopsADirectChild executes the ordinary
// boundary this platform actually selects, both for a child that exits on its
// own and for one that has to be stopped, and proves it publishes no descendant
// inventory and leaves shutdown to the caller's own termination ladder.
func TestUnixOtherOrdinaryBoundaryRunsAndStopsADirectChild(t *testing.T) {
	directory := t.TempDir()

	command := newProcessCommand("/bin/sh", "-c", "exit 0")
	configureProcessCommand(command)

	command.Dir = directory
	command.Env = BuildEnv(Options{OrdinaryEnvironment: OrdinaryEnvironment(), Cwd: directory})
	require.NotNil(t, command.Env)

	launch, err := prepareProcessTreeCommand(command, processLaunchOptions{})
	require.NoError(t, err)

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

	// A child that would outlive the adapter is still stopped and reaped by the
	// boundary's own completion ladder.
	stubborn := newProcessCommand("/bin/sh", "-c", "sleep 60")
	configureProcessCommand(stubborn)

	stubbornLaunch, err := prepareProcessTreeCommand(stubborn, processLaunchOptions{})
	require.NoError(t, err)

	stubbornTree, err := processStartContained(stubbornLaunch)
	require.NoError(t, err)
	require.NoError(t, processBoundaryComplete(stubbornTree, 5*time.Second))

	var exitErr *exec.ExitError
	require.ErrorAs(t, processWaitContained(stubbornTree, stubbornLaunch.cmd), &exitErr)
	require.NoError(t, processContainmentClose(stubbornTree))
}

// TestUnixOtherContainmentAnswersAnAbsentBoundary proves the boundary reports
// its own absence instead of faulting, the way every other platform's does.
func TestUnixOtherContainmentAnswersAnAbsentBoundary(t *testing.T) {
	require.ErrorIs(t, (*processContainment)(nil).complete(time.Second), ErrProcessContainmentIncomplete)
	require.ErrorIs(t, (*processContainment)(nil).wait(nil), ErrProcessContainmentIncomplete)
	require.ErrorContains(t, (&processContainment{}).complete(time.Second), "boundary is unavailable")
	require.ErrorContains(t, (&processContainment{}).wait(nil), "boundary is unavailable")
}
