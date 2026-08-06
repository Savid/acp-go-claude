//go:build linux

package claude

import (
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// containmentCovProcRoot points the group-membership scan at a synthetic
// /proc so a test can decide exactly which processes the containment sees.
func containmentCovProcRoot(t *testing.T, entries map[int]string) string {
	t.Helper()

	root := t.TempDir()
	for pid, stat := range entries {
		directory := filepath.Join(root, strconv.Itoa(pid))
		require.NoError(t, os.MkdirAll(directory, 0o700))
		require.NoError(t, os.WriteFile(filepath.Join(directory, "stat"), []byte(stat), 0o600))
	}

	previous := turnSupervisorProcRoot
	turnSupervisorProcRoot = root

	t.Cleanup(func() { turnSupervisorProcRoot = previous })

	return root
}

// containmentCovStat renders a /proc/<pid>/stat line with the state and
// process group the scan reads. Fields 4..20 only have to be present and
// parseable; the scan reads state, parent, group and start time.
func containmentCovStat(pid int, state byte, groupID int) string {
	line := "(claude) " + string(state) + " 1 " + strconv.Itoa(groupID)
	for range 17 {
		line += " 0"
	}

	return strconv.Itoa(pid) + " " + line + "\n"
}

func containmentCovFakeKill(t *testing.T, result error) {
	t.Helper()

	previous := syscallKill
	syscallKill = func(int, syscall.Signal) error { return result }

	t.Cleanup(func() { syscallKill = previous })
}

// TestRunningProcessGroupMembersCountsOnlyLiveGroupMembers proves the
// quiescence scan does not confuse a reaped tree with a running one. A member
// that exited lingers as a zombie until its parent reaps it and kill(2) keeps
// addressing the group while one is present, so the scan must exclude zombies
// and must exclude members of other groups.
func TestRunningProcessGroupMembersCountsOnlyLiveGroupMembers(t *testing.T) {
	const group = 4242

	containmentCovProcRoot(t, map[int]string{
		11: containmentCovStat(11, 'S', group),
		12: containmentCovStat(12, 'Z', group),
		13: containmentCovStat(13, 'R', group),
		14: containmentCovStat(14, 'S', group+1),
	})

	running, err := runningProcessGroupMembers(group)
	require.NoError(t, err)
	require.Equal(t, 2, running)
}

// TestRunningProcessGroupMembersSurfacesAnUnreadableProcess proves the scan
// refuses to answer when it cannot read a process it can see. Treating an
// unreadable entry as "not running" would let quiescence be declared over a
// tree that is still alive, and the caller reaps the supervisor immediately
// after quiescence returns.
func TestRunningProcessGroupMembersSurfacesAnUnreadableProcess(t *testing.T) {
	const group = 4242

	root := containmentCovProcRoot(t, map[int]string{
		11: containmentCovStat(11, 'S', group),
		12: "truncated",
	})
	require.DirExists(t, filepath.Join(root, "12"))

	running, err := runningProcessGroupMembers(group)
	require.Error(t, err)
	require.Zero(t, running)
}

// TestRunningProcessGroupMembersSurfacesAnUnreadableProcRoot proves the same
// for the enumeration itself: a proc root that cannot be listed is an error,
// not an empty tree.
func TestRunningProcessGroupMembersSurfacesAnUnreadableProcRoot(t *testing.T) {
	root := containmentCovProcRoot(t, nil)
	turnSupervisorProcRoot = filepath.Join(root, "missing")

	running, err := runningProcessGroupMembers(4242)
	require.ErrorIs(t, err, os.ErrNotExist)
	require.Zero(t, running)
}

// TestWaitUntilEmptyRefusesToDeclareQuiescenceItCannotProve proves the wait
// loop propagates a failed membership scan rather than returning as if the
// group had drained. The caller reaps the supervisor on a nil return, so a
// swallowed scan error would reap a live tree.
func TestWaitUntilEmptyRefusesToDeclareQuiescenceItCannotProve(t *testing.T) {
	const group = 4242

	root := containmentCovProcRoot(t, nil)
	turnSupervisorProcRoot = filepath.Join(root, "missing")

	// EPERM means the group still exists but this process may not signal it,
	// which is exactly the case that has to fall through to the scan.
	containmentCovFakeKill(t, syscall.EPERM)

	containment := &processContainment{processGroupID: group}

	err := containment.waitUntilEmpty(50 * time.Millisecond)
	require.ErrorIs(t, err, os.ErrNotExist)
	require.ErrorContains(t, err, "probe Claude process supervisor 4242")
}

// TestWaitUntilEmptyReturnsWhenTheGroupHasDrained proves the same loop does
// return once the scan reports no live member, so the refusal above is a
// refusal and not the only outcome the loop can produce.
func TestWaitUntilEmptyReturnsWhenTheGroupHasDrained(t *testing.T) {
	const group = 4242

	containmentCovProcRoot(t, map[int]string{
		11: containmentCovStat(11, 'Z', group),
		12: containmentCovStat(12, 'S', group+1),
	})
	containmentCovFakeKill(t, syscall.EPERM)

	containment := &processContainment{processGroupID: group}
	require.NoError(t, containment.waitUntilEmpty(50*time.Millisecond))
}
