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

	// ExecutablePath is the Claude CLI executable path. If empty, PATH is searched.
	ExecutablePath string
	// Home sets CLAUDE_CONFIG_DIR for launched Claude CLI sessions.
	Home string
	// DefaultModel is passed to newly created Claude sessions when non-empty.
	DefaultModel string
	// Env is merged into every launched Claude process environment.
	Env map[string]string

	// Logger receives structured diagnostic logs. If nil, the default logger is used.
	Logger *slog.Logger
	// TracerProvider records adapter spans. If nil, tracing is a no-op.
	TracerProvider trace.TracerProvider
	// MeterProvider records adapter metrics. If nil, metrics are no-ops.
	MeterProvider metric.MeterProvider
	// TextMapPropagator extracts ACP _meta trace context and injects Claude launch env.
	// If nil, W3C trace context plus baggage propagation is used.
	TextMapPropagator propagation.TextMapPropagator

	// SessionStore mirrors Claude transcript writes and backs store restores.
	SessionStore SessionStore
	// SessionStoreLoadTimeout bounds store load/list operations used for resume.
	SessionStoreLoadTimeout time.Duration
	// ConcurrencyLimits controls process-local backpressure.
	ConcurrencyLimits ConcurrencyLimits
	// SeedFiles maps paths relative to the resolved Claude config directory to
	// file contents written into that directory before each Claude CLI session
	// launches, so the launched CLI reads them as its own config (e.g.
	// settings.json).
	SeedFiles map[string]string
	// SettingsFile is a path relative to the resolved Claude config directory
	// passed to the Claude CLI as --settings, loading an additional settings
	// layer on top of the base settings.json. It requires an explicit Home.
	SettingsFile string

	// DefaultPermissionMode is the initial Claude permission mode.
	DefaultPermissionMode string
	// DefaultSystemPrompt is passed to newly created Claude sessions when non-empty.
	DefaultSystemPrompt string
	// HideAuth suppresses Claude subscription terminal auth methods.
	HideAuth bool
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
	// TurnTimeout bounds one Claude prompt turn. Zero (the default) means no
	// deadline. On expiry the turn is aborted and fails with cause "timeout".
	TurnTimeout time.Duration

	defaultPermissionModeSet bool
}

// ConcurrencyLimits controls per-agent/session backpressure. Zero fields use defaults.
type ConcurrencyLimits struct {
	MaxActiveSessions        int
	MaxConcurrentClientCalls int
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

// WithExecutablePath sets the Claude CLI executable path. If unset, PATH is searched.
func WithExecutablePath(path string) Option {
	return func(options *Options) {
		options.ExecutablePath = path
	}
}

// WithHome sets CLAUDE_CONFIG_DIR for launched Claude CLI sessions.
func WithHome(path string) Option {
	return func(options *Options) {
		options.Home = path
	}
}

// WithSessionStore configures external Claude transcript storage.
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

// WithClaudeDefaultPermissionMode sets the initial Claude permission mode.
func WithClaudeDefaultPermissionMode(mode string) Option {
	return func(options *Options) {
		options.DefaultPermissionMode = mode
		options.defaultPermissionModeSet = true
	}
}

// WithClaudeDefaultSystemPrompt sets the system prompt passed to Claude sessions.
func WithClaudeDefaultSystemPrompt(prompt string) Option {
	return func(options *Options) {
		options.DefaultSystemPrompt = prompt
	}
}

// WithClaudeHideAuth suppresses Claude subscription terminal auth methods.
func WithClaudeHideAuth(enabled bool) Option {
	return func(options *Options) {
		options.HideAuth = enabled
	}
}

// WithClaudeBareMode launches Claude sessions with --bare. Bare mode disables
// Claude's automatic project/context discovery and keychain/OAuth auth; explicit
// ACP-provided MCP config, system prompt, additional directories, and
// API-key/apiKeyHelper auth are still passed.
func WithClaudeBareMode(enabled bool) Option {
	return func(options *Options) {
		options.BareMode = enabled
	}
}

// WithClaudeSettingSources configures Claude Code filesystem settings sources passed
// as --setting-sources. With no arguments, user/project/local sources are
// disabled for launched Claude sessions.
func WithClaudeSettingSources(sources ...SettingSource) Option {
	return func(options *Options) {
		options.SettingSources = make([]SettingSource, len(sources))
		copy(options.SettingSources, sources)
	}
}

// WithClaudeSettingsFile registers a settings-overlay file loaded on top of the
// base settings.json. relpath is confined to the resolved Claude config
// directory (the same anchor as WithSeedFiles) and passed to the Claude CLI as
// --settings <abspath>. It requires an explicit Home: setting it without a
// resolvable home, an absolute path, a ".." escape, or an empty key fails
// closed at session start.
func WithClaudeSettingsFile(relpath string) Option {
	return func(options *Options) {
		options.SettingsFile = relpath
	}
}

// WithClaudeAllowSkipPermissionsFlag permits adding Claude's skip-permissions capability flag.
func WithClaudeAllowSkipPermissionsFlag(enabled bool) Option {
	return func(options *Options) {
		options.AllowSkipPermissionsFlag = enabled
	}
}

// WithClaudeInitializeTimeout bounds the Claude control-protocol initialize request.
func WithClaudeInitializeTimeout(timeout time.Duration) Option {
	return func(options *Options) {
		options.InitializeTimeout = timeout
	}
}

// WithClaudeControlHandlerTimeout bounds one inbound Claude control request.
func WithClaudeControlHandlerTimeout(timeout time.Duration) Option {
	return func(options *Options) {
		options.ControlHandlerTimeout = timeout
	}
}

// WithTurnTimeout bounds one Claude prompt turn. Zero (the default) disables the
// deadline. On expiry the native turn is aborted and session/prompt fails with a
// claude_turn_failed error whose cause is "timeout" (never cancelled).
func WithTurnTimeout(timeout time.Duration) Option {
	return func(options *Options) {
		options.TurnTimeout = timeout
	}
}

// WithEnv adds environment variables to every launched Claude process.
func WithEnv(env map[string]string) Option {
	return func(options *Options) {
		options.Env = env
	}
}

// WithConcurrencyLimits sets process-local backpressure limits. Zero fields use defaults.
func WithConcurrencyLimits(limits ConcurrencyLimits) Option {
	return func(options *Options) {
		options.ConcurrencyLimits = limits
	}
}

// WithSeedFiles registers files written into the session's resolved Claude
// config directory before the Claude CLI launches. Keys are paths relative to
// that directory and values are the file contents (e.g. settings.json). Paths
// are confined to the config directory: absolute paths, ".." escapes, and
// empty keys fail closed at session start.
func WithSeedFiles(files map[string]string) Option {
	return func(options *Options) {
		options.SeedFiles = cloneStringMap(files)
	}
}
