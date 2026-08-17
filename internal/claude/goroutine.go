package claude

import (
	"context"
	"log/slog"
)

func handleClaudeGoroutinePanic(
	ctx context.Context,
	log *slog.Logger,
	name string,
	shutdown func(any),
	recovered any,
) {
	if recovered == nil {
		return
	}

	if log == nil {
		log = slog.Default()
	}

	log.ErrorContext(ctx, "claude goroutine panic", slog.String("goroutine", name), slog.Any("panic", recovered))

	if shutdown != nil {
		shutdown(recovered)
	}
}
