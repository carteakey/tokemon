package database

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tokemon/tokemon/internal/catalog"
)

func TestDatabaseLockReclaimsStaleMarker(t *testing.T) {
	databasePath := filepath.Join(t.TempDir(), "tokemon.db")
	lockPath := databasePath + ".lock"
	if err := os.WriteFile(lockPath, []byte("pid=stale\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	staleAt := time.Now().Add(-databaseLockStaleAfter - time.Second)
	if err := os.Chtimes(lockPath, staleAt, staleAt); err != nil {
		t.Fatal(err)
	}
	store, err := Open(databasePath, catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(lockPath); !os.IsNotExist(err) {
		t.Fatalf("lock marker after clean close: err=%v", err)
	}
}
