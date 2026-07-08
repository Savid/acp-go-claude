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

	ctx := context.Background()
	session, conn := newRawEventSession(t, "session-1", true)

	session.emitRawClaudeMessage(ctx, oversizedMessage())

	events := decodeRawEvents(t, conn)
	require.Len(t, events, 1)
	require.Equal(t, "session-1", events[0].SessionId)
	require.Equal(t, 1, events[0].Sequence)
	require.Equal(t, "claude", events[0].Source)
	require.Equal(t, true, events[0].Event[rawEventFieldTruncated])
	require.Equal(t, rawEventReasonOversize, events[0].Event[rawEventFieldReason])
	require.EqualValues(t, rawEventMaxBytes, events[0].Event[rawEventFieldMaxBytes])
	sizeBytes, ok := events[0].Event[rawEventFieldSizeBytes].(float64)
	require.True(t, ok)
	require.Greater(t, sizeBytes, float64(rawEventMaxBytes))
}

// Case 2 — the per-session sequence is contiguous, starts at 1, and every event
// (normal or marker) consumes exactly one sequence.
func TestRawEventContiguousSequence(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	session, conn := newRawEventSession(t, "session-1", true)

	session.emitRawClaudeMessage(ctx, normalMessage())
	session.emitRawClaudeMessage(ctx, oversizedMessage())
	session.emitRawClaudeMessage(ctx, normalMessage())
	session.emitRawClaudeMessage(ctx, normalMessage())

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

	first.emitRawClaudeMessage(ctx, normalMessage())
	second.emitRawClaudeMessage(ctx, normalMessage())
	first.emitRawClaudeMessage(ctx, normalMessage())
	second.emitRawClaudeMessage(ctx, normalMessage())

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

	session.emitRawClaudeMessage(ctx, normalMessage())
	session.emitRawClaudeMessage(ctx, oversizedMessage())
	// A message whose raw payload cannot marshal still emits a valid marker.
	session.emitRawClaudeMessage(ctx, &claude.SystemMessage{
		Raw: map[string]any{"type": "system", "bad": func() {}},
	})

	events := decodeRawEvents(t, conn)
	require.Len(t, events, 3)
	require.NotContains(t, events[0].Event, rawEventFieldTruncated)
	require.Equal(t, rawEventReasonOversize, events[1].Event[rawEventFieldReason])
	require.Equal(t, rawEventReasonUnserializable, events[2].Event[rawEventFieldReason])
	require.NotContains(t, events[2].Event, rawEventFieldSizeBytes)
}

// Case 5 — a raw-event emit failure never aborts the turn: it is recorded
// internally and never surfaced on the wire.
func TestRawEventEmitFailureDoesNotFailTurn(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	session, conn := newRawEventSession(t, "session-1", true)
	conn.extensionErr = errors.New("notify failed")

	session.emitRawClaudeMessage(ctx, normalMessage())
	require.Empty(t, conn.Extensions())
}

// Case 6 — with raw events disabled (the default), no notifications are emitted
// regardless of event volume.
func TestRawEventDefaultOff(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	session, conn := newRawEventSession(t, "session-1", false)

	for range 5 {
		session.emitRawClaudeMessage(ctx, normalMessage())
	}

	require.Empty(t, conn.Extensions())
}
