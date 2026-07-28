package main

import (
	"flag"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunCatalogValidate(t *testing.T) {
	path := filepath.Join("..", "..", "catalog", "models.yaml")
	if err := run([]string{"catalog", "validate", "--catalog", path}); err != nil {
		t.Fatal(err)
	}
}

func TestRunCatalogValidateRejectsInvalidCatalog(t *testing.T) {
	path := filepath.Join(t.TempDir(), "models.yaml")
	if err := os.WriteFile(path, []byte("schema_version: 1\nmodels: {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	err := run([]string{"catalog", "validate", "--catalog", path})
	if err == nil || !strings.Contains(err.Error(), `schema_version must be "2"`) {
		t.Fatalf("unexpected validation error: %v", err)
	}
}

func TestApplyServerConfigPrecedence(t *testing.T) {
	t.Setenv("TOKEMON_SERVER_ADDR", "")
	t.Setenv("TOKEMON_DATABASE", "")
	t.Setenv("TOKEMON_INGEST_TOKEN", "")
	t.Setenv("TOKEMON_MODEL_CATALOG", "")
	t.Setenv("TOKEMON_ANALYTICS_TIMEZONE", "")

	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	addr := flags.String("addr", ":8080", "")
	databasePath := flags.String("database", "tokemon.db", "")
	ingestToken := flags.String("ingest-token", "", "")
	catalogPath := flags.String("catalog", "catalog/models.yaml", "")
	timezone := flags.String("timezone", "UTC", "")
	if err := flags.Parse([]string{}); err != nil {
		t.Fatal(err)
	}
	applyServerConfig(flags, map[string]string{
		"TOKEMON_SERVER_ADDR":        "0.0.0.0:18080",
		"TOKEMON_DATABASE":           "/tmp/tokemon.db",
		"TOKEMON_INGEST_TOKEN":       "file-token",
		"TOKEMON_MODEL_CATALOG":      "/tmp/models.yaml",
		"TOKEMON_ANALYTICS_TIMEZONE": "America/Toronto",
	}, addr, databasePath, ingestToken, catalogPath, timezone)
	if *addr != "0.0.0.0:18080" || *databasePath != "/tmp/tokemon.db" || *ingestToken != "file-token" || *catalogPath != "/tmp/models.yaml" || *timezone != "America/Toronto" {
		t.Fatalf("config values not applied: addr=%q database=%q token=%q catalog=%q timezone=%q", *addr, *databasePath, *ingestToken, *catalogPath, *timezone)
	}

	if err := flags.Set("addr", "127.0.0.1:9999"); err != nil {
		t.Fatal(err)
	}
	applyServerConfig(flags, map[string]string{"TOKEMON_SERVER_ADDR": "0.0.0.0:18080"}, addr, databasePath, ingestToken, catalogPath, timezone)
	if *addr != "127.0.0.1:9999" {
		t.Fatalf("explicit flag was overwritten: %q", *addr)
	}
}

func TestRunAgentPersistsStateAndRetriesAfterFailedUpload(t *testing.T) {
	home := t.TempDir()
	transcriptDir := filepath.Join(home, ".claude", "projects", "project")
	if err := os.MkdirAll(transcriptDir, 0o755); err != nil {
		t.Fatal(err)
	}
	transcript := `{"type":"assistant","timestamp":"2026-07-13T12:00:00Z","sessionId":"session-1","message":{"model":"claude-sonnet","usage":{"input_tokens":10,"output_tokens":20,"cache_read_input_tokens":30,"cache_creation_input_tokens":40}}}` + "\n"
	if err := os.WriteFile(filepath.Join(transcriptDir, "session.jsonl"), []byte(transcript), 0o600); err != nil {
		t.Fatal(err)
	}

	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/healthz" {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		if r.URL.Path == "/api/v1/agents/heartbeat" {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ok"}`))
			return
		}
		if r.URL.Path != "/api/v1/events/batch" {
			t.Fatalf("unexpected agent request path: %q", r.URL.Path)
		}
		calls++
		if calls == 1 {
			http.Error(w, "temporary failure", http.StatusServiceUnavailable)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"accepted":1,"duplicates":0,"current_lifetime_tokens":100}`))
	}))
	defer server.Close()
	statePath := filepath.Join(t.TempDir(), "agent-state.db")
	args := []string{"agent", "--server", server.URL, "--home", home, "--machine-id", "machine", "--state", statePath, "--once", "--timeout", "5s"}
	if err := run(args); err == nil {
		t.Fatal("first run unexpectedly succeeded")
	}
	if calls != 1 {
		t.Fatalf("first run calls = %d, want 1", calls)
	}
	if err := run(args); err != nil {
		t.Fatalf("retry run failed: %v", err)
	}
	if calls != 2 {
		t.Fatalf("retry run calls = %d, want 2", calls)
	}
	if err := run(args); err != nil {
		t.Fatalf("restart run failed: %v", err)
	}
	if calls != 2 {
		t.Fatalf("unchanged restart uploaded again: calls = %d", calls)
	}
}

func TestRunAgentSkipsCollectionWhenHubIsUnavailable(t *testing.T) {
	healthCalls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/healthz" {
			t.Fatalf("agent continued past failed health check: %s", r.URL.Path)
		}
		healthCalls++
		http.Error(w, "offline", http.StatusServiceUnavailable)
	}))
	defer server.Close()

	home := t.TempDir()
	statePath := filepath.Join(t.TempDir(), "agent-state.db")
	err := run([]string{"agent", "--server", server.URL, "--home", home, "--machine-id", "machine", "--state", statePath, "--once", "--timeout", "5s"})
	if err == nil || !strings.Contains(err.Error(), "hub health check") {
		t.Fatalf("agent error = %v, want hub health failure", err)
	}
	if healthCalls != 1 {
		t.Fatalf("health calls = %d, want 1", healthCalls)
	}
}
