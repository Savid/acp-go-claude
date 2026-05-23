//go:build integration

package integration

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	claudeacp "github.com/savid/acp-go-claude"
	"github.com/stretchr/testify/require"
)

const (
	claudeSessionImportChunkMethod       = "_claude/session/importChunk"
	claudeSessionCommitImportMethod      = "_claude/session/commitImport"
	claudeSessionImportFormat            = "claude-jsonl"
	resumeFromFileExampleSessionID       = "e7a7b6ad-0ff8-4e04-b39b-5df77a720fd7"
	resumeFromFileExampleUserText        = "Say hello in one short sentence."
	resumeFromFileExampleAssistantText   = "Hello from the saved session."
	resumeFromFileImportedResumeSentinel = "ACP_IMPORTED_RESUME_OK"
)

func TestClaudeCLISessionImportChunkCommitReplaceLoadAndResume(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	cwd := t.TempDir()
	store := newRecordingSessionStore()
	entries := readExampleSessionJSONL(t)

	client := &recordingClient{}
	conn := connectLiveAgent(t, ctx, client, acp.InitializeRequest{}, claudeacp.WithSessionStore(store))

	importSessionEntries(t, ctx, conn, "integration-import-1", cwd, entries)
	importSessionEntries(t, ctx, conn, "integration-import-2", cwd, entries)
	require.Equal(t, 1, store.replaceCount())

	_, err := conn.LoadSession(ctx, acp.LoadSessionRequest{
		SessionId:  acp.SessionId(resumeFromFileExampleSessionID),
		Cwd:        cwd,
		McpServers: []acp.McpServer{},
	})
	require.NoError(t, err)
	require.Contains(t, client.text(), resumeFromFileExampleUserText)
	require.Contains(t, client.text(), resumeFromFileExampleAssistantText)
	_, err = conn.CloseSession(ctx, acp.CloseSessionRequest{SessionId: acp.SessionId(resumeFromFileExampleSessionID)})
	require.NoError(t, err)

	client = &recordingClient{}
	conn = connectLiveAgent(t, ctx, client, acp.InitializeRequest{}, claudeacp.WithSessionStore(store))
	_, err = conn.ResumeSession(ctx, acp.ResumeSessionRequest{
		SessionId:  acp.SessionId(resumeFromFileExampleSessionID),
		Cwd:        cwd,
		McpServers: []acp.McpServer{},
	})
	require.NoError(t, err)

	resp, err := conn.Prompt(ctx, acp.PromptRequest{
		SessionId: acp.SessionId(resumeFromFileExampleSessionID),
		Prompt: []acp.ContentBlock{
			acp.TextBlock("Reply exactly " + resumeFromFileImportedResumeSentinel + " and do not use tools."),
		},
	})
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, resp.StopReason)
	require.Contains(t, client.text(), resumeFromFileImportedResumeSentinel)

	_, err = conn.CloseSession(ctx, acp.CloseSessionRequest{SessionId: acp.SessionId(resumeFromFileExampleSessionID)})
	require.NoError(t, err)
}

func readExampleSessionJSONL(t *testing.T) []json.RawMessage {
	t.Helper()

	path := filepath.Join("..", "examples", "resume-from-file", "session.jsonl")
	file, err := os.Open(path) // #nosec G304 -- fixed repository test fixture path.
	require.NoError(t, err)
	defer func() { require.NoError(t, file.Close()) }()

	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

	var entries []json.RawMessage
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(line) == 0 {
			continue
		}

		entry := append(json.RawMessage(nil), line...)
		require.True(t, json.Valid(entry), "invalid fixture JSONL entry: %s", string(entry))
		entries = append(entries, entry)
	}
	require.NoError(t, scanner.Err())
	require.NotEmpty(t, entries)

	return entries
}

func importSessionEntries(
	t *testing.T,
	ctx context.Context,
	conn *acp.ClientSideConnection,
	importID string,
	cwd string,
	entries []json.RawMessage,
) {
	t.Helper()

	raw, err := conn.CallExtension(ctx, claudeSessionImportChunkMethod, map[string]any{
		"importId":  importID,
		"sessionId": resumeFromFileExampleSessionID,
		"cwd":       cwd,
		"format":    claudeSessionImportFormat,
		"offset":    0,
		"entries":   entries,
	})
	require.NoError(t, err)

	var chunk struct {
		ImportID string `json:"importId"`
		Offset   int    `json:"offset"`
		Entries  int    `json:"entries"`
	}
	require.NoError(t, json.Unmarshal(raw, &chunk))
	require.Equal(t, importID, chunk.ImportID)
	require.Equal(t, len(entries), chunk.Offset)
	require.Equal(t, len(entries), chunk.Entries)

	raw, err = conn.CallExtension(ctx, claudeSessionCommitImportMethod, map[string]any{"importId": importID})
	require.NoError(t, err)

	var commit struct {
		ImportID  string `json:"importId"`
		SessionID string `json:"sessionId"`
		Entries   int    `json:"entries"`
		SHA256    string `json:"sha256"`
	}
	require.NoError(t, json.Unmarshal(raw, &commit))
	require.Equal(t, importID, commit.ImportID)
	require.Equal(t, resumeFromFileExampleSessionID, commit.SessionID)
	require.Equal(t, len(entries), commit.Entries)
	require.NotEmpty(t, commit.SHA256)
}
