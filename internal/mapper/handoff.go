package mapper

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"math"
	"net/url"
	"path/filepath"
	"strings"

	"github.com/coder/acp-go-sdk"
)

const (
	// MetaKeyHandoff is the reserved content-block metadata key carrying the
	// local-handoff envelope for an empty-data prompt image.
	MetaKeyHandoff = "acp-go.dev/handoff"
	// HandoffVersion is the only handoff envelope version this adapter accepts.
	HandoffVersion = 1

	// HandoffPathNotAllowed and HandoffMissingFile are the verdicts a refused
	// handoff read reports through HandoffPathError.
	HandoffPathNotAllowed = errPathNotAllowed
	HandoffMissingFile    = errMissingFile

	handoffFieldVersion   = "version"
	handoffFieldDigest    = "digest"
	handoffFieldSizeBytes = "sizeBytes"
	handoffFieldCount     = 3

	digestHexLength = 64
	uriSchemeFile   = "file"
	uriHostLocal    = "localhost"
)

// HandoffFileReader reads the bytes of a handoff-form prompt image. An
// implementation confines the path to the adapter's configured handoff read
// root and reports a refusal as a HandoffPathError.
type HandoffFileReader interface {
	ReadHandoffImage(ctx context.Context, path string, maxBytes int64) (HandoffFile, error)
}

// HandoffFile is one bounded handoff-file read. Data holds at most maxBytes+1
// bytes, so Truncated reports a file that outgrew the per-image gate and whose
// digest therefore cannot be verified. Size is the file's real size, which is
// what a byte-limit rejection reports.
type HandoffFile struct {
	Data      []byte
	Size      int64
	Truncated bool
}

// HandoffPathError is a refused handoff read carrying the input verdict to
// report for it.
type HandoffPathError struct {
	Verdict string
	Message string
}

func (e *HandoffPathError) Error() string {
	return e.Message
}

type handoffEnvelope struct {
	digest    string
	sizeBytes int64
}

// promptImageHandoffForm reports handoff intent on an empty-data image block: a
// handoff envelope key, or a uri naming a local file. A block with neither
// signal never becomes a handoff verdict.
func promptImageHandoffForm(image *acp.ContentBlockImage) bool {
	if _, ok := image.Meta[MetaKeyHandoff]; ok {
		return true
	}

	if image.Uri == nil {
		return false
	}

	parsed, err := url.Parse(*image.Uri)

	return err == nil && parsed.Scheme == uriSchemeFile
}

// handoffImageData resolves a handoff-form image block to the base64 payload
// the native request carries. The envelope and the uri are validated as block
// structure, the file is read through the caller's root-confined reader, its
// digest is verified whenever the whole file fit inside the read bound, and the
// embedded-form gate chain then runs on the bytes.
func handoffImageData(
	ctx context.Context,
	reader HandoffFileReader,
	image *acp.ContentBlockImage,
	index int,
	promptBytes *int64,
	limits ImageInputLimits,
) (string, error) {
	if reader == nil {
		return "", handoffInputError(errInvalidHandoff, index, "no handoff read root is configured")
	}

	envelope, err := parseHandoffEnvelope(image.Meta, index)
	if err != nil {
		return "", err
	}

	path, err := handoffFilePath(image.Uri, index)
	if err != nil {
		return "", err
	}

	bound := handoffReadBound(limits)

	file, err := reader.ReadHandoffImage(ctx, path, bound)
	if err != nil {
		var refused *HandoffPathError
		if errors.As(err, &refused) {
			return "", handoffInputError(refused.Verdict, index, refused.Message)
		}

		return "", err
	}

	if !file.Truncated {
		if err := verifyHandoffBytes(file.Data, envelope, index); err != nil {
			return "", err
		}
	}

	if !portableImageMIME(image.MimeType) {
		return "", imageInputError(errInvalidMediaType, index, 0, 0)
	}

	if err := validateRasterBytes(file.Data, file.Size, image.MimeType, index, promptBytes, handoffGates(limits, file)); err != nil {
		return "", err
	}

	return base64.StdEncoding.EncodeToString(file.Data), nil
}

// handoffReadBound bounds the read even when the per-image policy gate is
// disabled: reading a host-named file unbounded is never an option, so the
// decoded-frame clamp stands in as the ceiling.
func handoffReadBound(limits ImageInputLimits) int64 {
	if limits.MaxBytesPerImage > 0 {
		return limits.MaxBytesPerImage
	}

	return MaxDecodedFrameBytes
}

// handoffGates keeps a file whose digest could not be verified from ever being
// forwarded. A truncated read means the file outgrew the read bound, so when no
// per-image policy gate would reject it the read bound becomes the gate and the
// byte verdict fires in its pinned position in the chain.
func handoffGates(limits ImageInputLimits, file HandoffFile) ImageInputLimits {
	if file.Truncated && limits.MaxBytesPerImage <= 0 {
		limits.MaxBytesPerImage = MaxDecodedFrameBytes
	}

	return limits
}

func parseHandoffEnvelope(meta map[string]any, index int) (handoffEnvelope, error) {
	value, ok := meta[MetaKeyHandoff]
	if !ok {
		return handoffEnvelope{}, handoffInputError(errInvalidHandoff, index, "handoff metadata is missing")
	}

	object, ok := value.(map[string]any)
	if !ok || len(object) != handoffFieldCount {
		return handoffEnvelope{}, handoffInputError(
			errInvalidHandoff,
			index,
			"handoff metadata must contain exactly version, digest, and sizeBytes",
		)
	}

	if !handoffVersionSupported(object[handoffFieldVersion]) {
		return handoffEnvelope{}, handoffInputError(errInvalidHandoff, index, "unsupported handoff metadata version")
	}

	digest, ok := object[handoffFieldDigest].(string)
	if !ok || !lowercaseHexDigest(digest) {
		return handoffEnvelope{}, handoffInputError(
			errInvalidHandoff,
			index,
			"handoff digest must be 64 lowercase hexadecimal characters",
		)
	}

	size, ok := handoffSizeBytes(object[handoffFieldSizeBytes])
	if !ok {
		return handoffEnvelope{}, handoffInputError(
			errInvalidHandoff,
			index,
			"handoff sizeBytes must be a non-negative integer",
		)
	}

	return handoffEnvelope{digest: digest, sizeBytes: size}, nil
}

func handoffVersionSupported(value any) bool {
	switch version := value.(type) {
	case int:
		return version == HandoffVersion
	case float64:
		return version == HandoffVersion
	default:
		return false
	}
}

func lowercaseHexDigest(digest string) bool {
	if len(digest) != digestHexLength {
		return false
	}

	for _, char := range digest {
		if (char < '0' || char > '9') && (char < 'a' || char > 'f') {
			return false
		}
	}

	return true
}

func handoffSizeBytes(value any) (int64, bool) {
	switch size := value.(type) {
	case int:
		return int64(size), size >= 0
	case int64:
		return size, size >= 0
	case float64:
		if size != math.Trunc(size) || size < 0 || size > math.MaxInt64 {
			return 0, false
		}

		return int64(size), true
	default:
		return 0, false
	}
}

func handoffFilePath(uri *string, index int) (string, error) {
	if uri == nil || strings.TrimSpace(*uri) == "" {
		return "", handoffInputError(errInvalidHandoff, index, "handoff block carries no uri")
	}

	parsed, err := url.Parse(*uri)
	if err != nil {
		return "", handoffInputError(errInvalidHandoff, index, "handoff uri cannot be parsed")
	}

	if parsed.Scheme != uriSchemeFile {
		return "", handoffInputError(errInvalidHandoff, index, "handoff uri scheme must be file")
	}

	if parsed.Host != "" && parsed.Host != uriHostLocal {
		return "", handoffInputError(errInvalidHandoff, index, "handoff uri host is not local")
	}

	if !filepath.IsAbs(parsed.Path) {
		return "", handoffInputError(errInvalidHandoff, index, "handoff uri path must be absolute")
	}

	return filepath.Clean(parsed.Path), nil
}

// verifyHandoffBytes fails closed: bytes that do not match the declared size
// and digest are rejected outright and never fall back to another form.
func verifyHandoffBytes(data []byte, envelope handoffEnvelope, index int) error {
	if int64(len(data)) != envelope.sizeBytes {
		return handoffInputError(
			errHandoffDigestMismatch,
			index,
			"handoff file size does not match the declared sizeBytes",
		)
	}

	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != envelope.digest {
		return handoffInputError(
			errHandoffDigestMismatch,
			index,
			"handoff file bytes do not match the declared digest",
		)
	}

	return nil
}

func handoffInputError(reason string, index int, message string) error {
	return acp.NewInvalidParams(map[string]any{
		keyFieldField: fieldPromptImage,
		keyErrorField: reason,
		keyIndex:      index,
		keyMessage:    message,
	})
}
