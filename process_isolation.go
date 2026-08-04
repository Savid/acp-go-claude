package claudeacp

import "github.com/savid/acp-go-claude/internal/claude"

func claudeProcessIsolation(value *ProcessIsolation) *claude.ProcessIsolation {
	if value == nil {
		return nil
	}

	return &claude.ProcessIsolation{
		UID: value.UID, GID: value.GID, BaseEnvironment: cloneStringMap(value.BaseEnvironment),
	}
}
