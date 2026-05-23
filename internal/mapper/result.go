package mapper

import (
	"maps"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
)

const (
	keyOrigin           = "origin"
	keyUsage            = "usage"
	keyModelUsage       = "modelUsage"
	keyStructuredOutput = "structuredOutput"

	keyInputTokens       = "inputTokens"
	keyOutputTokens      = "outputTokens"
	keyCachedReadTokens  = "cachedReadTokens"
	keyCachedWriteTokens = "cachedWriteTokens"
	keyThoughtTokens     = "thoughtTokens"
	keyTotalTokens       = "totalTokens"
	keyContextWindow     = "contextWindow"

	stopReasonErrorMaxTurns = "error_max_turns"
	stopReasonEndTurn       = "end_turn"
	stopReasonMaxTokens     = "max_tokens"
	stopReasonRefusal       = "refusal"
	stopReasonStopSequence  = "stop_sequence"
	stopReasonToolDeferred  = "tool_deferred"
	stopReasonToolUse       = "tool_use"
)

// StopReason converts a Claude terminal result into an ACP stop reason.
func StopReason(result *claude.ResultMessage, cancelled bool) acp.StopReason {
	if cancelled {
		return acp.StopReasonCancelled
	}

	if result == nil {
		return acp.StopReasonEndTurn
	}

	if result.Subtype == stopReasonErrorMaxTurns {
		return acp.StopReasonMaxTurnRequests
	}

	switch result.StopReason {
	case stopReasonMaxTokens:
		return acp.StopReasonMaxTokens
	case stopReasonRefusal:
		return acp.StopReasonRefusal
	case "", stopReasonEndTurn, stopReasonToolUse, stopReasonStopSequence, stopReasonToolDeferred:
		return acp.StopReasonEndTurn
	default:
		return acp.StopReasonEndTurn
	}
}

// UnknownStopReason returns the Claude stop_reason when it is not a known value.
func UnknownStopReason(result *claude.ResultMessage) string {
	if result == nil || result.Subtype == stopReasonErrorMaxTurns {
		return ""
	}

	switch result.StopReason {
	case "", stopReasonMaxTokens, stopReasonRefusal, stopReasonEndTurn, stopReasonToolUse, stopReasonStopSequence, stopReasonToolDeferred:
		return ""
	default:
		return result.StopReason
	}
}

// UsageUpdate converts Claude result and context metadata into an ACP usage update.
func UsageUpdate(result *claude.ResultMessage, contextUsage ...*claude.ContextUsage) []acp.SessionUpdate {
	if result == nil || (result.TotalCostUSD == nil && len(result.StructuredOutput) == 0) {
		return nil
	}

	tokenUsage := Usage(result)
	used := 0

	if tokenUsage != nil {
		used = tokenUsage.TotalTokens
	}

	size := contextWindow(result)

	if usage := firstContextUsage(contextUsage); usage != nil {
		if usage.MaxTokens > 0 {
			size = usage.MaxTokens
		}

		if usage.TotalTokens > 0 {
			used = usage.TotalTokens
		}
	}

	var cost *acp.Cost
	if result.TotalCostUSD != nil {
		cost = &acp.Cost{
			Amount:   *result.TotalCostUSD,
			Currency: "USD",
		}
	}

	update := acp.SessionUpdate{
		UsageUpdate: &acp.SessionUsageUpdate{
			Cost: cost,
			Size: size,
			Used: used,
		},
	}
	mergeUsageMeta(update.UsageUpdate, ClaudeUsageMeta(result))

	if len(result.Origin) > 0 {
		usageMeta(update.UsageUpdate)[keyOrigin] = result.Origin
	}

	if len(result.StructuredOutput) > 0 {
		usageMeta(update.UsageUpdate)[keyStructuredOutput] = result.StructuredOutput
	}

	return []acp.SessionUpdate{
		update,
	}
}

// ClaudeUsageMeta converts Claude-native usage details into usage_update
// extension metadata. ACP keeps token breakdowns on PromptResponse.Usage; this
// preserves the same data for clients that consume only streamed updates.
func ClaudeUsageMeta(result *claude.ResultMessage) map[string]any {
	if result == nil {
		return nil
	}

	meta := make(map[string]any)
	if usage := Usage(result); usage != nil {
		meta[keyUsage] = UsageMeta(usage)
	}

	if modelUsage := modelUsageMeta(result.ModelUsage); len(modelUsage) > 0 {
		meta[keyModelUsage] = modelUsage
	}

	if len(meta) == 0 {
		return nil
	}

	return meta
}

// UsageMeta converts ACP usage into a stable metadata map with all token fields
// present, including zero values.
func UsageMeta(usage *acp.Usage) map[string]any {
	if usage == nil {
		return nil
	}

	meta := map[string]any{
		keyInputTokens:       usage.InputTokens,
		keyOutputTokens:      usage.OutputTokens,
		keyCachedReadTokens:  0,
		keyCachedWriteTokens: 0,
		keyThoughtTokens:     0,
		keyTotalTokens:       usage.TotalTokens,
	}

	if usage.CachedReadTokens != nil {
		meta[keyCachedReadTokens] = *usage.CachedReadTokens
	}

	if usage.CachedWriteTokens != nil {
		meta[keyCachedWriteTokens] = *usage.CachedWriteTokens
	}

	if usage.ThoughtTokens != nil {
		meta[keyThoughtTokens] = *usage.ThoughtTokens
	}

	return meta
}

func usageMeta(update *acp.SessionUsageUpdate) map[string]any {
	if update.Meta == nil {
		update.Meta = map[string]any{}
	}

	claudeMeta, _ := update.Meta[keyClaude].(map[string]any)
	if claudeMeta == nil {
		claudeMeta = map[string]any{}
		update.Meta[keyClaude] = claudeMeta
	}

	return claudeMeta
}

func mergeUsageMeta(update *acp.SessionUsageUpdate, meta map[string]any) {
	if len(meta) == 0 {
		return
	}

	claudeMeta := usageMeta(update)
	maps.Copy(claudeMeta, meta)
}

// Usage converts Claude token usage metadata into ACP prompt usage.
func Usage(result *claude.ResultMessage) *acp.Usage {
	if result == nil {
		return nil
	}

	if result.Usage != nil {
		return usageFromClaude(result.Usage)
	}

	if len(result.ModelUsage) == 0 {
		return nil
	}

	var inputTokens, outputTokens, cachedReadTokens, cachedWriteTokens int
	for _, usage := range result.ModelUsage {
		inputTokens += usage.InputTokens
		outputTokens += usage.OutputTokens
		cachedReadTokens += usage.CacheReadInputTokens
		cachedWriteTokens += usage.CacheCreationInputTokens
	}

	return acpUsage(inputTokens, outputTokens, cachedReadTokens, cachedWriteTokens, 0)
}

func usageFromClaude(usage *claude.Usage) *acp.Usage {
	return acpUsage(
		usage.InputTokens,
		usage.OutputTokens,
		usage.CachedInputTokens,
		usage.CacheCreationInputTokens,
		usage.ReasoningOutputTokens,
	)
}

func acpUsage(inputTokens, outputTokens, cachedReadTokens, cachedWriteTokens, thoughtTokens int) *acp.Usage {
	totalTokens := inputTokens + outputTokens + cachedReadTokens + cachedWriteTokens + thoughtTokens
	if totalTokens == 0 {
		return nil
	}

	result := &acp.Usage{
		InputTokens:  inputTokens,
		OutputTokens: outputTokens,
		TotalTokens:  totalTokens,
	}

	if cachedReadTokens > 0 {
		result.CachedReadTokens = &cachedReadTokens
	}

	if cachedWriteTokens > 0 {
		result.CachedWriteTokens = &cachedWriteTokens
	}

	if thoughtTokens > 0 {
		result.ThoughtTokens = &thoughtTokens
	}

	return result
}

func contextWindow(result *claude.ResultMessage) int {
	var size int
	for _, usage := range result.ModelUsage {
		if usage.ContextWindow > size {
			size = usage.ContextWindow
		}
	}

	return size
}

func modelUsageMeta(usages map[string]claude.ModelUsage) map[string]any {
	if len(usages) == 0 {
		return nil
	}

	meta := make(map[string]any, len(usages))
	for model, usage := range usages {
		modelMeta := map[string]any{
			keyInputTokens:       usage.InputTokens,
			keyOutputTokens:      usage.OutputTokens,
			keyCachedReadTokens:  usage.CacheReadInputTokens,
			keyCachedWriteTokens: usage.CacheCreationInputTokens,
		}
		if usage.ContextWindow > 0 {
			modelMeta[keyContextWindow] = usage.ContextWindow
		}

		meta[model] = modelMeta
	}

	return meta
}

func firstContextUsage(usages []*claude.ContextUsage) *claude.ContextUsage {
	for _, usage := range usages {
		if usage != nil {
			return usage
		}
	}

	return nil
}
