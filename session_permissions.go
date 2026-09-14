package claudeacp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"strconv"
	"strings"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/savid/acp-go-core/lifecycle"
	"github.com/savid/acp-go-core/observer"
)

const permissionOptionAllow acp.PermissionOptionId = "allow"
const permissionOptionDeny acp.PermissionOptionId = "deny"

// handleControl publishes any tool state before its callback can run concurrently.
func (s *session) handleControl(ctx context.Context, rt *runtime, event claude.Event) {
	ctx = context.WithValue(ctx, parentToolKey{}, event.ParentToolUseID)
	if event.Request == nil {
		return
	}

	request := *event.Request
	if request.Subtype == "hook_callback" {
		_ = rt.client.Reply(ctx, event.RequestID, map[string]any{}, nil)

		return
	}

	s.mu.Lock()
	t := s.turn
	c := s.cycle
	closing := s.closing
	s.mu.Unlock()

	if t != nil {
		s.acceptTurn(ctx, t)
		c = &t.cycle
	}

	if c == nil && !closing {
		s.openAgentCycle(ctx)
		s.mu.Lock()
		c = s.cycle
		s.mu.Unlock()
	}

	if c == nil || closing {
		_ = rt.client.Reply(ctx, event.RequestID, map[string]any{permissionBehavior: permissionOptionDeny, nativeMessage: "Session closed"}, nil)

		return
	}

	if request.Subtype == controlCanUseTool {
		if err := s.publishPendingTool(ctx, &c.state, request); err != nil {
			c.failure = err
		}
	}

	dialogCtx, cancel := context.WithCancelCause(context.WithoutCancel(ctx))

	unregister := s.registerDialog(event.RequestID, cancel)

	go func() {
		defer cancel(nil)
		defer unregister()

		var (
			result any
			err    error
		)

		switch request.Subtype {
		case controlCanUseTool:
			if request.ToolName == "AskUserQuestion" {
				result = s.answerQuestions(dialogCtx, c, request)

				break
			}

			permissionCtx, finish := s.agent.observe.StartPermission(dialogCtx, request.ToolName, s.permissionMode())
			answer := s.requestPermission(permissionCtx, c, request)
			finish(observer.PermissionResult{Behavior: string(answer), Mode: s.permissionMode(), ToolName: request.ToolName})

			result = map[string]any{permissionBehavior: permissionOptionDeny, nativeMessage: "Permission denied"}
			if answer == permissionOptionAllow {
				result = map[string]any{permissionBehavior: permissionOptionAllow, "updatedInput": request.Input}
			}
		case controlElicitation:
			result = s.elicit(dialogCtx, c, request)
		default:
			err = errors.New("unsupported native control request")
		}

		replyCtx, replyCancel := context.WithTimeout(context.WithoutCancel(dialogCtx), sessionAbortTimeout)
		defer replyCancel()

		if replyErr := rt.client.Reply(replyCtx, event.RequestID, result, err); replyErr != nil {
			s.agent.log.DebugContext(replyCtx, "native control reply failed", slog.String("session_id", string(s.id)))
		}
	}()
}

func (s *session) elicit(ctx context.Context, c *cycle, request claude.ControlRequest) map[string]any {
	conn := s.agent.connection()

	cancelled := map[string]any{elicitationAction: "cancel"}
	if conn == nil {
		return cancelled
	}

	var params acp.UnstableCreateElicitationRequest

	if request.Mode == elicitationModeURL {
		s.agent.mu.Lock()
		supported := s.agent.clientCapabilities.Elicitation != nil && s.agent.clientCapabilities.Elicitation.Url != nil
		s.agent.mu.Unlock()

		if !supported {
			return cancelled
		}

		params.Url = &acp.UnstableCreateElicitationUrl{Mode: elicitationModeURL, Message: request.Message, Url: request.URL, ElicitationId: acp.UnstableElicitationId(request.ElicitationID)}
	} else {
		if !s.agent.clientSupportsFormElicitation() {
			return cancelled
		}

		var schema acp.UnstableElicitationSchema
		if err := json.Unmarshal(request.RequestedSchema, &schema); err != nil {
			return cancelled
		}

		params.Form = &acp.UnstableCreateElicitationForm{Mode: elicitationModeForm, Message: request.Message, RequestedSchema: schema}
	}

	ctx, finish := s.agent.observe.StartElicitation(ctx)
	response, err := announcedRequest(ctx, s, c, lifecycle.ActionElicitation, func(requestCtx context.Context, meta map[string]any) (acp.UnstableCreateElicitationResponse, error) {
		if params.Form != nil {
			params.Form.Meta = meta
		} else {
			params.Url.Meta = meta
		}

		return conn.UnstableCreateElicitation(requestCtx, params)
	}, func(response acp.UnstableCreateElicitationResponse, err error) lifecycle.ActionState {
		switch {
		case err != nil:
			return lifecycle.ActionFailed
		case response.Accept != nil:
			return lifecycle.ActionAccepted
		case response.Decline != nil:
			return lifecycle.ActionDeclined
		default:
			return lifecycle.ActionCancelled
		}
	})
	finish(observer.ElicitationResult{Accepted: err == nil && response.Accept != nil, Err: err})

	if err != nil {
		return cancelled
	}

	if response.Accept != nil {
		return map[string]any{elicitationAction: elicitationAccept, nativeContent: response.Accept.Content}
	}

	if response.Decline != nil {
		return map[string]any{elicitationAction: "decline"}
	}

	return cancelled
}

func (s *session) requestPermission(ctx context.Context, c *cycle, prompt claude.ControlRequest) acp.PermissionOptionId {
	conn := s.agent.connection()
	if conn == nil {
		return permissionOptionDeny
	}

	kind := toolKindForName(prompt.ToolName)
	status := acp.ToolCallStatusPending
	title := prompt.ToolName

	toolCall := acp.ToolCallUpdate{
		ToolCallId: acp.ToolCallId(prompt.ToolUseID),
		Title:      &title,
		Kind:       &kind,
		Status:     &status,
	}
	if len(prompt.Input) > 0 {
		toolCall.RawInput = prompt.Input
	}

	resp, err := announcedRequest(ctx, s, c, lifecycle.ActionPermission,
		func(requestCtx context.Context, meta map[string]any) (acp.RequestPermissionResponse, error) {
			return conn.RequestPermission(requestCtx, acp.RequestPermissionRequest{
				Meta:      meta,
				SessionId: s.id,
				ToolCall:  toolCall,
				Options: []acp.PermissionOption{
					{OptionId: permissionOptionAllow, Name: "Allow", Kind: acp.PermissionOptionKindAllowOnce},
					{OptionId: permissionOptionDeny, Name: "Deny", Kind: acp.PermissionOptionKindRejectOnce},
				},
			})
		},
		func(resp acp.RequestPermissionResponse, err error) lifecycle.ActionState {
			switch {
			case err != nil:
				return lifecycle.ActionFailed
			case resp.Outcome.Selected == nil:
				return lifecycle.ActionCancelled
			case resp.Outcome.Selected.OptionId == permissionOptionAllow:
				return lifecycle.ActionAccepted
			default:
				return lifecycle.ActionDeclined
			}
		})
	if err != nil || resp.Outcome.Selected == nil || resp.Outcome.Selected.OptionId != permissionOptionAllow {
		return permissionOptionDeny
	}

	return permissionOptionAllow
}

// announcedRequest sends one client request that holds native work, announces
// the action it answers once the request is on the wire, and resolves that
// action exactly once.
func announcedRequest[T any](
	ctx context.Context,
	s *session,
	c *cycle,
	kind lifecycle.ActionKind,
	send func(context.Context, map[string]any) (T, error),
	resolved func(T, error) lifecycle.ActionState,
) (T, error) {
	var zero T

	releaseCall, err := s.agent.acquireClientCall()
	if err != nil {
		return zero, err
	}
	defer releaseCall()

	actionID, err := s.reserveAction(c)
	if err != nil {
		return zero, err
	}

	if actionID == "" {
		return send(ctx, nil)
	}

	type answer struct {
		value T
		err   error
	}

	answers := make(chan answer, 1)

	var written <-chan struct{}
	if t := s.agent.transportRef(); t != nil {
		written = t.AwaitRequestWrite(actionID)
	}

	go func() {
		value, err := send(ctx, s.actionCorrelation(c, actionID))
		answers <- answer{value: value, err: err}
	}()

	if written != nil {
		select {
		case <-written:
		case result := <-answers:
			answers <- result
		}
	}

	if err := s.lcActionPendingWithID(ctx, c, actionID, kind); err != nil {
		s.agent.log.ErrorContext(ctx, "announce lifecycle action failed",
			slog.String("session_id", string(s.id)), slog.String("reason", err.Error()))
	}

	result := <-answers
	state := resolved(result.value, result.err)

	if result.err != nil && errors.Is(context.Cause(ctx), errDialogCancelled) {
		state = lifecycle.ActionCancelled
	}

	if err := s.lcActionResolved(context.WithoutCancel(ctx), c, actionID, state); err != nil {
		s.agent.log.ErrorContext(ctx, "resolve lifecycle action failed",
			slog.String("session_id", string(s.id)), slog.String("reason", err.Error()))
	}

	return result.value, result.err
}

func (s *session) reserveAction(c *cycle) (string, error) {
	s.lcMu.Lock()
	defer s.lcMu.Unlock()

	if s.lc.stream == nil || s.lc.stream.Fenced() || c.turnID == "" {
		return "", nil
	}

	return s.nextLifecycleID(elicitationAction), nil
}

func toolKindForName(name string) acp.ToolKind {
	switch name {
	case "Read":
		return acp.ToolKindRead
	case "Edit", "Write", "NotebookEdit":
		return acp.ToolKindEdit
	case "Bash":
		return acp.ToolKindExecute
	case "Grep", "Glob":
		return acp.ToolKindSearch
	case "WebFetch":
		return acp.ToolKindFetch
	default:
		return acp.ToolKindOther
	}
}

const nativeMessage = "message"

// answerQuestions returns answers in the native tool's updated input.
func (s *session) answerQuestions(ctx context.Context, c *cycle, request claude.ControlRequest) map[string]any {
	denied := map[string]any{permissionBehavior: permissionOptionDeny, nativeMessage: "Question cancelled"}

	encoded, err := json.Marshal(request.Input["questions"])
	if err != nil {
		return denied
	}

	var questions []struct {
		Question    string `json:"question"`
		MultiSelect bool   `json:"multiSelect"`
		Options     []struct {
			Label       string `json:"label"`
			Description string `json:"description"`
		} `json:"options"`
	}
	if json.Unmarshal(encoded, &questions) != nil || len(questions) == 0 {
		return denied
	}

	properties := make(map[string]any)

	required := make([]string, 0, len(questions))
	for index, question := range questions {
		key := "q" + strconv.Itoa(index)
		required = append(required, key)

		choices := make([]string, 0, len(question.Options))
		for _, option := range question.Options {
			choices = append(choices, option.Label)
		}

		property := map[string]any{schemaTypeKey: schemaTypeString, "title": question.Question, "enum": choices}
		if question.MultiSelect {
			property = map[string]any{schemaTypeKey: "array", "title": question.Question, "items": map[string]any{schemaTypeKey: schemaTypeString, "enum": choices}}
		}

		properties[key] = property
	}

	schema, _ := json.Marshal(map[string]any{schemaTypeKey: "object", "properties": properties, "required": required})

	response := s.elicit(ctx, c, claude.ControlRequest{Mode: elicitationModeForm, Message: "Claude needs your input.", RequestedSchema: schema})
	if response[elicitationAction] != elicitationAccept {
		return denied
	}

	content, ok := response[nativeContent].(map[string]any)
	if !ok {
		return denied
	}

	answers := make(map[string]string, len(questions))
	for index, question := range questions {
		value := content["q"+strconv.Itoa(index)]
		if text, ok := value.(string); ok {
			answers[question.Question] = text

			continue
		}

		values, ok := value.([]any)
		if !ok {
			return denied
		}

		labels := make([]string, 0, len(values))
		for _, value := range values {
			label, ok := value.(string)
			if !ok {
				return denied
			}

			labels = append(labels, label)
		}

		answers[question.Question] = strings.Join(labels, ", ")
	}

	input := maps.Clone(request.Input)
	input["answers"] = answers

	return map[string]any{permissionBehavior: permissionOptionAllow, "updatedInput": input}
}

const (
	elicitationModeURL  = "url"
	elicitationModeForm = "form"
	schemaTypeString    = "string"
	schemaTypeKey       = "type"
)
