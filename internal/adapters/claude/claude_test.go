package claude

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/tokemon/tokemon/internal/adapters"
	"github.com/tokemon/tokemon/internal/usage"
)

func TestParseAssistantUsageWithoutConversationContent(t *testing.T) {
	home := t.TempDir()
	transcriptDir := filepath.Join(home, ".claude", "projects", "encoded-project")
	if err := os.MkdirAll(transcriptDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(transcriptDir, "session-123.jsonl")
	content := strings.Join([]string{
		`{"type":"user","timestamp":"2026-07-12T12:00:00Z","message":{"role":"user","content":"do not export this prompt /private/repo"}}`,
		`{"type":"assistant","timestamp":"2026-07-12T12:00:01Z","sessionId":"session-123","message":{"model":"claude-opus-4","content":"do not export this response","usage":{"input_tokens":10,"output_tokens":20,"cache_read_input_tokens":30,"cache_creation_input_tokens":40}}}`,
		`{"type":"system","subtype":"turn_duration","timestamp":"2026-07-12T12:00:02Z","sessionId":"session-123","durationMs":1234}`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
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
	if event.TotalTokens == nil || *event.TotalTokens != 100 || event.TokenAccuracy != usage.AccuracyDerived {
		t.Fatalf("unexpected total: %+v", event)
	}
	if event.DurationMS == nil || *event.DurationMS != 1234 {
		t.Fatalf("unexpected duration: %+v", event)
	}
	if event.Model != "claude-opus-4" || event.Provider != "anthropic" || event.SessionID != "session-123" {
		t.Fatalf("unexpected identity: %+v", event)
	}
	if event.Source.Identity == "" || strings.Contains(event.Source.Identity, "encoded-project") || strings.Contains(event.Source.Identity, home) {
		t.Fatalf("source identity leaked a local path: %+v", event.Source)
	}
	serialized, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(serialized), "do not export") || strings.Contains(string(serialized), "/private/repo") {
		t.Fatalf("conversation content leaked into event: %s", serialized)
	}
	if err := event.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestParseLargeClaudeRecordWithinBound(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".claude", "projects", "project", "session.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	privateContent := strings.Repeat("private conversation content", 100000)
	line := `{"type":"assistant","timestamp":"2026-07-12T12:00:01Z","sessionId":"session","message":{"model":"claude-sonnet","content":"` + privateContent + `","usage":{"input_tokens":10,"output_tokens":20}}}` + "\n"
	if len(line) <= adapters.MaxRecordBytes || len(line) >= maxRecordBytes {
		t.Fatalf("fixture size = %d, want between default and Claude bounds", len(line))
	}
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := New(home).Parse(context.Background(), adapters.Source{Path: path}, adapters.ParseRequest{MachineID: "machine"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Events) != 1 || result.Events[0].InputTokens == nil || *result.Events[0].InputTokens != 10 || result.Events[0].OutputTokens == nil || *result.Events[0].OutputTokens != 20 {
		t.Fatalf("large record usage = %+v", result.Events)
	}
	payload, err := json.Marshal(result.Events[0])
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(payload), "private conversation content") {
		t.Fatal("conversation content leaked into usage event")
	}
}

func TestNormalizedPayloadIsExactAndMetadataOnly(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".claude", "projects", "private-project", "session.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	content := strings.Join([]string{
		`{"type":"assistant","timestamp":"2026-07-12T12:00:01Z","sessionId":"session-123","cwd":"/private/repo","slug":"private title","message":{"model":"claude-sonnet-4","content":"private response","usage":{"input_tokens":10,"output_tokens":20,"cache_read_input_tokens":30,"cache_creation_input_tokens":40}}}`,
		`{"type":"system","subtype":"turn_duration","timestamp":"2026-07-12T12:00:02Z","sessionId":"session-123","durationMs":1234}`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	result, err := New(home).Parse(context.Background(), adapters.Source{Path: path}, adapters.ParseRequest{MachineID: "machine"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Events) != 1 {
		t.Fatalf("events = %d, want 1", len(result.Events))
	}
	payload, err := json.Marshal(result.Events[0])
	if err != nil {
		t.Fatal(err)
	}
	const want = `{"schema_version":"1","event_id":"sha256:18ff56ed97210f57f21a30ce17b70bd266d25b66b52f13bf6f9c6ed26ef90ecf","timestamp":"2026-07-12T12:00:01Z","machine_id":"machine","session_id":"session-123","provider":"anthropic","model":"claude-sonnet-4","tool":"claude-code","input_tokens":10,"output_tokens":20,"cache_read_tokens":30,"cache_write_tokens":40,"reasoning_tokens":null,"total_tokens":100,"duration_ms":1234,"cost":null,"currency":"USD","token_accuracy":"derived","source":{"adapter":"claude-code","adapter_version":"0.2.0","identity":"sha256:51e816557d9eccffd61d80ae073d8208c3a0b2b46332013f8cbb292ad8f9d7c1","offset":1}}`
	if string(payload) != want {
		t.Fatalf("payload mismatch\n got: %s\nwant: %s", payload, want)
	}
}

func TestParseUnknownCacheFieldsDoesNotInventTotal(t *testing.T) {
	home := t.TempDir()
	transcriptDir := filepath.Join(home, ".claude", "projects", "project")
	if err := os.MkdirAll(transcriptDir, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(transcriptDir, "session.jsonl")
	line := `{"type":"assistant","timestamp":"2026-07-12T12:00:01Z","message":{"model":"claude-sonnet","usage":{"input_tokens":10,"output_tokens":20}}}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := New(home).Parse(context.Background(), adapters.Source{Path: path}, adapters.ParseRequest{MachineID: "machine"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Events) != 1 || result.Events[0].TotalTokens != nil || result.Events[0].TokenAccuracy != usage.AccuracyUnknown {
		t.Fatalf("unknown fields were silently converted: %+v", result.Events)
	}
}

func TestParseUsesCommittedCursorForAppendedRecords(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".claude", "projects", "project", "session.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	first := `{"type":"assistant","timestamp":"2026-07-12T12:00:01Z","sessionId":"session-123","message":{"model":"claude-sonnet","usage":{"input_tokens":10,"output_tokens":20,"cache_read_input_tokens":30,"cache_creation_input_tokens":40}}}` + "\n"
	second := `{"type":"assistant","timestamp":"2026-07-12T12:00:02Z","sessionId":"session-123","message":{"model":"claude-sonnet","usage":{"input_tokens":11,"output_tokens":21,"cache_read_input_tokens":31,"cache_creation_input_tokens":41}}}` + "\n"
	if err := os.WriteFile(path, []byte(first), 0o600); err != nil {
		t.Fatal(err)
	}
	adapter := New(home)
	source := adapters.Source{Path: path}
	initial, err := adapter.Parse(context.Background(), source, adapters.ParseRequest{MachineID: "machine"})
	if err != nil {
		t.Fatal(err)
	}
	if len(initial.Events) != 1 || initial.Cursor.Offset != int64(len(first)) || initial.Cursor.Line != 1 {
		t.Fatalf("unexpected initial parse: %+v", initial)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(second); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	incremental, err := adapter.Parse(context.Background(), source, adapters.ParseRequest{MachineID: "machine", Cursor: initial.Cursor})
	if err != nil {
		t.Fatal(err)
	}
	full, err := adapter.Parse(context.Background(), source, adapters.ParseRequest{MachineID: "machine"})
	if err != nil {
		t.Fatal(err)
	}
	if len(incremental.Events) != 1 || len(full.Events) != 2 || incremental.Events[0].EventID != full.Events[1].EventID {
		t.Fatalf("cursor changed event identity: incremental=%+v full=%+v", incremental.Events, full.Events)
	}
}

func TestParseLeavesIncompleteFinalRecordForRetry(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".claude", "projects", "project", "session.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	complete := `{"type":"assistant","timestamp":"2026-07-12T12:00:01Z","sessionId":"session-123","message":{"model":"claude-sonnet","usage":{"input_tokens":10,"output_tokens":20,"cache_read_input_tokens":30,"cache_creation_input_tokens":40}}}` + "\n"
	partial := `{"type":"assistant","timestamp":"2026-07-12T12:00:02Z","sessionId":"session-123","message":{"model":"claude-sonnet","usage":{"input_tokens":11`
	if err := os.WriteFile(path, []byte(complete+partial), 0o600); err != nil {
		t.Fatal(err)
	}
	adapter := New(home)
	result, err := adapter.Parse(context.Background(), adapters.Source{Path: path}, adapters.ParseRequest{MachineID: "machine"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Events) != 1 || result.Cursor.Offset != int64(len(complete)) {
		t.Fatalf("incomplete record advanced cursor: %+v", result)
	}
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(`,"output_tokens":21,"cache_read_input_tokens":31,"cache_creation_input_tokens":41}}}` + "\n"); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	retry, err := adapter.Parse(context.Background(), adapters.Source{Path: path}, adapters.ParseRequest{MachineID: "machine", Cursor: result.Cursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(retry.Events) != 1 {
		t.Fatalf("completed record was not retried: %+v", retry.Events)
	}
}

func TestParseReplaysPreviousLineForLateDurationMetadata(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".claude", "projects", "project", "session.jsonl")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	assistant := `{"type":"assistant","timestamp":"2026-07-12T12:00:01Z","sessionId":"session-123","message":{"model":"claude-sonnet","usage":{"input_tokens":10,"output_tokens":20,"cache_read_input_tokens":30,"cache_creation_input_tokens":40}}}` + "\n"
	if err := os.WriteFile(path, []byte(assistant), 0o600); err != nil {
		t.Fatal(err)
	}
	adapter := New(home)
	initial, err := adapter.Parse(context.Background(), adapters.Source{Path: path}, adapters.ParseRequest{MachineID: "machine"})
	if err != nil {
		t.Fatal(err)
	}
	if len(initial.Events) != 1 || initial.Events[0].DurationMS != nil {
		t.Fatalf("unexpected initial parse: %+v", initial)
	}
	duration := `{"type":"system","subtype":"turn_duration","timestamp":"2026-07-12T12:00:02Z","sessionId":"session-123","durationMs":1234}` + "\n"
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(duration); err != nil {
		file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	updated, err := adapter.Parse(context.Background(), adapters.Source{Path: path}, adapters.ParseRequest{MachineID: "machine", Cursor: initial.Cursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(updated.Events) != 1 || updated.Events[0].DurationMS == nil || *updated.Events[0].DurationMS != 1234 {
		t.Fatalf("late duration was not attached: %+v", updated)
	}
	if updated.Events[0].EventID != initial.Events[0].EventID {
		t.Fatalf("late duration changed event ID: initial=%q updated=%q", initial.Events[0].EventID, updated.Events[0].EventID)
	}
}
