package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tokemon/tokemon/internal/catalog"
	"github.com/tokemon/tokemon/internal/evolution"
	"github.com/tokemon/tokemon/internal/usage"
	_ "modernc.org/sqlite"
)

type Store struct {
	db      *sql.DB
	catalog *catalog.Catalog
}

type IngestResult struct {
	Accepted      int      `json:"accepted"`
	Duplicates    int      `json:"duplicates"`
	Rejected      int      `json:"rejected"`
	Errors        []string `json:"errors,omitempty"`
	PreviousStage int      `json:"previous_stage"`
	CurrentStage  int      `json:"current_stage"`
	Evolved       bool     `json:"evolved"`
	PreviousTotal int64    `json:"previous_lifetime_tokens"`
	CurrentTotal  int64    `json:"current_lifetime_tokens"`
}

type ModelTotal struct {
	Model    string  `json:"model"`
	Tokens   int64   `json:"tokens"`
	Sessions int64   `json:"sessions"`
	Cost     float64 `json:"cost"`
}

type MachineTotal struct {
	Machine string `json:"machine"`
	Tokens  int64  `json:"tokens"`
}

type ToolTotal struct {
	Tool   string `json:"tool"`
	Tokens int64  `json:"tokens"`
}

type ProjectTotal struct {
	Project  string `json:"project"`
	Tokens   int64  `json:"tokens"`
	Sessions int64  `json:"sessions"`
	Machines int64  `json:"machines"`
}

type CostSummary struct {
	Amount         float64 `json:"amount"`
	PricedTokens   int64   `json:"priced_tokens"`
	UnpricedTokens int64   `json:"unpriced_tokens"`
}

type CacheSummary struct {
	HitRate        float64 `json:"hit_rate"`
	CachedTokens   int64   `json:"cached_tokens"`
	EligibleTokens int64   `json:"eligible_tokens"`
}

type TokenComposition struct {
	InputTokens         int64 `json:"input_tokens"`
	UncachedInputTokens int64 `json:"uncached_input_tokens"`
	CachedInputTokens   int64 `json:"cached_input_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
	UnclassifiedTokens  int64 `json:"unclassified_tokens"`
}

const activityWeeks = 53

const (
	databaseSchemaVersion = 1
	backupRetention       = 10
)

type TokenBreakdown struct {
	Name          string `json:"name"`
	Tokens        int64  `json:"tokens"`
	UnknownTokens int64  `json:"unknown_tokens,omitempty"`
}

type DailyUsage struct {
	Date          string           `json:"date"`
	Tokens        int64            `json:"tokens"`
	Events        int64            `json:"events"`
	UnknownTokens int64            `json:"unknown_tokens,omitempty"`
	ByModel       []TokenBreakdown `json:"by_model,omitempty"`
	ByProvider    []TokenBreakdown `json:"by_provider,omitempty"`
}

type ActivityDay struct {
	Date          string           `json:"date"`
	Label         string           `json:"label"`
	Tokens        int64            `json:"tokens"`
	Events        int64            `json:"events"`
	UnknownTokens int64            `json:"unknown_tokens,omitempty"`
	Level         int              `json:"level"`
	Future        bool             `json:"future,omitempty"`
	ByModel       []TokenBreakdown `json:"by_model,omitempty"`
	ByProvider    []TokenBreakdown `json:"by_provider,omitempty"`
}

type ActivityWeek struct {
	MonthLabel string        `json:"month_label,omitempty"`
	Days       []ActivityDay `json:"days"`
}

type ActivityHeatmap struct {
	StartDate      string         `json:"start_date"`
	EndDate        string         `json:"end_date"`
	TotalTokens    int64          `json:"total_tokens"`
	ActiveDays     int64          `json:"active_days"`
	MaxDailyTokens int64          `json:"max_daily_tokens"`
	Weeks          []ActivityWeek `json:"weeks"`
}

type Overview struct {
	LifetimeTokens int64                    `json:"lifetime_tokens"`
	Evolution      evolution.Snapshot       `json:"evolution"`
	Activity       ActivityHeatmap          `json:"activity"`
	ByModel        []ModelTotal             `json:"by_model"`
	ByTool         []ToolTotal              `json:"by_tool"`
	ByMachine      []MachineTotal           `json:"by_machine"`
	ByProject      []ProjectTotal           `json:"by_project"`
	EstimatedCost  CostSummary              `json:"estimated_cost"`
	Cache          CacheSummary             `json:"cache"`
	Threads        int64                    `json:"threads"`
	Accuracy       map[usage.Accuracy]int64 `json:"accuracy"`
}

func Open(path string, modelCatalog *catalog.Catalog) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("database path is required")
	}
	fileBacked := !strings.HasPrefix(path, ":") && !strings.HasPrefix(path, "file:")
	existing := false
	if fileBacked {
		if info, err := os.Stat(path); err == nil {
			existing = info.Size() > 0
		} else if !os.IsNotExist(err) {
			return nil, fmt.Errorf("inspect database: %w", err)
		}
		if directory := filepath.Dir(path); directory != "." {
			if err := os.MkdirAll(directory, 0o755); err != nil {
				return nil, fmt.Errorf("create database directory: %w", err)
			}
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA busy_timeout = 5000"); err != nil {
		db.Close()
		return nil, fmt.Errorf("configure database busy timeout: %w", err)
	}
	if fileBacked {
		var journalMode string
		if err := db.QueryRow("PRAGMA journal_mode = WAL").Scan(&journalMode); err != nil {
			db.Close()
			return nil, fmt.Errorf("enable database WAL mode: %w", err)
		}
		if !strings.EqualFold(journalMode, "wal") {
			db.Close()
			return nil, fmt.Errorf("enable database WAL mode: got %q", journalMode)
		}
	}
	store := &Store{db: db, catalog: modelCatalog}
	if existing {
		version, err := schemaVersion(context.Background(), db)
		if err != nil {
			db.Close()
			return nil, err
		}
		if version < databaseSchemaVersion {
			if err := backupBeforeMigration(context.Background(), db, path, version, databaseSchemaVersion, time.Now().UTC()); err != nil {
				db.Close()
				return nil, err
			}
		}
	}
	if err := store.migrate(context.Background()); err != nil {
		db.Close()
		return nil, err
	}
	return store, nil
}

func (s *Store) Close() error { return s.db.Close() }

func (s *Store) migrate(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS machines (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  operating_system TEXT NOT NULL DEFAULT '',
  architecture TEXT NOT NULL DEFAULT '',
  agent_version TEXT NOT NULL DEFAULT '',
  first_seen_at TEXT NOT NULL,
  last_seen_at TEXT NOT NULL
);
CREATE TABLE IF NOT EXISTS usage_events (
  event_id TEXT PRIMARY KEY,
  timestamp TEXT NOT NULL,
  machine_id TEXT NOT NULL,
  project TEXT NOT NULL DEFAULT '',
  provider TEXT NOT NULL,
  raw_model TEXT NOT NULL,
  canonical_model TEXT NOT NULL DEFAULT '',
  tool TEXT NOT NULL,
  input_tokens INTEGER,
  output_tokens INTEGER,
  cache_read_tokens INTEGER,
  cache_write_tokens INTEGER,
  reasoning_tokens INTEGER,
  total_tokens INTEGER,
  cost REAL,
  cost_estimated INTEGER NOT NULL DEFAULT 0,
  currency TEXT NOT NULL DEFAULT 'USD',
  session_id TEXT,
  duration_ms INTEGER,
  token_accuracy TEXT NOT NULL,
  adapter TEXT NOT NULL,
  adapter_version TEXT NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_usage_events_timestamp ON usage_events(timestamp);
CREATE INDEX IF NOT EXISTS idx_usage_events_model ON usage_events(canonical_model, raw_model);
CREATE INDEX IF NOT EXISTS idx_usage_events_machine ON usage_events(machine_id);
`)
	if err != nil {
		return err
	}
	hasProject, err := hasColumn(ctx, s.db, "usage_events", "project")
	if err != nil {
		return err
	}
	if !hasProject {
		_, err = s.db.ExecContext(ctx, `ALTER TABLE usage_events ADD COLUMN project TEXT NOT NULL DEFAULT ''`)
	}
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, fmt.Sprintf(`PRAGMA user_version = %d`, databaseSchemaVersion))
	return err
}

func schemaVersion(ctx context.Context, db *sql.DB) (int, error) {
	var version int
	if err := db.QueryRowContext(ctx, `PRAGMA user_version`).Scan(&version); err != nil {
		return 0, fmt.Errorf("read database schema version: %w", err)
	}
	return version, nil
}

func backupBeforeMigration(ctx context.Context, db *sql.DB, databasePath string, fromVersion, toVersion int, now time.Time) error {
	var integrity string
	if err := db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&integrity); err != nil {
		return fmt.Errorf("check database before migration: %w", err)
	}
	if integrity != "ok" {
		return fmt.Errorf("database integrity check failed before migration: %s", integrity)
	}

	backupDir := filepath.Join(filepath.Dir(databasePath), "backups")
	if err := os.MkdirAll(backupDir, 0o700); err != nil {
		return fmt.Errorf("create backup directory: %w", err)
	}
	name := fmt.Sprintf("%s-v%d-before-v%d-%s.db", strings.TrimSuffix(filepath.Base(databasePath), filepath.Ext(databasePath)), fromVersion, toVersion, now.Format("20060102T150405.000000000Z"))
	backupPath := filepath.Join(backupDir, name)
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, backupPath); err != nil {
		return fmt.Errorf("back up database before migration: %w", err)
	}
	if err := pruneBackups(backupDir, strings.TrimSuffix(filepath.Base(databasePath), filepath.Ext(databasePath))+"-", backupRetention); err != nil {
		return fmt.Errorf("prune database backups: %w", err)
	}
	return nil
}

func pruneBackups(directory, prefix string, keep int) error {
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasPrefix(entry.Name(), prefix) && strings.HasSuffix(entry.Name(), ".db") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names[:max(0, len(names)-keep)] {
		if err := os.Remove(filepath.Join(directory, name)); err != nil {
			return err
		}
	}
	return nil
}

func hasColumn(ctx context.Context, db *sql.DB, table, column string) (bool, error) {
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, kind string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &primaryKey); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

func (s *Store) Ingest(ctx context.Context, events []usage.Event) (IngestResult, error) {
	before, err := s.LifetimeTokens(ctx)
	if err != nil {
		return IngestResult{}, err
	}
	result := IngestResult{PreviousTotal: before, PreviousStage: evolution.Stage(before)}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return result, err
	}
	defer tx.Rollback()

	for index, event := range events {
		if err := event.Validate(); err != nil {
			result.Rejected++
			result.Errors = append(result.Errors, fmt.Sprintf("event %d: %v", index+1, err))
			continue
		}
		if isAggregateCodexEvent(event) && event.SessionID != "" {
			var detailedExists bool
			if err := tx.QueryRowContext(ctx, `SELECT EXISTS(
SELECT 1 FROM usage_events
WHERE machine_id = ? AND session_id = ? AND adapter = 'codex'
AND (input_tokens IS NOT NULL OR output_tokens IS NOT NULL OR cache_read_tokens IS NOT NULL OR cache_write_tokens IS NOT NULL)
)`, event.MachineID, event.SessionID).Scan(&detailedExists); err != nil {
				return result, err
			}
			if detailedExists {
				// Older agents may keep sending aggregate snapshots while a rollout
				// is in progress. Once detailed events exist for the session, the
				// aggregate is obsolete and must not be allowed to double-count it.
				result.Duplicates++
				continue
			}
		}
		if event.Source.Adapter == "codex" && event.SessionID != "" && event.InputTokens != nil {
			// Detailed Codex JSONL events supersede the older aggregate thread
			// snapshot for the same session. Remove it before inserting component
			// events so upgrading an agent cannot double-count lifetime usage.
			if _, err := tx.ExecContext(ctx, `DELETE FROM usage_events
WHERE machine_id = ? AND session_id = ? AND adapter = 'codex'
AND input_tokens IS NULL AND output_tokens IS NULL AND cache_read_tokens IS NULL AND cache_write_tokens IS NULL`, event.MachineID, event.SessionID); err != nil {
				return result, err
			}
		}
		canonicalModel := event.CanonicalModel
		var estimatedCost *float64
		if canonicalModel == "" && s.catalog != nil {
			if id, model, ok := s.catalog.Resolve(event.Model); ok {
				canonicalModel = id
				if event.Cost == nil {
					estimatedCost, _ = catalog.Estimate(event, model)
				}
			}
		}
		cost := event.Cost
		costEstimated := 0
		if event.CostEstimated {
			costEstimated = 1
		}
		if cost == nil && estimatedCost != nil {
			cost = estimatedCost
			costEstimated = 1
		}
		machineName := event.MachineID
		if err := upsertMachine(ctx, tx, event, machineName); err != nil {
			return result, err
		}
		res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO usage_events (
event_id, timestamp, machine_id, project, provider, raw_model, canonical_model, tool,
input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, reasoning_tokens,
total_tokens, cost, cost_estimated, currency, session_id, duration_ms, token_accuracy,
adapter, adapter_version
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			event.EventID, event.Timestamp.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"), event.MachineID,
			usage.NormalizeProject(event.Project), event.Provider, event.Model, canonicalModel, event.Tool,
			ptrValue(event.InputTokens), ptrValue(event.OutputTokens), ptrValue(event.CacheReadTokens), ptrValue(event.CacheWriteTokens), ptrValue(event.ReasoningTokens),
			ptrValue(event.TotalTokens), cost, costEstimated, currency(event.Currency), nullString(event.SessionID), ptrValue(event.DurationMS),
			string(event.TokenAccuracy), event.Source.Adapter, event.Source.AdapterVersion,
		)
		if err != nil {
			return result, err
		}
		rows, err := res.RowsAffected()
		if err != nil {
			return result, err
		}
		if rows == 0 {
			// Some local providers expose a mutable aggregate per session rather
			// than append-only records. Keep the deterministic event ID stable and
			// refresh the snapshot so periodic scans do not double-count it.
			_, err = tx.ExecContext(ctx, `UPDATE usage_events SET
timestamp=?, machine_id=?, project=?, provider=?, raw_model=?, canonical_model=?, tool=?,
input_tokens=?, output_tokens=?, cache_read_tokens=?, cache_write_tokens=?, reasoning_tokens=?,
total_tokens=?, cost=?, cost_estimated=?, currency=?, session_id=?, duration_ms=?, token_accuracy=?,
adapter=?, adapter_version=?
WHERE event_id=?`,
				event.Timestamp.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"), event.MachineID,
				usage.NormalizeProject(event.Project), event.Provider, event.Model, canonicalModel, event.Tool,
				ptrValue(event.InputTokens), ptrValue(event.OutputTokens), ptrValue(event.CacheReadTokens), ptrValue(event.CacheWriteTokens), ptrValue(event.ReasoningTokens),
				ptrValue(event.TotalTokens), cost, costEstimated, currency(event.Currency), nullString(event.SessionID), ptrValue(event.DurationMS),
				string(event.TokenAccuracy), event.Source.Adapter, event.Source.AdapterVersion, event.EventID,
			)
			if err != nil {
				return result, err
			}
			result.Duplicates++
		} else {
			result.Accepted++
		}
	}
	if err := tx.Commit(); err != nil {
		return result, err
	}
	after, err := s.LifetimeTokens(ctx)
	if err != nil {
		return result, err
	}
	result.CurrentTotal = after
	result.CurrentStage = evolution.Stage(after)
	result.Evolved = result.CurrentStage > result.PreviousStage
	return result, nil
}

func isAggregateCodexEvent(event usage.Event) bool {
	return event.Source.Adapter == "codex" &&
		event.InputTokens == nil && event.OutputTokens == nil &&
		event.CacheReadTokens == nil && event.CacheWriteTokens == nil
}

func upsertMachine(ctx context.Context, tx *sql.Tx, event usage.Event, name string) error {
	now := event.Timestamp.UTC().Format("2006-01-02T15:04:05.999999999Z07:00")
	_, err := tx.ExecContext(ctx, `INSERT INTO machines (id, name, first_seen_at, last_seen_at)
VALUES (?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET name=excluded.name, last_seen_at=excluded.last_seen_at`, event.MachineID, name, now, now)
	return err
}

func ptrValue(value *int64) any {
	if value == nil {
		return nil
	}
	return *value
}

func nullString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func currency(value string) string {
	if value == "" {
		return "USD"
	}
	return value
}

func (s *Store) LifetimeTokens(ctx context.Context) (int64, error) {
	var total sql.NullInt64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(total_tokens), 0) FROM usage_events WHERE total_tokens IS NOT NULL`).Scan(&total)
	return total.Int64, err
}

func (s *Store) TokenComposition(ctx context.Context) (TokenComposition, error) {
	var result TokenComposition
	err := s.db.QueryRowContext(ctx, `SELECT
COALESCE(SUM(COALESCE(input_tokens, 0) + COALESCE(cache_read_tokens, 0) + COALESCE(cache_write_tokens, 0)), 0),
COALESCE(SUM(input_tokens), 0),
COALESCE(SUM(COALESCE(cache_read_tokens, 0) + COALESCE(cache_write_tokens, 0)), 0),
COALESCE(SUM(output_tokens), 0),
COALESCE(SUM(MAX(COALESCE(total_tokens, 0) - COALESCE(input_tokens, 0) - COALESCE(cache_read_tokens, 0) - COALESCE(cache_write_tokens, 0) - COALESCE(output_tokens, 0), 0)), 0)
FROM usage_events`).Scan(
		&result.InputTokens,
		&result.UncachedInputTokens,
		&result.CachedInputTokens,
		&result.OutputTokens,
		&result.UnclassifiedTokens,
	)
	return result, err
}

func (s *Store) Evolution(ctx context.Context) (evolution.Snapshot, error) {
	total, err := s.LifetimeTokens(ctx)
	if err != nil {
		return evolution.Snapshot{}, err
	}
	return evolution.SnapshotFor(total), nil
}

func (s *Store) DailyUsage(ctx context.Context, start, end time.Time) ([]DailyUsage, error) {
	start = dateOnly(start)
	end = dateOnly(end)
	if !end.After(start) {
		return []DailyUsage{}, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT substr(timestamp, 1, 10),
COALESCE(NULLIF(canonical_model, ''), raw_model), provider,
COALESCE(SUM(total_tokens), 0), COUNT(*),
COALESCE(SUM(CASE WHEN total_tokens IS NULL THEN 1 ELSE 0 END), 0)
FROM usage_events
WHERE timestamp >= ? AND timestamp < ?
GROUP BY 1, 2, 3 ORDER BY 1, 4 DESC`, start.Format(time.RFC3339), end.Format(time.RFC3339))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	type dailyAccumulator struct {
		item       DailyUsage
		byModel    map[string]TokenBreakdown
		byProvider map[string]TokenBreakdown
	}
	byDate := make(map[string]*dailyAccumulator)
	for rows.Next() {
		var date, model, provider string
		var tokens, events, unknownTokens int64
		if err := rows.Scan(&date, &model, &provider, &tokens, &events, &unknownTokens); err != nil {
			return nil, err
		}
		item := byDate[date]
		if item == nil {
			item = &dailyAccumulator{
				item:       DailyUsage{Date: date},
				byModel:    make(map[string]TokenBreakdown),
				byProvider: make(map[string]TokenBreakdown),
			}
			byDate[date] = item
		}
		item.item.Tokens += tokens
		item.item.Events += events
		item.item.UnknownTokens += unknownTokens
		modelBreakdown := item.byModel[model]
		modelBreakdown.Name = model
		modelBreakdown.Tokens += tokens
		modelBreakdown.UnknownTokens += unknownTokens
		item.byModel[model] = modelBreakdown
		providerBreakdown := item.byProvider[provider]
		providerBreakdown.Name = provider
		providerBreakdown.Tokens += tokens
		providerBreakdown.UnknownTokens += unknownTokens
		item.byProvider[provider] = providerBreakdown
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	result := make([]DailyUsage, 0, len(byDate))
	for _, item := range byDate {
		item.item.ByModel = breakdowns(item.byModel)
		item.item.ByProvider = breakdowns(item.byProvider)
		result = append(result, item.item)
	}
	sort.Slice(result, func(left, right int) bool { return result[left].Date < result[right].Date })
	return result, nil
}

func breakdowns(values map[string]TokenBreakdown) []TokenBreakdown {
	result := make([]TokenBreakdown, 0, len(values))
	for _, breakdown := range values {
		result = append(result, breakdown)
	}
	sort.Slice(result, func(left, right int) bool {
		if result[left].Tokens == result[right].Tokens {
			return result[left].Name < result[right].Name
		}
		return result[left].Tokens > result[right].Tokens
	})
	return result
}

func (s *Store) Activity(ctx context.Context, now time.Time) (ActivityHeatmap, error) {
	today := dateOnly(now)
	currentWeekStart := today.AddDate(0, 0, -int(today.Weekday()))
	start := currentWeekStart.AddDate(0, 0, -7*(activityWeeks-1))
	end := currentWeekStart.AddDate(0, 0, 7*activityWeeks)
	daily, err := s.DailyUsage(ctx, start, end)
	if err != nil {
		return ActivityHeatmap{}, err
	}
	byDate := make(map[string]DailyUsage, len(daily))
	var totalTokens, activeDays, maxDailyTokens int64
	for _, item := range daily {
		byDate[item.Date] = item
		if item.Events > 0 {
			activeDays++
		}
		totalTokens += item.Tokens
		if item.Tokens > maxDailyTokens {
			maxDailyTokens = item.Tokens
		}
	}

	result := ActivityHeatmap{
		StartDate:      start.Format("2006-01-02"),
		EndDate:        today.Format("2006-01-02"),
		TotalTokens:    totalTokens,
		ActiveDays:     activeDays,
		MaxDailyTokens: maxDailyTokens,
		Weeks:          make([]ActivityWeek, 0, activityWeeks),
	}
	for weekIndex := 0; weekIndex < activityWeeks; weekIndex++ {
		weekStart := start.AddDate(0, 0, weekIndex*7)
		week := ActivityWeek{MonthLabel: activityMonthLabel(weekStart, weekIndex), Days: make([]ActivityDay, 0, 7)}
		for dayIndex := 0; dayIndex < 7; dayIndex++ {
			date := weekStart.AddDate(0, 0, dayIndex)
			dateString := date.Format("2006-01-02")
			item := byDate[dateString]
			future := date.After(today)
			week.Days = append(week.Days, ActivityDay{
				Date:          dateString,
				Label:         date.Format("Monday, January 2, 2006"),
				Tokens:        item.Tokens,
				Events:        item.Events,
				UnknownTokens: item.UnknownTokens,
				Level:         activityLevel(item.Tokens, maxDailyTokens),
				Future:        future,
				ByModel:       append([]TokenBreakdown(nil), item.ByModel...),
				ByProvider:    append([]TokenBreakdown(nil), item.ByProvider...),
			})
		}
		result.Weeks = append(result.Weeks, week)
	}
	return result, nil
}

func dateOnly(value time.Time) time.Time {
	value = value.UTC()
	return time.Date(value.Year(), value.Month(), value.Day(), 0, 0, 0, 0, time.UTC)
}

func activityMonthLabel(weekStart time.Time, weekIndex int) string {
	for dayIndex := 0; dayIndex < 7; dayIndex++ {
		if day := weekStart.AddDate(0, 0, dayIndex); day.Day() == 1 {
			return day.Format("Jan")
		}
	}
	if weekIndex == 0 {
		return weekStart.Format("Jan")
	}
	return ""
}

func activityLevel(tokens, maxTokens int64) int {
	if tokens <= 0 || maxTokens <= 0 {
		return 0
	}
	ratio := float64(tokens) / float64(maxTokens)
	switch {
	case ratio <= 0.25:
		return 1
	case ratio <= 0.5:
		return 2
	case ratio <= 0.75:
		return 3
	default:
		return 4
	}
}

func (s *Store) Overview(ctx context.Context) (Overview, error) {
	total, err := s.LifetimeTokens(ctx)
	if err != nil {
		return Overview{}, err
	}
	result := Overview{LifetimeTokens: total, Evolution: evolution.SnapshotFor(total), Accuracy: make(map[usage.Accuracy]int64)}
	if err := s.db.QueryRowContext(ctx, `SELECT
COALESCE(SUM(cost), 0),
COALESCE(SUM(CASE WHEN cost IS NOT NULL THEN total_tokens ELSE 0 END), 0),
COALESCE(SUM(CASE WHEN cost IS NULL THEN total_tokens ELSE 0 END), 0)
FROM usage_events`).Scan(
		&result.EstimatedCost.Amount,
		&result.EstimatedCost.PricedTokens,
		&result.EstimatedCost.UnpricedTokens,
	); err != nil {
		return Overview{}, err
	}
	if err := s.db.QueryRowContext(ctx, `SELECT
COALESCE(SUM(CASE WHEN input_tokens IS NOT NULL AND cache_read_tokens IS NOT NULL THEN cache_read_tokens ELSE 0 END), 0),
COALESCE(SUM(CASE WHEN input_tokens IS NOT NULL AND cache_read_tokens IS NOT NULL THEN input_tokens + cache_read_tokens ELSE 0 END), 0),
COUNT(DISTINCT CASE WHEN NULLIF(session_id, '') IS NOT NULL THEN machine_id || char(31) || provider || char(31) || tool || char(31) || session_id END)
FROM usage_events`).Scan(&result.Cache.CachedTokens, &result.Cache.EligibleTokens, &result.Threads); err != nil {
		return Overview{}, err
	}
	if result.Cache.EligibleTokens > 0 {
		result.Cache.HitRate = float64(result.Cache.CachedTokens) / float64(result.Cache.EligibleTokens)
	}
	result.Activity, err = s.Activity(ctx, time.Now().UTC())
	if err != nil {
		return Overview{}, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT COALESCE(NULLIF(canonical_model, ''), raw_model), COUNT(*), COALESCE(SUM(total_tokens), 0), COALESCE(SUM(cost), 0) FROM usage_events GROUP BY 1 ORDER BY 3 DESC`)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var item ModelTotal
		if err := rows.Scan(&item.Model, &item.Sessions, &item.Tokens, &item.Cost); err != nil {
			rows.Close()
			return result, err
		}
		result.ByModel = append(result.ByModel, item)
	}
	if err := rows.Close(); err != nil {
		return result, err
	}
	rows, err = s.db.QueryContext(ctx, `SELECT tool, COALESCE(SUM(total_tokens), 0) FROM usage_events GROUP BY tool ORDER BY 2 DESC`)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var item ToolTotal
		if err := rows.Scan(&item.Tool, &item.Tokens); err != nil {
			rows.Close()
			return result, err
		}
		result.ByTool = append(result.ByTool, item)
	}
	if err := rows.Close(); err != nil {
		return result, err
	}
	rows, err = s.db.QueryContext(ctx, `SELECT machine_id, COALESCE(SUM(total_tokens), 0) FROM usage_events GROUP BY machine_id ORDER BY 2 DESC`)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var item MachineTotal
		if err := rows.Scan(&item.Machine, &item.Tokens); err != nil {
			rows.Close()
			return result, err
		}
		result.ByMachine = append(result.ByMachine, item)
	}
	if err := rows.Close(); err != nil {
		return result, err
	}
	rows, err = s.db.QueryContext(ctx, `SELECT project, COALESCE(SUM(total_tokens), 0), COUNT(DISTINCT NULLIF(session_id, '')), COUNT(DISTINCT machine_id) FROM usage_events WHERE project <> '' GROUP BY project ORDER BY 2 DESC`)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var item ProjectTotal
		if err := rows.Scan(&item.Project, &item.Tokens, &item.Sessions, &item.Machines); err != nil {
			rows.Close()
			return result, err
		}
		result.ByProject = append(result.ByProject, item)
	}
	if err := rows.Close(); err != nil {
		return result, err
	}
	rows, err = s.db.QueryContext(ctx, `SELECT token_accuracy, COUNT(*) FROM usage_events GROUP BY token_accuracy`)
	if err != nil {
		return result, err
	}
	for rows.Next() {
		var accuracy string
		var count int64
		if err := rows.Scan(&accuracy, &count); err != nil {
			rows.Close()
			return result, err
		}
		result.Accuracy[usage.Accuracy(accuracy)] = count
	}
	return result, rows.Close()
}

func (s *Store) Events(ctx context.Context) ([]usage.Event, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT event_id, timestamp, machine_id, project, provider, raw_model, canonical_model, tool,
input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, reasoning_tokens, total_tokens,
cost, cost_estimated, currency, session_id, duration_ms, token_accuracy, adapter, adapter_version
FROM usage_events ORDER BY timestamp, event_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var events []usage.Event
	for rows.Next() {
		var event usage.Event
		var timestamp string
		var canonicalModel string
		var input, output, cacheRead, cacheWrite, reasoning, total, duration sql.NullInt64
		var cost sql.NullFloat64
		var costEstimated int
		var session sql.NullString
		var accuracy, adapter, adapterVersion string
		if err := rows.Scan(&event.EventID, &timestamp, &event.MachineID, &event.Project, &event.Provider, &event.Model, &canonicalModel, &event.Tool,
			&input, &output, &cacheRead, &cacheWrite, &reasoning, &total, &cost, &costEstimated, &event.Currency, &session, &duration, &accuracy, &adapter, &adapterVersion); err != nil {
			return nil, err
		}
		parsed, err := timeParse(timestamp)
		if err != nil {
			return nil, err
		}
		event.SchemaVersion = usage.SchemaVersion
		event.Timestamp = parsed
		event.CanonicalModel = canonicalModel
		event.InputTokens = nullInt64(input)
		event.OutputTokens = nullInt64(output)
		event.CacheReadTokens = nullInt64(cacheRead)
		event.CacheWriteTokens = nullInt64(cacheWrite)
		event.ReasoningTokens = nullInt64(reasoning)
		event.TotalTokens = nullInt64(total)
		event.DurationMS = nullInt64(duration)
		event.Cost = nullFloat64(cost)
		event.CostEstimated = costEstimated != 0
		event.SessionID = session.String
		event.TokenAccuracy = usage.Accuracy(accuracy)
		event.Source = usage.Source{Adapter: adapter, AdapterVersion: adapterVersion}
		events = append(events, event)
	}
	return events, rows.Err()
}

func timeParse(value string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, value)
}

func nullInt64(value sql.NullInt64) *int64 {
	if !value.Valid {
		return nil
	}
	return usage.Int64(value.Int64)
}

func nullFloat64(value sql.NullFloat64) *float64 {
	if !value.Valid {
		return nil
	}
	return usage.Float64(value.Float64)
}

func (s *Store) PurgeBefore(ctx context.Context, before string) (int64, error) {
	if _, err := timeParse(before + "T00:00:00Z"); err != nil {
		return 0, fmt.Errorf("before must be YYYY-MM-DD: %w", err)
	}
	result, err := s.db.ExecContext(ctx, `DELETE FROM usage_events WHERE timestamp < ?`, before+"T00:00:00Z")
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}
