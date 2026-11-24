package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
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
	files, err := filepath.Glob(filepath.Join(destination, fmt.Sprintf("tokemon-backup-v%d-*.db", databaseSchemaVersion)))
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
	// before mutating it, then preserves the event through the current schema version.
	store, err := Open(destinationPath, catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	events, err := store.Events(context.Background())
	if err != nil || len(events) != 1 || events[0].EventID != "legacy-event" {
		t.Fatalf("migrated events=%+v err=%v", events, err)
	}
	migrationBackups, err := filepath.Glob(filepath.Join(filepath.Dir(destinationPath), "backups", fmt.Sprintf("tokemon-v3-before-v%d-*.db", databaseSchemaVersion)))
	if err != nil || len(migrationBackups) != 1 {
		t.Fatalf("migration backups=%v err=%v", migrationBackups, err)
	}
}

func TestRestoreRemovesInvalidNewDestinationOnPostInstallVerificationFailure(t *testing.T) {
	directory := t.TempDir()
	sourcePath := filepath.Join(directory, "source.db")
	store, err := Open(sourcePath, catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Ingest(context.Background(), []usage.Event{backupTestEvent("restore-cleanup", time.Now().UTC(), 42)}); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	backup, err := CreateBackup(context.Background(), BackupOptions{
		DatabasePath: sourcePath, DestinationDir: filepath.Join(directory, "off-host"),
	})
	if err != nil {
		t.Fatal(err)
	}

	var calls int
	verify := func(ctx context.Context, path string) (VerifyResult, error) {
		calls++
		result, verifyErr := VerifyBackup(ctx, path)
		if verifyErr != nil {
			return VerifyResult{}, verifyErr
		}
		if calls == 2 {
			return VerifyResult{}, errors.New("simulated post-install verification failure")
		}
		return result, nil
	}
	destination := filepath.Join(directory, "new", "tokemon.db")
	_, err = restoreWithVerifier(context.Background(), RestoreOptions{DatabasePath: destination, BackupPath: backup.Path}, verify)
	if err == nil || !strings.Contains(err.Error(), "verify restored database") {
		t.Fatalf("post-install restore error = %v", err)
	}
	for _, path := range []string{destination, destination + "-wal", destination + "-shm"} {
		if _, statErr := os.Stat(path); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatalf("post-install failure left %s: %v", path, statErr)
		}
	}
}

func TestRepresentativeScaleBackupRestore(t *testing.T) {
	directory := t.TempDir()
	sourcePath := filepath.Join(directory, "large.db")
	store, err := Open(sourcePath, catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`PRAGMA wal_autocheckpoint = 1000000`); err != nil {
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
	if info, err := os.Stat(sourcePath + "-wal"); err != nil || info.Size() == 0 {
		t.Fatalf("expected representative fixture WAL, info=%v, err=%v", info, err)
	}
	backup, err := CreateBackup(context.Background(), BackupOptions{
		DatabasePath: sourcePath, DestinationDir: filepath.Join(directory, "off-host"), Retention: 2,
		Now: time.Date(2026, 8, 10, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
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

func TestLatestBackupInfo(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "tokemon.db")
	missingDir := filepath.Join(dir, "missing-backups")

	// Missing directory returns Exists: false, Count: 0, nil error.
	info, err := latestBackupInfoAt(dbPath, missingDir, time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC))
	if err != nil || info.Exists || info.Count != 0 {
		t.Fatalf("missing dir = %+v, err=%v", info, err)
	}

	backupDir := filepath.Join(dir, "backups")
	if err := os.MkdirAll(backupDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// Non-scheduled artifacts (-wal, -shm, migration snapshots, pre-restore, unrelated) are ignored.
	ignored := []string{
		"tokemon-v3-before-v4-20260810T110000.000000000Z.db",
		"tokemon-before-restore-v4-20260810T110000.000000000Z.db",
		"tokemon-backup-v4-20260810T110000.000000000Z.db-wal",
		"tokemon-backup-v4-20260810T110000.000000000Z.db-shm",
		"other-backup-v4-20260810T110000.000000000Z.db",
		"tokemon-backup-v0-20260810T110000.000000000Z.db",
	}
	for _, name := range ignored {
		if err := os.WriteFile(filepath.Join(backupDir, name), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	info, err = latestBackupInfoAt(dbPath, "", time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC))
	if err != nil || info.Exists || info.Count != 0 {
		t.Fatalf("only ignored files = %+v, err=%v", info, err)
	}

	// Scheduled backups with UTC filename timestamps (and collision suffix) override OS ModTime.
	older := filepath.Join(backupDir, "tokemon-backup-v4-20260810T010000.000000000Z.db")
	newer := filepath.Join(backupDir, "tokemon-backup-v4-20260810T100000.000000000Z.db")
	newerSeq := filepath.Join(backupDir, "tokemon-backup-v4-20260810T100000.000000000Z-1.db")
	for _, p := range []string{older, newer, newerSeq} {
		if err := os.WriteFile(p, []byte("db"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	// Make older file have a newer OS ModTime to verify filename UTC timestamp takes precedence.
	_ = os.Chtimes(older, time.Date(2026, 8, 10, 11, 59, 0, 0, time.UTC), time.Date(2026, 8, 10, 11, 59, 0, 0, time.UTC))

	now := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	info, err = latestBackupInfoAt(dbPath, "", now)
	if err != nil {
		t.Fatal(err)
	}
	if !info.Exists || info.Count != 3 || info.Path != newerSeq || info.Age != 2*time.Hour {
		t.Fatalf("latest backup = %+v, want count=3 path=%s age=2h", info, newerSeq)
	}

	// ModTime fallback when filename lacks a valid timestamp.
	manualDir := filepath.Join(dir, "manual-backups")
	if err := os.MkdirAll(manualDir, 0o755); err != nil {
		t.Fatal(err)
	}
	manual := filepath.Join(manualDir, "tokemon-backup-v4-manual.db")
	if err := os.WriteFile(manual, []byte("db"), 0o600); err != nil {
		t.Fatal(err)
	}
	modTime := time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC)
	_ = os.Chtimes(manual, modTime, modTime)
	manualInfo, err := latestBackupInfoAt(dbPath, manualDir, now)
	if err != nil || !manualInfo.Exists || manualInfo.Count != 1 || manualInfo.Age != 3*time.Hour {
		t.Fatalf("manual ModTime fallback = %+v, err=%v", manualInfo, err)
	}

	// Backup path being a regular file surfaces an error.
	notDir := filepath.Join(dir, "file-not-dir")
	if err := os.WriteFile(notDir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LatestBackupInfo(dbPath, notDir); err == nil {
		t.Fatal("expected error when backupDir is a file")
	}
}

func TestCreateBackupStagesAtomicallyBeforeVerify(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "tokemon.db")
	store, err := Open(dbPath, catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	const eventCount = 2000
	events := make([]usage.Event, 0, eventCount)
	base := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	for i := 0; i < eventCount; i++ {
		events = append(events, backupTestEvent(fmt.Sprintf("atomic-%04d", i), base.Add(time.Duration(i)*time.Second), 10))
	}
	if _, err := store.Ingest(context.Background(), events); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	destDir := filepath.Join(dir, "backups")
	stopPoll := make(chan struct{})
	pollErr := make(chan error, 1)
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stopPoll:
				return
			default:
			}
			info, err := LatestBackupInfo(dbPath, destDir)
			if err != nil {
				select {
				case pollErr <- fmt.Errorf("LatestBackupInfo error: %w", err):
				default:
				}
				return
			}
			if info.Exists {
				verified, err := VerifyBackup(context.Background(), info.Path)
				if err != nil {
					select {
					case pollErr <- fmt.Errorf("visible backup %q failed VerifyBackup: %w", info.Path, err):
					default:
					}
					return
				}
				if verified.EventCount != eventCount || verified.LifetimeTokens != eventCount*10 {
					select {
					case pollErr <- fmt.Errorf("visible backup %q partial counts: %+v", info.Path, verified):
					default:
					}
					return
				}
			}
		}
	}()

	for i := 0; i < 3; i++ {
		res, err := CreateBackup(context.Background(), BackupOptions{
			DatabasePath:   dbPath,
			DestinationDir: destDir,
			Retention:      5,
			Now:            base.Add(time.Duration(i) * time.Minute),
		})
		if err != nil {
			close(stopPoll)
			wg.Wait()
			t.Fatalf("CreateBackup %d: %v", i, err)
		}
		if res.EventCount != eventCount || res.LifetimeTokens != eventCount*10 {
			close(stopPoll)
			wg.Wait()
			t.Fatalf("CreateBackup %d counts = %+v", i, res)
		}
	}
	close(stopPoll)
	wg.Wait()
	select {
	case err := <-pollErr:
		t.Fatal(err)
	default:
	}

	entries, err := os.ReadDir(destDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 3 {
		t.Fatalf("destDir entries = %d, want 3 (no leftover staging files)", len(entries))
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			t.Fatalf("leftover hidden staging file in destDir: %q", entry.Name())
		}
	}
}

func TestCreateBackupCancelledDuringStagingLeavesNoVisibleBackup(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "tokemon.db")
	store, err := Open(dbPath, catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	events := make([]usage.Event, 0, 1500)
	base := time.Date(2026, 8, 10, 12, 0, 0, 0, time.UTC)
	for i := 0; i < 1500; i++ {
		events = append(events, backupTestEvent(fmt.Sprintf("cancel-%04d", i), base.Add(time.Duration(i)*time.Second), 5))
	}
	if _, err := store.Ingest(context.Background(), events); err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	destDir := filepath.Join(dir, "backups")
	// Test both immediate pre-cancellation and mid-flight cancellation across short delays.
	for _, delay := range []time.Duration{0, 1 * time.Millisecond, 5 * time.Millisecond, 10 * time.Millisecond} {
		ctx, cancel := context.WithCancel(context.Background())
		if delay == 0 {
			cancel()
		} else {
			time.AfterFunc(delay, cancel)
		}
		_, err := CreateBackup(ctx, BackupOptions{
			DatabasePath:   dbPath,
			DestinationDir: destDir,
			Retention:      5,
			Now:            base.Add(delay),
		})
		cancel()
		if err != nil {
			info, infoErr := LatestBackupInfo(dbPath, destDir)
			if infoErr != nil || info.Exists || info.Count != 0 {
				t.Fatalf("cancelled CreateBackup (delay=%v) left visible backup: info=%+v err=%v", delay, info, infoErr)
			}
			entries, readErr := os.ReadDir(destDir)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if len(entries) != 0 {
				t.Fatalf("cancelled CreateBackup (delay=%v) left files in destDir: %v", delay, entries)
			}
			continue
		}
		// If backup completed before the timer fired, clean destDir and continue testing shorter delays.
		_ = os.RemoveAll(destDir)
	}

	// Specifically ensure an already-canceled context leaves zero files and returns error.
	canceledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := CreateBackup(canceledCtx, BackupOptions{
		DatabasePath:   dbPath,
		DestinationDir: destDir,
		Retention:      5,
		Now:            base.Add(time.Hour),
	}); err == nil {
		t.Fatal("expected error for already-canceled CreateBackup")
	}
	entries, err := os.ReadDir(destDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("expected 0 files in destDir after canceled CreateBackup, got %v", entries)
	}
}

func TestCreateBackupConcurrentWithContinuousIngest(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "tokemon.db")
	store, err := Open(dbPath, catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.db.Exec(`PRAGMA wal_autocheckpoint = 1000000`); err != nil {
		t.Fatal(err)
	}

	ctx := context.Background()
	const (
		seedCount      = 100
		seedTokensEach = int64(100)
		minIngests     = 100
		backupRuns     = 20
	)
	seedBase := time.Date(2026, 8, 10, 10, 0, 0, 0, time.UTC)
	seedEvents := make([]usage.Event, 0, seedCount)
	for i := 0; i < seedCount; i++ {
		seedEvents = append(seedEvents, backupTestEvent(fmt.Sprintf("seed-%04d", i), seedBase.Add(time.Duration(i)*time.Second), seedTokensEach))
	}
	if _, err := store.Ingest(ctx, seedEvents); err != nil {
		t.Fatal(err)
	}

	destDir := filepath.Join(dir, "backups")
	backupsDone := make(chan struct{})
	firstWaveReady := make(chan struct{})
	var readyOnce sync.Once
	var totalIngests atomic.Int64
	var ingestErrMu sync.Mutex
	var ingestErr error

	const workers = 2
	var wg sync.WaitGroup
	for w := 0; w < workers; w++ {
		workerID := w
		wg.Add(1)
		go func() {
			defer wg.Done()
			for seq := 0; seq < 2000; seq++ {
				select {
				case <-backupsDone:
					if totalIngests.Load() >= minIngests {
						return
					}
				default:
				}
				ev := backupTestEvent(
					fmt.Sprintf("live-w%d-%04d", workerID, seq),
					seedBase.Add(time.Hour+time.Duration(workerID*10000+seq)*time.Millisecond),
					150,
				)
				res, err := store.Ingest(ctx, []usage.Event{ev})
				if err != nil || res.Rejected != 0 || res.Accepted != 1 {
					ingestErrMu.Lock()
					if ingestErr == nil {
						ingestErr = fmt.Errorf("worker %d seq %d ingest failed: res=%+v err=%v", workerID, seq, res, err)
					}
					ingestErrMu.Unlock()
					return
				}
				if totalIngests.Add(1) >= 4 {
					readyOnce.Do(func() { close(firstWaveReady) })
				}
			}
		}()
	}

	<-firstWaveReady

	results := make([]BackupResult, 0, backupRuns)
	for i := 0; i < backupRuns; i++ {
		res, err := CreateBackup(ctx, BackupOptions{
			DatabasePath:   dbPath,
			DestinationDir: destDir,
			Retention:      backupRuns + 10,
			Now:            seedBase.Add(2*time.Hour + time.Duration(i)*time.Second),
		})
		if err != nil {
			close(backupsDone)
			wg.Wait()
			t.Fatalf("CreateBackup %d failed under concurrent ingest: %v", i, err)
		}
		results = append(results, res)
	}
	close(backupsDone)
	wg.Wait()

	ingestErrMu.Lock()
	err = ingestErr
	ingestErrMu.Unlock()
	if err != nil {
		t.Fatal(err)
	}

	gotIngests := totalIngests.Load()
	if gotIngests < minIngests {
		t.Fatalf("total concurrent ingests = %d, want >= %d", gotIngests, minIngests)
	}

	finalStats, err := queryUsageSnapshotStats(ctx, store.db)
	if err != nil {
		t.Fatal(err)
	}
	initialTokens := int64(seedCount) * seedTokensEach
	wantFinalEvents := int64(seedCount) + gotIngests
	wantFinalTokens := initialTokens + gotIngests*150
	if finalStats.EventCount != wantFinalEvents || finalStats.LifetimeTokens != wantFinalTokens {
		t.Fatalf("finalStats = %+v, want events=%d tokens=%d", finalStats, wantFinalEvents, wantFinalTokens)
	}

	for i, res := range results {
		verified, err := VerifyBackup(ctx, res.Path)
		if err != nil {
			t.Fatalf("VerifyBackup %d (%s): %v", i, res.Path, err)
		}
		if verified.SchemaVersion != res.SchemaVersion ||
			verified.EventCount != res.EventCount ||
			verified.LifetimeTokens != res.LifetimeTokens ||
			verified.SHA256 != res.SHA256 {
			t.Fatalf("backup %d mismatch between CreateBackup (%+v) and VerifyBackup (%+v)", i, res, verified)
		}
		if res.EventCount < seedCount || res.EventCount > wantFinalEvents {
			t.Fatalf("backup %d EventCount=%d out of range [%d, %d]", i, res.EventCount, seedCount, wantFinalEvents)
		}
		if res.LifetimeTokens < initialTokens || res.LifetimeTokens > wantFinalTokens {
			t.Fatalf("backup %d LifetimeTokens=%d out of range [%d, %d]", i, res.LifetimeTokens, initialTokens, wantFinalTokens)
		}
	}

	entries, err := os.ReadDir(destDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, entry := range entries {
		if strings.HasPrefix(entry.Name(), ".") {
			t.Fatalf("leftover hidden staging file in destDir: %q", entry.Name())
		}
	}
}

func TestUsageSnapshotMatchesAndRejectsDivergence(t *testing.T) {
	cases := []struct {
		name     string
		before   usageSnapshotStats
		after    usageSnapshotStats
		verified VerifyResult
		want     bool
	}{
		{
			name:     "exact match on idle source",
			before:   usageSnapshotStats{SchemaVersion: 5, EventCount: 160, LifetimeTokens: 24000},
			after:    usageSnapshotStats{SchemaVersion: 5, EventCount: 160, LifetimeTokens: 24000},
			verified: VerifyResult{SchemaVersion: 5, EventCount: 160, LifetimeTokens: 24000},
			want:     true,
		},
		{
			name:     "matches post-vacuum after concurrent ingest",
			before:   usageSnapshotStats{SchemaVersion: 5, EventCount: 160, LifetimeTokens: 24000},
			after:    usageSnapshotStats{SchemaVersion: 5, EventCount: 161, LifetimeTokens: 24150},
			verified: VerifyResult{SchemaVersion: 5, EventCount: 161, LifetimeTokens: 24150},
			want:     true,
		},
		{
			name:     "monotonically bracketed between continuous ingests",
			before:   usageSnapshotStats{SchemaVersion: 5, EventCount: 160, LifetimeTokens: 24000},
			after:    usageSnapshotStats{SchemaVersion: 5, EventCount: 163, LifetimeTokens: 24450},
			verified: VerifyResult{SchemaVersion: 5, EventCount: 161, LifetimeTokens: 24150},
			want:     true,
		},
		{
			name:     "monotonically bracketed with zero-token events",
			before:   usageSnapshotStats{SchemaVersion: 5, EventCount: 160, LifetimeTokens: 24000},
			after:    usageSnapshotStats{SchemaVersion: 5, EventCount: 162, LifetimeTokens: 24000},
			verified: VerifyResult{SchemaVersion: 5, EventCount: 161, LifetimeTokens: 24000},
			want:     true,
		},
		{
			name:     "rejects divergence on idle source",
			before:   usageSnapshotStats{SchemaVersion: 5, EventCount: 160, LifetimeTokens: 24000},
			after:    usageSnapshotStats{SchemaVersion: 5, EventCount: 160, LifetimeTokens: 24000},
			verified: VerifyResult{SchemaVersion: 5, EventCount: 159, LifetimeTokens: 23850},
			want:     false,
		},
		{
			name:     "rejects non-monotonic token regression",
			before:   usageSnapshotStats{SchemaVersion: 5, EventCount: 160, LifetimeTokens: 24000},
			after:    usageSnapshotStats{SchemaVersion: 5, EventCount: 163, LifetimeTokens: 24450},
			verified: VerifyResult{SchemaVersion: 5, EventCount: 161, LifetimeTokens: 23900},
			want:     false,
		},
		{
			name:     "rejects verified exceeding after bound",
			before:   usageSnapshotStats{SchemaVersion: 5, EventCount: 160, LifetimeTokens: 24000},
			after:    usageSnapshotStats{SchemaVersion: 5, EventCount: 163, LifetimeTokens: 24450},
			verified: VerifyResult{SchemaVersion: 5, EventCount: 164, LifetimeTokens: 24450},
			want:     false,
		},
		{
			name:     "rejects schema version mismatch",
			before:   usageSnapshotStats{SchemaVersion: 5, EventCount: 160, LifetimeTokens: 24000},
			after:    usageSnapshotStats{SchemaVersion: 5, EventCount: 161, LifetimeTokens: 24150},
			verified: VerifyResult{SchemaVersion: 4, EventCount: 161, LifetimeTokens: 24150},
			want:     false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := usageSnapshotMatches(tc.before, tc.after, tc.verified); got != tc.want {
				t.Fatalf("usageSnapshotMatches(%+v, %+v, %+v) = %v, want %v", tc.before, tc.after, tc.verified, got, tc.want)
			}
		})
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
