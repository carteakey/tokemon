package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"log/slog"

	"github.com/tokemon/tokemon/internal/catalog"
	"github.com/tokemon/tokemon/internal/database"
)

func TestHealthReadinessMetricsAndRedactedLogs(t *testing.T) {
	store, err := database.Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var logs bytes.Buffer
	server, err := NewWithConfig(store, Config{
		IngestToken: "secret", DashboardToken: "dashboard", Logger: slog.New(slog.NewJSONHandler(&logs, nil)),
		StaleAgentAfter: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	health := httptest.NewRecorder()
	server.Handler().ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if health.Code != http.StatusOK || !strings.Contains(health.Body.String(), `"status":"ok"`) {
		t.Fatalf("health = %d %s", health.Code, health.Body.String())
	}

	invalid := httptest.NewRequest(http.MethodPost, "/api/v1/events/batch", strings.NewReader(`{"events":[{"schema_version":"1","event_id":"bad","timestamp":"2026-08-10T00:00:00Z","machine_id":"machine","provider":"provider","model":"model","tool":"tool","input_tokens":2,"total_tokens":1,"token_accuracy":"unknown","source":{"adapter":"test","adapter_version":"1"}}]}`))
	invalid.Header.Set("Authorization", "Bearer secret")
	invalidResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(invalidResponse, invalid)
	if invalidResponse.Code != http.StatusBadRequest {
		t.Fatalf("invalid ingest = %d %s", invalidResponse.Code, invalidResponse.Body.String())
	}

	heartbeat := httptest.NewRequest(http.MethodPost, "/api/v1/agents/heartbeat", strings.NewReader(`{"machine_id":"machine","source_count":2,"source_error_count":2}`))
	heartbeat.Header.Set("Authorization", "Bearer secret")
	heartbeatResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(heartbeatResponse, heartbeat)
	if heartbeatResponse.Code != http.StatusOK {
		t.Fatalf("heartbeat = %d %s", heartbeatResponse.Code, heartbeatResponse.Body.String())
	}

	metricsRequest := httptest.NewRequest(http.MethodGet, "/metrics", nil)
	metricsRequest.SetBasicAuth("tokemon", "dashboard")
	metricsResponse := httptest.NewRecorder()
	server.Handler().ServeHTTP(metricsResponse, metricsRequest)
	if metricsResponse.Code != http.StatusOK {
		t.Fatalf("metrics = %d %s", metricsResponse.Code, metricsResponse.Body.String())
	}
	var metrics MetricsSnapshot
	if err := json.Unmarshal(metricsResponse.Body.Bytes(), &metrics); err != nil {
		t.Fatal(err)
	}
	if metrics.RequestsTotal < 4 || metrics.RequestFailures != 0 || metrics.IngestRejectedEvents != 1 || metrics.SourceErrors != 2 || metrics.DBOperations < 2 || metrics.DBLatencyTotalMS == 0 || metrics.DBLatencyMaxMS == 0 {
		t.Fatalf("unexpected metrics: %+v", metrics)
	}
	if strings.Contains(logs.String(), "secret") || strings.Contains(logs.String(), "dashboard") || strings.Contains(logs.String(), "event_id") {
		t.Fatalf("logs contain credentials or payload fields: %s", logs.String())
	}
	for _, line := range strings.Split(strings.TrimSpace(logs.String()), "\n") {
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("log is not JSON: %v (%s)", err, line)
		}
	}

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	closedHealth := httptest.NewRecorder()
	server.Handler().ServeHTTP(closedHealth, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if closedHealth.Code != http.StatusServiceUnavailable {
		t.Fatalf("closed-store health = %d, want %d", closedHealth.Code, http.StatusServiceUnavailable)
	}
}
