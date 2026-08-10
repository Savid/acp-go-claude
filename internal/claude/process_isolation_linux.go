//go:build linux

package claude

import (
	"os/exec"
	"syscall"
)

// applyProcessCredential hands the native child the explicitly configured
// identity. It exists only for the hardened Linux boundary: ordinary
// same-identity execution changes no credential at all, so it never reaches
// here and never sheds the ambient supplementary groups that belong to the
// identity the adapter already runs as.
func applyProcessCredential(cmd *exec.Cmd, isolation *ProcessIsolation) error {
	if err := validateProcessIsolation(isolation); err != nil {
		return err
	}

	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}

	cmd.SysProcAttr.Credential = &syscall.Credential{
		Uid: isolation.UID, Gid: isolation.GID, Groups: []uint32{}, NoSetGroups: false,
	}

	return nil
}
