package claudeacp

import (
	"fmt"
	"maps"
	"slices"
	"strings"

	"github.com/coder/acp-go-sdk"

	"github.com/savid/acp-go-claude/internal/claude"
)

const (
	valAmbiguous = "ambiguous"

	envPathKey        = "PATH"
	envNodeOptionsKey = "NODE_OPTIONS"
	envBashEnvKey     = "BASH_ENV"
	envShellEnvKey    = "ENV"
	envClaudeCodeKey  = "CLAUDECODE"
)

func validEnvName(key string) bool {
	return key != "" && !strings.ContainsAny(key, "=\x00")
}

// blockedAgentEnvKey reports whether a caller-supplied env key names a
// variable the adapter refuses on every surface. The private adapter
// namespace is refused under every spelling. Every other name is one the
// native process, its loader, or its shell reads under an exact platform
// spelling, so the comparison goes through the platform identity: on Unix
// `path` and `env` are variables of the host's own.
func blockedAgentEnvKey(key string) bool {
	if privateAdapterEnvName(key) || managedClaudeRootEnvKey(key) {
		return true
	}

	switch name := claude.EnvironmentKey(key); name {
	case envNodeOptionsKey, envBashEnvKey, envShellEnvKey, envClaudeCodeKey:
		return true
	default:
		return strings.HasPrefix(name, "LD_") || strings.HasPrefix(name, "DYLD_")
	}
}

// blockedClaudeEnvKey additionally refuses PATH in a session env: the ordered
// extraPathDirs option is the only session-scoped search-path authority.
func blockedClaudeEnvKey(key string) bool {
	return blockedAgentEnvKey(key) || claude.EnvironmentKey(key) == envPathKey
}

// validateAgentEnv applies the session name rule to the static Agent-scoped
// environment, with PATH allowed because that surface establishes the native
// base search path. A refusal fails Agent construction.
func validateAgentEnv(env map[string]string) error {
	seen := make(map[string]string, len(env))

	for _, key := range slices.Sorted(maps.Keys(env)) {
		if !validEnvName(key) || strings.ContainsRune(env[key], '\x00') {
			return fmt.Errorf("environment key %q is not a variable name", key)
		}

		if blockedAgentEnvKey(key) {
			return fmt.Errorf("environment key %q is reserved", key)
		}

		identity := claude.EnvironmentKey(key)
		if previous, duplicate := seen[identity]; duplicate {
			return fmt.Errorf("environment keys %q and %q name the same variable", previous, key)
		}

		seen[identity] = key
	}

	return nil
}

// validateSessionEnv checks a session environment in sorted key order, so the
// first refusal is the same on every call. A key that cannot be a variable
// name, a value carrying a NUL, and a blocked name each fail as unsupported at
// the key exactly as the host sent it. Two keys that name one variable under
// the platform identity fail as ambiguous at the later key: a Go map carries
// no order, so the value such a map would deliver is unknowable.
func validateSessionEnv(env map[string]string, path string) error {
	seen := make(map[string]struct{}, len(env))

	for _, key := range slices.Sorted(maps.Keys(env)) {
		if !validEnvName(key) || strings.ContainsRune(env[key], '\x00') || blockedClaudeEnvKey(key) {
			return unsupportedField(path + "." + key)
		}

		identity := claude.EnvironmentKey(key)
		if _, duplicate := seen[identity]; duplicate {
			return ambiguousField(path + "." + key)
		}

		seen[identity] = struct{}{}
	}

	return nil
}

func ambiguousField(path string) error {
	return acp.NewInvalidParams(map[string]any{
		jsonFieldError: valAmbiguous,
		jsonFieldField: path,
	})
}

// ValidateClaudeSessionMeta reports the refusal a session/new, session/load,
// session/resume, or fork request carrying meta receives from this package's
// _meta.claude parsing, or nil when the vendor namespace is accepted. A
// provider-auth binding is validated structurally here; whether an agent
// admits one at all is decided by its own configuration.
func ValidateClaudeSessionMeta(meta map[string]any) error {
	_, err := claudeOptionsFromMetaWithProviderAuth(meta, true)

	return err
}
