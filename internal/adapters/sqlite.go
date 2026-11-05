package adapters

import (
	"container/list"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"sync"
	"syscall"
	"time"

	_ "modernc.org/sqlite"
)

// OpenReadOnly opens a discovered SQLite source without allowing the agent to
// mutate the provider's database.
func OpenReadOnly(ctx context.Context, path string) (*sql.DB, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
	return openReadOnlyDB(ctx, path)
}

func openReadOnlyDB(ctx context.Context, path string) (*sql.DB, error) {
	dsn := (&url.URL{Scheme: "file", Path: path, RawQuery: "mode=ro"}).String()
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("open %s read-only: %w", path, err)
	}
	return db, nil
}

// sqliteHandle is one cached read-only connection plus the file identity it
// was opened against, so a source replaced at the same path is detected
// instead of silently serving the old inode's data.
type sqliteHandle struct {
	db           *sql.DB
	device       uint64
	inode        uint64
	identifiable bool
	inserted     *list.Element
}

// SQLiteHandles reuses read-only SQLite connections across polling cycles.
// Opening a modernc SQLite database costs an order of magnitude more than the
// queries a cycle runs, so per-source handles are kept on the adapter (whose
// lifetime is the agent process) and validated against the current file
// identity on every use.
type SQLiteHandles struct {
	mu      sync.Mutex
	entries map[string]*sqliteHandle
	order   *list.List // path strings, least recently used first
	max     int
}

// NewSQLiteHandles returns a handle cache holding at most max connections.
func NewSQLiteHandles(max int) *SQLiteHandles {
	if max <= 0 {
		max = 8
	}
	return &SQLiteHandles{entries: make(map[string]*sqliteHandle), order: list.New(), max: max}
}

// Len reports the number of cached handles.
func (h *SQLiteHandles) Len() int {
	h.mu.Lock()
	defer h.mu.Unlock()
	return len(h.entries)
}

// fileIdentity returns the file's device and inode where the platform
// exposes them (unix); ok is false where it does not, in which case callers
// must not reuse a cached connection.
func fileIdentity(info os.FileInfo) (device, inode uint64, ok bool) {
	if stat, ok := info.Sys().(*syscall.Stat_t); ok {
		return uint64(stat.Dev), uint64(stat.Ino), true
	}
	return 0, 0, false
}

// Get returns a read-only handle for path, reusing a cached connection when
// the file's identity is unchanged. The file is stat'd on every call: a
// missing file drops the cached handle and returns the error, and a replaced
// file reopens so the caller never reads a stale inode.
func (h *SQLiteHandles) Get(ctx context.Context, path string) (*sql.DB, error) {
	info, err := os.Stat(path)
	if err != nil {
		h.drop(path)
		return nil, err
	}
	device, inode, identifiable := fileIdentity(info)

	h.mu.Lock()
	defer h.mu.Unlock()
	if handle, ok := h.entries[path]; ok {
		if identifiable && handle.device == device && handle.inode == inode {
			// A cheap ping guards against a connection that went bad while
			// cached (for example after a provider-side recovery); a dead
			// handle is dropped and reopened below.
			if err := handle.db.PingContext(ctx); err == nil {
				h.order.MoveToBack(handle.inserted)
				return handle.db, nil
			}
			_ = handle.db.Close()
			h.order.Remove(handle.inserted)
			delete(h.entries, path)
		} else {
			_ = handle.db.Close()
			h.order.Remove(handle.inserted)
			delete(h.entries, path)
		}
		// The path now refers to a different file: the cached connection
		// points at the old inode and would serve stale data.
		_ = handle.db.Close()
		h.order.Remove(handle.inserted)
		delete(h.entries, path)
	}

	db, err := openReadOnlyDB(ctx, path)
	if err != nil {
		return nil, err
	}
	handle := &sqliteHandle{db: db, device: device, inode: inode, identifiable: identifiable}
	if len(h.entries) >= h.max {
		if oldest := h.order.Front(); oldest != nil {
			if stalePath, ok := oldest.Value.(string); ok {
				if stale := h.entries[stalePath]; stale != nil {
					_ = stale.db.Close()
				}
				delete(h.entries, stalePath)
			}
			h.order.Remove(oldest)
		}
	}
	handle.inserted = h.order.PushBack(path)
	h.entries[path] = handle
	return db, nil
}

func (h *SQLiteHandles) drop(path string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if handle, ok := h.entries[path]; ok {
		_ = handle.db.Close()
		h.order.Remove(handle.inserted)
		delete(h.entries, path)
	}
}

func HasTable(ctx context.Context, db *sql.DB, name string) (bool, error) {
	var found string
	err := db.QueryRowContext(ctx, `SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, name).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	return err == nil, err
}

func HasColumn(ctx context.Context, db *sql.DB, table, column string) (bool, error) {
	rows, err := db.QueryContext(ctx, `PRAGMA table_info(`+table+`)`)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid, notNull, primaryKey int
		var name, kind string
		var defaultValue any
		if err := rows.Scan(&cid, &name, &kind, &notNull, &defaultValue, &primaryKey); err != nil {
			return false, err
		}
		if name == column {
			return true, nil
		}
	}
	return false, rows.Err()
}

// UnixTime accepts the timestamp units used by the local tools: seconds,
// milliseconds, microseconds, or nanoseconds since the Unix epoch.
func UnixTime(value int64) time.Time {
	switch {
	case value >= 1_000_000_000_000_000_000:
		return time.Unix(0, value)
	case value >= 1_000_000_000_000_000:
		return time.Unix(0, value*1_000)
	case value >= 1_000_000_000_000:
		return time.UnixMilli(value)
	default:
		return time.Unix(value, 0)
	}
}

func epochMilliseconds(value int64) int64 {
	switch {
	case value >= 1_000_000_000_000_000_000:
		return value / 1_000_000
	case value >= 1_000_000_000_000_000:
		return value / 1_000
	case value >= 1_000_000_000_000:
		return value
	default:
		return value * 1_000
	}
}

func DurationMS(start, end int64) *int64 {
	if end <= start {
		return nil
	}
	duration := epochMilliseconds(end) - epochMilliseconds(start)
	if duration < 0 {
		return nil
	}
	return &duration
}
