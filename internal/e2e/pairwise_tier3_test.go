//go:build e2e

package e2e_test

import (
	"bytes"
	"encoding/base64"
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
	p3DupDimSelectRe      = regexp.MustCompile(`(?i)<select[^>]*\bname=["']dimension["']`)
	p3BaseCSSVersionedRe  = regexp.MustCompile(`/static/tokemon/base\.css\?v=[^"'\s>]+`)
	p3StagePNGVersionedRe = regexp.MustCompile(`/static/tokemon/stage-\d+\.png\?v=[^"'\s>]+`)
	p3FontVersionedRe     = regexp.MustCompile(`/static/tokemon/fonts/pixelify-sans-latin\.woff2\?v=[^"'\s>]+`)
	p3PreloadLinkRe       = regexp.MustCompile(`(?i)<link\b[^>]*\brel="preload"[^>]*>`)
	p3CreatureImgRe       = regexp.MustCompile(`<img\b[^>]*class="[^"]*\bcreature-art\b[^"]*"[^>]*>`)
	p3WidthAttrRe         = regexp.MustCompile(`\bwidth="\d+"`)
	p3HeightAttrRe        = regexp.MustCompile(`\bheight="\d+"`)
	p3ContentVisRe        = regexp.MustCompile(`content-visibility\s*:\s*auto`)
	p3ContainIntrinsicRe  = regexp.MustCompile(`contain-intrinsic-size\s*:`)
)

func p3LoadLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("LoadLocation(%q): %v", name, err)
	}
	return loc
}

func p3DoErr(client *http.Client, method, fullURL string, body []byte, headers map[string]string) (int, http.Header, []byte, error) {
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, fullURL, reader)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("new %s %s: %w", method, fullURL, err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, nil, fmt.Errorf("%s %s: %w", method, fullURL, err)
	}
	data, readErr := io.ReadAll(resp.Body)
	closeErr := resp.Body.Close()
	if readErr != nil {
		return 0, nil, nil, fmt.Errorf("read %s %s: %w", method, fullURL, readErr)
	}
	if closeErr != nil {
		return 0, nil, nil, fmt.Errorf("close %s %s: %w", method, fullURL, closeErr)
	}
	return resp.StatusCode, resp.Header, data, nil
}

func p3Do(t *testing.T, client *http.Client, method, fullURL string, body []byte, headers map[string]string) (int, http.Header, []byte) {
	t.Helper()
	code, hdr, data, err := p3DoErr(client, method, fullURL, body, headers)
	if err != nil {
		t.Fatal(err)
	}
	return code, hdr, data
}

func p3GetHTML(t *testing.T, client *http.Client, fullURL string) string {
	t.Helper()
	code, _, body := p3Do(t, client, http.MethodGet, fullURL, nil, map[string]string{"Accept": "text/html"})
	if code != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200; body=%s", fullURL, code, string(body))
	}
	return string(body)
}

func p3WaitHTMLContains(t *testing.T, client *http.Client, fullURL, needle string) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last string
	for {
		last = p3GetHTML(t, client, fullURL)
		if strings.Contains(last, needle) {
			return last
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s HTML to contain %q (len=%d)", fullURL, needle, len(last))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func p3WaitAnalyticsTokens(t *testing.T, client *http.Client, fullURL string, wantTokens int64) analyticsAPIPayload {
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

func p3WaitEvolution(t *testing.T, client *http.Client, baseURL string, pred func(evolutionPayload) bool) evolutionPayload {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last evolutionPayload
	for {
		getJSONWithClient(t, client, baseURL+"/api/v1/evolution", &last)
		if pred(last) {
			return last
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for /api/v1/evolution condition; last=%+v", last)
		}
		time.Sleep(150 * time.Millisecond)
	}
}

func p3WaitQuotas(t *testing.T, client *http.Client, baseURL string, pred func([]ssQuotaRow) bool) []ssQuotaRow {
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

func p3PostAliases(t *testing.T, s *testServer, client *http.Client, values url.Values) (int, http.Header, []byte) {
	t.Helper()
	if !values.Has("csrf_token") {
		settingsHTML := p3GetHTML(t, client, s.BaseURL+"/settings")
		values.Set("csrf_token", extractCSRFToken(t, []byte(settingsHTML)))
	}
	noRedir := ssNoRedirectClient(client)
	return p3Do(t, noRedir, http.MethodPost, s.BaseURL+"/settings/aliases", []byte(values.Encode()), map[string]string{
		"Content-Type": "application/x-www-form-urlencoded",
		"Origin":       s.BaseURL,
		"Referer":      s.BaseURL + "/settings",
	})
}

func p3SectionBetween(t *testing.T, html, startMarker, endMarker string) string {
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

func p3ExtractHrefs(fragment string) []string {
	matches := overviewHrefRe.FindAllStringSubmatch(fragment, -1)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		if len(m) >= 2 {
			out = append(out, stdhtml.UnescapeString(m[1]))
		}
	}
	return out
}

func p3CookieNoDecompressClient(t *testing.T, s *testServer) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatalf("cookiejar.New: %v", err)
	}
	loginClient := &http.Client{
		Jar:     jar,
		Timeout: 10 * time.Second,
	}
	code, _, loginHTML := p3Do(t, loginClient, http.MethodGet, s.BaseURL+"/login", nil, map[string]string{"Accept": "text/html"})
	if code != http.StatusOK {
		t.Fatalf("GET /login status = %d, want 200", code)
	}
	csrf := extractCSRFToken(t, loginHTML)
	form := url.Values{
		"username":   {s.Username},
		"password":   {s.Password},
		"csrf_token": {csrf},
		"next":       {"/"},
	}
	postCode, _, postBody := p3Do(t, loginClient, http.MethodPost, s.BaseURL+"/login", []byte(form.Encode()), map[string]string{
		"Content-Type": "application/x-www-form-urlencoded",
		"Origin":       s.BaseURL,
		"Referer":      s.BaseURL + "/login",
	})
	if postCode != http.StatusOK {
		t.Fatalf("POST /login final status = %d, want 200: %s", postCode, string(postBody))
	}
	return &http.Client{
		Jar:     jar,
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DisableCompression: true,
		},
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

func p3WriteCodexJSONL(t *testing.T, path, sessionID, cwd, model string, ts time.Time, input, cached, output, total int64) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	content := strings.Join([]string{
		fmt.Sprintf(`{"timestamp":%q,"type":"session_meta","payload":{"id":%q,"model_provider":"openai","cwd":%q,"base_instructions":"SECRET_BASE_INSTR"}}`, ts.Format(time.RFC3339), sessionID, cwd),
		fmt.Sprintf(`{"timestamp":%q,"type":"turn_context","payload":{"model":%q,"cwd":%q}}`, ts.Add(time.Second).Format(time.RFC3339), model, cwd),
		fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":%d,"cached_input_tokens":%d,"output_tokens":%d,"reasoning_output_tokens":0,"total_tokens":%d},"total_token_usage":{"input_tokens":%d,"cached_input_tokens":%d,"output_tokens":%d,"reasoning_output_tokens":0,"total_tokens":%d}}}}`,
			ts.Add(2*time.Second).Format(time.RFC3339), input, cached, output, total, input, cached, output, total),
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write codex jsonl %s: %v", path, err)
	}
}

func p3WriteClaudeJSONL(t *testing.T, path, sessionID, cwd, entrypoint, model string, ts time.Time, input, output, cacheRead, cacheWrite int64) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	epField := ""
	if entrypoint != "" {
		epField = fmt.Sprintf(`,"entrypoint":%q`, entrypoint)
	}
	content := strings.Join([]string{
		fmt.Sprintf(`{"type":"user","timestamp":%q,"sessionId":%q,"cwd":%q,"slug":"SECRET_SLUG"%s,"message":{"role":"user","content":"SECRET_CLAUDE_PROMPT"}}`, ts.Format(time.RFC3339), sessionID, cwd, epField),
		fmt.Sprintf(`{"type":"assistant","timestamp":%q,"sessionId":%q,"cwd":%q,"slug":"SECRET_SLUG"%s,"message":{"model":%q,"content":"SECRET_CLAUDE_RESPONSE","usage":{"input_tokens":%d,"output_tokens":%d,"cache_read_input_tokens":%d,"cache_creation_input_tokens":%d}}}`,
			ts.Add(time.Second).Format(time.RFC3339), sessionID, cwd, epField, model, input, output, cacheRead, cacheWrite),
	}, "\n") + "\n"
	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatalf("write claude jsonl %s: %v", path, err)
	}
}

func p3CaptureOverviewETag(t *testing.T, client *http.Client, baseURL string) (targetURL, etag string, headers map[string]string) {
	t.Helper()
	targetURL = baseURL + "/api/v1/analytics/overview"
	code, hdr, _ := p3Do(t, client, http.MethodGet, targetURL, nil, nil)
	if code != http.StatusOK {
		t.Fatalf("GET %s status = %d", targetURL, code)
	}
	if etag = hdr.Get("ETag"); etag != "" {
		return targetURL, etag, nil
	}
	targetURL = baseURL + "/"
	headers = map[string]string{"Accept": "text/html"}
	code2, hdr2, _ := p3Do(t, client, http.MethodGet, targetURL, nil, headers)
	if code2 != http.StatusOK {
		t.Fatalf("GET %s status = %d", targetURL, code2)
	}
	return targetURL, hdr2.Get("ETag"), headers
}

// =============================================================================
// Tier 3 Part A: Features 1-12 Paired Across Features 1-24 (12 Tests)
// =============================================================================

func TestPairwise_F1_F15_F17_SourceHealthGroupingWithAliasAndLiveRefresh(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "p3-f1-15-17"})
	now := time.Now().UTC()
	stale := now.Add(-2 * time.Hour)

	s.ingestEvents(t, []ingestEvent{{
		EventID:     "p3-f1-ev-1",
		MachineID:   "m-pw-health",
		SessionID:   "sess-pw-health-1",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(100),
	}})
	s.sendHeartbeat(t, "m-pw-health", "codex",
		heartbeatSource{Adapter: "codex", IdentityHash: "sha256:c1-" + strings.Repeat("a", 50), LastSuccessfulSync: &now},
		heartbeatSource{Adapter: "codex", IdentityHash: "sha256:c2-" + strings.Repeat("b", 50), LastSuccessfulSync: &now},
		heartbeatSource{Adapter: "codex", IdentityHash: "sha256:c3-" + strings.Repeat("c", 50), LastSuccessfulSync: &now},
		heartbeatSource{Adapter: "codex", IdentityHash: "sha256:c4-" + strings.Repeat("d", 50), LastSuccessfulSync: &stale},
		heartbeatSource{Adapter: "codex", IdentityHash: "sha256:c5-" + strings.Repeat("e", 50), LastSuccessfulSync: &now, LastErrorClass: "timeout"},
		heartbeatSource{Adapter: "claude-code", IdentityHash: "sha256:cc1-" + strings.Repeat("f", 49), LastSuccessfulSync: &now},
	)

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 100)

	codeAlias, _, _ := p3PostAliases(t, s, client, url.Values{
		"machine_identity": {"m-pw-health"},
		"machine_alias":    {"Lab Studio"},
	})
	if codeAlias != http.StatusSeeOther && codeAlias != http.StatusOK {
		t.Fatalf("POST /settings/aliases status = %d", codeAlias)
	}

	ovHTML := p3WaitHTMLContains(t, client, s.BaseURL+"/", "codex · 1 stale · 4 fresh")
	healthSec := p3SectionBetween(t, ovHTML, `<section class="panel health-panel"`, `</section>`)
	if !strings.Contains(healthSec, "Lab Studio") || !strings.Contains(healthSec, "timeout") {
		t.Fatalf("health-panel missing machine alias Lab Studio or timeout error summary: %s", healthSec)
	}
	if got := strings.Count(healthSec, `class="source-health-line"`); got != 2 {
		t.Fatalf("source-health-line count = %d, want 2: %s", got, healthSec)
	}
	if !strings.Contains(healthSec, `<details class="source-details">`) || strings.Contains(healthSec, `<details class="source-details" open`) {
		t.Fatalf("expected collapsed <details class=\"source-details\">: %s", healthSec)
	}
	overviewAssertNoAutoRefreshReload(t, ovHTML)

	etagTarget, etag, baseHdrs := p3CaptureOverviewETag(t, client, s.BaseURL)
	if etag != "" {
		condHdrs := map[string]string{"If-None-Match": etag}
		for k, v := range baseHdrs {
			condHdrs[k] = v
		}
		c304, _, _ := p3Do(t, client, http.MethodGet, etagTarget, nil, condHdrs)
		if c304 != http.StatusNotModified {
			t.Fatalf("conditional GET %s status = %d, want 304", etagTarget, c304)
		}
	} else if !strings.Contains(ovHTML, "fetch(") && !strings.Contains(ovHTML, "EventSource(") {
		t.Fatalf("/ HTML missing ETag and missing fetch(/EventSource( in-place update script")
	}

	// Second heartbeat clears timeout and adds claude-desktop; ingest 200 more tokens.
	s.sendHeartbeat(t, "m-pw-health", "codex",
		heartbeatSource{Adapter: "codex", IdentityHash: "sha256:c1-" + strings.Repeat("a", 50), LastSuccessfulSync: &now},
		heartbeatSource{Adapter: "codex", IdentityHash: "sha256:c2-" + strings.Repeat("b", 50), LastSuccessfulSync: &now},
		heartbeatSource{Adapter: "codex", IdentityHash: "sha256:c3-" + strings.Repeat("c", 50), LastSuccessfulSync: &now},
		heartbeatSource{Adapter: "codex", IdentityHash: "sha256:c4-" + strings.Repeat("d", 50), LastSuccessfulSync: &stale},
		heartbeatSource{Adapter: "codex", IdentityHash: "sha256:c5-" + strings.Repeat("e", 50), LastSuccessfulSync: &now},
		heartbeatSource{Adapter: "claude-desktop", IdentityHash: "sha256:cd1-" + strings.Repeat("9", 49), LastSuccessfulSync: &now},
	)
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "p3-f1-ev-2",
		MachineID:   "m-pw-health",
		SessionID:   "sess-pw-health-2",
		Provider:    "anthropic",
		Model:       "claude-sonnet-4",
		Tool:        "claude-desktop",
		TotalTokens: i64(200),
	}})

	waitOverviewLifetimeTokens(t, client, s.BaseURL, 300)
	if etag != "" {
		condHdrs := map[string]string{"If-None-Match": etag}
		for k, v := range baseHdrs {
			condHdrs[k] = v
		}
		deadline := time.Now().Add(4 * time.Second)
		for {
			cNew, _, _ := p3Do(t, client, http.MethodGet, etagTarget, nil, condHdrs)
			if cNew == http.StatusOK {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("conditional GET %s stuck at %d after new ingest", etagTarget, cNew)
			}
			time.Sleep(100 * time.Millisecond)
		}
	}

	ovHTML2 := p3WaitHTMLContains(t, client, s.BaseURL+"/", "claude-desktop · ")
	healthSec2 := p3SectionBetween(t, ovHTML2, `<section class="panel health-panel"`, `</section>`)
	if !strings.Contains(healthSec2, "Lab Studio") || !strings.Contains(healthSec2, "codex · 1 stale · 4 fresh") || !strings.Contains(healthSec2, "claude-desktop · ") || !strings.Contains(healthSec2, "ago") || !strings.Contains(healthSec2, `<time datetime="`) {
		t.Fatalf("updated health-panel missing expected alias, updated codex summary, claude-desktop relative age, or <time datetime=...>: %s", healthSec2)
	}
	if strings.Contains(healthSec2, "timeout") {
		t.Fatalf("healthSec2 unexpectedly retained cleared timeout error: %s", healthSec2)
	}
	if strings.Contains(healthSec2, `<details class="source-details" open`) {
		t.Fatalf("source-details unexpectedly expanded after live update")
	}
}

func TestPairwise_F2_F13_F14_F20_MachineAliasesDrillDownPillsAndCSV(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "p3-f2-13-14-20"})
	s.ingestEvents(t, []ingestEvent{
		{
			EventID:         "p3-f2-ev-1",
			MachineID:       "m-pw-mac",
			SessionID:       "s-pw-mac",
			Provider:        "openai",
			Model:           "gpt-5.4",
			Tool:            "codex",
			InputTokens:     i64(1_000_000),
			CacheReadTokens: i64(2_000_000),
			OutputTokens:    i64(200_000),
			TotalTokens:     i64(3_200_000),
		},
		{
			EventID:         "p3-f2-ev-2",
			MachineID:       "m-pw-linux",
			SessionID:       "s-pw-linux",
			Provider:        "anthropic",
			Model:           "claude-sonnet-4",
			Tool:            "claude-code",
			InputTokens:     i64(500_000),
			CacheReadTokens: i64(500_000),
			OutputTokens:    i64(1_000_000),
			TotalTokens:     i64(2_000_000),
		},
	})

	client := s.authClient(t)
	p3WaitAnalyticsTokens(t, client, s.BaseURL+"/api/v1/analytics?period=30d", 5_200_000)

	code, _, _ := p3PostAliases(t, s, client, url.Values{
		"model_identity":   {"m-pw-mac"},
		"model_alias":      {"WRONG_MODEL_ALIAS"},
		"machine_identity": {"m-pw-mac", "m-pw-linux"},
		"machine_alias":    {"Mac Fleet", "Linux Fleet"},
	})
	if code != http.StatusSeeOther && code != http.StatusOK {
		t.Fatalf("POST /settings/aliases status = %d", code)
	}

	filteredHTML := p3GetHTML(t, client, s.BaseURL+"/analytics?period=30d&dimension=machines&model=gpt-5.4")
	if p3DupDimSelectRe.MatchString(filteredHTML) {
		t.Fatalf("duplicate <select name=\"dimension\"> found in /analytics")
	}
	detailsSeg := p3SectionBetween(t, filteredHTML, `<details class="filter-details"`, `</details>`)
	filterForm := p3SectionBetween(t, detailsSeg, `<form class="filter-form"`, `</form>`)
	if !strings.Contains(filterForm, `<input type="hidden" name="dimension" value="machines">`) {
		t.Fatalf("filter form missing hidden dimension=machines: %s", filterForm)
	}
	summarySeg := p3SectionBetween(t, detailsSeg, `<summary`, `</summary>`)
	if !strings.Contains(summarySeg, "1") {
		t.Fatalf("expected active filter count 1 in <summary>: %s", summarySeg)
	}

	bd := p3SectionBetween(t, filteredHTML, `class="panel breakdown-panel"`, `class="panel share-panel"`)
	share := p3SectionBetween(t, filteredHTML, `class="panel share-panel"`, `class="panel sessions-panel"`)
	for _, seg := range []string{bd, share} {
		if !strings.Contains(seg, "Mac Fleet") {
			t.Fatalf("expected Mac Fleet in section: %s", seg)
		}
		if strings.Contains(seg, "WRONG_MODEL_ALIAS") || strings.Contains(seg, "Linux Fleet") {
			t.Fatalf("section leaked WRONG_MODEL_ALIAS or filtered-out Linux Fleet: %s", seg)
		}
	}
	for _, want := range []string{"$4.50 saved", "$6.00", "/1M"} {
		if !strings.Contains(filteredHTML, want) {
			t.Fatalf("filtered /analytics missing %q", want)
		}
	}

	filterPanel := p3SectionBetween(t, filteredHTML, `class="panel filter-panel"`, `class="stats"`)
	var pillDropModel string
	for _, h := range p3ExtractHrefs(filterPanel) {
		if !strings.HasPrefix(h, "/analytics") {
			continue
		}
		u, err := url.Parse(h)
		if err != nil {
			t.Fatalf("parse pill href %q: %v", h, err)
		}
		if u.Query().Get("model") == "" && u.Query().Get("dimension") == "machines" {
			pillDropModel = h
			break
		}
	}
	if pillDropModel == "" {
		t.Fatalf("missing active filter pill link removing model=gpt-5.4 while preserving dimension=machines: %s", filterPanel)
	}

	unfilteredHTML := p3GetHTML(t, client, s.BaseURL+"/analytics?period=30d&dimension=machines")
	bdAll := p3SectionBetween(t, unfilteredHTML, `class="panel breakdown-panel"`, `class="panel share-panel"`)
	var macDrillHref string
	for _, h := range p3ExtractHrefs(bdAll) {
		u, err := url.Parse(h)
		if err == nil && u.Path == "/analytics" && u.Query().Get("machine") == "m-pw-mac" {
			macDrillHref = h
			break
		}
	}
	if macDrillHref == "" {
		t.Fatalf("missing machine=m-pw-mac drill-down href in %s", bdAll)
	}
	drilledHTML := p3GetHTML(t, client, s.BaseURL+macDrillHref)
	if !strings.Contains(drilledHTML, "$4.50 saved") || !strings.Contains(drilledHTML, "$6.00") {
		t.Fatalf("drilled /analytics missing $4.50 saved / $6.00")
	}
	if strings.Contains(analyticsStripForms(drilledHTML), "Linux Fleet") {
		t.Fatalf("drilled machine=m-pw-mac page leaked Linux Fleet outside forms")
	}

	// Verify export link preserves download attribute and emits raw machine ID in CSV
	// on the machines-dimension view (drilledHTML switches dimension to projects).
	var csvHref string
	exportRe := regexp.MustCompile(`<a\b[^>]*href="([^"]*export[^"]*)"[^>]*>`)
	for _, m := range exportRe.FindAllStringSubmatch(filteredHTML, -1) {
		tag := m[0]
		href := stdhtml.UnescapeString(m[1])
		if strings.Contains(href, "format=csv") && strings.Contains(tag, "download") {
			csvHref = href
			break
		}
	}
	if csvHref == "" {
		t.Fatalf("missing <a href=\"...export...format=csv\" download> link on filtered /analytics")
	}
	cExp, hExp, bExp := p3Do(t, client, http.MethodGet, s.BaseURL+csvHref, nil, nil)
	if cExp != http.StatusOK {
		t.Fatalf("GET %s status = %d", csvHref, cExp)
	}
	if !strings.Contains(hExp.Get("Content-Disposition"), "attachment;") {
		t.Fatalf("CSV export missing attachment Content-Disposition: %q", hExp.Get("Content-Disposition"))
	}
	recs, err := csv.NewReader(bytes.NewReader(bExp)).ReadAll()
	if err != nil || len(recs) < 2 {
		t.Fatalf("invalid CSV export (%v): %s", err, string(bExp))
	}
	if !strings.Contains(string(bExp), "m-pw-mac") || strings.Contains(string(bExp), "Mac Fleet") {
		t.Fatalf("CSV export must contain raw m-pw-mac (not alias Mac Fleet): %s", string(bExp))
	}
}

func TestPairwise_F3_F16_F23_TimezoneLocalizedTimeStreaksAndHeatmapDrill(t *testing.T) {
	requireE2E(t)
	laLoc := p3LoadLoc(t, "America/Los_Angeles")
	s := startTestServer(t, serverOpts{IngestToken: "p3-f3-16-23", Timezone: "America/Los_Angeles"})

	nowLA := time.Now().In(laLoc)
	laToday := time.Date(nowLA.Year(), nowLA.Month(), nowLA.Day(), 0, 0, 0, 0, laLoc)
	tsD2 := time.Date(laToday.AddDate(0, 0, -2).Year(), laToday.AddDate(0, 0, -2).Month(), laToday.AddDate(0, 0, -2).Day(), 23, 15, 0, 0, laLoc)
	tsD1 := time.Date(laToday.AddDate(0, 0, -1).Year(), laToday.AddDate(0, 0, -1).Month(), laToday.AddDate(0, 0, -1).Day(), 23, 15, 0, 0, laLoc)
	tsD0 := nowLA.Add(-10 * time.Minute).Truncate(time.Second)
	if tsD0.In(laLoc).Format("2006-01-02") != laToday.Format("2006-01-02") {
		tsD0 = laToday
	}

	laD1 := tsD1.In(laLoc).Format("2006-01-02")
	utcD1 := tsD1.UTC().Format("2006-01-02")
	if laD1 == utcD1 {
		t.Fatalf("setup invariant failed: LA date %s == UTC date %s", laD1, utcD1)
	}

	s.ingestEvents(t, []ingestEvent{
		{EventID: "p3-f3-d2", Timestamp: tsD2.UTC().Format(time.RFC3339Nano), MachineID: "m-la", SessionID: "sess-la-d2", Project: "proj-la", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(1000)},
		{EventID: "p3-f3-d1", Timestamp: tsD1.UTC().Format(time.RFC3339Nano), MachineID: "m-la", SessionID: "sess-la-d1", Project: "proj-la", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(5000)},
		{EventID: "p3-f3-d0", Timestamp: tsD0.UTC().Format(time.RFC3339Nano), MachineID: "m-la", SessionID: "sess-la-d0", Project: "proj-la", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(2000)},
	})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 8000)

	var ov overviewPayload
	getJSONWithClient(t, client, s.BaseURL+"/api/v1/analytics/overview", &ov)
	if ov.Timezone != "America/Los_Angeles" || ov.Activity.CurrentStreak != 3 || ov.Activity.LongestStreak != 3 || ov.Activity.MaxDailyTokens != 5000 {
		t.Fatalf("unexpected overview tz/streak: tz=%q act=%+v", ov.Timezone, ov.Activity)
	}
	if gotPeak := overviewParsePeakDayDate(ov.Activity.PeakDay); gotPeak != laD1 {
		t.Fatalf("peak_day = %q, want LA date %q (not UTC %q)", gotPeak, laD1, utcD1)
	}

	ovHTML := p3WaitHTMLContains(t, client, s.BaseURL+"/", fmt.Sprintf(`data-date="%s"`, laD1))
	actHeader := overviewStripActivityLegend(p3SectionBetween(t, ovHTML, `<section class="panel activity-panel"`, `<div class="activity-shell">`))
	if !overviewHasStreakValue(actHeader, 3) || (!strings.Contains(actHeader, "5,000") && !strings.Contains(actHeader, "5K")) {
		t.Fatalf("activity header missing streak 3 or peak 5,000/5K: %s", actHeader)
	}
	sessionsSec := p3SectionBetween(t, ovHTML, `<section class="panel sessions-panel"`, `</section>`)
	times := ssExtractTimeElems(sessionsSec)
	if len(times) == 0 {
		t.Fatalf("sessions-panel missing <time> elements")
	}
	wantDT := tsD0.In(laLoc).Format(time.RFC3339)
	if !strings.Contains(sessionsSec, fmt.Sprintf(`datetime="%s"`, wantDT)) {
		t.Fatalf("sessions-panel missing LA datetime %q: %s", wantDT, sessionsSec)
	}
	if !strings.Contains(times[0].Title, "PST") && !strings.Contains(times[0].Title, "PDT") {
		t.Fatalf("sessions-panel <time title=%q> missing PST/PDT", times[0].Title)
	}
	if regexp.MustCompile(`-\d+[mhd] ago`).MatchString(sessionsSec) {
		t.Fatalf("negative relative age rendered in %s", sessionsSec)
	}

	// Follow heatmap day filter to /sessions & /api/v1/sessions.
	var dayRows []ssSessionRow
	getJSONWithClient(t, client, fmt.Sprintf("%s/api/v1/sessions?from=%s&to=%s", s.BaseURL, laD1, laD1), &dayRows)
	if len(dayRows) != 1 || dayRows[0].SessionID != "sess-la-d1" {
		t.Fatalf("expected only sess-la-d1 for LA day %s, got %+v", laD1, dayRows)
	}
	dayHTML := p3GetHTML(t, client, fmt.Sprintf("%s/sessions?from=%s&to=%s", s.BaseURL, laD1, laD1))
	if !strings.Contains(dayHTML, "sess-la-d1") || strings.Contains(dayHTML, "sess-la-d2") || strings.Contains(dayHTML, "sess-la-d0") {
		t.Fatalf("day-filtered /sessions?from=%s&to=%s did not isolate sess-la-d1", laD1, laD1)
	}

	detHTML := p3GetHTML(t, client, s.BaseURL+"/sessions/sess-la-d1")
	wantD1DT := tsD1.In(laLoc).Format(time.RFC3339)
	if !strings.Contains(detHTML, wantD1DT) {
		t.Fatalf("/sessions/sess-la-d1 missing LA datetime %q", wantD1DT)
	}
	if !strings.Contains(detHTML, "5,000") && !strings.Contains(detHTML, "5K") {
		t.Fatalf("/sessions/sess-la-d1 missing 5,000/5K tokens")
	}
}

func TestPairwise_F4_F10_F15_PreRenderedCompositionEvolutionSWRAndAutoPause(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "p3-f4-10-15"})
	s.ingestEvents(t, []ingestEvent{{
		EventID:      "p3-f4-1",
		MachineID:    "m-evo-comp",
		SessionID:    "s-evo-comp-1",
		Provider:     "openai",
		Model:        "gpt-5.4",
		Tool:         "codex",
		InputTokens:  i64(15),
		OutputTokens: i64(5),
		TotalTokens:  i64(20),
	}})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 20)
	p3WaitEvolution(t, client, s.BaseURL, func(ev evolutionPayload) bool {
		return ev.LifetimeTokens == 20 && ev.Stage == 1
	})

	html1 := p3WaitHTMLContains(t, client, s.BaseURL+"/", `id="live-input-tokens" data-display="15"`)
	comp := p3SectionBetween(t, html1, `id="token-composition"`, `class="progress-block"`)
	for _, want := range []string{
		`id="live-input-tokens" data-display="15"`,
		`id="live-output-tokens" data-display="5"`,
		`id="live-input-share">75%</span>`,
		`id="live-output-share">25%</span>`,
	} {
		if !strings.Contains(comp, want) {
			t.Fatalf("pre-rendered composition missing %q: %s", want, comp)
		}
	}
	if strings.Contains(html1, "Usage totals are loading") {
		t.Fatalf("unexpected loading placeholder on /")
	}
	cachedTag := regexp.MustCompile(`<span[^>]*id="live-cached-segment"[^>]*>`).FindString(comp)
	if !strings.Contains(cachedTag, " hidden") {
		t.Fatalf("expected live-cached-segment hidden when cached==0, got %s", cachedTag)
	}
	autoPausedIdx := strings.Index(html1, "const autoPaused =")
	pollLifetimeIdx := strings.Index(html1, "const pollLifetime =")
	if autoPausedIdx < 0 || pollLifetimeIdx < 0 || autoPausedIdx > pollLifetimeIdx {
		t.Fatalf("expected const autoPaused before const pollLifetime")
	}
	if !strings.Contains(html1[pollLifetimeIdx:], "autoPaused()") {
		t.Fatalf("pollLifetime must honor autoPaused()")
	}
	overviewAssertNoAutoRefreshReload(t, html1)
	if !strings.Contains(html1, `id="refresh-now"`) || !strings.Contains(html1, `id="auto-refresh-toggle"`) {
		t.Fatalf("missing live refresh controls on /")
	}

	// Burst ingest crossing Stage 2 (Bytelet >= 100) with cached tokens.
	s.ingestEvents(t, []ingestEvent{
		{EventID: "p3-f4-2a", MachineID: "m-evo-comp", Provider: "openai", Model: "gpt-5.4", Tool: "codex", InputTokens: i64(20), CacheReadTokens: i64(20), OutputTokens: i64(5), TotalTokens: i64(45)},
		{EventID: "p3-f4-2b", MachineID: "m-evo-comp", Provider: "openai", Model: "gpt-5.4", Tool: "codex", InputTokens: i64(10), CacheReadTokens: i64(20), OutputTokens: i64(15), TotalTokens: i64(45)},
	})

	// Immediate non-blocking SWR read.
	var imm evolutionPayload
	getJSONWithClient(t, client, s.BaseURL+"/api/v1/evolution", &imm)

	ev2 := p3WaitEvolution(t, client, s.BaseURL, func(ev evolutionPayload) bool {
		return ev.LifetimeTokens == 110 && ev.Stage == 2 && ev.Form == "bytelet"
	})
	if ev2.Composition.InputTokens != 85 || ev2.Composition.CachedInputTokens != 40 || ev2.Composition.OutputTokens != 25 {
		t.Fatalf("unexpected Stage 2 evolution composition: %+v", ev2.Composition)
	}
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 110)

	html2 := p3WaitHTMLContains(t, client, s.BaseURL+"/", `data-stage="2"`)
	for _, want := range []string{`stage-02.png`, `data-display="85"`, `data-display="25"`} {
		if !strings.Contains(html2, want) {
			t.Fatalf("updated / HTML missing %q", want)
		}
	}
	cachedTag2 := regexp.MustCompile(`<span[^>]*id="live-cached-segment"[^>]*>`).FindString(html2)
	if strings.Contains(cachedTag2, " hidden") {
		t.Fatalf("live-cached-segment remained hidden after cached ingest: %s", cachedTag2)
	}
}

func TestPairwise_F5_F6_F18_F19_CodexDedupGuardWALConcurrentAndRestartBackup(t *testing.T) {
	requireE2E(t)
	workDir := t.TempDir()
	dbPath := filepath.Join(workDir, "pw5.db")
	bakDir := filepath.Join(workDir, "backups")
	agentHome := filepath.Join(workDir, "agent-home")
	stateDB := filepath.Join(workDir, "state.db")

	s1 := startTestServer(t, serverOpts{
		IngestToken:    "p3-f5-6-18-19",
		DBPath:         dbPath,
		BackupDir:      bakDir,
		BackupInterval: "0",
	})
	baseTS := time.Now().UTC().Add(-2 * time.Hour)
	activePath := filepath.Join(agentHome, ".codex", "sessions", "2026", "11", "20", "rollout-pw5.jsonl")
	p3WriteCodexJSONL(t, activePath, "codex-pw5-1", "/Users/alice/pw5-proj", "gpt-5.4", baseTS, 800, 400, 200, 1000)

	if _, stderr, err := runAgentWithArgs(t, s1, agentHome, stateDB, "m-pw5", "codex"); err != nil {
		t.Fatalf("initial codex agent run failed: %v\nstderr: %s", err, stderr)
	}
	c1 := s1.authClient(t)
	waitOverviewLifetimeTokens(t, c1, s1.BaseURL, 1000)
	if _, err := os.Stat(dbPath + "-wal"); err != nil {
		t.Fatalf("expected WAL sidecar at %s-wal: %v", dbPath, err)
	}

	// Move rollout to archived .jsonl.zst with turn 1 (same timestamp/session) + turn 2 (+400 tokens).
	archPayload := strings.Join([]string{
		fmt.Sprintf(`{"timestamp":%q,"type":"session_meta","payload":{"id":"codex-pw5-1","model_provider":"openai","cwd":"/Users/alice/pw5-proj"}}`, baseTS.Format(time.RFC3339)),
		fmt.Sprintf(`{"timestamp":%q,"type":"turn_context","payload":{"model":"gpt-5.4","cwd":"/Users/alice/pw5-proj"}}`, baseTS.Add(time.Second).Format(time.RFC3339)),
		fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":800,"cached_input_tokens":400,"output_tokens":200,"reasoning_output_tokens":0,"total_tokens":1000},"total_token_usage":{"input_tokens":800,"cached_input_tokens":400,"output_tokens":200,"reasoning_output_tokens":0,"total_tokens":1000}}}}`, baseTS.Add(2*time.Second).Format(time.RFC3339)),
		fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":300,"cached_input_tokens":100,"output_tokens":100,"reasoning_output_tokens":0,"total_tokens":400},"total_token_usage":{"input_tokens":1100,"cached_input_tokens":500,"output_tokens":300,"reasoning_output_tokens":0,"total_tokens":1400}}}}`, baseTS.Add(10*time.Second).Format(time.RFC3339)),
	}, "\n") + "\n"
	zstPath := filepath.Join(agentHome, ".codex", "archived_sessions", "rollout-pw5.jsonl.zst")
	writeRawZstdFrame(t, zstPath, []byte(archPayload))
	if err := os.Remove(activePath); err != nil {
		t.Fatalf("remove active rollout: %v", err)
	}

	// Concurrent WAL reads while archived zst ingests.
	var wg sync.WaitGroup
	errCh := make(chan string, 32)
	for i := 0; i < 6; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, p := range []string{"/", "/analytics", "/sessions", "/api/v1/evolution"} {
				code, _, body, err := p3DoErr(c1, http.MethodGet, s1.BaseURL+p, nil, nil)
				if err != nil {
					errCh <- fmt.Sprintf("concurrent GET %s error: %v", p, err)
					return
				}
				if code != http.StatusOK {
					errCh <- fmt.Sprintf("concurrent GET %s -> %d: %s", p, code, string(body))
					return
				}
			}
		}()
	}
	if _, stderr, err := runAgentWithArgs(t, s1, agentHome, stateDB, "m-pw5", "codex"); err != nil {
		t.Fatalf("archived zst agent run failed: %v\nstderr: %s", err, stderr)
	}
	wg.Wait()
	close(errCh)
	for msg := range errCh {
		t.Fatal(msg)
	}
	waitOverviewLifetimeTokens(t, c1, s1.BaseURL, 1400)

	bOut, bErr, err := s1.runCLI(t, nil, "backup", "create", "--destination", bakDir, "--database", dbPath)
	if err != nil {
		t.Fatalf("online backup create failed: %v\nstderr: %s", err, bErr)
	}
	var bakRes struct {
		Path           string `json:"path"`
		LifetimeTokens int64  `json:"lifetime_tokens"`
	}
	if err := json.Unmarshal([]byte(bOut), &bakRes); err != nil {
		t.Fatalf("decode backup result: %v (%s)", err, bOut)
	}
	if bakRes.LifetimeTokens != 1400 {
		t.Fatalf("backup lifetime_tokens = %d, want 1400", bakRes.LifetimeTokens)
	}
	s1.Stop()

	for cycle := 2; cycle <= 3; cycle++ {
		sR := startTestServer(t, serverOpts{
			IngestToken:    "p3-f5-6-18-19",
			DBPath:         dbPath,
			BackupDir:      bakDir,
			BackupInterval: "0",
		})
		var ov overviewPayload
		getJSONWithClient(t, sR.authClient(t), sR.BaseURL+"/api/v1/analytics/overview", &ov)
		if ov.LifetimeTokens != 1400 {
			t.Fatalf("restart cycle %d lifetime_tokens = %d, want 1400", cycle, ov.LifetimeTokens)
		}
		stOut, stErr, err := sR.runCLI(t, nil, "status", "--server", sR.BaseURL, "--token", sR.IngestToken, "--json")
		if err != nil {
			t.Fatalf("tokemon status --json cycle %d failed: %v\nstderr: %s", cycle, err, stErr)
		}
		var stAny any
		if err := json.Unmarshal([]byte(stOut), &stAny); err != nil || !jsonContainsNumber(stAny, 1400) {
			t.Fatalf("tokemon status --json cycle %d missing 1400: %s (%v)", cycle, stOut, err)
		}
		migFiles, err := filepath.Glob(filepath.Join(bakDir, "*-before-v*.db"))
		if err != nil {
			t.Fatalf("glob migration backups: %v", err)
		}
		if len(migFiles) != 0 {
			t.Fatalf("unexpected spurious migration backups on restart %d: %v", cycle, migFiles)
		}
		if _, stderr, err := runAgentWithArgs(t, sR, agentHome, stateDB, "m-pw5", "codex"); err != nil {
			t.Fatalf("re-run agent cycle %d failed: %v\nstderr: %s", cycle, err, stderr)
		}
		waitOverviewLifetimeTokens(t, sR.authClient(t), sR.BaseURL, 1400)
		sR.Stop()
	}

	vOut, vErr, err := s1.runCLI(t, nil, "backup", "verify", bakRes.Path)
	if err != nil {
		t.Fatalf("backup verify failed: %v\nstderr: %s", err, vErr)
	}
	if !strings.Contains(vOut, `"lifetime_tokens":1400`) {
		t.Fatalf("backup verify missing lifetime_tokens 1400: %s", vOut)
	}
}

func TestPairwise_F7_F8_F18_F23_IncrementalFacetsStreamingAnalyticsCustomFromToAndClaudeDesktop(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "p3-f7-8-18-23", Timezone: "UTC"})
	client := s.authClient(t)

	// Warm caches while DB is empty.
	p3GetHTML(t, client, s.BaseURL+"/sessions")
	var emptyAna analyticsAPIPayload
	getJSONWithClient(t, client, s.BaseURL+"/api/v1/analytics", &emptyAna)

	now := time.Now().UTC()
	d5TS := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.UTC).AddDate(0, 0, -5)
	d1TS := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.UTC).AddDate(0, 0, -1)
	d5 := d5TS.Format("2006-01-02")
	d1 := d1TS.Format("2006-01-02")

	agentHome := filepath.Join(t.TempDir(), "agent-home")
	stateDB := filepath.Join(t.TempDir(), "state.db")

	zstPayload := strings.Join([]string{
		fmt.Sprintf(`{"timestamp":%q,"type":"session_meta","payload":{"id":"pw6-cdx-1","model_provider":"openai","cwd":"/Users/alice/proj-zst"}}`, d5TS.Format(time.RFC3339)),
		fmt.Sprintf(`{"timestamp":%q,"type":"turn_context","payload":{"model":"gpt-5.4","cwd":"/Users/alice/proj-zst"}}`, d5TS.Add(time.Second).Format(time.RFC3339)),
		fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":500,"cached_input_tokens":100,"output_tokens":100,"reasoning_output_tokens":0,"total_tokens":600},"total_token_usage":{"input_tokens":500,"cached_input_tokens":100,"output_tokens":100,"reasoning_output_tokens":0,"total_tokens":600}}}}`, d5TS.Add(2*time.Second).Format(time.RFC3339)),
	}, "\n") + "\n"
	writeRawZstdFrame(t, filepath.Join(agentHome, ".codex", "archived_sessions", "pw6.jsonl.zst"), []byte(zstPayload))

	claudeDir := filepath.Join(agentHome, ".claude", "projects", "-Users-alice-proj-desk")
	p3WriteClaudeJSONL(t, filepath.Join(claudeDir, "desk.jsonl"), "pw6-desk-1", "/Users/alice/proj-desk", "claude-desktop", "claude-sonnet-4", d1TS, 300, 100, 0, 0)
	p3WriteClaudeJSONL(t, filepath.Join(claudeDir, "cli.jsonl"), "pw6-cli-1", "/Users/alice/proj-desk", "cli", "claude-sonnet-4", d1TS.Add(5*time.Minute), 80, 20, 0, 0)

	if _, stderr, err := runAgentWithArgs(t, s, agentHome, stateDB, "m-pw6", "codex,claude-code"); err != nil {
		t.Fatalf("agent --once failed: %v\nstderr: %s", err, stderr)
	}

	// Immediate cold-key query (no 60s TTL wait).
	rangeURL := fmt.Sprintf("%s/api/v1/analytics?from=%s&to=%s&dimension=harnesses", s.BaseURL, d5, d1)
	ana := p3WaitAnalyticsTokens(t, client, rangeURL, 1100)
	var facets analyticsFacetsPayload
	getJSONWithClient(t, client, rangeURL, &facets)
	if !containsAll(facets.Facets.Tools, "codex", "claude-desktop", "claude-code") || !containsAll(facets.Facets.Projects, "proj-zst", "proj-desk") {
		t.Fatalf("incremental facets missing expected tools/projects: %+v", facets.Facets)
	}

	var ptsSum, bdSum int64
	for _, pt := range ana.Points {
		ptsSum += pt.Tokens
	}
	byTool := make(map[string]int64)
	for _, b := range ana.Breakdown {
		bdSum += b.Tokens
		byTool[b.Name] = b.Tokens
	}
	if ptsSum != 1100 || bdSum != 1100 || byTool["codex"] != 600 || byTool["claude-desktop"] != 400 || byTool["claude-code"] != 100 {
		t.Fatalf("streaming analytics inconsistency: pts=%d bd=%d byTool=%v", ptsSum, bdSum, byTool)
	}

	sessHTML := p3GetHTML(t, client, fmt.Sprintf("%s/sessions?from=%s&to=%s", s.BaseURL, d5, d1))
	for _, opt := range []string{"claude-desktop", "proj-zst", "proj-desk"} {
		if !strings.Contains(sessHTML, fmt.Sprintf(`value="%s"`, opt)) {
			t.Fatalf("/sessions HTML missing immediate facet option %q", opt)
		}
	}

	// Narrow range to d1 only.
	d1URL := fmt.Sprintf("%s/api/v1/analytics?from=%s&to=%s&dimension=harnesses", s.BaseURL, d1, d1)
	anaD1 := p3WaitAnalyticsTokens(t, client, d1URL, 500)
	for _, b := range anaD1.Breakdown {
		if b.Name == "codex" {
			t.Fatalf("codex from %s leaked into %s breakdown: %+v", d5, d1, anaD1.Breakdown)
		}
	}
	d1HTML := p3GetHTML(t, client, fmt.Sprintf("%s/analytics?from=%s&to=%s&dimension=harnesses", s.BaseURL, d1, d1))
	if !strings.Contains(d1HTML, "Claude Desktop") {
		t.Fatalf("/analytics missing Claude Desktop display mapping")
	}
	if !strings.Contains(d1HTML, "from="+d1) || !strings.Contains(d1HTML, "to="+d1) {
		t.Fatalf("/analytics export/nav links missing custom from=%s&to=%s", d1, d1)
	}
}

func TestPairwise_F9_F13_F16_OverviewLimit8MachinesDedupAndSessionsDrillDown(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "p3-f9-13-16"})
	now := time.Now().UTC()
	events := make([]ingestEvent, 0, 105)
	for i := 0; i < 105; i++ {
		machine := "m-top"
		toks := int64(20)
		proj := "proj-alpha"
		if i >= 60 && i < 90 {
			machine = "m-mid"
			toks = 10
			proj = "proj-beta"
		} else if i >= 90 {
			machine = "m-low"
			toks = 5
			proj = "proj-beta"
		}
		events = append(events, ingestEvent{
			EventID:     fmt.Sprintf("p3-f9-ev-%03d", i),
			Timestamp:   now.Add(-time.Duration(105-i) * time.Minute).Format(time.RFC3339Nano),
			MachineID:   machine,
			SessionID:   fmt.Sprintf("sess-pw7-%03d", i),
			Project:     proj,
			Provider:    "openai",
			Model:       "gpt-5.4",
			Tool:        "codex",
			TotalTokens: i64(toks),
		})
	}
	s.ingestEvents(t, events)

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 1575)

	var ov overviewPayload
	getJSONWithClient(t, client, s.BaseURL+"/api/v1/analytics/overview", &ov)
	var machines []struct {
		ID             string `json:"id"`
		LifetimeTokens int64  `json:"lifetime_tokens"`
	}
	getJSONWithClient(t, client, s.BaseURL+"/api/v1/machines", &machines)
	if len(ov.ByMachine) != 3 || ov.ByMachine[0].Machine != "m-top" || ov.ByMachine[0].Tokens != 1200 ||
		ov.ByMachine[1].Machine != "m-mid" || ov.ByMachine[1].Tokens != 300 ||
		ov.ByMachine[2].Machine != "m-low" || ov.ByMachine[2].Tokens != 75 {
		t.Fatalf("unexpected overview by_machine order/totals: %+v", ov.ByMachine)
	}
	machineMap := make(map[string]int64)
	for _, m := range machines {
		machineMap[m.ID] = m.LifetimeTokens
	}
	for _, bm := range ov.ByMachine {
		if machineMap[bm.Machine] != bm.Tokens {
			t.Fatalf("ByMachine[%q]=%d != /api/v1/machines %d", bm.Machine, bm.Tokens, machineMap[bm.Machine])
		}
	}

	ovHTML := p3WaitHTMLContains(t, client, s.BaseURL+"/", "sess-pw7-104")
	sessSec := p3SectionBetween(t, ovHTML, `<section class="panel sessions-panel"`, `</section>`)
	if got := strings.Count(sessSec, `data-label="When"`); got != 8 {
		t.Fatalf("overview recent sessions row count = %d, want 8", got)
	}
	for i := 97; i <= 104; i++ {
		if !strings.Contains(sessSec, fmt.Sprintf("sess-pw7-%03d", i)) {
			t.Fatalf("overview missing newest session sess-pw7-%03d", i)
		}
	}
	if strings.Contains(sessSec, "sess-pw7-096") || strings.Contains(sessSec, "sess-pw7-000") {
		t.Fatalf("overview recent sessions leaked rows beyond LIMIT 8")
	}

	// Follow Machine token usage drill-down for m-top.
	machTbl := p3SectionBetween(t, ovHTML, `<table aria-label="Machine token usage">`, `</table>`)
	var mTopHref string
	for _, h := range p3ExtractHrefs(machTbl) {
		if strings.Contains(h, "machine=m-top") {
			mTopHref = h
			break
		}
	}
	if mTopHref == "" {
		t.Fatalf("missing machine=m-top drill-down href in %s", machTbl)
	}
	mTopHTML := p3GetHTML(t, client, s.BaseURL+mTopHref)
	if !strings.Contains(mTopHTML, "1,200") {
		t.Fatalf("drilled m-top page missing 1,200")
	}
	mTopNoForms := analyticsStripForms(mTopHTML)
	if strings.Contains(mTopNoForms, "m-mid") || strings.Contains(mTopNoForms, "m-low") {
		t.Fatalf("drilled m-top page leaked m-mid/m-low outside forms")
	}

	// Pagination >100 and search q to /sessions/<id>.
	page1HTML := p3GetHTML(t, client, s.BaseURL+"/sessions?period=all")
	if strings.Contains(ssTableBodyRe.FindString(page1HTML), "sess-pw7-000") {
		t.Fatalf("oldest row sess-pw7-000 unexpectedly appeared on /sessions page 1")
	}
	curHTML := page1HTML
	foundOldest := false
	for nextPage := 2; nextPage <= 10; nextPage++ {
		re := regexp.MustCompile(fmt.Sprintf(`href="([^"]*(?:\?|&(?:amp;)?)page=%d(?:&[^"]*)?)"`, nextPage))
		m := re.FindStringSubmatch(curHTML)
		if len(m) < 2 {
			break
		}
		href := stdhtml.UnescapeString(m[1])
		target := s.BaseURL + href
		if strings.HasPrefix(href, "?") {
			target = s.BaseURL + "/sessions" + href
		}
		curHTML = p3GetHTML(t, client, target)
		if strings.Contains(ssTableBodyRe.FindString(curHTML), "sess-pw7-000") {
			foundOldest = true
			break
		}
	}
	if !foundOldest {
		t.Fatalf("pagination never reached sess-pw7-000 beyond row 100")
	}

	qHTML := p3GetHTML(t, client, s.BaseURL+"/sessions?period=all&q=sess-pw7-000")
	if !strings.Contains(qHTML, "/sessions/sess-pw7-000") {
		t.Fatalf("/sessions?q=sess-pw7-000 missing link to /sessions/sess-pw7-000")
	}
	detHTML := p3GetHTML(t, client, s.BaseURL+"/sessions/sess-pw7-000")
	if !strings.Contains(detHTML, "m-top") || !strings.Contains(detHTML, "proj-alpha") || strings.Contains(detHTML, ".jsonl") {
		t.Fatalf("/sessions/sess-pw7-000 unexpected content: %s", detHTML)
	}
}

func TestPairwise_F10_F21_QuotaSWRPaceBarSQLiteCRUDAndAlerts(t *testing.T) {
	requireE2E(t)
	var mu sync.Mutex
	var webhookPayloads []string
	var ntfyPayloads []string
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
		if strings.HasPrefix(r.URL.Path, "/webhook") {
			webhookPayloads = append(webhookPayloads, string(raw))
		} else if strings.HasPrefix(r.URL.Path, "/ntfy") {
			ntfyPayloads = append(ntfyPayloads, string(raw))
		}
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(alertSrv.Close)

	quotasYAML := `schema_version: "1"
quotas:
  - label: "YAML Anthropic Cap"
    provider: anthropic
    reset_day: 1
    budget:
      tokens_per_month: 5000
`
	dbPath := filepath.Join(t.TempDir(), "pw8.db")
	s1 := startTestServer(t, serverOpts{
		IngestToken:     "p3-f10-21",
		DBPath:          dbPath,
		QuotasYAML:      quotasYAML,
		AlertWebhookURL: alertSrv.URL + "/webhook",
		ExtraEnv: map[string]string{
			"TOKEMON_ALERT_NTFY_URL": alertSrv.URL + "/ntfy/e2e",
		},
	})
	c1 := s1.authClient(t)

	form := url.Values{
		"label":            {"SQLite OpenAI Cap"},
		"provider":         {"openai"},
		"model":            {"gpt-5.4"},
		"reset_day":        {"1"},
		"tokens_per_month": {"1000"},
	}
	cCreate, _, bCreate := ssSubmitQuotaCreate(t, s1, c1, form, true)
	if cCreate != http.StatusOK {
		t.Fatalf("create SQLite quota failed (%d): %s", cCreate, string(bCreate))
	}

	merged := p3WaitQuotas(t, c1, s1.BaseURL, func(rows []ssQuotaRow) bool {
		return len(rows) == 2
	})
	hasYAML, hasSQLite := false, false
	for _, q := range merged {
		if q.Label == "YAML Anthropic Cap" {
			hasYAML = true
		}
		if q.Label == "SQLite OpenAI Cap" {
			hasSQLite = true
		}
	}
	if !hasYAML || !hasSQLite {
		t.Fatalf("expected merged yaml + sqlite quotas in /api/v1/quotas, got %+v", merged)
	}
	settingsHTML := p3GetHTML(t, c1, s1.BaseURL+"/settings")
	var sawYAMLSource, sawSQLiteSource bool
	for _, card := range strings.Split(settingsHTML, `<article class="quota-card"`) {
		if strings.Contains(card, `data-source="yaml"`) && strings.Contains(card, "YAML Anthropic Cap") {
			sawYAMLSource = true
		}
		if strings.Contains(card, `data-source="sqlite"`) && strings.Contains(card, "SQLite OpenAI Cap") {
			sawSQLiteSource = true
		}
	}
	if !sawYAMLSource || !sawSQLiteSource {
		t.Fatalf("/settings missing expected data-source=\"yaml\" / data-source=\"sqlite\" quota cards: %s", settingsHTML)
	}
	p3WaitHTMLContains(t, c1, s1.BaseURL+"/", "SQLite OpenAI Cap")

	s1.ingestEvents(t, []ingestEvent{{
		EventID:     "p3-f10-21-ev",
		MachineID:   "m-pw8",
		SessionID:   "sess-pw8-secret",
		Project:     "proj-pw8-secret",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(1500),
	}})

	p3WaitQuotas(t, c1, s1.BaseURL, func(rows []ssQuotaRow) bool {
		var openAIOver, anthropicOK bool
		for _, q := range rows {
			if q.Label == "SQLite OpenAI Cap" && q.TokensUsed == 1500 && q.State == "over" {
				openAIOver = true
			}
			if q.Label == "YAML Anthropic Cap" && q.TokensUsed == 0 && q.State == "ok" {
				anthropicOK = true
			}
		}
		return openAIOver && anthropicOK
	})

	ovHTML := p3WaitHTMLContains(t, c1, s1.BaseURL+"/", "1,500")
	qSec := overviewQuotaSection(t, ovHTML, "SQLite OpenAI Cap")
	if !strings.Contains(strings.ToLower(qSec), "over") || (!strings.Contains(qSec, "1,500") && !strings.Contains(qSec, "1.5K")) {
		t.Fatalf("/ quota pace bar missing over and 1,500/1.5K state: %s", qSec)
	}

	// Verify both webhook and ntfy alerts delivered without leaking session/project.
	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		wLen, nLen := len(webhookPayloads), len(ntfyPayloads)
		mu.Unlock()
		if wLen > 0 && nLen > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for webhook (%d) and ntfy (%d) alerts", wLen, nLen)
		}
		time.Sleep(100 * time.Millisecond)
	}
	mu.Lock()
	allAlerts := strings.Join(append(append([]string(nil), webhookPayloads...), ntfyPayloads...), "\n")
	mu.Unlock()
	if !strings.Contains(allAlerts, "SQLite OpenAI Cap") {
		t.Fatalf("alerts missing quota label SQLite OpenAI Cap: %s", allAlerts)
	}
	for _, secret := range []string{"sess-pw8-secret", "proj-pw8-secret"} {
		if strings.Contains(allAlerts, secret) {
			t.Fatalf("alert leaked sensitive identifier %q: %s", secret, allAlerts)
		}
	}

	s1.Stop()

	// Restart persistence check.
	s2 := startTestServer(t, serverOpts{
		IngestToken: "p3-f10-21",
		DBPath:      dbPath,
		QuotasYAML:  quotasYAML,
	})
	c2 := s2.authClient(t)
	p3WaitQuotas(t, c2, s2.BaseURL, func(rows []ssQuotaRow) bool {
		if len(rows) != 2 {
			return false
		}
		for _, q := range rows {
			if q.Label == "SQLite OpenAI Cap" && q.State == "over" {
				return true
			}
		}
		return false
	})
	ovHTML2 := p3GetHTML(t, c2, s2.BaseURL+"/")
	if !strings.Contains(ovHTML2, "SQLite OpenAI Cap") || !strings.Contains(ovHTML2, "YAML Anthropic Cap") {
		t.Fatalf("restarted / missing persisted quotas")
	}
}

func TestPairwise_F11_F12_F17_GzipStaticImmutableBaseCSSChromeAndSettingsAuth(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{
		IngestToken:   "p3-f11-12-17",
		Username:      "admin",
		Password:      "pw-chrome-gzip-123",
		EnableTokedex: true,
	})
	s.ingestEvents(t, []ingestEvent{
		{EventID: "p3-f11-1", MachineID: "m-pw9", SessionID: "s-pw9-1", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(500)},
		{EventID: "p3-f11-2", MachineID: "m-pw9", SessionID: "s-pw9-2", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(500)},
		{EventID: "p3-f11-3", MachineID: "m-pw9", SessionID: "s-pw9-3", Provider: "anthropic", Model: "claude-sonnet-4", Tool: "claude-code", TotalTokens: i64(200)},
	})
	waitOverviewLifetimeTokens(t, s.authClient(t), s.BaseURL, 1200)

	gzClient := p3CookieNoDecompressClient(t, s)
	var baseCSSURL, stageURL, fontURL string
	pages := []string{"/", "/analytics", "/sessions", "/settings", "/insights", "/tokedex"}
	for _, p := range pages {
		code, hdr, gzBody := p3Do(t, gzClient, http.MethodGet, s.BaseURL+p, nil, map[string]string{
			"Accept":          "text/html",
			"Accept-Encoding": "gzip",
		})
		if code != http.StatusOK {
			t.Fatalf("GET %s status = %d, want 200", p, code)
		}
		if hdr.Get("Content-Encoding") != "gzip" {
			t.Fatalf("GET %s Content-Encoding = %q, want gzip", p, hdr.Get("Content-Encoding"))
		}
		if !varyHasAcceptEncoding(&http.Response{Header: hdr}) {
			t.Fatalf("GET %s missing Vary: Accept-Encoding", p)
		}
		if hdr.Get("Cache-Control") != "no-store" {
			t.Fatalf("GET %s Cache-Control = %q, want no-store", p, hdr.Get("Cache-Control"))
		}
		plain := string(gunzipBytes(t, gzBody))
		cssHref := p3BaseCSSVersionedRe.FindString(plain)
		if cssHref == "" {
			t.Fatalf("%s missing versioned base.css?v=...", p)
		}
		if baseCSSURL == "" {
			baseCSSURL = cssHref
		} else if cssHref != baseCSSURL {
			t.Fatalf("%s base.css URL %q != %q", p, cssHref, baseCSSURL)
		}
		if !strings.Contains(plain, `class="skip-link"`) || !strings.Contains(plain, `#main-content`) {
			t.Fatalf("%s missing skip-link -> #main-content", p)
		}
		if !strings.Contains(plain, `logout-form`) || !strings.Contains(plain, `settings-link`) || !strings.Contains(plain, `<footer`) {
			t.Fatalf("%s missing shared chrome partials", p)
		}
		nav := ssExtractNavBlock(t, p, plain)
		if strings.Contains(nav, `href="/insights"`) {
			t.Fatalf("CAR-205 violation on %s: /insights linked inside .nav", p)
		}
		if p == "/" {
			stageURL = p3StagePNGVersionedRe.FindString(plain)
			fontURL = p3FontVersionedRe.FindString(plain)
		}
		if p == "/settings" {
			gptIdx := strings.Index(plain, "gpt-5.4")
			sonIdx := strings.Index(plain, "claude-sonnet-4")
			if gptIdx < 0 || sonIdx < 0 || gptIdx > sonIdx {
				t.Fatalf("/settings models not sorted by usage DESC (gpt=%d, sonnet=%d)", gptIdx, sonIdx)
			}
			if !strings.Contains(plain, "/tokedex?q=gpt-5.4") || !strings.Contains(plain, "/analytics") {
				t.Fatalf("/settings missing model cross-links")
			}
		}
	}

	cCSS, hCSS, bCSS := p3Do(t, gzClient, http.MethodGet, s.BaseURL+baseCSSURL, nil, map[string]string{"Accept-Encoding": "gzip"})
	if cCSS != http.StatusOK || hCSS.Get("Content-Encoding") != "gzip" || hCSS.Get("Cache-Control") != "public, max-age=31536000, immutable" {
		t.Fatalf("base.css unexpected status=%d ce=%q cc=%q", cCSS, hCSS.Get("Content-Encoding"), hCSS.Get("Cache-Control"))
	}
	if len(gunzipBytes(t, bCSS)) == 0 {
		t.Fatalf("empty gunzipped base.css")
	}

	if stageURL == "" || fontURL == "" {
		t.Fatalf("missing versioned stage (%q) or font (%q) URL on /", stageURL, fontURL)
	}
	for _, tc := range []struct {
		u     string
		magic []byte
	}{
		{stageURL, []byte("\x89PNG\r\n\x1a\n")},
		{fontURL, []byte("wOF2")},
	} {
		cBin, hBin, bBin := p3Do(t, gzClient, http.MethodGet, s.BaseURL+tc.u, nil, map[string]string{"Accept-Encoding": "gzip"})
		if cBin != http.StatusOK || hBin.Get("Content-Encoding") != "" || hBin.Get("Cache-Control") != "public, max-age=31536000, immutable" {
			t.Fatalf("binary %s unexpected status=%d ce=%q cc=%q", tc.u, cBin, hBin.Get("Content-Encoding"), hBin.Get("Cache-Control"))
		}
		if !bytes.HasPrefix(bBin, tc.magic) {
			t.Fatalf("binary %s missing expected magic prefix", tc.u)
		}
	}
}

func TestPairwise_F12_F22_F24_CAR205PasswordAuthTokedexDetailAndCardSVG(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{
		IngestToken:    "p3-f12-ing",
		DashboardToken: "p3-f12-dash",
		Username:       "admin",
		Password:       "pw-card-22-24",
		EnableTokedex:  true,
		Timezone:       "UTC",
	})
	now := time.Now().UTC()
	todayUTC := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	tsD2 := todayUTC.AddDate(0, 0, -2).Add(12 * time.Hour)
	tsD1 := todayUTC.AddDate(0, 0, -1).Add(12 * time.Hour)
	tsD0a := now.Add(-30 * time.Minute).Truncate(time.Second)
	tsD0b := now.Add(-15 * time.Minute).Truncate(time.Second)
	if tsD0a.Format("2006-01-02") != todayUTC.Format("2006-01-02") {
		tsD0a = todayUTC
		tsD0b = now.Truncate(time.Second)
	}
	s.ingestEvents(t, []ingestEvent{
		{EventID: "p3-f12-1", Timestamp: tsD2.Format(time.RFC3339Nano), MachineID: "m-secret-box", SessionID: "sess-pw10-secret", Project: "proj-pw10-secret", Provider: "openai", Model: "gpt-5.4", Tool: "codex", InputTokens: i64(300), OutputTokens: i64(100), TotalTokens: i64(400)},
		{EventID: "p3-f12-2", Timestamp: tsD1.Format(time.RFC3339Nano), MachineID: "m-secret-box", SessionID: "sess-pw10-secret", Project: "proj-pw10-secret", Provider: "openai", Model: "gpt-5.4", Tool: "codex", InputTokens: i64(400), OutputTokens: i64(100), TotalTokens: i64(500)},
		{EventID: "p3-f12-3", Timestamp: tsD0a.Format(time.RFC3339Nano), MachineID: "m-secret-box", SessionID: "sess-pw10-secret", Project: "proj-pw10-secret", Provider: "openai", Model: "gpt-5.4", Tool: "codex", InputTokens: i64(500), OutputTokens: i64(100), TotalTokens: i64(600)},
		{EventID: "p3-f12-4", Timestamp: tsD0b.Format(time.RFC3339Nano), MachineID: "m-secret-box", SessionID: "sess-pw10-secret", Project: "proj-pw10-secret", Provider: "custom", Model: "custom-pw10-model", Tool: "codex", TotalTokens: i64(200)},
	})

	cookieClient := p3CookieNoDecompressClient(t, s)
	// Decompressing client for HTML checks:
	htmlClient := s.authClient(t)
	waitOverviewLifetimeTokens(t, htmlClient, s.BaseURL, 1700)

	cAlias, _, _ := p3PostAliases(t, s, htmlClient, url.Values{
		"machine_identity": {"m-secret-box"},
		"machine_alias":    {"Secret Alias Box"},
	})
	if cAlias != http.StatusSeeOther && cAlias != http.StatusOK {
		t.Fatalf("POST /settings/aliases status = %d", cAlias)
	}

	// Anonymous checks: HTML -> 303 /login?next=..., API -> 401, IngestToken != DashboardToken -> 401.
	for _, path := range []string{"/insights", "/tokedex", "/tokedex/gpt-5.4", "/api/v1/card.svg"} {
		resp, _ := s.doReq(t, http.MethodGet, path, nil, map[string]string{"Accept": "text/html"})
		if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), "/login?next=") {
			t.Fatalf("anon HTML %s status=%d loc=%q, want 303 /login?next=...", path, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
	for _, path := range []string{"/api/v1/card.svg", "/api/v1/insights"} {
		resp, _ := s.doReq(t, http.MethodGet, path, nil, nil)
		if resp.StatusCode != http.StatusUnauthorized || !strings.Contains(resp.Header.Get("WWW-Authenticate"), "Basic") {
			t.Fatalf("anon API %s status=%d www=%q, want 401 Basic", path, resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
		}
		respIng, _ := s.doReq(t, http.MethodGet, path, nil, map[string]string{"Authorization": "Bearer " + s.IngestToken})
		if respIng.StatusCode != http.StatusUnauthorized {
			t.Fatalf("IngestToken on %s status=%d, want 401", path, respIng.StatusCode)
		}
	}

	anHTML := p3GetHTML(t, htmlClient, s.BaseURL+"/analytics?period=30d")
	if !strings.Contains(anHTML, `href="/tokedex#uncataloged-title"`) {
		t.Fatalf("/analytics missing /tokedex#uncataloged-title link")
	}

	itcWaitCatalog(t, htmlClient, s.BaseURL+"/api/v1/catalog", func(c itcCatalogSnapshot) bool {
		return c.ObservedTokens == 1700 && c.UncatalogedModels == 1
	})
	tdHTML := p3GetHTML(t, htmlClient, s.BaseURL+"/tokedex")
	if !strings.Contains(tdHTML, `id="uncataloged-title"`) || !strings.Contains(tdHTML, "custom-pw10-model") {
		t.Fatalf("/tokedex missing uncataloged section or custom-pw10-model")
	}
	if whatIf := itcExtractWhatIfSection(tdHTML); whatIf == "" && !strings.Contains(strings.ToLower(tdHTML), "what-if") && !strings.Contains(strings.ToLower(tdHTML), "what if") {
		t.Fatalf("/tokedex missing 30d what-if section")
	}
	if nav := ssExtractNavBlock(t, "/tokedex", tdHTML); !strings.Contains(nav, `href="/tokedex"`) || strings.Contains(nav, `href="/insights"`) {
		t.Fatalf("/tokedex .nav failed CAR-205 or tokedex link check: %s", nav)
	}

	detHTML := p3GetHTML(t, htmlClient, s.BaseURL+"/tokedex/gpt-5.4")
	if navDet := ssExtractNavBlock(t, "/tokedex/gpt-5.4", detHTML); !strings.Contains(navDet, `href="/tokedex"`) || strings.Contains(navDet, `href="/insights"`) {
		t.Fatalf("/tokedex/gpt-5.4 .nav failed CAR-205 or tokedex link check: %s", navDet)
	}
	if !strings.Contains(detHTML, "GPT-5.4") || !strings.Contains(detHTML, "http") {
		t.Fatalf("/tokedex/gpt-5.4 missing display name or provenance URL")
	}

	// Verify /api/v1/card.svg across cookie, Bearer DashboardToken, and Basic auth.
	basicCreds := base64.StdEncoding.EncodeToString([]byte(s.Username + ":" + s.Password))
	authVariants := []struct {
		name string
		do   func() (int, http.Header, []byte)
	}{
		{"cookie", func() (int, http.Header, []byte) {
			return p3Do(t, cookieClient, http.MethodGet, s.BaseURL+"/api/v1/card.svg", nil, nil)
		}},
		{"bearer", func() (int, http.Header, []byte) {
			return p3Do(t, s.Client, http.MethodGet, s.BaseURL+"/api/v1/card.svg", nil, map[string]string{"Authorization": "Bearer " + s.DashboardToken})
		}},
		{"basic", func() (int, http.Header, []byte) {
			return p3Do(t, s.Client, http.MethodGet, s.BaseURL+"/api/v1/card.svg", nil, map[string]string{"Authorization": "Basic " + basicCreds})
		}},
	}
	for _, av := range authVariants {
		code, hdr, svgBytes := av.do()
		if code != http.StatusOK || !strings.Contains(hdr.Get("Content-Type"), "image/svg+xml") || hdr.Get("Cache-Control") != "no-store" {
			t.Fatalf("card.svg (%s) status=%d ct=%q cc=%q", av.name, code, hdr.Get("Content-Type"), hdr.Get("Cache-Control"))
		}
		itcValidateSVGXML(t, svgBytes)
		svgStr := string(svgBytes)
		svgLower := strings.ToLower(svgStr)
		for _, hex := range []string{"#10110f", "#9bbba0", "#d2a477"} {
			if !strings.Contains(svgLower, hex) {
				t.Fatalf("card.svg (%s) missing palette color %s", av.name, hex)
			}
		}
		svgText := itcExtractSVGText(t, svgBytes)
		if !strings.Contains(svgText, "Promptling") || (!strings.Contains(svgText, "1,700") && !strings.Contains(svgText, "1.7K")) {
			t.Fatalf("card.svg (%s) missing Promptling or 1,700/1.7K in %q", av.name, svgText)
		}
		if !regexp.MustCompile(`(?:^|\D)3(?:\D|$)`).MatchString(svgText) {
			t.Fatalf("card.svg (%s) missing streak 3 in %q", av.name, svgText)
		}
		for _, leak := range []string{"proj-pw10-secret", "sess-pw10-secret", "m-secret-box", "Secret Alias Box"} {
			if strings.Contains(svgStr, leak) {
				t.Fatalf("card.svg (%s) leaked %q", av.name, leak)
			}
		}
	}
}

func TestPairwise_F1_F3_F11_LCPAndLocalizedHealthTimestamps(t *testing.T) {
	requireE2E(t)
	tokyoLoc := p3LoadLoc(t, "Asia/Tokyo")
	s := startTestServer(t, serverOpts{IngestToken: "p3-f1-3-11", Timezone: "Asia/Tokyo"})
	now := time.Now().UTC()
	stale := now.Add(-2 * time.Hour)
	yesterday2015UTC := time.Date(now.Year(), now.Month(), now.Day(), 20, 15, 0, 0, time.UTC).AddDate(0, 0, -1)

	s.sendHeartbeat(t, "m-pw11-tokyo", "codex",
		heartbeatSource{Adapter: "codex", IdentityHash: "sha256:t1-" + strings.Repeat("1", 50), LastSuccessfulSync: &stale},
		heartbeatSource{Adapter: "codex", IdentityHash: "sha256:t2-" + strings.Repeat("2", 50), LastSuccessfulSync: &now},
		heartbeatSource{Adapter: "codex", IdentityHash: "sha256:t3-" + strings.Repeat("3", 50), LastSuccessfulSync: &now},
		heartbeatSource{Adapter: "codex", IdentityHash: "sha256:t4-" + strings.Repeat("4", 50), LastSuccessfulSync: &now},
	)
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "p3-f1-3-11-ev",
		Timestamp:   yesterday2015UTC.Format(time.RFC3339Nano),
		MachineID:   "m-pw11-tokyo",
		SessionID:   "sess-pw11-tokyo",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(250),
	}})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 250)
	ovHTML := p3WaitHTMLContains(t, client, s.BaseURL+"/", "codex · 1 stale · 3 fresh")

	var hasPreload bool
	for _, tag := range p3PreloadLinkRe.FindAllString(ovHTML, -1) {
		if strings.Contains(tag, `as="font"`) && strings.Contains(tag, `pixelify-sans-latin.woff2`) && strings.Contains(tag, `crossorigin`) {
			hasPreload = true
			break
		}
	}
	if !hasPreload {
		t.Fatalf("/ HTML missing font preload link")
	}
	imgTag := p3CreatureImgRe.FindString(ovHTML)
	if imgTag == "" || !strings.Contains(imgTag, `fetchpriority="high"`) || !p3WidthAttrRe.MatchString(imgTag) || !p3HeightAttrRe.MatchString(imgTag) {
		t.Fatalf(".creature-art missing LCP attrs: %q", imgTag)
	}
	cssHref := p3BaseCSSVersionedRe.FindString(ovHTML)
	if cssHref == "" {
		t.Fatalf("/ HTML missing versioned base.css")
	}
	cCSS, _, bCSS := p3Do(t, client, http.MethodGet, s.BaseURL+cssHref, nil, nil)
	cssCorpus := ovHTML + "\n" + string(bCSS)
	if cCSS != http.StatusOK || !p3ContentVisRe.MatchString(cssCorpus) || !p3ContainIntrinsicRe.MatchString(cssCorpus) {
		t.Fatalf("CSS missing content-visibility / contain-intrinsic-size (status=%d)", cCSS)
	}

	healthSec := p3SectionBetween(t, ovHTML, `<section class="panel health-panel"`, `</section>`)
	if !strings.Contains(healthSec, `<details class="source-details">`) {
		t.Fatalf("health-panel missing collapsed source-details")
	}
	healthTimes := ssExtractTimeElems(healthSec)
	if len(healthTimes) == 0 || !strings.Contains(healthTimes[0].Datetime, "+09:00") || !strings.Contains(healthTimes[0].Title, "JST") {
		t.Fatalf("health-panel <time> missing +09:00/JST: %+v", healthTimes)
	}

	sessSec := p3SectionBetween(t, ovHTML, `<section class="panel sessions-panel"`, `</section>`)
	wantDT := yesterday2015UTC.In(tokyoLoc).Format(time.RFC3339)
	if !strings.Contains(sessSec, fmt.Sprintf(`datetime="%s"`, wantDT)) {
		t.Fatalf("sessions-panel missing JST datetime %q: %s", wantDT, sessSec)
	}
	sessTimes := ssExtractTimeElems(sessSec)
	if len(sessTimes) == 0 || !strings.Contains(sessTimes[0].Title, "JST") {
		t.Fatalf("sessions-panel <time> missing JST title: %+v", sessTimes)
	}
}

func TestPairwise_F6_F10_F15_F19_ConcurrentWALReadsSWRRefreshAndAutomatedBackup(t *testing.T) {
	requireE2E(t)
	bakDir := filepath.Join(t.TempDir(), "pw12-backups")
	s := startTestServer(t, serverOpts{
		IngestToken:     "p3-f6-10-15-19",
		BackupDir:       bakDir,
		BackupInterval:  "300ms",
		BackupRetention: 2,
		EnableTokedex:   true,
	})
	client := s.authClient(t)

	// Warm all SWR endpoints & capture initial ETag.
	for _, p := range []string{"/api/v1/evolution", "/api/v1/catalog", "/api/v1/insights", "/sessions"} {
		code, _, _ := p3Do(t, client, http.MethodGet, s.BaseURL+p, nil, nil)
		if code != http.StatusOK {
			t.Fatalf("warm GET %s status = %d", p, code)
		}
	}
	etagTarget, etag, baseHdrs := p3CaptureOverviewETag(t, client, s.BaseURL)

	var wg sync.WaitGroup
	errCh := make(chan string, 64)
	stopReaders := make(chan struct{})
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			paths := []string{"/", "/sessions", "/insights", "/tokedex", "/api/v1/evolution", "/api/v1/analytics/overview"}
			for {
				select {
				case <-stopReaders:
					return
				default:
					for _, p := range paths {
						code, _, body, err := p3DoErr(client, http.MethodGet, s.BaseURL+p, nil, nil)
						if err != nil {
							errCh <- fmt.Sprintf("reader GET %s error: %v", p, err)
							return
						}
						if code != http.StatusOK {
							errCh <- fmt.Sprintf("reader GET %s -> %d: %s", p, code, string(body))
							return
						}
					}
				}
			}
		}()
	}

	for i := 0; i < 10; i++ {
		s.ingestEvents(t, []ingestEvent{{
			EventID:     fmt.Sprintf("p3-f6-12-%02d", i),
			MachineID:   "m-pw12",
			SessionID:   fmt.Sprintf("s-pw12-%02d", i),
			Provider:    "openai",
			Model:       "gpt-5.4",
			Tool:        "codex",
			TotalTokens: i64(50),
		}})
		time.Sleep(40 * time.Millisecond)
	}
	// Let automated backups run through at least ~3 cycles while readers exercise WAL.
	time.Sleep(800 * time.Millisecond)
	close(stopReaders)
	wg.Wait()
	close(errCh)
	for msg := range errCh {
		t.Fatal(msg)
	}

	waitOverviewLifetimeTokens(t, client, s.BaseURL, 500)
	if etag != "" {
		condHdrs := map[string]string{"If-None-Match": etag}
		for k, v := range baseHdrs {
			condHdrs[k] = v
		}
		code, _, _ := p3Do(t, client, http.MethodGet, etagTarget, nil, condHdrs)
		if code != http.StatusOK {
			t.Fatalf("conditional GET %s after ingest status = %d, want 200", etagTarget, code)
		}
	} else {
		ovHTML := p3GetHTML(t, client, s.BaseURL+"/")
		if !strings.Contains(ovHTML, "fetch(") && !strings.Contains(ovHTML, "EventSource(") {
			t.Fatalf("/ HTML missing ETag and missing fetch(/EventSource( in-place update script")
		}
	}
	p3WaitEvolution(t, client, s.BaseURL, func(ev evolutionPayload) bool {
		return ev.LifetimeTokens == 500
	})
	itcWaitCatalog(t, client, s.BaseURL+"/api/v1/catalog", func(c itcCatalogSnapshot) bool {
		return c.ObservedTokens == 500
	})
	itcWaitInsights(t, client, s.BaseURL+"/api/v1/insights", func(ins itcInsightsPayload) bool {
		return ins.DataQuality.KnownTokens == 500
	})
	ssWaitSessions(t, client, s.BaseURL+"/api/v1/sessions", func(rows []ssSessionRow) bool {
		return len(rows) == 10
	})

	stShort, stErr, err := s.runCLI(t, nil, "status", "--server", s.BaseURL, "--token", s.IngestToken, "--short")
	if err != nil || !strings.Contains(stShort, "500") {
		t.Fatalf("status --short failed (%v, %s): %q", err, stErr, stShort)
	}
	stJSON, stJErr, err := s.runCLI(t, nil, "status", "--server", s.BaseURL, "--token", s.IngestToken, "--json")
	if err != nil {
		t.Fatalf("status --json failed (%v): %s", err, stJErr)
	}
	var stObj any
	if err := json.Unmarshal([]byte(stJSON), &stObj); err != nil || !jsonContainsNumber(stObj, 500) {
		t.Fatalf("status --json missing 500: %s (%v)", stJSON, err)
	}

	// Wait until >=1 automated backup file appears and check /settings age before stopping.
	deadline := time.Now().Add(5 * time.Second)
	for {
		files, err := filepath.Glob(filepath.Join(bakDir, "*-backup-v*.db"))
		if err != nil {
			t.Fatalf("glob backups: %v", err)
		}
		if len(files) >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for automated backup in %s", bakDir)
		}
		time.Sleep(100 * time.Millisecond)
	}
	setHTML := p3GetHTML(t, client, s.BaseURL+"/settings")
	if strings.Contains(setHTML, "No backups yet") {
		t.Fatalf("/settings still says 'No backups yet' after automated backup")
	}

	s.Stop()
	files, err := filepath.Glob(filepath.Join(bakDir, "*-backup-v*.db"))
	if err != nil {
		t.Fatalf("glob backups after stop: %v", err)
	}
	if len(files) == 0 || len(files) > 2 {
		t.Fatalf("expected 1..2 retained backups, got %d: %v", len(files), files)
	}
	vOut, vErr, err := s.runCLI(t, nil, "backup", "verify", files[len(files)-1])
	if err != nil || !strings.Contains(vOut, `"schema_version"`) {
		t.Fatalf("backup verify failed (%v, %s): %s", err, vErr, vOut)
	}
}

// =============================================================================
// Tier 3 Part B: Features 13-24 Interactions & F1-F24 Bridges (12 Tests)
// =============================================================================

func TestPairwise_B01_CodexZstAndClaudeDesktopAcrossAnalyticsAndTokedex(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "p3-b01-tok", EnableTokedex: true})
	agentHome := filepath.Join(t.TempDir(), "agent-home")
	stateDB := filepath.Join(t.TempDir(), "state.db")
	now := time.Now().UTC().Add(-1 * time.Hour)

	zstPayload := strings.Join([]string{
		fmt.Sprintf(`{"timestamp":%q,"type":"session_meta","payload":{"id":"b01-cdx-1","model_provider":"openai","cwd":"/Users/alice/repo-zst"}}`, now.Format(time.RFC3339)),
		fmt.Sprintf(`{"timestamp":%q,"type":"turn_context","payload":{"model":"gpt-5.4","cwd":"/Users/alice/repo-zst"}}`, now.Add(time.Second).Format(time.RFC3339)),
		fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","info":{"last_token_usage":{"input_tokens":1600,"cached_input_tokens":600,"output_tokens":200,"reasoning_output_tokens":0,"total_tokens":1800},"total_token_usage":{"input_tokens":1600,"cached_input_tokens":600,"output_tokens":200,"reasoning_output_tokens":0,"total_tokens":1800}}}}`, now.Add(2*time.Second).Format(time.RFC3339)),
	}, "\n") + "\n"
	writeRawZstdFrame(t, filepath.Join(agentHome, ".codex", "archived_sessions", "r1.jsonl.zst"), []byte(zstPayload))

	claudeDir := filepath.Join(agentHome, ".claude", "projects", "-Users-alice-repo-desk")
	p3WriteClaudeJSONL(t, filepath.Join(claudeDir, "desk.jsonl"), "b01-desk-1", "/Users/alice/repo-desk", "claude-desktop", "claude-sonnet-4-5", now.Add(3*time.Minute), 300, 100, 0, 0)
	p3WriteClaudeJSONL(t, filepath.Join(claudeDir, "cli.jsonl"), "b01-cli-1", "/Users/alice/repo-desk", "cli", "claude-sonnet-4-5", now.Add(4*time.Minute), 80, 20, 0, 0)

	if _, stderr, err := runAgentWithArgs(t, s, agentHome, stateDB, "m-b01", "codex,claude-code"); err != nil {
		t.Fatalf("agent --once failed: %v\nstderr: %s", err, stderr)
	}
	client := s.authClient(t)

	var facets analyticsFacetsPayload
	getJSONWithClient(t, client, s.BaseURL+"/api/v1/analytics?period=30d&tool=claude-desktop", &facets)
	if !containsAll(facets.Facets.Tools, "codex", "claude-desktop", "claude-code") {
		t.Fatalf("facets.tools missing expected harnesses: %+v", facets.Facets.Tools)
	}

	anHTML := p3GetHTML(t, client, s.BaseURL+"/analytics?dimension=harnesses&tool=claude-desktop")
	if !strings.Contains(anHTML, "Claude Desktop") || !strings.Contains(anHTML, `data-count="400"`) {
		t.Fatalf("/analytics?dimension=harnesses&tool=claude-desktop missing Claude Desktop or 400: %s", anHTML)
	}
	if !strings.Contains(anHTML, `<input type="hidden" name="dimension" value="harnesses">`) {
		t.Fatalf("/analytics missing hidden dimension=harnesses")
	}
	detailsSeg := p3SectionBetween(t, anHTML, `<details class="filter-details"`, `</details>`)
	if !strings.Contains(p3SectionBetween(t, detailsSeg, `<summary`, `</summary>`), "1") {
		t.Fatalf("/analytics filter summary missing active count 1")
	}

	itcWaitCatalog(t, client, s.BaseURL+"/api/v1/catalog", func(c itcCatalogSnapshot) bool {
		return c.ObservedTokens == 2300
	})
	tdHTML := p3GetHTML(t, client, s.BaseURL+"/tokedex")
	if !strings.Contains(tdHTML, "1,800") || !strings.Contains(tdHTML, "500") {
		t.Fatalf("/tokedex missing observed 1,800 or 500 tokens")
	}
	gptDet := p3GetHTML(t, client, s.BaseURL+"/tokedex/gpt-5.4")
	sonDet := p3GetHTML(t, client, s.BaseURL+"/tokedex/claude-sonnet-4")
	if !strings.Contains(gptDet, "1,800") || !strings.Contains(sonDet, "Claude Desktop") {
		t.Fatalf("/tokedex detail views missing 1,800 or Claude Desktop mapping")
	}
	for _, body := range []string{anHTML, tdHTML, gptDet, sonDet} {
		if strings.Contains(body, "/Users/alice") || strings.Contains(body, "r1.jsonl.zst") {
			t.Fatalf("UI leaked raw local path or .jsonl.zst filename")
		}
	}
}

func TestPairwise_B02_CustomFromToProjectSearchPaginationDrillDownPrivacy(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "p3-b02-tok", Timezone: "UTC"})
	now := time.Now().UTC()
	anchor := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.UTC).AddDate(0, 0, -5)
	fromStr := anchor.AddDate(0, 0, -1).Format("2006-01-02")
	toStr := anchor.Format("2006-01-02")
	outsideTS := anchor.AddDate(0, 0, -4)

	events := make([]ingestEvent, 0, 120)
	for i := 0; i < 110; i++ {
		events = append(events, ingestEvent{
			EventID:         fmt.Sprintf("b02-in-%03d", i),
			Timestamp:       anchor.Add(-time.Duration(110-i) * time.Minute).Format(time.RFC3339Nano),
			MachineID:       "m-b02",
			SessionID:       fmt.Sprintf("sess-inc-%03d", i),
			Project:         "proj-incident",
			Provider:        "openai",
			Model:           "gpt-5.4",
			Tool:            "codex",
			InputTokens:     i64(800),
			CacheReadTokens: i64(400),
			OutputTokens:    i64(int64(200 + i)),
			TotalTokens:     i64(int64(1400 + i)),
		})
	}
	for i := 0; i < 5; i++ {
		events = append(events,
			ingestEvent{EventID: fmt.Sprintf("b02-out-%d", i), Timestamp: outsideTS.Add(time.Duration(i) * time.Minute).Format(time.RFC3339Nano), MachineID: "m-b02", SessionID: fmt.Sprintf("sess-inc-old-%d", i), Project: "proj-incident", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(500)},
			ingestEvent{EventID: fmt.Sprintf("b02-oth-%d", i), Timestamp: anchor.Add(time.Duration(i) * time.Minute).Format(time.RFC3339Nano), MachineID: "m-b02", SessionID: fmt.Sprintf("sess-inc-other-%d", i), Project: "proj-other", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(500)},
		)
	}
	s.ingestEvents(t, events)
	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 164995)

	anHTML := p3GetHTML(t, client, fmt.Sprintf("%s/analytics?from=%s&to=%s&dimension=projects", s.BaseURL, fromStr, toStr))
	bd := p3SectionBetween(t, anHTML, `class="panel breakdown-panel"`, `class="panel share-panel"`)
	var projDrill string
	for _, h := range p3ExtractHrefs(bd) {
		if strings.Contains(h, "project=proj-incident") && strings.Contains(h, "from="+fromStr) && strings.Contains(h, "to="+toStr) {
			projDrill = h
			break
		}
	}
	if projDrill == "" {
		t.Fatalf("projects breakdown missing drill-down carrying project=proj-incident & from/to: %s", bd)
	}
	drillHTML := p3GetHTML(t, client, s.BaseURL+projDrill)
	drillNoForms := analyticsStripForms(drillHTML)
	if !strings.Contains(drillNoForms, "proj-incident") || strings.Contains(drillNoForms, "proj-other") || strings.Contains(drillNoForms, "sess-inc-old-") {
		t.Fatalf("followed projDrill %q unexpected content", projDrill)
	}

	baseQ := fmt.Sprintf("/sessions?period=all&from=%s&to=%s&project=proj-incident&q=sess-inc&sort=tokens&dir=desc", fromStr, toStr)
	p1HTML := p3GetHTML(t, client, s.BaseURL+baseQ)
	p1Body := ssTableBodyRe.FindString(p1HTML)
	if !strings.Contains(p1Body, "sess-inc-109") || strings.Contains(p1Body, "sess-inc-000") || strings.Contains(p1Body, "proj-other") || strings.Contains(p1Body, "sess-inc-old-") {
		t.Fatalf("/sessions page 1 unexpected rows")
	}

	curHTML := p1HTML
	reachedLowest := false
	for nextPage := 2; nextPage <= 10; nextPage++ {
		re := regexp.MustCompile(fmt.Sprintf(`href="([^"]*(?:\?|&(?:amp;)?)page=%d(?:&[^"]*)?)"`, nextPage))
		m := re.FindStringSubmatch(curHTML)
		if len(m) < 2 {
			break
		}
		href := stdhtml.UnescapeString(m[1])
		for _, wantParam := range []string{"from=" + fromStr, "to=" + toStr, "project=proj-incident", "q=sess-inc", "sort=tokens", "dir=desc"} {
			if !strings.Contains(href, wantParam) {
				t.Fatalf("pagination link %q lost %q", href, wantParam)
			}
		}
		target := s.BaseURL + href
		if strings.HasPrefix(href, "?") {
			target = s.BaseURL + "/sessions" + href
		}
		curHTML = p3GetHTML(t, client, target)
		if strings.Contains(ssTableBodyRe.FindString(curHTML), "sess-inc-000") {
			reachedLowest = true
			break
		}
	}
	if !reachedLowest {
		t.Fatalf("pagination did not reach sess-inc-000")
	}

	cExp, _, csvBytes := p3Do(t, client, http.MethodGet, fmt.Sprintf("%s/api/v1/sessions/export?format=csv&period=all&from=%s&to=%s&project=proj-incident&q=sess-inc", s.BaseURL, fromStr, toStr), nil, nil)
	if cExp != http.StatusOK {
		t.Fatalf("sessions CSV export status = %d", cExp)
	}
	recs, err := csv.NewReader(bytes.NewReader(csvBytes)).ReadAll()
	if err != nil || len(recs) != 111 {
		t.Fatalf("sessions CSV export rows = %d (err=%v), want 111", len(recs), err)
	}

	detHTML := p3GetHTML(t, client, s.BaseURL+"/sessions/sess-inc-109")
	if !strings.Contains(detHTML, "33.3%") || !strings.Contains(detHTML, "proj-incident") || strings.Contains(detHTML, ".jsonl") {
		t.Fatalf("/sessions/sess-inc-109 unexpected detail content: %s", detHTML)
	}
}

func TestPairwise_B03_HeatmapTrendClickThroughTimezoneAndStreaks(t *testing.T) {
	requireE2E(t)
	torLoc := p3LoadLoc(t, "America/Toronto")
	s := startTestServer(t, serverOpts{IngestToken: "p3-b03-tok", Timezone: "America/Toronto"})

	nowTor := time.Now().In(torLoc)
	torToday := time.Date(nowTor.Year(), nowTor.Month(), nowTor.Day(), 0, 0, 0, 0, torLoc)
	tsD2 := time.Date(torToday.AddDate(0, 0, -2).Year(), torToday.AddDate(0, 0, -2).Month(), torToday.AddDate(0, 0, -2).Day(), 21, 30, 0, 0, torLoc)
	tsD1 := time.Date(torToday.AddDate(0, 0, -1).Year(), torToday.AddDate(0, 0, -1).Month(), torToday.AddDate(0, 0, -1).Day(), 21, 30, 0, 0, torLoc)
	tsD0 := nowTor.Add(-10 * time.Minute).Truncate(time.Second)
	if tsD0.In(torLoc).Format("2006-01-02") != torToday.Format("2006-01-02") {
		tsD0 = torToday.Add(5 * time.Minute)
	}

	torD1 := tsD1.In(torLoc).Format("2006-01-02")
	utcD1 := tsD1.UTC().Format("2006-01-02")
	if torD1 == utcD1 {
		t.Fatalf("setup invariant failed: Toronto %s == UTC %s", torD1, utcD1)
	}

	s.ingestEvents(t, []ingestEvent{
		{EventID: "b03-d2", Timestamp: tsD2.UTC().Format(time.RFC3339Nano), MachineID: "m-tor", SessionID: "sess-tor-d2", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(1000)},
		{EventID: "b03-d1", Timestamp: tsD1.UTC().Format(time.RFC3339Nano), MachineID: "m-tor", SessionID: "sess-tor-d1", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(5000)},
		{EventID: "b03-d0", Timestamp: tsD0.UTC().Format(time.RFC3339Nano), MachineID: "m-tor", SessionID: "sess-tor-d0", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(1500)},
	})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 7500)

	var ov overviewPayload
	getJSONWithClient(t, client, s.BaseURL+"/api/v1/analytics/overview", &ov)
	if ov.Activity.CurrentStreak != 3 || ov.Activity.LongestStreak != 3 || overviewParsePeakDayDate(ov.Activity.PeakDay) != torD1 {
		t.Fatalf("unexpected Toronto streak/peak: %+v", ov.Activity)
	}

	ovHTML := p3WaitHTMLContains(t, client, s.BaseURL+"/", fmt.Sprintf(`data-date="%s"`, torD1))
	actHeader := overviewStripActivityLegend(p3SectionBetween(t, ovHTML, `<section class="panel activity-panel"`, `<div class="activity-shell">`))
	if !overviewHasStreakValue(actHeader, 3) || (!strings.Contains(actHeader, "5,000") && !strings.Contains(actHeader, "5K")) {
		t.Fatalf("activity header missing streak 3 or peak 5,000/5K: %s", actHeader)
	}
	if !strings.Contains(ovHTML, `class="activity-cell`) || !strings.Contains(ovHTML, fmt.Sprintf(`data-date="%s"`, torD1)) {
		t.Fatalf("/ heatmap missing cell data-date=%q", torD1)
	}

	anHTML := p3GetHTML(t, client, s.BaseURL+"/analytics?period=7d")
	trend := p3SectionBetween(t, anHTML, `class="trend-chart`, `class="panel breakdown-panel"`)
	if !strings.Contains(trend, fmt.Sprintf(`data-date="%s"`, torD1)) {
		t.Fatalf("/analytics?period=7d trend-column missing Toronto date %q", torD1)
	}

	var torRows, utcRows []ssSessionRow
	getJSONWithClient(t, client, fmt.Sprintf("%s/api/v1/sessions?from=%s&to=%s", s.BaseURL, torD1, torD1), &torRows)
	getJSONWithClient(t, client, fmt.Sprintf("%s/api/v1/sessions?from=%s&to=%s", s.BaseURL, utcD1, utcD1), &utcRows)
	if len(torRows) != 1 || torRows[0].SessionID != "sess-tor-d1" {
		t.Fatalf("Toronto day %s rows = %+v, want [sess-tor-d1]", torD1, torRows)
	}
	for _, r := range utcRows {
		if r.SessionID == "sess-tor-d1" {
			t.Fatalf("UTC day %s unexpectedly matched Toronto 21:30 session", utcD1)
		}
	}

	sessHTML := p3GetHTML(t, client, fmt.Sprintf("%s/sessions?from=%s&to=%s", s.BaseURL, torD1, torD1))
	times := ssExtractTimeElems(sessHTML)
	if len(times) == 0 || (!strings.Contains(times[0].Datetime, "-05:00") && !strings.Contains(times[0].Datetime, "-04:00")) {
		t.Fatalf("/sessions missing EST/EDT offset in <time>: %+v", times)
	}
	if !strings.Contains(times[0].Title, "EST") && !strings.Contains(times[0].Title, "EDT") {
		t.Fatalf("/sessions <time title=%q> missing EST/EDT", times[0].Title)
	}
}

func TestPairwise_B04_DeclutteredAnalyticsPillsCacheSavingsEffectiveRateAndExport(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "p3-b04-tok"})
	s.ingestEvents(t, []ingestEvent{
		{EventID: "b04-1", MachineID: "m1", SessionID: "s1", Provider: "openai", Model: "gpt-5.4", Tool: "codex", InputTokens: i64(200_000), CacheReadTokens: i64(800_000), OutputTokens: i64(50_000), TotalTokens: i64(1_050_000)},
		{EventID: "b04-2", MachineID: "m1", SessionID: "s2", Provider: "anthropic", Model: "claude-sonnet-4", Tool: "claude-code", InputTokens: i64(900_000), CacheReadTokens: i64(100_000), OutputTokens: i64(50_000), TotalTokens: i64(1_050_000)},
		{EventID: "b04-3", MachineID: "=CMD()", SessionID: "s3", Provider: "openai", Model: "gpt-5.4", Tool: "codex", InputTokens: i64(100), TotalTokens: i64(100)},
	})
	client := s.authClient(t)
	p3WaitAnalyticsTokens(t, client, s.BaseURL+"/api/v1/analytics?period=30d", 2_100_100)

	cAlias, _, _ := p3PostAliases(t, s, client, url.Values{
		"machine_identity": {"m1"},
		"machine_alias":    {"Box One"},
	})
	if cAlias != http.StatusSeeOther && cAlias != http.StatusOK {
		t.Fatalf("POST /settings/aliases status = %d", cAlias)
	}

	filteredHTML := p3GetHTML(t, client, s.BaseURL+"/analytics?period=30d&dimension=harnesses&machine=m1&tool=codex")
	if p3DupDimSelectRe.MatchString(filteredHTML) || strings.Contains(filteredHTML, `id="dimension-filter"`) {
		t.Fatalf("duplicate dimension <select> found")
	}
	if !strings.Contains(filteredHTML, `<input type="hidden" name="dimension" value="harnesses">`) {
		t.Fatalf("filter form missing hidden dimension=harnesses")
	}
	detailsSeg := p3SectionBetween(t, filteredHTML, `<details class="filter-details"`, `</details>`)
	summarySeg := p3SectionBetween(t, detailsSeg, `<summary`, `</summary>`)
	if !strings.Contains(summarySeg, "Filters") || !strings.Contains(summarySeg, "(2)") {
		t.Fatalf("filter summary missing Filters (2): %q", summarySeg)
	}
	for _, want := range []string{"$1.80 saved", "$1.38", "80.0%"} {
		if !strings.Contains(filteredHTML, want) {
			t.Fatalf("filtered Codex view missing %q", want)
		}
	}

	filterPanel := p3SectionBetween(t, filteredHTML, `class="panel filter-panel"`, `class="stats"`)
	var dropToolHref string
	for _, h := range p3ExtractHrefs(filterPanel) {
		if !strings.HasPrefix(h, "/analytics") {
			continue
		}
		u, err := url.Parse(h)
		if err == nil && u.Query().Get("tool") == "" && u.Query().Get("machine") == "m1" && u.Query().Get("dimension") == "harnesses" {
			dropToolHref = h
			break
		}
	}
	if dropToolHref == "" {
		t.Fatalf("missing pill remove link dropping tool=codex while keeping machine=m1: %s", filterPanel)
	}
	m1OnlyHTML := p3GetHTML(t, client, s.BaseURL+dropToolHref)
	for _, want := range []string{"$2.07 saved", "$2.35", "80.0%", "10.0%"} {
		if !strings.Contains(m1OnlyHTML, want) {
			t.Fatalf("machine=m1 combined view missing %q", want)
		}
	}

	// Unfiltered machines CSV export: formula sanitization (=CMD()) and raw machine IDs (m1, not Box One).
	machHTML := p3GetHTML(t, client, s.BaseURL+"/analytics?period=30d&dimension=machines")
	unfilteredDetails := p3SectionBetween(t, machHTML, `<details class="filter-details"`, `</details>`)
	unfilteredSummary := p3SectionBetween(t, unfilteredDetails, `<summary`, `</summary>`)
	if strings.Contains(unfilteredSummary, "(") || strings.Contains(unfilteredSummary, "(2)") {
		t.Fatalf("unfiltered filter summary unexpectedly contained active count: %q", unfilteredSummary)
	}
	var csvHref string
	exportRe := regexp.MustCompile(`<a\b[^>]*href="([^"]*export[^"]*)"[^>]*>`)
	for _, m := range exportRe.FindAllStringSubmatch(machHTML, -1) {
		if strings.Contains(m[1], "format=csv") && strings.Contains(m[0], "download") {
			csvHref = stdhtml.UnescapeString(m[1])
			break
		}
	}
	if csvHref == "" {
		t.Fatalf("missing CSV download link on /analytics?dimension=machines")
	}
	codeExp, _, csvBytes := p3Do(t, client, http.MethodGet, s.BaseURL+csvHref, nil, nil)
	if codeExp != http.StatusOK {
		t.Fatalf("CSV export status = %d", codeExp)
	}
	csvStr := string(csvBytes)
	if !strings.Contains(csvStr, "'=CMD()") {
		t.Fatalf("CSV export did not sanitize formula prefix '=CMD(): %s", csvStr)
	}
	if !strings.Contains(csvStr, ",m1,") || strings.Contains(csvStr, "Box One") {
		t.Fatalf("CSV export must preserve raw m1 and omit alias Box One: %s", csvStr)
	}
}

func TestPairwise_B05_SQLiteAndYAMLQuotasPaceBarAlertsAndSWR(t *testing.T) {
	requireE2E(t)
	var mu sync.Mutex
	var whBodies, ntfyBodies []string
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
		if strings.HasPrefix(r.URL.Path, "/webhook") {
			whBodies = append(whBodies, string(raw))
		} else if strings.HasPrefix(r.URL.Path, "/ntfy") {
			ntfyBodies = append(ntfyBodies, string(raw))
		}
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	t.Cleanup(alertSrv.Close)

	quotasYAML := `schema_version: "1"
quotas:
  - label: "YAML Anthropic Cap"
    provider: anthropic
    reset_day: 1
    budget:
      tokens_per_month: 8000
`
	s := startTestServer(t, serverOpts{
		IngestToken:     "p3-b05-tok",
		QuotasYAML:      quotasYAML,
		AlertWebhookURL: alertSrv.URL + "/webhook",
		ExtraEnv: map[string]string{
			"TOKEMON_ALERT_NTFY_URL": alertSrv.URL + "/ntfy/e2e",
		},
	})
	client := s.authClient(t)
	// Warm /api/v1/quotas and /.
	p3WaitQuotas(t, client, s.BaseURL, func(rows []ssQuotaRow) bool { return len(rows) == 1 })
	p3GetHTML(t, client, s.BaseURL+"/")

	cCreate, _, bCreate := ssSubmitQuotaCreate(t, s, client, url.Values{
		"label":            {"SQLite OpenAI Cap"},
		"provider":         {"openai"},
		"reset_day":        {"1"},
		"tokens_per_month": {"1000"},
	}, true)
	if cCreate != http.StatusOK {
		t.Fatalf("create quota failed (%d): %s", cCreate, string(bCreate))
	}

	var merged []ssQuotaRow
	getJSONWithClient(t, client, s.BaseURL+"/api/v1/quotas", &merged)
	if len(merged) != 2 {
		t.Fatalf("expected immediate SWR invalidation -> 2 merged quotas, got %+v", merged)
	}
	ovMerged := p3WaitHTMLContains(t, client, s.BaseURL+"/", "SQLite OpenAI Cap")
	if !strings.Contains(ovMerged, "YAML Anthropic Cap") {
		t.Fatalf("/ missing YAML Anthropic Cap alongside SQLite OpenAI Cap")
	}

	s.ingestEvents(t, []ingestEvent{{
		EventID:     "p3-b05-ev",
		MachineID:   "m-b05",
		SessionID:   "sess-secret-q",
		Project:     "proj-secret-q",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(1200),
	}})

	p3WaitQuotas(t, client, s.BaseURL, func(rows []ssQuotaRow) bool {
		for _, q := range rows {
			if q.Label == "SQLite OpenAI Cap" && q.State == "over" && q.TokensUsed == 1200 {
				return true
			}
		}
		return false
	})
	ovOver := p3WaitHTMLContains(t, client, s.BaseURL+"/", "1,200")
	qSec := overviewQuotaSection(t, ovOver, "SQLite OpenAI Cap")
	if !strings.Contains(strings.ToLower(qSec), "over") || (!strings.Contains(qSec, "1,200") && !strings.Contains(qSec, "1.2K")) {
		t.Fatalf("/ pace bar missing over and 1,200/1.2K: %s", qSec)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		ready := len(whBodies) > 0 && len(ntfyBodies) > 0
		mu.Unlock()
		if ready {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for webhook & ntfy alerts")
		}
		time.Sleep(100 * time.Millisecond)
	}
	mu.Lock()
	joined := strings.Join(append(append([]string(nil), whBodies...), ntfyBodies...), "\n")
	mu.Unlock()
	if !strings.Contains(joined, "SQLite OpenAI Cap") || strings.Contains(joined, "sess-secret-q") || strings.Contains(joined, "proj-secret-q") {
		t.Fatalf("alert privacy/content failure: %s", joined)
	}

	// Edit quota to 5000 and confirm state returns to ok.
	setHTML := p3GetHTML(t, client, s.BaseURL+"/settings")
	_, editForms, _ := ssDiscoverQuotaForms(setHTML)
	if len(editForms) == 0 {
		t.Fatalf("/settings missing edit form for SQLite quota")
	}
	ef := editForms[0]
	editVals := url.Values{"csrf_token": {extractCSRFToken(t, []byte(setHTML))}}
	ssCopyHiddenInputs(ef.Inner, editVals)
	ssCopySubmitButton(ef.Inner, false, editVals)
	editVals.Set("label", "SQLite OpenAI Cap")
	editVals.Set("provider", "openai")
	editVals.Set("reset_day", "1")
	editVals.Set("tokens_per_month", "5000")
	editURL := ef.Action
	if !strings.HasPrefix(editURL, "http") {
		editURL = s.BaseURL + editURL
	}
	cEdit, _, bEdit := ssPostForm(t, client, editURL, s.BaseURL, s.BaseURL+"/settings", editVals)
	if cEdit != http.StatusOK {
		t.Fatalf("edit quota failed (%d): %s", cEdit, string(bEdit))
	}
	p3WaitQuotas(t, client, s.BaseURL, func(rows []ssQuotaRow) bool {
		for _, q := range rows {
			if q.Label == "SQLite OpenAI Cap" && q.State == "ok" {
				return true
			}
		}
		return false
	})
}

func TestPairwise_B06_TokedexEnablementAnalyticsLinkWhatIfDetailAndSettingsAliases(t *testing.T) {
	requireE2E(t)
	dbPath := filepath.Join(t.TempDir(), "b06.db")
	s1 := startTestServer(t, serverOpts{IngestToken: "p3-b06-tok", DBPath: dbPath, EnableTokedex: true})
	now := time.Now().UTC()
	s1.ingestEvents(t, []ingestEvent{
		{EventID: "b06-1", Timestamp: now.Add(-24 * time.Hour).Format(time.RFC3339Nano), MachineID: "m-b06", SessionID: "s-b06-1", Provider: "openai", Model: "gpt-5.4", Tool: "codex", InputTokens: i64(1_000_000), CacheReadTokens: i64(500_000), OutputTokens: i64(200_000), TotalTokens: i64(1_700_000)},
		{EventID: "b06-2", Timestamp: now.AddDate(0, 0, -45).Format(time.RFC3339Nano), MachineID: "m-b06", SessionID: "s-b06-2", Provider: "openai", Model: "gpt-5.4", Tool: "codex", InputTokens: i64(9_000_000), TotalTokens: i64(9_000_000)},
		{EventID: "b06-3", Timestamp: now.Add(-12 * time.Hour).Format(time.RFC3339Nano), MachineID: "m-b06", SessionID: "s-b06-3", Provider: "anthropic", Model: "claude-haiku-5-5", Tool: "claude-code", TotalTokens: i64(500_000)},
		{EventID: "b06-4", Timestamp: now.Add(-6 * time.Hour).Format(time.RFC3339Nano), MachineID: "m-b06", SessionID: "s-b06-4", Provider: "custom", Model: "mystery-pair-9b", Tool: "codex", TotalTokens: i64(200_000)},
	})
	c1 := s1.authClient(t)
	waitOverviewLifetimeTokens(t, c1, s1.BaseURL, 11_400_000)

	cAlias, _, _ := p3PostAliases(t, s1, c1, url.Values{
		"model_identity": {"gpt-5.4"},
		"model_alias":    {"Flagship GPT"},
	})
	if cAlias != http.StatusSeeOther && cAlias != http.StatusOK {
		t.Fatalf("POST /settings/aliases status = %d", cAlias)
	}

	setHTML := p3GetHTML(t, c1, s1.BaseURL+"/settings")
	gptIdx := strings.Index(setHTML, "gpt-5.4")
	haikuIdx := strings.Index(setHTML, "claude-haiku-5-5")
	if gptIdx < 0 || haikuIdx < 0 || gptIdx > haikuIdx {
		t.Fatalf("/settings models not sorted DESC by usage (gpt=%d, haiku=%d)", gptIdx, haikuIdx)
	}
	if !strings.Contains(setHTML, "Flagship GPT") || !strings.Contains(setHTML, "/tokedex") || !strings.Contains(setHTML, "model=gpt-5.4") {
		t.Fatalf("/settings missing Flagship GPT or cross-links")
	}

	anHTML := p3GetHTML(t, c1, s1.BaseURL+"/analytics?period=30d")
	if !strings.Contains(anHTML, `href="/tokedex#uncataloged-title"`) {
		t.Fatalf("/analytics missing /tokedex#uncataloged-title link")
	}

	itcWaitCatalog(t, c1, s1.BaseURL+"/api/v1/catalog", func(c itcCatalogSnapshot) bool {
		return c.ObservedTokens == 11_400_000 && c.UncatalogedModels == 1
	})
	tdHTML := p3GetHTML(t, c1, s1.BaseURL+"/tokedex")
	if !strings.Contains(tdHTML, `id="uncataloged-title"`) || !strings.Contains(tdHTML, "mystery-pair-9b") || !strings.Contains(tdHTML, `href="/settings`) || !strings.Contains(tdHTML, `href="/tokedex/gpt-5.4"`) {
		t.Fatalf("/tokedex missing required sections/links")
	}
	whatIf := itcExtractWhatIfSection(tdHTML)
	if whatIf == "" || (!strings.Contains(whatIf, "1,700,000") && !strings.Contains(whatIf, "1.7M") && !strings.Contains(whatIf, "$5.63") && !strings.Contains(whatIf, "$5.62")) {
		t.Fatalf("/tokedex 30d what-if missing 30d basis: %s", whatIf)
	}
	if strings.Contains(whatIf, "$0.00") {
		t.Fatalf("/tokedex 30d what-if fabricated $0.00")
	}
	if haikuCard, ok := itcExtractModelArticle(tdHTML, "claude-haiku-5-5"); !ok || strings.Contains(haikuCard, "$0.00") {
		t.Fatalf("/tokedex claude-haiku-5-5 missing or fabricated $0.00")
	}

	detHTML := p3GetHTML(t, c1, s1.BaseURL+"/tokedex/gpt-5.4")
	if !strings.Contains(detHTML, "s-b06-1") || !strings.Contains(detHTML, "http") {
		t.Fatalf("/tokedex/gpt-5.4 missing session or pricing source URL")
	}
	s1.Stop()

	// Restart with EnableTokedex: false.
	s2 := startTestServer(t, serverOpts{IngestToken: "p3-b06-tok", DBPath: dbPath, EnableTokedex: false})
	c2 := s2.authClient(t)
	for _, path := range []string{"/tokedex", "/tokedex/gpt-5.4"} {
		code, _, _ := p3Do(t, c2, http.MethodGet, s2.BaseURL+path, nil, map[string]string{"Accept": "text/html"})
		if code != http.StatusNotFound {
			t.Fatalf("disabled %s status = %d, want 404", path, code)
		}
	}
	anOff := p3GetHTML(t, c2, s2.BaseURL+"/analytics?period=30d")
	setOff := p3GetHTML(t, c2, s2.BaseURL+"/settings")
	if strings.Contains(anOff, "/tokedex#uncataloged-title") || strings.Contains(setOff, `href="/tokedex`) {
		t.Fatalf("disabled tokedex cross-links leaked in /analytics or /settings")
	}
	var cat itcCatalogSnapshot
	getJSONWithClient(t, c2, s2.BaseURL+"/api/v1/catalog", &cat)
	if cat.ObservedTokens != 11_400_000 {
		t.Fatalf("/api/v1/catalog unexpected when UI disabled: %+v", cat)
	}
}

func TestPairwise_B07_StatusCLICardSVGAuthAndPrivacyTelemetry(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{
		IngestToken:    "ing-tok-b07",
		DashboardToken: "dash-tok-b07",
		Username:       "admin",
		Password:       "pw-b07-123",
	})
	agentHome := filepath.Join(t.TempDir(), "agent-home")
	stateDB := filepath.Join(t.TempDir(), "state.db")
	now := time.Now().UTC().Add(-15 * time.Minute)
	p3WriteCodexJSONL(t, filepath.Join(agentHome, ".codex", "sessions", "2026", "11", "20", "b07.jsonl"), "sess-b07-priv", "/Users/alice/secret-proj", "gpt-5.4", now, 100, 0, 40, 140)
	if _, stderr, err := runAgentWithArgs(t, s, agentHome, stateDB, "host-b07-priv", "codex"); err != nil {
		t.Fatalf("agent --once failed: %v\nstderr: %s", err, stderr)
	}

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 140)
	cAlias, _, _ := p3PostAliases(t, s, client, url.Values{
		"machine_identity": {"host-b07-priv"},
		"machine_alias":    {"Secret Alias B07"},
	})
	if cAlias != http.StatusSeeOther && cAlias != http.StatusOK {
		t.Fatalf("POST /settings/aliases status = %d", cAlias)
	}

	c401, h401, _ := p3Do(t, s.Client, http.MethodGet, s.BaseURL+"/api/v1/card.svg", nil, nil)
	if c401 != http.StatusUnauthorized || !strings.Contains(h401.Get("WWW-Authenticate"), "Basic") {
		t.Fatalf("anon API card.svg status=%d www=%q", c401, h401.Get("WWW-Authenticate"))
	}
	c303, h303, _ := p3Do(t, s.Client, http.MethodGet, s.BaseURL+"/api/v1/card.svg", nil, map[string]string{"Accept": "text/html"})
	if c303 != http.StatusSeeOther || h303.Get("Location") != "/login?next=%2Fapi%2Fv1%2Fcard.svg" {
		t.Fatalf("anon HTML card.svg status=%d loc=%q", c303, h303.Get("Location"))
	}
	cIng, _, _ := p3Do(t, s.Client, http.MethodGet, s.BaseURL+"/api/v1/card.svg", nil, map[string]string{"Authorization": "Bearer ing-tok-b07"})
	if cIng != http.StatusUnauthorized {
		t.Fatalf("IngestToken on card.svg status=%d, want 401", cIng)
	}

	for _, doCard := range []func() (int, http.Header, []byte){
		func() (int, http.Header, []byte) {
			return p3Do(t, s.Client, http.MethodGet, s.BaseURL+"/api/v1/card.svg", nil, map[string]string{"Authorization": "Bearer dash-tok-b07"})
		},
		func() (int, http.Header, []byte) {
			return p3Do(t, client, http.MethodGet, s.BaseURL+"/api/v1/card.svg", nil, nil)
		},
	} {
		code, hdr, svgBytes := doCard()
		if code != http.StatusOK || !strings.Contains(hdr.Get("Content-Type"), "image/svg+xml") || hdr.Get("Cache-Control") != "no-store" {
			t.Fatalf("authed card.svg status=%d ct=%q cc=%q", code, hdr.Get("Content-Type"), hdr.Get("Cache-Control"))
		}
		itcValidateSVGXML(t, svgBytes)
		svgStr := string(svgBytes)
		svgLower := strings.ToLower(svgStr)
		for _, hex := range []string{"#10110f", "#9bbba0", "#d2a477"} {
			if !strings.Contains(svgLower, hex) {
				t.Fatalf("card.svg missing palette color %s", hex)
			}
		}
		svgText := itcExtractSVGText(t, svgBytes)
		if !strings.Contains(svgText, "Bytelet") || !strings.Contains(svgText, "140") {
			t.Fatalf("card.svg missing Bytelet or 140 in %q", svgText)
		}
		for _, leak := range []string{"secret-proj", "sess-b07-priv", "host-b07-priv", "Secret Alias B07", "SECRET_BASE_INSTR", "/Users/alice"} {
			if strings.Contains(svgStr, leak) {
				t.Fatalf("card.svg leaked %q", leak)
			}
		}
	}

	for _, extra := range [][]string{nil, {"--short"}, {"--json"}} {
		args := append([]string{"status", "--server", s.BaseURL, "--token", s.DashboardToken}, extra...)
		out, stderr, err := s.runCLI(t, nil, args...)
		if err != nil {
			t.Fatalf("tokemon %v failed: %v\nstderr: %s", args, err, stderr)
		}
		if !strings.Contains(out, "140") {
			t.Fatalf("tokemon %v missing 140: %s", args, out)
		}
		if len(extra) == 1 && extra[0] == "--short" && strings.Contains(strings.TrimSpace(out), "\n") {
			t.Fatalf("tokemon status --short must be single line: %q", out)
		}
		if len(extra) == 1 && extra[0] == "--json" {
			var parsed any
			if err := json.Unmarshal([]byte(out), &parsed); err != nil || !jsonContainsNumber(parsed, 140) {
				t.Fatalf("tokemon status --json invalid or missing 140: %s (%v)", out, err)
			}
		}
		for _, leak := range []string{"secret-proj", "sess-b07-priv", "/Users/alice", ".jsonl"} {
			if strings.Contains(out+stderr, leak) {
				t.Fatalf("tokemon %v leaked %q", args, leak)
			}
		}
	}
}

func TestPairwise_B08_AutomatedBackupSettingsAgeRestoreAndRestartIdempotency(t *testing.T) {
	requireE2E(t)
	workDir := t.TempDir()
	dbPath := filepath.Join(workDir, "tokemon.db")
	bakDir := filepath.Join(workDir, "backups")

	// Start with BackupInterval=0 to verify decoy filtering & "No backups yet".
	s0 := startTestServer(t, serverOpts{IngestToken: "p3-b08-tok", DBPath: dbPath, BackupDir: bakDir, BackupInterval: "0"})
	if err := os.MkdirAll(bakDir, 0o755); err != nil {
		t.Fatalf("mkdir bakDir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bakDir, "tokemon-before-v4-decoy.db"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write decoy: %v", err)
	}
	if err := os.WriteFile(filepath.Join(bakDir, "tokemon-backup-v4-decoy.db.tmp"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write tmp decoy: %v", err)
	}
	set0 := p3GetHTML(t, s0.authClient(t), s0.BaseURL+"/settings")
	if (!strings.Contains(set0, "No backups recorded yet") && !strings.Contains(set0, "No backups yet")) || !strings.Contains(set0, `<div class="backup-label">Retained</div>`) || !strings.Contains(set0, `<div class="backup-value">0</div>`) {
		t.Fatalf("/settings with only decoys missing empty backup status or Retained 0")
	}
	if err := os.Remove(filepath.Join(bakDir, "tokemon-before-v4-decoy.db")); err != nil {
		t.Fatalf("remove decoy: %v", err)
	}
	s0.Stop()

	s1 := startTestServer(t, serverOpts{IngestToken: "p3-b08-tok", DBPath: dbPath, BackupDir: bakDir, BackupInterval: "250ms", BackupRetention: 2})
	c1 := s1.authClient(t)
	evInit := ingestEvent{
		EventID:     "b08-codex-init",
		MachineID:   "m-b08",
		SessionID:   "s-b08-1",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(600),
		Source:      ingestSource{Adapter: "codex", AdapterVersion: "1.0"},
	}
	s1.ingestEvents(t, []ingestEvent{
		evInit,
		{EventID: "b08-claude-init", MachineID: "m-b08", SessionID: "s-b08-2", Provider: "anthropic", Model: "claude-sonnet-4", Tool: "claude-code", TotalTokens: i64(400)},
	})
	waitOverviewLifetimeTokens(t, c1, s1.BaseURL, 1000)
	if cA, _, _ := p3PostAliases(t, s1, c1, url.Values{"machine_identity": {"m-b08"}, "machine_alias": {"Box-B08"}}); cA != http.StatusSeeOther && cA != http.StatusOK {
		t.Fatalf("alias save failed: %d", cA)
	}
	if cQ, _, _ := ssSubmitQuotaCreate(t, s1, c1, url.Values{"label": {"Cap-B08"}, "provider": {"openai"}, "reset_day": {"1"}, "tokens_per_month": {"5000"}}, true); cQ != http.StatusOK {
		t.Fatalf("quota create failed: %d", cQ)
	}

	var autoFiles []string
	deadline := time.Now().Add(5 * time.Second)
	for {
		var err error
		autoFiles, err = filepath.Glob(filepath.Join(bakDir, "*-backup-v*.db"))
		if err != nil {
			t.Fatalf("glob backups: %v", err)
		}
		if len(autoFiles) >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for automated backup in %s", bakDir)
		}
		time.Sleep(100 * time.Millisecond)
	}
	time.Sleep(600 * time.Millisecond)
	set1 := p3GetHTML(t, c1, s1.BaseURL+"/settings")
	if strings.Contains(set1, "No backups yet") || (!strings.Contains(set1, "just now") && !strings.Contains(set1, "ago") && !strings.Contains(set1, "tokemon-backup-v")) {
		t.Fatalf("/settings missing backup age after automated backup")
	}

	// Capture a verified 1000-token snapshot before adding +500 extra tokens.
	snapOut, snapErr, err := s1.runCLI(t, nil, "backup", "create", "--destination", bakDir, "--database", dbPath, "--retention", "10")
	if err != nil {
		t.Fatalf("backup create failed: %v\nstderr: %s", err, snapErr)
	}
	var snapRes struct {
		Path           string `json:"path"`
		LifetimeTokens int64  `json:"lifetime_tokens"`
	}
	if err := json.Unmarshal([]byte(snapOut), &snapRes); err != nil || snapRes.LifetimeTokens != 1000 {
		t.Fatalf("unexpected snapRes: %+v (%v)", snapRes, err)
	}

	s1.ingestEvents(t, []ingestEvent{{EventID: "b08-extra", MachineID: "m-b08", SessionID: "s-b08-3", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(500)}})
	waitOverviewLifetimeTokens(t, c1, s1.BaseURL, 1500)
	s1.Stop()

	rOut, rErr, err := s1.runCLI(t, nil, "backup", "restore", "--source", snapRes.Path, "--database", dbPath, "--force")
	if err != nil || !strings.Contains(rOut, `"lifetime_tokens":1000`) {
		t.Fatalf("backup restore failed (%v, %s): %s", err, rErr, rOut)
	}

	for cycle := 1; cycle <= 2; cycle++ {
		sR := startTestServer(t, serverOpts{IngestToken: "p3-b08-tok", DBPath: dbPath, BackupDir: bakDir, BackupInterval: "0"})
		cR := sR.authClient(t)
		waitOverviewLifetimeTokens(t, cR, sR.BaseURL, 1000)
		setR := p3GetHTML(t, cR, sR.BaseURL+"/settings")
		if !strings.Contains(setR, "Box-B08") || !strings.Contains(setR, "Cap-B08") {
			t.Fatalf("restart cycle %d lost alias Box-B08 or quota Cap-B08", cycle)
		}
		dupRes := sR.ingestEvents(t, []ingestEvent{evInit})
		if dupRes.Duplicates != 1 || dupRes.Accepted != 0 {
			t.Fatalf("restart cycle %d dedup failed: %+v", cycle, dupRes)
		}
		migFiles, err := filepath.Glob(filepath.Join(bakDir, "*-before-v[0-9]*.db"))
		if err != nil {
			t.Fatalf("glob migration backups: %v", err)
		}
		if len(migFiles) != 0 {
			t.Fatalf("unexpected migration backup files on restart %d: %v", cycle, migFiles)
		}
		sR.Stop()
	}
}

func TestPairwise_B09_OverviewDrillDownsLiveUpdateHealthGroupingAndLimit8(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "p3-b09-tok"})
	now := time.Now().UTC()
	stale := now.Add(-2 * time.Hour)
	events := make([]ingestEvent, 0, 10)
	for i := 0; i < 10; i++ {
		proj := "proj-b09-a"
		model := "gpt-5.4"
		tool := "codex"
		prov := "openai"
		if i%2 == 1 {
			proj = "proj-b09-b"
			model = "claude-sonnet-4"
			tool = "claude-code"
			prov = "anthropic"
		}
		events = append(events, ingestEvent{
			EventID:      fmt.Sprintf("b09-ev-%02d", i),
			Timestamp:    now.Add(-time.Duration(20-i) * time.Minute).Format(time.RFC3339Nano),
			MachineID:    "m-b09",
			SessionID:    fmt.Sprintf("s-%02d", i),
			Project:      proj,
			Provider:     prov,
			Model:        model,
			Tool:         tool,
			InputTokens:  i64(80),
			OutputTokens: i64(20),
			TotalTokens:  i64(100),
		})
	}
	s.ingestEvents(t, events)
	s.sendHeartbeat(t, "m-b09", "codex",
		heartbeatSource{Adapter: "codex", IdentityHash: "sha256:b1-" + strings.Repeat("a", 50), LastSuccessfulSync: &now},
		heartbeatSource{Adapter: "codex", IdentityHash: "sha256:b2-" + strings.Repeat("b", 50), LastSuccessfulSync: &now},
		heartbeatSource{Adapter: "codex", IdentityHash: "sha256:b3-" + strings.Repeat("c", 50), LastSuccessfulSync: &stale},
		heartbeatSource{Adapter: "codex", IdentityHash: "sha256:b4-" + strings.Repeat("d", 50), LastSuccessfulSync: &now, LastErrorClass: "timeout"},
	)

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 1000)
	ovHTML := p3WaitHTMLContains(t, client, s.BaseURL+"/", "codex · 1 stale · 3 fresh")

	comp := p3SectionBetween(t, ovHTML, `id="token-composition"`, `class="progress-block"`)
	if !strings.Contains(comp, `data-display="800"`) || !strings.Contains(comp, `data-display="200"`) {
		t.Fatalf("overview composition missing pre-rendered 800/200: %s", comp)
	}
	sessSec := p3SectionBetween(t, ovHTML, `<section class="panel sessions-panel"`, `</section>`)
	if got := strings.Count(sessSec, `data-label="When"`); got != 8 {
		t.Fatalf("overview recent sessions = %d, want 8", got)
	}
	if strings.Contains(sessSec, "s-00") || strings.Contains(sessSec, "s-01") {
		t.Fatalf("overview recent sessions leaked oldest s-00/s-01")
	}
	healthSec := p3SectionBetween(t, ovHTML, `<section class="panel health-panel"`, `</section>`)
	if !strings.Contains(healthSec, "timeout") || !strings.Contains(healthSec, `<details class="source-details">`) || len(ssExtractTimeElems(healthSec)) == 0 {
		t.Fatalf("health-panel missing timeout error summary, collapsed details, or <time>: %s", healthSec)
	}
	overviewAssertNoAutoRefreshReload(t, ovHTML)

	etagTarget, etag, baseHdrs := p3CaptureOverviewETag(t, client, s.BaseURL)
	if etag != "" {
		cond := map[string]string{"If-None-Match": etag}
		for k, v := range baseHdrs {
			cond[k] = v
		}
		c304, _, _ := p3Do(t, client, http.MethodGet, etagTarget, nil, cond)
		if c304 != http.StatusNotModified {
			t.Fatalf("conditional GET %s status = %d, want 304", etagTarget, c304)
		}
	} else if !strings.Contains(ovHTML, "fetch(") && !strings.Contains(ovHTML, "EventSource(") {
		t.Fatalf("/ HTML missing ETag and missing fetch(/EventSource( in-place update script")
	}

	s.ingestEvents(t, []ingestEvent{{
		EventID:      "b09-ev-10",
		Timestamp:    now.Add(-1 * time.Minute).Format(time.RFC3339Nano),
		MachineID:    "m-b09",
		SessionID:    "s-10",
		Project:      "proj-b09-a",
		Provider:     "openai",
		Model:        "gpt-5.4",
		Tool:         "codex",
		InputTokens:  i64(80),
		OutputTokens: i64(20),
		TotalTokens:  i64(100),
	}})
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 1100)
	if etag != "" {
		cond := map[string]string{"If-None-Match": etag}
		for k, v := range baseHdrs {
			cond[k] = v
		}
		deadline := time.Now().Add(4 * time.Second)
		for {
			c200, hdr200, _ := p3Do(t, client, http.MethodGet, etagTarget, nil, cond)
			if c200 == http.StatusOK && hdr200.Get("ETag") != etag {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("ETag did not rotate after 11th ingest")
			}
			time.Sleep(100 * time.Millisecond)
		}
	}

	ovHTML2 := p3WaitHTMLContains(t, client, s.BaseURL+"/", "s-10")
	for _, dc := range []struct {
		caption string
		param   string
		wantVal string
		wantTok string
		exclude string
	}{
		{"Project token usage", "project", "proj-b09-a", "600", "proj-b09-b"},
		{"Harness token usage", "tool", "codex", "600", "proj-b09-b"},
		{"Model token usage", "model", "gpt-5.4", "600", "proj-b09-b"},
		{"Machine token usage", "machine", "m-b09", "1,100", ""},
	} {
		tbl := p3SectionBetween(t, ovHTML2, fmt.Sprintf(`<table aria-label="%s">`, dc.caption), `</table>`)
		hrefs := p3ExtractHrefs(tbl)
		if len(hrefs) == 0 {
			t.Fatalf("%s table missing drill-down links", dc.caption)
		}
		u, err := url.Parse(hrefs[0])
		if err != nil {
			t.Fatalf("%s invalid drill-down href %q: %v", dc.caption, hrefs[0], err)
		}
		if got := u.Query().Get(dc.param); got != dc.wantVal {
			t.Fatalf("%s drill-down %q param %s=%q, want %q", dc.caption, hrefs[0], dc.param, got, dc.wantVal)
		}
		targetHTML := p3GetHTML(t, client, s.BaseURL+hrefs[0])
		stripped := analyticsStripForms(targetHTML)
		if strings.TrimSpace(stripped) == "" || !strings.Contains(stripped, dc.wantVal) || !strings.Contains(stripped, dc.wantTok) {
			t.Fatalf("drill-down %s missing expected %q / %q", hrefs[0], dc.wantVal, dc.wantTok)
		}
		if dc.exclude != "" && strings.Contains(stripped, dc.exclude) {
			t.Fatalf("drill-down %s leaked excluded %q", hrefs[0], dc.exclude)
		}
	}
}

func TestPairwise_B10_InsightsRovingTabindexCAR205TimezoneAndSWR(t *testing.T) {
	requireE2E(t)
	nyLoc := p3LoadLoc(t, "America/New_York")
	s := startTestServer(t, serverOpts{
		IngestToken:   "p3-b10-tok",
		Password:      "ins-pw-123",
		EnableTokedex: true,
		Timezone:      "America/New_York",
	})

	nowNY := time.Now().In(nyLoc)
	yestNY := time.Date(nowNY.Year(), nowNY.Month(), nowNY.Day(), 0, 0, 0, 0, nyLoc).AddDate(0, 0, -1)
	peakTS := time.Date(yestNY.Year(), yestNY.Month(), yestNY.Day(), 22, 30, 0, 0, nyLoc)
	offPeakTS := time.Date(yestNY.Year(), yestNY.Month(), yestNY.Day(), 9, 0, 0, 0, nyLoc)

	s.ingestEvents(t, []ingestEvent{
		{EventID: "b10-1", Timestamp: peakTS.UTC().Format(time.RFC3339Nano), MachineID: "m-b10-a", SessionID: "s-b10-1", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(1500)},
		{EventID: "b10-2", Timestamp: offPeakTS.UTC().Format(time.RFC3339Nano), MachineID: "m-b10-b", SessionID: "s-b10-2", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(300)},
	})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 1800)

	ins := itcWaitInsights(t, client, s.BaseURL+"/api/v1/insights", func(i itcInsightsPayload) bool {
		return i.DataQuality.KnownTokens == 1800
	})
	if ins.PeakHour != "22:00" {
		t.Fatalf("America/New_York PeakHour = %q, want 22:00", ins.PeakHour)
	}

	insHTML := p3GetHTML(t, client, s.BaseURL+"/insights")
	rhythmSec := p3SectionBetween(t, insHTML, "24-hour rhythm", "Weekday distribution")
	buttons := itcHourButtonRe.FindAllStringSubmatch(rhythmSec, -1)
	if len(buttons) != 24 {
		t.Fatalf("24-hour rhythm button count = %d, want 24", len(buttons))
	}
	zeroCnt := 0
	var activeAttrs string
	for _, b := range buttons {
		if strings.Contains(b[1], `tabindex="0"`) {
			zeroCnt++
			activeAttrs = b[1]
		}
	}
	if zeroCnt != 1 || !strings.Contains(activeAttrs, "22:00") {
		t.Fatalf("roving tabindex=0 count=%d attrs=%q, want 1 on 22:00", zeroCnt, activeAttrs)
	}
	if _, ok := itcExtractBalancedAriaLive(rhythmSec); !ok {
		t.Fatalf("/insights missing aria-live=\"polite\" readout")
	}
	if srTbl, ok := itcExtractSROnlyTable(rhythmSec); !ok || !strings.Contains(srTbl, "00:00") || !strings.Contains(srTbl, "23:00") {
		t.Fatalf("/insights missing .sr-only hourly table")
	}
	for _, key := range []string{"ArrowRight", "ArrowLeft", "Home", "End"} {
		if !strings.Contains(insHTML, key) {
			t.Fatalf("/insights missing keyboard handler key %q", key)
		}
	}

	// CAR-205 across all pages including detail routes.
	for _, p := range []string{"/", "/analytics", "/sessions", "/sessions/s-b10-1", "/settings", "/insights", "/tokedex", "/tokedex/gpt-5.4"} {
		body := p3GetHTML(t, client, s.BaseURL+p)
		nav := ssExtractNavBlock(t, p, body)
		if strings.Contains(nav, `href="/insights"`) {
			t.Fatalf("CAR-205 violation on %s: /insights linked inside .nav", p)
		}
	}

	// Alias change invalidates /insights SWR immediately + machine filter keys remain isolated.
	if cA, _, _ := p3PostAliases(t, s, client, url.Values{"machine_identity": {"m-b10-a"}, "machine_alias": {"NY Workstation"}}); cA != http.StatusSeeOther && cA != http.StatusOK {
		t.Fatalf("alias save status = %d", cA)
	}
	var insA, insB itcInsightsPayload
	getJSONWithClient(t, client, s.BaseURL+"/api/v1/insights?machine=m-b10-a", &insA)
	getJSONWithClient(t, client, s.BaseURL+"/api/v1/insights?machine=m-b10-b", &insB)
	if insA.DataQuality.KnownTokens != 1500 || insB.DataQuality.KnownTokens != 300 {
		t.Fatalf("insights machine SWR isolation failed: m-b10-a=%d m-b10-b=%d", insA.DataQuality.KnownTokens, insB.DataQuality.KnownTokens)
	}
}

func TestPairwise_B11_MachineAliasesBreakdownLegendUsageSortAndRawCSV(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "p3-b11-tok"})
	s.ingestEvents(t, []ingestEvent{
		{EventID: "b11-1", MachineID: "m-zulu", SessionID: "s-1", Provider: "openai", Model: "m-zulu", Tool: "codex", InputTokens: i64(200_000), CacheReadTokens: i64(800_000), TotalTokens: i64(1_000_000)},
		{EventID: "b11-2", MachineID: "m-yankee", SessionID: "s-2", Provider: "openai", Model: "gpt-5.4", Tool: "codex", InputTokens: i64(450_000), CacheReadTokens: i64(50_000), TotalTokens: i64(500_000)},
		{EventID: "b11-3", MachineID: "m-xray", SessionID: "s-3", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(400_000)},
		{EventID: "b11-4", MachineID: "m-whiskey", SessionID: "s-4", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(300_000)},
		{EventID: "b11-5", MachineID: "m-victor", SessionID: "s-5", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(200_000)},
		{EventID: "b11-6", MachineID: "m-alpha", SessionID: "s-6", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(100_000)},
	})
	client := s.authClient(t)
	p3WaitAnalyticsTokens(t, client, s.BaseURL+"/api/v1/analytics?period=30d&dimension=machines", 2_500_000)

	cAlias, _, _ := p3PostAliases(t, s, client, url.Values{
		"machine_identity": {"m-zulu", "m-yankee", "m-xray", "m-whiskey", "m-victor", "m-alpha"},
		"machine_alias":    {"Alias1", "Alias2", "Alias3", "Alias4", "Alias5", "Alias6"},
		"model_identity":   {"m-zulu"},
		"model_alias":      {"Model-Only-Alias"},
	})
	if cAlias != http.StatusSeeOther && cAlias != http.StatusOK {
		t.Fatalf("POST /settings/aliases status = %d", cAlias)
	}

	setHTML := p3GetHTML(t, client, s.BaseURL+"/settings")
	machSec := p3SectionBetween(t, setHTML, `id="machines-title"`, `</form>`)
	lastIdx := -1
	for _, id := range []string{"m-zulu", "m-yankee", "m-xray", "m-whiskey", "m-victor", "m-alpha"} {
		idx := strings.Index(machSec, id)
		if idx < 0 || idx <= lastIdx {
			t.Fatalf("/settings machines not sorted DESC by usage (%s idx=%d prev=%d)", id, idx, lastIdx)
		}
		lastIdx = idx
	}

	anHTML := p3GetHTML(t, client, s.BaseURL+"/analytics?period=30d&dimension=machines")
	bd := p3SectionBetween(t, anHTML, `class="panel breakdown-panel"`, `class="panel share-panel"`)
	share := p3SectionBetween(t, anHTML, `class="panel share-panel"`, `class="panel sessions-panel"`)
	for _, a := range []string{"Alias1", "Alias2", "Alias3", "Alias4", "Alias5", "Alias6"} {
		if !strings.Contains(bd, fmt.Sprintf(`<span class="breakdown-name">%s</span>`, a)) {
			t.Fatalf("breakdown missing %s: %s", a, bd)
		}
	}
	for _, a := range []string{"Alias1", "Alias2", "Alias3", "Alias4", "Alias5"} {
		if !strings.Contains(share, fmt.Sprintf(`<strong>%s</strong>`, a)) {
			t.Fatalf("share legend missing %s: %s", a, share)
		}
	}
	if !strings.Contains(share, "Other") {
		t.Fatalf("6-machine share legend missing Other bucket: %s", share)
	}
	if strings.Contains(bd, "Model-Only-Alias") || strings.Contains(share, "Model-Only-Alias") {
		t.Fatalf("machine dimension leaked Model-Only-Alias")
	}
	if !strings.Contains(bd, "80.0%") || !strings.Contains(bd, "10.0%") {
		t.Fatalf("machine breakdown missing per-row cache hit %% alongside aliases: %s", bd)
	}

	cExp, _, csvBytes := p3Do(t, client, http.MethodGet, s.BaseURL+"/api/v1/analytics/export?period=30d&dimension=machines&format=csv", nil, nil)
	if cExp != http.StatusOK {
		t.Fatalf("CSV export status = %d", cExp)
	}
	csvStr := string(csvBytes)
	for _, id := range []string{"m-zulu", "m-yankee", "m-xray", "m-whiskey", "m-victor", "m-alpha"} {
		if !strings.Contains(csvStr, ","+id+",") {
			t.Fatalf("CSV export missing raw machine ID %q: %s", id, csvStr)
		}
	}
	if strings.Contains(csvStr, "Alias1") || strings.Contains(csvStr, "Alias6") {
		t.Fatalf("CSV export leaked display aliases: %s", csvStr)
	}
}

func TestPairwise_B12_UnknownTokensAndUnpricedModelsAcrossAllSurfaces(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "p3-b12-tok", EnableTokedex: true, Timezone: "UTC"})
	client := s.authClient(t)

	cQ, _, bQ := ssSubmitQuotaCreate(t, s, client, url.Values{
		"label":          {"Cost Only Unpriced"},
		"provider":       {"anthropic"},
		"reset_day":      {"1"},
		"cost_per_month": {"50.0"},
	}, true)
	if cQ != http.StatusOK {
		t.Fatalf("create cost quota failed (%d): %s", cQ, string(bQ))
	}

	now := time.Now().UTC()
	todayUTC := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	tsD1 := todayUTC.AddDate(0, 0, -1).Add(12 * time.Hour)
	tsD0 := now.Add(-30 * time.Minute).Truncate(time.Second)
	if tsD0.Format("2006-01-02") != todayUTC.Format("2006-01-02") {
		tsD0 = todayUTC
	}
	s.ingestEvents(t, []ingestEvent{
		{EventID: "b12-1", Timestamp: tsD1.Format(time.RFC3339Nano), MachineID: "m-unk", SessionID: "sess-unk-1", Project: "proj-unk", Provider: "anthropic", Model: "claude-haiku-5-5", Tool: "claude-code", TokenAccuracy: "unknown"},
		{EventID: "b12-2", Timestamp: tsD0.Format(time.RFC3339Nano), MachineID: "m-unk", SessionID: "sess-unk-2", Project: "proj-unk", Provider: "custom", Model: "uncat-unk-model", Tool: "codex", TokenAccuracy: "unknown"},
	})

	// Wait for quota/analytics/catalog SWR to settle.
	quotas := p3WaitQuotas(t, client, s.BaseURL, func(rows []ssQuotaRow) bool {
		return len(rows) == 1 && rows[0].State == "unknown"
	})
	if quotas[0].CostUsed != nil {
		t.Fatalf("expected CostUsed == nil for unknown events, got %v", *quotas[0].CostUsed)
	}

	var ov overviewPayload
	deadline := time.Now().Add(4 * time.Second)
	for {
		getJSONWithClient(t, client, s.BaseURL+"/api/v1/analytics/overview", &ov)
		if ov.Activity.CurrentStreak == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected CurrentStreak=2 for unknown-token active days, got %+v", ov.Activity)
		}
		time.Sleep(100 * time.Millisecond)
	}
	if ov.Activity.LongestStreak != 2 || ov.Activity.MaxDailyTokens != 0 {
		t.Fatalf("unexpected activity for unknown events: %+v", ov.Activity)
	}

	ovHTML := p3GetHTML(t, client, s.BaseURL+"/")
	qSec := overviewQuotaSection(t, ovHTML, "Cost Only Unpriced")
	if !strings.Contains(strings.ToLower(qSec), "unknown") || strings.Contains(qSec, "$0.00") {
		t.Fatalf("/ quota bar must show Unknown (never $0.00): %s", qSec)
	}

	anHTML := p3GetHTML(t, client, s.BaseURL+"/analytics?period=30d")
	if strings.Contains(anHTML, "$0.00 saved") || strings.Contains(anHTML, "$0.00/1M") || strings.Contains(anHTML, "NaN") {
		t.Fatalf("/analytics fabricated $0.00 or NaN for unknown/unpriced events")
	}
	if !strings.Contains(anHTML, "Unknown") && !strings.Contains(anHTML, "—") {
		t.Fatalf("/analytics missing unknown/unpriced marker (Unknown or —)")
	}

	itcWaitCatalog(t, client, s.BaseURL+"/api/v1/catalog", func(c itcCatalogSnapshot) bool {
		return c.UncatalogedModels == 1
	})
	tdHTML := p3GetHTML(t, client, s.BaseURL+"/tokedex")
	haikuCard, ok := itcExtractModelArticle(tdHTML, "claude-haiku-5-5")
	if !ok || !strings.Contains(haikuCard, "Rates not published yet") || strings.Contains(haikuCard, "$0.00") {
		t.Fatalf("/tokedex claude-haiku-5-5 missing unpublished rates or showed $0.00")
	}
	whatIf := itcExtractWhatIfSection(tdHTML)
	if whatIf == "" {
		t.Fatalf("/tokedex missing 30d what-if section")
	}
	if strings.Contains(whatIf, "$0.00") {
		t.Fatalf("/tokedex what-if fabricated $0.00: %s", whatIf)
	}

	sessHTML := p3GetHTML(t, client, s.BaseURL+"/sessions")
	sessBody := ssTableBodyRe.FindString(sessHTML)
	if !strings.Contains(sessBody, "Unknown") || strings.Contains(sessBody, "0K") || strings.Contains(sessBody, "$0.00") {
		t.Fatalf("/sessions must render Unknown (never 0K or $0.00): %s", sessBody)
	}
	detHTML := p3GetHTML(t, client, s.BaseURL+"/sessions/sess-unk-1")
	if !strings.Contains(detHTML, "Unknown") || strings.Contains(detHTML, "$0.00") {
		t.Fatalf("/sessions/sess-unk-1 must render Unknown (never $0.00)")
	}
}
