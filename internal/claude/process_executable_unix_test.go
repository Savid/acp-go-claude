//go:build !windows

package claude

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// TestEnvironmentKeyIsExactOnUnix pins the identity half of the platform seam.
// A Unix environment is case-sensitive, so PATH and path are two variables and
// nothing built on this seam may fold one into the other; the Windows half is
// pinned beside its own implementation.
func TestEnvironmentKeyIsExactOnUnix(t *testing.T) {
	t.Parallel()

	require.Equal(t, envSearchPath, EnvironmentKey(envSearchPath))
	require.Equal(t, "path", EnvironmentKey("path"))
	require.NotEqual(t, EnvironmentKey(envSearchPath), EnvironmentKey("path"))
}
