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

func TestCommandCatalogFollowsRuntime(t *testing.T) {
	t.Parallel()
	for _, commands := range []string{"native", "empty"} {
		t.Run(commands, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, WithEnv(map[string]string{fakeClaudeEnv: "1", "ACP_GO_CLAUDE_TEST_COMMANDS": commands}))
			h.initialize()
			session := h.newSession()
			h.rec.waitFor(t, func(updates []acp.SessionNotification) bool { return len(commandSnapshots(updates)) > 0 })
			initial := commandSnapshots(h.rec.snapshot())[0]
			if commands == "empty" {
				require.Empty(t, initial)
			} else {
				require.Len(t, initial, 1)
				require.Equal(t, "compact", initial[0].Name)
			}
			_, err := h.prompt(session.SessionId, "CRASH", nil)
			require.Equal(t, "claude_turn_failed", requestErrorData(t, err)["error"])
			_, err = h.prompt(session.SessionId, "HELLO", nil)
			require.NoError(t, err)
			catalogs := commandSnapshots(h.rec.snapshot())
			require.Len(t, catalogs, 2)
			require.Equal(t, initial, catalogs[len(catalogs)-1])
		})
	}
}
func commandSnapshots(updates []acp.SessionNotification) [][]acp.AvailableCommand {
	var snapshots [][]acp.AvailableCommand
	for _, update := range updates {
		if commands := update.Update.AvailableCommandsUpdate; commands != nil {
			snapshots = append(snapshots, commands.AvailableCommands)
		}
	}

	return snapshots
}
