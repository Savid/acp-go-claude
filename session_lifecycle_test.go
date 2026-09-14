package claudeacp

import (
	"encoding/json"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/savid/acp-go-core/lifecycle"
	"github.com/stretchr/testify/require"
)

// reduceAll runs every recorded notification through the core reducer, so
// the stream the adapter emitted is proven against the same validator the
// fixture battery drives.
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
