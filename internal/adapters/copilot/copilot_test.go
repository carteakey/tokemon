package copilot

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tokemon/tokemon/internal/adapters"
	"github.com/tokemon/tokemon/internal/usage"
)

func TestParseShutdownUsageWithoutCollectingSessionContent(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".copilot", "session-state", "session-private-title")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "events.jsonl")
	fixture := strings.Join([]string{
		`{"type":"session.start","timestamp":"2026-07-13T12:00:00Z","data":{"sessionId":"private session title","startTime":"2026-07-13T12:00:00Z","context":{"cwd":"/private/repos/Carteakey.dev"}}}`,
		`{"type":"user.message","timestamp":"2026-07-13T12:01:00Z","data":{"content":"SECRET PROMPT"}}`,
		`{"type":"assistant.message","timestamp":"2026-07-13T12:01:01Z","data":{"content":"SECRET RESPONSE","outputTokens":20}}`,
		`{"type":"tool.execution_start","timestamp":"2026-07-13T12:01:02Z","data":{"toolName":"edit","arguments":{"path":"/private/repos/Carteakey.dev/secret.go"}}}`,
		`{"type":"session.shutdown","timestamp":"2026-07-13T12:02:00Z","data":{"sessionStartTime":1783944000000,"modelMetrics":{"claude-sonnet-4.5":{"requests":{"count":2,"cost":2},"usage":{"inputTokens":100,"outputTokens":20,"cacheReadTokens":30,"cacheWriteTokens":40,"reasoningTokens":5}}}}}`,
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(fixture), 0o600); err != nil {
		t.Fatal(err)
	}

	adapter := New(home)
	sources, err := adapter.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 1 || sources[0].Path != path {
		t.Fatalf("sources = %+v, want one Copilot session source", sources)
	}

	result, err := adapter.Parse(context.Background(), sources[0], adapters.ParseRequest{MachineID: "machine"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Events) != 1 {
		t.Fatalf("events = %d, want 1", len(result.Events))
	}
	event := result.Events[0]
	if event.SessionID != usage.NormalizeSessionID("private session title") || event.SessionID == "private session title" || event.Project != "carteakey.dev" {
		t.Fatalf("unexpected session metadata: %+v", event)
	}
	if err := usage.ValidateOutbound(event); err != nil {
		t.Fatalf("normalized Copilot session rejected by outbound guard: %v", err)
	}
	if event.Provider != "github" || event.Model != "claude-sonnet-4.5" || event.Tool != adapterID {
		t.Fatalf("unexpected identity fields: %+v", event)
	}
	if event.InputTokens == nil || *event.InputTokens != 100 || event.OutputTokens == nil || *event.OutputTokens != 20 ||
		event.CacheReadTokens == nil || *event.CacheReadTokens != 30 || event.CacheWriteTokens == nil || *event.CacheWriteTokens != 40 ||
		event.ReasoningTokens == nil || *event.ReasoningTokens != 5 || event.TotalTokens == nil || *event.TotalTokens != 195 {
		t.Fatalf("unexpected token fields: %+v", event)
	}
	if event.TokenAccuracy != usage.AccuracyDerived || event.Timestamp != time.UnixMilli(1783944000000).UTC() {
		t.Fatalf("unexpected accuracy or timestamp: %+v", event)
	}
	if event.Metadata != nil || event.Source.Identity == path || strings.Contains(string(mustJSON(t, event)), "SECRET") || strings.Contains(string(mustJSON(t, event)), "/private/") {
		t.Fatalf("privacy boundary failed: %+v", event)
	}
	if err := event.Validate(); err != nil {
		t.Fatal(err)
	}

	repeated, err := adapter.Parse(context.Background(), sources[0], adapters.ParseRequest{MachineID: "machine", Cursor: result.Cursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(repeated.Events) != 0 || repeated.Cursor != result.Cursor {
		t.Fatalf("unchanged snapshot was reparsed: first=%+v second=%+v", event, repeated)
	}
}

func TestSessionIDFallbackAndAdversarialExplicitValuesRemainOpaque(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  string
	}{
		{name: "missing", raw: ""},
		{name: "title", raw: "private session title"},
		{name: "relative path", raw: "../private/title"},
		{name: "credential", raw: "ghp_private-token"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			root := filepath.Join(home, ".copilot", "session-state", "private session title")
			if err := os.MkdirAll(root, 0o755); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, "events.jsonl")
			start := ""
			if tc.raw != "" {
				start = `{"type":"session.start","timestamp":"2026-07-13T12:00:00Z","data":{"sessionId":"` + tc.raw + `","startTime":"2026-07-13T12:00:00Z","context":{"cwd":"/private/repos/Carteakey.dev"}}}` + "\n"
			}
			fixture := start + `{"type":"session.shutdown","timestamp":"2026-07-13T12:02:00Z","data":{"sessionStartTime":1783944000000,"modelMetrics":{"model":{"usage":{"inputTokens":1,"outputTokens":2,"cacheReadTokens":3,"cacheWriteTokens":4}}}}}` + "\n"
			if err := os.WriteFile(path, []byte(fixture), 0o600); err != nil {
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
			event := first.Events[0]
			want := usage.HashSessionID(path)
			if tc.raw != "" {
				want = usage.NormalizeSessionID(tc.raw)
			}
			if event.SessionID != want || (tc.raw != "" && strings.Contains(event.SessionID, tc.raw)) {
				t.Fatalf("session ID = %q, want opaque %q for %q", event.SessionID, want, tc.raw)
			}
			if err := usage.ValidateOutbound(event); err != nil {
				t.Fatalf("normalized session rejected by outbound guard: %v", err)
			}

			second, err := adapter.Parse(context.Background(), sources[0], adapters.ParseRequest{MachineID: "machine"})
			if err != nil || len(second.Events) != 1 || second.Events[0].EventID != event.EventID {
				t.Fatalf("session replacement changed identity: first=%+v second=%+v error=%v", first, second, err)
			}
		})
	}
}

func TestParseReturnsModelsInStableOrderAndIgnoresPartialFinalRecord(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".copilot", "session-state", "session-1")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "events.jsonl")
	complete := `{"type":"session.shutdown","timestamp":"2026-07-13T12:02:00Z","data":{"sessionStartTime":1783944000000,"modelMetrics":{"z-model":{"usage":{"inputTokens":1,"outputTokens":2,"cacheReadTokens":3,"cacheWriteTokens":4}},"a-model":{"usage":{"inputTokens":5,"outputTokens":6,"cacheReadTokens":7,"cacheWriteTokens":8}}}}}`
	if err := os.WriteFile(path, []byte(complete+"\n{\"type\":\"session.shutdown\""), 0o600); err != nil {
		t.Fatal(err)
	}

	sources, err := New(home).Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	result, err := New(home).Parse(context.Background(), sources[0], adapters.ParseRequest{MachineID: "machine"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Events) != 2 || result.Events[0].Model != "a-model" || result.Events[1].Model != "z-model" {
		t.Fatalf("events = %+v, want stable a-model then z-model", result.Events)
	}
	if result.Cursor.Offset != int64(len(complete)+1) {
		t.Fatalf("cursor offset = %d, want %d before partial record", result.Cursor.Offset, len(complete)+1)
	}
}

func mustJSON(t *testing.T, value any) []byte {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return data
}
