## Unreleased

### Fixed

- **The dashboard server runs at interactive scheduling priority.** The macOS
  LaunchAgent was written with `ProcessType=Background`, so launchd scheduled
  the hub at priority 4 and stretched every request under load. The installer
  now writes `ProcessType=Interactive`, and the installer smoke asserts the
  plist value. On the live hub the process priority moved from 4 to 31:
  `/analytics` median 17ms → 8ms (max 2.5s → 18ms), `/sessions` median 450ms →
  70ms (max 3.1s → 115ms), `/healthz` max 669ms → 1.2ms. See
  `docs/analytics-latency.md`.
- **A valid session is no longer throttled.** `dashboardAuth` checked the login
  throttle before the session cookie, so a locked-out IP (a shared/NAT address,
  or anyone who mistyped a password earlier) got `429` even with a valid
  session. The cheap HMAC cookie is now validated first; the throttle runs only
  for requests that still present credential material, before any bcrypt work.
  Bare navigations no longer consume the login budget, and the `POST /login`
  throttle is unchanged. Fixes #40.
- **Analytics invalidations are coalesced behind a 2s window.** Every accepted
  ingest invalidated the analytics cache, so a steady stream started a ~0.5s
  refresh roughly once per ingest. An entry refreshed within
  `analyticsMinRefreshInterval` now defers its next refresh to the window's
  end, so a burst of N ingests starts at most one recompute. Stale-while-
  revalidate is unchanged; the cost is at most 2s of extra staleness after an
  ingest. Verified on a scratch instance: a single ingest appeared in 131ms, a
  10-event burst coalesced to one refresh and appeared after ~2.0s with no
  events lost.
- **Overlapping quotas no longer double-count.** `Store.QuotaStatus` measured
  every configured quota independently, so an event inside a provider+model
  quota also counted toward the provider quota and a catch-all, contradicting
  the documented "most specific matching quota applies" rule. Each event is now
  attributed to exactly one quota — the most specific match — and specificity
  ranks by the number of non-empty match fields, so a model-only quota beats a
  catch-all deterministically. Fixes #36.
- **`reset_day` 29–31 is accepted and clamps as documented.** `Validate`
  rejected anything above 28, so the documented "day 31 resets on Feb 28/29"
  clamping path was unreachable from config. `reset_day` now accepts 1–31 and
  the clamping is pinned by tests for February (leap and non-leap) and 30-day
  months. Fixes #37.
- **The dashboard pre-render is published with its overview entry.** The
  background overview refresh installed the new entry and only then rendered
  the page, so a `GET /` in that window missed the page cache and rebuilt it on
  the request path — including a `Sessions` query that queued behind the
  analytics refresh (756–934ms observed). The rendered page now rides on the
  overview entry and is built before the entry is published, so a fresh
  overview is never visible without its page and the two can never be stitched
  from different entries. Measured on a 20k-event synthetic database: 80.7ms
  median request-path rebuild vs 27.8µs for the pre-rendered page. Warm `/` on
  the live hub is 0.6–1.6ms. See `docs/analytics-latency.md`.

### Accessibility

- **Overflowing regions are keyboard-reachable.** The overview's daily activity,
  machine health, and recent sessions regions (and the analytics usage-share
  chart) scroll horizontally at common widths but had no `tabindex`, so a
  keyboard-only user could not scroll them. They now expose
  `tabindex="0" role="region"` with a descriptive label; the analytics
  dimension nav gets a `tabindex` only, so it keeps its navigation landmark.
- **The browser gate now renders three viewports.** Every gated page runs at
  1280×800, 768×900, and 390×844 with the shared invariants and the axe scan,
  so narrow-width accessibility is no longer unguarded. `/insights` and
  `/login` joined both gates; doing so surfaced and fixed two pre-existing
  `/insights` violations (insight cards missing a `role="list"` parent, and the
  Apply-filters button inheriting the UA button fill at 2.36:1 contrast). The
  axe allowlist stays empty.
- **Small consistency fixes:** the sessions export links are grouped instead of
  pushed to opposite ends of the header, and sortable session headers finally
  have a hover/focus/active style (`.session-sort` had no CSS rule at all).
- **Form controls meet the 3:1 non-text contrast floor.** Control borders used
  `--line-bright` (2.11:1 against the panel), below WCAG 1.4.11's requirement
  for UI component boundaries — and axe's A/AA tag set does not test it. A
  separate `--control-border` token (`#66705f`, 3.41:1) now applies to form
  fields and interactive buttons only; panels, cards, tables and pills keep the
  deliberate heavier border. `assertContrastPolicy` guards the token at ≥3:1.
- **Hero controls have real accessible names.** The lifetime counter announces
  "Lifetime tokens: N" instead of a bare number, and the evolution progressbar
  is named "Evolution progress" instead of reading as "42%". The settings form
  is named "Display name aliases" rather than borrowing the "Models" heading,
  and the active analytics dimension tab now carries `aria-current` plus an
  underline so it is not conveyed by colour alone.
- **The gate now catches document-level horizontal overflow** at every
  viewport. It immediately found a pre-existing 41px overflow on `/analytics`
  at 768px (the non-wrapping heading/export row); the heading now wraps below
  860px, leaving the desktop layout unchanged.
- **The heatmap is a real keyboard grid.** The overview's 53-week activity
  grid rendered one tab stop per active day (179 on the live hub, growing with
  the year) while its `role="grid"` promised arrow-key navigation. It now
  loads with exactly one tab stop — the most recent active day, chosen
  server-side so it holds without JS — and Left/Right move by day, Up/Down by
  week, Home/End to the row ends, carrying the tab stop with focus. Tooltips,
  labels, roles and geometry are unchanged.
- **Analytics charts are one tab stop each.** Every bucket was a Tab stop
  (60 at 30 days, ~180 at `period=all`). Each chart now renders one tab stop
  (the trend chart's peak bucket, else the first) with arrow-key navigation
  between buckets; the live readout, selection, tooltip and labels are
  unchanged.
- **Phone-width chrome and the 641px gap.** The gate gained a 641×900 viewport
  (one pixel above the 640px column breakpoint), which exposed a 2px overflow
  from the topbar gear on the pages where the Tokedex link makes four nav
  items; those templates now wrap up to 700px like analytics already did. Below
  500px the refresh and pause controls are icon-only with 32×32 touch targets
  and their `aria-label`/`aria-pressed` state intact, so the topbar no longer
  stacks to three rows.

### Tests

- **The release browser smoke now covers the login/session/logout flow.** It
  drives a real headless Chrome through the form, a rejected password (401 +
  `role="alert"`), a successful login (303 + session cookie), logout without
  the CSRF token (403), logout with it (303 + cookie cleared), and the
  signed-out redirect. This is the gap that let the v0.4.3 anonymous-HTML
  redirect regression reach the gate. Fixes #39.
- **The browser smoke gates the per-source connector health panel.** A
  dedicated subtest pins each seeded adapter to its expected state
  (Fresh/Stale/Unknown), asserts the state is announced as text rather than
  colour-only, covers the never-synced source and its inline error, and covers
  both empty states (a machine that reported no sources, and an empty
  database). Fixes #26.

## 0.4.4

A correctness-and-cost pass: the Tokedex MVP is complete behind its flag,
the catalog finally prices most of what the hub actually uses, warm analytics
renders stop queuing behind ingest, and CI runs again.

### Added

- **Tokedex MVP is complete** (still opt-in: `/tokedex` answers 404 until
  `TOKEMON_ENABLE_TOKEDEX` is set). The page now has a tier legend, honest
  empty/unknown states, and client-side sort / filter / search over the model
  cards — sort by tokens, events, or name within each tier, filter by tier,
  provider, and priced-vs-unknown, and free-text search over display names and
  aliases. No new endpoints; the controls are progressive enhancement over the
  fully server-rendered grid, and the page stays axe-clean.
- **Catalog pricing verification:** 24 of 35 models now carry `mode: standard`
  rates with a source URL and `verified_at` date, covering 97.3% of observed
  tokens on the live hub (up from 32.5%). Models whose providers publish only
  tiered, time-of-day, or promotional rates stay `mode: unknown` rather than
  guessing.

### Fixed

- **Warm `/analytics` no longer queues behind the ingest refresh.** The
  stale-while-revalidate path was serving the cached result in under a
  millisecond, but the handler's next store call (`DisplayAliases`) shared the
  store's single SQLite connection with the background analytics refresh and
  waited for its longest stage. Alias rows are now cached in the store and
  dropped on write, so a warm render does no database work at all. Live
  measurement under continuous ingest: 20 consecutive warm requests, median
  23ms, max 29ms (previously spikes of 0.3–1.2s and outliers to 4s). See
  `docs/analytics-latency.md`.
- **Security: Go 1.26.11.** GO-2026-6859 (HTTP/1 server crash from a
  `MaxBytesReader` header race in `net/http`) is reachable from the dashboard
  handler and the server's own `MaxBytesReader` use. Bumped `go.mod`, the
  container build image, and every workflow.
- **CI runs were cancelled on every push.** GitHub retired the macOS 14 images
  on 2026-11-02; a job targeting `macos-14` is cancelled one second in with no
  runner, which marks the whole run cancelled even when every other job passes.
  The installer matrix now uses `macos-15` for the Apple Silicon leg.
- The site's local quick-start snippet runs the server in the background and
  waits for `/healthz` before the agent's one-shot sync, so pasting it actually
  reaches the server.

## 0.4.3

A speed and account pass: pages stay fast across ingest cycles and cold
restarts, the dashboard gained a real login form with signed sessions, the
machine health panel gained per-source connector state, usage quotas became
trackable, and the release pipeline learned to check itself before tagging.

### Upgrade notes

- **Login form (new):** when a password account is configured
  (`TOKEMON_DASHBOARD_USERNAME` + `TOKEMON_DASHBOARD_PASSWORD_HASH`), browsers
  get a real login page and a `tokemon_session` cookie (HttpOnly,
  SameSite=Lax, Secure on TLS or trusted-proxy-terminated HTTPS, 12-hour
  lifetime) instead of the Basic Auth prompt. Only wrong credentials consume
  the login-throttle budget — expired or forged cookies are cleared and
  redirect without locking a human out. Sessions are stateless: there is no
  server-side revocation, so a copied cookie stays valid until TTL or
  credential rotation. Without a password account nothing changes (the form
  is a 404 and session cookies are never accepted); deployments with no
  dashboard token reset sessions on restart. Basic Auth, Bearer tokens, and
  trusted-proxy identities keep working unchanged.
- The Tokedex catalog UI ships opt-in: `/tokedex` and `/catalog` answer 404
  and drop out of the navigation until `tokemon serve --enable-tokedex` (or
  `TOKEMON_ENABLE_TOKEDEX`) is set. The catalog and models read APIs stay
  open.
- Usage-vs-quota tracking is off by default: point `tokemon serve --quotas`
  (or `TOKEMON_QUOTAS`) at a quotas YAML file defining monthly provider
  budgets; without a file nothing changes.
- The overview's unconditional 60-second meta refresh is replaced by an
  explicit control: "Refresh now" reloads on demand and "Pause auto-refresh"
  (remembered per session) stops the timer, per WCAG 2.2.1.

### Changes

- Sped up the dashboard and analytics pages under ingest churn: the overview
  serves a pre-rendered page that the background refresh rebuilds (warm
  requests drop from seconds to milliseconds), machine lifetime/today/week
  aggregates are computed with the cached overview instead of per render,
  and analytics results are cached per filter (bounded at 64 filters) and
  re-served stale while one refresh runs — no page load waits on a recompute
  after an ingest.
- Sped up cold starts: as the listener comes up, the server warms its caches
  in the background — the overview and its pre-rendered dashboard first, then
  the default analytics window, strictly in that order because the two full
  scans contend on the same SQLite database. Once the warm-up completes,
  readers get cached bytes immediately; a request arriving while a warm-up
  compute is still in flight joins it and pays the remaining time, so the
  very first page after a restart can still wait on part of the recompute.
- Fixed 500 "context canceled" errors when a reader reloaded mid-compute:
  shared cache computes run detached from the triggering request, and
  handlers no longer fail when the client goes away.
- Added a real login form for the password account: CSRF-protected form with
  same-origin Origin/Referer enforcement on login and logout, a signed
  stateless session cookie whose key is derived from the dashboard token
  (random per-process key when no token exists), an open-redirect-safe `next`
  parameter, and a login throttle shared with Basic/Bearer attempts that is
  checked before any bcrypt work.
- Added per-source connector health: heartbeats report each source's last
  successful sync and a coarse error class (never raw paths), the machine
  health panel shows a Fresh/Stale/Unknown pill per source, and the browser
  smoke covers the panel's four health shapes and the quota panel.
- Added the analytics Insights strip: four deterministic, usage-derived stats
  under the stat row with honest Unknown and omission states.
- Filled the model catalog: 23 new entries folding observed raw model forms
  into canonical names, and priced-coverage accounting that separates
  cataloged models with unpublished rates from genuinely uncataloged ones.
- Hardened dashboard authentication: constant-time username comparison, a
  bcrypt 72-byte input limit warning in `tokemon passwd`, and bounded rate
  limiting with backoff for failed login attempts.
- Improved dashboard accessibility and polish: the analytics trend chart is a
  labelled region, session/model IDs gained a click-to-copy affordance, the
  catalog help links to the how-to guide, and analytics "Unknown" buckets
  label themselves per dimension (Unknown project, model, machine, harness,
  provider).
- Rounded out CI: staticcheck, actionlint, markdownlint, and go mod tidy
  gates, plus a headless-browser smoke with an axe-core accessibility
  regression gate.
- Hardened the release pipeline: the container image bakes and asserts the
  real version and commit, and a pre-tag dry run (browser smoke, image
  identity, trivy scan, race suite) runs against a candidate commit before
  tagging.
- Agent internals: one SQLite handle cleanup path with reopen and evict
  debug logging (via `TOKEMON_LOG_LEVEL`) and a Close hook for future
  adapter shutdown wiring.

## 0.4.2

The dashboard quality release: authentication options, accessibility,
honest totals, and export tooling, on top of significant ingest and
rendering performance work.

### Upgrade notes

- `tokemon serve` now implements the documented `--dashboard-token`
  default: with no dashboard token configured, the ingest token
  authenticates the dashboard too. Environments that previously relied
  on the dashboard returning 401 without an explicit
  `TOKEMON_DASHBOARD_TOKEN` should set one (or an explicit empty value)
  to keep the web UI closed.
- Optional password accounts: generate a hash with `tokemon passwd` and
  set `TOKEMON_DASHBOARD_USERNAME` together with
  `TOKEMON_DASHBOARD_PASSWORD_HASH` (both or neither; the server refuses
  to start on a mismatch). Humans can then sign in without holding the
  raw ingest token.
- The first start after upgrading builds the new covering index over
  lifetime token totals; expect a one-time pause on large databases
  before ingest and rendering pick up the speedups.
- CSV exports (sessions and analytics breakdown) are additive; existing
  JSON export URLs are unchanged.

### Changes

- Added optional dashboard password authentication: `tokemon passwd` writes a bcrypt hash; configure it with `TOKEMON_DASHBOARD_USERNAME` and `TOKEMON_DASHBOARD_PASSWORD_HASH` to sign in with a username and password instead of the raw dashboard token. Token logins, CSRF, and the ingest token requirement are unchanged.
- Fixed dashboard WCAG AA contrast: the analytics Apply-filters button no longer renders accent text on the browser's default gray button background (2.54:1 → 8.43:1), and dimmed labels, footers, table headers, and axis text use a faint-gray token that passes on both card and page backgrounds (3.77–4.04:1 → 5.04–5.39:1).
- Stopped the overview breakdown tables from truncating share percentages such as 100.0% by widening the share column (16% → 25%) in the fixed table layout.
- Made dashboard keyboard focus visible: the settings gear keeps a solid accent outline instead of dropping it, and window links, selects, and action buttons show solid outlines on focus instead of ~16–22% alpha ones.
- Reduced overview keyboard tab stops from 374 to 20 by making token-activity heatmap cells focusable only on days with usage or unknown tokens; the tooltip continues to open on focus for the remaining cells.
- Exposed odometer values to screen readers via role=img and aria-label so assistive tech hears the formatted total instead of every digit-reel value.
- Added a covering index over lifetime token totals so ingest batches, evolution recomputes, and dashboard renders stop scanning the full usage history.
- Cached dashboard overview renders between ingests with the same bounded freshness window as the evolution summary, collapsing concurrent dashboard, analytics overview, and settings loads into one recompute; a compute that raced an ingest can no longer reinstall stale results.
- Stopped the agent from re-writing unchanged source cursors on every poll cycle.
- Added CSV export for the current analytics breakdown and for sessions alongside the existing JSON exports, so spreadsheets get the same filtered view the dashboard renders.
- Added server-side sortable columns (When, Total, Cost, Duration) to the session timeline; headers are links that expose aria-sort, and sort state rides in the URL so filtered, sorted views can be shared.
- Added a data-freshness pill to the overview Today card that reflects the age of the newest persisted event.
- Made session tables readable: `ses_…` identifiers collapse into a friendly display name, and the literal `unknown` model aggregate is labeled "Unknown model" in breakdowns and the Tokedex instead of surfacing the raw sentinel.
- Disclosed what was previously silent: unpriced tokens now appear on both cost cards instead of blending into estimates, and the Tokedex states pricing coverage plus a dedicated "Uncataloged models" section for models seen in usage but missing from the bundled catalog.
- Derived Claude project labels from the transcript cwd and the `.claude` projects folder so project breakdowns no longer depend on adapters reporting an explicit project; usage from providers that never report a project now buckets as "Unknown project" in the overview instead of being silently dropped, and displayed shares reconcile with lifetime totals.
- Sped up polling and rendering: per-source SQLite handles are reused across cycles instead of reopened, the Codex session metadata index survives polls, Codex session logs tail-parse incrementally instead of re-reading from byte 0 (benchmarks included), Overview's scalar queries collapsed into one pass, and the DailyUsage UTC fast path skips per-row timestamp parsing while validating calendar dates.
- Implemented the documented `--dashboard-token` fallback: serving with only `TOKEMON_INGEST_TOKEN` now authenticates the dashboard with it instead of returning 401, matching the README and 0.4.0 upgrade notes.
- Fixed Codex ingestion dropping a source's aggregate when a detailed event carried no total, and rewinding the Codex cursor over an unterminated final record so a partially written line cannot wedge the source.
- Documented dashboard authentication (token, password, precedence), standardized the unknown-value vocabulary across docs and UI, disclosed unknown markers in the dashboard README section, and recorded the known-gaps audit in `docs/project-unknowns-audit.md`.

## 0.4.1

- Added a metadata-only ZCode adapter reading `model_usage` from `~/.zcode/cli/db/db.sqlite`. Terminal CLI, desktop-app, and subagent sessions share that store, so one source covers every ZCode mode; usage aggregates per session and model with cache-aware input decomposition.
- Environments that pin `TOKEMON_ADAPTERS` must add `zcode` to the list to pick the adapter up; unset selections enable it by default.

## 0.4.0

The first signed Tokemon release and the first published release since 0.3.2.
It also ships the 0.3.3 changes, which were tagged but never published as a
GitHub Release.

### Upgrade notes

- `tokemon serve` now refuses to start without `TOKEMON_INGEST_TOKEN`. An empty token is accepted only with `--dev-loopback` (or `TOKEMON_DEV_LOOPBACK=true`) on an explicit loopback listen address.
- Dashboard pages, read APIs, and `/metrics` now require Basic auth (`tokemon:<token>`) or a Bearer token. The credential defaults to the ingest token; set `TOKEMON_DASHBOARD_TOKEN` for a separate one. `/healthz` stays unauthenticated.
- `deploy/install-agent.sh` rejects inline `--token` values; provide the token through `TOKEMON_INGEST_TOKEN` or `--token-stdin`.
- The installer now requires Cosign and accepts only signed release assets, so `--version` must name 0.4.0 or later.
- Release images are published to `ghcr.io/carteakey/tokemon` by semver and digest only; there is no `latest` tag.

### Changes

- Hardened the hub trust boundary with CSRF and same-origin checks on settings, constant-time token comparison, a restricted loopback bypass, an opt-in trusted-proxy boundary (`TOKEMON_TRUSTED_PROXY_CIDRS`), bounded request bodies, a 1,000-event ingest batch cap, and stricter event validation.
- Added one outbound privacy guard for `tokemon inspect` and every upload. It rejects content, credentials, tool arguments, full paths, titles, and unapproved metadata before a request opens, without advancing the cursor. Project labels are lowercase basenames, and `TOKEMON_INCLUDE_PROJECTS=false` omits them.
- Normalized path-like Copilot and OpenCode session IDs so one unsafe ID cannot block a batch.
- Added `tokemon backup create`, `verify`, and `restore` with WAL-safe versioned snapshots, retention, a pre-restore snapshot, and the `deploy/tokemon-backup.sh` scheduler wrapper.
- Added a bind-mount-safe database lock so `serve`, `import`, `export`, `purge`, and migrations never run concurrently.
- Added bounded HTTP timeouts and graceful shutdown (`TOKEMON_SERVER_*_TIMEOUT`), a SQLite-backed `/healthz` readiness probe, authenticated `/metrics` counters, credential-free JSON logs, and an agent request deadline (`TOKEMON_AGENT_REQUEST_TIMEOUT`).
- Added deployment preflight and postflight guards, a release-context guard for persistent data, and an incident-response runbook.
- Added the `/tokedex` model catalog, a filterable metadata-only `/sessions` timeline with export, machine health, and today/week context, plus timeline, sessions, machines, catalog, and models read APIs.
- Added an `/insights` page of deterministic aggregate cards. An optional AI recap runs only when `TOKEMON_INSIGHTS_OPENAI_API_KEY` is set and sends bounded aggregates with `store: false`. The page is not yet linked from the main navigation.
- Improved dashboard accessibility, mobile navigation, and readability.
- Fixed overlapping columns in the overview Machine health and Recent sessions tables, added labelled phone layouts for them and the Sessions timeline, unified navigation and headers across every page, and made model versions (`Opus 5.5`), costs (`$9,295.77`), and long durations (`21m 33s`) read naturally.
- Bounded JSONL record reads in the Claude, Copilot, generic, and OpenClaw adapters so one unterminated line cannot grow without limit.
- Kept agent backfills within the request deadline, split ingest batches by event count as well as size, and preserved token components when a provider total is inconsistent.
- Added a tag-driven release workflow that builds the four native archives, generates an SPDX SBOM, scans and publishes a multi-arch GHCR image, and signs every asset and the image with keyless Cosign.
- Added two-agent system and browser regression tests, a migration compatibility gate, cross-platform installer upgrade tests, malformed Hermes fixtures, and `tokemon art validate` for evolution art.
- Pinned Go 1.26.9 and digest-pinned base images across the module, CI, release workflow, and Docker build.
## 0.3.2

- Published the first MIT-licensed, public-ready Tokemon release.
- Added polished public documentation, deployment guidance, security policy, and release notes.
- Added CI coverage for race tests, static analysis, Compose validation, and all four release archives.
- Consolidated the current provider adapters, metadata-only privacy model, multi-machine dashboard, analytics views, and checksum-verified installers into one release line.

## 0.3.1

- Added authenticated downloads for private GitHub release assets.
- Preserved custom agent state paths during upgrades.
- Corrected default state and install paths for alternate homes.

## 0.3.0

- Added modular built-in provider adapters and per-machine adapter selection.
- Added metadata-only agent heartbeats and deployment diagnostics.
- Added version and build identity to the CLI and agent metadata.
- Added checksum-verified macOS and Linux release archives for arm64 and amd64.
- Added user-level launchd and systemd installation with preserved cursor state.
