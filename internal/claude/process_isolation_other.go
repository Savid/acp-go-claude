//go:build !unix

package claude

import (
	"errors"
	"os/exec"
)

func validateProcessIsolationPlatform() error {
	return errors.New("process isolation is unsupported on this platform")
}
func sharedProcessIdentity(*ProcessIsolation) bool { return false }
func applyProcessCredential(*exec.Cmd, *ProcessIsolation) error {
	return errors.New("process isolation is unsupported on this platform")
}
func verifySupervisorIdentity() error {
	return errors.New("process isolation is unsupported on this platform")
}
