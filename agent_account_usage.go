package claudeacp

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"time"

	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/savid/acp-go-core/usage"
	"github.com/savid/acp-go-core/usage/anthropic"
	"github.com/savid/acp-go-core/usage/gateway"
	"github.com/savid/acp-go-core/usage/openaicodex"
	"github.com/savid/acp-go-core/usage/opencodego"
	"github.com/savid/acp-go-core/usage/openrouter"
	"github.com/savid/acp-go-core/wire"
)

// accountUsage answers _claude/accountUsage through the addressed session. The
// request is decoded first so its trace keys open the span.
func (a *Agent) accountUsage(ctx context.Context, params json.RawMessage) (resp wire.AccountUsageResponse, err error) {
	request, refusal := wire.DecodeAccountUsageRequest(params, wire.AccountUsageScopeSession)

	ctx, finish := a.observe.StartACP(ctx, request.Meta, AccountUsageMethod)
	defer func() { finish(err) }()

	if refusal != nil {
		return wire.AccountUsageResponse{}, refusal
	}

	s, err := a.session(ctx, request.SessionID)
	if err != nil {
		return wire.AccountUsageResponse{}, err
	}

	switch request.ProviderID {
	case "":
		return wire.AccountUsageResponse{}, wire.Missing("providerId")
	case anthropic.ProviderID:
		return s.accountUsage(ctx)
	case openaicodex.ProviderID, opencodego.ProviderID, openrouter.ProviderID:
		return s.gatewayUsage(ctx, request.ProviderID)
	default:
		return wire.AccountUsageResponse{}, wire.Unsupported("providerId")
	}
}

// accountUsage reads the subscription allowance through the session's claude
// process. It holds the foreground gate for the whole read, so it never
// addresses a process another operation is still configuring, and a read that
// arrives while a prompt, config change, restore, or read holds the gate is
// refused with backpressure.
func (s *session) accountUsage(ctx context.Context) (wire.AccountUsageResponse, error) {
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

	report, err := rt.client.AccountUsage(readCtx)
	if err == nil {
		var response wire.AccountUsageResponse

		response, err = accountUsageResponse(report, time.Now())
		if errors.Is(err, errNativeUsageUnread) && !s.processReported(rt) {
			// The process has completed no turn, so it has no report yet.
			response, err = wire.AccountUsageUnavailable(wire.AccountUsageNotReported), nil
		}

		if err == nil || errors.Is(err, errNativeUsageUnread) {
			if response.Available {
				return response, nil
			}

			// A setup token can still supply windows through the probe. Without
			// one, a native report that failed to arrive stays a failure, so the
			// host retries it instead of recording an account without allowance.
			unread := err

			response, err = s.setupTokenUsage(readCtx, rt)
			if err == nil && !response.Available && unread == nil {
				response, err = gateway.ReadRoutes(readCtx, s.agent.usageTransport, func(context.Context) ([]gateway.Route, error) { return claude.GatewayRoutes(rt.env), nil }, anthropic.ProviderID, response)
			}

			if err == nil && (response.Available || unread == nil) {
				return response, nil
			}

			if err == nil {
				err = unread
			}
		}
	}

	// A close or poisoning that raced the read ended the process under it; the
	// session's state, not the read, is the answer.
	if admission := s.admissionError(); admission != nil {
		return wire.AccountUsageResponse{}, admission
	}

	s.agent.log.ErrorContext(ctx, "claude account usage read failed",
		slog.String("session_id", string(s.id)), slog.String("reason", err.Error()))

	return wire.AccountUsageResponse{}, usage.RequestError(vendor, err)
}

// errNativeUsageUnread marks an account that reports allowances whose report
// is absent. After a completed turn that is a failed read, not an account
// without allowance; before one the process simply has no report yet.
var errNativeUsageUnread = errors.New("native usage report missing")

func accountUsageResponse(report claude.AccountUsage, now time.Time) (wire.AccountUsageResponse, error) {
	if !report.Available {
		return wire.AccountUsageUnavailable(wire.AccountUsageNotReported), nil
	}

	if report.Windows == nil {
		return wire.AccountUsageResponse{}, errNativeUsageUnread
	}

	return report.Windows.Observation().Response(report.Plan, now)
}
