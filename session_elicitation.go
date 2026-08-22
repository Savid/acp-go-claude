package claudeacp

import (
	"context"
	"fmt"
	"maps"
	"strings"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/savid/acp-go-claude/internal/lifecycle"
	"github.com/savid/acp-go-claude/internal/mapper"
	"github.com/savid/acp-go-claude/internal/observer"
)

// askUserQuestionSecretRefusal is the whole answer a secret-marked question
// gets. It names the rule and nothing else.
const askUserQuestionSecretRefusal = "AskUserQuestion refused: this agent does not collect secrets through ACP elicitation"

const (
	schemaFieldWriteOnly = "writeOnly"
	schemaFormatPassword = "password"
)

type elicitationRequestCancel struct {
	cancel context.CancelFunc
	fail   context.CancelFunc
	owner  lifecycleInteractionOwner
}

// questionsSolicitSecret reports whether the native harness marked any question
// as asking for a credential.
func questionsSolicitSecret(questions []askUserQuestion) bool {
	for _, question := range questions {
		if question.IsSecret {
			return true
		}
	}

	return false
}

// schemaSolicitsSecret walks a native elicitation schema for the markers a form
// uses to say a field holds a credential. The adapter refuses such a form whole
// rather than forwarding it with a redaction hint: host-side masking is
// presentation, not permission, and an answer collected under it would still
// come back here in plaintext. The value is one decoded JSON document, so the
// walk terminates.
func schemaSolicitsSecret(value any) bool {
	switch typed := value.(type) {
	case map[string]any:
		if secretSchemaMarker(typed) {
			return true
		}

		for _, nested := range typed {
			if schemaSolicitsSecret(nested) {
				return true
			}
		}
	case []any:
		for _, nested := range typed {
			if schemaSolicitsSecret(nested) {
				return true
			}
		}
	}

	return false
}

func secretSchemaMarker(property map[string]any) bool {
	if format, _ := property[jsonFieldFormat].(string); strings.EqualFold(format, schemaFormatPassword) {
		return true
	}

	if writeOnly, _ := property[schemaFieldWriteOnly].(bool); writeOnly {
		return true
	}

	// The harness spells the marker the same way in an MCP schema property as it
	// does in an AskUserQuestion question.
	isSecret, _ := property[askFieldIsSecret].(bool)

	return isSecret
}

// registerElicitation wraps ctx in a tracked cancellable context so that
// session/cancel and teardown can resolve a pending elicitation as cancelled
// instead of leaving it dangling until the native control-handler timeout.
func (s *agentSession) registerElicitation(
	ctx context.Context,
	owner lifecycleInteractionOwner,
) (context.Context, context.CancelFunc) {
	elicitationCtx, cancelCause := context.WithCancelCause(ctx)
	cancel := func() { cancelCause(context.Canceled) }
	fail := func() { cancelCause(errExactInteractionContainment) }
	entry := &elicitationRequestCancel{cancel: cancel, fail: fail, owner: owner}

	s.callbackOwnershipMu.Lock()
	ownerCurrent := owner.incarnation == nil || s.currentNativeIncarnation() == owner.incarnation
	s.mu.Lock()
	if s.elicitationCancel == nil {
		s.elicitationCancel = make(map[int64]*elicitationRequestCancel)
	}

	s.elicitationSeq++
	id := s.elicitationSeq
	ownerFailed := owner.incarnation != nil && owner.incarnation.failed.Load()
	closing := s.closing

	if !ownerFailed && ownerCurrent && !closing {
		s.elicitationCancel[id] = entry
	}

	turnCancelled := s.turnCancelled
	s.mu.Unlock()
	s.callbackOwnershipMu.Unlock()

	if ownerFailed || !ownerCurrent {
		fail()
	} else if turnCancelled || closing {
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

	// The refusal precedes the pending tool call and the elicitation request
	// alike, so a secret-marked question is never published, never rendered and
	// never answered. The message carries no field, value or caller text: the
	// point of failing closed is that nothing about the credential leaves here.
	if questionsSolicitSecret(questions) {
		return claude.PermissionDecision{
			Behavior: claude.BehaviorDeny,
			Message:  askUserQuestionSecretRefusal,
		}, nil
	}

	if request.ToolUseID == "" {
		return claude.PermissionDecision{
			Behavior: claude.BehaviorDeny,
			Message:  "AskUserQuestion callback is missing its native tool-use ID",
		}, nil
	}

	scope := elicitationScope{
		SessionID: s.id, TurnNonce: turnNonceFromContext(ctx), ToolCallID: acp.ToolCallId(request.ToolUseID),
	}

	action, err := s.beginLifecycleAction(ctx, lifecycle.ActionElicitation)
	if err != nil {
		return claude.PermissionDecision{}, err
	}

	elicitationCtx, finishElicitation := s.registerElicitation(ctx, action.interactionOwner())
	defer finishElicitation()

	actionState := lifecycle.ActionFailed

	defer func() {
		if resolveErr := action.resolve(ctx, actionState); resolveErr != nil && err == nil {
			decision, err = claude.PermissionDecision{}, resolveErr
		}
	}()

	emitPending := func() error {
		return s.emitPendingToolCall(
			ctx,
			askUserQuestionTool,
			request.ToolUseID,
			mapper.ToolTitle(askUserQuestionTool, request.Input),
			request.Input,
			request.Raw,
		)
	}

	wireAdmission, err := action.prepareWireAdmission(ctx, emitPending)
	if err != nil {
		return claude.PermissionDecision{}, err
	}

	resp, err := conn.CreateElicitation(elicitationCtx, acp.UnstableCreateElicitationRequest{
		Form: &acp.UnstableCreateElicitationForm{
			Message:         askUserQuestionMessage(questions),
			Mode:            claude.ElicitationModeForm,
			RequestedSchema: askUserQuestionSchema(questions),
			Meta: withLifecycleMeta(map[string]any{
				claudeMetaKey: map[string]any{
					"toolName":  askUserQuestionTool,
					acpFieldRaw: request.Raw,
				},
			}, action.meta()),
		},
	}, scope, wireAdmission)
	if err != nil {
		actionState = interactionActionState(elicitationCtx, elicitationActionState(resp, err))

		if permissionRequestCancelled(err) && s.wasTurnCancelled() {
			return claude.PermissionDecision{
				Behavior:  claude.BehaviorDeny,
				Message:   permissionCancelledMessage,
				Interrupt: true,
			}, nil
		}

		return claude.PermissionDecision{}, err
	}

	if !action.responseOwnerCurrent() {
		return claude.PermissionDecision{
			Behavior: claude.BehaviorDeny,
			Message:  "AskUserQuestion response belongs to a retired native incarnation",
		}, nil
	}

	actionState = interactionActionState(elicitationCtx, elicitationActionState(resp, nil))

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
) (response claude.ElicitationResponse, formErr error) {
	// The schema is inspected before the pending tool call publishes the native
	// input and before the form is built, so a credential-soliciting form is
	// declined without ever reaching the client.
	if schemaSolicitsSecret(request.RequestedSchema) {
		return claude.ElicitationResponse{Action: claude.ElicitationActionDecline}, nil
	}

	action, err := s.beginLifecycleAction(ctx, lifecycle.ActionElicitation)
	if err != nil {
		return claude.ElicitationResponse{}, err
	}

	elicitationCtx, finishElicitation := s.registerElicitation(ctx, action.interactionOwner())
	defer finishElicitation()

	actionState := lifecycle.ActionFailed

	defer func() {
		if resolveErr := action.resolve(ctx, actionState); resolveErr != nil && formErr == nil {
			response, formErr = claude.ElicitationResponse{}, resolveErr
		}
	}()

	var emitPending func() error
	if request.ToolUseID != "" {
		emitPending = func() error {
			return s.emitPendingToolCall(ctx, "MCP elicitation", request.ToolUseID, request.Message, request.Raw, request.Raw)
		}
	}

	wireAdmission, err := action.prepareWireAdmission(ctx, emitPending)
	if err != nil {
		return claude.ElicitationResponse{}, err
	}

	resp, err := conn.CreateElicitation(elicitationCtx, acp.UnstableCreateElicitationRequest{
		Form: &acp.UnstableCreateElicitationForm{
			Message:         request.Message,
			Mode:            claude.ElicitationModeForm,
			RequestedSchema: elicitationSchema(request.RequestedSchema),
			Meta:            withLifecycleMeta(s.elicitationMeta(request), action.meta()),
		},
	}, s.elicitationScope(ctx, request), wireAdmission)
	if err != nil {
		actionState = interactionActionState(elicitationCtx, elicitationActionState(resp, err))

		if permissionRequestCancelled(err) && s.wasTurnCancelled() {
			return claude.ElicitationResponse{Action: claude.ElicitationActionCancel}, nil
		}

		return claude.ElicitationResponse{}, err
	}

	if !action.responseOwnerCurrent() {
		return claude.ElicitationResponse{Action: claude.ElicitationActionDecline}, nil
	}

	actionState = interactionActionState(elicitationCtx, elicitationActionState(resp, nil))

	return claudeElicitationResponse(resp), nil
}

func (s *agentSession) createURLElicitation(
	ctx context.Context,
	conn agentClient,
	request claude.ElicitationRequest,
) (response claude.ElicitationResponse, urlErr error) {
	elicitationID := request.ElicitationID
	if elicitationID == "" {
		id, err := newUUID()
		if err != nil {
			return claude.ElicitationResponse{}, err
		}

		elicitationID = id
	}

	action, err := s.beginLifecycleAction(ctx, lifecycle.ActionElicitation)
	if err != nil {
		return claude.ElicitationResponse{}, err
	}

	elicitationCtx, finishElicitation := s.registerElicitation(ctx, action.interactionOwner())
	defer finishElicitation()

	actionState := lifecycle.ActionFailed

	defer func() {
		if resolveErr := action.resolve(ctx, actionState); resolveErr != nil && urlErr == nil {
			response, urlErr = claude.ElicitationResponse{}, resolveErr
		}
	}()

	var emitPending func() error
	if request.ToolUseID != "" {
		emitPending = func() error {
			return s.emitPendingToolCall(ctx, "MCP elicitation", request.ToolUseID, request.Message, request.Raw, request.Raw)
		}
	}

	wireAdmission, err := action.prepareWireAdmission(ctx, emitPending)
	if err != nil {
		return claude.ElicitationResponse{}, err
	}

	resp, err := conn.CreateElicitation(elicitationCtx, acp.UnstableCreateElicitationRequest{
		Url: &acp.UnstableCreateElicitationUrl{
			ElicitationId: acp.UnstableElicitationId(elicitationID),
			Message:       request.Message,
			Mode:          claude.ElicitationModeURL,
			Url:           request.URL,
			Meta:          withLifecycleMeta(s.elicitationMeta(request), action.meta()),
		},
	}, s.elicitationScope(ctx, request), wireAdmission)
	if err != nil {
		actionState = interactionActionState(elicitationCtx, elicitationActionState(resp, err))

		if permissionRequestCancelled(err) && s.wasTurnCancelled() {
			return claude.ElicitationResponse{Action: claude.ElicitationActionCancel}, nil
		}

		return claude.ElicitationResponse{}, err
	}

	if !action.responseOwnerCurrent() {
		return claude.ElicitationResponse{Action: claude.ElicitationActionDecline}, nil
	}

	actionState = interactionActionState(elicitationCtx, elicitationActionState(resp, nil))

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

// elicitationScope routes one outbound elicitation by the route the inbound
// callback authenticated with, never by whichever turn the session happens to be
// running. A callback that carries no route names no turn to answer on, and the
// request is refused rather than routed through a turn that never asked for it.
func (s *agentSession) elicitationScope(
	ctx context.Context,
	request claude.ElicitationRequest,
) elicitationScope {
	scope := elicitationScope{SessionID: s.id, TurnNonce: turnNonceFromContext(ctx)}
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
