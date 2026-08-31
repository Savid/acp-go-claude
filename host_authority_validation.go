package claudeacp

import (
	"fmt"
	"strings"

	"github.com/savid/acp-go-claude/internal/claude"
)

const (
	claudeConfigDirEnv = "CLAUDE_CONFIG_DIR"
	homeEnv            = "HOME"
	xdgConfigHomeEnv   = "XDG_CONFIG_HOME"
)

func validateHostAuthorityOptions(options Options) error {
	if options.hostAuthoritySet {
		if !validHostAuthority(options.HostAuthority) {
			return ErrHostAuthorityUnavailable
		}

		if environment := options.HostAuthority.NativeEnvironment(); environment == nil {
			return fmt.Errorf("%w: native environment is unavailable", ErrHostAuthorityUnavailable)
		}
	}

	for key := range options.Env {
		if privateAdapterEnvName(key) {
			return fmt.Errorf("environment key %q uses the reserved %s prefix", key, privateAdapterEnvPrefix)
		}

		if managedClaudeRootEnvKey(key) {
			return fmt.Errorf("environment key %q is managed by the native launch boundary", key)
		}
	}

	return nil
}

func privateAdapterEnvName(key string) bool {
	return strings.HasPrefix(strings.ToUpper(key), privateAdapterEnvPrefix)
}

func managedClaudeRootEnvKey(key string) bool {
	switch claude.EnvironmentKey(key) {
	case claudeConfigDirEnv, homeEnv, "XDG_CACHE_HOME", xdgConfigHomeEnv, "XDG_DATA_HOME", "XDG_RUNTIME_DIR", "XDG_STATE_HOME":
		return true
	default:
		return false
	}
}
