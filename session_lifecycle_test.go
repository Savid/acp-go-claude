package claudeacp

import (
	"encoding/json"
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
