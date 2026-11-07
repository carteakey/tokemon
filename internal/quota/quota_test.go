package quota

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestLoadValidatesQuotas(t *testing.T) {
	document := `schema_version: 1
quotas:
  - provider: openai
    label: OpenAI monthly cap
    reset_day: 1
    budget:
      tokens_per_month: 50000000
  - provider: anthropic
    model: claude-opus-5-5
    reset_day: 15
    budget:
      cost_per_month: 250.5
`
	path := filepath.Join(t.TempDir(), "quotas.yaml")
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	quotas, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(quotas) != 2 {
		t.Fatalf("quotas = %+v, want 2", quotas)
	}
	if *quotas[0].Budget.TokensPerMonth != 50_000_000 || *quotas[1].Budget.CostPerMonth != 250.5 {
		t.Fatalf("budgets = %+v", quotas)
	}

	for _, test := range []struct {
		name     string
		document string
		wantErr  string
	}{
		{"wrong schema", "schema_version: 2\nquotas: []\n", `quotas schema_version must be "1"`},
		{"reset day out of range", "schema_version: 1\nquotas:\n  - provider: openai\n    reset_day: 29\n    budget:\n      tokens_per_month: 10\n", "reset_day must be between 1 and 28"},
		{"no budget", "schema_version: 1\nquotas:\n  - provider: openai\n    reset_day: 1\n    budget: {}\n", "budget requires tokens_per_month or cost_per_month"},
		{"zero tokens", "schema_version: 1\nquotas:\n  - provider: openai\n    reset_day: 1\n    budget:\n      tokens_per_month: 0\n", "tokens_per_month must be positive"},
		{"negative cost", "schema_version: 1\nquotas:\n  - provider: openai\n    reset_day: 1\n    budget:\n      cost_per_month: -5\n", "cost_per_month must be positive"},
		{"duplicate", "schema_version: 1\nquotas:\n  - provider: openai\n    reset_day: 1\n    budget:\n      tokens_per_month: 10\n  - provider: openai\n    reset_day: 5\n    budget:\n      tokens_per_month: 20\n", "duplicates quota 1"},
	} {
		path := filepath.Join(t.TempDir(), "quotas.yaml")
		if err := os.WriteFile(path, []byte(test.document), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil || !strings.Contains(err.Error(), test.wantErr) {
			t.Errorf("%s: err = %v, want it to contain %q", test.name, err, test.wantErr)
		}
	}
}

func TestSpecificityAndMatch(t *testing.T) {
	global := Quota{ResetDay: 1}
	providerQuota := Quota{Provider: "openai", ResetDay: 1}
	modelQuota := Quota{Provider: "openai", Model: "gpt-6.1-sol", ResetDay: 1}
	if Specificity(modelQuota) >= Specificity(providerQuota) || Specificity(providerQuota) >= Specificity(global) {
		t.Fatal("specificity ordering is wrong")
	}
	if !Match(modelQuota, "openai", "gpt-6.1-sol") || Match(modelQuota, "openai", "gpt-6-luna") {
		t.Fatal("model match is wrong")
	}
	if !Match(providerQuota, "openai", "anything") || Match(providerQuota, "anthropic", "anything") {
		t.Fatal("provider match is wrong")
	}
	if !Match(global, "", "") {
		t.Fatal("global quota should match everything")
	}
}

func TestPeriodForMonthBoundaries(t *testing.T) {
	utc := time.UTC
	// Mid-month: the period started on this month's reset day.
	start, end := PeriodFor(time.Date(2026, 11, 15, 12, 0, 0, 0, utc), 1, utc)
	if !start.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, utc)) || !end.Equal(time.Date(2026, 12, 1, 0, 0, 0, 0, utc)) {
		t.Fatalf("period = %s .. %s", start, end)
	}
	// Exactly on the reset instant: the new period starts now.
	start, _ = PeriodFor(time.Date(2026, 11, 1, 0, 0, 0, 0, utc), 1, utc)
	if !start.Equal(time.Date(2026, 11, 1, 0, 0, 0, 0, utc)) {
		t.Fatalf("start on reset day = %s", start)
	}
	// Reset day 31: on Feb 15 the running period started Jan 31 (January has
	// a 31st) and ends at the next reset, clamped to February's last day.
	start, end = PeriodFor(time.Date(2026, 2, 15, 0, 0, 0, 0, utc), 31, utc)
	if !start.Equal(time.Date(2026, 1, 31, 0, 0, 0, 0, utc)) || !end.Equal(time.Date(2026, 2, 28, 0, 0, 0, 0, utc)) {
		t.Fatalf("clamped period = %s .. %s", start, end)
	}
	// Leap year: a day-30 quota resets Mar 30 after Feb 29 in 2028; on Feb 10
	// the running period is Jan 30 .. Feb 29.
	start, end = PeriodFor(time.Date(2028, 2, 10, 0, 0, 0, 0, utc), 30, utc)
	if !start.Equal(time.Date(2028, 1, 30, 0, 0, 0, 0, utc)) || !end.Equal(time.Date(2028, 2, 29, 0, 0, 0, 0, utc)) {
		t.Fatalf("leap period = %s .. %s", start, end)
	}
	// After the clamped reset day passes, the next period ends Mar 31.
	start, end = PeriodFor(time.Date(2026, 3, 5, 0, 0, 0, 0, utc), 31, utc)
	if !start.Equal(time.Date(2026, 2, 28, 0, 0, 0, 0, utc)) || !end.Equal(time.Date(2026, 3, 31, 0, 0, 0, 0, utc)) {
		t.Fatalf("post-February period = %s .. %s", start, end)
	}
}

func TestPeriodForDSTTransitions(t *testing.T) {
	// America/New_York: DST starts 2026-03-08 and ends 2026-11-01 (local).
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Skip("tzdata unavailable")
	}
	// A period containing the spring-forward transition: start stays local
	// midnight Mar 1 (EST, -05:00), end is local midnight Apr 1 (EDT, -04:00).
	start, end := PeriodFor(time.Date(2026, 3, 15, 12, 0, 0, 0, loc), 1, loc)
	if !start.Equal(time.Date(2026, 3, 1, 0, 0, 0, 0, loc)) || start.UTC().Hour() != 5 {
		t.Fatalf("DST spring start = %s (%s)", start, start.UTC())
	}
	if !end.Equal(time.Date(2026, 4, 1, 0, 0, 0, 0, loc)) || end.UTC().Hour() != 4 {
		t.Fatalf("DST spring end = %s (%s)", end, end.UTC())
	}
	// A period containing the fall-back transition keeps local midnights.
	start, end = PeriodFor(time.Date(2026, 10, 15, 12, 0, 0, 0, loc), 2, loc)
	if !start.Equal(time.Date(2026, 10, 2, 0, 0, 0, 0, loc)) {
		t.Fatalf("DST fall start = %s", start)
	}
	if !end.Equal(time.Date(2026, 11, 2, 0, 0, 0, 0, loc)) || end.UTC().Hour() != 5 {
		t.Fatalf("DST fall end = %s (%s)", end, end.UTC())
	}
}
