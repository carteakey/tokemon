package opencode

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
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
	unsafeSessionID := "/private/repo/private-session-title"
	_, err = db.Exec(`INSERT INTO session (id, time_created, time_updated, model, directory, tokens_input, tokens_output, tokens_reasoning, tokens_cache_read, tokens_cache_write, cost)
VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?), (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?), (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"session-1", 1783787474000, 1783787475000, string(model), "/private/repos/Carteakey.dev", 10, 20, 3, 4, 5, 0.42,
		unsafeSessionID, 1783787475000, 1783787476000, string(model), "/private/repos/safe-project", 1, 2, 0, 0, 0, 0.01,
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
	if len(result.Events) != 2 {
		t.Fatalf("events = %d, want 2", len(result.Events))
	}
	eventsBySession := make(map[string]usage.Event, len(result.Events))
	for _, event := range result.Events {
		eventsBySession[event.SessionID] = event
	}
	event, ok := eventsBySession["session-1"]
	if !ok {
		t.Fatalf("safe OpenCode session ID changed: %+v", result.Events)
	}
	if event.TotalTokens == nil || *event.TotalTokens != 42 || event.Model != "deepseek-ai/deepseek-v4-pro" || event.Provider != "nvidia" {
		t.Fatalf("unexpected event: %+v", event)
	}
	if want := usage.DeterministicID("machine", adapterID, "opencode:opencode.db:session", 0, "snapshot", "session-1"); event.EventID != want {
		t.Fatalf("safe OpenCode event ID changed: got %q, want %q", event.EventID, want)
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
	if err := usage.ValidateOutbound(event); err != nil {
		t.Fatalf("safe OpenCode event failed outbound validation: %v", err)
	}

	unsafeEvent, ok := eventsBySession[usage.NormalizeSessionID(unsafeSessionID)]
	if !ok || !strings.HasPrefix(unsafeEvent.SessionID, "sha256:") || strings.Contains(unsafeEvent.SessionID, unsafeSessionID) {
		t.Fatalf("unsafe OpenCode session ID was not made opaque: %+v", unsafeEvent)
	}
	if err := unsafeEvent.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := usage.ValidateOutbound(unsafeEvent); err != nil {
		t.Fatalf("normalized OpenCode event failed outbound validation: %v", err)
	}
	if err := usage.ValidateOutboundBatch(result.Events); err != nil {
		t.Fatalf("normalized OpenCode batch failed outbound validation: %v", err)
	}
	if err := usage.ValidateOutboundBatch([]usage.Event{{SessionID: "session-1"}, {SessionID: unsafeSessionID}}); !errors.Is(err, usage.ErrSensitiveContent) {
		t.Fatalf("raw unsafe OpenCode ID did not demonstrate batch rejection: %v", err)
	}
	serialized, err := json.Marshal(unsafeEvent)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(serialized), unsafeSessionID) || strings.Contains(string(serialized), "private-session-title") || strings.Contains(string(serialized), "/private/") {
		t.Fatalf("unsafe OpenCode session value leaked into event: %s", serialized)
	}

	reparsed, err := New(home).Parse(context.Background(), sources[0], adapters.ParseRequest{MachineID: "machine"})
	if err != nil || len(reparsed.Events) != len(result.Events) {
		t.Fatalf("fresh OpenCode parse = %+v, error: %v", reparsed, err)
	}
	for _, reparsedEvent := range reparsed.Events {
		if previous, exists := eventsBySession[reparsedEvent.SessionID]; !exists || previous.EventID != reparsedEvent.EventID {
			t.Fatalf("OpenCode event identity changed across parses: first=%+v second=%+v", previous, reparsedEvent)
		}
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
