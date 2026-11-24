package api

import (
	"bytes"
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tokemon/tokemon/internal/catalog"
	"github.com/tokemon/tokemon/internal/database"
	"github.com/tokemon/tokemon/internal/usage"
)

// These tests are deliberately dependency-free browser contracts. They run in
// CI against the rendered server output and make the selectors/keyboard and
// responsive hooks that the live browser check relies on explicit. The
// authenticated desktop and 390px checks are performed against the deployed
// hub with the in-app browser skill (see docs/system-tests.md).
func TestAnalyticsDesktopBrowserRegression(t *testing.T) {
	body, css := renderAnalyticsRegressionPage(t)
	combined := append(append([]byte(nil), body...), css...)
	for _, want := range []string{
		`<meta name="viewport" content="width=device-width, initial-scale=1">`,
		`<details class="export-menu">`, `<details class="filter-details">`,
		`<input type="hidden" name="period"`, `<input type="hidden" name="dimension"`, `<select id="project-filter" name="project">`, `<select id="machine-filter" name="machine">`, `<select id="provider-filter" name="provider">`, `<select id="model-filter" name="model">`, `<select id="harness-filter" name="tool">`,
		"Apply filters", "Reset", "Token trend", "Usage share", "Recent sessions",
		`<table aria-label="Analytics breakdown">`, `class="trend-column`, `class="share-point`, `type="button"`,
		`aria-describedby="share-tooltip"`, `role="tooltip"`,
		`point.addEventListener('focus'`, `column.addEventListener('focus'`,
		`filterForm.requestSubmit`,
		`window.matchMedia('(prefers-reduced-motion: reduce)')`,
	} {
		if !bytes.Contains(body, []byte(want)) {
			t.Fatalf("desktop analytics output missing %q", want)
		}
	}
	if bytes.Contains(body, []byte(`id="dimension-filter"`)) {
		t.Fatal("desktop analytics output still contains duplicate dimension-filter select")
	}
	for _, want := range []string{
		`.trend-chart { position: relative; z-index: 1; display: flex;`, `.table-scroll { overflow-x: auto;`,
		`@media (prefers-reduced-motion: reduce)`,
	} {
		if !bytes.Contains(combined, []byte(want)) {
			t.Fatalf("desktop analytics CSS bundle missing %q", want)
		}
	}
	if strings.Contains(string(body), "private prompt") || strings.Contains(string(body), "/private/repo") {
		t.Fatal("desktop dashboard rendered conversation/path content")
	}
}

func TestAnalyticsMobile390BrowserRegression(t *testing.T) {
	body, css := renderAnalyticsRegressionPage(t)
	combined := append(append([]byte(nil), body...), css...)
	for _, want := range []string{
		`@media (max-width: 820px)`, `@media (max-width: 700px)`, `@media (max-width: 430px)`,
		`main:not(.login-card) { width: min(100% - 20px, 620px);`,
		`main:not(.login-card) { width: min(100% - 12px, 420px);`,
		`.filter-form, .stats { grid-template-columns: 1fr; }`,
		`.analytics-grid { grid-template-columns: minmax(0, 1fr); }`,
		`.trend-chart { position: relative;`, `overflow-x: auto;`,
		`.share-scroll { margin-left: 58px; overflow-x: auto; }`,
		`.table-scroll { overflow-x: auto;`,
		`@media (prefers-reduced-motion: reduce)`,
	} {
		if !bytes.Contains(combined, []byte(want)) {
			t.Fatalf("390px analytics output missing responsive contract %q", want)
		}
	}
}

func TestMobileNavigation390BrowserRegression(t *testing.T) {
	store, err := database.Open(t.TempDir()+"/mobile-nav.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Ingest(context.Background(), []usage.Event{{
		SchemaVersion: usage.SchemaVersion,
		EventID:       "mobile-nav-ev",
		Timestamp:     time.Now().UTC(),
		MachineID:     "box",
		Provider:      "openai",
		Model:         "gpt-5",
		Tool:          "codex",
		SessionID:     "reg-sess",
		TotalTokens:   usage.Int64(42),
		TokenAccuracy: usage.AccuracyReported,
		Source:        usage.Source{Adapter: "codex", AdapterVersion: "1"},
	}}); err != nil {
		t.Fatal(err)
	}
	server, err := newTestServer(store, "mobile-nav-token")
	if err != nil {
		t.Fatal(err)
	}

	pages := []string{"/", "/analytics", "/tokedex", "/sessions", "/sessions/reg-sess", "/insights", "/settings"}
	for _, path := range pages {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.SetBasicAuth("tokemon", testDashboardToken)
		response := httptest.NewRecorder()
		server.Handler().ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("%s status = %d: %s", path, response.Code, response.Body.String())
		}
		body := response.Body.Bytes()
		css := fetchLinkedCSS(t, server, body)
		compactCSS := strings.NewReplacer(" ", "", "\n", "", "\r", "", "\t", "").Replace(string(css))
		for _, media := range []string{`@media (max-width: 820px)`, `@media (max-width: 640px)`, `@media (max-width: 500px)`} {
			if !bytes.Contains(css, []byte(media)) {
				t.Fatalf("%s linked CSS missing mobile breakpoint %q", path, media)
			}
		}
		if !strings.Contains(compactCSS, `.nav-link{padding:6px7px;font-size:10px;}`) {
			t.Fatalf("%s linked CSS missing compact mobile navigation contract", path)
		}
		if !strings.Contains(compactCSS, `.topbar-actions{margin-left:auto;}`) {
			t.Fatalf("%s linked CSS missing .topbar-actions wrap alignment", path)
		}
		for _, want := range []string{
			`<nav class="nav"`,
			`href="/analytics"`,
			`<div class="topbar-actions">`,
			`<a class="settings-link`,
		} {
			if !bytes.Contains(body, []byte(want)) {
				t.Fatalf("%s missing navigation contract %q", path, want)
			}
		}
	}
}

// The breakdown cards' fixed 60/24/16 column split once leaked into the wide
// health and session tables, collapsing their middle columns on top of each
// other. Keep those tables content-sized with labelled cells for phones.
func TestOverviewWideTablesKeepTheirOwnLayout(t *testing.T) {
	store, err := database.Open(t.TempDir()+"/wide-tables.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if err := store.RecordHeartbeat(context.Background(), database.AgentHeartbeat{MachineID: "laptop", OperatingSystem: "darwin", Architecture: "arm64", SourceCount: 3, SourceErrorCount: 1}); err != nil {
		t.Fatal(err)
	}
	server, err := newTestServer(store, "wide-tables-token")
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.SetBasicAuth("tokemon", testDashboardToken)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	body := response.Body.String()
	css := string(fetchLinkedCSS(t, server, response.Body.Bytes()))
	combined := body + "\n" + css
	for _, want := range []string{
		`.health-table, .sessions-table { table-layout: auto; }`,
		`.health-table :is(th, td):first-child, .sessions-table :is(th, td):first-child { width: 100%;`,
	} {
		if !strings.Contains(combined, want) {
			t.Errorf("overview missing wide-table CSS contract %q", want)
		}
	}
	for _, want := range []string{
		`<td data-label="Last seen">`, `<td data-label="Lifetime">`, `class="sync-error">1 source error<span class="table-muted">of 3 sources</span>`,
		`<a class="settings-link" href="/settings"`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("overview missing wide-table HTML contract %q", want)
		}
	}
}

func TestAnalyticsEmptyStateBrowserRegression(t *testing.T) {
	store, err := database.Open(t.TempDir()+"/empty.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server, err := newTestServer(store, "empty-token")
	if err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/analytics?period=all&dimension=models", nil)
	request.SetBasicAuth("tokemon", testDashboardToken)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("empty analytics status = %d: %s", response.Code, response.Body.String())
	}
	for _, want := range []string{"No token activity in this window.", "No token totals to compare in this window.", "No sessions match these filters."} {
		if !strings.Contains(response.Body.String(), want) {
			t.Fatalf("empty analytics missing %q", want)
		}
	}
}

func fetchLinkedCSS(t *testing.T, server *Server, html []byte) []byte {
	t.Helper()
	matches := stylesheetPattern.FindAllSubmatch(html, -1)
	if len(matches) == 0 {
		t.Fatalf("rendered HTML missing linked stylesheet")
	}
	var out bytes.Buffer
	for _, match := range matches {
		href := string(match[1])
		req := httptest.NewRequest(http.MethodGet, href, nil)
		rec := httptest.NewRecorder()
		server.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("stylesheet %s status = %d", href, rec.Code)
		}
		if ct := rec.Header().Get("Content-Type"); !strings.Contains(ct, "text/css") {
			t.Fatalf("stylesheet %s Content-Type = %q, want text/css", href, ct)
		}
		out.Write(rec.Body.Bytes())
		out.WriteByte('\n')
	}
	return out.Bytes()
}

func renderAnalyticsRegressionPage(t *testing.T) ([]byte, []byte) {
	t.Helper()
	store, err := database.Open(t.TempDir()+"/analytics.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server, err := newTestServer(store, "analytics-token")
	if err != nil {
		t.Fatal(err)
	}
	base := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	events := []usage.Event{
		{
			SchemaVersion: usage.SchemaVersion,
			EventID:       "browser-desktop-event",
			Timestamp:     base,
			MachineID:     "desktop-agent",
			Project:       "desktop-project",
			Provider:      "anthropic",
			Model:         "claude-sonnet-4",
			Tool:          "claude-code",
			InputTokens:   usage.Int64(70),
			OutputTokens:  usage.Int64(30),
			TotalTokens:   usage.Int64(100),
			Currency:      "USD",
			TokenAccuracy: usage.AccuracyReported,
			Source:        usage.Source{Adapter: "claude-code", AdapterVersion: "test"},
		},
		{
			SchemaVersion: usage.SchemaVersion,
			EventID:       "browser-mobile-event",
			Timestamp:     base.Add(time.Hour),
			MachineID:     "mobile-agent",
			Project:       "mobile-project",
			Provider:      "openai",
			Model:         "gpt-5",
			Tool:          "codex",
			InputTokens:   usage.Int64(20),
			OutputTokens:  usage.Int64(10),
			TotalTokens:   usage.Int64(30),
			Currency:      "USD",
			TokenAccuracy: usage.AccuracyReported,
			Source:        usage.Source{Adapter: "codex", AdapterVersion: "test"},
		},
	}
	if _, err := store.Ingest(context.Background(), events); err != nil {
		t.Fatal(err)
	}
	request := httptest.NewRequest(http.MethodGet, "/analytics?period=7d&dimension=models&machine=desktop-agent", nil)
	request.SetBasicAuth("tokemon", testDashboardToken)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("analytics status = %d: %s", response.Code, response.Body.String())
	}
	body := response.Body.Bytes()
	return body, fetchLinkedCSS(t, server, body)
}
