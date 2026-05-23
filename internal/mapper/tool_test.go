package mapper

import (
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/stretchr/testify/require"
)

func TestToolMetadataMapping(t *testing.T) {
	t.Parallel()

	require.Equal(t, acp.ToolKindThink, ToolCallInfo("Agent", "", nil, ToolUpdateOptions{}).Kind)
	require.Equal(t, acp.ToolKindRead, ToolCallInfo("Read", "", nil, ToolUpdateOptions{}).Kind)
	require.Equal(t, acp.ToolKindEdit, ToolCallInfo("MultiEdit", "", nil, ToolUpdateOptions{}).Kind)
	require.Equal(t, acp.ToolKindExecute, ToolCallInfo("Bash", "", nil, ToolUpdateOptions{}).Kind)
	require.Equal(t, acp.ToolKindSearch, ToolCallInfo("Grep", "", nil, ToolUpdateOptions{}).Kind)
	require.Equal(t, acp.ToolKindFetch, ToolCallInfo("WebFetch", "", nil, ToolUpdateOptions{}).Kind)
	require.Equal(t, acp.ToolKindThink, ToolCallInfo("TodoWrite", "", nil, ToolUpdateOptions{}).Kind)
	require.Equal(t, acp.ToolKindSwitchMode, ToolCallInfo("ExitPlanMode", "", nil, ToolUpdateOptions{}).Kind)
	require.Equal(t, acp.ToolKindOther, ToolCallInfo("Unknown", "", nil, ToolUpdateOptions{}).Kind)

	require.Equal(t, "Read /tmp/a", ToolTitle("Read", map[string]any{"file_path": "/tmp/a"}))
	require.Equal(t, "LS /tmp", ToolTitle("LS", map[string]any{"path": "/tmp"}))
	require.Equal(t, acp.ToolKindSearch, ToolCallInfo("LS", "", map[string]any{"path": "/tmp"}, ToolUpdateOptions{}).Kind)
	require.Equal(t, "make test", ToolTitle("Bash", map[string]any{"command": "make test"}))
	require.Equal(t, "Read File", ToolTitle("Read", nil))
	require.Equal(t, "Read File (1 - 1)", ToolTitle("Read", map[string]any{"limit": 1}))
	require.Equal(t, "Claude tool call", ToolTitle(" ", nil))
	require.Equal(t, []acp.ToolCallLocation{{Path: "/tmp/a"}}, locations(map[string]any{"path": "/tmp/a"}))
	require.Equal(t, []acp.ToolCallLocation{{Path: "/tmp/a"}, {Path: "/tmp/b"}}, locations(map[string]any{
		"file_path": "/tmp/a",
		"path":      "/tmp/b",
	}))
	require.Nil(t, locations(nil))
	require.Nil(t, locations(map[string]any{"command": "make test"}))
}

func TestToolCallInfoVariants(t *testing.T) {
	t.Parallel()

	info := ToolCallInfo("Agent", "tool-1", map[string]any{
		"description": "Explore code",
		"prompt":      "Find tests",
	}, ToolUpdateOptions{})
	require.Equal(t, "Explore code", info.Title)
	require.Equal(t, acp.ToolKindThink, info.Kind)
	require.Equal(t, "Find tests", info.Content[0].Content.Content.Text.Text)
	require.Equal(t, "Task", ToolCallInfo("Task", "", nil, ToolUpdateOptions{}).Title)

	info = ToolCallInfo("Bash", "bash-1", nil, ToolUpdateOptions{SupportsTerminalOutput: true})
	require.Equal(t, "Terminal", info.Title)
	require.Equal(t, "bash-1", info.Content[0].Terminal.TerminalId)
	info = ToolCallInfo("Bash", "", map[string]any{"description": "Run tests"}, ToolUpdateOptions{})
	require.Equal(t, "Run tests", info.Content[0].Content.Content.Text.Text)

	info = ToolCallInfo("Read", "", map[string]any{
		"file_path": "/repo/a.go",
		"offset":    3,
		"limit":     4,
	}, ToolUpdateOptions{Cwd: "/repo"})
	require.Equal(t, "Read a.go (3 - 6)", info.Title)
	require.Equal(t, 3, *info.Locations[0].Line)
	require.Equal(t, "Read /repo/a.go (from line 8)", ToolTitle("Read", map[string]any{
		"file_path": "/repo/a.go",
		"offset":    8,
	}))

	info = ToolCallInfo("Write", "", map[string]any{
		"file_path": "/repo/new.go",
		"content":   "package main\n",
	}, ToolUpdateOptions{Cwd: "/repo"})
	require.Equal(t, "Write new.go", info.Title)
	require.Equal(t, "package main\n", info.Content[0].Diff.NewText)
	require.Equal(t, "draft", ToolCallInfo("Write", "", map[string]any{"content": "draft"}, ToolUpdateOptions{}).Content[0].Content.Content.Text.Text)

	info = ToolCallInfo("Edit", "", map[string]any{
		"file_path":  "/repo/a.go",
		"old_string": "old",
		"new_string": "new",
	}, ToolUpdateOptions{Cwd: "/repo"})
	require.Equal(t, "Edit a.go", info.Title)
	require.Equal(t, "old", *info.Content[0].Diff.OldText)
	info = ToolCallInfo("Edit", "", map[string]any{"file_path": "/repo/a.go", "new_string": "new"}, ToolUpdateOptions{})
	require.Nil(t, info.Content[0].Diff.OldText)

	info = ToolCallInfo("MultiEdit", "", map[string]any{
		"file_path": "/repo/a.go",
		"edits": []any{
			map[string]any{"old_string": "first", "new_string": "second"},
			map[string]any{"new_string": "inserted"},
			map[string]any{"old_string": "", "new_string": ""},
		},
	}, ToolUpdateOptions{Cwd: "/repo"})
	require.Equal(t, "Edit a.go", info.Title)
	require.Len(t, info.Content, 2)
	require.Equal(t, "first", *info.Content[0].Diff.OldText)
	require.Equal(t, "second", info.Content[0].Diff.NewText)
	require.Nil(t, info.Content[1].Diff.OldText)
	require.Equal(t, "inserted", info.Content[1].Diff.NewText)

	info = ToolCallInfo("NotebookEdit", "", map[string]any{
		"notebook_path": "/repo/notes.ipynb",
		"cell_id":       "cell-1",
		"new_source":    "print('hi')",
	}, ToolUpdateOptions{Cwd: "/repo"})
	require.Equal(t, "Edit notes.ipynb cell cell-1", info.Title)
	require.Equal(t, "/repo/notes.ipynb", info.Locations[0].Path)
	require.Equal(t, "print('hi')", info.Content[0].Diff.NewText)

	info = ToolCallInfo("Glob", "", map[string]any{"path": "/repo", "pattern": "*.go"}, ToolUpdateOptions{})
	require.Equal(t, "Find `/repo` `*.go`", info.Title)
	require.Equal(t, "/repo", info.Locations[0].Path)

	info = ToolCallInfo("Grep", "", map[string]any{
		"-i":          true,
		"-n":          true,
		"-A":          1,
		"-B":          2,
		"-C":          3,
		"output_mode": "files_with_matches",
		"head_limit":  5,
		"glob":        "*.go",
		"type":        "go",
		"multiline":   true,
		"pattern":     "func",
		"path":        "/repo",
	}, ToolUpdateOptions{})
	require.Equal(t, `grep -i -n -A 1 -B 2 -C 3 -l | head -5 --include="*.go" --type=go -P "func" /repo`, info.Title)
	require.Equal(t, "grep -c", ToolCallInfo("Grep", "", map[string]any{"output_mode": "count"}, ToolUpdateOptions{}).Title)

	info = ToolCallInfo("WebFetch", "", map[string]any{"url": "https://example.com", "prompt": "Summarize"}, ToolUpdateOptions{})
	require.Equal(t, "Fetch https://example.com", info.Title)
	require.Equal(t, "Summarize", info.Content[0].Content.Content.Text.Text)
	require.Equal(t, "Fetch", ToolCallInfo("WebFetch", "", nil, ToolUpdateOptions{}).Title)

	info = ToolCallInfo("WebSearch", "", map[string]any{
		"query":           "acp\n*code*",
		"allowed_domains": []any{"example.com"},
		"blocked_domains": []string{"bad.example"},
	}, ToolUpdateOptions{})
	require.Equal(t, `search: "acp\n*code*" (allowed: example.com) (blocked: bad.example)`, info.Title)
	require.Equal(t, "Web search", ToolCallInfo("WebSearch", "", nil, ToolUpdateOptions{}).Title)

	require.Equal(t, "Update TODOs", ToolCallInfo("TodoWrite", "", nil, ToolUpdateOptions{}).Title)
	require.Equal(t, "Update TODOs", ToolCallInfo("TodoWrite", "", map[string]any{"todos": []any{map[string]any{}}}, ToolUpdateOptions{}).Title)
	require.Equal(t, "Update TODOs: one", ToolCallInfo("TodoWrite", "", map[string]any{
		"todos": []any{map[string]any{"content": "one"}},
	}, ToolUpdateOptions{}).Title)

	info = ToolCallInfo("ExitPlanMode", "", map[string]any{"plan": "Ship it"}, ToolUpdateOptions{})
	require.Equal(t, "Ready to code?", info.Title)
	require.Equal(t, acp.ToolKindSwitchMode, info.Kind)
	require.Equal(t, "Ship it", info.Content[0].Content.Content.Text.Text)

	info = ToolCallInfo("Other", "", map[string]any{"x": 1}, ToolUpdateOptions{})
	require.Equal(t, "Other", info.Title)
	require.Contains(t, info.Content[0].Content.Content.Text.Text, `"x": 1`)
	require.Equal(t, "Other", ToolCallInfo("Other", "", map[string]any{"bad": func() {}}, ToolUpdateOptions{}).Title)
	require.Equal(t, "Unknown Tool", otherToolInfo("", nil).Title)
	info = ToolCallInfo("Run", "", map[string]any{"command": "make test"}, ToolUpdateOptions{})
	require.Equal(t, "Run make test", info.Title)
	require.Contains(t, info.Content[0].Content.Content.Text.Text, `"command": "make test"`)
	require.Equal(t, "FutureTool /tmp/a", ToolTitle("FutureTool", map[string]any{"path": "/tmp/a"}))
	require.Equal(t, "Claude tool call", ToolCallInfo("", "", nil, ToolUpdateOptions{}).Title)
}

func TestPlanEntries(t *testing.T) {
	t.Parallel()

	require.Nil(t, PlanEntries(nil))
	entries := PlanEntries(map[string]any{"todos": []any{
		"bad",
		map[string]any{"content": ""},
		map[string]any{"content": "one", "status": "in_progress"},
		map[string]any{"content": "two", "status": "completed"},
		map[string]any{"content": "three", "status": "unknown"},
	}})
	require.Equal(t, []acp.PlanEntry{
		{Content: "one", Priority: acp.PlanEntryPriorityMedium, Status: acp.PlanEntryStatusInProgress},
		{Content: "two", Priority: acp.PlanEntryPriorityMedium, Status: acp.PlanEntryStatusCompleted},
		{Content: "three", Priority: acp.PlanEntryPriorityMedium, Status: acp.PlanEntryStatusPending},
	}, entries)
}

func TestMessageToUpdates(t *testing.T) {
	t.Parallel()

	updates := MessageToUpdatesWithOptions(&claude.AssistantMessage{
		Content: []claude.ContentBlock{
			claude.TextBlock{Text: "hello"},
			claude.ThinkingBlock{Thinking: "thinking"},
			claude.ToolUseBlock{ID: "structured-1", Name: "StructuredOutput", Input: map[string]any{"ok": true}},
			claude.ToolUseBlock{ID: "tool-1", Name: "Read", Input: map[string]any{"file_path": "/tmp/a"}},
			claude.ToolResultBlock{
				ToolUseID: "tool-1",
				IsError:   true,
				Content:   []claude.ContentBlock{claude.TextBlock{Text: "failed"}},
				Raw:       map[string]any{"x": "y"},
			},
		},
	}, ToolUpdateOptions{})

	require.Len(t, updates, 4)
	require.Equal(t, "hello", updates[0].AgentMessageChunk.Content.Text.Text)
	require.Equal(t, "thinking", updates[1].AgentThoughtChunk.Content.Text.Text)
	require.Equal(t, acp.ToolCallId("tool-1"), updates[2].ToolCall.ToolCallId)
	require.Equal(t, acp.ToolKindRead, updates[2].ToolCall.Kind)
	require.Equal(t, acp.ToolCallStatusFailed, *updates[3].ToolCallUpdate.Status)
	require.Equal(t, "```\nfailed\n```", updates[3].ToolCallUpdate.Content[0].Content.Content.Text.Text)

	updates = MessageToUpdatesWithOptions(&claude.AssistantMessage{
		Content: []claude.ContentBlock{
			claude.TextBlock{Text: "<local-command-stdout>ok</local-command-stdout>"},
			claude.ThinkingBlock{},
			claude.ToolResultBlock{ToolUseID: "tool-2", Content: []claude.ContentBlock{claude.UnknownBlock{Type: "future"}}},
			claude.UnknownBlock{Type: "future"},
		},
	}, ToolUpdateOptions{})
	require.Len(t, updates, 1)
	require.Equal(t, acp.ToolCallStatusCompleted, *updates[0].ToolCallUpdate.Status)
	require.Empty(t, updates[0].ToolCallUpdate.Content)
	require.Nil(t, MessageToUpdatesWithOptions(&claude.UserMessage{}, ToolUpdateOptions{}))
	require.Nil(t, MessageToUpdatesWithOptions(&claude.UserMessage{Content: "hello"}, ToolUpdateOptions{}))
	require.Nil(t, MessageToUpdatesWithOptions(&claude.UserMessage{Content: []any{
		map[string]any{"type": "text", "text": "hello"},
		map[string]any{"type": "tool_result"},
		"bad",
	}}, ToolUpdateOptions{}))
	require.Nil(t, MessageToUpdatesWithOptions(&claude.ResultMessage{}, ToolUpdateOptions{}))
	require.Nil(t, MessageToUpdatesWithOptions(&claude.UserMessage{Content: []any{
		map[string]any{"type": "tool_result", "tool_use_id": "structured-1", "content": "ok"},
	}}, ToolUpdateOptions{ToolUses: map[string]claude.ToolUseBlock{
		"structured-1": {ID: "structured-1", Name: "StructuredOutput"},
	}}))

	updates = MessageToUpdatesWithOptions(&claude.AssistantMessage{
		Content: []claude.ContentBlock{
			claude.UnknownBlock{Type: "image", Raw: map[string]any{
				"source": map[string]any{"type": "base64", "data": "img", "media_type": "image/png"},
			}},
		},
	}, ToolUpdateOptions{})
	require.Len(t, updates, 1)
	require.Equal(t, "image/png", updates[0].AgentMessageChunk.Content.Image.MimeType)

	cache := make(map[string]claude.ToolUseBlock)
	updates = MessageToUpdatesWithOptions(&claude.StreamEventMessage{
		EventType: "content_block_delta",
		Event: map[string]any{
			"delta": map[string]any{"type": streamEventTextDelta, "text": "partial"},
		},
	}, ToolUpdateOptions{ToolUses: cache})
	require.Len(t, updates, 1)
	require.Equal(t, "partial", updates[0].AgentMessageChunk.Content.Text.Text)

	updates = MessageToUpdatesWithOptions(&claude.StreamEventMessage{
		ParentToolUseID: "parent-stream",
		EventType:       "content_block_delta",
		Event: map[string]any{
			"delta": map[string]any{"type": streamEventThinkingDelta, "thinking": "work"},
		},
	}, ToolUpdateOptions{ToolUses: cache})
	require.Len(t, updates, 1)
	require.Equal(t, "work", updates[0].AgentThoughtChunk.Content.Text.Text)
	require.Equal(t, "parent-stream", updateParentToolUseID(updates[0]))

	updates = MessageToUpdatesWithOptions(&claude.StreamEventMessage{
		EventType: "content_block_start",
		Event: map[string]any{
			"content_block": map[string]any{
				"type":  "tool_use",
				"id":    "stream-tool",
				"name":  "Read",
				"input": map[string]any{"file_path": "/tmp/a"},
			},
		},
	}, ToolUpdateOptions{ToolUses: cache})
	require.Len(t, updates, 1)
	require.Equal(t, acp.ToolCallId("stream-tool"), updates[0].ToolCall.ToolCallId)

	require.Nil(t, MessageToUpdatesWithOptions(&claude.StreamEventMessage{
		EventType: "content_block_start",
		Event:     map[string]any{},
	}, ToolUpdateOptions{}))
	require.Nil(t, MessageToUpdatesWithOptions(&claude.StreamEventMessage{
		EventType: "content_block_delta",
		Event:     map[string]any{},
	}, ToolUpdateOptions{}))
	require.Nil(t, MessageToUpdatesWithOptions(&claude.StreamEventMessage{
		EventType: "content_block_delta",
		Event: map[string]any{
			"delta": map[string]any{"type": streamEventTextDelta},
		},
	}, ToolUpdateOptions{}))
	require.Nil(t, MessageToUpdatesWithOptions(&claude.StreamEventMessage{
		EventType: "unknown",
	}, ToolUpdateOptions{}))

	updates = MessageToUpdatesWithOptions(&claude.AssistantMessage{
		Content: []claude.ContentBlock{
			claude.ToolUseBlock{ID: "stream-tool", Name: "Read", Input: map[string]any{"file_path": "/tmp/a"}},
		},
	}, ToolUpdateOptions{ToolUses: cache})
	require.Len(t, updates, 1)
	require.Nil(t, updates[0].ToolCall)
	require.Equal(t, acp.ToolCallId("stream-tool"), updates[0].ToolCallUpdate.ToolCallId)

	updates = MessageToUpdatesWithOptions(&claude.AssistantMessage{
		ParentToolUseID: "parent-1",
		Content: []claude.ContentBlock{
			claude.TextBlock{Text: "child"},
			claude.ThinkingBlock{Thinking: "thought"},
			claude.ToolUseBlock{ID: "todo-parent", Name: "TodoWrite", Input: map[string]any{
				"todos": []any{map[string]any{"content": "nested", "status": "in_progress"}},
			}},
			claude.ToolUseBlock{ID: "bash-parent", Name: "Bash", Input: map[string]any{"command": "echo ok"}},
			claude.ToolResultBlock{ToolUseID: "bash-parent", Raw: map[string]any{"content": "ok"}},
		},
	}, ToolUpdateOptions{
		SupportsTerminalOutput: true,
		ToolUses:               map[string]claude.ToolUseBlock{},
	})
	require.Len(t, updates, 6)
	for _, update := range updates {
		require.Equal(t, "parent-1", updateParentToolUseID(update))
	}
}

func TestMessageToUpdatesUserToolResults(t *testing.T) {
	t.Parallel()

	cache := make(map[string]claude.ToolUseBlock)
	updates := MessageToUpdatesWithOptions(&claude.AssistantMessage{
		Content: []claude.ContentBlock{
			claude.ToolUseBlock{
				ID:    "web-1",
				Name:  "WebSearch",
				Input: map[string]any{"query": "current time in Brisbane"},
			},
		},
	}, ToolUpdateOptions{ToolUses: cache})
	require.Len(t, updates, 1)
	require.Equal(t, acp.ToolCallId("web-1"), updates[0].ToolCall.ToolCallId)

	updates = MessageToUpdatesWithOptions(&claude.UserMessage{
		ParentToolUseID: "parent-user",
		Content: []any{
			map[string]any{"type": "text", "text": "ignored"},
			map[string]any{
				"type":        "tool_result",
				"tool_use_id": "web-1",
				"content":     "web output",
			},
		},
	}, ToolUpdateOptions{ToolUses: cache})
	require.Len(t, updates, 1)
	require.Equal(t, "parent-user", updateParentToolUseID(updates[0]))
	require.Equal(t, acp.ToolCallId("web-1"), updates[0].ToolCallUpdate.ToolCallId)
	require.Equal(t, acp.ToolCallStatusCompleted, *updates[0].ToolCallUpdate.Status)
	require.Equal(t, "web output", updates[0].ToolCallUpdate.Content[0].Content.Content.Text.Text)
	require.Equal(t, map[string]any{
		"type":        "tool_result",
		"tool_use_id": "web-1",
		"content":     "web output",
	}, updates[0].ToolCallUpdate.RawOutput)
	claudeMeta, ok := updates[0].ToolCallUpdate.Meta[keyClaude].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "WebSearch", claudeMeta[keyToolName])

	updates = MessageToUpdatesWithOptions(&claude.UserMessage{
		Raw: map[string]any{
			keyMessage: map[string]any{
				keyContent: []any{map[string]any{
					"type":        "tool_result",
					"tool_use_id": "missing-1",
					"content": []any{
						map[string]any{"type": "text", "text": "fallback"},
					},
					"is_error": true,
				}},
			},
		},
	}, ToolUpdateOptions{})
	require.Len(t, updates, 1)
	require.Equal(t, acp.ToolCallId("missing-1"), updates[0].ToolCallUpdate.ToolCallId)
	require.Equal(t, acp.ToolCallStatusFailed, *updates[0].ToolCallUpdate.Status)
	require.Equal(t, "```\nfallback\n```", updates[0].ToolCallUpdate.Content[0].Content.Content.Text.Text)
	require.Nil(t, userMessageContent(nil))
}

func TestMessageToUpdatesRichToolContent(t *testing.T) {
	t.Parallel()

	cache := make(map[string]claude.ToolUseBlock)
	updates := MessageToUpdatesWithOptions(&claude.AssistantMessage{
		Content: []claude.ContentBlock{
			claude.ToolUseBlock{ID: "todo-1", Name: "TodoWrite", Input: map[string]any{
				"todos": []any{map[string]any{"content": "write tests", "status": "pending"}},
			}},
			claude.ToolUseBlock{ID: "todo-empty", Name: "TodoWrite", Input: map[string]any{}},
			claude.ToolUseBlock{ID: "bash-1", Name: "Bash", Input: map[string]any{"command": "go test"}},
			claude.ToolResultBlock{
				ToolUseID: "bash-1",
				Raw: map[string]any{"content": map[string]any{
					"type":        "bash_code_execution_result",
					"stdout":      "ok",
					"stderr":      "warn",
					"return_code": float64(7),
				}},
			},
		},
	}, ToolUpdateOptions{SupportsTerminalOutput: true, ToolUses: cache})

	require.Len(t, updates, 4)
	require.Equal(t, "write tests", updates[0].Plan.Entries[0].Content)
	require.Equal(t, "bash-1", updates[1].ToolCall.Content[0].Terminal.TerminalId)
	require.Equal(t, map[string]any{"terminal_id": "bash-1"}, updates[1].ToolCall.Meta["terminal_info"])
	terminalOutput, ok := updates[2].ToolCallUpdate.Meta["terminal_output"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "ok\nwarn", terminalOutput["data"])
	terminalExit, ok := updates[3].ToolCallUpdate.Meta["terminal_exit"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, 7, terminalExit["exit_code"])
	require.Equal(t, nil, terminalExit["signal"])
}

func TestToolResultContentVariants(t *testing.T) {
	t.Parallel()

	cache := map[string]claude.ToolUseBlock{
		"read-1":  {ID: "read-1", Name: "Read"},
		"bash-1":  {ID: "bash-1", Name: "Bash"},
		"edit-1":  {ID: "edit-1", Name: "Edit"},
		"multi-1": {ID: "multi-1", Name: "MultiEdit"},
		"plan-1":  {ID: "plan-1", Name: "ExitPlanMode"},
	}
	updates := MessageToUpdatesWithOptions(&claude.AssistantMessage{
		Content: []claude.ContentBlock{
			claude.ToolResultBlock{ToolUseID: "read-1", Raw: map[string]any{"content": "```\nread\n"}},
			claude.ToolResultBlock{ToolUseID: "bash-1", Raw: map[string]any{"content": []any{
				map[string]any{"type": "text", "text": "stdout"},
				map[string]any{"type": "text", "text": "stderr"},
			}}},
			claude.ToolResultBlock{ToolUseID: "edit-1", Raw: map[string]any{"content": map[string]any{
				"filePath": "/repo/a.go",
				"structuredPatch": []any{
					map[string]any{"newStart": int64(4), "lines": []any{" context", "-old", "+new", ""}},
					map[string]any{"newStart": 8, "lines": []string{"+added"}},
					map[string]any{"lines": []any{}},
				},
			}}},
			claude.ToolResultBlock{ToolUseID: "multi-1", Raw: map[string]any{"content": map[string]any{
				"filePath":        "/repo/a.go",
				"structuredPatch": []any{map[string]any{"newStart": 2, "lines": []any{"-before", "+after"}}},
			}}},
			claude.ToolResultBlock{ToolUseID: "plan-1"},
			claude.ToolResultBlock{ToolUseID: "generic-1", Content: []claude.ContentBlock{
				claude.UnknownBlock{Type: "image", Raw: map[string]any{"data": "img", "media_type": "image/png"}},
				claude.UnknownBlock{Type: "future", Raw: map[string]any{"type": "future"}},
			}},
			claude.ToolResultBlock{ToolUseID: "err-1", IsError: true, Content: []claude.ContentBlock{
				claude.UnknownBlock{Type: "future", Raw: map[string]any{"type": "future"}},
			}},
		},
	}, ToolUpdateOptions{ToolUses: cache})

	require.Len(t, updates, 7)
	require.Contains(t, updates[0].ToolCallUpdate.Content[0].Content.Content.Text.Text, "````")
	require.Equal(t, "```console\nstdout\nstderr\n```", updates[1].ToolCallUpdate.Content[0].Content.Content.Text.Text)
	require.Len(t, updates[2].ToolCallUpdate.Content, 2)
	require.Equal(t, "after", updates[3].ToolCallUpdate.Content[0].Diff.NewText)
	require.Equal(t, "/repo/a.go", updates[2].ToolCallUpdate.Locations[0].Path)
	require.Equal(t, 4, *updates[2].ToolCallUpdate.Locations[0].Line)
	require.Equal(t, "Exited Plan Mode", *updates[4].ToolCallUpdate.Title)
	require.Equal(t, "image/png", updates[5].ToolCallUpdate.Content[0].Content.Content.Image.MimeType)
	require.Contains(t, updates[6].ToolCallUpdate.Content[0].Content.Content.Text.Text, "```")

	readContent := readToolResultContent(claude.ToolResultBlock{Raw: map[string]any{"content": []any{
		map[string]any{"type": "text", "text": "```\nread\n"},
		map[string]any{"type": "image", "source": map[string]any{
			"type": "base64", "data": "img", "media_type": "image/png",
		}},
	}}})
	require.Len(t, readContent, 2)
	require.Contains(t, readContent[0].Content.Content.Text.Text, "````")
	require.Equal(t, "image/png", readContent[1].Content.Content.Image.MimeType)

	content, ok := contentBlockToToolContent(claude.UnknownBlock{Type: "image", Raw: map[string]any{
		"source": map[string]any{"data": "img", "media_type": "image/jpeg"},
	}}, false)
	require.True(t, ok)
	require.Equal(t, "image/jpeg", content.Content.Content.Image.MimeType)

	_, ok = contentBlockToToolContent(claude.UnknownBlock{Type: "image"}, false)
	require.False(t, ok)
	_, ok = contentBlockToToolContent(claude.ThinkingBlock{}, false)
	require.False(t, ok)

	_, ok = assistantUnknownBlockUpdate(claude.UnknownBlock{Type: "image", Raw: map[string]any{
		"source": map[string]any{"type": "base64"},
	}})
	require.False(t, ok)
}

func TestSpecialToolResultContent(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		raw  map[string]any
		want string
	}{
		{
			name: "tool reference",
			raw:  map[string]any{"type": "tool_reference", "tool_name": "Read"},
			want: "Tool: Read",
		},
		{
			name: "tool search",
			raw: map[string]any{"type": "tool_search_tool_search_result", "tool_references": []any{
				map[string]any{"tool_name": "Read"},
				map[string]any{"tool_name": "Write"},
			}},
			want: "Tools found: Read, Write",
		},
		{
			name: "tool search empty",
			raw:  map[string]any{"type": "tool_search_tool_search_result"},
			want: "Tools found: none",
		},
		{
			name: "tool search error",
			raw:  map[string]any{"type": "tool_search_tool_result_error", "error_code": "bad", "error_message": "nope"},
			want: "Error: bad - nope",
		},
		{
			name: "web search",
			raw:  map[string]any{"type": "web_search_result", "title": "Result", "url": "https://example.com"},
			want: "Result (https://example.com)",
		},
		{
			name: "web search error",
			raw:  map[string]any{"type": "web_search_tool_result_error", "error_code": "rate_limit"},
			want: "Error: rate_limit",
		},
		{
			name: "web fetch",
			raw:  map[string]any{"type": "web_fetch_result", "url": "https://example.com"},
			want: "Fetched: https://example.com",
		},
		{
			name: "web fetch error",
			raw:  map[string]any{"type": "web_fetch_tool_result_error", "error_code": "not_found"},
			want: "Error: not_found",
		},
		{
			name: "code execution",
			raw:  map[string]any{"type": "code_execution_result", "stdout": "ok"},
			want: "Output: ok",
		},
		{
			name: "code execution error",
			raw:  map[string]any{"type": "code_execution_tool_result_error", "error_code": "failed"},
			want: "Error: failed",
		},
		{
			name: "bash code execution",
			raw:  map[string]any{"type": "bash_code_execution_result", "stderr": "warn"},
			want: "Output: warn",
		},
		{
			name: "bash code execution error",
			raw:  map[string]any{"type": "bash_code_execution_tool_result_error", "error_code": "failed"},
			want: "Error: failed",
		},
		{
			name: "text editor view",
			raw:  map[string]any{"type": "text_editor_code_execution_view_result", "content": "file"},
			want: "file",
		},
		{
			name: "text editor create",
			raw:  map[string]any{"type": "text_editor_code_execution_create_result"},
			want: "File created",
		},
		{
			name: "text editor update",
			raw:  map[string]any{"type": "text_editor_code_execution_create_result", "is_file_update": true},
			want: "File updated",
		},
		{
			name: "text editor replace",
			raw:  map[string]any{"type": "text_editor_code_execution_str_replace_result", "lines": []any{"one", "two"}},
			want: "one\ntwo",
		},
		{
			name: "text editor error",
			raw:  map[string]any{"type": "text_editor_code_execution_tool_result_error", "error_code": "bad", "error_message": "nope"},
			want: "Error: bad - nope",
		},
		{
			name: "url image",
			raw: map[string]any{"type": "image", "source": map[string]any{
				"type": "url", "url": "https://example.com/a.png",
			}},
			want: "[image: https://example.com/a.png]",
		},
		{
			name: "generic image",
			raw:  map[string]any{"type": "image"},
			want: "[image]",
		},
		{
			name: "file image",
			raw:  map[string]any{"type": "image", "source": map[string]any{"type": "file"}},
			want: "[image: file reference]",
		},
		{
			name: "web search url only",
			raw:  map[string]any{"type": "web_search_result", "url": "https://example.com"},
			want: "https://example.com",
		},
		{
			name: "web search title only",
			raw:  map[string]any{"type": "web_search_result", "title": "Result"},
			want: "Result",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			content, ok := rawMapToToolContent(tt.raw, false, false)
			require.True(t, ok)
			require.Equal(t, tt.want, content.Content.Content.Text.Text)
		})
	}

	content, ok := rawMapToToolContent(map[string]any{"type": "future", "x": "y"}, true, false)
	require.True(t, ok)
	require.Contains(t, content.Content.Content.Text.Text, "```")
	require.Contains(t, content.Content.Content.Text.Text, `"x":"y"`)

	_, ok = rawMapToToolContent(map[string]any{"type": "future", "bad": func() {}}, false, false)
	require.False(t, ok)
}

func TestToolMappingHelpers(t *testing.T) {
	t.Parallel()

	require.Equal(t, "/tmp/a", displayPathForCwd("/tmp/a", ""))
	require.Equal(t, "/tmp/a", displayPathForCwd("/tmp/a", "/repo"))
	require.Equal(t, "/repo", displayPathForCwd("/repo", "/repo"))
	require.Equal(t, "a/b.go", displayPathForCwd("/repo/a/b.go", "/repo"))
	require.Equal(t, acp.ToolCallLocation{Path: "/tmp/a"}, locationWithOptionalLine("/tmp/a", 0))
	require.Nil(t, toolMeta("", nil))
	require.Equal(t, "", stringInput(nil, "x"))
	require.Equal(t, 2, mustInt(intInput(map[string]any{"x": int64(2)}, "x")))
	require.Equal(t, 3, mustInt(intInput(map[string]any{"x": float64(3)}, "x")))
	_, ok := intInput(nil, "x")
	require.False(t, ok)
	_, ok = intInput(map[string]any{"x": "bad"}, "x")
	require.False(t, ok)
	_, ok = intInput(map[string]any{"x": float64(3.5)}, "x")
	require.False(t, ok)
	require.False(t, boolInput(nil, "x"))
	require.True(t, boolInput(map[string]any{"x": true}, "x"))
	require.Nil(t, stringSliceInput(nil, "x"))
	require.Nil(t, stringSliceInput(map[string]any{"x": 1}, "x"))
	require.Equal(t, []string{"a"}, stringSliceInput(map[string]any{"x": []string{"a"}}, "x"))
	require.Equal(t, []string{"a"}, stringSliceInput(map[string]any{"x": []any{"a", 1}}, "x"))
	_, ok = mapInput(nil, "x")
	require.False(t, ok)
	value, ok := mapInput(map[string]any{"x": map[string]any{"ok": true}}, "x")
	require.True(t, ok)
	require.Equal(t, map[string]any{"ok": true}, value)
	value, ok = mapInput(map[string]any{"x": nil}, "x")
	require.False(t, ok)
	require.Nil(t, value)
	_, ok = mapInput(map[string]any{}, "x")
	require.False(t, ok)
	require.NotNil(t, stringPtr(""))
	require.Equal(t, []string{"a", "b"}, nonEmptyStrings("", "a", "b"))
	require.Equal(t, "", firstNonEmptyString("", ""))
	require.Equal(t, "a", firstNonEmptyString("", "a"))
	require.Equal(t, "```\nx\n```", markdownEscape("x\n"))
	require.Equal(t, "```\nx\n```", codeBlock("x"))
	require.Equal(t, "", trailingNewline("x\n"))
	require.Equal(t, "\n", trailingNewline("x"))
	require.Equal(t, "```console\nx\n```", consoleBlock("x\n\n"))
	require.Equal(t, [2]string{"", ""}, pair(imageData(nil)))
	require.Equal(t, [2]string{"img", ""}, pair(imageData(map[string]any{"data": "img"})))
	require.Nil(t, deltaContentBlock(map[string]any{"type": streamEventThinkingDelta}))
	require.Nil(t, deltaContentBlock(map[string]any{"type": "future"}))
	require.NotNil(t, updateMeta(acp.UpdateUserMessageText("user")))
	require.NotNil(t, updateMeta(acp.SessionUpdate{ToolCall: &acp.SessionUpdateToolCall{}}))
	require.NotNil(t, updateMeta(acp.SessionUpdate{ToolCallUpdate: &acp.SessionToolCallUpdate{}}))
	require.Nil(t, updateMeta(acp.SessionUpdate{}))
	setParentToolUseID(nil, "parent")
	meta := map[string]any{keyClaude: map[string]any{}}
	setParentToolUseID(meta, "parent")
	claudeMeta, ok := meta[keyClaude].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "parent", claudeMeta["parentToolUseId"])

	require.Nil(t, toolResultUpdates(claude.ToolResultBlock{ToolUseID: "todo-1"}, ToolUpdateOptions{
		ToolUses: map[string]claude.ToolUseBlock{"todo-1": {ID: "todo-1", Name: "TodoWrite"}},
	}))
	require.Nil(t, readToolResultContent(claude.ToolResultBlock{}))
	require.Nil(t, bashToolResultContent(claude.ToolResultBlock{}))
	content, locations := diffToolResultContent(map[string]any{})
	require.Nil(t, content)
	require.Nil(t, locations)
	content, locations = DiffToolResultContent(map[string]any{})
	require.Nil(t, content)
	require.Nil(t, locations)
	content, locations = diffToolResultContent(map[string]any{
		"filePath":        "/tmp/a",
		"structuredPatch": []any{map[string]any{"lines": []string{""}}},
	})
	require.Empty(t, content)
	require.Empty(t, locations)

	oldText, newText := structuredPatchText([]string{"context", " same", "-old", "+new"})
	require.Equal(t, "context\nsame\nold", oldText)
	require.Equal(t, "context\nsame\nnew", newText)

	output, exitCode := bashOutputAndExit(claude.ToolResultBlock{
		IsError: true,
		Content: []claude.ContentBlock{claude.TextBlock{Text: "failed"}},
	})
	require.Equal(t, "failed", output)
	require.Nil(t, exitCode)

	output, exitCode = bashOutputAndExit(claude.ToolResultBlock{
		Raw: map[string]any{"content": map[string]any{
			"type":   toolResultBashCodeExecution,
			"stdout": "ok",
		}},
	})
	require.Equal(t, "ok", output)
	require.Nil(t, exitCode)

	updates := bashToolResultUpdates(claude.ToolResultBlock{
		ToolUseID: "bash-1",
		Raw:       map[string]any{"content": "done"},
	}, acp.ToolCallStatusCompleted, "")
	terminalExit, ok := updates[1].ToolCallUpdate.Meta[keyTerminalExit].(map[string]any)
	require.True(t, ok)
	require.NotContains(t, terminalExit, keyTerminalExitCode)
	require.Equal(t, "fallback", toolResultText(claude.ToolResultBlock{
		Content: []claude.ContentBlock{claude.TextBlock{Text: "fallback"}},
	}))
	require.Nil(t, rawToolResultContent("", false, false))
	require.Equal(t, "one", rawToolResultContent([]any{"", "one"}, false, false)[0].Content.Content.Text.Text)
	require.Equal(t, "ok", rawToolResultContent(map[string]any{"type": "text", "text": "ok"}, false, false)[0].Content.Content.Text.Text)
}

func mustInt(value int, ok bool) int {
	if !ok {
		return 0
	}

	return value
}

func updateParentToolUseID(update acp.SessionUpdate) any {
	var meta map[string]any
	switch {
	case update.AgentMessageChunk != nil:
		meta = update.AgentMessageChunk.Meta
	case update.AgentThoughtChunk != nil:
		meta = update.AgentThoughtChunk.Meta
	case update.Plan != nil:
		meta = update.Plan.Meta
	case update.ToolCall != nil:
		meta = update.ToolCall.Meta
	case update.ToolCallUpdate != nil:
		meta = update.ToolCallUpdate.Meta
	}

	claudeMeta, _ := meta[keyClaude].(map[string]any)

	return claudeMeta["parentToolUseId"]
}

func pair(first string, second string) [2]string {
	return [2]string{first, second}
}
