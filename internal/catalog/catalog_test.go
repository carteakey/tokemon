package catalog

import (
	"path/filepath"
	"testing"

	"github.com/tokemon/tokemon/internal/usage"
)

func TestResolveAndEstimate(t *testing.T) {
	inputPrice := 10.0
	outputPrice := 20.0
	catalog := MustNew(map[string]Model{
		"test-model": {
			Provider: "test", DisplayName: "Test Model", Aliases: []string{"provider/raw-model"},
			Pricing: Pricing{Currency: "USD", Mode: "standard", VerifiedAt: "2026-07-12", Source: "https://example.com/pricing", InputPricePerMillion: &inputPrice, OutputPricePerMillion: &outputPrice},
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

func TestEstimateRejectsAggregateOnlyTokens(t *testing.T) {
	inputPrice := 10.0
	model := Model{Pricing: Pricing{InputPricePerMillion: &inputPrice}}
	cost, ok := Estimate(usage.Event{TotalTokens: usage.Int64(1_000_000)}, model)
	if ok || cost != nil {
		t.Fatalf("aggregate-only usage must remain unpriced: %v, %v", cost, ok)
	}
}

func TestBundledCatalogResolvesCurrentCodingModels(t *testing.T) {
	catalog, err := Load(filepath.Join("..", "..", "catalog", "models.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	tests := []struct {
		alias      string
		canonical  string
		inputPrice float64
	}{
		{alias: "gpt-5.5", canonical: "gpt-5.5", inputPrice: 5},
		{alias: "gpt-5.6-sol", canonical: "gpt-5.6-sol", inputPrice: 5},
		{alias: "gpt-5.6-terra", canonical: "gpt-5.6-terra", inputPrice: 2.5},
		{alias: "gpt-5.6-luna", canonical: "gpt-5.6-luna", inputPrice: 1},
		{alias: "claude-opus-4-7", canonical: "claude-opus-4.5-plus", inputPrice: 5},
		{alias: "claude-sonnet-4-6", canonical: "claude-sonnet-4", inputPrice: 3},
	}
	for _, test := range tests {
		id, model, ok := catalog.Resolve(test.alias)
		if !ok || id != test.canonical || model.Pricing.InputPricePerMillion == nil || *model.Pricing.InputPricePerMillion != test.inputPrice {
			t.Errorf("Resolve(%q) = %q, %+v, %v", test.alias, id, model, ok)
		}
	}
}

func TestNewRejectsAliasConflicts(t *testing.T) {
	price := 1.0
	model := func(name string, aliases ...string) Model {
		return Model{Provider: "test", DisplayName: name, Aliases: aliases, Pricing: Pricing{Currency: "USD", Mode: "standard", VerifiedAt: "2026-07-12", Source: "https://example.com/pricing", InputPricePerMillion: &price, OutputPricePerMillion: &price}}
	}
	_, err := New(map[string]Model{
		"first":  model("First", "shared"),
		"second": model("Second", "shared"),
	})
	if err == nil || err.Error() != `model "second" alias "shared" conflicts with model "first"` {
		t.Fatalf("unexpected conflict error: %v", err)
	}
}

func TestNewValidatesPricingProvenance(t *testing.T) {
	price := 1.0
	_, err := New(map[string]Model{
		"model": {Provider: "test", DisplayName: "Model", Aliases: []string{"raw"}, Pricing: Pricing{Currency: "USD", Mode: "standard", InputPricePerMillion: &price, OutputPricePerMillion: &price}},
	})
	if err == nil || err.Error() != `model "model" verified_at: date is required` {
		t.Fatalf("unexpected provenance error: %v", err)
	}
}
