package mapper

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
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

	// maxHandoffBlocksPerPrompt bounds the files one prompt may name. The
	// handoff form deliberately decouples the bytes an adapter reads from the
	// frame that asked for them, so the block count is what keeps a single
	// prompt from multiplying the per-image bound without limit.
	maxHandoffBlocksPerPrompt = 64

	// maxHandoffSizeBytes is one past the largest int64, which is the first
	// float64 a declared sizeBytes may not be. The bound is spelled out rather
	// than derived from math.MaxInt64, which is not representable as a float64
	// and rounds up when converted.
	maxHandoffSizeBytes = 9223372036854775808.0

	// handoffEnvelopeMismatch is the single answer for bytes that disagree with
	// the envelope, whichever field disagreed.
	handoffEnvelopeMismatch = "handoff file does not match the declared envelope"
)

// HandoffFileReader opens a handoff-form prompt image. An implementation
// confines path to the adapter's configured handoff read root, admits only a
// regular file, and reports a refusal as a HandoffPathError. It reports no size
// of its own: the caller bounds the read and the bytes it gets back are the only
// size any verdict may consult.
type HandoffFileReader interface {
	OpenHandoffImage(ctx context.Context, path string) (io.ReadCloser, error)
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
// the native request carries. Block structure is validated first, then the file
// is located and opened through the caller's root-confined reader, then the
// media type and the declared size are checked before a single byte is read, and
// only bytes that match the declared digest reach the embedded gate chain.
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

	file, err := reader.OpenHandoffImage(ctx, path)
	if err != nil {
		var refused *HandoffPathError
		if errors.As(err, &refused) {
			return "", handoffInputError(refused.Verdict, index, refused.Message)
		}

		return "", err
	}
	defer file.Close()

	// Where the file lives is a deployment question and is answered above
	// whatever the block declares. Everything from here costs I/O, so the two
	// checks that need none run first.
	if !portableImageMIME(image.MimeType) {
		return "", imageInputError(errInvalidMediaType, index, 0, 0)
	}

	gate := EffectiveInputBytesPerImage(limits.MaxBytesPerImage)
	if envelope.sizeBytes > gate {
		return "", imageInputError(errImageTooLarge, index, envelope.sizeBytes, gate)
	}

	// One byte past the gate distinguishes a file that fits from one that does
	// not without holding an unbounded read.
	data, err := io.ReadAll(io.LimitReader(file, gate+1))
	if err != nil {
		return "", handoffInputError(errMissingFile, index, "handoff file cannot be read")
	}

	// The bytes in hand are the only size this verdict may consult, and a read
	// that came back over the bound leaves nothing behind: rejecting here is
	// what keeps unverified bytes out of every gate below.
	if int64(len(data)) > gate {
		return "", imageInputError(errImageTooLarge, index, int64(len(data)), gate)
	}

	if err := verifyHandoffBytes(data, envelope, index); err != nil {
		return "", err
	}

	if err := validateRasterBytes(data, image.MimeType, index, fieldPromptImage, promptBytes, limits); err != nil {
		return "", err
	}

	return base64.StdEncoding.EncodeToString(data), nil
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
		return handoffEnvelope{}, handoffInputError(errInvalidHandoff, index, "handoff digest must be 64 lowercase hexadecimal characters")
	}

	size, ok := handoffSizeBytes(object[handoffFieldSizeBytes])
	if !ok {
		return handoffEnvelope{}, handoffInputError(errInvalidHandoff, index, "handoff sizeBytes must be a non-negative integer")
	}

	return handoffEnvelope{digest: digest, sizeBytes: size}, nil
}

func handoffVersionSupported(value any) bool {
	version, ok := handoffNumber(value)

	return ok && version == HandoffVersion
}

// handoffNumber accepts every shape a JSON decoder may hand back for an
// envelope number, so which decoder options the transport happens to enable
// cannot decide whether a conforming block is accepted.
func handoffNumber(value any) (float64, bool) {
	switch number := value.(type) {
	case float64:
		return number, true
	case json.Number:
		parsed, err := number.Float64()

		return parsed, err == nil
	case int:
		return float64(number), true
	case int64:
		return float64(number), true
	default:
		return 0, false
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

// handoffSizeBytes admits a declared size only after checking it as a float.
// Converting first would hand the range check a value Go leaves undefined: on
// one architecture an out-of-range float truncates to a negative int64 and on
// another it saturates to the largest one.
func handoffSizeBytes(value any) (int64, bool) {
	size, ok := handoffNumber(value)
	if !ok || size != math.Trunc(size) || size < 0 || size >= maxHandoffSizeBytes {
		return 0, false
	}

	return int64(size), true
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
// and digest are rejected outright and never fall back to another form. Both
// branches report one message, because a caller able to tell a size failure
// from a digest failure could sweep sizeBytes and read the exact length of any
// file this read can reach out of which message came back.
func verifyHandoffBytes(data []byte, envelope handoffEnvelope, index int) error {
	if int64(len(data)) != envelope.sizeBytes {
		return handoffInputError(errHandoffDigestMismatch, index, handoffEnvelopeMismatch)
	}

	sum := sha256.Sum256(data)
	if subtle.ConstantTimeCompare([]byte(hex.EncodeToString(sum[:])), []byte(envelope.digest)) != 1 {
		return handoffInputError(errHandoffDigestMismatch, index, handoffEnvelopeMismatch)
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
