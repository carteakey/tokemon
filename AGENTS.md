# Tokemon Repository Instructions

## Planning

Linear is the source of truth for committed planned work in this repository.

- Team: `carteakey`
- Project: [Tokemon v0.2](https://linear.app/carteakey/project/tokemon-v02-ef3cefe83acc)
- Active milestone: `v0.2 Integrations & Evolution Art`
- Initial issues: CAR-68, CAR-65, CAR-63, CAR-64, CAR-67, and CAR-66

Do not duplicate committed work in GitHub Issues or `TODO.md`.

## Product guardrails

- Treat `docs/tokemon-spec-v0.2.md` as authoritative; `docs/token-bandit-spec.md` is historical context.
- Store usage metadata only. Do not collect prompts, responses, source code, repository contents, or conversation titles by default.
- Prefer the existing Go, SQLite, server-rendered HTML, and deterministic evolution architecture.
- Keep provider quirks inside adapters and preserve unknown values as unknown.

## Delivery rule

Use the `ship` loop after every implementation change:

```text
change → test → deploy → verify
```

Do not report a change complete after tests alone. Deploy the affected Tokemon service to the active target, then verify its health and the changed behavior. For server or dashboard changes, deploy the primary Mac hub and verify its Tailscale URL; for agent changes, redeploy each affected enrolled machine. Keep the deployment scoped to the changed component and preserve persistent data.
