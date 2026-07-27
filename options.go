package claudeacp

import (
	"context"
	"log/slog"
	"time"

	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/propagation"
	"go.opentelemetry.io/otel/trace"
)

// Option configures the Claude ACP agent.
type Option func(*Options)

// RuntimeResourceKind identifies the lifecycle scope consuming a host-managed resource.
type RuntimeResourceKind string

const (
	RuntimeResourceRuntime   RuntimeResourceKind = "runtime"
	RuntimeResourceSession   RuntimeResourceKind = "session"
	RuntimeResourcePrompt    RuntimeResourceKind = "prompt"
	RuntimeResourceDiscovery RuntimeResourceKind = "discovery"
)

type RuntimeProcessKind string

const (
	RuntimeProcessHomeLockSupervisor RuntimeProcessKind = "home_lock_supervisor"
	RuntimeProcessProviderDescendant RuntimeProcessKind = "provider_descendant"
)

// RuntimeContainmentMode identifies the selected native process boundary.
type RuntimeContainmentMode string

const privateAdapterEnvPrefix = "ACP_" + "GO_CLAUDE_INTERNAL_"

const (
	RuntimeContainmentAuthoritative RuntimeContainmentMode = "authoritative"
	RuntimeContainmentBestEffort    RuntimeContainmentMode = "best_effort"
	RuntimeContainmentUnavailable   RuntimeContainmentMode = "unavailable"
)

type RuntimeStartupStage string

const (
	RuntimeStartupSpawn         RuntimeStartupStage = "spawn"
	RuntimeStartupReadiness     RuntimeStartupStage = "readiness"
	RuntimeStartupConfiguration RuntimeStartupStage = "configuration"
	RuntimeStartupSession       RuntimeStartupStage = "session"
)

// RuntimeResourceHooks lets an embedding host enforce native-root and scratch-root limits.
type RuntimeResourceHooks struct {
	AcquireNativeRoot      func(context.Context, RuntimeResourceKind) (func(), error)
	ReserveScratchRoot     func(context.Context, RuntimeResourceKind) (func(), error)
	ObserveProcess         func(context.Context, RuntimeProcessKind, int64)
	ObserveProcessSnapshot func(context.Context, RuntimeProcessKind, int)
	ObserveStartupStage    func(context.Context, RuntimeResourceKind, RuntimeStartupStage, time.Duration, error)
	ObserveContainment     func(context.Context, RuntimeContainmentMode)
}

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
	// ScratchDir is the parent directory for all ephemeral on-disk
	// materialization (per-session roots, hydration temp files, probe dirs).
	// Empty means the system temp directory.
	ScratchDir string
	// InputHandoffRoot is the absolute directory a host hands prompt-image
	// bytes over in. It is a read root only: nothing is ever written, moved, or
	// deleted under it. Empty rejects every handoff-form image block.
	InputHandoffRoot string
	// ProviderAuthRoot is the absolute host-owned durable directory holding the
	// adapter's values-free provider-auth ledger. Empty leaves every
	// `_claude/auth/*` leg unadvertised.
	ProviderAuthRoot string
	// ProviderAuthDirectHome is the exact canonical Claude config directory the
	// operator consents to a provider-auth leg clearing. Empty, or unequal to
	// Home, leaves `_claude/auth/disconnect` unadvertised.
	ProviderAuthDirectHome string
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

	// SessionStore replaces the default in-memory authority for transcript discovery and restore.
	SessionStore SessionStore
	// SessionStoreLoadTimeout bounds store reads used for restore and listing.
	SessionStoreLoadTimeout time.Duration
	// ConcurrencyLimits controls process-local backpressure.
	ConcurrencyLimits ConcurrencyLimits
	// ImageLimits controls decoded image bytes accepted from prompts and
	// emitted in session updates.
	ImageLimits ImageLimits
	// SeedFiles maps paths relative to the resolved Claude config directory to
	// file contents written into that directory before each Claude CLI session
	// launches, so the launched CLI reads them as its own config (e.g.
	// settings.json).
	SeedFiles map[string]string
	// SettingsFile is a path relative to the resolved Claude config directory
	// passed to the Claude CLI as --settings, loading an additional settings
	// layer on top of the base settings.json. It requires an explicit Home.
	SettingsFile string

	// DirectAPI allows the adapter to make its own outbound calls to the
	// Anthropic API. It is only consulted by `_claude/rateLimits`, which falls
	// back to the API when the harness reports no usage windows — that fallback
	// may issue a billable one-token inference request against the configured
	// account. Enabled by default; disable it to keep the adapter from
	// contacting any network service on its own behalf.
	DirectAPI bool

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
	TurnTimeout          time.Duration
	RuntimeResourceHooks RuntimeResourceHooks
	// DarwinBestEffortContainment explicitly accepts Darwin's process-group
	// boundary and its escaped-descendant and numeric-PGID-reuse risks.
	DarwinBestEffortContainment bool

	defaultPermissionModeSet bool
}

// ConcurrencyLimits controls per-agent/session backpressure. Zero fields use defaults.
type ConcurrencyLimits struct {
	MaxActiveSessions        int
	MaxConcurrentClientCalls int
}

// ImageLimits controls decoded image bytes at the ACP boundary.
type ImageLimits struct {
	MaxInputBytesPerImage     int64
	MaxInputBytesPerPrompt    int64
	MaxOutputBytesPerImage    int64
	MaxOutputBytesPerToolCall int64
}

const defaultImageBytes int64 = 6 * 1024 * 1024

func applyOptions(opts []Option) Options {
	options := Options{
		AgentName:             "acp-go-claude",
		AgentTitle:            "acp-go-claude",
		AgentVersion:          "0.1.0",
		DirectAPI:             true,
		DefaultPermissionMode: string(modeDefault),
		SettingSources:        defaultSettingSources(),
		InitializeTimeout:     time.Minute,
		ControlHandlerTimeout: 5 * time.Minute,
		ImageLimits: ImageLimits{
			MaxInputBytesPerImage:     defaultImageBytes,
			MaxInputBytesPerPrompt:    defaultImageBytes,
			MaxOutputBytesPerImage:    defaultImageBytes,
			MaxOutputBytesPerToolCall: defaultImageBytes,
		},
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

// WithScratchDir sets the parent directory for all ephemeral on-disk
// materialization (per-session roots, hydration temp files, probe dirs).
// Empty means the system temp directory. The directory is created 0700
// when missing.
func WithScratchDir(dir string) Option {
	return func(options *Options) {
		options.ScratchDir = dir
	}
}

// WithInputHandoffRoot sets the absolute directory prompt images may be handed
// over in as local files instead of embedded base64. An image block with empty
// `data`, a `file://` uri under this root, and a valid handoff envelope is read
// and digest-verified before it reaches Claude. The directory is read-only to
// the adapter, which never writes, moves, or deletes anything under it, and it
// is the host's to create and clean up. Unset (the default) rejects every
// handoff-form block; a relative path fails initialization.
func WithInputHandoffRoot(dir string) Option {
	return func(options *Options) {
		options.InputHandoffRoot = dir
	}
}

// WithProviderAuthRoot sets the absolute host-owned durable directory holding
// the adapter's values-free provider-auth ledger. The ledger records which
// native slot each connection generation owns and never credential material,
// authorization URLs, or pasted values. The directory is created 0700 when
// missing and entries are written 0600. Unset (the default), unusable, or
// relative leaves every `_claude/auth/*` leg absent from the capability
// advertisement and answering method-not-found; a relative path additionally
// fails initialization.
func WithProviderAuthRoot(path string) Option {
	return func(options *Options) {
		options.ProviderAuthRoot = path
	}
}

// WithProviderAuthDirectHome names the exact canonical Claude config directory
// the operator consents to `_claude/auth/disconnect` clearing, which is an
// account-level removal in a home the operator also uses. The leg is advertised
// and answers only while this equals the configured Home after path cleaning;
// it authorizes exactly that directory, never a parent, a child, or a symlink
// target of it. Unset (the default) advertises six legs instead of seven.
func WithProviderAuthDirectHome(path string) Option {
	return func(options *Options) {
		options.ProviderAuthDirectHome = path
	}
}

// WithDarwinBestEffortContainment opts into the explicitly limited Darwin
// process-group backend. It is invalid on every non-Darwin platform.
func WithDarwinBestEffortContainment() Option {
	return func(options *Options) {
		options.DarwinBestEffortContainment = true
	}
}

// WithRuntimeResourceHooks installs host-facing native-root and scratch-root admission hooks.
func WithRuntimeResourceHooks(hooks RuntimeResourceHooks) Option {
	return func(options *Options) {
		options.RuntimeResourceHooks = hooks
	}
}

// WithSessionStore replaces the default in-memory session authority with a host store.
func WithSessionStore(store SessionStore) Option {
	return func(options *Options) {
		options.SessionStore = store
	}
}

// WithSessionStoreLoadTimeout bounds session store reads used during restore and listing.
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

// WithClaudeDirectAPI controls whether the adapter may call the Anthropic API
// itself. It is enabled by default and only affects `_claude/rateLimits`: when
// the harness reports no usage windows the adapter reads them from the API,
// which can cost a one-token inference request. Disable it to guarantee the
// adapter never opens a connection of its own; `_claude/rateLimits` then
// reports only what the harness prints.
func WithClaudeDirectAPI(enabled bool) Option {
	return func(options *Options) {
		options.DirectAPI = enabled
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

// WithImageLimits sets decoded image byte limits. Zero fields disable the
// corresponding adapter policy limit.
func WithImageLimits(limits ImageLimits) Option {
	return func(options *Options) {
		options.ImageLimits = limits
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
