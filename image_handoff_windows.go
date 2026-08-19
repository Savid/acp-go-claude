//go:build windows

package claudeacp

// handoffOpenFlags adds nothing on Windows: the platform exposes no
// non-blocking open mode for this call. Containment is still the read root's,
// and the descriptor's own mode check still refuses a name under the root that
// does not lead to a regular file.
const handoffOpenFlags = 0
