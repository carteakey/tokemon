package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/tokemon/tokemon/internal/catalog"
	"github.com/tokemon/tokemon/internal/database"
	"github.com/tokemon/tokemon/internal/usage"
)

// A reader that arrives after a background overview refresh has installed a
// fresh entry must be served the pre-rendered page, not a request-path rebuild.
// The refresh publishes the entry and its page together, so the reader can
// never observe the new overview with the old page (the 756-934ms window in
// docs/analytics-latency.md). Closing the store proves the warm request did no
// database work at all: a Sessions or DisplayAliases call would fail.
//
// The same request also proves coherence: the page and the overview JSON are
// read from the same cached entry, so with the store closed both must still
// report the post-ingest total.
func TestDashboardAfterBackgroundRefreshNeedsNoDatabase(t *testing.T) {
	store, err := database.Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	server, err := newTestServer(store, "")
	if err != nil {
		t.Fatal(err)
	}

	ingest := func(eventID string, tokens int64) {
		t.Helper()
		payload, err := json.Marshal(batchRequest{Events: []usage.Event{dashboardEvent(eventID, "machine-a", tokens)}})
		if err != nil {
			t.Fatal(err)
		}
		request := httptest.NewRequest(http.MethodPost, "/api/v1/events/batch", bytes.NewReader(payload))
		request.Header.Set("Content-Type", "application/json")
		accepted := httptest.NewRecorder()
		testHandler(server).ServeHTTP(accepted, request)
		if accepted.Code != http.StatusOK {
			t.Fatalf("ingest status = %d: %s", accepted.Code, accepted.Body.String())
		}
	}
	fetch := func() *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		testHandler(server).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
		return response
	}

	ingest("prerender-race-a", 42)
	// Cold render, then the warm render that attaches the page to the entry.
	if response := fetch(); response.Code != http.StatusOK {
		t.Fatalf("cold dashboard status = %d, body %s", response.Code, response.Body.String())
	}
	if response := fetch(); response.Code != http.StatusOK {
		t.Fatalf("warm dashboard status = %d, body %s", response.Code, response.Body.String())
	}
	if fetched, html := cachedDashboardEntry(server); fetched.IsZero() || len(html) == 0 {
		t.Fatal("warm dashboard did not cache its page on the overview entry")
	}

	// The ingest invalidates the overview; the next reader is served the stale
	// page immediately while one background refresh runs.
	ingest("prerender-race-b", 500)
	stale := fetch()
	if stale.Code != http.StatusOK {
		t.Fatalf("stale dashboard status = %d, body %s", stale.Code, stale.Body.String())
	}
	if bytes.Contains(stale.Body.Bytes(), []byte("542")) {
		t.Fatal("stale dashboard already shows the post-ingest total; the refresh was not stale-while-revalidate")
	}

	// Wait for the refresh to publish the fresh entry together with its page,
	// and assert the invariant on every sample: a fresh entry is never visible
	// without its page. That is what closes the window — a reader can arrive at
	// any point and still find the bytes it needs.
	deadline := time.Now().Add(5 * time.Second)
	for {
		fetched, html := cachedDashboardEntry(server)
		if !fetched.IsZero() {
			if len(html) == 0 {
				t.Fatal("a fresh overview entry was published without its pre-rendered page")
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background refresh did not publish an entry with a pre-rendered page")
		}
		runtime.Gosched()
	}

	fetched, html := cachedDashboardEntry(server)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	warm := fetch()
	if warm.Code != http.StatusOK {
		t.Fatalf("dashboard after the refresh status = %d, body %s", warm.Code, warm.Body.String())
	}
	if !bytes.Equal(warm.Body.Bytes(), html) {
		t.Fatal("dashboard after the refresh did not serve the pre-rendered page; it rebuilt on the request path")
	}
	if !bytes.Contains(warm.Body.Bytes(), []byte("542")) {
		t.Fatal("pre-rendered page does not show the post-ingest total")
	}

	// The overview served beside the page must agree with it: both come from
	// the same entry, so the store being closed cannot change the answer.
	overview := httptest.NewRecorder()
	testHandler(server).ServeHTTP(overview, httptest.NewRequest(http.MethodGet, "/api/v1/analytics/overview", nil))
	if overview.Code != http.StatusOK {
		t.Fatalf("overview status = %d, body %s", overview.Code, overview.Body.String())
	}
	if !strings.Contains(overview.Body.String(), `"lifetime_tokens":542`) {
		t.Fatalf("served overview does not agree with the served page: %s", overview.Body.String())
	}
	if _, servedFetched, err := server.cachedOverview(context.Background()); err != nil || !servedFetched.Equal(fetched) {
		t.Fatalf("served overview fetched = %v (err %v), page fetched = %v", servedFetched, err, fetched)
	}
}

// A pre-render that fails publishes the overview entry without a page (see
// startOverviewRefreshLocked). The next reader must build the page on the
// request path rather than serve nothing or stale bytes, and that build is
// attached to the entry so the failure does not cost every later reader.
func TestDashboardServesRequestPathBuildWhenPreRenderMissing(t *testing.T) {
	store, err := database.Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server, err := newTestServer(store, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Ingest(context.Background(), []usage.Event{dashboardEvent("prerender-missing", "machine-a", 42)}); err != nil {
		t.Fatal(err)
	}

	fetch := func() *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		testHandler(server).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
		return response
	}
	if response := fetch(); response.Code != http.StatusOK {
		t.Fatalf("cold dashboard status = %d, body %s", response.Code, response.Body.String())
	}

	// The exact state a failed pre-render leaves: a published entry with no page.
	server.overviewMu.Lock()
	server.overviewCache.html = nil
	server.overviewMu.Unlock()

	response := fetch()
	if response.Code != http.StatusOK {
		t.Fatalf("dashboard with a missing pre-render status = %d, body %s", response.Code, response.Body.String())
	}
	if !bytes.Contains(response.Body.Bytes(), []byte("42")) {
		t.Fatal("request-path fallback did not render the overview data")
	}
	fetched, html := cachedDashboardEntry(server)
	if fetched.IsZero() || len(html) == 0 {
		t.Fatal("request-path fallback was not attached to the overview entry")
	}
	if !bytes.Equal(response.Body.Bytes(), html) {
		t.Fatal("attached fallback page differs from the response that built it")
	}
}
