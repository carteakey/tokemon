package database

import (
	"context"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/tokemon/tokemon/internal/catalog"
	"github.com/tokemon/tokemon/internal/quota"
	"github.com/tokemon/tokemon/internal/usage"
)

func TestQuotaStatusMeasuresConfiguredPeriods(t *testing.T) {
	tokensBudget := int64(50_000_000)
	costBudget := 100.0
	// A reset day ~2 days before now guarantees the current period has at
	// least ~2 days of elapsed history regardless of the real date.
	now := time.Now().UTC()
	resetDay := now.Day() - 2
	if resetDay < 1 {
		resetDay = 15
	}
	if resetDay > 28 {
		resetDay = 28
	}
	quotas := []quota.Quota{
		{Provider: "openai", Label: "OpenAI cap", ResetDay: resetDay, Budget: quota.Budget{TokensPerMonth: &tokensBudget}},
		{Provider: "anthropic", Model: "claude-opus-5-5", Label: "Opus cap", ResetDay: resetDay, Budget: quota.Budget{CostPerMonth: &costBudget}},
	}
	store, err := OpenWithLocation(filepath.Join(t.TempDir(), "tokemon.db"), catalog.Empty(), time.UTC, quotas...)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	start, end := quota.PeriodFor(now, resetDay, time.UTC)
	elapsed := now.Sub(start)
	if elapsed < 24*time.Hour {
		t.Skipf("period has less than a day of history (start %s)", start)
	}
	at := func(fraction float64) time.Time {
		return start.Add(time.Duration(fraction*elapsed.Hours()) * time.Hour)
	}
	event := func(id string, when time.Time, provider, model string, tokens int64, withCost bool) usage.Event {
		e := usage.Event{
			SchemaVersion: usage.SchemaVersion, EventID: id, Timestamp: when,
			MachineID: "machine", SessionID: "session-" + id, Provider: provider,
			Model: model, Tool: "codex", InputTokens: usage.Int64(tokens / 2), OutputTokens: usage.Int64(tokens / 2),
			TotalTokens: usage.Int64(tokens), TokenAccuracy: usage.AccuracyReported,
			Source: usage.Source{Adapter: "codex", AdapterVersion: "test"},
		}
		if withCost {
			e.Cost = usage.Float64(10.0)
		}
		return e
	}
	events := []usage.Event{
		event("quota-early", at(0.1), "openai", "gpt-6.1-sol", 10_000_000, true),
		event("quota-mid", at(0.6), "openai", "gpt-6.1-sol", 30_000_000, true),
		event("quota-late", at(0.9), "openai", "gpt-6.1-sol", 25_000_000, true),
		// Unknown total: disclosed on the quota, never counted as tokens.
		{SchemaVersion: usage.SchemaVersion, EventID: "quota-unknown", Timestamp: at(0.5), MachineID: "machine", Provider: "openai", Model: "gpt-6.1-sol", Tool: "codex", TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "codex", AdapterVersion: "test"}},
		// Cost-budget quota with no cost rows: cost stays Unknown.
		event("quota-opus", at(0.7), "anthropic", "claude-opus-5-5", 5_000_000, false),
		// Wrong model: the provider+model quota must not match it.
		event("quota-other", at(0.8), "anthropic", "claude-sonnet-5-5", 9_000_000, false),
	}
	if result, err := store.Ingest(context.Background(), events); err != nil || result.Accepted != len(events) {
		t.Fatalf("ingest = %+v, err %v", result, err)
	}

	statuses := store.QuotaStatus(context.Background(), now)
	if len(statuses) != 2 {
		t.Fatalf("statuses = %+v, want 2", statuses)
	}

	// The provider+model quota is more specific and comes first.
	opus := statuses[0]
	if opus.Label != "Opus cap" || opus.Model != "claude-opus-5-5" {
		t.Fatalf("first status = %+v", opus)
	}
	if opus.TokensUsed != 5_000_000 || opus.TokensBudget != nil {
		t.Errorf("opus tokens = %d (budget %+v), want only the matching model's usage", opus.TokensUsed, opus.TokensBudget)
	}
	if opus.CostUsed != nil || opus.CostBudget == nil || *opus.CostBudget != 100 {
		t.Errorf("opus cost = %+v, want Unknown against a $100 budget", opus.CostUsed)
	}
	if opus.State != "unknown" {
		t.Errorf("opus state = %q, want unknown (cost budget, no priced usage)", opus.State)
	}
	if opus.PeriodStart != start.Format("2006-01-02") || opus.PeriodEnd != end.Format("2006-01-02") {
		t.Errorf("opus period = %s .. %s, want %s .. %s", opus.PeriodStart, opus.PeriodEnd, start.Format("2006-01-02"), end.Format("2006-01-02"))
	}
	wantDays := int(end.Sub(start).Hours() / 24)
	if len(opus.Daily) != wantDays {
		t.Errorf("opus daily points = %d, want one per period day (%d)", len(opus.Daily), wantDays)
	}

	openai := statuses[1]
	if openai.Label != "OpenAI cap" {
		t.Fatalf("second status = %+v", openai)
	}
	if openai.TokensUsed != 65_000_000 {
		t.Errorf("openai tokens = %d, want 65000000 (unknown-total event excluded)", openai.TokensUsed)
	}
	if openai.UnknownTotalEvents != 1 {
		t.Errorf("unknown total events = %d, want 1", openai.UnknownTotalEvents)
	}
	if openai.State != "over" {
		t.Errorf("openai state = %q, want over (65M of 50M)", openai.State)
	}
	earlyIndex := int(at(0.1).Sub(start).Hours() / 24)
	if openai.Daily[earlyIndex].CumulativeTokens != 10_000_000 {
		t.Errorf("cumulative at the early event's day = %d, want 10000000", openai.Daily[earlyIndex].CumulativeTokens)
	}
	last := openai.Daily[len(openai.Daily)-1]
	if last.Date != end.Add(-24*time.Hour).Format("2006-01-02") || last.CumulativeTokens != 65_000_000 {
		t.Errorf("final point = %+v, want 65M on the period's last day", last)
	}
	// Linear pace: 65M over the elapsed fraction projects across the full
	// period, and the 50M budget is crossed at 50/65 of the elapsed span.
	projected := int64(65_000_000 * end.Sub(start).Hours() / elapsed.Hours())
	if openai.ProjectedTokens == nil || *openai.ProjectedTokens < projected*99/100 || *openai.ProjectedTokens > projected*101/100 {
		t.Errorf("projection = %+v, want about %d", openai.ProjectedTokens, projected)
	}
	wantCrossing := start.Add(time.Duration((50.0/65.0)*elapsed.Hours()) * time.Hour).Format("2006-01-02")
	if openai.ProjectedOverageDate != wantCrossing {
		t.Errorf("overage date = %q, want %q", openai.ProjectedOverageDate, wantCrossing)
	}
	if openai.Daily[0].Date != start.Format("2006-01-02") {
		t.Errorf("daily start = %s, want %s", openai.Daily[0].Date, start.Format("2006-01-02"))
	}
}

func TestQuotaStatusEmptyWithoutQuotas(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "tokemon.db"), catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if statuses := store.QuotaStatus(context.Background(), time.Now()); statuses != nil {
		t.Fatalf("statuses = %+v, want nil without quotas", statuses)
	}
}

// quotaUsageEvent builds one priced usage event at a fixed instant.
func quotaUsageEvent(id string, when time.Time, provider, model string, tokens int64) usage.Event {
	return usage.Event{
		SchemaVersion: usage.SchemaVersion, EventID: id, Timestamp: when,
		MachineID: "machine", SessionID: "session-" + id, Provider: provider,
		Model: model, Tool: "codex", InputTokens: usage.Int64(tokens / 2), OutputTokens: usage.Int64(tokens - tokens/2),
		TotalTokens: usage.Int64(tokens), TokenAccuracy: usage.AccuracyReported,
		Source: usage.Source{Adapter: "codex", AdapterVersion: "test"},
	}
}

// ingestQuotaEvents ingests events and fails the test on any rejection.
func ingestQuotaEvents(t *testing.T, store *Store, events []usage.Event) {
	t.Helper()
	if result, err := store.Ingest(context.Background(), events); err != nil || result.Accepted != len(events) {
		t.Fatalf("ingest = %+v, err %v", result, err)
	}
}

// fixedNow anchors every attribution test to a deterministic period: with
// reset_day 1 the current period is 2026-11-01 .. 2026-12-01 (UTC).
func fixedNow() time.Time {
	return time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
}

func TestQuotaStatusAttributesEventsToMostSpecificQuota(t *testing.T) {
	providerBudget := int64(100_000_000)
	modelBudget := int64(40_000_000)
	quotas := []quota.Quota{
		{Provider: "openai", Label: "OpenAI cap", ResetDay: 1, Budget: quota.Budget{TokensPerMonth: &providerBudget}},
		{Provider: "openai", Model: "gpt-6.1-sol", Label: "Sol cap", ResetDay: 1, Budget: quota.Budget{TokensPerMonth: &modelBudget}},
	}
	store, err := OpenWithLocation(filepath.Join(t.TempDir(), "tokemon.db"), catalog.Empty(), time.UTC, quotas...)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ingestQuotaEvents(t, store, []usage.Event{
		quotaUsageEvent("overlap-sol-1", time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC), "openai", "gpt-6.1-sol", 30_000_000),
		quotaUsageEvent("overlap-sol-2", time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC), "openai", "gpt-6.1-sol", 5_000_000),
		quotaUsageEvent("overlap-other", time.Date(2026, 11, 5, 11, 0, 0, 0, time.UTC), "openai", "gpt-6-luna", 10_000_000),
	})

	statuses := store.QuotaStatus(context.Background(), fixedNow())
	if len(statuses) != 2 {
		t.Fatalf("statuses = %+v, want 2", statuses)
	}
	// The provider+model quota is more specific and comes first; the model's
	// events belong to it alone, so the provider quota measures only the
	// remainder instead of the double count (issue #36).
	sol, openai := statuses[0], statuses[1]
	if sol.Label != "Sol cap" || openai.Label != "OpenAI cap" {
		t.Fatalf("ordering = %q then %q, want the specific quota first", sol.Label, openai.Label)
	}
	if sol.TokensUsed != 35_000_000 {
		t.Errorf("sol tokens = %d, want 35000000", sol.TokensUsed)
	}
	if openai.TokensUsed != 10_000_000 {
		t.Errorf("openai tokens = %d, want 10000000 (remainder, not 45000000)", openai.TokensUsed)
	}
	if last := sol.Daily[len(sol.Daily)-1]; last.CumulativeTokens != 35_000_000 {
		t.Errorf("sol final point = %+v, want 35000000", last)
	}
	if last := openai.Daily[len(openai.Daily)-1]; last.CumulativeTokens != 10_000_000 {
		t.Errorf("openai final point = %+v, want 10000000", last)
	}
	if sol.State != "ok" || openai.State != "ok" {
		t.Errorf("states = %q / %q, want ok / ok", sol.State, openai.State)
	}
}

func TestQuotaStatusCatchAllGetsOnlyRemainder(t *testing.T) {
	allBudget := int64(100_000_000)
	providerBudget := int64(50_000_000)
	quotas := []quota.Quota{
		{Provider: "anthropic", Label: "Anthropic cap", ResetDay: 1, Budget: quota.Budget{TokensPerMonth: &providerBudget}},
		{Label: "Everything cap", ResetDay: 1, Budget: quota.Budget{TokensPerMonth: &allBudget}},
	}
	store, err := OpenWithLocation(filepath.Join(t.TempDir(), "tokemon.db"), catalog.Empty(), time.UTC, quotas...)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ingestQuotaEvents(t, store, []usage.Event{
		quotaUsageEvent("catchall-anthropic", time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC), "anthropic", "claude-opus-5-5", 20_000_000),
		quotaUsageEvent("catchall-openai", time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC), "openai", "gpt-6.1-sol", 30_000_000),
	})

	statuses := store.QuotaStatus(context.Background(), fixedNow())
	if len(statuses) != 2 {
		t.Fatalf("statuses = %+v, want 2", statuses)
	}
	anthropic, everything := statuses[0], statuses[1]
	if anthropic.Label != "Anthropic cap" || everything.Label != "Everything cap" {
		t.Fatalf("ordering = %q then %q, want the provider quota first", anthropic.Label, everything.Label)
	}
	if anthropic.TokensUsed != 20_000_000 {
		t.Errorf("anthropic tokens = %d, want 20000000", anthropic.TokensUsed)
	}
	if everything.TokensUsed != 30_000_000 {
		t.Errorf("catch-all tokens = %d, want 30000000 (only the unclaimed remainder)", everything.TokensUsed)
	}
}

func TestQuotaStatusModelOnlyQuotaBeatsCatchAll(t *testing.T) {
	// A model-only quota (no provider) ranks above a catch-all, so its model's
	// events belong to it from every provider. The catch-all is defined first
	// on purpose: configuration order must not decide the match.
	allBudget := int64(100_000_000)
	modelBudget := int64(40_000_000)
	quotas := []quota.Quota{
		{Label: "Everything cap", ResetDay: 1, Budget: quota.Budget{TokensPerMonth: &allBudget}},
		{Model: "gpt-6.1-sol", Label: "Sol cap", ResetDay: 1, Budget: quota.Budget{TokensPerMonth: &modelBudget}},
	}
	store, err := OpenWithLocation(filepath.Join(t.TempDir(), "tokemon.db"), catalog.Empty(), time.UTC, quotas...)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ingestQuotaEvents(t, store, []usage.Event{
		quotaUsageEvent("modelonly-openai", time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC), "openai", "gpt-6.1-sol", 30_000_000),
		quotaUsageEvent("modelonly-anthropic", time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC), "anthropic", "gpt-6.1-sol", 5_000_000),
		quotaUsageEvent("modelonly-opus", time.Date(2026, 11, 5, 11, 0, 0, 0, time.UTC), "anthropic", "claude-opus-5-5", 20_000_000),
	})

	statuses := store.QuotaStatus(context.Background(), fixedNow())
	if len(statuses) != 2 {
		t.Fatalf("statuses = %+v, want 2", statuses)
	}
	sol, everything := statuses[0], statuses[1]
	if sol.Label != "Sol cap" || everything.Label != "Everything cap" {
		t.Fatalf("ordering = %q then %q, want the model-only quota first", sol.Label, everything.Label)
	}
	if sol.TokensUsed != 35_000_000 {
		t.Errorf("model-only tokens = %d, want 35000000 (the model's events from every provider)", sol.TokensUsed)
	}
	if everything.TokensUsed != 20_000_000 {
		t.Errorf("catch-all tokens = %d, want 20000000 (only the unclaimed remainder)", everything.TokensUsed)
	}
}

func TestQuotaStatusNonOverlappingQuotasUnchanged(t *testing.T) {
	openaiBudget := int64(50_000_000)
	anthropicBudget := int64(50_000_000)
	quotas := []quota.Quota{
		{Provider: "openai", Label: "OpenAI cap", ResetDay: 1, Budget: quota.Budget{TokensPerMonth: &openaiBudget}},
		{Provider: "anthropic", Label: "Anthropic cap", ResetDay: 1, Budget: quota.Budget{TokensPerMonth: &anthropicBudget}},
	}
	store, err := OpenWithLocation(filepath.Join(t.TempDir(), "tokemon.db"), catalog.Empty(), time.UTC, quotas...)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ingestQuotaEvents(t, store, []usage.Event{
		quotaUsageEvent("split-openai", time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC), "openai", "gpt-6.1-sol", 10_000_000),
		quotaUsageEvent("split-anthropic", time.Date(2026, 11, 6, 10, 0, 0, 0, time.UTC), "anthropic", "claude-opus-5-5", 25_000_000),
	})

	statuses := store.QuotaStatus(context.Background(), fixedNow())
	if len(statuses) != 2 {
		t.Fatalf("statuses = %+v, want 2", statuses)
	}
	if statuses[0].Provider != "openai" || statuses[1].Provider != "anthropic" {
		t.Fatalf("providers = %q / %q, want openai / anthropic", statuses[0].Provider, statuses[1].Provider)
	}
	if statuses[0].TokensUsed != 10_000_000 || statuses[1].TokensUsed != 25_000_000 {
		t.Errorf("tokens = %d / %d, want 10000000 / 25000000", statuses[0].TokensUsed, statuses[1].TokensUsed)
	}
}

func TestQuotaStatusClampsResetDay31Period(t *testing.T) {
	// Day 31 now loads from config (issue #37) and the period clamps to the
	// month's last day: running 2026-02-10, the period is Jan 31 .. Feb 28.
	budget := int64(1_000_000)
	quotas := []quota.Quota{
		{Provider: "openai", Label: "OpenAI cap", ResetDay: 31, Budget: quota.Budget{TokensPerMonth: &budget}},
	}
	store, err := OpenWithLocation(filepath.Join(t.TempDir(), "tokemon.db"), catalog.Empty(), time.UTC, quotas...)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ingestQuotaEvents(t, store, []usage.Event{
		quotaUsageEvent("clamp-inside", time.Date(2026, 2, 5, 10, 0, 0, 0, time.UTC), "openai", "gpt-6.1-sol", 100_000),
		quotaUsageEvent("clamp-after-reset", time.Date(2026, 2, 28, 10, 0, 0, 0, time.UTC), "openai", "gpt-6.1-sol", 200_000),
	})

	statuses := store.QuotaStatus(context.Background(), time.Date(2026, 2, 10, 12, 0, 0, 0, time.UTC))
	if len(statuses) != 1 {
		t.Fatalf("statuses = %+v, want 1", statuses)
	}
	status := statuses[0]
	if status.PeriodStart != "2026-01-31" || status.PeriodEnd != "2026-02-28" {
		t.Errorf("period = %s .. %s, want 2026-01-31 .. 2026-02-28", status.PeriodStart, status.PeriodEnd)
	}
	if status.TokensUsed != 100_000 {
		t.Errorf("tokens = %d, want 100000 (the post-reset event belongs to the next period)", status.TokensUsed)
	}
	if len(status.Daily) != 28 {
		t.Errorf("daily points = %d, want 28 (Jan 31 through Feb 27)", len(status.Daily))
	}
	if last := status.Daily[len(status.Daily)-1]; last.Date != "2026-02-27" || last.CumulativeTokens != 100_000 {
		t.Errorf("final point = %+v, want 100000 on 2026-02-27", last)
	}
}

func TestQuotaStatusTimezoneDSTAndSubsecondMidnightBoundaries(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	kolkata, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatal(err)
	}
	kathmandu, err := time.LoadLocation("Asia/Kathmandu")
	if err != nil {
		t.Fatal(err)
	}

	budget := int64(10_000_000)
	costBudget := 50.0
	q := []quota.Quota{{Label: "Global", ResetDay: 1, Budget: quota.Budget{TokensPerMonth: &budget, CostPerMonth: &costBudget}}}

	t.Run("spring-forward-and-subsecond-midnight-NY", func(t *testing.T) {
		store, err := OpenWithLocation(filepath.Join(t.TempDir(), "tokemon.db"), catalog.Empty(), ny, q...)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()

		// Period 2026-03-01 .. 2026-04-01 in America/New_York:
		// 2026-03-01 00:00 EST = 2026-03-01T05:00:00Z
		// 2026-03-08 is spring-forward (2026-03-08 04:59:59.999Z -> Mar 7; 2026-03-08 05:00:00.123Z -> Mar 8;
		// 2026-03-09 03:59:59.999Z -> Mar 8; 2026-03-09 04:00:00.123Z -> Mar 9).
		// 2026-04-01 00:00:00.123 EDT = 2026-04-01T04:00:00.123Z -> belongs to NEXT period (excluded!).
		events := []usage.Event{
			quotaUsageEvent("before-start", time.Date(2026, 3, 1, 4, 59, 59, 999_000_000, time.UTC), "openai", "gpt-5", 999),
			quotaUsageEvent("start-subsec", time.Date(2026, 3, 1, 5, 0, 0, 123_000_000, time.UTC), "openai", "gpt-5", 100),
			quotaUsageEvent("dst-mar7-end", time.Date(2026, 3, 8, 4, 59, 59, 999_000_000, time.UTC), "openai", "gpt-5", 200),
			quotaUsageEvent("dst-mar8-subsec", time.Date(2026, 3, 8, 5, 0, 0, 123_000_000, time.UTC), "openai", "gpt-5", 300),
			quotaUsageEvent("dst-mar8-end", time.Date(2026, 3, 9, 3, 59, 59, 999_000_000, time.UTC), "openai", "gpt-5", 400),
			quotaUsageEvent("dst-mar9-subsec", time.Date(2026, 3, 9, 4, 0, 0, 123_000_000, time.UTC), "openai", "gpt-5", 500),
			quotaUsageEvent("after-end-subsec", time.Date(2026, 4, 1, 4, 0, 0, 123_000_000, time.UTC), "openai", "gpt-5", 888),
		}
		ingestQuotaEvents(t, store, events)

		statuses := store.QuotaStatus(context.Background(), time.Date(2026, 3, 15, 12, 0, 0, 0, ny))
		if len(statuses) != 1 {
			t.Fatalf("statuses = %+v", statuses)
		}
		st := statuses[0]
		if st.TokensUsed != 1500 {
			t.Fatalf("TokensUsed = %d, want 1500 (excluding before-start and after-end-subsec)", st.TokensUsed)
		}
		byDate := map[string]int64{}
		for _, pt := range st.Daily {
			byDate[pt.Date] = pt.CumulativeTokens
		}
		if byDate["2026-03-01"] != 100 || byDate["2026-03-07"] != 300 || byDate["2026-03-08"] != 1000 || byDate["2026-03-09"] != 1500 {
			t.Fatalf("cumulative DST buckets mismatch: mar1=%d mar7=%d mar8=%d mar9=%d",
				byDate["2026-03-01"], byDate["2026-03-07"], byDate["2026-03-08"], byDate["2026-03-09"])
		}
	})

	t.Run("fall-back-NY", func(t *testing.T) {
		store, err := OpenWithLocation(filepath.Join(t.TempDir(), "tokemon.db"), catalog.Empty(), ny, q...)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()

		// 2026-11-01 is fall-back in NY (EDT -> EST):
		// 2026-11-01 00:00 EDT = 2026-11-01T04:00:00Z
		// 2026-11-02 00:00 EST = 2026-11-02T05:00:00Z
		events := []usage.Event{
			quotaUsageEvent("nov1-early", time.Date(2026, 11, 1, 4, 0, 0, 123_000_000, time.UTC), "openai", "gpt-5", 100),
			quotaUsageEvent("nov1-late", time.Date(2026, 11, 2, 4, 59, 59, 0, time.UTC), "openai", "gpt-5", 200),
			quotaUsageEvent("nov2-start", time.Date(2026, 11, 2, 5, 0, 0, 123_000_000, time.UTC), "openai", "gpt-5", 300),
		}
		ingestQuotaEvents(t, store, events)
		st := store.QuotaStatus(context.Background(), time.Date(2026, 11, 10, 12, 0, 0, 0, ny))[0]
		if st.Daily[0].Date != "2026-11-01" || st.Daily[0].CumulativeTokens != 300 || st.Daily[1].CumulativeTokens != 600 {
			t.Fatalf("fall-back cumulative = %+v", st.Daily[:2])
		}
	})

	for _, tc := range []struct {
		name string
		loc  *time.Location
		// Just before and just after 2026-11-05 00:00 local in UTC
		beforeUTC time.Time
		afterUTC  time.Time
	}{
		{
			name:      "half-hour-Kolkata",
			loc:       kolkata, // UTC+05:30 -> midnight is 18:30 UTC prior day
			beforeUTC: time.Date(2026, 11, 4, 18, 29, 59, 0, time.UTC),
			afterUTC:  time.Date(2026, 11, 4, 18, 30, 0, 123_000_000, time.UTC),
		},
		{
			name:      "45min-Kathmandu",
			loc:       kathmandu, // UTC+05:45 -> midnight is 18:15 UTC prior day
			beforeUTC: time.Date(2026, 11, 4, 18, 14, 59, 0, time.UTC),
			afterUTC:  time.Date(2026, 11, 4, 18, 15, 0, 123_000_000, time.UTC),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store, err := OpenWithLocation(filepath.Join(t.TempDir(), "tokemon.db"), catalog.Empty(), tc.loc, q...)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()

			evBefore := quotaUsageEvent("b", tc.beforeUTC, "openai", "gpt-5", 100)
			evAfter := quotaUsageEvent("a", tc.afterUTC, "openai", "gpt-5", 250)
			// Also include a non-Z legacy timestamp row directly in SQLite to exercise the fallback path.
			ingestQuotaEvents(t, store, []usage.Event{evBefore, evAfter})
			if _, err := store.db.Exec(`INSERT INTO usage_events (event_id, timestamp, machine_id, project, provider, raw_model, canonical_model, tool, total_tokens, cost, token_accuracy, adapter, adapter_version) VALUES ('legacy-offset', ?, 'm', '', 'openai', 'gpt-5', '', 'codex', 50, 1.5, 'reported', 'codex', '1')`,
				time.Date(2026, 11, 5, 10, 0, 0, 0, tc.loc).Format(time.RFC3339)); err != nil {
				t.Fatal(err)
			}

			st := store.QuotaStatus(context.Background(), time.Date(2026, 11, 10, 12, 0, 0, 0, tc.loc))[0]
			byDate := map[string]int64{}
			for _, pt := range st.Daily {
				byDate[pt.Date] = pt.CumulativeTokens
			}
			if byDate["2026-11-04"] != 100 || byDate["2026-11-05"] != 400 {
				t.Fatalf("%s buckets = nov4:%d nov5:%d", tc.name, byDate["2026-11-04"], byDate["2026-11-05"])
			}
			if st.CostUsed == nil || *st.CostUsed != 1.5 {
				t.Fatalf("%s CostUsed = %+v, want 1.5", tc.name, st.CostUsed)
			}
		})
	}
}

func TestQuotaStatusSWRCoalescingAndDayRollover(t *testing.T) {
	budget := int64(100_000)
	store, err := OpenWithLocation(filepath.Join(t.TempDir(), "tokemon.db"), catalog.Empty(), time.UTC,
		quota.Quota{Label: "Cap", ResetDay: 1, Budget: quota.Budget{TokensPerMonth: &budget}},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	ctx := context.Background()
	ingestQuotaEvents(t, store, []usage.Event{
		quotaUsageEvent("q-swr-1", time.Date(2026, 11, 5, 10, 0, 0, 0, time.UTC), "openai", "gpt-5", 10_000),
	})

	now1 := time.Date(2026, 11, 10, 2, 0, 0, 0, time.UTC)
	st1 := store.QuotaStatus(ctx, now1)
	if len(st1) != 1 || st1[0].TokensUsed != 10_000 || st1[0].ProjectedTokens == nil {
		t.Fatalf("initial QuotaStatus = %+v", st1)
	}
	proj1 := *st1[0].ProjectedTokens
	store.quotaMu.Lock()
	cachePtr1 := store.quotaCache
	store.quotaMu.Unlock()

	// Cache hit with a later 'now' on the same localDay recomputes projection accurately without DB query.
	now2 := time.Date(2026, 11, 10, 22, 0, 0, 0, time.UTC)
	st2 := store.QuotaStatus(ctx, now2)
	store.quotaMu.Lock()
	cachePtr2 := store.quotaCache
	store.quotaMu.Unlock()
	if cachePtr2 != cachePtr1 {
		t.Fatalf("expected same-localDay read to hit existing cache entry (%p vs %p)", cachePtr2, cachePtr1)
	}
	if st2[0].ProjectedTokens == nil || *st2[0].ProjectedTokens >= proj1 {
		t.Fatalf("expected projection to decrease at later elapsed time: proj1=%d proj2=%v", proj1, st2[0].ProjectedTokens)
	}
	if got := store.quotaRefreshes.Load(); got != 0 {
		t.Fatalf("refreshes on warm same-day read = %d, want 0", got)
	}

	// Burst of ingests inside 2s coalescing window keeps serving stale immediately and starts 0 refreshes inside window.
	ingestQuotaEvents(t, store, []usage.Event{
		quotaUsageEvent("q-swr-2", time.Date(2026, 11, 5, 11, 0, 0, 0, time.UTC), "openai", "gpt-5", 20_000),
		quotaUsageEvent("q-swr-3", time.Date(2026, 11, 5, 12, 0, 0, 0, time.UTC), "openai", "gpt-5", 5_000),
	})
	inside := store.QuotaStatus(ctx, now2)
	if inside[0].TokensUsed != 10_000 || store.quotaRefreshes.Load() != 0 {
		t.Fatalf("inside coalescing window = %+v, refreshes = %d", inside[0], store.quotaRefreshes.Load())
	}

	// Expire coalescing window; next read returns stale immediately and triggers 1 background refresh.
	store.quotaMu.Lock()
	warmed := store.quotaCache.fetched
	store.quotaCache.refreshDue = time.Now().Add(-time.Millisecond)
	store.quotaMu.Unlock()

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = store.QuotaStatus(ctx, now2)
		}()
	}
	wg.Wait()
	if got := store.quotaRefreshes.Load(); got != 1 {
		t.Fatalf("refreshes after post-window burst = %d, want 1", got)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		store.quotaMu.Lock()
		done := store.quotaCache != nil && store.quotaCache.fetched.After(warmed)
		store.quotaMu.Unlock()
		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background quota refresh did not complete")
		}
		time.Sleep(5 * time.Millisecond)
	}
	afterIngest := store.QuotaStatus(ctx, now2)
	if afterIngest[0].TokensUsed != 35_000 {
		t.Fatalf("TokensUsed after SWR refresh = %d, want 35000", afterIngest[0].TokensUsed)
	}

	// PurgeBefore deleting rows invalidates quotaCache too.
	if deleted, err := store.PurgeBefore(ctx, "2026-11-06"); err != nil || deleted != 3 {
		t.Fatalf("PurgeBefore = %d, err=%v", deleted, err)
	}
	store.quotaMu.Lock()
	warmed2 := store.quotaCache.fetched
	store.quotaCache.refreshDue = time.Now().Add(-time.Millisecond)
	store.quotaMu.Unlock()
	_ = store.QuotaStatus(ctx, now2)
	deadline = time.Now().Add(5 * time.Second)
	for {
		store.quotaMu.Lock()
		done := store.quotaCache != nil && store.quotaCache.fetched.After(warmed2)
		store.quotaMu.Unlock()
		if done {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background quota refresh after purge did not complete")
		}
		time.Sleep(5 * time.Millisecond)
	}
	afterPurge := store.QuotaStatus(ctx, now2)
	if afterPurge[0].TokensUsed != 0 {
		t.Fatalf("TokensUsed after purge refresh = %d, want 0", afterPurge[0].TokensUsed)
	}

	// Same-month localDay rollover (2026-11-10 -> 2026-11-20) bypasses prior-day cache synchronously.
	store.quotaMu.Lock()
	cacheBeforeRollover := store.quotaCache
	store.quotaMu.Unlock()
	refreshesBeforeRollover := store.quotaRefreshes.Load()
	laterSameMonth := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	rolledDay := store.QuotaStatus(ctx, laterSameMonth)
	store.quotaMu.Lock()
	cacheAfterRollover := store.quotaCache
	store.quotaMu.Unlock()
	if cacheAfterRollover == nil || cacheAfterRollover == cacheBeforeRollover || cacheAfterRollover.localDay != "2026-11-20" {
		t.Fatalf("expected new cache entry for localDay rollover 2026-11-20, got %+v", cacheAfterRollover)
	}
	if store.quotaRefreshes.Load() != refreshesBeforeRollover {
		t.Fatalf("expected synchronous recompute on localDay rollover without async refresh increment")
	}
	if rolledDay[0].PeriodStart != "2026-11-01" || rolledDay[0].PeriodEnd != "2026-12-01" {
		t.Fatalf("same-month rollover period = %s .. %s", rolledDay[0].PeriodStart, rolledDay[0].PeriodEnd)
	}

	// localDay rollover (next month) bypasses prior-day cache synchronously and resets period.
	nextMonth := time.Date(2026, 12, 5, 12, 0, 0, 0, time.UTC)
	rolled := store.QuotaStatus(ctx, nextMonth)
	if rolled[0].PeriodStart != "2026-12-01" || rolled[0].PeriodEnd != "2027-01-01" {
		t.Fatalf("rollover period = %s .. %s", rolled[0].PeriodStart, rolled[0].PeriodEnd)
	}
}

func TestQuotaStatusNonZLegacyBoundary(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	kolkata, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatal(err)
	}

	budget := int64(10_000_000)
	q := []quota.Quota{{Label: "Global", ResetDay: 1, Budget: quota.Budget{TokensPerMonth: &budget}}}

	insertRaw := func(t *testing.T, store *Store, id, ts string, tokens int64) {
		t.Helper()
		if _, err := store.db.Exec(
			`INSERT INTO usage_events (event_id, timestamp, machine_id, project, provider, raw_model, canonical_model, tool, total_tokens, token_accuracy, adapter, adapter_version) VALUES (?, ?, 'm', '', 'openai', 'gpt-5', '', 'codex', ?, 'reported', 'codex', '1')`,
			id, ts, tokens,
		); err != nil {
			t.Fatal(err)
		}
	}

	t.Run("NY-start-offset-included", func(t *testing.T) {
		store, err := OpenWithLocation(filepath.Join(t.TempDir(), "tokemon.db"), catalog.Empty(), ny, q...)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()

		insertRaw(t, store, "ny-in", "2026-03-01T01:00:00-05:00", 333)
		st := store.QuotaStatus(context.Background(), time.Date(2026, 3, 15, 12, 0, 0, 0, ny))
		if len(st) != 1 || st[0].TokensUsed != 333 {
			t.Fatalf("NY start non-Z TokensUsed = %+v, want 333", st)
		}
		if st[0].Daily[0].Date != "2026-03-01" || st[0].Daily[0].CumulativeTokens != 333 {
			t.Fatalf("NY start daily[0] = %+v, want 2026-03-01 cumulative 333", st[0].Daily[0])
		}
	})

	t.Run("Kolkata-end-offset-included", func(t *testing.T) {
		store, err := OpenWithLocation(filepath.Join(t.TempDir(), "tokemon.db"), catalog.Empty(), kolkata, q...)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()

		insertRaw(t, store, "kol-in", "2026-11-30T23:00:00+05:30", 333)
		st := store.QuotaStatus(context.Background(), time.Date(2026, 11, 15, 12, 0, 0, 0, kolkata))
		if len(st) != 1 || st[0].TokensUsed != 333 {
			t.Fatalf("Kolkata end non-Z TokensUsed = %+v, want 333", st)
		}
		last := st[0].Daily[len(st[0].Daily)-1]
		if last.Date != "2026-11-30" || last.CumulativeTokens != 333 {
			t.Fatalf("Kolkata end last daily = %+v, want 2026-11-30 cumulative 333", last)
		}
	})

	t.Run("out-of-period-non-Z-neighbors-excluded", func(t *testing.T) {
		storeNY, err := OpenWithLocation(filepath.Join(t.TempDir(), "ny.db"), catalog.Empty(), ny, q...)
		if err != nil {
			t.Fatal(err)
		}
		defer storeNY.Close()
		insertRaw(t, storeNY, "ny-before", "2026-02-28T23:00:00-05:00", 777)
		insertRaw(t, storeNY, "ny-after", "2026-04-01T00:30:00-04:00", 888)
		stNY := storeNY.QuotaStatus(context.Background(), time.Date(2026, 3, 15, 12, 0, 0, 0, ny))
		if len(stNY) != 1 || stNY[0].TokensUsed != 0 {
			t.Fatalf("NY out-of-period non-Z TokensUsed = %+v, want 0", stNY)
		}

		storeKol, err := OpenWithLocation(filepath.Join(t.TempDir(), "kol.db"), catalog.Empty(), kolkata, q...)
		if err != nil {
			t.Fatal(err)
		}
		defer storeKol.Close()
		insertRaw(t, storeKol, "kol-before", "2026-10-31T23:00:00+05:30", 777)
		insertRaw(t, storeKol, "kol-after", "2026-12-01T01:00:00+05:30", 888)
		stKol := storeKol.QuotaStatus(context.Background(), time.Date(2026, 11, 15, 12, 0, 0, 0, kolkata))
		if len(stKol) != 1 || stKol[0].TokensUsed != 0 {
			t.Fatalf("Kolkata out-of-period non-Z TokensUsed = %+v, want 0", stKol)
		}
	})

	t.Run("subsecond-non-Z-boundaries", func(t *testing.T) {
		store, err := OpenWithLocation(filepath.Join(t.TempDir(), "subsec.db"), catalog.Empty(), ny, q...)
		if err != nil {
			t.Fatal(err)
		}
		defer store.Close()

		insertRaw(t, store, "subsec-before", "2026-02-28T23:59:59.999-05:00", 999)
		insertRaw(t, store, "subsec-in", "2026-03-01T00:00:00.123-05:00", 333)
		insertRaw(t, store, "subsec-after", "2026-04-01T00:00:00.123-04:00", 555)
		st := store.QuotaStatus(context.Background(), time.Date(2026, 3, 15, 12, 0, 0, 0, ny))
		if len(st) != 1 || st[0].TokensUsed != 333 {
			t.Fatalf("sub-second non-Z TokensUsed = %+v, want 333", st)
		}
	})
}
