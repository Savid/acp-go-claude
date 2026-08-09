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

// sharedProcessIdentity reports whether the native identity is the identity the
// supervisor already runs as. Nothing separates the two ends of the launch in
// that shape, so every step that exists to cross the boundary has nothing to
// cross. A zero effective uid never qualifies: the supervisor holds the trusted
// identity there, and a nonzero native uid is required everywhere, so the two
// can never name the same identity. Only the Linux backend recognises the
// shape; the Darwin backend states its own boundary and is left as it is.
func sharedProcessIdentity(isolation *ProcessIsolation) bool {
	if isolation == nil || processIsolationGOOS != processIsolationLinux {
		return false
	}

	effectiveUID := processEffectiveUID()

	return effectiveUID > 0 && uint64(isolation.UID) == uint64(effectiveUID)
}

func applyProcessCredential(cmd *exec.Cmd, isolation *ProcessIsolation) error {
	if err := validateProcessIsolation(isolation); err != nil {
		return err
	}

	if cmd.SysProcAttr == nil {
		cmd.SysProcAttr = &syscall.SysProcAttr{}
	}

	// Requesting no credential change at all is the only honest instruction
	// when the native identity is already the running one. The supplementary
	// groups belong to the account the supervisor was started under, and an
	// unprivileged process can neither shed them nor re-enter them.
	if sharedProcessIdentity(isolation) {
		effectiveGID := processEffectiveGID()
		if effectiveGID < 0 || uint64(isolation.GID) != uint64(effectiveGID) {
			return fmt.Errorf(
				"native group %d cannot be entered from group %d; %s",
				isolation.GID, effectiveGID, sharedIdentitySupervisorRemedy,
			)
		}

		return nil
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
