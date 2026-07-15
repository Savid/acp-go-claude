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
			var (
				release func()
				err     error
			)

			if acquire == nil {
				release = func() {}
			} else {
				release, err = acquire(ctx, lifecycle)
			}

			if err != nil || release == nil {
				observe.RecordRuntimeResourceAdmission(ctx, resource, string(lifecycle), "rejected")

				return release, err
			}

			observe.RecordRuntimeResourceAdmission(ctx, resource, string(lifecycle), "admitted")
			observe.AddRuntimeResource(ctx, resource, 1)

			var once sync.Once

			return func() {
				once.Do(func() {
					release()
					observe.AddRuntimeResource(context.Background(), resource, -1)
				})
			}, nil
		}
	}
	hooks.AcquireNativeRoot = wrapAcquire("managed_native_root", hooks.AcquireNativeRoot)
	hooks.ReserveScratchRoot = wrapAcquire("adapter_scratch_root", hooks.ReserveScratchRoot)

	externalProcess := hooks.ObserveProcess
	hooks.ObserveProcess = func(ctx context.Context, kind RuntimeProcessKind, delta int64) {
		observe.AddRuntimeProcess(ctx, string(kind), delta)

		if externalProcess != nil {
			externalProcess(ctx, kind, delta)
		}
	}
	externalSnapshot := hooks.ObserveProcessSnapshot
	hooks.ObserveProcessSnapshot = func(ctx context.Context, kind RuntimeProcessKind, count int) {
		observe.SetRuntimeProcess(ctx, string(kind), count)

		if externalSnapshot != nil {
			externalSnapshot(ctx, kind, count)
		}
	}
	externalStage := hooks.ObserveStartupStage
	hooks.ObserveStartupStage = func(ctx context.Context, lifecycle RuntimeResourceKind, stage RuntimeStartupStage, elapsed time.Duration, err error) {
		observe.ObserveRuntimeStartupStage(ctx, string(lifecycle), string(stage), elapsed, err)

		if externalStage != nil {
			externalStage(ctx, lifecycle, stage, elapsed, err)
		}
	}

	return hooks
}

func observeRuntimeProcess(ctx context.Context, hooks RuntimeResourceHooks, kind RuntimeProcessKind, delta int64) {
	if hooks.ObserveProcess != nil && delta != 0 {
		hooks.ObserveProcess(ctx, kind, delta)
	}
}

func observeRuntimeProcessSnapshot(ctx context.Context, hooks RuntimeResourceHooks, kind RuntimeProcessKind, count int) {
	if hooks.ObserveProcessSnapshot != nil && count >= 0 {
		hooks.ObserveProcessSnapshot(ctx, kind, count)
	}
}

type runtimeProcessSnapshotTracker struct {
	mu         sync.Mutex
	hooks      RuntimeResourceHooks
	sources    map[*runtimeProcessSnapshotSource]func() (int, bool)
	publishing bool
	dirty      bool
	emit       func(RuntimeResourceHooks, int)
	lastCount  int
	lastValid  bool
}

type runtimeProcessSnapshotSource struct {
	tracker *runtimeProcessSnapshotTracker
}

func newRuntimeProcessSnapshotTracker(hooks RuntimeResourceHooks) *runtimeProcessSnapshotTracker {
	return &runtimeProcessSnapshotTracker{
		hooks:   hooks,
		sources: make(map[*runtimeProcessSnapshotSource]func() (int, bool)),
	}
}

func (t *runtimeProcessSnapshotTracker) newSource() *runtimeProcessSnapshotSource {
	return &runtimeProcessSnapshotSource{tracker: t}
}

func (s *runtimeProcessSnapshotSource) started(ctx context.Context, inventory func() (int, bool)) {
	s.tracker.mu.Lock()
	s.tracker.sources[s] = inventory
	startPublishing := s.tracker.markDirtyLocked(ctx)
	s.tracker.mu.Unlock()

	if startPublishing {
		s.tracker.publish()
	}
}

func (s *runtimeProcessSnapshotSource) quiesced(ctx context.Context) {
	s.tracker.mu.Lock()
	delete(s.tracker.sources, s)
	startPublishing := s.tracker.markDirtyLocked(ctx)
	s.tracker.mu.Unlock()

	if startPublishing {
		s.tracker.publish()
	}
}

func (t *runtimeProcessSnapshotTracker) markDirtyLocked(ctx context.Context) bool {
	t.dirty = true
	t.emit = func(hooks RuntimeResourceHooks, count int) {
		observeRuntimeProcessSnapshot(ctx, hooks, RuntimeProcessProviderDescendant, count)
	}

	if t.publishing {
		return false
	}

	t.publishing = true

	return true
}

func (t *runtimeProcessSnapshotTracker) publish() {
	for {
		t.mu.Lock()
		if !t.dirty {
			t.publishing = false
			t.mu.Unlock()

			return
		}

		t.dirty = false
		total := 0
		available := true

		for _, inventory := range t.sources {
			count, exact := inventory()
			if !exact {
				available = false

				break
			}

			total += count
		}

		hooks := t.hooks
		emit := t.emit
		observe := available && (!t.lastValid || total != t.lastCount)
		t.lastValid = available

		if available {
			t.lastCount = total
		}
		t.mu.Unlock()

		if observe {
			emit(hooks, total)
		}
	}
}

func observeRuntimeStartupStage(
	ctx context.Context,
	hooks RuntimeResourceHooks,
	lifecycle RuntimeResourceKind,
	stage RuntimeStartupStage,
	started time.Time,
	err error,
) {
	if hooks.ObserveStartupStage != nil {
		hooks.ObserveStartupStage(ctx, lifecycle, stage, time.Since(started), err)
	}
}
