package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tokemon/tokemon/internal/catalog"
	"github.com/tokemon/tokemon/internal/database"
	insightai "github.com/tokemon/tokemon/internal/insights"
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

func TestHeartbeatRequiresTokenAndRecordsMachineMetadata(t *testing.T) {
	store, err := database.Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server, err := New(store, "secret")
	if err != nil {
		t.Fatal(err)
	}
	payload := `{"machine_id":"linux-box","agent_version":"0.3.0","operating_system":"linux","architecture":"arm64","adapters":[{"id":"codex","version":"0.6.0"},{"id":"openclaw","version":"0.1.0"}],"source_count":4,"source_error_count":1}`
	request := httptest.NewRequest(http.MethodPost, "/api/v1/agents/heartbeat", strings.NewReader(payload))
	unauthorized := httptest.NewRecorder()
	server.Handler().ServeHTTP(unauthorized, request)
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("unauthorized status = %d, want %d", unauthorized.Code, http.StatusUnauthorized)
	}

	request = httptest.NewRequest(http.MethodPost, "/api/v1/agents/heartbeat", strings.NewReader(payload))
	request.Header.Set("Authorization", "Bearer secret")
	accepted := httptest.NewRecorder()
	server.Handler().ServeHTTP(accepted, request)
	if accepted.Code != http.StatusOK {
		t.Fatalf("heartbeat status = %d, want %d: %s", accepted.Code, http.StatusOK, accepted.Body.String())
	}

	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/machines", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("machines status = %d, want %d: %s", response.Code, http.StatusOK, response.Body.String())
	}
	var machines []database.MachineInfo
	if err := json.Unmarshal(response.Body.Bytes(), &machines); err != nil {
		t.Fatal(err)
	}
	if len(machines) != 1 || machines[0].ID != "linux-box" || machines[0].AgentVersion != "0.3.0" || machines[0].OperatingSystem != "linux" || machines[0].Architecture != "arm64" || machines[0].SourceCount != 4 || machines[0].SourceErrorCount != 1 {
		t.Fatalf("machines = %+v", machines)
	}
	if strings.Join(machines[0].DetectedAdapters, ",") != "codex,openclaw" {
		t.Fatalf("detected adapters = %v", machines[0].DetectedAdapters)
	}
}

func TestHealthReportsDatabaseReadiness(t *testing.T) {
	store, err := database.Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(store, "secret")
	if err != nil {
		store.Close()
		t.Fatal(err)
	}

	ready := httptest.NewRecorder()
	server.Handler().ServeHTTP(ready, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if ready.Code != http.StatusOK {
		t.Fatalf("ready health status = %d, want %d", ready.Code, http.StatusOK)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	unavailable := httptest.NewRecorder()
	server.Handler().ServeHTTP(unavailable, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if unavailable.Code != http.StatusServiceUnavailable {
		t.Fatalf("closed health status = %d, want %d", unavailable.Code, http.StatusServiceUnavailable)
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

func TestDashboardAliasSettingsUseCompactDefaultsAndSavedOverrides(t *testing.T) {
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
		EventID:       "alias-settings-1",
		Timestamp:     time.Now().UTC(),
		MachineID:     "Kartikeys-MacBook-Air",
		Provider:      "anthropic",
		Model:         "claude-sonnet-4",
		Tool:          "claude-code",
		TotalTokens:   usage.Int64(42),
		TokenAccuracy: usage.AccuracyReported,
		Source:        usage.Source{Adapter: "test", AdapterVersion: "1"},
	}
	if _, err := store.Ingest(context.Background(), []usage.Event{event}); err != nil {
		t.Fatal(err)
	}

	settings := httptest.NewRecorder()
	server.Handler().ServeHTTP(settings, httptest.NewRequest(http.MethodGet, "/settings", nil))
	if settings.Code != http.StatusOK {
		t.Fatalf("settings status = %d, want %d", settings.Code, http.StatusOK)
	}
	for _, want := range []string{"Settings", "claude-sonnet-4", "Sonnet 4", "Kartikeys-MacBook-Air", "MacBook Air", "Save aliases"} {
		if !strings.Contains(settings.Body.String(), want) {
			t.Fatalf("settings does not contain %q: %s", want, settings.Body.String())
		}
	}

	form := url.Values{
		"model_identity":   {"claude-sonnet-4"},
		"model_alias":      {"Sonnet 4"},
		"machine_identity": {"Kartikeys-MacBook-Air"},
		"machine_alias":    {"Work Mac"},
	}
	request := httptest.NewRequest(http.MethodPost, "/settings/aliases", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	saved := httptest.NewRecorder()
	server.Handler().ServeHTTP(saved, request)
	if saved.Code != http.StatusSeeOther || saved.Header().Get("Location") != "/settings?saved=1" {
		t.Fatalf("save response = %d, location %q", saved.Code, saved.Header().Get("Location"))
	}

	dashboard := httptest.NewRecorder()
	server.Handler().ServeHTTP(dashboard, httptest.NewRequest(http.MethodGet, "/", nil))
	body := dashboard.Body.String()
	for _, want := range []string{">Sonnet 4</span>", ">Work Mac</span>", `title="claude-sonnet-4"`, `title="Kartikeys-MacBook-Air"`} {
		if !strings.Contains(body, want) {
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

func TestEvolutionEndpointCachesUntilSuccessfulIngest(t *testing.T) {
	store, err := database.Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server, err := New(store, "secret")
	if err != nil {
		t.Fatal(err)
	}
	initial := usage.Event{SchemaVersion: usage.SchemaVersion, EventID: "cache-initial", Timestamp: time.Now().UTC(), MachineID: "machine", Provider: "openai", Model: "model", Tool: "codex", TotalTokens: usage.Int64(10), TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "codex", AdapterVersion: "test"}}
	if _, err := store.Ingest(context.Background(), []usage.Event{initial}); err != nil {
		t.Fatal(err)
	}

	handler := server.Handler()
	first := httptest.NewRecorder()
	handler.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/api/v1/evolution", nil))
	if first.Code != http.StatusOK || server.evolutionCache == nil {
		t.Fatalf("initial evolution response = %d %s, cache = %#v", first.Code, first.Body.String(), server.evolutionCache)
	}
	cached := server.evolutionCache
	second := httptest.NewRecorder()
	handler.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/api/v1/evolution", nil))
	if second.Code != http.StatusOK || server.evolutionCache != cached || second.Body.String() != first.Body.String() {
		t.Fatalf("repeated evolution response missed cache: first=%s second=%s", first.Body.String(), second.Body.String())
	}

	update := usage.Event{SchemaVersion: usage.SchemaVersion, EventID: "cache-update", Timestamp: time.Now().UTC(), MachineID: "machine", Provider: "openai", Model: "model", Tool: "codex", TotalTokens: usage.Int64(15), TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "codex", AdapterVersion: "test"}}
	payload, err := json.Marshal(batchRequest{Events: []usage.Event{update}})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/events/batch", bytes.NewReader(payload))
	request.Header.Set("Authorization", "Bearer secret")
	accepted := httptest.NewRecorder()
	handler.ServeHTTP(accepted, request)
	if accepted.Code != http.StatusOK || server.evolutionCache != nil {
		t.Fatalf("ingest response = %d %s, cache was not invalidated", accepted.Code, accepted.Body.String())
	}

	refreshed := httptest.NewRecorder()
	handler.ServeHTTP(refreshed, httptest.NewRequest(http.MethodGet, "/api/v1/evolution", nil))
	if refreshed.Code != http.StatusOK || !strings.Contains(refreshed.Body.String(), `"lifetime_tokens":25`) || server.evolutionCache == nil || server.evolutionCache == cached {
		t.Fatalf("refreshed evolution response = %d %s, cache = %#v", refreshed.Code, refreshed.Body.String(), server.evolutionCache)
	}
}

func TestEvolutionEndpointCacheHandlesConcurrentPolling(t *testing.T) {
	store, err := database.Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server, err := New(store, "")
	if err != nil {
		t.Fatal(err)
	}
	event := usage.Event{SchemaVersion: usage.SchemaVersion, EventID: "concurrent-cache", Timestamp: time.Now().UTC(), MachineID: "machine", Provider: "openai", Model: "model", Tool: "codex", TotalTokens: usage.Int64(42), TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "codex", AdapterVersion: "test"}}
	if _, err := store.Ingest(context.Background(), []usage.Event{event}); err != nil {
		t.Fatal(err)
	}

	handler := server.Handler()
	start := make(chan struct{})
	var wait sync.WaitGroup
	for worker := 0; worker < 16; worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			for requestIndex := 0; requestIndex < 20; requestIndex++ {
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/evolution", nil))
				body := response.Body.String()
				if response.Code != http.StatusOK || (!strings.Contains(body, `"lifetime_tokens":42`) && !strings.Contains(body, `"lifetime_tokens":43`)) {
					t.Errorf("concurrent evolution response = %d %s", response.Code, response.Body.String())
					return
				}
			}
		}()
	}
	wait.Add(1)
	go func() {
		defer wait.Done()
		<-start
		update := usage.Event{SchemaVersion: usage.SchemaVersion, EventID: "concurrent-update", Timestamp: time.Now().UTC(), MachineID: "machine", Provider: "openai", Model: "model", Tool: "codex", TotalTokens: usage.Int64(1), TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "codex", AdapterVersion: "test"}}
		payload, err := json.Marshal(batchRequest{Events: []usage.Event{update}})
		if err != nil {
			t.Errorf("marshal concurrent update: %v", err)
			return
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/v1/events/batch", bytes.NewReader(payload)))
		if response.Code != http.StatusOK {
			t.Errorf("concurrent ingest response = %d %s", response.Code, response.Body.String())
		}
	}()
	close(start)
	wait.Wait()

	final := httptest.NewRecorder()
	handler.ServeHTTP(final, httptest.NewRequest(http.MethodGet, "/api/v1/evolution", nil))
	if final.Code != http.StatusOK || !strings.Contains(final.Body.String(), `"lifetime_tokens":43`) {
		t.Fatalf("final evolution response = %d %s", final.Code, final.Body.String())
	}
}

func TestEvolutionEndpointRecomputesAfterCacheTTL(t *testing.T) {
	store, err := database.Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server, err := New(store, "")
	if err != nil {
		t.Fatal(err)
	}
	event := usage.Event{SchemaVersion: usage.SchemaVersion, EventID: "ttl-event", Timestamp: time.Now().UTC(), MachineID: "machine", Provider: "openai", Model: "model", Tool: "codex", TotalTokens: usage.Int64(7), TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "codex", AdapterVersion: "test"}}
	if _, err := store.Ingest(context.Background(), []usage.Event{event}); err != nil {
		t.Fatal(err)
	}

	handler := server.Handler()
	first := httptest.NewRecorder()
	handler.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/api/v1/evolution", nil))
	if first.Code != http.StatusOK || server.evolutionCache == nil {
		t.Fatalf("initial evolution response = %d %s", first.Code, first.Body.String())
	}
	original := server.evolutionCache
	if _, err := server.cachedEvolution(context.Background()); err != nil {
		t.Fatal(err)
	}
	if server.evolutionCache != original || time.Since(original.fetched) > time.Second {
		t.Fatalf("fresh cache was overwritten: %#v", server.evolutionCache)
	}

	server.evolutionCache.fetched = time.Now().Add(-evolutionCacheTTL - time.Second)
	backdatedEntry := server.evolutionCache
	refreshed := httptest.NewRecorder()
	handler.ServeHTTP(refreshed, httptest.NewRequest(http.MethodGet, "/api/v1/evolution", nil))
	if refreshed.Code != http.StatusOK || !strings.Contains(refreshed.Body.String(), `"lifetime_tokens":7`) {
		t.Fatalf("expired evolution response = %d %s", refreshed.Code, refreshed.Body.String())
	}
	server.evolutionMu.Lock()
	recomputed := server.evolutionCache
	server.evolutionMu.Unlock()
	if recomputed == nil || recomputed == backdatedEntry || time.Since(recomputed.fetched) > time.Second {
		t.Fatalf("expired cache was not recomputed: %#v", server.evolutionCache)
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
		"codex":        "codex",
		"claude-code":  "claude",
		"copilot-cli":  "copilot",
		"opencode":     "opencode",
		"antigravity":  "gemini",
		"openclaw":     "openclaw",
		"hermes-agent": "hermes",
	} {
		glyph := glyphForHarness(tool)
		if glyph.Kind != "harness" || glyph.Preset != preset {
			t.Fatalf("harness %q uses glyph kind %q preset %q", tool, glyph.Kind, glyph.Preset)
		}
	}
	if got := harnessName("copilot-cli"); got != "GitHub Copilot CLI" {
		t.Fatalf("Copilot harness label = %q", got)
	}
	if got := harnessName("hermes-agent"); got != "Hermes Agent" {
		t.Fatalf("Hermes harness label = %q", got)
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
	if regexp.MustCompile(`<a[^>]*class="[^"]*nav-link[^"]*"[^>]*href="/insights"`).Match(body) {
		t.Fatal("dashboard still exposes Insights in primary navigation")
	}
	if regexp.MustCompile(`(?i)\bknown\b`).Match(body) {
		t.Fatalf("dashboard still renders known qualifier: %s", body)
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

func TestAnalyticsPageAndJSONExport(t *testing.T) {
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
		EventID:       "analytics-page",
		Timestamp:     time.Now().UTC().Add(-24 * time.Hour),
		MachineID:     "machine-one",
		Project:       "project-a",
		Provider:      "openai",
		Model:         "gpt",
		Tool:          "codex",
		SessionID:     "session-one",
		InputTokens:   usage.Int64(60),
		OutputTokens:  usage.Int64(40),
		TotalTokens:   usage.Int64(100),
		TokenAccuracy: usage.AccuracyReported,
		Source:        usage.Source{Adapter: "codex", AdapterVersion: "test"},
	}
	if _, err := store.Ingest(context.Background(), []usage.Event{event}); err != nil {
		t.Fatal(err)
	}

	pageResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(pageResponse, httptest.NewRequest(http.MethodGet, "/analytics?period=7d&dimension=models&machine=machine-one", nil))
	if pageResponse.Code != http.StatusOK {
		t.Fatalf("analytics page status = %d, want %d: %s", pageResponse.Code, http.StatusOK, pageResponse.Body.String())
	}
	for _, want := range []string{"<title>Tokemon · Analytics</title>", "Export analytics JSON", "Token trend", "Usage share", "Models · top 5", "share-bars", "share-column", "share-stack", "share-segment", "share-hit-grid", "aria-label=\"Share scale\"", "share-tooltip", "role=\"tooltip\"", "data-share-tooltip-date", "data-share-tooltip-total", "data-share-tooltip-series", "Unknown totals present", "data-share-value", "aria-describedby=\"share-tooltip\"", "point.addEventListener('pointerenter'", "point.addEventListener('focus'", "point.addEventListener('click'", "Recent sessions", "name=\"period\"", "All machines", ">24H</a>", ">Providers</a>", "data-count=", "data-label=", "prefers-reduced-motion", "trend-y-axis", "trend-annotation", "Peak bucket", "tick-start"} {
		if !bytes.Contains(pageResponse.Body.Bytes(), []byte(want)) {
			t.Fatalf("analytics page does not contain %q: %s", want, pageResponse.Body.String())
		}
	}
	if bytes.Contains(pageResponse.Body.Bytes(), []byte("share-readout")) {
		t.Fatal("analytics page still renders the persistent usage-share readout")
	}
	if bytes.Contains(pageResponse.Body.Bytes(), []byte(`<svg class="share-svg"`)) || bytes.Contains(pageResponse.Body.Bytes(), []byte("share-area")) {
		t.Fatal("analytics page still renders continuous usage-share paths")
	}
	if bytes.Contains(pageResponse.Body.Bytes(), []byte(`class="share-point selected`)) {
		t.Fatal("analytics page still renders a permanently selected usage-share bucket")
	}
	if got := strings.Count(pageResponse.Body.String(), `class="share-column`); got != 7 {
		t.Fatalf("usage-share columns = %d, want seven discrete buckets", got)
	}
	if got := strings.Count(pageResponse.Body.String(), `class="share-column empty`); got != 6 {
		t.Fatalf("empty usage-share buckets = %d, want six intentional empty buckets", got)
	}
	if bytes.Contains(pageResponse.Body.Bytes(), []byte(`/api/v1/analytics/overview">Data</a>`)) {
		t.Fatal("analytics navigation still points directly at the overview JSON")
	}
	if regexp.MustCompile(`<a[^>]*class="[^"]*nav-link[^"]*"[^>]*href="/insights"`).Match(pageResponse.Body.Bytes()) {
		t.Fatal("analytics page still exposes Insights in primary navigation")
	}
	if regexp.MustCompile(`(?i)\bknown\b`).Match(pageResponse.Body.Bytes()) {
		t.Fatalf("analytics page still renders known qualifier: %s", pageResponse.Body.String())
	}

	exportResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(exportResponse, httptest.NewRequest(http.MethodGet, "/api/v1/analytics/export?period=7d&dimension=models", nil))
	if exportResponse.Code != http.StatusOK {
		t.Fatalf("analytics export status = %d, want %d: %s", exportResponse.Code, http.StatusOK, exportResponse.Body.String())
	}
	if got := exportResponse.Header().Get("Content-Disposition"); got != `attachment; filename="tokemon-analytics-7d.json"` {
		t.Fatalf("content disposition = %q", got)
	}
	var exported database.Analytics
	if err := json.Unmarshal(exportResponse.Body.Bytes(), &exported); err != nil {
		t.Fatalf("decode analytics export: %v", err)
	}
	if exported.Filter.Period != "7d" || exported.Summary.Tokens != 100 || exported.Filter.Dimension != "models" || len(exported.ShareSeries) != 1 || exported.ShareSeries[0].Name != "gpt" || len(exported.SharePoints) != 7 {
		t.Fatalf("unexpected exported analytics: %+v", exported)
	}
}

func TestAnalyticsChartScaleAndDateTicks(t *testing.T) {
	for _, test := range []struct {
		maximum int64
		want    int64
	}{
		{maximum: 1, want: 1},
		{maximum: 83_000_000, want: 100_000_000},
		{maximum: 220_762_070, want: 250_000_000},
	} {
		if got := analyticsNiceMax(test.maximum); got != test.want {
			t.Errorf("analyticsNiceMax(%d) = %d, want %d", test.maximum, got, test.want)
		}
	}

	ticks := analyticsAxisTicks(250_000_000)
	if len(ticks) != 5 || ticks[0].Value != 250_000_000 || ticks[0].Position != 100 || ticks[4].Value != 0 || ticks[4].Position != 0 {
		t.Fatalf("unexpected analytics axis ticks: %+v", ticks)
	}
	if got := analyticsDateTickClass(5, 30, "30d"); got != " tick" {
		t.Fatalf("30-day date tick class = %q, want tick", got)
	}
	if got := analyticsDateTickClass(6, 30, "30d"); got != "" {
		t.Fatalf("unexpected intermediate 30-day tick class: %q", got)
	}
}

type insightsRecapStub struct {
	text string
	err  error
}

func (stub insightsRecapStub) Generate(context.Context, database.Insights) (string, error) {
	return stub.text, stub.err
}

func TestInsightsPageAndAPIRespectAnalyticsFilters(t *testing.T) {
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
		EventID:       "insights-render",
		Timestamp:     time.Now().UTC(),
		MachineID:     "insights-machine",
		Provider:      "openai",
		Model:         "gpt",
		Tool:          "codex",
		SessionID:     "private-session-id",
		InputTokens:   usage.Int64(60),
		OutputTokens:  usage.Int64(40),
		TotalTokens:   usage.Int64(100),
		TokenAccuracy: usage.AccuracyReported,
		Source:        usage.Source{Adapter: "codex", AdapterVersion: "test"},
	}
	unknown := usage.Event{
		SchemaVersion: usage.SchemaVersion,
		EventID:       "insights-unknown",
		Timestamp:     time.Now().UTC(),
		MachineID:     "insights-machine",
		Provider:      "openai",
		Model:         "gpt",
		Tool:          "codex",
		TokenAccuracy: usage.AccuracyUnknown,
		Source:        usage.Source{Adapter: "codex", AdapterVersion: "test"},
	}
	if result, err := store.Ingest(context.Background(), []usage.Event{event, unknown}); err != nil || result.Accepted != 2 {
		t.Fatalf("unexpected insights ingest result: %+v, error: %v", result, err)
	}

	pageResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(pageResponse, httptest.NewRequest(http.MethodGet, "/insights?period=7d&dimension=models&machine=insights-machine", nil))
	if pageResponse.Code != http.StatusOK {
		t.Fatalf("insights page status = %d, want %d: %s", pageResponse.Code, http.StatusOK, pageResponse.Body.String())
	}
	body := pageResponse.Body.String()
	for _, want := range []string{"<title>Tokemon · Insights</title>", "Overview", "Analytics", "Insights", "Privacy boundary", "prompts", "responses", "source paths", "24-hour rhythm", "Weekday distribution", "Why this?", "Evidence", "Basis", "Open this slice in Analytics", "AI recap", "Recap disabled / unconfigured", "unknown token totals", "@media (max-width: 700px)", "name=\"dimension\"", "name=\"machine\"", "name=\"provider\"", "name=\"model\"", "name=\"tool\""} {
		if !strings.Contains(body, want) {
			t.Fatalf("insights page does not contain %q: %s", want, body)
		}
	}
	if strings.Contains(body, "private-session-id") || strings.Contains(body, "session_id") {
		t.Fatal("insights page leaked session metadata")
	}
	if regexp.MustCompile(`(?i)\bknown\b`).MatchString(body) {
		t.Fatalf("insights page still renders known qualifier: %s", body)
	}
	if regexp.MustCompile(`<a[^>]*class="[^"]*nav-link[^"]*"[^>]*href="/insights"`).MatchString(body) {
		t.Fatal("insights page still exposes Insights in primary navigation")
	}

	apiResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(apiResponse, httptest.NewRequest(http.MethodGet, "/api/v1/insights?period=7d&dimension=models&machine=insights-machine", nil))
	if apiResponse.Code != http.StatusOK {
		t.Fatalf("insights API status = %d, want %d: %s", apiResponse.Code, http.StatusOK, apiResponse.Body.String())
	}
	var payload struct {
		database.Insights
		AIRecap InsightsRecap `json:"ai_recap"`
	}
	if err := json.Unmarshal(apiResponse.Body.Bytes(), &payload); err != nil {
		t.Fatalf("decode insights API: %v", err)
	}
	if payload.Filter.Period != "7d" || payload.Filter.Dimension != "models" || payload.Filter.Machine != "insights-machine" {
		t.Fatalf("unexpected insights filter: %+v", payload.Filter)
	}
	if payload.AIRecap.Status != InsightsRecapDisabled {
		t.Fatalf("default AI recap status = %q, want %q", payload.AIRecap.Status, InsightsRecapDisabled)
	}
	if strings.Contains(apiResponse.Body.String(), "session_id") || strings.Contains(apiResponse.Body.String(), "private-session-id") {
		t.Fatal("insights API leaked session metadata")
	}
	if regexp.MustCompile(`(?i)\bknown\b`).Match(apiResponse.Body.Bytes()) {
		t.Fatalf("insights API still emits known qualifier: %s", apiResponse.Body.String())
	}
	defaultResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(defaultResponse, httptest.NewRequest(http.MethodGet, "/api/v1/insights", nil))
	if defaultResponse.Code != http.StatusOK {
		t.Fatalf("default insights API status = %d, want %d", defaultResponse.Code, http.StatusOK)
	}
	var defaultPayload struct {
		database.Insights
	}
	if err := json.Unmarshal(defaultResponse.Body.Bytes(), &defaultPayload); err != nil {
		t.Fatalf("decode default insights API: %v", err)
	}
	if defaultPayload.Filter.Period != "30d" {
		t.Fatalf("default insights period = %q, want 30d", defaultPayload.Filter.Period)
	}
}

func TestInsightsRecapStatusesAndEmptyState(t *testing.T) {
	store, err := database.Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server, err := NewWithInsights(store, "", insightsRecapStub{text: "A generated aggregate recap."})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/insights", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("empty insights status = %d, want %d", response.Code, http.StatusOK)
	}
	for _, want := range []string{"No strong signals in this window.", "No token metadata matches these filters yet.", "Generated recap", "A generated aggregate recap."} {
		if !strings.Contains(response.Body.String(), want) {
			t.Fatalf("empty insights does not contain %q: %s", want, response.Body.String())
		}
	}
	allWindow := httptest.NewRecorder()
	server.Handler().ServeHTTP(allWindow, httptest.NewRequest(http.MethodGet, "/insights?period=all", nil))
	if allWindow.Code != http.StatusOK || !strings.Contains(allWindow.Body.String(), "No comparable baseline for the all-time window") {
		t.Fatalf("all-time baseline state = %d %s", allWindow.Code, allWindow.Body.String())
	}

	store2, err := database.Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()
	unavailable, err := NewWithInsights(store2, "", insightsRecapStub{err: fmt.Errorf("provider offline")})
	if err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	unavailable.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/insights", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "Recap temporarily unavailable") {
		t.Fatalf("unavailable recap page = %d %s", response.Code, response.Body.String())
	}
}

func TestInsightsRenderHidesDatabaseErrorDetails(t *testing.T) {
	store, err := database.Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	server, err := New(store, "")
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/insights", nil))
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("closed-store insights status = %d, want %d", response.Code, http.StatusInternalServerError)
	}
	body := response.Body.String()
	if !strings.Contains(body, "Insights are temporarily unavailable.") || strings.Contains(body, "sql:") || strings.Contains(body, "database is closed") {
		t.Fatalf("insights error leaked database details: %s", body)
	}
}

type insightsSynthesizerStub struct {
	input insightai.Input
}

func (stub *insightsSynthesizerStub) Synthesize(_ context.Context, input insightai.Input) (insightai.Output, error) {
	stub.input = input
	return insightai.Output{Title: "Usage pulse", Summary: "Your aggregate rhythm shifted.", EvidenceIDs: []string{"rhythm-peak-time"}}, nil
}

func TestAggregateAIRecapRedactsSelectedDimensionLabels(t *testing.T) {
	stub := &insightsSynthesizerStub{}
	result := database.Insights{
		Period: "30d", Timezone: "America/Toronto",
		Filter:      database.AnalyticsQuery{Period: "30d", Dimension: "projects", Machine: "private-machine"},
		DataQuality: database.InsightDataQuality{Confidence: "high", Qualifier: "all event totals are available"},
		Cards: []database.InsightCard{
			{ID: "dimension-concentration", Category: "concentration", Observation: "secret-project: 81% of tokens", Basis: "project share"},
			{ID: "rhythm-peak-time", Category: "rhythm", Observation: "14:00 local hour is highest", Basis: "tokens grouped by configured timezone"},
		},
	}
	recap, err := aggregateAIRecapProvider{synthesizer: stub}.Generate(context.Background(), result)
	if err != nil {
		t.Fatal(err)
	}
	if recap != "Usage pulse — Your aggregate rhythm shifted." {
		t.Fatalf("recap = %q", recap)
	}
	payload, err := json.Marshal(stub.input)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"secret-project", "private-machine", "project share"} {
		if strings.Contains(string(payload), forbidden) {
			t.Fatalf("aggregate AI input leaked %q: %s", forbidden, payload)
		}
	}
	if _, err := insightai.ValidateInput(stub.input); err != nil {
		t.Fatalf("aggregate AI input rejected by transport policy: %v", err)
	}
	if len(stub.input.Evidence) != 2 || stub.input.ScopeLabels[0].Label != insightai.ScopeFilteredView {
		t.Fatalf("unexpected aggregate AI input: %+v", stub.input)
	}
}

func TestSettingsRequireAuthenticationAndCSRF(t *testing.T) {
	store, err := database.Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server, err := New(store, "secret")
	if err != nil {
		t.Fatal(err)
	}
	handler := server.Handler()

	anonymous := httptest.NewRecorder()
	handler.ServeHTTP(anonymous, httptest.NewRequest(http.MethodGet, "/settings", nil))
	if anonymous.Code != http.StatusSeeOther || anonymous.Header().Get("Location") != "/settings/login" {
		t.Fatalf("anonymous settings status = %d, location %q", anonymous.Code, anonymous.Header().Get("Location"))
	}

	form := url.Values{"model_identity": {"claude-sonnet-4"}, "model_alias": {"Sonnet 4"}}
	request := httptest.NewRequest(http.MethodPost, "/settings/aliases", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	unauthenticated := httptest.NewRecorder()
	handler.ServeHTTP(unauthenticated, request)
	if unauthenticated.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated save status = %d, want %d", unauthenticated.Code, http.StatusUnauthorized)
	}

	loginPage := httptest.NewRecorder()
	handler.ServeHTTP(loginPage, httptest.NewRequest(http.MethodGet, "/settings/login", nil))
	cookies := loginPage.Result().Cookies()
	if loginPage.Code != http.StatusOK {
		t.Fatalf("login page status = %d", loginPage.Code)
	}
	var csrfCookie *http.Cookie
	for _, cookie := range cookies {
		if cookie.Name == "login_csrf" {
			csrfCookie = cookie
		}
	}
	if csrfCookie == nil {
		t.Fatal("login page did not set a login_csrf cookie")
	}

	login := url.Values{"token": {"wrong"}, "_csrf": {csrfCookie.Value}}
	loginRequest := httptest.NewRequest(http.MethodPost, "/settings/login", strings.NewReader(login.Encode()))
	loginRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	loginRequest.AddCookie(csrfCookie)
	wrongToken := httptest.NewRecorder()
	handler.ServeHTTP(wrongToken, loginRequest)
	if wrongToken.Code != http.StatusUnauthorized {
		t.Fatalf("wrong-token login status = %d, want %d", wrongToken.Code, http.StatusUnauthorized)
	}

	login = url.Values{"token": {"secret"}, "_csrf": {csrfCookie.Value}}
	loginRequest = httptest.NewRequest(http.MethodPost, "/settings/login", strings.NewReader(login.Encode()))
	loginRequest.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	loginRequest.AddCookie(csrfCookie)
	success := httptest.NewRecorder()
	handler.ServeHTTP(success, loginRequest)
	if success.Code != http.StatusSeeOther {
		t.Fatalf("login status = %d, want %d", success.Code, http.StatusSeeOther)
	}
	var sessionCookie *http.Cookie
	for _, cookie := range success.Result().Cookies() {
		if cookie.Name == dashboardSessionCookieName {
			sessionCookie = cookie
		}
	}
	if sessionCookie == nil || sessionCookie.HttpOnly != true || sessionCookie.SameSite != http.SameSiteLaxMode || sessionCookie.Expires.IsZero() {
		t.Fatalf("login did not issue a secure session cookie: %+v", sessionCookie)
	}

	settingsPage := httptest.NewRecorder()
	settingsRequest := httptest.NewRequest(http.MethodGet, "/settings", nil)
	settingsRequest.AddCookie(sessionCookie)
	handler.ServeHTTP(settingsPage, settingsRequest)
	if settingsPage.Code != http.StatusOK {
		t.Fatalf("authenticated settings status = %d", settingsPage.Code)
	}
	sessionParts := strings.Split(sessionCookie.Value, ".")
	if len(sessionParts) != 3 || !strings.Contains(settingsPage.Body.String(), `name="_csrf" value="`+sessionParts[0]+`"`) {
		t.Fatalf("settings page does not embed the session CSRF nonce: %s", settingsPage.Body.String())
	}

	form = url.Values{"model_identity": {"claude-sonnet-4"}, "model_alias": {"Sonnet 4"}, "_csrf": {"forged"}}
	request = httptest.NewRequest(http.MethodPost, "/settings/aliases", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(sessionCookie)
	forged := httptest.NewRecorder()
	handler.ServeHTTP(forged, request)
	if forged.Code != http.StatusForbidden {
		t.Fatalf("CSRF-forged save status = %d, want %d", forged.Code, http.StatusForbidden)
	}

	form = url.Values{"model_identity": {"claude-sonnet-4"}, "model_alias": {"Sonnet 4"}, "_csrf": {sessionParts[0]}}
	request = httptest.NewRequest(http.MethodPost, "/settings/aliases", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	request.AddCookie(sessionCookie)
	saved := httptest.NewRecorder()
	handler.ServeHTTP(saved, request)
	if saved.Code != http.StatusSeeOther {
		t.Fatalf("authenticated save status = %d, want %d", saved.Code, http.StatusSeeOther)
	}

	logout := httptest.NewRecorder()
	logoutRequest := httptest.NewRequest(http.MethodPost, "/settings/logout", nil)
	logoutRequest.AddCookie(sessionCookie)
	handler.ServeHTTP(logout, logoutRequest)
	if logout.Code != http.StatusSeeOther {
		t.Fatalf("logout status = %d, want %d", logout.Code, http.StatusSeeOther)
	}
	var expired bool
	for _, cookie := range logout.Result().Cookies() {
		if cookie.Name == dashboardSessionCookieName && cookie.MaxAge < 0 {
			expired = true
		}
	}
	if !expired {
		t.Fatal("logout did not expire the session cookie")
	}
}

func TestSettingsStayOpenWithoutIngestToken(t *testing.T) {
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
	server.Handler().ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/settings", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("tokenless settings status = %d, want %d", response.Code, http.StatusOK)
	}
}

func TestExpiredDashboardSessionIsRejected(t *testing.T) {
	store, err := database.Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server, err := New(store, "secret")
	if err != nil {
		t.Fatal(err)
	}
	expired := dashboardSession{value: "expired-session", issuedAt: time.Now().UTC().Add(-dashboardSessionDuration - time.Minute)}
	issuedAt := expired.issuedAt.Unix()
	cookie := sessionCookieValue(expired, server.signSession(expired.value, issuedAt))
	request := httptest.NewRequest(http.MethodGet, "/settings", nil)
	request.AddCookie(&http.Cookie{Name: dashboardSessionCookieName, Value: cookie})
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusSeeOther || response.Header().Get("Location") != "/settings/login" {
		t.Fatalf("expired session status = %d, location %q", response.Code, response.Header().Get("Location"))
	}
}

func TestIngestRejectsOversizedBatches(t *testing.T) {
	store, err := database.Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server, err := New(store, "")
	if err != nil {
		t.Fatal(err)
	}
	events := make([]usage.Event, 0, maxEventsPerBatch+1)
	for index := 0; index < maxEventsPerBatch+1; index++ {
		events = append(events, usage.Event{
			SchemaVersion: usage.SchemaVersion,
			EventID:       fmt.Sprintf("oversized-%d", index),
			Timestamp:     time.Now(),
			MachineID:     "machine",
			Provider:      "provider",
			Model:         "model",
			Tool:          "tool",
			TotalTokens:   usage.Int64(1),
			TokenAccuracy: usage.AccuracyReported,
			Source:        usage.Source{Adapter: "test", AdapterVersion: "1"},
		})
	}
	payload, err := json.Marshal(batchRequest{Events: events})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/events/batch", bytes.NewReader(payload))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized batch status = %d, want %d", response.Code, http.StatusRequestEntityTooLarge)
	}
}

func TestIngestRejectsTrailingJSON(t *testing.T) {
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
		EventID:       "trailing-json",
		Timestamp:     time.Now().UTC(),
		MachineID:     "machine",
		Provider:      "provider",
		Model:         "model",
		Tool:          "tool",
		TotalTokens:   usage.Int64(1),
		TokenAccuracy: usage.AccuracyReported,
		Source:        usage.Source{Adapter: "test", AdapterVersion: "1"},
	}
	payload, err := json.Marshal(batchRequest{Events: []usage.Event{event}})
	if err != nil {
		t.Fatal(err)
	}
	payload = append(payload, []byte(`{"events":[]}`)...)
	request := httptest.NewRequest(http.MethodPost, "/api/v1/events/batch", bytes.NewReader(payload))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("trailing JSON status = %d, want %d", response.Code, http.StatusBadRequest)
	}
}

func TestBatchIngestRejectsInvalidEvents(t *testing.T) {
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
		EventID:       "bad-event",
		Timestamp:     time.Now(),
		MachineID:     "machine",
		Provider:      "provider",
		Model:         "model",
		Tool:          "tool",
		InputTokens:   usage.Int64(60),
		TotalTokens:   usage.Int64(30),
		TokenAccuracy: usage.AccuracyReported,
		Source:        usage.Source{Adapter: "test", AdapterVersion: "1"},
	}
	payload, err := json.Marshal(batchRequest{Events: []usage.Event{event}})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/events/batch", bytes.NewReader(payload))
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	var result database.IngestResult
	if response.Code != http.StatusOK {
		t.Fatalf("batch status = %d, want %d", response.Code, http.StatusOK)
	}
	if err := json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Rejected != 1 || result.Accepted != 0 {
		t.Fatalf("invalid event was not rejected: %+v", result)
	}
}
