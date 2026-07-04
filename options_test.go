package claudeacp

import (
	"log/slog"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	metricnoop "go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

func TestApplyOptionsBranches(t *testing.T) {
	t.Parallel()

	logger := slog.Default()
	store := NewInMemorySessionStore()
	meter := metricnoop.NewMeterProvider()
	tracer := tracenoop.NewTracerProvider()
	propagator := propagation.TraceContext{}
	env := map[string]string{"ANTHROPIC_BASE_URL": "https://example.test"}

	options := applyOptions([]Option{
		WithLogger(logger),
		WithAgentName("agent-name"),
		WithAgentTitle("Agent Title"),
		WithAgentVersion("v1.2.3"),
		WithMeterProvider(meter),
		WithTextMapPropagator(propagator),
		WithTracerProvider(tracer),
		WithExecutablePath("/bin/claude"),
		WithHome("/tmp/claude-home"),
		WithSessionStore(store),
		WithSessionStoreLoadTimeout(2 * time.Second),
		WithDefaultModel("claude-sonnet"),
		WithClaudeDefaultPermissionMode(permissionModeAcceptEdits),
		WithClaudeDefaultSystemPrompt("system"),
		WithClaudeHideAuth(true),
		WithClaudeBareMode(true),
		WithClaudeSettingSources(SettingSourceProject, SettingSourceLocal),
		WithClaudeAllowSkipPermissionsFlag(true),
		WithClaudeInitializeTimeout(3 * time.Second),
		WithClaudeControlHandlerTimeout(4 * time.Second),
		WithEnv(env),
		WithConcurrencyLimits(ConcurrencyLimits{
			MaxActiveSessions:        2,
			MaxConcurrentPrompts:     3,
			MaxConcurrentClientCalls: 4,
		}),
	})

	require.Same(t, logger, options.Logger)
	require.Equal(t, "agent-name", options.AgentName)
	require.Equal(t, "Agent Title", options.AgentTitle)
	require.Equal(t, "v1.2.3", options.AgentVersion)
	require.Equal(t, meter, options.MeterProvider)
	require.Equal(t, tracer, options.TracerProvider)
	require.Equal(t, propagator, options.TextMapPropagator)
	require.Equal(t, "/bin/claude", options.ExecutablePath)
	require.Equal(t, "/tmp/claude-home", options.Home)
	require.Same(t, store, options.SessionStore)
	require.Equal(t, 2*time.Second, options.SessionStoreLoadTimeout)
	require.Equal(t, "claude-sonnet", options.DefaultModel)
	require.Equal(t, permissionModeAcceptEdits, options.DefaultPermissionMode)
	require.True(t, options.defaultPermissionModeSet)
	require.Equal(t, "system", options.DefaultSystemPrompt)
	require.True(t, options.HideAuth)
	require.True(t, options.BareMode)
	require.Equal(t, []SettingSource{SettingSourceProject, SettingSourceLocal}, options.SettingSources)
	require.True(t, options.AllowSkipPermissionsFlag)
	require.Equal(t, 3*time.Second, options.InitializeTimeout)
	require.Equal(t, 4*time.Second, options.ControlHandlerTimeout)
	require.Equal(t, env, options.Env)
	require.Equal(t, 2, options.ConcurrencyLimits.MaxActiveSessions)
	require.Equal(t, 3, options.ConcurrencyLimits.MaxConcurrentPrompts)
	require.Equal(t, 4, options.ConcurrencyLimits.MaxConcurrentClientCalls)
	require.Equal(t, []string{"project", "local"}, settingSourceArgs(options.SettingSources))

	defaults := applyOptions(nil)
	require.Equal(t, []SettingSource{SettingSourceUser, SettingSourceProject, SettingSourceLocal}, defaults.SettingSources)
}
