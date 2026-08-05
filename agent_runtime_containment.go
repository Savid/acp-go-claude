package claudeacp

import (
	"errors"
	"fmt"
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

func containmentMode(options Options) RuntimeContainmentMode {
	if options.DarwinBestEffortContainment && runtimeGOOS != platformDarwin {
		return RuntimeContainmentUnavailable
	}

	switch runtimeGOOS {
	case platformLinux:
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
