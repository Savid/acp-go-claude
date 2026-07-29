package claude

import "os/exec"

// newProcessCommand builds a native command that no context can kill. Every
// child on this surface outlives the call that spawns it: a session root
// outlives session/new, and a login child outlives authorize, which returns
// while the operator is still reading the authorization URL. Requests are
// dispatched one goroutine each and their context is cancelled the moment the
// handler returns, so a context-bound child dies before it has been used. The
// containment boundary is the sole authoritative shutdown channel — quiesce,
// wait, close — and it is driven by the owner whose lifetime the child shares.
func newProcessCommand(path string, args ...string) *exec.Cmd {
	return exec.Command(path, args...) //nolint:gosec // Launches the configured Claude binary.
}
