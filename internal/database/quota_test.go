package database

import (
	"context"
	"path/filepath"
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
