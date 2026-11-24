//go:build e2e

package e2e_test

import (
	"bytes"
	"encoding/base64"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"
)

type itcCatalogPricing struct {
	Currency                string   `json:"currency"`
	Mode                    string   `json:"mode"`
	VerifiedAt              string   `json:"verified_at,omitempty"`
	Source                  string   `json:"source,omitempty"`
	InputPricePerMillion    *float64 `json:"input_per_million,omitempty"`
	CachedInputPricePerMill *float64 `json:"cached_input_per_million,omitempty"`
	CacheWritePricePerMill  *float64 `json:"cache_write_per_million,omitempty"`
	OutputPricePerMillion   *float64 `json:"output_per_million,omitempty"`
}

type itcCatalogEntry struct {
	ID          string            `json:"id"`
	Provider    string            `json:"provider"`
	DisplayName string            `json:"display_name"`
	Aliases     []string          `json:"aliases"`
	Tier        string            `json:"tier"`
	Pricing     itcCatalogPricing `json:"pricing"`
	UsageTokens int64             `json:"usage_tokens"`
	UsageEvents int64             `json:"usage_events"`
	Context     string            `json:"context"`
}

type itcUncatalogedModel struct {
	ID     string `json:"id"`
	Tokens int64  `json:"tokens"`
	Events int64  `json:"events"`
}

type itcCatalogSnapshot struct {
	SchemaVersion          string                `json:"schema_version"`
	Models                 []itcCatalogEntry     `json:"models"`
	Tiers                  []string              `json:"tiers"`
	Uncataloged            []itcUncatalogedModel `json:"uncataloged,omitempty"`
	ObservedTokens         int64                 `json:"observed_tokens,omitempty"`
	PricedTokens           int64                 `json:"priced_tokens,omitempty"`
	CatalogedUnknownTokens int64                 `json:"cataloged_unknown_tokens,omitempty"`
	UncatalogedModels      int                   `json:"uncataloged_models,omitempty"`
}

type itcInsightBucket struct {
	Label  string  `json:"label"`
	Tokens int64   `json:"tokens"`
	Events int64   `json:"events"`
	Share  float64 `json:"share"`
}

type itcInsightDataQuality struct {
	KnownEvents   int64   `json:"known_events"`
	UnknownEvents int64   `json:"unknown_events"`
	KnownTokens   int64   `json:"known_tokens"`
	EventCoverage float64 `json:"event_coverage"`
	Confidence    string  `json:"confidence"`
}

type itcInsightCard struct {
	ID           string `json:"id"`
	Category     string `json:"category"`
	Title        string `json:"title"`
	Narrative    string `json:"narrative"`
	Evidence     string `json:"evidence"`
	AnalyticsURL string `json:"analytics_url,omitempty"`
}

type itcInsightsPayload struct {
	Period      string                `json:"period"`
	Timezone    string                `json:"timezone"`
	DataQuality itcInsightDataQuality `json:"data_quality"`
	Hourly      []itcInsightBucket    `json:"hourly"`
	Weekdays    []itcInsightBucket    `json:"weekdays"`
	PeakHour    string                `json:"peak_hour"`
	PeakWeekday string                `json:"peak_weekday"`
	Cards       []itcInsightCard      `json:"cards"`
}

type itcSVGDocument struct {
	XMLName xml.Name   `xml:"svg"`
	Xmlns   string     `xml:"xmlns,attr"`
	ViewBox string     `xml:"viewBox,attr"`
	Width   string     `xml:"width,attr"`
	Height  string     `xml:"height,attr"`
	Attrs   []xml.Attr `xml:",any,attr"`
}

var (
	itcHourButtonRe      = regexp.MustCompile(`(?is)<button\b([^>]*class="[^"]*\bhour\b[^"]*"[^>]*)>`)
	itcAriaLiveOpenRe    = regexp.MustCompile(`(?is)<([a-z][a-z0-9]*)\b[^>]*\baria-live\s*=\s*(?:"polite"|'polite')[^>]*>`)
	itcPeakNoteOpenRe    = regexp.MustCompile(`(?is)<([a-z][a-z0-9]*)\b[^>]*class="(?:[^"]*\s)?peak-note(?:\s[^"]*)?"[^>]*>`)
	itcSROnlyTableRe     = regexp.MustCompile(`(?is)(?:<div\b[^>]*class="(?:[^"]*\s)?sr-only(?:\s[^"]*)?"[^>]*>\s*(<table\b.*?</table>)|(<table\b[^>]*class="(?:[^"]*\s)?sr-only(?:\s[^"]*)?"[^>]*>.*?</table>))`)
	itcTopbarBlockRe     = regexp.MustCompile(`(?is)<header\b[^>]*class="(?:[^"]*\s)?topbar(?:\s[^"]*)?"[^>]*>.*?</header>`)
	itcTierSectionOpenRe = regexp.MustCompile(`(?is)<section\b[^>]*class="(?:[^"]*\s)?tier(?:\s[^"]*)?"[^>]*>`)
	itcArticleRe         = regexp.MustCompile(`(?is)<article\b[^>]*>.*?</article>`)
	itcZeroTokensRe      = regexp.MustCompile(`(?i)\b0 tokens\b`)
	itcNavBlockRe        = regexp.MustCompile(`(?s)<nav\b[^>]*class="[^"]*\bnav\b[^"]*"[^>]*>(.*?)</nav>`)
	itcKnownWordRe       = regexp.MustCompile(`(?i)\bknown\b`)
	itcPositiveTabIdxRe  = regexp.MustCompile(`(?i)\btabindex="[1-9][0-9]*"`)
	itcSVGGeometryRe     = regexp.MustCompile(`(?i)<(rect|path|polyline|polygon|line)\b`)
	itcCardStageRe       = regexp.MustCompile(`(?i)(egg|bitling|bytelet|prompt imp|cache cub|vector lynx|code caster|agent hound|context dragon|token titan)`)
)

func itcDo(t *testing.T, client *http.Client, method, fullURL string, body []byte, headers map[string]string) (int, http.Header, []byte) {
	t.Helper()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequest(method, fullURL, reader)
	if err != nil {
		t.Fatalf("new request %s %s: %v", method, fullURL, err)
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

func itcGetHTML(t *testing.T, client *http.Client, fullURL string) string {
	t.Helper()
	code, _, body := itcDo(t, client, http.MethodGet, fullURL, nil, map[string]string{"Accept": "text/html"})
	if code != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200\nbody: %s", fullURL, code, string(body))
	}
	return string(body)
}

func itcWaitCatalog(t *testing.T, client *http.Client, fullURL string, pred func(itcCatalogSnapshot) bool) itcCatalogSnapshot {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last itcCatalogSnapshot
	for {
		var snap itcCatalogSnapshot
		getJSONWithClient(t, client, fullURL, &snap)
		last = snap
		if pred(snap) {
			return snap
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s SWR condition (observed=%d uncataloged=%d)", fullURL, last.ObservedTokens, last.UncatalogedModels)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func itcWaitInsights(t *testing.T, client *http.Client, fullURL string, pred func(itcInsightsPayload) bool) itcInsightsPayload {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last itcInsightsPayload
	for {
		var ins itcInsightsPayload
		getJSONWithClient(t, client, fullURL, &ins)
		last = ins
		if pred(ins) {
			return ins
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s SWR condition (known_tokens=%d known_events=%d)", fullURL, last.DataQuality.KnownTokens, last.DataQuality.KnownEvents)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func itcWaitHTML(t *testing.T, client *http.Client, fullURL string, pred func(string) bool) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last string
	for {
		body := itcGetHTML(t, client, fullURL)
		last = body
		if pred(body) {
			return body
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for HTML condition on %s (len=%d)", fullURL, len(last))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func itcPostAliases(t *testing.T, s *testServer, client *http.Client, form url.Values) {
	t.Helper()
	settingsHTML := itcGetHTML(t, client, s.BaseURL+"/settings")
	form.Set("csrf_token", extractCSRFToken(t, []byte(settingsHTML)))
	code, _, body := itcDo(t, client, http.MethodPost, s.BaseURL+"/settings/aliases", []byte(form.Encode()), map[string]string{
		"Content-Type": "application/x-www-form-urlencoded",
		"Origin":       s.BaseURL,
		"Referer":      s.BaseURL + "/settings",
	})
	if code != http.StatusOK {
		t.Fatalf("POST /settings/aliases status = %d: %s", code, string(body))
	}
}

func itcExtractSection(body, start, end string) string {
	idx := strings.Index(body, start)
	if idx < 0 {
		return ""
	}
	rest := body[idx:]
	if end == "" {
		return rest
	}
	if endIdx := strings.Index(rest, end); endIdx >= 0 {
		return rest[:endIdx]
	}
	return rest
}

func itcBalancedTagBlock(rest, tag string) string {
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

func itcExtractBalancedAriaLive(fragment string) (string, bool) {
	loc := itcAriaLiveOpenRe.FindStringSubmatchIndex(fragment)
	if loc == nil {
		return "", false
	}
	tag := strings.ToLower(fragment[loc[2]:loc[3]])
	block := itcBalancedTagBlock(fragment[loc[0]:], tag)
	if block == "" {
		return "", false
	}
	return block, true
}

func itcExtractPeakSummary(htmlBody string) string {
	var out strings.Builder
	for _, loc := range itcPeakNoteOpenRe.FindAllStringSubmatchIndex(htmlBody, -1) {
		tag := strings.ToLower(htmlBody[loc[2]:loc[3]])
		if block := itcBalancedTagBlock(htmlBody[loc[0]:], tag); block != "" {
			out.WriteString(block)
			out.WriteByte('\n')
		}
	}
	rhythmSec := itcExtractSection(htmlBody, "24-hour rhythm", "Weekday distribution")
	if live, ok := itcExtractBalancedAriaLive(rhythmSec); ok {
		out.WriteString(live)
		out.WriteByte('\n')
	}
	return out.String()
}

func itcExtractSROnlyTable(section string) (string, bool) {
	m := itcSROnlyTableRe.FindStringSubmatch(section)
	if len(m) < 3 {
		return "", false
	}
	if m[1] != "" {
		return m[1], true
	}
	if m[2] != "" {
		return m[2], true
	}
	return "", false
}

func itcStripTopbar(htmlBody string) string {
	return itcTopbarBlockRe.ReplaceAllString(htmlBody, "")
}

func itcStripTierSections(htmlBody string) string {
	body := itcStripTopbar(htmlBody)
	for {
		loc := itcTierSectionOpenRe.FindStringIndex(body)
		if loc == nil {
			break
		}
		block := itcBalancedTagBlock(body[loc[0]:], "section")
		if block == "" {
			break
		}
		body = body[:loc[0]] + body[loc[0]+len(block):]
	}
	return body
}

func itcExtractModelArticle(htmlBody, modelID string) (string, bool) {
	for _, art := range itcArticleRe.FindAllString(htmlBody, -1) {
		if strings.Contains(art, `title="`+modelID+`"`) ||
			strings.Contains(art, ">"+modelID+"<") ||
			strings.Contains(art, "/tokedex/"+modelID) {
			return art, true
		}
	}
	return "", false
}

func itcExtractWhatIfSection(htmlBody string) string {
	nonTier := itcStripTierSections(htmlBody)
	openRe := regexp.MustCompile(`(?is)<(section|article|aside|details|div)\b[^>]*>`)
	var fallback string
	for _, loc := range openRe.FindAllStringSubmatchIndex(nonTier, -1) {
		openTag := strings.ToLower(nonTier[loc[0]:loc[1]])
		if strings.Contains(openTag, "filter-panel") {
			continue
		}
		tag := strings.ToLower(nonTier[loc[2]:loc[3]])
		block := itcBalancedTagBlock(nonTier[loc[0]:], tag)
		if block == "" {
			continue
		}
		lower := strings.ToLower(block)
		if !strings.Contains(lower, "what-if") && !strings.Contains(lower, "what if") {
			continue
		}
		if strings.Contains(lower, "<main") || strings.Contains(lower, `class="heading"`) || strings.Contains(lower, "<footer") {
			continue
		}
		if strings.Contains(openTag, "whatif") || strings.Contains(openTag, "what-if") {
			return block
		}
		if fallback == "" {
			fallback = block
		}
	}
	return fallback
}

func itcValidateSVGXML(t *testing.T, svgBytes []byte) itcSVGDocument {
	t.Helper()
	var doc itcSVGDocument
	if err := xml.Unmarshal(svgBytes, &doc); err != nil {
		t.Fatalf("invalid SVG XML: %v\nsvg:\n%s", err, string(svgBytes))
	}
	dec := xml.NewDecoder(bytes.NewReader(svgBytes))
	for {
		_, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("xml.NewDecoder token error: %v\nsvg:\n%s", err, string(svgBytes))
		}
	}
	return doc
}

func itcExtractSVGText(t *testing.T, svgBytes []byte) string {
	t.Helper()
	dec := xml.NewDecoder(bytes.NewReader(svgBytes))
	var out strings.Builder
	textDepth := 0
	for {
		tok, err := dec.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatalf("xml.NewDecoder error extracting SVG text: %v\nsvg:\n%s", err, string(svgBytes))
		}
		switch v := tok.(type) {
		case xml.StartElement:
			local := strings.ToLower(v.Name.Local)
			if local == "text" || local == "tspan" {
				textDepth++
			}
		case xml.EndElement:
			local := strings.ToLower(v.Name.Local)
			if (local == "text" || local == "tspan") && textDepth > 0 {
				textDepth--
				out.WriteByte(' ')
			}
		case xml.CharData:
			if textDepth > 0 {
				out.Write(v)
			}
		}
	}
	return out.String()
}

// --- Area A: Feature 10 (SWR Caches on /tokedex & /insights) ---

func TestSWR_F10_T1_TokedexAndCatalogAPIRefreshAfterIngest(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{
		IngestToken:   "itc-swr-t1-1",
		EnableTokedex: true,
	})
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "itc-swr-cat-1",
		MachineID:   "m-cat-1",
		SessionID:   "s-cat-1",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(250),
	}})
	client := s.authClient(t)

	// Warm /tokedex, /api/v1/catalog, and /api/v1/models.
	itcWaitCatalog(t, client, s.BaseURL+"/api/v1/catalog", func(c itcCatalogSnapshot) bool {
		return c.ObservedTokens == 250
	})
	var models []itcCatalogEntry
	getJSONWithClient(t, client, s.BaseURL+"/api/v1/models", &models)
	_ = itcGetHTML(t, client, s.BaseURL+"/tokedex")

	// Ingest cataloged + uncataloged model usage.
	s.ingestEvents(t, []ingestEvent{
		{
			EventID:     "itc-swr-cat-2",
			MachineID:   "m-cat-1",
			SessionID:   "s-cat-2",
			Provider:    "openai",
			Model:       "gpt-5.4",
			Tool:        "codex",
			TotalTokens: i64(750),
		},
		{
			EventID:     "itc-swr-cat-3",
			MachineID:   "m-cat-1",
			SessionID:   "s-cat-3",
			Provider:    "custom",
			Model:       "mystery-model-swr",
			Tool:        "codex",
			TotalTokens: i64(500),
		},
	})

	snap := itcWaitCatalog(t, client, s.BaseURL+"/api/v1/catalog", func(c itcCatalogSnapshot) bool {
		return c.ObservedTokens == 1500 && c.UncatalogedModels == 1
	})
	if snap.PricedTokens != 1000 {
		t.Fatalf("PricedTokens = %d, want 1000", snap.PricedTokens)
	}

	models = nil
	getJSONWithClient(t, client, s.BaseURL+"/api/v1/models", &models)
	var gptTokens int64
	for _, m := range models {
		if m.ID == "gpt-5.4" {
			gptTokens = m.UsageTokens
		}
	}
	if gptTokens != 1000 {
		t.Fatalf("/api/v1/models gpt-5.4 usage_tokens = %d, want 1000", gptTokens)
	}

	tokedexHTML := itcWaitHTML(t, client, s.BaseURL+"/tokedex", func(body string) bool {
		return strings.Contains(body, "mystery-model-swr") && strings.Contains(body, "1,000")
	})
	if !strings.Contains(tokedexHTML, "mystery-model-swr") {
		t.Fatalf("/tokedex did not converge to mystery-model-swr")
	}
}

func TestSWR_F10_T1_InsightsPageAndAPIRefreshAfterIngest(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "itc-swr-t1-2"})
	now := time.Now().UTC()
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "itc-swr-ins-1",
		Timestamp:   now.Add(-time.Hour).Format(time.RFC3339Nano),
		MachineID:   "m-ins-1",
		SessionID:   "s-ins-1",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(400),
	}})
	client := s.authClient(t)

	itcWaitInsights(t, client, s.BaseURL+"/api/v1/insights", func(ins itcInsightsPayload) bool {
		return ins.DataQuality.KnownTokens == 400 && ins.DataQuality.KnownEvents == 1
	})
	_ = itcGetHTML(t, client, s.BaseURL+"/insights")

	s.ingestEvents(t, []ingestEvent{{
		EventID:     "itc-swr-ins-2",
		Timestamp:   now.Add(-30 * time.Minute).Format(time.RFC3339Nano),
		MachineID:   "m-ins-1",
		SessionID:   "s-ins-2",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(600),
	}})

	ins := itcWaitInsights(t, client, s.BaseURL+"/api/v1/insights", func(i itcInsightsPayload) bool {
		return i.DataQuality.KnownTokens == 1000 && i.DataQuality.KnownEvents == 2
	})
	if len(ins.Hourly) != 24 || len(ins.Weekdays) != 7 {
		t.Fatalf("unexpected hourly (%d) / weekday (%d) bucket lengths", len(ins.Hourly), len(ins.Weekdays))
	}
	insHTML := itcWaitHTML(t, client, s.BaseURL+"/insights", func(body string) bool {
		return strings.Contains(body, "1,000")
	})
	if !strings.Contains(insHTML, "1,000") {
		t.Fatalf("/insights HTML did not converge to 1,000 tokens")
	}
}

func TestSWR_F10_T1_InsightsFilterKeysCachedIndependently(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "itc-swr-t1-3"})
	now := time.Now().UTC()
	s.ingestEvents(t, []ingestEvent{
		{
			EventID:     "itc-ins-k1",
			Timestamp:   now.Add(-2 * time.Hour).Format(time.RFC3339Nano),
			MachineID:   "m-ins-k1",
			SessionID:   "s-k1",
			Provider:    "openai",
			Model:       "gpt-5.4",
			Tool:        "codex",
			TotalTokens: i64(333),
		},
		{
			EventID:     "itc-ins-k2",
			Timestamp:   now.Add(-time.Hour).Format(time.RFC3339Nano),
			MachineID:   "m-ins-k2",
			SessionID:   "s-k2",
			Provider:    "anthropic",
			Model:       "claude-sonnet-4",
			Tool:        "claude-code",
			TotalTokens: i64(777),
		},
	})
	client := s.authClient(t)

	ins1 := itcWaitInsights(t, client, s.BaseURL+"/api/v1/insights?period=7d&machine=m-ins-k1", func(i itcInsightsPayload) bool {
		return i.DataQuality.KnownTokens == 333
	})
	ins2 := itcWaitInsights(t, client, s.BaseURL+"/api/v1/insights?period=7d&machine=m-ins-k2", func(i itcInsightsPayload) bool {
		return i.DataQuality.KnownTokens == 777
	})
	if ins1.DataQuality.KnownTokens != 333 || ins2.DataQuality.KnownTokens != 777 {
		t.Fatalf("insights cache keys collided: m1=%d m2=%d", ins1.DataQuality.KnownTokens, ins2.DataQuality.KnownTokens)
	}
}

func TestSWR_F10_T1_SettingsAliasUpdateImmediatelyVisibleOnInsights(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "itc-swr-t1-4"})
	now := time.Now().UTC()
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "itc-ins-al-1",
		Timestamp:   now.Add(-time.Hour).Format(time.RFC3339Nano),
		MachineID:   "m-ins-alias-raw",
		SessionID:   "s-ins-al-1",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(500),
	}})
	client := s.authClient(t)
	itcWaitInsights(t, client, s.BaseURL+"/api/v1/insights", func(i itcInsightsPayload) bool {
		return i.DataQuality.KnownTokens == 500
	})
	// Warm /insights HTML.
	_ = itcGetHTML(t, client, s.BaseURL+"/insights")

	form := url.Values{}
	form.Set("model_identity", "gpt-5.4")
	form.Set("model_alias", "Insights Immediate Model")
	form.Set("machine_identity", "m-ins-alias-raw")
	form.Set("machine_alias", "Insights Immediate Box")
	itcPostAliases(t, s, client, form)

	// Immediately fetch /insights without waiting for TTL.
	insHTML := itcGetHTML(t, client, s.BaseURL+"/insights")
	if !strings.Contains(insHTML, "Insights Immediate Model") || !strings.Contains(insHTML, "Insights Immediate Box") {
		t.Fatalf("/insights did not reflect updated aliases immediately")
	}
}

func TestSWR_F10_T1_TokedexCatalogAPIStillOpenWhenTokedexUIGated(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{
		IngestToken:   "itc-swr-t1-5",
		EnableTokedex: false,
	})
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "itc-gated-cat-1",
		MachineID:   "m-gated-cat",
		SessionID:   "s-gated-cat",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(888),
	}})
	client := s.authClient(t)

	// UI stays 404.
	uiCode, _, _ := itcDo(t, client, http.MethodGet, s.BaseURL+"/tokedex", nil, map[string]string{"Accept": "text/html"})
	if uiCode != http.StatusNotFound {
		t.Fatalf("GET /tokedex when EnableTokedex=false status = %d, want 404", uiCode)
	}

	// API stays 200 and refreshes via SWR.
	snap := itcWaitCatalog(t, client, s.BaseURL+"/api/v1/catalog", func(c itcCatalogSnapshot) bool {
		return c.ObservedTokens == 888
	})
	if snap.ObservedTokens != 888 {
		t.Fatalf("/api/v1/catalog observed = %d, want 888", snap.ObservedTokens)
	}
	var models []itcCatalogEntry
	getJSONWithClient(t, client, s.BaseURL+"/api/v1/models", &models)
	if len(models) == 0 {
		t.Fatalf("/api/v1/models returned empty slice")
	}
}

func TestSWR_F10_T2_CoalescingInsideTwoSecondWindowServesNonBlocking(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{
		IngestToken:   "itc-swr-t2-1",
		EnableTokedex: true,
	})
	client := s.authClient(t)
	// Warm empty caches.
	itcWaitCatalog(t, client, s.BaseURL+"/api/v1/catalog", func(c itcCatalogSnapshot) bool { return c.ObservedTokens == 0 })
	itcWaitInsights(t, client, s.BaseURL+"/api/v1/insights", func(i itcInsightsPayload) bool { return i.DataQuality.KnownTokens == 0 })

	now := time.Now().UTC()
	for i := 0; i < 5; i++ {
		s.ingestEvents(t, []ingestEvent{{
			EventID:     fmt.Sprintf("itc-coal-%d", i),
			Timestamp:   now.Add(-time.Duration(i+1) * time.Minute).Format(time.RFC3339Nano),
			MachineID:   "m-coal",
			SessionID:   fmt.Sprintf("s-coal-%d", i),
			Provider:    "openai",
			Model:       "gpt-5.4",
			Tool:        "codex",
			TotalTokens: i64(100),
		}})
		cCat, _, _ := itcDo(t, client, http.MethodGet, s.BaseURL+"/api/v1/catalog", nil, nil)
		cIns, _, _ := itcDo(t, client, http.MethodGet, s.BaseURL+"/api/v1/insights", nil, nil)
		if cCat != http.StatusOK || cIns != http.StatusOK {
			t.Fatalf("burst read %d failed: catalog=%d insights=%d", i, cCat, cIns)
		}
	}

	itcWaitCatalog(t, client, s.BaseURL+"/api/v1/catalog", func(c itcCatalogSnapshot) bool { return c.ObservedTokens == 500 })
	itcWaitInsights(t, client, s.BaseURL+"/api/v1/insights", func(i itcInsightsPayload) bool { return i.DataQuality.KnownTokens == 500 })
}

func TestSWR_F10_T2_RejectedOrDuplicateIngestDoesNotCorruptCaches(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "itc-swr-t2-2"})
	ev := ingestEvent{
		EventID:     "itc-dup-1",
		MachineID:   "m-dup",
		SessionID:   "s-dup",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(300),
	}
	s.ingestEvents(t, []ingestEvent{ev})
	client := s.authClient(t)
	itcWaitCatalog(t, client, s.BaseURL+"/api/v1/catalog", func(c itcCatalogSnapshot) bool { return c.ObservedTokens == 300 })
	itcWaitInsights(t, client, s.BaseURL+"/api/v1/insights", func(i itcInsightsPayload) bool { return i.DataQuality.KnownTokens == 300 })

	// Submit duplicate + malformed 400 batch.
	dupRes := s.ingestEvents(t, []ingestEvent{ev})
	if dupRes.Duplicates != 1 {
		t.Fatalf("expected duplicate=1, got %+v", dupRes)
	}
	badCode, _, _ := itcDo(t, s.Client, http.MethodPost, s.BaseURL+"/api/v1/events/batch", []byte(`{"events":[{"event_id":""}]}`), map[string]string{
		"Content-Type":  "application/json",
		"Authorization": "Bearer " + s.IngestToken,
	})
	if badCode != http.StatusBadRequest {
		t.Fatalf("malformed ingest status = %d, want 400", badCode)
	}

	time.Sleep(2200 * time.Millisecond)
	cat := itcWaitCatalog(t, client, s.BaseURL+"/api/v1/catalog", func(c itcCatalogSnapshot) bool { return c.ObservedTokens == 300 })
	ins := itcWaitInsights(t, client, s.BaseURL+"/api/v1/insights", func(i itcInsightsPayload) bool { return i.DataQuality.KnownTokens == 300 })
	if cat.ObservedTokens != 300 || ins.DataQuality.KnownTokens != 300 {
		t.Fatalf("cache corrupted: catalog=%d insights=%d", cat.ObservedTokens, ins.DataQuality.KnownTokens)
	}
}

func TestSWR_F10_T2_InsightsLRUBoundBeyond64DistinctFilterKeys(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "itc-swr-t2-3"})
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "itc-lru-1",
		MachineID:   "m-lru-00",
		SessionID:   "s-lru-00",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(123),
	}})
	client := s.authClient(t)

	for i := 0; i < 72; i++ {
		code, _, _ := itcDo(t, client, http.MethodGet, fmt.Sprintf("%s/api/v1/insights?machine=m-lru-%02d", s.BaseURL, i), nil, nil)
		if code != http.StatusOK {
			t.Fatalf("insights LRU filter %d status = %d", i, code)
		}
	}
	ins := itcWaitInsights(t, client, s.BaseURL+"/api/v1/insights?machine=m-lru-00", func(i itcInsightsPayload) bool {
		return i.DataQuality.KnownTokens == 123
	})
	if ins.DataQuality.KnownTokens != 123 {
		t.Fatalf("evicted key returned %d, want 123", ins.DataQuality.KnownTokens)
	}
}

func TestSWR_F10_T2_EmptyDatabaseWarmThenFirstIngest(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{
		IngestToken:   "itc-swr-t2-4",
		EnableTokedex: true,
	})
	client := s.authClient(t)

	_ = itcGetHTML(t, client, s.BaseURL+"/tokedex")
	_ = itcGetHTML(t, client, s.BaseURL+"/insights")
	itcWaitCatalog(t, client, s.BaseURL+"/api/v1/catalog", func(c itcCatalogSnapshot) bool { return c.ObservedTokens == 0 })
	itcWaitInsights(t, client, s.BaseURL+"/api/v1/insights", func(i itcInsightsPayload) bool { return i.DataQuality.KnownTokens == 0 })

	s.ingestEvents(t, []ingestEvent{{
		EventID:     "itc-empty-first",
		MachineID:   "m-ef",
		SessionID:   "s-ef",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(222),
	}})

	itcWaitCatalog(t, client, s.BaseURL+"/api/v1/catalog", func(c itcCatalogSnapshot) bool { return c.ObservedTokens == 222 })
	itcWaitInsights(t, client, s.BaseURL+"/api/v1/insights", func(i itcInsightsPayload) bool { return i.DataQuality.KnownTokens == 222 })
}

func TestSWR_F10_T2_InsightsCacheIsolatedFromQueryDimensionAndPeriod(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "itc-swr-t2-5"})
	now := time.Now().UTC()
	s.ingestEvents(t, []ingestEvent{
		{
			EventID:     "itc-iso-recent",
			Timestamp:   now.Add(-time.Hour).Format(time.RFC3339Nano),
			MachineID:   "m-iso",
			SessionID:   "s-iso-rec",
			Provider:    "openai",
			Model:       "gpt-5.4",
			Tool:        "codex",
			TotalTokens: i64(100),
		},
		{
			EventID:     "itc-iso-old",
			Timestamp:   now.AddDate(0, 0, -40).Format(time.RFC3339Nano),
			MachineID:   "m-iso",
			SessionID:   "s-iso-old",
			Provider:    "openai",
			Model:       "gpt-5.4",
			Tool:        "codex",
			TotalTokens: i64(900),
		},
	})
	client := s.authClient(t)

	ins24h := itcWaitInsights(t, client, s.BaseURL+"/api/v1/insights?period=24h", func(i itcInsightsPayload) bool {
		return i.DataQuality.KnownTokens == 100
	})
	insAll := itcWaitInsights(t, client, s.BaseURL+"/api/v1/insights?period=all", func(i itcInsightsPayload) bool {
		return i.DataQuality.KnownTokens == 1000
	})
	if ins24h.DataQuality.KnownTokens != 100 || insAll.DataQuality.KnownTokens != 1000 {
		t.Fatalf("period isolation failed: 24h=%d all=%d", ins24h.DataQuality.KnownTokens, insAll.DataQuality.KnownTokens)
	}
}

// --- Area B: Feature 17 (/insights Roving Tabindex & Privacy) ---

func TestInsights_F17_T1_24HourRhythmButtonsAndSingleRovingTabStop(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{
		IngestToken: "itc-ins-t1-1",
		Timezone:    "UTC",
	})
	now := time.Now().UTC()
	dayBase := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, time.UTC)
	if dayBase.Add(14 * time.Hour).After(now) {
		dayBase = dayBase.AddDate(0, 0, -1)
	}
	s.ingestEvents(t, []ingestEvent{
		{EventID: "ins-h-1", Timestamp: dayBase.Add(9 * time.Hour).Format(time.RFC3339Nano), MachineID: "m-ins", SessionID: "s1", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(200)},
		{EventID: "ins-h-2", Timestamp: dayBase.Add(14 * time.Hour).Format(time.RFC3339Nano), MachineID: "m-ins", SessionID: "s2", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(1500)},
	})
	client := s.authClient(t)
	ins := itcWaitInsights(t, client, s.BaseURL+"/api/v1/insights", func(i itcInsightsPayload) bool {
		return i.DataQuality.KnownTokens == 1700
	})

	htmlBody := itcGetHTML(t, client, s.BaseURL+"/insights")
	rhythmSec := itcExtractSection(htmlBody, "24-hour rhythm", "Weekday distribution")
	if rhythmSec == "" {
		t.Fatalf("/insights missing 24-hour rhythm section")
	}
	if itcPositiveTabIdxRe.MatchString(rhythmSec) {
		t.Fatalf("24-hour rhythm section contains forbidden positive tabindex")
	}

	buttons := itcHourButtonRe.FindAllStringSubmatch(rhythmSec, -1)
	if len(buttons) != 24 {
		t.Fatalf("24-hour rhythm <button class=\"hour\"> count = %d, want 24\nsec=%s", len(buttons), rhythmSec)
	}
	var zeroCount, negOneCount int
	var activeAttrs string
	for _, b := range buttons {
		attrs := b[1]
		if !strings.Contains(attrs, `type="button"`) {
			t.Fatalf("hour button missing type=\"button\": %s", attrs)
		}
		if !strings.Contains(attrs, `aria-label="`) {
			t.Fatalf("hour button missing aria-label: %s", attrs)
		}
		if strings.Contains(attrs, `tabindex="0"`) {
			zeroCount++
			activeAttrs = attrs
		} else if strings.Contains(attrs, `tabindex="-1"`) {
			negOneCount++
		} else {
			t.Fatalf("hour button missing roving tabindex=0/-1: %s", attrs)
		}
	}
	if zeroCount != 1 || negOneCount != 23 {
		t.Fatalf("roving tabindex counts: zero=%d negOne=%d, want 1 and 23", zeroCount, negOneCount)
	}
	if ins.PeakHour != "" && !strings.Contains(activeAttrs, ins.PeakHour) {
		t.Fatalf("initial tabindex=0 button (%s) does not match PeakHour %q", activeAttrs, ins.PeakHour)
	}
}

func TestInsights_F17_T1_AriaLiveReadoutAndKeyboardNavScript(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "itc-ins-t1-2", Timezone: "UTC"})
	now := time.Now().UTC()
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "ins-live-1",
		Timestamp:   now.Add(-time.Hour).Format(time.RFC3339Nano),
		MachineID:   "m-live",
		SessionID:   "s-live",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(850),
	}})
	client := s.authClient(t)
	ins := itcWaitInsights(t, client, s.BaseURL+"/api/v1/insights", func(i itcInsightsPayload) bool {
		return i.DataQuality.KnownTokens == 850
	})

	htmlBody := itcGetHTML(t, client, s.BaseURL+"/insights")
	rhythmSec := itcExtractSection(htmlBody, "24-hour rhythm", "Weekday distribution")
	liveReadout, ok := itcExtractBalancedAriaLive(rhythmSec)
	if !ok {
		t.Fatalf("24-hour rhythm section missing aria-live=\"polite\" readout element: %s", rhythmSec)
	}
	if ins.PeakHour != "" && !strings.Contains(liveReadout, ins.PeakHour) {
		t.Fatalf("aria-live readout %q missing PeakHour %q", liveReadout, ins.PeakHour)
	}
	if !strings.Contains(liveReadout, "850") {
		t.Fatalf("aria-live readout %q missing peak token count 850", liveReadout)
	}

	for _, keyName := range []string{"ArrowRight", "ArrowLeft", "Home", "End", "tabindex"} {
		if !strings.Contains(htmlBody, keyName) {
			t.Fatalf("/insights script missing roving keyboard navigation token %q", keyName)
		}
	}
}

func TestInsights_F17_T1_SROnlyTableAccessibilityParity(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "itc-ins-t1-3", Timezone: "UTC"})
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "ins-sr-1",
		Timestamp:   time.Now().UTC().Add(-time.Hour).Format(time.RFC3339Nano),
		MachineID:   "m-sr",
		SessionID:   "s-sr",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(420),
	}})
	client := s.authClient(t)
	itcWaitInsights(t, client, s.BaseURL+"/api/v1/insights", func(i itcInsightsPayload) bool {
		return i.DataQuality.KnownTokens == 420
	})

	htmlBody := itcGetHTML(t, client, s.BaseURL+"/insights")
	rhythmSec := itcExtractSection(htmlBody, "24-hour rhythm", "Weekday distribution")
	tbl, ok := itcExtractSROnlyTable(rhythmSec)
	if !ok {
		t.Fatalf("24-hour rhythm missing .sr-only accessible data <table>: %s", rhythmSec)
	}
	if !strings.Contains(tbl, `<th scope="col"`) {
		t.Fatalf(".sr-only rhythm table missing <th scope=\"col\">")
	}
	if !strings.Contains(tbl, "aria-label=") && !strings.Contains(tbl, "<caption") {
		t.Fatalf(".sr-only rhythm table missing aria-label or <caption>")
	}
	if !strings.Contains(tbl, "420") {
		t.Fatalf(".sr-only rhythm table missing ingested token count 420: %s", tbl)
	}
	for h := 0; h < 24; h++ {
		label := fmt.Sprintf("%02d:00", h)
		if !strings.Contains(tbl, label) {
			t.Fatalf(".sr-only rhythm table missing hour bucket %q", label)
		}
	}
}

func TestInsights_F17_T1_WeekdayDistributionAndPeakNotesMatchAPI(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{
		IngestToken: "itc-ins-t1-4",
		Timezone:    "America/New_York",
	})
	now := time.Now().UTC()
	s.ingestEvents(t, []ingestEvent{
		{
			EventID:     "ins-wd-1",
			Timestamp:   now.Add(-2 * time.Hour).Format(time.RFC3339Nano),
			MachineID:   "m-wd",
			SessionID:   "s-wd-1",
			Provider:    "openai",
			Model:       "gpt-5.4",
			Tool:        "codex",
			TotalTokens: i64(900),
		},
		{
			EventID:     "ins-wd-2",
			Timestamp:   now.AddDate(0, 0, -2).Format(time.RFC3339Nano),
			MachineID:   "m-wd",
			SessionID:   "s-wd-2",
			Provider:    "openai",
			Model:       "gpt-5.4",
			Tool:        "codex",
			TotalTokens: i64(100),
		},
	})
	client := s.authClient(t)
	ins := itcWaitInsights(t, client, s.BaseURL+"/api/v1/insights", func(i itcInsightsPayload) bool {
		return i.DataQuality.KnownTokens == 1000
	})

	htmlBody := itcGetHTML(t, client, s.BaseURL+"/insights")
	peakSummary := itcExtractPeakSummary(htmlBody)
	if peakSummary == "" {
		t.Fatalf("/insights missing .peak-note / peak summary elements")
	}
	if !strings.Contains(peakSummary, ins.PeakHour) || !strings.Contains(peakSummary, ins.PeakWeekday) {
		t.Fatalf("/insights peak summary %q missing PeakHour %q or PeakWeekday %q", peakSummary, ins.PeakHour, ins.PeakWeekday)
	}
	if !strings.Contains(htmlBody, "90.0%") {
		t.Fatalf("/insights weekday distribution missing 90.0%% share")
	}
}

func TestInsights_F17_T1_InsightsFiltersPreservePeriodAndSlices(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "itc-ins-t1-5"})
	now := time.Now().UTC()
	s.ingestEvents(t, []ingestEvent{
		{EventID: "ins-f-1", Timestamp: now.Add(-time.Hour).Format(time.RFC3339Nano), MachineID: "m-slice-1", SessionID: "s1", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(600)},
		{EventID: "ins-f-2", Timestamp: now.Add(-2 * time.Hour).Format(time.RFC3339Nano), MachineID: "m-slice-2", SessionID: "s2", Provider: "anthropic", Model: "claude-sonnet-4", Tool: "claude-code", TotalTokens: i64(400)},
		{EventID: "ins-f-3", Timestamp: now.AddDate(0, 0, -20).Format(time.RFC3339Nano), MachineID: "m-slice-1", SessionID: "s3", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(500)},
	})
	client := s.authClient(t)

	ins := itcWaitInsights(t, client, s.BaseURL+"/api/v1/insights?period=7d&dimension=models&machine=m-slice-1&tool=codex", func(i itcInsightsPayload) bool {
		return i.DataQuality.KnownTokens == 600
	})
	if ins.DataQuality.KnownEvents != 1 {
		t.Fatalf("filtered insights KnownEvents = %d, want 1", ins.DataQuality.KnownEvents)
	}
	htmlBody := itcGetHTML(t, client, s.BaseURL+"/insights?period=7d&dimension=models&machine=m-slice-1&tool=codex")
	if !strings.Contains(htmlBody, "600") {
		t.Fatalf("filtered /insights HTML missing 600 tokens")
	}
}

func TestInsights_F17_T2_EmptyAndZeroTokenHoursStillExposeSafeRovingDefault(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "itc-ins-t2-1"})
	client := s.authClient(t)

	htmlBody := itcGetHTML(t, client, s.BaseURL+"/insights")
	if strings.Contains(htmlBody, "NaN") {
		t.Fatalf("empty /insights contains NaN")
	}
	rhythmSec := itcExtractSection(htmlBody, "24-hour rhythm", "Weekday distribution")
	if _, hasSRTable := itcExtractSROnlyTable(rhythmSec); strings.Contains(rhythmSec, `role="img"`) && !hasSRTable {
		t.Fatalf("24-hour rhythm uses role=img without accessible table")
	}
	buttons := itcHourButtonRe.FindAllStringSubmatch(rhythmSec, -1)
	if len(buttons) == 0 && strings.Contains(rhythmSec, "No hourly rhythm to show yet") {
		return
	}
	if len(buttons) != 24 {
		t.Fatalf("empty /insights hour button count = %d, want 24 (or explicit empty note)", len(buttons))
	}
	zeroCount := 0
	negOneCount := 0
	for i, b := range buttons {
		if strings.Contains(b[1], `tabindex="0"`) {
			zeroCount++
			if i != 0 {
				t.Fatalf("empty /insights placed tabindex=0 on hour %d instead of 00:00", i)
			}
		} else if strings.Contains(b[1], `tabindex="-1"`) {
			negOneCount++
		} else {
			t.Fatalf("empty /insights hour button %d missing roving tabindex=0/-1: %s", i, b[1])
		}
	}
	if zeroCount != 1 || negOneCount != 23 {
		t.Fatalf("empty /insights roving tabindex counts: zero=%d negOne=%d, want 1 and 23", zeroCount, negOneCount)
	}
}

func TestInsights_F17_T2_MetadataOnlyPrivacyAcrossRhythmButtonsTableAndAPI(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "itc-ins-t2-2"})

	agentHome := filepath.Join(t.TempDir(), "agent-home")
	stateDB := filepath.Join(t.TempDir(), "agent-state.db")
	emptyCfg := filepath.Join(t.TempDir(), "empty-agent.env")
	rollout := filepath.Join(agentHome, ".codex", "sessions", "2026", "11", "20", "insights-secret.jsonl")
	if err := os.MkdirAll(filepath.Dir(rollout), 0o755); err != nil {
		t.Fatalf("mkdir codex rollout dir: %v", err)
	}
	tBase := time.Now().UTC().Add(-time.Hour)
	sessID := "sess-insights-never-leak-999"
	rawLines := strings.Join([]string{
		fmt.Sprintf(`{"timestamp":%q,"type":"session_meta","payload":{"id":%q,"model_provider":"openai","cwd":"/Users/alice/secret-repo","base_instructions":"SECRET_SYSTEM_INSTRUCTIONS"}}`, tBase.Format(time.RFC3339), sessID),
		fmt.Sprintf(`{"timestamp":%q,"type":"turn_context","payload":{"model":"gpt-5.4","cwd":"/Users/alice/secret-repo","summary":"SECRET_TITLE_DO_NOT_LEAK"}}`, tBase.Add(time.Second).Format(time.RFC3339)),
		fmt.Sprintf(`{"timestamp":%q,"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"SECRET_PROMPT_DO_NOT_LEAK"}]}}`, tBase.Add(2*time.Second).Format(time.RFC3339)),
		fmt.Sprintf(`{"timestamp":%q,"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"SECRET_RESPONSE_DO_NOT_LEAK"}]}}`, tBase.Add(3*time.Second).Format(time.RFC3339)),
		fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"cached_input_tokens":0,"output_tokens":40,"reasoning_output_tokens":0,"total_tokens":140}}}}`, tBase.Add(4*time.Second).Format(time.RFC3339)),
	}, "\n") + "\n"
	if err := os.WriteFile(rollout, []byte(rawLines), 0o644); err != nil {
		t.Fatalf("write rollout: %v", err)
	}
	_, stderr, err := s.runCLI(t, nil,
		"agent", "--once",
		"--config", emptyCfg,
		"--server", s.BaseURL,
		"--token", s.IngestToken,
		"--machine-id", "m-ins-priv",
		"--home", agentHome,
		"--state", stateDB,
		"--adapters", "codex",
	)
	if err != nil {
		t.Fatalf("agent --once failed: %v\nstderr: %s", err, stderr)
	}

	client := s.authClient(t)
	itcWaitInsights(t, client, s.BaseURL+"/api/v1/insights", func(i itcInsightsPayload) bool {
		return i.DataQuality.KnownTokens == 140
	})

	htmlBody := itcGetHTML(t, client, s.BaseURL+"/insights")
	_, _, apiBytes := itcDo(t, client, http.MethodGet, s.BaseURL+"/api/v1/insights", nil, nil)
	for _, surface := range []string{htmlBody, string(apiBytes)} {
		lower := strings.ToLower(surface)
		for _, forbidden := range []string{
			sessID,
			"secret_prompt_do_not_leak",
			"secret_response_do_not_leak",
			"secret_title_do_not_leak",
			"/users/alice",
			".jsonl",
		} {
			if strings.Contains(lower, forbidden) {
				t.Fatalf("/insights surface leaked %q", forbidden)
			}
		}
		if itcKnownWordRe.MatchString(surface) {
			t.Fatalf("/insights surface unexpectedly contains \\bknown\\b")
		}
	}
}

func TestInsights_F17_T2_AllTimePeriodOmitsMomentumBaselineWithStatusNote(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "itc-ins-t2-3"})
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "ins-all-1",
		MachineID:   "m-all",
		SessionID:   "s-all",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(500),
	}})
	client := s.authClient(t)
	itcWaitInsights(t, client, s.BaseURL+"/api/v1/insights?period=all", func(i itcInsightsPayload) bool {
		return i.DataQuality.KnownTokens == 500
	})

	htmlBody := itcGetHTML(t, client, s.BaseURL+"/insights?period=all")
	if !strings.Contains(htmlBody, "No comparable baseline for the all-time window") {
		t.Fatalf("/insights?period=all missing all-time baseline note")
	}
}

func TestInsights_F17_T2_UnknownTotalEventsFlaggedWithoutCoercingToZero(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "itc-ins-t2-4"})
	s.ingestEvents(t, []ingestEvent{{
		EventID:       "ins-unk-1",
		MachineID:     "m-unk",
		SessionID:     "s-unk",
		Provider:      "openai",
		Model:         "gpt-5.4",
		Tool:          "codex",
		TokenAccuracy: "unknown",
	}})
	client := s.authClient(t)
	itcWaitInsights(t, client, s.BaseURL+"/api/v1/insights", func(i itcInsightsPayload) bool {
		return i.DataQuality.UnknownEvents == 1
	})

	htmlBody := itcGetHTML(t, client, s.BaseURL+"/insights")
	if !strings.Contains(htmlBody, "unknown token totals") {
		t.Fatalf("/insights missing unknown-events banner")
	}
}

func TestInsights_F17_T2_InsightsAuthAndCAR205UnlinkedWithFilters(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{
		IngestToken: "itc-ins-t2-5",
		Username:    "admin",
		Password:    "ins-pass-123",
	})

	codeHTML, hdrHTML, _ := itcDo(t, s.Client, http.MethodGet, s.BaseURL+"/insights?period=7d", nil, map[string]string{"Accept": "text/html"})
	if codeHTML != http.StatusSeeOther || hdrHTML.Get("Location") != "/login?next="+url.QueryEscape("/insights?period=7d") {
		t.Fatalf("unauth /insights?period=7d status=%d loc=%q", codeHTML, hdrHTML.Get("Location"))
	}
	codeAPI, _, _ := itcDo(t, s.Client, http.MethodGet, s.BaseURL+"/api/v1/insights?period=7d", nil, nil)
	if codeAPI != http.StatusUnauthorized {
		t.Fatalf("unauth /api/v1/insights?period=7d status=%d, want 401", codeAPI)
	}

	client := s.authClient(t)
	body := itcGetHTML(t, client, s.BaseURL+"/insights?period=7d")
	nav := itcNavBlockRe.FindStringSubmatch(body)
	if len(nav) < 2 {
		t.Fatalf("/insights?period=7d missing <nav class=\"nav\">")
	}
	if strings.Contains(nav[1], `href="/insights"`) {
		t.Fatalf("CAR-205 violation: /insights linked inside primary .nav")
	}
}

func TestInsights_F17_T2_PreservesNonFacetFilterSelects(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "itc-ins-nonfacet-1"})
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "ins-nf-1",
		MachineID:   "active-box",
		SessionID:   "s-nf-1",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(250),
	}})
	client := s.authClient(t)
	itcWaitInsights(t, client, s.BaseURL+"/api/v1/insights?period=30d", func(i itcInsightsPayload) bool {
		return i.DataQuality.KnownTokens == 250
	})

	htmlBody := itcGetHTML(t, client, s.BaseURL+"/insights?period=30d&dimension=models&machine=missing-machine&provider=missing-prov&model=missing-model&tool=missing-tool")
	for _, want := range []string{
		`<option value="missing-machine" selected>`,
		`<option value="missing-prov" selected>`,
		`<option value="missing-model" selected>`,
		`<option value="missing-tool" selected>`,
	} {
		if !strings.Contains(htmlBody, want) {
			t.Fatalf("/insights dropped active non-facet filter option %q", want)
		}
	}
}

// --- Area C: Feature 22 (Tokedex, 30d What-If, /tokedex/<model>, "claude-desktop") ---

func TestTokedex_F22_T1_EnabledShowsNavCardsLegendAndPricedVsUnknown(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{
		IngestToken:   "itc-td-t1-1",
		EnableTokedex: true,
	})
	s.ingestEvents(t, []ingestEvent{
		{EventID: "td-1", MachineID: "m-td", SessionID: "s-td-1", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(1000)},
		{EventID: "td-2", MachineID: "m-td", SessionID: "s-td-2", Provider: "anthropic", Model: "claude-haiku-5-5", Tool: "claude-code", TotalTokens: i64(500)},
	})
	client := s.authClient(t)
	itcWaitCatalog(t, client, s.BaseURL+"/api/v1/catalog", func(c itcCatalogSnapshot) bool {
		return c.ObservedTokens == 1500
	})

	htmlBody := itcGetHTML(t, client, s.BaseURL+"/tokedex")
	nav := itcNavBlockRe.FindString(htmlBody)
	if !strings.Contains(nav, `href="/tokedex"`) {
		t.Fatalf("enabled /tokedex missing in primary .nav")
	}
	for _, want := range []string{
		"not measured model quality",
		"Rates not published yet",
		"No usage recorded yet",
	} {
		if !strings.Contains(htmlBody, want) {
			t.Fatalf("/tokedex missing expected text %q", want)
		}
	}
	if itcZeroTokensRe.MatchString(htmlBody) {
		t.Fatalf("/tokedex rendered forbidden \"0 tokens\" on zero-usage model card")
	}
}

func TestTokedex_F22_T1_UncatalogedTitlePresentWhenUncatalogedElseAbsent(t *testing.T) {
	requireE2E(t)
	// Sub-case (a): uncataloged model present -> #uncataloged-title rendered.
	sA := startTestServer(t, serverOpts{IngestToken: "itc-td-t1-2a", EnableTokedex: true})
	sA.ingestEvents(t, []ingestEvent{{
		EventID:     "td-uncat-1",
		MachineID:   "m-td-a",
		SessionID:   "s-td-a",
		Provider:    "custom",
		Model:       "mystery-model-x",
		Tool:        "codex",
		TotalTokens: i64(300),
	}})
	clientA := sA.authClient(t)
	itcWaitCatalog(t, clientA, sA.BaseURL+"/api/v1/catalog", func(c itcCatalogSnapshot) bool {
		return c.UncatalogedModels == 1
	})
	htmlA := itcGetHTML(t, clientA, sA.BaseURL+"/tokedex")
	if !strings.Contains(htmlA, `id="uncataloged-title"`) || !strings.Contains(htmlA, "mystery-model-x") {
		t.Fatalf("/tokedex with uncataloged model missing #uncataloged-title or mystery-model-x")
	}

	// Sub-case (b): all observed models resolve to catalog -> #uncataloged-title absent.
	sB := startTestServer(t, serverOpts{IngestToken: "itc-td-t1-2b", EnableTokedex: true})
	sB.ingestEvents(t, []ingestEvent{{
		EventID:     "td-cat-only-1",
		MachineID:   "m-td-b",
		SessionID:   "s-td-b",
		Provider:    "openai",
		Model:       "openai/gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(400),
	}})
	clientB := sB.authClient(t)
	itcWaitCatalog(t, clientB, sB.BaseURL+"/api/v1/catalog", func(c itcCatalogSnapshot) bool {
		return c.ObservedTokens == 400
	})
	htmlB := itcGetHTML(t, clientB, sB.BaseURL+"/tokedex")
	if strings.Contains(htmlB, `id="uncataloged-title"`) {
		t.Fatalf("/tokedex with only cataloged models should omit #uncataloged-title")
	}
	if !strings.Contains(htmlB, "Every observed model resolves to a bundled catalog entry") {
		t.Fatalf("/tokedex missing fully-cataloged confirmation message")
	}
}

func TestTokedex_F22_T1_ClientSide30DayWhatIfCostComparison(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{
		IngestToken:   "itc-td-t1-3",
		EnableTokedex: true,
	})
	now := time.Now().UTC()
	// Ingest both a 30d event (1,700,000) and an older >30d event (9,000,000) so
	// lifetime tokens (10,700,000) differ from the 30d what-if window (1,700,000).
	s.ingestEvents(t, []ingestEvent{
		{
			EventID:         "td-whatif-1",
			Timestamp:       now.Add(-24 * time.Hour).Format(time.RFC3339Nano),
			MachineID:       "m-wi",
			SessionID:       "s-wi",
			Provider:        "openai",
			Model:           "gpt-5.4",
			Tool:            "codex",
			InputTokens:     i64(1_000_000),
			CacheReadTokens: i64(500_000),
			OutputTokens:    i64(200_000),
			TotalTokens:     i64(1_700_000),
		},
		{
			EventID:     "td-whatif-old",
			Timestamp:   now.AddDate(0, 0, -45).Format(time.RFC3339Nano),
			MachineID:   "m-wi",
			SessionID:   "s-wi-old",
			Provider:    "openai",
			Model:       "gpt-5.4",
			Tool:        "codex",
			InputTokens: i64(9_000_000),
			TotalTokens: i64(9_000_000),
		},
	})
	client := s.authClient(t)
	itcWaitCatalog(t, client, s.BaseURL+"/api/v1/catalog", func(c itcCatalogSnapshot) bool {
		return c.ObservedTokens == 10_700_000
	})

	htmlBody := itcGetHTML(t, client, s.BaseURL+"/tokedex")
	if !strings.Contains(htmlBody, "10,700,000") && !strings.Contains(htmlBody, "10.7M") {
		t.Fatalf("/tokedex lifetime card missing 10,700,000 observed tokens")
	}

	nonTier := itcStripTierSections(htmlBody)
	whatIfScope := itcExtractWhatIfSection(htmlBody)
	if whatIfScope == "" {
		whatIfScope = nonTier
	}
	lowerScope := strings.ToLower(whatIfScope)
	if !strings.Contains(lowerScope, "what-if") && !strings.Contains(lowerScope, "what if") {
		t.Fatalf("/tokedex missing 30d what-if cost comparison UI/section")
	}
	has30dBasis := strings.Contains(whatIfScope, "1,700,000") ||
		strings.Contains(whatIfScope, "1.7M") ||
		strings.Contains(whatIfScope, "1700000") ||
		strings.Contains(whatIfScope, "$5.63") ||
		strings.Contains(whatIfScope, "$5.62") ||
		strings.Contains(whatIfScope, "5.625")
	if !has30dBasis {
		t.Fatalf("/tokedex what-if UI/data missing 30d window basis (1,700,000 / 1.7M / $5.63) distinct from 10,700,000 lifetime:\n%s", whatIfScope)
	}
	if strings.Contains(htmlBody, "NaN") {
		t.Fatalf("/tokedex contains NaN")
	}
	// Unknown-pricing models must remain unpublished/Unknown, never $0.00.
	haikuCard, ok := itcExtractModelArticle(htmlBody, "claude-haiku-5-5")
	if !ok {
		t.Fatalf("/tokedex missing <article> card for claude-haiku-5-5")
	}
	if strings.Contains(haikuCard, "$0.00") {
		t.Fatalf("mode: unknown card claude-haiku-5-5 rendered $0.00: %s", haikuCard)
	}
	if strings.Contains(whatIfScope, "$0.00") {
		t.Fatalf("30d what-if UI rendered $0.00: %s", whatIfScope)
	}
}

func TestTokedex_F22_T1_ModelDetailPageShowsTrendProvenanceMachinesAndSessions(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{
		IngestToken:   "itc-td-t1-4",
		EnableTokedex: true,
	})
	now := time.Now().UTC()
	s.ingestEvents(t, []ingestEvent{
		{
			EventID:     "td-det-1",
			Timestamp:   now.Add(-48 * time.Hour).Format(time.RFC3339Nano),
			MachineID:   "m-td-det-alpha",
			SessionID:   "sess-gpt54-only-1",
			Project:     "proj-gpt54",
			Provider:    "openai",
			Model:       "openai/gpt-5.4",
			Tool:        "codex",
			TotalTokens: i64(1200),
		},
		{
			EventID:     "td-det-2",
			Timestamp:   now.Add(-2 * time.Hour).Format(time.RFC3339Nano),
			MachineID:   "m-td-det-beta",
			SessionID:   "sess-other-model-999",
			Project:     "proj-other",
			Provider:    "anthropic",
			Model:       "claude-sonnet-4",
			Tool:        "claude-code",
			TotalTokens: i64(900),
		},
	})
	client := s.authClient(t)
	itcWaitCatalog(t, client, s.BaseURL+"/api/v1/catalog", func(c itcCatalogSnapshot) bool {
		return c.ObservedTokens == 2100
	})

	detailHTML := itcGetHTML(t, client, s.BaseURL+"/tokedex/gpt-5.4")
	for _, want := range []string{
		"GPT-5.4",
		"https://openai.com/api/pricing/",
		"2026-07-12",
		"m-td-det-alpha",
		"sess-gpt54-only-1",
		"1,200",
	} {
		if !strings.Contains(detailHTML, want) {
			t.Fatalf("/tokedex/gpt-5.4 missing expected detail %q", want)
		}
	}
	if !regexp.MustCompile(`(?is)<h[2-4]\b[^>]*>.*?(?:weekly|trend).*?</h[2-4]>`).MatchString(detailHTML) {
		t.Fatalf("/tokedex/gpt-5.4 missing weekly trend section heading")
	}
	if strings.Contains(detailHTML, "sess-other-model-999") || strings.Contains(detailHTML, "m-td-det-beta") {
		t.Fatalf("/tokedex/gpt-5.4 leaked sessions/machines from unrelated model")
	}
}

func TestTokedex_F22_T1_CatalogModelCardsLinkToDetailViewAndSettingsCrossLink(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{
		IngestToken:   "itc-td-t1-5",
		EnableTokedex: true,
	})
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "td-link-1",
		MachineID:   "m-td-link",
		SessionID:   "s-td-link",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(500),
	}})
	client := s.authClient(t)
	itcWaitCatalog(t, client, s.BaseURL+"/api/v1/catalog", func(c itcCatalogSnapshot) bool {
		return c.ObservedTokens == 500
	})

	tokedexHTML := itcGetHTML(t, client, s.BaseURL+"/tokedex")
	tokedexBody := itcStripTopbar(tokedexHTML)
	if !strings.Contains(tokedexBody, `href="/tokedex/gpt-5.4"`) {
		t.Fatalf("/tokedex model card missing link to /tokedex/gpt-5.4")
	}
	if !strings.Contains(tokedexBody, `href="/settings`) {
		t.Fatalf("/tokedex page body missing cross-link to /settings outside topbar")
	}

	settingsHTML := itcGetHTML(t, client, s.BaseURL+"/settings")
	settingsBody := itcStripTopbar(settingsHTML)
	if !strings.Contains(settingsBody, `href="/tokedex`) {
		t.Fatalf("/settings page body missing cross-link to /tokedex outside topbar")
	}
}

func TestTokedex_F22_T1_ClaudeDesktopDisplayMappingAcrossSurfaces(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{
		IngestToken:   "itc-td-t1-6",
		EnableTokedex: true,
	})
	now := time.Now().UTC()
	sessID := "sess-claude-desktop-1"
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "td-cd-1",
		Timestamp:   now.Add(-15 * time.Minute).Format(time.RFC3339Nano),
		MachineID:   "m-cd",
		SessionID:   sessID,
		Project:     "proj-desktop",
		Provider:    "anthropic",
		Model:       "claude-sonnet-4",
		Tool:        "claude-desktop",
		TotalTokens: i64(800),
	}})
	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 800)
	itcWaitCatalog(t, client, s.BaseURL+"/api/v1/catalog", func(c itcCatalogSnapshot) bool {
		return c.ObservedTokens == 800
	})

	for _, p := range []string{
		"/",
		"/analytics",
		"/sessions",
		"/sessions/" + sessID,
		"/insights",
		"/tokedex/claude-sonnet-4",
	} {
		body := itcGetHTML(t, client, s.BaseURL+p)
		if !strings.Contains(body, "Claude Desktop") {
			t.Fatalf("surface %s did not map claude-desktop -> \"Claude Desktop\"", p)
		}
	}
}

func TestTokedex_F22_T2_DisabledFlagReturns404OnBothIndexAndDetailAfterAuth(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{
		IngestToken:   "itc-td-t2-1",
		Username:      "admin",
		Password:      "td-pass-123",
		EnableTokedex: false,
	})
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "td-dis-1",
		MachineID:   "m-dis",
		SessionID:   "s-dis",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(100),
	}})

	// Auth runs before feature gate: anonymous HTML -> 303 /login?next=...
	for _, p := range []string{"/tokedex", "/tokedex/gpt-5.4"} {
		code, hdrs, _ := itcDo(t, s.Client, http.MethodGet, s.BaseURL+p, nil, map[string]string{"Accept": "text/html"})
		if code != http.StatusSeeOther || hdrs.Get("Location") != "/login?next="+url.QueryEscape(p) {
			t.Fatalf("anonymous %s status=%d loc=%q, want 303 /login?next=...", p, code, hdrs.Get("Location"))
		}
	}

	// Authenticated -> 404 on UI routes, 200 on /api/v1/catalog.
	client := s.authClient(t)
	for _, p := range []string{"/tokedex", "/catalog", "/tokedex/gpt-5.4"} {
		code, _, _ := itcDo(t, client, http.MethodGet, s.BaseURL+p, nil, map[string]string{"Accept": "text/html"})
		if code != http.StatusNotFound {
			t.Fatalf("authed %s when EnableTokedex=false status = %d, want 404", p, code)
		}
	}
	codeAPI, _, _ := itcDo(t, client, http.MethodGet, s.BaseURL+"/api/v1/catalog", nil, nil)
	if codeAPI != http.StatusOK {
		t.Fatalf("authed /api/v1/catalog when EnableTokedex=false status = %d, want 200", codeAPI)
	}
}

func TestTokedex_F22_T2_DetailUnknownOrUncatalogedModelReturns404(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{
		IngestToken:   "itc-td-t2-2",
		EnableTokedex: true,
	})
	uncatModel := "observed-uncataloged-model-404"
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "td-uncat-404-1",
		MachineID:   "m-td-404",
		SessionID:   "s-td-404",
		Provider:    "custom",
		Model:       uncatModel,
		Tool:        "codex",
		TotalTokens: i64(250),
	}})
	client := s.authClient(t)
	itcWaitCatalog(t, client, s.BaseURL+"/api/v1/catalog", func(c itcCatalogSnapshot) bool {
		return c.UncatalogedModels == 1
	})

	cUncat, _, bodyUncat := itcDo(t, client, http.MethodGet, s.BaseURL+"/tokedex/"+url.PathEscape(uncatModel), nil, map[string]string{"Accept": "text/html"})
	if cUncat != http.StatusOK {
		t.Fatalf("GET /tokedex/%s status = %d, want 200 for observed uncataloged model", uncatModel, cUncat)
	}
	uncatStr := string(bodyUncat)
	if !strings.Contains(uncatStr, "Uncataloged") || !strings.Contains(uncatStr, "Unknown") {
		t.Fatalf("GET /tokedex/%s missing Uncataloged/Unknown markers", uncatModel)
	}
	if strings.Contains(uncatStr, "$0.00") {
		t.Fatalf("GET /tokedex/%s leaked $0.00 for uncataloged model", uncatModel)
	}

	for _, slug := range []string{"does-not-exist-model", "unobserved-uncataloged-model", "%3Cscript%3Ealert(1)%3C%2Fscript%3E", "..%2F..%2Fetc%2Fpasswd"} {
		code, _, body := itcDo(t, client, http.MethodGet, s.BaseURL+"/tokedex/"+slug, nil, map[string]string{"Accept": "text/html"})
		if code != http.StatusNotFound && code != http.StatusBadRequest {
			t.Fatalf("GET /tokedex/%s status = %d, want 400 or 404", slug, code)
		}
		if strings.Contains(string(body), "<script>alert(1)</script>") {
			t.Fatalf("GET /tokedex/%s reflected unescaped script", slug)
		}
	}
}

func TestTokedex_F22_T2_WhatIfUnknownModelAndZero30dUsageNeverShowZeroDollars(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{
		IngestToken:   "itc-td-t2-3",
		EnableTokedex: true,
	})
	s.ingestEvents(t, []ingestEvent{{
		EventID:       "td-wi-unk-1",
		MachineID:     "m-wi-unk",
		SessionID:     "s-wi-unk",
		Provider:      "anthropic",
		Model:         "claude-haiku-5-5",
		Tool:          "claude-code",
		TokenAccuracy: "unknown",
	}})
	client := s.authClient(t)
	itcWaitCatalog(t, client, s.BaseURL+"/api/v1/catalog", func(c itcCatalogSnapshot) bool {
		return len(c.Models) > 0
	})

	htmlBody := itcGetHTML(t, client, s.BaseURL+"/tokedex")
	nonTier := itcStripTierSections(htmlBody)
	lowerNonTier := strings.ToLower(nonTier)
	if !strings.Contains(lowerNonTier, "what-if") && !strings.Contains(lowerNonTier, "what if") {
		t.Fatalf("/tokedex missing 30d what-if UI/section")
	}
	if strings.Contains(htmlBody, "NaN") {
		t.Fatalf("/tokedex rendered NaN with zero/unknown 30d usage")
	}
	haikuCard, ok := itcExtractModelArticle(htmlBody, "claude-haiku-5-5")
	if !ok {
		t.Fatalf("/tokedex missing <article> card for claude-haiku-5-5")
	}
	if strings.Contains(haikuCard, "$0.00") {
		t.Fatalf("mode: unknown card claude-haiku-5-5 showed $0.00: %s", haikuCard)
	}
	if whatIfSec := itcExtractWhatIfSection(htmlBody); whatIfSec != "" && strings.Contains(whatIfSec, "$0.00") {
		t.Fatalf("30d what-if section showed $0.00 for unknown/zero 30d usage: %s", whatIfSec)
	}
}

func TestTokedex_F22_T2_NonModelSentinelsDisclosedWithoutInflatingUncatalogedCount(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{
		IngestToken:   "itc-td-t2-4",
		EnableTokedex: true,
	})
	s.ingestEvents(t, []ingestEvent{
		{EventID: "td-sent-1", MachineID: "m-sent", SessionID: "s1", Provider: "anthropic", Model: "unknown", Tool: "claude-code", TotalTokens: i64(100)},
		{EventID: "td-sent-2", MachineID: "m-sent", SessionID: "s2", Provider: "anthropic", Model: "<synthetic>", Tool: "claude-code", TotalTokens: i64(100)},
		{EventID: "td-sent-3", MachineID: "m-sent", SessionID: "s3", Provider: "google", Model: "gemini-default", Tool: "antigravity", TotalTokens: i64(100)},
		{EventID: "td-real-1", MachineID: "m-sent", SessionID: "s4", Provider: "custom", Model: "real-uncataloged-9", Tool: "codex", TotalTokens: i64(200)},
	})
	client := s.authClient(t)
	snap := itcWaitCatalog(t, client, s.BaseURL+"/api/v1/catalog", func(c itcCatalogSnapshot) bool {
		return c.ObservedTokens == 500
	})
	if snap.UncatalogedModels != 1 {
		t.Fatalf("UncatalogedModels = %d, want 1 (sentinels must not inflate count)", snap.UncatalogedModels)
	}

	htmlBody := itcGetHTML(t, client, s.BaseURL+"/tokedex")
	if !strings.Contains(htmlBody, `id="uncataloged-title"`) {
		t.Fatalf("/tokedex missing #uncataloged-title")
	}
	if !strings.Contains(htmlBody, "1 uncataloged model") {
		t.Fatalf("/tokedex coverage header did not say \"1 uncataloged model\"")
	}
	for _, id := range []string{"unknown", "&lt;synthetic&gt;", "gemini-default", "real-uncataloged-9"} {
		if !strings.Contains(htmlBody, id) {
			t.Fatalf("/tokedex uncataloged section missing sentinel/model %q", id)
		}
	}
}

func TestTokedex_F22_T2_EmptyCatalogAndNoUsageStates(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{
		IngestToken:   "itc-td-t2-5",
		EnableTokedex: true,
		CatalogYAML:   "schema_version: 2\nmodels: {}\n",
	})
	client := s.authClient(t)

	htmlBody := itcGetHTML(t, client, s.BaseURL+"/tokedex")
	if !strings.Contains(htmlBody, "No catalog models are configured") {
		t.Fatalf("empty catalog /tokedex missing empty-catalog guidance")
	}
	if strings.Contains(htmlBody, `id="tier-legend-title"`) || strings.Contains(htmlBody, `id="uncataloged-title"`) {
		t.Fatalf("empty catalog /tokedex unexpectedly rendered tier legend or uncataloged section")
	}
}

func TestTokedex_F22_T2_DetailViewMetadataOnlyNoPromptsOrPaths(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{
		IngestToken:   "itc-td-t2-6",
		EnableTokedex: true,
	})

	agentHome := filepath.Join(t.TempDir(), "agent-home")
	stateDB := filepath.Join(t.TempDir(), "agent-state.db")
	emptyCfg := filepath.Join(t.TempDir(), "empty-agent.env")
	rollout := filepath.Join(agentHome, ".codex", "sessions", "2026", "11", "20", "td-secret-rollout.jsonl")
	if err := os.MkdirAll(filepath.Dir(rollout), 0o755); err != nil {
		t.Fatalf("mkdir codex rollout dir: %v", err)
	}
	tBase := time.Now().UTC().Add(-time.Hour)
	rawLines := strings.Join([]string{
		fmt.Sprintf(`{"timestamp":%q,"type":"session_meta","payload":{"id":"sess-td-priv-1","model_provider":"openai","cwd":"/Users/alice/secret-repo","base_instructions":"SECRET_SYSTEM_INSTRUCTIONS"}}`, tBase.Format(time.RFC3339)),
		fmt.Sprintf(`{"timestamp":%q,"type":"turn_context","payload":{"model":"gpt-5.4","cwd":"/Users/alice/secret-repo","summary":"SECRET_TITLE_DO_NOT_LEAK"}}`, tBase.Add(time.Second).Format(time.RFC3339)),
		fmt.Sprintf(`{"timestamp":%q,"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"SECRET_PROMPT_DO_NOT_LEAK"}]}}`, tBase.Add(2*time.Second).Format(time.RFC3339)),
		fmt.Sprintf(`{"timestamp":%q,"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"SECRET_RESPONSE_DO_NOT_LEAK"}]}}`, tBase.Add(3*time.Second).Format(time.RFC3339)),
		fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"cached_input_tokens":0,"output_tokens":40,"reasoning_output_tokens":0,"total_tokens":140}}}}`, tBase.Add(4*time.Second).Format(time.RFC3339)),
	}, "\n") + "\n"
	if err := os.WriteFile(rollout, []byte(rawLines), 0o644); err != nil {
		t.Fatalf("write rollout: %v", err)
	}

	_, stderr, err := s.runCLI(t, nil,
		"agent", "--once",
		"--config", emptyCfg,
		"--server", s.BaseURL,
		"--token", s.IngestToken,
		"--machine-id", "m-td-priv",
		"--home", agentHome,
		"--state", stateDB,
		"--adapters", "codex",
	)
	if err != nil {
		t.Fatalf("agent --once failed: %v\nstderr: %s", err, stderr)
	}

	client := s.authClient(t)
	itcWaitCatalog(t, client, s.BaseURL+"/api/v1/catalog", func(c itcCatalogSnapshot) bool {
		return c.ObservedTokens == 140
	})

	detailHTML := itcGetHTML(t, client, s.BaseURL+"/tokedex/gpt-5.4")
	lower := strings.ToLower(detailHTML)
	for _, forbidden := range []string{
		"secret_prompt_do_not_leak",
		"secret_response_do_not_leak",
		"secret_title_do_not_leak",
		"secret_system_instructions",
		"/users/alice",
		".jsonl",
	} {
		if strings.Contains(lower, forbidden) {
			t.Fatalf("/tokedex/gpt-5.4 leaked %q", forbidden)
		}
	}
}

// --- Area D: Feature 24 (/api/v1/card.svg Share Card) ---

func TestCardSVG_F24_T1_AuthRequiredByDefaultJSON401AndHTML303(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{
		IngestToken:    "card-ing-tok",
		DashboardToken: "card-dash-tok",
		Username:       "admin",
		Password:       "card-pass-123",
	})

	// Anonymous non-HTML -> 401 + WWW-Authenticate: Basic.
	c401, h401, _ := itcDo(t, s.Client, http.MethodGet, s.BaseURL+"/api/v1/card.svg", nil, nil)
	if c401 != http.StatusUnauthorized {
		t.Fatalf("anon GET /api/v1/card.svg status = %d, want 401", c401)
	}
	if !strings.Contains(h401.Get("WWW-Authenticate"), "Basic") {
		t.Fatalf("401 missing WWW-Authenticate Basic header: %q", h401.Get("WWW-Authenticate"))
	}

	// Anonymous Accept: text/html -> 303 /login?next=%2Fapi%2Fv1%2Fcard.svg.
	c303, h303, _ := itcDo(t, s.Client, http.MethodGet, s.BaseURL+"/api/v1/card.svg", nil, map[string]string{"Accept": "text/html"})
	if c303 != http.StatusSeeOther || h303.Get("Location") != "/login?next=%2Fapi%2Fv1%2Fcard.svg" {
		t.Fatalf("anon HTML GET /api/v1/card.svg status=%d loc=%q, want 303 /login?next=%%2Fapi%%2Fv1%%2Fcard.svg", c303, h303.Get("Location"))
	}

	// Cookie auth -> 200 image/svg+xml + Cache-Control: no-store.
	cookieClient := s.authClient(t)
	cCookie, hCookie, _ := itcDo(t, cookieClient, http.MethodGet, s.BaseURL+"/api/v1/card.svg", nil, nil)
	if cCookie != http.StatusOK || !strings.Contains(hCookie.Get("Content-Type"), "image/svg+xml") || hCookie.Get("Cache-Control") != "no-store" {
		t.Fatalf("cookie auth card.svg unexpected: status=%d ct=%q cc=%q", cCookie, hCookie.Get("Content-Type"), hCookie.Get("Cache-Control"))
	}

	// Bearer DashboardToken -> 200.
	cBearer, _, _ := itcDo(t, s.Client, http.MethodGet, s.BaseURL+"/api/v1/card.svg", nil, map[string]string{
		"Authorization": "Bearer " + s.DashboardToken,
	})
	if cBearer != http.StatusOK {
		t.Fatalf("Bearer DashboardToken card.svg status = %d, want 200", cBearer)
	}

	// Basic auth -> 200.
	basicCreds := base64.StdEncoding.EncodeToString([]byte(s.Username + ":" + s.Password))
	cBasic, _, _ := itcDo(t, s.Client, http.MethodGet, s.BaseURL+"/api/v1/card.svg", nil, map[string]string{
		"Authorization": "Basic " + basicCreds,
	})
	if cBasic != http.StatusOK {
		t.Fatalf("Basic auth card.svg status = %d, want 200", cBasic)
	}
}

func TestCardSVG_F24_T1_ValidParseableXMLAndSVGStructure(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "card-f24-t1-2"})
	client := s.authClient(t)

	code, hdrs, svgBytes := itcDo(t, client, http.MethodGet, s.BaseURL+"/api/v1/card.svg", nil, nil)
	if code != http.StatusOK {
		t.Fatalf("GET /api/v1/card.svg status = %d", code)
	}
	if !strings.Contains(hdrs.Get("Content-Type"), "image/svg+xml") {
		t.Fatalf("Content-Type = %q, want image/svg+xml", hdrs.Get("Content-Type"))
	}
	doc := itcValidateSVGXML(t, svgBytes)
	if doc.Xmlns != "http://www.w3.org/2000/svg" {
		t.Fatalf("<svg xmlns=%q>, want http://www.w3.org/2000/svg", doc.Xmlns)
	}
	if strings.TrimSpace(doc.ViewBox) == "" || strings.TrimSpace(doc.Width) == "" || strings.TrimSpace(doc.Height) == "" {
		t.Fatalf("<svg> missing explicit viewBox/width/height: viewBox=%q width=%q height=%q", doc.ViewBox, doc.Width, doc.Height)
	}
}

func TestCardSVG_F24_T1_ReflectsAggregateStageTokensStreakAndSparkline(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "card-f24-t1-3", Timezone: "UTC"})
	now := time.Now().UTC()
	s.ingestEvents(t, []ingestEvent{
		{EventID: "card-agg-1", Timestamp: now.AddDate(0, 0, -2).Format(time.RFC3339Nano), MachineID: "m-card", SessionID: "s1", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(400)},
		{EventID: "card-agg-2", Timestamp: now.AddDate(0, 0, -1).Format(time.RFC3339Nano), MachineID: "m-card", SessionID: "s2", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(500)},
		{EventID: "card-agg-3", Timestamp: now.Format(time.RFC3339Nano), MachineID: "m-card", SessionID: "s3", Provider: "openai", Model: "gpt-5.4", Tool: "codex", TotalTokens: i64(600)},
	})
	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 1500)

	code, _, svgBytes := itcDo(t, client, http.MethodGet, s.BaseURL+"/api/v1/card.svg", nil, nil)
	if code != http.StatusOK {
		t.Fatalf("GET /api/v1/card.svg status = %d", code)
	}
	_ = itcValidateSVGXML(t, svgBytes)
	svg := string(svgBytes)
	svgText := itcExtractSVGText(t, svgBytes)
	// 1,500 tokens = Stage 3 Prompt Imp.
	if !strings.Contains(svgText, "Prompt Imp") {
		t.Fatalf("/api/v1/card.svg missing Stage 3 form name Prompt Imp in SVG <text> (%q):\n%s", svgText, svg)
	}
	if !strings.Contains(svgText, "1,500") && !strings.Contains(svgText, "1.5K") {
		t.Fatalf("/api/v1/card.svg missing formatted lifetime token total 1,500 / 1.5K in SVG <text> (%q):\n%s", svgText, svg)
	}
	if !regexp.MustCompile(`(?:^|\D)3(?:\D|$)`).MatchString(svgText) {
		t.Fatalf("/api/v1/card.svg missing 3-day active/streak signal in SVG <text> (%q):\n%s", svgText, svg)
	}
	if !itcSVGGeometryRe.MatchString(svg) {
		t.Fatalf("/api/v1/card.svg missing SVG sparkline/panel geometry elements")
	}
}

func TestCardSVG_F24_T1_UsesTokemon16BitPalette(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "card-f24-t1-4"})
	client := s.authClient(t)

	code, _, svgBytes := itcDo(t, client, http.MethodGet, s.BaseURL+"/api/v1/card.svg", nil, nil)
	if code != http.StatusOK {
		t.Fatalf("GET /api/v1/card.svg status = %d", code)
	}
	svgLower := strings.ToLower(string(svgBytes))
	for _, hexColor := range []string{"#10110f", "#171916", "#9bbba0", "#d2a477", "#f0ede5"} {
		if !strings.Contains(svgLower, hexColor) {
			t.Fatalf("/api/v1/card.svg missing STYLE_GUIDE 16-bit palette color %s", hexColor)
		}
	}
}

func TestCardSVG_F24_T1_UpdatesAfterIngestBurst(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "card-f24-t1-5"})
	client := s.authClient(t)

	_, _, initialSVG := itcDo(t, client, http.MethodGet, s.BaseURL+"/api/v1/card.svg", nil, nil)
	if !strings.Contains(string(initialSVG), "Egg") {
		t.Fatalf("initial card.svg missing Stage 0 Egg: %s", string(initialSVG))
	}

	s.ingestEvents(t, []ingestEvent{{
		EventID:     "card-burst-1",
		MachineID:   "m-burst",
		SessionID:   "s-burst",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(150),
	}})
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 150)

	deadline := time.Now().Add(5 * time.Second)
	for {
		_, _, updatedSVG := itcDo(t, client, http.MethodGet, s.BaseURL+"/api/v1/card.svg", nil, nil)
		text := string(updatedSVG)
		if strings.Contains(text, "Bytelet") && strings.Contains(text, "150") {
			_ = itcValidateSVGXML(t, updatedSVG)
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("card.svg did not update to Bytelet / 150 after ingest burst:\n%s", text)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestCardSVG_F24_T2_PrivacySafeZeroProjectSessionMachinePathOrPromptLeak(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "card-f24-t2-1"})

	// Ingest via both agent rollout and direct batch with distinctive secrets + aliases.
	agentHome := filepath.Join(t.TempDir(), "agent-home")
	stateDB := filepath.Join(t.TempDir(), "agent-state.db")
	emptyCfg := filepath.Join(t.TempDir(), "empty-agent.env")
	rollout := filepath.Join(agentHome, ".codex", "sessions", "2026", "11", "20", "card-secret.jsonl")
	if err := os.MkdirAll(filepath.Dir(rollout), 0o755); err != nil {
		t.Fatalf("mkdir codex rollout dir: %v", err)
	}
	tBase := time.Now().UTC().Add(-time.Hour)
	rawLines := strings.Join([]string{
		fmt.Sprintf(`{"timestamp":%q,"type":"session_meta","payload":{"id":"sess-card-secret-999","model_provider":"openai","cwd":"/Users/alice/top-secret-codename","base_instructions":"SECRET_SYSTEM_INSTRUCTIONS"}}`, tBase.Format(time.RFC3339)),
		fmt.Sprintf(`{"timestamp":%q,"type":"turn_context","payload":{"model":"gpt-5.4","cwd":"/Users/alice/top-secret-codename","summary":"SECRET_TITLE_DO_NOT_LEAK"}}`, tBase.Add(time.Second).Format(time.RFC3339)),
		fmt.Sprintf(`{"timestamp":%q,"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"SECRET_PROMPT_DO_NOT_LEAK"}]}}`, tBase.Add(2*time.Second).Format(time.RFC3339)),
		fmt.Sprintf(`{"timestamp":%q,"type":"event_msg","payload":{"type":"token_count","info":{"total_token_usage":{"input_tokens":100,"cached_input_tokens":0,"output_tokens":50,"reasoning_output_tokens":0,"total_tokens":150}}}}`, tBase.Add(3*time.Second).Format(time.RFC3339)),
	}, "\n") + "\n"
	if err := os.WriteFile(rollout, []byte(rawLines), 0o644); err != nil {
		t.Fatalf("write rollout: %v", err)
	}
	_, stderr, err := s.runCLI(t, nil,
		"agent", "--once",
		"--config", emptyCfg,
		"--server", s.BaseURL,
		"--token", s.IngestToken,
		"--machine-id", "secret-workstation-host",
		"--home", agentHome,
		"--state", stateDB,
		"--adapters", "codex",
	)
	if err != nil {
		t.Fatalf("agent --once failed: %v\nstderr: %s", err, stderr)
	}

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 150)

	// Add a distinctive machine alias too — share card must stay aggregate-only.
	aliasForm := url.Values{}
	aliasForm.Set("machine_identity", "secret-workstation-host")
	aliasForm.Set("machine_alias", "Secret Studio Mac")
	itcPostAliases(t, s, client, aliasForm)

	code, _, svgBytes := itcDo(t, client, http.MethodGet, s.BaseURL+"/api/v1/card.svg", nil, nil)
	if code != http.StatusOK {
		t.Fatalf("GET /api/v1/card.svg status = %d", code)
	}
	_ = itcValidateSVGXML(t, svgBytes)
	lower := strings.ToLower(string(svgBytes))
	for _, forbidden := range []string{
		"top-secret-codename",
		"sess-card-secret-999",
		"secret-workstation-host",
		"secret studio mac",
		"/users/alice",
		"secret_prompt_do_not_leak",
		"secret_title_do_not_leak",
		".jsonl",
	} {
		if strings.Contains(lower, forbidden) {
			t.Fatalf("/api/v1/card.svg leaked private identifier/content %q", forbidden)
		}
	}
}

func TestCardSVG_F24_T2_SpecialCharactersXMLEscapedAndValidXML(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "card-f24-t2-2"})
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "card-xml-1",
		MachineID:   "m-<xml>&\"host\"",
		SessionID:   "sess-<xml>&\"1\"",
		Project:     "proj-<xml>&\"1\"",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(120),
	}})
	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 120)

	code, _, svgBytes := itcDo(t, client, http.MethodGet, s.BaseURL+"/api/v1/card.svg?label=%3Cevil%3E%26test", nil, nil)
	if code != http.StatusOK {
		t.Fatalf("GET /api/v1/card.svg status = %d", code)
	}
	_ = itcValidateSVGXML(t, svgBytes)
	if strings.Contains(string(svgBytes), "<evil>") {
		t.Fatalf("/api/v1/card.svg reflected unescaped query string")
	}
}

func TestCardSVG_F24_T2_EmptyDatabaseRendersValidStageZeroCard(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "card-f24-t2-3"})
	client := s.authClient(t)

	code, _, svgBytes := itcDo(t, client, http.MethodGet, s.BaseURL+"/api/v1/card.svg", nil, nil)
	if code != http.StatusOK {
		t.Fatalf("GET /api/v1/card.svg on empty DB status = %d", code)
	}
	_ = itcValidateSVGXML(t, svgBytes)
	svg := string(svgBytes)
	svgText := itcExtractSVGText(t, svgBytes)
	if !strings.Contains(svgText, "Egg") {
		t.Fatalf("empty DB card.svg missing Stage 0 Egg in SVG <text> (%q):\n%s", svgText, svg)
	}
	if !regexp.MustCompile(`(?:^|\D)0(?:\D|$)`).MatchString(svgText) {
		t.Fatalf("empty DB card.svg missing 0 token count in SVG <text> (%q):\n%s", svgText, svg)
	}
	if strings.Contains(svg, "NaN") || strings.Contains(svg, "Infinity") {
		t.Fatalf("empty DB card.svg contains NaN/Infinity")
	}
}

func TestCardSVG_F24_T2_DistinctIngestTokenRejectedOnCardSVG(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{
		IngestToken:    "card-ing-only",
		DashboardToken: "card-dash-only",
	})

	cIng, _, _ := itcDo(t, s.Client, http.MethodGet, s.BaseURL+"/api/v1/card.svg", nil, map[string]string{
		"Authorization": "Bearer " + s.IngestToken,
	})
	if cIng != http.StatusUnauthorized {
		t.Fatalf("IngestToken on /api/v1/card.svg status = %d, want 401", cIng)
	}

	cDash, _, _ := itcDo(t, s.Client, http.MethodGet, s.BaseURL+"/api/v1/card.svg", nil, map[string]string{
		"Authorization": "Bearer " + s.DashboardToken,
	})
	if cDash != http.StatusOK {
		t.Fatalf("DashboardToken on /api/v1/card.svg status = %d, want 200", cDash)
	}
}

func TestCardSVG_F24_T2_MethodNotAllowedOrQuerySafe(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "card-f24-t2-5"})
	client := s.authClient(t)

	cPost, _, _ := itcDo(t, client, http.MethodPost, s.BaseURL+"/api/v1/card.svg", []byte("x=1"), nil)
	if cPost == http.StatusOK || cPost >= http.StatusInternalServerError {
		t.Fatalf("POST /api/v1/card.svg status = %d, want 405/404 (non-200, non-5xx)", cPost)
	}

	// Verify concurrent reads work cleanly and XSS query param is never reflected.
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := client.Get(s.BaseURL + "/api/v1/card.svg?evil=%3Cscript%3Ealert(1)%3C/script%3E")
			if err != nil {
				errs <- err
				return
			}
			body, readErr := io.ReadAll(resp.Body)
			closeErr := resp.Body.Close()
			if readErr != nil {
				errs <- readErr
				return
			}
			if closeErr != nil {
				errs <- closeErr
				return
			}
			if resp.StatusCode != http.StatusOK {
				errs <- fmt.Errorf("status = %d", resp.StatusCode)
				return
			}
			if strings.Contains(string(body), "<script>") {
				errs <- fmt.Errorf("reflected script tag")
				return
			}
			if !itcCardStageRe.Match(body) {
				errs <- fmt.Errorf("missing stage name in card.svg")
				return
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent card.svg check failed: %v", err)
	}
}
