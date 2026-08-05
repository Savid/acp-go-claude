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
// CLI accepts in its place. The context is fresh because the copy runs on
// the session-load path without one, and every keystore call underneath
// carries its own bound.
func readClaudeResumeKeychainCredential(source string, provided ...claude.Options) ([]byte, error) {
	var options claude.Options
	if len(provided) > 0 {
		options = provided[0]
	}

	if options.ProcessIsolation == nil {
		return nil, nil
	}

	data, err := readAuthKeychainCredential(context.Background(), source, authNativeUser(options), options)
	if err != nil {
		return nil, fmt.Errorf("claude resume keystore credential: %w", err)
	}

	return data, nil
}
