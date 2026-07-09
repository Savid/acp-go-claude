package claudeacp

import (
	"bytes"
	"context"
	"encoding/json"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/savid/acp-go-claude/internal/mapper"
)

var stableMCPServers = mapper.StableMCPServers

// rateLimitsProbeTimeout bounds one `claude /usage` probe run.
const rateLimitsProbeTimeout = 60 * time.Second

// RateLimitsResponse is the `_claude/rateLimits` response payload. Windows is
// empty when the harness reports no subscription usage; values are only ever
// harness-reported.
type RateLimitsResponse struct {
	Windows  []RateLimitWindow `json:"windows"`
	PlanType string            `json:"planType,omitempty"`
}

// RateLimitWindow is one harness-reported subscription usage window.
type RateLimitWindow struct {
	// ID is the vendor-native window id, e.g. "session" or "week-all-models".
	ID string `json:"id"`
	// UsedPercent is the harness-reported percentage of the window consumed.
	UsedPercent float64 `json:"usedPercent"`
	// ResetsAt is the RFC3339 reset time, omitted when not reported.
	ResetsAt string `json:"resetsAt,omitempty"`
}

func (a *Agent) handleRateLimits(ctx context.Context, raw json.RawMessage) (RateLimitsResponse, error) {
	if err := validateEmptyParams(raw); err != nil {
		return RateLimitsResponse{}, acp.NewInvalidParams(map[string]any{jsonFieldError: err.Error()})
	}

	if err := a.ensureOpen(); err != nil {
		return RateLimitsResponse{}, err
	}

	claudeHome, err := canonicalClaudeHome(a.options.Home)
	if err != nil {
		return RateLimitsResponse{}, err
	}

	probeCtx, cancel := context.WithTimeout(ctx, rateLimitsProbeTimeout)
	defer cancel()

	limits, err := a.queryRateLimits(probeCtx, claude.Options{
		CLIPath:    a.options.ExecutablePath,
		ClaudeHome: claudeHome,
		Env:        a.options.Env,
	})
	if err != nil {
		return RateLimitsResponse{}, err
	}

	windows := make([]RateLimitWindow, 0, len(limits.Windows))
	for _, window := range limits.Windows {
		resetsAt := ""
		if !window.ResetsAt.IsZero() {
			resetsAt = window.ResetsAt.Format(time.RFC3339)
		}

		windows = append(windows, RateLimitWindow{
			ID:          window.ID,
			UsedPercent: window.UsedPercent,
			ResetsAt:    resetsAt,
		})
	}

	return RateLimitsResponse{Windows: windows}, nil
}

// validateEmptyParams accepts absent, null, or empty-object params and rejects
// everything else.
func validateEmptyParams(raw json.RawMessage) error {
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}

	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()

	var params struct{}

	return decoder.Decode(&params)
}

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
