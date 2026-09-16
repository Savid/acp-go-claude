package claudeacp

import (
	"github.com/coder/acp-go-sdk"

	"github.com/savid/acp-go-core/wire"
)

// WithSessionClaudeOptions merges claude-specific options into _meta.claude.options.
func WithSessionClaudeOptions(options ClaudeOptions) wire.SessionRequestOption {
	return wire.WithSessionMetaValue(options.clone().Meta())
}

// WithSessionOutputSchema sets the native structured-output JSON schema. An
// empty schema is refused at session start.
func WithSessionOutputSchema(schema map[string]any) wire.SessionRequestOption {
	return wire.WithSessionMetaValue(ClaudeOptions{OutputSchema: wire.CloneMap(schema)}.Meta())
}

// WithSessionRawEvents toggles raw claude event emission for the session.
func WithSessionRawEvents(enabled bool) wire.SessionRequestOption {
	return wire.WithSessionMetaValue(map[string]any{
		vendor: map[string]any{metaRawEventKey: map[string]any{metaEnabledKey: enabled}},
	})
}

// SetModelRequest constructs a model selector update by native identifier.
func SetModelRequest(sessionID acp.SessionId, model string) acp.SetSessionConfigOptionRequest {
	return wire.SetConfigOptionRequest(sessionID, configModel, acp.SessionConfigValueId(model))
}
