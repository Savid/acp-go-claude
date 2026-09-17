package claude

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/stretchr/testify/require"
)

func TestQuotaCacheIndependentCadenceAndAccounts(t *testing.T) {
	c := NewQuotaCache()
	now := time.Date(2026, 9, 18, 1, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	var calls [2]int
	probe := func(_ context.Context, fable bool) (QuotaResult, error) {
		id := QuotaSession
		lane := 0
		if fable {
			id = QuotaFable
			lane = 1
		}
		calls[lane]++
		w, _ := quotaWindow(id, .99, now.Add(5*time.Hour).Unix(), now)

		return QuotaResult{Windows: []QuotaWindow{w}}, nil
	}
	first, err := c.Read(t.Context(), [32]byte{1}, probe)
	require.NoError(t, err)
	for range 4 {
		now = now.Add(time.Minute)
		got, readErr := c.Read(t.Context(), [32]byte{1}, probe)
		require.NoError(t, readErr)
		require.Equal(t, first, got)
	}
	require.Equal(t, [2]int{1, 1}, calls)
	now = now.Add(time.Minute)
	_, err = c.Read(t.Context(), [32]byte{1}, probe)
	require.NoError(t, err)
	require.Equal(t, [2]int{2, 1}, calls)
	now = now.Add(25 * time.Minute)
	_, err = c.Read(t.Context(), [32]byte{1}, probe)
	require.NoError(t, err)
	require.Equal(t, [2]int{3, 2}, calls)
	_, err = c.Read(t.Context(), [32]byte{2}, probe)
	require.NoError(t, err)
	require.Equal(t, [2]int{4, 3}, calls)
}

func TestQuotaCacheScopedInvalidationAndBackoff(t *testing.T) {
	c := NewQuotaCache()
	now := time.Date(2026, 9, 18, 1, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	key := [32]byte{1}
	var calls [2]int
	fail := false
	probe := func(_ context.Context, fable bool) (QuotaResult, error) {
		id, lane := QuotaSession, 0
		if fable {
			id, lane = QuotaFable, 1
		}
		calls[lane]++
		if fail {
			return QuotaResult{}, errors.New("timeout")
		}
		w, _ := quotaWindow(id, .99, now.Add(5*time.Hour).Unix(), now)

		return QuotaResult{Windows: []QuotaWindow{w}}, nil
	}
	first, err := c.Read(t.Context(), key, probe)
	require.NoError(t, err)
	now = now.Add(time.Minute)
	c.Observe(key, nil, QuotaFable)
	fail = true
	got, err := c.Read(t.Context(), key, probe)
	require.NoError(t, err)
	require.Equal(t, [2]int{1, 2}, calls)
	require.Equal(t, first.Windows[0], got.Windows[0])
	require.Equal(t, 99.0, got.Windows[1].Percent)
	require.Equal(t, now, got.Windows[1].StaleAt)
	for range 20 {
		c.Observe(key, nil, QuotaFable)
		_, err = c.Read(t.Context(), key, probe)
		require.NoError(t, err)
	}
	require.Equal(t, [2]int{1, 2}, calls)
	now = now.Add(5 * time.Minute)
	_, err = c.Read(t.Context(), key, probe)
	require.NoError(t, err)
	require.Equal(t, [2]int{2, 3}, calls)
	require.Equal(t, now.Add(15*time.Minute), c.entries[key].lanes[1].next)
}

func TestQuotaCacheSharedExhaustionSuppressesProbesUntilReset(t *testing.T) {
	c := NewQuotaCache()
	now := time.Date(2026, 9, 18, 1, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	key := [32]byte{1}
	fable, _ := quotaWindow(QuotaFable, .99, now.Add(7*24*time.Hour).Unix(), now)
	c.Observe(key, []QuotaWindow{fable}, "")
	reset := now.Add(2 * time.Minute)
	exhausted, _ := quotaWindow(QuotaSession, 1, reset.Unix(), now)
	c.Observe(key, []QuotaWindow{exhausted}, QuotaSession)
	probe := func(_ context.Context, fable bool) (QuotaResult, error) {
		require.False(t, fable)
		w, _ := quotaWindow(QuotaSession, .01, now.Add(5*time.Hour).Unix(), now)

		return QuotaResult{Windows: []QuotaWindow{w}}, nil
	}
	got, err := c.Read(t.Context(), key, func(context.Context, bool) (QuotaResult, error) {
		t.Fatal("exhausted window must suppress probes")

		return QuotaResult{}, nil
	})
	require.NoError(t, err)
	require.Equal(t, fable, got.Windows[1])
	now = reset
	got, err = c.Read(t.Context(), key, probe)
	require.NoError(t, err)
	require.Equal(t, 1.0, got.Windows[0].Percent)
	require.Equal(t, fable, got.Windows[1])
}

func TestQuotaCacheCoalescesAndTurnObservationWinsRace(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := NewQuotaCache()
		key := [32]byte{1}
		started, release := make(chan struct{}), make(chan struct{})
		var calls atomic.Int32
		probe := func(_ context.Context, fable bool) (QuotaResult, error) {
			calls.Add(1)
			if fable {
				return QuotaResult{Unavailable: true}, nil
			}
			close(started)
			<-release
			w, _ := quotaWindow(QuotaSession, .99, time.Now().Add(time.Hour).Unix(), time.Now())

			return QuotaResult{Windows: []QuotaWindow{w}}, nil
		}
		results := make(chan QuotaResult, 2)
		go func() { r, err := c.Read(t.Context(), key, probe); require.NoError(t, err); results <- r }()
		<-started
		go func() { r, err := c.Read(t.Context(), key, probe); require.NoError(t, err); results <- r }()
		synctest.Wait()
		w, _ := quotaWindow(QuotaSession, 1, time.Now().Add(time.Hour).Unix(), time.Now())
		c.Observe(key, []QuotaWindow{w}, QuotaSession)
		close(release)
		for range 2 {
			require.Equal(t, []QuotaWindow{w}, (<-results).Windows)
		}
		require.Equal(t, int32(1), calls.Load())
	})
}

func TestQuotaCacheNegativeResultAndCancellation(t *testing.T) {
	c := NewQuotaCache()
	now := time.Now()
	c.now = func() time.Time { return now }
	key := [32]byte{1}
	var calls int
	probe := func(context.Context, bool) (QuotaResult, error) {
		calls++

		return QuotaResult{Unavailable: true}, nil
	}
	_, err := c.Read(t.Context(), key, probe)
	require.NoError(t, err)
	now = now.Add(5 * time.Hour)
	_, err = c.Read(t.Context(), key, probe)
	require.NoError(t, err)
	require.Equal(t, 2, calls)
	now = now.Add(time.Hour)
	_, err = c.Read(t.Context(), key, probe)
	require.NoError(t, err)
	require.Equal(t, 4, calls)
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = c.Read(ctx, [32]byte{2}, func(ctx context.Context, _ bool) (QuotaResult, error) { return QuotaResult{}, ctx.Err() })
	require.ErrorIs(t, err, context.Canceled)
	require.NotContains(t, c.entries, [32]byte{2})
}

func TestQuotaCacheRepeatedRefusalsDoNotRepeatSuccessfulProbe(t *testing.T) {
	c := NewQuotaCache()
	now := time.Date(2026, 9, 18, 1, 0, 0, 0, time.UTC)
	c.now = func() time.Time { return now }
	key := [32]byte{1}
	calls := 0
	probe := func(_ context.Context, fable bool) (QuotaResult, error) {
		calls++
		id := QuotaSession
		if fable {
			id = QuotaFable
		}
		w, _ := quotaWindow(id, .99, now.Add(time.Hour).Unix(), now)

		return QuotaResult{Windows: []QuotaWindow{w}}, nil
	}
	_, err := c.Read(t.Context(), key, probe)
	require.NoError(t, err)
	now = now.Add(time.Minute)
	c.Observe(key, nil, QuotaFable)
	_, err = c.Read(t.Context(), key, probe)
	require.NoError(t, err)
	require.Equal(t, 3, calls)
	for range 10 {
		c.Observe(key, nil, QuotaFable)
		_, err = c.Read(t.Context(), key, probe)
		require.NoError(t, err)
	}
	require.Equal(t, 3, calls)
}

func TestQuotaCacheKeepsFailureWhenOtherModelIsUnavailable(t *testing.T) {
	c := NewQuotaCache()
	failure := errors.New("haiku timeout")
	_, err := c.Read(t.Context(), [32]byte{1}, func(_ context.Context, fable bool) (QuotaResult, error) {
		if fable {
			return QuotaResult{Unavailable: true}, nil
		}

		return QuotaResult{}, failure
	})
	require.ErrorIs(t, err, failure)
}
