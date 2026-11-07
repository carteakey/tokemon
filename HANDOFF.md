# Tokemon handoff — 2026-11-07

Written by supervisor23 at the end of the 2026-11-06/07 batch. Everything
below is verified against the live hub and `origin/main`.

## TL;DR

- `origin/main = f5bac22` (docs: save the multi-agent ship loop), clean.
- **v0.4.2 is published**: https://github.com/carteakey/tokemon/releases/tag/v0.4.2
  (tag at `19db504`, 21 assets, signed, SBOM, image scan 0 HIGH/0 CRITICAL).
- The live Mac hub runs the current binary with password auth:
  `admin` / the password in `~/.config/tokemon/server.env`.
- 18 commits landed after the v0.4.2 tag; they are **unreleased** (no
  `## Unreleased` CHANGELOG section yet — task `ef35bc54`).
- Repo is clean: one local branch (`main`), one remote branch (`origin/main`),
  one worktree. 21 deleted branches are backed up under
  `refs/backup/cleanup-20261107/`.
- Workflow saved in `docs/ship-loop.md`, linked from `AGENTS.md`.

## What shipped (post-0.4.2, on main)

| Area | Commit(s) |
| --- | --- |
| Perf: analytics facets split, filter cache, SQL previous window | `c2b0944` |
| Perf: overview stale-while-revalidate (no more 6s spikes) | `6c80ef0` |
| Perf: SQLite handle cache cleanup, debug logs, Close | `2cfb79c` |
| Fix: shared cache computes survive client cancel (no 500 "context canceled") | `e455618`, `c05359c` |
| Feature: per-source connector health (heartbeat `sources[]`, machine panel) | `00b8183` |
| Feature: analytics Insights strip (burn rate, top mover, cache, coverage) | `d425587` |
| Catalog: 23 entries, `mode: unknown` pricing, honest coverage accounting | `c383b69`, `6002b02` |
| UI: dashboard polish (chart role, copy-ID, catalog help) | `22688b1` |
| Auth: constant-time username, 72-byte passwd guard, login throttle | `94a689b` |
| CI: staticcheck/actionlint/markdownlint/tidy gates | `648929b` |
| CI: browser smoke + axe a11y gate (`internal/browsercheck`) | `b94f7d5` |
| Release: image version assertion + pre-tag dry-run gate | `21632f9`, `b2c4fef` |
| Docs: per-source health, spec amendments, CHANGELOG batch | `c27cdaf`, `995eacd`, `db2d6a4` |

## Live hub

- Service: launchd `com.tokemon.server`, binary `~/.local/bin/tokemon`,
  config `~/.config/tokemon/server.env`, DB `~/repos/tokemon/data/tokemon.db`
  (88MB, 118k events), catalog `~/Library/Application Support/Tokemon/models.yaml`.
- Auth: password account enabled (`TOKEMON_DASHBOARD_USERNAME` +
  `TOKEMON_DASHBOARD_PASSWORD_HASH`); dashboard token fallback still works.
- Timings (warm): `/analytics` ~0.1s, `/sessions` ~0.4–0.9s, `/tokedex` ~0.4s,
  `/` ~1.8s.
- **Restart gotcha:** after replacing the binary, macOS kills the unsigned copy
  (`rc=137`). Run `xattr -d com.apple.provenance ~/.local/bin/tokemon` and
  `codesign --force --sign - ~/.local/bin/tokemon`, then
  `launchctl kickstart -k gui/$(id -u)/com.tokemon.server`. Startup takes
  ~15–20s before `/healthz` answers.

## Open work (team board + tokemon issues)

Team board (open): `ef35bc54` CHANGELOG, `95e8f907` browser smoke for source
health, `667b230e` meta-refresh control, `f8faad14` login form, `d48acd20`
quota chart (in progress), `328fb6d9` Tokedex flag/roadmap, `66306d46`
overview SWR (done — close it), `1814245f` dashboard render 1.8s, plus the
new analytics-SWR task.

GitHub issues in `carteakey/tokemon`: #23–#33 mirror the above (release
assertions, pre-tag dry run, meta-refresh, login form, browser smoke, page
perf, analytics insights, Tokedex flag, quota chart, catalog pricing,
overview SWR).

## Known issues / risks

1. **Analytics is still cold after every ingest** (~6.6s first request, then
   0.1s). The overview got stale-while-revalidate (`6c80ef0`); analytics did
   not. New task queued. This is the biggest remaining UX issue.
2. **`/` is ~1.8s warm**: `MachinesAt` lifetime GROUP BY ~0.54s + ~1.2s in the
   214KB html/template render. Task `1814245f`.
3. **v0.4.2 image reports `version: dev`** — the workflow fix landed after the
   tag. Code/tag/signatures are correct; cosmetic. Decide whether to re-cut
   v0.4.2 or let v0.4.3 carry it.
4. **Catalog deployment path**: the hub loads
   `~/Library/Application Support/Tokemon/models.yaml`, not the repo copy.
   After changing `catalog/models.yaml`, copy it there and restart. The
   current live file is the filled catalog (35 models, 93 names).
5. **`mode: unknown` pricing** is honest but most new entries are unpriced;
   real rates need verification against provider pricing pages.
6. **Startup is slow** (~15–20s) after a restart; health checks should retry.

## Decisions pending

- Re-cut v0.4.2 for the image version, or ship v0.4.3 later? (Recommend
  v0.4.3, since the code/tag/signatures are correct.)
- Quota chart MVP: approve the config-defined quota design (`d48acd20`).
- Tokedex: keep it hidden behind `TOKEMON_ENABLE_TOKEDEX` until the roadmap
  is done (`328fb6d9`).

## How to operate

- Read `docs/ship-loop.md` and `AGENTS.md`. Gate = `gofmt -l` clean +
  `go build ./...` + `go vet ./...` + `go test ./... -count=1` in a throwaway
  worktree at the exact commit; add `-race`, catalog validate, browser smoke,
  or `deploy/release-check.sh` for the affected surface.
- Workers never push; the supervisor gates and pushes. One worktree per
  worker. Nudge once, then reassign.
- Deploy: build `./cmd/tokemon`, install to `~/.local/bin/tokemon`, sign,
  restart launchd, verify `/healthz` + auth + the changed page.

## Reference artifacts

- Ship-loop playbook PR: `carteakey/herding#19`
- Workflow feedback issue: `carteakey/herding#20`
- Release: https://github.com/carteakey/tokemon/releases/tag/v0.4.2
