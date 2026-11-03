// Package zcode reads metadata-only token usage from ZCode's local SQLite
// store. Terminal CLI sessions, desktop-app sessions, and subagents all write
// into the same database, so one source covers every ZCode mode. It
// deliberately never queries the message, part, or session_input tables,
// which contain prompts, responses, and tool payloads.
package zcode

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/tokemon/tokemon/internal/adapters"
	"github.com/tokemon/tokemon/internal/usage"
)

const (
	adapterID      = "zcode"
	adapterVersion = "0.1.0"
	cursorVersion  = "1"
	databaseName   = "db.sqlite"
)

type Adapter struct {
	root  string
	cache *adapters.SnapshotCache
}

func New(home string) *Adapter {
	return &Adapter{
		root:  filepath.Join(home, ".zcode", "cli", "db"),
		cache: adapters.NewSnapshotCache(),
	}
}

func (a *Adapter) ID() string { return adapterID }

func (a *Adapter) NormalizeModel(raw string) string {
	if model := strings.TrimSpace(raw); model != "" {
		return model
	}
	return "unknown"
}

func (a *Adapter) Capabilities() adapters.Capabilities {
	return adapters.Capabilities{
		InputTokens: true, OutputTokens: true, CacheTokens: true,
		ReasoningTokens: true, SessionID: true, Duration: true,
	}
}

func (a *Adapter) Discover(ctx context.Context) ([]adapters.Source, error) {
	path := filepath.Join(a.root, databaseName)
	var sources []adapters.Source
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		a.cache.Prune(nil)
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if info.Mode().IsRegular() {
		sources = append(sources, adapters.Source{Path: path, Identity: a.sourceIdentity(path)})
	}
	seen := make(map[string]struct{}, len(sources))
	for _, source := range sources {
		seen[source.Path] = struct{}{}
	}
	a.cache.Prune(seen)
	sort.Slice(sources, func(i, j int) bool { return sources[i].Path < sources[j].Path })
	return sources, nil
}

func (a *Adapter) Parse(ctx context.Context, source adapters.Source, request adapters.ParseRequest) (adapters.ParseResult, error) {
	if strings.TrimSpace(request.MachineID) == "" {
		return adapters.ParseResult{}, errors.New("machine ID is required")
	}
	signature, err := adapters.Signature(source.Path)
	if err != nil {
		return adapters.ParseResult{}, err
	}
	identity := source.Identity
	if identity == "" {
		identity = a.sourceIdentity(source.Path)
	}
	cursorIdentity := adapterID + ":" + cursorVersion + ":" + identity
	if cached, err, ok := a.cache.Lookup(source.Path, signature, cursorIdentity, request.Cursor); ok {
		return cached, err
	}

	result, err := a.parseUncached(ctx, source.Path, request.MachineID, identity, cursorIdentity)
	a.cache.Store(source.Path, signature, cursorIdentity, request.Cursor, result, err)
	return result, err
}

// tokenCounts mirrors ZCode's per-request usage columns. ZCode's input_tokens
// is normalized and already includes cache reads (and cache creation when the
// provider reports it), so nonCachedInput subtracts both to keep the
// prompt/cache/output split additive.
type tokenCounts struct {
	input, output, cacheRead, cacheWrite, reasoning int64
}

func (c tokenCounts) nonCachedInput() int64 {
	value := c.input - c.cacheRead - c.cacheWrite
	if value < 0 {
		return 0
	}
	return value
}

func (c tokenCounts) total() int64 {
	return c.nonCachedInput() + c.output + c.cacheRead + c.cacheWrite
}

func (c tokenCounts) nonZero() bool {
	return c.input > 0 || c.output > 0 || c.cacheRead > 0 || c.cacheWrite > 0 || c.reasoning > 0
}

type sessionAggregate struct {
	sessionID, model, provider, directory string
	startedAt, completedAt                float64
	durationMS                            int64
	tokens                                tokenCounts
}

func (a *Adapter) parseUncached(ctx context.Context, path, machineID, identity, cursorIdentity string) (adapters.ParseResult, error) {
	result := adapters.ParseResult{Cursor: adapters.Cursor{Identity: cursorIdentity}}
	db, closeDB, err := a.openSource(ctx, path)
	if err != nil {
		return adapters.ParseResult{}, err
	}
	defer closeDB()

	hasUsage, err := adapters.HasTable(ctx, db, "model_usage")
	if err != nil {
		return adapters.ParseResult{}, err
	}
	if !hasUsage {
		return result, nil
	}
	usageColumns, err := columns(ctx, db, "model_usage")
	if err != nil {
		return adapters.ParseResult{}, err
	}
	for _, required := range []string{"session_id", "model_id", "provider_id", "input_tokens", "output_tokens", "started_at"} {
		if !usageColumns[required] {
			// An older or unknown schema means usage metadata cannot be read.
			// Warn once per poll so agents do not silently report zero usage.
			slog.Warn("zcode model_usage schema lacks required columns; usage metadata skipped",
				"missing", required)
			return result, nil
		}
	}

	// The session table is optional: subagent sessions always carry rows, but
	// a LEFT JOIN keeps usage events alive even when a session row is missing.
	projectExpression := "''"
	if hasSession, err := adapters.HasTable(ctx, db, "session"); err != nil {
		return adapters.ParseResult{}, err
	} else if hasSession {
		sessionColumns, err := columns(ctx, db, "session")
		if err != nil {
			return adapters.ParseResult{}, err
		}
		if sessionColumns["directory"] {
			// The LEFT JOIN yields NULL for sessions without a session row.
			projectExpression = "COALESCE(s.directory, '')"
		}
	}

	aggregates, order, err := readAggregates(ctx, db, usageColumns, projectExpression)
	if err != nil {
		return adapters.ParseResult{}, err
	}

	for _, key := range order {
		aggregate := aggregates[key]
		if !aggregate.tokens.nonZero() {
			continue
		}
		timestamp := unixMilli(firstPositive(aggregate.completedAt, aggregate.startedAt))
		if timestamp.IsZero() {
			continue
		}
		result.Events = append(result.Events, a.event(machineID, identity, aggregate, timestamp))
	}
	return result, nil
}

func readAggregates(ctx context.Context, db *sql.DB, c map[string]bool, projectExpression string) (map[string]*sessionAggregate, []string, error) {
	query := `SELECT u.session_id, u.model_id, ` + textExpr(c, "provider_id") + `, ` + projectExpression + `,
       MIN(u.started_at), MAX(COALESCE(u.completed_at, u.started_at)),
       ` + qualifiedSumExpr(c, "u", "duration_ms") + `,
       SUM(COALESCE(u.input_tokens, 0)), SUM(COALESCE(u.output_tokens, 0)),
       ` + qualifiedSumExpr(c, "u", "cache_read_input_tokens") + `,
       ` + qualifiedSumExpr(c, "u", "cache_creation_input_tokens") + `,
       ` + qualifiedSumExpr(c, "u", "reasoning_tokens") + `
FROM model_usage u
LEFT JOIN session s ON s.id = u.session_id
WHERE COALESCE(u.input_tokens, 0) + COALESCE(u.output_tokens, 0) +
      COALESCE(u.cache_read_input_tokens, 0) + COALESCE(u.cache_creation_input_tokens, 0) +
      COALESCE(u.reasoning_tokens, 0) > 0
GROUP BY u.session_id, u.model_id, ` + textExpr(c, "provider_id") + `
ORDER BY 5, u.session_id, u.model_id`
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, nil, fmt.Errorf("read ZCode model usage: %w", err)
	}
	defer rows.Close()

	aggregates := make(map[string]*sessionAggregate)
	var order []string
	for rows.Next() {
		var aggregate sessionAggregate
		var provider, directory string
		if err := rows.Scan(
			&aggregate.sessionID, &aggregate.model, &provider, &directory,
			&aggregate.startedAt, &aggregate.completedAt,
			&aggregate.durationMS,
			&aggregate.tokens.input, &aggregate.tokens.output,
			&aggregate.tokens.cacheRead, &aggregate.tokens.cacheWrite,
			&aggregate.tokens.reasoning,
		); err != nil {
			return nil, nil, err
		}
		aggregate.provider = strings.TrimSpace(provider)
		aggregate.directory = directory
		aggregate.durationMS = max64(0, aggregate.durationMS)
		aggregate.tokens = safeCounts(aggregate.tokens)
		if strings.TrimSpace(aggregate.sessionID) == "" || !aggregate.tokens.nonZero() {
			continue
		}
		key := strings.Join([]string{aggregate.sessionID, aggregate.model, aggregate.provider}, "\x00")
		if _, ok := aggregates[key]; !ok {
			order = append(order, key)
		}
		aggregates[key] = &aggregate
	}
	return aggregates, order, rows.Err()
}

func (a *Adapter) event(machineID, identity string, aggregate *sessionAggregate, timestamp time.Time) usage.Event {
	provider := aggregate.provider
	if provider == "" {
		provider = "unknown"
	}
	event := usage.Event{
		SchemaVersion:    usage.SchemaVersion,
		EventID:          usage.DeterministicID(machineID, adapterID, identity, 0, rowKey(aggregate), aggregate.sessionID),
		Timestamp:        timestamp,
		MachineID:        machineID,
		SessionID:        usage.NormalizeSessionID(aggregate.sessionID),
		Project:          usage.NormalizeProject(aggregate.directory),
		Provider:         provider,
		Model:            a.NormalizeModel(aggregate.model),
		Tool:             adapterID,
		InputTokens:      usage.Int64(aggregate.tokens.nonCachedInput()),
		OutputTokens:     usage.Int64(aggregate.tokens.output),
		CacheReadTokens:  usage.Int64(aggregate.tokens.cacheRead),
		CacheWriteTokens: usage.Int64(aggregate.tokens.cacheWrite),
		ReasoningTokens:  usage.Int64(aggregate.tokens.reasoning),
		TotalTokens:      usage.Int64(aggregate.tokens.total()),
		Currency:         "USD",
		TokenAccuracy:    usage.AccuracyDerived,
		Source:           usage.Source{Adapter: adapterID, AdapterVersion: adapterVersion, Identity: identity},
	}
	if aggregate.durationMS > 0 {
		event.DurationMS = usage.Int64(aggregate.durationMS)
	}
	return event
}

func rowKey(aggregate *sessionAggregate) string {
	return strings.Join([]string{aggregate.model, aggregate.provider}, "\x00")
}

// openSource opens the database read-only. ZCode keeps the database open with
// a hot WAL, so if the direct read-only open fails (for example when the
// shared-memory sidecar would need recovery), the database and its sidecars
// are copied to a temporary directory and the copy is opened instead. The
// originals are never written to.
func (a *Adapter) openSource(ctx context.Context, path string) (*sql.DB, func(), error) {
	db, err := adapters.OpenReadOnly(ctx, path)
	if err == nil {
		return db, func() { _ = db.Close() }, nil
	}
	snapshot, cleanup, snapErr := snapshotCopy(path)
	if snapErr != nil {
		return nil, func() {}, fmt.Errorf("open %s read-only: %w", path, err)
	}
	db, err = adapters.OpenReadOnly(ctx, snapshot)
	if err != nil {
		cleanup()
		return nil, func() {}, err
	}
	return db, func() { _ = db.Close(); cleanup() }, nil
}

// snapshotCopy copies the database and its WAL sidecars to a temporary
// directory. The copy can observe a torn live snapshot, which the snapshot
// cache signature invalidates on the next poll, so a copy is a fallback for
// reading something rather than failing the poll outright.
func snapshotCopy(path string) (string, func(), error) {
	dir, err := os.MkdirTemp("", "tokemon-zcode-")
	if err != nil {
		return "", func() {}, err
	}
	cleanup := func() { _ = os.RemoveAll(dir) }
	target := filepath.Join(dir, databaseName)
	for _, source := range []string{path, path + "-wal", path + "-shm"} {
		if err := copyFile(source, filepath.Join(dir, filepath.Base(source))); err != nil && !os.IsNotExist(err) {
			cleanup()
			return "", func() {}, err
		}
	}
	return target, cleanup, nil
}

func copyFile(source, target string) error {
	in, err := os.Open(source)
	if err != nil {
		return err
	}
	defer in.Close()
	out, err := os.Create(target)
	if err != nil {
		return err
	}
	defer out.Close()
	if _, err := io.Copy(out, in); err != nil {
		return err
	}
	return out.Close()
}

func (a *Adapter) sourceIdentity(path string) string {
	relative, err := filepath.Rel(a.root, path)
	if err != nil {
		relative = filepath.Base(path)
	}
	return adapters.HashIdentity(adapterID + ":" + filepath.ToSlash(relative))
}

func columns(ctx context.Context, db *sql.DB, table string) (map[string]bool, error) {
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	result := make(map[string]bool)
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, kind string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &primaryKey); err != nil {
			return nil, err
		}
		result[name] = true
	}
	return result, rows.Err()
}

func textExpr(c map[string]bool, name string) string {
	if c[name] {
		return `COALESCE(` + name + `, '')`
	}
	return `''`
}

func qualifiedSumExpr(c map[string]bool, alias, name string) string {
	if c[name] {
		return `SUM(COALESCE(` + alias + `.` + name + `, 0))`
	}
	return `0`
}

// unixMilli converts an epoch float from the database. ZCode stores
// millisecond timestamps; adapters.UnixTime accepts seconds through
// nanoseconds so older or future unit changes still parse.
func unixMilli(value float64) time.Time {
	if value <= 0 {
		return time.Time{}
	}
	return adapters.UnixTime(int64(value)).UTC()
}

func firstPositive(values ...float64) float64 {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}

func safeCounts(c tokenCounts) tokenCounts {
	c.input = max64(0, c.input)
	c.output = max64(0, c.output)
	c.cacheRead = max64(0, c.cacheRead)
	c.cacheWrite = max64(0, c.cacheWrite)
	c.reasoning = max64(0, c.reasoning)
	return c
}

func max64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}

var _ adapters.Adapter = (*Adapter)(nil)
