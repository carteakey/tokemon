package database

import (
	"context"
	"testing"
	"time"

	"github.com/tokemon/tokemon/internal/catalog"
	"github.com/tokemon/tokemon/internal/usage"
)

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
