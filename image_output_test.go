package claudeacp

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/coder/acp-go-sdk"
	"github.com/savid/acp-go-claude/internal/claude"
	"github.com/stretchr/testify/require"
)

func TestImageOutputInlineStoreReplayAndDedup(t *testing.T) {
	t.Parallel()

	png := outputFixtureBase64(t, "valid.png")
	store := NewInMemorySessionStore()
	agent := NewAgent(WithSessionStore(store))
	session := &agentSession{
		agent:              agent,
		id:                 "session-1",
		imageArtifacts:     make(map[string]storedImageArtifact),
		emittedAgentImages: make(map[string]struct{}),
	}
	meta := map[string]any{claudeMetaKey: map[string]any{
		"messageId":           "message-1",
		"_internalImageIndex": 0,
	}}
	update := acp.UpdateAgentMessage(acp.ImageBlock(png, "image/png"))
	update.AgentMessageChunk.Meta = meta

	prepared, emit, _, err := session.prepareImageUpdateLocked(t.Context(), update, false)
	require.NoError(t, err)
	require.True(t, emit)
	require.Equal(t, png, prepared.AgentMessageChunk.Content.Image.Data)
	require.Equal(t, "image/png", prepared.AgentMessageChunk.Content.Image.MimeType)
	require.NotContains(t, prepared.AgentMessageChunk.Meta[claudeMetaKey], "_internalImageIndex")

	subkeys, err := store.ListSubkeys(t.Context(), SessionKey{SessionID: "session-1"})
	require.NoError(t, err)
	require.Len(t, subkeys, 1)
	require.True(t, imageArtifactSubpath(subkeys[0]))

	replay := acp.UpdateAgentMessage(acp.ImageBlock("ignored", "image/jpeg"))
	replay.AgentMessageChunk.Meta = map[string]any{claudeMetaKey: map[string]any{
		"messageId":           "message-1",
		"_internalImageIndex": 0,
	}}
	session.emittedAgentImages = make(map[string]struct{})
	prepared, emit, _, err = session.prepareImageUpdateLocked(t.Context(), replay, true)
	require.NoError(t, err)
	require.True(t, emit)
	require.Equal(t, png, prepared.AgentMessageChunk.Content.Image.Data)
	require.Equal(t, "image/png", prepared.AgentMessageChunk.Content.Image.MimeType)

	prepared, emit, _, err = session.prepareImageUpdateLocked(t.Context(), replay, true)
	require.NoError(t, err)
	require.False(t, emit)
	require.Nil(t, prepared.AgentMessageChunk)

	missing := &agentSession{
		agent:          NewAgent(),
		id:             "session-2",
		imageArtifacts: make(map[string]storedImageArtifact),
	}
	replay.AgentMessageChunk.Meta = map[string]any{claudeMetaKey: map[string]any{
		"messageId":           "message-1",
		"_internalImageIndex": 0,
	}}
	_, missingEmit, _, err := missing.prepareImageUpdateLocked(t.Context(), replay, true)
	require.False(t, missingEmit)
	requireImageOutputError(t, err, imageOutputStorageFailed)
}

// TestImageOutputGuidanceSplitsRecoverableFromFatal pins the blast radius of
// every image-output verdict: an ordinary mistake that can be retried carries
// guidance and keeps the turn, the adapter's own store breaking does not.
func TestImageOutputGuidanceSplitsRecoverableFromFatal(t *testing.T) {
	t.Parallel()

	recoverable := map[string]string{
		imageOutputPathNotAllowed:    imageGuidancePathNotAllowed,
		imageOutputMissingFile:       imageGuidanceMissingFile,
		imageOutputTooLarge:          imageGuidanceTooLarge,
		imageOutputNotRaster:         imageGuidanceNotRaster,
		imageOutputInvalidBase64:     imageGuidanceInvalidBase64,
		imageOutputMediaTypeMismatch: imageGuidanceMIMEMismatched,
	}

	for reason, guidance := range recoverable {
		message, ok := imageOutputGuidance(imageOutputFailure(reason, "detail", 0, 0))
		require.True(t, ok, reason)
		require.Equal(t, guidance, message)

		// The guidance says what to do next and never describes the input.
		require.NotContains(t, message, "root")
		require.NotContains(t, message, "path")
	}

	_, ok := imageOutputGuidance(imageOutputFailure(imageOutputStorageFailed, "detail", 0, 0))
	require.False(t, ok)

	_, ok = imageOutputGuidance(acp.NewInternalError(map[string]any{"stage": "other"}))
	require.False(t, ok)

	_, ok = imageOutputGuidance(errors.New("not a request error"))
	require.False(t, ok)
}

// TestImageOutputReadsFromTheOSTempDir pins the temp directory as an allowed
// root against the real os.TempDir, not a narrowed one: the fixture is created
// through os.MkdirTemp so it sits wherever this platform actually puts temp
// files, symlinked parents included.
func TestImageOutputReadsFromTheOSTempDir(t *testing.T) {
	t.Parallel()

	session := &agentSession{
		agent:          NewAgent(WithSessionStore(NewInMemorySessionStore())),
		id:             "temp-root",
		cwd:            t.TempDir(),
		imageArtifacts: make(map[string]storedImageArtifact),
	}

	dir, err := os.MkdirTemp("", "claude-image-output")
	require.NoError(t, err)

	t.Cleanup(func() { _ = os.RemoveAll(dir) })

	path := filepath.Join(dir, "frame_01.png")
	require.NoError(t, os.WriteFile(path, outputFixtureBytes(t, "valid.png"), 0o600))

	require.False(t, pathWithinAnyRoot(path, []string{session.cwd}, true))

	data, err := session.readAllowedImageFile(t.Context(), path)
	require.NoError(t, err)
	require.Equal(t, outputFixtureBytes(t, "valid.png"), data)
}

// This case narrows the process temp directory to tell an allowed root from a
// refused one, so it cannot run in parallel.
func TestImageOutputValidationAndLocalRoots(t *testing.T) {
	workspace := t.TempDir()
	scratch := t.TempDir()
	agent := NewAgent(
		WithSessionStore(NewInMemorySessionStore()),
		WithImageLimits(ImageLimits{
			MaxOutputBytesPerImage:    2000,
			MaxOutputBytesPerToolCall: 4000,
		}),
	)
	session := &agentSession{
		agent:              agent,
		id:                 "session-1",
		cwd:                workspace,
		imageScratchDir:    scratch,
		imageArtifacts:     make(map[string]storedImageArtifact),
		toolContent:        make(map[acp.ToolCallId][]acp.ToolCallContent),
		emittedAgentImages: make(map[string]struct{}),
	}

	cases := []struct {
		name   string
		image  acp.ContentBlock
		reason string
	}{
		{name: "invalid base64", image: acp.ImageBlock("%", "image/png"), reason: imageOutputInvalidBase64},
		{
			name:   "not raster",
			image:  acp.ImageBlock(base64.StdEncoding.EncodeToString([]byte("not a raster")), "image/png"),
			reason: imageOutputNotRaster,
		},
		{name: "media mismatch", image: acp.ImageBlock(outputFixtureBase64(t, "valid.jpg"), "image/png"), reason: imageOutputMediaTypeMismatch},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			_, _, err := session.prepareOutputContent(t.Context(), "agent:test:0", test.image, false)
			requireImageOutputError(t, err, test.reason)
		})
	}

	bmp := validBMP()
	block, _, err := session.prepareOutputContent(
		t.Context(),
		"agent:bmp:0",
		acp.ImageBlock(base64.StdEncoding.EncodeToString(bmp), ""),
		false,
	)
	require.NoError(t, err)
	require.Equal(t, "image/bmp", block.Image.MimeType)

	dataURL := "data:image/png;base64," + outputFixtureBase64(t, "valid.png")
	dataURLBlock := acp.ContentBlock{
		Image: &acp.ContentBlockImage{Type: "image", Uri: &dataURL},
	}
	block, _, err = session.prepareOutputContent(t.Context(), "agent:data-url:0", dataURLBlock, false)
	require.NoError(t, err)
	require.Equal(t, outputFixtureBase64(t, "valid.png"), block.Image.Data)
	require.Equal(t, "image/png", block.Image.MimeType)
	require.Nil(t, block.Image.Uri)

	badDataURL := "data:image/png,%"
	_, _, err = session.prepareOutputContent(t.Context(), "agent:bad-data-url:0", acp.ContentBlock{
		Image: &acp.ContentBlockImage{Type: "image", Uri: &badDataURL},
	}, false)
	requireImageOutputError(t, err, imageOutputInvalidBase64)
	badDataURL = "data:image/png;base64,%"
	_, _, err = session.prepareOutputContent(t.Context(), "agent:bad-base64-data-url:0", acp.ContentBlock{
		Image: &acp.ContentBlockImage{Type: "image", Uri: &badDataURL},
	}, false)
	requireImageOutputError(t, err, imageOutputInvalidBase64)

	workspaceImage := filepath.Join(workspace, "image.png")
	require.NoError(t, os.WriteFile(workspaceImage, outputFixtureBytes(t, "valid.png"), 0o600))
	uri := "file://" + workspaceImage
	local := acp.ContentBlock{Image: &acp.ContentBlockImage{Type: "image", Uri: &uri}}
	block, _, err = session.prepareOutputContent(t.Context(), "tool:read:0", local, false)
	require.NoError(t, err)
	require.Equal(t, outputFixtureBase64(t, "valid.png"), block.Image.Data)
	require.Nil(t, block.Image.Uri)

	missingURI := "file://" + filepath.Join(workspace, "missing.png")
	_, _, err = session.prepareOutputContent(t.Context(), "tool:missing:0", acp.ContentBlock{
		Image: &acp.ContentBlockImage{Type: "image", Uri: &missingURI},
	}, false)
	requireImageOutputError(t, err, imageOutputMissingFile)

	// The OS temp directory is an allowed root, so this case narrows it to a
	// directory of its own; the workspace and scratch roots above were created
	// before the narrowing and stay outside it.
	//
	// The narrowed root is a symlink to the directory that holds the file, which
	// is what the temp directory itself is on macOS. Only a check that resolves
	// the root to the same degree as the candidate can match it, so this case
	// fails on every host if the root side stops being resolved rather than only
	// on the hosts whose temp directory happens to be a link.
	private := t.TempDir()
	tempTarget := filepath.Join(private, "tmp-target")
	require.NoError(t, os.Mkdir(tempTarget, 0o700))
	tempRoot := filepath.Join(private, "tmp")
	require.NoError(t, os.Symlink(tempTarget, tempRoot))
	outsideRoot := filepath.Join(private, "outside")
	require.NoError(t, os.Mkdir(outsideRoot, 0o700))
	narrowTempDir(t, tempRoot)

	tempImage := filepath.Join(tempRoot, "frame_01.png")
	require.NoError(t, os.WriteFile(tempImage, outputFixtureBytes(t, "valid.png"), 0o600))
	tempURI := "file://" + tempImage
	block, _, err = session.prepareOutputContent(t.Context(), "tool:temp:0", acp.ContentBlock{
		Image: &acp.ContentBlockImage{Type: "image", Uri: &tempURI},
	}, false)
	require.NoError(t, err)
	require.Equal(t, outputFixtureBase64(t, "valid.png"), block.Image.Data)

	outside := filepath.Join(outsideRoot, "outside.png")
	require.NoError(t, os.WriteFile(outside, outputFixtureBytes(t, "valid.png"), 0o600))
	outsideURI := "file://" + outside
	_, _, err = session.prepareOutputContent(t.Context(), "tool:outside:0", acp.ContentBlock{
		Image: &acp.ContentBlockImage{Type: "image", Uri: &outsideURI},
	}, false)
	requireImageOutputError(t, err, imageOutputPathNotAllowed)

	link := filepath.Join(workspace, "escape.png")
	require.NoError(t, os.Symlink(outside, link))
	linkURI := "file://" + link
	_, _, err = session.prepareOutputContent(t.Context(), "tool:link:0", acp.ContentBlock{
		Image: &acp.ContentBlockImage{Type: "image", Uri: &linkURI},
	}, false)
	requireImageOutputError(t, err, imageOutputPathNotAllowed)

	dirURI := "file://" + workspace
	_, _, err = session.prepareOutputContent(t.Context(), "tool:dir:0", acp.ContentBlock{
		Image: &acp.ContentBlockImage{Type: "image", Uri: &dirURI},
	}, false)
	requireImageOutputError(t, err, imageOutputPathNotAllowed)
}

func TestImageOutputEdges(t *testing.T) {
	pngBytes := outputFixtureBytes(t, "valid.png")
	png := base64.StdEncoding.EncodeToString(pngBytes)
	store := NewInMemorySessionStore()
	agent := NewAgent(
		WithSessionStore(store),
		WithImageLimits(ImageLimits{
			MaxOutputBytesPerImage:    int64(len(pngBytes)),
			MaxOutputBytesPerToolCall: int64(len(pngBytes)),
		}),
	)
	workspace := t.TempDir()
	session := &agentSession{agent: agent, id: "session", cwd: workspace}

	resource := acp.ResourceLinkBlock("image", "https://example.com/image.png")
	prepared, fingerprint, err := session.prepareOutputContent(t.Context(), "agent:link:0", resource, false)
	require.NoError(t, err)
	require.Equal(t, resource, prepared)
	require.NotEmpty(t, fingerprint)

	text := acp.TextBlock("text")
	prepared, fingerprint, err = session.prepareOutputContent(t.Context(), "agent:text:0", text, false)
	require.NoError(t, err)
	require.Equal(t, text, prepared)
	require.Empty(t, fingerprint)

	uri := "https://example.com/provenance.png"
	inline := acp.ImageBlock(png, "image/png")
	inline.Image.Uri = &uri
	prepared, _, err = session.prepareOutputContent(t.Context(), "agent:inline-uri:0", inline, false)
	require.NoError(t, err)
	require.Equal(t, uri, *prepared.Image.Uri)

	localProvenance := "file:///provenance.png"
	inline.Image.Uri = &localProvenance
	prepared, _, err = session.prepareOutputContent(t.Context(), "agent:inline-local:0", inline, false)
	require.NoError(t, err)
	require.Nil(t, prepared.Image.Uri)

	tooSmall := &agentSession{
		agent: NewAgent(
			WithSessionStore(NewInMemorySessionStore()),
			WithImageLimits(ImageLimits{
				MaxOutputBytesPerImage:    int64(len(pngBytes) - 1),
				MaxOutputBytesPerToolCall: int64(len(pngBytes) - 1),
			}),
		),
		id: "small",
	}
	_, _, err = tooSmall.prepareOutputContent(t.Context(), "agent:small:0", acp.ImageBlock(png, "image/png"), false)
	requireImageOutputError(t, err, imageOutputTooLarge)

	largeBMP := make([]byte, maxACPImageDecodedBytes+1)
	copy(largeBMP, validBMP())
	_, _, err = (&agentSession{
		agent: NewAgent(WithSessionStore(NewInMemorySessionStore()), WithImageLimits(ImageLimits{})),
		id:    "hard",
	}).prepareOutputContent(
		t.Context(),
		"agent:hard:0",
		acp.ImageBlock(base64.StdEncoding.EncodeToString(largeBMP), "image/bmp"),
		false,
	)
	requireImageOutputError(t, err, imageOutputTooLarge)
	require.Equal(t, maxACPImageDecodedBytes, effectiveOutputLimit(maxACPImageDecodedBytes+1))

	appendFailure := &faultSessionStore{
		SessionStore: NewInMemorySessionStore(),
		appendErr:    errors.New("append failed"),
	}
	_, _, err = (&agentSession{
		agent: NewAgent(WithSessionStore(appendFailure)),
		id:    "failure",
	}).prepareOutputContent(t.Context(), "agent:failure:0", acp.ImageBlock(png, "image/png"), false)
	requireImageOutputError(t, err, imageOutputStorageFailed)

	_, _, err = session.materializeOutputImage(t.Context(), &acp.ContentBlockImage{Type: "image"})
	requireImageOutputError(t, err, imageOutputMissingFile)

	relative := "relative.png"
	_, _, err = session.materializeOutputImage(t.Context(), &acp.ContentBlockImage{Type: "image", Uri: &relative})
	requireImageOutputError(t, err, imageOutputPathNotAllowed)
	for _, location := range []string{"%", "relative.png", "file://remote/tmp/image.png", "file://localhost", "https://example.com/image.png"} {
		_, locationErr := localImagePath(location)
		require.Error(t, locationErr)
	}
	absolute := filepath.Join(workspace, "image.png")
	require.Equal(t, absolute, requireLocalImagePath(t, absolute))
	require.Equal(t, absolute, requireLocalImagePath(t, "file://"+absolute))

	require.False(t, pathWithinAnyRoot(absolute, []string{"", filepath.Join(workspace, "missing")}, true))
	require.True(t, toolContentEqual(acp.ToolCallContent{}, acp.ToolCallContent{}))
	require.False(t, toolContentEqual(acp.ToolCallContent{}, acp.ToolContent(acp.TextBlock("x"))))
	leftImage := acp.ToolContent(acp.ImageBlock("", ""))
	leftURI := "file:///image.png"
	leftImage.Content.Content.Image.Uri = &leftURI
	rightImage := acp.ToolContent(acp.ImageBlock("", ""))
	rightImage.Content.Content.Image.Uri = &leftURI
	require.True(t, toolContentEqual(leftImage, rightImage))
	require.False(t, toolContentEqual(
		acp.ToolContent(acp.TextBlock("left")),
		acp.ToolContent(acp.TextBlock("right")),
	))

	meta := map[string]any{claudeMetaKey: map[string]any{
		"messageId":           "message",
		"_internalImageIndex": float64(2),
	}}
	require.Equal(t, "agent:message:2", agentImageIdentity(meta))

	passthrough, emit, _, err := session.prepareImageUpdateLocked(
		t.Context(),
		acp.UpdateAgentMessage(acp.TextBlock("text")),
		false,
	)
	require.NoError(t, err)
	require.True(t, emit)
	require.NotNil(t, passthrough.AgentMessageChunk)

	session.emittedAgentImages = nil
	imageUpdate := acp.UpdateAgentMessage(acp.ImageBlock(png, "image/png"))
	imageUpdate.AgentMessageChunk.Meta = map[string]any{claudeMetaKey: map[string]any{
		"messageId":           "new-message",
		"_internalImageIndex": 0,
	}}
	_, emit, _, err = session.prepareImageUpdateLocked(t.Context(), imageUpdate, false)
	require.NoError(t, err)
	require.True(t, emit)

	toolStart := acp.StartToolCall(
		"tool-start",
		"start",
		acp.WithStartContent([]acp.ToolCallContent{acp.ToolContent(acp.TextBlock("input"))}),
	)
	toolStart.ToolCall.RawOutput = map[string]any{"url": "https://example.com/?token=secret"}
	preparedUpdate, emit, _, err := session.prepareImageUpdateLocked(t.Context(), toolStart, false)
	require.NoError(t, err)
	require.True(t, emit)
	sanitizedRawOutput, ok := preparedUpdate.ToolCall.RawOutput.(map[string]any)
	require.True(t, ok)
	require.NotContains(t, sanitizedRawOutput["url"], "secret")

	badStart := acp.StartToolCall(
		"tool-bad",
		"bad",
		acp.WithStartContent([]acp.ToolCallContent{acp.ToolContent(acp.ImageBlock("%", "image/png"))}),
	)
	_, _, failedID, err := session.prepareImageUpdateLocked(t.Context(), badStart, false)
	require.Equal(t, acp.ToolCallId("tool-bad"), failedID)
	requireImageOutputError(t, err, imageOutputInvalidBase64)

	other := acp.SessionUpdate{SessionInfoUpdate: &acp.SessionSessionInfoUpdate{}}
	preparedUpdate, emit, _, err = session.prepareImageUpdateLocked(t.Context(), other, false)
	require.NoError(t, err)
	require.True(t, emit)
	require.NotNil(t, preparedUpdate.SessionInfoUpdate)
}

func TestImageOutputReplayValidation(t *testing.T) {
	pngBytes := outputFixtureBytes(t, "valid.png")
	png := base64.StdEncoding.EncodeToString(pngBytes)
	fingerprint := imageFingerprint(pngBytes)
	valid := storedImageArtifact{
		Version:     imageArtifactVersion,
		Identity:    "agent:valid:0",
		Fingerprint: fingerprint,
		MimeType:    "image/png",
		Data:        png,
		URI:         "https://example.com/image.png",
		CreatedAt:   imageArtifactNow().UnixMilli(),
	}
	session := &agentSession{
		agent: NewAgent(WithImageLimits(ImageLimits{
			MaxOutputBytesPerImage:    int64(len(pngBytes)),
			MaxOutputBytesPerToolCall: int64(len(pngBytes)),
		})),
		imageArtifacts: map[string]storedImageArtifact{
			imageArtifactKey(valid.Identity, valid.Fingerprint): valid,
		},
	}
	block, _, err := session.prepareOutputContent(
		t.Context(),
		valid.Identity,
		acp.ImageBlock("ignored", "image/jpeg"),
		true,
	)
	require.NoError(t, err)
	require.Equal(t, valid.URI, *block.Image.Uri)
	require.Empty(t, remoteImageURI("file:///image.png"))
	require.Empty(t, remoteImageURI("%"))

	shifted := valid
	shifted.Identity = "tool:tool-9:0"
	shifted.URI = ""
	session.imageArtifacts[imageArtifactKey(shifted.Identity, shifted.Fingerprint)] = shifted
	block, _, err = session.prepareOutputContent(t.Context(), "tool:tool-9:5", acp.ImageBlock(png, "image/png"), true)
	require.NoError(t, err)
	require.Equal(t, png, block.Image.Data)

	cases := []struct {
		name     string
		artifact storedImageArtifact
		reason   string
		agent    *Agent
	}{
		{
			name: "base64",
			artifact: storedImageArtifact{
				Identity: "agent:base64:0", Fingerprint: fingerprint, MimeType: "image/png", Data: "%",
			},
			reason: imageOutputStorageFailed,
		},
		{
			name: "checksum",
			artifact: storedImageArtifact{
				Identity: "agent:checksum:0", Fingerprint: "wrong", MimeType: "image/png", Data: png,
			},
			reason: imageOutputStorageFailed,
		},
		{
			name: "limit",
			artifact: storedImageArtifact{
				Identity: "agent:limit:0", Fingerprint: fingerprint, MimeType: "image/png", Data: png,
			},
			reason: imageOutputTooLarge,
			agent: NewAgent(WithImageLimits(ImageLimits{
				MaxOutputBytesPerImage:    int64(len(pngBytes) - 1),
				MaxOutputBytesPerToolCall: int64(len(pngBytes) - 1),
			})),
		},
		{
			name: "mime",
			artifact: storedImageArtifact{
				Identity: "agent:mime:0", Fingerprint: fingerprint, MimeType: "image/jpeg", Data: png,
			},
			reason: imageOutputStorageFailed,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			agent := test.agent
			if agent == nil {
				agent = NewAgent()
			}
			replay := &agentSession{
				agent: agent,
				imageArtifacts: map[string]storedImageArtifact{
					imageArtifactKey(test.artifact.Identity, test.artifact.Fingerprint): test.artifact,
				},
			}
			_, _, err := replay.prepareOutputContent(
				t.Context(),
				test.artifact.Identity,
				acp.ImageBlock("ignored", "image/png"),
				true,
			)
			requireImageOutputError(t, err, test.reason)
		})
	}
}

func TestImageOutputFilesystemFailures(t *testing.T) {
	workspace := t.TempDir()
	path := filepath.Join(workspace, "image.png")
	require.NoError(t, os.WriteFile(path, outputFixtureBytes(t, "valid.png"), 0o600))
	session := &agentSession{
		agent: NewAgent(WithImageLimits(ImageLimits{
			MaxOutputBytesPerImage:    447,
			MaxOutputBytesPerToolCall: 447,
		})),
		cwd: workspace,
	}

	originalEval := imageOutputEvalSymlinks
	originalStat := imageOutputStat
	originalOpen := imageOutputOpen
	originalReadAll := imageOutputReadAll
	t.Cleanup(func() {
		imageOutputEvalSymlinks = originalEval
		imageOutputStat = originalStat
		imageOutputOpen = originalOpen
		imageOutputReadAll = originalReadAll
	})

	imageOutputEvalSymlinks = func(string) (string, error) { return "", errors.New("resolve") }
	_, err := session.readAllowedImageFile(t.Context(), path)
	requireImageOutputError(t, err, imageOutputPathNotAllowed)
	imageOutputEvalSymlinks = originalEval

	imageOutputStat = func(string) (os.FileInfo, error) { return nil, os.ErrNotExist }
	_, err = session.readAllowedImageFile(t.Context(), path)
	requireImageOutputError(t, err, imageOutputMissingFile)
	imageOutputStat = func(string) (os.FileInfo, error) { return nil, errors.New("stat") }
	_, err = session.readAllowedImageFile(t.Context(), path)
	requireImageOutputError(t, err, imageOutputPathNotAllowed)
	imageOutputStat = originalStat

	_, err = session.readAllowedImageFile(t.Context(), path)
	requireImageOutputError(t, err, imageOutputTooLarge)

	info, err := os.Stat(path)
	require.NoError(t, err)
	imageOutputStat = func(string) (os.FileInfo, error) {
		return imageSizedFileInfo{FileInfo: info, size: 0}, nil
	}
	imageOutputOpen = func(string) (*os.File, error) { return nil, errors.New("open") }
	_, err = session.readAllowedImageFile(t.Context(), path)
	requireImageOutputError(t, err, imageOutputMissingFile)
	imageOutputOpen = originalOpen

	imageOutputReadAll = func(io.Reader) ([]byte, error) { return nil, errors.New("read") }
	_, err = session.readAllowedImageFile(t.Context(), path)
	requireImageOutputError(t, err, imageOutputMissingFile)
	imageOutputReadAll = originalReadAll

	cancelled, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = session.readAllowedImageFile(cancelled, path)
	require.ErrorIs(t, err, context.Canceled)

	_, err = session.readAllowedImageFile(t.Context(), path)
	requireImageOutputError(t, err, imageOutputTooLarge)
}

type imageSizedFileInfo struct {
	os.FileInfo
	size int64
}

func (i imageSizedFileInfo) Size() int64 {
	return i.size
}

func requireLocalImagePath(t *testing.T, location string) string {
	t.Helper()

	path, err := localImagePath(location)
	require.NoError(t, err)

	return path
}

func TestImageOutputToolSnapshotsAggregateAndFailureFatality(t *testing.T) {
	t.Parallel()

	png := outputFixtureBase64(t, "valid.png")
	agent := NewAgent(
		WithSessionStore(NewInMemorySessionStore()),
		WithImageLimits(ImageLimits{
			MaxOutputBytesPerImage:    1000,
			MaxOutputBytesPerToolCall: 1000,
		}),
	)
	conn := newRecordingAgentClient()
	agent.setConnection(conn)
	session := &agentSession{
		agent:          agent,
		id:             "session-1",
		imageArtifacts: make(map[string]storedImageArtifact),
		toolContent:    make(map[acp.ToolCallId][]acp.ToolCallContent),
	}

	start := acp.StartToolCall(
		"tool-1",
		"Generate",
		acp.WithStartContent([]acp.ToolCallContent{acp.ToolContent(acp.TextBlock("prompt"))}),
	)
	require.NoError(t, session.emitUpdates(t.Context(), []acp.SessionUpdate{start}))

	complete := acp.UpdateToolCall(
		"tool-1",
		acp.WithUpdateStatus(acp.ToolCallStatusCompleted),
		acp.WithUpdateContent([]acp.ToolCallContent{acp.ToolContent(acp.ImageBlock(png, "image/png"))}),
	)
	require.NoError(t, session.emitUpdates(t.Context(), []acp.SessionUpdate{complete}))
	updates := conn.Updates()
	require.Len(t, updates, 2)
	require.Len(t, updates[1].Update.ToolCallUpdate.Content, 2)
	require.Equal(t, "prompt", updates[1].Update.ToolCallUpdate.Content[0].Content.Content.Text.Text)
	require.Equal(t, png, updates[1].Update.ToolCallUpdate.Content[1].Content.Content.Image.Data)

	statusOnly := acp.UpdateToolCall("tool-1", acp.WithUpdateStatus(acp.ToolCallStatusCompleted))
	require.NoError(t, session.emitUpdates(t.Context(), []acp.SessionUpdate{statusOnly}))
	require.Nil(t, conn.Updates()[2].Update.ToolCallUpdate.Content)

	aggregateAgent := NewAgent(
		WithSessionStore(NewInMemorySessionStore()),
		WithImageLimits(ImageLimits{
			MaxOutputBytesPerImage:    1000,
			MaxOutputBytesPerToolCall: 700,
		}),
	)
	aggregateSession := &agentSession{
		agent:          aggregateAgent,
		id:             "session-2",
		imageArtifacts: make(map[string]storedImageArtifact),
		toolContent:    make(map[acp.ToolCallId][]acp.ToolCallContent),
	}
	twoImages := []acp.ToolCallContent{
		acp.ToolContent(acp.ImageBlock(png, "image/png")),
		acp.ToolContent(acp.ImageBlock(png, "image/png")),
	}
	_, aggregateEmit, _, err := aggregateSession.prepareImageUpdateLocked(t.Context(), acp.UpdateToolCall(
		"tool-2",
		acp.WithUpdateContent(twoImages),
	), false)
	require.False(t, aggregateEmit)
	requireImageOutputError(t, err, imageOutputTooLarge)

	var requestErr *acp.RequestError

	require.ErrorAs(t, err, &requestErr)
	aggregateData, ok := requestErr.Data.(map[string]any)
	require.True(t, ok)
	require.EqualValues(t, 896, aggregateData["sizeBytes"])

	// A refused image output fails its own tool call, carries the guidance the
	// model can act on, and leaves the turn running with its context.
	failed := acp.UpdateToolCall(
		"tool-3",
		acp.WithUpdateStatus(acp.ToolCallStatusCompleted),
		acp.WithUpdateContent([]acp.ToolCallContent{acp.ToolContent(acp.ImageBlock("%", "image/png"))}),
	)
	require.NoError(t, session.emitUpdates(t.Context(), []acp.SessionUpdate{failed}))
	requireImageRefusal(t, conn.Updates()[len(conn.Updates())-1].Update, imageGuidanceInvalidBase64)

	// The turn keeps making progress after the refusal.
	require.NoError(t, session.emitUpdates(t.Context(), []acp.SessionUpdate{acp.UpdateAgentMessageText("still here")}))
	require.Equal(t, "still here", conn.Updates()[len(conn.Updates())-1].Update.AgentMessageChunk.Content.Text.Text)

	// An agent image has no tool call to attribute to, so the guidance takes
	// the image's place rather than vanishing.
	agentRefusal := acp.UpdateAgentMessage(acp.ImageBlock("%", "image/png"))
	require.NoError(t, session.emitUpdates(t.Context(), []acp.SessionUpdate{agentRefusal}))
	requireImageRefusal(t, conn.Updates()[len(conn.Updates())-1].Update, imageGuidanceInvalidBase64)

	// The artifact store no longer holding a replayed image is the adapter's
	// own durability breaking, so it still ends the turn. The tool call reports
	// failed for attribution and carries no guidance: there is nothing to retry.
	lost := acp.UpdateToolCall("tool-4", acp.WithUpdateContent([]acp.ToolCallContent{
		acp.ToolContent(acp.ImageBlock(png, "image/png")),
	}))
	err = session.emitUpdatesWithAssistantIdentity(t.Context(), []acp.SessionUpdate{lost}, "", true)
	requireImageOutputError(t, err, imageOutputStorageFailed)
	lostUpdate := conn.Updates()[len(conn.Updates())-1].Update.ToolCallUpdate
	require.Equal(t, acp.ToolCallStatusFailed, *lostUpdate.Status)
	require.Nil(t, lostUpdate.Content)

	failingConn := newRecordingAgentClient()
	failingConn.sessionUpdateErr = errors.New("failed update")
	failingAgent := NewAgent(WithSessionStore(NewInMemorySessionStore()))
	failingAgent.setConnection(failingConn)
	failingSession := &agentSession{
		agent: failingAgent,
		id:    "failure",
	}
	err = failingSession.emitUpdates(t.Context(), []acp.SessionUpdate{failed})
	require.ErrorContains(t, err, "failed update")

	dedupConn := newRecordingAgentClient()
	dedupAgent := NewAgent(WithSessionStore(NewInMemorySessionStore()))
	dedupAgent.setConnection(dedupConn)
	dedupSession := &agentSession{agent: dedupAgent, id: "dedup"}
	firstAgentImage := acp.UpdateAgentMessage(acp.ImageBlock(png, "image/png"))
	firstAgentImage.AgentMessageChunk.Meta = map[string]any{claudeMetaKey: map[string]any{
		"messageId":           "message",
		"_internalImageIndex": 0,
	}}
	require.NoError(t, dedupSession.emitUpdates(t.Context(), []acp.SessionUpdate{firstAgentImage}))
	repeatedAgentImage := acp.UpdateAgentMessage(acp.ImageBlock(png, "image/png"))
	repeatedAgentImage.AgentMessageChunk.Meta = map[string]any{claudeMetaKey: map[string]any{
		"messageId":           "message",
		"_internalImageIndex": 0,
	}}
	require.NoError(t, dedupSession.emitUpdates(t.Context(), []acp.SessionUpdate{repeatedAgentImage}))
	require.Len(t, dedupConn.Updates(), 1)
}

func TestImageTranscriptCanonicalStoreAndSweep(t *testing.T) {
	png := outputFixtureBase64(t, "valid.png")
	agent := NewAgent(WithSessionStore(NewInMemorySessionStore()))
	session := &agentSession{
		agent:          agent,
		id:             "session-1",
		imageArtifacts: make(map[string]storedImageArtifact),
	}
	decoded, err := base64.StdEncoding.DecodeString(png)
	require.NoError(t, err)
	_, err = session.persistImageArtifact(
		t.Context(),
		"tool:tool-1:1",
		imageFingerprint(decoded),
		"image/png",
		png,
		"",
	)
	require.NoError(t, err)

	entry := SessionStoreEntry(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tool-1","content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + png + `"}}]}]}}`)
	sanitized, err := session.sanitizeTranscriptImageEntries(t.Context(), []SessionStoreEntry{entry})
	require.NoError(t, err)
	require.NotContains(t, string(sanitized[0]), png)
	require.Contains(t, string(sanitized[0]), transcriptArtifactSourceType)

	rehydrated, err := rehydrateTranscriptImageEntries(sanitized, session.imageArtifacts)
	require.NoError(t, err)
	require.Contains(t, string(rehydrated[0]), png)

	oldNow := imageArtifactNow
	imageArtifactNow = func() time.Time { return time.Unix(100000, 0) }
	t.Cleanup(func() { imageArtifactNow = oldNow })

	old := storedImageArtifact{
		Version:     imageArtifactVersion,
		Identity:    "agent:old:0",
		Fingerprint: imageFingerprint(decoded),
		MimeType:    "image/png",
		Data:        png,
		CreatedAt:   imageArtifactNow().Add(-imageArtifactTTL - time.Second).UnixMilli(),
	}
	raw, err := json.Marshal(old)
	require.NoError(t, err)
	store := NewInMemorySessionStore()
	oldKey := imageArtifactKey(old.Identity, old.Fingerprint)
	require.NoError(t, store.Append(t.Context(), SessionKey{SessionID: "old", Subpath: oldKey}, []SessionStoreEntry{raw}))
	sweepAgent := NewAgent(WithSessionStore(store))
	loaded, err := sweepAgent.loadImageArtifacts(t.Context(), "old")
	require.NoError(t, err)
	require.Empty(t, loaded)
	entries, err := store.Load(t.Context(), SessionKey{SessionID: "old", Subpath: oldKey})
	require.NoError(t, err)
	require.Empty(t, entries)

	ref := SessionStoreEntry(`{"type":"assistant","message":{"content":[{"type":"image","source":{"type":"acp_artifact","artifact_key":"` + oldKey + `","media_type":"image/png"}}]}}`)
	_, err = rehydrateTranscriptImageEntries([]SessionStoreEntry{ref}, loaded)
	requireImageOutputError(t, err, imageOutputStorageFailed)
}

func TestImageArtifactForkCopy(t *testing.T) {
	t.Parallel()

	store := NewInMemorySessionStore()
	agent := NewAgent(WithSessionStore(store))
	png := outputFixtureBase64(t, "valid.png")
	decoded, err := base64.StdEncoding.DecodeString(png)
	require.NoError(t, err)
	artifact := storedImageArtifact{
		Version:     imageArtifactVersion,
		Identity:    "agent:message-1:0",
		Fingerprint: imageFingerprint(decoded),
		MimeType:    "image/png",
		Data:        png,
		CreatedAt:   imageArtifactNow().UnixMilli(),
	}
	sourceKey := imageArtifactKey(artifact.Identity, artifact.Fingerprint)
	raw, err := json.Marshal(artifact)
	require.NoError(t, err)
	require.NoError(t, store.Append(t.Context(), SessionKey{
		SessionID: "parent",
		Subpath:   sourceKey,
	}, []SessionStoreEntry{raw}))

	loaded, err := agent.loadImageArtifacts(t.Context(), "parent")
	require.NoError(t, err)
	require.NoError(t, agent.copyImageArtifacts(t.Context(), "child", loaded))
	child, err := store.Load(t.Context(), SessionKey{SessionID: "child", Subpath: sourceKey})
	require.NoError(t, err)
	require.Equal(t, []SessionStoreEntry{raw}, child)
}

func TestImageArtifactStoreEdges(t *testing.T) {
	t.Parallel()

	agent := NewAgent()
	artifacts, err := agent.loadImageArtifacts(t.Context(), "")
	require.NoError(t, err)
	require.Empty(t, artifacts)

	listFailure := &faultSessionStore{
		SessionStore:   NewInMemorySessionStore(),
		listSubkeysErr: errors.New("list"),
	}
	_, err = NewAgent(WithSessionStore(listFailure)).loadImageArtifacts(t.Context(), "session")
	require.ErrorContains(t, err, "list image artifacts")

	loadFailureStore := NewInMemorySessionStore()
	require.NoError(t, loadFailureStore.Append(t.Context(), SessionKey{
		SessionID: "session",
		Subpath:   imageArtifactPrefix + "failed.json",
	}, []SessionStoreEntry{json.RawMessage(`{}`)}))
	loadFailure := &faultSessionStore{
		SessionStore:   loadFailureStore,
		loadSubpathErr: errors.New("load"),
	}
	_, err = NewAgent(WithSessionStore(loadFailure)).loadImageArtifacts(t.Context(), "session")
	require.ErrorContains(t, err, "load image artifact")

	png := outputFixtureBase64(t, "valid.png")
	decoded, err := base64.StdEncoding.DecodeString(png)
	require.NoError(t, err)
	valid := storedImageArtifact{
		Version:     imageArtifactVersion,
		Identity:    "agent:message:0",
		Fingerprint: imageFingerprint(decoded),
		MimeType:    "image/png",
		Data:        png,
		CreatedAt:   imageArtifactNow().UnixMilli(),
	}
	validRaw, err := json.Marshal(valid)
	require.NoError(t, err)

	const mixedSessionID = "11111111-1111-4111-8111-111111111111"

	mixed := NewInMemorySessionStore()
	require.NoError(t, mixed.Append(t.Context(), SessionKey{
		SessionID: mixedSessionID,
	}, []SessionStoreEntry{json.RawMessage(`{"type":"user","message":{"content":"hello"}}`)}))
	require.NoError(t, mixed.Append(t.Context(), SessionKey{
		SessionID: mixedSessionID,
		Subpath:   "subagents/ignored",
	}, []SessionStoreEntry{json.RawMessage(`{}`)}))
	require.NoError(t, mixed.Append(t.Context(), SessionKey{
		SessionID: mixedSessionID,
		Subpath:   imageArtifactPrefix + "multiple.json",
	}, []SessionStoreEntry{json.RawMessage(`{}`), json.RawMessage(`{}`)}))
	require.NoError(t, mixed.Append(t.Context(), SessionKey{
		SessionID: mixedSessionID,
		Subpath:   imageArtifactPrefix + "malformed.json",
	}, []SessionStoreEntry{json.RawMessage(`{`)}))
	validKey := imageArtifactKey(valid.Identity, valid.Fingerprint)
	require.NoError(t, mixed.Append(t.Context(), SessionKey{
		SessionID: mixedSessionID,
		Subpath:   validKey,
	}, []SessionStoreEntry{validRaw}))
	loaded, err := NewAgent(WithSessionStore(mixed)).loadImageArtifacts(t.Context(), mixedSessionID)
	require.NoError(t, err)
	require.Equal(t, map[string]storedImageArtifact{validKey: valid}, loaded)

	materializeAgent := NewAgent(
		WithSessionStore(mixed),
		WithScratchDir(t.TempDir()),
	)
	materialized, err := materializeAgent.materializeStoreSession(
		t.Context(),
		mixedSessionID,
		t.TempDir(),
		t.TempDir(),
	)
	require.NoError(t, err)
	require.NotNil(t, materialized)
	matches, err := filepath.Glob(filepath.Join(materialized.configDir, "projects", "*", mixedSessionID, "_artifacts", "*"))
	require.NoError(t, err)
	require.Empty(t, matches)
	require.NoError(t, materialized.Close())

	expired := valid
	expired.Identity = "agent:expired:0"
	expired.CreatedAt = imageArtifactNow().Add(-imageArtifactTTL - time.Second).UnixMilli()
	expiredRaw, err := json.Marshal(expired)
	require.NoError(t, err)
	expiredKey := imageArtifactKey(expired.Identity, expired.Fingerprint)
	deleteFailureBase := NewInMemorySessionStore()
	require.NoError(t, deleteFailureBase.Append(t.Context(), SessionKey{
		SessionID: "session",
		Subpath:   expiredKey,
	}, []SessionStoreEntry{expiredRaw}))
	deleteFailure := &faultSessionStore{
		SessionStore: deleteFailureBase,
		deleteErr:    errors.New("delete"),
	}
	_, err = NewAgent(WithSessionStore(deleteFailure)).loadImageArtifacts(t.Context(), "session")
	require.ErrorContains(t, err, "delete expired image artifact")

	copyFailure := &faultSessionStore{
		SessionStore: NewInMemorySessionStore(),
		appendErr:    errors.New("append"),
	}
	err = NewAgent(WithSessionStore(copyFailure)).copyImageArtifacts(
		t.Context(),
		"child",
		map[string]storedImageArtifact{validKey: valid},
	)
	require.ErrorContains(t, err, "store forked image artifact")

	session := &agentSession{
		agent: NewAgent(WithSessionStore(NewInMemorySessionStore())),
		id:    "session",
	}
	stored, err := session.persistImageArtifact(
		t.Context(),
		valid.Identity,
		valid.Fingerprint,
		valid.MimeType,
		valid.Data,
		valid.URI,
	)
	require.NoError(t, err)
	require.Equal(t, valid.Identity, stored.Identity)
	again, err := session.persistImageArtifact(
		t.Context(),
		valid.Identity,
		valid.Fingerprint,
		valid.MimeType,
		valid.Data,
		valid.URI,
	)
	require.NoError(t, err)
	require.Equal(t, stored, again)

	found, ok := session.imageArtifactByIdentity(valid.Identity)
	require.True(t, ok)
	require.Equal(t, stored, found)
	_, ok = session.imageArtifactByIdentity("missing")
	require.False(t, ok)
	found, ok = session.imageArtifactByFingerprint("agent:", valid.Fingerprint)
	require.True(t, ok)
	require.Equal(t, stored, found)
	_, ok = session.imageArtifactByFingerprint("tool:", valid.Fingerprint)
	require.False(t, ok)

	require.True(t, validStoredImageArtifact(valid))
	invalid := valid
	invalid.Data = "%"
	require.False(t, validStoredImageArtifact(invalid))
	invalid = valid
	invalid.Fingerprint = "wrong"
	require.False(t, validStoredImageArtifact(invalid))
	invalid = valid
	invalid.MimeType = "image/jpeg"
	require.False(t, validStoredImageArtifact(invalid))
}

func TestImageTranscriptEdges(t *testing.T) {
	t.Parallel()

	png := outputFixtureBase64(t, "valid.png")
	decoded, err := base64.StdEncoding.DecodeString(png)
	require.NoError(t, err)
	fingerprint := imageFingerprint(decoded)
	agentArtifact := storedImageArtifact{
		Version:     imageArtifactVersion,
		Identity:    "agent:message:0",
		Fingerprint: fingerprint,
		MimeType:    "image/png",
		Data:        png,
		CreatedAt:   imageArtifactNow().UnixMilli(),
	}
	toolArtifact := agentArtifact
	toolArtifact.Identity = "tool:tool-1:0"
	session := &agentSession{imageArtifacts: map[string]storedImageArtifact{
		imageArtifactKey(agentArtifact.Identity, agentArtifact.Fingerprint): agentArtifact,
		imageArtifactKey(toolArtifact.Identity, toolArtifact.Fingerprint):   toolArtifact,
	}}

	entries := []SessionStoreEntry{
		json.RawMessage(`not-json`),
		json.RawMessage(`{"type":"other"}`),
		json.RawMessage(`{"type":"assistant","message":{"id":"message","content":[null,{"type":"text","text":"x"},{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + png + `"}}]}}`),
		json.RawMessage(`{"type":"user","message":{"content":[null,{"type":"text","text":"x"},{"type":"tool_result","tool_use_id":"tool-1","content":{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + png + `"}}}]}}`),
		json.RawMessage(`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tool-1","content":[null,{"type":"text","text":"x"}]}]}}`),
	}
	sanitized, err := session.sanitizeTranscriptImageEntries(t.Context(), entries)
	require.NoError(t, err)
	require.Equal(t, entries[0], sanitized[0])
	require.NotContains(t, string(sanitized[2]), png)
	require.NotContains(t, string(sanitized[3]), png)

	rehydrated, err := rehydrateTranscriptImageEntries(sanitized, session.imageArtifacts)
	require.NoError(t, err)
	require.Equal(t, sanitized[0], rehydrated[0])
	require.Contains(t, string(rehydrated[2]), png)
	require.Contains(t, string(rehydrated[3]), png)

	noData := map[string]any{jsonFieldType: "image"}
	require.NoError(t, session.replaceTranscriptImageData(t.Context(), noData, "agent:none:0"))

	invalidBase64 := map[string]any{
		jsonFieldType: "image",
		"source": map[string]any{
			jsonFieldType: "base64",
			"media_type":  "image/png",
			"data":        "%",
		},
	}
	err = session.replaceTranscriptImageData(t.Context(), invalidBase64, "tool:missing:0")
	requireImageOutputError(t, err, imageOutputInvalidBase64)

	notRaster := map[string]any{
		jsonFieldType: "image",
		"source": map[string]any{
			jsonFieldType: "base64",
			"media_type":  "image/png",
			"data":        base64.StdEncoding.EncodeToString([]byte("not a raster image")),
		},
	}
	err = session.replaceTranscriptImageData(t.Context(), notRaster, "tool:notraster:0")
	requireImageOutputError(t, err, imageOutputNotRaster)

	emptySession := &agentSession{imageArtifacts: map[string]storedImageArtifact{}}
	toolMapEntry := json.RawMessage(
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tool-x",` +
			`"content":{"type":"image","source":{"type":"base64","media_type":"image/png","data":"%"}}}]}}`,
	)
	_, err = emptySession.sanitizeTranscriptImageEntries(t.Context(), []SessionStoreEntry{toolMapEntry})
	requireImageOutputError(t, err, imageOutputInvalidBase64)

	toolArrayEntry := json.RawMessage(
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"tool-x",` +
			`"content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"%"}}]}]}}`,
	)
	_, err = emptySession.sanitizeTranscriptImageEntries(t.Context(), []SessionStoreEntry{toolArrayEntry})
	requireImageOutputError(t, err, imageOutputInvalidBase64)

	badRef := json.RawMessage(`{"type":"assistant","message":{"content":[{"type":"image","source":{"type":"acp_artifact","artifact_key":"bad"}}]}}`)
	_, err = rehydrateTranscriptImageEntries(
		[]SessionStoreEntry{badRef},
		map[string]storedImageArtifact{"bad": {Data: "%"}},
	)
	requireImageOutputError(t, err, imageOutputStorageFailed)

	replaySession := &agentSession{imageArtifacts: map[string]storedImageArtifact{
		imageArtifactKey(agentArtifact.Identity, agentArtifact.Fingerprint): agentArtifact,
	}}
	err = replaySession.replayTranscriptEntries(t.Context(), []SessionStoreEntry{badRef})
	requireImageOutputError(t, err, imageOutputStorageFailed)

	// A mirror frame that reaches the store before the live emission that
	// normally persists its bytes must not turn-fatal with a false
	// storage_failed: the frame's own bytes are persisted and referenced.
	home := t.TempDir()
	mirrorStore := NewInMemorySessionStore()
	mirrorSession := &agentSession{
		agent:          NewAgent(WithSessionStore(mirrorStore)),
		id:             "11111111-1111-4111-8111-111111111111",
		imageArtifacts: make(map[string]storedImageArtifact),
	}
	mirror := newSessionMirror(nil, mirrorStore, home, mirrorSession)
	err = mirror.appendFrame(t.Context(), &claude.TranscriptMirrorMessage{
		FilePath: filepath.Join(
			home,
			"projects",
			"workspace",
			"11111111-1111-4111-8111-111111111111.jsonl",
		),
		Entries: []SessionStoreEntry{
			json.RawMessage(`{"type":"assistant","uuid":"missing","message":{"content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"` + png + `"}}]}}`),
		},
	})
	require.NoError(t, err)
	require.Len(t, mirrorSession.imageArtifacts, 1)

	// A mirror frame whose bytes are unusable still fails the append rather than
	// storing a broken artifact.
	badMirror := newSessionMirror(nil, NewInMemorySessionStore(), home, &agentSession{
		imageArtifacts: map[string]storedImageArtifact{},
	})
	err = badMirror.appendFrame(t.Context(), &claude.TranscriptMirrorMessage{
		FilePath: filepath.Join(
			home,
			"projects",
			"workspace",
			"22222222-2222-4222-8222-222222222222.jsonl",
		),
		Entries: []SessionStoreEntry{
			json.RawMessage(`{"type":"assistant","uuid":"broken","message":{"content":[{"type":"image","source":{"type":"base64","media_type":"image/png","data":"%"}}]}}`),
		},
	})
	requireImageOutputError(t, err, imageOutputInvalidBase64)
}

func TestRawImageDiagnosticsExcludeBase64AndSignedQuery(t *testing.T) {
	t.Parallel()

	png := outputFixtureBase64(t, "valid.png")
	raw := rawClaudeMessage(&claude.UserMessage{Raw: map[string]any{
		"type": "user",
		"message": map[string]any{"content": []any{map[string]any{
			"type": "image",
			"source": map[string]any{
				"type":       "base64",
				"media_type": "image/png",
				"data":       png,
				"url":        "https://example.com/image.png?token=secret",
			},
		}}},
	}})
	encoded, err := json.Marshal(raw)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), png)
	require.NotContains(t, string(encoded), "secret")
	require.Contains(t, string(encoded), `"sizeBytes":448`)
	require.Contains(t, string(encoded), `"sha256"`)

	withoutData := map[string]any{jsonFieldType: "image"}
	sanitizeDiagnosticImage(withoutData, withoutData)
	require.NotContains(t, withoutData, "imageMetadata")

	invalid := map[string]any{
		jsonFieldType: "image",
		"data":        "%",
		"mimeType":    "image/png",
	}
	sanitized, ok := sanitizeDiagnosticValue(invalid).(map[string]any)
	require.True(t, ok)
	require.NotContains(t, sanitized, "data")
	invalidMetadata, ok := sanitized["imageMetadata"].(map[string]any)
	require.True(t, ok)
	require.Equal(t, "image/png", invalidMetadata["mimeType"])
	require.NotContains(t, invalidMetadata, "sha256")

	require.Equal(t, "data:[redacted]", redactDiagnosticURI("data:image/png;base64,abc"))
	require.Equal(t, "%", redactDiagnosticURI("%"))
	require.Equal(t, "file:///tmp/image.png", redactDiagnosticURI("file:///tmp/image.png"))
}

// narrowTempDir points os.TempDir at dir for the duration of one test, so a
// case can hold a directory the adapter must refuse even though the real temp
// directory is an allowed root. Every variable os.TempDir consults on any
// supported platform is set.
func narrowTempDir(t *testing.T, dir string) {
	t.Helper()

	for _, name := range []string{"TMPDIR", "TMP", "TEMP"} {
		t.Setenv(name, dir)
	}

	require.Equal(t, dir, os.TempDir())
}

// requireImageRefusal asserts a recoverable image-output verdict reached the
// client as its own update and left the turn running.
func requireImageRefusal(t *testing.T, update acp.SessionUpdate, guidance string) {
	t.Helper()

	if update.ToolCallUpdate != nil {
		require.NotNil(t, update.ToolCallUpdate.Status)
		require.Equal(t, acp.ToolCallStatusFailed, *update.ToolCallUpdate.Status)
		require.Len(t, update.ToolCallUpdate.Content, 1)
		require.NotNil(t, update.ToolCallUpdate.Content[0].Content)
		require.NotNil(t, update.ToolCallUpdate.Content[0].Content.Content.Text)
		require.Equal(t, guidance, update.ToolCallUpdate.Content[0].Content.Content.Text.Text)

		return
	}

	require.NotNil(t, update.AgentMessageChunk)
	require.NotNil(t, update.AgentMessageChunk.Content.Text)
	require.Equal(t, guidance, update.AgentMessageChunk.Content.Text.Text)
}

func requireImageOutputError(t *testing.T, err error, reason string) {
	t.Helper()

	var requestErr *acp.RequestError

	require.ErrorAs(t, err, &requestErr)
	require.Equal(t, -32603, requestErr.Code)
	data, ok := requestErr.Data.(map[string]any)
	require.True(t, ok)
	require.Equal(t, turnFailedError, data[jsonFieldError])
	require.Equal(t, failureCauseTransport, data[failureFieldCause])
	require.Equal(t, imageOutputStage, data["stage"])
	require.Equal(t, reason, data["reason"])
}

func outputFixtureBytes(t *testing.T, name string) []byte {
	t.Helper()

	data, err := os.ReadFile(filepath.Join("internal", "mapper", "testdata", "images", name))
	require.NoError(t, err)

	return data
}

func outputFixtureBase64(t *testing.T, name string) string {
	t.Helper()

	return base64.StdEncoding.EncodeToString(outputFixtureBytes(t, name))
}

func validBMP() []byte {
	data := make([]byte, 58)
	copy(data, "BM")
	binary.LittleEndian.PutUint32(data[2:6], uint32(len(data)))
	binary.LittleEndian.PutUint32(data[10:14], 54)
	binary.LittleEndian.PutUint32(data[14:18], 40)
	binary.LittleEndian.PutUint32(data[18:22], 1)
	binary.LittleEndian.PutUint32(data[22:26], 1)
	binary.LittleEndian.PutUint16(data[26:28], 1)
	binary.LittleEndian.PutUint16(data[28:30], 24)

	return data
}

func TestImageLimitConstruction(t *testing.T) {
	t.Parallel()

	agent := NewAgent(WithImageLimits(ImageLimits{MaxInputBytesPerImage: -1}))
	_, err := agent.Initialize(t.Context(), acp.InitializeRequest{})
	var requestErr *acp.RequestError
	require.ErrorAs(t, err, &requestErr)

	// The caller's initialize params were fine; the agent this host built was not.
	require.Equal(t, -32603, requestErr.Code)
	require.Equal(t, "Internal error", requestErr.Message)
}

func TestImageScratchDirectoryLifecycle(t *testing.T) {
	parent := t.TempDir()
	path, err := createImageScratchDir(parent)
	require.NoError(t, err)
	info, err := os.Stat(path)
	require.NoError(t, err)
	require.Equal(t, os.FileMode(0o700), info.Mode().Perm())
	require.True(t, strings.HasPrefix(filepath.Base(path), "acp-go-claude-images-"))
	require.NoError(t, os.RemoveAll(path))

	originalMkdirTemp := imageScratchMkdirTemp
	t.Cleanup(func() { imageScratchMkdirTemp = originalMkdirTemp })
	imageScratchMkdirTemp = func(string, string) (string, error) {
		return "", errors.New("mkdir")
	}
	_, err = createImageScratchDir(parent)
	require.ErrorContains(t, err, "create image scratch dir")

	imageScratchMkdirTemp = func(string, string) (string, error) {
		return "\x00", nil
	}
	_, err = createImageScratchDir(parent)
	require.ErrorContains(t, err, "set image scratch permissions")

	blocker := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(blocker, nil, 0o600))
	imageScratchMkdirTemp = originalMkdirTemp
	_, err = createImageScratchDir(filepath.Join(blocker, "child"))
	require.Error(t, err)
}
