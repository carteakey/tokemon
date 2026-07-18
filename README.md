# Tokemon

<img src="web/static/tokemon/token-dex.png" alt="Tokemon token-dex icon" width="96">

> A tiny, local-first token garden for your coding agents.

Tokemon reads usage metadata, stores it in SQLite, and grows one original little creature as your lifetime token count climbs. Feed it tokens. Watch it evolve.

Tokemon is an independent project made just for fun and is not affiliated with, endorsed by, or connected to Pokémon or The Pokémon Company.

No prompts, responses, source code, conversation titles, or repository paths are collected by default.
When a supported tool reports its working directory, Tokemon keeps only the lowercase final directory name as a project label. This lets matching projects merge across machines without sending full paths.

## Start

Requires Go 1.26+.

```bash
go run ./cmd/tokemon serve --database ./data/tokemon.db
```

Open [localhost:8080](http://localhost:8080), then sync the current machine:

```bash
go run ./cmd/tokemon agent --server http://127.0.0.1:8080 --once
```

Leave off `--once` to keep polling. The agent currently supports Claude Code transcript JSONL plus Codex, GitHub Copilot CLI, OpenCode, and Antigravity usage records. The Copilot adapter reads only durable per-model shutdown aggregates from `~/.copilot/session-state/*/events.jsonl`; it does not read prompts, responses, tool arguments, titles, or repository paths. The Antigravity adapter reads only generation metadata from `~/.gemini/antigravity-cli/conversations/*.db`; it does not read transcripts, prompts, responses, artifacts, or conversation titles. Set `TOKEMON_INGEST_TOKEN` before exposing the ingest endpoint beyond a trusted local network.

Unsupported tools can emit normalized schema-v1 events to an explicit JSONL path. Repeat `--jsonl` for multiple files or quote a glob so Tokemon, rather than the shell, discovers matching files:

```bash
go run ./cmd/tokemon agent --server http://127.0.0.1:8080 --jsonl '~/ai-usage/*.jsonl'
```

Generic JSONL parsing supports byte cursors and safe truncation/replacement resets. The native agent persists cursors and metadata fingerprints in `~/.local/share/tokemon/state.db`, advancing them only after a successful upload. Local paths are hashed before they can enter outgoing metadata.

Managed installs can use `~/.config/tokemon/agent.env`; explicit flags override environment variables, which override that file. See [macOS deployment](deploy/macos/README.md) for the LaunchAgent installer.

The overview includes a Pokédex-inspired, pixel-style activity field for the last 53 weeks plus lifetime usage cuts by project, harness, model, and machine. Harness usage groups the recorded tools, including Claude Code, Codex, GitHub Copilot CLI, OpenCode, and Antigravity. Hover or focus any past day to see its total plus model and provider token breakdowns; dates are grouped in UTC and unknown token totals remain visibly marked rather than being treated as zero.

Open `/analytics` for rolling 24-hour, 7-day, 30-day, 90-day, and all-time token trends; previous-window comparisons; filters by machine/provider/model/harness; breakdowns by project, harness, provider, model, or machine; and recent metadata-only sessions. The trend chart includes a compact token scale, readable timeline ticks, and a single peak marker. Every bucket remains keyboard-readable, quiet periods stay visible as zeroes, and the page includes an explicit **Export analytics JSON** action. The browser-facing page is separate from the machine-readable `/api/v1/analytics` endpoint; downloads use `/api/v1/analytics/export` and include the selected filters.

Use the small gear in the overview header to assign compact dashboard aliases for models and machines. Aliases affect presentation only; raw identities remain available in tooltips and the data view.

## Import or export

Normalized JSONL works too:

```bash
go run ./cmd/tokemon inspect examples/usage.jsonl
go run ./cmd/tokemon import --database ./data/tokemon.db examples/usage.jsonl
go run ./cmd/tokemon export --database ./data/tokemon.db exported.jsonl
```

## Develop

```bash
go test ./...
go run ./cmd/tokemon catalog validate --catalog catalog/models.yaml
```

`catalog/models.yaml` is the committed source of truth for API-equivalent pricing. Schema-v2 entries require an official HTTPS source, verification date, USD standard rates, and deterministic aliases. Unknown models remain unpriced; conflicting aliases or invalid pricing prevent the catalog from loading.

Read the [v0.2 product spec](tokemon-spec-v0.2.md), the [visual style guide](STYLE_GUIDE.md), or run `go run ./cmd/tokemon --help` for the full command list.

See [DEPLOYMENT.md](DEPLOYMENT.md) for the multi-machine roadmap, endpoint contract, macOS LaunchAgent path, Linux container path, Claude integration boundary, and security checklist.
