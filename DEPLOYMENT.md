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

Keep the hub behind a private network such as Tailscale or WireGuard, or an
authenticated reverse proxy. The ingest token is required for every
non-loopback deployment. Dashboard and read APIs require Basic auth
(`tokemon:<token>`) or a Bearer token; set `TOKEMON_DASHBOARD_TOKEN` for a
separate credential. Direct public-internet exposure remains unsupported.

For a supported containerized agent with read-only provider mounts, see
[`deploy/agent-container.md`](deploy/agent-container.md) and its Compose
example. The hub Compose service remains the default deployment for the
dashboard.

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
# Authenticated dashboard smoke check (do not print the token):
curl --fail -u "tokemon:${TOKEMON_INGEST_TOKEN}" http://localhost:18787/
# Authenticated operational counters (metadata only):
curl --fail -u "tokemon:${TOKEMON_INGEST_TOKEN}" http://localhost:18787/metrics
```

Open [localhost:18787](http://localhost:18787) to view the dashboard. The default host port is `18787`; set `TOKEMON_PORT` before starting Compose to use another port. The container listens on port `8080` internally.

The server uses bounded HTTP read, header, write, idle, and graceful-shutdown
timeouts. Set `TOKEMON_SERVER_READ_TIMEOUT`,
`TOKEMON_SERVER_READ_HEADER_TIMEOUT`, `TOKEMON_SERVER_WRITE_TIMEOUT`,
`TOKEMON_SERVER_IDLE_TIMEOUT`, or `TOKEMON_SERVER_SHUTDOWN_TIMEOUT` in the
Compose environment when the defaults do not fit a trusted proxy. `SIGTERM`
and `SIGINT` stop accepting new requests, drain in-flight work for the bounded
shutdown window, and close SQLite. `/healthz` is an unauthenticated readiness
probe backed by `SELECT 1`: it returns `200` only when SQLite is available and
`503` otherwise. `GET /metrics` requires dashboard authentication and reports
process-local request/ingest/source-error counters plus SQLite-derived stale
agents and DB latency; logs are JSON and omit credentials, query strings,
request bodies, and local paths.

The SQLite database persists at `./data/tokemon.db`. The Compose service mounts the model catalog read-only, runs without root privileges by default, and keeps its root filesystem read-only. If the host data directory is owned by a different user, set `TOKEMON_UID` and `TOKEMON_GID` as shown above so SQLite can write its database and WAL files.

Useful service commands:

```bash
docker compose -f deploy/docker-compose.yml ps
docker compose -f deploy/docker-compose.yml logs --tail=100 tokemon
docker compose -f deploy/docker-compose.yml down
```

## Release integrity and safe upgrades

Release artifacts are four architecture-specific archives (macOS/Linux,
amd64/arm64), `checksums.txt`, and an SPDX JSON SBOM. The release workflow
signs the checksums, every archive, and the SBOM with Cosign's keyless GitHub
Actions identity. No private signing key is stored in this repository. The
installer requires Cosign, verifies the certificate identity
`https://github.com/carteakey/tokemon/.github/workflows/release.yml@refs/tags/v<VERSION>`
and the Sigstore OIDC issuer, then checks the archive hash and embedded
`tokemon version` before installation. Missing signatures, a changed archive,
or a mismatched version is a hard failure.

For an immutable GHCR deployment, use the release overlay with the digest
recorded by CI (there is deliberately no `latest` tag):

```bash
export TOKEMON_VERSION=0.3.3
export TOKEMON_IMAGE_DIGEST=sha256:<digest-from-release-image-digest.txt>
docker compose --env-file .env \
  -f deploy/docker-compose.yml -f deploy/docker-compose.release.yml \
  config --quiet
docker compose --env-file .env \
  -f deploy/docker-compose.yml -f deploy/docker-compose.release.yml \
  up -d --no-build
```

Before replacing a running hub, run a read-only release preflight: confirm the
database begins with the SQLite header, `PRAGMA integrity_check` is `ok`, the
schema is supported, and event/token counts are recorded. Create and verify a
WAL-safe off-host snapshot immediately before recreation. If any check fails,
do not run Compose; stop only the Tokemon service and escalate with the
preserved database and snapshot hashes. After start, repeat the integrity,
schema, count, `/healthz`, authenticated dashboard, and metrics checks. A
post-start mismatch is a failed deployment and must not be hidden by an
automatic restart loop.

The portable guards implement this contract without printing row data:

```bash
deploy/deploy-guard.sh preflight \
  --database /var/lib/tokemon/data/tokemon.db \
  --backup-destination /mnt/tokemon-off-host
docker compose --env-file .env \
  -f deploy/docker-compose.yml -f deploy/docker-compose.release.yml \
  up -d --no-build
deploy/deploy-guard.sh postflight \
  --database /var/lib/tokemon/data/tokemon.db \
  --health http://127.0.0.1:18787/healthz
```

Preflight canonicalizes both paths and refuses a backup destination that is
the live database directory, the database itself, or any nested/symlinked path
under that directory. Use a separate mounted/off-host destination; a merely
non-empty path is not sufficient.

`deploy/check-database.sh` refuses a zeroed/non-SQLite file before any Compose
operation. `deploy/test-database-guard.sh` is the regression test for the
incident failure mode.

The upgrade/rollback contract is:

1. Keep the previous immutable image digest and the verified off-host SQLite
   snapshot.
2. Deploy the new semver+digest image with the persistent data mount unchanged.
3. Let the normal schema migration run only after the preflight passes; a
   migration must preserve its versioned before/after snapshot.
4. If health or counts fail, stop the service, restore the last verified
   snapshot using the offline restore procedure, and redeploy the previous
   image digest. Never delete the data directory or Compose volume.

The migration compatibility gate runs on every pull request and release. It
opens a representative old-schema fixture, verifies that the migration keeps
metadata rows and records the rollback snapshot, and exercises the current
WAL mode. Persistent databases, WAL/SHM sidecars, backups, incidents, and
private fixtures are excluded from Git, Docker, and release contexts by
`deploy/release-guard.sh`; the guard fails closed if one is ever tracked or
present in a build context.

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
  --machine-id laptop \
  --adapters claude-code,codex \
  --token-stdin <<'TOKEN'
replace-with-a-generated-secret
TOKEN
```

The installer selects the matching macOS or Linux architecture, writes a mode-`0600` configuration, preserves the local state database across upgrades, and installs launchd or systemd without requiring root. Use a private server base URL; the installer adds the API paths itself.

For a locally built binary, replace `--version 0.3.2` with `--binary ./tokemon`:

```bash
bash deploy/install-agent.sh \
  --binary ./tokemon \
  --server https://tokemon.example.ts.net \
  --adapters claude-code,codex \
  --token-stdin <<'TOKEN'
replace-with-a-generated-secret
TOKEN
```

Use `--no-supervisor` when another process manager owns the agent. Remove the user-level service and its configuration with:

```bash
bash deploy/install-agent.sh --uninstall
```

The local cursor database is preserved by uninstall so reinstalling the agent does not require a full rescan. For noninteractive installs, provide `TOKEMON_INGEST_TOKEN` in the environment or pipe one line to `--token-stdin`; inline token arguments are rejected so secrets never appear in process arguments.

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
# Optional: set false to omit even normalized project basenames.
TOKEMON_INCLUDE_PROJECTS=false
TOKEMON_STATE=/Users/example/.local/share/tokemon/state.db
TOKEMON_AGENT_REQUEST_TIMEOUT=30s
```

Explicit command-line flags override environment variables. The agent state contains cursors and sync metadata only. Provider files are read locally and are never uploaded as source content.

### Privacy boundary and project labels

The agent applies one outbound guard to both `tokemon inspect` and every event
upload. Unknown event fields, prompt/response text, source code, credentials,
tool arguments, full paths, conversation titles, and unapproved metadata are
rejected before the first request is opened. A rejected batch does not advance
the local cursor or change the hub database.

Provider adapters normalize a working directory to a lowercase basename (for
example, `/Users/alice/work/SecretClient` becomes `secretclient`). This avoids
disclosing a full path, but the basename itself can still identify a sensitive
project. Set `TOKEMON_INCLUDE_PROJECTS=false` (or pass
`--include-projects=false`) to omit project labels while retaining token and
provider totals. You can also use `TOKEMON_ADAPTERS` as an explicit provider
allowlist and leave `TOKEMON_JSONL_PATHS` empty; generic JSONL is never scanned
unless a path is configured. Extension metadata must use the documented
`tokemon_` namespace and must not contain content, path, credential, or URL
values.

Before each collection pass, the agent checks the hub's `/healthz` endpoint. If the hub is offline, the agent applies its bounded failure backoff without scanning provider histories; cursor state remains unchanged and collection resumes when the hub is healthy.

Health, heartbeat, and ingest calls each use the configured request deadline
(`TOKEMON_AGENT_REQUEST_TIMEOUT`, or `--request-timeout`). A failed or timed
out ingest never commits the local source cursor or event fingerprint, so the
next pass retries the same metadata batch safely.

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

For lost/corrupt data, credential rotation, privacy reports, evidence
preservation, and a disposable recovery tabletop, use the public
[incident-response runbook](docs/incident-response.md).

- Keep the hub on a private network or behind an authenticated proxy.
- Set a strong `TOKEMON_INGEST_TOKEN` for every multi-machine deployment.
- Health is intentionally unauthenticated for liveness checks; protect every
  other route with the dashboard credential. If a reverse proxy supplies
  `X-Forwarded-User`, configure its exact source CIDR in
  `TOKEMON_TRUSTED_PROXY_CIDRS` and make the proxy strip client-supplied copies.
- Event requests are bounded before SQLite mutation: compressed body 10 MiB,
  decompressed body 8 MiB, serialized event batch 4 MiB, and 1,000 events.
  Invalid IDs/timestamps/currency/cost/components or sensitive/oversized
  metadata are rejected as a whole batch, preserving the previous database
  state.
- Store tokens in mode-`0600` files or a platform secret store; do not commit them or bake them into images.
- Run native agents as the local user and containers as non-root.
- Mount provider data read-only when using containers.
- Tokemon collects usage metadata only; prompts, responses, source code, conversation titles, and full repository paths are not part of the default event payload.
