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
	third := fetch()
	if third == first {
		t.Fatal("analytics payload did not change after ingest; the cache was not invalidated")
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
