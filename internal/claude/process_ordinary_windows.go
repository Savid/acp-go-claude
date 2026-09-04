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
	extensions := ordinaryWindowsExecutableExtensions(ordinaryEnvironmentValue(environment, "PATHEXT"))
	if ordinaryWindowsHasExtension(path) {
		if err := ordinaryWindowsExecutableFile(path); err == nil {
			return path, nil
		}
	}

	for _, extension := range extensions {
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

	extensions := make([]string, 0, 4)
	for extension := range strings.SplitSeq(value, ";") {
		extension = strings.ToLower(strings.TrimSpace(extension))
		if extension == "" {
			continue
		}
		if extension[0] != '.' {
			extension = "." + extension
		}
		extensions = append(extensions, extension)
	}

	return extensions
}

func ordinaryWindowsHasExtension(path string) bool {
	index := strings.LastIndexByte(path, '.')
	return index >= 0 && strings.LastIndexAny(path, `:\/`) < index
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
