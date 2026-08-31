package claude

import (
	"os"
	"testing"

	"github.com/stretchr/testify/require"
)

func writeShellScript(t *testing.T, path string, contents string) string {
	t.Helper()
	require.NoError(t, os.WriteFile(path, []byte(contents), 0o700))

	return path
}

func splitEventsForTest(events <-chan TransportEvent) (<-chan map[string]any, <-chan error) {
	messages := make(chan map[string]any, 1024)
	errs := make(chan error, 16)
	go func() {
		defer close(messages)
		defer close(errs)
		for event := range events {
			if event.Err != nil {
				errs <- event.Err

				continue
			}
			messages <- event.Message
		}
	}()

	return messages, errs
}
