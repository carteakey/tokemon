package usage

import (
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
