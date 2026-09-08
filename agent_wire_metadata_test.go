package claudeacp

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/lifecycle"
	"github.com/savid/acp-go-claude/internal/mapper"
	"github.com/stretchr/testify/require"
)

func TestWireRouteNumbersKeepExactValuesAndValidationOrder(t *testing.T) {
	t.Parallel()
	for _, version := range []string{"1", "1.0", "1e0", "1.0000000000000001", "1e400", "-1e-400"} {
		t.Run(version, func(t *testing.T) {
			t.Parallel()
			params := json.RawMessage(`{"sessionId":"session","prompt":[],"_META":{"acp-go.dev/route":{"version":` + version + `,"turnNonce":"turn"},"acp-go.dev/lifecycle":{"version":1e400}}}`)
			request, decodeErr := decodeLocalAgentParams[acp.PromptRequest, *acp.PromptRequest](params)
			require.Nil(t, decodeErr)
			agent := NewAgent()
			agent.retainNegotiatedLifecycle(lifecycle.Negotiated{Version: 1})
			session := &agentSession{agent: agent}
			_, err := session.Prompt(t.Context(), request)
			var refusal *acp.RequestError
			require.ErrorAs(t, err, &refusal)
			field := routeMemberPath(routeFieldVer)
			if version == "1" || version == "1.0" || version == "1e0" {
				field = lifecycle.MetaPath + ".version"
			}
			require.Equal(t, map[string]any{"error": "unsupported", "field": field}, refusal.Data)
		})
	}
}

func TestServeOwnedNumberOverflowKeepsEarlierRefusals(t *testing.T) {
	t.Parallel()
	initialize := `{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1,"clientCapabilities":{}}}`
	control := `{"jsonrpc":"2.0","id":2,"method":"session/prompt","params":{"sessionId":"missing","prompt":[]}}`
	overflow := `{"jsonrpc":"2.0","id":3,"method":"session/prompt","params":{"sessionId":"missing","prompt":[],"_meta":{"acp-go.dev/lifecycle":{"version":1e400}}}}`
	responses := lifecycleWireResponses(t, nil, initialize, control, overflow)
	require.NotNil(t, responses[1].Error)
	require.Equal(t, responses[1].Error, responses[2].Error)
	invalidOptions := []Option{WithImageLimits(ImageLimits{MaxInputBytesPerImage: -1})}
	responses = lifecycleWireResponses(t, invalidOptions,
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":1,"_META":{"acp-go.dev/lifecycle":{"version":1e400}}}}`)
	require.NotNil(t, responses[0].Error)
	require.Equal(t, -32603, responses[0].Error.Code)
}

func TestWireRetentionPreservesForeignMapMerging(t *testing.T) {
	t.Parallel()
	params := json.RawMessage(`{"protocolVersion":1,"_meta":{"first":1,"merged":{"a":1}},"_META":{"second":2,"merged":{"b":2},"acp-go.dev/lifecycle":{"version":1}}}`)
	var expected acp.InitializeRequest
	require.NoError(t, json.Unmarshal(params, &expected))
	actual, err := decodeLocalAgentParams[acp.InitializeRequest, *acp.InitializeRequest](params)
	require.Nil(t, err)
	delete(expected.Meta, lifecycle.MetaKey)
	delete(actual.Meta, lifecycle.MetaKey)
	require.Equal(t, expected.Meta, actual.Meta)
}

type wireHandoffReader struct{ opens int }

func (r *wireHandoffReader) OpenHandoffImage(context.Context, string) (io.ReadCloser, error) {
	r.opens++

	return nil, &mapper.HandoffPathError{Verdict: mapper.HandoffPathNotAllowed, Message: "test read boundary"}
}

func TestWireHandoffNumbersPreserveSelectionAndNoIOGates(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name, version, size, data, message string
		unsetRoot                          bool
	}{
		{name: "fractional version", version: "1.0000000000000001", size: "1", message: "unsupported handoff metadata version"},
		{name: "fractional size", version: "1", size: "1.0000000000000001", message: "handoff sizeBytes must be a non-negative integer"},
		{name: "tiny negative size", version: "1", size: "-1e-400", message: "handoff sizeBytes must be a non-negative integer"},
		{name: "overflow", version: "1", size: "1e400", message: "handoff sizeBytes must be a non-negative integer"},
		{name: "unset root first", version: "1e400", size: "-1e-400", unsetRoot: true, message: "no handoff read root is configured"},
		{name: "embedded data wins", version: "1e400", size: "-1e-400", data: outputFixtureBase64(t, "valid.png")},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()
			params := json.RawMessage(`{"sessionId":"session","prompt":[{"type":"image","data":"` + test.data + `","mimeType":"image/png","uri":"file:///unused","_META":{"acp-go.dev/handoff":{"version":` + test.version + `,"sizeBytes":` + test.size + `,"digest":"` + strings.Repeat("0", 64) + `"}}}]}`)
			request, decodeErr := decodeLocalAgentParams[acp.PromptRequest, *acp.PromptRequest](params)
			require.Nil(t, decodeErr)
			reader := &wireHandoffReader{}
			var selected mapper.HandoffFileReader = reader
			if test.unsetRoot {
				selected = nil
			}
			_, err := mapper.PromptToClaude(t.Context(), request.Prompt, nil, mapper.ImageInputLimits{}, selected)
			if test.message == "" {
				require.NoError(t, err)
			} else {
				details := requireHandoffEnvelope(t, err, "invalid_handoff")
				require.Equal(t, test.message, details["message"])
				require.Equal(t, 0, details["index"])
			}
			require.Zero(t, reader.opens)
		})
	}
}
