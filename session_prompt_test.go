package claudeacp

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-core/wire"
	"github.com/stretchr/testify/require"
)

func TestPromptMediaReachesNativeShape(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	bytes, err := base64.StdEncoding.DecodeString(tinyPNG)
	require.NoError(t, err)
	path := filepath.Join(root, "input.png")
	require.NoError(t, os.WriteFile(path, bytes, 0600))
	digest := sha256.Sum256(bytes)
	uri := "file://" + path
	handoff := acp.ImageBlock("", "image/png")
	handoff.Image.Uri = &uri
	handoff.Image.Meta = map[string]any{wire.HandoffKey: map[string]any{"version": 1, "digest": hex.EncodeToString(digest[:]), "sizeBytes": len(bytes)}}
	agent := NewAgent(WithInputHandoffRoot(root))
	session := agent.newSession(sessionStart{cwd: t.TempDir()})
	inline := acp.ImageBlock(tinyPNG, "image/png")
	mapped, err := session.mapPrompt(t.Context(), []acp.ContentBlock{acp.TextBlock("before"), inline, acp.TextBlock("after")})
	require.NoError(t, err)
	require.Len(t, mapped.content, 3)
	require.Equal(t, tinyPNG, mapped.content[1].Source.Data)
	require.Equal(t, "before", mapped.content[0].Text)
	require.Equal(t, "after", mapped.content[2].Text)
	transported, err := session.mapPrompt(t.Context(), []acp.ContentBlock{handoff})
	require.NoError(t, err)
	require.Equal(t, mapped.content[1], transported.content[0])
	encoded, err := json.Marshal(transported.content)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), path)
	inline.Image.Uri = &uri
	forwarded, err := session.mapPrompt(t.Context(), []acp.ContentBlock{inline})
	require.NoError(t, err)
	require.Equal(t, tinyPNG, forwarded.content[0].Source.Data)
}

func TestPromptImageGateFailures(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		name, data, mime string
		limit            int64
	}{
		{nativeBase64, "%%%", "image/png", 0},
		{"mime", tinyPNG, "image/jpeg", 0},
		{"format", tinyPNG, "image/svg+xml", 0},
		{"size", tinyPNG, "image/png", 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			agent := NewAgent(WithImageLimits(ImageLimits{MaxInputBytesPerImage: tc.limit}))
			session := agent.newSession(sessionStart{cwd: t.TempDir()})
			_, err := session.mapPrompt(t.Context(), []acp.ContentBlock{acp.ImageBlock(tc.data, tc.mime)})
			require.Equal(t, -32602, requestErrorCode(t, err))
			require.Equal(t, "prompt.image", requestErrorData(t, err)["field"])
		})
	}
}

func TestRequestCancellationKeepsOtherSessionRunning(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.initialize(withLifecycle())
	first, second := h.newSession(), h.newSession()
	ctx, cancel := context.WithCancel(h.ctx())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		request := TextPromptRequest(first.SessionId, "BLOCK")
		request.Meta = promptMeta(1)
		_, err := h.conn.Prompt(ctx, request)
		done <- err
	}()
	h.rec.waitFor(t, func(updates []acp.SessionNotification) bool { return len(lifecycleEvents(updates)) >= 4 })
	cancel()
	require.Error(t, <-done)
	response, err := h.prompt(second.SessionId, "independent", promptMeta(2))
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, response.StopReason)
	h.rec.waitFor(t, func(updates []acp.SessionNotification) bool {
		for _, update := range updates {
			if update.SessionId != first.SessionId {
				continue
			}
			events := lifecycleEvents([]acp.SessionNotification{update})
			if len(events) > 0 && events[0]["state"] == "idle" {
				return true
			}
		}

		return false
	})
	require.True(t, reduceAll(t, first.SessionId, h.rec.snapshot()).Settled())
	require.True(t, reduceAll(t, second.SessionId, h.rec.snapshot()).Settled())
}

func TestNativeFailureAndTimeout(t *testing.T) {
	t.Parallel()
	for _, tc := range []struct {
		prompt, cause string
		timeout       time.Duration
	}{{"ERROR", "provider", 0}, {"BLOCK", "timeout", 200 * time.Millisecond}} {
		t.Run(tc.cause, func(t *testing.T) {
			t.Parallel()
			h := newHarness(t, WithTurnTimeout(tc.timeout))
			h.initialize(withLifecycle())
			session := h.newSession()
			_, err := h.prompt(session.SessionId, tc.prompt, promptMeta(1))
			data := requestErrorData(t, err)
			require.Equal(t, "claude_turn_failed", data["error"])
			require.Equal(t, tc.cause, data["cause"])
			require.True(t, reduceAll(t, session.SessionId, h.rec.snapshot()).Settled())
		})
	}
}

func TestNativeNoiseLeavesACPUsable(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.initialize()
	session := h.newSession()
	_, err := h.prompt(session.SessionId, "NOISE", nil)
	require.Equal(t, "claude_turn_failed", requestErrorData(t, err)["error"])
	next := h.newSession()
	response, err := h.prompt(next.SessionId, "HELLO", nil)
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, response.StopReason)
}
