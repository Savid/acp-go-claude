//go:build windows

package claudeacp

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// requireProjectKeyTruncationBoundary pins the 200-unit truncation boundary in
// the Windows spelling: the drive and its separator sanitize to three units of
// their own, so the segments that reach the limit are counted from there.
func requireProjectKeyTruncationBoundary(t *testing.T) {
	t.Helper()

	atLimit, err := projectKeyForDirectory(`C:\de` + strings.Repeat(`\deep`, 39))
	require.NoError(t, err)
	require.Equal(t, "C--de"+strings.Repeat("-deep", 39), atLimit)

	overLimit, err := projectKeyForDirectory(`C:\dee` + strings.Repeat(`\deep`, 39))
	require.NoError(t, err)
	require.Equal(t, ("C--dee" + strings.Repeat("-deep", 39))[:200]+"-1ow2jt", overLimit)
}
