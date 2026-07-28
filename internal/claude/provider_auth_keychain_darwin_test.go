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

	authKeychainTool = func(ctx context.Context, args []string) (int, error) {
		deadline, ok := ctx.Deadline()
		require.True(t, ok, "every keystore call carries a bound")
		require.False(t, deadline.IsZero())

		calls = append(calls, args)

		return 0, nil
	}

	require.NoError(t, RemoveAuthKeychainItems(t.Context(), "/tmp/cfg", "operator"))
	require.Len(t, calls, 4)
	require.Equal(t, "delete-generic-password", calls[0][0])
	require.Equal(t, "-a", calls[0][3])
	require.Equal(t, "operator", calls[0][4])
}

func TestRemoveAuthKeychainItemsSeparatesAbsenceFromTransientFailure(t *testing.T) {
	original := authKeychainTool

	t.Cleanup(func() { authKeychainTool = original })

	authKeychainTool = func(context.Context, []string) (int, error) { return 44, nil }
	require.NoError(t, RemoveAuthKeychainItems(t.Context(), "/tmp/cfg", "operator"))

	authKeychainTool = func(context.Context, []string) (int, error) { return 1, nil }
	require.ErrorContains(t, RemoveAuthKeychainItems(t.Context(), "/tmp/cfg", "operator"), "status 1")

	// A keychain that refuses the delete answers 51 with the item still in it.
	// Reported as success, the caller would tell the operator a credential was
	// cleared that a later login still finds.
	authKeychainTool = func(context.Context, []string) (int, error) { return 51, nil }
	require.ErrorContains(t, RemoveAuthKeychainItems(t.Context(), "/tmp/cfg", "operator"), "status 51")

	want := errors.New("tool missing")
	authKeychainTool = func(context.Context, []string) (int, error) { return 0, want }
	require.ErrorIs(t, RemoveAuthKeychainItems(t.Context(), "/tmp/cfg", "operator"), want)
}

func TestAuthKeychainToolReportsTheRealPlatformExitStatus(t *testing.T) {
	// The real tool answers "absent" for an item nothing ever wrote, which is
	// what keeps disconnect idempotent on a home that holds no credential.
	code, err := authKeychainTool(t.Context(), []string{
		"delete-generic-password",
		"-s", "acp-go-claude-canary-service-that-does-not-exist",
		"-a", "acp-go-claude-canary-account",
	})
	require.NoError(t, err)
	require.True(t, authKeychainAbsent(code), "unexpected status %d", code)
}

func TestAuthKeychainToolSeparatesSuccessFromALaunchFailure(t *testing.T) {
	code, err := authKeychainTool(t.Context(), []string{"list-keychains"})
	require.NoError(t, err)
	require.Zero(t, code)

	ctx, cancel := context.WithCancel(t.Context())
	cancel()

	_, err = authKeychainTool(ctx, []string{"list-keychains"})
	require.Error(t, err)
}
