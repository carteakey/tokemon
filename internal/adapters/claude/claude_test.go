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
