# Tokemon Improvement Plan (2026-11-23)

Audit of `tokemon` at `main` (`a486062`) covering `internal/api/server.go`, `internal/database/{database,dashboard,quota}.go`, open issues (`#19`–`#22`, `#38`), recent audits (`docs/analytics-latency.md`, `docs/data-audit-2026-11-19.md`, `docs/tokedex-roadmap.md`), and live database shape (`184,946` events, `149 MB`, `657` `machine_source_health` rows). All UI recommendations follow `STYLE_GUIDE.md` and preserve CAR-205 (`/insights` remains unlinked from primary nav).

## 1. High-Impact Bugs & UX Regressions Found on `a486062`

1. **Machine Health rendered 657 individual source rows inline on `/` (`internal/api/server.go`).**
   - After commit `9f5569c` raised `maxHeartbeatSources` to `4096`, the live database held **657** rows in `machine_source_health` (`377` on `Kartikeys-MacBook-Air-2`, `185` on `kpc-cachy`, `57` on `Kartikeys-MacBook-Air`, `38` on `fx505`).
   - Rendering every row inside a single `<td>` made one table row ~6,000+ px tall, buried *Recent sessions*, added `~1,300+` DOM nodes, and bloated pre-rendered `/` HTML.
   - **Fix:** Group per machine by adapter (`codex · 142 fresh`, `claude-code · 89 fresh`, errors/stale first) with a collapsed `<details>` list for stale/errored sources.
2. **Machine display aliases were ignored in Analytics breakdown and Usage Share legend.**
   - Both rendered `breakdownName` with `$.ModelAliases` even when `Dimension == "machines"`.
   - **Fix:** Pass both alias maps so machine aliases render consistently everywhere.
3. **All "When" and "Last seen" timestamps rendered in unlabeled UTC.**
   - `analyticsShortTime` used `parsed.UTC()`, ignoring `TOKEMON_ANALYTICS_TIMEZONE` (`America/Toronto`).
   - **Fix:** Format in `store.reportingLocation()` and emit `<time datetime="...">` with concise relative age.
4. **Overview hero flashed an empty composition bar and `—` mini-odometers on every load.**
   - Input/Output counters waited for client-side `fetch('/api/v1/evolution')`, and `pollLifetime` kept firing every 2s even when **Pause auto-refresh** was active.
   - **Fix:** Pre-render `TokenComposition` inside `Overview` HTML and respect `autoPaused()`.
5. **Startup ran a full-table Codex deduplication `DELETE` on every boot.**
   - `migrate()` ran the 12-column `GROUP BY` deduplication `DELETE` unconditionally even when `PRAGMA user_version == 4`, causing ~3s startup latency.
   - **Fix:** Guard one-time data migrations behind `if currentVersion < databaseSchemaVersion`.

## 2. UI / UX Improvements

| Priority | Improvement | Rationale |
| --- | --- | --- |
| P0 | Extract shared `base.css` + Go template chrome partials (`head`, `topbar`, `footer`) (Epic `#22`) | Eliminates 7x copy-paste CSS/HTML drift across templates (`1200px` vs `1408px`, `820px` vs `700px` wrap breakpoints, inconsistent active states) and reduces HTML payload by ~30–40%. |
| P0 | Clickable drill-downs across Overview, Analytics, and heatmaps | Clicking a Project, Harness, Model, or Machine row opens filtered `/analytics` or `/sessions`; clicking a heatmap day or trend bar opens day-filtered `/sessions`. |
| P1 | Declutter Analytics filter bar (Epic `#22`) | Drop duplicate `Breakdown` `<select>`, collapse secondary filters behind a compact toolbar with active filter pills and auto-submit so KPIs and charts sit above the fold. |
| P1 | Replace 60s full-page `location.reload()` on `/` | Use ETag/generation-driven in-place updates so counters, freshness, and recent sessions update smoothly without flashing or re-spinning every odometer reel. |
| P1 | Upgrade `/sessions` into an investigation table | Add Project filter, search input (session ID / project), pagination beyond `LIMIT 100`, compact token display with exact hover, and metadata-only `/sessions/<id>` detail view. |
| P2 | Keyboard-accessible `/insights` rhythm bars and smarter `/settings` | Add roving-tabindex readout on `/insights` 24h bars; usage-sort and filter `/settings` aliases with reset buttons and Tokedex cross-links. |

## 3. Speed & Delivery Improvements

### SQLite & Query Architecture

- **Split SQLite read and write handles + WAL PRAGMAs:** Keep `SetMaxOpenConns(1)` for writes and open a read-only pool (`?mode=ro`, `SetMaxOpenConns(4)`) with `synchronous=NORMAL`, `cache_size=-64000`, `temp_store=MEMORY`, and `mmap_size=268435456`.
- **Incremental `Facets` cache:** Update cached facet sets incrementally on ingest instead of re-running 4 full-table `SELECT DISTINCT` scans (477ms) on every batch.
- **Single-pass streaming `Analytics`:** Merge `analyticsSummary`, `analyticsPoints`, and `analyticsShareTimeline` into one streaming SQL pass.
- **Trim redundant `Overview` scans:** Reuse `MachinesAt` totals for `ByMachine`, fold `TokenComposition` into `overviewTotalsQuery`, and cap `/` recent sessions to `LIMIT 8`.
- **Close remaining uncached hot paths:** Switch `/api/v1/evolution` from hard-evict to SWR + 2s coalescing; cache `Store.QuotaStatus` (`#38`) with SWR + SQL day/provider/model aggregation; add composite index `(timestamp DESC, session_id, machine_id, provider, tool)` + SWR caches for `/sessions`, `/tokedex`, and `/insights`.

### HTTP, Assets & Rendering

- **Outbound gzip compression middleware** for HTML and JSON responses.
- **Immutable `/static/` caching** (`Cache-Control: public, max-age=31536000, immutable`) with content-hashed `?v=` query strings.
- **Lossless pixel-art PNG optimization** (`token-dex.png`, `stage-*.png`) plus `fetchpriority="high"` and explicit dimensions on `.creature-art`.
- **`content-visibility: auto; contain-intrinsic-size: auto none auto 320px;`** on strictly below-the-fold panels (verified for keyboard reachability).

## 4. High-Utility Product Features

1. **Codex `.jsonl.zst` archived-rollout collection** (`internal/adapters/codex`) plus optional `claude-desktop` harness separation (`internal/adapters/claude`).
2. **Cache savings and cost-efficiency metrics** in Analytics (`$X saved`, effective `$/1M` tokens, per-harness/model cache hit rates).
3. **UI-editable quotas in `/settings`** (SQLite `quotas` table alongside `TOKEMON_QUOTAS`), compact budget pace bar on `/`, and optional webhook/ntfy/local overage alerts.
4. **Enable Tokedex (`TOKEMON_ENABLE_TOKEDEX=true`)**, link uncataloged models from Analytics to `/tokedex#uncataloged-title`, and add `/tokedex/<model>` detail view + 30d cost what-if comparison.
5. **Activity Streaks** (`current`/`longest` streak + peak day) in the Token Activity header and custom `from`/`to` date range selection on `/analytics`.
6. **`tokemon status` CLI** (`--short`/`--json`) and privacy-safe authenticated **`/api/v1/card.svg`** share card (`#21`).
7. **Built-in daily WAL-safe automated backups** in `tokemon server` (retention 10) with last-backup age shown in `/settings`.

## 5. Planned Execution Slices

| Slice | Scope | Status |
| --- | --- | --- |
| Slice 1 — Quick Fixes & Hygiene | Source-health grouping, Analytics machine aliases, localized timestamps, pre-rendered hero composition, `migrate()` guard | Shipped (`a1e2bce`) |
| Slice 2 — Speed & Delivery | SQLite R/W pool + PRAGMAs, incremental facets, single-pass Analytics, SWR caches, gzip, immutable static caching, lossless PNGs | Shipped (`1016ca1`, `4aad6d4`) |
| Slice 3 — UI Polish & Drill-Downs | Shared `base.css` + partials, clickable drill-downs, compact Analytics filters, in-place `/` polling, `/sessions` + `/sessions/<id>`, `/insights` + `/settings` polish | Shipped (`b113605`, `3b6567c`, `18f9c78`) |
| Slice 4 — High-Utility Features | Codex `.jsonl.zst`, `claude-desktop`, `tokemon status`, daily backups, cache savings/`$/1M`, SQLite quotas + alerts, Tokedex detail/what-if, streaks + `from`/`to`, `/api/v1/card.svg` | Shipped (`7043384`, `fc344bd`, `ebc2b23`) |
