//go:build !windows

package claudeacp

import (
	"errors"
	"fmt"
)

// syncAuthLedgerDirectory flushes the ledger directory entry, so a rename this
// write has already returned from survives a crash rather than only a clean
// process exit.
func syncAuthLedgerDirectory(path string) error {
	dir, err := ledgerOpen(path)
	if err != nil {
		return fmt.Errorf("open provider auth ledger root: %w", err)
	}

	return errors.Join(dir.Sync(), dir.Close())
}
