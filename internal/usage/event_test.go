package usage

import (
	"math"
	"testing"
	"time"
)

func TestDeterministicID(t *testing.T) {
	left := DeterministicID("machine", "adapter", "file", 42, "2026-07-11T18:42:00Z", "session")
	right := DeterministicID("machine", "adapter", "file", 42, "2026-07-11T18:42:00Z", "session")
	if left != right {
		t.Fatalf("IDs differ: %q != %q", left, right)
	}
	if left[:7] != "sha256:" {
		t.Fatalf("ID lacks sha256 prefix: %q", left)
	}
}

func validEventForTest() Event {
	return Event{
		SchemaVersion: SchemaVersion,
		EventID:       "event-1",
		Timestamp:     time.Now().UTC(),
		MachineID:     "machine",
		Provider:      "provider",
		Model:         "model",
		Tool:          "tool",
		TokenAccuracy: AccuracyUnknown,
		Source:        Source{Adapter: "adapter", AdapterVersion: "1"},
	}
}

func TestValidatePreservesLegitimateUnknowns(t *testing.T) {
	event := validEventForTest()
	if err := event.Validate(); err != nil {
		t.Fatalf("unknown optional values should remain valid: %v", err)
	}
}

func TestValidateRejectsUnsafeValues(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*Event)
	}{
		{"future timestamp", func(event *Event) { event.Timestamp = time.Now().Add(48 * time.Hour) }},
		{"path project", func(event *Event) { event.Project = "/Users/alice/project" }},
		{"currency", func(event *Event) { event.Currency = "usd" }},
		{"infinite cost", func(event *Event) { event.Cost = Float64(math.Inf(1)) }},
		{"inconsistent totals", func(event *Event) {
			event.InputTokens = Int64(3)
			event.TotalTokens = Int64(2)
		}},
		{"sensitive metadata", func(event *Event) { event.Metadata = map[string]any{"prompt": "do not store"} }},
		{"oversized metadata", func(event *Event) {
			event.Metadata = map[string]any{"note": string(make([]byte, MaxMetadataStringSize+1))}
		}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			event := validEventForTest()
			test.mutate(&event)
			if err := event.Validate(); err == nil {
				t.Fatal("Validate accepted an unsafe value")
			}
		})
	}
}

func TestNormalizeProjectDropsPathAndCase(t *testing.T) {
	for input, want := range map[string]string{
		"/Users/alice/repos/Carteakey.dev": `carteakey.dev`,
		`C:\\Users\\alice\\repos\\TOKEMON`: `tokemon`,
		"project-name":                     `project-name`,
		"/":                                ``,
	} {
		if got := NormalizeProject(input); got != want {
			t.Errorf("NormalizeProject(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestValidateRejectsUnknownAccuracy(t *testing.T) {
	event := Event{
		SchemaVersion: SchemaVersion,
		EventID:       "event-1",
		Timestamp:     time.Now(),
		MachineID:     "machine",
		Provider:      "provider",
		Model:         "model",
		Tool:          "tool",
		TokenAccuracy: "not-valid",
	}
	if err := event.Validate(); err == nil {
		t.Fatal("Validate accepted an unknown accuracy")
	}
}
