//go:build unix

package claude

import (
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// TestProcessIsolationOmissionAllowsOrdinaryUser executes a real ordinary
// launch. It proves the omitted policy runs the native command as the identity
// the adapter already holds — root or not — with its ambient supplementary
// groups, without applying a credential, arming a privileged supervisor
// handshake, or reaching any identity authority.
func TestProcessIsolationOmissionAllowsOrdinaryUser(t *testing.T) {
	output, childRead, err := os.Pipe()
	require.NoError(t, err)

	command := newProcessCommand("/bin/sh", "-c", "id -u; id -g")
	configureProcessCommand(command)

	command.Dir = t.TempDir()
	command.Stdout = childRead
	command.Env = BuildEnv(Options{
		OrdinaryEnvironment: OrdinaryEnvironment(),
		Cwd:                 command.Dir,
	})
	require.NotNil(t, command.Env)

	launch, err := prepareProcessTreeCommand(command, processLaunchOptions{})
	require.NoError(t, err)
	require.True(t, launch.ordinary)
	require.Nil(t, launch.startGate, "ordinary execution arms no supervisor start gate")
	require.Nil(t, launch.control, "ordinary execution arms no guardian control channel")
	require.Nil(t, launch.proof, "ordinary execution publishes no containment proof")
	require.Empty(t, launch.inherited, "ordinary execution hands the child no private descriptors")
	require.Nil(t, launch.cmd.SysProcAttr.Credential, "ordinary execution applies no credential")

	tree, err := processStartContained(launch)
	require.NoError(t, err)
	require.NoError(t, childRead.Close())

	count, exact := tree.processSnapshot()
	require.False(t, exact, "ordinary execution proves no descendant inventory")
	require.Zero(t, count)

	reported, err := io.ReadAll(output)
	require.NoError(t, err)
	require.NoError(t, output.Close())

	require.NoError(t, processWaitContained(tree, launch.cmd))
	require.NoError(t, processBoundaryComplete(tree, time.Second))
	require.NoError(t, processContainmentClose(tree))

	identity := strings.Fields(string(reported))
	require.Len(t, identity, 2)
	require.Equal(t, strconv.Itoa(os.Geteuid()), identity[0], "ordinary execution runs as the caller's uid")
	require.Equal(t, strconv.Itoa(os.Getegid()), identity[1], "ordinary execution runs as the caller's gid")
}

// TestOrdinaryLaunchReportsAStartFailure proves a native command that cannot
// start is reported as the launch failure it is, with no boundary handed back.
func TestOrdinaryLaunchReportsAStartFailure(t *testing.T) {
	launch, err := prepareProcessTreeCommand(
		newProcessCommand(filepath.Join(t.TempDir(), "missing")), processLaunchOptions{},
	)
	require.NoError(t, err)

	tree, err := processStartContained(launch)
	require.Error(t, err)
	require.Nil(t, tree)
}

// TestOrdinaryBoundaryHoldsItsReapUntilAnOwnerAppears proves the boundary does
// not begin exec.Cmd.Wait when it starts the child. os/exec closes the parent
// ends of the command's own pipes inside Wait, so a reap begun with the child
// would close the caller's stdout before the caller drained it — here, the
// child's whole transcript would be lost even though it exited zero.
func TestOrdinaryBoundaryHoldsItsReapUntilAnOwnerAppears(t *testing.T) {
	dir := t.TempDir()
	command := newProcessCommand("/bin/sh", "-c",
		`i=0; while [ "$i" -lt 200 ]; do echo "line-$i"; i=$((i + 1)); done; printf wrote > "$1"; exit 0`,
		"sh", filepath.Join(dir, "wrote"),
	)
	configureProcessCommand(command)

	command.Dir = dir
	command.Env = BuildEnv(Options{OrdinaryEnvironment: OrdinaryEnvironment(), Cwd: dir})

	stdout, err := command.StdoutPipe()
	require.NoError(t, err)

	launch, err := prepareProcessTreeCommand(command, processLaunchOptions{})
	require.NoError(t, err)

	tree, err := processStartContained(launch)
	require.NoError(t, err)

	require.Eventually(t, func() bool {
		_, statErr := os.Stat(filepath.Join(dir, "wrote"))

		return statErr == nil
	}, 10*time.Second, 5*time.Millisecond)

	// The child has finished and is exiting, and still nothing reaps it: the
	// single reap belongs to whoever owns the wait, and nobody does yet.
	require.Never(t, func() bool {
		select {
		case <-tree.ordinary.waiter.done:
			return true
		default:
			return false
		}
	}, 200*time.Millisecond, 10*time.Millisecond,
		"the reap must not run before an owner begins it")

	written, err := io.ReadAll(stdout)
	require.NoError(t, err, "the child's output pipe must outlive the child")
	require.Len(t, strings.Fields(string(written)), 200)

	require.NoError(t, processWaitContained(tree, launch.cmd))
	require.NoError(t, processBoundaryComplete(tree, time.Second))
	require.NoError(t, processContainmentClose(tree))
}

// TestOrdinaryBoundaryCompletesADirectlyOwnedChild proves the ordinary boundary
// stops a child that would otherwise outlive the adapter, reaps it exactly once,
// and leaves the caller's termination ladder in charge of shutdown.
func TestOrdinaryBoundaryCompletesADirectlyOwnedChild(t *testing.T) {
	command := newProcessCommand("/bin/sh", "-c", "sleep 60")
	configureProcessCommand(command)

	launch, err := prepareProcessTreeCommand(command, processLaunchOptions{})
	require.NoError(t, err)

	tree, err := processStartContained(launch)
	require.NoError(t, err)
	require.False(t, processContainmentOwnsShutdown(tree),
		"the ordinary boundary leaves shutdown to the caller's ladder")

	waiter := startChildExit(tree, launch.cmd)
	require.Same(t, tree.ordinary.observeExit(), waiter,
		"the boundary's own reap is the only one")

	require.NoError(t, processBoundaryComplete(tree, 5*time.Second))

	var exitErr *exec.ExitError
	require.ErrorAs(t, processWaitContained(tree, launch.cmd), &exitErr)
	require.NoError(t, processContainmentClose(tree))
}
