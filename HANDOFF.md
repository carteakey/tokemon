# Tokemon handoff — 2026-11-12

Written by supervisor (opencode) after closing the v0.4.3 follow-up batch.
Everything below is verified against the live hub and `origin/main` unless it
says otherwise.

## TL;DR

- `origin/main = caf7b4d`, clean, CI **green** (first green run since
  2026-11-07).
- The live Mac hub runs **`0.4.4-dev` @ `caf7b4d`** (go1.26.11), with the
  password login form enabled (`admin` / the password in
  `~/.config/tokemon/server.env`). Tokedex is still **gated off**; quotas are
  still **off**.
- **Tokedex MVP is complete** (roadmap items 1–4). It stays opt-in until the
  user reviews it and agrees the page earns its nav slot.
- **Warm `/analytics` is fixed**: 20 consecutive warm requests under live
  ingest, median 23ms, max 29ms (was 0.3–1.2s with 4s outliers).
- **CI cancellation explained and fixed**, and the fix exposed a real
  security gate failure (GO-2026-6859) which is also fixed.
- No open board tasks. Two decisions are waiting on the user (see
  "Waiting on you").

## What shipped (this batch, on main)

| Commit(s) | Area |
| --- | --- |
| `97f94d7` | CI: `macos-14` (retired 2026-11-02) → `macos-15`; runs stop being cancelled |
| `92e5c4e` | Build: Go 1.26.11 for GO-2026-6859 (`net/http` MaxBytesReader race) |
| `4854df0` | Catalog: pricing verification, 24/35 models, 97.3% of observed tokens |
| `460091f` | Tokedex: client-side sort / filter / search over the card grid |
| `caf7b4d` | Perf: warm analytics page does no database work (`docs/analytics-latency.md`) |
| `b252074` | Site: local quick-start runs the server in the background, waits for `/healthz` |

`55b7edd` (tier legend + empty/unknown states) and `c63f6d5` (staticcheck fix)
landed just before this batch and were already on main.

## CI: why every run was cancelled

GitHub retired the macOS 14 runner images on **2026-11-02**. A job targeting
`macos-14` is cancelled one second in with `runner_name` empty and zero steps,
and GitHub marks the **whole run** `cancelled` even when `test` and the other
installer jobs pass. There was never a `concurrency` block involved.

Evidence: runs `42949174186`, `42934364780`, `42678266857`, `42677820011`,
`42426091687` all show the identical signature on `Installer smoke
(macos-14)`.

With that fixed, the `test` job failed honestly on **GO-2026-6859** — an
HTTP/1 server crash from a `MaxBytesReader` header race, reachable from
`api.responseStatusWriter` and the server's own `MaxBytesReader` use, fixed in
go1.26.11. `go.mod`, the Dockerfile build image (digest re-resolved from the
Docker Hub index) and `setup-go` in all three workflows are now on 1.26.11.

Green run: `43258375794` (all four jobs).

## Live hub

- Service: launchd `com.tokemon.server`, binary `~/.local/bin/tokemon`
  (`0.4.4-dev`, `caf7b4d`, go1.26.11), config `~/.config/tokemon/server.env`,
  DB `~/repos/tokemon/data/tokemon.db` (~120k events), catalog
  `~/Library/Application Support/Tokemon/models.yaml`.
- Listen: `0.0.0.0:18787`.
- **The hub runs at launchd `ProcessType=Background` (priority 4).** That is
  why per-request latency on the hub is much worse than the same binary at
  normal priority. Keep it in mind before trusting any local benchmark.
- Verified after the deploy: `/healthz` 200 after 14s; anonymous HTML `/` →
  `303 /login?next=%2F`; `/login` 200; Bearer `/` and `/analytics` 200;
  `/tokedex` 404 (still gated); `/api/v1/quotas` 200; catalog 35 models, 24
  priced, `priced_tokens` 22.05B / 22.66B observed.
- Pricing is visible: session costs now render (`$0.04`, `$0.07`) instead of
  `Unknown` for priced models.
- **Restart gotcha (unchanged):** after replacing the binary, macOS kills the
  unsigned copy (`rc=137`). Run
  `xattr -d com.apple.provenance ~/.local/bin/tokemon` and
  `codesign --force --sign - ~/.local/bin/tokemon`, then
  `launchctl kickstart -k gui/$(id -u)/com.tokemon.server`. Startup takes
  ~9–14s before `/healthz` answers.
- **Catalog deployment path:** the hub loads
  `~/Library/Application Support/Tokemon/models.yaml`, not the repo copy.
  Copy the file and restart after catalog changes.

## Analytics latency (task 039bbe0f) — fixed

The stale-while-revalidate path was working: `cachedAnalytics` served the last
good result in under a millisecond and never joined the in-flight compute. The
time went to the handler's *next* store call, `DisplayAliases`, which shares
the store's single SQLite connection (`SetMaxOpenConns(1)`) with the background
analytics refresh. The refresh's longest stage is the facets `DISTINCT` scan
(~477ms of a ~500ms refresh), and ingest invalidates the facets cache on every
write, so a reader arriving mid-refresh queued behind it. At priority 4 those
stages stretch ~8–10x, which is where the 0.3–1.2s came from. The 2–4s
outliers coincided with whole-process stalls during the release window
(`/healthz` 1–6s, readiness 503, heartbeat 12.5s), not with analytics work.

Fix: cache the alias rows in the store, dropped by `SetDisplayAlias` (the only
writer in the process). A warm analytics render now does zero database work.

Live measurement after the deploy, ingest running, 20 consecutive requests 1s
apart: min 13ms, median 23ms, p90 28ms, **max 29ms**.

Documented but **not** fixed (see `docs/analytics-latency.md`):

1. The analytics refresh still runs on every ingest (~50% duty cycle at
   priority 4). Coalescing invalidations or a per-day rollup is the next win.
2. `deploy/macos/install-server.sh` pins the hub at `ProcessType=Background`.
   Moving the server to `ProcessType=Interactive` would remove the ~8–10x
   scheduling penalty for every request.
3. The dashboard pre-render race (occasional 0.7–0.9s warm `/`).
4. The quota-panel query path when `TOKEMON_QUOTAS` is configured.

## Tokedex MVP — complete, still gated

Roadmap items 1–4 are all merged and gated (`docs/tokedex-roadmap.md`):

1. **Pricing verification** — 24/35 models priced from fetched provider pages,
   97.3% of observed tokens. Haiku 5.5 (tiered by prompt length), DeepSeek
   (peak/off-peak), Gemini 3/3.7 Flash (different model IDs or unlisted),
   Nemotron (no per-token rate), Qwen 3.8 Flash Next, Codex Auto Review and
   the free-tier IDs stay `mode: unknown` on purpose.
2. **Tier legend** — one row per populated tier plus the provenance line.
3. **Sort / filter / search** — client-side, per-tier sorting, `role="status"`
   result count, no-match state, `<noscript>` note, axe-clean.
4. **Empty / unknown states** — "Rates not published yet", "No usage recorded
   yet", nothing-uncataloged note, empty-catalog help.

Known limitations: sorting is per tier (flattening would strip the tier
headings); the controls are inert without JS (full grid still renders);
uncataloged cards never match a provider filter because their provider is
genuinely unknown.

To flip the flag: `TOKEMON_ENABLE_TOKEDEX=true` in
`~/.config/tokemon/server.env`, restart, and verify `/tokedex` 200 plus the
nav link.

## Waiting on you

1. **Enable Tokedex?** The checklist's last box is the user reviewing the MVP
   and agreeing the page earns its nav slot. A preview with the flag on was
   left running at `http://127.0.0.1:18789/tokedex` (DB copy, safe to click;
   log in as `admin`).
2. **Pricing drift.** The verification pass left three pre-existing entries on
   their old numbers because the new ones are promotional or freshly cut:
   `gpt-5.6-luna` ($1/$6 catalog vs $0.20/$1.20 published), `gpt-5.6-terra`
   ($2.50/$15 vs $2/$12), `gpt-5.6-sol` ($5/$30 vs $4/$20, promotional through
   2026-11-30). Decide whether the catalog tracks current published rates.

## Team mechanics (read this before trying to spawn workers)

- **Herding is not running.** `herding team status` returns `not_running`
  (control socket refused). The board was read directly from
  `~/.local/state/herdr/gpui/team/team.sqlite3`; no task could be claimed or
  completed through the CLI.
- **zcode cannot run any model.** `zcode -p` fails with
  `Select a model before sending a message`, and the provider registry reports
  `providerCount: 0` with `entitled: false` on all ten providers
  (`account:zai-individual-coding-plan`, `account:zai-start-plan`, …). This is
  the same account/quota condition as `carteakey/herding#55`. It needs a
  `zcode login` / plan refresh by the user; it cannot be fixed headlessly.
- Workers for this batch ran as opencode subagents, one worktree each, and
  were gated at their exact commit before landing.

## How to operate

- Read `docs/ship-loop.md` and `AGENTS.md`. Gate =
  `gofmt -l .` clean + `go build ./...` + `go vet ./...` +
  `go test ./... -count=1` in a throwaway worktree at the exact commit; add
  `-race`, catalog validate, browser smoke, or `deploy/release-check.sh` for
  the affected surface.
- Workers never push; the supervisor gates and pushes. One worktree per
  worker. Nudge once, then reassign.
- Deploy: build `./cmd/tokemon`, install to `~/.local/bin/tokemon`, sign,
  restart launchd, verify `/healthz` + auth + the changed page.
- Worktrees and landed feature branches were cleaned up in this batch; only
  `main` remains locally.

## Open items / risks

1. **No release tag since v0.4.3.** `main` carries the Tokedex MVP, the
   analytics fix and the Go security bump. Cut `v0.4.4` with
   `bash deploy/release-check.sh` on the candidate commit before tagging.
2. **Tokedex is hidden** until the flag is flipped (see "Waiting on you").
3. **Quotas are off** (`TOKEMON_QUOTAS` unset).
4. **`mode: unknown` tail** is now small but real, and the three drifted
   OpenAI entries above are stale by choice.
5. **Session revocation** is still TTL/credential-rotation only.
6. **CI minutes:** the installer matrix now runs `ubuntu-latest`,
   `macos-15-intel` and `macos-15`; two macOS runners per push is the bulk of
   the cost if that ever matters.

## Reference artifacts

- Ship-loop playbook PR: `carteakey/herding#19`
- Workflow feedback issue: `carteakey/herding#20`
- Team-store hand-editing incident: `carteakey/herding#52`
- zcode quota/health false-positive: `carteakey/herding#55`
- Release: https://github.com/carteakey/tokemon/releases/tag/v0.4.3
- Analytics findings: `docs/analytics-latency.md`
- Tokedex roadmap: `docs/tokedex-roadmap.md`
