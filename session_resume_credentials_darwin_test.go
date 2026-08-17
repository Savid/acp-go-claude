package claudeacp

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/stretchr/testify/require"
)

func TestReadClaudeResumeKeychainCredentialAnswersAbsenceForAnUnwrittenHome(t *testing.T) {
	original := readAuthKeychainCredential
	t.Cleanup(func() { readAuthKeychainCredential = original })
	// A config dir nothing ever logged into owns no keychain item, so the
	// real keystore answers absence rather than an error. That is what keeps
	// file-authenticated and env-authenticated homes on the plaintext leg.
	data, err := readClaudeResumeKeychainCredential(filepath.Join(t.TempDir(), "never-logged-in"))
	require.NoError(t, err)
	require.Nil(t, data)

	readAuthKeychainCredential = func(context.Context, string, string, claude.Options) ([]byte, error) {
		return nil, errors.New("keychain failed")
	}
	_, err = readClaudeResumeKeychainCredential(filepath.Join(t.TempDir(), "invalid-policy"), claude.Options{ProcessIsolation: &claude.ProcessIsolation{}})
	require.ErrorContains(t, err, "claude resume keystore credential")

	readAuthKeychainCredential = func(context.Context, string, string, claude.Options) ([]byte, error) {
		return []byte("credential"), nil
	}
	data, err = readClaudeResumeKeychainCredential(filepath.Join(t.TempDir(), "credential"), claude.Options{ProcessIsolation: &claude.ProcessIsolation{}})
	require.NoError(t, err)
	require.Equal(t, []byte("credential"), data)
}
