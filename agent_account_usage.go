package claudeacp

import (
	"context"
	"encoding/json"
	"log/slog"
	"time"

	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/savid/acp-go-core/wire"
)

// internalClassAccountUsage is the claude_internal_failure class of a native
// account-usage read that failed.
const internalClassAccountUsage = "account_usage"

// accountUsage answers _claude/accountUsage through the addressed session. The
// request is decoded first so its trace keys open the span.
func (a *Agent) accountUsage(ctx context.Context, params json.RawMessage) (resp wire.AccountUsageResponse, err error) {
	request, refusal := wire.DecodeAccountUsageRequest(params, wire.AccountUsageScopeSession)

	ctx, finish := a.observe.StartACP(ctx, request.Meta, AccountUsageMethod)
	defer func() { finish(err) }()

	if refusal != nil {
		return wire.AccountUsageResponse{}, refusal
	}

	if request.ProviderID != "" {
		return wire.AccountUsageResponse{}, wire.Unsupported("providerId")
	}

	s, err := a.session(ctx, request.SessionID)
	if err != nil {
		return wire.AccountUsageResponse{}, err
	}

	return s.accountUsage(ctx)
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

	usage, err := rt.client.AccountUsage(readCtx)
	if err == nil {
		var response wire.AccountUsageResponse
		if response, err = accountUsageResponse(usage, time.Now()); err == nil {
			if response.Available {
				return response, nil
			}

			response, err = s.setupTokenUsage(readCtx, rt)
			if err == nil {
				return response, nil
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

	return wire.AccountUsageResponse{}, wire.InternalFailure(vendor, internalClassAccountUsage)
}

func accountUsageResponse(usage claude.AccountUsage, now time.Time) (wire.AccountUsageResponse, error) {
	if !usage.Available || usage.Windows == nil {
		return wire.AccountUsageUnavailable(wire.AccountUsageNotReported), nil
	}

	return usage.Windows.Observation().Response(usage.Plan, now)
}
