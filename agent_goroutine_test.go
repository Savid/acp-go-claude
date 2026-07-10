package claudeacp

import (
	"context"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestRecoverAgentGoroutine(t *testing.T) {
	t.Parallel()

	logger := slog.New(slog.DiscardHandler)

	func() {
		defer recoverAgentGoroutine(context.Background(), logger, "test")

		panic("boom")
	}()
}

func TestRecoverAgentGoroutineUsesDefaultLogger(t *testing.T) {
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.DiscardHandler))
	t.Cleanup(func() { slog.SetDefault(oldLogger) })

	var recovered any

	handleAgentGoroutinePanic(context.Background(), nil, "test", func(value any) {
		recovered = value
	}, "boom")

	require.Equal(t, "boom", recovered)
}

func TestRecoverAgentGoroutineNoPanic(t *testing.T) {
	t.Parallel()

	called := false

	handleAgentGoroutinePanic(context.Background(), nil, "test", func(any) {
		called = true
	}, nil)

	require.False(t, called)
}
