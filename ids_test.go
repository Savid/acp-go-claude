package claudeacp

import (
	"errors"
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestNewUUID(t *testing.T) {
	t.Parallel()

	id, err := newUUID()
	require.NoError(t, err)
	require.Regexp(t, regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`), id)
}

func TestNewUUIDError(t *testing.T) {
	random := uuidRandom
	uuidRandom = errReader{err: errors.New("random failed")}
	t.Cleanup(func() {
		uuidRandom = random
	})

	_, err := newUUID()
	require.Error(t, err)
}

type errReader struct {
	err error
}

func (r errReader) Read([]byte) (int, error) {
	return 0, r.err
}
