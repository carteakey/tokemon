package agent

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/tokemon/tokemon/internal/database"
	"github.com/tokemon/tokemon/internal/usage"
)

func TestClientIngestRedactsMetadataBeforeUpload(t *testing.T) {
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
		for _, event := range request.Events {
			if event.Metadata != nil {
				t.Errorf("uploaded event %q retained metadata", event.EventID)
			}
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
			Metadata:      map[string]any{"test_payload": strings.Repeat("x", 3<<20)},
		}
	}

	result, err := (Client{ServerURL: server.URL}).Ingest(t.Context(), events)
	if err != nil {
		t.Fatal(err)
	}
	if requests != 1 || result.Accepted != len(events) || result.CurrentTotal != 1 {
		t.Fatalf("requests = %d, result = %+v, want one redacted request and all events accepted", requests, result)
	}
}

func TestClientIngestSplitsBatchesBelowCountCap(t *testing.T) {
	requests := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		var request batchRequest
		if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Errorf("decode request: %v", err)
			return
		}
		if len(request.Events) > maxIngestBatchEvents {
			t.Errorf("batch = %d events, want at most %d", len(request.Events), maxIngestBatchEvents)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(database.IngestResult{Accepted: len(request.Events), CurrentTotal: int64(requests)})
	}))
	defer server.Close()

	events := make([]usage.Event, maxIngestBatchEvents*2+1)
	for index := range events {
		events[index] = usage.Event{
			SchemaVersion: usage.SchemaVersion,
			EventID:       "id" + fmt.Sprintf("%d", index),
			MachineID:     "machine-" + string(rune('a'+index/10)),
			Provider:      "provider",
			Model:         "model",
			Tool:          "codex",
		}
	}

	result, err := (Client{ServerURL: server.URL}).Ingest(t.Context(), events)
	if err != nil {
		t.Fatal(err)
	}
	if requests != 3 {
		t.Fatalf("requests = %d, want 3 batches for %d events", requests, len(events))
	}
	if result.Accepted != len(events) {
		t.Fatalf("accepted = %d, want %d", result.Accepted, len(events))
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
