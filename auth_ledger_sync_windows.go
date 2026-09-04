//go:build windows

package claudeacp

// syncAuthLedgerDirectory is a no-op on Windows. FlushFileBuffers rejects a
// directory handle opened through os.Open, so asking for one fails every ledger
// write with "Access is denied" and takes the whole provider-auth surface with
// it. What stays durable without it is the entry itself: the temporary file is
// written, chmodded, and fsynced before the rename that publishes it, and NTFS
// journals the rename as metadata, so a ledger entry that a completed write
// reported is either wholly present or wholly absent after a crash.
func syncAuthLedgerDirectory(string) error { return nil }
