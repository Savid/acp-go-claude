package mapper

import (
	"encoding/json"
	"fmt"
	"math"
	neturl "net/url"
	pathpkg "path"
	"path/filepath"
	"strings"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
)

const (
	toolBash             = "Bash"
	toolEdit             = "Edit"
	toolExitPlanMode     = "ExitPlanMode"
	toolGrepCommand      = "grep"
	toolMultiEdit        = "MultiEdit"
	toolNotebookEdit     = "NotebookEdit"
	toolRead             = "Read"
	toolStructuredOutput = "StructuredOutput"
	toolTask             = "Task"
	toolTodoWrite        = "TodoWrite"
	toolWorkflow         = "Workflow"
	toolWrite            = "Write"

	keyAllowedDomains     = "allowed_domains"
	keyBlockedDomains     = "blocked_domains"
	keyClaude             = "claude"
	keyContent            = "content"
	keyDescription        = "description"
	keyErrorCode          = "error_code"
	keyErrorMessage       = "error_message"
	keyGlob               = "glob"
	keyHeadLimit          = "head_limit"
	keyIsError            = "is_error"
	keyIsFileUpdate       = "is_file_update"
	keyLimit              = "limit"
	keyLines              = "lines"
	keyMultiline          = "multiline"
	keyMessageID          = "messageId"
	keyEdits              = "edits"
	keyNotebookPath       = "notebook_path"
	keyNewSource          = "new_source"
	keyCellID             = "cell_id"
	keyNewStart           = "newStart"
	keyNewString          = "new_string"
	keyOffset             = "offset"
	keyOldString          = "old_string"
	keyOutputMode         = "output_mode"
	keyPattern            = "pattern"
	keyPlan               = "plan"
	keyPrompt             = "prompt"
	keyQuery              = "query"
	keyReturnCode         = "return_code"
	keyStderr             = "stderr"
	keyStdout             = "stdout"
	keyStatus             = "status"
	keyStructuredPatch    = "structuredPatch"
	keyTodos              = "todos"
	keyToolName           = "toolName"
	keyToolNameSnake      = "tool_name"
	keyToolResponse       = "toolResponse"
	keyToolReferences     = "tool_references"
	keyToolUseID          = "tool_use_id"
	keyDelta              = "delta"
	keyTerminalID         = "terminal_id"
	keyTerminalInfo       = "terminal_info"
	keyTerminalOutput     = "terminal_output"
	keyTerminalExit       = "terminal_exit"
	keyTerminalData       = "data"
	keyTerminalExitCode   = "exit_code"
	keyTerminalSignal     = "signal"
	keyThinking           = "thinking"
	keyTitle              = "title"
	keyToolUseResult      = "tool_use_result"
	keyInternalImageIndex = "_internalImageIndex"

	streamEventContentBlockDelta = "content_block_delta"
	streamEventContentBlockStart = "content_block_start"
	streamEventThinkingDelta     = "thinking_delta"
	streamEventTextDelta         = "text_delta"

	toolResultBashCodeExecution      = "bash_code_execution_result"
	toolResultBashCodeExecutionError = "bash_code_execution_tool_result_error"
	toolResultCodeExecution          = "code_execution_result"
	toolResultCodeExecutionError     = "code_execution_tool_result_error"
	toolResultTextEditorCreate       = "text_editor_code_execution_create_result"
	toolResultTextEditorError        = "text_editor_code_execution_tool_result_error"
	toolResultTextEditorStrReplace   = "text_editor_code_execution_str_replace_result"
	toolResultTextEditorView         = "text_editor_code_execution_view_result"
	toolResultToolReference          = "tool_reference"
	toolResultToolSearch             = "tool_search_tool_search_result"
	toolResultToolSearchError        = "tool_search_tool_result_error"
	toolResultWebFetch               = "web_fetch_result"
	toolResultWebFetchError          = "web_fetch_tool_result_error"
	toolResultWebSearch              = "web_search_result"
	toolResultWebSearchError         = "web_search_tool_result_error"

	localCommandStderrTag = "<local-command-stderr>"
	localCommandStdoutTag = "<local-command-stdout>"
)

// ToolUpdateOptions carries client/session context for tool-call mapping.
type ToolUpdateOptions struct {
	Cwd                     string
	SupportsTerminalOutput  bool
	ReplayAssistantIdentity bool
	ToolUses                map[string]claude.ToolUseBlock
	ParentToolUseID         string
	Workflow                *WorkflowTracker
}

// ToolInfo describes ACP-facing metadata for a Claude tool call.
type ToolInfo struct {
	Title     string
	Kind      acp.ToolKind
	Locations []acp.ToolCallLocation
	Content   []acp.ToolCallContent
}

// ToolTitle returns a concise UI title for a Claude tool call.
func ToolTitle(toolName string, input map[string]any) string {
	return ToolCallInfo(toolName, "", input, ToolUpdateOptions{}).Title
}

// ToolCallInfo derives ACP tool-call metadata from Claude tool input.
func ToolCallInfo(toolName string, toolID string, input map[string]any, options ToolUpdateOptions) ToolInfo {
	switch strings.ToLower(toolName) {
	case "agent", "task":
		return agentToolInfo(input)
	case keyWorkflow:
		return workflowToolInfo(input)
	case "bash":
		return bashToolInfo(toolID, input, options.SupportsTerminalOutput)
	case "read", "notebookread":
		return readToolInfo(input, options.Cwd)
	case "write":
		return writeToolInfo(input, options.Cwd)
	case "edit", "multiedit", "notebookedit":
		return editToolInfo(input, options.Cwd)
	case "glob":
		return globToolInfo(input)
	case "grep":
		return grepToolInfo(input)
	case "ls":
		return lsToolInfo(input)
	case "webfetch":
		return webFetchToolInfo(input)
	case "websearch":
		return webSearchToolInfo(input)
	case "todowrite":
		return todoWriteToolInfo(input)
	case "exitplanmode":
		return exitPlanModeToolInfo(input)
	case "other":
		return otherToolInfo(toolName, input)
	default:
		info := otherToolInfo(toolName, input)
		if strings.TrimSpace(toolName) == "" {
			info.Title = "Claude tool call"
		}

		if path := firstNonEmptyString(stringInput(input, keyFilePath), stringInput(input, keyPath)); path != "" {
			info.Title += " " + path
		} else if command := stringInput(input, keyCommand); command != "" {
			info.Title += " " + command
		}

		return info
	}
}

// MessageToUpdatesWithOptions maps one Claude message to ACP updates with session context.
func MessageToUpdatesWithOptions(msg claude.Message, options ToolUpdateOptions) []acp.SessionUpdate {
	switch typed := msg.(type) {
	case *claude.AssistantMessage:
		if options.ParentToolUseID == "" {
			options.ParentToolUseID = typed.ParentToolUseID
		}

		return assistantUpdates(typed, options)
	case *claude.UserMessage:
		if options.ParentToolUseID == "" {
			options.ParentToolUseID = typed.ParentToolUseID
		}

		return userToolResultUpdates(typed, options)
	case *claude.StreamEventMessage:
		return streamEventUpdates(typed, options)
	case *claude.SystemMessage:
		return workflowSystemUpdates(typed, options)
	default:
		return nil
	}
}

func userToolResultUpdates(msg *claude.UserMessage, options ToolUpdateOptions) []acp.SessionUpdate {
	content := userMessageContent(msg)

	values, _ := content.([]any)
	if len(values) == 0 {
		return nil
	}

	updates := make([]acp.SessionUpdate, 0, len(values))
	for _, value := range values {
		block, _ := value.(map[string]any)
		if block == nil || stringInput(block, keyType) != claude.BlockTypeToolResult {
			continue
		}

		parsed, ok := claude.ParseContentBlock(block).(claude.ToolResultBlock)
		if !ok || parsed.ToolUseID == "" {
			continue
		}

		workflowUpdates := workflowLaunchResultUpdates(parsed, options, workflowLaunchResult(msg.Raw))
		if len(workflowUpdates) > 0 {
			updates = append(updates, workflowUpdates...)

			continue
		}

		updates = append(updates, toolResultUpdates(parsed, options)...)
	}

	if len(updates) == 0 {
		return nil
	}

	return updates
}

func userMessageContent(msg *claude.UserMessage) any {
	if msg == nil {
		return nil
	}

	if msg.Content != nil {
		return msg.Content
	}

	message, _ := msg.Raw[keyMessage].(map[string]any)

	return message[keyContent]
}

func streamEventUpdates(msg *claude.StreamEventMessage, options ToolUpdateOptions) []acp.SessionUpdate {
	if options.ParentToolUseID == "" {
		options.ParentToolUseID = msg.ParentToolUseID
	}

	switch msg.EventType {
	case streamEventContentBlockStart:
		blockRaw, ok := mapInput(msg.Event, "content_block")
		if !ok || blockRaw == nil {
			return nil
		}

		return assistantUpdates(&claude.AssistantMessage{
			Content:         []claude.ContentBlock{claude.ParseContentBlock(blockRaw)},
			ParentToolUseID: msg.ParentToolUseID,
		}, options)
	case streamEventContentBlockDelta:
		deltaRaw, ok := mapInput(msg.Event, keyDelta)
		if !ok || deltaRaw == nil {
			return nil
		}

		block := deltaContentBlock(deltaRaw)
		if block == nil {
			return nil
		}

		return assistantUpdates(&claude.AssistantMessage{
			Content:         []claude.ContentBlock{block},
			ParentToolUseID: msg.ParentToolUseID,
		}, options)
	default:
		return nil
	}
}

func deltaContentBlock(raw map[string]any) claude.ContentBlock {
	switch stringInput(raw, keyType) {
	case streamEventTextDelta:
		if text := stringInput(raw, keyText); text != "" {
			return claude.TextBlock{Text: text}
		}
	case streamEventThinkingDelta:
		if thinking := stringInput(raw, keyThinking); thinking != "" {
			return claude.ThinkingBlock{Thinking: thinking}
		}
	}

	return nil
}

func assistantUpdates(msg *claude.AssistantMessage, options ToolUpdateOptions) []acp.SessionUpdate {
	if options.ToolUses == nil {
		options.ToolUses = make(map[string]claude.ToolUseBlock)
	}

	updates := make([]acp.SessionUpdate, 0, len(msg.Content))
	for _, block := range msg.Content {
		switch typed := block.(type) {
		case claude.TextBlock:
			if localCommandMarkerText(typed.Text) {
				continue
			}

			updates = append(updates, withParentToolUseID(acp.UpdateAgentMessageText(typed.Text), options.ParentToolUseID))
		case claude.ThinkingBlock:
			if typed.Thinking != "" {
				updates = append(updates, withParentToolUseID(acp.UpdateAgentThoughtText(typed.Thinking), options.ParentToolUseID))
			}
		case claude.ToolUseBlock:
			updates = append(updates, toolUseUpdates(typed, options)...)
		case claude.ToolResultBlock:
			updates = append(updates, toolResultUpdates(typed, options)...)
		case claude.UnknownBlock:
			if content, ok := assistantUnknownBlockUpdate(typed); ok {
				updates = append(updates, withParentToolUseID(content, options.ParentToolUseID))
			}
		}
	}

	if checkpointableAssistantMessage(msg, options.ReplayAssistantIdentity) {
		for _, update := range updates {
			setAssistantMessageID(updateMeta(update), msg.MessageID)
		}
	} else if msg.MessageID != "" {
		for _, update := range updates {
			if update.AgentMessageChunk != nil &&
				(update.AgentMessageChunk.Content.Image != nil ||
					update.AgentMessageChunk.Content.ResourceLink != nil) {
				setAssistantMessageID(updateMeta(update), msg.MessageID)
			}
		}
	}

	imageIndex := 0

	for _, update := range updates {
		if update.AgentMessageChunk == nil ||
			(update.AgentMessageChunk.Content.Image == nil &&
				update.AgentMessageChunk.Content.ResourceLink == nil) {
			continue
		}

		setInternalImageIndex(updateMeta(update), imageIndex)
		imageIndex++
	}

	return updates
}

func setInternalImageIndex(meta map[string]any, index int) {
	if meta == nil {
		return
	}

	claudeMeta, _ := meta[keyClaude].(map[string]any)
	if claudeMeta == nil {
		claudeMeta = make(map[string]any)
		meta[keyClaude] = claudeMeta
	}

	claudeMeta[keyInternalImageIndex] = index
}

func checkpointableAssistantMessage(msg *claude.AssistantMessage, replay bool) bool {
	return msg.MessageID != "" && msg.ParentToolUseID == "" &&
		(replay || msg.StopReason != "" && msg.StopReason != stopReasonToolUse)
}

func assistantUnknownBlockUpdate(block claude.UnknownBlock) (acp.SessionUpdate, bool) {
	if block.Type != typeImage {
		return acp.SessionUpdate{}, false
	}

	content, ok := imageContentBlock(block.Raw)
	if !ok {
		return acp.SessionUpdate{}, false
	}

	return acp.UpdateAgentMessage(content), true
}

func localCommandMarkerText(text string) bool {
	return strings.Contains(text, localCommandStdoutTag) || strings.Contains(text, localCommandStderrTag)
}

func toolUseUpdates(block claude.ToolUseBlock, options ToolUpdateOptions) []acp.SessionUpdate {
	_, alreadyKnown := options.ToolUses[block.ID]
	if block.ID != "" {
		options.ToolUses[block.ID] = block
	}

	if strings.EqualFold(block.Name, toolTodoWrite) {
		entries := PlanEntries(block.Input)
		if len(entries) == 0 {
			return nil
		}

		return []acp.SessionUpdate{withParentToolUseID(acp.UpdatePlan(entries...), options.ParentToolUseID)}
	}

	if strings.EqualFold(block.Name, toolStructuredOutput) {
		return nil
	}

	info := ToolCallInfo(block.Name, block.ID, block.Input, options)
	if alreadyKnown {
		update := acp.UpdateToolCall(
			acp.ToolCallId(block.ID),
			acp.WithUpdateTitle(info.Title),
			acp.WithUpdateKind(info.Kind),
			acp.WithUpdateLocations(info.Locations),
			acp.WithUpdateContent(info.Content),
			acp.WithUpdateRawInput(block.Input),
		)
		update.ToolCallUpdate.Meta = toolMeta(block.Name, nil)

		return []acp.SessionUpdate{withParentToolUseID(update, options.ParentToolUseID)}
	}

	update := acp.StartToolCall(
		acp.ToolCallId(block.ID),
		info.Title,
		acp.WithStartKind(info.Kind),
		acp.WithStartStatus(acp.ToolCallStatusPending),
		acp.WithStartLocations(info.Locations),
		acp.WithStartContent(info.Content),
		acp.WithStartRawInput(block.Input),
	)
	update.ToolCall.Meta = toolMeta(block.Name, nil)

	if strings.EqualFold(block.Name, toolBash) && options.SupportsTerminalOutput && block.ID != "" {
		update.ToolCall.Meta[keyTerminalInfo] = map[string]any{keyTerminalID: block.ID}
	}

	return []acp.SessionUpdate{withParentToolUseID(update, options.ParentToolUseID)}
}

func toolResultUpdates(block claude.ToolResultBlock, options ToolUpdateOptions) []acp.SessionUpdate {
	toolUse, known := options.ToolUses[block.ToolUseID]
	if !known {
		toolUse = claude.ToolUseBlock{ID: block.ToolUseID}
	}

	if strings.EqualFold(toolUse.Name, toolTodoWrite) {
		return nil
	}

	if strings.EqualFold(toolUse.Name, toolStructuredOutput) {
		return nil
	}

	status := acp.ToolCallStatusCompleted
	if block.IsError {
		status = acp.ToolCallStatusFailed
	}

	if strings.EqualFold(toolUse.Name, toolBash) && options.SupportsTerminalOutput {
		return bashToolResultUpdates(block, status, options.ParentToolUseID)
	}

	content, locations := toolResultContent(block, toolUse)
	update := acp.UpdateToolCall(
		acp.ToolCallId(block.ToolUseID),
		acp.WithUpdateStatus(status),
		acp.WithUpdateContent(content),
		acp.WithUpdateLocations(locations),
		acp.WithUpdateRawOutput(block.Raw),
	)
	update.ToolCallUpdate.Meta = toolMeta(toolUse.Name, block.Raw)

	if strings.EqualFold(toolUse.Name, toolExitPlanMode) && !block.IsError {
		update.ToolCallUpdate.Title = stringPtr("Exited Plan Mode")
	}

	return []acp.SessionUpdate{withParentToolUseID(update, options.ParentToolUseID)}
}

func bashToolResultUpdates(block claude.ToolResultBlock, status acp.ToolCallStatus, parentToolUseID string) []acp.SessionUpdate {
	output, exitCode := bashOutputAndExit(block)
	updates := make([]acp.SessionUpdate, 0, 2)

	if strings.TrimSpace(output) != "" {
		terminalOutput := acp.UpdateToolCall(acp.ToolCallId(block.ToolUseID))
		terminalOutput.ToolCallUpdate.Meta = map[string]any{
			keyTerminalOutput: map[string]any{
				keyTerminalID:   block.ToolUseID,
				keyTerminalData: output,
			},
		}
		updates = append(updates, withParentToolUseID(terminalOutput, parentToolUseID))
	}

	final := acp.UpdateToolCall(
		acp.ToolCallId(block.ToolUseID),
		acp.WithUpdateStatus(status),
		acp.WithUpdateContent([]acp.ToolCallContent{acp.ToolTerminalRef(block.ToolUseID)}),
		acp.WithUpdateRawOutput(block.Raw),
	)
	final.ToolCallUpdate.Meta = toolMeta(toolBash, block.Raw)

	terminalExit := map[string]any{
		keyTerminalID:     block.ToolUseID,
		keyTerminalSignal: nil,
	}
	if exitCode != nil {
		terminalExit[keyTerminalExitCode] = *exitCode
	}

	final.ToolCallUpdate.Meta[keyTerminalExit] = terminalExit

	return append(updates, withParentToolUseID(final, parentToolUseID))
}

func agentToolInfo(input map[string]any) ToolInfo {
	title := stringInput(input, keyDescription)
	if title == "" {
		title = toolTask
	}

	var content []acp.ToolCallContent
	if prompt := stringInput(input, keyPrompt); prompt != "" {
		content = []acp.ToolCallContent{acp.ToolContent(acp.TextBlock(prompt))}
	}

	return ToolInfo{Title: title, Kind: acp.ToolKindThink, Content: content}
}

func workflowToolInfo(input map[string]any) ToolInfo {
	title := firstNonEmptyString(
		stringInput(input, "workflow_name"),
		stringInput(input, "name"),
		stringInput(input, keyDescription),
		stringInput(input, "summary"),
	)
	if title == "" {
		title = toolWorkflow
	}

	var content []acp.ToolCallContent
	if description := firstNonEmptyString(stringInput(input, keyDescription), stringInput(input, "summary")); description != "" {
		content = []acp.ToolCallContent{acp.ToolContent(acp.TextBlock(description))}
	}

	return ToolInfo{Title: title, Kind: acp.ToolKindThink, Content: content}
}

func bashToolInfo(toolID string, input map[string]any, supportsTerminalOutput bool) ToolInfo {
	title := stringInput(input, keyCommand)
	if title == "" {
		title = "Terminal"
	}

	var content []acp.ToolCallContent
	if supportsTerminalOutput && toolID != "" {
		content = []acp.ToolCallContent{acp.ToolTerminalRef(toolID)}
	} else if description := stringInput(input, keyDescription); description != "" {
		content = []acp.ToolCallContent{acp.ToolContent(acp.TextBlock(description))}
	}

	return ToolInfo{Title: title, Kind: acp.ToolKindExecute, Content: content}
}

func readToolInfo(input map[string]any, cwd string) ToolInfo {
	path := stringInput(input, keyFilePath)

	displayPath := "File"
	if path != "" {
		displayPath = displayPathForCwd(path, cwd)
	}

	offset, hasOffset := intInput(input, keyOffset)
	limit, hasLimit := intInput(input, keyLimit)
	title := "Read " + displayPath

	if hasLimit && limit > 0 {
		start := 1
		if hasOffset {
			start = offset
		}

		title = fmt.Sprintf("%s (%d - %d)", title, start, start+limit-1)
	} else if hasOffset {
		title = fmt.Sprintf("%s (from line %d)", title, offset)
	}

	return ToolInfo{Title: title, Kind: acp.ToolKindRead, Locations: locations(input)}
}

func writeToolInfo(input map[string]any, cwd string) ToolInfo {
	path := stringInput(input, keyFilePath)

	title := toolWrite
	if path != "" {
		title = "Write " + displayPathForCwd(path, cwd)
	}

	var content []acp.ToolCallContent
	if path != "" {
		content = []acp.ToolCallContent{acp.ToolDiffContent(path, stringInput(input, keyContent))}
	} else if text := stringInput(input, keyContent); text != "" {
		content = []acp.ToolCallContent{acp.ToolContent(acp.TextBlock(text))}
	}

	return ToolInfo{Title: title, Kind: acp.ToolKindEdit, Locations: locations(input), Content: content}
}

func editToolInfo(input map[string]any, cwd string) ToolInfo {
	path := stringInput(input, keyFilePath)
	notebookPath := stringInput(input, keyNotebookPath)

	title := toolEdit
	if displayPath := firstNonEmptyString(path, notebookPath); displayPath != "" {
		title = "Edit " + displayPathForCwd(displayPath, cwd)
	}

	if notebookPath != "" {
		if cellID := stringInput(input, keyCellID); cellID != "" {
			title += " cell " + cellID
		}
	}

	oldText := stringInput(input, keyOldString)
	newText := stringInput(input, keyNewString)

	var content []acp.ToolCallContent

	switch {
	case path != "" && len(editItems(input)) > 0:
		content = multiEditToolContent(path, input)
	case notebookPath != "" && stringInput(input, keyNewSource) != "":
		content = []acp.ToolCallContent{acp.ToolDiffContent(notebookPath, stringInput(input, keyNewSource))}
	case path != "" && (oldText != "" || newText != ""):
		if oldText != "" {
			content = []acp.ToolCallContent{acp.ToolDiffContent(path, newText, oldText)}
		} else {
			content = []acp.ToolCallContent{acp.ToolDiffContent(path, newText)}
		}
	}

	return ToolInfo{Title: title, Kind: acp.ToolKindEdit, Locations: editLocations(input), Content: content}
}

func editItems(input map[string]any) []any {
	items, _ := input[keyEdits].([]any)

	return items
}

func multiEditToolContent(path string, input map[string]any) []acp.ToolCallContent {
	items := editItems(input)
	content := make([]acp.ToolCallContent, 0, len(items))

	for _, item := range items {
		edit, _ := item.(map[string]any)
		oldText := stringInput(edit, keyOldString)
		newText := stringInput(edit, keyNewString)

		if oldText == "" && newText == "" {
			continue
		}

		if oldText != "" {
			content = append(content, acp.ToolDiffContent(path, newText, oldText))
		} else {
			content = append(content, acp.ToolDiffContent(path, newText))
		}
	}

	return content
}

func editLocations(input map[string]any) []acp.ToolCallLocation {
	if notebookPath := stringInput(input, keyNotebookPath); notebookPath != "" {
		return []acp.ToolCallLocation{locationWithOptionalLine(notebookPath, 0)}
	}

	return locations(input)
}

func globToolInfo(input map[string]any) ToolInfo {
	title := "Find"
	if path := stringInput(input, keyPath); path != "" {
		title += " `" + path + "`"
	}

	if pattern := stringInput(input, keyPattern); pattern != "" {
		title += " `" + pattern + "`"
	}

	return ToolInfo{Title: title, Kind: acp.ToolKindSearch, Locations: locations(input)}
}

func grepToolInfo(input map[string]any) ToolInfo {
	var title strings.Builder
	title.WriteString(toolGrepCommand)

	if boolInput(input, "-i") {
		title.WriteString(" -i")
	}

	if boolInput(input, "-n") {
		title.WriteString(" -n")
	}

	for _, flag := range []string{"-A", "-B", "-C"} {
		if value, ok := intInput(input, flag); ok {
			fmt.Fprintf(&title, " %s %d", flag, value)
		}
	}

	switch stringInput(input, keyOutputMode) {
	case "files_with_matches":
		title.WriteString(" -l")
	case "count":
		title.WriteString(" -c")
	}

	if value, ok := intInput(input, keyHeadLimit); ok {
		fmt.Fprintf(&title, " | head -%d", value)
	}

	if glob := stringInput(input, keyGlob); glob != "" {
		title.WriteString(` --include="`)
		title.WriteString(glob)
		title.WriteString(`"`)
	}

	if fileType := stringInput(input, keyType); fileType != "" {
		title.WriteString(" --type=")
		title.WriteString(fileType)
	}

	if boolInput(input, keyMultiline) {
		title.WriteString(" -P")
	}

	if pattern := stringInput(input, keyPattern); pattern != "" {
		title.WriteString(` "`)
		title.WriteString(pattern)
		title.WriteString(`"`)
	}

	if path := stringInput(input, keyPath); path != "" {
		title.WriteString(" ")
		title.WriteString(path)
	}

	return ToolInfo{Title: title.String(), Kind: acp.ToolKindSearch}
}

func lsToolInfo(input map[string]any) ToolInfo {
	title := "LS"
	if path := stringInput(input, keyPath); path != "" {
		title += " " + path
	}

	return ToolInfo{Title: title, Kind: acp.ToolKindSearch, Locations: locations(input)}
}

func webFetchToolInfo(input map[string]any) ToolInfo {
	title := "Fetch"
	if rawURL := stringInput(input, keyURL); rawURL != "" {
		title += " " + rawURL
	}

	var content []acp.ToolCallContent
	if prompt := stringInput(input, keyPrompt); prompt != "" {
		content = []acp.ToolCallContent{acp.ToolContent(acp.TextBlock(prompt))}
	}

	return ToolInfo{Title: title, Kind: acp.ToolKindFetch, Content: content}
}

func webSearchToolInfo(input map[string]any) ToolInfo {
	title := "Web search"
	if query := stringInput(input, keyQuery); query != "" {
		title = fmt.Sprintf("search: %q", query)
	}

	if domains := stringSliceInput(input, keyAllowedDomains); len(domains) > 0 {
		title += " (allowed: " + strings.Join(domains, ", ") + ")"
	}

	if domains := stringSliceInput(input, keyBlockedDomains); len(domains) > 0 {
		title += " (blocked: " + strings.Join(domains, ", ") + ")"
	}

	return ToolInfo{Title: title, Kind: acp.ToolKindFetch}
}

func todoWriteToolInfo(input map[string]any) ToolInfo {
	todos, _ := input[keyTodos].([]any)
	if len(todos) == 0 {
		return ToolInfo{Title: "Update TODOs", Kind: acp.ToolKindThink}
	}

	labels := make([]string, 0, len(todos))
	for _, item := range todos {
		todo, _ := item.(map[string]any)
		if content := stringInput(todo, keyContent); content != "" {
			labels = append(labels, content)
		}
	}

	if len(labels) == 0 {
		return ToolInfo{Title: "Update TODOs", Kind: acp.ToolKindThink}
	}

	return ToolInfo{Title: "Update TODOs: " + strings.Join(labels, ", "), Kind: acp.ToolKindThink}
}

func exitPlanModeToolInfo(input map[string]any) ToolInfo {
	var content []acp.ToolCallContent
	if plan := stringInput(input, keyPlan); plan != "" {
		content = []acp.ToolCallContent{acp.ToolContent(acp.TextBlock(plan))}
	}

	return ToolInfo{Title: "Ready to code?", Kind: acp.ToolKindSwitchMode, Content: content}
}

func otherToolInfo(toolName string, input map[string]any) ToolInfo {
	title := strings.TrimSpace(toolName)
	if title == "" {
		title = "Unknown Tool"
	}

	output := "{}"

	if input != nil {
		if data, err := json.MarshalIndent(input, "", "  "); err == nil {
			output = string(data)
		}
	}

	return ToolInfo{
		Title:   title,
		Kind:    acp.ToolKindOther,
		Content: []acp.ToolCallContent{acp.ToolContent(acp.TextBlock("```json\n" + output + "\n```"))},
	}
}

// PlanEntries converts Claude TodoWrite input into ACP plan entries.
func PlanEntries(input map[string]any) []acp.PlanEntry {
	todos, _ := input[keyTodos].([]any)
	if len(todos) == 0 {
		return nil
	}

	entries := make([]acp.PlanEntry, 0, len(todos))
	for _, item := range todos {
		todo, _ := item.(map[string]any)
		if todo == nil {
			continue
		}

		content := stringInput(todo, keyContent)
		if content == "" {
			continue
		}

		entries = append(entries, acp.PlanEntry{
			Content:  content,
			Priority: acp.PlanEntryPriorityMedium,
			Status:   planEntryStatus(stringInput(todo, keyStatus)),
		})
	}

	return entries
}

func planEntryStatus(status string) acp.PlanEntryStatus {
	switch acp.PlanEntryStatus(status) {
	case acp.PlanEntryStatusInProgress:
		return acp.PlanEntryStatusInProgress
	case acp.PlanEntryStatusCompleted:
		return acp.PlanEntryStatusCompleted
	default:
		return acp.PlanEntryStatusPending
	}
}

func toolResultContent(
	block claude.ToolResultBlock,
	toolUse claude.ToolUseBlock,
) ([]acp.ToolCallContent, []acp.ToolCallLocation) {
	if block.IsError {
		return genericToolResultContent(block, true), nil
	}

	switch {
	case strings.EqualFold(toolUse.Name, toolRead):
		return readToolResultContent(block), nil
	case strings.EqualFold(toolUse.Name, toolBash):
		return bashToolResultContent(block), nil
	case strings.EqualFold(toolUse.Name, toolEdit),
		strings.EqualFold(toolUse.Name, toolMultiEdit),
		strings.EqualFold(toolUse.Name, toolNotebookEdit),
		strings.EqualFold(toolUse.Name, toolWrite):
		return diffToolResultContent(block.Raw)
	default:
		return genericToolResultContent(block, false), nil
	}
}

func readToolResultContent(block claude.ToolResultBlock) []acp.ToolCallContent {
	return toolResultContentBlocks(block.Raw[keyContent], block.Content, false, true)
}

func bashToolResultContent(block claude.ToolResultBlock) []acp.ToolCallContent {
	output, _ := bashOutputAndExit(block)
	if strings.TrimSpace(output) == "" {
		return nil
	}

	return []acp.ToolCallContent{acp.ToolContent(acp.TextBlock(consoleBlock(output)))}
}

func genericToolResultContent(block claude.ToolResultBlock, isError bool) []acp.ToolCallContent {
	return toolResultContentBlocks(block.Raw[keyContent], block.Content, isError, false)
}

// DiffToolResultContent converts Claude structuredPatch tool output into ACP diff content.
func DiffToolResultContent(raw map[string]any) ([]acp.ToolCallContent, []acp.ToolCallLocation) {
	return diffToolResultContent(raw)
}

func toolResultContentBlocks(raw any, blocks []claude.ContentBlock, isError bool, escapeReadText bool) []acp.ToolCallContent {
	if result := rawToolResultContent(raw, isError, escapeReadText); len(result) > 0 {
		return result
	}

	result := make([]acp.ToolCallContent, 0, len(blocks))
	for _, block := range blocks {
		if content, ok := contentBlockToToolContent(block, isError); ok {
			result = append(result, content)
		}
	}

	if len(result) == 0 {
		return nil
	}

	return result
}

func rawToolResultContent(raw any, isError bool, escapeReadText bool) []acp.ToolCallContent {
	switch typed := raw.(type) {
	case string:
		if typed == "" {
			return nil
		}

		return []acp.ToolCallContent{textToolContent(typed, isError, escapeReadText)}
	case map[string]any:
		if content, ok := rawMapToToolContent(typed, isError, escapeReadText); ok {
			return []acp.ToolCallContent{content}
		}
	case []any:
		result := make([]acp.ToolCallContent, 0, len(typed))
		for _, item := range typed {
			switch value := item.(type) {
			case string:
				if value != "" {
					result = append(result, textToolContent(value, isError, escapeReadText))
				}
			case map[string]any:
				if content, ok := rawMapToToolContent(value, isError, escapeReadText); ok {
					result = append(result, content)
				}
			}
		}

		return result
	}

	return nil
}

func contentBlockToToolContent(block claude.ContentBlock, isError bool) (acp.ToolCallContent, bool) {
	switch typed := block.(type) {
	case claude.TextBlock:
		return textToolContent(typed.Text, isError, false), true
	case claude.UnknownBlock:
		return unknownBlockToToolContent(typed, isError)
	default:
		return acp.ToolCallContent{}, false
	}
}

func unknownBlockToToolContent(block claude.UnknownBlock, isError bool) (acp.ToolCallContent, bool) {
	if block.Type == typeImage {
		if content, ok := imageContentBlock(block.Raw); ok {
			return acp.ToolContent(content), true
		}
	}

	if block.Raw != nil {
		return rawMapToToolContent(block.Raw, isError, false)
	}

	return acp.ToolCallContent{}, false
}

func rawMapToToolContent(raw map[string]any, isError bool, escapeReadText bool) (acp.ToolCallContent, bool) {
	wrapText := func(text string) (acp.ToolCallContent, bool) {
		return textToolContent(text, isError, escapeReadText), true
	}

	switch stringInput(raw, keyType) {
	case typeText:
		return wrapText(stringInput(raw, keyText))
	case typeImage:
		if content, ok := imageContentBlock(raw); ok {
			return acp.ToolContent(content), true
		}

		return acp.ToolCallContent{}, false
	case toolResultToolReference:
		return wrapText("Tool: " + stringInput(raw, keyToolNameSnake))
	case toolResultToolSearch:
		return wrapText("Tools found: " + toolReferenceNames(raw))
	case toolResultToolSearchError, toolResultTextEditorError:
		return wrapText(errorCodeMessage(raw))
	case toolResultWebSearch:
		return wrapText(webSearchResultText(raw))
	case toolResultWebSearchError, toolResultWebFetchError, toolResultCodeExecutionError, toolResultBashCodeExecutionError:
		return wrapText("Error: " + stringInput(raw, keyErrorCode))
	case toolResultWebFetch:
		return wrapText("Fetched: " + stringInput(raw, keyURL))
	case toolResultCodeExecution, toolResultBashCodeExecution:
		return wrapText("Output: " + firstNonEmptyString(stringInput(raw, keyStdout), stringInput(raw, keyStderr)))
	case toolResultTextEditorView:
		return wrapText(stringInput(raw, keyContent))
	case toolResultTextEditorCreate:
		if boolInput(raw, keyIsFileUpdate) {
			return wrapText("File updated")
		}

		return wrapText("File created")
	case toolResultTextEditorStrReplace:
		return wrapText(strings.Join(stringSliceInput(raw, keyLines), "\n"))
	default:
		if data, err := json.Marshal(raw); err == nil {
			return wrapText(string(data))
		}
	}

	return acp.ToolCallContent{}, false
}

func textToolContent(text string, isError bool, escapeReadText bool) acp.ToolCallContent {
	if escapeReadText {
		text = markdownEscape(text)
	} else if isError {
		text = codeBlock(text)
	}

	return acp.ToolContent(acp.TextBlock(text))
}

func toolReferenceNames(raw map[string]any) string {
	references, _ := raw[keyToolReferences].([]any)

	names := make([]string, 0, len(references))
	for _, reference := range references {
		item, _ := reference.(map[string]any)
		if name := stringInput(item, keyToolNameSnake); name != "" {
			names = append(names, name)
		}
	}

	if len(names) == 0 {
		return "none"
	}

	return strings.Join(names, ", ")
}

func errorCodeMessage(raw map[string]any) string {
	text := "Error: " + stringInput(raw, keyErrorCode)
	if message := stringInput(raw, keyErrorMessage); message != "" {
		text += " - " + message
	}

	return text
}

func webSearchResultText(raw map[string]any) string {
	title := stringInput(raw, keyTitle)

	url := stringInput(raw, keyURL)
	if title == "" {
		return url
	}

	if url == "" {
		return title
	}

	return title + " (" + url + ")"
}

func diffToolResultContent(raw map[string]any) ([]acp.ToolCallContent, []acp.ToolCallLocation) {
	source := raw
	if nested, _ := raw[keyContent].(map[string]any); nested != nil {
		source = nested
	}

	filePath := stringInput(source, "filePath")
	patches, _ := source[keyStructuredPatch].([]any)

	if filePath == "" || len(patches) == 0 {
		return nil, nil
	}

	content := make([]acp.ToolCallContent, 0, len(patches))
	locations := make([]acp.ToolCallLocation, 0, len(patches))

	for _, item := range patches {
		patch, _ := item.(map[string]any)
		lines := stringSliceInput(patch, "lines")

		if len(lines) == 0 {
			continue
		}

		oldText, newText := structuredPatchText(lines)
		if oldText == "" && newText == "" {
			continue
		}

		line, _ := intInput(patch, keyNewStart)
		locations = append(locations, locationWithOptionalLine(filePath, line))

		if oldText != "" {
			content = append(content, acp.ToolDiffContent(filePath, newText, oldText))
		} else {
			content = append(content, acp.ToolDiffContent(filePath, newText))
		}
	}

	return content, locations
}

func structuredPatchText(lines []string) (string, string) {
	oldLines := make([]string, 0, len(lines))
	newLines := make([]string, 0, len(lines))

	for _, line := range lines {
		if line == "" {
			oldLines = append(oldLines, "")
			newLines = append(newLines, "")

			continue
		}

		switch line[0] {
		case '-':
			oldLines = append(oldLines, line[1:])
		case '+':
			newLines = append(newLines, line[1:])
		case ' ':
			oldLines = append(oldLines, line[1:])
			newLines = append(newLines, line[1:])
		default:
			oldLines = append(oldLines, line)
			newLines = append(newLines, line)
		}
	}

	return strings.Join(oldLines, "\n"), strings.Join(newLines, "\n")
}

func bashOutputAndExit(block claude.ToolResultBlock) (string, *int) {
	if rawContent, ok := block.Raw[keyContent].(map[string]any); ok {
		if stringInput(rawContent, keyType) == toolResultBashCodeExecution {
			output := strings.Join(nonEmptyStrings(
				stringInput(rawContent, keyStdout),
				stringInput(rawContent, keyStderr),
			), "\n")
			if code, ok := intInput(rawContent, keyReturnCode); ok {
				return output, &code
			}

			return output, nil
		}
	}

	return toolResultText(block), nil
}

func toolResultText(block claude.ToolResultBlock) string {
	if text, ok := block.Raw[keyContent].(string); ok {
		return text
	}

	if values, ok := block.Raw[keyContent].([]any); ok {
		parts := make([]string, 0, len(values))
		for _, value := range values {
			raw, _ := value.(map[string]any)
			if text := stringInput(raw, keyText); text != "" {
				parts = append(parts, text)
			}
		}

		return strings.Join(parts, "\n")
	}

	parts := make([]string, 0, len(block.Content))
	for _, content := range block.Content {
		if text, ok := content.(claude.TextBlock); ok && text.Text != "" {
			parts = append(parts, text.Text)
		}
	}

	return strings.Join(parts, "\n")
}

func markdownEscape(text string) string {
	fence := "```"

	for line := range strings.SplitSeq(text, "\n") {
		trimmed := strings.TrimLeft(line, "`")
		if len(line)-len(trimmed) >= len(fence) {
			fence += "`"
		}
	}

	return fence + "\n" + text + trailingNewline(text) + fence
}

func consoleBlock(output string) string {
	return "```console\n" + strings.TrimRight(output, "\n") + "\n```"
}

func codeBlock(text string) string {
	return "```\n" + text + "\n```"
}

func trailingNewline(text string) string {
	if strings.HasSuffix(text, "\n") {
		return ""
	}

	return "\n"
}

func locations(input map[string]any) []acp.ToolCallLocation {
	if input == nil {
		return nil
	}

	filePath := stringInput(input, keyFilePath)
	path := stringInput(input, keyPath)

	if filePath != "" {
		line, _ := intInput(input, keyOffset)

		out := []acp.ToolCallLocation{locationWithOptionalLine(filePath, line)}
		if path != "" && path != filePath {
			out = append(out, locationWithOptionalLine(path, 0))
		}

		return out
	}

	if path == "" {
		return nil
	}

	return []acp.ToolCallLocation{locationWithOptionalLine(path, 0)}
}

func locationWithOptionalLine(path string, line int) acp.ToolCallLocation {
	location := acp.ToolCallLocation{Path: path}
	if line > 0 {
		location.Line = &line
	}

	return location
}

func displayPathForCwd(path string, cwd string) string {
	if cwd == "" || path == "" || !filepath.IsAbs(path) {
		return path
	}

	cleanCwd, _ := filepath.Abs(cwd)
	cleanPath, _ := filepath.Abs(path)

	rel, err := filepath.Rel(cleanCwd, cleanPath)
	if err != nil || rel == "." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) || rel == ".." {
		return path
	}

	return rel
}

func toolMeta(toolName string, toolResponse map[string]any) map[string]any {
	claudeMeta := map[string]any{}
	if toolName != "" {
		claudeMeta[keyToolName] = toolName
	}

	if toolResponse != nil {
		claudeMeta[keyToolResponse] = toolResponse
	}

	if len(claudeMeta) == 0 {
		return nil
	}

	return map[string]any{keyClaude: claudeMeta}
}

func withParentToolUseID(update acp.SessionUpdate, parentToolUseID string) acp.SessionUpdate {
	if parentToolUseID == "" {
		return update
	}

	setParentToolUseID(updateMeta(update), parentToolUseID)

	return update
}

func updateMeta(update acp.SessionUpdate) map[string]any {
	switch {
	case update.UserMessageChunk != nil:
		if update.UserMessageChunk.Meta == nil {
			update.UserMessageChunk.Meta = make(map[string]any)
		}

		return update.UserMessageChunk.Meta
	case update.AgentMessageChunk != nil:
		if update.AgentMessageChunk.Meta == nil {
			update.AgentMessageChunk.Meta = make(map[string]any)
		}

		return update.AgentMessageChunk.Meta
	case update.AgentThoughtChunk != nil:
		if update.AgentThoughtChunk.Meta == nil {
			update.AgentThoughtChunk.Meta = make(map[string]any)
		}

		return update.AgentThoughtChunk.Meta
	case update.ToolCall != nil:
		if update.ToolCall.Meta == nil {
			update.ToolCall.Meta = make(map[string]any)
		}

		return update.ToolCall.Meta
	case update.ToolCallUpdate != nil:
		if update.ToolCallUpdate.Meta == nil {
			update.ToolCallUpdate.Meta = make(map[string]any)
		}

		return update.ToolCallUpdate.Meta
	case update.Plan != nil:
		if update.Plan.Meta == nil {
			update.Plan.Meta = make(map[string]any)
		}

		return update.Plan.Meta
	default:
		return nil
	}
}

func setParentToolUseID(meta map[string]any, parentToolUseID string) {
	if meta == nil {
		return
	}

	claudeMeta, _ := meta[keyClaude].(map[string]any)
	if claudeMeta == nil {
		claudeMeta = make(map[string]any)
		meta[keyClaude] = claudeMeta
	}

	claudeMeta["parentToolUseId"] = parentToolUseID
}

func setAssistantMessageID(meta map[string]any, messageID string) {
	if meta == nil || messageID == "" {
		return
	}

	claudeMeta, _ := meta[keyClaude].(map[string]any)
	if claudeMeta == nil {
		claudeMeta = make(map[string]any)
		meta[keyClaude] = claudeMeta
	}

	claudeMeta[keyMessageID] = messageID
}

func imageData(raw map[string]any) (string, string) {
	if raw == nil {
		return "", ""
	}

	if data := stringInput(raw, keyData); data != "" {
		return data, stringInput(raw, keyMediaType)
	}

	source, _ := raw[keySource].(map[string]any)

	return stringInput(source, keyData), stringInput(source, keyMediaType)
}

func imageContentBlock(raw map[string]any) (acp.ContentBlock, bool) {
	if raw == nil {
		return acp.ContentBlock{}, false
	}

	data, mimeType := imageData(raw)
	uri := imageURI(raw)

	if data != "" {
		block := acp.ImageBlock(data, mimeType)
		if uri != "" {
			block.Image.Uri = &uri
		}

		return block, true
	}

	if uri == "" {
		return acp.ContentBlock{}, false
	}

	if parsed, err := neturl.Parse(uri); err == nil && (parsed.Scheme == typeHTTP || parsed.Scheme == "https") {
		name := pathpkg.Base(parsed.Path)
		if name == "." || name == "/" || name == "" {
			name = typeImage
		}

		return acp.ResourceLinkBlock(name, uri), true
	}

	return acp.ContentBlock{
		Image: &acp.ContentBlockImage{
			Type:     typeImage,
			MimeType: mimeType,
			Uri:      &uri,
		},
	}, true
}

func imageURI(raw map[string]any) string {
	for _, key := range []string{keyURI, keyURL, keyPath, keyFilePath} {
		if value := stringInput(raw, key); value != "" {
			return value
		}
	}

	source, _ := raw[keySource].(map[string]any)
	for _, key := range []string{keyURI, keyURL, keyPath, keyFilePath} {
		if value := stringInput(source, key); value != "" {
			return value
		}
	}

	return ""
}

func stringInput(input map[string]any, key string) string {
	if input == nil {
		return ""
	}

	value, _ := input[key].(string)

	return value
}

func mapInput(input map[string]any, key string) (map[string]any, bool) {
	if input == nil {
		return nil, false
	}

	raw, exists := input[key]
	if !exists {
		return nil, false
	}

	value, ok := raw.(map[string]any)

	return value, ok
}

func intInput(input map[string]any, key string) (int, bool) {
	if input == nil {
		return 0, false
	}

	switch value := input[key].(type) {
	case int:
		return value, true
	case int64:
		return int(value), true
	case float64:
		if math.Trunc(value) != value {
			return 0, false
		}

		return int(value), true
	default:
		return 0, false
	}
}

func boolInput(input map[string]any, key string) bool {
	if input == nil {
		return false
	}

	value, _ := input[key].(bool)

	return value
}

func stringSliceInput(input map[string]any, key string) []string {
	if input == nil {
		return nil
	}

	switch values := input[key].(type) {
	case []string:
		return append([]string(nil), values...)
	case []any:
		result := make([]string, 0, len(values))
		for _, value := range values {
			if text, ok := value.(string); ok {
				result = append(result, text)
			}
		}

		return result
	default:
		return nil
	}
}

func stringPtr(value string) *string {
	return &value
}

func nonEmptyStrings(values ...string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" {
			result = append(result, value)
		}
	}

	return result
}

func firstNonEmptyString(values ...string) string {
	for _, value := range values {
		if value != "" {
			return value
		}
	}

	return ""
}
