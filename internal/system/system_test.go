package system_test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/tokemon/tokemon/internal/adapters"
	"github.com/tokemon/tokemon/internal/adapters/claude"
	"github.com/tokemon/tokemon/internal/adapters/generic"
	"github.com/tokemon/tokemon/internal/agent"
	"github.com/tokemon/tokemon/internal/api"
	"github.com/tokemon/tokemon/internal/catalog"
	"github.com/tokemon/tokemon/internal/database"
	"github.com/tokemon/tokemon/internal/usage"
)

const (
	systemToken = "car67-system-test-token"
	baseTime    = "2026-08-09T12:00:00Z"
)

// TestTwoAgentSystemFlow exercises the real provider adapter -> local state ->
// authenticated client -> HTTP batch API -> SQLite path. It intentionally uses
// Claude and Generic JSONL fixtures only; provider implementation remains in
// the adapter packages and is not duplicated here.
func TestTwoAgentSystemFlow(t *testing.T) {
	ctx := context.Background()
	store, err := database.Open(filepath.Join(t.TempDir(), "hub.db"), catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	hub, err := api.NewWithConfig(store, api.Config{IngestToken: systemToken, DashboardToken: systemToken})
	if err != nil {
		t.Fatal(err)
	}
	httpServer := httptest.NewServer(hub.Handler())
	defer httpServer.Close()

	homeA := t.TempDir()
	claudePath := filepath.Join(homeA, ".claude", "projects", "private-project", "session.jsonl")
	if err := os.MkdirAll(filepath.Dir(claudePath), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(claudePath, []byte(claudeFixture()), 0o600); err != nil {
		t.Fatal(err)
	}

	homeB := t.TempDir()
	genericPath := filepath.Join(homeB, "usage.jsonl")
	if err := os.WriteFile(genericPath, []byte(genericLine("agent-b-initial", baseTime, 9000)), 0o600); err != nil {
		t.Fatal(err)
	}

	stateA, err := agent.OpenState(filepath.Join(t.TempDir(), "agent-a", "state.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer stateA.Close()
	stateBPath := filepath.Join(t.TempDir(), "agent-b", "state.db")
	stateB, err := agent.OpenState(stateBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer stateB.Close()

	clientA := agent.Client{ServerURL: httpServer.URL, Token: systemToken}
	clientB := agent.Client{ServerURL: httpServer.URL, Token: systemToken}
	adaptersA := []adapters.Adapter{claude.New(homeA)}
	adaptersB := []adapters.Adapter{generic.New(genericPath)}

	firstA, eventsA, _ := syncAgent(t, ctx, stateA, clientA, adaptersA, "agent-a", true)
	if firstA.Accepted != 1 || firstA.Duplicates != 0 || firstA.CurrentTotal != 150 {
		t.Fatalf("agent A first sync = %+v, want one accepted Claude event and 150 total", firstA)
	}
	if len(eventsA) != 1 || eventsA[0].Provider != "anthropic" {
		t.Fatalf("agent A events = %+v, want one Claude event", eventsA)
	}
	assertMetadataOnly(t, eventsA)

	firstB, eventsB, _ := syncAgent(t, ctx, stateB, clientB, adaptersB, "agent-b", true)
	if firstB.Accepted != 1 || firstB.CurrentTotal != 9150 {
		t.Fatalf("agent B first sync = %+v, want one accepted Generic event and 9150 total", firstB)
	}
	if len(eventsB) != 1 || eventsB[0].MachineID != "agent-b" {
		t.Fatalf("agent B events = %+v, want machine attribution", eventsB)
	}
	assertMetadataOnly(t, eventsB)

	// A rescan through the same state store has no pending events. A direct
	// duplicate upload still exercises server-side idempotency without changing
	// lifetime totals.
	unchangedA, _, _ := syncAgent(t, ctx, stateA, clientA, adaptersA, "agent-a", false)
	if unchangedA.Accepted != 0 || unchangedA.Duplicates != 0 {
		t.Fatalf("agent A unchanged rescan uploaded data: %+v", unchangedA)
	}
	duplicate, err := clientB.Ingest(ctx, eventsB)
	if err != nil {
		t.Fatal(err)
	}
	if duplicate.Accepted != 0 || duplicate.Duplicates != 1 || duplicate.CurrentTotal != 9150 {
		t.Fatalf("duplicate upload = %+v, want one duplicate and unchanged total", duplicate)
	}

	// Appending a record advances only agent B's cursor. The first upload is
	// intentionally rejected with a bad token; neither cursor nor fingerprint
	// may advance until the authenticated retry succeeds.
	snapshotBeforeFailure, err := stateB.Snapshot(ctx, "agent-b")
	if err != nil {
		t.Fatal(err)
	}
	if err := appendFile(genericPath, genericLine("agent-b-retry", "2026-08-09T12:01:00Z", 2000)); err != nil {
		t.Fatal(err)
	}
	failedClient := agent.Client{ServerURL: httpServer.URL, Token: "wrong-token"}
	failedEvents, failedReports := collectPending(t, ctx, stateB, adaptersB, "agent-b")
	if len(failedEvents) != 1 || len(failedReports) != 1 {
		t.Fatalf("failed upload collection = events %d reports %d, want one each", len(failedEvents), len(failedReports))
	}
	if _, err := failedClient.Ingest(ctx, failedEvents); err == nil {
		t.Fatal("failed upload unexpectedly succeeded")
	} else if !strings.Contains(err.Error(), "401") {
		t.Fatalf("failed upload error = %v, want authentication failure", err)
	}
	snapshotAfterFailure, err := stateB.Snapshot(ctx, "agent-b")
	if err != nil {
		t.Fatal(err)
	}
	if !sameCursors(snapshotBeforeFailure.Cursors, snapshotAfterFailure.Cursors) {
		t.Fatalf("failed upload advanced cursor: before=%+v after=%+v", snapshotBeforeFailure.Cursors, snapshotAfterFailure.Cursors)
	}
	retry, retryEvents, retryReports := syncAgent(t, ctx, stateB, clientB, adaptersB, "agent-b", false)
	if retry.Accepted != 1 || retry.CurrentTotal != 11150 || len(retryEvents) != 1 || len(retryReports) != 1 {
		t.Fatalf("authenticated retry = %+v events=%d reports=%d, want accepted retry and stage crossing", retry, len(retryEvents), len(retryReports))
	}
	if !retry.Evolved || retry.PreviousStage != 3 || retry.CurrentStage != 4 {
		t.Fatalf("retry evolution = %+v, want stage 3 to 4", retry)
	}

	// Truncation/replacement resets the generic cursor safely. The new record is
	// accepted once while the older server history remains intact.
	if err := os.WriteFile(genericPath, []byte(genericLine("agent-b-rotated", "2026-08-09T12:02:00Z", 3000)), 0o600); err != nil {
		t.Fatal(err)
	}
	rotated, rotatedEvents, _ := syncAgent(t, ctx, stateB, clientB, adaptersB, "agent-b", false)
	if rotated.Accepted != 1 || rotated.CurrentTotal != 14150 || len(rotatedEvents) != 1 || rotatedEvents[0].EventID != "agent-b-rotated" {
		t.Fatalf("rotated source sync = %+v events=%+v, want one replacement event and 14150 total", rotated, rotatedEvents)
	}

	// Restarting from the durable state database does not replay unchanged
	// records and retains the independent machine B cursor.
	if err := stateB.Close(); err != nil {
		t.Fatal(err)
	}
	stateB, err = agent.OpenState(stateBPath)
	if err != nil {
		t.Fatal(err)
	}
	defer stateB.Close()
	restarted, _, _ := syncAgent(t, ctx, stateB, clientB, adaptersB, "agent-b", false)
	if restarted.Accepted != 0 || restarted.Duplicates != 0 {
		t.Fatalf("restart replayed events: %+v", restarted)
	}

	stored, err := store.Events(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(stored) != 4 {
		t.Fatalf("stored events = %d, want Claude + initial/retry/rotation Generic events", len(stored))
	}
	assertMetadataOnly(t, stored)
	if got, err := store.LifetimeTokens(ctx); err != nil || got != 14150 {
		t.Fatalf("lifetime tokens = %d, err=%v, want 14150", got, err)
	}
	snapshot, err := store.Evolution(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Stage != 4 || snapshot.Form != "context-cub" {
		t.Fatalf("final evolution = %+v, want stage 4 context-cub", snapshot)
	}
	for _, event := range stored {
		if event.MachineID != "agent-a" && event.MachineID != "agent-b" {
			t.Fatalf("stored event has unexpected machine attribution: %+v", event)
		}
	}

	// The authenticated dashboard and read APIs expose machine attribution and
	// totals while unauthenticated dashboard access remains rejected.
	unauthenticated, err := http.Get(httpServer.URL + "/")
	if err != nil {
		t.Fatal(err)
	}
	if unauthenticated.StatusCode != http.StatusUnauthorized {
		unauthenticated.Body.Close()
		t.Fatalf("unauthenticated dashboard status = %d, want 401", unauthenticated.StatusCode)
	}
	unauthenticated.Body.Close()

	dashboardRequest, err := http.NewRequest(http.MethodGet, httpServer.URL+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	dashboardRequest.SetBasicAuth("tokemon", systemToken)
	dashboardResponse, err := http.DefaultClient.Do(dashboardRequest)
	if err != nil {
		t.Fatal(err)
	}
	dashboardBody, err := readAndClose(dashboardResponse)
	if err != nil {
		t.Fatal(err)
	}
	if dashboardResponse.StatusCode != http.StatusOK || !bytes.Contains(dashboardBody, []byte("14,150")) {
		t.Fatalf("authenticated dashboard status/body = %d/%d bytes", dashboardResponse.StatusCode, len(dashboardBody))
	}

	machinesRequest, err := http.NewRequest(http.MethodGet, httpServer.URL+"/api/v1/machines", nil)
	if err != nil {
		t.Fatal(err)
	}
	machinesRequest.Header.Set("Authorization", "Bearer "+systemToken)
	machinesResponse, err := http.DefaultClient.Do(machinesRequest)
	if err != nil {
		t.Fatal(err)
	}
	machinesBody, err := readAndClose(machinesResponse)
	if err != nil {
		t.Fatal(err)
	}
	if machinesResponse.StatusCode != http.StatusOK || !bytes.Contains(machinesBody, []byte("agent-a")) || !bytes.Contains(machinesBody, []byte("agent-b")) {
		t.Fatalf("machines status/body = %d/%s", machinesResponse.StatusCode, machinesBody)
	}

}

func syncAgent(t *testing.T, ctx context.Context, state *agent.StateStore, client agent.Client, list []adapters.Adapter, machineID string, heartbeat bool) (database.IngestResult, []usage.Event, []adapters.SourceReport) {
	t.Helper()
	if err := client.Health(ctx); err != nil {
		t.Fatal(err)
	}
	if heartbeat {
		adapterHeartbeats := make([]agent.AdapterHeartbeat, 0, len(list))
		for _, adapter := range list {
			adapterHeartbeats = append(adapterHeartbeats, agent.AdapterHeartbeat{ID: adapter.ID(), Version: "test", Capabilities: adapter.Capabilities()})
		}
		if err := client.Heartbeat(ctx, agent.HeartbeatRequest{MachineID: machineID, AgentVersion: "system-test", OperatingSystem: "test", Architecture: "test", Adapters: adapterHeartbeats}); err != nil {
			t.Fatal(err)
		}
	}
	events, reports := collectPending(t, ctx, state, list, machineID)
	result := database.IngestResult{}
	if len(events) > 0 {
		var err error
		result, err = client.Ingest(ctx, events)
		if err != nil {
			t.Fatal(err)
		}
	}
	if err := state.Commit(ctx, machineID, reports, events, time.Now().UTC()); err != nil {
		t.Fatal(err)
	}
	return result, events, reports
}

func collectPending(t *testing.T, ctx context.Context, state *agent.StateStore, list []adapters.Adapter, machineID string) ([]usage.Event, []adapters.SourceReport) {
	t.Helper()
	snapshot, err := state.Snapshot(ctx, machineID)
	if err != nil {
		t.Fatal(err)
	}
	events, reports := adapters.CollectWithCursors(ctx, list, machineID, snapshot.Cursors)
	if len(events) == 0 {
		return nil, reports
	}
	pending, err := state.Pending(ctx, machineID, events)
	if err != nil {
		t.Fatal(err)
	}
	return pending, reports
}

func assertMetadataOnly(t *testing.T, events []usage.Event) {
	t.Helper()
	payload, err := json.Marshal(events)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"private prompt", "private response", "/private/repo", "private title", "conversation content"} {
		if bytes.Contains(payload, []byte(forbidden)) {
			t.Fatalf("adapter payload contains forbidden %q: %s", forbidden, payload)
		}
	}
	for _, event := range events {
		if strings.Contains(event.Source.Identity, "/") || strings.Contains(event.Source.Identity, "private-project") {
			t.Fatalf("event source identity leaks local path: %+v", event.Source)
		}
		if err := event.Validate(); err != nil {
			t.Fatalf("metadata event invalid: %v", err)
		}
	}
}

func sameCursors(left, right map[string]adapters.Cursor) bool {
	if len(left) != len(right) {
		return false
	}
	for key, value := range left {
		if right[key] != value {
			return false
		}
	}
	return true
}

func appendFile(path, content string) error {
	file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	if _, err := file.WriteString(content); err != nil {
		file.Close()
		return err
	}
	return file.Close()
}

func readAndClose(response *http.Response) ([]byte, error) {
	defer response.Body.Close()
	return io.ReadAll(response.Body)
}

func genericLine(id, timestamp string, total int64) string {
	event := usage.Event{
		SchemaVersion: usage.SchemaVersion,
		EventID:       id,
		Timestamp:     mustTime(timestamp),
		Project:       "/private/repository/project",
		Provider:      "generic-provider",
		Model:         "generic-model",
		Tool:          "generic-jsonl",
		TotalTokens:   usage.Int64(total),
		Currency:      "USD",
		TokenAccuracy: usage.AccuracyReported,
	}
	payload, err := json.Marshal(event)
	if err != nil {
		panic(err)
	}
	return string(payload) + "\n"
}

func claudeFixture() string {
	return strings.Join([]string{
		`{"type":"user","timestamp":"2026-08-09T11:59:59Z","message":{"role":"user","content":"private prompt /private/repo"}}`,
		`{"type":"assistant","timestamp":"2026-08-09T12:00:00Z","sessionId":"agent-a-session","cwd":"/private/repository/project","slug":"private title","message":{"model":"claude-sonnet-4","content":"private response","usage":{"input_tokens":100,"output_tokens":50,"cache_read_input_tokens":0,"cache_creation_input_tokens":0}}}`,
		`{"type":"system","subtype":"turn_duration","timestamp":"2026-08-09T12:00:01Z","sessionId":"agent-a-session","durationMs":1234}`,
	}, "\n") + "\n"
}

func mustTime(value string) time.Time {
	parsed, err := time.Parse(time.RFC3339, value)
	if err != nil {
		panic(err)
	}
	return parsed
}
