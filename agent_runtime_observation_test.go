package claudeacp

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/savid/acp-go-claude/internal/observer"
	"github.com/stretchr/testify/require"
)

func TestRuntimeObservationHooksComposeExactLifetimes(t *testing.T) {
	var releases int
	var snapshot int
	var stage RuntimeStartupStage
	var containment RuntimeContainmentMode
	hooks := instrumentRuntimeResourceHooks(RuntimeResourceHooks{
		AcquireNativeRoot: func(context.Context, RuntimeResourceKind) (func(), error) {
			return func() { releases++ }, nil
		},
		ObserveProcessSnapshot: func(_ context.Context, _ RuntimeProcessKind, count int) {
			snapshot = count
		},
		ObserveStartupStage: func(_ context.Context, _ RuntimeResourceKind, got RuntimeStartupStage, _ time.Duration, _ error) {
			stage = got
		},
		ObserveContainment: func(_ context.Context, mode RuntimeContainmentMode) { containment = mode },
	}, observer.New(observer.Config{}))

	release, err := hooks.AcquireNativeRoot(t.Context(), RuntimeResourceSession)
	require.NoError(t, err)
	release()
	release()
	require.Equal(t, 1, releases)

	observeRuntimeProcessSnapshot(t.Context(), hooks, RuntimeProcessProviderDescendant, 3)
	observeRuntimeStartupStage(t.Context(), hooks, RuntimeResourceRuntime, RuntimeStartupReadiness, time.Now(), nil)
	hooks.ObserveContainment(t.Context(), RuntimeContainmentBestEffort)
	require.Equal(t, 3, snapshot)
	require.Equal(t, RuntimeStartupReadiness, stage)
	require.Equal(t, RuntimeContainmentBestEffort, containment)

	wantErr := errors.New("full")
	rejected := instrumentRuntimeResourceHooks(RuntimeResourceHooks{
		ReserveScratchRoot: func(context.Context, RuntimeResourceKind) (func(), error) {
			return nil, wantErr
		},
	}, observer.New(observer.Config{}))
	_, err = rejected.ReserveScratchRoot(t.Context(), RuntimeResourcePrompt)
	require.ErrorIs(t, err, wantErr)
}

func TestRuntimeProcessSnapshotTrackerSuppressesBestEffortInventories(t *testing.T) {
	var snapshots []int
	tracker := newRuntimeProcessSnapshotTracker(RuntimeResourceHooks{
		ObserveProcessSnapshot: func(_ context.Context, _ RuntimeProcessKind, count int) {
			snapshots = append(snapshots, count)
		},
	}, false)
	source := tracker.newSource()
	source.started(t.Context(), func() (int, bool) { return 7, true })
	source.completed(t.Context())
	require.Empty(t, snapshots, "best-effort containment must not publish descendant totals, including zero")
}

func TestRuntimeProcessSnapshotTrackerAggregatesOnlyCompleteInventories(t *testing.T) {
	var snapshots []int
	tracker := newRuntimeProcessSnapshotTracker(RuntimeResourceHooks{
		ObserveProcessSnapshot: func(_ context.Context, kind RuntimeProcessKind, count int) {
			require.Equal(t, RuntimeProcessProviderDescendant, kind)
			snapshots = append(snapshots, count)
		},
	})
	first := tracker.newSource()
	second := tracker.newSource()
	unknown := tracker.newSource()

	firstCount := 1
	first.started(t.Context(), func() (int, bool) { return firstCount, true })
	firstCount = 4
	second.started(t.Context(), func() (int, bool) { return 2, true })
	unknown.started(t.Context(), func() (int, bool) { return 0, false })
	firstCount = 5
	first.started(t.Context(), func() (int, bool) { return firstCount, true })
	require.Equal(t, []int{1, 6}, snapshots, "every boundary must re-query all active inventories")

	unknown.completed(t.Context())
	second.completed(t.Context())
	first.completed(t.Context())
	require.Equal(t, []int{1, 6, 7, 5, 0}, snapshots)

	unproven := tracker.newSource()
	unproven.started(t.Context(), func() (int, bool) { return 3, true })
	unknown.started(t.Context(), func() (int, bool) { return 0, false })
	unproven.completed(t.Context())
	require.Equal(t, []int{1, 6, 7, 5, 0, 3}, snapshots, "an unproven tree must retain unknown inventory and prevent zero")
}

func TestRuntimeProcessSnapshotTrackerSerializesConcurrentLifecycles(t *testing.T) {
	var snapshots []int
	tracker := newRuntimeProcessSnapshotTracker(RuntimeResourceHooks{
		ObserveProcessSnapshot: func(_ context.Context, _ RuntimeProcessKind, count int) {
			snapshots = append(snapshots, count)
		},
	})

	var group sync.WaitGroup
	for range 64 {
		source := tracker.newSource()
		group.Go(func() {
			source.started(t.Context(), func() (int, bool) { return 1, true })
			source.completed(t.Context())
		})
	}
	group.Wait()

	require.NotEmpty(t, snapshots)
	require.Equal(t, 0, snapshots[len(snapshots)-1])
}

func TestRuntimeProcessSnapshotTrackerAllowsReentrantQuiescence(t *testing.T) {
	var (
		snapshots []int
		source    *runtimeProcessSnapshotSource
	)
	tracker := newRuntimeProcessSnapshotTracker(RuntimeResourceHooks{
		ObserveProcessSnapshot: func(ctx context.Context, _ RuntimeProcessKind, count int) {
			snapshots = append(snapshots, count)
			if count == 1 {
				source.completed(ctx)
			}
		},
	})
	source = tracker.newSource()

	source.started(t.Context(), func() (int, bool) { return 1, true })

	require.Equal(t, []int{1, 0}, snapshots)
}
