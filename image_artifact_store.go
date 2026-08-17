package claudeacp

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/savid/acp-go-claude/internal/mapper"
)

const (
	imageArtifactPrefix  = "_artifacts/images/"
	imageArtifactVersion = 1
	imageArtifactTTL     = 24 * time.Hour
)

var imageArtifactNow = time.Now

type storedImageArtifact struct {
	Version     int    `json:"version"`
	Identity    string `json:"identity"`
	Fingerprint string `json:"fingerprint"`
	MimeType    string `json:"mimeType"`
	Data        string `json:"data"`
	URI         string `json:"uri,omitempty"`
	CreatedAt   int64  `json:"createdAt"`
}

func imageArtifactSubpath(subpath string) bool {
	return strings.HasPrefix(subpath, imageArtifactPrefix)
}

func imageArtifactKey(identity string, fingerprint string) string {
	sum := sha256.Sum256([]byte(identity + "\x00" + fingerprint))

	return imageArtifactPrefix + hex.EncodeToString(sum[:]) + ".json"
}

func (a *Agent) loadImageArtifacts(ctx context.Context, sourceSessionID string) (map[string]storedImageArtifact, error) {
	artifacts := make(map[string]storedImageArtifact)
	if sourceSessionID == "" {
		return artifacts, nil
	}

	store := a.sessionStore()
	mainKey := SessionKey{SessionID: sourceSessionID}

	listCtx, cancel := context.WithTimeout(ctx, a.sessionStoreLoadTimeout())
	defer cancel()

	listCtx, finish := a.observe.StartSessionStore(listCtx, "list_image_artifacts")
	subkeys, err := store.ListSubkeys(listCtx, mainKey)
	finish(err)

	if err != nil {
		return nil, fmt.Errorf("list image artifacts: %w", err)
	}

	cutoff := imageArtifactNow().Add(-imageArtifactTTL).UnixMilli()

	for _, subpath := range subkeys {
		if !imageArtifactSubpath(subpath) {
			continue
		}

		key := SessionKey{SessionID: sourceSessionID, Subpath: subpath}

		entries, err := a.loadStoreEntries(ctx, store, key)
		if err != nil {
			return nil, fmt.Errorf("load image artifact: %w", err)
		}

		if len(entries) != 1 {
			continue
		}

		var artifact storedImageArtifact
		if err := json.Unmarshal(entries[0], &artifact); err != nil ||
			artifact.Version != imageArtifactVersion ||
			artifact.Identity == "" ||
			artifact.Fingerprint == "" ||
			artifact.Data == "" ||
			!validStoredImageArtifact(artifact) {
			continue
		}

		if artifact.CreatedAt < cutoff {
			if err := store.Delete(ctx, key); err != nil {
				return nil, fmt.Errorf("delete expired image artifact: %w", err)
			}

			continue
		}

		artifacts[subpath] = artifact
	}

	return artifacts, nil
}

func (a *Agent) copyImageArtifacts(
	ctx context.Context,
	targetSessionID string,
	artifacts map[string]storedImageArtifact,
) error {
	for subpath, artifact := range artifacts {
		entry, _ := json.Marshal(artifact)
		if err := a.sessionStore().Append(ctx, SessionKey{
			SessionID: targetSessionID,
			Subpath:   subpath,
		}, []SessionStoreEntry{entry}); err != nil {
			return fmt.Errorf("store forked image artifact: %w", err)
		}
	}

	return nil
}

func (s *agentSession) persistImageArtifact(
	ctx context.Context,
	identity string,
	fingerprint string,
	mimeType string,
	data string,
	uri string,
) (storedImageArtifact, error) {
	subpath := imageArtifactKey(identity, fingerprint)

	s.imageMu.Lock()
	defer s.imageMu.Unlock()

	if s.imageArtifacts == nil {
		s.imageArtifacts = make(map[string]storedImageArtifact)
	}

	if artifact, ok := s.imageArtifacts[subpath]; ok {
		return artifact, nil
	}

	artifact := storedImageArtifact{
		Version:     imageArtifactVersion,
		Identity:    identity,
		Fingerprint: fingerprint,
		MimeType:    mimeType,
		Data:        data,
		URI:         uri,
		CreatedAt:   imageArtifactNow().UnixMilli(),
	}
	entry, _ := json.Marshal(artifact)

	key := SessionKey{SessionID: string(s.id), Subpath: subpath}
	appendCtx, finish := s.agent.observe.StartSessionStore(ctx, "append_image_artifact")
	err := s.agent.sessionStore().Append(appendCtx, key, []SessionStoreEntry{entry})
	finish(err)

	if err != nil {
		return storedImageArtifact{}, fmt.Errorf("store image artifact: %w", err)
	}

	s.imageArtifacts[subpath] = artifact

	return artifact, nil
}

// snapshotImageArtifacts copies the live artifact map under imageMu so a reader
// can iterate it without racing concurrent persistImageArtifact writes.
func (s *agentSession) snapshotImageArtifacts() map[string]storedImageArtifact {
	s.imageMu.Lock()
	defer s.imageMu.Unlock()

	snapshot := make(map[string]storedImageArtifact, len(s.imageArtifacts))
	for subpath, artifact := range s.imageArtifacts {
		snapshot[subpath] = artifact
	}

	return snapshot
}

// persistTranscriptImageArtifact stores an image artifact from a transcript
// mirror frame whose bytes have not yet been persisted by live emission. The
// mirror frame is the authoritative byte source at that moment, so a lookup
// miss reflects live/mirror ordering rather than lost canonical bytes.
func (s *agentSession) persistTranscriptImageArtifact(
	ctx context.Context,
	identity string,
	data string,
) (storedImageArtifact, error) {
	decoded, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return storedImageArtifact{}, imageOutputFailure(
			imageOutputInvalidBase64,
			"mirrored image output contains invalid base64",
			0,
			0,
		)
	}

	info, ok := mapper.InspectRaster(decoded)
	if !ok {
		return storedImageArtifact{}, imageOutputFailure(
			imageOutputNotRaster,
			"mirrored image output bytes are not a raster",
			0,
			0,
		)
	}

	return s.persistImageArtifact(
		ctx,
		identity,
		imageFingerprint(decoded),
		info.MimeType,
		base64.StdEncoding.EncodeToString(decoded),
		"",
	)
}

func (s *agentSession) imageArtifactByIdentity(identity string) (storedImageArtifact, bool) {
	s.imageMu.Lock()
	defer s.imageMu.Unlock()

	for _, artifact := range s.imageArtifacts {
		if artifact.Identity == identity {
			return artifact, true
		}
	}

	return storedImageArtifact{}, false
}

func (s *agentSession) imageArtifactByFingerprint(prefix string, fingerprint string) (storedImageArtifact, bool) {
	s.imageMu.Lock()
	defer s.imageMu.Unlock()

	for _, artifact := range s.imageArtifacts {
		if strings.HasPrefix(artifact.Identity, prefix) && artifact.Fingerprint == fingerprint {
			return artifact, true
		}
	}

	return storedImageArtifact{}, false
}

// toolArtifactByFingerprint recovers a tool-provenance artifact whose content
// index shifted between live emission and replay, matching on the tool-call
// identity prefix plus the checksum of the replayed bytes.
func (s *agentSession) toolArtifactByFingerprint(identity string, data string) (storedImageArtifact, bool) {
	const toolIdentityPrefix = "tool:"

	if !strings.HasPrefix(identity, toolIdentityPrefix) {
		return storedImageArtifact{}, false
	}

	decoded, err := base64.StdEncoding.DecodeString(data)
	if err != nil {
		return storedImageArtifact{}, false
	}

	separator := strings.LastIndex(identity, ":")

	return s.imageArtifactByFingerprint(identity[:separator+1], imageFingerprint(decoded))
}

func validStoredImageArtifact(artifact storedImageArtifact) bool {
	decoded, err := base64.StdEncoding.DecodeString(artifact.Data)
	if err != nil || imageFingerprint(decoded) != artifact.Fingerprint {
		return false
	}

	info, ok := mapper.InspectRaster(decoded)

	return ok && info.MimeType == artifact.MimeType
}
