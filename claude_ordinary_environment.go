package claudeacp

import "github.com/savid/acp-go-claude/internal/claude"

func captureOrdinaryEnvironment(options Options) map[string]string {
	if options.hostAuthoritySet {
		return nil
	}

	return claude.OrdinaryEnvironment()
}

func (a *Agent) ordinaryEnvironment() map[string]string {
	return cloneStringMap(a.ordinaryEnv)
}
