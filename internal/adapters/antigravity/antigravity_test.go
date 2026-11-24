package antigravity

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tokemon/tokemon/internal/adapters"
	"github.com/tokemon/tokemon/internal/usage"
	_ "modernc.org/sqlite"
)

func TestDiscoverDoesNotOpenSQLite(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".gemini", "antigravity-cli", "conversations")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(root, "first.db")
	second := filepath.Join(root, "second.db")
	if err := os.WriteFile(first, []byte("first"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("second"), 0o600); err != nil {
		t.Fatal(err)
	}

	a := New(home)
	readDirs := 0
	readDir := a.readDir
	a.readDir = func(path string) ([]os.DirEntry, error) {
		readDirs++
		return readDir(path)
	}
	a.open = func(context.Context, string) (*sql.DB, error) {
		t.Fatal("discovery opened SQLite")
		return nil, nil
	}
	if sources, err := a.Discover(context.Background()); err != nil || len(sources) != 2 {
		t.Fatalf("first discovery = %d sources, error: %v", len(sources), err)
	}
	for i := 0; i < 100; i++ {
		if sources, err := a.Discover(context.Background()); err != nil || len(sources) != 2 {
			t.Fatalf("cached discovery %d = %d sources, error: %v", i, len(sources), err)
		}
	}
	if readDirs != 1 {
		t.Fatalf("cached discovery directory reads = %d, want 1", readDirs)
	}
	if err := os.WriteFile(second, []byte("second file changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}
	third := filepath.Join(root, "third.db")
	if err := os.WriteFile(third, []byte("third"), 0o600); err != nil {
		t.Fatal(err)
	}
	sources, err := a.Discover(context.Background())
	if err != nil || len(sources) != 3 {
		t.Fatalf("new file discovery = %d sources, error: %v", len(sources), err)
	}
	if readDirs != 2 {
		t.Fatalf("new file directory reads = %d, want 2", readDirs)
	}
}

func TestParseCachesUnsupportedDatabase(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".gemini", "antigravity-cli", "conversations")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "other.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE unrelated (id INTEGER PRIMARY KEY)`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	a := New(home)
	sources, err := a.Discover(context.Background())
	if err != nil || len(sources) != 1 {
		t.Fatalf("sources = %d, error: %v", len(sources), err)
	}
	first, err := a.Parse(context.Background(), sources[0], adapters.ParseRequest{MachineID: "machine"})
	if err != nil || len(first.Events) != 0 {
		t.Fatalf("first unsupported parse = %+v, error: %v", first, err)
	}
	a.open = func(context.Context, string) (*sql.DB, error) {
		return nil, fmt.Errorf("unsupported source was reopened")
	}
	for i := 0; i < 100; i++ {
		result, err := a.Parse(context.Background(), sources[0], adapters.ParseRequest{MachineID: "machine", Cursor: first.Cursor})
		if err != nil || len(result.Events) != 0 || result.Cursor != first.Cursor {
			t.Fatalf("cached unsupported parse %d = %+v, error: %v", i, result, err)
		}
	}
}

func TestParseCachesMalformedDatabaseError(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".gemini", "antigravity-cli", "conversations")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "malformed.db")
	if err := os.WriteFile(path, []byte("not sqlite"), 0o600); err != nil {
		t.Fatal(err)
	}

	a := New(home)
	sources, err := a.Discover(context.Background())
	if err != nil || len(sources) != 1 {
		t.Fatalf("sources = %d, error: %v", len(sources), err)
	}
	opens := 0
	a.open = func(ctx context.Context, path string) (*sql.DB, error) {
		opens++
		return adapters.OpenReadOnly(ctx, path)
	}
	_, firstErr := a.Parse(context.Background(), sources[0], adapters.ParseRequest{MachineID: "machine"})
	if firstErr == nil {
		t.Fatal("malformed parse unexpectedly succeeded")
	}
	for i := 0; i < 100; i++ {
		_, err := a.Parse(context.Background(), sources[0], adapters.ParseRequest{MachineID: "machine"})
		if err == nil || err.Error() != firstErr.Error() {
			t.Fatalf("cached malformed parse %d error = %v, want %v", i, err, firstErr)
		}
	}
	if opens != 1 {
		t.Fatalf("malformed database opens = %d, want 1", opens)
	}
}

func TestParseGenerationMetadataWithoutReadingConversationContent(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".gemini", "antigravity-cli", "conversations")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "session-private-title.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE gen_metadata (idx INTEGER PRIMARY KEY, data BLOB, size INTEGER NOT NULL DEFAULT 0);
CREATE TABLE trajectory_metadata_blob (id TEXT PRIMARY KEY, data BLOB);
CREATE TABLE steps (idx INTEGER PRIMARY KEY, step_payload BLOB);
INSERT INTO gen_metadata (idx, data, size) VALUES (?, ?, ?);
INSERT INTO steps (idx, step_payload) VALUES (0, 'secret prompt and response');`, 7, fixtureMetadata(), len(fixtureMetadata())); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO trajectory_metadata_blob (id, data) VALUES (?, ?)`, "main", fixtureTrajectoryMetadata()); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	a := New(home)
	var opened []*sql.DB
	a.open = func(ctx context.Context, path string) (*sql.DB, error) {
		db, err := adapters.OpenReadOnly(ctx, path)
		if db != nil {
			opened = append(opened, db)
		}
		return db, err
	}
	sources, err := a.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 1 {
		t.Fatalf("sources = %d, want 1", len(sources))
	}
	result, err := a.Parse(context.Background(), sources[0], adapters.ParseRequest{MachineID: "machine"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Events) != 1 {
		t.Fatalf("events = %d, want 1", len(result.Events))
	}
	event := result.Events[0]
	if event.InputTokens == nil || *event.InputTokens != 1200 || event.OutputTokens == nil || *event.OutputTokens != 345 {
		t.Fatalf("unexpected token fields: %+v", event)
	}
	if event.TotalTokens == nil || *event.TotalTokens != 1545 || event.Model != "gemini-3.5-pro" || event.Provider != "google" {
		t.Fatalf("unexpected normalized event: %+v", event)
	}
	if event.Timestamp != time.Date(2026, 7, 12, 21, 30, 0, 123, time.UTC) || event.TokenAccuracy != usage.AccuracyReported {
		t.Fatalf("unexpected timestamp or accuracy: %+v", event)
	}
	if event.Project != "carteakey.dev" || event.Metadata != nil || event.Source.Identity == sources[0].Path || !strings.HasPrefix(event.SessionID, "sha256:") || strings.Contains(event.SessionID, "session-private-title") {
		t.Fatalf("privacy boundary failed: %+v", event)
	}
	if err := event.Validate(); err != nil {
		t.Fatal(err)
	}
	legacyCursor := adapters.Cursor{Identity: adapters.HashIdentity(sources[0].Path), Offset: result.Cursor.Offset}
	backfill, err := a.Parse(context.Background(), sources[0], adapters.ParseRequest{MachineID: "machine", Cursor: legacyCursor})
	if err != nil || len(backfill.Events) != 1 {
		t.Fatalf("legacy cursor backfill = %+v, error: %v", backfill, err)
	}
	if backfill.Events[0].EventID != event.EventID || backfill.Events[0].Project != event.Project {
		t.Fatalf("legacy cursor changed event identity or project: first=%+v backfill=%+v", event, backfill.Events[0])
	}
	if len(opened) != 2 {
		t.Fatalf("opened SQLite handles = %d, want 2", len(opened))
	}
	for _, db := range opened {
		stats := db.Stats()
		if stats.InUse != 0 || stats.OpenConnections != 0 || stats.Idle != 0 {
			t.Fatalf("SQLite handles leaked: %+v", stats)
		}
	}
}

func TestDecodeGenerationMetadataRejectsMalformedData(t *testing.T) {
	if _, ok := decodeGenerationMetadata([]byte("secret transcript")); ok {
		t.Fatal("malformed protobuf was accepted")
	}
}

func TestDecodeTrajectoryProjectNormalizesFileURI(t *testing.T) {
	if project := decodeTrajectoryProject(fixtureTrajectoryMetadata()); project != "carteakey.dev" {
		t.Fatalf("project = %q, want carteakey.dev", project)
	}
}

func TestParseSkipsUnchangedDatabase(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".gemini", "antigravity-cli", "conversations")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "unchanged.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE gen_metadata (idx INTEGER PRIMARY KEY, data BLOB, size INTEGER NOT NULL DEFAULT 0); INSERT INTO gen_metadata (idx, data, size) VALUES (?, ?, ?)`, 7, fixtureMetadata(), len(fixtureMetadata())); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	a := New(home)
	sources, err := a.Discover(context.Background())
	if err != nil || len(sources) != 1 {
		t.Fatalf("sources = %d, error: %v", len(sources), err)
	}
	first, err := a.Parse(context.Background(), sources[0], adapters.ParseRequest{MachineID: "machine"})
	if err != nil || len(first.Events) != 1 {
		t.Fatalf("first parse = %d events, error: %v", len(first.Events), err)
	}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := a.Parse(canceled, sources[0], adapters.ParseRequest{MachineID: "machine"}); err == nil {
		t.Fatal("canceled parse unexpectedly succeeded")
	}
	retry, err := a.Parse(context.Background(), sources[0], adapters.ParseRequest{MachineID: "machine"})
	if err != nil || len(retry.Events) != 1 {
		t.Fatalf("retry after canceled parse = %d events, error: %v", len(retry.Events), err)
	}
	opens := 0
	a.open = func(context.Context, string) (*sql.DB, error) {
		opens++
		return nil, fmt.Errorf("source was reopened")
	}
	for i := 0; i < 100; i++ {
		second, err := a.Parse(context.Background(), sources[0], adapters.ParseRequest{MachineID: "machine", Cursor: first.Cursor})
		if err != nil {
			t.Fatal(err)
		}
		if len(second.Events) != 0 || second.Cursor != first.Cursor {
			t.Fatalf("cached parse %d = %+v, want unchanged cursor and no events", i, second)
		}
	}
	if opens != 0 {
		t.Fatalf("unchanged source opens = %d, want 0", opens)
	}
	if err := os.WriteFile(path+"-wal", []byte("changed"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := a.Parse(context.Background(), sources[0], adapters.ParseRequest{MachineID: "machine", Cursor: first.Cursor}); err == nil {
		t.Fatal("changed source was not reopened")
	}
	if opens != 1 {
		t.Fatalf("changed source opens = %d, want 1", opens)
	}
}

func fixtureMetadata() []byte {
	stats := message(varintField(1, 345), varintField(2, 1200))
	timestamp := message(varintField(1, uint64(time.Date(2026, 7, 12, 21, 30, 0, 123, time.UTC).Unix())), varintField(2, 123))
	generation := message(bytesField(4, stats), bytesField(9, bytesField(4, timestamp)), bytesField(19, []byte("gemini-3.5-pro")))
	return message(bytesField(1, generation))
}

func fixtureTrajectoryMetadata() []byte {
	return message(bytesField(1, bytesField(1, []byte("file:///Users/alice/repos/Carteakey.dev"))))
}

func message(fields ...[]byte) []byte {
	var result []byte
	for _, field := range fields {
		result = append(result, field...)
	}
	return result
}

func bytesField(field uint64, value []byte) []byte {
	result := encodeVarint(field<<3 | 2)
	result = append(result, encodeVarint(uint64(len(value)))...)
	return append(result, value...)
}

func varintField(field, value uint64) []byte {
	return append(encodeVarint(field<<3), encodeVarint(value)...)
}

func encodeVarint(value uint64) []byte {
	var result []byte
	for value >= 0x80 {
		result = append(result, byte(value)|0x80)
		value >>= 7
	}
	return append(result, byte(value))
}

func TestParseGenerationStepsAfterMetadataMigration(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".gemini", "antigravity-cli", "conversations")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "migrated.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// New-format generation records no longer carry a timestamp; the counters
	// and timestamps live in steps.metadata. The transcript stays in a separate
	// field and must never be read.
	generation := message(bytesField(3, message(bytesField(28, []byte("gemini-3.8-flash")))))
	stepOne := message(
		bytesField(1, message(varintField(1, 1793760236), varintField(2, 0))),
		bytesField(9, message(varintField(1, 8192), varintField(2, 1200), varintField(3, 345))),
	)
	stepTwo := message(
		bytesField(1, message(varintField(1, 1793761390), varintField(2, 0))),
		bytesField(9, message(varintField(1, 8192), varintField(2, 1300), varintField(3, 100))),
	)
	if _, err := db.Exec(`CREATE TABLE gen_metadata (idx INTEGER PRIMARY KEY, data BLOB, size INTEGER NOT NULL DEFAULT 0);
CREATE TABLE steps (idx INTEGER PRIMARY KEY, step_type INTEGER NOT NULL DEFAULT 0, metadata BLOB);
CREATE TABLE trajectory_metadata_blob (id TEXT PRIMARY KEY, data BLOB);`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO gen_metadata (idx, data, size) VALUES (0, ?, 0), (1, ?, 0)`, generation, generation); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO steps (idx, step_type, metadata) VALUES (1, 15, ?), (2, 15, ?), (3, 132, 'tool')`, stepOne, stepTwo); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO trajectory_metadata_blob (id, data) VALUES ('main', ?)`, fixtureTrajectoryMetadata()); err != nil {
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
	if len(result.Events) != 2 {
		t.Fatalf("events = %d, want 2", len(result.Events))
	}
	first := result.Events[0]
	if first.InputTokens == nil || *first.InputTokens != 1200 || first.OutputTokens == nil || *first.OutputTokens != 345 || first.TotalTokens == nil || *first.TotalTokens != 1545 {
		t.Fatalf("unexpected step tokens: %+v", first)
	}
	if first.Model != "gemini-3.8-flash" || first.Provider != "google" || first.Project != "carteakey.dev" {
		t.Fatalf("unexpected step model or project: %+v", first)
	}
	if first.Timestamp != time.Unix(1793760236, 0).UTC() || first.Tool != "antigravity" {
		t.Fatalf("unexpected step timestamp or tool: %+v", first)
	}
	if err := usage.ValidateOutboundBatch(result.Events); err != nil {
		t.Fatalf("step batch failed outbound validation: %v", err)
	}
	repeated, err := adapter.Parse(context.Background(), sources[0], adapters.ParseRequest{MachineID: "machine", Cursor: result.Cursor})
	if err != nil || len(repeated.Events) != 0 || repeated.Cursor != result.Cursor {
		t.Fatalf("unchanged step source was reparsed: %+v, error: %v", repeated, err)
	}
}

func TestParseGenerationStepsCacheReadReasoningAndIncrementalCursor(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".gemini", "antigravity-cli", "conversations")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "cached-steps.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	generation := message(bytesField(3, message(bytesField(28, []byte("gemini-4-hydrogen-tiered-high")))))
	// Cold turn: field 5 omitted -> CacheReadTokens = 0.
	stepCold := message(
		bytesField(1, message(varintField(1, 1793760100), varintField(2, 0))),
		bytesField(9, message(varintField(1, 1535), varintField(2, 12000), varintField(3, 500), varintField(9, 200), varintField(10, 300))),
	)
	// Warm turn: field 5 = 346516 cache read, field 2 = 3487 uncached input,
	// field 3 = 332 total output (field 9 = 289 thinking + field 10 = 43 text).
	stepWarm := message(
		bytesField(1, message(varintField(1, 1793760200), varintField(2, 0))),
		bytesField(9, message(varintField(1, 1535), varintField(2, 3487), varintField(3, 332), varintField(5, 346516), varintField(9, 289), varintField(10, 43))),
	)
	if _, err := db.Exec(`CREATE TABLE gen_metadata (idx INTEGER PRIMARY KEY, data BLOB, size INTEGER NOT NULL DEFAULT 0);
CREATE TABLE steps (idx INTEGER PRIMARY KEY, step_type INTEGER NOT NULL DEFAULT 0, metadata BLOB);
CREATE TABLE trajectory_metadata_blob (id TEXT PRIMARY KEY, data BLOB);`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO gen_metadata (idx, data, size) VALUES (0, ?, 0)`, generation); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if _, err := db.Exec(`INSERT INTO steps (idx, step_type, metadata) VALUES (1, 15, ?), (2, 15, ?)`, stepCold, stepWarm); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	adapter := New(home)
	caps := adapter.Capabilities()
	if !caps.CacheTokens || !caps.ReasoningTokens {
		t.Fatalf("capabilities = %+v, want CacheTokens and ReasoningTokens enabled", caps)
	}
	sources, err := adapter.Discover(context.Background())
	if err != nil || len(sources) != 1 {
		t.Fatalf("sources = %d, error: %v", len(sources), err)
	}
	first, err := adapter.Parse(context.Background(), sources[0], adapters.ParseRequest{MachineID: "machine"})
	if err != nil || len(first.Events) != 2 {
		t.Fatalf("first parse = %d events, error: %v", len(first.Events), err)
	}
	cold := first.Events[0]
	if cold.CacheReadTokens == nil || *cold.CacheReadTokens != 0 || cold.ReasoningTokens == nil || *cold.ReasoningTokens != 200 || cold.TotalTokens == nil || *cold.TotalTokens != 12500 {
		t.Fatalf("cold step tokens = %+v", cold)
	}
	warm := first.Events[1]
	if warm.InputTokens == nil || *warm.InputTokens != 3487 || warm.CacheReadTokens == nil || *warm.CacheReadTokens != 346516 || warm.OutputTokens == nil || *warm.OutputTokens != 332 || warm.ReasoningTokens == nil || *warm.ReasoningTokens != 289 || warm.TotalTokens == nil || *warm.TotalTokens != 350335 {
		t.Fatalf("warm step tokens = %+v", warm)
	}
	if err := usage.ValidateOutboundBatch(first.Events); err != nil {
		t.Fatalf("step batch failed validation: %v", err)
	}

	// Upgrading from an older v0.2.0 cursor re-reads from offset 0 with the
	// exact same deterministic EventIDs so Store.Ingest updates rows in place.
	oldCursor := adapters.Cursor{
		Identity: adapters.HashIdentity(adapterID + "\x00" + "0.2.0" + "\x00" + sources[0].Path),
		Offset:   2,
	}
	upgraded, err := adapter.Parse(context.Background(), sources[0], adapters.ParseRequest{MachineID: "machine", Cursor: oldCursor})
	if err != nil || len(upgraded.Events) != 2 {
		t.Fatalf("v0.2.0 upgrade parse = %d events, error: %v", len(upgraded.Events), err)
	}
	if upgraded.Events[0].EventID != cold.EventID || upgraded.Events[1].EventID != warm.EventID {
		t.Fatalf("upgrade changed deterministic event IDs")
	}

	// Appending a third step and parsing with the v0.3.0 cursor emits only the new step.
	db, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	stepThird := message(
		bytesField(1, message(varintField(1, 1793760300), varintField(2, 0))),
		bytesField(9, message(varintField(1, 1535), varintField(2, 1000), varintField(3, 150), varintField(5, 350000), varintField(9, 50), varintField(10, 100))),
	)
	if _, err := db.Exec(`INSERT INTO steps (idx, step_type, metadata) VALUES (3, 15, ?)`, stepThird); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	inc, err := adapter.Parse(context.Background(), sources[0], adapters.ParseRequest{MachineID: "machine", Cursor: first.Cursor})
	if err != nil || len(inc.Events) != 1 {
		t.Fatalf("incremental parse = %d events, error: %v", len(inc.Events), err)
	}
	if inc.Events[0].TotalTokens == nil || *inc.Events[0].TotalTokens != 351150 || inc.Cursor.Offset != 3 {
		t.Fatalf("incremental event = %+v, cursor = %+v", inc.Events[0], inc.Cursor)
	}
}

func TestParseLegacyGenerationMetadataUsesActualOutputAndCacheRead(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".gemini", "antigravity-cli", "conversations")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "legacy-cache.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	// Field 1 (1132) is the ceiling; field 3 (605) is actual output;
	// field 2 (9405) is uncached input; field 5 (4048) is cache read; field 9 (503) is reasoning.
	stats := message(
		varintField(1, 1132),
		varintField(2, 9405),
		varintField(3, 605),
		varintField(5, 4048),
		varintField(9, 503),
		varintField(10, 102),
	)
	timestamp := message(varintField(1, uint64(time.Date(2026, 6, 16, 3, 32, 45, 497045000, time.UTC).Unix())), varintField(2, 497045000))
	generation := message(bytesField(4, stats), bytesField(9, bytesField(4, timestamp)), bytesField(19, []byte("gemini-3-flash-agent")))
	record := message(bytesField(1, generation))

	if _, err := db.Exec(`CREATE TABLE gen_metadata (idx INTEGER PRIMARY KEY, data BLOB, size INTEGER NOT NULL DEFAULT 0);
INSERT INTO gen_metadata (idx, data, size) VALUES (0, ?, ?);`, record, len(record)); err != nil {
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
	res, err := adapter.Parse(context.Background(), sources[0], adapters.ParseRequest{MachineID: "machine"})
	if err != nil || len(res.Events) != 1 {
		t.Fatalf("legacy parse = %d events, error: %v", len(res.Events), err)
	}
	ev := res.Events[0]
	if ev.InputTokens == nil || *ev.InputTokens != 9405 ||
		ev.OutputTokens == nil || *ev.OutputTokens != 605 ||
		ev.CacheReadTokens == nil || *ev.CacheReadTokens != 4048 ||
		ev.ReasoningTokens == nil || *ev.ReasoningTokens != 503 ||
		ev.TotalTokens == nil || *ev.TotalTokens != 14058 {
		t.Fatalf("unexpected legacy tokens: %+v", ev)
	}
}
