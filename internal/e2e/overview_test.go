//go:build e2e

package e2e_test

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	stdhtml "html"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

type overviewMachineSource struct {
	Adapter            string  `json:"adapter"`
	SourceHash         string  `json:"source_hash"`
	LastSuccessfulSync *string `json:"last_successful_sync,omitempty"`
	LastErrorClass     string  `json:"last_error_class,omitempty"`
	State              string  `json:"state"`
	Age                string  `json:"age"`
}

type overviewMachineInfo struct {
	ID               string                  `json:"id"`
	Name             string                  `json:"name"`
	Status           string                  `json:"status"`
	LastSeenAt       string                  `json:"last_seen_at"`
	TodayTokens      int64                   `json:"today_tokens"`
	WeekTokens       int64                   `json:"week_tokens"`
	LifetimeTokens   int64                   `json:"lifetime_tokens"`
	SourceCount      int                     `json:"source_count"`
	SourceErrorCount int                     `json:"source_error_count"`
	SyncContext      string                  `json:"sync_context"`
	Sources          []overviewMachineSource `json:"sources"`
}

type overviewComposition struct {
	InputTokens         int64 `json:"input_tokens"`
	UncachedInputTokens int64 `json:"uncached_input_tokens"`
	CachedInputTokens   int64 `json:"cached_input_tokens"`
	OutputTokens        int64 `json:"output_tokens"`
	UnclassifiedTokens  int64 `json:"unclassified_tokens"`
}

type overviewMachineTotal struct {
	Machine string `json:"machine"`
	Tokens  int64  `json:"tokens"`
}

type overviewActivity struct {
	StartDate      string          `json:"start_date"`
	EndDate        string          `json:"end_date"`
	TotalTokens    int64           `json:"total_tokens"`
	ActiveDays     int64           `json:"active_days"`
	MaxDailyTokens int64           `json:"max_daily_tokens"`
	CurrentStreak  int64           `json:"current_streak"`
	LongestStreak  int64           `json:"longest_streak"`
	PeakDay        json.RawMessage `json:"peak_day"`
}

type overviewPayload struct {
	LifetimeTokens int64                  `json:"lifetime_tokens"`
	Timezone       string                 `json:"timezone"`
	Today          json.RawMessage        `json:"today"`
	Week           json.RawMessage        `json:"week"`
	Evolution      json.RawMessage        `json:"evolution"`
	Composition    overviewComposition    `json:"composition"`
	Activity       overviewActivity       `json:"activity"`
	ByModel        json.RawMessage        `json:"by_model"`
	ByTool         json.RawMessage        `json:"by_tool"`
	ByMachine      []overviewMachineTotal `json:"by_machine"`
	ByProject      json.RawMessage        `json:"by_project"`
	EstimatedCost  json.RawMessage        `json:"estimated_cost"`
	Cache          json.RawMessage        `json:"cache"`
	Threads        int64                  `json:"threads"`
	Machines       []overviewMachineInfo  `json:"machines"`
}

type overviewQuotaStatus struct {
	Provider   string   `json:"provider"`
	Model      string   `json:"model"`
	Label      string   `json:"label"`
	TokensUsed int64    `json:"tokens_used"`
	CostUsed   *float64 `json:"cost_used"`
	State      string   `json:"state"`
}

func overviewGetResp(t *testing.T, client *http.Client, fullURL string, headers map[string]string) (int, http.Header, []byte) {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, fullURL, nil)
	if err != nil {
		t.Fatalf("new GET %s: %v", fullURL, err)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET %s: %v", fullURL, err)
	}
	body, readErr := io.ReadAll(resp.Body)
	closeErr := resp.Body.Close()
	if readErr != nil {
		t.Fatalf("read GET %s body: %v", fullURL, readErr)
	}
	if closeErr != nil {
		t.Fatalf("close GET %s body: %v", fullURL, closeErr)
	}
	return resp.StatusCode, resp.Header, body
}

func overviewGetHTML(t *testing.T, client *http.Client, fullURL string) string {
	t.Helper()
	code, _, body := overviewGetResp(t, client, fullURL, map[string]string{"Accept": "text/html"})
	if code != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200; body=%s", fullURL, code, string(body))
	}
	return string(body)
}

func overviewLoadLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("LoadLocation(%q): %v", name, err)
	}
	return loc
}

func overviewWaitHTMLContains(t *testing.T, client *http.Client, baseURL, needle string) string {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
	var last string
	for {
		last = overviewGetHTML(t, client, baseURL+"/")
		if strings.Contains(last, needle) {
			return last
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for / HTML to contain %q", needle)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func overviewSendRawHeartbeat(t *testing.T, s *testServer, rawJSON []byte) (int, []byte) {
	t.Helper()
	headers := map[string]string{"Content-Type": "application/json"}
	if s.IngestToken != "" {
		headers["Authorization"] = "Bearer " + s.IngestToken
	}
	resp, body := s.doReq(t, http.MethodPost, "/api/v1/agents/heartbeat", rawJSON, headers)
	return resp.StatusCode, body
}

func overviewSectionBetween(t *testing.T, html, startMarker, endMarker string) string {
	t.Helper()
	start := strings.Index(html, startMarker)
	if start < 0 {
		t.Fatalf("section start marker %q not found in HTML", startMarker)
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

var overviewHrefRe = regexp.MustCompile(`href="([^"]+)"`)

func overviewExtractHrefs(fragment string) []string {
	matches := overviewHrefRe.FindAllStringSubmatch(fragment, -1)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		if len(m) >= 2 {
			out = append(out, stdhtml.UnescapeString(m[1]))
		}
	}
	return out
}

var overviewFormRe = regexp.MustCompile(`(?is)<form\b[^>]*>.*?</form>`)
var overviewTagRe = regexp.MustCompile(`<[^>]+>`)
var overviewSpaceRe = regexp.MustCompile(`\s+`)

func overviewStripForms(html string) string {
	return overviewFormRe.ReplaceAllString(html, " ")
}

func overviewAutoRefreshTickBody(html string) (string, bool) {
	idx := -1
	for _, decl := range []string{"const autoRefreshTick", "let autoRefreshTick", "var autoRefreshTick", "function autoRefreshTick"} {
		if i := strings.Index(html, decl); i >= 0 {
			idx = i
			break
		}
	}
	if idx < 0 {
		idx = strings.Index(html, "autoRefreshTick")
	}
	if idx < 0 {
		return "", false
	}
	rest := html[idx:]
	if len(rest) > 1500 {
		rest = rest[:1500]
	}
	braceStart := strings.IndexByte(rest, '{')
	semiBefore := strings.IndexByte(rest, ';')
	if braceStart < 0 || (semiBefore >= 0 && semiBefore < braceStart) {
		if semiBefore >= 0 {
			return rest[:semiBefore+1], true
		}
		return rest, true
	}
	depth := 0
	for i := braceStart; i < len(rest); i++ {
		switch rest[i] {
		case '{':
			depth++
		case '}':
			depth--
			if depth == 0 {
				return rest[:i+1], true
			}
		}
	}
	return rest, true
}

func overviewAssertNoAutoRefreshReload(t *testing.T, html string) {
	t.Helper()
	if body, ok := overviewAutoRefreshTickBody(html); ok && strings.Contains(body, "location.reload") {
		t.Fatalf("autoRefreshTick still calls location.reload(); expected smooth in-place update: %s", body)
	}
}

func overviewBalancedTagBlock(rest, tag string) string {
	openPrefix := "<" + tag
	closeTag := "</" + tag + ">"
	depth := 0
	lower := strings.ToLower(rest)
	for i := 0; i < len(lower); {
		if strings.HasPrefix(lower[i:], openPrefix) {
			nextIdx := i + len(openPrefix)
			if nextIdx == len(lower) || lower[nextIdx] == '>' || lower[nextIdx] == ' ' || lower[nextIdx] == '\t' || lower[nextIdx] == '\n' || lower[nextIdx] == '\r' || lower[nextIdx] == '/' {
				depth++
				i = nextIdx
				continue
			}
		}
		if strings.HasPrefix(lower[i:], closeTag) {
			depth--
			i += len(closeTag)
			if depth == 0 {
				return rest[:i]
			}
			continue
		}
		i++
	}
	return ""
}

func overviewQuotaSection(t *testing.T, html, label string) string {
	t.Helper()
	afterHeader := html
	if idx := strings.Index(html, "</header>"); idx >= 0 {
		afterHeader = html[idx+len("</header>"):]
	}
	for _, marker := range []string{`class="panel quota-panel"`, `class="quota-panel"`, `class="quota-pace"`, `class="quota-bar"`, `class="quota-list"`, `class="budget-pace"`} {
		if start := strings.Index(afterHeader, marker); start >= 0 {
			tagStart := strings.LastIndex(afterHeader[:start], "<")
			if tagStart >= 0 {
				tagRest := afterHeader[tagStart:]
				lowerTag := strings.ToLower(tagRest)
				var sec string
				switch {
				case strings.HasPrefix(lowerTag, "<section"):
					sec = overviewBalancedTagBlock(tagRest, "section")
				case strings.HasPrefix(lowerTag, "<div"):
					sec = overviewBalancedTagBlock(tagRest, "div")
				case strings.HasPrefix(lowerTag, "<article"):
					sec = overviewBalancedTagBlock(tagRest, "article")
				default:
					if end := strings.Index(afterHeader[start:], "</div>"); end >= 0 {
						sec = afterHeader[start : start+end]
					}
				}
				if sec != "" && strings.Contains(sec, label) {
					return sec
				}
			}
		}
	}
	labelIdx := strings.Index(afterHeader, label)
	if labelIdx < 0 {
		t.Fatalf("quota label %q not found in / body", label)
	}
	start := labelIdx - 120
	if start < 0 {
		start = 0
	}
	for _, guard := range []string{`id="lifetime-counter"`, `id="live-progress"`, `</article>`} {
		if g := strings.LastIndex(afterHeader[:labelIdx], guard); g >= 0 && g+len(guard) > start {
			start = g + len(guard)
		}
	}
	if sIdx := strings.LastIndex(afterHeader[:labelIdx], `<div class="quota`); sIdx >= 0 && sIdx > start {
		start = sIdx
	} else if sIdx := strings.LastIndex(afterHeader[:labelIdx], "<section"); sIdx >= 0 && sIdx > start {
		if !strings.Contains(afterHeader[sIdx:labelIdx], "lifetime-counter") {
			start = sIdx
		}
	}
	end := labelIdx + len(label) + 320
	if end > len(afterHeader) {
		end = len(afterHeader)
	}
	for _, stop := range []string{"</section>", "</article>", `class="content-grid"`, `class="sessions-panel"`} {
		if sIdx := strings.Index(afterHeader[labelIdx:], stop); sIdx >= 0 && labelIdx+sIdx < end {
			end = labelIdx + sIdx
		}
	}
	return afterHeader[start:end]
}

func overviewStripActivityLegend(s string) string {
	idx := strings.Index(s, `<div class="activity-legend"`)
	if idx < 0 {
		return s
	}
	rest := s[idx:]
	depth := 0
	for i := 0; i < len(rest); {
		if strings.HasPrefix(rest[i:], "<div") {
			depth++
			i += 4
			continue
		}
		if strings.HasPrefix(rest[i:], "</div>") {
			depth--
			i += 6
			if depth == 0 {
				return s[:idx] + rest[i:]
			}
			continue
		}
		i++
	}
	return s[:idx]
}

func overviewHasStreakValue(actHeaderNoLegend string, n int) bool {
	plain := overviewSpaceRe.ReplaceAllString(overviewTagRe.ReplaceAllString(actHeaderNoLegend, " "), " ")
	re := regexp.MustCompile(fmt.Sprintf(`(?i)(\b%d\s*d\b|\b%d\s+days?\b|\b%d-day\b|(?:streak|current|longest|best)[^0-9]{0,20}\b%d\b|\b%d\b[^0-9]{0,20}(?:streak|current|longest|best))`, n, n, n, n, n))
	return re.MatchString(plain)
}

func overviewHasToken(s, sub string) bool {
	return strings.Contains(s, sub)
}

func overviewParsePeakDayDate(raw json.RawMessage) string {
	trimmed := strings.TrimSpace(string(raw))
	if trimmed == "" || trimmed == "null" {
		return ""
	}
	var asStr string
	if err := json.Unmarshal(raw, &asStr); err == nil {
		return asStr
	}
	var asObj map[string]any
	if err := json.Unmarshal(raw, &asObj); err == nil {
		for _, k := range []string{"date", "day", "Date", "Day"} {
			if v, ok := asObj[k].(string); ok && v != "" {
				return v
			}
		}
	}
	return ""
}

// =============================================================================
// Area 1: Feature 1 — machine_source_health adapter grouping & <details> (LANDED)
// =============================================================================

func TestOverview_F1_T1_MultiSourceSingleAdapterSummary(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "ov-f1-t1-1"})
	now := time.Now().UTC()

	sources := make([]heartbeatSource, 0, 12)
	for i := 0; i < 10; i++ {
		sources = append(sources, heartbeatSource{
			Adapter:            "codex",
			IdentityHash:       fmt.Sprintf("sha256:codex-%02d-%s", i, strings.Repeat("a", 48)),
			LastSuccessfulSync: &now,
		})
	}
	for i := 0; i < 2; i++ {
		sources = append(sources, heartbeatSource{
			Adapter:            "claude-code",
			IdentityHash:       fmt.Sprintf("sha256:claude-%02d-%s", i, strings.Repeat("b", 48)),
			LastSuccessfulSync: &now,
		})
	}
	s.sendHeartbeat(t, "m-f1-multi", "codex", sources...)
	s.ingestEvents(t, []ingestEvent{{
		EventID:      "ov-f1-t1-1-ev",
		MachineID:    "m-f1-multi",
		SessionID:    "sess-f1-1",
		Provider:     "openai",
		Model:        "gpt-5.4",
		Tool:         "codex",
		InputTokens:  i64(70),
		OutputTokens: i64(30),
		TotalTokens:  i64(100),
	}})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 100)
	html := overviewWaitHTMLContains(t, client, s.BaseURL, "codex · 10 fresh")

	health := overviewSectionBetween(t, html, `<section class="panel health-panel"`, `</section>`)
	if got := strings.Count(health, `class="source-health-line"`); got != 2 {
		t.Fatalf("source-health-line count = %d, want 2; health=%s", got, health)
	}
	if !strings.Contains(health, "codex · 10 fresh") {
		t.Fatalf("expected 'codex · 10 fresh' in health section")
	}
	if !strings.Contains(health, "claude-code · 2 fresh") {
		t.Fatalf("expected 'claude-code · 2 fresh' in health section")
	}
	if !strings.Contains(health, `<details class="source-details">`) {
		t.Fatalf("expected collapsed <details class=\"source-details\"> without open attribute")
	}
	if strings.Contains(health, `<details class="source-details" open`) {
		t.Fatalf("expected source-details to be collapsed by default")
	}
	if !strings.Contains(health, "12 sources across 2 adapters") {
		t.Fatalf("expected summary '12 sources across 2 adapters' in health section")
	}
}

func TestOverview_F1_T1_AttentionFirstOrdering(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "ov-f1-t1-2"})
	fresh := time.Now().UTC()
	stale := fresh.Add(-2 * time.Hour)

	s.sendHeartbeat(t, "m-f1-order", "codex",
		heartbeatSource{
			Adapter:            "alpha-adapter",
			IdentityHash:       "sha256:alpha-" + strings.Repeat("1", 56),
			LastSuccessfulSync: &fresh,
		},
		heartbeatSource{
			Adapter:            "beta-adapter",
			IdentityHash:       "sha256:beta-" + strings.Repeat("2", 57),
			LastSuccessfulSync: &stale,
		},
		heartbeatSource{
			Adapter:            "zeta-adapter",
			IdentityHash:       "sha256:zeta-" + strings.Repeat("3", 57),
			LastSuccessfulSync: &fresh,
			LastErrorClass:     "timeout",
		},
	)
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "ov-f1-t1-2-ev",
		MachineID:   "m-f1-order",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(50),
	}})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 50)
	html := overviewWaitHTMLContains(t, client, s.BaseURL, "zeta-adapter")

	sh := overviewSectionBetween(t, html, `<span class="source-health">`, `</td>`)
	zetaIdx := strings.Index(sh, "zeta-adapter")
	betaIdx := strings.Index(sh, "beta-adapter")
	alphaIdx := strings.Index(sh, "alpha-adapter")
	if zetaIdx < 0 || betaIdx < 0 || alphaIdx < 0 {
		t.Fatalf("missing expected adapter lines in %s", sh)
	}
	if !(zetaIdx < betaIdx && betaIdx < alphaIdx) {
		t.Fatalf("expected errored zeta (%d) < stale beta (%d) < fresh alpha (%d)", zetaIdx, betaIdx, alphaIdx)
	}
}

func TestOverview_F1_T1_MixedStatesInsideOneAdapter(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "ov-f1-t1-3"})
	fresh := time.Now().UTC()
	stale := fresh.Add(-2 * time.Hour)

	sources := []heartbeatSource{
		{Adapter: "codex", IdentityHash: "sha256:s1-" + strings.Repeat("a", 50), LastSuccessfulSync: &stale},
		{Adapter: "codex", IdentityHash: "sha256:s2-" + strings.Repeat("b", 50), LastSuccessfulSync: &stale},
		{Adapter: "codex", IdentityHash: "sha256:u1-" + strings.Repeat("c", 50), LastSuccessfulSync: nil},
		{Adapter: "codex", IdentityHash: "sha256:f1-" + strings.Repeat("d", 50), LastSuccessfulSync: &fresh},
		{Adapter: "codex", IdentityHash: "sha256:f2-" + strings.Repeat("e", 50), LastSuccessfulSync: &fresh},
		{Adapter: "codex", IdentityHash: "sha256:f3-" + strings.Repeat("f", 50), LastSuccessfulSync: &fresh},
	}
	s.sendHeartbeat(t, "m-f1-mixed", "codex", sources...)
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "ov-f1-t1-3-ev",
		MachineID:   "m-f1-mixed",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(25),
	}})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 25)
	html := overviewWaitHTMLContains(t, client, s.BaseURL, "codex · 2 stale · 1 never synced · 3 fresh")

	health := overviewSectionBetween(t, html, `<section class="panel health-panel"`, `</section>`)
	if !strings.Contains(health, `status-pill stale">Stale</span> codex · 2 stale · 1 never synced · 3 fresh`) {
		t.Fatalf("unexpected mixed-state summary in %s", health)
	}
	if !strings.Contains(health, "3 stale or errored (6 total)") {
		t.Fatalf("expected '3 stale or errored (6 total)' in %s", health)
	}
	if got := strings.Count(health, `class="source-detail-line"`); got != 3 {
		t.Fatalf("source-detail-line count = %d, want 3 problem sources", got)
	}
}

func TestOverview_F1_T1_SingleSourcePerAdapterOmitsDetails(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "ov-f1-t1-4"})
	fresh := time.Now().UTC()
	stale := fresh.Add(-2 * time.Hour)

	claudeHash := "sha256:" + strings.Repeat("1", 64)
	codexHash := "sha256:" + strings.Repeat("2", 64)
	s.sendHeartbeat(t, "m-f1-single", "codex",
		heartbeatSource{Adapter: "claude-code", IdentityHash: claudeHash, LastSuccessfulSync: &fresh},
		heartbeatSource{Adapter: "codex", IdentityHash: codexHash, LastSuccessfulSync: &stale},
	)
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "ov-f1-t1-4-ev",
		MachineID:   "m-f1-single",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(40),
	}})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 40)
	html := overviewWaitHTMLContains(t, client, s.BaseURL, claudeHash)

	health := overviewSectionBetween(t, html, `<section class="panel health-panel"`, `</section>`)
	if got := strings.Count(health, `class="source-health-line"`); got != 2 {
		t.Fatalf("source-health-line count = %d, want 2", got)
	}
	if strings.Contains(health, `class="source-details"`) {
		t.Fatalf("did not expect <details class=\"source-details\"> when each adapter has 1 source")
	}
	if !strings.Contains(health, fmt.Sprintf(`title="%s"`, claudeHash)) || !strings.Contains(health, fmt.Sprintf(`title="%s"`, codexHash)) {
		t.Fatalf("expected individual source hash titles on single-source lines")
	}
	if !strings.Contains(health, "ago") {
		t.Fatalf("expected individual relative age on single-source lines")
	}
}

func TestOverview_F1_T1_APIMachinesPreservesAllRawSources(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "ov-f1-t1-5"})
	now := time.Now().UTC()

	sources := make([]heartbeatSource, 0, 61)
	for i := 0; i < 31; i++ {
		sources = append(sources, heartbeatSource{
			Adapter:            "codex",
			IdentityHash:       fmt.Sprintf("sha256:raw-c-%03d", i),
			LastSuccessfulSync: &now,
		})
	}
	for i := 0; i < 30; i++ {
		sources = append(sources, heartbeatSource{
			Adapter:            "claude-code",
			IdentityHash:       fmt.Sprintf("sha256:raw-l-%03d", i),
			LastSuccessfulSync: &now,
		})
	}
	s.sendHeartbeat(t, "m-f1-raw61", "codex", sources...)
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "ov-f1-t1-5-ev",
		MachineID:   "m-f1-raw61",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(120),
	}})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 120)

	var machines []overviewMachineInfo
	getJSONWithClient(t, client, s.BaseURL+"/api/v1/machines", &machines)
	if len(machines) != 1 {
		t.Fatalf("machines len = %d, want 1", len(machines))
	}
	if len(machines[0].Sources) != 61 {
		t.Fatalf("/api/v1/machines sources len = %d, want 61 ungrouped", len(machines[0].Sources))
	}
	for _, src := range machines[0].Sources {
		if src.State != "Fresh" || src.Age == "" || !strings.HasPrefix(src.SourceHash, "sha256:") {
			t.Fatalf("unexpected raw source record: %+v", src)
		}
	}

	html := overviewWaitHTMLContains(t, client, s.BaseURL, "61 sources across 2 adapters")
	health := overviewSectionBetween(t, html, `<section class="panel health-panel"`, `</section>`)
	if got := strings.Count(health, `class="source-health-line"`); got != 2 {
		t.Fatalf("expected 2 grouped lines on /, got %d", got)
	}
}

func TestOverview_F1_T1_MultiMachineIsolation(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "ov-f1-t1-6"})
	fresh := time.Now().UTC()

	s.sendHeartbeat(t, "host-alpha-one", "codex",
		heartbeatSource{Adapter: "codex", IdentityHash: "sha256:a1", LastSuccessfulSync: &fresh, LastErrorClass: "read_failed"},
		heartbeatSource{Adapter: "codex", IdentityHash: "sha256:a2", LastSuccessfulSync: &fresh},
	)
	s.sendHeartbeat(t, "host-beta-two", "claude-code")
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "ov-f1-t1-6-ev",
		MachineID:   "host-alpha-one",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(80),
	}})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 80)
	html := overviewWaitHTMLContains(t, client, s.BaseURL, "Heartbeat metadata only")

	health := overviewSectionBetween(t, html, `<section class="panel health-panel"`, `</section>`)
	boxASeg := overviewSectionBetween(t, health, "<td>host-alpha-one", "</tr>")
	if !strings.Contains(boxASeg, `class="source-health"`) || !strings.Contains(boxASeg, "read_failed") {
		t.Fatalf("expected host-alpha-one to render grouped .source-health with read_failed; got %s", boxASeg)
	}
	boxBSeg := overviewSectionBetween(t, health, "<td>host-beta-two", "</tr>")
	if !strings.Contains(boxBSeg, "Heartbeat metadata only") {
		t.Fatalf("expected host-beta-two to render Heartbeat metadata only; got %s", boxBSeg)
	}
	if strings.Contains(boxBSeg, `class="source-health-line"`) {
		t.Fatalf("host-beta-two with empty sources must not render source-health-line; got %s", boxBSeg)
	}
}

func TestOverview_F1_T2_ProblemDetailsCappedAt50WithOmissionNotice(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "ov-f1-t2-1"})
	stale := time.Now().UTC().Add(-2 * time.Hour)

	sources := make([]heartbeatSource, 0, 60)
	for i := 0; i < 60; i++ {
		errClass := ""
		if i == 59 {
			errClass = "upload_failed"
		}
		sources = append(sources, heartbeatSource{
			Adapter:            "codex",
			IdentityHash:       fmt.Sprintf("sha256:prob-%02d", i),
			LastSuccessfulSync: &stale,
			LastErrorClass:     errClass,
		})
	}
	s.sendHeartbeat(t, "m-f1-cap50", "codex", sources...)
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "ov-f1-t2-1-ev",
		MachineID:   "m-f1-cap50",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(30),
	}})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 30)
	html := overviewWaitHTMLContains(t, client, s.BaseURL, "+ 10 more stale or errored sources")

	health := overviewSectionBetween(t, html, `<section class="panel health-panel"`, `</section>`)
	if got := strings.Count(health, `class="source-detail-line"`); got != 51 {
		t.Fatalf("source-detail-line count = %d, want 51 (50 problems + 1 omission notice)", got)
	}
	firstDetailIdx := strings.Index(health, `class="source-detail-line"`)
	errIdx := strings.Index(health, "upload_failed")
	if firstDetailIdx < 0 || errIdx < 0 || errIdx > firstDetailIdx+350 {
		t.Fatalf("expected errored source upload_failed first in detail list")
	}
}

func TestOverview_F1_T2_HeartbeatScaleBeyond256Sources(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "ov-f1-t2-2"})
	now := time.Now().UTC()

	sources := make([]heartbeatSource, 0, 300)
	for i := 0; i < 300; i++ {
		sources = append(sources, heartbeatSource{
			Adapter:            "codex",
			IdentityHash:       fmt.Sprintf("sha256:scale-%03d", i),
			LastSuccessfulSync: &now,
		})
	}
	s.sendHeartbeat(t, "m-f1-300", "codex", sources...)
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "ov-f1-t2-2-ev",
		MachineID:   "m-f1-300",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(10),
	}})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 10)

	var machines []overviewMachineInfo
	getJSONWithClient(t, client, s.BaseURL+"/api/v1/machines", &machines)
	if len(machines) != 1 || len(machines[0].Sources) != 300 {
		t.Fatalf("expected 1 machine with 300 raw sources, got %+v", machines)
	}

	html := overviewWaitHTMLContains(t, client, s.BaseURL, "codex · 300 fresh")
	health := overviewSectionBetween(t, html, `<section class="panel health-panel"`, `</section>`)
	if got := strings.Count(health, `class="source-health-line"`); got != 1 {
		t.Fatalf("expected 1 grouped line for 300 same-adapter sources, got %d", got)
	}
}

func TestOverview_F1_T2_LegacyHeartbeatWithoutSourcesPreservesExisting(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "ov-f1-t2-3"})
	now := time.Now().UTC()

	s.sendHeartbeat(t, "m-f1-legacy", "codex",
		heartbeatSource{Adapter: "codex", IdentityHash: "sha256:leg-1", LastSuccessfulSync: &now},
		heartbeatSource{Adapter: "claude-code", IdentityHash: "sha256:leg-2", LastSuccessfulSync: &now},
	)
	legacyPayload := []byte(`{"machine_id":"m-f1-legacy","agent_version":"0.3.0","operating_system":"darwin","architecture":"arm64","adapters":[{"id":"codex","version":"1.0"}],"source_count":2,"source_error_count":0}`)
	code, respBody := overviewSendRawHeartbeat(t, s, legacyPayload)
	if code != http.StatusOK {
		t.Fatalf("legacy heartbeat status = %d: %s", code, string(respBody))
	}

	s.ingestEvents(t, []ingestEvent{{
		EventID:     "ov-f1-t2-3-ev",
		MachineID:   "m-f1-legacy",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(20),
	}})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 20)

	var machines []overviewMachineInfo
	getJSONWithClient(t, client, s.BaseURL+"/api/v1/machines", &machines)
	if len(machines) != 1 || len(machines[0].Sources) != 2 {
		t.Fatalf("expected legacy heartbeat to preserve 2 sources, got %+v", machines)
	}
	html := overviewWaitHTMLContains(t, client, s.BaseURL, `class="source-health-line"`)
	if !strings.Contains(html, "codex") || !strings.Contains(html, "claude-code") {
		t.Fatalf("expected preserved sources on / HTML")
	}
}

func TestOverview_F1_T2_EmptySourcesArrayClearsPreviousRows(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "ov-f1-t2-4"})
	now := time.Now().UTC()

	s.sendHeartbeat(t, "m-f1-clear", "codex",
		heartbeatSource{Adapter: "codex", IdentityHash: "sha256:clr-1", LastSuccessfulSync: &now},
		heartbeatSource{Adapter: "codex", IdentityHash: "sha256:clr-2", LastSuccessfulSync: &now},
	)
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "ov-f1-t2-4-ev1",
		MachineID:   "m-f1-clear",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(10),
	}})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 10)
	_ = overviewWaitHTMLContains(t, client, s.BaseURL, "codex · 2 fresh")

	s.sendHeartbeat(t, "m-f1-clear", "codex")
	var machines []overviewMachineInfo
	getJSONWithClient(t, client, s.BaseURL+"/api/v1/machines", &machines)
	if len(machines) != 1 || len(machines[0].Sources) != 0 {
		t.Fatalf("expected empty sources array to clear previous sources, got %+v", machines)
	}

	s.ingestEvents(t, []ingestEvent{{
		EventID:     "ov-f1-t2-4-ev2",
		MachineID:   "m-f1-clear",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(20),
	}})
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 30)
	html := overviewWaitHTMLContains(t, client, s.BaseURL, "Heartbeat metadata only")
	health := overviewSectionBetween(t, html, `<section class="panel health-panel"`, `</section>`)
	if strings.Contains(health, `class="source-health-line"`) {
		t.Fatalf("expected zero source-health-line after clearing sources; got %s", health)
	}
}

func TestOverview_F1_T2_EmptyDatabaseShowsExplicitEmptyRow(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "ov-f1-t2-5"})
	client := s.authClient(t)
	html := overviewGetHTML(t, client, s.BaseURL+"/")

	health := overviewSectionBetween(t, html, `<section class="panel health-panel"`, `</section>`)
	if !strings.Contains(health, "No machine heartbeats yet.") {
		t.Fatalf("expected 'No machine heartbeats yet.' on empty server")
	}
	if strings.Contains(health, `class="source-health"`) || strings.Contains(health, `class="source-health-line"`) {
		t.Fatalf("did not expect source-health blocks on empty DB")
	}
}

func TestOverview_F1_T2_PrivacyAndValidationNoPathLeak(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "ov-f1-t2-6"})
	now := time.Now().UTC()

	badPayload := []byte(`{"machine_id":"m-f1-bad","adapters":[{"id":"codex","version":"1.0"}],"source_count":1,"sources":[{"adapter":"codex","identity_hash":""}]}`)
	code, _ := overviewSendRawHeartbeat(t, s, badPayload)
	if code != http.StatusBadRequest {
		t.Fatalf("missing identity_hash status = %d, want 400", code)
	}

	hash := "sha256:" + strings.Repeat("e", 64)
	s.sendHeartbeat(t, "m-f1-priv", "codex", heartbeatSource{
		Adapter:            "codex",
		IdentityHash:       hash,
		LastSuccessfulSync: &now,
	})
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "ov-f1-t2-6-ev",
		MachineID:   "m-f1-priv",
		Project:     "clean-proj",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(15),
	}})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 15)
	html := overviewWaitHTMLContains(t, client, s.BaseURL, hash)
	_, _, rawMachines := overviewGetResp(t, client, s.BaseURL+"/api/v1/machines", nil)
	for _, body := range []string{html, string(rawMachines)} {
		for _, forbidden := range []string{"/Users/", "/home/", ".jsonl"} {
			if strings.Contains(body, forbidden) {
				t.Fatalf("unexpected local path artifact %q leaked in output", forbidden)
			}
		}
	}
}

// =============================================================================
// Area 2: Feature 3 — Localized <time datetime="..."> timestamps on / (LANDED)
// =============================================================================

func TestOverview_F3_T1_TimezoneHonoredOnLastSeenAndWhen(t *testing.T) {
	requireE2E(t)
	loc := overviewLoadLoc(t, "America/Toronto")
	s := startTestServer(t, serverOpts{IngestToken: "ov-f3-t1-1", Timezone: "America/Toronto"})

	// Anchor within 30d at 03:30 UTC (22:30 EST previous local day).
	nowUTC := time.Now().UTC()
	ts := time.Date(nowUTC.Year(), nowUTC.Month(), nowUTC.Day(), 3, 30, 0, 0, time.UTC).AddDate(0, 0, -1)
	local := ts.In(loc)
	wantDT := local.Format(time.RFC3339)
	wantDisp := local.Format("Jan 2, 15:04") + " ·"

	now := time.Now().UTC()
	s.sendHeartbeat(t, "m-f3-tor", "codex", heartbeatSource{
		Adapter:            "codex",
		IdentityHash:       "sha256:tor-" + strings.Repeat("a", 50),
		LastSuccessfulSync: &now,
	})
	s.ingestEvents(t, []ingestEvent{{
		EventID:      "ov-f3-t1-1-ev",
		Timestamp:    ts.Format(time.RFC3339Nano),
		MachineID:    "m-f3-tor",
		SessionID:    "sess-f3-tor",
		Project:      "proj-tor",
		Provider:     "openai",
		Model:        "gpt-5.4",
		Tool:         "codex",
		InputTokens:  i64(40),
		OutputTokens: i64(10),
		TotalTokens:  i64(50),
	}})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 50)
	html := overviewWaitHTMLContains(t, client, s.BaseURL, wantDT)

	health := overviewSectionBetween(t, html, `<section class="panel health-panel"`, `</section>`)
	lastSeenCell := overviewSectionBetween(t, health, `data-label="Last seen"`, `</td>`)
	wantZone := time.Now().In(loc).Format("MST")
	if !strings.Contains(lastSeenCell, "<time datetime=") || !strings.Contains(lastSeenCell, wantZone) {
		t.Fatalf("expected localized <time> with %s in Last seen cell; got %s", wantZone, lastSeenCell)
	}

	sessions := overviewSectionBetween(t, html, `<section class="panel sessions-panel"`, `</section>`)
	whenCell := overviewSectionBetween(t, sessions, `data-label="When"`, `</td>`)
	if !strings.Contains(whenCell, fmt.Sprintf(`datetime="%s"`, wantDT)) || !strings.Contains(whenCell, wantDisp) {
		t.Fatalf("expected datetime=%q and %q in When cell; got %s", wantDT, wantDisp, whenCell)
	}
}

func TestOverview_F3_T1_DifferentTimezoneShiftsFormattedHour(t *testing.T) {
	requireE2E(t)
	loc := overviewLoadLoc(t, "Asia/Tokyo")
	s := startTestServer(t, serverOpts{IngestToken: "ov-f3-t1-2", Timezone: "Asia/Tokyo"})

	nowUTC := time.Now().UTC()
	ts := time.Date(nowUTC.Year(), nowUTC.Month(), nowUTC.Day(), 20, 15, 0, 0, time.UTC).AddDate(0, 0, -2)
	local := ts.In(loc)
	wantDT := local.Format(time.RFC3339)
	wantDisp := local.Format("Jan 2, 15:04") + " ·"

	s.ingestEvents(t, []ingestEvent{{
		EventID:     "ov-f3-t1-2-ev",
		Timestamp:   ts.Format(time.RFC3339Nano),
		MachineID:   "m-f3-tyo",
		SessionID:   "sess-f3-tyo",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(60),
	}})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 60)
	html := overviewWaitHTMLContains(t, client, s.BaseURL, wantDT)

	sessions := overviewSectionBetween(t, html, `<section class="panel sessions-panel"`, `</section>`)
	if !strings.Contains(sessions, fmt.Sprintf(`datetime="%s"`, wantDT)) || !strings.Contains(sessions, wantDisp) {
		t.Fatalf("expected JST datetime %q and display %q; got %s", wantDT, wantDisp, sessions)
	}
	if !strings.Contains(sessions, "+09:00") {
		t.Fatalf("expected +09:00 offset in Tokyo session timestamp")
	}
}

func TestOverview_F3_T1_RelativeAgeBuckets(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "ov-f3-t1-3", Timezone: "UTC"})
	base := time.Now().UTC()

	cases := []struct {
		id  string
		ts  time.Time
		age string
	}{
		{"sess-age-now", base.Add(-15 * time.Second), "just now"},
		{"sess-age-15m", base.Add(-15*time.Minute - 10*time.Second), "15m ago"},
		{"sess-age-4h", base.Add(-4*time.Hour - 2*time.Minute), "4h ago"},
		{"sess-age-3d", base.Add(-3*24*time.Hour - 2*time.Hour), "3d ago"},
	}
	events := make([]ingestEvent, 0, len(cases))
	for i, tc := range cases {
		events = append(events, ingestEvent{
			EventID:     fmt.Sprintf("ov-f3-age-%d", i),
			Timestamp:   tc.ts.Format(time.RFC3339Nano),
			MachineID:   "m-f3-age",
			SessionID:   tc.id,
			Provider:    "openai",
			Model:       "gpt-5.4",
			Tool:        "codex",
			TotalTokens: i64(25),
		})
	}
	s.ingestEvents(t, events)

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 100)
	html := overviewWaitHTMLContains(t, client, s.BaseURL, "sess-age-3d")
	sessions := overviewSectionBetween(t, html, `<section class="panel sessions-panel"`, `</section>`)

	for _, tc := range cases {
		if !strings.Contains(sessions, tc.age) {
			t.Fatalf("expected relative age %q for %s in sessions table", tc.age, tc.id)
		}
	}
}

func TestOverview_F3_T1_TitleAttributeIncludesFullDateAndZone(t *testing.T) {
	requireE2E(t)
	loc := overviewLoadLoc(t, "America/Toronto")
	s := startTestServer(t, serverOpts{IngestToken: "ov-f3-t1-4", Timezone: "America/Toronto"})

	ts := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	wantTitle := ts.In(loc).Format("2006-01-02 15:04:05 MST")

	s.ingestEvents(t, []ingestEvent{{
		EventID:     "ov-f3-t1-4-ev",
		Timestamp:   ts.Format(time.RFC3339Nano),
		MachineID:   "m-f3-title",
		SessionID:   "sess-f3-title",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(35),
	}})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 35)
	html := overviewWaitHTMLContains(t, client, s.BaseURL, wantTitle)
	sessions := overviewSectionBetween(t, html, `<section class="panel sessions-panel"`, `</section>`)
	if !strings.Contains(sessions, fmt.Sprintf(`title="%s"`, wantTitle)) {
		t.Fatalf("expected full title=%q in sessions table; got %s", wantTitle, sessions)
	}
}

func TestOverview_F3_T1_ConsistencyAcrossOverviewAndSessions(t *testing.T) {
	requireE2E(t)
	loc := overviewLoadLoc(t, "Europe/London")
	s := startTestServer(t, serverOpts{IngestToken: "ov-f3-t1-5", Timezone: "Europe/London"})

	ts := time.Now().UTC().Add(-90 * time.Minute).Truncate(time.Second)
	wantDT := ts.In(loc).Format(time.RFC3339)

	s.ingestEvents(t, []ingestEvent{{
		EventID:     "ov-f3-t1-5-ev",
		Timestamp:   ts.Format(time.RFC3339Nano),
		MachineID:   "m-f3-lon",
		SessionID:   "sess-f3-lon",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(45),
	}})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 45)
	ovHTML := overviewWaitHTMLContains(t, client, s.BaseURL, wantDT)
	sessHTML := overviewGetHTML(t, client, s.BaseURL+"/sessions")

	needle := fmt.Sprintf(`datetime="%s"`, wantDT)
	if !strings.Contains(ovHTML, needle) {
		t.Fatalf("overview missing %s", needle)
	}
	if !strings.Contains(sessHTML, needle) {
		t.Fatalf("/sessions missing %s", needle)
	}
}

func TestOverview_F3_T2_FutureTimestampClampedToJustNow(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "ov-f3-t2-1", Timezone: "UTC"})
	future := time.Now().UTC().Add(10 * time.Minute).Truncate(time.Second)

	s.ingestEvents(t, []ingestEvent{{
		EventID:     "ov-f3-t2-1-ev",
		Timestamp:   future.Format(time.RFC3339Nano),
		MachineID:   "m-f3-fut",
		SessionID:   "sess-f3-fut",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(20),
	}})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 20)
	html := overviewWaitHTMLContains(t, client, s.BaseURL, "sess-f3-fut")
	sessions := overviewSectionBetween(t, html, `<section class="panel sessions-panel"`, `</section>`)
	if !strings.Contains(sessions, "just now</time>") {
		t.Fatalf("expected future timestamp age clamped to 'just now'; got %s", sessions)
	}
	if strings.Contains(sessions, "m ago") && strings.Contains(sessions, "-") {
		// Check specifically no negative relative age like -10m ago.
		if regexp.MustCompile(`-\d+[mhd] ago`).MatchString(sessions) {
			t.Fatalf("found negative relative age in %s", sessions)
		}
	}
}

func TestOverview_F3_T2_DSTTransitionBoundary(t *testing.T) {
	requireE2E(t)
	// Verify America/New_York renders the current local offset cleanly and never hardcodes a single offset.
	// Using a relative recent timestamp AND verifying /sessions?period=all with 2026-11-01 DST fall-back!
	s := startTestServer(t, serverOpts{IngestToken: "ov-f3-t2-2", Timezone: "America/New_York"})
	nowTS := time.Now().UTC().Add(-30 * time.Minute).Truncate(time.Second)
	preDST := "2026-11-01T05:30:00Z"  // 01:30 EDT (-04:00)
	postDST := "2026-11-01T06:30:00Z" // 01:30 EST (-05:00)

	s.ingestEvents(t, []ingestEvent{
		{
			EventID:     "ov-f3-dst-now",
			Timestamp:   nowTS.Format(time.RFC3339Nano),
			MachineID:   "m-f3-dst",
			SessionID:   "sess-f3-dst-now",
			Provider:    "openai",
			Model:       "gpt-5.4",
			Tool:        "codex",
			TotalTokens: i64(10),
		},
		{
			EventID:     "ov-f3-dst-pre",
			Timestamp:   preDST,
			MachineID:   "m-f3-dst",
			SessionID:   "sess-f3-dst-pre",
			Provider:    "openai",
			Model:       "gpt-5.4",
			Tool:        "codex",
			TotalTokens: i64(10),
		},
		{
			EventID:     "ov-f3-dst-post",
			Timestamp:   postDST,
			MachineID:   "m-f3-dst",
			SessionID:   "sess-f3-dst-post",
			Provider:    "openai",
			Model:       "gpt-5.4",
			Tool:        "codex",
			TotalTokens: i64(10),
		},
	})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 30)
	sessHTML := overviewGetHTML(t, client, s.BaseURL+"/sessions?period=all")
	if !strings.Contains(sessHTML, `datetime="2026-11-01T01:30:00-04:00"`) {
		t.Fatalf("expected pre-fallback EDT -04:00 offset in /sessions?period=all")
	}
	if !strings.Contains(sessHTML, `datetime="2026-11-01T01:30:00-05:00"`) {
		t.Fatalf("expected post-fallback EST -05:00 offset in /sessions?period=all")
	}
}

func TestOverview_F3_T2_NanosecondPrecisionTimestampParsed(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "ov-f3-t2-3", Timezone: "UTC"})
	base := time.Now().UTC().Add(-10 * time.Minute)
	nanoTS := time.Date(base.Year(), base.Month(), base.Day(), base.Hour(), base.Minute(), 12, 789123456, time.UTC)

	s.ingestEvents(t, []ingestEvent{{
		EventID:     "ov-f3-t2-3-ev",
		Timestamp:   nanoTS.Format(time.RFC3339Nano),
		MachineID:   "m-f3-nano",
		SessionID:   "sess-f3-nano",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(18),
	}})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 18)
	wantDT := nanoTS.Format(time.RFC3339)
	html := overviewWaitHTMLContains(t, client, s.BaseURL, wantDT)
	if strings.Contains(html, ".789123456Z") {
		t.Fatalf("nanosecond raw string leaked unformatted into / HTML")
	}
}

func TestOverview_F3_T2_FreshnessPillStatesEmptyFreshStale(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "ov-f3-t2-4", Timezone: "UTC"})
	client := s.authClient(t)

	emptyHTML := overviewGetHTML(t, client, s.BaseURL+"/")
	if !strings.Contains(emptyHTML, "Unknown · no events yet") {
		t.Fatalf("expected 'Unknown · no events yet' freshness pill on empty DB")
	}

	staleTS := time.Now().UTC().Add(-2 * time.Hour)
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "ov-f3-stale",
		Timestamp:   staleTS.Format(time.RFC3339Nano),
		MachineID:   "m-f3-fresh",
		SessionID:   "sess-f3-stale",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(10),
	}})
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 10)
	staleHTML := overviewWaitHTMLContains(t, client, s.BaseURL, "Stale · ")
	if !strings.Contains(staleHTML, `status-pill stale`) {
		t.Fatalf("expected stale freshness pill")
	}

	freshTS := time.Now().UTC().Add(-5 * time.Second)
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "ov-f3-fresh",
		Timestamp:   freshTS.Format(time.RFC3339Nano),
		MachineID:   "m-f3-fresh",
		SessionID:   "sess-f3-fresh",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(15),
	}})
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 25)
	freshHTML := overviewWaitHTMLContains(t, client, s.BaseURL, "Fresh · ")
	if !strings.Contains(freshHTML, `status-pill connected`) {
		t.Fatalf("expected connected Fresh freshness pill")
	}
}

func TestOverview_F3_T2_NoRawUTCOffsetDriftOnMidnightRollover(t *testing.T) {
	requireE2E(t)
	loc := overviewLoadLoc(t, "America/Los_Angeles")
	s := startTestServer(t, serverOpts{IngestToken: "ov-f3-t2-5", Timezone: "America/Los_Angeles"})

	nowUTC := time.Now().UTC()
	// 06:30 UTC is 22:30 PST or 23:30 PDT (always previous calendar day in Los Angeles year-round).
	ts := time.Date(nowUTC.Year(), nowUTC.Month(), nowUTC.Day(), 6, 30, 0, 0, time.UTC).AddDate(0, 0, -1)
	local := ts.In(loc)
	wantMonthDay := local.Format("Jan 2")
	utcMonthDay := ts.UTC().Format("Jan 2")
	if wantMonthDay == utcMonthDay {
		t.Fatalf("test setup error: expected local date %q != UTC date %q", wantMonthDay, utcMonthDay)
	}

	s.ingestEvents(t, []ingestEvent{{
		EventID:     "ov-f3-t2-5-ev",
		Timestamp:   ts.Format(time.RFC3339Nano),
		MachineID:   "m-f3-la",
		SessionID:   "sess-f3-la",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(55),
	}})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 55)
	html := overviewWaitHTMLContains(t, client, s.BaseURL, local.Format(time.RFC3339))
	sessions := overviewSectionBetween(t, html, `<section class="panel sessions-panel"`, `</section>`)
	wantLocal := local.Format("Jan 2, 15:04")
	if !strings.Contains(sessions, wantLocal) {
		t.Fatalf("expected local previous-day %q; got %s", wantLocal, sessions)
	}
}

// =============================================================================
// Area 3: Feature 4 — Pre-rendered TokenComposition & pollLifetime pause (LANDED)
// =============================================================================

func TestOverview_F4_T1_PreRenderedCompositionSegmentsAndShares(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "ov-f4-t1-1"})
	s.ingestEvents(t, []ingestEvent{{
		EventID:          "ov-f4-t1-1-ev",
		MachineID:        "m-f4-1",
		SessionID:        "sess-f4-1",
		Provider:         "openai",
		Model:            "gpt-5.4",
		Tool:             "codex",
		InputTokens:      i64(400),
		CacheReadTokens:  i64(500),
		CacheWriteTokens: i64(100),
		OutputTokens:     i64(200),
		TotalTokens:      i64(1200),
	}})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 1200)
	html := overviewWaitHTMLContains(t, client, s.BaseURL, `id="live-input-tokens" data-display="1,000"`)

	comp := overviewSectionBetween(t, html, `id="token-composition"`, `class="progress-block"`)
	for _, want := range []string{
		`id="live-input-tokens" data-display="1,000"`,
		`<span class="odometer-static">1,000</span>`,
		`id="live-output-tokens" data-display="200"`,
		`<span class="odometer-static">200</span>`,
		`id="live-input-share">83.3%</span>`,
		`id="live-output-share">16.7%</span>`,
	} {
		if !strings.Contains(comp, want) {
			t.Fatalf("composition HTML missing %q in:\n%s", want, comp)
		}
	}
	for _, segID := range []string{"live-uncached-segment", "live-cached-segment", "live-output-segment"} {
		segRe := regexp.MustCompile(fmt.Sprintf(`<span[^>]*id="%s"[^>]*>`, segID))
		tag := segRe.FindString(comp)
		if tag == "" {
			t.Fatalf("segment %s not found", segID)
		}
		if strings.Contains(tag, " hidden") {
			t.Fatalf("segment %s should not be hidden: %s", segID, tag)
		}
	}
}

func TestOverview_F4_T1_ZeroCachedHidesCachedSegment(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "ov-f4-t1-2"})
	s.ingestEvents(t, []ingestEvent{{
		EventID:          "ov-f4-t1-2-ev",
		MachineID:        "m-f4-2",
		Provider:         "openai",
		Model:            "gpt-5.4",
		Tool:             "codex",
		InputTokens:      i64(750),
		CacheReadTokens:  i64(0),
		CacheWriteTokens: i64(0),
		OutputTokens:     i64(250),
		TotalTokens:      i64(1000),
	}})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 1000)
	html := overviewWaitHTMLContains(t, client, s.BaseURL, `id="live-input-tokens" data-display="750"`)

	comp := overviewSectionBetween(t, html, `id="token-composition"`, `class="progress-block"`)
	cachedRe := regexp.MustCompile(`<span[^>]*id="live-cached-segment"[^>]*>`)
	cachedTag := cachedRe.FindString(comp)
	if !strings.Contains(cachedTag, " hidden") || !strings.Contains(cachedTag, "width: 0%") {
		t.Fatalf("expected live-cached-segment hidden with width: 0%%; got %s", cachedTag)
	}
	if !strings.Contains(comp, `id="live-input-share">75%</span>`) || !strings.Contains(comp, `id="live-output-share">25%</span>`) {
		t.Fatalf("expected 75%% and 25%% shares; got %s", comp)
	}
}

func TestOverview_F4_T1_OverviewJSONMatchesPreRenderedComposition(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "ov-f4-t1-3"})
	s.ingestEvents(t, []ingestEvent{
		{
			EventID:         "ov-f4-t1-3-a",
			MachineID:       "m-f4-3",
			Provider:        "openai",
			Model:           "gpt-5.4",
			Tool:            "codex",
			InputTokens:     i64(1000),
			CacheReadTokens: i64(2000),
			OutputTokens:    i64(500),
			TotalTokens:     i64(3500),
		},
		{
			EventID:          "ov-f4-t1-3-b",
			MachineID:        "m-f4-3",
			Provider:         "anthropic",
			Model:            "claude-sonnet-4",
			Tool:             "claude-code",
			InputTokens:      i64(300),
			CacheWriteTokens: i64(200),
			OutputTokens:     i64(100),
			TotalTokens:      i64(600),
		},
	})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 4100)

	var ov overviewPayload
	getJSONWithClient(t, client, s.BaseURL+"/api/v1/analytics/overview", &ov)
	if ov.Composition.InputTokens != 3500 || ov.Composition.UncachedInputTokens != 1300 || ov.Composition.CachedInputTokens != 2200 || ov.Composition.OutputTokens != 600 {
		t.Fatalf("unexpected overview composition: %+v", ov.Composition)
	}

	var evo struct {
		LifetimeTokens int64               `json:"lifetime_tokens"`
		Composition    overviewComposition `json:"composition"`
	}
	deadline := time.Now().Add(4 * time.Second)
	for {
		getJSONWithClient(t, client, s.BaseURL+"/api/v1/evolution", &evo)
		if evo.LifetimeTokens == 4100 && evo.Composition == ov.Composition {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("evolution composition %+v (tokens=%d) != overview composition %+v after SWR wait", evo.Composition, evo.LifetimeTokens, ov.Composition)
		}
		time.Sleep(50 * time.Millisecond)
	}

	html := overviewWaitHTMLContains(t, client, s.BaseURL, `id="live-input-tokens" data-display="3,500"`)
	if !strings.Contains(html, `id="live-output-tokens" data-display="600"`) {
		t.Fatalf("HTML live-output-tokens did not match JSON 600")
	}
}

func TestOverview_F4_T1_AutoPausedCheckedInPollLifetimeAndControlsPresent(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "ov-f4-t1-4"})
	client := s.authClient(t)
	html := overviewGetHTML(t, client, s.BaseURL+"/")

	for _, want := range []string{
		`id="refresh-now"`,
		`id="auto-refresh-toggle"`,
		`aria-pressed="false"`,
		`Pause auto-refresh`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("/ HTML missing control %q", want)
		}
	}

	autoPausedDecl := strings.Index(html, "const autoPaused =")
	pollLifetimeDecl := strings.Index(html, "const pollLifetime =")
	if autoPausedDecl < 0 || pollLifetimeDecl < 0 || autoPausedDecl > pollLifetimeDecl {
		t.Fatalf("expected autoPaused declared before pollLifetime (autoPaused=%d, pollLifetime=%d)", autoPausedDecl, pollLifetimeDecl)
	}
	pollSection := overviewSectionBetween(t, html, "const pollLifetime =", "const autoToggle =")
	if !strings.Contains(pollSection, "autoPaused()") {
		t.Fatalf("expected pollLifetime to check autoPaused() before fetching")
	}
	if !strings.Contains(html, "if (!paused) pollLifetime();") {
		t.Fatalf("expected toggle resume path to trigger pollLifetime()")
	}
}

func TestOverview_F4_T1_PreRenderedAfterRestart(t *testing.T) {
	requireE2E(t)
	s1 := startTestServer(t, serverOpts{IngestToken: "ov-f4-t1-5"})
	s1.ingestEvents(t, []ingestEvent{{
		EventID:         "ov-f4-t1-5-ev",
		MachineID:       "m-f4-rst",
		Provider:        "openai",
		Model:           "gpt-5.4",
		Tool:            "codex",
		InputTokens:     i64(900),
		CacheReadTokens: i64(100),
		OutputTokens:    i64(250),
		TotalTokens:     i64(1250),
	}})
	c1 := s1.authClient(t)
	waitOverviewLifetimeTokens(t, c1, s1.BaseURL, 1250)
	dbPath := s1.DBPath
	s1.Stop()

	s2 := startTestServer(t, serverOpts{IngestToken: "ov-f4-t1-5", DBPath: dbPath})
	c2 := s2.authClient(t)
	html := overviewGetHTML(t, c2, s2.BaseURL+"/")
	if !strings.Contains(html, `id="live-input-tokens" data-display="1,000"`) || !strings.Contains(html, `id="live-output-tokens" data-display="250"`) {
		t.Fatalf("first GET / after restart did not pre-render composition")
	}
}

func TestOverview_F4_T2_EmptyDBPreRendersZeroAndDashesWithoutLoadingText(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "ov-f4-t2-1"})
	client := s.authClient(t)
	html := overviewGetHTML(t, client, s.BaseURL+"/")

	if strings.Contains(html, "Usage totals are loading") {
		t.Fatalf("unexpected loading placeholder text in pre-rendered / HTML")
	}
	comp := overviewSectionBetween(t, html, `id="token-composition"`, `class="progress-block"`)
	for _, segID := range []string{"live-uncached-segment", "live-cached-segment", "live-output-segment"} {
		segRe := regexp.MustCompile(fmt.Sprintf(`<span[^>]*id="%s"[^>]*>`, segID))
		tag := segRe.FindString(comp)
		if !strings.Contains(tag, " hidden") || !strings.Contains(tag, "width: 0%") {
			t.Fatalf("empty DB segment %s should be hidden with width: 0%%; got %s", segID, tag)
		}
	}
	if !strings.Contains(comp, `id="live-input-share">—</span>`) || !strings.Contains(comp, `id="live-output-share">—</span>`) {
		t.Fatalf("empty DB shares should be em-dash —; got %s", comp)
	}
	if !strings.Contains(comp, `id="live-input-tokens" data-display="0"`) || !strings.Contains(comp, `id="live-output-tokens" data-display="0"`) {
		t.Fatalf("empty DB counts should be 0; got %s", comp)
	}
}

func TestOverview_F4_T2_UnclassifiedTokensOmittedFromOverviewHero(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "ov-f4-t2-2"})
	s.ingestEvents(t, []ingestEvent{
		{
			EventID:     "ov-f4-unclass-1",
			MachineID:   "m-f4-u",
			Provider:    "openai",
			Model:       "gpt-5.4",
			Tool:        "codex",
			TotalTokens: i64(500),
		},
		{
			EventID:      "ov-f4-class-2",
			MachineID:    "m-f4-u",
			Provider:     "openai",
			Model:        "gpt-5.4",
			Tool:         "codex",
			InputTokens:  i64(100),
			OutputTokens: i64(50),
			TotalTokens:  i64(150),
		},
	})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 650)

	var ov overviewPayload
	getJSONWithClient(t, client, s.BaseURL+"/api/v1/analytics/overview", &ov)
	if ov.Composition.UnclassifiedTokens != 500 {
		t.Fatalf("overview composition.unclassified_tokens = %d, want 500", ov.Composition.UnclassifiedTokens)
	}

	html := overviewWaitHTMLContains(t, client, s.BaseURL, `id="live-input-tokens" data-display="100"`)
	comp := overviewSectionBetween(t, html, `id="token-composition"`, `class="progress-block"`)
	if !strings.Contains(comp, `id="live-input-share">66.7%</span>`) || !strings.Contains(comp, `id="live-output-share">33.3%</span>`) {
		t.Fatalf("expected shares computed from classified tokens only (66.7%%/33.3%%); got %s", comp)
	}
	if strings.Contains(comp, "live-unclassified") || strings.Contains(comp, "Unknown</span>") {
		t.Fatalf("unclassified tokens must not appear in overview hero #token-composition")
	}
}

func TestOverview_F4_T2_ZeroOutputHidesOutputSegmentOnly(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "ov-f4-t2-3"})
	s.ingestEvents(t, []ingestEvent{{
		EventID:      "ov-f4-t2-3-ev",
		MachineID:    "m-f4-zout",
		Provider:     "openai",
		Model:        "gpt-5.4",
		Tool:         "codex",
		InputTokens:  i64(1000),
		OutputTokens: i64(0),
		TotalTokens:  i64(1000),
	}})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 1000)
	html := overviewWaitHTMLContains(t, client, s.BaseURL, `id="live-input-share">100%</span>`)

	comp := overviewSectionBetween(t, html, `id="token-composition"`, `class="progress-block"`)
	outSeg := regexp.MustCompile(`<span[^>]*id="live-output-segment"[^>]*>`).FindString(comp)
	inSeg := regexp.MustCompile(`<span[^>]*id="live-uncached-segment"[^>]*>`).FindString(comp)
	if !strings.Contains(outSeg, " hidden") {
		t.Fatalf("expected live-output-segment hidden when output=0; got %s", outSeg)
	}
	if strings.Contains(inSeg, " hidden") {
		t.Fatalf("did not expect live-uncached-segment hidden; got %s", inSeg)
	}
	if !strings.Contains(comp, `id="live-output-share">0%</span>`) {
		t.Fatalf("expected 0%% output share when input>0 and output=0; got %s", comp)
	}
}

func TestOverview_F4_T2_NoTemplateSanitizationMarkers(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "ov-f4-t2-4"})
	s.ingestEvents(t, []ingestEvent{{
		EventID:         "ov-f4-t2-4-ev",
		MachineID:       "m-f4-tmpl",
		Provider:        "openai",
		Model:           "gpt-5.4",
		Tool:            "codex",
		InputTokens:     i64(250),
		CacheReadTokens: i64(250),
		OutputTokens:    i64(500),
		TotalTokens:     i64(1000),
	}})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 1000)
	html := overviewWaitHTMLContains(t, client, s.BaseURL, `id="live-input-tokens" data-display="500"`)
	if strings.Contains(html, "ZgotmplZ") {
		t.Fatalf("found Go html/template ZgotmplZ sanitization marker in / HTML")
	}
}

func TestOverview_F4_T2_ZeroInputAndOutputWithNonZeroTotal(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "ov-f4-t2-5"})
	s.ingestEvents(t, []ingestEvent{{
		EventID:      "ov-f4-t2-5-ev",
		MachineID:    "m-f4-zero-io",
		Provider:     "openai",
		Model:        "gpt-5.4",
		Tool:         "codex",
		InputTokens:  i64(0),
		OutputTokens: i64(0),
		TotalTokens:  i64(900),
	}})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 900)
	html := overviewWaitHTMLContains(t, client, s.BaseURL, `data-display="900"`)
	comp := overviewSectionBetween(t, html, `id="token-composition"`, `class="progress-block"`)
	if !strings.Contains(comp, `id="live-input-share">—</span>`) || !strings.Contains(comp, `id="live-output-share">—</span>`) {
		t.Fatalf("expected — shares when input+output == 0 even with total=900; got %s", comp)
	}
	if strings.Contains(comp, "NaN") {
		t.Fatalf("found NaN in composition HTML")
	}
}

// =============================================================================
// Area 4: Feature 9 — Overview.ByMachine & / Recent Sessions LIMIT 8 (LANDED)
// =============================================================================

func TestOverview_F9_T1_RecentSessionsCappedToEightMostRecentOnOverview(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "ov-f9-t1-1"})
	base := time.Now().UTC().Add(-20 * time.Hour)

	events := make([]ingestEvent, 0, 15)
	for i := 0; i < 15; i++ {
		events = append(events, ingestEvent{
			EventID:     fmt.Sprintf("ov-f9-s-%02d", i),
			Timestamp:   base.Add(time.Duration(i) * time.Hour).Format(time.RFC3339Nano),
			MachineID:   "m-f9-cap",
			SessionID:   fmt.Sprintf("sess-cap-%02d", i),
			Project:     "proj-cap",
			Provider:    "openai",
			Model:       "gpt-5.4",
			Tool:        "codex",
			TotalTokens: i64(100),
		})
	}
	s.ingestEvents(t, events)

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 1500)
	html := overviewWaitHTMLContains(t, client, s.BaseURL, "sess-cap-14")
	sessions := overviewSectionBetween(t, html, `<section class="panel sessions-panel"`, `</section>`)

	for i := 7; i < 15; i++ {
		want := fmt.Sprintf("sess-cap-%02d", i)
		if !strings.Contains(sessions, want) {
			t.Fatalf("expected newest 8 session %q on /, missing from:\n%s", want, sessions)
		}
	}
	for i := 0; i < 7; i++ {
		omit := fmt.Sprintf("sess-cap-%02d", i)
		if strings.Contains(sessions, omit) {
			t.Fatalf("expected older session %q omitted by LIMIT 8 on /", omit)
		}
	}
}

func TestOverview_F9_T1_SessionsPageRetainsAllBeyondEight(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "ov-f9-t1-2"})
	base := time.Now().UTC().Add(-20 * time.Hour)

	events := make([]ingestEvent, 0, 15)
	for i := 0; i < 15; i++ {
		events = append(events, ingestEvent{
			EventID:     fmt.Sprintf("ov-f9-all-%02d", i),
			Timestamp:   base.Add(time.Duration(i) * time.Hour).Format(time.RFC3339Nano),
			MachineID:   "m-f9-all",
			SessionID:   fmt.Sprintf("sess-all-%02d", i),
			Provider:    "openai",
			Model:       "gpt-5.4",
			Tool:        "codex",
			TotalTokens: i64(10),
		})
	}
	s.ingestEvents(t, events)

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 150)

	sessHTML := overviewGetHTML(t, client, s.BaseURL+"/sessions")
	for i := 0; i < 15; i++ {
		want := fmt.Sprintf("sess-all-%02d", i)
		if !strings.Contains(sessHTML, want) {
			t.Fatalf("/sessions missing %q (expected >8 sessions retained)", want)
		}
	}
	var apiSessions []sessionRow
	getJSONWithClient(t, client, s.BaseURL+"/api/v1/sessions", &apiSessions)
	if len(apiSessions) != 15 {
		t.Fatalf("/api/v1/sessions len = %d, want 15", len(apiSessions))
	}
}

func TestOverview_F9_T1_ByMachineSortedDescendingAndDrivesTopMachine(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "ov-f9-t1-3"})
	s.ingestEvents(t, []ingestEvent{
		{EventID: "ov-f9-m1", MachineID: "m-low-box", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(100)},
		{EventID: "ov-f9-m2", MachineID: "m-top-box", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(9000)},
		{EventID: "ov-f9-m3", MachineID: "m-mid-box", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(2500)},
	})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 11600)

	var ov overviewPayload
	getJSONWithClient(t, client, s.BaseURL+"/api/v1/analytics/overview", &ov)
	if len(ov.ByMachine) != 3 {
		t.Fatalf("by_machine len = %d, want 3", len(ov.ByMachine))
	}
	wantOrder := []overviewMachineTotal{
		{Machine: "m-top-box", Tokens: 9000},
		{Machine: "m-mid-box", Tokens: 2500},
		{Machine: "m-low-box", Tokens: 100},
	}
	for i, w := range wantOrder {
		if ov.ByMachine[i] != w {
			t.Fatalf("by_machine[%d] = %+v, want %+v", i, ov.ByMachine[i], w)
		}
	}

	html := overviewWaitHTMLContains(t, client, s.BaseURL, `title="m-top-box"`)
	statGrid := overviewSectionBetween(t, html, `<section class="stat-grid"`, `</section>`)
	if !strings.Contains(statGrid, `title="m-top-box"`) {
		t.Fatalf("Top machine stat card did not show m-top-box: %s", statGrid)
	}
	machinesTable := overviewSectionBetween(t, html, `<table aria-label="Machine token usage">`, `</table>`)
	topIdx := strings.Index(machinesTable, "m-top-box")
	midIdx := strings.Index(machinesTable, "m-mid-box")
	lowIdx := strings.Index(machinesTable, "m-low-box")
	if !(topIdx >= 0 && midIdx > topIdx && lowIdx > midIdx) {
		t.Fatalf("Machines table not sorted descending: top=%d mid=%d low=%d", topIdx, midIdx, lowIdx)
	}
}

func TestOverview_F9_T1_ByMachineMatchesMachinesLifetimeTotals(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "ov-f9-t1-4"})
	now := time.Now().UTC()
	s.sendHeartbeat(t, "m-sync-a", "codex")
	s.sendHeartbeat(t, "m-sync-b", "claude-code")
	s.ingestEvents(t, []ingestEvent{
		{EventID: "ov-f9-s1", Timestamp: now.Add(-48 * time.Hour).Format(time.RFC3339Nano), MachineID: "m-sync-a", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(1200)},
		{EventID: "ov-f9-s2", Timestamp: now.Add(-1 * time.Hour).Format(time.RFC3339Nano), MachineID: "m-sync-a", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(800)},
		{EventID: "ov-f9-s3", Timestamp: now.Add(-2 * time.Hour).Format(time.RFC3339Nano), MachineID: "m-sync-b", Provider: "anthropic", Model: "claude-sonnet-4", Tool: "claude-code", TotalTokens: i64(450)},
	})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 2450)

	var ov overviewPayload
	getJSONWithClient(t, client, s.BaseURL+"/api/v1/analytics/overview", &ov)
	machineMap := make(map[string]int64)
	for _, m := range ov.Machines {
		machineMap[m.ID] = m.LifetimeTokens
	}
	if len(ov.ByMachine) != 2 {
		t.Fatalf("by_machine len = %d, want 2", len(ov.ByMachine))
	}
	for _, bm := range ov.ByMachine {
		got, ok := machineMap[bm.Machine]
		if !ok || got != bm.Tokens {
			t.Fatalf("by_machine[%s]=%d does not match machines lifetime_tokens=%d (ok=%v)", bm.Machine, bm.Tokens, got, ok)
		}
	}
}

func TestOverview_F9_T1_OverviewJSONSchemaCompleteness(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "ov-f9-t1-5"})
	now := time.Now().UTC()
	s.sendHeartbeat(t, "m-schema", "codex")
	s.ingestEvents(t, []ingestEvent{{
		EventID:      "ov-f9-t1-5-ev",
		Timestamp:    now.Format(time.RFC3339Nano),
		MachineID:    "m-schema",
		SessionID:    "sess-schema",
		Project:      "proj-schema",
		Provider:     "openai",
		Model:        "gpt-5.4",
		Tool:         "codex",
		InputTokens:  i64(100),
		OutputTokens: i64(50),
		TotalTokens:  i64(150),
	}})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 150)

	_, _, raw := overviewGetResp(t, client, s.BaseURL+"/api/v1/analytics/overview", nil)
	var doc map[string]any
	if err := json.Unmarshal(raw, &doc); err != nil {
		t.Fatalf("unmarshal overview JSON: %v", err)
	}
	for _, key := range []string{
		"lifetime_tokens", "timezone", "today", "week", "evolution",
		"composition", "activity", "by_model", "by_tool", "by_machine",
		"by_project", "estimated_cost", "cache", "threads", "machines",
	} {
		val, ok := doc[key]
		if !ok || val == nil {
			t.Fatalf("overview JSON missing or null key %q", key)
		}
	}
}

func TestOverview_F9_T2_ByMachineTieBreakDeterministicAlphabetical(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "ov-f9-t2-1"})
	s.ingestEvents(t, []ingestEvent{
		{EventID: "ov-f9-tie-z", MachineID: "m-zulu", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(1000)},
		{EventID: "ov-f9-tie-a", MachineID: "m-alpha", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(1000)},
	})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 2000)

	for attempt := 0; attempt < 3; attempt++ {
		var ov overviewPayload
		getJSONWithClient(t, client, s.BaseURL+"/api/v1/analytics/overview", &ov)
		if len(ov.ByMachine) != 2 || ov.ByMachine[0].Machine != "m-alpha" || ov.ByMachine[1].Machine != "m-zulu" {
			t.Fatalf("attempt %d: by_machine = %+v, want [m-alpha, m-zulu]", attempt, ov.ByMachine)
		}
	}
}

func TestOverview_F9_T2_OverviewRecentSessionsExcludesOlderThan30Days(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "ov-f9-t2-2"})
	now := time.Now().UTC()

	events := make([]ingestEvent, 0, 13)
	for i := 0; i < 10; i++ {
		events = append(events, ingestEvent{
			EventID:     fmt.Sprintf("ov-f9-old-%02d", i),
			Timestamp:   now.AddDate(0, 0, -45).Add(time.Duration(i) * time.Minute).Format(time.RFC3339Nano),
			MachineID:   "m-f9-win",
			SessionID:   fmt.Sprintf("sess-old45-%02d", i),
			Provider:    "openai",
			Model:       "gpt-5.4",
			Tool:        "codex",
			TotalTokens: i64(10),
		})
	}
	for i := 0; i < 3; i++ {
		events = append(events, ingestEvent{
			EventID:     fmt.Sprintf("ov-f9-new-%02d", i),
			Timestamp:   now.Add(-time.Duration(i+1) * time.Hour).Format(time.RFC3339Nano),
			MachineID:   "m-f9-win",
			SessionID:   fmt.Sprintf("sess-new-%02d", i),
			Provider:    "openai",
			Model:       "gpt-5.4",
			Tool:        "codex",
			TotalTokens: i64(20),
		})
	}
	s.ingestEvents(t, events)

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 160)
	html := overviewWaitHTMLContains(t, client, s.BaseURL, "sess-new-00")
	sessions := overviewSectionBetween(t, html, `<section class="panel sessions-panel"`, `</section>`)
	for i := 0; i < 3; i++ {
		if !strings.Contains(sessions, fmt.Sprintf("sess-new-%02d", i)) {
			t.Fatalf("missing recent session sess-new-%02d on /", i)
		}
	}
	if strings.Contains(sessions, "sess-old45-") {
		t.Fatalf("overview Recent sessions must not backfill >30d sessions")
	}

	allSessHTML := overviewGetHTML(t, client, s.BaseURL+"/sessions?period=all")
	if !strings.Contains(allSessHTML, "sess-old45-00") || !strings.Contains(allSessHTML, "sess-new-00") {
		t.Fatalf("/sessions?period=all should include both old and new sessions")
	}
}

func TestOverview_F9_T2_EventsWithoutSessionIDExcludedFromRecentSessions(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "ov-f9-t2-3"})
	now := time.Now().UTC()
	s.ingestEvents(t, []ingestEvent{
		{EventID: "ov-f9-nosess-1", Timestamp: now.Add(-1 * time.Minute).Format(time.RFC3339Nano), MachineID: "m-f9-ns", SessionID: "", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(500)},
		{EventID: "ov-f9-nosess-2", Timestamp: now.Add(-2 * time.Minute).Format(time.RFC3339Nano), MachineID: "m-f9-ns", SessionID: "", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(300)},
		{EventID: "ov-f9-sess-1", Timestamp: now.Add(-3 * time.Minute).Format(time.RFC3339Nano), MachineID: "m-f9-ns", SessionID: "sess-real-1", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(100)},
		{EventID: "ov-f9-sess-2", Timestamp: now.Add(-4 * time.Minute).Format(time.RFC3339Nano), MachineID: "m-f9-ns", SessionID: "sess-real-2", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(100)},
	})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 1000)

	var ov overviewPayload
	getJSONWithClient(t, client, s.BaseURL+"/api/v1/analytics/overview", &ov)
	if ov.Threads != 2 {
		t.Fatalf("threads = %d, want 2", ov.Threads)
	}
	html := overviewWaitHTMLContains(t, client, s.BaseURL, "sess-real-1")
	sessions := overviewSectionBetween(t, html, `<section class="panel sessions-panel"`, `</section>`)
	if got := strings.Count(sessions, `data-label="When"`); got != 2 {
		t.Fatalf("session row count = %d, want 2 (empty session_id excluded)", got)
	}
}

func TestOverview_F9_T2_MultiEventSameSessionAggregatesIntoSingleRow(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "ov-f9-t2-4"})
	now := time.Now().UTC()

	events := make([]ingestEvent, 0, 5)
	for i := 0; i < 5; i++ {
		events = append(events, ingestEvent{
			EventID:     fmt.Sprintf("ov-f9-agg-%d", i),
			Timestamp:   now.Add(-time.Duration(5-i) * time.Minute).Format(time.RFC3339Nano),
			MachineID:   "m-f9-agg",
			SessionID:   "sess-grouped-one",
			Provider:    "openai",
			Model:       "gpt-5.4",
			Tool:        "codex",
			TotalTokens: i64(200),
		})
	}
	s.ingestEvents(t, events)

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 1000)
	html := overviewWaitHTMLContains(t, client, s.BaseURL, "sess-grouped-one")
	sessions := overviewSectionBetween(t, html, `<section class="panel sessions-panel"`, `</section>`)
	if got := strings.Count(sessions, "sess-grouped-one"); got == 0 {
		t.Fatalf("expected sess-grouped-one in sessions panel")
	}
	if got := strings.Count(sessions, `data-label="When"`); got != 1 {
		t.Fatalf("expected 1 aggregated session row, got %d", got)
	}
	if !strings.Contains(sessions, "1,000") {
		t.Fatalf("expected aggregated session total 1,000 in %s", sessions)
	}
}

func TestOverview_F9_T2_EmptyOverviewShowsExplicitEmptyStates(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "ov-f9-t2-5"})
	client := s.authClient(t)
	html := overviewGetHTML(t, client, s.BaseURL+"/")

	for _, want := range []string{
		"No sessions in the selected window.",
		"No machines yet.",
		`<div class="stat-label">Top machine</div><div class="stat-value muted">—</div>`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("empty overview missing %q", want)
		}
	}
}

// =============================================================================
// Area 5: Feature 13 (Overview) — Breakdown Drill-Downs & Heatmap Click-Through (M3)
// =============================================================================

func TestOverview_F13_T1_OverviewBreakdownRowsHaveDrillDownLinks(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "ov-f13-t1-1"})
	s.ingestEvents(t, []ingestEvent{
		{EventID: "ov-f13-1", MachineID: "m-drill-a", SessionID: "s-d1", Project: "proj-alpha", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(400)},
		{EventID: "ov-f13-2", MachineID: "m-drill-b", SessionID: "s-d2", Project: "proj-beta", Provider: "anthropic", Model: "claude-sonnet-4", Tool: "claude-code", TotalTokens: i64(200)},
	})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 600)
	html := overviewWaitHTMLContains(t, client, s.BaseURL, "proj-alpha")

	tables := []struct {
		caption string
		param   string
	}{
		{"Project token usage", "project="},
		{"Harness token usage", "tool="},
		{"Model token usage", "model="},
		{"Machine token usage", "machine="},
	}
	for _, tbl := range tables {
		seg := overviewSectionBetween(t, html, fmt.Sprintf(`<table aria-label="%s">`, tbl.caption), `</table>`)
		hrefs := overviewExtractHrefs(seg)
		found := false
		for _, h := range hrefs {
			if (strings.HasPrefix(h, "/analytics?") || strings.HasPrefix(h, "/sessions?")) && strings.Contains(h, tbl.param) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("table %q has no clickable drill-down <a href> carrying %q; hrefs=%v", tbl.caption, tbl.param, hrefs)
		}
	}
}

func TestOverview_F13_T1_FollowingOverviewDrillDownLinksFiltersResults(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "ov-f13-t1-2"})
	s.ingestEvents(t, []ingestEvent{
		{EventID: "ov-f13-f1", MachineID: "m-follow-1", SessionID: "s-f1", Project: "proj-target", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(777)},
		{EventID: "ov-f13-f2", MachineID: "m-follow-2", SessionID: "s-f2", Project: "proj-other", Provider: "anthropic", Model: "claude-sonnet-4", Tool: "claude-code", TotalTokens: i64(333)},
	})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 1110)
	html := overviewWaitHTMLContains(t, client, s.BaseURL, "proj-target")

	for _, tableCaption := range []string{"Project token usage", "Harness token usage", "Model token usage", "Machine token usage"} {
		seg := overviewSectionBetween(t, html, fmt.Sprintf(`<table aria-label="%s">`, tableCaption), `</table>`)
		var targetHref string
		for _, h := range overviewExtractHrefs(seg) {
			if strings.Contains(h, "proj-target") || strings.Contains(h, "codex") || strings.Contains(h, "gpt-5.4") || strings.Contains(h, "m-follow-1") {
				targetHref = h
				break
			}
		}
		if targetHref == "" {
			t.Fatalf("no target drill-down href found in %q", tableCaption)
		}
		targetHTML := overviewGetHTML(t, client, s.BaseURL+targetHref)
		if !strings.Contains(targetHTML, "777") {
			t.Fatalf("following %s did not display filtered total 777", targetHref)
		}
		if strings.Contains(targetHTML, "333") {
			t.Fatalf("following %s leaked excluded entity total 333", targetHref)
		}
		bodyNoForms := overviewStripForms(targetHTML)
		for _, excluded := range []string{"proj-other", "m-follow-2", "s-f2"} {
			if strings.Contains(bodyNoForms, excluded) {
				t.Fatalf("following %s leaked excluded entity %q outside filter controls", targetHref, excluded)
			}
		}
	}
}

func TestOverview_F13_T1_HeatmapNonFutureCellsCarryDateAndNavigateToDaySessions(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "ov-f13-t1-3", Timezone: "UTC"})
	now := time.Now().UTC()
	d1 := now.AddDate(0, 0, -2).Format("2006-01-02")
	d2 := now.AddDate(0, 0, -5).Format("2006-01-02")

	s.ingestEvents(t, []ingestEvent{
		{EventID: "ov-f13-hm1", Timestamp: d1 + "T12:00:00Z", MachineID: "m-hm", SessionID: "sess-hm-d1", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(300)},
		{EventID: "ov-f13-hm2", Timestamp: d2 + "T12:00:00Z", MachineID: "m-hm", SessionID: "sess-hm-d2", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(200)},
	})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 500)
	html := overviewWaitHTMLContains(t, client, s.BaseURL, d1)

	grid := overviewSectionBetween(t, html, `class="activity-grid"`, `class="content-grid"`)
	if !strings.Contains(grid, fmt.Sprintf(`data-date="%s"`, d1)) || !strings.Contains(grid, fmt.Sprintf(`data-date="%s"`, d2)) {
		t.Fatalf("expected non-future heatmap cells to carry data-date=%s and %s", d1, d2)
	}
	if !strings.Contains(html, "/sessions?from=") {
		t.Fatalf("expected overview script/markup to wire heatmap click-through to /sessions?from=...")
	}
}

func TestOverview_F13_T1_DayFilteredSessionsEndpointReturnsOnlyThatDay(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "ov-f13-t1-4", Timezone: "UTC"})
	now := time.Now().UTC()
	d1 := now.AddDate(0, 0, -2).Format("2006-01-02")
	d2 := now.AddDate(0, 0, -4).Format("2006-01-02")

	s.ingestEvents(t, []ingestEvent{
		{EventID: "ov-f13-day1", Timestamp: d1 + "T14:00:00Z", MachineID: "m-day", SessionID: "sess-only-d1", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(111)},
		{EventID: "ov-f13-day2", Timestamp: d2 + "T14:00:00Z", MachineID: "m-day", SessionID: "sess-only-d2", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(222)},
	})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 333)

	sessHTML := overviewGetHTML(t, client, fmt.Sprintf("%s/sessions?from=%s&to=%s", s.BaseURL, d1, d1))
	if !strings.Contains(sessHTML, "sess-only-d1") || strings.Contains(sessHTML, "sess-only-d2") {
		t.Fatalf("/sessions?from=%s&to=%s did not isolate day d1 sessions", d1, d1)
	}
	var rows []sessionRow
	getJSONWithClient(t, client, fmt.Sprintf("%s/api/v1/sessions?from=%s&to=%s", s.BaseURL, d1, d1), &rows)
	if len(rows) != 1 || rows[0].SessionID != "sess-only-d1" {
		t.Fatalf("/api/v1/sessions?from=%s&to=%s = %+v, want [sess-only-d1]", d1, d1, rows)
	}
}

func TestOverview_F13_T1_HeatmapPreservesRovingTabindexAndSpanContract(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "ov-f13-t1-5", Timezone: "UTC"})
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "ov-f13-rov-1",
		MachineID:   "m-rov",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(50),
	}})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 50)
	html := overviewWaitHTMLContains(t, client, s.BaseURL, `class="activity-grid"`)
	grid := overviewSectionBetween(t, html, `class="activity-grid"`, `</section>`)

	cellRe := regexp.MustCompile(`<span class="activity-cell[^"]*"[^>]*role="gridcell"[^>]*>`)
	cells := cellRe.FindAllString(grid, -1)
	if len(cells) != 371 {
		t.Fatalf("heatmap cell count = %d, want 371 (53*7)", len(cells))
	}
	tabZeroCount := 0
	for _, cell := range cells {
		if strings.Contains(cell, `tabindex="0"`) {
			tabZeroCount++
		}
		if strings.Contains(cell, " future") {
			if !strings.Contains(cell, `aria-hidden="true"`) {
				t.Fatalf("future heatmap cell missing aria-hidden=true: %s", cell)
			}
			if strings.Contains(cell, "tabindex=") || strings.Contains(cell, "data-date=") {
				t.Fatalf("future heatmap cell must not have tabindex or clickable data-date: %s", cell)
			}
		}
	}
	if tabZeroCount != 1 {
		t.Fatalf("roving tabindex=\"0\" count = %d, want exactly 1", tabZeroCount)
	}
}

func TestOverview_F13_T2_SpecialCharactersInProjectAndModelURLEscaped(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "ov-f13-t2-1"})
	s.ingestEvents(t, []ingestEvent{
		{EventID: "ov-f13-sp1", MachineID: "m-sp", SessionID: "s-sp1", Project: "my repo&v2+alpha", Provider: "openai", Model: "org/model+v2", Tool: "codex", TotalTokens: i64(444)},
		{EventID: "ov-f13-sp2", MachineID: "m-sp", SessionID: "s-sp2", Project: "plain-proj", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(111)},
	})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 555)
	// Go's html/template escapes '&' as '&amp;' and '+' as '&#43;' (UTF-7 XSS guard).
	html := overviewWaitHTMLContains(t, client, s.BaseURL, "my repo&amp;v2&#43;alpha")

	projSeg := overviewSectionBetween(t, html, `<table aria-label="Project token usage">`, `</table>`)
	var projHref string
	for _, h := range overviewExtractHrefs(projSeg) {
		if strings.Contains(h, "project=") {
			projHref = h
			break
		}
	}
	if projHref == "" {
		t.Fatalf("expected URL-escaped project drill-down href in %s", projSeg)
	}
	u, err := url.Parse(projHref)
	if err != nil {
		t.Fatalf("parse drill-down href %q: %v", projHref, err)
	}
	if got := u.Query().Get("project"); got != "my repo&v2+alpha" {
		t.Fatalf("decoded project query param = %q, want %q (raw href=%q)", got, "my repo&v2+alpha", projHref)
	}
	targetHTML := overviewGetHTML(t, client, s.BaseURL+projHref)
	if !strings.Contains(targetHTML, "444") {
		t.Fatalf("following escaped project drill-down %q did not show 444", projHref)
	}
	bodyNoForms := overviewStripForms(targetHTML)
	for _, excluded := range []string{"plain-proj", "s-sp2"} {
		if strings.Contains(bodyNoForms, excluded) {
			t.Fatalf("following escaped project drill-down %q leaked excluded %q outside filter controls", projHref, excluded)
		}
	}
}

func TestOverview_F13_T2_UnknownProjectOrModelRowHandledSafely(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "ov-f13-t2-2"})
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "ov-f13-unk",
		MachineID:   "m-unk",
		SessionID:   "s-unk",
		Project:     "",
		Provider:    "anthropic",
		Model:       "unknown",
		Tool:        "claude-code",
		TotalTokens: i64(123),
	}})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 123)
	html := overviewWaitHTMLContains(t, client, s.BaseURL, "Unknown project")

	projSeg := overviewSectionBetween(t, html, `<table aria-label="Project token usage">`, `</table>`)
	for _, h := range overviewExtractHrefs(projSeg) {
		u, err := url.Parse(h)
		if err != nil {
			t.Fatalf("invalid href %q in unknown project row: %v", h, err)
		}
		if u.Query().Has("project") && u.Query().Get("project") == "" {
			t.Fatalf("unknown project row must not link to empty project= filter (%q)", h)
		}
	}
}

func TestOverview_F13_T2_HeatmapDayClickThroughHonorsConfiguredTimezone(t *testing.T) {
	requireE2E(t)
	loc := overviewLoadLoc(t, "America/New_York")
	s := startTestServer(t, serverOpts{IngestToken: "ov-f13-t2-3", Timezone: "America/New_York"})

	nowUTC := time.Now().UTC()
	// 03:00 UTC is 22:00 EST on the previous calendar day in New York.
	ts := time.Date(nowUTC.Year(), nowUTC.Month(), nowUTC.Day(), 3, 0, 0, 0, time.UTC).AddDate(0, 0, -2)
	localDay := ts.In(loc).Format("2006-01-02")
	utcDay := ts.UTC().Format("2006-01-02")
	if localDay == utcDay {
		t.Fatalf("setup invariant violated: localDay=%s == utcDay=%s", localDay, utcDay)
	}

	s.ingestEvents(t, []ingestEvent{{
		EventID:     "ov-f13-tz",
		Timestamp:   ts.Format(time.RFC3339Nano),
		MachineID:   "m-ny",
		SessionID:   "sess-ny-boundary",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(90),
	}})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 90)

	var localRows []sessionRow
	getJSONWithClient(t, client, fmt.Sprintf("%s/api/v1/sessions?from=%s&to=%s", s.BaseURL, localDay, localDay), &localRows)
	if len(localRows) != 1 || localRows[0].SessionID != "sess-ny-boundary" {
		t.Fatalf("expected sess-ny-boundary on localDay %s, got %+v", localDay, localRows)
	}
	var utcRows []sessionRow
	getJSONWithClient(t, client, fmt.Sprintf("%s/api/v1/sessions?from=%s&to=%s", s.BaseURL, utcDay, utcDay), &utcRows)
	if len(utcRows) != 0 {
		t.Fatalf("expected 0 sessions on utcDay %s, got %+v", utcDay, utcRows)
	}
}

func TestOverview_F13_T2_InvalidFromToOnSessionsRejectedOrBounded(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "ov-f13-t2-4"})
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "ov-f13-inv",
		MachineID:   "m-inv",
		SessionID:   "sess-inv",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(50),
	}})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 50)

	for _, path := range []string{
		"/sessions?from=not-a-date&to=2026-11-20",
		"/sessions?from=2026-11-20&to=2026-11-10",
		"/api/v1/sessions?from=not-a-date&to=2026-11-20",
		"/api/v1/sessions?from=2026-11-20&to=2026-11-10",
	} {
		code, _, body := overviewGetResp(t, client, s.BaseURL+path, nil)
		if code >= 500 {
			t.Fatalf("%s returned 5xx (%d): %s", path, code, string(body))
		}
		if code != http.StatusOK && code != http.StatusBadRequest {
			t.Fatalf("%s returned unexpected status %d", path, code)
		}
		lower := strings.ToLower(string(body))
		if strings.Contains(lower, "sqlite") || strings.Contains(lower, "syntax error") {
			t.Fatalf("%s leaked SQL error: %s", path, string(body))
		}
		// Ensure inverted or malformed /api/v1/sessions doesn't silently match sess-inv when expecting 0 or 400.
		if code == http.StatusOK && strings.Contains(path, "from=2026-11-20&to=2026-11-10") && strings.Contains(string(body), "sess-inv") {
			t.Fatalf("%s with inverted dates should not return today's sess-inv", path)
		}
	}
}

func TestOverview_F13_T2_DrillDownPreservesAuthGate(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{
		IngestToken: "ov-f13-t2-5-ing",
		Password:    "drill-pass-123",
	})

	for _, path := range []string{
		"/sessions?from=2026-11-20&to=2026-11-20",
		"/analytics?dimension=models&model=gpt-5.4",
	} {
		resp, _ := s.doReq(t, http.MethodGet, path, nil, map[string]string{"Accept": "text/html"})
		if resp.StatusCode != http.StatusSeeOther {
			t.Fatalf("unauthenticated HTML %s status = %d, want 303", path, resp.StatusCode)
		}
		if loc := resp.Header.Get("Location"); !strings.HasPrefix(loc, "/login") {
			t.Fatalf("unauthenticated HTML %s Location = %q, want /login...", path, loc)
		}
		respAPI, _ := s.doReq(t, http.MethodGet, path, nil, map[string]string{"Accept": "application/json"})
		if respAPI.StatusCode != http.StatusUnauthorized {
			t.Fatalf("unauthenticated JSON %s status = %d, want 401", path, respAPI.StatusCode)
		}
	}
}

// =============================================================================
// Area 6: Feature 15 — In-Place / Live Updates Without 60s location.reload() (M3)
// =============================================================================

func TestOverview_F15_T1_NoAutoRefreshFullPageReloadAndControlsPreserved(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "ov-f15-t1-1"})
	client := s.authClient(t)
	html := overviewGetHTML(t, client, s.BaseURL+"/")

	for _, want := range []string{
		`id="refresh-now"`,
		`aria-label="Refresh now"`,
		`id="auto-refresh-toggle"`,
		`aria-pressed="false"`,
		`Pause auto-refresh`,
		`class="refresh-glyph"`,
		`class="refresh-label"`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("/ HTML missing control markup %q", want)
		}
	}
	if strings.Contains(strings.ToLower(html), `http-equiv="refresh"`) {
		t.Fatalf("/ HTML must not contain meta http-equiv=refresh")
	}

	overviewAssertNoAutoRefreshReload(t, html)
	if !strings.Contains(html, "fetch(") && !strings.Contains(html, "EventSource(") {
		t.Fatalf("expected / script to use fetch() or EventSource() for live updates")
	}
}

func TestOverview_F15_T1_LiveUpdateReflectsNewIngestInOverviewAndHTML(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "ov-f15-t1-2"})
	client := s.authClient(t)

	s.ingestEvents(t, []ingestEvent{{
		EventID:      "ov-f15-u1",
		MachineID:    "m-live-1",
		SessionID:    "sess-live-1",
		Provider:     "openai",
		Model:        "gpt-5.4",
		Tool:         "codex",
		InputTokens:  i64(100),
		OutputTokens: i64(50),
		TotalTokens:  i64(150),
	}})
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 150)
	_ = overviewWaitHTMLContains(t, client, s.BaseURL, "sess-live-1")

	s.ingestEvents(t, []ingestEvent{{
		EventID:      "ov-f15-u2",
		MachineID:    "m-live-1",
		SessionID:    "sess-live-2",
		Provider:     "openai",
		Model:        "gpt-5.4",
		Tool:         "codex",
		InputTokens:  i64(200),
		OutputTokens: i64(50),
		TotalTokens:  i64(250),
	}})
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 400)
	html2 := overviewWaitHTMLContains(t, client, s.BaseURL, "sess-live-2")
	if !strings.Contains(html2, `data-display="400"`) {
		t.Fatalf("expected updated lifetime counter 400 in / HTML")
	}
}

func TestOverview_F15_T1_ETagConditionalGetWhenSupported(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "ov-f15-t1-3"})
	client := s.authClient(t)

	s.ingestEvents(t, []ingestEvent{{
		EventID:     "ov-f15-etag-1",
		MachineID:   "m-etag",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(100),
	}})
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 100)

	// Require at least one live overview endpoint or / to emit ETag or in-place update signal,
	// and verify conditional GET behavior cleanly against the exact endpoint whose ETag was captured.
	targetURL := s.BaseURL + "/api/v1/analytics/overview"
	code, hdr, body := overviewGetResp(t, client, targetURL, nil)
	if code != http.StatusOK {
		t.Fatalf("overview API status = %d", code)
	}
	etag := hdr.Get("ETag")
	var condBaseHeaders map[string]string
	if etag == "" {
		targetURL = s.BaseURL + "/"
		condBaseHeaders = map[string]string{"Accept": "text/html"}
		codePage, hdrPage, _ := overviewGetResp(t, client, targetURL, condBaseHeaders)
		if codePage != http.StatusOK {
			t.Fatalf("/ status = %d", codePage)
		}
		etag = hdrPage.Get("ETag")
	}
	if etag != "" {
		condHeaders := map[string]string{"If-None-Match": etag}
		for k, v := range condBaseHeaders {
			condHeaders[k] = v
		}
		condCode, _, _ := overviewGetResp(t, client, targetURL, condHeaders)
		if condCode != http.StatusNotModified {
			t.Fatalf("expected 304 Not Modified for %s matching ETag %q, got %d (body len=%d)", targetURL, etag, condCode, len(body))
		}
	}
	// Also ensure M3 replaced 60s location.reload() with live polling/SSE.
	html := overviewGetHTML(t, client, s.BaseURL+"/")
	overviewAssertNoAutoRefreshReload(t, html)
}

func TestOverview_F15_T1_EvolutionStageTransitionUpdatesWithoutCrash(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "ov-f15-t1-4"})
	client := s.authClient(t)

	html0 := overviewGetHTML(t, client, s.BaseURL+"/")
	if !strings.Contains(html0, `data-stage="0"`) {
		t.Fatalf("expected initial data-stage=\"0\"")
	}

	s.ingestEvents(t, []ingestEvent{{
		EventID:     "ov-f15-stage-1",
		MachineID:   "m-stage",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(15),
	}})
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 15)
	html1 := overviewWaitHTMLContains(t, client, s.BaseURL, `data-stage="1"`)
	if !strings.Contains(html1, "Bitling") || !strings.Contains(html1, "stage-01.png") {
		t.Fatalf("expected stage 1 Bitling / stage-01.png after crossing threshold")
	}
}

func TestOverview_F15_T1_AnyDashboardPollingOrStreamEndpointInHTMLWorks(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "ov-f15-t1-5"})
	client := s.authClient(t)
	html := overviewGetHTML(t, client, s.BaseURL+"/")

	apiRe := regexp.MustCompile(`['"](/api/v1/[a-zA-Z0-9/_-]+)['"]`)
	matches := apiRe.FindAllStringSubmatch(html, -1)
	if len(matches) == 0 {
		t.Fatalf("expected at least one /api/v1/ endpoint referenced in / HTML")
	}
	seen := make(map[string]bool)
	for _, m := range matches {
		path := m[1]
		if seen[path] {
			continue
		}
		seen[path] = true
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.BaseURL+path, nil)
		if err != nil {
			cancel()
			t.Fatalf("new GET %s: %v", path, err)
		}
		resp, err := client.Do(req)
		if err != nil {
			cancel()
			t.Fatalf("GET %s: %v", path, err)
		}
		if resp.StatusCode != http.StatusOK {
			_ = resp.Body.Close()
			cancel()
			t.Fatalf("referenced endpoint %s returned %d, want 200", path, resp.StatusCode)
		}
		ct := resp.Header.Get("Content-Type")
		if strings.Contains(ct, "text/event-stream") {
			t.Logf("detected SSE endpoint %s", path)
			closeErr := resp.Body.Close()
			cancel()
			if closeErr != nil {
				t.Fatalf("close SSE %s body: %v", path, closeErr)
			}
			continue
		}
		_, readErr := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
		closeErr := resp.Body.Close()
		cancel()
		if readErr != nil {
			t.Fatalf("read GET %s body: %v", path, readErr)
		}
		if closeErr != nil {
			t.Fatalf("close GET %s body: %v", path, closeErr)
		}
	}
	// Must also not rely on 60s location.reload().
	overviewAssertNoAutoRefreshReload(t, html)
}

func TestOverview_F15_T2_AllPollingAndStreamRoutesRequireDashboardAuth(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{
		IngestToken:    "ov-f15-ing-tok",
		DashboardToken: "ov-f15-dash-tok",
	})
	client := s.authClient(t)
	html := overviewGetHTML(t, client, s.BaseURL+"/")

	apiRe := regexp.MustCompile(`['"](/api/v1/[a-zA-Z0-9/_-]+)['"]`)
	seen := make(map[string]bool)
	for _, m := range apiRe.FindAllStringSubmatch(html, -1) {
		path := m[1]
		if seen[path] {
			continue
		}
		seen[path] = true
		resp, _ := s.doReq(t, http.MethodGet, path, nil, nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("unauthenticated GET %s status = %d, want 401", path, resp.StatusCode)
		}
	}
	overviewAssertNoAutoRefreshReload(t, html)
}

func TestOverview_F15_T2_StaleIfNoneMatchAfterIngestDoesNotStickAt304(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "ov-f15-t2-2"})
	client := s.authClient(t)

	s.ingestEvents(t, []ingestEvent{{
		EventID:     "ov-f15-inm-1",
		MachineID:   "m-inm",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(100),
	}})
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 100)
	_, hdr1, _ := overviewGetResp(t, client, s.BaseURL+"/api/v1/analytics/overview", nil)
	etag1 := hdr1.Get("ETag")

	s.ingestEvents(t, []ingestEvent{{
		EventID:     "ov-f15-inm-2",
		MachineID:   "m-inm",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(250),
	}})
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 350)

	if etag1 != "" {
		code, hdr2, body2 := overviewGetResp(t, client, s.BaseURL+"/api/v1/analytics/overview", map[string]string{"If-None-Match": etag1})
		if code != http.StatusOK {
			t.Fatalf("stale If-None-Match %q after ingest returned %d, want 200", etag1, code)
		}
		if hdr2.Get("ETag") == etag1 {
			t.Fatalf("ETag did not change after ingest (%q)", etag1)
		}
		if !strings.Contains(string(body2), "350") {
			t.Fatalf("expected updated 350 tokens in response body")
		}
	}
	html := overviewGetHTML(t, client, s.BaseURL+"/")
	overviewAssertNoAutoRefreshReload(t, html)
}

func TestOverview_F15_T2_BurstIngestsCoalesceWithoutErrorUnderConcurrentReads(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "ov-f15-t2-3"})
	client := s.authClient(t)

	var wg sync.WaitGroup
	errCh := make(chan string, 64)
	nowStr := time.Now().UTC().Format(time.RFC3339Nano)
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func(idx int) {
			defer wg.Done()
			payload, err := json.Marshal(map[string]any{
				"events": []ingestEvent{{
					SchemaVersion: "1",
					EventID:       fmt.Sprintf("ov-f15-burst-%d", idx),
					Timestamp:     nowStr,
					MachineID:     "m-burst",
					Provider:      "openai",
					Model:         "gpt-5.4",
					Tool:          "codex",
					TotalTokens:   i64(100),
					TokenAccuracy: "reported",
					Source:        ingestSource{Adapter: "test", AdapterVersion: "1.0"},
				}},
			})
			if err != nil {
				errCh <- fmt.Sprintf("marshal burst %d: %v", idx, err)
				return
			}
			req, err := http.NewRequest(http.MethodPost, s.BaseURL+"/api/v1/events/batch", bytes.NewReader(payload))
			if err != nil {
				errCh <- fmt.Sprintf("new burst req %d: %v", idx, err)
				return
			}
			req.Header.Set("Content-Type", "application/json")
			if s.IngestToken != "" {
				req.Header.Set("Authorization", "Bearer "+s.IngestToken)
			}
			resp, err := client.Do(req)
			if err != nil {
				errCh <- fmt.Sprintf("POST /api/v1/events/batch %d: %v", idx, err)
				return
			}
			body, readErr := io.ReadAll(resp.Body)
			closeErr := resp.Body.Close()
			if readErr != nil {
				errCh <- fmt.Sprintf("read batch %d: %v", idx, readErr)
				return
			}
			if closeErr != nil {
				errCh <- fmt.Sprintf("close batch %d: %v", idx, closeErr)
				return
			}
			if resp.StatusCode != http.StatusOK {
				errCh <- fmt.Sprintf("POST /api/v1/events/batch %d -> %d: %s", idx, resp.StatusCode, string(body))
			}
		}(i)
	}
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for _, p := range []string{"/", "/api/v1/analytics/overview", "/api/v1/evolution"} {
				req, err := http.NewRequest(http.MethodGet, s.BaseURL+p, nil)
				if err != nil {
					errCh <- fmt.Sprintf("new GET %s: %v", p, err)
					return
				}
				resp, err := client.Do(req)
				if err != nil {
					errCh <- fmt.Sprintf("GET %s: %v", p, err)
					return
				}
				_, readErr := io.Copy(io.Discard, resp.Body)
				closeErr := resp.Body.Close()
				if readErr != nil {
					errCh <- fmt.Sprintf("read GET %s: %v", p, readErr)
					return
				}
				if closeErr != nil {
					errCh <- fmt.Sprintf("close GET %s: %v", p, closeErr)
					return
				}
				if resp.StatusCode >= 500 || resp.StatusCode != http.StatusOK {
					errCh <- fmt.Sprintf("GET %s -> %d", p, resp.StatusCode)
				}
			}
		}()
	}
	wg.Wait()
	close(errCh)
	for msg := range errCh {
		t.Fatalf("concurrent read/ingest failure: %s", msg)
	}
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 500)
	html := overviewGetHTML(t, client, s.BaseURL+"/")
	overviewAssertNoAutoRefreshReload(t, html)
}

func TestOverview_F15_T2_AutoRefreshTogglePersistsSessionStorageKey(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "ov-f15-t2-4"})
	client := s.authClient(t)
	html := overviewGetHTML(t, client, s.BaseURL+"/")

	if !strings.Contains(html, "tokemon-auto-refresh") {
		t.Fatalf("expected sessionStorage key tokemon-auto-refresh in / script")
	}
	if !strings.Contains(html, "autoPaused()") {
		t.Fatalf("expected autoPaused() guard in / script")
	}
	overviewAssertNoAutoRefreshReload(t, html)
}

func TestOverview_F15_T2_StreamOrPollCleanShutdownUnderContextCancel(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "ov-f15-t2-5"})
	client := s.authClient(t)
	html := overviewGetHTML(t, client, s.BaseURL+"/")

	req, err := http.NewRequest(http.MethodGet, s.BaseURL+"/api/v1/analytics/overview", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("GET overview: %v", err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatalf("close overview body early: %v", err)
	}

	hResp, _ := s.doReq(t, http.MethodGet, "/healthz", nil, nil)
	if hResp.StatusCode != http.StatusOK {
		t.Fatalf("/healthz after canceled read = %d, want 200", hResp.StatusCode)
	}
	overviewAssertNoAutoRefreshReload(t, html)
}

// =============================================================================
// Area 7: Feature 21 (Overview) — / Compact Budget Pace Bar (M5)
// =============================================================================

func TestOverview_F21_T1_AbsentOnOverviewWhenNoQuotasConfigured(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "ov-f21-t1-1"})
	client := s.authClient(t)

	var statuses []overviewQuotaStatus
	getJSONWithClient(t, client, s.BaseURL+"/api/v1/quotas", &statuses)
	if len(statuses) != 0 {
		t.Fatalf("/api/v1/quotas len = %d, want 0", len(statuses))
	}
	html := overviewGetHTML(t, client, s.BaseURL+"/")
	// Strip <style> and <script> blocks so CSS rules (.quota-pace-panel) and JS
	// selectors do not trigger false positives when the quota DOM section is omitted.
	domHTML := regexp.MustCompile(`(?is)<(style|script)\b[^>]*>.*?</(?:style|script)>`).ReplaceAllString(html, "")
	for _, absent := range []string{"quota-panel", "quota-pace", "quota-title", "quota-track", "Config-defined budgets"} {
		if strings.Contains(domHTML, absent) {
			t.Fatalf("unconfigured / HTML must not render quota marker %q", absent)
		}
	}
}

func TestOverview_F21_T1_PresentOnOverviewWhenYAMLQuotaConfigured(t *testing.T) {
	requireE2E(t)
	quotasYAML := `schema_version: "1"
quotas:
  - label: "OpenAI Monthly"
    provider: openai
    reset_day: 1
    budget:
      tokens_per_month: 10000
`
	s := startTestServer(t, serverOpts{IngestToken: "ov-f21-t1-2", QuotasYAML: quotasYAML})
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "ov-f21-yaml-1",
		MachineID:   "m-q1",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(4000),
	}})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 4000)

	var statuses []overviewQuotaStatus
	getJSONWithClient(t, client, s.BaseURL+"/api/v1/quotas", &statuses)
	if len(statuses) != 1 || statuses[0].TokensUsed != 4000 || statuses[0].State != "ok" {
		t.Fatalf("/api/v1/quotas = %+v, want tokens_used=4000 state=ok", statuses)
	}

	html := overviewWaitHTMLContains(t, client, s.BaseURL, "OpenAI Monthly")
	quotaSec := overviewQuotaSection(t, html, "OpenAI Monthly")
	if !strings.Contains(quotaSec, "OpenAI Monthly") {
		t.Fatalf("/ HTML missing compact budget pace bar label 'OpenAI Monthly' in quota section: %s", quotaSec)
	}
	if !strings.Contains(quotaSec, "40%") && !strings.Contains(quotaSec, "4,000") && !strings.Contains(quotaSec, "4K") {
		t.Fatalf("/ HTML quota pace bar missing usage figure (40%% / 4,000 / 4K) in quota section: %s", quotaSec)
	}
}

func TestOverview_F21_T1_OverBudgetQuotaShowsOverStateOnOverview(t *testing.T) {
	requireE2E(t)
	quotasYAML := `schema_version: "1"
quotas:
  - label: "Cap 1K"
    provider: openai
    reset_day: 1
    budget:
      tokens_per_month: 1000
`
	s := startTestServer(t, serverOpts{IngestToken: "ov-f21-t1-3", QuotasYAML: quotasYAML})
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "ov-f21-over-1",
		MachineID:   "m-q2",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(1500),
	}})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 1500)

	var statuses []overviewQuotaStatus
	getJSONWithClient(t, client, s.BaseURL+"/api/v1/quotas", &statuses)
	if len(statuses) != 1 || statuses[0].State != "over" {
		t.Fatalf("quota state = %+v, want over", statuses)
	}
	html := overviewWaitHTMLContains(t, client, s.BaseURL, "Cap 1K")
	quotaSec := overviewQuotaSection(t, html, "Cap 1K")
	if !strings.Contains(quotaSec, "1,500") && !strings.Contains(quotaSec, "1.5K") {
		t.Fatalf("/ HTML quota bar missing 1,500 / 1.5K used in quota section: %s", quotaSec)
	}
	if !strings.Contains(strings.ToLower(quotaSec), "over") {
		t.Fatalf("/ HTML quota bar missing over-budget indicator/class in quota section: %s", quotaSec)
	}
}

func TestOverview_F21_T1_OverviewPaceBarUpdatesWithSQLiteQuotaCRUD(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "ov-f21-t1-4"})
	client := s.authClient(t)

	settingsHTML := overviewGetHTML(t, client, s.BaseURL+"/settings")
	if !strings.Contains(strings.ToLower(settingsHTML), "quota") {
		t.Fatalf("/settings missing SQLite Quota management UI")
	}
	csrf := extractCSRFToken(t, []byte(settingsHTML))

	// Look for quota form action on /settings.
	actionRe := regexp.MustCompile(`<form[^>]*action="([^"]*quota[^"]*)"[^>]*>`)
	m := actionRe.FindStringSubmatch(settingsHTML)
	if len(m) < 2 {
		t.Fatalf("/settings missing quota form action")
	}
	form := url.Values{}
	form.Set("csrf_token", csrf)
	form.Set("label", "SQLite Monthly")
	form.Set("provider", "openai")
	form.Set("reset_day", "1")
	form.Set("tokens_per_month", "5000")

	req, err := http.NewRequest(http.MethodPost, s.BaseURL+m[1], strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatalf("new quota POST: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", s.BaseURL)
	req.Header.Set("Referer", s.BaseURL+"/settings")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", m[1], err)
	}
	_, err = io.Copy(io.Discard, resp.Body)
	if closeErr := resp.Body.Close(); closeErr != nil {
		t.Fatalf("close POST quota body: %v", closeErr)
	}
	if err != nil {
		t.Fatalf("drain POST quota body: %v", err)
	}
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("POST %s status = %d", m[1], resp.StatusCode)
	}

	ovHTML := overviewWaitHTMLContains(t, client, s.BaseURL, "SQLite Monthly")
	if !strings.Contains(ovHTML, "SQLite Monthly") {
		t.Fatalf("/ HTML did not show created SQLite quota")
	}
}

func TestOverview_F21_T1_SpecificityNoDoubleCountBetweenCatchAllAndModelQuota(t *testing.T) {
	requireE2E(t)
	quotasYAML := `schema_version: "1"
quotas:
  - label: "All Usage"
    reset_day: 1
    budget:
      tokens_per_month: 20000
  - label: "GPT-5.4 Quota"
    provider: openai
    model: gpt-5.4
    reset_day: 1
    budget:
      tokens_per_month: 10000
`
	s := startTestServer(t, serverOpts{IngestToken: "ov-f21-t1-5", QuotasYAML: quotasYAML})
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "ov-f21-spec-1",
		MachineID:   "m-q3",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(2500),
	}})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 2500)

	var statuses []overviewQuotaStatus
	getJSONWithClient(t, client, s.BaseURL+"/api/v1/quotas", &statuses)
	byLabel := make(map[string]int64)
	for _, st := range statuses {
		byLabel[st.Label] = st.TokensUsed
	}
	if byLabel["GPT-5.4 Quota"] != 2500 || byLabel["All Usage"] != 0 {
		t.Fatalf("double-count or misattribution: %+v", byLabel)
	}
	html := overviewWaitHTMLContains(t, client, s.BaseURL, "GPT-5.4 Quota")
	if !strings.Contains(html, "All Usage") {
		t.Fatalf("/ HTML missing catch-all quota label")
	}
}

func TestOverview_F21_T2_CostQuotaWithUnpricedEventsShowsUnknownNeverZero(t *testing.T) {
	requireE2E(t)
	quotasYAML := `schema_version: "1"
quotas:
  - label: "Monthly Spend"
    reset_day: 1
    budget:
      cost_per_month: 50.0
`
	s := startTestServer(t, serverOpts{IngestToken: "ov-f21-t2-1", QuotasYAML: quotasYAML})
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "ov-f21-unp-1",
		MachineID:   "m-qu",
		Provider:    "custom",
		Model:       "uncataloged-model-x",
		Tool:        "codex",
		TotalTokens: i64(1000),
	}})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 1000)

	var statuses []overviewQuotaStatus
	getJSONWithClient(t, client, s.BaseURL+"/api/v1/quotas", &statuses)
	if len(statuses) != 1 || statuses[0].State != "unknown" || statuses[0].CostUsed != nil {
		t.Fatalf("expected state=unknown, cost_used=nil; got %+v", statuses)
	}
	html := overviewWaitHTMLContains(t, client, s.BaseURL, "Monthly Spend")
	quotaSec := overviewQuotaSection(t, html, "Monthly Spend")
	if !strings.Contains(strings.ToLower(quotaSec), "unknown") {
		t.Fatalf("/ quota bar should display Unknown when cost is unpriced in quota section: %s", quotaSec)
	}
}

func TestOverview_F21_T2_PriorPeriodEventsExcludedFromCurrentPaceBar(t *testing.T) {
	requireE2E(t)
	quotasYAML := `schema_version: "1"
quotas:
  - label: "Current Month"
    provider: openai
    reset_day: 1
    budget:
      tokens_per_month: 1000
`
	s := startTestServer(t, serverOpts{IngestToken: "ov-f21-t2-2", QuotasYAML: quotasYAML})
	now := time.Now().UTC()
	s.ingestEvents(t, []ingestEvent{
		{EventID: "ov-f21-old", Timestamp: now.AddDate(0, -2, 0).Format(time.RFC3339Nano), MachineID: "m-qp", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(50000)},
		{EventID: "ov-f21-cur", Timestamp: now.Format(time.RFC3339Nano), MachineID: "m-qp", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(500)},
	})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 50500)

	var statuses []overviewQuotaStatus
	getJSONWithClient(t, client, s.BaseURL+"/api/v1/quotas", &statuses)
	if len(statuses) != 1 || statuses[0].TokensUsed != 500 || statuses[0].State != "ok" {
		t.Fatalf("expected prior period excluded (tokens_used=500, ok); got %+v", statuses)
	}
	html := overviewWaitHTMLContains(t, client, s.BaseURL, "Current Month")
	quotaSec := overviewQuotaSection(t, html, "Current Month")
	if !strings.Contains(quotaSec, "500") && !strings.Contains(quotaSec, "50%") {
		t.Fatalf("/ HTML quota pace bar did not show current period 500 / 50%% in quota section: %s", quotaSec)
	}
}

func TestOverview_F21_T2_ZeroUsageInCurrentPeriodRendersZeroPercentSafely(t *testing.T) {
	requireE2E(t)
	quotasYAML := `schema_version: "1"
quotas:
  - label: "Zero Usage Quota"
    provider: openai
    reset_day: 1
    budget:
      tokens_per_month: 10000
`
	s := startTestServer(t, serverOpts{IngestToken: "ov-f21-t2-3", QuotasYAML: quotasYAML})
	client := s.authClient(t)

	var statuses []overviewQuotaStatus
	getJSONWithClient(t, client, s.BaseURL+"/api/v1/quotas", &statuses)
	if len(statuses) != 1 || statuses[0].TokensUsed != 0 || statuses[0].State != "ok" {
		t.Fatalf("expected zero usage quota state=ok; got %+v", statuses)
	}
	html := overviewGetHTML(t, client, s.BaseURL+"/")
	if !strings.Contains(html, "Zero Usage Quota") {
		t.Fatalf("/ HTML missing Zero Usage Quota pace bar")
	}
	if strings.Contains(html, "NaN") {
		t.Fatalf("/ HTML contains NaN with zero-usage quota")
	}
}

func TestOverview_F21_T2_QuotaTimezoneBoundaryMatchesReportingTimezone(t *testing.T) {
	requireE2E(t)
	loc := overviewLoadLoc(t, "America/Toronto")
	nowLocal := time.Now().In(loc)
	// First instant of current Toronto month: midnight local is 04:00 or 05:00 UTC.
	monthStartLocal := time.Date(nowLocal.Year(), nowLocal.Month(), 1, 0, 0, 0, 0, loc)
	justBeforeMonth := monthStartLocal.Add(-5 * time.Minute).UTC()
	justInsideMonth := monthStartLocal.Add(5 * time.Minute).UTC()

	quotasYAML := `schema_version: "1"
quotas:
  - label: "Toronto Monthly"
    provider: openai
    reset_day: 1
    budget:
      tokens_per_month: 5000
`
	s := startTestServer(t, serverOpts{IngestToken: "ov-f21-t2-4", Timezone: "America/Toronto", QuotasYAML: quotasYAML})
	s.ingestEvents(t, []ingestEvent{
		{EventID: "ov-f21-tz-before", Timestamp: justBeforeMonth.Format(time.RFC3339Nano), MachineID: "m-qtz", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(900)},
		{EventID: "ov-f21-tz-inside", Timestamp: justInsideMonth.Format(time.RFC3339Nano), MachineID: "m-qtz", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(300)},
	})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 1200)

	var statuses []overviewQuotaStatus
	getJSONWithClient(t, client, s.BaseURL+"/api/v1/quotas", &statuses)
	if len(statuses) != 1 || statuses[0].TokensUsed != 300 {
		t.Fatalf("expected Toronto month boundary tokens_used=300, got %+v", statuses)
	}
	html := overviewWaitHTMLContains(t, client, s.BaseURL, "Toronto Monthly")
	quotaSec := overviewQuotaSection(t, html, "Toronto Monthly")
	if !strings.Contains(quotaSec, "300") {
		t.Fatalf("/ HTML quota bar missing 300 in quota section: %s", quotaSec)
	}
}

func TestOverview_F21_T2_MultipleQuotasAndRestartPersistence(t *testing.T) {
	requireE2E(t)
	quotasYAML := `schema_version: "1"
quotas:
  - label: "OpenAI Budget"
    provider: openai
    reset_day: 1
    budget:
      tokens_per_month: 10000
  - label: "Anthropic Budget"
    provider: anthropic
    reset_day: 1
    budget:
      tokens_per_month: 20000
`
	s1 := startTestServer(t, serverOpts{IngestToken: "ov-f21-t2-5", QuotasYAML: quotasYAML})
	s1.ingestEvents(t, []ingestEvent{
		{EventID: "ov-f21-r1", MachineID: "m-qr", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(1000)},
		{EventID: "ov-f21-r2", MachineID: "m-qr", Provider: "anthropic", Model: "claude-sonnet-4", Tool: "claude-code", TotalTokens: i64(2000)},
	})
	c1 := s1.authClient(t)
	waitOverviewLifetimeTokens(t, c1, s1.BaseURL, 3000)
	dbPath := s1.DBPath
	s1.Stop()

	s2 := startTestServer(t, serverOpts{IngestToken: "ov-f21-t2-5", DBPath: dbPath, QuotasYAML: quotasYAML})
	c2 := s2.authClient(t)
	html := overviewGetHTML(t, c2, s2.BaseURL+"/")
	if !strings.Contains(html, "OpenAI Budget") || !strings.Contains(html, "Anthropic Budget") {
		t.Fatalf("/ HTML after restart missing multiple quota bars")
	}
}

// =============================================================================
// Area 8: Feature 23 (Streaks) — CurrentStreak, LongestStreak, PeakDay (M5)
// =============================================================================

func TestOverview_F23_T1_ConsecutiveStreakEndingToday(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "ov-f23-t1-1", Timezone: "UTC"})
	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.UTC)
	d1 := today.AddDate(0, 0, -1)
	d2 := today.AddDate(0, 0, -2)

	s.ingestEvents(t, []ingestEvent{
		{EventID: "ov-f23-1a", Timestamp: d2.Format(time.RFC3339Nano), MachineID: "m-str", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(1000)},
		{EventID: "ov-f23-1b", Timestamp: d1.Format(time.RFC3339Nano), MachineID: "m-str", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(5000)},
		{EventID: "ov-f23-1c", Timestamp: today.Add(-1 * time.Hour).Format(time.RFC3339Nano), MachineID: "m-str", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(2000)},
	})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 8000)

	var ov overviewPayload
	getJSONWithClient(t, client, s.BaseURL+"/api/v1/analytics/overview", &ov)
	if ov.Activity.CurrentStreak != 3 || ov.Activity.LongestStreak != 3 || ov.Activity.MaxDailyTokens != 5000 {
		t.Fatalf("unexpected streak metrics: %+v", ov.Activity)
	}
	wantPeak := d1.Format("2006-01-02")
	if got := overviewParsePeakDayDate(ov.Activity.PeakDay); got != wantPeak {
		t.Fatalf("peak_day = %q (raw=%s), want %q", got, string(ov.Activity.PeakDay), wantPeak)
	}

	html := overviewWaitHTMLContains(t, client, s.BaseURL, "8,000")
	actPanel := overviewStripActivityLegend(overviewSectionBetween(t, html, `<section class="panel activity-panel"`, `<div class="activity-shell">`))
	if !overviewHasStreakValue(actPanel, 3) || (!strings.Contains(actPanel, "5,000") && !strings.Contains(actPanel, "5K")) {
		t.Fatalf("activity-panel header missing streak (3) or peak (5,000 / 5K): %s", actPanel)
	}
}

func TestOverview_F23_T1_BrokenStreakDistinguishesCurrentFromLongest(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "ov-f23-t1-2", Timezone: "UTC"})
	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.UTC)

	var events []ingestEvent
	for offset := -10; offset <= -7; offset++ {
		events = append(events, ingestEvent{
			EventID:     fmt.Sprintf("ov-f23-brk-%d", offset),
			Timestamp:   today.AddDate(0, 0, offset).Format(time.RFC3339Nano),
			MachineID:   "m-brk",
			Provider:    "openai",
			Model:       "gpt-5.4",
			Tool:        "codex",
			TotalTokens: i64(200),
		})
	}
	events = append(events, ingestEvent{
		EventID:     "ov-f23-brk-today",
		Timestamp:   today.Add(-30 * time.Minute).Format(time.RFC3339Nano),
		MachineID:   "m-brk",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(300),
	})
	s.ingestEvents(t, events)

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 1100)

	var ov overviewPayload
	getJSONWithClient(t, client, s.BaseURL+"/api/v1/analytics/overview", &ov)
	if ov.Activity.CurrentStreak != 1 || ov.Activity.LongestStreak != 4 {
		t.Fatalf("expected current_streak=1, longest_streak=4; got %+v", ov.Activity)
	}
	html := overviewWaitHTMLContains(t, client, s.BaseURL, "1,100")
	actHeader := overviewStripActivityLegend(overviewSectionBetween(t, html, `<section class="panel activity-panel"`, `<div class="activity-shell">`))
	if !overviewHasStreakValue(actHeader, 4) || !overviewHasStreakValue(actHeader, 1) {
		t.Fatalf("activity header should show both current (1) and longest (4) streaks: %s", actHeader)
	}
}

func TestOverview_F23_T1_PeakDayUpdatesWhenNewDayExceedsPreviousPeak(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "ov-f23-t1-3", Timezone: "UTC"})
	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.UTC)
	d3 := today.AddDate(0, 0, -3)

	s.ingestEvents(t, []ingestEvent{{
		EventID:     "ov-f23-pk-1",
		Timestamp:   d3.Format(time.RFC3339Nano),
		MachineID:   "m-pk",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(1000),
	}})
	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 1000)

	var ov1 overviewPayload
	getJSONWithClient(t, client, s.BaseURL+"/api/v1/analytics/overview", &ov1)
	if got := overviewParsePeakDayDate(ov1.Activity.PeakDay); got != d3.Format("2006-01-02") {
		t.Fatalf("initial peak_day = %q, want %q", got, d3.Format("2006-01-02"))
	}

	s.ingestEvents(t, []ingestEvent{{
		EventID:     "ov-f23-pk-2",
		Timestamp:   today.Add(-10 * time.Minute).Format(time.RFC3339Nano),
		MachineID:   "m-pk",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(2500),
	}})
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 3500)

	var ov2 overviewPayload
	getJSONWithClient(t, client, s.BaseURL+"/api/v1/analytics/overview", &ov2)
	if got := overviewParsePeakDayDate(ov2.Activity.PeakDay); got != today.Format("2006-01-02") {
		t.Fatalf("updated peak_day = %q, want %q", got, today.Format("2006-01-02"))
	}
	if ov2.Activity.MaxDailyTokens != 2500 {
		t.Fatalf("max_daily_tokens = %d, want 2500", ov2.Activity.MaxDailyTokens)
	}
}

func TestOverview_F23_T1_MultipleEventsSameDayCountAsOneStreakDay(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "ov-f23-t1-4", Timezone: "UTC"})
	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.UTC)
	yesterday := today.AddDate(0, 0, -1)

	var events []ingestEvent
	for i := 0; i < 10; i++ {
		events = append(events, ingestEvent{
			EventID:     fmt.Sprintf("ov-f23-same-t-%d", i),
			Timestamp:   today.Add(time.Duration(i) * time.Minute).Format(time.RFC3339Nano),
			MachineID:   "m-same",
			Provider:    "openai",
			Model:       []string{"gpt-5.4", "gpt-5-codex", "o3"}[i%3],
			Tool:        "codex",
			TotalTokens: i64(100),
		})
	}
	for i := 0; i < 5; i++ {
		events = append(events, ingestEvent{
			EventID:     fmt.Sprintf("ov-f23-same-y-%d", i),
			Timestamp:   yesterday.Add(time.Duration(i) * time.Minute).Format(time.RFC3339Nano),
			MachineID:   "m-same",
			Provider:    "openai",
			Model:       "gpt-5.4",
			Tool:        "codex",
			TotalTokens: i64(50),
		})
	}
	s.ingestEvents(t, events)

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 1250)

	var ov overviewPayload
	getJSONWithClient(t, client, s.BaseURL+"/api/v1/analytics/overview", &ov)
	if ov.Activity.ActiveDays != 2 || ov.Activity.CurrentStreak != 2 || ov.Activity.LongestStreak != 2 {
		t.Fatalf("expected active_days=2 current=2 longest=2, got %+v", ov.Activity)
	}
	if ov.Activity.MaxDailyTokens != 1000 {
		t.Fatalf("expected summed daily peak 1000, got %d", ov.Activity.MaxDailyTokens)
	}
}

func TestOverview_F23_T1_StreakMatchesActivityHeatmapActiveDaysGrid(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "ov-f23-t1-5", Timezone: "UTC"})
	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.UTC)

	offsets := []int{-20, -19, -18, -10, -9, -1, 0}
	events := make([]ingestEvent, 0, len(offsets))
	for i, off := range offsets {
		events = append(events, ingestEvent{
			EventID:     fmt.Sprintf("ov-f23-grid-%d", i),
			Timestamp:   today.AddDate(0, 0, off).Format(time.RFC3339Nano),
			MachineID:   "m-grid",
			Provider:    "openai",
			Model:       "gpt-5.4",
			Tool:        "codex",
			TotalTokens: i64(100),
		})
	}
	s.ingestEvents(t, events)

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 700)

	var ov overviewPayload
	getJSONWithClient(t, client, s.BaseURL+"/api/v1/analytics/overview", &ov)
	if ov.Activity.ActiveDays != 7 || ov.Activity.CurrentStreak != 2 || ov.Activity.LongestStreak != 3 {
		t.Fatalf("expected active=7 current=2 longest=3, got %+v", ov.Activity)
	}
	if !(ov.Activity.CurrentStreak <= ov.Activity.LongestStreak && ov.Activity.LongestStreak <= ov.Activity.ActiveDays) {
		t.Fatalf("streak bounds invariant violated: %+v", ov.Activity)
	}
}

func TestOverview_F23_T2_EmptyDatabaseZeroStreaksAndNoFakePeakDay(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "ov-f23-t2-1"})
	client := s.authClient(t)

	var ov overviewPayload
	getJSONWithClient(t, client, s.BaseURL+"/api/v1/analytics/overview", &ov)
	if ov.Activity.CurrentStreak != 0 || ov.Activity.LongestStreak != 0 || ov.Activity.ActiveDays != 0 {
		t.Fatalf("expected 0 streaks on empty DB, got %+v", ov.Activity)
	}
	if peak := overviewParsePeakDayDate(ov.Activity.PeakDay); peak != "" {
		t.Fatalf("empty DB fabricated peak_day %q", peak)
	}
	html := overviewGetHTML(t, client, s.BaseURL+"/")
	actPanel := overviewSectionBetween(t, html, `<section class="panel activity-panel"`, `</section>`)
	if !strings.Contains(actPanel, "No token activity in this field yet.") {
		t.Fatalf("expected empty field message in activity panel")
	}
	if strings.Contains(actPanel, "0d streak") || strings.Contains(actPanel, "Peak:") {
		t.Fatalf("empty activity panel must not fabricate streak/peak badge: %s", actPanel)
	}
}

func TestOverview_F23_T2_GapEndsCurrentStreakAtZero(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "ov-f23-t2-2", Timezone: "UTC"})
	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.UTC)

	var events []ingestEvent
	for off := -5; off <= -3; off++ {
		events = append(events, ingestEvent{
			EventID:     fmt.Sprintf("ov-f23-gap-%d", off),
			Timestamp:   today.AddDate(0, 0, off).Format(time.RFC3339Nano),
			MachineID:   "m-gap",
			Provider:    "openai",
			Model:       "gpt-5.4",
			Tool:        "codex",
			TotalTokens: i64(100),
		})
	}
	s.ingestEvents(t, events)

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 300)

	var ov overviewPayload
	getJSONWithClient(t, client, s.BaseURL+"/api/v1/analytics/overview", &ov)
	if ov.Activity.CurrentStreak != 0 || ov.Activity.LongestStreak != 3 {
		t.Fatalf("expected current_streak=0, longest_streak=3 after 2-day gap; got %+v", ov.Activity)
	}
	html := overviewWaitHTMLContains(t, client, s.BaseURL, "300")
	actHeader := overviewStripActivityLegend(overviewSectionBetween(t, html, `<section class="panel activity-panel"`, `<div class="activity-shell">`))
	if !overviewHasStreakValue(actHeader, 3) {
		t.Fatalf("expected longest streak 3 in activity header: %s", actHeader)
	}
}

func TestOverview_F23_T2_UnknownTokenEventsStillCountAsActiveStreakDay(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "ov-f23-t2-3", Timezone: "UTC"})
	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.UTC)
	yesterday := today.AddDate(0, 0, -1)

	s.ingestEvents(t, []ingestEvent{
		{EventID: "ov-f23-unk-1", Timestamp: yesterday.Format(time.RFC3339Nano), MachineID: "m-unk-s", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: nil, TokenAccuracy: "unknown"},
		{EventID: "ov-f23-unk-2", Timestamp: today.Add(-10 * time.Minute).Format(time.RFC3339Nano), MachineID: "m-unk-s", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: nil, TokenAccuracy: "unknown"},
	})

	client := s.authClient(t)
	var ov overviewPayload
	deadline := time.Now().Add(4 * time.Second)
	for {
		getJSONWithClient(t, client, s.BaseURL+"/api/v1/analytics/overview", &ov)
		if ov.Activity.ActiveDays == 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for active_days == 2 on unknown token events; got %+v", ov.Activity)
		}
		time.Sleep(25 * time.Millisecond)
	}
	if ov.Activity.CurrentStreak != 2 || ov.Activity.LongestStreak != 2 {
		t.Fatalf("expected unknown-token events to count as 2 streak days; got %+v", ov.Activity)
	}
	if ov.Activity.MaxDailyTokens != 0 {
		t.Fatalf("expected max_daily_tokens=0 for unknown-only events, got %d", ov.Activity.MaxDailyTokens)
	}
}

func TestOverview_F23_T2_TimezoneMidnightShiftsStreakAndPeakDay(t *testing.T) {
	requireE2E(t)
	loc := overviewLoadLoc(t, "America/New_York")
	s := startTestServer(t, serverOpts{IngestToken: "ov-f23-t2-4", Timezone: "America/New_York"})

	nowNY := time.Now().In(loc)
	// Anchor 2 days back so both events are safely in the past at any hour of day:
	// 21:00 NY on day D-2 and 00:30 NY on day D-1 share the exact same UTC date (02:00Z / 05:30Z)
	// when NY is EST (or 01:00Z / 04:30Z when EDT), yet span 2 consecutive NY calendar days.
	dayMinus2 := time.Date(nowNY.Year(), nowNY.Month(), nowNY.Day(), 21, 0, 0, 0, loc).AddDate(0, 0, -2)
	dayMinus1 := time.Date(nowNY.Year(), nowNY.Month(), nowNY.Day(), 0, 30, 0, 0, loc).AddDate(0, 0, -1)
	if dayMinus2.UTC().Format("2006-01-02") != dayMinus1.UTC().Format("2006-01-02") {
		t.Fatalf("test setup error: expected same UTC day, got %s vs %s", dayMinus2.UTC().Format("2006-01-02"), dayMinus1.UTC().Format("2006-01-02"))
	}

	s.ingestEvents(t, []ingestEvent{
		{EventID: "ov-f23-tz-1", Timestamp: dayMinus2.UTC().Format(time.RFC3339Nano), MachineID: "m-ny-s", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(700)},
		{EventID: "ov-f23-tz-2", Timestamp: dayMinus1.UTC().Format(time.RFC3339Nano), MachineID: "m-ny-s", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(300)},
	})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 1000)

	var ov overviewPayload
	getJSONWithClient(t, client, s.BaseURL+"/api/v1/analytics/overview", &ov)
	if ov.Activity.ActiveDays != 2 || ov.Activity.LongestStreak != 2 {
		t.Fatalf("expected 2 NY days / longest_streak=2, got %+v", ov.Activity)
	}
	wantPeak := dayMinus2.In(loc).Format("2006-01-02")
	if got := overviewParsePeakDayDate(ov.Activity.PeakDay); got != wantPeak {
		t.Fatalf("NY peak_day = %q, want %q", got, wantPeak)
	}
}

func TestOverview_F23_T2_ActivityHeatmapWindow53WeekBoundary(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "ov-f23-t2-5", Timezone: "UTC"})
	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.UTC)
	outside53w := today.AddDate(0, 0, -400)

	s.ingestEvents(t, []ingestEvent{
		{EventID: "ov-f23-old400", Timestamp: outside53w.Format(time.RFC3339Nano), MachineID: "m-53w", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(99000)},
		{EventID: "ov-f23-inwin", Timestamp: today.Add(-1 * time.Hour).Format(time.RFC3339Nano), MachineID: "m-53w", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(400)},
	})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 99400)

	var ov overviewPayload
	getJSONWithClient(t, client, s.BaseURL+"/api/v1/analytics/overview", &ov)
	if ov.Activity.TotalTokens != 400 || ov.Activity.MaxDailyTokens != 400 || ov.Activity.ActiveDays != 1 || ov.Activity.CurrentStreak != 1 {
		t.Fatalf("53-week activity leaked >371d event: %+v", ov.Activity)
	}
	wantPeak := today.Format("2006-01-02")
	if got := overviewParsePeakDayDate(ov.Activity.PeakDay); got != wantPeak {
		t.Fatalf("peak_day = %q, want %q (not 400d-old event)", got, wantPeak)
	}
}
