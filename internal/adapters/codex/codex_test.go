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
	repeated, err := adapter.Parse(context.Background(), sources[0], adapters.ParseRequest{MachineID: "machine", Cursor: result.Cursor})
	if err != nil || len(repeated.Events) != 0 || repeated.Cursor != result.Cursor {
		t.Fatalf("unchanged thread snapshot was reparsed: %+v, error: %v", repeated, err)
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
	repeated, err := adapter.Parse(context.Background(), sources[0], adapters.ParseRequest{MachineID: "machine", Cursor: result.Cursor})
	if err != nil || len(repeated.Events) != 0 || repeated.Cursor != result.Cursor {
		t.Fatalf("unchanged session log was reparsed: %+v, error: %v", repeated, err)
	}
}

func TestParseSessionLogUsesCumulativeUsageDeltas(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".codex", "sessions", "2026", "07", "12")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "cumulative.jsonl")
	log := strings.Join([]string{
		`{"timestamp":"2026-07-12T12:00:00Z","type":"session_meta","payload":{"id":"session-cumulative","model_provider":"openai"}}`,
		`{"timestamp":"2026-07-12T12:00:01Z","type":"turn_context","payload":{"model":"gpt-5.6"}}`,
		`{"timestamp":"2026-07-12T12:00:02Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"cached_input_tokens":80,"output_tokens":20,"reasoning_output_tokens":5,"total_tokens":120},"last_token_usage":{"input_tokens":100,"cached_input_tokens":80,"output_tokens":20,"reasoning_output_tokens":5,"total_tokens":120}}}}`,
		`{"timestamp":"2026-07-12T12:00:03Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":150,"cached_input_tokens":120,"output_tokens":30,"reasoning_output_tokens":10,"total_tokens":180},"last_token_usage":{"input_tokens":999,"cached_input_tokens":900,"output_tokens":999,"reasoning_output_tokens":999,"total_tokens":1998}}}}`,
		`{"timestamp":"2026-07-12T12:00:04Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":150,"cached_input_tokens":120,"output_tokens":30,"reasoning_output_tokens":10,"total_tokens":180},"last_token_usage":{"input_tokens":700,"cached_input_tokens":600,"output_tokens":700,"reasoning_output_tokens":600,"total_tokens":1400}}}}`,
		`{"timestamp":"2026-07-12T12:00:05Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":180,"cached_input_tokens":140,"output_tokens":40,"reasoning_output_tokens":12,"total_tokens":220},"last_token_usage":{"input_tokens":800,"cached_input_tokens":700,"output_tokens":800,"reasoning_output_tokens":700,"total_tokens":1600}}}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(log), 0o600); err != nil {
		t.Fatal(err)
	}

	adapter := New(home)
	sources, err := adapter.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	result, err := adapter.Parse(context.Background(), sources[0], adapters.ParseRequest{MachineID: "machine"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Events) != 4 {
		t.Fatalf("events = %d, want 4: %+v", len(result.Events), result.Events)
	}
	wantTotals := []int64{120, 60, 0, 40}
	var total int64
	for index, event := range result.Events {
		if event.TotalTokens == nil || *event.TotalTokens != wantTotals[index] {
			t.Fatalf("event %d total = %v, want %d: %+v", index, event.TotalTokens, wantTotals[index], event)
		}
		total += *event.TotalTokens
	}
	if total != 220 {
		t.Fatalf("summed totals = %d, want cumulative final total 220", total)
	}
	if result.Events[1].InputTokens == nil || *result.Events[1].InputTokens != 10 || result.Events[1].CacheReadTokens == nil || *result.Events[1].CacheReadTokens != 40 || result.Events[1].OutputTokens == nil || *result.Events[1].OutputTokens != 10 {
		t.Fatalf("second event was not a cumulative delta: %+v", result.Events[1])
	}
}

func TestParseForkedSessionUsesChildIDAndDeduplicatesRepeatedUsage(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".codex", "sessions", "2026", "07", "12")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "fork.jsonl")
	log := strings.Join([]string{
		`{"timestamp":"2026-07-12T12:00:00Z","type":"session_meta","payload":{"id":"child-session","session_id":"parent-session","model_provider":"openai"}}`,
		`{"timestamp":"2026-07-12T12:00:00Z","type":"session_meta","payload":{"id":"parent-session","session_id":"parent-session","model_provider":"openai"}}`,
		`{"timestamp":"2026-07-12T12:00:01Z","type":"turn_context","payload":{"model":"gpt-5.6"}}`,
		`{"timestamp":"2026-07-12T12:00:02Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":100,"cached_input_tokens":80,"output_tokens":20,"reasoning_output_tokens":5}}}}`,
		`{"timestamp":"2026-07-12T12:00:02Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":100,"cached_input_tokens":80,"output_tokens":20,"reasoning_output_tokens":5}}}}`,
		`{"timestamp":"2026-07-12T12:00:03Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":110,"cached_input_tokens":80,"output_tokens":20,"reasoning_output_tokens":5}}}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(log), 0o600); err != nil {
		t.Fatal(err)
	}

	adapter := New(home)
	sources, err := adapter.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	result, err := adapter.Parse(context.Background(), sources[0], adapters.ParseRequest{MachineID: "machine"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Events) != 2 {
		t.Fatalf("events = %d, want 2: %+v", len(result.Events), result.Events)
	}
	for _, event := range result.Events {
		if event.SessionID != "child-session" {
			t.Fatalf("session ID = %q, want child-session: %+v", event.SessionID, event)
		}
	}

	legacyIdentity := adapters.HashIdentity("codex:session:parent-session")
	wantID := usage.DeterministicID("machine", "codex", legacyIdentity, 4, "2026-07-12T12:00:02Z", "parent-session")
	if result.Events[0].EventID != wantID || result.Events[0].Source.Identity != legacyIdentity {
		t.Fatalf("fork event identity changed incompatibly: got %+v, want ID %q and identity %q", result.Events[0], wantID, legacyIdentity)
	}
}

func TestParseForkedSessionSubtractsParentPrefixAtForkBoundary(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".codex", "sessions", "2026", "07", "12")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	parentPath := filepath.Join(root, "parent.jsonl")
	childPath := filepath.Join(root, "child.jsonl")
	parentLog := strings.Join([]string{
		`{"timestamp":"2026-07-12T12:00:00Z","type":"session_meta","payload":{"id":"parent-session","model_provider":"openai"}}`,
		`{"timestamp":"2026-07-12T12:00:00Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":80,"cached_input_tokens":20,"output_tokens":20,"total_tokens":100}}}}`,
		`{"timestamp":"2026-07-12T12:00:02Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":160,"cached_input_tokens":40,"output_tokens":40,"total_tokens":200}}}}`,
	}, "\n") + "\n"
	childLog := strings.Join([]string{
		`{"timestamp":"2026-07-12T12:00:03Z","type":"session_meta","payload":{"id":"child-session","forked_from_id":"parent-session","timestamp":"2026-07-12T12:00:01Z","model_provider":"openai"}}`,
		`{"timestamp":"2026-07-12T12:00:03Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":16,"cached_input_tokens":4,"output_tokens":4,"total_tokens":20}}}}`,
		`{"timestamp":"2026-07-12T12:00:04Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":80,"cached_input_tokens":20,"output_tokens":20,"total_tokens":100}}}}`,
		`{"timestamp":"2026-07-12T12:00:05Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":104,"cached_input_tokens":26,"output_tokens":26,"total_tokens":130}}}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(parentPath, []byte(parentLog), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(childPath, []byte(childLog), 0o600); err != nil {
		t.Fatal(err)
	}

	adapter := New(home)
	sources, err := adapter.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 2 {
		t.Fatalf("sources = %d, want 2", len(sources))
	}
	var childSource adapters.Source
	for _, source := range sources {
		if source.Path == childPath {
			childSource = source
		}
	}
	if childSource.Path == "" {
		t.Fatal("child source was not discovered")
	}
	result, err := adapter.Parse(context.Background(), childSource, adapters.ParseRequest{MachineID: "machine"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Events) != 3 {
		t.Fatalf("child events = %d, want 3: %+v", len(result.Events), result.Events)
	}
	if result.Events[0].TotalTokens == nil || *result.Events[0].TotalTokens != 0 || result.Events[1].TotalTokens == nil || *result.Events[1].TotalTokens != 0 || result.Events[2].TotalTokens == nil || *result.Events[2].TotalTokens != 30 {
		t.Fatalf("child prefix was not removed: %+v", result.Events)
	}
	if result.Events[0].SessionID != "child-session" || result.Events[1].SessionID != "child-session" {
		t.Fatalf("child session attribution changed: %+v", result.Events)
	}
}
