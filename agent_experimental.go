package claudeacp

import (
	"context"
	"errors"
	"sync"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/mapper"
)

// Logout clears auth state owned by this adapter.
func (a *Agent) Logout(ctx context.Context, _ acp.LogoutRequest) (acp.LogoutResponse, error) {
	sessions := a.clearGatewayAuthForLogout()

	var closeErrs []error

	errs := make(chan error, len(sessions))

	var wg sync.WaitGroup

	for _, session := range sessions {
		wg.Go(func() {
			defer recoverAgentGoroutine(ctx, a.log, "logout session close")

			_ = session.Cancel(ctx)
			if err := session.Close(ctx); err != nil {
				errs <- err
			}
		})
	}

	wg.Wait()
	close(errs)

	for err := range errs {
		closeErrs = append(closeErrs, err)
	}

	return acp.LogoutResponse{}, errors.Join(closeErrs...)
}

// UnstableAcceptNes records an accepted next-edit suggestion.
func (a *Agent) UnstableAcceptNes(_ context.Context, params acp.UnstableAcceptNesNotification) error {
	if err := params.Validate(); err != nil {
		return err
	}

	return a.recordNESDecision(params.SessionId, params.Id, nesDecisionAccepted, nil)
}

// UnstableCloseNes closes a next-edit suggestion session.
func (a *Agent) UnstableCloseNes(_ context.Context, params acp.UnstableCloseNesRequest) (acp.UnstableCloseNesResponse, error) {
	a.mu.Lock()
	session := a.nesSessions[params.SessionId]
	delete(a.nesSessions, params.SessionId)
	a.mu.Unlock()

	if session != nil {
		session.close()
	}

	a.docsMu.Lock()
	delete(a.documents, params.SessionId)
	delete(a.focusedDocuments, params.SessionId)
	a.docsMu.Unlock()

	return acp.UnstableCloseNesResponse{}, nil
}

// UnstableRejectNes records a rejected next-edit suggestion.
func (a *Agent) UnstableRejectNes(_ context.Context, params acp.UnstableRejectNesNotification) error {
	if err := params.Validate(); err != nil {
		return err
	}

	return a.recordNESDecision(params.SessionId, params.Id, nesDecisionRejected, params.Reason)
}

// UnstableStartNes starts a next-edit suggestion session.
func (a *Agent) UnstableStartNes(_ context.Context, params acp.UnstableStartNesRequest) (acp.UnstableStartNesResponse, error) {
	sessionID, err := newUUID()
	if err != nil {
		return acp.UnstableStartNesResponse{}, err
	}

	a.mu.Lock()
	a.nesSessions[acp.SessionId(sessionID)] = newNESSession(params)
	a.mu.Unlock()

	return acp.UnstableStartNesResponse{SessionId: acp.SessionId(sessionID)}, nil
}

// UnstableSuggestNes asks Claude to generate next-edit suggestions.
func (a *Agent) UnstableSuggestNes(ctx context.Context, params acp.UnstableSuggestNesRequest) (acp.UnstableSuggestNesResponse, error) {
	if err := params.Validate(); err != nil {
		return acp.UnstableSuggestNesResponse{}, err
	}

	session := a.nesSession(params.SessionId)
	if session == nil {
		return acp.UnstableSuggestNesResponse{}, acp.NewInvalidParams(map[string]any{acpFieldSessionID: params.SessionId})
	}

	suggestions, err := a.suggestNES(ctx, session, params)
	if err != nil {
		return acp.UnstableSuggestNesResponse{}, err
	}

	a.storeNESSuggestions(params.SessionId, suggestions)

	return acp.UnstableSuggestNesResponse{Suggestions: suggestions}, nil
}

// UnstableForkSession forks a Claude session and copies session permission rules.
func (a *Agent) UnstableForkSession(
	ctx context.Context,
	params acp.UnstableForkSessionRequest,
) (acp.UnstableForkSessionResponse, error) {
	metaOptions, err := claudeOptionsFromMeta(params.Meta)
	if err != nil {
		return acp.UnstableForkSessionResponse{}, acp.NewInvalidParams(map[string]any{jsonFieldError: err.Error()})
	}

	goalInput, err := parseGoalFromMeta(params.Meta)
	if err != nil {
		return acp.UnstableForkSessionResponse{}, acp.NewInvalidParams(map[string]any{jsonFieldError: err.Error()})
	}

	additionalDirectories := sessionAdditionalDirectories(params.AdditionalDirectories, metaOptions)
	if validationErr := validateSessionStartPaths(params.Cwd, additionalDirectories); validationErr != nil {
		return acp.UnstableForkSessionResponse{}, validationErr
	}

	mcpServers, err := mapper.StableMCPServers(params.McpServers)
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
		return acp.UnstableForkSessionResponse{}, err
	}

	session.persistPermissionRules(ctx)

	if err := a.storeStartedSession(ctx, session); err != nil {
		return acp.UnstableForkSessionResponse{}, err
	}

	session.applyStoredClientGoalInput(goalInput)

	if err := session.emitOptionalUpdates(ctx, mapper.AvailableCommandsUpdate(session.commands())); err != nil {
		a.removeSession(ctx, session.id, session)

		return acp.UnstableForkSessionResponse{}, err
	}

	session.emitCurrentUsageUpdate(ctx)

	return acp.UnstableForkSessionResponse{
		SessionId:     session.id,
		Meta:          sessionResponseMeta(session),
		ConfigOptions: sessionUnstableConfigOptions(session),
	}, nil
}
