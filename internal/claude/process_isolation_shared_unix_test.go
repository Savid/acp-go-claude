//go:build unix

package claude

import (
	"os/exec"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

// restoreSharedProcessIdentitySeams points the identity sources back at the
// running process when the test ends, so a case that describes an unprivileged
// deployment cannot leak that description into the next one.
func restoreSharedProcessIdentitySeams(t *testing.T) {
	t.Helper()

	goos, uid, gid := processIsolationGOOS, processEffectiveUID, processEffectiveGID
	t.Cleanup(func() {
		processIsolationGOOS = goos
		processEffectiveUID = uid
		processEffectiveGID = gid
	})
}

// TestSharedProcessIdentityNamesOnlyTheSupervisorsOwnLinuxIdentity proves what
// selects the arm that keeps privilege-free containment. It is uid equality and
// nothing else: no flag crosses the boundary, root can never match because the
// native uid must be nonzero, and Darwin states its own boundary so the shape
// is not recognised there.
func TestSharedProcessIdentityNamesOnlyTheSupervisorsOwnLinuxIdentity(t *testing.T) {
	restoreSharedProcessIdentitySeams(t)
	processIsolationGOOS = processIsolationLinux
	processEffectiveUID = func() int { return 1000 }

	require.False(t, sharedProcessIdentity(nil))
	require.True(t, sharedProcessIdentity(&ProcessIsolation{UID: 1000}))
	require.False(t, sharedProcessIdentity(&ProcessIsolation{UID: 1001}))

	processEffectiveUID = func() int { return 0 }
	require.False(t, sharedProcessIdentity(&ProcessIsolation{UID: 0}))
	require.False(t, sharedProcessIdentity(&ProcessIsolation{UID: 1000}))

	processEffectiveUID = func() int { return 1000 }
	processIsolationGOOS = "darwin"
	require.False(t, sharedProcessIdentity(&ProcessIsolation{UID: 1000}))
}

// TestSharedIdentityIsolationCarriesNoStandaloneOwnerFields proves the shape a
// supervisor running as the agent identity is allowed to describe. The durable
// standalone record proves an identity no live task holds, and the supervisor
// asking for it is such a task, so the record can never be written: the only
// coherent request is one that asks for no record at all. An identity the
// supervisor does not hold still has to name one.
func TestSharedIdentityIsolationCarriesNoStandaloneOwnerFields(t *testing.T) {
	restoreSharedProcessIdentitySeams(t)
	processIsolationGOOS = processIsolationLinux
	processEffectiveUID = func() int { return 1000 }

	shared := &ProcessIsolation{UID: 1000, GID: 1000, BaseEnvironment: map[string]string{}}
	require.NoError(t, validateProcessIsolation(shared))

	withOwner := *shared
	withOwner.StandaloneOwnerID = "claude-shared"

	err := validateProcessIsolation(&withOwner)
	require.ErrorContains(t, err, "standalone owner fields describe an identity the supervisor already holds")
	require.ErrorContains(t, err, "run the supervisor as root to isolate the agent identity")

	withStateRoot := *shared
	withStateRoot.StandaloneStateRoot = "/srv/claude"
	require.ErrorContains(
		t,
		validateProcessIsolation(&withStateRoot),
		"standalone owner fields describe an identity the supervisor already holds",
	)

	processEffectiveUID = func() int { return 0 }
	require.ErrorContains(
		t,
		validateProcessIsolation(shared),
		"standalone owner id must be 1..256 canonical ASCII bytes",
	)
}

// TestSharedIdentityCredentialRequestsNoIdentityChange proves the launch asks
// the kernel for nothing it cannot have. An unprivileged supervisor can neither
// re-enter its own identity nor shed the supplementary groups of the account it
// was started under, so the honest instruction is no credential at all. A group
// it could not enter is still refused, and the isolated arm keeps dropping to
// the native credential with every supplementary group removed.
func TestSharedIdentityCredentialRequestsNoIdentityChange(t *testing.T) {
	restoreSharedProcessIdentitySeams(t)
	processIsolationGOOS = processIsolationLinux
	processEffectiveUID = func() int { return 1000 }
	processEffectiveGID = func() int { return 1000 }

	shared := &ProcessIsolation{UID: 1000, GID: 1000, BaseEnvironment: map[string]string{}}

	command := exec.Command("/usr/bin/true")
	require.NoError(t, applyProcessCredential(command, shared))
	require.Nil(t, command.SysProcAttr.Credential)

	for _, group := range []int{1001, -1} {
		processEffectiveGID = func() int { return group }
		require.ErrorContains(
			t,
			applyProcessCredential(exec.Command("/usr/bin/true"), shared),
			"native group 1000 cannot be entered from group",
		)
	}

	processEffectiveUID = func() int { return 0 }
	processEffectiveGID = func() int { return 0 }

	isolated := &ProcessIsolation{
		UID: 1000, GID: 1000, BaseEnvironment: map[string]string{},
		StandaloneOwnerID: "claude-isolated", StandaloneStateRoot: "/srv/claude",
	}

	command = exec.Command("/usr/bin/true")
	require.NoError(t, applyProcessCredential(command, isolated))
	require.Equal(
		t,
		&syscall.Credential{Uid: 1000, Gid: 1000, Groups: []uint32{}, NoSetGroups: false},
		command.SysProcAttr.Credential,
	)
}
