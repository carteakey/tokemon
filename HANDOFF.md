# Tokemon handoff — 2026-11-11

Written by supervisor-oc (opencode) after taking the team over from
supervisor23 and cutting v0.4.3. Everything below is verified against the
live hub and `origin/main`.

## TL;DR

- `origin/main = 2ac1021` (release: prepare v0.4.3), clean.
- **v0.4.3 is published**: https://github.com/carteakey/tokemon/releases/tag/v0.4.3
  (annotated tag on `2ac1021`; release workflow `42934366817`).
- The live Mac hub now runs the **v0.4.3** binary (`version: 0.4.3`,
  `commit: 2ac1021…`) with the **password login form** enabled (`admin` /
  the password in `~/.config/tokemon/server.env`). Tokedex is hidden
  (`TOKEMON_ENABLE_TOKEDEX` unset); quotas are off (`TOKEMON_QUOTAS` unset).
- The release gate passed on the exact tagged commit in a clean worktree
  (browser smoke, image identity, trivy HIGH/CRITICAL, full `-race`), and the
  published assets were verified independently (checksums, cosign
  signatures, SBOM, image digest + reported version).
- Two durable tasks remain: the Tokedex MVP slice (in progress) and a new
  analytics-performance investigation. The v0.4.3 cut and the notes draft
  are done.
- **Team mechanics caveat:** the previous supervisor and workers were
  registered to `wD` panes that no longer existed, so the team could not be
  woken. The takeover, and the re-pointing of worker panes, were done by
  hand-editing the team store; see `carteakey/herding#52`. The zcode workers
  later died on a provider quota (`exceed quota limit`, code 1005) while
  `herding team heartbeat` still reported them `HEALTHY`; see
  `carteakey/herding#55`.

## What shipped (post-0.4.2, on main)

| Area | Commit(s) |
| --- | --- |
| Perf: analytics facets split, filter cache, SQL previous window | `c2b0944` |
| Perf: overview stale-while-revalidate | `6c80ef0` |
| Perf: analytics stale-while-revalidate | `e493f57` |
| Perf: pre-rendered dashboard page, machines folded into overview | `40678e8` |
| Perf: analytics cache bounded at 64 filters (LRU) | `e569136` |
| Perf: startup warm-up (overview first, then analytics; before listen) | `4d8d704`, `1b5389e`, `4383771` |
| Fix: shared cache computes survive client cancel; no 500 "context canceled" | `e455618`, `c05359c` |
| Feature: per-source connector health + browser coverage | `00b8183`, `40bfd37` |
| Feature: analytics Insights strip | `d425587` |
| Feature: usage-vs-quota panel + `/api/v1/quotas` | `a20f053` |
| Feature: pause/refresh control replaces the 60s meta refresh | `d3db041` |
| Feature: real login form + signed session cookies | `78418fd`, `0aa0e25`, `5c2aafc`, `605dd7b` |
| Catalog: 23 entries, `mode: unknown` pricing, honest coverage | `c383b69`, `6002b02` |
| Tokedex: gated behind `TOKEMON_ENABLE_TOKEDEX` + roadmap doc | `44abc99` |
| Auth: constant-time username, 72-byte guard, login throttle | `94a689b` |
| CI: staticcheck/actionlint/markdownlint/tidy; browser smoke + axe | `648929b`, `b94f7d5` |
| Release: image version bake/assertion; pre-tag dry run | `02d4305`, `21632f9`, `b2c4fef` |
| Docs: per-source health, CHANGELOG, spec amendments | `c27cdaf`, `1a1c8b2`, `995eacd` |

## Live hub

- Service: launchd `com.tokemon.server`, binary `~/.local/bin/tokemon`
  (`0.4.3`, `2ac1021`), config `~/.config/tokemon/server.env`, DB
  `~/repos/tokemon/data/tokemon.db` (~119k events), catalog
  `~/Library/Application Support/Tokemon/models.yaml`.
- Auth: password account enabled (`TOKEMON_DASHBOARD_USERNAME` +
  `TOKEMON_DASHBOARD_PASSWORD_HASH`). The dashboard token fallback still
  works with Basic/Bearer (the ingest token is the fallback).
- Login flow (live-verified): anonymous HTML `/` → `303 /login?next=/`;
  `/login` 200; wrong password `401` + `role=alert`; correct password →
  `303` to `next` + `tokemon_session` cookie; logout → `303 /login` and
  cookie cleared; Basic/Bearer unchanged; `/tokedex` 404; `/api/v1/quotas`
  200; logout without CSRF 403.
- Timings (v0.4.3, live ingest on): warm `/` ~1.7ms. Warm `/analytics` is
  **not** the 15–30ms the previous handoff recorded — five consecutive warm
  requests measured 3.97s / 0.16s / 0.33s / 2.35s / 1.18s. The agent is
  ingesting ~300–400 events/hour and each ingest appears to invalidate the
  analytics cache, so a request that lands during or just after a recompute
  pays for it. Tracked as board task `039bbe0f`. Cold restart: `healthz`
  ~13–14s (environmental: launchd/Gatekeeper/cold page cache), first `/`
  ~4s and first `/analytics` ~4–8s while the warm compute is in flight.
- **Restart gotcha:** after replacing the binary, macOS kills the unsigned
  copy (`rc=137`). Run `xattr -d com.apple.provenance ~/.local/bin/tokemon`
  and `codesign --force --sign - ~/.local/bin/tokemon`, then
  `launchctl kickstart -k gui/$(id -u)/com.tokemon.server`. Startup takes
  ~13s before `/healthz` answers.

## Login form / session notes

- Routes: `GET /login`, `POST /login`, `POST /logout`; all 404 when no
  password account is configured.
- Cookie: `tokemon_session`, HttpOnly, SameSite=Lax, Path=/, `Secure` on TLS
  or trusted-proxy-asserted HTTPS, 12h `Max-Age` (no `Expires`), stateless
  HMAC token (`v1|expiry|nonce|fingerprint|signature`).
- Key: HMAC-SHA256 domain-separated from the dashboard token; random
  32-byte per-process key when no token exists (sessions reset on restart).
  The bcrypt hash is only an 8-byte one-way fingerprint inside the signed
  payload, never key material.
- Throttle: shared with Basic/Bearer, checked before bcrypt; only wrong
  Basic/Bearer and wrong `POST /login` credentials consume the budget.
  Expired/forged cookies clear and redirect/401 with zero failures.
- CSRF/origin: same-origin `Origin` or `Referer` required on login/logout;
  the static token is defense-in-depth. `next` is `url.Parse`-validated and
  rejects `//`, `/\`, decoded variants, schemes/hosts, control chars, and
  overlong values.
- `Cache-Control: no-store` on authenticated responses and login surfaces.
- Trade-off: no server-side revocation; a copied cookie is valid until TTL
  or credential rotation. Logout is client-side cookie clearing.
- Token-only deployments keep the `401 + WWW-Authenticate: Basic` challenge
  for anonymous HTML (the redirect only fires when a password account is
  configured).

## Open work (Herdr board)

- `eaca1130` **Tokedex roadmap MVP** — tier explanations + empty/unknown
  states (items 2 and 4 of `docs/tokedex-roadmap.md`). Tokedex stays gated
  off until the MVP set is merged and reviewed. In progress on `wG:p6`.
- `039bbe0f` **Perf: warm `/analytics` is 2-4s under live ingest** — measured
  on the live hub after the v0.4.3 deploy: five consecutive warm requests
  returned 3.97s / 0.16s / 0.33s / 2.35s / 1.18s while warm `/` returned
  1.7ms. Either the documented 15–30ms was measured between ingests, or the
  stale-while-revalidate path does not hold while the agent ingests
  (~300–400 events/hour). Unassigned.
- `cfd49e95` **Release v0.4.3** — done. `e1190139` **notes draft** — done
  (its content was folded into the `## 0.4.3` CHANGELOG section, which had
  been missing the login-form and cold-start entries).
- `8b62af88` **Review the v0.4.3 candidate** — was in progress when the zcode
  workers died on quota; re-dispatched on opencode, review range is now
  `4383771..2ac1021`.

GitHub issues #23–#33 mirror the older board items; several are now shipped
(release assertions, pre-tag dry run, meta-refresh, browser smoke, page
perf, analytics insights, Tokedex flag, catalog pricing, overview SWR).

## Known issues / risks

1. **Cold start is still ~4s for the first page** on a real restart: the
   warm-up cannot beat the cold compute; a request arriving during the
   in-flight compute joins it. The 13s to `healthz` is environmental, not
   this code path.
2. **Tokedex is hidden** (`TOKEMON_ENABLE_TOKEDEX` unset). Enable only
   after the MVP set in `docs/tokedex-roadmap.md` lands.
3. **Quotas are off** (`TOKEMON_QUOTAS` unset). Point it at a `quotas.yaml`
   to enable the panel; no file means no behavior change.
4. **Catalog deployment path**: the hub loads
   `~/Library/Application Support/Tokemon/models.yaml`, not the repo copy.
   Copy the file and restart after changes.
5. **`mode: unknown` pricing** is honest but most new entries are unpriced;
   real rates need verification against provider pricing pages.
6. **Session revocation** is TTL/credential-rotation only; there is no
   server-side denylist. Documented trade-off.
7. **No-token deployments** use a random per-process session key, so
   sessions reset on restart (documented in DEPLOYMENT).
8. **Worktrees/branches**: the main checkout plus five worker worktrees
   (`tokemon-worker3-health`, `tokemon-worker3-quota`,
   `tokemon-wt-analytics-swr`, `tokemon-wt-dashboard-perf`,
   `tokemon-wt-quota-chart`) and several landed feature branches remain.
   Clean them up after confirming the work is landed; keep `main`.

## Release v0.4.3 checklist — DONE

1. `bash deploy/release-check.sh` on `2ac1021` (browser smoke, image
   identity, trivy, race) — PASS. `deploy/release-guard.sh --tracked` and
   `--context .` both clean.
2. `## Unreleased` promoted to `## 0.4.3`, with the login-form and
   cold-start entries folded in from the notes draft (the section predated
   both).
3. Tag `v0.4.3` (annotated) pushed with `main`; Release workflow published
   signed assets.
4. Verified independently: all four tarballs + `sbom.spdx.json` match
   `checksums.txt`; `cosign verify-blob` OK for `checksums.txt`,
   `image-digest.txt`, `sbom.spdx.json` and the linux/amd64 tarball against
   identity `https://github.com/carteakey/tokemon/`; image
   `ghcr.io/carteakey/tokemon:0.4.3` pulls to `sha256:149df131…` (matching
   `image-digest.txt`) and reports `version: 0.4.3`.
5. Deployed to the hub and verified: `/healthz` 200 after 14s, anonymous
   HTML `/` → `303 /login?next=%2F`, `/login` 200, Bearer and Basic 200,
   `/tokedex` 404, `/api/v1/quotas` 200, warm `/` 1.7ms.

**Open:** CI on `main` has not completed a single run since 2026-11-07 —
every run is cancelled ~2 minutes in, including a manual re-run with no
concurrent push. There is no `concurrency` block in any workflow, so the
cancellation is external to the repo config. Gate on local runs until that
is explained.

## How to operate

- Read `docs/ship-loop.md` and `AGENTS.md`. Gate = `gofmt -l` clean +
  `go build ./...` + `go vet ./...` + `go test ./... -count=1` in a
  throwaway worktree at the exact commit; add `-race`, catalog validate,
  browser smoke, or `deploy/release-check.sh` for the affected surface.
- Workers never push; the supervisor gates and pushes. One worktree per
  worker. Nudge once, then reassign.
- Deploy: build `./cmd/tokemon`, install to `~/.local/bin/tokemon`, sign,
  restart launchd, verify `/healthz` + auth + the changed page. For the
  login form, also verify the anonymous redirect, the login page, and the
  token/Basic path.

## Reference artifacts

- Ship-loop playbook PR: `carteakey/herding#19`
- Workflow feedback issue: `carteakey/herding#20`
- Release: https://github.com/carteakey/tokemon/releases/tag/v0.4.2
- v0.4.3 notes draft: `/Users/kchauhan/repos/release-notes-v0.4.3-draft.md`
- Tokedex roadmap: `docs/tokedex-roadmap.md`
