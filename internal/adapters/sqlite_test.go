package adapters

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
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
