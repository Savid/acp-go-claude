package claude

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type modelCatalogTestClock struct {
	nanos atomic.Int64
}

func (c *modelCatalogTestClock) now() time.Time          { return time.Unix(0, c.nanos.Load()) }
func (c *modelCatalogTestClock) advance(d time.Duration) { c.nanos.Add(int64(d)) }

type modelCatalogTestExchange struct {
	request *http.Request
	result  chan modelCatalogHTTPResult
}

type modelCatalogHTTPResult struct {
	status int
	body   string
}

type modelCatalogListResult struct {
	models []APIModel
	err    error
}

func newModelCatalogTestCache(t *testing.T, honorCancellation bool) (*ModelCatalogCache, *modelCatalogTestClock, <-chan modelCatalogTestExchange) {
	t.Helper()
	clock := &modelCatalogTestClock{}
	clock.nanos.Store(time.Date(2026, 9, 9, 0, 0, 0, 0, time.UTC).UnixNano())
	requests := make(chan modelCatalogTestExchange, modelCatalogCacheEntries)
	cache := NewModelCatalogCache()
	cache.now = clock.now
	cache.transport = modelCatalogRoundTripFunc(func(request *http.Request) (*http.Response, error) {
		exchange := modelCatalogTestExchange{request: request, result: make(chan modelCatalogHTTPResult, 1)}
		requests <- exchange
		var result modelCatalogHTTPResult
		if honorCancellation {
			select {
			case result = <-exchange.result:
			case <-request.Context().Done():
				return nil, request.Context().Err()
			}
		} else {
			result = <-exchange.result
		}

		return &http.Response{StatusCode: result.status, Body: io.NopCloser(strings.NewReader(result.body)), Header: make(http.Header)}, nil
	})
	t.Cleanup(cache.Close)

	return cache, clock, requests
}

func modelCatalogListAsync(ctx context.Context, cache *ModelCatalogCache, access ModelCatalogAccess) <-chan modelCatalogListResult {
	result := make(chan modelCatalogListResult, 1)
	go func() {
		models, err := cache.List(ctx, access)
		result <- modelCatalogListResult{models: models, err: err}
	}()

	return result
}

func modelCatalogCurrentFlight(t *testing.T, cache *ModelCatalogCache, access ModelCatalogAccess) *modelCatalogFlight {
	t.Helper()
	cache.mu.Lock()
	defer cache.mu.Unlock()
	entry := cache.entries[modelCatalogKey(access)]
	require.NotNil(t, entry)
	require.NotNil(t, entry.flight)

	return entry.flight
}

func modelCatalogPrime(t *testing.T, cache *ModelCatalogCache, requests <-chan modelCatalogTestExchange, access ModelCatalogAccess, id string) []APIModel {
	t.Helper()
	result := modelCatalogListAsync(t.Context(), cache, access)
	exchange := <-requests
	exchange.result <- modelCatalogHTTPResult{status: http.StatusOK, body: modelCatalogTestPage(id, false)}
	first := <-result
	require.NoError(t, first.err)
	require.Equal(t, id, first.models[0].ID)

	return first.models
}

func TestModelCatalogCacheFreshCloneAndBackgroundRefresh(t *testing.T) {
	t.Parallel()
	cache, clock, requests := newModelCatalogTestCache(t, true)
	access := modelCatalogTestAccess("https://host")
	first := modelCatalogPrime(t, cache, requests, access, "first")
	first[0].ID = "mutated"
	first[0].SupportedEffortLevels[0] = "mutated"
	clock.advance(modelCatalogFreshFor - time.Nanosecond)
	fresh, err := cache.List(t.Context(), access)
	require.NoError(t, err)
	require.Equal(t, "first", fresh[0].ID)
	require.Equal(t, []string{"high"}, fresh[0].SupportedEffortLevels)
	require.Empty(t, requests)

	clock.advance(time.Nanosecond)
	stale, err := cache.List(t.Context(), access)
	require.NoError(t, err)
	require.Equal(t, "first", stale[0].ID)
	exchange := <-requests
	flight := modelCatalogCurrentFlight(t, cache, access)
	stale[0].SupportedEffortLevels[0] = "mutated"
	stillStale, err := cache.List(t.Context(), access)
	require.NoError(t, err)
	require.Equal(t, []string{"high"}, stillStale[0].SupportedEffortLevels)
	require.Empty(t, requests)
	exchange.result <- modelCatalogHTTPResult{status: http.StatusOK, body: modelCatalogTestPage("second", false)}
	<-flight.done
	refreshed, err := cache.List(t.Context(), access)
	require.NoError(t, err)
	require.Equal(t, "second", refreshed[0].ID)
	require.Empty(t, requests)
}

func TestModelCatalogCacheEmptySuccessIsCached(t *testing.T) {
	t.Parallel()
	cache, _, requests := newModelCatalogTestCache(t, true)
	access := modelCatalogTestAccess("https://host")
	result := modelCatalogListAsync(t.Context(), cache, access)
	exchange := <-requests
	exchange.result <- modelCatalogHTTPResult{status: http.StatusOK, body: `{"data":[],"has_more":false}`}
	first := <-result
	require.NoError(t, first.err)
	require.NotNil(t, first.models)
	models, err := cache.List(t.Context(), access)
	require.NoError(t, err)
	require.Empty(t, models)
	require.Empty(t, requests)
}

func TestModelCatalogCacheSeparatesAccountEndpointAndAuthKind(t *testing.T) {
	t.Parallel()
	cache, _, requests := newModelCatalogTestCache(t, true)
	accesses := []ModelCatalogAccess{
		{Endpoint: "https://first/v1/models", Credential: "account-a"},
		{Endpoint: "https://first/v1/models", Credential: "account-b"},
		{Endpoint: "https://second/v1/models", Credential: "account-a"},
		{Endpoint: "https://first/v1/models", Credential: "account-a", OAuth: true},
	}
	for i, access := range accesses {
		modelCatalogPrime(t, cache, requests, access, fmt.Sprint(i))
	}
	for i, access := range accesses {
		models, err := cache.List(t.Context(), access)
		require.NoError(t, err)
		require.Equal(t, fmt.Sprint(i), models[0].ID)
	}
	require.Empty(t, requests)
	cache.mu.Lock()
	require.Len(t, cache.entries, len(accesses))
	cache.mu.Unlock()
}

func TestModelCatalogCacheTransientFailureCooldownAndStaleLimit(t *testing.T) {
	t.Parallel()
	cache, clock, requests := newModelCatalogTestCache(t, true)
	access := modelCatalogTestAccess("https://host")
	modelCatalogPrime(t, cache, requests, access, "first")
	clock.advance(modelCatalogFreshFor)
	stale, err := cache.List(t.Context(), access)
	require.NoError(t, err)
	require.Equal(t, "first", stale[0].ID)
	exchange := <-requests
	flight := modelCatalogCurrentFlight(t, cache, access)
	exchange.result <- modelCatalogHTTPResult{status: http.StatusServiceUnavailable}
	<-flight.done
	for range 3 {
		stale, err = cache.List(t.Context(), access)
		require.NoError(t, err)
		require.Equal(t, "first", stale[0].ID)
	}
	require.Empty(t, requests)
	clock.advance(modelCatalogRetryAfter - time.Nanosecond)
	_, err = cache.List(t.Context(), access)
	require.NoError(t, err)
	require.Empty(t, requests)
	clock.advance(time.Nanosecond)
	_, err = cache.List(t.Context(), access)
	require.NoError(t, err)
	exchange = <-requests
	flight = modelCatalogCurrentFlight(t, cache, access)
	exchange.result <- modelCatalogHTTPResult{status: http.StatusTooManyRequests}
	<-flight.done

	clock.advance(modelCatalogStaleFor - modelCatalogFreshFor - modelCatalogRetryAfter)
	result := modelCatalogListAsync(t.Context(), cache, access)
	exchange = <-requests
	select {
	case <-result:
		t.Fatal("one-hour-old data was returned before the fetch completed")
	default:
	}
	exchange.result <- modelCatalogHTTPResult{status: http.StatusServiceUnavailable}
	failed := <-result
	require.ErrorIs(t, failed.err, errModelCatalogTransient)
	require.Nil(t, failed.models)
	models, err := cache.List(t.Context(), access)
	require.ErrorIs(t, err, errModelCatalogTransient)
	require.Nil(t, models)
	require.Empty(t, requests)
}

func TestModelCatalogCacheAuthAndMalformedRefreshDiscardStale(t *testing.T) {
	t.Parallel()
	for _, response := range []modelCatalogHTTPResult{
		{status: http.StatusUnauthorized}, {status: http.StatusForbidden},
		{status: http.StatusOK, body: `{"data":"malformed","has_more":false}`},
		{status: http.StatusBadRequest},
	} {
		t.Run(fmt.Sprint(response.status), func(t *testing.T) {
			t.Parallel()
			cache, clock, requests := newModelCatalogTestCache(t, true)
			access := modelCatalogTestAccess("https://host")
			modelCatalogPrime(t, cache, requests, access, "first")
			clock.advance(modelCatalogFreshFor)
			_, err := cache.List(t.Context(), access)
			require.NoError(t, err)
			exchange := <-requests
			flight := modelCatalogCurrentFlight(t, cache, access)
			exchange.result <- response
			<-flight.done
			for range 3 {
				models, listErr := cache.List(t.Context(), access)
				require.Error(t, listErr)
				require.Nil(t, models)
				if response.status == http.StatusUnauthorized || response.status == http.StatusForbidden {
					require.ErrorIs(t, listErr, ErrModelCatalogNotAuthenticated)
				}
			}
			require.Empty(t, requests)
			clock.advance(modelCatalogRetryAfter)
			modelCatalogPrime(t, cache, requests, access, "recovered")
		})
	}
}

func TestModelCatalogCacheCoalescesAndWaiterCancellationIsIndependent(t *testing.T) {
	t.Parallel()
	cache, _, requests := newModelCatalogTestCache(t, true)
	access := modelCatalogTestAccess("https://host")
	ctx, cancel := context.WithCancel(t.Context())
	first := modelCatalogListAsync(ctx, cache, access)
	exchange := <-requests
	deadline, ok := exchange.request.Context().Deadline()
	require.True(t, ok)
	require.WithinDuration(t, time.Now().Add(modelCatalogFetchTimeout), deadline, time.Second)
	const waiters = 32
	results := make([]<-chan modelCatalogListResult, 0, waiters)
	for range waiters {
		results = append(results, modelCatalogListAsync(t.Context(), cache, access))
	}
	cancel()
	canceled := <-first
	require.ErrorIs(t, canceled.err, context.Canceled)
	require.Nil(t, canceled.models)
	require.NoError(t, exchange.request.Context().Err())
	exchange.result <- modelCatalogHTTPResult{status: http.StatusOK, body: modelCatalogTestPage("shared", false)}
	for _, result := range results {
		completed := <-result
		require.NoError(t, completed.err)
		require.Equal(t, "shared", completed.models[0].ID)
		completed.models[0].ID = "mutated"
	}
	require.Empty(t, requests)
}

func TestModelCatalogCacheInvalidateFencesOldPublication(t *testing.T) {
	t.Parallel()
	cache, _, requests := newModelCatalogTestCache(t, false)
	access := modelCatalogTestAccess("https://host")
	first := modelCatalogListAsync(t.Context(), cache, access)
	oldExchange := <-requests
	cache.Invalidate()
	require.ErrorIs(t, oldExchange.request.Context().Err(), context.Canceled)
	second := modelCatalogListAsync(t.Context(), cache, access)
	newExchange := <-requests
	oldExchange.result <- modelCatalogHTTPResult{status: http.StatusOK, body: modelCatalogTestPage("retired", false)}
	old := <-first
	require.ErrorIs(t, old.err, errModelCatalogInvalidated)
	require.Nil(t, old.models)
	newExchange.result <- modelCatalogHTTPResult{status: http.StatusOK, body: modelCatalogTestPage("current", false)}
	current := <-second
	require.NoError(t, current.err)
	require.Equal(t, "current", current.models[0].ID)
	models, err := cache.List(t.Context(), access)
	require.NoError(t, err)
	require.Equal(t, "current", models[0].ID)
}

func TestModelCatalogCacheCloseCancelsAndJoinsWorkers(t *testing.T) {
	t.Parallel()
	cache, _, requests := newModelCatalogTestCache(t, false)
	access := modelCatalogTestAccess("https://host")
	result := modelCatalogListAsync(t.Context(), cache, access)
	exchange := <-requests
	closed := make(chan struct{})
	go func() {
		cache.Close()
		close(closed)
	}()
	<-exchange.request.Context().Done()
	select {
	case <-closed:
		t.Fatal("Close returned before its worker exited")
	default:
	}
	models, err := cache.List(t.Context(), access)
	require.ErrorIs(t, err, errModelCatalogClosed)
	require.Nil(t, models)
	exchange.result <- modelCatalogHTTPResult{status: http.StatusOK, body: modelCatalogTestPage("retired", false)}
	<-closed
	completed := <-result
	require.ErrorIs(t, completed.err, errModelCatalogClosed)
	require.Nil(t, completed.models)
	cache.Close()
	cache.Invalidate()
	require.Empty(t, requests)
}

func TestModelCatalogCacheEvictsLeastRecentlyUsedIdleEntry(t *testing.T) {
	t.Parallel()
	cache, _, requests := newModelCatalogTestCache(t, true)
	accesses := make([]ModelCatalogAccess, modelCatalogCacheEntries+1)
	for i := range accesses {
		accesses[i] = ModelCatalogAccess{Endpoint: "https://host/v1/models", Credential: fmt.Sprintf("account-%d", i)}
	}
	for i, access := range accesses[:modelCatalogCacheEntries] {
		modelCatalogPrime(t, cache, requests, access, fmt.Sprint(i))
	}
	_, err := cache.List(t.Context(), accesses[0])
	require.NoError(t, err)
	modelCatalogPrime(t, cache, requests, accesses[modelCatalogCacheEntries], "new")
	cache.mu.Lock()
	require.Len(t, cache.entries, modelCatalogCacheEntries)
	require.Contains(t, cache.entries, modelCatalogKey(accesses[0]))
	require.NotContains(t, cache.entries, modelCatalogKey(accesses[1]))
	cache.mu.Unlock()
	modelCatalogPrime(t, cache, requests, accesses[1], "reloaded")
}

func TestModelCatalogCacheBoundsFlightsAcrossInvalidation(t *testing.T) {
	t.Parallel()
	cache, _, requests := newModelCatalogTestCache(t, false)
	access := modelCatalogTestAccess("https://host")
	exchanges := make([]modelCatalogTestExchange, 0, modelCatalogCacheEntries)
	results := make([]<-chan modelCatalogListResult, 0, modelCatalogCacheEntries)
	for range modelCatalogCacheEntries {
		results = append(results, modelCatalogListAsync(t.Context(), cache, access))
		exchanges = append(exchanges, <-requests)
		cache.Invalidate()
	}
	models, err := cache.List(t.Context(), access)
	require.ErrorIs(t, err, errModelCatalogUnavailable)
	require.Nil(t, models)
	require.Empty(t, requests)
	for i, exchange := range exchanges {
		exchange.result <- modelCatalogHTTPResult{status: http.StatusOK, body: modelCatalogTestPage("retired", false)}
		completed := <-results[i]
		require.ErrorIs(t, completed.err, errModelCatalogInvalidated)
		require.Nil(t, completed.models)
	}
	modelCatalogPrime(t, cache, requests, access, "current")
}

func TestModelCatalogCacheAllBusyEntriesRefuseExcessIdentity(t *testing.T) {
	t.Parallel()
	cache, _, requests := newModelCatalogTestCache(t, true)
	results := make([]<-chan modelCatalogListResult, 0, modelCatalogCacheEntries)
	for i := range modelCatalogCacheEntries {
		access := ModelCatalogAccess{Endpoint: "https://host/v1/models", Credential: fmt.Sprintf("account-%d", i)}
		results = append(results, modelCatalogListAsync(t.Context(), cache, access))
		<-requests
	}
	models, err := cache.List(t.Context(), modelCatalogTestAccess("https://host"))
	require.ErrorIs(t, err, errModelCatalogUnavailable)
	require.Nil(t, models)
	require.Empty(t, requests)
	cache.Close()
	for _, result := range results {
		require.ErrorIs(t, (<-result).err, errModelCatalogClosed)
	}
}

func TestModelCatalogCacheConcurrentCloseAndInvalidate(t *testing.T) {
	t.Parallel()
	cache, _, requests := newModelCatalogTestCache(t, true)
	result := modelCatalogListAsync(t.Context(), cache, modelCatalogTestAccess("https://host"))
	<-requests
	var workers sync.WaitGroup
	for range 16 {
		workers.Go(cache.Close)
		workers.Go(cache.Invalidate)
	}
	workers.Wait()
	require.Error(t, (<-result).err)
	cache.mu.Lock()
	require.Empty(t, cache.entries)
	require.Empty(t, cache.flights)
	cache.mu.Unlock()
}
