package claudeacp

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestClaudeOptionsMetaAndParsing(t *testing.T) {
	t.Parallel()

	options := ClaudeOptions{
		Model:          "sonnet",
		Bare:           true,
		Env:            map[string]string{"ANTHROPIC_BASE_URL": "https://example.test"},
		OutputSchema:   map[string]any{"type": "object"},
		SystemPrompt:   "system",
		PermissionMode: permissionModeDontAsk,
	}
	meta := options.Meta()
	parsed, err := claudeOptionsFromMeta(meta)
	require.NoError(t, err)
	require.Equal(t, options, parsed)

	options.Env["ANTHROPIC_BASE_URL"] = "changed"
	options.OutputSchema["type"] = "changed"
	require.Equal(t, "https://example.test", parsed.Env["ANTHROPIC_BASE_URL"])
	require.Equal(t, "object", parsed.OutputSchema["type"])

	jsonSchema := outputSchemaJSONSchema(parsed.OutputSchema)
	jsonSchema["type"] = "changed-again"
	require.Equal(t, "object", parsed.OutputSchema["type"])
	require.Nil(t, outputSchemaJSONSchema(nil))
}

func TestClaudeOptionsValidationBranches(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name    string
		meta    map[string]any
		wantErr string
	}{
		{
			name:    "unknown claude field",
			meta:    map[string]any{claudeMetaKey: map[string]any{"extra": true}},
			wantErr: "_meta.claude.extra",
		},
		{
			name:    "raw event not object",
			meta:    map[string]any{claudeMetaKey: map[string]any{metaRawEventKey: true}},
			wantErr: "rawEvent must be an object",
		},
		{
			name: "raw event enabled not bool",
			meta: map[string]any{claudeMetaKey: map[string]any{
				metaRawEventKey: map[string]any{metaRawEventEnabledKey: "yes"},
			}},
			wantErr: "enabled must be a boolean",
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
			wantErr: "options must be an object",
		},
		{
			name:    "bare not bool",
			meta:    map[string]any{claudeMetaKey: map[string]any{metaOptionsKey: map[string]any{metaBareKey: "yes"}}},
			wantErr: "bare must be a boolean",
		},
		{
			name:    "env not object",
			meta:    map[string]any{claudeMetaKey: map[string]any{metaOptionsKey: map[string]any{settingsFieldEnv: "bad"}}},
			wantErr: "env must be an object",
		},
		{
			name:    "env value not string",
			meta:    map[string]any{claudeMetaKey: map[string]any{metaOptionsKey: map[string]any{settingsFieldEnv: map[string]any{"A": 1}}}},
			wantErr: "env.A must be a string",
		},
		{
			name:    "system prompt not string",
			meta:    map[string]any{claudeMetaKey: map[string]any{metaOptionsKey: map[string]any{metaSystemPromptKey: 1}}},
			wantErr: "systemPrompt must be a string",
		},
		{
			name:    "model not string",
			meta:    map[string]any{claudeMetaKey: map[string]any{metaOptionsKey: map[string]any{metaModelKey: 1}}},
			wantErr: "model must be a string",
		},
		{
			name:    "permission mode not string",
			meta:    map[string]any{claudeMetaKey: map[string]any{metaOptionsKey: map[string]any{metaPermissionModeKey: 1}}},
			wantErr: "permissionMode must be a string",
		},
		{
			name:    "permission mode unsupported",
			meta:    map[string]any{claudeMetaKey: map[string]any{metaOptionsKey: map[string]any{metaPermissionModeKey: "invalid"}}},
			wantErr: "is not supported",
		},
		{
			name:    "schema not object",
			meta:    map[string]any{claudeMetaKey: map[string]any{metaOptionsKey: map[string]any{metaOutputSchemaKey: "bad"}}},
			wantErr: "outputSchema must be an object",
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

	_, err = validateOutputSchema(map[string]any{})
	require.ErrorContains(t, err, "must be a non-empty object")
}
