package database

import (
	"context"
	"database/sql"
	"sort"
	"strings"
	"time"

	"github.com/tokemon/tokemon/internal/quota"
)

const (
	QuotaSourceYAML   = "yaml"
	QuotaSourceSQLite = "sqlite"
)

// ConfiguredQuota is one effective quota definition together with its origin
// (YAML config vs SQLite UI store) and override metadata.
type ConfiguredQuota struct {
	quota.Quota
	Source        string `json:"source"`
	OverridesYAML bool   `json:"overrides_yaml,omitempty"`
	UpdatedAt     string `json:"updated_at,omitempty"`
}

func normalizeYAMLQuotas(quotas []quota.Quota) []quota.Quota {
	if len(quotas) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(quotas))
	out := make([]quota.Quota, 0, len(quotas))
	for _, q := range quotas {
		norm := quota.Normalize(q)
		key := quota.Key(norm.Provider, norm.Model)
		if _, exists := seen[key]; exists {
			continue
		}
		seen[key] = struct{}{}
		out = append(out, cloneQuota(norm))
	}
	return out
}

func cloneQuota(q quota.Quota) quota.Quota {
	if q.Budget.TokensPerMonth != nil {
		v := *q.Budget.TokensPerMonth
		q.Budget.TokensPerMonth = &v
	}
	if q.Budget.CostPerMonth != nil {
		v := *q.Budget.CostPerMonth
		q.Budget.CostPerMonth = &v
	}
	return q
}

func cloneConfiguredQuotas(items []ConfiguredQuota) []ConfiguredQuota {
	if len(items) == 0 {
		return nil
	}
	out := make([]ConfiguredQuota, len(items))
	for i, item := range items {
		item.Quota = cloneQuota(item.Quota)
		out[i] = item
	}
	return out
}

func orderedQuotaList(quotas []quota.Quota) []quota.Quota {
	ordered := make([]quota.Quota, len(quotas))
	for i, q := range quotas {
		ordered[i] = cloneQuota(q)
	}
	sort.SliceStable(ordered, func(left, right int) bool {
		return quota.Specificity(ordered[left]) < quota.Specificity(ordered[right])
	})
	return ordered
}

func orderedQuotaListFromConfigured(configured []ConfiguredQuota) []quota.Quota {
	ordered := make([]quota.Quota, len(configured))
	for i, item := range configured {
		ordered[i] = cloneQuota(item.Quota)
	}
	return ordered
}

func mergeQuotas(yamlQuotas []quota.Quota, sqliteRows []ConfiguredQuota) []ConfiguredQuota {
	sqliteByKey := make(map[string]ConfiguredQuota, len(sqliteRows))
	for _, row := range sqliteRows {
		sqliteByKey[quota.Key(row.Provider, row.Model)] = row
	}
	consumed := make(map[string]struct{}, len(sqliteRows))
	merged := make([]ConfiguredQuota, 0, len(yamlQuotas)+len(sqliteRows))
	for _, yq := range yamlQuotas {
		key := quota.Key(yq.Provider, yq.Model)
		if override, ok := sqliteByKey[key]; ok {
			override.Source = QuotaSourceSQLite
			override.OverridesYAML = true
			merged = append(merged, override)
			consumed[key] = struct{}{}
			continue
		}
		merged = append(merged, ConfiguredQuota{
			Quota:  cloneQuota(yq),
			Source: QuotaSourceYAML,
		})
	}
	for _, row := range sqliteRows {
		key := quota.Key(row.Provider, row.Model)
		if _, ok := consumed[key]; ok {
			continue
		}
		row.Source = QuotaSourceSQLite
		row.OverridesYAML = false
		merged = append(merged, row)
	}
	sort.SliceStable(merged, func(left, right int) bool {
		return quota.Specificity(merged[left].Quota) < quota.Specificity(merged[right].Quota)
	})
	return merged
}

func (s *Store) resetQuotaConfigAndCache() {
	s.quotaMu.Lock()
	s.quotaGeneration++
	s.quotaConfigLoaded = false
	s.mergedConfigCache = nil
	s.quotaCache = nil
	s.quotaMu.Unlock()
}

func (s *Store) effectiveQuotas(ctx context.Context) ([]ConfiguredQuota, error) {
	for {
		s.quotaMu.Lock()
		if s.quotaConfigLoaded {
			cloned := cloneConfiguredQuotas(s.mergedConfigCache)
			s.quotaMu.Unlock()
			return cloned, nil
		}
		gen := s.quotaGeneration
		s.quotaMu.Unlock()

		rows, err := s.readHandle().QueryContext(ctx, `SELECT provider, model, label, reset_day, tokens_per_month, cost_per_month, updated_at
FROM quotas
ORDER BY LOWER(provider), LOWER(model)`)
		if err != nil {
			return nil, err
		}
		var sqliteRows []ConfiguredQuota
		for rows.Next() {
			var item ConfiguredQuota
			var tokens sql.NullInt64
			var cost sql.NullFloat64
			if err := rows.Scan(&item.Provider, &item.Model, &item.Label, &item.ResetDay, &tokens, &cost, &item.UpdatedAt); err != nil {
				rows.Close()
				return nil, err
			}
			item.Quota = quota.Normalize(item.Quota)
			item.Budget.TokensPerMonth = nullInt64(tokens)
			item.Budget.CostPerMonth = nullFloat64(cost)
			item.Source = QuotaSourceSQLite
			sqliteRows = append(sqliteRows, item)
		}
		if err := rows.Close(); err != nil {
			return nil, err
		}
		if err := rows.Err(); err != nil {
			return nil, err
		}

		merged := mergeQuotas(s.quotas, sqliteRows)

		s.quotaMu.Lock()
		if s.quotaGeneration == gen {
			s.mergedConfigCache = cloneConfiguredQuotas(merged)
			s.quotaConfigLoaded = true
			s.quotaMu.Unlock()
			return merged, nil
		}
		s.quotaMu.Unlock()

		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
}

// Quotas returns the effective merged quota configuration (SQLite overrides
// YAML on matching normalized (provider, model) keys) sorted stably by
// specificity.
func (s *Store) Quotas(ctx context.Context) ([]ConfiguredQuota, error) {
	return s.effectiveQuotas(ctx)
}

// UpsertQuota validates, normalizes, and stores a quota in SQLite, overriding
// any matching YAML quota, and immediately invalidates Store quota caches.
func (s *Store) UpsertQuota(ctx context.Context, candidate quota.Quota) error {
	candidate = quota.Normalize(candidate)
	if err := quota.Validate(candidate); err != nil {
		return err
	}
	var costArg any
	if candidate.Budget.CostPerMonth != nil {
		costArg = *candidate.Budget.CostPerMonth
	}
	updatedAt := time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := s.db.ExecContext(ctx, `INSERT INTO quotas (
  provider, model, label, reset_day, tokens_per_month, cost_per_month, updated_at
) VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(provider, model) DO UPDATE SET
  provider = excluded.provider,
  model = excluded.model,
  label = excluded.label,
  reset_day = excluded.reset_day,
  tokens_per_month = excluded.tokens_per_month,
  cost_per_month = excluded.cost_per_month,
  updated_at = excluded.updated_at`,
		candidate.Provider, candidate.Model, candidate.Label, candidate.ResetDay,
		ptrValue(candidate.Budget.TokensPerMonth), costArg, updatedAt,
	); err != nil {
		return err
	}
	s.resetQuotaConfigAndCache()
	return nil
}

// DeleteQuota removes a SQLite-stored quota matching (provider, model) case-
// insensitively and immediately invalidates Store quota caches if a row was
// removed. Deleting an override restores any underlying YAML quota.
func (s *Store) DeleteQuota(ctx context.Context, provider, model string) error {
	provider = strings.TrimSpace(provider)
	model = strings.TrimSpace(model)
	res, err := s.db.ExecContext(ctx, `DELETE FROM quotas WHERE provider = ? AND model = ?`, provider, model)
	if err != nil {
		return err
	}
	if rows, err := res.RowsAffected(); err == nil && rows > 0 {
		s.resetQuotaConfigAndCache()
	}
	return nil
}

const (
	quotaCacheTTL           = time.Minute
	quotaMinRefreshInterval = 2 * time.Second
)

type warmupContextKey struct{}

// WithWarmupContext marks a context as originating from startup cache warmup
// rather than an external request. Unserved warmup entries are dropped on the
// first ingest invalidation so post-startup requests never observe stale-zero
// startup snapshots.
func WithWarmupContext(ctx context.Context) context.Context {
	return context.WithValue(ctx, warmupContextKey{}, true)
}

// IsWarmupContext reports whether ctx came from startup cache warmup.
func IsWarmupContext(ctx context.Context) bool {
	v, _ := ctx.Value(warmupContextKey{}).(bool)
	return v
}

type cachedQuotaBucket struct {
	configured  quota.Quota
	start       time.Time
	end         time.Time
	periodStart string
	periodEnd   string
	perDay      map[string]quotaDay
}

type quotaCacheEntry struct {
	localDay   string
	buckets    []cachedQuotaBucket
	fetched    time.Time
	refreshDue time.Time
	fromWarmup bool
}

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
	return s.quotaStatus(ctx, now, false)
}

// QuotaStatusFresh returns quota statuses, triggering and awaiting an
// in-flight SWR refresh when the cached bucket is stale so alert rechecks
// evaluate the freshly computed values without arbitrary sleep timers.
func (s *Store) QuotaStatusFresh(ctx context.Context, now time.Time) []QuotaStatus {
	return s.quotaStatus(ctx, now, true)
}

// SetQuotaRefreshDelayForTest injects an artificial delay into quota cache
// refreshes so tests can verify slow-refresh behavior deterministically.
func (s *Store) SetQuotaRefreshDelayForTest(delay time.Duration) {
	s.quotaMu.Lock()
	s.quotaRefreshDelay = delay
	s.quotaMu.Unlock()
}

func (s *Store) quotaStatus(ctx context.Context, now time.Time, awaitFresh bool) []QuotaStatus {
	loc := s.reportingLocation()
	configured, err := s.effectiveQuotas(ctx)
	if err != nil {
		if len(s.quotas) == 0 {
			return nil
		}
		return unknownQuotaStatuses(orderedQuotaList(s.quotas), loc, now)
	}
	if len(configured) == 0 {
		return nil
	}
	localDay := now.In(loc).Format("2006-01-02")
	const maxFreshAttempts = 4
	attempts := 0
	var lastEntry *quotaCacheEntry

	for {
		s.quotaMu.Lock()
		if cached := s.quotaCache; cached != nil && cached.localDay == localDay {
			if !IsWarmupContext(ctx) {
				cached.fromWarmup = false
			}
			stale := time.Since(cached.fetched) >= quotaCacheTTL ||
				(!cached.refreshDue.IsZero() && !time.Now().Before(cached.refreshDue))
			if stale && s.quotaReady == nil && (!awaitFresh || attempts < maxFreshAttempts) {
				s.startQuotaRefreshLocked(localDay, now)
			}
			entry := cached
			ready := s.quotaReady
			if awaitFresh && ready != nil && attempts < maxFreshAttempts {
				attempts++
				s.quotaMu.Unlock()
				select {
				case <-ready:
					configured, err = s.effectiveQuotas(ctx)
					if err != nil {
						if len(s.quotas) == 0 {
							return nil
						}
						return unknownQuotaStatuses(orderedQuotaList(s.quotas), loc, now)
					}
					if len(configured) == 0 {
						return nil
					}
					continue
				case <-ctx.Done():
					return buildQuotaStatuses(entry, loc, now)
				}
			}
			s.quotaMu.Unlock()
			return buildQuotaStatuses(entry, loc, now)
		}
		if ready := s.quotaReady; ready != nil {
			if awaitFresh && attempts >= maxFreshAttempts {
				s.quotaMu.Unlock()
				if lastEntry != nil {
					return buildQuotaStatuses(lastEntry, loc, now)
				}
				return unknownQuotaStatuses(orderedQuotaListFromConfigured(configured), loc, now)
			}
			if awaitFresh {
				attempts++
			}
			s.quotaMu.Unlock()
			select {
			case <-ready:
				configured, err = s.effectiveQuotas(ctx)
				if err != nil {
					if len(s.quotas) == 0 {
						return nil
					}
					return unknownQuotaStatuses(orderedQuotaList(s.quotas), loc, now)
				}
				if len(configured) == 0 {
					return nil
				}
				continue
			case <-ctx.Done():
				if lastEntry != nil {
					return buildQuotaStatuses(lastEntry, loc, now)
				}
				return unknownQuotaStatuses(orderedQuotaListFromConfigured(configured), loc, now)
			}
		}
		generation := s.quotaGeneration
		ready := make(chan struct{})
		s.quotaReady = ready
		s.quotaReadyDay = localDay
		s.quotaMu.Unlock()

		entry, ok := s.computeQuotaCacheEntry(ctx, localDay, now)

		s.quotaMu.Lock()
		superseded := s.quotaGeneration != generation
		if ok && !superseded {
			entry.fromWarmup = IsWarmupContext(ctx)
			s.quotaCache = entry
		}
		s.quotaReady = nil
		s.quotaReadyDay = ""
		s.quotaMu.Unlock()
		close(ready)

		if !ok {
			if lastEntry != nil {
				return buildQuotaStatuses(lastEntry, loc, now)
			}
			return unknownQuotaStatuses(orderedQuotaListFromConfigured(configured), loc, now)
		}
		lastEntry = entry
		if awaitFresh && superseded && attempts < maxFreshAttempts && ctx.Err() == nil {
			attempts++
			configured, err = s.effectiveQuotas(ctx)
			if err != nil {
				if len(s.quotas) == 0 {
					return nil
				}
				return unknownQuotaStatuses(orderedQuotaList(s.quotas), loc, now)
			}
			if len(configured) == 0 {
				return nil
			}
			continue
		}
		return buildQuotaStatuses(entry, loc, now)
	}
}

func (s *Store) startQuotaRefreshLocked(localDay string, now time.Time) {
	if !s.tryStartBackground() {
		return
	}
	s.quotaRefreshes.Add(1)
	generation := s.quotaGeneration
	ready := make(chan struct{})
	s.quotaReady = ready
	s.quotaReadyDay = localDay
	go func() {
		defer s.bgWG.Done()
		ctx, cancel := context.WithTimeout(s.backgroundContext(), 30*time.Second)
		defer cancel()

		entry, ok := s.computeQuotaCacheEntry(ctx, localDay, now)

		s.quotaMu.Lock()
		if ok && s.quotaGeneration == generation {
			s.quotaCache = entry
		}
		s.quotaReady = nil
		s.quotaReadyDay = ""
		s.quotaMu.Unlock()
		close(ready)
	}()
}

func (s *Store) invalidateQuotaCache() {
	now := time.Now()
	s.quotaMu.Lock()
	s.quotaGeneration++
	if s.quotaCache != nil {
		if s.quotaCache.fromWarmup {
			s.quotaCache = nil
		} else {
			due := now
			if !s.quotaCache.refreshDue.IsZero() && !s.quotaCache.refreshDue.After(now) {
				due = s.quotaCache.refreshDue
			} else if windowEnd := s.quotaCache.fetched.Add(quotaMinRefreshInterval); windowEnd.After(now) {
				due = windowEnd
			}
			s.quotaCache.refreshDue = due
		}
	}
	s.quotaMu.Unlock()
}

func (s *Store) computeQuotaCacheEntry(ctx context.Context, localDay string, now time.Time) (*quotaCacheEntry, bool) {
	s.quotaMu.Lock()
	delay := s.quotaRefreshDelay
	s.quotaMu.Unlock()
	if delay > 0 {
		timer := time.NewTimer(delay)
		select {
		case <-timer.C:
		case <-ctx.Done():
			timer.Stop()
			return nil, false
		}
	}
	configuredQuotas, err := s.effectiveQuotas(ctx)
	if err != nil {
		return nil, false
	}
	ordered := orderedQuotaListFromConfigured(configuredQuotas)
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
			},
		})
	}

	// One SQL aggregation covers the union of the periods; attribution happens
	// in Go so each aggregate bucket is counted at most once.
	aggregates, ok := s.quotaAggregates(ctx, windowStart, windowEnd, loc)
	if !ok {
		return nil, false
	}
	for _, agg := range aggregates {
		if index := mostSpecificQuota(measurements, agg); index >= 0 {
			measurements[index].addAggregate(agg)
		}
	}

	buckets := make([]cachedQuotaBucket, len(measurements))
	for index := range measurements {
		buckets[index] = cachedQuotaBucket{
			configured:  measurements[index].configured,
			start:       measurements[index].start,
			end:         measurements[index].end,
			periodStart: measurements[index].status.PeriodStart,
			periodEnd:   measurements[index].status.PeriodEnd,
			perDay:      measurements[index].perDay,
		}
	}
	return &quotaCacheEntry{
		localDay: localDay,
		buckets:  buckets,
		fetched:  time.Now(),
	}, true
}

func buildQuotaStatuses(entry *quotaCacheEntry, loc *time.Location, now time.Time) []QuotaStatus {
	statuses := make([]QuotaStatus, 0, len(entry.buckets))
	for _, bucket := range entry.buckets {
		m := quotaMeasurement{
			configured: bucket.configured,
			start:      bucket.start,
			end:        bucket.end,
			perDay:     bucket.perDay,
			status: QuotaStatus{
				Provider:    bucket.configured.Provider,
				Model:       bucket.configured.Model,
				Label:       bucket.configured.Label,
				ResetDay:    bucket.configured.ResetDay,
				PeriodStart: bucket.periodStart,
				PeriodEnd:   bucket.periodEnd,
				Daily:       []QuotaPoint{},
			},
		}
		m.fillStatus(loc, now)
		statuses = append(statuses, m.status)
	}
	return statuses
}

func unknownQuotaStatuses(ordered []quota.Quota, loc *time.Location, now time.Time) []QuotaStatus {
	statuses := make([]QuotaStatus, 0, len(ordered))
	for _, configured := range ordered {
		start, end := quota.PeriodFor(now, configured.ResetDay, loc)
		statuses = append(statuses, QuotaStatus{
			Provider:    configured.Provider,
			Model:       configured.Model,
			Label:       configured.Label,
			ResetDay:    configured.ResetDay,
			PeriodStart: start.In(loc).Format("2006-01-02"),
			PeriodEnd:   end.In(loc).Format("2006-01-02"),
			Daily:       []QuotaPoint{},
			State:       "unknown",
		})
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

// addAggregate folds one attributed SQL aggregate row into the quota's per-day totals.
func (m *quotaMeasurement) addAggregate(agg quotaAggregateRow) {
	day := m.perDay[agg.day]
	day.events += agg.events
	day.unknownEvents += agg.unknownEvents
	day.tokens += agg.tokens
	if agg.cost.Valid {
		day.cost += agg.cost.Float64
		day.costKnown = true
	}
	m.perDay[agg.day] = day
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

// mostSpecificQuota returns the index of the quota an aggregate row belongs to:
// the most specific matching quota whose current period contains the row's day,
// or -1 when no configured quota covers it. The measurements are ordered by
// ascending specificity, so the first match wins; equal ranks fall back to
// configuration order.
func mostSpecificQuota(measurements []quotaMeasurement, agg quotaAggregateRow) int {
	for index := range measurements {
		if measurements[index].matchesEvent(agg.provider, agg.canonical, agg.raw) &&
			measurements[index].containsDay(agg.day) {
			return index
		}
	}
	return -1
}

type quotaAggregateRow struct {
	day           string
	provider      string
	canonical     string
	raw           string
	tokens        int64
	unknownEvents int64
	cost          sql.NullFloat64
	events        int64
}

// quotaAggregates aggregates usage rows covering [start, end) in SQL by
// (local_day, provider, canonical_model, raw_model). Local-day boundaries are
// formatted in UTC without a trailing 'Z' ("2006-01-02T15:04:05") so ASCII
// lexicographic comparison handles sub-second fractional timestamps and DST
// transitions identically to Go's time.In(loc). Non-Z timestamps are scanned
// with a +/-24h window to cover all local offsets and filtered/bucketed in Go.
// ok is false when rows cannot be read or parsed.
func (s *Store) quotaAggregates(ctx context.Context, start, end time.Time, loc *time.Location) ([]quotaAggregateRow, bool) {
	if !end.After(start) {
		return nil, true
	}
	var caseSQL strings.Builder
	args := make([]any, 0, 74)
	caseSQL.WriteString("CASE WHEN substr(timestamp, -1) != 'Z' THEN timestamp")
	for day := start; day.Before(end); {
		nextDay := time.Date(day.Year(), day.Month(), day.Day()+1, 0, 0, 0, 0, loc)
		if !nextDay.Before(end) {
			caseSQL.WriteString(" ELSE ?")
			args = append(args, day.Format("2006-01-02"))
			break
		}
		caseSQL.WriteString(" WHEN timestamp < ? THEN ?")
		args = append(args, nextDay.UTC().Format("2006-01-02T15:04:05"), day.Format("2006-01-02"))
		day = nextDay
	}
	caseSQL.WriteString(" END")

	startStr := start.UTC().Format("2006-01-02T15:04:05")
	endStr := end.UTC().Format("2006-01-02T15:04:05")
	paddedStartStr := start.Add(-24 * time.Hour).UTC().Format("2006-01-02T15:04:05")
	paddedEndStr := end.Add(24 * time.Hour).UTC().Format("2006-01-02T15:04:05")
	args = append(args, paddedStartStr, paddedEndStr, startStr, endStr)

	query := `SELECT ` + caseSQL.String() + `, provider, canonical_model, raw_model,
COALESCE(SUM(total_tokens), 0),
SUM(CASE WHEN total_tokens IS NULL THEN 1 ELSE 0 END),
SUM(cost),
COUNT(*)
FROM usage_events
WHERE timestamp >= ? AND timestamp < ?
  AND (substr(timestamp, -1) != 'Z' OR (timestamp >= ? AND timestamp < ?))
GROUP BY 1, provider, canonical_model, raw_model`

	rows, err := s.readHandle().QueryContext(ctx, query, args...)
	if err != nil {
		return nil, false
	}
	defer rows.Close()
	var result []quotaAggregateRow
	for rows.Next() {
		var row quotaAggregateRow
		if err := rows.Scan(&row.day, &row.provider, &row.canonical, &row.raw,
			&row.tokens, &row.unknownEvents, &row.cost, &row.events); err != nil {
			return nil, false
		}
		if len(row.day) != 10 {
			parsed, err := timeParse(row.day)
			if err != nil {
				return nil, false
			}
			if parsed.Before(start) || !parsed.Before(end) {
				continue
			}
			row.day = parsed.In(loc).Format("2006-01-02")
		}
		result = append(result, row)
	}
	if err := rows.Err(); err != nil {
		return nil, false
	}
	return result, true
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
