//go:build windows

package claudeacp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestSyncAuthLedgerDirectoryWindows pins the documented Windows behaviour:
// FlushFileBuffers refuses a directory handle opened through os.Open, so the
// ledger commit never opens its root and a fault planted on that open cannot
// reach a write. The entry itself is still readable back, which is the
// durability the commit promises here.
func TestSyncAuthLedgerDirectoryWindows(t *testing.T) {
	original := ledgerOpen
	ledgerOpen = func(string) (*os.File, error) { return nil, errTestRandom }

	t.Cleanup(func() { ledgerOpen = original })

	require.NoError(t, syncAuthLedgerDirectory(t.TempDir()))

	root := filepath.Join(t.TempDir(), "root")
	require.NoError(t, os.Mkdir(root, 0o700))

	ledger, err := newAuthLedger(Options{ProviderAuthRoot: root, Home: t.TempDir()})
	require.NoError(t, err)

	record := authLedgerRecord{ProviderID: "anthropic"}
	require.NoError(t, ledger.write(record))

	stored, found, err := ledger.read("anthropic")
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, record, stored)
}
