package claudeacp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"

	"github.com/savid/acp-go-claude/internal/claude"
)

const fakeClaudeEnv = "ACP_GO_CLAUDE_TEST_FAKE"
const fakeClaudeEnvVersion = "ACP_GO_CLAUDE_TEST_VERSION"

type fakeClaude struct {
	id, path string
	mu       sync.Mutex
	turnMu   sync.Mutex
	abort    chan struct{}
	replies  map[string]chan json.RawMessage
}

func runFakeClaude(args []string) int {
	if slices.Contains(args, "--version") {
		version := os.Getenv(fakeClaudeEnvVersion)
		if version == "" {
			version = "2.1.270"
		}
		fmt.Println(version + " (Claude Code)")

		return 0
	}
	cwd, _ := os.Getwd()
	id := ""
	for i, arg := range args {
		if (arg == "--session-id" || arg == "--resume") && i+1 < len(args) {
			id = args[i+1]
		}
	}
	f := &fakeClaude{id: id, path: claude.SessionPath(os.Getenv(claude.EnvConfigDir), cwd, id), replies: make(map[string]chan json.RawMessage)}
	if dump := os.Getenv("ACP_GO_CLAUDE_TEST_ENV_DUMP"); dump != "" {
		_ = os.WriteFile(dump, []byte(strings.Join(os.Environ(), "\n")), 0o600)
	}
	settings := map[string]any{"model": "default", nativeEffortLevel: "low", nativeOutputStyle: "default"}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 64<<10), 16<<20)
	for scanner.Scan() {
		var frame struct {
			Type      string         `json:"type"`
			RequestID string         `json:"request_id"` //nolint:tagliatelle // Claude uses this native wire spelling.
			Request   map[string]any `json:"request"`
			Message   claude.Message `json:"message"`
			Response  struct {
				RequestID string          `json:"request_id"` //nolint:tagliatelle // Claude uses this native wire spelling.
				Response  json.RawMessage `json:"response"`
			} `json:"response"`
		}
		if json.Unmarshal(scanner.Bytes(), &frame) != nil {
			return 1
		}
		switch frame.Type {
		case "control_request":
			result := any(map[string]any{})
			switch frame.Request["subtype"] {
			case "initialize":
				result = claude.InitializeResponse{Models: []claude.Model{{Value: "default", DisplayName: "Default", SupportsEffort: true, SupportedEffortLevels: []string{"low", "high"}, SupportsAutoMode: true}, {Value: "haiku", DisplayName: "Haiku"}}, Commands: []claude.Command{{Name: "compact", Description: "Compact"}, {Name: "clear"}}, PermissionMode: "default", OutputStyle: "default", AvailableOutputStyles: []string{"default", "concise"}}
			case "get_settings":
				result = map[string]any{"effective": settings}
			case "set_model":
				settings["model"] = frame.Request["model"]
			case "set_permission_mode":
				if _, ok := frame.Request["mode"].(string); !ok {
					return 2
				}
			case "apply_flag_settings":
				values, ok := frame.Request["settings"].(map[string]any)
				if !ok {
					return 2
				}
				maps.Copy(settings, values)
			case "get_context_usage":
				result = claude.ContextUsage{TotalTokens: 20, MaxTokens: 1000, Model: "default"}
			case "interrupt":
				f.turnMu.Lock()
				if f.abort != nil {
					close(f.abort)
					f.abort = nil
				}
				f.turnMu.Unlock()
			default:
				f.write(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "error", "request_id": frame.RequestID, "error": "unsupported fake control operation"}})

				continue
			}
			if initialized, ok := result.(claude.InitializeResponse); ok && os.Getenv("ACP_GO_CLAUDE_TEST_COMMANDS") == "empty" {
				initialized.Commands = []claude.Command{}
				result = initialized
			}
			f.write(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "success", "request_id": frame.RequestID, "response": result}})
		case "user":
			blocks, _ := frame.Message.ContentBlocks()
			var text strings.Builder
			for _, block := range blocks {
				if block.Type == "text" {
					text.WriteString(block.Text)
				}
			}
			abort := make(chan struct{})
			f.turnMu.Lock()
			f.abort = abort
			f.turnMu.Unlock()
			go f.turn(text.String(), abort)
		case "control_response":
			f.mu.Lock()
			reply := f.replies[frame.Response.RequestID]
			f.mu.Unlock()
			if reply != nil {
				reply <- frame.Response.Response
			}
		}
	}

	return 0
}
func (f *fakeClaude) write(value any) {
	f.mu.Lock()
	defer f.mu.Unlock()
	_ = json.NewEncoder(os.Stdout).Encode(value)
}
func (f *fakeClaude) row(role, text string) {
	_ = os.MkdirAll(filepath.Dir(f.path), 0o700)
	file, err := os.OpenFile(f.path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return
	}
	defer file.Close()
	_ = json.NewEncoder(file).Encode(map[string]any{"type": role, "sessionId": f.id, "message": map[string]any{"id": role + text, "role": role, "content": []map[string]any{{"type": "text", "text": text}}}})
}
func (f *fakeClaude) turn(text string, abort <-chan struct{}) {
	if text == "NOISE" {
		fmt.Fprintln(os.Stderr, "native stderr noise")
		fmt.Println("native stdout noise")

		return
	}
	if text == "CRASH" {
		os.Exit(23)
	}
	f.row("user", text)
	f.write(map[string]any{"type": "stream_event", "session_id": f.id, "event": map[string]any{"type": nativeMessageStart, "message": map[string]any{"id": "reply-" + text, "role": "assistant", "content": []any{}}}})
	if text == "BLOCK" {
		<-abort
		f.write(map[string]any{"type": "result", "session_id": f.id, "stop_reason": "aborted"})

		return
	}
	outcome := ""
	if text == "PERMISSION" || text == "ELICIT" || text == "ELICIT_URL" || text == "QUESTION" {
		request := map[string]any{"subtype": "can_use_tool", "tool_name": "Write", "tool_use_id": "tool-1", "input": map[string]any{"file_path": "/tmp/example", "content": "hello"}}
		if text == "ELICIT" {
			request = map[string]any{"subtype": "elicitation", "mode": "form", "message": "Choose a color", "requestedSchema": map[string]any{"type": "object", "properties": map[string]any{"color": map[string]any{"type": "string"}}, "required": []string{"color"}}}
		}
		if text == "ELICIT_URL" {
			request = map[string]any{"subtype": "elicitation", "mode": "url", "message": "Open the page", "url": "https://example.test/authorize", "elicitationId": "url-1"}
		}
		if text == "QUESTION" {
			request = map[string]any{"subtype": "can_use_tool", "tool_name": "AskUserQuestion", "tool_use_id": "question-tool", "input": map[string]any{"questions": []any{map[string]any{"question": "Pick a color", "options": []any{map[string]any{"label": "blue"}, map[string]any{"label": "red"}}}}}}
		}
		reply := make(chan json.RawMessage, 1)
		f.mu.Lock()
		f.replies["question-1"] = reply
		f.mu.Unlock()
		f.write(map[string]any{"type": "control_request", "request_id": "question-1", "request": request})
		select {
		case value := <-reply:
			var answer map[string]any
			if json.Unmarshal(value, &answer) != nil {
				return
			}
			if text == "PERMISSION" {
				outcome, _ = answer["behavior"].(string)
			}
			if text == "ELICIT" || text == "ELICIT_URL" {
				outcome, _ = answer["action"].(string)
			}
			if text == "QUESTION" {
				var answer struct {
					UpdatedInput struct {
						Answers map[string]string `json:"answers"`
					} `json:"updatedInput"`
				}
				_ = json.Unmarshal(value, &answer)
				if answer.UpdatedInput.Answers["Pick a color"] != "blue" {
					f.write(map[string]any{"type": "result", "is_error": true, "errors": []string{"missing answer"}})

					return
				}
			}
		case <-abort:
		}
	}
	if text == "ERROR" {
		f.write(map[string]any{"type": "result", "session_id": f.id, "is_error": true, "errors": []string{"provider refused"}})

		return
	}
	answer := "reply: " + text
	if outcome != "" {
		answer = outcome
	}
	f.write(map[string]any{"type": "stream_event", "session_id": f.id, "event": map[string]any{"type": "content_block_delta", "delta": map[string]any{"type": "text_delta", "text": answer[:3]}}})
	f.write(map[string]any{"type": "assistant", "session_id": f.id, "message": map[string]any{"id": "reply-" + text, "role": "assistant", "content": []map[string]any{{"type": "text", "text": answer}}}})
	f.row("assistant", answer)
	f.write(map[string]any{"type": "result", "session_id": f.id, "subtype": "success", "usage": map[string]any{"input_tokens": 10, "output_tokens": 5}, "total_cost_usd": 0.01})
	if text == "AGENTWORK" {
		f.turn("background", abort)
	}
}
