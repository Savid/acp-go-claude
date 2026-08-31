package claudeacp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/savid/acp-go-claude/internal/lifecycle"
	"github.com/stretchr/testify/require"
	"go.uber.org/goleak"
)

type gatedLifecycleActionWriter struct {
	writer  io.Writer
	entered chan actionWireIdentity
	release chan struct{}
	err     error
	once    sync.Once
}

type permanentlyBlockedHostWriter struct {
	entered chan struct{}
	closed  chan struct{}
	once    sync.Once
	close   sync.Once
}

type interruptFullHostWriter struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

type failingInterruptWriter struct{}

func (failingInterruptWriter) Write(data []byte) (int, error) { return len(data), nil }

func (failingInterruptWriter) Close() error { return errors.New("opaque close failure") }

func (failingInterruptWriter) SetWriteDeadline(time.Time) error {
	return errors.New("opaque deadline failure")
}

func newInterruptFullHostWriter() *interruptFullHostWriter {
	return &interruptFullHostWriter{entered: make(chan struct{}), release: make(chan struct{})}
}

func (w *interruptFullHostWriter) Write(data []byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.release

	return len(data), nil
}

func (w *interruptFullHostWriter) Close() error {
	select {
	case <-w.release:
	default:
		close(w.release)
	}

	return nil
}

type gatedPartialHostWriter struct {
	entered chan struct{}
	release chan struct{}
	closed  chan struct{}
	once    sync.Once
}

func (w *gatedPartialHostWriter) Write(data []byte) (int, error) {
	close(w.entered)
	<-w.release

	return len(data) - 1, nil
}

func (w *gatedPartialHostWriter) Close() error {
	w.once.Do(func() { close(w.closed) })

	return nil
}

type closableBuffer struct{ bytes.Buffer }

func (*closableBuffer) Close() error { return nil }

type blockingJSONValue struct {
	entered chan struct{}
	release chan struct{}
	once    sync.Once
	calls   atomic.Int32
	err     error
}

func (v *blockingJSONValue) MarshalJSON() ([]byte, error) {
	if v.calls.Add(1) <= 2 {
		return []byte(`{}`), nil
	}
	v.once.Do(func() { close(v.entered) })
	<-v.release

	return nil, v.err
}

func newPermanentlyBlockedHostWriter() *permanentlyBlockedHostWriter {
	return &permanentlyBlockedHostWriter{entered: make(chan struct{}), closed: make(chan struct{})}
}

func (w *permanentlyBlockedHostWriter) Write([]byte) (int, error) {
	w.once.Do(func() { close(w.entered) })
	<-w.closed

	return 0, errHostWriterClosed
}

func (w *permanentlyBlockedHostWriter) Close() error {
	w.close.Do(func() { close(w.closed) })

	return nil
}

func (w *gatedLifecycleActionWriter) Close() error {
	w.open()

	return nil
}

func (w *gatedLifecycleActionWriter) open() {
	w.once.Do(func() { close(w.release) })
}

func (w *gatedLifecycleActionWriter) Write(data []byte) (int, error) {
	_, identity, action := actionRequestWireIdentity(data)
	if !action {
		return w.writer.Write(data)
	}

	w.entered <- identity
	<-w.release
	if w.err != nil {
		return 0, w.err
	}

	return w.writer.Write(data)
}

func lifecycleActionMetaForTest(actionID string) map[string]any {
	return map[string]any{lifecycle.MetaKey: lifecycle.ActionCorrelationValue(
		"11111111-1111-4111-8111-111111111111",
		lifecycle.ActionUpdate{
			ActionID: actionID,
			Owner: lifecycle.Owner{
				Type: lifecycle.OwnerTurn,
				ID:   "22222222-2222-4222-8222-222222222222",
			},
		},
	)}
}

func newGatedLifecycleActionConnection(
	t *testing.T,
	writeErr error,
) (*localAgentConnection, *gatedLifecycleActionWriter, *bytes.Buffer) {
	t.Helper()

	c2aR, c2aW := io.Pipe()
	a2cR, a2cW := io.Pipe()
	t.Cleanup(func() {
		_ = c2aR.Close()
		_ = c2aW.Close()
		_ = a2cR.Close()
		_ = a2cW.Close()
	})

	var logs bytes.Buffer
	agent := NewAgent(WithLogger(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))))
	gate := &gatedLifecycleActionWriter{
		writer:  a2cW,
		entered: make(chan actionWireIdentity, 1),
		release: make(chan struct{}),
		err:     writeErr,
	}
	clientConn := acp.NewClientSideConnection(&recordingClient{}, c2aW, a2cR)
	conn := newLocalAgentConnection(agent, gate, c2aR)
	agent.setConnection(conn)
	_, err := clientConn.Initialize(t.Context(), acp.InitializeRequest{})
	require.NoError(t, err)

	return conn, gate, &logs
}

func TestLifecycleActionsAnnounceOnlyAfterExactHostRequestWrite(t *testing.T) {
	for _, test := range []struct {
		name   string
		method string
		call   func(context.Context, *localAgentConnection, actionWireAdmission) error
	}{
		{
			name:   "permission",
			method: acp.ClientMethodSessionRequestPermission,
			call: func(ctx context.Context, conn *localAgentConnection, action actionWireAdmission) error {
				_, err := conn.RequestPermission(ctx, acp.RequestPermissionRequest{
					Meta:    lifecycleActionMetaForTest(action.actionID),
					Options: []acp.PermissionOption{{OptionId: permissionRejectOnce, Kind: acp.PermissionOptionKindRejectOnce}},
				}, action)

				return err
			},
		},
		{
			name:   "elicitation",
			method: acp.ClientMethodElicitationCreate,
			call: func(ctx context.Context, conn *localAgentConnection, action actionWireAdmission) error {
				_, err := conn.CreateElicitation(ctx, acp.UnstableCreateElicitationRequest{
					Form: &acp.UnstableCreateElicitationForm{
						Message: "approve",
						Mode:    "form",
						Meta:    lifecycleActionMetaForTest(action.actionID),
					},
				}, elicitationScope{SessionID: "session", TurnNonce: "turn", ToolCallID: "tool"}, action)

				return err
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			conn, gate, _ := newGatedLifecycleActionConnection(t, nil)
			actionID := "action-" + test.name
			observed := make(chan actionWireIdentity, 1)
			action := actionWireAdmission{
				actionID: actionID,
				written: func(_ context.Context, identity actionWireIdentity) error {
					observed <- identity

					return nil
				},
			}
			done := make(chan error, 1)
			go func() { done <- test.call(t.Context(), conn, action) }()

			entered := <-gate.entered
			require.Equal(t, test.method, entered.method)
			require.NotEmpty(t, entered.requestID)
			select {
			case <-observed:
				t.Fatal("pending action was announced before the request write completed")
			default:
			}

			gate.open()
			require.Equal(t, entered, <-observed)
			require.NoError(t, <-done)
		})
	}
}

func TestActionsWithoutLifecycleNegotiationUseExactWriteAdmission(t *testing.T) {
	for _, test := range []struct {
		name   string
		method string
		call   func(context.Context, *localAgentConnection, actionWireAdmission) error
	}{
		{
			name:   "permission",
			method: acp.ClientMethodSessionRequestPermission,
			call: func(ctx context.Context, conn *localAgentConnection, action actionWireAdmission) error {
				_, err := conn.RequestPermission(ctx, acp.RequestPermissionRequest{
					SessionId: "session",
					ToolCall:  acp.ToolCallUpdate{ToolCallId: "tool"},
					Options:   []acp.PermissionOption{{OptionId: permissionRejectOnce, Kind: acp.PermissionOptionKindRejectOnce}},
				}, action)

				return err
			},
		},
		{
			name:   "elicitation",
			method: acp.ClientMethodElicitationCreate,
			call: func(ctx context.Context, conn *localAgentConnection, action actionWireAdmission) error {
				_, err := conn.CreateElicitation(ctx, acp.UnstableCreateElicitationRequest{
					Form: &acp.UnstableCreateElicitationForm{Message: "approve", Mode: "form"},
				}, elicitationScope{SessionID: "session", TurnNonce: "turn", ToolCallID: "tool"}, action)

				return err
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			conn, gate, _ := newGatedLifecycleActionConnection(t, nil)
			observed := make(chan actionWireIdentity, 1)
			action := actionWireAdmission{written: func(_ context.Context, identity actionWireIdentity) error {
				observed <- identity

				return nil
			}}
			done := make(chan error, 1)
			go func() { done <- test.call(t.Context(), conn, action) }()

			entered := <-gate.entered
			require.Equal(t, test.method, entered.method)
			select {
			case <-observed:
				t.Fatal("pending content crossed before the request write completed")
			default:
			}

			gate.open()
			require.Equal(t, entered, <-observed)
			require.NoError(t, <-done)
		})
	}
}

func TestLifecycleActionWriteFailureIsClosedAndNeverAnnounced(t *testing.T) {
	const sentinel = "opaque-host-write-failure"
	conn, gate, logs := newGatedLifecycleActionConnection(t, errors.New(sentinel))
	observed := make(chan actionWireIdentity, 1)
	action := actionWireAdmission{
		actionID: "action-write-failure",
		written: func(_ context.Context, identity actionWireIdentity) error {
			observed <- identity

			return nil
		},
	}
	done := make(chan error, 1)
	go func() {
		_, err := conn.RequestPermission(t.Context(), acp.RequestPermissionRequest{
			Meta:    lifecycleActionMetaForTest(action.actionID),
			Options: []acp.PermissionOption{{OptionId: permissionRejectOnce, Kind: acp.PermissionOptionKindRejectOnce}},
		}, action)
		done <- err
	}()

	<-gate.entered
	gate.open()
	err := <-done
	require.ErrorIs(t, err, errActionWireWrite)
	require.NotContains(t, err.Error(), sentinel)
	select {
	case <-observed:
		t.Fatal("failed request write announced an action")
	default:
	}
	require.NotContains(t, logs.String(), sentinel)
}

func TestLifecycleActionAdmissionFailsAtEachPreResponseBoundary(t *testing.T) {
	params := acp.RequestPermissionRequest{Options: []acp.PermissionOption{{
		OptionId: permissionRejectOnce,
		Kind:     acp.PermissionOptionKindRejectOnce,
	}}}
	publishErr := errors.New("publish failed")
	local := &localAgentConnection{hooks: &postResponseHooks{writes: newHostWriteOwner(&closableBuffer{})}}
	_, err := sendLifecycleActionRequest[acp.RequestPermissionResponse](
		local, t.Context(), acp.ClientMethodSessionRequestPermission, params,
		actionWireAdmission{publish: func() error { return publishErr }, written: func(context.Context, actionWireIdentity) error { return nil }},
	)
	require.ErrorIs(t, err, publishErr)

	_, err = sendLifecycleActionRequest[acp.RequestPermissionResponse](
		local, t.Context(), "unsupported", params,
		actionWireAdmission{written: func(context.Context, actionWireIdentity) error { return nil }},
	)
	require.ErrorIs(t, err, errActionWireRegistration)

	_, err = sendLifecycleActionRequest[acp.RequestPermissionResponse](
		local, t.Context(), acp.ClientMethodSessionRequestPermission, params,
		actionWireAdmission{actionID: "mismatch", written: func(context.Context, actionWireIdentity) error { return nil }},
	)
	require.ErrorIs(t, err, errActionWireRegistration)

	unsupported := &localAgentConnection{hooks: &postResponseHooks{writes: newHostWriteOwner(&bytes.Buffer{})}}
	_, err = sendLifecycleActionRequest[acp.RequestPermissionResponse](
		unsupported, t.Context(), acp.ClientMethodSessionRequestPermission, params,
		actionWireAdmission{written: func(context.Context, actionWireIdentity) error { return nil }},
	)
	require.ErrorIs(t, err, errHostWriterUnsupported)

	held, err := local.hooks.beginSDKWrite(t.Context(), "held", false)
	require.NoError(t, err)
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = sendLifecycleActionRequest[acp.RequestPermissionResponse](
		local, canceled, acp.ClientMethodSessionRequestPermission, params,
		actionWireAdmission{written: func(context.Context, actionWireIdentity) error { return nil }},
	)
	require.ErrorIs(t, err, context.Canceled)
	local.hooks.finishSDKCall(held)
}

func TestLifecycleActionCompletionAndCancellationBeforeTheHostWrite(t *testing.T) {
	newRequest := func(blocker *blockingJSONValue) acp.RequestPermissionRequest {
		title := "tool"

		return acp.RequestPermissionRequest{
			Meta: lifecycleActionMetaForTest("pre-write"),
			ToolCall: acp.ToolCallUpdate{
				ToolCallId: "tool", Title: &title, RawInput: blocker,
			},
			Options: []acp.PermissionOption{{OptionId: permissionRejectOnce, Kind: acp.PermissionOptionKindRejectOnce}},
		}
	}
	action := actionWireAdmission{actionID: "pre-write", written: func(context.Context, actionWireIdentity) error { return nil }}

	t.Run("request fails before writing", func(t *testing.T) {
		conn, _, _ := newGatedLifecycleActionConnection(t, nil)
		marshalErr := errors.New("marshal failed")
		blocker := &blockingJSONValue{entered: make(chan struct{}), release: make(chan struct{}), err: marshalErr}
		done := make(chan error, 1)
		go func() {
			_, err := sendLifecycleActionRequest[acp.RequestPermissionResponse](
				conn, t.Context(), acp.ClientMethodSessionRequestPermission, newRequest(blocker), action,
			)
			done <- err
		}()
		<-blocker.entered
		close(blocker.release)
		require.ErrorContains(t, <-done, marshalErr.Error())
	})

	t.Run("caller cancels before writing", func(t *testing.T) {
		conn, _, _ := newGatedLifecycleActionConnection(t, nil)
		blocker := &blockingJSONValue{entered: make(chan struct{}), release: make(chan struct{}), err: errors.New("marshal stopped")}
		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		request := newRequest(blocker)
		go func() {
			_, err := sendLifecycleActionRequest[acp.RequestPermissionResponse](
				conn, ctx, acp.ClientMethodSessionRequestPermission, request, action,
			)
			done <- err
		}()
		<-blocker.entered
		cancel()
		for {
			conn.hooks.mu.Lock()
			live := len(conn.hooks.actionWrites) != 0
			conn.hooks.mu.Unlock()
			if !live {
				break
			}
			runtime.Gosched()
		}
		close(blocker.release)
		err := <-done
		require.ErrorIs(t, err, context.Canceled)
		require.ErrorIs(t, err, errHostWriteAborted)
	})
}

func TestLifecycleActionObserverFailureCancelsTheExactRequest(t *testing.T) {
	conn, gate, _ := newGatedLifecycleActionConnection(t, nil)
	observeErr := errors.New("observe failed")
	done := make(chan error, 1)
	go func() {
		_, err := conn.RequestPermission(t.Context(), acp.RequestPermissionRequest{
			Options: []acp.PermissionOption{{OptionId: permissionRejectOnce, Kind: acp.PermissionOptionKindRejectOnce}},
		}, actionWireAdmission{written: func(context.Context, actionWireIdentity) error { return observeErr }})
		done <- err
	}()
	<-gate.entered
	gate.open()
	require.ErrorIs(t, <-done, observeErr)
}

func TestElicitationActionWriteFailureIsClosedAndNeverAnnounced(t *testing.T) {
	const sentinel = "opaque-elicitation-write-failure"
	conn, gate, logs := newGatedLifecycleActionConnection(t, errors.New(sentinel))
	observed := make(chan actionWireIdentity, 1)
	action := actionWireAdmission{
		actionID: "action-elicit-write-failure",
		written: func(_ context.Context, identity actionWireIdentity) error {
			observed <- identity

			return nil
		},
	}
	done := make(chan error, 1)
	go func() {
		_, err := conn.CreateElicitation(t.Context(), acp.UnstableCreateElicitationRequest{
			Form: &acp.UnstableCreateElicitationForm{
				Message: "approve", Mode: "form", Meta: lifecycleActionMetaForTest(action.actionID),
			},
		}, elicitationScope{SessionID: "session", TurnNonce: "turn", ToolCallID: "tool"}, action)
		done <- err
	}()

	<-gate.entered
	gate.open()
	err := <-done
	require.ErrorIs(t, err, errActionWireWrite)
	require.NotContains(t, err.Error(), sentinel)
	select {
	case <-observed:
		t.Fatal("failed elicitation write announced an action")
	default:
	}
	require.NotContains(t, logs.String(), sentinel)
}

func TestCancelledBlockedHostWriteIsInterruptedJoinedAndRetired(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	inputReader, inputWriter := io.Pipe()
	t.Cleanup(func() {
		_ = inputReader.Close()
		_ = inputWriter.Close()
	})

	agent := NewAgent()
	writer := newPermanentlyBlockedHostWriter()
	conn := newLocalAgentConnection(agent, writer, inputReader)
	agent.setConnection(conn)

	ctx, cancel := context.WithCancel(t.Context())
	published := make(chan struct{})
	announced := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := conn.RequestPermission(ctx, acp.RequestPermissionRequest{
			Meta:    lifecycleActionMetaForTest("blocked-action"),
			Options: []acp.PermissionOption{{OptionId: permissionRejectOnce, Kind: acp.PermissionOptionKindRejectOnce}},
		}, actionWireAdmission{
			actionID: "blocked-action",
			publish: func() error {
				close(published)

				return nil
			},
			written: func(context.Context, actionWireIdentity) error {
				close(announced)

				return nil
			},
		})
		done <- err
	}()

	<-published
	<-writer.entered
	cancel()

	select {
	case err := <-done:
		require.ErrorIs(t, err, context.Canceled)
		require.ErrorIs(t, err, errHostWriteAborted)
	case <-t.Context().Done():
		t.Fatal("blocked host writer retained callback ownership after cancellation")
	}

	select {
	case <-announced:
		t.Fatal("a contained partial write announced a phantom action")
	default:
	}

	laterErr := conn.SessionUpdate(t.Context(), acp.SessionNotification{
		SessionId: "session", Update: acp.UpdateAgentMessageText("later"),
	})
	require.Error(t, laterErr)
	require.Contains(t, laterErr.Error(), errHostWrite.Error())
	require.NoError(t, agent.Close())
	require.NoError(t, inputWriter.Close())
	require.NoError(t, inputReader.Close())
	<-conn.Done()
}

func TestSessionUpdateCancellationInterruptsAndJoinsItsStalledWrite(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	inputReader, inputWriter := io.Pipe()
	writer := newPermanentlyBlockedHostWriter()
	agent := NewAgent()
	conn := newLocalAgentConnection(agent, writer, inputReader)
	agent.setConnection(conn)
	_, err := agent.Initialize(t.Context(), acp.InitializeRequest{Meta: lifecycleOfferMeta(1)})
	require.NoError(t, err)
	require.True(t, agent.negotiatedLifecycle().Present())

	ctx, cancel := context.WithCancel(t.Context())
	writeDone := make(chan error, 1)
	go func() {
		writeDone <- conn.SessionUpdate(ctx, acp.SessionNotification{
			SessionId: "session", Update: acp.UpdateAgentMessageText("blocked"),
		})
	}()
	<-writer.entered
	cancel()

	var writeErr error
	select {
	case writeErr = <-writeDone:
	case <-t.Context().Done():
		t.Fatal("canceled lifecycle write retained callback ownership")
	}

	require.NoError(t, agent.Close())
	require.NoError(t, inputWriter.Close())
	require.NoError(t, inputReader.Close())
	<-conn.Done()
	require.ErrorContains(t, writeErr, "host transport write failed")
}

func TestCompletedActionWriteWinsCancellationAndKeepsWriterUsable(t *testing.T) {
	conn, gate, _ := newGatedLifecycleActionConnection(t, nil)
	ctx, cancel := context.WithCancel(t.Context())
	observed := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		_, err := conn.RequestPermission(ctx, acp.RequestPermissionRequest{
			Meta: lifecycleActionMetaForTest("completed-action"),
			Options: []acp.PermissionOption{{
				OptionId: permissionRejectOnce,
				Kind:     acp.PermissionOptionKindRejectOnce,
			}},
		}, actionWireAdmission{
			actionID: "completed-action",
			written: func(context.Context, actionWireIdentity) error {
				close(observed)
				<-release

				return nil
			},
		})
		done <- err
	}()

	<-gate.entered
	gate.open()
	<-observed
	cancel()
	close(release)
	<-done
	require.NoError(t, conn.SessionUpdate(t.Context(), acp.SessionNotification{
		SessionId: "session", Update: acp.UpdateAgentMessageText("writer-still-live"),
	}))
}

func TestIncompleteActionWriteLosesCancellationAndJoinsInterruption(t *testing.T) {
	writer := &gatedPartialHostWriter{
		entered: make(chan struct{}),
		release: make(chan struct{}),
		closed:  make(chan struct{}),
	}
	owner := newHostWriteOwner(writer)
	owner.requireInterruptible()

	writeDone := make(chan error, 1)
	go func() {
		_, err := owner.Write([]byte("frame"))
		writeDone <- err
	}()
	<-writer.entered

	interruptDone := make(chan error, 1)
	go func() { interruptDone <- owner.interruptActive() }()
	<-writer.closed
	close(writer.release)

	require.NoError(t, <-interruptDone)
	require.ErrorIs(t, <-writeDone, errHostWriteAborted)
	require.ErrorIs(t, func() error {
		_, err := owner.Write([]byte("next"))

		return err
	}(), errHostWriterClosed)
}

func TestInterruptedBlockedFullWriteWinsExactCompletion(t *testing.T) {
	writer := newInterruptFullHostWriter()
	owner := newHostWriteOwner(writer)
	owner.requireInterruptible()

	writeDone := make(chan error, 1)
	go func() {
		_, err := owner.Write([]byte("complete-frame"))
		writeDone <- err
	}()
	<-writer.entered

	interruptDone := make(chan error, 1)
	go func() { interruptDone <- owner.interruptActive() }()

	require.NoError(t, <-writeDone)
	require.NoError(t, <-interruptDone)
}

func TestHostWriteOwnershipRejectsEveryInvalidAdmissionBoundary(t *testing.T) {
	hooks := &postResponseHooks{log: slog.New(slog.DiscardHandler)}
	_, err := hooks.registerActionWrite(actionRequestCorrelation{})
	require.ErrorIs(t, err, errHostWriterUnsupported)
	require.NoError(t, hooks.interruptActiveWrite())
	require.NoError(t, hooks.closeWrites())

	hooks.writes = newHostWriteOwner(&closableBuffer{})
	closedHooks := &postResponseHooks{writes: newHostWriteOwner(&closableBuffer{})}
	require.NoError(t, closedHooks.closeWrites())
	_, err = closedHooks.registerActionWrite(actionRequestCorrelation{})
	require.ErrorIs(t, err, errHostWriterClosed)
	correlation, ok := actionRequestCorrelationFor(
		acp.ClientMethodSessionRequestPermission,
		json.RawMessage(`{"sessionId":"session"}`),
	)
	require.True(t, ok)
	registration, err := hooks.registerActionWrite(correlation)
	require.NoError(t, err)
	_, err = hooks.registerActionWrite(correlation)
	require.ErrorIs(t, err, errActionWireRegistration)
	require.False(t, hooks.withdrawActionWrite(actionRequestCorrelation{method: "other"}, registration))
	require.True(t, hooks.withdrawActionWrite(correlation, registration))

	registration, err = hooks.registerActionWrite(correlation)
	require.NoError(t, err)
	wire := []byte(`{"jsonrpc":"2.0","id":1,"method":"session/request_permission","params":{"sessionId":"session"}}`)
	started, identity, action := hooks.beginActionWrite(wire)
	require.True(t, action)
	require.Same(t, registration, started)
	require.False(t, hooks.withdrawActionWrite(correlation, registration))
	started, _, action = hooks.beginActionWrite(wire)
	require.True(t, action)
	require.Nil(t, started)
	hooks.finishActionWrite(registration, identity, true)
	require.True(t, (<-registration.result).written)

	unregistered := &postResponseHooks{log: slog.New(slog.DiscardHandler)}
	wrapped := unregistered.wrap(&closableBuffer{})
	_, err = wrapped.Write(wire)
	require.ErrorIs(t, err, errActionWireRegistration)

	write, err := hooks.beginSDKWrite(t.Context(), "method", false)
	require.NoError(t, err)
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = hooks.beginSDKWrite(canceled, "method", false)
	require.ErrorIs(t, err, context.Canceled)
	hooks.finishSDKCall(write)
	write, err = hooks.beginSDKWrite(t.Context(), "method", false)
	require.NoError(t, err)
	line := hooks.beginSDKLine([]byte(`{"method":"method"}`))
	require.Same(t, write, line)
	hooks.finishSDKLine(line)
	hooks.finishSDKLine(line)
	hooks.finishSDKLine(&sdkWriteOwnership{})

	cancelHooks := &postResponseHooks{writes: newHostWriteOwner(&closableBuffer{})}
	cancelCtx, cancelWrite := context.WithCancel(t.Context())
	write, err = cancelHooks.beginSDKWrite(cancelCtx, "method", true)
	require.NoError(t, err)
	cancelWrite()
	cancelHooks.finishSDKCall(write)
	require.True(t, cancelHooks.writes.closed())

	unsupported := newHostWriteOwner(&bytes.Buffer{})
	unsupported.requireInterruptible()
	_, err = unsupported.Write([]byte("frame"))
	require.ErrorIs(t, err, errHostWriterUnsupported)

	preCanceled := newHostWriteOwner(&closableBuffer{})
	ownership := &sdkWriteOwnership{}
	ownership.canceled.Store(true)
	_, err = preCanceled.write([]byte("frame"), ownership)
	require.ErrorIs(t, err, errHostWriteAborted)

	failing := newHostWriteOwner(failingInterruptWriter{})
	err = failing.abort()
	require.ErrorContains(t, err, "deadline interruption failed")
	require.ErrorContains(t, err, "close interruption failed")
	require.NotContains(t, err.Error(), "opaque")

	stageCause := errors.New("opaque stage cause")
	stageErr := &safeStageError{stage: errHostWrite, cause: stageCause}
	require.ErrorIs(t, stageErr, errHostWrite)
	require.ErrorIs(t, stageErr, stageCause)

	_, ok = actionRequestCorrelationFor(
		acp.ClientMethodSessionRequestPermission,
		map[string]any{"invalid": make(chan struct{})},
	)
	require.False(t, ok)
	require.Empty(t, lifecycleActionID(map[string]any{"invalid": make(chan struct{})}))
	require.Empty(t, lifecycleActionID(map[string]any{"_meta": map[string]any{lifecycle.MetaKey: "invalid"}}))

	hooks.cancelPending()
	canceledHook := false
	hooks.enqueue("response", func() {}, func() { canceledHook = true })
	hooks.cancelPending()
	require.True(t, canceledHook)
}

func TestCanceledWriteCallbackCannotOvertakeCompletedOwnership(t *testing.T) {
	hooks := &postResponseHooks{writes: newHostWriteOwner(&closableBuffer{})}
	ctx, cancel := context.WithCancel(t.Context())
	write, err := hooks.beginSDKWrite(ctx, "method", true)
	require.NoError(t, err)

	hooks.mu.Lock()
	cancel()
	write.completed = true
	hooks.activeWrite = nil
	hooks.releaseSDKWriteLocked()
	hooks.mu.Unlock()
	<-write.joined

	require.False(t, hooks.writes.closed())
	next, err := hooks.beginSDKWrite(t.Context(), "next", false)
	require.NoError(t, err)
	hooks.finishSDKCall(next)
}

func TestInterruptExactContainsOnlyTheNamedActiveWrite(t *testing.T) {
	writer := newInterruptFullHostWriter()
	owner := newHostWriteOwner(writer)
	owner.requireInterruptible()
	done := make(chan error, 1)
	go func() {
		_, err := owner.Write([]byte("frame"))
		done <- err
	}()
	<-writer.entered

	(&hostWriteTimeout{owner: owner, id: 99}).interrupt()
	select {
	case <-done:
		t.Fatal("an unrelated write identity was interrupted")
	default:
	}
	owner.interruptExact(1)
	require.NoError(t, <-done)
}

func TestAgentCloseInterruptsAndJoinsStalledLifecycleWrite(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	inputReader, inputWriter := io.Pipe()
	writer := newPermanentlyBlockedHostWriter()
	agent := NewAgent()
	conn := newLocalAgentConnection(agent, writer, inputReader)
	agent.setConnection(conn)
	_, err := agent.Initialize(t.Context(), acp.InitializeRequest{Meta: lifecycleOfferMeta(1)})
	require.NoError(t, err)
	require.True(t, agent.negotiatedLifecycle().Present())

	writeDone := make(chan error, 1)
	go func() {
		writeDone <- conn.SessionUpdate(t.Context(), acp.SessionNotification{
			SessionId: "session", Update: acp.UpdateAgentMessageText("blocked"),
		})
	}()
	<-writer.entered

	require.NoError(t, agent.Close())
	writeErr := <-writeDone
	require.NoError(t, inputWriter.Close())
	require.NoError(t, inputReader.Close())
	<-conn.Done()
	require.ErrorContains(t, writeErr, "host transport write failed")
}

func TestLocalAgentConnectionClientMethods(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	c2aR, c2aW := io.Pipe()
	a2cR, a2cW := io.Pipe()
	t.Cleanup(func() {
		_ = c2aR.Close()
		_ = c2aW.Close()
		_ = a2cR.Close()
		_ = a2cW.Close()
	})

	agent := NewAgent()
	recording := &recordingClient{}
	clientConn := acp.NewClientSideConnection(recording, c2aW, a2cR)
	conn := newLocalAgentConnection(agent, a2cW, c2aR)
	agent.setConnection(conn)
	writeAdmission := actionWireAdmission{written: func(context.Context, actionWireIdentity) error { return nil }}

	_, err := clientConn.Initialize(ctx, acp.InitializeRequest{})
	require.NoError(t, err)
	require.NotNil(t, conn.Done())

	require.NoError(t, conn.UnstableCompleteElicitation(ctx, acp.UnstableCompleteElicitationNotification{ElicitationId: "e1"}))
	_, err = conn.CreateElicitation(ctx, acp.UnstableCreateElicitationRequest{}, elicitationScope{}, actionWireAdmission{})
	require.ErrorContains(t, err, "form or url")
	requestID := "r1"
	_, err = conn.CreateElicitation(ctx, acp.UnstableCreateElicitationRequest{Url: &acp.UnstableCreateElicitationUrl{ElicitationId: "e2", Message: "m", Mode: "url", Url: "https://example.test", Meta: map[string]any{"u": "m"}}}, elicitationScope{SessionID: "s", TurnNonce: "turn-1", RequestID: &requestID}, writeAdmission)
	require.NoError(t, err)
	_, err = conn.CreateElicitation(ctx, acp.UnstableCreateElicitationRequest{Form: &acp.UnstableCreateElicitationForm{Message: "approve", Mode: "form", Meta: map[string]any{"f": "m"}}}, elicitationScope{SessionID: "s", TurnNonce: "turn-2", ToolCallID: "tool-1"}, writeAdmission)
	require.NoError(t, err)
	_, err = conn.RequestPermission(ctx, acp.RequestPermissionRequest{}, actionWireAdmission{})
	require.ErrorIs(t, err, errActionWireRegistration)
	elicitations := recording.Elicitations()
	require.Len(t, elicitations, 2)
	require.Equal(t, map[string]any{
		"u": "m",
		routeMetaKey: map[string]any{
			routeFieldVer:  float64(1),
			routeFieldID:   "s",
			routeFieldTurn: "turn-1",
			"requestId":    "r1",
		},
	}, elicitations[0].Url.Meta)
	require.Equal(t, map[string]any{
		"f": "m",
		routeMetaKey: map[string]any{
			routeFieldVer:  float64(1),
			routeFieldID:   "s",
			routeFieldTurn: "turn-2",
			"toolCallId":   "tool-1",
		},
	}, elicitations[1].Form.Meta)
	_, err = conn.ReadTextFile(ctx, acp.ReadTextFileRequest{Path: "/tmp/file"})
	require.Error(t, err)
	_, err = conn.WriteTextFile(ctx, acp.WriteTextFileRequest{Path: "/tmp/file", Content: "body"})
	require.Error(t, err)
	permission, err := conn.RequestPermission(ctx, acp.RequestPermissionRequest{
		Options: []acp.PermissionOption{{OptionId: permissionRejectOnce, Kind: acp.PermissionOptionKindRejectOnce}},
	}, writeAdmission)
	require.NoError(t, err)
	require.NotNil(t, permission.Outcome.Cancelled)
	require.NoError(t, conn.SessionUpdate(ctx, acp.SessionNotification{SessionId: "s", Update: acp.UpdateAgentMessageText("hello")}))
	_, err = conn.CreateTerminal(ctx, acp.CreateTerminalRequest{})
	require.Error(t, err)
	_, err = conn.KillTerminal(ctx, acp.KillTerminalRequest{})
	require.Error(t, err)
	_, err = conn.TerminalOutput(ctx, acp.TerminalOutputRequest{})
	require.Error(t, err)
	_, err = conn.ReleaseTerminal(ctx, acp.ReleaseTerminalRequest{})
	require.Error(t, err)
	_, err = conn.WaitForTerminalExit(ctx, acp.WaitForTerminalExitRequest{})
	require.Error(t, err)
	require.NoError(t, conn.NotifyExtension(ctx, "_client/test", map[string]any{"ok": true}))
	require.Error(t, conn.NotifyExtension(ctx, "bad", nil))
}

func TestEveryLocalClientCallSharesBackpressureAndCanceledWriteAdmission(t *testing.T) {
	agent := NewAgent(WithConcurrencyLimits(ConcurrencyLimits{MaxConcurrentClientCalls: 1}))
	conn := &localAgentConnection{agent: agent, hooks: &postResponseHooks{writes: newHostWriteOwner(&closableBuffer{})}}

	calls := []func(context.Context) error{
		func(ctx context.Context) error {
			return conn.UnstableCompleteElicitation(ctx, acp.UnstableCompleteElicitationNotification{})
		},
		func(ctx context.Context) error {
			_, err := conn.CreateElicitation(ctx, acp.UnstableCreateElicitationRequest{
				Form: &acp.UnstableCreateElicitationForm{Message: "question", Mode: "form"},
			}, elicitationScope{SessionID: "session", TurnNonce: "turn", ToolCallID: "tool"}, actionWireAdmission{})

			return err
		},
		func(ctx context.Context) error {
			_, err := conn.ReadTextFile(ctx, acp.ReadTextFileRequest{})

			return err
		},
		func(ctx context.Context) error {
			_, err := conn.WriteTextFile(ctx, acp.WriteTextFileRequest{})

			return err
		},
		func(ctx context.Context) error {
			_, err := conn.RequestPermission(ctx, acp.RequestPermissionRequest{}, actionWireAdmission{})

			return err
		},
		func(ctx context.Context) error { return conn.SessionUpdate(ctx, acp.SessionNotification{}) },
		func(ctx context.Context) error {
			_, err := conn.CreateTerminal(ctx, acp.CreateTerminalRequest{})

			return err
		},
		func(ctx context.Context) error {
			_, err := conn.KillTerminal(ctx, acp.KillTerminalRequest{})

			return err
		},
		func(ctx context.Context) error {
			_, err := conn.TerminalOutput(ctx, acp.TerminalOutputRequest{})

			return err
		},
		func(ctx context.Context) error {
			_, err := conn.ReleaseTerminal(ctx, acp.ReleaseTerminalRequest{})

			return err
		},
		func(ctx context.Context) error {
			_, err := conn.WaitForTerminalExit(ctx, acp.WaitForTerminalExitRequest{})

			return err
		},
		func(ctx context.Context) error { return conn.NotifyExtension(ctx, "_test", map[string]any{}) },
	}

	release, err := agent.acquireClientCall(t.Context())
	require.NoError(t, err)
	for _, call := range calls {
		require.Error(t, call(t.Context()))
	}
	release()

	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	canceled = context.WithValue(canceled, clientCallPermitContextKey{}, &clientCallPermit{agent: agent})
	held, err := conn.hooks.beginSDKWrite(t.Context(), "held", false)
	require.NoError(t, err)
	for _, call := range calls {
		require.ErrorIs(t, call(canceled), context.Canceled)
	}
	conn.hooks.finishSDKCall(held)
}

func TestLocalAgentDispatcherBranches(t *testing.T) {
	t.Parallel()

	agent := NewAgent()
	conn := &localAgentConnection{agent: agent}
	ctx := context.Background()

	_, reqErr := conn.handle(ctx, acp.AgentMethodSessionList, nil)
	require.NotNil(t, reqErr)
	require.Equal(t, -32600, reqErr.Code)

	resp, reqErr := conn.handle(ctx, acp.AgentMethodInitialize, json.RawMessage(`{}`))
	require.Nil(t, reqErr)
	require.NotNil(t, resp)
	require.True(t, conn.initialized.Load())

	_, reqErr = conn.handle(ctx, "unknown", nil)
	require.NotNil(t, reqErr)
	require.Equal(t, -32601, reqErr.Code)
	_, reqErr = conn.handle(ctx, "_unknown", nil)
	require.NotNil(t, reqErr)
	require.Equal(t, -32601, reqErr.Code)

	forkAgent := newForkTestAgent(t, nil)
	forkConn := &localAgentConnection{agent: forkAgent}
	forkConn.initialized.Store(true)
	rawFork, err := json.Marshal(ForkSessionRequest("parent", t.TempDir()))
	require.NoError(t, err)
	resp, reqErr = forkConn.handle(ctx, ForkSessionMethod, rawFork)
	require.Nil(t, reqErr)
	require.NotNil(t, resp)

	_, reqErr = conn.handle(ctx, acp.AgentMethodAuthenticate, json.RawMessage(`{bad`))
	require.NotNil(t, reqErr)
	require.Equal(t, -32602, reqErr.Code)
	_, reqErr = conn.handle(ctx, acp.AgentMethodSessionNew, json.RawMessage(`{"cwd":"relative"}`))
	require.NotNil(t, reqErr)
	require.Equal(t, -32602, reqErr.Code)

	_, reqErr = localResponse((*Agent).Authenticate)(ctx, agent, json.RawMessage(`{"methodId":"m"}`))
	require.NotNil(t, reqErr)
	_, reqErr = localNotification((*Agent).Cancel)(ctx, agent, json.RawMessage(`{"sessionId":"missing"}`))
	require.NotNil(t, reqErr)
	_, reqErr = localNotification((*Agent).Cancel)(ctx, agent, json.RawMessage(`{bad`))
	require.NotNil(t, reqErr)
	require.Equal(t, -32602, reqErr.Code)
}

// TestLocalAgentDispatcherRefusesAdmissionBeforeDispatchOrDecode pins the stdio
// path an embedded host actually speaks. Admission runs ahead of method dispatch
// and parameter decoding, and each refusal keeps its own verdict on the wire: a
// closed agent cannot accept the request at all, while a refused construction
// option is the host's build and must arrive with the payload it was given
// rather than restated as prose inside a different code.
func TestLocalAgentDispatcherRefusesAdmissionBeforeDispatchOrDecode(t *testing.T) {
	t.Parallel()

	closed := NewAgent()
	require.NoError(t, closed.Close())

	refused := NewAgent(WithImageLimits(ImageLimits{MaxInputBytesPerImage: -1}))

	var refusedErr *acp.RequestError
	require.ErrorAs(t, refused.configurationError(), &refusedErr)

	ctx := context.Background()

	admissions := []struct {
		name    string
		agent   *Agent
		code    int
		message string
		data    any
	}{
		{
			name:    "closed agent",
			agent:   closed,
			code:    -32600,
			message: "Invalid request",
			data:    map[string]any{jsonFieldError: errAgentClosed.Error()},
		},
		{
			name:    "refused construction option",
			agent:   refused,
			code:    -32603,
			message: "Internal error",
			data:    refusedErr.Data,
		},
	}

	tests := []struct {
		name        string
		initialized bool
		method      string
		params      json.RawMessage
	}{
		{name: "before initialization gate", method: acp.AgentMethodSessionList},
		{name: "initialize", method: acp.AgentMethodInitialize, params: json.RawMessage(`{bad`)},
		{name: "unknown stable method", initialized: true, method: "unknown"},
		{name: "unknown extension method", initialized: true, method: "_unknown"},
		{name: "known malformed params", initialized: true, method: acp.AgentMethodAuthenticate, params: json.RawMessage(`{bad`)},
	}

	for _, admission := range admissions {
		for _, tc := range tests {
			t.Run(admission.name+"/"+tc.name, func(t *testing.T) {
				t.Parallel()

				conn := &localAgentConnection{agent: admission.agent}
				conn.initialized.Store(tc.initialized)

				_, reqErr := conn.handle(ctx, tc.method, tc.params)
				require.NotNil(t, reqErr)
				require.Equal(t, admission.code, reqErr.Code)
				require.Equal(t, admission.message, reqErr.Message)
				require.Equal(t, admission.data, reqErr.Data)
			})
		}
	}
}

// TestLocalAgentDispatcherReportsAnHonoredCancelFromEveryFailurePath proves the
// request context reaches the error mapper on every dispatch path that can
// fail. Each of these answers its own specific code while the request is live;
// once the peer has withdrawn it they must all answer -32800, which they can
// only do if the handler's own context was threaded through instead of a
// detached one that could never observe the cancel.
func TestLocalAgentDispatcherReportsAnHonoredCancelFromEveryFailurePath(t *testing.T) {
	t.Parallel()

	closedAgent := func(t *testing.T) *Agent {
		t.Helper()

		agent := NewAgent()
		require.NoError(t, agent.Close())

		return agent
	}

	tests := []struct {
		name     string
		agent    func(*testing.T) *Agent
		method   string
		params   json.RawMessage
		liveCode int
	}{
		{
			name:     "admission guard",
			agent:    closedAgent,
			method:   acp.AgentMethodSessionList,
			liveCode: -32600,
		},
		{
			name:     "extension method",
			method:   "_unknown",
			liveCode: -32601,
		},
		{
			name:     "response handler",
			method:   acp.AgentMethodAuthenticate,
			params:   json.RawMessage(`{"methodId":"m"}`),
			liveCode: -32602,
		},
		{
			name:     "notification handler",
			method:   acp.AgentMethodSessionCancel,
			params:   json.RawMessage(`{"sessionId":"missing"}`),
			liveCode: -32602,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			newConn := func() *localAgentConnection {
				agent := NewAgent()
				if tc.agent != nil {
					agent = tc.agent(t)
				}

				conn := &localAgentConnection{agent: agent}
				conn.initialized.Store(true)

				return conn
			}

			_, liveErr := newConn().handle(t.Context(), tc.method, tc.params)
			require.NotNil(t, liveErr)
			require.Equal(t, tc.liveCode, liveErr.Code)

			cancelled, cancel := context.WithCancelCause(t.Context())
			cancel(context.Canceled)

			_, cancelledErr := newConn().handle(cancelled, tc.method, tc.params)
			require.NotNil(t, cancelledErr)
			require.Equal(t, -32800, cancelledErr.Code)
		})
	}
}

func TestStableSessionForkReturnsMethodNotFound(t *testing.T) {
	t.Parallel()

	agent := NewAgent()
	conn := &localAgentConnection{agent: agent}
	conn.initialized.Store(true)
	ctx := context.Background()

	raw, err := json.Marshal(ForkSessionRequest("parent", t.TempDir()))
	require.NoError(t, err)

	// The adapter exposes fork only through the namespaced extension method; the
	// stable ACP session/fork route must be method-not-found (-32601).
	_, reqErr := conn.handle(ctx, acp.AgentMethodSessionFork, raw)
	require.NotNil(t, reqErr)
	require.Equal(t, -32601, reqErr.Code)

	_, extErr := agent.HandleExtensionMethod(ctx, acp.AgentMethodSessionFork, raw)
	require.Error(t, extErr)

	var extReqErr *acp.RequestError
	require.ErrorAs(t, extErr, &extReqErr)
	require.Equal(t, -32601, extReqErr.Code)
}

func TestLifecycleCommandUpdatePostResponseHook(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name   string
		method string
		params json.RawMessage
		result any
	}{
		{
			name:   "new",
			method: acp.AgentMethodSessionNew,
			params: postResponseHookParams(nil, "1"),
			result: acp.NewSessionResponse{SessionId: "session-1"},
		},
		{
			name:   "load",
			method: acp.AgentMethodSessionLoad,
			params: postResponseHookParams(map[string]string{"sessionId": "session-1"}, "1"),
			result: acp.LoadSessionResponse{},
		},
		{
			name:   "resume",
			method: acp.AgentMethodSessionResume,
			params: postResponseHookParams(map[string]string{"sessionId": "session-1"}, "1"),
			result: acp.ResumeSessionResponse{},
		},
		{
			name:   "fork",
			method: ForkSessionMethod,
			params: postResponseHookParams(nil, "1"),
			result: acp.UnstableForkSessionResponse{SessionId: "session-1"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()

			ctx := context.Background()
			agent := NewAgent()
			client := newRecordingAgentClient()
			agent.setConnection(client)
			session := &agentSession{
				agent:             agent,
				id:                "session-1",
				availableCommands: []claude.SlashCommand{{Name: "help", Description: "Help"}},
			}
			agent.mu.Lock()
			agent.sessions[session.id] = session
			agent.mu.Unlock()

			hooks := &postResponseHooks{log: agent.log}
			conn := &localAgentConnection{agent: agent, hooks: hooks}
			conn.enqueueSessionEstablishedHook(ctx, tc.method, tc.params, tc.result)

			resultJSON, err := json.Marshal(tc.result)
			require.NoError(t, err)
			var output closableBuffer
			wrapped := hooks.wrap(&output)
			_, err = wrapped.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":` + string(resultJSON) + "}\n"))
			require.NoError(t, err)
			require.Contains(t, output.String(), `"id":1`)

			require.Eventually(t, func() bool {
				return len(availableCommandUpdates(client.Updates())) == 1
			}, time.Second, 10*time.Millisecond)
		})
	}
}

func TestStartupPermissionWaitsForSuccessfulEstablishmentPublication(t *testing.T) {
	agent := NewAgent()
	clientConn := newRecordingAgentClient()
	clientConn.permission = permissionAllowOnce
	agent.setConnection(clientConn)
	_, err := agent.Initialize(t.Context(), acp.InitializeRequest{Meta: lifecycleOfferMeta(1)})
	require.NoError(t, err)

	transport := newFakeClaudeTransport()
	native := claude.NewClient(agent.log, claude.Options{}, transport)
	require.NoError(t, native.Start(t.Context()))
	session := &agentSession{
		agent: agent, id: "startup-session", client: native,
		turn: make(chan struct{}, sessionTurnCapacity), cwd: t.TempDir(),
		permissionRules: map[string]string{},
	}
	require.NoError(t, session.installEstablishmentGate(native))
	native.SetControlHandlerAdmission(session.admitControlCallback)
	agent.mu.Lock()
	agent.sessions[session.id] = session
	agent.mu.Unlock()

	route := session.establishmentRoute(native)
	require.NotEmpty(t, route)
	decisionDone := make(chan error, 1)
	go func() {
		callbackCtx, finish, admitted := session.admitControlCallback(t.Context(), route)
		if !admitted {
			decisionDone <- errors.New("startup callback was not retained")

			return
		}
		defer finish()
		_, permissionErr := session.handlePermission(callbackCtx, claude.PermissionRequest{
			ToolName: "Write", ToolUseID: "startup-write",
			Input: map[string]any{"file_path": "/tmp/startup", "content": "x"},
		})
		decisionDone <- permissionErr
	}()

	require.Empty(t, clientConn.Updates())

	buffer := &closableBuffer{}
	hooks := &postResponseHooks{log: agent.log, writes: newHostWriteOwner(buffer)}
	local := &localAgentConnection{agent: agent, hooks: hooks}
	local.enqueueSessionEstablishedHook(
		t.Context(), acp.AgentMethodSessionNew, postResponseHookParams(nil, "1"),
		acp.NewSessionResponse{SessionId: session.id},
	)
	writer := &postResponseWriter{writer: hooks.writes, hooks: hooks}
	response := []byte(`{"jsonrpc":"2.0","id":1,"result":{"sessionId":"startup-session"}}`)
	n, err := writer.Write(response)
	require.NoError(t, err)
	require.Equal(t, len(response), n)
	require.NoError(t, <-decisionDone)

	ownerIndex := -1
	ordinaryIndex := -1
	pendingIndex := -1
	terminalIndex := -1
	for index, notification := range clientConn.Updates() {
		if notification.Update.ToolCall != nil && notification.Update.ToolCall.ToolCallId == "startup-write" {
			ordinaryIndex = index
		}
	}
	for _, entry := range lifecycleNotificationEvents(t, clientConn) {
		switch entry.event["type"] {
		case string(lifecycle.EventStateUpdate):
			if entry.event["state"] == string(lifecycle.ForegroundRunning) &&
				entry.event["cause"] == string(lifecycle.CauseActivity) && ownerIndex < 0 {
				ownerIndex = entry.index
			}
		case string(lifecycle.EventActionUpdate):
			action := requireAnyMap(t, entry.event["action"])
			if action["state"] == string(lifecycle.ActionPending) {
				pendingIndex = entry.index
			}
			if action["state"] == string(lifecycle.ActionAccepted) {
				terminalIndex = entry.index
			}
		}
	}
	require.GreaterOrEqual(t, ownerIndex, 0)
	require.Greater(t, ordinaryIndex, ownerIndex)
	require.Greater(t, pendingIndex, ordinaryIndex)
	require.Greater(t, terminalIndex, pendingIndex)

	pushNativeFrames(transport, resultFrame())
	awaitLifecycleEvent(t, clientConn, transitionMatcher(lifecycle.ForegroundIdle, lifecycle.CauseActivity))
	require.NoError(t, session.Close(t.Context()))
}

func TestAgentCloseJoinsPostResponseEstablishmentProducer(t *testing.T) {
	defer goleak.VerifyNone(t, goleak.IgnoreCurrent())

	session, transport, _, cleanup := newNegotiatedPromptFlowSession(t)
	defer cleanup()

	agent := session.agent
	carrier := newGatedSessionUpdateClient(2)
	agent.setConnection(carrier)
	agent.mu.Lock()
	agent.sessions[session.id] = session
	agent.mu.Unlock()

	hooks := &postResponseHooks{log: agent.log}
	local := &localAgentConnection{agent: agent, hooks: hooks}
	result := acp.NewSessionResponse{SessionId: session.id}
	local.enqueueSessionEstablishedHook(
		t.Context(), acp.AgentMethodSessionNew, postResponseHookParams(nil, "1"), result,
	)

	// Hold the first establishment boundary before the worker can serve the
	// native pump. The response hook will be removed and its goroutine launched,
	// but the producer admission acquired at enqueue time must already belong to
	// Close.
	session.pumpServeMu.Lock()
	resultJSON, err := json.Marshal(result)
	require.NoError(t, err)
	hooks.runAfterResponseWrite([]byte(`{"jsonrpc":"2.0","id":1,"result":` + string(resultJSON) + `}`))
	hooks.mu.Lock()
	require.Empty(t, hooks.all, "the successful response transfers the pre-admitted hook exactly once")
	hooks.mu.Unlock()

	closeStarted := make(chan struct{})
	closeDone := make(chan error, 1)
	go func() {
		close(closeStarted)
		closeDone <- agent.Close()
	}()
	<-closeStarted

	for {
		agent.mu.Lock()
		closed := agent.closed
		agent.mu.Unlock()
		if closed {
			break
		}

		select {
		case <-t.Context().Done():
			t.Fatal("context ended before Agent.Close entered its close ladder")
		default:
			runtime.Gosched()
		}
	}

	require.Zero(t, transport.CloseCalls(), "native teardown waits behind the establishment producer")

	session.pumpServeMu.Unlock()
	close(carrier.release)
	require.NoError(t, <-closeDone)
	require.Positive(t, transport.CloseCalls())
}

func TestPostResponseHookRequiresExactSuccessfulResponseAndRunsOnce(t *testing.T) {
	t.Parallel()

	hooks := &postResponseHooks{log: slog.New(slog.DiscardHandler)}
	started := make(chan struct{})
	release := make(chan struct{})
	done := make(chan struct{})

	var calls atomic.Int32
	hooks.enqueue("7", func() {
		calls.Add(1)
		close(started)
		<-release
		close(done)
	})

	nonResponses := [][]byte{
		[]byte(`{"jsonrpc":"2.0","id":7,"method":"session/request_permission","params":{}}`),
		[]byte(`{"jsonrpc":"2.0","id":7,"method":"elicitation/create","params":{}}`),
		[]byte(`{"jsonrpc":"2.0","id":7}`),
		[]byte(`{"jsonrpc":"2.0","id":7,"result":null,"error":null}`),
		[]byte(`{"jsonrpc":"2.0","id":8,"result":null}`),
		[]byte(`{"id":7,"result":null}`),
	}
	for _, frame := range nonResponses {
		hooks.runAfterResponseWrite(frame)
		hooks.mu.Lock()
		require.Len(t, hooks.all, 1, "a colliding request or malformed response cannot release the hook")
		hooks.mu.Unlock()
		require.Zero(t, calls.Load())
	}

	hooks.runAfterResponseWrite([]byte(`{"jsonrpc":"2.0","id":7,"result":null}`))
	<-started

	// A duplicate successful response finds no hook to run while the first
	// worker is still held at the deterministic barrier.
	hooks.runAfterResponseWrite([]byte(`{"jsonrpc":"2.0","id":7,"result":{}}`))
	require.Equal(t, int32(1), calls.Load())
	hooks.mu.Lock()
	require.Empty(t, hooks.all)
	hooks.mu.Unlock()

	close(release)
	<-done
}

func TestLifecycleCommandPostResponseHookUsesResponseIDForIdenticalResults(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	agent := NewAgent()
	client := newRecordingAgentClient()
	agent.setConnection(client)
	sessionOne := &agentSession{
		agent:             agent,
		id:                "session-1",
		availableCommands: []claude.SlashCommand{{Name: "one", Description: "One"}},
	}
	sessionTwo := &agentSession{
		agent:             agent,
		id:                "session-2",
		availableCommands: []claude.SlashCommand{{Name: "two", Description: "Two"}},
	}
	agent.mu.Lock()
	agent.sessions[sessionOne.id] = sessionOne
	agent.sessions[sessionTwo.id] = sessionTwo
	agent.mu.Unlock()

	hooks := &postResponseHooks{log: agent.log}
	conn := &localAgentConnection{agent: agent, hooks: hooks}
	conn.enqueueSessionEstablishedHook(
		ctx,
		acp.AgentMethodSessionResume,
		postResponseHookParams(map[string]string{"sessionId": string(sessionOne.id)}, "1"),
		acp.ResumeSessionResponse{},
	)
	conn.enqueueSessionEstablishedHook(
		ctx,
		acp.AgentMethodSessionResume,
		postResponseHookParams(map[string]string{"sessionId": string(sessionTwo.id)}, "2"),
		acp.ResumeSessionResponse{},
	)

	hooks.runAfterResponseWrite([]byte(`{"jsonrpc":"2.0","id":2,"result":{}}`))
	require.Eventually(t, func() bool {
		updates := availableCommandUpdates(client.Updates())

		return len(updates) == 1 && len(updates[0].AvailableCommands) == 1 && updates[0].AvailableCommands[0].Name == "two"
	}, time.Second, 10*time.Millisecond)

	hooks.runAfterResponseWrite([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	require.Eventually(t, func() bool {
		updates := availableCommandUpdates(client.Updates())

		return len(updates) == 2 && len(updates[1].AvailableCommands) == 1 && updates[1].AvailableCommands[0].Name == "one"
	}, time.Second, 10*time.Millisecond)
}

// TestPostResponseHookRequestIDSurvivesARealRequestsParams pins the tag against
// the params an establishing request actually carries. mcpServers is mandatory
// and _meta is an object, so a reader that expected every value to be a string
// recovered no tag at all — and a session whose hook never ran emitted no
// opening snapshot, leaving its host waiting on an incarnation nobody would
// ever name.
func TestPostResponseHookRequestIDSurvivesARealRequestsParams(t *testing.T) {
	t.Parallel()

	tagged := tagPostResponseHookRequest([]byte(
		`{"jsonrpc":"2.0","id":9,"method":"session/new","params":` +
			`{"cwd":"/tmp/work","mcpServers":[],"_meta":{"lifecycle":{"version":1}}}}`,
	))

	var msg struct {
		Params json.RawMessage `json:"params"`
	}
	require.NoError(t, json.Unmarshal(tagged, &msg))
	require.Equal(t, "9", postResponseHookRequestID(msg.Params))

	require.Empty(t, postResponseHookRequestID(json.RawMessage(`{"cwd":"/tmp/work"}`)))
	require.Empty(t, postResponseHookRequestID(json.RawMessage(
		`{"`+postResponseHookIDParam+`":7}`,
	)))
}

func TestLifecycleCommandPostResponseHookBranches(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	agent := NewAgent()
	client := newRecordingAgentClient()
	agent.setConnection(client)

	connWithoutHooks := &localAgentConnection{agent: agent}
	connWithoutHooks.enqueueSessionEstablishedHook(ctx, acp.AgentMethodSessionNew, nil, acp.NewSessionResponse{SessionId: "session-1"})

	hooks := &postResponseHooks{log: agent.log}
	conn := &localAgentConnection{agent: agent, hooks: hooks}
	closedSession := &agentSession{agent: agent, id: "closed"}
	closedSession.producers.seal()
	agent.sessions[closedSession.id] = closedSession
	conn.enqueueSessionEstablishedHook(ctx, acp.AgentMethodSessionNew, postResponseHookParams(nil, "closed"), acp.NewSessionResponse{SessionId: closedSession.id})
	conn.enqueueSessionEstablishedHook(ctx, acp.AgentMethodSessionList, nil, acp.ListSessionsResponse{})
	conn.enqueueSessionEstablishedHook(ctx, acp.AgentMethodSessionLoad, json.RawMessage(`{bad`), acp.LoadSessionResponse{})
	conn.enqueueSessionEstablishedHook(ctx, acp.AgentMethodSessionNew, nil, acp.NewSessionResponse{SessionId: "session-1"})
	require.Empty(t, postResponseHookRequestID(json.RawMessage(`{bad`)))

	result := acp.NewSessionResponse{SessionId: "missing"}
	conn.enqueueSessionEstablishedHook(ctx, acp.AgentMethodSessionNew, postResponseHookParams(nil, "1"), result)
	resultJSON, err := json.Marshal(result)
	require.NoError(t, err)
	hooks.runAfterResponseWrite([]byte(`{"jsonrpc":"2.0","id":1,"result":` + string(resultJSON) + `}`))

	require.Never(t, func() bool {
		return len(availableCommandUpdates(client.Updates())) > 0
	}, 50*time.Millisecond, 5*time.Millisecond)

	failAgent := NewAgent()
	failClient := newRecordingAgentClient()
	failClient.sessionUpdateErr = errors.New("post update failed")
	failAgent.setConnection(failClient)
	failSession := &agentSession{
		agent:             failAgent,
		id:                "session-1",
		availableCommands: []claude.SlashCommand{{Name: "help"}},
	}
	failAgent.sessions[failSession.id] = failSession
	failHooks := &postResponseHooks{log: failAgent.log}
	failConn := &localAgentConnection{agent: failAgent, hooks: failHooks}
	failResult := acp.NewSessionResponse{SessionId: failSession.id}
	failConn.enqueueSessionEstablishedHook(ctx, acp.AgentMethodSessionNew, postResponseHookParams(nil, "1"), failResult)
	failResultJSON, err := json.Marshal(failResult)
	require.NoError(t, err)
	failHooks.runAfterResponseWrite([]byte(`{"jsonrpc":"2.0","id":1,"result":` + string(failResultJSON) + `}`))

	require.Never(t, func() bool {
		return len(availableCommandUpdates(failClient.Updates())) > 0
	}, 50*time.Millisecond, 5*time.Millisecond)

	hooks.runAfterResponseWrite([]byte(`{bad`))
	hooks.runAfterResponseWrite([]byte(`{"jsonrpc":"2.0","method":"session/update"}`))
	hooks.runAfterResponseWrite([]byte(`{"jsonrpc":"2.0","id":1,"error":{"code":-1,"message":"x"}}`))
	hooks.enqueue("1", func() {})
	hooks.runAfterResponseWrite([]byte(`{"jsonrpc":"2.0","id":1,"result":{"other":true}}`))

	_, ok := establishedSessionID("unknown", nil, nil)
	require.False(t, ok)
	_, ok = establishedSessionID(acp.AgentMethodSessionResume, json.RawMessage(`{bad`), acp.ResumeSessionResponse{})
	require.False(t, ok)
}

func TestSessionEstablishedHookLatchesALifecycleStreamFailure(t *testing.T) {
	ctx := context.Background()
	agent := NewAgent()
	client := newRecordingAgentClient()
	agent.setConnection(client)
	_, err := agent.Initialize(ctx, acp.InitializeRequest{Meta: lifecycleOfferMeta(1)})
	require.NoError(t, err)

	session := &agentSession{
		agent:             agent,
		id:                "session-1",
		client:            claude.NewClient(nil, claude.Options{}, newFakeClaudeTransport()),
		availableCommands: []claude.SlashCommand{{Name: "help"}},
	}
	agent.mu.Lock()
	agent.sessions[session.id] = session
	agent.mu.Unlock()

	client.sessionUpdateErr = errors.New("snapshot delivery")

	hooks := &postResponseHooks{log: agent.log}
	conn := &localAgentConnection{agent: agent, hooks: hooks}
	result := acp.NewSessionResponse{SessionId: session.id}
	conn.enqueueSessionEstablishedHook(ctx, acp.AgentMethodSessionNew, postResponseHookParams(nil, "1"), result)
	resultJSON, err := json.Marshal(result)
	require.NoError(t, err)
	hooks.runAfterResponseWrite([]byte(`{"jsonrpc":"2.0","id":1,"result":` + string(resultJSON) + `}`))

	stream := session.lifecycleStream()
	require.NotNil(t, stream)
	require.Eventually(t, func() bool {
		stream.mu.Lock()
		defer stream.mu.Unlock()

		return stream.lost != nil
	}, time.Second, 10*time.Millisecond)
	require.Empty(t, client.Updates(), "a failed opening snapshot delivers nothing")
}

func TestPostResponseHookRecoversPanic(t *testing.T) {
	t.Parallel()

	ran := false
	runPostResponseHook(slog.New(slog.DiscardHandler), func() {
		defer func() { ran = true }()

		panic("hook panic")
	})
	require.True(t, ran)
}

func TestPostResponseHookRequestReaderTagsLifecycleRequests(t *testing.T) {
	t.Parallel()

	tagged, err := io.ReadAll(newPostResponseHookRequestReader(bytes.NewBufferString(
		`{"jsonrpc":"2.0","id":7,"method":"session/resume","params":{"sessionId":"session-1"}}` + "\n",
	)))
	require.NoError(t, err)

	var msg struct {
		Params map[string]string `json:"params"`
	}
	require.NoError(t, json.Unmarshal(tagged, &msg))
	require.Equal(t, "7", msg.Params[postResponseHookIDParam])

	smallReader := newPostResponseHookRequestReader(bytes.NewBufferString(
		`{"jsonrpc":"2.0","id":8,"method":"session/resume","params":{"sessionId":"session-1"}}`,
	))
	buf := make([]byte, 5)
	n, err := smallReader.Read(buf)
	require.NoError(t, err)
	require.Equal(t, 5, n)
	rest, err := io.ReadAll(smallReader)
	require.NoError(t, err)
	require.Contains(t, string(buf)+string(rest), postResponseHookIDParam)

	tests := [][]byte{
		[]byte(`{bad`),
		[]byte(`{"jsonrpc":"2.0","method":"session/resume","params":{}}`),
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"session/list","params":{}}`),
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"session/resume"}`),
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"session/resume","params":[]}`),
		[]byte(`{"jsonrpc":"2.0","id":1,"method":"session/resume","params":null}`),
	}
	for _, line := range tests {
		require.Equal(t, line, tagPostResponseHookRequest(line))
	}
}

func postResponseHookParams(values map[string]string, responseID string) json.RawMessage {
	params := map[string]string{postResponseHookIDParam: responseID}
	for key, value := range values {
		params[key] = value
	}

	raw, _ := json.Marshal(params)

	return raw
}

func TestConnectionInputGate(t *testing.T) {
	t.Parallel()

	reader, writer := io.Pipe()
	gate := newConnectionInputGate(reader)
	done := make(chan []byte, 1)
	go func() {
		buf := make([]byte, 4)
		n, _ := gate.Read(buf)
		done <- buf[:n]
	}()

	select {
	case <-done:
		t.Fatal("gate read before open")
	default:
	}

	gate.open()
	gate.open()
	_, err := writer.Write([]byte("test"))
	require.NoError(t, err)
	require.Equal(t, []byte("test"), <-done)
	require.NoError(t, writer.Close())
	require.NoError(t, reader.Close())
}
