package claude

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestHandleClaudeGoroutinePanic(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.DiscardHandler)
	var recovered any

	handleClaudeGoroutinePanic(context.Background(), logger, "test", func(value any) {
		recovered = value
	}, "boom")

	require.Equal(t, "boom", recovered)
}

func TestHandleClaudeGoroutinePanicWithoutShutdown(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.DiscardHandler)

	handleClaudeGoroutinePanic(context.Background(), logger, "test", nil, "boom")
}

func TestHandleClaudeGoroutinePanicUsesDefaultLogger(t *testing.T) {
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.DiscardHandler))
	t.Cleanup(func() { slog.SetDefault(oldLogger) })

	var recovered any

	handleClaudeGoroutinePanic(context.Background(), nil, "test", func(value any) {
		recovered = value
	}, "boom")

	require.Equal(t, "boom", recovered)
}

func TestHandleClaudeGoroutinePanicNoPanic(t *testing.T) {
	t.Parallel()

	called := false

	handleClaudeGoroutinePanic(context.Background(), nil, "test", func(any) {
		called = true
	}, nil)

	require.False(t, called)
}
