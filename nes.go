package claudeacp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
)

const (
	nesSuggestionTimeout = 90 * time.Second
	nesMaxInFlight       = 2

	nesDecisionAccepted = "accepted"
	nesDecisionRejected = "rejected"
)

const nesSystemPrompt = `You generate Agent Client Protocol next-edit suggestions for an editor.
Return only JSON. Do not include Markdown fences or explanatory text.
Return {"suggestions":[]} when there is no clear useful suggestion.
Every suggestion must use one of these shapes:
{"kind":"edit","id":"...","uri":"file:///path","edits":[{"range":{"start":{"line":0,"character":0},"end":{"line":0,"character":0}},"newText":"..."}]}
{"kind":"jump","id":"...","uri":"file:///path","position":{"line":0,"character":0}}
{"kind":"rename","id":"...","uri":"file:///path","position":{"line":0,"character":0},"newName":"..."}
{"kind":"searchAndReplace","id":"...","uri":"file:///path","search":"...","replace":"...","isRegex":false}
Use zero-based line and character positions. Prefer one concise edit suggestion.`

type nesSession struct {
	start       acp.UnstableStartNesRequest
	suggestions map[string]acp.UnstableNesSuggestion
	decisions   []nesDecision

	mu         sync.Mutex
	done       <-chan struct{}
	cancel     context.CancelFunc
	suggestSem chan struct{}
}

type nesDecision struct {
	ID       string
	Outcome  string
	Reason   string
	Recorded time.Time
}

type nesSuggestionEnvelope struct {
	Suggestions []acp.UnstableNesSuggestion `json:"suggestions"`
}

func newNESSession(start acp.UnstableStartNesRequest) *nesSession {
	ctx, cancel := context.WithCancel(context.Background())

	return &nesSession{
		start:       cloneNesStartRequest(start),
		suggestions: make(map[string]acp.UnstableNesSuggestion),
		done:        ctx.Done(),
		cancel:      cancel,
		suggestSem:  make(chan struct{}, nesMaxInFlight),
	}
}

func (s *nesSession) close() {
	if s.cancel != nil {
		s.cancel()
	}
}

func (s *nesSession) acquireSuggest(ctx context.Context) (func(), error) {
	select {
	case <-s.done:
		return nil, context.Canceled
	default:
	}

	select {
	case s.suggestSem <- struct{}{}:
		return func() { <-s.suggestSem }, nil
	case <-s.done:
		return nil, context.Canceled
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (s *nesSession) suggestionContext(ctx context.Context) (context.Context, context.CancelFunc) {
	turnCtx, cancel := context.WithTimeout(ctx, nesSuggestionTimeout)
	stop := make(chan struct{})

	var stopOnce sync.Once

	go func() {
		defer recoverAgentGoroutine(ctx, nil, "NES suggestion watcher")

		select {
		case <-s.done:
			cancel()
		case <-stop:
		}
	}()

	return turnCtx, func() {
		stopOnce.Do(func() {
			close(stop)
			cancel()
		})
	}
}

func (a *Agent) nesSession(sessionID acp.SessionId) *nesSession {
	a.mu.Lock()
	defer a.mu.Unlock()

	return a.nesSessions[sessionID]
}

func cloneNesStartRequest(start acp.UnstableStartNesRequest) acp.UnstableStartNesRequest {
	cloned := start
	cloned.Meta = cloneAnyMap(start.Meta)
	cloned.WorkspaceFolders = slices.Clone(start.WorkspaceFolders)

	if start.Repository != nil {
		repository := *start.Repository
		cloned.Repository = &repository
	}

	if start.WorkspaceUri != nil {
		workspaceURI := *start.WorkspaceUri
		cloned.WorkspaceUri = &workspaceURI
	}

	return cloned
}

func cloneAnyMap(values map[string]any) map[string]any {
	if values == nil {
		return nil
	}

	cloned := make(map[string]any, len(values))
	for key, value := range values {
		cloned[key] = cloneAny(value)
	}

	return cloned
}

func cloneAnySlice(values []any) []any {
	if values == nil {
		return nil
	}

	cloned := make([]any, len(values))
	for i, value := range values {
		cloned[i] = cloneAny(value)
	}

	return cloned
}

func cloneAny(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneAnyMap(typed)
	case []any:
		return cloneAnySlice(typed)
	default:
		return value
	}
}

func (a *Agent) suggestNES(
	ctx context.Context,
	session *nesSession,
	params acp.UnstableSuggestNesRequest,
) ([]acp.UnstableNesSuggestion, error) {
	if session == nil {
		return nil, acp.NewInvalidParams(map[string]any{acpFieldSessionID: params.SessionId})
	}

	releaseSuggest, err := session.acquireSuggest(ctx)
	if err != nil {
		return nil, err
	}
	defer releaseSuggest()

	document := a.nesDocumentForRequest(params)

	prompt, err := nesPrompt(session, document, params)
	if err != nil {
		return nil, err
	}

	turnCtx, cancel := session.suggestionContext(ctx)
	defer cancel()

	options := a.nesClaudeOptions(session, params.SessionId)

	client := a.newClaudeClient(a.log, options)
	if startErr := client.Start(turnCtx); startErr != nil {
		return nil, fmt.Errorf("start Claude NES suggestion session: %w", startErr)
	}

	defer func() {
		if closeErr := client.Close(); closeErr != nil {
			a.log.DebugContext(ctx, "close Claude NES suggestion session failed", slog.String(jsonFieldError, closeErr.Error()))
		}
	}()

	if queryErr := client.Query(turnCtx, []map[string]any{{
		jsonFieldType: jsonFieldText,
		jsonFieldText: prompt,
	}}); queryErr != nil {
		return nil, fmt.Errorf("send Claude NES suggestion request: %w", queryErr)
	}

	text, err := collectNESSuggestionText(turnCtx, client)
	if err != nil {
		return nil, err
	}

	suggestions, err := parseNESSuggestions(text, params)
	if err != nil {
		a.log.DebugContext(ctx, "parse Claude NES suggestion response failed", slog.String(jsonFieldError, err.Error()))

		return []acp.UnstableNesSuggestion{}, nil
	}

	return suggestions, nil
}

func (a *Agent) nesClaudeOptions(session *nesSession, sessionID acp.SessionId) claude.Options {
	options := claude.Options{
		CLIPath:               a.options.ClaudePath,
		Cwd:                   nesWorkspacePath(session.start),
		ClaudeHome:            a.options.ClaudeHome,
		Env:                   a.options.Env,
		SessionID:             string(sessionID),
		Model:                 a.options.DefaultModel,
		SystemText:            nesSystemPrompt,
		PermissionMode:        string(modePlan),
		PermissionPromptTool:  permissionPromptTool,
		InitializeTimeout:     a.options.InitializeTimeout,
		ControlHandlerTimeout: a.options.ControlHandlerTimeout,
	}

	options.AddDirs = nesWorkspaceAddDirs(session.start, options.Cwd)
	options.PermissionHandler = func(context.Context, claude.PermissionRequest) (claude.PermissionDecision, error) {
		return claude.PermissionDecision{
			Behavior: claude.BehaviorDeny,
			Message:  "NES suggestions are generated from ACP editor context only",
		}, nil
	}

	return options
}

func nesWorkspacePath(start acp.UnstableStartNesRequest) string {
	if start.WorkspaceUri != nil {
		if path := fileURIToPath(*start.WorkspaceUri); path != "" {
			return path
		}
	}

	for _, folder := range start.WorkspaceFolders {
		if path := fileURIToPath(folder.Uri); path != "" {
			return path
		}
	}

	return ""
}

func nesWorkspaceAddDirs(start acp.UnstableStartNesRequest, cwd string) []string {
	dirs := make([]string, 0, len(start.WorkspaceFolders))

	seen := map[string]struct{}{}
	if cwd != "" {
		seen[cwd] = struct{}{}
	}

	for _, folder := range start.WorkspaceFolders {
		path := fileURIToPath(folder.Uri)
		if path == "" {
			continue
		}

		if _, ok := seen[path]; ok {
			continue
		}

		seen[path] = struct{}{}
		dirs = append(dirs, path)
	}

	return dirs
}

func fileURIToPath(raw string) string {
	parsed, err := url.Parse(raw)
	if err != nil || parsed.Scheme != "file" {
		return ""
	}

	if parsed.Host != "" && parsed.Host != "localhost" {
		return ""
	}

	path := parsed.Path
	if len(path) >= 3 && path[0] == '/' && path[2] == ':' {
		return path[1:]
	}

	return path
}

func (a *Agent) nesDocumentForRequest(params acp.UnstableSuggestNesRequest) documentState {
	a.docsMu.Lock()
	if documents := a.documents[params.SessionId]; documents != nil {
		if document, ok := documents[params.Uri]; ok {
			a.docsMu.Unlock()

			return document
		}
	}
	a.docsMu.Unlock()

	if params.Context != nil {
		for _, recent := range params.Context.RecentFiles {
			if recent.Uri == params.Uri {
				return documentState{
					URI:        recent.Uri,
					LanguageID: recent.LanguageId,
					Text:       recent.Text,
					Version:    params.Version,
				}
			}
		}
	}

	return documentState{URI: params.Uri, Version: params.Version}
}

func nesPrompt(
	session *nesSession,
	document documentState,
	params acp.UnstableSuggestNesRequest,
) (string, error) {
	start, decisions := session.promptSnapshot()

	payload := map[string]any{
		"task":             "Return ACP NES suggestions for the current editor state.",
		"positionEncoding": "client-negotiated ACP positions",
		jsonFieldRequest: map[string]any{
			acpFieldSessionID: params.SessionId,
			jsonFieldURI:      params.Uri,
			"version":         params.Version,
			"triggerKind":     params.TriggerKind,
			"position":        params.Position,
			"selection":       params.Selection,
		},
		"workspace": map[string]any{
			"workspaceUri":     start.WorkspaceUri,
			"workspaceFolders": start.WorkspaceFolders,
			"repository":       start.Repository,
		},
		"document": map[string]any{
			jsonFieldURI:  document.URI,
			"languageId":  document.LanguageID,
			"version":     document.Version,
			jsonFieldText: truncateDocumentText(document.Text),
		},
		"context":           params.Context,
		"previousDecisions": decisions,
		"requirements": []string{
			"Return valid JSON only.",
			"Use the requested URI unless suggesting a jump to another known file.",
			"Do not invent APIs or files.",
			"Return no more than three suggestions.",
		},
	}

	data, err := json.MarshalIndent(payload, "", "  ")
	if err != nil {
		return "", fmt.Errorf("encode NES prompt: %w", err)
	}

	return string(data), nil
}

func (s *nesSession) promptSnapshot() (acp.UnstableStartNesRequest, []nesDecision) {
	s.mu.Lock()
	defer s.mu.Unlock()

	return cloneNesStartRequest(s.start), slices.Clone(s.decisions)
}

func collectNESSuggestionText(ctx context.Context, client *claude.Client) (string, error) {
	var (
		text       strings.Builder
		resultText string
	)

	for {
		msg, err := client.Receive(ctx)
		if err != nil {
			return "", fmt.Errorf("receive Claude NES suggestion response: %w", err)
		}

		switch typed := msg.(type) {
		case *claude.AssistantMessage:
			for _, block := range typed.Content {
				if textBlock, ok := block.(claude.TextBlock); ok {
					text.WriteString(textBlock.Text)
				}
			}
		case *claude.ResultMessage:
			if typed.IsError {
				return "", fmt.Errorf("claude NES suggestion failed: %s", strings.Join(typed.Errors, "; "))
			}

			if typed.Result != "" {
				resultText = typed.Result
			}

			if text.Len() > 0 {
				return text.String(), nil
			}

			return resultText, nil
		}
	}
}

func parseNESSuggestions(text string, params acp.UnstableSuggestNesRequest) ([]acp.UnstableNesSuggestion, error) {
	raw := extractNESJSON(text)
	if raw == "" {
		return nil, fmt.Errorf("missing JSON object in Claude NES response")
	}

	var envelope nesSuggestionEnvelope
	if err := json.Unmarshal([]byte(raw), &envelope); err != nil {
		var suggestions []acp.UnstableNesSuggestion
		if arrayErr := json.Unmarshal([]byte(raw), &suggestions); arrayErr != nil {
			return nil, err
		}

		envelope.Suggestions = suggestions
	}

	return normalizeNESSuggestions(envelope.Suggestions, params), nil
}

func extractNESJSON(text string) string {
	trimmed := strings.TrimSpace(text)
	if trimmed == "" {
		return ""
	}

	if strings.HasPrefix(trimmed, "```") {
		trimmed = strings.TrimSpace(strings.TrimPrefix(trimmed, "```json"))

		trimmed = strings.TrimSpace(strings.TrimPrefix(trimmed, "```"))
		if end := strings.LastIndex(trimmed, "```"); end >= 0 {
			trimmed = strings.TrimSpace(trimmed[:end])
		}
	}

	if strings.HasPrefix(trimmed, "{") || strings.HasPrefix(trimmed, "[") {
		return trimmed
	}

	objectStart := strings.IndexByte(trimmed, '{')
	objectEnd := strings.LastIndexByte(trimmed, '}')
	arrayStart := strings.IndexByte(trimmed, '[')
	arrayEnd := strings.LastIndexByte(trimmed, ']')

	if arrayStart >= 0 && arrayEnd > arrayStart && (objectStart < 0 || arrayStart < objectStart) {
		return trimmed[arrayStart : arrayEnd+1]
	}

	if objectStart >= 0 && objectEnd > objectStart {
		return trimmed[objectStart : objectEnd+1]
	}

	if arrayStart >= 0 && arrayEnd > arrayStart {
		return trimmed[arrayStart : arrayEnd+1]
	}

	return ""
}

func normalizeNESSuggestions(
	suggestions []acp.UnstableNesSuggestion,
	params acp.UnstableSuggestNesRequest,
) []acp.UnstableNesSuggestion {
	normalized := make([]acp.UnstableNesSuggestion, 0, len(suggestions))
	for i := range suggestions {
		suggestion := suggestions[i]
		if normalizeNESSuggestion(&suggestion, params, i) {
			normalized = append(normalized, suggestion)
		}
	}

	return normalized
}

func normalizeNESSuggestion(
	suggestion *acp.UnstableNesSuggestion,
	params acp.UnstableSuggestNesRequest,
	index int,
) bool {
	id := fallbackNESSuggestionID(params, index)

	switch {
	case suggestion.Edit != nil:
		if len(suggestion.Edit.Edits) == 0 {
			return false
		}

		if suggestion.Edit.Id == "" {
			suggestion.Edit.Id = id
		}

		if suggestion.Edit.Uri == "" {
			suggestion.Edit.Uri = params.Uri
		}
	case suggestion.Jump != nil:
		if suggestion.Jump.Id == "" {
			suggestion.Jump.Id = id
		}

		if suggestion.Jump.Uri == "" {
			suggestion.Jump.Uri = params.Uri
		}
	case suggestion.Rename != nil:
		if suggestion.Rename.NewName == "" {
			return false
		}

		if suggestion.Rename.Id == "" {
			suggestion.Rename.Id = id
		}

		if suggestion.Rename.Uri == "" {
			suggestion.Rename.Uri = params.Uri
		}
	case suggestion.SearchAndReplace != nil:
		if suggestion.SearchAndReplace.Search == "" {
			return false
		}

		if suggestion.SearchAndReplace.Id == "" {
			suggestion.SearchAndReplace.Id = id
		}

		if suggestion.SearchAndReplace.Uri == "" {
			suggestion.SearchAndReplace.Uri = params.Uri
		}
	default:
		return false
	}

	return suggestion.Validate() == nil
}

func fallbackNESSuggestionID(params acp.UnstableSuggestNesRequest, index int) string {
	sum := sha256.Sum256(fmt.Appendf(nil, "%s|%s|%d|%d", params.SessionId, params.Uri, params.Version, index))

	return "nes-" + hex.EncodeToString(sum[:8])
}

func (a *Agent) storeNESSuggestions(sessionID acp.SessionId, suggestions []acp.UnstableNesSuggestion) {
	if len(suggestions) == 0 {
		return
	}

	a.mu.Lock()
	session := a.nesSessions[sessionID]
	a.mu.Unlock()

	if session == nil {
		return
	}

	session.mu.Lock()
	defer session.mu.Unlock()

	for _, suggestion := range suggestions {
		id := nesSuggestionID(suggestion)
		if id != "" {
			session.suggestions[id] = suggestion
		}
	}
}

func nesSuggestionID(suggestion acp.UnstableNesSuggestion) string {
	switch {
	case suggestion.Edit != nil:
		return suggestion.Edit.Id
	case suggestion.Jump != nil:
		return suggestion.Jump.Id
	case suggestion.Rename != nil:
		return suggestion.Rename.Id
	case suggestion.SearchAndReplace != nil:
		return suggestion.SearchAndReplace.Id
	default:
		return ""
	}
}

func (a *Agent) recordNESDecision(
	sessionID acp.SessionId,
	suggestionID string,
	outcome string,
	reason *acp.UnstableNesRejectReason,
) error {
	a.mu.Lock()
	session := a.nesSessions[sessionID]
	a.mu.Unlock()

	if session == nil {
		return acp.NewInvalidParams(map[string]any{acpFieldSessionID: sessionID})
	}

	session.mu.Lock()
	defer session.mu.Unlock()

	if _, ok := session.suggestions[suggestionID]; !ok {
		return acp.NewInvalidParams(map[string]any{"suggestionId": suggestionID})
	}

	decision := nesDecision{
		ID:       suggestionID,
		Outcome:  outcome,
		Recorded: time.Now(),
	}
	if reason != nil {
		decision.Reason = string(*reason)
	}

	session.decisions = append(session.decisions, decision)

	return nil
}
