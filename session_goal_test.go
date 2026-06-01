package claudeacp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/savid/acp-go-claude/internal/mapper"
	"github.com/stretchr/testify/require"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestGoalMetaParsingValidationAndCanonicalOutput(t *testing.T) {
	t.Parallel()

	input, err := parseGoalFromMeta(map[string]any{
		claudeMetaKey: map[string]any{
			claudeGoalMetaKey: map[string]any{
				goalFieldObjective:           " Ship OAuth login ",
				goalFieldCompletionCondition: " Browser login works ",
				goalFieldStatus:              ClaudeGoalStatusBlocked,
				goalFieldReason:              " Waiting for credentials ",
				goalFieldCreatedAt:           "ignored",
				goalFieldUpdatedAt:           nil,
				goalFieldGoalID:              "ignored",
				goalFieldSource:              "ignored",
			},
		},
	})
	require.NoError(t, err)
	require.True(t, input.present)
	require.False(t, input.clear)
	require.Equal(t, ClaudeGoal{
		Objective:           "Ship OAuth login",
		CompletionCondition: "Browser login works",
		Status:              ClaudeGoalStatusBlocked,
		Reason:              "Waiting for credentials",
	}, input.goal)

	require.Equal(t, map[string]any{
		goalFieldObjective:           "Ship OAuth login",
		goalFieldCompletionCondition: "Browser login works",
		goalFieldStatus:              ClaudeGoalStatusBlocked,
		goalFieldCreatedAt:           nil,
		goalFieldUpdatedAt:           nil,
		goalFieldCompletedAt:         nil,
		goalFieldReason:              "Waiting for credentials",
		goalFieldReasonCode:          nil,
		goalFieldGoalID:              nil,
		goalFieldSource:              nil,
	}, canonicalGoalMeta(input.goal))

	_, err = parseGoalValue("goal")
	require.ErrorContains(t, err, "_meta.claude.goal must be null or an object")

	_, err = parseGoalValue(map[string]any{goalFieldStatus: ClaudeGoalStatusActive})
	require.ErrorContains(t, err, "_meta.claude.goal.objective is required")

	_, err = parseGoalValue(map[string]any{
		goalFieldObjective: "Ship",
		goalFieldStatus:    ClaudeGoalStatusCompleted,
	})
	require.ErrorContains(t, err, `_meta.claude.goal.status must be "active" or "blocked"`)

	_, err = parseGoalValue(map[string]any{
		goalFieldObjective:  "Ship",
		goalFieldReasonCode: goalReasonNativeFailed,
	})
	require.ErrorContains(t, err, "_meta.claude.goal.reasonCode is adapter-owned")

	_, err = parseGoalValue(map[string]any{
		goalFieldObjective: strings.Repeat("x", maxGoalObjectiveBytes+1),
	})
	require.ErrorContains(t, err, "must be at most 4096 bytes")

	_, err = parseGoalValue(map[string]any{
		goalFieldObjective: "Ship",
		"unexpected":       true,
	})
	require.ErrorContains(t, err, "_meta.claude.goal.unexpected is not supported")

	input, err = parseGoalFromMeta(nil)
	require.NoError(t, err)
	require.False(t, input.present)

	input, err = parseGoalFromMeta(map[string]any{claudeMetaKey: map[string]any{claudeGoalMetaKey: nil}})
	require.NoError(t, err)
	require.True(t, input.present)
	require.True(t, input.clear)

	input, err = parseGoalValue(map[string]any{goalFieldObjective: "Ship"})
	require.NoError(t, err)
	require.Equal(t, ClaudeGoalStatusActive, input.goal.Status)

	input, err = parseGoalValue(map[string]any{goalFieldObjective: "Ship", goalFieldReason: "   "})
	require.NoError(t, err)
	require.Empty(t, input.goal.Reason)

	_, err = parseGoalValue(map[string]any{goalFieldObjective: 123})
	require.ErrorContains(t, err, "_meta.claude.goal.objective must be a string")

	_, err = parseGoalValue(map[string]any{goalFieldObjective: "   "})
	require.ErrorContains(t, err, "_meta.claude.goal.objective is required")

	_, err = parseGoalValue(map[string]any{
		goalFieldObjective:           "Ship",
		goalFieldCompletionCondition: 123,
	})
	require.ErrorContains(t, err, "_meta.claude.goal.completionCondition must be a string or null")

	_, err = parseGoalValue(map[string]any{
		goalFieldObjective: "Ship",
		goalFieldStatus:    123,
	})
	require.ErrorContains(t, err, "_meta.claude.goal.status must be a string or null")

	_, err = parseGoalValue(map[string]any{
		goalFieldObjective: "Ship",
		goalFieldReason:    strings.Repeat("x", maxGoalTextBytes+1),
	})
	require.ErrorContains(t, err, "_meta.claude.goal.reason must be at most 4096 bytes")
}

func TestSetClaudeGoalExtensionSetAndClear(t *testing.T) {
	t.Parallel()

	agent := NewAgent(WithClaudeHome(t.TempDir()))
	client := &stubAgentClient{}
	agent.setConnection(client)

	session := &Session{agent: agent, id: "session-1"}
	agent.sessions[session.id] = session

	params := mustJSONRaw(t, SetGoalRequest(session.id, ClaudeGoal{
		Objective:           "Ship OAuth login",
		CompletionCondition: "Browser login works",
		Status:              ClaudeGoalStatusActive,
	}))

	resp, err := agent.HandleExtensionMethod(context.Background(), claudeSessionSetGoalMethod, params)
	require.NoError(t, err)

	respMap := requireAnyMap(t, resp)
	goal := requireAnyMap(t, respMap[claudeGoalMetaKey])
	require.Equal(t, "Ship OAuth login", goal[goalFieldObjective])
	require.Equal(t, "Browser login works", goal[goalFieldCompletionCondition])
	require.Equal(t, ClaudeGoalStatusActive, goal[goalFieldStatus])
	require.NotEmpty(t, goal[goalFieldCreatedAt])
	require.NotEmpty(t, goal[goalFieldUpdatedAt])
	require.Nil(t, goal[goalFieldCompletedAt])
	require.Nil(t, goal[goalFieldReason])
	require.Nil(t, goal[goalFieldReasonCode])
	require.Nil(t, goal[goalFieldGoalID])
	require.Equal(t, ClaudeGoalSourceClient, goal[goalFieldSource])

	updates := client.recordedUpdates()
	require.Len(t, updates, 1)
	info := updates[0].Update.SessionInfoUpdate
	require.NotNil(t, info)
	require.Nil(t, info.Title)
	require.Nil(t, info.UpdatedAt)
	claudeMeta := requireAnyMap(t, info.Meta[claudeMetaKey])
	require.Equal(t, goal, claudeMeta[claudeGoalMetaKey])

	resp, err = agent.HandleExtensionMethod(context.Background(), claudeSessionSetGoalMethod, mustJSONRaw(t, ClearGoalRequest(session.id)))
	require.NoError(t, err)
	respMap = requireAnyMap(t, resp)
	require.Nil(t, respMap[claudeGoalMetaKey])

	updates = client.recordedUpdates()
	require.Len(t, updates, 2)
	claudeMeta = requireAnyMap(t, updates[1].Update.SessionInfoUpdate.Meta[claudeMetaKey])
	require.Nil(t, claudeMeta[claudeGoalMetaKey])
}

func TestSetClaudeGoalExtensionValidationErrors(t *testing.T) {
	t.Parallel()

	agent := NewAgent(WithClaudeHome(t.TempDir()))
	session := &Session{agent: agent, id: "session-1"}
	agent.sessions[session.id] = session

	_, err := agent.HandleExtensionMethod(context.Background(), claudeSessionSetGoalMethod, json.RawMessage(`{`))
	require.Error(t, err)

	_, err = agent.HandleExtensionMethod(context.Background(), claudeSessionSetGoalMethod, mustJSONRaw(t, map[string]any{
		claudeGoalMetaKey: nil,
	}))
	require.Error(t, err)

	_, err = agent.HandleExtensionMethod(context.Background(), claudeSessionSetGoalMethod, mustJSONRaw(t, map[string]any{
		acpFieldSessionID: "session-1",
	}))
	require.Error(t, err)

	_, err = agent.HandleExtensionMethod(context.Background(), claudeSessionSetGoalMethod, json.RawMessage(`{"sessionId":"session-1","goal":`))
	require.Error(t, err)

	_, err = agent.HandleExtensionMethod(context.Background(), claudeSessionSetGoalMethod, mustJSONRaw(t, map[string]any{
		acpFieldSessionID: session.id,
		claudeGoalMetaKey: "bad",
	}))
	require.Error(t, err)

	_, err = agent.HandleExtensionMethod(context.Background(), claudeSessionSetGoalMethod, mustJSONRaw(t, map[string]any{
		acpFieldSessionID: "missing",
		claudeGoalMetaKey: nil,
	}))
	require.Error(t, err)

	agent.setConnection(&stubAgentClient{updateErr: errors.New("update failed")})
	_, err = agent.HandleExtensionMethod(context.Background(), claudeSessionSetGoalMethod, mustJSONRaw(t, SetGoalRequest(session.id, ClaudeGoal{Objective: "Ship"})))
	require.ErrorContains(t, err, "update failed")
}

func TestGoalLifecycleMetaValidationErrors(t *testing.T) {
	t.Parallel()

	meta := map[string]any{claudeMetaKey: map[string]any{claudeGoalMetaKey: "bad"}}
	agent := NewAgent(WithClaudeHome(t.TempDir()))

	_, err := agent.NewSession(context.Background(), acp.NewSessionRequest{Cwd: "/repo", McpServers: []acp.McpServer{}, Meta: meta})
	require.Error(t, err)

	_, err = agent.ResumeSession(context.Background(), acp.ResumeSessionRequest{SessionId: "session-1", Cwd: "/repo", McpServers: []acp.McpServer{}, Meta: meta})
	require.Error(t, err)

	_, err = agent.LoadSession(context.Background(), acp.LoadSessionRequest{SessionId: "session-1", Cwd: "/repo", McpServers: []acp.McpServer{}, Meta: meta})
	require.Error(t, err)

	_, err = agent.UnstableForkSession(context.Background(), acp.UnstableForkSessionRequest{SessionId: "session-1", Cwd: "/repo", McpServers: []acp.UnstableMcpServer{}, Meta: meta})
	require.Error(t, err)

	closedAgent := NewAgent(WithClaudeHome(t.TempDir()))
	require.NoError(t, closedAgent.Close())
	_, err = closedAgent.NewSession(context.Background(), acp.NewSessionRequest{Cwd: "/repo", McpServers: []acp.McpServer{}})
	require.ErrorIs(t, err, errAgentClosed)

	closedForkAgent := NewAgent(WithClaudeHome(t.TempDir()))
	closedForkAgent.sessions["session-1"] = &Session{permissionRules: map[string]string{}}
	closedForkAgent.closed = true
	_, err = closedForkAgent.UnstableForkSession(context.Background(), acp.UnstableForkSessionRequest{SessionId: "session-1", Cwd: "/repo", McpServers: []acp.UnstableMcpServer{}})
	require.ErrorIs(t, err, errAgentClosed)
}

func TestGoalResponseMetaAndListSummary(t *testing.T) {
	t.Parallel()

	session := &Session{
		id:    "session-1",
		cwd:   "/repo",
		model: "sonnet",
		availableModels: []claude.AvailableModelInfo{{
			Value:                 "sonnet",
			SupportedEffortLevels: []string{effortLow, effortHigh},
		}},
		effort: effortHigh,
	}
	session.setGoalSnapshot(&ClaudeGoal{
		Objective:           "Ship " + strings.Repeat("x", maxGoalSummaryRunes+20),
		CompletionCondition: "All tests pass",
		Status:              ClaudeGoalStatusActive,
		CreatedAt:           "2026-05-27T00:00:00Z",
		UpdatedAt:           "2026-05-27T00:00:01Z",
		Source:              ClaudeGoalSourceClient,
	}, true)

	meta := sessionResponseMeta(session)
	claudeMeta := requireAnyMap(t, meta[claudeMetaKey])
	require.Equal(t, "sonnet", claudeMeta[claudeModelMetaModelIDKey])
	require.Equal(t, effortHigh, claudeMeta[claudeModelMetaVariantKey])
	require.Equal(t, []string{effortLow, effortHigh}, claudeMeta[claudeModelMetaAvailableVariantsKey])
	require.Contains(t, claudeMeta, claudeGoalMetaKey)

	info := session.sessionInfo(session.id)
	summary := requireAnyMap(t, requireAnyMap(t, info.Meta[claudeMetaKey])[claudeGoalMetaKey])
	require.Equal(t, ClaudeGoalStatusActive, summary[goalFieldStatus])
	objective, ok := summary[goalFieldObjective].(string)
	require.True(t, ok)
	require.LessOrEqual(t, len([]rune(objective)), maxGoalSummaryRunes)
	require.NotContains(t, summary, goalFieldCompletionCondition)
}

func TestAgentLoadSessionGoalReplayAndLifecycleOverride(t *testing.T) {
	t.Parallel()

	const sessionID = "11111111-1111-4111-8111-111111111111"

	claudeHome := t.TempDir()
	writeTranscript(t, claudeHome, "/repo", sessionID, []string{
		`{"type":"user","uuid":"22222222-2222-4222-8222-222222222222","cwd":"/repo","sessionId":"11111111-1111-4111-8111-111111111111","timestamp":"2026-05-14T00:00:00Z","message":{"content":"hello"}}`,
		`{"type":"attachment","timestamp":"2026-05-14T00:00:01Z","attachment":{"type":"goal_status","met":false,"condition":"Stored goal"}}`,
	})

	fake := newAgentFakeTransport()
	agent := NewAgent(WithClaudeHome(claudeHome))
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		require.Equal(t, sessionID, options.ResumeID)

		return claude.NewClient(nil, options, fake)
	}

	client := &recordingACPClient{}
	_ = connectAgentForTest(t, agent, client)

	load, err := agent.LoadSession(context.Background(), acp.LoadSessionRequest{
		SessionId:  sessionID,
		Cwd:        "/repo",
		McpServers: []acp.McpServer{},
		Meta: map[string]any{claudeMetaKey: map[string]any{claudeGoalMetaKey: map[string]any{
			goalFieldObjective:           "Lifecycle goal",
			goalFieldCompletionCondition: "Lifecycle condition",
		}}},
	})
	require.NoError(t, err)

	responseGoal := requireAnyMap(t, requireAnyMap(t, load.Meta[claudeMetaKey])[claudeGoalMetaKey])
	require.Equal(t, "Lifecycle goal", responseGoal[goalFieldObjective])

	require.Eventually(t, func() bool {
		return len(client.recordedUpdates()) >= 2
	}, time.Second, 10*time.Millisecond)

	updates := client.recordedUpdates()
	require.NotNil(t, updates[0].Update.UserMessageChunk)
	require.Equal(t, "hello", updates[0].Update.UserMessageChunk.Content.Text.Text)
	require.NotNil(t, updates[1].Update.SessionInfoUpdate)
	infoGoal := requireAnyMap(t, requireAnyMap(t, updates[1].Update.SessionInfoUpdate.Meta[claudeMetaKey])[claudeGoalMetaKey])
	require.Equal(t, "Lifecycle goal", infoGoal[goalFieldObjective])
	require.Equal(t, "Lifecycle condition", infoGoal[goalFieldCompletionCondition])
}

func TestAgentLoadSessionGoalReplayFailureBranches(t *testing.T) {
	t.Parallel()

	const sessionID = "11111111-1111-4111-8111-111111111111"

	t.Run("scan canceled after replay", func(t *testing.T) {
		t.Parallel()

		claudeHome := t.TempDir()
		writeTranscript(t, claudeHome, "/repo", sessionID, []string{
			`{"type":"user","uuid":"22222222-2222-4222-8222-222222222222","cwd":"/repo","sessionId":"11111111-1111-4111-8111-111111111111","timestamp":"2026-05-14T00:00:00Z","message":{"content":"hello"}}`,
			`{"type":"attachment","timestamp":"2026-05-14T00:00:01Z","attachment":{"type":"goal_status","met":false,"condition":"Stored goal"}}`,
		})

		fake := newAgentFakeTransport()
		agent := NewAgent(WithClaudeHome(claudeHome))
		agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
			require.Equal(t, sessionID, options.ResumeID)

			return claude.NewClient(nil, options, fake)
		}

		ctx, cancel := context.WithCancel(context.Background())
		client := &cancelOnSessionUpdateClient{cancel: cancel}
		agent.setConnection(client)

		_, err := agent.LoadSession(ctx, acp.LoadSessionRequest{
			SessionId:  sessionID,
			Cwd:        "/repo",
			McpServers: []acp.McpServer{},
		})
		require.ErrorContains(t, err, "context canceled")
		require.True(t, fake.isClosed())
		require.Empty(t, agent.sessions)
	})

	t.Run("goal update error", func(t *testing.T) {
		t.Parallel()

		claudeHome := t.TempDir()
		writeTranscript(t, claudeHome, "/repo", sessionID, []string{
			`{"type":"user","uuid":"22222222-2222-4222-8222-222222222222","cwd":"/repo","sessionId":"11111111-1111-4111-8111-111111111111","timestamp":"2026-05-14T00:00:00Z","message":{"content":"hello"}}`,
			`{"type":"attachment","timestamp":"2026-05-14T00:00:01Z","attachment":{"type":"goal_status","met":false,"condition":"Stored goal"}}`,
		})

		fake := newAgentFakeTransport()
		agent := NewAgent(WithClaudeHome(claudeHome))
		agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
			require.Equal(t, sessionID, options.ResumeID)

			return claude.NewClient(nil, options, fake)
		}

		updateErr := errors.New("goal update failed")
		agent.setConnection(&stubAgentClient{updateErr: updateErr, updateErrAfter: 1})

		_, err := agent.LoadSession(context.Background(), acp.LoadSessionRequest{
			SessionId:  sessionID,
			Cwd:        "/repo",
			McpServers: []acp.McpServer{},
			Meta: map[string]any{claudeMetaKey: map[string]any{claudeGoalMetaKey: map[string]any{
				goalFieldObjective: "Lifecycle goal",
			}}},
		})
		require.ErrorIs(t, err, updateErr)
		require.True(t, fake.isClosed())
		require.Empty(t, agent.sessions)
	})
}

func TestGoalSessionMirrorAlwaysEnabledWithoutStore(t *testing.T) {
	t.Parallel()

	fake := newAgentFakeTransport()
	var captured claude.Options
	agent := NewAgent(WithClaudeHome(t.TempDir()))
	agent.newClaudeClient = func(_ *slog.Logger, options claude.Options) *claude.Client {
		captured = options

		return claude.NewClient(nil, options, fake)
	}

	resp, err := agent.NewSession(context.Background(), NewSessionRequest(
		"/repo",
		WithSessionGoal(ClaudeGoal{Objective: "Storeless mirror goal"}),
	))
	require.NoError(t, err)
	require.True(t, captured.SessionMirror)
	require.Nil(t, agent.options.SessionStore)
	require.Equal(t, "Storeless mirror goal", requireAnyMap(t, requireAnyMap(t, resp.Meta[claudeMetaKey])[claudeGoalMetaKey])[goalFieldObjective])
}

func TestGoalObservabilityPrivacy(t *testing.T) {
	t.Parallel()

	reader := sdkmetric.NewManualReader()
	recorder := tracetest.NewSpanRecorder()
	tracerProvider := sdktrace.NewTracerProvider(
		sdktrace.WithSampler(sdktrace.AlwaysSample()),
		sdktrace.WithSpanProcessor(recorder),
	)
	meterProvider := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	var logs bytes.Buffer
	agent := NewAgent(
		WithClaudeHome(t.TempDir()),
		WithLogger(slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))),
		WithMeterProvider(meterProvider),
		WithTracerProvider(tracerProvider),
	)
	client := &stubAgentClient{}
	agent.setConnection(client)

	session := &Session{agent: agent, id: "session-1"}
	agent.sessions[session.id] = session

	secretObjective := "SECRET_GOAL_OBJECTIVE"
	secretCondition := "SECRET_GOAL_CONDITION"
	secretReason := "SECRET_GOAL_REASON"
	_, err := agent.HandleExtensionMethod(context.Background(), claudeSessionSetGoalMethod, mustJSONRaw(t, SetGoalRequest(session.id, ClaudeGoal{
		Objective:           secretObjective,
		CompletionCondition: secretCondition,
		Status:              ClaudeGoalStatusBlocked,
		Reason:              secretReason,
	})))
	require.NoError(t, err)

	session.logGoalMirrorError(context.Background(), "native_update", errors.New(secretObjective+" from downstream"))

	var metrics metricdata.ResourceMetrics
	require.NoError(t, reader.Collect(context.Background(), &metrics))

	telemetry := fmt.Sprint(tracetest.SpanStubsFromReadOnlySpans(recorder.Ended()), metrics, logs.String())
	require.Contains(t, telemetry, claudeSessionSetGoalMethod)
	require.Contains(t, logs.String(), "redacted")
	require.Contains(t, logs.String(), "error_kind=other")

	for _, forbidden := range []string{secretObjective, secretCondition, secretReason} {
		require.NotContains(t, telemetry, forbidden)
	}
}

func BenchmarkGoalStorelessMirrorExtraction(b *testing.B) {
	session := &Session{mirror: newSessionMirror(nil, nil, b.TempDir())}
	frame := &claude.TranscriptMirrorMessage{Entries: []json.RawMessage{
		json.RawMessage(`{"type":"attachment","timestamp":"2026-05-27T00:00:00Z","attachment":{"type":"goal_status","met":false,"condition":"Benchmark goal"}}`),
		json.RawMessage(`{"type":"attachment","attachment":{"type":"skill_inventory","name":"ignored"}}`),
	}}

	b.ReportAllocs()
	for b.Loop() {
		if _, err := session.handleSessionMirror(context.Background(), frame); err != nil {
			b.Fatal(err)
		}
	}
}

type cancelOnSessionUpdateClient struct {
	stubAgentClient
	cancel context.CancelFunc
}

func (c *cancelOnSessionUpdateClient) SessionUpdate(
	ctx context.Context,
	params acp.SessionNotification,
) error {
	err := c.stubAgentClient.SessionUpdate(ctx, params)
	c.cancel()

	return err
}

type cancelLateOnSessionUpdateClient struct {
	stubAgentClient
	session *Session
}

func (c *cancelLateOnSessionUpdateClient) SessionUpdate(
	ctx context.Context,
	params acp.SessionNotification,
) error {
	err := c.stubAgentClient.SessionUpdate(ctx, params)
	c.session.mu.Lock()
	cancel := c.session.lateMirrorCancel
	c.session.mu.Unlock()
	if cancel != nil {
		cancel()
	}

	return err
}

func TestTranscriptMirrorGoalMappingAndClear(t *testing.T) {
	t.Parallel()

	agent := NewAgent(WithClaudeHome(t.TempDir()))
	client := &stubAgentClient{}
	agent.setConnection(client)
	session := &Session{agent: agent, id: "session-1", mirror: newSessionMirror(nil, nil, t.TempDir())}

	frame := &claude.TranscriptMirrorMessage{Entries: []json.RawMessage{
		json.RawMessage(`{"type":"attachment","timestamp":"2026-05-27T00:00:00Z","uuid":"goal-row-1","attachment":{"type":"goal_status","sentinel":true,"met":false,"condition":"Ship OAuth login"}}`),
		json.RawMessage(`{"type":"attachment","timestamp":"2026-05-27T00:00:02Z","attachment":{"type":"goal_status","met":true,"condition":"Ship OAuth login","reason":"Done"}}`),
	}}
	handled, err := session.handleSessionMirror(context.Background(), frame)
	require.NoError(t, err)
	require.True(t, handled)

	goal, _ := session.goalSnapshot()
	require.NotNil(t, goal)
	require.Equal(t, ClaudeGoalStatusCompleted, goal.Status)
	require.Equal(t, "goal-row-1", goal.GoalID)
	require.Equal(t, "2026-05-27T00:00:02Z", goal.CompletedAt)
	require.Equal(t, "Done", goal.Reason)

	frame = &claude.TranscriptMirrorMessage{Entries: []json.RawMessage{
		json.RawMessage(`{"type":"user","timestamp":"2026-05-27T00:00:03Z","uuid":"cmd-1","message":{"content":"/goal clear"}}`),
		json.RawMessage(`{"type":"system","timestamp":"2026-05-27T00:00:04Z","subtype":"local_command","parentUuid":"cmd-1","content":"<local-command-stdout>No goal set</local-command-stdout>"}`),
	}}
	handled, err = session.handleSessionMirror(context.Background(), frame)
	require.NoError(t, err)
	require.True(t, handled)

	goal, _ = session.goalSnapshot()
	require.Nil(t, goal)
	updates := client.recordedUpdates()
	require.Len(t, updates, 2)
	require.Nil(t, requireAnyMap(t, updates[1].Update.SessionInfoUpdate.Meta[claudeMetaKey])[claudeGoalMetaKey])
}

func TestTranscriptMirrorGoalFailureOverrideAndStaleClientProtection(t *testing.T) {
	t.Parallel()

	session := &Session{}
	require.NoError(t, session.applyClientGoalInput(context.Background(), goalMetaInput{
		present: true,
		goal: ClaudeGoal{
			Objective: "Client goal",
			Status:    ClaudeGoalStatusActive,
		},
	}, false))

	changed := session.applyNativeGoal(ClaudeGoal{
		Objective:  "Older native goal",
		Status:     ClaudeGoalStatusActive,
		UpdatedAt:  "2000-01-01T00:00:00Z",
		CreatedAt:  "2000-01-01T00:00:00Z",
		Source:     ClaudeGoalSourceClaude,
		ReasonCode: goalReasonNativeFailed,
	})
	require.False(t, changed)
	goal, _ := session.goalSnapshot()
	require.Equal(t, "Client goal", goal.Objective)

	session = &Session{}
	err := session.applyTranscriptMirrorGoals(context.Background(), &claude.TranscriptMirrorMessage{Entries: []json.RawMessage{
		json.RawMessage(`{"type":"system","content":"Stop hook block cap reached; overriding stop hook."}`),
		json.RawMessage(`{"type":"attachment","timestamp":"2026-05-27T00:00:00Z","attachment":{"type":"goal_status","met":true,"condition":"Forced goal"}}`),
	}}, false)
	require.NoError(t, err)
	goal, _ = session.goalSnapshot()
	require.Equal(t, ClaudeGoalStatusBlocked, goal.Status)
	require.Equal(t, goalReasonStopHookOverride, goal.ReasonCode)

	session = &Session{}
	err = session.applyTranscriptMirrorGoals(context.Background(), &claude.TranscriptMirrorMessage{Entries: []json.RawMessage{
		json.RawMessage(`{"type":"attachment","timestamp":"2026-05-27T00:00:00Z","attachment":{"type":"goal_status","met":false,"failed":true,"condition":"Impossible","reason":"Contradiction"}}`),
	}}, false)
	require.NoError(t, err)
	goal, _ = session.goalSnapshot()
	require.Equal(t, ClaudeGoalStatusBlocked, goal.Status)
	require.Equal(t, goalReasonNativeFailed, goal.ReasonCode)
	require.Empty(t, goal.CompletedAt)
	require.Equal(t, "Contradiction", goal.Reason)
}

func TestTranscriptMirrorGoalParserBranches(t *testing.T) {
	t.Parallel()

	session := &Session{}
	require.NoError(t, session.applyClientGoalInput(context.Background(), goalMetaInput{}, false))

	err := session.applyTranscriptMirrorGoals(context.Background(), &claude.TranscriptMirrorMessage{Entries: []json.RawMessage{
		json.RawMessage(`{`),
		json.RawMessage(`{"type":"attachment","attachment":{"type":"not_goal"}}`),
		json.RawMessage(`{"type":"attachment","attachment":{"type":"goal_status","condition":"   "}}`),
		json.RawMessage(`{"type":"user","message":{"content":"/goal clear"}}`),
		json.RawMessage(`{"type":"system","subtype":"hook_response","parentUuid":"cmd-1","content":"No goal set"}`),
		json.RawMessage(`{"type":"assistant","content":"No goal set"}`),
		json.RawMessage(`{"type":"system","subtype":"local_command","parentUuid":"missing","content":"No goal set"}`),
		json.RawMessage(`{"type":"system","content":`),
	}}, false)
	require.NoError(t, err)
	goal, _ := session.goalSnapshot()
	require.Nil(t, goal)

	require.NoError(t, session.applyTranscriptMirrorGoals(context.Background(), &claude.TranscriptMirrorMessage{
		Entries: []json.RawMessage{
			json.RawMessage(`{`),
		},
	}, false))
	accumulator := goalAccumulator{}
	accumulator.applyTranscriptEntries([]json.RawMessage{
		json.RawMessage(`{`),
	})
	require.Nil(t, accumulator.snapshot())

	require.False(t, goalTimeAfter("2026-05-27T00:00:00Z", "2026-05-27T00:00:00Z"))
	require.True(t, goalTimeAfter("bad", "2026-05-27T00:00:00Z"))
	require.True(t, goalTimeAfter("2026-05-27T00:00:00Z", ""))
	require.NotEmpty(t, transcriptEntryTimestamp(map[string]any{}))
	require.NotEmpty(t, transcriptEntryTimestamp(map[string]any{"timestamp": "bad"}))

	command, ok := transcriptGoalClearCommand(map[string]any{
		jsonFieldType: "user",
		jsonFieldMessage: map[string]any{
			systemContent: []any{map[string]any{jsonFieldText: "<command-name>/goal</command-name><command-args>clear</command-args>"}},
		},
	})
	require.False(t, ok)
	require.Empty(t, command.uuid)

	require.Equal(t, goalClearOutputNone, transcriptGoalClearOutput(map[string]any{jsonFieldType: "system"}, nil))
	require.Equal(t, goalClearOutputNone, transcriptGoalClearOutput(map[string]any{jsonFieldType: "assistant"}, map[string]goalClearCandidate{"cmd": {uuid: "cmd"}}))
	require.Equal(t, goalClearOutputNone, transcriptGoalClearOutput(map[string]any{jsonFieldType: "system", jsonFieldSubtype: "hook_response"}, map[string]goalClearCandidate{"cmd": {uuid: "cmd"}}))
	require.Equal(t, goalClearOutputNone, transcriptGoalClearOutput(map[string]any{jsonFieldType: "system", jsonFieldSubtype: "local_command", "parentUuid": "missing"}, map[string]goalClearCandidate{"cmd": {uuid: "cmd"}}))
	require.Equal(t, goalClearOutputUnmatched, transcriptGoalClearOutput(
		map[string]any{jsonFieldType: "system", jsonFieldSubtype: "local_command", "parentUuid": "cmd", systemContent: "not a clear"},
		map[string]goalClearCandidate{"cmd": {uuid: "cmd"}},
	))
	require.False(t, transcriptGoalStopHookOverride(transcriptRawEntries([]json.RawMessage{json.RawMessage(`{`)})))
	require.False(t, goalClearSlashCommand([]acp.ContentBlock{acp.TextBlock("hello"), acp.TextBlock("/goal clear")}))
	require.True(t, goalClearSlashCommand([]acp.ContentBlock{acp.TextBlock("   "), acp.TextBlock("/goal clear")}))
	require.True(t, goalClearSlashCommand([]acp.ContentBlock{acp.TextBlock("/goal clear")}))

	session = &Session{agent: NewAgent(WithClaudeHome(t.TempDir()), WithLogger(slog.New(slog.DiscardHandler)))}
	(&Session{}).logNativeGoalClearUnmatched(context.Background())
	err = session.applyTranscriptMirrorGoals(context.Background(), &claude.TranscriptMirrorMessage{Entries: []json.RawMessage{
		json.RawMessage(`{"type":"user","timestamp":"2026-05-27T00:00:00Z","uuid":"cmd-log","message":{"content":"/goal clear"}}`),
		json.RawMessage(`{"type":"system","timestamp":"2026-05-27T00:00:01Z","subtype":"local_command","parentUuid":"cmd-log","content":"unexpected output"}`),
	}}, false)
	require.NoError(t, err)

	session = &Session{}
	require.True(t, session.applyNativeGoal(ClaudeGoal{Objective: "No timestamp", Status: ClaudeGoalStatusActive, Source: ClaudeGoalSourceClaude}))
	goal, _ = session.goalSnapshot()
	require.NotEmpty(t, goal.UpdatedAt)
	require.Equal(t, goal.UpdatedAt, goal.CreatedAt)
}

func TestLocalGoalClearResultAndReplayBranches(t *testing.T) {
	t.Parallel()

	agent := NewAgent(WithClaudeHome(t.TempDir()))
	client := &stubAgentClient{}
	agent.setConnection(client)
	session := &Session{agent: agent, id: "session-1"}

	require.NoError(t, session.applyClientGoalInput(context.Background(), goalMetaInput{present: true, goal: ClaudeGoal{Objective: "Ship"}}, false))
	require.NoError(t, session.applyLocalGoalClearResult(context.Background(), "not a clear", "2026-05-27T00:00:00Z"))
	goal, _ := session.goalSnapshot()
	require.NotNil(t, goal)

	require.NoError(t, session.applyLocalGoalClearResult(context.Background(), "No goal set", "2000-01-01T00:00:00Z"))
	goal, _ = session.goalSnapshot()
	require.NotNil(t, goal)

	require.NoError(t, session.applyLocalGoalClearResult(context.Background(), "No goal set", "2999-01-01T00:00:00Z"))
	goal, _ = session.goalSnapshot()
	require.Nil(t, goal)
	require.Len(t, client.recordedUpdates(), 1)

	_, err := session.applyReplayGoalSnapshot(context.Background(), "/missing/transcript.jsonl")
	require.Error(t, err)

	path := t.TempDir() + "/goal.jsonl"
	require.NoError(t, os.WriteFile(path, []byte(`{"type":"attachment","timestamp":"2026-05-27T00:00:00Z","attachment":{"type":"goal_status","met":false,"condition":"Replay goal"}}`), 0o600))
	changed, err := session.applyReplayGoalSnapshot(context.Background(), path)
	require.NoError(t, err)
	require.True(t, changed)
	goal, _ = session.goalSnapshot()
	require.Equal(t, "Replay goal", goal.Objective)

	changed, err = session.applyReplayGoalSnapshot(context.Background(), path)
	require.NoError(t, err)
	require.True(t, changed)

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = scanTranscriptGoalReader(cancelled, strings.NewReader("\nnot json\n"+`{"type":"attachment"}`))
	require.ErrorIs(t, err, context.Canceled)

	_, err = scanTranscriptGoalReader(context.Background(), errorReader{})
	require.ErrorContains(t, err, "read failed")

	require.NoError(t, session.applyClientGoalInput(context.Background(), goalMetaInput{present: true, goal: ClaudeGoal{Objective: "Newer client"}}, false))
	oldPath := t.TempDir() + "/old-goal.jsonl"
	require.NoError(t, os.WriteFile(oldPath, []byte(`{"type":"attachment","timestamp":"2000-01-01T00:00:00Z","attachment":{"type":"goal_status","met":false,"condition":"Old replay"}}`), 0o600))
	changed, err = session.applyReplayGoalSnapshot(context.Background(), oldPath)
	require.NoError(t, err)
	require.False(t, changed)
	goal, _ = session.goalSnapshot()
	require.Equal(t, "Newer client", goal.Objective)
}

func TestGoalLateProcessorAndLoggingBranches(t *testing.T) {
	t.Parallel()

	session := &Session{}
	session.startLateMirrorProcessor(context.Background())
	session.stopLateMirrorProcessor(context.Background())
	session.clearLateMirrorProcessor(make(chan struct{}))
	require.Nil(t, session.agentLogger())
	session.logGoalMirrorError(context.Background(), "ignored", nil)
	session.logGoalMirrorError(context.Background(), "ignored", io.EOF)

	agent := NewAgent(WithClaudeHome(t.TempDir()), WithLogger(slog.New(slog.DiscardHandler)))
	session.agent = agent
	require.NotNil(t, session.agentLogger())
	session.logGoalMirrorError(context.Background(), "logged", io.EOF)
	require.Equal(t, goalMirrorErrorCanceled, goalMirrorErrorKind(context.Canceled))
	require.Equal(t, goalMirrorErrorTimeout, goalMirrorErrorKind(context.DeadlineExceeded))
	require.Equal(t, goalMirrorErrorStoreAppend, goalMirrorErrorKind(fmt.Errorf("%w: downstream", errSessionMirrorAppend)))
	require.Equal(t, goalMirrorErrorAgentClosed, goalMirrorErrorKind(errAgentClosed))
	require.Equal(t, goalMirrorErrorConnection, goalMirrorErrorKind(errACPConnectionNotAttached))
	require.Equal(t, goalMirrorErrorOther, goalMirrorErrorKind(io.EOF))

	done := make(chan struct{})
	session.mu.Lock()
	session.lateMirrorCancel = func() {}
	session.lateMirrorDone = done
	session.mu.Unlock()

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	session.stopLateMirrorProcessor(ctx)

	close(done)
	session.stopLateMirrorProcessor(context.Background())

	session.mu.Lock()
	session.lateMirrorCancel = func() {}
	session.mu.Unlock()
	session.stopLateMirrorProcessor(context.Background())

	done = make(chan struct{})
	session.mu.Lock()
	session.lateMirrorCancel = func() {}
	session.lateMirrorDone = done
	session.lateMirrorStopTimeout = time.Nanosecond
	session.mu.Unlock()
	session.stopLateMirrorProcessor(context.Background())
	session.lateMirrorStopTimeout = 0
	close(done)

	done = make(chan struct{})
	session.mu.Lock()
	session.lateMirrorDone = done
	session.mu.Unlock()
	session.clearLateMirrorProcessor(done)
}

func TestGoalLateProcessorReceivesMirrorAndHandlesErrors(t *testing.T) {
	t.Parallel()

	t.Run("success", func(t *testing.T) {
		t.Parallel()

		fake := newAgentFakeTransport()
		agent := NewAgent(WithClaudeHome(t.TempDir()))
		client := &stubAgentClient{}
		agent.setConnection(client)
		claudeClient := claude.NewClient(nil, claude.Options{}, fake)
		require.NoError(t, claudeClient.Start(context.Background()))
		defer func() { require.NoError(t, claudeClient.Close()) }()
		session := &Session{
			agent:  agent,
			id:     "session-1",
			client: claudeClient,
			mirror: newSessionMirror(nil, nil, t.TempDir()),
		}
		fake.incoming <- map[string]any{
			jsonFieldType: "transcript_mirror",
			jsonFieldEntries: []any{
				map[string]any{
					jsonFieldType: "attachment",
					"timestamp":   "2026-05-27T00:00:00Z",
					"attachment": map[string]any{
						jsonFieldType: "goal_status",
						"met":         false,
						"condition":   "Late goal",
					},
				},
			},
		}

		session.startLateMirrorProcessor(context.Background())
		require.Eventually(t, func() bool {
			goal, _ := session.goalSnapshot()

			return goal != nil && goal.Objective == "Late goal"
		}, time.Second, 10*time.Millisecond)
		session.stopLateMirrorProcessor(context.Background())
	})

	t.Run("workflow update", func(t *testing.T) {
		t.Parallel()

		fake := newAgentFakeTransport()
		agent := NewAgent(WithClaudeHome(t.TempDir()))
		client := &stubAgentClient{}
		agent.setConnection(client)
		claudeClient := claude.NewClient(nil, claude.Options{}, fake)
		require.NoError(t, claudeClient.Start(context.Background()))
		defer func() { require.NoError(t, claudeClient.Close()) }()
		session := &Session{
			agent:  agent,
			id:     "session-1",
			client: claudeClient,
			mirror: newSessionMirror(nil, nil, t.TempDir()),
		}
		tracker := mapper.NewWorkflowTracker()
		require.NotEmpty(t, mapper.MessageToUpdatesWithOptions(&claude.SystemMessage{
			Subtype: "task_started",
			Raw: map[string]any{
				"task_id":     "task-1",
				"tool_use_id": "workflow-1",
			},
		}, mapper.ToolUpdateOptions{Workflow: tracker}))
		fake.incoming <- map[string]any{
			jsonFieldType:    "system",
			jsonFieldSubtype: "task_updated",
			"task_id":        "task-1",
			"patch":          map[string]any{"status": "completed"},
		}

		session.startLateMirrorProcessor(context.Background(), mapper.ToolUpdateOptions{Workflow: tracker})
		require.Eventually(t, func() bool {
			updates := client.recordedUpdates()
			if len(updates) == 0 {
				return false
			}

			toolUpdate := updates[len(updates)-1].Update.ToolCallUpdate

			return toolUpdate != nil && toolUpdate.Status != nil && *toolUpdate.Status == acp.ToolCallStatusCompleted
		}, time.Second, 10*time.Millisecond)
		session.stopLateMirrorProcessor(context.Background())
	})

	t.Run("workflow update error", func(t *testing.T) {
		t.Parallel()

		fake := newAgentFakeTransport()
		agent := NewAgent(WithClaudeHome(t.TempDir()))
		agent.setConnection(&stubAgentClient{updateErr: errors.New("update failed")})
		claudeClient := claude.NewClient(nil, claude.Options{}, fake)
		require.NoError(t, claudeClient.Start(context.Background()))
		defer func() { require.NoError(t, claudeClient.Close()) }()
		session := &Session{
			agent:  agent,
			id:     "session-1",
			client: claudeClient,
			mirror: newSessionMirror(nil, nil, t.TempDir()),
		}
		tracker := mapper.NewWorkflowTracker()
		require.NotEmpty(t, mapper.MessageToUpdatesWithOptions(&claude.SystemMessage{
			Subtype: "task_started",
			Raw: map[string]any{
				"task_id":     "task-1",
				"tool_use_id": "workflow-1",
			},
		}, mapper.ToolUpdateOptions{Workflow: tracker}))
		fake.incoming <- map[string]any{
			jsonFieldType:    "system",
			jsonFieldSubtype: "task_updated",
			"task_id":        "task-1",
			"patch":          map[string]any{"status": "completed"},
		}

		session.startLateMirrorProcessor(context.Background(), mapper.ToolUpdateOptions{Workflow: tracker})
		waitLateMirrorDone(t, session)
	})

	t.Run("raw emit error", func(t *testing.T) {
		t.Parallel()

		fake := newAgentFakeTransport()
		agent := NewAgent(WithClaudeHome(t.TempDir()))
		agent.setConnection(&stubAgentClient{extensionErr: errors.New("extension failed")})
		claudeClient := claude.NewClient(nil, claude.Options{}, fake)
		require.NoError(t, claudeClient.Start(context.Background()))
		defer func() { require.NoError(t, claudeClient.Close()) }()
		session := &Session{
			agent:       agent,
			id:          "session-1",
			client:      claudeClient,
			mirror:      newSessionMirror(nil, nil, t.TempDir()),
			rawMessages: rawMessageConfig{All: true},
		}
		fake.incoming <- map[string]any{jsonFieldType: "transcript_mirror", jsonFieldEntries: []any{}}
		session.startLateMirrorProcessor(context.Background())
		waitLateMirrorDone(t, session)
	})

	t.Run("mirror update error", func(t *testing.T) {
		t.Parallel()

		fake := newAgentFakeTransport()
		agent := NewAgent(WithClaudeHome(t.TempDir()))
		agent.setConnection(&stubAgentClient{updateErr: errors.New("update failed")})
		claudeClient := claude.NewClient(nil, claude.Options{}, fake)
		require.NoError(t, claudeClient.Start(context.Background()))
		defer func() { require.NoError(t, claudeClient.Close()) }()
		session := &Session{
			agent:  agent,
			id:     "session-1",
			client: claudeClient,
			mirror: newSessionMirror(nil, nil, t.TempDir()),
		}
		fake.incoming <- map[string]any{
			jsonFieldType: "transcript_mirror",
			jsonFieldEntries: []any{
				map[string]any{
					jsonFieldType: "attachment",
					"timestamp":   "2026-05-27T00:00:00Z",
					"attachment": map[string]any{
						jsonFieldType: "goal_status",
						"met":         false,
						"condition":   "Late error goal",
					},
				},
			},
		}
		session.startLateMirrorProcessor(context.Background())
		waitLateMirrorDone(t, session)
	})

	t.Run("canceled before next receive", func(t *testing.T) {
		t.Parallel()

		fake := newAgentFakeTransport()
		agent := NewAgent(WithClaudeHome(t.TempDir()))
		claudeClient := claude.NewClient(nil, claude.Options{}, fake)
		require.NoError(t, claudeClient.Start(context.Background()))
		defer func() { require.NoError(t, claudeClient.Close()) }()
		session := &Session{
			agent:  agent,
			id:     "session-1",
			client: claudeClient,
			mirror: newSessionMirror(nil, nil, t.TempDir()),
		}
		agent.setConnection(&cancelLateOnSessionUpdateClient{session: session})
		fake.incoming <- map[string]any{
			jsonFieldType: "transcript_mirror",
			jsonFieldEntries: []any{
				map[string]any{
					jsonFieldType: "attachment",
					"timestamp":   "2026-05-27T00:00:00Z",
					"attachment": map[string]any{
						jsonFieldType: "goal_status",
						"met":         false,
						"condition":   "Cancel late loop",
					},
				},
			},
		}
		fake.incoming <- map[string]any{jsonFieldType: "user"}

		session.startLateMirrorProcessor(context.Background())
		waitLateMirrorDone(t, session)

		msg, err := claudeClient.Receive(context.Background())
		require.NoError(t, err)
		require.IsType(t, &claude.UserMessage{}, msg)
	})
}

func TestPromptLocalGoalClearAndMirrorErrorBranches(t *testing.T) {
	t.Parallel()

	fake := newAgentFakeTransport()
	fake.resultText = "No goal set"
	agent := NewAgent(WithClaudeHome(t.TempDir()))
	agent.newClaudeClient = func(log *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(log, options, fake)
	}
	client := &recordingACPClient{}
	_ = connectAgentForTest(t, agent, client)

	resp, err := agent.NewSession(context.Background(), acp.NewSessionRequest{Cwd: "/repo", McpServers: []acp.McpServer{}})
	require.NoError(t, err)

	session, err := agent.session(resp.SessionId)
	require.NoError(t, err)
	require.NoError(t, session.applyClientGoalInput(context.Background(), goalMetaInput{present: true, goal: ClaudeGoal{Objective: "Ship"}}, false))

	_, err = agent.Prompt(context.Background(), acp.PromptRequest{
		SessionId: resp.SessionId,
		Prompt:    []acp.ContentBlock{acp.TextBlock("/goal clear")},
	})
	require.NoError(t, err)

	goal, _ := session.goalSnapshot()
	require.Nil(t, goal)

	handled, err := (&Session{mirror: newSessionMirror(nil, nil, t.TempDir())}).handleSessionMirror(context.Background(), &claude.UserMessage{})
	require.NoError(t, err)
	require.False(t, handled)

	mirrorErrorAgent := NewAgent(WithClaudeHome(t.TempDir()))
	mirrorErrorAgent.setConnection(&stubAgentClient{updateErr: errors.New("update failed")})
	mirrorErrorSession := &Session{agent: mirrorErrorAgent, id: "session-1"}
	handled, err = mirrorErrorSession.handleSessionMirror(context.Background(), &claude.TranscriptMirrorMessage{Entries: []json.RawMessage{
		json.RawMessage(`{"type":"attachment","timestamp":"2026-05-27T00:00:00Z","attachment":{"type":"goal_status","met":false,"condition":"Goal"}}`),
	}})
	require.True(t, handled)
	require.ErrorContains(t, err, "update failed")

	promptErrorAgent := NewAgent(WithClaudeHome(t.TempDir()))
	promptErrorAgent.setConnection(&stubAgentClient{updateErr: errors.New("update failed")})
	errorFake := newAgentFakeTransport()
	claudeClient := claude.NewClient(nil, claude.Options{}, errorFake)
	require.NoError(t, claudeClient.Start(context.Background()))
	defer func() { require.NoError(t, claudeClient.Close()) }()
	promptErrorSession := &Session{agent: promptErrorAgent, id: "session-1", client: claudeClient}
	require.NoError(t, promptErrorSession.applyClientGoalInput(context.Background(), goalMetaInput{present: true, goal: ClaudeGoal{Objective: "Ship"}}, false))
	_, _, err = promptErrorSession.finishPromptResult(
		context.Background(),
		context.Background(),
		acp.PromptRequest{Prompt: []acp.ContentBlock{acp.TextBlock("/goal clear")}},
		&claude.ResultMessage{Subtype: "success", StopReason: "end_turn", Result: "No goal set"},
		&promptLoopState{},
		mapper.ToolUpdateOptions{},
		false,
		true,
		"2999-01-01T00:00:00Z",
	)
	require.ErrorContains(t, err, "update failed")
}

func TestPromptSystemIdleDrainMirrorError(t *testing.T) {
	t.Parallel()

	fake := newAgentFakeTransport()
	fake.setSuppressResult(true)
	fake.setSendHook(func(payload any) {
		if _, ok := payload.(map[string]any); !ok {
			return
		}

		fake.incoming <- map[string]any{
			jsonFieldType:    "system",
			jsonFieldSubtype: systemSubtypeSessionStateChanged,
			systemState:      systemStateIdle,
		}
		fake.errs <- errors.New("drain failed")
	})

	agent := NewAgent(WithClaudeHome(t.TempDir()))
	agent.newClaudeClient = func(log *slog.Logger, options claude.Options) *claude.Client {
		return claude.NewClient(log, options, fake)
	}
	_ = connectAgentForTest(t, agent, &recordingACPClient{})

	resp, err := agent.NewSession(context.Background(), acp.NewSessionRequest{Cwd: "/repo", McpServers: []acp.McpServer{}})
	require.NoError(t, err)

	_, err = agent.Prompt(context.Background(), acp.PromptRequest{
		SessionId: resp.SessionId,
		Prompt:    []acp.ContentBlock{acp.TextBlock("hello")},
	})
	require.Error(t, err)
}

func TestDrainSessionMirrorWorkflowUpdateError(t *testing.T) {
	t.Parallel()

	fake := newAgentFakeTransport()
	agent := NewAgent(WithClaudeHome(t.TempDir()))
	agent.setConnection(&stubAgentClient{updateErr: errors.New("update failed")})
	claudeClient := claude.NewClient(nil, claude.Options{}, fake)
	require.NoError(t, claudeClient.Start(context.Background()))
	defer func() { require.NoError(t, claudeClient.Close()) }()
	session := &Session{
		agent:  agent,
		id:     "session-1",
		client: claudeClient,
		mirror: newSessionMirror(nil, nil, t.TempDir()),
	}
	tracker := mapper.NewWorkflowTracker()
	require.NotEmpty(t, mapper.MessageToUpdatesWithOptions(&claude.SystemMessage{
		Subtype: "task_started",
		Raw: map[string]any{
			"task_id":     "task-1",
			"tool_use_id": "workflow-1",
		},
	}, mapper.ToolUpdateOptions{Workflow: tracker}))
	fake.incoming <- map[string]any{
		jsonFieldType:    "system",
		jsonFieldSubtype: "task_updated",
		"task_id":        "task-1",
		"patch":          map[string]any{"status": "completed"},
	}

	err := session.drainSessionMirror(context.Background(), mapper.ToolUpdateOptions{Workflow: tracker})
	require.ErrorContains(t, err, "update failed")
}

func waitLateMirrorDone(t *testing.T, session *Session) {
	t.Helper()

	session.mu.Lock()
	done := session.lateMirrorDone
	session.mu.Unlock()
	require.NotNil(t, done)

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("late mirror processor did not stop")
	}
}

type errorReader struct{}

func (errorReader) Read([]byte) (int, error) {
	return 0, errors.New("read failed")
}

func TestScanTranscriptGoalReader(t *testing.T) {
	t.Parallel()

	goal, err := scanTranscriptGoalReader(context.Background(), strings.NewReader(strings.Join([]string{
		``,
		`not json`,
		`{"type":"attachment","timestamp":"2026-05-27T00:00:00Z","uuid":"goal-row-1","attachment":{"type":"goal_status","sentinel":true,"met":false,"condition":"Persisted goal"}}`,
		`{"type":"attachment","timestamp":"2026-05-27T00:00:01Z","attachment":{"type":"goal_status","met":true,"condition":"Persisted goal"}}`,
	}, "\n")))
	require.NoError(t, err)
	require.NotNil(t, goal)
	require.Equal(t, ClaudeGoalStatusCompleted, goal.Status)
	require.Equal(t, "goal-row-1", goal.GoalID)

	goal, err = scanTranscriptGoalReader(context.Background(), strings.NewReader(strings.Join([]string{
		`{"type":"attachment","timestamp":"2026-05-27T00:00:00Z","attachment":{"type":"goal_status","met":false,"condition":"Persisted goal"}}`,
		`{"type":"user","timestamp":"2026-05-27T00:00:01Z","uuid":"cmd-1","message":{"content":"/goal clear"}}`,
		`{"type":"system","timestamp":"2026-05-27T00:00:02Z","subtype":"local_command","parentUuid":"cmd-1","content":"Goal cleared: Persisted goal"}`,
	}, "\n")))
	require.NoError(t, err)
	require.Nil(t, goal)
}

func TestSessionGoalRequestBuilders(t *testing.T) {
	t.Parallel()

	goal := ClaudeGoal{
		Objective:           "Ship OAuth login",
		CompletionCondition: "Browser login works",
		Status:              ClaudeGoalStatusBlocked,
		Reason:              "Waiting",
	}
	req := NewSessionRequest("/repo", WithSessionGoal(goal))
	claudeMeta := requireAnyMap(t, req.Meta[claudeMetaKey])
	require.Equal(t, map[string]any{
		goalFieldObjective:           "Ship OAuth login",
		goalFieldCompletionCondition: "Browser login works",
		goalFieldStatus:              ClaudeGoalStatusBlocked,
		goalFieldReason:              "Waiting",
	}, claudeMeta[claudeGoalMetaKey])

	req = NewSessionRequest("/repo", WithSessionGoal(goal), WithSessionGoalClear())
	claudeMeta = requireAnyMap(t, req.Meta[claudeMetaKey])
	require.Nil(t, claudeMeta[claudeGoalMetaKey])

	require.Equal(t, map[string]any{
		acpFieldSessionID: acp.SessionId("session-1"),
		claudeGoalMetaKey: map[string]any{
			goalFieldObjective:           "Ship OAuth login",
			goalFieldStatus:              ClaudeGoalStatusBlocked,
			goalFieldReason:              "Waiting",
			goalFieldCompletionCondition: "Browser login works",
		},
	}, SetGoalRequest("session-1", goal))
	require.Equal(t, map[string]any{
		acpFieldSessionID: acp.SessionId("session-1"),
		claudeGoalMetaKey: nil,
	}, ClearGoalRequest("session-1"))
}

func mustJSONRaw(t *testing.T, value any) json.RawMessage {
	t.Helper()

	data, err := json.Marshal(value)
	require.NoError(t, err)

	return data
}
