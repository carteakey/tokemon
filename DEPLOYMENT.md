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

## Back up and restore SQLite

Use the built-in backup command for a standalone, versioned snapshot:

```bash
tokemon backup create \
  --database ./data/tokemon.db \
  --destination /mnt/tokemon-off-host \
  --retention 10
tokemon backup verify /mnt/tokemon-off-host/tokemon-backup-v4-20260810T000000.000000000Z.db
```

`backup create` uses SQLite `VACUUM INTO`, so committed rows still in the
source WAL are included. It never copies `tokemon.db` or its `-wal`/`-shm`
sidecars. Files are named with the schema version and a UTC timestamp, and
only the newest `--retention` files for that backup destination are retained.
The destination should be an encrypted, mounted off-host volume or another
operator-managed replication target; a successful command exits zero and a
failed destination, integrity check, or equivalence check exits non-zero.

For a repeatable scheduled job, use the portable wrapper and make the
destination available before the scheduler runs it:

```bash
TOKEMON_DATABASE=/var/lib/tokemon/data/tokemon.db \
TOKEMON_BACKUP_DESTINATION=/mnt/tokemon-off-host \
TOKEMON_BACKUP_RETENTION=10 \
  deploy/tokemon-backup.sh
```

For example, a daily cron entry can call the same script (with absolute paths)
and alert on its exit status:

```cron
17 2 * * * TOKEMON_DATABASE=/var/lib/tokemon/data/tokemon.db TOKEMON_BACKUP_DESTINATION=/mnt/tokemon-off-host /var/lib/tokemon/deploy/tokemon-backup.sh >>/var/log/tokemon-backup.log 2>&1
```

Restore only while the service is stopped so the destination lock can be
acquired. Verify the source first, then require `--force` to replace an
existing database:

```bash
docker compose --env-file .env -f deploy/docker-compose.yml stop tokemon
tokemon backup verify /mnt/tokemon-off-host/tokemon-backup-v4-20260810T000000.000000000Z.db
tokemon backup restore \
  --source /mnt/tokemon-off-host/tokemon-backup-v4-20260810T000000.000000000Z.db \
  --database ./data/tokemon.db --force
docker compose --env-file .env -f deploy/docker-compose.yml up -d
curl --fail http://localhost:18787/healthz
```

Before a forced restore, Tokemon creates a verified
`data/backups/tokemon-before-restore-*.db` snapshot. CAR-79 migration snapshots
(`tokemon-v<old>-before-v<new>-*.db`) remain separate and are never pruned by
scheduled-backup retention. A restored older schema is migrated on the next
`serve`/CLI open, with the existing pre-migration snapshot and integrity check
preserved; this is the supported rollback path.

### Database process and retention policy

File-backed Tokemon opens hold an advisory `<database>.lock` for the lifetime
of the server or CLI process. `serve`, `import`, `export`, `purge`, and schema
migrations therefore serialize and deterministically reject a concurrent
database process. Stop the service before restore or other external file
replacement. `backup create` and `backup verify` are read-only SQLite
operations and can run while the server is serving.

`purge --before YYYY-MM-DD` interprets the boundary as midnight UTC and deletes
only events strictly before it. Evolution is derived from the remaining event
total, so deleting history can lower the displayed evolution stage; that is
expected and is covered by the restore/purge checks. Purging events does not
delete any SQLite or off-host backup, so retention cleanup remains an explicit
backup policy rather than an implicit data deletion.

## Install an agent

Use the shared installer to install a checksum-verified release binary and a user-level supervisor:

```bash
bash deploy/install-agent.sh \
  --version 0.3.2 \
  --server https://tokemon.example.ts.net \
  --token 'replace-with-a-generated-secret' \
  --machine-id laptop \
  --adapters claude-code,codex
```

The installer selects the matching macOS or Linux architecture, writes a mode-`0600` configuration, preserves the local state database across upgrades, and installs launchd or systemd without requiring root. Use a private server base URL; the installer adds the API paths itself.

For a locally built binary, replace `--version 0.3.2` with `--binary ./tokemon`:

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

Before each collection pass, the agent checks the hub's `/healthz` endpoint. If the hub is offline, the agent applies its bounded failure backoff without scanning provider histories; cursor state remains unchanged and collection resumes when the hub is healthy.

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
