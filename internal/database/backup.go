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
	"strconv"
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

// BackupInfo describes the scheduled backups present for a database.
type BackupInfo struct {
	Exists    bool          `json:"exists"`
	Path      string        `json:"path,omitempty"`
	Timestamp time.Time     `json:"timestamp,omitempty"`
	Age       time.Duration `json:"age,omitempty"`
	Count     int           `json:"count"`
}

// LatestBackupInfo inspects backupDir (defaulting to <database-dir>/backups)
// for scheduled backups matching <base>-backup-v<schema>-*.db, returning the
// newest backup's path, timestamp, age, and total count.
func LatestBackupInfo(databasePath, backupDir string) (BackupInfo, error) {
	return latestBackupInfoAt(databasePath, backupDir, time.Now().UTC())
}

func latestBackupInfoAt(databasePath, backupDir string, now time.Time) (BackupInfo, error) {
	databasePath = strings.TrimSpace(databasePath)
	backupDir = strings.TrimSpace(backupDir)
	if backupDir == "" {
		if databasePath == "" || !isFileBackedDatabase(databasePath) {
			return BackupInfo{}, nil
		}
		backupDir = filepath.Join(filepath.Dir(databasePath), "backups")
	}
	base := "tokemon"
	if databasePath != "" && isFileBackedDatabase(databasePath) {
		if stem := strings.TrimSuffix(filepath.Base(databasePath), filepath.Ext(databasePath)); stem != "" {
			base = stem
		}
	}
	prefix := base + backupFilePrefix + "v"

	entries, err := os.ReadDir(backupDir)
	if errors.Is(err, os.ErrNotExist) {
		return BackupInfo{}, nil
	}
	if err != nil {
		return BackupInfo{}, fmt.Errorf("read backup directory: %w", err)
	}

	var (
		count    int
		hasBest  bool
		bestName string
		bestTS   time.Time
		bestSeq  int
	)
	for _, entry := range entries {
		if entry.IsDir() {
			continue
		}
		name := entry.Name()
		if !strings.HasPrefix(name, prefix) || !strings.HasSuffix(name, ".db") {
			continue
		}
		rest := strings.TrimSuffix(strings.TrimPrefix(name, prefix), ".db")
		versionStr, stampPart, ok := strings.Cut(rest, "-")
		if !ok || stampPart == "" {
			continue
		}
		if schemaVer, err := strconv.Atoi(versionStr); err != nil || schemaVer <= 0 {
			continue
		}
		ts, seq, ok := parseBackupFilenameTimestamp(stampPart)
		if !ok {
			info, infoErr := entry.Info()
			if errors.Is(infoErr, os.ErrNotExist) {
				continue
			}
			if infoErr != nil {
				return BackupInfo{}, fmt.Errorf("stat backup %q: %w", name, infoErr)
			}
			ts = info.ModTime().UTC()
			seq = -1
		}
		count++
		if !hasBest || ts.After(bestTS) || (ts.Equal(bestTS) && (seq > bestSeq || (seq == bestSeq && name > bestName))) {
			hasBest = true
			bestName = name
			bestTS = ts.UTC()
			bestSeq = seq
		}
	}
	if !hasBest {
		return BackupInfo{}, nil
	}
	age := now.UTC().Sub(bestTS)
	if age < 0 {
		age = 0
	}
	return BackupInfo{
		Exists:    true,
		Path:      filepath.Join(backupDir, bestName),
		Timestamp: bestTS,
		Age:       age,
		Count:     count,
	}, nil
}

func parseBackupFilenameTimestamp(stampPart string) (time.Time, int, bool) {
	zIdx := strings.IndexByte(stampPart, 'Z')
	if zIdx <= 0 {
		return time.Time{}, 0, false
	}
	seq := 0
	if suffix := stampPart[zIdx+1:]; suffix != "" {
		if !strings.HasPrefix(suffix, "-") {
			return time.Time{}, 0, false
		}
		parsedSeq, err := strconv.Atoi(suffix[1:])
		if err != nil || parsedSeq <= 0 {
			return time.Time{}, 0, false
		}
		seq = parsedSeq
	}
	rawStamp := stampPart[:zIdx+1]
	for _, layout := range []string{"20060102T150405.000000000Z", "20060102T150405.999999999Z", "20060102T150405Z"} {
		if parsed, err := time.Parse(layout, rawStamp); err == nil {
			return parsed.UTC(), seq, true
		}
	}
	return time.Time{}, 0, false
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
	return restoreWithVerifier(ctx, options, VerifyBackup)
}

func restoreWithVerifier(ctx context.Context, options RestoreOptions, verify func(context.Context, string) (VerifyResult, error)) (RestoreResult, error) {
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
	staged, err := verify(ctx, tempPath)
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
	verified, err := verify(ctx, databasePath)
	if err != nil || verified.EventCount != sourceStats.EventCount || verified.LifetimeTokens != sourceStats.LifetimeTokens {
		if err == nil {
			err = fmt.Errorf("restored database differs from backup: events %d/%d, tokens %d/%d", verified.EventCount, sourceStats.EventCount, verified.LifetimeTokens, sourceStats.LifetimeTokens)
		}
		if preRestore != "" {
			_ = os.Remove(databasePath)
			_ = os.Remove(databasePath + "-wal")
			_ = os.Remove(databasePath + "-shm")
			_ = copyFile(preRestore, databasePath)
		} else {
			_ = os.Remove(databasePath)
			_ = os.Remove(databasePath + "-wal")
			_ = os.Remove(databasePath + "-shm")
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
	initial := usageSnapshotStats{
		SchemaVersion:  stats.SchemaVersion,
		EventCount:     stats.EventCount,
		LifetimeTokens: stats.LifetimeTokens,
	}
	tempPath, verified, err := vacuumVerifiedSnapshot(
		ctx, db, destination, base, "backup", initial, now,
		"write SQLite backup",
		"verify SQLite backup",
		"backup",
	)
	if err != nil {
		return BackupResult{}, err
	}
	backupPath, err := uniqueBackupPath(destination, prefix, verified.SchemaVersion, now)
	if err != nil {
		_ = os.Remove(tempPath)
		return BackupResult{}, err
	}
	if err := os.Rename(tempPath, backupPath); err != nil {
		_ = os.Remove(tempPath)
		return BackupResult{}, fmt.Errorf("install SQLite backup: %w", err)
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

type usageSnapshotStats struct {
	SchemaVersion  int
	EventCount     int64
	LifetimeTokens int64
}

func queryUsageSnapshotStats(ctx context.Context, db *sql.DB) (usageSnapshotStats, error) {
	version, err := schemaVersion(ctx, db)
	if err != nil {
		return usageSnapshotStats{}, err
	}
	var events, tokens int64
	if exists, err := hasTable(ctx, db, "usage_events"); err != nil {
		return usageSnapshotStats{}, err
	} else if exists {
		if err := db.QueryRowContext(ctx, `SELECT COUNT(*), COALESCE(SUM(COALESCE(total_tokens, 0)), 0) FROM usage_events`).Scan(&events, &tokens); err != nil {
			return usageSnapshotStats{}, fmt.Errorf("read usage totals: %w", err)
		}
	}
	return usageSnapshotStats{
		SchemaVersion:  version,
		EventCount:     events,
		LifetimeTokens: tokens,
	}, nil
}

func usageSnapshotMatches(before, after usageSnapshotStats, verified VerifyResult) bool {
	if verified.SchemaVersion == before.SchemaVersion &&
		verified.EventCount == before.EventCount &&
		verified.LifetimeTokens == before.LifetimeTokens {
		return true
	}
	if verified.SchemaVersion == after.SchemaVersion &&
		verified.EventCount == after.EventCount &&
		verified.LifetimeTokens == after.LifetimeTokens {
		return true
	}
	if verified.SchemaVersion == before.SchemaVersion &&
		verified.SchemaVersion == after.SchemaVersion &&
		before.EventCount <= verified.EventCount &&
		verified.EventCount <= after.EventCount &&
		before.LifetimeTokens <= verified.LifetimeTokens &&
		verified.LifetimeTokens <= after.LifetimeTokens {
		return true
	}
	return false
}

func vacuumVerifiedSnapshot(
	ctx context.Context,
	db *sql.DB,
	directory, base, kind string,
	initial usageSnapshotStats,
	now time.Time,
	stageAction, verifyAction, mismatchLabel string,
) (string, VerifyResult, error) {
	before := initial
	const maxAttempts = 3
	for attempt := 0; attempt < maxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return "", VerifyResult{}, fmt.Errorf("%s: %w", verifyAction, err)
		}
		tempPath, err := temporaryStagingPath(directory, base, kind, now)
		if err != nil {
			return "", VerifyResult{}, err
		}
		if err := vacuumInto(ctx, db, tempPath); err != nil {
			_ = os.Remove(tempPath)
			return "", VerifyResult{}, fmt.Errorf("%s: %w", stageAction, err)
		}
		verified, err := VerifyBackup(ctx, tempPath)
		if err != nil {
			_ = os.Remove(tempPath)
			return "", VerifyResult{}, fmt.Errorf("%s: %w", verifyAction, err)
		}
		if verified.SchemaVersion == before.SchemaVersion &&
			verified.EventCount == before.EventCount &&
			verified.LifetimeTokens == before.LifetimeTokens {
			if err := ctx.Err(); err != nil {
				_ = os.Remove(tempPath)
				return "", VerifyResult{}, fmt.Errorf("%s: %w", verifyAction, err)
			}
			return tempPath, verified, nil
		}
		after, err := queryUsageSnapshotStats(ctx, db)
		if err != nil {
			_ = os.Remove(tempPath)
			return "", VerifyResult{}, fmt.Errorf("%s: %w", verifyAction, err)
		}
		if usageSnapshotMatches(before, after, verified) {
			if err := ctx.Err(); err != nil {
				_ = os.Remove(tempPath)
				return "", VerifyResult{}, fmt.Errorf("%s: %w", verifyAction, err)
			}
			return tempPath, verified, nil
		}
		_ = os.Remove(tempPath)
		if attempt+1 == maxAttempts {
			return "", VerifyResult{}, fmt.Errorf("%s differs from source: events %d/%d, tokens %d/%d", mismatchLabel, verified.EventCount, after.EventCount, verified.LifetimeTokens, after.LifetimeTokens)
		}
		before = after
	}
	return "", VerifyResult{}, fmt.Errorf("%s differs from source", mismatchLabel)
}

func verifyDatabase(ctx context.Context, db *sql.DB, path string) (VerifyResult, error) {
	var integrity string
	if err := db.QueryRowContext(ctx, `PRAGMA integrity_check`).Scan(&integrity); err != nil {
		return VerifyResult{}, fmt.Errorf("check database integrity: %w", err)
	}
	if !strings.EqualFold(integrity, "ok") {
		return VerifyResult{}, fmt.Errorf("database integrity check failed: %s", integrity)
	}
	stats, err := queryUsageSnapshotStats(ctx, db)
	if err != nil {
		return VerifyResult{}, err
	}
	digest, err := fileSHA256(path)
	if err != nil {
		return VerifyResult{}, err
	}
	return VerifyResult{
		Path:           path,
		SchemaVersion:  stats.SchemaVersion,
		EventCount:     stats.EventCount,
		LifetimeTokens: stats.LifetimeTokens,
		SHA256:         digest,
	}, nil
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
	return temporaryStagingPath(directory, base, "restore", now)
}

func temporaryStagingPath(directory, base, kind string, now time.Time) (string, error) {
	name := fmt.Sprintf(".%s.%s-%s-", base, kind, now.UTC().Format("20060102T150405.000000000Z"))
	file, err := os.CreateTemp(directory, name)
	if err != nil {
		return "", fmt.Errorf("create %s staging file: %w", kind, err)
	}
	path := file.Name()
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("close %s staging file: %w", kind, err)
	}
	if err := os.Remove(path); err != nil {
		return "", fmt.Errorf("prepare %s staging path: %w", kind, err)
	}
	return path, nil
}

func snapshotExistingDatabase(ctx context.Context, path string, now time.Time) (string, error) {
	db, err := openMaintenanceDatabase(path)
	if err != nil {
		return "", err
	}
	defer db.Close()
	stats, err := verifyDatabase(ctx, db, path)
	if err != nil {
		return "", fmt.Errorf("verify destination before restore: %w", err)
	}
	directory := filepath.Join(filepath.Dir(path), "backups")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", fmt.Errorf("create restore backup directory: %w", err)
	}
	base := strings.TrimSuffix(filepath.Base(path), filepath.Ext(path))
	prefix := base + restoreFilePrefix
	initial := usageSnapshotStats{
		SchemaVersion:  stats.SchemaVersion,
		EventCount:     stats.EventCount,
		LifetimeTokens: stats.LifetimeTokens,
	}
	tempPath, verified, err := vacuumVerifiedSnapshot(
		ctx, db, directory, base, "snapshot", initial, now,
		"snapshot destination before restore",
		"verify pre-restore snapshot",
		"pre-restore snapshot",
	)
	if err != nil {
		return "", err
	}
	snapshotSchema := databaseSchemaVersion
	if verified.SchemaVersion > 0 {
		snapshotSchema = verified.SchemaVersion
	}
	backupPath, err := uniqueBackupPath(directory, prefix, snapshotSchema, now)
	if err != nil {
		_ = os.Remove(tempPath)
		return "", err
	}
	if err := os.Rename(tempPath, backupPath); err != nil {
		_ = os.Remove(tempPath)
		return "", fmt.Errorf("install pre-restore snapshot: %w", err)
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
