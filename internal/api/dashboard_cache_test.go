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

func dashboardEvent(eventID, machine string, tokens int64) usage.Event {
	return usage.Event{
		SchemaVersion: usage.SchemaVersion,
		EventID:       eventID,
		Timestamp:     time.Now().UTC(),
		MachineID:     machine,
		Provider:      "provider",
		Model:         "model",
		Tool:          "codex",
		TotalTokens:   usage.Int64(tokens),
		TokenAccuracy: usage.AccuracyReported,
		Source:        usage.Source{Adapter: "codex", AdapterVersion: "test"},
	}
}

// cachedDashboardEntry snapshots the served overview entry's fetched time and
// pre-rendered page. A zero fetched time means the entry is stale (an ingest
// invalidated it) and the page, if any, is the stale one.
func cachedDashboardEntry(server *Server) (time.Time, []byte) {
	server.overviewMu.Lock()
	defer server.overviewMu.Unlock()
	if server.overviewCache == nil {
		return time.Time{}, nil
	}
	return server.overviewCache.fetched, server.overviewCache.html
}

// The dashboard page is rendered once per overview entry and served as cached
// bytes: repeat requests must be byte-identical, an ingest must not force a
// rebuild (the stale page is served like the stale overview), and the next
// background refresh must install a page that shows the new data.
func TestDashboardServesCachedPageAndTracksRefresh(t *testing.T) {
	store, err := database.Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server, err := newTestServer(store, "")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Ingest(context.Background(), []usage.Event{dashboardEvent("dash-cache-a", "machine-a", 42)}); err != nil {
		t.Fatal(err)
	}

	fetch := func() string {
		response := httptest.NewRecorder()
		testHandler(server).ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))
		if response.Code != http.StatusOK {
			t.Fatalf("dashboard status = %d, body %s", response.Code, response.Body.String())
		}
		return response.Body.String()
	}

	first := fetch()
	if !bytes.Contains([]byte(first), []byte("42")) {
		t.Fatal("first render does not show the ingested total")
	}
	if second := fetch(); second != first {
		t.Fatal("two identical dashboard requests produced different pages; the HTML cache is not serving one render")
	}

	// Ingest through the HTTP path — the write path the caches invalidate on.
	payload, err := json.Marshal(batchRequest{Events: []usage.Event{dashboardEvent("dash-cache-b", "machine-b", 500)}})
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

	// The invalidated page is served immediately instead of rebuilding.
	stale := fetch()

	// The background refresh pre-renders the page for the fresh overview and
	// publishes the entry and its page together.
	deadline := time.Now().Add(5 * time.Second)
	for {
		fetched, html := cachedDashboardEntry(server)
		if !fetched.IsZero() && len(html) > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("background dashboard pre-render did not complete")
		}
		time.Sleep(10 * time.Millisecond)
	}
	fresh := fetch()
	if !bytes.Contains([]byte(fresh), []byte("542")) {
		t.Fatal("refreshed page does not show the new total")
	}
	if fresh == stale {
		t.Fatal("refreshed page is identical to the stale page; the pre-render did not pick up the ingest")
	}
}
