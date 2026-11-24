package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/tokemon/tokemon/internal/catalog"
	"github.com/tokemon/tokemon/internal/database"
	"github.com/tokemon/tokemon/internal/usage"
)

func TestM3BSessionsFiltersPaginationExportAndCompactTokens(t *testing.T) {
	store, err := database.Open(t.TempDir()+"/m3b-sessions.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	base := time.Now().UTC().Add(-3 * time.Hour)
	events := make([]usage.Event, 0, 106)
	for i := 0; i < 105; i++ {
		events = append(events, usage.Event{
			SchemaVersion: usage.SchemaVersion,
			EventID:       fmt.Sprintf("m3b-pg-%03d", i),
			Timestamp:     base.Add(time.Duration(i) * time.Second),
			MachineID:     "m-alpha",
			Project:       "proj-alpha",
			Provider:      "openai",
			Model:         "gpt-5",
			Tool:          "codex",
			SessionID:     fmt.Sprintf("sess-m3b-%03d", i),
			InputTokens:   usage.Int64(1500 + int64(i)),
			OutputTokens:  usage.Int64(500),
			TotalTokens:   usage.Int64(2000 + int64(i)),
			TokenAccuracy: usage.AccuracyReported,
			Source:        usage.Source{Adapter: "codex", AdapterVersion: "1"},
		})
	}
	// One session with nil/unknown token counts on a second project.
	events = append(events, usage.Event{
		SchemaVersion: usage.SchemaVersion,
		EventID:       "m3b-unknown",
		Timestamp:     base.Add(200 * time.Second),
		MachineID:     "m-beta",
		Project:       "proj-beta",
		Provider:      "anthropic",
		Model:         "claude-sonnet-4",
		Tool:          "claude-code",
		SessionID:     "sess-m3b-nil",
		TotalTokens:   nil,
		TokenAccuracy: usage.AccuracyUnknown,
		Source:        usage.Source{Adapter: "claude", AdapterVersion: "1"},
	})
	if _, err := store.Ingest(context.Background(), events); err != nil {
		t.Fatal(err)
	}

	server, err := newTestServer(store, "secret")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = server.Close() }()
	handler := testHandler(server)

	// analyticsQueryFromRequest preserves trimmed from/to so the store can fail closed on malformed ranges,
	// while parseQueryDate validates calendar YYYY-MM-DD inputs for <input type="date">.
	req := httptest.NewRequest(http.MethodGet, "/sessions?period=90d&project=proj-alpha&q=sess-m3b&from=2026-11-01&to=2026-11-30", nil)
	q := analyticsQueryFromRequest(req)
	if q.Period != "90d" || q.Project != "proj-alpha" || q.Search != "sess-m3b" || q.From != "2026-11-01" || q.To != "2026-11-30" {
		t.Fatalf("analyticsQueryFromRequest = %+v", q)
	}
	badDate := analyticsQueryFromRequest(httptest.NewRequest(http.MethodGet, "/sessions?from=%202026-02-30%20&to=%20not-a-date%20", nil))
	if badDate.From != "2026-02-30" || badDate.To != "not-a-date" {
		t.Fatalf("analyticsQueryFromRequest should preserve trimmed invalid dates: %+v", badDate)
	}
	if parseQueryDate("2026-02-30") != "" || parseQueryDate("not-a-date") != "" || parseQueryDate("2026-11-1") != "" || parseQueryDate("2026-11-01") != "2026-11-01" {
		t.Fatalf("parseQueryDate validation mismatch")
	}

	// sessionsExportURL preserves project, q, from, to.
	expURL := sessionsExportURL(q, "csv")
	for _, want := range []string{"project=proj-alpha", "q=sess-m3b", "from=2026-11-01", "to=2026-11-30", "format=csv"} {
		if !strings.Contains(expURL, want) {
			t.Fatalf("sessionsExportURL = %q, missing %q", expURL, want)
		}
	}

	// sessionSortStates drops page while keeping filters.
	sortReq := httptest.NewRequest(http.MethodGet, "/sessions?project=proj-alpha&q=sess&page=2", nil)
	sorts := sessionSortStates(sortReq, "when", "desc")
	if strings.Contains(sorts["tokens"].URL, "page=") || !strings.Contains(sorts["tokens"].URL, "project=proj-alpha") {
		t.Fatalf("sessionSortStates tokens URL = %q", sorts["tokens"].URL)
	}

	// Page 1 and Page 2 on /sessions?project=proj-alpha&sort=tokens&dir=asc.
	rec1 := httptest.NewRecorder()
	handler.ServeHTTP(rec1, httptest.NewRequest(http.MethodGet, "/sessions?project=proj-alpha&sort=tokens&dir=asc", nil))
	if rec1.Code != http.StatusOK {
		t.Fatalf("GET /sessions page 1 = %d: %s", rec1.Code, rec1.Body.String())
	}
	body1 := rec1.Body.String()
	for _, want := range []string{
		`Showing 1–100 of 105`,
		`Page 1 of 2`,
		`href="/sessions/sess-m3b-000"`,
		`title="2,000 tokens" aria-label="2,000 tokens">2K</td>`,
		`for="sessions-search"`,
		`for="sessions-project"`,
		`for="sessions-from"`,
		`for="sessions-to"`,
		`Remove filter Project: proj-alpha`,
		`Next &rarr;`,
	} {
		if !strings.Contains(body1, want) {
			t.Fatalf("/sessions page 1 missing %q", want)
		}
	}
	if strings.Contains(body1, `sess-m3b-104`) {
		t.Fatalf("/sessions page 1 unexpectedly included row 105")
	}

	rec2 := httptest.NewRecorder()
	handler.ServeHTTP(rec2, httptest.NewRequest(http.MethodGet, "/sessions?project=proj-alpha&sort=tokens&dir=asc&page=2", nil))
	if rec2.Code != http.StatusOK {
		t.Fatalf("GET /sessions page 2 = %d", rec2.Code)
	}
	body2 := rec2.Body.String()
	for _, want := range []string{`Showing 101–105 of 105`, `Page 2 of 2`, `sess-m3b-104`, `project=proj-alpha`} {
		if !strings.Contains(body2, want) {
			t.Fatalf("/sessions page 2 missing %q", want)
		}
	}

	// Unknown token counts render "Unknown" with unavailable tooltip on /sessions.
	recNil := httptest.NewRecorder()
	handler.ServeHTTP(recNil, httptest.NewRequest(http.MethodGet, "/sessions?project=proj-beta", nil))
	if recNil.Code != http.StatusOK || !strings.Contains(recNil.Body.String(), `class="unknown" title="Token count unavailable" aria-label="Unknown">Unknown</td>`) {
		t.Fatalf("/sessions nil token cell unexpected: %s", recNil.Body.String())
	}

	// Compact tokens and session links on / Recent Sessions.
	recDash := httptest.NewRecorder()
	handler.ServeHTTP(recDash, httptest.NewRequest(http.MethodGet, "/", nil))
	if recDash.Code != http.StatusOK {
		t.Fatalf("GET / = %d", recDash.Code)
	}
	dashBody := recDash.Body.String()
	if !strings.Contains(dashBody, `class="session-link" href="/sessions/sess-m3b-`) ||
		!strings.Contains(dashBody, `title="2,104 tokens" aria-label="2,104 tokens">2.1K</td>`) {
		t.Fatalf("dashboard Recent Sessions missing session link or compact token cell")
	}

	// /api/v1/sessions preserves []SessionRecord JSON array and pagination headers.
	recAPI := httptest.NewRecorder()
	handler.ServeHTTP(recAPI, httptest.NewRequest(http.MethodGet, "/api/v1/sessions?project=proj-alpha&page=2", nil))
	if recAPI.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/sessions = %d", recAPI.Code)
	}
	if recAPI.Header().Get("X-Total-Count") != "105" || recAPI.Header().Get("X-Page") != "2" {
		t.Fatalf("pagination headers = total=%q page=%q", recAPI.Header().Get("X-Total-Count"), recAPI.Header().Get("X-Page"))
	}
	var apiRows []database.SessionRecord
	if err := json.Unmarshal(recAPI.Body.Bytes(), &apiRows); err != nil || len(apiRows) != 5 {
		t.Fatalf("/api/v1/sessions JSON len=%d err=%v", len(apiRows), err)
	}

	// /api/v1/sessions/export returns all 105 rows beyond LIMIT 100.
	recExport := httptest.NewRecorder()
	handler.ServeHTTP(recExport, httptest.NewRequest(http.MethodGet, "/api/v1/sessions/export?project=proj-alpha", nil))
	if recExport.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/sessions/export = %d", recExport.Code)
	}
	var exportedRows []database.SessionRecord
	if err := json.Unmarshal(recExport.Body.Bytes(), &exportedRows); err != nil || len(exportedRows) != 105 {
		t.Fatalf("/api/v1/sessions/export len=%d err=%v", len(exportedRows), err)
	}
}

func TestM3BSessionDetailRouteAuthAndPrivacy(t *testing.T) {
	store, err := database.Open(t.TempDir()+"/m3b-detail.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	hashID := "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	t0 := time.Date(2026, 11, 20, 12, 0, 0, 0, time.UTC)
	if _, err := store.Ingest(context.Background(), []usage.Event{
		{
			SchemaVersion: usage.SchemaVersion, EventID: "d1", Timestamp: t0,
			MachineID: "m1", Project: "proj-detail", Provider: "openai", Model: "gpt-5", Tool: "codex",
			SessionID: hashID, InputTokens: usage.Int64(100), OutputTokens: usage.Int64(50),
			TotalTokens: usage.Int64(150), TokenAccuracy: usage.AccuracyReported,
			Source: usage.Source{Adapter: "codex", AdapterVersion: "1"},
		},
		{
			SchemaVersion: usage.SchemaVersion, EventID: "d2", Timestamp: t0.Add(30 * time.Second),
			MachineID: "m1", Project: "proj-detail", Provider: "anthropic", Model: "claude-sonnet-4", Tool: "codex",
			SessionID: hashID, InputTokens: usage.Int64(200), OutputTokens: usage.Int64(100),
			TotalTokens: usage.Int64(300), TokenAccuracy: usage.AccuracyReported,
			Source: usage.Source{Adapter: "codex", AdapterVersion: "1"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	pwServer := passwordAccountServer(t, store)
	defer func() { _ = pwServer.Close() }()

	// Unauthenticated HTML redirects 303 to /login?next=...; API/non-HTML gets 401.
	unauthHTML := httptest.NewRequest(http.MethodGet, "/sessions/"+hashID, nil)
	unauthHTML.Header.Set("Accept", "text/html")
	recRedirect := httptest.NewRecorder()
	pwServer.Handler().ServeHTTP(recRedirect, unauthHTML)
	if recRedirect.Code != http.StatusSeeOther || recRedirect.Header().Get("Location") != "/login?next=%2Fsessions%2F"+url.QueryEscape(hashID) {
		t.Fatalf("unauth HTML status=%d loc=%q", recRedirect.Code, recRedirect.Header().Get("Location"))
	}

	unauthAPI := httptest.NewRequest(http.MethodGet, "/sessions/"+hashID, nil)
	rec401 := httptest.NewRecorder()
	pwServer.Handler().ServeHTTP(rec401, unauthAPI)
	if rec401.Code != http.StatusUnauthorized {
		t.Fatalf("unauth status=%d, want 401", rec401.Code)
	}

	// Authenticated valid sha256:... ID renders 200 metadata-only detail.
	body := renderAccessiblePage(t, pwServer, "/sessions/"+hashID)
	for _, want := range []string{
		hashID,
		`1 model switch`,
		`proj-detail`,
		`Model breakdown`,
		`Event timeline`,
		`href="/sessions"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("/sessions/{id} missing %q", want)
		}
	}
	if strings.Contains(body, "/Users/") || strings.Contains(body, "/home/") {
		t.Fatalf("/sessions/{id} leaked path prefix")
	}

	// Missing/invalid IDs return 404.
	for _, bad := range []string{"/sessions/nonexistent", "/sessions/", "/sessions/" + strings.Repeat("a", usage.MaxSessionIDLength+1)} {
		req := httptest.NewRequest(http.MethodGet, bad, nil)
		req.SetBasicAuth("tokemon", testDashboardToken)
		rec := httptest.NewRecorder()
		pwServer.Handler().ServeHTTP(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("GET %s = %d, want 404", bad, rec.Code)
		}
	}
}

func TestM3BDrillDownsHeatmapAndTrendClickThroughs(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	store, err := database.OpenWithLocation(t.TempDir()+"/m3b-drill.db", catalog.Empty(), ny)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Now().UTC()
	if _, err := store.Ingest(context.Background(), []usage.Event{
		{
			SchemaVersion: usage.SchemaVersion, EventID: "dr-1", Timestamp: now.Add(-10 * time.Minute),
			MachineID: "box+1", Project: "a&b", Provider: "openai", Model: "gpt-5", Tool: "codex",
			SessionID: "sess-dr-1", TotalTokens: usage.Int64(500), TokenAccuracy: usage.AccuracyReported,
			Source: usage.Source{Adapter: "codex", AdapterVersion: "1"},
		},
		{
			SchemaVersion: usage.SchemaVersion, EventID: "dr-2", Timestamp: now.Add(-20 * time.Minute),
			MachineID: "box+1", Project: "", Provider: "anthropic", Model: "unknown", Tool: "claude-code",
			SessionID: "sess-dr-2", TotalTokens: usage.Int64(200), TokenAccuracy: usage.AccuracyReported,
			Source: usage.Source{Adapter: "claude", AdapterVersion: "1"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	server, err := newTestServer(store, "secret")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = server.Close() }()
	handler := testHandler(server)

	// Overview breakdown links and heatmap data-date.
	dashRec := httptest.NewRecorder()
	handler.ServeHTTP(dashRec, httptest.NewRequest(http.MethodGet, "/", nil))
	if dashRec.Code != http.StatusOK {
		t.Fatalf("GET / = %d", dashRec.Code)
	}
	dashBody := dashRec.Body.String()
	for _, want := range []string{
		`href="/sessions?period=all&amp;project=a%26b"`,
		`href="/analytics?period=all&amp;dimension=projects&amp;tool=codex"`,
		`href="/analytics?period=all&amp;dimension=projects&amp;model=gpt-5"`,
		`href="/analytics?period=all&amp;dimension=projects&amp;machine=box%2B1"`,
		`openDaySessions(cell)`,
	} {
		if !strings.Contains(dashBody, want) {
			t.Fatalf("overview missing drill-down %q", want)
		}
	}
	// Unknown buckets must NOT be wrapped in <a class="row-link">.
	if strings.Contains(dashBody, `project=Unknown`) || strings.Contains(dashBody, `model=unknown`) {
		t.Fatalf("overview rendered clickable link for unknown bucket")
	}
	// Future cells must NOT have data-date; non-future cells MUST have data-date.
	futureCellRe := regexp.MustCompile(`<span class="activity-cell[^"]*future[^"]*"[^>]*data-date=`)
	if futureCellRe.MatchString(dashBody) {
		t.Fatalf("future heatmap cell unexpectedly has data-date")
	}
	nonFutureDateRe := regexp.MustCompile(`<span class="activity-cell[^"]*" role="gridcell" data-date="\d{4}-\d{2}-\d{2}"`)
	if !nonFutureDateRe.MatchString(dashBody) {
		t.Fatalf("non-future heatmap cell missing data-date")
	}

	// Analytics breakdown drill-downs & trend bar data-date/from/to across 24h, 30d, all.
	for _, period := range []string{"24h", "30d", "all"} {
		path := "/analytics?period=" + period + "&dimension=projects&machine=box%2B1"
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d", path, rec.Code)
		}
		body := rec.Body.String()
		if regexp.MustCompile(`data-(date|from|to)="\d{4}-\d{2}-\d{2}T`).MatchString(body) {
			t.Fatalf("%s emitted non-date RFC3339 string in data-date/from/to", path)
		}
		if !strings.Contains(body, `data-filter-machine="box&#43;1"`) {
			t.Fatalf("%s missing data-filter-machine on .trend-chart", path)
		}
		if !strings.Contains(body, `href="/sessions?machine=box%2B1&amp;period=`+period+`&amp;project=a%26b"`) {
			t.Fatalf("%s missing project drill-down preserving active filter", path)
		}
		if strings.Contains(body, `project=Unknown`) {
			t.Fatalf("%s rendered clickable link for Unknown project", path)
		}
		// Standalone word \bknown\b invariant must hold on all /analytics periods too.
		if regexp.MustCompile(`(?i)\bknown\b`).MatchString(body) {
			t.Fatalf("%s violated no-standalone-known invariant", path)
		}
	}

	// Analytics non-project dimension drills down to /analytics preserving active filters.
	recModels := httptest.NewRecorder()
	handler.ServeHTTP(recModels, httptest.NewRequest(http.MethodGet, "/analytics?period=30d&dimension=models&machine=box%2B1", nil))
	if recModels.Code != http.StatusOK {
		t.Fatalf("GET /analytics models = %d", recModels.Code)
	}
	modelsBody := recModels.Body.String()
	if !strings.Contains(modelsBody, `href="/analytics?dimension=projects&amp;machine=box%2B1&amp;model=gpt-5&amp;period=30d"`) {
		t.Fatalf("analytics model drill-down missing expected URL in: %s", modelsBody)
	}
	if strings.Contains(modelsBody, `model=unknown`) {
		t.Fatalf("analytics model drill-down linked unknown model")
	}

	// analyticsPeriodURL clears from/to when switching preset period.
	cleared := analyticsPeriodURL(database.AnalyticsQuery{Period: "30d", Dimension: "projects", From: "2026-11-01", To: "2026-11-02"}, "7d")
	if strings.Contains(cleared, "from=") || strings.Contains(cleared, "to=") || !strings.Contains(cleared, "period=7d") {
		t.Fatalf("analyticsPeriodURL did not clear from/to: %q", cleared)
	}
}

func TestM3BMalformedDateQueryFailsClosed(t *testing.T) {
	store, err := database.OpenWithLocation(t.TempDir()+"/m3b-bad-dates.db", catalog.Empty(), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Now().UTC()
	if _, err := store.Ingest(context.Background(), []usage.Event{
		{
			SchemaVersion: usage.SchemaVersion, EventID: "bd-nov1",
			Timestamp: time.Date(2026, 11, 1, 12, 0, 0, 0, time.UTC),
			MachineID: "m1", Project: "proj-nov1", Provider: "openai", Model: "gpt-5", Tool: "codex",
			SessionID: "sess-bd-nov1", TotalTokens: usage.Int64(100), TokenAccuracy: usage.AccuracyReported,
			Source: usage.Source{Adapter: "codex", AdapterVersion: "1"},
		},
		{
			SchemaVersion: usage.SchemaVersion, EventID: "bd-now",
			Timestamp: now.Add(-10 * time.Minute),
			MachineID: "m1", Project: "proj-now", Provider: "openai", Model: "gpt-5", Tool: "codex",
			SessionID: "sess-bd-now", TotalTokens: usage.Int64(250), TokenAccuracy: usage.AccuracyReported,
			Source: usage.Source{Adapter: "codex", AdapterVersion: "1"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	server, err := newTestServer(store, "secret")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = server.Close() }()
	handler := testHandler(server)

	// Baseline sanity check: valid date range returns the seeded Nov 1 event.
	validSessRec := httptest.NewRecorder()
	handler.ServeHTTP(validSessRec, httptest.NewRequest(http.MethodGet, "/api/v1/sessions?from=2026-11-01&to=2026-11-01", nil))
	if validSessRec.Code != http.StatusOK || validSessRec.Header().Get("X-Total-Count") != "1" {
		t.Fatalf("baseline /api/v1/sessions status=%d total=%q", validSessRec.Code, validSessRec.Header().Get("X-Total-Count"))
	}
	validAnalRec := httptest.NewRecorder()
	handler.ServeHTTP(validAnalRec, httptest.NewRequest(http.MethodGet, "/api/v1/analytics?from=2026-11-01&to=2026-11-01", nil))
	var validAnal database.Analytics
	if err := json.Unmarshal(validAnalRec.Body.Bytes(), &validAnal); err != nil || validAnal.Summary.Events != 1 {
		t.Fatalf("baseline /api/v1/analytics events=%d err=%v", validAnal.Summary.Events, err)
	}

	cases := []struct {
		name           string
		query          string
		wantFromInput  string
		wantToInput    string
		wantFilterPill []string
	}{
		{
			name:           "invalid_from_valid_to",
			query:          "from=2026-02-30&to=2026-11-01",
			wantFromInput:  `<input id="sessions-from" type="date" name="from" value="">`,
			wantToInput:    `<input id="sessions-to" type="date" name="to" value="2026-11-01">`,
			wantFilterPill: []string{`Remove filter From: 2026-02-30`, `Remove filter To: 2026-11-01`},
		},
		{
			name:           "valid_from_invalid_to",
			query:          "from=2026-11-01&to=2026-11-1",
			wantFromInput:  `<input id="sessions-from" type="date" name="from" value="2026-11-01">`,
			wantToInput:    `<input id="sessions-to" type="date" name="to" value="">`,
			wantFilterPill: []string{`Remove filter From: 2026-11-01`, `Remove filter To: 2026-11-1`},
		},
		{
			name:           "non_date_from",
			query:          "from=not-a-date",
			wantFromInput:  `<input id="sessions-from" type="date" name="from" value="">`,
			wantToInput:    `<input id="sessions-to" type="date" name="to" value="">`,
			wantFilterPill: []string{`Remove filter From: not-a-date`},
		},
		{
			name:           "inverted_range",
			query:          "from=2026-11-05&to=2026-11-01",
			wantFromInput:  `<input id="sessions-from" type="date" name="from" value="2026-11-05">`,
			wantToInput:    `<input id="sessions-to" type="date" name="to" value="2026-11-01">`,
			wantFilterPill: []string{`Remove filter From: 2026-11-05`, `Remove filter To: 2026-11-01`},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// /api/v1/sessions
			recAPISess := httptest.NewRecorder()
			handler.ServeHTTP(recAPISess, httptest.NewRequest(http.MethodGet, "/api/v1/sessions?"+tc.query, nil))
			if recAPISess.Code != http.StatusOK {
				t.Fatalf("/api/v1/sessions = %d", recAPISess.Code)
			}
			if got := recAPISess.Header().Get("X-Total-Count"); got != "0" {
				t.Fatalf("/api/v1/sessions X-Total-Count = %q, want 0", got)
			}
			var apiSessRows []database.SessionRecord
			if err := json.Unmarshal(recAPISess.Body.Bytes(), &apiSessRows); err != nil || len(apiSessRows) != 0 {
				t.Fatalf("/api/v1/sessions len=%d err=%v", len(apiSessRows), err)
			}

			// /api/v1/sessions/export
			recExpSess := httptest.NewRecorder()
			handler.ServeHTTP(recExpSess, httptest.NewRequest(http.MethodGet, "/api/v1/sessions/export?"+tc.query, nil))
			if recExpSess.Code != http.StatusOK {
				t.Fatalf("/api/v1/sessions/export = %d", recExpSess.Code)
			}
			var expSessRows []database.SessionRecord
			if err := json.Unmarshal(recExpSess.Body.Bytes(), &expSessRows); err != nil || len(expSessRows) != 0 {
				t.Fatalf("/api/v1/sessions/export len=%d err=%v", len(expSessRows), err)
			}

			// /sessions HTML
			recHTMLSess := httptest.NewRecorder()
			handler.ServeHTTP(recHTMLSess, httptest.NewRequest(http.MethodGet, "/sessions?"+tc.query, nil))
			if recHTMLSess.Code != http.StatusOK {
				t.Fatalf("/sessions = %d", recHTMLSess.Code)
			}
			sessBody := recHTMLSess.Body.String()
			for _, want := range append([]string{
				`0 sessions · Invalid date range`,
				`No sessions match these filters.`,
				tc.wantFromInput,
				tc.wantToInput,
			}, tc.wantFilterPill...) {
				if !strings.Contains(sessBody, want) {
					t.Fatalf("/sessions missing %q", want)
				}
			}

			// /api/v1/analytics
			recAPIAnal := httptest.NewRecorder()
			handler.ServeHTTP(recAPIAnal, httptest.NewRequest(http.MethodGet, "/api/v1/analytics?"+tc.query, nil))
			if recAPIAnal.Code != http.StatusOK {
				t.Fatalf("/api/v1/analytics = %d", recAPIAnal.Code)
			}
			var anal database.Analytics
			if err := json.Unmarshal(recAPIAnal.Body.Bytes(), &anal); err != nil {
				t.Fatalf("/api/v1/analytics unmarshal: %v", err)
			}
			if anal.Summary.Events != 0 || anal.Summary.Tokens != 0 || len(anal.Points) != 0 || len(anal.Breakdown) != 0 {
				t.Fatalf("/api/v1/analytics did not fail closed: events=%d tokens=%d points=%d breakdown=%d",
					anal.Summary.Events, anal.Summary.Tokens, len(anal.Points), len(anal.Breakdown))
			}

			// /api/v1/analytics/timeline
			recTimeline := httptest.NewRecorder()
			handler.ServeHTTP(recTimeline, httptest.NewRequest(http.MethodGet, "/api/v1/analytics/timeline?"+tc.query, nil))
			if recTimeline.Code != http.StatusOK {
				t.Fatalf("/api/v1/analytics/timeline = %d", recTimeline.Code)
			}
			var tl struct {
				Points []database.AnalyticsPoint `json:"points"`
			}
			if err := json.Unmarshal(recTimeline.Body.Bytes(), &tl); err != nil || len(tl.Points) != 0 {
				t.Fatalf("/api/v1/analytics/timeline points=%d err=%v", len(tl.Points), err)
			}

			// /api/v1/analytics/export
			recExpAnal := httptest.NewRecorder()
			handler.ServeHTTP(recExpAnal, httptest.NewRequest(http.MethodGet, "/api/v1/analytics/export?"+tc.query, nil))
			if recExpAnal.Code != http.StatusOK {
				t.Fatalf("/api/v1/analytics/export = %d", recExpAnal.Code)
			}
			var expAnal database.Analytics
			if err := json.Unmarshal(recExpAnal.Body.Bytes(), &expAnal); err != nil || expAnal.Summary.Events != 0 || len(expAnal.Breakdown) != 0 {
				t.Fatalf("/api/v1/analytics/export did not fail closed: events=%d breakdown=%d err=%v", expAnal.Summary.Events, len(expAnal.Breakdown), err)
			}

			// /analytics HTML
			recHTMLAnal := httptest.NewRecorder()
			handler.ServeHTTP(recHTMLAnal, httptest.NewRequest(http.MethodGet, "/analytics?"+tc.query, nil))
			if recHTMLAnal.Code != http.StatusOK {
				t.Fatalf("/analytics = %d", recHTMLAnal.Code)
			}
			analBody := recHTMLAnal.Body.String()
			for _, want := range []string{`No token activity in this window.`, `No usage matches these filters.`} {
				if !strings.Contains(analBody, want) {
					t.Fatalf("/analytics missing %q", want)
				}
			}
		})
	}
}
