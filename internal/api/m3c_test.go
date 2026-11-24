package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/tokemon/tokemon/internal/catalog"
	"github.com/tokemon/tokemon/internal/database"
	"github.com/tokemon/tokemon/internal/usage"
)

func TestM3CAnalyticsFilterPillsAndStatMeta(t *testing.T) {
	store, err := database.OpenWithLocation(t.TempDir()+"/m3c-analytics.db", catalog.Empty(), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if _, err := store.Ingest(context.Background(), []usage.Event{
		{
			SchemaVersion: usage.SchemaVersion,
			EventID:       "m3c-a1",
			Timestamp:     time.Date(2026, 11, 1, 10, 0, 0, 0, time.UTC),
			MachineID:     "desktop-agent",
			Project:       "tokemon",
			Provider:      "openai",
			Model:         "gpt-5",
			Tool:          "codex",
			SessionID:     "sess-a1",
			TotalTokens:   usage.Int64(500),
			TokenAccuracy: usage.AccuracyReported,
			Source:        usage.Source{Adapter: "codex", AdapterVersion: "1"},
		},
		{
			SchemaVersion: usage.SchemaVersion,
			EventID:       "m3c-a2",
			Timestamp:     time.Now().UTC().Add(-time.Hour),
			MachineID:     "laptop-agent",
			Project:       "other",
			Provider:      "anthropic",
			Model:         "claude-sonnet-4",
			Tool:          "claude-code",
			SessionID:     "sess-a2",
			TotalTokens:   usage.Int64(250),
			TokenAccuracy: usage.AccuracyReported,
			Source:        usage.Source{Adapter: "claude-code", AdapterVersion: "1"},
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

	// Active filter pills preserve period and dimension on one-click removal.
	pillRec := httptest.NewRecorder()
	handler.ServeHTTP(pillRec, httptest.NewRequest(http.MethodGet, "/analytics?period=7d&dimension=models&machine=desktop-agent&project=tokemon", nil))
	if pillRec.Code != http.StatusOK {
		t.Fatalf("GET /analytics with filters = %d", pillRec.Code)
	}
	pillBody := pillRec.Body.String()
	for _, want := range []string{
		`Filters <span class="filter-count" aria-label="2 active">(2)</span>`,
		`Remove filter Machine: desktop-agent`,
		`Remove filter Project: tokemon`,
		`href="/analytics?dimension=models&amp;period=7d&amp;project=tokemon"`,
		`href="/analytics?dimension=models&amp;machine=desktop-agent&amp;period=7d"`,
		`href="/analytics?dimension=models&amp;period=7d"`,
		`Clear all`,
	} {
		if !strings.Contains(pillBody, want) {
			t.Fatalf("filtered /analytics missing %q", want)
		}
	}

	// Invalid date queries render "Invalid date range" in .stat-meta instead of raw "not-a-date -> ".
	for _, q := range []string{
		"from=not-a-date",
		"to=bad",
		"from=2026-11-05&to=2026-11-01",
		"from=2099-01-01",
	} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/analytics?"+q, nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("GET /analytics?%s = %d", q, rec.Code)
		}
		body := rec.Body.String()
		if !strings.Contains(body, `<div class="stat-meta">Invalid date range</div>`) {
			t.Fatalf("/analytics?%s missing clean Invalid date range .stat-meta", q)
		}
		if strings.Contains(body, `not-a-date →`) || strings.Contains(body, `→ bad`) || strings.Contains(body, `2099-01-01 →`) {
			t.Fatalf("/analytics?%s leaked raw malformed range in .stat-meta", q)
		}

		apiRec := httptest.NewRecorder()
		handler.ServeHTTP(apiRec, httptest.NewRequest(http.MethodGet, "/api/v1/analytics?"+q, nil))
		if apiRec.Code != http.StatusOK {
			t.Fatalf("GET /api/v1/analytics?%s = %d", q, apiRec.Code)
		}
		var payload database.Analytics
		if err := json.Unmarshal(apiRec.Body.Bytes(), &payload); err != nil {
			t.Fatalf("decode /api/v1/analytics?%s: %v", q, err)
		}
		if payload.StartDate != "" || payload.EndDate != "" {
			t.Fatalf("/api/v1/analytics?%s = (%q, %q), want empty start_date/end_date", q, payload.StartDate, payload.EndDate)
		}
		if payload.Summary.Events != 0 || len(payload.Points) != 0 {
			t.Fatalf("/api/v1/analytics?%s returned non-empty data: events=%d points=%d", q, payload.Summary.Events, len(payload.Points))
		}
	}

	// Single-day to-only query collapses "2026-11-01 -> 2026-11-01" to "2026-11-01".
	singleRec := httptest.NewRecorder()
	handler.ServeHTTP(singleRec, httptest.NewRequest(http.MethodGet, "/analytics?to=2026-11-01", nil))
	if singleRec.Code != http.StatusOK {
		t.Fatalf("GET /analytics?to=2026-11-01 = %d", singleRec.Code)
	}
	singleBody := singleRec.Body.String()
	if !strings.Contains(singleBody, `<div class="stat-meta">2026-11-01</div>`) {
		t.Fatalf("single-day to-only /analytics missing collapsed date .stat-meta")
	}
	if strings.Contains(singleBody, `2026-11-01 → 2026-11-01`) {
		t.Fatalf("single-day to-only /analytics still rendered duplicate date range")
	}
}

func TestM3CDashboardETag304HeartbeatAndNoReload(t *testing.T) {
	store, err := database.Open(t.TempDir()+"/m3c-dash.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	server, err := newTestServer(store, "secret")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = server.Close() }()

	if _, err := store.Ingest(context.Background(), []usage.Event{
		dashboardEvent("etag-ev-1", "box-a", 120),
	}); err != nil {
		t.Fatal(err)
	}

	handler := testHandler(server)
	// Cold then warm GET / to attach pre-rendered html + etag.
	for i := 0; i < 2; i++ {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("warm GET / #%d = %d", i+1, rec.Code)
		}
	}

	first := httptest.NewRecorder()
	handler.ServeHTTP(first, httptest.NewRequest(http.MethodGet, "/", nil))
	if first.Code != http.StatusOK {
		t.Fatalf("GET / = %d", first.Code)
	}
	etag := first.Header().Get("ETag")
	if etag == "" {
		t.Fatal("GET / did not emit ETag header")
	}
	if strings.Contains(first.Body.String(), "location.reload") {
		t.Fatal("dashboard HTML still contains location.reload")
	}
	if got := first.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("GET / Cache-Control = %q, want no-store", got)
	}

	// Warm /api/v1/evolution and confirm ETag / 304.
	evRec := httptest.NewRecorder()
	handler.ServeHTTP(evRec, httptest.NewRequest(http.MethodGet, "/api/v1/evolution", nil))
	if evRec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/evolution = %d", evRec.Code)
	}
	evETag := evRec.Header().Get("ETag")
	if evETag == "" {
		t.Fatal("GET /api/v1/evolution did not emit ETag")
	}
	ev304Req := httptest.NewRequest(http.MethodGet, "/api/v1/evolution", nil)
	ev304Req.Header.Set("If-None-Match", evETag)
	ev304 := httptest.NewRecorder()
	handler.ServeHTTP(ev304, ev304Req)
	if ev304.Code != http.StatusNotModified || ev304.Body.Len() != 0 {
		t.Fatalf("GET /api/v1/evolution with If-None-Match = %d len=%d, want 304 empty", ev304.Code, ev304.Body.Len())
	}

	// Heartbeat invalidates overviewCache so Machine Health refreshes on next SWR cycle.
	hbReq := httptest.NewRequest(http.MethodPost, "/api/v1/agents/heartbeat", strings.NewReader(`{"machine_id":"box-hb","agent_version":"test","operating_system":"darwin","architecture":"arm64","adapters":[{"id":"codex"}],"source_count":1,"source_error_count":0}`))
	hbReq.Header.Set("Authorization", "Bearer secret")
	hbRec := httptest.NewRecorder()
	server.Handler().ServeHTTP(hbRec, hbReq)
	if hbRec.Code != http.StatusOK {
		t.Fatalf("POST heartbeat = %d: %s", hbRec.Code, hbRec.Body.String())
	}

	staleRec := httptest.NewRecorder()
	handler.ServeHTTP(staleRec, httptest.NewRequest(http.MethodGet, "/", nil))
	if staleRec.Header().Get("X-Tokemon-Refreshing") != "1" {
		t.Fatalf("GET / after heartbeat missing X-Tokemon-Refreshing: 1 (got %q)", staleRec.Header().Get("X-Tokemon-Refreshing"))
	}

	// Wait for background refresh to publish fresh entry with new ETag.
	deadline := time.Now().Add(5 * time.Second)
	var freshETag string
	for {
		fetched, html := cachedDashboardEntry(server)
		if !fetched.IsZero() && len(html) > 0 && bytes.Contains(html, []byte(`data-machine="box-hb"`)) {
			server.overviewMu.Lock()
			freshETag = server.overviewCache.etag
			server.overviewMu.Unlock()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("overview cache did not refresh machine health after heartbeat")
		}
		runtime.Gosched()
	}
	if freshETag == "" || freshETag == etag {
		t.Fatalf("fresh ETag = %q, old ETag = %q, expected distinct non-empty ETag", freshETag, etag)
	}

	// Close store to verify 304 on GET / needs zero database access.
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	req304 := httptest.NewRequest(http.MethodGet, "/", nil)
	req304.Header.Set("If-None-Match", freshETag)
	rec304 := httptest.NewRecorder()
	handler.ServeHTTP(rec304, req304)
	if rec304.Code != http.StatusNotModified || rec304.Body.Len() != 0 {
		t.Fatalf("DB-closed GET / with If-None-Match = %d len=%d, want 304 empty", rec304.Code, rec304.Body.Len())
	}
	if got := rec304.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("304 Cache-Control = %q, want no-store", got)
	}
}

func TestM3CDashboardAuthExpiryRedirectsEvenWithIfNoneMatch(t *testing.T) {
	store, err := database.Open(t.TempDir()+"/m3c-auth.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	server := passwordAccountServer(t, store)
	defer func() { _ = server.Close() }()

	req := httptest.NewRequest(http.MethodGet, "/", nil)
	req.Header.Set("Accept", "text/html")
	req.Header.Set("If-None-Match", `"stale-etag"`)
	rec := httptest.NewRecorder()
	server.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusSeeOther || rec.Header().Get("Location") != "/login?next=%2F" {
		t.Fatalf("expired auth GET / with If-None-Match = %d location=%q, want 303 /login?next=%%2F", rec.Code, rec.Header().Get("Location"))
	}
}

func TestM3CInsightsRovingTableAndSettingsAliases(t *testing.T) {
	price := 3.0
	modelCatalog := catalog.MustNew(map[string]catalog.Model{
		"claude-sonnet-4": {
			Provider: "anthropic", DisplayName: "Claude Sonnet 4", Aliases: []string{"claude-sonnet-4"}, Tier: "S",
			Pricing: catalog.Pricing{Currency: "USD", Mode: "standard", VerifiedAt: "2026-11-07", Source: "https://example.com/pricing", InputPricePerMillion: &price, OutputPricePerMillion: &price},
		},
	})
	store, err := database.Open(t.TempDir()+"/m3c-settings.db", modelCatalog)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Now().UTC()
	if _, err := store.Ingest(context.Background(), []usage.Event{
		{
			SchemaVersion: usage.SchemaVersion,
			EventID:       "m3c-s1",
			Timestamp:     now.Add(-2 * time.Minute),
			MachineID:     "mac-high",
			Provider:      "anthropic",
			Model:         "claude-sonnet-4",
			Tool:          "claude-code",
			TotalTokens:   usage.Int64(5000),
			TokenAccuracy: usage.AccuracyReported,
			Source:        usage.Source{Adapter: "claude-code", AdapterVersion: "1"},
		},
		{
			SchemaVersion: usage.SchemaVersion,
			EventID:       "m3c-s2",
			Timestamp:     now.Add(-time.Minute),
			MachineID:     "mac-low",
			Provider:      "openai",
			Model:         "gpt-5",
			Tool:          "codex",
			TotalTokens:   usage.Int64(1200),
			TokenAccuracy: usage.AccuracyReported,
			Source:        usage.Source{Adapter: "codex", AdapterVersion: "1"},
		},
		{
			SchemaVersion: usage.SchemaVersion,
			EventID:       "m3c-s3",
			Timestamp:     now.Add(-30 * time.Second),
			MachineID:     "mac-unknown-only",
			Provider:      "openai",
			Model:         "unknown-tok-model",
			Tool:          "codex",
			TokenAccuracy: usage.AccuracyUnknown,
			Source:        usage.Source{Adapter: "codex", AdapterVersion: "1"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.SetDisplayAlias(context.Background(), database.AliasKindModel, "unused-aliased-model", "Unused Alias"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetDisplayAlias(context.Background(), database.AliasKindModel, "claude-sonnet-4", "Sonnet 4"); err != nil {
		t.Fatal(err)
	}

	// Enabled Tokedex server.
	enabled, err := NewWithConfig(store, Config{IngestToken: "secret", DashboardToken: testDashboardToken, EnableTokedex: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = enabled.Close() }()

	// /insights checks: roving tabindex, aria-live readout, sr-only table, no standalone \bknown\b.
	insRec := httptest.NewRecorder()
	testHandler(enabled).ServeHTTP(insRec, httptest.NewRequest(http.MethodGet, "/insights?period=7d", nil))
	if insRec.Code != http.StatusOK {
		t.Fatalf("GET /insights = %d", insRec.Code)
	}
	insBody := insRec.Body.String()
	for _, want := range []string{
		`class="rhythm-readout" aria-live="polite"`,
		`id="rhythm-readout-value"`,
		`class="histogram" role="region" aria-label="24-hour token rhythm"`,
		`<button type="button" class="hour`,
		`id="rhythm-data-table"`,
		`<table aria-label="24-hour rhythm data table">`,
	} {
		if !strings.Contains(insBody, want) {
			t.Fatalf("/insights missing %q", want)
		}
	}
	if regexp.MustCompile(`(?i)\bknown\b`).MatchString(insBody) {
		t.Fatalf("/insights violated no-standalone-known invariant")
	}

	// /settings with EnableTokedex: true.
	setRec := httptest.NewRecorder()
	testHandler(enabled).ServeHTTP(setRec, httptest.NewRequest(http.MethodGet, "/settings", nil))
	if setRec.Code != http.StatusOK {
		t.Fatalf("GET /settings = %d", setRec.Code)
	}
	setBody := setRec.Body.String()
	for _, want := range []string{
		`id="alias-filter"`,
		`id="alias-filter-status"`,
		`5K tokens · 1 event`,
		`title="5,000 tokens · 1 event"`,
		`Unknown tokens · 1 event`,
		`No usage recorded yet`,
		`href="/analytics?dimension=models&amp;model=claude-sonnet-4"`,
		`href="/tokedex?q=claude-sonnet-4"`,
		`href="/analytics?dimension=machines&amp;machine=mac-high"`,
		`data-reset-alias aria-label="Reset alias for claude-sonnet-4"`,
		`data-reset-alias aria-label="Reset alias for gpt-5" disabled`,
	} {
		if !strings.Contains(setBody, want) {
			t.Fatalf("/settings missing %q", want)
		}
	}
	// Verify Tokens DESC ordering: claude-sonnet-4 (5000) before gpt-5 (1200) before unknown-tok-model (0, 1 event) before unused-aliased-model (no usage).
	idxSonnet := strings.Index(setBody, `data-identity="claude-sonnet-4"`)
	idxGPT := strings.Index(setBody, `data-identity="gpt-5"`)
	idxUnknown := strings.Index(setBody, `data-identity="unknown-tok-model"`)
	idxUnused := strings.Index(setBody, `data-identity="unused-aliased-model"`)
	if !(idxSonnet >= 0 && idxSonnet < idxGPT && idxGPT < idxUnknown && idxUnknown < idxUnused) {
		t.Fatalf("settings model rows out of order: sonnet=%d gpt=%d unknown=%d unused=%d", idxSonnet, idxGPT, idxUnknown, idxUnused)
	}

	// /tokedex?q= support & /settings back-link.
	tokRec := httptest.NewRecorder()
	testHandler(enabled).ServeHTTP(tokRec, httptest.NewRequest(http.MethodGet, "/tokedex?q=claude-sonnet-4", nil))
	if tokRec.Code != http.StatusOK {
		t.Fatalf("GET /tokedex = %d", tokRec.Code)
	}
	tokBody := tokRec.Body.String()
	for _, want := range []string{
		`href="/settings">Manage display aliases in Settings`,
		`data-search="Claude Sonnet 4 claude-sonnet-4"`,
		`idNode ? ' ' + idNode.textContent : ''`,
		`new URLSearchParams(window.location.search).get('q')`,
	} {
		if !strings.Contains(tokBody, want) {
			t.Fatalf("/tokedex missing %q", want)
		}
	}

	// Disabled Tokedex must emit zero /tokedex links on /settings.
	disabled, err := NewWithConfig(store, Config{IngestToken: "secret", DashboardToken: testDashboardToken, EnableTokedex: false})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = disabled.Close() }()
	disSetRec := httptest.NewRecorder()
	testHandler(disabled).ServeHTTP(disSetRec, httptest.NewRequest(http.MethodGet, "/settings", nil))
	if disSetRec.Code != http.StatusOK {
		t.Fatalf("disabled GET /settings = %d", disSetRec.Code)
	}
	if strings.Contains(disSetRec.Body.String(), `href="/tokedex`) {
		t.Fatal("disabled /settings still renders /tokedex cross-links")
	}
}

func TestM3CEvolutionETagTracksUnclassifiedTokens(t *testing.T) {
	storeA, err := database.Open(t.TempDir()+"/m3c-ev-a.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer storeA.Close()
	if _, err := storeA.Ingest(context.Background(), []usage.Event{{
		SchemaVersion: usage.SchemaVersion,
		EventID:       "ev-classified",
		Timestamp:     time.Now().UTC(),
		MachineID:     "box-a",
		Provider:      "openai",
		Model:         "gpt-5",
		Tool:          "codex",
		InputTokens:   usage.Int64(100),
		OutputTokens:  usage.Int64(0),
		TotalTokens:   usage.Int64(100),
		TokenAccuracy: usage.AccuracyReported,
		Source:        usage.Source{Adapter: "codex", AdapterVersion: "1"},
	}}); err != nil {
		t.Fatal(err)
	}

	storeB, err := database.Open(t.TempDir()+"/m3c-ev-b.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer storeB.Close()
	if _, err := storeB.Ingest(context.Background(), []usage.Event{{
		SchemaVersion: usage.SchemaVersion,
		EventID:       "ev-unclassified",
		Timestamp:     time.Now().UTC(),
		MachineID:     "box-a",
		Provider:      "openai",
		Model:         "gpt-5",
		Tool:          "codex",
		InputTokens:   usage.Int64(100),
		OutputTokens:  usage.Int64(0),
		TotalTokens:   usage.Int64(150),
		TokenAccuracy: usage.AccuracyReported,
		Source:        usage.Source{Adapter: "codex", AdapterVersion: "1"},
	}}); err != nil {
		t.Fatal(err)
	}

	srvA, err := newTestServer(storeA, "secret")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srvA.Close() }()
	srvB, err := newTestServer(storeB, "secret")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = srvB.Close() }()

	recA := httptest.NewRecorder()
	testHandler(srvA).ServeHTTP(recA, httptest.NewRequest(http.MethodGet, "/api/v1/evolution", nil))
	recB := httptest.NewRecorder()
	testHandler(srvB).ServeHTTP(recB, httptest.NewRequest(http.MethodGet, "/api/v1/evolution", nil))
	if recA.Code != http.StatusOK || recB.Code != http.StatusOK {
		t.Fatalf("evolution status codes = (%d, %d), want (200, 200)", recA.Code, recB.Code)
	}
	etagA := recA.Header().Get("ETag")
	etagB := recB.Header().Get("ETag")
	if etagA == "" || etagB == "" || etagA == etagB {
		t.Fatalf("evolution ETag failed to distinguish unclassified tokens: etagA=%q etagB=%q", etagA, etagB)
	}
	if !strings.HasSuffix(etagB, `-50"`) {
		t.Fatalf("evolution ETag with 50 unclassified tokens = %q, want -50 suffix", etagB)
	}
}
