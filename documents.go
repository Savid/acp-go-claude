package claudeacp

import (
	"fmt"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/coder/acp-go-sdk"
)

const maxDocumentContextChars = 20000

type documentState struct {
	URI        string
	LanguageID string
	Text       string
	Version    int
	Saved      bool
}

func applyDocumentChanges(
	text string,
	changes []acp.UnstableTextDocumentContentChangeEvent,
	encoding acp.PositionEncodingKind,
) (string, error) {
	for _, change := range changes {
		if change.Range == nil {
			text = change.Text

			continue
		}

		next, err := replaceDocumentRange(text, *change.Range, change.Text, encoding)
		if err != nil {
			return "", err
		}

		text = next
	}

	return text, nil
}

func replaceDocumentRange(
	text string,
	changeRange acp.UnstableRange,
	replacement string,
	encoding acp.PositionEncodingKind,
) (string, error) {
	start, err := documentPositionOffset(text, changeRange.Start, encoding)
	if err != nil {
		return "", fmt.Errorf("invalid range start: %w", err)
	}

	end, err := documentPositionOffset(text, changeRange.End, encoding)
	if err != nil {
		return "", fmt.Errorf("invalid range end: %w", err)
	}

	if start > end {
		return "", fmt.Errorf("range start is after range end")
	}

	return text[:start] + replacement + text[end:], nil
}

func documentPositionOffset(text string, position acp.UnstablePosition, encoding acp.PositionEncodingKind) (int, error) {
	if position.Line < 0 || position.Character < 0 {
		return 0, fmt.Errorf("position must be non-negative")
	}

	offset := 0
	for line := 0; line < position.Line; line++ {
		newline := strings.IndexByte(text[offset:], '\n')
		if newline < 0 {
			return 0, fmt.Errorf("line %d is past end of document", position.Line)
		}

		offset += newline + 1
	}

	lineEnd := len(text)
	if newline := strings.IndexByte(text[offset:], '\n'); newline >= 0 {
		lineEnd = offset + newline
	}

	lineText := text[offset:lineEnd]

	switch encoding {
	case acp.PositionEncodingKindUtf8:
		return documentPositionOffsetUTF8(lineText, offset, position.Character)
	case acp.PositionEncodingKindUtf32:
		return documentPositionOffsetUTF32(lineText, offset, position.Character)
	case acp.PositionEncodingKindUtf16:
		return documentPositionOffsetUTF16(lineText, offset, position.Character)
	default:
		return 0, fmt.Errorf("unsupported position encoding %q", encoding)
	}
}

func documentPositionOffsetUTF8(lineText string, lineOffset int, character int) (int, error) {
	if character > len(lineText) {
		return 0, fmt.Errorf("character %d is past end of line", character)
	}

	if !utf8.ValidString(lineText[:character]) {
		return 0, fmt.Errorf("character %d is not a UTF-8 boundary", character)
	}

	return lineOffset + character, nil
}

func documentPositionOffsetUTF16(lineText string, lineOffset int, character int) (int, error) {
	units := 0
	for byteIndex, r := range lineText {
		if units == character {
			return lineOffset + byteIndex, nil
		}

		units++
		if r > 0xFFFF {
			units++
		}

		if units > character {
			return 0, fmt.Errorf("character %d splits a UTF-16 surrogate pair", character)
		}
	}

	if units == character {
		return lineOffset + len(lineText), nil
	}

	return 0, fmt.Errorf("character %d is past end of line", character)
}

func documentPositionOffsetUTF32(lineText string, lineOffset int, character int) (int, error) {
	lineRunes := []rune(lineText)
	if character > len(lineRunes) {
		return 0, fmt.Errorf("character %d is past end of line", character)
	}

	return lineOffset + len(string(lineRunes[:character])), nil
}

func documentContextText(documents map[string]documentState, focusedURI string) string {
	selected := selectDocumentContext(documents, focusedURI)
	if len(selected) == 0 {
		return ""
	}

	var builder strings.Builder
	builder.WriteString("ACP editor context. Use this only when it is relevant to the user's request.")

	for _, document := range selected {
		builder.WriteString("\n\nDocument: ")
		builder.WriteString(document.URI)

		if document.LanguageID != "" {
			builder.WriteString("\nLanguage: ")
			builder.WriteString(document.LanguageID)
		}

		builder.WriteString("\nVersion: ")
		fmt.Fprintf(&builder, "%d", document.Version)

		if document.Saved {
			builder.WriteString("\nSaved: true")
		}

		builder.WriteString("\nContent:\n")
		builder.WriteString(truncateDocumentText(document.Text))
	}

	return builder.String()
}

func selectDocumentContext(documents map[string]documentState, focusedURI string) []documentState {
	if len(documents) == 0 {
		return nil
	}

	if focusedURI != "" {
		if document, ok := documents[focusedURI]; ok {
			return []documentState{document}
		}
	}

	uris := make([]string, 0, len(documents))
	for uri := range documents {
		uris = append(uris, uri)
	}

	sort.Strings(uris)

	selected := make([]documentState, 0, len(uris))
	for _, uri := range uris {
		selected = append(selected, documents[uri])
	}

	return selected
}

func truncateDocumentText(text string) string {
	runes := []rune(text)
	if len(runes) <= maxDocumentContextChars {
		return text
	}

	return string(runes[:maxDocumentContextChars]) + "\n[truncated]"
}
