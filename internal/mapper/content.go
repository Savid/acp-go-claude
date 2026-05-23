package mapper

import (
	"encoding/xml"
	"fmt"
	"strings"

	"github.com/coder/acp-go-sdk"
)

// PromptToClaude converts ACP prompt content to Claude stream-json user content.
func PromptToClaude(prompt []acp.ContentBlock) ([]map[string]any, error) {
	blocks := make([]map[string]any, 0, len(prompt))
	contextBlocks := make([]map[string]any, 0)

	for _, block := range prompt {
		converted, context, err := contentBlockToClaude(block)
		if err != nil {
			return nil, err
		}

		blocks = append(blocks, converted...)
		contextBlocks = append(contextBlocks, context...)
	}

	return append(blocks, contextBlocks...), nil
}

func contentBlockToClaude(block acp.ContentBlock) ([]map[string]any, []map[string]any, error) {
	switch {
	case block.Text != nil:
		if textAudienceIsUserOnly(block.Text.Annotations) {
			return nil, nil, nil
		}

		return []map[string]any{textBlock(rewriteMCPSlashCommand(block.Text.Text))}, nil, nil
	case block.Image != nil:
		if block.Image.Data != "" {
			return []map[string]any{base64Block(typeImage, block.Image.MimeType, block.Image.Data)}, nil, nil
		}

		if block.Image.Uri != nil && httpURI(*block.Image.Uri) {
			return []map[string]any{urlImageBlock(*block.Image.Uri)}, nil, nil
		}

		return nil, nil, fmt.Errorf("image prompt content requires base64 data or an http(s) URI")
	case block.ResourceLink != nil:
		return []map[string]any{textBlock(resourceLinkText(*block.ResourceLink))}, nil, nil
	case block.Resource != nil:
		return resourceToClaude(block.Resource.Resource)
	case block.Audio != nil:
		return nil, nil, fmt.Errorf("audio prompt content is not supported by Claude Code")
	default:
		return nil, nil, fmt.Errorf("unsupported empty ACP content block")
	}
}

func textAudienceIsUserOnly(annotations *acp.Annotations) bool {
	return annotations != nil &&
		len(annotations.Audience) == 1 &&
		annotations.Audience[0] == acp.RoleUser
}

func rewriteMCPSlashCommand(text string) string {
	if !strings.HasPrefix(text, "/mcp:") {
		return text
	}

	commandText, args, hasArgs := strings.Cut(text, " ")

	parts := strings.SplitN(strings.TrimPrefix(commandText, "/mcp:"), ":", 2)
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || strings.ContainsAny(parts[0], "\t\n\r") {
		return text
	}

	rewritten := "/" + parts[0] + ":" + parts[1] + " (MCP)"
	if hasArgs {
		rewritten += " " + args
	}

	return rewritten
}

func resourceToClaude(resource acp.EmbeddedResourceResource) ([]map[string]any, []map[string]any, error) {
	if resource.TextResourceContents != nil {
		context := []map[string]any{textBlock(contextResourceText(
			resource.TextResourceContents.Uri,
			resource.TextResourceContents.Text,
		))}
		if resource.TextResourceContents.Uri != "" {
			return []map[string]any{textBlock(formatURIAsLink(resource.TextResourceContents.Uri))}, context, nil
		}

		return nil, context, nil
	}

	if resource.BlobResourceContents != nil {
		mimeType := ""
		if resource.BlobResourceContents.MimeType != nil {
			mimeType = *resource.BlobResourceContents.MimeType
		}

		if strings.HasPrefix(mimeType, "image/") {
			return []map[string]any{base64Block(typeImage, mimeType, resource.BlobResourceContents.Blob)}, nil, nil
		}

		if mimeType == "application/pdf" {
			return []map[string]any{base64Block(typeDocument, mimeType, resource.BlobResourceContents.Blob)}, nil, nil
		}

		return nil, nil, fmt.Errorf("unsupported embedded resource mime type %q", mimeType)
	}

	return nil, nil, fmt.Errorf("empty embedded resource")
}

func textBlock(text string) map[string]any {
	return map[string]any{
		keyType: typeText,
		keyText: text,
	}
}

func base64Block(blockType string, mimeType string, data string) map[string]any {
	return map[string]any{
		keyType: blockType,
		keySource: map[string]any{
			keyType:      sourceBase64,
			keyMediaType: mimeType,
			keyData:      data,
		},
	}
}

func urlImageBlock(uri string) map[string]any {
	return map[string]any{
		keyType: typeImage,
		keySource: map[string]any{
			keyType: sourceURL,
			keyURL:  uri,
		},
	}
}

func resourceLinkText(link acp.ContentBlockResourceLink) string {
	return formatURIAsLink(strings.TrimSpace(link.Uri))
}

func formatURIAsLink(uri string) string {
	if !strings.HasPrefix(uri, "file://") && !strings.HasPrefix(uri, "zed://") {
		return uri
	}

	name := linkName(uri)
	if name == "" {
		name = uri
	}

	return fmt.Sprintf("[@%s](%s)", name, uri)
}

func linkName(uri string) string {
	trimmed := strings.TrimRight(uri, "/")
	if trimmed == "file:" || trimmed == "zed:" {
		return ""
	}

	index := strings.LastIndex(trimmed, "/")
	if index < 0 || index == len(trimmed)-1 {
		return trimmed
	}

	return trimmed[index+1:]
}

func contextResourceText(uri string, text string) string {
	var escapedURI strings.Builder

	var escapedText strings.Builder

	_ = xml.EscapeText(&escapedURI, []byte(uri))
	_ = xml.EscapeText(&escapedText, []byte(text))

	var output strings.Builder
	output.WriteString("\n<context ref=\"")
	output.WriteString(escapedURI.String())
	output.WriteString("\">\n")
	output.WriteString(escapedText.String())
	output.WriteString("\n</context>")

	return output.String()
}

func httpURI(uri string) bool {
	return strings.HasPrefix(uri, "http://") || strings.HasPrefix(uri, "https://")
}
