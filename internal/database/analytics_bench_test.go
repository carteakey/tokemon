package database

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/tokemon/tokemon/internal/catalog"
)

// benchStore opens the production-shaped database named by
// TOKEMON_BENCH_DB (a read-only copy of a real hub database). Without the
// variable the benchmarks skip so the regular test suite stays fast and
// hermetic.
func benchStore(b *testing.B) *Store {
	path := os.Getenv("TOKEMON_BENCH_DB")
	if path == "" {
		b.Skip("TOKEMON_BENCH_DB not set; point it at a production-shaped copy to run")
	}
	store, err := Open(path, catalog.Empty())
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = store.Close() })
	return store
}

func benchContext(b *testing.B) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.Background(), 5*time.Minute)
}

func BenchmarkAnalytics(b *testing.B) {
	store := benchStore(b)
	ctx, cancel := benchContext(b)
	defer cancel()
	for _, period := range []string{"30d", "all"} {
		b.Run("period="+period, func(b *testing.B) {
			query := AnalyticsQuery{Period: period, Dimension: "providers", Now: time.Now().UTC()}
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := store.Analytics(ctx, query); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkAnalyticsStages times the individual queries inside Store.Analytics
// so a regression can be pinned to one stage instead of the whole call.
func BenchmarkAnalyticsStages(b *testing.B) {
	store := benchStore(b)
	ctx, cancel := benchContext(b)
	defer cancel()
	query := AnalyticsQuery{Period: "30d", Dimension: "providers", Now: time.Now().UTC()}
	normalized, start, end := store.normalizeAnalyticsQuery(query)
	where, args := analyticsWhere(normalized, start, end)
	db := store.db
	b.Run("summary", func(b *testing.B) {
		var summary AnalyticsSummary
		for i := 0; i < b.N; i++ {
			if err := store.analyticsSummary(ctx, where, args, &summary); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("summary-sql-side", func(b *testing.B) {
		// SQL-side sketch of what analyticsSummary must produce: token and
		// event totals without pulling rows into Go. Kept here to compare
		// against the Go-side stage before committing to a rewrite.
		for i := 0; i < b.N; i++ {
			var tokens, events, unknown sql.NullInt64
			if err := db.QueryRowContext(ctx, `SELECT COALESCE(SUM(total_tokens),0), COUNT(*),
COUNT(*) FILTER (WHERE total_tokens IS NULL) FROM usage_events WHERE `+where, args...).
				Scan(&tokens, &events, &unknown); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("points", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, err := store.analyticsPoints(ctx, normalized, where, args); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("breakdown", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, err := store.analyticsBreakdown(ctx, normalized, where, args, 1); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("share-timeline", func(b *testing.B) {
		breakdown, err := store.analyticsBreakdown(ctx, normalized, where, args, 1)
		if err != nil {
			b.Fatal(err)
		}
		b.ResetTimer()
		for i := 0; i < b.N; i++ {
			if _, _, err := store.analyticsShareTimeline(ctx, normalized, where, args, start, end, breakdown, 1); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("sessions", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, err := store.analyticsSessions(ctx, where, args); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("facets", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, err := store.analyticsFacets(ctx); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("lifetime-tokens", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, err := store.LifetimeTokens(ctx); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("sessions-with-sort", func(b *testing.B) {
		for i := 0; i < b.N; i++ {
			if _, err := store.SessionsWithSort(ctx, query, "when", "desc"); err != nil {
				b.Fatal(err)
			}
		}
	})
}
