package adapters

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// FileSignature identifies the contents that can affect a metadata snapshot.
// SQLite adapters also include the optional WAL sidecar because a database can
// change without the main file's size or modification time changing.
type FileSignature struct {
	Size        int64
	ModTime     int64
	SidecarSize int64
	SidecarTime int64
	Sidecar     bool
}

// Signature returns a lightweight file signature without opening the source.
func Signature(path string) (FileSignature, error) {
	info, err := os.Stat(path)
	if err != nil {
		return FileSignature{}, err
	}
	signature := FileSignature{Size: info.Size(), ModTime: info.ModTime().UnixNano()}
	if sidecar, err := os.Stat(path + "-wal"); err == nil {
		signature.Sidecar = true
		signature.SidecarSize = sidecar.Size()
		signature.SidecarTime = sidecar.ModTime().UnixNano()
	} else if !os.IsNotExist(err) {
		return FileSignature{}, err
	}
	return signature, nil
}

type snapshotCacheEntry struct {
	signature     FileSignature
	identity      string
	requestCursor Cursor
	resultCursor  Cursor
	err           error
}

// SnapshotCache suppresses unchanged full-source parses without retaining the
// parsed events. Entries are bounded by the currently discovered source set.
type SnapshotCache struct {
	mu      sync.Mutex
	entries map[string]snapshotCacheEntry
}

func NewSnapshotCache() *SnapshotCache {
	return &SnapshotCache{entries: make(map[string]snapshotCacheEntry)}
}

// Lookup returns a cursor-only result for a matching successful snapshot, or a
// previously cached source error. Context cancellation is never cached.
func (c *SnapshotCache) Lookup(path string, signature FileSignature, identity string, request Cursor) (ParseResult, error, bool) {
	if c == nil {
		return ParseResult{}, nil, false
	}
	c.mu.Lock()
	entry, ok := c.entries[path]
	c.mu.Unlock()
	if !ok || entry.signature != signature || entry.identity != identity {
		return ParseResult{}, nil, false
	}
	if entry.err != nil {
		if entry.requestCursor != request {
			return ParseResult{}, nil, false
		}
		return ParseResult{}, entry.err, true
	}
	if entry.resultCursor != request {
		return ParseResult{}, nil, false
	}
	return ParseResult{Cursor: entry.resultCursor}, nil, true
}

// Store records only cursor/error metadata. It intentionally drops event
// slices so a large historical snapshot cannot remain reachable between polls.
func (c *SnapshotCache) Store(path string, signature FileSignature, identity string, request Cursor, result ParseResult, err error) {
	if c == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return
	}
	c.mu.Lock()
	c.entries[path] = snapshotCacheEntry{
		signature: signature, identity: identity, requestCursor: request,
		resultCursor: result.Cursor, err: err,
	}
	c.mu.Unlock()
}

// Prune removes cache entries for sources that no longer exist or are no
// longer exposed by an adapter. This keeps long-running agents bounded.
func (c *SnapshotCache) Prune(seen map[string]struct{}) {
	if c == nil {
		return
	}
	c.mu.Lock()
	for path := range c.entries {
		if _, ok := seen[path]; !ok {
			delete(c.entries, path)
		}
	}
	c.mu.Unlock()
}

type directoryCacheEntry struct {
	paths       []string
	directories map[string]FileSignature
}

// DirectoryCache avoids repeating recursive directory walks when the set of
// source files is unchanged. It validates known directory metadata first, so
// new or removed files/directories invalidate the listing without ignoring
// newly-created sessions.
type DirectoryCache struct {
	mu      sync.Mutex
	entries map[string]directoryCacheEntry
	walk    func(string, fs.WalkDirFunc) error
}

func NewDirectoryCache() *DirectoryCache {
	return &DirectoryCache{entries: make(map[string]directoryCacheEntry), walk: filepath.WalkDir}
}

// Paths returns regular files matching any requested extension suffix under root.
func (c *DirectoryCache) Paths(ctx context.Context, root string, extensions ...string) ([]string, error) {
	if c == nil {
		return nil, nil
	}
	normalized := normalizeExtensions(extensions)
	if len(normalized) == 0 {
		return nil, nil
	}
	key := root + "\x00" + strings.Join(normalized, "\x00")
	c.mu.Lock()
	cached, ok := c.entries[key]
	c.mu.Unlock()
	if ok {
		unchanged, err := directoriesUnchanged(ctx, cached.directories)
		if err != nil {
			return nil, err
		}
		if unchanged {
			return append([]string(nil), cached.paths...), nil
		}
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	walk := c.walk
	if walk == nil {
		walk = filepath.WalkDir
	}
	var paths []string
	directories := make(map[string]FileSignature)
	err := walk(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			if os.IsNotExist(err) {
				return nil
			}
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if entry.IsDir() {
			info, err := entry.Info()
			if err != nil {
				if os.IsNotExist(err) {
					return nil
				}
				return err
			}
			directories[path] = FileSignature{Size: info.Size(), ModTime: info.ModTime().UnixNano()}
			return nil
		}
		if entry.Type().IsRegular() && hasAnyExtension(path, normalized) {
			paths = append(paths, path)
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		return nil, err
	}
	sort.Strings(paths)
	if len(directories) == 0 {
		return paths, nil
	}
	entry := directoryCacheEntry{paths: append([]string(nil), paths...), directories: directories}
	c.mu.Lock()
	c.entries[key] = entry
	c.mu.Unlock()
	return paths, nil
}

func normalizeExtensions(extensions []string) []string {
	if len(extensions) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(extensions))
	normalized := make([]string, 0, len(extensions))
	for _, ext := range extensions {
		ext = strings.ToLower(strings.TrimSpace(ext))
		if ext == "" {
			continue
		}
		if _, exists := seen[ext]; exists {
			continue
		}
		seen[ext] = struct{}{}
		normalized = append(normalized, ext)
	}
	sort.Strings(normalized)
	return normalized
}

func hasAnyExtension(path string, extensions []string) bool {
	base := filepath.Base(path)
	for _, ext := range extensions {
		if len(base) >= len(ext) && strings.EqualFold(base[len(base)-len(ext):], ext) {
			return true
		}
	}
	return false
}

func directoriesUnchanged(ctx context.Context, directories map[string]FileSignature) (bool, error) {
	for path, expected := range directories {
		if err := ctx.Err(); err != nil {
			return false, err
		}
		info, err := os.Stat(path)
		if os.IsNotExist(err) {
			return false, nil
		}
		if err != nil {
			return false, err
		}
		actual := FileSignature{Size: info.Size(), ModTime: info.ModTime().UnixNano()}
		if actual != expected {
			return false, nil
		}
	}
	return true, nil
}
