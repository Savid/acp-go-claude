//go:build !linux

package claude

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestGeneratedNativeTreeOwnershipOnUnsupportedPlatforms(t *testing.T) {
	require.NoError(t, handoffGeneratedNativeTree("", nil))
	require.NoError(t, handoffGeneratedNativeTree("", &ProcessIsolation{
		UID: uint32(os.Geteuid()), GID: uint32(os.Getegid()),
	}))
	require.ErrorContains(t, handoffGeneratedNativeTree("", &ProcessIsolation{
		UID: uint32(os.Geteuid() + 1), GID: uint32(os.Getegid() + 1),
	}), "unsupported")
}
