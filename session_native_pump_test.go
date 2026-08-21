package claudeacp

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/savid/acp-go-claude/internal/lifecycle"
	"github.com/stretchr/testify/require"
)

type gatedRawNotificationClient struct {
	*recordingAgentClient
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func TestNativeTurnSinkSeparatesPreDispatchAndAcceptedFrames(t *testing.T) {
	incarnation := &nativeIncarnation{lost: make(chan struct{})}
	sink := newNativeTurnSink("route", incarnation)
	before := &claude.AssistantMessage{Raw: map[string]any{"phase": "before"}}
	during := &claude.AssistantMessage{Raw: map[string]any{"phase": "during"}}
	beforeFrame := nativeOwnedFrame{message: before, route: "route"}
	duringFrame := nativeOwnedFrame{message: during, route: "route"}

	admission := sink.admit(beforeFrame)
	require.Nil(t, admission.pending)
	require.False(t, sink.beginDispatch())
	require.Equal(t, []nativeOwnedFrame{beforeFrame}, sink.takeBeforeDispatch())
	require.True(t, sink.beginDispatch())

	admission = sink.admit(duringFrame)
	require.Nil(t, admission.pending)
	require.Empty(t, sink.messages, "dispatch-buffered frame escaped before lifecycle acceptance")

	sink.accept()
	frame, err := sink.next(t.Context(), incarnation)
	require.NoError(t, err)
	require.Same(t, during, frame.message)
}

func TestNativeTurnSinkBackpressuresAndPromotesTheExactNextFrame(t *testing.T) {
	incarnation := &nativeIncarnation{lost: make(chan struct{})}
	sink := newNativeTurnSink("route", incarnation)
	require.True(t, sink.beginDispatch())

	for index := range nativePumpQueue {
		admission := sink.admit(nativeOwnedFrame{
			message: &claude.AssistantMessage{Raw: map[string]any{"index": index}},
			route:   "route",
		})
		require.Nil(t, admission.pending)
	}

	pendingMessage := &claude.AssistantMessage{Raw: map[string]any{"index": nativePumpQueue}}
	pending := sink.admit(nativeOwnedFrame{message: pendingMessage, route: "route"})
	require.NotNil(t, pending.pending)
	sink.accept()

	select {
	case <-pending.pending:
		t.Fatal("a full accepted queue released native backpressure before its head advanced")
	default:
	}

	first, err := sink.next(t.Context(), incarnation)
	require.NoError(t, err)
	require.Equal(t, 0, first.message.RawMessage()["index"])

	select {
	case <-pending.pending:
	case <-t.Context().Done():
		t.Fatal("the next admitted frame was not promoted after the queue advanced")
	}

	for index := 1; index < nativePumpQueue; index++ {
		frame, nextErr := sink.next(t.Context(), incarnation)
		require.NoError(t, nextErr)
		require.Equal(t, index, frame.message.RawMessage()["index"])
	}

	last, err := sink.next(t.Context(), incarnation)
	require.NoError(t, err)
	require.Same(t, pendingMessage, last.message)
}

func TestNativeFrameOwnershipRoutesEveryCausalContinuationAndRetiresTaskIDs(t *testing.T) {
	t.Parallel()

	const (
		routeA = "route-a"
		routeB = "route-b"
		taskID = "task-reused"
		toolA  = "tool-a"
		toolB  = "tool-b"
	)

	var ownership nativeFrameOwnership

	resolve := func(msg claude.Message, current string, wantRoute string, wantCausal bool) {
		t.Helper()

		route, causal, err := ownership.resolve(msg, current)
		require.NoError(t, err)
		require.Equal(t, wantRoute, route)
		require.Equal(t, wantCausal, causal)
	}

	resolve(&claude.SystemMessage{
		Subtype: nativeTaskStartedSubtype,
		Raw: map[string]any{
			"task_id": taskID, "tool_use_id": toolA,
		},
	}, routeA, routeA, true)

	assistant := &claude.AssistantMessage{
		ParentToolUseID: toolA,
		MessageID:       "assistant-a",
		Raw: map[string]any{
			"type": claude.MessageTypeAssistant, "uuid": "assistant-a",
		},
	}
	resolve(assistant, routeB, routeA, true)
	resolve(&claude.StreamEventMessage{
		ParentToolUseID: toolA,
		Raw: map[string]any{
			"type": claude.MessageTypeStream, "uuid": "stream-a",
		},
	}, routeB, routeA, true)
	resolve(&claude.UserMessage{
		ParentToolUseID: toolA,
		Raw: map[string]any{
			"type": claude.MessageTypeUser, "uuid": "user-a",
		},
	}, routeB, routeA, true)

	path := "/native/session/subagents/agent-a.jsonl"
	resolve(&claude.TranscriptMirrorMessage{
		FilePath: path,
		Entries: []json.RawMessage{
			json.RawMessage(`{"type":"assistant","uuid":"assistant-a"}`),
		},
		Raw: map[string]any{"type": claude.MessageTypeMirror},
	}, routeB, routeA, true)
	resolve(&claude.TranscriptMirrorMessage{
		FilePath: path,
		Raw:      map[string]any{"type": claude.MessageTypeMirror},
	}, routeB, routeA, true)

	cost := 1.25
	resolve(&claude.ResultMessage{
		Origin:       map[string]any{"kind": originKindTaskNotification, "task_id": taskID},
		TotalCostUSD: &cost,
		Usage:        &claude.Usage{InputTokens: 3, OutputTokens: 5},
		Raw:          map[string]any{"type": claude.MessageTypeResult},
	}, routeB, routeA, true)

	// The terminal result retires only the completed task binding. First sight of
	// the same task ID under B therefore establishes a new exact owner rather
	// than inheriting A.
	resolve(&claude.SystemMessage{
		Subtype: nativeTaskStartedSubtype,
		Raw: map[string]any{
			"task_id": taskID, "tool_use_id": toolB,
		},
	}, routeB, routeB, true)
	resolve(&claude.SystemMessage{
		Subtype: nativeTaskNotificationSubtype,
		Raw:     map[string]any{"task_id": taskID},
	}, routeA, routeB, true)
	resolve(&claude.ResultMessage{
		Origin: map[string]any{"kind": originKindTaskNotification, "task_id": taskID},
		Raw:    map[string]any{"type": claude.MessageTypeResult},
	}, routeA, routeB, true)
}

func TestNativeFrameOwnershipRefusesInsufficientCausalIdentity(t *testing.T) {
	t.Parallel()

	ownership := nativeFrameOwnership{}

	_, causal, err := ownership.resolve(&claude.AssistantMessage{
		ParentToolUseID: "unknown-tool",
		Raw:             map[string]any{"type": claude.MessageTypeAssistant},
	}, "current-b")
	require.True(t, causal)
	require.ErrorIs(t, err, errNativeFrameOwnership)

	_, causal, err = ownership.resolve(&claude.ResultMessage{
		Origin: map[string]any{"kind": originKindTaskNotification},
		Raw:    map[string]any{"type": claude.MessageTypeResult},
	}, "current-b")
	require.True(t, causal)
	require.ErrorIs(t, err, errNativeFrameOwnership, "an id-less result cannot borrow a FIFO task owner")

	_, causal, err = ownership.resolve(&claude.TranscriptMirrorMessage{
		FilePath: "/native/session/subagents/unknown.jsonl",
		Entries: []json.RawMessage{
			json.RawMessage(`{"type":"assistant","parent_tool_use_id":"unknown-tool"}`),
		},
		Raw: map[string]any{"type": claude.MessageTypeMirror},
	}, "current-b")
	require.True(t, causal)
	require.ErrorIs(t, err, errNativeFrameOwnership)
}

func TestNativeFrameOwnershipRefusesEveryConflictingIdentityPath(t *testing.T) {
	t.Run("malformed task start", func(t *testing.T) {
		var ownership nativeFrameOwnership
		_, causal, err := ownership.resolve(&claude.SystemMessage{
			Subtype: nativeTaskStartedSubtype,
			Raw:     map[string]any{"task_id": "task"},
		}, "route-a")
		require.True(t, causal)
		require.ErrorIs(t, err, errNativeFrameOwnership)
	})

	t.Run("task without owner", func(t *testing.T) {
		var ownership nativeFrameOwnership
		_, causal, err := ownership.resolve(&claude.SystemMessage{
			Subtype: nativeTaskStartedSubtype,
			Raw:     map[string]any{"task_id": "task", "tool_use_id": "tool"},
		}, "")
		require.True(t, causal)
		require.ErrorIs(t, err, errNativeFrameOwnership)
	})

	t.Run("reused task conflict and exact retransmission", func(t *testing.T) {
		var ownership nativeFrameOwnership
		start := &claude.SystemMessage{Subtype: nativeTaskStartedSubtype, Raw: map[string]any{
			"task_id": "task", "tool_use_id": "tool",
		}}
		_, _, err := ownership.resolve(start, "route-a")
		require.NoError(t, err)
		route, causal, err := ownership.resolve(start, "route-a")
		require.NoError(t, err)
		require.True(t, causal)
		require.Equal(t, "route-a", route)

		_, causal, err = ownership.resolve(&claude.SystemMessage{
			Subtype: nativeTaskStartedSubtype,
			Raw:     map[string]any{"task_id": "task", "tool_use_id": "other-tool"},
		}, "route-a")
		require.True(t, causal)
		require.ErrorIs(t, err, errNativeFrameOwnership)
	})

	t.Run("tool owner conflict", func(t *testing.T) {
		ownership := nativeFrameOwnership{tools: map[string]string{"tool": "route-b"}}
		_, causal, err := ownership.resolve(&claude.SystemMessage{
			Subtype: nativeTaskStartedSubtype,
			Raw:     map[string]any{"task_id": "task", "tool_use_id": "tool"},
		}, "route-a")
		require.True(t, causal)
		require.ErrorIs(t, err, errNativeFrameOwnership)
		require.ErrorIs(t, ownership.bindTool("", "route-a"), errNativeFrameOwnership)
	})

	t.Run("continuation parent conflict", func(t *testing.T) {
		ownership := nativeFrameOwnership{
			tasks: map[string]nativeTaskBinding{"task": {route: "route-a", toolUseID: "tool-a"}},
			tools: map[string]string{"parent-b": "route-b"},
		}
		_, causal, err := ownership.resolve(&claude.SystemMessage{
			Subtype: nativeTaskNotificationSubtype,
			Raw:     map[string]any{"task_id": "task", "parent_tool_use_id": "parent-b"},
		}, "route-b")
		require.True(t, causal)
		require.ErrorIs(t, err, errNativeFrameOwnership)
	})

	t.Run("message owner conflicts", func(t *testing.T) {
		ownership := nativeFrameOwnership{
			tools:    map[string]string{"parent-a": "route-a"},
			messages: map[string]string{"message": "route-b"},
		}
		_, causal, err := ownership.resolve(&claude.AssistantMessage{
			ParentToolUseID: "parent-a",
			Raw:             map[string]any{"uuid": "message"},
		}, "route-b")
		require.True(t, causal)
		require.ErrorIs(t, err, errNativeFrameOwnership)

		ownership = nativeFrameOwnership{messages: map[string]string{"message": "route-b"}}
		_, causal, err = ownership.resolve(&claude.AssistantMessage{
			Raw: map[string]any{"uuid": "message"},
		}, "route-a")
		require.False(t, causal)
		require.ErrorIs(t, err, errNativeFrameOwnership)
	})

	t.Run("assistant tool conflict", func(t *testing.T) {
		ownership := nativeFrameOwnership{tools: map[string]string{"tool": "route-b"}}
		_, _, err := ownership.resolve(&claude.AssistantMessage{
			Content: []claude.ContentBlock{claude.ToolUseBlock{ID: "tool"}},
			Raw:     map[string]any{},
		}, "route-a")
		require.ErrorIs(t, err, errNativeFrameOwnership)
	})

	t.Run("task result identity conflict", func(t *testing.T) {
		ownership := nativeFrameOwnership{
			tasks:    map[string]nativeTaskBinding{"task": {route: "route-a", toolUseID: "tool"}},
			messages: map[string]string{"result": "route-b"},
		}
		_, causal, err := ownership.resolve(&claude.ResultMessage{
			Origin: map[string]any{"kind": originKindTaskNotification, "task_id": "task"},
			Raw:    map[string]any{"uuid": "result"},
		}, "route-a")
		require.True(t, causal)
		require.ErrorIs(t, err, errNativeFrameOwnership)
	})

	t.Run("mirror owner conflicts", func(t *testing.T) {
		ownership := nativeFrameOwnership{
			tasks: map[string]nativeTaskBinding{
				"task-a": {route: "route-a", toolUseID: "tool-a"},
				"task-b": {route: "route-b", toolUseID: "tool-b"},
			},
		}
		_, causal, err := ownership.resolve(&claude.TranscriptMirrorMessage{
			Entries: []json.RawMessage{
				json.RawMessage(`{"task_id":"task-a"}`),
				json.RawMessage(`{"task_id":"task-b"}`),
			},
			Raw: map[string]any{},
		}, "route-a")
		require.True(t, causal)
		require.ErrorIs(t, err, errNativeFrameOwnership)

		ownership = nativeFrameOwnership{
			tasks:   map[string]nativeTaskBinding{"task-a": {route: "route-a", toolUseID: "tool-a"}},
			mirrors: map[string]string{"/mirror": "route-b"},
		}
		_, causal, err = ownership.resolve(&claude.TranscriptMirrorMessage{
			FilePath: "/mirror",
			Entries:  []json.RawMessage{json.RawMessage(`{"task_id":"task-a"}`)},
			Raw:      map[string]any{},
		}, "route-a")
		require.True(t, causal)
		require.ErrorIs(t, err, errNativeFrameOwnership)

		ownership = nativeFrameOwnership{}
		_, causal, err = ownership.resolve(&claude.TranscriptMirrorMessage{
			Entries: []json.RawMessage{json.RawMessage(`{"task_id":"unknown"}`)},
			Raw:     map[string]any{},
		}, "route-a")
		require.True(t, causal)
		require.ErrorIs(t, err, errNativeFrameOwnership)
	})

	t.Run("mirror capture retains one parent route", func(t *testing.T) {
		ownership := nativeFrameOwnership{
			tools:    map[string]string{"parent": "route-a"},
			messages: map[string]string{"mirror": "route-b"},
		}
		_, causal, err := ownership.resolve(&claude.TranscriptMirrorMessage{
			Entries: []json.RawMessage{json.RawMessage(`{"parent_tool_use_id":"parent"}`)},
			Raw:     map[string]any{"uuid": "mirror"},
		}, "route-a")
		require.True(t, causal)
		require.ErrorIs(t, err, errNativeFrameOwnership)
	})

	var ownership nativeFrameOwnership
	_, causal, err := ownership.resolve(nil, "route-a")
	require.False(t, causal)
	require.NoError(t, err)
	require.NoError(t, validateNativeIdentitySchema(nil))
	require.Empty(t, nativeParentToolUseID(nil))

	_, causal, err = ownership.resolve(&claude.TranscriptMirrorMessage{
		Entries: []json.RawMessage{json.RawMessage(`{`)}, Raw: map[string]any{},
	}, "route-a")
	require.True(t, causal)
	require.ErrorIs(t, err, errNativeFrameOwnership)
}

func TestNativeFrameOwnershipRefusesCamelCaseCausalFields(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name string
		msg  claude.Message
	}{
		{name: "task ID", msg: &claude.SystemMessage{
			Subtype: nativeTaskStartedSubtype,
			Raw: map[string]any{
				"task_id": "task-a", "taskId": "task-a", "tool_use_id": "tool-a",
			},
		}},
		{name: "tool use ID", msg: &claude.SystemMessage{
			Subtype: nativeTaskStartedSubtype,
			Raw: map[string]any{
				"task_id": "task-a", "tool_use_id": "tool-a", "toolUseId": "tool-a",
			},
		}},
		{name: "parent tool use ID", msg: &claude.AssistantMessage{
			Raw: map[string]any{
				"type": claude.MessageTypeAssistant, "parentToolUseId": "tool-a",
			},
		}},
		{name: "result origin task ID", msg: &claude.ResultMessage{
			Origin: map[string]any{
				"kind": originKindTaskNotification, "taskId": "task-a",
			},
			Raw: map[string]any{"type": claude.MessageTypeResult},
		}},
		{name: "result origin tool use ID", msg: &claude.ResultMessage{
			Origin: map[string]any{
				"kind": originKindTaskNotification, "toolUseId": "tool-a",
			},
			Raw: map[string]any{"type": claude.MessageTypeResult},
		}},
		{name: "result origin parent tool use ID", msg: &claude.ResultMessage{
			Origin: map[string]any{
				"kind": originKindTaskNotification, "parentToolUseId": "tool-a",
			},
			Raw: map[string]any{"type": claude.MessageTypeResult},
		}},
		{name: "nested result origin alias", msg: &claude.ResultMessage{
			Origin: map[string]any{
				"kind":   originKindTaskNotification,
				"nested": map[string]any{"toolUseId": "tool-a"},
			},
			Raw: map[string]any{"type": claude.MessageTypeResult},
		}},
		{name: "mixed result origin schema", msg: &claude.ResultMessage{
			Origin: map[string]any{
				"kind": originKindTaskNotification, "task_id": "task-a", "taskId": "task-a",
			},
			Raw: map[string]any{"type": claude.MessageTypeResult},
		}},
		{name: "mirror task ID", msg: &claude.TranscriptMirrorMessage{
			FilePath: "/native/session/subagents/camel-task.jsonl",
			Entries: []json.RawMessage{
				json.RawMessage(`{"type":"assistant","taskId":"task-a"}`),
			},
			Raw: map[string]any{"type": claude.MessageTypeMirror},
		}},
		{name: "mirror parent tool use ID", msg: &claude.TranscriptMirrorMessage{
			FilePath: "/native/session/subagents/camel-parent.jsonl",
			Entries: []json.RawMessage{
				json.RawMessage(`{"type":"assistant","parentToolUseId":"tool-a"}`),
			},
			Raw: map[string]any{"type": claude.MessageTypeMirror},
		}},
		{name: "mirror tool use ID", msg: &claude.TranscriptMirrorMessage{
			FilePath: "/native/session/subagents/camel-tool.jsonl",
			Entries: []json.RawMessage{
				json.RawMessage(`{"type":"assistant","toolUseId":"tool-a"}`),
			},
			Raw: map[string]any{"type": claude.MessageTypeMirror},
		}},
		{name: "nested mirror alias", msg: &claude.TranscriptMirrorMessage{
			FilePath: "/native/session/subagents/camel-nested.jsonl",
			Entries: []json.RawMessage{
				json.RawMessage(`{"type":"assistant","message":{"content":[{"parentToolUseId":"tool-a"}]}}`),
			},
			Raw: map[string]any{"type": claude.MessageTypeMirror},
		}},
		{name: "mixed mirror schema", msg: &claude.TranscriptMirrorMessage{
			FilePath: "/native/session/subagents/camel-mixed.jsonl",
			Entries: []json.RawMessage{
				json.RawMessage(`{"type":"assistant","task_id":"task-a","taskId":"task-a"}`),
			},
			Raw: map[string]any{"type": claude.MessageTypeMirror},
		}},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			t.Parallel()

			var ownership nativeFrameOwnership
			_, causal, err := ownership.resolve(test.msg, "route-b")
			require.True(t, causal)
			require.ErrorIs(t, err, errNativeFrameOwnership)
		})
	}
}

func (c *gatedRawNotificationClient) NotifyExtension(ctx context.Context, method string, params any) error {
	c.once.Do(func() { close(c.entered) })
	select {
	case <-c.release:
	case <-ctx.Done():
		return ctx.Err()
	}

	return c.recordingAgentClient.NotifyExtension(ctx, method, params)
}

func TestPromptClaimsFrameBeforeProjectionWhenSinkWithdraws(t *testing.T) {
	session, recorded, stream := newLifecycleStreamTestSession(t)
	session.id = "11111111-1111-4111-8111-111111111111"
	session.rawMessages = rawMessageConfig{All: true}
	gated := &gatedRawNotificationClient{
		recordingAgentClient: recorded,
		entered:              make(chan struct{}),
		release:              make(chan struct{}),
	}
	session.agent.setConnection(gated)
	require.NoError(t, stream.incarnate(t.Context()))

	pump := session.nativePumpHandle()
	defer session.stopNativePump()
	incarnation := &nativeIncarnation{lost: make(chan struct{})}
	session.setAutonomousRoute("autonomous-route", incarnation)
	sink, releaseSink := pump.attachTurn("prompt-route", incarnation)
	require.True(t, sink.beginDispatch())
	sink.accept()

	message := &claude.AssistantMessage{Raw: map[string]any{"type": "assistant"}}
	frame, err := pump.captureOwnedFrame(incarnation, message)
	require.NoError(t, err)
	require.Equal(t, "prompt-route", frame.route)
	require.True(t, frame.foregroundOwned)
	delivered := make(chan struct{})
	go func() {
		pump.deliver(t.Context(), incarnation, frame)
		close(delivered)
	}()

	next := make(chan error, 1)
	go func() {
		_, err := pump.next(withTurnRoute(t.Context(), "prompt-route"), sink, incarnation)
		next <- err
	}()

	<-gated.entered
	<-delivered
	releaseSink()
	close(gated.release)
	require.NoError(t, <-next)
	require.Len(t, recorded.Extensions(), 1)
}

func TestNativePumpFullSinkDepartureKeepsCapturedPromptOwner(t *testing.T) {
	const (
		routeA   = "full-sink-owner-a"
		routeB   = "replacement-owner-b"
		sentinel = "FULL-SINK-OWNER-A"
	)

	tests := []struct {
		name    string
		message func(home string, sessionID string) claude.Message
		assert  func(t *testing.T, conn *recordingAgentClient, store *InMemorySessionStore, sessionID string)
	}{
		{
			name: "typed",
			message: func(_ string, _ string) claude.Message {
				return &claude.AssistantMessage{
					Content: []claude.ContentBlock{claude.TextBlock{Text: sentinel}},
					Raw: map[string]any{
						"type": claude.MessageTypeAssistant, "sentinel": sentinel,
					},
				}
			},
			assert: func(t *testing.T, conn *recordingAgentClient, _ *InMemorySessionStore, _ string) {
				t.Helper()

				_, found := sentinelNotificationIndex(t, conn, sentinel)
				require.True(t, found)
			},
		},
		{
			name: "usage and cost",
			message: func(_ string, _ string) claude.Message {
				cost := 1.25

				return &claude.ResultMessage{
					TotalCostUSD: &cost,
					Usage:        &claude.Usage{InputTokens: 3, OutputTokens: 5},
					Raw:          map[string]any{"type": claude.MessageTypeResult},
				}
			},
			assert: func(t *testing.T, conn *recordingAgentClient, _ *InMemorySessionStore, _ string) {
				t.Helper()

				var usage *acp.SessionUsageUpdate
				for _, notification := range conn.Updates() {
					if notification.Update.UsageUpdate != nil {
						usage = notification.Update.UsageUpdate
					}
				}

				require.NotNil(t, usage)
				require.Equal(t, 8, usage.Used)
				require.Equal(t, &acp.Cost{Amount: 1.25, Currency: costCurrencyUSD}, usage.Cost)
			},
		},
		{
			name: "mirror journal",
			message: func(home string, sessionID string) claude.Message {
				return &claude.TranscriptMirrorMessage{
					FilePath: filepath.Join(home, "projects", "project", sessionID+".jsonl"),
					Entries: []json.RawMessage{
						json.RawMessage(`{"type":"user","message":"` + sentinel + `"}`),
					},
					Raw: map[string]any{"type": claude.MessageTypeMirror},
				}
			},
			assert: func(t *testing.T, _ *recordingAgentClient, store *InMemorySessionStore, sessionID string) {
				t.Helper()

				entries, err := store.Load(t.Context(), SessionKey{SessionID: sessionID})
				require.NoError(t, err)
				require.Len(t, entries, 1)
				require.Contains(t, string(entries[0]), sentinel)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			session, recorded, stream := newLifecycleStreamTestSession(t)
			session.id = "11111111-1111-4111-8111-111111111111"
			session.rawMessages = rawMessageConfig{All: true}
			store := NewInMemorySessionStore()
			home := t.TempDir()
			session.mirror = newSessionMirror(session.agent.log, store, home, session)
			gated := &gatedRawNotificationClient{
				recordingAgentClient: recorded,
				entered:              make(chan struct{}),
				release:              make(chan struct{}),
			}
			session.agent.setConnection(gated)
			require.NoError(t, stream.incarnate(t.Context()))
			turnA, err := stream.dispatch(t.Context(), lifecycle.Submission{
				SubmissionID: "full-sink-a", ClientNonce: "full-sink-client-a",
			}, routeA, func() error { return nil })
			require.NoError(t, err)

			pump := session.nativePumpHandle()
			defer session.stopNativePump()
			incarnation := &nativeIncarnation{lost: make(chan struct{})}
			session.setAutonomousRoute("autonomous-route", incarnation)
			releaseForeground := session.takeForeground()
			sinkA, releaseA := pump.attachTurn(routeA, incarnation)
			require.True(t, sinkA.beginDispatch())
			sinkA.accept()

			message := test.message(home, string(session.id))
			frame, err := pump.captureOwnedFrame(incarnation, message)
			require.NoError(t, err)
			require.Equal(t, routeA, frame.route)
			require.True(t, frame.foregroundOwned)
			require.False(t, frame.causal, "the regression frame has no native causal identity")

			for range nativePumpQueue {
				sinkA.messages = append(sinkA.messages, nativeOwnedFrame{
					message: &claude.AssistantMessage{}, route: routeA, foregroundOwned: true,
				})
			}

			delivered := make(chan struct{})
			go func() {
				pump.deliver(t.Context(), incarnation, frame)
				close(delivered)
			}()

			require.NoError(t, stream.settleTurn(t.Context(), turnA, lifecycleOutcome{
				stopReason: lifecycle.StopReasonEndTurn, outcome: lifecycle.OutcomeSuccess,
			}))
			releasedA := make(chan struct{})
			go func() {
				releaseA()
				close(releasedA)
			}()

			select {
			case <-gated.entered:
			case <-t.Context().Done():
				t.Fatal("sink retirement did not start projecting its admitted prefix")
			}

			turnB, err := stream.dispatch(t.Context(), lifecycle.Submission{
				SubmissionID: "full-sink-b", ClientNonce: "full-sink-client-b",
			}, routeB, func() error { return nil })
			require.NoError(t, err)
			sinkB, releaseB := pump.attachTurn(routeB, incarnation)
			defer releaseB()
			require.True(t, sinkB.beginDispatch())
			sinkB.accept()
			beforeProjection := len(recorded.Updates())
			releaseForeground()

			close(gated.release)
			<-releasedA
			<-delivered
			require.NoError(t, pump.barrier(t.Context()))
			require.Empty(t, sinkB.messages)

			rawEvents := decodeRawEvents(t, recorded)
			require.Len(t, rawEvents, nativePumpQueue+1)
			for _, rawEvent := range rawEvents {
				rawRoute := requireAnyMap(t, rawEvent.Meta[routeMetaKey])
				require.Equal(t, routeA, rawRoute[routeFieldTurn])
			}

			for _, notification := range recorded.Updates()[beforeProjection:] {
				route := requireAnyMap(t, notification.Meta[routeMetaKey])
				require.Equal(t, routeA, route[routeFieldTurn])
				require.NotEqual(t, routeB, route[routeFieldTurn], "B receives no typed update or settlement")
				_, lifecycleUpdate := notification.Meta[lifecycle.MetaKey]
				require.False(t, lifecycleUpdate, "A's captured frame cannot settle running B")
			}

			test.assert(t, recorded, store, string(session.id))
			require.NoError(t, stream.settleTurn(t.Context(), turnB, lifecycleOutcome{
				stopReason: lifecycle.StopReasonEndTurn, outcome: lifecycle.OutcomeSuccess,
			}))
		})
	}
}

func TestNativeTurnSinkRetirementDrainsAdmittedIngressBeforeReplacement(t *testing.T) {
	const (
		routeA = "retiring-owner-a"
		routeB = "replacement-owner-b"
		taskID = "retiring-task-a"
		toolID = "retiring-tool-a"
	)

	session, recorded, _ := newLifecycleStreamTestSession(t)
	session.id = "11111111-1111-4111-8111-111111111111"
	session.rawMessages = rawMessageConfig{All: true}
	store := NewInMemorySessionStore()
	home := t.TempDir()
	session.mirror = newSessionMirror(session.agent.log, store, home, session)
	pump := session.nativePumpHandle()
	defer session.stopNativePump()
	incarnation := &nativeIncarnation{lost: make(chan struct{})}
	session.setAutonomousRoute("autonomous-route", incarnation)
	sinkA, releaseA := pump.attachTurn(routeA, incarnation)
	require.True(t, sinkA.beginDispatch())

	deliver := func(msg claude.Message) {
		t.Helper()

		frame, err := pump.captureOwnedFrame(incarnation, msg)
		require.NoError(t, err)
		require.Equal(t, routeA, frame.route)
		pump.deliver(t.Context(), incarnation, frame)
	}
	assistant := func(marker string, text string) claude.Message {
		return &claude.AssistantMessage{
			Content: []claude.ContentBlock{claude.TextBlock{Text: text}},
			Raw: map[string]any{
				"type": claude.MessageTypeAssistant, "marker": marker,
			},
		}
	}

	deliver(assistant("dispatch-buffered", "DISPATCH-BUFFERED-A"))
	require.Empty(t, sinkA.messages)
	sinkA.accept()
	deliver(&claude.ResultMessage{Raw: map[string]any{
		"type": claude.MessageTypeResult, "marker": "prompt-terminal",
	}})

	for range 2 {
		_, err := pump.next(withTurnRoute(t.Context(), routeA), sinkA, incarnation)
		require.NoError(t, err)
	}

	deliver(assistant("post-terminal", "POST-TERMINAL-A"))
	deliver(&claude.SystemMessage{
		Subtype: nativeTaskStartedSubtype,
		Raw: map[string]any{
			"type": claude.MessageTypeSystem, "subtype": nativeTaskStartedSubtype,
			"task_id": taskID, "tool_use_id": toolID, "marker": "causal-task",
		},
	})
	deliver(&claude.TranscriptMirrorMessage{
		FilePath: filepath.Join(home, "projects", "project", string(session.id)+".jsonl"),
		Entries: []json.RawMessage{
			json.RawMessage(`{"type":"assistant","task_id":"` + taskID + `","marker":"mirror-a"}`),
		},
		Raw: map[string]any{"type": claude.MessageTypeMirror, "marker": "causal-mirror"},
	})
	cost := 2.5
	deliver(&claude.ResultMessage{
		Origin:       map[string]any{"kind": originKindTaskNotification, "task_id": taskID},
		TotalCostUSD: &cost,
		Usage:        &claude.Usage{InputTokens: 7, OutputTokens: 11},
		Raw: map[string]any{
			"type": claude.MessageTypeResult, "marker": "causal-result",
		},
	})

	gated := &gatedRawNotificationClient{
		recordingAgentClient: recorded,
		entered:              make(chan struct{}),
		release:              make(chan struct{}),
	}
	session.agent.setConnection(gated)
	releasedA := make(chan struct{})
	go func() {
		releaseA()
		close(releasedA)
	}()

	select {
	case <-gated.entered:
	case <-t.Context().Done():
		t.Fatal("retirement did not reach the post-terminal frame")
	}

	sinkB, releaseB := pump.attachTurn(routeB, incarnation)
	defer releaseB()
	require.True(t, sinkB.beginDispatch())
	sinkB.accept()
	close(gated.release)
	<-releasedA
	require.NoError(t, pump.barrier(t.Context()))
	require.Empty(t, sinkB.messages)

	events := decodeRawEvents(t, recorded)
	require.Len(t, events, 6)
	markers := make([]string, 0, len(events))
	for _, event := range events {
		route := requireAnyMap(t, event.Meta[routeMetaKey])
		require.Equal(t, routeA, route[routeFieldTurn])
		markers = append(markers, nativeStringField(event.Event, "marker"))
	}
	require.Equal(t, []string{
		"dispatch-buffered", "prompt-terminal", "post-terminal",
		"causal-task", "causal-mirror", "causal-result",
	}, markers)

	_, tailFound := sentinelNotificationIndex(t, recorded, "POST-TERMINAL-A")
	require.True(t, tailFound)

	var usage *acp.SessionUsageUpdate
	for _, notification := range recorded.Updates() {
		if notification.Update.UsageUpdate != nil {
			usage = notification.Update.UsageUpdate
		}
	}
	require.NotNil(t, usage)
	require.Equal(t, 18, usage.Used)
	require.Equal(t, &acp.Cost{Amount: cost, Currency: costCurrencyUSD}, usage.Cost)

	entries, err := store.Load(t.Context(), SessionKey{SessionID: string(session.id)})
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Contains(t, string(entries[0]), "mirror-a")
}

func TestNativeSinkRetirementProjectsNonForegroundFramesUnderTheirCapturedRoute(t *testing.T) {
	t.Run("conversation", func(t *testing.T) {
		session, _, stream := newLifecycleStreamTestSession(t)
		require.NoError(t, stream.incarnate(t.Context()))
		pump := session.nativePumpHandle()
		defer session.stopNativePump()
		incarnation := &nativeIncarnation{}
		session.setAutonomousRoute("autonomous", incarnation)
		sink, release := pump.attachTurn("prompt", incarnation)
		sink.admit(nativeOwnedFrame{
			route:   "autonomous",
			message: &claude.AssistantMessage{Content: []claude.ContentBlock{claude.TextBlock{Text: "late"}}},
		})
		release()
		require.NotNil(t, session.excursion)
	})

	t.Run("projection failure", func(t *testing.T) {
		session := &agentSession{agent: NewAgent(), rawMessages: rawMessageConfig{All: true}}
		pump := session.nativePumpHandle()
		defer session.stopNativePump()
		incarnation := &nativeIncarnation{lost: make(chan struct{}), mirrorReady: make(chan struct{})}
		incarnation.superviseOnce.Do(func() {})
		session.setAutonomousRoute("autonomous", incarnation)
		sink, release := pump.attachTurn("prompt", incarnation)
		sink.admit(nativeOwnedFrame{
			route:   "autonomous",
			message: &claude.AssistantMessage{Raw: map[string]any{"type": claude.MessageTypeAssistant}},
		})
		release()
		require.True(t, incarnation.failed.Load())
	})
}

func TestOptionalRawFailureDoesNotHoleTypedMirrorOrTerminalDelivery(t *testing.T) {
	session, conn, stream := newLifecycleStreamTestSession(t)
	session.id = "11111111-1111-4111-8111-111111111111"
	session.rawMessages = rawMessageConfig{All: true}
	store := NewInMemorySessionStore()
	home := t.TempDir()
	session.mirror = newSessionMirror(session.agent.log, store, home, session)
	pump := session.nativePumpHandle()
	defer session.stopNativePump()

	require.NoError(t, stream.incarnate(t.Context()))
	turnID, err := stream.dispatch(t.Context(), lifecycle.Submission{
		SubmissionID: "raw-submission", ClientNonce: "raw-client",
	}, "raw-route", func() error { return nil })
	require.NoError(t, err)

	conn.extensionErr = errors.New("SECRET_SENTINEL_raw_notify")
	conversation, err := pump.projectOwnedFrame(withTurnRoute(t.Context(), "raw-route"), nil,
		&claude.AssistantMessage{Raw: map[string]any{"type": "assistant"}})
	require.NoError(t, err)
	require.True(t, conversation)
	require.NoError(t, session.emitUpdates(withTurnRoute(t.Context(), "raw-route"),
		[]acp.SessionUpdate{acp.UpdateAgentMessageText("typed-after-raw-failure")}))

	mirror := &claude.TranscriptMirrorMessage{
		FilePath: filepath.Join(home, "projects", "project", string(session.id)+".jsonl"),
		Entries:  []json.RawMessage{json.RawMessage(`{"type":"user","message":"mirrored"}`)},
		Raw:      map[string]any{"type": claude.MessageTypeMirror},
	}
	conversation, err = pump.projectOwnedFrame(withTurnRoute(t.Context(), "raw-route"), nil, mirror)
	require.NoError(t, err)
	require.False(t, conversation)
	require.NoError(t, pump.barrier(t.Context()))

	conn.extensionErr = nil
	require.NoError(t, session.emitRawClaudeMessage(withTurnRoute(t.Context(), "raw-route"),
		&claude.SystemMessage{Raw: map[string]any{"type": "system"}}))
	require.Len(t, conn.Extensions(), 1)
	var raw map[string]any
	require.NoError(t, json.Unmarshal(conn.Extensions()[0].params, &raw))
	require.Equal(t, float64(1), raw[rawEventFieldSequence], "failed optional raw delivery consumes no sequence")

	require.NoError(t, stream.settleTurn(t.Context(), turnID,
		lifecycleOutcome{stopReason: lifecycle.StopReasonEndTurn, outcome: lifecycle.OutcomeSuccess}))
	entries, err := store.Load(t.Context(), SessionKey{SessionID: string(session.id)})
	require.NoError(t, err)
	require.Len(t, entries, 1)
	require.Contains(t, string(entries[0]), "mirrored")

	_, typed := sentinelNotificationIndex(t, conn, "typed-after-raw-failure")
	terminal := false
	for _, notification := range conn.Updates() {
		if envelope, ok := notification.Meta[lifecycle.MetaKey].(map[string]any); ok {
			event := requireAnyMap(t, envelope["event"])
			terminal = terminal || (event["type"] == string(lifecycle.EventStateUpdate) &&
				event["state"] == string(lifecycle.ForegroundIdle))
		}
	}
	require.True(t, typed)
	require.True(t, terminal)
}

func TestNativePumpTerminalBranches(t *testing.T) {
	session := &agentSession{agent: NewAgent()}
	session.stopNativePump() // never started
	pump := newNativePump(session)
	pump.quitOnce.Do(func() { close(pump.quit) })
	<-pump.workDone
	require.NoError(t, pump.barrier(t.Context()))

	require.ErrorIs(t, pump.incarnationError(), claude.ErrMessageStreamClosed)
	require.True(t, pump.incarnationEnded())

	pump.lost = make(chan struct{})
	require.False(t, pump.incarnationEnded())
	close(pump.lost)
	require.True(t, pump.incarnationEnded())

	want := errors.New("native lost")
	pump.err = want
	require.ErrorIs(t, pump.incarnationError(), want)

	msg, err := pump.next(t.Context(), newNativeTurnSink("route", nil), nil)
	require.ErrorIs(t, err, claude.ErrMessageStreamClosed)
	require.Nil(t, msg)
	incarnation := &nativeIncarnation{lost: make(chan struct{})}
	require.ErrorIs(t, incarnation.failure(), claude.ErrMessageStreamClosed)
	pump.failIncarnation(t.Context(), nil, want, "guard")
	pump.failIncarnation(t.Context(), incarnation, nil, "guard")
	pump.recordIncarnationEnd(nil, want)
	pump.recordIncarnationEnd(incarnation, nil)
}

func TestNativePumpDeliveryOwnershipEdges(t *testing.T) {
	session := &agentSession{agent: NewAgent()}
	pump := newNativePump(session)
	defer session.stopNativePump()
	require.Empty(t, newNativeTurnSink("route", nil).causalRoute())
	var nilIncarnation *nativeIncarnation
	nilIncarnation.signalMirrorReady()
	retired, err := session.retireExactNativeIncarnationLocked(t.Context(), nil)
	require.False(t, retired)
	require.NoError(t, err)
	require.False(t, pump.stopReceivingExact(&nativeIncarnation{}))
	require.ErrorIs(t, pump.terminalStoreError(), errNativeOutboxExited)
	terminalPump := &nativePump{commitErr: errors.New("stored")}
	require.ErrorContains(t, terminalPump.terminalStoreError(), "stored")

	pump.dispatch(t.Context(), nil, &claude.AssistantMessage{})
	incarnation := &nativeIncarnation{}
	incarnation.failed.Store(true)
	pump.dispatch(t.Context(), incarnation, &claude.AssistantMessage{})
	pump.deliver(t.Context(), incarnation, nativeOwnedFrame{
		message: &claude.AssistantMessage{}, route: "route",
	})
	incarnation.failed.Store(false)
	failingIncarnation := &nativeIncarnation{
		lost: make(chan struct{}), done: make(chan struct{}), mirrorReady: make(chan struct{}),
	}
	failingIncarnation.superviseOnce.Do(func() {})
	pump.dispatch(t.Context(), failingIncarnation, &claude.AssistantMessage{Raw: map[string]any{"toolUseId": "alias"}})
	require.True(t, failingIncarnation.failed.Load())
	_, err = pump.captureOwnedFrame(incarnation, &claude.AssistantMessage{Raw: map[string]any{"toolUseId": "alias"}})
	require.ErrorIs(t, err, errNativeFrameOwnership)
	_, err = pump.captureOwnedFrame(incarnation, &claude.AssistantMessage{})
	require.ErrorIs(t, err, errNativeFrameOwnership)
	pump.deliver(t.Context(), nil, nativeOwnedFrame{message: &claude.AssistantMessage{}})

	incarnation.expectedStop.Store(true)
	pump.sink = newNativeTurnSink("route", &nativeIncarnation{})
	pump.deliver(t.Context(), incarnation, nativeOwnedFrame{
		message: &claude.AssistantMessage{}, route: "route",
	})

	pump.sink = nil
	pump.deliver(t.Context(), incarnation, nativeOwnedFrame{
		message: &claude.AssistantMessage{}, route: "route",
	})

	session.setAutonomousRoute("route", incarnation)
	release := session.takeForeground()
	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	pump.deliver(cancelled, incarnation, nativeOwnedFrame{
		message: &claude.AssistantMessage{}, route: "route",
	})
	release()

	pump.recordCommitError(errors.New("mirror enqueue failed"))
	conversation, err := pump.projectOwnedFrame(t.Context(), incarnation, &claude.TranscriptMirrorMessage{})
	require.ErrorContains(t, err, "mirror enqueue failed")
	require.False(t, conversation)
}

func TestNativePumpPendingDeliveryHonorsItsWithdrawnReader(t *testing.T) {
	session := &agentSession{agent: NewAgent()}
	pump := newNativePump(session)
	defer session.stopNativePump()
	incarnation := &nativeIncarnation{}
	sink := newNativeTurnSink("route", incarnation)
	require.True(t, sink.beginDispatch())
	sink.accept()
	for index := 0; index < nativePumpQueue; index++ {
		sink.admit(nativeOwnedFrame{route: "route", message: &claude.AssistantMessage{}})
	}
	pump.sink = sink
	canceled, cancel := context.WithCancel(t.Context())
	cancel()
	pump.deliver(canceled, incarnation, nativeOwnedFrame{route: "route", message: &claude.AssistantMessage{}})
	require.Len(t, sink.close(), nativePumpQueue+1)
}

func TestNativeProjectionReturnsOptionalRawTransportFailure(t *testing.T) {
	agent := NewAgent()
	session := &agentSession{agent: agent, rawMessages: rawMessageConfig{All: true}}
	pump := newNativePump(session)
	defer session.stopNativePump()

	conversation, err := pump.projectOwnedFrame(t.Context(), &nativeIncarnation{}, &claude.AssistantMessage{
		Raw: map[string]any{"type": claude.MessageTypeAssistant},
	})
	require.False(t, conversation)
	require.ErrorIs(t, err, errACPConnectionNotAttached)
	incarnation := &nativeIncarnation{lost: make(chan struct{}), mirrorReady: make(chan struct{})}
	incarnation.superviseOnce.Do(func() {})
	session.setAutonomousRoute("route", incarnation)
	pump.deliver(t.Context(), incarnation, nativeOwnedFrame{
		route: "route", message: &claude.AssistantMessage{Raw: map[string]any{"type": claude.MessageTypeAssistant}},
	})
	require.True(t, incarnation.failed.Load())
}

func TestNativeOutboxContainsItsOwnUnexpectedPanic(t *testing.T) {
	incarnation := &nativeIncarnation{lost: make(chan struct{}), mirrorReady: make(chan struct{})}
	incarnation.superviseOnce.Do(func() {})
	pump := &nativePump{
		session:     &agentSession{agent: NewAgent()},
		work:        make(chan nativePumpWork, 1),
		workDone:    make(chan struct{}),
		incarnation: incarnation,
	}
	barrier := make(chan error)
	close(barrier)
	pump.work <- nativePumpWork{barrier: barrier}
	pump.outbox(t.Context())
	require.ErrorIs(t, pump.storeError(), errNativeOutboxExited)
	require.True(t, incarnation.failed.Load())
}

func TestNativePumpRetirementAbandonsIncompleteContainment(t *testing.T) {
	session, _, stream := newLifecycleStreamTestSession(t)
	session.turn = make(chan struct{}, sessionTurnCapacity)
	transport := newFakeClaudeTransport()
	client := claude.NewClient(nil, claude.Options{}, transport)
	require.NoError(t, client.Start(t.Context()))
	session.client = client
	require.NoError(t, session.serveNativePump(t.Context(), client))
	incarnation := session.currentNativeIncarnation()
	transport.closeErr = claude.ErrProcessContainmentIncomplete

	retired, err := session.retireExactNativeIncarnationLocked(t.Context(), incarnation)
	require.True(t, retired)
	require.ErrorIs(t, err, claude.ErrProcessContainmentIncomplete)
	stream.mu.Lock()
	require.False(t, stream.live)
	stream.mu.Unlock()
	session.stopNativePump()
}

func TestServeNativePumpContainsClientWhenRouteMintingFails(t *testing.T) {
	session := &agentSession{agent: NewAgent()}
	transport := newFakeClaudeTransport()
	client := claude.NewClient(nil, claude.Options{}, transport)
	require.NoError(t, client.Start(t.Context()))
	previous := uuidRandom
	uuidRandom = errReader{err: errors.New("route entropy failed")}
	t.Cleanup(func() { uuidRandom = previous })

	require.ErrorContains(t, session.serveNativePump(t.Context(), client), "route entropy failed")
	require.Positive(t, transport.CloseCalls())
	session.stopNativePump()
}

func TestNativePumpBarrierHonorsCancellationAndDrain(t *testing.T) {
	session := &agentSession{agent: NewAgent()}
	pump := &nativePump{
		session:  session,
		work:     make(chan nativePumpWork),
		quit:     make(chan struct{}),
		workDone: make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	require.ErrorIs(t, pump.barrier(ctx), context.Canceled)

	answer := make(chan error, 1)
	pump.work = make(chan nativePumpWork, 1)
	pump.work <- nativePumpWork{barrier: answer}
	pump.drain(t.Context())
	require.NoError(t, <-answer)
}

func TestNativePumpDrainTimeoutStopsAndJoinsTheExactReader(t *testing.T) {
	previous := sessionSettlementTimeout
	sessionSettlementTimeout = 0
	t.Cleanup(func() { sessionSettlementTimeout = previous })
	done := make(chan struct{})
	var once sync.Once
	incarnation := &nativeIncarnation{
		done: done,
		stop: func() { once.Do(func() { close(done) }) },
	}
	pump := &nativePump{incarnation: incarnation, done: done}
	require.ErrorIs(t, pump.drainReceiving(t.Context()), errNativeReceiveExited)
}

func TestNativeReaderContainsBothPanicAndUnexpectedExit(t *testing.T) {
	newIncarnation := func(client *claude.Client) *nativeIncarnation {
		incarnation := &nativeIncarnation{
			client: client, lost: make(chan struct{}), done: make(chan struct{}), mirrorReady: make(chan struct{}),
		}
		incarnation.superviseOnce.Do(func() {})

		return incarnation
	}
	session := &agentSession{agent: NewAgent()}
	pump := &nativePump{session: session}

	panicked := newIncarnation(nil)
	pump.receive(t.Context(), panicked)
	require.True(t, panicked.failed.Load())
	require.ErrorIs(t, panicked.failure(), errNativeReceiveExited)

	client := claude.NewClient(nil, claude.Options{}, newFakeClaudeTransport())
	exited := newIncarnation(client)
	pump.receive(t.Context(), exited)
	require.True(t, exited.failed.Load())
	require.ErrorIs(t, exited.failure(), claude.ErrClientNotStarted)
}

func TestNativePumpBarrierAfterOutboxExit(t *testing.T) {
	session := &agentSession{agent: NewAgent()}

	retired := &nativePump{
		session:  session,
		work:     make(chan nativePumpWork),
		workDone: make(chan struct{}),
	}
	close(retired.workDone)
	require.ErrorIs(t, retired.barrier(t.Context()), errNativeOutboxExited)

	stopped := &nativePump{
		session:  session,
		work:     make(chan nativePumpWork),
		quit:     make(chan struct{}),
		workDone: make(chan struct{}),
	}
	go func() {
		<-stopped.work
		close(stopped.workDone)
	}()
	require.ErrorIs(t, stopped.barrier(t.Context()), errNativeOutboxExited)

	cancelled := &nativePump{
		session:  session,
		work:     make(chan nativePumpWork),
		quit:     make(chan struct{}),
		workDone: make(chan struct{}),
	}
	ctx, cancel := context.WithCancel(t.Context())
	go func() {
		<-cancelled.work
		cancel()
	}()
	require.ErrorIs(t, cancelled.barrier(ctx), context.Canceled)
}

func TestServeNativePumpPropagatesIncarnationEndFailure(t *testing.T) {
	ctx := t.Context()
	session, conn, stream := newLifecycleStreamTestSession(t)
	clientA := claude.NewClient(nil, claude.Options{}, newFakeClaudeTransport())
	require.NoError(t, clientA.Start(ctx))
	require.NoError(t, session.serveNativePump(ctx, clientA))

	_, err := stream.dispatch(ctx, lifecycle.Submission{SubmissionID: "s", ClientNonce: "c"}, "nonce", func() error { return nil })
	require.NoError(t, err)
	_, err = writePreparedActionForTest(ctx, stream, "nonce", lifecycle.ActionPermission)
	require.NoError(t, err)

	conn.sessionUpdateErr = errors.New("loss delivery")
	clientB := claude.NewClient(nil, claude.Options{}, newFakeClaudeTransport())
	require.NoError(t, clientB.Start(ctx))
	err = session.serveNativePump(ctx, clientB)
	require.ErrorContains(t, err, "lifecycle delivery failed")

	pump := session.nativePumpHandle()
	pump.mu.Lock()
	require.Nil(t, pump.client, "the replacement is never published when the old incarnation cannot retire")
	pump.mu.Unlock()

	session.stopNativePump()
	require.NoError(t, clientA.Close())
}

func TestRebindBarrierFailureAbandonsOldIncarnationAndPublishesNoReplacement(t *testing.T) {
	ctx := t.Context()
	session, conn, stream := newLifecycleStreamTestSession(t)
	session.turn = make(chan struct{}, sessionTurnCapacity)
	transportA := newFakeClaudeTransport()
	clientA := claude.NewClient(session.agent.log, claude.Options{}, transportA)
	require.NoError(t, clientA.Start(ctx))
	session.client = clientA
	require.NoError(t, session.serveNativePump(ctx, clientA))
	incarnationA := session.currentNativeIncarnation()
	routeA := session.autonomousRoute()
	turnID, err := stream.openAgentTurn(ctx, routeA)
	require.NoError(t, err)
	require.NotEmpty(t, turnID)

	barrierErr := errors.New("rebind mirror barrier failed")
	session.nativePumpHandle().recordCommitError(barrierErr)
	transportB := newFakeClaudeTransport()
	clientB := claude.NewClient(session.agent.log, claude.Options{}, transportB)
	require.NoError(t, clientB.Start(ctx))
	session.mu.Lock()
	session.client = clientB
	session.mu.Unlock()

	beforeRebind := len(conn.Updates())
	err = session.serveNativePump(ctx, clientB)
	require.ErrorIs(t, err, barrierErr)
	require.NotContains(t, err.Error(), barrierErr.Error())
	require.Equal(t, 1, transportB.CloseCalls(), "unpublishable replacement is contained")
	require.False(t, session.nativePumpHandle().serves(incarnationA))
	_, finishRetired, admitted := session.admitControlCallback(ctx, routeA)
	require.False(t, admitted)
	finishRetired()
	require.Zero(t, countLifecycleEvents(t, conn, beforeRebind, func(event map[string]any) bool {
		return event["type"] == string(lifecycle.EventSnapshot) ||
			event["state"] == string(lifecycle.ForegroundIdle)
	}))
	stream.mu.Lock()
	require.False(t, stream.live)
	require.Nil(t, stream.turn)
	stream.mu.Unlock()

	session.stopNativePump()
	require.NoError(t, clientA.Close())
}

// TestServeNativePumpContainsAnIncarnationItCannotAnnounce proves the opening
// snapshot precedes the reader: a snapshot the host never received leaves no
// reader running, no pump state published, and no live process the host was never
// told about.
func TestServeNativePumpContainsAnIncarnationItCannotAnnounce(t *testing.T) {
	ctx := t.Context()
	session, conn, _ := newLifecycleStreamTestSession(t)

	transport := newFakeClaudeTransport()
	client := claude.NewClient(nil, claude.Options{}, transport)
	require.NoError(t, client.Start(ctx))
	session.client = client

	conn.sessionUpdateErr = errors.New("snapshot delivery")
	require.ErrorContains(t, session.serveNativePump(ctx, client), "lifecycle delivery failed")
	require.Equal(t, 1, transport.CloseCalls(), "the process the snapshot could not name is contained")

	pump := session.nativePumpHandle()
	pump.mu.Lock()
	defer pump.mu.Unlock()
	require.Nil(t, pump.client)
	require.Nil(t, pump.stop)
	require.Nil(t, pump.done, "no reader was ever started")
}

// TestServeNativePumpAdmitsOneIncarnationUnderConcurrentCallers proves the whole
// transition is one step. Session establishment and a prompt both point the pump
// at the same process, and exactly one incarnation is announced and one reader
// serves it — never two identities for one process lifetime.
func TestServeNativePumpAdmitsOneIncarnationUnderConcurrentCallers(t *testing.T) {
	ctx := t.Context()
	session, conn, _ := newLifecycleStreamTestSession(t)
	client := claude.NewClient(nil, claude.Options{}, newFakeClaudeTransport())
	require.NoError(t, client.Start(ctx))

	const callers = 8

	var (
		serves  sync.WaitGroup
		results = make([]error, callers)
	)

	serves.Add(callers)

	for index := range callers {
		go func() {
			defer serves.Done()

			results[index] = session.serveNativePump(ctx, client)
		}()
	}

	serves.Wait()

	for _, err := range results {
		require.NoError(t, err)
	}

	snapshots := 0

	for _, eventType := range lifecycleEventTypes(t, conn) {
		if eventType == string(lifecycle.EventSnapshot) {
			snapshots++
		}
	}

	require.Equal(t, 1, snapshots, "one process lifetime is one incarnation")

	pump := session.nativePumpHandle()
	pump.mu.Lock()
	require.Same(t, client, pump.client)
	pump.mu.Unlock()

	session.stopNativePump()
	require.NoError(t, client.Close())
}

// TestServeNativePumpRefusesAClosingSession proves close is terminal for the pump
// too: the post-response establishment hook can land while a close is tearing the
// session down, and it starts no reader and opens no incarnation behind it.
func TestServeNativePumpRefusesAClosingSession(t *testing.T) {
	ctx := t.Context()
	session, conn, _ := newLifecycleStreamTestSession(t)
	session.beginClose()

	err := session.serveNativePump(ctx, claude.NewClient(nil, claude.Options{}, newFakeClaudeTransport()))
	require.Error(t, err)

	pump := session.nativePumpHandle()
	pump.mu.Lock()
	require.Nil(t, pump.client)
	pump.mu.Unlock()

	require.Empty(t, lifecycleEventTypes(t, conn), "a closing session announces no incarnation")
}
