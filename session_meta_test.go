package claudeacp

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestClaudeOptionsMeta(t *testing.T) {
	t.Parallel()

	options := ClaudeOptions{
		Bare:                  true,
		Env:                   map[string]string{"A": "1"},
		SystemPrompt:          "system",
		Model:                 "opus",
		PermissionMode:        permissionModeDontAsk,
		AdditionalDirectories: []string{"/extra"},
		OutputFormat: &ClaudeOutputFormat{
			Type: ClaudeOutputFormatJSONSchema,
			Schema: map[string]any{
				"type": map[string]any{"nested": true},
			},
		},
	}

	meta := options.Meta()
	options.Env["A"] = "changed"
	options.AdditionalDirectories[0] = "/changed"
	options.OutputFormat.Schema["type"] = "changed"

	claude, ok := meta[claudeMetaKey].(map[string]any)
	require.True(t, ok)
	rawOptions, ok := claude[metaOptionsKey].(map[string]any)
	require.True(t, ok)
	require.Equal(t, true, rawOptions[metaBareKey])
	require.Equal(t, map[string]string{"A": "1"}, rawOptions[settingsFieldEnv])
	require.Equal(t, "system", rawOptions[metaSystemPromptKey])
	require.Equal(t, "opus", rawOptions[metaModelKey])
	require.Equal(t, permissionModeDontAsk, rawOptions[metaPermissionModeKey])
	require.Equal(t, []string{"/extra"}, rawOptions[metaAdditionalDirectoriesKey])
	require.Equal(t, map[string]any{
		metaOutputFormatTypeKey: ClaudeOutputFormatJSONSchema,
		metaOutputFormatSchemaKey: map[string]any{
			"type": map[string]any{"nested": true},
		},
	}, rawOptions[metaOutputFormatKey])
}

func TestClaudeOptionsFromMeta(t *testing.T) {
	t.Parallel()

	meta := map[string]any{
		claudeMetaKey: map[string]any{
			metaOptionsKey: map[string]any{
				metaBareKey: true,
				settingsFieldEnv: map[string]any{
					"SESSION": "1",
				},
				metaSystemPromptKey:          "session system",
				metaModelKey:                 "sonnet",
				metaPermissionModeKey:        permissionModeAcceptEdits,
				metaAdditionalDirectoriesKey: []any{"/option-root"},
				metaOutputFormatKey: map[string]any{
					metaOutputFormatTypeKey: ClaudeOutputFormatJSONSchema,
					metaOutputFormatSchemaKey: map[string]any{
						"type": "object",
					},
				},
			},
		},
	}

	options, err := claudeOptionsFromMeta(meta)
	require.NoError(t, err)
	require.True(t, options.Bare)
	require.Equal(t, map[string]string{"SESSION": "1"}, options.Env)
	require.Equal(t, "session system", options.SystemPrompt)
	require.Equal(t, "sonnet", options.Model)
	require.Equal(t, permissionModeAcceptEdits, options.PermissionMode)
	require.Equal(t, []string{"/option-root"}, options.AdditionalDirectories)
	require.Equal(t, &ClaudeOutputFormat{
		Type:   ClaudeOutputFormatJSONSchema,
		Schema: map[string]any{"type": "object"},
	}, options.OutputFormat)
	require.Equal(t, []string{"/primary", "/option-root"}, sessionAdditionalDirectories([]string{"/primary"}, options))
	require.Equal(t, map[string]any{"type": "object"}, outputFormatJSONSchema(options.OutputFormat))
	require.Nil(t, outputFormatJSONSchema(nil))
	require.Nil(t, outputFormatJSONSchema(&ClaudeOutputFormat{Type: "other", Schema: map[string]any{"type": "object"}}))
}

func TestClaudeOptionsMetaRoundTrip(t *testing.T) {
	t.Parallel()

	options, err := claudeOptionsFromMeta(ClaudeOptions{
		Bare:                  true,
		Env:                   map[string]string{"FROM_META": "1"},
		SystemPrompt:          "meta helper system",
		Model:                 "opus",
		PermissionMode:        permissionModeDontAsk,
		AdditionalDirectories: []string{"/helper"},
		OutputFormat: &ClaudeOutputFormat{
			Type:   ClaudeOutputFormatJSONSchema,
			Schema: map[string]any{"type": "object"},
		},
	}.Meta())
	require.NoError(t, err)
	require.True(t, options.Bare)
	require.Equal(t, map[string]string{"FROM_META": "1"}, options.Env)
	require.Equal(t, "meta helper system", options.SystemPrompt)
	require.Equal(t, "opus", options.Model)
	require.Equal(t, permissionModeDontAsk, options.PermissionMode)
	require.Equal(t, []string{"/helper"}, options.AdditionalDirectories)
	require.Equal(t, map[string]any{"type": "object"}, options.OutputFormat.Schema)
}

func TestSessionMetaIgnoresTopLevelSystemPrompt(t *testing.T) {
	t.Parallel()

	options, err := claudeOptionsFromMeta(map[string]any{
		metaSystemPromptKey: "top-level system",
	})
	require.NoError(t, err)
	require.Empty(t, options.SystemPrompt)
}

func TestClaudeOptionsRejectUnsupportedOrInvalidValues(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name string
		meta map[string]any
		want string
	}{
		{
			name: "options must be object",
			meta: map[string]any{
				claudeMetaKey: map[string]any{metaOptionsKey: "bad"},
			},
			want: "_meta.claude.options must be an object",
		},
		{
			name: "unknown key",
			meta: map[string]any{
				claudeMetaKey: map[string]any{
					metaOptionsKey: map[string]any{"cwd": "/repo"},
				},
			},
			want: "_meta.claude.options.cwd is not supported",
		},
		{
			name: "bare must be boolean",
			meta: map[string]any{
				claudeMetaKey: map[string]any{
					metaOptionsKey: map[string]any{metaBareKey: "yes"},
				},
			},
			want: "_meta.claude.options.bare must be a boolean",
		},
		{
			name: "env must be object",
			meta: map[string]any{
				claudeMetaKey: map[string]any{
					metaOptionsKey: map[string]any{settingsFieldEnv: "bad"},
				},
			},
			want: "_meta.claude.options.env must be an object",
		},
		{
			name: "env values must be strings",
			meta: map[string]any{
				claudeMetaKey: map[string]any{
					metaOptionsKey: map[string]any{settingsFieldEnv: map[string]any{"A": 1}},
				},
			},
			want: "_meta.claude.options.env.A must be a string",
		},
		{
			name: "env key must be valid",
			meta: map[string]any{
				claudeMetaKey: map[string]any{
					metaOptionsKey: map[string]any{settingsFieldEnv: map[string]any{"bad-key": "1"}},
				},
			},
			want: "_meta.claude.options.env.bad-key is not a valid environment variable name",
		},
		{
			name: "env key must not be blocked",
			meta: map[string]any{
				claudeMetaKey: map[string]any{
					metaOptionsKey: map[string]any{settingsFieldEnv: map[string]any{"LD_PRELOAD": "libhook.so"}},
				},
			},
			want: "_meta.claude.options.env.LD_PRELOAD is not allowed",
		},
		{
			name: "env key must not override path",
			meta: map[string]any{
				claudeMetaKey: map[string]any{
					metaOptionsKey: map[string]any{settingsFieldEnv: map[string]any{"PATH": "/tmp/bin"}},
				},
			},
			want: "_meta.claude.options.env.PATH is not allowed",
		},
		{
			name: "system prompt must be string",
			meta: map[string]any{
				claudeMetaKey: map[string]any{
					metaOptionsKey: map[string]any{metaSystemPromptKey: []any{"bad"}},
				},
			},
			want: "_meta.claude.options.systemPrompt must be a string",
		},
		{
			name: "model must be string",
			meta: map[string]any{
				claudeMetaKey: map[string]any{
					metaOptionsKey: map[string]any{metaModelKey: true},
				},
			},
			want: "_meta.claude.options.model must be a string",
		},
		{
			name: "permission mode must be string",
			meta: map[string]any{
				claudeMetaKey: map[string]any{
					metaOptionsKey: map[string]any{metaPermissionModeKey: true},
				},
			},
			want: "_meta.claude.options.permissionMode must be a string",
		},
		{
			name: "permission mode must be known",
			meta: map[string]any{
				claudeMetaKey: map[string]any{
					metaOptionsKey: map[string]any{metaPermissionModeKey: "danger"},
				},
			},
			want: "_meta.claude.options.permissionMode is not supported: danger",
		},
		{
			name: "additional directories must be array",
			meta: map[string]any{
				claudeMetaKey: map[string]any{
					metaOptionsKey: map[string]any{metaAdditionalDirectoriesKey: "/repo"},
				},
			},
			want: "_meta.claude.options.additionalDirectories must be an array",
		},
		{
			name: "additional directory values must be strings",
			meta: map[string]any{
				claudeMetaKey: map[string]any{
					metaOptionsKey: map[string]any{metaAdditionalDirectoriesKey: []any{1}},
				},
			},
			want: "_meta.claude.options.additionalDirectories[0] must be a string",
		},
		{
			name: "output format must be object",
			meta: map[string]any{
				claudeMetaKey: map[string]any{
					metaOptionsKey: map[string]any{metaOutputFormatKey: "bad"},
				},
			},
			want: "_meta.claude.options.outputFormat must be an object",
		},
		{
			name: "output format type must be string",
			meta: map[string]any{
				claudeMetaKey: map[string]any{
					metaOptionsKey: map[string]any{metaOutputFormatKey: map[string]any{
						metaOutputFormatTypeKey: true,
					}},
				},
			},
			want: "_meta.claude.options.outputFormat.type must be a string",
		},
		{
			name: "output format schema must be object",
			meta: map[string]any{
				claudeMetaKey: map[string]any{
					metaOptionsKey: map[string]any{metaOutputFormatKey: map[string]any{
						metaOutputFormatTypeKey:   ClaudeOutputFormatJSONSchema,
						metaOutputFormatSchemaKey: []any{},
					}},
				},
			},
			want: "_meta.claude.options.outputFormat.schema must be an object",
		},
		{
			name: "output format unknown key",
			meta: map[string]any{
				claudeMetaKey: map[string]any{
					metaOptionsKey: map[string]any{metaOutputFormatKey: map[string]any{
						metaOutputFormatTypeKey: "json_schema",
						"extra":                 true,
					}},
				},
			},
			want: "_meta.claude.options.outputFormat.extra is not supported",
		},
		{
			name: "output format type must be known",
			meta: map[string]any{
				claudeMetaKey: map[string]any{
					metaOptionsKey: map[string]any{metaOutputFormatKey: map[string]any{
						metaOutputFormatTypeKey:   "other",
						metaOutputFormatSchemaKey: map[string]any{"type": "object"},
					}},
				},
			},
			want: "_meta.claude.options.outputFormat.type is not supported: other",
		},
		{
			name: "output format schema must be non empty",
			meta: map[string]any{
				claudeMetaKey: map[string]any{
					metaOptionsKey: map[string]any{metaOutputFormatKey: map[string]any{
						metaOutputFormatTypeKey:   ClaudeOutputFormatJSONSchema,
						metaOutputFormatSchemaKey: map[string]any{},
					}},
				},
			},
			want: "_meta.claude.options.outputFormat.schema must be a non-empty object",
		},
		{
			name: "output format schema must marshal",
			meta: map[string]any{
				claudeMetaKey: map[string]any{
					metaOptionsKey: map[string]any{metaOutputFormatKey: map[string]any{
						metaOutputFormatTypeKey:   ClaudeOutputFormatJSONSchema,
						metaOutputFormatSchemaKey: map[string]any{"bad": func() {}},
					}},
				},
			},
			want: "_meta.claude.options.outputFormat.schema must be JSON-serializable: json: unsupported type: func()",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			_, err := claudeOptionsFromMeta(tc.meta)
			require.EqualError(t, err, tc.want)
		})
	}
}

func TestValidateClaudeOptionsAllowsSupportedPermissionModes(t *testing.T) {
	t.Parallel()

	for _, mode := range []string{
		string(modeDefault),
		permissionModeAcceptEdits,
		permissionModeBypassPermissions,
		string(modePlan),
		permissionModeDontAsk,
		string(modeAuto),
	} {
		t.Run(mode, func(t *testing.T) {
			t.Parallel()

			_, err := validateClaudeOptions(ClaudeOptions{PermissionMode: mode})
			require.NoError(t, err)
		})
	}
}
