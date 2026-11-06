package adapters

import (
	"container/list"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
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
	path         string
	device       uint64
	inode        uint64
	identifiable bool
	inserted     *list.Element
}

// SQLiteHandles reuses read-only SQLite connections across polling cycles.
// Opening a modernc SQLite database costs an order of magnitude more than the
// queries a cycle runs, so per-source handles are kept on the adapter (whose
// lifetime is the agent process) and validated against the current file
// identity on every use. A reuse also pings the connection, so one that went
// bad while cached is reopened instead of failing a cycle; the ping costs a
// small fraction of the open it guards (see the SQLiteHandles benchmarks in
// sqlite_test.go).
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
		// A cheap ping guards against a connection that went bad while
		// cached (for example after a provider-side recovery): a dead or
		// stale-inode handle is dropped and reopened below, never served.
		matches := identifiable && handle.device == device && handle.inode == inode
		if matches && handle.db.PingContext(ctx) == nil {
			h.order.MoveToBack(handle.inserted)
			return handle.db, nil
		}
		reason := "identity_changed"
		if matches {
			reason = "connection_lost"
		}
		_ = h.removeLocked(handle)
		logSQLiteHandleEvent("sqlite_handle_reopened", reason, path)
	}

	db, err := openReadOnlyDB(ctx, path)
	if err != nil {
		return nil, err
	}
	handle := &sqliteHandle{db: db, path: path, device: device, inode: inode, identifiable: identifiable}
	if len(h.entries) >= h.max {
		if oldest := h.order.Front(); oldest != nil {
			if stalePath, ok := oldest.Value.(string); ok {
				if stale := h.entries[stalePath]; stale != nil {
					logSQLiteHandleEvent("sqlite_handle_evicted", "capacity", stalePath)
					_ = h.removeLocked(stale)
				}
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
		_ = h.removeLocked(handle)
	}
}

// removeLocked closes one cached handle and forgets it. The caller holds h.mu.
func (h *SQLiteHandles) removeLocked(handle *sqliteHandle) error {
	err := handle.db.Close()
	h.order.Remove(handle.inserted)
	delete(h.entries, handle.path)
	return err
}

// Close closes every cached handle and empties the cache. The cache stays
// usable: a later Get reopens on demand. Adapters currently live for the
// whole agent process, where exit closes the connections; Close exists so a
// future adapter shutdown hook can release the handles earlier.
func (h *SQLiteHandles) Close() error {
	h.mu.Lock()
	defer h.mu.Unlock()
	var firstErr error
	for _, handle := range h.entries {
		if err := h.removeLocked(handle); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// logSQLiteHandleEvent records a debug cache event for diagnosing a thrashing
// cache. Like every adapter log it never carries the raw path: the source
// identity hash correlates the events of one source without exposing it.
func logSQLiteHandleEvent(message, reason, path string) {
	logger := slog.Default()
	if !logger.Enabled(context.Background(), slog.LevelDebug) {
		return
	}
	logger.Debug(message, "reason", reason, "source", HashIdentity(path))
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
