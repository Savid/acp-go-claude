package claude

import (
	"context"
	"crypto/sha256"
	"errors"
	"net/http"
	"slices"
	"sync"
	"time"
)

const (
	modelCatalogCacheEntries = 64
	modelCatalogFreshFor     = 10 * time.Minute
	modelCatalogStaleFor     = time.Hour
	modelCatalogRetryAfter   = 30 * time.Second
	modelCatalogFetchTimeout = 3 * time.Second
)

var (
	errModelCatalogClosed      = errors.New("model catalog cache closed")
	errModelCatalogInvalidated = errors.New("model catalog cache invalidated")
)

// ModelCatalogCache owns bounded Models API requests and account-scoped results.
// Construct it with NewModelCatalogCache and close it with its owning agent.
type ModelCatalogCache struct {
	mu         sync.Mutex
	entries    map[[sha256.Size]byte]*modelCatalogEntry
	flights    map[*modelCatalogFlight]struct{}
	generation uint64
	sequence   uint64
	closed     bool
	closeDone  chan struct{}
	workers    sync.WaitGroup
	transport  http.RoundTripper
	now        func() time.Time
}

type modelCatalogEntry struct {
	models    []APIModel
	fetchedAt time.Time
	retryAt   time.Time
	lastErr   error
	flight    *modelCatalogFlight
	used      uint64
}

type modelCatalogFlight struct {
	done       chan struct{}
	cancel     context.CancelFunc
	generation uint64
	models     []APIModel
	err        error
}

// NewModelCatalogCache creates an isolated cache and HTTP transport.
func NewModelCatalogCache() *ModelCatalogCache {
	return &ModelCatalogCache{
		entries:   make(map[[sha256.Size]byte]*modelCatalogEntry),
		flights:   make(map[*modelCatalogFlight]struct{}),
		closeDone: make(chan struct{}), now: time.Now,
		transport: &http.Transport{
			MaxIdleConns: modelCatalogCacheEntries, MaxIdleConnsPerHost: modelCatalogCacheEntries,
			MaxConnsPerHost: modelCatalogCacheEntries, IdleConnTimeout: time.Minute,
			TLSHandshakeTimeout: modelCatalogFetchTimeout, ResponseHeaderTimeout: modelCatalogFetchTimeout,
			MaxResponseHeaderBytes: 64 << 10,
		},
	}
}

// List returns fresh facts, or bounded stale facts while a shared refresh runs.
// Canceling a caller only stops that caller's wait, not the shared request.
func (c *ModelCatalogCache) List(ctx context.Context, access ModelCatalogAccess) ([]APIModel, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	if _, err := modelCatalogURL(access); err != nil {
		return nil, err
	}

	key := modelCatalogKey(access)

	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()

		return nil, errModelCatalogClosed
	}

	entry, err := c.entryLocked(key)
	if err != nil {
		c.mu.Unlock()

		return nil, err
	}

	now := c.now()
	if models, cachedErr, ready := entry.cached(now); ready {
		c.mu.Unlock()

		return models, cachedErr
	}

	if entry.flight == nil {
		if len(c.flights) >= modelCatalogCacheEntries {
			c.mu.Unlock()

			return nil, errModelCatalogUnavailable
		}

		c.startLocked(access, entry)
	}

	flight := entry.flight
	if entry.stale(now) {
		models := cloneAPIModels(entry.models)
		c.mu.Unlock()

		return models, nil
	}

	c.mu.Unlock()

	return c.wait(ctx, flight)
}

func modelCatalogKey(access ModelCatalogAccess) [sha256.Size]byte {
	identity := []byte(access.Endpoint + "\x00" + access.Credential + "\x00")
	if access.OAuth {
		identity[len(identity)-1] = 1
	}

	return sha256.Sum256(identity)
}

func (c *ModelCatalogCache) entryLocked(key [sha256.Size]byte) (*modelCatalogEntry, error) {
	entry := c.entries[key]
	if entry == nil {
		if len(c.entries) >= modelCatalogCacheEntries && !c.evictLocked() {
			return nil, errModelCatalogUnavailable
		}

		entry = &modelCatalogEntry{}
		c.entries[key] = entry
	}

	c.sequence++
	entry.used = c.sequence

	return entry, nil
}

func (c *ModelCatalogCache) evictLocked() bool {
	var oldest *modelCatalogEntry

	var oldestKey [sha256.Size]byte

	for key, entry := range c.entries {
		if entry.flight == nil && (oldest == nil || entry.used < oldest.used) {
			oldest, oldestKey = entry, key
		}
	}

	if oldest == nil {
		return false
	}

	delete(c.entries, oldestKey)

	return true
}

func (e *modelCatalogEntry) stale(now time.Time) bool {
	return e.models != nil && now.Before(e.fetchedAt.Add(modelCatalogStaleFor))
}

func (e *modelCatalogEntry) cached(now time.Time) ([]APIModel, error, bool) {
	if e.models != nil && now.Before(e.fetchedAt.Add(modelCatalogFreshFor)) {
		return cloneAPIModels(e.models), nil, true
	}

	if now.Before(e.retryAt) {
		if e.lastErr == errModelCatalogTransient && e.stale(now) {
			return cloneAPIModels(e.models), nil, true
		}

		return nil, e.lastErr, true
	}

	return nil, nil, false
}

func (c *ModelCatalogCache) startLocked(access ModelCatalogAccess, entry *modelCatalogEntry) {
	ctx, cancel := context.WithTimeout(context.Background(), modelCatalogFetchTimeout)
	flight := &modelCatalogFlight{done: make(chan struct{}), cancel: cancel, generation: c.generation}
	entry.flight = flight
	c.flights[flight] = struct{}{}

	c.workers.Go(func() {
		defer cancel()

		models, err := readModelCatalogAPI(ctx, c.transport, access)
		c.finish(entry, flight, models, err)
	})
}

func (c *ModelCatalogCache) finish(entry *modelCatalogEntry, flight *modelCatalogFlight, models []APIModel, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	defer close(flight.done)

	delete(c.flights, flight)
	entry.flight = nil

	switch {
	case c.closed:
		flight.err = errModelCatalogClosed
	case flight.generation != c.generation:
		flight.err = errModelCatalogInvalidated
	default:
		entry.publish(c.now(), models, err)
		flight.models, flight.err = models, err
	}
}

func (e *modelCatalogEntry) publish(now time.Time, models []APIModel, err error) {
	if err == nil {
		e.models = models
		e.fetchedAt, e.retryAt, e.lastErr = now, time.Time{}, nil

		return
	}

	e.retryAt, e.lastErr = now.Add(modelCatalogRetryAfter), err
	if err != errModelCatalogTransient {
		e.models = nil
		e.fetchedAt = time.Time{}
	}
}

func (c *ModelCatalogCache) wait(ctx context.Context, flight *modelCatalogFlight) ([]APIModel, error) {
	select {
	case <-ctx.Done():
		return nil, ctx.Err()
	case <-flight.done:
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	if c.closed {
		return nil, errModelCatalogClosed
	}

	if flight.generation != c.generation {
		return nil, errModelCatalogInvalidated
	}

	return cloneAPIModels(flight.models), flight.err
}

// Invalidate clears all cached identities and fences any pending publication.
func (c *ModelCatalogCache) Invalidate() {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.invalidateLocked()
}

func (c *ModelCatalogCache) invalidateLocked() {
	c.generation++
	clear(c.entries)

	for flight := range c.flights {
		flight.cancel()
	}
}

// Close prevents further requests, cancels outstanding work, and joins workers.
func (c *ModelCatalogCache) Close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		<-c.closeDone

		return
	}

	c.closed = true
	c.invalidateLocked()
	c.mu.Unlock()
	c.workers.Wait()

	if transport, ok := c.transport.(interface{ CloseIdleConnections() }); ok {
		transport.CloseIdleConnections()
	}

	close(c.closeDone)
}

func cloneAPIModels(models []APIModel) []APIModel {
	cloned := slices.Clone(models)
	for i := range cloned {
		cloned[i].SupportedEffortLevels = slices.Clone(cloned[i].SupportedEffortLevels)
	}

	return cloned
}
