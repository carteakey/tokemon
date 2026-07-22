package antigravity

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
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
	if event.Project != "carteakey.dev" || event.Metadata != nil || event.Source.Identity == sources[0].Path || event.SessionID != "session-private-title" {
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
