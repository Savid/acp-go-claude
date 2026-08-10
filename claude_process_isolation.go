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

// captureImplicitIsolation snapshots the ordinary current-identity launch
// policy exactly once, at agent construction, and only when no explicit policy
// was configured. Every native launch is then handed a clone of this one
// capture, so the implicit base environment is deterministic for the agent's
// whole lifetime.
func captureImplicitIsolation(options Options) *claude.ProcessIsolation {
	if options.ProcessIsolation != nil {
		return nil
	}

	return claude.ImplicitProcessIsolation()
}

// claudeIsolation is the launch policy every native process runs under: the
// explicit policy when one was configured, otherwise a clone of the implicit
// current-identity capture.
func (a *Agent) claudeIsolation() *claude.ProcessIsolation {
	if isolation := claudeProcessIsolation(a.options.ProcessIsolation); isolation != nil {
		return isolation
	}

	if a.implicitIsolation == nil {
		return nil
	}

	clone := *a.implicitIsolation
	clone.BaseEnvironment = cloneStringMap(a.implicitIsolation.BaseEnvironment)

	return &clone
}
