package database

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"
)

const (
	backupFilePrefix  = "-backup-"
	restoreFilePrefix = "-before-restore-"
)

// BackupOptions controls an online, WAL-aware SQLite backup. DestinationDir
// should be a mounted off-host destination (or another directory managed by
// the operator's replication tooling). A backup is written as a new,
// versioned file and older files with the same prefix are pruned.
type BackupOptions struct {
	DatabasePath   string
	DestinationDir string
	Retention      int
	Now            time.Time
}

// BackupResult is the integrity/equivalence record emitted after a backup is
// written and reopened successfully.
type BackupResult struct {
	Path           string `json:"path"`
	Timestamp      string `json:"timestamp"`
	SchemaVersion  int    `json:"schema_version"`
	EventCount     int64  `json:"event_count"`
	LifetimeTokens int64  `json:"lifetime_tokens"`
	SHA256         string `json:"sha256"`
}

// RestoreOptions controls replacing a database from a verified SQLite
// snapshot. Existing destinations require Force so an accidental restore
// cannot silently overwrite data. A pre-restore snapshot is retained beside
// CAR-79 migration snapshots for rollback.
type RestoreOptions struct {
	DatabasePath string
	BackupPath   string
	Force        bool
	Now          time.Time
}

// RestoreResult records the verified source and restored database state.
type RestoreResult struct {
	SourcePath       string `json:"source_path"`
	DestinationPath  string `json:"destination_path"`
	PreRestoreBackup string `json:"pre_restore_backup,omitempty"`
	Timestamp        string `json:"timestamp"`
	SchemaVersion    int    `json:"schema_version"`
	EventCount       int64  `json:"event_count"`
	LifetimeTokens   int64  `json:"lifetime_tokens"`
	SHA256           string `json:"sha256"`
}

// VerifyResult is the integrity and equivalence summary for a SQLite file.
type VerifyResult struct {
	Path           string `json:"path"`
	SchemaVersion  int    `json:"schema_version"`
	EventCount     int64  `json:"event_count"`
	LifetimeTokens int64  `json:"lifetime_tokens"`
	SHA256         string `json:"sha256"`
}

// CreateBackup creates a standalone SQLite snapshot using VACUUM INTO. This
// is safe while Tokemon is serving: SQLite reads a transactionally consistent
// snapshot that includes committed rows still held in the source WAL. It does
// not copy the database, -wal, or -shm sidecars.
func CreateBackup(ctx context.Context, options BackupOptions) (BackupResult, error) {
	path := strings.TrimSpace(options.DatabasePath)
	destination := strings.TrimSpace(options.DestinationDir)
	if path == "" {
		return BackupResult{}, errors.New("database path is required")
	}
	if destination == "" {
		return BackupResult{}, errors.New("backup destination directory is required")
	}
	retention := options.Retention
	if retention == 0 {
		retention = backupRetention
	}
	if retention < 1 {
		return BackupResult{}, errors.New("backup retention must be at least 1")
	}
	if !isFileBackedDatabase(path) {
		return BackupResult{}, errors.New("backup requires a file-backed database")
	}
	info, err := os.Stat(path)
	if err != nil {
		return BackupResult{}, fmt.Errorf("inspect database: %w", err)
	}
	if info.IsDir() {
		return BackupResult{}, fmt.Errorf("database path %q is a directory", path)
	}
	if err := os.MkdirAll(destination, 0o700); err != nil {
		return BackupResult{}, fmt.Errorf("create backup destination: %w", err)
	}

	db, err := openMaintenanceDatabase(path)
	if err != nil {
		return BackupResult{}, err
	}
	defer db.Close()

	now := options.Now
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	result, err := backupDatabaseAt(ctx, db, path, destination, retention, now)
	if err != nil {
		return BackupResult{}, err
	}
	return result, nil
}

// VerifyBackup checks a SQLite file without applying migrations. It is useful
// for scheduled jobs and restore runbooks because it reports corruption before
// a file is copied or made live.
func VerifyBackup(ctx context.Context, path string) (VerifyResult, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return VerifyResult{}, errors.New("backup path is required")
	}
	if !isFileBackedDatabase(path) {
		return VerifyResult{}, errors.New("backup verification requires a file-backed database")
	}
	if info, err := os.Stat(path); err != nil {
		return VerifyResult{}, fmt.Errorf("inspect backup: %w", err)
	} else if info.IsDir() {
		return VerifyResult{}, fmt.Errorf("backup path %q is a directory", path)
	}
	db, err := openMaintenanceDatabase(path)
	if err != nil {
		return VerifyResult{}, err
	}
	defer db.Close()
	return verifyDatabase(ctx, db, path)
}

// Restore replaces a destination with a verified standalone SQLite snapshot.
// The Tokemon process lock is acquired for the destination, so a running
// server or mutating CLI deterministically rejects the restore. The source is
// read through SQLite rather than copied byte-for-byte, which safely handles a
// source WAL and produces a clean destination without sidecars.
func Restore(ctx context.Context, options RestoreOptions) (RestoreResult, error) {
	databasePath := strings.TrimSpace(options.DatabasePath)
	backupPath := strings.TrimSpace(options.BackupPath)
	if databasePath == "" {
		return RestoreResult{}, errors.New("database path is required")
	}
	if backupPath == "" {
		return RestoreResult{}, errors.New("backup path is required")
	}
	if !isFileBackedDatabase(databasePath) || !isFileBackedDatabase(backupPath) {
		return RestoreResult{}, errors.New("restore requires file-backed database paths")
	}
	databasePath, err := filepath.Abs(databasePath)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("resolve database path: %w", err)
	}
	backupPath, err = filepath.Abs(backupPath)
	if err != nil {
		return RestoreResult{}, fmt.Errorf("resolve backup path: %w", err)
	}
	if databasePath == backupPath {
		return RestoreResult{}, errors.New("database and backup paths must differ")
	}
	if info, err := os.Stat(backupPath); err != nil {
		return RestoreResult{}, fmt.Errorf("inspect backup: %w", err)
	} else if info.IsDir() {
		return RestoreResult{}, fmt.Errorf("backup path %q is a directory", backupPath)
	}

	if err := os.MkdirAll(filepath.Dir(databasePath), 0o755); err != nil {
		return RestoreResult{}, fmt.Errorf("create database directory: %w", err)
	}
	lock, err := acquireDatabaseLock(databasePath)
	if err != nil {
		return RestoreResult{}, err
	}
	defer lock.Close()

	sourceDB, err := openMaintenanceDatabase(backupPath)
	if err != nil {
		return RestoreResult{}, err
	}
	now := options.Now
	if now.IsZero() {
		now = time.Now().UTC()
	} else {
		now = now.UTC()
	}
	sourceStats, err := verifyDatabase(ctx, sourceDB, backupPath)
	if err != nil {
		sourceDB.Close()
		return RestoreResult{}, fmt.Errorf("verify backup: %w", err)
	}
	tempPath, err := temporaryRestorePath(filepath.Dir(databasePath), filepath.Base(databasePath), now)
	if err != nil {
		sourceDB.Close()
		return RestoreResult{}, err
	}
	if err := vacuumInto(ctx, sourceDB, tempPath); err != nil {
		sourceDB.Close()
		_ = os.Remove(tempPath)
		return RestoreResult{}, fmt.Errorf("stage restored database: %w", err)
	}
	if err := sourceDB.Close(); err != nil {
		_ = os.Remove(tempPath)
		return RestoreResult{}, fmt.Errorf("close backup: %w", err)
	}
	staged, err := VerifyBackup(ctx, tempPath)
	if err != nil {
		_ = os.Remove(tempPath)
		return RestoreResult{}, fmt.Errorf("verify staged restore: %w", err)
	}
	if staged.EventCount != sourceStats.EventCount || staged.LifetimeTokens != sourceStats.LifetimeTokens {
		_ = os.Remove(tempPath)
		return RestoreResult{}, fmt.Errorf("staged restore differs from backup: events %d/%d, tokens %d/%d", staged.EventCount, sourceStats.EventCount, staged.LifetimeTokens, sourceStats.LifetimeTokens)
	}

	var preRestore string
	if _, err := os.Stat(databasePath); err == nil {
		if !options.Force {
			_ = os.Remove(tempPath)
			return RestoreResult{}, fmt.Errorf("destination %q already exists; pass --force to replace it", databasePath)
		}
		preRestore, err = snapshotExistingDatabase(ctx, databasePath, now)
		if err != nil {
			_ = os.Remove(tempPath)
			return RestoreResult{}, err
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		_ = os.Remove(tempPath)
		return RestoreResult{}, fmt.Errorf("inspect destination: %w", err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.Remove(databasePath + suffix); err != nil && !errors.Is(err, os.ErrNotExist) {
			_ = os.Remove(tempPath)
			return RestoreResult{}, fmt.Errorf("remove stale SQLite %s sidecar: %w", suffix, err)
		}
	}
	if err := os.Rename(tempPath, databasePath); err != nil {
		_ = os.Remove(tempPath)
		return RestoreResult{}, fmt.Errorf("install restored database: %w", err)
	}
	verified, err := VerifyBackup(ctx, databasePath)
	if err != nil || verified.EventCount != sourceStats.EventCount || verified.LifetimeTokens != sourceStats.LifetimeTokens {
		if err == nil {
			err = fmt.Errorf("restored database differs from backup: events %d/%d, tokens %d/%d", verified.EventCount, sourceStats.EventCount, verified.LifetimeTokens, sourceStats.LifetimeTokens)
		}
		if preRestore != "" {
			_ = os.Remove(databasePath)
			_ = os.Remove(databasePath + "-wal")
			_ = os.Remove(databasePath + "-shm")
			_ = copyFile(preRestore, databasePath)
		}
		return RestoreResult{}, fmt.Errorf("verify restored database: %w", err)
	}

	return RestoreResult{
		SourcePath:       backupPath,
		DestinationPath:  databasePath,
		PreRestoreBackup: preRestore,
		Timestamp:        now.Format(time.RFC3339Nano),
		SchemaVersion:    verified.SchemaVersion,
		EventCount:       verified.EventCount,
		LifetimeTokens:   verified.LifetimeTokens,
		SHA256:           verified.SHA256,
	}, nil
}

func backupDatabaseAt(ctx context.Context, db *sql.DB, databasePath, destination string, retention int, now time.Time) (BackupResult, error) {
	stats, err := verifyDatabase(ctx, db, databasePath)
	if err != nil {
		return BackupResult{}, fmt.Errorf("verify database before backup: %w", err)
	}
	base := strings.TrimSuffix(filepath.Base(databasePath), filepath.Ext(databasePath))
	prefix := base + backupFilePrefix
	backupPath, err := uniqueBackupPath(destination, prefix, stats.SchemaVersion, now)
	if err != nil {
		return BackupResult{}, err
	}
	if err := vacuumInto(ctx, db, backupPath); err != nil {
		_ = os.Remove(backupPath)
		return BackupResult{}, fmt.Errorf("write SQLite backup: %w", err)
	}
	verified, err := VerifyBackup(ctx, backupPath)
	if err != nil {
		_ = os.Remove(backupPath)
		return BackupResult{}, fmt.Errorf("verify SQLite backup: %w", err)
	}
	if verified.EventCount != stats.EventCount || verified.LifetimeTokens != stats.LifetimeTokens {
		_ = os.Remove(backupPath)
		return BackupResult{}, fmt.Errorf("backup differs from source: events %d/%d, tokens %d/%d", verified.EventCount, stats.EventCount, verified.LifetimeTokens, stats.LifetimeTokens)
	}
	if err := pruneBackups(destination, prefix, retention); err != nil {
		return BackupResult{}, fmt.Errorf("prune scheduled backups: %w", err)
	}
	return BackupResult{
		Path:           backupPath,
		Timestamp:      now.Format(time.RFC3339Nano),
		SchemaVersion:  verified.SchemaVersion,
		EventCount:     verified.EventCount,
		LifetimeTokens: verified.LifetimeTokens,
		SHA256:         verified.SHA256,
	}, nil
}

func verifyDatabase(ctx context.Context, db *sql.DB, path string) (VerifyResult, error) {
	var integrity string
	if err := db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&integrity); err != nil {
		return VerifyResult{}, fmt.Errorf("check database integrity: %w", err)
	}
	if !strings.EqualFold(integrity, "ok") {
		return VerifyResult{}, fmt.Errorf("database integrity check failed: %s", integrity)
	}
	version, err := schemaVersion(ctx, db)
	if err != nil {
		return VerifyResult{}, err
	}
	var events, tokens int64
	if exists, err := hasTable(ctx, db, "usage_events"); err != nil {
		return VerifyResult{}, err
	} else if exists {
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(COALESCE(total_tokens, 0)), 0) FROM usage_events`).Scan(&events, &tokens); err != nil {
			return VerifyResult{}, fmt.Errorf("read usage totals: %w", err)
		}
	}
	digest, err := fileSHA256(path)
	if err != nil {
		return VerifyResult{}, err
	}
	return VerifyResult{Path: path, SchemaVersion: version, EventCount: events, LifetimeTokens: tokens, SHA256: digest}, nil
}

func openMaintenanceDatabase(path string) (*sql.DB, error) {
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open SQLite database: %w", err)
	}
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(`PRAGMA busy_timeout = 5000`); err != nil {
		db.Close()
		return nil, fmt.Errorf("configure SQLite busy timeout: %w", err)
	}
	return db, nil
}

func vacuumInto(ctx context.Context, db *sql.DB, destination string) error {
	if _, err := db.ExecContext(ctx, `VACUUM INTO ?`, destination); err != nil {
		return err
	}
	return nil
}

func uniqueBackupPath(destination, prefix string, schema int, now time.Time) (string, error) {
	stamp := now.UTC().Format("20060102T150405.000000000Z")
	for sequence := 0; sequence < 1000; sequence++ {
		name := fmt.Sprintf("%sv%d-%s.db", prefix, schema, stamp)
		if sequence > 0 {
			name = fmt.Sprintf("%sv%d-%s-%d.db", prefix, schema, stamp, sequence)
		}
		path := filepath.Join(destination, name)
		if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
			return path, nil
		} else if err != nil {
			return "", fmt.Errorf("inspect backup destination: %w", err)
		}
	}
	return "", errors.New("could not allocate a unique backup filename")
}

func temporaryRestorePath(directory, base string, now time.Time) (string, error) {
	name := fmt.Sprintf(".%s.restore-%s-", base, now.UTC().Format("20060102T150405.000000000Z"))
	file, err := os.CreateTemp(directory, name)
	if err != nil {
		return "", fmt.Errorf("create restore staging file: %w", err)
	}
	path := file.Name()
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("close restore staging file: %w", err)
	}
	if err := os.Remove(path); err != nil {
		return "", fmt.Errorf("prepare restore staging path: %w", err)
	}
	return path, nil
}

func snapshotExistingDatabase(ctx context.Context, path string, now time.Time) (string, error) {
	db, err := openMaintenanceDatabase(path)
	if err != nil {
		return "", err
	}
	defer db.Close()
	if _, err := verifyDatabase(ctx, db, path); err != nil {
		return "", fmt.Errorf("verify destination before restore: %w", err)
	}
	directory := filepath.Join(filepath.Dir(path), "backups")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", fmt.Errorf("create restore backup directory: %w", err)
	}
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	prefix := base + restoreFilePrefix
	backupPath, err := uniqueBackupPath(directory, prefix, databaseSchemaVersion, now)
	if err != nil {
		return "", err
	}
	if err := vacuumInto(ctx, db, backupPath); err != nil {
		_ = os.Remove(backupPath)
		return "", fmt.Errorf("snapshot destination before restore: %w", err)
	}
	if _, err := VerifyBackup(ctx, backupPath); err != nil {
		_ = os.Remove(backupPath)
		return "", fmt.Errorf("verify pre-restore snapshot: %w", err)
	}
	if err := pruneBackups(directory, prefix, backupRetention); err != nil {
		return "", fmt.Errorf("prune pre-restore snapshots: %w", err)
	}
	return backupPath, nil
}

func copyFile(source, destination string) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := io.Copy(output, input); err != nil {
		_ = output.Close()
		return err
	}
	return output.Close()
}

func fileSHA256(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", fmt.Errorf("hash %q: %w", path, err)
	}
	defer file.Close()
	hash := sha256.New()
	if _, err := io.Copy(hash, file); err != nil {
		return "", fmt.Errorf("hash %q: %w", path, err)
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func hasTable(ctx context.Context, db *sql.DB, table string) (bool, error) {
	var exists bool
	if err := db.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM sqlite_master WHERE type = 'table' AND name = ?)`, table).Scan(&exists); err != nil {
		return false, fmt.Errorf("check table %q: %w", table, err)
	}
	return exists, nil
}

func isFileBackedDatabase(path string) bool {
	return !strings.HasPrefix(path, ":") && !strings.HasPrefix(path, "file:")
}
