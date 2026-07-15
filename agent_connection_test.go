package claudeacp

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

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

	reqErr := acp.NewInvalidParams(map[string]any{"x": "y"})
	require.Nil(t, requestError(nil))
	require.Same(t, reqErr, requestError(reqErr))
	require.Equal(t, -32800, requestError(context.Canceled).Code)
	require.Equal(t, -32603, requestError(errors.New("boom")).Code)
}
