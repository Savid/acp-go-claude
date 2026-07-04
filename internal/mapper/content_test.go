package mapper

import (
	"testing"

	"github.com/coder/acp-go-sdk"
	"github.com/stretchr/testify/require"
)

func TestPromptToClaude(t *testing.T) {
	t.Parallel()

	blocks, err := PromptToClaude([]acp.ContentBlock{
		acp.TextBlock("hello"),
		acp.ImageBlock("abc", "image/png"),
		acp.ResourceLinkBlock("readme", "file:///tmp/README.md"),
	}, nil)

	require.NoError(t, err)
	require.Len(t, blocks, 3)
	require.Equal(t, map[string]any{"type": "text", "text": "hello"}, blocks[0])
	require.Equal(t, "image", blocks[1]["type"])
	require.Equal(t, map[string]any{
		"type":       "base64",
		"media_type": "image/png",
		"data":       "abc",
	}, blocks[1]["source"])
	require.Equal(t, "[@README.md](file:///tmp/README.md)", blocks[2]["text"])
}

func TestPromptToClaudeTextAudienceAnnotations(t *testing.T) {
	t.Parallel()

	blocks, err := PromptToClaude([]acp.ContentBlock{
		textBlockWithAudience("model-visible", acp.RoleAssistant),
		textBlockWithAudience("client-only", acp.RoleUser),
		textBlockWithAudience("mixed", acp.RoleUser, acp.RoleAssistant),
		textBlockWithAudience("empty"),
	}, nil)

	require.NoError(t, err)
	require.Equal(t, []map[string]any{
		{"type": "text", "text": "model-visible"},
		{"type": "text", "text": "mixed"},
		{"type": "text", "text": "empty"},
	}, blocks)
}

func textBlockWithAudience(text string, audience ...acp.Role) acp.ContentBlock {
	return acp.ContentBlock{Text: &acp.ContentBlockText{
		Type: "text",
		Text: text,
		Annotations: &acp.Annotations{
			Audience: audience,
		},
	}}
}

func TestPromptToClaudeRewritesAdvertisedMCPSlashCommands(t *testing.T) {
	t.Parallel()

	blocks, err := PromptToClaude([]acp.ContentBlock{
		acp.TextBlock("/mcp:server:name\targs"),
		acp.TextBlock("/mcp:server:name"),
	}, []acp.AvailableCommand{{Name: "mcp:server:name"}})

	require.NoError(t, err)
	require.Equal(t, "/server:name (MCP)\targs", blocks[0]["text"])
	require.Equal(t, "/server:name (MCP)", blocks[1]["text"])
}

func TestPromptToClaudeLeavesUnadvertisedMCPSlashTextByteIdentical(t *testing.T) {
	t.Parallel()

	blocks, err := PromptToClaude([]acp.ContentBlock{
		acp.TextBlock("/mcp:server:name args"),
		acp.TextBlock("/mcp:bad\tserver:name"),
		acp.TextBlock("/mcp:server"),
		acp.TextBlock("/mcp::name"),
		acp.TextBlock("/compact"),
		acp.TextBlock("prefix /mcp:server:name"),
	}, []acp.AvailableCommand{
		{Name: "mcp:other:name"},
		{Name: "mcp:server"},
		{Name: "mcp::name"},
		{Name: "mcp:bad server:name"},
	})

	require.NoError(t, err)
	require.Equal(t, "/mcp:server:name args", blocks[0]["text"])
	require.Equal(t, "/mcp:bad\tserver:name", blocks[1]["text"])
	require.Equal(t, "/mcp:server", blocks[2]["text"])
	require.Equal(t, "/mcp::name", blocks[3]["text"])
	require.Equal(t, "/compact", blocks[4]["text"])
	require.Equal(t, "prefix /mcp:server:name", blocks[5]["text"])
}

func TestPromptToClaudeEmbeddedResources(t *testing.T) {
	t.Parallel()

	pdfMime := "application/pdf"
	imageMime := "image/jpeg"
	uriImage := "https://example.com/image.png"
	blocks, err := PromptToClaude([]acp.ContentBlock{
		acp.ResourceBlock(acp.EmbeddedResourceResource{
			TextResourceContents: &acp.TextResourceContents{Uri: "file:///tmp/a.txt", Text: "body"},
		}),
		acp.ResourceBlock(acp.EmbeddedResourceResource{
			TextResourceContents: &acp.TextResourceContents{Text: "inline"},
		}),
		acp.ResourceBlock(acp.EmbeddedResourceResource{
			BlobResourceContents: &acp.BlobResourceContents{MimeType: &imageMime, Blob: "img"},
		}),
		acp.ResourceBlock(acp.EmbeddedResourceResource{
			BlobResourceContents: &acp.BlobResourceContents{MimeType: &pdfMime, Blob: "pdf"},
		}),
		{
			Image: &acp.ContentBlockImage{
				Type: "image",
				Uri:  &uriImage,
			},
		},
	}, nil)

	require.NoError(t, err)
	require.Len(t, blocks, 6)
	require.Equal(t, "[@a.txt](file:///tmp/a.txt)", blocks[0]["text"])
	require.Equal(t, "image", blocks[1]["type"])
	require.Equal(t, "document", blocks[2]["type"])
	require.Equal(t, "image", blocks[3]["type"])
	require.Equal(t, map[string]any{"type": "url", "url": uriImage}, blocks[3]["source"])
	require.Equal(t, "\n<context ref=\"file:///tmp/a.txt\">\nbody\n</context>", blocks[4]["text"])
	require.Equal(t, "\n<context ref=\"\">\ninline\n</context>", blocks[5]["text"])
}

func TestPromptToClaudeResourceLinks(t *testing.T) {
	t.Parallel()

	blocks, err := PromptToClaude([]acp.ContentBlock{
		acp.ResourceLinkBlock("ticket", "https://example.com/T-1"),
		acp.ResourceLinkBlock("", "%gh&%ij"),
		acp.ResourceLinkBlock("local", "file://localhost/tmp/a.txt"),
		acp.ResourceLinkBlock("drive", "file:///C:/repo/a.txt"),
		acp.ResourceLinkBlock("remote", "file://example.com/tmp/a.txt"),
		acp.ResourceLinkBlock("zed", "zed://workspace/file.go"),
	}, nil)

	require.NoError(t, err)
	require.Equal(t, "https://example.com/T-1", blocks[0]["text"])
	require.Equal(t, "%gh&%ij", blocks[1]["text"])
	require.Equal(t, "[@a.txt](file://localhost/tmp/a.txt)", blocks[2]["text"])
	require.Equal(t, "[@a.txt](file:///C:/repo/a.txt)", blocks[3]["text"])
	require.Equal(t, "[@a.txt](file://example.com/tmp/a.txt)", blocks[4]["text"])
	require.Equal(t, "[@file.go](zed://workspace/file.go)", blocks[5]["text"])
	require.Equal(t, "", linkName(""))
	require.Equal(t, "[@file://](file://)", formatURIAsLink("file://"))
}

func TestPromptToClaudeUnsupported(t *testing.T) {
	t.Parallel()

	_, err := PromptToClaude([]acp.ContentBlock{acp.AudioBlock("abc", "audio/wav")}, nil)
	require.Error(t, err)

	_, err = PromptToClaude([]acp.ContentBlock{{}}, nil)
	require.Error(t, err)

	_, err = PromptToClaude([]acp.ContentBlock{
		acp.ResourceBlock(acp.EmbeddedResourceResource{
			BlobResourceContents: &acp.BlobResourceContents{Blob: "bin"},
		}),
	}, nil)
	require.Error(t, err)

	_, err = PromptToClaude([]acp.ContentBlock{
		acp.ResourceBlock(acp.EmbeddedResourceResource{}),
	}, nil)
	require.Error(t, err)

	_, err = PromptToClaude([]acp.ContentBlock{{Image: &acp.ContentBlockImage{Type: "image"}}}, nil)
	require.Error(t, err)

	fileURI := "file:///tmp/image.png"
	_, err = PromptToClaude([]acp.ContentBlock{{Image: &acp.ContentBlockImage{Type: "image", Uri: &fileURI}}}, nil)
	require.Error(t, err)
}
