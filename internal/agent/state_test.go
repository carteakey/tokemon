package agent

import (
	"context"
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
	if err := store.Commit(context.Background(), machineID, []adapters.SourceReport{report}, []usage.Event{event}, time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)); err != nil {
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
	cursor, ok := snapshot.Cursors[sourceKey(report.Adapter, report.Path)]
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
	if err := store.Commit(context.Background(), machineID, []adapters.SourceReport{report}, []usage.Event{event}, time.Now()); err != nil {
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
	if err := store.Commit(context.Background(), "machine-a", []adapters.SourceReport{report}, nil, time.Now()); err != nil {
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
