# Analytics latency under live ingest

Findings from the warm `GET /analytics` investigation (board task 039bbe0f,
2026-11-12). The reported symptom was 2-4s warm renders while the docs claim
15-30ms; warm `GET /` stayed at ~1.7ms.

## Method

- Copied the live hub database (`.backup`, never the live file) to
  `/tmp/analytics-repro.db` and ran the built server on `127.0.0.1:18788` with
  the live catalog and `TOKEMON_ANALYTICS_TIMEZONE=America/Toronto`.
- Emulated live ingest by POSTing one synthetic metadata-only event to
  `/api/v1/events/batch` every 10s (the live agent's ~300-400 events/hour), so
  each measurement ran through the real invalidate path.
- The live hub runs under launchd with `ProcessType=Background`, which macOS
  schedules at priority 4. The repro server was put at the same priority with
  `taskpolicy -b -p <pid>`; at normal shell priority the same workload measured
  10-13ms warm.

## Where the time goes

Instrumented timings for the analytics page (live-shaped data, 120k events,
30d/projects), one event every 10s:

| Stage | Before | After |
| --- | --- | --- |
| `cachedAnalytics` (stale serve) | 0.04-0.7ms | 0.04-0.7ms |
| `DisplayAliases` | 55-1177ms | 0.0007ms |
| `QuotaStatus` (no quotas configured) | 0.001ms | 0.001ms |
| template render | 13-35ms | 13-35ms |

The stale-while-revalidate path was holding: `cachedAnalytics` never joined the
in-flight compute and returned the last good result in well under a
millisecond. The time was spent in the handler's *next* store call.

## Root cause

The background analytics refresh holds the store's single SQLite connection
(`SetMaxOpenConns(1)`) for each stage of `Store.Analytics`. The longest stage is
the facets scan: on a production-shaped copy, `analyticsFacets` costs 477ms of a
~500ms refresh (summary 153ms, points 85ms, share timeline 69ms, sessions 63ms,
breakdown 56ms, lifetime 6.6ms). Ingest invalidates the facets cache on every
write, so each refresh pays that scan again.

A reader that arrives while the refresh runs still has to issue one query of its
own — `DisplayAliases` — and that query queues behind whichever stage is
running. Under the hub's background scheduling priority those stages stretch to
0.3-1.2s, so a "warm" render paid the refresh's latency even though the cached
analytics result was served instantly. The 2-4s outliers in the live log
coincided with whole-process stalls (`/healthz` 1-6s, readiness 503, heartbeat
12.5s) during the release window, not with analytics work.

## Fix

`Store.DisplayAliases` now caches the alias rows and `SetDisplayAlias` (the only
writer) drops the cache. The analytics page's warm path therefore performs no
database work at all: cached analytics + cached aliases + template render. This
is the smallest change that restores the documented invariant that a reader
never waits on a recompute once a last-good result exists.

Regression test: `TestAnalyticsPageWarmRenderNeedsNoDatabase` warms the page,
closes the store, and requires the next render to still return 200.

## Before/after

Five consecutive warm requests 1s apart with ingest running, four rounds,
priority 4, milliseconds:

| Round | Before | After |
| --- | --- | --- |
| 1 | 33, 44, 421, 26, 28 | 26, 17, 23, 22, 27 |
| 2 | 382, 548, 571, 239, 64 | 28, 30, 20, 21, 16 |
| 3 | 395, 265, 349, 25, 353 | 23, 15, 25, 13, 25 |
| 4 | 626, 243, 558, 22, 24 | 30, 25, 25, 16, 14 |

Warm `GET /` reference: 1-3ms in both runs.

At normal shell priority the same workload measured 10-13ms before the fix and
6-14ms after it: the regression only bites when the process is starved, which is
the live hub's condition (`ProcessType=Background`). The fix removes the
dependency on the refresh either way; the priority is what made it visible.

## Scheduling priority

The live hub ran under launchd with `ProcessType=Background`, which macOS
schedules at priority 4. That is the wrong class for an interactive dashboard
server: every query stretches by roughly an order of magnitude under load, and
it is what made the analytics refresh visible to readers in the first place.

`deploy/macos/install-server.sh` now writes `ProcessType=Interactive`, and the
installer smoke asserts the plist value so it cannot regress. After reloading
the live LaunchAgent the process priority moved from 4 to 31.

Twenty consecutive warm requests per path, live hub, ingest running, before
(priority 4) and after (priority 31):

| Path | Before: median / max | After: median / max |
| --- | --- | --- |
| `/healthz` | ~0.5ms / 669ms | 0.7ms / 1.2ms |
| `/` | ~1.0ms / 1.3ms | 0.9ms / 1.3ms |
| `/analytics` | 17ms / 2518ms | 8.2ms / 17.5ms |
| `/sessions` | 450ms / 3147ms | 70ms / 115ms |

Before numbers are the `duration_ms` values the hub logged at priority 4. The
priority change does not replace the cache fix; it removes the scheduling
penalty that amplified every remaining cost.

## Invalidation coalescing

Every accepted batch still invalidates the analytics cache, but the
invalidation no longer forces an immediate refresh. `invalidateAnalyticsCache`
records a refresh deadline on each cache entry: an entry refreshed within
`analyticsMinRefreshInterval` (2 seconds, `internal/api/server.go`) defers its
next refresh until the window elapses, and a burst of N ingests inside the
window therefore starts at most one ~0.5s recompute instead of N. A steady
1 event/10s stream now refreshes at most once per 2s window rather than once
per ingest, which cuts the refresh duty cycle and the window in which a reader
can queue behind a refresh.

Freshness trade-off: after an ingest, a cached analytics result can be at most
`analyticsMinRefreshInterval` (2s) staler than the store. Readers keep the
stale-while-revalidate contract — the last good result is served immediately
and a reader never blocks on a recompute once a last-good result exists. An
entry last refreshed before the window is due on the next read exactly as
before, and without an ingest `analyticsCacheTTL` (60s) still bounds staleness.

Regression tests: `internal/api/analytics_coalesce_test.go` (a burst of
invalidations produces one refresh, a reader inside the window is served the
cached result, an entry older than the window still refreshes on the next
read) and the ingest flow in `TestAnalyticsCacheServesAndInvalidates`.

## Dashboard pre-render race

The overview entry and the dashboard page rendered from it are now one cache
entry: `overviewCacheEntry` carries the `html` built from its `result`, and the
background refresh renders the page *before* it publishes the entry. A reader
therefore cannot observe a fresh overview paired with the old page — the state
that made `GET /` rebuild on the request path and run `DisplayAliases` plus a
`Sessions` query.

Before, `startOverviewRefreshLocked` installed the new entry, released
`overviewMu`, and only then called `buildDashboardHTML` in the same goroutine. A
`GET /` arriving in that window matched the new `fetched` time against the old
page, missed, and paid the rebuild (756-934ms when the `Sessions` query queued
behind the analytics refresh).

After, the refresh computes the overview, renders the page, and publishes both
under `overviewMu`. Readers keep the previous entry and its page for the
render's duration — the same stale-while-revalidate bargain they already accept
for the refresh itself — and the new entry is never visible without its bytes.
A failed render publishes the entry without a page, so the next reader falls
back to a request-path build (which is then attached to the entry) instead of
serving stale bytes forever.

Cost removed for a reader that used to land in the window, 20k-event synthetic
database, 30 samples, median: 80.7ms for the request-path rebuild (aliases,
sessions, template) versus 27.8µs for the pre-rendered page. On the live hub the
same rebuild measured 756-934ms because its `Sessions` query queued behind the
analytics refresh. Warm `GET /` is unchanged at 1-3ms.

Regression tests: `internal/api/dashboard_prerender_test.go` (a reader after a
background refresh is served the pre-rendered bytes with the store closed, a
fresh entry is never visible without its page, and a missing pre-render falls
back to a request-path build) plus the existing
`TestDashboardServesCachedPageAndTracksRefresh`.

## Not fixed here (follow-ups)

- **Quota panel.** With `TOKEMON_QUOTAS` configured, `QuotaStatus` still runs a
  per-request query and would queue behind a refresh the same way. The live hub
  has no quotas configured, so it was left alone.
