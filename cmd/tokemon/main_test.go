package main

import (
	"context"
	"flag"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"golang.org/x/crypto/bcrypt"
)

func TestRunCatalogValidate(t *testing.T) {
	path := filepath.Join("..", "..", "catalog", "models.yaml")
	if err := run([]string{"catalog", "validate", "--catalog", path}); err != nil {
		t.Fatal(err)
	}
}

func TestServeHTTPGracefulShutdown(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	server := &http.Server{Addr: "127.0.0.1:0", Handler: http.NotFoundHandler()}
	go func() {
		time.Sleep(20 * time.Millisecond)
		cancel()
	}()
	if err := serveHTTP(ctx, server, time.Second); err != nil {
		t.Fatalf("serveHTTP shutdown: %v", err)
	}
}

func TestRunArtValidate(t *testing.T) {
	path := filepath.Join("..", "..", "web", "static", "tokemon")
	if err := run([]string{"art", "validate", "--dir", path}); err != nil {
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

func TestReadEventsRejectsSensitiveFieldsBeforeInspectOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sensitive.jsonl")
	line := `{"schema_version":"1","event_id":"inspect-sensitive","timestamp":"2026-07-12T12:00:00Z","machine_id":"machine","provider":"example","model":"model","tool":"generic-jsonl","token_accuracy":"reported","prompt":"private prompt","total_tokens":1}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readEvents(path); err == nil || !strings.Contains(err.Error(), "disallowed sensitive content") {
		t.Fatalf("readEvents error = %v, want privacy rejection", err)
	}
}

func TestReadEventsRejectsSensitiveNestedSourceFields(t *testing.T) {
	path := filepath.Join(t.TempDir(), "nested-sensitive.jsonl")
	line := `{"schema_version":"1","event_id":"inspect-source-sensitive","timestamp":"2026-07-12T12:00:00Z","machine_id":"machine","provider":"example","model":"model","tool":"generic-jsonl","token_accuracy":"reported","source":{"adapter":"generic-jsonl","path":"/private/repository"}}` + "\n"
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := readEvents(path); err == nil || !strings.Contains(err.Error(), "disallowed sensitive content") {
		t.Fatalf("readEvents error = %v, want nested privacy rejection", err)
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

func TestApplyAgentConfigProjectLabelOptOut(t *testing.T) {
	t.Setenv("TOKEMON_INCLUDE_PROJECTS", "")
	flags := flag.NewFlagSet("agent", flag.ContinueOnError)
	serverURL := flags.String("server", "http://localhost", "")
	token := flags.String("token", "token", "")
	machineID := flags.String("machine-id", "machine", "")
	home := flags.String("home", "/tmp/home", "")
	adapters := flags.String("adapters", "codex", "")
	interval := flags.Duration("interval", time.Minute, "")
	statePath := flags.String("state", "/tmp/state.db", "")
	includeProjects := flags.Bool("include-projects", true, "")
	if err := flags.Parse(nil); err != nil {
		t.Fatal(err)
	}
	applyAgentConfig(flags, map[string]string{"TOKEMON_INCLUDE_PROJECTS": "false"}, serverURL, token, machineID, home, adapters, interval, statePath, includeProjects)
	if *includeProjects {
		t.Fatal("config opt-out did not disable project labels")
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

// Ported from the production-hardening line: a rejected upload must not
// advance agent state, so the next pass retries the same events.
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

// Adapted from the production-hardening line to main's serve security model:
// an empty ingest token fails closed unless --dev-loopback is paired with an
// explicit loopback address (api.ValidateServeConfig coverage lives in
// internal/api/security_test.go).
func TestRunServeFailsClosedWithoutIngestToken(t *testing.T) {
	t.Setenv("TOKEMON_INGEST_TOKEN", "")
	config := filepath.Join(t.TempDir(), "server.env")
	err := run([]string{"serve", "--addr", "127.0.0.1:0", "--config", config, "--database", filepath.Join(t.TempDir(), "tokemon.db")})
	if err == nil || !strings.Contains(err.Error(), "TOKEMON_INGEST_TOKEN is required") {
		t.Fatalf("serve without a token = %v, want a token requirement error", err)
	}
}

func TestRunServeDevLoopbackRequiresLoopbackAddress(t *testing.T) {
	t.Setenv("TOKEMON_INGEST_TOKEN", "")
	config := filepath.Join(t.TempDir(), "server.conf")
	err := run([]string{"serve", "--dev-loopback", "--addr", "0.0.0.0:0", "--config", config, "--database", filepath.Join(t.TempDir(), "tokemon.db")})
	if err == nil || !strings.Contains(err.Error(), "loopback") {
		t.Fatalf("dev-loopback on a non-loopback address = %v, want a loopback error", err)
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

// The passwd subcommand (ported from the pre-merge auth design) must emit a
// hash that bcrypt accepts and that verifies against the source password.
func TestRunPasswdGeneratesVerifiableHash(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.WriteString("hunter2\n"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	originalStdin := os.Stdin
	os.Stdin = reader
	originalStdout := os.Stdout
	readBack, readBackWriter, err := os.Pipe()
	if err != nil {
		os.Stdin = originalStdin
		t.Fatal(err)
	}
	os.Stdout = readBackWriter
	err = run([]string{"passwd", "--stdin"})
	os.Stdin = originalStdin
	if err := readBackWriter.Close(); err != nil {
		t.Fatal(err)
	}
	os.Stdout = originalStdout
	if err != nil {
		t.Fatal(err)
	}
	output, err := io.ReadAll(readBack)
	if err != nil {
		t.Fatal(err)
	}
	hash := strings.TrimSpace(string(output))
	if !strings.HasPrefix(hash, "$2a$") && !strings.HasPrefix(hash, "$2b$") {
		t.Fatalf("passwd output = %q, want a bcrypt hash", hash)
	}
	if _, err := bcrypt.Cost([]byte(hash)); err != nil {
		t.Fatalf("passwd produced an unusable hash: %v", err)
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte("hunter2")) != nil {
		t.Fatal("passwd hash does not verify against the entered password")
	}
	if bcrypt.CompareHashAndPassword([]byte(hash), []byte("other")) == nil {
		t.Fatal("passwd hash verifies against the wrong password")
	}
}

func TestRunPasswdRejectsNonTerminalWithoutStdinFlag(t *testing.T) {
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.WriteString("hunter2\n"); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	originalStdin := os.Stdin
	os.Stdin = reader
	err = run([]string{"passwd"})
	os.Stdin = originalStdin
	if err == nil || !strings.Contains(err.Error(), "--stdin") {
		t.Fatalf("pipe without --stdin = %v, want a --stdin hint", err)
	}
}

func TestApplyServerSecurityConfigPasswordPairing(t *testing.T) {
	t.Setenv("TOKEMON_DASHBOARD_USERNAME", "")
	t.Setenv("TOKEMON_DASHBOARD_PASSWORD_HASH", "")
	t.Setenv("TOKEMON_DASHBOARD_TOKEN", "")
	t.Setenv("TOKEMON_DEV_LOOPBACK", "")
	t.Setenv("TOKEMON_TRUSTED_PROXY_CIDRS", "")

	flags := flag.NewFlagSet("serve", flag.ContinueOnError)
	dashboardToken := flags.String("dashboard-token", "", "")
	devLoopback := flags.Bool("dev-loopback", false, "")
	trustedProxyCIDRs := flags.String("trusted-proxy-cidrs", "", "")
	dashboardUsername := flags.String("dashboard-username", "", "")
	dashboardPasswordHash := flags.String("dashboard-password-hash", "", "")

	values := map[string]string{
		"TOKEMON_DASHBOARD_USERNAME":      "kim",
		"TOKEMON_DASHBOARD_PASSWORD_HASH": "$2a$10$examplehashfromconfigfile",
	}
	applyServerSecurityConfig(flags, values, dashboardToken, devLoopback, trustedProxyCIDRs, dashboardUsername, dashboardPasswordHash)
	if *dashboardUsername != "kim" || *dashboardPasswordHash != "$2a$10$examplehashfromconfigfile" {
		t.Fatalf("config-file values lost: username=%q hash=%q", *dashboardUsername, *dashboardPasswordHash)
	}

	flags = flag.NewFlagSet("serve", flag.ContinueOnError)
	dashboardToken = flags.String("dashboard-token", "", "")
	devLoopback = flags.Bool("dev-loopback", false, "")
	trustedProxyCIDRs = flags.String("trusted-proxy-cidrs", "", "")
	dashboardUsername = flags.String("dashboard-username", "", "")
	dashboardPasswordHash = flags.String("dashboard-password-hash", "", "")
	if err := flags.Set("dashboard-username", "flag-user"); err != nil {
		t.Fatal(err)
	}
	if err := flags.Set("dashboard-password-hash", "$2a$flaghash"); err != nil {
		t.Fatal(err)
	}
	applyServerSecurityConfig(flags, values, dashboardToken, devLoopback, trustedProxyCIDRs, dashboardUsername, dashboardPasswordHash)
	if *dashboardUsername != "flag-user" || *dashboardPasswordHash != "$2a$flaghash" {
		t.Fatalf("explicit flags were overwritten: username=%q hash=%q", *dashboardUsername, *dashboardPasswordHash)
	}
}
