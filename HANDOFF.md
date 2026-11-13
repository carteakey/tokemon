# Tokemon handoff — 2026-11-13

Written by supervisor (`super`, opencode) after the post-v0.4.4 performance and
accessibility batch. Everything below was verified against the live hub and
`origin/main` unless it says otherwise.

## TL;DR

- `origin/main = 6e3182f`, clean, CI **green**.
- **v0.4.4 is still the latest tag.** This batch is unreleased; `CHANGELOG.md`
  has an `## Unreleased` section with all of it.
- The live Mac hub runs `0.4.5-dev` at `d534a6c` (one docs-only commit behind
  main), launchd priority **31**, `/healthz` 200.
- **15 commits landed** since the last handoff: interactive launchd priority,
  analytics invalidation coalescing, the dashboard pre-render race, the login
  throttle fix, quota attribution, and a large accessibility slice.
- **16 GitHub issues closed** (#23–#33, #35–#37, #39, #40). Open: #38 and the
  #19–#22 epics.
- **Three worker branches are in flight** (heatmap roving tabindex, chart
  roving tabindex, mobile chrome). All pushed at base `6e3182f`; none has code
  yet.
- The Herding control socket is still dead (`carteakey/herding#77`), so the
  team CLI is unusable; the board was read from `team.sqlite3` directly.

## What shipped (this batch, on main)

| Commit(s) | Area |
| --- | --- |
| `6a295d2` | Perf: launchd `ProcessType=Background` → `Interactive` (priority 4 → 31) |
| `53b2562` | Perf: coalesce analytics cache invalidations behind a 2s window |
| `eeb6da7` | Perf: publish the dashboard pre-render with its overview entry |
| `d6fb1ca` | Fix: a valid session is no longer throttled (#40) |
| `511f6dc` | Test: browser smoke covers login/session/logout (#39) |
| `1da4b94` | Test: browser smoke gates the connector-health panel (#26) |
| `aaeaa28` | Fix: quota most-specific attribution + `reset_day` 29–31 (#36, #37) |
| `6902042` | A11y: keyboard-reachable overflow regions + 3-viewport gate |
| `d534a6c` | A11y: `--control-border` 3.41:1, hero names, document-overflow gate |
| `0aa00fd`, `d3b0e69`, `97bea08`, `33c1e7d`, `da8bedc`, `6e3182f` | CHANGELOG / docs |

### Measured results

- **launchd priority** (live hub, 20 warm requests each): `/analytics` median
  17ms → 8ms (max 2.5s → 18ms), `/sessions` median 450ms → 70ms (max 3.1s →
  115ms), `/healthz` max 669ms → 1.2ms. Numbers recorded in
  `docs/analytics-latency.md`.
- **Invalidation coalescing** (scratch instance): a single ingest appeared in
  131ms; a 10-event burst coalesced to one refresh and appeared after ~2.0s
  with no events lost.
- **Pre-render race**: 80.7ms median request-path rebuild vs 27.8µs for the
  pre-rendered page (20k-event synthetic DB). Warm `/` on the live hub is
  0.6–1.6ms.
- **Accessibility**: 7 pages × 3 viewports (1280×800, 768×900, 390×844), axe
  allowlist **empty**. The new document-overflow check immediately found a
  pre-existing 41px overflow on `/analytics` at 768px, now fixed.

## Live hub

- Service: launchd `com.tokemon.server`, binary `~/.local/bin/tokemon`
  (`0.4.5-dev`, `d534a6c`), config `~/.config/tokemon/server.env`, DB
  `~/repos/tokemon/data/tokemon.db`, catalog
  `~/Library/Application Support/Tokemon/models.yaml`.
- Listen: `0.0.0.0:18787`. **Priority 31** (`ProcessType=Interactive`).
- Verified: `/healthz` 200, anonymous HTML `/` → `303 /login?next=%2F`,
  `/login` 200, Bearer `/`, `/analytics`, `/sessions`, `/settings`,
  `/insights` all 200, `/tokedex` 404 (still gated), `/api/v1/quotas` 200.
- Warm latency right now: `/` 0.8–2.8ms, `/analytics` 5.5–11ms, `/sessions`
  92–128ms (first request after a restart can be ~0.9s).
- **Restart gotcha:** after replacing the binary, macOS kills the unsigned copy
  (`rc=137`). Run `xattr -d com.apple.provenance ~/.local/bin/tokemon` and
  `codesign --force --sign - ~/.local/bin/tokemon`, then
  `launchctl kickstart -k gui/$(id -u)/com.tokemon.server`. Startup now takes
  ~3s before `/healthz` answers.
- **Catalog deployment path:** the hub loads
  `~/Library/Application Support/Tokemon/models.yaml`, not the repo copy.

## In flight (worker branches, pushed at base `6e3182f`)

| Branch | Worker | Task |
| --- | --- | --- |
| `fix/heatmap-roving-tabindex` | zcode1 | One tab stop instead of 179 on the activity heatmap; arrow-key day/week nav |
| `fix/chart-roving-tabindex` | zcode2 | One tab stop per analytics chart instead of 60–180; arrows move buckets |
| `fix/mobile-chrome` | zcode3 | Sessions 641px overflow; icon-only refresh/pause at ≤500px with 32px targets |

All three are still in the investigation phase (no code committed). They were
asked to commit and push at their first working increment and again at the end.
Gate each at its exact commit, cherry-pick onto main, deploy, verify.

## Team mechanics (read before spawning workers)

- **Herding's control socket is dead while the app runs.** `herding team
  status/task/bulletin` all return `not_running` / `Connection refused`. The app
  process is alive and redrawing (~43% CPU); its listener never accepts. Filed
  as **`carteakey/herding#77`**. Do **not** kill the app: it hosts every agent
  pane, including the supervisor's.
- Consequence: the board cannot be written. Read `team.sqlite3` directly
  (read-only is safe; the app holds a ~4 MB WAL, so do not hand-edit). The one
  open board task, `039bbe0f` (warm `/analytics`), is fixed but cannot be
  closed.
- **zcode workers are alive but flaky.** All three run
  `account:zai-start-plan/GLM-5.3-Flash` with ~85% context left. Turns
  intermittently fail with `Model request failed / Turn failed` (quota class),
  then resume when woken. A retry prompt can fail in ~1s. Recorded on
  `carteakey/herding#55`.
- Workers were dispatched with `herdr pane run <pane> "Read /tmp/brief-*.md ..."`
  because team wakes/bulletins are unavailable. Reports come back in the pane.
- **Mistake to avoid:** opencode subagents were dispatched into the *same*
  worktrees the zcode workers were using. Nothing broke, but give each agent
  its own worktree.
- Subagents (`general`) are a reliable fallback when zcode dies; they produced
  the pre-render, connector-health, and both UI slices.

## Open issues / follow-ups

1. **#38 — `Store.QuotaStatus` is uncached** (~139ms per quota per render).
   Quotas are off on the hub, so this is not live-impacting.
2. **#22 — dashboard visual hierarchy / responsive controls** (epic). The
   remaining UI work: analytics IA (collapse the filter panel, drop the
   duplicate dimension `<select>`), and CSS extraction. The shared blocks
   (`:root`, `.topbar`, `.nav`, `.footer`, `.skip-link`) are duplicated across
   all seven templates — the root cause of most drift. That should be its own
   slice with the browser gate run at every viewport.
3. **Sessions latency** is 92–128ms warm (was 70ms earlier in the batch); it is
   an uncached query. Worth a look if it grows.
4. **CI runner flake:** on 2026-11-12 the `macos-15` arm64 installer job sat
   queued 15 minutes with `runnerName: null` and was cancelled, failing a
   docs-only run. A re-run was green. Two macOS legs per push is the cost
   centre; if it recurs, consider gating the macOS legs to PRs/tags.
5. **CAR-205:** `/insights` stays reachable behind dashboard auth but is
   deliberately **not** linked from primary navigation. Do not "fix" this.
6. **Tokedex** is still gated off (`TOKEMON_ENABLE_TOKEDEX` unset) pending the
   user's review.
7. **Pricing drift:** `gpt-5.6-luna`, `gpt-5.6-terra`, `gpt-5.6-sol` remain on
   old catalog numbers by choice; decide whether the catalog tracks current
   published/promotional rates.

## How to operate

- Gate = `gofmt -l .` clean + `go build ./...` + `go vet ./...` +
  `go test ./... -count=1` at the exact commit, plus
  `go test ./internal/api -race -count=1` for auth/cache changes and
  `TOKEMON_BROWSER_SMOKE=1 go test ./internal/browsercheck -count=1` for
  template/JS/CSS changes. The browser smoke is now 7 pages × 3 viewports.
- Land by cherry-picking onto current `main` in a throwaway worktree, gating
  there, then `git merge --ff-only` and push. Deploy only when the server
  changed.
- Deploy: build `./cmd/tokemon` with
  `-X internal/version.Version=0.4.5-dev -X ...Commit=<sha>`, install to
  `~/.local/bin/tokemon`, sign, restart launchd, verify `/healthz` + auth + the
  changed page.
- Release: tag only after `bash deploy/release-check.sh` on the candidate
  commit (browser smoke, image identity, trivy, full `-race`).

## Reference artifacts

- Herding control-socket bug: `carteakey/herding#77`
- zcode quota/health recurrence: `carteakey/herding#55`
- Analytics findings: `docs/analytics-latency.md`
- Tokedex roadmap: `docs/tokedex-roadmap.md`
- Last release: https://github.com/carteakey/tokemon/releases/tag/v0.4.4
