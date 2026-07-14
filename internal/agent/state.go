package agent

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/tokemon/tokemon/internal/adapters"
	"github.com/tokemon/tokemon/internal/usage"
	_ "modernc.org/sqlite"
)

const stateSchema = `
CREATE TABLE IF NOT EXISTS source_state (
    machine_id TEXT NOT NULL,
    adapter TEXT NOT NULL,
    source_path TEXT NOT NULL,
    source_identity TEXT NOT NULL,
    cursor_identity TEXT NOT NULL,
    cursor_offset INTEGER NOT NULL,
    cursor_line INTEGER NOT NULL DEFAULT 0,
    last_successful_sync TEXT NOT NULL,
    PRIMARY KEY (machine_id, adapter, source_path)
);

CREATE TABLE IF NOT EXISTS event_state (
    machine_id TEXT NOT NULL,
    event_id TEXT NOT NULL,
    fingerprint BLOB NOT NULL,
    last_successful_sync TEXT NOT NULL,
    PRIMARY KEY (machine_id, event_id)
);
`

// StateStore persists source cursors and event fingerprints for one agent.
// The server database remains a separate store of normalized usage events.
type StateStore struct {
	db *sql.DB
}

// StateSnapshot is the committed view used for one polling pass.
type StateSnapshot struct {
	Cursors      map[string]adapters.Cursor
	fingerprints map[string][sha256.Size]byte
}

// OpenState opens or creates a local agent state database.
func OpenState(path string) (*StateStore, error) {
	if strings.TrimSpace(path) == "" {
		return nil, errors.New("agent state path is required")
	}
	if directory := filepath.Dir(path); directory != "." {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return nil, fmt.Errorf("create agent state directory: %w", err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open agent state: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec("PRAGMA busy_timeout = 5000"); err != nil {
		db.Close()
		return nil, fmt.Errorf("configure agent state: %w", err)
	}
	if _, err := db.Exec(stateSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("initialize agent state: %w", err)
	}
	if !strings.HasPrefix(path, ":") && !strings.HasPrefix(path, "file:") {
		if err := os.Chmod(path, 0o600); err != nil {
			db.Close()
			return nil, fmt.Errorf("protect agent state: %w", err)
		}
	}
	return &StateStore{db: db}, nil
}

func (s *StateStore) Close() error {
	if s == nil || s.db == nil {
		return nil
	}
	return s.db.Close()
}

// Snapshot loads only committed state for the configured machine. A changed
// machine ID intentionally starts with an empty view.
func (s *StateStore) Snapshot(ctx context.Context, machineID string) (StateSnapshot, error) {
	snapshot := StateSnapshot{
		Cursors:      make(map[string]adapters.Cursor),
		fingerprints: make(map[string][sha256.Size]byte),
	}
	rows, err := s.db.QueryContext(ctx, `
SELECT adapter, source_path, cursor_identity, cursor_offset, cursor_line
FROM source_state
WHERE machine_id = ?`, machineID)
	if err != nil {
		return StateSnapshot{}, fmt.Errorf("read agent cursors: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var adapter, sourcePath, identity string
		var offset, line int64
		if err := rows.Scan(&adapter, &sourcePath, &identity, &offset, &line); err != nil {
			return StateSnapshot{}, fmt.Errorf("scan agent cursor: %w", err)
		}
		snapshot.Cursors[sourceKey(adapter, sourcePath)] = adapters.Cursor{Identity: identity, Offset: offset, Line: line}
	}
	if err := rows.Err(); err != nil {
		return StateSnapshot{}, fmt.Errorf("read agent cursors: %w", err)
	}

	rows, err = s.db.QueryContext(ctx, `
SELECT event_id, fingerprint
FROM event_state
WHERE machine_id = ?`, machineID)
	if err != nil {
		return StateSnapshot{}, fmt.Errorf("read agent event state: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var eventID string
		var fingerprint []byte
		if err := rows.Scan(&eventID, &fingerprint); err != nil {
			return StateSnapshot{}, fmt.Errorf("scan agent event state: %w", err)
		}
		if len(fingerprint) != sha256.Size {
			return StateSnapshot{}, fmt.Errorf("event %q has invalid fingerprint length %d", eventID, len(fingerprint))
		}
		var digest [sha256.Size]byte
		copy(digest[:], fingerprint)
		snapshot.fingerprints[eventID] = digest
	}
	if err := rows.Err(); err != nil {
		return StateSnapshot{}, fmt.Errorf("read agent event state: %w", err)
	}
	return snapshot, nil
}

// Pending returns only new or changed events since the last successful sync.
func (s StateSnapshot) Pending(events []usage.Event) []usage.Event {
	pending := make([]usage.Event, 0, len(events))
	for _, event := range events {
		fingerprint := eventFingerprint(event)
		if previous, ok := s.fingerprints[event.EventID]; ok && previous == fingerprint {
			continue
		}
		pending = append(pending, event)
	}
	return pending
}

// Commit advances successful source cursors and records uploaded event
// fingerprints atomically. Call it only after the server accepts the batch,
// or when a poll produced no pending events.
func (s *StateStore) Commit(ctx context.Context, machineID string, reports []adapters.SourceReport, events []usage.Event, syncedAt time.Time) error {
	if strings.TrimSpace(machineID) == "" {
		return errors.New("machine ID is required")
	}
	synced := syncedAt.UTC().Format(time.RFC3339Nano)
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin agent state commit: %w", err)
	}
	rollback := func(commitErr error) error {
		_ = tx.Rollback()
		return commitErr
	}
	for _, report := range reports {
		if report.Err != nil {
			continue
		}
		identity := report.Cursor.Identity
		if identity == "" {
			identity = report.Source.Identity
		}
		if identity == "" {
			identity = adapters.HashIdentity(report.Adapter + ":" + report.Path)
		}
		cursor := report.Cursor
		cursor.Identity = identity
		_, err := tx.ExecContext(ctx, `
INSERT INTO source_state (
    machine_id, adapter, source_path, source_identity,
    cursor_identity, cursor_offset, cursor_line, last_successful_sync
) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(machine_id, adapter, source_path) DO UPDATE SET
    source_identity = excluded.source_identity,
    cursor_identity = excluded.cursor_identity,
    cursor_offset = excluded.cursor_offset,
    cursor_line = excluded.cursor_line,
    last_successful_sync = excluded.last_successful_sync`,
			machineID, report.Adapter, report.Path, identity,
			cursor.Identity, cursor.Offset, cursor.Line, synced)
		if err != nil {
			return rollback(fmt.Errorf("save cursor for %s: %w", report.Path, err))
		}
	}
	for _, event := range events {
		if strings.TrimSpace(event.EventID) == "" {
			return rollback(errors.New("cannot save agent state for event without an ID"))
		}
		fingerprint := eventFingerprint(event)
		_, err := tx.ExecContext(ctx, `
INSERT INTO event_state (machine_id, event_id, fingerprint, last_successful_sync)
VALUES (?, ?, ?, ?)
ON CONFLICT(machine_id, event_id) DO UPDATE SET
    fingerprint = excluded.fingerprint,
    last_successful_sync = excluded.last_successful_sync`,
			machineID, event.EventID, fingerprint[:], synced)
		if err != nil {
			return rollback(fmt.Errorf("save event state for %s: %w", event.EventID, err))
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit agent state: %w", err)
	}
	return nil
}

func sourceKey(adapter, path string) string {
	return adapter + "\x00" + path
}
