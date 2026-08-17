//go:build unix

package claude

import (
	"fmt"
	"os"
)

var (
	processEffectiveUID = os.Geteuid
	processEffectiveGID = os.Getegid
)

// validateProcessIsolationPlatform has nothing left to reject on a unix
// platform: an explicit policy carries its own identity, and the platform gate
// that refuses one outside Linux lives in the launch backend, before any spawn.
func validateProcessIsolationPlatform(*ProcessIsolation) error { return nil }

// ordinaryLaunchIdentityEnvironment stamps the identity an ordinary private
// bootstrap helper must already hold. Ordinary execution changes no credential,
// so the identity it names is simply the adapter's own.
func ordinaryLaunchIdentityEnvironment(env []string, modeKey string, mode string) []string {
	return launchIdentityEnvironment(env, modeKey, mode, processEffectiveUID(), processEffectiveGID())
}

// verifyLaunchIdentity proves a private bootstrap helper runs as the identity
// its parent stamped before it execs the native command. Ordinary execution
// applies no credential, so the ambient supplementary groups belong to that
// identity and are not inspected.
func verifyLaunchIdentity() error {
	uid, gid, err := expectedLaunchIdentity()
	if err != nil {
		return err
	}

	actualUID, actualGID := processEffectiveUID(), processEffectiveGID()
	if int64(actualUID) != uid || int64(actualGID) != gid {
		return fmt.Errorf("native launch identity mismatch: got %d:%d, want %d:%d", actualUID, actualGID, uid, gid)
	}

	return nil
}
