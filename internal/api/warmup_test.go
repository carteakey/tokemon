package api

import (
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
	if html, ok := server.cachedDashboardHTML(time.Time{}); ok {
		t.Fatalf("dashboard pre-render present before the warm: %d bytes", len(html))
	}

	callerCtx, cancel := context.WithCancel(context.Background())
	cancel()
	server.WarmCaches(callerCtx)

	server.overviewMu.Lock()
	cached := server.overviewCache
	fetched := time.Time{}
	if cached != nil {
		fetched = cached.fetched
	}
	server.overviewMu.Unlock()
	if cached == nil || fetched.IsZero() {
		t.Fatal("warm left no fresh overview cache entry")
	}
	if _, ok := server.cachedDashboardHTML(fetched); !ok {
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
