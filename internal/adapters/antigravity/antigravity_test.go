package antigravity

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tokemon/tokemon/internal/adapters"
	"github.com/tokemon/tokemon/internal/usage"
	_ "modernc.org/sqlite"
)

func TestParseGenerationMetadataWithoutReadingConversationContent(t *testing.T) {
	home := t.TempDir()
	root := filepath.Join(home, ".gemini", "antigravity-cli", "conversations")
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, "session-private-title.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE gen_metadata (idx INTEGER PRIMARY KEY, data BLOB, size INTEGER NOT NULL DEFAULT 0);
CREATE TABLE steps (idx INTEGER PRIMARY KEY, step_payload BLOB);
INSERT INTO gen_metadata (idx, data, size) VALUES (?, ?, ?);
INSERT INTO steps (idx, step_payload) VALUES (0, 'secret prompt and response');`, 7, fixtureMetadata(), len(fixtureMetadata())); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	a := New(home)
	sources, err := a.Discover(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(sources) != 1 {
		t.Fatalf("sources = %d, want 1", len(sources))
	}
	result, err := a.Parse(context.Background(), sources[0], adapters.ParseRequest{MachineID: "machine"})
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Events) != 1 {
		t.Fatalf("events = %d, want 1", len(result.Events))
	}
	event := result.Events[0]
	if event.InputTokens == nil || *event.InputTokens != 1200 || event.OutputTokens == nil || *event.OutputTokens != 345 {
		t.Fatalf("unexpected token fields: %+v", event)
	}
	if event.TotalTokens == nil || *event.TotalTokens != 1545 || event.Model != "gemini-3.5-pro" || event.Provider != "google" {
		t.Fatalf("unexpected normalized event: %+v", event)
	}
	if event.Timestamp != time.Date(2026, 7, 12, 21, 30, 0, 123, time.UTC) || event.TokenAccuracy != usage.AccuracyReported {
		t.Fatalf("unexpected timestamp or accuracy: %+v", event)
	}
	if event.Project != "" || event.Metadata != nil || event.Source.Identity == sources[0].Path || event.SessionID != "session-private-title" {
		t.Fatalf("privacy boundary failed: %+v", event)
	}
	if err := event.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestDecodeGenerationMetadataRejectsMalformedData(t *testing.T) {
	if _, ok := decodeGenerationMetadata([]byte("secret transcript")); ok {
		t.Fatal("malformed protobuf was accepted")
	}
}

func fixtureMetadata() []byte {
	stats := message(varintField(1, 345), varintField(2, 1200))
	timestamp := message(varintField(1, uint64(time.Date(2026, 7, 12, 21, 30, 0, 123, time.UTC).Unix())), varintField(2, 123))
	generation := message(bytesField(4, stats), bytesField(9, bytesField(4, timestamp)), bytesField(19, []byte("gemini-3.5-pro")))
	return message(bytesField(1, generation))
}

func message(fields ...[]byte) []byte {
	var result []byte
	for _, field := range fields {
		result = append(result, field...)
	}
	return result
}

func bytesField(field uint64, value []byte) []byte {
	result := encodeVarint(field<<3 | 2)
	result = append(result, encodeVarint(uint64(len(value)))...)
	return append(result, value...)
}

func varintField(field, value uint64) []byte {
	return append(encodeVarint(field<<3), encodeVarint(value)...)
}

func encodeVarint(value uint64) []byte {
	var result []byte
	for value >= 0x80 {
		result = append(result, byte(value)|0x80)
		value >>= 7
	}
	return append(result, byte(value))
}
