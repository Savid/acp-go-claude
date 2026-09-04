//go:build !windows

package claude

import "os/exec"

func ordinaryExecutableCandidate(path string, _ []string) (string, error) {
	return exec.LookPath(path)
}
