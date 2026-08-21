package claude

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAuthKeychainItemsCoversOnlyCurrentCredentialNameShapes(t *testing.T) {
	t.Parallel()

	const configDir = "/tmp/claude-cfg-A"

	sum := sha256.Sum256([]byte(configDir))
	hash := hex.EncodeToString(sum[:])[:8]

	require.Equal(t, []AuthKeychainItem{
		{Service: "Claude Code-credentials-" + hash, Account: "operator"},
		{Service: "Claude Code-custom-oauth-credentials-" + hash, Account: "operator"},
	}, AuthKeychainItems(configDir, "operator"))

	// Two config dirs never share an item name.
	require.NotEqual(t, AuthKeychainItems(configDir, "operator"), AuthKeychainItems("/tmp/claude-cfg-B", "operator"))
}

func TestAuthKeychainCredentialItemsCoversOnlyTheCredentialShapes(t *testing.T) {
	t.Parallel()

	const configDir = "/tmp/claude-cfg-A"

	sum := sha256.Sum256([]byte(configDir))
	hash := hex.EncodeToString(sum[:])[:8]

	require.Equal(t, []AuthKeychainItem{
		{Service: "Claude Code-credentials-" + hash, Account: "operator"},
		{Service: "Claude Code-custom-oauth-credentials-" + hash, Account: "operator"},
	}, AuthKeychainCredentialItems(configDir, "operator"))

	require.NotEqual(t,
		AuthKeychainCredentialItems(configDir, "operator"),
		AuthKeychainCredentialItems("/tmp/claude-cfg-B", "operator"))
}

func TestAuthKeychainAccountFallsBackWhenTheUserFailsTheHarnessSanitiser(t *testing.T) {
	t.Parallel()

	require.Equal(t, "operator", authKeychainAccount("operator"))
	require.Equal(t, "user.name-1_2", authKeychainAccount("user.name-1_2"))
	require.Equal(t, authKeychainAccountFallback, authKeychainAccount(""))
	require.Equal(t, authKeychainAccountFallback, authKeychainAccount("bad user"))
	require.Equal(t, authKeychainAccountFallback, authKeychainAccount("prefix\nsuffix"))
}

func TestAuthKeychainAbsentIsExactlyTheTwoDocumentedStatuses(t *testing.T) {
	t.Parallel()

	// Absence is the only status the removal ladder may swallow. Widening this
	// set makes a keychain that refused the delete — 51, the refusal a locked or
	// denied keychain answers with — read as one that never held the item, and
	// disconnect then reports success over a credential that is still there.
	absent := map[int]bool{36: true, 44: true}

	for code := range 128 {
		require.Equal(t, absent[code], authKeychainAbsent(code), "status %d", code)
	}
}
