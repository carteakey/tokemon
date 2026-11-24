package api

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"encoding/xml"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tokemon/tokemon/internal/catalog"
	"github.com/tokemon/tokemon/internal/database"
	"github.com/tokemon/tokemon/internal/quota"
	"github.com/tokemon/tokemon/internal/usage"
)

func TestM5BAnalyticsCacheSavingsEffectiveRateFromToAndTokedexLink(t *testing.T) {
	inRate := 3.0
	crRate := 0.30
	outRate := 15.0
	cat := catalog.MustNew(map[string]catalog.Model{
		"claude-sonnet-4": {
			Provider: "anthropic", DisplayName: "Claude Sonnet 4", Aliases: []string{"claude-sonnet-4"}, Tier: "S",
			Pricing: catalog.Pricing{
				Currency: "USD", Mode: "standard", VerifiedAt: "2026-11-20", Source: "https://example.com/pricing",
				InputPricePerMillion:     &inRate,
				CacheReadPricePerMillion: &crRate,
				OutputPricePerMillion:    &outRate,
			},
		},
		"mystery-unknown": {
			Provider: "openai", DisplayName: "Mystery Unknown", Aliases: []string{"mystery-unknown"}, Tier: "A",
			Pricing: catalog.Pricing{Currency: "USD", Mode: "unknown", VerifiedAt: "2026-11-20"},
		},
	})
	store, err := database.OpenWithLocation(filepath.Join(t.TempDir(), "m5b-analytics.db"), cat, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Seed 2 days in November so custom from/to with period=24h renders "Nov DD" day labels.
	day1 := time.Date(2026, 11, 2, 12, 0, 0, 0, time.UTC)
	day2 := time.Date(2026, 11, 3, 12, 0, 0, 0, time.UTC)
	events := []usage.Event{
		{
			SchemaVersion: usage.SchemaVersion, EventID: "m5b-a1", Timestamp: day1,
			MachineID: "m1", Project: "proj-priced", Provider: "anthropic", Model: "claude-sonnet-4",
			Tool: "claude-code", SessionID: "s1",
			InputTokens: usage.Int64(1_000_000), CacheReadTokens: usage.Int64(500_000), OutputTokens: usage.Int64(100_000),
			TotalTokens: usage.Int64(1_600_000), Currency: "USD", TokenAccuracy: usage.AccuracyReported,
			Source: usage.Source{Adapter: "claude-code", AdapterVersion: "1"},
		},
		{
			SchemaVersion: usage.SchemaVersion, EventID: "m5b-a2", Timestamp: day2,
			MachineID: "m1", Project: "proj-unpriced", Provider: "openai", Model: "mystery-unknown",
			Tool: "codex", SessionID: "s2",
			InputTokens: usage.Int64(200_000), OutputTokens: usage.Int64(100_000),
			TotalTokens: usage.Int64(300_000), Currency: "USD", TokenAccuracy: usage.AccuracyReported,
			Source: usage.Source{Adapter: "codex", AdapterVersion: "1"},
		},
	}
	if _, err := store.Ingest(context.Background(), events); err != nil {
		t.Fatal(err)
	}

	enabledServer, err := NewWithConfig(store, Config{IngestToken: "secret", DashboardToken: testDashboardToken, EnableTokedex: true})
	if err != nil {
		t.Fatal(err)
	}
	defer enabledServer.Close()

	rec := httptest.NewRecorder()
	testHandler(enabledServer).ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/analytics?period=24h&dimension=models&from=2026-11-01&to=2026-11-04", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /analytics = %d: %s", rec.Code, rec.Body.String())
	}
	body := rec.Body.String()

	// Cache savings ($1.35 saved on 500K cached tokens: 0.5 * (3.00 - 0.30) = 1.35) & effective $/1M.
	for _, want := range []string{
		"$1.35 saved",
		"/1M",
		`<th scope="col">Cache hit</th>`,
		`<th scope="col">Cost</th>`,
		`<th scope="col">$/1M</th>`,
		`<th scope="col">Saved</th>`,
		`href="/tokedex#uncataloged-title"`,
		`id="analytics-from"`,
		`id="analytics-to"`,
		`value="2026-11-01"`,
		`value="2026-11-04"`,
		`Nov 2`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("/analytics missing %q", want)
		}
	}
	if strings.Contains(body, `window-link active`) || strings.Contains(body, `aria-current="page">24H`) {
		t.Error("window-link remained active while custom from/to was active")
	}
	if strings.Contains(body, `<span class="trend-label">2</span>`) {
		t.Error("period=24h with custom from/to truncated Nov 2 tick label to '2'")
	}
	if strings.Contains(body, "$0.00 saved") || strings.Contains(body, "$0.00/1M") {
		t.Error("unpriced/ineligible values fabricated $0.00 saved or $0.00/1M")
	}
	if regexp.MustCompile(`(?i)\bknown\b`).MatchString(body) {
		t.Fatalf("/analytics violated no-standalone-known invariant")
	}

	// With EnableTokedex=false, unpriced_share must NOT link to /tokedex#uncataloged-title.
	disabledServer, err := NewWithConfig(store, Config{IngestToken: "secret", DashboardToken: testDashboardToken, EnableTokedex: false})
	if err != nil {
		t.Fatal(err)
	}
	defer disabledServer.Close()
	recDis := httptest.NewRecorder()
	testHandler(disabledServer).ServeHTTP(recDis, httptest.NewRequest(http.MethodGet, "/analytics?period=all&dimension=models", nil))
	if recDis.Code != http.StatusOK {
		t.Fatalf("disabled GET /analytics = %d", recDis.Code)
	}
	if strings.Contains(recDis.Body.String(), "/tokedex#uncataloged-title") {
		t.Error("disabled Tokedex still linked unpriced_share to /tokedex#uncataloged-title")
	}
}

func TestM5BSettingsQuotaCRUDBackupAgeAndOverviewPaceBar(t *testing.T) {
	yamlBudget := int64(10_000_000)
	yamlQuotas := []quota.Quota{
		{Provider: "anthropic", Label: "YAML Anthropic", ResetDay: 1, Budget: quota.Budget{TokensPerMonth: &yamlBudget}},
	}
	tmpDir := t.TempDir()
	dbPath := filepath.Join(tmpDir, "tokemon.db")
	backupDir := filepath.Join(tmpDir, "backups")
	if err := os.MkdirAll(backupDir, 0o755); err != nil {
		t.Fatal(err)
	}

	store, err := database.OpenWithLocation(dbPath, catalog.Empty(), time.UTC, yamlQuotas...)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Seed a real SQLite backup so LatestBackupInfo finds it.
	if _, err := database.CreateBackup(context.Background(), database.BackupOptions{
		DatabasePath:   dbPath,
		DestinationDir: backupDir,
		Retention:      10,
	}); err != nil {
		t.Fatal(err)
	}

	start, _ := quota.PeriodFor(time.Now(), 1, time.UTC)
	if _, err := store.Ingest(context.Background(), []usage.Event{{
		SchemaVersion: usage.SchemaVersion, EventID: "m5b-q1", Timestamp: start.Add(time.Hour),
		MachineID: "box", Project: "proj", Provider: "anthropic", Model: "claude-sonnet-4",
		Tool: "claude-code", SessionID: "sess-q1",
		InputTokens: usage.Int64(2_000_000), OutputTokens: usage.Int64(500_000), TotalTokens: usage.Int64(2_500_000),
		TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "claude-code", AdapterVersion: "1"},
	}}); err != nil {
		t.Fatal(err)
	}

	server, err := NewWithConfig(store, Config{
		IngestToken:     "secret",
		DashboardToken:  testDashboardToken,
		EnableTokedex:   true,
		DatabasePath:    dbPath,
		BackupDir:       backupDir,
		BackupInterval:  24 * time.Hour,
		BackupRetention: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()
	handler := testHandler(server)

	// 1. CSRF check on POST /settings/quotas.
	noCSRF := httptest.NewRequest(http.MethodPost, "/settings/quotas", strings.NewReader("provider=openai&tokens_per_month=1000&reset_day=1"))
	noCSRF.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	noCSRFRec := httptest.NewRecorder()
	handler.ServeHTTP(noCSRFRec, noCSRF)
	if noCSRFRec.Code != http.StatusForbidden || !strings.Contains(noCSRFRec.Body.String(), `role="alert"`) {
		t.Fatalf("missing CSRF POST /settings/quotas = %d: %s", noCSRFRec.Code, noCSRFRec.Body.String())
	}

	// 2. Warm / overview cache initially shows YAML quota.
	dashWarm := httptest.NewRecorder()
	handler.ServeHTTP(dashWarm, httptest.NewRequest(http.MethodGet, "/", nil))
	if dashWarm.Code != http.StatusOK || !strings.Contains(dashWarm.Body.String(), "YAML Anthropic") {
		t.Fatalf("initial / missing YAML Anthropic pace bar: %d", dashWarm.Code)
	}

	// 3. GET /settings shows YAML config badge and backup age without leaking raw BackupInfo.Path.
	setRec := httptest.NewRecorder()
	handler.ServeHTTP(setRec, httptest.NewRequest(http.MethodGet, "/settings", nil))
	if setRec.Code != http.StatusOK {
		t.Fatalf("GET /settings = %d", setRec.Code)
	}
	setBody := setRec.Body.String()
	for _, want := range []string{"YAML config", "Override in UI", "Latest backup", "Every 24h0m0s (retain 10)"} {
		if !strings.Contains(setBody, want) {
			t.Errorf("/settings missing %q", want)
		}
	}
	if strings.Contains(setBody, backupDir) || strings.Contains(setBody, dbPath) {
		t.Fatal("/settings leaked raw filesystem backup or database path")
	}

	// 4. Validation error returns 400 with role="alert".
	badForm := url.Values{
		"csrf_token": {server.csrfToken()},
		"action":     {"upsert"},
		"provider":   {"openai"},
		"reset_day":  {"99"},
	}
	badReq := httptest.NewRequest(http.MethodPost, "/settings/quotas", strings.NewReader(badForm.Encode()))
	badReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	badRec := httptest.NewRecorder()
	handler.ServeHTTP(badRec, badReq)
	if badRec.Code != http.StatusBadRequest || !strings.Contains(badRec.Body.String(), `role="alert"`) {
		t.Fatalf("invalid quota POST = %d, want 400 with role=alert", badRec.Code)
	}

	// 4b. Duplicate collision via action=create rejects YAML and SQLite collisions (case-insensitive) with 409 + role="alert".
	for _, tc := range []struct {
		name     string
		provider string
		model    string
		wantText string
	}{
		{name: "yaml_collision_case_insensitive", provider: "Anthropic", model: "", wantText: "YAML Anthropic"},
	} {
		dupForm := url.Values{
			"csrf_token":       {server.csrfToken()},
			"action":           {"create"},
			"provider":         {tc.provider},
			"model":            {tc.model},
			"label":            {"Duplicate Attempt"},
			"reset_day":        {"1"},
			"tokens_per_month": {"999"},
		}
		dupReq := httptest.NewRequest(http.MethodPost, "/settings/quotas", strings.NewReader(dupForm.Encode()))
		dupReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		dupRec := httptest.NewRecorder()
		handler.ServeHTTP(dupRec, dupReq)
		if dupRec.Code != http.StatusConflict || !strings.Contains(dupRec.Body.String(), `role="alert"`) {
			t.Fatalf("%s POST /settings/quotas = %d, want 409 with role=alert", tc.name, dupRec.Code)
		}
		if !strings.Contains(dupRec.Body.String(), tc.wantText) {
			t.Fatalf("%s POST /settings/quotas did not preserve existing %q", tc.name, tc.wantText)
		}
	}

	// 5. Override YAML quota in SQLite -> 303 redirect, drops overview HTML cache, updates / immediately.
	overrideForm := url.Values{
		"csrf_token":       {server.csrfToken()},
		"action":           {"upsert"},
		"provider":         {"anthropic"},
		"label":            {"UI Override Anthropic"},
		"reset_day":        {"1"},
		"tokens_per_month": {"5000000"},
	}
	ovReq := httptest.NewRequest(http.MethodPost, "/settings/quotas", strings.NewReader(overrideForm.Encode()))
	ovReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	ovRec := httptest.NewRecorder()
	handler.ServeHTTP(ovRec, ovReq)
	if ovRec.Code != http.StatusSeeOther || ovRec.Header().Get("Location") != "/settings?quota_saved=override" {
		t.Fatalf("override POST = %d loc=%q", ovRec.Code, ovRec.Header().Get("Location"))
	}
	dashAfterOverride := httptest.NewRecorder()
	handler.ServeHTTP(dashAfterOverride, httptest.NewRequest(http.MethodGet, "/", nil))
	if !strings.Contains(dashAfterOverride.Body.String(), "UI Override Anthropic") {
		t.Fatal("/ did not drop overview HTML cache after quota override")
	}

	// 6. Add SQLite-only quota via action=create and check settings badges + SQLite collision rejection.
	addForm := url.Values{
		"csrf_token":       {server.csrfToken()},
		"action":           {"create"},
		"provider":         {"openai"},
		"model":            {"gpt-5.4"},
		"label":            {"UI OpenAI Cap"},
		"reset_day":        {"1"},
		"tokens_per_month": {"1000000"},
	}
	addReq := httptest.NewRequest(http.MethodPost, "/settings/quotas", strings.NewReader(addForm.Encode()))
	addReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	addRec := httptest.NewRecorder()
	handler.ServeHTTP(addRec, addReq)
	if addRec.Code != http.StatusSeeOther || addRec.Header().Get("Location") != "/settings?quota_saved=1" {
		t.Fatalf("add quota POST = %d loc=%q", addRec.Code, addRec.Header().Get("Location"))
	}
	dupSQLiteForm := url.Values{
		"csrf_token":       {server.csrfToken()},
		"action":           {"create"},
		"provider":         {"OpenAI"},
		"model":            {"GPT-5.4"},
		"label":            {"Duplicate SQLite"},
		"reset_day":        {"1"},
		"tokens_per_month": {"2000000"},
	}
	dupSQLiteReq := httptest.NewRequest(http.MethodPost, "/settings/quotas", strings.NewReader(dupSQLiteForm.Encode()))
	dupSQLiteReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	dupSQLiteRec := httptest.NewRecorder()
	handler.ServeHTTP(dupSQLiteRec, dupSQLiteReq)
	if dupSQLiteRec.Code != http.StatusConflict || !strings.Contains(dupSQLiteRec.Body.String(), `role="alert"`) {
		t.Fatalf("sqlite collision POST /settings/quotas = %d, want 409 with role=alert", dupSQLiteRec.Code)
	}
	if !strings.Contains(dupSQLiteRec.Body.String(), "UI OpenAI Cap") {
		t.Fatal("sqlite collision overwrote UI OpenAI Cap")
	}
	setAfterAdd := httptest.NewRecorder()
	handler.ServeHTTP(setAfterAdd, httptest.NewRequest(http.MethodGet, "/settings", nil))
	for _, want := range []string{"UI override", "Reset to YAML", "UI (SQLite)", "Delete"} {
		if !strings.Contains(setAfterAdd.Body.String(), want) {
			t.Errorf("/settings after override+add missing %q", want)
		}
	}

	// 7. Reset override back to YAML.
	resetForm := url.Values{
		"csrf_token":     {server.csrfToken()},
		"action":         {"delete"},
		"provider":       {"anthropic"},
		"overrides_yaml": {"1"},
	}
	resetReq := httptest.NewRequest(http.MethodPost, "/settings/quotas", strings.NewReader(resetForm.Encode()))
	resetReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resetRec := httptest.NewRecorder()
	handler.ServeHTTP(resetRec, resetReq)
	if resetRec.Code != http.StatusSeeOther || resetRec.Header().Get("Location") != "/settings?quota_saved=reset" {
		t.Fatalf("reset override POST = %d loc=%q", resetRec.Code, resetRec.Header().Get("Location"))
	}
	dashAfterReset := httptest.NewRecorder()
	handler.ServeHTTP(dashAfterReset, httptest.NewRequest(http.MethodGet, "/", nil))
	if !strings.Contains(dashAfterReset.Body.String(), "YAML Anthropic") {
		t.Fatal("/ did not restore YAML Anthropic after Reset to YAML")
	}

	// 8. Graceful backup error fallback when backupDir is a regular file.
	fileAsDir := filepath.Join(tmpDir, "not-a-dir")
	if err := os.WriteFile(fileAsDir, []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	errServer, err := NewWithConfig(store, Config{
		IngestToken: "secret", DashboardToken: testDashboardToken, DatabasePath: dbPath, BackupDir: fileAsDir,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer errServer.Close()
	errSet := httptest.NewRecorder()
	testHandler(errServer).ServeHTTP(errSet, httptest.NewRequest(http.MethodGet, "/settings", nil))
	if errSet.Code != http.StatusOK || !strings.Contains(errSet.Body.String(), "Unavailable") {
		t.Fatalf("backup error fallback /settings = %d, want 200 with Unavailable", errSet.Code)
	}
}

func TestM5BDashboardPaceBarWarmCacheAndStreaksEdgeCases(t *testing.T) {
	// 1. Unknown-token-only DB: ActiveDays > 0, PeakDay == nil -> renders "—" cleanly, zero \bknown\b.
	storeUnknown, err := database.OpenWithLocation(filepath.Join(t.TempDir(), "u.db"), catalog.Empty(), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	defer storeUnknown.Close()
	if _, err := storeUnknown.Ingest(context.Background(), []usage.Event{{
		SchemaVersion: usage.SchemaVersion, EventID: "u-only", Timestamp: time.Now().UTC(),
		MachineID: "box", Provider: "openai", Model: "gpt-5", Tool: "codex", SessionID: "su",
		TokenAccuracy: usage.AccuracyUnknown, Source: usage.Source{Adapter: "codex", AdapterVersion: "1"},
	}}); err != nil {
		t.Fatal(err)
	}
	srvUnknown, err := newTestServer(storeUnknown, "")
	if err != nil {
		t.Fatal(err)
	}
	defer srvUnknown.Close()
	recU := httptest.NewRecorder()
	testHandler(srvUnknown).ServeHTTP(recU, httptest.NewRequest(http.MethodGet, "/", nil))
	if recU.Code != http.StatusOK {
		t.Fatalf("GET / unknown-only = %d", recU.Code)
	}
	bodyU := recU.Body.String()
	for _, want := range []string{`class="activity-streaks"`, `Streak <strong>1d</strong>`, `Best <strong>1d</strong>`, `Peak <strong class="unknown-value">—</strong>`} {
		if !strings.Contains(bodyU, want) {
			t.Errorf("unknown-only / missing %q", want)
		}
	}
	if regexp.MustCompile(`(?i)\bknown\b`).MatchString(bodyU) {
		t.Fatal("/ violated no-standalone-known invariant")
	}

	// 2. Active quota pace bar rendered in warm overviewCacheEntry.html serves DB-free after store.Close().
	budget := int64(1000)
	storeQuota, err := database.OpenWithLocation(filepath.Join(t.TempDir(), "q.db"), catalog.Empty(), time.UTC,
		quota.Quota{Provider: "openai", Label: "Warm Pace Cap", ResetDay: 1, Budget: quota.Budget{TokensPerMonth: &budget}},
	)
	if err != nil {
		t.Fatal(err)
	}
	start, _ := quota.PeriodFor(time.Now(), 1, time.UTC)
	if _, err := storeQuota.Ingest(context.Background(), []usage.Event{{
		SchemaVersion: usage.SchemaVersion, EventID: "q-warm", Timestamp: start.Add(time.Hour),
		MachineID: "box", Provider: "openai", Model: "gpt-5", Tool: "codex", SessionID: "sq",
		TotalTokens: usage.Int64(500), TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "codex", AdapterVersion: "1"},
	}}); err != nil {
		t.Fatal(err)
	}
	srvQuota, err := newTestServer(storeQuota, "")
	if err != nil {
		t.Fatal(err)
	}
	defer srvQuota.Close()
	recCold := httptest.NewRecorder()
	testHandler(srvQuota).ServeHTTP(recCold, httptest.NewRequest(http.MethodGet, "/", nil))
	if recCold.Code != http.StatusOK || !strings.Contains(recCold.Body.String(), "Warm Pace Cap") {
		t.Fatalf("cold / with quota = %d", recCold.Code)
	}
	_ = storeQuota.Close()
	recWarm := httptest.NewRecorder()
	testHandler(srvQuota).ServeHTTP(recWarm, httptest.NewRequest(http.MethodGet, "/", nil))
	if recWarm.Code != http.StatusOK || !strings.Contains(recWarm.Body.String(), "Warm Pace Cap") {
		t.Fatalf("warm / after store.Close = %d", recWarm.Code)
	}
}

func TestM5BAlertValidationDeduplicationAndPrivacy(t *testing.T) {
	store, err := database.OpenWithLocation(filepath.Join(t.TempDir(), "alerts.db"), catalog.Empty(), time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Invalid alert URL schemes/hosts rejected at startup.
	for _, bad := range []string{"file:///etc/passwd", "ftp://example.com/hook", "https://", "https://bad\nhost/path"} {
		if _, err := NewWithConfig(store, Config{IngestToken: "secret", AlertWebhookURL: bad}); err == nil {
			t.Fatalf("expected error for invalid webhook URL %q", bad)
		}
		if _, err := NewWithConfig(store, Config{IngestToken: "secret", AlertNtfyURL: bad}); err == nil {
			t.Fatalf("expected error for invalid ntfy URL %q", bad)
		}
	}

	var mu sync.Mutex
	var webhookPayloads []quotaAlertPayload
	var ntfyBodies []string
	var ntfyTitles []string
	alertNotify := make(chan struct{}, 16)

	webhookSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var p quotaAlertPayload
		if err := json.Unmarshal(body, &p); err == nil {
			mu.Lock()
			webhookPayloads = append(webhookPayloads, p)
			mu.Unlock()
		}
		w.WriteHeader(http.StatusOK)
		alertNotify <- struct{}{}
	}))
	defer webhookSrv.Close()

	ntfySrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		ntfyBodies = append(ntfyBodies, string(body))
		ntfyTitles = append(ntfyTitles, r.Header.Get("Title"))
		mu.Unlock()
		w.WriteHeader(http.StatusOK)
		alertNotify <- struct{}{}
	}))
	defer ntfySrv.Close()

	// Configure a 1,000 token monthly quota via SQLite.
	budget := int64(1000)
	if err := store.UpsertQuota(context.Background(), quota.Quota{
		Provider: "openai", Label: "Monthly OpenAI", ResetDay: 1,
		Budget: quota.Budget{TokensPerMonth: &budget},
	}); err != nil {
		t.Fatal(err)
	}

	srv, err := NewWithConfig(store, Config{
		IngestToken:     "secret",
		DashboardToken:  testDashboardToken,
		AlertWebhookURL: webhookSrv.URL,
		AlertNtfyURL:    ntfySrv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	handler := testHandler(srv)

	ingestBatch := func(id string, tokens int64) {
		t.Helper()
		payload, _ := json.Marshal(batchRequest{Events: []usage.Event{{
			SchemaVersion: usage.SchemaVersion, EventID: id, Timestamp: time.Now().UTC(),
			MachineID: "secret-machine-name", Project: "secret-project-name",
			Provider: "openai", Model: "gpt-5", Tool: "codex", SessionID: "secret-session-id",
			TotalTokens: usage.Int64(tokens), TokenAccuracy: usage.AccuracyReported,
			Source: usage.Source{Adapter: "codex", AdapterVersion: "1"},
		}}})
		req := httptest.NewRequest(http.MethodPost, "/api/v1/events/batch", bytes.NewReader(payload))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer secret")
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("ingest %s = %d: %s", id, rec.Code, rec.Body.String())
		}
	}

	waitAlerts := func(wantTotal int) {
		t.Helper()
		deadline := time.After(4 * time.Second)
		for {
			mu.Lock()
			got := len(webhookPayloads) + len(ntfyBodies)
			mu.Unlock()
			if got >= wantTotal {
				return
			}
			select {
			case <-alertNotify:
			case <-deadline:
				t.Fatalf("timed out waiting for %d alerts (got %d)", wantTotal, got)
			}
		}
	}

	// 85% -> fires warning once to webhook + ntfy (2 requests).
	ingestBatch("alert-1", 850)
	waitAlerts(2)

	// Another small ingest still at 90% -> deduplicated (no new alerts).
	ingestBatch("alert-2", 50)
	time.Sleep(150 * time.Millisecond)
	mu.Lock()
	if len(webhookPayloads) != 1 || webhookPayloads[0].Threshold != "warning" {
		t.Fatalf("expected 1 warning webhook, got %+v", webhookPayloads)
	}
	rawWebhook, _ := json.Marshal(webhookPayloads[0])
	rawNtfy := ntfyBodies[0]
	mu.Unlock()

	for _, forbidden := range []string{"secret-machine-name", "secret-project-name", "secret-session-id", "/Users/", "/tmp/"} {
		if strings.Contains(string(rawWebhook), forbidden) || strings.Contains(rawNtfy, forbidden) {
			t.Fatalf("alert payload leaked sensitive field %q: webhook=%s ntfy=%s", forbidden, string(rawWebhook), rawNtfy)
		}
	}

	// Cross 100% -> fires overage once.
	ingestBatch("alert-3", 200)
	waitAlerts(4)
	mu.Lock()
	if len(webhookPayloads) != 2 || webhookPayloads[1].Threshold != "overage" || webhookPayloads[1].State != "over" {
		t.Fatalf("expected overage webhook, got %+v", webhookPayloads)
	}
	if !strings.Contains(ntfyTitles[1], "overage") {
		t.Fatalf("expected overage ntfy title, got %q", ntfyTitles[1])
	}
	mu.Unlock()
}

func TestM5BTokedexWhatIfDetailAndShareCardSVG(t *testing.T) {
	inPrice := 3.0
	outPrice := 15.0
	// Standard model intentionally omits cache read/write prices to exercise fallback annotation.
	cat := catalog.MustNew(map[string]catalog.Model{
		"claude-sonnet-4": {
			Provider: "anthropic", DisplayName: "Claude Sonnet 4", Aliases: []string{"claude-sonnet-4"}, Tier: "S",
			Pricing: catalog.Pricing{Currency: "USD", Mode: "standard", VerifiedAt: "2026-11-20", Source: "https://example.com/pricing", InputPricePerMillion: &inPrice, OutputPricePerMillion: &outPrice},
		},
		"gpt-5": {
			Provider: "openai", DisplayName: "GPT-5", Aliases: []string{"gpt-5", "openai/gpt-5"}, Tier: "A",
			Pricing: catalog.Pricing{Currency: "USD", Mode: "unknown", VerifiedAt: "2026-11-20"},
		},
		"zero-model": {
			Provider: "openai", DisplayName: "Zero Model", Aliases: []string{"zero-model"}, Tier: "B",
			Pricing: catalog.Pricing{Currency: "USD", Mode: "standard", VerifiedAt: "2026-11-20", Source: "https://example.com/pricing", InputPricePerMillion: &inPrice, OutputPricePerMillion: &outPrice},
		},
	})
	store, err := database.OpenWithLocation(filepath.Join(t.TempDir(), "tokedex.db"), cat, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Now().UTC()
	if _, err := store.Ingest(context.Background(), []usage.Event{
		{
			SchemaVersion: usage.SchemaVersion, EventID: "td-1", Timestamp: now.Add(-time.Hour),
			MachineID: "private-laptop", Project: "private-repo",
			Provider: "anthropic", Model: "claude-sonnet-4", Tool: "claude-desktop", SessionID: "private-session-xyz",
			InputTokens: usage.Int64(100_000), CacheReadTokens: usage.Int64(50_000), OutputTokens: usage.Int64(10_000),
			TotalTokens: usage.Int64(160_000), Currency: "USD", TokenAccuracy: usage.AccuracyReported,
			Source: usage.Source{Adapter: "claude-code", AdapterVersion: "1"},
		},
		{
			SchemaVersion: usage.SchemaVersion, EventID: "td-2", Timestamp: now.Add(-2 * time.Hour),
			MachineID: "private-laptop", Project: "private-repo",
			Provider: "openai", Model: "openai/gpt-5", Tool: "codex", SessionID: "private-session-abc",
			InputTokens: usage.Int64(5_000), OutputTokens: usage.Int64(1_000), TotalTokens: usage.Int64(6_000),
			Currency: "USD", TokenAccuracy: usage.AccuracyReported,
			Source: usage.Source{Adapter: "codex", AdapterVersion: "1"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	srv, err := NewWithConfig(store, Config{IngestToken: "secret", DashboardToken: testDashboardToken, EnableTokedex: true})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	handler := testHandler(srv)

	// 1. /tokedex what-if panel & cache fallback note.
	catRec := httptest.NewRecorder()
	handler.ServeHTTP(catRec, httptest.NewRequest(http.MethodGet, "/tokedex", nil))
	if catRec.Code != http.StatusOK {
		t.Fatalf("GET /tokedex = %d", catRec.Code)
	}
	catBody := catRec.Body.String()
	for _, want := range []string{
		`id="whatif-title">30-day what-if comparison`,
		`id="whatif-baseline"`,
		`id="whatif-scale"`,
		`cache @ input rate`,
		`href="/tokedex/claude-sonnet-4"`,
		`href="/tokedex/gpt-5"`,
	} {
		if !strings.Contains(catBody, want) {
			t.Errorf("/tokedex missing %q", want)
		}
	}

	// 2. /tokedex/{model...}: canonical, slash alias, zero-usage, and 404.
	for _, tc := range []struct {
		path   string
		status int
		wants  []string
	}{
		{"/tokedex/claude-sonnet-4", http.StatusOK, []string{"Claude Sonnet 4", "Claude Desktop", "12-week usage trend", "Machine distribution"}},
		{"/tokedex/openai/gpt-5", http.StatusOK, []string{"GPT-5", "Requested via alias", "openai/gpt-5", "Rates not published yet"}},
		{"/tokedex/openai%2Fgpt-5", http.StatusOK, []string{"GPT-5", "openai/gpt-5"}},
		{"/tokedex/zero-model", http.StatusOK, []string{"Zero Model", "No usage recorded yet", "No usage recorded in the last 12 weeks.", `aria-label="Model machine distribution"`}},
		{"/tokedex/does-not-exist", http.StatusNotFound, nil},
		{"/tokedex/%2e%2e", http.StatusNotFound, nil},
		{"/tokedex/%2e", http.StatusNotFound, nil},
		{"/tokedex/", http.StatusNotFound, nil},
	} {
		rec := httptest.NewRecorder()
		handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, tc.path, nil))
		if rec.Code != tc.status {
			t.Errorf("GET %s = %d, want %d", tc.path, rec.Code, tc.status)
		}
		for _, want := range tc.wants {
			if !strings.Contains(rec.Body.String(), want) {
				t.Errorf("GET %s missing %q", tc.path, want)
			}
		}
	}

	// 3. Disabled Tokedex returns 404 for detail route after auth.
	disSrv, err := NewWithConfig(store, Config{IngestToken: "secret", DashboardToken: testDashboardToken, EnableTokedex: false})
	if err != nil {
		t.Fatal(err)
	}
	defer disSrv.Close()
	disRec := httptest.NewRecorder()
	testHandler(disSrv).ServeHTTP(disRec, httptest.NewRequest(http.MethodGet, "/tokedex/claude-sonnet-4", nil))
	if disRec.Code != http.StatusNotFound {
		t.Fatalf("disabled GET /tokedex/claude-sonnet-4 = %d, want 404", disRec.Code)
	}

	// 4. Share card /api/v1/card.svg auth, gzip, XML validity, and strict privacy.
	unauthReq := httptest.NewRequest(http.MethodGet, "/api/v1/card.svg", nil)
	unauthRec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(unauthRec, unauthReq)
	if unauthRec.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated /api/v1/card.svg = %d, want 401", unauthRec.Code)
	}

	pwSrv := passwordAccountServer(t, store)
	defer pwSrv.Close()
	browserReq := httptest.NewRequest(http.MethodGet, "/api/v1/card.svg", nil)
	browserReq.Header.Set("Accept", "text/html")
	browserRec := httptest.NewRecorder()
	pwSrv.Handler().ServeHTTP(browserRec, browserReq)
	if browserRec.Code != http.StatusSeeOther || !strings.HasPrefix(browserRec.Header().Get("Location"), "/login?next=") {
		t.Fatalf("browser unauthenticated /api/v1/card.svg = %d loc=%q", browserRec.Code, browserRec.Header().Get("Location"))
	}

	cardReq := httptest.NewRequest(http.MethodGet, "/api/v1/card.svg", nil)
	cardReq.Header.Set("Accept-Encoding", "gzip")
	cardRec := httptest.NewRecorder()
	handler.ServeHTTP(cardRec, cardReq)
	if cardRec.Code != http.StatusOK {
		t.Fatalf("GET /api/v1/card.svg = %d", cardRec.Code)
	}
	if ct := cardRec.Header().Get("Content-Type"); !strings.Contains(ct, "image/svg+xml") {
		t.Fatalf("card.svg Content-Type = %q", ct)
	}
	if cc := cardRec.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("card.svg Cache-Control = %q, want no-store", cc)
	}
	if ce := cardRec.Header().Get("Content-Encoding"); ce != "gzip" {
		t.Fatalf("card.svg Content-Encoding = %q, want gzip", ce)
	}
	gz, err := gzip.NewReader(cardRec.Body)
	if err != nil {
		t.Fatal(err)
	}
	svgBytes, err := io.ReadAll(gz)
	_ = gz.Close()
	if err != nil {
		t.Fatal(err)
	}
	var parsedXML struct {
		XMLName xml.Name `xml:"svg"`
	}
	if err := xml.Unmarshal(svgBytes, &parsedXML); err != nil {
		t.Fatalf("card.svg is not valid XML: %v\n%s", err, string(svgBytes))
	}
	svgStr := string(svgBytes)
	for _, want := range []string{"TOKEMON FIELD CARD", "166,000", "LIFETIME TOKENS", "LAST 30 DAYS"} {
		if !strings.Contains(svgStr, want) {
			t.Errorf("card.svg missing %q", want)
		}
	}
	for _, forbidden := range []string{"private-laptop", "private-repo", "private-session-xyz", "private-session-abc", "claude-sonnet-4", "openai/gpt-5"} {
		if strings.Contains(svgStr, forbidden) {
			t.Fatalf("card.svg leaked sensitive field %q", forbidden)
		}
	}
}

func TestM5BAlertSWRRecheckConcurrencyRace(t *testing.T) {
	yamlBudget := int64(1000)
	store, err := database.OpenWithLocation(
		filepath.Join(t.TempDir(), "alerts-race.db"),
		catalog.Empty(),
		time.UTC,
		quota.Quota{Provider: "openai", Label: "Tight YAML Cap", ResetDay: 1, Budget: quota.Budget{TokensPerMonth: &yamlBudget}},
	)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	// Seed 850 tokens (warning on 1,000 YAML quota) and a permissive 10,000 UI override.
	start, _ := quota.PeriodFor(time.Now(), 1, time.UTC)
	if _, err := store.Ingest(context.Background(), []usage.Event{{
		SchemaVersion: usage.SchemaVersion, EventID: "race-evt-1", Timestamp: start.Add(time.Hour),
		MachineID: "box", Provider: "openai", Model: "gpt-5", Tool: "codex", SessionID: "s-race",
		TotalTokens: usage.Int64(850), TokenAccuracy: usage.AccuracyReported,
		Source: usage.Source{Adapter: "codex", AdapterVersion: "1"},
	}}); err != nil {
		t.Fatal(err)
	}
	highBudget := int64(10000)
	if err := store.UpsertQuota(context.Background(), quota.Quota{
		Provider: "openai", Label: "Permissive UI Cap", ResetDay: 1, Budget: quota.Budget{TokensPerMonth: &highBudget},
	}); err != nil {
		t.Fatal(err)
	}

	var hits atomic.Int64
	alertNotify := make(chan string, 64)
	webhookSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var p quotaAlertPayload
		if err := json.Unmarshal(body, &p); err == nil {
			hits.Add(1)
			select {
			case alertNotify <- p.Threshold:
			default:
			}
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer webhookSrv.Close()

	srv, err := NewWithConfig(store, Config{
		IngestToken:     "secret",
		DashboardToken:  testDashboardToken,
		AlertWebhookURL: webhookSrv.URL,
	})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()

	// Delete permissive UI override -> immediately evaluates exposed tighter YAML quota (850/1000 = 85%).
	delForm := url.Values{
		"csrf_token":     {srv.csrfToken()},
		"action":         {"delete"},
		"provider":       {"openai"},
		"overrides_yaml": {"1"},
	}
	delReq := httptest.NewRequest(http.MethodPost, "/settings/quotas", strings.NewReader(delForm.Encode()))
	delReq.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	delRec := httptest.NewRecorder()
	testHandler(srv).ServeHTTP(delRec, delReq)
	if delRec.Code != http.StatusSeeOther {
		t.Fatalf("delete override = %d, want 303", delRec.Code)
	}
	select {
	case got := <-alertNotify:
		if got != "warning" {
			t.Fatalf("alert after delete override = %q, want warning", got)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("timed out waiting for immediate alert after deleting permissive UI override")
	}

	// Hammer concurrent/staggered triggerQuotaAlerts(true) across rapid exit boundaries under -race.
	srv.alerts.mu.Lock()
	srv.alerts.recheckWait = 5 * time.Millisecond
	srv.alerts.recheckSettle = 2 * time.Millisecond
	srv.alerts.mu.Unlock()

	var wg sync.WaitGroup
	for round := 0; round < 6; round++ {
		for g := 0; g < 16; g++ {
			wg.Add(1)
			go func(delay int) {
				defer wg.Done()
				if delay > 0 {
					time.Sleep(time.Duration(delay) * time.Millisecond)
				}
				srv.triggerQuotaAlerts(true)
			}(g % 9)
		}
		wg.Wait()
		// Wait for the active recheck loop to exit cleanly.
		deadline := time.Now().Add(2 * time.Second)
		for time.Now().Before(deadline) {
			srv.alerts.mu.Lock()
			running := srv.alerts.recheckRunning
			needed := srv.alerts.recheckNeeded
			srv.alerts.mu.Unlock()
			if !running && !needed {
				break
			}
			time.Sleep(2 * time.Millisecond)
		}
		srv.alerts.mu.Lock()
		running, needed := srv.alerts.recheckRunning, srv.alerts.recheckNeeded
		srv.alerts.mu.Unlock()
		if running || needed {
			t.Fatalf("round %d left stranded recheck state: running=%v needed=%v", round, running, needed)
		}
	}

	// Confirm follow-up triggerQuotaAlerts(true) still runs and delivers after reset.
	before := hits.Load()
	srv.resetAlertStateForQuota("openai", "")
	srv.triggerQuotaAlerts(true)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) && hits.Load() == before {
		time.Sleep(5 * time.Millisecond)
	}
	if hits.Load() <= before {
		t.Fatal("triggerQuotaAlerts(true) did not deliver after concurrent recheck stress")
	}
}

func TestM5BTokedexDetailUnknownTokensOnlyAndEdgeGuards(t *testing.T) {
	cat := catalog.MustNew(map[string]catalog.Model{
		"cat-unknown-only": {
			Provider: "openai", DisplayName: "Cataloged Unknown Only", Aliases: []string{"cat-unknown-only"}, Tier: "A",
			Pricing: catalog.Pricing{Currency: "USD", Mode: "unknown", VerifiedAt: "2026-11-20"},
		},
	})
	store, err := database.OpenWithLocation(filepath.Join(t.TempDir(), "tokedex-unknown.db"), cat, time.UTC)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	now := time.Now().UTC()
	if _, err := store.Ingest(context.Background(), []usage.Event{
		{
			SchemaVersion: usage.SchemaVersion, EventID: "unk-only-1", Timestamp: now.Add(-time.Hour),
			MachineID: "unk-box", Provider: "openai", Model: "cat-unknown-only", Tool: "codex", SessionID: "sess-unk-1",
			TokenAccuracy: usage.AccuracyUnknown, Source: usage.Source{Adapter: "codex", AdapterVersion: "1"},
		},
		{
			SchemaVersion: usage.SchemaVersion, EventID: "unk-mixed-1", Timestamp: now.Add(-2 * time.Hour),
			MachineID: "unk-box", Provider: "openai", Model: "mixed-uncataloged", Tool: "codex", SessionID: "sess-mix-1",
			TokenAccuracy: usage.AccuracyUnknown, Source: usage.Source{Adapter: "codex", AdapterVersion: "1"},
		},
		{
			SchemaVersion: usage.SchemaVersion, EventID: "unk-mixed-2", Timestamp: now.Add(-time.Hour),
			MachineID: "known-box", Provider: "openai", Model: "mixed-uncataloged", Tool: "codex", SessionID: "sess-mix-2",
			InputTokens: usage.Int64(300), OutputTokens: usage.Int64(200), TotalTokens: usage.Int64(500),
			TokenAccuracy: usage.AccuracyReported, Source: usage.Source{Adapter: "codex", AdapterVersion: "1"},
		},
	}); err != nil {
		t.Fatal(err)
	}

	srv, err := NewWithConfig(store, Config{IngestToken: "secret", DashboardToken: testDashboardToken, EnableTokedex: true})
	if err != nil {
		t.Fatal(err)
	}
	defer srv.Close()
	handler := testHandler(srv)

	// 1. Unknown-token-only model renders "—" in Total tokens, Input/Output, and Machine Tokens cells (never 0).
	rec := httptest.NewRecorder()
	handler.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/tokedex/cat-unknown-only", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("GET /tokedex/cat-unknown-only = %d", rec.Code)
	}
	body := rec.Body.String()
	for _, want := range []string{
		`<div class="stat-label">Total tokens</div><div class="stat-value unknown-value">—</div>`,
		`1 unknown`,
		`<div class="stat-label">Input / Output</div><div class="stat-value unknown-value">—</div>`,
		`<td data-label="Tokens" class="unknown-value">—</td>`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("unknown-only /tokedex/cat-unknown-only missing %q", want)
		}
	}
	for _, forbidden := range []string{
		`<div class="stat-label">Total tokens</div><div class="stat-value">0</div>`,
		`<div class="stat-label">Input / Output</div><div class="stat-value">0 / 0</div>`,
		`<td data-label="Tokens">0</td>`,
	} {
		if strings.Contains(body, forbidden) {
			t.Errorf("unknown-only /tokedex/cat-unknown-only rendered forbidden %q", forbidden)
		}
	}

	// 2. Mixed model shows 500 on known-box and "—" on unk-box.
	mixRec := httptest.NewRecorder()
	handler.ServeHTTP(mixRec, httptest.NewRequest(http.MethodGet, "/tokedex/mixed-uncataloged", nil))
	if mixRec.Code != http.StatusOK {
		t.Fatalf("GET /tokedex/mixed-uncataloged = %d", mixRec.Code)
	}
	mixBody := mixRec.Body.String()
	for _, want := range []string{
		`<div class="stat-label">Total tokens</div><div class="stat-value">500</div>`,
		`1 unknown`,
		`<td data-label="Tokens">500</td>`,
		`<td data-label="Tokens" class="unknown-value">—</td>`,
	} {
		if !strings.Contains(mixBody, want) {
			t.Errorf("mixed /tokedex/mixed-uncataloged missing %q", want)
		}
	}

	// 3. Control characters and backslash in /tokedex/{model} return 404.
	for _, badPath := range []string{
		"/tokedex/foo%01bar",
		"/tokedex/foo%1fbar",
		"/tokedex/foo%7fbar",
		"/tokedex/foo%5cbar",
	} {
		badRec := httptest.NewRecorder()
		handler.ServeHTTP(badRec, httptest.NewRequest(http.MethodGet, badPath, nil))
		if badRec.Code != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", badPath, badRec.Code)
		}
	}

	// 4. Share card NaN progress guard produces valid XML without NaN.
	var ov database.Overview
	ov.Evolution.Stage = 2
	ov.Evolution.FormName = "Egg Guard"
	ov.Evolution.Progress = math.NaN()
	svg := renderShareCardSVG(ov)
	if strings.Contains(svg, "NaN") {
		t.Fatalf("renderShareCardSVG emitted NaN: %s", svg)
	}
	var parsedXML struct {
		XMLName xml.Name `xml:"svg"`
	}
	if err := xml.Unmarshal([]byte(svg), &parsedXML); err != nil {
		t.Fatalf("renderShareCardSVG invalid XML on NaN progress: %v\n%s", err, svg)
	}
}
