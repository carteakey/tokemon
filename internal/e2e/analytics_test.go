//go:build e2e

package e2e_test

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	stdhtml "html"
	"io"
	"math"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"
)

type analyticsCostSummary struct {
	Amount         float64 `json:"amount"`
	PricedTokens   int64   `json:"priced_tokens"`
	UnpricedTokens int64   `json:"unpriced_tokens"`
}

type analyticsCacheSummary struct {
	HitRate        float64 `json:"hit_rate"`
	CachedTokens   int64   `json:"cached_tokens"`
	EligibleTokens int64   `json:"eligible_tokens"`
}

type analyticsSummaryPayload struct {
	Tokens           int64                 `json:"tokens"`
	Events           int64                 `json:"events"`
	ActiveDays       int64                 `json:"active_days"`
	AverageActiveDay float64               `json:"average_active_day"`
	UnknownEvents    int64                 `json:"unknown_events"`
	EstimatedCost    analyticsCostSummary  `json:"estimated_cost"`
	Cache            analyticsCacheSummary `json:"cache"`
	SessionTokens    int64                 `json:"session_tokens"`
	Threads          int64                 `json:"threads"`
}

type analyticsComparisonPayload struct {
	PreviousTokens       int64    `json:"previous_tokens"`
	PreviousEvents       int64    `json:"previous_events"`
	TokenChangePercent   *float64 `json:"token_change_percent"`
	PreviousCacheHitRate *float64 `json:"previous_cache_hit_rate"`
}

type analyticsPointPayload struct {
	Date          string `json:"date"`
	Label         string `json:"label"`
	Tokens        int64  `json:"tokens"`
	Events        int64  `json:"events"`
	UnknownEvents int64  `json:"unknown_events"`
	InputTokens   int64  `json:"input_tokens"`
	CachedTokens  int64  `json:"cached_tokens"`
	OutputTokens  int64  `json:"output_tokens"`
}

type analyticsBreakdownPayload struct {
	Name     string  `json:"name"`
	Tokens   int64   `json:"tokens"`
	Events   int64   `json:"events"`
	Sessions int64   `json:"sessions"`
	Share    float64 `json:"share"`
}

type analyticsShareSeriesPayload struct {
	Name   string  `json:"name"`
	Tokens int64   `json:"tokens"`
	Share  float64 `json:"share"`
}

type analyticsSharePointPayload struct {
	Date          string                        `json:"date"`
	Label         string                        `json:"label"`
	Tokens        int64                         `json:"tokens"`
	UnknownEvents int64                         `json:"unknown_events"`
	Values        []analyticsShareSeriesPayload `json:"values"`
}

type analyticsFilterPayload struct {
	Period    string `json:"period"`
	Dimension string `json:"dimension"`
	Machine   string `json:"machine"`
	Provider  string `json:"provider"`
	Model     string `json:"model"`
	Tool      string `json:"tool"`
	From      string `json:"from"`
	To        string `json:"to"`
}

type analyticsAPIPayload struct {
	LifetimeTokens int64                         `json:"lifetime_tokens"`
	Timezone       string                        `json:"timezone"`
	Filter         analyticsFilterPayload        `json:"filter"`
	StartDate      string                        `json:"start_date"`
	EndDate        string                        `json:"end_date"`
	Bucket         string                        `json:"bucket"`
	Summary        analyticsSummaryPayload       `json:"summary"`
	Comparison     *analyticsComparisonPayload   `json:"comparison"`
	Points         []analyticsPointPayload       `json:"points"`
	MaxTokens      int64                         `json:"max_tokens"`
	Breakdown      []analyticsBreakdownPayload   `json:"breakdown"`
	ShareSeries    []analyticsShareSeriesPayload `json:"share_series"`
	SharePoints    []analyticsSharePointPayload  `json:"share_points"`
	Sessions       []json.RawMessage             `json:"sessions"`
	Facets         analyticsFacetsPayload        `json:"facets"`
}

type analyticsTimelinePayload struct {
	Filter    analyticsFilterPayload  `json:"filter"`
	StartDate string                  `json:"start_date"`
	EndDate   string                  `json:"end_date"`
	Bucket    string                  `json:"bucket"`
	Points    []analyticsPointPayload `json:"points"`
}

func analyticsGetResp(t *testing.T, client *http.Client, fullURL string, headers map[string]string) (int, http.Header, []byte) {
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
		t.Fatalf("read GET %s: %v", fullURL, readErr)
	}
	if closeErr != nil {
		t.Fatalf("close GET %s: %v", fullURL, closeErr)
	}
	return resp.StatusCode, resp.Header, body
}

func analyticsGetHTML(t *testing.T, client *http.Client, fullURL string) string {
	t.Helper()
	code, _, body := analyticsGetResp(t, client, fullURL, map[string]string{"Accept": "text/html"})
	if code != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200; body=%s", fullURL, code, string(body))
	}
	return string(body)
}

func analyticsPostAliases(t *testing.T, s *testServer, client *http.Client, formValues url.Values) (int, []byte) {
	t.Helper()
	settingsHTML := analyticsGetHTML(t, client, s.BaseURL+"/settings")
	if !formValues.Has("csrf_token") {
		formValues.Set("csrf_token", extractCSRFToken(t, []byte(settingsHTML)))
	}
	req, err := http.NewRequest(http.MethodPost, s.BaseURL+"/settings/aliases", strings.NewReader(formValues.Encode()))
	if err != nil {
		t.Fatalf("new POST /settings/aliases: %v", err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("Origin", s.BaseURL)
	req.Header.Set("Referer", s.BaseURL+"/settings")
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("POST /settings/aliases: %v", err)
	}
	body, readErr := io.ReadAll(resp.Body)
	closeErr := resp.Body.Close()
	if readErr != nil {
		t.Fatalf("read POST /settings/aliases: %v", readErr)
	}
	if closeErr != nil {
		t.Fatalf("close POST /settings/aliases: %v", closeErr)
	}
	return resp.StatusCode, body
}

func analyticsWaitTokens(t *testing.T, client *http.Client, fullURL string, wantTokens int64) analyticsAPIPayload {
	t.Helper()
	deadline := time.Now().Add(4 * time.Second)
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
		time.Sleep(50 * time.Millisecond)
	}
}

func analyticsSectionBetween(t *testing.T, html, startMarker, endMarker string) string {
	t.Helper()
	start := strings.Index(html, startMarker)
	if start < 0 {
		t.Fatalf("analytics section marker %q not found", startMarker)
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

func analyticsLoadLoc(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatalf("LoadLocation(%q): %v", name, err)
	}
	return loc
}

var analyticsFormRe = regexp.MustCompile(`(?is)<form\b[^>]*>.*?</form>`)

func analyticsStripForms(html string) string {
	return analyticsFormRe.ReplaceAllString(html, " ")
}

func analyticsExtractHrefs(fragment string) []string {
	re := regexp.MustCompile(`href="([^"]+)"`)
	matches := re.FindAllStringSubmatch(fragment, -1)
	out := make([]string, 0, len(matches))
	for _, m := range matches {
		if len(m) >= 2 {
			out = append(out, stdhtml.UnescapeString(m[1]))
		}
	}
	return out
}

// =============================================================================
// Area 1: Feature 2 — Analytics MachineAliases (Breakdown & Share) (LANDED)
// =============================================================================

func TestAnalytics_F2_T1_MachineAliasesInBreakdownAndShareLegend(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f2-t1-1"})
	s.ingestEvents(t, []ingestEvent{
		{EventID: "an-f2-1a", MachineID: "raw-host-a", SessionID: "s-1", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(600)},
		{EventID: "an-f2-1b", MachineID: "raw-host-b", SessionID: "s-2", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(400)},
	})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 1000)

	form := url.Values{
		"machine_identity": {"raw-host-a", "raw-host-b"},
		"machine_alias":    {"Mac Studio", "Linux Rig"},
	}
	code, _ := analyticsPostAliases(t, s, client, form)
	if code != http.StatusOK && code != http.StatusSeeOther {
		t.Fatalf("POST /settings/aliases status = %d", code)
	}

	html := analyticsGetHTML(t, client, s.BaseURL+"/analytics?period=30d&dimension=machines")
	for _, want := range []string{
		`<span class="breakdown-name">Mac Studio</span>`,
		`<span class="breakdown-name">Linux Rig</span>`,
		`<strong>Mac Studio</strong>`,
		`<strong>Linux Rig</strong>`,
		`data-name="Mac Studio"`,
		`data-name="Linux Rig"`,
		`<th scope="col">Mac Studio share</th>`,
		`<th scope="col">Linux Rig share</th>`,
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("/analytics?dimension=machines missing %q", want)
		}
	}
}

func TestAnalytics_F2_T1_MachineAliasesDoNotCollideWithModelAliases(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f2-t1-2"})
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "an-f2-2a",
		MachineID:   "raw-box-id",
		SessionID:   "s-col",
		Provider:    "openai",
		Model:       "raw-box-id",
		Tool:        "codex",
		TotalTokens: i64(500),
	}})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 500)

	form := url.Values{
		"model_identity":   {"raw-box-id"},
		"model_alias":      {"WRONG-MODEL-ALIAS"},
		"machine_identity": {"raw-box-id"},
		"machine_alias":    {"RIGHT-MACHINE-ALIAS"},
	}
	code, _ := analyticsPostAliases(t, s, client, form)
	if code != http.StatusOK && code != http.StatusSeeOther {
		t.Fatalf("POST /settings/aliases status = %d", code)
	}

	html := analyticsGetHTML(t, client, s.BaseURL+"/analytics?period=30d&dimension=machines")
	breakdown := analyticsSectionBetween(t, html, `class="panel breakdown-panel"`, `class="panel share-panel"`)
	share := analyticsSectionBetween(t, html, `class="panel share-panel"`, `class="panel sessions-panel"`)
	for _, seg := range []string{breakdown, share} {
		if !strings.Contains(seg, "RIGHT-MACHINE-ALIAS") {
			t.Fatalf("expected RIGHT-MACHINE-ALIAS in section; got %s", seg)
		}
		if strings.Contains(seg, "WRONG-MODEL-ALIAS") {
			t.Fatalf("machine dimension leaked WRONG-MODEL-ALIAS in %s", seg)
		}
	}
}

func TestAnalytics_F2_T1_MachineAliasesInTopMoverInsight(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f2-t1-3", Timezone: "UTC"})
	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.UTC)

	// Over period=7d (7 buckets, mid=3): host-1 holds day -6 (first half avg 33%),
	// host-2 holds days -2, -1, 0 (second half avg 75%) -> host-2 is Top mover (+75.0 pts).
	s.ingestEvents(t, []ingestEvent{
		{EventID: "an-f2-tm-1", Timestamp: today.AddDate(0, 0, -6).Format(time.RFC3339Nano), MachineID: "host-mv-1", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(100)},
		{EventID: "an-f2-tm-2", Timestamp: today.AddDate(0, 0, -2).Format(time.RFC3339Nano), MachineID: "host-mv-2", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(100)},
		{EventID: "an-f2-tm-3", Timestamp: today.AddDate(0, 0, -1).Format(time.RFC3339Nano), MachineID: "host-mv-2", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(100)},
		{EventID: "an-f2-tm-4", Timestamp: today.Add(-1 * time.Hour).Format(time.RFC3339Nano), MachineID: "host-mv-2", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(100)},
	})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 400)

	code, _ := analyticsPostAliases(t, s, client, url.Values{
		"machine_identity": {"host-mv-2"},
		"machine_alias":    {"Build Mini"},
	})
	if code != http.StatusOK && code != http.StatusSeeOther {
		t.Fatalf("POST /settings/aliases status = %d", code)
	}

	html := analyticsGetHTML(t, client, s.BaseURL+"/analytics?period=7d&dimension=machines")
	insights := analyticsSectionBetween(t, html, `class="insights-strip"`, `</section>`)
	if !strings.Contains(insights, "Top mover") || !strings.Contains(insights, "Build Mini held") {
		t.Fatalf("expected Top mover insight to display machine alias 'Build Mini'; got %s", insights)
	}
}

func TestAnalytics_F2_T1_MachineAliasesAppliedInSessionContextAndFilterSelect(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f2-t1-4"})
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "an-f2-4a",
		MachineID:   "raw-host-ctx",
		SessionID:   "sess-ctx-1",
		Project:     "proj-ctx",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(250),
	}})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 250)

	code, _ := analyticsPostAliases(t, s, client, url.Values{
		"machine_identity": {"raw-host-ctx"},
		"machine_alias":    {"Studio Rack"},
		"model_identity":   {"gpt-5.4"},
		"model_alias":      {"GPT Five Four"},
	})
	if code != http.StatusOK && code != http.StatusSeeOther {
		t.Fatalf("POST /settings/aliases status = %d", code)
	}

	html := analyticsGetHTML(t, client, s.BaseURL+"/analytics?period=30d&dimension=models")
	if !strings.Contains(html, `<option value="raw-host-ctx">Studio Rack</option>`) {
		t.Fatalf("expected machine filter option with Studio Rack label")
	}
	sessions := analyticsSectionBetween(t, html, `id="sessions-title"`, `</section>`)
	if !strings.Contains(sessions, "Studio Rack") || !strings.Contains(sessions, "GPT Five Four") {
		t.Fatalf("expected machine and model aliases in Recent sessions; got %s", sessions)
	}
}

func TestAnalytics_F2_T1_MachineAliasUpdateAndClearImmediatelyReflected(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f2-t1-5"})
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "an-f2-5a",
		MachineID:   "host-alpha-one",
		SessionID:   "s-upd",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(300),
	}})

	client := s.authClient(t)
	analyticsWaitTokens(t, client, s.BaseURL+"/api/v1/analytics?period=30d", 300)

	analyticsPostAliases(t, s, client, url.Values{"machine_identity": {"host-alpha-one"}, "machine_alias": {"Old Box"}})
	html1 := analyticsGetHTML(t, client, s.BaseURL+"/analytics?period=30d&dimension=machines")
	if !strings.Contains(html1, `<span class="breakdown-name">Old Box</span>`) {
		t.Fatalf("expected Old Box alias immediately on /analytics")
	}

	analyticsPostAliases(t, s, client, url.Values{"machine_identity": {"host-alpha-one"}, "machine_alias": {"New Box"}})
	html2 := analyticsGetHTML(t, client, s.BaseURL+"/analytics?period=30d&dimension=machines")
	if !strings.Contains(html2, `<span class="breakdown-name">New Box</span>`) || strings.Contains(html2, "Old Box") {
		t.Fatalf("expected immediate update to New Box")
	}

	analyticsPostAliases(t, s, client, url.Values{"machine_identity": {"host-alpha-one"}, "machine_alias": {""}})
	html3 := analyticsGetHTML(t, client, s.BaseURL+"/analytics?period=30d&dimension=machines")
	if !strings.Contains(html3, `<span class="breakdown-name">host-alpha-one</span>`) || strings.Contains(html3, "New Box") {
		t.Fatalf("expected blank alias to clear override back to host-alpha-one")
	}
}

func TestAnalytics_F2_T2_PreservesRawIDsInJSONAndCSVExports(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f2-t2-1"})
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "an-f2-raw-1",
		MachineID:   "raw-host-export",
		SessionID:   "s-raw",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(500),
	}})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 500)
	analyticsPostAliases(t, s, client, url.Values{"machine_identity": {"raw-host-export"}, "machine_alias": {"Pretty Alias"}})

	var api analyticsAPIPayload
	getJSONWithClient(t, client, s.BaseURL+"/api/v1/analytics?period=30d&dimension=machines", &api)
	if len(api.Breakdown) != 1 || api.Breakdown[0].Name != "raw-host-export" {
		t.Fatalf("API breakdown did not preserve raw machine ID: %+v", api.Breakdown)
	}
	if len(api.ShareSeries) != 1 || api.ShareSeries[0].Name != "raw-host-export" {
		t.Fatalf("API share_series did not preserve raw machine ID: %+v", api.ShareSeries)
	}

	code, _, csvBody := analyticsGetResp(t, client, s.BaseURL+"/api/v1/analytics/export?period=30d&dimension=machines&format=csv", nil)
	if code != http.StatusOK {
		t.Fatalf("CSV export status = %d", code)
	}
	records, err := csv.NewReader(bytes.NewReader(csvBody)).ReadAll()
	if err != nil {
		t.Fatalf("parse CSV: %v", err)
	}
	if len(records) != 2 || records[1][2] != "raw-host-export" {
		t.Fatalf("CSV export did not preserve raw machine ID: %+v", records)
	}
	if strings.Contains(string(csvBody), "Pretty Alias") {
		t.Fatalf("CSV export leaked display alias Pretty Alias")
	}
}

func TestAnalytics_F2_T2_UnknownMachineBucketLabel(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f2-t2-2"})
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "an-f2-unk-m",
		MachineID:   "unknown",
		SessionID:   "s-unkm",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(100),
	}})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 100)

	html := analyticsGetHTML(t, client, s.BaseURL+"/analytics?period=30d&dimension=machines")
	if !strings.Contains(html, `<span class="unknown-value">Unknown machine</span>`) {
		t.Fatalf("expected Unknown machine label for 'unknown' machine bucket")
	}
}

func TestAnalytics_F2_T2_SixMachinesOtherGroupingWithAliases(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f2-t2-3"})

	var events []ingestEvent
	identities := make([]string, 0, 6)
	aliases := make([]string, 0, 6)
	for i := 1; i <= 6; i++ {
		id := fmt.Sprintf("m-grp-%d", i)
		identities = append(identities, id)
		aliases = append(aliases, fmt.Sprintf("Alias%d", i))
		events = append(events, ingestEvent{
			EventID:     fmt.Sprintf("an-f2-grp-%d", i),
			MachineID:   id,
			SessionID:   fmt.Sprintf("s-grp-%d", i),
			Provider:    "openai",
			Model:       "gpt-5.4",
			Tool:        "codex",
			TotalTokens: i64(int64(7-i) * 100),
		})
	}
	s.ingestEvents(t, events)
	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 2100)

	analyticsPostAliases(t, s, client, url.Values{
		"machine_identity": identities,
		"machine_alias":    aliases,
	})

	html := analyticsGetHTML(t, client, s.BaseURL+"/analytics?period=30d&dimension=machines")
	legend := analyticsSectionBetween(t, html, `class="share-legend"`, `class="share-visual"`)
	for i := 1; i <= 5; i++ {
		if !strings.Contains(legend, fmt.Sprintf("<strong>Alias%d</strong>", i)) {
			t.Fatalf("legend missing top-5 Alias%d: %s", i, legend)
		}
	}
	if !strings.Contains(legend, `<strong>Other</strong>`) {
		t.Fatalf("legend missing Other for 6th machine: %s", legend)
	}
	if strings.Contains(legend, "Alias6") {
		t.Fatalf("legend should fold Alias6 into Other: %s", legend)
	}
	breakdown := analyticsSectionBetween(t, html, `class="panel breakdown-panel"`, `class="panel share-panel"`)
	if !strings.Contains(breakdown, "Alias6") {
		t.Fatalf("breakdown table should still include Alias6")
	}
}

func TestAnalytics_F2_T2_MachineAliasesHTMLEscapedAgainstXSS(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f2-t2-4"})
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "an-f2-xss",
		MachineID:   "m-xss-box",
		SessionID:   "s-xss",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(200),
	}})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 200)

	xss := `Box <script>alert(1)</script>`
	code, _ := analyticsPostAliases(t, s, client, url.Values{
		"machine_identity": {"m-xss-box"},
		"machine_alias":    {xss},
	})
	if code != http.StatusOK && code != http.StatusSeeOther {
		t.Fatalf("POST /settings/aliases status = %d", code)
	}

	html := analyticsGetHTML(t, client, s.BaseURL+"/analytics?period=30d&dimension=machines")
	if strings.Contains(html, "<script>alert(1)</script>") {
		t.Fatalf("unescaped XSS payload rendered in /analytics HTML")
	}
	if !strings.Contains(html, "&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Fatalf("expected HTML-escaped script tag in /analytics")
	}
}

func TestAnalytics_F2_T2_SettingsPostAuthAndCSRFRejected(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f2-t2-5-ing", Password: "an-pass-123"})
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "an-f2-csrf-1",
		MachineID:   "m-csrf-box",
		SessionID:   "s-csrf",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(100),
	}})

	// Unauthenticated POST rejected.
	unauthForm := url.Values{"machine_identity": {"m-csrf-box"}, "machine_alias": {"Hack"}}
	unauthResp, _ := s.doReq(t, http.MethodPost, "/settings/aliases", []byte(unauthForm.Encode()), map[string]string{
		"Content-Type": "application/x-www-form-urlencoded",
	})
	if unauthResp.StatusCode != http.StatusUnauthorized && unauthResp.StatusCode != http.StatusSeeOther {
		t.Fatalf("unauthenticated POST /settings/aliases status = %d", unauthResp.StatusCode)
	}

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 100)

	// Missing CSRF token -> 403.
	reqBadCSRF, err := http.NewRequest(http.MethodPost, s.BaseURL+"/settings/aliases", strings.NewReader(unauthForm.Encode()))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	reqBadCSRF.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	reqBadCSRF.Header.Set("Origin", s.BaseURL)
	respBadCSRF, err := client.Do(reqBadCSRF)
	if err != nil {
		t.Fatalf("do bad csrf: %v", err)
	}
	if err := respBadCSRF.Body.Close(); err != nil {
		t.Fatalf("close bad csrf body: %v", err)
	}
	if respBadCSRF.StatusCode != http.StatusForbidden {
		t.Fatalf("missing CSRF POST /settings/aliases status = %d, want 403", respBadCSRF.StatusCode)
	}

	// Cross-origin Origin -> 403 even with valid CSRF token.
	settingsHTML := analyticsGetHTML(t, client, s.BaseURL+"/settings")
	csrf := extractCSRFToken(t, []byte(settingsHTML))
	evilForm := url.Values{
		"csrf_token":       {csrf},
		"machine_identity": {"m-csrf-box"},
		"machine_alias":    {"EvilOrigin"},
	}
	reqEvil, err := http.NewRequest(http.MethodPost, s.BaseURL+"/settings/aliases", strings.NewReader(evilForm.Encode()))
	if err != nil {
		t.Fatalf("new evil request: %v", err)
	}
	reqEvil.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	reqEvil.Header.Set("Origin", "https://evil.example")
	respEvil, err := client.Do(reqEvil)
	if err != nil {
		t.Fatalf("do evil origin: %v", err)
	}
	if err := respEvil.Body.Close(); err != nil {
		t.Fatalf("close evil body: %v", err)
	}
	if respEvil.StatusCode != http.StatusForbidden {
		t.Fatalf("cross-origin POST /settings/aliases status = %d, want 403", respEvil.StatusCode)
	}

	html := analyticsGetHTML(t, client, s.BaseURL+"/analytics?period=30d&dimension=machines")
	if strings.Contains(html, "EvilOrigin") || strings.Contains(html, "Hack") {
		t.Fatalf("rejected alias mutation leaked to /analytics")
	}
}

// =============================================================================
// Area 2: Feature 3 (Analytics) — Localized <time datetime="..."> (LANDED)
// =============================================================================

func TestAnalytics_F3_T1_RecentSessionsLocalizedTimeAmericaToronto(t *testing.T) {
	requireE2E(t)
	loc := analyticsLoadLoc(t, "America/Toronto")
	s := startTestServer(t, serverOpts{IngestToken: "an-f3-t1-1", Timezone: "America/Toronto"})

	ts := time.Date(2026, 11, 20, 3, 30, 0, 0, time.UTC) // 2026-11-19 22:30:00 EST
	local := ts.In(loc)
	wantDT := local.Format(time.RFC3339)

	s.ingestEvents(t, []ingestEvent{{
		EventID:     "an-f3-tor",
		Timestamp:   ts.Format(time.RFC3339Nano),
		MachineID:   "m-tor",
		SessionID:   "sess-tor-1",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(100),
	}})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 100)
	html := analyticsGetHTML(t, client, s.BaseURL+"/analytics?period=all")
	sessions := analyticsSectionBetween(t, html, `id="sessions-title"`, `</section>`)
	if !strings.Contains(sessions, fmt.Sprintf(`datetime="%s"`, wantDT)) {
		t.Fatalf("expected datetime=%q in /analytics Recent sessions; got %s", wantDT, sessions)
	}
	if !strings.Contains(sessions, `title="2026-11-19 22:30:00 EST">Nov 19, 22:30 ·`) {
		t.Fatalf("expected localized title and Nov 19, 22:30 display in %s", sessions)
	}
}

func TestAnalytics_F3_T1_RecentSessionsLocalizedTimeAsiaTokyo(t *testing.T) {
	requireE2E(t)
	loc := analyticsLoadLoc(t, "Asia/Tokyo")
	s := startTestServer(t, serverOpts{IngestToken: "an-f3-t1-2", Timezone: "Asia/Tokyo"})

	ts := time.Date(2026, 11, 19, 20, 15, 0, 0, time.UTC) // 2026-11-20 05:15:00 JST
	wantDT := ts.In(loc).Format(time.RFC3339)

	s.ingestEvents(t, []ingestEvent{{
		EventID:     "an-f3-tyo",
		Timestamp:   ts.Format(time.RFC3339Nano),
		MachineID:   "m-tyo",
		SessionID:   "sess-tyo-1",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(100),
	}})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 100)
	html := analyticsGetHTML(t, client, s.BaseURL+"/analytics?period=all")
	sessions := analyticsSectionBetween(t, html, `id="sessions-title"`, `</section>`)
	if !strings.Contains(sessions, fmt.Sprintf(`datetime="%s"`, wantDT)) || !strings.Contains(sessions, "Nov 20, 05:15 ·") {
		t.Fatalf("expected JST next-day Nov 20, 05:15; got %s", sessions)
	}
}

func TestAnalytics_F3_T1_RecentSessionsRelativeAgeBuckets(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f3-t1-3", Timezone: "UTC"})
	now := time.Now().UTC()

	buckets := []struct {
		id  string
		ts  time.Time
		age string
	}{
		{"s-b-now", now.Add(-15 * time.Second), "· just now</time>"},
		{"s-b-15m", now.Add(-15*time.Minute - 10*time.Second), "· 15m ago</time>"},
		{"s-b-5h", now.Add(-5*time.Hour - 2*time.Minute), "· 5h ago</time>"},
		{"s-b-3d", now.Add(-3*24*time.Hour - 2*time.Hour), "· 3d ago</time>"},
	}
	events := make([]ingestEvent, 0, len(buckets))
	for i, b := range buckets {
		events = append(events, ingestEvent{
			EventID:     fmt.Sprintf("an-f3-age-%d", i),
			Timestamp:   b.ts.Format(time.RFC3339Nano),
			MachineID:   "m-age",
			SessionID:   b.id,
			Provider:    "openai",
			Model:       "gpt-5.4",
			Tool:        "codex",
			TotalTokens: i64(10),
		})
	}
	s.ingestEvents(t, events)

	client := s.authClient(t)
	analyticsWaitTokens(t, client, s.BaseURL+"/api/v1/analytics?period=30d", 40)
	html := analyticsGetHTML(t, client, s.BaseURL+"/analytics?period=30d")
	sessions := analyticsSectionBetween(t, html, `id="sessions-title"`, `</section>`)
	for _, b := range buckets {
		if !strings.Contains(sessions, b.age) {
			t.Fatalf("/analytics Recent sessions missing relative age %q", b.age)
		}
	}
}

func TestAnalytics_F3_T1_RecentSessionsDSTTransitionSpringForward(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f3-t1-4", Timezone: "America/New_York"})

	s.ingestEvents(t, []ingestEvent{
		{EventID: "an-f3-dst-1", Timestamp: "2026-03-08T06:30:00Z", MachineID: "m-dst", SessionID: "s-dst-1", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(10)},
		{EventID: "an-f3-dst-2", Timestamp: "2026-03-08T07:30:00Z", MachineID: "m-dst", SessionID: "s-dst-2", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(10)},
	})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 20)
	html := analyticsGetHTML(t, client, s.BaseURL+"/analytics?period=all")
	sessions := analyticsSectionBetween(t, html, `id="sessions-title"`, `</section>`)
	if !strings.Contains(sessions, `datetime="2026-03-08T01:30:00-05:00"`) || !strings.Contains(sessions, "EST") {
		t.Fatalf("missing pre-spring-forward EST -05:00 in %s", sessions)
	}
	if !strings.Contains(sessions, `datetime="2026-03-08T03:30:00-04:00"`) || !strings.Contains(sessions, "EDT") {
		t.Fatalf("missing post-spring-forward EDT -04:00 in %s", sessions)
	}
}

func TestAnalytics_F3_T1_RecentSessionsOrderingAndLatest12Cap(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f3-t1-5", Timezone: "UTC"})
	base := time.Now().UTC().Add(-20 * time.Hour)

	events := make([]ingestEvent, 0, 15)
	for i := 0; i < 15; i++ {
		events = append(events, ingestEvent{
			EventID:     fmt.Sprintf("an-f3-cap-%02d", i),
			Timestamp:   base.Add(time.Duration(i) * time.Hour).Format(time.RFC3339Nano),
			MachineID:   "m-cap12",
			SessionID:   fmt.Sprintf("s-cap12-%02d", i),
			Project:     fmt.Sprintf("proj-%02d", i),
			Provider:    "openai",
			Model:       "gpt-5.4",
			Tool:        "codex",
			TotalTokens: i64(10),
		})
	}
	s.ingestEvents(t, events)

	client := s.authClient(t)
	analyticsWaitTokens(t, client, s.BaseURL+"/api/v1/analytics?period=30d", 150)
	html := analyticsGetHTML(t, client, s.BaseURL+"/analytics?period=30d")
	sessions := analyticsSectionBetween(t, html, `id="sessions-title"`, `</section>`)

	if got := strings.Count(sessions, `<time datetime="`); got != 12 {
		t.Fatalf("/analytics Recent sessions rendered %d rows, want 12", got)
	}
	for i := 3; i < 15; i++ {
		if !strings.Contains(sessions, fmt.Sprintf("proj-%02d", i)) {
			t.Fatalf("missing newest session proj-%02d", i)
		}
	}
	for i := 0; i < 3; i++ {
		if strings.Contains(sessions, fmt.Sprintf("proj-%02d", i)) {
			t.Fatalf("older session proj-%02d should be excluded by LIMIT 12", i)
		}
	}

	// Verify descending timestamp order.
	dtRe := regexp.MustCompile(`datetime="([^"]+)"`)
	matches := dtRe.FindAllStringSubmatch(sessions, -1)
	for i := 1; i < len(matches); i++ {
		prev, err1 := time.Parse(time.RFC3339, matches[i-1][1])
		cur, err2 := time.Parse(time.RFC3339, matches[i][1])
		if err1 != nil || err2 != nil {
			t.Fatalf("parse datetime attributes: %v / %v", err1, err2)
		}
		if cur.After(prev) {
			t.Fatalf("Recent sessions out of descending order: %s before %s", matches[i-1][1], matches[i][1])
		}
	}
}

func TestAnalytics_F3_T2_RecentSessionsExcludesEmptySessionID(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f3-t2-1"})
	s.ingestEvents(t, []ingestEvent{
		{EventID: "an-f3-ns-1", MachineID: "m-ns", SessionID: "", Project: "no-sess-proj", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(200)},
		{EventID: "an-f3-ns-2", MachineID: "m-ns", SessionID: "real-sess-1", Project: "has-sess-proj", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(100)},
	})

	client := s.authClient(t)
	analyticsWaitTokens(t, client, s.BaseURL+"/api/v1/analytics?period=30d", 300)
	html := analyticsGetHTML(t, client, s.BaseURL+"/analytics?period=30d")
	sessions := analyticsSectionBetween(t, html, `id="sessions-title"`, `</section>`)
	if got := strings.Count(sessions, `<time datetime="`); got != 1 {
		t.Fatalf("expected 1 session row (excluding empty session_id), got %d", got)
	}
	if strings.Contains(sessions, "no-sess-proj") || !strings.Contains(sessions, "has-sess-proj") {
		t.Fatalf("unexpected rows in Recent sessions: %s", sessions)
	}
}

func TestAnalytics_F3_T2_SlightlyFutureTimestampClampsAge(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f3-t2-2", Timezone: "UTC"})
	future := time.Now().UTC().Add(2 * time.Minute).Truncate(time.Second)

	s.ingestEvents(t, []ingestEvent{{
		EventID:     "an-f3-fut",
		Timestamp:   future.Format(time.RFC3339Nano),
		MachineID:   "m-fut",
		SessionID:   "sess-fut",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(50),
	}})

	client := s.authClient(t)
	analyticsWaitTokens(t, client, s.BaseURL+"/api/v1/analytics?period=30d", 50)
	html := analyticsGetHTML(t, client, s.BaseURL+"/analytics?period=30d")
	sessions := analyticsSectionBetween(t, html, `id="sessions-title"`, `</section>`)
	if !strings.Contains(sessions, "· just now</time>") {
		t.Fatalf("expected future timestamp clamped to just now; got %s", sessions)
	}
	if !strings.Contains(sessions, fmt.Sprintf(`datetime="%s"`, future.Format(time.RFC3339))) {
		t.Fatalf("expected valid RFC3339 datetime in %s", sessions)
	}
}

func TestAnalytics_F3_T2_UnauthRejectedAndNoPathLeak(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f3-t2-3-ing", Password: "an-priv-password"})
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "an-f3-priv",
		MachineID:   "m-priv",
		SessionID:   "sess-priv",
		Project:     "clean-project",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(25),
	}})

	resp, _ := s.doReq(t, http.MethodGet, "/analytics", nil, map[string]string{"Accept": "text/html"})
	if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), "/login") {
		t.Fatalf("unauthenticated HTML /analytics status=%d loc=%q", resp.StatusCode, resp.Header.Get("Location"))
	}
	respAPI, _ := s.doReq(t, http.MethodGet, "/analytics", nil, map[string]string{"Accept": "application/json"})
	if respAPI.StatusCode != http.StatusUnauthorized {
		t.Fatalf("unauthenticated /analytics without HTML accept status=%d, want 401", respAPI.StatusCode)
	}

	client := s.authClient(t)
	html := analyticsGetHTML(t, client, s.BaseURL+"/analytics")
	for _, forbidden := range []string{"/Users/", "/home/", ".jsonl"} {
		if strings.Contains(html, forbidden) {
			t.Fatalf("/analytics leaked %q", forbidden)
		}
	}
}

func TestAnalytics_F3_T2_DefaultUTCTimezone(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f3-t2-4"})
	ts := time.Now().UTC().Add(-10 * time.Minute).Truncate(time.Second)

	s.ingestEvents(t, []ingestEvent{{
		EventID:     "an-f3-utc",
		Timestamp:   ts.Format(time.RFC3339Nano),
		MachineID:   "m-utc",
		SessionID:   "sess-utc",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(25),
	}})

	client := s.authClient(t)
	analyticsWaitTokens(t, client, s.BaseURL+"/api/v1/analytics?period=30d", 25)
	html := analyticsGetHTML(t, client, s.BaseURL+"/analytics?period=30d")
	sessions := analyticsSectionBetween(t, html, `id="sessions-title"`, `</section>`)
	if !strings.Contains(sessions, fmt.Sprintf(`datetime="%s"`, ts.Format(time.RFC3339))) || !strings.Contains(sessions, " UTC\"") {
		t.Fatalf("expected UTC datetime and title on default timezone server; got %s", sessions)
	}
}

func TestAnalytics_F3_T2_EmptyFilterState(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f3-t2-5"})
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "an-f3-emp",
		MachineID:   "m-exists",
		SessionID:   "sess-exists",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(25),
	}})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 25)
	html := analyticsGetHTML(t, client, s.BaseURL+"/analytics?machine=nonexistent-box")
	sessions := analyticsSectionBetween(t, html, `id="sessions-title"`, `</section>`)
	if !strings.Contains(sessions, "No sessions match these filters.") {
		t.Fatalf("expected empty sessions state in %s", sessions)
	}
	if strings.Contains(sessions, "<time ") {
		t.Fatalf("did not expect <time> elements when filter matches zero sessions")
	}
}

// =============================================================================
// Area 3: Feature 8 — Single-Pass Streaming Analytics Accuracy (LANDED)
// =============================================================================

func TestAnalytics_F8_T1_SummaryPointsBreakdownShareConsistency(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f8-t1-1", Timezone: "UTC"})
	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.UTC)

	s.ingestEvents(t, []ingestEvent{
		{EventID: "an-f8-1", Timestamp: today.AddDate(0, 0, -2).Format(time.RFC3339Nano), MachineID: "m1", SessionID: "s1", Project: "alpha", Provider: "openai", Model: "gpt-5.4", Tool: "codex", InputTokens: i64(100), CacheReadTokens: i64(200), CacheWriteTokens: i64(50), OutputTokens: i64(50), TotalTokens: i64(400)},
		{EventID: "an-f8-2", Timestamp: today.AddDate(0, 0, -1).Format(time.RFC3339Nano), MachineID: "m1", SessionID: "s2", Project: "beta", Provider: "anthropic", Model: "claude-sonnet-4", Tool: "claude-code", InputTokens: i64(300), CacheReadTokens: i64(100), CacheWriteTokens: i64(0), OutputTokens: i64(200), TotalTokens: i64(600)},
		{EventID: "an-f8-3", Timestamp: today.Add(-1 * time.Hour).Format(time.RFC3339Nano), MachineID: "m2", SessionID: "s3", Project: "alpha", Provider: "openai", Model: "gpt-5.4", Tool: "codex", InputTokens: i64(50), CacheReadTokens: i64(0), CacheWriteTokens: i64(50), OutputTokens: i64(100), TotalTokens: i64(200)},
	})

	client := s.authClient(t)
	api := analyticsWaitTokens(t, client, s.BaseURL+"/api/v1/analytics?period=30d&dimension=projects", 1200)

	if api.Summary.Tokens != 1200 || api.Summary.Events != 3 {
		t.Fatalf("summary = %+v, want tokens=1200 events=3", api.Summary)
	}
	var ptTok, ptEv, ptIn, ptCached, ptOut int64
	for _, p := range api.Points {
		ptTok += p.Tokens
		ptEv += p.Events
		ptIn += p.InputTokens
		ptCached += p.CachedTokens
		ptOut += p.OutputTokens
	}
	if ptTok != api.Summary.Tokens || ptEv != api.Summary.Events {
		t.Fatalf("points sum (%d/%d) != summary (%d/%d)", ptTok, ptEv, api.Summary.Tokens, api.Summary.Events)
	}
	if ptIn != 450 || ptCached != 400 || ptOut != 350 {
		t.Fatalf("point composition sums = (in=%d, cached=%d, out=%d), want (450, 400, 350)", ptIn, ptCached, ptOut)
	}
	var bdTok, bdEv int64
	var bdShare float64
	for _, b := range api.Breakdown {
		bdTok += b.Tokens
		bdEv += b.Events
		bdShare += b.Share
	}
	if bdTok != api.Summary.Tokens || bdEv != api.Summary.Events || math.Abs(bdShare-1.0) > 1e-6 {
		t.Fatalf("breakdown sums (%d/%d/%.6f) mismatch summary", bdTok, bdEv, bdShare)
	}
}

func TestAnalytics_F8_T1_AllFiveDimensionsAndCrossFilters(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f8-t1-2"})
	s.ingestEvents(t, []ingestEvent{
		{EventID: "an-f8-d1", MachineID: "m-one", SessionID: "s-1", Project: "p-one", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(700)},
		{EventID: "an-f8-d2", MachineID: "m-two", SessionID: "s-2", Project: "p-two", Provider: "anthropic", Model: "claude-sonnet-4", Tool: "claude-code", TotalTokens: i64(300)},
	})

	client := s.authClient(t)
	analyticsWaitTokens(t, client, s.BaseURL+"/api/v1/analytics?period=30d&dimension=projects", 1000)

	wantFirst := map[string]string{
		"projects":  "p-one",
		"harnesses": "codex",
		"providers": "openai",
		"models":    "gpt-5.4",
		"machines":  "m-one",
	}
	for dim, wantName := range wantFirst {
		var api analyticsAPIPayload
		getJSONWithClient(t, client, s.BaseURL+"/api/v1/analytics?period=30d&dimension="+dim, &api)
		if len(api.Breakdown) != 2 || api.Breakdown[0].Name != wantName || api.Breakdown[0].Tokens != 700 {
			t.Fatalf("dimension %s breakdown[0] = %+v, want %s/700", dim, api.Breakdown, wantName)
		}
	}

	var filtered analyticsAPIPayload
	getJSONWithClient(t, client, s.BaseURL+"/api/v1/analytics?period=30d&dimension=models&machine=m-two&provider=anthropic&model=claude-sonnet-4&tool=claude-code", &filtered)
	if filtered.Summary.Tokens != 300 || filtered.Summary.Threads != 1 || len(filtered.Breakdown) != 1 {
		t.Fatalf("cross-filtered analytics = %+v", filtered)
	}
}

func TestAnalytics_F8_T1_TimelineEndpointMatchesAnalyticsPoints(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f8-t1-3", Timezone: "UTC"})
	now := time.Now().UTC()
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "an-f8-tl-1",
		Timestamp:   now.Add(-2 * time.Hour).Format(time.RFC3339Nano),
		MachineID:   "m-tl",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(500),
	}})

	client := s.authClient(t)
	analyticsWaitTokens(t, client, s.BaseURL+"/api/v1/analytics?period=30d", 500)

	cases := []struct {
		period string
		bucket string
		count  int
	}{
		{"24h", "hour", 25},
		{"7d", "day", 7},
		{"30d", "day", 30},
		{"90d", "day", 90},
		{"all", "month", 1},
	}
	for _, tc := range cases {
		var full analyticsAPIPayload
		var tl analyticsTimelinePayload
		getJSONWithClient(t, client, s.BaseURL+"/api/v1/analytics?period="+tc.period, &full)
		getJSONWithClient(t, client, s.BaseURL+"/api/v1/analytics/timeline?period="+tc.period, &tl)
		if tl.Bucket != tc.bucket {
			t.Fatalf("period=%s bucket=%q, want %q", tc.period, tl.Bucket, tc.bucket)
		}
		if len(tl.Points) != tc.count {
			t.Fatalf("period=%s points len=%d, want %d", tc.period, len(tl.Points), tc.count)
		}
		b1, err1 := json.Marshal(full.Points)
		b2, err2 := json.Marshal(tl.Points)
		if err1 != nil || err2 != nil {
			t.Fatalf("marshal points: %v / %v", err1, err2)
		}
		if !bytes.Equal(b1, b2) {
			t.Fatalf("period=%s timeline points differ from /api/v1/analytics points", tc.period)
		}
	}
}

func TestAnalytics_F8_T1_PeriodComparisonWindowAndCacheDelta(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f8-t1-4", Timezone: "UTC"})
	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.UTC)

	// Current 7d window: [today-6d .. today+1d). Previous 7d window: [today-13d .. today-6d).
	s.ingestEvents(t, []ingestEvent{
		{EventID: "an-f8-cmp-prev", Timestamp: today.AddDate(0, 0, -10).Format(time.RFC3339Nano), MachineID: "m-cmp", Provider: "openai", Model: "gpt-5.4", Tool: "codex", InputTokens: i64(200), CacheReadTokens: i64(200), OutputTokens: i64(100), TotalTokens: i64(500)},
		{EventID: "an-f8-cmp-cur", Timestamp: today.AddDate(0, 0, -2).Format(time.RFC3339Nano), MachineID: "m-cmp", Provider: "openai", Model: "gpt-5.4", Tool: "codex", InputTokens: i64(250), CacheReadTokens: i64(750), OutputTokens: i64(0), TotalTokens: i64(1000)},
	})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 1500)

	var api7 analyticsAPIPayload
	getJSONWithClient(t, client, s.BaseURL+"/api/v1/analytics?period=7d", &api7)
	if api7.Comparison == nil {
		t.Fatalf("expected non-nil comparison for period=7d")
	}
	if api7.Comparison.PreviousTokens != 500 || api7.Comparison.PreviousEvents != 1 {
		t.Fatalf("previous = (%d, %d), want (500, 1)", api7.Comparison.PreviousTokens, api7.Comparison.PreviousEvents)
	}
	if api7.Comparison.TokenChangePercent == nil || math.Abs(*api7.Comparison.TokenChangePercent-100.0) > 1e-6 {
		t.Fatalf("token_change_percent = %v, want +100.0", api7.Comparison.TokenChangePercent)
	}
	if api7.Comparison.PreviousCacheHitRate == nil || math.Abs(*api7.Comparison.PreviousCacheHitRate-0.5) > 1e-6 {
		t.Fatalf("previous_cache_hit_rate = %v, want 0.5", api7.Comparison.PreviousCacheHitRate)
	}

	var apiAll analyticsAPIPayload
	getJSONWithClient(t, client, s.BaseURL+"/api/v1/analytics?period=all", &apiAll)
	if apiAll.Comparison != nil {
		t.Fatalf("expected nil comparison for period=all, got %+v", apiAll.Comparison)
	}
}

func TestAnalytics_F8_T1_ShareTimelineTopFivePlusOtherAndRowSum(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f8-t1-5", Timezone: "UTC"})
	now := time.Now().UTC()
	today := time.Date(now.Year(), now.Month(), now.Day(), 12, 0, 0, 0, time.UTC)

	models := []struct {
		name string
		day1 int64
		day2 int64
	}{
		{"m-a", 1000, 100},
		{"m-b", 800, 100},
		{"m-c", 600, 100},
		{"m-d", 400, 100},
		{"m-e", 300, 100},
		{"m-z", 50, 50},
		{"m-y", 50, 50},
	}
	var events []ingestEvent
	for i, m := range models {
		events = append(events,
			ingestEvent{EventID: fmt.Sprintf("an-f8-sh1-%d", i), Timestamp: today.AddDate(0, 0, -2).Format(time.RFC3339Nano), MachineID: "m-sh", Provider: "openai", Model: m.name, Tool: "codex", TotalTokens: i64(m.day1)},
			ingestEvent{EventID: fmt.Sprintf("an-f8-sh2-%d", i), Timestamp: today.AddDate(0, 0, -1).Format(time.RFC3339Nano), MachineID: "m-sh", Provider: "openai", Model: m.name, Tool: "codex", TotalTokens: i64(m.day2)},
		)
	}
	s.ingestEvents(t, events)

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 3800)

	var api analyticsAPIPayload
	getJSONWithClient(t, client, s.BaseURL+"/api/v1/analytics?period=7d&dimension=models", &api)
	if len(api.ShareSeries) != 6 || api.ShareSeries[5].Name != "Other" {
		t.Fatalf("expected 6 share_series ending with Other; got %+v", api.ShareSeries)
	}
	// Tie-break check between m-y (100) and m-z (100).
	if len(api.Breakdown) != 7 || api.Breakdown[5].Name != "m-y" || api.Breakdown[6].Name != "m-z" {
		t.Fatalf("expected alphabetical tie-break m-y before m-z; got %+v", api.Breakdown)
	}
	for _, sp := range api.SharePoints {
		if sp.Tokens == 0 {
			continue
		}
		var sum float64
		for _, v := range sp.Values {
			sum += v.Share
		}
		if math.Abs(sum-1.0) > 1e-6 {
			t.Fatalf("share_point %s share sum = %.6f, want 1.0", sp.Date, sum)
		}
	}
}

func TestAnalytics_F8_T2_UnknownTotalsRemainUnknownNotZero(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f8-t2-1"})
	s.ingestEvents(t, []ingestEvent{
		{EventID: "an-f8-unk-1", MachineID: "m-u", Provider: "openai", Model: "mystery-unpriced", Tool: "codex", InputTokens: i64(100), CacheReadTokens: i64(50), TotalTokens: nil, TokenAccuracy: "unknown"},
		{EventID: "an-f8-k-1", MachineID: "m-u", Provider: "openai", Model: "mystery-unpriced", Tool: "codex", TotalTokens: i64(200)},
	})

	client := s.authClient(t)
	var api analyticsAPIPayload
	api = analyticsWaitTokens(t, client, s.BaseURL+"/api/v1/analytics?period=30d", 200)
	if api.Summary.UnknownEvents != 1 || api.Summary.Events != 2 {
		t.Fatalf("expected unknown_events=1 events=2, got %+v", api.Summary)
	}
	if api.Summary.Cache.CachedTokens != 50 || api.Summary.Cache.EligibleTokens != 150 {
		t.Fatalf("expected cache (50/150) preserved on unknown-total event, got %+v", api.Summary.Cache)
	}
	if api.Summary.EstimatedCost.UnpricedTokens != 200 {
		t.Fatalf("unpriced_tokens = %d, want 200 (nil total excluded)", api.Summary.EstimatedCost.UnpricedTokens)
	}
}

func TestAnalytics_F8_T2_CacheSummaryExcludesEventsMissingInputOrCacheRead(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f8-t2-2"})
	s.ingestEvents(t, []ingestEvent{
		{EventID: "an-f8-c-1", MachineID: "m-c", Provider: "openai", Model: "gpt-5.4", Tool: "codex", InputTokens: nil, CacheReadTokens: i64(100), TotalTokens: i64(100)},
		{EventID: "an-f8-c-2", MachineID: "m-c", Provider: "openai", Model: "gpt-5.4", Tool: "codex", InputTokens: i64(100), CacheReadTokens: nil, TotalTokens: i64(100)},
	})

	client := s.authClient(t)
	api := analyticsWaitTokens(t, client, s.BaseURL+"/api/v1/analytics?period=30d", 200)
	if api.Summary.Cache.EligibleTokens != 0 || api.Summary.Cache.CachedTokens != 0 || api.Summary.Cache.HitRate != 0 {
		t.Fatalf("expected partial cache fields excluded from summary.cache; got %+v", api.Summary.Cache)
	}
}

func TestAnalytics_F8_T2_InvalidPeriodAndDimensionFallbackDefaults(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f8-t2-3"})
	client := s.authClient(t)

	var api analyticsAPIPayload
	getJSONWithClient(t, client, s.BaseURL+"/api/v1/analytics?period=bogus&dimension=bogus", &api)
	if api.Filter.Period != "30d" || api.Filter.Dimension != "projects" {
		t.Fatalf("fallback filter = %+v, want period=30d dimension=projects", api.Filter)
	}
}

func TestAnalytics_F8_T2_EmptyDatabaseAndZeroMatchFilter(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f8-t2-4"})
	client := s.authClient(t)

	for _, u := range []string{
		s.BaseURL + "/api/v1/analytics?period=30d",
		s.BaseURL + "/api/v1/analytics?period=30d&model=does-not-exist",
	} {
		var api analyticsAPIPayload
		getJSONWithClient(t, client, u, &api)
		if api.Summary.Tokens != 0 || api.Summary.Events != 0 || api.Summary.ActiveDays != 0 || api.Summary.AverageActiveDay != 0 || api.Summary.Cache.HitRate != 0 {
			t.Fatalf("%s expected zero summary; got %+v", u, api.Summary)
		}
		if len(api.Breakdown) != 0 {
			t.Fatalf("%s expected empty breakdown; got %+v", u, api.Breakdown)
		}
	}
}

func TestAnalytics_F8_T2_AuthRequiredAndSWRConvergenceAfterIngest(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f8-t2-5-ing", Password: "an-swr-pass"})

	for _, p := range []string{"/api/v1/analytics", "/api/v1/analytics/timeline"} {
		resp, _ := s.doReq(t, http.MethodGet, p, nil, nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("unauthenticated %s status = %d, want 401", p, resp.StatusCode)
		}
	}

	client := s.authClient(t)
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "an-f8-swr-1",
		MachineID:   "m-swr",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(100),
	}})
	analyticsWaitTokens(t, client, s.BaseURL+"/api/v1/analytics?period=30d", 100)

	s.ingestEvents(t, []ingestEvent{{
		EventID:     "an-f8-swr-2",
		MachineID:   "m-swr",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(250),
	}})
	analyticsWaitTokens(t, client, s.BaseURL+"/api/v1/analytics?period=30d", 350)
}

// =============================================================================
// Area 4: Feature 13 (Analytics) — Breakdown Drill-Downs & Trend Click-Through (M3)
// =============================================================================

func TestAnalytics_F13_T1_BreakdownRowsLinkToFilteredAnalytics(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f13-t1-1"})
	s.ingestEvents(t, []ingestEvent{
		{EventID: "an-f13-1", MachineID: "m-dr-1", SessionID: "s-dr1", Project: "p-dr1", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(600)},
		{EventID: "an-f13-2", MachineID: "m-dr-2", SessionID: "s-dr2", Project: "p-dr2", Provider: "anthropic", Model: "claude-sonnet-4", Tool: "claude-code", TotalTokens: i64(400)},
	})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 1000)

	dims := []struct {
		dim   string
		param string
		val   string
	}{
		{"machines", "machine=", "m-dr-1"},
		{"providers", "provider=", "openai"},
		{"models", "model=", "gpt-5.4"},
		{"harnesses", "tool=", "codex"},
	}
	for _, d := range dims {
		html := analyticsGetHTML(t, client, s.BaseURL+"/analytics?period=7d&dimension="+d.dim)
		bd := analyticsSectionBetween(t, html, `class="panel breakdown-panel"`, `class="panel share-panel"`)
		var target string
		for _, h := range analyticsExtractHrefs(bd) {
			if strings.HasPrefix(h, "/analytics?") && strings.Contains(h, d.param+url.QueryEscape(d.val)) && strings.Contains(h, "period=7d") {
				target = h
				break
			}
		}
		if target == "" {
			t.Fatalf("dimension %s breakdown missing drill-down link carrying %s%s; bd=%s", d.dim, d.param, d.val, bd)
		}
		filteredHTML := analyticsGetHTML(t, client, s.BaseURL+target)
		if !strings.Contains(filteredHTML, `data-count="600"`) {
			t.Fatalf("following %s did not filter Period tokens to 600", target)
		}
	}
}

func TestAnalytics_F13_T1_ProjectsBreakdownRowsLinkToFilteredSessions(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f13-t1-2"})
	s.ingestEvents(t, []ingestEvent{
		{EventID: "an-f13-p1", MachineID: "m-p", SessionID: "sess-p1", Project: "proj-target", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(500)},
		{EventID: "an-f13-p2", MachineID: "m-p", SessionID: "sess-p2", Project: "proj-other", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(200)},
	})

	client := s.authClient(t)
	analyticsWaitTokens(t, client, s.BaseURL+"/api/v1/analytics?period=30d&dimension=projects", 700)

	html := analyticsGetHTML(t, client, s.BaseURL+"/analytics?period=30d&dimension=projects")
	bd := analyticsSectionBetween(t, html, `class="panel breakdown-panel"`, `class="panel share-panel"`)
	var target string
	for _, h := range analyticsExtractHrefs(bd) {
		if (strings.HasPrefix(h, "/sessions?") || strings.HasPrefix(h, "/analytics?")) && strings.Contains(h, "project=proj-target") {
			target = h
			break
		}
	}
	if target == "" {
		t.Fatalf("projects breakdown missing /sessions? or /analytics? link carrying project=proj-target; bd=%s", bd)
	}
	targetHTML := analyticsGetHTML(t, client, s.BaseURL+target)
	if strings.HasPrefix(target, "/sessions?") {
		if !strings.Contains(targetHTML, "sess-p1") || strings.Contains(targetHTML, "sess-p2") {
			t.Fatalf("following %s did not isolate proj-target sessions", target)
		}
	} else {
		if !strings.Contains(targetHTML, `data-count="500"`) || strings.Contains(analyticsStripForms(targetHTML), "proj-other") {
			t.Fatalf("following %s did not isolate proj-target analytics (500 tokens)", target)
		}
	}
}

func TestAnalytics_F13_T1_DailyTrendBarsCarryDateAndClickThroughToSessions(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f13-t1-3", Timezone: "UTC"})
	now := time.Now().UTC()
	d1 := now.AddDate(0, 0, -2).Format("2006-01-02")
	d2 := now.AddDate(0, 0, -4).Format("2006-01-02")

	s.ingestEvents(t, []ingestEvent{
		{EventID: "an-f13-tr1", Timestamp: d1 + "T12:00:00Z", MachineID: "m-tr", SessionID: "sess-tr-d1", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(300)},
		{EventID: "an-f13-tr2", Timestamp: d2 + "T12:00:00Z", MachineID: "m-tr", SessionID: "sess-tr-d2", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(200)},
	})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 500)

	html := analyticsGetHTML(t, client, s.BaseURL+"/analytics?period=7d&machine=m-tr")
	trend := analyticsSectionBetween(t, html, `class="trend-chart`, `class="panel breakdown-panel"`)
	if !strings.Contains(trend, fmt.Sprintf(`data-date="%s"`, d1)) {
		t.Fatalf("daily trend bars missing data-date=%q", d1)
	}
	if !strings.Contains(html, "/sessions?") {
		t.Fatalf("/analytics missing click-through wiring to /sessions")
	}

	sessHTML := analyticsGetHTML(t, client, fmt.Sprintf("%s/sessions?from=%s&to=%s&machine=m-tr", s.BaseURL, d1, d1))
	if !strings.Contains(sessHTML, "sess-tr-d1") || strings.Contains(sessHTML, "sess-tr-d2") {
		t.Fatalf("/sessions?from=%s&to=%s did not isolate d1 session", d1, d1)
	}
}

func TestAnalytics_F13_T1_BreakdownDrillDownPreservesActiveSecondaryFilters(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f13-t1-4"})
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "an-f13-pres",
		MachineID:   "box-1",
		SessionID:   "s-pres",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(400),
	}})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 400)

	html := analyticsGetHTML(t, client, s.BaseURL+"/analytics?period=30d&dimension=models&machine=box-1")
	bd := analyticsSectionBetween(t, html, `class="panel breakdown-panel"`, `class="panel share-panel"`)
	found := false
	for _, h := range analyticsExtractHrefs(bd) {
		if strings.Contains(h, "model=gpt-5.4") && strings.Contains(h, "machine=box-1") && strings.Contains(h, "period=30d") {
			found = true
			break
		}
	}
	if !found {
		t.Fatalf("breakdown drill-down did not preserve active machine=box-1 filter: %s", bd)
	}
}

func TestAnalytics_F13_T1_TrendChartPreservesRovingTabindexAndReadoutAttrs(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f13-t1-5"})
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "an-f13-rov",
		MachineID:   "m-rov",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(100),
	}})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 100)

	html := analyticsGetHTML(t, client, s.BaseURL+"/analytics?period=7d")
	if !strings.Contains(html, `id="trend-readout-value"`) {
		t.Fatalf("missing #trend-readout-value")
	}
	trend := analyticsSectionBetween(t, html, `class="trend-chart`, `class="panel breakdown-panel"`)
	btnRe := regexp.MustCompile(`<button class="trend-column[^"]*"[^>]*>`)
	btns := btnRe.FindAllString(trend, -1)
	if len(btns) != 7 {
		t.Fatalf("trend-column count = %d, want 7", len(btns))
	}
	tabZero := 0
	for _, b := range btns {
		if strings.Contains(b, `tabindex="0"`) {
			tabZero++
		}
		for _, attr := range []string{`type="button"`, `data-label=`, `data-tokens=`, `data-input=`, `data-cached=`, `data-output=`} {
			if !strings.Contains(b, attr) {
				t.Fatalf("trend-column missing %s: %s", attr, b)
			}
		}
	}
	if tabZero != 1 {
		t.Fatalf("trend-column tabindex=0 count = %d, want 1", tabZero)
	}
}

func TestAnalytics_F13_T2_UnknownBreakdownBucketDoesNotDrillToAllFilter(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f13-t2-1"})
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "an-f13-unk",
		MachineID:   "m-unk",
		SessionID:   "s-unk",
		Project:     "",
		Provider:    "anthropic",
		Model:       "unknown",
		Tool:        "claude-code",
		TotalTokens: i64(150),
	}})

	client := s.authClient(t)
	analyticsWaitTokens(t, client, s.BaseURL+"/api/v1/analytics?period=30d&dimension=projects", 150)

	for _, check := range []struct {
		dim   string
		param string
	}{
		{"projects", "project="},
		{"models", "model="},
	} {
		html := analyticsGetHTML(t, client, s.BaseURL+"/analytics?period=30d&dimension="+check.dim)
		bd := analyticsSectionBetween(t, html, `class="panel breakdown-panel"`, `class="panel share-panel"`)
		if !strings.Contains(bd, `class="unknown-value"`) {
			t.Fatalf("expected unknown-value row in %s", check.dim)
		}
		for _, h := range analyticsExtractHrefs(bd) {
			u, err := url.Parse(h)
			if err != nil {
				t.Fatalf("parse href %q: %v", h, err)
			}
			key := strings.TrimSuffix(check.param, "=")
			if u.Query().Has(key) && u.Query().Get(key) == "" {
				t.Fatalf("unknown bucket linked to empty %s filter (%q)", check.param, h)
			}
		}
	}
}

func TestAnalytics_F13_T2_BreakdownDrillDownURLEscapesSpecialCharacters(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f13-t2-2"})
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "an-f13-esc",
		MachineID:   "m-esc",
		SessionID:   "s-esc",
		Project:     "team alpha&v1+beta",
		Provider:    "openai",
		Model:       "org/model+v2",
		Tool:        "codex",
		TotalTokens: i64(333),
	}})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 333)

	html := analyticsGetHTML(t, client, s.BaseURL+"/analytics?period=30d&dimension=models")
	bd := analyticsSectionBetween(t, html, `class="panel breakdown-panel"`, `class="panel share-panel"`)
	var target string
	for _, h := range analyticsExtractHrefs(bd) {
		if strings.Contains(h, "model=") {
			target = h
			break
		}
	}
	if target == "" {
		t.Fatalf("expected escaped model drill-down link in %s", bd)
	}
	u, err := url.Parse(target)
	if err != nil {
		t.Fatalf("parse %q: %v", target, err)
	}
	if u.Query().Get("model") != "org/model+v2" {
		t.Fatalf("decoded model param = %q, want %q", u.Query().Get("model"), "org/model+v2")
	}
	filteredHTML := analyticsGetHTML(t, client, s.BaseURL+target)
	if !strings.Contains(filteredHTML, `data-count="333"`) {
		t.Fatalf("following %s did not return matching 333 total", target)
	}
}

func TestAnalytics_F13_T2_Hourly24hAndMonthlyAllTrendBarsHandleNonDayBucketsSafely(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f13-t2-3", Timezone: "UTC"})
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "an-f13-nonday",
		MachineID:   "m-nd",
		SessionID:   "s-nd",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(100),
	}})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 100)

	isoTimeRe := regexp.MustCompile(`from=\d{4}-\d{2}-\d{2}T`)
	for _, period := range []string{"24h", "all"} {
		html := analyticsGetHTML(t, client, s.BaseURL+"/analytics?period="+period)
		if isoTimeRe.MatchString(html) {
			t.Fatalf("period=%s emitted non-YYYY-MM-DD timestamp in /sessions from= param", period)
		}
	}
	// Ensure daily period still implements click-through.
	html7d := analyticsGetHTML(t, client, s.BaseURL+"/analytics?period=7d")
	if !strings.Contains(html7d, "/sessions?from=") && !strings.Contains(html7d, "data-date=") {
		t.Fatalf("7d trend bars missing click-through date attributes")
	}
}

func TestAnalytics_F13_T2_TrendClickThroughOnZeroTokenEmptyDay(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f13-t2-4", Timezone: "UTC"})
	now := time.Now().UTC()
	activeDay := now.Format("2006-01-02")
	emptyDay := now.AddDate(0, 0, -3).Format("2006-01-02")

	s.ingestEvents(t, []ingestEvent{{
		EventID:     "an-f13-zday",
		Timestamp:   activeDay + "T12:00:00Z",
		MachineID:   "m-zd",
		SessionID:   "s-zd",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(200),
	}})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 200)

	html := analyticsGetHTML(t, client, s.BaseURL+"/analytics?period=7d")
	trend := analyticsSectionBetween(t, html, `class="trend-chart`, `class="panel breakdown-panel"`)
	if !strings.Contains(trend, fmt.Sprintf(`data-date="%s"`, emptyDay)) {
		t.Fatalf("expected zero-token day %s to carry data-date", emptyDay)
	}
	sessHTML := analyticsGetHTML(t, client, fmt.Sprintf("%s/sessions?from=%s&to=%s", s.BaseURL, emptyDay, emptyDay))
	if !strings.Contains(sessHTML, "No sessions match these filters.") {
		t.Fatalf("expected empty sessions message for zero-token day %s", emptyDay)
	}
}

func TestAnalytics_F13_T2_DrillDownTargetsRequireAuthAndStayMetadataOnly(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f13-t2-5-ing", Password: "an-f13-pass"})
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "an-f13-auth",
		MachineID:   "m-auth",
		SessionID:   "s-auth",
		Project:     "clean-proj",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(100),
	}})

	today := time.Now().UTC().Format("2006-01-02")
	for _, p := range []string{
		fmt.Sprintf("/sessions?from=%s&to=%s", today, today),
		"/analytics?period=30d&dimension=models&model=gpt-5.4",
	} {
		resp, _ := s.doReq(t, http.MethodGet, p, nil, map[string]string{"Accept": "text/html"})
		if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), "/login") {
			t.Fatalf("unauthenticated %s status=%d loc=%q, want 303 /login", p, resp.StatusCode, resp.Header.Get("Location"))
		}
	}
}

// =============================================================================
// Area 5: Feature 14 — Decluttered Analytics Filter Bar & Exports (M3)
// =============================================================================

func TestAnalytics_F14_T1_FilterBarDropsDuplicateDimensionSelectAndUsesHiddenInput(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f14-t1-1"})
	client := s.authClient(t)

	html := analyticsGetHTML(t, client, s.BaseURL+"/analytics?dimension=models&period=7d")
	dupDimSelectRe := regexp.MustCompile(`(?i)<select[^>]*\bname=["']dimension["']`)
	if strings.Contains(html, `id="dimension-filter"`) || dupDimSelectRe.MatchString(html) {
		t.Fatalf("/analytics still contains duplicate dimension <select> filter")
	}
	filterForm := analyticsSectionBetween(t, html, `<form class="filter-form"`, `</form>`)
	if !strings.Contains(filterForm, `<input type="hidden" name="dimension" value="models">`) {
		t.Fatalf("filter form missing <input type=\"hidden\" name=\"dimension\" value=\"models\">; got %s", filterForm)
	}
	if !strings.Contains(html, `class="dimension-nav"`) {
		t.Fatalf("/analytics missing .dimension-nav tabs")
	}
}

func TestAnalytics_F14_T1_SecondaryFiltersInsideCollapsibleDetailsWithCount(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f14-t1-2"})
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "an-f14-det",
		MachineID:   "m1",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(100),
	}})
	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 100)

	html := analyticsGetHTML(t, client, s.BaseURL+"/analytics?machine=m1&provider=openai")
	filterPanel := analyticsSectionBetween(t, html, `class="panel filter-panel"`, `class="stats"`)
	if !strings.Contains(filterPanel, "<details") || !strings.Contains(filterPanel, "<summary") {
		t.Fatalf("secondary filters not wrapped in collapsible <details>; got %s", filterPanel)
	}
	detailsSeg := analyticsSectionBetween(t, filterPanel, "<details", "</details>")
	for _, want := range []string{`name="machine"`, `name="provider"`, `name="model"`, `name="tool"`} {
		if !strings.Contains(detailsSeg, want) {
			t.Fatalf("collapsible details missing %s", want)
		}
	}
	summarySeg := analyticsSectionBetween(t, detailsSeg, "<summary", "</summary>")
	if !strings.Contains(summarySeg, "2") {
		t.Fatalf("expected active filter count 2 in collapsible <summary>; got %s", summarySeg)
	}
	if !strings.Contains(filterPanel, "Apply filters") || !strings.Contains(filterPanel, "Reset") {
		t.Fatalf("filter toolbar missing Apply filters / Reset controls")
	}
}

func TestAnalytics_F14_T1_ActiveFilterPillsRenderAndRemoveSingleFilter(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f14-t1-3"})
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "an-f14-pill",
		MachineID:   "m1",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(100),
	}})
	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 100)

	html := analyticsGetHTML(t, client, s.BaseURL+"/analytics?period=30d&dimension=models&machine=m1&tool=codex")
	filterPanel := analyticsSectionBetween(t, html, `class="panel filter-panel"`, `class="stats"`)

	var removeMachineHref string
	for _, h := range analyticsExtractHrefs(filterPanel) {
		if !strings.HasPrefix(h, "/analytics") {
			continue
		}
		u, err := url.Parse(h)
		if err != nil {
			t.Fatalf("parse pill href %q: %v", h, err)
		}
		q := u.Query()
		if q.Get("machine") == "" && q.Get("tool") == "codex" && q.Get("dimension") == "models" {
			removeMachineHref = h
			break
		}
	}
	if removeMachineHref == "" {
		t.Fatalf("expected active filter pill remove link dropping only machine=m1 while keeping tool=codex; panel=%s", filterPanel)
	}
	afterHTML := analyticsGetHTML(t, client, s.BaseURL+removeMachineHref)
	if !strings.Contains(afterHTML, `<input type="hidden" name="dimension" value="models">`) &&
		!strings.Contains(afterHTML, `<h2 class="section-title" id="breakdown-title">Models</h2>`) {
		t.Fatalf("following remove pill link lost active dimension=models")
	}
}

func TestAnalytics_F14_T1_ExportLinksPreserveDownloadAttrAndAllFilters(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f14-t1-4"})
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "an-f14-exp",
		MachineID:   "m1",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(200),
	}})
	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 200)

	html := analyticsGetHTML(t, client, s.BaseURL+"/analytics?period=7d&dimension=machines&machine=m1&model=gpt-5.4")
	exportRe := regexp.MustCompile(`<a[^>]*href="([^"]*export[^"]*)"[^>]*>`)
	matches := exportRe.FindAllStringSubmatch(html, -1)
	if len(matches) < 2 {
		t.Fatalf("expected >=2 export links matching a[href*=export], got %d", len(matches))
	}
	for _, m := range matches {
		if !strings.Contains(strings.ToLower(m[0]), "download") {
			t.Fatalf("export link missing download attribute: %s", m[0])
		}
		href := stdhtml.UnescapeString(m[1])
		for _, wantParam := range []string{"period=7d", "dimension=machines", "machine=m1", "model=gpt-5.4"} {
			if !strings.Contains(href, wantParam) {
				t.Fatalf("export link %q missing %q", href, wantParam)
			}
		}
		code, hdr, _ := analyticsGetResp(t, client, s.BaseURL+href, nil)
		if code != http.StatusOK || !strings.Contains(hdr.Get("Content-Disposition"), "attachment;") {
			t.Fatalf("GET %s status=%d disposition=%q", href, code, hdr.Get("Content-Disposition"))
		}
	}
}

func TestAnalytics_F14_T1_NoActiveFiltersShowsCleanToolbarWithoutSpuriousPills(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f14-t1-5"})
	client := s.authClient(t)

	html := analyticsGetHTML(t, client, s.BaseURL+"/analytics")
	filterPanel := analyticsSectionBetween(t, html, `class="panel filter-panel"`, `class="stats"`)
	if !strings.Contains(filterPanel, "<details") {
		t.Fatalf("expected collapsible <details> in filter panel")
	}
	if regexp.MustCompile(`<details[^>]*\bopen\b`).MatchString(filterPanel) {
		t.Fatalf("secondary filter <details> should be collapsed (no open attr) when no filters active")
	}
	if strings.Contains(filterPanel, "filter-pill") && regexp.MustCompile(`class="[^"]*filter-pill[^"]*"`).MatchString(filterPanel) {
		t.Fatalf("unfiltered /analytics should not render active filter pills")
	}
}

func TestAnalytics_F14_T2_FilterFormSubmitPreservesDimensionAndWindow(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f14-t2-1"})
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "an-f14-sub",
		MachineID:   "m1",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(150),
	}})
	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 150)

	html := analyticsGetHTML(t, client, s.BaseURL+"/analytics?period=90d&dimension=harnesses&machine=m1")
	filterForm := analyticsSectionBetween(t, html, `<form class="filter-form"`, `</form>`)
	if !strings.Contains(filterForm, `<input type="hidden" name="dimension" value="harnesses">`) {
		t.Fatalf("hidden dimension input missing harnesses")
	}
	if !strings.Contains(html, `<h2 class="section-title" id="breakdown-title">Harnesses</h2>`) {
		t.Fatalf("breakdown title did not keep Harnesses")
	}
}

func TestAnalytics_F14_T2_CSVExportFormulaInjectionSanitization(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f14-t2-2"})
	s.ingestEvents(t, []ingestEvent{
		{
			EventID:     "an-f14-csv-inj-1",
			MachineID:   "+1+1",
			SessionID:   "s-inj-1",
			Project:     "=CMD()",
			Provider:    "openai",
			Model:       "@SUM(A1)",
			Tool:        "-exec",
			TotalTokens: i64(100),
		},
		{
			EventID:     "an-f14-csv-inj-2",
			MachineID:   "=CMD()",
			SessionID:   "s-inj-2",
			Project:     "plain-proj",
			Provider:    "openai",
			Model:       "gpt-5.4",
			Tool:        "codex",
			TotalTokens: i64(200),
		},
	})

	client := s.authClient(t)
	analyticsWaitTokens(t, client, s.BaseURL+"/api/v1/analytics?period=30d&dimension=projects", 300)

	checks := []struct {
		dim   string
		wants []string
	}{
		// usage.NormalizeProject lowercases project names -> "=cmd()".
		{"projects", []string{"'=cmd()"}},
		// MachineID preserves case -> verify both "+1+1" and uppercase "=CMD()".
		{"machines", []string{"'=CMD()", "'+1+1"}},
		{"models", []string{"'@SUM(A1)"}},
		{"harnesses", []string{"'-exec"}},
	}
	for _, c := range checks {
		code, _, body := analyticsGetResp(t, client, s.BaseURL+"/api/v1/analytics/export?period=30d&dimension="+c.dim+"&format=csv", nil)
		if code != http.StatusOK {
			t.Fatalf("CSV export %s status=%d", c.dim, code)
		}
		rows, err := csv.NewReader(bytes.NewReader(body)).ReadAll()
		if err != nil {
			t.Fatalf("parse CSV %s: %v", c.dim, err)
		}
		if len(rows) < 2 {
			t.Fatalf("CSV export %s missing data rows: %+v", c.dim, rows)
		}
		seenVals := make(map[string]bool)
		for _, r := range rows[1:] {
			if len(r) > 2 {
				seenVals[r[2]] = true
			}
		}
		for _, want := range c.wants {
			if !seenVals[want] {
				t.Fatalf("CSV formula sanitization dim=%s missing %q; rows=%+v", c.dim, want, rows)
			}
		}
	}
}

func TestAnalytics_F14_T2_ExportInvalidFormatDefaultsToJSON(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f14-t2-3"})
	client := s.authClient(t)

	for _, u := range []string{
		s.BaseURL + "/api/v1/analytics/export?format=xml",
		s.BaseURL + "/api/v1/analytics/export",
	} {
		code, hdr, body := analyticsGetResp(t, client, u, nil)
		if code != http.StatusOK {
			t.Fatalf("%s status=%d, want 200", u, code)
		}
		if ct := hdr.Get("Content-Type"); !strings.Contains(ct, "application/json") {
			t.Fatalf("%s Content-Type=%q, want application/json", u, ct)
		}
		if disp := hdr.Get("Content-Disposition"); !strings.Contains(disp, ".json") {
			t.Fatalf("%s Content-Disposition=%q, want .json attachment", u, disp)
		}
		var doc map[string]any
		if err := json.Unmarshal(body, &doc); err != nil {
			t.Fatalf("%s invalid JSON: %v", u, err)
		}
	}
}

func TestAnalytics_F14_T2_FilterBarDoesNotLeakUnauthenticatedOrXSSParams(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f14-t2-4-ing", Password: "f14-password"})

	xssPath := `/analytics?machine=%22%3E%3Cscript%3Ealert(1)%3C%2Fscript%3E`
	resp, _ := s.doReq(t, http.MethodGet, xssPath, nil, map[string]string{"Accept": "text/html"})
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("unauthenticated /analytics status=%d, want 303", resp.StatusCode)
	}

	client := s.authClient(t)
	html := analyticsGetHTML(t, client, s.BaseURL+xssPath)
	if strings.Contains(html, "<script>alert(1)</script>") {
		t.Fatalf("reflected XSS script tag in /analytics filter bar")
	}
}

func TestAnalytics_F14_T2_ResetFilterLinkClearsSecondaryFiltersSafely(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f14-t2-5"})
	s.ingestEvents(t, []ingestEvent{
		{EventID: "an-f14-rst-1", MachineID: "m1", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(100)},
		{EventID: "an-f14-rst-2", MachineID: "m2", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(200)},
	})
	client := s.authClient(t)
	analyticsWaitTokens(t, client, s.BaseURL+"/api/v1/analytics?period=30d", 300)

	html := analyticsGetHTML(t, client, s.BaseURL+"/analytics?machine=m1")
	filterPanel := analyticsSectionBetween(t, html, `class="panel filter-panel"`, `class="stats"`)
	var resetHref string
	for _, h := range analyticsExtractHrefs(filterPanel) {
		if h == "/analytics" || strings.HasPrefix(h, "/analytics?") && !strings.Contains(h, "machine=") {
			resetHref = h
		}
	}
	if resetHref == "" {
		t.Fatalf("expected Reset link clearing secondary filters in %s", filterPanel)
	}
	afterReset := analyticsGetHTML(t, client, s.BaseURL+resetHref)
	if !strings.Contains(afterReset, `data-count="300"`) {
		t.Fatalf("following Reset %q did not restore unfiltered 300 total", resetHref)
	}
}

// =============================================================================
// Area 6: Feature 20 — Estimated Cache Savings, $/1M, Per-Row Cache Hit (M5)
// =============================================================================

func TestAnalytics_F20_T1_CacheSavingsExactMathPricedModels(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f20-t1-1"})
	// gpt-5.4: input=$2.50/M, cached_input=$0.25/M, output=$15/M.
	// 1M input ($2.50) + 2M cache_read ($0.50) + 200k output ($3.00) = $6.00 cost (distinct from savings).
	// Cache savings = 2.0 * (2.50 - 0.25) = $4.50 saved.
	s.ingestEvents(t, []ingestEvent{{
		EventID:         "an-f20-1",
		MachineID:       "m-f20",
		SessionID:       "s-f20",
		Provider:        "openai",
		Model:           "gpt-5.4",
		Tool:            "codex",
		InputTokens:     i64(1_000_000),
		CacheReadTokens: i64(2_000_000),
		OutputTokens:    i64(200_000),
		TotalTokens:     i64(3_200_000),
	}})

	client := s.authClient(t)
	analyticsWaitTokens(t, client, s.BaseURL+"/api/v1/analytics?period=30d", 3_200_000)

	html := analyticsGetHTML(t, client, s.BaseURL+"/analytics?period=30d")
	if !strings.Contains(html, "$4.50 saved") {
		t.Fatalf("/analytics missing '$4.50 saved' cache savings figure")
	}
	if !strings.Contains(html, "$6.00") {
		t.Fatalf("/analytics missing '$6.00' total cost")
	}

	_, _, raw := analyticsGetResp(t, client, s.BaseURL+"/api/v1/analytics?period=30d", nil)
	doc := mustUnmarshalAny(t, raw)
	if !jsonContainsNumber(doc, 4.5) || !jsonContainsNumber(doc, 6.0) {
		t.Fatalf("/api/v1/analytics missing distinct 4.5 (savings) and 6.0 (cost) values: %s", string(raw))
	}
}

func mustUnmarshalAny(t *testing.T, data []byte) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(data, &v); err != nil {
		t.Fatalf("json.Unmarshal: %v", err)
	}
	return v
}

func TestAnalytics_F20_T1_EffectiveCostPerMillionTokens(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f20-t1-2"})
	// claude-sonnet-4: input=$3/M, cached=$0.30/M, output=$15/M.
	// 500k input ($1.50) + 500k cached ($0.15) + 1M output ($15.00) = $16.65 over 2M tokens -> $8.325 ($8.33)/1M.
	s.ingestEvents(t, []ingestEvent{{
		EventID:         "an-f20-2",
		MachineID:       "m-f20-eff",
		Provider:        "anthropic",
		Model:           "claude-sonnet-4",
		Tool:            "claude-code",
		InputTokens:     i64(500_000),
		CacheReadTokens: i64(500_000),
		OutputTokens:    i64(1_000_000),
		TotalTokens:     i64(2_000_000),
	}})

	client := s.authClient(t)
	analyticsWaitTokens(t, client, s.BaseURL+"/api/v1/analytics?period=30d", 2_000_000)

	html := analyticsGetHTML(t, client, s.BaseURL+"/analytics?period=30d")
	if !strings.Contains(html, "$8.33") && !strings.Contains(html, "$8.32") {
		t.Fatalf("/analytics missing effective $/1M rate ($8.33/1M)")
	}
	if !strings.Contains(html, "/1M") {
		t.Fatalf("/analytics missing /1M unit indicator")
	}
}

func TestAnalytics_F20_T1_PerHarnessCacheHitRateAndSavingsBreakdown(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f20-t1-3"})
	// Codex (gpt-5.4): 200k*2.50 + 800k*0.25 + 50k*15 = $1.45 cost, $1.80 saved, $1.38/1M, 80.0% cache hit.
	// Claude Code (claude-sonnet-4): 900k*3.00 + 100k*0.30 + 50k*15 = $3.48 cost, $0.27 saved, $3.31/1M, 10.0% cache hit.
	// Combined: $4.93 cost, $2.07 saved, $2.35/1M over 2.1M tokens.
	s.ingestEvents(t, []ingestEvent{
		{EventID: "an-f20-h1", MachineID: "m-h", Provider: "openai", Model: "gpt-5.4", Tool: "codex", InputTokens: i64(200_000), CacheReadTokens: i64(800_000), OutputTokens: i64(50_000), TotalTokens: i64(1_050_000)},
		{EventID: "an-f20-h2", MachineID: "m-h", Provider: "anthropic", Model: "claude-sonnet-4", Tool: "claude-code", InputTokens: i64(900_000), CacheReadTokens: i64(100_000), OutputTokens: i64(50_000), TotalTokens: i64(1_050_000)},
	})

	client := s.authClient(t)
	analyticsWaitTokens(t, client, s.BaseURL+"/api/v1/analytics?period=30d&dimension=harnesses", 2_100_000)

	html := analyticsGetHTML(t, client, s.BaseURL+"/analytics?period=30d&dimension=harnesses")
	if !strings.Contains(html, "$2.07 saved") {
		t.Fatalf("/analytics missing combined '$2.07 saved'")
	}
	if !strings.Contains(html, "$2.35") || !strings.Contains(html, "/1M") {
		t.Fatalf("/analytics missing combined '$2.35/1M'")
	}
	bd := analyticsSectionBetween(t, html, `class="panel breakdown-panel"`, `class="panel share-panel"`)
	codexRow := analyticsSectionBetween(t, bd, "Codex", "</tr>")
	claudeRow := analyticsSectionBetween(t, bd, "Claude Code", "</tr>")
	if !strings.Contains(codexRow, "80.0%") || strings.Contains(codexRow, "10.0%") {
		t.Fatalf("Codex breakdown row should show 80.0%% (not 10.0%%); got %s", codexRow)
	}
	if !strings.Contains(claudeRow, "10.0%") || strings.Contains(claudeRow, "80.0%") {
		t.Fatalf("Claude Code breakdown row should show 10.0%% (not 80.0%%); got %s", claudeRow)
	}
	if strings.Contains(bd, "$") {
		if !strings.Contains(codexRow, "$1.80") && !strings.Contains(codexRow, "$1.38") && !strings.Contains(codexRow, "$1.45") {
			t.Fatalf("Codex row renders currency without expected $1.80/$1.38/$1.45: %s", codexRow)
		}
		if !strings.Contains(claudeRow, "$0.27") && !strings.Contains(claudeRow, "$3.31") && !strings.Contains(claudeRow, "$3.48") {
			t.Fatalf("Claude Code row renders currency without expected $0.27/$3.31/$3.48: %s", claudeRow)
		}
	}
}

func TestAnalytics_F20_T1_PerModelCacheHitRateAndEffectiveRateBreakdown(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f20-t1-4"})
	// gpt-5.4: $0.70 cost, $1.80 saved, $0.70/1M, 80.0% cache hit.
	// claude-sonnet-4: $2.73 cost, $0.27 saved, $2.73/1M, 10.0% cache hit.
	// Combined: $3.43 cost over 2M -> $1.72/1M, $2.07 saved.
	s.ingestEvents(t, []ingestEvent{
		{EventID: "an-f20-m1", MachineID: "m-m", Provider: "openai", Model: "gpt-5.4", Tool: "codex", InputTokens: i64(200_000), CacheReadTokens: i64(800_000), OutputTokens: i64(0), TotalTokens: i64(1_000_000)},
		{EventID: "an-f20-m2", MachineID: "m-m", Provider: "anthropic", Model: "claude-sonnet-4", Tool: "claude-code", InputTokens: i64(900_000), CacheReadTokens: i64(100_000), OutputTokens: i64(0), TotalTokens: i64(1_000_000)},
	})

	client := s.authClient(t)
	analyticsWaitTokens(t, client, s.BaseURL+"/api/v1/analytics?period=30d&dimension=models", 2_000_000)

	html := analyticsGetHTML(t, client, s.BaseURL+"/analytics?period=30d&dimension=models")
	if !strings.Contains(html, "$2.07 saved") || (!strings.Contains(html, "$1.72") && !strings.Contains(html, "$1.71")) || !strings.Contains(html, "/1M") {
		t.Fatalf("/analytics missing combined $2.07 saved or $1.72/1M")
	}
	bd := analyticsSectionBetween(t, html, `class="panel breakdown-panel"`, `class="panel share-panel"`)
	if !strings.Contains(bd, "Cache") && !strings.Contains(bd, "Hit") {
		t.Fatalf("models breakdown missing cache column header; got %s", bd)
	}
	gptRow := analyticsSectionBetween(t, bd, "GPT-5.4", "</tr>")
	sonnetMarker := "Claude Sonnet"
	if !strings.Contains(bd, sonnetMarker) {
		sonnetMarker = "Sonnet 4"
	}
	sonnetRow := analyticsSectionBetween(t, bd, sonnetMarker, "</tr>")
	if !strings.Contains(gptRow, "80.0%") || strings.Contains(gptRow, "10.0%") {
		t.Fatalf("GPT-5.4 breakdown row should show 80.0%% (not 10.0%%); got %s", gptRow)
	}
	if !strings.Contains(sonnetRow, "10.0%") || strings.Contains(sonnetRow, "80.0%") {
		t.Fatalf("Claude Sonnet breakdown row should show 10.0%% (not 80.0%%); got %s", sonnetRow)
	}
	if strings.Contains(bd, "$") {
		if !strings.Contains(gptRow, "$0.70") && !strings.Contains(gptRow, "$1.80") {
			t.Fatalf("GPT-5.4 row renders currency without expected $0.70/$1.80: %s", gptRow)
		}
		if !strings.Contains(sonnetRow, "$2.73") && !strings.Contains(sonnetRow, "$0.27") {
			t.Fatalf("Claude Sonnet row renders currency without expected $2.73/$0.27: %s", sonnetRow)
		}
	}
}

func TestAnalytics_F20_T1_CacheWriteTokensIncludedInEffectiveCost(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f20-t1-cw"})
	// claude-sonnet-4: input=$3/M, cached_input=$0.30/M, cache_write=$3.75/M, output=$15/M.
	// 1M input ($3.00) + 1M cache_read ($0.30) + 1M cache_write ($3.75) + 200k output ($3.00) = $10.05 over 3.2M -> $3.14/1M.
	// Cache savings = 1.0 * (3.00 - 0.30) = $2.70 saved.
	s.ingestEvents(t, []ingestEvent{{
		EventID:          "an-f20-cw-1",
		MachineID:        "m-cw",
		Provider:         "anthropic",
		Model:            "claude-sonnet-4",
		Tool:             "claude-code",
		InputTokens:      i64(1_000_000),
		CacheReadTokens:  i64(1_000_000),
		CacheWriteTokens: i64(1_000_000),
		OutputTokens:     i64(200_000),
		TotalTokens:      i64(3_200_000),
	}})

	client := s.authClient(t)
	api := analyticsWaitTokens(t, client, s.BaseURL+"/api/v1/analytics?period=30d", 3_200_000)
	if api.Summary.EstimatedCost.PricedTokens != 3_200_000 || math.Abs(api.Summary.EstimatedCost.Amount-10.05) > 1e-6 {
		t.Fatalf("estimated_cost = %+v, want amount=10.05 priced_tokens=3200000", api.Summary.EstimatedCost)
	}

	html := analyticsGetHTML(t, client, s.BaseURL+"/analytics?period=30d")
	for _, want := range []string{"$10.05", "$2.70 saved", "$3.14", "/1M"} {
		if !strings.Contains(html, want) {
			t.Fatalf("/analytics missing %q when CacheWriteTokens priced at $3.75/M", want)
		}
	}
}

func TestAnalytics_F20_T1_MixedPricedAndUnpricedWindowReportsPartialSavingsHonestly(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f20-t1-5"})
	s.ingestEvents(t, []ingestEvent{
		// gpt-5.4: $4.50 cost, $4.50 saved, 3.1M priced tokens.
		{EventID: "an-f20-mix1", MachineID: "m-mix", Provider: "openai", Model: "gpt-5.4", Tool: "codex", InputTokens: i64(1_000_000), CacheReadTokens: i64(2_000_000), OutputTokens: i64(100_000), TotalTokens: i64(3_100_000)},
		// claude-haiku-5-5 is mode: unknown in catalog -> 2M unpriced tokens, 0 priced savings.
		{EventID: "an-f20-mix2", MachineID: "m-mix", Provider: "anthropic", Model: "claude-haiku-5-5", Tool: "claude-code", InputTokens: i64(1_000_000), CacheReadTokens: i64(1_000_000), OutputTokens: i64(0), TotalTokens: i64(2_000_000)},
	})

	client := s.authClient(t)
	analyticsWaitTokens(t, client, s.BaseURL+"/api/v1/analytics?period=30d", 5_100_000)

	html := analyticsGetHTML(t, client, s.BaseURL+"/analytics?period=30d")
	// Effective $/1M must divide $4.50 by 3.1M priced tokens ($1.45/1M), never 5.1M ($0.88/1M).
	if !strings.Contains(html, "$4.50 saved") {
		t.Fatalf("expected '$4.50 saved' from priced gpt-5.4 subset only")
	}
	if !strings.Contains(html, "$1.45") {
		t.Fatalf("expected effective $/1M computed over 3.1M priced tokens ($1.45/1M)")
	}
	if strings.Contains(html, "$0.88") {
		t.Fatalf("effective $/1M must not divide cost by unpriced tokens")
	}
}

func TestAnalytics_F20_T2_UnknownModelCacheSavingsStaysUnknownNotZero(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f20-t2-1"})
	s.ingestEvents(t, []ingestEvent{{
		EventID:         "an-f20-unk-m",
		MachineID:       "m-unk",
		Provider:        "custom",
		Model:           "mystery-unpriced-7b",
		Tool:            "codex",
		InputTokens:     i64(1_000_000),
		CacheReadTokens: i64(1_000_000),
		OutputTokens:    i64(0),
		TotalTokens:     i64(2_000_000),
	}})

	client := s.authClient(t)
	analyticsWaitTokens(t, client, s.BaseURL+"/api/v1/analytics?period=30d", 2_000_000)

	html := analyticsGetHTML(t, client, s.BaseURL+"/analytics?period=30d")
	if strings.Contains(html, "$0.00 saved") || strings.Contains(html, "$0.00/1M") {
		t.Fatalf("unpriced model must not fabricate $0.00 saved or $0.00/1M")
	}
	// Cache hit rate itself (50.0%) is still valid from token counts!
	if !strings.Contains(html, "50.0%") {
		t.Fatalf("expected 50.0%% cache hit rate even when pricing is unknown")
	}
	// Also ensure effective $/1M / savings UI is actually implemented (not absent).
	if !strings.Contains(html, "/1M") && !strings.Contains(strings.ToLower(html), "saved") {
		t.Fatalf("expected cache savings / effective $/1M UI in /analytics")
	}
}

func TestAnalytics_F20_T2_PricedModelWithoutCacheRateOmitsCacheSavings(t *testing.T) {
	requireE2E(t)
	// mistral-large-latest has input=$2/M, output=$6/M, and NO cached_input_per_million in catalog/models.yaml.
	t.Run("nonzero_cache_read_stays_unpriced", func(t *testing.T) {
		requireE2E(t)
		s := startTestServer(t, serverOpts{IngestToken: "an-f20-t2-2a"})
		s.ingestEvents(t, []ingestEvent{{
			EventID:         "an-f20-nocrate-nz",
			MachineID:       "m-noc",
			Provider:        "mistral",
			Model:           "mistral-large-latest",
			Tool:            "opencode",
			InputTokens:     i64(1_000_000),
			CacheReadTokens: i64(500_000),
			OutputTokens:    i64(500_000),
			TotalTokens:     i64(2_000_000),
		}})

		client := s.authClient(t)
		api := analyticsWaitTokens(t, client, s.BaseURL+"/api/v1/analytics?period=30d", 2_000_000)
		if api.Summary.EstimatedCost.PricedTokens != 0 {
			t.Fatalf("expected priced_tokens=0 when CacheReadTokens>0 without cached_input_per_million, got %+v", api.Summary.EstimatedCost)
		}

		html := analyticsGetHTML(t, client, s.BaseURL+"/analytics?period=30d")
		if !strings.Contains(html, "33.3%") {
			t.Fatalf("expected 33.3%% cache hit rate from token counts")
		}
		if strings.Contains(html, "$0.00 saved") || strings.Contains(html, "$1.00 saved") || strings.Contains(html, "$0.00/1M") {
			t.Fatalf("unpriced mistral-large-latest must not fabricate $0.00 saved or $0.00/1M")
		}
		if !strings.Contains(html, "—") {
			t.Fatalf("expected unknown — placeholder for unpriced cost/savings/$/1M")
		}
	})

	t.Run("zero_cache_read_priced_without_cache_rate", func(t *testing.T) {
		requireE2E(t)
		s := startTestServer(t, serverOpts{IngestToken: "an-f20-t2-2b"})
		// 1M input ($2.00) + 0 cached + 1M output ($6.00) = $8.00 over 2M -> $4.00/1M.
		s.ingestEvents(t, []ingestEvent{{
			EventID:         "an-f20-nocrate-z",
			MachineID:       "m-noc-z",
			Provider:        "mistral",
			Model:           "mistral-large-latest",
			Tool:            "opencode",
			InputTokens:     i64(1_000_000),
			CacheReadTokens: i64(0),
			OutputTokens:    i64(1_000_000),
			TotalTokens:     i64(2_000_000),
		}})

		client := s.authClient(t)
		api := analyticsWaitTokens(t, client, s.BaseURL+"/api/v1/analytics?period=30d", 2_000_000)
		if api.Summary.EstimatedCost.PricedTokens != 2_000_000 || math.Abs(api.Summary.EstimatedCost.Amount-8.0) > 1e-6 {
			t.Fatalf("expected priced_tokens=2M amount=8.0, got %+v", api.Summary.EstimatedCost)
		}

		html := analyticsGetHTML(t, client, s.BaseURL+"/analytics?period=30d")
		if !strings.Contains(html, "$4.00") || !strings.Contains(html, "/1M") {
			t.Fatalf("expected $4.00/1M on priced mistral-large-latest with 0 CacheReadTokens")
		}
		if strings.Contains(html, "$1.00 saved") || strings.Contains(html, "-$") {
			t.Fatalf("model without cached_input_per_million must not fabricate non-zero or negative cache savings")
		}
	})
}

func TestAnalytics_F20_T2_ZeroCacheReadTokensOnPricedModel(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f20-t2-3"})
	// gpt-5.4: 1M input ($2.50) + 0 cached + 500k output ($7.50) = $10.00 over 1.5M = $6.67/1M.
	s.ingestEvents(t, []ingestEvent{{
		EventID:         "an-f20-zcache",
		MachineID:       "m-zc",
		Provider:        "openai",
		Model:           "gpt-5.4",
		Tool:            "codex",
		InputTokens:     i64(1_000_000),
		CacheReadTokens: i64(0),
		OutputTokens:    i64(500_000),
		TotalTokens:     i64(1_500_000),
	}})

	client := s.authClient(t)
	analyticsWaitTokens(t, client, s.BaseURL+"/api/v1/analytics?period=30d", 1_500_000)

	html := analyticsGetHTML(t, client, s.BaseURL+"/analytics?period=30d")
	if strings.Contains(html, "NaN") || strings.Contains(html, "-$") {
		t.Fatalf("zero cache read produced NaN or negative savings")
	}
	if !strings.Contains(html, "$6.67") || !strings.Contains(html, "/1M") {
		t.Fatalf("expected effective $6.67/1M for gpt-5.4 with 0 cache read")
	}
}

func TestAnalytics_F20_T2_BreakdownRowWithoutEligibleCacheInputShowsUnknownDash(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f20-t2-4"})
	s.ingestEvents(t, []ingestEvent{
		{EventID: "an-f20-elig", MachineID: "m-el", Provider: "openai", Model: "gpt-5.4", Tool: "codex", InputTokens: i64(500), CacheReadTokens: i64(250), TotalTokens: i64(750)},
		{EventID: "an-f20-noel", MachineID: "m-el", Provider: "openai", Model: "gpt-5.4", Tool: "opencode", InputTokens: nil, CacheReadTokens: nil, TotalTokens: i64(250)},
	})

	client := s.authClient(t)
	analyticsWaitTokens(t, client, s.BaseURL+"/api/v1/analytics?period=30d&dimension=harnesses", 1000)

	html := analyticsGetHTML(t, client, s.BaseURL+"/analytics?period=30d&dimension=harnesses")
	bd := analyticsSectionBetween(t, html, `class="panel breakdown-panel"`, `class="panel share-panel"`)
	codexRow := analyticsSectionBetween(t, bd, "Codex", "</tr>")
	if !strings.Contains(codexRow, "33.3%") {
		t.Fatalf("expected Codex row 33.3%% per-row cache hit rate in %s", codexRow)
	}
	opencodeRow := analyticsSectionBetween(t, bd, "OpenCode", "</tr>")
	if regexp.MustCompile(`(?i)>\s*0\.0%\s*<`).MatchString(opencodeRow) {
		t.Fatalf("OpenCode row without eligible input/cache tokens must show —/Unknown, not 0.0%%: %s", opencodeRow)
	}
	if !strings.Contains(opencodeRow, "—") && !strings.Contains(opencodeRow, "Unknown") {
		t.Fatalf("OpenCode row missing — or Unknown cache hit cell: %s", opencodeRow)
	}
}

func TestAnalytics_F20_T2_CacheReadPriceHigherThanInputPriceClampedNonNegative(t *testing.T) {
	requireE2E(t)
	customCatalog := `schema_version: "2"
models:
  inverted-cache-model:
    provider: custom
    display_name: Inverted Cache Model
    tier: Standard
    aliases: ["inverted-cache-model"]
    pricing:
      currency: USD
      mode: standard
      verified_at: 2026-11-12
      source: https://example.com/pricing
      input_per_million: 1.0
      cached_input_per_million: 5.0
      output_per_million: 2.0
`
	s := startTestServer(t, serverOpts{IngestToken: "an-f20-t2-5", CatalogYAML: customCatalog})
	s.ingestEvents(t, []ingestEvent{{
		EventID:         "an-f20-invcat",
		MachineID:       "m-ic",
		Provider:        "custom",
		Model:           "inverted-cache-model",
		Tool:            "codex",
		InputTokens:     i64(1_000_000),
		CacheReadTokens: i64(1_000_000),
		OutputTokens:    i64(0),
		TotalTokens:     i64(2_000_000),
	}})

	client := s.authClient(t)
	analyticsWaitTokens(t, client, s.BaseURL+"/api/v1/analytics?period=30d", 2_000_000)

	html := analyticsGetHTML(t, client, s.BaseURL+"/analytics?period=30d")
	if strings.Contains(html, "-$") || strings.Contains(html, "$-") {
		t.Fatalf("inverted cache price leaked negative dollar savings")
	}
	if !strings.Contains(html, "/1M") {
		t.Fatalf("expected effective $/1M UI in /analytics")
	}
}

// =============================================================================
// Area 7: Feature 22 (Analytics Link) — /tokedex#uncataloged-title Link (M5)
// =============================================================================

func TestAnalytics_F22_T1_UncatalogedUsageLinksToTokedexWhenEnabled(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f22-t1-1", EnableTokedex: true})
	s.ingestEvents(t, []ingestEvent{
		{EventID: "an-f22-1a", MachineID: "m-td", Provider: "openai", Model: "gpt-5.4", Tool: "codex", InputTokens: i64(300), OutputTokens: i64(200), TotalTokens: i64(500)},
		{EventID: "an-f22-1b", MachineID: "m-td", Provider: "custom", Model: "mystery-llm-9b", Tool: "codex", TotalTokens: i64(500)},
	})

	client := s.authClient(t)
	analyticsWaitTokens(t, client, s.BaseURL+"/api/v1/analytics?period=30d", 1000)

	html := analyticsGetHTML(t, client, s.BaseURL+"/analytics?period=30d")
	insights := analyticsSectionBetween(t, html, `class="insights-strip"`, `</section>`)
	if !strings.Contains(insights, "Catalog coverage") || !strings.Contains(insights, `href="/tokedex#uncataloged-title"`) {
		t.Fatalf("expected Catalog coverage card with link to /tokedex#uncataloged-title; got %s", insights)
	}
}

func TestAnalytics_F22_T1_UncatalogedLinkTargetResolvesTo200WithUncatalogedSection(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f22-t1-2", EnableTokedex: true})
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "an-f22-2a",
		MachineID:   "m-td2",
		Provider:    "custom",
		Model:       "mystery-llm-9b",
		Tool:        "codex",
		TotalTokens: i64(400),
	}})

	client := s.authClient(t)
	analyticsWaitTokens(t, client, s.BaseURL+"/api/v1/analytics?period=30d", 400)

	anHTML := analyticsGetHTML(t, client, s.BaseURL+"/analytics?period=30d")
	if !strings.Contains(anHTML, `href="/tokedex#uncataloged-title"`) {
		t.Fatalf("/analytics missing /tokedex#uncataloged-title link")
	}
	tdHTML := analyticsGetHTML(t, client, s.BaseURL+"/tokedex")
	if !strings.Contains(tdHTML, `id="uncataloged-title"`) || !strings.Contains(tdHTML, "mystery-llm-9b") {
		t.Fatalf("/tokedex missing id=uncataloged-title or mystery-llm-9b")
	}
}

func TestAnalytics_F22_T1_CatalogedUnknownModeModelStillSurfacesCoverageCardAndLink(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f22-t1-3", EnableTokedex: true})
	s.ingestEvents(t, []ingestEvent{
		{EventID: "an-f22-3a", MachineID: "m-td3", Provider: "openai", Model: "gpt-5.4", Tool: "codex", InputTokens: i64(300), OutputTokens: i64(200), TotalTokens: i64(500)},
		{EventID: "an-f22-3b", MachineID: "m-td3", Provider: "anthropic", Model: "claude-haiku-5-5", Tool: "claude-code", TotalTokens: i64(500)},
	})

	client := s.authClient(t)
	analyticsWaitTokens(t, client, s.BaseURL+"/api/v1/analytics?period=30d", 1000)

	html := analyticsGetHTML(t, client, s.BaseURL+"/analytics?period=30d")
	insights := analyticsSectionBetween(t, html, `class="insights-strip"`, `</section>`)
	if !strings.Contains(insights, "Catalog coverage") || !strings.Contains(insights, "50.0%") {
		t.Fatalf("expected 50.0%% Catalog coverage insight; got %s", insights)
	}
	if !strings.Contains(insights, `href="/tokedex#uncataloged-title"`) {
		t.Fatalf("expected /tokedex#uncataloged-title link when unpriced tokens exist and Tokedex enabled")
	}
}

func TestAnalytics_F22_T1_AllTokensPricedOmitsUncatalogedCardAndLink(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f22-t1-4", EnableTokedex: true})
	s.ingestEvents(t, []ingestEvent{{
		EventID:      "an-f22-4a",
		MachineID:    "m-td4",
		Provider:     "openai",
		Model:        "gpt-5.4",
		Tool:         "codex",
		InputTokens:  i64(600),
		OutputTokens: i64(400),
		TotalTokens:  i64(1000),
	}})

	client := s.authClient(t)
	analyticsWaitTokens(t, client, s.BaseURL+"/api/v1/analytics?period=30d", 1000)

	html := analyticsGetHTML(t, client, s.BaseURL+"/analytics?period=30d")
	if strings.Contains(html, "Catalog coverage") || strings.Contains(html, "/tokedex#uncataloged-title") {
		t.Fatalf("fully priced window must omit Catalog coverage card and uncataloged link")
	}
	// Also verify Tokedex link IS active in topbar nav when enabled (proving server is running M5 Tokedex).
	if !strings.Contains(html, `href="/tokedex"`) {
		t.Fatalf("expected /tokedex nav link when EnableTokedex=true")
	}
}

func TestAnalytics_F22_T1_UncatalogedLinkPersistsAcrossPeriodsAndDimensions(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f22-t1-5", EnableTokedex: true})
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "an-f22-5a",
		Timestamp:   time.Now().UTC().Add(-1 * time.Hour).Format(time.RFC3339Nano),
		MachineID:   "m-td5",
		Provider:    "custom",
		Model:       "mystery-llm-9b",
		Tool:        "codex",
		TotalTokens: i64(500),
	}})

	client := s.authClient(t)
	analyticsWaitTokens(t, client, s.BaseURL+"/api/v1/analytics?period=30d", 500)

	for _, q := range []string{"period=24h&dimension=models", "period=7d&dimension=machines", "period=30d&dimension=projects"} {
		html := analyticsGetHTML(t, client, s.BaseURL+"/analytics?"+q)
		if !strings.Contains(html, `href="/tokedex#uncataloged-title"`) {
			t.Fatalf("/analytics?%s missing /tokedex#uncataloged-title link", q)
		}
	}
}

func TestAnalytics_F22_T2_UncatalogedLinkAbsentWhenTokedexDisabled(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f22-t2-1", EnableTokedex: false})
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "an-f22-dis",
		MachineID:   "m-dis",
		Provider:    "custom",
		Model:       "mystery-llm-9b",
		Tool:        "codex",
		TotalTokens: i64(500),
	}})

	client := s.authClient(t)
	analyticsWaitTokens(t, client, s.BaseURL+"/api/v1/analytics?period=30d", 500)

	html := analyticsGetHTML(t, client, s.BaseURL+"/analytics?period=30d")
	if !strings.Contains(html, "Catalog coverage") {
		t.Fatalf("expected Catalog coverage card text when unpriced tokens exist")
	}
	if strings.Contains(html, "/tokedex#uncataloged-title") || strings.Contains(html, `href="/tokedex`) {
		t.Fatalf("Tokedex disabled must not render /tokedex links on /analytics")
	}
	code, _, _ := analyticsGetResp(t, client, s.BaseURL+"/tokedex", nil)
	if code != http.StatusNotFound {
		t.Fatalf("GET /tokedex when disabled status=%d, want 404", code)
	}
}

func TestAnalytics_F22_T2_FilterExcludingUncatalogedModelsHidesCard(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f22-t2-2", EnableTokedex: true})
	s.ingestEvents(t, []ingestEvent{
		{EventID: "an-f22-f1", MachineID: "m1", Provider: "openai", Model: "gpt-5.4", Tool: "codex", InputTokens: i64(300), OutputTokens: i64(200), TotalTokens: i64(500)},
		{EventID: "an-f22-f2", MachineID: "m2", Provider: "custom", Model: "mystery-llm-9b", Tool: "codex", TotalTokens: i64(500)},
	})

	client := s.authClient(t)
	analyticsWaitTokens(t, client, s.BaseURL+"/api/v1/analytics?period=30d", 1000)

	htmlM1 := analyticsGetHTML(t, client, s.BaseURL+"/analytics?period=30d&machine=m1")
	if strings.Contains(htmlM1, "Catalog coverage") || strings.Contains(htmlM1, "/tokedex#uncataloged-title") {
		t.Fatalf("filtered priced-only machine=m1 should omit Catalog coverage card")
	}
	htmlM2 := analyticsGetHTML(t, client, s.BaseURL+"/analytics?period=30d&machine=m2")
	if !strings.Contains(htmlM2, "100.0%") || !strings.Contains(htmlM2, `href="/tokedex#uncataloged-title"`) {
		t.Fatalf("filtered uncataloged machine=m2 should show 100.0%% coverage card with link")
	}
}

func TestAnalytics_F22_T2_ZeroUsageEmptyAnalyticsOmitsCoverageCard(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f22-t2-3", EnableTokedex: true})
	client := s.authClient(t)

	html := analyticsGetHTML(t, client, s.BaseURL+"/analytics?period=30d")
	if strings.Contains(html, "Catalog coverage") || strings.Contains(html, "/tokedex#uncataloged-title") || strings.Contains(html, "NaN") {
		t.Fatalf("empty DB should omit Catalog coverage card without NaN")
	}
	tdCode, _, _ := analyticsGetResp(t, client, s.BaseURL+"/tokedex", nil)
	if tdCode != http.StatusOK {
		t.Fatalf("GET /tokedex when EnableTokedex=true status=%d, want 200", tdCode)
	}
}

func TestAnalytics_F22_T2_UncatalogedCardZeroTotalTokensEventHandledCleanly(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f22-t2-4", EnableTokedex: true})
	s.ingestEvents(t, []ingestEvent{{
		EventID:       "an-f22-nil-tot",
		MachineID:     "m-niltot",
		Provider:      "custom",
		Model:         "mystery-llm-9b",
		Tool:          "codex",
		TotalTokens:   nil,
		TokenAccuracy: "unknown",
	}})

	client := s.authClient(t)
	deadline := time.Now().Add(4 * time.Second)
	for {
		var payload analyticsAPIPayload
		getJSONWithClient(t, client, s.BaseURL+"/api/v1/analytics?period=30d", &payload)
		if payload.Summary.Events == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("/api/v1/analytics?period=30d summary.events = %d, want 1 (SWR timeout)", payload.Summary.Events)
		}
		time.Sleep(50 * time.Millisecond)
	}
	html := analyticsGetHTML(t, client, s.BaseURL+"/analytics?period=30d")
	if strings.Contains(html, "NaN") {
		t.Fatalf("/analytics rendered NaN on unknown-total uncataloged event")
	}
	tdCode, _, _ := analyticsGetResp(t, client, s.BaseURL+"/tokedex", nil)
	if tdCode != http.StatusOK {
		t.Fatalf("GET /tokedex status=%d, want 200", tdCode)
	}
}

func TestAnalytics_F22_T2_TokedexEnabledUncatalogedLinkRequiresAuth(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f22-t2-5-ing", Password: "td-password", EnableTokedex: true})

	for _, p := range []string{"/analytics", "/tokedex"} {
		resp, _ := s.doReq(t, http.MethodGet, p, nil, map[string]string{"Accept": "text/html"})
		if resp.StatusCode != http.StatusSeeOther || !strings.HasPrefix(resp.Header.Get("Location"), "/login") {
			t.Fatalf("unauthenticated %s status=%d, want 303 /login", p, resp.StatusCode)
		}
	}
	client := s.authClient(t)
	tdCode, _, _ := analyticsGetResp(t, client, s.BaseURL+"/tokedex", nil)
	if tdCode != http.StatusOK {
		t.Fatalf("authenticated /tokedex status=%d, want 200", tdCode)
	}
}

// =============================================================================
// Area 8: Feature 23 (from/to) — Custom Date Range on /analytics* (M5)
// =============================================================================

func TestAnalytics_F23_T1_CustomFromToRangeOnPageAPIAndTimeline(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f23-t1-1", Timezone: "UTC"})
	s.ingestEvents(t, []ingestEvent{
		{EventID: "an-f23-1", Timestamp: "2026-11-01T12:00:00Z", MachineID: "m-r", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(1000)},
		{EventID: "an-f23-2", Timestamp: "2026-11-05T12:00:00Z", MachineID: "m-r", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(200)},
		{EventID: "an-f23-3", Timestamp: "2026-11-10T12:00:00Z", MachineID: "m-r", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(300)},
		{EventID: "an-f23-4", Timestamp: "2026-11-15T12:00:00Z", MachineID: "m-r", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(2000)},
	})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 3500)

	var api analyticsAPIPayload
	getJSONWithClient(t, client, s.BaseURL+"/api/v1/analytics?from=2026-11-05&to=2026-11-10", &api)
	if api.StartDate != "2026-11-05" || api.EndDate != "2026-11-10" || len(api.Points) != 6 || api.Summary.Tokens != 500 {
		t.Fatalf("custom range /api/v1/analytics = (start=%s end=%s pts=%d tok=%d), want (2026-11-05, 2026-11-10, 6, 500)", api.StartDate, api.EndDate, len(api.Points), api.Summary.Tokens)
	}

	var tl analyticsTimelinePayload
	getJSONWithClient(t, client, s.BaseURL+"/api/v1/analytics/timeline?from=2026-11-05&to=2026-11-10", &tl)
	if tl.StartDate != "2026-11-05" || tl.EndDate != "2026-11-10" || len(tl.Points) != 6 {
		t.Fatalf("custom range /api/v1/analytics/timeline = %+v", tl)
	}

	html := analyticsGetHTML(t, client, s.BaseURL+"/analytics?from=2026-11-05&to=2026-11-10")
	if !strings.Contains(html, `data-count="500"`) || !strings.Contains(html, "2026-11-05") || !strings.Contains(html, "2026-11-10") {
		t.Fatalf("/analytics custom range page missing 500 or 2026-11-05..2026-11-10")
	}
}

func TestAnalytics_F23_T1_CustomFromToExportCSVAndJSON(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f23-t1-2", Timezone: "UTC"})
	s.ingestEvents(t, []ingestEvent{
		{EventID: "an-f23-e1", Timestamp: "2026-11-01T12:00:00Z", MachineID: "m-e", Project: "outside-proj", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(900)},
		{EventID: "an-f23-e2", Timestamp: "2026-11-07T12:00:00Z", MachineID: "m-e", Project: "inside-proj", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(450)},
	})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 1350)

	codeJ, hdrJ, jsonBody := analyticsGetResp(t, client, s.BaseURL+"/api/v1/analytics/export?from=2026-11-05&to=2026-11-10&dimension=projects", nil)
	if codeJ != http.StatusOK || !strings.Contains(hdrJ.Get("Content-Disposition"), "attachment;") {
		t.Fatalf("JSON export status=%d disp=%q", codeJ, hdrJ.Get("Content-Disposition"))
	}
	var exp analyticsAPIPayload
	if err := json.Unmarshal(jsonBody, &exp); err != nil {
		t.Fatalf("decode exported JSON: %v", err)
	}
	if exp.Summary.Tokens != 450 || len(exp.Breakdown) != 1 || exp.Breakdown[0].Name != "inside-proj" {
		t.Fatalf("exported JSON leaked outside range: %+v", exp)
	}

	codeC, hdrC, csvBody := analyticsGetResp(t, client, s.BaseURL+"/api/v1/analytics/export?from=2026-11-05&to=2026-11-10&dimension=projects&format=csv", nil)
	if codeC != http.StatusOK || !strings.Contains(hdrC.Get("Content-Disposition"), "attachment;") {
		t.Fatalf("CSV export status=%d disp=%q", codeC, hdrC.Get("Content-Disposition"))
	}
	rows, err := csv.NewReader(bytes.NewReader(csvBody)).ReadAll()
	if err != nil {
		t.Fatalf("parse CSV: %v", err)
	}
	if len(rows) != 2 || rows[1][2] != "inside-proj" || rows[1][3] != "450" {
		t.Fatalf("exported CSV unexpected rows: %+v", rows)
	}
}

func TestAnalytics_F23_T1_CustomFromToPreservedInDimensionLinksAndExportLinks(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f23-t1-3", Timezone: "UTC"})
	client := s.authClient(t)

	html := analyticsGetHTML(t, client, s.BaseURL+"/analytics?from=2026-11-05&to=2026-11-10&dimension=models")
	dimNav := analyticsSectionBetween(t, html, `class="dimension-nav"`, `</nav>`)
	for _, h := range analyticsExtractHrefs(dimNav) {
		if !strings.Contains(h, "from=2026-11-05") || !strings.Contains(h, "to=2026-11-10") {
			t.Fatalf("dimension link %q dropped custom from/to", h)
		}
	}
	filterForm := analyticsSectionBetween(t, html, `<form class="filter-form"`, `</form>`)
	if !strings.Contains(filterForm, `value="2026-11-05"`) || !strings.Contains(filterForm, `value="2026-11-10"`) {
		t.Fatalf("filter form missing preserved from/to inputs: %s", filterForm)
	}
	exportRe := regexp.MustCompile(`<a[^>]*href="([^"]*export[^"]*)"`)
	for _, m := range exportRe.FindAllStringSubmatch(html, -1) {
		h := stdhtml.UnescapeString(m[1])
		if !strings.Contains(h, "from=2026-11-05") || !strings.Contains(h, "to=2026-11-10") {
			t.Fatalf("export link %q dropped custom from/to", h)
		}
	}
}

func TestAnalytics_F23_T1_CustomFromToCombinedWithSecondaryFilters(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f23-t1-4", Timezone: "UTC"})
	s.ingestEvents(t, []ingestEvent{
		{EventID: "an-f23-c1", Timestamp: "2026-11-06T12:00:00Z", MachineID: "m1", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(300)},
		{EventID: "an-f23-c2", Timestamp: "2026-11-06T12:00:00Z", MachineID: "m2", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(500)},
		{EventID: "an-f23-c3", Timestamp: "2026-11-06T12:00:00Z", MachineID: "m1", Provider: "anthropic", Model: "claude-sonnet-4", Tool: "claude-code", TotalTokens: i64(700)},
		{EventID: "an-f23-c4", Timestamp: "2026-11-12T12:00:00Z", MachineID: "m1", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(900)},
	})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 2400)

	var api analyticsAPIPayload
	getJSONWithClient(t, client, s.BaseURL+"/api/v1/analytics?from=2026-11-05&to=2026-11-10&machine=m1&tool=codex", &api)
	if api.Summary.Tokens != 300 || api.Summary.Events != 1 {
		t.Fatalf("combined custom range + secondary filters = %+v, want tokens=300 events=1", api.Summary)
	}
}

func TestAnalytics_F23_T1_CustomFromToPreviousWindowComparison(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f23-t1-5", Timezone: "UTC"})
	// 6-day prior window: 2026-10-30..2026-11-04; 6-day current window: 2026-11-05..2026-11-10.
	s.ingestEvents(t, []ingestEvent{
		{EventID: "an-f23-p-old", Timestamp: "2026-11-01T12:00:00Z", MachineID: "m-p", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(400)},
		{EventID: "an-f23-p-cur", Timestamp: "2026-11-07T12:00:00Z", MachineID: "m-p", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(600)},
	})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 1000)

	var api analyticsAPIPayload
	getJSONWithClient(t, client, s.BaseURL+"/api/v1/analytics?from=2026-11-05&to=2026-11-10", &api)
	if api.Comparison == nil || api.Comparison.PreviousTokens != 400 {
		t.Fatalf("expected comparison.previous_tokens=400, got %+v", api.Comparison)
	}
	if api.Comparison.TokenChangePercent == nil || math.Abs(*api.Comparison.TokenChangePercent-50.0) > 1e-6 {
		t.Fatalf("expected +50.0%% change, got %v", api.Comparison.TokenChangePercent)
	}
}

func TestAnalytics_F23_T2_CustomFromToSingleDayInclusiveEndBoundary(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f23-t2-1", Timezone: "UTC"})
	s.ingestEvents(t, []ingestEvent{
		{EventID: "an-f23-b1", Timestamp: "2026-11-09T23:59:59Z", MachineID: "m-b", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(1000)},
		{EventID: "an-f23-b2", Timestamp: "2026-11-10T00:00:00Z", MachineID: "m-b", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(100)},
		{EventID: "an-f23-b3", Timestamp: "2026-11-10T23:59:59Z", MachineID: "m-b", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(200)},
		{EventID: "an-f23-b4", Timestamp: "2026-11-11T00:00:00Z", MachineID: "m-b", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(2000)},
	})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 3300)

	var api analyticsAPIPayload
	getJSONWithClient(t, client, s.BaseURL+"/api/v1/analytics?from=2026-11-10&to=2026-11-10", &api)
	if api.StartDate != "2026-11-10" || api.EndDate != "2026-11-10" || len(api.Points) != 1 || api.Summary.Tokens != 300 || api.Summary.Events != 2 {
		t.Fatalf("single-day boundary = (start=%s end=%s pts=%d tok=%d ev=%d), want (2026-11-10, 2026-11-10, 1, 300, 2)", api.StartDate, api.EndDate, len(api.Points), api.Summary.Tokens, api.Summary.Events)
	}
}

func TestAnalytics_F23_T2_CustomFromToTimezoneDayBoundary(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f23-t2-2", Timezone: "America/Toronto"})
	// On 2026-11-10, Toronto is EST (UTC-5):
	// 04:59:59Z = Nov 9 23:59:59 EST (excluded); 05:00:00Z = Nov 10 00:00:00 EST (included).
	s.ingestEvents(t, []ingestEvent{
		{EventID: "an-f23-tz1", Timestamp: "2026-11-10T04:59:59Z", MachineID: "m-tz", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(900)},
		{EventID: "an-f23-tz2", Timestamp: "2026-11-10T05:00:00Z", MachineID: "m-tz", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(250)},
	})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 1150)

	var api analyticsAPIPayload
	getJSONWithClient(t, client, s.BaseURL+"/api/v1/analytics?from=2026-11-10&to=2026-11-10", &api)
	if api.StartDate != "2026-11-10" || api.EndDate != "2026-11-10" || api.Summary.Tokens != 250 {
		t.Fatalf("Toronto day boundary tokens=%d start=%s end=%s, want 250 on 2026-11-10", api.Summary.Tokens, api.StartDate, api.EndDate)
	}
}

func TestAnalytics_F23_T2_CustomFromToDSTTransitionBoundary(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f23-t2-dst", Timezone: "America/Toronto"})
	// America/Toronto DST transitions in 2026:
	// Spring forward 2026-03-08 (23h day): 05:00Z (00:00 EST) .. 03:59:59Z next day (23:59:59 EDT).
	// Fall back 2026-11-01 (25h day): 04:00Z (00:00 EDT) .. 04:59:59Z next day (23:59:59 EST).
	s.ingestEvents(t, []ingestEvent{
		{EventID: "an-f23-spr-pre", Timestamp: "2026-03-08T04:59:59Z", MachineID: "m-dst", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(900)},
		{EventID: "an-f23-spr-in1", Timestamp: "2026-03-08T05:00:00Z", MachineID: "m-dst", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(120)},
		{EventID: "an-f23-spr-in2", Timestamp: "2026-03-09T03:30:00Z", MachineID: "m-dst", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(180)},
		{EventID: "an-f23-spr-post", Timestamp: "2026-03-09T04:00:00Z", MachineID: "m-dst", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(950)},

		{EventID: "an-f23-fall-pre", Timestamp: "2026-11-01T03:59:59Z", MachineID: "m-dst", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(800)},
		{EventID: "an-f23-fall-in1", Timestamp: "2026-11-01T04:00:00Z", MachineID: "m-dst", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(100)},
		{EventID: "an-f23-fall-in2", Timestamp: "2026-11-02T04:30:00Z", MachineID: "m-dst", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(200)},
		{EventID: "an-f23-fall-post", Timestamp: "2026-11-02T05:00:00Z", MachineID: "m-dst", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(850)},
	})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 4100)

	var spring analyticsAPIPayload
	getJSONWithClient(t, client, s.BaseURL+"/api/v1/analytics?from=2026-03-08&to=2026-03-08", &spring)
	if spring.StartDate != "2026-03-08" || spring.EndDate != "2026-03-08" || spring.Summary.Tokens != 300 || spring.Summary.Events != 2 {
		t.Fatalf("Toronto spring-forward DST day 2026-03-08 = %+v, want tokens=300 events=2", spring.Summary)
	}

	var fall analyticsAPIPayload
	getJSONWithClient(t, client, s.BaseURL+"/api/v1/analytics?from=2026-11-01&to=2026-11-01", &fall)
	if fall.StartDate != "2026-11-01" || fall.EndDate != "2026-11-01" || fall.Summary.Tokens != 300 || fall.Summary.Events != 2 {
		t.Fatalf("Toronto fall-back DST day 2026-11-01 = %+v, want tokens=300 events=2", fall.Summary)
	}
}

func TestAnalytics_F23_T2_CustomFromToInvertedOrMalformedDateRejectedOrSafe(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f23-t2-3", Timezone: "UTC"})
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "an-f23-safe",
		Timestamp:   "2026-11-08T12:00:00Z",
		MachineID:   "m-safe",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(100),
	}})
	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 100)

	// Confirm valid custom range works first (so test fails before M5 lands).
	var valid analyticsAPIPayload
	getJSONWithClient(t, client, s.BaseURL+"/api/v1/analytics?from=2026-11-05&to=2026-11-10", &valid)
	if valid.StartDate != "2026-11-05" || valid.EndDate != "2026-11-10" {
		t.Fatalf("custom from/to not supported yet: start=%s end=%s", valid.StartDate, valid.EndDate)
	}

	for _, q := range []string{
		"from=2026-11-10&to=2026-11-05",
		"from=not-a-date&to=2026-11-10",
		"from=2026-02-30&to=2026-03-01",
		"from=2026-11-05",
	} {
		code, _, body := analyticsGetResp(t, client, s.BaseURL+"/api/v1/analytics?"+q, nil)
		if code >= 500 {
			t.Fatalf("/api/v1/analytics?%s returned 5xx (%d): %s", q, code, string(body))
		}
		if code != http.StatusBadRequest && code != http.StatusOK {
			t.Fatalf("/api/v1/analytics?%s returned status %d", q, code)
		}
		if code == http.StatusOK {
			var res analyticsAPIPayload
			if err := json.Unmarshal(body, &res); err != nil {
				t.Fatalf("decode %s: %v", q, err)
			}
			if res.StartDate > res.EndDate {
				t.Fatalf("/api/v1/analytics?%s returned inverted dates %s > %s", q, res.StartDate, res.EndDate)
			}
		}
	}
}

func TestAnalytics_F23_T2_CustomFromToCacheKeyIsolation(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f23-t2-4", Timezone: "UTC"})
	s.ingestEvents(t, []ingestEvent{
		{EventID: "an-f23-k1", Timestamp: "2026-11-05T12:00:00Z", MachineID: "m-k", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(111)},
		{EventID: "an-f23-k2", Timestamp: "2026-11-07T12:00:00Z", MachineID: "m-k", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(222)},
	})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 333)

	var r1, r2 analyticsAPIPayload
	getJSONWithClient(t, client, s.BaseURL+"/api/v1/analytics?from=2026-11-05&to=2026-11-06", &r1)
	getJSONWithClient(t, client, s.BaseURL+"/api/v1/analytics?from=2026-11-07&to=2026-11-08", &r2)

	if r1.StartDate != "2026-11-05" || r1.Summary.Tokens != 111 {
		t.Fatalf("range 1 got %+v, want start=2026-11-05 tokens=111", r1.Summary)
	}
	if r2.StartDate != "2026-11-07" || r2.Summary.Tokens != 222 {
		t.Fatalf("range 2 cache collision: got %+v (start=%s), want start=2026-11-07 tokens=222", r2.Summary, r2.StartDate)
	}
}

func TestAnalytics_F23_T2_CustomFromToUnauthRejected(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "an-f23-t2-5-ing", Password: "f23-password"})

	respHTML, _ := s.doReq(t, http.MethodGet, "/analytics?from=2026-11-05&to=2026-11-10", nil, map[string]string{"Accept": "text/html"})
	if respHTML.StatusCode != http.StatusSeeOther || !strings.HasPrefix(respHTML.Header.Get("Location"), "/login") {
		t.Fatalf("unauthenticated HTML /analytics?from=... status=%d", respHTML.StatusCode)
	}
	for _, p := range []string{
		"/api/v1/analytics?from=2026-11-05&to=2026-11-10",
		"/api/v1/analytics/timeline?from=2026-11-05&to=2026-11-10",
		"/api/v1/analytics/export?from=2026-11-05&to=2026-11-10",
	} {
		resp, _ := s.doReq(t, http.MethodGet, p, nil, nil)
		if resp.StatusCode != http.StatusUnauthorized {
			t.Fatalf("unauthenticated %s status=%d, want 401", p, resp.StatusCode)
		}
	}
}
