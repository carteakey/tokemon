package codex

import (
	"bufio"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tokemon/tokemon/internal/adapters"
	"github.com/tokemon/tokemon/internal/agent"
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

func TestParseSessionLogHashesFilenameFallbackAndRemainsStable(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".codex", "sessions", "2026", "07", "12")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "private title.jsonl")
	log := `{"timestamp":"2026-07-12T12:00:03Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15}}}}` + "\n"
	if err := os.WriteFile(path, []byte(log), 0o600); err != nil {
		t.Fatal(err)
	}
	adapter := New(home)
	sources, err := adapter.Discover(context.Background())
	if err != nil || len(sources) != 1 {
		t.Fatalf("sources = %+v, error: %v", sources, err)
	}
	first, err := adapter.Parse(context.Background(), sources[0], adapters.ParseRequest{MachineID: "machine"})
	if err != nil || len(first.Events) != 1 {
		t.Fatalf("first parse = %+v, error: %v", first, err)
	}
	second, err := adapter.Parse(context.Background(), sources[0], adapters.ParseRequest{MachineID: "machine"})
	if err != nil || len(second.Events) != 1 {
		t.Fatalf("second parse = %+v, error: %v", second, err)
	}
	if !strings.HasPrefix(first.Events[0].SessionID, "sha256:") || strings.Contains(first.Events[0].SessionID, "private title") {
		t.Fatalf("filename leaked through session ID: %q", first.Events[0].SessionID)
	}
	if first.Events[0].EventID != second.Events[0].EventID {
		t.Fatalf("fallback session ID changed event identity: first=%+v second=%+v", first.Events[0], second.Events[0])
	}
}

func TestDiscoveringNewSessionDoesNotInvalidateUnchangedSession(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".codex", "sessions", "2026", "07", "12")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	firstPath := filepath.Join(root, "first.jsonl")
	firstLog := strings.Join([]string{
		`{"timestamp":"2026-07-12T12:00:00Z","type":"session_meta","payload":{"id":"first-session","model_provider":"openai"}}`,
		`{"timestamp":"2026-07-12T12:00:01Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":80,"cached_input_tokens":20,"output_tokens":20,"total_tokens":100}}}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(firstPath, []byte(firstLog), 0o600); err != nil {
		t.Fatal(err)
	}

	adapter := New(home)
	sources, err := adapter.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	first, err := adapter.Parse(context.Background(), sources[0], adapters.ParseRequest{MachineID: "machine"})
	if err != nil || len(first.Events) != 1 {
		t.Fatalf("initial parse = %+v, error: %v", first, err)
	}

	secondPath := filepath.Join(root, "second.jsonl")
	secondLog := strings.Join([]string{
		`{"timestamp":"2026-07-12T12:01:00Z","type":"session_meta","payload":{"id":"second-session","model_provider":"openai"}}`,
		`{"timestamp":"2026-07-12T12:01:01Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":40,"cached_input_tokens":10,"output_tokens":10,"total_tokens":50}}}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(secondPath, []byte(secondLog), 0o600); err != nil {
		t.Fatal(err)
	}
	sources, err = adapter.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	unchanged, err := adapter.Parse(context.Background(), adapters.Source{Path: firstPath, Identity: adapters.HashIdentity(firstPath)}, adapters.ParseRequest{MachineID: "machine", Cursor: first.Cursor})
	if err != nil || len(unchanged.Events) != 0 || unchanged.Cursor != first.Cursor {
		t.Fatalf("new session invalidated unchanged session: %+v, error: %v", unchanged, err)
	}
}

func TestLateForkParentInvalidatesOnlyDependentChild(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".codex", "sessions", "2026", "07", "12")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	childPath := filepath.Join(root, "child.jsonl")
	childLog := strings.Join([]string{
		`{"timestamp":"2026-07-12T12:00:03Z","type":"session_meta","payload":{"id":"child-session","forked_from_id":"parent-session","timestamp":"2026-07-12T12:00:01Z","model_provider":"openai"}}`,
		`{"timestamp":"2026-07-12T12:00:04Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":80,"cached_input_tokens":20,"output_tokens":20,"total_tokens":100}}}}`,
		`{"timestamp":"2026-07-12T12:00:05Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":104,"cached_input_tokens":26,"output_tokens":26,"total_tokens":130}}}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(childPath, []byte(childLog), 0o600); err != nil {
		t.Fatal(err)
	}

	adapter := New(home)
	sources, err := adapter.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	beforeParent, err := adapter.Parse(context.Background(), sources[0], adapters.ParseRequest{MachineID: "machine"})
	if err != nil || len(beforeParent.Events) != 2 {
		t.Fatalf("initial child parse = %+v, error: %v", beforeParent, err)
	}

	parentPath := filepath.Join(root, "parent.jsonl")
	parentLog := strings.Join([]string{
		`{"timestamp":"2026-07-12T12:00:00Z","type":"session_meta","payload":{"id":"parent-session","model_provider":"openai"}}`,
		`{"timestamp":"2026-07-12T12:00:00Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":80,"cached_input_tokens":20,"output_tokens":20,"total_tokens":100}}}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(parentPath, []byte(parentLog), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := adapter.Discover(context.Background()); err != nil {
		t.Fatal(err)
	}

	withParent, err := adapter.Parse(context.Background(), adapters.Source{Path: childPath, Identity: adapters.HashIdentity(childPath)}, adapters.ParseRequest{MachineID: "machine", Cursor: beforeParent.Cursor})
	if err != nil || len(withParent.Events) != 2 {
		t.Fatalf("dependent child was not reparsed: %+v, error: %v", withParent, err)
	}
	if withParent.Events[0].TotalTokens == nil || *withParent.Events[0].TotalTokens != 0 || withParent.Events[1].TotalTokens == nil || *withParent.Events[1].TotalTokens != 30 {
		t.Fatalf("late parent baseline was not applied: %+v", withParent.Events)
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

// Incremental tail-parsing must produce exactly the events a full re-parse of
// the same file produces: identical event IDs (they encode the absolute line
// number), identical token deltas, and identical cursor positions.
func TestCodexTailParseMatchesFullParse(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".codex", "sessions", "2026", "11", "05")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "incremental.jsonl")
	baseLines := []string{
		`{"timestamp":"2026-11-05T12:00:00Z","type":"session_meta","payload":{"id":"session-incremental","model_provider":"openai"}}`,
		`{"timestamp":"2026-11-05T12:00:01Z","type":"turn_context","payload":{"model":"gpt-5.6","cwd":"/home/user/project"}}`,
		`{"timestamp":"2026-11-05T12:00:02Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"cached_input_tokens":80,"output_tokens":20,"reasoning_output_tokens":5,"total_tokens":120}}}}`,
		`{"timestamp":"2026-11-05T12:00:03Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":150,"cached_input_tokens":120,"output_tokens":30,"reasoning_output_tokens":10,"total_tokens":180}}}}`,
	}
	appendedLines := []string{
		`{"timestamp":"2026-11-05T12:00:04Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":180,"cached_input_tokens":140,"output_tokens":40,"reasoning_output_tokens":12,"total_tokens":220}}}}`,
		// A cumulative reset (total drops) starts a new sequence.
		`{"timestamp":"2026-11-05T12:00:05Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":60,"cached_input_tokens":10,"output_tokens":50,"reasoning_output_tokens":20,"total_tokens":110}}}}`,
	}
	writeLog := func(lines []string) {
		if err := os.WriteFile(path, []byte(strings.Join(lines, "\n")+"\n"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	writeLog(baseLines)

	adapter := New(home)
	sources, err := adapter.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	source := sources[0]
	first, err := adapter.Parse(context.Background(), source, adapters.ParseRequest{MachineID: "machine"})
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Events) != 2 || first.Cursor.Offset == 0 || first.Cursor.Line != 4 || first.Cursor.State == "" {
		t.Fatalf("first parse = %d events, cursor %+v", len(first.Events), first.Cursor)
	}

	// Append: the classic active-session shape (size and mtime change).
	writeLog(append(baseLines, appendedLines...))
	resumed, err := adapter.Parse(context.Background(), source, adapters.ParseRequest{MachineID: "machine", Cursor: first.Cursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(resumed.Events) != 2 {
		t.Fatalf("resumed parse = %d events, want only the 2 appended: %+v", len(resumed.Events), resumed.Events)
	}

	// Ground truth: a from-scratch full parse of the same final content.
	fullAdapter := New(home)
	fullSources, err := fullAdapter.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	full, err := fullAdapter.Parse(context.Background(), fullSources[0], adapters.ParseRequest{MachineID: "machine"})
	if err != nil {
		t.Fatal(err)
	}
	combined := append(append([]usage.Event{}, first.Events...), resumed.Events...)
	if len(combined) != len(full.Events) {
		t.Fatalf("incremental produced %d events, full parse produced %d", len(combined), len(full.Events))
	}
	for index := range combined {
		got, want := combined[index], full.Events[index]
		if got.EventID != want.EventID || got.TotalTokens == nil || want.TotalTokens == nil || *got.TotalTokens != *want.TotalTokens ||
			got.Project != want.Project || got.Provider != want.Provider || got.Model != want.Model || got.SessionID != want.SessionID ||
			got.InputTokens == nil || want.InputTokens == nil || *got.InputTokens != *want.InputTokens {
			t.Fatalf("event %d diverged:\n incremental %+v\n full       %+v", index, got, want)
		}
	}
	if resumed.Cursor.Offset != full.Cursor.Offset || resumed.Cursor.Line != full.Cursor.Line {
		t.Fatalf("incremental cursor %+v != full cursor %+v", resumed.Cursor, full.Cursor)
	}
}

func TestCodexTailParseFallsBackAfterTruncation(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".codex", "sessions", "2026", "11", "05")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "truncated.jsonl")
	log := strings.Join([]string{
		`{"timestamp":"2026-11-05T12:00:00Z","type":"session_meta","payload":{"id":"session-truncated","model_provider":"openai"}}`,
		`{"timestamp":"2026-11-05T12:00:02Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"output_tokens":20,"total_tokens":120}}}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(log), 0o600); err != nil {
		t.Fatal(err)
	}
	adapter := New(home)
	sources, err := adapter.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	first, err := adapter.Parse(context.Background(), sources[0], adapters.ParseRequest{MachineID: "machine"})
	if err != nil || len(first.Events) != 1 {
		t.Fatalf("first parse = %+v, err %v", first, err)
	}

	// Simulate replacement with a smaller file: the cursor offset now points
	// past the end, which must reset to a full re-parse.
	if err := os.WriteFile(path, []byte(log[:len(log)/2]), 0o600); err != nil {
		t.Fatal(err)
	}
	second, err := adapter.Parse(context.Background(), sources[0], adapters.ParseRequest{MachineID: "machine", Cursor: first.Cursor})
	if err != nil {
		t.Fatal(err)
	}
	// The stored offset points past the smaller file, so the parse must reset:
	// the cursor describes a full parse of the truncated content, not a resume.
	if second.Cursor.Offset != 149 || second.Cursor.Line != 2 || len(second.Events) != 0 {
		t.Fatalf("truncated file parse = %d events, cursor %+v, want a full re-parse of the truncated content", len(second.Events), second.Cursor)
	}
}

func TestCodexTailParseRewindsPartialFinalRecord(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".codex", "sessions", "2026", "11", "05")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "partial.jsonl")
	complete := []string{
		`{"timestamp":"2026-11-05T12:00:00Z","type":"session_meta","payload":{"id":"session-partial","model_provider":"openai"}}`,
		`{"timestamp":"2026-11-05T12:00:02Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"output_tokens":20,"total_tokens":120}}}}`,
	}
	partial := `{"timestamp":"2026-11-05T12:00:03Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_us`
	if err := os.WriteFile(path, []byte(strings.Join(complete, "\n")+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	adapter := New(home)
	sources, err := adapter.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	first, err := adapter.Parse(context.Background(), sources[0], adapters.ParseRequest{MachineID: "machine"})
	if err != nil || len(first.Events) != 1 {
		t.Fatalf("first parse = %+v, err %v", first, err)
	}

	// An active session mid-write: only a fragment of the next record exists.
	if err := os.WriteFile(path, []byte(strings.Join(complete, "\n")+"\n"+partial), 0o600); err != nil {
		t.Fatal(err)
	}
	resumed, err := adapter.Parse(context.Background(), sources[0], adapters.ParseRequest{MachineID: "machine", Cursor: first.Cursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(resumed.Events) != 0 || resumed.Cursor.Offset != first.Cursor.Offset || resumed.Cursor.Line != first.Cursor.Line {
		t.Fatalf("partial-tail parse = %d events, cursor %+v, want an unchanged cursor", len(resumed.Events), resumed.Cursor)
	}

	// The record completes; the next parse emits it exactly once with the line
	// number a full parse would have assigned.
	completed := partial + `age":{"input_tokens":130,"output_tokens":30,"total_tokens":160}}}}`
	if err := os.WriteFile(path, []byte(strings.Join(complete, "\n")+"\n"+completed+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	final, err := adapter.Parse(context.Background(), sources[0], adapters.ParseRequest{MachineID: "machine", Cursor: resumed.Cursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(final.Events) != 1 || final.Events[0].TotalTokens == nil || *final.Events[0].TotalTokens != 40 || final.Events[0].Source.Offset != 3 {
		t.Fatalf("completed-tail parse = %+v", final.Events)
	}
}

func TestCodexTailParseSurvivesStateStoreRoundTrip(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".codex", "sessions", "2026", "11", "05")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "roundtrip.jsonl")
	log := strings.Join([]string{
		`{"timestamp":"2026-11-05T12:00:00Z","type":"session_meta","payload":{"id":"session-roundtrip","model_provider":"openai"}}`,
		`{"timestamp":"2026-11-05T12:00:02Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"output_tokens":20,"total_tokens":120}}}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(log), 0o600); err != nil {
		t.Fatal(err)
	}
	adapter := New(home)
	sources, err := adapter.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	first, err := adapter.Parse(context.Background(), sources[0], adapters.ParseRequest{MachineID: "machine"})
	if err != nil || first.Cursor.State == "" {
		t.Fatalf("first parse cursor %+v, err %v", first.Cursor, err)
	}

	statePath := filepath.Join(t.TempDir(), "state.db")
	store, err := agent.OpenState(statePath)
	if err != nil {
		t.Fatal(err)
	}
	const machineID = "machine"
	report := adapters.SourceReport{Adapter: "codex", Path: path, Source: adapters.Source{Path: path}, Cursor: first.Cursor}
	if err := store.Commit(context.Background(), machineID, agent.StateSnapshot{}, []adapters.SourceReport{report}, nil, time.Now()); err != nil {
		store.Close()
		t.Fatal(err)
	}
	snapshot, err := store.Snapshot(context.Background(), machineID)
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	store.Close()
	restored := snapshot.Cursors[sourceKey(report.Adapter, report.Path)]
	if restored != first.Cursor {
		t.Fatalf("state store round trip lost cursor state: %+v != %+v", restored, first.Cursor)
	}

	// A resumed parse driven by the restored cursor parses only new lines.
	if err := os.WriteFile(path, []byte(log+`{"timestamp":"2026-11-05T12:00:03Z","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":120,"output_tokens":25,"total_tokens":145}}}}`+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	resumed, err := adapter.Parse(context.Background(), sources[0], adapters.ParseRequest{MachineID: "machine", Cursor: restored})
	if err != nil {
		t.Fatal(err)
	}
	if len(resumed.Events) != 1 || resumed.Events[0].TotalTokens == nil || *resumed.Events[0].TotalTokens != 25 {
		t.Fatalf("post-roundtrip resume = %+v", resumed.Events)
	}
}

func sourceKey(adapter, path string) string {
	return adapter + "\x00" + path
}

// The benchmarks quantify the phase-2 win: with a committed cursor the parse
// touches only the appended tail, while a from-scratch parse of the same file
// walks every line. Run with:
//
//	go test ./internal/adapters/codex/ -bench BenchmarkCodex -run '^$'
func benchmarkCodexLog(b *testing.B, lines int) (*Adapter, []adapters.Source, adapters.Cursor) {
	b.Helper()
	home := b.TempDir()
	root := filepath.Join(home, ".codex", "sessions", "2026", "11", "05")
	if err := os.MkdirAll(root, 0o755); err != nil {
		b.Fatal(err)
	}
	path := filepath.Join(root, "benchmark.jsonl")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0o600)
	if err != nil {
		b.Fatal(err)
	}
	writer := bufio.NewWriterSize(file, 1<<20)
	fmt.Fprintf(writer, "%s\n", `{"timestamp":"2026-11-05T12:00:00Z","type":"session_meta","payload":{"id":"session-benchmark","model_provider":"openai"}}`)
	fmt.Fprintf(writer, "%s\n", `{"timestamp":"2026-11-05T12:00:01Z","type":"turn_context","payload":{"model":"gpt-5.6","cwd":"/home/user/project"}}`)
	var cumulative int64
	for index := 0; index < lines; index++ {
		cumulative += 40
		fmt.Fprintf(writer, `{"timestamp":"2026-11-05T12:00:%02dZ","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":%d,"cached_input_tokens":%d,"output_tokens":%d,"reasoning_output_tokens":%d,"total_tokens":%d}}}}`+"\n",
			index%60, cumulative, cumulative*3/4, cumulative/4, cumulative/8, cumulative)
	}
	if err := writer.Flush(); err != nil {
		b.Fatal(err)
	}
	if err := file.Close(); err != nil {
		b.Fatal(err)
	}

	adapter := New(home)
	sources, err := adapter.Discover(context.Background())
	if err != nil {
		b.Fatal(err)
	}
	first, err := adapter.Parse(context.Background(), sources[0], adapters.ParseRequest{MachineID: "machine"})
	if err != nil {
		b.Fatal(err)
	}
	return adapter, sources, first.Cursor
}

func BenchmarkCodexParseFull(b *testing.B) {
	adapter, sources, _ := benchmarkCodexLog(b, 20000)
	b.SetBytes(int64(20000))
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		result, err := adapter.Parse(context.Background(), sources[0], adapters.ParseRequest{MachineID: "machine"})
		if err != nil {
			b.Fatal(err)
		}
		if len(result.Events) != 20000 {
			b.Fatalf("events = %d", len(result.Events))
		}
	}
}

func BenchmarkCodexParseTail(b *testing.B) {
	adapter, sources, cursor := benchmarkCodexLog(b, 20000)
	path := sources[0].Path
	// Append the tail a real active session would produce between polls.
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		b.Fatal(err)
	}
	var tail int64 = 800000
	tailLines := 100
	for index := 0; index < tailLines; index++ {
		tail += 40
		fmt.Fprintf(file, `{"timestamp":"2026-11-05T13:00:%02dZ","type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":%d,"cached_input_tokens":%d,"output_tokens":%d,"reasoning_output_tokens":%d,"total_tokens":%d}}}}`+"\n",
			index%60, tail, tail*3/4, tail/4, tail/8, tail)
	}
	if err := file.Close(); err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(tailLines))
	b.ResetTimer()
	for index := 0; index < b.N; index++ {
		result, err := adapter.Parse(context.Background(), sources[0], adapters.ParseRequest{MachineID: "machine", Cursor: cursor})
		if err != nil {
			b.Fatal(err)
		}
		if len(result.Events) != tailLines {
			b.Fatalf("events = %d, want only the tail", len(result.Events))
		}
	}
}
