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

// EffectiveInputBytesPerImage is the per-image decoded byte bound prompt
// validation enforces for the configured policy limit. A disabled or oversized
// limit falls back to the decoded-frame clamp, because reading a host-named file
// unbounded is never an option and no larger image can reach the adapter anyway.
func EffectiveInputBytesPerImage(configured int64) int64 {
	if configured <= 0 || configured > MaxDecodedFrameBytes {
		return MaxDecodedFrameBytes
	}

	return configured
}

// EffectiveInputBytesPerPrompt is the per-prompt aggregate decoded byte bound
// prompt validation enforces. A configured aggregate binds as given; a zero one
// enforces no total at all, which is exactly what it reports.
func EffectiveInputBytesPerPrompt(configured int64) int64 {
	if configured <= 0 {
		return 0
	}

	return configured
}

// promptMediaState is the per-prompt accounting the byte and count gates share:
// the decoded bytes charged so far and the handoff-form blocks read so far.
type promptMediaState struct {
	bytes         int64
	handoffBlocks int
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

	var media promptMediaState

	for _, block := range prompt {
		// Validation reads host-named files, so a cancelled turn stops between
		// blocks rather than after the whole prompt has been pulled off disk.
		if err := ctx.Err(); err != nil {
			return nil, err
		}

		converted, appended, gated, err := contentBlockToClaude(
			ctx,
			block,
			advertised,
			mediaIndex,
			&media,
			limits,
			handoff,
		)
		if err != nil {
			return nil, err
		}

		if gated {
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
	media *promptMediaState,
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
		converted, err := promptImageToClaude(ctx, block.Image, mediaIndex, media, limits, handoff)

		return converted, nil, true, err
	case block.ResourceLink != nil:
		return []map[string]any{textBlock(resourceLinkText(*block.ResourceLink))}, nil, false, nil
	case block.Resource != nil:
		return resourceToClaude(
			block.Resource.Resource,
			mediaIndex,
			&media.bytes,
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
	media *promptMediaState,
	limits ImageInputLimits,
	handoff HandoffFileReader,
) ([]map[string]any, error) {
	if image.Data == "" && promptImageHandoffForm(image) {
		data, err := handoffImageData(ctx, handoff, image, index, media, limits)
		if err != nil {
			return nil, err
		}

		return []map[string]any{base64Block(typeImage, image.MimeType, data)}, nil
	}

	decoded, err := validatePromptImage(image.Data, image.MimeType, index, fieldPromptImage, &media.bytes, limits)
	if err != nil {
		return nil, err
	}

	// Re-encoded from the bytes that passed the gates rather than forwarded as
	// received: base64 decoding ignores line breaks and accepts non-canonical
	// trailing bits, so two host spellings of one image would otherwise reach
	// the harness as two different payloads.
	return []map[string]any{base64Block(typeImage, image.MimeType, base64.StdEncoding.EncodeToString(decoded))}, nil
}

// validatePromptImage returns the decoded bytes it admitted, which is what the
// native request is built from.
func validatePromptImage(
	data string,
	mimeType string,
	index int,
	field string,
	promptBytes *int64,
	limits ImageInputLimits,
) ([]byte, error) {
	if data == "" {
		return nil, mediaInputError(field, errMissingImageData, index, 0, 0)
	}

	if !portableImageMIME(mimeType) {
		return nil, mediaInputError(field, errInvalidMediaType, index, 0, 0)
	}

	decoded, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return nil, mediaInputError(field, errInvalidBase64, index, 0, 0)
	}

	if err := validateRasterBytes(decoded, mimeType, index, field, promptBytes, limits); err != nil {
		return nil, err
	}

	return decoded, nil
}

// validateRasterBytes runs the structural and byte gates shared by every
// inbound image form. Every byte verdict and every charge to the aggregate is
// decided on the bytes in hand, so nothing a separate size report claims can
// move them.
func validateRasterBytes(
	decoded []byte,
	mimeType string,
	index int,
	field string,
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
			return mediaInputError(field, errMediaTypeMismatch, index, 0, 0)
		}

		return mediaInputError(field, errInvalidDimensions, index, 0, 0)
	}

	if info.animated {
		return mediaInputError(field, errAnimatedUnsupported, index, 0, 0)
	}

	if info.mime != mimeType {
		return mediaInputError(field, errMediaTypeMismatch, index, 0, 0)
	}

	size := int64(len(decoded))

	maxPerImage := EffectiveInputBytesPerImage(limits.MaxBytesPerImage)
	if size > maxPerImage {
		return mediaInputError(field, errImageTooLarge, index, size, maxPerImage)
	}

	return chargePromptBytes(size, index, field, promptBytes, limits)
}

// chargePromptBytes adds size to the prompt's running total and enforces the
// aggregate on it. Every form that puts host bytes into the native request goes
// through here, so declaring bytes as one content shape rather than another
// cannot buy a second budget.
func chargePromptBytes(size int64, index int, field string, promptBytes *int64, limits ImageInputLimits) error {
	*promptBytes += size

	maxPerPrompt := EffectiveInputBytesPerPrompt(limits.MaxBytesPerPrompt)
	if maxPerPrompt > 0 && *promptBytes > maxPerPrompt {
		return mediaInputError(field, errImageTooLarge, index, *promptBytes, maxPerPrompt)
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

// mediaInputError reports a gated-media rejection. field names the kind of
// prompt block the bytes arrived on, which is independent of the gate chain the
// declared media type routed them through: a resource block stays
// prompt.resource even when the image chain is what rejected it. index is the
// media index, shared by image blocks and gated blob resources, so a document
// and an image in one prompt never report the same one.
func mediaInputError(field string, reason string, index int, size int64, maxBytes int64) error {
	data := map[string]any{
		keyFieldField: field,
		keyErrorField: reason,
		keyIndex:      index,
	}
	if size > 0 || maxBytes > 0 {
		data[keySizeBytes] = size
		data[keyMaxBytes] = maxBytes
	}

	return acp.NewInvalidParams(data)
}

func imageInputError(reason string, index int, size int64, maxBytes int64) error {
	return mediaInputError(fieldPromptImage, reason, index, size, maxBytes)
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
		// Text inlined into the prompt is prompt payload like any other, so it
		// is charged the aggregate rather than being a way around it.
		if err := chargePromptBytes(
			int64(len(resource.TextResourceContents.Text)),
			mediaIndex,
			fieldPromptResource,
			promptMediaBytes,
			limits,
		); err != nil {
			return nil, nil, false, err
		}

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

		// Routing is decided on the normalized media type so a noncanonical
		// declaration reaches the gates its bytes belong to instead of falling
		// through to a channel that validates nothing.
		normalized := normalizeDeclaredMIME(mimeType)

		if strings.HasPrefix(normalized, imageMIMEPrefix) {
			// The allowlist still admits on the verbatim declaration, so a
			// noncanonical raster is rejected rather than silently accepted.
			decoded, err := validatePromptImage(
				blob,
				mimeType,
				mediaIndex,
				fieldPromptResource,
				promptMediaBytes,
				limits,
			)
			if err != nil {
				return nil, nil, true, err
			}

			return []map[string]any{base64Block(typeImage, mimeType, base64.StdEncoding.EncodeToString(decoded))}, nil, true, nil
		}

		// A gated blob consumes a media index like an image block, so a
		// document and an image in one prompt never report the same index.
		if normalized == mimePDF {
			decoded, err := validatePromptBlob(blob, mediaIndex, promptMediaBytes, limits)
			if err != nil {
				return nil, nil, true, err
			}

			return []map[string]any{base64Block(typeDocument, normalized, base64.StdEncoding.EncodeToString(decoded))}, nil, true, nil
		}

		return nil, nil, false, acp.NewInvalidParams(map[string]any{keyErrorField: errValueUnsupported, keyFieldField: fieldPromptResource})
	}

	return nil, nil, false, acp.NewInvalidParams(map[string]any{keyFieldField: fieldPromptResource, keyErrorField: errMissingResourceData})
}

// validatePromptBlob gates an embedded blob resource whatever its media type:
// its base64 must decode, its decoded size must fit the per-image byte gate,
// and its bytes are charged to the per-prompt aggregate alongside image bytes.
// It returns the decoded bytes, which is what the native request is built from.
//
// The decode is strict, so an encoding with non-zero padding bits or embedded
// whitespace is rejected rather than quietly reinterpreted, and the caller
// re-encodes from the bytes that passed the gates: the payload the harness sees
// is then the one the gates measured, whatever spelling the host chose.
func validatePromptBlob(blob string, index int, promptBytes *int64, limits ImageInputLimits) ([]byte, error) {
	decoded, err := base64.StdEncoding.Strict().DecodeString(blob)
	if err != nil {
		return nil, mediaInputError(fieldPromptResource, errInvalidBase64, index, 0, 0)
	}

	size := int64(len(decoded))

	maxPerImage := EffectiveInputBytesPerImage(limits.MaxBytesPerImage)
	if size > maxPerImage {
		return nil, mediaInputError(fieldPromptResource, errImageTooLarge, index, size, maxPerImage)
	}

	if err := chargePromptBytes(size, index, fieldPromptResource, promptBytes, limits); err != nil {
		return nil, err
	}

	return decoded, nil
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
