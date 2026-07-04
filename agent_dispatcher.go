package claudeacp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/coder/acp-go-sdk"
)

type localAgentConnection struct {
	agent       *Agent
	conn        *acp.Connection
	initialized atomic.Bool
}

type localAgentHandler func(context.Context, *Agent, json.RawMessage) (any, *acp.RequestError)

type localAgentParams[Req any] interface {
	*Req
	Validate() error
}

var (
	_ agentClient = (*localAgentConnection)(nil)

	localAgentHandlers = map[string]localAgentHandler{
		acp.AgentMethodAuthenticate:           localResponse((*Agent).Authenticate),
		acp.AgentMethodInitialize:             localResponse((*Agent).Initialize),
		acp.AgentMethodLogout:                 localResponse((*Agent).Logout),
		acp.AgentMethodSessionCancel:          localNotification((*Agent).Cancel),
		acp.AgentMethodSessionClose:           localResponse((*Agent).CloseSession),
		acp.AgentMethodSessionDelete:          localResponse((*Agent).UnstableDeleteSession),
		acp.AgentMethodSessionList:            localResponse((*Agent).ListSessions),
		acp.AgentMethodSessionLoad:            localResponse((*Agent).LoadSession),
		acp.AgentMethodSessionNew:             localResponse((*Agent).NewSession),
		acp.AgentMethodSessionPrompt:          localResponse((*Agent).Prompt),
		acp.AgentMethodSessionResume:          localResponse((*Agent).ResumeSession),
		acp.AgentMethodSessionSetConfigOption: localResponse((*Agent).SetSessionConfigOption),
	}
)

func newLocalAgentConnection(agent *Agent, output io.Writer, input io.Reader) *localAgentConnection {
	conn := &localAgentConnection{agent: agent}
	inputGate := newConnectionInputGate(input)
	conn.conn = acp.NewConnection(conn.handle, output, inputGate)
	conn.conn.SetLogger(agent.log)
	inputGate.open()

	return conn
}

type connectionInputGate struct {
	reader io.Reader
	ready  chan struct{}
	once   sync.Once
}

// connectionInputGate blocks the SDK receive goroutine until the connection
// logger is installed. The SDK starts receiving inside NewConnection.
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

func (c *localAgentConnection) Done() <-chan struct{} {
	return c.conn.Done()
}

func (c *localAgentConnection) handle(ctx context.Context, method string, params json.RawMessage) (any, *acp.RequestError) {
	if method != acp.AgentMethodInitialize && !c.initialized.Load() {
		return nil, acp.NewInvalidRequest(map[string]any{
			jsonFieldMethod: method,
			jsonFieldError:  "initialize must be called before other ACP methods",
		})
	}

	if method != acp.AgentMethodInitialize {
		release, err := c.agent.acquireClientCall(ctx)
		if err != nil {
			return nil, requestError(err)
		}

		defer release()
	}

	if strings.HasPrefix(method, "_") {
		result, err := c.agent.HandleExtensionMethod(ctx, method, params)

		return result, requestError(err)
	}

	handler, ok := localAgentHandlers[method]
	if !ok {
		return nil, acp.NewMethodNotFound(method)
	}

	result, reqErr := handler(ctx, c.agent, params)
	if method == acp.AgentMethodInitialize && reqErr == nil {
		c.initialized.Store(true)
	}

	return result, reqErr
}

func localResponse[Req any, ReqPtr localAgentParams[Req], Resp any](
	call func(*Agent, context.Context, Req) (Resp, error),
) localAgentHandler {
	return func(ctx context.Context, agent *Agent, params json.RawMessage) (any, *acp.RequestError) {
		value, reqErr := decodeLocalAgentParams[Req, ReqPtr](params)
		if reqErr != nil {
			return nil, reqErr
		}

		resp, err := call(agent, ctx, value)
		if err != nil {
			return nil, requestError(err)
		}

		return resp, nil
	}
}

func localNotification[Req any, ReqPtr localAgentParams[Req]](
	call func(*Agent, context.Context, Req) error,
) localAgentHandler {
	return func(ctx context.Context, agent *Agent, params json.RawMessage) (any, *acp.RequestError) {
		value, reqErr := decodeLocalAgentParams[Req, ReqPtr](params)
		if reqErr != nil {
			return nil, reqErr
		}

		if err := call(agent, ctx, value); err != nil {
			return nil, requestError(err)
		}

		return nil, nil
	}
}

func decodeLocalAgentParams[Req any, ReqPtr localAgentParams[Req]](params json.RawMessage) (Req, *acp.RequestError) {
	var value Req
	if err := json.Unmarshal(params, &value); err != nil {
		return value, acp.NewInvalidParams(map[string]any{jsonFieldError: err.Error()})
	}

	if err := ReqPtr(&value).Validate(); err != nil {
		return value, acp.NewInvalidParams(map[string]any{jsonFieldError: err.Error()})
	}

	return value, nil
}

func (c *localAgentConnection) UnstableCompleteElicitation(
	ctx context.Context,
	params acp.UnstableCompleteElicitationNotification,
) error {
	return c.conn.SendNotification(ctx, acp.ClientMethodElicitationComplete, params)
}

func (c *localAgentConnection) UnstableCreateElicitation(
	ctx context.Context,
	params acp.UnstableCreateElicitationRequest,
) (acp.UnstableCreateElicitationResponse, error) {
	return c.CreateElicitation(ctx, params, elicitationScope{})
}

func (c *localAgentConnection) CreateElicitation(
	ctx context.Context,
	params acp.UnstableCreateElicitationRequest,
	scope elicitationScope,
) (acp.UnstableCreateElicitationResponse, error) {
	raw, err := scopedElicitationParams(params, scope)
	if err != nil {
		return acp.UnstableCreateElicitationResponse{}, err
	}

	return acp.SendRequest[acp.UnstableCreateElicitationResponse](c.conn, ctx, acp.ClientMethodElicitationCreate, raw)
}

func (c *localAgentConnection) ReadTextFile(
	ctx context.Context,
	params acp.ReadTextFileRequest,
) (acp.ReadTextFileResponse, error) {
	return acp.SendRequest[acp.ReadTextFileResponse](c.conn, ctx, acp.ClientMethodFsReadTextFile, params)
}

func (c *localAgentConnection) WriteTextFile(
	ctx context.Context,
	params acp.WriteTextFileRequest,
) (acp.WriteTextFileResponse, error) {
	return acp.SendRequest[acp.WriteTextFileResponse](c.conn, ctx, acp.ClientMethodFsWriteTextFile, params)
}

func (c *localAgentConnection) RequestPermission(
	ctx context.Context,
	params acp.RequestPermissionRequest,
) (acp.RequestPermissionResponse, error) {
	return acp.SendRequest[acp.RequestPermissionResponse](c.conn, ctx, acp.ClientMethodSessionRequestPermission, params)
}

func (c *localAgentConnection) SessionUpdate(ctx context.Context, params acp.SessionNotification) error {
	return c.conn.SendNotification(ctx, acp.ClientMethodSessionUpdate, params)
}

func (c *localAgentConnection) CreateTerminal(
	ctx context.Context,
	params acp.CreateTerminalRequest,
) (acp.CreateTerminalResponse, error) {
	return acp.SendRequest[acp.CreateTerminalResponse](c.conn, ctx, acp.ClientMethodTerminalCreate, params)
}

func (c *localAgentConnection) KillTerminal(
	ctx context.Context,
	params acp.KillTerminalRequest,
) (acp.KillTerminalResponse, error) {
	return acp.SendRequest[acp.KillTerminalResponse](c.conn, ctx, acp.ClientMethodTerminalKill, params)
}

func (c *localAgentConnection) TerminalOutput(
	ctx context.Context,
	params acp.TerminalOutputRequest,
) (acp.TerminalOutputResponse, error) {
	return acp.SendRequest[acp.TerminalOutputResponse](c.conn, ctx, acp.ClientMethodTerminalOutput, params)
}

func (c *localAgentConnection) ReleaseTerminal(
	ctx context.Context,
	params acp.ReleaseTerminalRequest,
) (acp.ReleaseTerminalResponse, error) {
	return acp.SendRequest[acp.ReleaseTerminalResponse](c.conn, ctx, acp.ClientMethodTerminalRelease, params)
}

func (c *localAgentConnection) WaitForTerminalExit(
	ctx context.Context,
	params acp.WaitForTerminalExitRequest,
) (acp.WaitForTerminalExitResponse, error) {
	return acp.SendRequest[acp.WaitForTerminalExitResponse](c.conn, ctx, acp.ClientMethodTerminalWaitForExit, params)
}

func (c *localAgentConnection) NotifyExtension(ctx context.Context, method string, params any) error {
	if method == "" || !strings.HasPrefix(method, "_") {
		return fmt.Errorf("extension method name must start with '_' (got %q)", method)
	}

	return c.conn.SendNotification(ctx, method, params)
}
