package claudeacp

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/stretchr/testify/require"
)

type rawEventPayload struct {
	Meta      map[string]any `json:"_meta"` //nolint:tagliatelle // ACP wire format.
	SessionId string         `json:"sessionId"`
	Sequence  int            `json:"sequence"`
	Source    string         `json:"source"`
	Event     map[string]any `json:"event"`
}

func decodeRawEvents(t *testing.T, conn *recordingAgentClient) []rawEventPayload {
	t.Helper()

	events := make([]rawEventPayload, 0, len(conn.Extensions()))
	for _, ext := range conn.Extensions() {
		require.Equal(t, RawEventMethod, ext.method)
		require.True(t, json.Valid(ext.params), "raw event payload must be valid JSON")

		var payload rawEventPayload
		require.NoError(t, json.Unmarshal(ext.params, &payload))
		events = append(events, payload)
	}

	return events
}

func newRawEventSession(t *testing.T, id string, enabled bool) (*agentSession, *recordingAgentClient) {
	t.Helper()

	agent := NewAgent()
	conn := newRecordingAgentClient()
	agent.setConnection(conn)

	session := &agentSession{agent: agent, id: acp.SessionId(id)}
	if enabled {
		session.rawMessages = rawMessageConfig{All: true}
	}

	return session, conn
}

func oversizedMessage() *claude.SystemMessage {
	return &claude.SystemMessage{Raw: map[string]any{"type": "system", "data": strings.Repeat("x", rawEventMaxBytes)}}
}

func normalMessage() *claude.SystemMessage {
	return &claude.SystemMessage{Raw: map[string]any{"type": "system"}}
}

// Case 1 — an oversized event is emitted as the fixed marker, never dropped, with
// the envelope intact.
func TestRawEventOversizeMarker(t *testing.T) {
	t.Parallel()

	ctx := withTurnRoute(context.Background(), "turn-raw")
	session, conn := newRawEventSession(t, "session-1", true)

	require.NoError(t, session.emitRawClaudeMessage(ctx, oversizedMessage()))

	events := decodeRawEvents(t, conn)
	require.Len(t, events, 1)
	require.Equal(t, "session-1", events[0].SessionId)
	require.Equal(t, 1, events[0].Sequence)
	require.Equal(t, "claude", events[0].Source)
	require.Equal(t, map[string]any{routeMetaKey: map[string]any{
		routeFieldVer: float64(routeVersion), routeFieldTurn: "turn-raw",
	}}, events[0].Meta)
	require.Equal(t, true, events[0].Event[rawEventFieldTruncated])
	require.Equal(t, rawEventReasonOversize, events[0].Event[rawEventFieldReason])
	require.EqualValues(t, rawEventMaxBytes, events[0].Event[rawEventFieldMaxBytes])
	sizeBytes, ok := events[0].Event[rawEventFieldSizeBytes].(float64)
	require.True(t, ok)
	require.Greater(t, sizeBytes, float64(rawEventMaxBytes))
}

func TestRawEventFinalPayloadBoundaryIncludesRouteMeta(t *testing.T) {
	t.Parallel()

	payload := map[string]any{
		acpFieldSessionID:     "session-1",
		rawEventFieldSequence: int64(1),
		rawEventFieldSource:   claudeMetaKey,
		rawEventFieldEvent:    map[string]any{"data": ""},
		"_meta":               turnRouteMeta(strings.Repeat("n", routeTurnNonceMaxBytes)),
	}
	empty, err := json.Marshal(payload)
	require.NoError(t, err)
	padding := rawEventMaxBytes - len(empty)
	require.Positive(t, padding)
	payload[rawEventFieldEvent] = map[string]any{"data": strings.Repeat("x", padding)}

	capped, err := capRawEventPayload(payload)
	require.NoError(t, err)
	encoded, err := json.Marshal(capped)
	require.NoError(t, err)
	require.Len(t, encoded, rawEventMaxBytes)
	require.NotContains(t, capped[rawEventFieldEvent], rawEventFieldTruncated)

	payload[rawEventFieldEvent] = map[string]any{"data": strings.Repeat("x", padding+1)}
	capped, err = capRawEventPayload(payload)
	require.NoError(t, err)
	encoded, err = json.Marshal(capped)
	require.NoError(t, err)
	require.LessOrEqual(t, len(encoded), rawEventMaxBytes)
	marker, ok := capped[rawEventFieldEvent].(map[string]any)
	require.True(t, ok)
	require.Equal(t, rawEventReasonOversize, marker[rawEventFieldReason])
	require.Equal(t, rawEventMaxBytes+1, marker[rawEventFieldSizeBytes])
}

func TestRawEventFinalPayloadRejectsUnboundedInternalRoute(t *testing.T) {
	t.Parallel()

	hugeNonce := strings.Repeat("n", rawEventMaxBytes)
	payload := map[string]any{
		acpFieldSessionID:     "session-1",
		rawEventFieldSequence: int64(1),
		rawEventFieldSource:   claudeMetaKey,
		rawEventFieldEvent:    normalMessage().RawMessage(),
		"_meta":               turnRouteMeta(hugeNonce),
	}

	_, err := capRawEventPayload(payload)
	require.ErrorContains(t, err, "exceeds")

	session, conn := newRawEventSession(t, "session-1", true)
	require.NoError(t, session.emitRawClaudeMessage(withTurnRoute(context.Background(), hugeNonce), normalMessage()))
	require.Empty(t, conn.Extensions())
	require.Zero(t, session.rawEventSequence)

	payload["_meta"] = map[string]any{"bad": make(chan int)}
	_, err = capRawEventPayload(payload)
	require.ErrorContains(t, err, "marshal capped")
}

// Case 2 — the per-session sequence is contiguous, starts at 1, and every event
// (normal or marker) consumes exactly one sequence.
func TestRawEventContiguousSequence(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	session, conn := newRawEventSession(t, "session-1", true)

	require.NoError(t, session.emitRawClaudeMessage(ctx, normalMessage()))
	require.NoError(t, session.emitRawClaudeMessage(ctx, oversizedMessage()))
	require.NoError(t, session.emitRawClaudeMessage(ctx, normalMessage()))
	require.NoError(t, session.emitRawClaudeMessage(ctx, normalMessage()))

	events := decodeRawEvents(t, conn)
	require.Len(t, events, 4)
	for index, event := range events {
		require.Equal(t, index+1, event.Sequence)
	}
}

// Case 3 — sequences are per-session: two sessions each start at 1 and stay
// contiguous, independent of one another.
func TestRawEventCrossSessionIsolation(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	agent := NewAgent()
	conn := newRecordingAgentClient()
	agent.setConnection(conn)

	first := &agentSession{agent: agent, id: "session-a", rawMessages: rawMessageConfig{All: true}}
	second := &agentSession{agent: agent, id: "session-b", rawMessages: rawMessageConfig{All: true}}

	require.NoError(t, first.emitRawClaudeMessage(ctx, normalMessage()))
	require.NoError(t, second.emitRawClaudeMessage(ctx, normalMessage()))
	require.NoError(t, first.emitRawClaudeMessage(ctx, normalMessage()))
	require.NoError(t, second.emitRawClaudeMessage(ctx, normalMessage()))

	var seqA, seqB []int
	for _, event := range decodeRawEvents(t, conn) {
		switch event.SessionId {
		case "session-a":
			seqA = append(seqA, event.Sequence)
		case "session-b":
			seqB = append(seqB, event.Sequence)
		}
	}

	require.Equal(t, []int{1, 2}, seqA)
	require.Equal(t, []int{1, 2}, seqB)
}

// Case 4 — every emitted notification carries valid-JSON event data, including
// an over-limit event and an event that fails to marshal.
func TestRawEventValidJSONInvariant(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	session, conn := newRawEventSession(t, "session-1", true)

	require.NoError(t, session.emitRawClaudeMessage(ctx, normalMessage()))
	require.NoError(t, session.emitRawClaudeMessage(ctx, oversizedMessage()))
	// A message whose raw payload cannot marshal still emits a valid marker.
	require.NoError(t, session.emitRawClaudeMessage(ctx, &claude.SystemMessage{
		Raw: map[string]any{"type": "system", "bad": func() {}},
	}))

	events := decodeRawEvents(t, conn)
	require.Len(t, events, 3)
	require.NotContains(t, events[0].Event, rawEventFieldTruncated)
	require.Equal(t, rawEventReasonOversize, events[1].Event[rawEventFieldReason])
	require.Equal(t, rawEventReasonUnserializable, events[2].Event[rawEventFieldReason])
	require.NotContains(t, events[2].Event, rawEventFieldSizeBytes)
}

// Case 5 — a raw-event emit failure is optional and does not advance sequence.
func TestRawEventEmitFailureIsContained(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	session, conn := newRawEventSession(t, "session-1", true)
	conn.extensionErr = errors.New("notify failed")

	require.NoError(t, session.emitRawClaudeMessage(ctx, normalMessage()))
	require.Empty(t, conn.Extensions())
	require.Zero(t, session.rawEventSequence)

	conn.extensionErr = nil
	require.NoError(t, session.emitRawClaudeMessage(ctx, normalMessage()))
	events := decodeRawEvents(t, conn)
	require.Len(t, events, 1)
	require.Equal(t, 1, events[0].Sequence)
	require.EqualValues(t, 1, session.rawEventSequence)
}

// Case 6 — with raw events disabled (the default), no notifications are emitted
// regardless of event volume.
func TestRawEventDefaultOff(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	session, conn := newRawEventSession(t, "session-1", false)

	for range 5 {
		require.NoError(t, session.emitRawClaudeMessage(ctx, normalMessage()))
	}

	require.Empty(t, conn.Extensions())
}
