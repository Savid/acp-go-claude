package claudeacp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/savid/acp-go-claude/internal/observer"
)

const (
	ClaudeGoalStatusActive    = "active"
	ClaudeGoalStatusBlocked   = "blocked"
	ClaudeGoalStatusCompleted = "completed"

	ClaudeGoalSourceClient = "client"
	ClaudeGoalSourceClaude = "claude"

	claudeGoalsCapabilityKey = "goals"
	claudeGoalMetaKey        = "goal"

	claudeSessionSetGoalMethod = "_claude/session/setGoal"

	goalFieldCompletedAt         = "completedAt"
	goalFieldCompletionCondition = "completionCondition"
	goalFieldCreatedAt           = "createdAt"
	goalFieldGoalID              = "goalId"
	goalFieldObjective           = "objective"
	goalFieldReason              = "reason"
	goalFieldReasonCode          = "reasonCode"
	goalFieldSource              = "source"
	goalFieldStatus              = "status"
	goalFieldUpdatedAt           = "updatedAt"

	goalReasonNativeFailed     = "native_failed"
	goalReasonStopHookOverride = "stop_hook_override"

	goalNativeTypeGoalStatus = "goal_status"
	goalNativeClearCommand   = "clear"

	// Version-probed against claude 2.1.150. These strings come from the
	// undocumented local-command output for /goal clear; live tests pin the
	// Claude version and fail loudly if the wording drifts.
	goalNativeClearOK    = "Goal cleared:"
	goalNativeClearEmpty = "No goal set"

	goalCapabilityStateKey     = "state"
	goalTranscriptTimestampKey = "timestamp"
	goalTranscriptTypeAttach   = "attachment"
	goalTranscriptTypeSystem   = "system"
	goalTranscriptTypeUser     = "user"

	maxGoalObjectiveBytes = 4096
	maxGoalTextBytes      = 4096
	maxGoalSummaryRunes   = 256

	sessionMirrorLateTimeout = 2 * time.Second
	sessionMirrorStopTimeout = 2 * time.Second

	goalMirrorErrorCanceled    = "canceled"
	goalMirrorErrorTimeout     = "timeout"
	goalMirrorErrorStoreAppend = "store_append"
	goalMirrorErrorAgentClosed = "agent_closed"
	goalMirrorErrorConnection  = "connection"
	goalMirrorErrorOther       = "other"
)

// ClaudeGoal is the Claude-specific session goal metadata shape used in
// _meta.claude.goal and _claude/session/setGoal requests.
type ClaudeGoal struct {
	Objective           string `json:"objective"`
	CompletionCondition string `json:"completionCondition,omitempty"`
	Status              string `json:"status,omitempty"`
	CreatedAt           string `json:"createdAt,omitempty"`
	UpdatedAt           string `json:"updatedAt,omitempty"`
	CompletedAt         string `json:"completedAt,omitempty"`
	Reason              string `json:"reason,omitempty"`
	ReasonCode          string `json:"reasonCode,omitempty"`
	GoalID              string `json:"goalId,omitempty"`
	Source              string `json:"source,omitempty"`
}

type goalMetaInput struct {
	present bool
	clear   bool
	goal    ClaudeGoal
}

type goalClearCandidate struct {
	uuid string
}

func parseGoalFromMeta(meta map[string]any) (goalMetaInput, error) {
	claudeMeta, _ := meta[claudeMetaKey].(map[string]any)
	if claudeMeta == nil {
		return goalMetaInput{}, nil
	}

	raw, ok := claudeMeta[claudeGoalMetaKey]
	if !ok {
		return goalMetaInput{}, nil
	}

	return parseGoalValue(raw)
}

func parseGoalValue(raw any) (goalMetaInput, error) {
	if raw == nil {
		return goalMetaInput{present: true, clear: true}, nil
	}

	rawMap, _ := raw.(map[string]any)
	if rawMap == nil {
		return goalMetaInput{}, fmt.Errorf("_meta.%s.%s must be null or an object", claudeMetaKey, claudeGoalMetaKey)
	}

	goal, err := parseClientGoalObject(rawMap)
	if err != nil {
		return goalMetaInput{}, err
	}

	return goalMetaInput{present: true, goal: goal}, nil
}

func parseClientGoalObject(raw map[string]any) (ClaudeGoal, error) {
	for key := range raw {
		switch key {
		case goalFieldObjective,
			goalFieldCompletionCondition,
			goalFieldStatus,
			goalFieldCreatedAt,
			goalFieldUpdatedAt,
			goalFieldCompletedAt,
			goalFieldReason,
			goalFieldReasonCode,
			goalFieldGoalID,
			goalFieldSource:
		default:
			return ClaudeGoal{}, fmt.Errorf("_meta.%s.%s.%s is not supported", claudeMetaKey, claudeGoalMetaKey, key)
		}
	}

	objective, err := requiredGoalString(raw, goalFieldObjective, maxGoalObjectiveBytes)
	if err != nil {
		return ClaudeGoal{}, err
	}

	condition, err := optionalGoalString(raw, goalFieldCompletionCondition, maxGoalTextBytes)
	if err != nil {
		return ClaudeGoal{}, err
	}

	reason, err := optionalGoalString(raw, goalFieldReason, maxGoalTextBytes)
	if err != nil {
		return ClaudeGoal{}, err
	}

	if value, ok := raw[goalFieldReasonCode]; ok && value != nil {
		return ClaudeGoal{}, fmt.Errorf("_meta.%s.%s.%s is adapter-owned", claudeMetaKey, claudeGoalMetaKey, goalFieldReasonCode)
	}

	status, err := optionalGoalStatus(raw)
	if err != nil {
		return ClaudeGoal{}, err
	}

	if status == "" {
		status = ClaudeGoalStatusActive
	}

	if status != ClaudeGoalStatusActive && status != ClaudeGoalStatusBlocked {
		return ClaudeGoal{}, fmt.Errorf("_meta.%s.%s.%s must be %q or %q", claudeMetaKey, claudeGoalMetaKey, goalFieldStatus, ClaudeGoalStatusActive, ClaudeGoalStatusBlocked)
	}

	return ClaudeGoal{
		Objective:           objective,
		CompletionCondition: condition,
		Status:              status,
		Reason:              reason,
	}, nil
}

func requiredGoalString(raw map[string]any, key string, limit int) (string, error) {
	value, ok := raw[key]
	if !ok || value == nil {
		return "", fmt.Errorf("_meta.%s.%s.%s is required", claudeMetaKey, claudeGoalMetaKey, key)
	}

	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("_meta.%s.%s.%s must be a string", claudeMetaKey, claudeGoalMetaKey, key)
	}

	text = strings.TrimSpace(text)
	if text == "" {
		return "", fmt.Errorf("_meta.%s.%s.%s is required", claudeMetaKey, claudeGoalMetaKey, key)
	}

	if len(text) > limit {
		return "", fmt.Errorf("_meta.%s.%s.%s must be at most %d bytes", claudeMetaKey, claudeGoalMetaKey, key, limit)
	}

	return text, nil
}

func optionalGoalString(raw map[string]any, key string, limit int) (string, error) {
	value, ok := raw[key]
	if !ok || value == nil {
		return "", nil
	}

	text, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("_meta.%s.%s.%s must be a string or null", claudeMetaKey, claudeGoalMetaKey, key)
	}

	text = strings.TrimSpace(text)
	if text == "" {
		return "", nil
	}

	if len(text) > limit {
		return "", fmt.Errorf("_meta.%s.%s.%s must be at most %d bytes", claudeMetaKey, claudeGoalMetaKey, key, limit)
	}

	return text, nil
}

func optionalGoalStatus(raw map[string]any) (string, error) {
	value, ok := raw[goalFieldStatus]
	if !ok || value == nil {
		return "", nil
	}

	status, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("_meta.%s.%s.%s must be a string or null", claudeMetaKey, claudeGoalMetaKey, goalFieldStatus)
	}

	return strings.TrimSpace(status), nil
}

func (a *Agent) setClaudeGoal(ctx context.Context, params json.RawMessage) (resp any, err error) {
	ctx, finish := a.observe.StartACP(ctx, nil, claudeSessionSetGoalMethod)
	defer func() { finish(observer.ACPResult{Err: err}) }()

	var request struct {
		SessionID acp.SessionId   `json:"sessionId"`
		Goal      json.RawMessage `json:"goal"`
	}
	if unmarshalErr := json.Unmarshal(params, &request); unmarshalErr != nil {
		return nil, acp.NewInvalidParams(map[string]any{jsonFieldError: unmarshalErr.Error()})
	}

	if request.SessionID == "" {
		return nil, acp.NewInvalidParams(map[string]any{acpFieldSessionID: request.SessionID})
	}

	if request.Goal == nil {
		return nil, acp.NewInvalidParams(map[string]any{jsonFieldError: "goal is required"})
	}

	var raw any

	_ = json.Unmarshal(request.Goal, &raw)

	input, err := parseGoalValue(raw)
	if err != nil {
		return nil, acp.NewInvalidParams(map[string]any{jsonFieldError: err.Error()})
	}

	session, err := a.session(request.SessionID)
	if err != nil {
		return nil, err
	}

	if err := session.applyClientGoalInput(ctx, input, true); err != nil {
		return nil, err
	}

	return map[string]any{claudeGoalMetaKey: session.goalMetaValue()}, nil
}

func (s *Session) applyClientGoalInput(ctx context.Context, input goalMetaInput, emit bool) error {
	changed := s.applyStoredClientGoalInput(input)
	if emit && changed {
		return s.emitGoalInfoUpdate(ctx)
	}

	return nil
}

func (s *Session) applyStoredClientGoalInput(input goalMetaInput) bool {
	if !input.present {
		return false
	}

	if input.clear {
		s.setGoalSnapshot(nil, true)

		return true
	}

	now := time.Now().UTC().Format(time.RFC3339Nano)
	goal := input.goal
	goal.CreatedAt = now
	goal.UpdatedAt = now
	goal.CompletedAt = ""
	goal.GoalID = ""
	goal.Source = ClaudeGoalSourceClient
	goal.ReasonCode = ""

	if goal.Status == "" {
		goal.Status = ClaudeGoalStatusActive
	}

	s.setGoalSnapshot(&goal, true)

	return true
}

func (s *Session) setGoalSnapshot(goal *ClaudeGoal, bumpRevision bool) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if goal == nil {
		s.goal = nil
	} else {
		cloned := *goal
		s.goal = &cloned
	}

	if bumpRevision {
		s.goalRevision++
	}
}

func (s *Session) goalSnapshot() (*ClaudeGoal, int64) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.goal == nil {
		return nil, s.goalRevision
	}

	goal := *s.goal

	return &goal, s.goalRevision
}

func (s *Session) goalMetaValue() any {
	goal, _ := s.goalSnapshot()
	if goal == nil {
		return nil
	}

	return canonicalGoalMeta(*goal)
}

func (s *Session) goalSummaryMetaValue() any {
	goal, _ := s.goalSnapshot()
	if goal == nil {
		return nil
	}

	objective := goal.Objective
	if utf8.RuneCountInString(objective) > maxGoalSummaryRunes {
		runes := []rune(objective)
		objective = strings.TrimSpace(string(runes[:maxGoalSummaryRunes-3])) + "..."
	}

	return map[string]any{
		goalFieldObjective: objective,
		goalFieldStatus:    goal.Status,
	}
}

func canonicalGoalMeta(goal ClaudeGoal) map[string]any {
	return map[string]any{
		goalFieldObjective:           goal.Objective,
		goalFieldCompletionCondition: nullableString(goal.CompletionCondition),
		goalFieldStatus:              goal.Status,
		goalFieldCreatedAt:           nullableString(goal.CreatedAt),
		goalFieldUpdatedAt:           nullableString(goal.UpdatedAt),
		goalFieldCompletedAt:         nullableString(goal.CompletedAt),
		goalFieldReason:              nullableString(goal.Reason),
		goalFieldReasonCode:          nullableString(goal.ReasonCode),
		goalFieldGoalID:              nullableString(goal.GoalID),
		goalFieldSource:              nullableString(goal.Source),
	}
}

func nullableString(value string) any {
	if value == "" {
		return nil
	}

	return value
}

func (s *Session) emitGoalInfoUpdate(ctx context.Context) error {
	return s.emitOptionalUpdates(ctx, []acp.SessionUpdate{{
		SessionInfoUpdate: &acp.SessionSessionInfoUpdate{
			Meta: map[string]any{
				claudeMetaKey: map[string]any{
					claudeGoalMetaKey: s.goalMetaValue(),
				},
			},
		},
	}})
}

func (s *Session) applyTranscriptMirrorGoals(ctx context.Context, frame *claude.TranscriptMirrorMessage, emit bool) error {
	if frame == nil || len(frame.Entries) == 0 {
		return nil
	}

	rawEntries := transcriptRawEntries(frame.Entries)
	if len(rawEntries) == 0 {
		return nil
	}

	changed := false
	clearCommands := make(map[string]goalClearCandidate)
	override := transcriptGoalStopHookOverride(rawEntries)

	for _, raw := range rawEntries {
		if command, ok := transcriptGoalClearCommand(raw); ok {
			clearCommands[command.uuid] = command

			continue
		}

		switch transcriptGoalClearOutput(raw, clearCommands) {
		case goalClearOutputConfirmed:
			changed = s.applyNativeGoalClear(raw) || changed

			continue
		case goalClearOutputUnmatched:
			s.logNativeGoalClearUnmatched(ctx)
		}

		if goal, ok := nativeGoalFromTranscriptEntry(raw, override); ok {
			changed = s.applyNativeGoal(goal) || changed
		}
	}

	if changed && emit {
		return s.emitGoalInfoUpdate(ctx)
	}

	return nil
}

func transcriptRawEntries(entries []json.RawMessage) []map[string]any {
	rawEntries := make([]map[string]any, 0, len(entries))
	for _, entry := range entries {
		raw := map[string]any{}
		if err := json.Unmarshal(entry, &raw); err != nil {
			continue
		}

		rawEntries = append(rawEntries, raw)
	}

	return rawEntries
}

func nativeGoalFromTranscriptEntry(raw map[string]any, override bool) (ClaudeGoal, bool) {
	if rawString(raw, jsonFieldType) != goalTranscriptTypeAttach {
		return ClaudeGoal{}, false
	}

	attachment := rawMap(raw, "attachment")
	if rawString(attachment, jsonFieldType) != goalNativeTypeGoalStatus {
		return ClaudeGoal{}, false
	}

	condition := strings.TrimSpace(rawString(attachment, "condition"))
	if condition == "" {
		return ClaudeGoal{}, false
	}

	updatedAt := transcriptEntryTimestamp(raw)
	status := ClaudeGoalStatusActive
	completedAt := ""
	reasonCode := ""

	met := rawBool(attachment, "met")
	failed := rawBool(attachment, "failed")

	switch {
	case failed:
		status = ClaudeGoalStatusBlocked
		reasonCode = goalReasonNativeFailed
	case met && override:
		status = ClaudeGoalStatusBlocked
		reasonCode = goalReasonStopHookOverride
	case met:
		status = ClaudeGoalStatusCompleted
		completedAt = updatedAt
	}

	return ClaudeGoal{
		Objective:           condition,
		CompletionCondition: condition,
		Status:              status,
		CreatedAt:           updatedAt,
		UpdatedAt:           updatedAt,
		CompletedAt:         completedAt,
		Reason:              strings.TrimSpace(rawString(attachment, "reason")),
		ReasonCode:          reasonCode,
		GoalID:              rawString(raw, "uuid"),
		Source:              ClaudeGoalSourceClaude,
	}, true
}

func (s *Session) applyNativeGoal(next ClaudeGoal) bool {
	if next.UpdatedAt == "" {
		next.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
	}

	if next.CreatedAt == "" {
		next.CreatedAt = next.UpdatedAt
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.goal != nil && s.goal.Source == ClaudeGoalSourceClient && !goalTimeAfter(next.UpdatedAt, s.goal.UpdatedAt) {
		return false
	}

	if s.goal != nil && s.goal.Source == ClaudeGoalSourceClaude && s.goal.Objective == next.Objective {
		if next.GoalID == "" {
			next.GoalID = s.goal.GoalID
		}

		if s.goal.CreatedAt != "" {
			next.CreatedAt = s.goal.CreatedAt
		}
	}

	s.goal = &next
	s.goalRevision++

	return true
}

func (s *Session) applyNativeGoalClear(raw map[string]any) bool {
	updatedAt := transcriptEntryTimestamp(raw)

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.goal != nil && s.goal.Source == ClaudeGoalSourceClient && !goalTimeAfter(updatedAt, s.goal.UpdatedAt) {
		return false
	}

	s.goal = nil
	s.goalRevision++

	return true
}

func goalTimeAfter(candidate string, current string) bool {
	if current == "" {
		return true
	}

	candidateTime, candidateErr := time.Parse(time.RFC3339Nano, candidate)
	currentTime, currentErr := time.Parse(time.RFC3339Nano, current)

	if candidateErr != nil || currentErr != nil {
		return candidate != current
	}

	return candidateTime.After(currentTime)
}

func transcriptEntryTimestamp(raw map[string]any) string {
	value := rawString(raw, goalTranscriptTimestampKey)
	if value == "" {
		return time.Now().UTC().Format(time.RFC3339Nano)
	}

	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Now().UTC().Format(time.RFC3339Nano)
	}

	return parsed.UTC().Format(time.RFC3339Nano)
}

func transcriptGoalClearCommand(raw map[string]any) (goalClearCandidate, bool) {
	if rawString(raw, jsonFieldType) != goalTranscriptTypeUser {
		return goalClearCandidate{}, false
	}

	text := transcriptEntryText(raw)
	if !isGoalClearText(text) {
		return goalClearCandidate{}, false
	}

	uuid := rawString(raw, "uuid")
	if uuid == "" {
		return goalClearCandidate{}, false
	}

	return goalClearCandidate{uuid: uuid}, true
}

type goalClearOutputMatch int

const (
	goalClearOutputNone goalClearOutputMatch = iota
	goalClearOutputConfirmed
	goalClearOutputUnmatched
)

func transcriptGoalClearOutput(raw map[string]any, commands map[string]goalClearCandidate) goalClearOutputMatch {
	if len(commands) == 0 {
		return goalClearOutputNone
	}

	switch rawString(raw, jsonFieldType) {
	case goalTranscriptTypeSystem:
		if rawString(raw, jsonFieldSubtype) != systemSubtypeLocalCommand && rawString(raw, jsonFieldSubtype) != systemSubtypeLocalCommandOutput {
			return goalClearOutputNone
		}
	default:
		return goalClearOutputNone
	}

	parent := rawString(raw, "parentUuid")
	if _, ok := commands[parent]; !ok {
		return goalClearOutputNone
	}

	if goalClearConfirmation(transcriptEntryText(raw)) {
		return goalClearOutputConfirmed
	}

	return goalClearOutputUnmatched
}

func transcriptEntryText(raw map[string]any) string {
	if text := rawString(raw, systemContent); text != "" {
		return text
	}

	message := rawMap(raw, jsonFieldMessage)
	switch content := message[systemContent].(type) {
	case string:
		return content
	case []any:
		var parts []string

		for _, item := range content {
			block, _ := item.(map[string]any)
			if text := rawString(block, jsonFieldText); text != "" {
				parts = append(parts, text)
			}
		}

		return strings.Join(parts, "\n")
	default:
		return ""
	}
}

func isGoalClearText(text string) bool {
	text = strings.TrimSpace(strings.ReplaceAll(text, "\x00", ""))
	if args, ok := strings.CutPrefix(text, "/goal"); ok {
		args = strings.TrimSpace(args)

		return args == goalNativeClearCommand
	}

	return strings.Contains(text, "<command-name>/goal</command-name>") &&
		strings.Contains(text, "<command-args>clear</command-args>")
}

func goalClearConfirmation(text string) bool {
	return strings.Contains(text, goalNativeClearOK) || strings.Contains(text, goalNativeClearEmpty)
}

func transcriptGoalStopHookOverride(entries []map[string]any) bool {
	for _, raw := range entries {
		text := strings.ToLower(transcriptEntryText(raw))
		if strings.Contains(text, "stop hook") && (strings.Contains(text, "override") || strings.Contains(text, "block cap")) {
			return true
		}
	}

	return false
}

func (s *Session) applyLocalGoalClearResult(ctx context.Context, result string, updatedAt string) error {
	if !goalClearConfirmation(result) {
		return nil
	}

	if !s.applyNativeGoalClear(map[string]any{goalTranscriptTimestampKey: updatedAt}) {
		return nil
	}

	return s.emitGoalInfoUpdate(ctx)
}

func goalClearSlashCommand(prompt []acp.ContentBlock) bool {
	return isGoalClearText(firstPromptText(prompt))
}

func scanTranscriptGoalFile(ctx context.Context, path string) (*ClaudeGoal, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()

	return scanTranscriptGoalReader(ctx, file)
}

func scanTranscriptGoalReader(ctx context.Context, reader io.Reader) (*ClaudeGoal, error) {
	accumulator := goalAccumulator{}
	entries := []json.RawMessage{}
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 0, 64*1024), 10*1024*1024)

	for scanner.Scan() {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 || line[0] != '{' {
			continue
		}

		entries = append(entries, json.RawMessage(append([]byte(nil), line...)))
	}

	if err := scanner.Err(); err != nil {
		return nil, err
	}

	if len(entries) > 0 {
		accumulator.applyTranscriptEntries(entries)
	}

	return accumulator.snapshot(), nil
}

func (s *Session) applyReplayGoalSnapshot(ctx context.Context, path string) (bool, error) {
	goal, err := scanTranscriptGoalFile(ctx, path)
	if err != nil {
		return false, err
	}

	if goal == nil {
		return false, nil
	}

	if !s.applyNativeGoal(*goal) {
		return false, nil
	}

	return true, nil
}

type goalAccumulator struct {
	goal *ClaudeGoal
}

func (a *goalAccumulator) applyTranscriptEntries(entries []json.RawMessage) {
	rawEntries := transcriptRawEntries(entries)
	if len(rawEntries) == 0 {
		return
	}

	clearCommands := make(map[string]goalClearCandidate)
	override := transcriptGoalStopHookOverride(rawEntries)

	for _, raw := range rawEntries {
		if command, ok := transcriptGoalClearCommand(raw); ok {
			clearCommands[command.uuid] = command

			continue
		}

		if transcriptGoalClearOutput(raw, clearCommands) == goalClearOutputConfirmed {
			a.goal = nil

			continue
		}

		if goal, ok := nativeGoalFromTranscriptEntry(raw, override); ok {
			if a.goal != nil && a.goal.Source == ClaudeGoalSourceClaude && a.goal.Objective == goal.Objective {
				if goal.GoalID == "" {
					goal.GoalID = a.goal.GoalID
				}

				if a.goal.CreatedAt != "" {
					goal.CreatedAt = a.goal.CreatedAt
				}
			}

			a.goal = &goal
		}
	}
}

func (a *goalAccumulator) snapshot() *ClaudeGoal {
	if a.goal == nil {
		return nil
	}

	goal := *a.goal

	return &goal
}

func (s *Session) startLateMirrorProcessor(ctx context.Context) {
	if s.client == nil || sessionMirrorLateTimeout <= 0 {
		return
	}

	s.stopLateMirrorProcessor(ctx)

	lateCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), sessionMirrorLateTimeout)
	done := make(chan struct{})

	s.mu.Lock()
	s.lateMirrorCancel = cancel
	s.lateMirrorDone = done
	s.mu.Unlock()

	// Between turns Claude is expected to emit mirror frames only. Raw-event
	// subscribers still see any unexpected non-mirror frame; normal prompt
	// processing is intentionally not run here, and the next prompt stops this
	// processor before it starts reading.
	go func() {
		defer recoverAgentGoroutine(ctx, s.agentLogger(), "late session mirror goal processor")
		defer s.clearLateMirrorProcessor(done)
		defer cancel()
		defer close(done)

		for {
			if err := lateCtx.Err(); err != nil {
				return
			}

			msg, err := s.client.Receive(lateCtx)
			if err != nil {
				return
			}

			if err := s.emitRawClaudeMessage(lateCtx, msg); err != nil {
				s.logGoalMirrorError(lateCtx, "emit_raw", err)

				return
			}

			if _, err := s.handleSessionMirror(lateCtx, msg); err != nil {
				s.logGoalMirrorError(lateCtx, "process_mirror", err)

				return
			}
		}
	}()
}

func (s *Session) stopLateMirrorProcessor(ctx context.Context) {
	s.mu.Lock()
	cancel := s.lateMirrorCancel
	done := s.lateMirrorDone
	s.lateMirrorCancel = nil
	s.lateMirrorDone = nil
	s.mu.Unlock()

	if cancel == nil {
		return
	}

	cancel()

	if done == nil {
		return
	}

	timeout := s.lateMirrorStopTimeout
	if timeout <= 0 {
		timeout = sessionMirrorStopTimeout
	}

	waitCtx, cancelWait := context.WithTimeout(context.WithoutCancel(ctx), timeout)
	defer cancelWait()

	select {
	case <-done:
	case <-waitCtx.Done():
		s.logGoalMirrorError(waitCtx, "stop_timeout", waitCtx.Err())
	}
}

func (s *Session) clearLateMirrorProcessor(done chan struct{}) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.lateMirrorDone != done {
		return
	}

	s.lateMirrorCancel = nil
	s.lateMirrorDone = nil
}

func (s *Session) agentLogger() *slog.Logger {
	if s.agent == nil {
		return nil
	}

	return s.agent.log
}

func rawMap(raw map[string]any, key string) map[string]any {
	value, _ := raw[key].(map[string]any)

	return value
}

func rawString(raw map[string]any, key string) string {
	value, _ := raw[key].(string)

	return value
}

func rawBool(raw map[string]any, key string) bool {
	value, _ := raw[key].(bool)

	return value
}

func (s *Session) logGoalMirrorError(ctx context.Context, msg string, err error) {
	if err == nil || s.agent == nil {
		return
	}

	s.agent.log.DebugContext(
		ctx,
		"late Claude mirror goal processing failed",
		slog.String("operation", msg),
		slog.String("error_kind", goalMirrorErrorKind(err)),
		slog.String(jsonFieldError, "redacted"),
	)
}

func (s *Session) logNativeGoalClearUnmatched(ctx context.Context) {
	if s.agent == nil {
		return
	}

	s.agent.log.DebugContext(ctx, "Claude native goal clear output did not match known confirmation", slog.String("operation", "native_clear_unmatched"))
}

func goalMirrorErrorKind(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return goalMirrorErrorCanceled
	case errors.Is(err, context.DeadlineExceeded):
		return goalMirrorErrorTimeout
	case errors.Is(err, errSessionMirrorAppend):
		return goalMirrorErrorStoreAppend
	case errors.Is(err, errAgentClosed):
		return goalMirrorErrorAgentClosed
	case errors.Is(err, errACPConnectionNotAttached):
		return goalMirrorErrorConnection
	default:
		return goalMirrorErrorOther
	}
}
