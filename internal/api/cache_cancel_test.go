package api

import (
	"context"
	"testing"

	"github.com/tokemon/tokemon/internal/catalog"
	"github.com/tokemon/tokemon/internal/database"
)

// A client that disconnects or reloads cancels its request context. The shared
// cache computes must survive that: they run detached from the triggering
// request, so the compute still completes and other waiters get a result
// instead of "context canceled".
func TestCachedComputesSurviveCanceledTriggeringRequest(t *testing.T) {
	store, err := database.Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	server, err := newTestServer(store, "")
	if err != nil {
		t.Fatal(err)
	}

	canceled, cancel := context.WithCancel(context.Background())
	cancel()

	if _, _, err := server.cachedOverview(canceled); err != nil {
		t.Fatalf("cachedOverview with a canceled triggering request = %v, want success", err)
	}
	if _, err := server.cachedEvolution(canceled); err != nil {
		t.Fatalf("cachedEvolution with a canceled triggering request = %v, want success", err)
	}
	if _, err := server.cachedAnalytics(canceled, database.AnalyticsQuery{Period: "30d"}); err != nil {
		t.Fatalf("cachedAnalytics with a canceled triggering request = %v, want success", err)
	}
}

// clientGone must classify the two context endings and nothing else, so
// handlers only suppress the 5xx for a request that no longer has a reader.
func TestClientGoneClassification(t *testing.T) {
	if !clientGone(context.Canceled) {
		t.Fatal("context.Canceled must be treated as client gone")
	}
	if !clientGone(context.DeadlineExceeded) {
		t.Fatal("context.DeadlineExceeded must be treated as client gone")
	}
	if clientGone(nil) {
		t.Fatal("nil must not be treated as client gone")
	}
}
