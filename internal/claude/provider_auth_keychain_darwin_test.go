package claude

import (
	"bytes"
	"context"
	"errors"
	"io"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRemoveAuthKeychainItemsCurrentBehavior(t *testing.T) {
	prior := authKeychainTool
	t.Cleanup(func() { authKeychainTool = prior })

	var calls [][]string
	authKeychainTool = func(ctx context.Context, args []string, _ Options) (int, error) {
		_, bounded := ctx.Deadline()
		require.True(t, bounded)
		calls = append(calls, append([]string(nil), args...))

		return 0, nil
	}
	require.NoError(t, RemoveAuthKeychainItems(t.Context(), "/tmp/cfg", "operator", Options{}))
	require.Len(t, calls, 2)
	require.Equal(t, "delete-generic-password", calls[0][0])
	require.Equal(t, "operator", calls[0][4])

	authKeychainTool = func(context.Context, []string, Options) (int, error) { return 44, nil }
	require.NoError(t, RemoveAuthKeychainItems(t.Context(), "/tmp/cfg", "operator", Options{}))
	authKeychainTool = func(context.Context, []string, Options) (int, error) { return 51, nil }
	require.ErrorContains(t, RemoveAuthKeychainItems(t.Context(), "/tmp/cfg", "operator", Options{}), "status 51")
	want := errors.New("tool unavailable")
	authKeychainTool = func(context.Context, []string, Options) (int, error) { return 0, want }
	require.ErrorIs(t, RemoveAuthKeychainItems(t.Context(), "/tmp/cfg", "operator", Options{}), want)
}

func TestReadAuthKeychainCredentialCurrentBehavior(t *testing.T) {
	prior := authKeychainReadTool
	t.Cleanup(func() { authKeychainReadTool = prior })

	calls := 0
	authKeychainReadTool = func(ctx context.Context, _ []string, _ Options) ([]byte, int, error) {
		_, bounded := ctx.Deadline()
		require.True(t, bounded)
		calls++
		if calls == 1 {
			return nil, 44, nil
		}

		return []byte(`{"claudeAiOauth":{"accessToken":"unit-secret"}}` + "\n"), 0, nil
	}
	credential, err := ReadAuthKeychainCredential(t.Context(), "/tmp/cfg", "operator", Options{})
	require.NoError(t, err)
	require.Equal(t, `{"claudeAiOauth":{"accessToken":"unit-secret"}}`, string(credential))
	require.Equal(t, 2, calls)

	authKeychainReadTool = func(context.Context, []string, Options) ([]byte, int, error) { return []byte("\n"), 0, nil }
	credential, err = ReadAuthKeychainCredential(t.Context(), "/tmp/cfg", "operator", Options{})
	require.NoError(t, err)
	require.Nil(t, credential)

	authKeychainReadTool = func(context.Context, []string, Options) ([]byte, int, error) { return nil, 51, nil }
	_, err = ReadAuthKeychainCredential(t.Context(), "/tmp/cfg", "operator", Options{})
	require.ErrorContains(t, err, "status 51")
}

func TestManagedAuthKeychainLaunchDoesNotPrepareOperatorClaudeHome(t *testing.T) {
	var prepared []string
	var request NativeRequest
	authority := &NativeAuthority{
		NativeEnvironment: func() map[string]string { return map[string]string{"PATH": "/usr/bin:/bin"} },
		PrepareNativeTree: func(_ context.Context, root string) error {
			prepared = append(prepared, root)

			return nil
		},
		ReclaimNativeTree: func(context.Context, string) error { return nil },
		StartNative: func(_ context.Context, got NativeRequest) (NativeProcess, error) {
			request = got

			return &authorityTestProcess{
				stdin: &authorityTestWriteCloser{}, stdout: io.NopCloser(bytes.NewBufferString("keychain\n")), stderr: io.NopCloser(bytes.NewReader(nil)),
				wait:   func(context.Context) (NativeResult, error) { return NativeResult{}, nil },
				revoke: func(context.Context) error { return nil },
			}, nil
		},
	}
	output, code, err := runContainedAuthKeychainTool(t.Context(), []string{"list-keychains"}, Options{
		ClaudeHome: "/operator/.claude", Authority: authority,
	})
	require.NoError(t, err)
	require.Zero(t, code)
	require.Equal(t, "keychain\n", string(output))
	require.Empty(t, prepared)
	require.Equal(t, "security", request.Executable)
	require.Equal(t, "/", request.WorkingDirectory)
}
