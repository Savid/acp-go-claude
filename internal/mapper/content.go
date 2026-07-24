package mapper

import (
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"strings"

	"github.com/coder/acp-go-sdk"
)

// errValueUnsupported is the uniform machine-readable token for prompt content
// the Claude harness cannot accept; it is surfaced as a -32602 invalid-params
// error so hosts classify it as a bad request, not a server-internal failure.
const errValueUnsupported = "unsupported"

// Uniform invalid-params data keys and field paths for prompt-content
// rejections.
const (
	keyErrorField       = "error"
	keyFieldField       = "field"
	fieldPromptImage    = "prompt.image"
	fieldPromptResource = "prompt.resource"

	errMissingImageData    = "missing_data"
	errInvalidBase64       = "invalid_base64"
	errInvalidMediaType    = "invalid_media_type"
	errMediaTypeMismatch   = "media_type_mismatch"
	errAnimatedUnsupported = "animated_not_supported"
	errInvalidDimensions   = "invalid_dimensions"
	errImageTooLarge       = "too_large"
	errMissingResourceData = "missing resource data or uri"
)

// ImageInputLimits contains the decoded-byte limits used while mapping a prompt.
type ImageInputLimits struct {
	MaxBytesPerImage  int64
	MaxBytesPerPrompt int64
}

// PromptToClaude converts ACP prompt content to Claude stream-json user content.
// An empty prompt fails closed with the uniform unsupported-prompt shape.
func PromptToClaude(
	prompt []acp.ContentBlock,
	advertisedCommands []acp.AvailableCommand,
	limits ImageInputLimits,
) ([]map[string]any, error) {
	if len(prompt) == 0 {
		return nil, acp.NewInvalidParams(map[string]any{keyErrorField: errValueUnsupported, keyFieldField: keyPrompt})
	}

	blocks := make([]map[string]any, 0, len(prompt))
	contextBlocks := make([]map[string]any, 0)
	advertised := advertisedCommandSet(advertisedCommands)
	imageIndex := 0

	var promptImageBytes int64

	for _, block := range prompt {
		converted, context, image, err := contentBlockToClaude(block, advertised, imageIndex, &promptImageBytes, limits)
		if err != nil {
			return nil, err
		}

		if image {
			imageIndex++
		}

		blocks = append(blocks, converted...)
		contextBlocks = append(contextBlocks, context...)
	}

	return append(blocks, contextBlocks...), nil
}

func contentBlockToClaude(
	block acp.ContentBlock,
	advertisedCommands map[string]struct{},
	imageIndex int,
	promptImageBytes *int64,
	limits ImageInputLimits,
) ([]map[string]any, []map[string]any, bool, error) {
	switch {
	case block.Text != nil:
		if textAudienceIsUserOnly(block.Text.Annotations) {
			return nil, nil, false, nil
		}

		return []map[string]any{textBlock(rewriteAdvertisedMCPSlashCommand(block.Text.Text, advertisedCommands))}, nil, false, nil
	case block.Image != nil:
		if err := validatePromptImage(block.Image.Data, block.Image.MimeType, imageIndex, promptImageBytes, limits); err != nil {
			return nil, nil, true, err
		}

		return []map[string]any{base64Block(typeImage, block.Image.MimeType, block.Image.Data)}, nil, true, nil
	case block.ResourceLink != nil:
		return []map[string]any{textBlock(resourceLinkText(*block.ResourceLink))}, nil, false, nil
	case block.Resource != nil:
		converted, context, image, err := resourceToClaude(
			block.Resource.Resource,
			imageIndex,
			promptImageBytes,
			limits,
		)

		return converted, context, image, err
	default:
		return nil, nil, false, acp.NewInvalidParams(map[string]any{keyErrorField: errValueUnsupported, keyFieldField: keyPrompt})
	}
}

func validatePromptImage(
	data string,
	mimeType string,
	index int,
	promptBytes *int64,
	limits ImageInputLimits,
) error {
	if data == "" {
		return imageInputError(errMissingImageData, index, 0, 0)
	}

	if !portableImageMIME(mimeType) {
		return imageInputError(errInvalidMediaType, index, 0, 0)
	}

	decoded, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return imageInputError(errInvalidBase64, index, 0, 0)
	}

	info, ok := parseRaster(decoded)
	if !ok {
		// Bytes that sniff as no allowlisted raster are a media-type mismatch.
		// A recognized container whose header yields no valid dimensions is an
		// invalid_dimensions failure, reported ahead of the declared-vs-sniffed
		// mismatch checked below even when the declared type also disagrees.
		if info.mime == "" {
			return imageInputError(errMediaTypeMismatch, index, 0, 0)
		}

		return imageInputError(errInvalidDimensions, index, 0, 0)
	}

	if info.animated {
		return imageInputError(errAnimatedUnsupported, index, 0, 0)
	}

	if info.mime != mimeType {
		return imageInputError(errMediaTypeMismatch, index, 0, 0)
	}

	size := int64(len(decoded))
	if limits.MaxBytesPerImage > 0 && size > limits.MaxBytesPerImage {
		return imageInputError(errImageTooLarge, index, size, limits.MaxBytesPerImage)
	}

	*promptBytes += size
	if limits.MaxBytesPerPrompt > 0 && *promptBytes > limits.MaxBytesPerPrompt {
		return imageInputError(errImageTooLarge, index, *promptBytes, limits.MaxBytesPerPrompt)
	}

	return nil
}

func portableImageMIME(mimeType string) bool {
	switch mimeType {
	case mimePNG, mimeJPEG, mimeGIF, mimeWebP:
		return true
	default:
		return false
	}
}

func imageInputError(reason string, index int, size int64, maxBytes int64) error {
	data := map[string]any{
		keyFieldField: fieldPromptImage,
		keyErrorField: reason,
		keyIndex:      index,
	}
	if size > 0 || maxBytes > 0 {
		data["sizeBytes"] = size
		data["maxBytes"] = maxBytes
	}

	return acp.NewInvalidParams(data)
}

func advertisedCommandSet(commands []acp.AvailableCommand) map[string]struct{} {
	if len(commands) == 0 {
		return nil
	}

	advertised := make(map[string]struct{}, len(commands))
	for _, command := range commands {
		if validSlashCommandName(command.Name) {
			advertised[command.Name] = struct{}{}
		}
	}

	return advertised
}

func textAudienceIsUserOnly(annotations *acp.Annotations) bool {
	return annotations != nil &&
		len(annotations.Audience) == 1 &&
		annotations.Audience[0] == acp.RoleUser
}

func rewriteAdvertisedMCPSlashCommand(text string, advertisedCommands map[string]struct{}) string {
	name := leadingSlashCommandName(text)
	if !strings.HasPrefix(name, "mcp:") {
		return text
	}

	if _, ok := advertisedCommands[name]; !ok {
		return text
	}

	server, command, ok := strings.Cut(strings.TrimPrefix(name, "mcp:"), ":")
	if !ok || server == "" || command == "" {
		return text
	}

	return "/" + server + ":" + command + " (MCP)" + text[len(name)+1:]
}

func resourceToClaude(
	resource acp.EmbeddedResourceResource,
	imageIndex int,
	promptImageBytes *int64,
	limits ImageInputLimits,
) ([]map[string]any, []map[string]any, bool, error) {
	if resource.TextResourceContents != nil {
		context := []map[string]any{textBlock(contextResourceText(
			resource.TextResourceContents.Uri,
			resource.TextResourceContents.Text,
		))}
		if resource.TextResourceContents.Uri != "" {
			return []map[string]any{textBlock(formatURIAsLink(resource.TextResourceContents.Uri))}, context, false, nil
		}

		return nil, context, false, nil
	}

	if resource.BlobResourceContents != nil {
		mimeType := ""
		if resource.BlobResourceContents.MimeType != nil {
			mimeType = *resource.BlobResourceContents.MimeType
		}

		if strings.HasPrefix(mimeType, "image/") {
			if err := validatePromptImage(
				resource.BlobResourceContents.Blob,
				mimeType,
				imageIndex,
				promptImageBytes,
				limits,
			); err != nil {
				return nil, nil, true, err
			}

			return []map[string]any{base64Block(typeImage, mimeType, resource.BlobResourceContents.Blob)}, nil, true, nil
		}

		if mimeType == "application/pdf" {
			return []map[string]any{base64Block(typeDocument, mimeType, resource.BlobResourceContents.Blob)}, nil, false, nil
		}

		return nil, nil, false, acp.NewInvalidParams(map[string]any{keyErrorField: errValueUnsupported, keyFieldField: fieldPromptResource})
	}

	return nil, nil, false, acp.NewInvalidParams(map[string]any{keyFieldField: fieldPromptResource, keyErrorField: errMissingResourceData})
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
