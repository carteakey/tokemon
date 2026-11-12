package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/tokemon/tokemon/internal/catalog"
	"github.com/tokemon/tokemon/internal/database"
	"github.com/tokemon/tokemon/internal/usage"
)

func cacheBenchEvent(eventID, machine string, tokens int64) usage.Event {
	return usage.Event{
		SchemaVersion: usage.SchemaVersion,
		EventID:       eventID,
		Timestamp:     time.Now().UTC(),
		MachineID:     machine,
		Provider:      "provider",
		Model:         "model",
		Tool:          "tool",
		TotalTokens:   usage.Int64(tokens),
		TokenAccuracy: usage.AccuracyReported,
		Source:        usage.Source{Adapter: "test", AdapterVersion: "1"},
	}
}

// The analytics cache must serve one compute for repeat renders of the same
// filter and drop it as soon as an ingest writes, so a cached page never
// hides new data for longer than the ingest takes.
func TestAnalyticsCacheServesAndInvalidates(t *testing.T) {
	store, err := database.Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	if _, err := store.Ingest(context.Background(), []usage.Event{cacheBenchEvent("ev-a", "machine-a", 100)}); err != nil {
		t.Fatal(err)
	}
	server, err := newTestServer(store, "")
	if err != nil {
		t.Fatal(err)
	}

	fetch := func() string {
		response := httptest.NewRecorder()
		testHandler(server).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/analytics", nil))
		if response.Code != http.StatusOK {
			t.Fatalf("analytics status = %d, body %s", response.Code, response.Body.String())
		}
		return response.Body.String()
	}

	first := fetch()
	if second := fetch(); second != first {
		t.Fatal("two identical analytics requests produced different payloads; the cache is not serving one compute")
	}

	// Ingest through the HTTP path, like real agents do — that is the write
	// path the analytics cache invalidates on.
	payload, err := json.Marshal(batchRequest{Events: []usage.Event{cacheBenchEvent("ev-b", "machine-b", 500)}})
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
	// The invalidated cache serves the last good result immediately while one
	// background compute refreshes it.
	third := fetch()
	if third != first {
		t.Fatal("stale analytics payload changed immediately after ingest; stale-while-revalidate is not serving the cached result")
	}
	// A reader that lands while the background refresh is still running must
	// also be served the stale result instead of waiting on the recompute.
	if fourth := fetch(); fourth != first {
		t.Fatal("reader during the background refresh waited for or diverged from the stale result")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		server.analyticsMu.Lock()
		refreshed := false
		for _, entry := range server.analyticsCache {
			if !entry.fetched.IsZero() {
				refreshed = true
			}
		}
		server.analyticsMu.Unlock()
		if refreshed {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background analytics refresh did not complete")
		}
		time.Sleep(10 * time.Millisecond)
	}
	fresh := fetch()
	if fresh == first {
		t.Fatal("analytics payload did not change after the background refresh; the cache was not invalidated")
	}

	// A different filter is an independent cache entry, not a stale share.
	response := httptest.NewRecorder()
	testHandler(server).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/api/v1/analytics?period=all", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("analytics period=all status = %d", response.Code)
	}

	// The sessions page only needs facets now; it must still render.
	response = httptest.NewRecorder()
	testHandler(server).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/sessions", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("sessions status = %d", response.Code)
	}
}

// A warm analytics render must not need the database. After an ingest the
// background refresh can hold the store's single connection for a long query
// (the facets scans dominate), and a reader that still issues a query pays that
// refresh's latency — the 0.3-1s "warm" renders measured on the live hub. The
// page caches the display aliases so a warm render is pure template work.
func TestAnalyticsPageWarmRenderNeedsNoDatabase(t *testing.T) {
	store, err := database.Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Ingest(context.Background(), []usage.Event{cacheBenchEvent("ev-warm", "machine-a", 100)}); err != nil {
		t.Fatal(err)
	}
	server, err := newTestServer(store, "")
	if err != nil {
		t.Fatal(err)
	}

	render := func() *httptest.ResponseRecorder {
		response := httptest.NewRecorder()
		testHandler(server).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/analytics", nil))
		return response
	}
	if response := render(); response.Code != http.StatusOK {
		t.Fatalf("cold analytics status = %d, body %s", response.Code, response.Body.String())
	}
	// Closing the store makes any remaining query fail, so a 200 here proves
	// the warm render served the cached analytics result and the cached
	// aliases without waiting on the database connection.
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if response := render(); response.Code != http.StatusOK {
		t.Fatalf("warm analytics status = %d after the store closed, body %s", response.Code, response.Body.String())
	}
}
