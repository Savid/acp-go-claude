package claudeacp

import (
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/savid/acp-go-claude/internal/claude"
)

// captureAmbientEnvironment is the seam the adapter's own environment is read
// through. Tests select a fixed environment rather than mutating the process's.
var captureAmbientEnvironment = os.Environ

func captureOrdinaryEnvironment(options Options) map[string]string {
	if options.hostAuthoritySet {
		return nil
	}

	return claude.OrdinaryEnvironment(ambientEnvironmentEntries(options))
}

// ambientEnvironmentEntries is the ordered block ordinary execution inherits
// from: the adapter's own process environment, or the one
// WithAmbientEnvironment supplied in its place, folded in sorted key order.
func ambientEnvironmentEntries(options Options) []string {
	if options.AmbientEnvironment == nil {
		return captureAmbientEnvironment()
	}

	keys := slices.Sorted(maps.Keys(options.AmbientEnvironment))
	entries := make([]string, 0, len(keys))

	for _, key := range keys {
		entries = append(entries, key+"="+options.AmbientEnvironment[key])
	}

	return entries
}

// validateAmbientEnvironment refuses a supplied block whose entries could not
// be environment entries at all. Which names the block then contributes is
// decided by the ordinary inheritance rules, never here.
func validateAmbientEnvironment(env map[string]string) error {
	for _, key := range slices.Sorted(maps.Keys(env)) {
		switch {
		case key == "" || strings.ContainsAny(key, "=\x00"):
			return fmt.Errorf("ambient environment key %q is not a variable name", key)
		case strings.ContainsRune(env[key], '\x00'):
			return fmt.Errorf("ambient environment value for %q contains NUL", key)
		}
	}

	return nil
}

func (a *Agent) ordinaryEnvironment() map[string]string {
	return cloneStringMap(a.ordinaryEnv)
}

// claudeConfigDir is the directory Claude reads and writes for a session:
// claudeHome when WithHome names one, otherwise ".claude" under the home of
// the environment the native process inherits. The adapter's own settings,
// transcript, and permission paths follow the child, never the adapter's
// account. "" means the base environment names no home.
func (a *Agent) claudeConfigDir(claudeHome string) string {
	if trimmed := strings.TrimSpace(claudeHome); trimmed != "" {
		return filepath.Clean(trimmed)
	}

	env := a.effectiveNativeEnvironment(nil)
	for _, name := range []string{homeEnv, "USERPROFILE"} {
		if home := strings.TrimSpace(env[claude.EnvironmentKey(name)]); home != "" {
			return filepath.Join(home, settingsDirName)
		}
	}

	return ""
}
