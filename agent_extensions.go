package claudeacp

import (
	"context"
	"encoding/json"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/mapper"
)

var stableMCPServers = mapper.StableMCPServers

// Logout clears auth state owned by this adapter.
func (a *Agent) Logout(_ context.Context, _ acp.LogoutRequest) (acp.LogoutResponse, error) {
	return acp.LogoutResponse{}, nil
}

func (a *Agent) handleForkSession(
	ctx context.Context,
	raw json.RawMessage,
) (acp.UnstableForkSessionResponse, error) {
	var params acp.UnstableForkSessionRequest
	if err := json.Unmarshal(raw, &params); err != nil {
		return acp.UnstableForkSessionResponse{}, acp.NewInvalidParams(map[string]any{jsonFieldError: err.Error()})
	}

	if err := params.Validate(); err != nil {
		return acp.UnstableForkSessionResponse{}, acp.NewInvalidParams(map[string]any{jsonFieldError: err.Error()})
	}

	metaOptions, err := claudeOptionsFromMeta(params.Meta)
	if err != nil {
		return acp.UnstableForkSessionResponse{}, lifecycleMetaError(err)
	}

	additionalDirectories := sessionAdditionalDirectories(params.AdditionalDirectories)
	if validationErr := validateSessionStartPaths(params.Cwd, additionalDirectories); validationErr != nil {
		return acp.UnstableForkSessionResponse{}, validationErr
	}

	mcpServers, err := stableMCPServers(params.McpServers)
	if err != nil {
		return acp.UnstableForkSessionResponse{}, acp.NewInvalidParams(map[string]any{jsonFieldError: err.Error()})
	}

	sessionID, err := newUUID()
	if err != nil {
		return acp.UnstableForkSessionResponse{}, err
	}

	permissionRules, err := a.permissionRulesForSession(ctx, params.SessionId)
	if err != nil {
		return acp.UnstableForkSessionResponse{}, err
	}

	if openErr := a.ensureOpen(); openErr != nil {
		return acp.UnstableForkSessionResponse{}, openErr
	}

	session, err := a.startSession(ctx, acp.SessionId(sessionID), sessionStart{
		Cwd:                   params.Cwd,
		AdditionalDirectories: additionalDirectories,
		McpServers:            mcpServers,
		ResumeID:              string(params.SessionId),
		ForkSession:           true,
		PermissionRules:       permissionRules,
		MetaOptions:           metaOptions,
		RawMessages:           rawMessageConfigFromMeta(params.Meta),
	})
	if err != nil {
		// Forking an unknown or deleted parent returns the same uniform
		// unknown-session error as every other session-scoped request method,
		// matching resume and load.
		if missingClaudeSessionError(err) {
			return acp.UnstableForkSessionResponse{}, unknownSessionError()
		}

		return acp.UnstableForkSessionResponse{}, err
	}

	session.persistPermissionRules(ctx)

	if err := a.storeStartedSession(ctx, session); err != nil {
		return acp.UnstableForkSessionResponse{}, err
	}

	session.emitCurrentUsageUpdate(ctx)

	return acp.UnstableForkSessionResponse{
		SessionId:     session.id,
		Meta:          sessionResponseMeta(session),
		ConfigOptions: sessionUnstableConfigOptions(session),
	}, nil
}
