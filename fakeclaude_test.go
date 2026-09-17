package claudeacp

import (
	"bufio"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"

	"github.com/savid/acp-go-claude/internal/claude"
)

const fakeClaudeEnv = "ACP_GO_CLAUDE_TEST_FAKE"
const fakeClaudeEnvArgvDump = "ACP_GO_CLAUDE_TEST_ARGV_DUMP"

// fakeClaudeEnvAccountUsage selects the get_usage answer: the allowance below,
// "unavailable" for a home with no allowance, "refuse" for a control error, or
// "hold" to create fakeClaudeEnvAccountUsageHold and answer once it is removed.
const fakeClaudeEnvAccountUsage = "ACP_GO_CLAUDE_TEST_ACCOUNT_USAGE"

// fakeClaudeEnvAccountUsageHold names the file a held get_usage waits on.
const fakeClaudeEnvAccountUsageHold = "ACP_GO_CLAUDE_TEST_ACCOUNT_USAGE_HOLD"

// fakeScopeKey is the native member scoping one usage limit to a model.
const fakeScopeKey = "scope"

// fakeAccountUsage is the native get_usage answer shape: fixed window members
// and null placeholders beside the generic limits list the adapter reads.
var fakeAccountUsage = map[string]any{
	"subscription_type":     "enterprise",
	"rate_limits_available": true,
	"rate_limits": map[string]any{
		"five_hour":      map[string]any{"utilization": 4, "resets_at": "2026-09-17T03:30:00.051309+00:00"},
		"seven_day":      map[string]any{"utilization": 15, "resets_at": "2026-09-19T08:00:00.051333+00:00"},
		"seven_day_opus": nil,
		"limits": []any{
			map[string]any{"kind": limitKindSession, "group": limitKindSession, "percent": 4, "severity": "normal", "resets_at": "2026-09-17T03:30:00.051309+00:00", fakeScopeKey: nil, "is_active": false},
			map[string]any{"kind": "weekly_all", "group": "weekly", "percent": 15, "severity": "normal", "resets_at": "2026-09-19T08:00:00.051333+00:00", fakeScopeKey: nil, "is_active": false},
			map[string]any{"kind": "weekly_scoped", "group": "weekly", "percent": 22, "severity": "normal", "resets_at": "2026-09-19T08:00:00.051625+00:00", fakeScopeKey: map[string]any{"model": map[string]any{"id": nil, "display_name": "Fable"}, "surface": nil}, "is_active": true},
		},
	},
	"behaviors": nil,
}

// fakeClaudeEnvResumeHold names a file a resumed fake claude creates before it
// stops answering, so a test can act while the adapter is still relaunching.
const fakeClaudeEnvResumeHold = "ACP_GO_CLAUDE_TEST_RESUME_HOLD"

// fakeClaudeResumeHold is how long a held resume refuses to serve. It outlasts
// the shutdown the adapter sends when it gives up on the relaunch.
const fakeClaudeResumeHold = 30 * time.Second

// accountUsage answers get_usage per fakeClaudeEnvAccountUsage. A refusal is
// written here and reported as unanswered.
func (f *fakeClaude) accountUsage(requestID string) (any, bool) {
	switch os.Getenv(fakeClaudeEnvAccountUsage) {
	case "unavailable":
		return map[string]any{"subscription_type": nil, "rate_limits_available": false, "rate_limits": nil, "behaviors": nil}, true
	case "refuse":
		f.write(map[string]any{"type": "control_response", "response": map[string]any{"subtype": "error", "request_id": requestID, "error": "usage unavailable"}})

		return nil, false
	case "hold":
		hold := os.Getenv(fakeClaudeEnvAccountUsageHold)
		_ = os.WriteFile(hold, []byte("held\n"), 0o600)

		for {
			if _, err := os.Stat(hold); err != nil {
				return fakeAccountUsage, true
			}

			time.Sleep(10 * time.Millisecond)
		}
	default:
		return fakeAccountUsage, true
	}
}

type fakeClaude struct {
	id, path string
	mu       sync.Mutex
	turnMu   sync.Mutex
	abort    chan struct{}
	replies  map[string]chan json.RawMessage
}

// launchFlag returns the value of a repeated <name> <value> launch flag, and
// whether the flag was present at all.
func launchFlag(args []string, name string) (string, bool) {
	for index, arg := range args {
		if arg == name && index+1 < len(args) {
			return args[index+1], true
		}

		if value, found := strings.CutPrefix(arg, name+"="); found {
			return value, true
		}
	}

	return "", false
}

// fakeClaudeSessionID reads the session id from the launch flags, and holds a
// resumed launch for as long as a test asked before serving it.
func fakeClaudeSessionID(args []string) string {
	id, _ := launchFlag(args, "--session-id")
	resumed, isResume := launchFlag(args, "--resume")
	if isResume {
		id = resumed
	}
	if hold := os.Getenv(fakeClaudeEnvResumeHold); hold != "" && isResume {
		_ = os.WriteFile(hold, []byte("held\n"), 0o600)
		time.Sleep(fakeClaudeResumeHold)
	}

	return id
}

func runFakeClaude(args []string) int {
	cwd, _ := os.Getwd()
	id := fakeClaudeSessionID(args)
	f := &fakeClaude{id: id, path: claude.SessionPath(os.Getenv(claude.EnvConfigDir), cwd, id), replies: make(map[string]chan json.RawMessage)}
	if dump := os.Getenv("ACP_GO_CLAUDE_TEST_ENV_DUMP"); dump != "" {
		_ = os.WriteFile(dump, []byte(strings.Join(os.Environ(), "\n")), 0o600)
	}

	if dump := os.Getenv(fakeClaudeEnvArgvDump); dump != "" {
		_ = os.WriteFile(dump, []byte(strings.Join(args, "\n")), 0o600)
	}
	// The launch flags decide what the settings report, so a dropped flag is
	// visible to a test rather than silently absorbed. The handshake reports the
	// native default permission mode, so a caller's requested mode has to
	// survive without any echo of --permission-mode.
	launchModel := nativeDefault
	if model, ok := launchFlag(args, "--model"); ok {
		launchModel = model
	}
	structured, structuredRequested := launchFlag(args, "--json-schema")
	settings := map[string]any{"model": launchModel, nativeEffortLevel: "low", nativeOutputStyle: "default"}
	// Claude Code announces itself before it answers anything, so the adapter
	// must already be able to forward a record when the process starts.
	f.write(map[string]any{"type": "system", "subtype": "init", "uuid": uuid.NewString(), "session_id": id})
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
				result = claude.InitializeResponse{Models: []claude.Model{{Value: "default", DisplayName: "Default", SupportsEffort: true, SupportedEffortLevels: []string{"low", "high"}, SupportsAutoMode: true}, {Value: "haiku", DisplayName: "Haiku"}}, Commands: []claude.Command{{Name: "compact", Description: "Compact"}, {Name: "clear"}}, PermissionMode: nativeDefault, OutputStyle: "default", AvailableOutputStyles: []string{"default", "concise"}}
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
				result = claude.ContextUsage{TotalTokens: 20, MaxTokens: 1000}
			case "get_usage":
				var answered bool
				if result, answered = f.accountUsage(frame.RequestID); !answered {
					continue
				}
			case "interrupt":
				// A harness that will not abort is the rung the shutdown ladder
				// has to survive.
				if os.Getenv("ACP_GO_CLAUDE_TEST_IGNORE_INTERRUPT") == "1" {
					break
				}
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
			if text.String() == "EXIT" {
				// The turn completes and then the harness leaves, so the
				// generation ends with no ACP request waiting on it.
				f.turn(text.String(), abort, structured, structuredRequested)

				return 0
			}
			go f.turn(text.String(), abort, structured, structuredRequested)
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
	_ = json.NewEncoder(file).Encode(map[string]any{"type": role, "uuid": uuid.NewString(), "sessionId": f.id, "cwd": filepath.Dir(f.path), "timestamp": "2026-01-01T00:00:00Z", "message": map[string]any{"id": role + text, "role": role, "content": []map[string]any{{"type": "text", "text": text}}}})
}
func (f *fakeClaude) turn(text string, abort <-chan struct{}, structured string, structuredRequested bool) {
	if text == "NOISE" {
		fmt.Fprintln(os.Stderr, "native stderr noise")
		fmt.Println("native stdout noise")

		return
	}
	if text == "CRASH" {
		os.Exit(23)
	}
	f.row("user", text)
	f.write(map[string]any{"type": "stream_event", "uuid": "stream-" + text, "session_id": f.id, "event": map[string]any{"type": nativeMessageStart, "message": map[string]any{"id": "reply-" + text, "role": "assistant", "content": []any{}}}})
	if text == "QUOTA_SESSION_EXHAUSTED" {
		f.write(map[string]any{"type": "rate_limit_event", "session_id": f.id, "rate_limit_info": map[string]any{"status": "rejected", "rateLimitType": "five_hour", "utilization": 1, "resetsAt": time.Now().Add(time.Hour).Unix()}})
		f.write(map[string]any{"type": "assistant", "session_id": f.id, "error": "rate_limit"})
		f.write(map[string]any{"type": "result", "session_id": f.id, "is_error": true, "errors": []string{"usage limit"}})

		return
	}
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
	f.write(map[string]any{"type": "assistant", "uuid": "assistant-" + text, "session_id": f.id, "message": map[string]any{"id": "reply-" + text, "role": "assistant", "content": []map[string]any{{"type": "text", "text": answer}}}})
	f.row("assistant", answer)
	result := map[string]any{"type": "result", "uuid": "result-" + text, "session_id": f.id, "subtype": "success", "usage": map[string]any{"input_tokens": 10, "output_tokens": 5}, "total_cost_usd": 0.01}
	if structuredRequested {
		var decoded any
		if json.Unmarshal([]byte(structured), &decoded) == nil {
			result["structured_output"] = map[string]any{"schema": decoded, "answer": answer}
		}
	}
	f.write(result)
	if text == "AGENTHANG" {
		f.turn("BLOCK", abort, structured, structuredRequested)
	}
	if text == "AGENTWORK" {
		f.turn("background", abort, structured, structuredRequested)
	}
}
