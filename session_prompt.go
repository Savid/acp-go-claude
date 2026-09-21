package claudeacp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"time"

	"github.com/coder/acp-go-sdk"

	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/savid/acp-go-core/image"
	"github.com/savid/acp-go-core/lifecycle"
	"github.com/savid/acp-go-core/wire"
)

const (
	limitSessionPrompt = "session_prompt"

	stopReasonLength    = "length"
	stopReasonMaxTokens = "max_tokens"
	stopReasonAborted   = "aborted"
	stopReasonError     = "error"
)

// nativePrompt carries Anthropic content blocks in their request order.
type nativePrompt struct{ content []claude.ContentBlock }

func (s *session) mapPrompt(ctx context.Context, blocks []acp.ContentBlock) (nativePrompt, error) {
	if len(blocks) == 0 {
		return nativePrompt{}, wire.Unsupported("prompt")
	}

	decoded, refusal, err := image.ValidatePrompt(ctx, blocks, image.Options{Limits: s.agent.options.ImageLimits.core(), HandoffRoot: s.agent.options.InputHandoffRoot, Blobs: func(mime string) image.BlobDisposition {
		if mime == mimePDF {
			return image.BlobGate
		}

		return image.BlobRefuse
	}})
	if err != nil {
		return nativePrompt{}, err
	}

	if refusal != nil {
		return nativePrompt{}, refusal.InvalidParams()
	}

	media := make(map[int]image.Decoded, len(decoded))
	for _, item := range decoded {
		media[item.Block] = item
	}

	var prompt nativePrompt

	for position, block := range blocks {
		if item, isMedia := media[position]; isMedia {
			kind := contentBlockTypeImage
			if item.MIME == mimePDF {
				kind = "document"
			}

			prompt.content = append(prompt.content, claude.ContentBlock{Type: kind, Source: &claude.Source{Type: nativeBase64, MediaType: item.MIME, Data: base64.StdEncoding.EncodeToString(item.Data)}})

			continue
		}

		switch {
		case block.Text != nil:
			if !wire.AudienceIsUserOnly(block.Text.Annotations) && strings.TrimSpace(block.Text.Text) != "" {
				prompt.content = append(prompt.content, claude.ContentBlock{Type: contentBlockTypeText, Text: block.Text.Text})
			}
		case block.ResourceLink != nil:
			prompt.content = append(prompt.content, claude.ContentBlock{Type: contentBlockTypeText, Text: block.ResourceLink.Uri})
		case block.Resource != nil:
			if text := block.Resource.Resource.TextResourceContents; text != nil {
				prompt.content = append(prompt.content, claude.ContentBlock{Type: contentBlockTypeText, Text: wire.ContextResourceText(text.Uri, text.Text)})
			}
		default:
			return nativePrompt{}, wire.Unsupported("prompt")
		}
	}

	if len(prompt.content) == 0 {
		return nativePrompt{}, wire.Unsupported("prompt")
	}

	return prompt, nil
}

// prompt sends one turn to claude and streams updates until the run settles.
func (s *session) prompt(ctx context.Context, params acp.PromptRequest, raw json.RawMessage) (acp.PromptResponse, error) {
	meta := lifecycle.RetainRequestMetadata(params.Meta, raw)

	submission, paramErr := lifecycle.DecodePromptCorrelation(meta, s.lifecycleNegotiated())
	if paramErr != nil {
		return acp.PromptResponse{}, wire.ParamRefusal(paramErr)
	}

	if err := s.admissionError(); err != nil {
		return acp.PromptResponse{}, err
	}

	release, err := s.acquireGate(limitSessionPrompt)
	if err != nil {
		return acp.PromptResponse{}, err
	}
	defer release()

	// The turn runs under a context the session owns, so a peer request the SDK
	// refuses for this session never cancels the turn already in flight. Only
	// session/cancel, close, and a lost generation end it.
	turnCtx, cancelTurn := context.WithCancel(context.WithoutCancel(ctx))
	defer cancelTurn()

	t := &turn{
		cycle:      cycle{Cycle: lifecycle.Cycle{Origin: lifecycle.CauseSubmission}, state: cycleState{tools: make(map[string]*toolState)}},
		submission: submission,
		settled:    make(chan struct{}),
		finished:   make(chan struct{}),
		cancelCtx:  cancelTurn,
	}

	// The turn is installed before the request-scoped work a cancel has to be
	// able to interrupt, and the busy check shares its critical section so a
	// cycle the pump opens can neither be missed nor wedge the session.
	s.mu.Lock()
	if s.cycle != nil {
		s.mu.Unlock()

		return acp.PromptResponse{}, wire.Backpressure(limitSessionPrompt)
	}

	s.turn = t
	s.mu.Unlock()

	defer close(t.finished)

	defer func() {
		s.mu.Lock()
		if s.turn == t {
			s.turn = nil
		}
		s.mu.Unlock()
	}()

	mapped, err := s.mapPrompt(turnCtx, params.Prompt)
	if err != nil {
		if turnCtx.Err() != nil {
			return wire.CancelledResponse(params), nil
		}

		return acp.PromptResponse{}, err
	}

	// A cancel that lands before native dispatch creates neither submission nor
	// turn and answers cancelled.
	if turnCtx.Err() != nil {
		return wire.CancelledResponse(params), nil
	}

	rt, err := s.ensureRuntime(turnCtx)
	if err != nil {
		if turnCtx.Err() != nil {
			return wire.CancelledResponse(params), nil
		}

		return acp.PromptResponse{}, err
	}

	if err := rt.client.Prompt(turnCtx, s.nativeID, mapped.content); err != nil {
		if !s.turnAccepted(t) {
			if turnCtx.Err() != nil {
				return wire.CancelledResponse(params), nil
			}

			return acp.PromptResponse{}, s.dispatchFailure(context.WithoutCancel(ctx), rt, err)
		}
	}

	s.acceptTurn(turnCtx, t)

	select {
	case <-t.settled:
	case <-turnCtx.Done():
		select {
		case <-t.settled:
		case <-time.After(sessionAbortTimeout):
			// The native abort did not settle the run, so the generation ends
			// rather than leaving a live child bound to a fenced stream. The
			// pump joins this turn's terminal event, so only the child is
			// signalled here; settlement drops the binding.
			s.signalRuntime(context.WithoutCancel(ctx), rt)
			t.settle(turnTransportEnded)
		}
	}

	return s.settleTurn(ctx, rt, t, params)
}

// dispatchFailure classifies a prompt command claude never accepted: a native
// rejection carries its text as a provider failure, a dead child is a
// process exit, and everything else is transport.
func (s *session) dispatchFailure(ctx context.Context, rt *runtime, err error) error {
	var commandErr *claude.CommandError
	if errors.As(err, &commandErr) {
		return wire.TurnFailed(vendor, wire.TurnFailure{Cause: wire.CauseProvider, Message: commandErr.Message})
	}

	return s.transportFailure(ctx, rt, err)
}

// transportFailure recovers the real cause behind a lost native stream: the
// child's exit status and stderr tail where it died, otherwise the
// transport error.
func (s *session) transportFailure(ctx context.Context, rt *runtime, err error) error {
	return wire.TurnFailed(vendor, wire.TransportFailure(ctx, rt.proc, "claude process", err, rt.client.Err))
}

// cycleVerdict is how one cycle ended, in the terms the lifecycle stream and
// the prompt response need.
type cycleVerdict struct {
	outcome    lifecycle.Outcome
	stopReason string
	failure    error
}

// judgeCycle records how a natively settled cycle finished. The cancel guard
// runs before every failure mapping.
func (s *session) judgeCycle(c *cycle, cancelled bool) cycleVerdict {
	failure := s.cycleFailure(c)

	switch {
	case cancelled:
		return cycleVerdict{outcome: lifecycle.OutcomeCancelled, stopReason: lifecycle.StopReasonCancelled}
	case failure != nil:
		return cycleVerdict{outcome: lifecycle.OutcomeFailed, failure: failure}
	case c.state.stopReason == stopReasonError:
		message := strings.TrimSpace(c.state.errorMessage)
		if message == "" {
			message = "claude reported a turn error"
		}

		return cycleVerdict{outcome: lifecycle.OutcomeFailed, failure: wire.TurnFailed(vendor, wire.TurnFailure{Cause: wire.CauseProvider, Message: message})}
	}

	stop := acp.StopReasonEndTurn
	outcome := lifecycle.OutcomeSuccess

	switch c.state.stopReason {
	case stopReasonLength, stopReasonMaxTokens:
		stop = acp.StopReasonMaxTokens
		outcome = lifecycle.OutcomeLimit
	case stopReasonAborted:
		stop = acp.StopReasonCancelled
		outcome = lifecycle.OutcomeCancelled
	}

	return cycleVerdict{outcome: outcome, stopReason: string(stop)}
}

// settleTurn is the one settlement point every accepted prompt reaches:
// usage and session info, the durable mirror commit, the terminal idle, and
// only then the response or error.
func (s *session) settleTurn(ctx context.Context, rt *runtime, t *turn, params acp.PromptRequest) (acp.PromptResponse, error) {
	s.beginSettlement(&t.cycle)

	settleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionSettleTimeout)
	defer cancel()

	s.mu.Lock()
	cancelled := t.cancelled
	s.mu.Unlock()

	var verdict cycleVerdict

	commitFailed := false

	switch {
	case cancelled:
		verdict = cycleVerdict{outcome: lifecycle.OutcomeCancelled, stopReason: lifecycle.StopReasonCancelled}
	case t.ended == turnTransportEnded:
		verdict = cycleVerdict{outcome: lifecycle.OutcomeFailed, failure: s.transportFailure(settleCtx, rt, nil)}
	default:
		verdict = s.judgeCycle(&t.cycle, false)
	}

	if t.ended == turnSettled {
		if !cancelled {
			stats := s.settledStats(settleCtx, rt)
			s.emitUsage(settleCtx, &t.state, stats)
			s.emitSessionInfo(settleCtx, params.Prompt)
		}

		if err := s.commitMirror(settleCtx); err != nil {
			// The turn is not durable, so the incarnation is fenced and the
			// generation ends before this response is answered.
			s.fenceStream()
			s.signalRuntime(settleCtx, rt)
			s.dropRuntime(rt)

			commitFailed = true
			verdict.failure = s.mirrorFailure(&t.state, err)
			verdict.outcome = lifecycle.OutcomeFailed
		}
	}

	// A turn whose transport ended holds no durable commit, so the incarnation
	// is fenced before the idle rather than asserting a terminal state the store
	// does not back, and its generation is unbound before this response is
	// answered.
	if t.ended == turnTransportEnded {
		s.fenceStream()
		s.dropRuntime(rt)
	}

	if s.claimCancellation(&t.cycle) && !commitFailed {
		verdict = cycleVerdict{outcome: lifecycle.OutcomeCancelled, stopReason: lifecycle.StopReasonCancelled}
	}

	if err := s.lc.Idle(settleCtx, t.Cycle, verdict.stopReason, verdict.outcome); err != nil && verdict.failure == nil {
		verdict.failure = err
	}

	if verdict.failure != nil {
		return acp.PromptResponse{}, verdict.failure
	}

	return acp.PromptResponse{
		StopReason:    acp.StopReason(verdict.stopReason),
		Usage:         t.state.usage,
		UserMessageId: params.MessageId,
	}, nil
}

func (s *session) settledStats(ctx context.Context, rt *runtime) *claude.ContextUsage {
	stats, err := rt.client.ContextUsage(ctx)
	if err != nil {
		return nil
	}

	return &stats
}

// mirrorFailure maps a failed mirror commit onto the turn-failure shape. A
// turn that delivered image bytes lost their durable replay representation.
func (s *session) mirrorFailure(state *cycleState, err error) error {
	s.agent.log.Error("session mirror commit failed", slog.String("session_id", string(s.id)), slog.String("reason", err.Error()))

	failure := wire.TurnFailure{Cause: wire.CauseTransport, Message: "session mirror commit failed"}
	if state.imagesEmitted {
		failure.Message = "image output is no longer available from the artifact store"
		failure.Stage = image.OutputStage
		failure.Reason = image.ReasonStorageFailed
	}

	return wire.TurnFailed(vendor, failure)
}
