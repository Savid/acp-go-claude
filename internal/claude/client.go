package claude

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strconv"
	"sync"
)

type CommandError struct{ Message string }

func (e *CommandError) Error() string { return e.Message }

type Client struct {
	input   io.Writer
	output  io.Reader
	writeMu sync.Mutex
	mu      sync.Mutex
	next    uint64
	pending map[string]chan controlResponse
	events  chan Event
	done    chan struct{}
	err     error
}

type controlResponse struct {
	Subtype   string          `json:"subtype"`
	RequestID string          `json:"request_id"` //nolint:tagliatelle // Claude uses this native wire spelling.
	Response  json.RawMessage `json:"response"`
	Error     string          `json:"error"`
}

func NewClient(input io.Writer, output io.Reader) *Client {
	return &Client{input: input, output: output, pending: make(map[string]chan controlResponse), events: make(chan Event), done: make(chan struct{})}
}

func (c *Client) Start(ctx context.Context) { go c.read(ctx) }
func (c *Client) Events() <-chan Event      { return c.events }
func (c *Client) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.err
}

func (c *Client) read(ctx context.Context) {
	defer close(c.done)
	defer close(c.events)

	scanner := bufio.NewScanner(c.output)
	scanner.Buffer(make([]byte, 64<<10), 16<<20)

	for scanner.Scan() {
		raw := append(json.RawMessage(nil), scanner.Bytes()...)

		var header struct {
			Type     string          `json:"type"`
			Response controlResponse `json:"response"`
		}
		if err := json.Unmarshal(raw, &header); err != nil {
			c.fail(fmt.Errorf("decode native frame: %w", err))

			return
		}

		if header.Type == typeControlResponse {
			c.mu.Lock()
			pending := c.pending[header.Response.RequestID]
			c.mu.Unlock()

			if pending != nil {
				select {
				case pending <- header.Response:
				default:
				}
			}

			continue
		}

		var event Event
		if err := json.Unmarshal(raw, &event); err != nil {
			c.fail(fmt.Errorf("decode native event: %w", err))

			return
		}

		event.Raw = raw
		select {
		case c.events <- event:
		case <-ctx.Done():
			c.fail(ctx.Err())

			return
		}
	}

	c.fail(scanner.Err())
}
func (c *Client) fail(err error) { c.mu.Lock(); defer c.mu.Unlock(); c.err = err }

func (c *Client) write(ctx context.Context, value any) error {
	if err := ctx.Err(); err != nil {
		return err
	}

	c.writeMu.Lock()
	defer c.writeMu.Unlock()

	return json.NewEncoder(c.input).Encode(value)
}

func (c *Client) Control(ctx context.Context, request map[string]any, result any) error {
	c.mu.Lock()
	c.next++
	id := strconv.FormatUint(c.next, 10)
	reply := make(chan controlResponse, 1)
	c.pending[id] = reply

	c.mu.Unlock()
	defer func() { c.mu.Lock(); delete(c.pending, id); c.mu.Unlock() }()

	if err := c.write(ctx, map[string]any{keyType: "control_request", keyRequestID: id, "request": request}); err != nil {
		return err
	}

	select {
	case response := <-reply:
		if response.Subtype == responseError {
			return &CommandError{Message: response.Error}
		}

		if result == nil {
			return nil
		}

		return json.Unmarshal(response.Response, result)
	case <-ctx.Done():
		return ctx.Err()
	case <-c.done:
		if err := c.Err(); err != nil {
			return err
		}

		return io.EOF
	}
}

func (c *Client) Initialize(ctx context.Context) (InitializeResponse, error) {
	var response InitializeResponse

	err := c.Control(ctx, map[string]any{keySubtype: "initialize"}, &response)

	return response, err
}
func (c *Client) Prompt(ctx context.Context, id string, content []ContentBlock) error {
	return c.write(ctx, map[string]any{keyType: roleUser, "session_id": id, "message": map[string]any{"role": roleUser, "content": content}, "parent_tool_use_id": nil})
}
func (c *Client) Abort(ctx context.Context) error {
	return c.Control(ctx, map[string]any{keySubtype: "interrupt"}, nil)
}
func (c *Client) SetModel(ctx context.Context, model string) error {
	return c.Control(ctx, map[string]any{keySubtype: "set_model", "model": model}, nil)
}
func (c *Client) SetPermissionMode(ctx context.Context, mode string) error {
	return c.Control(ctx, map[string]any{keySubtype: "set_permission_mode", "mode": mode}, nil)
}
func (c *Client) ApplySettings(ctx context.Context, settings map[string]any) error {
	return c.Control(ctx, map[string]any{keySubtype: "apply_flag_settings", "settings": settings}, nil)
}
func (c *Client) ContextUsage(ctx context.Context) (ContextUsage, error) {
	var result ContextUsage

	err := c.Control(ctx, map[string]any{keySubtype: "get_context_usage"}, &result)

	return result, err
}
func (c *Client) Reply(ctx context.Context, id string, result any, err error) error {
	response := map[string]any{keyRequestID: id, keySubtype: "success", keyResponse: result}
	if err != nil {
		response = map[string]any{keyRequestID: id, keySubtype: responseError, responseError: "control request failed"}
	}

	return c.write(ctx, map[string]any{keyType: typeControlResponse, keyResponse: response})
}

const (
	roleUser            = "user"
	keyRequestID        = "request_id"
	keyResponse         = "response"
	responseError       = "error"
	keySubtype          = "subtype"
	typeControlResponse = "control_response"
)

const keyType = "type"

type Settings struct {
	Effective struct {
		Model       string `json:"model"`
		EffortLevel string `json:"effortLevel"`
		OutputStyle string `json:"outputStyle"`
	} `json:"effective"`
}

func (c *Client) Settings(ctx context.Context) (Settings, error) {
	var settings Settings

	err := c.Control(ctx, map[string]any{keySubtype: "get_settings"}, &settings)

	return settings, err
}
