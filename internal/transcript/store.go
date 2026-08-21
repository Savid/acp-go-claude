package transcript

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
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/savid/acp-go-claude/internal/mapper"
)

const (
	projectsDirName  = "projects"
	maxReplayUpdates = 10000
	maxTitleLength   = 256

	// replaySource names what replay read, for the log line that reports rows it
	// could not decode.
	replaySource = "session store"

	entryTypeAssistant = "assistant"
	entryTypeAITitle   = "ai-title"
	entryTypeResult    = "result"
	entryTypeUser      = "user"

	contentTypeDocument   = "document"
	contentTypeImage      = "image"
	contentTypeText       = "text"
	contentTypeToolResult = "tool_result"

	keyAITitle          = "aiTitle"
	keyContent          = "content"
	keyCwd              = "cwd"
	keyCustomTitle      = "customTitle"
	keyData             = "data"
	keyError            = "error"
	keyIsCompactSummary = "isCompactSummary"
	keyIsError          = "is_error"
	keyIsMeta           = "isMeta"
	keyIsSidechain      = "isSidechain"
	keyMediaType        = "media_type"
	keyMessage          = "message"
	keyMimeType         = "mime_type"
	keySource           = "source"
	keySummary          = "summary"
	keyToolUseID        = "tool_use_id"
	keyType             = "type"
)

var (
	sessionFilePattern     = regexp.MustCompile(`^[0-9a-fA-F-]{36}\.jsonl$`)
	localCommandTagPattern = regexp.MustCompile(`(?s)<command-name>.*?</command-name>|<command-message>.*?</command-message>|<command-args>.*?</command-args>|<local-command-stdout>.*?</local-command-stdout>|<local-command-stderr>.*?</local-command-stderr>`)
)

var errReplayTruncated = errors.New("transcript replay truncated")

var (
	storeAbs         = filepath.Abs
	storeOpen        = func(path string) (transcriptFile, error) { return os.Open(path) }
	storeUserHomeDir = os.UserHomeDir
)

type transcriptFile interface {
	io.Closer
	io.Reader
}

// Store reads Claude Code's local transcript files.
type Store struct {
	ClaudeHome string
}

// Session contains transcript metadata and replayable updates.
type Session struct {
	Info acp.SessionInfo
	Path string
}

// List returns local transcript-backed Claude sessions.
func (s Store) List(ctx context.Context, cwd *string, additionalDirs []string) ([]Session, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if len(additionalDirs) > 0 {
		return nil, nil
	}

	if cwd != nil && strings.TrimSpace(*cwd) != "" {
		return s.listForCwd(ctx, *cwd)
	}

	return s.listAll(ctx)
}

// Find returns metadata for a single session ID.
func (s Store) Find(ctx context.Context, sessionID string, cwd string) (*Session, error) {
	sessions, err := s.List(ctx, nonEmptyStringPtr(cwd), nil)
	if err != nil {
		return nil, err
	}

	for _, session := range sessions {
		if string(session.Info.SessionId) == sessionID {
			return &session, nil
		}
	}

	return nil, os.ErrNotExist
}

// ReplayEntries converts store-authoritative transcript rows into ACP session
// updates. The rows are the session store's own and already in memory, so
// replay opens nothing and the only outcome besides the updates is whether the
// cap cut them short.
func ReplayEntries(entries []json.RawMessage) ([]acp.SessionUpdate, bool) {
	var transcript bytes.Buffer

	for _, entry := range entries {
		trimmed := bytes.TrimSpace(entry)
		if len(trimmed) == 0 {
			continue
		}

		transcript.Write(trimmed)
		transcript.WriteByte('\n')
	}

	return replayUpdates(transcript.Bytes())
}

// replayUpdates reads the rows twice: once to learn every tool use they carry,
// so a result may precede the use it answers, and once to emit. Neither pass
// reports a read failure, because held bytes have none, and the only refusal
// the emitting handler raises is the cap the second result reports. A row is a
// whole stored value rather than a file tail, so a row that will not decode is
// counted and skipped and never treated as torn.
func replayUpdates(rows []byte) ([]acp.SessionUpdate, bool) {
	transcriptToolUses := collectTranscriptToolUses(rows)

	var updates []acp.SessionUpdate

	truncated := false

	options := mapper.ToolUpdateOptions{
		ReplayAssistantIdentity: true,
		ToolUses:                make(map[string]claude.ToolUseBlock),
	}

	skippedLines := 0

	_ = readTranscriptLines(bytes.NewReader(rows), func(line transcriptLine) error {
		entry, skipped := decodeLine(line.Text)
		if skipped {
			skippedLines++
		}

		if entry == nil {
			return nil
		}

		if cwd := stringField(entry, keyCwd); cwd != "" {
			options.Cwd = cwd
		}

		entryUpdates := entryUpdatesWithOptions(entry, options, transcriptToolUses)
		if len(updates)+len(entryUpdates) > maxReplayUpdates {
			remaining := maxReplayUpdates - len(updates)
			if remaining > 0 {
				updates = append(updates, entryUpdates[:remaining]...)
			}

			truncated = true

			return errReplayTruncated
		}

		updates = append(updates, entryUpdates...)

		return nil
	})

	logSkippedTranscriptLines(replaySource, skippedLines)

	return updates, truncated
}

func collectTranscriptToolUses(rows []byte) map[string]claude.ToolUseBlock {
	toolUses := make(map[string]claude.ToolUseBlock)

	_ = readTranscriptLines(bytes.NewReader(rows), func(line transcriptLine) error {
		entry, ok := decodeLineQuiet(line.Text)
		if !ok {
			return nil
		}

		collectEntryToolUses(entry, toolUses)

		return nil
	})

	return toolUses
}

func decodeLineQuiet(line string) (map[string]any, bool) {
	line = strings.TrimSpace(line)
	if line == "" || line[0] != '{' {
		return nil, false
	}

	var entry map[string]any
	if err := json.Unmarshal([]byte(line), &entry); err != nil {
		return nil, false
	}

	return entry, true
}

func collectEntryToolUses(entry map[string]any, toolUses map[string]claude.ToolUseBlock) {
	if stringField(entry, keyType) != entryTypeAssistant || !visibleEntry(entry) {
		return
	}

	msg, err := claude.ParseMessage(entry)
	if err != nil {
		return
	}

	assistant := mustAssistantMessage(msg)

	for _, block := range assistant.Content {
		toolUse, ok := block.(claude.ToolUseBlock)
		if !ok || toolUse.ID == "" {
			continue
		}

		toolUses[toolUse.ID] = toolUse
	}
}

func mustAssistantMessage(msg claude.Message) *claude.AssistantMessage {
	switch typed := msg.(type) {
	case *claude.AssistantMessage:
		return typed
	default:
		panic("assistant transcript entry parsed as non-assistant message")
	}
}

func (s Store) listForCwd(ctx context.Context, cwd string) ([]Session, error) {
	canonical := canonicalPath(cwd)
	exact := filepath.Join(s.projectsDir(), ProjectDirName(canonical))

	var sessions []Session

	if info, err := os.Stat(exact); err == nil && info.IsDir() {
		found, err := s.readSessionsFromDir(ctx, exact, canonical)
		if err != nil {
			return nil, err
		}

		for _, session := range found {
			if canonicalPath(session.Info.Cwd) == canonical {
				sessions = append(sessions, session)
			}
		}
	}

	if len(sessions) == 0 {
		all, err := s.listAll(ctx)
		if err != nil {
			return nil, err
		}

		for _, session := range all {
			if canonicalPath(session.Info.Cwd) == canonical {
				sessions = append(sessions, session)
			}
		}
	}

	return sortAndDedupe(sessions), nil
}

func (s Store) listAll(ctx context.Context) ([]Session, error) {
	entries, err := os.ReadDir(s.projectsDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("read Claude projects dir: %w", err)
	}

	var sessions []Session

	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		if !entry.IsDir() {
			continue
		}

		found, err := s.readSessionsFromDir(ctx, filepath.Join(s.projectsDir(), entry.Name()), "")
		if err != nil {
			return nil, err
		}

		sessions = append(sessions, found...)
	}

	return sortAndDedupe(sessions), nil
}

func (s Store) readSessionsFromDir(ctx context.Context, dir string, fallbackCwd string) ([]Session, error) {
	entries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) || errors.Is(err, os.ErrPermission) {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("read transcript dir: %w", err)
	}

	sessions := make([]Session, 0, len(entries))
	for _, entry := range entries {
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		if entry.IsDir() || !sessionFilePattern.MatchString(entry.Name()) {
			continue
		}

		path := filepath.Join(dir, entry.Name())

		session, err := readSession(path, fallbackCwd)
		if errors.Is(err, errNoVisibleTranscript) || errors.Is(err, os.ErrPermission) {
			continue
		}

		if err != nil {
			return nil, err
		}

		sessions = append(sessions, *session)
	}

	return sessions, nil
}

func (s Store) configHome() string {
	if strings.TrimSpace(s.ClaudeHome) != "" {
		return filepath.Clean(s.ClaudeHome)
	}

	if configDir := strings.TrimSpace(os.Getenv("CLAUDE_CONFIG_DIR")); configDir != "" {
		return filepath.Clean(configDir)
	}

	home, err := storeUserHomeDir()
	if err != nil {
		return filepath.Clean(".claude")
	}

	return filepath.Join(home, ".claude")
}

func (s Store) projectsDir() string {
	return filepath.Join(s.configHome(), projectsDirName)
}

var errNoVisibleTranscript = errors.New("transcript has no visible session content")

func readSession(path string, fallbackCwd string) (*Session, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, fmt.Errorf("stat transcript: %w", err)
	}

	file, err := storeOpen(path)
	if err != nil {
		return nil, fmt.Errorf("open transcript: %w", err)
	}

	defer file.Close()

	sessionID := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	session := Session{
		Path: path,
		Info: acp.SessionInfo{
			SessionId: acp.SessionId(sessionID),
		},
	}

	updatedAt := info.ModTime().UTC().Format(time.RFC3339)
	session.Info.UpdatedAt = &updatedAt

	title := ""
	firstPrompt := ""
	cwd := ""
	hasVisible := false

	skippedLines := 0

	err = readTranscriptLines(file, func(line transcriptLine) error {
		entry, skipped := decodeLine(line.Text)
		if skipped {
			skippedLines++

			if line.Final {
				logTornTranscriptLine(path, line.Offset)
			}
		}

		if entry == nil {
			return nil
		}

		if entryTitle := titleFromEntry(entry); entryTitle != "" {
			title = entryTitle
		}

		if cwd == "" {
			cwd = stringField(entry, keyCwd)
		}

		if visibleEntry(entry) {
			hasVisible = true
		}

		if firstPrompt == "" && stringField(entry, keyType) == entryTypeUser && visibleEntry(entry) {
			firstPrompt = firstUserPrompt(entry)
		}

		return nil
	})

	logSkippedTranscriptLines(path, skippedLines)

	if err != nil {
		return nil, fmt.Errorf("read transcript: %w", err)
	}

	if !hasVisible {
		return nil, errNoVisibleTranscript
	}

	if title == "" {
		title = firstPrompt
	}

	if title == "" {
		title = sessionID
	}

	title = sanitizeTitle(title)

	if cwd == "" {
		cwd = fallbackCwd
	}

	session.Info.Title = &title
	session.Info.Cwd = cwd

	return &session, nil
}

type transcriptLine struct {
	Text   string
	Offset int64
	Final  bool
}

func readTranscriptLines(reader io.Reader, handle func(transcriptLine) error) error {
	buffered := bufio.NewReader(reader)
	offset := int64(0)

	for {
		line, err := buffered.ReadString('\n')
		if line != "" {
			lineInfo := transcriptLine{
				Text:   line,
				Offset: offset,
				Final:  errors.Is(err, io.EOF),
			}
			if handleErr := handle(lineInfo); handleErr != nil {
				return handleErr
			}

			offset += int64(len(line))
		}

		if err == nil {
			continue
		}

		if errors.Is(err, io.EOF) {
			return nil
		}

		return err
	}
}

func decodeLine(line string) (map[string]any, bool) {
	line = strings.TrimSpace(line)
	if line == "" || line[0] != '{' {
		return nil, false
	}

	var entry map[string]any
	if err := json.Unmarshal([]byte(line), &entry); err != nil {
		slog.Default().Debug("skip invalid Claude transcript line", slog.String("stage", "transcript_decode"))

		return nil, true
	}

	return entry, false
}

func logSkippedTranscriptLines(_ string, count int) {
	if count == 0 {
		return
	}

	slog.Default().Warn("skipped malformed Claude transcript lines", slog.Int("count", count))
}

func logTornTranscriptLine(_ string, _ int64) {
	slog.Default().Warn("skipped torn Claude transcript final line", slog.String("stage", "transcript_final_line"))
}

func visibleEntry(entry map[string]any) bool {
	switch stringField(entry, keyType) {
	case entryTypeUser, entryTypeAssistant:
	default:
		return false
	}

	if boolField(entry, keyIsMeta) || boolField(entry, keyIsSidechain) || boolField(entry, keyIsCompactSummary) {
		return false
	}

	if stringField(entry, keyType) == entryTypeUser {
		return visibleUserEntry(entry)
	}

	return true
}

func visibleUserEntry(entry map[string]any) bool {
	message, _ := entry[keyMessage].(map[string]any)
	if message == nil {
		return false
	}

	switch content := message[keyContent].(type) {
	case string:
		return strings.TrimSpace(stripLocalCommandMetadata(content)) != ""
	case []any:
		for _, item := range content {
			block, _ := item.(map[string]any)
			if visibleUserBlock(block) {
				return true
			}
		}
	}

	return false
}

func visibleUserBlock(block map[string]any) bool {
	switch stringField(block, keyType) {
	case contentTypeText:
		return strings.TrimSpace(stripLocalCommandMetadata(stringField(block, contentTypeText))) != ""
	case contentTypeImage, contentTypeDocument:
		data, mimeType := sourceData(block)

		return data != "" && mimeType != ""
	case contentTypeToolResult:
		return stringField(block, keyToolUseID) != ""
	default:
		return false
	}
}

func entryUpdatesWithOptions(
	entry map[string]any,
	options mapper.ToolUpdateOptions,
	transcriptToolUses ...map[string]claude.ToolUseBlock,
) []acp.SessionUpdate {
	switch stringField(entry, keyType) {
	case entryTypeAITitle:
		title := stringField(entry, keyAITitle)
		if title == "" {
			return nil
		}

		return []acp.SessionUpdate{{SessionInfoUpdate: &acp.SessionSessionInfoUpdate{Title: &title}}}
	case entryTypeUser:
		if !visibleEntry(entry) {
			return nil
		}

		return userUpdatesWithOptions(entry, options, transcriptToolUses...)
	case entryTypeAssistant:
		if !visibleEntry(entry) {
			return nil
		}

		msg, err := claude.ParseMessage(entry)
		if err != nil {
			return nil
		}

		return mapper.MessageToUpdatesWithOptions(msg, options)
	case entryTypeResult:
		msg, _ := claude.ParseMessage(entry)
		result, _ := msg.(*claude.ResultMessage)

		return mapper.UsageUpdate(result)
	default:
		return nil
	}
}

func userUpdatesWithOptions(
	entry map[string]any,
	options mapper.ToolUpdateOptions,
	transcriptToolUses ...map[string]claude.ToolUseBlock,
) []acp.SessionUpdate {
	message, _ := entry[keyMessage].(map[string]any)
	if message == nil {
		return nil
	}

	switch content := message[keyContent].(type) {
	case string:
		content = stripLocalCommandMetadata(content)
		if strings.TrimSpace(content) == "" {
			return nil
		}

		return []acp.SessionUpdate{acp.UpdateUserMessageText(content)}
	case []any:
		updates := make([]acp.SessionUpdate, 0, len(content))
		for _, item := range content {
			block, _ := item.(map[string]any)
			if block == nil {
				continue
			}

			updates = append(updates, userContentUpdates(block, options, transcriptToolUses...)...)
		}

		return updates
	default:
		return nil
	}
}

func userContentUpdates(
	block map[string]any,
	options mapper.ToolUpdateOptions,
	transcriptToolUses ...map[string]claude.ToolUseBlock,
) []acp.SessionUpdate {
	switch stringField(block, keyType) {
	case contentTypeText:
		text := stripLocalCommandMetadata(stringField(block, contentTypeText))
		if strings.TrimSpace(text) == "" {
			return nil
		}

		return []acp.SessionUpdate{acp.UpdateUserMessage(acp.TextBlock(text))}
	case contentTypeImage, contentTypeDocument:
		content, ok := transcriptContentBlock(block)
		if !ok {
			return nil
		}

		return []acp.SessionUpdate{acp.UpdateUserMessage(content)}
	case contentTypeToolResult:
		return toolResultUpdates(block, options, transcriptToolUses...)
	default:
		return nil
	}
}

func stripLocalCommandMetadata(text string) string {
	return localCommandTagPattern.ReplaceAllString(text, "")
}

func transcriptContentBlock(block map[string]any) (acp.ContentBlock, bool) {
	switch stringField(block, keyType) {
	case contentTypeText:
		text := stringField(block, contentTypeText)
		if strings.TrimSpace(text) == "" {
			return acp.ContentBlock{}, false
		}

		return acp.TextBlock(text), true
	case contentTypeImage:
		data, mimeType := sourceData(block)
		if data == "" || mimeType == "" {
			return acp.ContentBlock{}, false
		}

		return acp.ImageBlock(data, mimeType), true
	case contentTypeDocument:
		data, mimeType := sourceData(block)
		if data == "" || mimeType == "" {
			return acp.ContentBlock{}, false
		}

		return acp.ResourceBlock(acp.EmbeddedResourceResource{
			BlobResourceContents: &acp.BlobResourceContents{
				Blob:     data,
				MimeType: &mimeType,
				Uri:      "claude-transcript:document",
			},
		}), true
	default:
		return acp.ContentBlock{}, false
	}
}

func toolResultUpdates(
	block map[string]any,
	options mapper.ToolUpdateOptions,
	transcriptToolUses ...map[string]claude.ToolUseBlock,
) []acp.SessionUpdate {
	toolUseID := stringField(block, keyToolUseID)
	if toolUseID == "" {
		return nil
	}

	if updates := mappedToolResultUpdates(block, options); len(updates) > 0 {
		return updates
	}

	for _, toolUses := range transcriptToolUses {
		toolUse, known := toolUses[toolUseID]
		if !known {
			continue
		}

		replayOptions := options
		replayOptions.ToolUses = map[string]claude.ToolUseBlock{toolUseID: toolUse}

		if updates := mappedToolResultUpdates(block, replayOptions); len(updates) > 0 {
			return updates
		}
	}

	status := acp.ToolCallStatusCompleted
	if boolField(block, keyIsError) {
		status = acp.ToolCallStatusFailed
	}

	opts := []acp.ToolCallUpdateOpt{
		acp.WithUpdateStatus(status),
		acp.WithUpdateRawOutput(block),
	}

	if content := transcriptToolResultContent(block[keyContent]); len(content) > 0 {
		opts = append(opts, acp.WithUpdateContent(content))
	}

	return []acp.SessionUpdate{acp.UpdateToolCall(acp.ToolCallId(toolUseID), opts...)}
}

func mappedToolResultUpdates(block map[string]any, options mapper.ToolUpdateOptions) []acp.SessionUpdate {
	toolUseID := stringField(block, keyToolUseID)
	if toolUseID == "" {
		return nil
	}

	if _, known := options.ToolUses[toolUseID]; !known {
		return nil
	}

	parsed, _ := claude.ParseContentBlock(block).(claude.ToolResultBlock)
	if parsed.ToolUseID == "" {
		return nil
	}

	return mapper.MessageToUpdatesWithOptions(&claude.AssistantMessage{
		Content: []claude.ContentBlock{parsed},
	}, options)
}

func transcriptToolResultContent(raw any) []acp.ToolCallContent {
	switch content := raw.(type) {
	case string:
		if strings.TrimSpace(content) == "" {
			return nil
		}

		return []acp.ToolCallContent{acp.ToolContent(acp.TextBlock(content))}
	case []any:
		result := make([]acp.ToolCallContent, 0, len(content))
		for _, item := range content {
			block, _ := item.(map[string]any)
			if block == nil {
				continue
			}

			if contentBlock, ok := transcriptContentBlock(block); ok {
				result = append(result, acp.ToolContent(contentBlock))
			}
		}

		return result
	default:
		return nil
	}
}

func sourceData(block map[string]any) (string, string) {
	data := stringField(block, keyData)
	mimeType := stringField(block, keyMimeType)

	source, _ := block[keySource].(map[string]any)
	if source != nil {
		if data == "" {
			data = stringField(source, keyData)
		}

		if mimeType == "" {
			mimeType = stringField(source, keyMediaType)
		}
	}

	return data, mimeType
}

func titleFromEntry(entry map[string]any) string {
	switch stringField(entry, keyType) {
	case entryTypeAITitle:
		return stringField(entry, keyAITitle)
	default:
		if custom := stringField(entry, keyCustomTitle); custom != "" {
			return custom
		}

		return stringField(entry, keySummary)
	}
}

func firstUserPrompt(entry map[string]any) string {
	message, _ := entry[keyMessage].(map[string]any)
	if message == nil {
		return ""
	}

	switch content := message[keyContent].(type) {
	case string:
		text := stripLocalCommandMetadata(content)
		if strings.TrimSpace(text) != "" {
			return sanitizeTitle(text)
		}
	case []any:
		for _, item := range content {
			block, _ := item.(map[string]any)
			if stringField(block, keyType) != contentTypeText {
				continue
			}

			text := stripLocalCommandMetadata(stringField(block, contentTypeText))
			if strings.TrimSpace(text) == "" {
				continue
			}

			return sanitizeTitle(text)
		}
	}

	return ""
}

func sanitizeTitle(text string) string {
	text = strings.Join(strings.Fields(text), " ")

	runes := []rune(text)
	if len(runes) > maxTitleLength {
		return strings.TrimSpace(string(runes[:maxTitleLength-3])) + "..."
	}

	return text
}

func sortAndDedupe(sessions []Session) []Session {
	byID := make(map[acp.SessionId]Session, len(sessions))
	for _, session := range sessions {
		existing, ok := byID[session.Info.SessionId]
		if !ok || updatedAfter(session.Info.UpdatedAt, existing.Info.UpdatedAt) {
			byID[session.Info.SessionId] = session
		}
	}

	result := make([]Session, 0, len(byID))
	for _, session := range byID {
		result = append(result, session)
	}

	sort.Slice(result, func(i, j int) bool {
		return updatedAfter(result[i].Info.UpdatedAt, result[j].Info.UpdatedAt)
	})

	return result
}

func updatedAfter(left *string, right *string) bool {
	if left == nil {
		return false
	}

	if right == nil {
		return true
	}

	leftTime, leftErr := time.Parse(time.RFC3339, *left)
	rightTime, rightErr := time.Parse(time.RFC3339, *right)

	return leftErr == nil && rightErr == nil && leftTime.After(rightTime)
}

func canonicalPath(path string) string {
	if strings.TrimSpace(path) == "" {
		return ""
	}

	absolute, err := storeAbs(path)
	if err != nil {
		return filepath.Clean(path)
	}

	resolved, err := filepath.EvalSymlinks(absolute)
	if err == nil {
		return filepath.Clean(resolved)
	}

	return filepath.Clean(absolute)
}

func nonEmptyStringPtr(value string) *string {
	if strings.TrimSpace(value) == "" {
		return nil
	}

	return &value
}

func stringField(entry map[string]any, key string) string {
	value, _ := entry[key].(string)

	return value
}

func boolField(entry map[string]any, key string) bool {
	value, _ := entry[key].(bool)

	return value
}
