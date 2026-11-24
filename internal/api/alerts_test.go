package api

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/tokemon/tokemon/internal/catalog"
	"github.com/tokemon/tokemon/internal/database"
	"github.com/tokemon/tokemon/internal/quota"
	"github.com/tokemon/tokemon/internal/usage"
)

func TestPostQuotaNtfyDualBudgetSelectsHigherUtilizationDimension(t *testing.T) {
	var mu sync.Mutex
	var bodies []string
	ntfySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := io.ReadAll(r.Body)
		mu.Lock()
		bodies = append(bodies, string(b))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
	}))
	defer ntfySrv.Close()

	store, err := database.Open(filepath.Join(t.TempDir(), "ntfy-dual.db"), catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	srv, err := NewWithConfig(store, Config{
		IngestToken:    "secret",
		DashboardToken: testDashboardToken,
		AlertNtfyURL:   ntfySrv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	// Case 1: Cost triggers overage (135%) while tokens are <0.1% (250k / 1B).
	tokensHigh := int64(1_000_000_000)
	costLow := 1.00
	costUsed := 1.35
	if err := srv.postQuotaNtfy(context.Background(), ntfySrv.URL, quotaAlertPayload{
		SchemaVersion: "1",
		Event:         "quota_threshold",
		Threshold:     "overage",
		State:         "over",
		Label:         "Dual Budget Test",
		PeriodStart:   "2026-11-01",
		PeriodEnd:     "2026-12-01",
		Percent:       135.0,
		TokensUsed:    250_000,
		TokensBudget:  &tokensHigh,
		CostUsed:      &costUsed,
		CostBudget:    &costLow,
	}); err != nil {
		t.Fatalf("cost-triggered postQuotaNtfy: %v", err)
	}

	// Case 2: Tokens trigger warning (85%) while cost is 10% ($1.00 / $10.00).
	tokensLow := int64(1_000_000)
	costHigh := 10.00
	costUsedSmall := 1.00
	if err := srv.postQuotaNtfy(context.Background(), ntfySrv.URL, quotaAlertPayload{
		SchemaVersion: "1",
		Event:         "quota_threshold",
		Threshold:     "warning",
		State:         "ok",
		Label:         "Dual Budget Tokens",
		PeriodStart:   "2026-11-01",
		PeriodEnd:     "2026-12-01",
		Percent:       85.0,
		TokensUsed:    850_000,
		TokensBudget:  &tokensLow,
		CostUsed:      &costUsedSmall,
		CostBudget:    &costHigh,
	}); err != nil {
		t.Fatalf("token-triggered postQuotaNtfy: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(bodies) != 2 {
		t.Fatalf("received %d ntfy messages, want 2", len(bodies))
	}
	if !strings.Contains(bodies[0], "135.0% ($1.35 / $1.00 · 250,000 / 1,000,000,000 tokens)") {
		t.Fatalf("cost-triggered ntfy body = %q, expected cost primary + token context", bodies[0])
	}
	if !strings.Contains(bodies[1], "85.0% (850,000 / 1,000,000 tokens · $1.00 / $10.00)") {
		t.Fatalf("token-triggered ntfy body = %q, expected tokens primary + cost context", bodies[1])
	}
}

func TestQuotaAlertSWRRecheckAwaitsSlowRefreshOver200ms(t *testing.T) {
	var mu sync.Mutex
	var payloads []quotaAlertPayload
	notify := make(chan struct{}, 8)
	webhookSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var p quotaAlertPayload
		if err := json.Unmarshal(body, &p); err == nil {
			mu.Lock()
			payloads = append(payloads, p)
			mu.Unlock()
			select {
			case notify <- struct{}{}:
			default:
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer webhookSrv.Close()

	store, err := database.OpenWithLocation(filepath.Join(t.TempDir(), "slow-swr-alerts.db"), catalog.Empty(), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	budget := int64(1000)
	if err := store.UpsertQuota(context.Background(), quota.Quota{
		Provider: "openai", Label: "Slow SWR Cap", ResetDay: 1,
		Budget: quota.Budget{TokensPerMonth: &budget},
	}); err != nil {
		t.Fatal(err)
	}

	// Seed 500 tokens (50%, below 80% warning) and warm the QuotaStatus SWR cache.
	start, _ := quota.PeriodFor(time.Now(), 1, time.UTC)
	if _, err := store.Ingest(context.Background(), []usage.Event{{
		SchemaVersion: usage.SchemaVersion, EventID: "slow-swr-1", Timestamp: start.Add(time.Hour),
		MachineID: "box", Provider: "openai", Model: "gpt-5", Tool: "codex", SessionID: "s-slow",
		TotalTokens: usage.Int64(500), TokenAccuracy: usage.AccuracyReported,
		Source: usage.Source{Adapter: "codex", AdapterVersion: "1"},
	}}); err != nil {
		t.Fatal(err)
	}
	statuses := store.QuotaStatus(context.Background(), time.Now())
	if len(statuses) != 1 || statuses[0].TokensUsed != 500 {
		t.Fatalf("warm QuotaStatus = %+v, want 500 tokens", statuses)
	}

	// Inject >200ms SQLite refresh delay (exceeding the old hardcoded 150ms settle sleep).
	store.SetQuotaRefreshDelayForTest(250 * time.Millisecond)

	srv, err := NewWithConfig(store, Config{
		IngestToken:     "secret",
		DashboardToken:  testDashboardToken,
		AlertWebhookURL: webhookSrv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	// Ingest 400 more tokens (900/1000 = 90% warning). Immediate check sees stale 50%,
	// then the post-coalesce SWR recheck must await the 250ms refresh via QuotaStatusFresh
	// and still fire the warning alert.
	payload, _ := json.Marshal(batchRequest{Events: []usage.Event{{
		SchemaVersion: usage.SchemaVersion, EventID: "slow-swr-2", Timestamp: start.Add(2 * time.Hour),
		MachineID: "box", Provider: "openai", Model: "gpt-5", Tool: "codex", SessionID: "s-slow",
		TotalTokens: usage.Int64(400), TokenAccuracy: usage.AccuracyReported,
		Source: usage.Source{Adapter: "codex", AdapterVersion: "1"},
	}}})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/events/batch", bytes.NewReader(payload))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer secret")
	rec := httptest.NewRecorder()
	testHandler(srv).ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("ingest = %d: %s", rec.Code, rec.Body.String())
	}

	select {
	case <-notify:
	case <-time.After(5 * time.Second):
		t.Fatal("timed out waiting for quota alert across 250ms SWR refresh delay")
	}

	mu.Lock()
	defer mu.Unlock()
	if len(payloads) != 1 || payloads[0].Threshold != "warning" || payloads[0].TokensUsed != 900 {
		t.Fatalf("alerts = %+v, want 1 warning alert at 900 tokens", payloads)
	}
}
