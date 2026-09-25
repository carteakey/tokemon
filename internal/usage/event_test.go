package usage

import (
	"strings"
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

func TestValidateRejectsBadEventIDFormats(t *testing.T) {
	for _, eventID := range []string{"", "with space", "sha256:short", "sha256:" + strings.Repeat("x", 64), strings.Repeat("a", maxEventIDLength+1)} {
		event := validEvent()
		event.EventID = eventID
		if err := event.Validate(); err == nil {
			t.Errorf("Validate accepted event_id %q", eventID)
		}
	}
}

func TestValidateRejectsFutureAndAncientTimestamps(t *testing.T) {
	future := validEvent()
	future.Timestamp = time.Now().Add(2 * time.Hour)
	if err := future.Validate(); err == nil {
		t.Fatal("Validate accepted a future timestamp")
	}
	ancient := validEvent()
	ancient.Timestamp = time.Date(1999, 12, 31, 23, 59, 59, 0, time.UTC)
	if err := ancient.Validate(); err == nil {
		t.Fatal("Validate accepted an ancient timestamp")
	}
}

func TestValidateRejectsOversizedFields(t *testing.T) {
	event := validEvent()
	event.MachineID = strings.Repeat("m", maxMachineIDLength+1)
	if err := event.Validate(); err == nil {
		t.Fatal("Validate accepted an oversized machine_id")
	}
	event = validEvent()
	event.Provider = strings.Repeat("p", maxProviderLength+1)
	if err := event.Validate(); err == nil {
		t.Fatal("Validate accepted an oversized provider")
	}
	event = validEvent()
	event.Model = strings.Repeat("m", maxModelLength+1)
	if err := event.Validate(); err == nil {
		t.Fatal("Validate accepted an oversized model")
	}
	event = validEvent()
	event.SessionID = strings.Repeat("s", maxSessionIDLength+1)
	if err := event.Validate(); err == nil {
		t.Fatal("Validate accepted an oversized session_id")
	}
}

func TestValidateRejectsBadCurrencies(t *testing.T) {
	for _, currency := range []string{"usd", "US", "USDD", "US1"} {
		event := validEvent()
		event.Currency = currency
		if err := event.Validate(); err == nil {
			t.Errorf("Validate accepted currency %q", currency)
		}
	}
	event := validEvent()
	event.Currency = "USD"
	if err := event.Validate(); err != nil {
		t.Fatalf("Validate rejected a valid currency: %v", err)
	}
}

func TestValidateRejectsInconsistentTokenComponents(t *testing.T) {
	event := validEvent()
	event.InputTokens = Int64(60)
	event.OutputTokens = Int64(30)
	event.TotalTokens = Int64(50)
	if err := event.Validate(); err == nil {
		t.Fatal("Validate accepted components exceeding the reported total")
	}
	event = validEvent()
	event.InputTokens = Int64(10)
	event.OutputTokens = Int64(5)
	event.CacheReadTokens = Int64(2)
	event.ReasoningTokens = Int64(3)
	event.TotalTokens = Int64(20)
	if err := event.Validate(); err != nil {
		t.Fatalf("Validate rejected holistically reported components: %v", err)
	}
}

func TestValidateRejectsMetadataPayloads(t *testing.T) {
	event := validEvent()
	event.Metadata = map[string]any{"prompt": "do not upload this"}
	if err := event.Validate(); err == nil {
		t.Fatal("Validate accepted an event metadata payload")
	}
	if sanitized := SanitizeOutbound(event); sanitized.Metadata != nil {
		t.Fatal("SanitizeOutbound retained event metadata")
	}
}

func TestSanitizeOutboundPreservesComponentsAndDropsImpossibleTotal(t *testing.T) {
	event := validEvent()
	event.InputTokens = Int64(3)
	event.CacheReadTokens = Int64(10)
	event.TotalTokens = Int64(5)
	event.TokenAccuracy = AccuracyReported

	sanitized := SanitizeOutbound(event)
	if sanitized.TotalTokens != nil {
		t.Fatalf("total_tokens = %d, want unknown", *sanitized.TotalTokens)
	}
	if sanitized.InputTokens == nil || *sanitized.InputTokens != 3 || sanitized.CacheReadTokens == nil || *sanitized.CacheReadTokens != 10 {
		t.Fatalf("reported components changed: %+v", sanitized)
	}
	if sanitized.TokenAccuracy != AccuracyUnknown {
		t.Fatalf("token_accuracy = %q, want %q", sanitized.TokenAccuracy, AccuracyUnknown)
	}
	if err := sanitized.Validate(); err != nil {
		t.Fatalf("sanitized event remains invalid: %v", err)
	}
}

func validEvent() Event {
	return Event{
		SchemaVersion: SchemaVersion,
		EventID:       "event-1",
		Timestamp:     time.Now().Add(-time.Hour),
		MachineID:     "machine",
		Provider:      "provider",
		Model:         "model",
		Tool:          "tool",
		TokenAccuracy: AccuracyReported,
	}
}
