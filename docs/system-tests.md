# System and browser regression gate

CAR-67 keeps the release gate close to Tokemon's real seams. The fixture-backed
system test uses Claude Code and Generic JSONL records, two independent agent
state databases, the authenticated agent client, the batch API, and a clean
SQLite hub. It covers:

- independent machine attribution and durable source cursors;
- failed-upload retry with cursor/fingerprint retention;
- append, truncation/replacement, restart recovery, and duplicate upload
  idempotency;
- metadata-only event payloads (including private transcript fixtures);
- evolution threshold crossing and final lifetime totals; and
- authenticated dashboard and machine read APIs.

Run the focused gate locally:

```bash
go test ./internal/system ./internal/api -run 'SystemFlow|BrowserRegression' -count=1
```

The browser regression tests intentionally have no browser or JavaScript
dependency. They assert the rendered dashboard contract in CI: desktop and
390px responsive hooks, table/chart/filter selectors, overflow containment,
keyboard focus handlers, tooltip semantics, reduced-motion CSS, and empty-state
copy. This keeps failures deterministic on a clean Go checkout.

The headless-browser gate (`internal/browsercheck`) complements those contract
tests with a real Chromium pass over `/`, `/analytics`, `/tokedex`,
`/sessions`, and `/settings` against a seeded scratch database, run in CI
right after the Go tests. It asserts the interactive invariants the manual
passes kept catching (skip link to a real `#main-content`, horizontally
scrolling regions keyboard-reachable, `aria-sort` on sorted session headers,
export links using `download`, the Fresh pill) and runs a vendored axe-core
scan (pinned 4.10.3, MPL-2.0, in `testdata/`) over the WCAG 2.x A/AA tag set.
The scan emulates `prefers-reduced-motion` so contrast is measured on settled
text. Violations fail the gate unless allowlisted in `browsercheck_test.go`
with a documented reason — the current allowlist is only the overview's
deliberate 60-second auto-reload. Run it locally with:

```bash
TOKEMON_BROWSER_SMOKE=1 go test ./internal/browsercheck -count=1
```

After deploying the hub, complete the visual/interactive pass with the
in-app Browser skill:

1. At a desktop viewport (1440×900 or larger), authenticate through the normal
   dashboard flow and inspect Overview and Analytics. Exercise period,
   dimension, machine, provider, model, and harness filters; focus a trend
   column and a usage-share point with the keyboard; verify tables, tooltips,
   and empty/error states.
2. Set the viewport to exactly 390px wide (a phone-height viewport is fine),
   repeat the same filter and keyboard checks, and confirm charts/tables scroll
   within their panels without page-level horizontal overflow.
3. Enable the browser's reduced-motion preference and reload. Counters/charts
   must remain readable and no long animation should run.

Use only a test or operator-provided session. Do not print, capture, or commit
dashboard or ingest tokens, prompts, responses, source code, or repository
paths. Record the deployed hub URL, viewport sizes, checks performed, and any
failure in the CAR-67 Linear evidence comment.
