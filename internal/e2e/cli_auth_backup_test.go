//go:build e2e

package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

func runServeExpectFail(t *testing.T, homeDir string, env map[string]string, args ...string) (string, string, error) {
	t.Helper()
	requireE2E(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, tokemonBin, args...)
	cmd.Env = isolatedBaseEnv(homeDir, env)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

func writeAgentCodexRollout(t *testing.T, homeDir, relPath, sessionID string, totalTokens int64) {
	t.Helper()
	fullPath := filepath.Join(homeDir, ".codex", relPath)
	if err := os.MkdirAll(filepath.Dir(fullPath), 0o755); err != nil {
		t.Fatalf("mkdir codex rollout dir: %v", err)
	}
	cached := totalTokens / 2
	input := (totalTokens * 3) / 4
	uncachedInput := input
	if uncachedInput >= cached {
		// Codex payload input_tokens includes cached_input_tokens.
		input = uncachedInput
	}
	output := totalTokens - input
	lines := strings.Join([]string{
		fmt.Sprintf(`{"timestamp":"2026-11-20T10:00:00Z","type":"session_meta","payload":{"id":%q,"model_provider":"openai","cwd":"/Users/alice/secret-repo","base_instructions":"SECRET_SYSTEM_INSTRUCTIONS"}}`, sessionID),
		`{"timestamp":"2026-11-20T10:00:01Z","type":"turn_context","payload":{"model":"gpt-5.4","cwd":"/Users/alice/secret-repo"}}`,
		fmt.Sprintf(`{"timestamp":"2026-11-20T10:00:03Z","type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":%d,"cached_input_tokens":%d,"output_tokens":%d,"reasoning_output_tokens":0,"total_tokens":%d},"total_token_usage":{"input_tokens":%d,"cached_input_tokens":%d,"output_tokens":%d,"reasoning_output_tokens":0,"total_tokens":%d}}}}`,
			input, cached, output, totalTokens, input, cached, output, totalTokens),
	}, "\n") + "\n"
	if err := os.WriteFile(fullPath, []byte(lines), 0o644); err != nil {
		t.Fatalf("write codex rollout %s: %v", fullPath, err)
	}
}

func getJSONWithClient(t *testing.T, client *http.Client, fullURL string, dst any) {
	t.Helper()
	resp, err := client.Get(fullURL)
	if err != nil {
		t.Fatalf("GET %s: %v", fullURL, err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read %s: %v", fullURL, err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET %s status = %d: %s", fullURL, resp.StatusCode, string(body))
	}
	if err := json.Unmarshal(body, dst); err != nil {
		t.Fatalf("decode JSON from %s: %v\nbody: %s", fullURL, err, string(body))
	}
}

// --- Area A: CLI (serve, passwd, backup, status, TOKEMON_QUOTAS, agent) ---

func TestCLI_ServeConfigPrecedenceAndHealthz(t *testing.T) {
	requireE2E(t)
	workDir := t.TempDir()
	homeDir := filepath.Join(workDir, "home")
	if err := os.MkdirAll(homeDir, 0o755); err != nil {
		t.Fatalf("mkdir homeDir: %v", err)
	}
	dbPath := filepath.Join(workDir, "cli-prec.db")
	addr := freeLoopbackAddr(t)

	serverEnvPath := filepath.Join(workDir, "server.env")
	envContent := fmt.Sprintf(
		"TOKEMON_SERVER_ADDR=%s\nTOKEMON_DATABASE=%s\nTOKEMON_INGEST_TOKEN=cli-prec-tok\nTOKEMON_ANALYTICS_TIMEZONE=UTC\n",
		addr, dbPath,
	)
	if err := os.WriteFile(serverEnvPath, []byte(envContent), 0o600); err != nil {
		t.Fatalf("write server.env: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, tokemonBin, "serve", "--config", serverEnvPath, "--timezone", "America/New_York")
	cmd.Env = isolatedBaseEnv(homeDir, nil)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start tokemon serve: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	defer func() {
		_ = cmd.Process.Signal(os.Interrupt)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	}()

	baseURL := "http://" + addr
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := http.Get(baseURL + "/healthz")
		if err == nil {
			body, readErr := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if readErr == nil && resp.StatusCode == http.StatusOK {
				if !strings.Contains(string(body), `"status":"ok"`) {
					t.Fatalf("unexpected /healthz body: %s", string(body))
				}
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("server never became healthy\nstdout:\n%s\nstderr:\n%s", stdout.String(), stderr.String())
		}
		time.Sleep(25 * time.Millisecond)
	}

	req, err := http.NewRequest(http.MethodGet, baseURL+"/api/v1/analytics/overview", nil)
	if err != nil {
		t.Fatalf("new overview request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer cli-prec-tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET overview: %v", err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read overview: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("overview status = %d: %s", resp.StatusCode, string(raw))
	}
	var ov struct {
		Timezone string `json:"timezone"`
	}
	if err := json.Unmarshal(raw, &ov); err != nil {
		t.Fatalf("decode overview: %v", err)
	}
	if ov.Timezone != "America/New_York" {
		t.Fatalf("overview timezone = %q, want America/New_York (CLI flag must override server.env)", ov.Timezone)
	}
}

func TestCLI_PasswdStdinGeneratesVerifiableHash(t *testing.T) {
	requireE2E(t)
	homeDir := t.TempDir()
	const pass = "correct-horse-battery"
	stdout, stderr, err := runTokemonCLI(t, homeDir, nil, pass+"\n", "passwd", "--stdin")
	if err != nil {
		t.Fatalf("tokemon passwd --stdin failed: %v\nstderr: %s", err, stderr)
	}
	hash := strings.TrimSpace(stdout)
	if !strings.HasPrefix(hash, "$2a$") && !strings.HasPrefix(hash, "$2b$") {
		t.Fatalf("expected bcrypt hash prefix $2a$/$2b$, got %q", hash)
	}

	s := startTestServer(t, serverOpts{
		IngestToken: "passwd-ing-tok",
		ExtraEnv: map[string]string{
			"TOKEMON_DASHBOARD_USERNAME":      "alice",
			"TOKEMON_DASHBOARD_PASSWORD_HASH": hash,
		},
	})
	resp, loginHTML := s.doReq(t, http.MethodGet, "/login", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /login status = %d", resp.StatusCode)
	}
	csrf := extractCSRFToken(t, loginHTML)
	form := url.Values{
		"username":   {"alice"},
		"password":   {pass},
		"csrf_token": {csrf},
		"next":       {"/"},
	}
	loginResp, _ := s.doReq(t, http.MethodPost, "/login", []byte(form.Encode()), map[string]string{
		"Content-Type": "application/x-www-form-urlencoded",
		"Origin":       s.BaseURL,
		"Referer":      s.BaseURL + "/login",
	})
	if loginResp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST /login with passwd hash status = %d, want 303", loginResp.StatusCode)
	}
}

func TestCLI_BackupCreateVerifyRestoreRoundTrip(t *testing.T) {
	s := startTestServer(t, serverOpts{IngestToken: "bak-rt-tok"})
	s.ingestEvents(t, []ingestEvent{{
		EventID:      "bak-rt-1",
		MachineID:    "m-bak",
		SessionID:    "s-bak",
		Provider:     "openai",
		Model:        "gpt-5.4",
		Tool:         "codex",
		InputTokens:  i64(700),
		OutputTokens: i64(300),
		TotalTokens:  i64(1000),
	}})

	bakDir := filepath.Join(t.TempDir(), "backups")
	out, stderr, err := s.runCLI(t, nil, "backup", "create", "--destination", bakDir, "--database", s.DBPath)
	if err != nil {
		t.Fatalf("backup create failed: %v\nstderr: %s", err, stderr)
	}
	var createRes struct {
		Path           string `json:"path"`
		SchemaVersion  int    `json:"schema_version"`
		EventCount     int64  `json:"event_count"`
		LifetimeTokens int64  `json:"lifetime_tokens"`
		SHA256         string `json:"sha256"`
	}
	if err := json.Unmarshal([]byte(out), &createRes); err != nil {
		t.Fatalf("decode backup create JSON: %v\nraw: %s", err, out)
	}
	if createRes.EventCount != 1 || createRes.LifetimeTokens != 1000 || createRes.SHA256 == "" {
		t.Fatalf("unexpected BackupResult: %+v", createRes)
	}

	vOut, vErr, err := s.runCLI(t, nil, "backup", "verify", createRes.Path)
	if err != nil {
		t.Fatalf("backup verify failed: %v\nstderr: %s", err, vErr)
	}
	var verifyRes struct {
		EventCount     int64  `json:"event_count"`
		LifetimeTokens int64  `json:"lifetime_tokens"`
		SHA256         string `json:"sha256"`
	}
	if err := json.Unmarshal([]byte(vOut), &verifyRes); err != nil {
		t.Fatalf("decode backup verify JSON: %v\nraw: %s", err, vOut)
	}
	if verifyRes.EventCount != 1 || verifyRes.LifetimeTokens != 1000 || verifyRes.SHA256 != createRes.SHA256 {
		t.Fatalf("verify mismatch: %+v vs %+v", verifyRes, createRes)
	}

	dbPath := s.DBPath
	s.Stop()

	for _, p := range []string{dbPath, dbPath + "-wal", dbPath + "-shm"} {
		_ = os.Remove(p)
	}
	rOut, rErr, err := s.runCLI(t, nil, "backup", "restore", "--source", createRes.Path, "--database", dbPath)
	if err != nil {
		t.Fatalf("backup restore failed: %v\nstderr: %s", err, rErr)
	}
	if !strings.Contains(rOut, `"lifetime_tokens":1000`) {
		t.Fatalf("unexpected restore output: %s", rOut)
	}

	s2 := startTestServer(t, serverOpts{IngestToken: "bak-rt-tok", DBPath: dbPath})
	var ov struct {
		LifetimeTokens int64 `json:"lifetime_tokens"`
	}
	getJSONWithClient(t, s2.authClient(t), s2.BaseURL+"/api/v1/analytics/overview", &ov)
	if ov.LifetimeTokens != 1000 {
		t.Fatalf("restored lifetime_tokens = %d, want 1000", ov.LifetimeTokens)
	}
}

func jsonContainsNumber(v any, want float64) bool {
	switch x := v.(type) {
	case float64:
		return x == want
	case map[string]any:
		for _, child := range x {
			if jsonContainsNumber(child, want) {
				return true
			}
		}
	case []any:
		for _, child := range x {
			if jsonContainsNumber(child, want) {
				return true
			}
		}
	}
	return false
}

func TestCLI_StatusDefaultShortAndJSON(t *testing.T) {
	s := startTestServer(t, serverOpts{IngestToken: "status-tok"})
	s.ingestEvents(t, []ingestEvent{{
		EventID:      "status-ev-1",
		MachineID:    "m-status",
		SessionID:    "s-status",
		Provider:     "openai",
		Model:        "gpt-5.4",
		Tool:         "codex",
		InputTokens:  i64(400),
		OutputTokens: i64(100),
		TotalTokens:  i64(500),
	}})
	now := time.Now().UTC()
	s.sendHeartbeat(t, "m-status", "codex", heartbeatSource{
		Adapter:            "codex",
		IdentityHash:       "sha256:" + strings.Repeat("b", 64),
		LastSuccessfulSync: &now,
	})

	outDef, errDef, err := s.runCLI(t, nil, "status", "--server", s.BaseURL, "--token", s.IngestToken)
	if err != nil {
		t.Fatalf("tokemon status failed: %v\nstderr: %s", err, errDef)
	}
	if strings.TrimSpace(outDef) == "" || !strings.Contains(outDef, "500") {
		t.Fatalf("expected human-readable status with 500 tokens, got:\n%s", outDef)
	}

	outShort, errShort, err := s.runCLI(t, nil, "status", "--server", s.BaseURL, "--token", s.IngestToken, "--short")
	if err != nil {
		t.Fatalf("tokemon status --short failed: %v\nstderr: %s", err, errShort)
	}
	trimmedShort := strings.TrimSpace(outShort)
	if trimmedShort == "" || strings.Contains(trimmedShort, "\n") {
		t.Fatalf("status --short must be single non-empty line, got %q", outShort)
	}
	if !strings.Contains(trimmedShort, "500") {
		t.Fatalf("status --short missing ingested token total 500, got %q", trimmedShort)
	}

	outJSON, errJSON, err := s.runCLI(t, nil, "status", "--server", s.BaseURL, "--token", s.IngestToken, "--json")
	if err != nil {
		t.Fatalf("tokemon status --json failed: %v\nstderr: %s", err, errJSON)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(outJSON), &parsed); err != nil {
		t.Fatalf("status --json invalid JSON: %v\nraw: %s", err, outJSON)
	}
	if len(parsed) == 0 {
		t.Fatalf("status --json returned empty object")
	}
	if !jsonContainsNumber(parsed, 500) {
		t.Fatalf("status --json missing ingested token total 500: %s", outJSON)
	}
}

func TestCLI_StatusReadsAgentEnvConfig(t *testing.T) {
	s := startTestServer(t, serverOpts{IngestToken: "agent-status-tok"})
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "status-env-1",
		MachineID:   "m-env",
		Provider:    "anthropic",
		Model:       "claude-sonnet-4-5",
		Tool:        "claude-code",
		TotalTokens: i64(250),
	}})

	agentEnv := filepath.Join(t.TempDir(), "agent.env")
	cfg := fmt.Sprintf("TOKEMON_SERVER_URL=%s\nTOKEMON_INGEST_TOKEN=%s\n", s.BaseURL, s.IngestToken)
	if err := os.WriteFile(agentEnv, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write agent.env: %v", err)
	}

	out, stderr, err := s.runCLI(t, nil, "status", "--config", agentEnv, "--json")
	if err != nil {
		t.Fatalf("tokemon status --config failed: %v\nstderr: %s", err, stderr)
	}
	var parsed map[string]any
	if err := json.Unmarshal([]byte(out), &parsed); err != nil {
		t.Fatalf("status --config --json invalid JSON: %v\nraw: %s", err, out)
	}
	if len(parsed) == 0 || !jsonContainsNumber(parsed, 250) {
		t.Fatalf("status --config --json missing ingested token total 250: %s", out)
	}
}

func TestCLI_ServeLoadsQuotasFromServerEnvAndCLI(t *testing.T) {
	requireE2E(t)
	workDir := t.TempDir()
	homeDir := filepath.Join(workDir, "home")
	if err := os.MkdirAll(homeDir, 0o755); err != nil {
		t.Fatalf("mkdir homeDir: %v", err)
	}
	dbPath := filepath.Join(workDir, "quotas-env.db")
	quotasPath := filepath.Join(workDir, "quotas.yaml")
	quotasYAML := `schema_version: "1"
quotas:
  - provider: openai
    model: gpt-5.4
    label: OpenAI Monthly Cap
    reset_day: 1
    budget:
      tokens_per_month: 5000000
      cost_per_month: 120.5
`
	if err := os.WriteFile(quotasPath, []byte(quotasYAML), 0o600); err != nil {
		t.Fatalf("write quotas.yaml: %v", err)
	}

	addr := freeLoopbackAddr(t)
	serverEnvPath := filepath.Join(workDir, "server.env")
	envContent := fmt.Sprintf(
		"TOKEMON_SERVER_ADDR=%s\nTOKEMON_DATABASE=%s\nTOKEMON_INGEST_TOKEN=quota-env-tok\nTOKEMON_QUOTAS=%s\n",
		addr, dbPath, quotasPath,
	)
	if err := os.WriteFile(serverEnvPath, []byte(envContent), 0o600); err != nil {
		t.Fatalf("write server.env: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	cmd := exec.CommandContext(ctx, tokemonBin, "serve", "--config", serverEnvPath)
	cmd.Env = isolatedBaseEnv(homeDir, nil)
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		t.Fatalf("start serve: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	defer func() {
		_ = cmd.Process.Signal(os.Interrupt)
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			_ = cmd.Process.Kill()
			<-done
		}
	}()

	baseURL := "http://" + addr
	deadline := time.Now().Add(10 * time.Second)
	for {
		resp, err := http.Get(baseURL + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				break
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("server failed to start\nstdout:%s\nstderr:%s", stdout.String(), stderr.String())
		}
		time.Sleep(25 * time.Millisecond)
	}

	req, err := http.NewRequest(http.MethodGet, baseURL+"/api/v1/quotas", nil)
	if err != nil {
		t.Fatalf("new /api/v1/quotas request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer quota-env-tok")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("GET /api/v1/quotas: %v", err)
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /api/v1/quotas: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("quotas status = %d: %s", resp.StatusCode, string(body))
	}
	var quotas []struct {
		Label        string `json:"label"`
		TokensBudget *int64 `json:"tokens_budget"`
	}
	if err := json.Unmarshal(body, &quotas); err != nil {
		t.Fatalf("decode quotas JSON: %v", err)
	}
	if len(quotas) != 1 || quotas[0].Label != "OpenAI Monthly Cap" || quotas[0].TokensBudget == nil || *quotas[0].TokensBudget != 5000000 {
		t.Fatalf("unexpected quotas from server.env TOKEMON_QUOTAS: %+v", quotas)
	}
}

func TestCLI_ServeFailsClosedWithoutIngestToken(t *testing.T) {
	requireE2E(t)
	workDir := t.TempDir()
	homeDir := filepath.Join(workDir, "home")
	if err := os.MkdirAll(homeDir, 0o755); err != nil {
		t.Fatalf("mkdir homeDir: %v", err)
	}
	emptyCfg := filepath.Join(workDir, "empty.env")
	if err := os.WriteFile(emptyCfg, []byte(""), 0o600); err != nil {
		t.Fatalf("write empty.env: %v", err)
	}

	_, stderr, err := runServeExpectFail(t, homeDir, nil,
		"serve", "--config", emptyCfg, "--addr", "127.0.0.1:0", "--database", filepath.Join(workDir, "a.db"),
	)
	if err == nil {
		t.Fatalf("expected serve without ingest token to fail")
	}
	if !strings.Contains(stderr, "TOKEMON_INGEST_TOKEN is required") {
		t.Fatalf("unexpected stderr for missing ingest token: %s", stderr)
	}

	_, stderr2, err2 := runServeExpectFail(t, homeDir, nil,
		"serve", "--config", emptyCfg, "--dev-loopback", "--addr", "0.0.0.0:0", "--database", filepath.Join(workDir, "b.db"),
	)
	if err2 == nil {
		t.Fatalf("expected serve --dev-loopback on 0.0.0.0 to fail")
	}
	if !strings.Contains(strings.ToLower(stderr2), "loopback") {
		t.Fatalf("unexpected stderr for non-loopback --dev-loopback: %s", stderr2)
	}
}

func TestCLI_ServeRejectsInvalidTimezoneAndBadQuotasYAML(t *testing.T) {
	requireE2E(t)
	workDir := t.TempDir()
	homeDir := filepath.Join(workDir, "home")
	if err := os.MkdirAll(homeDir, 0o755); err != nil {
		t.Fatalf("mkdir homeDir: %v", err)
	}
	emptyCfg := filepath.Join(workDir, "empty.env")
	if err := os.WriteFile(emptyCfg, []byte(""), 0o600); err != nil {
		t.Fatalf("write empty.env: %v", err)
	}

	_, _, err := runServeExpectFail(t, homeDir, nil,
		"serve", "--config", emptyCfg, "--addr", "127.0.0.1:0", "--ingest-token", "t",
		"--database", filepath.Join(workDir, "tz.db"), "--timezone", "Invalid/Zone",
	)
	if err == nil {
		t.Fatalf("expected invalid timezone to fail")
	}

	badQuotas := []string{
		"schema_version: \"2\"\nquotas:\n  - provider: openai\n    label: bad\n    reset_day: 1\n    budget:\n      tokens_per_month: 100\n",
		"schema_version: \"1\"\nquotas:\n  - provider: openai\n    label: bad\n    reset_day: 32\n    budget:\n      tokens_per_month: 100\n",
		"schema_version: \"1\"\nquotas:\n  - provider: openai\n    label: bad\n    reset_day: 1\n    budget:\n      tokens_per_month: 0\n",
		"schema_version: \"1\"\nquotas:\n  - provider: openai\n    model: gpt-5\n    label: a\n    reset_day: 1\n    budget:\n      tokens_per_month: 100\n  - provider: openai\n    model: gpt-5\n    label: b\n    reset_day: 1\n    budget:\n      tokens_per_month: 200\n",
	}
	for i, q := range badQuotas {
		qPath := filepath.Join(workDir, fmt.Sprintf("bad-quota-%d.yaml", i))
		if err := os.WriteFile(qPath, []byte(q), 0o600); err != nil {
			t.Fatalf("write bad-quota-%d.yaml: %v", i, err)
		}
		_, _, err := runServeExpectFail(t, homeDir, nil,
			"serve", "--config", emptyCfg, "--addr", "127.0.0.1:0", "--ingest-token", "t",
			"--database", filepath.Join(workDir, fmt.Sprintf("q-%d.db", i)), "--quotas", qPath,
		)
		if err == nil {
			t.Fatalf("bad quotas case %d unexpectedly succeeded", i)
		}
	}
}

func TestCLI_ServeRejectsUnpairedPasswordAuthConfig(t *testing.T) {
	requireE2E(t)
	workDir := t.TempDir()
	homeDir := filepath.Join(workDir, "home")
	if err := os.MkdirAll(homeDir, 0o755); err != nil {
		t.Fatalf("mkdir homeDir: %v", err)
	}
	emptyCfg := filepath.Join(workDir, "empty.env")
	if err := os.WriteFile(emptyCfg, []byte(""), 0o600); err != nil {
		t.Fatalf("write empty.env: %v", err)
	}

	_, _, err := runServeExpectFail(t, homeDir, nil,
		"serve", "--config", emptyCfg, "--addr", "127.0.0.1:0", "--ingest-token", "t",
		"--database", filepath.Join(workDir, "p1.db"), "--dashboard-username", "admin",
	)
	if err == nil {
		t.Fatalf("expected unpaired username without hash to fail")
	}

	_, _, err2 := runServeExpectFail(t, homeDir, nil,
		"serve", "--config", emptyCfg, "--addr", "127.0.0.1:0", "--ingest-token", "t",
		"--database", filepath.Join(workDir, "p2.db"), "--dashboard-username", "admin",
		"--dashboard-password-hash", "not-bcrypt",
	)
	if err2 == nil {
		t.Fatalf("expected invalid bcrypt hash to fail")
	}
}

func TestCLI_PasswdRejectsOver72BytesAndNonTTYWithoutStdin(t *testing.T) {
	requireE2E(t)
	homeDir := t.TempDir()

	longPass := strings.Repeat("a", 73)
	_, stderr, err := runTokemonCLI(t, homeDir, nil, longPass+"\n", "passwd", "--stdin")
	if err == nil {
		t.Fatalf("expected 73-byte password to fail")
	}
	if !strings.Contains(stderr, "72") {
		t.Fatalf("expected stderr to mention 72-byte limit, got: %s", stderr)
	}

	_, _, errNoFlag := runTokemonCLI(t, homeDir, nil, "abc\n", "passwd")
	if errNoFlag == nil {
		t.Fatalf("expected tokemon passwd without --stdin on non-TTY to fail")
	}
}

func TestCLI_UnknownSubcommandAndBackupMissingArgs(t *testing.T) {
	requireE2E(t)
	homeDir := t.TempDir()

	if _, _, err := runTokemonCLI(t, homeDir, nil, "", "restore"); err == nil {
		t.Fatalf("expected top-level tokemon restore to fail")
	}
	if _, _, err := runTokemonCLI(t, homeDir, nil, "", "backup", "create"); err == nil {
		t.Fatalf("expected backup create without --destination to fail")
	}
	if _, _, err := runTokemonCLI(t, homeDir, nil, "", "backup", "create", "--destination", t.TempDir(), "--retention", "0"); err == nil {
		t.Fatalf("expected backup create --retention 0 to fail")
	}
	if _, _, err := runTokemonCLI(t, homeDir, nil, "", "backup", "restore", "--source", filepath.Join(t.TempDir(), "missing.db")); err == nil {
		t.Fatalf("expected backup restore with missing source to fail")
	}
}

func TestCLI_StatusFailsOnUnreachableOrUnauthorizedHub(t *testing.T) {
	s := startTestServer(t, serverOpts{IngestToken: "real-status-tok"})

	unusedAddr := freeLoopbackAddr(t)
	if _, _, err := s.runCLI(t, nil, "status", "--server", "http://"+unusedAddr, "--token", "x", "--timeout", "1s"); err == nil {
		t.Fatalf("expected status against unreachable hub to fail")
	}
	if _, _, err := s.runCLI(t, nil, "status", "--server", s.BaseURL, "--token", "wrong-token"); err == nil {
		t.Fatalf("expected status with wrong token to fail")
	}
}

func TestCLI_StatusPrivacyNoSensitiveLeak(t *testing.T) {
	s := startTestServer(t, serverOpts{IngestToken: "status-priv-tok"})
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "status-priv-1",
		MachineID:   "m-priv",
		SessionID:   "sess-secret-123",
		Project:     "my-secret-proj",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(800),
	}})

	for _, extra := range [][]string{nil, {"--short"}, {"--json"}} {
		args := append([]string{"status", "--server", s.BaseURL, "--token", s.IngestToken}, extra...)
		out, stderr, err := s.runCLI(t, nil, args...)
		if err != nil {
			t.Fatalf("tokemon %v failed: %v\nstderr: %s", args, err, stderr)
		}
		combined := out + "\n" + stderr
		for _, forbidden := range []string{"sess-secret-123", "my-secret-proj", "/Users/", ".jsonl"} {
			if strings.Contains(combined, forbidden) {
				t.Fatalf("tokemon %v leaked %q in output:\n%s", args, forbidden, combined)
			}
		}
	}
}

func TestE2EAgentDotenvAndHeartbeatIngest(t *testing.T) {
	s := startTestServer(t, serverOpts{IngestToken: "agent-dotenv-tok"})
	agentHome := filepath.Join(t.TempDir(), "agent-home")
	stateDB := filepath.Join(t.TempDir(), "state.db")
	writeAgentCodexRollout(t, agentHome, filepath.Join("sessions", "2026", "11", "20", "rollout-1.jsonl"), "codex-dotenv-001", 1200)

	agentEnv := filepath.Join(t.TempDir(), "agent.env")
	cfg := fmt.Sprintf(
		"TOKEMON_SERVER_URL=%s\nTOKEMON_INGEST_TOKEN=%s\nTOKEMON_MACHINE_ID=e2e-mac\nTOKEMON_HOME=%s\nTOKEMON_STATE=%s\nTOKEMON_ADAPTERS=codex\n",
		s.BaseURL, s.IngestToken, agentHome, stateDB,
	)
	if err := os.WriteFile(agentEnv, []byte(cfg), 0o600); err != nil {
		t.Fatalf("write agent.env: %v", err)
	}

	out1, err1, err := s.runCLI(t, nil, "agent", "--once", "--config", agentEnv)
	if err != nil {
		t.Fatalf("1st agent --once failed: %v\nstdout: %s\nstderr: %s", err, out1, err1)
	}
	out2, err2, err := s.runCLI(t, nil, "agent", "--once", "--config", agentEnv)
	if err != nil {
		t.Fatalf("2nd agent --once failed: %v\nstdout: %s\nstderr: %s", err, out2, err2)
	}
	if !strings.Contains(out2, "No new or changed usage records") && !strings.Contains(out2, "No supported local usage records found.") {
		t.Fatalf("expected 2nd agent run to report no new/changed/supported records, got:\n%s", out2)
	}

	client := s.authClient(t)
	var machines []struct {
		ID               string `json:"id"`
		Status           string `json:"status"`
		LifetimeTokens   int64  `json:"lifetime_tokens"`
		SourceCount      int    `json:"source_count"`
		SourceErrorCount int    `json:"source_error_count"`
		Sources          []struct {
			Adapter    string `json:"adapter"`
			SourceHash string `json:"source_hash"`
			State      string `json:"state"`
		} `json:"sources"`
	}
	getJSONWithClient(t, client, s.BaseURL+"/api/v1/machines", &machines)
	if len(machines) != 1 {
		t.Fatalf("machines len = %d, want 1: %+v", len(machines), machines)
	}
	m := machines[0]
	if m.ID != "e2e-mac" || m.Status != "Connected" || m.LifetimeTokens != 1200 || m.SourceCount != 1 || m.SourceErrorCount != 0 {
		t.Fatalf("unexpected machine record: %+v", m)
	}
	if len(m.Sources) != 1 || m.Sources[0].Adapter != "codex" || m.Sources[0].State != "Fresh" || !strings.HasPrefix(m.Sources[0].SourceHash, "sha256:") {
		t.Fatalf("unexpected machine sources: %+v", m.Sources)
	}

	waitOverviewLifetimeTokens(t, client, s.BaseURL, 1200)
}

func TestE2EAgentAuthFailureDoesNotCommitCursors(t *testing.T) {
	s := startTestServer(t, serverOpts{IngestToken: "agent-cursor-tok"})
	agentHome := filepath.Join(t.TempDir(), "agent-home")
	stateDB := filepath.Join(t.TempDir(), "state.db")
	emptyCfg := filepath.Join(t.TempDir(), "missing-agent.env")
	writeAgentCodexRollout(t, agentHome, filepath.Join("sessions", "2026", "11", "20", "rollout-cur.jsonl"), "codex-cur-001", 1200)

	if _, _, err := s.runCLI(t, nil,
		"agent", "--once",
		"--config", emptyCfg,
		"--server", s.BaseURL,
		"--token", "wrong-token",
		"--machine-id", "m-cursor",
		"--home", agentHome,
		"--state", stateDB,
		"--adapters", "codex",
	); err == nil {
		t.Fatalf("expected agent --once with wrong token to fail")
	}

	client := s.authClient(t)
	var ov struct {
		LifetimeTokens int64 `json:"lifetime_tokens"`
	}
	getJSONWithClient(t, client, s.BaseURL+"/api/v1/analytics/overview", &ov)
	if ov.LifetimeTokens != 0 {
		t.Fatalf("expected 0 tokens after failed ingest, got %d", ov.LifetimeTokens)
	}

	_, stderr, err := s.runCLI(t, nil,
		"agent", "--once",
		"--config", emptyCfg,
		"--server", s.BaseURL,
		"--token", s.IngestToken,
		"--machine-id", "m-cursor",
		"--home", agentHome,
		"--state", stateDB,
		"--adapters", "codex",
	)
	if err != nil {
		t.Fatalf("retry agent --once with valid token failed: %v\nstderr: %s", err, stderr)
	}
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 1200)
}

func TestE2EIngestBatchRejectsPrivacyAndOversize(t *testing.T) {
	s := startTestServer(t, serverOpts{IngestToken: "privacy-batch-tok"})
	headers := map[string]string{
		"Content-Type":  "application/json",
		"Authorization": "Bearer " + s.IngestToken,
	}

	cases := []struct {
		name      string
		body      string
		forbidden []string
	}{
		{
			name:      "path_in_project",
			body:      `{"events":[{"schema_version":"1","event_id":"priv-1","timestamp":"2026-11-20T10:00:00Z","machine_id":"m1","project":"/Users/alice/secret-repo","provider":"openai","model":"gpt-5.4","tool":"codex","total_tokens":10,"token_accuracy":"reported","source":{"adapter":"codex","adapter_version":"1"}}]}`,
			forbidden: []string{"/Users/alice/secret-repo"},
		},
		{
			name:      "whitespace_in_session_id",
			body:      `{"events":[{"schema_version":"1","event_id":"priv-2","timestamp":"2026-11-20T10:00:00Z","machine_id":"m1","session_id":"bad\tsession\tid","provider":"openai","model":"gpt-5.4","tool":"codex","total_tokens":10,"token_accuracy":"reported","source":{"adapter":"codex","adapter_version":"1"}}]}`,
			forbidden: []string{"bad\tsession\tid"},
		},
		{
			name:      "sensitive_metadata_key",
			body:      `{"events":[{"schema_version":"1","event_id":"priv-3","timestamp":"2026-11-20T10:00:00Z","machine_id":"m1","provider":"openai","model":"gpt-5.4","tool":"codex","total_tokens":10,"token_accuracy":"reported","source":{"adapter":"codex","adapter_version":"1"},"metadata":{"prompt":"leak"}}]}`,
			forbidden: []string{"leak"},
		},
		{
			name: "trailing_json_object",
			body: `{"events":[{"schema_version":"1","event_id":"priv-4","timestamp":"2026-11-20T10:00:00Z","machine_id":"m1","provider":"openai","model":"gpt-5.4","tool":"codex","total_tokens":10,"token_accuracy":"reported","source":{"adapter":"codex","adapter_version":"1"}}]}{"extra":1}`,
		},
		{
			name: "empty_events",
			body: `{"events":[]}`,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, respBody := s.doReq(t, http.MethodPost, "/api/v1/events/batch", []byte(tc.body), headers)
			if resp.StatusCode != http.StatusBadRequest {
				t.Fatalf("status = %d, want 400: %s", resp.StatusCode, string(respBody))
			}
			for _, f := range tc.forbidden {
				if strings.Contains(string(respBody), f) {
					t.Fatalf("error response echoed forbidden string %q: %s", f, string(respBody))
				}
			}
		})
	}

	var ov struct {
		LifetimeTokens int64 `json:"lifetime_tokens"`
	}
	getJSONWithClient(t, s.authClient(t), s.BaseURL+"/api/v1/analytics/overview", &ov)
	if ov.LifetimeTokens != 0 {
		t.Fatalf("expected 0 tokens in DB after invalid batches, got %d", ov.LifetimeTokens)
	}
}

// --- Area B: Auth, Security, CAR-205 & Privacy (All Baseline-Compatible) ---

func TestAuth_PasswordLoginCookieFlowAndNextRedirect(t *testing.T) {
	s := startTestServer(t, serverOpts{
		IngestToken: "auth-ing-1",
		Username:    "admin",
		Password:    "auth-pass-1",
	})

	resp, _ := s.doReq(t, http.MethodGet, "/analytics?period=7d", nil, map[string]string{
		"Accept": "text/html,application/xhtml+xml",
	})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("anonymous HTML GET /analytics status = %d, want 303", resp.StatusCode)
	}
	if loc := resp.Header.Get("Location"); loc != "/login?next=%2Fanalytics%3Fperiod%3D7d" {
		t.Fatalf("Location = %q, want /login?next=%%2Fanalytics%%3Fperiod%%3D7d", loc)
	}

	loginResp, loginHTML := s.doReq(t, http.MethodGet, "/login?next=%2Fanalytics%3Fperiod%3D7d", nil, nil)
	if loginResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /login status = %d, want 200", loginResp.StatusCode)
	}
	if cc := loginResp.Header.Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("GET /login Cache-Control = %q, want no-store", cc)
	}
	csrf := extractCSRFToken(t, loginHTML)

	form := url.Values{
		"username":   {s.Username},
		"password":   {s.Password},
		"csrf_token": {csrf},
		"next":       {"/analytics?period=7d"},
	}
	postResp, _ := s.doReq(t, http.MethodPost, "/login", []byte(form.Encode()), map[string]string{
		"Content-Type": "application/x-www-form-urlencoded",
		"Origin":       s.BaseURL,
		"Referer":      s.BaseURL + "/login",
	})
	if postResp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST /login status = %d, want 303", postResp.StatusCode)
	}
	if loc := postResp.Header.Get("Location"); loc != "/analytics?period=7d" {
		t.Fatalf("POST /login Location = %q, want /analytics?period=7d", loc)
	}

	var sessionCookie *http.Cookie
	for _, c := range postResp.Cookies() {
		if c.Name == "tokemon_session" {
			sessionCookie = c
		}
	}
	if sessionCookie == nil || sessionCookie.Value == "" {
		t.Fatalf("missing tokemon_session cookie on login response")
	}
	if !sessionCookie.HttpOnly || sessionCookie.SameSite != http.SameSiteLaxMode || sessionCookie.Path != "/" || sessionCookie.MaxAge != 43200 {
		t.Fatalf("unexpected cookie attributes: %+v", sessionCookie)
	}

	authResp, authBody := s.doReq(t, http.MethodGet, "/analytics?period=7d", nil, map[string]string{
		"Accept": "text/html",
		"Cookie": sessionCookie.Name + "=" + sessionCookie.Value,
	})
	if authResp.StatusCode != http.StatusOK || !strings.Contains(string(authBody), "Analytics") {
		t.Fatalf("authenticated GET /analytics status = %d, body len = %d", authResp.StatusCode, len(authBody))
	}
}

func TestAuth_BearerDashboardTokenAndIngestFallback(t *testing.T) {
	s1 := startTestServer(t, serverOpts{IngestToken: "fallback-ing-tok"})
	for _, path := range []string{"/", "/api/v1/analytics/overview", "/api/v1/machines"} {
		resp, _ := s1.doReq(t, http.MethodGet, path, nil, map[string]string{
			"Authorization": "Bearer fallback-ing-tok",
		})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("fallback Bearer GET %s status = %d, want 200", path, resp.StatusCode)
		}
	}

	s2 := startTestServer(t, serverOpts{
		IngestToken:    "ing-only-tok",
		DashboardToken: "dash-only-tok",
	})
	respDash, _ := s2.doReq(t, http.MethodGet, "/api/v1/machines", nil, map[string]string{
		"Authorization": "Bearer dash-only-tok",
	})
	if respDash.StatusCode != http.StatusOK {
		t.Fatalf("distinct DashboardToken GET /api/v1/machines status = %d, want 200", respDash.StatusCode)
	}
	respIng, _ := s2.doReq(t, http.MethodGet, "/api/v1/machines", nil, map[string]string{
		"Authorization": "Bearer ing-only-tok",
	})
	if respIng.StatusCode != http.StatusUnauthorized {
		t.Fatalf("IngestToken against dashboard when DashboardToken differs: status = %d, want 401", respIng.StatusCode)
	}
}

func TestAuth_LogoutClearsCookieAndRedirects(t *testing.T) {
	s := startTestServer(t, serverOpts{
		IngestToken: "logout-ing",
		Username:    "admin",
		Password:    "logout-pass",
	})
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New: %v", err)
	}
	client := &http.Client{Jar: jar, Timeout: 10 * time.Second}
	loginPageResp, err := client.Get(s.BaseURL + "/login")
	if err != nil {
		t.Fatalf("GET /login: %v", err)
	}
	loginHTML, readErr := io.ReadAll(loginPageResp.Body)
	_ = loginPageResp.Body.Close()
	if readErr != nil {
		t.Fatalf("read /login: %v", readErr)
	}
	if loginPageResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /login status = %d, want 200", loginPageResp.StatusCode)
	}
	csrf := extractCSRFToken(t, loginHTML)

	form := url.Values{"username": {s.Username}, "password": {s.Password}, "csrf_token": {csrf}, "next": {"/"}}
	req, err := http.NewRequest(http.MethodPost, s.BaseURL+"/login", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("new POST /login request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", s.BaseURL)
	req.Header.Set("Referer", s.BaseURL+"/login")
	r, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST /login: %v", err)
	}
	homeHTML, readErr := io.ReadAll(r.Body)
	_ = r.Body.Close()
	if readErr != nil {
		t.Fatalf("read POST /login response: %v", readErr)
	}
	if r.StatusCode != http.StatusOK {
		t.Fatalf("POST /login status = %d, want 200", r.StatusCode)
	}

	logoutCSRF := extractCSRFToken(t, homeHTML)
	u, err := url.Parse(s.BaseURL)
	if err != nil {
		t.Fatalf("parse server BaseURL %q: %v", s.BaseURL, err)
	}
	var cookieHeader string
	for _, c := range jar.Cookies(u) {
		if c.Name == "tokemon_session" {
			cookieHeader = c.Name + "=" + c.Value
		}
	}
	if cookieHeader == "" {
		t.Fatalf("missing session cookie before logout")
	}

	logoutForm := url.Values{"csrf_token": {logoutCSRF}}
	logoutResp, _ := s.doReq(t, http.MethodPost, "/logout", []byte(logoutForm.Encode()), map[string]string{
		"Content-Type": "application/x-www-form-urlencoded",
		"Origin":       s.BaseURL,
		"Referer":      s.BaseURL + "/",
		"Cookie":       cookieHeader,
	})
	if logoutResp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST /logout status = %d, want 303", logoutResp.StatusCode)
	}
	if loc := logoutResp.Header.Get("Location"); loc != "/login" {
		t.Fatalf("POST /logout Location = %q, want /login", loc)
	}
	var cleared bool
	for _, c := range logoutResp.Cookies() {
		if c.Name == "tokemon_session" && c.Value == "" && c.MaxAge <= 0 {
			cleared = true
		}
	}
	if !cleared {
		t.Fatalf("POST /logout did not clear tokemon_session cookie: %+v", logoutResp.Cookies())
	}
}

func TestAuth_IngestAndHeartbeatAcceptValidToken(t *testing.T) {
	s := startTestServer(t, serverOpts{IngestToken: "valid-ingest-tok"})

	evPayload := []byte(`{"events":[{"schema_version":"1","event_id":"auth-ing-1","timestamp":"2026-11-20T10:00:00Z","machine_id":"m-auth","provider":"openai","model":"gpt-5.4","tool":"codex","total_tokens":10,"token_accuracy":"reported","source":{"adapter":"codex","adapter_version":"1"}}]}`)
	resp1, _ := s.doReq(t, http.MethodPost, "/api/v1/events/batch", evPayload, map[string]string{
		"Content-Type":  "application/json",
		"Authorization": "Bearer " + s.IngestToken,
	})
	if resp1.StatusCode != http.StatusOK {
		t.Fatalf("Bearer ingest status = %d, want 200", resp1.StatusCode)
	}

	evPayload2 := []byte(`{"events":[{"schema_version":"1","event_id":"auth-ing-2","timestamp":"2026-11-20T10:01:00Z","machine_id":"m-auth","provider":"openai","model":"gpt-5.4","tool":"codex","total_tokens":20,"token_accuracy":"reported","source":{"adapter":"codex","adapter_version":"1"}}]}`)
	resp2, _ := s.doReq(t, http.MethodPost, "/api/v1/events/batch", evPayload2, map[string]string{
		"Content-Type":           "application/json",
		"X-Tokemon-Ingest-Token": s.IngestToken,
	})
	if resp2.StatusCode != http.StatusOK {
		t.Fatalf("X-Tokemon-Ingest-Token ingest status = %d, want 200", resp2.StatusCode)
	}

	hbPayload := []byte(`{"machine_id":"m-auth","agent_version":"1.0","operating_system":"darwin","architecture":"arm64","adapters":[{"id":"codex","version":"1.0"}],"source_count":0,"source_error_count":0,"sources":[]}`)
	hb1, _ := s.doReq(t, http.MethodPost, "/api/v1/agents/heartbeat", hbPayload, map[string]string{
		"Content-Type":  "application/json",
		"Authorization": "Bearer " + s.IngestToken,
	})
	if hb1.StatusCode != http.StatusOK {
		t.Fatalf("Bearer heartbeat status = %d, want 200", hb1.StatusCode)
	}
	hb2, _ := s.doReq(t, http.MethodPost, "/api/v1/agents/heartbeat", hbPayload, map[string]string{
		"Content-Type":           "application/json",
		"X-Tokemon-Ingest-Token": s.IngestToken,
	})
	if hb2.StatusCode != http.StatusOK {
		t.Fatalf("X-Tokemon-Ingest-Token heartbeat status = %d, want 200", hb2.StatusCode)
	}
}

var (
	navBlockRe       = regexp.MustCompile(`(?s)<nav\b[^>]*class="[^"]*\bnav\b[^"]*"[^>]*>(.*?)</nav>`)
	navInsightsLnkRe = regexp.MustCompile(`<a\b[^>]*class="[^"]*nav-link[^"]*"[^>]*href="/insights"`)
)

func TestAuth_CAR205InsightsReachableButUnlinkedInNav(t *testing.T) {
	s := startTestServer(t, serverOpts{
		IngestToken:   "car205-tok",
		Username:      "admin",
		Password:      "car205-pass",
		EnableTokedex: true,
	})
	client := s.authClient(t)

	for _, path := range []string{"/insights", "/api/v1/insights"} {
		resp, err := client.Get(s.BaseURL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		_ = resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s status = %d, want 200", path, resp.StatusCode)
		}
	}

	for _, page := range []string{"/", "/analytics", "/sessions", "/settings", "/insights", "/tokedex"} {
		resp, err := client.Get(s.BaseURL + page)
		if err != nil {
			t.Fatalf("GET %s: %v", page, err)
		}
		body, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			t.Fatalf("read %s: %v", page, readErr)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s status = %d, want 200", page, resp.StatusCode)
		}
		navMatch := navBlockRe.FindSubmatch(body)
		if len(navMatch) < 2 {
			t.Fatalf("page %s missing <nav class=\"nav\">", page)
		}
		if navInsightsLnkRe.Match(navMatch[1]) || strings.Contains(string(navMatch[1]), `href="/insights"`) {
			t.Fatalf("CAR-205 violation on %s: /insights linked inside primary .nav:\n%s", page, string(navMatch[1]))
		}
	}
}

func TestAuth_MetadataOnlyPrivacyAcrossReadSurfaces(t *testing.T) {
	s := startTestServer(t, serverOpts{
		IngestToken: "priv-surf-tok",
		Username:    "admin",
		Password:    "priv-surf-pass",
	})
	s.ingestEvents(t, []ingestEvent{{
		EventID:      "priv-surf-1",
		MachineID:    "m-priv-surf",
		SessionID:    "sess-opaque-1",
		Project:      "alpha",
		Provider:     "openai",
		Model:        "gpt-5.4",
		Tool:         "codex",
		InputTokens:  i64(40),
		OutputTokens: i64(20),
		TotalTokens:  i64(60),
	}})
	now := time.Now().UTC()
	s.sendHeartbeat(t, "m-priv-surf", "codex", heartbeatSource{
		Adapter:            "codex",
		IdentityHash:       "sha256:" + strings.Repeat("c", 64),
		LastSuccessfulSync: &now,
	})

	client := s.authClient(t)

	// (a) Verify non-vacuous rendering of seeded session, machine, and token counts.
	var initialSessions []struct {
		SessionID string `json:"session_id"`
		Project   string `json:"project"`
	}
	getJSONWithClient(t, client, s.BaseURL+"/api/v1/sessions", &initialSessions)
	if len(initialSessions) != 1 || initialSessions[0].SessionID != "sess-opaque-1" {
		t.Fatalf("expected sess-opaque-1 in /api/v1/sessions, got %+v", initialSessions)
	}
	sessPageResp, err := client.Get(s.BaseURL + "/sessions")
	if err != nil {
		t.Fatalf("GET /sessions: %v", err)
	}
	sessPageBody, readErr := io.ReadAll(sessPageResp.Body)
	_ = sessPageResp.Body.Close()
	if readErr != nil {
		t.Fatalf("read /sessions: %v", readErr)
	}
	if sessPageResp.StatusCode != http.StatusOK || !strings.Contains(string(sessPageBody), "sess-opaque-1") {
		t.Fatalf("/sessions (status=%d) did not render sess-opaque-1", sessPageResp.StatusCode)
	}
	var initialMachines []struct {
		ID string `json:"id"`
	}
	getJSONWithClient(t, client, s.BaseURL+"/api/v1/machines", &initialMachines)
	if len(initialMachines) != 1 || initialMachines[0].ID != "m-priv-surf" {
		t.Fatalf("expected m-priv-surf in /api/v1/machines, got %+v", initialMachines)
	}
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 60)

	// (b) Verify POST /api/v1/events/batch rejects raw filesystem path and sensitive prompt/title metadata.
	badBatchHdrs := map[string]string{
		"Content-Type":  "application/json",
		"Authorization": "Bearer " + s.IngestToken,
	}
	for _, badBody := range []string{
		fmt.Sprintf(`{"events":[{"schema_version":"1","event_id":"priv-rej-path","timestamp":%q,"machine_id":"m-priv-surf","project":"/Users/alice/secret-repo","provider":"openai","model":"gpt-5.4","tool":"codex","total_tokens":10,"token_accuracy":"reported","source":{"adapter":"codex","adapter_version":"1"}}]}`, now.Format(time.RFC3339)),
		fmt.Sprintf(`{"events":[{"schema_version":"1","event_id":"priv-rej-meta","timestamp":%q,"machine_id":"m-priv-surf","provider":"openai","model":"gpt-5.4","tool":"codex","total_tokens":10,"token_accuracy":"reported","source":{"adapter":"codex","adapter_version":"1"},"metadata":{"user_prompt":"SECRET_PROMPT_DO_NOT_LEAK","assistant_response":"SECRET_RESPONSE_DO_NOT_LEAK","conversation_title":"SECRET_TITLE_DO_NOT_LEAK"}}]}`, now.Format(time.RFC3339)),
	} {
		rejResp, rejBody := s.doReq(t, http.MethodPost, "/api/v1/events/batch", []byte(badBody), badBatchHdrs)
		if rejResp.StatusCode != http.StatusBadRequest {
			t.Fatalf("expected 400 for sensitive batch payload, got %d: %s", rejResp.StatusCode, string(rejBody))
		}
	}

	// Seed a raw Codex .jsonl transcript containing prompts, responses, titles, and raw paths via tokemon agent --once.
	agentHome := filepath.Join(t.TempDir(), "agent-home")
	stateDB := filepath.Join(t.TempDir(), "agent-state.db")
	emptyCfg := filepath.Join(t.TempDir(), "empty-agent.env")
	rollout := filepath.Join(agentHome, ".codex", "sessions", "2026", "11", "20", "secret-rollout.jsonl")
	if err := os.MkdirAll(filepath.Dir(rollout), 0o755); err != nil {
		t.Fatalf("mkdir codex rollout dir: %v", err)
	}
	tBase := now.Add(-time.Hour)
	rawLines := strings.Join([]string{
		fmt.Sprintf(`{"timestamp":%q,"type":"session_meta","payload":{"id":"priv-agent-sess-1","model_provider":"openai","cwd":"/Users/alice/secret-repo","base_instructions":"SECRET_SYSTEM_INSTRUCTIONS"}}`, tBase.Format(time.RFC3339)),
		fmt.Sprintf(`{"timestamp":%q,"type":"turn_context","payload":{"model":"gpt-5.4","cwd":"/Users/alice/secret-repo","summary":"SECRET_TITLE_DO_NOT_LEAK"}}`, tBase.Add(time.Second).Format(time.RFC3339)),
		fmt.Sprintf(`{"timestamp":%q,"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"SECRET_PROMPT_DO_NOT_LEAK"}]}}`, tBase.Add(2*time.Second).Format(time.RFC3339)),
		fmt.Sprintf(`{"timestamp":%q,"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"SECRET_RESPONSE_DO_NOT_LEAK"}]}}`, tBase.Add(3*time.Second).Format(time.RFC3339)),
		fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"cached_input_tokens":0,"output_tokens":40,"reasoning_output_tokens":0,"total_tokens":140}}}}`, tBase.Add(4*time.Second).Format(time.RFC3339)),
	}, "\n") + "\n"
	if err := os.WriteFile(rollout, []byte(rawLines), 0o644); err != nil {
		t.Fatalf("write secret codex rollout: %v", err)
	}

	_, agentStderr, err := s.runCLI(t, nil,
		"agent", "--once",
		"--config", emptyCfg,
		"--server", s.BaseURL,
		"--token", s.IngestToken,
		"--machine-id", "m-priv-agent",
		"--home", agentHome,
		"--state", stateDB,
		"--adapters", "codex",
	)
	if err != nil {
		t.Fatalf("agent --once failed: %v\nstderr: %s", err, agentStderr)
	}

	waitOverviewLifetimeTokens(t, client, s.BaseURL, 200)
	type sessionSummary struct {
		SessionID string `json:"session_id"`
		Project   string `json:"project"`
	}
	var afterSessions []sessionSummary
	var foundAgentSession bool
	sessionsDeadline := time.Now().Add(4 * time.Second)
	for {
		afterSessions = nil
		getJSONWithClient(t, client, s.BaseURL+"/api/v1/sessions", &afterSessions)
		for _, row := range afterSessions {
			if row.SessionID == "priv-agent-sess-1" {
				foundAgentSession = true
				if row.Project != "secret-repo" {
					t.Fatalf("seeded agent session project = %q, want secret-repo", row.Project)
				}
				break
			}
		}
		if foundAgentSession || time.Now().After(sessionsDeadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if !foundAgentSession {
		t.Fatalf("seeded agent session priv-agent-sess-1 missing from /api/v1/sessions after 4s: %+v", afterSessions)
	}

	for _, path := range []string{
		"/", "/analytics", "/sessions", "/insights",
		"/api/v1/machines", "/api/v1/insights", "/api/v1/sessions",
	} {
		resp, err := client.Get(s.BaseURL + path)
		if err != nil {
			t.Fatalf("GET %s: %v", path, err)
		}
		body, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			t.Fatalf("read %s: %v", path, readErr)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET %s status = %d", path, resp.StatusCode)
		}
		lower := strings.ToLower(string(body))
		for _, forbidden := range []string{
			"secret_prompt_do_not_leak",
			"secret_response_do_not_leak",
			"secret_title_do_not_leak",
			"secret_system_instructions",
			"/users/alice",
			"/users/",
			".jsonl",
			"conversation_title",
			"user_prompt",
			"assistant_response",
		} {
			if strings.Contains(lower, forbidden) {
				t.Fatalf("surface %s leaked %q", path, forbidden)
			}
		}
	}
}

func TestAuth_UnauthHTMLvsAPIResponses(t *testing.T) {
	s := startTestServer(t, serverOpts{
		IngestToken: "unauth-ing-tok",
		Username:    "admin",
		Password:    "unauth-pass",
	})

	respHTML, _ := s.doReq(t, http.MethodGet, "/", nil, map[string]string{"Accept": "text/html"})
	if respHTML.StatusCode != http.StatusSeeOther || respHTML.Header.Get("Location") != "/login?next=%2F" {
		t.Fatalf("unauth HTML / got status=%d loc=%q", respHTML.StatusCode, respHTML.Header.Get("Location"))
	}

	respJSON, bodyJSON := s.doReq(t, http.MethodGet, "/", nil, map[string]string{"Accept": "application/json"})
	if respJSON.StatusCode != http.StatusUnauthorized || !strings.Contains(respJSON.Header.Get("WWW-Authenticate"), "Basic") {
		t.Fatalf("unauth JSON / got status=%d www-auth=%q body=%s", respJSON.StatusCode, respJSON.Header.Get("WWW-Authenticate"), string(bodyJSON))
	}

	respInsHTML, _ := s.doReq(t, http.MethodGet, "/insights", nil, map[string]string{"Accept": "text/html"})
	if respInsHTML.StatusCode != http.StatusSeeOther || respInsHTML.Header.Get("Location") != "/login?next=%2Finsights" {
		t.Fatalf("unauth HTML /insights got status=%d loc=%q", respInsHTML.StatusCode, respInsHTML.Header.Get("Location"))
	}

	respInsAPI, _ := s.doReq(t, http.MethodGet, "/api/v1/insights", nil, nil)
	if respInsAPI.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauth /api/v1/insights status = %d, want 401", respInsAPI.StatusCode)
	}
}

func TestAuth_LoginCSRFAndSameOriginEnforcement(t *testing.T) {
	s := startTestServer(t, serverOpts{
		IngestToken: "csrf-ing-tok",
		Username:    "admin",
		Password:    "csrf-pass",
	})

	_, loginHTML := s.doReq(t, http.MethodGet, "/login", nil, nil)
	validCSRF := extractCSRFToken(t, loginHTML)

	badLoginCases := []struct {
		name    string
		form    url.Values
		headers map[string]string
	}{
		{
			name:    "missing_csrf",
			form:    url.Values{"username": {s.Username}, "password": {s.Password}},
			headers: map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Origin": s.BaseURL},
		},
		{
			name:    "wrong_csrf",
			form:    url.Values{"username": {s.Username}, "password": {s.Password}, "csrf_token": {"forged-token"}},
			headers: map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Origin": s.BaseURL},
		},
		{
			name:    "cross_origin",
			form:    url.Values{"username": {s.Username}, "password": {s.Password}, "csrf_token": {validCSRF}},
			headers: map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Origin": "https://evil.example"},
		},
		{
			name:    "missing_origin_and_referer",
			form:    url.Values{"username": {s.Username}, "password": {s.Password}, "csrf_token": {validCSRF}},
			headers: map[string]string{"Content-Type": "application/x-www-form-urlencoded"},
		},
	}

	for _, tc := range badLoginCases {
		t.Run("login_"+tc.name, func(t *testing.T) {
			resp, _ := s.doReq(t, http.MethodPost, "/login", []byte(tc.form.Encode()), tc.headers)
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("POST /login (%s) status = %d, want 403", tc.name, resp.StatusCode)
			}
		})
	}

	for _, tc := range []struct {
		name    string
		csrf    string
		headers map[string]string
	}{
		{"missing_csrf", "", map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Origin": s.BaseURL}},
		{"cross_origin", validCSRF, map[string]string{"Content-Type": "application/x-www-form-urlencoded", "Origin": "https://evil.example"}},
	} {
		t.Run("logout_"+tc.name, func(t *testing.T) {
			f := url.Values{"csrf_token": {tc.csrf}}
			resp, _ := s.doReq(t, http.MethodPost, "/logout", []byte(f.Encode()), tc.headers)
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("POST /logout (%s) status = %d, want 403", tc.name, resp.StatusCode)
			}
		})
	}
}

func TestAuth_LoginOpenRedirectPrevention(t *testing.T) {
	s := startTestServer(t, serverOpts{
		IngestToken: "redir-ing-tok",
		Username:    "admin",
		Password:    "redir-pass",
	})
	_, loginHTML := s.doReq(t, http.MethodGet, "/login", nil, nil)
	csrf := extractCSRFToken(t, loginHTML)

	for _, evilNext := range []string{
		"//evil.example",
		`/\evil.example`,
		"https://evil.example",
		"%2F%2Fevil.example",
		"/ok\r\nSet-Cookie: evil=1",
	} {
		form := url.Values{
			"username":   {s.Username},
			"password":   {s.Password},
			"csrf_token": {csrf},
			"next":       {evilNext},
		}
		resp, _ := s.doReq(t, http.MethodPost, "/login", []byte(form.Encode()), map[string]string{
			"Content-Type": "application/x-www-form-urlencoded",
			"Origin":       s.BaseURL,
			"Referer":      s.BaseURL + "/login",
		})
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("POST /login next=%q status = %d, want 303", evilNext, resp.StatusCode)
		}
		if loc := resp.Header.Get("Location"); loc != "/" {
			t.Fatalf("POST /login next=%q Location = %q, want /", evilNext, loc)
		}
	}
}

func TestAuth_WrongPasswordThrottle429AndHealthBypass(t *testing.T) {
	s := startTestServer(t, serverOpts{
		IngestToken: "throttle-ing-tok",
		Username:    "admin",
		Password:    "throttle-right-pass",
	})
	_, loginHTML := s.doReq(t, http.MethodGet, "/login", nil, nil)
	csrf := extractCSRFToken(t, loginHTML)

	for i := 1; i <= 5; i++ {
		form := url.Values{
			"username":   {s.Username},
			"password":   {"wrong-password"},
			"csrf_token": {csrf},
			"next":       {"/"},
		}
		resp, body := s.doReq(t, http.MethodPost, "/login", []byte(form.Encode()), map[string]string{
			"Content-Type": "application/x-www-form-urlencoded",
			"Origin":       s.BaseURL,
			"Referer":      s.BaseURL + "/login",
		})
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("attempt %d status = %d, want 401", i, resp.StatusCode)
		}
		if !strings.Contains(string(body), "Invalid username or password.") {
			t.Fatalf("attempt %d missing error alert", i)
		}
		for _, c := range resp.Cookies() {
			if c.Name == "tokemon_session" && c.Value != "" {
				t.Fatalf("attempt %d unexpectedly issued session cookie", i)
			}
		}
	}

	form := url.Values{
		"username":   {s.Username},
		"password":   {"wrong-password"},
		"csrf_token": {csrf},
		"next":       {"/"},
	}
	resp6, _ := s.doReq(t, http.MethodPost, "/login", []byte(form.Encode()), map[string]string{
		"Content-Type": "application/x-www-form-urlencoded",
		"Origin":       s.BaseURL,
		"Referer":      s.BaseURL + "/login",
	})
	if resp6.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("6th attempt status = %d, want 429", resp6.StatusCode)
	}
	if ra := resp6.Header.Get("Retry-After"); ra == "" {
		t.Fatalf("6th attempt missing Retry-After header")
	}

	hz, _ := s.doReq(t, http.MethodGet, "/healthz", nil, nil)
	if hz.StatusCode != http.StatusOK {
		t.Fatalf("/healthz during login lockout status = %d, want 200", hz.StatusCode)
	}
	res := s.ingestEvents(t, []ingestEvent{{
		EventID:     "throttle-bypass-1",
		MachineID:   "m-thr",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(10),
	}})
	if res.Accepted != 1 {
		t.Fatalf("ingest during dashboard lockout failed: %+v", res)
	}
}

func TestAuth_DashboardTokenCannotIngestAndIngestTokenRejection(t *testing.T) {
	s := startTestServer(t, serverOpts{
		IngestToken:    "strict-ingest-tok",
		DashboardToken: "strict-dash-tok",
	})
	evBody := []byte(`{"events":[{"schema_version":"1","event_id":"rej-1","timestamp":"2026-11-20T10:00:00Z","machine_id":"m1","provider":"openai","model":"gpt-5.4","tool":"codex","total_tokens":10,"token_accuracy":"reported","source":{"adapter":"codex","adapter_version":"1"}}]}`)
	hbBody := []byte(`{"machine_id":"m1","agent_version":"1.0","operating_system":"darwin","architecture":"arm64","adapters":[],"source_count":0,"source_error_count":0,"sources":[]}`)

	for _, endpoint := range []struct {
		path string
		body []byte
	}{
		{"/api/v1/events/batch", evBody},
		{"/api/v1/agents/heartbeat", hbBody},
	} {
		for _, hdrs := range []map[string]string{
			{"Content-Type": "application/json"},
			{"Content-Type": "application/json", "Authorization": "Bearer strict-dash-tok"},
			{"Content-Type": "application/json", "X-Tokemon-Ingest-Token": "wrong-tok"},
			{"Content-Type": "application/json", "Authorization": "Bearer strict-ingest-tok", "X-Tokemon-Ingest-Token": "wrong-tok"},
		} {
			resp, _ := s.doReq(t, http.MethodPost, endpoint.path, endpoint.body, hdrs)
			if resp.StatusCode != http.StatusUnauthorized {
				t.Fatalf("POST %s with headers %v status = %d, want 401", endpoint.path, hdrs, resp.StatusCode)
			}
		}
	}
}

func TestAuth_IngestRejectsPathInjectionAndInvalidBatch(t *testing.T) {
	s := startTestServer(t, serverOpts{IngestToken: "val-batch-tok"})
	hdrs := map[string]string{
		"Content-Type":  "application/json",
		"Authorization": "Bearer " + s.IngestToken,
	}

	r1, b1 := s.doReq(t, http.MethodPost, "/api/v1/events/batch", []byte(`{"events":[{"schema_version":"1","event_id":"v-1","timestamp":"2026-11-20T10:00:00Z","machine_id":"m1","project":"/Users/secret/repo","provider":"openai","model":"gpt-5.4","tool":"codex","total_tokens":10,"token_accuracy":"reported","source":{"adapter":"codex","adapter_version":"1"}}]}`), hdrs)
	if r1.StatusCode != http.StatusBadRequest || !strings.Contains(string(b1), "event 1 is invalid") {
		t.Fatalf("path injection got %d: %s", r1.StatusCode, string(b1))
	}

	r2, _ := s.doReq(t, http.MethodPost, "/api/v1/events/batch", []byte(`{"events":[{"schema_version":"1","event_id":"v-2","timestamp":"2026-11-20T10:00:00Z","machine_id":"m1","provider":"openai","model":"gpt-5.4","tool":"codex","input_tokens":5,"total_tokens":1,"token_accuracy":"reported","source":{"adapter":"codex","adapter_version":"1"}}]}`), hdrs)
	if r2.StatusCode != http.StatusBadRequest {
		t.Fatalf("total<components got %d, want 400", r2.StatusCode)
	}

	r3, _ := s.doReq(t, http.MethodPost, "/api/v1/events/batch", []byte(`{"events":[]}`), hdrs)
	if r3.StatusCode != http.StatusBadRequest {
		t.Fatalf("empty events got %d, want 400", r3.StatusCode)
	}

	many := make([]ingestEvent, 1001)
	for i := range many {
		many[i] = ingestEvent{
			SchemaVersion: "1",
			EventID:       fmt.Sprintf("too-many-%d", i),
			Timestamp:     "2026-11-20T10:00:00Z",
			MachineID:     "m1",
			Provider:      "openai",
			Model:         "gpt-5.4",
			Tool:          "codex",
			TotalTokens:   i64(1),
			TokenAccuracy: "reported",
			Source:        ingestSource{Adapter: "codex", AdapterVersion: "1"},
		}
	}
	manyBytes, err := json.Marshal(map[string]any{"events": many})
	if err != nil {
		t.Fatalf("marshal 1001 events: %v", err)
	}
	r4, _ := s.doReq(t, http.MethodPost, "/api/v1/events/batch", manyBytes, hdrs)
	if r4.StatusCode != http.StatusBadRequest {
		t.Fatalf("1001 events got %d, want 400", r4.StatusCode)
	}

	r5, _ := s.doReq(t, http.MethodPost, "/api/v1/events/batch", []byte("not-valid-gzip"), map[string]string{
		"Content-Type":     "application/json",
		"Content-Encoding": "gzip",
		"Authorization":    "Bearer " + s.IngestToken,
	})
	if r5.StatusCode != http.StatusBadRequest {
		t.Fatalf("invalid gzip got %d, want 400", r5.StatusCode)
	}

	var ov struct {
		LifetimeTokens int64 `json:"lifetime_tokens"`
	}
	getJSONWithClient(t, s.authClient(t), s.BaseURL+"/api/v1/analytics/overview", &ov)
	if ov.LifetimeTokens != 0 {
		t.Fatalf("DB mutated despite rejected batches: lifetime_tokens=%d", ov.LifetimeTokens)
	}
}

func TestAuth_ForgedAndTamperedSessionCookieRejected(t *testing.T) {
	s := startTestServer(t, serverOpts{
		IngestToken: "forged-ing-tok",
		Username:    "admin",
		Password:    "forged-pass",
	})

	_, loginHTML := s.doReq(t, http.MethodGet, "/login", nil, nil)
	csrf := extractCSRFToken(t, loginHTML)
	form := url.Values{"username": {s.Username}, "password": {s.Password}, "csrf_token": {csrf}, "next": {"/"}}
	r, _ := s.doReq(t, http.MethodPost, "/login", []byte(form.Encode()), map[string]string{
		"Content-Type": "application/x-www-form-urlencoded",
		"Origin":       s.BaseURL,
		"Referer":      s.BaseURL + "/login",
	})
	var realCookie string
	for _, c := range r.Cookies() {
		if c.Name == "tokemon_session" {
			realCookie = c.Value
		}
	}
	if len(realCookie) < 4 {
		t.Fatalf("expected valid session cookie")
	}
	b := []byte(realCookie)
	if b[len(b)-1] == 'a' {
		b[len(b)-1] = 'b'
	} else {
		b[len(b)-1] = 'a'
	}
	tampered := string(b)

	for i := 0; i < 6; i++ {
		resp, _ := s.doReq(t, http.MethodGet, "/analytics", nil, map[string]string{
			"Accept": "text/html",
			"Cookie": "tokemon_session=" + tampered,
		})
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("tampered cookie attempt %d status = %d, want 303", i+1, resp.StatusCode)
		}
		var cleared bool
		for _, c := range resp.Cookies() {
			if c.Name == "tokemon_session" && c.MaxAge <= 0 {
				cleared = true
			}
		}
		if !cleared {
			t.Fatalf("tampered cookie attempt %d did not clear cookie", i+1)
		}
	}

	okResp, _ := s.doReq(t, http.MethodGet, "/api/v1/machines", nil, map[string]string{
		"Authorization": "Bearer " + s.DashboardToken,
	})
	if okResp.StatusCode != http.StatusOK {
		t.Fatalf("Bearer auth after tampered cookies status = %d, want 200 (must not trigger 429)", okResp.StatusCode)
	}
}

func TestAuth_LoginDisabledWithoutPasswordAccount(t *testing.T) {
	s := startTestServer(t, serverOpts{IngestToken: "token-only-tok"})
	for _, tc := range []struct {
		method string
		path   string
	}{
		{http.MethodGet, "/login"},
		{http.MethodPost, "/login"},
		{http.MethodPost, "/logout"},
	} {
		resp, _ := s.doReq(t, tc.method, tc.path, nil, nil)
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("%s %s on token-only server status = %d, want 404", tc.method, tc.path, resp.StatusCode)
		}
	}
}

// --- Area C: Backups, DB Restart Idempotency & SQLite WAL Concurrency ---

func TestBackup_OnlineVACUUMIntoIncludesLiveWALRows(t *testing.T) {
	s := startTestServer(t, serverOpts{IngestToken: "wal-bak-tok"})
	s.ingestEvents(t, []ingestEvent{
		{EventID: "wal-b-1", MachineID: "m1", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(300)},
		{EventID: "wal-b-2", MachineID: "m1", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(450)},
	})

	bakDir := filepath.Join(t.TempDir(), "online-backups")
	out, stderr, err := s.runCLI(t, nil, "backup", "create", "--destination", bakDir, "--database", s.DBPath)
	if err != nil {
		t.Fatalf("online backup create failed: %v\nstderr: %s", err, stderr)
	}
	var res struct {
		Path           string `json:"path"`
		EventCount     int64  `json:"event_count"`
		LifetimeTokens int64  `json:"lifetime_tokens"`
	}
	if err := json.Unmarshal([]byte(out), &res); err != nil {
		t.Fatalf("decode backup create: %v\nraw: %s", err, out)
	}
	if res.EventCount != 2 || res.LifetimeTokens != 750 {
		t.Fatalf("online backup missed WAL rows: %+v", res)
	}
	if !strings.Contains(filepath.Base(res.Path), "-backup-v") || !strings.HasSuffix(res.Path, "Z.db") {
		t.Fatalf("unexpected backup filename %s", res.Path)
	}
	for _, sidecar := range []string{res.Path + "-wal", res.Path + "-shm"} {
		if _, err := os.Stat(sidecar); !os.IsNotExist(err) {
			t.Fatalf("backup should not have sidecar %s", sidecar)
		}
	}
	if _, vErr, err := s.runCLI(t, nil, "backup", "verify", res.Path); err != nil {
		t.Fatalf("verify online backup failed: %v\nstderr: %s", err, vErr)
	}
}

func TestBackup_DailyAutomatedBackupOnServeAndRetention(t *testing.T) {
	requireE2E(t)
	bakDir := filepath.Join(t.TempDir(), "auto-backups")
	s := startTestServer(t, serverOpts{
		IngestToken:     "auto-bak-tok",
		BackupDir:       bakDir,
		BackupInterval:  "250ms",
		BackupRetention: 3,
	})
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "auto-bak-1",
		MachineID:   "m-auto",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(420),
	}})

	deadline := time.Now().Add(5 * time.Second)
	for {
		matches, err := filepath.Glob(filepath.Join(bakDir, "*-backup-v*.db"))
		if err != nil {
			t.Fatalf("glob automated backups: %v", err)
		}
		if len(matches) >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for automated backup in %s", bakDir)
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Allow >= 4 intervals for retention pruning, then stop the server cleanly before inspecting files.
	time.Sleep(1200 * time.Millisecond)
	s.Stop()

	files, err := filepath.Glob(filepath.Join(bakDir, "*-backup-v*.db"))
	if err != nil {
		t.Fatalf("glob automated backups after stop: %v", err)
	}
	if len(files) == 0 || len(files) > 3 {
		t.Fatalf("automated backup retention expected 1..3 files, got %d: %v", len(files), files)
	}
	latest := files[len(files)-1]
	vOut, vErr, err := s.runCLI(t, nil, "backup", "verify", latest)
	if err != nil {
		t.Fatalf("verify automated backup %s failed: %v\nstderr: %s", latest, err, vErr)
	}
	if !strings.Contains(vOut, `"lifetime_tokens":420`) {
		t.Fatalf("automated backup %s missing ingested tokens: %s", latest, vOut)
	}
}

func TestDB_RestartPreservesDataAndSkipsSpuriousMigrationBackup(t *testing.T) {
	requireE2E(t)
	workDir := t.TempDir()
	dbPath := filepath.Join(workDir, "restart.db")
	bakDir := filepath.Join(workDir, "backups")

	s1 := startTestServer(t, serverOpts{
		IngestToken:    "restart-tok",
		DBPath:         dbPath,
		BackupDir:      bakDir,
		BackupInterval: "0",
	})
	ev1 := ingestEvent{
		EventID:      "rst-codex-1",
		MachineID:    "m-rst",
		SessionID:    "s-rst-1",
		Provider:     "openai",
		Model:        "gpt-5.4",
		Tool:         "codex",
		InputTokens:  i64(400),
		OutputTokens: i64(100),
		TotalTokens:  i64(500),
		Source:       ingestSource{Adapter: "codex", AdapterVersion: "1.0"},
	}
	ev2 := ingestEvent{
		EventID:      "rst-claude-1",
		MachineID:    "m-rst",
		SessionID:    "s-rst-2",
		Provider:     "anthropic",
		Model:        "claude-sonnet-4-5",
		Tool:         "claude-code",
		InputTokens:  i64(200),
		OutputTokens: i64(100),
		TotalTokens:  i64(300),
		Source:       ingestSource{Adapter: "claude-code", AdapterVersion: "1.0"},
	}
	s1.ingestEvents(t, []ingestEvent{ev1, ev2})
	now := time.Now().UTC()
	s1.sendHeartbeat(t, "m-rst", "codex", heartbeatSource{
		Adapter:            "codex",
		IdentityHash:       "sha256:" + strings.Repeat("d", 64),
		LastSuccessfulSync: &now,
	})

	// Set machine alias via /settings/aliases.
	client := s1.authClient(t)
	settingsResp, err := client.Get(s1.BaseURL + "/settings")
	if err != nil {
		t.Fatalf("GET /settings: %v", err)
	}
	settingsHTML, readErr := io.ReadAll(settingsResp.Body)
	_ = settingsResp.Body.Close()
	if readErr != nil {
		t.Fatalf("read /settings: %v", readErr)
	}
	if settingsResp.StatusCode != http.StatusOK {
		t.Fatalf("GET /settings status = %d, want 200", settingsResp.StatusCode)
	}
	csrf := extractCSRFToken(t, settingsHTML)

	form := url.Values{
		"csrf_token":       {csrf},
		"machine_identity": {"m-rst"},
		"machine_alias":    {"Studio Mac"},
	}
	req, err := http.NewRequest(http.MethodPost, s1.BaseURL+"/settings/aliases", strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("new POST /settings/aliases request: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", s1.BaseURL)
	aliasResp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST /settings/aliases: %v", err)
	}
	_ = aliasResp.Body.Close()
	if aliasResp.StatusCode != http.StatusOK {
		t.Fatalf("POST /settings/aliases status = %d", aliasResp.StatusCode)
	}
	s1.Stop()

	// Restart cycle 1 & 2.
	for cycle := 1; cycle <= 2; cycle++ {
		sNext := startTestServer(t, serverOpts{
			IngestToken:    "restart-tok",
			DBPath:         dbPath,
			BackupDir:      bakDir,
			BackupInterval: "0",
		})
		c := sNext.authClient(t)
		var ov struct {
			LifetimeTokens int64 `json:"lifetime_tokens"`
		}
		getJSONWithClient(t, c, sNext.BaseURL+"/api/v1/analytics/overview", &ov)
		if ov.LifetimeTokens != 800 {
			t.Fatalf("restart cycle %d lifetime_tokens = %d, want 800", cycle, ov.LifetimeTokens)
		}
		var machines []struct {
			ID string `json:"id"`
		}
		getJSONWithClient(t, c, sNext.BaseURL+"/api/v1/machines", &machines)
		if len(machines) != 1 || machines[0].ID != "m-rst" {
			t.Fatalf("restart cycle %d machines = %+v", cycle, machines)
		}
		var sessions []struct {
			SessionID string `json:"session_id"`
		}
		getJSONWithClient(t, c, sNext.BaseURL+"/api/v1/sessions", &sessions)
		if len(sessions) != 2 {
			t.Fatalf("restart cycle %d sessions len = %d, want 2", cycle, len(sessions))
		}
		sResp, err := c.Get(sNext.BaseURL + "/settings")
		if err != nil {
			t.Fatalf("restart cycle %d GET /settings: %v", cycle, err)
		}
		sBody, readErr := io.ReadAll(sResp.Body)
		_ = sResp.Body.Close()
		if readErr != nil {
			t.Fatalf("restart cycle %d read /settings: %v", cycle, readErr)
		}
		if sResp.StatusCode != http.StatusOK {
			t.Fatalf("restart cycle %d GET /settings status = %d, want 200", cycle, sResp.StatusCode)
		}
		if !strings.Contains(string(sBody), "Studio Mac") {
			t.Fatalf("restart cycle %d lost machine alias Studio Mac", cycle)
		}
		if cycle == 2 {
			dupRes := sNext.ingestEvents(t, []ingestEvent{ev1, ev2})
			if dupRes.Accepted != 0 || dupRes.Duplicates != 2 || dupRes.CurrentTotal != 800 {
				t.Fatalf("re-ingest after restart double-counted: %+v", dupRes)
			}
		}
		sNext.Stop()
	}

	migBackups, err := filepath.Glob(filepath.Join(bakDir, "*-before-v*.db"))
	if err != nil {
		t.Fatalf("glob migration backups: %v", err)
	}
	if len(migBackups) != 0 {
		t.Fatalf("ordinary restarts created unexpected migration backups: %v", migBackups)
	}
}

func TestDB_WALModeAndConcurrentReadsDuringIngest(t *testing.T) {
	s := startTestServer(t, serverOpts{IngestToken: "wal-conc-tok"})
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "wal-init-1",
		MachineID:   "m-wal",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(100),
	}})
	if _, err := os.Stat(s.DBPath + "-wal"); err != nil {
		t.Fatalf("expected -wal sidecar at %s-wal: %v", s.DBPath, err)
	}

	client := s.authClient(t)
	var wg sync.WaitGroup
	errCh := make(chan error, 64)

	const batches = 8
	for b := 0; b < batches; b++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			ev := ingestEvent{
				SchemaVersion: "1",
				EventID:       fmt.Sprintf("wal-burst-%d", idx),
				Timestamp:     time.Now().UTC().Format(time.RFC3339Nano),
				MachineID:     "m-wal",
				SessionID:     fmt.Sprintf("s-wal-%d", idx),
				Provider:      "openai",
				Model:         "gpt-5.4",
				Tool:          "codex",
				TotalTokens:   i64(50),
				TokenAccuracy: "reported",
				Source:        ingestSource{Adapter: "codex", AdapterVersion: "1"},
			}
			payload, err := json.Marshal(map[string]any{"events": []ingestEvent{ev}})
			if err != nil {
				errCh <- fmt.Errorf("marshal ingest %d: %w", idx, err)
				return
			}
			req, err := http.NewRequest(http.MethodPost, s.BaseURL+"/api/v1/events/batch", bytes.NewReader(payload))
			if err != nil {
				errCh <- fmt.Errorf("new ingest request %d: %w", idx, err)
				return
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer "+s.IngestToken)
			resp, err := client.Do(req)
			if err != nil {
				errCh <- fmt.Errorf("ingest %d: %w", idx, err)
				return
			}
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				errCh <- fmt.Errorf("ingest %d status = %d", idx, resp.StatusCode)
			}
		}(b)
	}

	readPaths := []string{"/", "/analytics", "/sessions", "/api/v1/evolution", "/healthz"}
	for r := 0; r < 10; r++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			for _, p := range readPaths {
				resp, err := client.Get(s.BaseURL + p)
				if err != nil {
					errCh <- fmt.Errorf("reader %d %s: %w", idx, p, err)
					return
				}
				_, _ = io.Copy(io.Discard, resp.Body)
				_ = resp.Body.Close()
				if resp.StatusCode != http.StatusOK {
					errCh <- fmt.Errorf("reader %d %s status = %d", idx, p, resp.StatusCode)
				}
			}
		}(r)
	}

	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatalf("concurrent WAL R/W error: %v", err)
	}

	want := int64(100 + batches*50)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, want)
}

func TestBackup_RestoreForceCreatesPreRestoreSnapshot(t *testing.T) {
	s := startTestServer(t, serverOpts{IngestToken: "prerest-tok"})
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "v1-ev",
		MachineID:   "m1",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(111),
	}})
	bakDir := filepath.Join(t.TempDir(), "baks")
	v1Out, v1Err, err := s.runCLI(t, nil, "backup", "create", "--destination", bakDir, "--database", s.DBPath)
	if err != nil {
		t.Fatalf("backup V1 failed: %v\nstderr: %s", err, v1Err)
	}
	var v1Res struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(v1Out), &v1Res); err != nil {
		t.Fatalf("decode v1 backup JSON: %v\nraw: %s", err, v1Out)
	}

	s.ingestEvents(t, []ingestEvent{{
		EventID:     "v2-ev",
		MachineID:   "m1",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(222),
	}})
	dbPath := s.DBPath
	s.Stop()

	rOut, rErr, err := s.runCLI(t, nil, "backup", "restore", "--source", v1Res.Path, "--database", dbPath, "--force")
	if err != nil {
		t.Fatalf("backup restore --force failed: %v\nstderr: %s", err, rErr)
	}
	var rRes struct {
		PreRestoreBackup string `json:"pre_restore_backup"`
		LifetimeTokens   int64  `json:"lifetime_tokens"`
	}
	if err := json.Unmarshal([]byte(rOut), &rRes); err != nil {
		t.Fatalf("decode restore result: %v\nraw: %s", err, rOut)
	}
	if rRes.PreRestoreBackup == "" || rRes.LifetimeTokens != 111 {
		t.Fatalf("unexpected restore result: %+v", rRes)
	}

	preOut, preErr, err := s.runCLI(t, nil, "backup", "verify", rRes.PreRestoreBackup)
	if err != nil {
		t.Fatalf("verify pre-restore backup failed: %v\nstderr: %s", err, preErr)
	}
	if !strings.Contains(preOut, `"lifetime_tokens":333`) {
		t.Fatalf("pre-restore snapshot missing V2 totals (333): %s", preOut)
	}

	s2 := startTestServer(t, serverOpts{IngestToken: "prerest-tok", DBPath: dbPath})
	var ov struct {
		LifetimeTokens int64 `json:"lifetime_tokens"`
	}
	getJSONWithClient(t, s2.authClient(t), s2.BaseURL+"/api/v1/analytics/overview", &ov)
	if ov.LifetimeTokens != 111 {
		t.Fatalf("restored DB lifetime_tokens = %d, want 111", ov.LifetimeTokens)
	}
}

func TestBackup_RestoreRefusedWhileServeHoldsLock(t *testing.T) {
	s := startTestServer(t, serverOpts{IngestToken: "lock-bak-tok"})
	bakDir := filepath.Join(t.TempDir(), "baks")
	out, stderr, err := s.runCLI(t, nil, "backup", "create", "--destination", bakDir, "--database", s.DBPath)
	if err != nil {
		t.Fatalf("online backup failed: %v\nstderr: %s", err, stderr)
	}
	var b struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(out), &b); err != nil {
		t.Fatalf("decode backup JSON: %v\nraw: %s", err, out)
	}

	_, rStderr, rErr := s.runCLI(t, nil, "backup", "restore", "--source", b.Path, "--database", s.DBPath, "--force")
	if rErr == nil {
		t.Fatalf("expected backup restore while serve holds lock to fail")
	}
	if !strings.Contains(rStderr, "already in use") {
		t.Fatalf("expected stderr to contain 'already in use', got: %s", rStderr)
	}
}

func TestBackup_RestoreRefusedWithoutForceWhenDestExists(t *testing.T) {
	s := startTestServer(t, serverOpts{IngestToken: "noforce-tok"})
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "nf-1",
		MachineID:   "m1",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(50),
	}})
	bakDir := filepath.Join(t.TempDir(), "baks")
	out, _, err := s.runCLI(t, nil, "backup", "create", "--destination", bakDir, "--database", s.DBPath)
	if err != nil {
		t.Fatalf("backup create failed: %v", err)
	}
	var b struct {
		Path string `json:"path"`
	}
	if err := json.Unmarshal([]byte(out), &b); err != nil {
		t.Fatalf("decode backup JSON: %v\nraw: %s", err, out)
	}
	dbPath := s.DBPath
	s.Stop()

	_, rStderr, rErr := s.runCLI(t, nil, "backup", "restore", "--source", b.Path, "--database", dbPath)
	if rErr == nil {
		t.Fatalf("expected restore without --force on existing DB to fail")
	}
	if !strings.Contains(rStderr, "pass --force") {
		t.Fatalf("expected stderr to mention 'pass --force', got: %s", rStderr)
	}
}

func TestBackup_VerifyAndRestoreRejectCorruptOrMissingFile(t *testing.T) {
	requireE2E(t)
	homeDir := t.TempDir()
	corrupt := filepath.Join(t.TempDir(), "corrupt.db")
	if err := os.WriteFile(corrupt, []byte("not-a-sqlite-database"), 0o644); err != nil {
		t.Fatalf("write corrupt db: %v", err)
	}

	if _, _, err := runTokemonCLI(t, homeDir, nil, "", "backup", "verify", corrupt); err == nil {
		t.Fatalf("expected backup verify on corrupt file to fail")
	}
	newDest := filepath.Join(t.TempDir(), "new-dest.db")
	if _, _, err := runTokemonCLI(t, homeDir, nil, "", "backup", "restore", "--source", corrupt, "--database", newDest); err == nil {
		t.Fatalf("expected backup restore from corrupt file to fail")
	}
	if _, err := os.Stat(newDest); !os.IsNotExist(err) {
		t.Fatalf("corrupt restore must not create destination %s", newDest)
	}
}

func TestBackup_RetentionPreservesMigrationAndPreRestoreFiles(t *testing.T) {
	s := startTestServer(t, serverOpts{IngestToken: "ret-tok"})
	bakDir := filepath.Join(t.TempDir(), "ret-baks")
	if err := os.MkdirAll(bakDir, 0o755); err != nil {
		t.Fatalf("mkdir bakDir: %v", err)
	}
	base := strings.TrimSuffix(filepath.Base(s.DBPath), filepath.Ext(s.DBPath))
	migFile := filepath.Join(bakDir, base+"-v0-before-v4-20260101T000000.000000000Z.db")
	preFile := filepath.Join(bakDir, base+"-before-restore-v4-20260101T000000.000000000Z.db")
	if err := os.WriteFile(migFile, []byte("keep-mig"), 0o644); err != nil {
		t.Fatalf("write migFile: %v", err)
	}
	if err := os.WriteFile(preFile, []byte("keep-pre"), 0o644); err != nil {
		t.Fatalf("write preFile: %v", err)
	}

	for i := 0; i < 2; i++ {
		if _, stderr, err := s.runCLI(t, nil, "backup", "create", "--destination", bakDir, "--database", s.DBPath, "--retention", "1"); err != nil {
			t.Fatalf("backup create #%d failed: %v\nstderr: %s", i+1, err, stderr)
		}
		time.Sleep(15 * time.Millisecond)
	}

	baks, err := filepath.Glob(filepath.Join(bakDir, base+"-backup-*.db"))
	if err != nil {
		t.Fatalf("glob regular backups: %v", err)
	}
	if len(baks) != 1 {
		t.Fatalf("expected 1 regular backup after retention=1, got %d: %v", len(baks), baks)
	}
	for _, preserved := range []string{migFile, preFile} {
		if _, err := os.Stat(preserved); err != nil {
			t.Fatalf("expected %s to be preserved, got err: %v", preserved, err)
		}
	}
}

func TestBackup_AutomatedBackupDisabledWithZeroInterval(t *testing.T) {
	requireE2E(t)
	bakDir := filepath.Join(t.TempDir(), "no-auto-backups")
	s := startTestServer(t, serverOpts{
		IngestToken:    "no-auto-tok",
		BackupDir:      bakDir,
		BackupInterval: "0",
	})
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "no-auto-1",
		MachineID:   "m-noauto",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(10),
	}})
	time.Sleep(300 * time.Millisecond)
	s.Stop()

	matches, err := filepath.Glob(filepath.Join(bakDir, "*-backup-v*.db"))
	if err != nil {
		t.Fatalf("glob automated backups: %v", err)
	}
	if len(matches) != 0 {
		t.Fatalf("expected 0 automated backups with interval=0, got %v", matches)
	}
}

func TestDB_CodexAggregateSupersededIdempotentlyAcrossRestart(t *testing.T) {
	requireE2E(t)
	workDir := t.TempDir()
	dbPath := filepath.Join(workDir, "codex-dedup.db")

	s1 := startTestServer(t, serverOpts{IngestToken: "cdx-dedup-tok", DBPath: dbPath})
	s1.ingestEvents(t, []ingestEvent{{
		EventID:     "cdx-agg-1",
		MachineID:   "m1",
		SessionID:   "s1",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(1000),
		Source:      ingestSource{Adapter: "codex", AdapterVersion: "1.0"},
	}})
	s1.Stop()

	s2 := startTestServer(t, serverOpts{IngestToken: "cdx-dedup-tok", DBPath: dbPath})
	s2.ingestEvents(t, []ingestEvent{{
		EventID:      "cdx-det-1",
		MachineID:    "m1",
		SessionID:    "s1",
		Provider:     "openai",
		Model:        "gpt-5.4",
		Tool:         "codex",
		InputTokens:  i64(600),
		OutputTokens: i64(500),
		TotalTokens:  i64(1100),
		Source:       ingestSource{Adapter: "codex", AdapterVersion: "1.0"},
	}})
	s2.Stop()

	s3 := startTestServer(t, serverOpts{IngestToken: "cdx-dedup-tok", DBPath: dbPath})
	var ov struct {
		LifetimeTokens int64 `json:"lifetime_tokens"`
	}
	getJSONWithClient(t, s3.authClient(t), s3.BaseURL+"/api/v1/analytics/overview", &ov)
	if ov.LifetimeTokens != 1100 {
		t.Fatalf("Codex aggregate->detailed dedup across restart = %d, want 1100", ov.LifetimeTokens)
	}
}
