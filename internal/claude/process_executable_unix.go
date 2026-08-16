//go:build !windows

package claude

import "os/exec"

func ordinaryExecutableCandidate(path string, _ []string) (string, error) {
	return exec.LookPath(path)
}

func ordinaryEnvironmentValue(environment []string, key string) string {
	return environmentMap(environment)[key]
}

// EnvironmentKey normalizes an environment variable name for identity
// comparison. Unix environments are case-sensitive, so FOO and foo name two
// different variables and the name is its own identity.
func EnvironmentKey(key string) string {
	return key
}
