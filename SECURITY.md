# Security policy

## Supported versions

Security fixes are prioritized for the latest published Tokemon release.

## Reporting a vulnerability

Please do not include ingest tokens, provider data, prompts, responses, source
code, or database files in a report. Use [GitHub's private vulnerability
reporting](https://github.com/carteakey/tokemon/security/advisories/new) when it
is available for this repository. If private reporting is unavailable, contact
the maintainers through the Tokemon GitHub profile before opening a public
issue.

For operational containment, credential rotation, backup/restore, purge, and
privacy-report handling, follow the public [incident-response runbook](docs/incident-response.md).

Tokemon is designed for a trusted local or private network. Non-loopback
listeners fail closed without `TOKEMON_INGEST_TOKEN`; local development must
opt into `--dev-loopback` while binding explicitly to loopback. Dashboard,
settings, analytics, export, machines, and evolution routes require Basic or
Bearer authentication using `TOKEMON_DASHBOARD_TOKEN` (or the ingest token by
default). A trusted reverse proxy may assert `X-Forwarded-User` only from
configured `TOKEMON_TRUSTED_PROXY_CIDRS`, and must strip that header from
untrusted requests. Direct public-internet exposure remains unsupported.

Ingest bodies are bounded before SQLite mutation and invalid batches are
rejected atomically. Tokemon preserves legitimate unknown provider values, but
rejects malformed IDs/timestamps/currency/cost/component totals and
sensitive/oversized metadata.
