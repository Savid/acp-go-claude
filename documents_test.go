package claudeacp

import (
	"strings"
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/require"
)

func TestApplyDocumentChanges(t *testing.T) {
	t.Parallel()

	text, err := applyDocumentChanges(
		"one\ntwo\n",
		[]acp.UnstableTextDocumentContentChangeEvent{
			{Range: nil, Text: "alpha\nbeta\n"},
			{
				Range: &acp.UnstableRange{
					Start: acp.UnstablePosition{Line: 1, Character: 0},
					End:   acp.UnstablePosition{Line: 1, Character: 4},
				},
				Text: "gamma",
			},
		},
		acp.PositionEncodingKindUtf16,
	)
	require.NoError(t, err)
	require.Equal(t, "alpha\ngamma\n", text)
}

func TestApplyDocumentChangesErrors(t *testing.T) {
	t.Parallel()

	_, err := applyDocumentChanges(
		"one",
		[]acp.UnstableTextDocumentContentChangeEvent{
			{
				Range: &acp.UnstableRange{
					Start: acp.UnstablePosition{Line: 1, Character: 0},
					End:   acp.UnstablePosition{Line: 1, Character: 1},
				},
				Text: "two",
			},
		},
		acp.PositionEncodingKindUtf16,
	)
	require.Error(t, err)

	_, err = replaceDocumentRange("one", acp.UnstableRange{
		Start: acp.UnstablePosition{Line: 0, Character: 2},
		End:   acp.UnstablePosition{Line: 0, Character: 1},
	}, "", acp.PositionEncodingKindUtf16)
	require.Error(t, err)

	_, err = replaceDocumentRange("one", acp.UnstableRange{
		Start: acp.UnstablePosition{Line: 0, Character: 0},
		End:   acp.UnstablePosition{Line: 2, Character: 0},
	}, "", acp.PositionEncodingKindUtf16)
	require.Error(t, err)
}

func TestDocumentPositionOffsetEncodings(t *testing.T) {
	t.Parallel()

	text := "a😀b\n"
	replacementRange := acp.UnstableRange{
		Start: acp.UnstablePosition{Line: 0, Character: 1},
		End:   acp.UnstablePosition{Line: 0, Character: 3},
	}

	replaced, err := replaceDocumentRange(text, replacementRange, "x", acp.PositionEncodingKindUtf16)
	require.NoError(t, err)
	require.Equal(t, "axb\n", replaced)

	replacementRange.End.Character = 2
	replaced, err = replaceDocumentRange(text, replacementRange, "x", acp.PositionEncodingKindUtf32)
	require.NoError(t, err)
	require.Equal(t, "axb\n", replaced)

	replacementRange.End.Character = 5
	replaced, err = replaceDocumentRange(text, replacementRange, "x", acp.PositionEncodingKindUtf8)
	require.NoError(t, err)
	require.Equal(t, "axb\n", replaced)

	_, err = documentPositionOffset(text, acp.UnstablePosition{Line: 0, Character: 2}, acp.PositionEncodingKindUtf16)
	require.Error(t, err)

	_, err = documentPositionOffset(text, acp.UnstablePosition{Line: 0, Character: 2}, acp.PositionEncodingKindUtf8)
	require.Error(t, err)

	_, err = documentPositionOffset(text, acp.UnstablePosition{Line: 0, Character: 99}, acp.PositionEncodingKindUtf8)
	require.Error(t, err)

	_, err = documentPositionOffset(text, acp.UnstablePosition{Line: -1, Character: 0}, acp.PositionEncodingKindUtf16)
	require.Error(t, err)

	_, err = documentPositionOffset(text, acp.UnstablePosition{Line: 0, Character: 99}, acp.PositionEncodingKindUtf32)
	require.Error(t, err)

	_, err = documentPositionOffset(text, acp.UnstablePosition{Line: 0, Character: 0}, "unknown")
	require.Error(t, err)

	offset, err := documentPositionOffset(text, acp.UnstablePosition{Line: 0, Character: 4}, acp.PositionEncodingKindUtf16)
	require.NoError(t, err)
	require.Equal(t, len("a😀b"), offset)

	_, err = documentPositionOffset(text, acp.UnstablePosition{Line: 0, Character: 99}, acp.PositionEncodingKindUtf16)
	require.Error(t, err)
}

func TestDocumentContextText(t *testing.T) {
	t.Parallel()

	contextText := documentContextText(map[string]documentState{
		"file:///b.go": {URI: "file:///b.go", LanguageID: "go", Text: "package b", Version: 2},
		"file:///a.go": {URI: "file:///a.go", LanguageID: "go", Text: strings.Repeat("a", maxDocumentContextChars+1), Version: 1, Saved: true},
	}, "file:///a.go")

	require.Contains(t, contextText, "Document: file:///a.go")
	require.NotContains(t, contextText, "file:///b.go")
	require.Contains(t, contextText, "Saved: true")
	require.Contains(t, contextText, "[truncated]")

	contextText = documentContextText(map[string]documentState{
		"file:///b.go": {URI: "file:///b.go", Text: "b"},
		"file:///a.go": {URI: "file:///a.go", Text: "a"},
	}, "")
	require.Less(t, strings.Index(contextText, "file:///a.go"), strings.Index(contextText, "file:///b.go"))
	require.Empty(t, documentContextText(nil, ""))
}
