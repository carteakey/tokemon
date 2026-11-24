package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/tokemon/tokemon/internal/catalog"
	"github.com/tokemon/tokemon/internal/usage"
)

func testTokedexCatalog(t *testing.T) *catalog.Catalog {
	t.Helper()
	return catalog.MustNew(map[string]catalog.Model{
		"alpha": {
			Provider:    "openai",
			DisplayName: "Alpha Model",
			Aliases:     []string{"alpha", "provider/alpha", "legacy-alpha"},
			Tier:        "S",
			Pricing: catalog.Pricing{
				Currency:                  "USD",
				Mode:                      "standard",
				VerifiedAt:                "2026-11-01",
				Source:                    "https://openai.com/api/pricing",
				InputPricePerMillion:      usage.Float64(2.00),
				OutputPricePerMillion:     usage.Float64(10.00),
				CacheReadPricePerMillion:  usage.Float64(0.50),
				CacheWritePricePerMillion: usage.Float64(2.50),
			},
		},
		"smoke-zero-usage": {
			Provider:    "anthropic",
			DisplayName: "Zero Usage Model",
			Aliases:     []string{"smoke-zero-usage"},
			Tier:        "A",
			Pricing: catalog.Pricing{
				Currency:              "USD",
				Mode:                  "standard",
				VerifiedAt:            "2026-11-01",
				Source:                "https://anthropic.com/pricing",
				InputPricePerMillion:  usage.Float64(3.00),
				OutputPricePerMillion: usage.Float64(15.00),
			},
		},
		"unknown-price-model": {
			Provider:    "custom",
			DisplayName: "Unknown Price Model",
			Aliases:     []string{"unknown-price-model"},
			Tier:        "C",
			Pricing: catalog.Pricing{
				Currency:   "USD",
				Mode:       "unknown",
				VerifiedAt: "2026-11-01",
			},
		},
		"inverted-cache-model": {
			Provider:    "custom",
			DisplayName: "Inverted Cache Model",
			Aliases:     []string{"inverted-cache-model"},
			Tier:        "B",
			Pricing: catalog.Pricing{
				Currency:                 "USD",
				Mode:                     "standard",
				VerifiedAt:               "2026-11-01",
				Source:                   "https://example.com/pricing",
				InputPricePerMillion:     usage.Float64(1.00),
				OutputPricePerMillion:    usage.Float64(4.00),
				CacheReadPricePerMillion: usage.Float64(3.00),
			},
		},
	})
}

func TestTokedexModelDetailCanonicalAndAliasResolution(t *testing.T) {
	cat := testTokedexCatalog(t)
	store, err := OpenWithLocation(filepath.Join(t.TempDir(), "tokedex.db"), cat, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ctx := context.Background()
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

	if _, err := store.Ingest(ctx, []usage.Event{
		{
			SchemaVersion: usage.SchemaVersion, EventID: "td-canon", Timestamp: now.Add(-2 * time.Hour),
			MachineID: "m1", SessionID: "s1", Provider: "openai", Model: "alpha", Tool: "codex",
			InputTokens: usage.Int64(1_000_000), CacheReadTokens: usage.Int64(1_000_000), OutputTokens: usage.Int64(100_000),
			TotalTokens: usage.Int64(2_100_000), TokenAccuracy: usage.AccuracyReported,
			Source: usage.Source{Adapter: "codex", AdapterVersion: "1"},
		},
		{
			SchemaVersion: usage.SchemaVersion, EventID: "td-alias", Timestamp: now.Add(-time.Hour),
			MachineID: "m2", SessionID: "s2", Provider: "openai", Model: "provider/alpha", Tool: "codex",
			InputTokens: usage.Int64(500_000), CacheReadTokens: usage.Int64(500_000), OutputTokens: usage.Int64(50_000),
			TotalTokens: usage.Int64(1_050_000), TokenAccuracy: usage.AccuracyReported,
			Source: usage.Source{Adapter: "codex", AdapterVersion: "1"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	// Legacy un-normalized row (canonical_model = '' and raw_model = 'legacy-alpha').
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO usage_events (
  event_id, timestamp, machine_id, project, provider, raw_model, canonical_model, tool,
  input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, total_tokens, cost,
  session_id, token_accuracy, adapter, adapter_version
) VALUES ('td-legacy', ?, 'm1', 'proj', 'openai', 'legacy-alpha', '', 'codex', 500000, 50000, 500000, 0, 1050000, 1.75, 's3', 'reported', 'codex', '1')`,
		now.Add(-30*time.Minute).Format(time.RFC3339),
	); err != nil {
		t.Fatal(err)
	}

	canon, err := store.TokedexModelDetailAt(ctx, "alpha", now)
	if err != nil {
		t.Fatal(err)
	}
	if canon.ID != "alpha" || canon.RequestedID != "" || !canon.Cataloged || !canon.Priced {
		t.Fatalf("canon identity = %+v", canon)
	}
	if canon.Events != 3 || canon.Sessions != 3 || canon.Tokens != 4_200_000 {
		t.Fatalf("canon totals = events:%d sessions:%d tokens:%d", canon.Events, canon.Sessions, canon.Tokens)
	}
	// Cost: row1 (2.00 + 0.50 + 1.00 = 3.50) + row2 (1.00 + 0.25 + 0.50 = 1.75) + row3 (1.75) = 7.00.
	// Savings: 2M total cache_read * (2.00 - 0.50) = 3.00.
	// Effective $/1M: 7.00 / 4.2 = 1.666666...
	if !canon.HasCost || math.Abs(canon.EstimatedCost.Amount-7.0) > 1e-9 || canon.EstimatedCost.PricedTokens != 4_200_000 {
		t.Fatalf("canon cost = %+v (has=%v)", canon.EstimatedCost, canon.HasCost)
	}
	if !canon.HasCacheSavings || math.Abs(canon.CacheSavings-3.0) > 1e-9 {
		t.Fatalf("canon CacheSavings = %v (has=%v), want 3.0", canon.CacheSavings, canon.HasCacheSavings)
	}
	if canon.EffectiveCostPerMillion == nil || math.Abs(*canon.EffectiveCostPerMillion-(7.0/4.2)) > 1e-9 {
		t.Fatalf("canon EffectiveCostPerMillion = %v", canon.EffectiveCostPerMillion)
	}
	if len(canon.Machines) != 2 || canon.Machines[0].Machine != "m1" || math.Abs(canon.Machines[0].Share-0.75) > 1e-9 {
		t.Fatalf("canon Machines = %+v", canon.Machines)
	}
	if len(canon.RecentSessions) != 3 {
		t.Fatalf("canon RecentSessions = %d, want 3", len(canon.RecentSessions))
	}

	alias, err := store.TokedexModelDetailAt(ctx, "provider/alpha", now)
	if err != nil {
		t.Fatal(err)
	}
	if alias.ID != "alpha" || alias.RequestedID != "provider/alpha" || alias.Tokens != canon.Tokens || alias.Events != canon.Events {
		t.Fatalf("alias resolution mismatch: %+v", alias)
	}
}

func TestTokedexModelDetailZeroUsageUncatalogedAndNotFound(t *testing.T) {
	cat := testTokedexCatalog(t)
	store, err := OpenWithLocation(filepath.Join(t.TempDir(), "tokedex-states.db"), cat, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ctx := context.Background()
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	if _, err := store.Ingest(ctx, []usage.Event{{
		SchemaVersion: usage.SchemaVersion, EventID: "mys-1", Timestamp: now.Add(-time.Hour),
		MachineID: "m1", SessionID: "s1", Provider: "mystery", Model: "mystery-model", Tool: "generic",
		InputTokens: usage.Int64(100), CacheReadTokens: usage.Int64(100), OutputTokens: usage.Int64(50),
		TotalTokens: usage.Int64(250), TokenAccuracy: usage.AccuracyReported,
		Source: usage.Source{Adapter: "generic", AdapterVersion: "1"},
	}}); err != nil {
		t.Fatal(err)
	}

	zero, err := store.TokedexModelDetailAt(ctx, "smoke-zero-usage", now)
	if err != nil {
		t.Fatal(err)
	}
	if !zero.Cataloged || zero.Events != 0 || zero.Context != "No usage recorded yet" || len(zero.WeeklyTrend) != 0 || len(zero.RecentSessions) != 0 {
		t.Fatalf("zero-usage model = %+v", zero)
	}

	uncat, err := store.TokedexModelDetailAt(ctx, "mystery-model", now)
	if err != nil {
		t.Fatal(err)
	}
	if uncat.Cataloged || uncat.Priced || uncat.Tier != "Uncataloged" || uncat.Pricing.Mode != "unknown" || uncat.HasCost || uncat.Provider != "mystery" || len(uncat.Machines) != 1 || len(uncat.WeeklyTrend) != tokedexWeeklyTrendWeeks || len(uncat.RecentSessions) != 1 {
		t.Fatalf("uncataloged model = %+v", uncat)
	}

	for _, missing := range []string{"", "   ", "does-not-exist"} {
		_, err := store.TokedexModelDetailAt(ctx, missing, now)
		if !errors.Is(err, ErrModelNotFound) || !errors.Is(err, sql.ErrNoRows) {
			t.Fatalf("TokedexModelDetail(%q) err = %v, want ErrModelNotFound / sql.ErrNoRows", missing, err)
		}
	}
}

func TestTokedexModelDetailUnknownPricingAndEstimatedFallback(t *testing.T) {
	cat := testTokedexCatalog(t)
	store, err := OpenWithLocation(filepath.Join(t.TempDir(), "tokedex-fallback.db"), cat, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ctx := context.Background()
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)

	if _, err := store.Ingest(ctx, []usage.Event{
		{
			SchemaVersion: usage.SchemaVersion, EventID: "unk-m-1", Timestamp: now.Add(-time.Hour),
			MachineID: "m1", SessionID: "s1", Provider: "custom", Model: "unknown-price-model", Tool: "generic",
			InputTokens: usage.Int64(100), CacheReadTokens: usage.Int64(100), TotalTokens: usage.Int64(200),
			TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "generic", AdapterVersion: "1"},
		},
		{
			SchemaVersion: usage.SchemaVersion, EventID: "inv-m-1", Timestamp: now.Add(-time.Hour),
			MachineID: "m1", SessionID: "s2", Provider: "custom", Model: "inverted-cache-model", Tool: "generic",
			InputTokens: usage.Int64(500_000), CacheReadTokens: usage.Int64(500_000), TotalTokens: usage.Int64(1_000_000),
			TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "generic", AdapterVersion: "1"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	// Legacy NULL cost row with components (falls back to catalog.Estimate) AND aggregate-only row (stays UnpricedTokens).
	if _, err := store.db.ExecContext(ctx, `
INSERT INTO usage_events (event_id, timestamp, machine_id, project, provider, raw_model, canonical_model, tool, input_tokens, output_tokens, cache_read_tokens, total_tokens, cost, session_id, token_accuracy, adapter, adapter_version)
VALUES ('alpha-legacy-comp', ?, 'm1', '', 'openai', 'alpha', 'alpha', 'codex', 1000000, 0, 1000000, 2000000, NULL, 's3', 'reported', 'codex', '1'),
       ('alpha-legacy-agg', ?, 'm1', '', 'openai', 'alpha', 'alpha', 'codex', NULL, NULL, NULL, 500000, NULL, 's4', 'estimated', 'codex', '1')`,
		now.Add(-2*time.Hour).Format(time.RFC3339), now.Add(-time.Hour).Format(time.RFC3339),
	); err != nil {
		t.Fatal(err)
	}

	unk, err := store.TokedexModelDetailAt(ctx, "unknown-price-model", now)
	if err != nil {
		t.Fatal(err)
	}
	if unk.Priced || unk.HasCost || unk.EstimatedCost.UnpricedTokens != 200 || unk.HasCacheSavings || unk.EffectiveCostPerMillion != nil {
		t.Fatalf("unknown-price-model = %+v", unk)
	}

	inv, err := store.TokedexModelDetailAt(ctx, "inverted-cache-model", now)
	if err != nil {
		t.Fatal(err)
	}
	if !inv.HasCacheSavings || inv.CacheSavings != 0 {
		t.Fatalf("inverted cache savings = %v (has=%v), want 0", inv.CacheSavings, inv.HasCacheSavings)
	}

	alpha, err := store.TokedexModelDetailAt(ctx, "alpha", now)
	if err != nil {
		t.Fatal(err)
	}
	// Component legacy row: 1M*2.00 + 1M*0.50 = 2.50, savings 1.50; aggregate row: 500k unpriced.
	if !alpha.HasCost || math.Abs(alpha.EstimatedCost.Amount-2.50) > 1e-9 || alpha.EstimatedCost.PricedTokens != 2_000_000 || alpha.EstimatedCost.UnpricedTokens != 500_000 {
		t.Fatalf("alpha fallback cost = %+v", alpha.EstimatedCost)
	}
	if !alpha.HasCacheSavings || math.Abs(alpha.CacheSavings-1.50) > 1e-9 {
		t.Fatalf("alpha fallback savings = %v (has=%v), want 1.50", alpha.CacheSavings, alpha.HasCacheSavings)
	}
}

func TestTokedexModelDetailWeeklyTrendTimezoneAndDST(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	cat := testTokedexCatalog(t)
	store, err := OpenWithLocation(filepath.Join(t.TempDir(), "trend.db"), cat, ny)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Reference Wednesday Nov 11 2026 NY -> current week Sunday Nov 8 .. Nov 14.
	// Nov 1 2026 is fall-back DST in NY (Saturday Oct 31 23:30 EDT = Nov 1 03:30Z -> week Oct 25;
	// Sunday Nov 1 00:30 EDT = Nov 1 04:30Z -> week Nov 1).
	now := time.Date(2026, 11, 11, 12, 0, 0, 0, ny)
	ctx := context.Background()
	if _, err := store.Ingest(ctx, []usage.Event{
		{
			// 13 weeks ago -> excluded from 12-week trend, included in lifetime Tokens.
			SchemaVersion: usage.SchemaVersion, EventID: "w-old", Timestamp: time.Date(2026, 8, 10, 12, 0, 0, 0, ny),
			MachineID: "m1", SessionID: "s1", Provider: "openai", Model: "alpha", Tool: "codex",
			TotalTokens: usage.Int64(900), TokenAccuracy: usage.AccuracyReported,
			Source: usage.Source{Adapter: "codex", AdapterVersion: "1"},
		},
		{
			// Sat Oct 31 23:30 EDT -> week starting 2026-10-25.
			SchemaVersion: usage.SchemaVersion, EventID: "w-sat", Timestamp: time.Date(2026, 11, 1, 3, 30, 0, 0, time.UTC),
			MachineID: "m1", SessionID: "s2", Provider: "openai", Model: "alpha", Tool: "codex",
			TotalTokens: usage.Int64(100), TokenAccuracy: usage.AccuracyReported,
			Source: usage.Source{Adapter: "codex", AdapterVersion: "1"},
		},
		{
			// Sun Nov 1 00:30 EDT -> week starting 2026-11-01.
			SchemaVersion: usage.SchemaVersion, EventID: "w-sun", Timestamp: time.Date(2026, 11, 1, 4, 30, 0, 0, time.UTC),
			MachineID: "m1", SessionID: "s3", Provider: "openai", Model: "alpha", Tool: "codex",
			TotalTokens: usage.Int64(250), TokenAccuracy: usage.AccuracyReported,
			Source: usage.Source{Adapter: "codex", AdapterVersion: "1"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	detail, err := store.TokedexModelDetailAt(ctx, "alpha", now)
	if err != nil {
		t.Fatal(err)
	}
	if detail.Tokens != 1250 || len(detail.WeeklyTrend) != 12 {
		t.Fatalf("detail Tokens=%d len(WeeklyTrend)=%d", detail.Tokens, len(detail.WeeklyTrend))
	}
	byWeek := map[string]TokedexWeeklyPoint{}
	for _, pt := range detail.WeeklyTrend {
		byWeek[pt.WeekStart] = pt
	}
	if byWeek["2026-10-25"].Tokens != 100 || byWeek["2026-11-01"].Tokens != 250 || byWeek["2026-11-08"].Tokens != 0 {
		t.Fatalf("weekly buckets mismatch: oct25=%+v nov1=%+v nov8=%+v", byWeek["2026-10-25"], byWeek["2026-11-01"], byWeek["2026-11-08"])
	}
}

func TestTokedexModelDetailPrivacyAndSessionLimit(t *testing.T) {
	cat := testTokedexCatalog(t)
	store, err := OpenWithLocation(filepath.Join(t.TempDir(), "sessions.db"), cat, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ctx := context.Background()
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	events := make([]usage.Event, 0, 16)
	for i := 0; i < 15; i++ {
		events = append(events, usage.Event{
			SchemaVersion: usage.SchemaVersion, EventID: fmt.Sprintf("sess-ev-%02d", i),
			Timestamp: now.Add(time.Duration(i) * time.Minute),
			MachineID: "m1", Project: "/Users/alice/secret/repo", SessionID: fmt.Sprintf("sess-%02d", i),
			Provider: "openai", Model: "alpha", Tool: "codex",
			TotalTokens: usage.Int64(100 + int64(i)), TokenAccuracy: usage.AccuracyReported,
			Source: usage.Source{Adapter: "codex", AdapterVersion: "1"},
		})
	}
	// Empty session_id row must not appear in RecentSessions.
	events = append(events, usage.Event{
		SchemaVersion: usage.SchemaVersion, EventID: "sess-empty", Timestamp: now.Add(30 * time.Minute),
		MachineID: "m1", SessionID: "", Provider: "openai", Model: "alpha", Tool: "codex",
		TotalTokens: usage.Int64(50), TokenAccuracy: usage.AccuracyReported,
		Source: usage.Source{Adapter: "codex", AdapterVersion: "1"},
	})
	if _, err := store.Ingest(ctx, events); err != nil {
		t.Fatal(err)
	}

	detail, err := store.TokedexModelDetailAt(ctx, "alpha", now.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	if len(detail.RecentSessions) != 12 {
		t.Fatalf("len(RecentSessions) = %d, want 12", len(detail.RecentSessions))
	}
	if detail.RecentSessions[0].SessionID != "sess-14" || detail.RecentSessions[11].SessionID != "sess-03" {
		t.Fatalf("RecentSessions order = first:%q last:%q", detail.RecentSessions[0].SessionID, detail.RecentSessions[11].SessionID)
	}
	if detail.RecentSessions[0].Project != "repo" {
		t.Fatalf("expected normalized project basename 'repo', got %q", detail.RecentSessions[0].Project)
	}
}

func TestCatalogSnapshot30dComponentTotalsAndNullHandling(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	cat := testTokedexCatalog(t)
	store, err := OpenWithLocation(filepath.Join(t.TempDir(), "snap30d.db"), cat, ny)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Date(2026, 11, 20, 12, 0, 0, 0, ny)
	ctx := context.Background()
	if _, err := store.Ingest(ctx, []usage.Event{
		{
			// Outside 30d window (today-30d = 2026-10-21) -> excluded.
			SchemaVersion: usage.SchemaVersion, EventID: "c30-old", Timestamp: time.Date(2026, 10, 21, 23, 0, 0, 0, ny),
			MachineID: "m1", Provider: "openai", Model: "alpha", Tool: "codex",
			InputTokens: usage.Int64(999), OutputTokens: usage.Int64(999), TotalTokens: usage.Int64(1998),
			TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "codex", AdapterVersion: "1"},
		},
		{
			// Inside 30d window (today-29d = 2026-10-22 00:30 EDT) -> included.
			SchemaVersion: usage.SchemaVersion, EventID: "c30-comp", Timestamp: time.Date(2026, 10, 22, 0, 30, 0, 0, ny),
			MachineID: "m1", Provider: "openai", Model: "alpha", Tool: "codex",
			InputTokens: usage.Int64(100), CacheReadTokens: usage.Int64(200), CacheWriteTokens: usage.Int64(50),
			OutputTokens: usage.Int64(25), ReasoningTokens: usage.Int64(10), TotalTokens: usage.Int64(385),
			TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "codex", AdapterVersion: "1"},
		},
		{
			// Aggregate-only row (NULL components, non-null total) -> counted in UnclassifiedTokens & TotalTokens, NOT ComponentEvents.
			SchemaVersion: usage.SchemaVersion, EventID: "c30-agg", Timestamp: now.Add(-time.Hour),
			MachineID: "m1", Provider: "custom", Model: "unknown-price-model", Tool: "generic",
			TotalTokens: usage.Int64(400), TokenAccuracy: usage.AccuracyEstimated,
			Source: usage.Source{Adapter: "generic", AdapterVersion: "1"},
		},
		{
			// Unknown total_tokens row -> counted in UnknownEvents.
			SchemaVersion: usage.SchemaVersion, EventID: "c30-unk", Timestamp: now.Add(-30 * time.Minute),
			MachineID: "m1", Provider: "custom", Model: "uncataloged-z", Tool: "generic",
			TokenAccuracy: usage.AccuracyUnknown, Source: usage.Source{Adapter: "generic", AdapterVersion: "1"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	got, err := store.componentUsageTotals30dAt(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if got.StartDate != "2026-10-22" || got.EndDate != "2026-11-20" {
		t.Fatalf("dates = %s .. %s", got.StartDate, got.EndDate)
	}
	if got.Events != 3 || got.UnknownEvents != 1 || got.ComponentEvents != 1 {
		t.Fatalf("event counts = %+v", got)
	}
	if got.InputTokens != 100 || got.CacheReadTokens != 200 || got.CacheWriteTokens != 50 ||
		got.OutputTokens != 25 || got.ReasoningTokens != 10 || got.TotalTokens != 785 || got.UnclassifiedTokens != 410 {
		t.Fatalf("component totals = %+v", got)
	}

	snap, err := store.catalogSnapshotAt(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Recent30d != got {
		t.Fatalf("snap.Recent30d = %+v != %+v", snap.Recent30d, got)
	}
	if snap.PricedTokens != 2383 || snap.CatalogedUnknownTokens != 400 || snap.UncatalogedModels != 1 || len(snap.Uncataloged) != 1 {
		t.Fatalf("CatalogSnapshot accounting = priced:%d unkCat:%d uncatModels:%d uncat:%+v",
			snap.PricedTokens, snap.CatalogedUnknownTokens, snap.UncatalogedModels, snap.Uncataloged)
	}
}

func TestTokedexConcurrentReadsWithIngestRace(t *testing.T) {
	cat := testTokedexCatalog(t)
	store, err := OpenWithLocation(filepath.Join(t.TempDir(), "tokedex-race.db"), cat, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ctx := context.Background()
	now := time.Now().UTC()
	var wg sync.WaitGroup
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				_, _ = store.Ingest(ctx, []usage.Event{{
					SchemaVersion: usage.SchemaVersion,
					EventID:       fmt.Sprintf("tr-%d-%d", worker, j),
					Timestamp:     now.Add(-time.Duration(j) * time.Minute),
					MachineID:     "m1", SessionID: fmt.Sprintf("s-%d", worker),
					Provider: "openai", Model: "alpha", Tool: "codex",
					InputTokens: usage.Int64(100), CacheReadTokens: usage.Int64(50), OutputTokens: usage.Int64(25),
					TotalTokens: usage.Int64(175), TokenAccuracy: usage.AccuracyReported,
					Source: usage.Source{Adapter: "codex", AdapterVersion: "1"},
				}})
			}
		}(i)
	}
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < 10; j++ {
				_, _ = store.CatalogSnapshot(ctx)
				_, _ = store.ComponentUsageTotals30d(ctx)
				_, _ = store.TokedexModelDetail(ctx, "alpha")
			}
		}()
	}
	wg.Wait()
}
