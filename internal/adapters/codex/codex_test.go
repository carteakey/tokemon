package codex

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tokemon/tokemon/internal/adapters"
	"github.com/tokemon/tokemon/internal/usage"
	_ "modernc.org/sqlite"
)

func TestParseThreadSnapshotsWithoutConversationContent(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".codex")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "state_5.sqlite")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.Exec(`CREATE TABLE threads (
id TEXT PRIMARY KEY,
created_at_ms INTEGER NOT NULL,
updated_at_ms INTEGER NOT NULL,
model_provider TEXT NOT NULL,
model TEXT,
cwd TEXT NOT NULL,
tokens_used INTEGER NOT NULL DEFAULT 0
)`)
	if err != nil {
		db.Close()
		t.Fatal(err)
	}
	_, err = db.Exec(`INSERT INTO threads (id, created_at_ms, updated_at_ms, model_provider, model, cwd, tokens_used)
VALUES (?, ?, ?, ?, ?, ?, ?), (?, ?, ?, ?, ?, ?, ?)`,
		"thread-1", 1783787474000, 1783787475000, "openai", "gpt-5.5", "/private/repos/Carteakey.dev", 42,
		"thread-empty", 1783787474000, 1783787475000, "openai", "gpt-5.5", "/private/repos/other", 0)
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
	if event.TotalTokens == nil || *event.TotalTokens != 42 || event.Model != "gpt-5.5" || event.Provider != "openai" {
		t.Fatalf("unexpected event: %+v", event)
	}
	if event.Project != "carteakey.dev" {
		t.Fatalf("project = %q, want privacy-safe basename", event.Project)
	}
	if event.TokenAccuracy != usage.AccuracyReported || event.Metadata != nil {
		t.Fatalf("unexpected privacy or accuracy fields: %+v", event)
	}
	if err := event.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestParseSessionLogPriceDefiningTokensWithoutConversationContent(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".codex", "sessions", "2026", "07", "12")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "session.jsonl")
	log := strings.Join([]string{
		`{"timestamp":"2026-07-12T12:00:00Z","type":"session_meta","payload":{"id":"session-123","model_provider":"openai","cwd":"/private/repos/SecretProject","base_instructions":"do not export"}}`,
		`{"timestamp":"2026-07-12T12:00:01Z","type":"event_msg","payload":{"type":"user_message","message":"private prompt"}}`,
		`{"timestamp":"2026-07-12T12:00:02Z","type":"turn_context","payload":{"model":"gpt-5.6-luna","cwd":"/private/repos/SecretProject","summary":"private summary"}}`,
		`{"timestamp":"2026-07-12T12:00:03Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":100,"cached_input_tokens":80,"output_tokens":20,"reasoning_output_tokens":5,"total_tokens":120},"total_token_usage":{"input_tokens":100,"cached_input_tokens":80,"output_tokens":20,"reasoning_output_tokens":5,"total_tokens":120}}}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(log), 0o600); err != nil {
		t.Fatal(err)
	}

	adapter := New(home)
	sources, err := adapter.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 1 || sources[0].Path != path {
		t.Fatalf("unexpected sources: %+v", sources)
	}
	result, err := adapter.Parse(context.Background(), sources[0], adapters.ParseRequest{MachineID: "machine"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Events) != 1 {
		t.Fatalf("events = %d, want 1", len(result.Events))
	}
	event := result.Events[0]
	if event.InputTokens == nil || *event.InputTokens != 20 || event.CacheReadTokens == nil || *event.CacheReadTokens != 80 || event.OutputTokens == nil || *event.OutputTokens != 20 || event.ReasoningTokens == nil || *event.ReasoningTokens != 5 || event.TotalTokens == nil || *event.TotalTokens != 120 {
		t.Fatalf("unexpected token components: %+v", event)
	}
	if event.Model != "gpt-5.6-luna" || event.SessionID != "session-123" || event.Project != "secretproject" || event.TokenAccuracy != usage.AccuracyReported {
		t.Fatalf("unexpected normalized event: %+v", event)
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"private prompt", "private summary", "do not export", "/private/repos"} {
		if strings.Contains(string(encoded), forbidden) {
			t.Fatalf("event leaked %q: %s", forbidden, encoded)
		}
	}
}
