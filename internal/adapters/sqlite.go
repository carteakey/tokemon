package adapters

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"time"

	_ "modernc.org/sqlite"
)

// OpenReadOnly opens a discovered SQLite source without allowing the agent to
// mutate the provider's database.
func OpenReadOnly(ctx context.Context, path string) (*sql.DB, error) {
	if _, err := os.Stat(path); err != nil {
		return nil, err
	}
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
