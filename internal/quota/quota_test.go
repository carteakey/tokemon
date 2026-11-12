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
		{"reset day out of range", "schema_version: 1\nquotas:\n  - provider: openai\n    reset_day: 32\n    budget:\n      tokens_per_month: 10\n", "reset_day must be between 1 and 31"},
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

func TestLoadAcceptsResetDay31(t *testing.T) {
	// Day 29+ clamps to each month's last day (issue #37): the documented
	// billing-cycle convention must load from config, not just clamp in
	// PeriodFor if it could ever be reached.
	document := `schema_version: 1
quotas:
  - provider: openai
    label: OpenAI monthly cap
    reset_day: 31
    budget:
      tokens_per_month: 50000000
`
	path := filepath.Join(t.TempDir(), "quotas.yaml")
	if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	quotas, err := Load(path)
	if err != nil {
		t.Fatalf("reset_day 31 rejected: %v", err)
	}
	if len(quotas) != 1 || quotas[0].ResetDay != 31 {
		t.Fatalf("quotas = %+v, want one day-31 quota", quotas)
	}
}

func TestValidateResetDayRange(t *testing.T) {
	tokens := int64(10)
	for _, test := range []struct {
		resetDay int
		wantErr  bool
	}{
		{resetDay: 0, wantErr: true},
		{resetDay: -1, wantErr: true},
		{resetDay: 32, wantErr: true},
		{resetDay: 1},
		{resetDay: 28},
		{resetDay: 29},
		{resetDay: 31},
	} {
		candidate := Quota{ResetDay: test.resetDay, Budget: Budget{TokensPerMonth: &tokens}}
		err := Validate(candidate)
		if (err != nil) != test.wantErr {
			t.Errorf("Validate(reset_day %d) err = %v, wantErr %v", test.resetDay, err, test.wantErr)
		}
	}
}

func TestSpecificityAndMatch(t *testing.T) {
	global := Quota{ResetDay: 1}
	providerQuota := Quota{Provider: "openai", ResetDay: 1}
	modelOnlyQuota := Quota{Model: "gpt-6.1-sol", ResetDay: 1}
	modelQuota := Quota{Provider: "openai", Model: "gpt-6.1-sol", ResetDay: 1}

	// Rank by non-empty match fields: provider+model, then either field
	// alone (provider-only and model-only are a genuine tie), then catch-all.
	for _, test := range []struct {
		name string
		rank int
		q    Quota
	}{
		{"provider+model", 0, modelQuota},
		{"provider-only", 1, providerQuota},
		{"model-only", 1, modelOnlyQuota},
		{"catch-all", 2, global},
	} {
		if got := Specificity(test.q); got != test.rank {
			t.Errorf("Specificity(%s) = %d, want %d", test.name, got, test.rank)
		}
	}
	// The order match resolution relies on: a model-only quota deterministically
	// beats a catch-all instead of tying into configuration order.
	if !(Specificity(modelQuota) < Specificity(providerQuota) &&
		Specificity(providerQuota) <= Specificity(modelOnlyQuota) &&
		Specificity(modelOnlyQuota) < Specificity(global)) {
		t.Fatal("specificity ordering is wrong")
	}
	if !Match(modelQuota, "openai", "gpt-6.1-sol") || Match(modelQuota, "openai", "gpt-6-luna") {
		t.Fatal("model match is wrong")
	}
	if !Match(providerQuota, "openai", "anything") || Match(providerQuota, "anthropic", "anything") {
		t.Fatal("provider match is wrong")
	}
	if !Match(modelOnlyQuota, "openai", "gpt-6.1-sol") || !Match(modelOnlyQuota, "anthropic", "gpt-6.1-sol") || Match(modelOnlyQuota, "openai", "gpt-6-luna") {
		t.Fatal("model-only match is wrong")
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

func TestPeriodForClampsDay31AcrossMonthLengths(t *testing.T) {
	// Day 31 resets on each month's last day when the month is shorter
	// (issue #37): February non-leap and leap, and a 30-day month.
	utc := time.UTC
	tests := []struct {
		name string
		now  time.Time
		want time.Time // clamped end of the running period
	}{
		{"February non-leap", time.Date(2026, 2, 10, 0, 0, 0, 0, utc), time.Date(2026, 2, 28, 0, 0, 0, 0, utc)},
		{"February leap", time.Date(2028, 2, 10, 0, 0, 0, 0, utc), time.Date(2028, 2, 29, 0, 0, 0, 0, utc)},
		{"30-day month", time.Date(2026, 4, 15, 0, 0, 0, 0, utc), time.Date(2026, 4, 30, 0, 0, 0, 0, utc)},
	}
	for _, test := range tests {
		start, end := PeriodFor(test.now, 31, utc)
		if !end.Equal(test.want) {
			t.Errorf("%s: end = %s, want %s", test.name, end, test.want)
		}
		if !start.Before(test.want) {
			t.Errorf("%s: start %s not before end %s", test.name, start, test.want)
		}
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
