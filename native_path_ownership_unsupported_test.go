//go:build !linux

package claudeacp

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNativePathOwnershipOnUnsupportedPlatforms(t *testing.T) {
	require.NoError(t, handoffGeneratedNativeTree("", nil))
	require.NoError(t, validateNativeOwnedDirectory("", nil))

	current := &ProcessIsolation{UID: uint32(os.Geteuid()), GID: uint32(os.Getegid())}
	require.NoError(t, handoffGeneratedNativeTree("", current))
	require.NoError(t, validateNativeOwnedDirectory("", current))

	foreign := &ProcessIsolation{UID: current.UID + 1, GID: current.GID + 1}
	require.ErrorContains(t, handoffGeneratedNativeTree("", foreign), "unsupported")
	require.ErrorContains(t, validateNativeOwnedDirectory("", foreign), "unsupported")
}
