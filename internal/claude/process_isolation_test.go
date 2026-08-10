//go:build unix

package claude

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

type testProcessIdentityCapability struct{}

func (testProcessIdentityCapability) Duplicate() (*os.File, error) {
	return nil, errors.New("test capability cannot duplicate")
}

func TestProcessIdentityDispositionValidation(t *testing.T) {
	capability := testProcessIdentityCapability{}
	validStandalone := ProcessIsolation{StandaloneOwnerID: "deployment-1", StandaloneStateRoot: "/var/lib/acp-go-claude"}
	validBorrowed := ProcessIsolation{IdentityLock: capability, AuthorityDomain: capability}

	for name, isolation := range map[string]ProcessIsolation{
		"standalone":             validStandalone,
		"borrowed":               validBorrowed,
		"mixed capabilities":     {IdentityLock: capability},
		"borrowed owner":         {IdentityLock: capability, AuthorityDomain: capability, StandaloneOwnerID: "deployment-1"},
		"missing owner":          {StandaloneStateRoot: "/var/lib/acp-go-claude"},
		"invalid owner prefix":   {StandaloneOwnerID: "-deployment", StandaloneStateRoot: "/var/lib/acp-go-claude"},
		"invalid owner byte":     {StandaloneOwnerID: "deployment 1", StandaloneStateRoot: "/var/lib/acp-go-claude"},
		"long owner":             {StandaloneOwnerID: "a" + strings.Repeat("b", 256), StandaloneStateRoot: "/var/lib/acp-go-claude"},
		"relative root":          {StandaloneOwnerID: "deployment-1", StandaloneStateRoot: "relative"},
		"filesystem root":        {StandaloneOwnerID: "deployment-1", StandaloneStateRoot: "/"},
		"authority root":         {StandaloneOwnerID: "deployment-1", StandaloneStateRoot: "/var/lib/acp-go/agent-identities"},
		"beneath authority root": {StandaloneOwnerID: "deployment-1", StandaloneStateRoot: "/var/lib/acp-go/agent-identities/provider"},
		"control in root":        {StandaloneOwnerID: "deployment-1", StandaloneStateRoot: "/var/lib/provider\u0085"},
		"invalid utf8 in root":   {StandaloneOwnerID: "deployment-1", StandaloneStateRoot: string([]byte{'/', 0xff})},
	} {
		t.Run(name, func(t *testing.T) {
			err := validateStandaloneIdentityDisposition(&isolation)
			if name == "standalone" || name == "borrowed" {
				require.NoError(t, err)
			} else {
				require.Error(t, err)
			}
		})
	}
}

func TestProcessIsolationEnvironmentIsReplacementAndOverlay(t *testing.T) {
	t.Setenv("ACP_PROCESS_AMBIENT_CANARY", "must-not-leak")
	policy := &ProcessIsolation{UID: 123, GID: 456, BaseEnvironment: map[string]string{"PATH": "/usr/bin:/bin", "BASE": "yes", "OVERLAY": "base"}, StandaloneOwnerID: "test-owner", StandaloneStateRoot: "/var/lib/acp-go-test"}
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
	require.Nil(t, BuildEnv(Options{ProcessIsolation: &ProcessIsolation{UID: 1, GID: 2, BaseEnvironment: map[string]string{"PATH": "relative"}, StandaloneOwnerID: "test-owner", StandaloneStateRoot: "/var/lib/acp-go-test"}}))
	cmd := exec.Command("/usr/bin/true")
	policy := &ProcessIsolation{UID: 123, GID: 456, BaseEnvironment: map[string]string{}, StandaloneOwnerID: "test-owner", StandaloneStateRoot: "/var/lib/acp-go-test"}
	require.NoError(t, applyProcessCredential(cmd, policy))
	require.Equal(t, uint32(123), cmd.SysProcAttr.Credential.Uid)
	require.Equal(t, uint32(456), cmd.SysProcAttr.Credential.Gid)
	require.Empty(t, cmd.SysProcAttr.Credential.Groups)
}

func TestProcessIsolationValidationAndExecutableResolutionBranches(t *testing.T) {
	valid := &ProcessIsolation{UID: 123, GID: 456, BaseEnvironment: map[string]string{}, StandaloneOwnerID: "test-owner", StandaloneStateRoot: "/var/lib/acp-go-test"}
	require.ErrorContains(t, validateProcessIsolation(&ProcessIsolation{UID: 1, GID: 2}), "base environment")
	require.Error(t, validateProcessIsolation(&ProcessIsolation{UID: 1, GID: 2, BaseEnvironment: map[string]string{"BAD=KEY": "x"}}))
	require.Error(t, validateEnvironmentMap(map[string]string{"OK": "bad\x00value"}))
	require.NoError(t, validateProcessIsolation(valid))
	require.NoError(t, validateProcessSearchPath(""))

	_, err := resolveProcessExecutable(" ", nil)
	require.ErrorContains(t, err, "empty")
	_, err = resolveProcessExecutable("relative/tool", nil)
	require.ErrorContains(t, err, "not absolute")
	missing := filepath.Join(t.TempDir(), "missing")
	_, err = resolveProcessExecutable(missing, nil)
	require.ErrorContains(t, err, "stat executable")
	_, err = resolveProcessExecutable(t.TempDir(), nil)
	require.ErrorContains(t, err, "not executable")
	nonExecutable := filepath.Join(t.TempDir(), "tool")
	require.NoError(t, os.WriteFile(nonExecutable, []byte("tool"), 0o600))
	_, err = resolveProcessExecutable(nonExecutable, nil)
	require.ErrorContains(t, err, "not executable")
	executable := filepath.Join(t.TempDir(), "tool")
	require.NoError(t, os.WriteFile(executable, []byte("#!/bin/sh\n"), 0o700))
	resolved, err := resolveProcessExecutable(executable, nil)
	require.NoError(t, err)
	require.Equal(t, executable, resolved)
	_, err = resolveProcessExecutable("tool", []string{"HOME=/tmp"})
	require.ErrorContains(t, err, "PATH is empty")
	_, err = resolveProcessExecutable("tool", []string{"PATH=relative"})
	require.ErrorContains(t, err, "non-absolute")
	blocked := filepath.Join(t.TempDir(), "blocked")
	require.NoError(t, os.WriteFile(blocked, []byte("x"), 0o600))
	_, err = resolveProcessExecutable("child", []string{"PATH=" + blocked})
	require.Error(t, err)
	_, err = resolveProcessExecutable("missing", []string{"PATH=" + t.TempDir()})
	require.ErrorIs(t, err, exec.ErrNotFound)
}

func TestProcessIsolationValidatesLinuxIdentityDisposition(t *testing.T) {
	original := processIsolationGOOS
	processIsolationGOOS = "linux"
	t.Cleanup(func() { processIsolationGOOS = original })
	require.ErrorContains(t, validateProcessIsolation(&ProcessIsolation{UID: 1, GID: 2, BaseEnvironment: map[string]string{}}), "standalone owner")
}

func TestAdoptedIdentityDisposition(t *testing.T) {
	adopted := &ProcessIsolation{identityAuthorityAdopted: true}
	require.NoError(t, validateStandaloneIdentityDisposition(adopted))
	adopted.IdentityLock = testProcessIdentityCapability{}
	require.ErrorContains(t, validateStandaloneIdentityDisposition(adopted), "cannot carry")
	require.Error(t, applyProcessCredential(exec.Command("/usr/bin/true"), nil))
}

func TestSupervisorIdentityEnvironmentAndVerification(t *testing.T) {
	originalUID := processEffectiveUID
	originalGID := processEffectiveGID
	originalGroups := processGroups
	t.Cleanup(func() {
		processEffectiveUID = originalUID
		processEffectiveGID = originalGID
		processGroups = originalGroups
	})

	t.Setenv(processIsolationUIDEnv, "")
	t.Setenv(processIsolationGIDEnv, "")
	uid, _, _, err := expectedSupervisorIdentity()
	require.ErrorContains(t, err, "uid")
	require.Zero(t, uid)
	require.ErrorContains(t, verifySupervisorIdentity(), "uid")
	t.Setenv(processIsolationUIDEnv, "41")
	_, gid, implicit, err := expectedSupervisorIdentity()
	require.ErrorContains(t, err, "gid")
	require.Zero(t, gid)
	require.False(t, implicit)

	env := supervisorIdentityEnvironment([]string{"KEEP=yes"}, "MODE", "run", ProcessIsolation{UID: 41, GID: 42})
	values := environmentMap(env)
	require.Equal(t, "yes", values["KEEP"])
	require.Equal(t, "run", values["MODE"])
	require.Equal(t, "41", values[processIsolationUIDEnv])
	require.Equal(t, "42", values[processIsolationGIDEnv])

	t.Setenv(processIsolationGIDEnv, "42")
	processEffectiveUID = func() int { return 99 }
	processEffectiveGID = func() int { return 42 }
	require.ErrorContains(t, verifySupervisorIdentity(), "mismatch")
	processEffectiveUID = func() int { return 41 }
	processGroups = func() ([]int, error) { return nil, errors.New("groups") }
	require.ErrorContains(t, verifySupervisorIdentity(), "supplementary")
	processGroups = func() ([]int, error) { return []int{7}, nil }
	require.ErrorContains(t, verifySupervisorIdentity(), "not empty")
	processGroups = func() ([]int, error) { return nil, nil }
	require.NoError(t, verifySupervisorIdentity())
}
