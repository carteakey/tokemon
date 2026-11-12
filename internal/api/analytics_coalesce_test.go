package api

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/tokemon/tokemon/internal/catalog"
	"github.com/tokemon/tokemon/internal/database"
	"github.com/tokemon/tokemon/internal/usage"
)

// coalesceQuery pins one filter combination so every call in these tests hits
// the same analytics cache entry.
func coalesceQuery(now time.Time) database.AnalyticsQuery {
	return database.AnalyticsQuery{Period: "30d", Dimension: "projects", Now: now}
}

// newCoalesceServer seeds one 100-token event and returns a server whose
// analytics cache is still cold.
func newCoalesceServer(t *testing.T) (*Server, *database.Store) {
	t.Helper()
	store, err := database.Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	if _, err := store.Ingest(context.Background(), []usage.Event{cacheBenchEvent("ev-coalesce-a", "machine-a", 100)}); err != nil {
		t.Fatal(err)
	}
	server, err := newTestServer(store, "")
	if err != nil {
		t.Fatal(err)
	}
	return server, store
}

// warmCoalesceEntry fills the cache entry for coalesceQuery and returns its
// key and fetch time.
func warmCoalesceEntry(t *testing.T, server *Server) (string, time.Time) {
	t.Helper()
	query := coalesceQuery(time.Now())
	if _, err := server.cachedAnalytics(context.Background(), query); err != nil {
		t.Fatal(err)
	}
	key := analyticsCacheKey(query)
	server.analyticsMu.Lock()
	defer server.analyticsMu.Unlock()
	entry := server.analyticsCache[key]
	if entry == nil {
		t.Fatal("warm read did not populate the analytics cache entry")
	}
	return key, entry.fetched
}

// ingestCoalesceEvent writes one more 500-token event straight to the store,
// the write the ingest path invalidates the analytics cache for.
func ingestCoalesceEvent(t *testing.T, store *database.Store, eventID string) {
	t.Helper()
	if _, err := store.Ingest(context.Background(), []usage.Event{cacheBenchEvent(eventID, "machine-b", 500)}); err != nil {
		t.Fatal(err)
	}
}

// expireCoalescingWindow fast-forwards an entry past the window. The tests
// move the deadline directly because analyticsMinRefreshInterval is a
// constant and sleeping the real interval would slow the suite.
func expireCoalescingWindow(server *Server, key string) {
	server.analyticsMu.Lock()
	if entry := server.analyticsCache[key]; entry != nil {
		entry.refreshDue = time.Now().Add(-time.Millisecond)
	}
	server.analyticsMu.Unlock()
}

// waitCoalesceRefresh blocks until the background refresh for key installed a
// result newer than warmed.
func waitCoalesceRefresh(t *testing.T, server *Server, key string, warmed time.Time) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		server.analyticsMu.Lock()
		entry := server.analyticsCache[key]
		refreshed := entry != nil && entry.fetched.After(warmed)
		server.analyticsMu.Unlock()
		if refreshed {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("background analytics refresh did not complete")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

// A burst of invalidations inside the coalescing window must set exactly one
// refresh deadline — the window after the last refresh, never extended by the
// later invalidations — and start no compute of its own. After the window
// elapses, the next reader triggers exactly one refresh, concurrent readers
// during it start none, and the payload updates once it installs.
func TestAnalyticsInvalidationBurstProducesOneRefresh(t *testing.T) {
	server, store := newCoalesceServer(t)
	key, warmed := warmCoalesceEntry(t, server)
	ctx := context.Background()

	// Backdate the entry 1s so the window boundary sits a full second away
	// from now: a deadline that tracked the invalidation time instead of the
	// last refresh, or extended per invalidation, would land measurably
	// elsewhere.
	server.analyticsMu.Lock()
	server.analyticsCache[key].fetched = warmed.Add(-time.Second)
	warmed = server.analyticsCache[key].fetched
	server.analyticsMu.Unlock()

	ingestCoalesceEvent(t, store, "ev-coalesce-b")
	const burst = 5
	for range burst {
		server.invalidateAnalyticsCache()
	}

	server.analyticsMu.Lock()
	due := server.analyticsCache[key].refreshDue
	ready := server.analyticsCache[key].ready
	server.analyticsMu.Unlock()
	if !due.Equal(warmed.Add(analyticsMinRefreshInterval)) {
		t.Fatalf("refresh deadline = %v, want the window after the last refresh (%v); the burst stacked or extended deadlines", due, warmed.Add(analyticsMinRefreshInterval))
	}
	if ready != nil {
		t.Fatal("the invalidation burst started a compute")
	}
	if got := server.analyticsRefreshes.Load(); got != 0 {
		t.Fatalf("refreshes started by the burst = %d, want 0", got)
	}

	// After the window elapses the next reader triggers the one refresh and is
	// still served the stale payload immediately.
	expireCoalescingWindow(server, key)
	stale, err := server.cachedAnalytics(ctx, coalesceQuery(time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	if stale.Summary.Tokens != 100 {
		t.Fatalf("tokens served after the window = %d, want the cached 100", stale.Summary.Tokens)
	}
	if got := server.analyticsRefreshes.Load(); got != 1 {
		t.Fatalf("refreshes after the post-window reader = %d, want 1", got)
	}

	// Concurrent readers while the refresh runs must not each start one: each
	// is served the last good or the fresh result, but only one recompute may
	// ever run.
	var group sync.WaitGroup
	for index := 0; index < 8; index++ {
		group.Add(1)
		go func() {
			defer group.Done()
			if _, err := server.cachedAnalytics(ctx, coalesceQuery(time.Now())); err != nil {
				t.Error(err)
			}
		}()
	}
	group.Wait()
	waitCoalesceRefresh(t, server, key, warmed)

	fresh, err := server.cachedAnalytics(ctx, coalesceQuery(time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Summary.Tokens != 600 {
		t.Fatalf("tokens after the refresh = %d, want 600 once the ingested event counts", fresh.Summary.Tokens)
	}
	if got := server.analyticsRefreshes.Load(); got != 1 {
		t.Fatalf("total refreshes = %d, want 1: a reader started a second compute", got)
	}
}

// A reader during the coalescing window is served the cached result
// immediately: the payload must be the last good one and no compute may
// start, even though an ingest already landed.
func TestAnalyticsReaderInsideCoalescingWindowServesCache(t *testing.T) {
	server, store := newCoalesceServer(t)
	key, warmed := warmCoalesceEntry(t, server)
	ctx := context.Background()

	ingestCoalesceEvent(t, store, "ev-coalesce-b")
	server.invalidateAnalyticsCache()

	served, err := server.cachedAnalytics(ctx, coalesceQuery(time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	if served.Summary.Tokens != 100 {
		t.Fatalf("tokens served during the window = %d, want the cached 100", served.Summary.Tokens)
	}
	server.analyticsMu.Lock()
	entry := server.analyticsCache[key]
	ready, fetched := entry.ready, entry.fetched
	server.analyticsMu.Unlock()
	if ready != nil {
		t.Fatal("a reader inside the window started a compute")
	}
	if !fetched.Equal(warmed) {
		t.Fatal("the entry was refetched during the window")
	}
	if got := server.analyticsRefreshes.Load(); got != 0 {
		t.Fatalf("refreshes during the window = %d, want 0", got)
	}
}

// Coalescing only defers entries refreshed within the window: an entry last
// refreshed before the window stays due on the next read, as before.
func TestAnalyticsInvalidationOfOldEntryRefreshesOnNextRead(t *testing.T) {
	server, store := newCoalesceServer(t)
	key, warmed := warmCoalesceEntry(t, server)
	ctx := context.Background()

	server.analyticsMu.Lock()
	server.analyticsCache[key].fetched = warmed.Add(-analyticsMinRefreshInterval - time.Second)
	server.analyticsMu.Unlock()

	ingestCoalesceEvent(t, store, "ev-coalesce-b")
	server.invalidateAnalyticsCache()

	server.analyticsMu.Lock()
	due := server.analyticsCache[key].refreshDue
	server.analyticsMu.Unlock()
	if due.After(time.Now()) {
		t.Fatalf("refresh deadline = %v for an entry last refreshed before the window, want it due now", due)
	}

	stale, err := server.cachedAnalytics(ctx, coalesceQuery(time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	if stale.Summary.Tokens != 100 {
		t.Fatalf("tokens served = %d, want the cached 100", stale.Summary.Tokens)
	}
	if got := server.analyticsRefreshes.Load(); got != 1 {
		t.Fatalf("refreshes after the next read = %d, want 1", got)
	}
	waitCoalesceRefresh(t, server, key, warmed)
	fresh, err := server.cachedAnalytics(ctx, coalesceQuery(time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Summary.Tokens != 600 {
		t.Fatalf("tokens after the refresh = %d, want 600", fresh.Summary.Tokens)
	}
}
