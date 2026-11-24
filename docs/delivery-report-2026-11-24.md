# Tokemon Slices 1–4 Delivery, Audit & Antigravity Backfill Report (2026-11-24)

Summary of the 2026-11-23 / 2026-11-24 delivery batch in `/Users/kchauhan/repos/tokemon`, covering Slices 1–4, E2E and adversarial hardening, independent audit verification, live Mac hub deployment, and the follow-up Antigravity `0.3.0` `cache_read_tokens` / `reasoning_tokens` fix and backfill.

## 1. Landed Commits on Local `main` (Unpushed)

| Commit | Slice / Stage | Summary |
| --- | --- | --- |
| `a1e2bce` | M1 (Slice 1) | Group `machine_source_health` by adapter with collapsed stale/errored `<details>`, fix Analytics `MachineAliases` in breakdown & share legend, localize `<time datetime>` timestamps, pre-render hero `TokenComposition` + honor `autoPaused()` in `pollLifetime`, guard `migrate()` Codex dedup behind `currentVersion < databaseSchemaVersion`. |
| `1016ca1` | M2A (Slice 2) | Split SQLite write (`SetMaxOpenConns(1)`) and read-only (`?mode=ro`, `SetMaxOpenConns(4)`) pools + WAL PRAGMAs, incremental `Facets` cache, single-pass streaming `Analytics`, `Overview` `ByMachine` dedup + `LIMIT 8` sessions, composite session index, SWR + coalescing across evolution, quotas (`#38`), sessions, Tokedex, and insights. |
| `4aad6d4` | M2B (Slice 2) | Outbound gzip compression middleware (`Vary: Accept-Encoding`), immutable `?v=` `/static/` caching (`public, max-age=31536000, immutable`), lossless PNG optimization (`web/static/tokemon/*.png`), LCP `fetchpriority="high"`, and `content-visibility: auto; contain-intrinsic-size: auto none auto 320px;` on below-the-fold panels. |
| `b113605` | M3A (Slice 3) | Extract shared `web/static/tokemon/base.css` and `internal/api/templates/chrome.html` partials (`head`, `topbar`, `footer`) across all 7 templates (Epic `#22`), preserving CAR-205 (`0` `/insights` links in `.nav`). |
| `3b6567c` | M3B (Slice 3) | Clickable Overview/Analytics/heatmap/trend drill-downs, `/sessions` project filter + search + pagination + compact tokens, and metadata-only `/sessions/<id>` detail view. |
| `18f9c78` | M3C (Slice 3) | Compact Analytics filter toolbar with active pills + auto-submit, ETag-based in-place `/` updates replacing 60s full-page reloads, `/insights` 24h keyboard roving-tabindex readout, and `/settings` usage-sorted/filterable aliases with Reset & Tokedex links. |
| `7043384` | M4 (Slice 4A) | Codex `.jsonl.zst` archived-rollout ingestion (`github.com/klauspost/compress/zstd`), `claude-desktop` harness mapping, `tokemon status` CLI (`--short` / `--json`), and built-in daily WAL-safe automated backups (`retention = 10`, `24h` interval). |
| `fc344bd` | M5A (Slice 4B) | Schema v5 SQLite `quotas` table, Analytics cache savings & effective `$/1M` computation, Activity Streaks calculation, and Tokedex model-detail / 30d component queries. |
| `ebc2b23` | M5B (Slice 4B) | Analytics cache savings + custom `from`/`to` dates UI, `/settings` quotas CRUD + backup age + webhook/ntfy/local alerts, `/` budget pace bar, Tokedex `/tokedex/<model>` + 30d cost what-if comparison, and authenticated aggregate-only `/api/v1/card.svg` (`#21`). |
| `87b95e1` | M6 Phase 1 | Opaque-box 4-tier E2E suite in `internal/e2e/` (`//go:build e2e`, `TOKEMON_E2E=1`) with `365/365` initial pass across Tiers 1–4 and targeted bug fixes surfaced during verification. |
| `1067555` | M6 Phase 2 | 4 white-box adversarial Challenger cycles (fixing backup TOCTOU, UTF-8/TZ/SWR edge cases, and adding coverage tests), ending with clean Cycle 4 Challengers. |
| `498f82e` | Follow-up | Antigravity adapter `v0.3.0`: decode `cache_read_tokens` (`field 5`), `reasoning_tokens` (`field 9`), and actual output (`field 3`) across both step and legacy `gen_metadata` records; honor incremental step offsets for `v0.3.0` cursors; replace superseded pre-`sha256` UUID rows in `Store.Ingest` without double-counting. |

## 2. Verification & Audit Evidence

### Automated Gates (Slices 1–4 Audit at `1067555`)

- `gofmt -l .`: clean (empty output)
- `go build ./...`: PASS
- `go vet ./...` & `go vet -tags e2e ./internal/e2e`: PASS
- `go test ./... -count=1`: **25/25** packages PASS
- `go test ./internal/api -race -count=1`: PASS (`24.471s`, 0 races)
- `TOKEMON_BROWSER_SMOKE=1 go test ./internal/browsercheck -v -count=1`: PASS (`22.444s`, **73** PASS lines across all pages × 4 viewports, empty `axeAllowlist`)
- `TOKEMON_E2E=1 go test -tags e2e ./internal/e2e -v -count=1`: PASS (`73.472s`, **386** PASS lines = 368 top-level + 18 subtests, 0 FAIL, 0 SKIP)
- `tokemon art validate --dir web/static/tokemon`: PASS (19 assets, RGBA alpha verified)
- `tokemon catalog validate --catalog catalog/models.yaml`: PASS (schema 2, 46 models, 142 aliases)

### Follow-Up Gate (`498f82e`, Antigravity `0.3.0`)

- `gofmt -l .`, `go build ./...`, `go vet ./...`: PASS
- `go test ./... -count=1`: **25/25** packages PASS (including new `internal/adapters/antigravity` cache/reasoning/incremental-cursor tests and `internal/database` backfill/UUID-replacement tests)
- `go test ./internal/api -race -count=1`: PASS (`24.824s`, 0 races)

## 3. Antigravity `0.3.0` Discovery & Live Backfill Results

During post-run token analysis, inspecting Antigravity's raw conversation SQLite protobuf payloads (`steps.metadata` field `9` and legacy `gen_metadata` field `1.4`) revealed that:

- **Field `2`** holds **uncached** input tokens only.
- **Field `3`** holds total output tokens (`field 9` thinking/reasoning + `field 10` visible text, verified `f9 + f10 == f3` on `10,541/10,541` local step records).
- **Field `5`** holds **`cache_read_tokens`**, verified across consecutive steps (`step[n].f5 + step[n].f2 == step[n+1].f5`), which the `0.2.0` adapter ignored completely.

### Live Mac Hub Backfill (`Kartikeys-MacBook-Air`, `498f82e`)

Before deploying `498f82e`, a verified WAL-safe backup was created at `data/backups/tokemon-backup-v5-20261124T145615.214314000Z.db` (`194,115` events, `30,239,791,303` lifetime tokens). Restarting `com.tokemon.server` and `com.tokemon.agent` with `498f82e` automatically rotated Antigravity's cursor identity (`0.2.0` -> `0.3.0`) and backfilled all 327 local conversation DBs in ~15 seconds:

| Metric (`Kartikeys-MacBook-Air`, `adapter='antigravity'`) | Before (`0.2.0`) | After (`0.3.0`) | Delta |
| --- | ---: | ---: | ---: |
| Conversations (`sha256` / legacy `uuid`) | 301 / 26 | 327 / 1* | 25 legacy UUID sessions replaced cleanly |
| Events | 13,370 | 13,388 | 0 duplicates |
| Input tokens (uncached) | 133,330,197 | 133,439,528 | +0.11M (new turns) |
| **Cache read tokens** | **0** | **1,719,445,211 (1.72B)** | **+1.72B recovered** |
| Output tokens | 14,918,953 | 12,994,839 | -1.92M (fixed legacy field-1 ceiling over-count) |
| **Reasoning tokens** | **0** | **5,193,798 (5.19M)** | **+5.19M recovered** |
| **Total Antigravity tokens (this Mac)** | **148,249,150 (148.2M)** | **1,865,879,578 (1.87B)** | **+1.72B (12.6×)** |
| **Hub lifetime total (all machines)** | **30.24B** | **31.96B** | **+1.72B** |

*\*One historical 2-event UUID session (`93,496` tokens) whose local `.db` file is no longer on disk was preserved intact.*

### Remote Agents Pending Upgrade

When `kpc-cachy` (`43,856` Antigravity events), `Kartikeys-MacBook-Air-2` (`13,339` events), and `fx505` (`1,100` events) are updated to `498f82e+`, their agents will automatically rotate their Antigravity cursors and backfill their historical `cache_read_tokens` and `reasoning_tokens` on first sync without manual DB intervention.

## 4. Operational Notes & Teamwork Quota Post-Mortem

- **Live Mac hub state:** `~/.local/bin/tokemon` (`0.4.5-dev`, commit `498f82e`, adhoc-signed, `codesign -vv` valid), `TOKEMON_ENABLE_TOKEDEX=true` in `~/.config/tokemon/server.env`, `com.tokemon.server` healthy in ~1s (`GET /healthz` `200`, schema `user_version = 5`, gzip + immutable `/static/` verified), `com.tokemon.agent` active.
- **Backups retained in `data/backups/`:** Pre-deploy v4 snapshot, automatic v4→v5 migration snapshot, and pre-backfill v5 snapshot (`tokemon-backup-v5-20261124T145615.214314000Z.db`).
- **Multi-agent quota lesson learned:** Running all 4 slices plus 4-tier E2E and 4 adversarial cycles via multi-agent fan-out generated ~16,171 model requests (`8,293` Hydrogen generations across ~282 subagent sessions + `7,696` monitor calls) and ~1.23B total tokens (1.12B cached), tripping the request-based subscription quota window (`174` `429 RESOURCE_EXHAUSTED` responses). Prefer direct single-agent or tightly scoped execution for incremental changes going forward.
