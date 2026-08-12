package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
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
	db       *sql.DB
	catalog  *catalog.Catalog
	location *time.Location
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

// AgentHeartbeat contains deployment metadata only. It deliberately excludes
// local paths, prompts, responses, and token records.
type AgentHeartbeat struct {
	MachineID        string
	AgentVersion     string
	OperatingSystem  string
	Architecture     string
	Adapters         []string
	SourceCount      int
	SourceErrorCount int
}

type MachineInfo struct {
	ID               string   `json:"id"`
	Name             string   `json:"name"`
	OperatingSystem  string   `json:"operating_system"`
	Architecture     string   `json:"architecture"`
	AgentVersion     string   `json:"agent_version"`
	DetectedAdapters []string `json:"detected_adapters"`
	SourceCount      int      `json:"source_count"`
	SourceErrorCount int      `json:"source_error_count"`
	FirstSeenAt      string   `json:"first_seen_at"`
	LastSeenAt       string   `json:"last_seen_at"`
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
	databaseSchemaVersion = 4
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
	Timezone       string                   `json:"timezone"`
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

// AnalyticsQuery describes the bounded, metadata-only slice shown by the
// detailed analytics view. Now is injectable so callers and tests can use a
// stable UTC calendar boundary.
type AnalyticsQuery struct {
	Period    string    `json:"period"`
	Dimension string    `json:"dimension"`
	Machine   string    `json:"machine,omitempty"`
	Provider  string    `json:"provider,omitempty"`
	Model     string    `json:"model,omitempty"`
	Tool      string    `json:"tool,omitempty"`
	Now       time.Time `json:"-"`
}

type AnalyticsSummary struct {
	Tokens           int64        `json:"tokens"`
	Events           int64        `json:"events"`
	ActiveDays       int64        `json:"active_days"`
	AverageActiveDay float64      `json:"average_active_day"`
	UnknownEvents    int64        `json:"unknown_events"`
	EstimatedCost    CostSummary  `json:"estimated_cost"`
	Cache            CacheSummary `json:"cache"`
	SessionTokens    int64        `json:"session_tokens"`
	Threads          int64        `json:"threads"`
}

type AnalyticsComparison struct {
	PreviousTokens     int64    `json:"previous_tokens"`
	PreviousEvents     int64    `json:"previous_events"`
	TokenChangePercent *float64 `json:"token_change_percent,omitempty"`
}

type AnalyticsPoint struct {
	Date          string `json:"date"`
	Label         string `json:"label"`
	Tokens        int64  `json:"tokens"`
	Events        int64  `json:"events"`
	UnknownEvents int64  `json:"unknown_events,omitempty"`
	InputTokens   int64  `json:"input_tokens"`
	CachedTokens  int64  `json:"cached_tokens"`
	OutputTokens  int64  `json:"output_tokens"`
}

type AnalyticsBreakdown struct {
	Name     string  `json:"name"`
	Tokens   int64   `json:"tokens"`
	Events   int64   `json:"events"`
	Sessions int64   `json:"sessions"`
	Share    float64 `json:"share"`
}

type AnalyticsShareSeries struct {
	Name   string  `json:"name"`
	Tokens int64   `json:"tokens"`
	Share  float64 `json:"share"`
}

type AnalyticsSharePoint struct {
	Date          string                 `json:"date"`
	Label         string                 `json:"label"`
	Tokens        int64                  `json:"tokens"`
	UnknownEvents int64                  `json:"unknown_events,omitempty"`
	Values        []AnalyticsShareSeries `json:"values"`
}

type AnalyticsSession struct {
	SessionID     string `json:"session_id,omitempty"`
	Timestamp     string `json:"timestamp"`
	Machine       string `json:"machine"`
	Provider      string `json:"provider"`
	Model         string `json:"model"`
	Tool          string `json:"tool"`
	Project       string `json:"project,omitempty"`
	Tokens        int64  `json:"tokens"`
	Events        int64  `json:"events"`
	UnknownEvents int64  `json:"unknown_events,omitempty"`
}

type AnalyticsFacets struct {
	Machines  []string `json:"machines"`
	Providers []string `json:"providers"`
	Models    []string `json:"models"`
	Tools     []string `json:"tools"`
}

type Analytics struct {
	LifetimeTokens int64                  `json:"lifetime_tokens"`
	Timezone       string                 `json:"timezone"`
	Filter         AnalyticsQuery         `json:"filter"`
	StartDate      string                 `json:"start_date"`
	EndDate        string                 `json:"end_date"`
	Bucket         string                 `json:"bucket"`
	Summary        AnalyticsSummary       `json:"summary"`
	Comparison     *AnalyticsComparison   `json:"comparison,omitempty"`
	Points         []AnalyticsPoint       `json:"points"`
	MaxTokens      int64                  `json:"max_tokens"`
	Breakdown      []AnalyticsBreakdown   `json:"breakdown"`
	ShareSeries    []AnalyticsShareSeries `json:"share_series"`
	SharePoints    []AnalyticsSharePoint  `json:"share_points"`
	Sessions       []AnalyticsSession     `json:"recent_sessions"`
	Facets         AnalyticsFacets        `json:"facets"`
}

// InsightDistribution is a privacy-safe aggregate bucket. Labels are local
// calendar labels (hour or weekday), never event timestamps.
type InsightDistribution struct {
	Label  string  `json:"label"`
	Tokens int64   `json:"tokens"`
	Events int64   `json:"events"`
	Share  float64 `json:"share"`
}

// InsightDataQuality describes which portions of an Insights window are
// directly supported by stored metadata. Unknown values remain unknown; they
// are never converted to zero for a conclusion.
type InsightDataQuality struct {
	KnownEvents   int64                    `json:"known_events"`
	UnknownEvents int64                    `json:"unknown_events"`
	KnownTokens   int64                    `json:"known_tokens"`
	EventCoverage float64                  `json:"event_coverage"`
	Confidence    string                   `json:"confidence"`
	Qualifier     string                   `json:"qualifier,omitempty"`
	Accuracy      map[usage.Accuracy]int64 `json:"accuracy,omitempty"`
}

// InsightCard is an evidence-backed, privacy-safe local summary of one
// aggregate signal. Evidence and Narrative intentionally contain no session
// identifiers or raw event timestamps. AnalyticsQuery keeps a canonical
// deep-linkable filter so a reader can inspect the underlying analytics view
// without exposing records; an outbound AI projection may redact labels too.
type InsightCard struct {
	ID             string             `json:"id"`
	Category       string             `json:"category"`
	Title          string             `json:"title"`
	Narrative      string             `json:"narrative"`
	Observation    string             `json:"observation,omitempty"`
	Evidence       string             `json:"evidence"`
	Basis          string             `json:"basis"`
	AnalyticsURL   string             `json:"analytics_url,omitempty"`
	AnalyticsQuery AnalyticsQuery     `json:"analytics_query"`
	DataQuality    InsightDataQuality `json:"data_quality"`
	Qualifiers     []string           `json:"qualifiers,omitempty"`
	PeakHour       string             `json:"peak_hour,omitempty"`
	PeakWeekday    string             `json:"peak_weekday,omitempty"`
}

// Insights is the deterministic data projection consumed by the future
// /insights page and by optional language-model summarizers. It contains only
// aggregates and canonical analytics metadata.
type Insights struct {
	Period      string                `json:"period"`
	Timezone    string                `json:"timezone"`
	Scope       AnalyticsQuery        `json:"scope"`
	Filter      AnalyticsQuery        `json:"filter"`
	StartDate   string                `json:"start_date"`
	EndDate     string                `json:"end_date"`
	DataQuality InsightDataQuality    `json:"data_quality"`
	Qualifiers  []string              `json:"qualifiers,omitempty"`
	Hourly      []InsightDistribution `json:"hourly,omitempty"`
	Weekdays    []InsightDistribution `json:"weekdays,omitempty"`
	PeakHour    string                `json:"peak_hour,omitempty"`
	PeakWeekday string                `json:"peak_weekday,omitempty"`
	Cards       []InsightCard         `json:"cards"`
}

// InsightsResult is retained as a descriptive alias for callers that prefer
// result-oriented naming.
type InsightsResult = Insights

func Open(path string, modelCatalog *catalog.Catalog) (*Store, error) {
	return OpenWithLocation(path, modelCatalog, time.UTC)
}

// OpenWithLocation opens the store and uses location for all calendar
// bucketing. Event timestamps remain stored as UTC instants.
func OpenWithLocation(path string, modelCatalog *catalog.Catalog, location *time.Location) (*Store, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("database path is required")
	}
	if location == nil {
		location = time.UTC
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
	store := &Store{db: db, catalog: modelCatalog, location: location}
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

// Ready verifies that the SQLite handle can execute a trivial query. It is
// intentionally separate from Close so the HTTP health endpoint can report
// database readiness instead of only process liveness.
func (s *Store) Ready(ctx context.Context) error {
	if s == nil || s.db == nil {
		return errors.New("database is not open")
	}
	return s.db.PingContext(ctx)
}

func (s *Store) Timezone() string {
	if s.location == nil {
		return time.UTC.String()
	}
	return s.location.String()
}

func (s *Store) reportingLocation() *time.Location {
	if s.location == nil {
		return time.UTC
	}
	return s.location
}

func (s *Store) migrate(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx, `
CREATE TABLE IF NOT EXISTS machines (
  id TEXT PRIMARY KEY,
  name TEXT NOT NULL,
  operating_system TEXT NOT NULL DEFAULT '',
  architecture TEXT NOT NULL DEFAULT '',
  agent_version TEXT NOT NULL DEFAULT '',
  detected_adapters TEXT NOT NULL DEFAULT '',
  source_count INTEGER NOT NULL DEFAULT 0,
  source_error_count INTEGER NOT NULL DEFAULT 0,
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
CREATE TABLE IF NOT EXISTS display_aliases (
  kind TEXT NOT NULL CHECK (kind IN ('model', 'machine')),
  identity TEXT NOT NULL,
  alias TEXT NOT NULL,
  updated_at TEXT NOT NULL,
  PRIMARY KEY (kind, identity)
);
`)
	if err != nil {
		return err
	}
	for _, column := range []struct {
		name       string
		definition string
	}{
		{name: "detected_adapters", definition: "TEXT NOT NULL DEFAULT ''"},
		{name: "source_count", definition: "INTEGER NOT NULL DEFAULT 0"},
		{name: "source_error_count", definition: "INTEGER NOT NULL DEFAULT 0"},
	} {
		exists, err := hasColumn(ctx, s.db, "machines", column.name)
		if err != nil {
			return err
		}
		if !exists {
			if _, err := s.db.ExecContext(ctx, `ALTER TABLE machines ADD COLUMN `+column.name+` `+column.definition); err != nil {
				return fmt.Errorf("add machines.%s: %w", column.name, err)
			}
		}
	}
	if _, err := s.db.ExecContext(ctx, `
DELETE FROM usage_events
WHERE adapter = 'codex'
  AND input_tokens IS NOT NULL
  AND output_tokens IS NOT NULL
  AND cache_read_tokens IS NOT NULL
  AND cache_write_tokens IS NOT NULL
  AND reasoning_tokens IS NOT NULL
  AND total_tokens IS NOT NULL
  AND rowid NOT IN (
    SELECT MIN(rowid)
    FROM usage_events
    WHERE adapter = 'codex'
      AND input_tokens IS NOT NULL
      AND output_tokens IS NOT NULL
      AND cache_read_tokens IS NOT NULL
      AND cache_write_tokens IS NOT NULL
      AND reasoning_tokens IS NOT NULL
      AND total_tokens IS NOT NULL
    GROUP BY machine_id, session_id, timestamp, provider, raw_model, tool,
      input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, reasoning_tokens, total_tokens
  )`); err != nil {
		return fmt.Errorf("deduplicate Codex usage: %w", err)
	}
	if _, err := s.db.ExecContext(ctx, `
CREATE UNIQUE INDEX IF NOT EXISTS idx_usage_events_codex_usage
ON usage_events (
  machine_id, session_id, timestamp, provider, raw_model, tool,
  input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, reasoning_tokens, total_tokens
)
WHERE adapter = 'codex'
  AND input_tokens IS NOT NULL
  AND output_tokens IS NOT NULL
  AND cache_read_tokens IS NOT NULL
  AND cache_write_tokens IS NOT NULL
  AND reasoning_tokens IS NOT NULL
  AND total_tokens IS NOT NULL`); err != nil {
		return fmt.Errorf("create Codex usage deduplication index: %w", err)
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
		args := usageEventArgs(event, canonicalModel, cost, costEstimated)
		res, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO usage_events (
event_id, timestamp, machine_id, project, provider, raw_model, canonical_model, tool,
input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, reasoning_tokens,
total_tokens, cost, cost_estimated, currency, session_id, duration_ms, token_accuracy,
adapter, adapter_version
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
			args...,
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
			updated, err := refreshUsageEvent(ctx, tx, event, args)
			if err != nil {
				return result, err
			}
			if !updated {
				return result, fmt.Errorf("usage event %q was ignored without a matching stored row", event.EventID)
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

func usageEventArgs(event usage.Event, canonicalModel string, cost *float64, costEstimated int) []any {
	return []any{
		event.EventID, event.Timestamp.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"), event.MachineID,
		usage.NormalizeProject(event.Project), event.Provider, event.Model, canonicalModel, event.Tool,
		ptrValue(event.InputTokens), ptrValue(event.OutputTokens), ptrValue(event.CacheReadTokens), ptrValue(event.CacheWriteTokens), ptrValue(event.ReasoningTokens),
		ptrValue(event.TotalTokens), cost, costEstimated, currency(event.Currency), nullString(event.SessionID), ptrValue(event.DurationMS),
		string(event.TokenAccuracy), event.Source.Adapter, event.Source.AdapterVersion,
	}
}

func refreshUsageEvent(ctx context.Context, tx *sql.Tx, event usage.Event, args []any) (bool, error) {
	updateArgs := append([]any{args[0]}, args[1:]...)
	updateArgs = append(updateArgs, event.EventID)
	res, err := tx.ExecContext(ctx, `UPDATE OR IGNORE usage_events SET
event_id=?, timestamp=?, machine_id=?, project=?, provider=?, raw_model=?, canonical_model=?, tool=?,
input_tokens=?, output_tokens=?, cache_read_tokens=?, cache_write_tokens=?, reasoning_tokens=?,
total_tokens=?, cost=?, cost_estimated=?, currency=?, session_id=?, duration_ms=?, token_accuracy=?,
adapter=?, adapter_version=?
WHERE event_id=?`, updateArgs...)
	if err != nil {
		return false, err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if rows > 0 {
		return true, nil
	}

	if !isDetailedCodexEvent(event) {
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM usage_events WHERE event_id = ?)`, event.EventID).Scan(&exists); err != nil {
			return false, err
		}
		return exists, nil
	}

	duplicateID, found, err := findDetailedCodexDuplicate(ctx, tx, event)
	if err != nil {
		return false, err
	}
	if !found {
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM usage_events WHERE event_id = ?)`, event.EventID).Scan(&exists); err != nil {
			return false, err
		}
		return exists, nil
	}
	if duplicateID == event.EventID {
		return true, nil
	}

	// A corrected event may have the same dedup dimensions as another row while
	// its old event ID still occupies the row being refreshed. Keep one canonical
	// row and migrate the dedup winner to the incoming event ID.
	if _, err := tx.ExecContext(ctx, `DELETE FROM usage_events WHERE event_id = ?`, event.EventID); err != nil {
		return false, err
	}
	updateArgs = append([]any{args[0]}, args[1:]...)
	updateArgs = append(updateArgs, duplicateID)
	res, err = tx.ExecContext(ctx, `UPDATE usage_events SET
event_id=?, timestamp=?, machine_id=?, project=?, provider=?, raw_model=?, canonical_model=?, tool=?,
input_tokens=?, output_tokens=?, cache_read_tokens=?, cache_write_tokens=?, reasoning_tokens=?,
total_tokens=?, cost=?, cost_estimated=?, currency=?, session_id=?, duration_ms=?, token_accuracy=?,
adapter=?, adapter_version=?
WHERE event_id=?`, updateArgs...)
	if err != nil {
		return false, err
	}
	rows, err = res.RowsAffected()
	return rows > 0, err
}

func isDetailedCodexEvent(event usage.Event) bool {
	return event.Source.Adapter == "codex" && event.InputTokens != nil && event.OutputTokens != nil &&
		event.CacheReadTokens != nil && event.CacheWriteTokens != nil && event.ReasoningTokens != nil && event.TotalTokens != nil
}

func findDetailedCodexDuplicate(ctx context.Context, tx *sql.Tx, event usage.Event) (string, bool, error) {
	var eventID string
	err := tx.QueryRowContext(ctx, `SELECT event_id FROM usage_events
WHERE adapter = 'codex' AND machine_id = ? AND session_id IS ? AND timestamp = ?
AND provider = ? AND raw_model = ? AND tool = ?
AND input_tokens IS ? AND output_tokens IS ? AND cache_read_tokens IS ?
AND cache_write_tokens IS ? AND reasoning_tokens IS ? AND total_tokens IS ?
LIMIT 1`,
		event.MachineID, nullString(event.SessionID), event.Timestamp.UTC().Format("2006-01-02T15:04:05.999999999Z07:00"),
		event.Provider, event.Model, event.Tool,
		ptrValue(event.InputTokens), ptrValue(event.OutputTokens), ptrValue(event.CacheReadTokens),
		ptrValue(event.CacheWriteTokens), ptrValue(event.ReasoningTokens), ptrValue(event.TotalTokens),
	).Scan(&eventID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return eventID, true, nil
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
ON CONFLICT(id) DO UPDATE SET
    name = excluded.name,
    last_seen_at = CASE WHEN excluded.last_seen_at > machines.last_seen_at THEN excluded.last_seen_at ELSE machines.last_seen_at END`, event.MachineID, name, now, now)
	return err
}

func (s *Store) RecordHeartbeat(ctx context.Context, heartbeat AgentHeartbeat) error {
	if strings.TrimSpace(heartbeat.MachineID) == "" {
		return errors.New("machine ID is required")
	}
	if heartbeat.SourceCount < 0 || heartbeat.SourceErrorCount < 0 {
		return errors.New("heartbeat source counts cannot be negative")
	}
	now := time.Now().UTC().Format("2006-01-02T15:04:05.999999999Z07:00")
	adapters := strings.Join(normalizeAdapterIDs(heartbeat.Adapters), ",")
	_, err := s.db.ExecContext(ctx, `
INSERT INTO machines (
    id, name, operating_system, architecture, agent_version,
    detected_adapters, source_count, source_error_count, first_seen_at, last_seen_at
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET
    name = excluded.name,
    operating_system = CASE WHEN excluded.operating_system <> '' THEN excluded.operating_system ELSE machines.operating_system END,
    architecture = CASE WHEN excluded.architecture <> '' THEN excluded.architecture ELSE machines.architecture END,
    agent_version = CASE WHEN excluded.agent_version <> '' THEN excluded.agent_version ELSE machines.agent_version END,
    detected_adapters = excluded.detected_adapters,
    source_count = excluded.source_count,
    source_error_count = excluded.source_error_count,
    last_seen_at = excluded.last_seen_at`,
		heartbeat.MachineID, heartbeat.MachineID, heartbeat.OperatingSystem, heartbeat.Architecture,
		heartbeat.AgentVersion, adapters, heartbeat.SourceCount, heartbeat.SourceErrorCount, now, now)
	return err
}

func (s *Store) Machines(ctx context.Context) ([]MachineInfo, error) {
	rows, err := s.db.QueryContext(ctx, `
SELECT id, name, operating_system, architecture, agent_version,
       detected_adapters, source_count, source_error_count, first_seen_at, last_seen_at
FROM machines
ORDER BY last_seen_at DESC, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []MachineInfo
	for rows.Next() {
		var machine MachineInfo
		var adapterList string
		if err := rows.Scan(
			&machine.ID, &machine.Name, &machine.OperatingSystem, &machine.Architecture,
			&machine.AgentVersion, &adapterList, &machine.SourceCount, &machine.SourceErrorCount,
			&machine.FirstSeenAt, &machine.LastSeenAt,
		); err != nil {
			return nil, err
		}
		machine.DetectedAdapters = splitAdapterIDs(adapterList)
		result = append(result, machine)
	}
	return result, rows.Err()
}

func normalizeAdapterIDs(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if _, ok := seen[value]; ok {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	sort.Strings(result)
	return result
}

func splitAdapterIDs(value string) []string {
	if strings.TrimSpace(value) == "" {
		return []string{}
	}
	return normalizeAdapterIDs(strings.Split(value, ","))
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
	start = s.dateOnly(start)
	end = s.dateOnly(end)
	if !end.After(start) {
		return []DailyUsage{}, nil
	}
	rows, err := s.db.QueryContext(ctx, `SELECT timestamp,
COALESCE(NULLIF(canonical_model, ''), raw_model), provider, total_tokens
FROM usage_events
WHERE timestamp >= ? AND timestamp < ?
ORDER BY timestamp`, start.UTC().Format(time.RFC3339), end.UTC().Format(time.RFC3339))
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
		var timestamp, model, provider string
		var total sql.NullInt64
		if err := rows.Scan(&timestamp, &model, &provider, &total); err != nil {
			return nil, err
		}
		parsed, err := timeParse(timestamp)
		if err != nil {
			return nil, err
		}
		date := parsed.In(s.reportingLocation()).Format("2006-01-02")
		var tokens, unknownTokens int64
		if total.Valid {
			tokens = total.Int64
		} else {
			unknownTokens = 1
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
		item.item.Events++
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
	today := s.dateOnly(now)
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

func (s *Store) dateOnly(value time.Time) time.Time {
	value = value.In(s.reportingLocation())
	return time.Date(value.Year(), value.Month(), value.Day(), 0, 0, 0, 0, s.reportingLocation())
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
	result := Overview{LifetimeTokens: total, Timezone: s.Timezone(), Evolution: evolution.SnapshotFor(total), Accuracy: make(map[usage.Accuracy]int64)}
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
	result.Activity, err = s.Activity(ctx, time.Now())
	if err != nil {
		return Overview{}, err
	}
	rows, err := s.db.QueryContext(ctx, `SELECT COALESCE(NULLIF(canonical_model, ''), raw_model),
COUNT(DISTINCT CASE WHEN NULLIF(session_id, '') IS NOT NULL THEN machine_id || char(31) || provider || char(31) || tool || char(31) || session_id END),
COALESCE(SUM(total_tokens), 0), COALESCE(SUM(cost), 0)
FROM usage_events GROUP BY 1 ORDER BY 3 DESC`)
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
	rows, err = s.db.QueryContext(ctx, `SELECT project, COALESCE(SUM(total_tokens), 0),
COUNT(DISTINCT CASE WHEN NULLIF(session_id, '') IS NOT NULL THEN machine_id || char(31) || provider || char(31) || tool || char(31) || session_id END),
COUNT(DISTINCT machine_id)
FROM usage_events WHERE project <> '' GROUP BY project ORDER BY 2 DESC`)
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

func (s *Store) Analytics(ctx context.Context, query AnalyticsQuery) (Analytics, error) {
	query, start, end := s.normalizeAnalyticsQuery(query)
	result := Analytics{Timezone: s.Timezone(), Filter: query, Bucket: "day"}
	if query.Period == "24h" {
		result.Bucket = "hour"
	}
	result.LifetimeTokens, _ = s.LifetimeTokens(ctx)
	result.EndDate = s.dateOnly(end.Add(-time.Nanosecond)).Format("2006-01-02")
	result.StartDate = result.EndDate
	if !start.IsZero() {
		result.StartDate = start.In(s.reportingLocation()).Format("2006-01-02")
	}
	if query.Period == "all" {
		result.Bucket = "month"
	}

	where, args := analyticsWhere(query, start, end)
	if err := s.analyticsSummary(ctx, where, args, &result.Summary); err != nil {
		return Analytics{}, err
	}
	if !start.IsZero() {
		previousEnd := start
		previousStart := previousEnd.Add(-end.Sub(start))
		previousWhere, previousArgs := analyticsWhere(query, previousStart, previousEnd)
		var previous AnalyticsSummary
		if err := s.analyticsSummary(ctx, previousWhere, previousArgs, &previous); err != nil {
			return Analytics{}, err
		}
		comparison := &AnalyticsComparison{PreviousTokens: previous.Tokens, PreviousEvents: previous.Events}
		if previous.Tokens > 0 {
			change := float64(result.Summary.Tokens-previous.Tokens) * 100 / float64(previous.Tokens)
			comparison.TokenChangePercent = &change
		}
		result.Comparison = comparison
	}
	points, err := s.analyticsPoints(ctx, query, where, args)
	if err != nil {
		return Analytics{}, err
	}
	points = s.fillAnalyticsPoints(points, query, start, end)
	result.Points = points
	for _, point := range points {
		if point.Tokens > result.MaxTokens {
			result.MaxTokens = point.Tokens
		}
	}
	if query.Period == "all" && len(points) > 0 {
		result.StartDate = points[0].Date
	}
	result.Breakdown, err = s.analyticsBreakdown(ctx, query, where, args, result.Summary.Tokens)
	if err != nil {
		return Analytics{}, err
	}
	result.ShareSeries, result.SharePoints, err = s.analyticsShareTimeline(ctx, query, where, args, start, end, result.Breakdown, result.Summary.Tokens)
	if err != nil {
		return Analytics{}, err
	}
	result.Sessions, err = s.analyticsSessions(ctx, where, args)
	if err != nil {
		return Analytics{}, err
	}
	result.Facets, err = s.analyticsFacets(ctx)
	if err != nil {
		return Analytics{}, err
	}
	return result, nil
}

const (
	insightCategoryMomentum      = "momentum"
	insightCategoryRhythm        = "rhythm"
	insightCategoryConcentration = "concentration"
	insightCategoryComposition   = "composition"
	insightCategoryConfidence    = "confidence"
)

type insightCandidate struct {
	card  InsightCard
	score float64
}

type insightAggregate struct {
	hourTokens      [24]int64
	hourEvents      [24]int64
	weekdayTokens   [7]int64
	weekdayEvents   [7]int64
	knownEvents     int64
	unknownEvents   int64
	knownTokens     int64
	componentEvents int64
	inputTokens     int64
	cachedTokens    int64
	outputTokens    int64
	unclassified    int64
	accuracy        map[usage.Accuracy]int64
}

// Insights computes deterministic, privacy-safe cards over the same window
// and filters used by Analytics. It intentionally does not use an all-time
// prior for the "all" period: without an equal prior window, growth is not a
// defensible conclusion.
func (s *Store) Insights(ctx context.Context, query AnalyticsQuery) (Insights, error) {
	analytics, err := s.Analytics(ctx, query)
	if err != nil {
		return Insights{}, err
	}
	canonical, start, end := s.normalizeAnalyticsQuery(query)
	where, args := analyticsWhere(canonical, start, end)
	aggregate, err := s.insightsAggregate(ctx, where, args)
	if err != nil {
		return Insights{}, err
	}

	quality := insightQuality(analytics.Summary.Events, analytics.Summary.Tokens, aggregate.unknownEvents, aggregate.accuracy)
	result := Insights{
		Period:      canonical.Period,
		Timezone:    s.Timezone(),
		Scope:       canonical,
		Filter:      canonical,
		StartDate:   analytics.StartDate,
		EndDate:     analytics.EndDate,
		DataQuality: quality,
	}
	result.Qualifiers = insightQualifiers(quality, aggregate, analytics.Summary)
	result.Hourly, result.PeakHour = insightHourly(aggregate, aggregate.knownTokens, s.reportingLocation())
	result.Weekdays, result.PeakWeekday = insightWeekdays(aggregate, aggregate.knownTokens)

	url := analyticsInsightURL(canonical)
	candidates := make([]insightCandidate, 0, 5)
	if card, score, ok := insightMomentumCard(canonical, analytics, quality, url); ok {
		candidates = append(candidates, insightCandidate{card: card, score: score})
	}
	if card, score, ok := insightRhythmCard(canonical, aggregate, quality, result.PeakHour, result.PeakWeekday, url); ok {
		candidates = append(candidates, insightCandidate{card: card, score: score})
	}
	if card, score, ok := insightConcentrationCard(canonical, analytics, quality, url); ok {
		candidates = append(candidates, insightCandidate{card: card, score: score})
	}
	if card, score, ok := insightCompositionCard(canonical, aggregate, analytics.Summary, quality, url); ok {
		candidates = append(candidates, insightCandidate{card: card, score: score})
	}
	if card, score, ok := insightConfidenceCard(canonical, aggregate, analytics.Summary, quality, url); ok {
		candidates = append(candidates, insightCandidate{card: card, score: score})
	}

	sort.SliceStable(candidates, func(left, right int) bool {
		if candidates[left].score != candidates[right].score {
			return candidates[left].score > candidates[right].score
		}
		return candidates[left].card.ID < candidates[right].card.ID
	})
	if len(candidates) > 5 {
		candidates = candidates[:5]
	}
	result.Cards = make([]InsightCard, len(candidates))
	for index, candidate := range candidates {
		candidate.card.DataQuality = quality
		candidate.card.AnalyticsQuery.Now = time.Time{}
		result.Cards[index] = candidate.card
	}
	return result, nil
}

func (s *Store) insightsAggregate(ctx context.Context, where string, args []any) (insightAggregate, error) {
	result := insightAggregate{accuracy: make(map[usage.Accuracy]int64)}
	rows, err := s.db.QueryContext(ctx, `SELECT timestamp, total_tokens,
input_tokens, output_tokens, cache_read_tokens, cache_write_tokens, token_accuracy
FROM usage_events WHERE `+where, args...)
	if err != nil {
		return result, err
	}
	defer rows.Close()
	for rows.Next() {
		var timestamp, accuracy string
		var total, input, output, cached, cacheWrite sql.NullInt64
		if err := rows.Scan(&timestamp, &total, &input, &output, &cached, &cacheWrite, &accuracy); err != nil {
			return result, err
		}
		result.accuracy[usage.Accuracy(accuracy)]++
		parsed, err := timeParse(timestamp)
		if err != nil {
			return result, err
		}
		local := parsed.In(s.reportingLocation())
		hour := local.Hour()
		weekday := int(local.Weekday())
		result.hourEvents[hour]++
		result.weekdayEvents[weekday]++
		if !total.Valid {
			result.unknownEvents++
			continue
		}
		result.knownEvents++
		result.knownTokens += total.Int64
		result.hourTokens[hour] += total.Int64
		result.weekdayTokens[weekday] += total.Int64

		if input.Valid || output.Valid || cached.Valid || cacheWrite.Valid {
			result.componentEvents++
		}
		if input.Valid {
			result.inputTokens += input.Int64
		}
		if cached.Valid {
			result.cachedTokens += cached.Int64
		}
		if cacheWrite.Valid {
			result.cachedTokens += cacheWrite.Int64
		}
		if output.Valid {
			result.outputTokens += output.Int64
		}
		knownComponents := int64(0)
		if input.Valid {
			knownComponents += input.Int64
		}
		if cached.Valid {
			knownComponents += cached.Int64
		}
		if cacheWrite.Valid {
			knownComponents += cacheWrite.Int64
		}
		if output.Valid {
			knownComponents += output.Int64
		}
		if remainder := total.Int64 - knownComponents; remainder > 0 {
			result.unclassified += remainder
		}
	}
	if err := rows.Err(); err != nil {
		return result, err
	}
	return result, nil
}

func insightQuality(events, tokens, unknown int64, accuracy map[usage.Accuracy]int64) InsightDataQuality {
	known := events - unknown
	if known < 0 {
		known = 0
	}
	coverage := float64(0)
	if events > 0 {
		coverage = float64(known) / float64(events)
	}
	confidence := "unknown"
	switch {
	case events > 0 && coverage >= 0.99:
		confidence = "high"
	case events > 0 && coverage >= 0.9:
		confidence = "medium"
	case events > 0:
		confidence = "limited"
	}
	qualifier := ""
	switch confidence {
	case "high":
		qualifier = "all event totals are known in this window"
	case "medium", "limited":
		qualifier = "some event totals are unknown; known-token aggregates exclude them"
	default:
		qualifier = "no known token-total coverage is available"
	}
	copyAccuracy := make(map[usage.Accuracy]int64, len(accuracy))
	for key, value := range accuracy {
		copyAccuracy[key] = value
	}
	return InsightDataQuality{
		KnownEvents:   known,
		UnknownEvents: unknown,
		KnownTokens:   tokens,
		EventCoverage: coverage,
		Confidence:    confidence,
		Qualifier:     qualifier,
		Accuracy:      copyAccuracy,
	}
}

func insightQualifiers(quality InsightDataQuality, aggregate insightAggregate, summary AnalyticsSummary) []string {
	qualifiers := make([]string, 0, 4)
	if quality.UnknownEvents > 0 {
		qualifiers = append(qualifiers, fmt.Sprintf("%d event(s) have unknown token totals; known-token aggregates exclude them", quality.UnknownEvents))
	}
	if aggregate.componentEvents < quality.KnownEvents {
		qualifiers = append(qualifiers, "token components are incomplete for some known-total events")
	}
	if summary.EstimatedCost.PricedTokens == 0 {
		qualifiers = append(qualifiers, "cost coverage is unavailable in this window")
	} else if summary.EstimatedCost.UnpricedTokens > 0 {
		qualifiers = append(qualifiers, "cost coverage is partial; unpriced tokens remain excluded")
	}
	if len(qualifiers) == 0 {
		qualifiers = append(qualifiers, "all event totals are known for this window")
	}
	return qualifiers
}

func insightHourly(aggregate insightAggregate, total int64, location *time.Location) ([]InsightDistribution, string) {
	result := make([]InsightDistribution, 24)
	peak := -1
	for hour := 0; hour < len(result); hour++ {
		result[hour] = InsightDistribution{Label: fmt.Sprintf("%02d:00", hour), Tokens: aggregate.hourTokens[hour], Events: aggregate.hourEvents[hour]}
		if total > 0 {
			result[hour].Share = float64(result[hour].Tokens) / float64(total)
		}
		if result[hour].Tokens > 0 && (peak < 0 || result[hour].Tokens > aggregate.hourTokens[peak]) {
			peak = hour
		}
	}
	if peak < 0 {
		return result, ""
	}
	// Formatting through the configured location makes the timezone contract
	// explicit while avoiding any raw event timestamp in the result.
	return result, time.Date(2000, 1, 1, peak, 0, 0, 0, location).Format("15:04")
}

func insightWeekdays(aggregate insightAggregate, total int64) ([]InsightDistribution, string) {
	result := make([]InsightDistribution, 7)
	peak := -1
	for weekday := 0; weekday < len(result); weekday++ {
		label := time.Weekday(weekday).String()
		result[weekday] = InsightDistribution{Label: label, Tokens: aggregate.weekdayTokens[weekday], Events: aggregate.weekdayEvents[weekday]}
		if total > 0 {
			result[weekday].Share = float64(result[weekday].Tokens) / float64(total)
		}
		if result[weekday].Tokens > 0 && (peak < 0 || result[weekday].Tokens > aggregate.weekdayTokens[peak]) {
			peak = weekday
		}
	}
	if peak < 0 {
		return result, ""
	}
	return result, time.Weekday(peak).String()
}

func insightMomentumCard(query AnalyticsQuery, analytics Analytics, quality InsightDataQuality, analyticsURL string) (InsightCard, float64, bool) {
	if query.Period == "all" || analytics.Comparison == nil || analytics.Comparison.PreviousTokens <= 0 {
		return InsightCard{}, 0, false
	}
	change := float64(analytics.Summary.Tokens-analytics.Comparison.PreviousTokens) * 100 / float64(analytics.Comparison.PreviousTokens)
	direction := "steady"
	if change > 0 {
		direction = "up"
	} else if change < 0 {
		direction = "down"
	}
	qualifiers := []string{"comparison uses known totals in equal-length windows"}
	if quality.UnknownEvents > 0 {
		qualifiers = append(qualifiers, fmt.Sprintf("%d unknown-total event(s) are excluded", quality.UnknownEvents))
	}
	card := InsightCard{
		ID:             "momentum-vs-prior",
		Category:       insightCategoryMomentum,
		Title:          "Momentum vs prior window",
		Narrative:      fmt.Sprintf("Known token volume is %s relative to the equal prior window; this is a volume comparison, not a productivity or causal claim.", direction),
		Observation:    fmt.Sprintf("%+.1f%% change in known tokens", change),
		Evidence:       fmt.Sprintf("Known tokens: %d current vs %d equal prior (%+.1f%%).", analytics.Summary.Tokens, analytics.Comparison.PreviousTokens, change),
		Basis:          "(current known tokens - equal-prior known tokens) / equal-prior known tokens; unknown totals excluded",
		AnalyticsURL:   analyticsURL,
		AnalyticsQuery: query,
		Qualifiers:     qualifiers,
	}
	return card, absFloat(change) / 100, true
}

func insightRhythmCard(query AnalyticsQuery, aggregate insightAggregate, quality InsightDataQuality, peakHour, peakWeekday, analyticsURL string) (InsightCard, float64, bool) {
	if aggregate.knownTokens <= 0 || peakHour == "" || peakWeekday == "" {
		return InsightCard{}, 0, false
	}
	var hourTokens, dayTokens int64
	for hour := 0; hour < 24; hour++ {
		if fmt.Sprintf("%02d:00", hour) == peakHour {
			hourTokens = aggregate.hourTokens[hour]
			break
		}
	}
	for weekday := 0; weekday < 7; weekday++ {
		if time.Weekday(weekday).String() == peakWeekday {
			dayTokens = aggregate.weekdayTokens[weekday]
			break
		}
	}
	hourShare := float64(hourTokens) / float64(aggregate.knownTokens)
	dayShare := float64(dayTokens) / float64(aggregate.knownTokens)
	qualifiers := []string{"local-time aggregate; timing alone does not establish intent or productivity"}
	if quality.UnknownEvents > 0 {
		qualifiers = append(qualifiers, fmt.Sprintf("%d unknown-total event(s) are excluded", quality.UnknownEvents))
	}
	card := InsightCard{
		ID:             "rhythm-peak-time",
		Category:       insightCategoryRhythm,
		Title:          "Peak time and day",
		Narrative:      fmt.Sprintf("The largest known-token buckets occur around %s local time and on %s; this describes timing only.", peakHour, peakWeekday),
		Observation:    fmt.Sprintf("%s local hour and %s are the highest known-token buckets", peakHour, peakWeekday),
		Evidence:       fmt.Sprintf("Peak local hour %s: %d tokens (%.1f%%); peak local day %s: %d tokens (%.1f%%).", peakHour, hourTokens, hourShare*100, peakWeekday, dayTokens, dayShare*100),
		Basis:          "known tokens grouped by event timestamp converted to the configured timezone; ties use earliest hour and Sunday-first weekday order",
		AnalyticsURL:   analyticsURL,
		AnalyticsQuery: query,
		Qualifiers:     qualifiers,
		PeakHour:       peakHour,
		PeakWeekday:    peakWeekday,
	}
	return card, maxFloat(hourShare, dayShare), true
}

func insightConcentrationCard(query AnalyticsQuery, analytics Analytics, quality InsightDataQuality, analyticsURL string) (InsightCard, float64, bool) {
	if analytics.Summary.Tokens <= 0 || len(analytics.Breakdown) == 0 {
		return InsightCard{}, 0, false
	}
	top := analytics.Breakdown[0]
	if top.Tokens <= 0 {
		return InsightCard{}, 0, false
	}
	share := float64(top.Tokens) / float64(analytics.Summary.Tokens)
	dimension := analyticsDimensionName(query.Dimension)
	qualifiers := []string{"share is calculated from known tokens only"}
	if quality.UnknownEvents > 0 {
		qualifiers = append(qualifiers, fmt.Sprintf("%d unknown-total event(s) are excluded", quality.UnknownEvents))
	}
	card := InsightCard{
		ID:             "dimension-concentration",
		Category:       insightCategoryConcentration,
		Title:          fmt.Sprintf("Top %s concentration", dimension),
		Narrative:      fmt.Sprintf("%s accounts for %.1f%% of known tokens in the selected %s dimension.", top.Name, share*100, strings.ToLower(dimension)),
		Observation:    fmt.Sprintf("%s: %.1f%% of known tokens", top.Name, share*100),
		Evidence:       fmt.Sprintf("Top %s %q: %d known tokens (%.1f%% of %d known tokens).", strings.ToLower(dimension), top.Name, top.Tokens, share*100, analytics.Summary.Tokens),
		Basis:          "dimension known-token total / window known-token total; rows with unknown totals do not contribute",
		AnalyticsURL:   analyticsURL,
		AnalyticsQuery: query,
		Qualifiers:     qualifiers,
	}
	return card, share, true
}

func insightCompositionCard(query AnalyticsQuery, aggregate insightAggregate, summary AnalyticsSummary, quality InsightDataQuality, analyticsURL string) (InsightCard, float64, bool) {
	if aggregate.knownTokens <= 0 || aggregate.componentEvents == 0 {
		return InsightCard{}, 0, false
	}
	componentTotal := aggregate.inputTokens + aggregate.cachedTokens + aggregate.outputTokens + aggregate.unclassified
	if componentTotal <= 0 {
		return InsightCard{}, 0, false
	}
	inputShare := float64(aggregate.inputTokens) / float64(componentTotal)
	cachedShare := float64(aggregate.cachedTokens) / float64(componentTotal)
	outputShare := float64(aggregate.outputTokens) / float64(componentTotal)
	costCoverage := float64(0)
	if summary.EstimatedCost.PricedTokens+summary.EstimatedCost.UnpricedTokens > 0 {
		costCoverage = float64(summary.EstimatedCost.PricedTokens) / float64(summary.EstimatedCost.PricedTokens+summary.EstimatedCost.UnpricedTokens)
	}
	cacheText := "cache data unavailable"
	if summary.Cache.EligibleTokens > 0 {
		cacheText = fmt.Sprintf("cache hit %.1f%%", summary.Cache.HitRate*100)
	}
	costText := "cost coverage unavailable"
	if summary.EstimatedCost.PricedTokens+summary.EstimatedCost.UnpricedTokens > 0 {
		costText = fmt.Sprintf("cost coverage %.1f%%", costCoverage*100)
	}
	qualifiers := []string{"component shares are based on reported component fields; unclassified remainder is retained"}
	if summary.EstimatedCost.UnpricedTokens > 0 || summary.EstimatedCost.PricedTokens == 0 {
		qualifiers = append(qualifiers, "estimated cost is partial or unavailable")
	}
	if quality.UnknownEvents > 0 {
		qualifiers = append(qualifiers, fmt.Sprintf("%d unknown-total event(s) are excluded", quality.UnknownEvents))
	}
	card := InsightCard{
		ID:             "token-composition",
		Category:       insightCategoryComposition,
		Title:          "Token composition and coverage",
		Narrative:      fmt.Sprintf("Known component fields are split %.1f%% input, %.1f%% cached input, and %.1f%% output; %s and %s.", inputShare*100, cachedShare*100, outputShare*100, cacheText, costText),
		Observation:    fmt.Sprintf("Input %.1f%% · cached %.1f%% · output %.1f%%", inputShare*100, cachedShare*100, outputShare*100),
		Evidence:       fmt.Sprintf("Component totals: input %d, cached %d, output %d, unclassified %d; %s; %s.", aggregate.inputTokens, aggregate.cachedTokens, aggregate.outputTokens, aggregate.unclassified, cacheText, costText),
		Basis:          "component / (input + cached + output + unclassified); cache hit = cached eligible tokens / eligible input+cache tokens; cost coverage = priced known tokens / priced+unpriced known tokens",
		AnalyticsURL:   analyticsURL,
		AnalyticsQuery: query,
		Qualifiers:     qualifiers,
	}
	score := maxFloat(cachedShare, outputShare)
	if costCoverage < 1 {
		score = maxFloat(score, 1-costCoverage)
	}
	return card, score, true
}

func insightConfidenceCard(query AnalyticsQuery, aggregate insightAggregate, summary AnalyticsSummary, quality InsightDataQuality, analyticsURL string) (InsightCard, float64, bool) {
	if summary.Events <= 0 {
		return InsightCard{}, 0, false
	}
	accuracyLabel := "accuracy classification unavailable"
	if len(quality.Accuracy) > 0 {
		parts := make([]string, 0, len(quality.Accuracy))
		for _, key := range []usage.Accuracy{usage.AccuracyReported, usage.AccuracyDerived, usage.AccuracyEstimated, usage.AccuracyUnknown} {
			if count := quality.Accuracy[key]; count > 0 {
				parts = append(parts, fmt.Sprintf("%d %s", count, key))
			}
		}
		if len(parts) > 0 {
			accuracyLabel = strings.Join(parts, ", ")
		}
	}
	narrative := fmt.Sprintf("%d of %d events have known totals (%.1f%% coverage); data confidence is %s.", quality.KnownEvents, summary.Events, quality.EventCoverage*100, quality.Confidence)
	if quality.UnknownEvents > 0 {
		narrative += " Unknown totals are excluded from token-volume conclusions."
	}
	card := InsightCard{
		ID:             "data-confidence",
		Category:       insightCategoryConfidence,
		Title:          "Data confidence",
		Narrative:      narrative,
		Observation:    fmt.Sprintf("%.1f%% of events have known totals", quality.EventCoverage*100),
		Evidence:       fmt.Sprintf("Known events: %d; unknown events: %d; token accuracy classes: %s.", quality.KnownEvents, quality.UnknownEvents, accuracyLabel),
		Basis:          "known-total events / all events; unknown totals remain unknown and do not become zero",
		AnalyticsURL:   analyticsURL,
		AnalyticsQuery: query,
		Qualifiers:     []string{quality.Qualifier},
	}
	score := 1 - quality.EventCoverage
	if aggregate.componentEvents < quality.KnownEvents {
		score = maxFloat(score, float64(quality.KnownEvents-aggregate.componentEvents)/float64(maxInt64(1, quality.KnownEvents)))
	}
	return card, score, true
}

func analyticsInsightURL(query AnalyticsQuery) string {
	values := url.Values{}
	values.Set("period", query.Period)
	values.Set("dimension", query.Dimension)
	if query.Machine != "" {
		values.Set("machine", query.Machine)
	}
	if query.Provider != "" {
		values.Set("provider", query.Provider)
	}
	if query.Model != "" {
		values.Set("model", query.Model)
	}
	if query.Tool != "" {
		values.Set("tool", query.Tool)
	}
	return "/analytics?" + values.Encode()
}

func analyticsDimensionName(dimension string) string {
	switch dimension {
	case "harnesses":
		return "Harnesses"
	case "providers":
		return "Providers"
	case "models":
		return "Models"
	case "machines":
		return "Machines"
	default:
		return "Projects"
	}
}

func absFloat(value float64) float64 {
	if value < 0 {
		return -value
	}
	return value
}

func maxFloat(left, right float64) float64 {
	if left > right {
		return left
	}
	return right
}

func maxInt64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}

func (s *Store) normalizeAnalyticsQuery(query AnalyticsQuery) (AnalyticsQuery, time.Time, time.Time) {
	switch query.Period {
	case "24h", "7d", "30d", "90d", "all":
	default:
		query.Period = "30d"
	}
	switch query.Dimension {
	case "projects", "harnesses", "providers", "models", "machines":
	default:
		query.Dimension = "projects"
	}
	now := query.Now.In(s.reportingLocation())
	if now.IsZero() {
		now = time.Now().In(s.reportingLocation())
	}
	today := s.dateOnly(now)
	end := today.AddDate(0, 0, 1)
	start := time.Time{}
	switch query.Period {
	case "24h":
		end = now
		start = now.Add(-24 * time.Hour)
	case "7d":
		start = today.AddDate(0, 0, -6)
	case "30d":
		start = today.AddDate(0, 0, -29)
	case "90d":
		start = today.AddDate(0, 0, -89)
	}
	query.Now = time.Time{}
	return query, start, end
}

func analyticsWhere(query AnalyticsQuery, start, end time.Time) (string, []any) {
	conditions := []string{"1 = 1"}
	args := make([]any, 0, 6)
	if !start.IsZero() {
		conditions = append(conditions, "timestamp >= ?")
		args = append(args, start.UTC().Format(time.RFC3339))
	}
	if !end.IsZero() {
		conditions = append(conditions, "timestamp < ?")
		args = append(args, end.UTC().Format(time.RFC3339))
	}
	if query.Machine != "" {
		conditions = append(conditions, "machine_id = ?")
		args = append(args, query.Machine)
	}
	if query.Provider != "" {
		conditions = append(conditions, "provider = ?")
		args = append(args, query.Provider)
	}
	if query.Model != "" {
		conditions = append(conditions, "COALESCE(NULLIF(canonical_model, ''), raw_model) = ?")
		args = append(args, query.Model)
	}
	if query.Tool != "" {
		conditions = append(conditions, "tool = ?")
		args = append(args, query.Tool)
	}
	return strings.Join(conditions, " AND "), args
}

func (s *Store) analyticsSummary(ctx context.Context, where string, args []any, summary *AnalyticsSummary) error {
	rows, err := s.db.QueryContext(ctx, `SELECT timestamp, total_tokens, cost,
input_tokens, cache_read_tokens, session_id, machine_id, provider, tool
FROM usage_events WHERE `+where, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	days := make(map[string]struct{})
	threads := make(map[string]struct{})
	for rows.Next() {
		var timestamp, machine, provider, tool string
		var total, input, cached sql.NullInt64
		var cost sql.NullFloat64
		var session sql.NullString
		if err := rows.Scan(&timestamp, &total, &cost, &input, &cached, &session, &machine, &provider, &tool); err != nil {
			return err
		}
		parsed, err := timeParse(timestamp)
		if err != nil {
			return err
		}
		days[parsed.In(s.reportingLocation()).Format("2006-01-02")] = struct{}{}
		summary.Events++
		if !total.Valid {
			summary.UnknownEvents++
		} else {
			summary.Tokens += total.Int64
			if cost.Valid {
				summary.EstimatedCost.PricedTokens += total.Int64
			} else {
				summary.EstimatedCost.UnpricedTokens += total.Int64
			}
		}
		if cost.Valid {
			summary.EstimatedCost.Amount += cost.Float64
		}
		if input.Valid && cached.Valid {
			summary.Cache.CachedTokens += cached.Int64
			summary.Cache.EligibleTokens += input.Int64 + cached.Int64
		}
		if session.Valid && strings.TrimSpace(session.String) != "" {
			if total.Valid {
				summary.SessionTokens += total.Int64
			}
			threads[machine+"\x1f"+provider+"\x1f"+tool+"\x1f"+session.String] = struct{}{}
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	summary.ActiveDays = int64(len(days))
	summary.Threads = int64(len(threads))
	if summary.ActiveDays > 0 {
		summary.AverageActiveDay = float64(summary.Tokens) / float64(summary.ActiveDays)
	}
	if summary.Cache.EligibleTokens > 0 {
		summary.Cache.HitRate = float64(summary.Cache.CachedTokens) / float64(summary.Cache.EligibleTokens)
	}
	return nil
}

func (s *Store) analyticsPoints(ctx context.Context, query AnalyticsQuery, where string, args []any) ([]AnalyticsPoint, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT timestamp, total_tokens, input_tokens,
cache_read_tokens, cache_write_tokens, output_tokens
FROM usage_events WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byDate := make(map[string]AnalyticsPoint)
	for rows.Next() {
		var timestamp string
		var total, input, cached, cacheWrite, output sql.NullInt64
		if err := rows.Scan(&timestamp, &total, &input, &cached, &cacheWrite, &output); err != nil {
			return nil, err
		}
		parsed, err := timeParse(timestamp)
		if err != nil {
			return nil, err
		}
		local := parsed.In(s.reportingLocation())
		bucket := ""
		switch query.Period {
		case "24h":
			hour := time.Date(local.Year(), local.Month(), local.Day(), local.Hour(), 0, 0, 0, s.reportingLocation())
			bucket = hour.UTC().Format(time.RFC3339)
		case "all":
			bucket = local.Format("2006-01") + "-01"
		default:
			bucket = local.Format("2006-01-02")
		}
		point := byDate[bucket]
		point.Date = bucket
		point.Events++
		if total.Valid {
			point.Tokens += total.Int64
		} else {
			point.UnknownEvents++
		}
		if input.Valid {
			point.InputTokens += input.Int64
		}
		if cached.Valid {
			point.CachedTokens += cached.Int64
		}
		if cacheWrite.Valid {
			point.CachedTokens += cacheWrite.Int64
		}
		if output.Valid {
			point.OutputTokens += output.Int64
		}
		byDate[bucket] = point
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	result := make([]AnalyticsPoint, 0, len(byDate))
	for _, point := range byDate {
		point.Label = analyticsPointLabel(point.Date, query.Period, s.reportingLocation())
		result = append(result, point)
	}
	sort.Slice(result, func(left, right int) bool { return result[left].Date < result[right].Date })
	return result, nil
}

func (s *Store) fillAnalyticsPoints(points []AnalyticsPoint, query AnalyticsQuery, start, end time.Time) []AnalyticsPoint {
	if len(points) == 0 {
		return points
	}
	byDate := make(map[string]AnalyticsPoint, len(points))
	for _, point := range points {
		byDate[point.Date] = point
	}

	location := s.reportingLocation()
	var first, last time.Time
	var step func(time.Time) time.Time
	switch query.Period {
	case "24h":
		startLocal := start.In(location)
		endLocal := end.In(location)
		first = time.Date(startLocal.Year(), startLocal.Month(), startLocal.Day(), startLocal.Hour(), 0, 0, 0, location)
		lastLocal := time.Date(endLocal.Year(), endLocal.Month(), endLocal.Day(), endLocal.Hour(), 0, 0, 0, location)
		last = lastLocal.Add(time.Hour)
		step = func(value time.Time) time.Time { return value.Add(time.Hour) }
	case "all":
		parsed, err := time.Parse("2006-01-02", points[0].Date)
		if err != nil {
			return points
		}
		first = time.Date(parsed.Year(), parsed.Month(), 1, 0, 0, 0, 0, location)
		endDate := end.Add(-time.Nanosecond).In(location)
		last = time.Date(endDate.Year(), endDate.Month(), 1, 0, 0, 0, 0, location).AddDate(0, 1, 0)
		step = func(value time.Time) time.Time { return value.AddDate(0, 1, 0) }
	default:
		first = s.dateOnly(start)
		last = s.dateOnly(end)
		step = func(value time.Time) time.Time { return value.AddDate(0, 0, 1) }
	}

	result := make([]AnalyticsPoint, 0, len(points))
	for cursor := first; cursor.Before(last); cursor = step(cursor) {
		key := cursor.Format("2006-01-02")
		if query.Period == "24h" {
			key = cursor.UTC().Format(time.RFC3339)
		} else if query.Period == "all" {
			key = cursor.Format("2006-01-02")
		}
		point, ok := byDate[key]
		if !ok {
			point = AnalyticsPoint{Date: key}
		}
		point.Label = analyticsPointLabel(point.Date, query.Period, location)
		result = append(result, point)
	}
	return result
}

func analyticsPointLabel(date, period string, location *time.Location) string {
	if period == "24h" {
		parsed, err := time.Parse(time.RFC3339, date)
		if err != nil {
			return date
		}
		return parsed.In(location).Format("Jan 2 15:00")
	}
	parsed, err := time.Parse("2006-01-02", date)
	if err != nil {
		return date
	}
	if period == "all" {
		return parsed.Format("Jan 2006")
	}
	return parsed.Format("Jan 2")
}

func analyticsBreakdownExpression(dimension string) string {
	switch dimension {
	case "harnesses":
		return "NULLIF(tool, '')"
	case "providers":
		return "NULLIF(provider, '')"
	case "models":
		return "NULLIF(COALESCE(NULLIF(canonical_model, ''), raw_model), '')"
	case "machines":
		return "NULLIF(machine_id, '')"
	default:
		return "NULLIF(project, '')"
	}
}

func (s *Store) analyticsBreakdown(ctx context.Context, query AnalyticsQuery, where string, args []any, total int64) ([]AnalyticsBreakdown, error) {
	expression := "COALESCE(" + analyticsBreakdownExpression(query.Dimension) + ", 'Unknown')"
	rows, err := s.db.QueryContext(ctx, `SELECT `+expression+`,
COALESCE(SUM(total_tokens), 0),
COUNT(*),
COUNT(DISTINCT CASE WHEN NULLIF(session_id, '') IS NOT NULL THEN machine_id || char(31) || provider || char(31) || tool || char(31) || session_id END)
FROM usage_events WHERE `+where+` GROUP BY 1 ORDER BY 2 DESC, 1`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []AnalyticsBreakdown
	for rows.Next() {
		var item AnalyticsBreakdown
		if err := rows.Scan(&item.Name, &item.Tokens, &item.Events, &item.Sessions); err != nil {
			return nil, err
		}
		if total > 0 {
			item.Share = float64(item.Tokens) / float64(total)
		}
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) analyticsShareTimeline(ctx context.Context, query AnalyticsQuery, where string, args []any, start, end time.Time, breakdown []AnalyticsBreakdown, total int64) ([]AnalyticsShareSeries, []AnalyticsSharePoint, error) {
	const visibleSeries = 5
	series := make([]AnalyticsShareSeries, 0, visibleSeries+1)
	selected := make(map[string]int, visibleSeries)
	for _, item := range breakdown {
		if len(series) >= visibleSeries {
			break
		}
		selected[item.Name] = len(series)
		entry := AnalyticsShareSeries{Name: item.Name, Tokens: item.Tokens}
		if total > 0 {
			entry.Share = float64(item.Tokens) / float64(total)
		}
		series = append(series, entry)
	}
	if len(breakdown) > visibleSeries {
		other := AnalyticsShareSeries{Name: "Other"}
		for _, item := range breakdown[visibleSeries:] {
			other.Tokens += item.Tokens
		}
		if total > 0 {
			other.Share = float64(other.Tokens) / float64(total)
		}
		series = append(series, other)
	}
	if len(series) == 0 {
		return nil, nil, nil
	}

	expression := "COALESCE(" + analyticsBreakdownExpression(query.Dimension) + ", 'Unknown')"
	rows, err := s.db.QueryContext(ctx, `SELECT timestamp, total_tokens, `+expression+`
FROM usage_events WHERE `+where, args...)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	byDate := make(map[string]AnalyticsSharePoint)
	for rows.Next() {
		var timestamp, name string
		var tokens sql.NullInt64
		if err := rows.Scan(&timestamp, &tokens, &name); err != nil {
			return nil, nil, err
		}
		parsed, err := timeParse(timestamp)
		if err != nil {
			return nil, nil, err
		}
		bucket := analyticsBucketKey(parsed.In(s.reportingLocation()), query.Period, s.reportingLocation())
		point := byDate[bucket]
		point.Date = bucket
		if len(point.Values) == 0 {
			point.Values = make([]AnalyticsShareSeries, len(series))
			copy(point.Values, series)
			for index := range point.Values {
				point.Values[index].Tokens = 0
				point.Values[index].Share = 0
			}
		}
		if !tokens.Valid {
			point.UnknownEvents++
			byDate[bucket] = point
			continue
		}
		point.Tokens += tokens.Int64
		index, ok := selected[name]
		if !ok && len(series) > visibleSeries {
			index = len(series) - 1
			ok = true
		}
		if ok {
			point.Values[index].Tokens += tokens.Int64
		}
		byDate[bucket] = point
	}
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}
	points := s.fillAnalyticsSharePoints(byDate, series, query, start, end)
	return series, points, nil
}

func analyticsBucketKey(local time.Time, period string, location *time.Location) string {
	switch period {
	case "24h":
		hour := time.Date(local.Year(), local.Month(), local.Day(), local.Hour(), 0, 0, 0, location)
		return hour.UTC().Format(time.RFC3339)
	case "all":
		return local.Format("2006-01") + "-01"
	default:
		return local.Format("2006-01-02")
	}
}

func (s *Store) fillAnalyticsSharePoints(byDate map[string]AnalyticsSharePoint, series []AnalyticsShareSeries, query AnalyticsQuery, start, end time.Time) []AnalyticsSharePoint {
	if len(byDate) == 0 {
		return nil
	}
	seed := make([]AnalyticsPoint, 0, len(byDate))
	for _, point := range byDate {
		seed = append(seed, AnalyticsPoint{Date: point.Date})
	}
	sort.Slice(seed, func(left, right int) bool { return seed[left].Date < seed[right].Date })
	filled := s.fillAnalyticsPoints(seed, query, start, end)
	result := make([]AnalyticsSharePoint, 0, len(filled))
	for _, base := range filled {
		point, ok := byDate[base.Date]
		if !ok {
			point = AnalyticsSharePoint{Date: base.Date, Values: make([]AnalyticsShareSeries, len(series))}
			copy(point.Values, series)
			for index := range point.Values {
				point.Values[index].Tokens = 0
				point.Values[index].Share = 0
			}
		}
		point.Label = analyticsPointLabel(point.Date, query.Period, s.reportingLocation())
		if point.Tokens > 0 {
			for index := range point.Values {
				point.Values[index].Share = float64(point.Values[index].Tokens) / float64(point.Tokens)
			}
		}
		result = append(result, point)
	}
	return result
}

func (s *Store) analyticsSessions(ctx context.Context, where string, args []any) ([]AnalyticsSession, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT
MAX(timestamp),
MAX(NULLIF(session_id, '')),
machine_id,
provider,
COALESCE(NULLIF(MAX(NULLIF(canonical_model, '')), ''), MAX(raw_model)),
MAX(tool),
MAX(project),
COALESCE(SUM(total_tokens), 0),
COUNT(*),
COALESCE(SUM(CASE WHEN total_tokens IS NULL THEN 1 ELSE 0 END), 0)
FROM usage_events WHERE `+where+` AND NULLIF(session_id, '') IS NOT NULL
GROUP BY COALESCE(NULLIF(session_id, ''), event_id), machine_id, provider, tool
ORDER BY 1 DESC LIMIT 12`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var result []AnalyticsSession
	for rows.Next() {
		var item AnalyticsSession
		var sessionID, project sql.NullString
		if err := rows.Scan(&item.Timestamp, &sessionID, &item.Machine, &item.Provider, &item.Model, &item.Tool, &project, &item.Tokens, &item.Events, &item.UnknownEvents); err != nil {
			return nil, err
		}
		item.SessionID = sessionID.String
		item.Project = project.String
		result = append(result, item)
	}
	return result, rows.Err()
}

func (s *Store) analyticsFacets(ctx context.Context) (AnalyticsFacets, error) {
	var result AnalyticsFacets
	queries := []struct {
		target     *[]string
		expression string
	}{
		{target: &result.Machines, expression: "NULLIF(machine_id, '')"},
		{target: &result.Providers, expression: "NULLIF(provider, '')"},
		{target: &result.Models, expression: "NULLIF(COALESCE(NULLIF(canonical_model, ''), raw_model), '')"},
		{target: &result.Tools, expression: "NULLIF(tool, '')"},
	}
	for _, item := range queries {
		rows, err := s.db.QueryContext(ctx, `SELECT DISTINCT `+item.expression+` FROM usage_events WHERE `+item.expression+` IS NOT NULL ORDER BY 1`)
		if err != nil {
			return AnalyticsFacets{}, err
		}
		for rows.Next() {
			var value string
			if err := rows.Scan(&value); err != nil {
				rows.Close()
				return AnalyticsFacets{}, err
			}
			*item.target = append(*item.target, value)
		}
		if err := rows.Close(); err != nil {
			return AnalyticsFacets{}, err
		}
	}
	return result, nil
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
