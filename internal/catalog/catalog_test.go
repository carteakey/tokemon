package catalog

import (
	"testing"

	"github.com/tokemon/tokemon/internal/usage"
)

func TestResolveAndEstimate(t *testing.T) {
	inputPrice := 10.0
	outputPrice := 20.0
	catalog := New(map[string]Model{
		"test-model": {
			DisplayName:           "Test Model",
			Aliases:               []string{"provider/raw-model"},
			InputPricePerMillion:  &inputPrice,
			OutputPricePerMillion: &outputPrice,
		},
	})
	id, model, ok := catalog.Resolve("provider/raw-model")
	if !ok || id != "test-model" || model.DisplayName != "Test Model" {
		t.Fatalf("unexpected resolve result: %q, %+v, %v", id, model, ok)
	}
	event := usage.Event{InputTokens: usage.Int64(1_000_000), OutputTokens: usage.Int64(500_000)}
	cost, ok := Estimate(event, model)
	if !ok || cost == nil || *cost != 20 {
		t.Fatalf("unexpected estimate: %v, %v", cost, ok)
	}
}
