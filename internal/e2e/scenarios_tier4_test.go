//go:build e2e

package e2e_test

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	stdhtml "html"
	"io"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

var (
	s4BaseCSSRe  = regexp.MustCompile(`/static/tokemon/base\.css\?v=[^"'\s>]+`)
	s4StagePNGRe = regexp.MustCompile(`/static/tokemon/stage-\d+\.png\?v=[^"'\s>]+`)
	s4FontRe     = regexp.MustCompile(`/static/tokemon/fonts/pixelify-sans-latin\.woff2\?v=[^"'\s>]+`)
	s4TokenDexRe = regexp.MustCompile(`/static/tokemon/token-dex\.png\?v=[^"'\s>]+`)
	s4PreloadRe  = regexp.MustCompile(`(?i)<link\b[^>]*\brel="preload"[^>]*>`)
	s4CreatureRe = regexp.MustCompile(`<img\b[^>]*class="[^"]*\bcreature-art\b[^"]*"[^>]*>`)
	s4WidthRe    = regexp.MustCompile(`\bwidth="\d+"`)
	s4HeightRe   = regexp.MustCompile(`\bheight="\d+"`)
)

func s4LoadLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("LoadLocation(%q): %v", name, err)
	}
	return loc
}

func s4Do(t *testing.T, client *http.Client, method, fullURL string, body []byte, headers map[string]string) (int, http.Header, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, fullURL, reader)
	if err != nil {
		t.Fatalf("new %s %s: %v", method, fullURL, err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, fullURL, err)
	}
	data, readErr := io.ReadAll(resp.Body)
	closeErr := resp.Body.Close()
	if readErr != nil {
		t.Fatalf("read %s %s: %v", method, fullURL, readErr)
	}
	if closeErr != nil {
		t.Fatalf("close %s %s: %v", method, fullURL, closeErr)
	}
	return resp.StatusCode, resp.Header, data
}

func s4GetHTML(t *testing.T, client *http.Client, fullURL string) string {
	t.Helper()
	code, _, body := s4Do(t, client, http.MethodGet, fullURL, nil, map[string]string{"Accept": "text/html"})
	if code != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200; body=%s", fullURL, code, string(body))
	}
	return string(body)
}

func s4WaitHTMLContains(t *testing.T, client *http.Client, fullURL, needle string) string {
	t.Helper()
	return s4WaitHTMLContainsAny(t, client, fullURL, needle)
}

func s4WaitHTMLContainsAny(t *testing.T, client *http.Client, fullURL string, needles ...string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last string
	for {
		last = s4GetHTML(t, client, fullURL)
		for _, needle := range needles {
			if strings.Contains(last, needle) {
				return last
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s HTML to contain any of %q", fullURL, needles)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func s4WaitAnalyticsTokens(t *testing.T, client *http.Client, fullURL string, wantTokens int64) analyticsAPIPayload {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last int64 = -1
	for {
		var payload analyticsAPIPayload
		getJSONWithClient(t, client, fullURL, &payload)
		last = payload.Summary.Tokens
		if last == wantTokens {
			return payload
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s summary.tokens = %d, want %d (SWR timeout)", fullURL, last, wantTokens)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func s4WaitQuotas(t *testing.T, client *http.Client, baseURL string, pred func([]ssQuotaRow) bool) []ssQuotaRow {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last []ssQuotaRow
	for {
		last = nil
		getJSONWithClient(t, client, baseURL+"/api/v1/quotas", &last)
		if pred(last) {
			return last
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for /api/v1/quotas condition; last=%+v", last)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func s4SectionBetween(t *testing.T, html, startMarker, endMarker string) string {
	t.Helper()
	start := strings.Index(html, startMarker)
	if start < 0 {
		t.Fatalf("section start marker %q not found", startMarker)
	}
	rest := html[start:]
	if endMarker == "" {
		return rest
	}
	end := strings.Index(rest, endMarker)
	if end < 0 {
		return rest
	}
	return rest[:end]
}

func s4ExtractHrefs(fragment string) []string {
	matches := overviewHrefRe.FindAllStringSubmatch(fragment, -1)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		if len(m) >= 2 {
			out = append(out, stdhtml.UnescapeString(m[1]))
		}
	}
	return out
}

func s4PostAliases(t *testing.T, s *testServer, client *http.Client, values url.Values) (int, http.Header, []byte) {
	t.Helper()
	if !values.Has("csrf_token") {
		settingsHTML := s4GetHTML(t, client, s.BaseURL+"/settings")
		values.Set("csrf_token", extractCSRFToken(t, []byte(settingsHTML)))
	}
	return s4Do(t, ssNoRedirectClient(client), http.MethodPost, s.BaseURL+"/settings/aliases", []byte(values.Encode()), map[string]string{
		"Content-Type": "application/x-www-form-urlencoded",
		"Origin":       s.BaseURL,
		"Referer":      s.BaseURL + "/settings",
	})
}

func s4CodexRolloutLines(sessionID, cwd, model string, ts time.Time, input, cached, output, total int64) string {
	return strings.Join([]string{
		fmt.Sprintf(`{"timestamp":%q,"type":"session_meta","payload":{"id":%q,"model_provider":"openai","cwd":%q,"base_instructions":"SECRET_SCENARIO_INSTR"}}`, ts.UTC().Format(time.RFC3339), sessionID, cwd),
		fmt.Sprintf(`{"timestamp":%q,"type":"turn_context","payload":{"model":%q,"cwd":%q}}`, ts.Add(time.Second).UTC().Format(time.RFC3339), model, cwd),
		fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":%d,"cached_input_tokens":%d,"output_tokens":%d,"reasoning_output_tokens":0,"total_tokens":%d},"total_token_usage":{"input_tokens":%d,"cached_input_tokens":%d,"output_tokens":%d,"reasoning_output_tokens":0,"total_tokens":%d}}}}`,
			ts.Add(2*time.Second).UTC().Format(time.RFC3339), input, cached, output, total, input, cached, output, total),
	}, "\n") + "\n"
}

func s4WriteClaudeFile(t *testing.T, path, sessionID, cwd, entrypoint, model string, ts time.Time, input, output, cacheRead, cacheWrite int64) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	epField := ""
	if entrypoint != "" {
		epField = fmt.Sprintf(`,"entrypoint":%q`, entrypoint)
	}
	content := strings.Join([]string{
		fmt.Sprintf(`{"type":"user","timestamp":%q,"sessionId":%q,"cwd":%q,"slug":"SECRET_SCENARIO_TITLE"%s,"message":{"role":"user","content":"SECRET_SCENARIO_PROMPT"}}`, ts.UTC().Format(time.RFC3339), sessionID, cwd, epField),
		fmt.Sprintf(`{"type":"assistant","timestamp":%q,"sessionId":%q,"cwd":%q,"slug":"SECRET_SCENARIO_TITLE"%s,"message":{"model":%q,"content":"SECRET_SCENARIO_RESPONSE","usage":{"input_tokens":%d,"output_tokens":%d,"cache_read_input_tokens":%d,"cache_creation_input_tokens":%d}}}`,
			ts.Add(time.Second).UTC().Format(time.RFC3339), sessionID, cwd, epField, model, input, output, cacheRead, cacheWrite),
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

// =============================================================================
// Scenario 1: Multi-Machine Daily Coding Workflow
// =============================================================================

func TestScenario_01_MultiMachineDailyCodingWorkflow(t *testing.T) {
	requireE2E(t)
	torLoc := s4LoadLoc(t, "America/Toronto")
	s := startTestServer(t, serverOpts{IngestToken: "s4-sc1-tok", Timezone: "America/Toronto"})
	client := s.authClient(t)

	// Step 1: Verify empty /healthz and Stage 0 Egg overview.
	hCode, _, _ := s4Do(t, s.Client, http.MethodGet, s.BaseURL+"/healthz", nil, nil)
	if hCode != http.StatusOK {
		t.Fatalf("/healthz status = %d", hCode)
	}
	var initOv overviewPayload
	getJSONWithClient(t, client, s.BaseURL+"/api/v1/analytics/overview", &initOv)
	if initOv.LifetimeTokens != 0 {
		t.Fatalf("initial lifetime_tokens = %d, want 0", initOv.LifetimeTokens)
	}

	nowTor := time.Now().In(torLoc)
	torToday := time.Date(nowTor.Year(), nowTor.Month(), nowTor.Day(), 0, 0, 0, 0, torLoc)
	d2TS := torToday.AddDate(0, 0, -2).Add(14 * time.Hour)
	d1CodexTS := torToday.AddDate(0, 0, -1).Add(10 * time.Hour)
	d1CLITS := torToday.AddDate(0, 0, -1).Add(16 * time.Hour)
	todayTS := nowTor.Add(-20 * time.Minute).Truncate(time.Second)
	stepOffset := time.Minute
	if todayTS.In(torLoc).Format("2006-01-02") != torToday.Format("2006-01-02") {
		todayTS = torToday
		stepOffset = 0
	}

	// Step 2: Machine 1 (laptop-air) D-2 archived .jsonl.zst + D-1 active .jsonl + D-1 Claude CLI.
	homeLaptop := filepath.Join(t.TempDir(), "laptop-air")
	stateLaptop := filepath.Join(t.TempDir(), "laptop-state.db")

	zstD2 := s4CodexRolloutLines("s4-sc1-d2", "/Users/dev/myapp", "gpt-5.4", d2TS, 800, 200, 200, 1000)
	writeRawZstdFrame(t, filepath.Join(homeLaptop, ".codex", "archived_sessions", "day-minus2.jsonl.zst"), []byte(zstD2))

	d1ActivePath := filepath.Join(homeLaptop, ".codex", "sessions", "2026", "11", "23", "day-minus1.jsonl")
	d1ActivePayload := s4CodexRolloutLines("s4-sc1-d1", "/Users/dev/myapp", "gpt-5.4", d1CodexTS, 2000, 500, 500, 2500)
	if err := os.MkdirAll(filepath.Dir(d1ActivePath), 0o755); err != nil {
		t.Fatalf("mkdir active dir: %v", err)
	}
	if err := os.WriteFile(d1ActivePath, []byte(d1ActivePayload), 0o644); err != nil {
		t.Fatalf("write active d1: %v", err)
	}
	s4WriteClaudeFile(t, filepath.Join(homeLaptop, ".claude", "projects", "-Users-dev-myapp", "cli-d1.jsonl"), "s4-sc1-cli-d1", "/Users/dev/myapp", "cli", "claude-sonnet-4-5", d1CLITS, 400, 100, 0, 0)

	if _, stderr, err := runAgentWithArgs(t, s, homeLaptop, stateLaptop, "laptop-air", "codex,claude-code"); err != nil {
		t.Fatalf("laptop-air agent --once failed: %v\nstderr: %s", err, stderr)
	}
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 4000)

	// Step 3: Archive laptop-air D-1 rollout to .jsonl.zst (dedup check) & run studio-mac burst (>8 sessions).
	writeRawZstdFrame(t, filepath.Join(homeLaptop, ".codex", "archived_sessions", "day-minus1.jsonl.zst"), []byte(d1ActivePayload))
	if err := os.Remove(d1ActivePath); err != nil {
		t.Fatalf("remove active d1: %v", err)
	}
	if _, stderr, err := runAgentWithArgs(t, s, homeLaptop, stateLaptop, "laptop-air", "codex,claude-code"); err != nil {
		t.Fatalf("laptop-air archive dedup agent --once failed: %v\nstderr: %s", err, stderr)
	}

	homeStudio := filepath.Join(t.TempDir(), "studio-mac")
	stateStudio := filepath.Join(t.TempDir(), "studio-state.db")
	s4WriteClaudeFile(t, filepath.Join(homeStudio, ".claude", "projects", "-Users-dev-myapp", "desktop-today.jsonl"), "s4-sc1-desk-today", "/Users/dev/myapp", "claude-desktop", "claude-sonnet-4-5", todayTS, 600, 100, 200, 0)
	for i := 0; i < 7; i++ {
		s4WriteClaudeFile(t, filepath.Join(homeStudio, ".claude", "projects", "-Users-dev-myapp", fmt.Sprintf("extra-%d.jsonl", i)), fmt.Sprintf("s4-sc1-extra-%d", i), "/Users/dev/myapp", "cli", "claude-sonnet-4-5", todayTS.Add(time.Duration(i+1)*stepOffset), 80, 20, 0, 0)
	}
	if _, stderr, err := runAgentWithArgs(t, s, homeStudio, stateStudio, "studio-mac", "codex,claude-code"); err != nil {
		t.Fatalf("studio-mac agent --once failed: %v\nstderr: %s", err, stderr)
	}
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 5600)

	// Step 4: Verify / overview dashboard, composition, LIMIT 8, health grouping, streaks, and ETag live refresh.
	ovHTML := s4WaitHTMLContains(t, client, s.BaseURL+"/", "studio-mac")
	comp := s4SectionBetween(t, ovHTML, `id="token-composition"`, `class="progress-block"`)
	if strings.Contains(comp, "—") || !strings.Contains(ovHTML, `data-display="5,600"`) {
		t.Fatalf("overview composition or lifetime counter not pre-rendered: %s", comp)
	}
	if strings.Contains(ovHTML, `data-stage="0"`) {
		t.Fatalf("creature stayed at stage 0 Egg with 5,600 tokens")
	}
	sessSec := s4SectionBetween(t, ovHTML, `<section class="panel sessions-panel"`, `</section>`)
	if got := strings.Count(sessSec, `data-label="When"`); got != 8 {
		t.Fatalf("overview recent sessions row count = %d, want 8", got)
	}
	healthSec := s4SectionBetween(t, ovHTML, `<section class="panel health-panel"`, `</section>`)
	if !strings.Contains(healthSec, "laptop-air") || !strings.Contains(healthSec, "studio-mac") {
		t.Fatalf("health-panel missing laptop-air or studio-mac: %s", healthSec)
	}
	if !strings.Contains(healthSec, "codex ·") || !strings.Contains(healthSec, "claude-code ·") {
		t.Fatalf("health-panel missing adapter-grouped lines: %s", healthSec)
	}
	times := ssExtractTimeElems(sessSec)
	if len(times) == 0 || (!strings.Contains(times[0].Datetime, "-05:00") && !strings.Contains(times[0].Datetime, "-04:00")) {
		t.Fatalf("overview sessions <time> missing Toronto offset: %+v", times)
	}

	var ov overviewPayload
	getJSONWithClient(t, client, s.BaseURL+"/api/v1/analytics/overview", &ov)
	wantPeak := d1CodexTS.In(torLoc).Format("2006-01-02")
	if ov.Activity.CurrentStreak != 3 || ov.Activity.LongestStreak != 3 || ov.Activity.MaxDailyTokens != 3000 || overviewParsePeakDayDate(ov.Activity.PeakDay) != wantPeak {
		t.Fatalf("unexpected overview streaks/peak (want peak=%s 3000): %+v", wantPeak, ov.Activity)
	}
	overviewAssertNoAutoRefreshReload(t, ovHTML)

	// Capture ETag & verify 304 -> new ingest -> 200.
	cOv, hOv, _ := s4Do(t, client, http.MethodGet, s.BaseURL+"/api/v1/analytics/overview", nil, nil)
	if cOv != http.StatusOK {
		t.Fatalf("GET overview status = %d", cOv)
	}
	etag := hOv.Get("ETag")
	if etag != "" {
		c304, _, _ := s4Do(t, client, http.MethodGet, s.BaseURL+"/api/v1/analytics/overview", nil, map[string]string{"If-None-Match": etag})
		if c304 != http.StatusNotModified {
			t.Fatalf("conditional GET overview status = %d, want 304", c304)
		}
	}

	// Append 400 tokens to studio-mac & re-run agent.
	s4WriteClaudeFile(t, filepath.Join(homeStudio, ".claude", "projects", "-Users-dev-myapp", "extra-final.jsonl"), "s4-sc1-extra-final", "/Users/dev/myapp", "cli", "claude-sonnet-4-5", todayTS.Add(15*stepOffset), 300, 100, 0, 0)
	if _, stderr, err := runAgentWithArgs(t, s, homeStudio, stateStudio, "studio-mac", "codex,claude-code"); err != nil {
		t.Fatalf("studio-mac final agent --once failed: %v\nstderr: %s", err, stderr)
	}
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 6000)

	var facets analyticsFacetsPayload
	getJSONWithClient(t, client, s.BaseURL+"/api/v1/analytics?period=30d&machine=studio-mac", &facets)
	if !containsAll(facets.Facets.Tools, "codex", "claude-code", "claude-desktop") {
		t.Fatalf("incremental facets missing tools: %+v", facets.Facets.Tools)
	}
	if etag != "" {
		deadline := time.Now().Add(4 * time.Second)
		for {
			c200, _, _ := s4Do(t, client, http.MethodGet, s.BaseURL+"/api/v1/analytics/overview", nil, map[string]string{"If-None-Match": etag})
			if c200 == http.StatusOK {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("overview ETag stayed 304 after new ingest")
			}
			time.Sleep(100 * time.Millisecond)
		}
	}

	// Step 5: Verify tokemon status CLI modes + privacy.
	agentEnv := filepath.Join(t.TempDir(), "agent.env")
	if err := os.WriteFile(agentEnv, []byte(fmt.Sprintf("TOKEMON_SERVER_URL=%s\nTOKEMON_INGEST_TOKEN=%s\n", s.BaseURL, s.IngestToken)), 0o600); err != nil {
		t.Fatalf("write agent.env: %v", err)
	}
	for _, extra := range [][]string{nil, {"--short"}, {"--json"}} {
		args := append([]string{"status", "--config", agentEnv}, extra...)
		out, stderr, err := s.runCLI(t, nil, args...)
		if err != nil {
			t.Fatalf("tokemon %v failed: %v\nstderr: %s", args, err, stderr)
		}
		if len(extra) == 1 && extra[0] == "--short" {
			if strings.Contains(strings.TrimSpace(out), "\n") || (!strings.Contains(out, "6,000") && !strings.Contains(out, "6000") && !strings.Contains(out, "6K")) {
				t.Fatalf("status --short invalid: %q", out)
			}
		} else if len(extra) == 1 && extra[0] == "--json" {
			var v any
			if err := json.Unmarshal([]byte(out), &v); err != nil || !jsonContainsNumber(v, 6000) {
				t.Fatalf("status --json invalid or missing 6000: %s (%v)", out, err)
			}
		} else if !strings.Contains(out, "6,000") && !strings.Contains(out, "6000") && !strings.Contains(out, "6K") {
			t.Fatalf("status default missing 6,000/6000/6K: %s", out)
		}
		for _, leak := range []string{"myapp", "/Users/dev", "s4-sc1-", ".jsonl", ".zst"} {
			if strings.Contains(out+stderr, leak) {
				t.Fatalf("tokemon %v leaked %q", args, leak)
			}
		}
	}
}

// =============================================================================
// Scenario 2: Cost Optimization & Budget Governance
// =============================================================================

func TestScenario_02_CostOptimizationAndBudgetGovernance(t *testing.T) {
	requireE2E(t)
	var mu sync.Mutex
	var whPayloads []string
	alertSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, readErr := io.ReadAll(r.Body)
		if closeErr := r.Body.Close(); closeErr != nil {
			http.Error(w, closeErr.Error(), http.StatusInternalServerError)
			return
		}
		if readErr != nil {
			http.Error(w, readErr.Error(), http.StatusBadRequest)
			return
		}
		mu.Lock()
		whPayloads = append(whPayloads, string(raw))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(alertSrv.Close)

	quotasYAML := `schema_version: "1"
quotas:
  - label: "Anthropic Monthly Spend"
    provider: anthropic
    reset_day: 1
    budget:
      cost_per_month: 50.0
`
	s := startTestServer(t, serverOpts{
		IngestToken:     "s4-sc2-tok",
		EnableTokedex:   true,
		QuotasYAML:      quotasYAML,
		AlertWebhookURL: alertSrv.URL + "/webhook",
	})
	client := s.authClient(t)

	initHTML := s4WaitHTMLContains(t, client, s.BaseURL+"/", "Anthropic Monthly Spend")
	if strings.Contains(initHTML, "Codex Token Budget") {
		t.Fatalf("unexpected Codex Token Budget before creation")
	}

	cCreate, _, bCreate := ssSubmitQuotaCreate(t, s, client, url.Values{
		"label":            {"Codex Token Budget"},
		"provider":         {"openai"},
		"reset_day":        {"1"},
		"tokens_per_month": {"2000000"},
	}, true)
	if cCreate != http.StatusOK {
		t.Fatalf("create quota status = %d: %s", cCreate, string(bCreate))
	}
	s4WaitQuotas(t, client, s.BaseURL, func(rows []ssQuotaRow) bool { return len(rows) == 2 })
	s4WaitHTMLContains(t, client, s.BaseURL+"/", "Codex Token Budget")

	now := time.Now().UTC()
	monthStartUTC := time.Date(now.Year(), now.Month(), 1, 0, 0, 0, 0, time.UTC)
	tsGPT := now.Add(-2 * time.Hour)
	if tsGPT.Before(monthStartUTC) {
		tsGPT = monthStartUTC
	}
	tsSon := now.Add(-1 * time.Hour)
	if tsSon.Before(monthStartUTC) {
		tsSon = monthStartUTC
	}
	tsUncat := now.Add(-30 * time.Minute)
	if tsUncat.Before(monthStartUTC) {
		tsUncat = monthStartUTC
	}
	s.ingestEvents(t, []ingestEvent{
		{EventID: "sc2-gpt-30d", Timestamp: tsGPT.Format(time.RFC3339Nano), MachineID: "m-opt", SessionID: "sess-sc2-secret", Project: "proj-sc2-secret", Provider: "openai", Model: "gpt-5.4", Tool: "codex", InputTokens: i64(500_000), CacheReadTokens: i64(2_000_000), OutputTokens: i64(100_000), TotalTokens: i64(2_600_000)},
		{EventID: "sc2-son-30d", Timestamp: tsSon.Format(time.RFC3339Nano), MachineID: "m-opt", SessionID: "sess-sc2-2", Project: "proj-sc2-secret", Provider: "anthropic", Model: "claude-sonnet-4", Tool: "claude-code", InputTokens: i64(900_000), CacheReadTokens: i64(100_000), OutputTokens: i64(50_000), TotalTokens: i64(1_050_000)},
		{EventID: "sc2-uncat", Timestamp: tsUncat.Format(time.RFC3339Nano), MachineID: "m-opt", SessionID: "sess-sc2-3", Provider: "custom", Model: "mystery-opt-model", Tool: "codex", TotalTokens: i64(400_000)},
		{EventID: "sc2-gpt-old", Timestamp: now.AddDate(0, 0, -45).Format(time.RFC3339Nano), MachineID: "m-opt", SessionID: "sess-sc2-old", Provider: "openai", Model: "gpt-5.4", Tool: "codex", InputTokens: i64(5_000_000), TotalTokens: i64(5_000_000)},
	})

	s4WaitQuotas(t, client, s.BaseURL, func(rows []ssQuotaRow) bool {
		for _, q := range rows {
			if q.Label == "Codex Token Budget" && q.State == "over" && q.TokensUsed == 2_600_000 {
				return true
			}
		}
		return false
	})

	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		n := len(whPayloads)
		mu.Unlock()
		if n > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for overage webhook alert")
		}
		time.Sleep(100 * time.Millisecond)
	}
	mu.Lock()
	whJoined := strings.Join(whPayloads, "\n")
	mu.Unlock()
	if !strings.Contains(whJoined, "Codex Token Budget") || strings.Contains(whJoined, "sess-sc2-secret") || strings.Contains(whJoined, "proj-sc2-secret") {
		t.Fatalf("webhook alert content/privacy failure: %s", whJoined)
	}

	ovOver := s4WaitHTMLContainsAny(t, client, s.BaseURL+"/", "2,600,000", "2.6M")
	qCodex := overviewQuotaSection(t, ovOver, "Codex Token Budget")
	qAnth := overviewQuotaSection(t, ovOver, "Anthropic Monthly Spend")
	if !strings.Contains(strings.ToLower(qCodex), "over") || (!strings.Contains(qCodex, "2,600,000") && !strings.Contains(qCodex, "2.6M")) {
		t.Fatalf("/ Codex quota bar missing over and 2,600,000/2.6M: %s", qCodex)
	}
	if !strings.Contains(qAnth, "$3.48") {
		t.Fatalf("/ Anthropic quota bar missing $3.48: %s", qAnth)
	}

	s4WaitAnalyticsTokens(t, client, s.BaseURL+"/api/v1/analytics?period=30d&dimension=harnesses", 4_050_000)
	anHTML := s4GetHTML(t, client, s.BaseURL+"/analytics?period=30d&dimension=harnesses")
	for _, want := range []string{"$4.77 saved", "/1M", "80.0%", "10.0%", "Catalog coverage", `href="/tokedex#uncataloged-title"`} {
		if !strings.Contains(anHTML, want) {
			t.Fatalf("/analytics missing %q", want)
		}
	}

	itcWaitCatalog(t, client, s.BaseURL+"/api/v1/catalog", func(c itcCatalogSnapshot) bool {
		return c.ObservedTokens == 9_050_000 && c.UncatalogedModels == 1
	})
	tdHTML := s4GetHTML(t, client, s.BaseURL+"/tokedex")
	if !strings.Contains(tdHTML, `id="uncataloged-title"`) || !strings.Contains(tdHTML, "mystery-opt-model") || !strings.Contains(tdHTML, "400,000") {
		t.Fatalf("/tokedex missing uncataloged mystery-opt-model (400,000)")
	}
	if !strings.Contains(tdHTML, `href="/tokedex/gpt-5.4"`) {
		t.Fatalf("/tokedex missing link to /tokedex/gpt-5.4")
	}
	whatIf := itcExtractWhatIfSection(tdHTML)
	if whatIf == "" || strings.Contains(whatIf, "$0.00") {
		t.Fatalf("/tokedex what-if missing or fabricated $0.00: %s", whatIf)
	}
	for _, old5M := range []string{"5,000,000", "5000000", "7,600,000", "7600000", "9,050,000", "9050000", "7.6M", "9.05M", "9.1M"} {
		if strings.Contains(whatIf, old5M) {
			t.Fatalf("/tokedex 30d what-if unexpectedly included -45d 5M event (%q): %s", old5M, whatIf)
		}
	}

	detHTML := s4GetHTML(t, client, s.BaseURL+"/tokedex/gpt-5.4")
	for _, want := range []string{"GPT-5.4", "http", "m-opt", "sess-sc2-secret"} {
		if !strings.Contains(detHTML, want) {
			t.Fatalf("/tokedex/gpt-5.4 missing %q", want)
		}
	}

	// Edit Codex Token Budget to 5,000,000 -> state returns to ok.
	setHTML := s4GetHTML(t, client, s.BaseURL+"/settings")
	_, editForms, _ := ssDiscoverQuotaForms(setHTML)
	if len(editForms) == 0 {
		t.Fatalf("/settings missing edit form")
	}
	ef := editForms[0]
	editVals := url.Values{"csrf_token": {extractCSRFToken(t, []byte(setHTML))}}
	ssCopyHiddenInputs(ef.Inner, editVals)
	ssCopySubmitButton(ef.Inner, false, editVals)
	editVals.Set("label", "Codex Token Budget")
	editVals.Set("provider", "openai")
	editVals.Set("reset_day", "1")
	editVals.Set("tokens_per_month", "5000000")
	editURL := ef.Action
	if !strings.HasPrefix(editURL, "http") {
		editURL = s.BaseURL + editURL
	}
	cEdit, _, bEdit := ssPostForm(t, client, editURL, s.BaseURL, s.BaseURL+"/settings", editVals)
	if cEdit != http.StatusOK {
		t.Fatalf("edit quota failed (%d): %s", cEdit, string(bEdit))
	}
	s4WaitQuotas(t, client, s.BaseURL, func(rows []ssQuotaRow) bool {
		for _, q := range rows {
			if q.Label == "Codex Token Budget" && q.State == "ok" {
				return true
			}
		}
		return false
	})
}

// =============================================================================
// Scenario 3: Incident Investigation & Drill-Down Triage
// =============================================================================

func TestScenario_03_IncidentInvestigationAndDrillDownTriage(t *testing.T) {
	requireE2E(t)
	nyLoc := s4LoadLoc(t, "America/New_York")
	s := startTestServer(t, serverOpts{IngestToken: "s4-sc3-tok", Timezone: "America/New_York"})

	nowNY := time.Now().In(nyLoc)
	nyToday := time.Date(nowNY.Year(), nowNY.Month(), nowNY.Day(), 0, 0, 0, 0, nyLoc)
	spikeAnchor := nyToday.AddDate(0, 0, -1).Add(14 * time.Hour)
	spikeDay := spikeAnchor.In(nyLoc).Format("2006-01-02")

	var events []ingestEvent
	for i := 0; i < 10; i++ {
		events = append(events, ingestEvent{
			EventID:     fmt.Sprintf("sc3-base-%02d", i),
			Timestamp:   nyToday.AddDate(0, 0, -4).Add(time.Duration(i+1) * time.Hour).UTC().Format(time.RFC3339Nano),
			MachineID:   "m-inc",
			SessionID:   fmt.Sprintf("sess-steady-%02d", i),
			Project:     "proj-steady",
			Provider:    "openai",
			Model:       "gpt-5.4",
			Tool:        "codex",
			TotalTokens: i64(100),
		})
	}
	for i := 0; i < 112; i++ {
		events = append(events, ingestEvent{
			EventID:      fmt.Sprintf("sc3-sp-%03d", i),
			Timestamp:    spikeAnchor.Add(-time.Duration(115-i) * time.Minute).UTC().Format(time.RFC3339Nano),
			MachineID:    "m-inc",
			SessionID:    fmt.Sprintf("sess-spike-%03d", i),
			Project:      "proj-runaway",
			Provider:     "openai",
			Model:        "gpt-5.4",
			Tool:         "codex",
			InputTokens:  i64(int64(800 + i)),
			OutputTokens: i64(200),
			TotalTokens:  i64(int64(1000 + i)),
		})
	}
	for i := 0; i < 5; i++ {
		events = append(events, ingestEvent{
			EventID:     fmt.Sprintf("sc3-noise-%d", i),
			Timestamp:   spikeAnchor.Add(time.Duration(i+1) * time.Minute).UTC().Format(time.RFC3339Nano),
			MachineID:   "m-inc",
			SessionID:   fmt.Sprintf("sess-noise-%d", i),
			Project:     "proj-steady",
			Provider:    "openai",
			Model:       "gpt-5.4",
			Tool:        "codex",
			TotalTokens: i64(200),
		})
	}
	s.ingestEvents(t, events)

	// Ingest root-cause session via agent rollout (with secrets) + second model turn (33.3% cache hit).
	agentHome := filepath.Join(t.TempDir(), "agent-home")
	stateDB := filepath.Join(t.TempDir(), "state.db")
	rolloutPath := filepath.Join(agentHome, ".codex", "sessions", "2026", "11", "23", "rootcause.jsonl")
	if err := os.MkdirAll(filepath.Dir(rolloutPath), 0o755); err != nil {
		t.Fatalf("mkdir rollout dir: %v", err)
	}
	rcTS := spikeAnchor.Add(20 * time.Minute).UTC()
	rcPayload := strings.Join([]string{
		fmt.Sprintf(`{"timestamp":%q,"type":"session_meta","payload":{"id":"sess-spike-rootcause","model_provider":"openai","cwd":"/Users/alice/proj-runaway","base_instructions":"SECRET_INCIDENT_PROMPT"}}`, rcTS.Format(time.RFC3339)),
		fmt.Sprintf(`{"timestamp":%q,"type":"turn_context","payload":{"model":"gpt-5.4","cwd":"/Users/alice/proj-runaway"}}`, rcTS.Add(time.Second).Format(time.RFC3339)),
		fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":1400,"cached_input_tokens":400,"output_tokens":200,"reasoning_output_tokens":0,"total_tokens":1600},"total_token_usage":{"input_tokens":1400,"cached_input_tokens":400,"output_tokens":200,"reasoning_output_tokens":0,"total_tokens":1600}}}}`, rcTS.Add(2*time.Second).Format(time.RFC3339)),
		fmt.Sprintf(`{"timestamp":%q,"type":"turn_context","payload":{"model":"claude-sonnet-4","cwd":"/Users/alice/proj-runaway"}}`, rcTS.Add(3*time.Second).Format(time.RFC3339)),
		fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":3100,"cached_input_tokens":1100,"output_tokens":300,"reasoning_output_tokens":0,"total_tokens":3400},"total_token_usage":{"input_tokens":4500,"cached_input_tokens":1500,"output_tokens":500,"reasoning_output_tokens":0,"total_tokens":5000}}}}`, rcTS.Add(4*time.Second).Format(time.RFC3339)),
	}, "\n") + "\n"
	if err := os.WriteFile(rolloutPath, []byte(rcPayload), 0o644); err != nil {
		t.Fatalf("write rootcause rollout: %v", err)
	}
	if _, stderr, err := runAgentWithArgs(t, s, agentHome, stateDB, "m-inc", "codex"); err != nil {
		t.Fatalf("agent --once failed: %v\nstderr: %s", err, stderr)
	}

	client := s.authClient(t)
	// Baseline 1000 + 112 spike (sum 1000..1111 = 118,216) + 5*200 (1000) + rootcause 5000 = 125,216.
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 125216)

	var ov overviewPayload
	getJSONWithClient(t, client, s.BaseURL+"/api/v1/analytics/overview", &ov)
	if got := overviewParsePeakDayDate(ov.Activity.PeakDay); got != spikeDay {
		t.Fatalf("peak_day = %q, want spikeDay %q", got, spikeDay)
	}

	ovHTML := s4WaitHTMLContains(t, client, s.BaseURL+"/", fmt.Sprintf(`data-date="%s"`, spikeDay))
	if !strings.Contains(ovHTML, "/sessions?from=") {
		t.Fatalf("/ HTML missing heatmap click-through JS targeting /sessions?from=")
	}

	dayHTML := s4GetHTML(t, client, fmt.Sprintf("%s/sessions?from=%s&to=%s", s.BaseURL, spikeDay, spikeDay))
	if strings.Contains(dayHTML, "sess-steady-00") {
		t.Fatalf("day-filtered /sessions leaked baseline sess-steady-00")
	}
	times := ssExtractTimeElems(dayHTML)
	if len(times) == 0 || (!strings.Contains(times[0].Title, "EST") && !strings.Contains(times[0].Title, "EDT")) {
		t.Fatalf("day-filtered /sessions missing EST/EDT title: %+v", times)
	}

	anHTML := s4GetHTML(t, client, fmt.Sprintf("%s/analytics?from=%s&to=%s&dimension=projects", s.BaseURL, spikeDay, spikeDay))
	bd := s4SectionBetween(t, anHTML, `class="panel breakdown-panel"`, `class="panel share-panel"`)
	var projHref string
	for _, h := range s4ExtractHrefs(bd) {
		if strings.Contains(h, "project=proj-runaway") {
			projHref = h
			break
		}
	}
	if projHref == "" {
		t.Fatalf("/analytics projects breakdown missing proj-runaway drill-down link: %s", bd)
	}

	baseQ := fmt.Sprintf("/sessions?period=all&from=%s&to=%s&project=proj-runaway&q=sess-spike&sort=tokens&dir=desc", spikeDay, spikeDay)
	p1HTML := s4GetHTML(t, client, s.BaseURL+baseQ)
	p1Body := ssTableBodyRe.FindString(p1HTML)
	if strings.Contains(p1Body, "proj-steady") || strings.Contains(p1Body, "sess-steady-") || strings.Contains(p1Body, "sess-noise-") {
		t.Fatalf("filtered /sessions leaked proj-steady or noise sessions")
	}
	if !strings.Contains(p1Body, "sess-spike-rootcause") || !strings.Contains(p1Body, "5K") || !strings.Contains(p1Body, "5,000") {
		t.Fatalf("page 1 missing top session sess-spike-rootcause (5K / 5,000): %s", p1Body)
	}
	if strings.Contains(p1Body, "sess-spike-000") {
		t.Fatalf("page 1 unexpectedly showed lowest session sess-spike-000")
	}

	curHTML := p1HTML
	found000 := false
	for nextPage := 2; nextPage <= 10; nextPage++ {
		re := regexp.MustCompile(fmt.Sprintf(`href="([^"]*(?:\?|&amp;|&)page=%d(?:(?:&amp;|&)[^"]*)?)"`, nextPage))
		m := re.FindStringSubmatch(curHTML)
		if len(m) < 2 {
			if nextPage == 2 {
				t.Fatalf("page 1 HTML missing pagination link to page=2 across >100 sessions")
			}
			break
		}
		href := stdhtml.UnescapeString(m[1])
		for _, wantParam := range []string{"from=" + spikeDay, "to=" + spikeDay, "project=proj-runaway", "q=sess-spike", "sort=tokens", "dir=desc"} {
			if !strings.Contains(href, wantParam) {
				t.Fatalf("pagination link %q lost %q", href, wantParam)
			}
		}
		target := s.BaseURL + href
		if strings.HasPrefix(href, "?") {
			target = s.BaseURL + "/sessions" + href
		}
		curHTML = s4GetHTML(t, client, target)
		curBody := ssTableBodyRe.FindString(curHTML)
		if strings.Contains(curBody, "sess-spike-rootcause") {
			t.Fatalf("page %d duplicated sess-spike-rootcause", nextPage)
		}
		if strings.Contains(curBody, "sess-spike-000") {
			found000 = true
			break
		}
	}
	if !found000 {
		t.Fatalf("pagination never reached sess-spike-000 across >100 sessions")
	}

	detHTML := s4GetHTML(t, client, s.BaseURL+"/sessions/sess-spike-rootcause")
	for _, want := range []string{"proj-runaway", "GPT-5.4", "Sonnet 4", "5,000", "33.3%", "<time"} {
		if !strings.Contains(detHTML, want) && !strings.Contains(strings.ToLower(detHTML), strings.ToLower(want)) {
			t.Fatalf("/sessions/sess-spike-rootcause missing %q", want)
		}
	}
	for _, leak := range []string{"SECRET_INCIDENT_PROMPT", "SECRET_SCENARIO_INSTR", "/Users/alice", ".jsonl"} {
		if strings.Contains(detHTML, leak) {
			t.Fatalf("/sessions/sess-spike-rootcause leaked %q", leak)
		}
	}
}

// =============================================================================
// Scenario 4: Multi-Day Restart, Migration & Disaster Recovery
// =============================================================================

func TestScenario_04_MultiDayRestartMigrationAndDisasterRecovery(t *testing.T) {
	requireE2E(t)
	workDir := t.TempDir()
	dbPath := filepath.Join(workDir, "tokemon.db")
	bakDir := filepath.Join(workDir, "backups")

	s1 := startTestServer(t, serverOpts{
		IngestToken:     "s4-ing-tok",
		DBPath:          dbPath,
		BackupDir:       bakDir,
		BackupInterval:  "250ms",
		BackupRetention: 3,
		Timezone:        "UTC",
	})

	agentHome := filepath.Join(t.TempDir(), "agent-home")
	stateDB := filepath.Join(t.TempDir(), "state.db")
	now := time.Now().UTC().Add(-2 * time.Hour)
	rolloutPath := filepath.Join(agentHome, ".codex", "sessions", "2026", "11", "20", "rollout-s4.jsonl")
	if err := os.MkdirAll(filepath.Dir(rolloutPath), 0o755); err != nil {
		t.Fatalf("mkdir rollout: %v", err)
	}
	if err := os.WriteFile(rolloutPath, []byte(s4CodexRolloutLines("s4-codex-agent", "/Users/alice/s4-proj", "gpt-5.4", now, 800, 400, 200, 1000)), 0o644); err != nil {
		t.Fatalf("write rollout-s4: %v", err)
	}
	if _, stderr, err := runAgentWithArgs(t, s1, agentHome, stateDB, "m-s4", "codex"); err != nil {
		t.Fatalf("agent --once failed: %v\nstderr: %s", err, stderr)
	}
	if _, err := os.Stat(dbPath + "-wal"); err != nil {
		t.Fatalf("expected WAL file at %s-wal: %v", dbPath, err)
	}

	s1.ingestEvents(t, []ingestEvent{
		{EventID: "s4-cdx-agg", Timestamp: now.Add(10 * time.Minute).Format(time.RFC3339Nano), MachineID: "m-s4", SessionID: "s4-cdx-sup", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(500), Source: ingestSource{Adapter: "codex", AdapterVersion: "1.0"}},
		{EventID: "s4-claude-1", Timestamp: now.Add(15 * time.Minute).Format(time.RFC3339Nano), MachineID: "m-s4", SessionID: "s4-claude-s", Provider: "anthropic", Model: "claude-sonnet-4", Tool: "claude-code", TotalTokens: i64(400)},
	})
	c1 := s1.authClient(t)
	waitOverviewLifetimeTokens(t, c1, s1.BaseURL, 1900)

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
	time.Sleep(1100 * time.Millisecond)

	set1 := s4GetHTML(t, c1, s1.BaseURL+"/settings")
	if strings.Contains(set1, "No backups yet") || (!strings.Contains(set1, "just now") && !strings.Contains(set1, "ago") && !strings.Contains(set1, "tokemon-backup-v")) {
		t.Fatalf("/settings missing backup age after automated backup")
	}

	s1.Stop()
	files, err := filepath.Glob(filepath.Join(bakDir, "*-backup-v*.db"))
	if err != nil {
		t.Fatalf("glob automated backups after stop: %v", err)
	}
	if len(files) == 0 || len(files) > 3 {
		t.Fatalf("retention expected 1..3 backups, got %d: %v", len(files), files)
	}
	recoveryBackup := files[len(files)-1]
	for _, sc := range []string{recoveryBackup + "-wal", recoveryBackup + "-shm"} {
		if _, err := os.Stat(sc); !os.IsNotExist(err) {
			t.Fatalf("backup file unexpectedly has sidecar %s", sc)
		}
	}
	vOut, vErr, err := s1.runCLI(t, nil, "backup", "verify", recoveryBackup)
	if err != nil {
		t.Fatalf("backup verify failed: %v\nstderr: %s", err, vErr)
	}
	var vRes struct {
		LifetimeTokens int64 `json:"lifetime_tokens"`
	}
	if err := json.Unmarshal([]byte(vOut), &vRes); err != nil || vRes.LifetimeTokens != 1900 {
		t.Fatalf("verify recoveryBackup = %s (%v), want 1900", vOut, err)
	}

	// Restart Cycle 1: Codex dedup guard, no spurious migration backup, aggregate->detailed supersede, lock refusal.
	s2 := startTestServer(t, serverOpts{IngestToken: "s4-ing-tok", DBPath: dbPath, BackupDir: bakDir, BackupInterval: "0", Timezone: "UTC"})
	migFiles, err := filepath.Glob(filepath.Join(bakDir, "*-before-v[0-9]*.db"))
	if err != nil {
		t.Fatalf("glob migration backups: %v", err)
	}
	if len(migFiles) != 0 {
		t.Fatalf("unexpected migration backups on steady restart: %v", migFiles)
	}
	c2 := s2.authClient(t)
	var ov2 overviewPayload
	getJSONWithClient(t, c2, s2.BaseURL+"/api/v1/analytics/overview", &ov2)
	if ov2.LifetimeTokens != 1900 {
		t.Fatalf("s2 overview lifetime_tokens = %d, want 1900", ov2.LifetimeTokens)
	}

	s2.ingestEvents(t, []ingestEvent{{
		EventID:      "s4-cdx-det",
		Timestamp:    now.Add(11 * time.Minute).Format(time.RFC3339Nano),
		MachineID:    "m-s4",
		SessionID:    "s4-cdx-sup",
		Provider:     "openai",
		Model:        "gpt-5.4",
		Tool:         "codex",
		InputTokens:  i64(350),
		OutputTokens: i64(250),
		TotalTokens:  i64(600),
		Source:       ingestSource{Adapter: "codex", AdapterVersion: "1.0"},
	}})
	waitOverviewLifetimeTokens(t, c2, s2.BaseURL, 2000)

	_, lockStderr, lockErr := s2.runCLI(t, nil, "backup", "restore", "--source", recoveryBackup, "--database", dbPath, "--force")
	if lockErr == nil || !strings.Contains(lockStderr, "already in use") {
		t.Fatalf("expected restore while serve holds lock to fail with 'already in use', err=%v stderr=%s", lockErr, lockStderr)
	}
	s2.Stop()

	// Restart Cycle 2 + disaster simulation + restore.
	s3 := startTestServer(t, serverOpts{IngestToken: "s4-ing-tok", DBPath: dbPath, BackupDir: bakDir, BackupInterval: "0", Timezone: "UTC"})
	c3 := s3.authClient(t)
	waitOverviewLifetimeTokens(t, c3, s3.BaseURL, 2000)
	st3, st3Err, err := s3.runCLI(t, nil, "status", "--server", s3.BaseURL, "--token", s3.IngestToken, "--json")
	if err != nil {
		t.Fatalf("s3 status --json failed: %v\nstderr: %s", err, st3Err)
	}
	var st3Any any
	if err := json.Unmarshal([]byte(st3), &st3Any); err != nil || !jsonContainsNumber(st3Any, 2000) {
		t.Fatalf("s3 status --json missing 2000: %s (%v)", st3, err)
	}

	s3.ingestEvents(t, []ingestEvent{{
		EventID:     "s4-disaster-extra",
		MachineID:   "m-s4",
		SessionID:   "s4-disaster",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(9000),
	}})
	waitOverviewLifetimeTokens(t, c3, s3.BaseURL, 11000)
	s3.Stop()

	_, nfErr, nfExit := s3.runCLI(t, nil, "backup", "restore", "--source", recoveryBackup, "--database", dbPath)
	if nfExit == nil || !strings.Contains(nfErr, "pass --force") {
		t.Fatalf("expected restore without --force to fail with 'pass --force', err=%v stderr=%s", nfExit, nfErr)
	}
	rOut, rErr, err := s3.runCLI(t, nil, "backup", "restore", "--source", recoveryBackup, "--database", dbPath, "--force")
	if err != nil {
		t.Fatalf("backup restore --force failed: %v\nstderr: %s", err, rErr)
	}
	var rRes struct {
		PreRestoreBackup string `json:"pre_restore_backup"`
		LifetimeTokens   int64  `json:"lifetime_tokens"`
	}
	if err := json.Unmarshal([]byte(rOut), &rRes); err != nil || rRes.LifetimeTokens != 1900 || rRes.PreRestoreBackup == "" {
		t.Fatalf("unexpected restore result: %s (%v)", rOut, err)
	}
	preOut, preErr, err := s3.runCLI(t, nil, "backup", "verify", rRes.PreRestoreBackup)
	if err != nil || !strings.Contains(preOut, `"lifetime_tokens":11000`) {
		t.Fatalf("pre-restore snapshot verify failed (%v, %s): %s", err, preErr, preOut)
	}
	for _, sc := range []string{dbPath + "-wal", dbPath + "-shm"} {
		if _, err := os.Stat(sc); !os.IsNotExist(err) {
			t.Fatalf("expected %s removed after clean restore", sc)
		}
	}

	// Final Restart: recovered state + /settings backup age + dedup intact.
	s4 := startTestServer(t, serverOpts{IngestToken: "s4-ing-tok", DBPath: dbPath, BackupDir: bakDir, BackupInterval: "0", Timezone: "UTC"})
	c4 := s4.authClient(t)
	waitOverviewLifetimeTokens(t, c4, s4.BaseURL, 1900)
	stShort, stSErr, err := s4.runCLI(t, nil, "status", "--server", s4.BaseURL, "--token", s4.IngestToken, "--short")
	if err != nil || (!strings.Contains(stShort, "1,900") && !strings.Contains(stShort, "1900") && !strings.Contains(stShort, "1.9K")) {
		t.Fatalf("s4 status --short failed (%v, %s): %q", err, stSErr, stShort)
	}
	set4 := s4GetHTML(t, c4, s4.BaseURL+"/settings")
	if strings.Contains(set4, "No backups yet") || strings.Contains(set4, "before-restore") {
		t.Fatalf("s4 /settings backup age misread pre-restore or empty state")
	}
	dupRes := s4.ingestEvents(t, []ingestEvent{{
		EventID:     "s4-claude-1",
		MachineID:   "m-s4",
		SessionID:   "s4-claude-s",
		Provider:    "anthropic",
		Model:       "claude-sonnet-4",
		Tool:        "claude-code",
		TotalTokens: i64(400),
	}})
	if dupRes.Accepted != 0 || dupRes.Duplicates != 1 || dupRes.CurrentTotal != 1900 {
		t.Fatalf("post-restore dedup failed: %+v", dupRes)
	}
}

// =============================================================================
// Scenario 5: Hardened Authenticated Remote Hub & Shareable Telemetry
// =============================================================================

func TestScenario_05_HardenedAuthenticatedHubAndShareCard(t *testing.T) {
	requireE2E(t)
	ingestTok := "s5-ingest-secret-token"
	dashTok := "s5-dashboard-secret-token"
	password := "hardened-hub-pass-42"

	s := startTestServer(t, serverOpts{
		IngestToken:    ingestTok,
		DashboardToken: dashTok,
		Username:       "operator",
		Password:       password,
		EnableTokedex:  true,
		Timezone:       "UTC",
	})

	now := time.Now().UTC()
	todayUTC := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	s5D2TS := todayUTC.AddDate(0, 0, -2).Add(12 * time.Hour)
	s5D1TS := todayUTC.AddDate(0, 0, -1).Add(12 * time.Hour)
	s5D0TS := now.Add(-20 * time.Minute).Truncate(time.Second)
	if s5D0TS.Format("2006-01-02") != todayUTC.Format("2006-01-02") {
		s5D0TS = todayUTC
	}
	agentHome := filepath.Join(t.TempDir(), "agent-home")
	stateDB := filepath.Join(t.TempDir(), "state.db")
	rolloutPath := filepath.Join(agentHome, ".codex", "sessions", "2026", "11", "22", "s5-secret.jsonl")
	if err := os.MkdirAll(filepath.Dir(rolloutPath), 0o755); err != nil {
		t.Fatalf("mkdir rollout: %v", err)
	}
	rolloutLines := strings.Join([]string{
		fmt.Sprintf(`{"timestamp":%q,"type":"session_meta","payload":{"id":"s5-agent-sess","model_provider":"openai","cwd":"/Users/alice/top-secret-repo","base_instructions":"SECRET_PROMPT_DO_NOT_LEAK"}}`, s5D2TS.Format(time.RFC3339)),
		fmt.Sprintf(`{"timestamp":%q,"type":"turn_context","payload":{"model":"gpt-5.4","cwd":"/Users/alice/top-secret-repo"}}`, s5D2TS.Add(time.Second).Format(time.RFC3339)),
		fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":150,"cached_input_tokens":0,"output_tokens":50,"reasoning_output_tokens":0,"total_tokens":200},"total_token_usage":{"input_tokens":150,"cached_input_tokens":0,"output_tokens":50,"reasoning_output_tokens":0,"total_tokens":200}}}}`, s5D2TS.Add(2*time.Second).Format(time.RFC3339)),
	}, "\n") + "\n"
	if err := os.WriteFile(rolloutPath, []byte(rolloutLines), 0o644); err != nil {
		t.Fatalf("write rollout: %v", err)
	}
	if _, stderr, err := runAgentWithArgs(t, s, agentHome, stateDB, "m-s5-secret", "codex"); err != nil {
		t.Fatalf("agent --once failed: %v\nstderr: %s", err, stderr)
	}

	s.ingestEvents(t, []ingestEvent{
		{EventID: "s5-d1", Timestamp: s5D1TS.Format(time.RFC3339Nano), MachineID: "m-s5-secret", SessionID: "s5-sess-d1", Project: "top-secret-repo", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(300)},
		{EventID: "s5-d0", Timestamp: s5D0TS.Format(time.RFC3339Nano), MachineID: "m-s5-secret", SessionID: "s5-sess-d0", Project: "top-secret-repo", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(400)},
	})

	// Step 2: Unauthenticated boundaries & least-privilege ingest token rejection.
	for _, p := range []string{"/", "/analytics", "/sessions", "/settings", "/insights", "/tokedex", "/api/v1/card.svg"} {
		code, hdr, _ := s4Do(t, s.Client, http.MethodGet, s.BaseURL+p, nil, map[string]string{"Accept": "text/html"})
		if code != http.StatusSeeOther || !strings.HasPrefix(hdr.Get("Location"), "/login?next=") {
			t.Fatalf("anon HTML %s status=%d loc=%q, want 303 /login?next=...", p, code, hdr.Get("Location"))
		}
	}
	for _, p := range []string{"/api/v1/analytics/overview", "/api/v1/insights", "/api/v1/card.svg", "/api/v1/machines"} {
		code, hdr, _ := s4Do(t, s.Client, http.MethodGet, s.BaseURL+p, nil, nil)
		if code != http.StatusUnauthorized || !strings.Contains(hdr.Get("WWW-Authenticate"), "Basic") {
			t.Fatalf("anon API %s status=%d www=%q", p, code, hdr.Get("WWW-Authenticate"))
		}
	}
	for _, p := range []string{"/api/v1/card.svg", "/api/v1/machines"} {
		code, _, _ := s4Do(t, s.Client, http.MethodGet, s.BaseURL+p, nil, map[string]string{"Authorization": "Bearer " + ingestTok})
		if code != http.StatusUnauthorized {
			t.Fatalf("ingestTok on %s status=%d, want 401", p, code)
		}
	}

	// Step 3: Login CSRF, evil Origin rejection, and cookie login.
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New: %v", err)
	}
	cookieClient := &http.Client{
		Jar:     jar,
		Timeout: 10 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	cLogin, hLogin, bLogin := s4Do(t, cookieClient, http.MethodGet, s.BaseURL+"/login?next=%2Fanalytics%3Fperiod%3D30d", nil, map[string]string{"Accept": "text/html"})
	if cLogin != http.StatusOK || hLogin.Get("Cache-Control") != "no-store" {
		t.Fatalf("GET /login status=%d cc=%q", cLogin, hLogin.Get("Cache-Control"))
	}
	csrfLogin := extractCSRFToken(t, bLogin)

	badForm := url.Values{"username": {s.Username}, "password": {password}, "csrf_token": {"forged"}, "next": {"/"}}
	cForged, _, _ := s4Do(t, cookieClient, http.MethodPost, s.BaseURL+"/login", []byte(badForm.Encode()), map[string]string{
		"Content-Type": "application/x-www-form-urlencoded",
		"Origin":       s.BaseURL,
		"Referer":      s.BaseURL + "/login",
	})
	if cForged != http.StatusForbidden {
		t.Fatalf("forged CSRF POST /login status = %d, want 403", cForged)
	}
	evilForm := url.Values{"username": {s.Username}, "password": {password}, "csrf_token": {csrfLogin}, "next": {"/"}}
	cEvil, _, _ := s4Do(t, cookieClient, http.MethodPost, s.BaseURL+"/login", []byte(evilForm.Encode()), map[string]string{
		"Content-Type": "application/x-www-form-urlencoded",
		"Origin":       "https://evil.example",
		"Referer":      "https://evil.example/login",
	})
	if cEvil != http.StatusForbidden {
		t.Fatalf("evil Origin POST /login status = %d, want 403", cEvil)
	}

	okForm := url.Values{"username": {s.Username}, "password": {password}, "csrf_token": {csrfLogin}, "next": {"/analytics?period=30d"}}
	cOK, hOK, _ := s4Do(t, cookieClient, http.MethodPost, s.BaseURL+"/login", []byte(okForm.Encode()), map[string]string{
		"Content-Type": "application/x-www-form-urlencoded",
		"Origin":       s.BaseURL,
		"Referer":      s.BaseURL + "/login",
	})
	if cOK != http.StatusSeeOther || hOK.Get("Location") != "/analytics?period=30d" {
		t.Fatalf("valid POST /login status=%d loc=%q, want 303 /analytics?period=30d", cOK, hOK.Get("Location"))
	}
	baseURLParsed, err := url.Parse(s.BaseURL)
	if err != nil {
		t.Fatalf("parse BaseURL: %v", err)
	}
	var hasSessionCookie bool
	for _, c := range jar.Cookies(baseURLParsed) {
		if c.Name == "tokemon_session" && c.Value != "" {
			hasSessionCookie = true
		}
	}
	if !hasSessionCookie {
		t.Fatalf("POST /login did not set tokemon_session cookie")
	}

	// Set machine alias using authenticated cookie client.
	if cA, _, _ := s4PostAliases(t, s, cookieClient, url.Values{"machine_identity": {"m-s5-secret"}, "machine_alias": {"Secret Workstation"}}); cA != http.StatusSeeOther && cA != http.StatusOK {
		t.Fatalf("POST /settings/aliases status = %d", cA)
	}
	waitOverviewLifetimeTokens(t, cookieClient, s.BaseURL, 900)

	// Step 4: Authenticated HTML pages, CAR-205, /settings secret hygiene, LCP & static URLs.
	var cssURL, tokenDexURL, stageURL, fontURL string
	for _, p := range []string{"/", "/analytics", "/sessions", "/settings", "/insights", "/tokedex"} {
		code, hdr, raw := s4Do(t, cookieClient, http.MethodGet, s.BaseURL+p, nil, map[string]string{"Accept": "text/html"})
		if code != http.StatusOK || hdr.Get("Cache-Control") != "no-store" {
			t.Fatalf("authed GET %s status=%d cc=%q", p, code, hdr.Get("Cache-Control"))
		}
		htmlStr := string(raw)
		nav := ssExtractNavBlock(t, p, htmlStr)
		if !strings.Contains(nav, `href="/tokedex"`) || strings.Contains(nav, `href="/insights"`) {
			t.Fatalf("nav violation on %s: %s", p, nav)
		}
		if p == "/settings" {
			if !strings.Contains(htmlStr, "Password account") {
				t.Fatalf("/settings missing Password account mode")
			}
			for _, secret := range []string{ingestTok, dashTok, password, "$2a$", "$2b$"} {
				if strings.Contains(htmlStr, secret) {
					t.Fatalf("/settings leaked secret %q", secret)
				}
			}
		}
		if p == "/" {
			cssURL = s4BaseCSSRe.FindString(htmlStr)
			tokenDexURL = s4TokenDexRe.FindString(htmlStr)
			stageURL = s4StagePNGRe.FindString(htmlStr)
			fontURL = s4FontRe.FindString(htmlStr)
			var hasPreload bool
			for _, tag := range s4PreloadRe.FindAllString(htmlStr, -1) {
				if strings.Contains(tag, `as="font"`) && strings.Contains(tag, `pixelify-sans-latin.woff2`) && strings.Contains(tag, `crossorigin`) {
					hasPreload = true
					break
				}
			}
			imgTag := s4CreatureRe.FindString(htmlStr)
			if !hasPreload || imgTag == "" || !strings.Contains(imgTag, `fetchpriority="high"`) || !s4WidthRe.MatchString(imgTag) || !s4HeightRe.MatchString(imgTag) {
				t.Fatalf("/ missing font preload or .creature-art LCP attributes: img=%q", imgTag)
			}
		}
	}
	if cssURL == "" || tokenDexURL == "" || stageURL == "" || fontURL == "" {
		t.Fatalf("missing versioned static URLs on /: css=%q td=%q st=%q fnt=%q", cssURL, tokenDexURL, stageURL, fontURL)
	}

	// Step 5: Gzip + Vary + immutable /static/ + binary skip via noAutoDecompressClient.
	gzClient := &http.Client{
		Jar:     jar,
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DisableCompression: true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	for _, tc := range []struct {
		path string
		cc   string
	}{
		{"/", "no-store"},
		{"/analytics", "no-store"},
		{"/api/v1/analytics/overview", "no-store"},
		{"/api/v1/card.svg", "no-store"},
		{cssURL, "public, max-age=31536000, immutable"},
	} {
		cGz, hGz, bGz := s4Do(t, gzClient, http.MethodGet, s.BaseURL+tc.path, nil, map[string]string{"Accept-Encoding": "gzip"})
		if cGz != http.StatusOK || hGz.Get("Content-Encoding") != "gzip" || hGz.Get("Cache-Control") != tc.cc || !varyHasAcceptEncoding(&http.Response{Header: hGz}) {
			t.Fatalf("gzip GET %s unexpected: status=%d ce=%q cc=%q vary=%v", tc.path, cGz, hGz.Get("Content-Encoding"), hGz.Get("Cache-Control"), hGz.Values("Vary"))
		}
		if len(gunzipBytes(t, bGz)) == 0 {
			t.Fatalf("empty gunzipped %s", tc.path)
		}
		cPl, hPl, _ := s4Do(t, gzClient, http.MethodGet, s.BaseURL+tc.path, nil, nil)
		if cPl != http.StatusOK || hPl.Get("Content-Encoding") != "" || !varyHasAcceptEncoding(&http.Response{Header: hPl}) {
			t.Fatalf("plain GET %s unexpected: status=%d ce=%q vary=%v", tc.path, cPl, hPl.Get("Content-Encoding"), hPl.Values("Vary"))
		}
	}
	for _, bin := range []struct {
		u     string
		magic []byte
	}{
		{tokenDexURL, []byte("\x89PNG\r\n\x1a\n")},
		{stageURL, []byte("\x89PNG\r\n\x1a\n")},
		{fontURL, []byte("wOF2")},
	} {
		cB, hB, rawB := s4Do(t, gzClient, http.MethodGet, s.BaseURL+bin.u, nil, map[string]string{"Accept-Encoding": "gzip"})
		if cB != http.StatusOK || hB.Get("Content-Encoding") != "" || hB.Get("Cache-Control") != "public, max-age=31536000, immutable" || !bytes.HasPrefix(rawB, bin.magic) {
			t.Fatalf("binary static %s failed: status=%d ce=%q cc=%q", bin.u, cB, hB.Get("Content-Encoding"), hB.Get("Cache-Control"))
		}
	}

	// Step 6: Bearer DashboardToken /api/v1/card.svg structure + palette + aggregate fidelity + zero privacy leak.
	cCard, hCard, svgBytes := s4Do(t, s.Client, http.MethodGet, s.BaseURL+"/api/v1/card.svg", nil, map[string]string{"Authorization": "Bearer " + dashTok})
	if cCard != http.StatusOK || !strings.Contains(hCard.Get("Content-Type"), "image/svg+xml") || hCard.Get("Cache-Control") != "no-store" {
		t.Fatalf("Bearer card.svg failed: status=%d ct=%q cc=%q", cCard, hCard.Get("Content-Type"), hCard.Get("Cache-Control"))
	}
	doc := itcValidateSVGXML(t, svgBytes)
	if doc.Xmlns != "http://www.w3.org/2000/svg" || doc.ViewBox == "" || doc.Width == "" || doc.Height == "" {
		t.Fatalf("invalid card.svg root attrs: %+v", doc)
	}
	svgStr := string(svgBytes)
	svgLower := strings.ToLower(svgStr)
	for _, hex := range []string{"#10110f", "#171916", "#9bbba0", "#d2a477", "#f0ede5"} {
		if !strings.Contains(svgLower, hex) {
			t.Fatalf("card.svg missing palette %s", hex)
		}
	}
	svgText := itcExtractSVGText(t, svgBytes)
	if !strings.Contains(svgText, "Bytelet") || !strings.Contains(svgText, "900") || !regexp.MustCompile(`(?:^|\D)3(?:\D|$)`).MatchString(svgText) || !itcSVGGeometryRe.MatchString(svgStr) {
		t.Fatalf("card.svg missing Bytelet/900/3-streak/sparkline in %q", svgText)
	}
	for _, leak := range []string{"top-secret-repo", "Secret Workstation", "m-s5-secret", "s5-agent-sess", "s5-sess-", "/Users/", ".jsonl", "SECRET_PROMPT_DO_NOT_LEAK"} {
		if strings.Contains(svgStr, leak) {
			t.Fatalf("card.svg leaked %q", leak)
		}
	}

	// Step 7: POST /logout clears session cookie and subsequent requests via cookieClient are unauthenticated.
	homeHTML := s4GetHTML(t, cookieClient, s.BaseURL+"/")
	csrfLogout := extractCSRFToken(t, []byte(homeHTML))
	logoutForm := url.Values{"csrf_token": {csrfLogout}}
	cOut, hOut, _ := s4Do(t, cookieClient, http.MethodPost, s.BaseURL+"/logout", []byte(logoutForm.Encode()), map[string]string{
		"Content-Type": "application/x-www-form-urlencoded",
		"Origin":       s.BaseURL,
		"Referer":      s.BaseURL + "/",
	})
	if cOut != http.StatusSeeOther || hOut.Get("Location") != "/login" {
		t.Fatalf("POST /logout status=%d loc=%q, want 303 /login", cOut, hOut.Get("Location"))
	}
	var clearedSessionCookie bool
	for _, c := range (&http.Response{Header: hOut}).Cookies() {
		if c.Name == "tokemon_session" && (c.MaxAge <= 0 || c.Value == "") {
			clearedSessionCookie = true
		}
	}
	if !clearedSessionCookie {
		t.Fatalf("POST /logout did not emit clearing Set-Cookie for tokemon_session: %+v", hOut.Values("Set-Cookie"))
	}
	for _, c := range jar.Cookies(baseURLParsed) {
		if c.Name == "tokemon_session" && c.Value != "" {
			t.Fatalf("cookiejar still retained non-empty tokemon_session after logout: %+v", c)
		}
	}
	// cookieClient's jar honored Set-Cookie: subsequent API & HTML requests must be rejected.
	cAfterAPI, _, _ := s4Do(t, cookieClient, http.MethodGet, s.BaseURL+"/api/v1/card.svg", nil, nil)
	if cAfterAPI != http.StatusUnauthorized {
		t.Fatalf("cookieClient after logout on /api/v1/card.svg status=%d, want 401", cAfterAPI)
	}
	cAfterHTML, hAfterHTML, _ := s4Do(t, cookieClient, http.MethodGet, s.BaseURL+"/", nil, map[string]string{"Accept": "text/html"})
	if cAfterHTML != http.StatusSeeOther || !strings.HasPrefix(hAfterHTML.Get("Location"), "/login") {
		t.Fatalf("cookieClient after logout on / status=%d loc=%q, want 303 /login...", cAfterHTML, hAfterHTML.Get("Location"))
	}
}

// =============================================================================
// Scenario 6: Aliased Fleet Analytics & Uncataloged Model Discovery
// =============================================================================

func TestScenario_06_AliasedFleetAnalyticsAndUncatalogedDiscovery(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "s6-ing-tok", EnableTokedex: true, Timezone: "UTC"})
	s.ingestEvents(t, []ingestEvent{
		{
			EventID:         "s6-ev-1",
			MachineID:       "raw-mac-studio-01",
			SessionID:       "s6-sess-1",
			Project:         "fleet-core",
			Provider:        "openai",
			Model:           "gpt-5.4",
			Tool:            "codex",
			InputTokens:     i64(600_000),
			CacheReadTokens: i64(200_000),
			OutputTokens:    i64(200_000),
			TotalTokens:     i64(1_000_000),
		},
		{
			EventID:     "s6-ev-2",
			MachineID:   "raw-linux-box-02",
			SessionID:   "s6-sess-2",
			Project:     "fleet-eval",
			Provider:    "custom",
			Model:       "custom-uncataloged-70b",
			Tool:        "codex",
			TotalTokens: i64(500_000),
		},
	})
	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 1_500_000)
	s4WaitAnalyticsTokens(t, client, s.BaseURL+"/api/v1/analytics?period=30d", 1_500_000)

	// Step 2: /settings usage-sorted models & machines + save 4 aliases.
	setHTML := s4GetHTML(t, client, s.BaseURL+"/settings")
	modSec := s4SectionBetween(t, setHTML, `id="settings-form-title"`, `id="machines-title"`)
	machSec := s4SectionBetween(t, setHTML, `id="machines-title"`, `</form>`)
	if strings.Index(modSec, "gpt-5.4") < 0 || strings.Index(modSec, "custom-uncataloged-70b") < 0 || strings.Index(modSec, "gpt-5.4") > strings.Index(modSec, "custom-uncataloged-70b") {
		t.Fatalf("/settings models not sorted DESC by usage: %s", modSec)
	}
	if strings.Index(machSec, "raw-mac-studio-01") < 0 || strings.Index(machSec, "raw-linux-box-02") < 0 || strings.Index(machSec, "raw-mac-studio-01") > strings.Index(machSec, "raw-linux-box-02") {
		t.Fatalf("/settings machines not sorted DESC by usage: %s", machSec)
	}
	if !strings.Contains(modSec, "/analytics") || !strings.Contains(modSec, "/tokedex") {
		t.Fatalf("/settings models missing /analytics or /tokedex cross-links")
	}

	cSave, hSave, _ := s4PostAliases(t, s, client, url.Values{
		"machine_identity": {"raw-mac-studio-01", "raw-linux-box-02"},
		"machine_alias":    {"Studio Hub", "Build Node"},
		"model_identity":   {"gpt-5.4", "custom-uncataloged-70b"},
		"model_alias":      {"GPT-5.4 Flagship", "Local 70B"},
	})
	if cSave != http.StatusSeeOther || hSave.Get("Location") != "/settings?saved=1" {
		t.Fatalf("POST /settings/aliases status=%d loc=%q", cSave, hSave.Get("Location"))
	}
	savedHTML := s4GetHTML(t, client, s.BaseURL+"/settings?saved=1")
	for _, want := range []string{"Aliases saved.", "Studio Hub", "Build Node", "GPT-5.4 Flagship", "Local 70B"} {
		if !strings.Contains(savedHTML, want) {
			t.Fatalf("/settings?saved=1 missing %q", want)
		}
	}

	// Step 3: /analytics?period=30d&dimension=machines.
	anHTML := s4GetHTML(t, client, s.BaseURL+"/analytics?period=30d&dimension=machines")
	if !strings.Contains(anHTML, `<h2 class="section-title" id="breakdown-title">Machines</h2>`) && !strings.Contains(anHTML, `id="breakdown-title">Machines</h2>`) {
		t.Fatalf("/analytics?dimension=machines missing Machines breakdown title")
	}
	if !strings.Contains(anHTML, `<input type="hidden" name="dimension" value="machines">`) {
		t.Fatalf("/analytics?dimension=machines missing hidden dimension=machines")
	}
	bd := s4SectionBetween(t, anHTML, `class="panel breakdown-panel"`, `class="panel share-panel"`)
	share := s4SectionBetween(t, anHTML, `class="panel share-panel"`, `class="panel sessions-panel"`)
	for _, want := range []string{`<span class="breakdown-name">Studio Hub</span>`, `<span class="breakdown-name">Build Node</span>`} {
		if !strings.Contains(bd, want) {
			t.Fatalf("breakdown missing %q: %s", want, bd)
		}
	}
	for _, want := range []string{`<strong>Studio Hub</strong>`, `<strong>Build Node</strong>`, `data-name="Studio Hub"`, `data-name="Build Node"`, `>Studio Hub share</th>`, `>Build Node share</th>`} {
		if !strings.Contains(share, want) {
			t.Fatalf("share section missing %q: %s", want, share)
		}
	}
	for _, seg := range []string{bd, share} {
		if strings.Contains(seg, "GPT-5.4 Flagship") || strings.Contains(seg, "Local 70B") {
			t.Fatalf("machines dimension leaked model alias in %s", seg)
		}
	}

	filtHTML := s4GetHTML(t, client, s.BaseURL+"/analytics?period=30d&dimension=machines&machine=raw-mac-studio-01")
	if !strings.Contains(filtHTML, `data-count="1000000"`) && !strings.Contains(filtHTML, "1,000,000") {
		t.Fatalf("filtered machine=raw-mac-studio-01 missing 1,000,000 total")
	}

	// Step 4: Follow uncataloged model discovery link to /tokedex#uncataloged-title.
	insights := s4SectionBetween(t, anHTML, `class="insights-strip"`, `</section>`)
	if !strings.Contains(insights, "Catalog coverage") || !strings.Contains(insights, `href="/tokedex#uncataloged-title"`) {
		t.Fatalf("/analytics missing Catalog coverage link to /tokedex#uncataloged-title: %s", insights)
	}
	itcWaitCatalog(t, client, s.BaseURL+"/api/v1/catalog", func(c itcCatalogSnapshot) bool {
		return c.ObservedTokens == 1_500_000 && c.UncatalogedModels == 1
	})
	tdHTML := s4GetHTML(t, client, s.BaseURL+"/tokedex")
	if !strings.Contains(tdHTML, `id="uncataloged-title"`) {
		t.Fatalf("/tokedex missing uncataloged-title section")
	}
	uncatSec := s4SectionBetween(t, tdHTML, `id="uncataloged-title"`, `</section>`)
	if !strings.Contains(uncatSec, "custom-uncataloged-70b") || !strings.Contains(uncatSec, "500,000") || !strings.Contains(uncatSec, "1 event") {
		t.Fatalf("/tokedex missing uncataloged custom-uncataloged-70b entry (500,000 / 1 event): %s", uncatSec)
	}
	c200, _, _ := s4Do(t, client, http.MethodGet, s.BaseURL+"/tokedex/gpt-5.4", nil, map[string]string{"Accept": "text/html"})
	cUncat, _, bodyUncat := s4Do(t, client, http.MethodGet, s.BaseURL+"/tokedex/custom-uncataloged-70b", nil, map[string]string{"Accept": "text/html"})
	c404, _, _ := s4Do(t, client, http.MethodGet, s.BaseURL+"/tokedex/unobserved-uncataloged-model", nil, map[string]string{"Accept": "text/html"})
	if c200 != http.StatusOK || cUncat != http.StatusOK || c404 != http.StatusNotFound {
		t.Fatalf("/tokedex detail codes: gpt-5.4=%d (want 200), custom-uncataloged-70b=%d (want 200), unobserved=%d (want 404)", c200, cUncat, c404)
	}
	uncatDetailStr := string(bodyUncat)
	if !strings.Contains(uncatDetailStr, "Uncataloged") || !strings.Contains(uncatDetailStr, "Unknown") {
		t.Fatalf("/tokedex/custom-uncataloged-70b missing Uncataloged/Unknown markers")
	}
	if strings.Contains(uncatDetailStr, "$0.00") {
		t.Fatalf("/tokedex/custom-uncataloged-70b leaked $0.00 for uncataloged model")
	}

	// Step 5: Export CSV on dimension=machines -> preserves raw machine IDs (not aliases).
	cExp, hExp, csvBytes := s4Do(t, client, http.MethodGet, s.BaseURL+"/api/v1/analytics/export?period=30d&dimension=machines&format=csv", nil, nil)
	if cExp != http.StatusOK {
		t.Fatalf("CSV export status = %d", cExp)
	}
	if !strings.Contains(hExp.Get("Content-Type"), "text/csv") || hExp.Get("Content-Disposition") != `attachment; filename="tokemon-analytics-machines-30d.csv"` {
		t.Fatalf("unexpected CSV headers: ct=%q cd=%q", hExp.Get("Content-Type"), hExp.Get("Content-Disposition"))
	}
	recs, err := csv.NewReader(bytes.NewReader(csvBytes)).ReadAll()
	if err != nil || len(recs) != 3 {
		t.Fatalf("CSV rows = %d (err=%v), want 3: %s", len(recs), err, string(csvBytes))
	}
	wantHeader := []string{"dimension", "period", "name", "tokens", "events", "sessions", "share_percent"}
	for i, col := range wantHeader {
		if recs[0][i] != col {
			t.Fatalf("CSV header[%d] = %q, want %q", i, recs[0][i], col)
		}
	}
	csvStr := string(csvBytes)
	if !strings.Contains(csvStr, "machines,30d,raw-mac-studio-01,1000000,1,1,66.7") || !strings.Contains(csvStr, "machines,30d,raw-linux-box-02,500000,1,1,33.3") {
		t.Fatalf("CSV rows did not match expected raw machine totals/shares: %s", csvStr)
	}
	if strings.Contains(csvStr, "Studio Hub") || strings.Contains(csvStr, "Build Node") {
		t.Fatalf("CSV export leaked display aliases instead of raw machine IDs: %s", csvStr)
	}
}
