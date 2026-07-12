# Tokemon deployment and agent plan

**Status:** v0.2 rollout plan
**Last updated:** 2026-07-12

This document records the deployment architecture and rollout contract for Tokemon. Linear remains the source of truth for committed work; this is an implementation and operations document, not a second backlog.

Current slice status: the shared agent config loader, macOS LaunchAgent installer, and Claude Code adapter are implemented and covered by tests. Release publishing, the Linux container template, and durable cursor state remain ahead.

## Decision

Use one Go binary and one normalized event contract, with a platform-appropriate supervisor:

| Platform | Distribution | Supervisor | Default source access |
| --- | --- | --- | --- |
| macOS | Homebrew or signed release archive | user LaunchAgent | `~/.claude/projects`, `~/.codex`, OpenCode data |
| Linux | Docker/Podman image | Compose, Quadlet, or systemd | `~/.claude/projects`, `~/.codex`, OpenCode data |
| Minimal/managed hosts | signed release archive | systemd or an existing orchestrator | explicit configured paths |

The server remains a separate deployment from the agents. It owns SQLite, ingestion authentication, analytics, and the dashboard. An agent only reads local usage metadata and makes outbound requests.

## Endpoint and configuration contract

The agent is configured with the server **base URL**, not the API route:

```text
TOKEMON_SERVER_URL=https://tokemon.example.ts.net
```

The agent posts normalized batches to:

```text
${TOKEMON_SERVER_URL}/api/v1/events/batch
```

The shared v0.2 ingest credential is configured separately:

```text
TOKEMON_INGEST_TOKEN=replace-with-a-generated-secret
```

The canonical file for managed installs is:

```text
~/.config/tokemon/agent.env
```

The final configuration surface is:

```env
TOKEMON_SERVER_URL=https://tokemon.example.ts.net
TOKEMON_INGEST_TOKEN=...
TOKEMON_MACHINE_ID=mac-mini
TOKEMON_SCAN_INTERVAL=1m
TOKEMON_HOME=/Users/example
```

Resolution order is explicit flags, environment variables, the config file, then safe defaults. Secrets must not be placed in process arguments or container image layers. Config files containing tokens are user-readable only (`0600`).

The agent supports `--config`, `--server`, `--token`, `--machine-id`, `--interval`, and `--home`, plus the corresponding `TOKEMON_*` environment variables. The macOS installer writes this file and launches the service with `--config`.

## macOS first

The first supported install path is a user-level LaunchAgent. It does not require root, Docker, or an inbound port.

Target layout:

```text
~/.local/bin/tokemon
~/.config/tokemon/agent.env       # mode 0600
~/Library/LaunchAgents/com.tokemon.agent.plist
~/Library/Logs/Tokemon/agent.log
```

The installer will:

1. Detect Apple Silicon versus Intel.
2. Install or update the signed Tokemon binary through Homebrew or a release archive.
3. Write the endpoint and token configuration.
4. Generate a user LaunchAgent with `RunAtLoad` and a one-minute interval.
5. Load the service with `launchctl` and run one immediate sync.
6. Report the service state and the server response.

The launchd process runs as the logged-in user, reads provider databases read-only, and writes only its own logs. It will not recursively scan the home directory.

## Linux and container path

Publish one multi-architecture OCI image, initially `linux/amd64` and `linux/arm64`. The image uses the existing `tokemon agent` command; a separate agent codebase is unnecessary.

The Compose template will:

- use `restart: unless-stopped`;
- run as the host user's UID/GID where practical;
- use a read-only root filesystem;
- drop Linux capabilities and set `no-new-privileges`;
- mount only explicit provider paths read-only;
- accept `TOKEMON_INGEST_TOKEN` from the deployment environment rather than an image layer;
- expose no inbound agent port;
- avoid host networking and the Docker socket because Tokemon does not collect host metrics.

The initial Linux mounts are:

```text
${HOME}/.claude/projects     → /agent-home/.claude/projects:ro
${HOME}/.codex               → /agent-home/.codex:ro
${HOME}/.local/share/opencode → /agent-home/.local/share/opencode:ro
```

The server can be kept running as a detached Compose service from the repository
root:

```bash
docker compose --env-file .env -f deploy/docker-compose.yml up -d --build
docker compose --env-file .env -f deploy/docker-compose.yml ps
curl http://127.0.0.1:18787/healthz
```

The service restarts unless explicitly stopped, and SQLite persists in
`./data/tokemon.db`. The default host port is `18787` and binds all host
interfaces for private-network access; the container still listens on 8080
internally. Set `TOKEMON_PORT` to change it, and set the required
`TOKEMON_INGEST_TOKEN` in a local `.env` file or deployment environment. Keep
the port behind Tailscale, a VPN, or an authenticated reverse proxy; the
dashboard itself has no user login. Deploying a new build on request is the
same `up -d --build` command; stop it with
`docker compose --env-file .env -f deploy/docker-compose.yml down`.

The agent runs with `--home /agent-home`, so provider discovery stays identical inside and outside the container. A read-only mount limits mutation, not visibility; a compromised container could still read mounted files. The explicit mount allowlist and metadata-only parser are therefore both required.

Standalone binaries plus systemd remain the fallback for machines without Docker or Podman. Ansible can install either path across a fleet later.

## Claude Code integration

Claude Code usage is stored in session JSONL files under:

```text
~/.claude/projects/<encoded-project>/<session-id>.jsonl
```

The adapter will stream those files and inspect only assistant records containing `message.usage`. It will extract:

- timestamp;
- session ID;
- model;
- input tokens;
- output tokens;
- cache-read tokens;
- cache-creation/write tokens.

It will not send message content, tool content, project names, encoded project paths, titles, or repository paths. File identity is hashed before it participates in an event ID or source identity. Missing token fields remain unknown; totals are derived only when the required components are present.

Each assistant API response becomes one deterministic usage event. Stable identity is based on the hashed transcript identity, line offset, timestamp, and session ID. This permits rescans without duplicate lifetime totals while the durable cursor state is completed.

No live Claude transcript fixtures exist on this Mac today, so the enabled adapter ships with synthetic privacy fixtures and an explicit inspect-equivalent payload test until a real Claude session is available for end-to-end verification.

## Security baseline

- Keep ingestion behind Tailscale/WireGuard or HTTPS; do not expose the raw HTTP port publicly.
- Require `TOKEMON_INGEST_TOKEN` on any multi-machine server. Do not deploy with `change-me`.
- Prefer one token per trusted deployment until per-agent credentials and revocation exist.
- Store tokens in `0600` files or the platform secret store; do not put them in command lines, images, or Git.
- Keep agents outbound-only. No inbound agent listener is required.
- Run native agents as the user and containers as non-root.
- Open provider data read-only and mount only known paths.
- Keep `inspect` and export behavior privacy-verifiable.
- Pin release checksums and container image digests for managed deployments.

## Verification gates

Every deployment path must verify:

1. The supervisor reports the agent running.
2. The agent can reach the configured server endpoint.
3. The server accepts a valid token and rejects an invalid one.
4. A second sync refreshes existing snapshots instead of adding duplicates.
5. The dashboard attributes usage to the new machine.
6. A captured normalized payload contains no prompt, response, title, source-code, or repository-path content.
7. The agent survives a restart and retries when the server is temporarily unavailable.

## Rollout order

1. Finish the shared config loader and macOS LaunchAgent installer.
2. Add the Claude Code adapter and privacy fixtures alongside the macOS path.
3. Add cross-provider ingestion tests and source-path redaction assertions.
4. Publish signed macOS binaries and a Homebrew tap.
5. Publish the multi-architecture agent image and Linux Compose template.
6. Add systemd/Quadlet and Ansible paths after the first macOS and Docker installs are verified.

Relevant committed work is tracked in the Tokemon v0.2 Linear milestone: CAR-63, CAR-64, CAR-65, CAR-67, and CAR-68.
