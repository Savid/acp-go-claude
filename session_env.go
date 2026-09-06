package claudeacp

import (
	"maps"
	"slices"
	"strings"

	"github.com/coder/acp-go-sdk"
)

const (
	validationAmbiguous = "ambiguous"

	envPathKey        = "PATH"
	envNodeOptionsKey = "NODE_OPTIONS"
	envBashEnvKey     = "BASH_ENV"
	envShellEnvKey    = "ENV"
	envClaudeCodeKey  = "CLAUDECODE"
)

// sessionEnvIdentity is the name the target platform resolves an environment
// key by: the exact bytes on Unix, where PATH and path are two variables, and
// the upper-cased spelling on Windows, where they are one.
func sessionEnvIdentity(key string) string {
	if runtimeGOOS == platformWindows {
		return strings.ToUpper(key)
	}

	return key
}

func validEnvName(key string) bool {
	return key != "" && !strings.ContainsAny(key, "=\x00")
}

// blockedClaudeEnvKey reports whether a host-supplied session env key names a
// variable the adapter refuses to forward. The private adapter namespace is
// refused under every spelling. Every other name is one the native process,
// its loader, or its shell reads under an exact platform spelling, so the
// comparison goes through the platform identity: on Unix `path` and `env` are
// variables of the host's own.
func blockedClaudeEnvKey(key string) bool {
	if privateAdapterEnvName(key) || managedClaudeRootEnvKey(key) {
		return true
	}

	switch name := sessionEnvIdentity(key); name {
	case envPathKey, envNodeOptionsKey, envBashEnvKey, envShellEnvKey, envClaudeCodeKey:
		return true
	default:
		return strings.HasPrefix(name, "LD_") || strings.HasPrefix(name, "DYLD_")
	}
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

		identity := sessionEnvIdentity(key)
		if _, duplicate := seen[identity]; duplicate {
			return ambiguousField(path + "." + key)
		}

		seen[identity] = struct{}{}
	}

	return nil
}

func ambiguousField(path string) error {
	return acp.NewInvalidParams(map[string]any{
		jsonFieldError: validationAmbiguous,
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
