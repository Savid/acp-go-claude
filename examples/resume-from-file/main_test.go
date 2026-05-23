package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/require"
)

type fakeAgentConnection struct {
	initErr     error
	importErr   error
	loadErr     error
	promptErr   error
	promptErrAt int

	importMethod string
	importParams sessionImportParams
	loadCwd      string
	prompt       string
	prompts      []string
	closed       bool
	sessionID    acp.SessionId
	afterPrompt  func()
}

type errReader struct {
	err error
}

func (r errReader) Read([]byte) (int, error) {
	return 0, r.err
}

func (f *fakeAgentConnection) Initialize(context.Context, acp.InitializeRequest) (acp.InitializeResponse, error) {
	return acp.InitializeResponse{}, f.initErr
}

func (f *fakeAgentConnection) CallExtension(
	_ context.Context,
	method string,
	params any,
) (json.RawMessage, error) {
	f.importMethod = method

	raw, err := json.Marshal(params)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &f.importParams); err != nil {
		return nil, err
	}

	return json.RawMessage(`{"ok":true}`), f.importErr
}

func (f *fakeAgentConnection) LoadSession(
	_ context.Context,
	params acp.LoadSessionRequest,
) (acp.LoadSessionResponse, error) {
	f.sessionID = params.SessionId
	f.loadCwd = params.Cwd

	return acp.LoadSessionResponse{}, f.loadErr
}

func (f *fakeAgentConnection) Prompt(_ context.Context, params acp.PromptRequest) (acp.PromptResponse, error) {
	f.sessionID = params.SessionId
	if len(params.Prompt) > 0 && params.Prompt[0].Text != nil {
		f.prompt = params.Prompt[0].Text.Text
		f.prompts = append(f.prompts, f.prompt)
	}
	if f.promptErr != nil && (f.promptErrAt == 0 || len(f.prompts) == f.promptErrAt) {
		return acp.PromptResponse{}, f.promptErr
	}
	if f.afterPrompt != nil {
		f.afterPrompt()
	}

	return acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, nil
}

func (f *fakeAgentConnection) CloseSession(context.Context, acp.CloseSessionRequest) (acp.CloseSessionResponse, error) {
	f.closed = true

	return acp.CloseSessionResponse{}, nil
}

func TestClientFileMethods(t *testing.T) {
	t.Parallel()

	c := client{}
	path := filepath.Join(t.TempDir(), "note.txt")

	_, err := c.WriteTextFile(context.Background(), acp.WriteTextFileRequest{
		Path:    path,
		Content: "hello",
	})
	require.NoError(t, err)

	read, err := c.ReadTextFile(context.Background(), acp.ReadTextFileRequest{Path: path})
	require.NoError(t, err)
	require.Equal(t, "hello", read.Content)

	_, err = c.WriteTextFile(context.Background(), acp.WriteTextFileRequest{Path: "relative"})
	require.Error(t, err)

	_, err = c.ReadTextFile(context.Background(), acp.ReadTextFileRequest{Path: "relative"})
	require.Error(t, err)

	_, err = c.ReadTextFile(context.Background(), acp.ReadTextFileRequest{Path: filepath.Join(t.TempDir(), "missing.txt")})
	require.Error(t, err)

	parentFile := filepath.Join(t.TempDir(), "parent")
	require.NoError(t, os.WriteFile(parentFile, []byte("file"), 0o600))
	_, err = c.WriteTextFile(context.Background(), acp.WriteTextFileRequest{
		Path: filepath.Join(parentFile, "child.txt"),
	})
	require.Error(t, err)
}

func TestClientPermissionMethods(t *testing.T) {
	t.Parallel()

	c := client{}
	resp, err := c.RequestPermission(context.Background(), acp.RequestPermissionRequest{
		Options: []acp.PermissionOption{
			{OptionId: "reject", Kind: acp.PermissionOptionKindRejectOnce},
			{OptionId: "allow", Kind: acp.PermissionOptionKindAllowOnce},
		},
	})
	require.NoError(t, err)
	require.Equal(t, acp.PermissionOptionId("reject"), resp.Outcome.Selected.OptionId)

	resp, err = c.RequestPermission(context.Background(), acp.RequestPermissionRequest{})
	require.NoError(t, err)
	require.NotNil(t, resp.Outcome.Cancelled)
}

func TestClientSessionUpdatePrintsVisibleEvents(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	c := client{output: &output}
	messageID := "message-1"
	status := acp.ToolCallStatusCompleted

	require.Same(t, &output, c.writer())
	c.fallback.writeText(&output, "")
	c.fallback.text = "same"
	_, wrote := c.fallback.writeText(&output, "same")
	require.False(t, wrote)
	c.fallback.text = "prefix"
	c.fallback.writeText(&output, "different")
	c.fallback.text = ""
	require.NoError(t, c.SessionUpdate(context.Background(), acp.SessionNotification{
		Update: acp.SessionUpdate{
			UserMessageChunk: &acp.SessionUpdateUserMessageChunk{Content: acp.TextBlock("user text")},
		},
	}))
	require.NoError(t, c.SessionUpdate(context.Background(), acp.SessionNotification{
		Update: acp.SessionUpdate{
			AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: acp.TextBlock("agent text")},
		},
	}))
	require.NoError(t, c.SessionUpdate(context.Background(), acp.SessionNotification{
		Update: acp.SessionUpdate{
			AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{
				MessageId: &messageID,
				Content:   acp.TextBlock("Hello"),
			},
		},
	}))
	require.NoError(t, c.SessionUpdate(context.Background(), acp.SessionNotification{
		Update: acp.SessionUpdate{
			AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{
				MessageId: &messageID,
				Content:   acp.TextBlock("Hello world"),
			},
		},
	}))
	require.NoError(t, c.SessionUpdate(context.Background(), acp.SessionNotification{
		Update: acp.SessionUpdate{
			AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{
				MessageId: &messageID,
				Content:   acp.TextBlock("Hello world"),
			},
		},
	}))
	require.NoError(t, c.SessionUpdate(context.Background(), acp.SessionNotification{
		Update: acp.SessionUpdate{
			AgentThoughtChunk: &acp.SessionUpdateAgentThoughtChunk{Content: acp.TextBlock("thinking")},
		},
	}))
	require.NoError(t, c.SessionUpdate(context.Background(), acp.SessionNotification{
		Update: acp.SessionUpdate{
			ToolCall: &acp.SessionUpdateToolCall{ToolCallId: "tool-1", Title: "Read file"},
		},
	}))
	require.NoError(t, c.SessionUpdate(context.Background(), acp.SessionNotification{
		Update: acp.SessionUpdate{
			ToolCallUpdate: &acp.SessionToolCallUpdate{ToolCallId: "tool-1", Status: &status},
		},
	}))
	require.NoError(t, c.SessionUpdate(context.Background(), acp.SessionNotification{
		Update: acp.SessionUpdate{
			ToolCallUpdate: &acp.SessionToolCallUpdate{ToolCallId: "tool-2"},
		},
	}))
	require.NoError(t, c.SessionUpdate(context.Background(), acp.SessionNotification{
		Update: acp.SessionUpdate{
			AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: acp.TextBlock("")},
		},
	}))
	require.NoError(t, c.SessionUpdate(context.Background(), acp.SessionNotification{
		Update: acp.SessionUpdate{
			UserMessageChunk: &acp.SessionUpdateUserMessageChunk{Content: acp.TextBlock("")},
		},
	}))
	require.NoError(t, c.SessionUpdate(context.Background(), acp.SessionNotification{
		Update: acp.SessionUpdate{
			AgentThoughtChunk: &acp.SessionUpdateAgentThoughtChunk{Content: acp.TextBlock("")},
		},
	}))
	require.NoError(t, c.SessionUpdate(context.Background(), acp.SessionNotification{}))

	got := output.String()
	require.Contains(t, got, "different")
	require.Contains(t, got, "[user] user text")
	require.Contains(t, got, "agent text")
	require.Equal(t, 1, strings.Count(got, "Hello world"))
	require.Contains(t, got, "[thought] thinking")
	require.Contains(t, got, "[tool] tool-1 Read file")
	require.Contains(t, got, "[tool] tool-1 completed")
	require.NotContains(t, got, "[tool] tool-2")
}

func TestClientWriterDefaultsToStdout(t *testing.T) {
	t.Parallel()

	require.Same(t, os.Stdout, (&client{}).writer())
}

func TestClientSessionUpdateSuppressesImmediateSnapshotDuplicate(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	c := client{output: &output}
	messageID := "message-2"

	require.NoError(t, c.SessionUpdate(context.Background(), acp.SessionNotification{
		Update: acp.SessionUpdate{
			AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: acp.TextBlock("once")},
		},
	}))
	require.NoError(t, c.SessionUpdate(context.Background(), acp.SessionNotification{
		Update: acp.SessionUpdate{
			AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{
				MessageId: &messageID,
				Content:   acp.TextBlock("once"),
			},
		},
	}))

	require.Equal(t, "once", output.String())
}

func TestClientSessionUpdateSuppressesDoubledSnapshot(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	c := client{output: &output}
	messageID := "message-3"

	require.NoError(t, c.SessionUpdate(context.Background(), acp.SessionNotification{
		Update: acp.SessionUpdate{
			AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{
				MessageId: &messageID,
				Content:   acp.TextBlock("RESUMED_SESSION_OK"),
			},
		},
	}))
	require.NoError(t, c.SessionUpdate(context.Background(), acp.SessionNotification{
		Update: acp.SessionUpdate{
			AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{
				MessageId: &messageID,
				Content:   acp.TextBlock("RESUMED_SESSION_OKRESUMED_SESSION_OK"),
			},
		},
	}))

	require.Equal(t, "RESUMED_SESSION_OK", output.String())
}

func TestClientSessionUpdateSuppressesSnapshotAfterChunkedDeltas(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	c := client{output: &output}
	messageID := "message-4"
	snapshotID := "message-5"

	require.NoError(t, c.SessionUpdate(context.Background(), acp.SessionNotification{
		Update: acp.SessionUpdate{
			AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{
				MessageId: &messageID,
				Content:   acp.TextBlock("RESUMED_SESSION_"),
			},
		},
	}))
	require.NoError(t, c.SessionUpdate(context.Background(), acp.SessionNotification{
		Update: acp.SessionUpdate{
			AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{
				MessageId: &messageID,
				Content:   acp.TextBlock("RESUMED_SESSION_OK"),
			},
		},
	}))
	require.NoError(t, c.SessionUpdate(context.Background(), acp.SessionNotification{
		Update: acp.SessionUpdate{
			UserMessageChunk: &acp.SessionUpdateUserMessageChunk{Content: acp.TextBlock("")},
		},
	}))
	require.NoError(t, c.SessionUpdate(context.Background(), acp.SessionNotification{
		Update: acp.SessionUpdate{
			AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{
				MessageId: &snapshotID,
				Content:   acp.TextBlock("RESUMED_SESSION_OK"),
			},
		},
	}))

	require.Equal(t, "RESUMED_SESSION_OK", output.String())
}

func TestClientSessionUpdateEmptyAgentChunkResetsFallbackBoundary(t *testing.T) {
	t.Parallel()

	var output bytes.Buffer
	c := client{output: &output}

	require.NoError(t, c.SessionUpdate(context.Background(), acp.SessionNotification{
		Update: acp.SessionUpdate{
			AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: acp.TextBlock("previous")},
		},
	}))
	require.NoError(t, c.SessionUpdate(context.Background(), acp.SessionNotification{
		Update: acp.SessionUpdate{
			AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: acp.TextBlock("")},
		},
	}))
	require.NoError(t, c.SessionUpdate(context.Background(), acp.SessionNotification{
		Update: acp.SessionUpdate{
			AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: acp.TextBlock("R")},
		},
	}))
	require.NoError(t, c.SessionUpdate(context.Background(), acp.SessionNotification{
		Update: acp.SessionUpdate{
			AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: acp.TextBlock("ESUMED_SESSION_OK")},
		},
	}))
	require.NoError(t, c.SessionUpdate(context.Background(), acp.SessionNotification{
		Update: acp.SessionUpdate{
			AgentMessageChunk: &acp.SessionUpdateAgentMessageChunk{Content: acp.TextBlock("RESUMED_SESSION_OK")},
		},
	}))

	require.Equal(t, "previousRESUMED_SESSION_OK", output.String())
}

func TestClientSuppressRepeatedAgentTextBranches(t *testing.T) {
	t.Parallel()

	display := &messageDisplay{text: "half"}

	require.False(t, (&client{}).suppressRepeatedAgentText(display, "half"))
	require.True(t, (&client{agentRunText: "same"}).
		suppressRepeatedAgentText(&messageDisplay{}, "same"))
	require.True(t, (&client{agentRunText: "same"}).
		suppressRepeatedAgentText(&messageDisplay{}, "samesame"))
	require.True(t, (&client{lastUpdateWasAgent: true, lastAgentText: "snapshot"}).
		suppressRepeatedAgentText(&messageDisplay{}, "snapshot"))
	require.True(t, (&client{lastUpdateWasAgent: true}).
		suppressRepeatedAgentText(display, "halfhalf"))
	require.True(t, (&client{lastUpdateWasAgent: true, lastAgentText: "tail"}).
		suppressRepeatedAgentText(&messageDisplay{text: "other"}, "tailtail"))
	require.False(t, (&client{lastUpdateWasAgent: true, lastAgentText: "previous"}).
		suppressRepeatedAgentText(&messageDisplay{text: "current"}, "different"))
}

func TestCollapseRepeatedExactText(t *testing.T) {
	t.Parallel()

	require.Empty(t, collapseRepeatedExactText(""))
	require.Equal(t, "abc", collapseRepeatedExactText("abc"))
	require.Equal(t, "abc", collapseRepeatedExactText("abcabc"))
	require.Equal(t, "abcd", collapseRepeatedExactText("abcd"))
}

func TestClientTerminalMethods(t *testing.T) {
	t.Parallel()

	c := client{}
	terminal, err := c.CreateTerminal(context.Background(), acp.CreateTerminalRequest{})
	require.NoError(t, err)
	require.Equal(t, "terminal-1", terminal.TerminalId)

	output, err := c.TerminalOutput(context.Background(), acp.TerminalOutputRequest{})
	require.NoError(t, err)
	require.False(t, output.Truncated)

	_, err = c.KillTerminal(context.Background(), acp.KillTerminalRequest{})
	require.NoError(t, err)

	_, err = c.ReleaseTerminal(context.Background(), acp.ReleaseTerminalRequest{})
	require.NoError(t, err)

	_, err = c.WaitForTerminalExit(context.Background(), acp.WaitForTerminalExitRequest{})
	require.NoError(t, err)
}

func TestParseConfig(t *testing.T) {
	originalGetwd := getwd
	t.Cleanup(func() {
		getwd = originalGetwd
	})

	getwd = func() (string, error) {
		return "/repo", nil
	}

	cfg, err := parseConfig(nil, io.Discard)
	require.NoError(t, err)
	require.Equal(t, defaultSessionFilePath(), cfg.sessionFile)
	require.Equal(t, "/repo", cfg.cwd)
	require.Equal(t, defaultPrompt, cfg.prompt)

	cfg, err = parseConfig([]string{"hello", "again"}, io.Discard)
	require.NoError(t, err)
	require.Equal(t, defaultSessionFilePath(), cfg.sessionFile)
	require.Equal(t, "/repo", cfg.cwd)
	require.Equal(t, "hello again", cfg.prompt)

	cfg, err = parseConfig([]string{
		"-session-file", "saved.jsonl",
		"-session-id", "11111111-1111-4111-8111-111111111111",
		"-cwd", "/work",
		"-prompt", "resume now",
	}, io.Discard)
	require.NoError(t, err)
	require.Equal(t, "saved.jsonl", cfg.sessionFile)
	require.Equal(t, "11111111-1111-4111-8111-111111111111", cfg.sessionID)
	require.Equal(t, "/work", cfg.cwd)
	require.Equal(t, "resume now", cfg.prompt)
}

func TestDefaultSessionFilePath(t *testing.T) {
	path := defaultSessionFilePath()
	require.True(t, filepath.IsAbs(path))
	require.Equal(t, defaultSessionFile, filepath.Base(path))
	require.Equal(t, "resume-from-file", filepath.Base(filepath.Dir(path)))

	originalRuntimeCaller := runtimeCaller
	t.Cleanup(func() {
		runtimeCaller = originalRuntimeCaller
	})

	runtimeCaller = func(int) (uintptr, string, int, bool) {
		return 0, "", 0, false
	}
	require.Equal(t, defaultSessionFile, defaultSessionFilePath())
}

func TestParseConfigErrors(t *testing.T) {
	originalGetwd := getwd
	t.Cleanup(func() {
		getwd = originalGetwd
	})

	_, err := parseConfig([]string{"-unknown"}, io.Discard)
	require.Error(t, err)

	getwd = func() (string, error) {
		return "", errors.New("cwd")
	}
	_, err = parseConfig(nil, io.Discard)
	require.Error(t, err)

	getwd = func() (string, error) {
		return "relative", nil
	}
	_, err = parseConfig(nil, io.Discard)
	require.Error(t, err)
}

func TestLoadSessionFile(t *testing.T) {
	t.Parallel()

	path := filepath.Join(t.TempDir(), "session.jsonl")
	require.NoError(t, os.WriteFile(path, []byte(" \n{\"session_id\":\"s1\"}\n{\"type\":\"assistant\"}\n"), 0o600))

	entries, err := loadSessionFile(path)
	require.NoError(t, err)
	require.Len(t, entries, 2)
	require.JSONEq(t, `{"session_id":"s1"}`, string(entries[0]))
	require.JSONEq(t, `{"type":"assistant"}`, string(entries[1]))
}

func TestLoadSessionFileErrors(t *testing.T) {
	t.Parallel()

	_, err := loadSessionFile(filepath.Join(t.TempDir(), "missing.jsonl"))
	require.Error(t, err)
	require.Contains(t, err.Error(), "copy a real Claude JSONL transcript")

	path := filepath.Join(t.TempDir(), "empty.jsonl")
	require.NoError(t, os.WriteFile(path, []byte("\n\n"), 0o600))
	_, err = loadSessionFile(path)
	require.Error(t, err)

	path = filepath.Join(t.TempDir(), "bad.jsonl")
	require.NoError(t, os.WriteFile(path, []byte("{bad}\n"), 0o600))
	_, err = loadSessionFile(path)
	require.Error(t, err)
	require.Contains(t, err.Error(), "bad.jsonl:1")
}

func TestValidateJSONLine(t *testing.T) {
	t.Parallel()

	entry, err := validateJSONLine([]byte(`{"ok":true}`))
	require.NoError(t, err)
	require.JSONEq(t, `{"ok":true}`, string(entry))

	_, err = validateJSONLine([]byte(`null`))
	require.Error(t, err)

	_, err = validateJSONLine([]byte(`{"ok":true} {"extra":true}`))
	require.Error(t, err)

	_, err = validateJSONLine([]byte(`{bad}`))
	require.Error(t, err)
}

func TestSessionIDFromEntries(t *testing.T) {
	t.Parallel()

	require.Equal(t, "s1", sessionIDFromEntries([]json.RawMessage{
		json.RawMessage(`{"type":"user","session_id":" s1 "}`),
	}))
	require.Equal(t, "s2", sessionIDFromEntries([]json.RawMessage{
		json.RawMessage(`{"sessionId":"s2"}`),
	}))
	require.Empty(t, sessionIDFromEntries([]json.RawMessage{
		json.RawMessage(`{bad}`),
		json.RawMessage(`{"session_id":""}`),
		json.RawMessage(`{"sessionId":42}`),
	}))
}

func TestRunImportedResume(t *testing.T) {
	t.Parallel()

	conn := &fakeAgentConnection{}
	var output bytes.Buffer
	entries := []json.RawMessage{json.RawMessage(`{"session_id":"s1"}`)}

	err := runImportedResume(context.Background(), conn, config{
		sessionID: "s1",
		cwd:       "/repo",
		prompt:    "continue",
	}, entries, strings.NewReader("typed message\n"), &output)
	require.NoError(t, err)
	require.Equal(t, claudeSessionImportMethod, conn.importMethod)
	require.Equal(t, sessionImportParams{
		SessionID: "s1",
		Cwd:       "/repo",
		Format:    claudeSessionImportFormat,
		Entries:   entries,
	}, conn.importParams)
	require.Equal(t, acp.SessionId("s1"), conn.sessionID)
	require.Equal(t, "/repo", conn.loadCwd)
	require.Equal(t, []string{"continue", "typed message"}, conn.prompts)
	require.True(t, conn.closed)
	require.Contains(t, output.String(), "== previous session ==")
	require.Contains(t, output.String(), "== resume smoke test ==")
	require.Contains(t, output.String(), "resumed session: s1")
	require.Contains(t, output.String(), "enter one prompt")
	require.Contains(t, output.String(), "== typed prompt ==")
	require.Contains(t, output.String(), "stop reason: end_turn")
}

func TestRunImportedResumeErrors(t *testing.T) {
	t.Parallel()

	for _, conn := range []*fakeAgentConnection{
		{initErr: errors.New("init")},
		{importErr: errors.New("import")},
		{loadErr: errors.New("load")},
		{promptErr: errors.New("prompt")},
	} {
		err := runImportedResume(context.Background(), conn, config{
			sessionID: "s1",
			cwd:       "/repo",
			prompt:    "continue",
		}, []json.RawMessage{json.RawMessage(`{"session_id":"s1"}`)}, strings.NewReader("typed\n"), io.Discard)
		require.Error(t, err)
	}
}

func TestRunImportedResumeTypedPromptBranches(t *testing.T) {
	t.Parallel()

	for _, stdin := range []io.Reader{
		strings.NewReader("\n"),
		strings.NewReader(""),
	} {
		conn := &fakeAgentConnection{}
		var output bytes.Buffer

		err := runImportedResume(context.Background(), conn, config{
			sessionID: "s1",
			cwd:       "/repo",
			prompt:    "continue",
		}, []json.RawMessage{json.RawMessage(`{"session_id":"s1"}`)}, stdin, &output)
		require.NoError(t, err)
		require.Equal(t, []string{"continue"}, conn.prompts)
		require.Contains(t, output.String(), "no typed prompt entered")
	}

	reader := errReader{err: errors.New("read")}
	err := runImportedResume(context.Background(), &fakeAgentConnection{}, config{
		sessionID: "s1",
		cwd:       "/repo",
		prompt:    "continue",
	}, []json.RawMessage{json.RawMessage(`{"session_id":"s1"}`)}, reader, io.Discard)
	require.Error(t, err)

	err = runImportedResume(context.Background(), &fakeAgentConnection{
		promptErr:   errors.New("typed prompt"),
		promptErrAt: 2,
	}, config{
		sessionID: "s1",
		cwd:       "/repo",
		prompt:    "continue",
	}, []json.RawMessage{json.RawMessage(`{"session_id":"s1"}`)}, strings.NewReader("typed\n"), io.Discard)
	require.Error(t, err)

	ctx, cancel := context.WithCancel(context.Background())
	var output bytes.Buffer
	err = runImportedResume(ctx, &fakeAgentConnection{afterPrompt: cancel}, config{
		sessionID: "s1",
		cwd:       "/repo",
		prompt:    "continue",
	}, []json.RawMessage{json.RawMessage(`{"session_id":"s1"}`)}, strings.NewReader("typed\n"), &output)
	require.NoError(t, err)
	require.Contains(t, output.String(), "interrupted; closing session")
}

func TestReadTypedPromptInterrupted(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	var output bytes.Buffer
	prompt, err := readTypedPrompt(ctx, strings.NewReader("ignored\n"), &output)
	require.ErrorIs(t, err, errPromptInterrupted)
	require.Empty(t, prompt)
	require.Contains(t, output.String(), "Ctrl-C")
}

func TestReadTypedPromptInterruptedWhileWaiting(t *testing.T) {
	t.Parallel()

	reader, writer := io.Pipe()
	defer func() {
		_ = reader.Close()
		_ = writer.Close()
	}()

	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)

	go func() {
		_, err := readTypedPrompt(ctx, reader, io.Discard)
		done <- err
	}()

	time.Sleep(20 * time.Millisecond)
	cancel()

	require.ErrorIs(t, <-done, errPromptInterrupted)
}

func TestRun(t *testing.T) {
	originalStartAgent := startAgent
	originalGetwd := getwd
	t.Cleanup(func() {
		startAgent = originalStartAgent
		getwd = originalGetwd
	})

	path := filepath.Join(t.TempDir(), "session.jsonl")
	require.NoError(t, os.WriteFile(path, []byte(`{"session_id":"s1"}`+"\n"), 0o600))

	conn := &fakeAgentConnection{}
	var closed bool
	var waited bool
	startAgent = func(context.Context, io.Writer, io.Writer) (*startedAgent, error) {
		return &startedAgent{
			conn: conn,
			close: func() {
				closed = true
			},
			wait: func() error {
				waited = true

				return nil
			},
		}, nil
	}
	getwd = func() (string, error) {
		return "/repo", nil
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	code := run(
		context.Background(),
		[]string{"-session-file", path, "positional", "prompt"},
		strings.NewReader("typed\n"),
		&stdout,
		&stderr,
	)
	require.Equal(t, 0, code)
	require.Equal(t, []string{"positional prompt", "typed"}, conn.prompts)
	require.True(t, closed)
	require.True(t, waited)
	require.Empty(t, stderr.String())
}

func TestRunErrors(t *testing.T) {
	originalStartAgent := startAgent
	originalGetwd := getwd
	t.Cleanup(func() {
		startAgent = originalStartAgent
		getwd = originalGetwd
	})

	path := filepath.Join(t.TempDir(), "session.jsonl")
	require.NoError(t, os.WriteFile(path, []byte(`{"sessionId":"s1"}`+"\n"), 0o600))

	getwd = func() (string, error) {
		return "relative", nil
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	require.Equal(t, 1, run(context.Background(), []string{"-session-file", path}, strings.NewReader(""), &stdout, &stderr))
	require.Contains(t, stderr.String(), "cwd must be absolute")

	getwd = func() (string, error) {
		return "/repo", nil
	}

	stderr.Reset()
	require.Equal(t, 1, run(
		context.Background(),
		[]string{"-session-file", filepath.Join(t.TempDir(), "missing")},
		strings.NewReader(""),
		&stdout,
		&stderr,
	))
	require.Contains(t, stderr.String(), "missing")
	require.Contains(t, stderr.String(), "copy a real Claude JSONL transcript")

	missingIDPath := filepath.Join(t.TempDir(), "missing-id.jsonl")
	require.NoError(t, os.WriteFile(missingIDPath, []byte(`{"type":"user"}`+"\n"), 0o600))
	stderr.Reset()
	require.Equal(t, 1, run(
		context.Background(),
		[]string{"-session-file", missingIDPath},
		strings.NewReader(""),
		&stdout,
		&stderr,
	))
	require.Contains(t, stderr.String(), "session id not found")

	startAgent = func(context.Context, io.Writer, io.Writer) (*startedAgent, error) {
		return nil, errors.New("start")
	}
	stderr.Reset()
	require.Equal(t, 1, run(context.Background(), []string{"-session-file", path}, strings.NewReader(""), &stdout, &stderr))
	require.Contains(t, stderr.String(), "start")

	startAgent = func(context.Context, io.Writer, io.Writer) (*startedAgent, error) {
		return &startedAgent{conn: &fakeAgentConnection{initErr: errors.New("init")}}, nil
	}
	stderr.Reset()
	require.Equal(t, 1, run(context.Background(), []string{"-session-file", path}, strings.NewReader(""), &stdout, &stderr))
	require.Contains(t, stderr.String(), "init")

	cancelCtx, cancel := context.WithCancel(context.Background())
	cancel()
	startAgent = func(context.Context, io.Writer, io.Writer) (*startedAgent, error) {
		return &startedAgent{conn: &fakeAgentConnection{promptErr: errors.New("interrupt signal received")}}, nil
	}
	stderr.Reset()
	require.Equal(t, 130, run(cancelCtx, []string{"-session-file", path}, strings.NewReader(""), &stdout, &stderr))
	require.Contains(t, stderr.String(), "context canceled")
}

func TestMain(t *testing.T) {
	originalStartAgent := startAgent
	originalGetwd := getwd
	originalExit := exit
	originalArgs := os.Args
	originalStdin := os.Stdin
	t.Cleanup(func() {
		startAgent = originalStartAgent
		getwd = originalGetwd
		exit = originalExit
		os.Args = originalArgs
		os.Stdin = originalStdin
	})

	path := filepath.Join(t.TempDir(), "session.jsonl")
	require.NoError(t, os.WriteFile(path, []byte(`{"session_id":"s1"}`+"\n"), 0o600))

	startAgent = func(context.Context, io.Writer, io.Writer) (*startedAgent, error) {
		return &startedAgent{
			conn:  &fakeAgentConnection{},
			close: func() {},
			wait:  func() error { return nil },
		}, nil
	}
	getwd = func() (string, error) { return "/repo", nil }

	var gotCode int
	exit = func(code int) {
		gotCode = code
	}
	os.Args = []string{"resume-from-file", "-session-file", path}
	stdin, err := os.CreateTemp(t.TempDir(), "stdin")
	require.NoError(t, err)
	require.NoError(t, stdin.Close())
	stdin, err = os.Open(stdin.Name())
	require.NoError(t, err)
	defer func() {
		require.NoError(t, stdin.Close())
	}()
	os.Stdin = stdin

	main()

	require.Equal(t, 0, gotCode)
}

func TestStartAgentProcess(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses a POSIX shell script")
	}

	binDir := t.TempDir()
	goPath := filepath.Join(binDir, "go")
	err := os.WriteFile(goPath, []byte("#!/bin/sh\nwhile IFS= read -r _; do :; done\n"), 0o755)
	require.NoError(t, err)

	t.Setenv("PATH", binDir)

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	agent, err := startAgentProcess(context.Background(), &stdout, &stderr)
	require.NoError(t, err)
	require.NotNil(t, agent.conn)

	agent.close()
	require.NoError(t, agent.wait())
}

func TestStartAgentProcessUsesModuleEntrypoint(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses POSIX shell")
	}

	originalCommandContext := commandContext
	t.Cleanup(func() {
		commandContext = originalCommandContext
	})

	var gotName string
	var gotArgs []string
	commandContext = func(ctx context.Context, name string, args ...string) *exec.Cmd {
		gotName = name
		gotArgs = append([]string(nil), args...)

		return exec.CommandContext(ctx, "sh", "-c", "while IFS= read -r _; do :; done")
	}

	agent, err := startAgentProcess(context.Background(), io.Discard, io.Discard)
	require.NoError(t, err)
	require.NotNil(t, agent.conn)

	agent.close()
	require.NoError(t, agent.wait())
	require.Equal(t, "go", gotName)
	require.Equal(t, []string{"run", agentPackage}, gotArgs)
}

func TestStartAgentProcessStartError(t *testing.T) {
	t.Setenv("PATH", t.TempDir())

	agent, err := startAgentProcess(context.Background(), io.Discard, io.Discard)
	require.Error(t, err)
	require.Nil(t, agent)
}

func TestStartAgentProcessPipeErrors(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses POSIX shell")
	}

	originalCommandContext := commandContext
	t.Cleanup(func() {
		commandContext = originalCommandContext
	})

	commandContext = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, "sh", "-c", "cat")
		cmd.Stdin = strings.NewReader("")

		return cmd
	}
	agent, err := startAgentProcess(context.Background(), io.Discard, io.Discard)
	require.Error(t, err)
	require.Nil(t, agent)

	commandContext = func(ctx context.Context, _ string, _ ...string) *exec.Cmd {
		cmd := exec.CommandContext(ctx, "sh", "-c", "cat")
		cmd.Stdout = io.Discard

		return cmd
	}
	agent, err = startAgentProcess(context.Background(), io.Discard, io.Discard)
	require.Error(t, err)
	require.Nil(t, agent)
}

func TestConnectionInputGate(t *testing.T) {
	t.Parallel()

	gate := newConnectionInputGate(strings.NewReader("ok"))
	readDone := make(chan string, 1)

	go func() {
		buf := make([]byte, 2)
		n, err := gate.Read(buf)
		require.NoError(t, err)
		readDone <- string(buf[:n])
	}()

	select {
	case <-readDone:
		t.Fatal("read completed before gate opened")
	case <-time.After(20 * time.Millisecond):
	}

	gate.open()
	gate.open()
	require.Equal(t, "ok", <-readDone)
}
