package claudeacp

import "github.com/savid/acp-go-claude/internal/claude"

// ErrProcessTreeUnproven means shutdown could not prove that every native
// Claude descendant exited. Callers must keep the runtime quarantined.
var ErrProcessTreeUnproven = claude.ErrProcessTreeUnproven
