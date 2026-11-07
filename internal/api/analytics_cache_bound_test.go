package api

import (
	"context"
	"testing"
	"time"

	"github.com/tokemon/tokemon/internal/catalog"
	"github.com/tokemon/tokemon/internal/database"
)

func boundQuery(machine string) database.AnalyticsQuery {
	return database.AnalyticsQuery{Period: "30d", Dimension: "projects", Machine: machine, Now: time.Now().UTC()}
}

// The cache is bounded: inserting one key past the bound evicts exactly one
// entry, and the victim is the least recently served one — a filter that
// keeps being read survives.
func TestAnalyticsCacheEvictsLeastRecentlyServed(t *testing.T) {
	store, err := database.Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server, err := newTestServer(store, "")
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	fill := func(count int) {
		for index := 0; index < count; index++ {
			query := boundQuery("machine-" + string(rune('a'+index%26)) + string(rune('a'+index/26)))
			if _, err := server.cachedAnalytics(ctx, query); err != nil {
				t.Fatal(err)
			}
		}
	}
	fill(analyticsCacheMaxEntries)
	if len(server.analyticsCache) != analyticsCacheMaxEntries {
		t.Fatalf("cache length = %d, want %d", len(server.analyticsCache), analyticsCacheMaxEntries)
	}

	// Serve the oldest entry again right before the overflow insert: LRU must
	// keep it and evict a different one instead.
	kept := analyticsCacheKey(boundQuery("machine-aa"))
	if _, err := server.cachedAnalytics(ctx, boundQuery("machine-aa")); err != nil {
		t.Fatal(err)
	}
	oldest := ""
	for key, entry := range server.analyticsCache {
		if key != kept && (oldest == "" || entry.lastUse.Before(server.analyticsCache[oldest].lastUse)) {
			oldest = key
		}
	}
	if oldest == "" {
		t.Fatal("no eviction candidate found")
	}

	// Insert one more filter past the bound.
	if _, err := server.cachedAnalytics(ctx, boundQuery("machine-overflow")); err != nil {
		t.Fatal(err)
	}
	if len(server.analyticsCache) != analyticsCacheMaxEntries {
		t.Fatalf("cache length after overflow = %d, want %d", len(server.analyticsCache), analyticsCacheMaxEntries)
	}
	if _, ok := server.analyticsCache[kept]; !ok {
		t.Fatal("the most recently served entry was evicted")
	}
	if _, ok := server.analyticsCache[oldest]; ok {
		t.Fatal("the least recently served entry survived eviction")
	}
}

// An entry with a compute in flight is never evicted, and a stale entry (no
// in-flight compute, zero fetched) is a safe victim.
func TestAnalyticsCacheEvictionSkipsInFlight(t *testing.T) {
	store, err := database.Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server, err := newTestServer(store, "")
	if err != nil {
		t.Fatal(err)
	}

	server.analyticsMu.Lock()
	// The cache at the bound: one stale servable entry, one servable entry
	// whose refresh is in flight, and enough quiet entries to overflow.
	staleKey := "stale"
	staleEntry := &analyticsCacheEntry{result: database.Analytics{}, fetched: time.Time{}, hasResult: true, lastUse: time.Now().Add(-time.Hour)}
	inFlightKey := "in-flight"
	inFlight := &analyticsCacheEntry{fetched: time.Now().Add(-2 * time.Hour), hasResult: true, lastUse: time.Now().Add(-2 * time.Hour), ready: make(chan struct{})}
	server.analyticsCache = map[string]*analyticsCacheEntry{
		staleKey:    staleEntry,
		inFlightKey: inFlight,
	}
	quiet := analyticsCacheMaxEntries - 1
	for index := 0; index < quiet; index++ {
		key := analyticsCacheKey(boundQuery("machine-" + string(rune('a'+index%26)) + string(rune('a'+index/26))))
		server.analyticsCache[key] = &analyticsCacheEntry{result: database.Analytics{}, fetched: time.Now().Add(-time.Minute), hasResult: true, lastUse: time.Now().Add(-time.Minute)}
	}
	server.analyticsMu.Unlock()

	server.analyticsMu.Lock()
	server.evictAnalyticsEntriesLocked()
	server.analyticsMu.Unlock()

	if _, ok := server.analyticsCache[inFlightKey]; !ok {
		t.Fatal("the in-flight entry was evicted")
	}
	if _, ok := server.analyticsCache[staleKey]; ok {
		t.Fatal("the stale entry was not evicted although it was the least recently served evictable entry")
	}
	if len(server.analyticsCache) > analyticsCacheMaxEntries {
		t.Fatalf("cache length = %d, want at most %d", len(server.analyticsCache), analyticsCacheMaxEntries)
	}
}
