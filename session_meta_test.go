package claudeacp

import (
	"context"
	"log/slog"
	"os"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/stretchr/testify/require"
)

func TestClaudeOptionsMetaAndParsing(t *testing.T) {
	t.Parallel()

	options := ClaudeOptions{
		Model:          "sonnet",
		Bare:           true,
		Env:            map[string]string{"ANTHROPIC_BASE_URL": "https://example.test"},
		ExtraPathDirs:  []string{absTestPath("opt", "session", "bin")},
		OutputSchema:   map[string]any{"type": "object"},
		SystemPrompt:   "system",
		PermissionMode: permissionModeDontAsk,
	}
	meta := options.Meta()
	parsed, err := claudeOptionsFromMeta(meta)
	require.NoError(t, err)
	require.Equal(t, options, parsed)

	options.Env["ANTHROPIC_BASE_URL"] = "changed"
	options.ExtraPathDirs[0] = absTestPath("changed")
	options.OutputSchema["type"] = "changed"
	require.Equal(t, "https://example.test", parsed.Env["ANTHROPIC_BASE_URL"])
	require.Equal(t, []string{absTestPath("opt", "session", "bin")}, parsed.ExtraPathDirs)
	require.Equal(t, "object", parsed.OutputSchema["type"])

	jsonSchema := outputSchemaJSONSchema(parsed.OutputSchema)
	jsonSchema["type"] = "changed-again"
	require.Equal(t, "object", parsed.OutputSchema["type"])
	require.Nil(t, outputSchemaJSONSchema(nil))
}

// TestStartSessionCarriesExtraPathDirsToLaunch pins the plumbing seam: the
// session-scoped dirs must reach the launched process options in request order,
// because that order is what makes the caller's directory shadow the harness's
// own resolution.
func TestStartSessionCarriesExtraPathDirsToLaunch(t *testing.T) {
	ctx := context.Background()
	cwd := t.TempDir()
	sessionID := acp.SessionId("17171717-1717-4717-8717-171717171717")

	agent, _, _ := newFakeLifecycleAgent(t, newFakeClaudeTransport())

	var captured claude.Options
	transport := newFakeClaudeTransport()
	agent.newClaudeClient = func(log *slog.Logger, options claude.Options) *claude.Client {
		captured = options

		return claude.NewClient(log, options, transport)
	}

	session, err := agent.startSession(ctx, sessionID, sessionStart{
		Cwd: cwd,
		MetaOptions: ClaudeOptions{
			ExtraPathDirs: []string{absTestPath("opt", "session", "bin"), absTestPath("opt", "shared", "bin")},
		},
	})
	require.NoError(t, err)
	require.Equal(t, []string{absTestPath("opt", "session", "bin"), absTestPath("opt", "shared", "bin")}, captured.ExtraPathDirs)

	require.NoError(t, session.Close(ctx))
}

func TestSessionMetaCannotRedirectTheManagedClaudeHome(t *testing.T) {
	agent, _, _ := newFakeLifecycleAgent(t, newFakeClaudeTransport())

	copyCalled := false
	originalCopy := copyClaudeConfigFiles
	copyClaudeConfigFiles = func(string, string, claude.Options) error {
		copyCalled = true

		return nil
	}
	t.Cleanup(func() { copyClaudeConfigFiles = originalCopy })

	_, err := agent.NewSession(t.Context(), NewSessionRequest(t.TempDir(), WithSessionMeta(map[string]any{
		claudeMetaKey: map[string]any{
			metaOptionsKey: map[string]any{
				settingsFieldEnv: map[string]any{"CLAUDE_CONFIG_DIR": t.TempDir()},
			},
		},
	})))
	requireExactUnsupportedField(t, err, "_meta.claude.options.env.CLAUDE_CONFIG_DIR")
	require.False(t, copyCalled)
}

func TestCloneStringMap(t *testing.T) {
	t.Parallel()

	require.Nil(t, cloneStringMap(nil))

	original := map[string]string{"key": "value"}
	cloned := cloneStringMap(original)
	require.Equal(t, original, cloned)

	cloned["key"] = "changed"
	require.Equal(t, "value", original["key"])
}

func TestClaudeOptionsValidationBranches(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name      string
		meta      map[string]any
		wantField string
	}{
		{
			name:      "claude namespace not object",
			meta:      map[string]any{claudeMetaKey: "bad"},
			wantField: "_meta.claude",
		},
		{
			name:      "unknown claude field",
			meta:      map[string]any{claudeMetaKey: map[string]any{"extra": true}},
			wantField: "_meta.claude.extra",
		},
		{
			name:      "raw event not object",
			meta:      map[string]any{claudeMetaKey: map[string]any{metaRawEventKey: true}},
			wantField: "_meta.claude.rawEvent",
		},
		{
			name: "raw event enabled not bool",
			meta: map[string]any{claudeMetaKey: map[string]any{
				metaRawEventKey: map[string]any{metaRawEventEnabledKey: "yes"},
			}},
			wantField: "_meta.claude.rawEvent.enabled",
		},
		{
			name: "raw event unknown field",
			meta: map[string]any{claudeMetaKey: map[string]any{
				metaRawEventKey: map[string]any{"extra": true},
			}},
			wantField: "_meta.claude.rawEvent.extra",
		},
		{
			name:      "options not object",
			meta:      map[string]any{claudeMetaKey: map[string]any{metaOptionsKey: "bad"}},
			wantField: "_meta.claude.options",
		},
		{
			name:      "bare not bool",
			meta:      map[string]any{claudeMetaKey: map[string]any{metaOptionsKey: map[string]any{metaBareKey: "yes"}}},
			wantField: "_meta.claude.options.bare",
		},
		{
			name:      "env not object",
			meta:      map[string]any{claudeMetaKey: map[string]any{metaOptionsKey: map[string]any{settingsFieldEnv: "bad"}}},
			wantField: "_meta.claude.options.env",
		},
		{
			name:      "env value not string",
			meta:      map[string]any{claudeMetaKey: map[string]any{metaOptionsKey: map[string]any{settingsFieldEnv: map[string]any{"A": 1}}}},
			wantField: "_meta.claude.options.env.A",
		},
		{
			name:      "system prompt not string",
			meta:      map[string]any{claudeMetaKey: map[string]any{metaOptionsKey: map[string]any{metaSystemPromptKey: 1}}},
			wantField: "_meta.claude.options.systemPrompt",
		},
		{
			name:      "model not string",
			meta:      map[string]any{claudeMetaKey: map[string]any{metaOptionsKey: map[string]any{metaModelKey: 1}}},
			wantField: "_meta.claude.options.model",
		},
		{
			name:      "permission mode not string",
			meta:      map[string]any{claudeMetaKey: map[string]any{metaOptionsKey: map[string]any{metaPermissionModeKey: 1}}},
			wantField: "_meta.claude.options.permissionMode",
		},
		{
			name:      "permission mode unsupported",
			meta:      map[string]any{claudeMetaKey: map[string]any{metaOptionsKey: map[string]any{metaPermissionModeKey: "invalid"}}},
			wantField: "_meta.claude.options.permissionMode",
		},
		{
			name:      "schema not object",
			meta:      map[string]any{claudeMetaKey: map[string]any{metaOptionsKey: map[string]any{metaOutputSchemaKey: "bad"}}},
			wantField: metaOptionPath(metaOutputSchemaKey),
		},
		{
			name:      "schema not serializable",
			meta:      map[string]any{claudeMetaKey: map[string]any{metaOptionsKey: map[string]any{metaOutputSchemaKey: map[string]any{"bad": func() {}}}}},
			wantField: metaOptionPath(metaOutputSchemaKey),
		},
		{
			name:      "blocked env key",
			meta:      map[string]any{claudeMetaKey: map[string]any{metaOptionsKey: map[string]any{settingsFieldEnv: map[string]any{"PATH": "/bin"}}}},
			wantField: "_meta.claude.options.env.PATH",
		},
		{
			name: "managed root env key",
			meta: map[string]any{claudeMetaKey: map[string]any{metaOptionsKey: map[string]any{settingsFieldEnv: map[string]any{
				"XDG_CONFIG_HOME": "/attacker-selected",
			}}}},
			wantField: "_meta.claude.options.env.XDG_CONFIG_HOME",
		},
		{
			name:      "env key empty",
			meta:      map[string]any{claudeMetaKey: map[string]any{metaOptionsKey: map[string]any{settingsFieldEnv: map[string]any{"": "x"}}}},
			wantField: "_meta.claude.options.env.",
		},
		{
			name:      "env key carries an equals sign",
			meta:      map[string]any{claudeMetaKey: map[string]any{metaOptionsKey: map[string]any{settingsFieldEnv: map[string]any{"A=B": "x"}}}},
			wantField: "_meta.claude.options.env.A=B",
		},
		{
			name:      "env key carries a NUL",
			meta:      map[string]any{claudeMetaKey: map[string]any{metaOptionsKey: map[string]any{settingsFieldEnv: map[string]any{"A\x00B": "x"}}}},
			wantField: "_meta.claude.options.env.A\x00B",
		},
		{
			name:      "env value carries a NUL",
			meta:      map[string]any{claudeMetaKey: map[string]any{metaOptionsKey: map[string]any{settingsFieldEnv: map[string]any{"A": "x\x00y"}}}},
			wantField: "_meta.claude.options.env.A",
		},
		{
			name:      "extra path dirs not array",
			meta:      map[string]any{claudeMetaKey: map[string]any{metaOptionsKey: map[string]any{metaExtraPathDirsKey: "/opt/bin"}}},
			wantField: "_meta.claude.options.extraPathDirs",
		},
		{
			name:      "extra path dirs entry not string",
			meta:      map[string]any{claudeMetaKey: map[string]any{metaOptionsKey: map[string]any{metaExtraPathDirsKey: []any{absTestPath("opt", "bin"), 1}}}},
			wantField: "_meta.claude.options.extraPathDirs[1]",
		},
		{
			name:      "extra path dirs entry relative",
			meta:      map[string]any{claudeMetaKey: map[string]any{metaOptionsKey: map[string]any{metaExtraPathDirsKey: []any{absTestPath("opt", "bin"), "relative/bin"}}}},
			wantField: "_meta.claude.options.extraPathDirs[1]",
		},
		{
			name:      "extra path dirs entry empty",
			meta:      map[string]any{claudeMetaKey: map[string]any{metaOptionsKey: map[string]any{metaExtraPathDirsKey: []any{""}}}},
			wantField: "_meta.claude.options.extraPathDirs[0]",
		},
		{
			name: "extra path dirs entry contains separator",
			meta: map[string]any{claudeMetaKey: map[string]any{metaOptionsKey: map[string]any{
				metaExtraPathDirsKey: []any{absTestPath("opt", "bin") + string(os.PathListSeparator) + absTestPath("srv", "bin")},
			}}},
			wantField: "_meta.claude.options.extraPathDirs[0]",
		},
		{
			name:      "unknown option",
			meta:      map[string]any{claudeMetaKey: map[string]any{metaOptionsKey: map[string]any{"extra": true}}},
			wantField: "_meta.claude.options.extra",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := claudeOptionsFromMeta(tc.meta)
			requireExactUnsupportedField(t, err, tc.wantField)
		})
	}
}

func TestClaudeMetaSmallHelpers(t *testing.T) {
	t.Parallel()

	require.Equal(t, []string{absTestPath("a"), absTestPath("b")}, sessionAdditionalDirectories([]string{absTestPath("a"), absTestPath("b")}))
	require.True(t, blockedClaudeEnvKey("LD_PRELOAD"))
	// The blocklist compares through the platform identity, so a lowercase
	// spelling is a distinct variable on Unix and the same one on Windows.
	require.Equal(t,
		claude.EnvironmentKey("dyld_library_path") == "DYLD_LIBRARY_PATH",
		blockedClaudeEnvKey("dyld_library_path"),
	)
	require.True(t, blockedClaudeEnvKey(privateAdapterEnvPrefix+"TEST"))
	require.True(t, blockedClaudeEnvKey("CLAUDE_CONFIG_DIR"))
	require.True(t, blockedClaudeEnvKey("HOME"))
	require.True(t, blockedClaudeEnvKey("XDG_RUNTIME_DIR"))
	require.False(t, blockedClaudeEnvKey("ANTHROPIC_BASE_URL"))
	require.True(t, validClaudePermissionMode(string(modeDefault)))
	require.True(t, validClaudePermissionMode(permissionModeAcceptEdits))
	require.True(t, validClaudePermissionMode(permissionModeBypassPermissions))
	require.True(t, validClaudePermissionMode(string(modePlan)))
	require.True(t, validClaudePermissionMode(permissionModeDontAsk))
	require.True(t, validClaudePermissionMode(string(modeAuto)))
	require.False(t, validClaudePermissionMode("bad"))

	env, err := stringMapOption(map[string]string{"A": "B"}, "env")
	require.NoError(t, err)
	require.Equal(t, map[string]string{"A": "B"}, env)

	dirs, err := stringSliceOption([]string{absTestPath("a")}, metaExtraPathDirsKey)
	require.NoError(t, err)
	require.Equal(t, []string{absTestPath("a")}, dirs)

	dirs, err = stringSliceOption([]any{absTestPath("a"), absTestPath("b")}, metaExtraPathDirsKey)
	require.NoError(t, err)
	require.Equal(t, []string{absTestPath("a"), absTestPath("b")}, dirs)

	_, err = claudeOptionsFromMeta(map[string]any{
		claudeMetaKey: map[string]any{metaOptionsKey: map[string]any{metaOutputSchemaKey: map[string]any{}}},
	})
	requireExactUnsupportedField(t, err, metaOptionPath(metaOutputSchemaKey))
}
