package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/tokemon/tokemon/internal/catalog"
	"github.com/tokemon/tokemon/internal/database"
	"github.com/tokemon/tokemon/internal/quota"
	"github.com/tokemon/tokemon/internal/usage"
)

func warmupSeed(t *testing.T) (*database.Store, *Server) {
	t.Helper()
	store, err := database.Open(t.TempDir()+"/warmup.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	event := usage.Event{
		SchemaVersion: usage.SchemaVersion,
		EventID:       "warmup-event",
		Timestamp:     time.Now().UTC().Add(-time.Hour),
		MachineID:     "machine",
		Provider:      "provider",
		Model:         "model",
		Tool:          "tool",
		TotalTokens:   usage.Int64(120),
		TokenAccuracy: usage.AccuracyReported,
		Source:        usage.Source{Adapter: "test", AdapterVersion: "1"},
	}
	if _, err := store.Ingest(context.Background(), []usage.Event{event}); err != nil {
		t.Fatal(err)
	}
	server, err := newTestServer(store, "secret")
	if err != nil {
		t.Fatal(err)
	}
	return store, server
}

// WarmCaches must populate the overview entry, its pre-rendered dashboard
// HTML, and the default analytics window — with a canceled caller context,
// because the compute is detached from the caller by design.
func TestWarmCachesPopulatesCachesWithCanceledCaller(t *testing.T) {
	_, server := warmupSeed(t)

	if server.overviewCache != nil {
		t.Fatal("overview cache populated before the warm")
	}
	if _, html := cachedDashboardEntry(server); len(html) > 0 {
		t.Fatalf("dashboard pre-render present before the warm: %d bytes", len(html))
	}

	callerCtx, cancel := context.WithCancel(context.Background())
	cancel()
	server.WarmCaches(callerCtx)

	fetched, html := cachedDashboardEntry(server)
	if fetched.IsZero() {
		t.Fatal("warm left no fresh overview cache entry")
	}
	if len(html) == 0 {
		t.Fatal("warm did not build the pre-rendered dashboard HTML")
	}
	server.analyticsMu.Lock()
	_, analyticsCached := server.analyticsCache[analyticsCacheKey(database.AnalyticsQuery{Period: "30d", Dimension: "projects"})]
	server.analyticsMu.Unlock()
	if !analyticsCached {
		t.Fatal("warm did not populate the default analytics cache entry")
	}

	// The warmed dashboard must actually serve: an unauthenticated request is
	// outside the point, so authenticate like the dashboard does.
	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.SetBasicAuth("tokemon", testDashboardToken)
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("warmed dashboard status = %d: %s", recorder.Code, recorder.Body.String())
	}
	if !strings.Contains(recorder.Body.String(), "main-content") {
		t.Fatal("warmed dashboard response is not the rendered page")
	}
}

// An empty database must warm cleanly: the zero-state overview, pre-render,
// and analytics entry all build without events.
func TestWarmCachesHandlesEmptyDatabase(t *testing.T) {
	store, err := database.Open(t.TempDir()+"/empty.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server, err := NewWithConfig(store, Config{IngestToken: "secret", DashboardToken: "dashboard"})
	if err != nil {
		t.Fatal(err)
	}
	server.WarmCaches(context.Background())

	request := httptest.NewRequest(http.MethodGet, "/", nil)
	request.SetBasicAuth("tokemon", "dashboard")
	recorder := httptest.NewRecorder()
	server.Handler().ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("empty warm dashboard status = %d: %s", recorder.Code, recorder.Body.String())
	}
}

func TestWarmCachesDoesNotPoisonPostIngestReads(t *testing.T) {
	budget := int64(10_000)
	store, err := database.OpenWithLocation(
		t.TempDir()+"/warm-ingest.db",
		catalog.Empty(),
		time.UTC,
		quota.Quota{Provider: "openai", Label: "OpenAI", ResetDay: 1, Budget: quota.Budget{TokensPerMonth: &budget}},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	server, err := newTestServer(store, "secret")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = server.Close() }()

	// Warm caches while the database is still empty (including dashboard HTML -> quotaCache).
	server.WarmCaches(context.Background())

	ingest := func(eventID string, tokens int64) {
		t.Helper()
		payload, err := json.Marshal(batchRequest{Events: []usage.Event{{
			SchemaVersion: usage.SchemaVersion,
			EventID:       eventID,
			Timestamp:     time.Now().UTC(),
			MachineID:     "m-warm",
			Provider:      "openai",
			Model:         "gpt-5.4",
			Tool:          "codex",
			TotalTokens:   usage.Int64(tokens),
			TokenAccuracy: usage.AccuracyReported,
			Source:        usage.Source{Adapter: "codex", AdapterVersion: "test"},
		}}})
		if err != nil {
			t.Fatal(err)
		}
		req := httptest.NewRequest(http.MethodPost, "/api/v1/events/batch", bytes.NewReader(payload))
		req.Header.Set("Authorization", "Bearer secret")
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		testHandler(server).ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("ingest %s status = %d: %s", eventID, rec.Code, rec.Body.String())
		}
	}

	ingest("ev-warm-1", 500)

	// Unserved warmup entries must be dropped on ingest, so immediate single-shot
	// reads observe 500 tokens rather than stale-zero warmup snapshots.
	evoRec := httptest.NewRecorder()
	testHandler(server).ServeHTTP(evoRec, httptest.NewRequest(http.MethodGet, "/api/v1/evolution", nil))
	if evoRec.Code != http.StatusOK || !strings.Contains(evoRec.Body.String(), `"lifetime_tokens":500`) {
		t.Fatalf("post-warmup /api/v1/evolution = %d %s", evoRec.Code, evoRec.Body.String())
	}

	quotaRec := httptest.NewRecorder()
	testHandler(server).ServeHTTP(quotaRec, httptest.NewRequest(http.MethodGet, "/api/v1/quotas", nil))
	if quotaRec.Code != http.StatusOK || !strings.Contains(quotaRec.Body.String(), `"tokens_used":500`) {
		t.Fatalf("post-warmup /api/v1/quotas = %d %s", quotaRec.Code, quotaRec.Body.String())
	}

	settingsRec := httptest.NewRecorder()
	testHandler(server).ServeHTTP(settingsRec, httptest.NewRequest(http.MethodGet, "/settings", nil))
	if settingsRec.Code != http.StatusOK || !strings.Contains(settingsRec.Body.String(), "gpt-5.4") {
		t.Fatalf("post-warmup /settings missing gpt-5.4: %d", settingsRec.Code)
	}

	// Even after overviewCache has been served to a client, ?fresh=1 and /settings
	// must reflect a subsequent ingest immediately without stale data.
	ingest("ev-warm-2", 250)

	freshRec := httptest.NewRecorder()
	testHandler(server).ServeHTTP(freshRec, httptest.NewRequest(http.MethodGet, "/api/v1/analytics/overview?fresh=1", nil))
	if freshRec.Code != http.StatusOK || !strings.Contains(freshRec.Body.String(), `"lifetime_tokens":750`) {
		t.Fatalf("/api/v1/analytics/overview?fresh=1 = %d %s", freshRec.Code, freshRec.Body.String())
	}
}
