// Package browsercheck runs the rendered dashboard through a real headless
// Chrome: a functional smoke over the five dashboard pages plus an axe-core
// accessibility scan against a seeded scratch database. It is guarded by
// TOKEMON_BROWSER_SMOKE=1 so the ordinary `go test ./...` gate stays
// hermetic (no browser required); CI runs it explicitly after the Go tests.
//
// axe-core is vendored (unmodified) at testdata/axe.min.js, pinned to the
// 4.10.3 release: MPL-2.0, Copyright (c) Deque Systems, Inc.
package browsercheck

import (
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/chromedp"
	"github.com/tokemon/tokemon/internal/api"
	"github.com/tokemon/tokemon/internal/catalog"
	"github.com/tokemon/tokemon/internal/database"
	"github.com/tokemon/tokemon/internal/quota"
	"github.com/tokemon/tokemon/internal/usage"
)

//go:embed testdata/axe.min.js
var axeSource string

const dashboardToken = "smoke-dashboard"

// axeTags mirrors the WCAG 2.x A/AA success criteria the release promises;
// best-practice rules outside those tags stay out of the gate.
var axeTags = []string{"wcag2a", "wcag21a", "wcag2aa", "wcag21aa"}

// axeAllowlist maps a page path to the axe violation ids the release accepts
// for now, each with the reason it stands. Anything unlisted fails the gate.
// The allowlist is deliberately empty since 2026-11-07: every violation the
// scans surfaced (progressbar name, heatmap grid anatomy, animation-race
// contrast, the overview meta refresh) was fixed outright rather than
// allowed. The overview now refreshes through a pausable client control, so
// nothing is exempt from the WCAG 2.x A/AA scan. Keep it that way; fix, or
// land an allowlist entry with a reason, never silently.
var axeAllowlist = map[string]map[string]string{}

// The invariant scripts return a JSON array of problem strings; empty means
// the page keeps its contract.

// sharedInvariantJS checks what every page promises: a skip link to a real
// main target, and horizontally scrolling regions that keyboard users can
// actually reach.
const sharedInvariantJS = `(() => {
	const problems = [];
	if (!document.querySelector('a.skip-link[href="#main-content"]')) {
		problems.push('missing a.skip-link[href="#main-content"]');
	}
	if (!document.getElementById('main-content')) {
		problems.push('missing #main-content target');
	}
	for (const el of document.querySelectorAll('body *')) {
		const overflow = getComputedStyle(el).overflowX;
		if ((overflow === 'auto' || overflow === 'scroll') && el.scrollWidth > el.clientWidth + 1) {
			const tabindex = el.getAttribute('tabindex');
			if (tabindex === null || Number(tabindex) < 0) {
				problems.push('overflowing region not keyboard reachable: ' + el.tagName + '.' + el.className);
			}
		}
	}
	if (document.querySelector('meta[http-equiv="refresh"]')) {
		problems.push('meta refresh is back; the overview must use the pause/refresh control instead');
	}
	return JSON.stringify(problems);
})()`

// exportInvariantJS asserts every export anchor opts into a download instead
// of navigating the dashboard away and losing filter state.
const exportInvariantJS = `(() => {
	const problems = [];
	for (const anchor of document.querySelectorAll('a.action[href*="export"]')) {
		if (!anchor.hasAttribute('download')) {
			problems.push('export link navigates instead of downloading: ' + anchor.textContent.trim());
		}
	}
	return JSON.stringify(problems);
})()`

// sourceHealthInvariantJS asserts the overview machine-health panel: one
// line per reported source, a Fresh/Stale/Unknown pill on each, the error
// class inline on the failing source, the machine-level sync-error cell, and
// the metadata-only promise (no local paths anywhere in the DOM).
const sourceHealthInvariantJS = `(() => {
	const problems = [];
	const lines = Array.from(document.querySelectorAll('.source-health-line'));
	if (lines.length !== 4) {
		problems.push('expected 4 source health lines, found ' + lines.length);
	}
	const states = new Set(['Fresh', 'Stale', 'Unknown']);
	for (const line of lines) {
		const pill = line.querySelector('.status-pill');
		if (!pill || !states.has(pill.textContent.trim())) {
			problems.push('source line lacks a Fresh/Stale/Unknown pill: ' + line.textContent.trim());
		}
	}
	for (const adapter of ['claude-code', 'codex', 'zcode', 'opencode']) {
		if (lines.filter(line => line.textContent.includes(adapter)).length !== 1) {
			problems.push('adapter ' + adapter + ' does not render exactly one source line');
		}
	}
	const errorLine = lines.find(line => line.textContent.includes('opencode'));
	if (!errorLine || !(errorLine.querySelector('.sync-error') || {}).textContent || !errorLine.querySelector('.sync-error').textContent.includes('upload_failed')) {
		problems.push('erroring source does not show its error class inline');
	}
	const unknownLine = lines.find(line => line.textContent.includes('zcode'));
	if (!unknownLine || !unknownLine.textContent.includes('never synced')) {
		problems.push('unknown source does not say never synced');
	}
	if (!document.querySelector('td.sync-error')) {
		problems.push('machine row does not flag the sync-error cell');
	}
	const dom = document.documentElement.innerHTML;
	if (dom.includes('/Users/') || dom.includes('/home/')) {
		problems.push('page DOM contains a raw local path');
	}
	return JSON.stringify(problems);
})()`

// quotaAbsentInvariantJS guards the no-quotas default: the analytics page
// must not render the panel section at all.
const quotaAbsentInvariantJS = `(() => {
	const problems = [];
	if (document.getElementById('quota-title') || document.querySelector('.quota-panel')) {
		problems.push('quota panel rendered without quotas configured');
	}
	return JSON.stringify(problems);
})()`

// quotaConfiguredInvariantJS asserts the analytics quota panel with two
// configured budgets: an over-budget row (over styling, 120 percent track,
// pace detail) and an under-budget row (plain fill, 20 percent track).
const quotaConfiguredInvariantJS = `(() => {
	const problems = [];
	if (!document.getElementById('quota-title')) {
		problems.push('configured quota panel section is missing');
		return JSON.stringify(problems);
	}
	const rows = Array.from(document.querySelectorAll('.quota-panel .quota'));
	if (rows.length !== 2) {
		problems.push('expected 2 quota rows, found ' + rows.length);
	}
	const byLabel = (needle) => rows.find(row => row.textContent.includes(needle));
	const over = byLabel('OpenAI cap');
	const under = byLabel('Anthropic cap');
	if (!over || !under) {
		problems.push('quota rows missing the seeded labels');
		return JSON.stringify(problems);
	}
	const trackLabel = (row) => (row.querySelector('.quota-track') || {}).getAttribute ? row.querySelector('.quota-track').getAttribute('aria-label') : '';
	const figures = over.querySelector('.quota-figures');
	if (!figures || !figures.textContent.includes('60,000,000 tokens / 50,000,000 tokens')) {
		problems.push('over-budget figures wrong: ' + (figures ? figures.textContent : 'none'));
	}
	if (!trackLabel(over).includes('120 percent of the quota used')) {
		problems.push('over-budget track aria-label wrong: ' + trackLabel(over));
	}
	if (!over.querySelector('.quota-fill.over')) {
		problems.push('over-budget row lacks the .quota-fill.over styling');
	}
	if (!under.querySelector('.quota-fill') || under.querySelector('.quota-fill.over')) {
		problems.push('under-budget fill styling wrong');
	}
	if (!trackLabel(under).includes('20 percent of the quota used')) {
		problems.push('under-budget track aria-label wrong: ' + trackLabel(under));
	}
	for (const row of [over, under]) {
		const detail = row.querySelector('.insight-detail');
		if (!detail || !detail.textContent.trim()) {
			problems.push('quota row lacks the pace/reset detail line');
		}
	}
	return JSON.stringify(problems);
})()`

// refreshControlInvariantJS asserts the overview topbar control that
// replaced the meta refresh: a labeled manual refresh and a pause toggle
// exposing its state via aria-pressed.
const refreshControlInvariantJS = `(() => {
	const problems = [];
	const refresh = document.getElementById('refresh-now');
	if (!refresh || refresh.textContent.trim() !== 'Refresh now') {
		problems.push('overview lacks the labeled Refresh now button');
	}
	const toggle = document.getElementById('auto-refresh-toggle');
	if (!toggle) {
		problems.push('overview lacks the auto-refresh pause toggle');
	} else {
		if (toggle.getAttribute('aria-pressed') === null) {
			problems.push('auto-refresh toggle lacks aria-pressed');
		}
		if (!['Pause auto-refresh', 'Resume auto-refresh'].includes(toggle.textContent.trim())) {
			problems.push('auto-refresh toggle has an unexpected label: ' + toggle.textContent.trim());
		}
	}
	return JSON.stringify(problems);
})()`

const freshnessInvariantJS = `(() => {
	const problems = [];
	const pill = document.querySelector('.status-pill.connected');
	if (!pill || !pill.textContent.includes('Fresh')) {
		problems.push('fresh dashboard lacks the connected Fresh pill');
	}
	return JSON.stringify(problems);
})()`

const sortInvariantJS = `(() => {
	const problems = [];
	const sorted = document.querySelectorAll('th[aria-sort]');
	if (sorted.length === 0) {
		problems.push('sorted session table exposes no aria-sort');
	}
	for (const header of sorted) {
		const direction = header.getAttribute('aria-sort');
		if (direction !== 'ascending' && direction !== 'descending') {
			problems.push('header aria-sort is not a sort direction: ' + direction);
		}
	}
	return JSON.stringify(problems);
})()`

// settleAnimationJS injects a stylesheet that disables animations and
// transitions. Headless Chrome can leave elements parked at an animation's
// backwards fill (from) state when no frame ticks after load — the stat cards
// start at opacity .72 and contrast there measures 4.49:1 vs 7.6:1 settled —
// so the scan must measure the static stylesheet, not a frame race.
const settleAnimationJS = `(() => {
	const style = document.createElement('style');
	style.textContent = '*, *::before, *::after { animation: none !important; transition: none !important; }';
	document.head.append(style);
	return true;
})()`

// axeViolationsJS runs the vendored axe with the release's WCAG tag set and
// returns the violations as compact JSON for the allowlist check.
var axeViolationsJS = `axe.run(document, {runOnly: {type: 'tag', values: ['` + strings.Join(axeTags, `', '`) + `']}}).then(
	result => JSON.stringify(result.violations.map(violation => ({
		id: violation.id,
		impact: violation.impact,
		help: violation.help,
		nodes: violation.nodes.map(node => ({
			target: Array.isArray(node.target) ? node.target.join(' ') : String(node.target),
			html: node.html.slice(0, 160),
			data: node.any.length > 0 ? node.any[0].data : null,
		})),
	})))
)`

func TestDashboardBrowserSmokeAndAccessibility(t *testing.T) {
	if os.Getenv("TOKEMON_BROWSER_SMOKE") != "1" {
		t.Skip("set TOKEMON_BROWSER_SMOKE=1 with a local Chrome to run the browser smoke")
	}
	baseURL := startSeededServer(t, true)
	allocatorCtx, cancelAllocator := chromedp.NewExecAllocator(context.Background(),
		append(chromedp.DefaultExecAllocatorOptions[:],
			chromedp.NoSandbox,
			chromedp.WindowSize(1280, 800),
		)...)
	defer cancelAllocator()
	browserCtx, cancelBrowser := chromedp.NewContext(allocatorCtx)
	defer cancelBrowser()
	ctx, cancel := context.WithTimeout(browserCtx, 90*time.Second)
	defer cancel()
	if _, err := chromedp.Run(ctx, chromedp.Evaluate[int](`1`)); err != nil {
		t.Fatalf("could not start headless Chrome: %v", err)
	}
	// The templates honor reduced motion; emulating it keeps the scan
	// deterministic (contrast is measured on settled, full-opacity text
	// instead of mid-animation blends).
	if _, err := chromedp.Call(ctx, emulation.SetEmulatedMedia, emulation.SetEmulatedMediaParams{
		Features: []*emulation.MediaFeature{{Name: "prefers-reduced-motion", Value: "reduce"}},
	}); err != nil {
		t.Fatalf("could not emulate reduced motion: %v", err)
	}
	if reduced, err := chromedp.Run(ctx, chromedp.Evaluate[bool](`window.matchMedia('(prefers-reduced-motion: reduce)').matches`)); err != nil || !reduced {
		t.Fatalf("reduced-motion emulation did not apply (matches=%v, err=%v)", reduced, err)
	}

	pages := []struct {
		path       string
		invariants []string
	}{
		{path: "/", invariants: []string{freshnessInvariantJS, sourceHealthInvariantJS, refreshControlInvariantJS}},
		{path: "/analytics?period=7d&dimension=models", invariants: []string{exportInvariantJS, quotaAbsentInvariantJS}},
		{path: "/tokedex"},
		{path: "/sessions", invariants: []string{exportInvariantJS, sortInvariantJS}},
		{path: "/settings"},
	}
	for _, page := range pages {
		t.Run(page.path, func(t *testing.T) {
			if _, err := chromedp.Run(ctx, chromedp.Navigate(baseURL+page.path)); err != nil {
				t.Fatalf("navigation to %s failed: %v", page.path, err)
			}
			if _, err := chromedp.Run(ctx, chromedp.WaitReady("body")); err != nil {
				t.Fatalf("%s never became ready: %v", page.path, err)
			}
			if _, err := chromedp.Run(ctx, chromedp.Evaluate[bool](settleAnimationJS)); err != nil {
				t.Fatalf("could not settle animations on %s: %v", page.path, err)
			}
			for _, invariant := range append([]string{sharedInvariantJS}, page.invariants...) {
				assertNoProblems(t, ctx, invariant)
			}
			assertAxeAllowlist(t, ctx, page.path)
		})
	}

	// With the default configuration the catalog UI is opt-in: the route
	// answers 404 for an authenticated user and no page advertises it.
	t.Run("tokedex-disabled", func(t *testing.T) {
		disabledURL := startSeededServer(t, false)
		disabledCtx, cancelDisabled := chromedp.NewContext(allocatorCtx)
		defer cancelDisabled()
		disabledRunCtx, cancelRun := context.WithTimeout(disabledCtx, 30*time.Second)
		defer cancelRun()
		if _, err := chromedp.Call(disabledRunCtx, emulation.SetEmulatedMedia, emulation.SetEmulatedMediaParams{
			Features: []*emulation.MediaFeature{{Name: "prefers-reduced-motion", Value: "reduce"}},
		}); err != nil {
			t.Fatalf("could not emulate reduced motion: %v", err)
		}
		response, err := chromedp.Run(disabledRunCtx, chromedp.NavigateResponse(disabledURL+"/tokedex"))
		if err != nil {
			t.Fatalf("navigation to disabled /tokedex failed: %v", err)
		}
		if response.Status != http.StatusNotFound {
			t.Fatalf("disabled /tokedex status = %d, want %d", response.Status, http.StatusNotFound)
		}
		if _, err := chromedp.Run(disabledRunCtx, chromedp.Navigate(disabledURL+"/")); err != nil {
			t.Fatalf("navigation to disabled overview failed: %v", err)
		}
		linkCount, err := chromedp.Run(disabledRunCtx, chromedp.Evaluate[int](`document.querySelectorAll('a.nav-link[href="/tokedex"]').length`))
		if err != nil {
			t.Fatalf("nav link check failed: %v", err)
		}
		if linkCount != 0 {
			t.Fatalf("disabled deployment still renders %d Tokedex nav links", linkCount)
		}
	})

	// The pause toggle keeps its state across the reloads it controls, and
	// unpausing restores the default auto-refresh.
	t.Run("overview-refresh-pause", func(t *testing.T) {
		stateJS := `(() => { const toggle = document.getElementById('auto-refresh-toggle'); return JSON.stringify({ pressed: toggle.getAttribute('aria-pressed'), label: toggle.textContent.trim() }); })()`
		navigate := func() {
			t.Helper()
			if _, err := chromedp.Run(ctx, chromedp.Navigate(baseURL+"/")); err != nil {
				t.Fatalf("navigation failed: %v", err)
			}
			if _, err := chromedp.Run(ctx, chromedp.WaitReady("#auto-refresh-toggle")); err != nil {
				t.Fatalf("pause toggle never appeared: %v", err)
			}
		}
		readState := func() (pressed, label string) {
			t.Helper()
			stateJSON, err := chromedp.Run(ctx, chromedp.Evaluate[string](stateJS))
			if err != nil {
				t.Fatalf("could not read the toggle state: %v", err)
			}
			var state struct {
				Pressed string `json:"pressed"`
				Label   string `json:"label"`
			}
			if err := json.Unmarshal([]byte(stateJSON), &state); err != nil {
				t.Fatalf("toggle state malformed JSON %q: %v", stateJSON, err)
			}
			return state.Pressed, state.Label
		}
		navigate()
		if pressed, label := readState(); pressed != "false" || label != "Pause auto-refresh" {
			t.Fatalf("fresh overview toggle state = (%q, %q), want (false, Pause auto-refresh)", pressed, label)
		}
		if _, err := chromedp.Run(ctx, chromedp.Click("#auto-refresh-toggle")); err != nil {
			t.Fatalf("could not click the pause toggle: %v", err)
		}
		if pressed, label := readState(); pressed != "true" || label != "Resume auto-refresh" {
			t.Fatalf("paused toggle state = (%q, %q), want (true, Resume auto-refresh)", pressed, label)
		}
		// A reload — exactly what the control gates — must not lose the pause.
		navigate()
		if pressed, label := readState(); pressed != "true" || label != "Resume auto-refresh" {
			t.Fatalf("pause did not survive the reload: (%q, %q)", pressed, label)
		}
		if _, err := chromedp.Run(ctx, chromedp.Click("#auto-refresh-toggle")); err != nil {
			t.Fatalf("could not resume auto-refresh: %v", err)
		}
		if pressed, _ := readState(); pressed != "false" {
			t.Fatalf("resume left aria-pressed = %q, want false", pressed)
		}
	})

	// The quota panel on /analytics: configured budgets render the label,
	// figures, an accessible track, and the over-budget styling; the main
	// loop's no-quota server already asserts the section is absent.
	t.Run("quota-panel", func(t *testing.T) {
		quotaURL := startQuotaServer(t)
		if _, err := chromedp.Run(ctx, chromedp.Navigate(quotaURL+"/analytics?period=all&dimension=models")); err != nil {
			t.Fatalf("navigation to quota analytics failed: %v", err)
		}
		if _, err := chromedp.Run(ctx, chromedp.WaitReady("body")); err != nil {
			t.Fatalf("quota analytics never became ready: %v", err)
		}
		if _, err := chromedp.Run(ctx, chromedp.Evaluate[bool](settleAnimationJS)); err != nil {
			t.Fatalf("could not settle animations: %v", err)
		}
		for _, invariant := range []string{sharedInvariantJS, exportInvariantJS, quotaConfiguredInvariantJS} {
			assertNoProblems(t, ctx, invariant)
		}
		assertAxeAllowlist(t, ctx, "/analytics")
	})
}

// assertNoProblems evaluates an invariant script and fails with the reported
// problem list when the page breaks its contract.
func assertNoProblems(t *testing.T, ctx context.Context, invariant string) {
	t.Helper()
	problemsJSON, err := chromedp.Run(ctx, chromedp.Evaluate[string](invariant))
	if err != nil {
		t.Fatalf("invariant check failed to run: %v", err)
	}
	var problems []string
	if err := json.Unmarshal([]byte(problemsJSON), &problems); err != nil {
		t.Fatalf("invariant returned malformed JSON %q: %v", problemsJSON, err)
	}
	for _, problem := range problems {
		t.Errorf("%s", problem)
	}
}

type axeNode struct {
	Target string          `json:"target"`
	HTML   string          `json:"html"`
	Data   json.RawMessage `json:"data"`
}

type axeViolation struct {
	ID     string    `json:"id"`
	Impact string    `json:"impact"`
	Help   string    `json:"help"`
	Nodes  []axeNode `json:"nodes"`
}

// assertAxeAllowlist runs axe on the current page and fails for any violation
// id that is not allowlisted for this page with a documented reason.
func assertAxeAllowlist(t *testing.T, ctx context.Context, path string) {
	t.Helper()
	allowed := axeAllowlist[path]
	if _, err := chromedp.Run(ctx, chromedp.Evaluate[chromedp.Void](axeSource)); err != nil {
		t.Fatalf("could not inject axe: %v", err)
	}
	violationsJSON, err := chromedp.Run(ctx, chromedp.Evaluate[string](axeViolationsJS, chromedp.EvalAwaitPromise))
	if err != nil {
		t.Fatalf("axe scan failed to run: %v", err)
	}
	var violations []axeViolation
	if err := json.Unmarshal([]byte(violationsJSON), &violations); err != nil {
		t.Fatalf("axe returned malformed JSON %q: %v", violationsJSON, err)
	}
	for _, violation := range violations {
		reason, ok := allowed[violation.ID]
		if !ok {
			for _, node := range violation.Nodes {
				t.Errorf("axe %s (%s) on %s at %q: %s — data %s", violation.ID, violation.Impact, path, node.Target, node.HTML, node.Data)
			}
			continue
		}
		t.Logf("axe %s on %s allowlisted: %s (%d nodes)", violation.ID, path, reason, len(violation.Nodes))
	}
}

// startSeededServer opens a scratch database, seeds enough usage for every
// page to render with data, and serves the real handler on a loopback port.
// The browser signs in with Basic auth via URL credentials. enableTokedex
// mirrors the TOKEMON_ENABLE_TOKEDEX default-off gate.
func startSeededServer(t *testing.T, enableTokedex bool) string {
	t.Helper()
	store, err := database.Open(filepath.Join(t.TempDir(), "smoke.db"), catalog.Empty())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	base := time.Now().UTC().Add(-2 * time.Hour).Truncate(time.Second)
	events := []usage.Event{
		{
			// Two minutes old so the overview pill reads Fresh.
			SchemaVersion: usage.SchemaVersion,
			EventID:       "smoke-fresh-event",
			Timestamp:     time.Now().UTC().Add(-2 * time.Minute),
			MachineID:     "smoke-laptop",
			Project:       "smoke-project",
			Provider:      "anthropic",
			Model:         "claude-sonnet-4",
			Tool:          "claude-code",
			InputTokens:   usage.Int64(70),
			OutputTokens:  usage.Int64(30),
			TotalTokens:   usage.Int64(100),
			Currency:      "USD",
			TokenAccuracy: usage.AccuracyReported,
			Source:        usage.Source{Adapter: "claude-code", AdapterVersion: "smoke"},
		},
		{
			SchemaVersion: usage.SchemaVersion,
			EventID:       "smoke-overview-event",
			Timestamp:     base,
			MachineID:     "smoke-laptop",
			Project:       "smoke-project",
			Provider:      "anthropic",
			Model:         "claude-sonnet-4",
			Tool:          "claude-code",
			InputTokens:   usage.Int64(700),
			OutputTokens:  usage.Int64(300),
			TotalTokens:   usage.Int64(1000),
			Currency:      "USD",
			TokenAccuracy: usage.AccuracyReported,
			Source:        usage.Source{Adapter: "claude-code", AdapterVersion: "smoke"},
		},
		{
			SchemaVersion: usage.SchemaVersion,
			EventID:       "smoke-analytics-event",
			Timestamp:     base.Add(-26 * time.Hour),
			MachineID:     "smoke-desktop",
			Project:       "smoke-other",
			Provider:      "openai",
			Model:         "gpt-5",
			Tool:          "codex",
			InputTokens:   usage.Int64(200),
			OutputTokens:  usage.Int64(100),
			TotalTokens:   usage.Int64(300),
			Currency:      "USD",
			TokenAccuracy: usage.AccuracyReported,
			Source:        usage.Source{Adapter: "codex", AdapterVersion: "smoke"},
		},
	}
	if _, err := store.Ingest(context.Background(), events); err != nil {
		t.Fatal(err)
	}
	// One machine, four connector-health shapes: fresh sync, stale sync,
	// a source that never synced, and a stale source reporting an error
	// class — the exact rows the machine-health panel must render.
	now := time.Now().UTC()
	freshSync := now.Add(-time.Minute)
	staleSync := now.Add(-2 * time.Hour)
	staleErrSync := now.Add(-90 * time.Minute)
	if err := store.RecordHeartbeat(context.Background(), database.AgentHeartbeat{
		MachineID:        "smoke-laptop",
		OperatingSystem:  "darwin",
		Architecture:     "arm64",
		SourceCount:      4,
		SourceErrorCount: 1,
		Sources: []database.AgentSourceHealth{
			{Adapter: "claude-code", SourceHash: "smoke-hash-fresh", LastSuccessfulSync: &freshSync},
			{Adapter: "codex", SourceHash: "smoke-hash-stale", LastSuccessfulSync: &staleSync},
			{Adapter: "zcode", SourceHash: "smoke-hash-unknown"},
			{Adapter: "opencode", SourceHash: "smoke-hash-error", LastSuccessfulSync: &staleErrSync, LastErrorClass: "upload_failed"},
		},
	}); err != nil {
		t.Fatal(err)
	}
	return serveStore(t, store, enableTokedex)
}

// serveStore wraps an already-seeded store in the real handler on a loopback
// port and returns the Basic-auth base URL the browser signs in with.
func serveStore(t *testing.T, store *database.Store, enableTokedex bool) string {
	t.Helper()
	server, err := api.NewWithConfig(store, api.Config{
		IngestToken:    "smoke-ingest",
		DashboardToken: dashboardToken,
		EnableTokedex:  enableTokedex,
		Logger:         slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go http.Serve(listener, server.Handler())
	host := listener.Addr().String()
	return fmt.Sprintf("http://tokemon:%s@%s", dashboardToken, host)
}

// startQuotaServer opens a scratch store with two configured quotas and
// seeds one over-budget and one under-budget provider for the current
// monthly period, then serves it for the quota-panel subtest.
func startQuotaServer(t *testing.T) string {
	t.Helper()
	tokensBudget := int64(50_000_000)
	quotas := []quota.Quota{
		{Provider: "openai", Label: "OpenAI cap", ResetDay: 1, Budget: quota.Budget{TokensPerMonth: &tokensBudget}},
		{Provider: "anthropic", Label: "Anthropic cap", ResetDay: 1, Budget: quota.Budget{TokensPerMonth: &tokensBudget}},
	}
	store, err := database.OpenWithLocation(filepath.Join(t.TempDir(), "quota.db"), catalog.Empty(), time.UTC, quotas...)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	start, _ := quota.PeriodFor(time.Now().UTC(), 1, time.UTC)
	base := start.Add(time.Hour)
	events := []usage.Event{
		{
			SchemaVersion: usage.SchemaVersion,
			EventID:       "smoke-quota-over",
			Timestamp:     base,
			MachineID:     "smoke-laptop",
			Project:       "smoke-project",
			Provider:      "openai",
			Model:         "gpt-5",
			Tool:          "codex",
			InputTokens:   usage.Int64(30_000_000),
			OutputTokens:  usage.Int64(30_000_000),
			TotalTokens:   usage.Int64(60_000_000),
			Currency:      "USD",
			TokenAccuracy: usage.AccuracyReported,
			Source:        usage.Source{Adapter: "codex", AdapterVersion: "smoke"},
		},
		{
			SchemaVersion: usage.SchemaVersion,
			EventID:       "smoke-quota-under",
			Timestamp:     base.Add(time.Hour),
			MachineID:     "smoke-laptop",
			Project:       "smoke-project",
			Provider:      "anthropic",
			Model:         "claude-sonnet-4",
			Tool:          "claude-code",
			InputTokens:   usage.Int64(6_000_000),
			OutputTokens:  usage.Int64(4_000_000),
			TotalTokens:   usage.Int64(10_000_000),
			Currency:      "USD",
			TokenAccuracy: usage.AccuracyReported,
			Source:        usage.Source{Adapter: "claude-code", AdapterVersion: "smoke"},
		},
	}
	if _, err := store.Ingest(context.Background(), events); err != nil {
		t.Fatal(err)
	}
	return serveStore(t, store, false)
}
