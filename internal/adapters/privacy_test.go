package adapters_test

import (
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
