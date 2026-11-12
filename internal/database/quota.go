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
// store's reporting timezone. Each usage event is attributed to exactly one
// quota — the most specific match — so overlapping quotas never double-count
// an event. With no quotas configured it returns nil — the dashboard renders
// an explicit Unknown state for that case itself.
func (s *Store) QuotaStatus(ctx context.Context, now time.Time) []QuotaStatus {
	if len(s.quotas) == 0 {
		return nil
	}
	ordered := make([]quota.Quota, len(s.quotas))
	copy(ordered, s.quotas)
	sort.SliceStable(ordered, func(left, right int) bool {
		return quota.Specificity(ordered[left]) < quota.Specificity(ordered[right])
	})

	loc := s.reportingLocation()
	measurements := make([]quotaMeasurement, 0, len(ordered))
	var windowStart, windowEnd time.Time
	for _, configured := range ordered {
		start, end := quota.PeriodFor(now, configured.ResetDay, loc)
		if windowStart.IsZero() || start.Before(windowStart) {
			windowStart = start
		}
		if windowEnd.IsZero() || end.After(windowEnd) {
			windowEnd = end
		}
		measurements = append(measurements, quotaMeasurement{
			configured: configured,
			start:      start,
			end:        end,
			perDay:     make(map[string]quotaDay),
			status: QuotaStatus{
				Provider:    configured.Provider,
				Model:       configured.Model,
				Label:       configured.Label,
				ResetDay:    configured.ResetDay,
				PeriodStart: start.In(loc).Format("2006-01-02"),
				PeriodEnd:   end.In(loc).Format("2006-01-02"),
				Daily:       []QuotaPoint{},
			},
		})
	}

	// One fetch covers the union of the periods; attribution happens in Go so
	// each event is counted at most once. The measurement windows are
	// candidates too: with mismatched reset days a specific quota's period can
	// miss a day its broader quota covers, and the event then falls through to
	// the broader quota instead of vanishing from every budget.
	events, ok := s.quotaEvents(ctx, windowStart, windowEnd, loc)
	if !ok {
		for index := range measurements {
			measurements[index].status.State = "unknown"
		}
	} else {
		for _, event := range events {
			if index := mostSpecificQuota(measurements, event); index >= 0 {
				measurements[index].add(event)
			}
		}
		for index := range measurements {
			measurements[index].fillStatus(loc, now)
		}
	}

	statuses := make([]QuotaStatus, 0, len(measurements))
	for index := range measurements {
		statuses = append(statuses, measurements[index].status)
	}
	return statuses
}

// quotaMeasurement accumulates one quota's attributed usage before the
// status is filled in.
type quotaMeasurement struct {
	configured quota.Quota
	start, end time.Time
	perDay     map[string]quotaDay
	status     QuotaStatus
}

// matchesEvent reports whether the quota applies to an event with the given
// provider and model in either stored form (canonical or raw).
func (m *quotaMeasurement) matchesEvent(provider, canonical, raw string) bool {
	return quota.Match(m.configured, provider, canonical) || quota.Match(m.configured, provider, raw)
}

// containsDay reports whether the quota's current period covers a local day.
func (m *quotaMeasurement) containsDay(day string) bool {
	return day >= m.status.PeriodStart && day < m.status.PeriodEnd
}

// add folds one attributed event into the quota's per-day totals.
func (m *quotaMeasurement) add(event quotaEvent) {
	day := m.perDay[event.day]
	day.events++
	if !event.total.Valid {
		day.unknownEvents++
	} else {
		day.tokens += event.total.Int64
	}
	if event.cost.Valid {
		day.cost += event.cost.Float64
		day.costKnown = true
	}
	m.perDay[event.day] = day
}

// fillStatus folds the per-day usage into the status: a continuous daily
// cumulative line across the period, the totals, and the state + projection.
func (m *quotaMeasurement) fillStatus(loc *time.Location, now time.Time) {
	// Walk every local day of the period so the line is continuous even for
	// quiet days.
	var tokensUsed int64
	var costUsed float64
	var costKnown bool
	var unknownEvents int64
	for day := m.start; day.Before(m.end); day = day.AddDate(0, 0, 1) {
		key := day.Format("2006-01-02")
		measured := m.perDay[key]
		tokensUsed += measured.tokens
		costUsed += measured.cost
		costKnown = costKnown || measured.costKnown
		unknownEvents += measured.unknownEvents
		point := QuotaPoint{Date: key, CumulativeTokens: tokensUsed}
		if costKnown {
			costCopy := costUsed
			point.CumulativeCost = &costCopy
		}
		m.status.Daily = append(m.status.Daily, point)
	}
	m.status.TokensUsed = tokensUsed
	m.status.UnknownTotalEvents = unknownEvents
	if costKnown {
		m.status.CostUsed = &costUsed
	}
	m.status.TokensBudget = m.configured.Budget.TokensPerMonth
	m.status.CostBudget = m.configured.Budget.CostPerMonth

	m.status.State = quotaState(m.status)
	if projected := quotaProjection(m.status, m.start, m.end, now, loc); projected != nil {
		m.status.ProjectedTokens = projected.tokens
		m.status.ProjectedCost = projected.cost
		m.status.ProjectedOverageDate = projected.overageDate
	}
}

// mostSpecificQuota returns the index of the quota an event belongs to: the
// most specific matching quota whose current period contains the event's day,
// or -1 when no configured quota covers it. The measurements are ordered by
// ascending specificity, so the first match wins; equal ranks fall back to
// configuration order.
func mostSpecificQuota(measurements []quotaMeasurement, event quotaEvent) int {
	for index := range measurements {
		if measurements[index].matchesEvent(event.provider, event.canonical, event.raw) &&
			measurements[index].containsDay(event.day) {
			return index
		}
	}
	return -1
}

// quotaEvent is one usage row inside the union quota window, pre-parsed to
// its reporting-timezone day for attribution.
type quotaEvent struct {
	day       string
	provider  string
	canonical string
	raw       string
	total     sql.NullInt64
	cost      sql.NullFloat64
}

// quotaEvents loads the usage rows covering the union of the configured
// periods. ok is false when the rows cannot be read or parsed: every quota
// then reports unknown rather than a partial measurement.
func (s *Store) quotaEvents(ctx context.Context, start, end time.Time, loc *time.Location) ([]quotaEvent, bool) {
	rows, err := s.db.QueryContext(ctx, `SELECT timestamp, provider, canonical_model, raw_model, total_tokens, cost FROM usage_events
WHERE timestamp >= ? AND timestamp < ?`,
		start.UTC().Format(time.RFC3339), end.UTC().Format(time.RFC3339))
	if err != nil {
		return nil, false
	}
	defer rows.Close()
	var events []quotaEvent
	for rows.Next() {
		var timestamp string
		var event quotaEvent
		if err := rows.Scan(&timestamp, &event.provider, &event.canonical, &event.raw, &event.total, &event.cost); err != nil {
			return nil, false
		}
		parsed, err := timeParse(timestamp)
		if err != nil {
			return nil, false
		}
		event.day = parsed.In(loc).Format("2006-01-02")
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, false
	}
	return events, true
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
