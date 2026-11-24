//go:build e2e

package e2e_test

import (
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	tokemonBin string
	repoRoot   string

	passwordHashMu    sync.Mutex
	passwordHashCache = make(map[string]string)
)

func requireE2E(t *testing.T) {
	t.Helper()
	if os.Getenv("TOKEMON_E2E") != "1" {
		t.Skip("TOKEMON_E2E=1 not set")
	}
}

func TestMain(m *testing.M) {
	if os.Getenv("TOKEMON_E2E") != "1" {
		os.Exit(m.Run())
	}

	root, err := findRepoRoot()
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e TestMain: locate repo root: %v\n", err)
		os.Exit(1)
	}
	repoRoot = root

	tmpDir, err := os.MkdirTemp("", "tokemon-e2e-bin-*")
	if err != nil {
		fmt.Fprintf(os.Stderr, "e2e TestMain: create temp dir: %v\n", err)
		os.Exit(1)
	}

	binName := "tokemon"
	if runtime.GOOS == "windows" {
		binName = "tokemon.exe"
	}
	tokemonBin = filepath.Join(tmpDir, binName)

	cmd := exec.Command("go", "build", "-o", tokemonBin, "./cmd/tokemon")
	cmd.Dir = repoRoot
	if out, err := cmd.CombinedOutput(); err != nil {
		fmt.Fprintf(os.Stderr, "e2e TestMain: go build ./cmd/tokemon failed: %v\n%s\n", err, string(out))
		_ = os.RemoveAll(tmpDir)
		os.Exit(1)
	}

	code := m.Run()
	_ = os.RemoveAll(tmpDir)
	os.Exit(code)
}

func findRepoRoot() (string, error) {
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	dir := wd
	for {
		if _, err := os.Stat(filepath.Join(dir, "go.mod")); err == nil {
			return dir, nil
		}
		parent := filepath.Dir(dir)
		if parent == dir {
			return "", fmt.Errorf("go.mod not found above %s", wd)
		}
		dir = parent
	}
}

type serverOpts struct {
	Password        string
	Username        string
	IngestToken     string
	DashboardToken  string
	EnableTokedex   bool
	Timezone        string
	QuotasYAML      string
	CatalogYAML     string
	BackupDir       string
	BackupInterval  string
	BackupRetention int
	AlertWebhookURL string
	ExtraEnv        map[string]string
	DBPath          string
}

type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type testServer struct {
	t              *testing.T
	BaseURL        string
	DBPath         string
	BackupDir      string
	IngestToken    string
	DashboardToken string
	Username       string
	Password       string
	HomeDir        string
	ConfigPath     string
	Client         *http.Client

	cmd     *exec.Cmd
	stdout  syncBuffer
	stderr  syncBuffer
	mu      sync.Mutex
	stopped bool
	waitCh  chan error
}

func isolatedBaseEnv(homeDir string, extra map[string]string) []string {
	merged := make(map[string]string)
	for _, entry := range os.Environ() {
		k, v, ok := strings.Cut(entry, "=")
		if !ok {
			continue
		}
		if strings.HasPrefix(k, "TOKEMON_") || k == "HOME" {
			continue
		}
		merged[k] = v
	}
	if homeDir != "" {
		merged["HOME"] = homeDir
	}
	for k, v := range extra {
		merged[k] = v
	}
	keys := make([]string, 0, len(merged))
	for k := range merged {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]string, 0, len(keys))
	for _, k := range keys {
		out = append(out, k+"="+merged[k])
	}
	return out
}

func runTokemonCLI(t *testing.T, homeDir string, env map[string]string, stdin string, args ...string) (string, string, error) {
	t.Helper()
	requireE2E(t)
	if tokemonBin == "" {
		t.Fatalf("tokemon binary not built")
	}
	cmd := exec.Command(tokemonBin, args...)
	cmd.Env = isolatedBaseEnv(homeDir, env)
	if stdin != "" {
		cmd.Stdin = strings.NewReader(stdin)
	}
	var stdout, stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	return stdout.String(), stderr.String(), err
}

func hashPasswordForTest(t *testing.T, homeDir, password string) string {
	t.Helper()
	passwordHashMu.Lock()
	if h, ok := passwordHashCache[password]; ok {
		passwordHashMu.Unlock()
		return h
	}
	passwordHashMu.Unlock()

	out, stderr, err := runTokemonCLI(t, homeDir, nil, password+"\n", "passwd", "--stdin")
	if err != nil {
		t.Fatalf("tokemon passwd --stdin failed: %v\nstderr: %s", err, stderr)
	}
	hash := strings.TrimSpace(out)
	if !strings.HasPrefix(hash, "$2") {
		t.Fatalf("unexpected bcrypt hash output %q", hash)
	}
	passwordHashMu.Lock()
	passwordHashCache[password] = hash
	passwordHashMu.Unlock()
	return hash
}

func freeLoopbackAddr(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("probe free loopback port: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close()
	return addr
}

func startTestServer(t *testing.T, opts serverOpts) *testServer {
	t.Helper()
	requireE2E(t)

	workDir := t.TempDir()
	homeDir := filepath.Join(workDir, "home")
	if err := os.MkdirAll(homeDir, 0o755); err != nil {
		t.Fatalf("create isolated HOME: %v", err)
	}

	dbPath := opts.DBPath
	if dbPath == "" {
		dbPath = filepath.Join(workDir, "tokemon.db")
	}
	backupDir := opts.BackupDir
	if backupDir == "" {
		backupDir = filepath.Join(filepath.Dir(dbPath), "backups")
	}

	addr := freeLoopbackAddr(t)
	baseURL := "http://" + addr

	username := opts.Username
	var passwordHash string
	if opts.Password != "" {
		if username == "" {
			username = "admin"
		}
		passwordHash = hashPasswordForTest(t, homeDir, opts.Password)
	}

	catalogPath := filepath.Join(repoRoot, "catalog", "models.yaml")
	if opts.CatalogYAML != "" {
		catalogPath = filepath.Join(workDir, "models.yaml")
		if err := os.WriteFile(catalogPath, []byte(opts.CatalogYAML), 0o600); err != nil {
			t.Fatalf("write catalog YAML: %v", err)
		}
	}

	var quotasPath string
	if opts.QuotasYAML != "" {
		quotasPath = filepath.Join(workDir, "quotas.yaml")
		if err := os.WriteFile(quotasPath, []byte(opts.QuotasYAML), 0o600); err != nil {
			t.Fatalf("write quotas YAML: %v", err)
		}
	}

	tz := opts.Timezone
	if tz == "" {
		tz = "UTC"
	}

	configPath := filepath.Join(workDir, "server.env")
	if err := os.WriteFile(configPath, []byte("# isolated e2e server.env\n"), 0o600); err != nil {
		t.Fatalf("write empty server.env: %v", err)
	}

	envMap := map[string]string{
		"TOKEMON_SERVER_ADDR":        addr,
		"TOKEMON_DATABASE":           dbPath,
		"TOKEMON_MODEL_CATALOG":      catalogPath,
		"TOKEMON_ANALYTICS_TIMEZONE": tz,
		"TOKEMON_BACKUP_DIR":         backupDir,
	}
	if opts.IngestToken != "" {
		envMap["TOKEMON_INGEST_TOKEN"] = opts.IngestToken
	}
	effectiveDashToken := opts.DashboardToken
	if opts.DashboardToken != "" {
		envMap["TOKEMON_DASHBOARD_TOKEN"] = opts.DashboardToken
	} else if opts.IngestToken != "" {
		effectiveDashToken = opts.IngestToken
	}
	if passwordHash != "" {
		envMap["TOKEMON_DASHBOARD_USERNAME"] = username
		envMap["TOKEMON_DASHBOARD_PASSWORD_HASH"] = passwordHash
	}
	if opts.EnableTokedex {
		envMap["TOKEMON_ENABLE_TOKEDEX"] = "true"
	}
	if quotasPath != "" {
		envMap["TOKEMON_QUOTAS"] = quotasPath
	}
	if opts.BackupInterval != "" {
		envMap["TOKEMON_BACKUP_INTERVAL"] = opts.BackupInterval
	}
	if opts.BackupRetention > 0 {
		envMap["TOKEMON_BACKUP_RETENTION"] = strconv.Itoa(opts.BackupRetention)
	}
	if opts.AlertWebhookURL != "" {
		envMap["TOKEMON_ALERT_WEBHOOK_URL"] = opts.AlertWebhookURL
	}
	for k, v := range opts.ExtraEnv {
		envMap[k] = v
		if k == "TOKEMON_DASHBOARD_TOKEN" {
			effectiveDashToken = v
		}
	}

	args := []string{"serve", "--config", configPath, "--addr", addr}
	if opts.IngestToken == "" {
		args = append(args, "--dev-loopback")
	}
	if quotasPath != "" {
		args = append(args, "--quotas", quotasPath)
	}

	cmd := exec.Command(tokemonBin, args...)
	cmd.Env = isolatedBaseEnv(homeDir, envMap)

	s := &testServer{
		t:              t,
		BaseURL:        baseURL,
		DBPath:         dbPath,
		BackupDir:      backupDir,
		IngestToken:    opts.IngestToken,
		DashboardToken: effectiveDashToken,
		Username:       username,
		Password:       opts.Password,
		HomeDir:        homeDir,
		ConfigPath:     configPath,
		Client: &http.Client{
			Timeout: 10 * time.Second,
			CheckRedirect: func(*http.Request, []*http.Request) error {
				return http.ErrUseLastResponse
			},
		},
		cmd:    cmd,
		waitCh: make(chan error, 1),
	}
	cmd.Stdout = &s.stdout
	cmd.Stderr = &s.stderr

	if err := cmd.Start(); err != nil {
		t.Fatalf("start tokemon serve: %v", err)
	}
	go func() {
		s.waitCh <- cmd.Wait()
	}()

	t.Cleanup(func() {
		s.Stop()
		if t.Failed() {
			t.Logf("tokemon serve stdout:\n%s\ntokemon serve stderr:\n%s", s.stdout.String(), s.stderr.String())
		}
	})

	deadline := time.Now().Add(10 * time.Second)
	for {
		select {
		case err := <-s.waitCh:
			s.mu.Lock()
			s.stopped = true
			s.mu.Unlock()
			t.Fatalf("tokemon serve exited prematurely (%v)\nstdout:\n%s\nstderr:\n%s", err, s.stdout.String(), s.stderr.String())
		default:
		}

		resp, err := s.Client.Get(s.BaseURL + "/healthz")
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return s
			}
		}
		if time.Now().After(deadline) {
			s.Stop()
			t.Fatalf("timed out waiting for %s/healthz == 200\nstdout:\n%s\nstderr:\n%s", s.BaseURL, s.stdout.String(), s.stderr.String())
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func (s *testServer) Stop() {
	s.mu.Lock()
	if s.stopped || s.cmd == nil || s.cmd.Process == nil {
		s.mu.Unlock()
		return
	}
	s.stopped = true
	s.mu.Unlock()

	_ = s.cmd.Process.Signal(os.Interrupt)
	select {
	case <-s.waitCh:
		return
	case <-time.After(15 * time.Second):
		_ = s.cmd.Process.Kill()
		<-s.waitCh
	}
}

func (s *testServer) doReq(t *testing.T, method, path string, body []byte, headers map[string]string) (*http.Response, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, s.BaseURL+path, reader)
	if err != nil {
		t.Fatalf("new request %s %s: %v", method, path, err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := s.Client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, path, err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body %s %s: %v", method, path, err)
	}
	return resp, data
}

type bearerRoundTripper struct {
	token string
	base  http.RoundTripper
}

func (rt *bearerRoundTripper) RoundTrip(req *http.Request) (*http.Response, error) {
	if rt.token != "" && req.Header.Get("Authorization") == "" {
		clone := req.Clone(req.Context())
		clone.Header.Set("Authorization", "Bearer "+rt.token)
		req = clone
	}
	return rt.base.RoundTrip(req)
}

var csrfInputRe = regexp.MustCompile(`name="csrf_token"\s+value="([^"]+)"`)

func extractCSRFToken(t *testing.T, htmlBody []byte) string {
	t.Helper()
	m := csrfInputRe.FindSubmatch(htmlBody)
	if len(m) < 2 {
		t.Fatalf("csrf_token hidden input not found in HTML (%d bytes)", len(htmlBody))
	}
	return string(m[1])
}

func (s *testServer) authClient(t *testing.T) *http.Client {
	t.Helper()
	if s.Password != "" && s.Username != "" {
		jar, err := cookiejar.New(nil)
		if err != nil {
			t.Fatalf("cookiejar.New: %v", err)
		}
		client := &http.Client{
			Jar:     jar,
			Timeout: 10 * time.Second,
		}
		resp, err := client.Get(s.BaseURL + "/login")
		if err != nil {
			t.Fatalf("GET /login: %v", err)
		}
		body, err := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if err != nil {
			t.Fatalf("read /login body: %v", err)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /login status = %d, want 200", resp.StatusCode)
		}
		csrf := extractCSRFToken(t, body)

		form := url.Values{}
		form.Set("username", s.Username)
		form.Set("password", s.Password)
		form.Set("csrf_token", csrf)
		form.Set("next", "/")

		req, err := http.NewRequest(http.MethodPost, s.BaseURL+"/login", strings.NewReader(form.Encode()))
		if err != nil {
			t.Fatalf("new POST /login: %v", err)
		}
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("Origin", s.BaseURL)
		req.Header.Set("Referer", s.BaseURL+"/login")
		loginResp, err := client.Do(req)
		if err != nil {
			t.Fatalf("POST /login: %v", err)
		}
		_, _ = io.Copy(io.Discard, loginResp.Body)
		_ = loginResp.Body.Close()
		if loginResp.StatusCode != http.StatusOK {
			t.Fatalf("POST /login final status = %d, want 200", loginResp.StatusCode)
		}
		u, err := url.Parse(s.BaseURL)
		if err != nil {
			t.Fatalf("parse server BaseURL %q: %v", s.BaseURL, err)
		}
		var hasSession bool
		for _, c := range jar.Cookies(u) {
			if c.Name == "tokemon_session" && c.Value != "" {
				hasSession = true
			}
		}
		if !hasSession {
			t.Fatalf("POST /login did not set tokemon_session cookie")
		}
		return client
	}

	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &bearerRoundTripper{
			token: s.DashboardToken,
			base:  http.DefaultTransport,
		},
	}
}

type ingestSource struct {
	Adapter        string `json:"adapter"`
	AdapterVersion string `json:"adapter_version"`
	Identity       string `json:"identity,omitempty"`
	Offset         *int64 `json:"offset,omitempty"`
}

type ingestEvent struct {
	SchemaVersion    string            `json:"schema_version"`
	EventID          string            `json:"event_id"`
	Timestamp        string            `json:"timestamp"`
	MachineID        string            `json:"machine_id"`
	SessionID        string            `json:"session_id,omitempty"`
	Project          string            `json:"project,omitempty"`
	Provider         string            `json:"provider"`
	Model            string            `json:"model"`
	CanonicalModel   string            `json:"canonical_model,omitempty"`
	Tool             string            `json:"tool"`
	InputTokens      *int64            `json:"input_tokens,omitempty"`
	OutputTokens     *int64            `json:"output_tokens,omitempty"`
	CacheReadTokens  *int64            `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens *int64            `json:"cache_write_tokens,omitempty"`
	ReasoningTokens  *int64            `json:"reasoning_tokens,omitempty"`
	TotalTokens      *int64            `json:"total_tokens,omitempty"`
	DurationMS       *int64            `json:"duration_ms,omitempty"`
	Cost             *float64          `json:"cost,omitempty"`
	CostEstimated    bool              `json:"cost_estimated,omitempty"`
	Currency         string            `json:"currency,omitempty"`
	TokenAccuracy    string            `json:"token_accuracy"`
	Source           ingestSource      `json:"source"`
	Metadata         map[string]string `json:"metadata,omitempty"`
}

type ingestResult struct {
	Accepted      int      `json:"accepted"`
	Duplicates    int      `json:"duplicates"`
	Rejected      int      `json:"rejected"`
	Errors        []string `json:"errors,omitempty"`
	PreviousStage int      `json:"previous_stage"`
	CurrentStage  int      `json:"current_stage"`
	Evolved       bool     `json:"evolved"`
	PreviousTotal int64    `json:"previous_lifetime_tokens"`
	CurrentTotal  int64    `json:"current_lifetime_tokens"`
}

func i64(v int64) *int64 { return &v }

func f64(v float64) *float64 { return &v }

func (s *testServer) ingestEvents(t *testing.T, events []ingestEvent) ingestResult {
	t.Helper()
	normalized := make([]ingestEvent, len(events))
	copy(normalized, events)
	nowStr := time.Now().UTC().Format(time.RFC3339Nano)
	for i := range normalized {
		if normalized[i].SchemaVersion == "" {
			normalized[i].SchemaVersion = "1"
		}
		if normalized[i].Timestamp == "" {
			normalized[i].Timestamp = nowStr
		}
		if normalized[i].TokenAccuracy == "" {
			normalized[i].TokenAccuracy = "reported"
		}
		if normalized[i].Source.Adapter == "" {
			normalized[i].Source.Adapter = "test"
		}
		if normalized[i].Source.AdapterVersion == "" {
			normalized[i].Source.AdapterVersion = "1.0"
		}
	}
	payload, err := json.Marshal(map[string]any{"events": normalized})
	if err != nil {
		t.Fatalf("marshal ingest batch: %v", err)
	}
	headers := map[string]string{"Content-Type": "application/json"}
	if s.IngestToken != "" {
		headers["Authorization"] = "Bearer " + s.IngestToken
	}
	resp, body := s.doReq(t, http.MethodPost, "/api/v1/events/batch", payload, headers)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/v1/events/batch status = %d: %s", resp.StatusCode, string(body))
	}
	var out ingestResult
	if err := json.Unmarshal(body, &out); err != nil {
		t.Fatalf("decode ingestResult: %v (%s)", err, string(body))
	}
	return out
}

type heartbeatSource struct {
	Adapter            string     `json:"adapter"`
	IdentityHash       string     `json:"identity_hash"`
	LastSuccessfulSync *time.Time `json:"last_successful_sync"`
	LastErrorClass     string     `json:"last_error_class"`
}

func (s *testServer) sendHeartbeat(t *testing.T, machineID, adapter string, sources ...heartbeatSource) {
	t.Helper()
	if sources == nil {
		sources = []heartbeatSource{}
	}
	errCount := 0
	for _, src := range sources {
		if src.LastErrorClass != "" {
			errCount++
		}
	}
	reqBody := map[string]any{
		"machine_id":       machineID,
		"agent_version":    "e2e-1.0",
		"operating_system": runtime.GOOS,
		"architecture":     runtime.GOARCH,
		"adapters": []map[string]string{
			{"id": adapter, "version": "1.0"},
		},
		"source_count":       len(sources),
		"source_error_count": errCount,
		"sources":            sources,
	}
	payload, err := json.Marshal(reqBody)
	if err != nil {
		t.Fatalf("marshal heartbeat: %v", err)
	}
	headers := map[string]string{"Content-Type": "application/json"}
	if s.IngestToken != "" {
		headers["Authorization"] = "Bearer " + s.IngestToken
	}
	resp, body := s.doReq(t, http.MethodPost, "/api/v1/agents/heartbeat", payload, headers)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("POST /api/v1/agents/heartbeat status = %d: %s", resp.StatusCode, string(body))
	}
}

func (s *testServer) runCLI(t *testing.T, env map[string]string, args ...string) (string, string, error) {
	t.Helper()
	return runTokemonCLI(t, s.HomeDir, env, "", args...)
}

// writeRawZstdFrame writes a valid RFC 8878 Zstandard frame using raw
// uncompressed blocks (Block_Type = 0) and single-segment 4-byte LE
// Frame_Content_Size (FHD = 0xA0) with stdlib only.
func writeRawZstdFrame(t *testing.T, path string, payload []byte) {
	t.Helper()
	const maxZstdBlock = 128 * 1024

	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}

	var out bytes.Buffer
	out.Write([]byte{0x28, 0xB5, 0x2F, 0xFD})
	out.WriteByte(0xA0)

	var fcs [4]byte
	binary.LittleEndian.PutUint32(fcs[:], uint32(len(payload)))
	out.Write(fcs[:])

	if len(payload) == 0 {
		out.Write([]byte{0x01, 0x00, 0x00})
	} else {
		for off := 0; off < len(payload); off += maxZstdBlock {
			end := off + maxZstdBlock
			if end > len(payload) {
				end = len(payload)
			}
			chunk := payload[off:end]
			var last uint32
			if end == len(payload) {
				last = 1
			}
			hdr := (uint32(len(chunk)) << 3) | last
			out.Write([]byte{byte(hdr), byte(hdr >> 8), byte(hdr >> 16)})
			out.Write(chunk)
		}
	}

	if err := os.WriteFile(path, out.Bytes(), 0o644); err != nil {
		t.Fatalf("write zstd frame %s: %v", path, err)
	}
}

func waitOverviewLifetimeTokens(t *testing.T, client *http.Client, baseURL string, want int64) {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	var last int64 = -1
	for {
		resp, err := client.Get(baseURL + "/api/v1/analytics/overview")
		if err != nil {
			t.Fatalf("GET /api/v1/analytics/overview: %v", err)
		}
		body, readErr := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		if readErr != nil {
			t.Fatalf("read /api/v1/analytics/overview: %v", readErr)
		}
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("GET /api/v1/analytics/overview status = %d: %s", resp.StatusCode, string(body))
		}
		var ov struct {
			LifetimeTokens int64 `json:"lifetime_tokens"`
		}
		if err := json.Unmarshal(body, &ov); err != nil {
			t.Fatalf("decode overview: %v", err)
		}
		last = ov.LifetimeTokens
		if ov.LifetimeTokens == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("overview lifetime_tokens = %d, want %d (timed out waiting for SWR refresh)", last, want)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func TestHarness_HealthzAndBuildSmoke(t *testing.T) {
	s := startTestServer(t, serverOpts{IngestToken: "harness-ing-tok"})
	resp, body := s.doReq(t, http.MethodGet, "/healthz", nil, nil)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz status = %d, want 200", resp.StatusCode)
	}
	if !strings.Contains(string(body), `"status":"ok"`) {
		t.Fatalf("/healthz body = %s, want status ok", string(body))
	}
}

func TestHarness_IngestAndHeartbeatSmoke(t *testing.T) {
	s := startTestServer(t, serverOpts{
		IngestToken: "harness-ing-tok",
		Username:    "admin",
		Password:    "harness-pass-123",
	})
	now := time.Now().UTC()
	res := s.ingestEvents(t, []ingestEvent{{
		EventID:      "harness-smoke-1",
		Timestamp:    now.Format(time.RFC3339Nano),
		MachineID:    "m-smoke",
		SessionID:    "sess-smoke",
		Project:      "proj-smoke",
		Provider:     "openai",
		Model:        "gpt-5.4",
		Tool:         "codex",
		InputTokens:  i64(10),
		OutputTokens: i64(5),
		TotalTokens:  i64(15),
	}})
	if res.Accepted != 1 || res.CurrentTotal != 15 {
		t.Fatalf("unexpected ingest result: %+v", res)
	}
	s.sendHeartbeat(t, "m-smoke", "codex", heartbeatSource{
		Adapter:            "codex",
		IdentityHash:       "sha256:" + strings.Repeat("a", 64),
		LastSuccessfulSync: &now,
	})

	client := s.authClient(t)
	resp, err := client.Get(s.BaseURL + "/api/v1/machines")
	if err != nil {
		t.Fatalf("GET /api/v1/machines: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /api/v1/machines status = %d", resp.StatusCode)
	}
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read /api/v1/machines: %v", err)
	}
	if !strings.Contains(string(raw), `"id":"m-smoke"`) {
		t.Fatalf("machines JSON missing m-smoke: %s", string(raw))
	}
}

func TestHarness_WriteRawZstdFrameAndCLI(t *testing.T) {
	s := startTestServer(t, serverOpts{IngestToken: "harness-cli-tok"})
	zstPath := filepath.Join(t.TempDir(), "test.jsonl.zst")
	payload := append([]byte(`{"line":1}`+"\n"), bytes.Repeat([]byte("x"), 140*1024)...)
	writeRawZstdFrame(t, zstPath, payload)
	data, err := os.ReadFile(zstPath)
	if err != nil {
		t.Fatalf("read zst frame: %v", err)
	}
	if len(data) < 9 || !bytes.Equal(data[:4], []byte{0x28, 0xB5, 0x2F, 0xFD}) {
		t.Fatalf("invalid zstd magic in %s", zstPath)
	}

	stdout, stderr, err := s.runCLI(t, nil, "version")
	if err != nil {
		t.Fatalf("tokemon version failed: %v (stderr: %s)", err, stderr)
	}
	if strings.TrimSpace(stdout) == "" {
		t.Fatalf("tokemon version output was empty")
	}
}
