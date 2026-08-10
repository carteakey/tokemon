# Production readiness notes

The former launch checklist was promoted to Linear. The committed backlog is
authoritative there; do not recreate these entries in `TODO.md`:

- **CAR-132 — Harden ingest and dashboard trust boundaries:** token fail-closed
  behavior, private/trusted-proxy boundary, dashboard/settings auth and CSRF,
  constant-time bearer checks, bounded bodies/batches, strict event validation,
  and privacy-safe rejection behavior.
- **CAR-129 — HTTP lifecycle and operational reliability:** server timeouts,
  graceful shutdown, readiness, structured observability, and request timeout
  behavior.
- **CAR-128 — Durable ingestion and recovery:** WAL-safe backup/restore,
  retention/purge guidance, single-process migrations, and recovery tests.
- **CAR-133 — Release and supply-chain integrity:** CI/release gates,
  cross-platform installer checks, signing/SBOM/scanning, pinned images, and
  upgrade/rollback evidence.
- **CAR-131 — Privacy regression coverage:** outbound payload/fixture tests,
  normalized project-name disclosure, permissions, and read-only source mounts.
- **CAR-67 — Multi-agent ingestion quality:** retries, cursors, rotation,
  duplicate handling, threshold crossings, and end-to-end multi-agent tests.
- **CAR-130 — Dashboard quality and incident readiness:** browser/mobile,
  accessibility, reduced-motion, and documented incident response coverage.

No additional speculative notes are currently committed here. Tokemon remains a
private-network self-hosted product; direct public-internet deployment is
unsupported until dashboard authentication, authorization, and tenant
isolation are complete.
