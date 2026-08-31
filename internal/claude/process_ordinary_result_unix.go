//go:build !windows

package claude

import (
	"os"
	"syscall"
)

func ordinaryNativeResult(state *os.ProcessState, revoked bool) NativeResult {
	result := NativeResult{ExitCode: state.ExitCode(), Revoked: revoked}
	if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		result.Signal = int(status.Signal())
	}

	return result
}
