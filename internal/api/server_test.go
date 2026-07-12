package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tokemon/tokemon/internal/catalog"
	"github.com/tokemon/tokemon/internal/database"
	"github.com/tokemon/tokemon/internal/usage"
)

func TestBatchIngestRequiresTokenAndUpdatesEvolution(t *testing.T) {
	store, err := database.Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server, err := New(store, "secret")
	if err != nil {
		t.Fatal(err)
	}
	event := usage.Event{
		SchemaVersion: usage.SchemaVersion,
		EventID:       "api-event-1",
		Timestamp:     time.Date(2026, 7, 11, 18, 42, 0, 0, time.UTC),
		MachineID:     "machine",
		Provider:      "provider",
		Model:         "model",
		Tool:          "generic-jsonl",
		TotalTokens:   usage.Int64(10),
		TokenAccuracy: usage.AccuracyReported,
		Source:        usage.Source{Adapter: "generic-jsonl", AdapterVersion: "test"},
	}
	payload, err := json.Marshal(batchRequest{Events: []usage.Event{event}})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/events/batch", bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	unauthorized := httptest.NewRecorder()
	server.Handler().ServeHTTP(unauthorized, request)
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d, want %d", unauthorized.Code, http.StatusUnauthorized)
	}

	request = httptest.NewRequest(http.MethodPost, "/api/v1/events/batch", bytes.NewReader(payload))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Authorization", "Bearer secret")
	accepted := httptest.NewRecorder()
	server.Handler().ServeHTTP(accepted, request)
	if accepted.Code != http.StatusOK {
		t.Fatalf("accepted status = %d, want %d: %s", accepted.Code, accepted.Code, accepted.Body.String())
	}
	var result database.IngestResult
	if err := json.Unmarshal(accepted.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Accepted != 1 || result.CurrentStage != 1 || !result.Evolved {
		t.Fatalf("unexpected ingest result: %+v", result)
	}

	evolutionRequest := httptest.NewRequest(http.MethodGet, "/api/v1/evolution", nil).WithContext(context.Background())
	evolutionResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(evolutionResponse, evolutionRequest)
	if evolutionResponse.Code != http.StatusOK || !bytes.Contains(evolutionResponse.Body.Bytes(), []byte(`"stage":1`)) {
		t.Fatalf("unexpected evolution response: %d %s", evolutionResponse.Code, evolutionResponse.Body.String())
	}
}

func TestDashboardRendersDailyTokenActivityField(t *testing.T) {
	store, err := database.Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server, err := New(store, "")
	if err != nil {
		t.Fatal(err)
	}

	event := usage.Event{
		SchemaVersion: usage.SchemaVersion,
		EventID:       "activity-render-1",
		Timestamp:     time.Now().UTC().Add(-24 * time.Hour),
		MachineID:     "machine",
		Provider:      "provider",
		Model:         "model",
		Tool:          "tool",
		TotalTokens:   usage.Int64(42),
		TokenAccuracy: usage.AccuracyReported,
		Source:        usage.Source{Adapter: "test", AdapterVersion: "1"},
	}
	if _, err := store.Ingest(context.Background(), []usage.Event{event}); err != nil {
		t.Fatal(err)
	}

	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("dashboard status = %d, want %d", response.Code, http.StatusOK)
	}
	body := response.Body.String()
	for _, want := range []string{"Token activity", "last 53 weeks", "activity-cell", "42 tokens", "BY MODEL", "BY PROVIDER", "model · 42 tokens", "provider · 42 tokens"} {
		if !bytes.Contains(response.Body.Bytes(), []byte(want)) {
			t.Fatalf("dashboard does not contain %q: %s", want, body)
		}
	}
}

func TestDashboardRendersAPIEquivalentCostAndCoverage(t *testing.T) {
	inputPrice := 10.0
	outputPrice := 0.0
	modelCatalog := catalog.MustNew(map[string]catalog.Model{
		"priced-model": {Provider: "test", DisplayName: "Priced Model", Aliases: []string{"priced"}, Pricing: catalog.Pricing{Currency: "USD", Mode: "standard", VerifiedAt: "2026-07-12", Source: "https://example.com/pricing", InputPricePerMillion: &inputPrice, OutputPricePerMillion: &outputPrice}},
	})
	store, err := database.Open(t.TempDir()+"/tokemon.db", modelCatalog)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server, err := New(store, "")
	if err != nil {
		t.Fatal(err)
	}

	events := []usage.Event{
		{SchemaVersion: usage.SchemaVersion, EventID: "priced", Timestamp: time.Now().UTC(), MachineID: "machine", Provider: "provider", Model: "priced", Tool: "tool", InputTokens: usage.Int64(1_000_000), TotalTokens: usage.Int64(1_000_000), TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "test", AdapterVersion: "1"}},
		{SchemaVersion: usage.SchemaVersion, EventID: "unpriced", Timestamp: time.Now().UTC(), MachineID: "machine", Provider: "provider", Model: "unknown", Tool: "tool", TotalTokens: usage.Int64(500_000), TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "test", AdapterVersion: "1"}},
	}
	if _, err := store.Ingest(context.Background(), events); err != nil {
		t.Fatal(err)
	}

	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	for _, want := range []string{"API-equivalent cost", "Est. $10.00", "67% of known tokens", "not your subscription bill"} {
		if !bytes.Contains(response.Body.Bytes(), []byte(want)) {
			t.Fatalf("dashboard does not contain %q: %s", want, response.Body.String())
		}
	}
}

func TestDashboardRendersMergedProjectUsage(t *testing.T) {
	store, err := database.Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server, err := New(store, "")
	if err != nil {
		t.Fatal(err)
	}
	events := []usage.Event{
		{SchemaVersion: usage.SchemaVersion, EventID: "one", Timestamp: time.Now().UTC(), MachineID: "laptop", Project: "Carteakey.dev", Provider: "openai", Model: "model", Tool: "codex", TotalTokens: usage.Int64(100), TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "test", AdapterVersion: "1"}},
		{SchemaVersion: usage.SchemaVersion, EventID: "two", Timestamp: time.Now().UTC(), MachineID: "desktop", Project: "carteakey.dev", Provider: "openai", Model: "model", Tool: "codex", TotalTokens: usage.Int64(50), TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "test", AdapterVersion: "1"}},
	}
	if _, err := store.Ingest(context.Background(), events); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	for _, want := range []string{"Usage by project", "Merged across machines", "carteakey.dev", "150"} {
		if !bytes.Contains(response.Body.Bytes(), []byte(want)) {
			t.Fatalf("dashboard does not contain %q: %s", want, response.Body.String())
		}
	}
}
