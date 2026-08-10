package hermes

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tokemon/tokemon/internal/adapters"
	_ "modernc.org/sqlite"
)

func TestDiscoverDefaultAndNamedProfileDatabases(t *testing.T) {
	home := t.TempDir()
	paths := []string{
		filepath.Join(home, ".hermes", "state.db"),
		filepath.Join(home, ".hermes", "profiles", "coder", "state.db"),
		filepath.Join(home, ".hermes", "profiles", "writer", "state.db"),
	}
	for _, path := range paths {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, []byte("sqlite placeholder"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(home, ".hermes", "profiles", "not-a-profile"), []byte("ignored"), 0o600); err != nil {
		t.Fatal(err)
	}

	sources, err := New(home).Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 3 {
		t.Fatalf("sources = %d, want 3: %+v", len(sources), sources)
	}
	seenIdentities := make(map[string]bool)
	for _, source := range sources {
		if !strings.HasPrefix(source.Identity, "sha256:") || strings.Contains(source.Identity, "coder") {
			t.Fatalf("source identity leaks profile path: %+v", source)
		}
		if seenIdentities[source.Identity] {
			t.Fatalf("duplicate source identity: %q", source.Identity)
		}
		seenIdentities[source.Identity] = true
	}
}

func TestParseReconcilesModelUsageWithoutReadingTranscriptContent(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".hermes", "state.db")
	db := openHermesFixture(t, path, false)
	defer db.Close()

	_, err := db.Exec(`INSERT INTO sessions (
id, source, model, started_at, ended_at, input_tokens, output_tokens,
cache_read_tokens, cache_write_tokens, reasoning_tokens, cwd,
billing_provider, estimated_cost_usd, actual_cost_usd, title, system_prompt
) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"session-secret", "cli", "anthropic/claude-sonnet-5", 1785355200.25, 1785355260.5,
		100, 50, 10, 5, 20, "/Users/private/work/Tokemon", "nous", 2.0, 0.25,
		"private conversation title", "private system prompt")
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO messages (session_id, content, tool_calls) VALUES (?, ?, ?)`,
		"session-secret", "PROMPT_SECRET response secret", `{"command":"rm private"}`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO session_model_usage (
session_id, model, billing_provider, billing_base_url, billing_mode, task,
input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
reasoning_tokens, estimated_cost_usd, actual_cost_usd, last_seen
) VALUES
(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?),
(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"session-secret", "anthropic/claude-sonnet-5", "nous", "https://private.invalid/v1", "subscription", "", 60, 30, 5, 1, 10, 1.0, 0, 1785355250.25,
		"session-secret", "openai/gpt-5.6-terra", "openai", "https://private.invalid/v1", "api", "compression", 30, 15, 3, 2, 5, 0, 0.25, 1785355255.5)
	if err != nil {
		t.Fatal(err)
	}

	adapter := New(home)
	sources, err := adapter.Discover(context.Background())
	if err != nil || len(sources) != 1 {
		t.Fatalf("sources = %d, error: %v", len(sources), err)
	}
	result, err := adapter.Parse(context.Background(), sources[0], adapters.ParseRequest{MachineID: "machine"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Events) != 3 {
		t.Fatalf("events = %d, want two model rows plus residual: %+v", len(result.Events), result.Events)
	}

	var input, output, cacheRead, cacheWrite, reasoning, total int64
	var actualCost, estimatedCost float64
	for _, event := range result.Events {
		if err := event.Validate(); err != nil {
			t.Fatal(err)
		}
		if event.Project != "tokemon" || event.Tool != adapterID || event.SessionID != "session-secret" {
			t.Fatalf("unexpected normalized event: %+v", event)
		}
		input += *event.InputTokens
		output += *event.OutputTokens
		cacheRead += *event.CacheReadTokens
		cacheWrite += *event.CacheWriteTokens
		reasoning += *event.ReasoningTokens
		total += *event.TotalTokens
		if event.Cost != nil {
			if event.CostEstimated {
				estimatedCost += *event.Cost
			} else {
				actualCost += *event.Cost
			}
		}
	}
	if input != 100 || output != 50 || cacheRead != 10 || cacheWrite != 5 || reasoning != 20 || total != 165 {
		t.Fatalf("reconciled totals = in:%d out:%d read:%d write:%d reasoning:%d total:%d", input, output, cacheRead, cacheWrite, reasoning, total)
	}
	if estimatedCost != 1.0 || actualCost != 0.25 {
		t.Fatalf("costs = estimated %.2f actual %.2f", estimatedCost, actualCost)
	}

	payload, err := json.Marshal(result.Events)
	if err != nil {
		t.Fatal(err)
	}
	serialized := string(payload)
	for _, secret := range []string{"PROMPT_SECRET", "private conversation", "private system", "/Users/private", "private.invalid", "compression"} {
		if strings.Contains(serialized, secret) {
			t.Fatalf("events leaked %q: %s", secret, serialized)
		}
	}
	repeated, err := adapter.Parse(context.Background(), sources[0], adapters.ParseRequest{MachineID: "machine", Cursor: result.Cursor})
	if err != nil || len(repeated.Events) != 0 || repeated.Cursor != result.Cursor {
		t.Fatalf("unchanged Hermes snapshot was reparsed: %+v, error: %v", repeated, err)
	}
}

func TestParseActiveWALUpdateKeepsStableSnapshotID(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".hermes", "state.db")
	db := openHermesFixture(t, path, true)
	defer db.Close()
	_, err := db.Exec(`INSERT INTO sessions (
id, source, model, started_at, input_tokens, output_tokens, cache_read_tokens,
cache_write_tokens, reasoning_tokens, billing_provider
) VALUES ('active', 'cli', 'model', 1785355200, 10, 5, 0, 0, 0, 'provider')`)
	if err != nil {
		t.Fatal(err)
	}

	adapter := New(home)
	sources, err := adapter.Discover(context.Background())
	if err != nil || len(sources) != 1 {
		t.Fatalf("sources = %d, error: %v", len(sources), err)
	}
	first, err := adapter.Parse(context.Background(), sources[0], adapters.ParseRequest{MachineID: "machine"})
	if err != nil || len(first.Events) != 1 {
		t.Fatalf("first parse = %+v, error: %v", first, err)
	}
	firstID := first.Events[0].EventID
	if _, err := db.Exec(`UPDATE sessions SET input_tokens = 20, output_tokens = 8 WHERE id = 'active'`); err != nil {
		t.Fatal(err)
	}
	updated, err := adapter.Parse(context.Background(), sources[0], adapters.ParseRequest{MachineID: "machine", Cursor: first.Cursor})
	if err != nil || len(updated.Events) != 1 {
		t.Fatalf("updated parse = %+v, error: %v", updated, err)
	}
	if updated.Events[0].EventID != firstID || *updated.Events[0].TotalTokens != 28 {
		t.Fatalf("updated event is not a stable snapshot: %+v", updated.Events[0])
	}
}

func TestParseLegacyAggregateAndUnsupportedSchema(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".hermes", "state.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE sessions (
id TEXT PRIMARY KEY, started_at REAL NOT NULL, input_tokens INTEGER, output_tokens INTEGER
); INSERT INTO sessions VALUES ('legacy', 1785355200, 7, 3);`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	adapter := New(home)
	sources, _ := adapter.Discover(context.Background())
	result, err := adapter.Parse(context.Background(), sources[0], adapters.ParseRequest{MachineID: "machine"})
	if err != nil || len(result.Events) != 1 {
		t.Fatalf("legacy parse = %+v, error: %v", result, err)
	}
	if result.Events[0].Provider != "unknown" || result.Events[0].Model != "unknown" || *result.Events[0].TotalTokens != 10 {
		t.Fatalf("legacy event = %+v", result.Events[0])
	}

	unsupportedHome := t.TempDir()
	unsupportedPath := filepath.Join(unsupportedHome, ".hermes", "state.db")
	if err := os.MkdirAll(filepath.Dir(unsupportedPath), 0o755); err != nil {
		t.Fatal(err)
	}
	unsupported, err := sql.Open("sqlite", unsupportedPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := unsupported.Exec(`CREATE TABLE sessions (id TEXT PRIMARY KEY, title TEXT)`); err != nil {
		unsupported.Close()
		t.Fatal(err)
	}
	if err := unsupported.Close(); err != nil {
		t.Fatal(err)
	}
	unsupportedAdapter := New(unsupportedHome)
	unsupportedSources, _ := unsupportedAdapter.Discover(context.Background())
	empty, err := unsupportedAdapter.Parse(context.Background(), unsupportedSources[0], adapters.ParseRequest{MachineID: "machine"})
	if err != nil || len(empty.Events) != 0 {
		t.Fatalf("unsupported parse = %+v, error: %v", empty, err)
	}
	repeated, err := unsupportedAdapter.Parse(context.Background(), unsupportedSources[0], adapters.ParseRequest{MachineID: "machine", Cursor: empty.Cursor})
	if err != nil || len(repeated.Events) != 0 || repeated.Cursor != empty.Cursor {
		t.Fatalf("unsupported schema was not cached: %+v, error: %v", repeated, err)
	}
}

func TestParseSkipsMalformedRecordsAndKeepsReplacementIDsStable(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".hermes", "state.db")
	db := openHermesFixture(t, path, false)

	// Keep one valid snapshot beside records that are incomplete in ways seen
	// during interrupted writes: an empty identity and a missing timestamp.
	// Their string fields intentionally contain sensitive-looking values to
	// prove that skipped records cannot leak through a later event.
	_, err := db.Exec(`INSERT INTO sessions (
id, source, model, started_at, ended_at, input_tokens, output_tokens,
cache_read_tokens, cache_write_tokens, reasoning_tokens, cwd,
billing_provider, estimated_cost_usd, actual_cost_usd, title, system_prompt
) VALUES
('valid', 'cli', 'provider/model', 1785355200, 1785355260, 4, 6, 0, 0, 0,
 '/private/projects/keep-this-basename', 'provider', 0, 0, 'safe', 'safe'),
('', 'cli', 'private-model', 1785355200, 1785355260, 11, 13, 0, 0, 0,
 '/private/projects/should-not-leak', 'private-provider', 0, 0,
 'PRIVATE_TITLE', 'PRIVATE_PROMPT'),
('untimed', 'cli', 'private-model', 0, 0, 17, 19, 0, 0, 0,
 '/private/projects/should-not-leak', 'private-provider', 0, 0,
 'PRIVATE_TITLE_2', 'PRIVATE_PROMPT_2'),
('negative', 'cli', 'private-model', 1785355200, 1785355260, -7, -5, -3, -2, -1,
 '/private/projects/should-not-leak', 'private-provider', 0, 0,
 'PRIVATE_TITLE_3', 'PRIVATE_PROMPT_3')`)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO session_model_usage (
session_id, model, billing_provider, billing_base_url, billing_mode, task,
input_tokens, output_tokens, cache_read_tokens, cache_write_tokens,
reasoning_tokens, estimated_cost_usd, actual_cost_usd, last_seen
) VALUES
('untimed', 'PRIVATE_MODEL', 'PRIVATE_PROVIDER', 'https://private.invalid',
 'PRIVATE_MODE', 'PRIVATE_TASK', 0, 0, 0, 0, 0, 0, 0, 0),
('missing-session', 'PRIVATE_MODEL_2', 'PRIVATE_PROVIDER_2', 'https://private.invalid/2',
 'PRIVATE_MODE_2', 'PRIVATE_TASK_2', 12, 13, 0, 0, 0, 0, 0, 1785355200)`)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	adapter := New(home)
	sources, err := adapter.Discover(context.Background())
	if err != nil || len(sources) != 1 {
		t.Fatalf("sources = %d, error: %v", len(sources), err)
	}
	first, err := adapter.Parse(context.Background(), sources[0], adapters.ParseRequest{MachineID: "machine"})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Events) != 1 {
		t.Fatalf("malformed records were not skipped: %+v", first.Events)
	}
	if first.Events[0].SessionID != "valid" || first.Events[0].Project != "keep-this-basename" {
		t.Fatalf("unexpected valid event: %+v", first.Events[0])
	}
	payload, err := json.Marshal(first.Events)
	if err != nil {
		t.Fatal(err)
	}
	serialized := string(payload)
	for _, secret := range []string{"PRIVATE_TITLE", "PRIVATE_PROMPT", "private-model", "private-provider", "PRIVATE_MODEL", "PRIVATE_PROVIDER", "PRIVATE_TASK", "/private/projects"} {
		if strings.Contains(serialized, secret) {
			t.Fatalf("malformed fixture leaked %q: %s", secret, serialized)
		}
	}
	repeated, err := adapter.Parse(context.Background(), sources[0], adapters.ParseRequest{MachineID: "machine", Cursor: first.Cursor})
	if err != nil || len(repeated.Events) != 0 || repeated.Cursor != first.Cursor {
		t.Fatalf("unchanged malformed snapshot was not cached: %+v, error: %v", repeated, err)
	}

	// Repair the incomplete row, then update it once more. The repaired row is
	// emitted as a replacement snapshot, and its deterministic identity must
	// survive the token update while the original valid event remains stable.
	db = openExistingHermesFixture(t, path)
	if _, err := db.Exec(`UPDATE sessions SET started_at = 1785355300, input_tokens = 17, output_tokens = 19 WHERE id = 'untimed'`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := adapter.Parse(context.Background(), sources[0], adapters.ParseRequest{MachineID: "machine", Cursor: first.Cursor})
	if err != nil || len(second.Events) != 2 {
		t.Fatalf("repaired snapshot = %+v, error: %v", second, err)
	}
	ids := make(map[string]usageEventView)
	for _, event := range second.Events {
		ids[event.SessionID] = usageEventView{eventID: event.EventID, total: *event.TotalTokens}
	}
	if ids["valid"].eventID != first.Events[0].EventID || ids["untimed"].total != 36 {
		t.Fatalf("repaired events = %+v, first = %+v", ids, first.Events)
	}

	db = openExistingHermesFixture(t, path)
	if _, err := db.Exec(`UPDATE sessions SET input_tokens = 18 WHERE id = 'untimed'`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	third, err := adapter.Parse(context.Background(), sources[0], adapters.ParseRequest{MachineID: "machine", Cursor: second.Cursor})
	if err != nil || len(third.Events) != 2 {
		t.Fatalf("updated repaired snapshot = %+v, error: %v", third, err)
	}
	for _, event := range third.Events {
		if event.SessionID == "untimed" && event.EventID != ids["untimed"].eventID {
			t.Fatalf("replacement changed event ID: before=%q after=%q", ids["untimed"].eventID, event.EventID)
		}
	}
}

func TestParseTruncatedHermesDatabaseFailsClosed(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".hermes", "state.db")
	db := openHermesFixture(t, path, false)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		_ = os.Remove(path + suffix)
	}
	if err := os.Truncate(path, int64(len("SQLite format 3\x00"))); err != nil {
		t.Fatal(err)
	}

	adapter := New(home)
	sources, err := adapter.Discover(context.Background())
	if err != nil || len(sources) != 1 {
		t.Fatalf("sources = %d, error: %v", len(sources), err)
	}
	first, err := adapter.Parse(context.Background(), sources[0], adapters.ParseRequest{MachineID: "machine"})
	if err == nil || len(first.Events) != 0 {
		t.Fatalf("truncated database was not rejected: %+v, error: %v", first, err)
	}
	if strings.Contains(err.Error(), "PRIVATE_") {
		t.Fatalf("truncated database error leaked record content: %v", err)
	}
	repeated, repeatedErr := adapter.Parse(context.Background(), sources[0], adapters.ParseRequest{MachineID: "machine"})
	if repeatedErr == nil || len(repeated.Events) != 0 || repeatedErr.Error() != err.Error() {
		t.Fatalf("truncated error was not safely cached: %+v, error: %v", repeated, repeatedErr)
	}
}

func TestParseCorruptHermesDatabaseFailsClosed(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".hermes", "state.db")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("not a sqlite database PRIVATE_RECORD"), 0o600); err != nil {
		t.Fatal(err)
	}

	adapter := New(home)
	sources, err := adapter.Discover(context.Background())
	if err != nil || len(sources) != 1 {
		t.Fatalf("sources = %d, error: %v", len(sources), err)
	}
	result, err := adapter.Parse(context.Background(), sources[0], adapters.ParseRequest{MachineID: "machine"})
	if err == nil || len(result.Events) != 0 {
		t.Fatalf("corrupt database was not rejected: %+v, error: %v", result, err)
	}
	if strings.Contains(err.Error(), "PRIVATE_RECORD") {
		t.Fatalf("corrupt database error leaked record content: %v", err)
	}
}

type usageEventView struct {
	eventID string
	total   int64
}

func openExistingHermesFixture(t *testing.T, path string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	return db
}

func openHermesFixture(t *testing.T, path string, wal bool) *sql.DB {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if wal {
		if _, err := db.Exec(`PRAGMA journal_mode=WAL`); err != nil {
			db.Close()
			t.Fatal(err)
		}
	}
	_, err = db.Exec(`CREATE TABLE sessions (
id TEXT PRIMARY KEY,
source TEXT NOT NULL,
model TEXT,
started_at REAL NOT NULL,
ended_at REAL,
input_tokens INTEGER DEFAULT 0,
output_tokens INTEGER DEFAULT 0,
cache_read_tokens INTEGER DEFAULT 0,
cache_write_tokens INTEGER DEFAULT 0,
reasoning_tokens INTEGER DEFAULT 0,
cwd TEXT,
billing_provider TEXT,
estimated_cost_usd REAL,
actual_cost_usd REAL,
title TEXT,
system_prompt TEXT
);
CREATE TABLE messages (
id INTEGER PRIMARY KEY AUTOINCREMENT,
session_id TEXT NOT NULL,
content TEXT,
tool_calls TEXT
);
CREATE TABLE session_model_usage (
session_id TEXT NOT NULL,
model TEXT NOT NULL,
billing_provider TEXT NOT NULL DEFAULT '',
billing_base_url TEXT NOT NULL DEFAULT '',
billing_mode TEXT NOT NULL DEFAULT '',
task TEXT NOT NULL DEFAULT '',
input_tokens INTEGER NOT NULL DEFAULT 0,
output_tokens INTEGER NOT NULL DEFAULT 0,
cache_read_tokens INTEGER NOT NULL DEFAULT 0,
cache_write_tokens INTEGER NOT NULL DEFAULT 0,
reasoning_tokens INTEGER NOT NULL DEFAULT 0,
estimated_cost_usd REAL NOT NULL DEFAULT 0,
actual_cost_usd REAL NOT NULL DEFAULT 0,
last_seen REAL,
PRIMARY KEY (session_id, model, billing_provider, billing_base_url, billing_mode, task)
)`)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	return db
}
