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
