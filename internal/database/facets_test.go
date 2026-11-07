package database

import (
	"context"
	"strconv"
	"testing"
	"time"

	"github.com/tokemon/tokemon/internal/catalog"
	"github.com/tokemon/tokemon/internal/usage"
)

func facetsEvent(machine, provider, model, tool string, tokens int64) usage.Event {
	return usage.Event{
		SchemaVersion: usage.SchemaVersion,
		EventID:       "ev-" + machine + "-" + tool + "-" + model + "-" + strconv.FormatInt(tokens, 10),
		Timestamp:     time.Now().UTC(),
		MachineID:     machine,
		Provider:      provider,
		Model:         model,
		Tool:          tool,
		TotalTokens:   usage.Int64(tokens),
		TokenAccuracy: usage.AccuracyReported,
		Source:        usage.Source{Adapter: "test", AdapterVersion: "1"},
	}
}

// Facets are filter-independent, so the cached Facets must always agree with
// a fresh compute, and an ingest must invalidate the cache immediately — the
// pickers would otherwise miss a newly seen source for up to the TTL.
func TestFacetsCacheTracksIngest(t *testing.T) {
	store, err := Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if _, err := store.Ingest(ctx, []usage.Event{facetsEvent("machine-a", "prov", "model", "tool", 100)}); err != nil {
		t.Fatal(err)
	}

	first, err := store.Facets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Machines) != 1 || first.Machines[0] != "machine-a" {
		t.Fatalf("facets machines = %v, want [machine-a]", first.Machines)
	}

	// Second read is served from the cache and must be identical.
	second, err := store.Facets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Machines) != 1 || second.Machines[0] != "machine-a" {
		t.Fatalf("cached facets machines = %v, want [machine-a]", second.Machines)
	}

	if _, err := store.Ingest(ctx, []usage.Event{facetsEvent("machine-b", "prov", "model", "tool", 50)}); err != nil {
		t.Fatal(err)
	}
	third, err := store.Facets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(third.Machines) != 2 {
		t.Fatalf("facets after ingest machines = %v, want both machines (cache not invalidated)", third.Machines)
	}

	// The cached path and the raw compute must never diverge.
	direct, err := store.analyticsFacets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(direct.Machines) != len(third.Machines) {
		t.Fatalf("cached facets = %v, direct compute = %v", third.Machines, direct.Machines)
	}
}

// Analytics must keep working with the facets cache in place: the summary,
// comparison, and breakdown all have their own data paths.
func TestAnalyticsUsesCachedFacets(t *testing.T) {
	store, err := Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()
	if _, err := store.Ingest(ctx, []usage.Event{
		facetsEvent("machine-a", "prov", "model", "tool", 100),
		facetsEvent("machine-a", "prov", "model", "tool", 40),
	}); err != nil {
		t.Fatal(err)
	}
	result, err := store.Analytics(ctx, AnalyticsQuery{Period: "30d", Dimension: "providers", Now: time.Now().UTC()})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Facets.Machines) != 1 || result.Facets.Machines[0] != "machine-a" {
		t.Fatalf("analytics facets machines = %v, want [machine-a]", result.Facets.Machines)
	}
	if result.Summary.Tokens != 140 {
		t.Fatalf("summary tokens = %d, want 140", result.Summary.Tokens)
	}
}
