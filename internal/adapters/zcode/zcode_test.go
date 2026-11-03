package zcode

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tokemon/tokemon/internal/adapters"
	"github.com/tokemon/tokemon/internal/usage"
	_ "modernc.org/sqlite"
)

func newFixture(t *testing.T) (string, string) {
	t.Helper()
	home := t.TempDir()
	root := filepath.Join(home, ".zcode", "cli", "db")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	return home, filepath.Join(root, "db.sqlite")
}

func createSchema(t *testing.T, path string, sessionTable bool) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE model_usage (
id TEXT PRIMARY KEY,
session_id TEXT NOT NULL,
turn_id TEXT NOT NULL DEFAULT '',
query_source TEXT NOT NULL DEFAULT 'main_turn',
provider_id TEXT NOT NULL DEFAULT '',
model_id TEXT NOT NULL DEFAULT '',
status TEXT NOT NULL DEFAULT 'completed',
started_at INTEGER NOT NULL,
completed_at INTEGER,
duration_ms INTEGER NOT NULL DEFAULT 0,
input_tokens INTEGER NOT NULL DEFAULT 0,
output_tokens INTEGER NOT NULL DEFAULT 0,
reasoning_tokens INTEGER NOT NULL DEFAULT 0,
cache_creation_input_tokens INTEGER NOT NULL DEFAULT 0,
cache_read_input_tokens INTEGER NOT NULL DEFAULT 0
)`)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	if sessionTable {
		if _, err := db.Exec(`CREATE TABLE session (id TEXT PRIMARY KEY, directory TEXT NOT NULL DEFAULT '')`); err != nil {
			db.Close()
			t.Fatal(err)
		}
	}
	return db
}

func insertUsage(t *testing.T, db *sql.DB, rows ...[]any) {
	t.Helper()
	for _, row := range rows {
		_, err := db.Exec(`INSERT INTO model_usage (id, session_id, provider_id, model_id, status, started_at, completed_at, duration_ms, input_tokens, output_tokens, reasoning_tokens, cache_creation_input_tokens, cache_read_input_tokens)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, row...)
		if err != nil {
			db.Close()
			t.Fatal(err)
		}
	}
}

// TestParseAggregatesPerSessionAndModel covers desktop-style session IDs,
// terminal CLI session IDs, subagent sessions, per-model splitting, the
// cache-aware input decomposition, and zero-token row skipping.
func TestParseAggregatesPerSessionAndModel(t *testing.T) {
	home, path := newFixture(t)
	db := createSchema(t, path, true)
	insertUsage(t, db,
		// Desktop session, two requests with the same model: one aggregate event.
		[]any{"usage_model_1", "session_1473bfdf8bfcf3bd711f024d2411809b", "account:test-plan", "GLM-5.3-Flash", "completed", int64(1793723498060), int64(1793723507404), int64(9344), int64(83488), int64(370), int64(0), int64(0), int64(82816)},
		[]any{"usage_model_2", "session_1473bfdf8bfcf3bd711f024d2411809b", "account:test-plan", "GLM-5.3-Flash", "completed", int64(1793723500000), int64(1793723501000), int64(500), int64(5000), int64(100), int64(0), int64(0), int64(4000)},
		// Same session, different model: its own event.
		[]any{"usage_model_3", "session_1473bfdf8bfcf3bd711f024d2411809b", "account:test-plan", "GLM-4.7", "completed", int64(1793723502000), int64(1793723502500), int64(500), int64(100), int64(10), int64(0), int64(0), int64(0)},
		// Terminal CLI session with cache creation and reasoning components.
		[]any{"usage_model_4", "sess_3b067961-afe7-43dd-9f71-3834c63c26e4", "account:test-plan", "GLM-5.3-Flash", "completed", int64(1793723510000), int64(1793723510500), int64(500), int64(1200), int64(50), int64(10), int64(200), int64(800)},
		// Subagent session with its own session row.
		[]any{"usage_model_5", "sess_subagent_agent_8b551d2c-1241-4d3a-ac14-f60f0c852673", "account:test-plan", "GLM-5.3-Flash", "completed", int64(1793723520000), int64(1793723521000), int64(1000), int64(200), int64(20), int64(0), int64(0), int64(100)},
		// Failed and cancelled requests carry no tokens and are skipped.
		[]any{"usage_model_6", "session_1473bfdf8bfcf3bd711f024d2411809b", "account:test-plan", "GLM-5.3-Flash", "error", int64(1793723490000), nil, int64(631), int64(0), int64(0), int64(0), int64(0), int64(0)},
		// A session with usage but no session row keeps its event with no project.
		[]any{"usage_model_7", "sess_orphan_session", "account:test-plan", "GLM-5.3-Flash", "completed", int64(1793723530000), int64(1793723530100), int64(100), int64(50), int64(5), int64(0), int64(0), int64(0)},
	)
	if _, err := db.Exec(`INSERT INTO session (id, directory) VALUES
('session_1473bfdf8bfcf3bd711f024d2411809b', '/home/user/repos/crt-cabinet/'),
('sess_3b067961-afe7-43dd-9f71-3834c63c26e4', '/home/user/repos/l3ms'),
('sess_subagent_agent_8b551d2c-1241-4d3a-ac14-f60f0c852673', '/home/user/repos/l3ms')`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	adapter := New(home)
	sources, err := adapter.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 1 || sources[0].Identity == "" {
		t.Fatalf("sources = %+v, want one identified source", sources)
	}
	result, err := adapter.Parse(context.Background(), sources[0], adapters.ParseRequest{MachineID: "machine"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Events) != 5 {
		t.Fatalf("events = %d (%+v), want 5", len(result.Events), result.Events)
	}
	if result.Cursor.Identity == "" {
		t.Fatal("cursor identity is empty")
	}

	eventsByKey := make(map[string]usage.Event, len(result.Events))
	for _, event := range result.Events {
		key := event.SessionID + "\x00" + event.Model
		if _, exists := eventsByKey[key]; exists {
			t.Fatalf("duplicate event for %q: %+v", key, event)
		}
		eventsByKey[key] = event
	}

	desktop := eventsByKey["session_1473bfdf8bfcf3bd711f024d2411809b\x00GLM-5.3-Flash"]
	if desktop.InputTokens == nil || *desktop.InputTokens != 1672 {
		t.Fatalf("desktop input = %+v, want 1672 (normalized input minus cache reads)", desktop)
	}
	if desktop.OutputTokens == nil || *desktop.OutputTokens != 470 {
		t.Fatalf("desktop output = %+v, want 470", desktop)
	}
	if desktop.CacheReadTokens == nil || *desktop.CacheReadTokens != 86816 {
		t.Fatalf("desktop cache read = %+v, want 86816", desktop)
	}
	if desktop.TotalTokens == nil || *desktop.TotalTokens != 88958 {
		t.Fatalf("desktop total = %+v, want 88958", desktop)
	}
	if desktop.DurationMS == nil || *desktop.DurationMS != 9844 {
		t.Fatalf("desktop duration = %+v, want 9844", desktop)
	}
	if desktop.Provider != "account:test-plan" || desktop.Project != "crt-cabinet" {
		t.Fatalf("desktop provider/project = %q/%q, want plan provider and privacy-safe basename", desktop.Provider, desktop.Project)
	}
	if want := time.UnixMilli(1793723507404).UTC(); !desktop.Timestamp.Equal(want) {
		t.Fatalf("desktop timestamp = %v, want %v", desktop.Timestamp, want)
	}
	identity := adapters.HashIdentity("zcode:db.sqlite")
	if want := usage.DeterministicID("machine", adapterID, identity, 0, "GLM-5.3-Flash\x00account:test-plan", "session_1473bfdf8bfcf3bd711f024d2411809b"); desktop.EventID != want {
		t.Fatalf("desktop event ID = %q, want %q", desktop.EventID, want)
	}

	secondModel := eventsByKey["session_1473bfdf8bfcf3bd711f024d2411809b\x00GLM-4.7"]
	if secondModel.TotalTokens == nil || *secondModel.TotalTokens != 110 {
		t.Fatalf("second model total = %+v, want 110", secondModel)
	}
	if secondModel.EventID == desktop.EventID {
		t.Fatal("different models share an event ID")
	}

	cli := eventsByKey["sess_3b067961-afe7-43dd-9f71-3834c63c26e4\x00GLM-5.3-Flash"]
	if cli.InputTokens == nil || *cli.InputTokens != 200 || cli.CacheWriteTokens == nil || *cli.CacheWriteTokens != 200 {
		t.Fatalf("CLI input/cache write = %+v, want non-cached 200 and cache write 200", cli)
	}
	if cli.ReasoningTokens == nil || *cli.ReasoningTokens != 10 || cli.TotalTokens == nil || *cli.TotalTokens != 1250 {
		t.Fatalf("CLI reasoning/total = %+v, want reasoning 10 and total 1250", cli)
	}

	subagent := eventsByKey["sess_subagent_agent_8b551d2c-1241-4d3a-ac14-f60f0c852673\x00GLM-5.3-Flash"]
	if subagent.Project != "l3ms" {
		t.Fatalf("subagent project = %q, want l3ms", subagent.Project)
	}
	orphan := eventsByKey["sess_orphan_session\x00GLM-5.3-Flash"]
	if orphan.Project != "" {
		t.Fatalf("orphan project = %q, want empty", orphan.Project)
	}

	for i, event := range result.Events {
		if err := event.Validate(); err != nil {
			t.Fatalf("event %d failed validation: %v", i, err)
		}
	}
	if err := usage.ValidateOutboundBatch(result.Events); err != nil {
		t.Fatalf("batch failed outbound validation: %v", err)
	}

	reparsed, err := New(home).Parse(context.Background(), sources[0], adapters.ParseRequest{MachineID: "machine"})
	if err != nil || len(reparsed.Events) != len(result.Events) {
		t.Fatalf("fresh parse = %d events, error: %v", len(reparsed.Events), err)
	}
	for _, event := range reparsed.Events {
		key := event.SessionID + "\x00" + event.Model
		if previous, exists := eventsByKey[key]; !exists || previous.EventID != event.EventID {
			t.Fatalf("event identity changed across parses: %q first=%+v second=%+v", key, previous, event)
		}
	}
	repeated, err := adapter.Parse(context.Background(), sources[0], adapters.ParseRequest{MachineID: "machine", Cursor: result.Cursor})
	if err != nil || len(repeated.Events) != 0 || repeated.Cursor != result.Cursor {
		t.Fatalf("unchanged database was reparsed: %+v, error: %v", repeated, err)
	}
}

// TestParseHashesUnsafeSessionIDs keeps the privacy guarantee: a session ID
// that carries a path-like value never reaches an event as-is.
func TestParseHashesUnsafeSessionIDs(t *testing.T) {
	home, path := newFixture(t)
	db := createSchema(t, path, true)
	unsafe := "/private/repo/private-session-title"
	insertUsage(t, db,
		[]any{"usage_model_1", unsafe, "account:test-plan", "GLM-5.3-Flash", "completed", int64(1793723498060), int64(1793723507404), int64(9344), int64(100), int64(10), int64(0), int64(0), int64(0)},
	)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	adapter := New(home)
	sources, err := adapter.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	result, err := adapter.Parse(context.Background(), sources[0], adapters.ParseRequest{MachineID: "machine"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Events) != 1 {
		t.Fatalf("events = %d, want 1", len(result.Events))
	}
	event := result.Events[0]
	if !strings.HasPrefix(event.SessionID, "sha256:") || strings.Contains(event.SessionID, unsafe) {
		t.Fatalf("unsafe session ID was not made opaque: %+v", event)
	}
	serialized, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(serialized), unsafe) || strings.Contains(string(serialized), "/private/") {
		t.Fatalf("unsafe session value leaked into event: %s", serialized)
	}
	if err := event.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := usage.ValidateOutbound(event); err != nil {
		t.Fatalf("normalized event failed outbound validation: %v", err)
	}
}

// TestParseSkipsDatabasesWithoutUsageSchema covers a missing table and a
// table without the required columns: both degrade to an empty snapshot.
func TestParseSkipsDatabasesWithoutUsageSchema(t *testing.T) {
	home, path := newFixture(t)
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE session (id TEXT PRIMARY KEY, directory TEXT)`); err != nil {
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
	result, err := adapter.Parse(context.Background(), sources[0], adapters.ParseRequest{MachineID: "machine"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Events) != 0 || result.Cursor.Identity == "" {
		t.Fatalf("database without model_usage produced %+v", result)
	}

	db, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE model_usage (id TEXT PRIMARY KEY, session_id TEXT NOT NULL)`); err != nil {
		t.Fatal(err)
	}
	adapter = New(home)
	sources, err = adapter.Discover(context.Background())
	if err != nil || len(sources) != 1 {
		t.Fatalf("sources = %d, error: %v", len(sources), err)
	}
	result, err = adapter.Parse(context.Background(), sources[0], adapters.ParseRequest{MachineID: "machine"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Events) != 0 {
		t.Fatalf("schema without required columns produced %d events", len(result.Events))
	}
}

// TestDiscoverSkipsMissingDatabase keeps discovery silent when ZCode is not
// installed instead of failing the poll.
func TestDiscoverSkipsMissingDatabase(t *testing.T) {
	adapter := New(t.TempDir())
	sources, err := adapter.Discover(context.Background())
	if err != nil || len(sources) != 0 {
		t.Fatalf("sources = %+v, error: %v", sources, err)
	}
}

func TestParseRequiresMachineID(t *testing.T) {
	home, path := newFixture(t)
	db := createSchema(t, path, true)
	insertUsage(t, db,
		[]any{"usage_model_1", "session_1473bfdf8bfcf3bd711f024d2411809b", "account:test-plan", "GLM-5.3-Flash", "completed", int64(1793723498060), int64(1793723507404), int64(9344), int64(100), int64(10), int64(0), int64(0), int64(0)},
	)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	adapter := New(home)
	sources, err := adapter.Discover(context.Background())
	if err != nil || len(sources) != 1 {
		t.Fatalf("sources = %d, error: %v", len(sources), err)
	}
	if _, err := adapter.Parse(context.Background(), sources[0], adapters.ParseRequest{MachineID: " "}); err == nil {
		t.Fatal("blank machine ID was accepted")
	}
}

func TestNormalizeModel(t *testing.T) {
	adapter := New(t.TempDir())
	if got := adapter.NormalizeModel(" GLM-5.3-Flash "); got != "GLM-5.3-Flash" {
		t.Fatalf("NormalizeModel = %q", got)
	}
	if got := adapter.NormalizeModel("  "); got != "unknown" {
		t.Fatalf("NormalizeModel = %q, want unknown", got)
	}
}
