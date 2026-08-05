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
	ForkSessionMethod = "_claude/session/fork"
	RawEventMethod    = "_claude/rawEvent"
	RateLimitsMethod  = "_claude/rateLimits"
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

	configTypeSelect = "select"
	effortLow        = "low"
	effortMedium     = "medium"
	effortHigh       = "high"
	effortXHigh      = "xhigh"
	effortMax        = "max"

	clientMetaTerminalOutput = "terminal_output"
	permissionPromptTool     = "stdio"
	validationRequired       = "required"
	validationUnsupported    = "unsupported"
	validationDuplicate      = "duplicate"

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
	sessions           map[acp.SessionId]*agentSession
	store              SessionStore
	deleted            map[acp.SessionId]struct{}
	clientCalls        chan struct{}
	clientCapabilities acp.ClientCapabilities
	positionEncoding   acp.PositionEncodingKind
	permissionCache    map[acp.SessionId]map[string]string
	activeLimitErr     error
	configurationErr   error
	containmentMode    RuntimeContainmentMode
	containmentErr     error
	constructions      sync.WaitGroup
	closeOnce          sync.Once
	closeErr           error

	rateLimitsCacheMu   sync.Mutex
	rateLimitsCache     rateLimitsCacheEntry
	descendantProcesses *runtimeProcessSnapshotTracker
	providerAuth        *providerAuth

	newClaudeClient    func(*slog.Logger, claude.Options) *claude.Client
	queryRateLimits    func(context.Context, claude.Options) (claude.RateLimits, error)
	queryRateLimitsAPI func(context.Context, claude.RateLimitsProbe) (claude.RateLimits, error)
}

var (
	_ acp.Agent                  = (*Agent)(nil)
	_ acp.AgentLoader            = (*Agent)(nil)
	_ acp.ExtensionMethodHandler = (*Agent)(nil)
)

// NewAgent creates an ACP agent for Claude Code.
func NewAgent(opts ...Option) *Agent {
	options := applyOptions(opts)
	homeErr := normalizeStandaloneHome(&options)

	log := options.Logger
	if log == nil {
		log = slog.Default()
	}

	observe := observer.New(observer.Config{MeterProvider: options.MeterProvider, Propagator: options.TextMapPropagator, TracerProvider: options.TracerProvider, Version: options.AgentVersion})
	options.RuntimeResourceHooks = instrumentRuntimeResourceHooks(options.RuntimeResourceHooks, observe)

	mode := containmentMode(options)
	if options.RuntimeResourceHooks.ObserveContainment != nil {
		options.RuntimeResourceHooks.ObserveContainment(context.Background(), mode)
	}

	if mode == RuntimeContainmentBestEffort {
		log.Warn("Darwin best-effort process containment is enabled; escaped descendants may survive, numeric PGID reuse can cause collateral signalling, marker correlation is not ownership, markers can be scrubbed, and native-root permits do not bound escaped provider work",
			slog.String("containment", string(mode)),
		)
	}

	agent := &Agent{
		options:          options,
		log:              log,
		observe:          observe,
		sessions:         make(map[acp.SessionId]*agentSession),
		store:            NewInMemorySessionStore(),
		deleted:          make(map[acp.SessionId]struct{}),
		positionEncoding: acp.PositionEncodingKindUtf16,
		permissionCache:  make(map[acp.SessionId]map[string]string),
		activeLimitErr:   validateConcurrencyLimits(options.ConcurrencyLimits),
		configurationErr: errors.Join(
			homeErr,
			validateContainmentOptions(options),
			validateImageLimits(options.ImageLimits),
			validateInputHandoffRoot(options.InputHandoffRoot),
			validateProviderAuthRoot(options),
			validateProviderAuthDirectHome(options.ProviderAuthDirectHome),
		),
		containmentMode:     mode,
		descendantProcesses: newRuntimeProcessSnapshotTracker(options.RuntimeResourceHooks, mode == RuntimeContainmentAuthoritative),
		newClaudeClient: func(log *slog.Logger, options claude.Options) *claude.Client {
			return claude.NewClient(log, options, nil)
		},
		queryRateLimits:    claude.QueryRateLimits,
		queryRateLimitsAPI: claude.QueryRateLimitsAPI,
	}

	// A configured root is validated before it is advertised: a leg that cannot
	// record what it does must not be offered.
	if agent.configurationErr == nil {
		agent.providerAuth = newProviderAuth(agent)
	}

	return agent
}

// Serve runs an ACP agent over the provided streams.
func Serve(ctx context.Context, input io.Reader, output io.Writer, opts ...Option) (serveErr error) {
	if err := ctx.Err(); err != nil {
		return err
	}

	agent := newServeAgent(opts...)
	defer func() {
		if closeErr := agent.Close(); closeErr != nil {
			agent.log.DebugContext(context.Background(), "close Claude ACP agent failed", slog.String(jsonFieldError, closeErr.Error()))
			serveErr = closeErr
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
	a.closeOnce.Do(func() {
		a.closeErr = a.close()
	})

	return a.closeErr
}

func (a *Agent) close() error {
	a.mu.Lock()
	a.closed = true
	a.mu.Unlock()

	a.constructions.Wait()

	a.mu.Lock()

	sessions := make([]*agentSession, 0, len(a.sessions))
	for _, session := range a.sessions {
		sessions = append(sessions, session)
	}

	a.sessions = make(map[acp.SessionId]*agentSession)
	a.permissionCache = make(map[acp.SessionId]map[string]string)
	a.deleted = make(map[acp.SessionId]struct{})
	a.conn = nil
	a.mu.Unlock()

	if len(sessions) > 0 {
		a.observe.AddActiveSession(context.Background(), -int64(len(sessions)))
	}

	var closeErrs []error

	for _, session := range sessions {
		if err := session.Close(context.Background()); err != nil {
			closeErrs = append(closeErrs, err)
		}
	}

	a.mu.Lock()
	containmentErr := a.containmentErr
	a.mu.Unlock()

	return errors.Join(errors.Join(closeErrs...), containmentErr)
}

func (a *Agent) beginSessionConstruction() error {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.closed {
		return errAgentClosed
	}

	a.constructions.Add(1)

	return nil
}

func (a *Agent) endSessionConstruction() {
	a.constructions.Done()
}

func (a *Agent) recordContainmentError(err error) {
	if !errors.Is(err, claude.ErrProcessContainmentIncomplete) {
		return
	}

	a.mu.Lock()
	if a.containmentErr == nil {
		a.containmentErr = err
	}
	a.mu.Unlock()
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

	if err := a.configurationError(); err != nil {
		return acp.InitializeResponse{}, err
	}

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
		AuthMethods: []acp.AuthMethod{},
		AgentCapabilities: acp.AgentCapabilities{
			Meta:        a.capabilityMeta(),
			LoadSession: true,
			McpCapabilities: acp.McpCapabilities{
				Http: true,
			},
			PositionEncoding: &positionEncoding,
			PromptCapabilities: acp.PromptCapabilities{
				EmbeddedContext: true,
				Image:           true,
			},
			SessionCapabilities: acp.SessionCapabilities{
				Close:                 &acp.SessionCloseCapabilities{},
				Delete:                &acp.SessionDeleteCapabilities{},
				List:                  &acp.SessionListCapabilities{},
				Resume:                &acp.SessionResumeCapabilities{},
				AdditionalDirectories: &acp.SessionAdditionalDirectoriesCapabilities{},
			},
		},
	}

	return resp, nil
}

// capabilityMeta builds the advertised capability metadata: the reserved
// family literals beside the Claude namespace descriptors. The handoff literal
// is present only when a handoff read root is configured, so its absence tells
// a host its option never reached this adapter.
func (a *Agent) capabilityMeta() map[string]any {
	meta := map[string]any{
		routeMetaKey:         map[string]any{metaVersionsKey: []int{routeVersion}},
		mediaEnvelopeMetaKey: mediaEnvelope(a.options.ImageLimits),
		claudeMetaKey: map[string]any{
			"fork": map[string]any{
				"unstable":        true,
				jsonFieldMethod:   ForkSessionMethod,
				"request":         "acp.UnstableForkSessionRequest JSON payload only",
				jsonFieldResponse: "acp.UnstableForkSessionResponse JSON payload only",
			},
			"elicitation": map[string]any{
				"unstable": true,
				"scope":    string(RuntimeResourceSession),
				"tracks":   "in-progress ACP elicitation RFD",
			},
			"rawEvent": map[string]any{
				jsonFieldMethod:  RawEventMethod,
				"enabledBy":      "_meta.claude.rawEvent.enabled",
				"maxBytes":       rawEventMaxBytes,
				"defaultEnabled": false,
			},
			"sessionStore": map[string]any{
				"format":     SessionStoreFormat,
				jsonFieldKey: []string{acpFieldSessionID, "subpath"},
			},
			"structuredOutput": map[string]any{
				acpFieldConfig:  "_meta.claude.options.outputSchema",
				jsonFieldResult: "_meta.claude.structuredOutput",
				"schema":        "json_schema",
			},
		},
	}

	if a.options.InputHandoffRoot != "" {
		meta[handoffMetaKey] = map[string]any{metaVersionsKey: []int{handoffVersion}}
	}

	// The methods array is the host's only discovery surface for which legs
	// exist, so it is present only when the surface is, and it lists exactly
	// the enabled names. There is no injection key: Claude brokers no
	// credential back out and accepts none in.
	if a.providerAuth != nil {
		vendor, _ := meta[claudeMetaKey].(map[string]any)
		vendor[providerAuthCapabilityKey] = a.providerAuth.capability()
	}

	return meta
}

// Authenticate rejects agent-handled auth methods because Claude owns auth.
func (a *Agent) Authenticate(ctx context.Context, params acp.AuthenticateRequest) (resp acp.AuthenticateResponse, err error) {
	_, finish := a.observe.StartACP(ctx, params.Meta, "authenticate")
	defer func() { finish(observer.ACPResult{Err: err}) }()

	return acp.AuthenticateResponse{}, acp.NewInvalidParams(map[string]any{"methodId": params.MethodId})
}

// HandleExtensionMethod handles Claude-specific ACP extension methods. A
// closed agent rejects every extension call up front (-32600), before method
// dispatch and before any parameter validation.
func (a *Agent) HandleExtensionMethod(ctx context.Context, method string, params json.RawMessage) (any, error) {
	if err := a.ensureOpen(); err != nil {
		return nil, err
	}

	switch method {
	case ForkSessionMethod:
		return a.handleForkSession(ctx, params)
	case RateLimitsMethod:
		return a.handleRateLimits(ctx, params)
	default:
	}

	// An unadvertised provider-auth leg is not handled here and falls through
	// to the uniform method-not-found, exactly as an unknown method does.
	if result, handled, err := a.handleAuthExtensionMethod(ctx, method, params); handled {
		return result, err
	}

	return nil, acp.NewMethodNotFound(method)
}

type sessionStart struct {
	Cwd                   string
	AdditionalDirectories []string
	McpServers            []acp.McpServer
	ResumeID              string
	StoreEntries          []SessionStoreEntry
	ActiveSessionResume   bool
	ForkSession           bool
	PermissionRules       map[string]string
	MetaOptions           ClaudeOptions
	RawMessages           rawMessageConfig
}

type initialModelSelection struct {
	Model       string
	ShouldApply bool
}
