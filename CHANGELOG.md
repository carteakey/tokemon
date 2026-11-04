# Changelog

## Unreleased

- Fixed dashboard WCAG AA contrast: the analytics Apply-filters button no longer renders accent text on the browser's default gray button background (2.54:1 → 8.43:1), and dimmed labels, footers, table headers, and axis text use a faint-gray token that passes on both card and page backgrounds (3.77–4.04:1 → 5.04–5.39:1).
- Stopped the overview breakdown tables from truncating share percentages such as 100.0% by widening the share column (16% → 25%) in the fixed table layout.
- Made dashboard keyboard focus visible: the settings gear keeps a solid accent outline instead of dropping it, and window links, selects, and action buttons show solid outlines on focus instead of ~16–22% alpha ones.
- Reduced overview keyboard tab stops from 374 to 20 by making token-activity heatmap cells focusable only on days with usage or unknown tokens; the tooltip continues to open on focus for the remaining cells.
- Exposed odometer values to screen readers via role=img and aria-label so assistive tech hears the formatted total instead of every digit-reel value.

## 0.3.3

- Added a metadata-only Hermes Agent adapter for default and named-profile SQLite usage stores.
- Added an analytics usage-share timeline showing the top five harnesses, providers, models, or machines per window plus aggregated Others.
- Prevented new or archived Codex sessions from invalidating every unchanged session snapshot; late forks now reparse only the dependent child.
- Added a hub health preflight so offline hubs back off without repeatedly scanning local provider histories.
- Cached the dashboard evolution summary between ingests with a bounded freshness window, while keeping writes visible immediately and collapsing concurrent polling into one recompute.
- Hardened evolution and analytics accounting with schema drift warnings, missing-token attribution tests, and regression coverage for share timeline invariants.
- Added a covering index over lifetime token totals so ingest batches, evolution recomputes, and dashboard renders stop scanning the full usage history.
- Cached dashboard overview renders between ingests with the same bounded freshness window as the evolution summary, collapsing concurrent dashboard, analytics overview, and settings loads into one recompute; a compute that raced an ingest can no longer reinstall stale results.
- Stopped the agent from re-writing unchanged source cursors on every poll cycle.

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
