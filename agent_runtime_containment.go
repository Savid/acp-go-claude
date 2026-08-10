package claudeacp

import (
	"errors"
	"fmt"
	"os"
	"strings"
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

// containmentEffectiveUID is the seam the shared-identity report is derived
// through. The mode is selected from a faked GOOS in tests, so the identity it
// is compared against has to be selectable there too.
var containmentEffectiveUID = os.Geteuid

// sharedProcessIdentity reports whether the configured native identity is the
// identity this process already runs as. Root never qualifies: a zero effective
// uid is the trusted supervisor identity, and the native uid is required to be
// nonzero.
func sharedProcessIdentity(isolation *ProcessIsolation) bool {
	if isolation == nil {
		return false
	}

	effectiveUID := containmentEffectiveUID()

	return effectiveUID > 0 && uint64(isolation.UID) == uint64(effectiveUID)
}

// provesWholeTreeLifecycle reports whether the selected boundary can prove that
// every process it started has exited. Both linux boundaries can: they differ
// in whether the agent runs under its own credentials, not in what the
// subreaper observes.
func (mode RuntimeContainmentMode) provesWholeTreeLifecycle() bool {
	return mode == RuntimeContainmentAuthoritative || mode == RuntimeContainmentSharedIdentity
}

func containmentMode(options Options) RuntimeContainmentMode {
	if options.DarwinBestEffortContainment && runtimeGOOS != platformDarwin {
		return RuntimeContainmentUnavailable
	}

	switch runtimeGOOS {
	case platformLinux:
		// Omission launches the native tree as the identity this process
		// already runs as, so shared identity is the only truthful report; an
		// authoritative claim belongs solely to an explicit distinct identity.
		if options.ProcessIsolation == nil || sharedProcessIdentity(options.ProcessIsolation) {
			return RuntimeContainmentSharedIdentity
		}

		return RuntimeContainmentAuthoritative
	case platformDarwin:
		if options.DarwinBestEffortContainment {
			return RuntimeContainmentBestEffort
		}
	}

	return RuntimeContainmentUnavailable
}

func validateContainmentOptions(options Options) error {
	if options.DarwinBestEffortContainment && runtimeGOOS != platformDarwin {
		return errors.New("darwin best-effort containment is supported only on darwin")
	}

	for key := range options.Env {
		if strings.HasPrefix(strings.ToUpper(key), privateAdapterEnvPrefix) {
			return fmt.Errorf("environment key %q uses the reserved %s prefix", key, privateAdapterEnvPrefix)
		}

		if managedClaudeRootEnvKey(key) {
			return fmt.Errorf("environment key %q is managed by the process isolation policy", key)
		}
	}

	return nil
}

func managedClaudeRootEnvKey(key string) bool {
	switch strings.ToUpper(key) {
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
