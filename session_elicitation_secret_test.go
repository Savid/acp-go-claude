package claudeacp

import (
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/stretchr/testify/require"
)

// TestSecretMarkedStableFormFailsClosed is the deterministic proof for the
// elicitation refusal. A secret-marked AskUserQuestion is denied before the
// pending tool call publishes the native input and before any form reaches the
// client, and the refusal repeats nothing the caller supplied.
func TestSecretMarkedStableFormFailsClosed(t *testing.T) {
	t.Parallel()

	agent := NewAgent()
	agent.clientCapabilities.Elicitation = &acp.ElicitationCapabilities{Form: &acp.ElicitationFormCapabilities{}}

	conn := newRecordingAgentClient()
	accept := acp.NewUnstableCreateElicitationResponseAccept()
	accept.Accept.Content = map[string]any{"token": "sk-live-secret"}
	conn.elicitResponse = &accept
	agent.setConnection(conn)

	session := &agentSession{agent: agent, id: "session-1"}

	decision, err := session.handleAskUserQuestion(t.Context(), claude.PermissionRequest{
		ToolName:  askUserQuestionTool,
		ToolUseID: "ask-secret",
		Input: map[string]any{askFieldQuestions: []any{
			map[string]any{
				askFieldID:       "token",
				askFieldQuestion: "Paste your API token",
				askFieldIsSecret: true,
			},
		}},
		Raw: map[string]any{"raw": true},
	})

	require.NoError(t, err)
	require.Equal(t, claude.BehaviorDeny, decision.Behavior)
	require.Equal(t, askUserQuestionSecretRefusal, decision.Message)
	require.Nil(t, decision.UpdatedInput)

	// Nothing was published and nothing was asked, so no plaintext answer could
	// have been collected.
	require.Empty(t, conn.Elicitations())
	require.Empty(t, conn.Updates())

	// The refusal names the rule and never the question, the field or a value.
	require.NotContains(t, decision.Message, "token")
	require.NotContains(t, decision.Message, "Paste your API token")
}

// TestSecretMarkedMCPFormFailsClosed proves the second path fails closed too:
// an MCP form whose raw schema carries a credential marker is declined before
// the pending tool call and before the form is built.
func TestSecretMarkedMCPFormFailsClosed(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name     string
		property map[string]any
	}{
		{name: "password format", property: map[string]any{jsonFieldType: jsonSchemaTypeString, jsonFieldFormat: "PassWord"}},
		{name: "write only", property: map[string]any{jsonFieldType: jsonSchemaTypeString, schemaFieldWriteOnly: true}},
		{name: "secret marker", property: map[string]any{jsonFieldType: jsonSchemaTypeString, askFieldIsSecret: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			agent := NewAgent()
			agent.clientCapabilities.Elicitation = &acp.ElicitationCapabilities{Form: &acp.ElicitationFormCapabilities{}}

			conn := newRecordingAgentClient()
			agent.setConnection(conn)

			session := &agentSession{agent: agent, id: "session-1"}

			response, err := session.handleElicitation(t.Context(), claude.ElicitationRequest{
				Mode:      claude.ElicitationModeForm,
				Message:   "Sign in",
				ToolUseID: "mcp-secret",
				RequestedSchema: map[string]any{
					jsonFieldType: "object",
					"properties":  map[string]any{"credential": tc.property},
				},
			})

			require.NoError(t, err)
			require.Equal(t, claude.ElicitationActionDecline, response.Action)
			require.Nil(t, response.Content)
			require.Empty(t, conn.Elicitations())
			require.Empty(t, conn.Updates())
		})
	}
}

func TestSchemaSecretMarkersAreFoundAtEveryDepth(t *testing.T) {
	t.Parallel()

	require.False(t, schemaSolicitsSecret(nil))
	require.False(t, schemaSolicitsSecret("password"))
	require.False(t, schemaSolicitsSecret(map[string]any{
		"properties": map[string]any{"name": map[string]any{jsonFieldType: jsonSchemaTypeString}},
	}))
	require.False(t, schemaSolicitsSecret(map[string]any{jsonFieldFormat: "email"}))
	require.False(t, schemaSolicitsSecret(map[string]any{schemaFieldWriteOnly: false, askFieldIsSecret: false}))

	require.True(t, schemaSolicitsSecret(map[string]any{
		"properties": map[string]any{
			"nested": map[string]any{
				"anyOf": []any{
					map[string]any{jsonFieldType: jsonSchemaTypeString},
					map[string]any{jsonFieldFormat: schemaFormatPassword},
				},
			},
		},
	}))
	require.True(t, schemaSolicitsSecret([]any{map[string]any{schemaFieldWriteOnly: true}}))

	require.False(t, questionsSolicitSecret([]askUserQuestion{{ID: "a"}}))
	require.True(t, questionsSolicitSecret([]askUserQuestion{{ID: "a"}, {ID: "b", IsSecret: true}}))
}
