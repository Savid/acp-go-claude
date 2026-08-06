package claude

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"
)

const (
	defaultInitializeTimeout = time.Minute
	// permissionModeField and elicitationModeField are separate wire members
	// that happen to share a name: one is the mode a set_permission_mode
	// request carries, the other is the mode an elicitation request carries.
	permissionModeField  = "mode"
	elicitationModeField = "mode"
)

// Client is a stateful Claude CLI stream-json session.
type Client struct {
	log       *slog.Logger
	options   Options
	transport Transport

	stateMu    sync.RWMutex
	controller *Controller
	cancel     context.CancelFunc
	closed     bool
	closeOnce  sync.Once
	closeErr   error

	infoMu         sync.RWMutex
	initializeInfo InitializeInfo

	controlTurnMu    sync.RWMutex
	controlTurnNonce string
}

// InitializeInfo describes metadata returned by Claude's control initialize response.
type InitializeInfo struct {
	Commands              []SlashCommand
	Models                []AvailableModelInfo
	OutputStyle           string
	AvailableOutputStyles []string
}

// SlashCommand describes one Claude slash command.
type SlashCommand struct {
	Name         string
	Description  string
	ArgumentHint string
	Aliases      []string
}

// AvailableModelInfo describes one Claude model choice returned by the CLI.
type AvailableModelInfo struct {
	Value                 string
	DisplayName           string
	Description           string
	SupportedEffortLevels []string
	SupportsAutoMode      bool
}

// ContextUsage contains the current Claude context token usage.
type ContextUsage struct {
	TotalTokens int
	MaxTokens   int
}

// SettingsSnapshot contains the subset of Claude settings needed by ACP config options.
type SettingsSnapshot struct {
	Applied  AppliedSettings
	FastMode *bool
}

// AppliedSettings contains runtime-resolved Claude settings.
type AppliedSettings struct {
	Model  string
	Effort string
}

// NewClient creates a Claude client using the provided transport or a process transport.
func NewClient(log *slog.Logger, options Options, transport Transport) *Client {
	if log == nil {
		log = slog.Default()
	}

	if transport == nil {
		transport = NewProcessTransport(log, options)
	}

	return &Client{log: log, options: options, transport: transport}
}

// Start launches Claude, starts the controller, and initializes the control protocol.
// The ctx argument bounds startup and initialization only; after Start succeeds,
// the process lifetime is owned by Close.
func (c *Client) Start(ctx context.Context) error {
	if c.isClosed() {
		return ErrClientClosed
	}

	// Once startup succeeds, Close owns the process lifetime; ctx only bounds
	// Start and initialize.
	runCtx, cancel := context.WithCancel(context.Background())

	spawnStarted := time.Now()
	if err := c.transport.Start(runCtx); err != nil {
		c.observeStartupStage(ctx, "spawn", spawnStarted, err)
		cancel()

		return err
	}

	c.observeStartupStage(ctx, "spawn", spawnStarted, nil)

	controller := NewController(c.log, c.transport)
	controller.SetHandlerTimeout(c.options.ControlHandlerTimeout)
	controller.RegisterHandler("can_use_tool", c.handleCanUseTool)
	controller.RegisterHandler("elicitation", c.handleElicitation)
	controller.RegisterHandler("hook_callback", c.handleHookCallback)
	controller.Start(runCtx)

	c.stateMu.Lock()
	if c.closed {
		c.stateMu.Unlock()
		cancel()

		closeErr := c.closeTransportDebug(ctx, "client closed during start")

		return errors.Join(ErrClientClosed, closeErr)
	}

	c.controller = controller
	c.cancel = cancel
	c.stateMu.Unlock()

	timeout := c.options.InitializeTimeout
	if timeout <= 0 {
		timeout = defaultInitializeTimeout
	}

	readinessStarted := time.Now()
	resp, err := controller.SendRequest(ctx, "initialize", map[string]any{"hooks": c.options.Hooks.toPayload()}, timeout)
	c.observeStartupStage(ctx, "readiness", readinessStarted, err)

	if err != nil {
		cancel()

		closeErr := c.closeTransportDebug(ctx, "initialize failed")
		c.clearController(controller)

		return errors.Join(fmt.Errorf("initialize claude control protocol: %w", err), closeErr)
	}

	c.setInitializeInfo(parseInitializeInfo(resp.Response[keyResponse]))

	return nil
}

func (c *Client) observeStartupStage(ctx context.Context, stage string, started time.Time, err error) {
	if c.options.ObserveStartupStage != nil {
		c.options.ObserveStartupStage(ctx, stage, time.Since(started), err)
	}
}

func (c *Client) closeTransportDebug(ctx context.Context, reason string) error {
	if err := c.transport.Close(); err != nil {
		c.log.DebugContext(ctx, "close Claude transport failed", slog.String("reason", reason), slog.String("error", err.Error()))

		return err
	}

	return nil
}

func (c *Client) activeController() *Controller {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()

	return c.controller
}

func (c *Client) isClosed() bool {
	c.stateMu.RLock()
	defer c.stateMu.RUnlock()

	return c.closed
}

func (c *Client) clearController(controller *Controller) {
	c.stateMu.Lock()
	defer c.stateMu.Unlock()

	if c.controller == controller {
		c.controller = nil
		c.cancel = nil
	}
}

// InitializeInfo returns a copy of the metadata from Claude's initialize response.
func (c *Client) InitializeInfo() InitializeInfo {
	c.infoMu.RLock()
	defer c.infoMu.RUnlock()

	return InitializeInfo{
		Commands:              cloneSlashCommands(c.initializeInfo.Commands),
		Models:                append([]AvailableModelInfo(nil), c.initializeInfo.Models...),
		OutputStyle:           c.initializeInfo.OutputStyle,
		AvailableOutputStyles: append([]string(nil), c.initializeInfo.AvailableOutputStyles...),
	}
}

// RefreshInitializeInfo re-runs Claude's control initialize request and updates
// the cached discovery metadata.
func (c *Client) RefreshInitializeInfo(ctx context.Context) (InitializeInfo, error) {
	controller := c.activeController()
	if controller == nil {
		return InitializeInfo{}, ErrClientNotStarted
	}

	timeout := c.options.InitializeTimeout
	if timeout <= 0 {
		timeout = defaultInitializeTimeout
	}

	resp, err := controller.SendRequest(ctx, "initialize", map[string]any{"hooks": c.options.Hooks.toPayload()}, timeout)
	if err != nil {
		return InitializeInfo{}, fmt.Errorf("refresh claude control initialize: %w", err)
	}

	info := parseInitializeInfo(resp.Response[keyResponse])
	c.setInitializeInfo(info)

	return c.InitializeInfo(), nil
}

func (c *Client) setInitializeInfo(info InitializeInfo) {
	c.infoMu.Lock()
	defer c.infoMu.Unlock()

	c.initializeInfo = InitializeInfo{
		Commands:              cloneSlashCommands(info.Commands),
		Models:                append([]AvailableModelInfo(nil), info.Models...),
		OutputStyle:           info.OutputStyle,
		AvailableOutputStyles: append([]string(nil), info.AvailableOutputStyles...),
	}
}

func cloneSlashCommands(commands []SlashCommand) []SlashCommand {
	cloned := append([]SlashCommand(nil), commands...)
	for i, command := range commands {
		cloned[i].Aliases = append([]string(nil), command.Aliases...)
	}

	return cloned
}

func parseInitializeInfo(value any) InitializeInfo {
	raw, _ := value.(map[string]any)
	if raw == nil {
		return InitializeInfo{}
	}

	return InitializeInfo{
		Commands:              parseSlashCommands(raw["commands"]),
		Models:                parseAvailableModels(raw["models"]),
		OutputStyle:           stringValue(raw["output_style"]),
		AvailableOutputStyles: stringSlice(raw["available_output_styles"]),
	}
}

func parseSlashCommands(value any) []SlashCommand {
	values, _ := value.([]any)
	commands := make([]SlashCommand, 0, len(values))

	for _, value := range values {
		raw, _ := value.(map[string]any)
		if raw == nil {
			continue
		}

		name, _ := raw[keyName].(string)
		if name == "" {
			continue
		}

		description, _ := raw["description"].(string)
		argumentHint, _ := raw["argumentHint"].(string)
		commands = append(commands, SlashCommand{
			Name:         name,
			Description:  description,
			ArgumentHint: argumentHint,
			Aliases:      stringSlice(raw["aliases"]),
		})
	}

	return commands
}

func parseAvailableModels(value any) []AvailableModelInfo {
	values, _ := value.([]any)
	models := make([]AvailableModelInfo, 0, len(values))

	for _, value := range values {
		raw, _ := value.(map[string]any)
		if raw == nil {
			continue
		}

		model, _ := raw["value"].(string)
		if model == "" {
			continue
		}

		displayName, _ := raw["displayName"].(string)
		description, _ := raw["description"].(string)
		models = append(models, AvailableModelInfo{
			Value:                 model,
			DisplayName:           displayName,
			Description:           description,
			SupportedEffortLevels: stringSlice(raw["supportedEffortLevels"]),
			SupportsAutoMode:      boolValue(raw["supportsAutoMode"]),
		})
	}

	return models
}

// Query sends user content to Claude and binds inbound control callbacks to
// turnNonce until EndQuery clears that exact turn.
func (c *Client) Query(ctx context.Context, turnNonce string, content any) error {
	if c.isClosed() {
		return ErrClientClosed
	}

	c.controlTurnMu.Lock()
	c.controlTurnNonce = turnNonce
	c.controlTurnMu.Unlock()

	payload := map[string]any{
		keyType: MessageTypeUser,
		keyMessage: map[string]any{
			"role":     MessageTypeUser,
			keyContent: content,
		},
		keyParentToolID: nil,
		keySessionID:    c.options.SessionID,
	}

	return c.transport.Send(ctx, payload)
}

// EndQuery clears callback routing only when turnNonce still names the bound
// query. An older prompt's deferred cleanup therefore cannot clear a newer
// query's callback route.
func (c *Client) EndQuery(turnNonce string) {
	c.controlTurnMu.Lock()
	defer c.controlTurnMu.Unlock()

	if c.controlTurnNonce == turnNonce {
		c.controlTurnNonce = ""
	}
}

func (c *Client) controlHandlerContext(ctx context.Context) context.Context {
	c.controlTurnMu.RLock()
	turnNonce := c.controlTurnNonce
	c.controlTurnMu.RUnlock()

	if turnNonce == "" || c.options.ControlHandlerContext == nil {
		return ctx
	}

	return c.options.ControlHandlerContext(ctx, turnNonce)
}

// Receive waits for the next parsed Claude message.
func (c *Client) Receive(ctx context.Context) (Message, error) {
	controller := c.activeController()
	if controller == nil {
		return nil, ErrClientNotStarted
	}

	select {
	case raw, ok := <-controller.Messages():
		if !ok {
			// Surface the real transport/process cause (exit status, stderr
			// tail, transport error) rather than a bare stream-closed sentinel.
			if lastErr := controller.LastError(); lastErr != nil {
				return nil, lastErr
			}

			return nil, ErrMessageStreamClosed
		}

		msg, err := ParseMessage(raw)
		if err != nil {
			return nil, err
		}

		return msg, nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

// Alive reports whether the client still has a running controller. It returns
// false once the Claude process died or the client was closed, so callers can
// relaunch the native process lazily on the next turn.
func (c *Client) Alive() bool {
	if c.isClosed() {
		return false
	}

	controller := c.activeController()
	if controller == nil {
		return false
	}

	select {
	case <-controller.Done():
		return false
	default:
		return true
	}
}

// Interrupt asks Claude to stop the active turn.
func (c *Client) Interrupt(ctx context.Context) error {
	controller := c.activeController()
	if controller == nil {
		return ErrClientNotStarted
	}

	_, err := controller.SendRequest(ctx, "interrupt", nil, 5*time.Second)
	if err != nil {
		return fmt.Errorf("interrupt claude: %w", err)
	}

	return nil
}

// SetPermissionMode updates Claude's permission mode if supported by the CLI.
func (c *Client) SetPermissionMode(ctx context.Context, mode string) error {
	controller := c.activeController()
	if controller == nil {
		return ErrClientNotStarted
	}

	_, err := controller.SendRequest(ctx, "set_permission_mode", map[string]any{permissionModeField: mode}, 5*time.Second)
	if err != nil {
		return fmt.Errorf("set permission mode: %w", err)
	}

	return nil
}

// SetModel updates Claude's active model if supported by the CLI.
func (c *Client) SetModel(ctx context.Context, model string) error {
	controller := c.activeController()
	if controller == nil {
		return ErrClientNotStarted
	}

	_, err := controller.SendRequest(ctx, "set_model", map[string]any{keyModel: model}, 5*time.Second)
	if err != nil {
		return fmt.Errorf("set model: %w", err)
	}

	return nil
}

// SetOutputStyle updates Claude's output style if supported by the CLI.
func (c *Client) SetOutputStyle(ctx context.Context, style string) error {
	return c.ApplyFlagSettings(ctx, map[string]any{"outputStyle": style})
}

// SetEffort updates Claude's reasoning effort if supported by the CLI/model.
func (c *Client) SetEffort(ctx context.Context, effort string) error {
	return c.ApplyFlagSettings(ctx, map[string]any{"effort": effort})
}

// SetFastMode updates Claude's fast-mode setting if supported by the CLI/account.
func (c *Client) SetFastMode(ctx context.Context, enabled bool) error {
	return c.ApplyFlagSettings(ctx, map[string]any{keyFastMode: enabled})
}

// ApplyFlagSettings merges settings into Claude's process-local flag settings layer.
func (c *Client) ApplyFlagSettings(ctx context.Context, settings map[string]any) error {
	controller := c.activeController()
	if controller == nil {
		return ErrClientNotStarted
	}

	_, err := controller.SendRequest(ctx, "apply_flag_settings", map[string]any{"settings": settings}, 5*time.Second)
	if err != nil {
		return fmt.Errorf("apply flag settings: %w", err)
	}

	return nil
}

// GetSettings queries Claude for effective process settings.
func (c *Client) GetSettings(ctx context.Context) (*SettingsSnapshot, error) {
	controller := c.activeController()
	if controller == nil {
		return nil, ErrClientNotStarted
	}

	resp, err := controller.SendRequest(ctx, "get_settings", nil, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("get settings: %w", err)
	}

	payload, _ := resp.Response[keyResponse].(map[string]any)

	return parseSettings(payload), nil
}

// GetContextUsage queries Claude for current context usage.
func (c *Client) GetContextUsage(ctx context.Context) (*ContextUsage, error) {
	controller := c.activeController()
	if controller == nil {
		return nil, ErrClientNotStarted
	}

	resp, err := controller.SendRequest(ctx, "get_context_usage", nil, 5*time.Second)
	if err != nil {
		return nil, fmt.Errorf("get context usage: %w", err)
	}

	payload, _ := resp.Response[keyResponse].(map[string]any)

	return parseContextUsage(payload), nil
}

// Close terminates the Claude process.
func (c *Client) Close() error {
	c.closeOnce.Do(func() {
		c.stateMu.Lock()
		c.closed = true
		cancel := c.cancel
		c.stateMu.Unlock()

		if cancel != nil {
			cancel()
		}

		if c.transport != nil {
			c.closeErr = c.transport.Close()
		}
	})

	return c.closeErr
}

func (c *Client) handleCanUseTool(ctx context.Context, req *ControlRequest) (map[string]any, error) {
	ctx = c.controlHandlerContext(ctx)

	input, _ := req.Request["input"].(map[string]any)
	request := PermissionRequest{
		Input: input,
		Raw:   req.Request,
	}
	request.ToolName, _ = req.Request["tool_name"].(string)
	request.ToolUseID, _ = req.Request["tool_use_id"].(string)
	request.Title, _ = req.Request["title"].(string)
	request.Suggestions = mapSliceValue(req.Request["suggestions"])

	if c.options.PermissionHandler == nil {
		return PermissionDecision{
			Behavior: BehaviorDeny,
			Message:  "permission handler is not configured",
		}.toPayload(input), nil
	}

	decision, err := c.options.PermissionHandler(ctx, request)
	if err != nil {
		return nil, fmt.Errorf("permission handler: %w", err)
	}

	return decision.toPayload(input), nil
}

func (c *Client) handleElicitation(ctx context.Context, req *ControlRequest) (map[string]any, error) {
	ctx = c.controlHandlerContext(ctx)

	if c.options.ElicitationHandler == nil {
		return ElicitationResponse{Action: ElicitationActionDecline}.toPayload(), nil
	}

	request := ElicitationRequest{
		Raw:             req.Request,
		RequestedSchema: make(map[string]any),
	}
	request.MCPServerName, _ = req.Request["mcp_server_name"].(string)
	request.Message, _ = req.Request["message"].(string)
	request.Mode, _ = req.Request[elicitationModeField].(string)
	request.URL, _ = req.Request["url"].(string)
	request.ElicitationID, _ = req.Request["elicitation_id"].(string)
	request.ToolUseID, _ = req.Request["tool_use_id"].(string)

	if schema, ok := req.Request["requested_schema"].(map[string]any); ok {
		request.RequestedSchema = schema
	}

	request.Mode = request.requestedMode()

	response, err := c.options.ElicitationHandler(ctx, request)
	if err != nil {
		return nil, err
	}

	return response.toPayload(), nil
}

func (c *Client) handleHookCallback(ctx context.Context, req *ControlRequest) (map[string]any, error) {
	ctx = c.controlHandlerContext(ctx)

	input, _ := req.Request[keyInput].(map[string]any)
	responseMap, _ := input["tool_response"].(map[string]any)

	request := HookRequest{
		EventName:    stringValue(input["hook_event_name"]),
		ToolName:     stringValue(input["tool_name"]),
		ToolUseID:    firstStringValue(req.Request[keyToolUseID], input[keyToolUseID]),
		ToolResponse: responseMap,
	}

	if c.options.HookHandler == nil {
		return HookResponse{Continue: true}.toPayload(), nil
	}

	response, err := c.options.HookHandler(ctx, request)
	if err != nil {
		return nil, err
	}

	return response.toPayload(), nil
}

func firstStringValue(values ...any) string {
	for _, value := range values {
		if typed := stringValue(value); typed != "" {
			return typed
		}
	}

	return ""
}

func mapSliceValue(value any) []map[string]any {
	values, _ := value.([]any)

	result := make([]map[string]any, 0, len(values))
	for _, value := range values {
		raw, _ := value.(map[string]any)
		if raw != nil {
			result = append(result, raw)
		}
	}

	return result
}

func parseContextUsage(raw map[string]any) *ContextUsage {
	if raw == nil {
		return &ContextUsage{}
	}

	return &ContextUsage{
		TotalTokens: intValue(raw["totalTokens"]),
		MaxTokens:   intValue(raw["maxTokens"]),
	}
}

func parseSettings(raw map[string]any) *SettingsSnapshot {
	if raw == nil {
		return &SettingsSnapshot{}
	}

	appliedRaw, _ := raw["applied"].(map[string]any)
	effectiveRaw, _ := raw["effective"].(map[string]any)
	fastMode, _ := effectiveRaw[keyFastMode].(bool)

	if appliedRaw == nil {
		settings := &SettingsSnapshot{}
		if _, ok := effectiveRaw[keyFastMode]; ok {
			settings.FastMode = &fastMode
		}

		return settings
	}

	settings := &SettingsSnapshot{
		Applied: AppliedSettings{
			Model:  stringValue(appliedRaw[keyModel]),
			Effort: stringValue(appliedRaw["effort"]),
		},
	}
	if _, ok := effectiveRaw[keyFastMode]; ok {
		settings.FastMode = &fastMode
	}

	return settings
}
