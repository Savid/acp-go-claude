package claudeacp

import (
	"context"
	"slices"
	"strings"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/savid/acp-go-claude/internal/mapper"
)

func (s *Session) handleHookCallback(ctx context.Context, request claude.HookRequest) (claude.HookResponse, error) {
	response := claude.HookResponse{Continue: true}
	if request.EventName != systemHookPostToolUse || request.ToolUseID == "" || s.hookHandled(request.ToolUseID) {
		return response, nil
	}

	return response, s.handlePostToolUseHook(ctx, request.ToolUseID, request.ToolName, request.ToolResponse)
}

func (s *Session) handlePostToolUseHook(
	ctx context.Context,
	toolUseID string,
	toolName string,
	toolResponse map[string]any,
) error {
	if strings.EqualFold(toolName, enterPlanModeTool) {
		if err := s.enterPlanModeFromHook(ctx); err != nil {
			return err
		}

		s.markHookHandled(toolUseID)

		return nil
	}

	if !strings.EqualFold(toolName, "Edit") && !strings.EqualFold(toolName, "Write") {
		return nil
	}

	if toolResponse == nil {
		return nil
	}

	content, locations := mapper.DiffToolResultContent(toolResponse)
	if len(content) == 0 && len(locations) == 0 {
		return nil
	}

	update := acp.UpdateToolCall(
		acp.ToolCallId(toolUseID),
		acp.WithUpdateContent(content),
		acp.WithUpdateLocations(locations),
		acp.WithUpdateRawOutput(toolResponse),
	)
	update.ToolCallUpdate.Meta = map[string]any{
		claudeMetaKey: map[string]any{
			permissionUpdateToolName: toolName,
			claudeMetaToolResponse:   toolResponse,
		},
	}

	if err := s.emitOptionalUpdates(ctx, []acp.SessionUpdate{update}); err != nil {
		return err
	}

	s.markHookHandled(toolUseID)

	return nil
}

func (s *Session) hookHandled(toolUseID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, ok := s.handledHooks[toolUseID]

	return ok
}

func (s *Session) markHookHandled(toolUseID string) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.handledHooks == nil {
		s.handledHooks = map[string]struct{}{}
	}

	if _, ok := s.handledHooks[toolUseID]; ok {
		return
	}

	s.handledHooks[toolUseID] = struct{}{}
	s.handledHookOrder = append(s.handledHookOrder, toolUseID)

	if len(s.handledHookOrder) > maxHandledHooks {
		oldest := s.handledHookOrder[0]
		s.handledHookOrder = slices.Delete(s.handledHookOrder, 0, 1)
		delete(s.handledHooks, oldest)
	}
}

func (s *Session) enterPlanModeFromHook(ctx context.Context) error {
	s.setMode(modePlan)

	return s.emitOptionalUpdates(ctx, []acp.SessionUpdate{
		{CurrentModeUpdate: &acp.SessionCurrentModeUpdate{CurrentModeId: modePlan}},
	})
}

func promptFinishedBySystemIdle(msg claude.Message) bool {
	system, ok := msg.(*claude.SystemMessage)
	if !ok || system.Subtype != systemSubtypeSessionStateChanged {
		return false
	}

	return systemString(system, systemState) == systemStateIdle
}

func systemString(msg *claude.SystemMessage, key string) string {
	value, _ := msg.Raw[key].(string)

	return value
}

func systemMap(msg *claude.SystemMessage, key string) map[string]any {
	value, _ := msg.Raw[key].(map[string]any)

	return value
}

func elicitationIDFromSystem(msg *claude.SystemMessage) string {
	id, _ := msg.Raw["elicitation_id"].(string)

	return id
}
