# Tokemon

Tokemon is a small, self-hosted dashboard for AI coding usage. It stores usage metadata only and derives one global creature evolution from lifetime tokens.

The authoritative product direction is [tokemon-spec-v0.2.md](tokemon-spec-v0.2.md). [token-bandit-spec.md](token-bandit-spec.md) is retained as historical context; its name and casino theme are obsolete.

## Current slice

The repository now has a working Go foundation for:

- normalized schema-v1 JSONL events with validation and deterministic ID helpers;
- pure power-of-ten evolution calculations and the v0.2 form names;
- YAML model aliases and simple estimated API pricing;
- SQLite storage with idempotent ingestion, lifetime totals, export, and purge;
- shared-token batch ingestion and evolution/overview API endpoints;
- a deliberately small server-rendered overview page;
- `serve`, `import`, `export`, `inspect`, `discover`, and `purge` commands.

The native polling agent and Claude Code/Codex parsers are the next implementation slice. The `agent` command is intentionally explicit about that boundary instead of pretending those adapters exist.

## Run it

Requires Go 1.26 or newer.

```bash
go test ./...
go run ./cmd/tokemon serve --database ./data/tokemon.db
```

The server listens on `http://localhost:8080` by default. Set `TOKEMON_INGEST_TOKEN` or pass `--ingest-token` before exposing ingestion beyond a trusted local network.

Import normalized JSONL:

```bash
go run ./cmd/tokemon inspect examples/usage.jsonl
go run ./cmd/tokemon import --database ./data/tokemon.db examples/usage.jsonl
go run ./cmd/tokemon export --database ./data/tokemon.db exported.jsonl
```

The exact outgoing payload is what `inspect` prints. It never reads prompts, responses, source code, or repository paths.

## Normalized event

The generic JSONL escape hatch accepts one schema-v1 event per line. Token fields may be JSON `null`; unknown totals do not contribute to the lifetime counter.

```json
{"schema_version":"1","event_id":"sha256:...","timestamp":"2026-07-11T18:42:00Z","machine_id":"laptop","provider":"anthropic","model":"claude-opus-4","tool":"generic-jsonl","input_tokens":12000,"output_tokens":3400,"cache_read_tokens":null,"cache_write_tokens":null,"reasoning_tokens":null,"total_tokens":15400,"duration_ms":null,"cost":null,"currency":"USD","token_accuracy":"reported","source":{"adapter":"generic-jsonl","adapter_version":"1.0.0"}}
```

## Deployment

The initial Compose file is in [deploy/docker-compose.yml](deploy/docker-compose.yml). It builds the same binary used by the local commands and persists SQLite under `./data`.

## Design guardrails

- SQLite remains the source of stored usage.
- Evolution is derived state; there is no evolution table.
- Unknown values stay unknown rather than silently becoming zero.
- Prompt/response content is outside the normalized schema and is not collected by default.
- The dashboard is calm analytics with one original creature, not a game engine.
