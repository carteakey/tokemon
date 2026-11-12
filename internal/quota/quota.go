// Package quota defines operator-configured usage budgets tracked by the
// dashboard. Providers do not report quota data through the adapters, so
// quotas live in a static YAML file (mirroring the model catalog) and the
// dashboard measures persisted usage against them. Nothing here invents
// provider-reported numbers.
package quota

import (
	"errors"
	"fmt"
	"os"
	"time"

	"gopkg.in/yaml.v3"
)

// SchemaVersion is the quotas.yaml document version.
const SchemaVersion = "1"

// Budget is the monthly allowance for one quota. Either or both dimensions
// may be set; both must be positive when present.
type Budget struct {
	TokensPerMonth *int64   `yaml:"tokens_per_month" json:"tokens_per_month"`
	CostPerMonth   *float64 `yaml:"cost_per_month" json:"cost_per_month"`
}

// Quota is one operator-defined monthly budget. Provider matches the
// usage_events.provider column (empty matches every provider); Model
// optionally narrows the quota to one model (canonical or raw form). The
// most specific matching quota applies.
type Quota struct {
	Provider string `yaml:"provider" json:"provider"`
	Model    string `yaml:"model" json:"model"`
	Label    string `yaml:"label" json:"label"`
	// ResetDay is the day of month the period restarts, 1–31. Days beyond a
	// month's length clamp to that month's last day (day 31 resets on Feb
	// 28/29), the standard billing-cycle convention.
	ResetDay int    `yaml:"reset_day" json:"reset_day"`
	Budget   Budget `yaml:"budget" json:"budget"`
}

type file struct {
	SchemaVersion string  `yaml:"schema_version"`
	Quotas        []Quota `yaml:"quotas"`
}

// Load reads and validates a quotas.yaml document. An empty quotas list is
// valid and means the dashboard runs without quota tracking.
func Load(path string) ([]Quota, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read quotas file: %w", err)
	}
	var document file
	if err := yaml.Unmarshal(raw, &document); err != nil {
		return nil, fmt.Errorf("parse quotas file: %w", err)
	}
	if document.SchemaVersion != SchemaVersion {
		return nil, fmt.Errorf("quotas schema_version must be %q, got %q", SchemaVersion, document.SchemaVersion)
	}
	seen := make(map[string]int, len(document.Quotas))
	for index, quota := range document.Quotas {
		if err := Validate(quota); err != nil {
			return nil, fmt.Errorf("quota %d: %w", index+1, err)
		}
		key := quota.Provider + "\x00" + quota.Model
		if previous, ok := seen[key]; ok {
			return nil, fmt.Errorf("quota %d duplicates quota %d (provider %q, model %q)", index+1, previous, quota.Provider, quota.Model)
		}
		seen[key] = index + 1
	}
	return document.Quotas, nil
}

// Validate checks one quota definition.
func Validate(quota Quota) error {
	if quota.ResetDay < 1 || quota.ResetDay > 31 {
		return fmt.Errorf("reset_day must be between 1 and 31, got %d", quota.ResetDay)
	}
	if quota.Budget.TokensPerMonth == nil && quota.Budget.CostPerMonth == nil {
		return errors.New("budget requires tokens_per_month or cost_per_month")
	}
	if quota.Budget.TokensPerMonth != nil && *quota.Budget.TokensPerMonth <= 0 {
		return errors.New("tokens_per_month must be positive")
	}
	if quota.Budget.CostPerMonth != nil && *quota.Budget.CostPerMonth <= 0 {
		return errors.New("cost_per_month must be positive")
	}
	return nil
}

// Specificity ranks quotas for match resolution by their number of non-empty
// match fields: a provider+model quota beats a single-field quota (provider
// only or model only), which beats a catch-all. Ranking model-only quotas
// above catch-alls keeps match resolution deterministic — without it, a
// model-only quota and a catch-all would tie and configuration order would
// decide which one claims the event.
func Specificity(quota Quota) int {
	switch {
	case quota.Provider != "" && quota.Model != "":
		return 0
	case quota.Provider != "" || quota.Model != "":
		return 1
	default:
		return 2
	}
}

// Match reports whether the quota applies to an event with the given
// provider and model (model matches either the canonical or raw form).
func Match(quota Quota, provider, model string) bool {
	if quota.Provider != "" && quota.Provider != provider {
		return false
	}
	if quota.Model != "" && quota.Model != model {
		return false
	}
	return true
}

// PeriodFor returns the quota period containing now: [start, end) local
// midnights around the most recent reset. Reset days beyond a month's length
// clamp to that month's last day (day 31 resets on Feb 28/29), the standard
// billing-cycle convention.
func PeriodFor(now time.Time, resetDay int, loc *time.Location) (start, end time.Time) {
	local := now.In(loc)
	start = lastResetOnOrBefore(local, resetDay, loc)
	return start, clampedReset(start.Year(), start.Month()+1, resetDay, loc)
}

func lastResetOnOrBefore(now time.Time, resetDay int, loc *time.Location) time.Time {
	candidate := clampedReset(now.Year(), now.Month(), resetDay, loc)
	if candidate.After(now) {
		candidate = clampedReset(now.Year(), now.Month()-1, resetDay, loc)
	}
	return candidate
}

func clampedReset(year int, month time.Month, resetDay int, loc *time.Location) time.Time {
	lastDay := time.Date(year, month+1, 0, 0, 0, 0, 0, loc).Day()
	if resetDay > lastDay {
		resetDay = lastDay
	}
	return time.Date(year, month, resetDay, 0, 0, 0, 0, loc)
}
