//go:build windows

package claude

import "testing"

// requireNativeStartReportsALostWorkingDirectory is a no-op on Windows. The
// branch it drives elsewhere needs the process's own working directory to be
// deleted out from under it, and Windows holds that directory open for as long
// as a process sits in it, so the condition cannot be produced here at all.
func requireNativeStartReportsALostWorkingDirectory(*testing.T) {}
