package claudeacp

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/require"
)

func TestScopedElicitationParams(t *testing.T) {
	t.Parallel()

	requestID := "request-1"
	raw, err := scopedElicitationParams(acp.UnstableCreateElicitationRequest{
		Url: &acp.UnstableCreateElicitationUrl{
			ElicitationId: "elicit-1",
			Message:       "Open URL",
			Mode:          "url",
			Url:           "https://example.com",
		},
	}, elicitationScope{SessionID: "session-1", TurnNonce: "turn-1", RequestID: &requestID})
	require.NoError(t, err)

	var payload map[string]any
	require.NoError(t, json.Unmarshal(raw, &payload))
	meta := requireAnyMap(t, payload["_meta"])
	route := requireAnyMap(t, meta[routeMetaKey])
	require.Equal(t, "request-1", route["requestId"])
	require.Equal(t, "url", payload["mode"])
	require.Equal(t, "https://example.com", payload["url"])

	raw, err = scopedElicitationParams(acp.UnstableCreateElicitationRequest{
		Form: &acp.UnstableCreateElicitationForm{
			Message: "Approve?",
			Mode:    "form",
		},
	}, elicitationScope{SessionID: "session-1", TurnNonce: "turn-2", ToolCallID: "tool-1"})
	require.NoError(t, err)
	require.NoError(t, json.Unmarshal(raw, &payload))
	meta = requireAnyMap(t, payload["_meta"])
	route = requireAnyMap(t, meta[routeMetaKey])
	require.Equal(t, "session-1", route["sessionId"])
	require.Equal(t, "tool-1", route["toolCallId"])
	require.Equal(t, "form", payload["mode"])

	_, err = scopedElicitationParams(acp.UnstableCreateElicitationRequest{}, elicitationScope{})
	require.Error(t, err)

	_, err = scopedElicitationParams(acp.UnstableCreateElicitationRequest{
		Form: &acp.UnstableCreateElicitationForm{
			Message: "Bad meta",
			Mode:    "form",
			Meta:    map[string]any{"bad": func() {}},
		},
	}, elicitationScope{})
	require.Error(t, err)
}

func TestRequestError(t *testing.T) {
	t.Parallel()

	live := t.Context()
	reqErr := acp.NewInvalidParams(map[string]any{"x": "y"})
	require.Nil(t, requestError(live, nil))
	require.Same(t, reqErr, requestError(live, reqErr))
	require.Equal(t, -32603, requestError(live, errors.New("boom")).Code)

	// Nobody withdrew this request; a context.Canceled reachable only by
	// unwrapping the error is an ordinary failure, not a cancel.
	require.Equal(t, -32603, requestError(live, fmt.Errorf("read prompt: %w", context.Canceled)).Code)
}

// TestRequestErrorReportsAnHonoredCancelAheadOfTheHandlerError pins the
// discriminator and its ordering: a request context cancelled with cause
// context.Canceled is an honored $/cancel_request, and it outranks whatever the
// aborted handler was carrying. Answering a withdrawn request with a typed
// -32602 would tell the peer its parameters were bad instead of that its cancel
// landed.
func TestRequestErrorReportsAnHonoredCancelAheadOfTheHandlerError(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancelCause(t.Context())
	cancel(context.Canceled)

	for name, err := range map[string]error{
		"embedded request error": errors.Join(acp.NewInvalidParams(map[string]any{"x": "y"}), context.Canceled),
		"bare cancellation":      context.Canceled,
		"plain failure":          errors.New("boom"),
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			got := requestError(ctx, err)
			require.NotNil(t, got)
			require.Equal(t, -32800, got.Code)
		})
	}
}

// TestRequestErrorReportsATornDownConnectionByItsOwnError pins that transport
// teardown is not a cancel. The SDK cancels the parent context with the
// transport failure as the cause rather than context.Canceled, so a request
// killed by it reports what actually broke.
func TestRequestErrorReportsATornDownConnectionByItsOwnError(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithCancelCause(t.Context())
	defer cancel(nil)

	cancel(errors.New("connection closed"))

	invalid := acp.NewInvalidParams(map[string]any{"x": "y"})
	require.Same(t, invalid, requestError(ctx, errors.Join(invalid, context.Canceled)))
	require.Equal(t, -32603, requestError(ctx, errors.New("boom")).Code)
}

// TestRequestErrorReportsAnExpiredDeadlineAsAnInternalFailure pins that an
// adapter-internal deadline is a failure of the turn and never a withdrawal:
// its cause is context.DeadlineExceeded, which the cancel check excludes by
// name rather than by accident.
func TestRequestErrorReportsAnExpiredDeadlineAsAnInternalFailure(t *testing.T) {
	t.Parallel()

	ctx, cancel := context.WithTimeout(t.Context(), time.Nanosecond)
	defer cancel()

	<-ctx.Done()

	require.Equal(t, -32603, requestError(ctx, context.DeadlineExceeded).Code)
	require.Equal(t, -32603, requestError(ctx, errors.New("boom")).Code)
}
