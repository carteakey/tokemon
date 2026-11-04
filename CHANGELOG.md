## Unreleased

- Fixed dashboard WCAG AA contrast: the analytics Apply-filters button no longer renders accent text on the browser's default gray button background (2.54:1 → 8.43:1), and dimmed labels, footers, table headers, and axis text use a faint-gray token that passes on both card and page backgrounds (3.77–4.04:1 → 5.04–5.39:1).
- Stopped the overview breakdown tables from truncating share percentages such as 100.0% by widening the share column (16% → 25%) in the fixed table layout.
- Made dashboard keyboard focus visible: the settings gear keeps a solid accent outline instead of dropping it, and window links, selects, and action buttons show solid outlines on focus instead of ~16–22% alpha ones.
- Reduced overview keyboard tab stops from 374 to 20 by making token-activity heatmap cells focusable only on days with usage or unknown tokens; the tooltip continues to open on focus for the remaining cells.
- Exposed odometer values to screen readers via role=img and aria-label so assistive tech hears the formatted total instead of every digit-reel value.
- Added a covering index over lifetime token totals so ingest batches, evolution recomputes, and dashboard renders stop scanning the full usage history.
- Cached dashboard overview renders between ingests with the same bounded freshness window as the evolution summary, collapsing concurrent dashboard, analytics overview, and settings loads into one recompute; a compute that raced an ingest can no longer reinstall stale results.
- Stopped the agent from re-writing unchanged source cursors on every poll cycle.

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
