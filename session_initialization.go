package claudeacp

import (
	"context"
	"fmt"
)

// Claude accepts an empty session through --session-id; --resume requires a
// conversation. The configuration row durably reserves that exact native ID.
func (s *agentSession) initializeDurability(ctx context.Context) error {
	writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionSettlementTimeout)
	defer cancel()

	m := s.mirror
	m.configurationMu.Lock()
	defer m.configurationMu.Unlock()

	if m.configurationWritten {
		return nil
	}

	appendCtx, finish := s.agent.observe.StartSessionStore(writeCtx, "append")
	err := appendMirrorEntries(appendCtx, m.store, SessionKey{SessionID: string(s.id)}, []SessionStoreEntry{
		marshalSessionConfiguration(m.configuration),
	})
	finish(err)

	if err != nil {
		return fmt.Errorf("initialize session durability: %w", err)
	}

	m.configurationWritten = true

	return nil
}
