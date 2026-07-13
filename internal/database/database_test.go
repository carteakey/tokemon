package database

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tokemon/tokemon/internal/catalog"
	"github.com/tokemon/tokemon/internal/usage"
)

func TestOpenBacksUpExistingDatabaseBeforeMigration(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "tokemon.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE sentinel (value TEXT); INSERT INTO sentinel VALUES ('preserved')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := Open(path, catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	store.Close()
	backups, err := filepath.Glob(filepath.Join(directory, "backups", "tokemon-v0-before-v1-*.db"))
	if err != nil || len(backups) != 1 {
		t.Fatalf("backups = %v, err = %v", backups, err)
	}
	backup, err := sql.Open("sqlite", backups[0])
	if err != nil {
		t.Fatal(err)
	}
	defer backup.Close()
	var value string
	if err := backup.QueryRow(`SELECT value FROM sentinel`).Scan(&value); err != nil || value != "preserved" {
		t.Fatalf("backup value = %q, err = %v", value, err)
	}

	reopened, err := Open(path, catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	reopened.Close()
	backups, _ = filepath.Glob(filepath.Join(directory, "backups", "*.db"))
	if len(backups) != 1 {
		t.Fatalf("ordinary restart created another backup: %v", backups)
	}
}

func TestPruneBackupsKeepsNewestFiles(t *testing.T) {
	directory := t.TempDir()
	for _, name := range []string{"tokemon-1.db", "tokemon-2.db", "tokemon-3.db", "other.db"} {
		if err := os.WriteFile(filepath.Join(directory, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := pruneBackups(directory, "tokemon-", 2); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	want := []string{"other.db", "tokemon-2.db", "tokemon-3.db"}
	if fmt.Sprint(names) != fmt.Sprint(want) {
		t.Fatalf("files = %v, want %v", names, want)
	}
}

func TestIngestIsIdempotentAndDerivesEvolution(t *testing.T) {
	inputPrice, outputPrice := 10.0, 20.0
	modelCatalog := catalog.MustNew(map[string]catalog.Model{
		"test-model": {
			Provider: "test", DisplayName: "Test Model", Aliases: []string{"raw-model"},
			Pricing: catalog.Pricing{Currency: "USD", Mode: "standard", VerifiedAt: "2026-07-12", Source: "https://example.com/pricing", InputPricePerMillion: &inputPrice, OutputPricePerMillion: &outputPrice},
		},
	})
	store, err := Open(t.TempDir()+"/tokemon.db", modelCatalog)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	event := usage.Event{
		SchemaVersion: usage.SchemaVersion,
		EventID:       "event-1",
		Timestamp:     time.Date(2026, 7, 11, 18, 42, 0, 0, time.UTC),
		MachineID:     "machine-1",
		Provider:      "test-provider",
		Model:         "raw-model",
		Tool:          "generic-jsonl",
		InputTokens:   usage.Int64(2),
		OutputTokens:  usage.Int64(8),
		TotalTokens:   usage.Int64(10),
		TokenAccuracy: usage.AccuracyReported,
		Source:        usage.Source{Adapter: "generic-jsonl", AdapterVersion: "test"},
	}
	result, err := store.Ingest(context.Background(), []usage.Event{event})
	if err != nil {
		t.Fatal(err)
	}
	if result.Accepted != 1 || result.CurrentTotal != 10 || !result.Evolved || result.CurrentStage != 1 {
		t.Fatalf("unexpected first ingest: %+v", result)
	}
	result, err = store.Ingest(context.Background(), []usage.Event{event})
	if err != nil {
		t.Fatal(err)
	}
	if result.Duplicates != 1 || result.Accepted != 0 || result.CurrentTotal != 10 {
		t.Fatalf("unexpected duplicate ingest: %+v", result)
	}
	events, err := store.Events(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].CanonicalModel != "test-model" || events[0].Cost == nil {
		t.Fatalf("unexpected round-trip events: %+v", events)
	}
	overview, err := store.Overview(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if overview.EstimatedCost.Amount != 0.00018 || overview.EstimatedCost.PricedTokens != 10 || overview.EstimatedCost.UnpricedTokens != 0 {
		t.Fatalf("unexpected estimated cost summary: %+v", overview.EstimatedCost)
	}
}

func TestOverviewCostPreservesUnpricedUsage(t *testing.T) {
	inputPrice := 10.0
	outputPrice := 0.0
	modelCatalog := catalog.MustNew(map[string]catalog.Model{
		"priced-model": {Provider: "test", DisplayName: "Priced Model", Aliases: []string{"priced"}, Pricing: catalog.Pricing{Currency: "USD", Mode: "standard", VerifiedAt: "2026-07-12", Source: "https://example.com/pricing", InputPricePerMillion: &inputPrice, OutputPricePerMillion: &outputPrice}},
	})
	store, err := Open(t.TempDir()+"/tokemon.db", modelCatalog)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	events := []usage.Event{
		{SchemaVersion: usage.SchemaVersion, EventID: "priced", Timestamp: time.Now().UTC(), MachineID: "machine", Provider: "provider", Model: "priced", Tool: "tool", InputTokens: usage.Int64(1_000_000), TotalTokens: usage.Int64(1_000_000), TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "test", AdapterVersion: "1"}},
		{SchemaVersion: usage.SchemaVersion, EventID: "unpriced", Timestamp: time.Now().UTC(), MachineID: "machine", Provider: "provider", Model: "unknown", Tool: "tool", TotalTokens: usage.Int64(500_000), TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "test", AdapterVersion: "1"}},
	}
	if _, err := store.Ingest(context.Background(), events); err != nil {
		t.Fatal(err)
	}
	overview, err := store.Overview(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if overview.EstimatedCost.Amount != 10 || overview.EstimatedCost.PricedTokens != 1_000_000 || overview.EstimatedCost.UnpricedTokens != 500_000 {
		t.Fatalf("unexpected estimated cost summary: %+v", overview.EstimatedCost)
	}
}

func TestOverviewReportsCacheHitRateAndDistinctThreads(t *testing.T) {
	store, err := Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	events := []usage.Event{
		{SchemaVersion: usage.SchemaVersion, EventID: "one", Timestamp: now, MachineID: "machine", SessionID: "thread-1", Provider: "openai", Model: "model", Tool: "codex", InputTokens: usage.Int64(20), CacheReadTokens: usage.Int64(80), OutputTokens: usage.Int64(5), TotalTokens: usage.Int64(105), TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "codex", AdapterVersion: "test"}},
		{SchemaVersion: usage.SchemaVersion, EventID: "two", Timestamp: now, MachineID: "machine", SessionID: "thread-1", Provider: "openai", Model: "model", Tool: "codex", InputTokens: usage.Int64(30), CacheReadTokens: usage.Int64(70), OutputTokens: usage.Int64(5), TotalTokens: usage.Int64(105), TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "codex", AdapterVersion: "test"}},
		{SchemaVersion: usage.SchemaVersion, EventID: "three", Timestamp: now, MachineID: "machine", SessionID: "thread-2", Provider: "openai", Model: "model", Tool: "codex", TotalTokens: usage.Int64(50), TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "codex", AdapterVersion: "test"}},
	}
	if _, err := store.Ingest(context.Background(), events); err != nil {
		t.Fatal(err)
	}
	overview, err := store.Overview(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if overview.Cache.CachedTokens != 150 || overview.Cache.EligibleTokens != 200 || overview.Cache.HitRate != 0.75 {
		t.Fatalf("unexpected cache summary: %+v", overview.Cache)
	}
	if overview.Threads != 2 {
		t.Fatalf("threads = %d, want 2", overview.Threads)
	}
}

func TestOverviewMergesProjectsAcrossMachines(t *testing.T) {
	store, err := Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	events := []usage.Event{
		{SchemaVersion: usage.SchemaVersion, EventID: "project-a", Timestamp: time.Now().UTC(), MachineID: "laptop", SessionID: "session-a", Project: "/Users/alice/repos/Carteakey.dev", Provider: "openai", Model: "model", Tool: "codex", TotalTokens: usage.Int64(100), TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "test", AdapterVersion: "1"}},
		{SchemaVersion: usage.SchemaVersion, EventID: "project-b", Timestamp: time.Now().UTC(), MachineID: "desktop", SessionID: "session-b", Project: "/home/alice/CARTEAKEY.DEV", Provider: "anthropic", Model: "model", Tool: "claude-code", TotalTokens: usage.Int64(50), TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "test", AdapterVersion: "1"}},
	}
	if _, err := store.Ingest(context.Background(), events); err != nil {
		t.Fatal(err)
	}
	overview, err := store.Overview(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(overview.ByProject) != 1 || overview.ByProject[0].Project != "carteakey.dev" || overview.ByProject[0].Tokens != 150 || overview.ByProject[0].Machines != 2 || overview.ByProject[0].Sessions != 2 {
		t.Fatalf("unexpected project summary: %+v", overview.ByProject)
	}
	eventsRoundTrip, err := store.Events(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if eventsRoundTrip[0].Project != "carteakey.dev" {
		t.Fatalf("stored project leaked or was not normalized: %+v", eventsRoundTrip[0])
	}
}

func TestIngestRefreshesAnExistingSnapshot(t *testing.T) {
	store, err := Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	event := usage.Event{
		SchemaVersion: usage.SchemaVersion,
		EventID:       "snapshot-1",
		Timestamp:     time.Date(2026, 7, 11, 18, 42, 0, 0, time.UTC),
		MachineID:     "machine-1",
		Provider:      "openai",
		Model:         "gpt-5.5",
		Tool:          "codex",
		TotalTokens:   usage.Int64(10),
		TokenAccuracy: usage.AccuracyReported,
		Source:        usage.Source{Adapter: "codex", AdapterVersion: "test"},
	}
	if _, err := store.Ingest(context.Background(), []usage.Event{event}); err != nil {
		t.Fatal(err)
	}

	event.TotalTokens = usage.Int64(25)
	result, err := store.Ingest(context.Background(), []usage.Event{event})
	if err != nil {
		t.Fatal(err)
	}
	if result.Accepted != 0 || result.Duplicates != 1 || result.CurrentTotal != 25 {
		t.Fatalf("unexpected refreshed snapshot result: %+v", result)
	}
	events, err := store.Events(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].TotalTokens == nil || *events[0].TotalTokens != 25 {
		t.Fatalf("unexpected refreshed event: %+v", events)
	}
}

func TestDetailedCodexUsageSupersedesAggregateSnapshot(t *testing.T) {
	inputPrice, outputPrice, cachePrice, cacheWritePrice := 1.0, 10.0, 0.1, 1.0
	modelCatalog := catalog.MustNew(map[string]catalog.Model{
		"gpt": {Provider: "openai", DisplayName: "GPT", Aliases: []string{"gpt"}, Pricing: catalog.Pricing{Currency: "USD", Mode: "standard", VerifiedAt: "2026-07-12", Source: "https://example.com/pricing", InputPricePerMillion: &inputPrice, OutputPricePerMillion: &outputPrice, CacheReadPricePerMillion: &cachePrice, CacheWritePricePerMillion: &cacheWritePrice}},
	})
	store, err := Open(t.TempDir()+"/tokemon.db", modelCatalog)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	aggregate := usage.Event{SchemaVersion: usage.SchemaVersion, EventID: "aggregate", Timestamp: time.Now().UTC(), MachineID: "machine", SessionID: "session", Provider: "openai", Model: "gpt", Tool: "codex", TotalTokens: usage.Int64(100), TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "codex", AdapterVersion: "0.2.0"}}
	if _, err := store.Ingest(context.Background(), []usage.Event{aggregate}); err != nil {
		t.Fatal(err)
	}
	detailed := usage.Event{SchemaVersion: usage.SchemaVersion, EventID: "detailed", Timestamp: time.Now().UTC(), MachineID: "machine", SessionID: "session", Provider: "openai", Model: "gpt", Tool: "codex", InputTokens: usage.Int64(10), OutputTokens: usage.Int64(10), CacheReadTokens: usage.Int64(80), CacheWriteTokens: usage.Int64(0), TotalTokens: usage.Int64(100), TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "codex", AdapterVersion: "0.3.0"}}
	result, err := store.Ingest(context.Background(), []usage.Event{detailed})
	if err != nil {
		t.Fatal(err)
	}
	if result.CurrentTotal != 100 {
		t.Fatalf("lifetime tokens = %d, want 100", result.CurrentTotal)
	}
	events, err := store.Events(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].EventID != "detailed" || events[0].Cost == nil {
		t.Fatalf("unexpected migrated events: %+v", events)
	}

	result, err = store.Ingest(context.Background(), []usage.Event{aggregate})
	if err != nil {
		t.Fatal(err)
	}
	if result.Accepted != 0 || result.Duplicates != 1 || result.CurrentTotal != 100 {
		t.Fatalf("legacy aggregate was not ignored after detailed backfill: %+v", result)
	}
	events, err = store.Events(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].EventID != "detailed" {
		t.Fatalf("legacy aggregate returned after detailed backfill: %+v", events)
	}
}

func TestTokenCompositionSeparatesCachedAndUnclassifiedTokens(t *testing.T) {
	store, err := Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	events := []usage.Event{
		{SchemaVersion: usage.SchemaVersion, EventID: "detailed-components", Timestamp: time.Now().UTC(), MachineID: "machine", Provider: "openai", Model: "model", Tool: "codex", InputTokens: usage.Int64(40), CacheReadTokens: usage.Int64(50), CacheWriteTokens: usage.Int64(10), OutputTokens: usage.Int64(20), TotalTokens: usage.Int64(120), TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "test", AdapterVersion: "1"}},
		{SchemaVersion: usage.SchemaVersion, EventID: "aggregate-only", Timestamp: time.Now().UTC(), MachineID: "machine", Provider: "other", Model: "model", Tool: "other", TotalTokens: usage.Int64(30), TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "test", AdapterVersion: "1"}},
	}
	if _, err := store.Ingest(context.Background(), events); err != nil {
		t.Fatal(err)
	}
	composition, err := store.TokenComposition(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := TokenComposition{InputTokens: 100, UncachedInputTokens: 40, CachedInputTokens: 60, OutputTokens: 20, UnclassifiedTokens: 30}
	if composition != want {
		t.Fatalf("composition = %+v, want %+v", composition, want)
	}
}

func TestActivityBuildsPixelCalendarAndPreservesUnknownTotals(t *testing.T) {
	store, err := Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	base := func(id string, timestamp time.Time, total *int64) usage.Event {
		return usage.Event{
			SchemaVersion: usage.SchemaVersion,
			EventID:       id,
			Timestamp:     timestamp,
			MachineID:     "machine-1",
			Provider:      "provider",
			Model:         "model",
			Tool:          "tool",
			TotalTokens:   total,
			TokenAccuracy: usage.AccuracyReported,
			Source:        usage.Source{Adapter: "test", AdapterVersion: "1"},
		}
	}
	events := []usage.Event{
		base("activity-known-1", time.Date(2026, 7, 11, 18, 0, 0, 0, time.UTC), usage.Int64(120)),
		base("activity-unknown", time.Date(2026, 7, 11, 19, 0, 0, 0, time.UTC), nil),
		base("activity-known-2", time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC), usage.Int64(60)),
		base("activity-outside-window", time.Date(2025, 7, 1, 12, 0, 0, 0, time.UTC), usage.Int64(999)),
	}
	if result, err := store.Ingest(context.Background(), events); err != nil || result.Accepted != len(events) {
		t.Fatalf("unexpected ingest result: %+v, error: %v", result, err)
	}

	activity, err := store.Activity(context.Background(), time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if activity.StartDate != "2025-07-13" || activity.EndDate != "2026-07-12" {
		t.Fatalf("unexpected activity range: %+v", activity)
	}
	if len(activity.Weeks) != 53 || activity.TotalTokens != 180 || activity.ActiveDays != 2 || activity.MaxDailyTokens != 120 {
		t.Fatalf("unexpected activity summary: %+v", activity)
	}

	findDay := func(want string) (ActivityDay, bool) {
		for _, week := range activity.Weeks {
			for _, day := range week.Days {
				if day.Date == want {
					return day, true
				}
			}
		}
		return ActivityDay{}, false
	}
	known, ok := findDay("2026-07-11")
	if !ok || known.Tokens != 120 || known.Events != 2 || known.UnknownTokens != 1 || known.Level != 4 || known.Future {
		t.Fatalf("unexpected known day: %+v", known)
	}
	if len(known.ByModel) != 1 || known.ByModel[0].Name != "model" || known.ByModel[0].Tokens != 120 || known.ByModel[0].UnknownTokens != 1 {
		t.Fatalf("unexpected model breakdown: %+v", known.ByModel)
	}
	if len(known.ByProvider) != 1 || known.ByProvider[0].Name != "provider" || known.ByProvider[0].Tokens != 120 || known.ByProvider[0].UnknownTokens != 1 {
		t.Fatalf("unexpected provider breakdown: %+v", known.ByProvider)
	}
	quiet, ok := findDay("2026-07-09")
	if !ok || quiet.Events != 0 || quiet.Level != 0 || quiet.Future {
		t.Fatalf("unexpected quiet day: %+v", quiet)
	}
	future, ok := findDay("2026-07-13")
	if !ok || !future.Future {
		t.Fatalf("expected future cells after the current day: %+v", future)
	}
}
