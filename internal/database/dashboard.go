package database

import (
	"context"
	"database/sql"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/tokemon/tokemon/internal/catalog"
	"github.com/tokemon/tokemon/internal/usage"
)

// Machine connection windows are deliberately short so a missed heartbeat is
// visible without treating a temporarily sleeping laptop as permanently gone.
const (
	MachineConnectedWindow = 5 * time.Minute
	MachineStaleWindow     = 30 * time.Minute
)

const (
	MachineStatusConnected = "Connected"
	MachineStatusStale     = "Stale"
	MachineStatusOffline   = "Offline"
)

// PeriodSummary is the small today/week context shown beside the global
// lifetime counter. Unknown totals remain explicit instead of being coerced
// to zero in the UI.
type PeriodSummary struct {
	StartDate     string      `json:"start_date"`
	EndDate       string      `json:"end_date"`
	Tokens        int64       `json:"tokens"`
	Events        int64       `json:"events"`
	UnknownTokens int64       `json:"unknown_tokens,omitempty"`
	InputTokens   int64       `json:"input_tokens"`
	OutputTokens  int64       `json:"output_tokens"`
	EstimatedCost CostSummary `json:"estimated_cost"`
}

// SessionRecord is an aggregated, metadata-only session row. Every field is
// sourced from normalized usage metadata; transcript content is never queried.
type SessionRecord struct {
	SessionID     string         `json:"session_id"`
	Timestamp     string         `json:"timestamp"`
	Machine       string         `json:"machine"`
	Provider      string         `json:"provider"`
	Model         string         `json:"model"`
	Tool          string         `json:"tool"`
	Project       string         `json:"project,omitempty"`
	InputTokens   *int64         `json:"input_tokens,omitempty"`
	OutputTokens  *int64         `json:"output_tokens,omitempty"`
	TotalTokens   *int64         `json:"total_tokens,omitempty"`
	Cost          *float64       `json:"cost,omitempty"`
	CostEstimated bool           `json:"cost_estimated,omitempty"`
	DurationMS    *int64         `json:"duration_ms,omitempty"`
	Accuracy      usage.Accuracy `json:"accuracy"`
	Events        int64          `json:"events"`
}

// CatalogEntry joins the bundled YAML model metadata with observed usage
// context. The aliases and pricing provenance are safe, non-secret metadata.
type CatalogEntry struct {
	ID          string          `json:"id"`
	Provider    string          `json:"provider"`
	DisplayName string          `json:"display_name"`
	Aliases     []string        `json:"aliases"`
	Tier        string          `json:"tier"`
	Pricing     catalog.Pricing `json:"pricing"`
	UsageTokens int64           `json:"usage_tokens"`
	UsageEvents int64           `json:"usage_events"`
	Context     string          `json:"context"`
}

type CatalogSnapshot struct {
	SchemaVersion  string             `json:"schema_version"`
	Models         []CatalogEntry     `json:"models"`
	Tiers          []string           `json:"tiers"`
	Uncataloged    []UncatalogedModel `json:"uncataloged,omitempty"`
	ObservedTokens int64              `json:"observed_tokens,omitempty"`
	PricedTokens   int64              `json:"priced_tokens,omitempty"`
}

// UncatalogedModel is a model identifier observed in usage that the bundled
// catalog does not know. Its tokens still count toward totals but contribute
// no cost estimate.
type UncatalogedModel struct {
	ID     string `json:"id"`
	Tokens int64  `json:"tokens"`
	Events int64  `json:"events"`
}

func (s *Store) PeriodSummary(ctx context.Context, start, end time.Time) (PeriodSummary, error) {
	start = s.dateOnly(start)
	end = s.dateOnly(end)
	result := PeriodSummary{
		StartDate: start.Format("2006-01-02"),
		EndDate:   end.Add(-time.Nanosecond).Format("2006-01-02"),
	}
	if !end.After(start) {
		return result, nil
	}
	row := s.db.QueryRowContext(ctx, `SELECT
COUNT(*),
COALESCE(SUM(CASE WHEN total_tokens IS NOT NULL THEN total_tokens ELSE 0 END), 0),
COALESCE(SUM(CASE WHEN total_tokens IS NULL THEN 1 ELSE 0 END), 0),
COALESCE(SUM(CASE WHEN input_tokens IS NOT NULL THEN input_tokens ELSE 0 END), 0),
COALESCE(SUM(CASE WHEN output_tokens IS NOT NULL THEN output_tokens ELSE 0 END), 0),
COALESCE(SUM(CASE WHEN cost IS NOT NULL THEN cost ELSE 0 END), 0),
COALESCE(SUM(CASE WHEN cost IS NOT NULL AND total_tokens IS NOT NULL THEN total_tokens ELSE 0 END), 0),
COALESCE(SUM(CASE WHEN cost IS NULL AND total_tokens IS NOT NULL THEN total_tokens ELSE 0 END), 0)
FROM usage_events WHERE timestamp >= ? AND timestamp < ?`,
		start.UTC().Format(time.RFC3339), end.UTC().Format(time.RFC3339))
	if err := row.Scan(&result.Events, &result.Tokens, &result.UnknownTokens, &result.InputTokens, &result.OutputTokens,
		&result.EstimatedCost.Amount, &result.EstimatedCost.PricedTokens, &result.EstimatedCost.UnpricedTokens); err != nil {
		return PeriodSummary{}, err
	}
	return result, nil
}

// NormalizeSessionSort validates the session-table sort request. Unknown or
// missing values fall back to the timestamp default.
func NormalizeSessionSort(sort, dir string) (string, string) {
	switch sort {
	case "when", "tokens", "cost", "duration":
	default:
		sort = "when"
	}
	if dir != "asc" && dir != "desc" {
		dir = "desc"
	}
	return sort, dir
}

// sessionOrderBy builds the session-table ORDER BY from a validated sort key.
// Only whitelisted literals reach the query; rows with an unknown sort value
// (NULL aggregate) sort last regardless of direction, and timestamp breaks ties.
func sessionOrderBy(sort, dir string) string {
	expressions := map[string]string{
		"when":     "MAX(timestamp)",
		"tokens":   "CASE WHEN COUNT(total_tokens) = COUNT(*) THEN SUM(total_tokens) END",
		"cost":     "CASE WHEN COUNT(cost) = 0 THEN NULL ELSE SUM(cost) END",
		"duration": "CASE WHEN COUNT(duration_ms) = COUNT(*) THEN SUM(duration_ms) END",
	}
	column := expressions[sort]
	return "ORDER BY " + column + " IS NULL ASC, " + column + " " + strings.ToUpper(dir) + ", MAX(timestamp) DESC LIMIT 100"
}

func (s *Store) Sessions(ctx context.Context, query AnalyticsQuery) ([]SessionRecord, error) {
	return s.SessionsWithSort(ctx, query, "when", "desc")
}

// SessionsWithSort returns the session table ordered by the validated sort key
// (when, tokens, cost, or duration) in the requested direction.
func (s *Store) SessionsWithSort(ctx context.Context, query AnalyticsQuery, sort, dir string) ([]SessionRecord, error) {
	sort, dir = NormalizeSessionSort(sort, dir)
	query, start, end := s.normalizeAnalyticsQuery(query)
	where, args := analyticsWhere(query, start, end)
	rows, err := s.db.QueryContext(ctx, `SELECT
MAX(timestamp),
MAX(NULLIF(session_id, '')),
machine_id,
provider,
COALESCE(NULLIF(MAX(NULLIF(canonical_model, '')), ''), MAX(raw_model)),
MAX(tool),
MAX(project),
CASE WHEN COUNT(input_tokens) = COUNT(*) THEN SUM(input_tokens) END,
CASE WHEN COUNT(output_tokens) = COUNT(*) THEN SUM(output_tokens) END,
CASE WHEN COUNT(total_tokens) = COUNT(*) THEN SUM(total_tokens) END,
CASE WHEN COUNT(cost) = 0 THEN NULL ELSE SUM(cost) END,
MAX(cost_estimated),
CASE WHEN COUNT(duration_ms) = COUNT(*) THEN SUM(duration_ms) END,
CASE WHEN COUNT(DISTINCT token_accuracy) = 1 THEN MAX(token_accuracy) ELSE 'unknown' END,
COUNT(*)
FROM usage_events WHERE `+where+` AND NULLIF(session_id, '') IS NOT NULL
GROUP BY session_id, machine_id, provider, tool
`+sessionOrderBy(sort, dir), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []SessionRecord
	for rows.Next() {
		var item SessionRecord
		var input, output, total, duration sql.NullInt64
		var cost sql.NullFloat64
		if err := rows.Scan(&item.Timestamp, &item.SessionID, &item.Machine, &item.Provider, &item.Model, &item.Tool, &item.Project,
			&input, &output, &total, &cost, &item.CostEstimated, &duration, &item.Accuracy, &item.Events); err != nil {
			return nil, err
		}
		item.InputTokens = nullInt64(input)
		item.OutputTokens = nullInt64(output)
		item.TotalTokens = nullInt64(total)
		item.Cost = nullFloat64(cost)
		item.DurationMS = nullInt64(duration)
		result = append(result, item)
	}
	return result, rows.Err()
}

// MachineSourceHealth is one source's connector health as of the machine's
// last health-aware heartbeat. State uses the dashboard's freshness
// vocabulary (Fresh/Stale/Unknown) for the last successful sync; a non-empty
// LastErrorClass is the coarse token the agent reported for its latest
// collection attempt. SourceHash is the agent-side identity hash — the hub
// never receives raw local paths.
type MachineSourceHealth struct {
	Adapter            string  `json:"adapter"`
	SourceHash         string  `json:"source_hash"`
	LastSuccessfulSync *string `json:"last_successful_sync,omitempty"`
	LastErrorClass     string  `json:"last_error_class,omitempty"`
	State              string  `json:"state"`
	Age                string  `json:"age"`
}

// machineSourceHealthState derives the per-source display state from the last
// successful sync, reusing the machine stale window so the pill, machine
// status, and source rows agree on what "stale" means.
func machineSourceHealthState(lastSync string, now time.Time) (state, age string) {
	seen, err := timeParse(lastSync)
	if err != nil {
		return "Unknown", "never synced"
	}
	elapsed := now.UTC().Sub(seen.UTC())
	if elapsed < 0 {
		elapsed = 0
	}
	label := syncAgeLabel(elapsed) + " ago"
	if elapsed <= MachineStaleWindow {
		return "Fresh", label
	}
	return "Stale", label
}

// syncAgeLabel mirrors the dashboard freshness pill's duration formatting so
// per-source ages read like the freshness readout.
func syncAgeLabel(elapsed time.Duration) string {
	ms := elapsed.Milliseconds()
	switch {
	case ms < 1000:
		return fmt.Sprintf("%dms", ms)
	case ms < 60_000:
		return fmt.Sprintf("%.1fs", float64(ms)/1000)
	}
	seconds := (ms + 500) / 1000
	if seconds < 3600 {
		return fmt.Sprintf("%dm %02ds", seconds/60, seconds%60)
	}
	return fmt.Sprintf("%dh %02dm", seconds/3600, seconds%3600/60)
}

func (s *Store) MachinesAt(ctx context.Context, now time.Time) ([]MachineInfo, error) {
	now = now.In(s.reportingLocation())
	today := s.dateOnly(now)
	weekStart := today.AddDate(0, 0, -int(today.Weekday()))
	usageRows, err := s.db.QueryContext(ctx, `SELECT machine_id,
COALESCE(SUM(total_tokens), 0),
COALESCE(SUM(CASE WHEN timestamp >= ? AND timestamp < ? THEN total_tokens ELSE 0 END), 0),
COALESCE(SUM(CASE WHEN timestamp >= ? AND timestamp < ? THEN total_tokens ELSE 0 END), 0)
FROM usage_events GROUP BY machine_id`,
		today.UTC().Format(time.RFC3339), today.AddDate(0, 0, 1).UTC().Format(time.RFC3339),
		weekStart.UTC().Format(time.RFC3339), today.AddDate(0, 0, 1).UTC().Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	type usageTotals struct{ lifetime, today, week int64 }
	byMachine := make(map[string]usageTotals)
	for usageRows.Next() {
		var id string
		var totals usageTotals
		if err := usageRows.Scan(&id, &totals.lifetime, &totals.today, &totals.week); err != nil {
			usageRows.Close()
			return nil, err
		}
		byMachine[id] = totals
	}
	if err := usageRows.Close(); err != nil {
		return nil, err
	}

	rows, err := s.db.QueryContext(ctx, `SELECT id, name, operating_system, architecture, agent_version,
detected_adapters, source_count, source_error_count, first_seen_at, last_seen_at
FROM machines ORDER BY last_seen_at DESC, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []MachineInfo
	for rows.Next() {
		var machine MachineInfo
		var adapterList string
		if err := rows.Scan(&machine.ID, &machine.Name, &machine.OperatingSystem, &machine.Architecture,
			&machine.AgentVersion, &adapterList, &machine.SourceCount, &machine.SourceErrorCount,
			&machine.FirstSeenAt, &machine.LastSeenAt); err != nil {
			return nil, err
		}
		machine.DetectedAdapters = splitAdapterIDs(adapterList)
		totals := byMachine[machine.ID]
		machine.LifetimeTokens = totals.lifetime
		machine.TodayTokens = totals.today
		machine.WeekTokens = totals.week
		machine.Status = machineStatus(machine.LastSeenAt, now)
		if machine.SourceErrorCount > 0 {
			machine.SyncContext = fmt.Sprintf("%d source error%s", machine.SourceErrorCount, pluralSuffix(machine.SourceErrorCount))
		} else if machine.SourceCount > 0 {
			machine.SyncContext = fmt.Sprintf("%d source%s synced", machine.SourceCount, pluralSuffix(machine.SourceCount))
		} else {
			machine.SyncContext = "Heartbeat metadata only"
		}
		result = append(result, machine)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	health, err := s.machineSourceHealth(ctx, now)
	if err != nil {
		return nil, err
	}
	for index := range result {
		result[index].Sources = health[result[index].ID]
	}
	return result, nil
}

// machineSourceHealth loads every machine's per-source connector health and
// derives the display state and age. Machines whose agents predate the field
// have no rows and keep their machine-level sync context only.
func (s *Store) machineSourceHealth(ctx context.Context, now time.Time) (map[string][]MachineSourceHealth, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT machine_id, adapter, source_hash, last_successful_sync, last_error_class
FROM machine_source_health ORDER BY machine_id, adapter, source_hash`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	health := make(map[string][]MachineSourceHealth)
	for rows.Next() {
		var item MachineSourceHealth
		var machineID, syncedAt string
		if err := rows.Scan(&machineID, &item.Adapter, &item.SourceHash, &syncedAt, &item.LastErrorClass); err != nil {
			return nil, err
		}
		item.LastErrorClass = strings.TrimSpace(item.LastErrorClass)
		if strings.TrimSpace(syncedAt) != "" {
			synced := syncedAt
			item.LastSuccessfulSync = &synced
		}
		item.State, item.Age = machineSourceHealthState(syncedAt, now)
		health[machineID] = append(health[machineID], item)
	}
	return health, rows.Err()
}

func machineStatus(lastSeen string, now time.Time) string {
	seen, err := timeParse(lastSeen)
	if err != nil {
		return MachineStatusOffline
	}
	age := now.UTC().Sub(seen.UTC())
	if age < 0 || age <= MachineConnectedWindow {
		return MachineStatusConnected
	}
	if age <= MachineStaleWindow {
		return MachineStatusStale
	}
	return MachineStatusOffline
}

func pluralSuffix(value int) string {
	if value == 1 {
		return ""
	}
	return "s"
}

func (s *Store) CatalogSnapshot(ctx context.Context) (CatalogSnapshot, error) {
	result := CatalogSnapshot{SchemaVersion: catalog.SchemaVersion, Tiers: []string{"S", "A", "B", "C", "D", "Unranked"}}
	if s.catalog == nil {
		return result, nil
	}
	usageRows, err := s.db.QueryContext(ctx, `SELECT COALESCE(NULLIF(canonical_model, ''), raw_model),
COALESCE(SUM(total_tokens), 0), COUNT(*), COALESCE(SUM(CASE WHEN total_tokens IS NULL THEN 1 ELSE 0 END), 0)
FROM usage_events GROUP BY 1`)
	if err != nil {
		return CatalogSnapshot{}, err
	}
	type usageContext struct{ tokens, events, unknown int64 }
	observed := make(map[string]usageContext)
	for usageRows.Next() {
		var id string
		var context usageContext
		if err := usageRows.Scan(&id, &context.tokens, &context.events, &context.unknown); err != nil {
			usageRows.Close()
			return CatalogSnapshot{}, err
		}
		observed[id] = context
	}
	if err := usageRows.Close(); err != nil {
		return CatalogSnapshot{}, err
	}
	// Usage recorded under a raw alias folds into its catalog model so late
	// catalog additions and alias-form identifiers stay attributed; whatever
	// remains is disclosed as uncataloged instead of disappearing.
	for id, usage := range observed {
		if _, known := s.catalog.Models[id]; known {
			continue
		}
		if resolved, _, ok := s.catalog.Resolve(id); ok {
			if _, known := s.catalog.Models[resolved]; known {
				merged := observed[resolved]
				merged.tokens += usage.tokens
				merged.events += usage.events
				merged.unknown += usage.unknown
				observed[resolved] = merged
				delete(observed, id)
			}
		}
	}
	uncataloged := make([]UncatalogedModel, 0, len(observed))
	for id, usage := range observed {
		if _, known := s.catalog.Models[id]; known {
			result.PricedTokens += usage.tokens
			result.ObservedTokens += usage.tokens
			continue
		}
		result.ObservedTokens += usage.tokens
		uncataloged = append(uncataloged, UncatalogedModel{ID: id, Tokens: usage.tokens, Events: usage.events})
	}
	sort.Slice(uncataloged, func(left, right int) bool { return uncataloged[left].Tokens > uncataloged[right].Tokens })
	result.Uncataloged = uncataloged
	ids := make([]string, 0, len(s.catalog.Models))
	for id := range s.catalog.Models {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		model := s.catalog.Models[id]
		usage := observed[id]
		contextLabel := "No usage observed"
		if usage.events > 0 {
			contextLabel = "Observed in usage"
		}
		result.Models = append(result.Models, CatalogEntry{
			ID: id, Provider: model.Provider, DisplayName: model.DisplayName,
			Aliases: append([]string(nil), model.Aliases...), Tier: normalizeTier(model.Tier), Pricing: model.Pricing,
			UsageTokens: usage.tokens, UsageEvents: usage.events, Context: contextLabel,
		})
	}
	return result, nil
}

func normalizeTier(value string) string {
	value = strings.TrimSpace(value)
	switch value {
	case "S", "A", "B", "C", "D":
		return value
	default:
		return "Unranked"
	}
}
