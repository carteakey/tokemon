package usage

import (
	"strings"
	"testing"
	"time"
)

func TestValidateOutboundRejectsPathsAndSensitiveMetadataWithoutMutation(t *testing.T) {
	event := Event{
		SchemaVersion: SchemaVersion,
		EventID:       "privacy-fixture",
		Timestamp:     time.Unix(1, 0).UTC(),
		MachineID:     "machine",
		Provider:      "provider",
		Model:         "model",
		Tool:          "generic-jsonl",
		Project:       "/private/repository",
		Metadata:      map[string]any{"tokemon_prompt": "private prompt"},
	}
	if err := ValidateOutbound(event); err == nil || !strings.Contains(err.Error(), "disallowed sensitive content") {
		t.Fatalf("privacy guard error = %v", err)
	}
	if event.Project != "/private/repository" || event.Metadata["tokemon_prompt"] != "private prompt" {
		t.Fatalf("privacy guard mutated rejected event: %+v", event)
	}
}

func TestValidateOutboundRejectsCredentialOrPathLikeIdentifiers(t *testing.T) {
	base := Event{SchemaVersion: SchemaVersion, EventID: "event", Timestamp: time.Unix(1, 0).UTC(), MachineID: "machine", Provider: "provider", Model: "model", Tool: "tool"}
	for name, mutate := range map[string]func(*Event){
		"model path":                 func(event *Event) { event.Model = "/private/repository/model" },
		"provider credential":        func(event *Event) { event.Provider = "sk-private-token" },
		"session path":               func(event *Event) { event.SessionID = `C:\\Users\\private` },
		"session title":              func(event *Event) { event.SessionID = "private session title" },
		"session relative":           func(event *Event) { event.SessionID = "../private-session" },
		"session credential":         func(event *Event) { event.SessionID = "ghp_private-token" },
		"session sk prefix":          func(event *Event) { event.SessionID = "sk-private-token" },
		"session sk after separator": func(event *Event) { event.SessionID = "prefix_sk-secret-key" },
		"session sk after hyphen":    func(event *Event) { event.SessionID = "prefix-sk-secret-key" },
		"event id sk after slash":    func(event *Event) { event.EventID = "foo/sk-secret" },
		"model lowercase bearer":     func(event *Event) { event.Model = "bearer secret-token" },
		"provider mixed bearer":      func(event *Event) { event.Provider = "auth-BeArEr xyz" },
		"model github_pat":           func(event *Event) { event.Model = "github_pat_11ABC_secret" },
		"event id github_pat":        func(event *Event) { event.EventID = "tok_github_pat_11ABC_secret" },
		"model uppercase sk":         func(event *Event) { event.Model = "SK-proj-secret" },
		"metadata lowercase bearer":  func(event *Event) { event.Metadata = map[string]any{"tokemon_note": "bearer secret123"} },
		"metadata github_pat":        func(event *Event) { event.Metadata = map[string]any{"tokemon_note": "prefix-github_pat_11XYZ"} },
	} {
		event := base
		mutate(&event)
		if err := ValidateOutbound(event); err == nil || !strings.Contains(err.Error(), "disallowed sensitive content") {
			t.Errorf("%s error = %v", name, err)
		}
	}
}

func TestValidateOutboundAllowsDeskAndTaskSessionIdentifiers(t *testing.T) {
	base := Event{
		SchemaVersion: SchemaVersion,
		EventID:       "event-desk-1",
		Timestamp:     time.Unix(1, 0).UTC(),
		MachineID:     "machine",
		Provider:      "anthropic",
		Model:         "claude-sonnet-4-5",
		Tool:          "claude-desktop",
		TokenAccuracy: AccuracyReported,
		Source:        Source{Adapter: "claude-code", AdapterVersion: "1"},
	}
	for _, sid := range []string{"claude-desk-001", "af-desk-1", "task-42", "disk-check-9"} {
		event := base
		event.SessionID = sid
		if err := ValidateOutbound(event); err != nil {
			t.Errorf("ValidateOutbound(%q) unexpectedly rejected: %v", sid, err)
		}
	}
}

func TestFilterApprovedMetadataPreservesNamespacedUsageMetadata(t *testing.T) {
	metadata := map[string]any{"tokemon_usage_kind": "batch", "tokemon_cache_hit": true}
	filtered, err := FilterApprovedMetadata(metadata)
	if err != nil {
		t.Fatal(err)
	}
	if len(filtered) != len(metadata) || filtered["tokemon_usage_kind"] != "batch" || filtered["tokemon_cache_hit"] != true {
		t.Fatalf("filtered metadata = %#v", filtered)
	}
	if _, err := FilterApprovedMetadata(map[string]any{"unrelated": "do not forward"}); err == nil {
		t.Fatal("unrelated metadata was accepted")
	}
}

func TestValidateOutboundAllowsHashedSessionFallback(t *testing.T) {
	event := Event{
		SchemaVersion: SchemaVersion,
		EventID:       "privacy-hashed-session",
		Timestamp:     time.Unix(1, 0).UTC(),
		MachineID:     "machine",
		SessionID:     HashSessionID("/private/title.jsonl"),
		Provider:      "provider",
		Model:         "model",
		Tool:          "tool",
		TokenAccuracy: AccuracyUnknown,
		Source:        Source{Adapter: "adapter", AdapterVersion: "1"},
	}
	if err := ValidateOutbound(event); err != nil {
		t.Fatalf("hashed fallback rejected: %v", err)
	}
}
