package database

import (
	"context"
	"math"
	"path/filepath"
	"testing"
	"time"

	"github.com/tokemon/tokemon/internal/catalog"
	"github.com/tokemon/tokemon/internal/usage"
)

func testSavingsCatalog(t *testing.T) *catalog.Catalog {
	t.Helper()
	return catalog.MustNew(map[string]catalog.Model{
		"gpt-5.4": {
			Provider:    "openai",
			DisplayName: "GPT-5.4",
			Aliases:     []string{"gpt-5.4"},
			Tier:        "S",
			Pricing: catalog.Pricing{
				Currency:                  "USD",
				Mode:                      "standard",
				VerifiedAt:                "2026-11-01",
				Source:                    "https://openai.com/api/pricing",
				InputPricePerMillion:      usage.Float64(2.50),
				OutputPricePerMillion:     usage.Float64(15.00),
				CacheReadPricePerMillion:  usage.Float64(0.25),
				CacheWritePricePerMillion: usage.Float64(2.50),
			},
		},
		"claude-sonnet-4": {
			Provider:    "anthropic",
			DisplayName: "Claude Sonnet 4",
			Aliases:     []string{"claude-sonnet-4"},
			Tier:        "S",
			Pricing: catalog.Pricing{
				Currency:                  "USD",
				Mode:                      "standard",
				VerifiedAt:                "2026-11-01",
				Source:                    "https://anthropic.com/pricing",
				InputPricePerMillion:      usage.Float64(3.00),
				OutputPricePerMillion:     usage.Float64(15.00),
				CacheReadPricePerMillion:  usage.Float64(0.30),
				CacheWritePricePerMillion: usage.Float64(3.75),
			},
		},
		"mistral-no-cache": {
			Provider:    "mistral",
			DisplayName: "Mistral No Cache Rate",
			Aliases:     []string{"mistral-no-cache"},
			Tier:        "A",
			Pricing: catalog.Pricing{
				Currency:              "USD",
				Mode:                  "standard",
				VerifiedAt:            "2026-11-01",
				Source:                "https://mistral.ai/pricing",
				InputPricePerMillion:  usage.Float64(2.00),
				OutputPricePerMillion: usage.Float64(6.00),
			},
		},
		"inverted-cache": {
			Provider:    "custom",
			DisplayName: "Inverted Cache",
			Aliases:     []string{"inverted-cache"},
			Tier:        "B",
			Pricing: catalog.Pricing{
				Currency:                 "USD",
				Mode:                     "standard",
				VerifiedAt:               "2026-11-01",
				Source:                   "https://example.com/pricing",
				InputPricePerMillion:     usage.Float64(1.00),
				OutputPricePerMillion:    usage.Float64(2.00),
				CacheReadPricePerMillion: usage.Float64(5.00),
			},
		},
		"unknown-model": {
			Provider:    "anthropic",
			DisplayName: "Unknown Model",
			Aliases:     []string{"unknown-model"},
			Tier:        "B",
			Pricing: catalog.Pricing{
				Currency:   "USD",
				Mode:       "unknown",
				VerifiedAt: "2026-11-01",
			},
		},
	})
}

func TestAnalyticsCacheSavingsAndEffectiveRate(t *testing.T) {
	cat := testSavingsCatalog(t)
	store, err := OpenWithLocation(filepath.Join(t.TempDir(), "savings.db"), cat, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	if _, err := store.Ingest(ctx, []usage.Event{
		{
			SchemaVersion: usage.SchemaVersion, EventID: "sav-1", Timestamp: now.Add(-time.Hour),
			MachineID: "m1", SessionID: "s1", Provider: "openai", Model: "gpt-5.4", Tool: "codex",
			InputTokens: usage.Int64(1_000_000), CacheReadTokens: usage.Int64(2_000_000), OutputTokens: usage.Int64(200_000),
			TotalTokens: usage.Int64(3_200_000), TokenAccuracy: usage.AccuracyReported,
			Source: usage.Source{Adapter: "codex", AdapterVersion: "1"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	res, err := store.Analytics(ctx, AnalyticsQuery{Period: "30d", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if !res.Summary.HasCacheSavings || math.Abs(res.Summary.CacheSavings-4.5) > 1e-9 {
		t.Fatalf("Summary.CacheSavings = %v (has=%v), want 4.5", res.Summary.CacheSavings, res.Summary.HasCacheSavings)
	}
	if res.Summary.CacheSavingsPricedTokens != 3_200_000 {
		t.Fatalf("CacheSavingsPricedTokens = %d, want 3200000", res.Summary.CacheSavingsPricedTokens)
	}
	if math.Abs(res.Summary.EstimatedCost.Amount-6.0) > 1e-9 {
		t.Fatalf("EstimatedCost.Amount = %v, want 6.0", res.Summary.EstimatedCost.Amount)
	}
	rate, ok := res.Summary.EffectiveRatePerMillion()
	if !ok || !res.Summary.HasEffectiveCost || math.Abs(rate-1.875) > 1e-9 || math.Abs(res.Summary.EffectiveCostPerMillion-1.875) > 1e-9 {
		t.Fatalf("EffectiveRate = %v (ok=%v field=%v has=%v), want 1.875", rate, ok, res.Summary.EffectiveCostPerMillion, res.Summary.HasEffectiveCost)
	}
}

func TestAnalyticsMixedPricedAndUnpricedSavingsAndEffectiveRate(t *testing.T) {
	cat := testSavingsCatalog(t)
	store, err := OpenWithLocation(filepath.Join(t.TempDir(), "mixed.db"), cat, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	if _, err := store.Ingest(ctx, []usage.Event{
		{
			// gpt-5.4: 1M*2.50 + 2M*0.25 + 100k*15 = $4.50 cost, $4.50 saved, 3.1M priced tokens.
			SchemaVersion: usage.SchemaVersion, EventID: "mix-1", Timestamp: now.Add(-2 * time.Hour),
			MachineID: "m1", SessionID: "s1", Provider: "openai", Model: "gpt-5.4", Tool: "codex",
			InputTokens: usage.Int64(1_000_000), CacheReadTokens: usage.Int64(2_000_000), OutputTokens: usage.Int64(100_000),
			TotalTokens: usage.Int64(3_100_000), TokenAccuracy: usage.AccuracyReported,
			Source: usage.Source{Adapter: "codex", AdapterVersion: "1"},
		},
		{
			// mode: unknown -> 2M unpriced tokens, 0 savings.
			SchemaVersion: usage.SchemaVersion, EventID: "mix-2", Timestamp: now.Add(-time.Hour),
			MachineID: "m1", SessionID: "s2", Provider: "anthropic", Model: "unknown-model", Tool: "claude-code",
			InputTokens: usage.Int64(1_000_000), CacheReadTokens: usage.Int64(1_000_000), OutputTokens: usage.Int64(0),
			TotalTokens: usage.Int64(2_000_000), TokenAccuracy: usage.AccuracyReported,
			Source: usage.Source{Adapter: "claude-code", AdapterVersion: "1"},
		},
		{
			// uncataloged -> 1M unpriced tokens, 0 savings.
			SchemaVersion: usage.SchemaVersion, EventID: "mix-3", Timestamp: now.Add(-30 * time.Minute),
			MachineID: "m1", SessionID: "s3", Provider: "custom", Model: "brand-new-model", Tool: "generic",
			InputTokens: usage.Int64(500_000), CacheReadTokens: usage.Int64(500_000), OutputTokens: usage.Int64(0),
			TotalTokens: usage.Int64(1_000_000), TokenAccuracy: usage.AccuracyReported,
			Source: usage.Source{Adapter: "generic", AdapterVersion: "1"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	res, err := store.Analytics(ctx, AnalyticsQuery{Period: "30d", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if res.Summary.EstimatedCost.PricedTokens != 3_100_000 || res.Summary.EstimatedCost.UnpricedTokens != 3_000_000 {
		t.Fatalf("EstimatedCost = %+v", res.Summary.EstimatedCost)
	}
	wantEff := 4.50 * 1_000_000 / 3_100_000
	if !res.Summary.HasEffectiveCost || math.Abs(res.Summary.EffectiveCostPerMillion-wantEff) > 1e-9 {
		t.Fatalf("EffectiveCostPerMillion = %v (has=%v), want %v", res.Summary.EffectiveCostPerMillion, res.Summary.HasEffectiveCost, wantEff)
	}
	if !res.Summary.HasCacheSavings || math.Abs(res.Summary.CacheSavings-4.50) > 1e-9 {
		t.Fatalf("CacheSavings = %v (has=%v), want 4.50", res.Summary.CacheSavings, res.Summary.HasCacheSavings)
	}
}

func TestAnalyticsUnknownOrMissingCacheRateStaysUnknown(t *testing.T) {
	cat := testSavingsCatalog(t)
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()

	t.Run("unknown-mode-and-uncataloged", func(t *testing.T) {
		store, err := OpenWithLocation(filepath.Join(t.TempDir(), "unk.db"), cat, time.UTC)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()

		if _, err := store.Ingest(ctx, []usage.Event{
			{
				SchemaVersion: usage.SchemaVersion, EventID: "u-1", Timestamp: now.Add(-time.Hour),
				MachineID: "m1", SessionID: "s1", Provider: "anthropic", Model: "unknown-model", Tool: "claude-code",
				InputTokens: usage.Int64(1_000_000), CacheReadTokens: usage.Int64(1_000_000),
				TotalTokens: usage.Int64(2_000_000), TokenAccuracy: usage.AccuracyReported,
				Source: usage.Source{Adapter: "claude-code", AdapterVersion: "1"},
			},
			{
				SchemaVersion: usage.SchemaVersion, EventID: "u-2", Timestamp: now.Add(-30 * time.Minute),
				MachineID: "m1", SessionID: "s2", Provider: "custom", Model: "uncataloged-7b", Tool: "generic",
				InputTokens: usage.Int64(1_000_000), CacheReadTokens: usage.Int64(1_000_000),
				TotalTokens: usage.Int64(2_000_000), TokenAccuracy: usage.AccuracyReported,
				Source: usage.Source{Adapter: "generic", AdapterVersion: "1"},
			},
		}); err != nil {
			t.Fatal(err)
		}

		res, err := store.Analytics(ctx, AnalyticsQuery{Period: "30d", Now: now})
		if err != nil {
			t.Fatal(err)
		}
		if res.Summary.HasCacheSavings || res.Summary.HasEffectiveCost {
			t.Fatalf("expected unknown savings and effective cost, got %+v", res.Summary)
		}
		if _, ok := res.Summary.EffectiveRatePerMillion(); ok {
			t.Fatal("EffectiveRatePerMillion should return false when unpriced")
		}
		if math.Abs(res.Summary.Cache.HitRate-0.5) > 1e-9 {
			t.Fatalf("Cache.HitRate = %v, want 0.5", res.Summary.Cache.HitRate)
		}
	})

	t.Run("missing-cache-read-rate-with-and-without-cache-tokens", func(t *testing.T) {
		store, err := OpenWithLocation(filepath.Join(t.TempDir(), "nocache.db"), cat, time.UTC)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()

		// With CacheReadTokens > 0 on model lacking CacheReadPricePerMillion: cost is NULL, savings omitted.
		if _, err := store.Ingest(ctx, []usage.Event{{
			SchemaVersion: usage.SchemaVersion, EventID: "nc-1", Timestamp: now.Add(-time.Hour),
			MachineID: "m1", SessionID: "s1", Provider: "mistral", Model: "mistral-no-cache", Tool: "generic",
			InputTokens: usage.Int64(500_000), CacheReadTokens: usage.Int64(500_000), OutputTokens: usage.Int64(0),
			TotalTokens: usage.Int64(1_000_000), TokenAccuracy: usage.AccuracyReported,
			Source: usage.Source{Adapter: "generic", AdapterVersion: "1"},
		}}); err != nil {
			t.Fatal(err)
		}
		res1, err := store.Analytics(ctx, AnalyticsQuery{Period: "30d", Now: now})
		if err != nil {
			t.Fatal(err)
		}
		if res1.Summary.HasCacheSavings || res1.Summary.HasEffectiveCost || res1.Summary.EstimatedCost.PricedTokens != 0 {
			t.Fatalf("expected unpriced/no-savings when cache rate missing and CacheReadTokens>0: %+v", res1.Summary)
		}

		// With CacheReadTokens == 0: cost is priced ($4.00/1M), but HasCacheSavings remains false!
		store2, err := OpenWithLocation(filepath.Join(t.TempDir(), "nocache2.db"), cat, time.UTC)
		if err != nil {
			t.Fatal(err)
		}
		defer store2.Close()
		if _, err := store2.Ingest(ctx, []usage.Event{{
			SchemaVersion: usage.SchemaVersion, EventID: "nc-2", Timestamp: now.Add(-time.Hour),
			MachineID: "m1", SessionID: "s1", Provider: "mistral", Model: "mistral-no-cache", Tool: "generic",
			InputTokens: usage.Int64(500_000), CacheReadTokens: usage.Int64(0), OutputTokens: usage.Int64(500_000),
			TotalTokens: usage.Int64(1_000_000), TokenAccuracy: usage.AccuracyReported,
			Source: usage.Source{Adapter: "generic", AdapterVersion: "1"},
		}}); err != nil {
			t.Fatal(err)
		}
		res2, err := store2.Analytics(ctx, AnalyticsQuery{Period: "30d", Now: now})
		if err != nil {
			t.Fatal(err)
		}
		if !res2.Summary.HasEffectiveCost || math.Abs(res2.Summary.EffectiveCostPerMillion-4.0) > 1e-9 {
			t.Fatalf("expected EffectiveCostPerMillion=4.0, got %+v", res2.Summary)
		}
		if res2.Summary.HasCacheSavings {
			t.Fatalf("expected HasCacheSavings=false when CacheReadPricePerMillion is nil: %+v", res2.Summary)
		}
	})

	t.Run("inverted-cache-rate-clamped-non-negative", func(t *testing.T) {
		store, err := OpenWithLocation(filepath.Join(t.TempDir(), "inv.db"), cat, time.UTC)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()

		if _, err := store.Ingest(ctx, []usage.Event{{
			SchemaVersion: usage.SchemaVersion, EventID: "inv-1", Timestamp: now.Add(-time.Hour),
			MachineID: "m1", SessionID: "s1", Provider: "custom", Model: "inverted-cache", Tool: "generic",
			InputTokens: usage.Int64(500_000), CacheReadTokens: usage.Int64(500_000), OutputTokens: usage.Int64(0),
			TotalTokens: usage.Int64(1_000_000), TokenAccuracy: usage.AccuracyReported,
			Source: usage.Source{Adapter: "generic", AdapterVersion: "1"},
		}}); err != nil {
			t.Fatal(err)
		}
		res, err := store.Analytics(ctx, AnalyticsQuery{Period: "30d", Now: now})
		if err != nil {
			t.Fatal(err)
		}
		if !res.Summary.HasCacheSavings || res.Summary.CacheSavings != 0 {
			t.Fatalf("inverted cache savings = %v (has=%v), want 0 (clamped non-negative)", res.Summary.CacheSavings, res.Summary.HasCacheSavings)
		}
	})
}

func TestAnalyticsBreakdownPerRowCacheHitCostAndSavings(t *testing.T) {
	cat := testSavingsCatalog(t)
	store, err := OpenWithLocation(filepath.Join(t.TempDir(), "bd.db"), cat, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	if _, err := store.Ingest(ctx, []usage.Event{
		{
			// codex / gpt-5.4: 80% cache hit, $1.45 cost, $1.80 saved, $1.38/1M.
			SchemaVersion: usage.SchemaVersion, EventID: "bd-1", Timestamp: now.Add(-3 * time.Hour),
			MachineID: "m1", SessionID: "s1", Provider: "openai", Model: "gpt-5.4", Tool: "codex",
			InputTokens: usage.Int64(200_000), CacheReadTokens: usage.Int64(800_000), OutputTokens: usage.Int64(50_000),
			TotalTokens: usage.Int64(1_050_000), TokenAccuracy: usage.AccuracyReported,
			Source: usage.Source{Adapter: "codex", AdapterVersion: "1"},
		},
		{
			// claude-code / claude-sonnet-4: missing CacheReadTokens (ineligible for hit rate!), priced.
			SchemaVersion: usage.SchemaVersion, EventID: "bd-2", Timestamp: now.Add(-2 * time.Hour),
			MachineID: "m1", SessionID: "s2", Provider: "anthropic", Model: "claude-sonnet-4", Tool: "claude-code",
			InputTokens: usage.Int64(500_000), OutputTokens: usage.Int64(100_000),
			TotalTokens: usage.Int64(600_000), TokenAccuracy: usage.AccuracyReported,
			Source: usage.Source{Adapter: "claude-code", AdapterVersion: "1"},
		},
		{
			// generic / uncataloged: unpriced, 50% cache hit.
			SchemaVersion: usage.SchemaVersion, EventID: "bd-3", Timestamp: now.Add(-time.Hour),
			MachineID: "m1", SessionID: "s3", Provider: "custom", Model: "uncataloged-x", Tool: "generic",
			InputTokens: usage.Int64(100_000), CacheReadTokens: usage.Int64(100_000),
			TotalTokens: usage.Int64(200_000), TokenAccuracy: usage.AccuracyReported,
			Source: usage.Source{Adapter: "generic", AdapterVersion: "1"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	harnesses, err := store.Analytics(ctx, AnalyticsQuery{Period: "30d", Dimension: "harnesses", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	byHarness := map[string]AnalyticsBreakdown{}
	for _, row := range harnesses.Breakdown {
		byHarness[row.Name] = row
	}
	codex := byHarness["codex"]
	if !codex.HasCacheRate || math.Abs(codex.CacheHitRate-0.8) > 1e-9 || !codex.HasCost || math.Abs(codex.Cost-1.45) > 1e-9 || !codex.HasCacheSavings || math.Abs(codex.CacheSavings-1.80) > 1e-9 || !codex.HasEffectiveCost {
		t.Fatalf("codex breakdown = %+v", codex)
	}
	claude := byHarness["claude-code"]
	if claude.HasCacheRate || !claude.HasCost || !claude.HasEffectiveCost || claude.HasCacheSavings {
		t.Fatalf("claude-code breakdown (missing CacheReadTokens) = %+v", claude)
	}
	generic := byHarness["generic"]
	if !generic.HasCacheRate || math.Abs(generic.CacheHitRate-0.5) > 1e-9 || generic.HasCost || generic.HasEffectiveCost || generic.HasCacheSavings {
		t.Fatalf("generic breakdown (uncataloged) = %+v", generic)
	}

	models, err := store.Analytics(ctx, AnalyticsQuery{Period: "30d", Dimension: "models", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if len(models.Breakdown) != 3 || models.Breakdown[0].Name != "gpt-5.4" {
		t.Fatalf("models breakdown = %+v", models.Breakdown)
	}
}

func TestActivityStreaksAndPeakDay(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()
	makeEvent := func(id string, daysAgo int, tokens int64) usage.Event {
		return usage.Event{
			SchemaVersion: usage.SchemaVersion, EventID: id, Timestamp: now.AddDate(0, 0, -daysAgo),
			MachineID: "m1", Provider: "openai", Model: "gpt-5", Tool: "codex",
			TotalTokens: usage.Int64(tokens), TokenAccuracy: usage.AccuracyReported,
			Source: usage.Source{Adapter: "codex", AdapterVersion: "1"},
		}
	}

	t.Run("consecutive-ending-today-with-longer-earlier-streak", func(t *testing.T) {
		store, err := OpenWithLocation(filepath.Join(t.TempDir(), "s1.db"), catalog.Empty(), time.UTC)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()

		// Earlier 4-day streak (daysAgo 10,9,8,7), gap on day 6..3, recent 3-day streak ending today (2,1,0).
		events := []usage.Event{
			makeEvent("e10", 10, 100),
			makeEvent("e9", 9, 5000),
			makeEvent("e8", 8, 200),
			makeEvent("e7", 7, 300),
			makeEvent("e2", 2, 400),
			makeEvent("e1", 1, 500),
			makeEvent("e0", 0, 600),
		}
		if _, err := store.Ingest(ctx, events); err != nil {
			t.Fatal(err)
		}
		hm, err := store.Activity(ctx, now)
		if err != nil {
			t.Fatal(err)
		}
		if hm.CurrentStreak != 3 || hm.LongestStreak != 4 {
			t.Fatalf("streaks = current:%d longest:%d, want 3 / 4", hm.CurrentStreak, hm.LongestStreak)
		}
		if hm.PeakDay == nil || hm.PeakDay.Date != "2026-11-11" || hm.PeakDay.Tokens != 5000 {
			t.Fatalf("PeakDay = %+v, want 2026-11-11 with 5000 tokens", hm.PeakDay)
		}
	})

	t.Run("gap-ends-current-streak", func(t *testing.T) {
		store, err := OpenWithLocation(filepath.Join(t.TempDir(), "s2.db"), catalog.Empty(), time.UTC)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()

		if _, err := store.Ingest(ctx, []usage.Event{
			makeEvent("g4", 4, 100),
			makeEvent("g3", 3, 200),
			makeEvent("g2", 2, 300),
		}); err != nil {
			t.Fatal(err)
		}
		hm, err := store.Activity(ctx, now)
		if err != nil {
			t.Fatal(err)
		}
		if hm.CurrentStreak != 0 || hm.LongestStreak != 3 {
			t.Fatalf("streaks = current:%d longest:%d, want 0 / 3", hm.CurrentStreak, hm.LongestStreak)
		}
	})
}

func TestActivityCurrentStreakGraceWhenTodayEmpty(t *testing.T) {
	store, err := OpenWithLocation(filepath.Join(t.TempDir(), "grace.db"), catalog.Empty(), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Date(2026, 11, 20, 8, 0, 0, 0, time.UTC)
	ctx := context.Background()
	if _, err := store.Ingest(ctx, []usage.Event{
		{
			SchemaVersion: usage.SchemaVersion, EventID: "gr-1", Timestamp: now.AddDate(0, 0, -2),
			MachineID: "m1", Provider: "openai", Model: "gpt-5", Tool: "codex",
			TotalTokens: usage.Int64(100), TokenAccuracy: usage.AccuracyReported,
			Source: usage.Source{Adapter: "codex", AdapterVersion: "1"},
		},
		{
			SchemaVersion: usage.SchemaVersion, EventID: "gr-2", Timestamp: now.AddDate(0, 0, -1),
			MachineID: "m1", Provider: "openai", Model: "gpt-5", Tool: "codex",
			TotalTokens: usage.Int64(200), TokenAccuracy: usage.AccuracyReported,
			Source: usage.Source{Adapter: "codex", AdapterVersion: "1"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	hm, err := store.Activity(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if hm.CurrentStreak != 2 || hm.LongestStreak != 2 {
		t.Fatalf("streaks = current:%d longest:%d, want 2 / 2", hm.CurrentStreak, hm.LongestStreak)
	}
}

func TestActivityEmptyAndUnknownOnlyStreaks(t *testing.T) {
	now := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	ctx := context.Background()

	emptyStore, err := OpenWithLocation(filepath.Join(t.TempDir(), "empty.db"), catalog.Empty(), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	defer emptyStore.Close()
	emptyHM, err := emptyStore.Activity(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if emptyHM.CurrentStreak != 0 || emptyHM.LongestStreak != 0 || emptyHM.PeakDay != nil {
		t.Fatalf("empty Activity = %+v, want 0/0/nil", emptyHM)
	}

	unkStore, err := OpenWithLocation(filepath.Join(t.TempDir(), "unk.db"), catalog.Empty(), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	defer unkStore.Close()
	if _, err := unkStore.Ingest(ctx, []usage.Event{
		{
			SchemaVersion: usage.SchemaVersion, EventID: "unk-1", Timestamp: now.AddDate(0, 0, -1),
			MachineID: "m1", Provider: "openai", Model: "gpt-5", Tool: "codex",
			TokenAccuracy: usage.AccuracyUnknown, Source: usage.Source{Adapter: "codex", AdapterVersion: "1"},
		},
		{
			SchemaVersion: usage.SchemaVersion, EventID: "unk-2", Timestamp: now,
			MachineID: "m1", Provider: "openai", Model: "gpt-5", Tool: "codex",
			TokenAccuracy: usage.AccuracyUnknown, Source: usage.Source{Adapter: "codex", AdapterVersion: "1"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	unkHM, err := unkStore.Activity(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if unkHM.CurrentStreak != 2 || unkHM.LongestStreak != 2 || unkHM.PeakDay != nil || unkHM.MaxDailyTokens != 0 {
		t.Fatalf("unknown-only Activity = current:%d longest:%d peak:%+v max:%d", unkHM.CurrentStreak, unkHM.LongestStreak, unkHM.PeakDay, unkHM.MaxDailyTokens)
	}
}

func TestActivityStreaksTimezoneDSTAndPeakTieBreak(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenWithLocation(filepath.Join(t.TempDir(), "ny.db"), catalog.Empty(), ny)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// 2026-11-01 is fall-back DST in NY (04:00Z -> 00:00 EDT; 2026-11-02 05:00Z -> 00:00 EST).
	// Also test equal peak tie-breaking (earliest wins) and >53w old event exclusion.
	now := time.Date(2026, 11, 3, 12, 0, 0, 0, ny)
	ctx := context.Background()
	if _, err := store.Ingest(ctx, []usage.Event{
		{
			// > 53 weeks old: must NOT count toward streak or PeakDay.
			SchemaVersion: usage.SchemaVersion, EventID: "old-1", Timestamp: now.AddDate(-2, 0, 0),
			MachineID: "m1", Provider: "openai", Model: "gpt-5", Tool: "codex",
			TotalTokens: usage.Int64(999_999), TokenAccuracy: usage.AccuracyReported,
			Source: usage.Source{Adapter: "codex", AdapterVersion: "1"},
		},
		{
			// 2026-11-01 23:30 EDT = 2026-11-02T03:30:00Z -> local 2026-11-01.
			SchemaVersion: usage.SchemaVersion, EventID: "ny-nov1", Timestamp: time.Date(2026, 11, 2, 3, 30, 0, 0, time.UTC),
			MachineID: "m1", Provider: "openai", Model: "gpt-5", Tool: "codex",
			TotalTokens: usage.Int64(1000), TokenAccuracy: usage.AccuracyReported,
			Source: usage.Source{Adapter: "codex", AdapterVersion: "1"},
		},
		{
			// 2026-11-02 23:30 EST = 2026-11-03T04:30:00Z -> local 2026-11-02 (tie on 1000 tokens).
			SchemaVersion: usage.SchemaVersion, EventID: "ny-nov2", Timestamp: time.Date(2026, 11, 3, 4, 30, 0, 0, time.UTC),
			MachineID: "m1", Provider: "openai", Model: "gpt-5", Tool: "codex",
			TotalTokens: usage.Int64(1000), TokenAccuracy: usage.AccuracyReported,
			Source: usage.Source{Adapter: "codex", AdapterVersion: "1"},
		},
		{
			// Future event tomorrow: must NOT count toward streak or PeakDay.
			SchemaVersion: usage.SchemaVersion, EventID: "future-1", Timestamp: now.AddDate(0, 0, 1),
			MachineID: "m1", Provider: "openai", Model: "gpt-5", Tool: "codex",
			TotalTokens: usage.Int64(500_000), TokenAccuracy: usage.AccuracyReported,
			Source: usage.Source{Adapter: "codex", AdapterVersion: "1"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	hm, err := store.Activity(ctx, now)
	if err != nil {
		t.Fatal(err)
	}
	if hm.CurrentStreak != 2 || hm.LongestStreak != 2 {
		t.Fatalf("NY DST streaks = current:%d longest:%d, want 2 / 2", hm.CurrentStreak, hm.LongestStreak)
	}
	if hm.PeakDay == nil || hm.PeakDay.Date != "2026-11-01" || hm.PeakDay.Tokens != 1000 {
		t.Fatalf("PeakDay tie-break = %+v, want earliest 2026-11-01 with 1000 tokens", hm.PeakDay)
	}
}
