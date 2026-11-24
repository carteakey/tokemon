//go:build e2e

package e2e_test

import (
	"bytes"
	"encoding/csv"
	"encoding/json"
	"fmt"
	"html"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type ssSessionRow struct {
	SessionID     string   `json:"session_id"`
	Timestamp     string   `json:"timestamp"`
	Machine       string   `json:"machine"`
	Provider      string   `json:"provider"`
	Model         string   `json:"model"`
	Tool          string   `json:"tool"`
	Project       string   `json:"project,omitempty"`
	InputTokens   *int64   `json:"input_tokens,omitempty"`
	OutputTokens  *int64   `json:"output_tokens,omitempty"`
	TotalTokens   *int64   `json:"total_tokens,omitempty"`
	Cost          *float64 `json:"cost,omitempty"`
	CostEstimated bool     `json:"cost_estimated,omitempty"`
	DurationMS    *int64   `json:"duration_ms,omitempty"`
	Accuracy      string   `json:"accuracy"`
	Events        int64    `json:"events"`
}

type ssQuotaRow struct {
	ID             string   `json:"id,omitempty"`
	Label          string   `json:"label"`
	Provider       string   `json:"provider,omitempty"`
	Model          string   `json:"model,omitempty"`
	ResetDay       int      `json:"reset_day"`
	TokensPerMonth *int64   `json:"tokens_per_month,omitempty"`
	CostPerMonth   *float64 `json:"cost_per_month,omitempty"`
	TokensBudget   *int64   `json:"tokens_budget,omitempty"`
	CostBudget     *float64 `json:"cost_budget,omitempty"`
	TokensUsed     int64    `json:"tokens_used"`
	CostUsed       *float64 `json:"cost_used,omitempty"`
	Source         string   `json:"source,omitempty"`
	State          string   `json:"state,omitempty"`
}

type ssQuotaForm struct {
	Action string
	Inner  string
}

type ssTimeElem struct {
	Datetime string
	Title    string
	Text     string
}

var (
	ssBaseCSSHrefRe = regexp.MustCompile(`<link\b[^>]*href="(/static/tokemon/base\.css\?v=[^"]+)"[^>]*>`)
	ssScriptBlockRe = regexp.MustCompile(`(?is)<script\b[^>]*>.*?</script>`)
	ssStaticURLRe   = regexp.MustCompile(`(?is)(?:href|src)\s*=\s*["'](/static/[^"'\s>]+)["']|url\(\s*["']?(/static/[^"'\s)]+)["']?\s*\)`)
	ssSkipLinkRe    = regexp.MustCompile(`<a\b[^>]*class="[^"]*\bskip-link\b[^"]*"[^>]*href="#main-content"`)
	ssMainTargetRe  = regexp.MustCompile(`<main\b[^>]*id="main-content"`)
	ssNavBlockRe    = regexp.MustCompile(`(?s)<nav\b[^>]*class="[^"]*\bnav\b[^"]*"[^>]*>(.*?)</nav>`)
	ssTopbarBlockRe = regexp.MustCompile(`(?s)<header\b[^>]*class="[^"]*\btopbar\b[^"]*"[^>]*>(.*?)</header>`)
	ssSettingsLnkRe = regexp.MustCompile(`(?is)<a\b[^>]*class="[^"]*\bsettings-link\b[^"]*"[^>]*>`)
	ssActiveWordRe  = regexp.MustCompile(`(?:^|\s)active(?:\s|$)`)
	ssFooterBlockRe = regexp.MustCompile(`(?s)<footer\b[^>]*>.*?</footer>`)
	ssHrefRe        = regexp.MustCompile(`href="([^"]+)"`)
	ssLinkTagRe     = regexp.MustCompile(`<a\b[^>]*>.*?</a>`)
	ssFormBlockRe   = regexp.MustCompile(`(?is)<form\b([^>]*)>(.*?)</form>`)
	ssInputTagRe    = regexp.MustCompile(`(?is)<input\b([^>]*)>`)
	ssButtonTagRe   = regexp.MustCompile(`(?is)<button\b([^>]*)>(.*?)</button>`)
	ssAttrPairRe    = regexp.MustCompile(`(?is)\b([a-z0-9_:-]+)\s*=\s*(?:"([^"]*)"|'([^']*)')`)
	ssTableBodyRe   = regexp.MustCompile(`(?s)<tbody>(.*?)</tbody>`)
	ssTimeTagRe     = regexp.MustCompile(`(?is)<time\b([^>]*)>(.*?)</time>`)
	ssHTMLTagRe     = regexp.MustCompile(`<[^>]+>`)
)

func ssNoRedirectClient(client *http.Client) *http.Client {
	clone := *client
	clone.CheckRedirect = func(*http.Request, []*http.Request) error {
		return http.ErrUseLastResponse
	}
	return &clone
}

func ssDo(t *testing.T, client *http.Client, method, fullURL string, body []byte, headers map[string]string) (int, http.Header, []byte) {
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

func ssGetHTML(t *testing.T, client *http.Client, fullURL string) string {
	t.Helper()
	code, _, body := ssDo(t, client, http.MethodGet, fullURL, nil, map[string]string{"Accept": "text/html"})
	if code != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200\nbody: %s", fullURL, code, string(body))
	}
	return string(body)
}

func ssWaitSessions(t *testing.T, client *http.Client, fullURL string, pred func([]ssSessionRow) bool) []ssSessionRow {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last []ssSessionRow
	for {
		code, _, body := ssDo(t, client, http.MethodGet, fullURL, nil, nil)
		if code != http.StatusOK {
			t.Fatalf("GET %s status = %d: %s", fullURL, code, string(body))
		}
		var rows []ssSessionRow
		if err := json.Unmarshal(body, &rows); err != nil {
			t.Fatalf("decode %s: %v\nbody: %s", fullURL, err, string(body))
		}
		last = rows
		if pred(rows) {
			return rows
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s condition (last rows=%d: %+v)", fullURL, len(last), last)
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func ssWaitHTML(t *testing.T, client *http.Client, fullURL string, pred func(string) bool) string {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var last string
	for {
		body := ssGetHTML(t, client, fullURL)
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

func ssPostForm(t *testing.T, client *http.Client, fullURL, originBase, refererURL string, form url.Values) (int, http.Header, []byte) {
	t.Helper()
	headers := map[string]string{
		"Content-Type": "application/x-www-form-urlencoded",
	}
	if originBase != "" {
		headers["Origin"] = originBase
	}
	if refererURL != "" {
		headers["Referer"] = refererURL
	}
	return ssDo(t, client, http.MethodPost, fullURL, []byte(form.Encode()), headers)
}

func ssPostAliases(t *testing.T, s *testServer, client *http.Client, form url.Values, followRedirect bool) (int, http.Header, []byte) {
	t.Helper()
	if !form.Has("csrf_token") {
		settingsHTML := ssGetHTML(t, client, s.BaseURL+"/settings")
		form.Set("csrf_token", extractCSRFToken(t, []byte(settingsHTML)))
	}
	useClient := client
	if !followRedirect {
		useClient = ssNoRedirectClient(client)
	}
	return ssPostForm(t, useClient, s.BaseURL+"/settings/aliases", s.BaseURL, s.BaseURL+"/settings", form)
}

func ssParseTagAttrs(attrStr string) map[string]string {
	attrs := make(map[string]string)
	for _, m := range ssAttrPairRe.FindAllStringSubmatch(attrStr, -1) {
		val := m[2]
		if val == "" {
			val = m[3]
		}
		attrs[strings.ToLower(m[1])] = html.UnescapeString(val)
	}
	return attrs
}

func ssExtractTimeElems(fragment string) []ssTimeElem {
	var out []ssTimeElem
	for _, m := range ssTimeTagRe.FindAllStringSubmatch(fragment, -1) {
		attrs := ssParseTagAttrs(m[1])
		if attrs["datetime"] == "" || attrs["title"] == "" {
			continue
		}
		text := strings.TrimSpace(ssHTMLTagRe.ReplaceAllString(m[2], ""))
		out = append(out, ssTimeElem{
			Datetime: attrs["datetime"],
			Title:    attrs["title"],
			Text:     text,
		})
	}
	return out
}

func ssCopyHiddenInputs(formInner string, dst url.Values) {
	for _, im := range ssInputTagRe.FindAllStringSubmatch(formInner, -1) {
		attrs := ssParseTagAttrs(im[1])
		name := attrs["name"]
		if name == "" || name == "csrf_token" {
			continue
		}
		if strings.EqualFold(attrs["type"], "hidden") && !dst.Has(name) {
			dst.Set(name, attrs["value"])
		}
	}
}

func ssCopySubmitButton(formInner string, wantDelete bool, dst url.Values) {
	for _, bm := range ssButtonTagRe.FindAllStringSubmatch(formInner, -1) {
		attrs := ssParseTagAttrs(bm[1])
		btnType := strings.ToLower(attrs["type"])
		if btnType == "button" || btnType == "reset" {
			continue
		}
		btnText := strings.ToLower(strings.TrimSpace(ssHTMLTagRe.ReplaceAllString(bm[2], "")))
		btnAction := strings.ToLower(attrs["formaction"])
		btnVal := strings.ToLower(attrs["value"])
		isDel := strings.Contains(btnAction, "delete") || strings.Contains(btnAction, "remove") ||
			strings.Contains(btnVal, "delete") || strings.Contains(btnVal, "remove") ||
			strings.Contains(btnText, "delete") || strings.Contains(btnText, "remove")
		if isDel != wantDelete {
			continue
		}
		if name := attrs["name"]; name != "" && !dst.Has(name) {
			dst.Set(name, attrs["value"])
		}
		return
	}
}

func ssDiscoverQuotaForms(settingsHTML string) (createForm ssQuotaForm, editForms []ssQuotaForm, deleteForms []ssQuotaForm) {
	matches := ssFormBlockRe.FindAllStringSubmatch(settingsHTML, -1)
	for _, m := range matches {
		attrs := ssParseTagAttrs(m[1])
		action := attrs["action"]
		if action == "" {
			continue
		}
		inner := m[2]
		lowerAction := strings.ToLower(action)
		lowerInner := strings.ToLower(inner)
		if !strings.Contains(lowerAction, "quota") && !strings.Contains(lowerInner, "tokens_per_month") && !strings.Contains(lowerInner, "quota") {
			continue
		}

		var hasID, hasEditFields, hasDeleteHidden, hasUpdateHidden, hasHiddenProvider, hasHiddenModel, hasUpsertHidden bool
		for _, im := range ssInputTagRe.FindAllStringSubmatch(inner, -1) {
			ia := ssParseTagAttrs(im[1])
			name := strings.ToLower(ia["name"])
			val := strings.ToLower(ia["value"])
			if name == "id" || name == "quota_id" {
				hasID = true
			}
			if name == "label" || name == "tokens_per_month" || name == "cost_per_month" || name == "reset_day" {
				hasEditFields = true
			}
			if strings.EqualFold(ia["type"], "hidden") {
				if name == "provider" {
					hasHiddenProvider = true
				}
				if name == "model" {
					hasHiddenModel = true
				}
				if val == "delete" || val == "remove" {
					hasDeleteHidden = true
				}
				if val == "update" || val == "edit" {
					hasUpdateHidden = true
				}
				if name == "action" && val == "upsert" {
					hasUpsertHidden = true
				}
			}
		}

		var hasDeleteBtn bool
		var deleteBtnAction string
		for _, bm := range ssButtonTagRe.FindAllStringSubmatch(inner, -1) {
			ba := ssParseTagAttrs(bm[1])
			btnType := strings.ToLower(ba["type"])
			if btnType == "button" || btnType == "reset" {
				continue
			}
			btnText := strings.ToLower(strings.TrimSpace(ssHTMLTagRe.ReplaceAllString(bm[2], "")))
			btnAction := strings.ToLower(ba["formaction"])
			btnVal := strings.ToLower(ba["value"])
			if strings.Contains(btnAction, "delete") || strings.Contains(btnAction, "remove") ||
				strings.Contains(btnVal, "delete") || strings.Contains(btnVal, "remove") ||
				strings.Contains(btnText, "delete") || strings.Contains(btnText, "remove") {
				hasDeleteBtn = true
				if ba["formaction"] != "" && deleteBtnAction == "" {
					deleteBtnAction = ba["formaction"]
				}
			}
		}

		lowerClass := strings.ToLower(attrs["class"])
		lowerAria := strings.ToLower(attrs["aria-label"])
		isEditAction := strings.Contains(lowerAction, "edit") || strings.Contains(lowerAction, "update") ||
			strings.Contains(lowerClass, "quota-edit") || strings.Contains(lowerAria, "edit quota") ||
			(hasUpsertHidden && hasHiddenProvider && hasHiddenModel)
		isDeleteAction := strings.Contains(lowerAction, "delete") || strings.Contains(lowerAction, "remove")

		switch {
		case hasEditFields && (hasID || isEditAction || hasUpdateHidden):
			editForms = append(editForms, ssQuotaForm{Action: action, Inner: inner})
			if hasDeleteBtn {
				delAct := action
				if deleteBtnAction != "" {
					delAct = deleteBtnAction
				}
				deleteForms = append(deleteForms, ssQuotaForm{Action: delAct, Inner: inner})
			}
		case isDeleteAction || hasDeleteHidden || hasDeleteBtn:
			delAct := action
			if deleteBtnAction != "" {
				delAct = deleteBtnAction
			}
			deleteForms = append(deleteForms, ssQuotaForm{Action: delAct, Inner: inner})
		default:
			if createForm.Action == "" {
				createForm = ssQuotaForm{Action: action, Inner: inner}
			}
		}
	}
	return createForm, editForms, deleteForms
}

func ssSubmitQuotaCreate(t *testing.T, s *testServer, client *http.Client, values url.Values, followRedirect bool) (int, http.Header, []byte) {
	t.Helper()
	settingsHTML := ssGetHTML(t, client, s.BaseURL+"/settings")
	createForm, _, _ := ssDiscoverQuotaForms(settingsHTML)
	if createForm.Action == "" {
		t.Fatalf("/settings missing quota create form action")
	}
	ssCopyHiddenInputs(createForm.Inner, values)
	ssCopySubmitButton(createForm.Inner, false, values)
	if !values.Has("csrf_token") {
		values.Set("csrf_token", extractCSRFToken(t, []byte(settingsHTML)))
	}
	target := createForm.Action
	if !strings.HasPrefix(target, "http") {
		target = s.BaseURL + target
	}
	useClient := client
	if !followRedirect {
		useClient = ssNoRedirectClient(client)
	}
	return ssPostForm(t, useClient, target, s.BaseURL, s.BaseURL+"/settings", values)
}

func ssExtractNavBlock(t *testing.T, pageName, htmlBody string) string {
	t.Helper()
	m := ssNavBlockRe.FindStringSubmatch(htmlBody)
	if len(m) < 2 {
		t.Fatalf("%s missing <nav class=\"nav\">", pageName)
	}
	return m[1]
}

func ssExtractSelectBlock(t *testing.T, htmlBody, selectName string) string {
	t.Helper()
	re := regexp.MustCompile(`(?is)<select\b[^>]*name="` + regexp.QuoteMeta(selectName) + `"[^>]*>(.*?)</select>`)
	m := re.FindStringSubmatch(htmlBody)
	if len(m) < 2 {
		t.Fatalf("missing <select name=%q> in HTML", selectName)
	}
	return m[1]
}

func ssExtractSection(body, start, end string) string {
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

func ssSeedChromeFixture(t *testing.T, s *testServer) (sessionID, modelID string) {
	t.Helper()
	sessionID = "sess-chrome-f12-1"
	modelID = "gpt-5.4"
	s.ingestEvents(t, []ingestEvent{{
		EventID:      "evt-chrome-f12-1",
		Timestamp:    time.Now().UTC().Add(-10 * time.Minute).Format(time.RFC3339Nano),
		MachineID:    "m-chrome-1",
		SessionID:    sessionID,
		Project:      "proj-chrome",
		Provider:     "openai",
		Model:        modelID,
		Tool:         "codex",
		InputTokens:  i64(800),
		OutputTokens: i64(200),
		TotalTokens:  i64(1000),
		Cost:         f64(0.005),
		DurationMS:   i64(2500),
	}})
	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 1000)
	return sessionID, modelID
}

// --- Area 1: Feature 12 (Shared Chrome & CAR-205 Extension) ---

func TestChrome_F12_T1_BaseCSSVersionedLinkAndFetchAcrossAllPages(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{
		IngestToken:   "chrome-f12-t1-1",
		Username:      "admin",
		Password:      "chrome-pass-123",
		EnableTokedex: true,
	})
	sessID, modelID := ssSeedChromeFixture(t, s)
	client := s.authClient(t)

	pages := []string{
		"/",
		"/analytics",
		"/sessions",
		"/sessions/" + url.PathEscape(sessID),
		"/settings",
		"/insights",
		"/tokedex",
		"/tokedex/" + url.PathEscape(modelID),
	}

	var sharedCSSURL string
	for _, p := range pages {
		body := ssGetHTML(t, client, s.BaseURL+p)
		m := ssBaseCSSHrefRe.FindStringSubmatch(body)
		if len(m) < 2 {
			t.Fatalf("page %s missing <link ... href=\"/static/tokemon/base.css?v=...\">", p)
		}
		if sharedCSSURL == "" {
			sharedCSSURL = m[1]
		} else if m[1] != sharedCSSURL {
			t.Fatalf("page %s base.css URL = %q, want consistent %q", p, m[1], sharedCSSURL)
		}
	}

	code, headers, cssBytes := ssDo(t, client, http.MethodGet, s.BaseURL+sharedCSSURL, nil, nil)
	if code != http.StatusOK {
		t.Fatalf("GET %s status = %d, want 200", sharedCSSURL, code)
	}
	if ct := headers.Get("Content-Type"); !strings.Contains(ct, "text/css") {
		t.Fatalf("GET %s Content-Type = %q, want text/css", sharedCSSURL, ct)
	}
	css := string(cssBytes)
	for _, tok := range []string{"#10110f", "--bg", ".topbar", ".nav", "footer"} {
		if !strings.Contains(css, tok) {
			t.Fatalf("base.css missing shared token/selector %q", tok)
		}
	}
}

func TestChrome_F12_T1_ConsistentTopbarNavActionsAndFooterAcrossPages(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{
		IngestToken:   "chrome-f12-t1-2",
		Username:      "admin",
		Password:      "chrome-pass-123",
		EnableTokedex: true,
	})
	sessID, modelID := ssSeedChromeFixture(t, s)
	client := s.authClient(t)

	pages := []string{
		"/",
		"/analytics",
		"/sessions",
		"/sessions/" + url.PathEscape(sessID),
		"/settings",
		"/insights",
		"/tokedex",
		"/tokedex/" + url.PathEscape(modelID),
	}

	for _, p := range pages {
		body := ssGetHTML(t, client, s.BaseURL+p)
		if !ssSkipLinkRe.MatchString(body) {
			t.Fatalf("page %s missing skip-link targeting #main-content", p)
		}
		if !ssMainTargetRe.MatchString(body) {
			t.Fatalf("page %s missing <main id=\"main-content\">", p)
		}
		topbar := ssTopbarBlockRe.FindStringSubmatch(body)
		if len(topbar) < 2 {
			t.Fatalf("page %s missing <header class=\"topbar\">", p)
		}
		settingsTag := ssSettingsLnkRe.FindString(topbar[1])
		if settingsTag == "" || !strings.Contains(settingsTag, `href="/settings"`) {
			t.Fatalf("page %s topbar missing .settings-link href=\"/settings\"", p)
		}
		if !strings.Contains(topbar[1], `class="logout-form"`) {
			t.Fatalf("page %s topbar missing .logout-form when password configured", p)
		}
		nav := ssExtractNavBlock(t, p, topbar[1])
		for _, wantHref := range []string{`href="/"`, `href="/analytics"`, `href="/tokedex"`, `href="/sessions"`} {
			if !strings.Contains(nav, wantHref) {
				t.Fatalf("page %s nav missing %s", p, wantHref)
			}
		}
		if !ssFooterBlockRe.MatchString(body) {
			t.Fatalf("page %s missing <footer>", p)
		}
	}
}

func TestChrome_F12_T1_ActiveNavStateMatchesCurrentRoute(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{
		IngestToken:   "chrome-f12-t1-3",
		Username:      "admin",
		Password:      "chrome-pass-123",
		EnableTokedex: true,
	})
	sessID, modelID := ssSeedChromeFixture(t, s)
	client := s.authClient(t)

	cases := []struct {
		page       string
		wantNav    string
		wantSetAct bool
	}{
		{"/", "/", false},
		{"/analytics", "/analytics", false},
		{"/tokedex", "/tokedex", false},
		{"/tokedex/" + url.PathEscape(modelID), "/tokedex", false},
		{"/sessions", "/sessions", false},
		{"/sessions/" + url.PathEscape(sessID), "/sessions", false},
		{"/settings", "", true},
		{"/insights", "", false},
	}

	for _, tc := range cases {
		body := ssGetHTML(t, client, s.BaseURL+tc.page)
		nav := ssExtractNavBlock(t, tc.page, body)
		links := ssLinkTagRe.FindAllString(nav, -1)
		activeCount := 0
		for _, link := range links {
			isActive := strings.Contains(link, "active") || strings.Contains(link, `aria-current="page"`)
			if isActive {
				activeCount++
				if tc.wantNav == "" || !strings.Contains(link, fmt.Sprintf(`href="%s"`, tc.wantNav)) {
					t.Fatalf("page %s has unexpected active nav link %s (want %q)", tc.page, link, tc.wantNav)
				}
			}
		}
		if tc.wantNav != "" && activeCount != 1 {
			t.Fatalf("page %s active nav link count = %d, want 1 (href=%q); nav=%s", tc.page, activeCount, tc.wantNav, nav)
		}
		if tc.wantNav == "" && activeCount != 0 {
			t.Fatalf("page %s active nav link count = %d, want 0; nav=%s", tc.page, activeCount, nav)
		}
		topbar := ssTopbarBlockRe.FindString(body)
		settingsTag := ssSettingsLnkRe.FindString(topbar)
		if settingsTag == "" {
			t.Fatalf("page %s topbar missing .settings-link", tc.page)
		}
		setAttrs := ssParseTagAttrs(settingsTag)
		setActive := ssActiveWordRe.MatchString(setAttrs["class"]) || setAttrs["aria-current"] == "page"
		if setActive != tc.wantSetAct {
			t.Fatalf("page %s settings-link active = %v, want %v (tag=%s)", tc.page, setActive, tc.wantSetAct, settingsTag)
		}
	}
}

func TestChrome_F12_T1_TokedexNavLinkConditionalOnFlag(t *testing.T) {
	requireE2E(t)
	for _, enabled := range []bool{false, true} {
		s := startTestServer(t, serverOpts{
			IngestToken:   fmt.Sprintf("chrome-f12-flag-%v", enabled),
			EnableTokedex: enabled,
		})
		sessID, _ := ssSeedChromeFixture(t, s)
		client := s.authClient(t)

		for _, p := range []string{"/", "/analytics", "/sessions", "/sessions/" + url.PathEscape(sessID), "/settings", "/insights"} {
			body := ssGetHTML(t, client, s.BaseURL+p)
			nav := ssExtractNavBlock(t, p, body)
			hasTokedex := strings.Contains(nav, `href="/tokedex"`)
			if hasTokedex != enabled {
				t.Fatalf("EnableTokedex=%v on %s: nav has /tokedex = %v, want %v", enabled, p, hasTokedex, enabled)
			}
		}
	}
}

func TestChrome_F12_T1_CAR205ExtendedToDetailRoutes(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{
		IngestToken:   "chrome-f12-car205",
		EnableTokedex: true,
	})
	sessID, modelID := ssSeedChromeFixture(t, s)
	client := s.authClient(t)

	for _, p := range []string{"/sessions/" + url.PathEscape(sessID), "/tokedex/" + url.PathEscape(modelID)} {
		body := ssGetHTML(t, client, s.BaseURL+p)
		nav := ssExtractNavBlock(t, p, body)
		if strings.Contains(nav, `href="/insights"`) {
			t.Fatalf("CAR-205 violation on %s: primary nav linked /insights: %s", p, nav)
		}
	}
}

func TestChrome_F12_T2_LoginPageOmitsAppNavAndPreservesSkipLink(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{
		IngestToken: "chrome-f12-t2-login",
		Username:    "admin",
		Password:    "chrome-pass-123",
	})
	code, _, bodyBytes := ssDo(t, s.Client, http.MethodGet, s.BaseURL+"/login", nil, map[string]string{"Accept": "text/html"})
	if code != http.StatusOK {
		t.Fatalf("GET /login status = %d, want 200", code)
	}
	body := string(bodyBytes)
	if !ssSkipLinkRe.MatchString(body) {
		t.Fatalf("/login missing skip-link to #main-content")
	}
	if !ssMainTargetRe.MatchString(body) {
		t.Fatalf("/login missing <main id=\"main-content\">")
	}
	if !ssBaseCSSHrefRe.MatchString(body) {
		t.Fatalf("/login missing /static/tokemon/base.css?v= stylesheet link")
	}
	if ssTopbarBlockRe.MatchString(body) || ssNavBlockRe.MatchString(body) || ssFooterBlockRe.MatchString(body) {
		t.Fatalf("/login must omit app topbar, nav, and footer")
	}
}

func TestChrome_F12_T2_AnonymousHTMLRedirectsToLoginWithNext(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{
		IngestToken:   "chrome-f12-t2-redir",
		Username:      "admin",
		Password:      "chrome-pass-123",
		EnableTokedex: true,
	})
	sessID, modelID := ssSeedChromeFixture(t, s)

	for _, p := range []string{
		"/sessions/" + url.PathEscape(sessID),
		"/tokedex",
		"/tokedex/" + url.PathEscape(modelID),
		"/insights",
	} {
		code, hdrs, _ := ssDo(t, s.Client, http.MethodGet, s.BaseURL+p, nil, map[string]string{"Accept": "text/html"})
		if code != http.StatusSeeOther {
			t.Fatalf("anonymous GET %s status = %d, want 303", p, code)
		}
		loc := hdrs.Get("Location")
		wantPrefix := "/login?next=" + url.QueryEscape(p)
		if loc != wantPrefix {
			t.Fatalf("anonymous GET %s Location = %q, want %q", p, loc, wantPrefix)
		}
	}
}

func TestChrome_F12_T2_LogoutHiddenWhenPasswordUnconfigured(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{
		IngestToken:   "chrome-f12-t2-nologout",
		EnableTokedex: true,
	})
	sessID, modelID := ssSeedChromeFixture(t, s)
	client := s.authClient(t)

	for _, p := range []string{
		"/",
		"/analytics",
		"/sessions",
		"/sessions/" + url.PathEscape(sessID),
		"/settings",
		"/insights",
		"/tokedex",
		"/tokedex/" + url.PathEscape(modelID),
	} {
		body := ssGetHTML(t, client, s.BaseURL+p)
		if strings.Contains(body, "logout-form") || strings.Contains(body, `action="/logout"`) {
			t.Fatalf("page %s rendered logout form without password account", p)
		}
	}
}

func TestChrome_F12_T2_NoUnversionedStaticRefsAcrossChrome(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{
		IngestToken:   "chrome-f12-t2-ver",
		Username:      "admin",
		Password:      "chrome-pass-123",
		EnableTokedex: true,
	})
	sessID, modelID := ssSeedChromeFixture(t, s)
	client := s.authClient(t)

	pages := []string{
		"/",
		"/analytics",
		"/sessions",
		"/sessions/" + url.PathEscape(sessID),
		"/settings",
		"/insights",
		"/tokedex",
		"/tokedex/" + url.PathEscape(modelID),
	}
	for _, p := range pages {
		body := ssGetHTML(t, client, s.BaseURL+p)
		nonScriptBody := ssScriptBlockRe.ReplaceAllString(body, "")
		matches := ssStaticURLRe.FindAllStringSubmatch(nonScriptBody, -1)
		if len(matches) == 0 {
			t.Fatalf("page %s had zero /static/ references", p)
		}
		for _, m := range matches {
			ref := m[1]
			if ref == "" {
				ref = m[2]
			}
			if !strings.Contains(ref, "?v=") {
				t.Fatalf("page %s contains unversioned static ref %q", p, ref)
			}
		}
	}
}

func TestChrome_F12_T2_NoTemplateSanitizationMarkersInChrome(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{
		IngestToken:   "chrome-f12-t2-zgot",
		Username:      "admin",
		Password:      "chrome-pass-123",
		EnableTokedex: true,
	})
	sessID, modelID := ssSeedChromeFixture(t, s)
	client := s.authClient(t)

	code, _, loginBody := ssDo(t, s.Client, http.MethodGet, s.BaseURL+"/login", nil, map[string]string{"Accept": "text/html"})
	if code != http.StatusOK {
		t.Fatalf("GET /login status = %d", code)
	}
	if strings.Contains(string(loginBody), "ZgotmplZ") {
		t.Fatalf("/login contains ZgotmplZ")
	}

	for _, p := range []string{
		"/",
		"/analytics",
		"/sessions",
		"/sessions/" + url.PathEscape(sessID),
		"/settings",
		"/insights",
		"/tokedex",
		"/tokedex/" + url.PathEscape(modelID),
	} {
		body := ssGetHTML(t, client, s.BaseURL+p)
		if strings.Contains(body, "ZgotmplZ") {
			t.Fatalf("page %s contains ZgotmplZ template marker", p)
		}
	}
}

// --- Area 2: Feature 3 & 16 (/sessions & /sessions/<id>) ---

func TestSessions_F16_T1_ProjectFilterFacetAndURLPreservation(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "sess-f16-t1-1"})
	now := time.Now().UTC()
	s.ingestEvents(t, []ingestEvent{
		{
			EventID:     "f16-proj-1",
			Timestamp:   now.Add(-20 * time.Minute).Format(time.RFC3339Nano),
			MachineID:   "m-sess-1",
			SessionID:   "sess-proj-zulu",
			Project:     "zulu-proj",
			Provider:    "openai",
			Model:       "gpt-5.4",
			Tool:        "codex",
			TotalTokens: i64(100),
		},
		{
			EventID:     "f16-proj-2",
			Timestamp:   now.Add(-10 * time.Minute).Format(time.RFC3339Nano),
			MachineID:   "m-sess-1",
			SessionID:   "sess-proj-alpha",
			Project:     "alpha-proj",
			Provider:    "openai",
			Model:       "gpt-5.4",
			Tool:        "codex",
			TotalTokens: i64(200),
		},
	})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 300)

	htmlAll := ssGetHTML(t, client, s.BaseURL+"/sessions")
	projSelect := ssExtractSelectBlock(t, htmlAll, "project")
	alphaIdx := strings.Index(projSelect, `value="alpha-proj"`)
	zuluIdx := strings.Index(projSelect, `value="zulu-proj"`)
	if alphaIdx < 0 || zuluIdx < 0 {
		t.Fatalf("project select missing facets: %s", projSelect)
	}
	if alphaIdx > zuluIdx {
		t.Fatalf("project facets not sorted alphabetically: %s", projSelect)
	}

	filteredHTML := ssGetHTML(t, client, s.BaseURL+"/sessions?project=alpha-proj")
	tbody := ssTableBodyRe.FindString(filteredHTML)
	if !strings.Contains(tbody, "sess-proj-alpha") || strings.Contains(tbody, "sess-proj-zulu") {
		t.Fatalf("/sessions?project=alpha-proj unexpected tbody: %s", tbody)
	}
	if !strings.Contains(filteredHTML, "project=alpha-proj") {
		t.Fatalf("/sessions?project=alpha-proj did not preserve project param in sort/export links")
	}

	rows := ssWaitSessions(t, client, s.BaseURL+"/api/v1/sessions?project=alpha-proj", func(r []ssSessionRow) bool {
		return len(r) == 1
	})
	if rows[0].SessionID != "sess-proj-alpha" || rows[0].Project != "alpha-proj" {
		t.Fatalf("unexpected /api/v1/sessions?project=alpha-proj row: %+v", rows[0])
	}
}

func TestSessions_F16_T1_SearchQueryMatchesSessionIDAndProject(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "sess-f16-t1-2"})
	now := time.Now().UTC()
	s.ingestEvents(t, []ingestEvent{
		{
			EventID:     "f16-q-1",
			Timestamp:   now.Add(-30 * time.Minute).Format(time.RFC3339Nano),
			MachineID:   "m-q-1",
			SessionID:   "sess-needle-991",
			Project:     "backend-core",
			Provider:    "openai",
			Model:       "gpt-5.4",
			Tool:        "codex",
			TotalTokens: i64(150),
		},
		{
			EventID:     "f16-q-2",
			Timestamp:   now.Add(-20 * time.Minute).Format(time.RFC3339Nano),
			MachineID:   "m-q-1",
			SessionID:   "sess-other-111",
			Project:     "Web-Portal-App",
			Provider:    "openai",
			Model:       "gpt-5.4",
			Tool:        "codex",
			TotalTokens: i64(250),
		},
		{
			EventID:     "f16-q-3",
			Timestamp:   now.Add(-10 * time.Minute).Format(time.RFC3339Nano),
			MachineID:   "m-q-1",
			SessionID:   "sess-unrelated-777",
			Project:     "cli-tools",
			Provider:    "openai",
			Model:       "gpt-5.4",
			Tool:        "codex",
			TotalTokens: i64(350),
		},
	})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 750)

	// Case-insensitive match on session_id.
	rowsByID := ssWaitSessions(t, client, s.BaseURL+"/api/v1/sessions?q=NEEDLE", func(r []ssSessionRow) bool {
		return len(r) == 1
	})
	if rowsByID[0].SessionID != "sess-needle-991" {
		t.Fatalf("q=NEEDLE returned %+v", rowsByID)
	}

	// Case-insensitive match on project.
	htmlByProj := ssGetHTML(t, client, s.BaseURL+"/sessions?q=portal")
	tbody := ssTableBodyRe.FindString(htmlByProj)
	if !strings.Contains(tbody, "sess-other-111") || strings.Contains(tbody, "sess-needle-991") || strings.Contains(tbody, "sess-unrelated-777") {
		t.Fatalf("/sessions?q=portal unexpected rows: %s", tbody)
	}
	if !strings.Contains(htmlByProj, "q=portal") {
		t.Fatalf("/sessions?q=portal did not preserve q in sort/export links")
	}
}

func TestSessions_F16_T1_PaginationBeyondLimit100(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "sess-f16-t1-3"})
	now := time.Now().UTC()
	const totalSessions = 115
	events := make([]ingestEvent, 0, totalSessions)
	for i := 0; i < totalSessions; i++ {
		events = append(events, ingestEvent{
			EventID:     fmt.Sprintf("f16-pg-evt-%03d", i),
			Timestamp:   now.Add(-time.Duration(totalSessions-i) * time.Minute).Format(time.RFC3339Nano),
			MachineID:   "m-pg",
			SessionID:   fmt.Sprintf("sess-page-%03d", i),
			Project:     "proj-paged",
			Provider:    "openai",
			Model:       "gpt-5.4",
			Tool:        "codex",
			TotalTokens: i64(int64(100 + i)),
		})
	}
	s.ingestEvents(t, events)
	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 18055)

	fromStr := now.Add(-24 * time.Hour).Format("2006-01-02")
	toStr := now.Add(24 * time.Hour).Format("2006-01-02")
	baseQuery := fmt.Sprintf("/sessions?period=all&from=%s&to=%s&project=proj-paged&q=sess-page&sort=tokens&dir=desc", fromStr, toStr)

	page1HTML := ssGetHTML(t, client, s.BaseURL+baseQuery)
	// Newest/highest token session (sess-page-114 -> 214 tokens) on page 1, lowest (sess-page-000 -> 100 tokens) NOT on page 1.
	page1Body := ssTableBodyRe.FindString(page1HTML)
	if !strings.Contains(page1Body, "sess-page-114") {
		t.Fatalf("page 1 missing highest-token session sess-page-114")
	}
	if strings.Contains(page1Body, "sess-page-000") {
		t.Fatalf("page 1 unexpectedly included 115th row sess-page-000 without pagination")
	}

	// Walk pagination links dynamically (page=2, page=3, ...) so any page size < 115 (e.g. 50 or 100) reaches row 115.
	curHTML := page1HTML
	foundLowest := false
	for nextPage := 2; nextPage <= 10; nextPage++ {
		pageRe := regexp.MustCompile(fmt.Sprintf(`href="([^"]*(?:\?|&amp;|&)page=%d(?:(?:&amp;|&)[^"]*)?)"`, nextPage))
		m := pageRe.FindStringSubmatch(curHTML)
		if len(m) < 2 {
			if nextPage == 2 {
				t.Fatalf("page 1 HTML missing pagination link to page=2 for 115 sessions")
			}
			break
		}
		nextHref := html.UnescapeString(m[1])
		for _, wantParam := range []string{"project=proj-paged", "q=sess-page", "sort=tokens", "dir=desc", "from=" + fromStr, "to=" + toStr} {
			if !strings.Contains(nextHref, wantParam) {
				t.Fatalf("pagination link %q lost query parameter %q", nextHref, wantParam)
			}
		}
		if !strings.HasPrefix(nextHref, "/sessions") && !strings.HasPrefix(nextHref, "?") {
			t.Fatalf("unexpected pagination link target %q", nextHref)
		}
		nextURL := s.BaseURL + nextHref
		if strings.HasPrefix(nextHref, "?") {
			nextURL = s.BaseURL + "/sessions" + nextHref
		}

		curHTML = ssGetHTML(t, client, nextURL)
		curBody := ssTableBodyRe.FindString(curHTML)
		if strings.Contains(curBody, "sess-page-114") {
			t.Fatalf("page %d duplicated page 1 row sess-page-114", nextPage)
		}
		if strings.Contains(curBody, "sess-page-000") {
			foundLowest = true
			break
		}
	}
	if !foundLowest {
		t.Fatalf("pagination never reached lowest-token session sess-page-000 beyond row 100")
	}
}

func TestSessions_F16_T1_CompactTokenDisplayWithExactTitleAndLocalizedTime(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{
		IngestToken: "sess-f16-t1-4",
		Timezone:    "America/New_York",
	})
	ts := time.Now().UTC().Add(-5 * time.Minute)
	s.ingestEvents(t, []ingestEvent{{
		EventID:      "f16-compact-1",
		Timestamp:    ts.Format(time.RFC3339Nano),
		MachineID:    "m-compact",
		SessionID:    "sess-compact-1",
		Project:      "proj-compact",
		Provider:     "openai",
		Model:        "gpt-5.4",
		Tool:         "codex",
		InputTokens:  i64(1_000_000),
		OutputTokens: i64(500_000),
		TotalTokens:  i64(1_500_000),
		Cost:         f64(10.0),
		DurationMS:   i64(120_000),
	}})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 1_500_000)

	htmlBody := ssGetHTML(t, client, s.BaseURL+"/sessions")
	tbody := ssTableBodyRe.FindString(htmlBody)
	if !strings.Contains(tbody, "1.5M") || !strings.Contains(tbody, "500K") {
		t.Fatalf("/sessions missing compact token display (1.5M / 500K) in tbody: %s", tbody)
	}
	if !strings.Contains(tbody, "1,500,000") || !strings.Contains(tbody, "1,000,000") || !strings.Contains(tbody, "500,000") {
		t.Fatalf("/sessions missing exact comma-formatted token hover/title values in tbody: %s", tbody)
	}

	times := ssExtractTimeElems(tbody)
	if len(times) == 0 {
		t.Fatalf("/sessions missing localized <time datetime=... title=...>...</time>: %s", tbody)
	}
	tm := times[0]
	loc, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatalf("load America/New_York: %v", err)
	}
	parsedDT, err := time.Parse(time.RFC3339, tm.Datetime)
	if err != nil {
		t.Fatalf("parse datetime %q: %v", tm.Datetime, err)
	}
	_, wantOffset := ts.In(loc).Zone()
	_, gotOffset := parsedDT.Zone()
	if gotOffset != wantOffset {
		t.Fatalf("<time datetime=%q> offset=%d, want %d for America/New_York", tm.Datetime, gotOffset, wantOffset)
	}
	if !strings.Contains(tm.Text, "ago") && !strings.Contains(tm.Text, "just now") {
		t.Fatalf("<time> display %q missing concise relative age", tm.Text)
	}
}

func TestSessions_F16_T1_SessionDetailPageMetadataAndModelSwitches(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{
		IngestToken: "sess-f16-t1-5",
		Timezone:    "UTC",
	})
	t0 := time.Now().UTC().Add(-20 * time.Minute)
	t1 := t0.Add(5 * time.Minute)
	sessID := "sess-detail-multi-1"
	s.ingestEvents(t, []ingestEvent{
		{
			EventID:         "f16-det-1",
			Timestamp:       t0.Format(time.RFC3339Nano),
			MachineID:       "m-det-host",
			SessionID:       sessID,
			Project:         "proj-detail",
			Provider:        "openai",
			Model:           "gpt-5.4",
			Tool:            "codex",
			InputTokens:     i64(1000),
			CacheReadTokens: i64(400),
			OutputTokens:    i64(200),
			TotalTokens:     i64(1600),
			Cost:            f64(0.012),
			DurationMS:      i64(3000),
		},
		{
			EventID:         "f16-det-2",
			Timestamp:       t1.Format(time.RFC3339Nano),
			MachineID:       "m-det-host",
			SessionID:       sessID,
			Project:         "proj-detail",
			Provider:        "anthropic",
			Model:           "claude-sonnet-4",
			Tool:            "codex",
			InputTokens:     i64(2000),
			CacheReadTokens: i64(1100),
			OutputTokens:    i64(300),
			TotalTokens:     i64(3400),
			Cost:            f64(0.023),
			DurationMS:      i64(4500),
		},
	})

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 5000)

	// Verify /sessions links into /sessions/<id>.
	sessionsHTML := ssGetHTML(t, client, s.BaseURL+"/sessions")
	if !strings.Contains(sessionsHTML, "/sessions/"+sessID) {
		t.Fatalf("/sessions missing drill-down link to /sessions/%s", sessID)
	}

	detailHTML := ssGetHTML(t, client, s.BaseURL+"/sessions/"+url.PathEscape(sessID))
	for _, want := range []string{
		sessID,
		"proj-detail",
		"m-det-host",
		"Codex",
		"5,000",
		"GPT-5.4",
		"Sonnet 4",
		"<time",
	} {
		if !strings.Contains(detailHTML, want) && !strings.Contains(strings.ToLower(detailHTML), strings.ToLower(want)) {
			t.Fatalf("/sessions/%s missing expected metadata %q", sessID, want)
		}
	}
	// Verify cache hit % (33.3% = 1500 / (3000 + 1500)) matching Tokemon's CacheSummary formula.
	if !strings.Contains(detailHTML, "33.3%") {
		t.Fatalf("/sessions/%s missing cache hit rate 33.3%% (1500/4500)", sessID)
	}
}

func TestSessions_F16_T1_SessionsExportHonorsProjectSearchAndFromToBeyondPageLimit(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "sess-f16-t1-6"})
	now := time.Now().UTC()
	var events []ingestEvent
	for i := 0; i < 108; i++ {
		events = append(events, ingestEvent{
			EventID:     fmt.Sprintf("f16-exp-match-%03d", i),
			Timestamp:   now.Add(-time.Duration(i+1) * time.Minute).Format(time.RFC3339Nano),
			MachineID:   "m-exp",
			SessionID:   fmt.Sprintf("sess-export-match-%03d", i),
			Project:     "proj-target",
			Provider:    "openai",
			Model:       "gpt-5.4",
			Tool:        "codex",
			TotalTokens: i64(10),
		})
	}
	events = append(events, ingestEvent{
		EventID:     "f16-exp-other-proj",
		Timestamp:   now.Add(-30 * time.Second).Format(time.RFC3339Nano),
		MachineID:   "m-exp",
		SessionID:   "sess-export-other",
		Project:     "proj-other",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(10),
	})
	events = append(events, ingestEvent{
		EventID:     "f16-exp-old-date",
		Timestamp:   now.AddDate(0, 0, -45).Format(time.RFC3339Nano),
		MachineID:   "m-exp",
		SessionID:   "sess-export-old",
		Project:     "proj-target",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(10),
	})
	s.ingestEvents(t, events)
	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 1100)

	fromStr := now.AddDate(0, 0, -2).Format("2006-01-02")
	toStr := now.AddDate(0, 0, 1).Format("2006-01-02")
	csvPath := fmt.Sprintf("/api/v1/sessions/export?format=csv&period=all&from=%s&to=%s&project=proj-target&q=export-match", fromStr, toStr)

	code, _, csvBytes := ssDo(t, client, http.MethodGet, s.BaseURL+csvPath, nil, nil)
	if code != http.StatusOK {
		t.Fatalf("GET %s status = %d", csvPath, code)
	}
	recs, err := csv.NewReader(bytes.NewReader(csvBytes)).ReadAll()
	if err != nil {
		t.Fatalf("parse CSV export: %v", err)
	}
	// Header + 108 matching rows (>100 page limit).
	if len(recs) != 109 {
		t.Fatalf("CSV export rows = %d (incl header), want 109 (all 108 matching rows beyond 100 limit)", len(recs))
	}
	csvStr := string(csvBytes)
	if strings.Contains(csvStr, "sess-export-other") || strings.Contains(csvStr, "sess-export-old") {
		t.Fatalf("CSV export leaked non-matching rows")
	}

	jsonPath := fmt.Sprintf("/api/v1/sessions/export?period=all&from=%s&to=%s&project=proj-target&q=export-match", fromStr, toStr)
	var exported []ssSessionRow
	getJSONWithClient(t, client, s.BaseURL+jsonPath, &exported)
	if len(exported) != 108 {
		t.Fatalf("JSON export len = %d, want 108", len(exported))
	}
}

func TestSessions_F16_T2_SessionDetailUnknownIDReturns404AndAuthGated(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{
		IngestToken: "sess-f16-t2-1",
		Username:    "admin",
		Password:    "sess-pass-123",
	})

	// Anonymous checks.
	codeHTML, hdrHTML, _ := ssDo(t, s.Client, http.MethodGet, s.BaseURL+"/sessions/unknown-id-999", nil, map[string]string{"Accept": "text/html"})
	if codeHTML != http.StatusSeeOther || !strings.HasPrefix(hdrHTML.Get("Location"), "/login?next=") {
		t.Fatalf("unauth HTML GET /sessions/unknown-id-999 status=%d loc=%q, want 303 /login?next=...", codeHTML, hdrHTML.Get("Location"))
	}
	codeAPI, _, _ := ssDo(t, s.Client, http.MethodGet, s.BaseURL+"/sessions/unknown-id-999", nil, nil)
	if codeAPI != http.StatusUnauthorized {
		t.Fatalf("unauth non-HTML GET /sessions/unknown-id-999 status=%d, want 401", codeAPI)
	}

	client := s.authClient(t)
	codeAuthed, _, _ := ssDo(t, client, http.MethodGet, s.BaseURL+"/sessions/unknown-id-999", nil, map[string]string{"Accept": "text/html"})
	if codeAuthed != http.StatusNotFound {
		t.Fatalf("authed GET /sessions/unknown-id-999 status = %d, want 404", codeAuthed)
	}
}

func TestSessions_F16_T2_SessionDetailMetadataOnlyNoPromptOrPathLeak(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "sess-f16-t2-2"})

	agentHome := filepath.Join(t.TempDir(), "agent-home")
	stateDB := filepath.Join(t.TempDir(), "agent-state.db")
	emptyCfg := filepath.Join(t.TempDir(), "empty-agent.env")
	rollout := filepath.Join(agentHome, ".codex", "sessions", "2026", "11", "20", "secret-session-rollout.jsonl")
	if err := os.MkdirAll(filepath.Dir(rollout), 0o755); err != nil {
		t.Fatalf("mkdir codex rollout dir: %v", err)
	}
	tBase := time.Now().UTC().Add(-time.Hour)
	sessID := "sess-f16-secret-rollout-1"
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
		"--machine-id", "m-f16-priv",
		"--home", agentHome,
		"--state", stateDB,
		"--adapters", "codex",
	)
	if err != nil {
		t.Fatalf("agent --once failed: %v\nstderr: %s", err, stderr)
	}

	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 140)

	for _, path := range []string{"/sessions?q=secret-repo", "/sessions/" + url.PathEscape(sessID)} {
		body := ssGetHTML(t, client, s.BaseURL+path)
		lower := strings.ToLower(body)
		for _, forbidden := range []string{
			"secret_prompt_do_not_leak",
			"secret_response_do_not_leak",
			"secret_title_do_not_leak",
			"secret_system_instructions",
			"/users/alice",
			".jsonl",
		} {
			if strings.Contains(lower, forbidden) {
				t.Fatalf("surface %s leaked %q", path, forbidden)
			}
		}
	}
}

func TestSessions_F16_T2_SearchSQLInjectionXSSAndPaginationBounds(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "sess-f16-t2-3"})
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "f16-sqli-1",
		MachineID:   "m-sqli",
		SessionID:   "sess-sqli-safe",
		Project:     "proj-sqli",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(50),
	}})
	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 50)

	// SQLi attempt must not bypass filtering or return 5xx.
	code, _, sqliHTML := ssDo(t, client, http.MethodGet, s.BaseURL+"/sessions?q="+url.QueryEscape("' OR 1=1--"), nil, map[string]string{"Accept": "text/html"})
	if code != http.StatusOK {
		t.Fatalf("SQLi q status = %d, want 200", code)
	}
	if strings.Contains(ssTableBodyRe.FindString(string(sqliHTML)), "sess-sqli-safe") {
		t.Fatalf("SQLi query unexpectedly matched unrelated session")
	}

	// XSS attempt must be escaped.
	code, _, xssHTML := ssDo(t, client, http.MethodGet, s.BaseURL+"/sessions?q="+url.QueryEscape(`<script>alert(1)</script>`), nil, map[string]string{"Accept": "text/html"})
	if code != http.StatusOK {
		t.Fatalf("XSS q status = %d, want 200", code)
	}
	if strings.Contains(string(xssHTML), "<script>alert(1)</script>") {
		t.Fatalf("unescaped XSS reflected in /sessions")
	}

	// Pagination boundaries must never 5xx.
	for _, q := range []string{"?page=0", "?page=-5", "?page=999999", "?limit=-1", "?limit=999999", "?page=abc"} {
		c, _, _ := ssDo(t, client, http.MethodGet, s.BaseURL+"/sessions"+q, nil, map[string]string{"Accept": "text/html"})
		if c >= http.StatusInternalServerError {
			t.Fatalf("GET /sessions%s returned 5xx: %d", q, c)
		}
		cAPI, _, _ := ssDo(t, client, http.MethodGet, s.BaseURL+"/api/v1/sessions"+q, nil, nil)
		if cAPI >= http.StatusInternalServerError {
			t.Fatalf("GET /api/v1/sessions%s returned 5xx: %d", q, cAPI)
		}
	}
}

func TestSessions_F16_T2_UnknownTokenAndCostFieldsRemainUnknownNeverZero(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "sess-f16-t2-4"})
	sessID := "sess-unknown-fields-1"
	s.ingestEvents(t, []ingestEvent{{
		EventID:       "f16-unk-1",
		MachineID:     "m-unk",
		SessionID:     sessID,
		Project:       "proj-unk",
		Provider:      "openai",
		Model:         "gpt-5.4",
		Tool:          "codex",
		TokenAccuracy: "unknown",
	}})
	client := s.authClient(t)

	ssWaitSessions(t, client, s.BaseURL+"/api/v1/sessions", func(r []ssSessionRow) bool {
		return len(r) == 1
	})

	listHTML := ssGetHTML(t, client, s.BaseURL+"/sessions")
	tbody := ssTableBodyRe.FindString(listHTML)
	if !strings.Contains(tbody, "Unknown") {
		t.Fatalf("/sessions row missing Unknown for nil token/cost: %s", tbody)
	}
	if strings.Contains(tbody, ">0<") || strings.Contains(tbody, "0K") || strings.Contains(tbody, "$0.00") {
		t.Fatalf("/sessions coerced nil token/cost to zero: %s", tbody)
	}

	detailHTML := ssGetHTML(t, client, s.BaseURL+"/sessions/"+url.PathEscape(sessID))
	if !strings.Contains(detailHTML, "Unknown") {
		t.Fatalf("/sessions/%s missing Unknown for nil token/cost", sessID)
	}
	if strings.Contains(detailHTML, "$0.00") {
		t.Fatalf("/sessions/%s coerced nil cost to $0.00", sessID)
	}
}

func TestSessions_F16_T2_SessionDetailSpecialCharsAndMultiMachineDisambiguation(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "sess-f16-t2-5"})
	specialID := "sha256:abc_123-xyz"
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "f16-spec-1",
		MachineID:   "m-spec-1",
		SessionID:   specialID,
		Project:     "proj-spec",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(250),
	}})
	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 250)

	detailHTML := ssGetHTML(t, client, s.BaseURL+"/sessions/"+url.PathEscape(specialID))
	if !strings.Contains(detailHTML, specialID) {
		t.Fatalf("/sessions/%s did not render special session ID", specialID)
	}

	// Path traversal attempt must return 400 or 404, never 200 or 500.
	code, _, _ := ssDo(t, client, http.MethodGet, s.BaseURL+"/sessions/..%2F..%2Fetc%2Fpasswd", nil, map[string]string{"Accept": "text/html"})
	if code != http.StatusNotFound && code != http.StatusBadRequest {
		t.Fatalf("traversal /sessions/..%%2F..%%2Fetc%%2Fpasswd status = %d, want 400 or 404", code)
	}
}

func TestSessions_F3_T2_SessionsPageRelativeAgeAndTimezoneAcrossSorts(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{
		IngestToken: "sess-f3-t2-tz",
		Timezone:    "Asia/Tokyo",
	})
	ts := time.Now().UTC().Add(-2 * time.Hour)
	s.ingestEvents(t, []ingestEvent{
		{
			EventID:     "f3-tz-1",
			Timestamp:   ts.Format(time.RFC3339Nano),
			MachineID:   "m-tz",
			SessionID:   "sess-tz-tokyo-1",
			Project:     "proj-tz",
			Provider:    "openai",
			Model:       "gpt-5.4",
			Tool:        "codex",
			TotalTokens: i64(500),
		},
		{
			EventID:     "f3-tz-2",
			Timestamp:   ts.Add(-30 * time.Minute).Format(time.RFC3339Nano),
			MachineID:   "m-tz",
			SessionID:   "sess-tz-tokyo-2",
			Project:     "proj-tz",
			Provider:    "openai",
			Model:       "gpt-5.4",
			Tool:        "codex",
			TotalTokens: i64(100),
		},
	})
	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 600)

	htmlBody := ssGetHTML(t, client, s.BaseURL+"/sessions?sort=tokens&dir=desc&page=1")
	tbody := ssTableBodyRe.FindString(htmlBody)
	times := ssExtractTimeElems(tbody)
	if len(times) < 2 {
		t.Fatalf("expected >=2 localized <time> elements in /sessions?sort=tokens, got %d", len(times))
	}
	for _, tm := range times {
		if !strings.HasSuffix(tm.Datetime, "+09:00") {
			t.Fatalf("<time datetime=%q> did not use Asia/Tokyo +09:00 offset", tm.Datetime)
		}
		if !strings.Contains(tm.Title, "JST") && !strings.Contains(tm.Title, "+09") {
			t.Fatalf("<time title=%q> missing JST/+09 timezone label", tm.Title)
		}
		if !strings.Contains(tm.Text, "h ago") && !strings.Contains(tm.Text, "m ago") && !strings.Contains(tm.Text, "just now") {
			t.Fatalf("<time> display %q missing relative age", tm.Text)
		}
	}
}

// --- Area 3: Feature 10 (/sessions SWR Cache) ---

func TestSessionsSWR_F10_T1_ImmediateStaleServeAndConvergenceAfterIngest(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "sess-swr-t1-1"})
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "swr-s-1",
		MachineID:   "m-swr-1",
		SessionID:   "sess-swr-initial",
		Project:     "proj-swr",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(100),
	}})
	client := s.authClient(t)
	// Warm both JSON and HTML SWR cache.
	ssWaitSessions(t, client, s.BaseURL+"/api/v1/sessions", func(r []ssSessionRow) bool {
		return len(r) == 1
	})
	_ = ssGetHTML(t, client, s.BaseURL+"/sessions")

	// Ingest second session; immediate read must return 200 non-blocking, then converge after 2s window.
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "swr-s-2",
		MachineID:   "m-swr-1",
		SessionID:   "sess-swr-second",
		Project:     "proj-swr",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(200),
	}})
	code, _, _ := ssDo(t, client, http.MethodGet, s.BaseURL+"/api/v1/sessions", nil, nil)
	if code != http.StatusOK {
		t.Fatalf("immediate post-ingest GET /api/v1/sessions status = %d, want 200", code)
	}
	ssWaitSessions(t, client, s.BaseURL+"/api/v1/sessions", func(r []ssSessionRow) bool {
		return len(r) == 2
	})
	ssWaitHTML(t, client, s.BaseURL+"/sessions", func(body string) bool {
		return strings.Contains(body, "sess-swr-second")
	})
}

func TestSessionsSWR_F10_T1_BurstIngestsCoalesced(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "sess-swr-t1-2"})
	client := s.authClient(t)
	// Warm empty cache.
	ssWaitSessions(t, client, s.BaseURL+"/api/v1/sessions", func(r []ssSessionRow) bool {
		return len(r) == 0
	})

	for i := 0; i < 5; i++ {
		s.ingestEvents(t, []ingestEvent{{
			EventID:     fmt.Sprintf("swr-burst-%d", i),
			MachineID:   "m-burst",
			SessionID:   fmt.Sprintf("sess-burst-%d", i),
			Project:     "proj-burst",
			Provider:    "openai",
			Model:       "gpt-5.4",
			Tool:        "codex",
			TotalTokens: i64(50),
		}})
		code, _, _ := ssDo(t, client, http.MethodGet, s.BaseURL+"/api/v1/sessions", nil, nil)
		if code != http.StatusOK {
			t.Fatalf("burst read %d status = %d, want 200", i, code)
		}
	}

	ssWaitSessions(t, client, s.BaseURL+"/api/v1/sessions", func(r []ssSessionRow) bool {
		return len(r) == 5
	})
}

func TestSessionsSWR_F10_T1_AliasUpdateReflectsImmediatelyWithoutWaitingTTL(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "sess-swr-t1-3"})
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "swr-alias-1",
		MachineID:   "m-raw-alias-swr",
		SessionID:   "sess-alias-swr-1",
		Project:     "proj-alias",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(300),
	}})
	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 300)
	// Warm /sessions cache.
	_ = ssGetHTML(t, client, s.BaseURL+"/sessions")

	form := url.Values{}
	form.Set("model_identity", "gpt-5.4")
	form.Set("model_alias", "Immediate Model Alias")
	form.Set("machine_identity", "m-raw-alias-swr")
	form.Set("machine_alias", "Immediate Box Alias")
	code, _, _ := ssPostAliases(t, s, client, form, true)
	if code != http.StatusOK {
		t.Fatalf("POST /settings/aliases status = %d", code)
	}

	// Immediate GET /sessions without waiting 2s/60s must reflect new aliases.
	immediateHTML := ssGetHTML(t, client, s.BaseURL+"/sessions")
	if !strings.Contains(immediateHTML, "Immediate Model Alias") || !strings.Contains(immediateHTML, "Immediate Box Alias") {
		t.Fatalf("/sessions did not reflect updated aliases immediately")
	}
}

func TestSessionsSWR_F10_T1_SortAndFilterKeysCachedIndependently(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "sess-swr-t1-4"})
	now := time.Now().UTC()
	s.ingestEvents(t, []ingestEvent{
		{
			EventID:     "swr-key-1",
			Timestamp:   now.Add(-20 * time.Minute).Format(time.RFC3339Nano),
			MachineID:   "m-alpha",
			SessionID:   "sess-swr-alpha",
			Project:     "proj-a",
			Provider:    "openai",
			Model:       "gpt-5.4",
			Tool:        "codex",
			TotalTokens: i64(100),
		},
		{
			EventID:     "swr-key-2",
			Timestamp:   now.Add(-10 * time.Minute).Format(time.RFC3339Nano),
			MachineID:   "m-beta",
			SessionID:   "sess-swr-beta",
			Project:     "proj-b",
			Provider:    "anthropic",
			Model:       "claude-sonnet-4",
			Tool:        "claude-code",
			TotalTokens: i64(900),
		},
	})
	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 1000)

	htmlAsc := ssGetHTML(t, client, s.BaseURL+"/sessions?sort=tokens&dir=asc")
	bodyAsc := ssTableBodyRe.FindString(htmlAsc)
	if strings.Index(bodyAsc, "sess-swr-alpha") > strings.Index(bodyAsc, "sess-swr-beta") {
		t.Fatalf("sort=tokens&dir=asc wrong order: %s", bodyAsc)
	}

	htmlDesc := ssGetHTML(t, client, s.BaseURL+"/sessions?sort=tokens&dir=desc")
	bodyDesc := ssTableBodyRe.FindString(htmlDesc)
	if strings.Index(bodyDesc, "sess-swr-beta") > strings.Index(bodyDesc, "sess-swr-alpha") {
		t.Fatalf("sort=tokens&dir=desc wrong order: %s", bodyDesc)
	}

	htmlMachine := ssGetHTML(t, client, s.BaseURL+"/sessions?machine=m-alpha")
	bodyMach := ssTableBodyRe.FindString(htmlMachine)
	if !strings.Contains(bodyMach, "sess-swr-alpha") || strings.Contains(bodyMach, "sess-swr-beta") {
		t.Fatalf("machine=m-alpha contaminated: %s", bodyMach)
	}

	htmlTool := ssGetHTML(t, client, s.BaseURL+"/sessions?tool=claude-code")
	bodyTool := ssTableBodyRe.FindString(htmlTool)
	if !strings.Contains(bodyTool, "sess-swr-beta") || strings.Contains(bodyTool, "sess-swr-alpha") {
		t.Fatalf("tool=claude-code contaminated: %s", bodyTool)
	}
}

func TestSessionsSWR_F10_T1_ConcurrentReadersUnderIngestLoad(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "sess-swr-t1-5"})
	client := s.authClient(t)

	var wg sync.WaitGroup
	errs := make(chan error, 64)

	for w := 0; w < 5; w++ {
		wg.Add(1)
		go func(worker int) {
			defer wg.Done()
			for i := 0; i < 4; i++ {
				s.ingestEvents(t, []ingestEvent{{
					EventID:     fmt.Sprintf("swr-conc-%d-%d", worker, i),
					MachineID:   "m-conc",
					SessionID:   fmt.Sprintf("sess-conc-%d-%d", worker, i),
					Project:     "proj-conc",
					Provider:    "openai",
					Model:       "gpt-5.4",
					Tool:        "codex",
					TotalTokens: i64(25),
				}})
			}
		}(w)
	}

	for r := 0; r < 10; r++ {
		wg.Add(1)
		go func(reader int) {
			defer wg.Done()
			path := "/api/v1/sessions"
			if reader%2 == 1 {
				path = "/sessions"
			}
			for i := 0; i < 5; i++ {
				resp, err := client.Get(s.BaseURL + path)
				if err != nil {
					errs <- err
					return
				}
				_, readErr := io.Copy(io.Discard, resp.Body)
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
					errs <- fmt.Errorf("%s status = %d", path, resp.StatusCode)
					return
				}
				time.Sleep(30 * time.Millisecond)
			}
		}(r)
	}

	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatalf("concurrent SWR reader failed: %v", err)
	}

	ssWaitSessions(t, client, s.BaseURL+"/api/v1/sessions", func(r []ssSessionRow) bool {
		return len(r) == 20
	})
}

func TestSessionsSWR_F10_T2_CacheKeyIsolationForProjectSearchPageAndFromTo(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "sess-swr-t2-1"})
	now := time.Now().UTC()
	dRecent := now.Add(-2 * time.Hour)
	dOlder := now.AddDate(0, 0, -10)

	events := []ingestEvent{
		{
			EventID:     "swr-iso-1",
			Timestamp:   dRecent.Format(time.RFC3339Nano),
			MachineID:   "m-iso",
			SessionID:   "sess-iso-alpha-recent",
			Project:     "proj-iso-a",
			Provider:    "openai",
			Model:       "gpt-5.4",
			Tool:        "codex",
			TotalTokens: i64(100),
		},
		{
			EventID:     "swr-iso-2",
			Timestamp:   dOlder.Format(time.RFC3339Nano),
			MachineID:   "m-iso",
			SessionID:   "sess-iso-beta-older",
			Project:     "proj-iso-b",
			Provider:    "openai",
			Model:       "gpt-5.4",
			Tool:        "codex",
			TotalTokens: i64(200),
		},
	}
	for i := 0; i < 105; i++ {
		events = append(events, ingestEvent{
			EventID:     fmt.Sprintf("swr-iso-pg-%03d", i),
			Timestamp:   dRecent.Add(-time.Duration(i+1) * time.Minute).Format(time.RFC3339Nano),
			MachineID:   "m-iso",
			SessionID:   fmt.Sprintf("sess-iso-pg-%03d", i),
			Project:     "proj-iso-paged",
			Provider:    "openai",
			Model:       "gpt-5.4",
			Tool:        "codex",
			TotalTokens: i64(10),
		})
	}
	s.ingestEvents(t, events)
	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 1350)

	// Project isolation.
	rProjA := ssWaitSessions(t, client, s.BaseURL+"/api/v1/sessions?project=proj-iso-a", func(r []ssSessionRow) bool { return len(r) == 1 })
	rProjB := ssWaitSessions(t, client, s.BaseURL+"/api/v1/sessions?project=proj-iso-b", func(r []ssSessionRow) bool { return len(r) == 1 })
	if rProjA[0].SessionID != "sess-iso-alpha-recent" || rProjB[0].SessionID != "sess-iso-beta-older" {
		t.Fatalf("project cache key collision: a=%+v b=%+v", rProjA, rProjB)
	}

	// q search isolation.
	rQA := ssWaitSessions(t, client, s.BaseURL+"/api/v1/sessions?q=alpha", func(r []ssSessionRow) bool { return len(r) == 1 })
	rQB := ssWaitSessions(t, client, s.BaseURL+"/api/v1/sessions?q=beta", func(r []ssSessionRow) bool { return len(r) == 1 })
	if rQA[0].SessionID != "sess-iso-alpha-recent" || rQB[0].SessionID != "sess-iso-beta-older" {
		t.Fatalf("q cache key collision: a=%+v b=%+v", rQA, rQB)
	}

	// from/to isolation.
	recentDay := dRecent.Format("2006-01-02")
	olderDay := dOlder.Format("2006-01-02")
	rFromRecent := ssWaitSessions(t, client, fmt.Sprintf("%s/api/v1/sessions?from=%s&to=%s&project=proj-iso-a", s.BaseURL, recentDay, recentDay), func(r []ssSessionRow) bool {
		return len(r) == 1
	})
	rFromOlder := ssWaitSessions(t, client, fmt.Sprintf("%s/api/v1/sessions?from=%s&to=%s&project=proj-iso-a", s.BaseURL, olderDay, olderDay), func(r []ssSessionRow) bool {
		return len(r) == 0
	})
	if len(rFromRecent) != 1 || len(rFromOlder) != 0 {
		t.Fatalf("from/to cache key collision: recent=%d older=%d", len(rFromRecent), len(rFromOlder))
	}

	// Page isolation on HTML.
	p1HTML := ssGetHTML(t, client, s.BaseURL+"/sessions?project=proj-iso-paged&page=1")
	p2HTML := ssGetHTML(t, client, s.BaseURL+"/sessions?project=proj-iso-paged&page=2")
	if ssTableBodyRe.FindString(p1HTML) == ssTableBodyRe.FindString(p2HTML) {
		t.Fatalf("page=1 and page=2 returned identical session rows (cache key missing page)")
	}
}

func TestSessionsSWR_F10_T2_CacheBoundedEvictionBeyond64DistinctFilters(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "sess-swr-t2-2"})
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "swr-evict-1",
		MachineID:   "m-evict-00",
		SessionID:   "sess-evict-00",
		Project:     "proj-evict",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(100),
	}})
	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 100)

	// Query 72 distinct machine filters (> 64 LRU bound).
	for i := 0; i < 72; i++ {
		code, _, _ := ssDo(t, client, http.MethodGet, fmt.Sprintf("%s/api/v1/sessions?machine=m-evict-%02d", s.BaseURL, i), nil, nil)
		if code != http.StatusOK {
			t.Fatalf("filter %d status = %d, want 200", i, code)
		}
	}

	// Re-query the evicted first filter and verify accuracy.
	rows := ssWaitSessions(t, client, s.BaseURL+"/api/v1/sessions?machine=m-evict-00", func(r []ssSessionRow) bool {
		return len(r) == 1
	})
	if rows[0].SessionID != "sess-evict-00" {
		t.Fatalf("unexpected row after LRU eviction: %+v", rows[0])
	}
}

func TestSessionsSWR_F10_T2_EmptyDatabaseWarmCacheThenFirstIngestConverges(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "sess-swr-t2-3"})
	client := s.authClient(t)

	emptyRows := ssWaitSessions(t, client, s.BaseURL+"/api/v1/sessions", func(r []ssSessionRow) bool {
		return len(r) == 0
	})
	if len(emptyRows) != 0 {
		t.Fatalf("expected empty sessions on fresh DB, got %d", len(emptyRows))
	}
	emptyHTML := ssGetHTML(t, client, s.BaseURL+"/sessions")
	if !strings.Contains(emptyHTML, "No sessions match") {
		t.Fatalf("empty /sessions missing empty-state copy")
	}

	s.ingestEvents(t, []ingestEvent{{
		EventID:     "swr-first-1",
		MachineID:   "m-first",
		SessionID:   "sess-first-arrival",
		Project:     "proj-first",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(77),
	}})

	ssWaitSessions(t, client, s.BaseURL+"/api/v1/sessions", func(r []ssSessionRow) bool {
		return len(r) == 1 && r[0].SessionID == "sess-first-arrival"
	})
	ssWaitHTML(t, client, s.BaseURL+"/sessions", func(body string) bool {
		return strings.Contains(body, "sess-first-arrival")
	})
}

func TestSessionsSWR_F10_T2_ExportReflectsNewIngestAndIsolatesFilterKeys(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "sess-swr-t2-4"})
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "swr-exp-1",
		MachineID:   "m-exp-a",
		SessionID:   "sess-exp-a",
		Project:     "proj-a",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(100),
	}})
	client := s.authClient(t)
	ssWaitSessions(t, client, s.BaseURL+"/api/v1/sessions", func(r []ssSessionRow) bool { return len(r) == 1 })

	// Warm export for m-exp-a.
	code, _, csvA := ssDo(t, client, http.MethodGet, s.BaseURL+"/api/v1/sessions/export?format=csv&machine=m-exp-a", nil, nil)
	if code != http.StatusOK || !strings.Contains(string(csvA), "sess-exp-a") {
		t.Fatalf("initial CSV export failed: status=%d body=%s", code, string(csvA))
	}

	// Ingest m-exp-b and verify export converges and does not cross-contaminate.
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "swr-exp-2",
		MachineID:   "m-exp-b",
		SessionID:   "sess-exp-b",
		Project:     "proj-b",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(200),
	}})
	deadline := time.Now().Add(5 * time.Second)
	for {
		c, _, csvB := ssDo(t, client, http.MethodGet, s.BaseURL+"/api/v1/sessions/export?format=csv&machine=m-exp-b", nil, nil)
		if c == http.StatusOK && strings.Contains(string(csvB), "sess-exp-b") && !strings.Contains(string(csvB), "sess-exp-a") {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for m-exp-b CSV export; last=%s", string(csvB))
		}
		time.Sleep(100 * time.Millisecond)
	}
}

func TestSessionsSWR_F10_T2_DuplicateIngestDoesNotCorruptSessionAggregation(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "sess-swr-t2-5"})
	ev := ingestEvent{
		EventID:     "swr-dup-1",
		MachineID:   "m-dup",
		SessionID:   "sess-dup-1",
		Project:     "proj-dup",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(420),
	}
	res1 := s.ingestEvents(t, []ingestEvent{ev})
	if res1.Accepted != 1 {
		t.Fatalf("first ingest accepted = %d, want 1", res1.Accepted)
	}
	client := s.authClient(t)
	ssWaitSessions(t, client, s.BaseURL+"/api/v1/sessions", func(r []ssSessionRow) bool {
		return len(r) == 1 && r[0].TotalTokens != nil && *r[0].TotalTokens == 420
	})

	// Re-ingest exact duplicate event_id.
	res2 := s.ingestEvents(t, []ingestEvent{ev})
	if res2.Duplicates != 1 || res2.Accepted != 0 {
		t.Fatalf("duplicate ingest result = %+v, want duplicates=1 accepted=0", res2)
	}

	//Wait past SWR window and verify total remains 420, events remains 1.
	time.Sleep(2200 * time.Millisecond)
	rows := ssWaitSessions(t, client, s.BaseURL+"/api/v1/sessions", func(r []ssSessionRow) bool {
		return len(r) == 1 && r[0].TotalTokens != nil && *r[0].TotalTokens == 420 && r[0].Events == 1
	})
	if rows[0].Events != 1 || *rows[0].TotalTokens != 420 {
		t.Fatalf("duplicate corrupted session row: %+v", rows[0])
	}
}

// --- Area 4: Feature 17 (/settings Usage-Sorted Aliases, Filter, Reset & Cross-Links) ---

func TestSettings_F17_T1_AliasesSortedByUsageDescendingWithCounts(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "set-f17-t1-1"})
	s.ingestEvents(t, []ingestEvent{
		// Deliberately out of alphabetical order vs usage descending.
		{EventID: "set-u-1", MachineID: "m-alpha-low", SessionID: "s1", Provider: "openai", Model: "a-model-low", Tool: "codex", TotalTokens: i64(100)},
		{EventID: "set-u-2", MachineID: "m-zulu-high", SessionID: "s2", Provider: "openai", Model: "z-model-high", Tool: "codex", TotalTokens: i64(5000)},
		{EventID: "set-u-3", MachineID: "m-mike-mid", SessionID: "s3", Provider: "openai", Model: "m-model-mid", Tool: "codex", TotalTokens: i64(500)},
	})
	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 5600)

	settingsHTML := ssGetHTML(t, client, s.BaseURL+"/settings")
	modelsSec := ssExtractSection(settingsHTML, `id="settings-form-title"`, `id="machines-title"`)
	machinesSec := ssExtractSection(settingsHTML, `id="machines-title"`, `</form>`)

	zIdx := strings.Index(modelsSec, "z-model-high")
	mIdx := strings.Index(modelsSec, "m-model-mid")
	aIdx := strings.Index(modelsSec, "a-model-low")
	if zIdx < 0 || mIdx < 0 || aIdx < 0 || !(zIdx < mIdx && mIdx < aIdx) {
		t.Fatalf("/settings models not sorted by usage DESC (5000 -> 500 -> 100): z=%d m=%d a=%d\nsec=%s", zIdx, mIdx, aIdx, modelsSec)
	}
	if !strings.Contains(modelsSec, "5,000") && !strings.Contains(modelsSec, "5K") {
		t.Fatalf("/settings model rows missing usage token count")
	}

	mzIdx := strings.Index(machinesSec, "m-zulu-high")
	mmIdx := strings.Index(machinesSec, "m-mike-mid")
	maIdx := strings.Index(machinesSec, "m-alpha-low")
	if mzIdx < 0 || mmIdx < 0 || maIdx < 0 || !(mzIdx < mmIdx && mmIdx < maIdx) {
		t.Fatalf("/settings machines not sorted by usage DESC: z=%d m=%d a=%d\nsec=%s", mzIdx, mmIdx, maIdx, machinesSec)
	}
}

func TestSettings_F17_T1_FilterInputAndPerRowResetMarkupPresent(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "set-f17-t1-2"})
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "set-flt-1",
		MachineID:   "m-flt-1",
		SessionID:   "s-flt-1",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(250),
	}})
	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 250)

	settingsHTML := ssGetHTML(t, client, s.BaseURL+"/settings")
	var aliasSec string
	for _, loc := range ssFormBlockRe.FindAllStringSubmatchIndex(settingsHTML, -1) {
		attrs := ssParseTagAttrs(settingsHTML[loc[2]:loc[3]])
		if attrs["action"] == "/settings/aliases" {
			pre := settingsHTML[:loc[0]]
			for _, sep := range []string{"</header>", "</form>"} {
				if idx := strings.LastIndex(strings.ToLower(pre), sep); idx >= 0 {
					pre = pre[idx+len(sep):]
				}
			}
			aliasSec = pre + settingsHTML[loc[0]:loc[1]]
			break
		}
	}
	if aliasSec == "" {
		t.Fatalf("/settings missing /settings/aliases form")
	}
	hasSearchFilter := false
	for _, im := range ssInputTagRe.FindAllStringSubmatch(aliasSec, -1) {
		ia := ssParseTagAttrs(im[1])
		if strings.EqualFold(ia["type"], "hidden") {
			continue
		}
		if strings.EqualFold(ia["type"], "search") {
			hasSearchFilter = true
			break
		}
		for _, attrKey := range []string{"name", "id", "placeholder", "aria-label"} {
			v := strings.ToLower(ia[attrKey])
			if strings.Contains(v, "filter") || strings.Contains(v, "search") {
				hasSearchFilter = true
				break
			}
		}
		if hasSearchFilter {
			break
		}
	}
	if !hasSearchFilter {
		t.Fatalf("/settings alias section missing <input> filter/search control")
	}
	hasResetControl := regexp.MustCompile(`(?is)<button[^>]*>[^<]*reset[^<]*</button>|data-reset|reset-alias`).MatchString(aliasSec)
	if !hasResetControl {
		t.Fatalf("/settings alias section missing per-row alias reset control")
	}
}

func TestSettings_F17_T1_ModelCrossLinksToTokedexAndAnalytics(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{
		IngestToken:   "set-f17-t1-3",
		EnableTokedex: true,
	})
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "set-link-1",
		MachineID:   "m-link-1",
		SessionID:   "s-link-1",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(300),
	}})
	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 300)

	settingsHTML := ssGetHTML(t, client, s.BaseURL+"/settings")
	modelsSec := ssExtractSection(settingsHTML, `id="settings-form-title"`, `id="machines-title"`)
	if !strings.Contains(modelsSec, "/analytics") || !strings.Contains(modelsSec, "model=gpt-5.4") {
		t.Fatalf("/settings model rows missing /analytics model cross-link: %s", modelsSec)
	}
	if !strings.Contains(modelsSec, "/tokedex") {
		t.Fatalf("/settings model rows missing /tokedex cross-link when EnableTokedex=true: %s", modelsSec)
	}

	tokedexHTML := ssGetHTML(t, client, s.BaseURL+"/tokedex")
	if !strings.Contains(tokedexHTML, `href="/settings`) {
		t.Fatalf("/tokedex missing cross-link back to /settings")
	}
}

func TestSettings_F17_T1_SaveAndClearAliasRoundTripAcrossRestart(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "set-f17-t1-4"})
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "set-rt-1",
		MachineID:   "m-rt-host",
		SessionID:   "s-rt-1",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(400),
	}})
	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 400)

	form := url.Values{}
	form.Set("model_identity", "gpt-5.4")
	form.Set("model_alias", "Studio GPT")
	form.Set("machine_identity", "m-rt-host")
	form.Set("machine_alias", "Desk Rig")
	code, hdrs, _ := ssPostAliases(t, s, client, form, false)
	if code != http.StatusSeeOther || hdrs.Get("Location") != "/settings?saved=1" {
		t.Fatalf("POST /settings/aliases status=%d loc=%q, want 303 /settings?saved=1", code, hdrs.Get("Location"))
	}

	savedHTML := ssGetHTML(t, client, s.BaseURL+"/settings?saved=1")
	if !strings.Contains(savedHTML, "Aliases saved.") || !strings.Contains(savedHTML, "Studio GPT") || !strings.Contains(savedHTML, "Desk Rig") {
		t.Fatalf("/settings?saved=1 missing saved notice or alias values")
	}

	dbPath := s.DBPath
	s.Stop()

	s2 := startTestServer(t, serverOpts{
		IngestToken: "set-f17-t1-4",
		DBPath:      dbPath,
	})
	client2 := s2.authClient(t)
	persistedHTML := ssGetHTML(t, client2, s2.BaseURL+"/settings")
	if !strings.Contains(persistedHTML, "Studio GPT") || !strings.Contains(persistedHTML, "Desk Rig") {
		t.Fatalf("aliases lost across server restart")
	}

	// Clear aliases back to default.
	clearForm := url.Values{}
	clearForm.Set("model_identity", "gpt-5.4")
	clearForm.Set("model_alias", "")
	clearForm.Set("machine_identity", "m-rt-host")
	clearForm.Set("machine_alias", "")
	codeClear, _, _ := ssPostAliases(t, s2, client2, clearForm, true)
	if codeClear != http.StatusOK {
		t.Fatalf("clear aliases status = %d", codeClear)
	}
	clearedHTML := ssGetHTML(t, client2, s2.BaseURL+"/settings")
	if strings.Contains(clearedHTML, `value="Studio GPT"`) || strings.Contains(clearedHTML, `value="Desk Rig"`) {
		t.Fatalf("cleared aliases still present in /settings inputs")
	}
}

func TestSettings_F17_T1_AliasedOnlyZeroUsageEntryStillListedAndEditable(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "set-f17-t1-5"})
	client := s.authClient(t)

	// Set an alias for a model and machine with zero recorded usage events.
	form := url.Values{}
	form.Set("model_identity", "zero-usage-model")
	form.Set("model_alias", "Zero Model")
	form.Set("machine_identity", "zero-usage-machine")
	form.Set("machine_alias", "Zero Box")
	code, _, _ := ssPostAliases(t, s, client, form, true)
	if code != http.StatusOK {
		t.Fatalf("POST /settings/aliases status = %d", code)
	}

	settingsHTML := ssGetHTML(t, client, s.BaseURL+"/settings")
	if !strings.Contains(settingsHTML, "zero-usage-model") || !strings.Contains(settingsHTML, "Zero Model") {
		t.Fatalf("/settings did not list zero-usage aliased model")
	}
	if !strings.Contains(settingsHTML, "zero-usage-machine") || !strings.Contains(settingsHTML, "Zero Box") {
		t.Fatalf("/settings did not list zero-usage aliased machine")
	}
}

func TestSettings_F17_T2_ValidationRejectsTooLongAliasMissingIdentityAndMismatchedArrays(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "set-f17-t2-1"})
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "set-val-1",
		MachineID:   "m-val",
		SessionID:   "s-val",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(100),
	}})
	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 100)

	// Save valid baseline alias first.
	good := url.Values{}
	good.Set("model_identity", "gpt-5.4")
	good.Set("model_alias", "Valid Baseline")
	codeGood, _, _ := ssPostAliases(t, s, client, good, true)
	if codeGood != http.StatusOK {
		t.Fatalf("baseline alias POST failed: %d", codeGood)
	}

	csrf := extractCSRFToken(t, []byte(ssGetHTML(t, client, s.BaseURL+"/settings")))

	// 49 runes > 48 max.
	tooLong := url.Values{}
	tooLong.Set("csrf_token", csrf)
	tooLong.Set("model_identity", "gpt-5.4")
	tooLong.Set("model_alias", strings.Repeat("x", 49))
	c1, _, b1 := ssPostForm(t, client, s.BaseURL+"/settings/aliases", s.BaseURL, s.BaseURL+"/settings", tooLong)
	if c1 != http.StatusBadRequest || !strings.Contains(string(b1), `role="alert"`) {
		t.Fatalf(">48 rune alias status=%d body=%s, want 400 role=alert", c1, string(b1))
	}

	// Empty identity.
	emptyID := url.Values{}
	emptyID.Set("csrf_token", csrf)
	emptyID.Set("model_identity", "   ")
	emptyID.Set("model_alias", "Orphan Alias")
	c2, _, b2 := ssPostForm(t, client, s.BaseURL+"/settings/aliases", s.BaseURL, s.BaseURL+"/settings", emptyID)
	if c2 != http.StatusBadRequest || !strings.Contains(string(b2), `role="alert"`) {
		t.Fatalf("empty identity status=%d body=%s, want 400 role=alert", c2, string(b2))
	}

	// Mismatched array lengths.
	mismatch := url.Values{}
	mismatch.Set("csrf_token", csrf)
	mismatch.Add("model_identity", "gpt-5.4")
	mismatch.Add("model_identity", "claude-sonnet-4")
	mismatch.Add("model_alias", "Only One")
	c3, _, b3 := ssPostForm(t, client, s.BaseURL+"/settings/aliases", s.BaseURL, s.BaseURL+"/settings", mismatch)
	if c3 != http.StatusBadRequest || !strings.Contains(string(b3), `role="alert"`) {
		t.Fatalf("mismatched arrays status=%d body=%s, want 400 role=alert", c3, string(b3))
	}

	// Verify baseline remained intact.
	afterHTML := ssGetHTML(t, client, s.BaseURL+"/settings")
	if !strings.Contains(afterHTML, "Valid Baseline") {
		t.Fatalf("rejected invalid updates corrupted baseline alias")
	}
}

func TestSettings_F17_T2_CSRFAndOriginRefererEnforcement(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{
		IngestToken: "set-f17-t2-2",
		Username:    "admin",
		Password:    "set-pass-123",
	})
	client := s.authClient(t)
	csrf := extractCSRFToken(t, []byte(ssGetHTML(t, client, s.BaseURL+"/settings")))

	// Unauthenticated POST.
	unauthForm := url.Values{}
	unauthForm.Set("csrf_token", csrf)
	unauthForm.Set("model_identity", "gpt-5.4")
	unauthForm.Set("model_alias", "Hacker")
	cUnauth, _, _ := ssPostForm(t, s.Client, s.BaseURL+"/settings/aliases", s.BaseURL, s.BaseURL+"/settings", unauthForm)
	if cUnauth != http.StatusUnauthorized && cUnauth != http.StatusSeeOther {
		t.Fatalf("unauth POST /settings/aliases status = %d, want 401 or 303", cUnauth)
	}

	// Missing CSRF with valid session + Origin.
	noCSRF := url.Values{}
	noCSRF.Set("model_identity", "gpt-5.4")
	noCSRF.Set("model_alias", "No CSRF")
	cNoCSRF, _, bNoCSRF := ssPostForm(t, client, s.BaseURL+"/settings/aliases", s.BaseURL, s.BaseURL+"/settings", noCSRF)
	if cNoCSRF != http.StatusForbidden || !strings.Contains(string(bNoCSRF), `role="alert"`) {
		t.Fatalf("missing CSRF status=%d, want 403", cNoCSRF)
	}

	// Tampered CSRF.
	badCSRF := url.Values{}
	badCSRF.Set("csrf_token", csrf+"tampered")
	badCSRF.Set("model_identity", "gpt-5.4")
	badCSRF.Set("model_alias", "Bad CSRF")
	cBadCSRF, _, _ := ssPostForm(t, client, s.BaseURL+"/settings/aliases", s.BaseURL, s.BaseURL+"/settings", badCSRF)
	if cBadCSRF != http.StatusForbidden {
		t.Fatalf("tampered CSRF status=%d, want 403", cBadCSRF)
	}

	// Evil Origin with valid CSRF.
	goodVals := url.Values{}
	goodVals.Set("csrf_token", csrf)
	goodVals.Set("model_identity", "gpt-5.4")
	goodVals.Set("model_alias", "Evil Origin")
	cEvilOrigin, _, _ := ssPostForm(t, client, s.BaseURL+"/settings/aliases", "https://evil.example", s.BaseURL+"/settings", goodVals)
	if cEvilOrigin != http.StatusForbidden {
		t.Fatalf("evil Origin status=%d, want 403", cEvilOrigin)
	}

	// Evil Referer with valid CSRF (and empty Origin).
	cEvilRef, _, _ := ssPostForm(t, client, s.BaseURL+"/settings/aliases", "", "https://evil.example/attack", goodVals)
	if cEvilRef != http.StatusForbidden {
		t.Fatalf("evil Referer status=%d, want 403", cEvilRef)
	}
}

func TestSettings_F17_T2_XSSInAliasIdentityAndValueEscaped(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "set-f17-t2-3"})
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "set-xss-1",
		MachineID:   "m-xss",
		SessionID:   "sess-xss",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(100),
	}})
	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 100)

	xssPayload := `<img src=x onerror=alert(1)>`
	form := url.Values{}
	form.Set("model_identity", "gpt-5.4")
	form.Set("model_alias", xssPayload)
	form.Set("machine_identity", "m-xss")
	form.Set("machine_alias", `<script>alert("m")</script>`)
	code, _, _ := ssPostAliases(t, s, client, form, true)
	if code != http.StatusOK {
		t.Fatalf("POST /settings/aliases XSS test status = %d", code)
	}

	for _, p := range []string{"/settings", "/", "/sessions", "/analytics"} {
		body := ssGetHTML(t, client, s.BaseURL+p)
		if strings.Contains(body, xssPayload) || strings.Contains(body, `<script>alert("m")</script>`) {
			t.Fatalf("page %s rendered unescaped XSS alias payload", p)
		}
		if !strings.Contains(body, "&lt;img src=x onerror=alert(1)&gt;") && p == "/settings" {
			t.Fatalf("/settings did not preserve escaped alias text")
		}
	}
}

func TestSettings_F17_T2_TokedexDisabledSuppressesTokedexCrossLinkOnSettings(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{
		IngestToken:   "set-f17-t2-4",
		EnableTokedex: false,
	})
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "set-notd-1",
		MachineID:   "m-notd",
		SessionID:   "s-notd",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(150),
	}})
	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 150)

	settingsHTML := ssGetHTML(t, client, s.BaseURL+"/settings")
	if strings.Contains(settingsHTML, `href="/tokedex`) {
		t.Fatalf("/settings rendered /tokedex cross-link while TOKEMON_ENABLE_TOKEDEX=false")
	}
}

func TestSettings_F17_T2_AuthModeAndTokenSourceDisplayedWithoutLeakingSecrets(t *testing.T) {
	requireE2E(t)
	ingestTok := "super-secret-ingest-token-xyz"
	dashTok := "super-secret-dashboard-token-abc"
	pass := "super-secret-pass-999"
	s := startTestServer(t, serverOpts{
		IngestToken:    ingestTok,
		DashboardToken: dashTok,
		Username:       "admin",
		Password:       pass,
	})
	client := s.authClient(t)
	settingsHTML := ssGetHTML(t, client, s.BaseURL+"/settings")
	if !strings.Contains(settingsHTML, "Password account") {
		t.Fatalf("/settings missing Auth mode summary")
	}
	for _, secret := range []string{ingestTok, dashTok, pass, "$2a$", "$2b$"} {
		if strings.Contains(settingsHTML, secret) {
			t.Fatalf("/settings leaked secret %q in HTML", secret)
		}
	}
}

// --- Area 5: Feature 21 (/settings Quotas CRUD, YAML Merge, Backup Age & Alerts) ---

func TestSettings_F21_T1_QuotaCreateEditDeleteLifecycle(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "set-f21-t1-1"})
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "q-crud-1",
		MachineID:   "m-q-crud",
		SessionID:   "s-q-crud",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(600),
	}})
	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 600)

	// 1. Create SQLite quota via /settings.
	createForm := url.Values{}
	createForm.Set("label", "OpenAI Monthly Cap")
	createForm.Set("provider", "openai")
	createForm.Set("reset_day", "1")
	createForm.Set("tokens_per_month", "5000")
	cCreate, _, bCreate := ssSubmitQuotaCreate(t, s, client, createForm, true)
	if cCreate != http.StatusOK {
		t.Fatalf("create quota status = %d: %s", cCreate, string(bCreate))
	}

	settingsAfterCreate := ssWaitHTML(t, client, s.BaseURL+"/settings", func(body string) bool {
		return strings.Contains(body, "OpenAI Monthly Cap")
	})
	var quotas []ssQuotaRow
	getJSONWithClient(t, client, s.BaseURL+"/api/v1/quotas", &quotas)
	if len(quotas) != 1 || quotas[0].Label != "OpenAI Monthly Cap" {
		t.Fatalf("/api/v1/quotas after create = %+v", quotas)
	}
	ssWaitHTML(t, client, s.BaseURL+"/", func(body string) bool {
		return strings.Contains(body, "OpenAI Monthly Cap")
	})

	// 2. Edit SQLite quota via discovered edit form (or update action).
	_, editForms, _ := ssDiscoverQuotaForms(settingsAfterCreate)
	if len(editForms) == 0 {
		t.Fatalf("/settings missing quota edit/update form after creating SQLite quota")
	}
	editTargetForm := editForms[0]
	editForm := url.Values{}
	editForm.Set("csrf_token", extractCSRFToken(t, []byte(settingsAfterCreate)))
	ssCopyHiddenInputs(editTargetForm.Inner, editForm)
	ssCopySubmitButton(editTargetForm.Inner, false, editForm)
	editForm.Set("label", "OpenAI Monthly Updated")
	editForm.Set("provider", "openai")
	editForm.Set("reset_day", "1")
	editForm.Set("tokens_per_month", "9000")
	editURL := editTargetForm.Action
	if !strings.HasPrefix(editURL, "http") {
		editURL = s.BaseURL + editURL
	}
	cEdit, _, bEdit := ssPostForm(t, client, editURL, s.BaseURL, s.BaseURL+"/settings", editForm)
	if cEdit != http.StatusOK {
		t.Fatalf("edit quota POST %s status = %d: %s", editTargetForm.Action, cEdit, string(bEdit))
	}

	settingsAfterEdit := ssWaitHTML(t, client, s.BaseURL+"/settings", func(body string) bool {
		return strings.Contains(body, "OpenAI Monthly Updated")
	})
	quotas = nil
	getJSONWithClient(t, client, s.BaseURL+"/api/v1/quotas", &quotas)
	if len(quotas) != 1 || quotas[0].Label != "OpenAI Monthly Updated" {
		t.Fatalf("/api/v1/quotas after edit = %+v", quotas)
	}

	// 3. Delete SQLite quota via discovered delete form.
	_, _, deleteForms := ssDiscoverQuotaForms(settingsAfterEdit)
	if len(deleteForms) == 0 {
		t.Fatalf("/settings missing quota delete form")
	}
	delTargetForm := deleteForms[0]
	delForm := url.Values{}
	delForm.Set("csrf_token", extractCSRFToken(t, []byte(settingsAfterEdit)))
	ssCopyHiddenInputs(delTargetForm.Inner, delForm)
	ssCopySubmitButton(delTargetForm.Inner, true, delForm)
	delURL := delTargetForm.Action
	if !strings.HasPrefix(delURL, "http") {
		delURL = s.BaseURL + delURL
	}
	cDel, _, bDel := ssPostForm(t, client, delURL, s.BaseURL, s.BaseURL+"/settings", delForm)
	if cDel != http.StatusOK {
		t.Fatalf("delete quota POST %s status = %d: %s", delTargetForm.Action, cDel, string(bDel))
	}

	quotas = nil
	getJSONWithClient(t, client, s.BaseURL+"/api/v1/quotas", &quotas)
	if len(quotas) != 0 {
		t.Fatalf("/api/v1/quotas after delete = %+v, want empty", quotas)
	}
	settingsAfterDel := ssGetHTML(t, client, s.BaseURL+"/settings")
	if strings.Contains(settingsAfterDel, "OpenAI Monthly Updated") {
		t.Fatalf("deleted quota still visible on /settings")
	}
}

func TestSettings_F21_T1_SQLiteQuotasMergeWithTOKEMONQuotasYAML(t *testing.T) {
	requireE2E(t)
	quotasYAML := `schema_version: "1"
quotas:
  - label: "YAML Anthropic Cap"
    provider: anthropic
    reset_day: 1
    budget:
      tokens_per_month: 8000
`
	s := startTestServer(t, serverOpts{
		IngestToken: "set-f21-t1-2",
		QuotasYAML:  quotasYAML,
	})
	client := s.authClient(t)

	form := url.Values{}
	form.Set("label", "SQLite OpenAI Cap")
	form.Set("provider", "openai")
	form.Set("reset_day", "1")
	form.Set("tokens_per_month", "12000")
	cCreate, _, bCreate := ssSubmitQuotaCreate(t, s, client, form, true)
	if cCreate != http.StatusOK {
		t.Fatalf("create SQLite quota alongside YAML failed: %d %s", cCreate, string(bCreate))
	}

	var quotas []ssQuotaRow
	getJSONWithClient(t, client, s.BaseURL+"/api/v1/quotas", &quotas)
	if len(quotas) != 2 {
		t.Fatalf("merged /api/v1/quotas count = %d, want 2: %+v", len(quotas), quotas)
	}
	settingsHTML := ssGetHTML(t, client, s.BaseURL+"/settings")
	if !strings.Contains(settingsHTML, "YAML Anthropic Cap") || !strings.Contains(settingsHTML, "SQLite OpenAI Cap") {
		t.Fatalf("/settings missing merged YAML + SQLite quotas")
	}
	ovHTML := ssWaitHTML(t, client, s.BaseURL+"/", func(body string) bool {
		return strings.Contains(body, "YAML Anthropic Cap") && strings.Contains(body, "SQLite OpenAI Cap")
	})
	if !strings.Contains(ovHTML, "YAML Anthropic Cap") || !strings.Contains(ovHTML, "SQLite OpenAI Cap") {
		t.Fatalf("/ missing merged YAML + SQLite quotas")
	}
}

func TestSettings_F21_T1_QuotaPersistenceAcrossServerRestart(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "set-f21-t1-3"})
	s.ingestEvents(t, []ingestEvent{{
		EventID:     "q-pers-1",
		MachineID:   "m-q-pers",
		SessionID:   "s-q-pers",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(1250),
	}})
	client := s.authClient(t)
	waitOverviewLifetimeTokens(t, client, s.BaseURL, 1250)

	form := url.Values{}
	form.Set("label", "Persistent Cap")
	form.Set("provider", "openai")
	form.Set("reset_day", "1")
	form.Set("tokens_per_month", "5000")
	code, _, body := ssSubmitQuotaCreate(t, s, client, form, true)
	if code != http.StatusOK {
		t.Fatalf("create persistent quota status=%d body=%s", code, string(body))
	}

	dbPath := s.DBPath
	s.Stop()

	s2 := startTestServer(t, serverOpts{
		IngestToken: "set-f21-t1-3",
		DBPath:      dbPath,
	})
	client2 := s2.authClient(t)
	var quotas []ssQuotaRow
	getJSONWithClient(t, client2, s2.BaseURL+"/api/v1/quotas", &quotas)
	if len(quotas) != 1 || quotas[0].Label != "Persistent Cap" || quotas[0].TokensUsed != 1250 {
		t.Fatalf("restarted /api/v1/quotas = %+v, want Persistent Cap with TokensUsed=1250", quotas)
	}
	settingsHTML := ssGetHTML(t, client2, s2.BaseURL+"/settings")
	if !strings.Contains(settingsHTML, "Persistent Cap") {
		t.Fatalf("restarted /settings missing Persistent Cap")
	}
}

func TestSettings_F21_T1_LastBackupAgeDisplayedAndEmptyState(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{
		IngestToken:    "set-f21-t1-4",
		BackupInterval: "0",
	})
	client := s.authClient(t)

	emptyHTML := ssGetHTML(t, client, s.BaseURL+"/settings")
	if !strings.Contains(emptyHTML, "No backups yet") && !strings.Contains(emptyHTML, "No backups recorded yet") {
		t.Fatalf("/settings with empty BackupDir and BackupInterval=0 missing \"No backups yet\" / \"No backups recorded yet\"")
	}

	// Create a real WAL-safe backup via CLI.
	stdout, stderr, err := s.runCLI(t, nil,
		"backup", "create",
		"--database", s.DBPath,
		"--destination", s.BackupDir,
	)
	if err != nil {
		t.Fatalf("tokemon backup create failed: %v\nstdout: %s\nstderr: %s", err, stdout, stderr)
	}

	afterHTML := ssGetHTML(t, client, s.BaseURL+"/settings")
	if strings.Contains(afterHTML, "No backups yet") || strings.Contains(afterHTML, "No backups recorded yet") {
		t.Fatalf("/settings still shows empty-backup label after backup created in %s", s.BackupDir)
	}
	if !strings.Contains(afterHTML, "just now") && !strings.Contains(afterHTML, "ago") && !strings.Contains(afterHTML, "tokemon-backup-v") {
		t.Fatalf("/settings missing backup age/timestamp readout after backup creation")
	}
}

func TestSettings_F21_T1_OverageAlertWebhookAndNtfyDelivery(t *testing.T) {
	requireE2E(t)
	var mu sync.Mutex
	var webhookBodies []string
	var ntfyBodies []string

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
		if strings.HasPrefix(r.URL.Path, "/ntfy") {
			ntfyBodies = append(ntfyBodies, string(raw))
		} else {
			webhookBodies = append(webhookBodies, string(raw))
		}
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer alertSrv.Close()

	quotasYAML := `schema_version: "1"
quotas:
  - label: "AlertTightCap"
    provider: openai
    reset_day: 1
    budget:
      tokens_per_month: 1000
`
	s := startTestServer(t, serverOpts{
		IngestToken:     "set-f21-t1-5",
		QuotasYAML:      quotasYAML,
		AlertWebhookURL: alertSrv.URL + "/webhook",
		ExtraEnv: map[string]string{
			"TOKEMON_ALERT_NTFY_URL": alertSrv.URL + "/ntfy/tokemon-e2e",
		},
	})

	s.ingestEvents(t, []ingestEvent{{
		EventID:     "q-alert-1",
		MachineID:   "m-alert-secret-host",
		SessionID:   "sess-alert-secret-999",
		Project:     "proj-alert-secret",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(1200),
	}})

	deadline := time.Now().Add(5 * time.Second)
	for {
		mu.Lock()
		wLen := len(webhookBodies)
		nLen := len(ntfyBodies)
		mu.Unlock()
		if wLen >= 1 && nLen >= 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for webhook (%d) and ntfy (%d) overage alerts", wLen, nLen)
		}
		time.Sleep(50 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	for _, body := range append(append([]string(nil), webhookBodies...), ntfyBodies...) {
		if !strings.Contains(body, "AlertTightCap") {
			t.Fatalf("alert payload missing quota label AlertTightCap: %s", body)
		}
		for _, secret := range []string{"sess-alert-secret-999", "proj-alert-secret", "/Users/"} {
			if strings.Contains(body, secret) {
				t.Fatalf("alert payload leaked private detail %q: %s", secret, body)
			}
		}
	}
}

func TestSettings_F21_T2_DuplicateProviderModelCollisionWithYAMLOrSQLiteRejected(t *testing.T) {
	requireE2E(t)
	quotasYAML := `schema_version: "1"
quotas:
  - label: "YAML OpenAI GPT54"
    provider: openai
    model: gpt-5.4
    reset_day: 1
    budget:
      tokens_per_month: 5000
`
	s := startTestServer(t, serverOpts{
		IngestToken: "set-f21-t2-1",
		QuotasYAML:  quotasYAML,
	})
	client := s.authClient(t)

	// Collide with YAML (openai, gpt-5.4).
	collideYAML := url.Values{}
	collideYAML.Set("label", "Duplicate YAML")
	collideYAML.Set("provider", "openai")
	collideYAML.Set("model", "gpt-5.4")
	collideYAML.Set("reset_day", "1")
	collideYAML.Set("tokens_per_month", "1000")
	c1, _, b1 := ssSubmitQuotaCreate(t, s, client, collideYAML, true)
	if c1 < http.StatusBadRequest || c1 >= http.StatusInternalServerError {
		t.Fatalf("YAML collision status = %d, want 4xx; body=%s", c1, string(b1))
	}
	if !strings.Contains(string(b1), `role="alert"`) {
		t.Fatalf("YAML collision response missing accessible role=alert: %s", string(b1))
	}

	// Create valid SQLite quota (anthropic, claude-sonnet-4), then collide with itself.
	validSQL := url.Values{}
	validSQL.Set("label", "SQLite Anthropic")
	validSQL.Set("provider", "anthropic")
	validSQL.Set("model", "claude-sonnet-4")
	validSQL.Set("reset_day", "1")
	validSQL.Set("tokens_per_month", "3000")
	cOk, _, bOk := ssSubmitQuotaCreate(t, s, client, validSQL, true)
	if cOk != http.StatusOK {
		t.Fatalf("valid SQLite quota failed: %d %s", cOk, string(bOk))
	}

	collideSQL := url.Values{}
	collideSQL.Set("label", "Duplicate SQLite")
	collideSQL.Set("provider", "anthropic")
	collideSQL.Set("model", "claude-sonnet-4")
	collideSQL.Set("reset_day", "1")
	collideSQL.Set("tokens_per_month", "4000")
	c2, _, b2 := ssSubmitQuotaCreate(t, s, client, collideSQL, true)
	if c2 < http.StatusBadRequest || c2 >= http.StatusInternalServerError {
		t.Fatalf("SQLite collision status = %d, want 4xx; body=%s", c2, string(b2))
	}
}

func TestSettings_F21_T2_InvalidQuotaInputsRejected(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{IngestToken: "set-f21-t2-2"})
	client := s.authClient(t)

	badCases := []struct {
		name string
		vals map[string]string
	}{
		{"reset_day=0", map[string]string{"label": "Bad1", "reset_day": "0", "tokens_per_month": "1000"}},
		{"reset_day=32", map[string]string{"label": "Bad2", "reset_day": "32", "tokens_per_month": "1000"}},
		{"tokens=0", map[string]string{"label": "Bad3", "reset_day": "1", "tokens_per_month": "0"}},
		{"tokens=-50", map[string]string{"label": "Bad4", "reset_day": "1", "tokens_per_month": "-50"}},
		{"cost=-1", map[string]string{"label": "Bad5", "reset_day": "1", "cost_per_month": "-1"}},
		{"both_budgets_empty", map[string]string{"label": "Bad6", "reset_day": "1"}},
		{"non_numeric_tokens", map[string]string{"label": "Bad7", "reset_day": "1", "tokens_per_month": "abc"}},
	}

	for _, tc := range badCases {
		form := url.Values{}
		for k, v := range tc.vals {
			form.Set(k, v)
		}
		code, _, body := ssSubmitQuotaCreate(t, s, client, form, true)
		if code != http.StatusBadRequest {
			t.Fatalf("%s status = %d, want 400; body=%s", tc.name, code, string(body))
		}
		if !strings.Contains(string(body), `role="alert"`) {
			t.Fatalf("%s missing accessible role=alert: %s", tc.name, string(body))
		}
	}

	var quotas []ssQuotaRow
	getJSONWithClient(t, client, s.BaseURL+"/api/v1/quotas", &quotas)
	if len(quotas) != 0 {
		t.Fatalf("invalid quota inputs persisted rows: %+v", quotas)
	}
}

func TestSettings_F21_T2_QuotaPostRequiresAuthCSRFAndSameOrigin(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{
		IngestToken: "set-f21-t2-3",
		Username:    "admin",
		Password:    "set-quota-pass",
	})
	client := s.authClient(t)
	settingsHTML := ssGetHTML(t, client, s.BaseURL+"/settings")
	createForm, _, _ := ssDiscoverQuotaForms(settingsHTML)
	if createForm.Action == "" {
		t.Fatalf("/settings missing quota create form")
	}
	createAction := createForm.Action
	csrf := extractCSRFToken(t, []byte(settingsHTML))

	// Unauth POST.
	unauthForm := url.Values{}
	unauthForm.Set("csrf_token", csrf)
	unauthForm.Set("label", "Unauth Cap")
	unauthForm.Set("reset_day", "1")
	unauthForm.Set("tokens_per_month", "1000")
	cUnauth, _, _ := ssPostForm(t, s.Client, s.BaseURL+createAction, s.BaseURL, s.BaseURL+"/settings", unauthForm)
	if cUnauth != http.StatusUnauthorized && cUnauth != http.StatusSeeOther {
		t.Fatalf("unauth quota POST status = %d, want 401 or 303", cUnauth)
	}

	// Missing CSRF.
	noCSRF := url.Values{}
	noCSRF.Set("label", "NoCSRF Cap")
	noCSRF.Set("reset_day", "1")
	noCSRF.Set("tokens_per_month", "1000")
	cNoCSRF, _, _ := ssPostForm(t, client, s.BaseURL+createAction, s.BaseURL, s.BaseURL+"/settings", noCSRF)
	if cNoCSRF != http.StatusForbidden {
		t.Fatalf("missing CSRF quota POST status = %d, want 403", cNoCSRF)
	}

	// Evil Origin.
	evilForm := url.Values{}
	evilForm.Set("csrf_token", csrf)
	evilForm.Set("label", "EvilOrigin Cap")
	evilForm.Set("reset_day", "1")
	evilForm.Set("tokens_per_month", "1000")
	cEvil, _, _ := ssPostForm(t, client, s.BaseURL+createAction, "https://evil.example", s.BaseURL+"/settings", evilForm)
	if cEvil != http.StatusForbidden {
		t.Fatalf("evil Origin quota POST status = %d, want 403", cEvil)
	}
}

func TestSettings_F21_T2_BackupAgeIgnoresSidecarsAndPreMigrationSnapshots(t *testing.T) {
	requireE2E(t)
	s := startTestServer(t, serverOpts{
		IngestToken:    "set-f21-t2-4",
		BackupInterval: "0",
	})
	if err := os.MkdirAll(s.BackupDir, 0o755); err != nil {
		t.Fatalf("mkdir BackupDir: %v", err)
	}
	for _, name := range []string{
		"tokemon-backup-v4-20261124T000000Z.db.tmp",
		"tokemon-backup-v4-20261124T000000Z.db-wal",
		"tokemon-backup-v4-20261124T000000Z.db-shm",
		"tokemon-v4-before-v5-20261124T000000Z.db",
	} {
		if err := os.WriteFile(filepath.Join(s.BackupDir, name), []byte("non-backup"), 0o600); err != nil {
			t.Fatalf("write decoy %s: %v", name, err)
		}
	}

	client := s.authClient(t)
	settingsHTML := ssGetHTML(t, client, s.BaseURL+"/settings")
	if !strings.Contains(settingsHTML, "No backups yet") && !strings.Contains(settingsHTML, "No backups recorded yet") {
		t.Fatalf("/settings misread sidecars or migration snapshots as backups")
	}
	if strings.Contains(settingsHTML, "just now") || strings.Contains(settingsHTML, " ago") || strings.Contains(settingsHTML, "tokemon-backup-v") || strings.Contains(settingsHTML, "Latest backup") {
		t.Fatalf("/settings rendered backup age or filename for decoy files")
	}
	if !strings.Contains(settingsHTML, `<div class="backup-label">Retained</div>`) || !strings.Contains(settingsHTML, `<div class="backup-value">0</div>`) {
		t.Fatalf("/settings expected Retained 0 when only decoy sidecars/snapshots exist")
	}
}

func TestSettings_F21_T2_AlertDeduplicationAndUnreachableWebhookNonFatal(t *testing.T) {
	requireE2E(t)
	// Part A: Deduplication across repeated ingests in same quota period.
	var callCount atomic.Int32
	dedupSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if _, err := io.Copy(io.Discard, r.Body); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		if err := r.Body.Close(); err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		callCount.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer dedupSrv.Close()

	quotasYAML := `schema_version: "1"
quotas:
  - label: "DedupCap"
    provider: openai
    reset_day: 1
    budget:
      tokens_per_month: 1000
`
	s1 := startTestServer(t, serverOpts{
		IngestToken:     "set-f21-t2-5a",
		QuotasYAML:      quotasYAML,
		AlertWebhookURL: dedupSrv.URL,
	})
	// First single-step 0 -> 110% overage ingest may fire 1 (exceeded) or 2 (warning + exceeded) initial alert calls.
	s1.ingestEvents(t, []ingestEvent{{
		EventID:     "dedup-evt-0",
		MachineID:   "m-dedup",
		SessionID:   "s-dedup",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(1100),
	}})
	deadline := time.Now().Add(4 * time.Second)
	for callCount.Load() == 0 && time.Now().Before(deadline) {
		time.Sleep(50 * time.Millisecond)
	}
	// Give async dispatcher a moment to flush any initial warning/exceeded alerts.
	time.Sleep(300 * time.Millisecond)
	initialCalls := callCount.Load()
	if initialCalls < 1 || initialCalls > 2 {
		t.Fatalf("initial overage webhook fired %d times on 0->110%% jump, want 1 or 2", initialCalls)
	}

	// Subsequent repeated overage ingests in the same period must be deduplicated (0 new calls).
	for i := 1; i < 4; i++ {
		s1.ingestEvents(t, []ingestEvent{{
			EventID:     fmt.Sprintf("dedup-evt-%d", i),
			MachineID:   "m-dedup",
			SessionID:   "s-dedup",
			Provider:    "openai",
			Model:       "gpt-5.4",
			Tool:        "codex",
			TotalTokens: i64(1100),
		}})
	}
	time.Sleep(400 * time.Millisecond)
	if afterCalls := callCount.Load(); afterCalls != initialCalls {
		t.Fatalf("repeated overage ingests fired %d new webhook calls (initial=%d, total=%d), want 0 new (deduplicated)", afterCalls-initialCalls, initialCalls, afterCalls)
	}

	// Part B: Unreachable / 500 webhook must never block or fail ingest.
	failSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	failURL := failSrv.URL
	failSrv.Close()

	s2 := startTestServer(t, serverOpts{
		IngestToken:     "set-f21-t2-5b",
		QuotasYAML:      quotasYAML,
		AlertWebhookURL: failURL,
	})
	res := s2.ingestEvents(t, []ingestEvent{{
		EventID:     "nonfatal-evt-1",
		MachineID:   "m-nf",
		SessionID:   "s-nf",
		Provider:    "openai",
		Model:       "gpt-5.4",
		Tool:        "codex",
		TotalTokens: i64(1500),
	}})
	if res.Accepted != 1 || res.CurrentTotal != 1500 {
		t.Fatalf("ingest failed or dropped tokens when webhook unreachable: %+v", res)
	}
	client2 := s2.authClient(t)
	waitOverviewLifetimeTokens(t, client2, s2.BaseURL, 1500)
}
