package database

import (
	"context"
	"database/sql"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tokemon/tokemon/internal/catalog"
	"github.com/tokemon/tokemon/internal/usage"
)

const (
	tokedexWeeklyTrendWeeks = 12
	tokedexRecentSessions   = 12
)

// ErrModelNotFound indicates that a model ID is neither in the catalog nor
// observed in stored usage_events.
var ErrModelNotFound = sql.ErrNoRows

// ComponentUsageTotals holds raw token component sums over a bounded calendar
// window for client-side what-if pricing comparisons.
type ComponentUsageTotals struct {
	StartDate          string `json:"start_date"`
	EndDate            string `json:"end_date"`
	Events             int64  `json:"events"`
	UnknownEvents      int64  `json:"unknown_events,omitempty"`
	ComponentEvents    int64  `json:"component_events"`
	InputTokens        int64  `json:"input_tokens"`
	CacheReadTokens    int64  `json:"cache_read_tokens"`
	CacheWriteTokens   int64  `json:"cache_write_tokens"`
	OutputTokens       int64  `json:"output_tokens"`
	ReasoningTokens    int64  `json:"reasoning_tokens,omitempty"`
	TotalTokens        int64  `json:"total_tokens"`
	UnclassifiedTokens int64  `json:"unclassified_tokens,omitempty"`
}

// TokedexWeeklyPoint is one Sunday-start local week in a model's 12-week trend.
type TokedexWeeklyPoint struct {
	WeekStart     string  `json:"week_start"`
	WeekEnd       string  `json:"week_end"`
	Label         string  `json:"label"`
	Tokens        int64   `json:"tokens"`
	Events        int64   `json:"events"`
	UnknownEvents int64   `json:"unknown_events,omitempty"`
	Cost          float64 `json:"cost,omitempty"`
	PricedTokens  int64   `json:"priced_tokens,omitempty"`
}

// TokedexMachineUsage summarizes one machine's usage of a model.
type TokedexMachineUsage struct {
	Machine       string  `json:"machine"`
	Tokens        int64   `json:"tokens"`
	Events        int64   `json:"events"`
	UnknownEvents int64   `json:"unknown_events,omitempty"`
	Sessions      int64   `json:"sessions"`
	Cost          float64 `json:"cost,omitempty"`
	Share         float64 `json:"share"`
	LastSeen      string  `json:"last_seen"`
}

// TokedexModelDetail aggregates metadata-only usage, pricing provenance, 12-week
// trend, machine distribution, and recent sessions for a single cataloged or
// observed uncataloged model.
type TokedexModelDetail struct {
	ID                      string                `json:"id"`
	RequestedID             string                `json:"requested_id,omitempty"`
	DisplayName             string                `json:"display_name"`
	Provider                string                `json:"provider"`
	Providers               []string              `json:"providers,omitempty"`
	Tier                    string                `json:"tier"`
	Aliases                 []string              `json:"aliases"`
	Cataloged               bool                  `json:"cataloged"`
	Priced                  bool                  `json:"priced"`
	Pricing                 catalog.Pricing       `json:"pricing"`
	Context                 string                `json:"context"`
	Events                  int64                 `json:"events"`
	UnknownEvents           int64                 `json:"unknown_events,omitempty"`
	Sessions                int64                 `json:"sessions"`
	FirstSeen               string                `json:"first_seen,omitempty"`
	LastSeen                string                `json:"last_seen,omitempty"`
	Tokens                  int64                 `json:"tokens"`
	InputTokens             int64                 `json:"input_tokens"`
	CacheReadTokens         int64                 `json:"cache_read_tokens"`
	CacheWriteTokens        int64                 `json:"cache_write_tokens"`
	OutputTokens            int64                 `json:"output_tokens"`
	ReasoningTokens         int64                 `json:"reasoning_tokens"`
	EstimatedCost           CostSummary           `json:"estimated_cost"`
	HasCost                 bool                  `json:"has_cost"`
	EffectiveCostPerMillion *float64              `json:"effective_cost_per_million,omitempty"`
	Cache                   CacheSummary          `json:"cache"`
	CacheSavings            float64               `json:"cache_savings,omitempty"`
	HasCacheSavings         bool                  `json:"has_cache_savings,omitempty"`
	WeeklyTrend             []TokedexWeeklyPoint  `json:"weekly_trend"`
	Machines                []TokedexMachineUsage `json:"machines"`
	RecentSessions          []SessionRecord       `json:"recent_sessions"`
}

// ComponentUsageTotals30d returns raw component token sums over the last 30
// local calendar days in the store's reporting timezone.
func (s *Store) ComponentUsageTotals30d(ctx context.Context) (ComponentUsageTotals, error) {
	return s.componentUsageTotals30dAt(ctx, time.Now())
}

func (s *Store) componentUsageTotals30dAt(ctx context.Context, now time.Time) (ComponentUsageTotals, error) {
	loc := s.reportingLocation()
	query, start, end := s.normalizeAnalyticsQuery(AnalyticsQuery{Period: "30d", Now: now})
	where, args := analyticsWhere(query, start, end)
	result := ComponentUsageTotals{
		StartDate: start.In(loc).Format("2006-01-02"),
		EndDate:   s.dateOnly(end.Add(-time.Nanosecond)).Format("2006-01-02"),
	}
	row := s.readHandle().QueryRowContext(ctx, `SELECT
COUNT(*),
COALESCE(SUM(CASE WHEN total_tokens IS NULL THEN 1 ELSE 0 END), 0),
COALESCE(SUM(CASE WHEN input_tokens IS NOT NULL OR output_tokens IS NOT NULL OR cache_read_tokens IS NOT NULL OR cache_write_tokens IS NOT NULL THEN 1 ELSE 0 END), 0),
COALESCE(SUM(input_tokens), 0),
COALESCE(SUM(cache_read_tokens), 0),
COALESCE(SUM(cache_write_tokens), 0),
COALESCE(SUM(output_tokens), 0),
COALESCE(SUM(reasoning_tokens), 0),
COALESCE(SUM(total_tokens), 0),
COALESCE(SUM(MAX(COALESCE(total_tokens, 0) - COALESCE(input_tokens, 0) - COALESCE(cache_read_tokens, 0) - COALESCE(cache_write_tokens, 0) - COALESCE(output_tokens, 0), 0)), 0)
FROM usage_events WHERE `+where, args...)
	if err := row.Scan(
		&result.Events,
		&result.UnknownEvents,
		&result.ComponentEvents,
		&result.InputTokens,
		&result.CacheReadTokens,
		&result.CacheWriteTokens,
		&result.OutputTokens,
		&result.ReasoningTokens,
		&result.TotalTokens,
		&result.UnclassifiedTokens,
	); err != nil {
		return ComponentUsageTotals{}, err
	}
	return result, nil
}

// TokedexModelDetail resolves modelID (canonical ID, catalog alias, or observed
// uncataloged identifier) and returns metadata-only usage detail, pricing
// provenance, 12-week trend, machine breakdown, and up to 12 recent sessions.
func (s *Store) TokedexModelDetail(ctx context.Context, modelID string) (TokedexModelDetail, error) {
	return s.TokedexModelDetailAt(ctx, modelID, time.Now())
}

// TokedexModelDetailAt is the deterministic-clock counterpart of
// TokedexModelDetail for calendar-bounded weekly trends.
func (s *Store) TokedexModelDetailAt(ctx context.Context, modelID string, now time.Time) (TokedexModelDetail, error) {
	rawID := strings.TrimSpace(modelID)
	if rawID == "" {
		return TokedexModelDetail{}, ErrModelNotFound
	}

	var (
		detail            TokedexModelDetail
		catModel          catalog.Model
		matchIDs          []string
		savingsEligible   bool
		savingsPerMillion float64
	)

	if s.catalog != nil {
		if canonicalID, model, ok := s.catalog.Resolve(rawID); ok {
			catModel = model
			detail = TokedexModelDetail{
				ID:          canonicalID,
				DisplayName: model.DisplayName,
				Provider:    model.Provider,
				Tier:        normalizeTier(model.Tier),
				Aliases:     append([]string(nil), model.Aliases...),
				Cataloged:   true,
				Priced:      model.Pricing.Mode == "standard" && model.Pricing.InputPricePerMillion != nil && model.Pricing.OutputPricePerMillion != nil,
				Pricing:     model.Pricing,
			}
			if rawID != canonicalID {
				detail.RequestedID = rawID
			}
			p := model.Pricing
			if p.Mode == "standard" && p.InputPricePerMillion != nil && p.CacheReadPricePerMillion != nil {
				diff := *p.InputPricePerMillion - *p.CacheReadPricePerMillion
				if diff < 0 {
					diff = 0
				}
				savingsEligible = true
				savingsPerMillion = diff
			}
			seenIDs := make(map[string]struct{}, len(model.Aliases)+1)
			seenIDs[canonicalID] = struct{}{}
			matchIDs = append(matchIDs, canonicalID)
			for _, alias := range model.Aliases {
				if _, exists := seenIDs[alias]; exists {
					continue
				}
				seenIDs[alias] = struct{}{}
				matchIDs = append(matchIDs, alias)
			}
		}
	}

	if !detail.Cataloged {
		detail = TokedexModelDetail{
			ID:          rawID,
			DisplayName: rawID,
			Tier:        "Uncataloged",
			Aliases:     []string{},
			Cataloged:   false,
			Priced:      false,
			Pricing:     catalog.Pricing{Currency: "USD", Mode: "unknown"},
		}
		matchIDs = []string{rawID}
	}

	placeholders := strings.Repeat("?,", len(matchIDs)-1) + "?"
	args := make([]any, len(matchIDs))
	for i, id := range matchIDs {
		args[i] = id
	}

	rows, err := s.readHandle().QueryContext(ctx, `SELECT
timestamp, machine_id, provider, tool, session_id,
input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, reasoning_tokens,
total_tokens, cost
FROM usage_events
WHERE COALESCE(NULLIF(canonical_model, ''), raw_model) IN (`+placeholders+`)`, args...)
	if err != nil {
		return TokedexModelDetail{}, err
	}
	defer rows.Close()

	loc := s.reportingLocation()
	today := s.dateOnly(now)
	currentWeekStart := today.AddDate(0, 0, -int(today.Weekday()))
	trendStart := currentWeekStart.AddDate(0, 0, -7*(tokedexWeeklyTrendWeeks-1))
	trendStartKey := trendStart.Format("2006-01-02")
	trendEndKey := currentWeekStart.Format("2006-01-02")

	type providerAccum struct {
		tokens int64
		events int64
	}
	type machineAccum struct {
		tokens        int64
		events        int64
		unknownEvents int64
		sessions      map[string]struct{}
		cost          float64
		lastSeen      string
		lastSeenTime  time.Time
		hasLastTime   bool
	}

	var (
		hasFirstTime, hasLastTime time.Time
		firstValid, lastValid     bool
		sessions                  = make(map[string]struct{})
		providers                 = make(map[string]*providerAccum)
		machines                  = make(map[string]*machineAccum)
		weekMap                   = make(map[string]*TokedexWeeklyPoint)
	)

	for rows.Next() {
		var ts, machine, provider, tool string
		var session sql.NullString
		var input, output, cacheRead, cacheWrite, reasoning, total sql.NullInt64
		var cost sql.NullFloat64
		if err := rows.Scan(
			&ts, &machine, &provider, &tool, &session,
			&input, &output, &cacheRead, &cacheWrite, &reasoning,
			&total, &cost,
		); err != nil {
			return TokedexModelDetail{}, err
		}

		detail.Events++
		if total.Valid {
			detail.Tokens += total.Int64
		} else {
			detail.UnknownEvents++
		}
		if input.Valid {
			detail.InputTokens += input.Int64
		}
		if cacheRead.Valid {
			detail.CacheReadTokens += cacheRead.Int64
		}
		if cacheWrite.Valid {
			detail.CacheWriteTokens += cacheWrite.Int64
		}
		if output.Valid {
			detail.OutputTokens += output.Int64
		}
		if reasoning.Valid {
			detail.ReasoningTokens += reasoning.Int64
		}

		parsedAt, parseErr := timeParse(ts)
		if detail.FirstSeen == "" {
			detail.FirstSeen = ts
			if parseErr == nil {
				hasFirstTime = parsedAt
				firstValid = true
			}
		} else if parseErr == nil && firstValid {
			if parsedAt.Before(hasFirstTime) {
				hasFirstTime = parsedAt
				detail.FirstSeen = ts
			}
		} else if ts < detail.FirstSeen {
			detail.FirstSeen = ts
		}
		if detail.LastSeen == "" {
			detail.LastSeen = ts
			if parseErr == nil {
				hasLastTime = parsedAt
				lastValid = true
			}
		} else if parseErr == nil && lastValid {
			if parsedAt.After(hasLastTime) {
				hasLastTime = parsedAt
				detail.LastSeen = ts
			}
		} else if ts > detail.LastSeen {
			detail.LastSeen = ts
		}

		var threadKey string
		hasSession := session.Valid && strings.TrimSpace(session.String) != ""
		if hasSession {
			threadKey = machine + "\x1f" + provider + "\x1f" + tool + "\x1f" + session.String
			sessions[threadKey] = struct{}{}
		}

		if provider != "" {
			pa := providers[provider]
			if pa == nil {
				pa = &providerAccum{}
				providers[provider] = pa
			}
			pa.events++
			if total.Valid {
				pa.tokens += total.Int64
			}
		}

		var rowCost float64
		var rowHasCost bool
		if cost.Valid {
			rowCost = cost.Float64
			rowHasCost = true
		} else if detail.Cataloged && detail.Priced && (input.Valid || output.Valid || cacheRead.Valid || cacheWrite.Valid) {
			est, ok := catalog.Estimate(usage.Event{
				InputTokens:      nullInt64(input),
				OutputTokens:     nullInt64(output),
				CacheReadTokens:  nullInt64(cacheRead),
				CacheWriteTokens: nullInt64(cacheWrite),
			}, catModel)
			if ok && est != nil {
				rowCost = *est
				rowHasCost = true
			}
		}

		if rowHasCost {
			detail.EstimatedCost.Amount += rowCost
			detail.HasCost = true
			if total.Valid {
				detail.EstimatedCost.PricedTokens += total.Int64
			}
		} else if total.Valid {
			detail.EstimatedCost.UnpricedTokens += total.Int64
		}

		if input.Valid && cacheRead.Valid {
			detail.Cache.CachedTokens += cacheRead.Int64
			detail.Cache.EligibleTokens += input.Int64 + cacheRead.Int64
		}
		if savingsEligible && cacheRead.Valid && rowHasCost && total.Valid {
			detail.CacheSavings += float64(cacheRead.Int64) * savingsPerMillion / 1_000_000
			detail.HasCacheSavings = true
		}

		if machine != "" {
			ma := machines[machine]
			if ma == nil {
				ma = &machineAccum{sessions: make(map[string]struct{})}
				machines[machine] = ma
			}
			ma.events++
			if total.Valid {
				ma.tokens += total.Int64
			} else {
				ma.unknownEvents++
			}
			if rowHasCost {
				ma.cost += rowCost
			}
			if hasSession {
				ma.sessions[threadKey] = struct{}{}
			}
			if ma.lastSeen == "" {
				ma.lastSeen = ts
				if parseErr == nil {
					ma.lastSeenTime = parsedAt
					ma.hasLastTime = true
				}
			} else if parseErr == nil && ma.hasLastTime {
				if parsedAt.After(ma.lastSeenTime) {
					ma.lastSeenTime = parsedAt
					ma.lastSeen = ts
				}
			} else if ts > ma.lastSeen {
				ma.lastSeen = ts
			}
		}

		if parseErr == nil {
			localDay := s.dateOnly(parsedAt.In(loc))
			wk := localDay.AddDate(0, 0, -int(localDay.Weekday()))
			wkKey := wk.Format("2006-01-02")
			if wkKey >= trendStartKey && wkKey <= trendEndKey {
				wp := weekMap[wkKey]
				if wp == nil {
					wp = &TokedexWeeklyPoint{WeekStart: wkKey}
					weekMap[wkKey] = wp
				}
				wp.Events++
				if total.Valid {
					wp.Tokens += total.Int64
				} else {
					wp.UnknownEvents++
				}
				if rowHasCost {
					wp.Cost += rowCost
					if total.Valid {
						wp.PricedTokens += total.Int64
					}
				}
			}
		}
	}
	if err := rows.Err(); err != nil {
		return TokedexModelDetail{}, err
	}

	if !detail.Cataloged && detail.Events == 0 {
		return TokedexModelDetail{}, ErrModelNotFound
	}

	detail.Sessions = int64(len(sessions))
	if detail.Events > 0 {
		detail.Context = "Observed in usage"
	} else {
		detail.Context = "No usage recorded yet"
	}
	if detail.Cache.EligibleTokens > 0 {
		detail.Cache.HitRate = float64(detail.Cache.CachedTokens) / float64(detail.Cache.EligibleTokens)
	}
	if detail.HasCost && detail.EstimatedCost.PricedTokens > 0 {
		rate := detail.EstimatedCost.Amount * 1_000_000 / float64(detail.EstimatedCost.PricedTokens)
		detail.EffectiveCostPerMillion = &rate
	} else {
		detail.CacheSavings = 0
		detail.HasCacheSavings = false
	}

	if len(providers) > 0 {
		providerList := make([]string, 0, len(providers))
		topProvider := ""
		var topAccum providerAccum
		for name, acc := range providers {
			providerList = append(providerList, name)
			if topProvider == "" || acc.tokens > topAccum.tokens ||
				(acc.tokens == topAccum.tokens && (acc.events > topAccum.events || (acc.events == topAccum.events && name < topProvider))) {
				topProvider = name
				topAccum = *acc
			}
		}
		sort.Strings(providerList)
		detail.Providers = providerList
		if !detail.Cataloged {
			detail.Provider = topProvider
		}
	}

	if len(machines) > 0 {
		detail.Machines = make([]TokedexMachineUsage, 0, len(machines))
		for name, ma := range machines {
			item := TokedexMachineUsage{
				Machine:       name,
				Tokens:        ma.tokens,
				Events:        ma.events,
				UnknownEvents: ma.unknownEvents,
				Sessions:      int64(len(ma.sessions)),
				Cost:          ma.cost,
				LastSeen:      ma.lastSeen,
			}
			if detail.Tokens > 0 {
				item.Share = float64(ma.tokens) / float64(detail.Tokens)
			}
			detail.Machines = append(detail.Machines, item)
		}
		sort.Slice(detail.Machines, func(left, right int) bool {
			if detail.Machines[left].Tokens != detail.Machines[right].Tokens {
				return detail.Machines[left].Tokens > detail.Machines[right].Tokens
			}
			if detail.Machines[left].Events != detail.Machines[right].Events {
				return detail.Machines[left].Events > detail.Machines[right].Events
			}
			return detail.Machines[left].Machine < detail.Machines[right].Machine
		})
	}

	if len(weekMap) > 0 {
		detail.WeeklyTrend = make([]TokedexWeeklyPoint, 0, tokedexWeeklyTrendWeeks)
		for i := 0; i < tokedexWeeklyTrendWeeks; i++ {
			wk := trendStart.AddDate(0, 0, i*7)
			wkKey := wk.Format("2006-01-02")
			pt := TokedexWeeklyPoint{
				WeekStart: wkKey,
				WeekEnd:   wk.AddDate(0, 0, 6).Format("2006-01-02"),
				Label:     wk.Format("Jan 2"),
			}
			if existing, ok := weekMap[wkKey]; ok {
				pt.Tokens = existing.Tokens
				pt.Events = existing.Events
				pt.UnknownEvents = existing.UnknownEvents
				pt.Cost = existing.Cost
				pt.PricedTokens = existing.PricedTokens
			}
			detail.WeeklyTrend = append(detail.WeeklyTrend, pt)
		}
	}

	if detail.Events > 0 {
		sessionRows, err := s.readHandle().QueryContext(ctx, sessionSelectSQL+
			`COALESCE(NULLIF(canonical_model, ''), raw_model) IN (`+placeholders+`) AND NULLIF(session_id, '') IS NOT NULL
GROUP BY session_id, machine_id, provider, tool
ORDER BY MAX(timestamp) DESC LIMIT `+strconv.Itoa(tokedexRecentSessions), args...)
		if err != nil {
			return TokedexModelDetail{}, err
		}
		defer sessionRows.Close()
		recent, err := scanSessionRecords(sessionRows)
		if err != nil {
			return TokedexModelDetail{}, err
		}
		detail.RecentSessions = recent
	}

	return detail, nil
}
