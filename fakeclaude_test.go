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
const fakeClaudeEnvUnsetEffort = "ACP_GO_CLAUDE_TEST_UNSET_EFFORT"

// fakeClaudeEnvAccountUsage selects the get_usage answer: the allowance below,
// "unavailable" for a home with no allowance, "unread" for an account whose
// report claude could not fetch, "refuse" for a control error, or "hold" to
// create fakeClaudeEnvAccountUsageHold and answer once it is removed.
const fakeClaudeEnvAccountUsage = "ACP_GO_CLAUDE_TEST_ACCOUNT_USAGE"

// fakeClaudeEnvAccountUsageHold names the file a held get_usage waits on.
const fakeClaudeEnvAccountUsageHold = "ACP_GO_CLAUDE_TEST_ACCOUNT_USAGE_HOLD"

// fakeAccountUsage is the native get_usage answer shape: fixed window members
// with null placeholders for windows the account lacks, the model-scoped
// windows, and spending.
var fakeAccountUsage = map[string]any{
	"subscription_type":     "enterprise",
	"rate_limits_available": true,
	"rate_limits": map[string]any{
		"five_hour":        map[string]any{"utilization": 4, "resets_at": "2026-09-17T03:30:00.051309+00:00", "limit_dollars": nil, "locked_reason": nil},
		"seven_day":        map[string]any{"utilization": 15, "resets_at": "2026-09-19T08:00:00.051333+00:00", "limit_dollars": nil, "locked_reason": nil},
		"seven_day_opus":   nil,
		"seven_day_sonnet": nil,
		"extra_usage":      map[string]any{"is_enabled": false, "utilization": nil},
		"spend":            map[string]any{"used": map[string]any{"amount_minor": 0, "currency": "AUD", "exponent": 2}, "limit": nil, "percent": 0, "enabled": false},
		"model_scoped":     []any{map[string]any{"display_name": "Fable", "utilization": 22, "resets_at": "2026-09-19T08:00:00.051625+00:00"}},
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
	case "unread":
		return map[string]any{"subscription_type": "max", "rate_limits_available": true, "rate_limits": nil, "behaviors": nil}, true
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

	// usageMu guards the selected model and the session's cumulative cost,
	// which runs and control requests both read.
	usageMu sync.Mutex
	model   string
	cost    float64
}

// fakeContextWindow is each fake model's context window.
func fakeContextWindow(model string) int64 {
	if model == "haiku" {
		return 500
	}

	return 1000
}

// fakeCallCost is every fake model call's cost, exact in binary so sums
// compare.
const fakeCallCost = 0.25

// fakeCall is one model call's native usage: the new input, the cached prefix
// it read, the prefix it wrote to the cache, and its whole output.
type fakeCall struct {
	input, cacheRead, cacheWrite, output int
}

// start is the usage message_start carries: the request and an opening
// output figure.
func (c fakeCall) start() map[string]any {
	return map[string]any{"input_tokens": c.input, "cache_read_input_tokens": c.cacheRead, "cache_creation_input_tokens": c.cacheWrite, "output_tokens": 1}
}

// end is the usage message_delta carries: the request restated and the whole
// output.
func (c fakeCall) end() map[string]any {
	return map[string]any{"input_tokens": c.input, "cache_read_input_tokens": c.cacheRead, "cache_creation_input_tokens": c.cacheWrite, "output_tokens": c.output}
}

// fakeRun accumulates one query's calls as claude sums them for its result.
type fakeRun struct {
	f     *fakeClaude
	model string
	sum   fakeCall
}

// begin opens one model call's stream.
func (r *fakeRun) begin(id string, start map[string]any) {
	r.f.write(map[string]any{"type": "stream_event", "uuid": "stream-" + id, "session_id": r.f.id, "event": map[string]any{"type": nativeMessageStart, "message": map[string]any{"id": id, "role": "assistant", "model": r.model, "content": []any{}, "usage": start}}})
}

// assistant writes one assistant record of a call. Its usage repeats the
// call's opening figure, as claude's records do.
func (r *fakeRun) assistant(id string, row string, start map[string]any, content []map[string]any) {
	r.f.write(map[string]any{"type": "assistant", "uuid": row, "session_id": r.f.id, "message": map[string]any{"id": id, "role": "assistant", "model": r.model, "content": content, "usage": start}})
}

// finish closes one model call: its message_delta, then its usage in the
// query's sum and the session's cost.
func (r *fakeRun) finish(call fakeCall, delta map[string]any) {
	r.f.write(map[string]any{"type": "stream_event", "session_id": r.f.id, "event": map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": "end_turn"}, "usage": delta}})
	r.spend(call)
}

// spend counts one call that reached the provider.
func (r *fakeRun) spend(call fakeCall) {
	r.sum.input += call.input
	r.sum.cacheRead += call.cacheRead
	r.sum.cacheWrite += call.cacheWrite
	r.sum.output += call.output

	r.f.usageMu.Lock()
	r.f.cost += fakeCallCost
	r.f.usageMu.Unlock()
}

// step is one complete tool-using model call and its tool result.
func (r *fakeRun) step(id string, call fakeCall) {
	r.begin(id, call.start())
	r.assistant(id, "assistant-"+id, call.start(), []map[string]any{{"type": "tool_use", "id": "tool-" + id, "name": "Bash", "input": map[string]any{"command": "true"}}})
	r.finish(call, call.end())
	r.f.write(map[string]any{"type": "user", "session_id": r.f.id, "message": map[string]any{"role": "user", "content": []map[string]any{{"type": "tool_result", "tool_use_id": "tool-" + id, "content": "ok"}}}})
}

// result reports the query: its summed usage, the session's cumulative cost,
// and the context window of each model the session used.
func (r *fakeRun) result(text string) map[string]any {
	r.f.usageMu.Lock()
	cost := r.f.cost
	r.f.usageMu.Unlock()

	return map[string]any{"type": "result", "uuid": "result-" + text, "session_id": r.f.id, "subtype": "success",
		"usage":          map[string]any{"input_tokens": r.sum.input, "cache_read_input_tokens": r.sum.cacheRead, "cache_creation_input_tokens": r.sum.cacheWrite, "output_tokens": r.sum.output},
		"total_cost_usd": cost,
		"modelUsage": map[string]any{
			r.model:      map[string]any{"contextWindow": fakeContextWindow(r.model)},
			"fake-other": map[string]any{"contextWindow": 9000},
		}}
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
	settings := map[string]any{"model": launchModel, nativeOutputStyle: "default"}
	f.model = launchModel
	if os.Getenv(fakeClaudeEnvUnsetEffort) != "1" {
		settings[nativeEffortLevel] = "low"
	}
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
				model, _ := frame.Request["model"].(string)
				f.usageMu.Lock()
				f.model = model
				f.usageMu.Unlock()
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
				f.usageMu.Lock()
				result = claude.ContextUsage{TotalTokens: 20, MaxTokens: fakeContextWindow(f.model)}
				f.usageMu.Unlock()
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
	f.usageMu.Lock()
	run := &fakeRun{f: f, model: f.model}
	f.usageMu.Unlock()

	id, call := "reply-"+text, fakeCall{input: 10, output: 5}

	if text == "NOSTREAM" {
		// A call claude made without streaming reports only through its
		// records, which all carry the call's usage.
		call = fakeCall{input: 200, cacheRead: 300, output: 7}
		run.assistant(id, "assistant-thought-"+id, call.end(), []map[string]any{{"type": "thinking", "thinking": "hm"}})
		run.assistant(id, "assistant-"+id, call.end(), []map[string]any{{"type": "text", "text": "reply: " + text}})
		run.spend(call)
		f.row("assistant", "reply: "+text)
		f.write(run.result(text))

		return
	}

	call, start := run.prelude(text, id, call)

	run.begin(id, start)

	if text == "QUOTA_SESSION_EXHAUSTED" {
		f.write(map[string]any{"type": "rate_limit_event", "session_id": f.id, "rate_limit_info": map[string]any{"status": "rejected", "rateLimitType": "five_hour", "utilization": 1, "resetsAt": time.Now().Add(time.Hour).Unix()}})
		f.write(map[string]any{"type": "assistant", "session_id": f.id, "error": "rate_limit"})
		f.write(map[string]any{"type": "result", "session_id": f.id, "is_error": true, "errors": []string{"usage limit"}})

		return
	}
	if text == "BLOCK" || text == "STEPSLOW" {
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
	run.assistant(id, "assistant-"+text, start, []map[string]any{{"type": "text", "text": answer}})
	f.row("assistant", answer)
	run.finish(call, call.end())

	if text == "COMPACTEND" {
		// A manual compaction runs after the last call of the query.
		f.write(fakeCompactBoundary(f.id, "manual"))
	}

	result := run.result(text)
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

// prelude runs a script's model calls up to its final call and returns that
// call with the usage its message_start carries.
func (r *fakeRun) prelude(text string, id string, call fakeCall) (fakeCall, map[string]any) {
	switch text {
	case "GATEWAY":
		// A gateway that reports usage only at the end of the stream opens
		// the call with zeros and no cache figures.
		return fakeCall{input: 300, cacheRead: 700, output: 40}, map[string]any{"input_tokens": 0, "output_tokens": 0, "cache_read_input_tokens": nil, "cache_creation_input_tokens": nil}
	case "EMPTY":
		// A provider that reports no usage for the call at all.
		return fakeCall{}, fakeCall{}.end()
	}

	call = r.steps(text, id, call)

	return call, call.start()
}

// steps runs the model calls a script makes before its final call and returns
// that final call.
func (r *fakeRun) steps(text string, id string, call fakeCall) fakeCall {
	switch text {
	case "MULTI":
		// Each call resends the context the previous one left.
		r.step(id+"-1", fakeCall{input: 100, cacheRead: 1000, output: 20})
		r.step(id+"-2", fakeCall{input: 50, cacheRead: 1120, output: 30})

		return fakeCall{input: 40, cacheRead: 1200, output: 10}
	case "STEER":
		// Input queued while the query runs joins it at the next tool
		// boundary, and the query carries on.
		r.step(id+"-1", fakeCall{input: 100, cacheRead: 1000, output: 20})
		r.f.write(map[string]any{"type": "user", "session_id": r.f.id, "message": map[string]any{"role": "user", "content": []map[string]any{{"type": "text", "text": "steer"}}}})

		return fakeCall{input: 30, cacheRead: 1120, output: 10}
	case "RETRY":
		// The provider drops the first attempt mid-stream and claude retries.
		r.begin(id+"-1", fakeCall{input: 100, cacheRead: 1000}.start())
		r.f.write(map[string]any{"type": "system", "subtype": "api_retry", "session_id": r.f.id, "attempt": 1, "max_retries": 10, "retry_delay_ms": 0, "error_status": 529})

		return fakeCall{input: 100, cacheRead: 1000, output: 20}
	case "COMPACT":
		// The context crosses claude's threshold, so it compacts before the
		// next call.
		r.step(id+"-1", fakeCall{input: 100, cacheRead: 800, output: 50})
		r.f.write(fakeCompactBoundary(r.f.id, "auto"))

		return fakeCall{input: 10, cacheRead: 300, output: 5}
	case "COMPACTEND":
		return fakeCall{input: 100, cacheRead: 800, output: 50}
	case "STEPSLOW":
		r.step(id+"-1", fakeCall{input: 100, cacheRead: 1000, output: 20})

		return fakeCall{input: 50, cacheRead: 1120, output: 30}
	}

	return call
}

// fakeCompactBoundary is the record claude writes once a compaction replaced
// the context with a summary.
func fakeCompactBoundary(session string, trigger string) map[string]any {
	return map[string]any{"type": "system", "subtype": nativeCompactBoundary, "session_id": session, "compact_metadata": map[string]any{"trigger": trigger, "pre_tokens": 950, "post_tokens": 120}}
}
