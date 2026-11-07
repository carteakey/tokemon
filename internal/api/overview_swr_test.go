package api

import (
	"context"
	"testing"
	"time"

	"github.com/tokemon/tokemon/internal/catalog"
	"github.com/tokemon/tokemon/internal/database"
	"github.com/tokemon/tokemon/internal/usage"
)

// After an ingest invalidates the overview, readers must be served the last
// good result immediately while one background compute refreshes it — a
// reload must never wait on a full recompute.
func TestOverviewServesStaleWhileRefreshing(t *testing.T) {
	store, err := database.Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server, err := newTestServer(store, "")
	if err != nil {
		t.Fatal(err)
	}
	event := usage.Event{
		SchemaVersion: usage.SchemaVersion, EventID: "overview-swr", Timestamp: time.Now().UTC(),
		MachineID: "machine", Provider: "provider", Model: "model", Tool: "codex",
		TotalTokens: usage.Int64(42), TokenAccuracy: usage.AccuracyReported,
		Source: usage.Source{Adapter: "codex", AdapterVersion: "test"},
	}
	if _, err := store.Ingest(context.Background(), []usage.Event{event}); err != nil {
		t.Fatal(err)
	}

	first, _, err := server.cachedOverview(context.Background())
	if err != nil {
		t.Fatalf("warm overview: %v", err)
	}
	if first.LifetimeTokens != 42 {
		t.Fatalf("lifetime tokens = %d, want 42", first.LifetimeTokens)
	}

	server.invalidateOverviewCache()

	// The invalidated cache still serves the last good result immediately.
	stale, _, err := server.cachedOverview(context.Background())
	if err != nil {
		t.Fatalf("stale overview: %v", err)
	}
	if stale.LifetimeTokens != 42 {
		t.Fatalf("stale lifetime tokens = %d, want 42", stale.LifetimeTokens)
	}

	// A background refresh installs a fresh entry without blocking the reader.
	deadline := time.Now().Add(3 * time.Second)
	for {
		server.overviewMu.Lock()
		fresh := server.overviewCache != nil && !server.overviewCache.fetched.IsZero()
		server.overviewMu.Unlock()
		if fresh {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background overview refresh did not complete")
		}
		time.Sleep(10 * time.Millisecond)
	}
}
