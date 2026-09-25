package adapters_test

import (
	"strings"
	"testing"
	"time"

	"github.com/tokemon/tokemon/internal/adapters/builtin"
	"github.com/tokemon/tokemon/internal/usage"
)

// Provider-specific tests exercise adversarial local records in each adapter
// package. This registry-level table ensures every built-in output still
// passes the one outbound privacy boundary before it can reach an HTTP client.
func TestBuiltInAdapterOutputsUseOnlyApprovedEventFields(t *testing.T) {
	for _, definition := range builtin.Definitions() {
		event := usage.Event{
			SchemaVersion: usage.SchemaVersion,
			EventID:       "fixture-" + definition.ID,
			Timestamp:     time.Unix(1, 0).UTC(),
			MachineID:     "fixture-machine",
			Provider:      "fixture-provider",
			Model:         "fixture-model",
			Tool:          definition.ID,
			TokenAccuracy: usage.AccuracyReported,
			Source:        usage.Source{Adapter: definition.ID, AdapterVersion: definition.Version, Identity: "sha256:fixture"},
		}
		if err := usage.ValidateOutbound(event); err != nil {
			t.Errorf("%s output rejected by outbound guard: %v", definition.ID, err)
		}
	}
}

func TestBuiltInSessionIDsNormalizeAdversarialValues(t *testing.T) {
	for _, definition := range builtin.Definitions() {
		adapter := definition.New(builtin.Config{Home: t.TempDir()})
		if !adapter.Capabilities().SessionID {
			continue
		}
		for _, raw := range []string{
			"/private/" + definition.ID + "/session-title",
			"private session title",
			"ghp_" + definition.ID + "-credential",
		} {
			event := usage.Event{
				SchemaVersion: usage.SchemaVersion,
				EventID:       "fixture-" + definition.ID,
				Timestamp:     time.Unix(1, 0).UTC(),
				MachineID:     "fixture-machine",
				SessionID:     usage.NormalizeSessionID(raw),
				Provider:      "fixture-provider",
				Model:         "fixture-model",
				Tool:          definition.ID,
				TokenAccuracy: usage.AccuracyReported,
				Source:        usage.Source{Adapter: definition.ID, AdapterVersion: definition.Version, Identity: "sha256:fixture"},
			}
			if !strings.HasPrefix(event.SessionID, "sha256:") {
				t.Errorf("%s did not opaque-hash adversarial session ID %q: %q", definition.ID, raw, event.SessionID)
			}
			if err := usage.ValidateOutbound(event); err != nil {
				t.Errorf("%s normalized session ID rejected by outbound guard: %v", definition.ID, err)
			}
		}
	}
}
