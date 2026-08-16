//go:build windows

package claude

import (
	"errors"
	"os"
	"os/exec"
	"strings"
)

const defaultWindowsExecutableExtensions = ".com;.exe;.bat;.cmd"

func ordinaryExecutableCandidate(path string, environment []string) (string, error) {
	exts := ordinaryWindowsExecutableExtensions(ordinaryEnvironmentValue(environment, "PATHEXT"))

	if ordinaryWindowsHasExtension(path) {
		if err := ordinaryWindowsExecutableFile(path); err == nil {
			return path, nil
		}
	}

	for _, extension := range exts {
		candidate := path + extension
		if err := ordinaryWindowsExecutableFile(candidate); err == nil {
			return candidate, nil
		}
	}

	return "", exec.ErrNotFound
}

func ordinaryWindowsExecutableExtensions(value string) []string {
	if value == "" {
		value = defaultWindowsExecutableExtensions
	}

	exts := make([]string, 0, 4)
	for extension := range strings.SplitSeq(value, ";") {
		extension = strings.ToLower(strings.TrimSpace(extension))
		if extension == "" {
			continue
		}

		if extension[0] != '.' {
			extension = "." + extension
		}

		exts = append(exts, extension)
	}

	return exts
}

func ordinaryWindowsHasExtension(path string) bool {
	index := strings.LastIndexByte(path, '.')
	if index < 0 {
		return false
	}

	return strings.LastIndexAny(path, `:\/`) < index
}

func ordinaryWindowsExecutableFile(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return err
	}

	if info.IsDir() {
		return errors.New("executable candidate is a directory")
	}

	return nil
}

func ordinaryEnvironmentValue(environment []string, key string) string {
	value := ""

	for _, entry := range environment {
		candidate, candidateValue, ok := strings.Cut(entry, "=")
		if ok && strings.EqualFold(candidate, key) {
			value = candidateValue
		}
	}

	return value
}

// EnvironmentKey normalizes an environment variable name for identity
// comparison. Windows environment names are case-insensitive, so FOO and foo
// name one variable and the upper-cased spelling is its identity.
func EnvironmentKey(key string) string {
	return strings.ToUpper(key)
}
