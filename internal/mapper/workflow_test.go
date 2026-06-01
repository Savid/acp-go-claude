package mapper

import (
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/stretchr/testify/require"
)

func TestWorkflowToolMetadata(t *testing.T) {
	t.Parallel()

	info := ToolCallInfo("Workflow", "workflow-1", map[string]any{
		"name":        "wf",
		"description": "Run subagents",
	}, ToolUpdateOptions{})
	require.Equal(t, "wf", info.Title)
	require.Equal(t, acp.ToolKindThink, info.Kind)
	require.Equal(t, "Run subagents", info.Content[0].Content.Content.Text.Text)
	require.Equal(t, acp.ToolKindThink, ToolCallInfo("Workflow", "", nil, ToolUpdateOptions{}).Kind)
}

func TestWorkflowLaunchResultSuppression(t *testing.T) {
	t.Parallel()

	for _, status := range []string{workflowLaunchAsync, workflowLaunchRemote} {
		t.Run(status, func(t *testing.T) {
			t.Parallel()

			tracker := NewWorkflowTracker()
			updates := MessageToUpdatesWithOptions(&claude.UserMessage{
				Raw: map[string]any{
					keyToolUseResult: map[string]any{
						keyStatus:        status,
						"taskId":         "task-1",
						keyRunID:         "run-1",
						keySummary:       "launched",
						keyTranscriptDir: "/tmp/transcript",
						keyScriptPath:    "/tmp/script.js",
					},
				},
				Content: []any{map[string]any{
					keyType:      claude.BlockTypeToolResult,
					keyToolUseID: "workflow-1",
					keyContent:   "Workflow launched",
					keyIsError:   false,
				}},
			}, ToolUpdateOptions{
				ToolUses: map[string]claude.ToolUseBlock{
					"workflow-1": {ID: "workflow-1", Name: toolWorkflow},
				},
				Workflow: tracker,
			})

			require.Len(t, updates, 1)
			require.True(t, tracker.HasTracked())
			require.True(t, tracker.HasActive())
			update := updates[0].ToolCallUpdate
			require.Equal(t, acp.ToolCallId("workflow-1"), update.ToolCallId)
			require.Equal(t, acp.ToolCallStatusInProgress, *update.Status)
			require.Equal(t, acp.ToolKindThink, *update.Kind)
			require.Equal(t, "Workflow launched", update.Content[0].Content.Content.Text.Text)
			require.Equal(t, map[string]any{
				keyType:      claude.BlockTypeToolResult,
				keyToolUseID: "workflow-1",
				keyContent:   "Workflow launched",
				keyIsError:   false,
			}, update.RawOutput)

			claudeMeta := requireClaudeMeta(t, update.Meta)
			require.Equal(t, toolWorkflow, claudeMeta[keyToolName])
			require.Equal(t, map[string]any{
				keyTaskIDMeta:      "task-1",
				keyRunID:           "run-1",
				keySummary:         "launched",
				keyLaunchStatus:    status,
				keyTranscriptDir:   "/tmp/transcript",
				keyLogAvailable:    true,
				keyScriptPath:      "/tmp/script.js",
				keyScriptAvailable: true,
			}, claudeMeta[keyWorkflow])
		})
	}
}

func TestWorkflowLaunchResultErrorUsesGenericFailure(t *testing.T) {
	t.Parallel()

	updates := MessageToUpdatesWithOptions(&claude.UserMessage{
		Raw: map[string]any{
			keyToolUseResult: map[string]any{keyStatus: workflowLaunchAsync},
		},
		Content: []any{map[string]any{
			keyType:      claude.BlockTypeToolResult,
			keyToolUseID: "workflow-1",
			keyContent:   "review denied",
			keyIsError:   true,
		}},
	}, ToolUpdateOptions{
		ToolUses: map[string]claude.ToolUseBlock{
			"workflow-1": {ID: "workflow-1", Name: toolWorkflow},
		},
		Workflow: NewWorkflowTracker(),
	})

	require.Len(t, updates, 1)
	require.Equal(t, acp.ToolCallStatusFailed, *updates[0].ToolCallUpdate.Status)
	require.NotContains(t, requireClaudeMeta(t, updates[0].ToolCallUpdate.Meta), keyWorkflow)
}

func TestWorkflowTaskStartedInProgress(t *testing.T) {
	t.Parallel()

	tracker := NewWorkflowTracker()
	require.False(t, tracker.HasActive())
	updates := MessageToUpdatesWithOptions(&claude.SystemMessage{
		Subtype: systemSubtypeTaskStarted,
		Raw: map[string]any{
			keyType:            claude.MessageTypeSystem,
			"subtype":          systemSubtypeTaskStarted,
			keyTaskID:          "task-1",
			keyToolUseID:       "workflow-1",
			keyWorkflowNameRaw: "ship-it",
			keyDescription:     "Ship the feature",
			keyTaskType:        "local_workflow",
		},
	}, ToolUpdateOptions{Workflow: tracker})

	require.Len(t, updates, 1)
	update := updates[0].ToolCallUpdate
	require.Equal(t, acp.ToolCallId("workflow-1"), update.ToolCallId)
	require.Equal(t, acp.ToolCallStatusInProgress, *update.Status)
	require.Equal(t, acp.ToolKindThink, *update.Kind)
	require.Equal(t, "ship-it", *update.Title)
	require.Equal(t, "Ship the feature", update.Content[0].Content.Content.Text.Text)
	workflow := requireWorkflowMeta(t, update.Meta)
	require.Equal(t, "task-1", workflow[keyTaskIDMeta])
	require.Equal(t, "ship-it", workflow[keyWorkflowName])
	require.Equal(t, "local_workflow", workflow[keyTaskType])
	require.Equal(t, toolWorkflow, requireClaudeMeta(t, update.Meta)[keyToolName])
	require.True(t, tracker.HasActive())
}

func TestWorkflowProgressAccumulatesTopologyByIndex(t *testing.T) {
	t.Parallel()

	tracker := NewWorkflowTracker()
	startWorkflowTask(t, tracker, "task-1", "workflow-1")

	updates := MessageToUpdatesWithOptions(&claude.SystemMessage{
		Subtype: systemSubtypeTaskProgress,
		Raw: map[string]any{
			keyTaskID:    "task-1",
			keyToolUseID: "workflow-1",
			keyWorkflowProgress: []any{
				map[string]any{keyType: workflowProgressTypePhase, keyIndex: 1, keyTitle: "Phase One"},
				map[string]any{keyType: workflowProgressTypeAgent, keyIndex: 1, "label": "alpha", "phaseIndex": 1, keyState: workflowStatusStart, "promptPreview": "do it"},
				map[string]any{keyType: workflowProgressTypeAgent, keyIndex: 1, "label": "alpha", "phaseIndex": 1, "agentId": "agent-1", keyState: workflowStatusProgress, "tokens": 3},
			},
			keyLastToolName:   "Workflow",
			keySubagentType:   "workflow",
			keySkipTranscript: true,
		},
	}, ToolUpdateOptions{Workflow: tracker})
	require.Len(t, updates, 1)
	workflow := requireWorkflowMeta(t, updates[0].ToolCallUpdate.Meta)
	require.Equal(t, 1, workflow[keyActiveAgents])
	require.Equal(t, 0, workflow[keyCompletedAgents])
	require.Equal(t, 0, workflow[keyFailedAgents])
	require.Equal(t, "Workflow", workflow[keyLastToolName])
	require.Equal(t, "workflow", workflow[keySubagentType])
	require.Equal(t, true, workflow[keySkipTranscript])
	require.Len(t, workflow[keyPhases], 1)
	agents := requireWorkflowList(t, workflow[keyAgents])
	require.Len(t, agents, 1)
	require.Equal(t, "agent-1", agents[0]["agentId"])
	require.Equal(t, "do it", agents[0]["promptPreview"])
	require.Equal(t, 3, agents[0]["tokens"])

	updates = MessageToUpdatesWithOptions(&claude.SystemMessage{
		Subtype: systemSubtypeTaskProgress,
		Raw: map[string]any{
			keyTaskID:    "task-1",
			keyToolUseID: "workflow-1",
			keyWorkflowProgress: []any{
				map[string]any{keyType: workflowProgressTypeAgent, keyIndex: 1, keyState: workflowStatusDone, "resultPreview": "done"},
				map[string]any{keyType: workflowProgressTypeAgent, keyIndex: 2, "label": "beta", "phaseIndex": 1, keyState: workflowStatusError},
			},
		},
	}, ToolUpdateOptions{Workflow: tracker})
	require.Len(t, updates, 1)
	workflow = requireWorkflowMeta(t, updates[0].ToolCallUpdate.Meta)
	require.Equal(t, 0, workflow[keyActiveAgents])
	require.Equal(t, 1, workflow[keyCompletedAgents])
	require.Equal(t, 1, workflow[keyFailedAgents])
	agents = requireWorkflowList(t, workflow[keyAgents])
	require.Len(t, agents, 2)
	require.Equal(t, "agent-1", agents[0]["agentId"])
	require.Equal(t, "do it", agents[0]["promptPreview"])
	require.Equal(t, "done", agents[0]["resultPreview"])
	require.Equal(t, workflowStatusError, agents[1][keyState])
}

func TestWorkflowTerminalStatuses(t *testing.T) {
	t.Parallel()

	tests := []struct {
		status     string
		wantStatus *acp.ToolCallStatus
	}{
		{status: workflowStatusCompleted, wantStatus: ptrStatus(acp.ToolCallStatusCompleted)},
		{status: workflowStatusFailed, wantStatus: ptrStatus(acp.ToolCallStatusFailed)},
		{status: workflowStatusStopped, wantStatus: ptrStatus(acp.ToolCallStatusFailed)},
		{status: workflowStatusKilled, wantStatus: ptrStatus(acp.ToolCallStatusFailed)},
		{status: workflowStatusPending, wantStatus: ptrStatus(acp.ToolCallStatusInProgress)},
		{status: workflowStatusRunning, wantStatus: ptrStatus(acp.ToolCallStatusInProgress)},
		{status: workflowStatusPaused, wantStatus: ptrStatus(acp.ToolCallStatusInProgress)},
		{status: "future", wantStatus: nil},
	}

	for _, tt := range tests {
		t.Run(tt.status, func(t *testing.T) {
			t.Parallel()

			tracker := NewWorkflowTracker()
			startWorkflowTask(t, tracker, "task-1", "workflow-1")
			updates := MessageToUpdatesWithOptions(&claude.SystemMessage{
				Subtype: systemSubtypeTaskUpdated,
				Raw: map[string]any{
					keyTaskID: "task-1",
					keyPatch:  map[string]any{keyStatus: tt.status},
				},
			}, ToolUpdateOptions{Workflow: tracker})

			require.Len(t, updates, 1)
			if tt.wantStatus == nil {
				require.Nil(t, updates[0].ToolCallUpdate.Status)
			} else {
				require.Equal(t, *tt.wantStatus, *updates[0].ToolCallUpdate.Status)
			}
			require.Equal(t, tt.status, requireWorkflowMeta(t, updates[0].ToolCallUpdate.Meta)[keyStatus])
			require.True(t, tracker.HasTracked())
			require.Equal(t, tt.wantStatus == nil || *tt.wantStatus == acp.ToolCallStatusInProgress, tracker.HasActive())
		})
	}
}

func TestWorkflowNotificationPreservesSummaryUsageAndOutput(t *testing.T) {
	t.Parallel()

	tracker := NewWorkflowTracker()
	startWorkflowTask(t, tracker, "task-1", "workflow-1")

	updates := MessageToUpdatesWithOptions(&claude.SystemMessage{
		Subtype: systemSubtypeTaskNotification,
		Raw: map[string]any{
			keyTaskID:     "task-1",
			keyToolUseID:  "workflow-1",
			keyStatus:     workflowStatusCompleted,
			keyOutputFile: "/tmp/workflow.out",
			keySummary:    "Workflow completed",
			keyUsage:      map[string]any{"total_tokens": 4.0, "duration_ms": 10.0},
		},
	}, ToolUpdateOptions{Workflow: tracker})

	require.Len(t, updates, 1)
	require.Equal(t, acp.ToolCallStatusCompleted, *updates[0].ToolCallUpdate.Status)
	require.Equal(t, "Workflow completed", updates[0].ToolCallUpdate.Content[0].Content.Content.Text.Text)
	workflow := requireWorkflowMeta(t, updates[0].ToolCallUpdate.Meta)
	require.Equal(t, "Workflow completed", workflow[keySummary])
	require.Equal(t, "/tmp/workflow.out", workflow[keyOutputFile])
	require.Equal(t, true, workflow[keyOutputAvailable])
	require.Equal(t, map[string]any{"total_tokens": 4.0, "duration_ms": 10.0}, workflow[keyUsage])
}

func TestWorkflowMalformedFramesRecordErrors(t *testing.T) {
	t.Parallel()

	tracker := NewWorkflowTracker()
	require.Nil(t, MessageToUpdatesWithOptions(&claude.SystemMessage{
		Subtype: systemSubtypeTaskProgress,
		Raw:     map[string]any{keyTaskID: "task-1", keyToolUseID: "workflow-1", keyWorkflowProgress: "bad"},
	}, ToolUpdateOptions{Workflow: tracker}))
	require.Nil(t, MessageToUpdatesWithOptions(&claude.SystemMessage{
		Subtype: systemSubtypeTaskUpdated,
		Raw:     map[string]any{keyTaskID: "unknown", keyPatch: map[string]any{keyStatus: workflowStatusCompleted}},
	}, ToolUpdateOptions{Workflow: tracker}))

	errors := tracker.DrainFrameErrors()
	require.Len(t, errors, 2)
	require.Equal(t, WorkflowFrameError{Outcome: workflowOutcomeDropped, ErrorType: workflowErrorBadProgress, FrameSubtype: systemSubtypeTaskProgress}, errors[0])
	require.Equal(t, WorkflowFrameError{Outcome: workflowOutcomeDropped, ErrorType: workflowErrorUnknownTask, FrameSubtype: systemSubtypeTaskUpdated}, errors[1])
	require.Nil(t, tracker.DrainFrameErrors())
}

func TestWorkflowBranchCoverage(t *testing.T) {
	t.Parallel()

	var nilTracker *WorkflowTracker
	require.False(t, nilTracker.HasActive())
	require.Nil(t, nilTracker.DrainFrameErrors())
	require.Nil(t, workflowSystemUpdates(nil, ToolUpdateOptions{Workflow: NewWorkflowTracker()}))
	require.Nil(t, workflowSystemUpdates(&claude.SystemMessage{Subtype: "other"}, ToolUpdateOptions{Workflow: NewWorkflowTracker()}))
	require.Nil(t, workflowSystemUpdates(&claude.SystemMessage{Subtype: systemSubtypeTaskStarted}, ToolUpdateOptions{}))
	require.Nil(t, workflowLaunchResultUpdates(claude.ToolResultBlock{
		ToolUseID: "workflow-1",
	}, ToolUpdateOptions{}, map[string]any{keyStatus: workflowLaunchAsync}))
	require.Nil(t, workflowLaunchResultUpdates(claude.ToolResultBlock{
		ToolUseID: "workflow-1",
	}, ToolUpdateOptions{
		ToolUses: map[string]claude.ToolUseBlock{"workflow-1": {ID: "workflow-1", Name: "Write"}},
	}, map[string]any{keyStatus: workflowLaunchAsync}))
	require.NotNil(t, workflowLaunchResultUpdates(claude.ToolResultBlock{
		ToolUseID: "workflow-1",
	}, ToolUpdateOptions{
		ToolUses: map[string]claude.ToolUseBlock{"workflow-1": {ID: "workflow-1", Name: toolWorkflow}},
	}, map[string]any{keyStatus: workflowLaunchAsync}))

	tracker := NewWorkflowTracker()
	require.Nil(t, tracker.taskStarted(map[string]any{keyTaskID: "task-1"}, ""))
	require.Nil(t, tracker.taskProgress(map[string]any{keyTaskID: "task-1"}, ""))
	require.Len(t, tracker.taskProgress(map[string]any{
		keyTaskID:    "task-branch",
		keyToolUseID: "workflow-branch",
		keyWorkflowProgress: []any{
			nil,
			map[string]any{keyType: "future"},
		},
	}, ""), 1)
	require.Nil(t, tracker.taskNotification(map[string]any{keyTaskID: "task-1"}, ""))
	require.Empty(t, tracker.toolUseIDForTask(""))
	nilTracker.recordError(systemSubtypeTaskProgress, workflowErrorBadProgress)

	state := &workflowState{}
	require.False(t, state.active())
	state.applyLaunch(nil)
	state.applyTopFields(nil)
	state.applyPatch(nil)
	state.applyPatch(map[string]any{keyEndTime: 1})
	state.mergePhase(map[string]any{keyType: workflowProgressTypePhase})
	state.mergeAgent(map[string]any{keyType: workflowProgressTypeAgent})
	require.Empty(t, state.meta()[keyPhases])
	require.Empty(t, state.meta()[keyAgents])

	meta := map[string]any{}
	setWorkflowUpdateMeta(meta, nil)
	require.Empty(t, meta)
	setWorkflowUpdateMeta(meta, map[string]any{keyTaskIDMeta: "task-1"})
	require.Equal(t, "task-1", requireWorkflowMeta(t, meta)[keyTaskIDMeta])

	cloned := cloneWorkflowValue(map[string]any{
		"nested": map[string]any{"ok": true},
		"list":   []any{map[string]any{"value": "x"}},
		"strings": []string{
			"a",
			"b",
		},
	})
	require.Equal(t, map[string]any{
		"nested": map[string]any{"ok": true},
		"list":   []any{map[string]any{"value": "x"}},
		"strings": []string{
			"a",
			"b",
		},
	}, cloned)
}

func startWorkflowTask(t *testing.T, tracker *WorkflowTracker, taskID string, toolUseID string) {
	t.Helper()

	updates := MessageToUpdatesWithOptions(&claude.SystemMessage{
		Subtype: systemSubtypeTaskStarted,
		Raw: map[string]any{
			keyTaskID:          taskID,
			keyToolUseID:       toolUseID,
			keyWorkflowNameRaw: "workflow",
		},
	}, ToolUpdateOptions{Workflow: tracker})
	require.Len(t, updates, 1)
}

func requireClaudeMeta(t *testing.T, meta map[string]any) map[string]any {
	t.Helper()

	claudeMeta, ok := meta[keyClaude].(map[string]any)
	require.True(t, ok)

	return claudeMeta
}

func requireWorkflowMeta(t *testing.T, meta map[string]any) map[string]any {
	t.Helper()

	workflow, ok := requireClaudeMeta(t, meta)[keyWorkflow].(map[string]any)
	require.True(t, ok)

	return workflow
}

func requireWorkflowList(t *testing.T, value any) []map[string]any {
	t.Helper()

	raw, ok := value.([]map[string]any)
	require.True(t, ok)

	return raw
}

func ptrStatus(status acp.ToolCallStatus) *acp.ToolCallStatus {
	return &status
}
