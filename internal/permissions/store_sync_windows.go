//go:build windows

package permissions

// syncPermissionsDir is a no-op on Windows. FlushFileBuffers rejects a
// directory handle opened through os.Open, so asking for one fails every rule
// write with "Access is denied" and session permission rules never persist.
// What stays durable without it is the file itself: the temporary file is
// written and fsynced before the rename that publishes it, and NTFS journals
// the rename as metadata, so the rules file a completed Save reported is either
// wholly the old content or wholly the new one after a crash.
func syncPermissionsDir(string) error { return nil }
