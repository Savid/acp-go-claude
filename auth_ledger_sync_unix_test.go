//go:build !windows

package claudeacp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSyncAuthLedgerDirectoryUnix proves the POSIX flush both reaches a real
// directory and reports a root it cannot open, which is the branch the ledger
// write turns into an authCauseProcess failure.
func TestSyncAuthLedgerDirectoryUnix(t *testing.T) {
	root := t.TempDir()
	require.NoError(t, syncAuthLedgerDirectory(root))

	original := ledgerOpen
	ledgerOpen = func(string) (*os.File, error) { return nil, errTestRandom }

	t.Cleanup(func() { ledgerOpen = original })

	require.ErrorIs(t, syncAuthLedgerDirectory(filepath.Join(root, "missing")), errTestRandom)
}
