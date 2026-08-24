package claude

import (
	"crypto/sha256"
	"encoding/hex"
	"regexp"
)

// AuthKeychainItem names one generic-password item the login Keychain may hold
// for a config dir.
type AuthKeychainItem struct {
	Service string
	Account string
}

// authKeychainAccountFallback is the account claude uses when $USER fails its
// own sanitiser.
const authKeychainAccountFallback = "claude-code-user"

// authKeychainAccountPattern is anchored, matching the harness's own check. A
// substring match would accept a user name carrying separators the harness
// rejects and produce an item name nothing wrote.
var authKeychainAccountPattern = regexp.MustCompile(`\A[a-zA-Z0-9._-]+\z`)

// authKeychainServicePrefixes covers the two reachable item-name shapes. The
// custom-OAuth suffix is a shipped deployment mode, so a leg pinned only to the
// default shape is blind to a worker that logged in under one.
var authKeychainServicePrefixes = []string{"Claude Code", "Claude Code-custom-oauth"}

// authKeychainAbsentExitCodes are the platform tool's answers meaning the item
// is not there. Any other non-zero status is transient and is never read as
// absence.
var authKeychainAbsentExitCodes = []int{36, 44}

// AuthKeychainItems lists the current composite credential item a config dir may
// own across both reachable name shapes.
func AuthKeychainItems(configDir string, user string) []AuthKeychainItem {
	hash := authKeychainHash(configDir)
	account := authKeychainAccount(user)
	items := make([]AuthKeychainItem, 0, len(authKeychainServicePrefixes))

	for _, prefix := range authKeychainServicePrefixes {
		items = append(items, AuthKeychainItem{Service: prefix + "-credentials-" + hash, Account: account})
	}

	return items
}

// AuthKeychainCredentialItems lists the items that may hold a config dir's
// composite OAuth credential blob, across both reachable name shapes.
func AuthKeychainCredentialItems(configDir string, user string) []AuthKeychainItem {
	hash := authKeychainHash(configDir)
	account := authKeychainAccount(user)
	items := make([]AuthKeychainItem, 0, len(authKeychainServicePrefixes))

	for _, prefix := range authKeychainServicePrefixes {
		items = append(items, AuthKeychainItem{Service: prefix + "-credentials-" + hash, Account: account})
	}

	return items
}

func authKeychainHash(configDir string) string {
	sum := sha256.Sum256([]byte(configDir))

	return hex.EncodeToString(sum[:])[:8]
}

func authKeychainAccount(user string) string {
	if authKeychainAccountPattern.MatchString(user) {
		return user
	}

	return authKeychainAccountFallback
}

func authKeychainAbsent(code int) bool {
	for _, absent := range authKeychainAbsentExitCodes {
		if code == absent {
			return true
		}
	}

	return false
}
