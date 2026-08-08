# Production Readiness TODO

This is the launch checklist for a trustworthy Tokemon deployment. The target
is a private-network self-hosted release, not a public multi-tenant service.

## P0: Before Production

- [x] Keep the production branch buildable after security changes; add a CI test that exercises the authenticated settings flow before merging.
- [x] Fail closed when `TOKEMON_INGEST_TOKEN` is empty, except for an explicit loopback-only development mode.
- [ ] Define and enforce the dashboard trust boundary. Protect dashboard read endpoints or require an explicit private bind/authenticated reverse proxy; document Tailscale and VPN requirements.
- [x] Protect `/settings/aliases` with authentication and CSRF protection.
- [x] Give dashboard sessions a server-enforced expiry and secure cookie attributes.
- [ ] Add a dashboard session key rotation strategy and login-attempt throttling.
- [x] Replace `http.ListenAndServe` with a configured `http.Server` using read, write, idle, and header timeouts.
- [x] Add graceful shutdown on `SIGTERM` and `SIGINT`.
- [x] Limit decompressed gzip request bodies, event count, event field sizes, and total batch size.
- [x] Add explicit per-request timeouts to agent health, heartbeat, and ingest calls.
- [x] Add request validation for event ID format, string lengths, timestamps, currencies, and token consistency.

## P1: Reliability And Operations

- [x] Make `/healthz` verify SQLite readiness and return failure when the database is unavailable.
- [x] Do not advance agent state for rejected events.
- [ ] Add per-event acknowledgements or a durable dead-letter/retry path for rejected events.
- [x] Strictly decode request bodies, reject trailing JSON, and bound decompressed payloads, metadata, and individual adapter records.
- [ ] Add structured request and ingestion logs without logging tokens, credentials, or event payloads.
- [ ] Track ingestion failures, rejected events, source errors, stale agents, and database latency.
- [ ] Document a backup and restore procedure, including WAL-safe backups and an offline restore test.
- [ ] Add scheduled or off-host backups for the SQLite database.
- [ ] Define the single-process policy for migrations and CLI operations that access the database.
- [ ] Add retention and purge guidance, including the effect of deleting historical data on evolution stage.
- [x] Compare ingest tokens in constant time.
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

- [x] Add an outbound-payload test that rejects prompts, responses, source code, repository paths, titles, and unrelated metadata.
- [ ] Test every adapter with fixtures containing sensitive content and filesystem paths.
- [ ] Document that normalized project basenames can still reveal sensitive project names.
- [ ] Verify agent state and configuration permissions on macOS and Linux.
- [ ] Verify provider source mounts are read-only in containerized agent deployments.

## P2: Product Quality

- [x] Correct summary/session counts and add regression tests for cross-machine/session collisions.
- [ ] Add regression coverage for changing session metadata and define the desired session identity semantics.
- [ ] Implement the Tokedex/catalog view and reconcile the documented `/api/v1/models`, `/api/v1/catalog`, `/api/v1/sessions`, and timeline API contract with the shipped routes.
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
