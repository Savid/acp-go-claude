package claude

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestParseAssistantMessage(t *testing.T) {
	t.Parallel()

	msg, err := ParseMessage(map[string]any{
		"type":               "assistant",
		"session_id":         "session-1",
		"uuid":               "uuid-1",
		"parent_tool_use_id": "parent-1",
		"error":              "rate_limit",
		"message": map[string]any{
			"model":       "claude-test",
			"stop_reason": "end_turn",
			"content": []any{
				map[string]any{"type": "text", "text": "hello"},
				map[string]any{"type": "thinking", "thinking": "work", "signature": "sig"},
				map[string]any{"type": "tool_use", "id": "tool-1", "name": "Read", "input": map[string]any{"file_path": "/tmp/a"}},
				map[string]any{"type": "tool_result", "tool_use_id": "tool-1", "is_error": true, "content": []any{
					map[string]any{"type": "text", "text": "failed"},
				}},
			},
		},
	})

	require.NoError(t, err)

	assistant, ok := msg.(*AssistantMessage)
	require.True(t, ok)
	require.Equal(t, "claude-test", assistant.Model)
	require.Equal(t, "uuid-1", assistant.MessageID)
	require.Equal(t, "parent-1", assistant.ParentToolUseID)
	require.Equal(t, "end_turn", assistant.StopReason)
	require.Equal(t, "rate_limit", assistant.ErrorKind)
	require.Len(t, assistant.Content, 4)

	text, ok := assistant.Content[0].(TextBlock)
	require.True(t, ok)
	require.Equal(t, "hello", text.Text)

	thinking, ok := assistant.Content[1].(ThinkingBlock)
	require.True(t, ok)
	require.Equal(t, "work", thinking.Thinking)

	toolUse, ok := assistant.Content[2].(ToolUseBlock)
	require.True(t, ok)
	require.Equal(t, "Read", toolUse.Name)

	toolResult, ok := assistant.Content[3].(ToolResultBlock)
	require.True(t, ok)
	require.True(t, toolResult.IsError)
}

func TestParseResultMessage(t *testing.T) {
	t.Parallel()

	msg, err := ParseMessage(map[string]any{
		"type":        "result",
		"subtype":     "success",
		"is_error":    false,
		"session_id":  "session-1",
		"origin":      map[string]any{"kind": "task-notification"},
		"stop_reason": "end_turn",
		"result":      "done",
		"structured_output": map[string]any{
			"ok": true,
		},
		"duration_ms":    float64(12),
		"num_turns":      float64(2),
		"total_cost_usd": float64(0.01),
		"usage": map[string]any{
			"input_tokens":                float64(11),
			"output_tokens":               float64(7),
			"cache_read_input_tokens":     float64(3),
			"cache_creation_input_tokens": float64(2),
			"reasoning_output_tokens":     float64(5),
		},
		"modelUsage": map[string]any{
			"claude-sonnet-4-6": map[string]any{
				"inputTokens":              float64(11),
				"outputTokens":             float64(7),
				"cacheReadInputTokens":     float64(3),
				"cacheCreationInputTokens": float64(2),
				"webSearchRequests":        float64(1),
				"costUSD":                  float64(0.01),
				"contextWindow":            float64(200000),
				"maxOutputTokens":          float64(8192),
			},
		},
		"errors": []any{"one"},
	})

	require.NoError(t, err)

	result, ok := msg.(*ResultMessage)
	require.True(t, ok)
	require.Equal(t, "success", result.Subtype)
	require.Equal(t, "task-notification", result.Origin["kind"])
	require.Equal(t, "done", result.Result)
	require.Equal(t, map[string]any{"ok": true}, result.StructuredOutput)
	require.NotNil(t, result.TotalCostUSD)
	require.NotNil(t, result.Usage)
	require.Equal(t, 11, result.Usage.InputTokens)
	require.Equal(t, 7, result.Usage.OutputTokens)
	require.Equal(t, 3, result.Usage.CachedInputTokens)
	require.Equal(t, 2, result.Usage.CacheCreationInputTokens)
	require.Equal(t, 5, result.Usage.ReasoningOutputTokens)
	require.Len(t, result.ModelUsage, 1)
	require.Equal(t, 200000, result.ModelUsage["claude-sonnet-4-6"].ContextWindow)
	require.Equal(t, 2, result.ModelUsage["claude-sonnet-4-6"].CacheCreationInputTokens)
	require.Equal(t, []string{"one"}, result.Errors)
}

func TestParseResultMessageStringNumbers(t *testing.T) {
	t.Parallel()

	msg, err := ParseMessage(map[string]any{
		"type":           "result",
		"duration_ms":    "12",
		"num_turns":      "2",
		"total_cost_usd": "0.01",
		"usage": map[string]any{
			"input_tokens":                "11",
			"output_tokens":               "7",
			"cache_read_input_tokens":     "3",
			"cache_creation_input_tokens": "2",
			"reasoning_output_tokens":     "5",
		},
		"modelUsage": map[string]any{
			"claude-sonnet-4-6": map[string]any{
				"inputTokens":              "11",
				"outputTokens":             "7",
				"cacheReadInputTokens":     "3",
				"cacheCreationInputTokens": "2",
				"contextWindow":            "200000",
			},
		},
	})
	require.NoError(t, err)

	result, ok := msg.(*ResultMessage)
	require.True(t, ok)
	require.NotNil(t, result.TotalCostUSD)
	require.InDelta(t, 0.01, *result.TotalCostUSD, 0.000001)
	require.Equal(t, 11, result.Usage.InputTokens)
	require.Equal(t, 2, result.Usage.CacheCreationInputTokens)
	require.Equal(t, 200000, result.ModelUsage["claude-sonnet-4-6"].ContextWindow)
	require.Equal(t, 0, intValue("not-a-number"))
	require.Nil(t, floatPtr("not-a-number"))
}

func TestParseUserSystemAndUnknownBlocks(t *testing.T) {
	t.Parallel()

	userMsg, err := ParseMessage(map[string]any{
		"type":               "user",
		"session_id":         "session-1",
		"uuid":               "uuid-1",
		"parent_tool_use_id": "parent-1",
		"content":            "hello",
	})
	require.NoError(t, err)

	user, ok := userMsg.(*UserMessage)
	require.True(t, ok)
	require.Equal(t, MessageTypeUser, user.ClaudeType())
	require.Equal(t, "parent-1", user.ParentToolUseID)

	systemMsg, err := ParseMessage(map[string]any{
		"type":    "system",
		"subtype": "init",
		"cwd":     "/repo",
	})
	require.NoError(t, err)

	system, ok := systemMsg.(*SystemMessage)
	require.True(t, ok)
	require.Equal(t, MessageTypeSystem, system.ClaudeType())
	require.Equal(t, "init", system.Subtype)
	require.Equal(t, "/repo", system.Raw["cwd"])

	unknown, err := parseBlock(map[string]any{"type": "future"})
	require.NoError(t, err)
	require.Equal(t, "future", unknown.BlockType())
	require.Equal(t, BlockTypeText, ParseContentBlock(map[string]any{"type": "text", "text": "parsed"}).BlockType())
	require.Equal(t, "tool_use", ParseContentBlock(map[string]any{"type": "tool_use"}).BlockType())
	require.Equal(t, BlockTypeText, TextBlock{}.BlockType())
	require.Equal(t, BlockTypeThinking, ThinkingBlock{}.BlockType())
	require.Equal(t, BlockTypeToolUse, ToolUseBlock{}.BlockType())
	require.Equal(t, BlockTypeToolResult, ToolResultBlock{}.BlockType())
}

func TestParseAssistantRejectsUncorrelatableToolBlocks(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name  string
		block map[string]any
		want  string
	}{
		{
			name:  "tool use missing id",
			block: map[string]any{"type": "tool_use", "name": "Read", "input": map[string]any{"file_path": "/tmp/a"}},
			want:  "tool_use block missing id",
		},
		{
			name:  "tool result missing id",
			block: map[string]any{"type": "tool_result", "content": "done"},
			want:  "tool_result block missing tool_use_id",
		},
		{
			name:  "missing string type",
			block: map[string]any{"type": nil, "content": "done"},
			want:  "content block missing string type",
		},
		{
			name: "tool result invalid nested block",
			block: map[string]any{
				"type":        "tool_result",
				"tool_use_id": "tool-1",
				"content":     []any{map[string]any{"type": "tool_use"}},
			},
			want: "tool_use block missing id",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := ParseMessage(map[string]any{
				"type": "assistant",
				"message": map[string]any{
					"content": []any{tc.block},
				},
			})
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want)
		})
	}
}

func TestMessageRawMessage(t *testing.T) {
	t.Parallel()

	raw := map[string]any{"type": "test"}
	messages := []Message{
		&UserMessage{Raw: raw, RawJSONText: `{"type":"user"}`},
		&AssistantMessage{Raw: raw, RawJSONText: `{"type":"assistant"}`},
		&ResultMessage{Raw: raw, RawJSONText: `{"type":"result"}`},
		&SystemMessage{Raw: raw, RawJSONText: `{"type":"system"}`},
		&StreamEventMessage{Raw: raw, RawJSONText: `{"type":"stream_event"}`},
		&TranscriptMirrorMessage{Raw: raw, RawJSONText: `{"type":"transcript_mirror"}`},
		&UnknownMessage{Raw: raw, RawJSONText: `{"type":"future"}`},
	}

	for _, msg := range messages {
		require.Equal(t, raw, msg.RawMessage())
		require.NotEmpty(t, msg.RawJSON())
	}
}

func TestParseMessagePreservesRawJSONAndTranscriptMirror(t *testing.T) {
	t.Parallel()

	rawLine := `{"type":"transcript_mirror","filePath":"/tmp/.claude/projects/-repo/00000000-0000-4000-8000-000000000000.jsonl","entries":[{"type":"user"},{"type":"assistant"}]}`
	msg, err := ParseMessage(map[string]any{
		rawJSONInternalKey: rawLine,
		"type":             "transcript_mirror",
		"filePath":         "/tmp/.claude/projects/-repo/00000000-0000-4000-8000-000000000000.jsonl",
		"entries":          []any{map[string]any{"type": "user"}},
	})
	require.NoError(t, err)

	mirror, ok := msg.(*TranscriptMirrorMessage)
	require.True(t, ok)
	require.Equal(t, MessageTypeMirror, mirror.ClaudeType())
	require.Equal(t, rawLine, mirror.RawJSON())
	require.NotContains(t, mirror.RawMessage(), rawJSONInternalKey)
	require.Len(t, mirror.Entries, 2)
	require.JSONEq(t, `{"type":"user"}`, string(mirror.Entries[0]))

	msg, err = ParseMessage(map[string]any{
		"type":     "transcript_mirror",
		"filePath": "/tmp/.claude/projects/-repo/00000000-0000-4000-8000-000000000000.jsonl",
		"entries":  []any{map[string]any{"type": "user"}, ""},
	})
	require.NoError(t, err)
	mirror, ok = msg.(*TranscriptMirrorMessage)
	require.True(t, ok)
	require.Len(t, mirror.Entries, 2)

	_, err = ParseMessage(map[string]any{
		rawJSONInternalKey: `{"type":"transcript_mirror","entries":`,
		"type":             "transcript_mirror",
	})
	require.Error(t, err)
}

func TestParseTranscriptMirrorEntryCleaning(t *testing.T) {
	t.Parallel()

	mirror, err := parseTranscriptMirror(map[string]any{
		"type":    "transcript_mirror",
		"entries": []any{},
	}, "")
	require.NoError(t, err)
	require.Nil(t, mirror.Entries)

	require.Nil(t, cleanMirrorEntries([]json.RawMessage{json.RawMessage(` `)}))

	_, err = parseTranscriptMirror(map[string]any{
		"type":    "transcript_mirror",
		"entries": []any{make(chan struct{})},
	}, "")
	require.Error(t, err)
}

func TestParseStreamEventMessage(t *testing.T) {
	t.Parallel()

	msg, err := ParseMessage(map[string]any{
		"type":               "stream_event",
		"session_id":         "session-1",
		"uuid":               "uuid-1",
		"parent_tool_use_id": "parent-1",
		"event": map[string]any{
			"type": "content_block_delta",
			"delta": map[string]any{
				"type": "text_delta",
				"text": "hello",
			},
		},
	})
	require.NoError(t, err)

	stream, ok := msg.(*StreamEventMessage)
	require.True(t, ok)
	require.Equal(t, MessageTypeStream, stream.ClaudeType())
	require.Equal(t, "content_block_delta", stream.EventType)
	require.Equal(t, "parent-1", stream.ParentToolUseID)
	require.NotNil(t, stream.Event["delta"])
}

func TestParseResultNumericVariants(t *testing.T) {
	t.Parallel()

	msg, err := ParseMessage(map[string]any{
		"type":           "result",
		"duration_ms":    int64(12),
		"num_turns":      2,
		"total_cost_usd": float32(0.5),
		"errors":         []string{"one"},
	})
	require.NoError(t, err)

	result, ok := msg.(*ResultMessage)
	require.True(t, ok)
	require.Equal(t, []string{"one"}, result.Errors)
	require.NotNil(t, result.TotalCostUSD)

	require.Equal(t, 7, intValue(7))
	require.Equal(t, 12, intValue(int64(12)))
	require.Equal(t, 0, intValue(nil))
	require.Equal(t, 2.0, *floatPtr(2))
	require.Equal(t, 3.0, *floatPtr(int64(3)))
	require.Nil(t, floatPtr("bad"))
}

func TestParseResultNumericDefaults(t *testing.T) {
	t.Parallel()

	msg, err := ParseMessage(map[string]any{
		"type":           "result",
		"duration_ms":    "bad",
		"num_turns":      float64(3),
		"total_cost_usd": "bad",
		"errors":         []any{"one", 2, "two"},
	})
	require.NoError(t, err)

	result, ok := msg.(*ResultMessage)
	require.True(t, ok)
	require.Nil(t, result.TotalCostUSD)
	require.Equal(t, []string{"one", "two"}, result.Errors)
}

func TestParseResultUsageIgnoresInvalidEntries(t *testing.T) {
	t.Parallel()

	msg, err := ParseMessage(map[string]any{
		"type":       "result",
		"usage":      "bad",
		"modelUsage": map[string]any{"bad": "entry"},
	})
	require.NoError(t, err)

	result, ok := msg.(*ResultMessage)
	require.True(t, ok)
	require.Nil(t, result.Usage)
	require.Nil(t, result.ModelUsage)
}

func TestParseAssistantStringContent(t *testing.T) {
	t.Parallel()

	msg, err := ParseMessage(map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"content": "done",
		},
	})
	require.NoError(t, err)

	assistant, ok := msg.(*AssistantMessage)
	require.True(t, ok)
	require.Len(t, assistant.Content, 1)

	text, ok := assistant.Content[0].(TextBlock)
	require.True(t, ok)
	require.Equal(t, "done", text.Text)

	msg, err = ParseMessage(map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"content": "",
		},
	})
	require.NoError(t, err)

	assistant, ok = msg.(*AssistantMessage)
	require.True(t, ok)
	require.Empty(t, assistant.Content)
}

func TestParseUnknownAndInvalidAssistant(t *testing.T) {
	t.Parallel()

	msg, err := ParseMessage(map[string]any{"type": "future"})
	require.NoError(t, err)
	require.IsType(t, &UnknownMessage{}, msg)

	_, err = ParseMessage(map[string]any{"type": "assistant"})
	require.Error(t, err)
}

func TestMessageClaudeTypes(t *testing.T) {
	t.Parallel()

	require.Equal(t, MessageTypeAssistant, (&AssistantMessage{}).ClaudeType())
	require.Equal(t, MessageTypeResult, (&ResultMessage{}).ClaudeType())
	require.Equal(t, MessageTypeSystem, (&SystemMessage{}).ClaudeType())
	require.Equal(t, "future", (&UnknownMessage{Type: "future"}).ClaudeType())
}

func TestParseAdditionalBlockVariants(t *testing.T) {
	t.Parallel()

	msg, err := ParseMessage(map[string]any{
		"type": "assistant",
		"message": map[string]any{
			"content": []any{
				map[string]any{"type": BlockTypeServerUse, "id": "server-1", "name": "WebSearch"},
				map[string]any{"type": BlockTypeServerResult, "tool_use_id": "server-1", "content": []any{
					map[string]any{"type": "text", "text": "ok"},
				}},
				"ignored",
			},
		},
	})
	require.NoError(t, err)

	assistant, ok := msg.(*AssistantMessage)
	require.True(t, ok)
	require.Len(t, assistant.Content, 2)

	serverUse, ok := assistant.Content[0].(ToolUseBlock)
	require.True(t, ok)
	require.Equal(t, "WebSearch", serverUse.Name)
	require.Nil(t, serverUse.Input)

	serverResult, ok := assistant.Content[1].(ToolResultBlock)
	require.True(t, ok)
	require.Equal(t, "server-1", serverResult.ToolUseID)
	require.Len(t, serverResult.Content, 1)
}

func TestParseSystemKeepsRawData(t *testing.T) {
	t.Parallel()

	msg, err := ParseMessage(map[string]any{
		"type":    "system",
		"subtype": "init",
		"cwd":     "/repo",
	})
	require.NoError(t, err)

	system, ok := msg.(*SystemMessage)
	require.True(t, ok)
	require.Equal(t, "/repo", system.Raw["cwd"])
}
