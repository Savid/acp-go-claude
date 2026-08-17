package claude

import (
	"os/exec"
	"testing"

	"github.com/stretchr/testify/require"
)

// A native child outlives the call that spawned it on every platform. Both
// halves of that are asserted here rather than per platform, because the one
// platform a split like this is exercised on is the one where the split hides.
func TestProcessCommandCarriesNoCancellationPath(t *testing.T) {
	t.Parallel()

	command := newProcessCommand("true", "--version")

	require.Equal(t, []string{"true", "--version"}, command.Args)
	require.Nil(t, command.Cancel)

	configureProcessCommand(command)

	require.Nil(t, command.Cancel)
	require.Equal(t, processShutdownWaitDelay, command.WaitDelay)
	require.NotPanics(t, func() { configureProcessCommandPlatform(new(exec.Cmd)) })
}
