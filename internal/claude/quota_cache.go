package claude

import (
	"context"
	"errors"
	"slices"
	"sync"
	"time"
)

// QuotaCache shares observations and probe admission across sessions of one account.
type QuotaCache struct {
	mu      sync.Mutex
	entries map[[32]byte]*quotaEntry
	now     func() time.Time
}

type quotaEntry struct {
	windows          map[string]QuotaWindow
	versions         map[string]uint64
	dirty            map[string]bool
	invalidatedUntil map[string]time.Time
	blocked          map[string]time.Time
	lanes            [2]quotaLane
	running          chan struct{}
	lastUsed         time.Time
	notAuthenticated bool
	err              error
}

type quotaLane struct {
	next     time.Time
	failures int
}

func NewQuotaCache() *QuotaCache {
	return &QuotaCache{entries: make(map[[32]byte]*quotaEntry), now: time.Now}
}

func (c *QuotaCache) entry(key [32]byte) *quotaEntry {
	e := c.entries[key]
	if e == nil {
		for k, old := range c.entries {
			if old.running == nil && c.now().Sub(old.lastUsed) > 6*time.Hour {
				delete(c.entries, k)
			}
		}

		e = &quotaEntry{windows: make(map[string]QuotaWindow), versions: make(map[string]uint64), dirty: make(map[string]bool), invalidatedUntil: make(map[string]time.Time), blocked: make(map[string]time.Time)}
		c.entries[key] = e
	}

	e.lastUsed = c.now()

	return e
}

// Read admits at most one refresh per account; waiting callers share its result.
func (c *QuotaCache) Read(ctx context.Context, key [32]byte, probe func(context.Context, bool) (QuotaResult, error)) (QuotaResult, error) {
	if err := ctx.Err(); err != nil {
		return QuotaResult{}, err
	}

	c.mu.Lock()

	e := c.entry(key)
	if done := e.running; done != nil {
		c.mu.Unlock()

		select {
		case <-ctx.Done():
			return QuotaResult{}, ctx.Err()
		case <-done:
		}

		c.mu.Lock()
		defer c.mu.Unlock()

		return e.snapshot()
	}

	due := e.due(c.now())
	if !due[0] && !due[1] {
		result, err := e.snapshot()
		c.mu.Unlock()

		return result, err
	}

	e.running = make(chan struct{})
	c.mu.Unlock()

	for lane, needed := range due {
		if !needed {
			continue
		}

		c.mu.Lock()
		versions := map[string]uint64{QuotaSession: e.versions[QuotaSession], QuotaWeekly: e.versions[QuotaWeekly], QuotaFable: e.versions[QuotaFable]}
		stillDue := e.due(c.now())[lane]
		c.mu.Unlock()

		if !stillDue {
			continue
		}

		result, err := probe(ctx, lane == 1)

		c.mu.Lock()
		e.applyProbe(lane, result, err, versions, c.now())
		c.mu.Unlock()

		if ctx.Err() != nil || result.NotAuthenticated {
			break
		}
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	close(e.running)
	e.running = nil

	return e.snapshot()
}

func (e *quotaEntry) due(now time.Time) [2]bool {
	for id, reset := range e.blocked {
		if !reset.IsZero() && !now.Before(reset) {
			delete(e.blocked, id)

			lane := 0
			if id == QuotaFable {
				lane = 1
			}

			if e.lanes[lane].failures == 0 {
				e.lanes[lane].next = time.Time{}
			}
		}
	}

	var due [2]bool
	for lane := range due {
		blocked := false
		for id, reset := range e.blocked {
			if (id != QuotaFable || lane == 1) && now.Before(reset) {
				blocked = true
			}
		}

		due[lane] = !blocked && !now.Before(e.lanes[lane].next)
	}

	return due
}

func (e *quotaEntry) applyProbe(lane int, result QuotaResult, err error, versions map[string]uint64, now time.Time) {
	state := &e.lanes[lane]

	nextBefore := state.next
	if err != nil {
		state.failures++
		backoff := []time.Duration{5 * time.Minute, 15 * time.Minute, time.Hour}[min(state.failures-1, 2)]

		state.next = now.Add(backoff)
		if result.RetryAfter.After(state.next) {
			state.next = result.RetryAfter
		}

		e.err = err

		return
	}

	state.failures = 0

	ttl := QuotaHaikuTTL
	if lane == 1 {
		ttl = QuotaFableTTL
	}

	if result.Unavailable || result.NotAuthenticated {
		ttl = 6 * time.Hour
	}

	state.next = now.Add(ttl)
	if result.RetryAfter.After(state.next) {
		state.next = result.RetryAfter
	}

	e.notAuthenticated = result.NotAuthenticated
	if result.NotAuthenticated {
		clear(e.windows)

		for i := range e.lanes {
			e.lanes[i].next = state.next
		}
	}

	e.err = nil

	for _, w := range result.Windows {
		if lane == 0 && w.ID == QuotaFable {
			continue
		}

		if e.versions[w.ID] != versions[w.ID] {
			if e.dirty[w.ID] {
				state.next = nextBefore
			}

			continue
		}

		e.windows[w.ID] = w
		e.dirty[w.ID] = false
		delete(e.blocked, w.ID)

		if result.Exhausted == w.ID && w.ResetsAt.After(now) {
			e.blocked[w.ID] = w.ResetsAt
		}

		if w.ID == QuotaFable && lane == 1 || w.ID != QuotaFable && lane == 0 {
			if w.StaleAt.Before(state.next) && w.StaleAt.After(now) {
				state.next = w.StaleAt
			}
		}
	}

	if result.RetryAfter.After(state.next) {
		state.next = result.RetryAfter
	}
}

func (e *quotaEntry) snapshot() (QuotaResult, error) {
	result := QuotaResult{NotAuthenticated: e.notAuthenticated}
	for _, w := range e.windows {
		result.Windows = append(result.Windows, w)
	}

	slices.SortFunc(result.Windows, func(a, b QuotaWindow) int {
		if a.ID < b.ID {
			return -1
		}

		if a.ID > b.ID {
			return 1
		}

		return 0
	})

	if len(result.Windows) == 0 && e.err != nil {
		return result, e.err
	}

	return result, nil
}

// Observe updates only reported windows; a scoped refusal invalidates only its window.
func (c *QuotaCache) Observe(key [32]byte, windows []QuotaWindow, exhausted string) {
	c.mu.Lock()
	defer c.mu.Unlock()

	e := c.entry(key)
	now := c.now()

	if len(windows) > 0 {
		e.notAuthenticated = false
		e.err = nil
	}

	seen := make(map[string]bool, len(windows))
	for _, w := range windows {
		e.windows[w.ID] = w
		e.versions[w.ID]++
		e.dirty[w.ID] = false
		seen[w.ID] = true

		lane := 0
		if w.ID == QuotaFable {
			lane = 1
		}

		if e.lanes[lane].failures == 0 {
			e.lanes[lane].next = w.StaleAt
		}

		delete(e.blocked, w.ID)
	}

	if exhausted == "" {
		return
	}

	ids := []string{exhausted}
	if exhausted == QuotaUnknown {
		ids = []string{QuotaSession, QuotaWeekly, QuotaFable}
	}

	for _, id := range ids {
		w, exists := e.windows[id]
		if seen[id] {
			if w.ResetsAt.After(now) {
				e.blocked[id] = w.ResetsAt
			}

			continue
		}

		if e.dirty[id] {
			continue
		}

		e.dirty[id] = true
		e.versions[id]++

		if exists {
			w.StaleAt = now
			e.windows[id] = w
		}

		lane := 0
		if id == QuotaFable {
			lane = 1
		}

		if e.lanes[lane].failures == 0 && !now.Before(e.invalidatedUntil[id]) {
			e.lanes[lane].next = time.Time{}
			e.invalidatedUntil[id] = now.Add(quotaTTL(id))
		}
	}
}

var errQuotaProbe = errors.New("quota probe did not report allowance windows")
