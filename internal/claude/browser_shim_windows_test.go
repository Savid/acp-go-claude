//go:build windows

package claude

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNewBrowserShimRefusesToClaimContainment(t *testing.T) {
	t.Parallel()

	shim, err := newBrowserShim(t.TempDir())
	require.Nil(t, shim)
	require.ErrorIs(t, err, errBrowserShimUnsupported)
}
