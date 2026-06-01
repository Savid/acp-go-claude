package claudeacp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"sync"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/savid/acp-go-claude/internal/observer"
)

const (
	modeDefault           acp.SessionModeId = "default"
	modePlan              acp.SessionModeId = "plan"
	modeAcceptEdits       acp.SessionModeId = "accept_edits"
	modeBypassPermissions acp.SessionModeId = "bypass_permissions"
	modeAuto              acp.SessionModeId = "auto"
	modeDontAsk           acp.SessionModeId = "dont_ask"

	modeNameDefault = "Default"
	modeNameAuto    = "Auto"
	modeNameDontAsk = "Don't Ask"

	permissionModeAcceptEdits       = "acceptEdits"
	permissionModeBypassPermissions = "bypassPermissions"
	permissionModeDontAsk           = "dontAsk"

	configModel       acp.SessionConfigId = "model"
	configMode        acp.SessionConfigId = "mode"
	configOutputStyle acp.SessionConfigId = "output_style"
	configEffort      acp.SessionConfigId = "effort"
	configFastMode    acp.SessionConfigId = "fast_mode"

	configTypeSelect  = "select"
	configTypeBoolean = "boolean"
	effortLow         = "low"
	effortMedium      = "medium"
	effortHigh        = "high"
	effortXHigh       = "xhigh"
	effortMax         = "max"

	providerClaudeCode       = "claude-code"
	providerClaudeCodeTitle  = "Claude Code"
	providerClaudeCodeURL    = "claude://local"
	clientMetaTerminalOutput = "terminal_output"
	permissionPromptTool     = "stdio"
	validationRequired       = "required"
	capabilityRawEventsKey   = "rawEvents"

	mcpConfigTypeACP = "acp"

	listSessionsPageSize = 50
)

var osGeteuid = os.Geteuid
var newServeAgent = NewAgent

// Agent exposes Claude Code through ACP.
type Agent struct {
	options Options
	log     *slog.Logger
	observe *observer.Observer

	// Lock order: acquire mu before docsMu when both are needed. Do not call
	// session or bridge close methods while holding either lock.
	mu                 sync.Mutex
	closed             bool
	conn               agentClient
	sessions           map[acp.SessionId]*Session
	nesSessions        map[acp.SessionId]*nesSession
	docsMu             sync.Mutex
	documents          map[acp.SessionId]map[string]documentState
	focusedDocuments   map[acp.SessionId]string
	clientCapabilities acp.ClientCapabilities
	positionEncoding   acp.PositionEncodingKind
	mcpConnections     map[acp.UnstableMcpConnectionId]*mcpBridgeConn
	gatewayAuth        *gatewayAuth
	permissionCache    map[acp.SessionId]map[string]string
	importStore        *InMemorySessionStore
	imports            map[string]*sessionImport

	newClaudeClient func(*slog.Logger, claude.Options) *claude.Client
}

var (
	_ acp.Agent                  = (*Agent)(nil)
	_ acp.AgentLoader            = (*Agent)(nil)
	_ acp.AgentExperimental      = (*Agent)(nil)
	_ acp.ExtensionMethodHandler = (*Agent)(nil)
)

// NewAgent creates an ACP agent for Claude Code.
func NewAgent(opts ...Option) *Agent {
	options := applyOptions(opts)

	log := options.Logger
	if log == nil {
		log = slog.Default()
	}

	return &Agent{
		options:          options,
		log:              log,
		observe:          observer.New(observer.Config{MeterProvider: options.MeterProvider, Propagator: options.TextMapPropagator, TracerProvider: options.TracerProvider, Version: options.AgentVersion}),
		sessions:         make(map[acp.SessionId]*Session),
		nesSessions:      make(map[acp.SessionId]*nesSession),
		documents:        make(map[acp.SessionId]map[string]documentState),
		focusedDocuments: make(map[acp.SessionId]string),
		positionEncoding: acp.PositionEncodingKindUtf16,
		mcpConnections:   make(map[acp.UnstableMcpConnectionId]*mcpBridgeConn),
		permissionCache:  make(map[acp.SessionId]map[string]string),
		importStore:      NewInMemorySessionStore(),
		imports:          make(map[string]*sessionImport),
		newClaudeClient: func(log *slog.Logger, options claude.Options) *claude.Client {
			return claude.NewClient(log, options, nil)
		},
	}
}

// Serve runs an ACP agent over the provided streams.
func Serve(ctx context.Context, input io.Reader, output io.Writer, opts ...Option) error {
	agent := newServeAgent(opts...)
	defer func() {
		if err := agent.Close(); err != nil {
			agent.log.DebugContext(context.Background(), "close Claude ACP agent failed", slog.String(jsonFieldError, err.Error()))
		}
	}()

	conn := newLocalAgentConnection(agent, output, input)
	agent.setConnection(conn)

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-conn.Done():
		return nil
	}
}

// Close cancels and closes all resources owned by the agent.
func (a *Agent) Close() error {
	a.mu.Lock()

	sessions := make([]*Session, 0, len(a.sessions))
	for _, session := range a.sessions {
		sessions = append(sessions, session)
	}

	nesSessions := make([]*nesSession, 0, len(a.nesSessions))
	for _, session := range a.nesSessions {
		nesSessions = append(nesSessions, session)
	}

	a.sessions = make(map[acp.SessionId]*Session)
	a.nesSessions = make(map[acp.SessionId]*nesSession)
	a.mcpConnections = make(map[acp.UnstableMcpConnectionId]*mcpBridgeConn)
	a.imports = make(map[string]*sessionImport)
	a.permissionCache = make(map[acp.SessionId]map[string]string)
	a.conn = nil
	a.closed = true
	a.mu.Unlock()

	if len(sessions) > 0 {
		a.observe.AddActiveSession(context.Background(), -int64(len(sessions)))
	}

	a.docsMu.Lock()
	a.documents = make(map[acp.SessionId]map[string]documentState)
	a.focusedDocuments = make(map[acp.SessionId]string)
	a.docsMu.Unlock()

	var closeErrs []error

	for _, session := range sessions {
		if err := session.Close(context.Background()); err != nil {
			closeErrs = append(closeErrs, err)
		}
	}

	for _, session := range nesSessions {
		session.close()
	}

	return errors.Join(closeErrs...)
}

func (a *Agent) setConnection(conn agentClient) {
	a.mu.Lock()
	defer a.mu.Unlock()

	a.conn = conn
}

// Initialize implements ACP initialize.
func (a *Agent) Initialize(ctx context.Context, params acp.InitializeRequest) (resp acp.InitializeResponse, err error) {
	_, finish := a.observe.StartACP(ctx, params.Meta, "initialize")
	defer func() { finish(observer.ACPResult{Err: err}) }()

	title := a.options.AgentTitle
	positionEncoding := selectPositionEncoding(params.ClientCapabilities.PositionEncodings)

	a.mu.Lock()
	a.clientCapabilities = params.ClientCapabilities
	a.positionEncoding = positionEncoding
	a.mu.Unlock()

	resp = acp.InitializeResponse{
		ProtocolVersion: acp.ProtocolVersionNumber,
		AgentInfo: &acp.Implementation{
			Name:    a.options.AgentName,
			Title:   &title,
			Version: a.options.AgentVersion,
		},
		AuthMethods: a.authMethods(params),
		AgentCapabilities: acp.AgentCapabilities{
			Meta: map[string]any{
				claudeMetaKey: map[string]any{
					"promptQueueing": map[string]any{
						capabilityScopeKey: capabilityScopeSession,
						"sameSession":      true,
					},
					"sessionImport": map[string]any{
						capabilityScopeKey: capabilityScopeSession,
						jsonFieldFormat:    claudeSessionImportFormat,
						"methods": map[string]string{
							"import":       claudeSessionImportMethod,
							"importChunk":  claudeSessionImportChunkMethod,
							"commitImport": claudeSessionCommitImportMethod,
							"abortImport":  claudeSessionAbortImportMethod,
						},
					},
					rawSDKMessagesCapabilityKey: map[string]any{
						capabilityScopeKey:         capabilityScopeSession,
						rawSDKMessagesMethodKey:    rawClaudeSDKMessageMethod,
						rawSDKMessagesEnabledByKey: rawSDKMessagesEnabledByPath,
					},
					outputFormatCapabilityKey: map[string]any{
						capabilityScopeKey:     capabilityScopeSession,
						"types":                []string{ClaudeOutputFormatJSONSchema},
						"config":               outputFormatConfigPath,
						outputFormatResultKey:  outputFormatResultPath,
						"hiddenTool":           "StructuredOutput",
						capabilityRawEventsKey: rawClaudeSDKMessageMethod,
					},
					claudeGoalsCapabilityKey: map[string]any{
						capabilityScopeKey:     capabilityScopeSession,
						goalCapabilityStateKey: "session_info_update._meta.claude.goal",
						"initialState": map[string]any{
							"sessionResponses": []string{
								"session/new.result._meta.claude.goal",
								"session/load.result._meta.claude.goal",
								"session/resume.result._meta.claude.goal",
							},
							"listSummary": "session/list.result.sessions[]._meta.claude.goal",
						},
						"setMethod":              claudeSessionSetGoalMethod,
						"semantics":              "full-snapshot",
						"maxObjectiveBytes":      maxGoalObjectiveBytes,
						"maxSummaryRunes":        maxGoalSummaryRunes,
						"statuses":               []string{ClaudeGoalStatusActive, ClaudeGoalStatusCompleted, ClaudeGoalStatusBlocked},
						"clientSettableStatuses": []string{ClaudeGoalStatusActive, ClaudeGoalStatusBlocked},
						"clearValue":             nil,
					},
					"workflows": map[string]any{
						"updates":          true,
						capabilityScopeKey: capabilityScopeSession,
						"toolKind":         string(acp.ToolKindThink),
						"metadataPath":     "tool_call_update._meta.claude.workflow",
						"logs": map[string]any{
							"readByDefault": false,
						},
						capabilityRawEventsKey: rawClaudeSDKMessageMethod,
					},
				},
			},
			LoadSession: true,
			McpCapabilities: acp.McpCapabilities{
				Acp:  true,
				Http: true,
				Sse:  true,
			},
			Nes:              nesCapabilities(),
			Providers:        &acp.ProvidersCapabilities{},
			PositionEncoding: &positionEncoding,
			PromptCapabilities: acp.PromptCapabilities{
				EmbeddedContext: true,
				Image:           true,
			},
			SessionCapabilities: acp.SessionCapabilities{
				Close:                 &acp.SessionCloseCapabilities{},
				Fork:                  &acp.SessionForkCapabilities{},
				List:                  &acp.SessionListCapabilities{},
				Resume:                &acp.SessionResumeCapabilities{},
				AdditionalDirectories: &acp.SessionAdditionalDirectoriesCapabilities{},
			},
		},
	}

	return resp, nil
}

// Authenticate stores auth state for agent-handled methods.
func (a *Agent) Authenticate(ctx context.Context, params acp.AuthenticateRequest) (resp acp.AuthenticateResponse, err error) {
	_, finish := a.observe.StartACP(ctx, params.Meta, "authenticate")
	defer func() { finish(observer.ACPResult{Err: err}) }()

	if params.MethodId == authMethodGateway {
		auth, err := parseGatewayAuthMeta(params.Meta)
		if err != nil {
			return acp.AuthenticateResponse{}, acp.NewInvalidParams(map[string]any{jsonFieldError: err.Error()})
		}

		a.setGatewayAuth(auth)

		return acp.AuthenticateResponse{}, nil
	}

	return acp.AuthenticateResponse{}, acp.NewInvalidParams(map[string]any{"methodId": params.MethodId})
}

// HandleExtensionMethod handles Claude-specific ACP extension methods.
func (a *Agent) HandleExtensionMethod(ctx context.Context, method string, params json.RawMessage) (any, error) {
	switch method {
	case claudeSessionImportMethod:
		return a.importClaudeSession(ctx, params)
	case claudeSessionImportChunkMethod:
		return a.importClaudeSessionChunk(ctx, params)
	case claudeSessionCommitImportMethod:
		return a.commitClaudeSessionImport(ctx, params)
	case claudeSessionAbortImportMethod:
		return a.abortClaudeSessionImport(ctx, params)
	case claudeSessionSetGoalMethod:
		return a.setClaudeGoal(ctx, params)
	default:
		return nil, acp.NewMethodNotFound(method)
	}
}

type sessionStart struct {
	Cwd                   string
	AdditionalDirectories []string
	McpServers            []acp.McpServer
	ResumeID              string
	ForkSession           bool
	PermissionRules       map[string]string
	MetaOptions           ClaudeOptions
	RawMessages           rawMessageConfig
}

type initialModelSelection struct {
	Model       string
	ShouldApply bool
}
