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

func TestRunAgentDoesNotCommitRejectedEvents(t *testing.T) {
	home := t.TempDir()
	transcriptDir := filepath.Join(home, ".claude", "projects", "project")
	if err := os.MkdirAll(transcriptDir, 0o755); err != nil {
		t.Fatal(err)
	}
	transcript := `{"type":"assistant","timestamp":"2026-07-13T12:00:00Z","sessionId":"session-1","message":{"model":"claude-sonnet","usage":{"input_tokens":10,"output_tokens":20}}}` + "\n"
	if err := os.WriteFile(filepath.Join(transcriptDir, "session.jsonl"), []byte(transcript), 0o600); err != nil {
		t.Fatal(err)
	}

	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/healthz":
			w.WriteHeader(http.StatusNoContent)
		case "/api/v1/agents/heartbeat":
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"status":"ok"}`))
		case "/api/v1/events/batch":
			calls++
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"rejected":1,"errors":["event 1: invalid"]}`))
		default:
			t.Fatalf("unexpected agent request path: %q", r.URL.Path)
		}
	}))
	defer server.Close()
	statePath := filepath.Join(t.TempDir(), "agent-state.db")
	args := []string{"agent", "--server", server.URL, "--home", home, "--machine-id", "machine", "--state", statePath, "--once", "--timeout", "5s"}
	for attempt := 0; attempt < 2; attempt++ {
		err := run(args)
		if err == nil || !strings.Contains(err.Error(), "hub rejected 1") {
			t.Fatalf("attempt %d error = %v, want rejected-event error", attempt+1, err)
		}
	}
	if calls != 2 {
		t.Fatalf("rejected event upload calls = %d, want retry on the next run", calls)
	}
}

func TestRunServeFailsClosedWithoutIngestToken(t *testing.T) {
	t.Setenv("TOKEMON_INGEST_TOKEN", "")
	config := filepath.Join(t.TempDir(), "server.env")
	err := run([]string{"serve", "--addr", "127.0.0.1:0", "--config", config, "--database", filepath.Join(t.TempDir(), "tokemon.db")})
	if err == nil || !strings.Contains(err.Error(), "TOKEMON_INGEST_TOKEN is required") {
		t.Fatalf("serve without a token = %v, want a token requirement error", err)
	}
}

func TestConfiguredInsightsValuesPreferEnvironment(t *testing.T) {
	t.Setenv("TOKEMON_INSIGHTS_OPENAI_MODEL", "env-model")
	t.Setenv("TOKEMON_INSIGHTS_OPENAI_API_KEY", "env-secret")
	values := map[string]string{
		"TOKEMON_INSIGHTS_OPENAI_MODEL":   "file-model",
		"TOKEMON_INSIGHTS_OPENAI_API_KEY": "file-secret",
	}
	if got := configuredValue(values, "TOKEMON_INSIGHTS_OPENAI_MODEL"); got != "env-model" {
		t.Fatalf("configured model = %q, want env-model", got)
	}
	if got := configuredSecret(values, "TOKEMON_INSIGHTS_OPENAI_API_KEY"); got != "env-secret" {
		t.Fatalf("configured secret = %q, want env-secret", got)
	}
	t.Setenv("TOKEMON_INSIGHTS_OPENAI_MODEL", "")
	t.Setenv("TOKEMON_INSIGHTS_OPENAI_API_KEY", "")
	if got := configuredValue(values, "TOKEMON_INSIGHTS_OPENAI_MODEL"); got != "file-model" {
		t.Fatalf("file model = %q, want file-model", got)
	}
	if got := configuredSecret(values, "TOKEMON_INSIGHTS_OPENAI_API_KEY"); got != "file-secret" {
		t.Fatalf("file secret = %q, want file-secret", got)
	}
}

func TestRunServeDevModeRequiresLoopback(t *testing.T) {
	t.Setenv("TOKEMON_INGEST_TOKEN", "")
	config := filepath.Join(t.TempDir(), "server.conf")
	err := run([]string{"serve", "--dev", "--addr", "0.0.0.0:0", "--config", config, "--database", filepath.Join(t.TempDir(), "tokemon.db")})
	if err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("dev mode on a non-loopback address = %v, want a loopback error", err)
	}
}

func TestIsLoopbackAddr(t *testing.T) {
	for addr, want := range map[string]bool{
		"127.0.0.1:8080": true,
		"localhost:8080": true,
		"[::1]:8080":     true,
		":8080":          false,
		"0.0.0.0:8080":   false,
		"10.0.0.5:8080":  false,
	} {
		if got := isLoopbackAddr(addr); got != want {
			t.Errorf("isLoopbackAddr(%q) = %v, want %v", addr, got, want)
		}
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
