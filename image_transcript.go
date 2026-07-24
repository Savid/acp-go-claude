package claudeacp

import (
	"context"
	"encoding/json"
	"fmt"
)

const (
	transcriptArtifactSourceType = "acp_artifact"
	transcriptArtifactKey        = "artifact_key"
	transcriptEntryAssistant     = "assistant"
	transcriptEntryUser          = "user"
	transcriptSourceBase64       = "base64"
)

func (s *agentSession) sanitizeTranscriptImageEntries(
	ctx context.Context,
	entries []SessionStoreEntry,
) ([]SessionStoreEntry, error) {
	sanitized := make([]SessionStoreEntry, 0, len(entries))
	for _, entry := range entries {
		var value map[string]any
		if err := json.Unmarshal(entry, &value); err != nil {
			sanitized = append(sanitized, cloneStoreEntry(entry))

			continue
		}

		switch value[jsonFieldType] {
		case transcriptEntryAssistant:
			if err := s.sanitizeAssistantTranscriptImages(ctx, value); err != nil {
				return nil, err
			}
		case transcriptEntryUser:
			if err := s.sanitizeToolTranscriptImages(ctx, value); err != nil {
				return nil, err
			}
		}

		encoded, _ := json.Marshal(value)
		sanitized = append(sanitized, encoded)
	}

	return sanitized, nil
}

func (s *agentSession) sanitizeAssistantTranscriptImages(ctx context.Context, entry map[string]any) error {
	message, _ := entry[jsonFieldMessage].(map[string]any)
	content, _ := message["content"].([]any)

	messageID, _ := entry["uuid"].(string)
	if messageID == "" {
		messageID, _ = message["id"].(string)
	}

	imageIndex := 0

	for _, raw := range content {
		block, _ := raw.(map[string]any)
		if block == nil || block[jsonFieldType] != imageContentType {
			continue
		}

		identity := fmt.Sprintf("agent:%s:%d", messageID, imageIndex)
		imageIndex++

		if err := s.replaceTranscriptImageData(ctx, block, identity); err != nil {
			return err
		}
	}

	return nil
}

func (s *agentSession) sanitizeToolTranscriptImages(ctx context.Context, entry map[string]any) error {
	message, _ := entry[jsonFieldMessage].(map[string]any)
	content, _ := message["content"].([]any)

	for _, raw := range content {
		result, _ := raw.(map[string]any)
		if result == nil || result[jsonFieldType] != "tool_result" {
			continue
		}

		toolID, _ := result["tool_use_id"].(string)
		switch resultContent := result["content"].(type) {
		case map[string]any:
			if resultContent[jsonFieldType] == imageContentType {
				if err := s.replaceTranscriptImageData(ctx, resultContent, fmt.Sprintf("tool:%s:0", toolID)); err != nil {
					return err
				}
			}
		case []any:
			for index, item := range resultContent {
				block, _ := item.(map[string]any)
				if block == nil || block[jsonFieldType] != imageContentType {
					continue
				}

				if err := s.replaceTranscriptImageData(ctx, block, fmt.Sprintf("tool:%s:%d", toolID, index)); err != nil {
					return err
				}
			}
		}
	}

	return nil
}

func (s *agentSession) replaceTranscriptImageData(ctx context.Context, block map[string]any, identity string) error {
	data, _ := diagnosticImageData(block)
	if data == "" {
		return nil
	}

	artifact, ok := s.imageArtifactByIdentity(identity)
	if !ok {
		artifact, ok = s.toolArtifactByFingerprint(identity, data)
	}

	if !ok {
		persisted, err := s.persistTranscriptImageArtifact(ctx, identity, data)
		if err != nil {
			return err
		}

		artifact = persisted
	}

	delete(block, jsonFieldData)
	block["source"] = map[string]any{
		jsonFieldType:         transcriptArtifactSourceType,
		transcriptArtifactKey: imageArtifactKey(artifact.Identity, artifact.Fingerprint),
		jsonFieldMediaType:    artifact.MimeType,
	}

	return nil
}

func rehydrateTranscriptImageEntries(
	entries []SessionStoreEntry,
	artifacts map[string]storedImageArtifact,
) ([]SessionStoreEntry, error) {
	rehydrated := make([]SessionStoreEntry, 0, len(entries))
	for _, entry := range entries {
		var value any
		if err := json.Unmarshal(entry, &value); err != nil {
			rehydrated = append(rehydrated, cloneStoreEntry(entry))

			continue
		}

		if err := rehydrateTranscriptValue(value, artifacts); err != nil {
			return nil, err
		}

		encoded, _ := json.Marshal(value)
		rehydrated = append(rehydrated, encoded)
	}

	return rehydrated, nil
}

func rehydrateTranscriptValue(value any, artifacts map[string]storedImageArtifact) error {
	switch typed := value.(type) {
	case map[string]any:
		if typed[jsonFieldType] == imageContentType {
			source, _ := typed["source"].(map[string]any)
			if source != nil && source[jsonFieldType] == transcriptArtifactSourceType {
				subpath, _ := source[transcriptArtifactKey].(string)

				artifact, ok := artifacts[subpath]
				if !ok || !validStoredImageArtifact(artifact) {
					return imageOutputFailure(
						imageOutputStorageFailed,
						"image output is no longer available from the artifact store",
						0,
						0,
					)
				}

				typed["source"] = map[string]any{
					jsonFieldType:      transcriptSourceBase64,
					jsonFieldMediaType: artifact.MimeType,
					jsonFieldData:      artifact.Data,
				}
			}
		}

		for _, item := range typed {
			if err := rehydrateTranscriptValue(item, artifacts); err != nil {
				return err
			}
		}
	case []any:
		for _, item := range typed {
			if err := rehydrateTranscriptValue(item, artifacts); err != nil {
				return err
			}
		}
	}

	return nil
}
