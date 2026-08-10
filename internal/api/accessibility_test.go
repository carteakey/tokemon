package api

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/tokemon/tokemon/internal/catalog"
	"github.com/tokemon/tokemon/internal/database"
	"github.com/tokemon/tokemon/internal/usage"
)

var (
	tablePattern     = regexp.MustCompile(`(?s)<table\b([^>]*)>(.*?)</table>`)
	headerPattern    = regexp.MustCompile(`(?s)<th\b([^>]*)>`)
	colorPattern     = regexp.MustCompile(`--([a-z-]+):\s*(#[0-9a-fA-F]{6})`)
	focusNoneRegex   = regexp.MustCompile(`(?is):focus-visible[^}]*outline\s*:\s*none`)
	positiveTabRegex = regexp.MustCompile(`(?i)tabindex\s*=\s*["']?[1-9]`)
)

func TestAccessiblePagesExposeDeterministicContracts(t *testing.T) {
	store, err := database.Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Ingest(context.Background(), []usage.Event{{
		SchemaVersion: usage.SchemaVersion,
		EventID:       "accessibility-fixture",
		Timestamp:     time.Now().UTC(),
		MachineID:     "accessibility-machine",
		Provider:      "provider",
		Model:         "model",
		Tool:          "tool",
		TotalTokens:   usage.Int64(42),
		TokenAccuracy: usage.AccuracyReported,
		Source:        usage.Source{Adapter: "test", AdapterVersion: "1"},
	}}); err != nil {
		t.Fatal(err)
	}
	server, err := newTestServer(store, "secret")
	if err != nil {
		t.Fatal(err)
	}

	for _, path := range []string{"/", "/settings", "/analytics", "/sessions"} {
		body := renderAccessiblePage(t, server, path)
		assertAccessibleDocument(t, path, body)
	}

	invalid := httptest.NewRequest(http.MethodPost, "/settings/aliases", strings.NewReader("model_identity=a&model_alias=b"))
	invalid.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	invalid.SetBasicAuth("tokemon", testDashboardToken)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, invalid)
	if response.Code != http.StatusForbidden || !strings.Contains(response.Body.String(), `role="alert"`) {
		t.Fatalf("settings error response = %d %s", response.Code, response.Body.String())
	}
}

func renderAccessiblePage(t *testing.T, server *Server, path string) string {
	t.Helper()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	request.SetBasicAuth("tokemon", testDashboardToken)
	response := httptest.NewRecorder()
	server.Handler().ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("%s status = %d: %s", path, response.Code, response.Body.String())
	}
	return response.Body.String()
}

func assertAccessibleDocument(t *testing.T, path, body string) {
	t.Helper()
	for _, required := range []string{
		`<html lang="en">`,
		`<meta name="viewport"`,
		`<a class="skip-link" href="#main-content">Skip to content</a>`,
		`<h1`,
		`<h2`,
		`@media (max-width:`,
		`:focus-visible`,
		`prefers-reduced-motion`,
	} {
		if !strings.Contains(body, required) {
			t.Errorf("%s missing accessibility contract %q", path, required)
		}
	}
	if focusNoneRegex.MatchString(body) {
		t.Errorf("%s removes the visible focus indicator", path)
	}
	if positiveTabRegex.MatchString(body) {
		t.Errorf("%s uses a positive tabindex and breaks predictable keyboard order", path)
	}
	assertContrastPolicy(t, path, body)

	tables := tablePattern.FindAllStringSubmatch(body, -1)
	if len(tables) == 0 && path != "/settings" {
		t.Errorf("%s has no semantic data table", path)
	}
	for index, table := range tables {
		attrs, contents := table[1], table[2]
		if !strings.Contains(attrs, "aria-label=") && !strings.Contains(contents, "<caption") {
			t.Errorf("%s table %d has no accessible name", path, index+1)
		}
		for _, header := range headerPattern.FindAllStringSubmatch(contents, -1) {
			if !strings.Contains(header[1], "scope=") {
				t.Errorf("%s table %d has a header without scope", path, index+1)
			}
		}
	}

	if path == "/" {
		for _, required := range []string{`role="grid"`, `role="gridcell"`, `aria-label="Project token usage"`, `aria-label="Model token usage"`} {
			if !strings.Contains(body, required) {
				t.Errorf("overview missing %q", required)
			}
		}
	}
	if path == "/settings" {
		for _, required := range []string{`aria-label="Alias for`, `aria-labelledby="settings-form-title"`, `type="submit"`} {
			if !strings.Contains(body, required) {
				t.Errorf("settings missing %q", required)
			}
		}
	}
	if path == "/analytics" {
		for _, required := range []string{`for="dimension-filter"`, `for="machine-filter"`, `aria-label="Usage share data table"`, `aria-describedby="share-tooltip"`, `type="button"`} {
			if !strings.Contains(body, required) {
				t.Errorf("analytics missing %q", required)
			}
		}
	}
}

func assertContrastPolicy(t *testing.T, path, body string) {
	t.Helper()
	colors := make(map[string]string)
	for _, match := range colorPattern.FindAllStringSubmatch(body, -1) {
		if _, exists := colors[match[1]]; !exists {
			colors[match[1]] = match[2]
		}
	}
	background, ok := colors["bg"]
	if !ok {
		t.Errorf("%s has no background color token", path)
		return
	}
	for _, name := range []string{"text", "muted", "faint", "accent", "warm"} {
		foreground, ok := colors[name]
		if !ok {
			t.Errorf("%s has no %s color token", path, name)
			continue
		}
		if ratio := contrastRatio(foreground, background); ratio < 4.5 {
			t.Errorf("%s %s contrast = %.2f, want at least 4.5", path, name, ratio)
		}
	}
}

func contrastRatio(foreground, background string) float64 {
	foregroundLuminance := relativeLuminance(parseHexColor(foreground))
	backgroundLuminance := relativeLuminance(parseHexColor(background))
	if foregroundLuminance < backgroundLuminance {
		foregroundLuminance, backgroundLuminance = backgroundLuminance, foregroundLuminance
	}
	return (foregroundLuminance + 0.05) / (backgroundLuminance + 0.05)
}

func parseHexColor(value string) [3]float64 {
	var channels [3]float64
	for index := range channels {
		component := value[index*2+1 : index*2+3]
		parsed, _ := strconv.ParseUint(component, 16, 8)
		channels[index] = float64(parsed) / 255
	}
	return channels
}

func relativeLuminance(color [3]float64) float64 {
	for index, channel := range color {
		if channel <= 0.03928 {
			color[index] = channel / 12.92
		} else {
			color[index] = math.Pow((channel+0.055)/1.055, 2.4)
		}
	}
	return .2126*color[0] + .7152*color[1] + .0722*color[2]
}
