# Changelog

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
- Bounded JSONL record reads in the Claude, Copilot, generic, and OpenClaw adapters so one unterminated line cannot grow without limit.
- Kept agent backfills within the request deadline, split ingest batches by event count as well as size, and preserved token components when a provider total is inconsistent.
- Added a tag-driven release workflow that builds the four native archives, generates an SPDX SBOM, scans and publishes a multi-arch GHCR image, and signs every asset and the image with keyless Cosign.
- Added two-agent system and browser regression tests, a migration compatibility gate, cross-platform installer upgrade tests, malformed Hermes fixtures, and `tokemon art validate` for evolution art.
- Pinned Go 1.26.9 and digest-pinned base images across the module, CI, release workflow, and Docker build.

## 0.3.3

- Added a metadata-only Hermes Agent adapter for default and named-profile SQLite usage stores.
- Added an analytics usage-share timeline showing the top five harnesses, providers, models, or machines per window plus aggregated Others.
- Prevented new or archived Codex sessions from invalidating every unchanged session snapshot; late forks now reparse only the dependent child.
- Added a hub health preflight so offline hubs back off without repeatedly scanning local provider histories.
- Cached the dashboard evolution summary between ingests with a bounded freshness window, while keeping writes visible immediately and collapsing concurrent polling into one recompute.
- Hardened evolution and analytics accounting with schema drift warnings, missing-token attribution tests, and regression coverage for share timeline invariants.

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
