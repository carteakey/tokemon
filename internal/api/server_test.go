package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
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
	for _, want := range []string{"Token activity", "last 53 weeks", "activity-cell", "grid-template-rows: 12px repeat(7, 12px)", "height: 12px", "42 tokens", "BY MODEL", "BY PROVIDER", "model · 42 tokens", "provider · 42 tokens"} {
		if !bytes.Contains(response.Body.Bytes(), []byte(want)) {
			t.Fatalf("dashboard does not contain %q: %s", want, body)
		}
	}
}

func TestDashboardDoesNotDuplicateStageWatermarkBesideFormChip(t *testing.T) {
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
		EventID:       "stage-nine-render",
		Timestamp:     time.Now().UTC(),
		MachineID:     "machine",
		Provider:      "provider",
		Model:         "model",
		Tool:          "tool",
		TotalTokens:   usage.Int64(1_000_000_000),
		TokenAccuracy: usage.AccuracyReported,
		Source:        usage.Source{Adapter: "test", AdapterVersion: "1"},
	}
	if _, err := store.Ingest(context.Background(), []usage.Event{event}); err != nil {
		t.Fatal(err)
	}

	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	body := response.Body.String()
	if !strings.Contains(body, `<div class="stage-chip">FORM / 09</div>`) {
		t.Fatalf("dashboard does not render the stage-nine form chip")
	}
	if strings.Contains(body, `.creature-panel::after`) || strings.Contains(body, `content: "STAGE " attr(data-stage)`) {
		t.Fatalf("dashboard still renders the duplicate stage watermark")
	}
}

func TestDashboardPollsAndUpdatesLifetimeCounter(t *testing.T) {
	store, err := database.Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server, err := New(store, "")
	if err != nil {
		t.Fatal(err)
	}

	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	for _, want := range []string{
		`id="lifetime-counter"`,
		`id="token-composition"`,
		`class="composition-meter"`,
		`id="live-cached-segment"`,
		`class="composition-primary"`,
		`class="composition-item input"`,
		`class="composition-item output"`,
		`class="mini-odometer"`,
		`id="live-input-tokens"`,
		`id="live-output-tokens"`,
		`data-tooltip-kind="composition"`,
		`aria-label="Token mix"`,
		`>Input</span>`,
		`>Output</span>`,
		`.odometer-reel::after`,
		`.odometer-reel { flex-basis: .8em; }`,
		`linear-gradient(180deg, #292c26`,
		`fetch('/api/v1/evolution'`,
		`const updateComposition = (composition)`,
		`const total = input + output`,
		`updateOdometer(document.getElementById('live-input-tokens'), number.format(input))`,
		`updateOdometer(document.getElementById('live-output-tokens'), number.format(output))`,
		`setTooltip('live-input', 'Input'`,
		`setTooltip('live-output', 'Output'`,
		`cell.dataset.tooltipKind === 'composition'`,
		`segment.style.width`,
		`updateOdometer(counter, display)`,
		`character !== previous[index]`,
		`window.setInterval(pollLifetime, 2000)`,
	} {
		if !bytes.Contains(response.Body.Bytes(), []byte(want)) {
			t.Fatalf("dashboard does not contain %q", want)
		}
	}
	if bytes.Contains(response.Body.Bytes(), []byte(`live-unclassified`)) || bytes.Contains(response.Body.Bytes(), []byte(`Unknown</span>`)) {
		t.Fatal("dashboard should hide unknown token composition")
	}
}

func TestEvolutionEndpointIncludesTokenComposition(t *testing.T) {
	store, err := database.Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	event := usage.Event{SchemaVersion: usage.SchemaVersion, EventID: "components", Timestamp: time.Now().UTC(), MachineID: "machine", Provider: "openai", Model: "model", Tool: "codex", InputTokens: usage.Int64(5), CacheReadTokens: usage.Int64(7), CacheWriteTokens: usage.Int64(0), OutputTokens: usage.Int64(3), TotalTokens: usage.Int64(15), TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "codex", AdapterVersion: "test"}}
	if _, err := store.Ingest(context.Background(), []usage.Event{event}); err != nil {
		t.Fatal(err)
	}
	server, err := New(store, "")
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/evolution", nil))
	for _, want := range []string{`"lifetime_tokens":15`, `"input_tokens":12`, `"uncached_input_tokens":5`, `"cached_input_tokens":7`, `"output_tokens":3`} {
		if !bytes.Contains(response.Body.Bytes(), []byte(want)) {
			t.Fatalf("evolution response does not contain %q: %s", want, response.Body.String())
		}
	}
}

func TestDashboardRendersEstimatedAPICostWithoutPricingDisclaimer(t *testing.T) {
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
	body := response.Body.Bytes()
	for _, want := range []string{"Est. API cost", "$10.00"} {
		if !bytes.Contains(body, []byte(want)) {
			t.Fatalf("dashboard does not contain %q: %s", want, response.Body.String())
		}
	}
	for _, omitted := range []string{"Estimated from API pricing", "coverage", "not your bill"} {
		if bytes.Contains(body, []byte(omitted)) {
			t.Fatalf("dashboard still contains pricing disclaimer %q", omitted)
		}
	}
}

func TestDashboardRendersCacheHitAndThreads(t *testing.T) {
	store, err := database.Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	event := usage.Event{SchemaVersion: usage.SchemaVersion, EventID: "cache", Timestamp: time.Now().UTC(), MachineID: "machine", SessionID: "thread", Provider: "openai", Model: "model", Tool: "codex", InputTokens: usage.Int64(20), CacheReadTokens: usage.Int64(80), OutputTokens: usage.Int64(5), TotalTokens: usage.Int64(105), TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "codex", AdapterVersion: "test"}}
	if _, err := store.Ingest(context.Background(), []usage.Event{event}); err != nil {
		t.Fatal(err)
	}
	server, err := New(store, "")
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	body := response.Body.Bytes()
	for _, want := range []string{"Cache hit", "80.0%", "Threads", "/static/tokemon/icons/cache-hit.png", "/static/tokemon/icons/threads.png"} {
		if !bytes.Contains(body, []byte(want)) {
			t.Fatalf("dashboard does not contain %q: %s", want, response.Body.String())
		}
	}
	for _, omitted := range []string{"cached of", "distinct provider sessions"} {
		if bytes.Contains(body, []byte(omitted)) {
			t.Fatalf("dashboard still contains cache/session detail %q", omitted)
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
	for _, want := range []string{"Projects", "Harnesses", "Codex", "carteakey.dev", "150", `class="pixel-glyph project-glyph palette-`, `class="pixel-glyph harness-glyph preset-codex`, `class="pixel-glyph model-glyph palette-`, `class="pixel-glyph machine-glyph palette-`} {
		if !bytes.Contains(response.Body.Bytes(), []byte(want)) {
			t.Fatalf("dashboard does not contain %q: %s", want, response.Body.String())
		}
	}
	if bytes.Contains(response.Body.Bytes(), []byte("Merged across machines")) {
		t.Fatal("dashboard still explains project merging in visible copy")
	}
}

func TestModelGlyphIsStableAndNameDerived(t *testing.T) {
	first := glyphForModel(" GPT-5.5 ")
	again := glyphForModel("gpt-5.5")
	other := glyphForModel("claude-sonnet-4")
	if first != again {
		t.Fatal("model glyph changes after normalizing the same model name")
	}
	if first == other {
		t.Fatal("different model names produced the same glyph and palette")
	}
	if first.Cells[12] != glyphOn {
		t.Fatal("model glyph does not retain its center pixel")
	}
	for row := 0; row < 5; row++ {
		for column := 0; column < 2; column++ {
			if first.Cells[row*5+column] != first.Cells[row*5+(4-column)] {
				t.Fatalf("model glyph row %d is not symmetrical", row)
			}
		}
	}
}

func TestSemanticGlyphsAreStableAndKeepTheirSilhouettes(t *testing.T) {
	project := glyphForProject(" Tokemon ")
	if project != glyphForProject("tokemon") {
		t.Fatal("project glyph changes after normalizing the same project name")
	}
	if project.Kind != "project" || project.Cells[3] != glyphOff || project.Cells[4] != glyphOff || project.Cells[5] != glyphDim {
		t.Fatalf("project glyph lost its folder silhouette: %+v", project)
	}

	machine := glyphForMachine("MOSS-02")
	if machine != glyphForMachine("moss-02") {
		t.Fatal("machine glyph changes after normalizing the same machine name")
	}
	if machine.Kind != "machine" || machine.Cells[8] != glyphOn || machine.Cells[18] != glyphOn {
		t.Fatalf("machine glyph lost its rack status lights: %+v", machine)
	}
	if project == machine {
		t.Fatal("project and machine glyphs share the same semantic silhouette")
	}

	for tool, preset := range map[string]string{
		"codex":       "codex",
		"claude-code": "claude",
		"opencode":    "opencode",
		"antigravity": "gemini",
	} {
		glyph := glyphForHarness(tool)
		if glyph.Kind != "harness" || glyph.Preset != preset {
			t.Fatalf("harness %q uses glyph kind %q preset %q", tool, glyph.Kind, glyph.Preset)
		}
	}
}

func TestDashboardRendersFriendlyHarnessNames(t *testing.T) {
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
		{SchemaVersion: usage.SchemaVersion, EventID: "claude", Timestamp: time.Now().UTC(), MachineID: "machine", Provider: "anthropic", Model: "model", Tool: "claude-code", TotalTokens: usage.Int64(60), TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "claude-code", AdapterVersion: "test"}},
		{SchemaVersion: usage.SchemaVersion, EventID: "codex", Timestamp: time.Now().UTC(), MachineID: "machine", Provider: "openai", Model: "model", Tool: "codex", TotalTokens: usage.Int64(40), TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "codex", AdapterVersion: "test"}},
	}
	if _, err := store.Ingest(context.Background(), events); err != nil {
		t.Fatal(err)
	}

	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	for _, want := range []string{"Harnesses", "Claude Code", "Codex", "preset-claude", "preset-codex", "60.0%", "40.0%"} {
		if !bytes.Contains(response.Body.Bytes(), []byte(want)) {
			t.Fatalf("dashboard does not contain harness breakdown %q: %s", want, response.Body.String())
		}
	}
}

func TestDashboardOmitsDuplicateAndTechnicalCopy(t *testing.T) {
	store, err := database.Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server, err := New(store, "")
	if err != nil {
		t.Fatal(err)
	}

	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	body := response.Body.Bytes()
	for _, want := range []string{"Lifetime tokens", "Next evolution", "Top machine", "Local-first · no conversation content"} {
		if !bytes.Contains(body, []byte(want)) {
			t.Fatalf("dashboard does not contain concise copy %q", want)
		}
	}
	for _, omitted := range []string{
		"local · live every 2s",
		"Current form",
		"Tokemon form",
		"Power level",
		"Training ground",
		"A pixel field of daily usage",
		"Dashed edge",
		"analytics refresh every 60s",
		"schema v1",
	} {
		if bytes.Contains(body, []byte(omitted)) {
			t.Fatalf("dashboard still contains unnecessary copy %q", omitted)
		}
	}
}

func TestDashboardTrimsProjectAndModelUsage(t *testing.T) {
	store, err := database.Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server, err := New(store, "")
	if err != nil {
		t.Fatal(err)
	}

	events := make([]usage.Event, 0, 6)
	for index := 0; index < 6; index++ {
		events = append(events, usage.Event{
			SchemaVersion: usage.SchemaVersion,
			EventID:       fmt.Sprintf("breakdown-%02d", index),
			Timestamp:     time.Now().UTC(),
			MachineID:     "machine",
			Project:       fmt.Sprintf("project-%02d", index),
			Provider:      "provider",
			Model:         fmt.Sprintf("model-%02d", index),
			Tool:          "tool",
			TotalTokens:   usage.Int64(int64(1000 - index)),
			TokenAccuracy: usage.AccuracyReported,
			Source:        usage.Source{Adapter: "test", AdapterVersion: "1"},
		})
	}
	if _, err := store.Ingest(context.Background(), events); err != nil {
		t.Fatal(err)
	}

	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	body := response.Body.Bytes()
	for _, want := range []string{">project-00</span>", ">project-04</span>", ">model-00</span>", ">model-04</span>"} {
		if !bytes.Contains(body, []byte(want)) {
			t.Fatalf("dashboard does not contain visible top-five row %q", want)
		}
	}
	for _, omitted := range []string{">project-05</span>", ">model-05</span>"} {
		if bytes.Contains(body, []byte(omitted)) {
			t.Fatalf("dashboard contains omitted row %q", omitted)
		}
	}
	if bytes.Contains(body, []byte(`class="see-more"`)) {
		t.Fatal("dashboard renders a non-functional see-more control")
	}
}
