package mapper

import (
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/stretchr/testify/require"
)

func TestStopReason(t *testing.T) {
	t.Parallel()

	require.Equal(t, acp.StopReasonCancelled, StopReason(&claude.ResultMessage{}, true))
	require.Equal(t, acp.StopReasonEndTurn, StopReason(nil, false))
	require.Equal(t, acp.StopReasonMaxTokens, StopReason(&claude.ResultMessage{StopReason: "max_tokens"}, false))
	require.Equal(t, acp.StopReasonMaxTurnRequests, StopReason(&claude.ResultMessage{Subtype: "error_max_turns"}, false))
	require.Equal(t, acp.StopReasonRefusal, StopReason(&claude.ResultMessage{StopReason: "refusal"}, false))
	require.Equal(t, acp.StopReasonEndTurn, StopReason(&claude.ResultMessage{IsError: true, Subtype: "error_during_execution"}, false))
	require.Equal(t, acp.StopReasonEndTurn, StopReason(&claude.ResultMessage{StopReason: "tool_use"}, false))
	require.Equal(t, acp.StopReasonEndTurn, StopReason(&claude.ResultMessage{StopReason: "stop_sequence"}, false))
	require.Equal(t, acp.StopReasonEndTurn, StopReason(&claude.ResultMessage{StopReason: "future"}, false))
	require.Equal(t, acp.StopReasonEndTurn, StopReason(&claude.ResultMessage{}, false))
}

func TestUnknownStopReason(t *testing.T) {
	t.Parallel()

	require.Empty(t, UnknownStopReason(nil))
	require.Empty(t, UnknownStopReason(&claude.ResultMessage{Subtype: "error_max_turns", StopReason: "future"}))
	require.Empty(t, UnknownStopReason(&claude.ResultMessage{StopReason: ""}))
	require.Empty(t, UnknownStopReason(&claude.ResultMessage{StopReason: "max_tokens"}))
	require.Empty(t, UnknownStopReason(&claude.ResultMessage{StopReason: "refusal"}))
	require.Empty(t, UnknownStopReason(&claude.ResultMessage{StopReason: "end_turn"}))
	require.Empty(t, UnknownStopReason(&claude.ResultMessage{StopReason: "tool_use"}))
	require.Empty(t, UnknownStopReason(&claude.ResultMessage{StopReason: "stop_sequence"}))
	require.Empty(t, UnknownStopReason(&claude.ResultMessage{StopReason: "tool_deferred"}))
	require.Equal(t, "future", UnknownStopReason(&claude.ResultMessage{StopReason: "future"}))
}

func TestUsageUpdate(t *testing.T) {
	t.Parallel()

	require.Nil(t, UsageUpdate(nil))
	require.Nil(t, UsageUpdate(&claude.ResultMessage{}))
	require.Nil(t, ClaudeUsageMeta(nil))
	require.Nil(t, UsageMeta(nil))
	thought := 5
	require.Equal(t, map[string]any{
		"inputTokens":       1,
		"outputTokens":      2,
		"cachedReadTokens":  0,
		"cachedWriteTokens": 0,
		"thoughtTokens":     5,
		"totalTokens":       8,
	}, UsageMeta(&acp.Usage{
		InputTokens:   1,
		OutputTokens:  2,
		ThoughtTokens: &thought,
		TotalTokens:   8,
	}))

	cost := 0.42
	updates := UsageUpdate(&claude.ResultMessage{
		TotalCostUSD: &cost,
		Usage: &claude.Usage{
			InputTokens:              10,
			OutputTokens:             5,
			CachedInputTokens:        3,
			CacheCreationInputTokens: 4,
		},
		ModelUsage: map[string]claude.ModelUsage{
			"claude-sonnet-4-6": {ContextWindow: 200000},
		},
	}, &claude.ContextUsage{
		TotalTokens: 42,
		MaxTokens:   250000,
	})

	require.Len(t, updates, 1)
	require.Equal(t, 0.42, updates[0].UsageUpdate.Cost.Amount)
	require.Equal(t, "USD", updates[0].UsageUpdate.Cost.Currency)
	require.Equal(t, 250000, updates[0].UsageUpdate.Size)
	require.Equal(t, 42, updates[0].UsageUpdate.Used)
	require.Equal(t, map[string]any{
		"usage": map[string]any{
			"inputTokens":       10,
			"outputTokens":      5,
			"cachedReadTokens":  3,
			"cachedWriteTokens": 4,
			"thoughtTokens":     0,
			"totalTokens":       22,
		},
		"modelUsage": map[string]any{
			"claude-sonnet-4-6": map[string]any{
				"inputTokens":       0,
				"outputTokens":      0,
				"cachedReadTokens":  0,
				"cachedWriteTokens": 0,
				"contextWindow":     200000,
			},
		},
	}, updates[0].UsageUpdate.Meta[keyClaude])

	updates = UsageUpdate(&claude.ResultMessage{
		TotalCostUSD: &cost,
		Usage:        &claude.Usage{InputTokens: 1},
		Origin:       map[string]any{"kind": "task-notification"},
		StructuredOutput: map[string]any{
			"ok": true,
		},
	}, nil)
	require.Len(t, updates, 1)
	require.Equal(t, 1, updates[0].UsageUpdate.Used)
	require.Equal(t, map[string]any{
		"usage": map[string]any{
			"inputTokens":       1,
			"outputTokens":      0,
			"cachedReadTokens":  0,
			"cachedWriteTokens": 0,
			"thoughtTokens":     0,
			"totalTokens":       1,
		},
		"origin":           map[string]any{"kind": "task-notification"},
		"structuredOutput": map[string]any{"ok": true},
	}, updates[0].UsageUpdate.Meta[keyClaude])

	updates = UsageUpdate(&claude.ResultMessage{
		StructuredOutput: map[string]any{"ok": true},
	})
	require.Len(t, updates, 1)
	require.Nil(t, updates[0].UsageUpdate.Cost)
	require.Equal(t, map[string]any{
		"structuredOutput": map[string]any{"ok": true},
	}, updates[0].UsageUpdate.Meta[keyClaude])
}

func TestUsage(t *testing.T) {
	t.Parallel()

	require.Nil(t, Usage(nil))
	require.Nil(t, Usage(&claude.ResultMessage{}))
	require.Nil(t, Usage(&claude.ResultMessage{Usage: &claude.Usage{}}))

	usage := Usage(&claude.ResultMessage{
		Usage: &claude.Usage{
			InputTokens:              10,
			OutputTokens:             5,
			CachedInputTokens:        3,
			CacheCreationInputTokens: 4,
			ReasoningOutputTokens:    2,
		},
	})
	require.NotNil(t, usage)
	require.Equal(t, 10, usage.InputTokens)
	require.Equal(t, 5, usage.OutputTokens)
	require.Equal(t, 24, usage.TotalTokens)
	require.Equal(t, 3, *usage.CachedReadTokens)
	require.Equal(t, 4, *usage.CachedWriteTokens)
	require.Equal(t, 2, *usage.ThoughtTokens)

	usage = Usage(&claude.ResultMessage{
		ModelUsage: map[string]claude.ModelUsage{
			"sonnet": {
				InputTokens:              10,
				OutputTokens:             5,
				CacheReadInputTokens:     3,
				CacheCreationInputTokens: 2,
			},
			"opus": {
				InputTokens:  4,
				OutputTokens: 1,
			},
		},
	})
	require.NotNil(t, usage)
	require.Equal(t, 14, usage.InputTokens)
	require.Equal(t, 6, usage.OutputTokens)
	require.Equal(t, 25, usage.TotalTokens)
	require.Equal(t, 3, *usage.CachedReadTokens)
	require.Equal(t, 2, *usage.CachedWriteTokens)
}
