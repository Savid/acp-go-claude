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

func TestProcessIsolationFailsClosed(t *testing.T) {
	require.Nil(t, BuildEnv(Options{ProcessIsolation: &ProcessIsolation{UID: 0, GID: 2, BaseEnvironment: map[string]string{}}}))
	require.Nil(t, BuildEnv(Options{ProcessIsolation: &ProcessIsolation{UID: 1, GID: 2, BaseEnvironment: map[string]string{"PATH": "relative"}, StandaloneOwnerID: "test-owner", StandaloneStateRoot: "/var/lib/acp-go-test"}}))
	require.Nil(t, BuildEnv(Options{OrdinaryEnvironment: map[string]string{"BAD=KEY": "x"}}))
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
}

// TestNativeLaunchIdentityEnvironmentAndVerification proves the private launch
// helper refuses to exec anything unless it already runs as the identity its
// parent stamped, and that the stamp survives an unrelated inherited variable.
func TestNativeLaunchIdentityEnvironmentAndVerification(t *testing.T) {
	originalUID := processEffectiveUID
	originalGID := processEffectiveGID
	t.Cleanup(func() {
		processEffectiveUID = originalUID
		processEffectiveGID = originalGID
	})

	t.Setenv(processIsolationUIDEnv, "")
	t.Setenv(processIsolationGIDEnv, "")
	uid, _, err := expectedLaunchIdentity()
	require.ErrorContains(t, err, "uid")
	require.Zero(t, uid)
	require.ErrorContains(t, verifyLaunchIdentity(), "uid")
	t.Setenv(processIsolationUIDEnv, "41")
	_, gid, err := expectedLaunchIdentity()
	require.ErrorContains(t, err, "gid")
	require.Zero(t, gid)

	processEffectiveUID = func() int { return 41 }
	processEffectiveGID = func() int { return 42 }

	env := ordinaryLaunchIdentityEnvironment([]string{"KEEP=yes"}, "MODE", "run")
	values := environmentMap(env)
	require.Equal(t, "yes", values["KEEP"])
	require.Equal(t, "run", values["MODE"])
	require.Equal(t, "41", values[processIsolationUIDEnv])
	require.Equal(t, "42", values[processIsolationGIDEnv])

	t.Setenv(processIsolationGIDEnv, "42")
	require.NoError(t, verifyLaunchIdentity())
	processEffectiveUID = func() int { return 99 }
	require.ErrorContains(t, verifyLaunchIdentity(), "mismatch")
}

// TestProcessIsolationOmissionAllowsRoot proves the ordinary launch identity
// handshake is honest for a root caller too: root is a legitimate ordinary
// identity, so a zero id round-trips instead of failing closed the way an
// explicit policy's zero id must.
func TestProcessIsolationOmissionAllowsRoot(t *testing.T) {
	originalUID := processEffectiveUID
	originalGID := processEffectiveGID
	t.Cleanup(func() {
		processEffectiveUID = originalUID
		processEffectiveGID = originalGID
	})

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

			values := environmentMap(ordinaryLaunchIdentityEnvironment(nil, "MODE", "run"))
			t.Setenv(processIsolationUIDEnv, values[processIsolationUIDEnv])
			t.Setenv(processIsolationGIDEnv, values[processIsolationGIDEnv])

			require.NoError(t, verifyLaunchIdentity())
		})
	}

	// An explicit policy is held to the opposite rule: zero ids never name a
	// hardened native identity, so the same shape fails closed.
	require.ErrorContains(t,
		validateProcessIsolation(&ProcessIsolation{BaseEnvironment: map[string]string{}}),
		"must be nonzero",
	)
}
