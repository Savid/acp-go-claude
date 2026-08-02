//go:build !darwin

package claude

import "context"

// RemoveAuthKeychainItems has nothing to remove on Linux. The harness ships a
// separate binary per platform and the Linux artifact carries no keystore path
// at all: the plaintext store under the config dir is unconditionally
// authoritative there, and native logout removes it. Absence of a code path is
// not absence of an item, so this reports success rather than a lookup miss.
//
// Windows carries the same composite store Darwin does, over Credential
// Manager, and no item-name shape for it is pinned here: removal on Windows is
// whatever native logout performs, so a legacy item that survives it survives
// disconnect too.
func RemoveAuthKeychainItems(_ context.Context, _ string, _ string) error {
	return nil
}

// ReadAuthKeychainCredential has nothing to read outside Darwin, for the same
// residence reasons removal has nothing to remove: on Linux the plaintext
// store under the config dir is the only credential residence, and on Windows
// no Credential Manager item-name shape is pinned here. Absence, not an
// error, is the truthful answer.
func ReadAuthKeychainCredential(_ context.Context, _ string, _ string) ([]byte, error) {
	return nil, nil
}
