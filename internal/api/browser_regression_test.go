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
	body := renderAnalyticsRegressionPage(t)
	for _, want := range []string{
		`<meta name="viewport" content="width=device-width, initial-scale=1">`,
		`<input type="hidden" name="period"`, `<select name="dimension">`, `<select name="machine">`, `<select name="provider">`, `<select name="model">`, `<select name="tool">`,
		"Apply filters", "Reset", "Token trend", "Usage share", "Recent sessions",
		`<table>`, `class="trend-column`, `class="share-point`, `type="button"`,
		`aria-describedby="share-tooltip"`, `role="tooltip"`,
		`.trend-chart { position: relative; z-index: 1; display: flex;`, `.table-scroll { overflow-x: auto;`,
		`point.addEventListener('focus'`, `column.addEventListener('focus'`,
		`@media (prefers-reduced-motion: reduce)`, `window.matchMedia('(prefers-reduced-motion: reduce)')`,
	} {
		if !bytes.Contains(body, []byte(want)) {
			t.Fatalf("desktop analytics output missing %q", want)
		}
	}
	if strings.Contains(string(body), "private prompt") || strings.Contains(string(body), "/private/repo") {
		t.Fatal("desktop dashboard rendered conversation/path content")
	}
}

func TestAnalyticsMobile390BrowserRegression(t *testing.T) {
	body := renderAnalyticsRegressionPage(t)
	for _, want := range []string{
		`@media (max-width: 700px)`, `@media (max-width: 430px)`,
		`main { width: min(100% - 12px, 620px);`,
		`.filter-form, .stats { grid-template-columns: 1fr; }`,
		`.analytics-grid { grid-template-columns: minmax(0, 1fr); }`,
		`.trend-chart { position: relative;`, `overflow-x: auto;`,
		`.share-scroll { margin-left: 58px; overflow-x: auto; }`,
		`.table-scroll { overflow-x: auto;`,
		`@media (prefers-reduced-motion: reduce)`,
	} {
		if !bytes.Contains(body, []byte(want)) {
			t.Fatalf("390px analytics output missing responsive contract %q", want)
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
	for _, want := range []string{"No token activity in this window.", "No known token totals to compare in this window.", "No sessions match these filters."} {
		if !strings.Contains(response.Body.String(), want) {
			t.Fatalf("empty analytics missing %q", want)
		}
	}
}

func renderAnalyticsRegressionPage(t *testing.T) []byte {
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
	return response.Body.Bytes()
}
