package claudeacp

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/stretchr/testify/require"
)

func TestReadClaudeResumeKeychainCredentialConsultsTheKeystoreUnderEveryLaunchMode(t *testing.T) {
	original := readAuthKeychainCredential
	t.Cleanup(func() { readAuthKeychainCredential = original })

	// A config dir nothing ever logged into owns no keychain item, so the
	// keystore answers absence rather than an error. That is what keeps
	// file-authenticated and env-authenticated homes on the plaintext leg.
	readAuthKeychainCredential = func(context.Context, string, string, claude.Options) ([]byte, error) {
		return nil, nil
	}
	data, err := readClaudeResumeKeychainCredential(filepath.Join(t.TempDir(), "never-logged-in"))
	require.NoError(t, err)
	require.Nil(t, data)

	// The materialized resume destination never hashes to the login home's
	// item name, whatever launch mode spawned the CLI, so ordinary
	// same-identity execution consults the keystore exactly like an explicit
	// isolation deployment does.
	var consultedSource string
	readAuthKeychainCredential = func(_ context.Context, source string, _ string, _ claude.Options) ([]byte, error) {
		consultedSource = source

		return []byte("credential"), nil
	}
	ordinarySource := filepath.Join(t.TempDir(), "ordinary-login-home")
	data, err = readClaudeResumeKeychainCredential(ordinarySource, claude.Options{})
	require.NoError(t, err)
	require.Equal(t, []byte("credential"), data)
	require.Equal(t, ordinarySource, consultedSource)

	readAuthKeychainCredential = func(context.Context, string, string, claude.Options) ([]byte, error) {
		return nil, errors.New("keychain failed")
	}
	_, err = readClaudeResumeKeychainCredential(filepath.Join(t.TempDir(), "failing-keystore"))
	require.ErrorContains(t, err, "claude resume keystore credential")

	readAuthKeychainCredential = func(context.Context, string, string, claude.Options) ([]byte, error) {
		return []byte("credential"), nil
	}
	data, err = readClaudeResumeKeychainCredential(filepath.Join(t.TempDir(), "credential"), claude.Options{ProcessIsolation: &claude.ProcessIsolation{}})
	require.NoError(t, err)
	require.Equal(t, []byte("credential"), data)
}
