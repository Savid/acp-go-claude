package claudeacp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/stretchr/testify/require"
)

func TestElicitationCancelledDuringTurn(t *testing.T) {
	t.Parallel()

	agent := NewAgent()
	agent.clientCapabilities.Elicitation = &acp.ElicitationCapabilities{
		Form: &acp.ElicitationFormCapabilities{},
		Url:  &acp.ElicitationUrlCapabilities{},
	}
	session := &agentSession{agent: agent, id: "session-1", turnCancelled: true}
	conn := newRecordingAgentClient()
	conn.elicitErr = context.Canceled
	agent.setConnection(conn)

	formResp, err := session.createFormElicitation(t.Context(), conn, claude.ElicitationRequest{Mode: claude.ElicitationModeForm})
	require.NoError(t, err)
	require.Equal(t, claude.ElicitationActionCancel, formResp.Action)

	urlResp, err := session.createURLElicitation(t.Context(), conn, claude.ElicitationRequest{
		Mode:          claude.ElicitationModeURL,
		URL:           "https://example.test",
		ElicitationID: "e1",
	})
	require.NoError(t, err)
	require.Equal(t, claude.ElicitationActionCancel, urlResp.Action)

	decision, err := session.handleAskUserQuestion(t.Context(), claude.PermissionRequest{
		ToolName:  askUserQuestionTool,
		ToolUseID: "ask-cancelled",
		Input:     map[string]any{askFieldQuestions: []any{map[string]any{askFieldID: "q"}}},
	})
	require.NoError(t, err)
	require.Equal(t, claude.BehaviorDeny, decision.Behavior)
	require.True(t, decision.Interrupt)
}

func TestAskUserQuestionHelpers(t *testing.T) {
	t.Parallel()

	_, msg := parseAskUserQuestions(nil)
	require.Contains(t, msg, "missing tool input")
	_, msg = parseAskUserQuestions(map[string]any{})
	require.Contains(t, msg, "missing questions")
	_, msg = parseAskUserQuestions(map[string]any{askFieldQuestions: []any{"bad"}})
	require.Contains(t, msg, "no parseable")

	questions, msg := parseAskUserQuestions(map[string]any{askFieldQuestions: []any{
		map[string]any{
			askFieldQuestion:    "Pick one",
			askFieldHeader:      "Header",
			askFieldMultiSelect: false,
			askFieldOptions: []any{
				map[string]any{"label": "Yes", askFieldDescription: "Approve"},
				map[string]any{"label": ""},
				"skip",
			},
		},
		map[string]any{
			askFieldID:          "multi",
			askFieldQuestion:    "Pick many",
			askFieldMultiSelect: true,
			askFieldOptions:     []any{map[string]any{"label": "A"}, map[string]any{"label": "B"}},
		},
	}})
	require.Empty(t, msg)
	require.Equal(t, "question_1", questions[0].ID)
	require.Equal(t, "Pick one", askUserQuestionMessage(questions[:1]))
	require.Equal(t, "Claude needs more input.", askUserQuestionMessage(questions))
	require.Equal(t, "Header", askUserQuestionTitle(questions[0]))
	require.Equal(t, "Pick many", askUserQuestionTitle(questions[1]))

	schema := askUserQuestionSchema(questions)
	require.Contains(t, schema.Required, "question_1")
	require.Contains(t, schema.Required, "multi")
	multiSchema, ok := schema.Properties["multi"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "array", multiSchema[jsonFieldType])
	require.Len(t, askUserQuestionOneOf(questions[0].Options), 1)

	input := map[string]any{askFieldQuestions: []any{}}
	_, msg = applyAskUserAnswers(input, questions, nil)
	require.Contains(t, msg, "empty")
	_, msg = applyAskUserAnswers(input, questions, map[string]any{"missing": "x"})
	require.Contains(t, msg, "no valid")
	updated, msg := applyAskUserAnswers(input, questions, map[string]any{"question_1": "Yes", "multi": []any{"A", "", "B"}})
	require.Empty(t, msg)
	answers, ok := updated[askFieldAnswers].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "Yes", answers["Pick one"])
	require.Equal(t, "A, B", answers["Pick many"])
	require.Equal(t, "id", claudeAskAnswerKey(askUserQuestion{ID: "id"}))
	require.Equal(t, []string{"x"}, answerStrings("x"))
	require.Nil(t, answerStrings(""))
	require.Equal(t, []string{"a"}, answerStrings([]string{"", "a"}))
	require.Equal(t, []string{"a"}, nonEmptyStrings([]string{"", "a"}))
	require.Nil(t, answerStrings(1))
}

func TestHandleAskUserQuestion(t *testing.T) {
	t.Parallel()

	agent := NewAgent()
	session := &agentSession{agent: agent, id: "session-1"}
	decision, err := session.handleAskUserQuestion(t.Context(), claude.PermissionRequest{})
	require.NoError(t, err)
	require.Equal(t, claude.BehaviorDeny, decision.Behavior)

	conn := newRecordingAgentClient()
	accept := acp.NewUnstableCreateElicitationResponseAccept()
	accept.Accept.Content = map[string]any{"q": "Yes"}
	conn.elicitResponse = &accept
	agent.setConnection(conn)
	agent.clientCapabilities.Elicitation = &acp.ElicitationCapabilities{Form: &acp.ElicitationFormCapabilities{}}
	decision, err = session.handleAskUserQuestion(t.Context(), claude.PermissionRequest{Input: map[string]any{}})
	require.NoError(t, err)
	require.Equal(t, claude.BehaviorDeny, decision.Behavior)

	decision, err = session.handleAskUserQuestion(t.Context(), claude.PermissionRequest{
		ToolName:  askUserQuestionTool,
		ToolUseID: "ask-1",
		Input: map[string]any{askFieldQuestions: []any{
			map[string]any{askFieldID: "q", askFieldQuestion: "Question?"},
		}},
		Raw: map[string]any{"raw": true},
	})
	require.NoError(t, err)
	require.Equal(t, claude.BehaviorAllow, decision.Behavior)
	require.Equal(t, map[string]any{"Question?": "Yes"}, decision.UpdatedInput[askFieldAnswers])

	decline := acp.NewUnstableCreateElicitationResponseDecline()
	conn.elicitResponse = &decline
	decision, err = session.handleAskUserQuestion(t.Context(), claude.PermissionRequest{
		ToolUseID: "ask-decline",
		Input:     map[string]any{askFieldQuestions: []any{map[string]any{askFieldID: "q"}}},
	})
	require.NoError(t, err)
	require.Equal(t, claude.BehaviorDeny, decision.Behavior)

	accept.Accept.Content = map[string]any{"missing": "Yes"}
	conn.elicitResponse = &accept
	decision, err = session.handleAskUserQuestion(t.Context(), claude.PermissionRequest{
		ToolUseID: "ask-invalid",
		Input:     map[string]any{askFieldQuestions: []any{map[string]any{askFieldID: "q"}}},
	})
	require.NoError(t, err)
	require.Equal(t, claude.BehaviorDeny, decision.Behavior)

	conn.elicitErr = errors.New("elicit failed")
	_, err = session.handleAskUserQuestion(t.Context(), claude.PermissionRequest{
		ToolUseID: "ask-error",
		Input:     map[string]any{askFieldQuestions: []any{map[string]any{askFieldID: "q"}}},
	})
	require.ErrorContains(t, err, "elicit failed")

	decision, err = session.handleAskUserQuestion(t.Context(), claude.PermissionRequest{
		Input: map[string]any{askFieldQuestions: []any{map[string]any{askFieldID: "q"}}},
	})
	require.NoError(t, err)
	require.Contains(t, decision.Message, "missing its native tool-use ID")

	conn.elicitErr = nil
	conn.sessionUpdateErr = errors.New("pending update failed")
	_, err = session.handleAskUserQuestion(t.Context(), claude.PermissionRequest{
		ToolUseID: "ask-update-error",
		Input:     map[string]any{askFieldQuestions: []any{map[string]any{askFieldID: "q"}}},
	})
	require.ErrorContains(t, err, "pending update failed")
}

func TestNativeElicitationPendingToolUpdateFailure(t *testing.T) {
	agent := NewAgent()
	conn := newRecordingAgentClient()
	conn.sessionUpdateErr = errors.New("pending update failed")
	agent.setConnection(conn)
	session := &agentSession{agent: agent, id: "session-1"}

	_, err := session.createFormElicitation(t.Context(), conn, claude.ElicitationRequest{
		ToolUseID: "form-update-error",
	})
	require.ErrorContains(t, err, "pending update failed")

	_, err = session.createURLElicitation(t.Context(), conn, claude.ElicitationRequest{
		ToolUseID: "url-update-error", URL: "https://example.test", ElicitationID: "e1",
	})
	require.ErrorContains(t, err, "pending update failed")
}

func TestHandleAskUserQuestionRoutesActiveTurn(t *testing.T) {
	request := claude.PermissionRequest{
		ToolName:  askUserQuestionTool,
		ToolUseID: "ask-1",
		Input: map[string]any{askFieldQuestions: []any{
			map[string]any{askFieldID: "q", askFieldQuestion: "Question?"},
		}},
	}

	t.Run("active turn", func(t *testing.T) {
		agent := NewAgent()
		agent.clientCapabilities.Elicitation = &acp.ElicitationCapabilities{Form: &acp.ElicitationFormCapabilities{}}
		conn := newRoutedElicitationClient()
		agent.setConnection(conn)
		session := &agentSession{agent: agent, id: "session-1", turnNonce: "turn-current"}

		decision, err := session.handleAskUserQuestion(t.Context(), request)
		require.NoError(t, err)
		require.Equal(t, claude.BehaviorAllow, decision.Behavior)

		routed := conn.RoutedElicitations()
		require.Len(t, routed, 1)
		var payload map[string]any
		require.NoError(t, json.Unmarshal(routed[0], &payload))
		route := requireAnyMap(t, requireAnyMap(t, payload[jsonFieldMeta])[routeMetaKey])
		require.Equal(t, map[string]any{
			routeFieldVer:  float64(routeVersion),
			routeFieldID:   "session-1",
			routeFieldTurn: "turn-current",
			"toolCallId":   "ask-1",
		}, route)
	})

	t.Run("no active turn", func(t *testing.T) {
		agent := NewAgent()
		agent.clientCapabilities.Elicitation = &acp.ElicitationCapabilities{Form: &acp.ElicitationFormCapabilities{}}
		conn := newRoutedElicitationClient()
		agent.setConnection(conn)
		session := &agentSession{agent: agent, id: "session-1"}

		_, err := session.handleAskUserQuestion(t.Context(), request)
		require.ErrorContains(t, err, "route metadata requires sessionId and turnNonce")
		require.Empty(t, conn.RoutedElicitations())
		require.Empty(t, conn.Elicitations())
	})
}

func TestElicitationCallbacksPublishExactPendingToolOnACPWire(t *testing.T) {
	t.Run("AskUserQuestion", func(t *testing.T) {
		session, client, turnCtx := newCallbackOrderWireSession(t, permissionAllowOnce)
		decision, err := session.handleAskUserQuestion(turnCtx, claude.PermissionRequest{
			ToolName:  askUserQuestionTool,
			ToolUseID: "ask-wire",
			Input: map[string]any{askFieldQuestions: []any{
				map[string]any{askFieldID: "q", askFieldQuestion: "Proceed?"},
			}},
		})
		require.NoError(t, err)
		require.Equal(t, claude.BehaviorAllow, decision.Behavior)
		require.Equal(t, []string{"tool_call:ask-wire", "elicitation:ask-wire"}, client.Order())
	})

	t.Run("native MCP form", func(t *testing.T) {
		session, client, turnCtx := newCallbackOrderWireSession(t, permissionAllowOnce)
		response, err := session.handleElicitation(turnCtx, claude.ElicitationRequest{
			Mode:            claude.ElicitationModeForm,
			Message:         "Choose",
			ToolUseID:       "mcp-wire",
			RequestedSchema: map[string]any{jsonFieldType: "object"},
		})
		require.NoError(t, err)
		require.Equal(t, claude.ElicitationActionAccept, response.Action)
		require.Equal(t, []string{"tool_call:mcp-wire", "elicitation:mcp-wire"}, client.Order())
	})

	t.Run("native MCP URL", func(t *testing.T) {
		session, client, turnCtx := newCallbackOrderWireSession(t, permissionAllowOnce)
		response, err := session.handleElicitation(turnCtx, claude.ElicitationRequest{
			Mode:          claude.ElicitationModeURL,
			Message:       "Authorize",
			URL:           "https://example.test/authorize",
			ElicitationID: "elicitation-wire",
			ToolUseID:     "mcp-url-wire",
		})
		require.NoError(t, err)
		require.Equal(t, claude.ElicitationActionAccept, response.Action)
		require.Equal(t, []string{"tool_call:mcp-url-wire", "elicitation:mcp-url-wire"}, client.Order())
	})
}

type routedElicitationClient struct {
	*recordingAgentClient
	routed []json.RawMessage
}

func newRoutedElicitationClient() *routedElicitationClient {
	client := newRecordingAgentClient()
	accept := acp.NewUnstableCreateElicitationResponseAccept()
	accept.Accept.Content = map[string]any{"q": "Yes"}
	client.elicitResponse = &accept

	return &routedElicitationClient{recordingAgentClient: client}
}

func (c *routedElicitationClient) CreateElicitation(
	ctx context.Context,
	request acp.UnstableCreateElicitationRequest,
	scope elicitationScope,
) (acp.UnstableCreateElicitationResponse, error) {
	raw, err := scopedElicitationParams(request, scope)
	if err != nil {
		return acp.UnstableCreateElicitationResponse{}, err
	}

	c.mu.Lock()
	c.routed = append(c.routed, append(json.RawMessage(nil), raw...))
	c.mu.Unlock()

	return c.recordingAgentClient.CreateElicitation(ctx, request, scope)
}

func (c *routedElicitationClient) RoutedElicitations() []json.RawMessage {
	c.mu.Lock()
	defer c.mu.Unlock()

	return append([]json.RawMessage(nil), c.routed...)
}

func TestClientElicitationCapabilityGating(t *testing.T) {
	t.Parallel()

	for _, tt := range []struct {
		name     string
		caps     *acp.ElicitationCapabilities
		wantForm bool
		wantURL  bool
	}{
		{name: "nil", caps: nil, wantForm: false, wantURL: false},
		{name: "empty object", caps: &acp.ElicitationCapabilities{}, wantForm: true, wantURL: false},
		{name: "url only", caps: &acp.ElicitationCapabilities{Url: &acp.ElicitationUrlCapabilities{}}, wantForm: false, wantURL: true},
		{name: "form explicit", caps: &acp.ElicitationCapabilities{Form: &acp.ElicitationFormCapabilities{}}, wantForm: true, wantURL: false},
	} {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			agent := NewAgent()
			agent.clientCapabilities.Elicitation = tt.caps
			require.Equal(t, tt.wantForm, agent.clientSupportsFormElicitation())
			require.Equal(t, tt.wantURL, agent.clientSupportsURLElicitation())
		})
	}
}

func TestElicitationHelpersAndHandlers(t *testing.T) {
	agent := NewAgent()
	session := &agentSession{agent: agent, id: "session-1"}
	response, err := session.handleElicitation(t.Context(), claude.ElicitationRequest{Mode: claude.ElicitationModeForm})
	require.NoError(t, err)
	require.Equal(t, claude.ElicitationActionDecline, response.Action)

	conn := newRecordingAgentClient()
	agent.setConnection(conn)
	agent.clientCapabilities.Elicitation = &acp.ElicitationCapabilities{Form: &acp.ElicitationFormCapabilities{}, Url: &acp.ElicitationUrlCapabilities{}}
	response, err = session.handleElicitation(t.Context(), claude.ElicitationRequest{
		Mode:            claude.ElicitationModeForm,
		Message:         "Fill form",
		RequestedSchema: map[string]any{jsonFieldTitle: "Title", "description": "Desc", "required": []any{"name"}, "properties": map[string]any{"name": map[string]any{"type": "string"}}, jsonFieldType: "object"},
		Raw:             map[string]any{"raw": true},
		ToolUseID:       "tool-1",
		MCPServerName:   "server",
	})
	require.NoError(t, err)
	require.Equal(t, claude.ElicitationActionAccept, response.Action)
	require.Equal(t, map[string]any{"ok": true}, response.Content)
	require.Len(t, conn.Elicitations(), 1)

	conn.elicitErr = errors.New("form failed")
	_, err = session.handleElicitation(t.Context(), claude.ElicitationRequest{Mode: claude.ElicitationModeForm})
	require.ErrorContains(t, err, "form failed")
	conn.elicitErr = nil

	agent.clientCapabilities.Elicitation = &acp.ElicitationCapabilities{}
	response, err = session.handleElicitation(t.Context(), claude.ElicitationRequest{Mode: claude.ElicitationModeForm})
	require.NoError(t, err)
	require.Equal(t, claude.ElicitationActionAccept, response.Action)

	agent.clientCapabilities.Elicitation = &acp.ElicitationCapabilities{Url: &acp.ElicitationUrlCapabilities{}}
	response, err = session.handleElicitation(t.Context(), claude.ElicitationRequest{Mode: claude.ElicitationModeForm})
	require.NoError(t, err)
	require.Equal(t, claude.ElicitationActionDecline, response.Action)
	agent.clientCapabilities.Elicitation = &acp.ElicitationCapabilities{Form: &acp.ElicitationFormCapabilities{}}
	response, err = session.handleElicitation(t.Context(), claude.ElicitationRequest{Mode: claude.ElicitationModeURL, URL: "https://example.test"})
	require.NoError(t, err)
	require.Equal(t, claude.ElicitationActionDecline, response.Action)
	agent.clientCapabilities.Elicitation = &acp.ElicitationCapabilities{Form: &acp.ElicitationFormCapabilities{}, Url: &acp.ElicitationUrlCapabilities{}}

	response, err = session.handleElicitation(t.Context(), claude.ElicitationRequest{
		Mode:    claude.ElicitationModeURL,
		Message: "Open",
		URL:     "https://example.test",
	})
	require.NoError(t, err)
	require.Equal(t, claude.ElicitationActionAccept, response.Action)

	conn.elicitErr = errors.New("url failed")
	_, err = session.handleElicitation(t.Context(), claude.ElicitationRequest{
		Mode:          claude.ElicitationModeURL,
		ElicitationID: "existing",
		Message:       "Open",
		URL:           "https://example.test",
	})
	require.ErrorContains(t, err, "url failed")
	conn.elicitErr = nil

	previousUUIDRandom := uuidRandom
	uuidRandom = strings.NewReader("")
	t.Cleanup(func() { uuidRandom = previousUUIDRandom })
	_, err = session.handleElicitation(t.Context(), claude.ElicitationRequest{
		Mode:    claude.ElicitationModeURL,
		Message: "Open",
		URL:     "https://example.test",
	})
	require.ErrorContains(t, err, "read random uuid")
	uuidRandom = previousUUIDRandom

	response, err = session.handleElicitation(t.Context(), claude.ElicitationRequest{Mode: claude.ElicitationModeURL})
	require.NoError(t, err)
	require.Equal(t, claude.ElicitationActionDecline, response.Action)
	response, err = session.handleElicitation(t.Context(), claude.ElicitationRequest{Mode: "unknown"})
	require.NoError(t, err)
	require.Equal(t, claude.ElicitationActionDecline, response.Action)

	cancel := acp.NewUnstableCreateElicitationResponseCancel()
	require.Equal(t, claude.ElicitationActionCancel, claudeElicitationResponse(cancel).Action)
	require.Equal(t, claude.ElicitationActionDecline, claudeElicitationResponse(acp.UnstableCreateElicitationResponse{}).Action)
	require.Equal(t, acp.UnstableElicitationSchemaTypeObject, elicitationSchema(nil).Type)
	require.Equal(t, []string{"a"}, stringSliceValue([]string{"a"}))
	require.Equal(t, []string{"a"}, stringSliceValue([]any{"a", 1, ""}))
	require.Nil(t, stringSliceValue(1))
}
