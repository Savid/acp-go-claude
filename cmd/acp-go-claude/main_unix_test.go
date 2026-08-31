//go:build unix

package main

import (
	"bytes"
	"context"
	"io"
	"os"
	"syscall"
	"testing"

	claudeacp "github.com/savid/acp-go-claude"
	"github.com/stretchr/testify/require"
)

func TestRunReturnsSignalCode(t *testing.T) {
	originalServe := serve
	originalShutdown := shutdownOpenTelemetry
	t.Cleanup(func() { serve = originalServe; shutdownOpenTelemetry = originalShutdown })
	serve = func(ctx context.Context, _ io.Reader, _ io.Writer, _ ...claudeacp.Option) error {
		require.NoError(t, syscall.Kill(os.Getpid(), syscall.SIGTERM))
		<-ctx.Done()

		return ctx.Err()
	}
	shutdownOpenTelemetry = func(context.Context, func(context.Context) error) error { return nil }
	code := run(context.Background(), nil, bytes.NewReader(nil), io.Discard, io.Discard)
	require.Equal(t, 128+int(syscall.SIGTERM), code)
}
