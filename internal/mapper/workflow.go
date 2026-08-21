package mapper

import (
	"sort"
	"strings"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
)

const (
	systemSubtypeTaskNotification = "task_notification"
	systemSubtypeTaskProgress     = "task_progress"
	systemSubtypeTaskStarted      = "task_started"
	systemSubtypeTaskUpdated      = "task_updated"

	workflowProgressTypeAgent = "workflow_agent"
	workflowProgressTypePhase = "workflow_phase"

	workflowLaunchAsync  = "async_launched"
	workflowLaunchRemote = "remote_launched"

	workflowStatusCompleted = "completed"
	workflowStatusDone      = "done"
	workflowStatusError     = "error"
	workflowStatusFailed    = "failed"
	workflowStatusKilled    = "killed"
	workflowStatusPaused    = "paused"
	workflowStatusPending   = "pending"
	workflowStatusProgress  = "progress"
	workflowStatusRunning   = "running"
	workflowStatusStart     = "start"
	workflowStatusStopped   = "stopped"

	workflowErrorBadProgress       = "bad_workflow_progress"
	workflowErrorMissingIdentifier = "missing_identifier"
	workflowErrorUnknownTask       = "unknown_task"
	workflowOutcomeDropped         = "dropped"

	keyActiveAgents     = "activeAgents"
	keyAgents           = "agents"
	keyCompletedAgents  = "completedAgents"
	keyEndTime          = "end_time"
	keyFailedAgents     = "failedAgents"
	keyIndex            = "index"
	keyLastToolName     = "last_tool_name"
	keyLaunchStatus     = "launchStatus"
	keyLogAvailable     = "logAvailable"
	keyOutputAvailable  = "outputAvailable"
	keyOutputFile       = "output_file"
	keyPatch            = "patch"
	keyPhases           = "phases"
	keyRunID            = "runId"
	keyScriptAvailable  = "scriptAvailable"
	keyScriptPath       = "scriptPath"
	keySkipTranscript   = "skip_transcript"
	keyState            = "state"
	keySubagentType     = "subagent_type"
	keySummary          = "summary"
	keyTaskID           = "task_id"
	keyTaskIDMeta       = "taskId"
	keyTaskType         = "task_type"
	keyTranscriptDir    = "transcriptDir"
	keyWorkflow         = "workflow"
	keyWorkflowName     = "workflowName"
	keyWorkflowNameRaw  = "workflow_name"
	keyWorkflowProgress = "workflow_progress"
)

// WorkflowFrameError describes a dropped malformed workflow frame without
// carrying user-controlled task details.
type WorkflowFrameError struct {
	Outcome      string
	ErrorType    string
	FrameSubtype string
}

// WorkflowTracker carries per-prompt Workflow correlation and accumulated
// state. It is intentionally not safe for concurrent use; prompt mapping is
// single-threaded.
type WorkflowTracker struct {
	taskToTool map[string]string
	states     map[string]*workflowState
	errors     []WorkflowFrameError
}

type workflowState struct {
	taskID        string
	toolUseID     string
	runID         string
	workflowName  string
	description   string
	summary       string
	status        string
	launchStatus  string
	outputFile    string
	transcriptDir string
	scriptPath    string
	usage         map[string]any
	top           map[string]any
	phases        map[int]map[string]any
	agents        map[int]map[string]any
}

// NewWorkflowTracker creates per-turn state for Workflow task updates.
func NewWorkflowTracker() *WorkflowTracker {
	return &WorkflowTracker{}
}

// DrainFrameErrors returns and clears mapper-side workflow frame errors.
func (t *WorkflowTracker) DrainFrameErrors() []WorkflowFrameError {
	if t == nil || len(t.errors) == 0 {
		return nil
	}

	errors := append([]WorkflowFrameError(nil), t.errors...)
	t.errors = nil

	return errors
}

// Tracked reports how many distinct Workflow tasks this tracker has correlated.
// It is a watermark rather than a flag, so a caller whose tracker outlives it can
// tell the work it correlated itself from the work it inherited.
func (t *WorkflowTracker) Tracked() int {
	if t == nil {
		return 0
	}

	return len(t.states)
}

// HasActive reports whether any tracked Workflow has started and has not
// reached a terminal status.
func (t *WorkflowTracker) HasActive() bool {
	if t == nil {
		return false
	}

	for _, state := range t.states {
		if state.active() {
			return true
		}
	}

	return false
}

func workflowSystemUpdates(msg *claude.SystemMessage, options ToolUpdateOptions) []acp.SessionUpdate {
	if msg == nil {
		return nil
	}

	tracker := options.Workflow
	if tracker == nil {
		return nil
	}

	switch msg.Subtype {
	case systemSubtypeTaskStarted:
		return tracker.taskStarted(msg.Raw, options.ParentToolUseID)
	case systemSubtypeTaskProgress:
		return tracker.taskProgress(msg.Raw, options.ParentToolUseID)
	case systemSubtypeTaskUpdated:
		return tracker.taskUpdated(msg.Raw, options.ParentToolUseID)
	case systemSubtypeTaskNotification:
		return tracker.taskNotification(msg.Raw, options.ParentToolUseID)
	default:
		return nil
	}
}

func workflowLaunchResult(raw map[string]any) map[string]any {
	result, _ := raw[keyToolUseResult].(map[string]any)

	return result
}

func workflowLaunchResultUpdates(
	block claude.ToolResultBlock,
	options ToolUpdateOptions,
	launch map[string]any,
) []acp.SessionUpdate {
	if block.IsError || !workflowLaunchStatusOwned(stringInput(launch, keyStatus)) {
		return nil
	}

	toolUse, ok := workflowToolUse(block.ToolUseID, options)
	if !ok {
		return nil
	}

	tracker := options.Workflow
	if tracker == nil {
		tracker = NewWorkflowTracker()
	}

	state := tracker.ensureState(stringInput(launch, "taskId"), block.ToolUseID)
	state.applyLaunch(launch)

	content, locations := toolResultContent(block, toolUse)
	update := acp.UpdateToolCall(
		acp.ToolCallId(block.ToolUseID),
		acp.WithUpdateStatus(acp.ToolCallStatusInProgress),
		acp.WithUpdateKind(acp.ToolKindThink),
		acp.WithUpdateContent(content),
		acp.WithUpdateLocations(locations),
		acp.WithUpdateRawOutput(block.Raw),
	)
	update.ToolCallUpdate.Meta = toolMeta(toolWorkflow, block.Raw)
	setWorkflowUpdateMeta(update.ToolCallUpdate.Meta, state.meta())

	return []acp.SessionUpdate{withParentToolUseID(update, options.ParentToolUseID)}
}

func workflowLaunchStatusOwned(status string) bool {
	return status == workflowLaunchAsync || status == workflowLaunchRemote
}

func workflowToolUse(toolUseID string, options ToolUpdateOptions) (claude.ToolUseBlock, bool) {
	toolUse, ok := options.ToolUses[toolUseID]
	if !ok || !strings.EqualFold(toolUse.Name, toolWorkflow) {
		return claude.ToolUseBlock{}, false
	}

	return toolUse, true
}

func (t *WorkflowTracker) taskStarted(raw map[string]any, parentToolUseID string) []acp.SessionUpdate {
	taskID := stringInput(raw, keyTaskID)
	toolUseID := stringInput(raw, keyToolUseID)

	if taskID == "" || toolUseID == "" {
		t.recordError(systemSubtypeTaskStarted, workflowErrorMissingIdentifier)

		return nil
	}

	state := t.ensureState(taskID, toolUseID)
	state.workflowName = firstNonEmptyString(state.workflowName, stringInput(raw, keyWorkflowNameRaw))
	state.description = firstNonEmptyString(state.description, stringInput(raw, keyDescription))
	state.applyTopFields(raw)

	title := firstNonEmptyString(state.workflowName, state.description, state.summary, toolWorkflow)
	content := workflowContent(state.description)
	update := workflowUpdate(toolUseID, state, parentToolUseID,
		acp.WithUpdateStatus(acp.ToolCallStatusInProgress),
		acp.WithUpdateKind(acp.ToolKindThink),
		acp.WithUpdateTitle(title),
		acp.WithUpdateContent(content),
	)

	return []acp.SessionUpdate{update}
}

func (t *WorkflowTracker) taskProgress(raw map[string]any, parentToolUseID string) []acp.SessionUpdate {
	taskID := stringInput(raw, keyTaskID)
	toolUseID := firstNonEmptyString(stringInput(raw, keyToolUseID), t.toolUseIDForTask(taskID))

	if taskID == "" || toolUseID == "" {
		t.recordError(systemSubtypeTaskProgress, workflowErrorMissingIdentifier)

		return nil
	}

	progress, ok := raw[keyWorkflowProgress].([]any)
	if !ok {
		t.recordError(systemSubtypeTaskProgress, workflowErrorBadProgress)

		return nil
	}

	state := t.ensureState(taskID, toolUseID)
	state.applyTopFields(raw)

	for _, value := range progress {
		entry, _ := value.(map[string]any)
		if entry == nil {
			t.recordError(systemSubtypeTaskProgress, workflowErrorBadProgress)

			continue
		}

		switch stringInput(entry, keyType) {
		case workflowProgressTypePhase:
			state.mergePhase(entry)
		case workflowProgressTypeAgent:
			state.mergeAgent(entry)
		default:
			t.recordError(systemSubtypeTaskProgress, workflowErrorBadProgress)
		}
	}

	return []acp.SessionUpdate{workflowUpdate(toolUseID, state, parentToolUseID,
		acp.WithUpdateStatus(acp.ToolCallStatusInProgress),
		acp.WithUpdateKind(acp.ToolKindThink),
	)}
}

func (t *WorkflowTracker) taskUpdated(raw map[string]any, parentToolUseID string) []acp.SessionUpdate {
	taskID := stringInput(raw, keyTaskID)
	toolUseID := firstNonEmptyString(stringInput(raw, keyToolUseID), t.toolUseIDForTask(taskID))

	if taskID == "" || toolUseID == "" {
		t.recordError(systemSubtypeTaskUpdated, workflowErrorUnknownTask)

		return nil
	}

	state := t.ensureState(taskID, toolUseID)
	state.applyTopFields(raw)

	patch, _ := raw[keyPatch].(map[string]any)
	if patch != nil {
		state.applyPatch(patch)
	}

	opts := []acp.ToolCallUpdateOpt{acp.WithUpdateKind(acp.ToolKindThink)}
	if status, ok := workflowACPStatus(state.status); ok {
		opts = append(opts, acp.WithUpdateStatus(status))
	}

	return []acp.SessionUpdate{workflowUpdate(toolUseID, state, parentToolUseID, opts...)}
}

func (t *WorkflowTracker) taskNotification(raw map[string]any, parentToolUseID string) []acp.SessionUpdate {
	taskID := stringInput(raw, keyTaskID)
	toolUseID := firstNonEmptyString(stringInput(raw, keyToolUseID), t.toolUseIDForTask(taskID))

	if taskID == "" || toolUseID == "" {
		t.recordError(systemSubtypeTaskNotification, workflowErrorMissingIdentifier)

		return nil
	}

	state := t.ensureState(taskID, toolUseID)
	state.applyNotification(raw)

	opts := []acp.ToolCallUpdateOpt{acp.WithUpdateKind(acp.ToolKindThink)}
	if status, ok := workflowACPStatus(state.status); ok {
		opts = append(opts, acp.WithUpdateStatus(status))
	}

	if state.summary != "" {
		opts = append(opts, acp.WithUpdateContent(workflowContent(state.summary)))
	}

	return []acp.SessionUpdate{workflowUpdate(toolUseID, state, parentToolUseID, opts...)}
}

func (t *WorkflowTracker) ensureState(taskID string, toolUseID string) *workflowState {
	if t.states == nil {
		t.states = map[string]*workflowState{}
	}

	key := firstNonEmptyString(taskID, toolUseID)
	state := t.states[key]

	if state == nil {
		state = &workflowState{taskID: taskID, toolUseID: toolUseID}
		t.states[key] = state
	}

	if taskID != "" {
		state.taskID = taskID
	}

	if toolUseID != "" {
		state.toolUseID = toolUseID
	}

	if taskID != "" && toolUseID != "" {
		if t.taskToTool == nil {
			t.taskToTool = map[string]string{}
		}

		t.taskToTool[taskID] = toolUseID
	}

	return state
}

func (t *WorkflowTracker) toolUseIDForTask(taskID string) string {
	if t == nil || taskID == "" {
		return ""
	}

	return t.taskToTool[taskID]
}

func (t *WorkflowTracker) recordError(subtype string, errorType string) {
	if t == nil {
		return
	}

	t.errors = append(t.errors, WorkflowFrameError{
		Outcome:      workflowOutcomeDropped,
		ErrorType:    errorType,
		FrameSubtype: subtype,
	})
}

func (s *workflowState) applyLaunch(raw map[string]any) {
	if raw == nil {
		return
	}

	s.taskID = firstNonEmptyString(s.taskID, stringInput(raw, "taskId"))
	s.runID = firstNonEmptyString(s.runID, stringInput(raw, keyRunID))
	s.summary = firstNonEmptyString(s.summary, stringInput(raw, keySummary))
	s.launchStatus = firstNonEmptyString(s.launchStatus, stringInput(raw, keyStatus))
	s.transcriptDir = firstNonEmptyString(s.transcriptDir, stringInput(raw, keyTranscriptDir))
	s.scriptPath = firstNonEmptyString(s.scriptPath, stringInput(raw, keyScriptPath))
}

func (s *workflowState) applyTopFields(raw map[string]any) {
	if raw == nil {
		return
	}

	s.taskID = firstNonEmptyString(s.taskID, stringInput(raw, keyTaskID))
	s.workflowName = firstNonEmptyString(s.workflowName, stringInput(raw, keyWorkflowNameRaw))
	s.description = firstNonEmptyString(s.description, stringInput(raw, keyDescription))

	for _, key := range []string{keyLastToolName, keySubagentType, keySkipTranscript, keyTaskType} {
		if value, ok := raw[key]; ok {
			if s.top == nil {
				s.top = map[string]any{}
			}

			s.top[key] = cloneWorkflowValue(value)
		}
	}
}

func (s *workflowState) applyPatch(raw map[string]any) {
	if raw == nil {
		return
	}

	if value, ok := raw[keyStatus]; ok {
		s.status, _ = value.(string)
	}

	if value, ok := raw[keyEndTime]; ok {
		if s.top == nil {
			s.top = map[string]any{}
		}

		s.top[keyEndTime] = cloneWorkflowValue(value)
	}
}

func (s *workflowState) applyNotification(raw map[string]any) {
	s.applyTopFields(raw)

	if value, ok := raw[keyStatus]; ok {
		s.status, _ = value.(string)
	}

	s.summary = firstNonEmptyString(s.summary, stringInput(raw, keySummary))
	s.outputFile = firstNonEmptyString(s.outputFile, stringInput(raw, keyOutputFile))

	if usage, _ := raw[keyUsage].(map[string]any); usage != nil {
		s.usage = cloneWorkflowMap(usage)
	}
}

func (s *workflowState) mergePhase(raw map[string]any) {
	index, ok := intInput(raw, keyIndex)
	if !ok {
		return
	}

	if s.phases == nil {
		s.phases = map[int]map[string]any{}
	}

	s.phases[index] = mergeWorkflowMap(s.phases[index], raw)
}

func (s *workflowState) mergeAgent(raw map[string]any) {
	index, ok := intInput(raw, keyIndex)
	if !ok {
		return
	}

	if s.agents == nil {
		s.agents = map[int]map[string]any{}
	}

	s.agents[index] = mergeWorkflowMap(s.agents[index], raw)
}

func (s *workflowState) meta() map[string]any {
	meta := map[string]any{}

	if s.taskID != "" {
		meta[keyTaskIDMeta] = s.taskID
	}

	if s.runID != "" {
		meta[keyRunID] = s.runID
	}

	if s.workflowName != "" {
		meta[keyWorkflowName] = s.workflowName
	}

	if s.description != "" {
		meta[keyDescription] = s.description
	}

	if s.summary != "" {
		meta[keySummary] = s.summary
	}

	if s.status != "" {
		meta[keyStatus] = s.status
	}

	if s.launchStatus != "" {
		meta[keyLaunchStatus] = s.launchStatus
	}

	if s.outputFile != "" {
		meta[keyOutputFile] = s.outputFile
		meta[keyOutputAvailable] = true
	}

	if s.transcriptDir != "" {
		meta[keyTranscriptDir] = s.transcriptDir
		meta[keyLogAvailable] = true
	}

	if s.scriptPath != "" {
		meta[keyScriptPath] = s.scriptPath
		meta[keyScriptAvailable] = true
	}

	if len(s.usage) > 0 {
		meta[keyUsage] = cloneWorkflowMap(s.usage)
	}

	for key, value := range s.top {
		meta[key] = cloneWorkflowValue(value)
	}

	phases := sortedWorkflowMaps(s.phases)
	if len(phases) > 0 {
		meta[keyPhases] = phases
	}

	agents := sortedWorkflowMaps(s.agents)
	if len(agents) > 0 {
		meta[keyAgents] = agents
		active, completed, failed := workflowAgentCounts(agents)
		meta[keyActiveAgents] = active
		meta[keyCompletedAgents] = completed
		meta[keyFailedAgents] = failed
	}

	return meta
}

func (s *workflowState) active() bool {
	if s == nil || s.toolUseID == "" {
		return false
	}

	switch s.status {
	case workflowStatusCompleted, workflowStatusFailed, workflowStatusStopped, workflowStatusKilled:
		return false
	default:
		return true
	}
}

func workflowUpdate(
	toolUseID string,
	state *workflowState,
	parentToolUseID string,
	opts ...acp.ToolCallUpdateOpt,
) acp.SessionUpdate {
	update := acp.UpdateToolCall(acp.ToolCallId(toolUseID), opts...)
	update.ToolCallUpdate.Meta = toolMeta(toolWorkflow, nil)
	setWorkflowUpdateMeta(update.ToolCallUpdate.Meta, state.meta())

	return withParentToolUseID(update, parentToolUseID)
}

func setWorkflowUpdateMeta(meta map[string]any, workflow map[string]any) {
	if len(workflow) == 0 {
		return
	}

	claudeMeta, _ := meta[keyClaude].(map[string]any)
	if claudeMeta == nil {
		claudeMeta = map[string]any{}
		meta[keyClaude] = claudeMeta
	}

	claudeMeta[keyWorkflow] = workflow
}

func workflowContent(text string) []acp.ToolCallContent {
	if strings.TrimSpace(text) == "" {
		return nil
	}

	return []acp.ToolCallContent{acp.ToolContent(acp.TextBlock(text))}
}

func workflowACPStatus(status string) (acp.ToolCallStatus, bool) {
	switch status {
	case workflowStatusCompleted:
		return acp.ToolCallStatusCompleted, true
	case workflowStatusFailed, workflowStatusStopped, workflowStatusKilled:
		return acp.ToolCallStatusFailed, true
	case workflowStatusPending, workflowStatusRunning, workflowStatusPaused:
		return acp.ToolCallStatusInProgress, true
	default:
		return "", false
	}
}

func workflowAgentCounts(agents []map[string]any) (int, int, int) {
	var active, completed, failed int

	for _, agent := range agents {
		switch stringInput(agent, keyState) {
		case workflowStatusStart, workflowStatusProgress:
			active++
		case workflowStatusDone:
			completed++
		case workflowStatusError:
			failed++
		}
	}

	return active, completed, failed
}

func sortedWorkflowMaps(values map[int]map[string]any) []map[string]any {
	if len(values) == 0 {
		return nil
	}

	indexes := make([]int, 0, len(values))
	for index := range values {
		indexes = append(indexes, index)
	}

	sort.Ints(indexes)

	out := make([]map[string]any, 0, len(indexes))
	for _, index := range indexes {
		out = append(out, cloneWorkflowMap(values[index]))
	}

	return out
}

func mergeWorkflowMap(previous map[string]any, next map[string]any) map[string]any {
	merged := cloneWorkflowMap(previous)
	if merged == nil {
		merged = map[string]any{}
	}

	for key, value := range next {
		merged[key] = cloneWorkflowValue(value)
	}

	return merged
}

func cloneWorkflowMap(raw map[string]any) map[string]any {
	if raw == nil {
		return nil
	}

	cloned := make(map[string]any, len(raw))
	for key, value := range raw {
		cloned[key] = cloneWorkflowValue(value)
	}

	return cloned
}

func cloneWorkflowValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneWorkflowMap(typed)
	case []any:
		cloned := make([]any, len(typed))
		for i, item := range typed {
			cloned[i] = cloneWorkflowValue(item)
		}

		return cloned
	case []string:
		return append([]string(nil), typed...)
	default:
		return typed
	}
}
