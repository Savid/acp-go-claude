//go:build !darwin

package claudeacp

import "github.com/savid/acp-go-claude/internal/claude"

// readClaudeResumeKeychainCredential answers absence outside darwin: no other
// platform build carries a keystore credential residence this agent reads, so
// the plaintext file under the source config dir is the only leg the resume
// copy has.
func readClaudeResumeKeychainCredential(_ string, _ ...claude.Options) ([]byte, error) {
	return nil, nil
}
