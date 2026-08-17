package claudeacp

import (
	"path/filepath"
	"strconv"
)

func validateRequiredAbsolutePath(field string, path string) error {
	if !filepath.IsAbs(path) {
		return unsupportedField(field)
	}

	return nil
}

func validateOptionalAbsolutePath(field string, path *string) error {
	if path == nil || *path == "" {
		return nil
	}

	return validateRequiredAbsolutePath(field, *path)
}

func validateAbsolutePaths(field string, paths []string) error {
	for index, path := range paths {
		if !filepath.IsAbs(path) {
			return unsupportedField(field + "[" + strconv.Itoa(index) + "]")
		}
	}

	return nil
}

func validateSessionStartPaths(cwd string, additionalDirectories []string) error {
	if err := validateRequiredAbsolutePath(jsonFieldCwd, cwd); err != nil {
		return err
	}

	return validateAbsolutePaths(metaAdditionalDirectoriesKey, additionalDirectories)
}
