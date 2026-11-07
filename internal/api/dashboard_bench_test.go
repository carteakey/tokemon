package api

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/tokemon/tokemon/internal/catalog"
	"github.com/tokemon/tokemon/internal/database"
)

// benchStore opens the production-shaped database named by
// TOKEMON_BENCH_DB (a read-only copy of a real hub database). Without the
// variable the benchmarks skip so the regular test suite stays fast and
// hermetic.
func benchStore(b *testing.B) *database.Store {
	path := os.Getenv("TOKEMON_BENCH_DB")
	if path == "" {
		b.Skip("TOKEMON_BENCH_DB not set; point it at a production-shaped copy to run")
	}
	store, err := database.Open(path, catalog.Empty())
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = store.Close() })
	return store
}

// BenchmarkDashboardHandler measures the full "/" request path: overview
// (a cache hit after the first iteration), machine health, sessions, and the
// template render — the steady-state cost every warm page load pays.
func BenchmarkDashboardHandler(b *testing.B) {
	store := benchStore(b)
	server, err := NewWithConfig(store, Config{DashboardToken: testDashboardToken, AllowLoopbackDev: true})
	if err != nil {
		b.Fatal(err)
	}
	handler := testHandler(server)
	warm := httptest.NewRecorder()
	handler.ServeHTTP(warm, httptest.NewRequest(http.MethodGet, "/", nil))
	if warm.Code != http.StatusOK {
		b.Fatalf("dashboard status = %d", warm.Code)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/", nil))
		if recorder.Code != http.StatusOK {
			b.Fatalf("dashboard status = %d", recorder.Code)
		}
	}
}

// BenchmarkDashboardRender isolates the template render with production-shaped
// page data, so render regressions can be pinned separately from the queries.
func BenchmarkDashboardRender(b *testing.B) {
	store := benchStore(b)
	server, err := NewWithConfig(store, Config{DashboardToken: testDashboardToken, AllowLoopbackDev: true})
	if err != nil {
		b.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	overview, err := store.Overview(ctx)
	if err != nil {
		b.Fatal(err)
	}
	aliases, err := store.DisplayAliases(ctx)
	if err != nil {
		b.Fatal(err)
	}
	modelAliases, machineAliases := aliasMaps(aliases)
	machines, err := store.MachinesAt(ctx, time.Now())
	if err != nil {
		b.Fatal(err)
	}
	sessions, err := store.Sessions(ctx, database.AnalyticsQuery{Period: "30d", Dimension: "projects", Now: time.Now().UTC()})
	if err != nil {
		b.Fatal(err)
	}
	if len(sessions) > 8 {
		sessions = sessions[:8]
	}
	page := dashboardPage{Overview: overview, ModelAliases: modelAliases, MachineAliases: machineAliases, Machines: machines, Sessions: sessions, Freshness: dataFreshnessFor(overview.NewestEvent, time.Now())}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if err := server.template.ExecuteTemplate(io.Discard, "dashboard", page); err != nil {
			b.Fatal(err)
		}
	}
}

// BenchmarkDashboardStages times each store call the "/" handler makes, so a
// regression can be pinned to one stage instead of the whole request.
func BenchmarkDashboardStages(b *testing.B) {
	store := benchStore(b)
	server, err := NewWithConfig(store, Config{DashboardToken: testDashboardToken, AllowLoopbackDev: true})
	if err != nil {
		b.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()
	overview, err := store.Overview(ctx)
	if err != nil {
		b.Fatal(err)
	}
	b.Run("overview-cache-hit", func(b *testing.B) {
		handler := testHandler(server)
		warm := httptest.NewRecorder()
		handler.ServeHTTP(warm, httptest.NewRequest(http.MethodGet, "/", nil))
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			recorder := httptest.NewRecorder()
			handler.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/v1/overview", nil))
			if recorder.Code != http.StatusOK {
				b.Fatalf("overview status = %d", recorder.Code)
			}
		}
	})
	b.Run("display-aliases", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, err := store.DisplayAliases(ctx); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("machines-at", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, err := store.MachinesAt(ctx, time.Now()); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("sessions-30d", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, err := store.Sessions(ctx, database.AnalyticsQuery{Period: "30d", Dimension: "projects", Now: time.Now().UTC()}); err != nil {
				b.Fatal(err)
			}
		}
	})
	_ = overview
}
