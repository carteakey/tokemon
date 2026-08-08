# Changelog

## 0.3.3

- Added a metadata-only Hermes Agent adapter for default and named-profile SQLite usage stores.
- Added an analytics usage-share timeline showing the top five harnesses, providers, models, or machines per window plus aggregated Others.
- Prevented new or archived Codex sessions from invalidating every unchanged session snapshot; late forks now reparse only the dependent child.
- Added a hub health preflight so offline hubs back off without repeatedly scanning local provider histories.
- Cached the dashboard evolution summary between ingests with a bounded freshness window, while keeping writes visible immediately and collapsing concurrent polling into one recompute.
- Hardened evolution and analytics accounting with schema drift warnings, missing-token attribution tests, and regression coverage for share timeline invariants.

## 0.3.2

- Published the first MIT-licensed, public-ready Tokemon release.
- Added polished public documentation, deployment guidance, security policy, and release notes.
- Added CI coverage for race tests, static analysis, Compose validation, and all four release archives.
- Consolidated the current provider adapters, metadata-only privacy model, multi-machine dashboard, analytics views, and checksum-verified installers into one release line.

## 0.3.1

- Added authenticated downloads for private GitHub release assets.
- Preserved custom agent state paths during upgrades.
- Corrected default state and install paths for alternate homes.

## 0.3.0

- Added modular built-in provider adapters and per-machine adapter selection.
- Added metadata-only agent heartbeats and deployment diagnostics.
- Added version and build identity to the CLI and agent metadata.
- Added checksum-verified macOS and Linux release archives for arm64 and amd64.
- Added user-level launchd and systemd installation with preserved cursor state.
