package database

import (
	"errors"
	"fmt"
	"os"
	"sync"
	"syscall"
	"time"
)

const (
	// The marker is refreshed frequently enough for a crashed process to be
	// reclaimed without leaving maintenance permanently blocked. The marker is
	// in addition to flock because Docker/OrbStack bind mounts do not always
	// propagate advisory locks between the host and the Linux VM.
	databaseLockHeartbeat  = 5 * time.Second
	databaseLockStaleAfter = 2 * time.Minute
)

// databaseLock is a process-wide lock for a file-backed Tokemon database. The
// O_EXCL marker serializes host/container processes sharing a bind mount;
// syscall.Flock supplies kernel-level locking for native processes. A small
// heartbeat makes a marker left by a crashed process reclaimable.
type databaseLock struct {
	mu        sync.Mutex
	file      *os.File
	path      string
	stop      chan struct{}
	done      chan struct{}
	closeOnce sync.Once
	closeErr  error
}

func acquireDatabaseLock(databasePath string) (*databaseLock, error) {
	lockPath := databasePath + ".lock"
	var file *os.File
	for attempt := 0; attempt < 2; attempt++ {
		var err error
		file, err = os.OpenFile(lockPath, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
		if err == nil {
			break
		}
		if !errors.Is(err, os.ErrExist) {
			return nil, fmt.Errorf("open database lock %q: %w", lockPath, err)
		}
		stale, staleErr := staleDatabaseLock(lockPath)
		if staleErr != nil {
			return nil, fmt.Errorf("inspect database lock %q: %w", lockPath, staleErr)
		}
		if !stale {
			return nil, databaseInUseError(databasePath)
		}
		if err := os.Remove(lockPath); err != nil && !errors.Is(err, os.ErrNotExist) {
			return nil, databaseInUseError(databasePath)
		}
	}
	if file == nil {
		return nil, databaseInUseError(databasePath)
	}

	lock := &databaseLock{
		file: file,
		path: lockPath,
		stop: make(chan struct{}),
		done: make(chan struct{}),
	}
	if err := lock.refresh(); err != nil {
		_ = file.Close()
		_ = os.Remove(lockPath)
		return nil, fmt.Errorf("write database lock %q: %w", lockPath, err)
	}
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = file.Close()
		_ = os.Remove(lockPath)
		return nil, databaseInUseError(databasePath)
	}
	go lock.heartbeat()
	return lock, nil
}

func staleDatabaseLock(path string) (bool, error) {
	info, err := os.Stat(path)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return time.Since(info.ModTime()) > databaseLockStaleAfter, nil
}

func databaseInUseError(databasePath string) error {
	return fmt.Errorf("database %q is already in use; stop the server before running database maintenance", databasePath)
}

func (l *databaseLock) refresh() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.file == nil {
		return nil
	}
	if _, err := l.file.Seek(0, 0); err != nil {
		return err
	}
	if err := l.file.Truncate(0); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(l.file, "pid=%d\nstarted=%s\n", os.Getpid(), time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		return err
	}
	return l.file.Sync()
}

func (l *databaseLock) heartbeat() {
	ticker := time.NewTicker(databaseLockHeartbeat)
	defer ticker.Stop()
	defer close(l.done)
	for {
		select {
		case <-ticker.C:
			_ = l.refresh()
		case <-l.stop:
			return
		}
	}
}

func (l *databaseLock) Close() error {
	if l == nil {
		return nil
	}
	l.closeOnce.Do(func() {
		close(l.stop)
		<-l.done
		l.mu.Lock()
		file := l.file
		l.file = nil
		l.mu.Unlock()
		if file == nil {
			return
		}
		if err := syscall.Flock(int(file.Fd()), syscall.LOCK_UN); err != nil {
			_ = file.Close()
			_ = os.Remove(l.path)
			l.closeErr = err
			return
		}
		removeErr := os.Remove(l.path)
		if errors.Is(removeErr, os.ErrNotExist) {
			removeErr = nil
		}
		l.closeErr = errors.Join(file.Close(), removeErr)
	})
	return l.closeErr
}
