package database

import (
	"fmt"
	"os"
	"syscall"
)

// databaseLock is an advisory, process-wide lock for a file-backed Tokemon
// database. The lock file is intentionally retained after release: removing a
// lock path while another process still holds its descriptor would allow a
// second lock file to be created and bypass serialization.
type databaseLock struct {
	file *os.File
}

func acquireDatabaseLock(databasePath string) (*databaseLock, error) {
	lockPath := databasePath + ".lock"
	file, err := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open database lock %q: %w", lockPath, err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		return nil, fmt.Errorf("database %q is already in use; stop the server before running database maintenance: %w", databasePath, err)
	}
	if err := file.Truncate(0); err == nil {
		_, _ = fmt.Fprintf(file, "%d\n", os.Getpid())
	}
	return &databaseLock{file: file}, nil
}

func (l *databaseLock) Close() error {
	if l == nil || l.file == nil {
		return nil
	}
	file := l.file
	l.file = nil
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_UN); err != nil {
		_ = file.Close()
		return err
	}
	return file.Close()
}
