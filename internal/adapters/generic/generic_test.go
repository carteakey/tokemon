package generic

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

func TestDiscoverUsesOnlyConfiguredPathsAndGlobs(t *testing.T) {
	root := t.TempDir()
	configured := filepath.Join(root, "configured")
	nested := filepath.Join(configured, "nested")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	first := filepath.Join(configured, "one.jsonl")
	second := filepath.Join(nested, "two.jsonl")
	if err := os.WriteFile(first, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, nil, 0o600); err != nil {
		t.Fatal(err)
	}

	sources, err := New(filepath.Join(configured, "*.jsonl")).Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 1 || sources[0].Path != first || sources[0].Identity == "" {
		t.Fatalf("unexpected sources: %+v", sources)
	}
	if strings.Contains(sources[0].Identity, root) {
		t.Fatalf("source identity leaked its path: %s", sources[0].Identity)
	}
}

func TestParseAdvancesIncrementallyAndCreatesDeterministicIDs(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	first := eventLine("", "2026-07-12T12:00:00Z", usage.Int64(42), usage.AccuracyReported)
	if err := os.WriteFile(path, []byte(first), 0o600); err != nil {
		t.Fatal(err)
	}
	adapter := New(path)
	source := discoverOne(t, adapter)
	result, err := adapter.Parse(context.Background(), source, adapters.ParseRequest{MachineID: "mac"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Events) != 1 || result.Events[0].EventID == "" || result.Events[0].MachineID != "mac" {
		t.Fatalf("unexpected first result: %+v", result)
	}
	firstID := result.Events[0].EventID
	if result.Cursor.Identity != source.Identity || result.Cursor.Offset != int64(len(first)) {
		t.Fatalf("unexpected cursor: %+v", result.Cursor)
	}

	second := eventLine("provided-id", "2026-07-12T12:01:00Z", nil, usage.AccuracyUnknown)
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

	result2, err := adapter.Parse(context.Background(), source, adapters.ParseRequest{MachineID: "mac", Cursor: result.Cursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(result2.Events) != 1 || result2.Events[0].EventID != "provided-id" || result2.Events[0].TotalTokens != nil {
		t.Fatalf("unexpected incremental result: %+v", result2)
	}
	rescan, err := adapter.Parse(context.Background(), source, adapters.ParseRequest{MachineID: "mac"})
	if err != nil {
		t.Fatal(err)
	}
	if len(rescan.Events) != 2 || rescan.Events[0].EventID != firstID {
		t.Fatalf("IDs changed after rescan: %+v", rescan.Events)
	}
}

func TestParseRejectsMalformedRecordsWithoutAdvancing(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	content := eventLine("", "2026-07-12T12:00:00Z", usage.Int64(1), usage.AccuracyReported) + "{not-json}\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	adapter := New(path)
	result, err := adapter.Parse(context.Background(), discoverOne(t, adapter), adapters.ParseRequest{MachineID: "mac"})
	if err == nil || len(result.Events) != 0 || result.Cursor.Offset != 0 {
		t.Fatalf("malformed record advanced source: result=%+v err=%v", result, err)
	}
}

func TestParseResetsAfterTruncation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	long := eventLine("", "2026-07-12T12:00:00Z", usage.Int64(1000000), usage.AccuracyReported)
	if err := os.WriteFile(path, []byte(long), 0o600); err != nil {
		t.Fatal(err)
	}
	adapter := New(path)
	source := discoverOne(t, adapter)
	initial, err := adapter.Parse(context.Background(), source, adapters.ParseRequest{MachineID: "mac"})
	if err != nil {
		t.Fatal(err)
	}
	short := eventLine("", "2026-07-12T12:02:00Z", usage.Int64(1), usage.AccuracyReported)
	if err := os.WriteFile(path, []byte(short), 0o600); err != nil {
		t.Fatal(err)
	}
	reset, err := adapter.Parse(context.Background(), source, adapters.ParseRequest{MachineID: "mac", Cursor: initial.Cursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(reset.Events) != 1 || reset.Events[0].TotalTokens == nil || *reset.Events[0].TotalTokens != 1 {
		t.Fatalf("truncated source did not reset: %+v", reset)
	}
}

func TestParseResetsAfterReplacement(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	first := eventLine("", "2026-07-12T12:00:00Z", usage.Int64(10), usage.AccuracyReported)
	if err := os.WriteFile(path, []byte(first), 0o600); err != nil {
		t.Fatal(err)
	}
	adapter := New(path)
	oldSource := discoverOne(t, adapter)
	initial, err := adapter.Parse(context.Background(), oldSource, adapters.ParseRequest{MachineID: "mac"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	replacement := eventLine("", "2026-07-12T12:03:00Z", usage.Int64(20), usage.AccuracyReported)
	if err := os.WriteFile(path, []byte(replacement), 0o600); err != nil {
		t.Fatal(err)
	}
	newSource := discoverOne(t, adapter)
	if newSource.Identity == oldSource.Identity {
		t.Skip("filesystem reused the same file identity")
	}
	reset, err := adapter.Parse(context.Background(), newSource, adapters.ParseRequest{MachineID: "mac", Cursor: initial.Cursor})
	if err != nil {
		t.Fatal(err)
	}
	if len(reset.Events) != 1 || reset.Events[0].TotalTokens == nil || *reset.Events[0].TotalTokens != 20 {
		t.Fatalf("replaced source did not reset: %+v", reset)
	}
}

func TestParseRejectsSensitiveAndUnknownFieldsBeforeForwarding(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.jsonl")
	line := `{"schema_version":"1","event_id":"sensitive","timestamp":"2026-07-12T12:00:00Z","provider":"example","model":"example-model","tool":"example-tool","total_tokens":42,"token_accuracy":"reported","prompt":"private prompt","metadata":{"tokemon_usage_kind":"batch"}}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := New(path).Parse(context.Background(), discoverOne(t, New(path)), adapters.ParseRequest{MachineID: "machine"})
	if err == nil || !strings.Contains(err.Error(), "disallowed sensitive content") {
		t.Fatalf("sensitive generic record error = %v", err)
	}

	approved := usage.Event{
		SchemaVersion: usage.SchemaVersion,
		EventID:       "approved",
		Timestamp:     mustTime("2026-07-12T12:00:00Z"),
		Provider:      "example",
		Model:         "example-model",
		Tool:          "example-tool",
		TotalTokens:   usage.Int64(42),
		TokenAccuracy: usage.AccuracyReported,
		Metadata:      map[string]any{"tokemon_usage_kind": "batch"},
	}
	encoded, err := json.Marshal(approved)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(encoded, '\n'), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := New(path).Parse(context.Background(), discoverOne(t, New(path)), adapters.ParseRequest{MachineID: "machine"})
	if err != nil || len(result.Events) != 1 {
		t.Fatalf("approved generic record = %+v, error = %v", result.Events, err)
	}
	if result.Events[0].Metadata["tokemon_usage_kind"] != "batch" {
		t.Fatalf("approved metadata was not preserved: %+v", result.Events[0].Metadata)
	}
}

func discoverOne(t *testing.T, adapter *Adapter) adapters.Source {
	t.Helper()
	sources, err := adapter.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 1 {
		t.Fatalf("sources = %d, want 1", len(sources))
	}
	return sources[0]
}

func eventLine(id, timestamp string, total *int64, accuracy usage.Accuracy) string {
	event := usage.Event{
		SchemaVersion: usage.SchemaVersion,
		EventID:       id,
		Timestamp:     mustTime(timestamp),
		Provider:      "example",
		Model:         "example-model",
		Tool:          "example-tool",
		TotalTokens:   total,
		Currency:      "USD",
		TokenAccuracy: accuracy,
	}
	encoded, err := json.Marshal(event)
	if err != nil {
		panic(err)
	}
	return string(encoded) + "\n"
}

func mustTime(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		panic(err)
	}
	return parsed
}
