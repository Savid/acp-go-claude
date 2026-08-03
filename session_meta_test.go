package claudeacp

import (
	"context"
	"log/slog"
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
		ExtraPathDirs:  []string{"/opt/session/bin"},
		OutputSchema:   map[string]any{"type": "object"},
		SystemPrompt:   "system",
		PermissionMode: permissionModeDontAsk,
	}
	meta := options.Meta()
	parsed, err := claudeOptionsFromMeta(meta)
	require.NoError(t, err)
	require.Equal(t, options, parsed)

	options.Env["ANTHROPIC_BASE_URL"] = "changed"
	options.ExtraPathDirs[0] = "/changed"
	options.OutputSchema["type"] = "changed"
	require.Equal(t, "https://example.test", parsed.Env["ANTHROPIC_BASE_URL"])
	require.Equal(t, []string{"/opt/session/bin"}, parsed.ExtraPathDirs)
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
			ExtraPathDirs: []string{"/opt/session/bin", "/opt/shared/bin"},
		},
	})
	require.NoError(t, err)
	require.Equal(t, []string{"/opt/session/bin", "/opt/shared/bin"}, captured.ExtraPathDirs)

	require.NoError(t, session.Close(ctx))
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
		name    string
		meta    map[string]any
		wantErr string
	}{
		{
			name:    "claude namespace not object",
			meta:    map[string]any{claudeMetaKey: "bad"},
			wantErr: "_meta.claude",
		},
		{
			name:    "unknown claude field",
			meta:    map[string]any{claudeMetaKey: map[string]any{"extra": true}},
			wantErr: "_meta.claude.extra",
		},
		{
			name:    "raw event not object",
			meta:    map[string]any{claudeMetaKey: map[string]any{metaRawEventKey: true}},
			wantErr: "_meta.claude.rawEvent",
		},
		{
			name: "raw event enabled not bool",
			meta: map[string]any{claudeMetaKey: map[string]any{
				metaRawEventKey: map[string]any{metaRawEventEnabledKey: "yes"},
			}},
			wantErr: "_meta.claude.rawEvent.enabled",
		},
		{
			name: "raw event unknown field",
			meta: map[string]any{claudeMetaKey: map[string]any{
				metaRawEventKey: map[string]any{"extra": true},
			}},
			wantErr: "_meta.claude.rawEvent.extra",
		},
		{
			name:    "options not object",
			meta:    map[string]any{claudeMetaKey: map[string]any{metaOptionsKey: "bad"}},
			wantErr: "_meta.claude.options",
		},
		{
			name:    "bare not bool",
			meta:    map[string]any{claudeMetaKey: map[string]any{metaOptionsKey: map[string]any{metaBareKey: "yes"}}},
			wantErr: "_meta.claude.options.bare",
		},
		{
			name:    "env not object",
			meta:    map[string]any{claudeMetaKey: map[string]any{metaOptionsKey: map[string]any{settingsFieldEnv: "bad"}}},
			wantErr: "_meta.claude.options.env",
		},
		{
			name:    "env value not string",
			meta:    map[string]any{claudeMetaKey: map[string]any{metaOptionsKey: map[string]any{settingsFieldEnv: map[string]any{"A": 1}}}},
			wantErr: "_meta.claude.options.env.A",
		},
		{
			name:    "system prompt not string",
			meta:    map[string]any{claudeMetaKey: map[string]any{metaOptionsKey: map[string]any{metaSystemPromptKey: 1}}},
			wantErr: "_meta.claude.options.systemPrompt",
		},
		{
			name:    "model not string",
			meta:    map[string]any{claudeMetaKey: map[string]any{metaOptionsKey: map[string]any{metaModelKey: 1}}},
			wantErr: "_meta.claude.options.model",
		},
		{
			name:    "permission mode not string",
			meta:    map[string]any{claudeMetaKey: map[string]any{metaOptionsKey: map[string]any{metaPermissionModeKey: 1}}},
			wantErr: "_meta.claude.options.permissionMode",
		},
		{
			name:    "permission mode unsupported",
			meta:    map[string]any{claudeMetaKey: map[string]any{metaOptionsKey: map[string]any{metaPermissionModeKey: "invalid"}}},
			wantErr: "is not supported",
		},
		{
			name:    "schema not object",
			meta:    map[string]any{claudeMetaKey: map[string]any{metaOptionsKey: map[string]any{metaOutputSchemaKey: "bad"}}},
			wantErr: "_meta.claude.options.outputSchema",
		},
		{
			name:    "schema not serializable",
			meta:    map[string]any{claudeMetaKey: map[string]any{metaOptionsKey: map[string]any{metaOutputSchemaKey: map[string]any{"bad": func() {}}}}},
			wantErr: "must be JSON-serializable",
		},
		{
			name:    "blocked env key",
			meta:    map[string]any{claudeMetaKey: map[string]any{metaOptionsKey: map[string]any{settingsFieldEnv: map[string]any{"PATH": "/bin"}}}},
			wantErr: "is not allowed",
		},
		{
			name:    "invalid env key",
			meta:    map[string]any{claudeMetaKey: map[string]any{metaOptionsKey: map[string]any{settingsFieldEnv: map[string]any{"BAD-NAME": "x"}}}},
			wantErr: "is not a valid environment variable name",
		},
		{
			name:    "extra path dirs not array",
			meta:    map[string]any{claudeMetaKey: map[string]any{metaOptionsKey: map[string]any{metaExtraPathDirsKey: "/opt/bin"}}},
			wantErr: "_meta.claude.options.extraPathDirs",
		},
		{
			name:    "extra path dirs entry not string",
			meta:    map[string]any{claudeMetaKey: map[string]any{metaOptionsKey: map[string]any{metaExtraPathDirsKey: []any{"/opt/bin", 1}}}},
			wantErr: "_meta.claude.options.extraPathDirs[1]",
		},
		{
			name:    "extra path dirs entry relative",
			meta:    map[string]any{claudeMetaKey: map[string]any{metaOptionsKey: map[string]any{metaExtraPathDirsKey: []any{"/opt/bin", "relative/bin"}}}},
			wantErr: `_meta.claude.options.extraPathDirs[1] must be an absolute path: "relative/bin"`,
		},
		{
			name:    "extra path dirs entry empty",
			meta:    map[string]any{claudeMetaKey: map[string]any{metaOptionsKey: map[string]any{metaExtraPathDirsKey: []any{""}}}},
			wantErr: "must be an absolute path",
		},
		{
			name:    "unknown option",
			meta:    map[string]any{claudeMetaKey: map[string]any{metaOptionsKey: map[string]any{"extra": true}}},
			wantErr: "_meta.claude.options.extra",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := claudeOptionsFromMeta(tc.meta)
			require.ErrorContains(t, err, tc.wantErr)
		})
	}
}

func TestClaudeMetaSmallHelpers(t *testing.T) {
	t.Parallel()

	require.Equal(t, []string{"/a", "/b"}, sessionAdditionalDirectories([]string{"/a", "/b"}))
	require.True(t, blockedClaudeEnvKey("LD_PRELOAD"))
	require.True(t, blockedClaudeEnvKey("dyld_library_path"))
	require.True(t, blockedClaudeEnvKey(privateAdapterEnvPrefix+"TEST"))
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

	dirs, err := stringSliceOption([]string{"/a"}, metaExtraPathDirsKey)
	require.NoError(t, err)
	require.Equal(t, []string{"/a"}, dirs)

	dirs, err = stringSliceOption([]any{"/a", "/b"}, metaExtraPathDirsKey)
	require.NoError(t, err)
	require.Equal(t, []string{"/a", "/b"}, dirs)

	_, err = validateOutputSchema(map[string]any{})
	require.ErrorContains(t, err, "must be a non-empty object")
}
