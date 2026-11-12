package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tokemon/tokemon/internal/catalog"
	"github.com/tokemon/tokemon/internal/database"
	insightai "github.com/tokemon/tokemon/internal/insights"
	"github.com/tokemon/tokemon/internal/quota"
	"github.com/tokemon/tokemon/internal/usage"

	"golang.org/x/crypto/bcrypt"
)

const testDashboardToken = "test-dashboard-token"

func newTestServer(store *database.Store, ingestToken string) (*Server, error) {
	return NewWithConfig(store, Config{IngestToken: ingestToken, DashboardToken: testDashboardToken, AllowLoopbackDev: true, EnableTokedex: true})
}

func testHandler(server *Server) http.Handler {
	base := server.Handler()
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if server.config.AllowLoopbackDev && server.config.IngestToken == "" {
			r.RemoteAddr = "127.0.0.1:1234"
		}
		if r.Header.Get("Authorization") == "" && r.Header.Get("X-Tokemon-Ingest-Token") == "" {
			r.SetBasicAuth("test", testDashboardToken)
		}
		base.ServeHTTP(w, r)
	})
}

func TestBatchIngestRequiresTokenAndUpdatesEvolution(t *testing.T) {
	store, err := database.Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server, err := newTestServer(store, "secret")
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
	testHandler(server).ServeHTTP(evolutionResponse, evolutionRequest)
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
	server, err := newTestServer(store, "secret")
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
	testHandler(server).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/machines", nil))
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

func TestHeartbeatSourceHealthStatesAndOldAgentCompat(t *testing.T) {
	store, err := database.Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server, err := newTestServer(store, "secret")
	if err != nil {
		t.Fatal(err)
	}
	sendHeartbeat := func(payload string) *httptest.ResponseRecorder {
		request := httptest.NewRequest(http.MethodPost, "/api/v1/agents/heartbeat", strings.NewReader(payload))
		request.Header.Set("Authorization", "Bearer secret")
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		return response
	}

	now := time.Now().UTC()
	freshSync := now.Add(-2 * time.Minute).Format(time.RFC3339)
	staleSync := now.Add(-2 * time.Hour).Format(time.RFC3339)
	payload := fmt.Sprintf(`{"machine_id":"linux-box","agent_version":"0.4.2","operating_system":"linux","architecture":"arm64","adapters":[{"id":"codex"}],"source_count":3,"source_error_count":1,"sources":[`+
		`{"adapter":"codex","identity_hash":"sha256:%s","last_successful_sync":%q},`+
		`{"adapter":"claude-code","identity_hash":"sha256:%s","last_successful_sync":%q},`+
		`{"adapter":"hermes","identity_hash":"sha256:%s","last_error_class":"missing"}]}`,
		strings.Repeat("1", 64), freshSync, strings.Repeat("2", 64), staleSync, strings.Repeat("3", 64))
	accepted := sendHeartbeat(payload)
	if accepted.Code != http.StatusOK {
		t.Fatalf("heartbeat status = %d, want %d: %s", accepted.Code, http.StatusOK, accepted.Body.String())
	}

	machinesResponse := httptest.NewRecorder()
	testHandler(server).ServeHTTP(machinesResponse, httptest.NewRequest(http.MethodGet, "/api/v1/machines", nil))
	if machinesResponse.Code != http.StatusOK {
		t.Fatalf("machines status = %d", machinesResponse.Code)
	}
	var machines []database.MachineInfo
	if err := json.Unmarshal(machinesResponse.Body.Bytes(), &machines); err != nil {
		t.Fatal(err)
	}
	if len(machines) != 1 || len(machines[0].Sources) != 3 {
		t.Fatalf("machines = %+v", machines)
	}
	// Rows are ordered by adapter: claude-code (stale), codex (fresh), hermes
	// (unknown, never synced, currently failing).
	sources := machines[0].Sources
	if sources[0].Adapter != "claude-code" || sources[0].State != "Stale" || sources[0].LastErrorClass != "" || sources[0].LastSuccessfulSync == nil {
		t.Fatalf("stale source = %+v", sources[0])
	}
	if !strings.HasSuffix(sources[0].Age, " ago") {
		t.Fatalf("stale source age = %q, want a relative age", sources[0].Age)
	}
	if sources[1].Adapter != "codex" || sources[1].State != "Fresh" || sources[1].LastSuccessfulSync == nil {
		t.Fatalf("fresh source = %+v", sources[1])
	}
	if sources[2].Adapter != "hermes" || sources[2].State != "Unknown" || sources[2].Age != "never synced" || sources[2].LastErrorClass != "missing" || sources[2].LastSuccessfulSync != nil {
		t.Fatalf("unknown source = %+v", sources[2])
	}
	// Privacy: the identity hash is the only source identifier; raw local
	// paths never appear in the read model.
	if body := machinesResponse.Body.String(); strings.Contains(body, "/Users/") || strings.Contains(body, ".jsonl") {
		t.Fatalf("machines response leaks a local path:\n%s", body)
	}

	// The overview machine-health panel renders the per-source pills with the
	// shared Fresh/Stale/Unknown vocabulary.
	page := httptest.NewRecorder()
	testHandler(server).ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/", nil))
	if page.Code != http.StatusOK {
		t.Fatalf("overview status = %d", page.Code)
	}
	for _, want := range []string{"source-health-line", `status-pill connected">Fresh`, `status-pill stale">Stale`, "codex", "missing", "never synced"} {
		if !bytes.Contains(page.Body.Bytes(), []byte(want)) {
			t.Errorf("overview missing %q", want)
		}
	}

	// Old-agent compatibility: a heartbeat without the sources field keeps
	// the previously reported rows.
	oldAgent := sendHeartbeat(`{"machine_id":"linux-box","agent_version":"0.3.0","operating_system":"linux","architecture":"arm64","adapters":[{"id":"codex"}],"source_count":2,"source_error_count":0}`)
	if oldAgent.Code != http.StatusOK {
		t.Fatalf("old-agent heartbeat status = %d", oldAgent.Code)
	}
	machinesResponse = httptest.NewRecorder()
	testHandler(server).ServeHTTP(machinesResponse, httptest.NewRequest(http.MethodGet, "/api/v1/machines", nil))
	machines = nil
	if err := json.Unmarshal(machinesResponse.Body.Bytes(), &machines); err != nil {
		t.Fatal(err)
	}
	if len(machines) != 1 || len(machines[0].Sources) != 3 {
		t.Fatalf("old-agent heartbeat must keep existing source rows, got %+v", machines)
	}

	// A health-aware agent with zero sources clears its machine's rows.
	emptySources := sendHeartbeat(`{"machine_id":"linux-box","agent_version":"0.4.2","operating_system":"linux","architecture":"arm64","adapters":[],"source_count":0,"source_error_count":0,"sources":[]}`)
	if emptySources.Code != http.StatusOK {
		t.Fatalf("empty-sources heartbeat status = %d", emptySources.Code)
	}
	machinesResponse = httptest.NewRecorder()
	testHandler(server).ServeHTTP(machinesResponse, httptest.NewRequest(http.MethodGet, "/api/v1/machines", nil))
	machines = nil
	if err := json.Unmarshal(machinesResponse.Body.Bytes(), &machines); err != nil {
		t.Fatal(err)
	}
	if len(machines) != 1 || len(machines[0].Sources) != 0 {
		t.Fatalf("empty sources must clear rows, got %+v", machines)
	}

	// Validation: sources without an identity hash are rejected.
	rejected := sendHeartbeat(`{"machine_id":"linux-box","agent_version":"0.4.2","sources":[{"adapter":"codex","identity_hash":""}]}`)
	if rejected.Code != http.StatusBadRequest {
		t.Fatalf("heartbeat without identity hash status = %d, want %d", rejected.Code, http.StatusBadRequest)
	}
}

// The catalog UI is opt-in (TOKEMON_ENABLE_TOKEDEX, default off): the page
// routes 404 for authenticated users, the nav link disappears everywhere, and
// the read API and the auth boundary are unchanged.
func TestTokedexGatedBehindEnableFlag(t *testing.T) {
	store, err := database.Open(t.TempDir()+"/tokedex-gate.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	disabled, err := NewWithConfig(store, Config{IngestToken: "secret", DashboardToken: "dashboard"})
	if err != nil {
		t.Fatal(err)
	}
	get := func(server *Server, path string, authenticate bool) *httptest.ResponseRecorder {
		t.Helper()
		request := httptest.NewRequest(http.MethodGet, path, nil)
		if authenticate {
			request.SetBasicAuth("tokemon", "dashboard")
		}
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		return response
	}

	if response := get(disabled, "/tokedex", true); response.Code != http.StatusNotFound {
		t.Fatalf("disabled /tokedex status = %d, want %d", response.Code, http.StatusNotFound)
	}
	if response := get(disabled, "/catalog", true); response.Code != http.StatusNotFound {
		t.Fatalf("disabled /catalog status = %d, want %d", response.Code, http.StatusNotFound)
	}
	// The auth boundary stays intact: without credentials the route is still a
	// 401, not a 404, so the flag state does not leak to anonymous probes.
	if response := get(disabled, "/tokedex", false); response.Code != http.StatusUnauthorized {
		t.Fatalf("disabled unauthenticated /tokedex status = %d, want %d", response.Code, http.StatusUnauthorized)
	}
	for _, path := range []string{"/", "/analytics", "/sessions", "/settings"} {
		if body := get(disabled, path, true).Body.String(); strings.Contains(body, `href="/tokedex"`) {
			t.Fatalf("%s still advertises the gated Tokedex link", path)
		}
	}
	// The catalog read API is not part of the UI gate.
	if response := get(disabled, "/api/v1/catalog", true); response.Code != http.StatusOK {
		t.Fatalf("disabled /api/v1/catalog status = %d, want %d", response.Code, http.StatusOK)
	}

	enabled, err := NewWithConfig(store, Config{IngestToken: "secret", DashboardToken: "dashboard", EnableTokedex: true})
	if err != nil {
		t.Fatal(err)
	}
	if response := get(enabled, "/tokedex", true); response.Code != http.StatusOK {
		t.Fatalf("enabled /tokedex status = %d: %s", response.Code, response.Body.String())
	}
	if body := get(enabled, "/", true).Body.String(); !strings.Contains(body, `href="/tokedex"`) {
		t.Fatal("enabled overview lost the Tokedex navigation link")
	}
}

func TestCatalogSessionTimelineAndMachineHealthReadSurfaces(t *testing.T) {
	price := 1.0
	modelCatalog := catalog.MustNew(map[string]catalog.Model{
		"test-model": {Provider: "provider", DisplayName: "Test Model", Aliases: []string{"raw-model", "provider/raw-model"}, Tier: "S", Pricing: catalog.Pricing{Currency: "USD", Mode: "standard", VerifiedAt: "2026-07-12", Source: "https://example.com", InputPricePerMillion: &price, OutputPricePerMillion: &price}},
	})
	store, err := database.Open(t.TempDir()+"/tokemon.db", modelCatalog)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server, err := newTestServer(store, "secret")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	event := usage.Event{SchemaVersion: usage.SchemaVersion, EventID: "api-session", Timestamp: now, MachineID: "machine", SessionID: "session", Project: "/private/project", Provider: "provider", Model: "raw-model", Tool: "codex", InputTokens: usage.Int64(4), OutputTokens: usage.Int64(6), TotalTokens: usage.Int64(10), DurationMS: usage.Int64(1200), Cost: usage.Float64(.12), CostEstimated: true, TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "codex", AdapterVersion: "test"}}
	if _, err := store.Ingest(context.Background(), []usage.Event{event}); err != nil {
		t.Fatal(err)
	}
	heartbeat := httptest.NewRequest(http.MethodPost, "/api/v1/agents/heartbeat", strings.NewReader(`{"machine_id":"machine","agent_version":"test","operating_system":"linux","architecture":"amd64","adapters":[{"id":"codex"}],"source_count":1,"source_error_count":0}`))
	heartbeat.Header.Set("Authorization", "Bearer secret")
	heartbeatResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(heartbeatResponse, heartbeat)
	if heartbeatResponse.Code != http.StatusOK {
		t.Fatalf("heartbeat status = %d: %s", heartbeatResponse.Code, heartbeatResponse.Body.String())
	}

	checks := []struct {
		path string
		want []string
	}{
		{path: "/api/v1/catalog", want: []string{`"schema_version":"2"`, `"tier":"S"`, `"aliases":["raw-model","provider/raw-model"]`, `"usage_tokens":10`}},
		{path: "/api/v1/models", want: []string{`"display_name":"Test Model"`, `"context":"Observed in usage"`}},
		{path: "/api/v1/sessions?period=24h&machine=machine", want: []string{`"session_id":"session"`, `"input_tokens":4`, `"output_tokens":6`, `"total_tokens":10`, `"project":"project"`}},
		{path: "/api/v1/analytics/timeline?period=24h&machine=machine", want: []string{`"bucket":"hour"`, `"points"`}},
		{path: "/api/v1/machines", want: []string{`"status":"Connected"`, `"today_tokens":10`, `"sync_context":"1 source synced"`}},
	}
	for _, check := range checks {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, check.path, nil)
		request.SetBasicAuth("tokemon", testDashboardToken)
		server.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("%s status = %d: %s", check.path, response.Code, response.Body.String())
		}
		for _, want := range check.want {
			if !strings.Contains(response.Body.String(), want) {
				t.Fatalf("%s missing %q: %s", check.path, want, response.Body.String())
			}
		}
		if strings.Contains(response.Body.String(), "private/project") || strings.Contains(response.Body.String(), "conversation") {
			t.Fatalf("%s leaked private metadata: %s", check.path, response.Body.String())
		}
	}

	for _, path := range []string{"/", "/tokedex", "/sessions?period=24h&machine=machine"} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.SetBasicAuth("tokemon", testDashboardToken)
		server.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("%s status = %d", path, response.Code)
		}
		body := response.Body.String()
		for _, want := range []string{"Today", "This week", "Machine health", "Recent sessions"} {
			if path == "/" && !strings.Contains(body, want) {
				t.Fatalf("overview missing %q", want)
			}
		}
		if path == "/tokedex" && (!strings.Contains(body, "Tokedex") || !strings.Contains(body, "Aliases") || !strings.Contains(body, "Tier S")) {
			t.Fatalf("tokedex missing catalog content: %s", body)
		}
		if strings.Contains(body, "private/project") {
			t.Fatalf("%s leaked full project path", path)
		}
	}
	filtered := httptest.NewRecorder()
	filteredRequest := httptest.NewRequest(http.MethodGet, "/api/v1/analytics?period=24h&machine=other", nil)
	filteredRequest.SetBasicAuth("tokemon", testDashboardToken)
	server.Handler().ServeHTTP(filtered, filteredRequest)
	if filtered.Code != http.StatusOK || !strings.Contains(filtered.Body.String(), `"lifetime_tokens":10`) || !strings.Contains(filtered.Body.String(), `"tokens":0`) {
		t.Fatalf("filtered analytics changed global total: %d %s", filtered.Code, filtered.Body.String())
	}

	export := httptest.NewRecorder()
	exportRequest := httptest.NewRequest(http.MethodGet, "/api/v1/sessions/export?period=24h&machine=machine", nil)
	exportRequest.SetBasicAuth("tokemon", testDashboardToken)
	server.Handler().ServeHTTP(export, exportRequest)
	if export.Code != http.StatusOK || !strings.Contains(export.Header().Get("Content-Disposition"), "tokemon-sessions-24h.json") || !strings.Contains(export.Body.String(), `"session_id": "session"`) {
		t.Fatalf("session export = %d %s %s", export.Code, export.Header().Get("Content-Disposition"), export.Body.String())
	}
}

func TestDashboardRendersDailyTokenActivityField(t *testing.T) {
	store, err := database.Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server, err := newTestServer(store, "")
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
	testHandler(server).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
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
	server, err := newTestServer(store, "")
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
	testHandler(server).ServeHTTP(settings, httptest.NewRequest(http.MethodGet, "/settings", nil))
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
		"csrf_token":       {server.csrfToken()},
	}
	request := httptest.NewRequest(http.MethodPost, "/settings/aliases", strings.NewReader(form.Encode()))
	request.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	saved := httptest.NewRecorder()
	testHandler(server).ServeHTTP(saved, request)
	if saved.Code != http.StatusSeeOther || saved.Header().Get("Location") != "/settings?saved=1" {
		t.Fatalf("save response = %d, location %q", saved.Code, saved.Header().Get("Location"))
	}

	dashboard := httptest.NewRecorder()
	testHandler(server).ServeHTTP(dashboard, httptest.NewRequest(http.MethodGet, "/", nil))
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
	server, err := newTestServer(store, "")
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
	testHandler(server).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	body := response.Body.String()
	if !strings.Contains(body, `<div class="stage-chip">FORM / 09</div>`) {
		t.Fatalf("dashboard does not render the stage-nine form chip")
	}
	if !strings.Contains(body, `<strong>2B lifetime target</strong>`) {
		t.Fatalf("dashboard does not identify the next lifetime target")
	}
	if strings.Contains(body, `.creature-panel::after`) || strings.Contains(body, `content: "STAGE " attr(data-stage)`) {
		t.Fatalf("dashboard still renders the duplicate stage watermark")
	}
}

func TestDashboardRendersPopulatedEmptyAndBoundaryEvolutionStages(t *testing.T) {
	tests := []struct {
		name   string
		tokens int64
		stage  int
		form   string
		label  string
	}{
		{name: "empty egg", tokens: 0, stage: 0, form: "egg", label: "Egg"},
		{name: "populated context cub", tokens: 12_345, stage: 4, form: "context-cub", label: "Context Cub"},
		{name: "before late threshold", tokens: 1_999_999_999, stage: 9, form: "token-titan", label: "Token Titan"},
		{name: "late threshold", tokens: 2_000_000_000, stage: 10, form: "model-eater", label: "Model Eater"},
		{name: "final singularity", tokens: 1_000_000_000_000, stage: 18, form: "the-singularity", label: "The Singularity"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, err := database.Open(t.TempDir()+"/tokemon.db", catalog.Empty())
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			server, err := newTestServer(store, "")
			if err != nil {
				t.Fatal(err)
			}
			if test.tokens > 0 {
				event := usage.Event{
					SchemaVersion: usage.SchemaVersion,
					EventID:       "art-stage-" + strings.ReplaceAll(test.name, " ", "-"),
					Timestamp:     time.Now().UTC(),
					MachineID:     "art-test-machine",
					Provider:      "test",
					Model:         "test-model",
					Tool:          "test-tool",
					TotalTokens:   usage.Int64(test.tokens),
					TokenAccuracy: usage.AccuracyReported,
					Source:        usage.Source{Adapter: "test", AdapterVersion: "1"},
				}
				if _, err := store.Ingest(context.Background(), []usage.Event{event}); err != nil {
					t.Fatal(err)
				}
			}

			response := httptest.NewRecorder()
			testHandler(server).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
			if response.Code != http.StatusOK {
				t.Fatalf("dashboard status = %d, want 200: %s", response.Code, response.Body.String())
			}
			body := response.Body.String()
			for _, want := range []string{
				fmt.Sprintf(`data-stage="%d"`, test.stage),
				fmt.Sprintf(`src="/static/tokemon/stage-%02d.png"`, test.stage),
				fmt.Sprintf(`alt="%s, stage %d"`, test.label, test.stage),
				fmt.Sprintf(`<div class="stage-chip">FORM / %02d</div>`, test.stage),
			} {
				if !strings.Contains(body, want) {
					t.Fatalf("dashboard missing %q: %s", want, body)
				}
			}
		})
	}
}

func TestDashboardMissingAssetFallbackRuntimeContract(t *testing.T) {
	store, err := database.Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server, err := newTestServer(store, "")
	if err != nil {
		t.Fatal(err)
	}

	missing := httptest.NewRecorder()
	server.Handler().ServeHTTP(missing, httptest.NewRequest(http.MethodGet, "/static/tokemon/stage-99.png", nil))
	if missing.Code != http.StatusNotFound {
		t.Fatalf("missing stage status = %d, want 404", missing.Code)
	}

	page := httptest.NewRecorder()
	testHandler(server).ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/", nil))
	if page.Code != http.StatusOK {
		t.Fatalf("dashboard status = %d, want 200", page.Code)
	}
	body := page.Body.String()
	for _, want := range []string{
		`onerror="this.hidden=true;this.nextElementSibling.hidden=false"`,
		`<div class="creature-fallback" hidden>STAGE 0</div>`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("dashboard missing fallback contract %q: %s", want, body)
		}
	}
}

func TestDashboardPollsAndUpdatesLifetimeCounter(t *testing.T) {
	store, err := database.Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server, err := newTestServer(store, "")
	if err != nil {
		t.Fatal(err)
	}

	response := httptest.NewRecorder()
	testHandler(server).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
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
		`id="live-input-share"`,
		`id="live-output-share"`,
		`const percent = (value)`,
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
		`document.getElementById('live-input-share').textContent = percent(input)`,
		`document.getElementById('live-output-share').textContent = percent(output)`,
		`@media (max-width: 420px) { .composition-primary { grid-template-columns: 1fr; gap: 10px; } }`,
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
	server, err := newTestServer(store, "")
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	testHandler(server).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/evolution", nil))
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
	server, err := newTestServer(store, "secret")
	if err != nil {
		t.Fatal(err)
	}
	initial := usage.Event{SchemaVersion: usage.SchemaVersion, EventID: "cache-initial", Timestamp: time.Now().UTC(), MachineID: "machine", Provider: "openai", Model: "model", Tool: "codex", TotalTokens: usage.Int64(10), TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "codex", AdapterVersion: "test"}}
	if _, err := store.Ingest(context.Background(), []usage.Event{initial}); err != nil {
		t.Fatal(err)
	}

	handler := testHandler(server)
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
	server, err := newTestServer(store, "")
	if err != nil {
		t.Fatal(err)
	}
	event := usage.Event{SchemaVersion: usage.SchemaVersion, EventID: "concurrent-cache", Timestamp: time.Now().UTC(), MachineID: "machine", Provider: "openai", Model: "model", Tool: "codex", TotalTokens: usage.Int64(42), TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "codex", AdapterVersion: "test"}}
	if _, err := store.Ingest(context.Background(), []usage.Event{event}); err != nil {
		t.Fatal(err)
	}

	handler := testHandler(server)
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
	server, err := newTestServer(store, "")
	if err != nil {
		t.Fatal(err)
	}
	event := usage.Event{SchemaVersion: usage.SchemaVersion, EventID: "ttl-event", Timestamp: time.Now().UTC(), MachineID: "machine", Provider: "openai", Model: "model", Tool: "codex", TotalTokens: usage.Int64(7), TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "codex", AdapterVersion: "test"}}
	if _, err := store.Ingest(context.Background(), []usage.Event{event}); err != nil {
		t.Fatal(err)
	}

	handler := testHandler(server)
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

func TestOverviewEndpointCachesUntilSuccessfulIngest(t *testing.T) {
	store, err := database.Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server, err := newTestServer(store, "")
	if err != nil {
		t.Fatal(err)
	}
	initial := usage.Event{SchemaVersion: usage.SchemaVersion, EventID: "overview-cache-initial", Timestamp: time.Now().UTC(), MachineID: "machine", Provider: "openai", Model: "model", Tool: "codex", TotalTokens: usage.Int64(10), TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "codex", AdapterVersion: "test"}}
	if _, err := store.Ingest(context.Background(), []usage.Event{initial}); err != nil {
		t.Fatal(err)
	}

	handler := testHandler(server)
	first := httptest.NewRecorder()
	handler.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/api/v1/analytics/overview", nil))
	if first.Code != http.StatusOK || server.overviewCache == nil {
		t.Fatalf("initial overview response = %d %s, cache = %#v", first.Code, first.Body.String(), server.overviewCache)
	}
	cached := server.overviewCache
	second := httptest.NewRecorder()
	handler.ServeHTTP(second, httptest.NewRequest(http.MethodGet, "/api/v1/analytics/overview", nil))
	if second.Code != http.StatusOK || server.overviewCache != cached || second.Body.String() != first.Body.String() {
		t.Fatalf("repeated overview response missed cache: first=%s second=%s", first.Body.String(), second.Body.String())
	}
	dashboard := httptest.NewRecorder()
	handler.ServeHTTP(dashboard, httptest.NewRequest(http.MethodGet, "/", nil))
	if dashboard.Code != http.StatusOK || server.overviewCache != cached {
		t.Fatalf("dashboard response = %d, cache was not reused", dashboard.Code)
	}

	update := usage.Event{SchemaVersion: usage.SchemaVersion, EventID: "overview-cache-update", Timestamp: time.Now().UTC(), MachineID: "machine", Provider: "openai", Model: "model", Tool: "codex", TotalTokens: usage.Int64(15), TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "codex", AdapterVersion: "test"}}
	payload, err := json.Marshal(batchRequest{Events: []usage.Event{update}})
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodPost, "/api/v1/events/batch", bytes.NewReader(payload))
	accepted := httptest.NewRecorder()
	handler.ServeHTTP(accepted, request)
	if accepted.Code != http.StatusOK {
		t.Fatalf("ingest response = %d %s", accepted.Code, accepted.Body.String())
	}

	// Stale-while-revalidate: the ingest marks the cached result stale but
	// keeps it, so the next reader is served immediately while one background
	// compute refreshes it. Poll until the refreshed value is visible.
	deadline := time.Now().Add(3 * time.Second)
	for {
		refreshed := httptest.NewRecorder()
		handler.ServeHTTP(refreshed, httptest.NewRequest(http.MethodGet, "/api/v1/analytics/overview", nil))
		if refreshed.Code == http.StatusOK && strings.Contains(refreshed.Body.String(), `"lifetime_tokens":25`) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("overview did not refresh after ingest: %d %s", refreshed.Code, refreshed.Body.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestOverviewEndpointCacheHandlesConcurrentPolling(t *testing.T) {
	store, err := database.Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server, err := newTestServer(store, "")
	if err != nil {
		t.Fatal(err)
	}
	event := usage.Event{SchemaVersion: usage.SchemaVersion, EventID: "overview-concurrent-cache", Timestamp: time.Now().UTC(), MachineID: "machine", Provider: "openai", Model: "model", Tool: "codex", TotalTokens: usage.Int64(42), TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "codex", AdapterVersion: "test"}}
	if _, err := store.Ingest(context.Background(), []usage.Event{event}); err != nil {
		t.Fatal(err)
	}

	handler := testHandler(server)
	start := make(chan struct{})
	var wait sync.WaitGroup
	for worker := 0; worker < 8; worker++ {
		wait.Add(1)
		go func() {
			defer wait.Done()
			<-start
			for requestIndex := 0; requestIndex < 5; requestIndex++ {
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/analytics/overview", nil))
				body := response.Body.String()
				if response.Code != http.StatusOK || (!strings.Contains(body, `"lifetime_tokens":42`) && !strings.Contains(body, `"lifetime_tokens":43`)) {
					t.Errorf("concurrent overview response = %d %s", response.Code, response.Body.String())
					return
				}
			}
		}()
	}
	wait.Add(1)
	go func() {
		defer wait.Done()
		<-start
		update := usage.Event{SchemaVersion: usage.SchemaVersion, EventID: "overview-concurrent-update", Timestamp: time.Now().UTC(), MachineID: "machine", Provider: "openai", Model: "model", Tool: "codex", TotalTokens: usage.Int64(1), TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "codex", AdapterVersion: "test"}}
		payload, err := json.Marshal(batchRequest{Events: []usage.Event{update}})
		if err != nil {
			t.Errorf("marshal concurrent overview update: %v", err)
			return
		}
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/api/v1/events/batch", bytes.NewReader(payload)))
		if response.Code != http.StatusOK {
			t.Errorf("concurrent overview ingest response = %d %s", response.Code, response.Body.String())
		}
	}()
	close(start)
	wait.Wait()

	final := httptest.NewRecorder()
	handler.ServeHTTP(final, httptest.NewRequest(http.MethodGet, "/api/v1/analytics/overview", nil))
	if final.Code != http.StatusOK || !strings.Contains(final.Body.String(), `"lifetime_tokens":43`) {
		t.Fatalf("final overview response = %d %s", final.Code, final.Body.String())
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
	server, err := newTestServer(store, "")
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
	testHandler(server).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	body := response.Body.Bytes()
	for _, want := range []string{"Est. API cost", "$10.00", "1M priced", "500K unpriced"} {
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

func TestAnalyticsExportSupportsCSV(t *testing.T) {
	store, err := database.Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	events := []usage.Event{
		{SchemaVersion: usage.SchemaVersion, EventID: "an-csv-1", Timestamp: time.Now().UTC(), MachineID: "machine", SessionID: "sess-a", Project: "alpha", Provider: "provider", Model: "model", Tool: "tool", TotalTokens: usage.Int64(700), TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "test", AdapterVersion: "1"}},
		{SchemaVersion: usage.SchemaVersion, EventID: "an-csv-2", Timestamp: time.Now().UTC(), MachineID: "machine", SessionID: "sess-b", Project: "beta", Provider: "provider", Model: "model", Tool: "tool", TotalTokens: usage.Int64(300), TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "test", AdapterVersion: "1"}},
	}
	if _, err := store.Ingest(context.Background(), events); err != nil {
		t.Fatal(err)
	}
	server, err := newTestServer(store, "")
	if err != nil {
		t.Fatal(err)
	}

	response := httptest.NewRecorder()
	testHandler(server).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/analytics/export?dimension=projects&period=30d&format=csv", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("csv export status = %d", response.Code)
	}
	if got := response.Header().Get("Content-Type"); got != "text/csv; charset=utf-8" {
		t.Fatalf("csv content type = %q", got)
	}
	if got := response.Header().Get("Content-Disposition"); !strings.Contains(got, "tokemon-analytics-projects-30d.csv") {
		t.Fatalf("csv disposition = %q", got)
	}
	rows := strings.Split(strings.TrimRight(response.Body.String(), "\n"), "\n")
	if len(rows) != 3 {
		t.Fatalf("csv rows = %d, want header + 2 breakdown rows", len(rows))
	}
	if !strings.HasPrefix(rows[0], "dimension,period,name,tokens,events,sessions,share_percent") {
		t.Fatalf("csv header = %q", rows[0])
	}
	if !strings.Contains(rows[1], "projects,30d,alpha,700,1,1,70.0") {
		t.Fatalf("alpha row = %q", rows[1])
	}
	if !strings.Contains(rows[2], "projects,30d,beta,300,1,1,30.0") {
		t.Fatalf("beta row = %q", rows[2])
	}

	// default stays JSON
	jsonResponse := httptest.NewRecorder()
	testHandler(server).ServeHTTP(jsonResponse, httptest.NewRequest(http.MethodGet, "/api/v1/analytics/export?dimension=projects&period=30d", nil))
	if got := jsonResponse.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Fatalf("default export content type = %q", got)
	}
}
func TestSettingsShowsAuthModeAndTokenSource(t *testing.T) {
	store, err := database.Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Token-only mode with an explicit dashboard token (newTestServer default).
	server, err := newTestServer(store, "secret")
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	testHandler(server).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/settings", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("settings status = %d", response.Code)
	}
	body := response.Body.Bytes()
	for _, want := range []string{"Auth mode: <strong>Dashboard token</strong>", "explicit dashboard token"} {
		if !bytes.Contains(body, []byte(want)) {
			t.Fatalf("settings page does not contain %q", want)
		}
	}

	// Fallback mode: dashboard token came from the documented ingest default.
	fallbackServer, err := NewWithConfig(store, Config{IngestToken: "secret", DashboardToken: "secret", DashboardTokenFallback: true})
	if err != nil {
		t.Fatal(err)
	}
	fallbackResponse := httptest.NewRecorder()
	fallbackServer.Handler().ServeHTTP(fallbackResponse, httptest.NewRequest(http.MethodGet, "/settings", nil))
	// Token auth: Basic auth any username with the dashboard token.
	fallbackRequest := httptest.NewRequest(http.MethodGet, "/settings", nil)
	fallbackRequest.SetBasicAuth("ops", "secret")
	fallbackServer.Handler().ServeHTTP(fallbackResponse, fallbackRequest)
	for _, want := range []string{"Auth mode: <strong>Dashboard token</strong>", "ingest token (documented fallback)"} {
		if !bytes.Contains(fallbackResponse.Body.Bytes(), []byte(want)) {
			t.Fatalf("fallback settings page does not contain %q", want)
		}
	}

	// Password account mode.
	hash, err := bcrypt.GenerateFromPassword([]byte("human-password"), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	passwordServer, err := NewWithConfig(store, Config{IngestToken: "secret", DashboardToken: "secret", DashboardUsername: "ops", DashboardPasswordHash: hash})
	if err != nil {
		t.Fatal(err)
	}
	passwordRequest := httptest.NewRequest(http.MethodGet, "/settings", nil)
	passwordRequest.SetBasicAuth("ops", "human-password")
	passwordResponse := httptest.NewRecorder()
	passwordServer.Handler().ServeHTTP(passwordResponse, passwordRequest)
	if passwordResponse.Code != http.StatusOK {
		t.Fatalf("password settings status = %d", passwordResponse.Code)
	}
	if !bytes.Contains(passwordResponse.Body.Bytes(), []byte("Auth mode: <strong>Password account</strong>")) {
		t.Fatalf("password settings page missing auth mode: %s", passwordResponse.Body.String())
	}
}
func TestSessionsExportSupportsCSV(t *testing.T) {
	store, err := database.Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	events := []usage.Event{
		{SchemaVersion: usage.SchemaVersion, EventID: "csv-1", Timestamp: time.Now().UTC(), MachineID: "machine", SessionID: "sess_csv", Project: "project,with-commas", Provider: "provider", Model: "model", Tool: "tool", InputTokens: usage.Int64(10), OutputTokens: usage.Int64(5), TotalTokens: usage.Int64(15), Cost: usage.Float64(0.25), TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "test", AdapterVersion: "1"}},
		{SchemaVersion: usage.SchemaVersion, EventID: "csv-2", Timestamp: time.Now().UTC(), MachineID: "machine", SessionID: "sess_csv", Provider: "provider", Model: "model", Tool: "tool", TokenAccuracy: usage.AccuracyUnknown, Source: usage.Source{Adapter: "test", AdapterVersion: "1"}},
	}
	if _, err := store.Ingest(context.Background(), events); err != nil {
		t.Fatal(err)
	}
	server, err := newTestServer(store, "")
	if err != nil {
		t.Fatal(err)
	}

	response := httptest.NewRecorder()
	testHandler(server).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/sessions/export?format=csv", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("csv export status = %d", response.Code)
	}
	if got := response.Header().Get("Content-Type"); got != "text/csv; charset=utf-8" {
		t.Fatalf("csv content type = %q", got)
	}
	if got := response.Header().Get("Content-Disposition"); !strings.Contains(got, "tokemon-sessions-30d.csv") {
		t.Fatalf("csv disposition = %q", got)
	}
	rows := strings.Split(strings.TrimRight(response.Body.String(), "\n"), "\n")
	if len(rows) != 2 {
		t.Fatalf("csv rows = %d, want header + 1 session", len(rows))
	}
	if !strings.HasPrefix(rows[0], "session_id,timestamp,machine,provider,model,tool,project,") {
		t.Fatalf("csv header = %q", rows[0])
	}
	// unknown token fields render as empty cells, never zero; the comma
	// project is quoted per RFC 4180.
	// The session aggregates both events: unknown totals collapse to empty
	// cells (never zero), mixed accuracies become unknown, cost sums.
	if !strings.Contains(rows[1], `"project,with-commas"`) || !strings.Contains(rows[1], ",,0.25,false,,unknown,2") {
		t.Fatalf("csv data row = %q", rows[1])
	}

	// default stays JSON
	jsonResponse := httptest.NewRecorder()
	testHandler(server).ServeHTTP(jsonResponse, httptest.NewRequest(http.MethodGet, "/api/v1/sessions/export", nil))
	if got := jsonResponse.Header().Get("Content-Type"); got != "application/json; charset=utf-8" {
		t.Fatalf("default export content type = %q", got)
	}
}
func TestSessionsPageRendersSortableHeadersAndOrder(t *testing.T) {
	store, err := database.Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	session := func(id string, tokens int64) usage.Event {
		return usage.Event{SchemaVersion: usage.SchemaVersion, EventID: "ev-" + id, Timestamp: time.Now().UTC(), MachineID: "machine", SessionID: id, Provider: "provider", Model: "model", Tool: "tool", InputTokens: usage.Int64(tokens), OutputTokens: usage.Int64(0), TotalTokens: usage.Int64(tokens), TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "test", AdapterVersion: "1"}}
	}
	if _, err := store.Ingest(context.Background(), []usage.Event{session("small", 100), session("large", 900)}); err != nil {
		t.Fatal(err)
	}
	server, err := newTestServer(store, "")
	if err != nil {
		t.Fatal(err)
	}

	response := httptest.NewRecorder()
	testHandler(server).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/sessions?sort=tokens&dir=asc", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("sessions status = %d", response.Code)
	}
	body := response.Body.Bytes()
	for _, want := range []string{`aria-sort="ascending"`, "dir=desc&amp;sort=tokens", "small", "large"} {
		if !bytes.Contains(body, []byte(want)) {
			t.Fatalf("sessions page does not contain %q", want)
		}
	}
	if bytes.Contains(body, []byte("DROP")) {
		t.Fatal("sessions page leaked a raw sort expression")
	}

	// invalid sort key falls back to the timestamp default without error
	fallback := httptest.NewRecorder()
	testHandler(server).ServeHTTP(fallback, httptest.NewRequest(http.MethodGet, "/sessions?sort=bogus%3B--&dir=asc", nil))
	if fallback.Code != http.StatusOK || !bytes.Contains(fallback.Body.Bytes(), []byte("aria-sort=")) {
		t.Fatalf("fallback sort status = %d", fallback.Code)
	}
}

func TestDashboardShowsDataFreshness(t *testing.T) {
	store, err := database.Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server, err := newTestServer(store, "")
	if err != nil {
		t.Fatal(err)
	}

	// Fresh: an event just ingested.
	recent := usage.Event{SchemaVersion: usage.SchemaVersion, EventID: "freshness-recent", Timestamp: time.Now().UTC().Add(-2 * time.Minute), MachineID: "machine", Provider: "provider", Model: "model", Tool: "tool", TotalTokens: usage.Int64(50), TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "test", AdapterVersion: "1"}}
	if _, err := store.Ingest(context.Background(), []usage.Event{recent}); err != nil {
		t.Fatal(err)
	}
	fresh := httptest.NewRecorder()
	testHandler(server).ServeHTTP(fresh, httptest.NewRequest(http.MethodGet, "/", nil))
	if !bytes.Contains(fresh.Body.Bytes(), []byte("status-pill connected")) || !bytes.Contains(fresh.Body.Bytes(), []byte("Fresh")) {
		t.Fatalf("fresh dashboard lacks the fresh pill: %s", fresh.Body.String())
	}

	// Stale: only an old event remains (new store replaces nothing; use the same store with an old-dated event).
	stale := usage.Event{SchemaVersion: usage.SchemaVersion, EventID: "freshness-stale", Timestamp: time.Now().UTC().Add(-3 * time.Hour), MachineID: "machine", Provider: "provider", Model: "model", Tool: "tool", TotalTokens: usage.Int64(70), TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "test", AdapterVersion: "1"}}
	if _, err := store.Ingest(context.Background(), []usage.Event{stale}); err != nil {
		t.Fatal(err)
	}
	// Newest event is still the recent one, so force staleness by checking the stale path through the helper.
	state := dataFreshnessFor(&stale.Timestamp, time.Now().UTC())
	if state.Class != "stale" || state.State != "Stale" {
		t.Fatalf("stale state = %+v", state)
	}
	none := dataFreshnessFor(nil, time.Now().UTC())
	if none.Class != "" || none.State != "Unknown" {
		t.Fatalf("unknown state = %+v", none)
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
	server, err := newTestServer(store, "")
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	testHandler(server).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
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
	server, err := newTestServer(store, "")
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
	testHandler(server).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
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

func TestDashboardUsesCompactBreakdownTotalsWithExactTooltips(t *testing.T) {
	store, err := database.Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	event := usage.Event{SchemaVersion: usage.SchemaVersion, EventID: "compact-breakdown", Timestamp: time.Now().UTC(), MachineID: "workstation", Project: "tokemon", Provider: "openai", Model: "gpt-5.6-luna", Tool: "codex", TotalTokens: usage.Int64(1_165_236_062), TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "test", AdapterVersion: "1"}}
	if _, err := store.Ingest(context.Background(), []usage.Event{event}); err != nil {
		t.Fatal(err)
	}
	server, err := newTestServer(store, "")
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	testHandler(server).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
	body := response.Body.String()
	if !strings.Contains(body, `title="1,165,236,062 tokens" aria-label="1,165,236,062 tokens">1.2B</td>`) {
		t.Fatalf("dashboard does not pair a compact total with its exact tooltip: %s", body)
	}
	if !strings.Contains(body, `th:first-child, td:first-child { width: 60%; }`) {
		t.Fatal("dashboard does not reserve additional width for breakdown names")
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
		"zcode":        "zcode",
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
	if got := harnessName("zcode"); got != "ZCode" {
		t.Fatalf("ZCode harness label = %q", got)
	}
}

func TestDashboardRendersFriendlyHarnessNames(t *testing.T) {
	store, err := database.Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server, err := newTestServer(store, "")
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
	testHandler(server).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
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
	server, err := newTestServer(store, "")
	if err != nil {
		t.Fatal(err)
	}

	response := httptest.NewRecorder()
	testHandler(server).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
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
	server, err := newTestServer(store, "")
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
	testHandler(server).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
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
	server, err := newTestServer(store, "")
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
	testHandler(server).ServeHTTP(pageResponse, httptest.NewRequest(http.MethodGet, "/analytics?period=7d&dimension=models&machine=machine-one", nil))
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
	testHandler(server).ServeHTTP(exportResponse, httptest.NewRequest(http.MethodGet, "/api/v1/analytics/export?period=7d&dimension=models", nil))
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

func TestAnalyticsUnknownBucketLabelsAreDimensionAware(t *testing.T) {
	// All five dimensions get a dimension-aware label for the empty bucket.
	wantLabels := map[string]string{
		"projects":   "Unknown project",
		"models":     "Unknown model",
		"machines":   "Unknown machine",
		"harnesses":  "Unknown harness",
		"providers":  "Unknown provider",
		"everything": "Unknown project", // unknown dimensions default to projects
	}
	for dimension, want := range wantLabels {
		if got := analyticsUnknownLabel(dimension); got != want {
			t.Errorf("analyticsUnknownLabel(%q) = %q, want %q", dimension, got, want)
		}
	}
	if got := breakdownDisplayName("projects", nil, "Unknown"); got != "Unknown project" {
		t.Errorf("breakdownDisplayName projects sentinel = %q", got)
	}
	if got := breakdownDisplayName("projects", nil, "project-a"); got != "project-a" {
		t.Errorf("breakdownDisplayName projects real name = %q", got)
	}

	store, err := database.Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server, err := newTestServer(store, "")
	if err != nil {
		t.Fatal(err)
	}
	// The two sentinels reachable through ingest: the empty project and the
	// Claude adapter's literal "unknown" model. Both funnel into the store's
	// 'Unknown'/'unknown' bucket; the display layer picks the label.
	events := []usage.Event{
		{
			SchemaVersion: usage.SchemaVersion, EventID: "unknown-labels-project", Timestamp: time.Now().UTC(),
			MachineID: "machine-one", SessionID: "session-one", Provider: "anthropic",
			Model: "claude", Tool: "claude-code", TotalTokens: usage.Int64(50), TokenAccuracy: usage.AccuracyReported,
			Source: usage.Source{Adapter: "claude-code", AdapterVersion: "test"},
		},
		{
			SchemaVersion: usage.SchemaVersion, EventID: "unknown-labels-model", Timestamp: time.Now().UTC(),
			MachineID: "machine-one", SessionID: "session-two", Project: "project-b", Provider: "anthropic",
			Model: "unknown", Tool: "claude-code", TotalTokens: usage.Int64(30), TokenAccuracy: usage.AccuracyReported,
			Source: usage.Source{Adapter: "claude-code", AdapterVersion: "test"},
		},
	}
	if result, err := store.Ingest(context.Background(), events); err != nil || result.Accepted != 2 {
		t.Fatalf("ingest = %+v, err %v", result, err)
	}

	for _, test := range []struct{ dimension, label string }{
		{"projects", "Unknown project"},
		{"models", "Unknown model"},
	} {
		page := httptest.NewRecorder()
		testHandler(server).ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/analytics?period=30d&dimension="+test.dimension, nil))
		if page.Code != http.StatusOK {
			t.Fatalf("analytics page (%s) status = %d: %s", test.dimension, page.Code, page.Body.String())
		}
		if !bytes.Contains(page.Body.Bytes(), []byte(`class="unknown-value">`+test.label+`</span>`)) {
			t.Errorf("analytics %s breakdown missing unknown label %q", test.dimension, test.label)
		}
		if !bytes.Contains(page.Body.Bytes(), []byte(">"+test.label+" share</th>")) {
			t.Errorf("analytics %s usage-share table missing %q column header", test.dimension, test.label)
		}

		csvResponse := httptest.NewRecorder()
		testHandler(server).ServeHTTP(csvResponse, httptest.NewRequest(http.MethodGet, "/api/v1/analytics/export?period=30d&dimension="+test.dimension+"&format=csv", nil))
		if csvResponse.Code != http.StatusOK {
			t.Fatalf("analytics csv (%s) status = %d", test.dimension, csvResponse.Code)
		}
		if !strings.Contains(csvResponse.Body.String(), ","+test.label+",") {
			t.Errorf("analytics %s csv missing %q name cell: %s", test.dimension, test.label, csvResponse.Body.String())
		}
	}

	// The JSON API keeps the raw sentinel — display-time mapping only.
	jsonResponse := httptest.NewRecorder()
	testHandler(server).ServeHTTP(jsonResponse, httptest.NewRequest(http.MethodGet, "/api/v1/analytics/export?period=30d&dimension=projects", nil))
	if jsonResponse.Code != http.StatusOK {
		t.Fatalf("analytics json status = %d", jsonResponse.Code)
	}
	var exported database.Analytics
	if err := json.Unmarshal(jsonResponse.Body.Bytes(), &exported); err != nil {
		t.Fatalf("decode analytics export: %v", err)
	}
	if len(exported.Breakdown) != 2 {
		t.Fatalf("json export breakdown rows = %d, want 2: %+v", len(exported.Breakdown), exported.Breakdown)
	}
	var sentinel *database.AnalyticsBreakdown
	for index := range exported.Breakdown {
		if exported.Breakdown[index].Name == "Unknown" {
			sentinel = &exported.Breakdown[index]
		}
	}
	if sentinel == nil || sentinel.Tokens != 50 {
		t.Fatalf("json export must keep the raw sentinel, got %+v", exported.Breakdown)
	}
}

func TestTokedexCoverageLineAccountsForUnknownPricing(t *testing.T) {
	// The Tokedex line must not claim unknown-mode (unpublished rates)
	// catalog entries as priced, and the uncataloged count must exclude the
	// documented non-model sentinels.
	price := 2.0
	modelCatalog := catalog.MustNew(map[string]catalog.Model{
		"priced-model":   {Provider: "test", DisplayName: "Priced Model", Aliases: []string{"priced-model"}, Tier: "S", Pricing: catalog.Pricing{Currency: "USD", Mode: "standard", VerifiedAt: "2026-11-07", Source: "https://example.com", InputPricePerMillion: &price, OutputPricePerMillion: &price}},
		"awaiting-rates": {Provider: "test", DisplayName: "Awaiting Rates", Aliases: []string{"awaiting-alias"}, Pricing: catalog.Pricing{Currency: "USD", Mode: "unknown", VerifiedAt: "2026-11-07"}},
	})
	store, err := database.Open(t.TempDir()+"/tokemon.db", modelCatalog)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server, err := newTestServer(store, "")
	if err != nil {
		t.Fatal(err)
	}
	events := []usage.Event{
		{SchemaVersion: usage.SchemaVersion, EventID: "line-priced", Timestamp: time.Now().UTC(), MachineID: "machine", Provider: "test", Model: "priced-model", Tool: "tool", TotalTokens: usage.Int64(1_000_000), TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "test", AdapterVersion: "1"}},
		{SchemaVersion: usage.SchemaVersion, EventID: "line-awaiting", Timestamp: time.Now().UTC(), MachineID: "machine", Provider: "test", Model: "awaiting-alias", Tool: "tool", TotalTokens: usage.Int64(400_000), TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "test", AdapterVersion: "1"}},
		{SchemaVersion: usage.SchemaVersion, EventID: "line-mystery", Timestamp: time.Now().UTC(), MachineID: "machine", Provider: "test", Model: "mystery-model", Tool: "tool", TotalTokens: usage.Int64(200_000), TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "test", AdapterVersion: "1"}},
		{SchemaVersion: usage.SchemaVersion, EventID: "line-sentinel", Timestamp: time.Now().UTC(), MachineID: "machine", Provider: "test", Model: "unknown", Tool: "tool", TotalTokens: usage.Int64(100_000), TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "test", AdapterVersion: "1"}},
	}
	if result, err := store.Ingest(context.Background(), events); err != nil || result.Accepted != len(events) {
		t.Fatalf("ingest = %+v, err %v", result, err)
	}

	page := httptest.NewRecorder()
	testHandler(server).ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/tokedex", nil))
	if page.Code != http.StatusOK {
		t.Fatalf("tokedex status = %d", page.Code)
	}
	body := page.Body.String()
	for _, want := range []string{
		"Pricing coverage:",
		"in cataloged models with unpublished rates",
		"1 uncataloged model below",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("tokedex coverage line missing %q", want)
		}
	}
	// The old line counted every disclosed identifier as a model.
	if strings.Contains(body, "3 uncataloged models") {
		t.Errorf("tokedex coverage line still counts sentinels as models:\n%s", body[:600])
	}
}

func TestTokedexTierLegendAndEmptyStates(t *testing.T) {
	// The Tokedex MVP states: every tier that has models gets a legend row
	// saying tiers are bundled YAML judgment, unknown pricing says rates are
	// not published yet, zero-usage models say so instead of rendering zero,
	// and a fully cataloged deployment says nothing is uncataloged.
	price := 2.0
	modelCatalog := catalog.MustNew(map[string]catalog.Model{
		"priced-model": {Provider: "test", DisplayName: "Priced Model", Aliases: []string{"priced-model"}, Tier: "S", Pricing: catalog.Pricing{Currency: "USD", Mode: "standard", VerifiedAt: "2026-11-07", Source: "https://example.com", InputPricePerMillion: &price, OutputPricePerMillion: &price}},
		"zero-model":   {Provider: "test", DisplayName: "Zero Model", Aliases: []string{"zero-model"}, Tier: "A", Pricing: catalog.Pricing{Currency: "USD", Mode: "standard", VerifiedAt: "2026-11-07", Source: "https://example.com", InputPricePerMillion: &price, OutputPricePerMillion: &price}},
		"awaiting":     {Provider: "test", DisplayName: "Awaiting Rates", Aliases: []string{"awaiting"}, Tier: "B", Pricing: catalog.Pricing{Currency: "USD", Mode: "unknown", VerifiedAt: "2026-11-07"}},
		"unfamiliar":   {Provider: "test", DisplayName: "Unfamiliar", Aliases: []string{"unfamiliar"}, Tier: "Z", Pricing: catalog.Pricing{Currency: "USD", Mode: "standard", VerifiedAt: "2026-11-07", Source: "https://example.com", InputPricePerMillion: &price, OutputPricePerMillion: &price}},
	})
	store, err := database.Open(t.TempDir()+"/tokemon.db", modelCatalog)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server, err := newTestServer(store, "")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	events := []usage.Event{
		{SchemaVersion: usage.SchemaVersion, EventID: "mvp-priced", Timestamp: now, MachineID: "machine", Provider: "test", Model: "priced-model", Tool: "tool", TotalTokens: usage.Int64(1_000_000), TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "test", AdapterVersion: "1"}},
		{SchemaVersion: usage.SchemaVersion, EventID: "mvp-awaiting", Timestamp: now, MachineID: "machine", Provider: "test", Model: "awaiting", Tool: "tool", TotalTokens: usage.Int64(400_000), TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "test", AdapterVersion: "1"}},
	}
	if result, err := store.Ingest(context.Background(), events); err != nil || result.Accepted != len(events) {
		t.Fatalf("ingest = %+v, err %v", result, err)
	}

	page := httptest.NewRecorder()
	testHandler(server).ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/tokedex", nil))
	if page.Code != http.StatusOK {
		t.Fatalf("tokedex status = %d", page.Code)
	}
	body := page.Body.String()
	for _, want := range []string{
		// Tier legend: one row per populated tier, and the provenance caveat.
		`id="tier-legend-title">Tier legend</h2>`,
		"a maintainer judgment, not measured model quality",
		"Frontier capability",
		"Balanced everyday models",
		"Not yet tiered",
		// Unknown pricing: explicit copy, never a zero or an empty cell.
		"Rates not published yet",
		"status checked 2026-11-07",
		// Zero-usage catalog model: an empty state, not "0 tokens".
		"No usage recorded yet",
		// Fully cataloged: the uncataloged empty state replaces the section.
		"Every observed model resolves to a bundled catalog entry",
		// Nullable numeric rule survives on the page.
		"Unknown pricing stays Unknown, never zero",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("tokedex MVP page missing %q", want)
		}
	}
	if rows := strings.Count(body, `class="legend-row"`); rows != 4 {
		t.Errorf("tier legend rows = %d, want 4 (S, A, B, Unranked)", rows)
	}
	if strings.Contains(body, `>0</strong> tokens`) {
		t.Errorf("zero-usage model still renders a zero token count")
	}
	if strings.Contains(body, `id="uncataloged-title"`) {
		t.Errorf("fully cataloged deployment still renders the uncataloged section")
	}
	if strings.Contains(body, "No catalog models are configured.") {
		t.Errorf("populated catalog rendered the empty-catalog state")
	}
}

func TestTokedexEmptyCatalogStates(t *testing.T) {
	// An empty catalog with no usage must explain both absences instead of
	// rendering bare chrome.
	store, err := database.Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server, err := newTestServer(store, "")
	if err != nil {
		t.Fatal(err)
	}
	page := httptest.NewRecorder()
	testHandler(server).ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/tokedex", nil))
	if page.Code != http.StatusOK {
		t.Fatalf("tokedex status = %d", page.Code)
	}
	body := page.Body.String()
	for _, want := range []string{
		"No catalog models are configured.",
		"Add entries to <code>catalog/models.yaml</code> to populate the Tokedex.",
		"No usage has been observed yet, so there is nothing to reconcile against the catalog.",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("empty-catalog page missing %q", want)
		}
	}
	if strings.Contains(body, `id="tier-legend-title"`) {
		t.Errorf("empty catalog rendered a tier legend with nothing to explain")
	}
	if strings.Contains(body, "Every observed model resolves") {
		t.Errorf("empty catalog claimed every observed model is cataloged")
	}
}

func TestTokedexUncatalogedSectionStillRenders(t *testing.T) {
	// Usage on an identifier the catalog does not know keeps the uncataloged
	// section; the empty state must not swallow it.
	store, err := database.Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server, err := newTestServer(store, "")
	if err != nil {
		t.Fatal(err)
	}
	event := usage.Event{SchemaVersion: usage.SchemaVersion, EventID: "uncat", Timestamp: time.Now().UTC(), MachineID: "machine", Provider: "test", Model: "mystery-model", Tool: "tool", TotalTokens: usage.Int64(5), TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "test", AdapterVersion: "1"}}
	if _, err := store.Ingest(context.Background(), []usage.Event{event}); err != nil {
		t.Fatal(err)
	}
	page := httptest.NewRecorder()
	testHandler(server).ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/tokedex", nil))
	if page.Code != http.StatusOK {
		t.Fatalf("tokedex status = %d", page.Code)
	}
	body := page.Body.String()
	if !strings.Contains(body, `id="uncataloged-title"`) || !strings.Contains(body, "mystery-model") {
		t.Fatalf("uncataloged section missing for an unknown observed model")
	}
	if strings.Contains(body, "Every observed model resolves") {
		t.Fatalf("uncataloged empty state rendered alongside a real uncataloged model")
	}
}

func TestTokedexFilterSortAndSearchControls(t *testing.T) {
	// The card grid is filtered, sorted, and searched entirely in the browser:
	// the controls are labeled, every card carries the metadata the script
	// reads, and the full grid is server-rendered so it survives without JS.
	price := 2.0
	modelCatalog := catalog.MustNew(map[string]catalog.Model{
		"claude-sonnet-4": {Provider: "anthropic", DisplayName: "Claude Sonnet 4", Aliases: []string{"claude-sonnet-4", "sonnet"}, Tier: "S", Pricing: catalog.Pricing{Currency: "USD", Mode: "standard", VerifiedAt: "2026-11-07", Source: "https://example.com", InputPricePerMillion: &price, OutputPricePerMillion: &price}},
		"gpt-5":           {Provider: "openai", DisplayName: "GPT-5", Aliases: []string{"gpt-5"}, Tier: "A", Pricing: catalog.Pricing{Currency: "USD", Mode: "unknown", VerifiedAt: "2026-11-07"}},
		"zero-usage":      {Provider: "openai", DisplayName: "Zero Usage", Aliases: []string{"zero-usage"}, Tier: "B", Pricing: catalog.Pricing{Currency: "USD", Mode: "standard", VerifiedAt: "2026-11-07", Source: "https://example.com", InputPricePerMillion: &price, OutputPricePerMillion: &price}},
	})
	store, err := database.Open(t.TempDir()+"/tokemon.db", modelCatalog)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server, err := newTestServer(store, "")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	events := []usage.Event{
		{SchemaVersion: usage.SchemaVersion, EventID: "filter-sonnet-1", Timestamp: now, MachineID: "machine", Provider: "anthropic", Model: "claude-sonnet-4", Tool: "tool", TotalTokens: usage.Int64(700), TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "test", AdapterVersion: "1"}},
		{SchemaVersion: usage.SchemaVersion, EventID: "filter-sonnet-2", Timestamp: now, MachineID: "machine", Provider: "anthropic", Model: "claude-sonnet-4", Tool: "tool", TotalTokens: usage.Int64(300), TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "test", AdapterVersion: "1"}},
		{SchemaVersion: usage.SchemaVersion, EventID: "filter-gpt", Timestamp: now, MachineID: "machine", Provider: "openai", Model: "gpt-5", Tool: "tool", TotalTokens: usage.Int64(300), TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "test", AdapterVersion: "1"}},
		{SchemaVersion: usage.SchemaVersion, EventID: "filter-mystery", Timestamp: now, MachineID: "machine", Provider: "openai", Model: "mystery-model", Tool: "tool", TotalTokens: usage.Int64(5), TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "test", AdapterVersion: "1"}},
	}
	if result, err := store.Ingest(context.Background(), events); err != nil || result.Accepted != len(events) {
		t.Fatalf("ingest = %+v, err %v", result, err)
	}

	page := httptest.NewRecorder()
	testHandler(server).ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/tokedex", nil))
	if page.Code != http.StatusOK {
		t.Fatalf("tokedex status = %d", page.Code)
	}
	body := page.Body.String()
	for _, want := range []string{
		// The filter panel reuses the analytics filter-form styling.
		`class="panel filter-panel" aria-label="Catalog filters"`,
		`class="filter-form" method="get" action="/tokedex" role="search"`,
		`data-tokedex-filters`,
		// Every control has an explicit label association.
		`<label for="tokedex-search">Search<input id="tokedex-search" name="q" type="search"`,
		`<label for="tokedex-sort">Sort<select id="tokedex-sort" name="sort">`,
		`<label for="tokedex-tier">Tier<select id="tokedex-tier" name="tier">`,
		`<label for="tokedex-provider">Provider<select id="tokedex-provider" name="provider">`,
		`<label for="tokedex-pricing">Pricing<select id="tokedex-pricing" name="pricing">`,
		// Sort by tokens, events, and name.
		`<option value="catalog">Catalog order</option>`,
		`<option value="tokens">Tokens (high to low)</option>`,
		`<option value="events">Events (high to low)</option>`,
		`<option value="name">Name (A–Z)</option>`,
		// Filter by tier (including the uncataloged section), provider, and pricing.
		`<option value="S">S</option>`,
		`<option value="A">A</option>`,
		`<option value="B">B</option>`,
		`<option value="Uncataloged">Uncataloged</option>`,
		`<option value="anthropic">anthropic</option>`,
		`<option value="openai">openai</option>`,
		`<option value="priced">Priced</option>`,
		`<option value="unknown">Unknown</option>`,
		// Reset works as a plain link without JS and is intercepted with it.
		`<a href="/tokedex" data-filter-reset>Reset</a>`,
		// A polite result count, plus the no-JS explanation.
		`role="status" data-filter-status>Showing all 4 models.`,
		`<noscript><p class="filter-note">Sorting, filtering, and search need JavaScript.`,
		// Card metadata: display name + aliases for search, tier/provider/
		// pricing for filters, tokens/events for sorting.
		`data-model-card data-name="Claude Sonnet 4" data-search="Claude Sonnet 4 claude-sonnet-4 sonnet" data-tier="S" data-provider="anthropic" data-pricing="priced" data-tokens="1000" data-events="2"`,
		`data-model-card data-name="GPT-5" data-search="GPT-5 gpt-5" data-tier="A" data-provider="openai" data-pricing="unknown" data-tokens="300" data-events="1"`,
		`data-model-card data-name="Zero Usage" data-search="Zero Usage zero-usage" data-tier="B" data-provider="openai" data-pricing="priced" data-tokens="0" data-events="0"`,
		`data-model-card data-name="mystery-model" data-search="mystery-model" data-tier="Uncataloged" data-provider="" data-pricing="unknown" data-tokens="5" data-events="1"`,
		// Section hooks let the script hide empty tiers and reorder cards.
		`aria-labelledby="tier-S" data-filter-section`,
		`data-tier-count data-total="1"`,
		`data-card-grid`,
		`data-filter-empty hidden>No models match these filters.`,
		// Client-side behavior: live search, change filters, per-tier sort.
		`search.addEventListener('input', apply)`,
		`control.addEventListener('change', apply)`,
		`card.hidden = !match`,
		`sectionCards.sort(compare).forEach((card) => grid.append(card))`,
		`a.dataset.name.localeCompare(b.dataset.name)`,
		// Mobile contract: the filter form collapses with the rest of the page.
		`@media (max-width:620px)`,
		`.filter-form { grid-template-columns:repeat(2,minmax(0,1fr)); }`,
		`@media (max-width:420px) { .filter-form { grid-template-columns:1fr; }`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("tokedex filter page missing %q", want)
		}
	}
	// Progressive enhancement: the whole grid is in the HTML, not built by JS.
	if cards := strings.Count(body, `<article class="model-card" data-model-card`); cards != 4 {
		t.Errorf("server-rendered model cards = %d, want 4", cards)
	}
	if strings.Contains(body, `data-model-card hidden`) {
		t.Error("a model card is hidden in the server-rendered markup")
	}
	// No inline event handlers: the page follows the existing script-block
	// pattern instead of attributes a strict CSP would reject.
	for _, forbidden := range []string{"onclick=", "onchange=", "oninput=", "onsubmit="} {
		if strings.Contains(body, forbidden) {
			t.Errorf("tokedex page uses an inline event handler %q", forbidden)
		}
	}
}

func TestTokedexFilterPanelAbsentWithoutModels(t *testing.T) {
	// Nothing to filter means no filter chrome: the empty-catalog state
	// already explains the absence.
	store, err := database.Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server, err := newTestServer(store, "")
	if err != nil {
		t.Fatal(err)
	}
	page := httptest.NewRecorder()
	testHandler(server).ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/tokedex", nil))
	if page.Code != http.StatusOK {
		t.Fatalf("tokedex status = %d", page.Code)
	}
	body := page.Body.String()
	for _, unwanted := range []string{`class="panel filter-panel"`, `data-tokedex-filters`, `id="tokedex-search"`, `data-filter-status`, `data-filter-empty hidden`} {
		if strings.Contains(body, unwanted) {
			t.Errorf("empty catalog rendered filter chrome %q", unwanted)
		}
	}
}

func TestCatalogProvidersDeduplicatesAndSorts(t *testing.T) {
	tiers := []catalogTier{
		{Name: "S", Models: []database.CatalogEntry{{Provider: "openai"}, {Provider: "anthropic"}}},
		{Name: "A", Models: []database.CatalogEntry{{Provider: "openai"}, {Provider: ""}}},
	}
	got := catalogProviders(tiers)
	want := []string{"anthropic", "openai"}
	if len(got) != len(want) {
		t.Fatalf("catalogProviders = %v, want %v", got, want)
	}
	for index := range want {
		if got[index] != want[index] {
			t.Fatalf("catalogProviders = %v, want %v", got, want)
		}
	}
}

func TestAnalyticsQuotaPanel(t *testing.T) {
	// Without quotas the panel is absent entirely — no empty chrome.
	bare := catalog.MustNew(map[string]catalog.Model{})
	store, err := database.OpenWithLocation(filepath.Join(t.TempDir(), "bare.db"), bare, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	store.Close()
	store, err = database.OpenWithLocation(filepath.Join(t.TempDir(), "tokemon.db"), bare, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server, err := newTestServer(store, "")
	if err != nil {
		t.Fatal(err)
	}
	page := httptest.NewRecorder()
	testHandler(server).ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/analytics", nil))
	if page.Code != http.StatusOK {
		t.Fatalf("analytics status = %d", page.Code)
	}
	if strings.Contains(page.Body.String(), "Config-defined budgets") {
		t.Fatal("quota panel rendered without quotas configured")
	}

	// With quotas the panel renders the bar, figures, projection detail and
	// the reset date; over-budget rows use the over styling.
	tokensBudget := int64(50_000_000)
	quotas := []quota.Quota{{Provider: "openai", Label: "OpenAI cap", ResetDay: 1, Budget: quota.Budget{TokensPerMonth: &tokensBudget}}}
	quotaStore, err := database.OpenWithLocation(filepath.Join(t.TempDir(), "quota.db"), catalog.Empty(), time.UTC, quotas...)
	if err != nil {
		t.Fatal(err)
	}
	defer quotaStore.Close()
	quotaServer, err := newTestServer(quotaStore, "")
	if err != nil {
		t.Fatal(err)
	}
	start, end := quota.PeriodFor(time.Now(), 1, time.UTC)
	event := usage.Event{
		SchemaVersion: usage.SchemaVersion, EventID: "quota-panel", Timestamp: start.Add(time.Hour),
		MachineID: "machine", SessionID: "session-quota", Provider: "openai", Model: "gpt-6.1-sol",
		Tool: "codex", InputTokens: usage.Int64(30_000_000), OutputTokens: usage.Int64(30_000_000),
		TotalTokens: usage.Int64(60_000_000), TokenAccuracy: usage.AccuracyReported,
		Source: usage.Source{Adapter: "codex", AdapterVersion: "test"},
	}
	if result, err := quotaStore.Ingest(context.Background(), []usage.Event{event}); err != nil || result.Accepted != 1 {
		t.Fatalf("ingest = %+v, err %v", result, err)
	}

	page = httptest.NewRecorder()
	testHandler(quotaServer).ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/analytics", nil))
	if page.Code != http.StatusOK {
		t.Fatalf("analytics with quotas status = %d", page.Code)
	}
	body := page.Body.String()
	for _, want := range []string{
		`id="quota-title">Quota`,
		"OpenAI cap",
		"60,000,000 tokens / 50,000,000 tokens",
		`quota-fill over`,
		"Pace projects around",
		"on this pace the budget is crossed around",
		"· resets " + end.Format("2006-01-02"),
		"120 percent of the quota used",
	} {
		if !strings.Contains(body, want) {
			t.Errorf("quota panel missing %q", want)
		}
	}

	// The JSON endpoint exposes the same statuses for scripting.
	api := httptest.NewRecorder()
	testHandler(quotaServer).ServeHTTP(api, httptest.NewRequest(http.MethodGet, "/api/v1/quotas", nil))
	if api.Code != http.StatusOK || api.Header().Get("Content-Type") != "application/json; charset=utf-8" {
		t.Fatalf("quotas api status = %d", api.Code)
	}
	var statuses []database.QuotaStatus
	if err := json.Unmarshal(api.Body.Bytes(), &statuses); err != nil {
		t.Fatal(err)
	}
	if len(statuses) != 1 || statuses[0].State != "over" || statuses[0].TokensUsed != 60_000_000 {
		t.Fatalf("quotas api = %+v", statuses)
	}
}

func TestCopyIDAffordanceAndTrendChartRegion(t *testing.T) {
	price := 1.0
	modelCatalog := catalog.MustNew(map[string]catalog.Model{
		"test-model": {Provider: "provider", DisplayName: "Test Model", Aliases: []string{"raw-model"}, Tier: "S", Pricing: catalog.Pricing{Currency: "USD", Mode: "standard", VerifiedAt: "2026-07-12", Source: "https://example.com", InputPricePerMillion: &price, OutputPricePerMillion: &price}},
	})
	store, err := database.Open(t.TempDir()+"/tokemon.db", modelCatalog)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server, err := newTestServer(store, "")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	events := []usage.Event{
		{SchemaVersion: usage.SchemaVersion, EventID: "copy-id-cataloged", Timestamp: now, MachineID: "machine", SessionID: "sess_0123456789abcdef", Provider: "provider", Model: "raw-model", Tool: "codex", TotalTokens: usage.Int64(10), TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "codex", AdapterVersion: "test"}},
		{SchemaVersion: usage.SchemaVersion, EventID: "copy-id-uncataloged", Timestamp: now, MachineID: "machine", SessionID: "sess_0123456789abcdef", Provider: "provider", Model: "mystery-model", Tool: "codex", TotalTokens: usage.Int64(5), TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "codex", AdapterVersion: "test"}},
	}
	if result, err := store.Ingest(context.Background(), events); err != nil || result.Accepted != 2 {
		t.Fatalf("ingest = %+v, err %v", result, err)
	}

	pages := []struct {
		path string
		want []string
	}{
		{"/", []string{
			`class="copy-id" data-copy-value="sess_0123456789abcdef"`,
			`aria-label="Copy full session ID"`,
			`closest('.copy-id')`,
		}},
		{"/sessions", []string{
			`class="copy-id" data-copy-value="sess_0123456789abcdef"`,
			`aria-label="Copy full session ID"`,
			`closest('.copy-id')`,
		}},
		{"/analytics?period=30d", []string{
			`class="trend-chart period-30d" tabindex="0" role="region" aria-label="Token volume over the selected window"`,
		}},
		{"/tokedex", []string{
			// Cataloged tier: the full-ID row carries the copy affordance.
			`class="model-id-text">test-model</span>`,
			`data-copy-value="test-model"`,
			`aria-label="Copy full model ID"`,
			// Uncataloged section: copy affordance plus the catalog how-to link.
			`class="model-name model-name-row" title="mystery-model"`,
			`data-copy-value="mystery-model"`,
			`href="https://github.com/carteakey/tokemon/blob/main/docs/tokemon-spec-v0.2.md#16-model-catalog-and-tokedex"`,
			`how to add a model to catalog/models.yaml`,
			`closest('.copy-id')`,
		}},
	}
	for _, page := range pages {
		response := httptest.NewRecorder()
		testHandler(server).ServeHTTP(response, httptest.NewRequest(http.MethodGet, page.path, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("%s status = %d: %s", page.path, response.Code, response.Body.String())
		}
		for _, want := range page.want {
			if !bytes.Contains(response.Body.Bytes(), []byte(want)) {
				t.Errorf("%s missing %q", page.path, want)
			}
		}
	}
}

func TestAnalyticsInsightsComputations(t *testing.T) {
	now := time.Date(2026, 11, 15, 12, 0, 0, 0, time.UTC)
	previousRate := 0.25

	full := database.Analytics{
		Bucket: "day",
		Filter: database.AnalyticsQuery{Period: "30d", Dimension: "models"},
		Summary: database.AnalyticsSummary{
			Tokens:        1000,
			EstimatedCost: database.CostSummary{Amount: 30, PricedTokens: 800, UnpricedTokens: 200},
			Cache:         database.CacheSummary{HitRate: 0.5, CachedTokens: 250, EligibleTokens: 500},
		},
		Comparison: &database.AnalyticsComparison{PreviousCacheHitRate: &previousRate},
		Points:     make([]database.AnalyticsPoint, 15),
		ShareSeries: []database.AnalyticsShareSeries{
			{Name: "model-a"},
			{Name: "model-b"},
			{Name: "Other"},
		},
		SharePoints: []database.AnalyticsSharePoint{
			{Values: []database.AnalyticsShareSeries{{Name: "model-a", Share: 0.2}, {Name: "model-b", Share: 0.6}, {Name: "Other", Share: 0.2}}},
			{Values: []database.AnalyticsShareSeries{{Name: "model-a", Share: 0.2}, {Name: "model-b", Share: 0.6}, {Name: "Other", Share: 0.2}}},
			{Values: []database.AnalyticsShareSeries{{Name: "model-a", Share: 0.5}, {Name: "model-b", Share: 0.4}, {Name: "Other", Share: 0.1}}},
			{Values: []database.AnalyticsShareSeries{{Name: "model-a", Share: 0.5}, {Name: "model-b", Share: 0.4}, {Name: "Other", Share: 0.1}}},
		},
	}
	insights := analyticsInsights(full, now, nil, nil)
	byID := make(map[string]analyticsInsight, len(insights))
	for _, insight := range insights {
		byID[insight.ID] = insight
	}

	// Burn rate: $30 across 15 day buckets, November has 30 days.
	burn, ok := byID["burn_rate"]
	if !ok {
		t.Fatalf("burn rate insight missing: %+v", insights)
	}
	if burn.Value != "$2.00/day" {
		t.Errorf("burn rate value = %q, want $2.00/day", burn.Value)
	}
	if !strings.Contains(burn.Detail, "$60.00") || !strings.Contains(burn.Detail, "November") {
		t.Errorf("burn rate detail = %q, want a $60.00 November projection", burn.Detail)
	}

	// Top mover: model-a's share doubles between halves (+30 pts), model-b
	// drops (−20 pts); "Other" must be excluded from consideration.
	mover, ok := byID["top_mover"]
	if !ok {
		t.Fatalf("top mover insight missing: %+v", insights)
	}
	if mover.Value != "+30.0 pts" || mover.State != "up" || !strings.Contains(mover.Detail, "model-a") {
		t.Errorf("top mover = %+v, want model-a +30.0 pts up", mover)
	}

	// Cache efficiency: 50% now vs 25% previous → up 25 pts.
	cache, ok := byID["cache_efficiency"]
	if !ok {
		t.Fatalf("cache efficiency insight missing: %+v", insights)
	}
	if cache.Value != "50.0%" || cache.State != "up" || !strings.Contains(cache.Detail, "+25.0 pts") {
		t.Errorf("cache efficiency = %+v, want 50.0%% up with +25.0 pts", cache)
	}

	// Catalog coverage: 200 unpriced of 1000 total.
	coverage, ok := byID["unpriced_share"]
	if !ok {
		t.Fatalf("catalog coverage insight missing: %+v", insights)
	}
	if coverage.Value != "20.0%" || !strings.Contains(coverage.Detail, "200") {
		t.Errorf("catalog coverage = %+v, want 20.0%% with 200 unpriced", coverage)
	}

	// A fully priced window omits the coverage card instead of showing 0.0%%.
	pricedOnly := full
	pricedOnly.Summary.EstimatedCost.UnpricedTokens = 0
	for _, insight := range analyticsInsights(pricedOnly, now, nil, nil) {
		if insight.ID == "unpriced_share" {
			t.Errorf("fully priced window produced a coverage card: %+v", insight)
		}
	}

	// Missing inputs render with the Unknown vocabulary: no priced usage and
	// no cacheable input both stay Unknown; an hour-bucketed window has no
	// burn rate at all.
	unknown := database.Analytics{
		Bucket: "day",
		Filter: database.AnalyticsQuery{Period: "30d"},
		Summary: database.AnalyticsSummary{
			Tokens:        40,
			EstimatedCost: database.CostSummary{UnpricedTokens: 40},
		},
		Points: []database.AnalyticsPoint{{Tokens: 40}},
	}
	insights = analyticsInsights(unknown, now, nil, nil)
	byID = make(map[string]analyticsInsight, len(insights))
	for _, insight := range insights {
		byID[insight.ID] = insight
	}
	if burn, ok := byID["burn_rate"]; !ok || burn.Value != "Unknown" || !strings.Contains(burn.Detail, "No priced usage") {
		t.Errorf("unpriced burn rate = %+v, want Unknown with the no-priced-usage line", burn)
	}
	if cache, ok := byID["cache_efficiency"]; !ok || cache.Value != "Unknown" {
		t.Errorf("no-cacheable-input cache insight = %+v, want Unknown", cache)
	}
	if mover, ok := byID["top_mover"]; ok {
		t.Errorf("single-point window produced a mover card: %+v", mover)
	}
	if coverage, ok := byID["unpriced_share"]; !ok || coverage.Value != "100.0%" {
		t.Errorf("fully unpriced coverage = %+v, want 100.0%%", coverage)
	}

	// Hour buckets (24h view) have no honest month projection.
	hourly := database.Analytics{Bucket: "hour", Filter: database.AnalyticsQuery{Period: "24h"}}
	for _, insight := range analyticsInsights(hourly, now, nil, nil) {
		if insight.ID == "burn_rate" {
			t.Errorf("hour bucket produced a burn-rate card: %+v", insight)
		}
	}
}

func TestAnalyticsInsightsStripRender(t *testing.T) {
	store, err := database.Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server, err := newTestServer(store, "")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	var events []usage.Event
	// Current window: model-a early in the week, model-b late, both with
	// cache tokens and cost so every insight slot has real data.
	seed := func(dayOffset int, model string, eventID string) {
		events = append(events, usage.Event{
			SchemaVersion: usage.SchemaVersion, EventID: eventID, Timestamp: now.AddDate(0, 0, -dayOffset),
			MachineID: "machine-one", SessionID: "session-" + eventID, Provider: "anthropic",
			Model: model, Tool: "codex", InputTokens: usage.Int64(100), CacheReadTokens: usage.Int64(modelDayOffsetCache(dayOffset)),
			TotalTokens: usage.Int64(150), Cost: usage.Float64(2.0), TokenAccuracy: usage.AccuracyReported,
			Source: usage.Source{Adapter: "codex", AdapterVersion: "test"},
		})
	}
	seed(1, "model-a", "insight-a1")
	seed(2, "model-a", "insight-a2")
	seed(5, "model-b", "insight-b1")
	seed(6, "model-b", "insight-b2")
	// The last event carries no cost, so the window has unpriced tokens and
	// the catalog-coverage card has something to report.
	for index := range events {
		if events[index].EventID == "insight-b2" {
			events[index].Cost = nil
		}
	}
	// Previous window: one fully cached event gives a 0.5 hit-rate baseline.
	events = append(events, usage.Event{
		SchemaVersion: usage.SchemaVersion, EventID: "insight-prev", Timestamp: now.AddDate(0, 0, -10),
		MachineID: "machine-one", SessionID: "session-prev", Provider: "anthropic",
		Model: "model-a", Tool: "codex", InputTokens: usage.Int64(100), CacheReadTokens: usage.Int64(100),
		TotalTokens: usage.Int64(200), Cost: usage.Float64(1.0), TokenAccuracy: usage.AccuracyReported,
		Source: usage.Source{Adapter: "codex", AdapterVersion: "test"},
	})
	if result, err := store.Ingest(context.Background(), events); err != nil || result.Accepted != len(events) {
		t.Fatalf("ingest = %+v, err %v", result, err)
	}

	page := httptest.NewRecorder()
	testHandler(server).ServeHTTP(page, httptest.NewRequest(http.MethodGet, "/analytics?period=7d", nil))
	if page.Code != http.StatusOK {
		t.Fatalf("analytics page status = %d: %s", page.Code, page.Body.String())
	}
	for _, want := range []string{
		`aria-label="Usage insights"`,
		"Spend burn rate",
		"/day",
		"Top mover",
		" pts",
		"Cache efficiency",
		"Catalog coverage",
		"unpriced",
	} {
		if !bytes.Contains(page.Body.Bytes(), []byte(want)) {
			t.Errorf("analytics page insights strip missing %q", want)
		}
	}
	// The previous window's cache rate is lower than the current one, so the
	// efficiency card must carry the falling direction with a visible arrow
	// and a screen-reader word.
	for _, want := range []string{`comparison down`, "▼", "falling"} {
		if !bytes.Contains(page.Body.Bytes(), []byte(want)) {
			t.Errorf("analytics page insights strip missing %q", want)
		}
	}

	// The comparison rides along in the JSON API for the same window.
	export := httptest.NewRecorder()
	testHandler(server).ServeHTTP(export, httptest.NewRequest(http.MethodGet, "/api/v1/analytics/export?period=7d&dimension=models", nil))
	if export.Code != http.StatusOK {
		t.Fatalf("analytics export status = %d", export.Code)
	}
	var exported database.Analytics
	if err := json.Unmarshal(export.Body.Bytes(), &exported); err != nil {
		t.Fatalf("decode analytics export: %v", err)
	}
	if exported.Comparison == nil || exported.Comparison.PreviousCacheHitRate == nil {
		t.Fatalf("comparison missing previous cache rate: %+v", exported.Comparison)
	}
	if *exported.Comparison.PreviousCacheHitRate < 0.499 || *exported.Comparison.PreviousCacheHitRate > 0.501 {
		t.Errorf("previous cache hit rate = %v, want 0.5", *exported.Comparison.PreviousCacheHitRate)
	}
}

// modelDayOffsetCache gives early-week events a 50% cache rate and late-week
// events ~17%, so the window halves move the model mix and the cache rate.
func modelDayOffsetCache(dayOffset int) int64 {
	if dayOffset <= 2 {
		return 50
	}
	return 20
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
	server, err := newTestServer(store, "")
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
	testHandler(server).ServeHTTP(pageResponse, httptest.NewRequest(http.MethodGet, "/insights?period=7d&dimension=models&machine=insights-machine", nil))
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
	testHandler(server).ServeHTTP(apiResponse, httptest.NewRequest(http.MethodGet, "/api/v1/insights?period=7d&dimension=models&machine=insights-machine", nil))
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
	testHandler(server).ServeHTTP(defaultResponse, httptest.NewRequest(http.MethodGet, "/api/v1/insights", nil))
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
	server, err := NewWithConfig(store, Config{DashboardToken: testDashboardToken, AllowLoopbackDev: true}, insightsRecapStub{text: "A generated aggregate recap."})
	if err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	testHandler(server).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/insights", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("empty insights status = %d, want %d", response.Code, http.StatusOK)
	}
	for _, want := range []string{"No strong signals in this window.", "No token metadata matches these filters yet.", "Generated recap", "A generated aggregate recap."} {
		if !strings.Contains(response.Body.String(), want) {
			t.Fatalf("empty insights does not contain %q: %s", want, response.Body.String())
		}
	}
	allWindow := httptest.NewRecorder()
	testHandler(server).ServeHTTP(allWindow, httptest.NewRequest(http.MethodGet, "/insights?period=all", nil))
	if allWindow.Code != http.StatusOK || !strings.Contains(allWindow.Body.String(), "No comparable baseline for the all-time window") {
		t.Fatalf("all-time baseline state = %d %s", allWindow.Code, allWindow.Body.String())
	}

	store2, err := database.Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store2.Close()
	unavailable, err := NewWithConfig(store2, Config{DashboardToken: testDashboardToken, AllowLoopbackDev: true}, insightsRecapStub{err: fmt.Errorf("provider offline")})
	if err != nil {
		t.Fatal(err)
	}
	response = httptest.NewRecorder()
	testHandler(unavailable).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/insights", nil))
	if response.Code != http.StatusOK || !strings.Contains(response.Body.String(), "Recap temporarily unavailable") {
		t.Fatalf("unavailable recap page = %d %s", response.Code, response.Body.String())
	}
}

func TestInsightsRenderHidesDatabaseErrorDetails(t *testing.T) {
	store, err := database.Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	server, err := newTestServer(store, "")
	if err != nil {
		store.Close()
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	response := httptest.NewRecorder()
	testHandler(server).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/insights", nil))
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

func TestInsightsRoutesRequireDashboardAuth(t *testing.T) {
	store, err := database.Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server, err := newTestServer(store, "ingest-token")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/insights", "/api/v1/insights"} {
		anonymous := httptest.NewRecorder()
		server.Handler().ServeHTTP(anonymous, httptest.NewRequest(http.MethodGet, path, nil))
		if anonymous.Code != http.StatusUnauthorized {
			t.Fatalf("anonymous %s status = %d, want %d", path, anonymous.Code, http.StatusUnauthorized)
		}
		authed := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.SetBasicAuth("tokemon", testDashboardToken)
		server.Handler().ServeHTTP(authed, request)
		if authed.Code != http.StatusOK {
			t.Fatalf("authenticated %s status = %d, want %d: %s", path, authed.Code, http.StatusOK, authed.Body.String())
		}
	}
}

func TestDisplayFormattersReadNaturally(t *testing.T) {
	for raw, want := range map[string]string{
		"claude-opus-5-5":            "Opus 5.5",
		"claude-sonnet-4-5-20250929": "Sonnet 4.5",
		"claude-3-5-sonnet":          "3.5 Sonnet",
		"claude-opus-4":              "Opus 4",
		"gpt-6.1-sol":                "GPT-6.1 Sol",
	} {
		if got := compactModelName(raw); got != want {
			t.Errorf("compactModelName(%q) = %q, want %q", raw, got, want)
		}
	}
	for raw, want := range map[string]string{"macbook-air-2": "MacBook Air 2", "Kartikeys-MacBook-Air": "MacBook Air", "fx505": "fx505"} {
		if got := compactMachineName(raw); got != want {
			t.Errorf("compactMachineName(%q) = %q, want %q", raw, got, want)
		}
	}
	for value, want := range map[float64]string{0: "$0.00", 10: "$10.00", 9295.774: "$9,295.77", 1234567.891: "$1,234,567.89", -12.5: "-$12.50"} {
		if got := money(value); got != want {
			t.Errorf("money(%v) = %q, want %q", value, got, want)
		}
	}
	for ms, want := range map[int64]string{850: "850ms", 42_000: "42.0s", 1_292_500: "21m 33s", 3_600_000: "1h 00m", 5_025_000: "1h 23m"} {
		if got := durationLabel(ms); got != want {
			t.Errorf("durationLabel(%d) = %q, want %q", ms, got, want)
		}
	}
}

// Ported from the production-hardening line: the ingest endpoint must accept
// exactly one JSON value so a trailing suffix cannot bypass request limits.

func TestModelDisplayNameLabelsUnknownModel(t *testing.T) {
	if got := modelDisplayName(nil, "unknown"); got != "Unknown model" {
		t.Errorf("modelDisplayName(nil, %q) = %q, want %q", "unknown", got, "Unknown model")
	}
	if got := modelDisplayName(nil, "UNKNOWN"); got != "Unknown model" {
		t.Errorf("modelDisplayName(nil, %q) = %q, want %q", "UNKNOWN", got, "Unknown model")
	}
	aliases := map[string]string{"unknown": "Mystery"}
	if got := modelDisplayName(aliases, "unknown"); got != "Mystery" {
		t.Errorf("explicit alias must win: got %q", got)
	}
	if got := modelDisplayName(nil, "claude-sonnet-4-5"); got != "Sonnet 4.5" {
		t.Errorf("regular models unchanged: got %q", got)
	}
}

func TestSessionDisplayNameCoversProviderIDShapes(t *testing.T) {
	cases := []struct{ raw, want string }{
		{"sess_2692e21a-a705-4739-9736-9944066c00c1", "session 2692e21a"},
		{"sess_subagent_agent_fd6451fa-45b6-4a23-8ecd-4c0933fdc1d3", "subagent fd6451fa"},
		// ses_ (three s) is a distinct production shape; 687 live events use it.
		{"ses_0adfb3061ffeAZCKl4dOsKlk0B", "session 0adfb306"},
		{"session_9b0ae0790f7b4e81a7bc6b5e74ba0bf6", "session 9b0ae079"},
		{"local-17b847a5f18631cb112a0d7251603401ff0e70466ea0d6d5e441d88992fd2a3f", "session 17b847a5"},
		{"sha256:5f1c3b9a88f700112233445566778899aabbccddeeff00112233445566778899", "session 5f1c3b9a"},
		{"20261027_192814_9fa9fb3d", "2026-10-27 19:28"},
		{"plain-name", "plain-name"},
		{"a-very-long-unstructured-session-identifier-value", "a-very-long-unstru…"},
		{"", ""},
	}
	for _, tc := range cases {
		if got := sessionDisplayName(tc.raw); got != tc.want {
			t.Errorf("sessionDisplayName(%q) = %q, want %q", tc.raw, got, tc.want)
		}
	}
}

func TestIngestRejectsTrailingJSON(t *testing.T) {
	store, err := database.Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server, err := newTestServer(store, "secret")
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
	request.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("trailing JSON status = %d, want %d: %s", response.Code, http.StatusBadRequest, response.Body.String())
	}
}

// Spreadsheet applications execute cells that begin with a formula character;
// every text column of the CSV exports must be neutralized while numeric and
// boolean cells stay raw.
func TestCSVExportNeutralizesFormulaInjection(t *testing.T) {
	store, err := database.Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server, err := newTestServer(store, "")
	if err != nil {
		t.Fatal(err)
	}
	event := usage.Event{
		SchemaVersion: usage.SchemaVersion, EventID: "csv-injection", Timestamp: time.Now().UTC(),
		MachineID: "=cmd|'/c-calc'", SessionID: "+1313544", Project: "-etc-passwd", Provider: "@provider",
		Model: "model", Tool: "codex", TotalTokens: usage.Int64(10), TokenAccuracy: usage.AccuracyReported,
		Source: usage.Source{Adapter: "codex", AdapterVersion: "test"},
	}
	if result, err := store.Ingest(context.Background(), []usage.Event{event}); err != nil || result.Accepted != 1 {
		t.Fatalf("ingest = %+v, err %v", result, err)
	}

	sessions := httptest.NewRecorder()
	testHandler(server).ServeHTTP(sessions, httptest.NewRequest(http.MethodGet, "/api/v1/sessions/export?period=30d&format=csv", nil))
	if sessions.Code != http.StatusOK {
		t.Fatalf("sessions csv status = %d", sessions.Code)
	}
	body := sessions.Body.String()
	for _, want := range []string{
		`'+1313544`,
		`'=cmd|'/c-calc'`,
		`'-etc-passwd`,
		`'@provider`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("sessions csv missing neutralized cell %q", want)
		}
	}
	if strings.Contains(body, "\n=cmd") || strings.Contains(body, "\n+1313544") {
		t.Errorf("sessions csv contains an unneutralized formula cell:\n%s", body)
	}

	analytics := httptest.NewRecorder()
	testHandler(server).ServeHTTP(analytics, httptest.NewRequest(http.MethodGet, "/api/v1/analytics/export?dimension=projects&period=30d&format=csv", nil))
	if analytics.Code != http.StatusOK {
		t.Fatalf("analytics csv status = %d", analytics.Code)
	}
	if body := analytics.Body.String(); !strings.Contains(body, "'-etc-passwd") {
		t.Errorf("analytics csv missing neutralized project cell:\n%s", body)
	}
}
