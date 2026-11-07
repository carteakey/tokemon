package database

import (
	"context"
	"database/sql"
	"sort"
	"time"

	"github.com/tokemon/tokemon/internal/quota"
)

// QuotaPoint is one day's cumulative usage inside the quota period, in the
// reporting timezone.
type QuotaPoint struct {
	Date             string   `json:"date"`
	CumulativeTokens int64    `json:"cumulative_tokens"`
	CumulativeCost   *float64 `json:"cumulative_cost,omitempty"`
}

// QuotaStatus is one operator-defined quota measured over its current period.
// Unknown states are explicit: a cost dimension with no priced usage reports
// CostUsed = nil, and State says which dimension could not be measured.
type QuotaStatus struct {
	Provider           string       `json:"provider,omitempty"`
	Model              string       `json:"model,omitempty"`
	Label              string       `json:"label"`
	ResetDay           int          `json:"reset_day"`
	PeriodStart        string       `json:"period_start"`
	PeriodEnd          string       `json:"period_end"`
	TokensUsed         int64        `json:"tokens_used"`
	TokensBudget       *int64       `json:"tokens_budget,omitempty"`
	CostUsed           *float64     `json:"cost_used,omitempty"`
	CostBudget         *float64     `json:"cost_budget,omitempty"`
	UnknownTotalEvents int64        `json:"unknown_total_events,omitempty"`
	Daily              []QuotaPoint `json:"daily"`
	// ProjectedTokens is the linear-pace projection for the full period, set
	// only when the period has elapsed at least one day of usage.
	ProjectedTokens      *int64   `json:"projected_tokens,omitempty"`
	ProjectedCost        *float64 `json:"projected_cost,omitempty"`
	ProjectedOverageDate string   `json:"projected_overage_date,omitempty"`
	// State is "over" when any measured dimension already exceeds its budget,
	// "unknown" when the only configured dimension cannot be measured (a cost
	// budget with no priced usage), and "ok" otherwise.
	State string `json:"state"`
}

// QuotaStatus measures every configured quota over its current period in the
// store's reporting timezone. With no quotas configured it returns nil — the
// dashboard renders an explicit Unknown state for that case itself.
func (s *Store) QuotaStatus(ctx context.Context, now time.Time) []QuotaStatus {
	if len(s.quotas) == 0 {
		return nil
	}
	ordered := make([]quota.Quota, len(s.quotas))
	copy(ordered, s.quotas)
	sort.SliceStable(ordered, func(left, right int) bool {
		return quota.Specificity(ordered[left]) < quota.Specificity(ordered[right])
	})

	statuses := make([]QuotaStatus, 0, len(ordered))
	for _, configured := range ordered {
		statuses = append(statuses, s.quotaStatus(ctx, configured, now))
	}
	return statuses
}

func (s *Store) quotaStatus(ctx context.Context, configured quota.Quota, now time.Time) QuotaStatus {
	loc := s.reportingLocation()
	start, end := quota.PeriodFor(now, configured.ResetDay, loc)
	status := QuotaStatus{
		Provider:    configured.Provider,
		Model:       configured.Model,
		Label:       configured.Label,
		ResetDay:    configured.ResetDay,
		PeriodStart: start.In(loc).Format("2006-01-02"),
		PeriodEnd:   end.In(loc).Format("2006-01-02"),
		Daily:       []QuotaPoint{},
	}

	query := `SELECT timestamp, total_tokens, cost FROM usage_events
WHERE timestamp >= ? AND timestamp < ?`
	args := []any{start.UTC().Format(time.RFC3339), end.UTC().Format(time.RFC3339)}
	if configured.Provider != "" {
		query += ` AND provider = ?`
		args = append(args, configured.Provider)
	}
	if configured.Model != "" {
		query += ` AND (canonical_model = ? OR raw_model = ?)`
		args = append(args, configured.Model, configured.Model)
	}
	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		status.State = "unknown"
		return status
	}
	defer rows.Close()

	perDay := make(map[string]quotaDay)
	for rows.Next() {
		var timestamp string
		var total sql.NullInt64
		var cost sql.NullFloat64
		if err := rows.Scan(&timestamp, &total, &cost); err != nil {
			status.State = "unknown"
			return status
		}
		parsed, err := timeParse(timestamp)
		if err != nil {
			status.State = "unknown"
			return status
		}
		day := perDay[parsed.In(loc).Format("2006-01-02")]
		day.events++
		if !total.Valid {
			day.unknownEvents++
		} else {
			day.tokens += total.Int64
		}
		if cost.Valid {
			day.cost += cost.Float64
			day.costKnown = true
		}
		perDay[parsed.In(loc).Format("2006-01-02")] = day
	}
	if err := rows.Err(); err != nil {
		status.State = "unknown"
		return status
	}

	// Walk every local day of the period so the line is continuous even for
	// quiet days.
	var tokensUsed int64
	var costUsed float64
	var costKnown bool
	var unknownEvents int64
	for day := start; day.Before(end); day = day.AddDate(0, 0, 1) {
		key := day.Format("2006-01-02")
		measured := perDay[key]
		tokensUsed += measured.tokens
		costUsed += measured.cost
		costKnown = costKnown || measured.costKnown
		unknownEvents += measured.unknownEvents
		point := QuotaPoint{Date: key, CumulativeTokens: tokensUsed}
		if costKnown {
			costCopy := costUsed
			point.CumulativeCost = &costCopy
		}
		status.Daily = append(status.Daily, point)
	}
	status.TokensUsed = tokensUsed
	status.UnknownTotalEvents = unknownEvents
	if costKnown {
		status.CostUsed = &costUsed
	}
	status.TokensBudget = configured.Budget.TokensPerMonth
	status.CostBudget = configured.Budget.CostPerMonth

	status.State = quotaState(status)
	if projected := quotaProjection(status, start, end, now, loc); projected != nil {
		status.ProjectedTokens = projected.tokens
		status.ProjectedCost = projected.cost
		status.ProjectedOverageDate = projected.overageDate
	}
	return status
}

// quotaDay is one local day's measured usage.
type quotaDay struct {
	events        int64
	unknownEvents int64
	tokens        int64
	cost          float64
	costKnown     bool
}

func quotaState(status QuotaStatus) string {
	over := false
	measured := false
	if status.TokensBudget != nil {
		measured = true
		if status.TokensUsed > *status.TokensBudget {
			over = true
		}
	}
	if status.CostBudget != nil {
		if status.CostUsed != nil {
			measured = true
			if *status.CostUsed > *status.CostBudget {
				over = true
			}
		}
	}
	switch {
	case over:
		return "over"
	case measured:
		return "ok"
	default:
		return "unknown"
	}
}

type quotaProjectionResult struct {
	tokens      *int64
	cost        *float64
	overageDate string
}

// quotaProjection scales the measured pace linearly across the full period
// and finds the first local day the pace crosses the tightest budget. It
// requires at least one day of elapsed usage; otherwise nothing is projected.
func quotaProjection(status QuotaStatus, start, end, now time.Time, loc *time.Location) *quotaProjectionResult {
	elapsed := now.Sub(start)
	period := end.Sub(start)
	if elapsed <= 0 || period <= 0 {
		return nil
	}
	result := &quotaProjectionResult{}
	if status.TokensBudget != nil && status.TokensUsed > 0 {
		projected := int64(float64(status.TokensUsed) * period.Hours() / elapsed.Hours())
		result.tokens = &projected
		// The pace crosses the budget when the projection exceeds it — even
		// if measured usage has not caught up yet.
		if projected > *status.TokensBudget || status.TokensUsed > *status.TokensBudget {
			result.overageDate = crossingDate(start, end, float64(*status.TokensBudget), float64(status.TokensUsed), elapsed.Hours(), loc)
		}
	}
	if status.CostBudget != nil && status.CostUsed != nil && *status.CostUsed > 0 {
		projected := *status.CostUsed * period.Hours() / elapsed.Hours()
		result.cost = &projected
		if projected > *status.CostBudget || *status.CostUsed > *status.CostBudget {
			if result.overageDate == "" {
				result.overageDate = crossingDate(start, end, *status.CostBudget, *status.CostUsed, elapsed.Hours(), loc)
			}
		}
	}
	if result.tokens == nil && result.cost == nil {
		return nil
	}
	return result
}

// crossingDate is the first local day the measured pace reaches the budget:
// budget/pace hours after the period start, floored to a whole day and
// capped at the period's last day.
func crossingDate(start, end time.Time, budget, used, elapsedHours float64, loc *time.Location) string {
	daysToCross := (budget / used) * elapsedHours / 24
	if daysToCross < 1 {
		daysToCross = 1
	}
	crossing := start.Add(time.Duration(daysToCross*24) * time.Hour).In(loc)
	lastDay := end.Add(-24 * time.Hour)
	if crossing.After(lastDay) {
		crossing = lastDay
	}
	return crossing.Format("2006-01-02")
}
