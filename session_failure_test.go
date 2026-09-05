package claudeacp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/stretchr/testify/require"
)

func TestFailureBoundariesKeepSecretsOutAndCausalIdentityIn(t *testing.T) {
	const secret = "SECRET_SENTINEL_store_callback_panic_mirror_provider_path"
	cause := errors.New(secret)

	storeFailure := storeCommitError(cause)
	require.ErrorIs(t, storeFailure, cause)
	require.NotContains(t, storeFailure.Error(), secret)

	// The prompt-turn envelope is the one place native cause text is contractual:
	// `message` names the real provider cause so a host is not left guessing. What
	// stays out is everything else the frame carried — its subtype, its error
	// list, its structured output, and its raw body.
	const providerCause = "PROVIDER_CAUSE_rate_limit_exceeded"

	providerFailure := providerTurnFailure(&claude.ResultMessage{
		IsError: true, Error: providerCause, Result: secret, Subtype: secret,
		Errors: []string{secret}, StructuredOutput: map[string]any{"leak": secret},
		Raw: map[string]any{"leak": secret}, RawJSONText: secret,
	})
	require.Contains(t, providerFailure.Error(), providerCause)
	require.NotContains(t, providerFailure.Error(), secret)

	var providerWire *acp.RequestError
	require.ErrorAs(t, providerFailure, &providerWire)
	require.Equal(t, map[string]any{
		jsonFieldError:    turnFailedError,
		failureFieldCause: failureCauseProvider,
		jsonFieldMessage:  providerCause,
	}, providerWire.Data, "the envelope carries the cause and nothing else")

	deadlineFailure := errors.Join(context.DeadlineExceeded, cause)
	wireDeadline := requestError(context.Background(), deadlineFailure)
	require.Equal(t, "deadline", requireAnyMap(t, wireDeadline.Data)["class"])
	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	wireCancelled := requestError(cancelledCtx, errors.Join(context.Canceled, cause))
	require.Equal(t, -32800, wireCancelled.Code)

	wireStore := requestError(context.Background(), storeFailure)
	encoded, err := json.Marshal([]*acp.RequestError{wireDeadline, wireCancelled, wireStore})
	require.NoError(t, err)
	require.NotContains(t, string(encoded), secret)

	var logs bytes.Buffer
	log := slog.New(slog.NewTextHandler(&logs, nil))
	handleAgentGoroutinePanic(t.Context(), log, "secret-safe-test", nil, secret)
	mirror := newSessionMirror(log, NewInMemorySessionStore(), t.TempDir(), nil)
	require.NoError(t, mirror.appendFrame(t.Context(), &claude.TranscriptMirrorMessage{FilePath: secret}))
	require.NotContains(t, logs.String(), secret)
}

// requireTurnFailure asserts err is the uniform claude_turn_failed JSON-RPC
// error with the given code, cause, and (when non-empty) a message substring.
func requireTurnFailure(t *testing.T, err error, code int, cause string, messageContains string) map[string]any {
	t.Helper()

	require.Error(t, err)

	var reqErr *acp.RequestError
	require.ErrorAs(t, err, &reqErr)
	require.Equal(t, code, reqErr.Code)

	data, ok := reqErr.Data.(map[string]any)
	require.True(t, ok, "failure data must be a map")
	require.Equal(t, turnFailedError, data[jsonFieldError])
	require.Equal(t, cause, data[failureFieldCause])

	message, _ := data[jsonFieldMessage].(string)
	require.NotEmpty(t, message, "failure message must never be an empty placeholder")

	if messageContains != "" {
		require.Contains(t, message, messageContains)
	}

	return data
}

func TestNativeTurnFailureClassification(t *testing.T) {
	t.Parallel()

	require.Nil(t, nativeTurnFailure(nil))
	requireTurnFailure(t, nativeTurnFailure(context.Canceled), -32603, failureCauseTransport, context.Canceled.Error())
	requireTurnFailure(t, nativeTurnFailure(context.DeadlineExceeded), -32603, failureCauseTransport, context.DeadlineExceeded.Error())
	requireTurnFailure(t, nativeTurnFailure(claude.ErrProcessExited), -32603, failureCauseProcessExit, "claude exited")

	// The transport arm names the cause the adapter actually observed. The native
	// client reduces every failure it reports to an adapter-owned sentinel or a
	// closed process status before it gets here, so the text is a real cause
	// rather than a native payload.
	requireTurnFailure(t, nativeTurnFailure(errors.New("stdout reader failed")), -32603,
		failureCauseTransport, "stdout reader failed")

	// The constant survives only where there is no cause text at all.
	data := requireTurnFailure(t, nativeTurnFailure(errors.New("   ")), -32603,
		failureCauseTransport, nativeTransportFailureMessage)
	require.Equal(t, nativeTransportFailureMessage, data[jsonFieldMessage])
}

// TestProviderFailureMessageRecoversTheRealCause pins the recovery order the
// contract requires: the result frame's singular `error` field first, then its
// `result` text — the field Claude actually populates for an auth refusal — and
// the constant only when the frame names no cause at all.
func TestProviderFailureMessageRecoversTheRealCause(t *testing.T) {
	t.Parallel()

	require.Nil(t, providerTurnFailure(nil))

	for _, tc := range []struct {
		name   string
		result claude.ResultMessage
		want   string
	}{
		{"error field wins", claude.ResultMessage{IsError: true, Error: "upstream 429", Result: "ignored"}, "upstream 429"},
		{"result text is the fallback", claude.ResultMessage{IsError: true, Result: "context window exceeded"}, "context window exceeded"},
		{"whitespace is not a cause", claude.ResultMessage{IsError: true, Error: " \n ", Result: "real cause"}, "real cause"},
		{"no cause text at all", claude.ResultMessage{IsError: true}, providerFailureFallbackMessage},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			requireTurnFailure(t, providerTurnFailure(&tc.result), -32603, failureCauseProvider, tc.want)
		})
	}

	// A clean frame is no failure, and max_tokens settles as a stop reason.
	require.Nil(t, providerTurnFailure(&claude.ResultMessage{Result: "fine"}))
	require.Nil(t, providerTurnFailure(&claude.ResultMessage{IsError: true, StopReason: stopReasonMaxTokens}))
}

func TestReceiveTurnFailureRecordsUnexpectedNativeExitBeforeSanitizing(t *testing.T) {
	session := &agentSession{agent: NewAgent()}
	_, err := session.receiveTurnFailure(
		t.Context(), t.Context(), nil, claude.ErrMessageStreamClosed, false,
	)
	requireTurnFailure(t, err, -32603, failureCauseTransport, claude.ErrMessageStreamClosed.Error())
}

// A relaunch that fails to start surfaces as a transport failure at the next
// prompt; the session is never removed.
func TestEnsureClientAliveRelaunchError(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	var count int

	agent := NewAgent(WithHome(t.TempDir()), WithDefaultModel("sonnet"))
	agent.setConnection(newRecordingAgentClient())
	agent.newClaudeClient = func(log *slog.Logger, options claude.Options) *claude.Client {
		count++
		transport := newFakeClaudeTransport()
		if count >= 2 {
			transport.startErr = errors.New("relaunch failed")
		}

		return claude.NewClient(log, options, transport)
	}

	newResp, err := agent.NewSession(ctx, NewSessionRequest(t.TempDir()))
	require.NoError(t, err)

	sid := newResp.SessionId
	session := agent.sessions[sid]

	// Kill the first client so the next prompt must relaunch.
	require.NoError(t, session.client.Close())
	require.False(t, session.client.Alive())

	// The relaunch cause is the real cause of the failed turn, so it is named
	// rather than replaced by a placeholder.
	_, err = agent.Prompt(ctx, TextPromptRequest(sid, "test-turn", "hello"))
	requireTurnFailure(t, err, -32603, failureCauseTransport, "relaunch failed")
	require.Contains(t, agent.sessions, sid)
}

// T1 — a native provider error terminates the turn with a structured failure
// (never StopReason:end_turn); every provider error — including an auth failure
// — is -32603 with cause provider (this adapter advertises no ACP auth methods).
func TestTurnFailureProviderError(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	t.Run("rate limit is provider failure", func(t *testing.T) {
		t.Parallel()

		session, transport, cleanup := newPromptFlowSession(t)
		defer cleanup()

		// Provider payload text is classified but never surfaced.
		transport.queryMsgs = []map[string]any{{
			"type":     "result",
			"subtype":  "error",
			"is_error": true,
			"error":    "rate limit exceeded",
		}}

		resp, err := session.Prompt(ctx, TextPromptRequest(session.id, "test-turn", "hello"))
		require.Empty(t, resp.StopReason)
		data := requireTurnFailure(t, err, -32603, failureCauseProvider, "rate limit exceeded")

		// The failure data carries the cause and the uniform fields, nothing else.
		require.NotContains(t, data, jsonFieldSubtype)
		require.NotContains(t, data, "errors")
		require.NotContains(t, data, "errorKind")
	})

	t.Run("auth failure is provider failure", func(t *testing.T) {
		t.Parallel()

		session, transport, cleanup := newPromptFlowSession(t)
		defer cleanup()

		transport.queryMsgs = []map[string]any{{
			"type":   "result",
			"result": "Please run /login to authenticate",
		}}

		resp, err := session.Prompt(ctx, TextPromptRequest(session.id, "test-turn", "hello"))
		require.Empty(t, resp.StopReason)
		data := requireTurnFailure(t, err, -32603, failureCauseProvider, "Please run /login")

		// An empty subtype is omitted entirely rather than emitted as "", and the
		// login marker is text: it never derives a providerCredential member.
		require.NotContains(t, data, jsonFieldSubtype)
		require.NotContains(t, data, "providerCredential")
	})
}

// T2 — a severed turn transport reports the fixed sanitized classification.
func TestTurnFailureTransportRecoversCause(t *testing.T) {
	t.Parallel()

	session, transport, cleanup := newPromptFlowSession(t)
	defer cleanup()

	transport.queryMsgs = nil
	transport.onQuery = func() {
		transport.errs <- &claude.ProcessExitError{
			ExitCode: 1,
		}
	}

	_, err := session.Prompt(context.Background(), TextPromptRequest(session.id, "test-turn", "hello"))
	data := requireTurnFailure(t, err, -32603, failureCauseProcessExit, "claude exited with status 1")
	message, _ := data[jsonFieldMessage].(string)
	require.NotContains(t, message, "anthropic api")
	require.NotContains(t, message, "Please start a new session")
}

// T3 — process death mid-turn surfaces only the closed exit status, leaves the
// session addressable, and a follow-up prompt relaunches the native process.
func TestTurnFailureProcessDeathRelaunch(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	var transports []*fakeClaudeTransport

	agent := NewAgent(WithHome(t.TempDir()), WithDefaultModel("sonnet"))
	agent.setConnection(newRecordingAgentClient())
	agent.newClaudeClient = func(log *slog.Logger, options claude.Options) *claude.Client {
		transport := newFakeClaudeTransport()
		transports = append(transports, transport)

		return claude.NewClient(log, options, transport)
	}

	newResp, err := agent.NewSession(ctx, NewSessionRequest(t.TempDir()))
	require.NoError(t, err)

	sid := newResp.SessionId

	first := transports[len(transports)-1]
	first.queryMsgs = nil
	first.onQuery = func() {
		first.errs <- &claude.ProcessExitError{
			ExitCode: 9,
		}
	}

	_, err = agent.Prompt(ctx, TextPromptRequest(sid, "test-turn", "hello"))
	data := requireTurnFailure(t, err, -32603, failureCauseProcessExit, "claude exited with status 9")
	exitMessage, _ := data[jsonFieldMessage].(string)
	require.Contains(t, exitMessage, "status 9")

	// The session is never removed on a crash: it stays addressable and a
	// follow-up prompt does NOT return the unknown-session error — it relaunches
	// the native process lazily and completes.
	require.Contains(t, agent.sessions, sid)

	launchesBefore := len(transports)
	resp, err := agent.Prompt(ctx, TextPromptRequest(sid, "test-turn", "again"))
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, resp.StopReason)
	require.Greater(t, len(transports), launchesBefore, "the native process must be relaunched lazily")
}

// T4 — a malformed native frame fails the turn as a transport error. The
// abort closes the old native tree, then a follow-up prompt lazily relaunches
// Claude with the same native session id and succeeds.
func TestTurnFailureMalformedFrameRelaunches(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	session, transport, cleanup := newPromptFlowSession(t)
	defer cleanup()
	replacement := newFakeClaudeTransport()
	var replacementOptions claude.Options
	session.canRelaunch = true
	session.agent.newClaudeClient = func(log *slog.Logger, options claude.Options) *claude.Client {
		replacementOptions = options

		return claude.NewClient(log, options, replacement)
	}

	// An assistant frame without its message object cannot be parsed.
	transport.queryMsgs = []map[string]any{{"type": "assistant"}}

	_, err := session.Prompt(ctx, TextPromptRequest(session.id, "test-turn", "hello"))
	data := requireTurnFailure(t, err, -32603, failureCauseTransport, "")
	message, _ := data[jsonFieldMessage].(string)
	require.NotContains(t, message, "Please start a new session")

	// The ACP session survived the malformed frame: a normal turn relaunches and
	// resumes the native session instead of reusing the aborted process.
	replacement.queryMsgs = []map[string]any{{
		"type":        "result",
		"subtype":     "success",
		"is_error":    false,
		"stop_reason": "end_turn",
	}}

	resp, err := session.Prompt(ctx, TextPromptRequest(session.id, "test-turn", "again"))
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, resp.StopReason)
	require.Equal(t, string(session.id), replacementOptions.ResumeID)
	require.False(t, replacementOptions.ForkSession)
}

// T5 — a native error observed while the turn is cancelled maps to cancelled,
// never a failure error: the cancel guard runs before all failure mapping.
func TestTurnFailureCancelNotConflated(t *testing.T) {
	t.Parallel()

	session, transport, cleanup := newPromptFlowSession(t)
	defer cleanup()

	promptCtx, cancel := context.WithCancel(context.Background())
	transport.queryMsgs = nil
	transport.onQuery = func() {
		cancel()
		transport.errs <- errors.New("native boom")
	}

	resp, err := session.Prompt(promptCtx, TextPromptRequest(session.id, "test-turn", "hello"))
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonCancelled, resp.StopReason)
}

func TestCancelledPromptRejectsUnprovenNativeTreeSettlement(t *testing.T) {
	session, _, cleanup := newPromptFlowSession(t)
	defer cleanup()

	transport := newFakeClaudeTransport()
	client := claude.NewClient(nil, claude.Options{}, &closeErrTransport{
		Transport: transport,
		err:       errors.Join(errors.New("containment probe failed"), ErrContainmentIncomplete),
	})
	require.NoError(t, client.Start(t.Context()))
	session.client = client
	session.canRelaunch = true
	transport.queryMsgs = nil
	var cancelErr error
	transport.onQuery = func() {
		cancelErr = session.Cancel(t.Context())
	}

	_, err := session.Prompt(
		t.Context(),
		TextPromptRequest(session.id, "test-turn", "run the long tool"),
	)
	require.ErrorIs(t, cancelErr, ErrContainmentIncomplete)
	requireTurnFailure(t, err, -32603, failureCauseTransport, ErrContainmentIncomplete.Error())
	require.False(t, session.canRelaunch)
}

func TestTimeoutRejectsUnprovenNativeTreeSettlement(t *testing.T) {
	session, _, cleanup := newPromptFlowSession(t)
	defer cleanup()

	transport := newFakeClaudeTransport()
	client := claude.NewClient(nil, claude.Options{}, &closeErrTransport{
		Transport: transport,
		err:       errors.Join(errors.New("containment probe failed"), ErrContainmentIncomplete),
	})
	require.NoError(t, client.Start(t.Context()))
	session.client = client
	session.canRelaunch = true
	session.agent.options.TurnTimeout = time.Millisecond
	transport.queryMsgs = nil

	_, err := session.Prompt(
		t.Context(),
		TextPromptRequest(session.id, "test-turn", "run the long tool"),
	)
	requireTurnFailure(t, err, -32603, failureCauseTransport, ErrContainmentIncomplete.Error())
}

func TestParentCancelRejectsUnprovenNativeTreeSettlement(t *testing.T) {
	session, _, cleanup := newPromptFlowSession(t)
	defer cleanup()

	transport := newFakeClaudeTransport()
	client := claude.NewClient(nil, claude.Options{}, &closeErrTransport{
		Transport: transport,
		err:       errors.Join(errors.New("containment probe failed"), ErrContainmentIncomplete),
	})
	require.NoError(t, client.Start(t.Context()))
	session.client = client
	session.canRelaunch = true
	transport.queryMsgs = nil
	promptCtx, cancelPrompt := context.WithCancel(t.Context())
	transport.onQuery = cancelPrompt

	_, err := session.Prompt(
		promptCtx,
		TextPromptRequest(session.id, "test-turn", "run the long tool"),
	)
	requireTurnFailure(t, err, -32603, failureCauseTransport, ErrContainmentIncomplete.Error())
}

// T6 — with a turn deadline set, a silent-hang harness fails with cause timeout
// (never cancelled), and the native turn is aborted.
func TestTurnFailureTimeout(t *testing.T) {
	t.Parallel()

	session, transport, cleanup := newPromptFlowSession(t)
	defer cleanup()

	session.agent.options.TurnTimeout = 50 * time.Millisecond
	transport.queryMsgs = nil // harness hangs: no result, no error

	_, err := session.Prompt(context.Background(), TextPromptRequest(session.id, "test-turn", "hello"))
	requireTurnFailure(t, err, -32603, failureCauseTimeout, "")
	require.Equal(t, 1, transport.CloseCalls())
	require.False(t, session.client.Alive(), "timeout settled before the native client was closed")
}

// T7 — a user cancel and a WithTurnTimeout expiry that coincide resolve
// deterministically to cancelled: the cancel guard runs strictly before all
// failure mapping, so the turn is never reported as cause timeout and the
// timeout failure (a second native abort) never fires.
func TestTurnFailureCancelWinsOverCoincidentTimeout(t *testing.T) {
	t.Parallel()

	session, transport, cleanup := newPromptFlowSession(t)
	defer cleanup()

	session.agent.options.TurnTimeout = time.Millisecond
	transport.queryMsgs = nil // harness hangs so only the deadline/cancel end the turn

	// onQuery runs synchronously on the prompt goroutine before the receive loop.
	// Wait past the 1ms deadline so the timeout has provably expired, then fire a
	// real user cancel: both conditions now hold when the turn stream unblocks.
	transport.onQuery = func() {
		time.Sleep(25 * time.Millisecond)
		require.NoError(t, session.Cancel(context.Background()))
	}

	resp, err := session.Prompt(context.Background(), TextPromptRequest(session.id, "test-turn", "hello"))
	require.NoError(t, err, "coincident cancel must never surface a timeout failure")
	require.Equal(t, acp.StopReasonCancelled, resp.StopReason)
}
