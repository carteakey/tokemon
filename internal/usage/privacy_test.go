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
		"model path":          func(event *Event) { event.Model = "/private/repository/model" },
		"provider credential": func(event *Event) { event.Provider = "sk-private-token" },
		"session path":        func(event *Event) { event.SessionID = `C:\\Users\\private` },
		"session title":       func(event *Event) { event.SessionID = "private session title" },
		"session relative":    func(event *Event) { event.SessionID = "../private-session" },
		"session credential":  func(event *Event) { event.SessionID = "ghp_private-token" },
	} {
		event := base
		mutate(&event)
		if err := ValidateOutbound(event); err == nil || !strings.Contains(err.Error(), "disallowed sensitive content") {
			t.Errorf("%s error = %v", name, err)
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
