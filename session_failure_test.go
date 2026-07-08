package claudeacp

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/stretchr/testify/require"
)

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
	requireTurnFailure(t, nativeTurnFailure(claude.ErrProcessExited), -32603, failureCauseProcessExit, "claude exited")
	requireTurnFailure(t, nativeTurnFailure(errors.New("weird")), -32603, failureCauseTransport, "weird")
}

func TestProviderErrorMessageFallbacks(t *testing.T) {
	t.Parallel()

	require.Equal(t, "result text", providerErrorMessage(&claude.ResultMessage{Result: "result text"}))
	require.Equal(t, "singular error", providerErrorMessage(&claude.ResultMessage{Error: "singular error"}))
	require.Equal(t, "a; b", providerErrorMessage(&claude.ResultMessage{Errors: []string{"a", "b"}}))
	require.Equal(t, "sub", providerErrorMessage(&claude.ResultMessage{Subtype: "sub"}))
	require.Equal(t, "claude reported a turn error", providerErrorMessage(&claude.ResultMessage{}))
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

	_, err = agent.Prompt(ctx, TextPromptRequest(sid, "hello"))
	requireTurnFailure(t, err, -32603, failureCauseTransport, "relaunch failed")
	require.Contains(t, agent.sessions, sid)
}

// T1 — a native provider error terminates the turn with a structured failure
// (never StopReason:end_turn); every provider error — including an auth failure
// — is -32603 with cause provider (this sibling advertises no ACP auth methods).
func TestTurnFailureProviderError(t *testing.T) {
	t.Parallel()

	ctx := context.Background()

	t.Run("rate limit is provider failure", func(t *testing.T) {
		t.Parallel()

		session, transport, cleanup := newPromptFlowSession(t)
		defer cleanup()

		// The result frame carries the cause in the singular error field with an
		// empty result; it must still be surfaced.
		transport.queryMsgs = []map[string]any{{
			"type":     "result",
			"subtype":  "error",
			"is_error": true,
			"error":    "rate limit exceeded",
		}}

		resp, err := session.Prompt(ctx, TextPromptRequest(session.id, "hello"))
		require.Empty(t, resp.StopReason)
		requireTurnFailure(t, err, -32603, failureCauseProvider, "rate limit exceeded")
	})

	t.Run("auth failure is provider failure", func(t *testing.T) {
		t.Parallel()

		session, transport, cleanup := newPromptFlowSession(t)
		defer cleanup()

		transport.queryMsgs = []map[string]any{{
			"type":   "result",
			"result": "Please run /login to authenticate",
		}}

		resp, err := session.Prompt(ctx, TextPromptRequest(session.id, "hello"))
		require.Empty(t, resp.StopReason)
		requireTurnFailure(t, err, -32603, failureCauseProvider, "Please run /login")
	})
}

// T2 — a severed turn transport recovers and surfaces the real cause, never a
// bare EOF or the fixed "start a new session" placeholder.
func TestTurnFailureTransportRecoversCause(t *testing.T) {
	t.Parallel()

	session, transport, cleanup := newPromptFlowSession(t)
	defer cleanup()

	transport.queryMsgs = nil
	transport.onQuery = func() {
		transport.errs <- &claude.ProcessExitError{
			ExitCode:   1,
			StderrTail: "anthropic api: connection reset by peer",
			Err:        errors.New("read: connection reset"),
		}
	}

	_, err := session.Prompt(context.Background(), TextPromptRequest(session.id, "hello"))
	data := requireTurnFailure(t, err, -32603, failureCauseProcessExit, "anthropic api: connection reset by peer")
	message, _ := data[jsonFieldMessage].(string)
	require.NotContains(t, message, "Please start a new session")
}

// T3 — process death mid-turn surfaces the exit status and stderr tail, leaves
// the session addressable, and a follow-up prompt relaunches the native process.
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
			ExitCode:   9,
			StderrTail: "fatal: out of memory",
			Err:        errors.New("signal: killed"),
		}
	}

	_, err = agent.Prompt(ctx, TextPromptRequest(sid, "hello"))
	data := requireTurnFailure(t, err, -32603, failureCauseProcessExit, "fatal: out of memory")
	exitMessage, _ := data[jsonFieldMessage].(string)
	require.Contains(t, exitMessage, "status 9")

	// The session is never removed on a crash: it stays addressable and a
	// follow-up prompt does NOT return the unknown-session error — it relaunches
	// the native process lazily and completes.
	require.Contains(t, agent.sessions, sid)

	launchesBefore := len(transports)
	resp, err := agent.Prompt(ctx, TextPromptRequest(sid, "again"))
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, resp.StopReason)
	require.Greater(t, len(transports), launchesBefore, "the native process must be relaunched lazily")
}

// T4 — a malformed native frame fails the turn as a transport error without
// tearing down the live session; a follow-up prompt still succeeds.
func TestTurnFailureMalformedFrameNotFatal(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	session, transport, cleanup := newPromptFlowSession(t)
	defer cleanup()

	// An assistant frame without its message object cannot be parsed.
	transport.queryMsgs = []map[string]any{{"type": "assistant"}}

	_, err := session.Prompt(ctx, TextPromptRequest(session.id, "hello"))
	data := requireTurnFailure(t, err, -32603, failureCauseTransport, "")
	message, _ := data[jsonFieldMessage].(string)
	require.NotContains(t, message, "Please start a new session")

	// The live session survived the malformed frame: a normal turn still works.
	transport.queryMsgs = []map[string]any{{
		"type":        "result",
		"subtype":     "success",
		"is_error":    false,
		"stop_reason": "end_turn",
	}}

	resp, err := session.Prompt(ctx, TextPromptRequest(session.id, "again"))
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, resp.StopReason)
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

	resp, err := session.Prompt(promptCtx, TextPromptRequest(session.id, "hello"))
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonCancelled, resp.StopReason)
}

// T6 — with a turn deadline set, a silent-hang harness fails with cause timeout
// (never cancelled), and the native turn is aborted.
func TestTurnFailureTimeout(t *testing.T) {
	t.Parallel()

	session, transport, cleanup := newPromptFlowSession(t)
	defer cleanup()

	session.agent.options.TurnTimeout = 50 * time.Millisecond
	transport.queryMsgs = nil // harness hangs: no result, no error

	_, err := session.Prompt(context.Background(), TextPromptRequest(session.id, "hello"))
	requireTurnFailure(t, err, -32603, failureCauseTimeout, "")
}
