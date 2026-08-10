//go:build !unix

package claude

import "errors"

// validateProcessIsolationPlatform refuses every explicitly supplied hardened
// policy here: the platform has no credential boundary to apply. Ordinary
// same-identity execution supplies no policy, so it never reaches this gate.
func validateProcessIsolationPlatform(*ProcessIsolation) error {
	return errors.New("process isolation is unsupported on this platform")
}
