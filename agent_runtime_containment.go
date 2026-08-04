package claudeacp

import (
	"errors"
	"fmt"
	"strings"
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
	}

	return nil
}
