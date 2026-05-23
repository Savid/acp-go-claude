package claudeacp

import (
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// Option configures the Claude ACP agent.
type Option func(*Options)

// SettingSource selects one Claude Code filesystem settings source.
type SettingSource string

const (
	// SettingSourceUser loads user-level Claude settings and user Claude Code features.
	SettingSourceUser SettingSource = "user"
	// SettingSourceProject loads project-level Claude settings and Claude Code features from the session cwd.
	SettingSourceProject SettingSource = "project"
	// SettingSourceLocal loads local project Claude settings and local Claude Code features from the session cwd.
	SettingSourceLocal SettingSource = "local"
)

// Options configures the ACP agent process and the Claude CLI sessions it starts.
type Options struct {
	// AgentName is the protocol identifier advertised during ACP initialize.
	AgentName string
	// AgentTitle is the human-readable agent name advertised during ACP initialize.
	AgentTitle string
	// AgentVersion is the agent version advertised during ACP initialize.
	AgentVersion string

	// ClaudePath is the Claude CLI executable path. If empty, PATH is searched.
	ClaudePath string
	// ClaudeHome sets CLAUDE_CONFIG_DIR for launched Claude CLI sessions.
	ClaudeHome string

	// MCPProxyCommand is the executable used for ACP-transport MCP stdio shims.
	MCPProxyCommand string
	// MCPProxyArgs are prepended before the generated mcp-proxy arguments.
	MCPProxyArgs []string

	// Logger receives structured diagnostic logs. If nil, the default logger is used.
	Logger *slog.Logger
	// MeterProvider records adapter metrics. If nil, metrics are no-ops.
	MeterProvider metric.MeterProvider
	// TextMapPropagator extracts ACP _meta trace context and injects Claude launch env.
	// If nil, W3C trace context plus baggage propagation is used.
	TextMapPropagator propagation.TextMapPropagator
	// TracerProvider records adapter spans. If nil, tracing is a no-op.
	TracerProvider trace.TracerProvider

	// SessionStore mirrors Claude transcript writes and backs imported remote
	// sessions. If nil, imported sessions are kept in an in-memory store for
	// this agent process only and ordinary sessions are not mirrored.
	SessionStore SessionStore
	// SessionStoreLoadTimeout bounds store load/list operations used for resume.
	SessionStoreLoadTimeout time.Duration

	// DefaultModel is passed to newly created Claude sessions when non-empty.
	DefaultModel string
	// DefaultPermissionMode is the initial Claude permission mode.
	DefaultPermissionMode string
	// DefaultSystemPrompt is passed to newly created Claude sessions when non-empty.
	DefaultSystemPrompt string
	// HideClaudeAuth suppresses Claude subscription terminal auth methods.
	HideClaudeAuth bool
	// BareMode launches Claude with --bare for deterministic sessions that opt
	// out of Claude's automatic project/context discovery. Bare mode also
	// requires explicit API-key or apiKeyHelper auth.
	BareMode bool
	// SettingSources controls Claude Code filesystem settings sources loaded by
	// the Claude CLI. Nil uses the adapter default: user, project, local. An
	// empty slice passes --setting-sources= and disables those sources.
	SettingSources []SettingSource

	// AllowSkipPermissionsFlag permits adding Claude's skip-permissions capability flag.
	AllowSkipPermissionsFlag bool
	// InitializeTimeout bounds the Claude control-protocol initialize request.
	InitializeTimeout time.Duration
	// ControlHandlerTimeout bounds one inbound Claude control request.
	ControlHandlerTimeout time.Duration

	// Env is merged into every launched Claude process environment.
	Env map[string]string

	defaultPermissionModeSet bool
}

func applyOptions(opts []Option) Options {
	options := Options{
		AgentName:             "acp-go-claude",
		AgentTitle:            "acp-go-claude",
		AgentVersion:          "0.1.0",
		DefaultPermissionMode: string(modeDefault),
		SettingSources:        defaultSettingSources(),
		InitializeTimeout:     time.Minute,
		ControlHandlerTimeout: 5 * time.Minute,
	}

	for _, opt := range opts {
		opt(&options)
	}

	return options
}

func defaultSettingSources() []SettingSource {
	return []SettingSource{SettingSourceUser, SettingSourceProject, SettingSourceLocal}
}

func settingSourceArgs(sources []SettingSource) []string {
	args := make([]string, len(sources))
	for index, source := range sources {
		args[index] = string(source)
	}

	return args
}

// WithLogger configures structured diagnostic logging.
func WithLogger(logger *slog.Logger) Option {
	return func(options *Options) {
		options.Logger = logger
	}
}

// WithAgentName sets the protocol identifier advertised during ACP initialize.
func WithAgentName(name string) Option {
	return func(options *Options) {
		options.AgentName = name
	}
}

// WithAgentTitle sets the human-readable agent name advertised during ACP initialize.
func WithAgentTitle(title string) Option {
	return func(options *Options) {
		options.AgentTitle = title
	}
}

// WithAgentVersion sets the agent version advertised during ACP initialize and
// used by adapter OpenTelemetry instrumentation.
func WithAgentVersion(version string) Option {
	return func(options *Options) {
		options.AgentVersion = version
	}
}

// WithMeterProvider configures the OpenTelemetry meter provider used for
// adapter metrics. If unset, metrics are no-ops.
func WithMeterProvider(provider metric.MeterProvider) Option {
	return func(options *Options) {
		options.MeterProvider = provider
	}
}

// WithTextMapPropagator configures trace-context propagation for ACP _meta and
// Claude process launch environment. If unset, W3C trace context plus baggage
// propagation is used.
func WithTextMapPropagator(propagator propagation.TextMapPropagator) Option {
	return func(options *Options) {
		options.TextMapPropagator = propagator
	}
}

// WithTracerProvider configures the OpenTelemetry tracer provider used for
// adapter spans. If unset, tracing is a no-op.
func WithTracerProvider(provider trace.TracerProvider) Option {
	return func(options *Options) {
		options.TracerProvider = provider
	}
}

// WithClaudePath sets the Claude CLI executable path. If unset, PATH is searched.
func WithClaudePath(path string) Option {
	return func(options *Options) {
		options.ClaudePath = path
	}
}

// WithClaudeHome sets CLAUDE_CONFIG_DIR for launched Claude CLI sessions.
func WithClaudeHome(path string) Option {
	return func(options *Options) {
		options.ClaudeHome = path
	}
}

// WithSessionStore configures external Claude transcript storage.
//
// A configured store enables Claude transcript mirroring for new turns and lets
// session/load or session/resume hydrate missing local Claude JSONL from the
// store. Implement optional SessionStoreLister, SessionStoreSubkeyLister, and
// SessionStoreReplacer interfaces to support list, subagent hydration, and
// atomic replacement on import.
func WithSessionStore(store SessionStore) Option {
	return func(options *Options) {
		options.SessionStore = store
	}
}

// WithSessionStoreLoadTimeout bounds session store reads used during resume.
func WithSessionStoreLoadTimeout(timeout time.Duration) Option {
	return func(options *Options) {
		options.SessionStoreLoadTimeout = timeout
	}
}

// WithDefaultModel selects a Claude model for newly created sessions.
func WithDefaultModel(model string) Option {
	return func(options *Options) {
		options.DefaultModel = model
	}
}

// WithDefaultPermissionMode sets the initial Claude permission mode.
func WithDefaultPermissionMode(mode string) Option {
	return func(options *Options) {
		options.DefaultPermissionMode = mode
		options.defaultPermissionModeSet = true
	}
}

// WithDefaultSystemPrompt sets the system prompt passed to Claude sessions.
func WithDefaultSystemPrompt(prompt string) Option {
	return func(options *Options) {
		options.DefaultSystemPrompt = prompt
	}
}

// WithHideClaudeAuth suppresses Claude subscription terminal auth methods.
func WithHideClaudeAuth(enabled bool) Option {
	return func(options *Options) {
		options.HideClaudeAuth = enabled
	}
}

// WithBareMode launches Claude sessions with --bare. Bare mode disables
// Claude's automatic project/context discovery and keychain/OAuth auth; explicit
// ACP-provided MCP config, system prompt, additional directories, and
// API-key/apiKeyHelper auth are still passed.
func WithBareMode(enabled bool) Option {
	return func(options *Options) {
		options.BareMode = enabled
	}
}

// WithSettingSources configures Claude Code filesystem settings sources passed
// as --setting-sources. With no arguments, user/project/local sources are
// disabled for launched Claude sessions.
func WithSettingSources(sources ...SettingSource) Option {
	return func(options *Options) {
		options.SettingSources = make([]SettingSource, len(sources))
		copy(options.SettingSources, sources)
	}
}

// WithAllowSkipPermissionsFlag permits adding Claude's skip-permissions capability flag.
func WithAllowSkipPermissionsFlag(enabled bool) Option {
	return func(options *Options) {
		options.AllowSkipPermissionsFlag = enabled
	}
}

// WithInitializeTimeout bounds the Claude control-protocol initialize request.
func WithInitializeTimeout(timeout time.Duration) Option {
	return func(options *Options) {
		options.InitializeTimeout = timeout
	}
}

// WithControlHandlerTimeout bounds one inbound Claude control request.
func WithControlHandlerTimeout(timeout time.Duration) Option {
	return func(options *Options) {
		options.ControlHandlerTimeout = timeout
	}
}

// WithEnv adds environment variables to every launched Claude process.
func WithEnv(env map[string]string) Option {
	return func(options *Options) {
		options.Env = env
	}
}

// WithMCPProxyCommand sets the command used for ACP-transport MCP stdio shims.
// If unset, the current executable is used and must support the "mcp-proxy"
// subcommand.
func WithMCPProxyCommand(command string, args ...string) Option {
	return func(options *Options) {
		options.MCPProxyCommand = command

		options.MCPProxyArgs = append([]string(nil), args...)
	}
}
