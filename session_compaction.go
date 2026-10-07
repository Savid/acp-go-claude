package claudeacp

import (
	"context"

	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/savid/acp-go-core/wire"
)

func (s *session) projectCompaction(ctx context.Context, event claude.Event) error {
	value := wire.Compaction{Status: wire.CompactionCompleted}

	if metadata := event.CompactMetadata; metadata != nil {
		switch metadata.Trigger {
		case wire.CompactionTriggerAuto, wire.CompactionTriggerManual:
			value.Trigger = metadata.Trigger
		}

		value.ContextBefore = metadata.PreTokens
		value.ContextAfter = metadata.PostTokens
	}

	return s.compactions.Publish(ctx, s.agent.connection(), s.id, event.UUID, value)
}
