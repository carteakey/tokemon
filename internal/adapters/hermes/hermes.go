// Package hermes reads metadata-only token usage from Hermes Agent's local
// SQLite session store. It deliberately never queries the messages table,
// which contains prompts, responses, reasoning, and tool payloads.
package hermes

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
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
	adapterID      = "hermes-agent"
	adapterVersion = "0.1.0"
	cursorVersion  = "1"
)

type Adapter struct {
	root  string
	cache *adapters.SnapshotCache
}

func New(home string) *Adapter {
	return &Adapter{
		root:  filepath.Join(home, ".hermes"),
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
		ReasoningTokens: true, Cost: true, SessionID: true,
	}
}

func (a *Adapter) Discover(ctx context.Context) ([]adapters.Source, error) {
	paths := []string{filepath.Join(a.root, "state.db")}
	profilesRoot := filepath.Join(a.root, "profiles")
	entries, err := os.ReadDir(profilesRoot)
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	for _, entry := range entries {
		if entry.IsDir() {
			paths = append(paths, filepath.Join(profilesRoot, entry.Name(), "state.db"))
		}
	}

	var sources []adapters.Source
	seen := make(map[string]struct{})
	for _, path := range paths {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		info, err := os.Stat(path)
		if os.IsNotExist(err) || (err == nil && !info.Mode().IsRegular()) {
			continue
		}
		if err != nil {
			return nil, err
		}
		identity := a.sourceIdentity(path)
		sources = append(sources, adapters.Source{Path: path, Identity: identity})
		seen[path] = struct{}{}
	}
	sort.Slice(sources, func(i, j int) bool { return sources[i].Path < sources[j].Path })
	a.cache.Prune(seen)
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

type tokenCounts struct {
	input, output, cacheRead, cacheWrite, reasoning int64
}

func (c tokenCounts) total() int64 {
	// Hermes treats reasoning as a subset of output tokens. Its canonical
	// total is prompt (input + cache) plus output, so reasoning is reported as
	// a useful component without being counted twice.
	return c.input + c.output + c.cacheRead + c.cacheWrite
}

func (c tokenCounts) nonZero() bool {
	return c.input > 0 || c.output > 0 || c.cacheRead > 0 || c.cacheWrite > 0 || c.reasoning > 0
}

func (c tokenCounts) subtract(other tokenCounts) tokenCounts {
	return tokenCounts{
		input:      max64(0, c.input-other.input),
		output:     max64(0, c.output-other.output),
		cacheRead:  max64(0, c.cacheRead-other.cacheRead),
		cacheWrite: max64(0, c.cacheWrite-other.cacheWrite),
		reasoning:  max64(0, c.reasoning-other.reasoning),
	}
}

type sessionAggregate struct {
	id, model, provider, cwd  string
	startedAt, endedAt        float64
	tokens                    tokenCounts
	estimatedCost, actualCost float64
}

func (a *Adapter) parseUncached(ctx context.Context, path, machineID, identity, cursorIdentity string) (adapters.ParseResult, error) {
	result := adapters.ParseResult{Cursor: adapters.Cursor{Identity: cursorIdentity}}
	db, err := adapters.OpenReadOnly(ctx, path)
	if err != nil {
		return adapters.ParseResult{}, err
	}
	defer db.Close()

	hasSessions, err := adapters.HasTable(ctx, db, "sessions")
	if err != nil {
		return adapters.ParseResult{}, err
	}
	if !hasSessions {
		return result, nil
	}
	sessionColumns, err := columns(ctx, db, "sessions")
	if err != nil {
		return adapters.ParseResult{}, err
	}
	for _, required := range []string{"id", "started_at", "input_tokens", "output_tokens"} {
		if !sessionColumns[required] {
			// An older or local schema means usage metadata cannot be read.
			// Warn once per poll so agents do not silently report zero usage.
			slog.Warn("hermes state schema lacks required session columns; usage metadata skipped",
				"path", path, "missing", required)
			return result, nil
		}
	}

	sessions, sessionOrder, err := readSessions(ctx, db, sessionColumns)
	if err != nil {
		return adapters.ParseResult{}, err
	}
	attributed := make(map[string]tokenCounts)

	hasModelUsage, err := adapters.HasTable(ctx, db, "session_model_usage")
	if err != nil {
		return adapters.ParseResult{}, err
	}
	if hasModelUsage {
		modelColumns, err := columns(ctx, db, "session_model_usage")
		if err != nil {
			return adapters.ParseResult{}, err
		}
		if modelColumns["session_id"] && modelColumns["model"] && modelColumns["input_tokens"] && modelColumns["output_tokens"] {
			events, err := a.readModelUsage(ctx, db, sessionColumns, modelColumns, machineID, identity, attributed)
			if err != nil {
				return adapters.ParseResult{}, err
			}
			result.Events = append(result.Events, events...)
		}
	}

	for _, sessionID := range sessionOrder {
		session := sessions[sessionID]
		residual := session.tokens.subtract(attributed[sessionID])
		if !residual.nonZero() {
			continue
		}
		// Per-model rows already carry the best route-level cost available.
		// Session estimated and actual totals are alternate accounting views,
		// not additive values, so only attach the aggregate cost when no
		// model usage exists. This avoids counting an estimate alongside the
		// actual charge for the same calls.
		estimated, actual := 0.0, 0.0
		if !attributed[sessionID].nonZero() {
			estimated, actual = session.estimatedCost, session.actualCost
		}
		timestamp := unixFloat(firstPositive(session.endedAt, session.startedAt))
		if timestamp.IsZero() {
			continue
		}
		result.Events = append(result.Events, a.event(
			machineID, identity, "session-residual", session.id,
			timestamp, usage.NormalizeProject(session.cwd), session.provider,
			session.model, residual, estimated, actual,
		))
	}
	return result, nil
}

func readSessions(ctx context.Context, db *sql.DB, c map[string]bool) (map[string]sessionAggregate, []string, error) {
	query := `SELECT id, ` + textExpr(c, "model") + `, started_at, ` + numberExpr(c, "ended_at") + `,
       input_tokens, output_tokens, ` + integerExpr(c, "cache_read_tokens") + `,
       ` + integerExpr(c, "cache_write_tokens") + `, ` + integerExpr(c, "reasoning_tokens") + `,
       ` + textExpr(c, "cwd") + `, ` + textExpr(c, "billing_provider") + `,
       ` + numberExpr(c, "estimated_cost_usd") + `, ` + numberExpr(c, "actual_cost_usd") + `
FROM sessions
WHERE COALESCE(input_tokens, 0) + COALESCE(output_tokens, 0) + ` + integerExpr(c, "cache_read_tokens") + ` +
      ` + integerExpr(c, "cache_write_tokens") + ` + ` + integerExpr(c, "reasoning_tokens") + ` > 0
ORDER BY started_at, id`
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, nil, fmt.Errorf("read Hermes sessions: %w", err)
	}
	defer rows.Close()

	sessions := make(map[string]sessionAggregate)
	var order []string
	for rows.Next() {
		var s sessionAggregate
		if err := rows.Scan(
			&s.id, &s.model, &s.startedAt, &s.endedAt,
			&s.tokens.input, &s.tokens.output, &s.tokens.cacheRead,
			&s.tokens.cacheWrite, &s.tokens.reasoning, &s.cwd, &s.provider,
			&s.estimatedCost, &s.actualCost,
		); err != nil {
			return nil, nil, err
		}
		s.tokens = safeCounts(s.tokens)
		if strings.TrimSpace(s.id) == "" || s.startedAt <= 0 {
			continue
		}
		sessions[s.id] = s
		order = append(order, s.id)
	}
	return sessions, order, rows.Err()
}

func (a *Adapter) readModelUsage(
	ctx context.Context,
	db *sql.DB,
	sessionColumns, modelColumns map[string]bool,
	machineID, identity string,
	attributed map[string]tokenCounts,
) ([]usage.Event, error) {
	query := `SELECT u.session_id, u.model, ` + qualifiedTextExpr(modelColumns, "u", "billing_provider") + `,
       ` + qualifiedTextExpr(modelColumns, "u", "billing_base_url") + `,
       ` + qualifiedTextExpr(modelColumns, "u", "billing_mode") + `,
       ` + qualifiedTextExpr(modelColumns, "u", "task") + `,
       COALESCE(u.input_tokens, 0), COALESCE(u.output_tokens, 0),
       ` + qualifiedIntegerExpr(modelColumns, "u", "cache_read_tokens") + `,
       ` + qualifiedIntegerExpr(modelColumns, "u", "cache_write_tokens") + `,
       ` + qualifiedIntegerExpr(modelColumns, "u", "reasoning_tokens") + `,
       ` + qualifiedNumberExpr(modelColumns, "u", "estimated_cost_usd") + `,
       ` + qualifiedNumberExpr(modelColumns, "u", "actual_cost_usd") + `,
       ` + qualifiedNumberExpr(modelColumns, "u", "last_seen") + `,
       s.started_at, ` + qualifiedNumberExpr(sessionColumns, "s", "ended_at") + `,
       ` + qualifiedTextExpr(sessionColumns, "s", "cwd") + `
FROM session_model_usage u
JOIN sessions s ON s.id = u.session_id
WHERE COALESCE(u.input_tokens, 0) + COALESCE(u.output_tokens, 0) +
      ` + qualifiedIntegerExpr(modelColumns, "u", "cache_read_tokens") + ` +
      ` + qualifiedIntegerExpr(modelColumns, "u", "cache_write_tokens") + ` +
      ` + qualifiedIntegerExpr(modelColumns, "u", "reasoning_tokens") + ` > 0
ORDER BY u.session_id, u.model, 3, 4, 5, 6`
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("read Hermes model usage: %w", err)
	}
	defer rows.Close()

	var events []usage.Event
	for rows.Next() {
		var sessionID, model, provider, baseURL, billingMode, task, cwd string
		var tokens tokenCounts
		var estimated, actual, lastSeen, startedAt, endedAt float64
		if err := rows.Scan(
			&sessionID, &model, &provider, &baseURL, &billingMode, &task,
			&tokens.input, &tokens.output, &tokens.cacheRead, &tokens.cacheWrite,
			&tokens.reasoning, &estimated, &actual, &lastSeen, &startedAt, &endedAt, &cwd,
		); err != nil {
			return nil, err
		}
		tokens = safeCounts(tokens)
		if strings.TrimSpace(sessionID) == "" || !tokens.nonZero() {
			continue
		}
		timestamp := unixFloat(firstPositive(lastSeen, endedAt, startedAt))
		if timestamp.IsZero() {
			continue
		}
		rowKey := strings.Join([]string{model, provider, baseURL, billingMode, task}, "\x00")
		events = append(events, a.event(
			machineID, identity, rowKey, sessionID, timestamp,
			usage.NormalizeProject(cwd), provider, model, tokens, estimated, actual,
		))
		current := attributed[sessionID]
		current.input += tokens.input
		current.output += tokens.output
		current.cacheRead += tokens.cacheRead
		current.cacheWrite += tokens.cacheWrite
		current.reasoning += tokens.reasoning
		attributed[sessionID] = current
	}
	return events, rows.Err()
}

func (a *Adapter) event(machineID, identity, rowKey, sessionID string, timestamp time.Time, project, provider, model string, tokens tokenCounts, estimatedCost, actualCost float64) usage.Event {
	provider = strings.TrimSpace(provider)
	if provider == "" {
		provider = "unknown"
	}
	event := usage.Event{
		SchemaVersion:    usage.SchemaVersion,
		EventID:          usage.DeterministicID(machineID, adapterID, identity, 0, rowKey, sessionID),
		Timestamp:        timestamp,
		MachineID:        machineID,
		SessionID:        sessionID,
		Project:          project,
		Provider:         provider,
		Model:            a.NormalizeModel(model),
		Tool:             adapterID,
		InputTokens:      usage.Int64(tokens.input),
		OutputTokens:     usage.Int64(tokens.output),
		CacheReadTokens:  usage.Int64(tokens.cacheRead),
		CacheWriteTokens: usage.Int64(tokens.cacheWrite),
		ReasoningTokens:  usage.Int64(tokens.reasoning),
		TotalTokens:      usage.Int64(tokens.total()),
		Currency:         "USD",
		TokenAccuracy:    usage.AccuracyDerived,
		Source:           usage.Source{Adapter: adapterID, AdapterVersion: adapterVersion, Identity: identity},
	}
	if actualCost > 0 {
		event.Cost = usage.Float64(actualCost)
	} else if estimatedCost > 0 {
		event.Cost = usage.Float64(estimatedCost)
		event.CostEstimated = true
	}
	return event
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

func integerExpr(c map[string]bool, name string) string {
	if c[name] {
		return `COALESCE(` + name + `, 0)`
	}
	return `0`
}

func numberExpr(c map[string]bool, name string) string { return integerExpr(c, name) }

func qualifiedTextExpr(c map[string]bool, alias, name string) string {
	if c[name] {
		return `COALESCE(` + alias + `.` + name + `, '')`
	}
	return `''`
}

func qualifiedIntegerExpr(c map[string]bool, alias, name string) string {
	if c[name] {
		return `COALESCE(` + alias + `.` + name + `, 0)`
	}
	return `0`
}

func qualifiedNumberExpr(c map[string]bool, alias, name string) string {
	return qualifiedIntegerExpr(c, alias, name)
}

func safeCounts(c tokenCounts) tokenCounts {
	c.input = max64(0, c.input)
	c.output = max64(0, c.output)
	c.cacheRead = max64(0, c.cacheRead)
	c.cacheWrite = max64(0, c.cacheWrite)
	c.reasoning = max64(0, c.reasoning)
	return c
}

func unixFloat(value float64) time.Time {
	if value <= 0 {
		return time.Time{}
	}
	seconds := int64(value)
	nanoseconds := int64((value - float64(seconds)) * float64(time.Second))
	return time.Unix(seconds, nanoseconds).UTC()
}

func firstPositive(values ...float64) float64 {
	for _, value := range values {
		if value > 0 {
			return value
		}
	}
	return 0
}

func max64(left, right int64) int64 {
	if left > right {
		return left
	}
	return right
}

var _ adapters.Adapter = (*Adapter)(nil)
