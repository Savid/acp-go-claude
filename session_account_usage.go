package claudeacp

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/savid/acp-go-core/usage"
	"github.com/savid/acp-go-core/usage/gateway"
	"github.com/savid/acp-go-core/wire"
)

func (s *session) setupTokenUsage(ctx context.Context, rt *runtime) (wire.AccountUsageResponse, error) {
	access, err := rt.client.QuotaAccess(ctx, rt.env, s.options.Bare)
	if err != nil {
		return wire.AccountUsageResponse{}, err
	}

	if access == nil {
		return wire.AccountUsageUnavailable(wire.AccountUsageNotReported), nil
	}

	s.mu.Lock()
	rt.quotaAccess = access
	s.mu.Unlock()

	result, err := s.agent.quota.Read(ctx, access.Key(), func(probeCtx context.Context, fable bool) (claude.QuotaResult, error) {
		dir, scratchErr := s.agent.scratchDir("quota")
		if scratchErr != nil {
			return claude.QuotaResult{}, scratchErr
		}

		result, probeErr := claude.ProbeQuota(probeCtx, *access, rt.executable, rt.env, dir, fable)

		var removeErr error
		if !errors.Is(probeErr, claude.ErrQuotaProcessActive) {
			removeErr = os.RemoveAll(dir)
		}

		if probeErr != nil || removeErr != nil {
			return result, errors.Join(probeErr, removeErr)
		}

		current, accessErr := rt.client.QuotaAccess(probeCtx, rt.env, s.options.Bare)
		if accessErr != nil {
			return claude.QuotaResult{}, accessErr
		}

		if current == nil || current.Key() != access.Key() {
			return claude.QuotaResult{}, errors.New("quota credentials changed")
		}

		return result, nil
	})
	if err != nil {
		return wire.AccountUsageResponse{}, err
	}

	return setupTokenUsageResponse(result)
}

func setupTokenUsageResponse(result claude.QuotaResult) (wire.AccountUsageResponse, error) {
	if result.NotAuthenticated {
		return wire.AccountUsageUnavailable(wire.AccountUsageNotAuthenticated), nil
	}

	if len(result.Windows) == 0 {
		return wire.AccountUsageUnavailable(wire.AccountUsageNotReported), nil
	}

	response := wire.AccountUsageResponse{Available: true}

	for _, w := range result.Windows {
		limit := wire.AccountUsageLimit{ID: w.ID, UsedPercent: w.Percent, ObservedAt: wire.AccountUsageTime(w.ObservedAt)}
		if !w.ResetsAt.IsZero() {
			limit.ResetsAt = wire.AccountUsageTime(w.ResetsAt)
		}

		if w.ID == claude.QuotaSession {
			limit.WindowSeconds = 18000
		} else {
			limit.WindowSeconds = 604800
		}

		if w.ID == claude.QuotaFable {
			limit.Label = claude.QuotaFableLabel
		}

		response.Limits = append(response.Limits, limit)
	}

	return response, response.Validate()
}

func (s *session) observeQuota(rt *runtime, event claude.Event) {
	s.mu.Lock()
	access := rt.quotaAccess
	model := s.model

	if event.Model != "" {
		rt.quotaModel = event.Model
	}

	if event.Message != nil && event.Message.Model != "" {
		rt.quotaModel = event.Message.Model
	}

	if model == "" || model == nativeDefault {
		model = rt.quotaModel
	}

	classified := rt.quotaClassified
	s.mu.Unlock()

	if access == nil {
		return
	}

	if event.Message != nil && event.Message.Model != "" {
		model = event.Message.Model
	}

	windows, exhausted := claude.QuotaEvent(event, strings.HasPrefix(model, "claude-fable-") || model == "fable", time.Now().UTC())
	if exhausted != "" {
		s.mu.Lock()
		rt.quotaClassified = true
		s.mu.Unlock()
	} else if !classified && (event.Error == "rate_limit" || event.Error == "rate_limit_error" || event.Error == "usage_limit") {
		exhausted = claude.QuotaUnknown
	}

	if len(windows) > 0 || exhausted != "" {
		s.agent.quota.Observe(access.Key(), windows, exhausted)
	}

	if event.Type == nativeResult {
		s.mu.Lock()
		rt.quotaClassified = false
		s.mu.Unlock()
	}
}

// gatewayUsage reads a provider claude holds no account for through the
// gateway ANTHROPIC_BASE_URL names, when it names one. The read holds the
// foreground gate like the native read so it never races a launch.
func (s *session) gatewayUsage(ctx context.Context, providerID string) (wire.AccountUsageResponse, error) {
	if err := s.admissionError(); err != nil {
		return wire.AccountUsageResponse{}, err
	}

	release, err := s.acquireGate(limitSessionPrompt)
	if err != nil {
		return wire.AccountUsageResponse{}, err
	}
	defer release()

	rt, err := s.ensureRuntime(ctx)
	if err != nil {
		return wire.AccountUsageResponse{}, err
	}

	readCtx, cancel := context.WithTimeout(ctx, wire.AccountUsageReadTimeout)
	defer cancel()

	response, err := gateway.ReadRoutes(readCtx, s.agent.usageTransport, func(context.Context) ([]gateway.Route, error) { return claude.GatewayRoutes(rt.env), nil }, providerID, wire.AccountUsageUnavailable(wire.AccountUsageNotAuthenticated))
	if err != nil {
		s.agent.log.ErrorContext(ctx, "claude account usage read failed",
			slog.String("session_id", string(s.id)), slog.String("reason", err.Error()))

		return wire.AccountUsageResponse{}, usage.RequestError(vendor, err)
	}

	return response, nil
}
