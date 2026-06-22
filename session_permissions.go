package claudeacp

import (
	"context"
	"errors"
	"log/slog"
	"maps"
	"strings"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/savid/acp-go-claude/internal/mapper"
	"github.com/savid/acp-go-claude/internal/observer"
)

func (s *Session) permissionRule(toolName string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	behavior, ok := s.permissionRules[toolName]

	return behavior, ok
}

func (s *Session) setPermissionRule(ctx context.Context, toolName string, behavior string) {
	if toolName == "" {
		return
	}

	s.permissionSaveMu.Lock()
	defer s.permissionSaveMu.Unlock()

	s.mu.Lock()
	if s.permissionRules == nil {
		s.permissionRules = make(map[string]string)
	}

	s.permissionRules[toolName] = behavior
	rules := s.clonePermissionRulesLocked()
	s.mu.Unlock()

	err := savePermissionRules(ctx, s.agent.options.ClaudeHome, s.id, rules)
	if err == nil {
		s.agent.cachePermissionRules(s.id, rules)
	}

	if err != nil {
		s.agent.log.WarnContext(ctx, "save permission rules failed", slog.String(jsonFieldError, err.Error()))

		return
	}
}

func (s *Session) clonePermissionRules() map[string]string {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.clonePermissionRulesLocked()
}

func (s *Session) clonePermissionRulesLocked() map[string]string {
	rules := make(map[string]string, len(s.permissionRules))
	maps.Copy(rules, s.permissionRules)

	return rules
}

func (s *Session) persistPermissionRules(ctx context.Context) {
	s.permissionSaveMu.Lock()
	defer s.permissionSaveMu.Unlock()

	s.mu.Lock()
	rules := s.clonePermissionRulesLocked()
	s.mu.Unlock()

	err := savePermissionRules(ctx, s.agent.options.ClaudeHome, s.id, rules)
	if err == nil {
		s.agent.cachePermissionRules(s.id, rules)
	}

	if err != nil {
		s.agent.log.WarnContext(ctx, "save permission rules failed", slog.String(jsonFieldError, err.Error()))

		return
	}
}

func (s *Session) handlePermission(ctx context.Context, request claude.PermissionRequest) (decision claude.PermissionDecision, err error) {
	if request.ToolName == askUserQuestionTool {
		return s.handleAskUserQuestion(ctx, request)
	}

	mode, _, _ := s.modeInfo()

	ctx, finish := s.agent.observe.StartPermission(ctx, request.ToolName, string(mode))
	defer func() {
		finish(observer.PermissionResult{
			Behavior: decision.Behavior,
			Err:      err,
			Mode:     string(mode),
			ToolName: request.ToolName,
		})
	}()

	if request.ToolName == exitPlanModeTool {
		return s.handleExitPlanMode(ctx, request)
	}

	if behavior, ok := s.permissionRule(request.ToolName); ok {
		if behavior == claude.BehaviorAllow {
			return claude.PermissionDecision{Behavior: claude.BehaviorAllow, UpdatedInput: request.Input}, nil
		}

		return claude.PermissionDecision{Behavior: claude.BehaviorDeny, Message: "Denied by saved ACP permission rule"}, nil
	}

	conn := s.agent.connection()
	if conn == nil {
		return claude.PermissionDecision{Behavior: claude.BehaviorDeny, Message: "ACP client is unavailable"}, nil
	}

	toolCallID := request.ToolUseID
	if toolCallID == "" {
		toolCallID = request.ToolName
	}

	permissionCtx, finishPermissionRequest := s.permissionRequestContext(ctx, toolCallID)
	defer finishPermissionRequest()

	title := request.Title
	if title == "" {
		title = mapper.ToolTitle(request.ToolName, request.Input)
	}

	info := mapper.ToolCallInfo(request.ToolName, toolCallID, request.Input, mapper.ToolUpdateOptions{
		Cwd:                    s.cwd,
		SupportsTerminalOutput: s.agent.clientSupportsTerminalOutput(),
	})
	status := acp.ToolCallStatusPending
	kind := info.Kind

	resp, err := conn.RequestPermission(permissionCtx, acp.RequestPermissionRequest{
		SessionId: s.id,
		ToolCall: acp.ToolCallUpdate{
			ToolCallId: acp.ToolCallId(toolCallID),
			Title:      &title,
			Kind:       &kind,
			Status:     &status,
			Content:    info.Content,
			Locations:  info.Locations,
			RawInput:   request.Input,
			Meta:       map[string]any{acpMetaKey: request.Raw},
		},
		Options: []acp.PermissionOption{
			{OptionId: permissionAllowOnce, Name: "Allow once", Kind: acp.PermissionOptionKindAllowOnce},
			{OptionId: permissionAllowAlways, Name: describeAlwaysAllow(request.Suggestions, request.ToolName), Kind: acp.PermissionOptionKindAllowAlways},
			{OptionId: permissionRejectOnce, Name: "Reject once", Kind: acp.PermissionOptionKindRejectOnce},
			{OptionId: permissionRejectAlways, Name: "Reject always", Kind: acp.PermissionOptionKindRejectAlways},
		},
	})
	if err != nil {
		if permissionRequestCancelled(err) && s.wasTurnCancelled() {
			return claude.PermissionDecision{
				Behavior:  claude.BehaviorDeny,
				Message:   permissionCancelledMessage,
				Interrupt: true,
			}, nil
		}

		return claude.PermissionDecision{}, err
	}

	if resp.Outcome.Selected == nil {
		return claude.PermissionDecision{Behavior: claude.BehaviorDeny, Message: permissionCancelledMessage}, nil
	}

	switch resp.Outcome.Selected.OptionId {
	case permissionAllowOnce:
		return claude.PermissionDecision{Behavior: claude.BehaviorAllow, UpdatedInput: request.Input}, nil
	case permissionAllowAlways:
		if len(request.Suggestions) == 0 {
			s.setPermissionRule(ctx, request.ToolName, claude.BehaviorAllow)
		}

		return claude.PermissionDecision{
			Behavior:           claude.BehaviorAllow,
			UpdatedInput:       request.Input,
			UpdatedPermissions: permissionSuggestionsForAllowAlways(request.ToolName, request.Suggestions, permissionUpdate(request.ToolName, claude.BehaviorAllow)),
		}, nil
	case permissionRejectOnce:
		return claude.PermissionDecision{Behavior: claude.BehaviorDeny, Message: permissionRejectedMessage}, nil
	case permissionRejectAlways:
		s.setPermissionRule(ctx, request.ToolName, claude.BehaviorDeny)

		return claude.PermissionDecision{
			Behavior:           claude.BehaviorDeny,
			Message:            permissionRejectedMessage,
			UpdatedPermissions: []map[string]any{permissionUpdate(request.ToolName, claude.BehaviorDeny)},
		}, nil
	default:
		return claude.PermissionDecision{Behavior: claude.BehaviorDeny, Message: "Unknown permission option selected"}, nil
	}
}

func (s *Session) permissionRequestContext(ctx context.Context, id string) (context.Context, context.CancelFunc) {
	if id == "" {
		return ctx, func() {}
	}

	permissionCtx, cancel := context.WithCancel(ctx)
	entry := &permissionRequestCancel{cancel: cancel}

	s.mu.Lock()
	if s.permissionCancel == nil {
		s.permissionCancel = make(map[string]*permissionRequestCancel)
	}

	s.permissionCancel[id] = entry
	turnCancelled := s.turnCancelled
	s.mu.Unlock()

	if turnCancelled {
		cancel()
	}

	return permissionCtx, func() {
		s.mu.Lock()
		if s.permissionCancel[id] == entry {
			delete(s.permissionCancel, id)
		}
		s.mu.Unlock()

		cancel()
	}
}

func permissionRequestCancelled(err error) bool {
	if errors.Is(err, context.Canceled) {
		return true
	}

	var requestErr *acp.RequestError
	if !errors.As(err, &requestErr) {
		return false
	}

	return requestErr.Code == acp.NewRequestCancelled(nil).Code
}

type permissionRequestCancel struct {
	cancel context.CancelFunc
}

func (s *Session) handleExitPlanMode(
	ctx context.Context,
	request claude.PermissionRequest,
) (claude.PermissionDecision, error) {
	conn := s.agent.connection()
	if conn == nil {
		return claude.PermissionDecision{Behavior: claude.BehaviorDeny, Message: "ACP client is unavailable"}, nil
	}

	toolCallID := request.ToolUseID
	if toolCallID == "" {
		toolCallID = request.ToolName
	}

	_, model, availableModels := s.modeInfo()
	options := exitPlanModeOptions(model, availableModels)
	info := mapper.ToolCallInfo(request.ToolName, toolCallID, request.Input, mapper.ToolUpdateOptions{
		Cwd:                    s.cwd,
		SupportsTerminalOutput: s.agent.clientSupportsTerminalOutput(),
	})
	status := acp.ToolCallStatusPending
	title := info.Title

	if request.Title != "" {
		title = request.Title
	}

	resp, err := conn.RequestPermission(ctx, acp.RequestPermissionRequest{
		SessionId: s.id,
		ToolCall: acp.ToolCallUpdate{
			ToolCallId: acp.ToolCallId(toolCallID),
			Title:      &title,
			Kind:       &info.Kind,
			Status:     &status,
			Content:    info.Content,
			Locations:  info.Locations,
			RawInput:   request.Input,
			Meta:       map[string]any{acpMetaKey: request.Raw},
		},
		Options: options,
	})
	if err != nil {
		return claude.PermissionDecision{}, err
	}

	if resp.Outcome.Selected == nil {
		return claude.PermissionDecision{Behavior: claude.BehaviorDeny, Message: permissionCancelledMessage}, nil
	}

	selectedMode := acp.SessionModeId(resp.Outcome.Selected.OptionId)
	if !exitPlanModeSelectionAllows(selectedMode, options) {
		return claude.PermissionDecision{
			Behavior: claude.BehaviorDeny,
			Message:  "User rejected request to exit plan mode.",
		}, nil
	}

	s.setMode(selectedMode)

	if err := s.emitOptionalUpdates(ctx, []acp.SessionUpdate{
		{ConfigOptionUpdate: &acp.SessionConfigOptionUpdate{ConfigOptions: sessionConfigOptions(s)}},
	}); err != nil {
		return claude.PermissionDecision{}, err
	}

	return claude.PermissionDecision{
		Behavior:           claude.BehaviorAllow,
		UpdatedInput:       request.Input,
		UpdatedPermissions: permissionSuggestionsOrFallback(request.Suggestions, permissionModeUpdate(selectedMode)),
	}, nil
}

func exitPlanModeOptions(model string, availableModels []claude.AvailableModelInfo) []acp.PermissionOption {
	candidates := []acp.PermissionOption{
		{
			OptionId: acp.PermissionOptionId(modeBypassPermissions),
			Name:     "Yes, and bypass permissions",
			Kind:     acp.PermissionOptionKindAllowAlways,
		},
		{
			OptionId: acp.PermissionOptionId(modeAuto),
			Name:     `Yes, and use "auto" mode`,
			Kind:     acp.PermissionOptionKindAllowAlways,
		},
		{
			OptionId: acp.PermissionOptionId(modeAcceptEdits),
			Name:     "Yes, and auto-accept edits",
			Kind:     acp.PermissionOptionKindAllowAlways,
		},
		{
			OptionId: acp.PermissionOptionId(modeDefault),
			Name:     "Yes, and manually approve edits",
			Kind:     acp.PermissionOptionKindAllowOnce,
		},
		{
			OptionId: acp.PermissionOptionId(modePlan),
			Name:     "No, keep planning",
			Kind:     acp.PermissionOptionKindRejectOnce,
		},
	}

	options := make([]acp.PermissionOption, 0, len(candidates))
	for _, option := range candidates {
		if modeAvailableForModel(acp.SessionModeId(option.OptionId), model, availableModels) {
			options = append(options, option)
		}
	}

	return options
}

func exitPlanModeSelectionAllows(selected acp.SessionModeId, options []acp.PermissionOption) bool {
	switch selected {
	case modeDefault, modeAcceptEdits, modeAuto, modeBypassPermissions:
	default:
		return false
	}

	for _, option := range options {
		if option.OptionId == acp.PermissionOptionId(selected) {
			return true
		}
	}

	return false
}

func permissionUpdate(toolName string, behavior string) map[string]any {
	return map[string]any{
		jsonFieldType:               permissionUpdateAddRules,
		permissionUpdateBehavior:    behavior,
		permissionUpdateDestination: permissionUpdateSession,
		permissionUpdateRules: []map[string]any{
			{permissionUpdateToolName: toolName},
		},
	}
}

func permissionSuggestionsOrFallback(suggestions []map[string]any, fallback map[string]any) []map[string]any {
	if len(suggestions) > 0 {
		return suggestions
	}

	return []map[string]any{fallback}
}

func permissionSuggestionsForAllowAlways(toolName string, suggestions []map[string]any, fallback map[string]any) []map[string]any {
	if !strings.EqualFold(toolName, workflowTool) || len(suggestions) == 0 {
		return permissionSuggestionsOrFallback(suggestions, fallback)
	}

	normalized := make([]map[string]any, 0, len(suggestions))
	for _, suggestion := range suggestions {
		normalized = append(normalized, normalizeWorkflowPermissionSuggestion(suggestion))
	}

	return normalized
}

func normalizeWorkflowPermissionSuggestion(suggestion map[string]any) map[string]any {
	cloned, _ := clonePermissionSuggestionValue(suggestion).(map[string]any)

	if stringValue(cloned[jsonFieldType]) != permissionUpdateAddRules ||
		stringValue(cloned[permissionUpdateBehavior]) != claude.BehaviorAllow ||
		stringValue(cloned[permissionUpdateDestination]) != permissionUpdateLocalSettings {
		return cloned
	}

	for _, rule := range permissionRuleMaps(cloned[permissionUpdateRules]) {
		if workflowPermissionRuleName(stringValue(rule[permissionUpdateToolName])) {
			cloned[permissionUpdateDestination] = permissionUpdateSession

			break
		}
	}

	return cloned
}

func workflowPermissionRuleName(toolName string) bool {
	return strings.HasPrefix(toolName, workflowTool+"(")
}

func permissionRuleMaps(value any) []map[string]any {
	switch typed := value.(type) {
	case []map[string]any:
		return append([]map[string]any(nil), typed...)
	default:
		return mapSliceAny(value)
	}
}

func clonePermissionSuggestionValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		cloned := make(map[string]any, len(typed))
		for key, value := range typed {
			cloned[key] = clonePermissionSuggestionValue(value)
		}

		return cloned
	case []any:
		cloned := make([]any, len(typed))
		for i, value := range typed {
			cloned[i] = clonePermissionSuggestionValue(value)
		}

		return cloned
	case []map[string]any:
		cloned := make([]map[string]any, len(typed))
		for i, value := range typed {
			cloned[i], _ = clonePermissionSuggestionValue(value).(map[string]any)
		}

		return cloned
	case []string:
		return append([]string(nil), typed...)
	default:
		return typed
	}
}

func describeAlwaysAllow(suggestions []map[string]any, toolName string) string {
	if len(suggestions) == 0 {
		return "Always Allow all " + toolName
	}

	ruleLabels := make([]string, 0, len(suggestions))
	directories := make([]string, 0, len(suggestions))

	for _, suggestion := range suggestions {
		switch stringValue(suggestion[jsonFieldType]) {
		case permissionUpdateAddRules:
			if stringValue(suggestion[permissionUpdateBehavior]) != claude.BehaviorAllow {
				continue
			}

			for _, rule := range mapSliceAny(suggestion[permissionUpdateRules]) {
				ruleTool := stringValue(rule[permissionUpdateToolName])
				if ruleTool == "" {
					continue
				}

				if content := stringValue(rule[permissionUpdateRuleContent]); content != "" {
					ruleLabels = append(ruleLabels, ruleTool+"("+content+")")
				} else {
					ruleLabels = append(ruleLabels, "all "+ruleTool)
				}
			}
		case permissionUpdateAddDirs:
			directories = append(directories, stringSliceValue(suggestion[permissionUpdateDirectories])...)
		}
	}

	parts := make([]string, 0, 2)
	if len(ruleLabels) > 0 {
		parts = append(parts, strings.Join(ruleLabels, ", "))
	}

	if len(directories) > 0 {
		parts = append(parts, "access to "+strings.Join(directories, ", "))
	}

	if len(parts) == 0 {
		return "Always Allow all " + toolName
	}

	return "Always Allow " + strings.Join(parts, " and ")
}

func mapSliceAny(value any) []map[string]any {
	values, _ := value.([]any)

	result := make([]map[string]any, 0, len(values))
	for _, value := range values {
		raw, _ := value.(map[string]any)
		if raw != nil {
			result = append(result, raw)
		}
	}

	return result
}

func permissionModeUpdate(mode acp.SessionModeId) map[string]any {
	return map[string]any{
		jsonFieldType:               permissionUpdateSetMode,
		jsonFieldMode:               string(mode),
		permissionUpdateDestination: permissionUpdateSession,
	}
}
