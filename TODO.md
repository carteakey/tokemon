# Production Readiness TODO

This is the launch checklist for a trustworthy Tokemon deployment. The target
is a private-network self-hosted release, not a public multi-tenant service.

## P0: Before Production

- [ ] Fail closed when `TOKEMON_INGEST_TOKEN` is empty, except for an explicit loopback-only development mode.
- [ ] Define and enforce the dashboard trust boundary. Document Tailscale, VPN, or authenticated reverse-proxy requirements.
- [ ] Protect `/settings/aliases` with authentication and CSRF protection.
- [ ] Replace `http.ListenAndServe` with a configured `http.Server` using read, write, idle, and header timeouts.
- [ ] Add graceful shutdown on `SIGTERM` and `SIGINT`.
- [ ] Limit decompressed gzip request bodies, event count, event field sizes, and total batch size.
- [ ] Add explicit per-request timeouts to agent health, heartbeat, and ingest calls.
- [ ] Add request validation for event ID format, string lengths, timestamps, currencies, and token consistency.

## P1: Reliability And Operations

- [ ] Make `/healthz` verify SQLite readiness and return failure when the database is unavailable.
- [ ] Add structured request and ingestion logs without logging tokens, credentials, or event payloads.
- [ ] Track ingestion failures, rejected events, source errors, stale agents, and database latency.
- [ ] Document a backup and restore procedure, including WAL-safe backups and an offline restore test.
- [ ] Add scheduled or off-host backups for the SQLite database.
- [ ] Define the single-process policy for migrations and CLI operations that access the database.
- [ ] Add retention and purge guidance, including the effect of deleting historical data on evolution stage.
- [ ] Compare ingest tokens in constant time.
- [ ] Ensure database files, WAL files, backups, and provider fixtures are excluded from releases and version control.

## P1: Release And Supply Chain

- [ ] Add CI gates for `go test ./...`, `go vet ./...`, race tests, Docker builds, migrations, and integration tests.
- [ ] Test the release installer on clean macOS and Linux arm64 and amd64 environments.
- [ ] Publish signed release artifacts in addition to SHA-256 checksums.
- [ ] Generate an SBOM and scan Go dependencies and container images.
- [ ] Pin the Docker base image by digest.
- [ ] Publish immutable versioned container images and avoid using `latest` for production upgrades.
- [ ] Add an upgrade and rollback runbook with database compatibility checks.

## P1: Privacy Regression Coverage

- [ ] Add an outbound-payload test that rejects prompts, responses, source code, repository paths, titles, and unrelated metadata.
- [ ] Test every adapter with fixtures containing sensitive content and filesystem paths.
- [ ] Document that normalized project basenames can still reveal sensitive project names.
- [ ] Verify agent state and configuration permissions on macOS and Linux.
- [ ] Verify provider source mounts are read-only in containerized agent deployments.

## P2: Product Quality

- [ ] Add end-to-end coverage for two agents, retries, cursor safety, file rotation, duplicate ingestion, and threshold crossing.
- [ ] Add SQLite backup/restore and migration tests using representative production-sized data.
- [ ] Browser-test the dashboard on desktop and mobile, including tables, charts, keyboard navigation, and reduced motion.
- [ ] Add accessibility checks for contrast, labels, focus states, and chart alternatives.
- [ ] Add a documented incident response path for lost data, leaked credentials, and privacy reports.

## Launch Gate

Tokemon can be called production-ready for a private-network deployment when all P0
items are complete, restore has been tested, the release pipeline is repeatable,
and an agent has successfully synced to a clean hub from two supported machines.

Public-internet deployment remains unsupported until dashboard authentication,
authorization, and tenant isolation exist.
