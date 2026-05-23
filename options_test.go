package claudeacp

import (
	"log/slog"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/require"
	"go.opentelemetry.io/otel/metric/noop"
	"go.opentelemetry.io/otel/propagation"
	tracenoop "go.opentelemetry.io/otel/trace/noop"
)

func TestApplyOptionsDefaultsAndOverrides(t *testing.T) {
	t.Parallel()

	logger := slog.Default()
	meterProvider := noop.NewMeterProvider()
	propagator := propagation.TraceContext{}
	tracerProvider := tracenoop.NewTracerProvider()
	options := applyOptions([]Option{
		WithAgentName("custom-agent"),
		WithAgentTitle("Custom Agent"),
		WithAgentVersion("v1.2.3"),
		WithLogger(logger),
		WithMeterProvider(meterProvider),
		WithTextMapPropagator(propagator),
		WithTracerProvider(tracerProvider),
		WithClaudePath("/bin/claude"),
		WithClaudeHome("/tmp/claude-home"),
		WithDefaultModel("claude-test"),
		WithDefaultPermissionMode("plan"),
		WithDefaultSystemPrompt("system"),
		WithHideClaudeAuth(true),
		WithBareMode(true),
		WithSettingSources(SettingSourceProject),
		WithAllowSkipPermissionsFlag(true),
		WithInitializeTimeout(3 * time.Second),
		WithControlHandlerTimeout(4 * time.Second),
		WithEnv(map[string]string{"A": "B"}),
		WithMCPProxyCommand("/bin/proxy", "fixed"),
	})

	require.Equal(t, "custom-agent", options.AgentName)
	require.Equal(t, "Custom Agent", options.AgentTitle)
	require.Equal(t, "v1.2.3", options.AgentVersion)
	require.Equal(t, logger, options.Logger)
	require.Equal(t, meterProvider, options.MeterProvider)
	require.Equal(t, propagator, options.TextMapPropagator)
	require.Equal(t, tracerProvider, options.TracerProvider)
	require.Equal(t, "/bin/claude", options.ClaudePath)
	require.Equal(t, "/tmp/claude-home", options.ClaudeHome)
	require.Equal(t, "claude-test", options.DefaultModel)
	require.Equal(t, "plan", options.DefaultPermissionMode)
	require.Equal(t, "system", options.DefaultSystemPrompt)
	require.True(t, options.HideClaudeAuth)
	require.True(t, options.BareMode)
	require.Equal(t, []SettingSource{SettingSourceProject}, options.SettingSources)
	require.Equal(t, []string{"project"}, settingSourceArgs(options.SettingSources))
	require.True(t, options.AllowSkipPermissionsFlag)
	require.Equal(t, 3*time.Second, options.InitializeTimeout)
	require.Equal(t, 4*time.Second, options.ControlHandlerTimeout)
	require.Equal(t, map[string]string{"A": "B"}, options.Env)
	require.Equal(t, "/bin/proxy", options.MCPProxyCommand)
	require.Equal(t, []string{"fixed"}, options.MCPProxyArgs)
}

func TestApplyOptionsDefaultAndDisabledSettingSources(t *testing.T) {
	t.Parallel()

	options := applyOptions(nil)
	require.Equal(t, []SettingSource{SettingSourceUser, SettingSourceProject, SettingSourceLocal}, options.SettingSources)

	options = applyOptions([]Option{WithSettingSources()})
	require.Empty(t, options.SettingSources)
	require.NotNil(t, options.SettingSources)
	require.Empty(t, settingSourceArgs(options.SettingSources))
}

func TestPromptResultForObserver(t *testing.T) {
	t.Parallel()

	result := promptResultForObserver(acp.PromptResponse{StopReason: acp.StopReasonEndTurn}, assertAnError{}, "sonnet")
	require.Equal(t, assertAnError{}, result.Err)
	require.Equal(t, "sonnet", result.Model)
	require.Equal(t, string(acp.StopReasonEndTurn), result.StopReason)

	thoughtTokens := 3
	cachedReadTokens := 4
	cachedWriteTokens := 5
	result = promptResultForObserver(acp.PromptResponse{
		Usage: &acp.Usage{
			CachedReadTokens:  &cachedReadTokens,
			CachedWriteTokens: &cachedWriteTokens,
			InputTokens:       1,
			OutputTokens:      2,
			ThoughtTokens:     &thoughtTokens,
			TotalTokens:       15,
		},
	}, nil, "opus")
	require.Equal(t, 4, result.CachedReadTokens)
	require.Equal(t, 5, result.CachedWriteTokens)
	require.Equal(t, 1, result.InputTokens)
	require.Equal(t, 2, result.OutputTokens)
	require.Equal(t, 3, result.ThoughtTokens)
	require.Equal(t, 15, result.TotalTokens)
}

type assertAnError struct{}

func (assertAnError) Error() string { return "assertion error" }
