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
// The allowlist is deliberately tiny: 2026-11-06's verified scan flagged only
// the overview's auto-reload, and everything else the first pass surfaced
// (progressbar name, heatmap grid anatomy, animation-race contrast) was fixed
// outright in the same change.
var axeAllowlist = map[string]map[string]string{
	"/": {
		// The overview intentionally auto-reloads every 60s (WCAG 2.2.1's
		// real-time data case) so a pinned dashboard stays current. Making
		// the timing adjustable or adding a pause control is a product
		// change, not a markup fix; revisit there, not here.
		"meta-refresh": "deliberate 60s dashboard auto-reload",
	},
}

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
	baseURL := startSeededServer(t)
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
		{path: "/", invariants: []string{freshnessInvariantJS}},
		{path: "/analytics?period=7d&dimension=models", invariants: []string{exportInvariantJS}},
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
// The browser signs in with Basic auth via URL credentials.
func startSeededServer(t *testing.T) string {
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
	if err := store.RecordHeartbeat(context.Background(), database.AgentHeartbeat{MachineID: "smoke-laptop", OperatingSystem: "darwin", Architecture: "arm64", SourceCount: 1, SourceErrorCount: 0}); err != nil {
		t.Fatal(err)
	}
	server, err := api.NewWithConfig(store, api.Config{
		IngestToken:    "smoke-ingest",
		DashboardToken: dashboardToken,
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
