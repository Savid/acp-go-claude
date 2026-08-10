//go:build unix

package claude

import (
	"os/exec"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProcessIsolationOmissionAllowsOrdinaryUser(t *testing.T) {
	originalEnviron := implicitIsolationEnviron
	originalUID, originalGID := implicitIsolationUID, implicitIsolationGID
	t.Cleanup(func() {
		implicitIsolationEnviron = originalEnviron
		implicitIsolationUID, implicitIsolationGID = originalUID, originalGID
	})

	implicitIsolationEnviron = func() []string {
		return []string{
			"PATH=/usr/bin",
			"ANTHROPIC_API_KEY=ambient-key",
			"GOTRACEBACK=crash",
			"CLAUDE_CODE_CUSTOM_OAUTH_URL=https://example.invalid",
			envClaudeCodeNested + "=1",
			privateAdapterEnvPrefix + "MODE=guardian",
			"=empty-key",
			"malformed-entry",
		}
	}

	for _, identity := range []struct {
		name     string
		uid, gid int
	}{
		{name: "non-root", uid: 1000, gid: 1000},
		{name: "root", uid: 0, gid: 0},
	} {
		t.Run(identity.name, func(t *testing.T) {
			implicitIsolationUID = func() int { return identity.uid }
			implicitIsolationGID = func() int { return identity.gid }

			isolation := ImplicitProcessIsolation()
			require.True(t, isolation.Implicit)
			require.Equal(t, uint32(identity.uid), isolation.UID)
			require.Equal(t, uint32(identity.gid), isolation.GID)
			require.Equal(t, map[string]string{
				"PATH":              "/usr/bin",
				"ANTHROPIC_API_KEY": "ambient-key",
			}, isolation.BaseEnvironment)
		})
	}

	require.Equal(t, ^uint32(0), implicitIdentityValue(-1))
}

func TestProcessIsolationOmissionAllowsRoot(t *testing.T) {
	originalGOOS := processIsolationGOOS
	originalUID, originalGID := processEffectiveUID, processEffectiveGID
	t.Cleanup(func() {
		processIsolationGOOS = originalGOOS
		processEffectiveUID, processEffectiveGID = originalUID, originalGID
	})

	processIsolationGOOS = processIsolationLinux

	for _, identity := range []struct {
		name     string
		uid, gid int
	}{
		{name: "non-root", uid: 1000, gid: 1000},
		{name: "root", uid: 0, gid: 0},
	} {
		t.Run(identity.name, func(t *testing.T) {
			processEffectiveUID = func() int { return identity.uid }
			processEffectiveGID = func() int { return identity.gid }

			implicit := &ProcessIsolation{
				UID: uint32(identity.uid), GID: uint32(identity.gid),
				BaseEnvironment: map[string]string{}, Implicit: true,
			}
			require.NoError(t, validateProcessIsolation(implicit))
			require.True(t, sharedProcessIdentity(implicit))

			diverged := &ProcessIsolation{
				UID: implicit.UID + 1, GID: implicit.GID,
				BaseEnvironment: map[string]string{}, Implicit: true,
			}
			require.ErrorContains(t, validateProcessIsolation(diverged), "process runs as")
		})
	}

	authority := &ProcessIsolation{
		UID: 1000, GID: 1000, BaseEnvironment: map[string]string{},
		Implicit: true, StandaloneOwnerID: "owner",
	}
	require.ErrorContains(t, validateProcessIsolation(authority), "forbids identity capabilities")
}

// TestApplyProcessCredentialImplicitAppliesNoCredential proves the implicit
// launch never attaches a credential change: the command it prepared stays
// exactly the command it was handed.
func TestApplyProcessCredentialImplicitAppliesNoCredential(t *testing.T) {
	implicit := &ProcessIsolation{
		UID:             uint32(processEffectiveUID()),
		GID:             uint32(processEffectiveGID()),
		BaseEnvironment: map[string]string{},
		Implicit:        true,
	}

	cmd := exec.Command("/usr/bin/true")
	require.NoError(t, applyProcessCredential(cmd, implicit))
	require.Nil(t, cmd.SysProcAttr.Credential)
}

// TestSupervisorEnvironmentCarriesTheImplicitMarker proves the private
// supervisor handshake states whether the launch is the implicit
// current-identity one, and that the inherited verification then proves the
// identity without demanding the empty supplementary groups only an explicit
// credential change can produce.
func TestSupervisorEnvironmentCarriesTheImplicitMarker(t *testing.T) {
	implicit := ProcessIsolation{
		UID:             uint32(processEffectiveUID()),
		GID:             uint32(processEffectiveGID()),
		BaseEnvironment: map[string]string{},
		Implicit:        true,
	}

	env := supervisorIdentityEnvironment([]string{"KEEP=yes"}, "MODE", "run", implicit)
	values := environmentMap(env)
	require.Equal(t, "yes", values["KEEP"])
	require.Equal(t, "true", values[processIsolationImplicitEnv])

	t.Setenv(processIsolationUIDEnv, values[processIsolationUIDEnv])
	t.Setenv(processIsolationGIDEnv, values[processIsolationGIDEnv])
	t.Setenv(processIsolationImplicitEnv, "true")

	require.NoError(t, verifySupervisorIdentity())

	t.Setenv(processIsolationUIDEnv, "4294967294")
	require.ErrorContains(t, verifySupervisorIdentity(), "mismatch")

	// The zero id an implicit root launch carries is invalid the moment the
	// marker is absent: an explicit supervisor identity must be nonzero.
	t.Setenv(processIsolationUIDEnv, "0")
	t.Setenv(processIsolationGIDEnv, "0")
	t.Setenv(processIsolationImplicitEnv, "false")
	uid, gid, expectedImplicit, err := expectedSupervisorIdentity()
	require.ErrorContains(t, err, "uid")
	require.Zero(t, uid)
	require.Zero(t, gid)
	require.False(t, expectedImplicit)
}
