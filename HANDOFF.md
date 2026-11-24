# Tokemon handoff — 2026-11-24

Updated after landing Slices 1–4, E2E and adversarial hardening, live Mac hub deployment, and the Antigravity `0.3.0` cache-read/reasoning fix and backfill.

## TL;DR

- **`v0.5.0` tagged and pushed to `origin/main`.** Full release notes live under `## 0.5.0` in `CHANGELOG.md`.
- **Live Mac hub:** `~/.local/bin/tokemon` (`0.5.0`, adhoc-signed), `com.tokemon.server` healthy (`GET /healthz` `200`, ~1s boot), `com.tokemon.agent` active, SQLite schema `user_version = 5`, `TOKEMON_ENABLE_TOKEDEX=true` enabled in `~/.config/tokemon/server.env`.
- **Antigravity backfill complete on this Mac (`Kartikeys-MacBook-Air`):** `v0.3.0` recovered **1.72B** `cache_read_tokens` and **5.19M** `reasoning_tokens` across 327 local conversations (hub lifetime total `30.24B` → `31.96B`, zero duplicates).
- **Automated gates verified:** `gofmt`, `go build`, `go vet`, `go test ./... -count=1` (`25/25`), `go test ./internal/api -race -count=1`, `TOKEMON_BROWSER_SMOKE=1 go test ./internal/browsercheck -v -count=1` (`73` checks, empty axe allowlist), and `TOKEMON_E2E=1 go test -tags e2e ./internal/e2e -v -count=1` (`386` checks).

## What Shipped (Slices 1–4 + Antigravity `0.3.0`)

See `docs/improvement-plan-2026-11-23.md` and `docs/delivery-report-2026-11-24.md` for full details and audit evidence.

| Commit(s) | Area |
| --- | --- |
| `a1e2bce` | Slice 1: adapter-grouped `machine_source_health` with `<details>`, Analytics `MachineAliases` fix, localized `<time datetime>`, pre-rendered hero composition + pause guard, `migrate()` Codex dedup guard |
| `1016ca1`, `4aad6d4` | Slice 2: SQLite read/write pool split + WAL PRAGMAs, incremental `Facets`, single-pass `Analytics`, `Overview` dedup + `LIMIT 8`, SWR caches (`#38` quotas, evolution, sessions, Tokedex, insights), gzip middleware, immutable `?v=` `/static/`, lossless PNG crush, LCP `fetchpriority` + `content-visibility` |
| `b113605`, `3b6567c`, `18f9c78` | Slice 3: shared `base.css` + `chrome.html` partials (`#22`), clickable drill-downs, compact Analytics filter bar + pills, ETag in-place `/` refresh, `/sessions` search/project filter/pagination + `/sessions/<id>`, `/insights` 24h keyboard readout, `/settings` alias UX |
| `7043384`, `fc344bd`, `ebc2b23` | Slice 4: Codex `.jsonl.zst`, `claude-desktop` harness, `tokemon status` CLI, daily WAL-safe backups + `/settings` age, Analytics cache savings/`$/1M` + `from`/`to` dates, SQLite `quotas` v5 + `/settings` CRUD + `/` pace bar + alerts, Tokedex `/tokedex/<model>` + 30d what-if, Activity Streaks, authenticated `/api/v1/card.svg` (`#21`) |
| `87b95e1`, `1067555` | Opaque-box 4-tier E2E suite (`internal/e2e/`, `//go:build e2e`) + 4 adversarial Challenger hardening cycles |
| `498f82e` | Antigravity `0.3.0`: decode `cache_read_tokens` (`field 5`), `reasoning_tokens` (`field 9`), and real output (`field 3`); incremental step cursors; legacy UUID row replacement in `Store.Ingest` |

## Live Hub

- Service: launchd `com.tokemon.server` + `com.tokemon.agent`, binary `~/.local/bin/tokemon` (`0.4.5-dev`, `498f82e`), config `~/.config/tokemon/server.env` (`TOKEMON_ENABLE_TOKEDEX=true`), DB `~/repos/tokemon/data/tokemon.db` (`user_version = 5`), catalog `~/Library/Application Support/Tokemon/models.yaml`.
- Backups preserved in `~/repos/tokemon/data/backups/` (pre-deploy v4, pre-v5 migration, and pre-backfill v5 snapshots).
- **Restart gotcha:** after replacing `~/.local/bin/tokemon`, run `xattr -d com.apple.provenance ~/.local/bin/tokemon` and `codesign --force --sign - ~/.local/bin/tokemon`, then `launchctl kickstart -k gui/$(id -u)/com.tokemon.server` and `com.tokemon.agent`.
- **CAR-205 invariant:** `/insights` stays reachable behind dashboard auth and is deliberately **not** linked from primary navigation.

## Open Follow-Ups

1. **Upgrade remote agents (`kpc-cachy`, `Kartikeys-MacBook-Air-2`, `fx505`, and `raspberrypi` when online)** to `v0.5.0` so they pick up Codex `.jsonl.zst`, `claude-desktop` separation, and automatic Antigravity `0.3.0` `cache_read_tokens` / `reasoning_tokens` backfill.
2. **Pricing drift:** `gpt-5.6-luna`, `gpt-5.6-terra`, `gpt-5.6-sol` remain on old catalog numbers by choice; Gemini 4.x models stay `mode: unknown` until published API rates exist.

## Reference Artifacts

- Improvement plan: `docs/improvement-plan-2026-11-23.md`
- Delivery, audit & backfill report: `docs/delivery-report-2026-11-24.md`
- Analytics findings: `docs/analytics-latency.md`
- Data audit: `docs/data-audit-2026-11-19.md`
- Tokedex roadmap: `docs/tokedex-roadmap.md`
- Latest release: <https://github.com/carteakey/tokemon/releases/tag/v0.5.0>
