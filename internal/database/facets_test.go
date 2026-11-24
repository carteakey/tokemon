package database

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"sync"
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
	if len(first.Machines) != 1 || first.Machines[0] != "machine-a" || len(first.Projects) != 0 {
		t.Fatalf("facets = %+v, want machine-a and empty projects", first)
	}

	// Second read is served from the cache and must be identical.
	second, err := store.Facets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Machines) != 1 || second.Machines[0] != "machine-a" {
		t.Fatalf("cached facets machines = %v, want [machine-a]", second.Machines)
	}

	evB := facetsEvent("machine-b", "prov", "model", "tool", 50)
	evB.Project = "proj-one"
	if _, err := store.Ingest(ctx, []usage.Event{evB}); err != nil {
		t.Fatal(err)
	}
	third, err := store.Facets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(third.Machines) != 2 || !slices.Equal(third.Projects, []string{"proj-one"}) {
		t.Fatalf("facets after ingest = %+v, want both machines and [proj-one]", third)
	}

	// The cached path and the raw compute must never diverge.
	direct, err := store.analyticsFacets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(direct.Machines) != len(third.Machines) || !slices.Equal(direct.Projects, third.Projects) {
		t.Fatalf("cached facets = %+v, direct compute = %+v", third, direct)
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

func TestIncrementalFacetsPreservesAndInvalidatesAccurately(t *testing.T) {
	modelCatalog := &catalog.Catalog{
		Models: map[string]catalog.Model{
			"claude-sonnet-4": {Provider: "anthropic", DisplayName: "Sonnet 4", Aliases: []string{"sonnet-alias"}},
		},
		Aliases: map[string]string{
			"sonnet-alias": "claude-sonnet-4",
		},
	}
	store, err := Open(t.TempDir()+"/tokemon.db", modelCatalog)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()

	tsOld := time.Date(2026, 1, 10, 12, 0, 0, 0, time.UTC)
	tsNew := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	if _, err := store.Ingest(ctx, []usage.Event{
		{
			SchemaVersion: usage.SchemaVersion, EventID: "inc-1", Timestamp: tsNew,
			MachineID: "m2", Project: "proj-z", Provider: "anthropic", Model: "sonnet-alias", Tool: "claude-code",
			TotalTokens: usage.Int64(100), TokenAccuracy: usage.AccuracyReported,
			Source: usage.Source{Adapter: "claude", AdapterVersion: "1"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	f1, err := store.Facets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f1.Models, []string{"claude-sonnet-4"}) || !slices.Equal(f1.Projects, []string{"proj-z"}) {
		t.Fatalf("resolved facets = %+v, want model [claude-sonnet-4] and project [proj-z]", f1)
	}
	// Mutating returned slice must not affect cached slices.
	f1.Machines[0] = "mutated"
	f1.Projects[0] = "mutated-proj"
	f1Again, err := store.Facets(ctx)
	if err != nil || f1Again.Machines[0] != "m2" || f1Again.Projects[0] != "proj-z" {
		t.Fatalf("caller mutation leaked into cache: %+v, err=%v", f1Again, err)
	}

	store.facetsMu.RLock()
	ptrBefore := store.facetsCache
	store.facetsMu.RUnlock()

	// Ingest with already-known facets (and empty project) must keep exact cache pointer.
	if _, err := store.Ingest(ctx, []usage.Event{
		{
			SchemaVersion: usage.SchemaVersion, EventID: "inc-2", Timestamp: tsNew,
			MachineID: "m2", Project: "", Provider: "anthropic", Model: "sonnet-alias", Tool: "claude-code",
			TotalTokens: usage.Int64(50), TokenAccuracy: usage.AccuracyReported,
			Source: usage.Source{Adapter: "claude", AdapterVersion: "1"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	store.facetsMu.RLock()
	ptrAfterKnown := store.facetsCache
	store.facetsMu.RUnlock()
	if ptrAfterKnown != ptrBefore {
		t.Fatal("ingest with known facets replaced cache pointer unexpectedly")
	}

	// Non-facet-changing refresh (only TotalTokens changes) preserves cache pointer.
	if _, err := store.Ingest(ctx, []usage.Event{
		{
			SchemaVersion: usage.SchemaVersion, EventID: "inc-2", Timestamp: tsNew,
			MachineID: "m2", Project: "", Provider: "anthropic", Model: "sonnet-alias", Tool: "claude-code",
			TotalTokens: usage.Int64(75), TokenAccuracy: usage.AccuracyReported,
			Source: usage.Source{Adapter: "claude", AdapterVersion: "1"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	store.facetsMu.RLock()
	ptrAfterNoopRefresh := store.facetsCache
	store.facetsMu.RUnlock()
	if ptrAfterNoopRefresh != ptrBefore {
		t.Fatal("non-facet-changing refresh invalidated facets cache")
	}

	// Ingest with a new machine, model, and project sorts into the cached slices cleanly.
	if _, err := store.Ingest(ctx, []usage.Event{
		{
			SchemaVersion: usage.SchemaVersion, EventID: "inc-3", Timestamp: tsOld,
			MachineID: "m1", Project: "/tmp/proj-a", Provider: "openai", Model: "alpha-model", Tool: "codex",
			TotalTokens: usage.Int64(25), TokenAccuracy: usage.AccuracyReported,
			Source: usage.Source{Adapter: "codex", AdapterVersion: "1"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	f2, err := store.Facets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f2.Machines, []string{"m1", "m2"}) ||
		!slices.Equal(f2.Providers, []string{"anthropic", "openai"}) ||
		!slices.Equal(f2.Models, []string{"alpha-model", "claude-sonnet-4"}) ||
		!slices.Equal(f2.Tools, []string{"claude-code", "codex"}) ||
		!slices.Equal(f2.Projects, []string{"proj-a", "proj-z"}) {
		t.Fatalf("incremental facets mismatch: %+v", f2)
	}

	// Facet-changing refresh on inc-3 (model & project changed) invalidates stale model & project.
	if _, err := store.Ingest(ctx, []usage.Event{
		{
			SchemaVersion: usage.SchemaVersion, EventID: "inc-3", Timestamp: tsOld,
			MachineID: "m1", Project: "proj-b", Provider: "openai", Model: "beta-model", Tool: "codex",
			TotalTokens: usage.Int64(25), TokenAccuracy: usage.AccuracyReported,
			Source: usage.Source{Adapter: "codex", AdapterVersion: "1"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	f3, err := store.Facets(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(f3.Models, []string{"beta-model", "claude-sonnet-4"}) ||
		!slices.Equal(f3.Projects, []string{"proj-b", "proj-z"}) {
		t.Fatalf("facets after facet-changing refresh = %+v, want beta-model & proj-b", f3)
	}

	// Codex aggregate snapshot superseded by detailed Codex event invalidates aggregate's stale model.
	if _, err := store.Ingest(ctx, []usage.Event{
		{
			SchemaVersion: usage.SchemaVersion, EventID: "codex-agg", Timestamp: tsNew,
			MachineID: "m2", Provider: "openai", Model: "agg-only-model", Tool: "codex",
			SessionID: "codex-sess", TotalTokens: usage.Int64(10), TokenAccuracy: usage.AccuracyEstimated,
			Source: usage.Source{Adapter: "codex", AdapterVersion: "1"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	f4, err := store.Facets(ctx)
	if err != nil || !slices.Contains(f4.Models, "agg-only-model") {
		t.Fatalf("facets before codex supersede = %+v, err=%v", f4, err)
	}
	if _, err := store.Ingest(ctx, []usage.Event{
		{
			SchemaVersion: usage.SchemaVersion, EventID: "codex-det", Timestamp: tsNew,
			MachineID: "m2", Provider: "openai", Model: "beta-model", Tool: "codex",
			SessionID: "codex-sess", InputTokens: usage.Int64(6), OutputTokens: usage.Int64(4),
			CacheReadTokens: usage.Int64(0), CacheWriteTokens: usage.Int64(0), ReasoningTokens: usage.Int64(0),
			TotalTokens: usage.Int64(10), TokenAccuracy: usage.AccuracyReported,
			Source: usage.Source{Adapter: "codex", AdapterVersion: "1"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	f5, err := store.Facets(ctx)
	if err != nil || slices.Contains(f5.Models, "agg-only-model") {
		t.Fatalf("codex supersede did not drop stale model from facets: %+v, err=%v", f5, err)
	}

	// PurgeBefore deleting 0 rows preserves cache; deleting inc-3 drops m1.
	store.facetsMu.RLock()
	ptrBeforeNoopPurge := store.facetsCache
	store.facetsMu.RUnlock()
	if deleted, err := store.PurgeBefore(ctx, "2025-01-01"); err != nil || deleted != 0 {
		t.Fatalf("noop PurgeBefore = %d, err=%v", deleted, err)
	}
	store.facetsMu.RLock()
	ptrAfterNoopPurge := store.facetsCache
	store.facetsMu.RUnlock()
	if ptrAfterNoopPurge != ptrBeforeNoopPurge {
		t.Fatal("noop PurgeBefore invalidated facets cache")
	}

	if deleted, err := store.PurgeBefore(ctx, "2026-06-01"); err != nil || deleted != 1 {
		t.Fatalf("PurgeBefore deleted = %d, err=%v", deleted, err)
	}
	f6, err := store.Facets(ctx)
	if err != nil || slices.Contains(f6.Machines, "m1") {
		t.Fatalf("PurgeBefore did not invalidate m1 from facets: %+v, err=%v", f6, err)
	}
	direct, err := store.analyticsFacets(ctx)
	if err != nil || !slices.Equal(f6.Machines, direct.Machines) || !slices.Equal(f6.Models, direct.Models) || !slices.Equal(f6.Projects, direct.Projects) {
		t.Fatalf("facets %+v diverged from direct %+v, err=%v", f6, direct, err)
	}
	if slices.Contains(f6.Projects, "proj-b") {
		t.Fatalf("PurgeBefore did not drop proj-b from facets: %+v", f6)
	}
}

func TestFacetsConcurrentIngestPurgeRace(t *testing.T) {
	store, err := Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()

	var wg sync.WaitGroup
	for w := 0; w < 2; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 15; i++ {
				if _, err := store.Ingest(ctx, []usage.Event{{
					SchemaVersion: usage.SchemaVersion,
					EventID:       fmt.Sprintf("race-%d-%d", worker, i),
					Timestamp:     time.Date(2026, 6, 1+i%20, 12, 0, 0, 0, time.UTC),
					MachineID:     fmt.Sprintf("m-%d-%d", worker, i%4),
					Project:       fmt.Sprintf("proj-%d-%d", worker, i%3),
					Provider:      "openai",
					Model:         fmt.Sprintf("model-%d", i%5),
					Tool:          "codex",
					TotalTokens:   usage.Int64(10),
					TokenAccuracy: usage.AccuracyReported,
					Source:        usage.Source{Adapter: "codex", AdapterVersion: "1"},
				}}); err != nil {
					t.Errorf("ingest: %v", err)
					return
				}
				if i%5 == 0 {
					if _, err := store.PurgeBefore(ctx, "2026-06-05"); err != nil {
						t.Errorf("purge: %v", err)
						return
					}
				}
			}
		}(w)
	}
	for r := 0; r < 6; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 20; i++ {
				if _, err := store.Facets(ctx); err != nil {
					t.Errorf("facets: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
}
