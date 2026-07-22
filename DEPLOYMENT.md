# Tokemon deployment

This guide covers the supported public deployment paths for Tokemon: a persistent dashboard hub and optional agents on other trusted machines.

Tokemon has two roles:

- **Hub:** stores usage metadata in SQLite, serves the dashboard, and accepts authenticated agent uploads.
- **Agent:** reads supported local provider metadata, keeps local sync state, and sends metadata-only batches to the hub.

Agents are outbound-only. The hub is the only component that needs a reachable port.

## Choose a setup

| Setup | Best for |
| --- | --- |
| Docker Compose hub | A persistent local or private-network dashboard |
| Native agent installer | Syncing a macOS or Linux machine to an existing hub |
| macOS LaunchAgent scripts | Running a locally built binary under launchd |

Keep the hub behind a private network such as Tailscale, WireGuard, or an authenticated reverse proxy. The dashboard does not provide user login.

## Requirements

For the hub, install Docker Engine or Docker Desktop with Compose support. For native agents, use macOS or Linux on arm64 or amd64. A Go installation is only needed when building from source.

## Run the hub with Docker Compose

From the repository root, set an ingest token and start the service:

```bash
export TOKEMON_INGEST_TOKEN="$(openssl rand -hex 32)"
export TOKEMON_UID="$(id -u)"
export TOKEMON_GID="$(id -g)"

docker compose -f deploy/docker-compose.yml up -d --build
curl --fail http://localhost:18787/healthz
```

Open [localhost:18787](http://localhost:18787) to view the dashboard. The default host port is `18787`; set `TOKEMON_PORT` before starting Compose to use another port. The container listens on port `8080` internally.

The SQLite database persists at `./data/tokemon.db`. The Compose service mounts the model catalog read-only, runs without root privileges by default, and keeps its root filesystem read-only. If the host data directory is owned by a different user, set `TOKEMON_UID` and `TOKEMON_GID` as shown above so SQLite can write its database and WAL files.

Useful service commands:

```bash
docker compose -f deploy/docker-compose.yml ps
docker compose -f deploy/docker-compose.yml logs --tail=100 tokemon
docker compose -f deploy/docker-compose.yml down
```

Stopping the service does not remove `./data`. Back up the database before making manual repairs or moving it to another host, and never replace it while Tokemon is running.

## Install an agent

Use the shared installer to install a checksum-verified release binary and a user-level supervisor:

```bash
bash deploy/install-agent.sh \
  --version 0.3.1 \
  --server https://tokemon.example.ts.net \
  --token 'replace-with-a-generated-secret' \
  --machine-id laptop \
  --adapters claude-code,codex
```

The installer selects the matching macOS or Linux architecture, writes a mode-`0600` configuration, preserves the local state database across upgrades, and installs launchd or systemd without requiring root. Use a private server base URL; the installer adds the API paths itself.

For a locally built binary, replace `--version 0.3.1` with `--binary ./tokemon`:

```bash
bash deploy/install-agent.sh \
  --binary ./tokemon \
  --server https://tokemon.example.ts.net \
  --token 'replace-with-a-generated-secret' \
  --adapters claude-code,codex
```

Use `--no-supervisor` when another process manager owns the agent. Remove the user-level service and its configuration with:

```bash
bash deploy/install-agent.sh --uninstall
```

The local cursor database is preserved by uninstall so reinstalling the agent does not require a full rescan.

## macOS-specific installation

For a locally built hub or agent managed by launchd, see the [macOS installer guide](deploy/macos/README.md).

The server installer creates a user-level `com.tokemon.server` LaunchAgent. The agent installer creates `com.tokemon.agent`. Both run as the logged-in user and keep secrets in mode-`0600` configuration files rather than in supervisor arguments.

## Agent configuration

The most common settings are:

```env
TOKEMON_SERVER_URL=https://tokemon.example.ts.net
TOKEMON_INGEST_TOKEN=replace-with-a-generated-secret
TOKEMON_MACHINE_ID=laptop
TOKEMON_SCAN_INTERVAL=1m
TOKEMON_ADAPTERS=claude-code,codex
TOKEMON_STATE=/Users/example/.local/share/tokemon/state.db
```

Explicit command-line flags override environment variables. The agent state contains cursors and sync metadata only. Provider files are read locally and are never uploaded as source content.

## Verify a deployment

After starting the hub:

1. Check that `/healthz` returns successfully.
2. Confirm the dashboard loads from the private hub address.
3. Run one agent sync and confirm that the machine appears in the dashboard.
4. Run the next sync and confirm that existing usage is refreshed rather than duplicated.

For a one-time local sync:

```bash
go run ./cmd/tokemon agent \
  --server https://tokemon.example.ts.net \
  --token 'replace-with-a-generated-secret' \
  --once
```

For a managed agent, inspect the native supervisor logs after installation. The agent reports source health and deployment metadata through its authenticated heartbeat without sending local paths or provider record content.

## Upgrade and stop

To rebuild the Compose hub from the current checkout:

```bash
docker compose -f deploy/docker-compose.yml up -d --build
```

To upgrade a native agent, rerun `deploy/install-agent.sh` with the new release version. The installer keeps the existing state database and configuration path.

To stop the Compose hub:

```bash
docker compose -f deploy/docker-compose.yml down
```

## Security and privacy

- Keep the hub on a private network or behind an authenticated proxy.
- Set a strong `TOKEMON_INGEST_TOKEN` for every multi-machine deployment.
- Store tokens in mode-`0600` files or a platform secret store; do not commit them or bake them into images.
- Run native agents as the local user and containers as non-root.
- Mount provider data read-only when using containers.
- Tokemon collects usage metadata only; prompts, responses, source code, conversation titles, and full repository paths are not part of the default event payload.
