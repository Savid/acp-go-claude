//go:build unix

package claude

import (
	"os/exec"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProcessIsolationEnvironmentIsReplacementAndOverlay(t *testing.T) {
	t.Setenv("ACP_PROCESS_AMBIENT_CANARY", "must-not-leak")
	policy := &ProcessIsolation{UID: 123, GID: 456, BaseEnvironment: map[string]string{"PATH": "/usr/bin:/bin", "BASE": "yes", "OVERLAY": "base"}}
	env := BuildEnv(Options{ProcessIsolation: policy, Env: map[string]string{"OVERLAY": "option", "ONLY_OPTION": "yes"}})
	values := environmentMap(env)
	require.NotContains(t, values, "ACP_PROCESS_AMBIENT_CANARY")
	require.Equal(t, "yes", values["BASE"])
	require.Equal(t, "option", values["OVERLAY"])
	require.Equal(t, "yes", values["ONLY_OPTION"])
}

func TestProcessIsolationFailsClosedAndClearsGroups(t *testing.T) {
	require.Nil(t, BuildEnv(Options{}))
	require.Nil(t, BuildEnv(Options{ProcessIsolation: &ProcessIsolation{UID: 0, GID: 2, BaseEnvironment: map[string]string{}}}))
	require.Nil(t, BuildEnv(Options{ProcessIsolation: &ProcessIsolation{UID: 1, GID: 2, BaseEnvironment: map[string]string{"PATH": "relative"}}}))
	cmd := exec.Command("/usr/bin/true")
	policy := &ProcessIsolation{UID: 123, GID: 456, BaseEnvironment: map[string]string{}}
	require.NoError(t, applyProcessCredential(cmd, policy))
	require.Equal(t, uint32(123), cmd.SysProcAttr.Credential.Uid)
	require.Equal(t, uint32(456), cmd.SysProcAttr.Credential.Gid)
	require.Empty(t, cmd.SysProcAttr.Credential.Groups)
}
