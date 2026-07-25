package claudeacp

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strings"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/mapper"
)

const (
	imageOutputStage = "image_output"

	imageOutputInvalidBase64     = "invalid_base64"
	imageOutputNotRaster         = "not_a_raster"
	imageOutputMediaTypeMismatch = "media_type_mismatch"
	imageOutputMissingFile       = "missing_file"
	imageOutputPathNotAllowed    = "path_not_allowed"
	imageOutputTooLarge          = "too_large"
	imageOutputStorageFailed     = "storage_failed"

	maxACPImageDecodedBytes int64 = mapper.MaxDecodedFrameBytes
)

var (
	imageOutputEvalSymlinks = filepath.EvalSymlinks
	imageOutputStat         = os.Stat
	imageOutputOpen         = os.Open
	imageOutputReadAll      = io.ReadAll
)

func imageOutputFailure(reason string, message string, size int64, maxBytes int64) *acp.RequestError {
	data := map[string]any{
		jsonFieldError:    turnFailedError,
		failureFieldCause: failureCauseTransport,
		jsonFieldMessage:  message,
		"stage":           imageOutputStage,
		"reason":          reason,
	}
	if size > 0 || maxBytes > 0 {
		data["sizeBytes"] = size
		data["maxBytes"] = maxBytes
	}

	return acp.NewInternalError(data)
}

func (s *agentSession) prepareImageUpdateLocked(
	ctx context.Context,
	update acp.SessionUpdate,
	replay bool,
) (acp.SessionUpdate, bool, acp.ToolCallId, error) {
	switch {
	case update.AgentMessageChunk != nil:
		chunk := update.AgentMessageChunk
		if chunk.Content.Image == nil && chunk.Content.ResourceLink == nil {
			return update, true, "", nil
		}

		identity := agentImageIdentity(chunk.Meta)

		content, fingerprint, err := s.prepareOutputContent(ctx, identity, chunk.Content, replay)
		if err != nil {
			return acp.SessionUpdate{}, false, "", err
		}

		dedupKey := identity + "\x00" + fingerprint

		if s.emittedAgentImages == nil {
			s.emittedAgentImages = make(map[string]struct{})
		}

		if _, exists := s.emittedAgentImages[dedupKey]; exists {
			return acp.SessionUpdate{}, false, "", nil
		}

		s.emittedAgentImages[dedupKey] = struct{}{}
		chunk.Content = content

		return update, true, "", nil
	case update.ToolCall != nil:
		call := update.ToolCall

		call.RawOutput = sanitizeDiagnosticValue(call.RawOutput)
		if call.Content == nil {
			return update, true, "", nil
		}

		if s.toolContent == nil {
			s.toolContent = make(map[acp.ToolCallId][]acp.ToolCallContent)
		}

		content := mergeToolContent(s.toolContent[call.ToolCallId], call.Content)

		prepared, err := s.prepareToolContent(ctx, call.ToolCallId, content, replay)
		if err != nil {
			return acp.SessionUpdate{}, false, call.ToolCallId, err
		}

		call.Content = prepared
		s.toolContent[call.ToolCallId] = cloneToolContent(prepared)

		return update, true, "", nil
	case update.ToolCallUpdate != nil:
		call := update.ToolCallUpdate

		call.RawOutput = sanitizeDiagnosticValue(call.RawOutput)
		if call.Content == nil {
			return update, true, "", nil
		}

		if s.toolContent == nil {
			s.toolContent = make(map[acp.ToolCallId][]acp.ToolCallContent)
		}

		content := mergeToolContent(s.toolContent[call.ToolCallId], call.Content)

		prepared, err := s.prepareToolContent(ctx, call.ToolCallId, content, replay)
		if err != nil {
			return acp.SessionUpdate{}, false, call.ToolCallId, err
		}

		call.Content = prepared
		s.toolContent[call.ToolCallId] = cloneToolContent(prepared)

		return update, true, "", nil
	default:
		return update, true, "", nil
	}
}

func agentImageIdentity(meta map[string]any) string {
	messageID := ""
	imageIndex := 0

	claudeMeta, _ := meta[claudeMetaKey].(map[string]any)
	if claudeMeta != nil {
		messageID, _ = claudeMeta[jsonFieldMessageID].(string)
		switch value := claudeMeta["_internalImageIndex"].(type) {
		case int:
			imageIndex = value
		case float64:
			imageIndex = int(value)
		}

		delete(claudeMeta, "_internalImageIndex")
	}

	return fmt.Sprintf("agent:%s:%d", messageID, imageIndex)
}

func (s *agentSession) prepareToolContent(
	ctx context.Context,
	toolCallID acp.ToolCallId,
	content []acp.ToolCallContent,
	replay bool,
) ([]acp.ToolCallContent, error) {
	prepared := cloneToolContent(content)

	limit := effectiveOutputLimit(s.agent.options.ImageLimits.MaxOutputBytesPerToolCall)

	var total int64

	for index := range prepared {
		item := &prepared[index]
		if item.Content == nil || item.Content.Content.Image == nil {
			continue
		}

		identity := fmt.Sprintf("tool:%s:%d", toolCallID, index)

		mapped, _, err := s.prepareOutputContent(ctx, identity, item.Content.Content, replay)
		if err != nil {
			return nil, err
		}

		item.Content.Content = mapped

		if mapped.Image != nil {
			decoded, _ := base64.StdEncoding.DecodeString(mapped.Image.Data)
			total += int64(len(decoded))

			if total > limit {
				return nil, imageOutputFailure(
					imageOutputTooLarge,
					"image output exceeds the configured per-tool-call limit",
					total,
					limit,
				)
			}
		}
	}

	return prepared, nil
}

func (s *agentSession) prepareOutputContent(
	ctx context.Context,
	identity string,
	content acp.ContentBlock,
	replay bool,
) (acp.ContentBlock, string, error) {
	if content.ResourceLink != nil {
		fingerprint := sha256.Sum256([]byte(content.ResourceLink.Uri))

		return content, "uri:" + hex.EncodeToString(fingerprint[:]), nil
	}

	if content.Image == nil {
		return content, "", nil
	}

	if replay {
		artifact, ok := s.imageArtifactByIdentity(identity)
		if !ok {
			artifact, ok = s.toolArtifactByFingerprint(identity, content.Image.Data)
		}

		if !ok {
			return acp.ContentBlock{}, "", imageOutputFailure(
				imageOutputStorageFailed,
				"image output is no longer available from the artifact store",
				0,
				0,
			)
		}

		decoded, err := base64.StdEncoding.DecodeString(artifact.Data)
		if err != nil {
			return acp.ContentBlock{}, "", imageOutputFailure(
				imageOutputStorageFailed,
				"stored image output is not valid base64",
				0,
				0,
			)
		}

		if imageFingerprint(decoded) != artifact.Fingerprint {
			return acp.ContentBlock{}, "", imageOutputFailure(
				imageOutputStorageFailed,
				"stored image output checksum does not match",
				0,
				0,
			)
		}

		limit := effectiveOutputLimit(s.agent.options.ImageLimits.MaxOutputBytesPerImage)
		if int64(len(decoded)) > limit {
			return acp.ContentBlock{}, "", imageOutputFailure(
				imageOutputTooLarge,
				"stored image output exceeds the configured per-image limit",
				int64(len(decoded)),
				limit,
			)
		}

		info, ok := mapper.InspectRaster(decoded)
		if !ok || info.MimeType != artifact.MimeType {
			return acp.ContentBlock{}, "", imageOutputFailure(
				imageOutputStorageFailed,
				"stored image output metadata does not match its bytes",
				0,
				0,
			)
		}

		block := acp.ImageBlock(artifact.Data, artifact.MimeType)
		if uri := remoteImageURI(artifact.URI); uri != "" {
			block.Image.Uri = &uri
		}

		return block, artifact.Fingerprint, nil
	}

	image, err := normalizeDataURLImage(content.Image)
	if err != nil {
		return acp.ContentBlock{}, "", err
	}

	decoded, uri, err := s.materializeOutputImage(ctx, image)
	if err != nil {
		return acp.ContentBlock{}, "", err
	}

	limit := effectiveOutputLimit(s.agent.options.ImageLimits.MaxOutputBytesPerImage)

	size := int64(len(decoded))
	if size > limit {
		return acp.ContentBlock{}, "", imageOutputFailure(
			imageOutputTooLarge,
			"image output exceeds the configured per-image limit",
			size,
			limit,
		)
	}

	info, ok := mapper.InspectRaster(decoded)
	if !ok {
		return acp.ContentBlock{}, "", imageOutputFailure(
			imageOutputNotRaster,
			"image output bytes are not a raster",
			0,
			0,
		)
	}

	if image.MimeType != "" && image.MimeType != info.MimeType {
		return acp.ContentBlock{}, "", imageOutputFailure(
			imageOutputMediaTypeMismatch,
			"image output media type does not match its bytes",
			0,
			0,
		)
	}

	encoded := base64.StdEncoding.EncodeToString(decoded)
	fingerprint := imageFingerprint(decoded)

	artifact, err := s.persistImageArtifact(ctx, identity, fingerprint, info.MimeType, encoded, uri)
	if err != nil {
		return acp.ContentBlock{}, "", imageOutputFailure(
			imageOutputStorageFailed,
			err.Error(),
			0,
			0,
		)
	}

	block := acp.ImageBlock(artifact.Data, artifact.MimeType)
	if artifact.URI != "" {
		block.Image.Uri = &artifact.URI
	}

	return block, fingerprint, nil
}

func normalizeDataURLImage(image *acp.ContentBlockImage) (*acp.ContentBlockImage, error) {
	if image.Data != "" || image.Uri == nil || !strings.HasPrefix(*image.Uri, "data:") {
		return image, nil
	}

	header, encoded, ok := strings.Cut(strings.TrimPrefix(*image.Uri, "data:"), ",")
	if !ok || !strings.HasSuffix(header, ";base64") {
		return nil, imageOutputFailure(
			imageOutputInvalidBase64,
			"image output data URL is not base64 encoded",
			0,
			0,
		)
	}

	decoded, err := base64.StdEncoding.DecodeString(encoded)
	if err != nil {
		return nil, imageOutputFailure(
			imageOutputInvalidBase64,
			"image output data URL contains invalid base64",
			0,
			0,
		)
	}

	normalized := *image
	normalized.Data = base64.StdEncoding.EncodeToString(decoded)
	normalized.MimeType = strings.TrimSuffix(header, ";base64")
	normalized.Uri = nil

	return &normalized, nil
}

func (s *agentSession) materializeOutputImage(
	ctx context.Context,
	image *acp.ContentBlockImage,
) ([]byte, string, error) {
	if image.Data != "" {
		decoded, err := base64.StdEncoding.DecodeString(image.Data)
		if err != nil {
			return nil, "", imageOutputFailure(
				imageOutputInvalidBase64,
				"image output contains invalid base64",
				0,
				0,
			)
		}

		uri := ""
		if image.Uri != nil {
			uri = remoteImageURI(*image.Uri)
		}

		return decoded, uri, nil
	}

	if image.Uri == nil || strings.TrimSpace(*image.Uri) == "" {
		return nil, "", imageOutputFailure(
			imageOutputMissingFile,
			"image output does not contain bytes or a file path",
			0,
			0,
		)
	}

	path, err := localImagePath(*image.Uri)
	if err != nil {
		return nil, "", imageOutputFailure(imageOutputPathNotAllowed, err.Error(), 0, 0)
	}

	decoded, err := s.readAllowedImageFile(ctx, path)
	if err != nil {
		return nil, "", err
	}

	return decoded, "", nil
}

func remoteImageURI(value string) string {
	parsed, err := url.Parse(value)
	if err != nil || (parsed.Scheme != uriSchemeHTTP && parsed.Scheme != uriSchemeHTTPS) {
		return ""
	}

	return value
}

func localImagePath(location string) (string, error) {
	parsed, err := url.Parse(location)
	if err != nil {
		return "", fmt.Errorf("image output path is invalid")
	}

	switch parsed.Scheme {
	case "":
		if !filepath.IsAbs(location) {
			return "", fmt.Errorf("image output path must be absolute")
		}

		return filepath.Clean(location), nil
	case "file":
		if parsed.Host != "" && parsed.Host != "localhost" {
			return "", fmt.Errorf("image output file URI host is not local")
		}

		if parsed.Path == "" {
			return "", fmt.Errorf("image output file URI has no path")
		}

		return filepath.Clean(parsed.Path), nil
	default:
		return "", fmt.Errorf("image output path scheme is not allowed")
	}
}

func (s *agentSession) readAllowedImageFile(ctx context.Context, path string) ([]byte, error) {
	roots := []string{s.cwd, s.imageScratchDir}
	if !pathWithinAnyRoot(path, roots, false) {
		return nil, imageOutputFailure(
			imageOutputPathNotAllowed,
			"image output path is outside the allowed roots",
			0,
			0,
		)
	}

	resolved, err := imageOutputEvalSymlinks(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, imageOutputFailure(imageOutputMissingFile, "image output file does not exist", 0, 0)
		}

		return nil, imageOutputFailure(imageOutputPathNotAllowed, "image output path cannot be resolved safely", 0, 0)
	}

	if !pathWithinAnyRoot(resolved, roots, true) {
		return nil, imageOutputFailure(
			imageOutputPathNotAllowed,
			"image output path escapes the allowed roots",
			0,
			0,
		)
	}

	info, err := imageOutputStat(resolved)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, imageOutputFailure(imageOutputMissingFile, "image output file does not exist", 0, 0)
		}

		return nil, imageOutputFailure(imageOutputPathNotAllowed, "image output file cannot be inspected", 0, 0)
	}

	if !info.Mode().IsRegular() {
		return nil, imageOutputFailure(imageOutputPathNotAllowed, "image output path is not a regular file", 0, 0)
	}

	limit := effectiveOutputLimit(s.agent.options.ImageLimits.MaxOutputBytesPerImage)
	if info.Size() > limit {
		return nil, imageOutputFailure(
			imageOutputTooLarge,
			"image output exceeds the configured per-image limit",
			info.Size(),
			limit,
		)
	}

	file, err := imageOutputOpen(resolved)
	if err != nil {
		return nil, imageOutputFailure(imageOutputMissingFile, "image output file cannot be opened", 0, 0)
	}
	defer file.Close()

	reader := io.LimitReader(file, limit+1)

	data, err := imageOutputReadAll(reader)
	if err != nil {
		return nil, imageOutputFailure(imageOutputMissingFile, "image output file cannot be read", 0, 0)
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if int64(len(data)) > limit {
		return nil, imageOutputFailure(
			imageOutputTooLarge,
			"image output exceeds the configured per-image limit",
			int64(len(data)),
			limit,
		)
	}

	return data, nil
}

func effectiveOutputLimit(configured int64) int64 {
	if configured <= 0 || configured > maxACPImageDecodedBytes {
		return maxACPImageDecodedBytes
	}

	return configured
}

func pathWithinAnyRoot(path string, roots []string, resolveRoots bool) bool {
	for _, root := range roots {
		if root == "" {
			continue
		}

		if resolveRoots {
			resolved, err := imageOutputEvalSymlinks(root)
			if err != nil {
				continue
			}

			root = resolved
		}

		relative, err := filepath.Rel(filepath.Clean(root), filepath.Clean(path))
		if err == nil && relative != parentDirSegment &&
			!strings.HasPrefix(relative, parentDirSegment+string(filepath.Separator)) {
			return true
		}
	}

	return false
}

func imageFingerprint(data []byte) string {
	sum := sha256.Sum256(data)

	return hex.EncodeToString(sum[:])
}

func mergeToolContent(previous []acp.ToolCallContent, next []acp.ToolCallContent) []acp.ToolCallContent {
	merged := cloneToolContent(previous)

	for index, candidate := range next {
		seenInNext := 0

		for _, earlier := range next[:index] {
			if toolContentEqual(earlier, candidate) {
				seenInNext++
			}
		}

		seenInPrevious := 0

		for _, current := range previous {
			if toolContentEqual(current, candidate) {
				seenInPrevious++
			}
		}

		if seenInNext >= seenInPrevious {
			merged = append(merged, candidate)
		}
	}

	return merged
}

func toolContentEqual(left acp.ToolCallContent, right acp.ToolCallContent) bool {
	if left.Content == nil || right.Content == nil {
		return left.Content == nil && right.Content == nil
	}

	leftBlock := left.Content.Content

	rightBlock := right.Content.Content
	if leftBlock.Image != nil && rightBlock.Image != nil {
		if rightBlock.Image.Data != "" && leftBlock.Image.Data == rightBlock.Image.Data {
			return true
		}

		if rightBlock.Image.Uri != nil && leftBlock.Image.Uri != nil &&
			*rightBlock.Image.Uri == *leftBlock.Image.Uri {
			return true
		}
	}

	return reflect.DeepEqual(left, right)
}

func cloneToolContent(content []acp.ToolCallContent) []acp.ToolCallContent {
	if content == nil {
		return nil
	}

	return append([]acp.ToolCallContent(nil), content...)
}
