package claudeacp

import (
	"context"
	"fmt"

	"github.com/savid/acp-go-claude/internal/claude"
)

var readAuthKeychainCredential = claude.ReadAuthKeychainCredential

// readClaudeResumeKeychainCredential answers with the login Keychain
// credential blob the source config dir owns. A native login on darwin
// stores the OAuth credential only in the Keychain, keyed by a hash of the
// config dir path; a materialized temp dir hashes to an item name nothing
// ever wrote, so the blob must travel as the plaintext credential file the
// CLI accepts in its place. That premise keys on the resume destination
// differing from the login home — which a materialized temp dir always
// does — so the read runs under every launch mode, ordinary same-identity
// execution included. A home nothing natively logged into owns no item and
// answers absence, which keeps file- and env-authenticated homes on the
// plaintext leg. The context is fresh because the copy runs on the
// session-load path without one, and every keystore call underneath
// carries its own bound.
func readClaudeResumeKeychainCredential(source string, provided ...claude.Options) ([]byte, error) {
	var options claude.Options
	if len(provided) > 0 {
		options = provided[0]
	}

	data, err := readAuthKeychainCredential(context.Background(), source, authNativeUser(options), options)
	if err != nil {
		return nil, fmt.Errorf("claude resume keystore credential: %w", err)
	}

	return data, nil
}
