package agent

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tokemon/tokemon/internal/adapters"
	"github.com/tokemon/tokemon/internal/usage"
)

func TestStatePersistsCursorsAndEventFingerprints(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := OpenState(path)
	if err != nil {
		t.Fatal(err)
	}

	const machineID = "machine-a"
	report := adapters.SourceReport{
		Adapter: "generic-jsonl",
		Path:    "/private/local/usage.jsonl",
		Source:  adapters.Source{Path: "/private/local/usage.jsonl", Identity: "file-identity"},
		Cursor:  adapters.Cursor{Identity: "file-identity", Offset: 42, Line: 3},
	}
	event := stateEvent("event-1", 10)
	if err := store.Commit(context.Background(), machineID, StateSnapshot{}, []adapters.SourceReport{report}, []usage.Event{event}, time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("state mode = %o, want 600", info.Mode().Perm())
	}

	store, err = OpenState(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	snapshot, err := store.Snapshot(context.Background(), machineID)
	if err != nil {
		t.Fatal(err)
	}
	cursor, ok := snapshot.Cursors[SourceKey(report.Adapter, report.Path)]
	if !ok || cursor.Identity != "file-identity" || cursor.Offset != 42 || cursor.Line != 3 {
		t.Fatalf("cursor = %+v, present=%t", cursor, ok)
	}
	if pending, err := store.Pending(context.Background(), machineID, []usage.Event{event}); err != nil || len(pending) != 0 {
		t.Fatalf("unchanged event remained pending: %+v", pending)
	}
	changed := stateEvent("event-1", 20)
	if pending, err := store.Pending(context.Background(), machineID, []usage.Event{changed}); err != nil || len(pending) != 1 || pending[0].TotalTokens == nil || *pending[0].TotalTokens != 20 {
		t.Fatalf("changed event was not pending: %+v", pending)
	}

	// The committed sync time is readable for the heartbeat's per-source
	// health, keyed the same way as cursors.
	syncs, err := store.LastSuccessfulSyncs(context.Background(), machineID)
	if err != nil {
		t.Fatal(err)
	}
	synced, ok := syncs[SourceKey(report.Adapter, report.Path)]
	if !ok || !synced.Equal(time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)) {
		t.Fatalf("LastSuccessfulSyncs = %+v, want the committed time for %q", syncs, SourceKey(report.Adapter, report.Path))
	}
	if other, err := store.LastSuccessfulSyncs(context.Background(), "machine-b"); err != nil || len(other) != 0 {
		t.Fatalf("other machine syncs = %+v, err %v, want empty", other, err)
	}
}

func TestStateLeavesCursorAndFingerprintUncommittedUntilUploadSucceeds(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := OpenState(path)
	if err != nil {
		t.Fatal(err)
	}

	const machineID = "machine-a"
	report := adapters.SourceReport{
		Adapter: "generic-jsonl",
		Path:    "/private/local/usage.jsonl",
		Source:  adapters.Source{Path: "/private/local/usage.jsonl", Identity: "file-identity"},
		Cursor:  adapters.Cursor{Identity: "file-identity", Offset: 42},
	}
	event := stateEvent("event-1", 10)
	if pending, err := store.Pending(context.Background(), machineID, []usage.Event{event}); err != nil || len(pending) != 1 {
		store.Close()
		t.Fatalf("initial event pending = %d, want 1", len(pending))
	}
	// Simulate a failed upload: no Commit call is made.
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = OpenState(path)
	if err != nil {
		t.Fatal(err)
	}
	if pending, err := store.Pending(context.Background(), machineID, []usage.Event{event}); err != nil || len(pending) != 1 {
		store.Close()
		t.Fatalf("failed upload lost pending event: %d", len(pending))
	}
	if err := store.Commit(context.Background(), machineID, StateSnapshot{}, []adapters.SourceReport{report}, []usage.Event{event}, time.Now()); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = OpenState(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if pending, err := store.Pending(context.Background(), machineID, []usage.Event{event}); err != nil || len(pending) != 0 {
		t.Fatalf("committed event remained pending: %d", len(pending))
	}
}

func TestStateSkipsFailedSourceReports(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	store, err := OpenState(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	report := adapters.SourceReport{Adapter: "claude-code", Path: "/private/session.jsonl", Err: errors.New("source unavailable")}
	if err := store.Commit(context.Background(), "machine-a", StateSnapshot{}, []adapters.SourceReport{report}, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Snapshot(context.Background(), "machine-a")
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Cursors) != 0 {
		t.Fatalf("failed source advanced state: %+v", snapshot.Cursors)
	}
}

// Re-writing unchanged cursor rows once per poll cycle for every healthy
// source would churn the state database without changing any meaningful
// state, so Commit must skip them while still advancing changed cursors.
func TestStateCommitSkipsUnchangedCursorRows(t *testing.T) {
	store, err := OpenState(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	const machineID = "machine-a"
	report := adapters.SourceReport{
		Adapter: "generic-jsonl",
		Path:    "/private/local/usage.jsonl",
		Source:  adapters.Source{Path: "/private/local/usage.jsonl", Identity: "file-identity"},
		Cursor:  adapters.Cursor{Identity: "file-identity", Offset: 42, Line: 3},
	}
	firstSync := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	if err := store.Commit(context.Background(), machineID, StateSnapshot{}, []adapters.SourceReport{report}, nil, firstSync); err != nil {
		t.Fatal(err)
	}
	snapshot, err := store.Snapshot(context.Background(), machineID)
	if err != nil {
		t.Fatal(err)
	}

	unchangedSync := firstSync.Add(time.Hour)
	if err := store.Commit(context.Background(), machineID, snapshot, []adapters.SourceReport{report}, nil, unchangedSync); err != nil {
		t.Fatal(err)
	}
	var syncedAt string
	if err := store.db.QueryRow(`SELECT last_successful_sync FROM source_state WHERE machine_id = ? AND adapter = ? AND source_path = ?`, machineID, report.Adapter, report.Path).Scan(&syncedAt); err != nil {
		t.Fatal(err)
	}
	if syncedAt != firstSync.UTC().Format(time.RFC3339Nano) {
		t.Fatalf("unchanged cursor row was re-written: last_successful_sync = %q", syncedAt)
	}

	advanced := report
	advanced.Cursor = adapters.Cursor{Identity: "file-identity", Offset: 100, Line: 8}
	advancedSync := firstSync.Add(2 * time.Hour)
	if err := store.Commit(context.Background(), machineID, snapshot, []adapters.SourceReport{advanced}, nil, advancedSync); err != nil {
		t.Fatal(err)
	}
	var offset, line int64
	if err := store.db.QueryRow(`SELECT last_successful_sync, cursor_offset, cursor_line FROM source_state WHERE machine_id = ? AND adapter = ? AND source_path = ?`, machineID, report.Adapter, report.Path).Scan(&syncedAt, &offset, &line); err != nil {
		t.Fatal(err)
	}
	if syncedAt != advancedSync.UTC().Format(time.RFC3339Nano) || offset != 100 || line != 8 {
		t.Fatalf("changed cursor was not updated: sync = %q, offset = %d, line = %d", syncedAt, offset, line)
	}
}

func TestSnapshotDoesNotLoadHistoricalEventFingerprints(t *testing.T) {
	store, err := OpenState(filepath.Join(t.TempDir(), "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if _, err := store.db.Exec(`INSERT INTO event_state (machine_id, event_id, fingerprint, last_successful_sync) VALUES (?, ?, ?, ?)`, "machine-a", "historical", []byte("not a full fingerprint"), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Snapshot(context.Background(), "machine-a"); err != nil {
		t.Fatalf("snapshot scanned historical event fingerprints: %v", err)
	}
}

func stateEvent(id string, total int64) usage.Event {
	return usage.Event{
		SchemaVersion: usage.SchemaVersion,
		EventID:       id,
		Timestamp:     time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC),
		MachineID:     "machine-a",
		Provider:      "example",
		Model:         "example-model",
		Tool:          "example-tool",
		TotalTokens:   usage.Int64(total),
		Currency:      "USD",
		TokenAccuracy: usage.AccuracyReported,
		Source:        usage.Source{Adapter: "generic-jsonl", AdapterVersion: "test"},
	}
}

// Agents upgraded from an older release carry a source_state table without
// the cursor_state column; OpenState must migrate it in place so incremental
// resume state can be stored.
func TestOpenStateMigratesLegacySchema(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.db")
	legacy, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`
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
);`); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`INSERT INTO source_state (machine_id, adapter, source_path, source_identity, cursor_identity, cursor_offset, cursor_line, last_successful_sync) VALUES ('machine-a', 'codex', '/private/session.jsonl', 'identity', 'cursor-identity', 42, 3, '2026-11-05T00:00:00Z')`); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := OpenState(path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// The legacy row survives with an empty resume state.
	snapshot, err := store.Snapshot(context.Background(), "machine-a")
	if err != nil {
		t.Fatal(err)
	}
	cursor, ok := snapshot.Cursors[SourceKey("codex", "/private/session.jsonl")]
	if !ok || cursor.Offset != 42 || cursor.Line != 3 || cursor.State != "" {
		t.Fatalf("legacy cursor = %+v, present=%t", cursor, ok)
	}

	// A state-bearing cursor round-trips through the migrated schema.
	committed := adapters.Cursor{Identity: "cursor-identity", Offset: 100, Line: 8, State: `{"version":1}`}
	report := adapters.SourceReport{Adapter: "codex", Path: "/private/session.jsonl", Source: adapters.Source{Path: "/private/session.jsonl", Identity: "identity"}, Cursor: committed}
	if err := store.Commit(context.Background(), "machine-a", snapshot, []adapters.SourceReport{report}, nil, time.Now()); err != nil {
		t.Fatal(err)
	}
	snapshot, err = store.Snapshot(context.Background(), "machine-a")
	if err != nil {
		t.Fatal(err)
	}
	if got := snapshot.Cursors[SourceKey("codex", "/private/session.jsonl")]; got != committed {
		t.Fatalf("migrated schema lost cursor state: %+v != %+v", got, committed)
	}
}
