//go:build windows

package claude

import "os"

func ordinaryNativeResult(state *os.ProcessState, revoked bool) NativeResult {
	return NativeResult{ExitCode: state.ExitCode(), Revoked: revoked}
}
