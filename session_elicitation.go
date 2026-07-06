package claudeacp

import (
	"context"
	"fmt"
	"maps"
	"strings"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/savid/acp-go-claude/internal/observer"
)

type elicitationRequestCancel struct {
	cancel context.CancelFunc
}

// registerElicitation wraps ctx in a tracked cancellable context so that
// session/cancel and teardown can resolve a pending elicitation as cancelled
// instead of leaving it dangling until the native control-handler timeout.
func (s *agentSession) registerElicitation(ctx context.Context) (context.Context, context.CancelFunc) {
	elicitationCtx, cancel := context.WithCancel(ctx)
	entry := &elicitationRequestCancel{cancel: cancel}

	s.mu.Lock()
	if s.elicitationCancel == nil {
		s.elicitationCancel = make(map[int64]*elicitationRequestCancel)
	}

	s.elicitationSeq++
	id := s.elicitationSeq
	s.elicitationCancel[id] = entry
	turnCancelled := s.turnCancelled
	s.mu.Unlock()

	if turnCancelled {
		cancel()
	}

	return elicitationCtx, func() {
		s.mu.Lock()
		if s.elicitationCancel[id] == entry {
			delete(s.elicitationCancel, id)
		}
		s.mu.Unlock()

		cancel()
	}
}

func (s *agentSession) handleAskUserQuestion(
	ctx context.Context,
	request claude.PermissionRequest,
) (decision claude.PermissionDecision, err error) {
	ctx, finish := s.agent.observe.StartElicitation(ctx)
	defer func() {
		finish(observer.ElicitationResult{
			Accepted: decision.Behavior == claude.BehaviorAllow,
			Err:      err,
		})
	}()

	conn := s.agent.connection()

	if conn == nil || !s.agent.clientSupportsFormElicitation() {
		return claude.PermissionDecision{
			Behavior: claude.BehaviorDeny,
			Message:  "AskUserQuestion requires ACP form elicitation support",
		}, nil
	}

	questions, parseMessage := parseAskUserQuestions(request.Input)
	if parseMessage != "" {
		return claude.PermissionDecision{
			Behavior: claude.BehaviorDeny,
			Message:  "AskUserQuestion parse error: " + parseMessage,
		}, nil
	}

	scope := elicitationScope{SessionID: s.id}
	if request.ToolUseID != "" {
		scope.ToolCallID = acp.ToolCallId(request.ToolUseID)
	}

	elicitationCtx, finishElicitation := s.registerElicitation(ctx)
	defer finishElicitation()

	resp, err := conn.CreateElicitation(elicitationCtx, acp.UnstableCreateElicitationRequest{
		Form: &acp.UnstableCreateElicitationForm{
			Message:         askUserQuestionMessage(questions),
			Mode:            claude.ElicitationModeForm,
			RequestedSchema: askUserQuestionSchema(questions),
			Meta: map[string]any{
				claudeMetaKey: map[string]any{
					"toolName":  askUserQuestionTool,
					acpFieldRaw: request.Raw,
				},
			},
		},
	}, scope)
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

	if resp.Accept == nil {
		return claude.PermissionDecision{
			Behavior: claude.BehaviorDeny,
			Message:  "AskUserQuestion declined by ACP client",
		}, nil
	}

	updatedInput, applyMessage := applyAskUserAnswers(request.Input, questions, resp.Accept.Content)
	if applyMessage != "" {
		return claude.PermissionDecision{
			Behavior: claude.BehaviorDeny,
			Message:  "AskUserQuestion invalid response: " + applyMessage,
		}, nil
	}

	return claude.PermissionDecision{
		Behavior:     claude.BehaviorAllow,
		UpdatedInput: updatedInput,
	}, nil
}

func parseAskUserQuestions(input map[string]any) ([]askUserQuestion, string) {
	if input == nil {
		return nil, "missing tool input"
	}

	rawQuestions, ok := input[askFieldQuestions].([]any)
	if !ok || len(rawQuestions) == 0 {
		return nil, "missing questions"
	}

	questions := make([]askUserQuestion, 0, len(rawQuestions))
	for i, rawQuestion := range rawQuestions {
		questionMap, _ := rawQuestion.(map[string]any)
		if questionMap == nil {
			continue
		}

		question := askUserQuestion{}

		question.ID, _ = questionMap[askFieldID].(string)
		if question.ID == "" {
			question.ID = fmt.Sprintf("question_%d", i+1)
		}

		question.Question, _ = questionMap[askFieldQuestion].(string)
		question.Header, _ = questionMap[askFieldHeader].(string)

		question.MultiSelect, _ = questionMap[askFieldMultiSelect].(bool)
		question.IsOther, _ = questionMap[askFieldIsOther].(bool)
		question.IsSecret, _ = questionMap[askFieldIsSecret].(bool)
		question.Options = parseAskUserOptions(questionMap[askFieldOptions])
		questions = append(questions, question)
	}

	if len(questions) == 0 {
		return nil, "no parseable questions"
	}

	return questions, ""
}

func parseAskUserOptions(raw any) []askUserQuestionOption {
	rawOptions, _ := raw.([]any)

	options := make([]askUserQuestionOption, 0, len(rawOptions))
	for _, rawOption := range rawOptions {
		optionMap, _ := rawOption.(map[string]any)
		if optionMap == nil {
			continue
		}

		option := askUserQuestionOption{}
		option.Label, _ = optionMap["label"].(string)
		option.Description, _ = optionMap[askFieldDescription].(string)

		if option.Label != "" {
			options = append(options, option)
		}
	}

	return options
}

func askUserQuestionMessage(questions []askUserQuestion) string {
	if len(questions) == 1 && questions[0].Question != "" {
		return questions[0].Question
	}

	return "Claude needs more input."
}

func askUserQuestionSchema(questions []askUserQuestion) acp.UnstableElicitationSchema {
	schema := acp.UnstableElicitationSchema{
		Title:      acp.Ptr("Claude question"),
		Properties: make(map[string]any, len(questions)),
		Required:   make([]string, 0, len(questions)),
		Type:       acp.UnstableElicitationSchemaTypeObject,
	}

	for _, question := range questions {
		schema.Properties[question.ID] = askUserQuestionProperty(question)
		schema.Required = append(schema.Required, question.ID)
	}

	return schema
}

func askUserQuestionProperty(question askUserQuestion) map[string]any {
	property := map[string]any{
		jsonFieldType: jsonSchemaTypeString,
	}

	if title := askUserQuestionTitle(question); title != "" {
		property[jsonFieldTitle] = title
	}

	if question.Question != "" {
		property[askFieldDescription] = question.Question
	}

	if len(question.Options) == 0 {
		return property
	}

	if question.MultiSelect {
		property[jsonFieldType] = "array"
		property["minItems"] = 1
		property["items"] = map[string]any{
			jsonFieldType: jsonSchemaTypeString,
			"anyOf":       askUserQuestionOneOf(question.Options),
		}

		return property
	}

	property["oneOf"] = askUserQuestionOneOf(question.Options)

	return property
}

func askUserQuestionTitle(question askUserQuestion) string {
	if question.Header != "" {
		return question.Header
	}

	return question.Question
}

func askUserQuestionOneOf(options []askUserQuestionOption) []map[string]any {
	oneOf := make([]map[string]any, 0, len(options))
	for _, option := range options {
		item := map[string]any{
			"const":        option.Label,
			jsonFieldTitle: option.Label,
		}
		if option.Description != "" {
			item[askFieldDescription] = option.Description
		}

		oneOf = append(oneOf, item)
	}

	return oneOf
}

func applyAskUserAnswers(
	input map[string]any,
	questions []askUserQuestion,
	content map[string]any,
) (map[string]any, string) {
	if len(content) == 0 {
		return nil, "answers cannot be empty"
	}

	answers := make(map[string]any, len(questions))
	for _, question := range questions {
		answer := answerStrings(content[question.ID])
		if len(answer) > 0 {
			answers[claudeAskAnswerKey(question)] = claudeAskAnswerValue(question, answer)
		}
	}

	if len(answers) == 0 {
		return nil, "no valid answers matched question IDs"
	}

	updatedInput := make(map[string]any, len(input)+1)
	maps.Copy(updatedInput, input)
	updatedInput[askFieldAnswers] = answers

	return updatedInput, ""
}

func claudeAskAnswerKey(question askUserQuestion) string {
	if question.Question != "" {
		return question.Question
	}

	return question.ID
}

func claudeAskAnswerValue(question askUserQuestion, answers []string) string {
	if question.MultiSelect {
		return strings.Join(answers, ", ")
	}

	return answers[0]
}

func answerStrings(value any) []string {
	switch typed := value.(type) {
	case string:
		if typed == "" {
			return nil
		}

		return []string{typed}
	case []string:
		return nonEmptyStrings(typed)
	case []any:
		values := make([]string, 0, len(typed))
		for _, item := range typed {
			text, _ := item.(string)
			if text != "" {
				values = append(values, text)
			}
		}

		return values
	default:
		return nil
	}
}

func nonEmptyStrings(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" {
			result = append(result, value)
		}
	}

	return result
}

func (s *agentSession) handleElicitation(
	ctx context.Context,
	request claude.ElicitationRequest,
) (response claude.ElicitationResponse, err error) {
	ctx, finish := s.agent.observe.StartElicitation(ctx)
	defer func() {
		finish(observer.ElicitationResult{
			Accepted: response.Action == claude.ElicitationActionAccept,
			Err:      err,
		})
	}()

	conn := s.agent.connection()
	if conn == nil {
		return claude.ElicitationResponse{Action: claude.ElicitationActionDecline}, nil
	}

	switch request.Mode {
	case claude.ElicitationModeForm:
		if !s.agent.clientSupportsFormElicitation() {
			return claude.ElicitationResponse{Action: claude.ElicitationActionDecline}, nil
		}

		return s.createFormElicitation(ctx, conn, request)
	case claude.ElicitationModeURL:
		if !s.agent.clientSupportsURLElicitation() || request.URL == "" {
			return claude.ElicitationResponse{Action: claude.ElicitationActionDecline}, nil
		}

		return s.createURLElicitation(ctx, conn, request)
	default:
		return claude.ElicitationResponse{Action: claude.ElicitationActionDecline}, nil
	}
}

func (s *agentSession) createFormElicitation(
	ctx context.Context,
	conn agentClient,
	request claude.ElicitationRequest,
) (claude.ElicitationResponse, error) {
	elicitationCtx, finishElicitation := s.registerElicitation(ctx)
	defer finishElicitation()

	resp, err := conn.CreateElicitation(elicitationCtx, acp.UnstableCreateElicitationRequest{
		Form: &acp.UnstableCreateElicitationForm{
			Message:         request.Message,
			Mode:            claude.ElicitationModeForm,
			RequestedSchema: elicitationSchema(request.RequestedSchema),
			Meta:            s.elicitationMeta(request),
		},
	}, s.elicitationScope(request))
	if err != nil {
		if permissionRequestCancelled(err) && s.wasTurnCancelled() {
			return claude.ElicitationResponse{Action: claude.ElicitationActionCancel}, nil
		}

		return claude.ElicitationResponse{}, err
	}

	return claudeElicitationResponse(resp), nil
}

func (s *agentSession) createURLElicitation(
	ctx context.Context,
	conn agentClient,
	request claude.ElicitationRequest,
) (claude.ElicitationResponse, error) {
	elicitationID := request.ElicitationID
	if elicitationID == "" {
		id, err := newUUID()
		if err != nil {
			return claude.ElicitationResponse{}, err
		}

		elicitationID = id
	}

	elicitationCtx, finishElicitation := s.registerElicitation(ctx)
	defer finishElicitation()

	resp, err := conn.CreateElicitation(elicitationCtx, acp.UnstableCreateElicitationRequest{
		Url: &acp.UnstableCreateElicitationUrl{
			ElicitationId: acp.UnstableElicitationId(elicitationID),
			Message:       request.Message,
			Mode:          claude.ElicitationModeURL,
			Url:           request.URL,
			Meta:          s.elicitationMeta(request),
		},
	}, s.elicitationScope(request))
	if err != nil {
		if permissionRequestCancelled(err) && s.wasTurnCancelled() {
			return claude.ElicitationResponse{Action: claude.ElicitationActionCancel}, nil
		}

		return claude.ElicitationResponse{}, err
	}

	return claudeElicitationResponse(resp), nil
}

func (s *agentSession) elicitationMeta(request claude.ElicitationRequest) map[string]any {
	meta := map[string]any{
		acpFieldSessionID: string(s.id),
		acpFieldRaw:       request.Raw,
	}

	if request.MCPServerName != "" {
		meta["mcpServerName"] = request.MCPServerName
	}

	if request.ToolUseID != "" {
		meta["toolCallId"] = request.ToolUseID
	}

	return map[string]any{claudeMetaKey: meta}
}

func (s *agentSession) elicitationScope(request claude.ElicitationRequest) elicitationScope {
	scope := elicitationScope{SessionID: s.id}
	if request.ToolUseID != "" {
		scope.ToolCallID = acp.ToolCallId(request.ToolUseID)
	}

	return scope
}

func claudeElicitationResponse(resp acp.UnstableCreateElicitationResponse) claude.ElicitationResponse {
	switch {
	case resp.Accept != nil:
		return claude.ElicitationResponse{
			Action:  claude.ElicitationActionAccept,
			Content: resp.Accept.Content,
		}
	case resp.Cancel != nil:
		return claude.ElicitationResponse{Action: claude.ElicitationActionCancel}
	default:
		return claude.ElicitationResponse{Action: claude.ElicitationActionDecline}
	}
}

func elicitationSchema(raw map[string]any) acp.UnstableElicitationSchema {
	schema := acp.UnstableElicitationSchema{
		Properties: map[string]any{},
		Type:       acp.UnstableElicitationSchemaTypeObject,
	}

	if raw == nil {
		return schema
	}

	if description, _ := raw["description"].(string); description != "" {
		schema.Description = &description
	}

	if title, _ := raw[jsonFieldTitle].(string); title != "" {
		schema.Title = &title
	}

	if properties, _ := raw["properties"].(map[string]any); properties != nil {
		schema.Properties = properties
	}

	if required := stringSliceValue(raw["required"]); len(required) > 0 {
		schema.Required = required
	}

	if schemaType, _ := raw[jsonFieldType].(string); schemaType != "" {
		schema.Type = acp.UnstableElicitationSchemaType(schemaType)
	}

	return schema
}

func stringSliceValue(value any) []string {
	switch typed := value.(type) {
	case []string:
		return typed
	case []any:
		result := make([]string, 0, len(typed))
		for _, item := range typed {
			text, _ := item.(string)
			if text != "" {
				result = append(result, text)
			}
		}

		return result
	default:
		return nil
	}
}
