package claude

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// ordinaryTransportLines is large enough that a truncated read loses obviously
// more than the final line, and small enough to fit a pipe buffer so the
// fixture writes everything and exits without waiting for a reader.
const ordinaryTransportLines = 500

// TestProcessTransportOrdinaryBoundaryDeliversEveryLine drives a real
// ProcessTransport through the real ordinary boundary — no stubbed containment
// and no explicit policy — against a CLI that writes its whole stream-json
// transcript and exits immediately.
//
// The assertions pin that process collection cannot close the adapter-owned
// read ends before every buffered frame has been delivered.
func TestProcessTransportOrdinaryBoundaryDeliversEveryLine(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses /bin/sh")
	}
	originalProbe := claudeVersionProbe
	claudeVersionProbe = func(context.Context, Options) error { return nil }
	t.Cleanup(func() { claudeVersionProbe = originalProbe })

	dir := t.TempDir()
	wrote := filepath.Join(dir, "wrote")
	script := writeShellScript(t, filepath.Join(dir, "claude"), fmt.Sprintf(`#!/bin/sh
i=0
while [ "$i" -lt %d ]; do
  printf '{"type":"stream_event","seq":%%s}\n' "$i"
  i=$((i + 1))
done
printf '{"type":"result","subtype":"success"}\n'
printf wrote > "$WROTE_MARK"
exit 0
`, ordinaryTransportLines))

	transport := NewProcessTransport(nil, Options{
		CLIPath:             script,
		Cwd:                 dir,
		OrdinaryEnvironment: OrdinaryEnvironment(),
		Env:                 map[string]string{"WROTE_MARK": wrote},
	})

	require.NoError(t, transport.Start(context.Background()))

	// The child has published its whole transcript and is on its way out before
	// the reader starts, so the parent's pipe still has to hold everything.
	require.Eventually(t, func() bool {
		_, err := os.Stat(wrote)

		return err == nil
	}, 10*time.Second, 5*time.Millisecond)

	messages, errs := splitEventsForTest(transport.Events(context.Background()))

	seen := make([]map[string]any, 0, ordinaryTransportLines+1)
	for message := range messages {
		seen = append(seen, message)
	}

	var transportErrs []error
	for err := range errs {
		transportErrs = append(transportErrs, err)
	}

	require.Empty(t, transportErrs, "a completed turn must not be reported as a transport failure")
	require.Len(t, seen, ordinaryTransportLines+1)

	for index := range ordinaryTransportLines {
		require.Equal(t, "stream_event", seen[index]["type"])
		require.Equal(t, strconv.Itoa(index), fmt.Sprint(seen[index]["seq"]))
	}

	final := seen[len(seen)-1]
	require.Equal(t, "result", final["type"], "the terminating result line must survive the child's exit")
	require.Equal(t, "success", final["subtype"])

	require.NoError(t, transport.Close())
}

// TestProcessTransportOrdinaryBoundaryStillStopsANonExitingChild proves
// deferring the reap did not cost the boundary its teeth: a child that ignores
// stdin EOF and SIGTERM is still killed and collected by Close.
func TestProcessTransportOrdinaryBoundaryStillStopsANonExitingChild(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("test uses /bin/sh")
	}
	originalProbe := claudeVersionProbe
	claudeVersionProbe = func(context.Context, Options) error { return nil }
	t.Cleanup(func() { claudeVersionProbe = originalProbe })

	originalGrace, originalWaitDelay := processExitGracePeriod, processShutdownWaitDelay
	processExitGracePeriod = 20 * time.Millisecond
	processShutdownWaitDelay = 500 * time.Millisecond
	t.Cleanup(func() {
		processExitGracePeriod, processShutdownWaitDelay = originalGrace, originalWaitDelay
	})

	dir := t.TempDir()
	ready := filepath.Join(dir, "ready")
	script := writeShellScript(t, filepath.Join(dir, "claude"), `#!/bin/sh
trap '' TERM
printf ready > "$READY_MARK"
while :; do sleep 1; done
`)

	transport := NewProcessTransport(nil, Options{
		CLIPath:             script,
		Cwd:                 dir,
		OrdinaryEnvironment: OrdinaryEnvironment(),
		Env:                 map[string]string{"READY_MARK": ready},
	})

	require.NoError(t, transport.Start(context.Background()))
	require.Eventually(t, func() bool {
		_, err := os.Stat(ready)

		return err == nil
	}, 10*time.Second, 5*time.Millisecond)

	require.NoError(t, transport.Close())
}
