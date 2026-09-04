package claudeacp

import (
	"context"
	"errors"
	"path/filepath"
	"testing"

	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/stretchr/testify/require"
)

func TestReadClaudeResumeKeychainCredentialUsesSelectedNativeBoundary(t *testing.T) {
	prior := readAuthKeychainCredential
	t.Cleanup(func() { readAuthKeychainCredential = prior })

	readAuthKeychainCredential = func(context.Context, string, string, claude.Options) ([]byte, error) {
		return nil, nil
	}
	data, err := readClaudeResumeKeychainCredential(filepath.Join(t.TempDir(), "never-logged-in"))
	require.NoError(t, err)
	require.Nil(t, data)

	var consultedSource string
	var consultedAuthority *claude.NativeAuthority
	readAuthKeychainCredential = func(_ context.Context, source string, _ string, options claude.Options) ([]byte, error) {
		consultedSource = source
		consultedAuthority = options.Authority

		return []byte("credential"), nil
	}
	source := filepath.Join(t.TempDir(), "login-home")
	authority := &claude.NativeAuthority{}
	data, err = readClaudeResumeKeychainCredential(source, claude.Options{Authority: authority})
	require.NoError(t, err)
	require.Equal(t, []byte("credential"), data)
	require.Equal(t, source, consultedSource)
	require.Same(t, authority, consultedAuthority)

	want := errors.New("keychain failed")
	readAuthKeychainCredential = func(context.Context, string, string, claude.Options) ([]byte, error) {
		return nil, want
	}
	_, err = readClaudeResumeKeychainCredential(filepath.Join(t.TempDir(), "failure"))
	require.ErrorIs(t, err, want)
	require.ErrorContains(t, err, "claude resume keystore credential")
}
