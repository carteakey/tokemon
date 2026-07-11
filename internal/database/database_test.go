package database

import (
	"context"
	"testing"
	"time"

	"github.com/tokemon/tokemon/internal/catalog"
	"github.com/tokemon/tokemon/internal/usage"
)

func TestIngestIsIdempotentAndDerivesEvolution(t *testing.T) {
	inputPrice, outputPrice := 10.0, 20.0
	modelCatalog := catalog.New(map[string]catalog.Model{
		"test-model": {
			Aliases:               []string{"raw-model"},
			InputPricePerMillion:  &inputPrice,
			OutputPricePerMillion: &outputPrice,
		},
	})
	store, err := Open(t.TempDir()+"/tokemon.db", modelCatalog)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	event := usage.Event{
		SchemaVersion: usage.SchemaVersion,
		EventID:       "event-1",
		Timestamp:     time.Date(2026, 7, 11, 18, 42, 0, 0, time.UTC),
		MachineID:     "machine-1",
		Provider:      "test-provider",
		Model:         "raw-model",
		Tool:          "generic-jsonl",
		InputTokens:   usage.Int64(2),
		OutputTokens:  usage.Int64(8),
		TotalTokens:   usage.Int64(10),
		TokenAccuracy: usage.AccuracyReported,
		Source:        usage.Source{Adapter: "generic-jsonl", AdapterVersion: "test"},
	}
	result, err := store.Ingest(context.Background(), []usage.Event{event})
	if err != nil {
		t.Fatal(err)
	}
	if result.Accepted != 1 || result.CurrentTotal != 10 || !result.Evolved || result.CurrentStage != 1 {
		t.Fatalf("unexpected first ingest: %+v", result)
	}
	result, err = store.Ingest(context.Background(), []usage.Event{event})
	if err != nil {
		t.Fatal(err)
	}
	if result.Duplicates != 1 || result.Accepted != 0 || result.CurrentTotal != 10 {
		t.Fatalf("unexpected duplicate ingest: %+v", result)
	}
	events, err := store.Events(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].CanonicalModel != "test-model" || events[0].Cost == nil {
		t.Fatalf("unexpected round-trip events: %+v", events)
	}
}
