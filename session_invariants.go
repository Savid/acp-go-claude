package claudeacp

import (
	"context"

	"github.com/savid/acp-go-claude/internal/claude"
)

const claudeMessageTypeConversationReset = "conversation_reset"

// checkNativeSessionInvariant poisons the session when the native stream breaks
// one of the two invariants this adapter relies on. Each violation is recorded
// as one of the closed poison causes: the drifting native id names no cause of
// its own, because the value came from the harness and never crosses the wire.
func (s *agentSession) checkNativeSessionInvariant(ctx context.Context, msg claude.Message) error {
	if msg == nil {
		return nil
	}

	if msg.ClaudeType() == claudeMessageTypeConversationReset {
		return s.poison(ctx, poisonCauseConversationReset)
	}

	nativeID, _ := msg.RawMessage()["session_id"].(string)
	if nativeID == "" || nativeID == string(s.id) {
		return nil
	}

	return s.poison(ctx, poisonCauseSessionIDDrift)
}
