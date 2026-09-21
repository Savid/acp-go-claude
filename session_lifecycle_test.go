package claudeacp

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	acpcore "github.com/savid/acp-go-core"
	"github.com/savid/acp-go-core/lifecycle"
	"github.com/savid/acp-go-core/wire"
	"github.com/stretchr/testify/require"
)

// reduceAll feeds every notification recorded for one session to the core
// lifecycle reducer and returns the state it reaches.
func reduceAll(t *testing.T, sessionID acp.SessionId, updates []acp.SessionNotification) lifecycle.State {
	t.Helper()

	reducer := lifecycle.NewReducer(lifecycle.Options{Negotiated: lifecycle.Negotiated{Version: 1, UpdatesOutsidePrompt: true, ActivityKinds: []lifecycle.ActivityKind{}}})

	for _, update := range updates {
		if update.SessionId != sessionID {
			continue
		}

		params, err := json.Marshal(update)
		require.NoError(t, err)

		if err := reducer.ReduceSessionUpdate(params); err != nil {
			require.ErrorIs(t, err, lifecycle.ErrNoEnvelope, "reducer refused a notification")
		}
	}

	return reducer.State()
}

func TestAgentOriginCycleFollowsPrompt(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.initialize(withLifecycle())
	session := h.newSession(WithSessionRawEvents(true))
	_, err := h.prompt(session.SessionId, "AGENTWORK", promptMeta(1))
	require.NoError(t, err)
	h.rec.waitFor(t, func(updates []acp.SessionNotification) bool { return len(lifecycleEvents(updates)) >= 6 })
	state := reduceAll(t, session.SessionId, h.rec.snapshot())
	require.True(t, state.Settled())
	require.Len(t, state.Turns, 2)
	require.Contains(t, agentText(h.rec.snapshot()), "background")
	h.rec.mu.Lock()
	defer h.rec.mu.Unlock()
	starts := 0
	for _, raw := range h.rec.raw {
		var notification struct {
			Event claude.Event `json:"event"`
		}
		require.NoError(t, json.Unmarshal(raw, &notification))
		if event := notification.Event.Event; event != nil && event.Type == nativeMessageStart {
			starts++
		}
	}
	require.Equal(t, 2, starts)
}

func TestCloseBackgroundCycleRequiresCommit(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(map[bool]string{false: "committed", true: "failed"}[fail], func(t *testing.T) {
			store := &recoveryFaultStore{SessionStore: acpcore.NewInMemorySessionStore()}
			h := newHarness(t, WithSessionStore(store))
			h.initialize(withLifecycle())
			created := h.newSession()
			_, err := h.prompt(created.SessionId, "AGENTHANG", promptMeta(1))
			require.NoError(t, err)
			h.rec.waitFor(t, func(updates []acp.SessionNotification) bool {
				events := lifecycleEvents(updates)

				return len(events) >= 5 && events[4]["state"] == "running"
			})
			before := len(lifecycleEvents(h.rec.snapshot()))
			store.fail.Store(fail)
			_, err = h.conn.CloseSession(h.ctx(), acp.CloseSessionRequest{SessionId: created.SessionId})
			store.fail.Store(false)
			if fail {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
			}
			terminal := 0
			for _, event := range lifecycleEvents(h.rec.snapshot())[before:] {
				if event["state"] == "idle" {
					terminal++
					require.Equal(t, "cancelled", event["outcome"])
				}
			}
			if fail {
				require.Zero(t, terminal)
			} else {
				require.Equal(t, 1, terminal)
			}
		})
	}
}

// TestCapturedNativeAgentOrigin replays the captured Claude Code stream-json
// frames under testdata/native through the real decoder and the session's own
// event handling with no prompt in flight: the turn Claude Code ran on its own
// to report a finished background task opens an agent-origin cycle on its
// first stream record and settles on its result.
func TestCapturedNativeAgentOrigin(t *testing.T) {
	a := NewAgent(testOptions(t)...)
	t.Cleanup(func() { _ = a.Close() })
	rec := newRecorder()
	a.attach(rec, nil)
	request := acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber}
	withLifecycle()(&request)
	_, err := a.Initialize(t.Context(), request)
	require.NoError(t, err)
	created, err := a.NewSession(t.Context(), wire.NewSessionRequest(t.TempDir()))
	require.NoError(t, err)
	s, err := a.session(t.Context(), created.SessionId)
	require.NoError(t, err)
	s.mu.Lock()
	rt := s.runtime
	require.Nil(t, s.turn)
	s.mu.Unlock()

	data, err := os.ReadFile("testdata/native/agent-origin.json")
	require.NoError(t, err)
	data = []byte(strings.ReplaceAll(string(data), "fixture-session", s.nativeID))
	var frames []json.RawMessage
	require.NoError(t, json.Unmarshal(data, &frames))
	require.NotEmpty(t, frames)

	before := len(lifecycleEvents(rec.snapshot()))
	for _, frame := range frames {
		var event claude.Event
		require.NoError(t, json.Unmarshal(frame, &event))
		event.Raw = frame
		s.handleEvent(t.Context(), rt, event)
	}

	events := lifecycleEvents(rec.snapshot())[before:]
	require.Equal(t, []string{"state_update:running", "state_update:idle"}, eventTypes(events))
	require.Equal(t, "activity", events[0]["cause"])
	require.Equal(t, "activity", events[1]["cause"])
	require.Equal(t, "success", events[1]["outcome"])
	require.Contains(t, agentText(rec.snapshot()), "background agent")
	s.mu.Lock()
	require.Nil(t, s.cycle)
	s.mu.Unlock()
}

// negotiatedAnswer decodes the lifecycle answer the initialize response carries.
func negotiatedAnswer(t *testing.T, response acp.InitializeResponse) lifecycle.Negotiated {
	t.Helper()

	raw, err := json.Marshal(response.Meta[wire.LifecycleKey])
	require.NoError(t, err)

	var negotiated lifecycle.Negotiated
	require.NoError(t, json.Unmarshal(raw, &negotiated))

	return negotiated
}

// sessionFrames renders one session's recorded notifications as the payloads
// the host received, in delivery order.
func sessionFrames(t *testing.T, updates []acp.SessionNotification, sessionID acp.SessionId) []json.RawMessage {
	t.Helper()

	var frames []json.RawMessage

	for _, update := range updates {
		if update.SessionId != sessionID {
			continue
		}

		encoded, err := json.Marshal(update)
		require.NoError(t, err)
		frames = append(frames, encoded)
	}

	return frames
}

func idleTransitions(updates []acp.SessionNotification, sessionID acp.SessionId) int {
	count := 0

	for _, update := range updates {
		if update.SessionId != sessionID {
			continue
		}

		envelope, _ := update.Meta[wire.LifecycleKey].(map[string]any)
		event, _ := envelope["event"].(map[string]any)

		if event["type"] == "state_update" && event["state"] == "idle" {
			count++
		}
	}

	return count
}

// TestOrdinaryContentAttributesToTheForeground proves the contract's
// attribution rule over a recorded stream: every content update arrives
// while the foreground is live, and no vendor namespace hints a turn or
// message. AGENTWORK leaves a background turn behind its prompt.
func TestOrdinaryContentAttributesToTheForeground(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	negotiated := negotiatedAnswer(t, h.initialize(withLifecycle()))
	session := h.newSession()

	for n, text := range []string{"HELLO", "AGENTWORK"} {
		_, err := h.prompt(session.SessionId, text, promptMeta(n+1))
		require.NoError(t, err)
	}

	h.rec.waitFor(t, func(updates []acp.SessionNotification) bool {
		return idleTransitions(updates, session.SessionId) >= 3
	})

	updates := h.rec.snapshot()
	require.NotZero(t, contentUpdates(updates, session.SessionId), "the stream carried content to attribute")
	require.NoError(t, lifecycle.CheckAttribution(negotiated, sessionFrames(t, updates, session.SessionId)))
}

func contentUpdates(updates []acp.SessionNotification, sessionID acp.SessionId) int {
	count := 0

	for _, update := range updates {
		if update.SessionId == sessionID && (update.Update.AgentMessageChunk != nil || update.Update.ToolCall != nil) {
			count++
		}
	}

	return count
}

// streamFrame is one delivered lifecycle envelope's ordering identity and the
// event it carried, rendered as the canonical JSON a consumer compares.
type streamFrame struct {
	stream   string
	sequence float64
	event    string
}

// streamFrames reads the envelopes one recorder observed for a session.
func streamFrames(t *testing.T, updates []acp.SessionNotification, sessionID acp.SessionId) []streamFrame {
	t.Helper()

	frames := make([]streamFrame, 0, len(updates))

	for _, update := range updates {
		envelope, ok := update.Meta[wire.LifecycleKey].(map[string]any)
		if !ok || update.SessionId != sessionID {
			continue
		}

		stream, ok := envelope["streamId"].(string)
		require.True(t, ok, "an envelope named no stream")
		sequence, ok := envelope["sequence"].(float64)
		require.True(t, ok, "an envelope named no sequence")
		event, err := json.Marshal(envelope["event"])
		require.NoError(t, err)

		frames = append(frames, streamFrame{stream: stream, sequence: sequence, event: string(event)})
	}

	return frames
}

// Resuming a stored session through a new adapter must open a fresh stream.
// Each live adapter retains its stream across prompts, and each stream/sequence
// pair must identify the same content throughout the conversation.
func TestResumedProcessNamesItsOwnIncarnation(t *testing.T) {
	t.Parallel()

	home := t.TempDir()
	cwd := t.TempDir()
	store := acpcore.NewInMemorySessionStore()

	earlier := newHarness(t, WithHome(home), WithSessionStore(store))
	earlier.initialize(withLifecycle())
	created, err := earlier.conn.NewSession(earlier.ctx(), wire.NewSessionRequest(cwd))
	require.NoError(t, err)

	for n, text := range []string{"first", "second"} {
		_, promptErr := earlier.prompt(created.SessionId, text, promptMeta(n+1))
		require.NoError(t, promptErr)
	}

	_, err = earlier.conn.CloseSession(earlier.ctx(), acp.CloseSessionRequest{SessionId: created.SessionId})
	require.NoError(t, err)

	later := newHarness(t, WithHome(home), WithSessionStore(store))
	later.initialize(withLifecycle())
	_, err = later.conn.ResumeSession(later.ctx(), wire.ResumeSessionRequest(created.SessionId, cwd))
	require.NoError(t, err)
	_, err = later.prompt(created.SessionId, "third", promptMeta(3))
	require.NoError(t, err)

	before := streamFrames(t, earlier.rec.snapshot(), created.SessionId)
	after := streamFrames(t, later.rec.snapshot(), created.SessionId)
	require.NotEmpty(t, before)
	require.NotEmpty(t, after)

	firstStream := before[0].stream
	for _, frame := range before {
		require.Equal(t, firstStream, frame.stream, "one live process retained one incarnation across its prompts")
	}

	secondStream := after[0].stream
	for _, frame := range after {
		require.Equal(t, secondStream, frame.stream, "the resumed process retained its own incarnation")
	}

	require.NotEqual(t, firstStream, secondStream, "the resumed process reopened the name the earlier process published")
	require.True(t, strings.HasPrefix(firstStream, string(created.SessionId)+":"))
	require.True(t, strings.HasPrefix(secondStream, string(created.SessionId)+":"))

	require.Equal(t, float64(1), before[0].sequence)
	require.Equal(t, float64(1), after[0].sequence, "a fresh incarnation opens at sequence 1")
	require.Greater(t, before[len(before)-1].sequence, float64(1), "the earlier process published past the sequence the resumed one reuses")

	// The rule the host enforces: one (streamId, sequence) names one event
	// forever. The earlier process's frames and the resumed process's frames
	// are held to it together, so a resumed conversation can never present
	// itself as a rewrite of the stream already recorded.
	recorded := make(map[string]string, len(before)+len(after))

	for _, frame := range append(append([]streamFrame(nil), before...), after...) {
		identity := fmt.Sprintf("%s#%d", frame.stream, uint64(frame.sequence))
		if previous, seen := recorded[identity]; seen {
			require.Equal(t, previous, frame.event, "two different events claimed %s", identity)

			continue
		}

		recorded[identity] = frame.event
	}
}
