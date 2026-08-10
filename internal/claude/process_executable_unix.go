//go:build !windows

package claude

import "os/exec"

func ordinaryExecutableCandidate(path string, _ []string) (string, error) {
	return exec.LookPath(path)
}

func ordinaryEnvironmentValue(environment []string, key string) string {
	return environmentMap(environment)[key]
}

func launchEnvironmentKey(key string) string {
	return key
}
