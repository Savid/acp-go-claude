package claudeacp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/mapper"
)

var stableMCPServers = mapper.StableMCPServers

// Logout clears auth state owned by this adapter.
func (a *Agent) Logout(_ context.Context, params acp.LogoutRequest) (acp.LogoutResponse, error) {
	if err := rejectLifecycleMeta(params.Meta); err != nil {
		return acp.LogoutResponse{}, err
	}

	a.invalidateProviderObservations()

	return acp.LogoutResponse{}, nil
}

func (a *Agent) handleForkSession(
	ctx context.Context,
	raw json.RawMessage,
) (acp.UnstableForkSessionResponse, error) {
	// Params this route cannot decode, or that fail validation as a whole, are
	// refused by naming `params` itself. The Go decoder's prose is never the wire
	// answer: a caller reads which member it got wrong, not how this adapter
	// happens to spell the complaint.
	var params acp.UnstableForkSessionRequest
	if err := json.Unmarshal(raw, &params); err != nil {
		return acp.UnstableForkSessionResponse{}, unsupportedField(jsonFieldParams)
	}

	if err := params.Validate(); err != nil {
		return acp.UnstableForkSessionResponse{}, unsupportedField(jsonFieldParams)
	}

	metaOptions, err := claudeOptionsFromMetaWithProviderAuth(params.Meta, a.providerAuth != nil)
	if err != nil {
		return acp.UnstableForkSessionResponse{}, err
	}

	presence := configurationPresence(params.Meta)

	additionalDirectories := sessionAdditionalDirectories(params.AdditionalDirectories)
	if validationErr := validateSessionStartPaths(params.Cwd, additionalDirectories); validationErr != nil {
		return acp.UnstableForkSessionResponse{}, validationErr
	}

	mcpServers, err := stableMCPServers(params.McpServers)
	if err != nil {
		return acp.UnstableForkSessionResponse{}, unsupportedMCPServerField(err)
	}

	sessionID, err := newUUID()
	if err != nil {
		return acp.UnstableForkSessionResponse{}, err
	}

	permissionRules, err := a.permissionRulesForSession(ctx, params.SessionId)
	if err != nil {
		return acp.UnstableForkSessionResponse{}, err
	}

	var storeEntries []SessionStoreEntry

	a.mu.Lock()
	parent := a.sessions[params.SessionId]
	parentActive := parent != nil
	a.mu.Unlock()

	if parentActive {
		metaOptions = inheritSessionConfiguration(metaOptions, presence, parent.configuration)
	} else {
		stored, loadErr := a.storedSession(ctx, params.SessionId)
		if loadErr != nil {
			return acp.UnstableForkSessionResponse{}, loadErr
		}

		storeEntries = stored.Entries
		metaOptions = inheritSessionConfiguration(metaOptions, presence, stored.Configuration)
	}

	session, err := a.startAndStoreSession(ctx, acp.SessionId(sessionID), sessionStart{
		Cwd:                   params.Cwd,
		AdditionalDirectories: additionalDirectories,
		McpServers:            mcpServers,
		ResumeID:              string(params.SessionId),
		StoreEntries:          storeEntries,
		ActiveSessionResume:   parentActive,
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

	if err := session.emitCurrentUsageUpdate(ctx); err != nil {
		return acp.UnstableForkSessionResponse{}, err
	}

	return acp.UnstableForkSessionResponse{
		SessionId:     session.id,
		Meta:          sessionResponseMeta(session),
		ConfigOptions: sessionUnstableConfigOptions(session),
	}, nil
}

// unsupportedMCPServerField names the failing `mcpServers` entry when the mapper
// could identify one, and the whole member otherwise. Either way the answer is
// the uniform refusal rather than the mapper's own error text.
func unsupportedMCPServerField(err error) error {
	var unsupported *mapper.UnsupportedMCPServerError
	if errors.As(err, &unsupported) {
		return unsupportedField(fmt.Sprintf("mcpServers[%d]", unsupported.Index))
	}

	return unsupportedField("mcpServers")
}
