//go:build !windows

package claudeacp

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// requireProjectKeyTruncationBoundary pins the 200-unit truncation boundary in
// the POSIX spelling: a root of one separator, so forty "/deep" segments land
// exactly on the limit and forty-one carry it over.
func requireProjectKeyTruncationBoundary(t *testing.T) {
	t.Helper()

	atLimit, err := projectKeyForDirectory(strings.Repeat("/deep", 40))
	require.NoError(t, err)
	require.Equal(t, strings.Repeat("-deep", 40), atLimit)

	overLimit, err := projectKeyForDirectory(strings.Repeat("/deep", 41))
	require.NoError(t, err)
	require.Equal(t, strings.Repeat("-deep", 40)+"-lgqv39", overLimit)
}
