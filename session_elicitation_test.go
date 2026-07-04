package claudeacp

import (
	"errors"
	"strings"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/stretchr/testify/require"
)

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
		Input: map[string]any{askFieldQuestions: []any{map[string]any{askFieldID: "q"}}},
	})
	require.NoError(t, err)
	require.Equal(t, claude.BehaviorDeny, decision.Behavior)

	accept.Accept.Content = map[string]any{"missing": "Yes"}
	conn.elicitResponse = &accept
	decision, err = session.handleAskUserQuestion(t.Context(), claude.PermissionRequest{
		Input: map[string]any{askFieldQuestions: []any{map[string]any{askFieldID: "q"}}},
	})
	require.NoError(t, err)
	require.Equal(t, claude.BehaviorDeny, decision.Behavior)

	conn.elicitErr = errors.New("elicit failed")
	_, err = session.handleAskUserQuestion(t.Context(), claude.PermissionRequest{
		Input: map[string]any{askFieldQuestions: []any{map[string]any{askFieldID: "q"}}},
	})
	require.ErrorContains(t, err, "elicit failed")
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
