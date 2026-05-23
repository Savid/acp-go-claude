package main

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/coder/acp-go-sdk"
	claudeacp "github.com/savid/acp-go-claude"
)

const (
	agentPackage              = "github.com/savid/acp-go-claude/cmd/acp-go-claude"
	claudeSessionImportMethod = "_claude/session/import"
	claudeSessionImportFormat = "claude-jsonl"
	defaultSessionFile        = "session.jsonl"
	defaultPrompt             = "Reply with exactly RESUME_OK and do not use tools."
)

type client struct {
	output             io.Writer
	mu                 sync.Mutex
	messages           map[string]*messageDisplay
	fallback           messageDisplay
	lastAgentText      string
	agentRunText       string
	lastUpdateWasAgent bool
}

var _ acp.Client = (*client)(nil)

type agentConnection interface {
	Initialize(context.Context, acp.InitializeRequest) (acp.InitializeResponse, error)
	CallExtension(context.Context, string, any) (json.RawMessage, error)
	LoadSession(context.Context, acp.LoadSessionRequest) (acp.LoadSessionResponse, error)
	Prompt(context.Context, acp.PromptRequest) (acp.PromptResponse, error)
	CloseSession(context.Context, acp.CloseSessionRequest) (acp.CloseSessionResponse, error)
}

type startedAgent struct {
	conn  agentConnection
	close func()
	wait  func() error
}

type config struct {
	sessionFile string
	sessionID   string
	cwd         string
	prompt      string
}

type sessionImportParams struct {
	SessionID string            `json:"sessionId"`
	Cwd       string            `json:"cwd"`
	Format    string            `json:"format"`
	Entries   []json.RawMessage `json:"entries"`
}

var startAgent = startAgentProcess
var getwd = os.Getwd
var exit = os.Exit
var commandContext = exec.CommandContext
var runtimeCaller = runtime.Caller
var errPromptInterrupted = errors.New("typed prompt interrupted")

func defaultSessionFilePath() string {
	_, file, _, ok := runtimeCaller(0)
	if !ok {
		return defaultSessionFile
	}

	return filepath.Join(filepath.Dir(file), defaultSessionFile)
}

func (c *client) writer() io.Writer {
	if c.output != nil {
		return c.output
	}

	return os.Stdout
}

func (*client) ReadTextFile(_ context.Context, params acp.ReadTextFileRequest) (acp.ReadTextFileResponse, error) {
	if !filepath.IsAbs(params.Path) {
		return acp.ReadTextFileResponse{}, fmt.Errorf("path must be absolute: %s", params.Path)
	}

	data, err := os.ReadFile(params.Path)
	if err != nil {
		return acp.ReadTextFileResponse{}, err
	}

	return acp.ReadTextFileResponse{Content: string(data)}, nil
}

func (*client) WriteTextFile(_ context.Context, params acp.WriteTextFileRequest) (acp.WriteTextFileResponse, error) {
	if !filepath.IsAbs(params.Path) {
		return acp.WriteTextFileResponse{}, fmt.Errorf("path must be absolute: %s", params.Path)
	}

	if err := os.MkdirAll(filepath.Dir(params.Path), 0o755); err != nil {
		return acp.WriteTextFileResponse{}, err
	}

	return acp.WriteTextFileResponse{}, os.WriteFile(params.Path, []byte(params.Content), 0o600)
}

func (*client) RequestPermission(
	_ context.Context,
	params acp.RequestPermissionRequest,
) (acp.RequestPermissionResponse, error) {
	for _, option := range params.Options {
		if option.Kind == acp.PermissionOptionKindRejectOnce || option.Kind == acp.PermissionOptionKindRejectAlways {
			return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeSelected(option.OptionId)}, nil
		}
	}

	return acp.RequestPermissionResponse{Outcome: acp.NewRequestPermissionOutcomeCancelled()}, nil
}

type messageDisplay struct {
	text string
}

func (m *messageDisplay) writeText(output io.Writer, text string) (string, bool) {
	switch {
	case text == "":
		return "", false
	case m.text == text:
		return "", false
	case strings.HasPrefix(text, m.text):
		delta := text[len(m.text):]
		fmt.Fprint(output, delta)

		m.text = text

		return delta, true
	default:
		fmt.Fprint(output, text)

		m.text += text

		return text, true
	}
}

func (c *client) messageDisplay(messageID *string) *messageDisplay {
	if messageID == nil || *messageID == "" {
		return &c.fallback
	}

	if c.messages == nil {
		c.messages = make(map[string]*messageDisplay)
	}

	display := c.messages[*messageID]
	if display == nil {
		display = &messageDisplay{}
		c.messages[*messageID] = display
	}

	return display
}

func (c *client) SessionUpdate(_ context.Context, params acp.SessionNotification) error {
	c.mu.Lock()
	defer c.mu.Unlock()

	update := params.Update
	output := c.writer()

	switch {
	case update.UserMessageChunk != nil && update.UserMessageChunk.Content.Text != nil:
		text := update.UserMessageChunk.Content.Text.Text
		if text == "" {
			return nil
		}

		c.resetAgentRun()

		fmt.Fprintf(output, "\n[user] %s\n", text)
	case update.AgentMessageChunk != nil && update.AgentMessageChunk.Content.Text != nil:
		chunk := update.AgentMessageChunk
		if chunk.Content.Text.Text == "" {
			c.resetAgentRun()

			return nil
		}

		text := collapseRepeatedExactText(chunk.Content.Text.Text)
		display := c.messageDisplay(chunk.MessageId)

		if c.suppressRepeatedAgentText(display, text) {
			display.text = text

			return nil
		}

		if printed, ok := display.writeText(output, text); ok {
			c.lastAgentText = text
			if c.lastUpdateWasAgent {
				c.agentRunText += printed
			} else {
				c.agentRunText = printed
			}
		}

		c.lastUpdateWasAgent = true
	case update.AgentThoughtChunk != nil && update.AgentThoughtChunk.Content.Text != nil:
		text := update.AgentThoughtChunk.Content.Text.Text
		if text == "" {
			return nil
		}

		c.resetAgentRun()

		fmt.Fprintf(output, "\n[thought] %s\n", text)
	case update.ToolCall != nil:
		c.resetAgentRun()

		fmt.Fprintf(output, "\n[tool] %s %s\n", update.ToolCall.ToolCallId, update.ToolCall.Title)
	case update.ToolCallUpdate != nil && update.ToolCallUpdate.Status != nil:
		c.resetAgentRun()

		fmt.Fprintf(output, "\n[tool] %s %s\n", update.ToolCallUpdate.ToolCallId, *update.ToolCallUpdate.Status)
	}

	return nil
}

func (c *client) resetAgentRun() {
	c.lastUpdateWasAgent = false
	c.agentRunText = ""
	c.fallback.text = ""
}

func (c *client) suppressRepeatedAgentText(display *messageDisplay, text string) bool {
	switch {
	case c.agentRunText != "" && text == c.agentRunText:
		return true
	case c.agentRunText != "" && text == c.agentRunText+c.agentRunText:
		return true
	case !c.lastUpdateWasAgent:
		return false
	case display.text == "" && text == c.lastAgentText:
		return true
	case display.text != "" && text == display.text+display.text:
		return true
	case c.lastAgentText != "" && text == c.lastAgentText+c.lastAgentText:
		return true
	default:
		return false
	}
}

func collapseRepeatedExactText(text string) string {
	half := len(text) / 2
	if half == 0 || len(text)%2 != 0 {
		return text
	}

	if text[:half] == text[half:] {
		return text[:half]
	}

	return text
}

func (*client) CreateTerminal(context.Context, acp.CreateTerminalRequest) (acp.CreateTerminalResponse, error) {
	return acp.CreateTerminalResponse{TerminalId: "terminal-1"}, nil
}

func (*client) KillTerminal(context.Context, acp.KillTerminalRequest) (acp.KillTerminalResponse, error) {
	return acp.KillTerminalResponse{}, nil
}

func (*client) TerminalOutput(context.Context, acp.TerminalOutputRequest) (acp.TerminalOutputResponse, error) {
	return acp.TerminalOutputResponse{Output: "", Truncated: false}, nil
}

func (*client) ReleaseTerminal(context.Context, acp.ReleaseTerminalRequest) (acp.ReleaseTerminalResponse, error) {
	return acp.ReleaseTerminalResponse{}, nil
}

func (*client) WaitForTerminalExit(context.Context, acp.WaitForTerminalExitRequest) (acp.WaitForTerminalExitResponse, error) {
	return acp.WaitForTerminalExitResponse{}, nil
}

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()

	exit(run(ctx, os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}

func run(ctx context.Context, args []string, stdin io.Reader, stdout io.Writer, stderr io.Writer) int {
	cfg, err := parseConfig(args, stderr)
	if err != nil {
		printError(stderr, err)

		return 1
	}

	entries, err := loadSessionFile(cfg.sessionFile)
	if err != nil {
		printError(stderr, err)

		return 1
	}

	if cfg.sessionID == "" {
		cfg.sessionID = sessionIDFromEntries(entries)
	}

	if cfg.sessionID == "" {
		printError(stderr, errors.New("session id not found; pass -session-id"))

		return 1
	}

	agent, err := startAgent(ctx, stdout, stderr)
	if err != nil {
		printError(stderr, err)

		return 1
	}
	defer func() {
		if agent.close != nil {
			agent.close()
		}

		if agent.wait != nil {
			_ = agent.wait()
		}
	}()

	if err := runImportedResume(ctx, agent.conn, cfg, entries, stdin, stdout); err != nil {
		if ctx.Err() != nil {
			printError(stderr, ctx.Err())

			return 130
		}

		printError(stderr, err)

		return 1
	}

	return 0
}

func parseConfig(args []string, stderr io.Writer) (config, error) {
	cfg := config{
		sessionFile: defaultSessionFilePath(),
		prompt:      defaultPrompt,
	}

	flags := flag.NewFlagSet("resume-from-file", flag.ContinueOnError)
	flags.SetOutput(stderr)
	flags.StringVar(&cfg.sessionFile, "session-file", cfg.sessionFile, "Claude JSONL session file to import")
	flags.StringVar(&cfg.sessionID, "session-id", "", "Claude session ID; inferred from session_id/sessionId when omitted")
	flags.StringVar(&cfg.cwd, "cwd", "", "absolute working directory for the resumed Claude session")
	flags.StringVar(&cfg.prompt, "prompt", cfg.prompt, "prompt to send after session/resume")

	if err := flags.Parse(args); err != nil {
		return config{}, err
	}

	if positionalPrompt := strings.TrimSpace(strings.Join(flags.Args(), " ")); positionalPrompt != "" {
		cfg.prompt = positionalPrompt
	}

	if cfg.cwd == "" {
		cwd, err := getwd()
		if err != nil {
			return config{}, err
		}

		cfg.cwd = cwd
	}

	if !filepath.IsAbs(cfg.cwd) {
		return config{}, fmt.Errorf("cwd must be absolute: %s", cfg.cwd)
	}

	return cfg, nil
}

func loadSessionFile(path string) ([]json.RawMessage, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("open %s (copy a real Claude JSONL transcript here or pass -session-file): %w", path, err)
	}

	var entries []json.RawMessage

	for index, line := range bytes.Split(data, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}

		entry, err := validateJSONLine(line)
		if err != nil {
			return nil, fmt.Errorf("%s:%d: %w", path, index+1, err)
		}

		entries = append(entries, entry)
	}

	if len(entries) == 0 {
		return nil, fmt.Errorf("%s: no JSONL entries found", path)
	}

	return entries, nil
}

func validateJSONLine(line []byte) (json.RawMessage, error) {
	decoder := json.NewDecoder(bytes.NewReader(line))

	var object map[string]json.RawMessage
	if err := decoder.Decode(&object); err != nil {
		return nil, err
	}

	if object == nil {
		return nil, errors.New("entry must be a JSON object")
	}

	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return nil, errors.New("entry must contain exactly one JSON object")
	}

	return append(json.RawMessage(nil), line...), nil
}

func sessionIDFromEntries(entries []json.RawMessage) string {
	for _, entry := range entries {
		var object map[string]json.RawMessage
		if err := json.Unmarshal(entry, &object); err != nil {
			continue
		}

		for _, key := range []string{"session_id", "sessionId"} {
			var sessionID string
			if err := json.Unmarshal(object[key], &sessionID); err == nil && strings.TrimSpace(sessionID) != "" {
				return strings.TrimSpace(sessionID)
			}
		}
	}

	return ""
}

func startAgentProcess(ctx context.Context, output io.Writer, stderr io.Writer) (*startedAgent, error) {
	cmd := commandContext(ctx, "go", "run", agentPackage)

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, err
	}

	agentStdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, err
	}

	cmd.Stderr = stderr

	err = cmd.Start()
	if err != nil {
		return nil, err
	}

	agentOutputGate := newConnectionInputGate(agentStdout)
	conn := acp.NewClientSideConnection(&client{output: output}, stdin, agentOutputGate)
	conn.SetLogger(slog.New(slog.DiscardHandler))
	agentOutputGate.open()

	return &startedAgent{
		conn: conn,
		close: func() {
			_ = stdin.Close()
		},
		wait: cmd.Wait,
	}, nil
}

type connectionInputGate struct {
	reader io.Reader
	ready  chan struct{}
	once   sync.Once
}

// connectionInputGate blocks the SDK receive goroutine until the connection
// logger is installed. The SDK starts receiving inside NewClientSideConnection.
func newConnectionInputGate(reader io.Reader) *connectionInputGate {
	return &connectionInputGate{
		reader: reader,
		ready:  make(chan struct{}),
	}
}

func (g *connectionInputGate) open() {
	g.once.Do(func() {
		close(g.ready)
	})
}

func (g *connectionInputGate) Read(p []byte) (int, error) {
	<-g.ready

	return g.reader.Read(p)
}

func runImportedResume(
	ctx context.Context,
	conn agentConnection,
	cfg config,
	entries []json.RawMessage,
	stdin io.Reader,
	stdout io.Writer,
) error {
	_, err := conn.Initialize(ctx, acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber})
	if err != nil {
		return err
	}

	_, err = conn.CallExtension(ctx, claudeSessionImportMethod, sessionImportParams{
		SessionID: cfg.sessionID,
		Cwd:       cfg.cwd,
		Format:    claudeSessionImportFormat,
		Entries:   entries,
	})
	if err != nil {
		return err
	}

	sessionID := acp.SessionId(cfg.sessionID)

	fmt.Fprintln(stdout, "== previous session ==")

	_, err = conn.LoadSession(ctx, claudeacp.LoadSessionRequest(sessionID, cfg.cwd))
	if err != nil {
		return err
	}
	defer func() {
		_, _ = conn.CloseSession(ctx, acp.CloseSessionRequest{SessionId: sessionID})
	}()

	fmt.Fprintln(stdout, "\n== resume smoke test ==")

	resp, err := promptSession(ctx, conn, sessionID, cfg.prompt)
	if err != nil {
		return err
	}

	fmt.Fprintf(stdout, "\n\nresumed session: %s\nstop reason: %s\n", sessionID, resp.StopReason)

	typedPrompt, err := readTypedPrompt(ctx, stdin, stdout)
	if errors.Is(err, errPromptInterrupted) {
		fmt.Fprintln(stdout, "\ninterrupted; closing session")

		return nil
	}

	if err != nil {
		return err
	}

	if typedPrompt == "" {
		fmt.Fprintln(stdout, "\nno typed prompt entered; closing session")

		return nil
	}

	fmt.Fprintln(stdout, "\n== typed prompt ==")

	resp, err = promptSession(ctx, conn, sessionID, typedPrompt)
	if err != nil {
		return err
	}

	fmt.Fprintf(stdout, "\n\nstop reason: %s\n", resp.StopReason)

	return nil
}

func promptSession(
	ctx context.Context,
	conn agentConnection,
	sessionID acp.SessionId,
	prompt string,
) (acp.PromptResponse, error) {
	return conn.Prompt(ctx, claudeacp.TextPromptRequest(sessionID, prompt))
}

type typedPromptResult struct {
	text string
	err  error
}

func readTypedPrompt(ctx context.Context, stdin io.Reader, stdout io.Writer) (string, error) {
	fmt.Fprint(stdout, "\nenter one prompt (blank or Ctrl-C to exit): ")

	select {
	case <-ctx.Done():
		return "", errPromptInterrupted
	default:
	}

	result := make(chan typedPromptResult, 1)

	go func() {
		text, err := bufio.NewReader(stdin).ReadString('\n')
		if err != nil && !errors.Is(err, io.EOF) {
			result <- typedPromptResult{err: err}

			return
		}

		result <- typedPromptResult{text: strings.TrimSpace(text)}
	}()

	select {
	case <-ctx.Done():
		return "", errPromptInterrupted
	case got := <-result:
		return got.text, got.err
	}
}

func printError(stderr io.Writer, err error) {
	_, _ = fmt.Fprintf(stderr, "resume-from-file: %v\n", err)
}
