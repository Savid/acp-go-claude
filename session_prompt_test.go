package claudeacp

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	stdimage "image"
	"image/png"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/require"

	"context"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"time"

	acpcore "github.com/savid/acp-go-core"
	"github.com/savid/acp-go-core/wire"
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
		request := wire.TextPromptRequest(first.SessionId, "BLOCK")
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

func TestNativeFailure(t *testing.T) {
	t.Parallel()
	h := newHarness(t)
	h.initialize(withLifecycle())
	session := h.newSession()
	_, err := h.prompt(session.SessionId, "ERROR", promptMeta(1))
	data := requestErrorData(t, err)
	require.Equal(t, "claude_turn_failed", data["error"])
	require.Equal(t, "provider", data["cause"])
	require.True(t, reduceAll(t, session.SessionId, h.rec.snapshot()).Settled())
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

func promptRaster(t *testing.T) string {
	t.Helper()

	var b bytes.Buffer
	require.NoError(t, png.Encode(&b, stdimage.NewRGBA(stdimage.Rect(0, 0, 1, 1))))

	return base64.StdEncoding.EncodeToString(b.Bytes())
}
func TestImageInputOrder(t *testing.T) {
	s := &session{agent: NewAgent()}
	blocks := []acp.ContentBlock{acp.TextBlock("before"), acp.ImageBlock(promptRaster(t), "image/png"), acp.TextBlock("after")}
	mapped, err := s.mapPrompt(t.Context(), blocks)
	require.NoError(t, err)
	data, err := json.Marshal(mapped.content)
	require.NoError(t, err)
	var content []map[string]any
	require.NoError(t, json.Unmarshal(data, &content))
	kinds := make([]string, 0, len(content))
	for _, part := range content {
		kind, ok := part["type"].(string)
		require.True(t, ok)
		kinds = append(kinds, kind)
	}
	require.Equal(t, []string{"text", "image", "text"}, kinds)
}

func TestEmptyTextPromptIsRejected(t *testing.T) {
	s := &session{agent: NewAgent()}
	_, err := s.mapPrompt(t.Context(), []acp.ContentBlock{acp.TextBlock("")})
	require.Error(t, err, "empty text prompt was admitted for native dispatch")
}

// The pinned SDK cancels a session's previous prompt context before it
// dispatches the next prompt for that session, so the refusal of a peer prompt
// reaches the live turn as a cancelled request context. A turn driven by that
// context gives up at the abort rung; this one runs under a context the session
// owns and continues until its own native work finishes. The turn is parked in
// a client callback so the second prompt is dispatched while the first is
// unambiguously in flight.
func TestRefusedPeerPromptLeavesTheLiveTurnRunning(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.initialize(withLifecycle(), withFormElicitation())

	asked := make(chan struct{})
	release := make(chan struct{})
	releaseDialog := sync.OnceFunc(func() { close(release) })

	defer releaseDialog()

	h.rec.mu.Lock()
	h.rec.elicit = func(acp.UnstableCreateElicitationRequest) (acp.UnstableCreateElicitationResponse, error) {
		close(asked)
		<-release

		return acp.UnstableCreateElicitationResponse{Accept: &acp.UnstableCreateElicitationAccept{Action: elicitationAccept, Content: map[string]any{"color": "blue"}}}, nil
	}
	h.rec.mu.Unlock()

	session := h.newSession()

	type outcome struct {
		response acp.PromptResponse
		err      error
	}

	done := make(chan outcome, 1)

	go func() {
		response, err := h.prompt(session.SessionId, "ELICIT", promptMeta(1))
		done <- outcome{response: response, err: err}
	}()

	<-asked

	_, err := h.prompt(session.SessionId, "HELLO", promptMeta(2))
	require.Equal(t, "backpressure", requestErrorData(t, err)["error"])
	require.Equal(t, "session_prompt", requestErrorData(t, err)["limit"])

	select {
	case result := <-done:
		t.Fatalf("the refused peer prompt ended the live turn: %+v %v", result.response, result.err)
	case <-time.After(sessionAbortTimeout + time.Second):
	}

	releaseDialog()

	result := <-done
	require.NoError(t, result.err)
	require.Equal(t, acp.StopReasonEndTurn, result.response.StopReason)
	require.True(t, reduceAll(t, session.SessionId, h.rec.snapshot()).Settled())
}

func TestSessionCancelEndsTheTurnWithACancelledIdle(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	h.initialize(withLifecycle())
	session := h.newSession()

	done := make(chan acp.PromptResponse, 1)
	failed := make(chan error, 1)

	go func() {
		response, err := h.prompt(session.SessionId, "BLOCK", promptMeta(1))
		done <- response
		failed <- err
	}()

	h.rec.waitFor(t, func(updates []acp.SessionNotification) bool { return len(lifecycleEvents(updates)) >= 3 })
	require.NoError(t, h.conn.Cancel(h.ctx(), wire.CancelRequest(session.SessionId)))
	require.NoError(t, <-failed)
	require.Equal(t, acp.StopReasonCancelled, (<-done).StopReason)

	h.rec.waitFor(t, func(updates []acp.SessionNotification) bool {
		kinds := eventTypes(lifecycleEvents(updates))

		return len(kinds) > 0 && kinds[len(kinds)-1] == "state_update:idle"
	})

	events := lifecycleEvents(h.rec.snapshot())
	require.Equal(t, "cancelled", events[len(events)-1]["outcome"])
}

// abortSettleSlack is what the abort-timeout settlement may spend beyond the
// abort bound itself: process signalling, the terminal event, and scheduling.
const abortSettleSlack = 4 * time.Second

func TestAbortTimeoutEndsTheGenerationAndFences(t *testing.T) {
	h := newHarness(t, WithEnv(map[string]string{fakeClaudeEnv: "1", "ACP_GO_CLAUDE_TEST_IGNORE_INTERRUPT": "1"}))
	h.initialize(withLifecycle())
	session := h.newSession()

	done := make(chan error, 1)

	go func() {
		_, err := h.prompt(session.SessionId, "BLOCK", promptMeta(1))
		done <- err
	}()

	h.rec.waitFor(t, func(updates []acp.SessionNotification) bool { return len(lifecycleEvents(updates)) >= 3 })
	cancelledAt := time.Now()
	require.NoError(t, h.conn.Cancel(h.ctx(), wire.CancelRequest(session.SessionId)))
	require.NoError(t, <-done)

	// The rung signals the child and leaves the pump to reap it, so the prompt
	// settles on the abort bound. Joining the pump here would instead wait for
	// the turn this goroutine has not finished, and settle a shutdown rung late.
	require.Less(t, time.Since(cancelledAt), sessionAbortTimeout+abortSettleSlack,
		"the abort-timeout rung waited on the pump that is waiting on this turn")

	// The turn held no durable commit, so the incarnation is fenced instead of
	// asserting a terminal idle.
	require.Equal(t, []string{"lifecycle_snapshot", "prompt_accepted", "state_update:running"},
		eventTypes(lifecycleEvents(h.rec.snapshot())))

	// The generation ended with the fence, so the next prompt relaunches and
	// opens a new incarnation rather than publishing into the fenced one.
	_, err := h.prompt(session.SessionId, "HELLO", promptMeta(2))
	require.NoError(t, err)
	require.Equal(t, []string{
		"lifecycle_snapshot", "prompt_accepted", "state_update:running",
		"lifecycle_snapshot", "prompt_accepted", "state_update:running", "state_update:idle",
	}, eventTypes(lifecycleEvents(h.rec.snapshot())))
}

// A turn is the session's before the prompt has anything to dispatch, so a
// session/cancel that lands while claude is still being relaunched ends it
// there: the prompt answers cancelled, claude never receives the turn, and the
// lifecycle stream carries nothing for it.
func TestPromptCancelledWhileRelaunching(t *testing.T) {
	t.Parallel()

	held := filepath.Join(t.TempDir(), "relaunch-held")
	store := &mirrorFaultStore{SessionStore: acpcore.NewInMemorySessionStore()}
	h := newHarness(t, WithSessionStore(store),
		WithEnv(map[string]string{fakeClaudeEnv: "1", fakeClaudeEnvResumeHold: held}))
	h.initialize(withLifecycle())
	session := h.newSession()

	// A failed mirror commit ends the generation, so the next prompt relaunches
	// claude, and a relaunched fake announces nothing until it is released.
	store.fail.Store(true)

	_, err := h.prompt(session.SessionId, "HELLO", promptMeta(1))
	require.Equal(t, "claude_turn_failed", requestErrorData(t, err)["error"])
	store.fail.Store(false)

	before := eventTypes(lifecycleEvents(h.rec.snapshot()))

	type result struct {
		resp acp.PromptResponse
		err  error
	}

	done := make(chan result, 1)

	go func() {
		resp, promptErr := h.prompt(session.SessionId, "HELLO", promptMeta(2))
		done <- result{resp, promptErr}
	}()

	require.Eventually(t, func() bool {
		_, statErr := os.Stat(held)

		return statErr == nil
	}, testTimeout, time.Millisecond)

	require.NoError(t, h.conn.Cancel(h.ctx(), wire.CancelRequest(session.SessionId)))

	got := <-done
	require.NoError(t, got.err)
	require.Equal(t, acp.StopReasonCancelled, got.resp.StopReason)
	require.Equal(t, before, eventTypes(lifecycleEvents(h.rec.snapshot())),
		"a prompt claude never received opens no incarnation and publishes no acceptance")
}

// A $/cancel_request ends only the addressed handler's context: the turn it
// was driving stays the session's, completes successfully once, and the
// session keeps serving prompts.
func TestCancelRequestSettlesTheOriginalRequestOnce(t *testing.T) {
	t.Parallel()

	h := newHarness(t)
	permissionCtx, releasePermission := context.WithCancel(t.Context())
	defer releasePermission()
	entered := make(chan struct{}, 1)
	answer := h.rec.answer
	h.rec.answer = func(request acp.RequestPermissionRequest) acp.RequestPermissionResponse {
		entered <- struct{}{}
		<-permissionCtx.Done()

		return answer(request)
	}
	h.initialize(withLifecycle())
	session := h.newSession()

	request := wire.TextPromptRequest(session.SessionId, "PERMISSION")
	request.Meta = promptMeta(1)
	failed := make(chan error, 1)

	go func() {
		response, err := h.conn.Prompt(h.ctx(), request)
		if err == nil && response.StopReason != acp.StopReasonEndTurn {
			err = errors.New("request cancellation ended the native turn")
		}
		failed <- err
	}()

	select {
	case <-entered:
	case <-h.ctx().Done():
		t.Fatal("native permission request did not arrive")
	}
	require.NoError(t, h.input.cancelPrompt())

	_, busyErr := h.prompt(session.SessionId, "HELLO", promptMeta(2))
	require.Equal(t, "backpressure", requestErrorData(t, busyErr)["error"])
	releasePermission()
	h.rec.waitFor(t, func(updates []acp.SessionNotification) bool {
		return slices.Contains(eventTypes(lifecycleEvents(updates)), "state_update:idle")
	})

	idles := 0

	for _, update := range h.rec.snapshot() {
		envelope, _ := update.Meta[wire.LifecycleKey].(map[string]any)
		event, _ := envelope["event"].(map[string]any)
		if event["type"] == "state_update" && event["state"] == "idle" {
			idles++
			require.Equal(t, "success", event["outcome"])
			require.Equal(t, string(acp.StopReasonEndTurn), event["stopReason"])
		}
	}

	require.Equal(t, 1, idles, "the turn the cancelled request started settles exactly once")
	require.NoError(t, <-failed)

	resp, err := h.prompt(session.SessionId, "HELLO", promptMeta(3))
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, resp.StopReason)
}

// Handler cancellation can precede native dispatch; only session cancellation owns the turn.
func TestPromptOwnsCancellationBeforeDispatch(t *testing.T) {
	t.Parallel()
	a := NewAgent(testOptions(t)...)
	t.Cleanup(func() { _ = a.Close() })
	a.attach(newRecorder(), nil)
	_, err := a.Initialize(t.Context(), acp.InitializeRequest{ProtocolVersion: acp.ProtocolVersionNumber})
	require.NoError(t, err)
	created, err := a.NewSession(t.Context(), wire.NewSessionRequest(t.TempDir()))
	require.NoError(t, err)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	response, err := a.Prompt(ctx, wire.TextPromptRequest(created.SessionId, "HELLO"))
	require.NoError(t, err)
	require.Equal(t, acp.StopReasonEndTurn, response.StopReason)
}
