//go:build !darwin

package claude

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestAuthKeychainIsAbsentRatherThanUnsupportedOffDarwin proves the non-Darwin
// keystore surface reports absence, not failure. Disconnect calls the removal
// leg unconditionally and the credential read feeds a residence check, so an
// "unsupported platform" error from either would turn a Linux logout into a
// reported failure over a store that never existed, and would make the
// plaintext store under the config dir look unreadable rather than
// authoritative.
func TestAuthKeychainIsAbsentRatherThanUnsupportedOffDarwin(t *testing.T) {
	ctx := t.Context()

	require.NoError(t, RemoveAuthKeychainItems(ctx, "claude.ai", "user@example.com", Options{}))

	credential, err := ReadAuthKeychainCredential(ctx, "claude.ai", "user@example.com", Options{})
	require.NoError(t, err)
	require.Nil(t, credential)
}
