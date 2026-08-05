//go:build unix

package claude

import (
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

var (
	processEffectiveUID = os.Geteuid
	processEffectiveGID = os.Getegid
	processGroups       = os.Getgroups
)

func validateProcessIsolationPlatform() error { return nil }

func applyProcessCredential(cmd *exec.Cmd, isolation *ProcessIsolation) error {
	if err := validateProcessIsolation(isolation); err != nil {
		return err
	}

	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}

	cmd.SysProcAttr.Credential = &syscall.Credential{Uid: isolation.UID, Gid: isolation.GID, Groups: []uint32{}, NoSetGroups: false}

	return nil
}

func verifySupervisorIdentity() error {
	uid, gid, err := expectedSupervisorIdentity()
	if err != nil {
		return err
	}

	actualUID, actualGID := processEffectiveUID(), processEffectiveGID()
	if actualUID < 0 || actualGID < 0 || uint64(actualUID) != uint64(uid) || uint64(actualGID) != uint64(gid) {
		return fmt.Errorf("process isolation identity mismatch: got %d:%d, want %d:%d", actualUID, actualGID, uid, gid)
	}

	groups, err := processGroups()
	if err != nil {
		return fmt.Errorf("read process isolation supplementary groups: %w", err)
	}

	if len(groups) != 0 {
		return fmt.Errorf("process isolation supplementary groups are not empty: %v", groups)
	}

	return nil
}
