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
