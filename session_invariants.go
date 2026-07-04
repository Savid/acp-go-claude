package claudeacp

import (
	"context"
	"fmt"

	"github.com/savid/acp-go-claude/internal/claude"
)

const claudeMessageTypeConversationReset = "conversation_reset"

func (s *agentSession) checkNativeSessionInvariant(ctx context.Context, msg claude.Message) error {
	if msg == nil {
		return nil
	}

	if msg.ClaudeType() == claudeMessageTypeConversationReset {
		return s.poison(ctx, "native conversation_reset frame")
	}

	nativeID, _ := msg.RawMessage()["session_id"].(string)
	if nativeID == "" || nativeID == string(s.id) {
		return nil
	}

	return s.poison(ctx, fmt.Sprintf("native session_id drift: expected %s, got %s", s.id, nativeID))
}
