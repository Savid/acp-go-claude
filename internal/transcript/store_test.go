package transcript

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/savid/acp-go-claude/internal/mapper"
	"github.com/stretchr/testify/require"
)

const testSessionID = "11111111-1111-4111-8111-111111111111"

// replayLines replays rows the way the session store hands them over: whole
// stored values, one per row.
func replayLines(lines ...string) ([]acp.SessionUpdate, bool) {
	rows := make([]json.RawMessage, 0, len(lines))
	for _, line := range lines {
		rows = append(rows, json.RawMessage(line))
	}

	return ReplayEntries(rows)
}

func TestStoreListFindAndReplay(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	path := writeTestTranscript(t, home, "/repo", testSessionID, []string{
		`{"type":"user","uuid":"22222222-2222-4222-8222-222222222222","cwd":"/repo","sessionId":"11111111-1111-4111-8111-111111111111","timestamp":"2026-05-14T00:00:00Z","message":{"content":"hello"}}`,
		`{"type":"ai-title","sessionId":"11111111-1111-4111-8111-111111111111","aiTitle":"Greeting"}`,
		`{"type":"assistant","uuid":"33333333-3333-4333-8333-333333333333","cwd":"/repo","sessionId":"11111111-1111-4111-8111-111111111111","timestamp":"2026-05-14T00:00:01Z","message":{"content":[{"type":"text","text":"hi"}]}}`,
		`{"type":"result","subtype":"success","total_cost_usd":0.25,"usage":{"input_tokens":8,"output_tokens":3,"cache_read_input_tokens":2},"modelUsage":{"claude-test":{"contextWindow":200000}}}`,
	})

	cwd := "/repo"
	sessions, err := Store{ClaudeHome: home}.List(context.Background(), &cwd, nil)
	require.NoError(t, err)
	require.Len(t, sessions, 1)
	require.Equal(t, acp.SessionId(testSessionID), sessions[0].Info.SessionId)
	require.Equal(t, "Greeting", *sessions[0].Info.Title)
	require.Equal(t, "/repo", sessions[0].Info.Cwd)
	require.Equal(t, path, sessions[0].Path)

	found, err := Store{ClaudeHome: home}.Find(context.Background(), testSessionID, "/repo")
	require.NoError(t, err)
	require.Equal(t, path, found.Path)

	updates, truncated := replayLines(
		`{"type":"user","uuid":"22222222-2222-4222-8222-222222222222","cwd":"/repo","sessionId":"11111111-1111-4111-8111-111111111111","timestamp":"2026-05-14T00:00:00Z","message":{"content":"hello"}}`,
		`{"type":"ai-title","sessionId":"11111111-1111-4111-8111-111111111111","aiTitle":"Greeting"}`,
		`{"type":"assistant","uuid":"33333333-3333-4333-8333-333333333333","cwd":"/repo","sessionId":"11111111-1111-4111-8111-111111111111","timestamp":"2026-05-14T00:00:01Z","message":{"content":[{"type":"text","text":"hi"}]}}`,
		`{"type":"result","subtype":"success","total_cost_usd":0.25,"usage":{"input_tokens":8,"output_tokens":3,"cache_read_input_tokens":2},"modelUsage":{"claude-test":{"contextWindow":200000}}}`,
	)
	require.False(t, truncated)
	require.Len(t, updates, 4)
	require.Equal(t, "hello", updates[0].UserMessageChunk.Content.Text.Text)
	require.Equal(t, "Greeting", *updates[1].SessionInfoUpdate.Title)
	require.Equal(t, "hi", updates[2].AgentMessageChunk.Content.Text.Text)
	claudeMeta, ok := updates[2].AgentMessageChunk.Meta["claude"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "33333333-3333-4333-8333-333333333333", claudeMeta["messageId"])
	require.Equal(t, 0.25, updates[3].UsageUpdate.Cost.Amount)
	require.Equal(t, "USD", updates[3].UsageUpdate.Cost.Currency)
	require.Equal(t, 200000, updates[3].UsageUpdate.Size)
	require.Equal(t, 13, updates[3].UsageUpdate.Used)
}

func TestReplayEntriesUsesStoreRows(t *testing.T) {
	t.Parallel()

	updates, truncated := ReplayEntries([]json.RawMessage{
		json.RawMessage(`{"type":"user","uuid":"22222222-2222-4222-8222-222222222222","cwd":"/repo","message":{"content":"stored prompt"}}`),
		json.RawMessage(`   `),
		json.RawMessage(`{"type":"assistant","uuid":"33333333-3333-4333-8333-333333333333","cwd":"/repo","message":{"content":[{"type":"text","text":"stored answer"}]}}`),
	})
	require.False(t, truncated)
	require.Len(t, updates, 2)
	require.Equal(t, "stored prompt", updates[0].UserMessageChunk.Content.Text.Text)
	require.Equal(t, "stored answer", updates[1].AgentMessageChunk.Content.Text.Text)
}

func TestStoreListAllSortsAndDedupes(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	first := writeTestTranscript(t, home, "/repo", testSessionID, []string{
		`{"type":"user","uuid":"22222222-2222-4222-8222-222222222222","cwd":"/repo","message":{"content":"old"}}`,
	})
	second := writeTestTranscript(t, home, "/other", testSessionID, []string{
		`{"type":"user","uuid":"33333333-3333-4333-8333-333333333333","cwd":"/other","message":{"content":"new"}}`,
	})

	require.NoError(t, os.Chtimes(first, testTime(1), testTime(1)))
	require.NoError(t, os.Chtimes(second, testTime(2), testTime(2)))

	sessions, err := Store{ClaudeHome: home}.List(context.Background(), nil, nil)
	require.NoError(t, err)
	require.Len(t, sessions, 1)
	require.Equal(t, "/other", sessions[0].Info.Cwd)
}

func TestStoreListForCwdFallsBackToTranscriptCwd(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	target := filepath.Join(t.TempDir(), "target")
	require.NoError(t, os.MkdirAll(target, 0o755))

	writeTestTranscript(t, home, "/stored-elsewhere", testSessionID, []string{
		`{"type":"user","uuid":"22222222-2222-4222-8222-222222222222","cwd":"` + target + `","message":{"content":"hello"}}`,
	})

	sessions, err := Store{ClaudeHome: home}.List(context.Background(), &target, nil)
	require.NoError(t, err)
	require.Len(t, sessions, 1)
	require.Equal(t, target, sessions[0].Info.Cwd)
}

func TestStoreListForCwdFiltersSanitizedDirectoryCollisions(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	root := t.TempDir()
	target := filepath.Join(root, "foo", "bar")
	other := filepath.Join(root, "foo-bar")
	require.Equal(t, ProjectDirName(target), ProjectDirName(other))
	require.NoError(t, os.MkdirAll(target, 0o755))
	require.NoError(t, os.MkdirAll(other, 0o755))

	writeTestTranscript(t, home, target, testSessionID, []string{
		`{"type":"user","uuid":"22222222-2222-4222-8222-222222222222","cwd":"` + target + `","message":{"content":"target"}}`,
	})
	writeTestTranscript(t, home, other, "22222222-2222-4222-8222-222222222222", []string{
		`{"type":"user","uuid":"33333333-3333-4333-8333-333333333333","cwd":"` + other + `","message":{"content":"other"}}`,
	})

	sessions, err := Store{ClaudeHome: home}.List(context.Background(), &target, nil)
	require.NoError(t, err)
	require.Len(t, sessions, 1)
	require.Equal(t, acp.SessionId(testSessionID), sessions[0].Info.SessionId)
	require.Equal(t, target, sessions[0].Info.Cwd)
}

func TestStoreFiltersAdditionalDirsAndMissing(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	sessions, err := Store{ClaudeHome: home}.List(context.Background(), nil, []string{"/extra"})
	require.NoError(t, err)
	require.Empty(t, sessions)

	_, err = Store{ClaudeHome: home}.Find(context.Background(), testSessionID, "/repo")
	require.ErrorIs(t, err, os.ErrNotExist)

	_, err = Store{ClaudeHome: home}.Find(context.Background(), testSessionID, "")
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestStoreListErrors(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := Store{ClaudeHome: t.TempDir()}.List(ctx, nil, nil)
	require.ErrorIs(t, err, context.Canceled)

	home := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(home, projectsDirName), []byte("file"), 0o600))

	_, err = Store{ClaudeHome: home}.List(context.Background(), nil, nil)
	require.Error(t, err)

	_, err = Store{ClaudeHome: home}.readSessionsFromDir(context.Background(), filepath.Join(home, projectsDirName), "")
	require.Error(t, err)

	dir := filepath.Join(t.TempDir(), "project")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, testSessionID+".jsonl"), []byte("{}\n"), 0o600))

	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	_, err = Store{ClaudeHome: home}.readSessionsFromDir(canceled, dir, "")
	require.ErrorIs(t, err, context.Canceled)

	cancelHome := t.TempDir()
	require.NoError(t, os.MkdirAll(filepath.Join(cancelHome, projectsDirName, "project"), 0o755))
	_, err = Store{ClaudeHome: cancelHome}.listAll(canceled)
	require.ErrorIs(t, err, context.Canceled)

	missing, err := Store{ClaudeHome: home}.readSessionsFromDir(context.Background(), filepath.Join(t.TempDir(), "missing"), "")
	require.NoError(t, err)
	require.Empty(t, missing)

	_, err = Store{ClaudeHome: home}.Find(canceled, testSessionID, "")
	require.ErrorIs(t, err, context.Canceled)
}

func TestStoreListEdgeBranches(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	cwd := "/repo"
	projectDir := filepath.Join(home, projectsDirName, ProjectDirName(cwd))
	require.NoError(t, os.MkdirAll(projectDir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(projectDir, testSessionID+".jsonl"), []byte("{"+strings.Repeat("x", 10*1024*1024+1)), 0o600))

	sessions, err := Store{ClaudeHome: home}.List(context.Background(), &cwd, nil)
	require.NoError(t, err)
	require.Empty(t, sessions)

	fallbackHome := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(fallbackHome, projectsDirName), []byte("file"), 0o600))
	_, err = Store{ClaudeHome: fallbackHome}.List(context.Background(), &cwd, nil)
	require.Error(t, err)

	home = t.TempDir()
	projects := filepath.Join(home, projectsDirName)
	require.NoError(t, os.MkdirAll(filepath.Join(projects, "project"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(projects, "loose.jsonl"), []byte("{}\n"), 0o600))
	require.NoError(t, os.WriteFile(filepath.Join(projects, "project", "note.txt"), []byte("skip"), 0o600))
	require.NoError(t, os.Mkdir(filepath.Join(projects, "project", "nested"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(projects, "project", testSessionID+".jsonl"), []byte("{"+strings.Repeat("x", 10*1024*1024+1)), 0o600))

	sessions, err = Store{ClaudeHome: home}.List(context.Background(), nil, nil)
	require.NoError(t, err)
	require.Empty(t, sessions)

	home = t.TempDir()
	dir := filepath.Join(home, projectsDirName, "project")
	require.NoError(t, os.MkdirAll(dir, 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, "note.txt"), []byte("skip"), 0o600))
	require.NoError(t, os.Mkdir(filepath.Join(dir, "nested"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(dir, testSessionID+".jsonl"), []byte(
		`{"type":"user","isMeta":true,"message":{"content":"hidden"}}`+"\n",
	), 0o600))

	sessions, err = Store{ClaudeHome: home}.readSessionsFromDir(context.Background(), dir, "")
	require.NoError(t, err)
	require.Empty(t, sessions)

	if runtime.GOOS != "windows" {
		errorHome := t.TempDir()
		errorCwd := "/error"
		errorDir := filepath.Join(errorHome, projectsDirName, ProjectDirName(errorCwd))
		require.NoError(t, os.MkdirAll(errorDir, 0o755))
		require.NoError(t, os.Symlink(t.TempDir(), filepath.Join(errorDir, testSessionID+".jsonl")))

		_, err = Store{ClaudeHome: errorHome}.List(context.Background(), &errorCwd, nil)
		require.Error(t, err)

		_, err = Store{ClaudeHome: errorHome}.List(context.Background(), nil, nil)
		require.Error(t, err)
	}
}

func TestStoreConfigAndCanonicalFallbacks(t *testing.T) {
	abs := storeAbs
	open := storeOpen
	userHomeDir := storeUserHomeDir
	t.Cleanup(func() {
		storeAbs = abs
		storeOpen = open
		storeUserHomeDir = userHomeDir
	})

	storeUserHomeDir = func() (string, error) {
		return "", errors.New("home failed")
	}
	t.Setenv("CLAUDE_CONFIG_DIR", "")
	require.Equal(t, filepath.Clean(".claude"), Store{}.configHome())

	storeAbs = func(string) (string, error) {
		return "", errors.New("abs failed")
	}
	require.Equal(t, filepath.Clean("relative/path"), canonicalPath("relative/path"))

	home := t.TempDir()
	path := writeTestTranscript(t, home, "/repo", testSessionID, []string{
		`{"type":"user","message":{"content":"hello"}}`,
	})
	storeOpen = func(string) (transcriptFile, error) {
		return nil, errors.New("open failed")
	}
	_, err := readSession(path, "")
	require.Error(t, err)
}

func TestStoreFindFallsBackToAllWhenCwdEmpty(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	path := writeTestTranscript(t, home, "/repo", testSessionID, []string{
		`{"type":"user","uuid":"22222222-2222-4222-8222-222222222222","cwd":"/repo","message":{"content":"hello"}}`,
	})

	found, err := Store{ClaudeHome: home}.Find(context.Background(), testSessionID, "")
	require.NoError(t, err)
	require.Equal(t, path, found.Path)
}

func TestStoreUsesConfigDirEnvironment(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CLAUDE_CONFIG_DIR", home)

	writeTestTranscript(t, home, "/repo", testSessionID, []string{
		`{"type":"user","uuid":"22222222-2222-4222-8222-222222222222","cwd":"/repo","message":{"content":"hello"}}`,
	})

	sessions, err := Store{}.List(context.Background(), nil, nil)
	require.NoError(t, err)
	require.Len(t, sessions, 1)

	if runtime.GOOS != "windows" {
		homeDir := t.TempDir()
		t.Setenv("CLAUDE_CONFIG_DIR", "")
		t.Setenv("HOME", homeDir)

		require.Equal(t, filepath.Join(homeDir, ".claude"), Store{}.configHome())
	}
}

func TestReplaySkipsInvalidAndHiddenEntries(t *testing.T) {
	t.Parallel()

	updates, truncated := replayLines(
		`not-json`,
		`{"type":"user","uuid":"22222222-2222-4222-8222-222222222222","isMeta":true,"message":{"content":"hidden"}}`,
		`{"type":"user","uuid":"33333333-3333-4333-8333-333333333333","message":{"content":[{"type":"text","text":"visible"},{"type":"image","data":"abc"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"def"}},{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"pdf"}},{"type":"tool_result","tool_use_id":"tool-1","content":"tool output"},{"type":"tool_result","tool_use_id":"tool-2","is_error":true,"content":[{"type":"text","text":"failed"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"img"}},{"type":"document","source":{"type":"base64","media_type":"application/pdf","data":"doc"}},"bad"]}]}}`,
		`{"type":"assistant","message":{"content":"done"}}`,
	)
	require.False(t, truncated)
	require.Len(t, updates, 6)
	require.Equal(t, "visible", updates[0].UserMessageChunk.Content.Text.Text)
	require.Equal(t, "def", updates[1].UserMessageChunk.Content.Image.Data)
	require.Equal(t, "image/png", updates[1].UserMessageChunk.Content.Image.MimeType)
	require.Equal(t, "pdf", updates[2].UserMessageChunk.Content.Resource.Resource.BlobResourceContents.Blob)
	require.Equal(t, "application/pdf", *updates[2].UserMessageChunk.Content.Resource.Resource.BlobResourceContents.MimeType)
	require.Equal(t, acp.ToolCallId("tool-1"), updates[3].ToolCallUpdate.ToolCallId)
	require.Equal(t, acp.ToolCallStatusCompleted, *updates[3].ToolCallUpdate.Status)
	require.Equal(t, "tool output", updates[3].ToolCallUpdate.Content[0].Content.Content.Text.Text)
	require.Equal(t, acp.ToolCallId("tool-2"), updates[4].ToolCallUpdate.ToolCallId)
	require.Equal(t, acp.ToolCallStatusFailed, *updates[4].ToolCallUpdate.Status)
	require.Len(t, updates[4].ToolCallUpdate.Content, 3)
	require.Equal(t, "failed", updates[4].ToolCallUpdate.Content[0].Content.Content.Text.Text)
	require.Equal(t, "img", updates[4].ToolCallUpdate.Content[1].Content.Content.Image.Data)
	require.Equal(t, "doc", updates[4].ToolCallUpdate.Content[2].Content.Content.Resource.Resource.BlobResourceContents.Blob)
	require.Equal(t, "done", updates[5].AgentMessageChunk.Content.Text.Text)
}

func TestReplayStripsLocalCommandMetadata(t *testing.T) {
	t.Parallel()

	lines := []string{
		`{"type":"user","uuid":"22222222-2222-4222-8222-222222222222","cwd":"/repo","message":{"content":"<command-name>/model</command-name><command-message>model</command-message><command-args>opus</command-args><local-command-stdout>ok</local-command-stdout>"}}`,
		`{"type":"user","uuid":"33333333-3333-4333-8333-333333333333","cwd":"/repo","message":{"content":"<command-name>/model</command-name><local-command-stdout>ok</local-command-stdout>hi"}}`,
		`{"type":"user","uuid":"44444444-4444-4444-8444-444444444444","cwd":"/repo","message":{"content":[{"type":"text","text":"<command-name>/model</command-name>"},{"type":"text","text":"<local-command-stdout>ok</local-command-stdout>there"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"img"}}]}}`,
	}

	path := writeTestTranscript(t, t.TempDir(), "/repo", testSessionID, lines)

	session, err := readSession(path, "")
	require.NoError(t, err)
	require.Equal(t, "hi", *session.Info.Title)

	updates, truncated := replayLines(lines...)
	require.False(t, truncated)
	require.Len(t, updates, 3)
	require.Equal(t, "hi", updates[0].UserMessageChunk.Content.Text.Text)
	require.Equal(t, "there", updates[1].UserMessageChunk.Content.Text.Text)
	require.Equal(t, "img", updates[2].UserMessageChunk.Content.Image.Data)
	require.Empty(t, entryUpdatesWithOptions(map[string]any{
		"type":    "user",
		"message": map[string]any{"content": "<local-command-stderr>bad</local-command-stderr>"},
	}, mapper.ToolUpdateOptions{}))
}

func TestReplayKeepsToolUseCacheAcrossEntries(t *testing.T) {
	t.Parallel()

	updates, truncated := replayLines(
		`{"type":"assistant","uuid":"22222222-2222-4222-8222-222222222222","cwd":"/repo","message":{"content":[{"type":"tool_use","id":"tool-1","name":"Read","input":{"file_path":"/repo/main.go"}}]}}`,
		`{"type":"user","uuid":"33333333-3333-4333-8333-333333333333","cwd":"/repo","message":{"content":[{"type":"tool_result","tool_use_id":"tool-1","content":"`+"```go\\nfmt.Println(1)\\n```"+`"}]}}`,
	)
	require.False(t, truncated)
	require.Len(t, updates, 2)
	require.Equal(t, acp.ToolCallId("tool-1"), updates[0].ToolCall.ToolCallId)
	require.Equal(t, "Read main.go", updates[0].ToolCall.Title)
	require.Equal(t, acp.ToolCallStatusCompleted, *updates[1].ToolCallUpdate.Status)
	require.Contains(t, updates[1].ToolCallUpdate.Content[0].Content.Content.Text.Text, "````\n```go")
	meta, ok := updates[1].ToolCallUpdate.Meta["claude"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "Read", meta["toolName"])
}

func TestReplayUsesToolUseCacheAcrossOutOfOrderEntries(t *testing.T) {
	t.Parallel()

	updates, truncated := replayLines(
		`{"type":"user","uuid":"33333333-3333-4333-8333-333333333333","cwd":"/repo","message":{"content":[{"type":"tool_result","tool_use_id":"tool-1","content":"`+"```go\\nfmt.Println(1)\\n```"+`"}]}}`,
		`{"type":"assistant","uuid":"22222222-2222-4222-8222-222222222222","cwd":"/repo","message":{"content":[{"type":"tool_use","id":"tool-1","name":"Read","input":{"file_path":"/repo/main.go"}}]}}`,
	)
	require.False(t, truncated)
	require.Len(t, updates, 2)
	require.Equal(t, acp.ToolCallStatusCompleted, *updates[0].ToolCallUpdate.Status)
	require.Contains(t, updates[0].ToolCallUpdate.Content[0].Content.Content.Text.Text, "````\n```go")
	meta, ok := updates[0].ToolCallUpdate.Meta["claude"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "Read", meta["toolName"])
	require.Equal(t, acp.ToolCallId("tool-1"), updates[1].ToolCall.ToolCallId)
	require.Equal(t, "Read main.go", updates[1].ToolCall.Title)
}

func TestTranscriptAcceptsLargeJSONLines(t *testing.T) {
	t.Parallel()

	large := strings.Repeat("x", 10*1024*1024+1)
	line := `{"type":"user","uuid":"22222222-2222-4222-8222-222222222222","cwd":"/repo","message":{"content":"` + large + `"}}`
	path := writeTestTranscript(t, t.TempDir(), "/repo", testSessionID, []string{line})

	updates, truncated := replayLines(line)
	require.False(t, truncated)
	require.Len(t, updates, 1)
	require.Len(t, updates[0].UserMessageChunk.Content.Text.Text, len(large))

	session, err := readSession(path, "")
	require.NoError(t, err)
	require.Len(t, []rune(*session.Info.Title), maxTitleLength)
}

func TestReplayUpdatesCapsUpdateCount(t *testing.T) {
	t.Parallel()

	lines := make([]string, maxReplayUpdates+1)
	for i := range lines {
		lines[i] = `{"type":"user","uuid":"22222222-2222-4222-8222-222222222222","cwd":"/repo","message":{"content":"hello"}}`
	}

	updates, truncated := replayLines(lines...)
	require.True(t, truncated)
	require.Len(t, updates, maxReplayUpdates)
}

func TestReplayUpdatesCapsPartialEntry(t *testing.T) {
	t.Parallel()

	lines := make([]string, maxReplayUpdates)
	for i := range maxReplayUpdates - 1 {
		lines[i] = `{"type":"user","uuid":"22222222-2222-4222-8222-222222222222","cwd":"/repo","message":{"content":"hello"}}`
	}
	lines[maxReplayUpdates-1] = `{"type":"user","uuid":"22222222-2222-4222-8222-222222222222","cwd":"/repo","message":{"content":[{"type":"text","text":"first"},{"type":"text","text":"second"}]}}`

	updates, truncated := replayLines(lines...)
	require.True(t, truncated)
	require.Len(t, updates, maxReplayUpdates)
	require.Equal(t, "first", updates[maxReplayUpdates-1].UserMessageChunk.Content.Text.Text)
}

func TestTranscriptMalformedLinesAreCounted(t *testing.T) {
	t.Parallel()

	lines := []string{
		`{bad`,
		`{"type":"user","uuid":"22222222-2222-4222-8222-222222222222","cwd":"/repo","message":{"content":"hello"}}`,
	}

	path := writeTestTranscript(t, t.TempDir(), "/repo", testSessionID, lines)

	updates, truncated := replayLines(lines...)
	require.False(t, truncated)
	require.Len(t, updates, 1)

	session, err := readSession(path, "")
	require.NoError(t, err)
	require.Equal(t, "hello", *session.Info.Title)
}

func TestTranscriptMalformedFinalLineIsTornLine(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	projectDir := filepath.Join(home, projectsDirName, ProjectDirName("/repo"))
	require.NoError(t, os.MkdirAll(projectDir, 0o755))

	path := filepath.Join(projectDir, testSessionID+".jsonl")
	require.NoError(t, os.WriteFile(path, []byte(
		`{"type":"user","uuid":"22222222-2222-4222-8222-222222222222","cwd":"/repo","message":{"content":"hello"}}`+"\n"+
			`{bad`,
	), 0o600))

	session, err := readSession(path, "")
	require.NoError(t, err)
	require.Equal(t, "hello", *session.Info.Title)
}

func TestReadSessionTitleFallbacks(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	longPrompt := strings.Repeat("a", 300)
	path := writeTestTranscript(t, home, "/repo", testSessionID, []string{
		`{"type":"user","uuid":"22222222-2222-4222-8222-222222222222","cwd":"/repo","message":{"content":"` + longPrompt + `"}}`,
	})

	session, err := readSession(path, "")
	require.NoError(t, err)
	require.NotNil(t, session.Info.Title)
	require.Len(t, []rune(*session.Info.Title), maxTitleLength)
	require.True(t, strings.HasSuffix(*session.Info.Title, "..."))

	longUnicodePrompt := strings.Repeat("a", 250) + "\U0001f4bb" + strings.Repeat("b", 80)
	path = writeTestTranscript(t, home, "/unicode-title", "55555555-5555-4555-8555-555555555555", []string{
		`{"type":"user","uuid":"22222222-2222-4222-8222-222222222222","cwd":"/unicode-title","message":{"content":"` + longUnicodePrompt + `"}}`,
	})

	session, err = readSession(path, "")
	require.NoError(t, err)
	require.NotNil(t, session.Info.Title)
	require.True(t, utf8.ValidString(*session.Info.Title))
	require.Contains(t, *session.Info.Title, "\U0001f4bb")
	require.Len(t, []rune(*session.Info.Title), maxTitleLength)

	path = writeTestTranscript(t, home, "/whitespace-title", "66666666-6666-4666-8666-666666666666", []string{
		"{\"type\":\"user\",\"uuid\":\"22222222-2222-4222-8222-222222222222\",\"cwd\":\"/whitespace-title\",\"message\":{\"content\":\" hello\\n\\tthere  now \"}}",
	})

	session, err = readSession(path, "")
	require.NoError(t, err)
	require.Equal(t, "hello there now", *session.Info.Title)

	path = writeTestTranscript(t, home, "/image-title", "77777777-7777-4777-8777-777777777777", []string{
		`{"type":"user","uuid":"22222222-2222-4222-8222-222222222222","cwd":"/image-title","message":{"content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"img"}},{"type":"text","text":"text after image"}]}}`,
	})

	session, err = readSession(path, "")
	require.NoError(t, err)
	require.Equal(t, "text after image", *session.Info.Title)

	path = writeTestTranscript(t, home, "/other", "22222222-2222-4222-8222-222222222222", []string{
		`{"type":"assistant","uuid":"33333333-3333-4333-8333-333333333333","cwd":"/other","customTitle":"Custom","message":{"content":[{"type":"text","text":"hi"}]}}`,
	})

	session, err = readSession(path, "")
	require.NoError(t, err)
	require.Equal(t, "Custom", *session.Info.Title)

	path = writeTestTranscript(t, home, "/summary", "33333333-3333-4333-8333-333333333333", []string{
		`{"type":"assistant","uuid":"33333333-3333-4333-8333-333333333333","cwd":"/summary","summary":"Summary","message":{"content":[{"type":"text","text":"hi"}]}}`,
	})

	session, err = readSession(path, "")
	require.NoError(t, err)
	require.Equal(t, "Summary", *session.Info.Title)

	path = writeTestTranscript(t, home, "/id-title", "44444444-4444-4444-8444-444444444444", []string{
		`{"type":"user","uuid":"33333333-3333-4333-8333-333333333333","cwd":"/id-title","message":{"content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"img"}}]}}`,
	})

	session, err = readSession(path, "")
	require.NoError(t, err)
	require.Equal(t, "44444444-4444-4444-8444-444444444444", *session.Info.Title)

	require.False(t, visibleUserEntry(map[string]any{keyMessage: map[string]any{keyContent: []any{map[string]any{
		keyType: contentTypeImage,
		keySource: map[string]any{
			keyMediaType: "image/png",
		},
	}}}}))
	require.False(t, visibleUserBlock(map[string]any{keyType: "future"}))
	require.False(t, visibleUserEntry(map[string]any{}))
	require.Empty(t, firstUserPrompt(map[string]any{}))
	require.Empty(t, firstUserPrompt(map[string]any{keyMessage: map[string]any{keyContent: []any{map[string]any{
		keyType:         contentTypeText,
		contentTypeText: "   ",
	}}}}))
}

func TestReadSessionErrorsAndFallbackCwd(t *testing.T) {
	t.Parallel()

	_, err := readSession(filepath.Join(t.TempDir(), "missing.jsonl"), "")
	require.Error(t, err)

	if runtime.GOOS != "windows" {
		_, err = readSession(t.TempDir(), "")
		require.Error(t, err)
	}

	home := t.TempDir()
	path := writeTestTranscript(t, home, "/hidden", testSessionID, []string{
		`{"type":"user","isMeta":true,"cwd":"/hidden","message":{"content":"hidden"}}`,
	})

	_, err = readSession(path, "")
	require.ErrorIs(t, err, errNoVisibleTranscript)

	path = writeTestTranscript(t, home, "/fallback", "55555555-5555-4555-8555-555555555555", []string{
		`not-json`,
		`{"type":"user","message":{"content":"hello"}}`,
	})

	session, err := readSession(path, "/fallback-cwd")
	require.NoError(t, err)
	require.Equal(t, "/fallback-cwd", session.Info.Cwd)
}

func TestUserContentUpdateInvalids(t *testing.T) {
	t.Parallel()

	for _, block := range []map[string]any{
		{"type": "text", "text": " "},
		{"type": "image", "data": "abc"},
		{"type": "document", "source": map[string]any{"data": "pdf"}},
		{"type": "tool_result", "content": "missing id"},
		{"type": "future"},
	} {
		require.Empty(t, userContentUpdates(block, mapper.ToolUpdateOptions{}))
	}

	updates := userContentUpdates(map[string]any{
		"type":      "image",
		"data":      "abc",
		"mime_type": "image/png",
	}, mapper.ToolUpdateOptions{})
	require.Len(t, updates, 1)
	require.Equal(t, "abc", updates[0].UserMessageChunk.Content.Image.Data)

	var ok bool
	_, ok = transcriptContentBlock(map[string]any{"type": "future"})
	require.False(t, ok)
	_, ok = transcriptContentBlock(map[string]any{"type": "text", "text": " "})
	require.False(t, ok)
	content, ok := transcriptContentBlock(map[string]any{"type": "text", "text": "tool text"})
	require.True(t, ok)
	require.Equal(t, "tool text", content.Text.Text)

	require.Nil(t, transcriptToolResultContent(" "))
	require.Nil(t, transcriptToolResultContent(123))
}

func TestTranscriptHelperEdges(t *testing.T) {
	t.Parallel()

	require.Nil(t, entryUpdatesWithOptions(map[string]any{"type": "ai-title"}, mapper.ToolUpdateOptions{}))
	require.Nil(t, entryUpdatesWithOptions(map[string]any{"type": "future"}, mapper.ToolUpdateOptions{}))
	require.Empty(t, entryUpdatesWithOptions(map[string]any{"type": "assistant", "message": map[string]any{"content": 1}}, mapper.ToolUpdateOptions{}))
	require.Nil(t, entryUpdatesWithOptions(map[string]any{"type": "assistant", "isMeta": true}, mapper.ToolUpdateOptions{}))
	require.Nil(t, entryUpdatesWithOptions(map[string]any{"type": "assistant"}, mapper.ToolUpdateOptions{}))

	require.Nil(t, userUpdatesWithOptions(map[string]any{}, mapper.ToolUpdateOptions{}))
	require.Nil(t, userUpdatesWithOptions(map[string]any{"message": map[string]any{"content": " "}}, mapper.ToolUpdateOptions{}))
	require.Nil(t, userUpdatesWithOptions(map[string]any{"message": map[string]any{"content": 1}}, mapper.ToolUpdateOptions{}))
	require.Empty(t, userUpdatesWithOptions(map[string]any{
		"message": map[string]any{
			"content": []any{"bad", map[string]any{"type": "future"}},
		},
	}, mapper.ToolUpdateOptions{}))

	toolUses := map[string]claude.ToolUseBlock{}
	collectEntryToolUses(map[string]any{
		"type":    "assistant",
		"message": map[string]any{"content": []any{map[string]any{"type": "tool_use"}}},
	}, toolUses)
	require.Empty(t, toolUses)
	require.Panics(t, func() {
		mustAssistantMessage(&claude.UnknownMessage{})
	})

	require.Nil(t, mappedToolResultUpdates(map[string]any{}, mapper.ToolUpdateOptions{}))
	require.Nil(t, mappedToolResultUpdates(
		map[string]any{"type": "future", keyToolUseID: "tool-1"},
		mapper.ToolUpdateOptions{ToolUses: map[string]claude.ToolUseBlock{
			"tool-1": {ID: "tool-1", Name: "Read"},
		}},
	))

	older := "2026-05-14T00:00:00Z"
	newer := "2026-05-14T00:00:01Z"
	invalid := "not-a-time"

	require.False(t, updatedAfter(nil, &older))
	require.True(t, updatedAfter(&newer, nil))
	require.True(t, updatedAfter(&newer, &older))
	require.False(t, updatedAfter(&invalid, &older))
	require.False(t, updatedAfter(&newer, &invalid))
	require.Empty(t, canonicalPath(" "))
	require.Equal(t, "-", ProjectDirName(""))
	entry, skipped := decodeLine("")
	require.Nil(t, entry)
	require.False(t, skipped)
	entry, skipped = decodeLine("{bad")
	require.Nil(t, entry)
	require.True(t, skipped)
	logSkippedTranscriptLines("transcript.jsonl", 1)

	sorted := sortAndDedupe([]Session{
		{Info: acp.SessionInfo{SessionId: "one", UpdatedAt: &older}},
		{Info: acp.SessionInfo{SessionId: "two", UpdatedAt: &newer}},
	})
	require.Equal(t, acp.SessionId("two"), sorted[0].Info.SessionId)

	if runtime.GOOS != "windows" {
		target := t.TempDir()
		link := filepath.Join(t.TempDir(), "link")
		require.NoError(t, os.Symlink(target, link))
		require.Equal(t, canonicalPath(target), canonicalPath(link))
	}
}

func writeTestTranscript(t *testing.T, home string, cwd string, sessionID string, lines []string) string {
	t.Helper()

	projectDir := filepath.Join(home, projectsDirName, ProjectDirName(cwd))
	require.NoError(t, os.MkdirAll(projectDir, 0o755))

	path := filepath.Join(projectDir, sessionID+".jsonl")
	content := strings.Join(lines, "\n")
	if len(lines) > 0 {
		content += "\n"
	}

	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))

	return path
}

func testTime(multiplier int64) time.Time {
	return time.Unix(multiplier, 0)
}
