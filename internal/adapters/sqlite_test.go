package adapters

import (
	"context"
	"database/sql"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func newFixtureDB(t *testing.T, path, marker string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS marker (value TEXT); INSERT INTO marker VALUES ('` + marker + `')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestSQLiteHandlesReusesUnchangedFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	newFixtureDB(t, path, "one")
	handles := NewSQLiteHandles(2)
	ctx := context.Background()
	first, err := handles.Get(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	second, err := handles.Get(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatal("unchanged file opened a second connection instead of reusing the handle")
	}
	if handles.Len() != 1 {
		t.Fatalf("cache length = %d, want 1", handles.Len())
	}
}

func TestSQLiteHandlesReopensReplacedFiles(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	newFixtureDB(t, path, "old")
	handles := NewSQLiteHandles(2)
	ctx := context.Background()
	first, err := handles.Get(ctx, path)
	if err != nil {
		t.Fatal(err)
	}

	// Replace the file at the same path: a new inode, so the cached handle
	// must be dropped and reopened — it would otherwise serve the old inode.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	newFixtureDB(t, path, "new")
	second, err := handles.Get(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if second == first {
		t.Fatal("replaced file reused the stale handle")
	}
	var value string
	if err := second.QueryRowContext(ctx, `SELECT value FROM marker`).Scan(&value); err != nil || value != "new" {
		t.Fatalf("replaced file content = %q, err %v, want new", value, err)
	}
}

func TestSQLiteHandlesDropsDeletedFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	newFixtureDB(t, path, "gone")
	handles := NewSQLiteHandles(2)
	ctx := context.Background()
	if _, err := handles.Get(ctx, path); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if _, err := handles.Get(ctx, path); err == nil {
		t.Fatal("Get succeeded for a deleted file")
	}
	if handles.Len() != 0 {
		t.Fatalf("cache length = %d, want 0 after deletion", handles.Len())
	}
}

func TestSQLiteHandlesEvictsLeastRecentlyUsed(t *testing.T) {
	dir := t.TempDir()
	paths := make([]string, 3)
	for index, name := range []string{"a.db", "b.db", "c.db"} {
		paths[index] = filepath.Join(dir, name)
		newFixtureDB(t, paths[index], name)
	}
	handles := NewSQLiteHandles(2)
	ctx := context.Background()
	for _, path := range paths {
		if _, err := handles.Get(ctx, path); err != nil {
			t.Fatal(err)
		}
	}
	if handles.Len() != 2 {
		t.Fatalf("cache length = %d, want 2 after cap", handles.Len())
	}
	// a.db was evicted; getting it again must reopen without error.
	if _, err := handles.Get(ctx, paths[0]); err != nil {
		t.Fatal(err)
	}
	if handles.Len() != 2 {
		t.Fatalf("cache length = %d, want 2", handles.Len())
	}
}

func TestSQLiteHandlesRecoversBrokenCachedConnection(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	newFixtureDB(t, path, "data")
	handles := NewSQLiteHandles(2)
	ctx := context.Background()
	first, err := handles.Get(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	// Simulate a connection that went bad while cached.
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := handles.Get(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	var value string
	if err := second.QueryRowContext(ctx, `SELECT value FROM marker`).Scan(&value); err != nil || value != "data" {
		t.Fatalf("recovered handle read = %q, err %v", value, err)
	}
}

func TestSQLiteHandlesCloseEmptiesCache(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	newFixtureDB(t, path, "data")
	handles := NewSQLiteHandles(2)
	ctx := context.Background()
	first, err := handles.Get(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if err := handles.Close(); err != nil {
		t.Fatal(err)
	}
	if handles.Len() != 0 {
		t.Fatalf("cache length = %d, want 0 after Close", handles.Len())
	}
	// The cache stays usable: a later Get reopens on demand.
	second, err := handles.Get(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	if second == first {
		t.Fatal("Get after Close reused the closed handle")
	}
	var value string
	if err := second.QueryRowContext(ctx, `SELECT value FROM marker`).Scan(&value); err != nil || value != "data" {
		t.Fatalf("handle after Close read = %q, err %v", value, err)
	}
}

// recordCollector captures debug records so the cache's reopen and evict
// events can be asserted without letting them reach stderr.
type recordCollector struct {
	records []slog.Record
}

func (c *recordCollector) Enabled(context.Context, slog.Level) bool { return true }

func (c *recordCollector) Handle(_ context.Context, record slog.Record) error {
	c.records = append(c.records, record.Clone())
	return nil
}

func (c *recordCollector) WithAttrs([]slog.Attr) slog.Handler { return c }

func (c *recordCollector) WithGroup(string) slog.Handler { return c }

func TestSQLiteHandlesLogsReopenAndEvict(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.db")
	newFixtureDB(t, path, "one")
	collector := &recordCollector{}
	previous := slog.Default()
	slog.SetDefault(slog.New(collector))
	defer slog.SetDefault(previous)
	handles := NewSQLiteHandles(1)
	ctx := context.Background()
	if _, err := handles.Get(ctx, path); err != nil {
		t.Fatal(err)
	}
	// Replace the file at the same path: the identity change reopens and is
	// logged, and the log record must not carry the raw path.
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	newFixtureDB(t, path, "two")
	if _, err := handles.Get(ctx, path); err != nil {
		t.Fatal(err)
	}
	// The cache is at cap, so opening a second source evicts the first.
	other := filepath.Join(dir, "other.db")
	newFixtureDB(t, other, "other")
	if _, err := handles.Get(ctx, other); err != nil {
		t.Fatal(err)
	}
	counts := make(map[string]int)
	for _, record := range collector.records {
		counts[record.Message]++
		record.Attrs(func(attr slog.Attr) bool {
			if attr.Key == "source" && strings.Contains(attr.Value.String(), string(filepath.Separator)) {
				t.Errorf("debug record %q carries a raw path", record.Message)
			}
			return true
		})
	}
	if counts["sqlite_handle_reopened"] != 1 || counts["sqlite_handle_evicted"] != 1 {
		t.Fatalf("debug records = %v, want one reopen and one evict", counts)
	}
}

func BenchmarkSQLiteHandlesGet(b *testing.B) {
	path := filepath.Join(b.TempDir(), "state.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		b.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE marker (value TEXT)`); err != nil {
		b.Fatal(err)
	}
	if err := db.Close(); err != nil {
		b.Fatal(err)
	}
	handles := NewSQLiteHandles(2)
	ctx := context.Background()
	if _, err := handles.Get(ctx, path); err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if _, err := handles.Get(ctx, path); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkSQLiteHandlesColdOpen(b *testing.B) {
	path := filepath.Join(b.TempDir(), "state.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		b.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE marker (value TEXT)`); err != nil {
		b.Fatal(err)
	}
	if err := db.Close(); err != nil {
		b.Fatal(err)
	}
	ctx := context.Background()
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		db, err := OpenReadOnly(ctx, path)
		if err != nil {
			b.Fatal(err)
		}
		_ = db.Close()
	}
}

// BenchmarkSQLiteHandlesPing isolates the per-reuse ping that guards cached
// connections, so its cost can be compared against the open it avoids.
func BenchmarkSQLiteHandlesPing(b *testing.B) {
	path := filepath.Join(b.TempDir(), "state.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		b.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE marker (value TEXT)`); err != nil {
		b.Fatal(err)
	}
	if err := db.Close(); err != nil {
		b.Fatal(err)
	}
	handles := NewSQLiteHandles(2)
	ctx := context.Background()
	handle, err := handles.Get(ctx, path)
	if err != nil {
		b.Fatal(err)
	}
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		if err := handle.PingContext(ctx); err != nil {
			b.Fatal(err)
		}
	}
}
