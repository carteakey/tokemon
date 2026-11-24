package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/tokemon/tokemon/internal/catalog"
	"github.com/tokemon/tokemon/internal/usage"
)

func TestAnalyticsQueryFromToDSTOffsetsSubsecAndSearch(t *testing.T) {
	ny, err := time.LoadLocation("America/New_York")
	if err != nil {
		t.Fatal(err)
	}
	store, err := OpenWithLocation(filepath.Join(t.TempDir(), "tokemon.db"), catalog.Empty(), ny)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()

	// 2026-11-01 in America/New_York is fall-back DST (04:00Z to 05:00Z next day = 25h).
	// 2026-03-08 in America/New_York is spring-forward DST (05:00Z to 04:00Z next day = 23h).
	events := []usage.Event{
		{
			SchemaVersion: usage.SchemaVersion, EventID: "fb-before",
			Timestamp: time.Date(2026, 11, 1, 3, 59, 59, 900_000_000, time.UTC), // Oct 31 23:59:59.9 EDT
			MachineID: "m1", Project: "alpha_one", Provider: "openai", Model: "gpt-5", Tool: "codex",
			SessionID: "sess-fb-before", TotalTokens: usage.Int64(10), TokenAccuracy: usage.AccuracyReported,
			Source: usage.Source{Adapter: "codex", AdapterVersion: "1"},
		},
		{
			SchemaVersion: usage.SchemaVersion, EventID: "fb-start",
			Timestamp: time.Date(2026, 11, 1, 4, 0, 0, 123_000_000, time.UTC), // Nov 1 00:00:00.123 EDT
			MachineID: "m1", Project: "alpha_one", Provider: "openai", Model: "gpt-5", Tool: "codex",
			SessionID: "sess-fb-100%", TotalTokens: usage.Int64(20), TokenAccuracy: usage.AccuracyReported,
			Source: usage.Source{Adapter: "codex", AdapterVersion: "1"},
		},
		{
			SchemaVersion: usage.SchemaVersion, EventID: "fb-end",
			Timestamp: time.Date(2026, 11, 2, 4, 59, 59, 500_000_000, time.UTC), // Nov 1 23:59:59.5 EST
			MachineID: "m1", Project: "beta-two", Provider: "openai", Model: "gpt-5", Tool: "codex",
			SessionID: "sess-fb-end", TotalTokens: usage.Int64(30), TokenAccuracy: usage.AccuracyReported,
			Source: usage.Source{Adapter: "codex", AdapterVersion: "1"},
		},
		{
			SchemaVersion: usage.SchemaVersion, EventID: "fb-after",
			Timestamp: time.Date(2026, 11, 2, 5, 0, 0, 123_000_000, time.UTC), // Nov 2 00:00:00.123 EST
			MachineID: "m1", Project: "beta-two", Provider: "openai", Model: "gpt-5", Tool: "codex",
			SessionID: "sess-fb-after", TotalTokens: usage.Int64(40), TokenAccuracy: usage.AccuracyReported,
			Source: usage.Source{Adapter: "codex", AdapterVersion: "1"},
		},
		{
			SchemaVersion: usage.SchemaVersion, EventID: "sf-in",
			Timestamp: time.Date(2026, 3, 9, 3, 30, 0, 0, time.UTC), // Mar 8 23:30 EDT (spring-forward day)
			MachineID: "m1", Project: "alphaXone", Provider: "openai", Model: "gpt-5", Tool: "codex",
			SessionID: "sess-sf-in", TotalTokens: usage.Int64(50), TokenAccuracy: usage.AccuracyReported,
			Source: usage.Source{Adapter: "codex", AdapterVersion: "1"},
		},
		{
			SchemaVersion: usage.SchemaVersion, EventID: "sf-after",
			Timestamp: time.Date(2026, 3, 9, 4, 0, 0, 100_000_000, time.UTC), // Mar 9 00:00:00.1 EDT
			MachineID: "m1", Project: "alphaXone", Provider: "openai", Model: "gpt-5", Tool: "codex",
			SessionID: "sess-sf-after", TotalTokens: usage.Int64(60), TokenAccuracy: usage.AccuracyReported,
			Source: usage.Source{Adapter: "codex", AdapterVersion: "1"},
		},
		{
			SchemaVersion: usage.SchemaVersion, EventID: "offset-row",
			Timestamp: time.Date(2026, 11, 1, 15, 0, 0, 0, time.UTC),
			MachineID: "m1", Project: "offset-proj", Provider: "openai", Model: "gpt-5", Tool: "codex",
			SessionID: "sess-offset", TotalTokens: usage.Int64(70), TokenAccuracy: usage.AccuracyReported,
			Source: usage.Source{Adapter: "codex", AdapterVersion: "1"},
		},
	}
	if _, err := store.Ingest(ctx, events); err != nil {
		t.Fatal(err)
	}
	// Rewrite offset-row to a non-Z RFC3339 offset whose local NY date is Nov 1 (2026-11-02T01:30:00+02:00 = 2026-11-01T23:30:00Z = 18:30 EST).
	if _, err := store.db.Exec(`UPDATE usage_events SET timestamp = '2026-11-02T01:30:00+02:00' WHERE event_id = 'offset-row'`); err != nil {
		t.Fatal(err)
	}

	// Single fall-back day 2026-11-01 includes fb-start (20) + fb-end (30) + offset-row (70) = 120.
	nov1, err := store.Analytics(ctx, AnalyticsQuery{
		Period: "24h", // from/to overrides bucket to day
		From:   "2026-11-01",
		To:     "2026-11-01",
		Now:    time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if nov1.Bucket != "day" || nov1.Summary.Tokens != 120 || nov1.Summary.Events != 3 {
		t.Fatalf("nov1 analytics = bucket=%s tokens=%d events=%d, want day/120/3", nov1.Bucket, nov1.Summary.Tokens, nov1.Summary.Events)
	}

	// Spring-forward day 2026-03-08 includes only sf-in (50), not sf-after (.100Z on Mar 9).
	mar8, err := store.Analytics(ctx, AnalyticsQuery{
		From: "2026-03-08",
		To:   "2026-03-08",
		Now:  time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if mar8.Summary.Tokens != 50 || mar8.Summary.Events != 1 {
		t.Fatalf("mar8 analytics = tokens=%d events=%d, want 50/1", mar8.Summary.Tokens, mar8.Summary.Events)
	}

	// Inverted range (From > To) returns 0 rows and leaves StartDate/EndDate empty while preserving Filter.
	inverted, err := store.Analytics(ctx, AnalyticsQuery{From: "2026-11-05", To: "2026-11-01"})
	if err != nil {
		t.Fatal(err)
	}
	if inverted.Summary.Events != 0 || len(inverted.Points) != 0 {
		t.Fatalf("inverted range returned events=%d points=%d, want 0", inverted.Summary.Events, len(inverted.Points))
	}
	if inverted.StartDate != "" || inverted.EndDate != "" || inverted.Filter.From != "2026-11-05" || inverted.Filter.To != "2026-11-01" {
		t.Fatalf("inverted range dates = (%q, %q) filter=(%q, %q), want empty dates with preserved filter", inverted.StartDate, inverted.EndDate, inverted.Filter.From, inverted.Filter.To)
	}

	// Invalid date returns 0 rows without error and leaves StartDate/EndDate empty.
	invalid, err := store.Analytics(ctx, AnalyticsQuery{From: "2026-02-30", To: "2026-03-01"})
	if err != nil {
		t.Fatal(err)
	}
	if invalid.Summary.Events != 0 {
		t.Fatalf("invalid date returned events=%d, want 0", invalid.Summary.Events)
	}
	if invalid.StartDate != "" || invalid.EndDate != "" || invalid.Filter.From != "2026-02-30" || invalid.Filter.To != "2026-03-01" {
		t.Fatalf("invalid range dates = (%q, %q) filter=(%q, %q), want empty dates with preserved filter", invalid.StartDate, invalid.EndDate, invalid.Filter.From, invalid.Filter.To)
	}

	// Project filter and LIKE escaping for %, _, \.
	projFiltered, err := store.SessionsPage(ctx, AnalyticsQuery{Period: "all", Project: "alpha_one", Now: time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)}, "when", "desc", 1, 50)
	if err != nil || projFiltered.Total != 2 {
		t.Fatalf("project filter = %+v, err=%v", projFiltered, err)
	}
	likePercent, err := store.SessionsPage(ctx, AnalyticsQuery{Period: "all", Search: "%", Now: time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)}, "when", "desc", 1, 50)
	if err != nil || likePercent.Total != 1 || likePercent.Rows[0].SessionID != "sess-fb-100%" {
		t.Fatalf("search %% escaping = %+v, err=%v", likePercent, err)
	}
	likeUnderscore, err := store.SessionsPage(ctx, AnalyticsQuery{Period: "all", Search: "ALPHA_ONE", Now: time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)}, "when", "desc", 1, 50)
	if err != nil || likeUnderscore.Total != 2 {
		t.Fatalf("case-insensitive search _ escaping (must not match alphaXone) = %+v, err=%v", likeUnderscore, err)
	}
	likeSlash, err := store.SessionsPage(ctx, AnalyticsQuery{Period: "all", Search: `\`, Now: time.Date(2026, 11, 10, 0, 0, 0, 0, time.UTC)}, "when", "desc", 1, 50)
	if err != nil || likeSlash.Total != 0 {
		t.Fatalf("search \\ escaping = %+v, err=%v", likeSlash, err)
	}

	// analyticsInsightURL preserves from, to, project, q.
	u := analyticsInsightURL(AnalyticsQuery{Period: "30d", Dimension: "projects", From: "2026-11-01", To: "2026-11-02", Project: "alpha_one", Search: "fb"})
	for _, want := range []string{"from=2026-11-01", "to=2026-11-02", "project=alpha_one", "q=fb"} {
		if !strings.Contains(u, want) {
			t.Fatalf("analyticsInsightURL = %q, missing %q", u, want)
		}
	}

	// Asia/Kolkata (+05:30) fractional offset day boundary.
	ist, err := time.LoadLocation("Asia/Kolkata")
	if err != nil {
		t.Fatal(err)
	}
	istStore, err := OpenWithLocation(filepath.Join(t.TempDir(), "tokemon-ist.db"), catalog.Empty(), ist)
	if err != nil {
		t.Fatal(err)
	}
	defer istStore.Close()
	if _, err := istStore.Ingest(ctx, []usage.Event{
		{
			SchemaVersion: usage.SchemaVersion, EventID: "ist-before",
			Timestamp: time.Date(2026, 11, 1, 18, 29, 0, 0, time.UTC), // Nov 1 23:59 IST
			MachineID: "m1", Provider: "openai", Model: "gpt-5", Tool: "codex",
			TotalTokens: usage.Int64(11), TokenAccuracy: usage.AccuracyReported,
			Source: usage.Source{Adapter: "codex", AdapterVersion: "1"},
		},
		{
			SchemaVersion: usage.SchemaVersion, EventID: "ist-next",
			Timestamp: time.Date(2026, 11, 1, 18, 31, 0, 0, time.UTC), // Nov 2 00:01 IST
			MachineID: "m1", Provider: "openai", Model: "gpt-5", Tool: "codex",
			TotalTokens: usage.Int64(22), TokenAccuracy: usage.AccuracyReported,
			Source: usage.Source{Adapter: "codex", AdapterVersion: "1"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	istNov1, err := istStore.Analytics(ctx, AnalyticsQuery{From: "2026-11-01", To: "2026-11-01"})
	if err != nil || istNov1.Summary.Tokens != 11 {
		t.Fatalf("IST Nov 1 tokens = %d, err=%v (want 11)", istNov1.Summary.Tokens, err)
	}
}

func TestSessionsPageAndExportBeyondLimit100(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "tokemon.db"), catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()

	base := time.Now().UTC().Add(-2 * time.Hour)
	events := make([]usage.Event, 0, 125)
	for i := 0; i < 125; i++ {
		events = append(events, usage.Event{
			SchemaVersion: usage.SchemaVersion,
			EventID:       fmt.Sprintf("pg-%03d", i),
			Timestamp:     base.Add(time.Duration(i) * time.Second),
			MachineID:     "box",
			Project:       "pag-proj",
			Provider:      "openai",
			Model:         "gpt-5",
			Tool:          "codex",
			SessionID:     fmt.Sprintf("sess-pg-%03d", i),
			TotalTokens:   usage.Int64(int64(100 + i)),
			TokenAccuracy: usage.AccuracyReported,
			Source:        usage.Source{Adapter: "codex", AdapterVersion: "1"},
		})
	}
	if _, err := store.Ingest(ctx, events); err != nil {
		t.Fatal(err)
	}

	page1, err := store.SessionsPage(ctx, AnalyticsQuery{Period: "30d"}, "when", "desc", 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	if page1.Total != 125 || page1.Page != 1 || page1.PageSize != DefaultSessionsPageSize || page1.TotalPages != 2 || page1.HasPrev || !page1.HasNext || len(page1.Rows) != 100 {
		t.Fatalf("page1 = total=%d page=%d size=%d pages=%d prev=%v next=%v rows=%d",
			page1.Total, page1.Page, page1.PageSize, page1.TotalPages, page1.HasPrev, page1.HasNext, len(page1.Rows))
	}

	page2, err := store.SessionsPage(ctx, AnalyticsQuery{Period: "30d"}, "when", "desc", 99, 100)
	if err != nil {
		t.Fatal(err)
	}
	if page2.Page != 2 || !page2.HasPrev || page2.HasNext || len(page2.Rows) != 25 || page2.Rows[0].SessionID != "sess-pg-024" {
		t.Fatalf("clamped page2 = page=%d prev=%v next=%v rows=%d first=%v", page2.Page, page2.HasPrev, page2.HasNext, len(page2.Rows), page2.Rows)
	}

	clampedSize, err := store.SessionsPage(ctx, AnalyticsQuery{Period: "30d"}, "tokens", "asc", 1, 999)
	if err != nil || clampedSize.PageSize != MaxSessionsPageSize || len(clampedSize.Rows) != 125 {
		t.Fatalf("clampedSize = size=%d rows=%d err=%v", clampedSize.PageSize, len(clampedSize.Rows), err)
	}

	exported, err := store.ExportSessions(ctx, AnalyticsQuery{Period: "30d", Project: "pag-proj"}, "when", "desc")
	if err != nil || len(exported) != 125 {
		t.Fatalf("ExportSessions len = %d, err=%v", len(exported), err)
	}

	// Preserve existing SessionsWithLimit contracts.
	top8, err := store.SessionsWithLimit(ctx, AnalyticsQuery{Period: "30d"}, "when", "desc", 8)
	if err != nil || len(top8) != 8 {
		t.Fatalf("SessionsWithLimit(8) = %d, err=%v", len(top8), err)
	}
	def100, err := store.SessionsWithLimit(ctx, AnalyticsQuery{Period: "30d"}, "when", "desc", 0)
	if err != nil || len(def100) != 100 {
		t.Fatalf("SessionsWithLimit(0) = %d, err=%v", len(def100), err)
	}
}

func TestSessionDetailMetadataSwitchesUnknownsAndTimelineBound(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "tokemon.db"), catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	ctx := context.Background()

	// Schema version matches databaseSchemaVersion and idx_usage_events_session_id exists.
	ver, err := schemaVersion(ctx, store.db)
	if err != nil || ver != databaseSchemaVersion {
		t.Fatalf("schemaVersion = %d, err=%v, want %d", ver, err, databaseSchemaVersion)
	}
	var idxCount int
	if err := store.readHandle().QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_usage_events_session_id'`).Scan(&idxCount); err != nil || idxCount != 1 {
		t.Fatalf("idx_usage_events_session_id count = %d, err=%v", idxCount, err)
	}

	if _, err := store.SessionDetail(ctx, ""); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("empty session err = %v, want sql.ErrNoRows", err)
	}
	if _, err := store.SessionDetail(ctx, "nonexistent"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing session err = %v, want sql.ErrNoRows", err)
	}

	t0 := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
	cost1 := 0.12
	cost2 := 0.08
	if _, err := store.Ingest(ctx, []usage.Event{
		{
			SchemaVersion: usage.SchemaVersion, EventID: "det-1", Timestamp: t0,
			MachineID: "m1", Project: "proj-x", Provider: "openai", Model: "gpt-5", Tool: "codex",
			SessionID: "sess-multi", InputTokens: usage.Int64(100), OutputTokens: usage.Int64(50),
			CacheReadTokens: usage.Int64(100), CacheWriteTokens: usage.Int64(0), ReasoningTokens: usage.Int64(10),
			TotalTokens: usage.Int64(260), Cost: &cost1, DurationMS: usage.Int64(1200),
			TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "codex", AdapterVersion: "1"},
		},
		{
			SchemaVersion: usage.SchemaVersion, EventID: "det-2", Timestamp: t0.Add(45 * time.Second),
			MachineID: "m1", Project: "proj-x", Provider: "anthropic", Model: "claude-sonnet-4", Tool: "codex",
			SessionID: "sess-multi", InputTokens: usage.Int64(200), OutputTokens: usage.Int64(40),
			CacheReadTokens: usage.Int64(100), CacheWriteTokens: usage.Int64(20), ReasoningTokens: nil,
			TotalTokens: usage.Int64(360), Cost: &cost2, DurationMS: nil,
			TokenAccuracy: usage.AccuracyEstimated, Source: usage.Source{Adapter: "codex", AdapterVersion: "1"},
		},
		{
			SchemaVersion: usage.SchemaVersion, EventID: "det-3", Timestamp: t0.Add(90 * time.Second),
			MachineID: "m2", Project: "", Provider: "openai", Model: "gpt-5", Tool: "codex",
			SessionID: "sess-multi", InputTokens: usage.Int64(50), OutputTokens: usage.Int64(10),
			CacheReadTokens: usage.Int64(50), CacheWriteTokens: usage.Int64(0), ReasoningTokens: usage.Int64(0),
			TotalTokens: usage.Int64(110), Cost: nil, DurationMS: usage.Int64(800),
			TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "codex", AdapterVersion: "1"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	det, err := store.SessionDetail(ctx, "sess-multi")
	if err != nil {
		t.Fatal(err)
	}
	if det.Events != 3 || det.WallDurationMS != 90_000 || det.ModelSwitches != 2 {
		t.Fatalf("det summary = events=%d wall=%d switches=%d", det.Events, det.WallDurationMS, det.ModelSwitches)
	}
	if det.InputTokens == nil || *det.InputTokens != 350 || det.OutputTokens == nil || *det.OutputTokens != 100 || det.TotalTokens == nil || *det.TotalTokens != 730 {
		t.Fatalf("det token sums = in=%v out=%v total=%v", det.InputTokens, det.OutputTokens, det.TotalTokens)
	}
	if det.ReasoningTokens != nil || det.DurationMS != nil {
		t.Fatalf("partial nil fields should remain nil: reasoning=%v duration=%v", det.ReasoningTokens, det.DurationMS)
	}
	if det.Cost == nil || *det.Cost < 0.199 || *det.Cost > 0.201 {
		t.Fatalf("det cost = %v, want ~0.20", det.Cost)
	}
	if det.Cache.CachedTokens != 250 || det.Cache.EligibleTokens != 600 {
		t.Fatalf("det cache = %+v, want 250/600", det.Cache)
	}
	if det.Accuracy != usage.AccuracyUnknown {
		t.Fatalf("mixed accuracy = %q, want unknown", det.Accuracy)
	}
	if !slices.Equal(det.Machines, []string{"m1", "m2"}) || !slices.Equal(det.Projects, []string{"proj-x"}) {
		t.Fatalf("det slices = machines=%v projects=%v", det.Machines, det.Projects)
	}
	if len(det.ModelBreakdown) != 2 || det.ModelBreakdown[0].Model != "gpt-5" || *det.ModelBreakdown[0].TotalTokens != 370 {
		t.Fatalf("det model breakdown = %+v", det.ModelBreakdown)
	}

	// Timeline truncates at 500 events while preserving full totals.
	many := make([]usage.Event, 0, 510)
	for i := 0; i < 510; i++ {
		many = append(many, usage.Event{
			SchemaVersion: usage.SchemaVersion,
			EventID:       fmt.Sprintf("tl-%03d", i),
			Timestamp:     t0.Add(time.Duration(i) * time.Second),
			MachineID:     "m1",
			Provider:      "openai",
			Model:         "gpt-5",
			Tool:          "codex",
			SessionID:     "sess-long",
			TotalTokens:   usage.Int64(1),
			TokenAccuracy: usage.AccuracyReported,
			Source:        usage.Source{Adapter: "codex", AdapterVersion: "1"},
		})
	}
	if _, err := store.Ingest(ctx, many); err != nil {
		t.Fatal(err)
	}
	longDet, err := store.SessionDetail(ctx, "sess-long")
	if err != nil {
		t.Fatal(err)
	}
	if longDet.Events != 510 || *longDet.TotalTokens != 510 || !longDet.TimelineTruncated || len(longDet.Timeline) != maxSessionDetailTimelineEvents || longDet.Timeline[0].EventID != "tl-010" {
		t.Fatalf("longDet = events=%d total=%v trunc=%v timelineLen=%d first=%v",
			longDet.Events, longDet.TotalTokens, longDet.TimelineTruncated, len(longDet.Timeline), longDet.Timeline[0].EventID)
	}
}
