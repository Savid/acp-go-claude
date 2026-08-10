package claudeacp

import "github.com/savid/acp-go-claude/internal/claude"

func claudeProcessIsolation(value *ProcessIsolation) *claude.ProcessIsolation {
	if value == nil {
		return nil
	}

	return &claude.ProcessIsolation{
		UID: value.UID, GID: value.GID, BaseEnvironment: cloneStringMap(value.BaseEnvironment),
		StandaloneOwnerID: value.StandaloneOwnerID, StandaloneStateRoot: value.StandaloneStateRoot,
		IdentityLock: value.IdentityLock, AuthorityDomain: value.AuthorityDomain,
	}
}

// captureOrdinaryEnvironment snapshots the ambient environment ordinary
// same-identity execution launches native processes with. It is taken exactly
// once, at agent construction, and only when no explicit policy was configured,
// so the base is deterministic for the agent's whole lifetime. It is an
// ordinary runtime value: no ProcessIsolation is manufactured from it.
func captureOrdinaryEnvironment(options Options) map[string]string {
	if options.ProcessIsolation != nil {
		return nil
	}

	return claude.OrdinaryEnvironment()
}

// claudeIsolation is the explicitly configured hardened policy, or nil when the
// embedder configured none. Nil travels all the way to the launch selector,
// where it selects ordinary same-identity execution.
func (a *Agent) claudeIsolation() *claude.ProcessIsolation {
	return claudeProcessIsolation(a.options.ProcessIsolation)
}

// ordinaryEnvironment answers with a copy of the one ambient capture every
// ordinary native launch runs with. It is empty for an explicitly isolated
// agent, whose replacement base comes from the policy instead.
func (a *Agent) ordinaryEnvironment() map[string]string {
	return cloneStringMap(a.ordinaryEnv)
}
