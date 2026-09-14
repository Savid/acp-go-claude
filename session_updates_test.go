package claudeacp

import (
	"encoding/base64"
	"testing"

	"github.com/coder/acp-go-sdk"

	"github.com/stretchr/testify/require"
)

func TestReplayKeepsFragmentsWithOneAPIMessageID(t *testing.T) {
	t.Parallel()
	agent := NewAgent()
	rec := newRecorder()
	agent.attach(rec, nil)
	session := agent.newSession(sessionStart{cwd: t.TempDir()})
	session.id = "replay-session"
	rows := make([][]byte, 0, 3)
	rows = append(rows,
		[]byte(`{"type":"assistant","uuid":"thought","message":{"id":"api-message","role":"assistant","content":[{"type":"thinking","thinking":"considering"}]}}`),
		[]byte(`{"type":"assistant","uuid":"text","message":{"id":"api-message","role":"assistant","content":[{"type":"text","text":"answer"}]}}`),
	)
	rows = append(rows, rows[1])
	require.NoError(t, session.replay(t.Context(), rows))
	require.Equal(t, "answer", agentText(rec.snapshot()))
}
func TestPromptContentOrderAndDocuments(t *testing.T) {
	t.Parallel()
	agent := NewAgent()
	session := agent.newSession(sessionStart{cwd: t.TempDir()})
	mime := "application/pdf"
	blocks := []acp.ContentBlock{acp.TextBlock("before"), {Resource: &acp.ContentBlockResource{Resource: acp.EmbeddedResourceResource{BlobResourceContents: &acp.BlobResourceContents{MimeType: &mime, Blob: base64.StdEncoding.EncodeToString([]byte("%PDF-"))}}}}, acp.TextBlock("after")}
	prompt, err := session.mapPrompt(t.Context(), blocks)
	require.NoError(t, err)
	require.Len(t, prompt.content, 3)
	require.Equal(t, "before", prompt.content[0].Text)
	require.Equal(t, "document", prompt.content[1].Type)
	require.Equal(t, mime, prompt.content[1].Source.MediaType)
	require.Equal(t, "after", prompt.content[2].Text)
}

func TestReplayRejectsInvalidImageBytes(t *testing.T) {
	t.Parallel()
	agent := NewAgent()
	agent.attach(newRecorder(), nil)
	session := agent.newSession(sessionStart{cwd: t.TempDir()})
	session.id = "replay-session"
	rows := [][]byte{[]byte(`{"type":"assistant","uuid":"image","message":{"id":"api-image","role":"assistant","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"%%%"}}]}}`)}
	err := session.replay(t.Context(), rows)
	require.Equal(t, "claude_restore_failed", requestErrorData(t, err)["error"])
}
