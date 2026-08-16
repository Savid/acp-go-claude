package claudeacp

import (
	"errors"
	"fmt"
	"strings"

	"github.com/savid/acp-go-claude/internal/claude"
)

const (
	claudeConfigDirEnv = "CLAUDE_CONFIG_DIR"
	homeEnv            = "HOME"
	xdgConfigHomeEnv   = "XDG_CONFIG_HOME"
)

// ContainmentMode reports the effective platform process boundary.
func (a *Agent) ContainmentMode() RuntimeContainmentMode {
	if a == nil {
		return RuntimeContainmentUnavailable
	}

	return a.containmentMode
}

// provesWholeTreeLifecycle reports whether the selected boundary can prove that
// every process it started has exited. Only the hardened Linux boundary can:
// its trusted-root guardian owns a subreaper tree it can enumerate. Ordinary
// shared identity and Darwin best effort complete a directly owned boundary and
// deliberately claim nothing beyond it.
func (mode RuntimeContainmentMode) provesWholeTreeLifecycle() bool {
	return mode == RuntimeContainmentAuthoritative
}

// containmentOptionsConflict reports whether the two explicit containment
// options were combined, or the Darwin one was selected off Darwin. An explicit
// hardened identity policy cannot be downgraded to best effort, so the pair is
// refused before any native construction rather than resolved.
func containmentOptionsConflict(options Options) bool {
	if !options.DarwinBestEffortContainment {
		return false
	}

	return runtimeGOOS != platformDarwin || options.ProcessIsolation != nil
}

func containmentMode(options Options) RuntimeContainmentMode {
	if containmentOptionsConflict(options) {
		return RuntimeContainmentUnavailable
	}

	switch runtimeGOOS {
	case platformLinux:
		if options.ProcessIsolation != nil {
			return RuntimeContainmentAuthoritative
		}
	case platformDarwin:
		if options.DarwinBestEffortContainment {
			return RuntimeContainmentBestEffort
		}
	}

	// A supplied policy is a strict Linux selection: everywhere else it fails
	// closed rather than degrading. Omission is the ordinary default and runs
	// as this process's own identity on every platform the adapter supports.
	if options.ProcessIsolation != nil {
		return RuntimeContainmentUnavailable
	}

	return RuntimeContainmentSharedIdentity
}

func validateContainmentOptions(options Options) error {
	if options.DarwinBestEffortContainment && runtimeGOOS != platformDarwin {
		return errors.New("darwin best-effort containment is supported only on darwin")
	}

	if options.DarwinBestEffortContainment && options.ProcessIsolation != nil {
		return errors.New("darwin best-effort containment cannot be combined with WithProcessIsolation")
	}

	for key := range options.Env {
		if privateAdapterEnvName(key) {
			return fmt.Errorf("environment key %q uses the reserved %s prefix", key, privateAdapterEnvPrefix)
		}

		if managedClaudeRootEnvKey(key) {
			return fmt.Errorf("environment key %q is managed by the process isolation policy", key)
		}
	}

	return nil
}

// privateAdapterEnvName reports whether key falls inside the adapter's own
// reserved environment namespace. The test folds case on every platform,
// unlike claude.EnvironmentKey: the namespace is reserved by name rather than
// by variable identity, so no spelling of it is a host's to set.
func privateAdapterEnvName(key string) bool {
	return strings.HasPrefix(strings.ToUpper(key), privateAdapterEnvPrefix)
}

// managedClaudeRootEnvKey reports whether key names a root the isolation policy
// owns. This is a variable-identity question — the native process reads these
// by their exact platform names — so it goes through the platform seam rather
// than folding case everywhere.
func managedClaudeRootEnvKey(key string) bool {
	switch claude.EnvironmentKey(key) {
	case claudeConfigDirEnv, homeEnv,
		"XDG_CACHE_HOME", xdgConfigHomeEnv, "XDG_DATA_HOME", "XDG_RUNTIME_DIR", "XDG_STATE_HOME":
		return true
	default:
		return false
	}
}

func normalizeStandaloneHome(options *Options) error {
	if options == nil || options.ProcessIsolation == nil {
		return nil
	}

	isolation := options.ProcessIsolation
	if isolation.IdentityLock != nil || isolation.AuthorityDomain != nil || isolation.StandaloneStateRoot == "" {
		return nil
	}

	if options.Home == "" {
		options.Home = isolation.StandaloneStateRoot

		return nil
	}

	if options.Home != isolation.StandaloneStateRoot {
		return fmt.Errorf("WithHome must equal ProcessIsolation.StandaloneStateRoot %q", isolation.StandaloneStateRoot)
	}

	return nil
}
