package claude

import (
	"context"
	"errors"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRemoveAuthKeychainItemsRemovesEveryItemUnderABoundedCall(t *testing.T) {
	original := authKeychainTool

	t.Cleanup(func() { authKeychainTool = original })

	var calls [][]string

	authKeychainTool = func(ctx context.Context, args []string, _ Options) (int, error) {
		deadline, ok := ctx.Deadline()
		require.True(t, ok, "every keystore call carries a bound")
		require.False(t, deadline.IsZero())

		calls = append(calls, args)

		return 0, nil
	}

	require.NoError(t, RemoveAuthKeychainItems(t.Context(), "/tmp/cfg", "operator", Options{}))
	require.Len(t, calls, 4)
	require.Equal(t, "delete-generic-password", calls[0][0])
	require.Equal(t, "-a", calls[0][3])
	require.Equal(t, "operator", calls[0][4])
}

func TestRemoveAuthKeychainItemsSeparatesAbsenceFromTransientFailure(t *testing.T) {
	original := authKeychainTool

	t.Cleanup(func() { authKeychainTool = original })

	authKeychainTool = func(context.Context, []string, Options) (int, error) { return 44, nil }
	require.NoError(t, RemoveAuthKeychainItems(t.Context(), "/tmp/cfg", "operator", Options{}))

	authKeychainTool = func(context.Context, []string, Options) (int, error) { return 1, nil }
	require.ErrorContains(t, RemoveAuthKeychainItems(t.Context(), "/tmp/cfg", "operator", Options{}), "status 1")

	// A keychain that refuses the delete answers 51 with the item still in it.
	// Reported as success, the caller would tell the operator a credential was
	// cleared that a later login still finds.
	authKeychainTool = func(context.Context, []string, Options) (int, error) { return 51, nil }
	require.ErrorContains(t, RemoveAuthKeychainItems(t.Context(), "/tmp/cfg", "operator", Options{}), "status 51")

	want := errors.New("tool missing")
	authKeychainTool = func(context.Context, []string, Options) (int, error) { return 0, want }
	require.ErrorIs(t, RemoveAuthKeychainItems(t.Context(), "/tmp/cfg", "operator", Options{}), want)
}

func TestReadAuthKeychainCredentialReturnsTheFirstPresentItemUnderABoundedCall(t *testing.T) {
	original := authKeychainReadTool

	t.Cleanup(func() { authKeychainReadTool = original })

	var calls [][]string

	authKeychainReadTool = func(ctx context.Context, args []string, _ Options) ([]byte, int, error) {
		deadline, ok := ctx.Deadline()
		require.True(t, ok, "every keystore call carries a bound")
		require.False(t, deadline.IsZero())

		calls = append(calls, args)
		if len(calls) == 1 {
			return nil, 44, nil
		}

		// The platform tool appends one newline the stored blob never carries.
		return []byte(`{"claudeAiOauth":{"accessToken":"unit-secret"}}` + "\n"), 0, nil
	}

	data, err := ReadAuthKeychainCredential(t.Context(), "/tmp/cfg", "operator", Options{})
	require.NoError(t, err)
	require.Equal(t, `{"claudeAiOauth":{"accessToken":"unit-secret"}}`, string(data))
	require.Len(t, calls, 2)
	require.Equal(t, "find-generic-password", calls[0][0])
	require.Equal(t, "-a", calls[0][3])
	require.Equal(t, "operator", calls[0][4])
	require.Equal(t, "-w", calls[0][5])
}

func TestReadAuthKeychainCredentialAnswersAbsenceForMissingAndEmptyItems(t *testing.T) {
	original := authKeychainReadTool

	t.Cleanup(func() { authKeychainReadTool = original })

	authKeychainReadTool = func(context.Context, []string, Options) ([]byte, int, error) { return nil, 44, nil }
	data, err := ReadAuthKeychainCredential(t.Context(), "/tmp/cfg", "operator", Options{})
	require.NoError(t, err)
	require.Nil(t, data)

	// An item holding nothing is absence, not a credential.
	authKeychainReadTool = func(context.Context, []string, Options) ([]byte, int, error) { return []byte("\n"), 0, nil }
	data, err = ReadAuthKeychainCredential(t.Context(), "/tmp/cfg", "operator", Options{})
	require.NoError(t, err)
	require.Nil(t, data)
}

func TestReadAuthKeychainCredentialSeparatesAbsenceFromTransientFailure(t *testing.T) {
	original := authKeychainReadTool

	t.Cleanup(func() { authKeychainReadTool = original })

	authKeychainReadTool = func(context.Context, []string, Options) ([]byte, int, error) { return nil, 51, nil }
	_, err := ReadAuthKeychainCredential(t.Context(), "/tmp/cfg", "operator", Options{})
	require.ErrorContains(t, err, "status 51")

	want := errors.New("tool missing")
	authKeychainReadTool = func(context.Context, []string, Options) ([]byte, int, error) { return nil, 0, want }
	_, err = ReadAuthKeychainCredential(t.Context(), "/tmp/cfg", "operator", Options{})
	require.ErrorIs(t, err, want)
}

func TestReadAuthKeychainCredentialPrefersALaterItemOverAnEarlierFailure(t *testing.T) {
	original := authKeychainReadTool

	t.Cleanup(func() { authKeychainReadTool = original })

	var calls int

	authKeychainReadTool = func(context.Context, []string, Options) ([]byte, int, error) {
		calls++
		if calls == 1 {
			return nil, 51, nil
		}

		return []byte(`{"claudeAiOauth":{}}`), 0, nil
	}

	// A credential that is actually there beats reporting the first item's
	// refusal: the session either resumes logged in or it does not.
	data, err := ReadAuthKeychainCredential(t.Context(), "/tmp/cfg", "operator", Options{})
	require.NoError(t, err)
	require.Equal(t, `{"claudeAiOauth":{}}`, string(data))
}

func TestAuthKeychainReadToolReportsTheRealPlatformExitStatus(t *testing.T) {
	skipUnprivilegedDarwinIsolation(t)
	// The real tool answers "absent" for an item nothing ever wrote, which is
	// what lets a file-authenticated home resume without a keystore entry.
	output, code, err := authKeychainReadTool(t.Context(), []string{
		"find-generic-password",
		"-s", "acp-go-claude-canary-service-that-does-not-exist",
		"-a", "acp-go-claude-canary-account",
		"-w",
	}, Options{})
	require.NoError(t, err)
	require.Empty(t, output)
	require.True(t, authKeychainAbsent(code), "unexpected status %d", code)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, _, err = authKeychainReadTool(ctx, []string{"list-keychains"}, Options{})
	require.Error(t, err)
}

func TestAuthKeychainToolReportsTheRealPlatformExitStatus(t *testing.T) {
	skipUnprivilegedDarwinIsolation(t)
	// The real tool answers "absent" for an item nothing ever wrote, which is
	// what keeps disconnect idempotent on a home that holds no credential.
	code, err := authKeychainTool(t.Context(), []string{
		"delete-generic-password",
		"-s", "acp-go-claude-canary-service-that-does-not-exist",
		"-a", "acp-go-claude-canary-account",
	}, Options{})
	require.NoError(t, err)
	require.True(t, authKeychainAbsent(code), "unexpected status %d", code)
}

func TestAuthKeychainToolSeparatesSuccessFromALaunchFailure(t *testing.T) {
	skipUnprivilegedDarwinIsolation(t)
	code, err := authKeychainTool(t.Context(), []string{"list-keychains"}, Options{})
	require.NoError(t, err)
	require.Zero(t, code)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err = authKeychainTool(ctx, []string{"list-keychains"}, Options{})
	require.Error(t, err)
}
