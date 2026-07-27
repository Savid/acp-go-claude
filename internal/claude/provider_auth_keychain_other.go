//go:build !darwin

package claude

import "context"

// RemoveAuthKeychainItems has nothing to remove off Darwin. The harness ships a
// separate binary per platform and the Linux artifact carries no keystore path
// at all: the plaintext store under the config dir is unconditionally
// authoritative there, and native logout removes it. Absence of a code path is
// not absence of an item, so this reports success rather than a lookup miss.
func RemoveAuthKeychainItems(_ context.Context, _ string, _ string) error {
	return nil
}
