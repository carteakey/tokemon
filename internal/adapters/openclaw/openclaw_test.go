package openclaw

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tokemon/tokemon/internal/adapters"
	"github.com/tokemon/tokemon/internal/usage"
)

func TestDiscoverOnlyOpenClawSessionTranscripts(t *testing.T) {
	home := t.TempDir()
	paths := []string{
		filepath.Join(home, ".openclaw", "agents", "main", "sessions", "main.jsonl"),
		filepath.Join(home, ".openclaw", "agents", "worker", "sessions", "worker.jsonl"),
		filepath.Join(home, ".openclaw", "agents", "main", "sessions", "main.trajectory.jsonl"),
		filepath.Join(home, ".openclaw", "agents", "main", "other.jsonl"),
	}
	for _, path := range paths {
		if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}

	sources, err := New(home).Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 2 {
		t.Fatalf("sources = %d, want 2: %+v", len(sources), sources)
	}
	if sources[0].Path >= sources[1].Path {
		t.Fatalf("sources are not sorted: %+v", sources)
	}
	for _, source := range sources {
		if source.Identity == "" || strings.Contains(source.Identity, home) {
			t.Fatalf("source identity leaked local path: %+v", source)
		}
	}
}

func TestParseMetadataOnlyUsageAcrossTranscriptShapes(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".openclaw", "agents", "main", "sessions", "session-123.jsonl")
	content := strings.Join([]string{
		`{"type":"session","version":3,"id":"session-123","timestamp":"2026-07-18T23:00:00Z","cwd":"/private/repo"}`,
		`{"type":"message","id":"user-1","timestamp":"2026-07-18T23:00:01Z","message":{"role":"user","content":[{"type":"text","text":"private prompt"}]}}`,
		`{"type":"message","id":"assistant-1","timestamp":"2026-07-18T23:00:02Z","message":{"role":"assistant","provider":"anthropic","model":"claude-opus-4","content":[{"type":"text","text":"private response"}],"usage":{"input":10,"output":20,"cacheRead":30,"cacheWrite":40,"totalTokens":100,"cost":{"total":0.42}}}}`,
		`{"type":"message","id":"assistant-2","timestamp":1783944000000,"provider":"openai","model":"gpt-5.6","message":{"role":"assistant","content":[{"type":"toolCall","arguments":{"command":"private command"}}],"usage":{"input":20,"output":30,"cacheRead":0,"cacheWrite":0,"totalTokens":50}}}`,
		`{"type":"message","id":"delivery","timestamp":"2026-07-18T23:00:04Z","provider":"openclaw","model":"delivery-mirror","message":{"role":"assistant","usage":{"input":0,"output":0,"cacheRead":0,"cacheWrite":0,"totalTokens":0}}}`,
	}, "\n") + "\n"
	writeFile(t, path, content)

	source := discoverOne(t, New(home))
	result, err := New(home).Parse(context.Background(), source, adapters.ParseRequest{MachineID: "machine"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Events) != 2 {
		t.Fatalf("events = %d, want 2: %+v", len(result.Events), result.Events)
	}

	first := result.Events[0]
	if first.SessionID != "session-123" || first.Project != "repo" || first.Provider != "anthropic" || first.Model != "claude-opus-4" {
		t.Fatalf("unexpected first identity: %+v", first)
	}
	if first.TotalTokens == nil || *first.TotalTokens != 100 || first.TokenAccuracy != usage.AccuracyReported {
		t.Fatalf("unexpected first total: %+v", first)
	}
	if first.Cost == nil || *first.Cost != 0.42 || !first.CostEstimated {
		t.Fatalf("unexpected first cost: %+v", first)
	}
	if first.Timestamp != mustOpenClawTime(t, "2026-07-18T23:00:02Z") {
		t.Fatalf("unexpected first timestamp: %s", first.Timestamp)
	}

	second := result.Events[1]
	if second.Provider != "openai" || second.Model != "gpt-5.6" || second.Timestamp != time.UnixMilli(1783944000000).UTC() {
		t.Fatalf("unexpected second event: %+v", second)
	}
	if second.InputTokens == nil || *second.InputTokens != 20 || second.OutputTokens == nil || *second.OutputTokens != 30 {
		t.Fatalf("unexpected second token fields: %+v", second)
	}
	for _, value := range []string{"private prompt", "private response", "private command", "/private/repo"} {
		payload, err := json.Marshal(result.Events)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(payload), value) {
			t.Fatalf("transcript content leaked into events: %s", payload)
		}
	}
	for _, event := range result.Events {
		if event.Tool != "openclaw" || event.Source.Adapter != "openclaw" || strings.Contains(event.Source.Identity, home) {
			t.Fatalf("unexpected source fields: %+v", event.Source)
		}
		if err := event.Validate(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestParseDerivesTotalsOnlyFromCompleteComponents(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".openclaw", "agents", "main", "sessions", "session.jsonl")
	content := strings.Join([]string{
		`{"type":"session","id":"session","cwd":"/repo"}`,
		`{"type":"message","id":"derived","timestamp":"2026-07-18T23:00:02Z","message":{"role":"assistant","provider":"test","model":"model","usage":{"input":10,"output":20,"cacheRead":30,"cacheWrite":40}}}`,
		`{"type":"message","id":"unknown","timestamp":"2026-07-18T23:00:03Z","message":{"role":"assistant","provider":"test","model":"model","usage":{"input":10,"output":20}}}`,
	}, "\n") + "\n"
	writeFile(t, path, content)

	result, err := New(home).Parse(context.Background(), adapters.Source{Path: path}, adapters.ParseRequest{MachineID: "machine"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Events) != 2 {
		t.Fatalf("events = %d, want 2", len(result.Events))
	}
	if result.Events[0].TotalTokens == nil || *result.Events[0].TotalTokens != 100 || result.Events[0].TokenAccuracy != usage.AccuracyDerived {
		t.Fatalf("complete components did not derive total: %+v", result.Events[0])
	}
	if result.Events[1].TotalTokens != nil || result.Events[1].TokenAccuracy != usage.AccuracyUnknown {
		t.Fatalf("incomplete components invented total: %+v", result.Events[1])
	}
}

func TestParseUsesCommittedCursorAndRetriesIncompleteRecord(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".openclaw", "agents", "main", "sessions", "session.jsonl")
	header := `{"type":"session","id":"session","cwd":"/repo"}` + "\n"
	first := assistantLine("first", "2026-07-18T23:00:01Z", 10)
	writeFile(t, path, header+first)

	adapter := New(home)
	source := discoverOne(t, adapter)
	initial, err := adapter.Parse(context.Background(), source, adapters.ParseRequest{MachineID: "machine"})
	if err != nil {
		t.Fatal(err)
	}
	if len(initial.Events) != 1 {
		t.Fatalf("initial events = %d, want 1", len(initial.Events))
	}

	second := assistantLine("second", "2026-07-18T23:00:02Z", 20)
	appendFile(t, path, second)
	incremental, err := adapter.Parse(context.Background(), source, adapters.ParseRequest{MachineID: "machine", Cursor: initial.Cursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(incremental.Events) != 1 {
		t.Fatalf("incremental events = %d, want 1", len(incremental.Events))
	}
	full, err := adapter.Parse(context.Background(), source, adapters.ParseRequest{MachineID: "machine"})
	if err != nil {
		t.Fatal(err)
	}
	if len(full.Events) != 2 || full.Events[1].EventID != incremental.Events[0].EventID {
		t.Fatalf("cursor changed event identity: incremental=%+v full=%+v", incremental.Events, full.Events)
	}

	partial := `{"type":"message","id":"third","timestamp":"2026-07-18T23:00:03Z","message":{"role":"assistant","provider":"test","model":"model","usage":{"input":30`
	appendFile(t, path, partial)
	waiting, err := adapter.Parse(context.Background(), source, adapters.ParseRequest{MachineID: "machine", Cursor: incremental.Cursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(waiting.Events) != 0 || waiting.Cursor.Offset != incremental.Cursor.Offset {
		t.Fatalf("incomplete record advanced cursor: %+v", waiting)
	}
	appendFile(t, path, `,"output":1,"cacheRead":0,"cacheWrite":0,"totalTokens":31}}}`+"\n")
	retry, err := adapter.Parse(context.Background(), source, adapters.ParseRequest{MachineID: "machine", Cursor: waiting.Cursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(retry.Events) != 1 || retry.Events[0].SessionID != "session" {
		t.Fatalf("completed record was not retried: %+v", retry.Events)
	}
}

func TestParseResetsAfterHeaderReplacement(t *testing.T) {
	home := t.TempDir()
	path := filepath.Join(home, ".openclaw", "agents", "main", "sessions", "session.jsonl")
	writeFile(t, path, `{"type":"session","id":"old-session","cwd":"/old"}`+"\n"+assistantLine("old", "2026-07-18T23:00:01Z", 10))
	adapter := New(home)
	source := discoverOne(t, adapter)
	initial, err := adapter.Parse(context.Background(), source, adapters.ParseRequest{MachineID: "machine"})
	if err != nil {
		t.Fatal(err)
	}

	writeFile(t, path, `{"type":"session","id":"new-session","cwd":"/new"}`+"\n"+assistantLine("new", "2026-07-18T23:00:02Z", 20))
	reset, err := adapter.Parse(context.Background(), source, adapters.ParseRequest{MachineID: "machine", Cursor: initial.Cursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(reset.Events) != 1 || reset.Events[0].SessionID != "new-session" || reset.Events[0].Project != "new" {
		t.Fatalf("replacement did not reset safely: %+v", reset)
	}
}

func discoverOne(t *testing.T, adapter *Adapter) adapters.Source {
	t.Helper()
	sources, err := adapter.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 1 {
		t.Fatalf("sources = %d, want 1: %+v", len(sources), sources)
	}
	return sources[0]
}

func assistantLine(id, timestamp string, tokens int64) string {
	return `{"type":"message","id":"` + id + `","timestamp":"` + timestamp + `","message":{"role":"assistant","provider":"test","model":"model","usage":{"input":` + strconv.FormatInt(tokens, 10) + `,"output":1,"cacheRead":0,"cacheWrite":0,"totalTokens":` + strconv.FormatInt(tokens+1, 10) + `}}}` + "\n"
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func appendFile(t *testing.T, path, content string) {
	t.Helper()
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(content); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func mustOpenClawTime(t *testing.T, value string) time.Time {
	t.Helper()
	parsed, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		t.Fatal(err)
	}
	return parsed
}
