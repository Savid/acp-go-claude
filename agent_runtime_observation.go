package claudeacp

import (
	"context"
	"sync"
	"time"

	"github.com/savid/acp-go-claude/internal/observer"
)

func instrumentRuntimeResourceHooks(hooks RuntimeResourceHooks, observe *observer.Observer) RuntimeResourceHooks {
	wrapAcquire := func(resource string, acquire func(context.Context, RuntimeResourceKind) (func(), error)) func(context.Context, RuntimeResourceKind) (func(), error) {
		return func(ctx context.Context, lifecycle RuntimeResourceKind) (func(), error) {
			if acquire == nil {
				return func() {}, nil
			}

			release, err := acquire(ctx, lifecycle)
			if err != nil || release == nil {
				observe.RecordRuntimeResourceAdmission(ctx, resource, string(lifecycle), "rejected")

				return release, err
			}

			observe.RecordRuntimeResourceAdmission(ctx, resource, string(lifecycle), "admitted")
			observe.AddRuntimeResource(ctx, resource, 1)

			var once sync.Once

			return func() { once.Do(func() { release(); observe.AddRuntimeResource(context.Background(), resource, -1) }) }, nil
		}
	}
	hooks.ReserveScratchRoot = wrapAcquire("adapter_scratch_root", hooks.ReserveScratchRoot)
	externalStage := hooks.ObserveStartupStage
	hooks.ObserveStartupStage = func(ctx context.Context, lifecycle RuntimeResourceKind, stage RuntimeStartupStage, elapsed time.Duration, err error) {
		observe.ObserveRuntimeStartupStage(ctx, string(lifecycle), string(stage), elapsed, err)

		if externalStage != nil {
			externalStage(ctx, lifecycle, stage, elapsed, err)
		}
	}

	return hooks
}

func observeRuntimeStartupStage(ctx context.Context, hooks RuntimeResourceHooks, kind RuntimeResourceKind, stage RuntimeStartupStage, started time.Time, err error) {
	if hooks.ObserveStartupStage != nil {
		hooks.ObserveStartupStage(ctx, kind, stage, time.Since(started), err)
	}
}
