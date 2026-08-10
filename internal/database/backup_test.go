package database

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tokemon/tokemon/internal/catalog"
	"github.com/tokemon/tokemon/internal/evolution"
	"github.com/tokemon/tokemon/internal/usage"
)

func TestCreateBackupIncludesCommittedWALRows(t *testing.T) {
	directory := t.TempDir()
	databasePath := filepath.Join(directory, "tokemon.db")
	store, err := Open(databasePath, catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.db.Exec(`PRAGMA wal_autocheckpoint = 1000000`); err != nil {
		t.Fatal(err)
	}
	event := backupTestEvent("wal-event", time.Date(2026, 1, 1, 23, 59, 0, 0, time.UTC), 1_000_000)
	if _, err := store.Ingest(context.Background(), []usage.Event{event}); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(databasePath + "-wal"); err != nil || info.Size() == 0 {
		t.Fatalf("expected active WAL, info=%v, err=%v", info, err)
	}

	destination := filepath.Join(directory, "off-host")
	result, err := CreateBackup(context.Background(), BackupOptions{
		DatabasePath: databasePath, DestinationDir: destination, Retention: 2,
		Now: time.Date(2026, 8, 10, 0, 0, 0, 0, time.Local),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.EventCount != 1 || result.LifetimeTokens != 1_000_000 || !strings.HasSuffix(result.Path, "Z.db") {
		t.Fatalf("backup result = %+v", result)
	}
	verified, err := VerifyBackup(context.Background(), result.Path)
	if err != nil {
		t.Fatal(err)
	}
	if verified.EventCount != result.EventCount || verified.LifetimeTokens != result.LifetimeTokens {
		t.Fatalf("verified backup = %+v, result = %+v", verified, result)
	}
	if _, err := os.Stat(result.Path + "-wal"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("backup WAL sidecar exists: %v", err)
	}

	// A raw main-file copy is intentionally not an acceptable backup while the
	// source WAL is active: it misses the committed event.
	rawPath := filepath.Join(directory, "raw.db")
	mainBytes, err := os.ReadFile(databasePath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(rawPath, mainBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	raw, err := VerifyBackup(context.Background(), rawPath)
	if err != nil {
		t.Fatal(err)
	}
	if raw.EventCount == result.EventCount || raw.LifetimeTokens == result.LifetimeTokens {
		t.Fatalf("raw WAL-unsafe copy unexpectedly matched source: raw=%+v, backup=%+v", raw, result)
	}
}

func TestScheduledBackupRetentionUsesUTCAndPreservesMigrationSnapshots(t *testing.T) {
	directory := t.TempDir()
	databasePath := filepath.Join(directory, "tokemon.db")
	store, err := Open(databasePath, catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Ingest(context.Background(), []usage.Event{backupTestEvent("retention-event", time.Now().UTC(), 10)}); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(directory, "backups")
	for index := 0; index < 3; index++ {
		if _, err := CreateBackup(context.Background(), BackupOptions{
			DatabasePath: databasePath, DestinationDir: destination, Retention: 2,
			Now: time.Date(2026, 8, 10, 1, index, 0, 0, time.FixedZone("PDT", -7*60*60)),
		}); err != nil {
			t.Fatal(err)
		}
	}
	files, err := filepath.Glob(filepath.Join(destination, "tokemon-backup-v4-*.db"))
	if err != nil || len(files) != 2 {
		t.Fatalf("scheduled backups = %v, err = %v", files, err)
	}
	for _, path := range files {
		if !strings.Contains(filepath.Base(path), "Z.db") {
			t.Fatalf("backup filename is not UTC-marked: %q", path)
		}
	}
	// CAR-79's migration snapshots use a different prefix and are not touched
	// by scheduled-backup retention.
	migration := filepath.Join(destination, "tokemon-v0-before-v4-20260810T000000.000000000Z.db")
	if err := os.WriteFile(migration, []byte("migration snapshot"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateBackup(context.Background(), BackupOptions{
		DatabasePath: databasePath, DestinationDir: destination, Retention: 1,
		Now: time.Date(2026, 8, 10, 2, 0, 0, 0, time.UTC),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(migration); err != nil {
		t.Fatalf("migration snapshot was pruned: %v", err)
	}
}

func TestBackupFailureAndConcurrentProcessSignals(t *testing.T) {
	directory := t.TempDir()
	databasePath := filepath.Join(directory, "tokemon.db")
	store, err := Open(databasePath, catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	destinationFile := filepath.Join(directory, "destination-file")
	if err := os.WriteFile(destinationFile, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := CreateBackup(context.Background(), BackupOptions{
		DatabasePath: databasePath, DestinationDir: destinationFile,
		Retention: 2,
	}); err == nil {
		t.Fatal("backup to a file unexpectedly succeeded")
	}
	// A second Tokemon process cannot migrate or mutate the live database while
	// the server/store owns its advisory lock.
	if _, err := Open(databasePath, catalog.Empty()); err == nil || !strings.Contains(err.Error(), "already in use") {
		t.Fatalf("concurrent open error = %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if reopened, err := Open(databasePath, catalog.Empty()); err != nil {
		t.Fatal(err)
	} else if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRestorePurgesAtUTCBoundaryAndLowersEvolutionStage(t *testing.T) {
	directory := t.TempDir()
	sourcePath := filepath.Join(directory, "source.db")
	store, err := Open(sourcePath, catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	events := []usage.Event{
		backupTestEvent("before-boundary", time.Date(2026, 1, 1, 23, 59, 59, 0, time.UTC), 1_000_000),
		backupTestEvent("at-boundary", time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC), 10),
	}
	if _, err := store.Ingest(context.Background(), events); err != nil {
		t.Fatal(err)
	}
	overview, err := store.Overview(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if overview.Evolution.Stage != evolution.Stage(1_000_010) {
		t.Fatalf("source evolution = %+v", overview.Evolution)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	backup, err := CreateBackup(context.Background(), BackupOptions{
		DatabasePath: sourcePath, DestinationDir: filepath.Join(directory, "off-host"),
		Now: time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	destinationPath := filepath.Join(directory, "restored", "tokemon.db")
	result, err := Restore(context.Background(), RestoreOptions{
		DatabasePath: destinationPath, BackupPath: backup.Path,
		Now: time.Date(2026, 8, 10, 0, 1, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.EventCount != 2 || result.LifetimeTokens != 1_000_010 {
		t.Fatalf("restore result = %+v", result)
	}
	restored, err := Open(destinationPath, catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	deleted, err := restored.PurgeBefore(context.Background(), "2026-01-02")
	if err != nil || deleted != 1 {
		t.Fatalf("purge deleted=%d err=%v", deleted, err)
	}
	overview, err = restored.Overview(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if overview.LifetimeTokens != 10 || overview.Evolution.Stage != 1 {
		t.Fatalf("purged overview = %+v", overview)
	}
	if err := restored.Close(); err != nil {
		t.Fatal(err)
	}
	forced, err := Restore(context.Background(), RestoreOptions{
		DatabasePath: destinationPath, BackupPath: backup.Path, Force: true,
		Now: time.Date(2026, 8, 10, 0, 2, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if forced.PreRestoreBackup == "" {
		t.Fatalf("forced restore did not retain pre-restore snapshot: %+v", forced)
	}
	preRestore, err := VerifyBackup(context.Background(), forced.PreRestoreBackup)
	if err != nil {
		t.Fatal(err)
	}
	if preRestore.EventCount != 1 || preRestore.LifetimeTokens != 10 {
		t.Fatalf("pre-restore snapshot = %+v", preRestore)
	}
}

func TestRestoreOldSchemaMigratesWithRollbackSnapshot(t *testing.T) {
	directory := t.TempDir()
	sourcePath := filepath.Join(directory, "old.db")
	legacy, err := sql.Open("sqlite", sourcePath)
	if err != nil {
		t.Fatal(err)
	}
	legacySchema := `
CREATE TABLE machines (id TEXT PRIMARY KEY, name TEXT NOT NULL, first_seen_at TEXT NOT NULL, last_seen_at TEXT NOT NULL);
CREATE TABLE usage_events (
  event_id TEXT PRIMARY KEY, timestamp TEXT NOT NULL, machine_id TEXT NOT NULL,
  provider TEXT NOT NULL, raw_model TEXT NOT NULL, canonical_model TEXT NOT NULL DEFAULT '', tool TEXT NOT NULL,
  input_tokens INTEGER, output_tokens INTEGER, cache_read_tokens INTEGER, cache_write_tokens INTEGER,
  reasoning_tokens INTEGER, total_tokens INTEGER, cost REAL, cost_estimated INTEGER NOT NULL DEFAULT 0,
  currency TEXT NOT NULL DEFAULT 'USD', session_id TEXT, duration_ms INTEGER, token_accuracy TEXT NOT NULL,
  adapter TEXT NOT NULL, adapter_version TEXT NOT NULL
);
PRAGMA user_version = 3;`
	if _, err := legacy.Exec(legacySchema); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`INSERT INTO usage_events (event_id,timestamp,machine_id,provider,raw_model,canonical_model,tool,total_tokens,token_accuracy,adapter,adapter_version) VALUES ('legacy-event','2026-01-01T00:00:00Z','machine','openai','gpt','','codex',100,'reported','codex','0.3')`); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}

	backup, err := CreateBackup(context.Background(), BackupOptions{
		DatabasePath: sourcePath, DestinationDir: filepath.Join(directory, "off-host"),
		Now: time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	destinationPath := filepath.Join(directory, "restored", "tokemon.db")
	if _, err := Restore(context.Background(), RestoreOptions{DatabasePath: destinationPath, BackupPath: backup.Path}); err != nil {
		t.Fatal(err)
	}
	// Opening the old-schema restore runs the existing CAR-79 migration backup
	// before mutating it, then preserves the event through schema v4.
	store, err := Open(destinationPath, catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	events, err := store.Events(context.Background())
	if err != nil || len(events) != 1 || events[0].EventID != "legacy-event" {
		t.Fatalf("migrated events=%+v err=%v", events, err)
	}
	migrationBackups, err := filepath.Glob(filepath.Join(filepath.Dir(destinationPath), "backups", "tokemon-v3-before-v4-*.db"))
	if err != nil || len(migrationBackups) != 1 {
		t.Fatalf("migration backups=%v err=%v", migrationBackups, err)
	}
}

func TestRepresentativeScaleBackupRestore(t *testing.T) {
	directory := t.TempDir()
	sourcePath := filepath.Join(directory, "large.db")
	store, err := Open(sourcePath, catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	events := make([]usage.Event, 2500)
	for index := range events {
		events[index] = backupTestEvent(
			"representative-"+time.Date(2026, 1, 1, 0, 0, index, 0, time.UTC).Format("150405.000000000"),
			time.Date(2026, 1, 1, 0, 0, index, 0, time.UTC), 100,
		)
	}
	if _, err := store.Ingest(context.Background(), events); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	backup, err := CreateBackup(context.Background(), BackupOptions{
		DatabasePath: sourcePath, DestinationDir: filepath.Join(directory, "off-host"), Retention: 2,
		Now: time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := Restore(context.Background(), RestoreOptions{
		DatabasePath: filepath.Join(directory, "restored.db"), BackupPath: backup.Path,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.EventCount != int64(len(events)) || result.LifetimeTokens != int64(len(events))*100 {
		t.Fatalf("representative restore = %+v", result)
	}
}

func backupTestEvent(id string, timestamp time.Time, tokens int64) usage.Event {
	return usage.Event{
		SchemaVersion: usage.SchemaVersion, EventID: id, Timestamp: timestamp,
		MachineID: "backup-test-machine", Provider: "test", Model: "model", Tool: "generic-jsonl",
		TotalTokens: usage.Int64(tokens), TokenAccuracy: usage.AccuracyReported,
		Source: usage.Source{Adapter: "test", AdapterVersion: "1"},
	}
}
