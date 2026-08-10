# Tokemon

<img src="web/static/tokemon/token-dex.png" alt="Tokemon token-dex icon" width="96">

> A local-first token garden for your coding agents.

Tokemon turns usage metadata into a small evolving creature and a calm dashboard for seeing where your tokens go. It runs locally, keeps its data in SQLite, and can collect from more than one machine.

Privacy is the point: Tokemon stores usage metadata only. It does not collect prompts, responses, source code, conversation titles, or full repository paths by default.

Tokemon is an independent project made just for fun and is not affiliated with, endorsed by, or connected to Pokémon or The Pokémon Company.

![Tokemon dashboard showing the evolving creature, lifetime token counter, activity field, and usage summaries using representative sample data](docs/tokemon-dashboard.png)

## Supported providers

Claude Code · Codex · GitHub Copilot CLI · OpenCode · Antigravity · OpenClaw · Hermes Agent

Tools without a native provider can use Tokemon's generic event import path.

## Run locally

Requires Go 1.26+.

Start the dashboard:

```bash
go run ./cmd/tokemon serve --addr 127.0.0.1:8080 --dev-loopback --database ./data/tokemon.db
```

`--dev-loopback` is an explicit local-development exception: it permits an
empty ingest token only when the server listens on loopback. A non-loopback
listener must set `TOKEMON_INGEST_TOKEN`; the server fails closed otherwise.

Open [localhost:8080](http://localhost:8080), then sync the current machine once:

```bash
go run ./cmd/tokemon agent --server http://127.0.0.1:8080 --once
```

Leave off `--once` to keep the agent polling. For a multi-machine hub, set a
strong `TOKEMON_INGEST_TOKEN`; the same secret (or
`TOKEMON_DASHBOARD_TOKEN`) is required for dashboard and read-API access via
Basic auth (`tokemon:<token>`) or Bearer auth. Keep the hub on Tailscale,
WireGuard, or an authenticated reverse proxy. Direct public-internet exposure
is unsupported.

The overview shows lifetime tokens, today/week context, token mix, a 53-week activity field, machine health, recent metadata-only sessions, and breakdowns by project, provider, model, and machine. Open `/analytics` for trends, comparisons, filters, selected-breakdown share over time, and export. `/tokedex` renders the bundled YAML model tiers, aliases, pricing provenance, and observed usage context; `/sessions` provides a filterable metadata-only session timeline.

Read APIs include `/api/v1/analytics/timeline`, `/api/v1/sessions` (plus `/api/v1/sessions/export`), `/api/v1/machines`, `/api/v1/catalog`, and `/api/v1/models`. Filters affect only the requested analytics/session view; lifetime evolution and the primary token counter remain global.

The agent rejects sensitive or unknown outbound fields before upload, and
`tokemon inspect` applies the same check locally. Project labels are normalized
basenames (never full paths); set `TOKEMON_INCLUDE_PROJECTS=false` or pass
`--include-projects=false` when a basename could disclose a sensitive project.
Use `TOKEMON_ADAPTERS` as a provider allowlist and configure generic JSONL only
through explicit paths.

## Deploy with Docker Compose

Compose runs Tokemon as a persistent dashboard hub with SQLite data stored under `./data`.

From the repository root:

```bash
export TOKEMON_INGEST_TOKEN="$(openssl rand -hex 32)"
export TOKEMON_UID="$(id -u)"
export TOKEMON_GID="$(id -g)"

docker compose -f deploy/docker-compose.yml up -d --build
curl --fail http://localhost:18787/healthz
```

Then open [localhost:18787](http://localhost:18787). To follow logs or stop the hub:

```bash
docker compose -f deploy/docker-compose.yml logs -f tokemon
docker compose -f deploy/docker-compose.yml down
```

The default port is `18787`; set `TOKEMON_PORT` to change it. Compose requires
`TOKEMON_INGEST_TOKEN` and binds the dashboard to a private network. A reverse
proxy may assert an authenticated identity with `X-Forwarded-User` only when
its source CIDR is configured in `TOKEMON_TRUSTED_PROXY_CIDRS`; strip that
header from untrusted requests. The server applies bounded read, header, write,
idle, and shutdown timeouts; override them with
`TOKEMON_SERVER_READ_TIMEOUT`, `TOKEMON_SERVER_READ_HEADER_TIMEOUT`,
`TOKEMON_SERVER_WRITE_TIMEOUT`, `TOKEMON_SERVER_IDLE_TIMEOUT`, and
`TOKEMON_SERVER_SHUTDOWN_TIMEOUT` (or the matching `serve` flags). `SIGTERM`
and `SIGINT` stop new work, drain in-flight requests for the bounded shutdown
window, and close SQLite before exit. `/healthz` performs a read-only SQLite
probe and returns `503` while the database is unavailable. Authenticated
`GET /metrics` exposes deterministic request, ingest, source-error, stale-agent,
and SQLite latency counters; it never includes event payloads or credentials.
See the [public deployment guide](DEPLOYMENT.md) for multi-machine agents,
macOS supervisors, and release installation.

For WAL-safe, versioned SQLite snapshots, run `tokemon backup create` with an
off-host destination (or schedule `deploy/tokemon-backup.sh`). See the
[backup and restore runbook](DEPLOYMENT.md#back-up-and-restore-sqlite) for
retention, integrity verification, and offline restore steps.

## Roadmap

- **Current:** reliable multi-machine collection, metadata-only privacy, analytics, and the evolving Tokemon dashboard.
- **Next:** broader provider fixtures, release verification, and a smoother install path.
- **Later:** signed macOS and Linux distribution, more providers, and deeper evolution art.

## Develop

```bash
go test ./...
go run ./cmd/tokemon catalog validate --catalog catalog/models.yaml
```

Run `go run ./cmd/tokemon --help` for the full command list.

## Documentation

- [v0.2 product spec](https://github.com/carteakey/tokemon/blob/main/docs/tokemon-spec-v0.2.md)
- [Historical token-bandit spec](https://github.com/carteakey/tokemon/blob/main/docs/token-bandit-spec.md)
- [Visual style guide](https://github.com/carteakey/tokemon/blob/main/STYLE_GUIDE.md)
- [Changelog](CHANGELOG.md)
- [Incident-response runbook](docs/incident-response.md)

## License

Tokemon is released under the [MIT License](LICENSE).
