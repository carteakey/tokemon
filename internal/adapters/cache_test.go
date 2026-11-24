package adapters

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"

	"github.com/tokemon/tokemon/internal/usage"
)

func TestSnapshotCacheDoesNotRetainEvents(t *testing.T) {
	cache := NewSnapshotCache()
	signature := FileSignature{Size: 10, ModTime: 20}
	request := Cursor{Identity: "identity", Offset: 0}
	result := ParseResult{Events: []usage.Event{{EventID: "event"}}, Cursor: Cursor{Identity: "identity", Offset: 10}}
	cache.Store("source", signature, "identity", request, result, nil)

	cached, err, ok := cache.Lookup("source", signature, "identity", result.Cursor)
	if !ok || err != nil {
		t.Fatalf("cache lookup = result=%+v, error=%v, ok=%t", cached, err, ok)
	}
	if len(cached.Events) != 0 || cached.Cursor != result.Cursor {
		t.Fatalf("cached result retained events or cursor changed: %+v", cached)
	}
}

func TestSnapshotCacheDoesNotRetainCanceledErrorsAndPrunes(t *testing.T) {
	cache := NewSnapshotCache()
	signature := FileSignature{Size: 10, ModTime: 20}
	request := Cursor{Offset: 0}
	cache.Store("canceled", signature, "identity", request, ParseResult{}, context.Canceled)
	if _, err, ok := cache.Lookup("canceled", signature, "identity", request); ok || err != nil {
		t.Fatalf("canceled error was cached: error=%v, ok=%t", err, ok)
	}
	cache.Store("kept", signature, "identity", request, ParseResult{}, errors.New("malformed"))
	cache.Store("also-kept", signature, "identity", request, ParseResult{}, nil)
	cache.Prune(map[string]struct{}{"kept": {}})
	if _, err, ok := cache.Lookup("kept", signature, "identity", request); !ok || err == nil {
		t.Fatalf("kept error was pruned: error=%v, ok=%t", err, ok)
	}
	if _, err, ok := cache.Lookup("also-kept", signature, "identity", request); ok || err != nil {
		t.Fatalf("stale cache entry remained: error=%v, ok=%t", err, ok)
	}
}

func TestSignatureIncludesSQLiteWALSidecar(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.db")
	if err := os.WriteFile(path, []byte("database"), 0o600); err != nil {
		t.Fatal(err)
	}
	first, err := Signature(path)
	if err != nil {
		t.Fatal(err)
	}
	if first.Sidecar {
		t.Fatal("unexpected WAL sidecar")
	}
	if err := os.WriteFile(path+"-wal", []byte("wal"), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := Signature(path)
	if err != nil {
		t.Fatal(err)
	}
	if !second.Sidecar || second == first {
		t.Fatalf("WAL sidecar was not included in signature: first=%+v second=%+v", first, second)
	}
}

func TestDirectoryCacheAvoidsUnchangedRecursiveWalks(t *testing.T) {
	root := filepath.Join(t.TempDir(), "sessions")
	nested := filepath.Join(root, "2026", "07")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	firstPath := filepath.Join(nested, "first.jsonl")
	if err := os.WriteFile(firstPath, []byte("first\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cache := NewDirectoryCache()
	walks := 0
	walk := cache.walk
	cache.walk = func(path string, fn fs.WalkDirFunc) error {
		walks++
		return walk(path, fn)
	}
	for i := 0; i < 100; i++ {
		paths, err := cache.Paths(context.Background(), root, ".jsonl")
		if err != nil || len(paths) != 1 || paths[0] != firstPath {
			t.Fatalf("cached paths %d = %v, error: %v", i, paths, err)
		}
	}
	if walks != 1 {
		t.Fatalf("unchanged recursive walks = %d, want 1", walks)
	}
	secondPath := filepath.Join(nested, "second.jsonl")
	if err := os.WriteFile(secondPath, []byte("second\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	paths, err := cache.Paths(context.Background(), root, ".jsonl")
	if err != nil || len(paths) != 2 {
		t.Fatalf("changed paths = %v, error: %v", paths, err)
	}
	if walks != 2 {
		t.Fatalf("changed recursive walks = %d, want 2", walks)
	}
}

func TestDirectoryCachePathsMatchesMultiPartSuffixesWithoutCrossMatch(t *testing.T) {
	root := filepath.Join(t.TempDir(), "archived_sessions")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	jsonlPath := filepath.Join(root, "active.jsonl")
	zstPath := filepath.Join(root, "archived.JSONL.ZST")
	plainZstPath := filepath.Join(root, "other.zst")
	for _, p := range []string{jsonlPath, zstPath, plainZstPath} {
		if err := os.WriteFile(p, []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	cache := NewDirectoryCache()
	onlyJSONL, err := cache.Paths(context.Background(), root, ".jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if len(onlyJSONL) != 1 || onlyJSONL[0] != jsonlPath {
		t.Fatalf(".jsonl paths = %v, want [%s]", onlyJSONL, jsonlPath)
	}

	both, err := cache.Paths(context.Background(), root, ".jsonl", ".jsonl.zst")
	if err != nil {
		t.Fatal(err)
	}
	if len(both) != 2 || both[0] != jsonlPath || both[1] != zstPath {
		t.Fatalf("combined paths = %v, want [%s %s]", both, jsonlPath, zstPath)
	}

	none, err := cache.Paths(context.Background(), root)
	if err != nil || len(none) != 0 {
		t.Fatalf("empty extensions = %v, err=%v; want nil, nil", none, err)
	}
}

func TestDirectoryCacheSubdirLifecycleRootRecreateAndSymlinks(t *testing.T) {
	base := t.TempDir()
	root := filepath.Join(base, "sessions")
	sub := filepath.Join(root, "2026", "08")
	if err := os.MkdirAll(sub, 0o755); err != nil {
		t.Fatal(err)
	}
	fileA := filepath.Join(sub, "a.jsonl")
	if err := os.WriteFile(fileA, []byte("a\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	// Symlinked .jsonl file and symlinked subdir must both be skipped.
	outsideFile := filepath.Join(base, "outside.jsonl")
	if err := os.WriteFile(outsideFile, []byte("out\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideFile, filepath.Join(root, "sym.jsonl")); err != nil {
		t.Fatal(err)
	}
	outsideDir := filepath.Join(base, "outside-dir")
	if err := os.MkdirAll(outsideDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(outsideDir, "b.jsonl"), []byte("b\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outsideDir, filepath.Join(root, "symdir")); err != nil {
		t.Fatal(err)
	}

	cache := NewDirectoryCache()
	paths, err := cache.Paths(context.Background(), root, ".jsonl")
	if err != nil {
		t.Fatal(err)
	}
	if len(paths) != 1 || paths[0] != fileA {
		t.Fatalf("initial paths = %v, want [%s] (symlinks must be skipped)", paths, fileA)
	}

	// Creating a new subdirectory and file invalidates the cache.
	sub2 := filepath.Join(root, "2026", "09")
	if err := os.MkdirAll(sub2, 0o755); err != nil {
		t.Fatal(err)
	}
	fileC := filepath.Join(sub2, "c.jsonl")
	if err := os.WriteFile(fileC, []byte("c\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	paths, err = cache.Paths(context.Background(), root, ".jsonl")
	if err != nil || len(paths) != 2 {
		t.Fatalf("after subdir create paths = %v, err=%v (want 2)", paths, err)
	}

	// Deleting the subdirectory drops its file from Paths.
	if err := os.RemoveAll(sub2); err != nil {
		t.Fatal(err)
	}
	paths, err = cache.Paths(context.Background(), root, ".jsonl")
	if err != nil || len(paths) != 1 || paths[0] != fileA {
		t.Fatalf("after subdir delete paths = %v, err=%v (want [%s])", paths, err, fileA)
	}

	// Deleting root removes the stale entry from c.entries and returns empty.
	if err := os.RemoveAll(root); err != nil {
		t.Fatal(err)
	}
	paths, err = cache.Paths(context.Background(), root, ".jsonl")
	if err != nil || len(paths) != 0 {
		t.Fatalf("after root delete paths = %v, err=%v (want empty)", paths, err)
	}
	cache.mu.Lock()
	entriesCount := len(cache.entries)
	cache.mu.Unlock()
	if entriesCount != 0 {
		t.Fatalf("deleted root retained %d stale cache entries", entriesCount)
	}

	// Recreating root discovers newly added files cleanly.
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	recreated := filepath.Join(root, "recreated.jsonl")
	if err := os.WriteFile(recreated, []byte("new\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	paths, err = cache.Paths(context.Background(), root, ".jsonl")
	if err != nil || len(paths) != 1 || paths[0] != recreated {
		t.Fatalf("after root recreate paths = %v, err=%v (want [%s])", paths, err, recreated)
	}
}
