//go:build windows

package claude

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestStartAuthLoginRefusesBeforeReachingTheAuthority states the Windows half of
// what auth_login_unix_test.go proves elsewhere. A browser launch cannot be
// neutralised on this platform, and a tab that reaches the login's loopback
// listener completes the grant outright, so the login is refused before the
// authority is consulted at all: no tree is prepared, no native process starts,
// and the scratch parent is left exactly as it was found.
func TestStartAuthLoginRefusesBeforeReachingTheAuthority(t *testing.T) {
	t.Parallel()

	scratch := t.TempDir()

	var consulted []string

	authority := &NativeAuthority{
		NativeEnvironment: func() map[string]string {
			consulted = append(consulted, "environment")

			return map[string]string{"PATH": `C:\Windows\System32`}
		},
		PrepareNativeTree: func(context.Context, string) error {
			consulted = append(consulted, "prepare")

			return nil
		},
		ReclaimNativeTree: func(context.Context, string) error {
			consulted = append(consulted, "reclaim")

			return nil
		},
		StartNative: func(context.Context, NativeRequest) (NativeProcess, error) {
			consulted = append(consulted, "start")

			return nil, errors.New("unexpected start")
		},
	}

	options := Options{CLIPath: "claude", ClaudeHome: t.TempDir(), ScratchParent: scratch, Authority: authority}

	login, authorizeURL, err := StartAuthLogin(t.Context(), options)
	require.ErrorIs(t, err, errBrowserShimUnsupported)
	require.ErrorContains(t, err, "contain claude auth login browser launch")
	require.Nil(t, login)
	require.Empty(t, authorizeURL)

	child, childErr := startAuthLoginChild(t.Context(), options)
	require.ErrorIs(t, childErr, errBrowserShimUnsupported)
	require.Nil(t, child)

	require.Empty(t, consulted, "a refused containment never reaches the native authority")

	entries, readErr := os.ReadDir(scratch)
	require.NoError(t, readErr)
	require.Empty(t, entries, "a refused containment leaves nothing under the scratch parent")
}
