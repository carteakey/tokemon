package opencode

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/tokemon/tokemon/internal/adapters"
	"github.com/tokemon/tokemon/internal/usage"
	_ "modernc.org/sqlite"
)

func TestParseSessionSnapshotsAndDerivesTotal(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".local", "share", "opencode")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "opencode.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE session (
id TEXT PRIMARY KEY,
time_created INTEGER NOT NULL,
time_updated INTEGER NOT NULL,
model TEXT NOT NULL,
directory TEXT NOT NULL,
tokens_input INTEGER NOT NULL DEFAULT 0,
tokens_output INTEGER NOT NULL DEFAULT 0,
tokens_reasoning INTEGER NOT NULL DEFAULT 0,
tokens_cache_read INTEGER NOT NULL DEFAULT 0,
tokens_cache_write INTEGER NOT NULL DEFAULT 0,
cost REAL NOT NULL DEFAULT 0
)`)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	model, err := json.Marshal(map[string]string{"id": "deepseek-ai/deepseek-v4-pro", "providerID": "nvidia"})
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO session (id, time_created, time_updated, model, directory, tokens_input, tokens_output, tokens_reasoning, tokens_cache_read, tokens_cache_write, cost)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?), (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"session-1", 1783787474000, 1783787475000, string(model), "/private/repos/Carteakey.dev", 10, 20, 3, 4, 5, 0.42,
		"session-empty", 1783787474000, 1783787475000, string(model), "/private/repos/other", 0, 0, 0, 0, 0, 0)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	adapter := New(home)
	sources, err := adapter.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 1 {
		t.Fatalf("sources = %d, want 1", len(sources))
	}
	result, err := adapter.Parse(context.Background(), sources[0], adapters.ParseRequest{MachineID: "machine"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Events) != 1 {
		t.Fatalf("events = %d, want 1", len(result.Events))
	}
	event := result.Events[0]
	if event.TotalTokens == nil || *event.TotalTokens != 42 || event.Model != "deepseek-ai/deepseek-v4-pro" || event.Provider != "nvidia" {
		t.Fatalf("unexpected event: %+v", event)
	}
	if event.Cost == nil || *event.Cost != 0.42 || !event.CostEstimated || event.TokenAccuracy != usage.AccuracyDerived {
		t.Fatalf("unexpected cost or accuracy fields: %+v", event)
	}
	if event.InputTokens == nil || *event.InputTokens != 10 || event.Metadata != nil {
		t.Fatalf("unexpected token or privacy fields: %+v", event)
	}
	if event.Project != "carteakey.dev" {
		t.Fatalf("project = %q, want privacy-safe basename", event.Project)
	}
	if err := event.Validate(); err != nil {
		t.Fatal(err)
	}
	repeated, err := adapter.Parse(context.Background(), sources[0], adapters.ParseRequest{MachineID: "machine", Cursor: result.Cursor})
	if err != nil || len(repeated.Events) != 0 || repeated.Cursor != result.Cursor {
		t.Fatalf("unchanged OpenCode snapshot was reparsed: %+v, error: %v", repeated, err)
	}
}

func TestParseSkipsSessionMetadataWithoutTokenColumns(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".local", "share", "opencode")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "opencode.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE session (
id TEXT PRIMARY KEY,
time_created INTEGER NOT NULL,
time_updated INTEGER NOT NULL,
directory TEXT NOT NULL
)`); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	adapter := New(home)
	sources, err := adapter.Discover(context.Background())
	if err != nil || len(sources) != 1 {
		t.Fatalf("sources = %d, error: %v", len(sources), err)
	}
	result, err := adapter.Parse(context.Background(), sources[0], adapters.ParseRequest{MachineID: "machine"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Events) != 0 {
		t.Fatalf("unsupported schema produced %d events", len(result.Events))
	}
	repeated, err := adapter.Parse(context.Background(), sources[0], adapters.ParseRequest{MachineID: "machine", Cursor: result.Cursor})
	if err != nil || len(repeated.Events) != 0 || repeated.Cursor != result.Cursor {
		t.Fatalf("unsupported schema was not cached: %+v, error: %v", repeated, err)
	}
}
