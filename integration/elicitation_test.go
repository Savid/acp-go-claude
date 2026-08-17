//go:build integration

package integration

import (
	"context"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	claudeacp "github.com/savid/acp-go-claude"
	"github.com/stretchr/testify/require"
)

// askUserQuestionMetaKey and askUserQuestionToolName mirror the unexported
// adapter constants (claudeMetaKey / askUserQuestionTool). They are duplicated
// here because this integration package cannot import the root package's
// unexported identifiers, and they anchor the assertion that the recorded
// elicitation genuinely originated from the native AskUserQuestion tool.
const (
	askUserQuestionMetaKey  = "claude"
	askUserQuestionToolName = "AskUserQuestion"
)

// TestClaudeAskUserQuestionElicitationLive drives one short live turn whose
// prompt forces the native `claude` CLI to invoke its AskUserQuestion tool
// before answering. It proves that tool reaches ACP `elicitation/create`
// end to end: the adapter maps the native callback into a form elicitation,
// the client answers it, and the answered turn completes. The unit suite in
// session_elicitation_test.go covers handleAskUserQuestion and the capability
// gate in isolation; this is the only coverage that exercises the real CLI
// through the live wire.
func TestClaudeAskUserQuestionElicitationLive(t *testing.T) {
	requireLiveTokens(t)
	parallelWhenPortableClaudeAuth(t)

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	// Advertise only form elicitation so the adapter's capability gate opens
	// the AskUserQuestion path; the recordingClient auto-accepts by filling in
	// the required question IDs from the form schema.
	client := &recordingClient{}
	conn := connectLiveAgent(t, ctx, client, acp.InitializeRequest{
		ClientCapabilities: acp.ClientCapabilities{
			Elicitation: &acp.ElicitationCapabilities{
				Form: &acp.ElicitationFormCapabilities{},
			},
		},
	})

	session, err := conn.NewSession(ctx, claudeacp.NewSessionRequest(t.TempDir()))
	require.NoError(t, err)

	resp := promptWithRefusalRetry(t, func() (acp.PromptResponse, error) {
		return conn.Prompt(ctx, claudeacp.TextPromptRequest(
			session.SessionId,
			"turn-ask-user-question",
			"Before responding you MUST call your AskUserQuestion tool exactly once "+
				"to ask me a single clarifying question. Do not answer or take any "+
				"other action until you have asked it. My request is deliberately "+
				"ambiguous: \"Set it up.\" Ask which thing I want set up and offer a "+
				"few options. After I answer, reply briefly.",
		))
	})

	require.Equal(t, acp.StopReasonEndTurn, resp.StopReason)
	require.Positive(t, client.elicitationCount(), "native AskUserQuestion tool must reach elicitation/create")
	require.True(t, recordedAskUserQuestionElicitation(client.elicitationSnapshot()),
		"a recorded elicitation/create must be an AskUserQuestion form elicitation")
}

// recordedAskUserQuestionElicitation reports whether any recorded
// elicitation/create is a form elicitation carrying the adapter's
// AskUserQuestion meta marker.
func recordedAskUserQuestionElicitation(elicitations []acp.UnstableCreateElicitationRequest) bool {
	for _, elicitation := range elicitations {
		if elicitation.Form == nil {
			continue
		}

		meta, _ := elicitation.Form.Meta[askUserQuestionMetaKey].(map[string]any)
		if toolName, _ := meta["toolName"].(string); toolName == askUserQuestionToolName {
			return true
		}
	}

	return false
}
