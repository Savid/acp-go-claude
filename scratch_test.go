package claudeacp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestScratchParent(t *testing.T) {
	t.Parallel()

	require.Equal(t, os.TempDir(), scratchParent(""))
	require.Equal(t, "/custom/scratch", scratchParent("/custom/scratch"))
}

func TestEnsureScratchParent(t *testing.T) {
	t.Parallel()

	parent, err := ensureScratchParent("")
	require.NoError(t, err)
	require.Equal(t, os.TempDir(), parent)

	missing := filepath.Join(t.TempDir(), "nested", "scratch")
	parent, err = ensureScratchParent(missing)
	require.NoError(t, err)
	require.Equal(t, missing, parent)

	info, err := os.Stat(missing)
	require.NoError(t, err)
	require.True(t, info.IsDir())
	requirePrivateMode(t, 0o700, info)

	occupied := filepath.Join(t.TempDir(), "occupied")
	require.NoError(t, os.WriteFile(occupied, []byte("x"), 0o600))

	_, err = ensureScratchParent(occupied)
	require.ErrorContains(t, err, "create scratch parent dir")
}
