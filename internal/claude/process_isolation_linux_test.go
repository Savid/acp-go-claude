//go:build linux

package claude

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestApplyProcessCredentialIsExplicitOnly proves the credential change belongs
// to a supplied hardened policy alone: it validates that policy first, sheds
// every supplementary group, and has no ordinary counterpart to fall back to.
func TestApplyProcessCredentialIsExplicitOnly(t *testing.T) {
	require.ErrorContains(t, applyProcessCredential(exec.Command("/bin/sh"), nil), "required")
	require.ErrorContains(t,
		applyProcessCredential(exec.Command("/bin/sh"), &ProcessIsolation{
			UID: 64251, GID: 64252, BaseEnvironment: map[string]string{},
		}),
		"standalone owner",
		"a hardened policy is validated before any credential is attached",
	)

	cmd := exec.Command("/bin/sh")
	require.NoError(t, applyProcessCredential(cmd, &ProcessIsolation{
		UID: 64251, GID: 64252, BaseEnvironment: map[string]string{},
		StandaloneOwnerID: "test-owner", StandaloneStateRoot: "/var/lib/acp-go-test",
	}))
	require.Equal(t, uint32(64251), cmd.SysProcAttr.Credential.Uid)
	require.Equal(t, uint32(64252), cmd.SysProcAttr.Credential.Gid)
	require.Empty(t, cmd.SysProcAttr.Credential.Groups)
	require.False(t, cmd.SysProcAttr.Credential.NoSetGroups)
}

func TestProcessIsolationActualIdentityGroupsAndAmbientScrub(t *testing.T) {
	if os.Getenv("ACP_PROCESS_ISOLATION_HELPER") == "1" {
		groups, err := os.Getgroups()
		if err != nil {
			os.Exit(2)
		}
		fmt.Printf("%d:%d:%d:%s", os.Geteuid(), os.Getegid(), len(groups), os.Getenv("ACP_PROCESS_AMBIENT_CANARY"))
		os.Exit(0)
	}
	if os.Geteuid() != 0 {
		t.Skip("actual credential-drop proof requires a privileged Linux test process")
	}

	root, err := os.MkdirTemp("/tmp", "acp-go-claude-isolation-")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, os.RemoveAll(root)) })
	require.NoError(t, os.Chmod(root, 0o755))
	binary := filepath.Join(root, "isolation-helper")
	data, err := os.ReadFile(os.Args[0])
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(binary, data, 0o755))
	t.Setenv("ACP_PROCESS_AMBIENT_CANARY", "must-not-leak")

	const target = uint32(65534)
	cmd := exec.Command(binary, "-test.run=^TestProcessIsolationActualIdentityGroupsAndAmbientScrub$")
	cmd.Dir = "/"
	cmd.Env = []string{"PATH=/usr/bin:/bin", "ACP_PROCESS_ISOLATION_HELPER=1"}
	require.NoError(t, applyProcessCredential(cmd, &ProcessIsolation{
		UID: target, GID: target, BaseEnvironment: map[string]string{"PATH": "/usr/bin:/bin"},
		StandaloneOwnerID: "test-owner", StandaloneStateRoot: "/var/lib/acp-go-test",
	}))
	output, err := cmd.Output()
	require.NoError(t, err)
	require.Equal(t, "65534:65534:0:", strings.TrimSpace(string(output)))
}
