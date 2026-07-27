package claude

import (
	"crypto/sha256"
	"encoding/hex"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestAuthKeychainItemsCoversBothItemsAndBothNameShapes(t *testing.T) {
	t.Parallel()

	const configDir = "/tmp/claude-cfg-A"

	sum := sha256.Sum256([]byte(configDir))
	hash := hex.EncodeToString(sum[:])[:8]

	require.Equal(t, []AuthKeychainItem{
		{Service: "Claude Code-credentials-" + hash, Account: "operator"},
		{Service: "Claude Code-" + hash, Account: "operator"},
		{Service: "Claude Code-custom-oauth-credentials-" + hash, Account: "operator"},
		{Service: "Claude Code-custom-oauth-" + hash, Account: "operator"},
	}, AuthKeychainItems(configDir, "operator"))

	// Two config dirs never share an item name.
	require.NotEqual(t, AuthKeychainItems(configDir, "operator"), AuthKeychainItems("/tmp/claude-cfg-B", "operator"))
}

func TestAuthKeychainAccountFallsBackWhenTheUserFailsTheHarnessSanitiser(t *testing.T) {
	t.Parallel()

	require.Equal(t, "operator", authKeychainAccount("operator"))
	require.Equal(t, "user.name-1_2", authKeychainAccount("user.name-1_2"))
	require.Equal(t, authKeychainAccountFallback, authKeychainAccount(""))
	require.Equal(t, authKeychainAccountFallback, authKeychainAccount("bad user"))
	require.Equal(t, authKeychainAccountFallback, authKeychainAccount("prefix\nsuffix"))
}

func TestAuthKeychainAbsentExitCodes(t *testing.T) {
	t.Parallel()

	require.True(t, authKeychainAbsent(36))
	require.True(t, authKeychainAbsent(44))
	require.False(t, authKeychainAbsent(0))
	require.False(t, authKeychainAbsent(1))
}
