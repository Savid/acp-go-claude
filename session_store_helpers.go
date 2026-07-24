package claudeacp

import (
	"fmt"
	"path/filepath"
	"strings"
)

func (a *Agent) sessionStore() SessionStore {
	if a.options.SessionStore != nil {
		return a.options.SessionStore
	}

	return a.store
}

func validUUIDShape(value string) bool {
	if len(value) != 36 {
		return false
	}

	for i, char := range value {
		switch i {
		case 8, 13, 18, 23:
			if char != '-' {
				return false
			}
		default:
			if !isUUIDHex(char) {
				return false
			}
		}
	}

	return true
}

func isUUIDHex(char rune) bool {
	return (char >= '0' && char <= '9') ||
		(char >= 'a' && char <= 'f') ||
		(char >= 'A' && char <= 'F')
}

func isSafeSessionSubpath(subpath string) bool {
	if subpath == "" ||
		filepath.IsAbs(subpath) ||
		strings.HasPrefix(subpath, "/") ||
		strings.HasPrefix(subpath, "\\") ||
		strings.Contains(subpath, "\x00") ||
		filepath.VolumeName(subpath) != "" {
		return false
	}

	for _, part := range strings.FieldsFunc(subpath, func(r rune) bool { return r == '/' || r == '\\' }) {
		if part == "" || part == "." || part == parentDirSegment || strings.Contains(part, ":") {
			return false
		}
	}

	return true
}

func projectKeyForDirectory(cwd string) (string, error) {
	if strings.TrimSpace(cwd) == "" {
		return "", fmt.Errorf("cwd is required")
	}

	if !filepath.IsAbs(cwd) {
		return "", fmt.Errorf("cwd must be an absolute path")
	}

	absolute := filepath.Clean(cwd)
	if resolved, err := filepath.EvalSymlinks(absolute); err == nil {
		absolute = resolved
	}

	return sanitizeSessionProjectPath(filepath.Clean(absolute)), nil
}

func sanitizeSessionProjectPath(path string) string {
	var builder strings.Builder

	for _, char := range path {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') {
			builder.WriteRune(char)
		} else {
			builder.WriteByte('-')
		}
	}

	if builder.Len() == 0 {
		return "-"
	}

	return builder.String()
}
