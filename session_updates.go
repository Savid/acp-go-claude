package claudeacp

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/savid/acp-go-claude/internal/mapper"
	"github.com/savid/acp-go-claude/internal/transcript"
)

var (
	errAgentClosed              = errors.New("ACP agent is closed")
	errACPConnectionNotAttached = errors.New("ACP connection is not attached")
)

const liveSessionTitleMaxRunes = 256

func (s *Session) emitUpdates(ctx context.Context, updates []acp.SessionUpdate) error {
	if len(updates) == 0 {
		return nil
	}

	s.agent.observe.ObserveFirstPromptUpdate(ctx)

	s.agent.mu.Lock()
	if s.agent.closed {
		s.agent.mu.Unlock()

		return errAgentClosed
	}

	conn := s.agent.conn
	s.agent.mu.Unlock()

	if conn == nil {
		return errACPConnectionNotAttached
	}

	for _, update := range updates {
		if err := conn.SessionUpdate(ctx, acp.SessionNotification{
			SessionId: s.id,
			Update:    update,
		}); err != nil {
			return err
		}
	}

	return nil
}

func (s *Session) emitOptionalUpdates(ctx context.Context, updates []acp.SessionUpdate) error {
	if len(updates) == 0 {
		return nil
	}

	err := s.emitUpdates(ctx, updates)
	if errors.Is(err, errAgentClosed) || errors.Is(err, errACPConnectionNotAttached) {
		return nil
	}

	return err
}

func (s *Session) emitLiveSessionInfoUpdate(ctx context.Context, prompt []acp.ContentBlock) error {
	updatedAt := time.Now().UTC().Format(time.RFC3339)
	update := acp.SessionSessionInfoUpdate{UpdatedAt: &updatedAt}
	title := liveSessionTitleFromPrompt(prompt)

	s.mu.Lock()

	s.updatedAt = updatedAt
	if s.title == "" && title != "" {
		s.title = title
		update.Title = &title
	}
	s.mu.Unlock()

	return s.emitOptionalUpdates(ctx, []acp.SessionUpdate{{SessionInfoUpdate: &update}})
}

func (s *Session) sessionInfo(id acp.SessionId) acp.SessionInfo {
	s.mu.Lock()
	title := s.title
	updatedAt := s.updatedAt
	s.mu.Unlock()

	if title == "" {
		title = string(id)
	}

	info := acp.SessionInfo{
		SessionId:             id,
		Title:                 &title,
		Cwd:                   s.cwd,
		AdditionalDirectories: append([]string(nil), s.additionalDirectories...),
	}
	if updatedAt != "" {
		info.UpdatedAt = &updatedAt
	}

	if goal := s.goalSummaryMetaValue(); goal != nil {
		info.Meta = map[string]any{
			claudeMetaKey: map[string]any{
				claudeGoalMetaKey: goal,
			},
		}
	}

	return info
}

func liveSessionTitleFromPrompt(prompt []acp.ContentBlock) string {
	for _, block := range prompt {
		if block.Text == nil {
			continue
		}

		if title := normalizeLiveSessionTitle(block.Text.Text); title != "" {
			return title
		}
	}

	return ""
}

func normalizeLiveSessionTitle(text string) string {
	title := strings.Join(strings.Fields(text), " ")
	if title == "" {
		return ""
	}

	if utf8.RuneCountInString(title) <= liveSessionTitleMaxRunes {
		return title
	}

	runes := []rune(title)

	return strings.TrimSpace(string(runes[:liveSessionTitleMaxRunes-3])) + "..."
}

func (s *Session) emitRawClaudeMessage(ctx context.Context, msg claude.Message) error {
	raw := rawClaudeMessage(msg)
	if !s.rawMessages.ShouldEmit(raw) {
		return nil
	}

	s.agent.mu.Lock()
	if s.agent.closed || s.agent.conn == nil {
		s.agent.mu.Unlock()

		return nil
	}

	conn := s.agent.conn
	s.agent.mu.Unlock()

	payload := map[string]any{
		acpFieldSessionID: s.id,
		jsonFieldMessage:  raw,
	}

	if rawJSON := rawClaudeJSON(msg); rawJSON != "" {
		payload["rawJSON"] = rawJSON
	}

	if err := conn.NotifyExtension(ctx, rawClaudeSDKMessageMethod, payload); err != nil {
		s.agent.observe.RecordRawMessageEmitFailure(ctx, err)

		return err
	}

	return nil
}

func resultOriginKind(result *claude.ResultMessage) string {
	if result == nil {
		return ""
	}

	kind, _ := result.Origin["kind"].(string)

	return kind
}

func mergeUsage(total *acp.Usage, next *acp.Usage) *acp.Usage {
	if next == nil {
		return cloneUsage(total)
	}

	if total == nil {
		return cloneUsage(next)
	}

	merged := &acp.Usage{
		InputTokens:  total.InputTokens + next.InputTokens,
		OutputTokens: total.OutputTokens + next.OutputTokens,
		TotalTokens:  total.TotalTokens + next.TotalTokens,
	}

	if value := optionalIntSum(total.CachedReadTokens, next.CachedReadTokens); value != nil {
		merged.CachedReadTokens = value
	}

	if value := optionalIntSum(total.CachedWriteTokens, next.CachedWriteTokens); value != nil {
		merged.CachedWriteTokens = value
	}

	if value := optionalIntSum(total.ThoughtTokens, next.ThoughtTokens); value != nil {
		merged.ThoughtTokens = value
	}

	return merged
}

func cloneUsage(usage *acp.Usage) *acp.Usage {
	if usage == nil {
		return nil
	}

	cloned := *usage
	if usage.CachedReadTokens != nil {
		cloned.CachedReadTokens = acp.Ptr(*usage.CachedReadTokens)
	}

	if usage.CachedWriteTokens != nil {
		cloned.CachedWriteTokens = acp.Ptr(*usage.CachedWriteTokens)
	}

	if usage.ThoughtTokens != nil {
		cloned.ThoughtTokens = acp.Ptr(*usage.ThoughtTokens)
	}

	return &cloned
}

func optionalIntSum(left *int, right *int) *int {
	if left == nil && right == nil {
		return nil
	}

	value := 0
	if left != nil {
		value += *left
	}

	if right != nil {
		value += *right
	}

	return &value
}

func (s *Session) replayTranscript(ctx context.Context, path string) error {
	updates, truncated, err := transcript.ReplayUpdates(path)
	if err != nil {
		return err
	}

	if truncated {
		updates = append([]acp.SessionUpdate{
			acp.UpdateAgentMessageText("Claude transcript replay was truncated; only the first 10000 ACP updates were replayed."),
		}, updates...)
	}

	return s.emitUpdates(ctx, updates)
}

func (s *Session) emitMessageSideEffects(ctx context.Context, msg claude.Message) error {
	system, ok := msg.(*claude.SystemMessage)
	if !ok {
		return nil
	}

	switch system.Subtype {
	case systemStatus:
		if systemString(system, systemStatus) == systemStatusCompacting {
			return s.emitOptionalUpdates(ctx, []acp.SessionUpdate{acp.UpdateAgentMessageText(compactingMessageText)})
		}

		return nil
	case systemSubtypeCompactBoundary:
		return s.emitOptionalUpdates(ctx, []acp.SessionUpdate{
			{
				UsageUpdate: &acp.SessionUsageUpdate{
					Size: s.currentContextWindow(),
					Used: 0,
				},
			},
			acp.UpdateAgentMessageText(compactingCompleteMessageText),
		})
	case systemSubtypeLocalCommandOutput:
		if content := systemString(system, systemContent); content != "" {
			return s.emitOptionalUpdates(ctx, []acp.SessionUpdate{acp.UpdateAgentMessageText(content)})
		}

		return nil
	case elicitationComplete:
		return s.emitElicitationComplete(ctx, system)
	default:
		return nil
	}
}

func (s *Session) emitElicitationComplete(ctx context.Context, system *claude.SystemMessage) error {
	caps := s.agent.clientElicitationCapabilities()
	if caps == nil || caps.Url == nil {
		return nil
	}

	elicitationID := elicitationIDFromSystem(system)
	if elicitationID == "" {
		return nil
	}

	conn := s.agent.connection()
	if conn == nil {
		return nil
	}

	return conn.UnstableCompleteElicitation(ctx, acp.UnstableCompleteElicitationNotification{
		ElicitationId: acp.UnstableElicitationId(elicitationID),
	})
}

func (s *Session) emitHookResponseUpdates(ctx context.Context, msg claude.Message, options mapper.ToolUpdateOptions) error {
	system, ok := msg.(*claude.SystemMessage)
	if !ok || system.Subtype != systemSubtypeHookResponse {
		return nil
	}

	if systemString(system, systemHookEventName) != systemHookPostToolUse {
		return nil
	}

	toolUseID := systemString(system, systemToolUseID)
	if toolUseID == "" {
		return nil
	}

	if s.hookHandled(toolUseID) {
		return nil
	}

	toolUse := options.ToolUses[toolUseID]

	return s.handlePostToolUseHook(ctx, toolUseID, toolUse.Name, systemMap(system, systemToolResponse))
}
