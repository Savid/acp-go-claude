package claudeacp

import (
	"context"
	"testing"

	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/stretchr/testify/require"
)

func TestRawMessageConfigFromMeta(t *testing.T) {
	t.Parallel()

	require.False(t, rawMessageConfigFromMeta(nil).Enabled())
	require.False(t, rawMessageConfigFromMeta(map[string]any{claudeMetaKey: map[string]any{
		"other": true,
	}}).Enabled())
	require.False(t, rawMessageConfigFromMeta(map[string]any{claudeMetaKey: map[string]any{
		emitRawSDKMessagesKey: false,
	}}).Enabled())

	config := rawMessageConfigFromMeta(map[string]any{claudeMetaKey: map[string]any{
		emitRawSDKMessagesKey: true,
	}})
	require.True(t, config.Enabled())
	require.True(t, config.ShouldEmit(map[string]any{rawMessageTypeKey: "system"}))
	require.False(t, config.ShouldEmit(map[string]any{rawMessageTypeKey: "control_request"}))
	require.False(t, config.ShouldEmit(map[string]any{rawMessageTypeKey: "control_response"}))

	config = rawMessageConfigFromMeta(map[string]any{claudeMetaKey: map[string]any{
		emitRawSDKMessagesKey: []any{
			map[string]any{rawMessageTypeKey: "system", rawMessageSubtypeKey: "compact_boundary"},
			map[string]any{rawMessageTypeKey: "result", rawMessageOriginKey: "task-notification"},
			map[string]any{rawMessageSubtypeKey: "ignored"},
			"bad",
		},
	}})
	require.True(t, config.ShouldEmit(map[string]any{
		rawMessageTypeKey:    "system",
		rawMessageSubtypeKey: "compact_boundary",
	}))
	require.False(t, config.ShouldEmit(map[string]any{
		rawMessageTypeKey:    "system",
		rawMessageSubtypeKey: "status",
	}))
	require.False(t, config.ShouldEmit(map[string]any{
		rawMessageTypeKey: "result",
		rawMessageOriginKey: map[string]any{
			rawMessageOriginKindKey: "channel",
		},
	}))
	require.True(t, config.ShouldEmit(map[string]any{
		rawMessageTypeKey: "result",
		rawMessageOriginKey: map[string]any{
			rawMessageOriginKindKey: "task-notification",
		},
	}))
	require.False(t, config.ShouldEmit(nil))

	_, ok := rawMessageConfigFromValue("bad")
	require.False(t, ok)
	require.False(t, rawMessageFilter{Type: ""}.Matches(map[string]any{rawMessageTypeKey: "system"}))
	require.False(t, rawMessageFilter{Type: "system"}.Matches(nil))
	require.True(t, internalRawMessage(map[string]any{rawMessageTypeKey: "control_request"}))
	require.False(t, internalRawMessage(map[string]any{rawMessageTypeKey: "system"}))
	require.Empty(t, rawStringValue(nil, "missing"))
}

func TestRawClaudeMessage(t *testing.T) {
	t.Parallel()

	for _, msg := range []claude.Message{
		&claude.UserMessage{Raw: map[string]any{"type": "user"}},
		&claude.AssistantMessage{Raw: map[string]any{"type": "assistant"}},
		&claude.ResultMessage{Raw: map[string]any{"type": "result"}},
		&claude.SystemMessage{Raw: map[string]any{"type": "system"}},
		&claude.UnknownMessage{Raw: map[string]any{"type": "future"}},
	} {
		require.NotNil(t, rawClaudeMessage(msg))
	}

	require.Nil(t, rawClaudeMessage(nil))
	require.Empty(t, rawClaudeJSON(nil))
	require.Empty(t, rawClaudeJSON(&claude.UserMessage{}))

	agent := NewAgent()
	agent.conn = &stubAgentClient{}
	session := &Session{
		agent:       agent,
		id:          "session-1",
		rawMessages: rawMessageConfig{All: true},
	}
	require.NoError(t, session.emitRawClaudeMessage(context.Background(), &claude.UserMessage{
		Raw:         map[string]any{"type": "user"},
		RawJSONText: `{"type":"user"}`,
	}))
}
