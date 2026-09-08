package claudeacp

import "github.com/savid/acp-go-claude/internal/claude"

func (a *Agent) effectiveNativeEnvironment(overlay map[string]string) map[string]string {
	return claude.EffectiveEnvironment(claude.Options{
		Env:                 overlay,
		OrdinaryEnvironment: a.ordinaryEnvironment(),
		Authority:           a.claudeAuthority(),
	})
}
