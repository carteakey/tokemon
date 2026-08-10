package database

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tokemon/tokemon/internal/catalog"
	"github.com/tokemon/tokemon/internal/usage"
)

func TestOpenBacksUpExistingDatabaseBeforeMigration(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "tokemon.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE sentinel (value TEXT); INSERT INTO sentinel VALUES ('preserved')`); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	store, err := Open(path, catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	store.Close()
	backups, err := filepath.Glob(filepath.Join(directory, "backups", "tokemon-v0-before-v4-*.db"))
	if err != nil || len(backups) != 1 {
		t.Fatalf("backups = %v, err = %v", backups, err)
	}
	backup, err := sql.Open("sqlite", backups[0])
	if err != nil {
		t.Fatal(err)
	}
	defer backup.Close()
	var value string
	if err := backup.QueryRow(`SELECT value FROM sentinel`).Scan(&value); err != nil || value != "preserved" {
		t.Fatalf("backup value = %q, err = %v", value, err)
	}

	reopened, err := Open(path, catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	reopened.Close()
	backups, _ = filepath.Glob(filepath.Join(directory, "backups", "*.db"))
	if len(backups) != 1 {
		t.Fatalf("ordinary restart created another backup: %v", backups)
	}
}

func TestOpenUsesWALForFileDatabase(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "tokemon.db"), catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	var journalMode string
	if err := store.db.QueryRow("PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(journalMode, "wal") {
		t.Fatalf("journal mode = %q, want wal", journalMode)
	}
}

func TestRecordHeartbeatRoundTripsMachineMetadata(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "tokemon.db"), catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	err = store.RecordHeartbeat(context.Background(), AgentHeartbeat{
		MachineID: "machine", AgentVersion: "0.3.0", OperatingSystem: "darwin", Architecture: "arm64",
		Adapters: []string{"openclaw", "codex", "codex"}, SourceCount: 3, SourceErrorCount: 1,
	})
	if err != nil {
		t.Fatal(err)
	}
	machines, err := store.Machines(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(machines) != 1 || machines[0].AgentVersion != "0.3.0" || machines[0].OperatingSystem != "darwin" || machines[0].Architecture != "arm64" || machines[0].SourceCount != 3 || machines[0].SourceErrorCount != 1 {
		t.Fatalf("machines = %+v", machines)
	}
	if got := strings.Join(machines[0].DetectedAdapters, ","); got != "codex,openclaw" {
		t.Fatalf("detected adapters = %q", got)
	}
	heartbeatSeenAt := machines[0].LastSeenAt
	if _, err := store.Ingest(context.Background(), []usage.Event{{
		SchemaVersion: usage.SchemaVersion,
		EventID:       "historical-event",
		Timestamp:     time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
		MachineID:     "machine",
		Provider:      "openai",
		Model:         "gpt",
		Tool:          "codex",
		TotalTokens:   usage.Int64(1),
		TokenAccuracy: usage.AccuracyReported,
		Source:        usage.Source{Adapter: "codex", AdapterVersion: "0.6.0"},
	}}); err != nil {
		t.Fatal(err)
	}
	machines, err = store.Machines(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(machines) != 1 || machines[0].LastSeenAt != heartbeatSeenAt {
		t.Fatalf("historical event moved heartbeat last_seen_at: %+v", machines)
	}
}

func TestReadyAndStaleAgentsUseSQLiteState(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "tokemon.db"), catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Ready(context.Background()); err != nil {
		store.Close()
		t.Fatalf("ready before close: %v", err)
	}
	if err := store.RecordHeartbeat(context.Background(), AgentHeartbeat{MachineID: "machine"}); err != nil {
		store.Close()
		t.Fatal(err)
	}
	stale, err := store.StaleAgents(context.Background(), time.Now().UTC().Add(time.Minute))
	if err != nil || stale != 1 {
		store.Close()
		t.Fatalf("stale agents = %d, err = %v, want 1", stale, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Ready(context.Background()); err == nil {
		t.Fatal("ready after close unexpectedly succeeded")
	}
}

func TestDisplayAliasesRoundTrip(t *testing.T) {
	store, err := Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	if err := store.SetDisplayAlias(context.Background(), AliasKindModel, "claude-sonnet-4", "Sonnet 4"); err != nil {
		t.Fatal(err)
	}
	if err := store.SetDisplayAlias(context.Background(), AliasKindMachine, "machine-1", "Desk"); err != nil {
		t.Fatal(err)
	}
	aliases, err := store.DisplayAliases(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(aliases) != 2 || aliases[0].Kind != AliasKindMachine || aliases[1].Alias != "Sonnet 4" {
		t.Fatalf("aliases = %+v", aliases)
	}
	if err := store.SetDisplayAlias(context.Background(), AliasKindMachine, "machine-1", ""); err != nil {
		t.Fatal(err)
	}
	aliases, err = store.DisplayAliases(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(aliases) != 1 || aliases[0].Kind != AliasKindModel {
		t.Fatalf("aliases after clear = %+v", aliases)
	}
}

func TestPruneBackupsKeepsNewestFiles(t *testing.T) {
	directory := t.TempDir()
	for _, name := range []string{"tokemon-1.db", "tokemon-2.db", "tokemon-3.db", "other.db"} {
		if err := os.WriteFile(filepath.Join(directory, name), nil, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := pruneBackups(directory, "tokemon-", 2); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	want := []string{"other.db", "tokemon-2.db", "tokemon-3.db"}
	if fmt.Sprint(names) != fmt.Sprint(want) {
		t.Fatalf("files = %v, want %v", names, want)
	}
}

func TestIngestIsIdempotentAndDerivesEvolution(t *testing.T) {
	inputPrice, outputPrice := 10.0, 20.0
	modelCatalog := catalog.MustNew(map[string]catalog.Model{
		"test-model": {
			Provider: "test", DisplayName: "Test Model", Aliases: []string{"raw-model"},
			Pricing: catalog.Pricing{Currency: "USD", Mode: "standard", VerifiedAt: "2026-07-12", Source: "https://example.com/pricing", InputPricePerMillion: &inputPrice, OutputPricePerMillion: &outputPrice},
		},
	})
	store, err := Open(t.TempDir()+"/tokemon.db", modelCatalog)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	event := usage.Event{
		SchemaVersion: usage.SchemaVersion,
		EventID:       "event-1",
		Timestamp:     time.Date(2026, 7, 11, 18, 42, 0, 0, time.UTC),
		MachineID:     "machine-1",
		Provider:      "test-provider",
		Model:         "raw-model",
		Tool:          "generic-jsonl",
		InputTokens:   usage.Int64(2),
		OutputTokens:  usage.Int64(8),
		TotalTokens:   usage.Int64(10),
		TokenAccuracy: usage.AccuracyReported,
		Source:        usage.Source{Adapter: "generic-jsonl", AdapterVersion: "test"},
	}
	result, err := store.Ingest(context.Background(), []usage.Event{event})
	if err != nil {
		t.Fatal(err)
	}
	if result.Accepted != 1 || result.CurrentTotal != 10 || !result.Evolved || result.CurrentStage != 1 {
		t.Fatalf("unexpected first ingest: %+v", result)
	}
	result, err = store.Ingest(context.Background(), []usage.Event{event})
	if err != nil {
		t.Fatal(err)
	}
	if result.Duplicates != 1 || result.Accepted != 0 || result.CurrentTotal != 10 {
		t.Fatalf("unexpected duplicate ingest: %+v", result)
	}
	events, err := store.Events(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].CanonicalModel != "test-model" || events[0].Cost == nil {
		t.Fatalf("unexpected round-trip events: %+v", events)
	}
	overview, err := store.Overview(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if overview.EstimatedCost.Amount != 0.00018 || overview.EstimatedCost.PricedTokens != 10 || overview.EstimatedCost.UnpricedTokens != 0 {
		t.Fatalf("unexpected estimated cost summary: %+v", overview.EstimatedCost)
	}
}

func TestDetailedCodexUsageDeduplicatesMatchingRecords(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "tokemon.db"), catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	timestamp := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	detailed := func(id, session string) usage.Event {
		return usage.Event{
			SchemaVersion:    usage.SchemaVersion,
			EventID:          id,
			Timestamp:        timestamp,
			MachineID:        "machine",
			SessionID:        session,
			Provider:         "openai",
			Model:            "gpt-5.6",
			Tool:             "codex",
			InputTokens:      usage.Int64(2),
			OutputTokens:     usage.Int64(2),
			CacheReadTokens:  usage.Int64(8),
			CacheWriteTokens: usage.Int64(0),
			ReasoningTokens:  usage.Int64(1),
			TotalTokens:      usage.Int64(12),
			TokenAccuracy:    usage.AccuracyReported,
			Source:           usage.Source{Adapter: "codex", AdapterVersion: "0.4.0"},
		}
	}
	first := detailed("codex-first", "session-1")
	duplicate := detailed("codex-duplicate", "session-1")
	separateSession := detailed("codex-separate-session", "session-2")
	result, err := store.Ingest(context.Background(), []usage.Event{first, duplicate, separateSession})
	if err != nil {
		t.Fatal(err)
	}
	if result.Accepted != 2 || result.Duplicates != 1 || result.CurrentTotal != 24 {
		t.Fatalf("unexpected deduplication result: %+v", result)
	}
	events, err := store.Events(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("stored events = %d, want 2: %+v", len(events), events)
	}
}

func TestDetailedCodexUsageMergesDedupConflictDuringRefresh(t *testing.T) {
	store, err := Open(filepath.Join(t.TempDir(), "tokemon.db"), catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	timestamp := time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC)
	makeEvent := func(id string, total int64) usage.Event {
		return usage.Event{
			SchemaVersion: usage.SchemaVersion, EventID: id, Timestamp: timestamp,
			MachineID: "machine", SessionID: "session", Provider: "openai", Model: "gpt-5.6", Tool: "codex",
			InputTokens: usage.Int64(total), OutputTokens: usage.Int64(0), CacheReadTokens: usage.Int64(0),
			CacheWriteTokens: usage.Int64(0), ReasoningTokens: usage.Int64(0), TotalTokens: usage.Int64(total),
			TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "codex", AdapterVersion: "0.6.0"},
		}
	}
	if _, err := store.Ingest(context.Background(), []usage.Event{makeEvent("old-event", 10), makeEvent("other-event", 20)}); err != nil {
		t.Fatal(err)
	}
	refreshed := makeEvent("old-event", 20)
	result, err := store.Ingest(context.Background(), []usage.Event{refreshed})
	if err != nil {
		t.Fatal(err)
	}
	if result.Accepted != 0 || result.Duplicates != 1 || result.CurrentTotal != 20 {
		t.Fatalf("unexpected merged refresh: %+v", result)
	}
	events, err := store.Events(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].EventID != "old-event" || events[0].TotalTokens == nil || *events[0].TotalTokens != 20 {
		t.Fatalf("dedup conflict was not merged: %+v", events)
	}
}

func TestOverviewCostPreservesUnpricedUsage(t *testing.T) {
	inputPrice := 10.0
	outputPrice := 0.0
	modelCatalog := catalog.MustNew(map[string]catalog.Model{
		"priced-model": {Provider: "test", DisplayName: "Priced Model", Aliases: []string{"priced"}, Pricing: catalog.Pricing{Currency: "USD", Mode: "standard", VerifiedAt: "2026-07-12", Source: "https://example.com/pricing", InputPricePerMillion: &inputPrice, OutputPricePerMillion: &outputPrice}},
	})
	store, err := Open(t.TempDir()+"/tokemon.db", modelCatalog)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	events := []usage.Event{
		{SchemaVersion: usage.SchemaVersion, EventID: "priced", Timestamp: time.Now().UTC(), MachineID: "machine", Provider: "provider", Model: "priced", Tool: "tool", InputTokens: usage.Int64(1_000_000), TotalTokens: usage.Int64(1_000_000), TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "test", AdapterVersion: "1"}},
		{SchemaVersion: usage.SchemaVersion, EventID: "unpriced", Timestamp: time.Now().UTC(), MachineID: "machine", Provider: "provider", Model: "unknown", Tool: "tool", TotalTokens: usage.Int64(500_000), TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "test", AdapterVersion: "1"}},
	}
	if _, err := store.Ingest(context.Background(), events); err != nil {
		t.Fatal(err)
	}
	overview, err := store.Overview(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if overview.EstimatedCost.Amount != 10 || overview.EstimatedCost.PricedTokens != 1_000_000 || overview.EstimatedCost.UnpricedTokens != 500_000 {
		t.Fatalf("unexpected estimated cost summary: %+v", overview.EstimatedCost)
	}
}

func TestOverviewReportsCacheHitRateAndDistinctThreads(t *testing.T) {
	store, err := Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Now().UTC()
	events := []usage.Event{
		{SchemaVersion: usage.SchemaVersion, EventID: "one", Timestamp: now, MachineID: "machine", SessionID: "thread-1", Provider: "openai", Model: "model", Tool: "codex", InputTokens: usage.Int64(20), CacheReadTokens: usage.Int64(80), OutputTokens: usage.Int64(5), TotalTokens: usage.Int64(105), TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "codex", AdapterVersion: "test"}},
		{SchemaVersion: usage.SchemaVersion, EventID: "two", Timestamp: now, MachineID: "machine", SessionID: "thread-1", Provider: "openai", Model: "model", Tool: "codex", InputTokens: usage.Int64(30), CacheReadTokens: usage.Int64(70), OutputTokens: usage.Int64(5), TotalTokens: usage.Int64(105), TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "codex", AdapterVersion: "test"}},
		{SchemaVersion: usage.SchemaVersion, EventID: "three", Timestamp: now, MachineID: "machine", SessionID: "thread-2", Provider: "openai", Model: "model", Tool: "codex", TotalTokens: usage.Int64(50), TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "codex", AdapterVersion: "test"}},
	}
	if _, err := store.Ingest(context.Background(), events); err != nil {
		t.Fatal(err)
	}
	overview, err := store.Overview(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if overview.Cache.CachedTokens != 150 || overview.Cache.EligibleTokens != 200 || overview.Cache.HitRate != 0.75 {
		t.Fatalf("unexpected cache summary: %+v", overview.Cache)
	}
	if overview.Threads != 2 {
		t.Fatalf("threads = %d, want 2", overview.Threads)
	}
}

func TestOverviewMergesProjectsAcrossMachines(t *testing.T) {
	store, err := Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	events := []usage.Event{
		{SchemaVersion: usage.SchemaVersion, EventID: "project-a", Timestamp: time.Now().UTC(), MachineID: "laptop", SessionID: "session-a", Project: "/Users/alice/repos/Carteakey.dev", Provider: "openai", Model: "model", Tool: "codex", TotalTokens: usage.Int64(100), TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "test", AdapterVersion: "1"}},
		{SchemaVersion: usage.SchemaVersion, EventID: "project-b", Timestamp: time.Now().UTC(), MachineID: "desktop", SessionID: "session-b", Project: "/home/alice/CARTEAKEY.DEV", Provider: "anthropic", Model: "model", Tool: "claude-code", TotalTokens: usage.Int64(50), TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "test", AdapterVersion: "1"}},
	}
	if _, err := store.Ingest(context.Background(), events); err != nil {
		t.Fatal(err)
	}
	overview, err := store.Overview(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(overview.ByProject) != 1 || overview.ByProject[0].Project != "carteakey.dev" || overview.ByProject[0].Tokens != 150 || overview.ByProject[0].Machines != 2 || overview.ByProject[0].Sessions != 2 {
		t.Fatalf("unexpected project summary: %+v", overview.ByProject)
	}
	eventsRoundTrip, err := store.Events(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if eventsRoundTrip[0].Project != "carteakey.dev" {
		t.Fatalf("stored project leaked or was not normalized: %+v", eventsRoundTrip[0])
	}
}

func TestOverviewGroupsUsageByTool(t *testing.T) {
	store, err := Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	events := []usage.Event{
		{SchemaVersion: usage.SchemaVersion, EventID: "codex-a", Timestamp: time.Now().UTC(), MachineID: "laptop", Provider: "openai", Model: "model", Tool: "codex", TotalTokens: usage.Int64(100), TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "codex", AdapterVersion: "test"}},
		{SchemaVersion: usage.SchemaVersion, EventID: "codex-b", Timestamp: time.Now().UTC(), MachineID: "desktop", Provider: "openai", Model: "model", Tool: "codex", TotalTokens: usage.Int64(50), TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "codex", AdapterVersion: "test"}},
		{SchemaVersion: usage.SchemaVersion, EventID: "claude", Timestamp: time.Now().UTC(), MachineID: "laptop", Provider: "anthropic", Model: "model", Tool: "claude-code", TotalTokens: usage.Int64(75), TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "claude-code", AdapterVersion: "test"}},
	}
	if _, err := store.Ingest(context.Background(), events); err != nil {
		t.Fatal(err)
	}
	overview, err := store.Overview(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(overview.ByTool) != 2 || overview.ByTool[0].Tool != "codex" || overview.ByTool[0].Tokens != 150 || overview.ByTool[1].Tool != "claude-code" || overview.ByTool[1].Tokens != 75 {
		t.Fatalf("unexpected tool summary: %+v", overview.ByTool)
	}
}

func TestIngestRefreshesAnExistingSnapshot(t *testing.T) {
	store, err := Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	event := usage.Event{
		SchemaVersion: usage.SchemaVersion,
		EventID:       "snapshot-1",
		Timestamp:     time.Date(2026, 7, 11, 18, 42, 0, 0, time.UTC),
		MachineID:     "machine-1",
		Provider:      "openai",
		Model:         "gpt-5.5",
		Tool:          "codex",
		TotalTokens:   usage.Int64(10),
		TokenAccuracy: usage.AccuracyReported,
		Source:        usage.Source{Adapter: "codex", AdapterVersion: "test"},
	}
	if _, err := store.Ingest(context.Background(), []usage.Event{event}); err != nil {
		t.Fatal(err)
	}

	event.TotalTokens = usage.Int64(25)
	result, err := store.Ingest(context.Background(), []usage.Event{event})
	if err != nil {
		t.Fatal(err)
	}
	if result.Accepted != 0 || result.Duplicates != 1 || result.CurrentTotal != 25 {
		t.Fatalf("unexpected refreshed snapshot result: %+v", result)
	}
	events, err := store.Events(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].TotalTokens == nil || *events[0].TotalTokens != 25 {
		t.Fatalf("unexpected refreshed event: %+v", events)
	}
}

func TestDetailedCodexUsageSupersedesAggregateSnapshot(t *testing.T) {
	inputPrice, outputPrice, cachePrice, cacheWritePrice := 1.0, 10.0, 0.1, 1.0
	modelCatalog := catalog.MustNew(map[string]catalog.Model{
		"gpt": {Provider: "openai", DisplayName: "GPT", Aliases: []string{"gpt"}, Pricing: catalog.Pricing{Currency: "USD", Mode: "standard", VerifiedAt: "2026-07-12", Source: "https://example.com/pricing", InputPricePerMillion: &inputPrice, OutputPricePerMillion: &outputPrice, CacheReadPricePerMillion: &cachePrice, CacheWritePricePerMillion: &cacheWritePrice}},
	})
	store, err := Open(t.TempDir()+"/tokemon.db", modelCatalog)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	aggregate := usage.Event{SchemaVersion: usage.SchemaVersion, EventID: "aggregate", Timestamp: time.Now().UTC(), MachineID: "machine", SessionID: "session", Provider: "openai", Model: "gpt", Tool: "codex", TotalTokens: usage.Int64(100), TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "codex", AdapterVersion: "0.2.0"}}
	if _, err := store.Ingest(context.Background(), []usage.Event{aggregate}); err != nil {
		t.Fatal(err)
	}
	detailed := usage.Event{SchemaVersion: usage.SchemaVersion, EventID: "detailed", Timestamp: time.Now().UTC(), MachineID: "machine", SessionID: "session", Provider: "openai", Model: "gpt", Tool: "codex", InputTokens: usage.Int64(10), OutputTokens: usage.Int64(10), CacheReadTokens: usage.Int64(80), CacheWriteTokens: usage.Int64(0), TotalTokens: usage.Int64(100), TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "codex", AdapterVersion: "0.3.0"}}
	result, err := store.Ingest(context.Background(), []usage.Event{detailed})
	if err != nil {
		t.Fatal(err)
	}
	if result.CurrentTotal != 100 {
		t.Fatalf("lifetime tokens = %d, want 100", result.CurrentTotal)
	}
	events, err := store.Events(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].EventID != "detailed" || events[0].Cost == nil {
		t.Fatalf("unexpected migrated events: %+v", events)
	}

	result, err = store.Ingest(context.Background(), []usage.Event{aggregate})
	if err != nil {
		t.Fatal(err)
	}
	if result.Accepted != 0 || result.Duplicates != 1 || result.CurrentTotal != 100 {
		t.Fatalf("legacy aggregate was not ignored after detailed backfill: %+v", result)
	}
	events, err = store.Events(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 1 || events[0].EventID != "detailed" {
		t.Fatalf("legacy aggregate returned after detailed backfill: %+v", events)
	}
}

func TestTokenCompositionSeparatesCachedAndUnclassifiedTokens(t *testing.T) {
	store, err := Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	events := []usage.Event{
		{SchemaVersion: usage.SchemaVersion, EventID: "detailed-components", Timestamp: time.Now().UTC(), MachineID: "machine", Provider: "openai", Model: "model", Tool: "codex", InputTokens: usage.Int64(40), CacheReadTokens: usage.Int64(50), CacheWriteTokens: usage.Int64(10), OutputTokens: usage.Int64(20), TotalTokens: usage.Int64(120), TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "test", AdapterVersion: "1"}},
		{SchemaVersion: usage.SchemaVersion, EventID: "aggregate-only", Timestamp: time.Now().UTC(), MachineID: "machine", Provider: "other", Model: "model", Tool: "other", TotalTokens: usage.Int64(30), TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "test", AdapterVersion: "1"}},
	}
	if _, err := store.Ingest(context.Background(), events); err != nil {
		t.Fatal(err)
	}
	composition, err := store.TokenComposition(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := TokenComposition{InputTokens: 100, UncachedInputTokens: 40, CachedInputTokens: 60, OutputTokens: 20, UnclassifiedTokens: 30}
	if composition != want {
		t.Fatalf("composition = %+v, want %+v", composition, want)
	}
}

func TestActivityBuildsPixelCalendarAndPreservesUnknownTotals(t *testing.T) {
	store, err := Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	base := func(id string, timestamp time.Time, total *int64) usage.Event {
		return usage.Event{
			SchemaVersion: usage.SchemaVersion,
			EventID:       id,
			Timestamp:     timestamp,
			MachineID:     "machine-1",
			Provider:      "provider",
			Model:         "model",
			Tool:          "tool",
			TotalTokens:   total,
			TokenAccuracy: usage.AccuracyReported,
			Source:        usage.Source{Adapter: "test", AdapterVersion: "1"},
		}
	}
	events := []usage.Event{
		base("activity-known-1", time.Date(2026, 7, 11, 18, 0, 0, 0, time.UTC), usage.Int64(120)),
		base("activity-unknown", time.Date(2026, 7, 11, 19, 0, 0, 0, time.UTC), nil),
		base("activity-known-2", time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC), usage.Int64(60)),
		base("activity-outside-window", time.Date(2025, 7, 1, 12, 0, 0, 0, time.UTC), usage.Int64(999)),
	}
	if result, err := store.Ingest(context.Background(), events); err != nil || result.Accepted != len(events) {
		t.Fatalf("unexpected ingest result: %+v, error: %v", result, err)
	}

	activity, err := store.Activity(context.Background(), time.Date(2026, 7, 12, 12, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	if activity.StartDate != "2025-07-13" || activity.EndDate != "2026-07-12" {
		t.Fatalf("unexpected activity range: %+v", activity)
	}
	if len(activity.Weeks) != 53 || activity.TotalTokens != 180 || activity.ActiveDays != 2 || activity.MaxDailyTokens != 120 {
		t.Fatalf("unexpected activity summary: %+v", activity)
	}

	findDay := func(want string) (ActivityDay, bool) {
		for _, week := range activity.Weeks {
			for _, day := range week.Days {
				if day.Date == want {
					return day, true
				}
			}
		}
		return ActivityDay{}, false
	}
	known, ok := findDay("2026-07-11")
	if !ok || known.Tokens != 120 || known.Events != 2 || known.UnknownTokens != 1 || known.Level != 4 || known.Future {
		t.Fatalf("unexpected known day: %+v", known)
	}
	if len(known.ByModel) != 1 || known.ByModel[0].Name != "model" || known.ByModel[0].Tokens != 120 || known.ByModel[0].UnknownTokens != 1 {
		t.Fatalf("unexpected model breakdown: %+v", known.ByModel)
	}
	if len(known.ByProvider) != 1 || known.ByProvider[0].Name != "provider" || known.ByProvider[0].Tokens != 120 || known.ByProvider[0].UnknownTokens != 1 {
		t.Fatalf("unexpected provider breakdown: %+v", known.ByProvider)
	}
	quiet, ok := findDay("2026-07-09")
	if !ok || quiet.Events != 0 || quiet.Level != 0 || quiet.Future {
		t.Fatalf("unexpected quiet day: %+v", quiet)
	}
	future, ok := findDay("2026-07-13")
	if !ok || !future.Future {
		t.Fatalf("expected future cells after the current day: %+v", future)
	}
}

func TestConfiguredTimezoneControlsCalendarBuckets(t *testing.T) {
	location := time.FixedZone("Toronto", -4*60*60)
	store, err := OpenWithLocation(t.TempDir()+"/tokemon.db", catalog.Empty(), location)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	events := []usage.Event{
		{SchemaVersion: usage.SchemaVersion, EventID: "timezone-before-midnight", Timestamp: time.Date(2026, 7, 18, 2, 30, 0, 0, time.UTC), MachineID: "machine", Provider: "provider", Model: "model", Tool: "codex", TotalTokens: usage.Int64(10), TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "codex", AdapterVersion: "test"}},
		{SchemaVersion: usage.SchemaVersion, EventID: "timezone-after-midnight", Timestamp: time.Date(2026, 7, 18, 5, 0, 0, 0, time.UTC), MachineID: "machine", Provider: "provider", Model: "model", Tool: "codex", TotalTokens: usage.Int64(20), TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "codex", AdapterVersion: "test"}},
	}
	if result, err := store.Ingest(context.Background(), events); err != nil || result.Accepted != len(events) {
		t.Fatalf("unexpected ingest result: %+v, error: %v", result, err)
	}

	daily, err := store.DailyUsage(context.Background(), time.Date(2026, 7, 17, 0, 0, 0, 0, location), time.Date(2026, 7, 19, 0, 0, 0, 0, location))
	if err != nil {
		t.Fatal(err)
	}
	if len(daily) != 2 || daily[0].Date != "2026-07-17" || daily[0].Tokens != 10 || daily[1].Date != "2026-07-18" || daily[1].Tokens != 20 {
		t.Fatalf("daily buckets ignored configured timezone: %+v", daily)
	}

	analytics, err := store.Analytics(context.Background(), AnalyticsQuery{Period: "7d", Dimension: "projects", Now: time.Date(2026, 7, 18, 12, 0, 0, 0, time.UTC)})
	if err != nil {
		t.Fatal(err)
	}
	if analytics.Timezone != "Toronto" || analytics.Summary.ActiveDays != 2 {
		t.Fatalf("analytics timezone or active days = %+v", analytics)
	}
	var july17, july18 AnalyticsPoint
	for _, point := range analytics.Points {
		switch point.Date {
		case "2026-07-17":
			july17 = point
		case "2026-07-18":
			july18 = point
		}
	}
	if july17.Tokens != 10 || july18.Tokens != 20 {
		t.Fatalf("analytics buckets ignored configured timezone: Jul17=%+v Jul18=%+v", july17, july18)
	}
}

func TestAnalyticsSupportsPeriodsFiltersBreakdownsAndUnknownTotals(t *testing.T) {
	store, err := Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	events := []usage.Event{
		{SchemaVersion: usage.SchemaVersion, EventID: "analytics-one", Timestamp: time.Date(2026, 7, 13, 10, 0, 0, 0, time.UTC), MachineID: "machine-one", Project: "project-a", Provider: "openai", Model: "gpt", Tool: "codex", SessionID: "session-one", InputTokens: usage.Int64(60), CacheReadTokens: usage.Int64(20), OutputTokens: usage.Int64(20), TotalTokens: usage.Int64(100), TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "codex", AdapterVersion: "test"}},
		{SchemaVersion: usage.SchemaVersion, EventID: "analytics-two", Timestamp: time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC), MachineID: "machine-two", Project: "project-b", Provider: "anthropic", Model: "claude", Tool: "claude-code", SessionID: "session-two", TotalTokens: usage.Int64(50), TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "claude-code", AdapterVersion: "test"}},
		{SchemaVersion: usage.SchemaVersion, EventID: "analytics-unknown", Timestamp: time.Date(2026, 7, 13, 11, 0, 0, 0, time.UTC), MachineID: "machine-one", Project: "project-a", Provider: "openai", Model: "gpt", Tool: "codex", TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "codex", AdapterVersion: "test"}},
		{SchemaVersion: usage.SchemaVersion, EventID: "analytics-old", Timestamp: time.Date(2026, 6, 1, 10, 0, 0, 0, time.UTC), MachineID: "machine-two", Project: "project-old", Provider: "anthropic", Model: "claude", Tool: "claude-code", TotalTokens: usage.Int64(999), TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "claude-code", AdapterVersion: "test"}},
	}
	if result, err := store.Ingest(context.Background(), events); err != nil || result.Accepted != len(events) {
		t.Fatalf("unexpected ingest result: %+v, error: %v", result, err)
	}

	filtered, err := store.Analytics(context.Background(), AnalyticsQuery{Period: "30d", Dimension: "projects", Machine: "machine-one", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if filtered.Summary.Tokens != 100 || filtered.Summary.Events != 2 || filtered.Summary.ActiveDays != 1 || filtered.Summary.UnknownEvents != 1 || filtered.Summary.SessionTokens != 100 {
		t.Fatalf("unexpected filtered summary: %+v", filtered.Summary)
	}
	if len(filtered.Points) != 30 {
		t.Fatalf("unexpected filtered points: %+v", filtered.Points)
	}
	filteredDay := filtered.Points[len(filtered.Points)-1]
	if filtered.Points[0].Tokens != 0 || filteredDay.Date != "2026-07-13" || filteredDay.Tokens != 100 || filteredDay.UnknownEvents != 1 || filteredDay.InputTokens != 60 || filteredDay.CachedTokens != 20 || filteredDay.OutputTokens != 20 {
		t.Fatalf("analytics points do not preserve quiet days and the active bucket: %+v", filtered.Points)
	}
	if len(filtered.Breakdown) != 1 || filtered.Breakdown[0].Name != "project-a" || filtered.Breakdown[0].Tokens != 100 || filtered.Breakdown[0].Share != 1 {
		t.Fatalf("unexpected filtered breakdown: %+v", filtered.Breakdown)
	}
	if len(filtered.Sessions) != 1 || filtered.Sessions[0].SessionID != "session-one" {
		t.Fatalf("unexpected filtered sessions: %+v", filtered.Sessions)
	}
	if filtered.Comparison == nil || filtered.Comparison.PreviousTokens != 0 || filtered.Comparison.TokenChangePercent != nil {
		t.Fatalf("unexpected filtered comparison: %+v", filtered.Comparison)
	}

	last24Hours, err := store.Analytics(context.Background(), AnalyticsQuery{Period: "24h", Dimension: "providers", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if last24Hours.Bucket != "hour" || len(last24Hours.Points) != 25 || last24Hours.Summary.Tokens != 100 {
		t.Fatalf("unexpected rolling 24-hour analytics: %+v", last24Hours)
	}
	if len(last24Hours.Breakdown) != 1 || last24Hours.Breakdown[0].Name != "openai" {
		t.Fatalf("unexpected provider breakdown: %+v", last24Hours.Breakdown)
	}
	if last24Hours.Comparison == nil || last24Hours.Comparison.PreviousTokens != 50 || last24Hours.Comparison.TokenChangePercent == nil || *last24Hours.Comparison.TokenChangePercent != 100 {
		t.Fatalf("unexpected 24-hour comparison: %+v", last24Hours.Comparison)
	}

	allTime, err := store.Analytics(context.Background(), AnalyticsQuery{Period: "all", Dimension: "models", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if allTime.Bucket != "month" || len(allTime.Points) != 2 || allTime.Summary.Tokens != 1149 {
		t.Fatalf("unexpected all-time analytics: %+v", allTime)
	}
	if len(allTime.Breakdown) != 2 || allTime.Breakdown[0].Name != "claude" || allTime.Breakdown[1].Name != "gpt" {
		t.Fatalf("unexpected model breakdown: %+v", allTime.Breakdown)
	}
	if len(allTime.Facets.Machines) != 2 || len(allTime.Facets.Providers) != 2 || len(allTime.Facets.Models) != 2 || len(allTime.Facets.Tools) != 2 {
		t.Fatalf("unexpected analytics facets: %+v", allTime.Facets)
	}
}

func TestAnalyticsShareTimelineKeepsTopFiveAndGroupsOther(t *testing.T) {
	store, err := Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	events := make([]usage.Event, 0, 8)
	for index, tokens := range []int64{60, 50, 40, 30, 20, 10} {
		events = append(events, usage.Event{
			SchemaVersion: usage.SchemaVersion,
			EventID:       fmt.Sprintf("share-%d", index),
			Timestamp:     time.Date(2026, 7, 13, 10, index, 0, 0, time.UTC),
			MachineID:     "machine",
			Provider:      "provider",
			Model:         fmt.Sprintf("model-%d", index),
			Tool:          "codex",
			TotalTokens:   usage.Int64(tokens),
			TokenAccuracy: usage.AccuracyReported,
			Source:        usage.Source{Adapter: "codex", AdapterVersion: "test"},
		})
	}
	events = append(events,
		usage.Event{SchemaVersion: usage.SchemaVersion, EventID: "share-other-previous", Timestamp: time.Date(2026, 7, 12, 10, 0, 0, 0, time.UTC), MachineID: "machine", Provider: "provider", Model: "model-5", Tool: "codex", TotalTokens: usage.Int64(5), TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "codex", AdapterVersion: "test"}},
		usage.Event{SchemaVersion: usage.SchemaVersion, EventID: "share-unknown", Timestamp: time.Date(2026, 7, 13, 11, 0, 0, 0, time.UTC), MachineID: "machine", Provider: "provider", Model: "model-0", Tool: "codex", TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "codex", AdapterVersion: "test"}},
	)
	if result, err := store.Ingest(context.Background(), events); err != nil || result.Accepted != len(events) {
		t.Fatalf("unexpected ingest result: %+v, error: %v", result, err)
	}

	analytics, err := store.Analytics(context.Background(), AnalyticsQuery{Period: "7d", Dimension: "models", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if len(analytics.ShareSeries) != 6 {
		t.Fatalf("share series = %+v, want five named series and Other", analytics.ShareSeries)
	}
	for index := 0; index < 5; index++ {
		if want := fmt.Sprintf("model-%d", index); analytics.ShareSeries[index].Name != want {
			t.Fatalf("share series %d = %q, want %q: %+v", index, analytics.ShareSeries[index].Name, want, analytics.ShareSeries)
		}
	}
	if other := analytics.ShareSeries[5]; other.Name != "Other" || other.Tokens != 15 {
		t.Fatalf("Other series = %+v, want 15 tokens", other)
	}
	if len(analytics.SharePoints) != 7 {
		t.Fatalf("share points = %d, want seven zero-filled days: %+v", len(analytics.SharePoints), analytics.SharePoints)
	}
	active := analytics.SharePoints[len(analytics.SharePoints)-1]
	if active.Date != "2026-07-13" || active.Tokens != 210 || active.UnknownEvents != 1 || len(active.Values) != 6 || active.Values[5].Tokens != 10 {
		t.Fatalf("unexpected active share point: %+v", active)
	}
	if analytics.SharePoints[0].Tokens != 0 || len(analytics.SharePoints[0].Values) != 6 {
		t.Fatalf("quiet share bucket was not filled with the series shape: %+v", analytics.SharePoints[0])
	}
	// Every bucket's per-series tokens must account for all known tokens, so the
	// stacked bars always total to the bucket total even when a bucket's local
	// ranking differs from the window-wide top five.
	for _, point := range analytics.SharePoints {
		var total int64
		for _, value := range point.Values {
			total += value.Tokens
		}
		if total != point.Tokens {
			t.Fatalf("bucket %s series token sum = %d, want %d: %+v", point.Date, total, point.Tokens, point)
		}
	}
}

func TestAnalyticsShareTimelineBucketOutsideTopFiveRanksIntoOther(t *testing.T) {
	store, err := Open(t.TempDir()+"/tokemon.db", catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Date(2026, 7, 13, 12, 0, 0, 0, time.UTC)
	events := make([]usage.Event, 0, 12)
	for index, tokens := range []int64{60, 50, 40, 30, 20, 10} {
		events = append(events, usage.Event{
			SchemaVersion: usage.SchemaVersion,
			EventID:       fmt.Sprintf("rank-%d", index),
			Timestamp:     time.Date(2026, 7, 12, 10, index, 0, 0, time.UTC),
			MachineID:     "machine",
			Provider:      "provider",
			Model:         fmt.Sprintf("model-%d", index),
			Tool:          "codex",
			TotalTokens:   usage.Int64(tokens),
			TokenAccuracy: usage.AccuracyReported,
			Source:        usage.Source{Adapter: "codex", AdapterVersion: "test"},
		})
	}
	// On day 12 the models below the global top five dominate the bucket, so
	// their tokens must still be fully attributed to the Other series.
	for index, tokens := range []int64{1, 2} {
		events = append(events, usage.Event{
			SchemaVersion: usage.SchemaVersion,
			EventID:       fmt.Sprintf("rank-other-%d", index),
			Timestamp:     time.Date(2026, 7, 12, 11, index, 0, 0, time.UTC),
			MachineID:     "machine",
			Provider:      "provider",
			Model:         fmt.Sprintf("model-%d", 5+index),
			Tool:          "codex",
			TotalTokens:   usage.Int64(tokens),
			TokenAccuracy: usage.AccuracyReported,
			Source:        usage.Source{Adapter: "codex", AdapterVersion: "test"},
		})
	}
	for index, tokens := range []int64{5, 4, 3, 1, 0} {
		events = append(events, usage.Event{
			SchemaVersion: usage.SchemaVersion,
			EventID:       fmt.Sprintf("rank-day13-%d", index),
			Timestamp:     time.Date(2026, 7, 13, 9, index, 0, 0, time.UTC),
			MachineID:     "machine",
			Provider:      "provider",
			Model:         fmt.Sprintf("model-%d", index),
			Tool:          "codex",
			TotalTokens:   usage.Int64(tokens),
			TokenAccuracy: usage.AccuracyReported,
			Source:        usage.Source{Adapter: "codex", AdapterVersion: "test"},
		})
	}
	if result, err := store.Ingest(context.Background(), events); err != nil || result.Accepted != len(events) {
		t.Fatalf("unexpected ingest result: %+v, error: %v", result, err)
	}

	analytics, err := store.Analytics(context.Background(), AnalyticsQuery{Period: "7d", Dimension: "models", Now: now})
	if err != nil {
		t.Fatal(err)
	}
	if len(analytics.ShareSeries) != 6 {
		t.Fatalf("share series = %+v, want five named series and Other", analytics.ShareSeries)
	}
	for _, point := range analytics.SharePoints {
		var total int64
		for _, value := range point.Values {
			total += value.Tokens
		}
		if total != point.Tokens {
			t.Fatalf("bucket %s series token sum = %d, want %d: %+v", point.Date, total, point.Tokens, point)
		}
	}
	for _, date := range []string{"2026-07-12", "2026-07-13"} {
		if point := pointForDate(analytics.SharePoints, date); point == nil {
			t.Fatalf("missing share bucket for %s: %+v", date, analytics.SharePoints)
		}
	}
}

func pointForDate(points []AnalyticsSharePoint, date string) *AnalyticsSharePoint {
	for index := range points {
		if points[index].Date == date {
			return &points[index]
		}
	}
	return nil
}
