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
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/chromedp/cdproto/emulation"
	"github.com/chromedp/cdproto/network"
	"github.com/chromedp/chromedp"
	"github.com/tokemon/tokemon/internal/api"
	"github.com/tokemon/tokemon/internal/catalog"
	"github.com/tokemon/tokemon/internal/database"
	"github.com/tokemon/tokemon/internal/quota"
	"github.com/tokemon/tokemon/internal/usage"
	"golang.org/x/crypto/bcrypt"
)

//go:embed testdata/axe.min.js
var axeSource string

const dashboardToken = "smoke-dashboard"

// The dashboard password account the login-session-logout smoke signs in
// with; startSeededPasswordServer configures the server to accept it.
const (
	smokeLoginUsername = "smoke-operator"
	smokeLoginPassword = "smoke-passphrase"
)

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

// gateViewports are the widths every gated page renders at: the desktop hub
// plus the tablet and phone widths where the responsive layout reflows. 641
// is one pixel above the 640px column breakpoint — the width where the
// sessions heading once overflowed by 2px — so the gap between a breakpoint
// and its neighbour stays gated. The gate used to render only 1280x800 — the
// one width where the wide regions fit — so it was blind to the layout it
// exists to protect.
var gateViewports = []struct {
	name   string
	width  int64
	height int64
}{
	{name: "1280x800", width: 1280, height: 800},
	{name: "768x900", width: 768, height: 900},
	{name: "641x900", width: 641, height: 900},
	{name: "390x844", width: 390, height: 844},
}

// The invariant scripts return a JSON array of problem strings; empty means
// the page keeps its contract.

// sharedInvariantJS checks what every page promises: a skip link to a real
// main target, horizontally scrolling regions that keyboard users can
// actually reach, and a document that never scrolls sideways. The gate used
// to miss document-level overflow because it only inspected elements whose
// own overflow was auto/scroll, so the root check below is deliberate: a wide
// table belongs inside its scroll wrapper, not on the document.
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
	const root = document.documentElement;
	if (root.scrollWidth > root.clientWidth + 1) {
		problems.push('document overflows horizontally by ' + (root.scrollWidth - root.clientWidth) + 'px (scrollWidth ' + root.scrollWidth + ' > clientWidth ' + root.clientWidth + '); offenders: ' + documentOverflowOffenders());
	}
	if (document.querySelector('meta[http-equiv="refresh"]')) {
		problems.push('meta refresh is back; the overview must use the pause/refresh control instead');
	}
	return JSON.stringify(problems);

	// documentOverflowOffenders names the elements that actually push the
	// document wider. Elements clipped by an auto/scroll/hidden ancestor are
	// skipped: they overflow their own wrapper, not the page.
	function documentOverflowOffenders() {
		const limit = document.documentElement.clientWidth;
		const offenders = [];
		for (const el of document.querySelectorAll('body *')) {
			const rect = el.getBoundingClientRect();
			if (rect.width === 0 || rect.right <= limit + 1) continue;
			let clipped = false;
			for (let node = el.parentElement; node && node !== document.body; node = node.parentElement) {
				const overflow = getComputedStyle(node).overflowX;
				if (overflow === 'auto' || overflow === 'scroll' || overflow === 'hidden') {
					clipped = true;
					break;
				}
			}
			if (clipped) continue;
			const classes = typeof el.className === 'string' && el.className.trim() ? '.' + el.className.trim().split(/\s+/).join('.') : '';
			offenders.push(el.tagName.toLowerCase() + classes + (el.id ? '#' + el.id : '') + ' right=' + Math.round(rect.right) + 'px');
		}
		return offenders.length ? offenders.slice(0, 5).join(', ') : 'none found outside clipped regions';
	}
})()`

// tokedexInvariantJS asserts the Tokedex MVP states on the enabled page: the
// tier legend explains the letters and their bundled-YAML provenance, an
// unpublished-rates model says so instead of showing empty pricing, a
// zero-usage catalog model says so instead of rendering zero, and a fully
// cataloged deployment states that nothing is uncataloged.
const tokedexInvariantJS = `(() => {
	const problems = [];
	const legend = document.querySelector('.tier-legend');
	if (!legend) {
		problems.push('missing tier legend');
	} else {
		const rows = legend.querySelectorAll('.legend-row');
		if (rows.length !== 3) {
			problems.push('expected 3 tier legend rows (S, A, B), found ' + rows.length);
		}
		const text = legend.textContent;
		if (!text.includes('not measured model quality')) {
			problems.push('tier legend does not say tiers are not measured quality');
		}
		for (const tier of ['S', 'A', 'B']) {
			if (!Array.from(rows).some(row => row.querySelector('dt') && row.querySelector('dt').textContent.trim() === tier)) {
				problems.push('tier legend lacks a row for tier ' + tier);
			}
		}
	}
	const unknown = Array.from(document.querySelectorAll('.pricing-unknown'));
	if (unknown.length !== 1) {
		problems.push('expected exactly 1 unpublished-rates card, found ' + unknown.length);
	} else if (!unknown[0].textContent.includes('Rates not published yet')) {
		problems.push('unpublished-rates card lacks the standard copy');
	}
	const zeroHeading = document.querySelector('h3[title="smoke-zero-usage"]');
	const zeroCard = zeroHeading ? zeroHeading.closest('.model-card') : null;
	if (!zeroCard) {
		problems.push('zero-usage catalog model card is missing');
	} else {
		if (!zeroCard.textContent.includes('No usage recorded yet')) {
			problems.push('zero-usage catalog model lacks its empty state');
		}
		if (/\b0 tokens/.test(zeroCard.textContent)) {
			problems.push('zero-usage catalog model renders a zero token count');
		}
	}
	if (!document.body.textContent.includes('Every observed model resolves')) {
		problems.push('fully cataloged deployment lacks the uncataloged empty state');
	}
	if (document.getElementById('uncataloged-title')) {
		problems.push('uncataloged section rendered with nothing uncataloged');
	}
	return JSON.stringify(problems);
})()`

// tokedexFilterInvariantJS drives the client-side catalog controls: every
// control is labeled and keyboard reachable, and search, tier/provider/pricing
// filters, reset, and per-tier sorting actually change the visible card set.
const tokedexFilterInvariantJS = `(() => {
	const problems = [];
	const ids = ['tokedex-search', 'tokedex-sort', 'tokedex-tier', 'tokedex-provider', 'tokedex-pricing'];
	const controls = {};
	for (const id of ids) {
		const control = document.getElementById(id);
		if (!control) {
			problems.push('missing filter control #' + id);
			continue;
		}
		controls[id] = control;
		const label = document.querySelector('label[for="' + id + '"]');
		if (!label || !label.textContent.trim()) {
			problems.push('filter control #' + id + ' has no associated label');
		}
		if (control.disabled || control.getAttribute('tabindex') === '-1') {
			problems.push('filter control #' + id + ' is not keyboard reachable');
		}
	}
	const panel = document.querySelector('[data-tokedex-filters]');
	if (!panel || !panel.getAttribute('aria-label')) {
		problems.push('filter panel is missing its accessible name');
	}
	const status = document.querySelector('[data-filter-status]');
	if (!status || status.getAttribute('role') !== 'status') {
		problems.push('filter result count is not a status region');
	}
	if (!document.querySelector('[data-filter-reset]')) {
		problems.push('missing reset control');
	}
	if (problems.length > 0) return JSON.stringify(problems);
	const visible = () => Array.from(document.querySelectorAll('[data-model-card]')).filter((card) => !card.hidden);
	const set = (id, value) => {
		const control = controls[id];
		control.value = value;
		control.dispatchEvent(new Event(control.tagName === 'INPUT' ? 'input' : 'change', { bubbles: true }));
	};
	const total = visible().length;
	if (total !== 4) {
		problems.push('expected 4 visible model cards before filtering, found ' + total);
	}
	set('tokedex-search', 'sonnet');
	const byName = visible();
	if (byName.length !== 1 || byName[0].dataset.name !== 'Claude Sonnet 4') {
		problems.push('display-name search did not narrow to Claude Sonnet 4: ' + byName.map((card) => card.dataset.name).join(', '));
	}
	set('tokedex-search', 'gpt-5');
	const byAlias = visible();
	if (byAlias.length !== 1 || byAlias[0].dataset.name !== 'GPT-5') {
		problems.push('alias search did not narrow to GPT-5: ' + byAlias.map((card) => card.dataset.name).join(', '));
	}
	document.querySelector('[data-filter-reset]').click();
	if (visible().length !== total) {
		problems.push('reset did not restore the full grid');
	}
	set('tokedex-provider', 'openai');
	const openai = visible();
	if (openai.length !== 2 || openai.some((card) => card.dataset.provider !== 'openai')) {
		problems.push('provider filter did not limit the grid to the openai cards');
	}
	set('tokedex-provider', '');
	set('tokedex-pricing', 'unknown');
	const unknown = visible();
	if (unknown.length !== 1 || unknown[0].dataset.pricing !== 'unknown') {
		problems.push('pricing filter did not limit the grid to unknown-priced cards');
	}
	set('tokedex-pricing', '');
	set('tokedex-tier', 'B');
	const tierB = visible();
	if (tierB.length !== 1 || tierB[0].dataset.tier !== 'B') {
		problems.push('tier filter did not limit the grid to tier B');
	}
	set('tokedex-tier', '');
	const tierS = () => Array.from(document.querySelectorAll('section[aria-labelledby="tier-S"] [data-model-card]'));
	set('tokedex-sort', 'tokens');
	if (!tierS()[0] || tierS()[0].dataset.name !== 'Claude Sonnet 4') {
		problems.push('sort by tokens did not put the most-used tier S model first');
	}
	set('tokedex-sort', 'name');
	if (!tierS()[0] || tierS()[0].dataset.name !== 'Aardvark Smoke') {
		problems.push('sort by name did not put the alphabetically-first tier S model first');
	}
	document.querySelector('[data-filter-reset]').click();
	return JSON.stringify(problems);
})()`

const tokedexWhatIfInvariantJS = `(() => {
	const problems = [];
	const panel = document.querySelector('.whatif-panel');
	if (!panel) {
		problems.push('missing .whatif-panel');
		return JSON.stringify(problems);
	}
	const summary = panel.querySelector('[data-whatif-summary]');
	if (!summary || !summary.textContent.includes('970') || !summary.textContent.includes('430')) {
		problems.push('whatif summary missing seeded 970 input / 430 output totals');
	}
	const claudeRow = panel.querySelector('[data-whatif-row][data-model-id="claude-sonnet-4"]');
	if (!claudeRow) {
		problems.push('missing whatif row for claude-sonnet-4');
	} else {
		const costCell = claudeRow.querySelector('[data-whatif-cost-cell]');
		if (!costCell || !costCell.textContent.includes('$0.0042')) {
			problems.push('claude-sonnet-4 1x what-if cost = ' + (costCell ? costCell.textContent : 'missing') + ', want $0.0042');
		}
	}
	const gptRow = panel.querySelector('[data-whatif-row][data-model-id="gpt-5"]');
	if (!gptRow) {
		problems.push('missing whatif row for gpt-5');
	} else {
		const costCell = gptRow.querySelector('[data-whatif-cost-cell]');
		if (!costCell || !costCell.textContent.includes('Unknown') || costCell.textContent.includes('$0.00')) {
			problems.push('gpt-5 unknown pricing row must stay Unknown, got ' + (costCell ? costCell.textContent : 'missing'));
		}
	}
	const scale = document.getElementById('whatif-scale');
	const baseline = document.getElementById('whatif-baseline');
	if (!scale || !baseline) {
		problems.push('missing #whatif-scale or #whatif-baseline');
		return JSON.stringify(problems);
	}
	scale.value = '2';
	scale.dispatchEvent(new Event('change', { bubbles: true }));
	if (claudeRow) {
		const costCell = claudeRow.querySelector('[data-whatif-cost-cell]');
		if (!costCell || !costCell.textContent.includes('$0.0084')) {
			problems.push('2x scale did not update claude-sonnet-4 cost to $0.0084, got ' + (costCell ? costCell.textContent : 'missing'));
		}
	}
	baseline.value = 'claude-sonnet-4';
	baseline.dispatchEvent(new Event('change', { bubbles: true }));
	if (claudeRow) {
		const deltaCell = claudeRow.querySelector('[data-whatif-delta-cell]');
		if (!deltaCell || !deltaCell.textContent.includes('Baseline')) {
			problems.push('baseline selection did not mark claude-sonnet-4 as Baseline');
		}
	}
	scale.value = '1';
	scale.dispatchEvent(new Event('change', { bubbles: true }));
	baseline.value = '';
	baseline.dispatchEvent(new Event('change', { bubbles: true }));
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

// connectorHealthInvariantJS is the dedicated gate for the per-source
// connector health panel (issue #26). It goes beyond the page-loop's
// sourceHealthInvariantJS by pinning each seeded adapter to its expected
// state label, asserting the state is announced as text rather than carried
// by colour alone, and checking the no-source machine's explicit copy.
const connectorHealthInvariantJS = `(() => {
	const problems = [];
	const table = document.querySelector('table.health-table');
	if (!table) {
		problems.push('machine health panel is missing');
		return JSON.stringify(problems);
	}
	const rows = Array.from(table.querySelectorAll('tbody tr')).filter(row => !row.classList.contains('empty-row'));
	const reporting = rows.find(row => row.textContent.includes('smoke-laptop'));
	if (!reporting) {
		problems.push('machine health panel lacks the reporting machine row');
		return JSON.stringify(problems);
	}
	const lines = Array.from(reporting.querySelectorAll('.source-health-line'));
	if (lines.length !== 4) {
		problems.push('expected 4 per-source connector rows, found ' + lines.length);
	}
	// Each seeded source has a known shape: fresh sync, stale sync, never
	// synced, and a stale source carrying an error class.
	const expected = {'claude-code': 'Fresh', 'codex': 'Stale', 'zcode': 'Unknown', 'opencode': 'Stale'};
	for (const adapter of Object.keys(expected)) {
		const want = expected[adapter];
		const line = lines.find(candidate => candidate.textContent.includes(adapter));
		if (!line) {
			problems.push('missing connector row for ' + adapter);
			continue;
		}
		const pill = line.querySelector('.status-pill');
		if (!pill) {
			problems.push(adapter + ' row has no status pill');
			continue;
		}
		// The state must reach assistive tech as text, not only as the colour
		// class the stylesheet paints. Assert the announced label itself.
		const announced = (pill.getAttribute('aria-label') || pill.textContent).trim();
		if (announced !== want) {
			problems.push(adapter + ' pill announces ' + JSON.stringify(announced) + ', want ' + want);
		}
		if (pill.getAttribute('aria-hidden') === 'true') {
			problems.push(adapter + ' pill hides its state from assistive tech');
		}
	}
	const freshLine = lines.find(line => line.textContent.includes('claude-code'));
	const staleLine = lines.find(line => line.textContent.includes('codex'));
	if (freshLine && staleLine) {
		const freshPill = freshLine.querySelector('.status-pill');
		const stalePill = staleLine.querySelector('.status-pill');
		if (!freshPill || !stalePill) {
			problems.push('fresh/stale rows lost their status pills');
		} else {
			if (freshPill.textContent.trim() === stalePill.textContent.trim()) {
				problems.push('stale and healthy sources are not distinguished by their announced text');
			}
			if (!freshPill.classList.contains('connected') || !stalePill.classList.contains('stale')) {
				problems.push('fresh/stale pills lost their state classes');
			}
		}
	}
	const unknownLine = lines.find(line => line.textContent.includes('zcode'));
	if (!unknownLine || !unknownLine.textContent.includes('never synced')) {
		problems.push('never-synced source does not say never synced');
	}
	const errorLine = lines.find(line => line.textContent.includes('opencode'));
	if (!errorLine || !errorLine.querySelector('.sync-error') || !errorLine.querySelector('.sync-error').textContent.includes('upload_failed')) {
		problems.push('erroring source does not show its error class inline');
	}
	// A machine that reported no sources must render explicit copy, never an
	// empty sync-context cell.
	const silent = rows.find(row => row.textContent.includes('smoke-silent'));
	if (!silent) {
		problems.push('machine health panel lacks the no-source machine row');
	} else {
		if (silent.querySelector('.source-health-line')) {
			problems.push('no-source machine rendered per-source rows');
		}
		if (!silent.textContent.includes('Heartbeat metadata only')) {
			problems.push('no-source machine lacks its explicit empty copy');
		}
	}
	return JSON.stringify(problems);
})()`

// connectorHealthEmptyInvariantJS covers the panel's empty state: with no
// heartbeat at all the table must say so explicitly instead of rendering an
// empty box.
const connectorHealthEmptyInvariantJS = `(() => {
	const problems = [];
	const table = document.querySelector('table.health-table');
	if (!table) {
		problems.push('machine health panel is missing');
		return JSON.stringify(problems);
	}
	const rows = Array.from(table.querySelectorAll('tbody tr'));
	if (rows.length !== 1 || !rows[0].classList.contains('empty-row')) {
		problems.push('expected one empty machine-health row, found ' + rows.length);
	}
	if (!table.textContent.includes('No machine heartbeats yet.')) {
		problems.push('empty machine health panel lacks its explicit copy');
	}
	if (table.querySelector('.source-health-line') || table.querySelector('.source-health')) {
		problems.push('empty machine health panel rendered an empty source box');
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

// chartRovingInvariantJS guards the analytics charts' roving tabindex: each
// chart exposes exactly one Tab stop (the data lives in the sr-only table, so
// one stop per chart is the whole tab cost), ArrowRight moves focus to the
// next bucket with the stop following, the trend readout still announces the
// focused bucket, and the share chart roves independently of the trend chart.
// It must run after the other /analytics invariants: it drives focus and
// mutates the roving stops.
const chartRovingInvariantJS = `(() => {
	const problems = [];
	const trendColumns = Array.from(document.querySelectorAll('.trend-column'));
	const sharePoints = Array.from(document.querySelectorAll('.share-point'));
	const stops = (buckets) => buckets.filter((bucket) => bucket.getAttribute('tabindex') === '0');
	const roving = (selector, buckets) => {
		if (!buckets.length) {
			problems.push('no ' + selector + ' buckets rendered');
			return null;
		}
		const offPattern = buckets.filter((bucket) => bucket.getAttribute('tabindex') !== '0' && bucket.getAttribute('tabindex') !== '-1');
		if (offPattern.length) {
			problems.push(selector + ' buckets carry tabindex values outside the roving pattern');
		}
		const stopCount = stops(buckets).length;
		if (stopCount !== 1) {
			problems.push('expected exactly 1 ' + selector + '[tabindex="0"], found ' + stopCount);
		}
		return stops(buckets)[0] || buckets[0];
	};

	// ArrowRight moves focus to the next bucket, the roving stop follows, and
	// the live readout announces the focused bucket.
	const trendStop = roving('.trend-column', trendColumns);
	const shareStop = roving('.share-point', sharePoints);
	// Initial placement: the trend stop is the first peak-selected bucket when
	// there is one (else the first bucket); the share chart has no
	// server-rendered selection, so its stop is the first bucket.
	if (trendStop && trendStop !== trendColumns[0] && !(trendStop.classList.contains('peak') && trendStop.classList.contains('selected'))) {
		problems.push('trend tab stop is neither the first bucket nor a peak-selected bucket');
	}
	if (shareStop && shareStop !== sharePoints[0]) {
		problems.push('share tab stop is not the first bucket');
	}
	if (trendColumns.length >= 2) {
		const first = trendColumns[0];
		const next = trendColumns[1];
		const readout = document.getElementById('trend-readout-value');
		first.focus();
		const readoutBefore = readout ? readout.textContent : '';
		first.dispatchEvent(new KeyboardEvent('keydown', {key: 'ArrowRight', bubbles: true, cancelable: true}));
		if (document.activeElement !== next) {
			problems.push('ArrowRight did not move trend focus to the next bucket (activeElement: ' + (document.activeElement.className || document.activeElement.tagName) + ')');
		} else {
			const stopsNow = stops(trendColumns);
			if (stopsNow.length !== 1 || stopsNow[0] !== next) {
				problems.push('trend roving tab stop did not follow focus after ArrowRight');
			}
			if (readout && readout.textContent === readoutBefore) {
				problems.push('trend readout did not update after arrowing to ' + next.dataset.label);
			}
		}
	}
	const trendStopBeforeShare = stops(trendColumns)[0] || null;

	// The share chart roves independently: its stop follows focus and the
	// trend chart's stop is untouched.
	if (sharePoints.length >= 2) {
		const first = sharePoints[0];
		const next = sharePoints[1];
		first.focus();
		first.dispatchEvent(new KeyboardEvent('keydown', {key: 'ArrowRight', bubbles: true, cancelable: true}));
		if (document.activeElement !== next) {
			problems.push('ArrowRight did not move share focus to the next bucket (activeElement: ' + (document.activeElement.className || document.activeElement.tagName) + ')');
		} else {
			const stopsNow = stops(sharePoints);
			if (stopsNow.length !== 1 || stopsNow[0] !== next) {
				problems.push('share roving tab stop did not follow focus after ArrowRight');
			}
		}
		if ((stops(trendColumns)[0] || null) !== trendStopBeforeShare) {
			problems.push('share chart navigation disturbed the trend chart roving stop');
		}
	}

	// Leave the page as found: focus in the share chart would otherwise keep
	// the tooltip open for the axe scan.
	if (document.activeElement && document.activeElement !== document.body) {
		document.activeElement.blur();
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
	const announcedName = (control) => (control.getAttribute('aria-label') || control.textContent).trim();
	const refresh = document.getElementById('refresh-now');
	if (!refresh || announcedName(refresh) !== 'Refresh now') {
		problems.push('overview lacks the labeled Refresh now button');
	}
	const toggle = document.getElementById('auto-refresh-toggle');
	if (!toggle) {
		problems.push('overview lacks the auto-refresh pause toggle');
	} else {
		if (toggle.getAttribute('aria-pressed') === null) {
			problems.push('auto-refresh toggle lacks aria-pressed');
		}
		if (!['Pause auto-refresh', 'Resume auto-refresh'].includes(announcedName(toggle))) {
			problems.push('auto-refresh toggle has an unexpected label: ' + announcedName(toggle));
		}
	}
	// Phone width renders both controls icon-only: the visible glyph replaces
	// the text label, the announced name stays on aria-label, and the touch
	// target grows to at least 32x32. The rules are scoped to the widths where
	// the phone layout is active so the same invariant gates every viewport.
	if (window.innerWidth <= 500) {
		for (const control of [refresh, toggle]) {
			if (!control) continue;
			const box = control.getBoundingClientRect();
			if (box.width < 32 || box.height < 32) {
				problems.push(control.id + ' touch target is ' + Math.round(box.width) + 'x' + Math.round(box.height) + ', want at least 32x32');
			}
			const glyph = control.querySelector('.refresh-glyph');
			const label = control.querySelector('.refresh-label');
			if (!glyph || getComputedStyle(glyph).display === 'none') {
				problems.push(control.id + ' has no visible icon glyph at phone width');
			}
			if (!label || getComputedStyle(label).display !== 'none') {
				problems.push(control.id + ' still renders its text label at phone width; expected icon-only');
			}
			if (!announcedName(control)) {
				problems.push(control.id + ' has no accessible name at phone width');
			}
		}
		if (toggle) {
			const pressed = toggle.getAttribute('aria-pressed');
			const want = pressed === 'true' ? '\u25B6' : '\u23F8';
			const glyph = toggle.querySelector('.refresh-glyph');
			if (!glyph || glyph.textContent !== want) {
				problems.push('pause glyph at aria-pressed=' + pressed + ' is ' + (glyph ? glyph.textContent : 'none') + ', want ' + want);
			}
		}
	}
	return JSON.stringify(problems);
})()`

// activityGridInvariantJS asserts the overview heatmap's roving tabindex: the
// role="grid" contract is arrow-key cell navigation, so the grid must load
// with exactly one tabbable cell (the most recent active day), future and
// aria-hidden cells must stay non-focusable, and Left/Right plus Up/Down must
// move focus a day and a week with the tab stop following the focus.
const activityGridInvariantJS = `(() => {
	const problems = [];
	const grid = document.querySelector('.activity-grid');
	if (!grid) {
		return JSON.stringify(['overview lacks the .activity-grid']);
	}
	const navigable = (cell) => !cell.classList.contains('future') && cell.getAttribute('aria-hidden') !== 'true';
	const cells = Array.from(grid.querySelectorAll('.activity-cell'));
	for (const cell of cells) {
		if (!navigable(cell) && cell.getAttribute('tabindex') !== null) {
			problems.push('future or aria-hidden cell is focusable: ' + cell.className);
		}
	}
	const dayCells = cells.filter(navigable);
	const stops = dayCells.filter((cell) => cell.getAttribute('tabindex') === '0');
	if (stops.length !== 1) {
		problems.push('activity grid has ' + stops.length + ' tabindex="0" cells, want exactly 1');
		return JSON.stringify(problems);
	}
	const usage = dayCells.filter((cell) => /level-[1-4]/.test(cell.className) || cell.classList.contains('unknown'));
	if (usage.length > 0 && stops[0] !== usage[usage.length - 1]) {
		problems.push('the initial tab stop is not the most recent active day');
	}
	const rows = Array.from(grid.querySelectorAll('.activity-week'));
	if (rows.length < 3) {
		problems.push('activity grid has ' + rows.length + ' week rows, want at least 3');
		return JSON.stringify(problems);
	}
	const columnOf = (cell) => Array.from(cell.closest('.activity-week').querySelectorAll('.activity-cell')).indexOf(cell);
	const press = (cell, key) => cell.dispatchEvent(new KeyboardEvent('keydown', { key: key, bubbles: true, cancelable: true }));
	// Drive the arrows from a cell with room in every direction: the first day
	// of the third-to-last week. The rows below it are fully past, so ArrowDown
	// lands on the same weekday regardless of which weekday today is.
	const startRow = rows[rows.length - 3];
	const start = Array.from(startRow.querySelectorAll('.activity-cell')).filter(navigable)[0];
	if (!start) {
		problems.push('the third-to-last week has no navigable day cell');
		return JSON.stringify(problems);
	}
	// Position the origin of the keyboard events. This headless page has no
	// window focus, so focus() moves activeElement but fires no focus events
	// and :focus does not match; the arrows carry the roving tab stop
	// themselves, which is exactly what the assertions below check.
	start.focus();
	if (document.activeElement !== start) {
		problems.push('could not focus an activity cell');
		return JSON.stringify(problems);
	}
	press(start, 'ArrowRight');
	const afterRight = document.activeElement;
	if (afterRight === start) {
		problems.push('ArrowRight did not move focus');
		return JSON.stringify(problems);
	}
	if (afterRight.closest('.activity-week') !== startRow || columnOf(afterRight) !== columnOf(start) + 1) {
		problems.push('ArrowRight did not land on the next day cell of the same week');
	}
	if (afterRight.getAttribute('tabindex') !== '0' || start.getAttribute('tabindex') !== '-1') {
		problems.push('the roving tab stop did not follow ArrowRight');
	}
	press(afterRight, 'ArrowDown');
	const afterDown = document.activeElement;
	const downRow = rows[rows.length - 2];
	if (afterDown === afterRight) {
		problems.push('ArrowDown did not move focus');
	} else {
		if (afterDown.closest('.activity-week') !== downRow || columnOf(afterDown) !== columnOf(afterRight)) {
			problems.push('ArrowDown did not land on the same weekday in the next week');
		}
		if (afterDown.getAttribute('tabindex') !== '0' || afterRight.getAttribute('tabindex') !== '-1') {
			problems.push('the roving tab stop did not follow ArrowDown');
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

// creatureArtInvariantJS verifies the hero .creature-art LCP image loads with
// explicit manifest dimensions, fetchpriority="high", and zero aspect-ratio
// distortion across all gated viewports (both 640x640 and stage-09 568x640).
const creatureArtInvariantJS = `(() => {
	const problems = [];
	const img = document.querySelector('img.creature-art');
	if (!img) {
		return JSON.stringify(['missing img.creature-art']);
	}
	if (img.getAttribute('fetchpriority') !== 'high') {
		problems.push('creature-art fetchpriority=' + JSON.stringify(img.getAttribute('fetchpriority')) + ', want "high"');
	}
	if (!img.complete || img.naturalWidth <= 0 || img.naturalHeight <= 0) {
		problems.push('creature-art failed to load: complete=' + img.complete + ' natural=' + img.naturalWidth + 'x' + img.naturalHeight);
		return JSON.stringify(problems);
	}
	const attrW = Number(img.getAttribute('width'));
	const attrH = Number(img.getAttribute('height'));
	if (attrW !== img.naturalWidth || attrH !== img.naturalHeight) {
		problems.push('creature-art HTML dimensions ' + attrW + 'x' + attrH + ' != natural ' + img.naturalWidth + 'x' + img.naturalHeight);
	}
	const rect = img.getBoundingClientRect();
	if (rect.width <= 0 || rect.height <= 0) {
		problems.push('creature-art rendered box is empty: ' + rect.width + 'x' + rect.height);
	} else {
		const renderedRatio = rect.width / rect.height;
		const naturalRatio = img.naturalWidth / img.naturalHeight;
		if (Math.abs(renderedRatio - naturalRatio) >= 0.02) {
			problems.push('creature-art aspect ratio distorted: rendered=' + renderedRatio.toFixed(4) + ' natural=' + naturalRatio.toFixed(4));
		}
	}
	return JSON.stringify(problems);
})()`

// checkBelowFoldPanels verifies content-visibility: auto is applied to the
// listed below-the-fold panel selectors and that keyboard focus reaches
// interactive/tabbable descendants inside each skipped subtree.
func belowFoldReachabilityJS(selectors ...string) string {
	quoted := make([]string, len(selectors))
	for i, sel := range selectors {
		quoted[i] = fmt.Sprintf("%q", sel)
	}
	return `(() => {
		const problems = [];
		const selectors = [` + strings.Join(quoted, ",") + `];
		for (const sel of selectors) {
			const panel = document.querySelector(sel);
			if (!panel) {
				problems.push('missing below-the-fold panel ' + sel);
				continue;
			}
			const cv = getComputedStyle(panel).contentVisibility;
			if (cv !== 'auto') {
				problems.push(sel + ' content-visibility=' + JSON.stringify(cv) + ', want "auto"');
			}
			const target = panel.querySelector('a[href], button:not([disabled]), [tabindex="0"]');
			if (!target) {
				problems.push(sel + ' has no keyboard-reachable descendant');
				continue;
			}
			target.focus();
			if (document.activeElement !== target) {
				problems.push('focus could not reach ' + sel + ' descendant ' + target.tagName);
			} else {
				const rect = target.getBoundingClientRect();
				if (rect.height <= 0) {
					problems.push(sel + ' focused descendant has zero height');
				}
			}
		}
		if (document.activeElement && document.activeElement !== document.body) {
			document.activeElement.blur();
		}
		window.scrollTo(0, 0);
		return JSON.stringify(problems);
	})()`
}

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

const sessionsFilterInvariantJS = `(() => {
	const problems = [];
	for (const id of ['sessions-search', 'sessions-project', 'sessions-from', 'sessions-to', 'sessions-period', 'sessions-machine', 'sessions-provider', 'sessions-model', 'sessions-tool']) {
		const control = document.getElementById(id);
		if (!control) {
			problems.push('missing filter control #' + id);
			continue;
		}
		if (!document.querySelector('label[for="' + id + '"]')) {
			problems.push('missing explicit label[for="' + id + '"]');
		}
		control.focus();
		if (document.activeElement !== control) {
			problems.push('filter control #' + id + ' is not keyboard-reachable');
		}
	}
	if (document.activeElement && document.activeElement !== document.body) {
		document.activeElement.blur();
	}
	return JSON.stringify(problems);
})()`

const sessionDetailInvariantJS = `(() => {
	const problems = [];
	if (!document.querySelector('h1')) {
		problems.push('session detail missing <h1>');
	}
	const copyBtn = document.querySelector('.copy-id[data-copy-value="smoke-sess-alpha"]');
	if (!copyBtn) {
		problems.push('session detail missing copy-id button for smoke-sess-alpha');
	}
	if (!document.querySelector('a[href="/sessions"]')) {
		problems.push('session detail missing back link to /sessions');
	}
	if (!document.querySelector('table')) {
		problems.push('session detail missing semantic breakdown/timeline table');
	}
	const html = document.documentElement.innerHTML;
	if (html.includes('/Users/') || html.includes('/home/') || html.includes('ZgotmplZ')) {
		problems.push('session detail leaked path or template error marker');
	}
	return JSON.stringify(problems);
})()`

const tokedexDetailInvariantJS = `(() => {
	const problems = [];
	const h1 = document.querySelector('h1');
	if (!h1 || !h1.textContent.includes('Claude Sonnet 4')) {
		problems.push('tokedex detail missing Claude Sonnet 4 heading');
	}
	if (!document.querySelector('a[href="/tokedex"]')) {
		problems.push('tokedex detail missing back link to /tokedex');
	}
	if (!document.querySelector('.copy-id[data-copy-value="claude-sonnet-4"]')) {
		problems.push('tokedex detail missing copy-id button for claude-sonnet-4');
	}
	if (!document.querySelector('table[aria-label="Model machine distribution"]')) {
		problems.push('tokedex detail missing Model machine distribution table');
	}
	const html = document.documentElement.innerHTML;
	if (html.includes('/Users/') || html.includes('/home/') || html.includes('ZgotmplZ')) {
		problems.push('tokedex detail leaked path or template error marker');
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

// analyticsToolbarInvariantJS checks the decluttered /analytics toolbar:
// duplicate #dimension-filter is gone (replaced by hidden dimension input),
// secondary filters and export links start collapsed inside <details>, and
// every filter select has an explicit label[for].
const analyticsToolbarInvariantJS = `(() => {
	const problems = [];
	if (document.getElementById('dimension-filter')) {
		problems.push('duplicate #dimension-filter select must be removed');
	}
	if (!document.querySelector('input[type="hidden"][name="dimension"]')) {
		problems.push('missing hidden dimension input in filter form');
	}
	if (!document.querySelector('input[type="hidden"][name="period"]')) {
		problems.push('missing hidden period input in filter form');
	}
	const filterDetails = document.querySelector('details.filter-details');
	if (!filterDetails) {
		problems.push('missing details.filter-details');
	} else if (filterDetails.open) {
		problems.push('details.filter-details should be collapsed by default');
	}
	const exportMenu = document.querySelector('details.export-menu');
	if (!exportMenu) {
		problems.push('missing details.export-menu');
	} else if (exportMenu.open) {
		problems.push('details.export-menu should be collapsed by default');
	}
	for (const id of ['project-filter', 'machine-filter', 'provider-filter', 'model-filter', 'harness-filter']) {
		if (!document.getElementById(id)) {
			problems.push('missing filter select #' + id);
		}
		if (!document.querySelector('label[for="' + id + '"]')) {
			problems.push('missing label[for="' + id + '"]');
		}
	}
	if (!document.querySelector('.filter-form button[type="submit"]')) {
		problems.push('missing Apply filters submit button');
	}
	return JSON.stringify(problems);
})()`

// insightsRovingInvariantJS checks /insights 24-hour rhythm roving tabindex,
// live readout, and sr-only semantic table.
const insightsRovingInvariantJS = `(() => {
	const problems = [];
	const hist = document.querySelector('.histogram');
	if (!hist) {
		problems.push('missing .histogram');
		return JSON.stringify(problems);
	}
	if (hist.getAttribute('role') !== 'region') {
		problems.push('.histogram role = ' + hist.getAttribute('role') + ', want region');
	}
	const hours = Array.from(hist.querySelectorAll('button.hour[type="button"]'));
	if (hours.length !== 24) {
		problems.push('expected 24 button.hour[type="button"] elements, got ' + hours.length);
	}
	const stops = hours.filter((h) => h.getAttribute('tabindex') === '0');
	if (stops.length !== 1) {
		problems.push('expected exactly 1 .hour[tabindex="0"], got ' + stops.length);
	}
	const offPattern = hours.filter((h) => h.getAttribute('tabindex') !== '0' && h.getAttribute('tabindex') !== '-1');
	if (offPattern.length) {
		problems.push('.hour buttons carry tabindex outside roving pattern');
	}
	const peak = hist.querySelector('.hour.peak');
	if (peak && stops[0] !== peak) {
		problems.push('initial roving stop is not the peak hour');
	}
	const readout = document.getElementById('rhythm-readout-value');
	if (!readout || !document.querySelector('.rhythm-readout[aria-live="polite"]')) {
		problems.push('missing .rhythm-readout[aria-live="polite"] or #rhythm-readout-value');
	}
	if (hours.length >= 24) {
		hours[0].focus();
		const before = readout ? readout.textContent : '';
		hours[0].dispatchEvent(new KeyboardEvent('keydown', { key: 'ArrowRight', bubbles: true, cancelable: true }));
		if (document.activeElement !== hours[1]) {
			problems.push('ArrowRight did not move focus from hour 0 to hour 1');
		}
		if (hours[1].getAttribute('tabindex') !== '0' || hours[0].getAttribute('tabindex') !== '-1') {
			problems.push('roving tabindex did not follow ArrowRight');
		}
		if (readout && readout.textContent === before) {
			problems.push('rhythm readout did not update on ArrowRight');
		}
		hours[1].dispatchEvent(new KeyboardEvent('keydown', { key: 'End', bubbles: true, cancelable: true }));
		if (document.activeElement !== hours[23]) {
			problems.push('End did not move focus to hour 23');
		}
		hours[23].dispatchEvent(new KeyboardEvent('keydown', { key: 'Home', bubbles: true, cancelable: true }));
		if (document.activeElement !== hours[0]) {
			problems.push('Home did not move focus to hour 0');
		}
	}
	const tableRows = document.querySelectorAll('#rhythm-data-table table[aria-label="24-hour rhythm data table"] tbody tr');
	if (tableRows.length !== 24) {
		problems.push('expected 24 rows in #rhythm-data-table, got ' + tableRows.length);
	}
	if (document.activeElement && document.activeElement !== document.body) {
		document.activeElement.blur();
	}
	return JSON.stringify(problems);
})()`

// settingsAliasInvariantJS checks /settings usage-sorted aliases, live filter,
// per-row Reset focus restoration, and Analytics/Tokedex cross-links.
const settingsAliasInvariantJS = `(() => {
	const problems = [];
	const filter = document.getElementById('alias-filter');
	if (!filter) {
		problems.push('missing #alias-filter');
		return JSON.stringify(problems);
	}
	if (!document.querySelector('label[for="alias-filter"]')) {
		problems.push('missing label[for="alias-filter"]');
	}
	if (filter.closest('form[action="/settings/aliases"]')) {
		problems.push('#alias-filter must sit outside the alias POST form');
	}
	const status = document.getElementById('alias-filter-status');
	if (!status) {
		problems.push('missing #alias-filter-status');
	}
	const modelRows = Array.from(document.querySelectorAll('[data-alias-row][data-kind="model"]'));
	if (modelRows.length < 2) {
		problems.push('expected at least 2 seeded model alias rows');
	} else {
		if (modelRows[0].dataset.identity !== 'claude-sonnet-4' || modelRows[1].dataset.identity !== 'gpt-5') {
			problems.push('model rows not sorted by usage tokens DESC: ' + modelRows.map((r) => r.dataset.identity).join(','));
		}
		if (!modelRows[0].querySelector('a[href^="/analytics?dimension=models&model="]')) {
			problems.push('model row missing Analytics cross-link');
		}
		if (!modelRows[0].querySelector('a[href^="/tokedex?q="]')) {
			problems.push('model row missing Tokedex cross-link when enabled');
		}
		const usage = modelRows[0].querySelector('.alias-usage');
		if (!usage || !usage.textContent.includes('tokens')) {
			problems.push('model row missing usage summary');
		}
	}
	filter.value = 'gpt-5';
	filter.dispatchEvent(new Event('input', { bubbles: true }));
	const hiddenCount = modelRows.filter((r) => r.hidden).length;
	if (hiddenCount === 0) {
		problems.push('#alias-filter did not hide non-matching model rows');
	}
	filter.value = '';
	filter.dispatchEvent(new Event('input', { bubbles: true }));
	if (modelRows.some((r) => r.hidden)) {
		problems.push('clearing #alias-filter left rows hidden');
	}
	if (modelRows.length > 0) {
		const input = modelRows[0].querySelector('[data-alias-input]');
		const reset = modelRows[0].querySelector('[data-reset-alias]');
		if (!input || !reset) {
			problems.push('missing input or reset button on first model row');
		} else {
			const original = input.value;
			input.value = 'Temp Alias';
			input.dispatchEvent(new Event('input', { bubbles: true }));
			if (reset.disabled) {
				problems.push('reset button remained disabled after typing alias');
			}
			reset.click();
			if (input.value !== '' || !reset.disabled || document.activeElement !== input) {
				problems.push('reset button did not clear input, disable itself, and restore focus');
			}

			// Regression: editing or resetting a row while #alias-filter matches
			// only that row's custom alias must keep the row visible and keep
			// focus on the row's input (never dropping to document.body).
			input.value = 'UniqueSmokeAlias';
			input.dispatchEvent(new Event('input', { bubbles: true }));
			filter.value = 'UniqueSmokeAlias';
			filter.dispatchEvent(new Event('input', { bubbles: true }));
			if (modelRows[0].hidden) {
				problems.push('row hidden when #alias-filter matches its custom alias');
			}
			input.focus();
			input.value = 'EditedSmokeAlias';
			input.dispatchEvent(new Event('input', { bubbles: true }));
			if (modelRows[0].hidden || document.activeElement !== input) {
				problems.push('editing row while filtered by old alias hid row or dropped focus');
			}
			filter.value = 'EditedSmokeAlias';
			filter.dispatchEvent(new Event('input', { bubbles: true }));
			reset.click();
			if (input.value !== '' || !reset.disabled || modelRows[0].hidden || document.activeElement !== input) {
				problems.push('reset while filtered by own alias hid row or dropped focus to ' + (document.activeElement ? document.activeElement.tagName : 'none'));
			}
			if (status && !status.textContent.includes('Showing 1 of')) {
				problems.push('status count inaccurate while focused reset row stayed visible: ' + status.textContent);
			}
			const emptyModels = document.querySelector('[data-filter-empty="models"]');
			if (emptyModels && !emptyModels.hidden) {
				problems.push('empty-models notice shown while focused reset row is still visible');
			}

			filter.value = '';
			filter.dispatchEvent(new Event('input', { bubbles: true }));
			input.value = original;
			input.dispatchEvent(new Event('input', { bubbles: true }));
		}
	}
	if (document.activeElement && document.activeElement !== document.body) {
		document.activeElement.blur();
	}
	return JSON.stringify(problems);
})()`

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
	ctx, cancel := context.WithTimeout(browserCtx, 120*time.Second)
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
	// The headless tab is never OS-focused, so Chrome delivers no focus events
	// (element.focus() moves activeElement silently) and keyboard-driven
	// roving tabindex would be untestable. Focus emulation makes the page
	// behave like a focused tab for the session.
	if _, err := chromedp.Call(ctx, emulation.SetFocusEmulationEnabled, emulation.SetFocusEmulationEnabledParams{Enabled: true}); err != nil {
		t.Fatalf("could not enable focus emulation: %v", err)
	}
	if reduced, err := chromedp.Run(ctx, chromedp.Evaluate[bool](`window.matchMedia('(prefers-reduced-motion: reduce)').matches`)); err != nil || !reduced {
		t.Fatalf("reduced-motion emulation did not apply (matches=%v, err=%v)", reduced, err)
	}

	pages := []struct {
		path       string
		invariants []string
	}{
		{path: "/", invariants: []string{
			freshnessInvariantJS,
			sourceHealthInvariantJS,
			refreshControlInvariantJS,
			activityGridInvariantJS,
			creatureArtInvariantJS,
			quotaAbsentInvariantJS,
			belowFoldReachabilityJS(".health-panel", ".sessions-panel"),
		}},
		{path: "/analytics?period=7d&dimension=models", invariants: []string{
			exportInvariantJS,
			analyticsToolbarInvariantJS,
			quotaAbsentInvariantJS,
			belowFoldReachabilityJS(".share-panel", ".sessions-panel"),
			chartRovingInvariantJS,
		}},
		{path: "/tokedex", invariants: []string{tokedexInvariantJS, tokedexFilterInvariantJS, tokedexWhatIfInvariantJS}},
		{path: "/tokedex/claude-sonnet-4", invariants: []string{tokedexDetailInvariantJS}},
		{path: "/sessions", invariants: []string{exportInvariantJS, sortInvariantJS, sessionsFilterInvariantJS}},
		{path: "/sessions/smoke-sess-alpha", invariants: []string{sessionDetailInvariantJS}},
		{path: "/insights", invariants: []string{insightsRovingInvariantJS}},
		{path: "/settings", invariants: []string{settingsAliasInvariantJS}},
	}
	for _, page := range pages {
		t.Run(page.path, func(t *testing.T) {
			assertPageAtViewports(t, ctx, baseURL, page.path, page.invariants)
		})
	}
	// Leave the browser at the desktop viewport for the subtests below, which
	// predate the viewport loop and still exercise the wide layout.
	if _, err := chromedp.Run(ctx, chromedp.EmulateViewport(1280, 800)); err != nil {
		t.Fatalf("could not restore the desktop viewport: %v", err)
	}

	// The sign-in form is outside the authenticated page list: it only
	// renders when a password account is configured, so it gets its own
	// server, and it is held to the same viewport gate.
	t.Run("/login", func(t *testing.T) {
		passwordURL := startSeededPasswordServer(t)
		assertPageAtViewports(t, ctx, passwordURL, "/login", nil)
	})
	if _, err := chromedp.Run(ctx, chromedp.EmulateViewport(1280, 800)); err != nil {
		t.Fatalf("could not restore the desktop viewport: %v", err)
	}

	// Stage 9 (token-titan, stage-09.png) has natural dimensions 568x640
	// instead of 640x640; verify its explicit width/height attributes and
	// distortion-free rendering across all gated viewports.
	t.Run("creature-art-stage9", func(t *testing.T) {
		stage9Store, err := database.Open(filepath.Join(t.TempDir(), "stage9.db"), smokeCatalog())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { stage9Store.Close() })
		if _, err := stage9Store.Ingest(context.Background(), []usage.Event{{
			SchemaVersion: usage.SchemaVersion,
			EventID:       "smoke-stage9-event",
			Timestamp:     time.Now().UTC().Add(-time.Minute),
			MachineID:     "smoke-laptop",
			Project:       "smoke-project",
			Provider:      "anthropic",
			Model:         "claude-sonnet-4",
			Tool:          "claude-code",
			InputTokens:   usage.Int64(1_000_000_000),
			OutputTokens:  usage.Int64(999_999_999),
			TotalTokens:   usage.Int64(1_999_999_999),
			Currency:      "USD",
			TokenAccuracy: usage.AccuracyReported,
			Source:        usage.Source{Adapter: "claude-code", AdapterVersion: "smoke"},
		}}); err != nil {
			t.Fatal(err)
		}
		stage9URL := serveStore(t, stage9Store, false)
		assertPageAtViewports(t, ctx, stage9URL, "/", []string{creatureArtInvariantJS})
	})
	if _, err := chromedp.Run(ctx, chromedp.EmulateViewport(1280, 800)); err != nil {
		t.Fatalf("could not restore the desktop viewport: %v", err)
	}

	// The per-source connector health panel (issue #26) in a real browser:
	// one row per seeded source with its state announced as text, a stale
	// source visibly (not colour-only) distinguished from a healthy one, the
	// no-source machine's explicit copy, and the empty panel's explicit copy.
	t.Run("connector-health", func(t *testing.T) {
		navigate := func(url string) {
			t.Helper()
			if _, err := chromedp.Run(ctx, chromedp.Navigate(url+"/")); err != nil {
				t.Fatalf("navigation to %s failed: %v", url, err)
			}
			if _, err := chromedp.Run(ctx, chromedp.WaitReady("table.health-table")); err != nil {
				t.Fatalf("machine health panel never appeared on %s: %v", url, err)
			}
			if _, err := chromedp.Run(ctx, chromedp.Evaluate[bool](settleAnimationJS)); err != nil {
				t.Fatalf("could not settle animations on %s: %v", url, err)
			}
		}

		// Seeded sources: fresh, stale, never synced, and stale-with-error,
		// plus a machine that reported no sources at all.
		navigate(baseURL)
		for _, invariant := range []string{sharedInvariantJS, connectorHealthInvariantJS} {
			assertNoProblems(t, ctx, invariant)
		}
		assertAxeAllowlist(t, ctx, "/")

		// No heartbeat yet: the panel must say so instead of an empty box.
		emptyURL := serveStore(t, seedEmptyStore(t), false)
		navigate(emptyURL)
		for _, invariant := range []string{sharedInvariantJS, connectorHealthEmptyInvariantJS} {
			assertNoProblems(t, ctx, invariant)
		}
		assertAxeAllowlist(t, ctx, "/")
	})

	// Multi-source same-adapter machines group into one summary line per
	// adapter with a collapsed <details> panel that expands cleanly across all
	// gated viewports without horizontal overflow or axe violations.
	t.Run("connector-health-grouped", func(t *testing.T) {
		groupedStore, err := database.Open(filepath.Join(t.TempDir(), "grouped.db"), smokeCatalog())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { groupedStore.Close() })
		now := time.Now().UTC()
		freshSync := now.Add(-time.Minute)
		staleSync := now.Add(-2 * time.Hour)
		if err := groupedStore.RecordHeartbeat(context.Background(), database.AgentHeartbeat{
			MachineID:        "smoke-grouped",
			OperatingSystem:  "darwin",
			Architecture:     "arm64",
			SourceCount:      4,
			SourceErrorCount: 1,
			Sources: []database.AgentSourceHealth{
				{Adapter: "claude-code", SourceHash: "smoke-cc-1", LastSuccessfulSync: &freshSync},
				{Adapter: "codex", SourceHash: "smoke-codex-1", LastSuccessfulSync: &freshSync},
				{Adapter: "codex", SourceHash: "smoke-codex-2", LastSuccessfulSync: &freshSync},
				{Adapter: "codex", SourceHash: "smoke-codex-3", LastSuccessfulSync: &staleSync, LastErrorClass: "timeout"},
			},
		}); err != nil {
			t.Fatal(err)
		}
		groupedURL := serveStore(t, groupedStore, false)
		for _, viewport := range gateViewports {
			t.Run(viewport.name, func(t *testing.T) {
				if _, err := chromedp.Run(ctx, chromedp.EmulateViewport(viewport.width, viewport.height)); err != nil {
					t.Fatalf("could not emulate %s: %v", viewport.name, err)
				}
				if _, err := chromedp.Run(ctx, chromedp.Navigate(groupedURL+"/")); err != nil {
					t.Fatalf("navigation failed at %s: %v", viewport.name, err)
				}
				if _, err := chromedp.Run(ctx, chromedp.WaitReady("details.source-details")); err != nil {
					t.Fatalf("details.source-details missing at %s: %v", viewport.name, err)
				}
				if _, err := chromedp.Run(ctx, chromedp.Evaluate[bool](settleAnimationJS)); err != nil {
					t.Fatalf("could not settle animations at %s: %v", viewport.name, err)
				}
				assertNoProblems(t, ctx, sharedInvariantJS)
				collapsedProblems := `(() => {
					const problems = [];
					const lines = document.querySelectorAll('.source-health-line');
					if (lines.length !== 2) problems.push('expected 2 grouped adapter lines, got ' + lines.length);
					const details = document.querySelector('details.source-details');
					if (!details || details.open) problems.push('expected details.source-details collapsed by default');
					return JSON.stringify(problems);
				})()`
				assertNoProblems(t, ctx, collapsedProblems)
				assertAxeAllowlist(t, ctx, "/")

				// .health-panel uses content-visibility: auto; scrolling it into
				// view and waiting a render frame lets Chrome unlock hit-testing
				// for the previously offscreen subtree before clicking.
				if _, err := chromedp.Run(ctx, chromedp.ScrollIntoView("summary.source-details-summary")); err != nil {
					t.Fatalf("could not scroll source-details at %s: %v", viewport.name, err)
				}
				if _, err := chromedp.Run(ctx, chromedp.Evaluate[bool](`new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(() => resolve(true))))`, chromedp.EvalAwaitPromise)); err != nil {
					t.Fatalf("could not await render frame at %s: %v", viewport.name, err)
				}
				if _, err := chromedp.Run(ctx, chromedp.Click("summary.source-details-summary")); err != nil {
					t.Fatalf("could not expand source-details at %s: %v", viewport.name, err)
				}
				expandedProblems := `(() => {
					const problems = [];
					const details = document.querySelector('details.source-details');
					if (!details || !details.open) problems.push('details.source-details did not open on summary click');
					const items = document.querySelectorAll('.source-detail-line');
					if (items.length !== 1 || !items[0].textContent.includes('timeout')) {
						problems.push('expected 1 stale/errored detail row with timeout, got ' + items.length);
					}
					return JSON.stringify(problems);
				})()`
				assertNoProblems(t, ctx, expandedProblems)
				assertNoProblems(t, ctx, sharedInvariantJS)
				assertAxeAllowlist(t, ctx, "/")
			})
		}
		if _, err := chromedp.Run(ctx, chromedp.EmulateViewport(1280, 800)); err != nil {
			t.Fatalf("could not restore desktop viewport: %v", err)
		}
	})

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
		if _, err := chromedp.Run(disabledRunCtx, chromedp.Navigate(disabledURL+"/settings")); err != nil {
			t.Fatalf("navigation to disabled /settings failed: %v", err)
		}
		settingsTokedexCount, err := chromedp.Run(disabledRunCtx, chromedp.Evaluate[int](`document.querySelectorAll('a[href^="/tokedex"]').length`))
		if err != nil {
			t.Fatalf("settings tokedex link check failed: %v", err)
		}
		if settingsTokedexCount != 0 {
			t.Fatalf("disabled /settings still renders %d Tokedex links", settingsTokedexCount)
		}
		detailResp, err := chromedp.RunResponse(disabledRunCtx, chromedp.Navigate(disabledURL+"/tokedex/claude-sonnet-4"))
		if err != nil {
			t.Fatalf("navigation to disabled /tokedex/claude-sonnet-4 failed: %v", err)
		}
		if detailResp.Status != http.StatusNotFound {
			t.Fatalf("disabled /tokedex/claude-sonnet-4 status = %d, want %d", detailResp.Status, http.StatusNotFound)
		}
	})

	t.Run("tokedex-detail", func(t *testing.T) {
		if _, err := chromedp.Run(ctx, chromedp.Navigate(baseURL+"/tokedex")); err != nil {
			t.Fatalf("navigate /tokedex: %v", err)
		}
		if _, err := chromedp.Run(ctx, chromedp.WaitReady(`a.model-card-link[href="/tokedex/claude-sonnet-4"]`)); err != nil {
			t.Fatalf("model card link missing: %v", err)
		}
		if _, err := chromedp.RunResponse(ctx, chromedp.Click(`a.model-card-link[href="/tokedex/claude-sonnet-4"]`)); err != nil {
			t.Fatalf("tokedex detail click-through failed: %v", err)
		}
		if _, err := chromedp.Run(ctx, chromedp.WaitReady(`a[href="/sessions/smoke-sess-alpha"]`)); err != nil {
			t.Fatalf("tokedex detail session link missing: %v", err)
		}
		// Alias containing '/' resolves cleanly to canonical GPT-5 with Unknown pricing.
		if _, err := chromedp.Run(ctx, chromedp.Navigate(baseURL+"/tokedex/openai/gpt-5")); err != nil {
			t.Fatalf("navigate /tokedex/openai/gpt-5: %v", err)
		}
		aliasText, err := chromedp.Run(ctx, chromedp.Evaluate[string](`document.body.textContent`))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(aliasText, "GPT-5") || !strings.Contains(aliasText, "Rates not published yet") || strings.Contains(aliasText, "$0.00") {
			t.Fatalf("alias detail /tokedex/openai/gpt-5 rendered unexpected copy: %q", aliasText)
		}
		// Zero-usage model renders cleanly with semantic table.
		if _, err := chromedp.Run(ctx, chromedp.Navigate(baseURL+"/tokedex/smoke-zero-usage")); err != nil {
			t.Fatalf("navigate /tokedex/smoke-zero-usage: %v", err)
		}
		zeroText, err := chromedp.Run(ctx, chromedp.Evaluate[string](`document.body.textContent`))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(zeroText, "No usage recorded yet") || !strings.Contains(zeroText, "No usage recorded in the last 12 weeks.") {
			t.Fatalf("zero-usage detail missing empty copy: %q", zeroText)
		}
		// Nonexistent model returns 404.
		notFoundResp, err := chromedp.RunResponse(ctx, chromedp.Navigate(baseURL+"/tokedex/nonexistent-model"))
		if err != nil {
			t.Fatalf("navigate nonexistent model: %v", err)
		}
		if notFoundResp.Status != http.StatusNotFound {
			t.Fatalf("/tokedex/nonexistent-model status = %d, want 404", notFoundResp.Status)
		}
	})

	t.Run("quotas-settings-and-pace-bar", func(t *testing.T) {
		if _, err := chromedp.Run(ctx, chromedp.Navigate(baseURL+"/settings")); err != nil {
			t.Fatalf("navigate /settings: %v", err)
		}
		if _, err := chromedp.Run(ctx, chromedp.WaitReady("#quota-new-label")); err != nil {
			t.Fatalf("wait #quota-new-label: %v", err)
		}
		if _, err := chromedp.Run(ctx, chromedp.SetValue("#quota-new-label", "Smoke Monthly Cap")); err != nil {
			t.Fatalf("set #quota-new-label: %v", err)
		}
		if _, err := chromedp.Run(ctx, chromedp.SetValue("#quota-new-tokens", "5000")); err != nil {
			t.Fatalf("set #quota-new-tokens: %v", err)
		}
		if _, err := chromedp.RunResponse(ctx, chromedp.Click(`form.quota-add-form button[type="submit"]`)); err != nil {
			t.Fatalf("submit add quota: %v", err)
		}
		if _, err := chromedp.Run(ctx, chromedp.WaitReady(`[data-quota-row]`)); err != nil {
			t.Fatalf("wait [data-quota-row]: %v", err)
		}
		// / now renders the compact budget pace bar and passes accessibility + layout invariants.
		assertPageAtViewports(t, ctx, baseURL, "/", []string{
			`(() => { const p = document.querySelector('.quota-pace-panel'); return JSON.stringify(p && p.textContent.includes('Smoke Monthly Cap') ? [] : ['missing Smoke Monthly Cap pace bar on /']); })()`,
		})
		// Delete the quota in /settings and confirm / removes the pace bar cleanly.
		if _, err := chromedp.Run(ctx, chromedp.Navigate(baseURL+"/settings")); err != nil {
			t.Fatalf("navigate /settings for delete: %v", err)
		}
		if _, err := chromedp.Run(ctx, chromedp.WaitReady(`form.quota-delete-form button[type="submit"]`)); err != nil {
			t.Fatalf("wait delete button: %v", err)
		}
		if _, err := chromedp.RunResponse(ctx, chromedp.Click(`form.quota-delete-form button[type="submit"]`)); err != nil {
			t.Fatalf("submit delete quota: %v", err)
		}
		if _, err := chromedp.Run(ctx, chromedp.WaitReady(`#quota-new-label`)); err != nil {
			t.Fatalf("wait #quota-new-label after delete: %v", err)
		}
		if _, err := chromedp.Run(ctx, chromedp.Navigate(baseURL+"/")); err != nil {
			t.Fatalf("navigate / after delete: %v", err)
		}
		if _, err := chromedp.Run(ctx, chromedp.WaitReady(`#lifetime-counter`)); err != nil {
			t.Fatalf("wait #lifetime-counter after delete: %v", err)
		}
		absentProblems, err := chromedp.Run(ctx, chromedp.Evaluate[string](quotaAbsentInvariantJS))
		if err != nil || absentProblems != "[]" {
			t.Fatalf("quota pace bar did not clear after delete: problems=%s err=%v", absentProblems, err)
		}
	})

	// The pause toggle keeps its state across the reloads it controls, and
	// unpausing restores the default auto-refresh.
	t.Run("overview-refresh-pause", func(t *testing.T) {
		stateJS := `(() => { const toggle = document.getElementById('auto-refresh-toggle'); return JSON.stringify({ pressed: toggle.getAttribute('aria-pressed'), label: (toggle.getAttribute('aria-label') || toggle.textContent).trim() }); })()`
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

	// The login/session/logout flow in a real browser against a
	// password-enabled server: the form renders, wrong credentials fail with
	// a visible alert, correct credentials mint a session cookie and land on
	// the next target, logout demands its CSRF token, real logout clears the
	// cookie, and a signed-out browser is sent back to the form. The v0.4.3
	// anonymous-HTML redirect regression reached the gate because none of
	// this was exercised.
	t.Run("login-session-logout", func(t *testing.T) {
		passwordURL := startSeededPasswordServer(t)
		loginCtx, cancelLogin := chromedp.NewContext(allocatorCtx)
		defer cancelLogin()
		loginRunCtx, cancelLoginRun := context.WithTimeout(loginCtx, 90*time.Second)
		defer cancelLoginRun()

		// RunResponse only reports the final document of a redirect chain, so
		// the 303 assertions read records taken off the CDP network events:
		// one entry per rendered document plus one per intermediate redirect.
		// Listeners run synchronously in arrival order, so every record a
		// step produced is in the slice by the time its RunResponse returns.
		var wireMu sync.Mutex
		var wire []wireResponse
		wireLen := func() int {
			wireMu.Lock()
			defer wireMu.Unlock()
			return len(wire)
		}
		wireSince := func(mark int) []wireResponse {
			wireMu.Lock()
			defer wireMu.Unlock()
			return append([]wireResponse(nil), wire[mark:]...)
		}
		wireFind := func(mark int, match func(wireResponse) bool) bool {
			t.Helper()
			for _, response := range wireSince(mark) {
				if match(response) {
					return true
				}
			}
			t.Fatalf("no matching wire response after mark %d; recorded: %+v", mark, wireSince(mark))
			return false
		}
		if err := chromedp.Listen(loginCtx, func(ev any) {
			switch ev := ev.(type) {
			case *network.EventResponseReceived:
				if ev.Type == network.ResourceTypeDocument {
					wireMu.Lock()
					wire = append(wire, wireResponse{status: ev.Response.Status, url: ev.Response.URL})
					wireMu.Unlock()
				}
			case *network.EventRequestWillBeSent:
				if ev.RedirectResponse == nil || !strings.HasPrefix(ev.Request.URL, passwordURL) {
					return
				}
				wireMu.Lock()
				wire = append(wire, wireResponse{
					status:   ev.RedirectResponse.Status,
					url:      ev.RedirectResponse.URL,
					location: wireHeader(ev.RedirectResponse.Headers, "Location"),
				})
				wireMu.Unlock()
			}
		}); err != nil {
			t.Fatalf("could not subscribe to network events: %v", err)
		}

		// 1. GET /login renders the form: 200, a username field, a password
		// field, and the hidden CSRF field.
		formResponse, err := chromedp.RunResponse(loginRunCtx, chromedp.Navigate(passwordURL+"/login"))
		if err != nil {
			t.Fatalf("navigation to /login failed: %v", err)
		}
		if formResponse.Status != http.StatusOK {
			t.Fatalf("GET /login status = %d, want %d", formResponse.Status, http.StatusOK)
		}
		if _, err := chromedp.Run(loginRunCtx, chromedp.WaitReady("#login-password")); err != nil {
			t.Fatalf("login form never became ready: %v", err)
		}
		fieldsJSON, err := chromedp.Run(loginRunCtx, chromedp.Evaluate[string](`(() => {
			const form = document.querySelector('form[action="/login"]');
			return JSON.stringify({
				username: !!document.querySelector('#login-username[name="username"]'),
				password: !!document.querySelector('#login-password[name="password"]'),
				csrf: !!form && !!form.querySelector('input[type="hidden"][name="csrf_token"]'),
			});
		})()`))
		if err != nil {
			t.Fatalf("login form field check failed to run: %v", err)
		}
		var fields struct {
			Username bool `json:"username"`
			Password bool `json:"password"`
			CSRF     bool `json:"csrf"`
		}
		if err := json.Unmarshal([]byte(fieldsJSON), &fields); err != nil {
			t.Fatalf("login form field check returned malformed JSON %q: %v", fieldsJSON, err)
		}
		if !fields.Username || !fields.Password || !fields.CSRF {
			t.Fatalf("login form incomplete: username=%v password=%v hidden-csrf=%v", fields.Username, fields.Password, fields.CSRF)
		}

		// 2. Wrong password: 401 and a visible role=alert error.
		if _, err := chromedp.Run(loginRunCtx, chromedp.SetValue("#login-username", smokeLoginUsername)); err != nil {
			t.Fatalf("could not fill the username field: %v", err)
		}
		if _, err := chromedp.Run(loginRunCtx, chromedp.SetValue("#login-password", "deliberately-wrong")); err != nil {
			t.Fatalf("could not fill the password field: %v", err)
		}
		wrongResponse, err := chromedp.RunResponse(loginRunCtx, chromedp.Click(`form[action="/login"] button[type="submit"]`))
		if err != nil {
			t.Fatalf("wrong-password submit failed: %v", err)
		}
		if wrongResponse.Status != http.StatusUnauthorized {
			t.Fatalf("wrong-password submit status = %d, want %d", wrongResponse.Status, http.StatusUnauthorized)
		}
		alertJSON, err := chromedp.Run(loginRunCtx, chromedp.Evaluate[string](`(() => {
			const alert = document.querySelector('[role="alert"]');
			if (!alert) return JSON.stringify({present: false});
			const box = alert.getBoundingClientRect();
			return JSON.stringify({present: true, visible: box.width > 0 && box.height > 0, text: alert.textContent.trim()});
		})()`))
		if err != nil {
			t.Fatalf("login alert check failed to run: %v", err)
		}
		var alert struct {
			Present bool   `json:"present"`
			Visible bool   `json:"visible"`
			Text    string `json:"text"`
		}
		if err := json.Unmarshal([]byte(alertJSON), &alert); err != nil {
			t.Fatalf("login alert check returned malformed JSON %q: %v", alertJSON, err)
		}
		if !alert.Present || !alert.Visible || !strings.Contains(alert.Text, "Invalid username or password.") {
			t.Fatalf("wrong-password error alert = (present=%v, visible=%v, text=%q), want a visible alert telling the credentials were rejected", alert.Present, alert.Visible, alert.Text)
		}

		// 3. Correct password: a 303 to the next target with a session
		// cookie, and the authenticated page renders 200.
		signInMark := wireLen()
		if _, err := chromedp.Run(loginRunCtx, chromedp.Navigate(passwordURL+"/login?next=%2Fsessions")); err != nil {
			t.Fatalf("navigation to the next-carrying login form failed: %v", err)
		}
		if _, err := chromedp.Run(loginRunCtx, chromedp.WaitReady("#login-password")); err != nil {
			t.Fatalf("next-carrying login form never became ready: %v", err)
		}
		if _, err := chromedp.Run(loginRunCtx, chromedp.SetValue("#login-username", smokeLoginUsername)); err != nil {
			t.Fatalf("could not fill the username field: %v", err)
		}
		if _, err := chromedp.Run(loginRunCtx, chromedp.SetValue("#login-password", smokeLoginPassword)); err != nil {
			t.Fatalf("could not fill the password field: %v", err)
		}
		signedInResponse, err := chromedp.RunResponse(loginRunCtx, chromedp.Click(`form[action="/login"] button[type="submit"]`))
		if err != nil {
			t.Fatalf("correct-password submit failed: %v", err)
		}
		wireFind(signInMark, func(response wireResponse) bool {
			return response.status == http.StatusSeeOther &&
				response.url == passwordURL+"/login" &&
				response.location == "/sessions"
		})
		if signedInResponse.Status != http.StatusOK || !strings.HasPrefix(signedInResponse.URL, passwordURL+"/sessions") {
			t.Fatalf("after sign-in the browser landed on %s with status %d, want /sessions with 200", signedInResponse.URL, signedInResponse.Status)
		}
		if held := sessionCookieCount(t, loginRunCtx); held != 1 {
			t.Fatalf("after sign-in the browser holds %d session cookies, want exactly 1", held)
		}

		// 4. Logout without the CSRF token: 403. The request goes through the
		// real topbar form with its CSRF field stripped, so the origin check
		// still passes and the rejection is the token gate.
		if _, err := chromedp.Run(loginRunCtx, chromedp.Evaluate[bool](`(() => {
			const field = document.querySelector('form.logout-form input[name="csrf_token"]');
			if (!field) return false;
			field.remove();
			return true;
		})()`)); err != nil {
			t.Fatalf("could not strip the logout CSRF field: %v", err)
		}
		rejectedResponse, err := chromedp.RunResponse(loginRunCtx, chromedp.Click("form.logout-form button"))
		if err != nil {
			t.Fatalf("csrf-less logout submit failed: %v", err)
		}
		if rejectedResponse.Status != http.StatusForbidden {
			t.Fatalf("logout without the CSRF token status = %d, want %d", rejectedResponse.Status, http.StatusForbidden)
		}

		// 5. Logout with the CSRF token: 303 to /login and the session cookie
		// cleared.
		logoutMark := wireLen()
		if _, err := chromedp.Run(loginRunCtx, chromedp.Navigate(passwordURL+"/")); err != nil {
			t.Fatalf("navigation to the signed-in overview failed: %v", err)
		}
		if _, err := chromedp.Run(loginRunCtx, chromedp.WaitReady("form.logout-form")); err != nil {
			t.Fatalf("signed-in overview never became ready: %v", err)
		}
		signedOutResponse, err := chromedp.RunResponse(loginRunCtx, chromedp.Click("form.logout-form button"))
		if err != nil {
			t.Fatalf("logout submit failed: %v", err)
		}
		wireFind(logoutMark, func(response wireResponse) bool {
			return response.status == http.StatusSeeOther &&
				response.url == passwordURL+"/logout" &&
				response.location == "/login"
		})
		if signedOutResponse.Status != http.StatusOK || !strings.HasPrefix(signedOutResponse.URL, passwordURL+"/login") {
			t.Fatalf("after logout the browser landed on %s with status %d, want /login with 200", signedOutResponse.URL, signedOutResponse.Status)
		}
		if remaining := sessionCookieCount(t, loginRunCtx); remaining != 0 {
			t.Fatalf("after logout the browser still holds %d session cookies, want 0", remaining)
		}

		// 6. After logout, an authenticated page redirects browsers to the
		// login form with the target preserved in next.
		signedOutNavMark := wireLen()
		signedOutNavResponse, err := chromedp.Run(loginRunCtx, chromedp.NavigateResponse(passwordURL+"/sessions"))
		if err != nil {
			t.Fatalf("navigation to the signed-out /sessions failed: %v", err)
		}
		wireFind(signedOutNavMark, func(response wireResponse) bool {
			return response.status == http.StatusSeeOther &&
				response.url == passwordURL+"/sessions" &&
				response.location == "/login?next=%2Fsessions"
		})
		if signedOutNavResponse.Status != http.StatusOK || !strings.HasPrefix(signedOutNavResponse.URL, passwordURL+"/login") {
			t.Fatalf("signed-out navigation landed on %s with status %d, want /login with 200", signedOutNavResponse.URL, signedOutNavResponse.Status)
		}
	})

	t.Run("sessions-drilldown-and-filters", func(t *testing.T) {
		if _, err := chromedp.Run(ctx, chromedp.EmulateViewport(1280, 800)); err != nil {
			t.Fatalf("emulate desktop: %v", err)
		}
		if _, err := chromedp.Run(ctx, chromedp.Navigate(baseURL+"/sessions?project=smoke-project&q=smoke-sess-alpha")); err != nil {
			t.Fatalf("navigate filtered /sessions: %v", err)
		}
		if _, err := chromedp.Run(ctx, chromedp.WaitReady("a.session-link")); err != nil {
			t.Fatalf("session link never appeared: %v", err)
		}
		assertNoProblems(t, ctx, exportInvariantJS)
		filteredProblems := `(() => {
			const problems = [];
			const rows = Array.from(document.querySelectorAll('tbody tr'));
			if (rows.length !== 1 || !rows[0].textContent.includes('smoke-sess-alpha')) {
				problems.push('expected 1 row for smoke-sess-alpha, got ' + rows.length);
			}
			const exp = document.querySelector('a.action[href*="export"]');
			if (!exp || !exp.getAttribute('href').includes('project=smoke-project') || !exp.getAttribute('href').includes('q=smoke-sess-alpha')) {
				problems.push('export link lost active filters: ' + (exp && exp.getAttribute('href')));
			}
			return JSON.stringify(problems);
		})()`
		assertNoProblems(t, ctx, filteredProblems)
		resp, err := chromedp.RunResponse(ctx, chromedp.Click("a.session-link"))
		if err != nil {
			t.Fatalf("click session-link: %v", err)
		}
		if resp.Status != http.StatusOK || !strings.Contains(resp.URL, "/sessions/smoke-sess-alpha") {
			t.Fatalf("click session-link landed on %s status=%d", resp.URL, resp.Status)
		}
		if _, err := chromedp.Run(ctx, chromedp.Evaluate[bool](settleAnimationJS)); err != nil {
			t.Fatalf("settle animations after session-link click: %v", err)
		}
		assertNoProblems(t, ctx, sessionDetailInvariantJS)
		assertAxeAllowlist(t, ctx, "/sessions/smoke-sess-alpha")

		notFoundResp, err := chromedp.RunResponse(ctx, chromedp.Navigate(baseURL+"/sessions/nonexistent-id"))
		if err != nil {
			t.Fatalf("navigate missing session: %v", err)
		}
		if notFoundResp.Status != http.StatusNotFound {
			t.Fatalf("GET /sessions/nonexistent-id status = %d, want 404", notFoundResp.Status)
		}
	})

	t.Run("sessions-pagination", func(t *testing.T) {
		pagStore, err := database.Open(filepath.Join(t.TempDir(), "pag.db"), smokeCatalog())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { pagStore.Close() })
		now := time.Now().UTC().Add(-time.Hour)
		evts := make([]usage.Event, 0, 105)
		for i := 0; i < 105; i++ {
			evts = append(evts, usage.Event{
				SchemaVersion: usage.SchemaVersion,
				EventID:       fmt.Sprintf("pag-ev-%03d", i),
				Timestamp:     now.Add(time.Duration(i) * time.Second),
				MachineID:     "smoke-laptop",
				Project:       "pag-proj",
				Provider:      "anthropic",
				Model:         "claude-sonnet-4",
				Tool:          "claude-code",
				SessionID:     fmt.Sprintf("pag-sess-%03d", i),
				InputTokens:   usage.Int64(100 + int64(i)),
				OutputTokens:  usage.Int64(50),
				TotalTokens:   usage.Int64(150 + int64(i)),
				TokenAccuracy: usage.AccuracyReported,
				Source:        usage.Source{Adapter: "claude-code", AdapterVersion: "smoke"},
			})
		}
		if _, err := pagStore.Ingest(context.Background(), evts); err != nil {
			t.Fatal(err)
		}
		pagURL := serveStore(t, pagStore, false)
		if _, err := chromedp.Run(ctx, chromedp.Navigate(pagURL+"/sessions?project=pag-proj&sort=tokens&dir=asc")); err != nil {
			t.Fatalf("navigate paginated /sessions: %v", err)
		}
		if _, err := chromedp.Run(ctx, chromedp.WaitReady("nav.pagination a.page-link")); err != nil {
			t.Fatalf("pagination nav never appeared: %v", err)
		}
		p1Problems := `(() => {
			const problems = [];
			const rows = document.querySelectorAll('tbody tr');
			if (rows.length !== 100) problems.push('page 1 expected 100 rows, got ' + rows.length);
			const next = document.querySelector('nav.pagination a.page-link');
			const href = next ? next.getAttribute('href') : '';
			if (!href.includes('page=2') || !href.includes('project=pag-proj') || !href.includes('sort=tokens') || !href.includes('dir=asc')) {
				problems.push('next link missing preserved params: ' + href);
			}
			return JSON.stringify(problems);
		})()`
		assertNoProblems(t, ctx, p1Problems)
		resp, err := chromedp.RunResponse(ctx, chromedp.Click("nav.pagination a.page-link"))
		if err != nil {
			t.Fatalf("click next page: %v", err)
		}
		if resp.Status != http.StatusOK || !strings.Contains(resp.URL, "page=2") {
			t.Fatalf("page 2 landed on %s status=%d", resp.URL, resp.Status)
		}
		p2Problems := `(() => {
			const problems = [];
			const rows = document.querySelectorAll('tbody tr');
			if (rows.length !== 5) problems.push('page 2 expected 5 rows, got ' + rows.length);
			return JSON.stringify(problems);
		})()`
		assertNoProblems(t, ctx, p2Problems)
		if _, err := chromedp.Run(ctx, chromedp.Evaluate[bool](settleAnimationJS)); err != nil {
			t.Fatalf("settle animations after pagination click: %v", err)
		}
		assertNoProblems(t, ctx, sharedInvariantJS)
		assertAxeAllowlist(t, ctx, "/sessions")
	})

	t.Run("drilldowns-and-click-throughs", func(t *testing.T) {
		if _, err := chromedp.Run(ctx, chromedp.Navigate(baseURL+"/")); err != nil {
			t.Fatalf("navigate /: %v", err)
		}
		if _, err := chromedp.Run(ctx, chromedp.WaitReady("a.row-link")); err != nil {
			t.Fatalf("row-link missing on /: %v", err)
		}
		resp, err := chromedp.RunResponse(ctx, chromedp.Click("a.row-link"))
		if err != nil {
			t.Fatalf("click overview row-link: %v", err)
		}
		if resp.Status != http.StatusOK || (!strings.Contains(resp.URL, "/sessions?") && !strings.Contains(resp.URL, "/analytics?")) {
			t.Fatalf("overview row-link landed on %s status=%d", resp.URL, resp.Status)
		}

		// Heatmap day click-through on /.
		if _, err := chromedp.Run(ctx, chromedp.Navigate(baseURL+"/")); err != nil {
			t.Fatalf("navigate / for heatmap: %v", err)
		}
		if _, err := chromedp.Run(ctx, chromedp.WaitReady(`.activity-cell[data-date]`)); err != nil {
			t.Fatalf("heatmap cell missing data-date: %v", err)
		}
		hmResp, err := chromedp.RunResponse(ctx, chromedp.Click(`.activity-cell[data-date]`))
		if err != nil {
			t.Fatalf("click heatmap cell: %v", err)
		}
		if hmResp.Status != http.StatusOK || !strings.Contains(hmResp.URL, "/sessions?from=") {
			t.Fatalf("heatmap click landed on %s status=%d", hmResp.URL, hmResp.Status)
		}

		// Trend bar click-through on /analytics.
		if _, err := chromedp.Run(ctx, chromedp.Navigate(baseURL+"/analytics?period=7d&dimension=projects")); err != nil {
			t.Fatalf("navigate /analytics: %v", err)
		}
		if _, err := chromedp.Run(ctx, chromedp.WaitReady(`.trend-column[data-from]`)); err != nil {
			t.Fatalf("trend-column missing data-from: %v", err)
		}
		trendResp, err := chromedp.RunResponse(ctx, chromedp.Click(`.trend-column[data-from]`))
		if err != nil {
			t.Fatalf("click trend-column: %v", err)
		}
		if trendResp.Status != http.StatusOK || !strings.Contains(trendResp.URL, "/sessions?from=") {
			t.Fatalf("trend click landed on %s status=%d", trendResp.URL, trendResp.Status)
		}
	})

	// Decluttered /analytics toolbar: collapsed Filters (N), auto-submit,
	// removable active pills, and Export menu across desktop and phone widths.
	t.Run("analytics-filter-toolbar", func(t *testing.T) {
		if _, err := chromedp.Run(ctx, chromedp.EmulateViewport(1280, 800)); err != nil {
			t.Fatalf("emulate desktop: %v", err)
		}
		if _, err := chromedp.Run(ctx, chromedp.Navigate(baseURL+"/analytics?period=7d&dimension=models")); err != nil {
			t.Fatalf("navigate /analytics: %v", err)
		}
		if _, err := chromedp.Run(ctx, chromedp.WaitReady("details.filter-details")); err != nil {
			t.Fatalf("filter-details missing: %v", err)
		}
		if _, err := chromedp.Run(ctx, chromedp.Evaluate[bool](settleAnimationJS)); err != nil {
			t.Fatalf("settle animations: %v", err)
		}
		if _, err := chromedp.Run(ctx, chromedp.Click("summary.filter-summary")); err != nil {
			t.Fatalf("click filter-summary: %v", err)
		}
		assertNoProblems(t, ctx, sharedInvariantJS)
		assertAxeAllowlist(t, ctx, "/analytics")

		// Auto-submit on machine select change preserves period=7d and dimension=models.
		if _, err := chromedp.Run(ctx, chromedp.Evaluate[bool](`(() => {
			const sel = document.getElementById('machine-filter');
			sel.value = 'smoke-laptop';
			sel.dispatchEvent(new Event('change', { bubbles: true }));
			return true;
		})()`)); err != nil {
			t.Fatalf("trigger machine-filter change: %v", err)
		}
		deadline := time.Now().Add(5 * time.Second)
		for {
			curURL, err := chromedp.Run(ctx, chromedp.Evaluate[string](`window.location.href`))
			if err == nil && strings.Contains(curURL, "machine=smoke-laptop") {
				if !strings.Contains(curURL, "period=7d") || !strings.Contains(curURL, "dimension=models") {
					t.Fatalf("auto-submit URL lost period or dimension: %s", curURL)
				}
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("auto-submit did not navigate to filtered URL (last=%q err=%v)", curURL, err)
			}
			time.Sleep(50 * time.Millisecond)
		}
		if _, err := chromedp.Run(ctx, chromedp.WaitReady(".filter-pill")); err != nil {
			t.Fatalf("filter-pill missing after auto-submit: %v", err)
		}
		if _, err := chromedp.Run(ctx, chromedp.Evaluate[bool](settleAnimationJS)); err != nil {
			t.Fatalf("settle animations after filter auto-submit: %v", err)
		}
		assertNoProblems(t, ctx, `(() => {
			const problems = [];
			const count = document.querySelector('.filter-count');
			if (!count || !count.textContent.includes('(1)')) problems.push('expected Filters (1)');
			const pill = document.querySelector('.filter-pill');
			if (!pill || !pill.textContent.includes('Machine:')) problems.push('missing Machine filter pill');
			const exp = document.querySelector('.export-menu a.action[href*="export"]');
			if (!exp || !exp.getAttribute('href').includes('machine=smoke-laptop')) problems.push('export href missing active filter');
			return JSON.stringify(problems);
		})()`)

		// Export menu open at desktop & phone widths must not overflow or fail axe.
		for _, vp := range []struct {
			name string
			w, h int64
		}{
			{"desktop", 1280, 800},
			{"phone", 390, 844},
		} {
			if _, err := chromedp.Run(ctx, chromedp.EmulateViewport(vp.w, vp.h)); err != nil {
				t.Fatalf("emulate %s: %v", vp.name, err)
			}
			if _, err := chromedp.Run(ctx, chromedp.Click("summary.export-summary")); err != nil {
				t.Fatalf("click export-summary at %s: %v", vp.name, err)
			}
			assertNoProblems(t, ctx, sharedInvariantJS)
			assertAxeAllowlist(t, ctx, "/analytics")
			if _, err := chromedp.Run(ctx, chromedp.Click("summary.export-summary")); err != nil {
				t.Fatalf("close export-summary at %s: %v", vp.name, err)
			}
		}
		if _, err := chromedp.Run(ctx, chromedp.EmulateViewport(1280, 800)); err != nil {
			t.Fatalf("restore desktop viewport: %v", err)
		}

		// One-click pill removal clears machine while keeping period=7d and dimension=models.
		pillResp, err := chromedp.RunResponse(ctx, chromedp.Click(".filter-pill"))
		if err != nil {
			t.Fatalf("click filter-pill: %v", err)
		}
		if pillResp.Status != http.StatusOK || strings.Contains(pillResp.URL, "machine=") || !strings.Contains(pillResp.URL, "period=7d") || !strings.Contains(pillResp.URL, "dimension=models") {
			t.Fatalf("pill removal landed on unexpected URL %s status=%d", pillResp.URL, pillResp.Status)
		}
	})

	// In-place / live updates: paused auto-refresh stays quiet, #refresh-now
	// patches without reload, and active polling updates stage + counter +
	// sessions while keeping open <details> and .activity-cell focus intact.
	t.Run("overview-live-in-place-update", func(t *testing.T) {
		liveStore, err := database.Open(filepath.Join(t.TempDir(), "live.db"), smokeCatalog())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { liveStore.Close() })
		now := time.Now().UTC()
		freshSync := now.Add(-time.Minute)
		staleSync := now.Add(-2 * time.Hour)
		if _, err := liveStore.Ingest(context.Background(), []usage.Event{{
			SchemaVersion: usage.SchemaVersion,
			EventID:       "live-seed-1",
			Timestamp:     now.Add(-2 * time.Minute),
			MachineID:     "smoke-live",
			Project:       "live-proj",
			Provider:      "anthropic",
			Model:         "claude-sonnet-4",
			Tool:          "claude-code",
			SessionID:     "live-sess-1",
			InputTokens:   usage.Int64(70),
			OutputTokens:  usage.Int64(30),
			TotalTokens:   usage.Int64(100),
			Currency:      "USD",
			TokenAccuracy: usage.AccuracyReported,
			Source:        usage.Source{Adapter: "claude-code", AdapterVersion: "smoke"},
		}}); err != nil {
			t.Fatal(err)
		}
		if err := liveStore.RecordHeartbeat(context.Background(), database.AgentHeartbeat{
			MachineID:        "smoke-live",
			OperatingSystem:  "darwin",
			Architecture:     "arm64",
			SourceCount:      2,
			SourceErrorCount: 1,
			Sources: []database.AgentSourceHealth{
				{Adapter: "codex", SourceHash: "live-src-1", LastSuccessfulSync: &freshSync},
				{Adapter: "codex", SourceHash: "live-src-2", LastSuccessfulSync: &staleSync, LastErrorClass: "timeout"},
			},
		}); err != nil {
			t.Fatal(err)
		}
		liveURL := serveStore(t, liveStore, false)
		parsedURL, err := url.Parse(liveURL)
		if err != nil {
			t.Fatal(err)
		}
		ingestBatch := func(eventID, sessionID string, input, output, total int64) {
			t.Helper()
			payload, err := json.Marshal(map[string]any{
				"events": []usage.Event{{
					SchemaVersion: usage.SchemaVersion,
					EventID:       eventID,
					Timestamp:     time.Now().UTC(),
					MachineID:     "smoke-live",
					Project:       "live-proj",
					Provider:      "anthropic",
					Model:         "claude-sonnet-4",
					Tool:          "claude-code",
					SessionID:     sessionID,
					InputTokens:   usage.Int64(input),
					OutputTokens:  usage.Int64(output),
					TotalTokens:   usage.Int64(total),
					Currency:      "USD",
					TokenAccuracy: usage.AccuracyReported,
					Source:        usage.Source{Adapter: "claude-code", AdapterVersion: "smoke"},
				}},
			})
			if err != nil {
				t.Fatal(err)
			}
			req, err := http.NewRequest(http.MethodPost, "http://"+parsedURL.Host+"/api/v1/events/batch", strings.NewReader(string(payload)))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("Authorization", "Bearer smoke-ingest")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("ingest batch status = %d", resp.StatusCode)
			}
		}

		if _, err := chromedp.Run(ctx, chromedp.EmulateViewport(1280, 800)); err != nil {
			t.Fatalf("emulate desktop: %v", err)
		}
		if _, err := chromedp.Run(ctx, chromedp.Navigate(liveURL+"/")); err != nil {
			t.Fatalf("navigate live /: %v", err)
		}
		if _, err := chromedp.Run(ctx, chromedp.WaitReady("details.source-details")); err != nil {
			t.Fatalf("details.source-details missing: %v", err)
		}
		if _, err := chromedp.Run(ctx, chromedp.Evaluate[bool](`(() => { window.__tokemonNoReloadSentinel = 'alive'; return true; })()`)); err != nil {
			t.Fatalf("set sentinel: %v", err)
		}
		// Expand details.source-details.
		if _, err := chromedp.Run(ctx, chromedp.ScrollIntoView("summary.source-details-summary")); err != nil {
			t.Fatalf("scroll source-details: %v", err)
		}
		if _, err := chromedp.Run(ctx, chromedp.Evaluate[bool](`new Promise(resolve => requestAnimationFrame(() => requestAnimationFrame(() => resolve(true))))`, chromedp.EvalAwaitPromise)); err != nil {
			t.Fatalf("await frame: %v", err)
		}
		if _, err := chromedp.Run(ctx, chromedp.Click("summary.source-details-summary")); err != nil {
			t.Fatalf("expand source-details: %v", err)
		}

		// Paused state: ingest +500 tokens, wait >2.2s, confirm counter stays 100.
		if _, err := chromedp.Run(ctx, chromedp.Click("#auto-refresh-toggle")); err != nil {
			t.Fatalf("pause auto-refresh: %v", err)
		}
		ingestBatch("live-paused-1", "live-sess-2", 350, 150, 500)
		time.Sleep(2400 * time.Millisecond)
		if pausedVal, err := chromedp.Run(ctx, chromedp.Evaluate[string](`document.getElementById('lifetime-counter').dataset.display`)); err != nil || pausedVal != "100" {
			t.Fatalf("paused counter = %q (err=%v), want 100", pausedVal, err)
		}

		// Manual #refresh-now while paused updates counter to 600 + new session without reload.
		if _, err := chromedp.Run(ctx, chromedp.Click("#refresh-now")); err != nil {
			t.Fatalf("click #refresh-now: %v", err)
		}
		deadline := time.Now().Add(10 * time.Second)
		for {
			done, err := chromedp.Run(ctx, chromedp.Evaluate[bool](`(() => {
				const c = document.getElementById('lifetime-counter');
				const s = document.querySelector('.sessions-panel');
				return Boolean(c && c.dataset.display === '600' && s && s.textContent.includes('live-sess-2'));
			})()`))
			if err == nil && done {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("#refresh-now did not update counter to 600 and show live-sess-2 in time")
			}
			time.Sleep(100 * time.Millisecond)
		}
		assertNoProblems(t, ctx, `(() => {
			const problems = [];
			if (window.__tokemonNoReloadSentinel !== 'alive') problems.push('page reloaded on #refresh-now');
			const details = document.querySelector('details.source-details');
			if (!details || !details.open) problems.push('details.source-details did not stay open after #refresh-now');
			return JSON.stringify(problems);
		})()`)

		// Resume auto-refresh, focus active .activity-cell, ingest +400 -> 1,000 (Stage 3 Promptling).
		if _, err := chromedp.Run(ctx, chromedp.Click("#auto-refresh-toggle")); err != nil {
			t.Fatalf("resume auto-refresh: %v", err)
		}
		focusedDate, err := chromedp.Run(ctx, chromedp.Evaluate[string](`(() => {
			const cell = document.querySelector('.activity-cell[tabindex="0"]');
			if (!cell) return '';
			cell.focus();
			return cell.dataset.date || '';
		})()`))
		if err != nil || focusedDate == "" {
			t.Fatalf("failed to focus .activity-cell[tabindex=0]: date=%q err=%v", focusedDate, err)
		}
		ingestBatch("live-auto-1", "live-sess-3", 280, 120, 400)
		deadline = time.Now().Add(15 * time.Second)
		for {
			evolved, err := chromedp.Run(ctx, chromedp.Evaluate[bool](`(() => {
				const c = document.getElementById('lifetime-counter');
				const p = document.querySelector('.creature-panel');
				const f = document.querySelector('.form-name');
				const s = document.querySelector('.sessions-panel');
				return Boolean(c && c.dataset.display === '1,000' && p && p.dataset.stage === '3' && f && f.textContent === 'Promptling' && s && s.textContent.includes('live-sess-3'));
			})()`))
			if err == nil && evolved {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("auto-update did not evolve creature to Stage 3 Promptling at 1,000 tokens in time")
			}
			time.Sleep(150 * time.Millisecond)
		}
		assertNoProblems(t, ctx, fmt.Sprintf(`(() => {
			const problems = [];
			if (window.__tokemonNoReloadSentinel !== 'alive') problems.push('page reloaded during stage evolution');
			const img = document.querySelector('img.creature-art');
			if (!img || !img.getAttribute('src').includes('stage-03.png')) problems.push('creature-art src did not update to stage-03.png');
			const details = document.querySelector('details.source-details');
			if (!details || !details.open) problems.push('details.source-details lost open state on live update');
			const active = document.activeElement;
			if (!active || !active.classList.contains('activity-cell') || active.dataset.date !== %q) {
				problems.push('activity-cell focus lost on live update');
			}
			return JSON.stringify(problems);
		})()`, focusedDate))
		if _, err := chromedp.Run(ctx, chromedp.Evaluate[bool](`(() => { if (document.activeElement && document.activeElement !== document.body) document.activeElement.blur(); return true; })()`)); err != nil {
			t.Fatalf("blur active element: %v", err)
		}
		assertNoProblems(t, ctx, sharedInvariantJS)
		assertNoProblems(t, ctx, creatureArtInvariantJS)
		assertAxeAllowlist(t, ctx, "/")

		// Regression: pausing auto-refresh cancels a pending 400ms force:true
		// retry and prevents an in-flight force:true refresh from scheduling one.
		if _, err := chromedp.Run(ctx, chromedp.Navigate(liveURL+"/")); err != nil {
			t.Fatalf("navigate live / for pause-retry check: %v", err)
		}
		if _, err := chromedp.Run(ctx, chromedp.WaitReady("#refresh-now")); err != nil {
			t.Fatalf("#refresh-now never became ready: %v", err)
		}
		if _, err := chromedp.Run(ctx, chromedp.Evaluate[bool](`(() => {
			window.__pauseRetryCalls = 0;
			window.__pauseRetryDeferred = null;
			const origFetch = window.fetch.bind(window);
			window.fetch = (input, init) => {
				const url = typeof input === 'string' ? input : (input && input.url) || '';
				let pathname = url;
				try { pathname = new URL(url, window.location.origin).pathname; } catch (_) {}
				if (pathname === '/') {
					window.__pauseRetryCalls += 1;
					if (window.__pauseRetryHold) {
						return new Promise((resolve) => {
							window.__pauseRetryDeferred = () => resolve(new Response(null, { status: 304, headers: { 'X-Tokemon-Refreshing': '1' } }));
						});
					}
					return Promise.resolve(new Response(null, { status: 304, headers: { 'X-Tokemon-Refreshing': '1' } }));
				}
				return origFetch(input, init);
			};
			return true;
		})()`)); err != nil {
			t.Fatalf("install fetch interceptor: %v", err)
		}
		if _, err := chromedp.Run(ctx, chromedp.Click("#refresh-now")); err != nil {
			t.Fatalf("click #refresh-now for pending retry test: %v", err)
		}
		// Wait for the initial fetch to resolve and schedule its 400ms timer, then pause immediately.
		retryDeadline := time.Now().Add(2 * time.Second)
		for {
			calls, err := chromedp.Run(ctx, chromedp.Evaluate[int](`window.__pauseRetryCalls`))
			if err == nil && calls >= 1 {
				break
			}
			if time.Now().After(retryDeadline) {
				t.Fatalf("intercepted fetch never fired (calls=%d err=%v)", calls, err)
			}
			time.Sleep(10 * time.Millisecond)
		}
		if _, err := chromedp.Run(ctx, chromedp.Click("#auto-refresh-toggle")); err != nil {
			t.Fatalf("click #auto-refresh-toggle to pause pending retry: %v", err)
		}
		time.Sleep(550 * time.Millisecond)
		if calls, err := chromedp.Run(ctx, chromedp.Evaluate[int](`window.__pauseRetryCalls`)); err != nil || calls != 1 {
			t.Fatalf("pause did not cancel pending 400ms force:true retry: fetch calls = %d (err=%v), want 1", calls, err)
		}
		// Unpause, then test an in-flight force:true refresh that resolves after Pause is clicked.
		if _, err := chromedp.Run(ctx, chromedp.Click("#auto-refresh-toggle")); err != nil {
			t.Fatalf("unpause before in-flight check: %v", err)
		}
		if _, err := chromedp.Run(ctx, chromedp.Evaluate[bool](`(() => { window.__pauseRetryHold = true; return true; })()`)); err != nil {
			t.Fatalf("arm deferred fetch: %v", err)
		}
		if _, err := chromedp.Run(ctx, chromedp.Click("#refresh-now")); err != nil {
			t.Fatalf("click #refresh-now for in-flight test: %v", err)
		}
		if _, err := chromedp.Run(ctx, chromedp.Click("#auto-refresh-toggle")); err != nil {
			t.Fatalf("click #auto-refresh-toggle while force fetch in flight: %v", err)
		}
		if _, err := chromedp.Run(ctx, chromedp.Evaluate[bool](`(() => {
			window.__pauseRetryHold = false;
			if (window.__pauseRetryDeferred) window.__pauseRetryDeferred();
			return true;
		})()`)); err != nil {
			t.Fatalf("resolve deferred fetch: %v", err)
		}
		time.Sleep(550 * time.Millisecond)
		if calls, err := chromedp.Run(ctx, chromedp.Evaluate[int](`window.__pauseRetryCalls`)); err != nil || calls != 2 {
			t.Fatalf("in-flight force:true refresh scheduled 400ms retry after Pause: fetch calls = %d (err=%v), want 2", calls, err)
		}
		// Restore unpaused state so sessionStorage stays clean.
		if _, err := chromedp.Run(ctx, chromedp.Click("#auto-refresh-toggle")); err != nil {
			t.Fatalf("restore unpaused state: %v", err)
		}
	})

	t.Run("tokedex-whatif-parenthetical-no-usage", func(t *testing.T) {
		cat := catalog.MustNew(map[string]catalog.Model{
			"custom-bedrock-x": {
				Provider:    "anthropic",
				DisplayName: "Model X (Bedrock)",
				Aliases:     []string{"custom-bedrock-x"},
				Tier:        "A",
				Pricing: catalog.Pricing{
					Currency:                 "USD",
					Mode:                     "standard",
					Source:                   "https://example.com/pricing",
					VerifiedAt:               "2026-11-20",
					InputPricePerMillion:     usage.Float64(3.0),
					OutputPricePerMillion:    usage.Float64(15.0),
					CacheReadPricePerMillion: usage.Float64(0.3),
				},
			},
		})
		noUsageStore, err := database.Open(filepath.Join(t.TempDir(), "no-usage-tokedex.db"), cat)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { noUsageStore.Close() })

		noUsageURL := serveStore(t, noUsageStore, true)
		if _, err := chromedp.Run(ctx, chromedp.Navigate(noUsageURL+"/tokedex")); err != nil {
			t.Fatalf("navigate /tokedex: %v", err)
		}
		if _, err := chromedp.Run(ctx, chromedp.WaitReady("#whatif-baseline")); err != nil {
			t.Fatalf("#whatif-baseline missing: %v", err)
		}
		assertNoProblems(t, ctx, `(() => {
			const problems = [];
			const opt = document.querySelector('#whatif-baseline option[value="custom-bedrock-x"]');
			if (!opt) {
				problems.push('missing custom-bedrock-x option in #whatif-baseline');
			} else {
				if (opt.dataset.baseLabel !== 'Model X (Bedrock)') {
					problems.push('data-base-label = ' + JSON.stringify(opt.dataset.baseLabel));
				}
				if (opt.textContent.trim() !== 'Model X (Bedrock)') {
					problems.push('option textContent stripped suffix: ' + JSON.stringify(opt.textContent));
				}
			}
			return JSON.stringify(problems);
		})()`)
	})
}

// assertPageAtViewports renders one page at every gated viewport, runs the
// shared invariants plus the page's own at each, and scans axe at each. The
// narrow widths are the point: a region that only fits at 1280x800 still has
// to be keyboard reachable where it scrolls.
func assertPageAtViewports(t *testing.T, ctx context.Context, baseURL, path string, invariants []string) {
	t.Helper()
	for _, viewport := range gateViewports {
		t.Run(viewport.name, func(t *testing.T) {
			if _, err := chromedp.Run(ctx, chromedp.EmulateViewport(viewport.width, viewport.height)); err != nil {
				t.Fatalf("could not emulate %s: %v", viewport.name, err)
			}
			if _, err := chromedp.Run(ctx, chromedp.Navigate(baseURL+path)); err != nil {
				t.Fatalf("navigation to %s at %s failed: %v", path, viewport.name, err)
			}
			if _, err := chromedp.Run(ctx, chromedp.WaitReady("body")); err != nil {
				t.Fatalf("%s never became ready at %s: %v", path, viewport.name, err)
			}
			if _, err := chromedp.Run(ctx, chromedp.Evaluate[bool](settleAnimationJS)); err != nil {
				t.Fatalf("could not settle animations on %s at %s: %v", path, viewport.name, err)
			}
			for _, invariant := range append([]string{sharedInvariantJS}, invariants...) {
				assertNoProblems(t, ctx, invariant)
			}
			assertAxeAllowlist(t, ctx, path)
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

// wireResponse is one document-level HTTP exchange the login smoke observed
// over CDP: either a rendered document or an intermediate redirect, whose
// Location carries the hand-off target. RunResponse only ever reports the
// final document of a chain, so redirect assertions read these records.
type wireResponse struct {
	status   int64
	url      string
	location string
}

// wireHeader reads one response header case-insensitively from the CDP
// headers map.
func wireHeader(headers network.Headers, name string) string {
	for key, value := range headers {
		if strings.EqualFold(key, name) {
			text, _ := value.(string)
			return text
		}
	}
	return ""
}

// sessionCookieCount returns how many HttpOnly cookies holding a value the
// browser currently has for the page's host. The smoke servers set exactly
// one such cookie — the login session — so the count is 1 while signed in
// and 0 once logout has cleared it.
func sessionCookieCount(t *testing.T, ctx context.Context) int {
	t.Helper()
	cookies, err := chromedp.Call(ctx, network.GetCookies, network.GetCookiesParams{})
	if err != nil {
		t.Fatalf("could not read the browser cookies: %v", err)
	}
	count := 0
	for _, cookie := range cookies.Cookies {
		if cookie.HTTPOnly && cookie.Value != "" {
			count++
		}
	}
	return count
}

// smokeCatalog seeds the enabled Tokedex with the states the MVP must render:
// a priced model with usage, an unpublished-rates model with usage, and a
// priced model with no usage at all.
func smokeCatalog() *catalog.Catalog {
	price := 3.0
	return catalog.MustNew(map[string]catalog.Model{
		"claude-sonnet-4": {
			Provider: "anthropic", DisplayName: "Claude Sonnet 4", Aliases: []string{"claude-sonnet-4"}, Tier: "S",
			Pricing: catalog.Pricing{Currency: "USD", Mode: "standard", VerifiedAt: "2026-11-07", Source: "https://example.com/pricing", InputPricePerMillion: &price, OutputPricePerMillion: &price},
		},
		"gpt-5": {
			Provider: "openai", DisplayName: "GPT-5", Aliases: []string{"gpt-5", "openai/gpt-5"}, Tier: "A",
			Pricing: catalog.Pricing{Currency: "USD", Mode: "unknown", VerifiedAt: "2026-11-07"},
		},
		"smoke-zero-usage": {
			Provider: "openai", DisplayName: "Smoke Zero Usage", Aliases: []string{"smoke-zero-usage"}, Tier: "B",
			Pricing: catalog.Pricing{Currency: "USD", Mode: "standard", VerifiedAt: "2026-11-07", Source: "https://example.com/pricing", InputPricePerMillion: &price, OutputPricePerMillion: &price},
		},
		// A second tier-S model with no usage gives the sort invariant two
		// cards to reorder (tokens puts Claude first, name puts Aardvark first).
		"smoke-sort-model": {
			Provider: "anthropic", DisplayName: "Aardvark Smoke", Aliases: []string{"smoke-sort-model"}, Tier: "S",
			Pricing: catalog.Pricing{Currency: "USD", Mode: "standard", VerifiedAt: "2026-11-07", Source: "https://example.com/pricing", InputPricePerMillion: &price, OutputPricePerMillion: &price},
		},
	})
}

// startSeededServer opens a scratch database, seeds enough usage for every
// page to render with data, and serves the real handler on a loopback port.
// The browser signs in with Basic auth via URL credentials. enableTokedex
// mirrors the TOKEMON_ENABLE_TOKEDEX default-off gate.
func startSeededServer(t *testing.T, enableTokedex bool) string {
	return serveStore(t, seedSmokeStore(t), enableTokedex)
}

// seedSmokeStore opens a scratch database and seeds enough usage for every
// dashboard page to render with data: two usage-reporting machines, four
// connector-health shapes, a no-source machine, and events fresh enough for
// the overview pill to read Fresh.
func seedSmokeStore(t *testing.T) *database.Store {
	t.Helper()
	store, err := database.Open(filepath.Join(t.TempDir(), "smoke.db"), smokeCatalog())
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
			SessionID:     "smoke-sess-alpha",
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
			SessionID:     "smoke-sess-alpha",
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
			SessionID:     "smoke-sess-beta",
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
	// A second machine that reported no discovered sources: the panel must
	// render its explicit "Heartbeat metadata only" copy instead of an empty
	// source box. The non-nil empty slice is a health-aware agent with zero
	// sources, which clears any previously reported rows.
	if err := store.RecordHeartbeat(context.Background(), database.AgentHeartbeat{
		MachineID:       "smoke-silent",
		OperatingSystem: "linux",
		Architecture:    "amd64",
		SourceCount:     0,
		Sources:         []database.AgentSourceHealth{},
	}); err != nil {
		t.Fatal(err)
	}
	return store
}

// seedEmptyStore opens a scratch database with the smoke catalog but no usage
// events and no heartbeats, so the overview renders every empty state —
// including the machine-health panel's explicit "No machine heartbeats yet."
// row instead of an empty table body.
func seedEmptyStore(t *testing.T) *database.Store {
	t.Helper()
	store, err := database.Open(filepath.Join(t.TempDir(), "empty.db"), smokeCatalog())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	return store
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
	return fmt.Sprintf("http://tokemon:%s@%s", dashboardToken, listenOnLoopback(t, server.Handler()))
}

// startSeededPasswordServer serves the seeded scratch store behind a
// dashboard password account instead of URL-embedded Basic credentials, so
// the browser must sign in through the real login form. The returned base URL
// carries no credentials: anything authenticated must come from a session
// cookie.
func startSeededPasswordServer(t *testing.T) string {
	t.Helper()
	hash, err := bcrypt.GenerateFromPassword([]byte(smokeLoginPassword), bcrypt.MinCost)
	if err != nil {
		t.Fatal(err)
	}
	server, err := api.NewWithConfig(seedSmokeStore(t), api.Config{
		IngestToken:           "smoke-ingest",
		DashboardToken:        dashboardToken,
		DashboardUsername:     smokeLoginUsername,
		DashboardPasswordHash: hash,
		Logger:                slog.New(slog.DiscardHandler),
	})
	if err != nil {
		t.Fatal(err)
	}
	return "http://" + listenOnLoopback(t, server.Handler())
}

// listenOnLoopback serves the handler on a loopback port and returns the
// host:port; the listener closes when the test ends.
func listenOnLoopback(t *testing.T, handler http.Handler) string {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	go http.Serve(listener, handler)
	return listener.Addr().String()
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
