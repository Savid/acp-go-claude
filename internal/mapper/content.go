package mapper

import (
	"context"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"slices"
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

	keySizeBytes = "sizeBytes"
	keyMaxBytes  = "maxBytes"

	errMissingImageData    = "missing_data"
	errInvalidBase64       = "invalid_base64"
	errInvalidMediaType    = "invalid_media_type"
	errMediaTypeMismatch   = "media_type_mismatch"
	errAnimatedUnsupported = "animated_not_supported"
	errInvalidDimensions   = "invalid_dimensions"
	errImageTooLarge       = "too_large"
	errMissingResourceData = "missing resource data or uri"

	errInvalidHandoff        = "invalid_handoff"
	errPathNotAllowed        = "path_not_allowed"
	errMissingFile           = "missing_file"
	errHandoffDigestMismatch = "handoff_digest_mismatch"

	imageMIMEPrefix = "image/"
	mimePDF         = "application/pdf"
)

// portableImageMIMEs is the inbound media-type allowlist prompt validation
// enforces, in the order the adapter advertises it.
var portableImageMIMEs = []string{mimePNG, mimeJPEG, mimeGIF, mimeWebP}

// documentMIMEs are the media types an embedded blob resource maps to a native
// Claude document block.
var documentMIMEs = []string{mimePDF}

// PortableImageMIMEs returns the inbound image media-type allowlist enforced by
// prompt validation.
func PortableImageMIMEs() []string {
	return slices.Clone(portableImageMIMEs)
}

// DocumentMIMEs returns the media types mapped to a native document block.
func DocumentMIMEs() []string {
	return slices.Clone(documentMIMEs)
}

// ImageInputLimits contains the decoded-byte limits used while mapping a prompt.
type ImageInputLimits struct {
	MaxBytesPerImage  int64
	MaxBytesPerPrompt int64
}

// PromptToClaude converts ACP prompt content to Claude stream-json user content.
// An empty prompt fails closed with the uniform unsupported-prompt shape. A
// nil handoff reader rejects every handoff-form image block, which is how an
// unconfigured handoff read root fails closed.
func PromptToClaude(
	ctx context.Context,
	prompt []acp.ContentBlock,
	advertisedCommands []acp.AvailableCommand,
	limits ImageInputLimits,
	handoff HandoffFileReader,
) ([]map[string]any, error) {
	if len(prompt) == 0 {
		return nil, acp.NewInvalidParams(map[string]any{keyErrorField: errValueUnsupported, keyFieldField: keyPrompt})
	}

	blocks := make([]map[string]any, 0, len(prompt))
	contextBlocks := make([]map[string]any, 0)
	advertised := advertisedCommandSet(advertisedCommands)

	// mediaIndex counts the blocks the byte gates apply to, in request order:
	// image blocks and every gated blob resource whatever its media type. It is
	// the index a rejection reports, so no two gated blocks can share one.
	mediaIndex := 0

	var promptMediaBytes int64

	for _, block := range prompt {
		converted, appended, media, err := contentBlockToClaude(
			ctx,
			block,
			advertised,
			mediaIndex,
			&promptMediaBytes,
			limits,
			handoff,
		)
		if err != nil {
			return nil, err
		}

		if media {
			mediaIndex++
		}

		blocks = append(blocks, converted...)
		contextBlocks = append(contextBlocks, appended...)
	}

	return append(blocks, contextBlocks...), nil
}

// contentBlockToClaude reports whether the block consumed a media index in its
// third result.
func contentBlockToClaude(
	ctx context.Context,
	block acp.ContentBlock,
	advertisedCommands map[string]struct{},
	mediaIndex int,
	promptMediaBytes *int64,
	limits ImageInputLimits,
	handoff HandoffFileReader,
) ([]map[string]any, []map[string]any, bool, error) {
	switch {
	case block.Text != nil:
		if textAudienceIsUserOnly(block.Text.Annotations) {
			return nil, nil, false, nil
		}

		return []map[string]any{textBlock(rewriteAdvertisedMCPSlashCommand(block.Text.Text, advertisedCommands))}, nil, false, nil
	case block.Image != nil:
		converted, err := promptImageToClaude(ctx, block.Image, mediaIndex, promptMediaBytes, limits, handoff)

		return converted, nil, true, err
	case block.ResourceLink != nil:
		return []map[string]any{textBlock(resourceLinkText(*block.ResourceLink))}, nil, false, nil
	case block.Resource != nil:
		return resourceToClaude(
			block.Resource.Resource,
			mediaIndex,
			promptMediaBytes,
			limits,
		)
	default:
		return nil, nil, false, acp.NewInvalidParams(map[string]any{keyErrorField: errValueUnsupported, keyFieldField: keyPrompt})
	}
}

// promptImageToClaude selects the image block's form before any embedded gate
// runs: embedded bytes win over a uri exactly as before, an empty-data block
// signalling handoff intent is resolved through the handoff reader, and a block
// with neither is the unchanged missing_data rejection.
func promptImageToClaude(
	ctx context.Context,
	image *acp.ContentBlockImage,
	index int,
	promptBytes *int64,
	limits ImageInputLimits,
	handoff HandoffFileReader,
) ([]map[string]any, error) {
	if image.Data == "" && promptImageHandoffForm(image) {
		data, err := handoffImageData(ctx, handoff, image, index, promptBytes, limits)
		if err != nil {
			return nil, err
		}

		return []map[string]any{base64Block(typeImage, image.MimeType, data)}, nil
	}

	if err := validatePromptImage(image.Data, image.MimeType, index, promptBytes, limits); err != nil {
		return nil, err
	}

	return []map[string]any{base64Block(typeImage, image.MimeType, image.Data)}, nil
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

	return validateRasterBytes(decoded, int64(len(decoded)), mimeType, index, promptBytes, limits)
}

// validateRasterBytes runs the structural and byte gates shared by every
// inbound image form. size is the authoritative decoded size a byte rejection
// reports, which for a handoff file is its real size on disk rather than the
// length of the bounded read.
func validateRasterBytes(
	decoded []byte,
	size int64,
	mimeType string,
	index int,
	promptBytes *int64,
	limits ImageInputLimits,
) error {
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
	return slices.Contains(portableImageMIMEs, mimeType)
}

// normalizeDeclaredMIME lowercases a declared media type, trims it, and drops
// any parameters so a media-type prefix test sees the bare type. The allowlist
// comparison keeps the declared value verbatim, so a noncanonical raster
// declaration is still rejected rather than silently accepted.
func normalizeDeclaredMIME(mimeType string) string {
	base, _, _ := strings.Cut(mimeType, ";")

	return strings.ToLower(strings.TrimSpace(base))
}

func imageInputError(reason string, index int, size int64, maxBytes int64) error {
	data := map[string]any{
		keyFieldField: fieldPromptImage,
		keyErrorField: reason,
		keyIndex:      index,
	}
	if size > 0 || maxBytes > 0 {
		data[keySizeBytes] = size
		data[keyMaxBytes] = maxBytes
	}

	return acp.NewInvalidParams(data)
}

// resourceInputError reports a blob-resource rejection. Its index is the same
// media index an image block reports, because a gated blob occupies a position
// in that sequence.
func resourceInputError(reason string, index int, size int64, maxBytes int64) error {
	data := map[string]any{
		keyFieldField: fieldPromptResource,
		keyErrorField: reason,
		keyIndex:      index,
	}
	if size > 0 || maxBytes > 0 {
		data[keySizeBytes] = size
		data[keyMaxBytes] = maxBytes
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
	mediaIndex int,
	promptMediaBytes *int64,
	limits ImageInputLimits,
) ([]map[string]any, []map[string]any, bool, error) {
	if resource.TextResourceContents != nil {
		appended := []map[string]any{textBlock(contextResourceText(
			resource.TextResourceContents.Uri,
			resource.TextResourceContents.Text,
		))}
		if resource.TextResourceContents.Uri != "" {
			return []map[string]any{textBlock(formatURIAsLink(resource.TextResourceContents.Uri))}, appended, false, nil
		}

		return nil, appended, false, nil
	}

	if resource.BlobResourceContents != nil {
		mimeType := ""
		if resource.BlobResourceContents.MimeType != nil {
			mimeType = *resource.BlobResourceContents.MimeType
		}

		blob := resource.BlobResourceContents.Blob

		// The media-type prefix test is normalization-tolerant so a
		// noncanonical raster declaration reaches the image gates instead of
		// falling through to a channel that does not validate rasters at all.
		if strings.HasPrefix(normalizeDeclaredMIME(mimeType), imageMIMEPrefix) {
			if err := validatePromptImage(
				blob,
				mimeType,
				mediaIndex,
				promptMediaBytes,
				limits,
			); err != nil {
				return nil, nil, true, err
			}

			return []map[string]any{base64Block(typeImage, mimeType, blob)}, nil, true, nil
		}

		// A gated blob consumes a media index like an image block, so a
		// document and an image in one prompt never report the same index.
		if mimeType == mimePDF {
			if err := validatePromptBlob(blob, mediaIndex, promptMediaBytes, limits); err != nil {
				return nil, nil, true, err
			}

			return []map[string]any{base64Block(typeDocument, mimeType, blob)}, nil, true, nil
		}

		return nil, nil, false, acp.NewInvalidParams(map[string]any{keyErrorField: errValueUnsupported, keyFieldField: fieldPromptResource})
	}

	return nil, nil, false, acp.NewInvalidParams(map[string]any{keyFieldField: fieldPromptResource, keyErrorField: errMissingResourceData})
}

// validatePromptBlob gates an embedded blob resource whatever its media type:
// its base64 must decode, its decoded size must fit the per-image byte gate,
// and its bytes are charged to the per-prompt aggregate alongside image bytes.
func validatePromptBlob(blob string, index int, promptBytes *int64, limits ImageInputLimits) error {
	decoded, err := base64.StdEncoding.DecodeString(blob)
	if err != nil {
		return resourceInputError(errInvalidBase64, index, 0, 0)
	}

	size := int64(len(decoded))
	if limits.MaxBytesPerImage > 0 && size > limits.MaxBytesPerImage {
		return resourceInputError(errImageTooLarge, index, size, limits.MaxBytesPerImage)
	}

	*promptBytes += size
	if limits.MaxBytesPerPrompt > 0 && *promptBytes > limits.MaxBytesPerPrompt {
		return resourceInputError(errImageTooLarge, index, *promptBytes, limits.MaxBytesPerPrompt)
	}

	return nil
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
