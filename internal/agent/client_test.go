package agent

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tokemon/tokemon/internal/database"
	"github.com/tokemon/tokemon/internal/usage"
)

func TestClientIngestSplitsLargeBatches(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		body, err := io.ReadAll(io.LimitReader(r.Body, maxIngestBatchBytes+1))
		if err != nil {
			t.Errorf("read request: %v", err)
			return
		}
		if len(body) > maxIngestBatchBytes {
			t.Errorf("request body = %d bytes, want at most %d", len(body), maxIngestBatchBytes)
		}
		var request batchRequest
		if err := json.Unmarshal(body, &request); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(database.IngestResult{Accepted: len(request.Events), CurrentTotal: int64(requests)})
	}))
	defer server.Close()

	events := make([]usage.Event, 3)
	for index := range events {
		events[index] = usage.Event{
			SchemaVersion: usage.SchemaVersion,
			EventID:       "large-" + string(rune('a'+index)),
			MachineID:     "machine",
			Provider:      "provider",
			Model:         "model",
			Tool:          "codex",
			Metadata:      map[string]any{"tokemon_test_payload": strings.Repeat("x", 3<<20)},
		}
	}

	result, err := (Client{ServerURL: server.URL}).Ingest(t.Context(), events)
	if err != nil {
		t.Fatal(err)
	}
	if requests != len(events) || result.Accepted != len(events) || result.CurrentTotal != int64(len(events)) {
		t.Fatalf("requests = %d, result = %+v, want %d bounded requests and all events accepted", requests, result, len(events))
	}
}

func TestClientRejectsSensitivePayloadBeforeAnyRequest(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		http.Error(w, "unexpected request", http.StatusInternalServerError)
	}))
	defer server.Close()

	event := usage.Event{
		SchemaVersion: usage.SchemaVersion,
		EventID:       "sensitive-event",
		Timestamp:     time.Now().UTC(),
		MachineID:     "machine",
		Provider:      "provider",
		Model:         "model",
		Tool:          "generic-jsonl",
		TokenAccuracy: usage.AccuracyReported,
		Project:       "/private/repository",
		Metadata:      map[string]any{"tokemon_prompt": "never upload this"},
	}
	if _, err := (Client{ServerURL: server.URL}).Ingest(t.Context(), []usage.Event{event}); err == nil {
		t.Fatal("sensitive event was accepted")
	} else if !strings.Contains(err.Error(), "disallowed sensitive content") {
		t.Fatalf("error = %v, want privacy rejection", err)
	}
	if requests != 0 {
		t.Fatalf("server received %d requests after local privacy rejection", requests)
	}
}

func TestClientHeartbeatSendsDeploymentMetadata(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/v1/agents/heartbeat" {
			t.Fatalf("heartbeat path = %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Fatalf("authorization header = %q", r.Header.Get("Authorization"))
		}
		var request HeartbeatRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Fatalf("decode heartbeat: %v", err)
		}
		if request.MachineID != "machine" || request.AgentVersion != "0.3.0" || len(request.Adapters) != 1 || request.SourceCount != 2 || request.SourceErrorCount != 1 {
			t.Fatalf("heartbeat = %+v", request)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	}))
	defer server.Close()

	err := (Client{ServerURL: server.URL, Token: "secret"}).Heartbeat(t.Context(), HeartbeatRequest{
		MachineID: "machine", AgentVersion: "0.3.0", OperatingSystem: "linux", Architecture: "arm64",
		Adapters: []AdapterHeartbeat{{ID: "codex", Version: "0.6.0"}}, SourceCount: 2, SourceErrorCount: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
}

func TestClientHealthChecksHubBeforeCollection(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet || r.URL.Path != "/healthz" {
			t.Fatalf("health request = %s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer secret" {
			t.Fatalf("authorization header = %q", r.Header.Get("Authorization"))
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	if err := (Client{ServerURL: server.URL, Token: "secret"}).Health(t.Context()); err != nil {
		t.Fatal(err)
	}
}

func TestClientHealthRejectsUnavailableHub(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "database unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	err := (Client{ServerURL: server.URL}).Health(t.Context())
	if err == nil || !strings.Contains(err.Error(), "503 Service Unavailable") {
		t.Fatalf("health error = %v, want unavailable status", err)
	}
}
