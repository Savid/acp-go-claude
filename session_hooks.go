package claudeacp

import (
	"context"
	"slices"
	"strings"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/savid/acp-go-claude/internal/mapper"
)

func (s *agentSession) handleHookCallback(ctx context.Context, request claude.HookRequest) (claude.HookResponse, error) {
	response := claude.HookResponse{Continue: true}
	if request.EventName != systemHookPostToolUse || request.ToolUseID == "" || s.hookHandled(request.ToolUseID) {
		return response, nil
	}

	return response, s.handlePostToolUseHook(ctx, request.ToolUseID, request.ToolName, request.ToolResponse)
}

func (s *agentSession) handlePostToolUseHook(
	ctx context.Context,
	toolUseID string,
	toolName string,
	toolResponse map[string]any,
) error {
	return s.handleOwnedPostToolUse(ctx, toolUseID, toolName, toolResponse, func(emit func() error) error {
		return s.emitControlCallbackContent(ctx, emit)
	})
}

// handlePostToolUseFrame projects a typed native frame through the owner the
// pump already resolved. It is deliberately separate from the control callback
// path: a route-bearing frame is not a registered callback admission.
func (s *agentSession) handlePostToolUseFrame(
	ctx context.Context,
	toolUseID string,
	toolName string,
	toolResponse map[string]any,
) error {
	return s.handleOwnedPostToolUse(ctx, toolUseID, toolName, toolResponse, func(emit func() error) error {
		return emit()
	})
}

func (s *agentSession) handleOwnedPostToolUse(
	ctx context.Context,
	toolUseID string,
	toolName string,
	toolResponse map[string]any,
	emitOwned func(func() error) error,
) error {
	if strings.EqualFold(toolName, enterPlanModeTool) {
		if err := emitOwned(func() error {
			return s.enterPlanModeFromHook(ctx)
		}); err != nil {
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

	if err := emitOwned(func() error {
		return s.emitUpdates(ctx, []acp.SessionUpdate{update})
	}); err != nil {
		return err
	}

	s.markHookHandled(toolUseID)

	return nil
}

func (s *agentSession) emitControlCallbackContent(ctx context.Context, emit func() error) error {
	admission := controlCallbackAdmissionFromContext(ctx)
	if admission == nil || admission.session != s || admission.route == "" {
		return lifecycleViolationError("callback has no exact registered admission")
	}

	s.callbackOwnershipMu.Lock()
	s.mu.Lock()
	closing := s.closing
	s.mu.Unlock()

	if closing {
		s.callbackOwnershipMu.Unlock()

		return closedSessionError()
	}

	if _, live := s.callbackAdmissions[admission]; !live {
		s.callbackOwnershipMu.Unlock()

		return lifecycleViolationError("callback admission is no longer live")
	}

	incarnation := admission.incarnation
	if incarnation != nil &&
		(incarnation.failed.Load() || !s.nativePumpHandle().serves(incarnation)) {
		s.callbackOwnershipMu.Unlock()

		return lifecycleViolationError("callback native incarnation is no longer current")
	}

	ownedIncarnation, err := s.lifecycleStream().emitCallbackContent(ctx, admission.route, emit)
	s.callbackOwnershipMu.Unlock()

	if err != nil {
		if ownedIncarnation == nil {
			ownedIncarnation = incarnation
		}

		if ownedIncarnation != nil {
			s.failNativeIncarnation(ctx, ownedIncarnation, err, "hook_projection")
		}
	}

	return err
}

func (s *agentSession) hookHandled(toolUseID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()

	_, ok := s.handledHooks[toolUseID]

	return ok
}

func (s *agentSession) markHookHandled(toolUseID string) {
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

func (s *agentSession) enterPlanModeFromHook(ctx context.Context) error {
	s.setMode(modePlan)

	return s.emitUpdates(ctx, []acp.SessionUpdate{
		{ConfigOptionUpdate: &acp.SessionConfigOptionUpdate{ConfigOptions: sessionConfigOptions(s)}},
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
