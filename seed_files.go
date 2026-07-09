package claudeacp

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

const (
	seedFilesOptionField = "seedFiles"
	parentDirSegment     = ".."

	// seedManifestFileName tracks the relative paths the wrapper owns inside a
	// seed root so seed writes never clobber an operator-authored file.
	seedManifestFileName = ".seed-manifest.json"
	// seedBackupSuffix names the sidecar copy kept when a managed seed file's
	// contents change.
	seedBackupSuffix = ".seed.bak"
)

// seedTarget is one resolved seed write with its precomputed disk state.
type seedTarget struct {
	name   string
	path   string
	exists bool
}

// writeSeedFiles writes each configured seed file into the resolved Claude
// config directory before the Claude CLI launches, so the launched CLI reads
// them as its own config. Keys are paths relative to dir and values are written
// verbatim. Absolute paths, ".." escapes, and empty keys fail closed with the
// uniform unsupported error. Callers must only invoke this with an
// explicitly-configured Claude config directory; an empty dir also fails closed
// rather than writing to an unexpected location such as the default ~/.claude.
//
// Writes are guarded by an ownership manifest (seedManifestFileName) so the
// wrapper never overwrites a file it did not create: a first write records the
// relpath in the manifest; a subsequent write of a managed file keeps a
// seedBackupSuffix copy of the prior bytes when they change; a pre-existing
// unmanaged target fails closed, leaving all files untouched.
func writeSeedFiles(dir string, files map[string]string) error {
	if len(files) == 0 {
		return nil
	}

	if strings.TrimSpace(dir) == "" {
		return seedFilePathError("")
	}

	names := make([]string, 0, len(files))
	for name := range files {
		names = append(names, name)
	}

	slices.Sort(names)

	targets := make([]seedTarget, 0, len(names))
	for _, name := range names {
		target, err := resolveSeedFilePath(dir, name)
		if err != nil {
			return err
		}

		exists, err := seedTargetExists(target)
		if err != nil {
			return err
		}

		targets = append(targets, seedTarget{name: name, path: target, exists: exists})
	}

	manifest, err := loadSeedManifest(dir)
	if err != nil {
		return err
	}

	// Fail closed on any pre-existing unmanaged target before writing anything,
	// so a rejected seed leaves every file on disk untouched.
	for _, target := range targets {
		if !target.exists {
			continue
		}

		if _, managed := manifest[seedManifestKey(target.name)]; !managed {
			return seedFilePathError(target.name)
		}
	}

	added := false

	for _, target := range targets {
		newBytes := []byte(files[target.name])

		if target.exists {
			// Managed target (guaranteed by the fail-closed scan above): back up
			// changed bytes, skip identical ones.
			current, readErr := materializeReadFile(target.path)
			if readErr != nil {
				return fmt.Errorf("read managed seed file: %w", readErr)
			}

			if bytes.Equal(current, newBytes) {
				continue
			}

			if backupErr := materializeWriteFile(target.path+seedBackupSuffix, current, 0o600); backupErr != nil {
				return fmt.Errorf("back up managed seed file: %w", backupErr)
			}
		}

		if err := materializeMkdirAll(filepath.Dir(target.path), 0o700); err != nil {
			return fmt.Errorf("create seed file directory: %w", err)
		}

		if err := materializeWriteFile(target.path, newBytes, 0o600); err != nil {
			return fmt.Errorf("write seed file: %w", err)
		}

		if _, managed := manifest[seedManifestKey(target.name)]; !managed {
			manifest[seedManifestKey(target.name)] = struct{}{}
			added = true
		}
	}

	if added {
		if err := writeSeedManifest(dir, manifest); err != nil {
			return err
		}
	}

	return nil
}

// prepareSeededClaudeConfig writes the configured seed files into the effective
// Claude config dir and resolves the optional settings-file overlay to an
// absolute --settings path. Both require an explicit Home so the adapter never
// writes into or resolves against the operator's default ~/.claude: when Home is
// unset (claudeHome == "") either option fails closed. It returns the absolute
// settings-file path, empty when no overlay is configured.
func (a *Agent) prepareSeededClaudeConfig(claudeHome string, processClaudeHome string) (string, error) {
	if len(a.options.SeedFiles) > 0 {
		if claudeHome == "" {
			return "", seedFilePathError("")
		}

		if err := writeSeedFiles(processClaudeHome, a.options.SeedFiles); err != nil {
			return "", err
		}
	}

	if a.options.SettingsFile == "" {
		return "", nil
	}

	if claudeHome == "" {
		return "", settingsFileError("")
	}

	return resolveClaudeSettingsFile(processClaudeHome, a.options.SettingsFile)
}

func seedTargetExists(path string) (bool, error) {
	if _, err := materializeStat(path); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}

		return false, fmt.Errorf("stat seed file: %w", err)
	}

	return true, nil
}

func seedManifestKey(name string) string {
	return filepath.ToSlash(name)
}

func loadSeedManifest(dir string) (map[string]struct{}, error) {
	data, err := materializeReadFile(filepath.Join(dir, seedManifestFileName))
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return make(map[string]struct{}), nil
		}

		return nil, fmt.Errorf("read seed manifest: %w", err)
	}

	var entries []string
	if err := json.Unmarshal(data, &entries); err != nil {
		return nil, fmt.Errorf("decode seed manifest: %w", err)
	}

	manifest := make(map[string]struct{}, len(entries))
	for _, entry := range entries {
		manifest[entry] = struct{}{}
	}

	return manifest, nil
}

func writeSeedManifest(dir string, manifest map[string]struct{}) error {
	entries := make([]string, 0, len(manifest))
	for entry := range manifest {
		entries = append(entries, entry)
	}

	slices.Sort(entries)

	// entries is a []string and cannot fail to marshal.
	data, _ := json.Marshal(entries)

	if err := materializeWriteFile(filepath.Join(dir, seedManifestFileName), data, 0o600); err != nil {
		return fmt.Errorf("write seed manifest: %w", err)
	}

	return nil
}

func resolveSeedFilePath(dir string, name string) (string, error) {
	if !validSeedFilePath(name) {
		return "", seedFilePathError(name)
	}

	return filepath.Join(dir, filepath.FromSlash(name)), nil
}

func validSeedFilePath(name string) bool {
	if name == "" ||
		filepath.IsAbs(name) ||
		strings.HasPrefix(name, "/") ||
		strings.HasPrefix(name, "\\") ||
		strings.Contains(name, "\x00") ||
		filepath.VolumeName(name) != "" {
		return false
	}

	for _, part := range strings.FieldsFunc(name, func(r rune) bool { return r == '/' || r == '\\' }) {
		if part == "" || part == "." || part == parentDirSegment || strings.Contains(part, ":") {
			return false
		}
	}

	return true
}

func seedFilePathError(name string) error {
	field := seedFilesOptionField
	if name != "" {
		field = fmt.Sprintf("%s[%q]", seedFilesOptionField, name)
	}

	return unsupportedField(field)
}
