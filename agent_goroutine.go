package claudeacp

import (
	"context"
	"log/slog"
)

func recoverAgentGoroutine(ctx context.Context, log *slog.Logger, name string) {
	handleAgentGoroutinePanic(ctx, log, name, nil, recover())
}

func handleAgentGoroutinePanic(
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

	log.ErrorContext(ctx, "agent goroutine panic", slog.String("goroutine", name), slog.Any("panic", recovered))

	if shutdown != nil {
		shutdown(recovered)
	}
}
