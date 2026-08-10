# Tokemon

**Version:** 0.2  
**Status:** MVP product specification  
**Working tagline:** *Your tokens are evolving.*

> A tiny, self-hosted dashboard that finds AI coding usage across your machines, counts every token, and evolves your Tokemon at deterministic lifetime-usage checkpoints.

---

## 1. The Idea

Tokemon scans local usage records from AI coding tools such as Claude Code and Codex, normalizes them into a common format, and sends usage metadata from multiple machines to one central dashboard.

The centre of the dashboard is a large lifetime-token odometer and one original creature: your **Tokemon**.

Every time total lifetime usage crosses a configured threshold, the Tokemon evolves. Thresholds use powers of ten through 1B tokens, then become denser so late-stage progression remains visible.

```text
9,999 tokens      → current form
10,000 tokens     → evolution
99,999 tokens     → current form
100,000 tokens    → evolution
```

The joke is immediate, but the analytics underneath it must remain accurate, useful, and restrained.

Tokemon should be:

- Self-hosted
- Privacy-first
- Lightweight
- Multi-machine
- Easy to deploy
- Useful with almost no configuration
- Extensible across providers
- Funny without becoming noisy
- Technically boring in the best possible way

Tokemon must never collect prompts, responses, source code, or repository contents by default. It processes usage metadata only.

---

## 2. Product Positioning

Tokemon answers four questions:

1. How many tokens am I using?
2. Which tools, models, providers, and machines are using them?
3. How much would that usage approximately cost?
4. What has this thing evolved into now?

The product should feel like:

> Plausible Analytics with one small creature slowly becoming an eldritch god because you refused to stop using coding agents.

The interface is a serious analytics dashboard first. The evolving mascot is the one absurd element that gives the product an identity.

---

## 3. Product Principles

1. **Metadata, not conversations.**
2. **One binary should be enough.**
3. **SQLite until SQLite is genuinely insufficient.**
4. **Provider quirks belong inside adapters.**
5. **Unknown values remain unknown.**
6. **Every number should explain where it came from.**
7. **The mascot may be silly; the data may not be.**
8. **Evolution is deterministic, not a second product.**
9. **Adding a provider should not require redesigning the system.**
10. **Boring infrastructure is a feature.**
11. **No component should exist merely because it may become useful later.**
12. **No dark patterns, artificial streak anxiety, loot boxes, currencies, or engagement machinery.**

---

## 4. Strict v0.2 Scope

Version 0.2 consists of:

- One Go binary
- `serve`, `agent`, `discover`, `inspect`, `import`, `export`, and `purge` commands
- One Docker Compose service for the server
- Native agents on each machine
- SQLite
- Claude Code adapter
- Codex adapter
- Generic JSONL adapter
- OpenClaw adapter
- Hermes Agent adapter
- Incremental polling
- Idempotent ingestion
- One-page analytics dashboard
- Lifetime token odometer
- One global Tokemon
- Deterministic order-of-magnitude evolution
- Current evolution stage
- Progress to the next evolution
- Daily usage chart
- Usage by model
- Usage by machine
- Recent sessions
- Static YAML model catalog
- Simple model tier list called the **Tokedex**
- Simple estimated API cost
- No prompt or response collection

The implementation should feel like:

> One server, one tiny agent, three adapters, one database, one creature, and one excellent dashboard.

---

## 5. Non-Goals

Version 0.2 will not:

- Proxy model requests
- Replace LiteLLM
- Inspect source code
- Store prompts or responses
- Evaluate answer quality
- Score developers
- Track employee productivity
- Require Kubernetes
- Require Postgres
- Require Redis
- Require a JavaScript-heavy frontend
- Use WebSockets or Server-Sent Events
- Support runtime-loaded plugins
- Implement user accounts
- Implement role-based access control
- Implement organization billing
- Calculate exact subscription-token costs
- Perform currency conversion
- Maintain pricing history
- Synchronize the model catalog remotely
- Recursively scan an entire home directory
- Provide a retention-management interface
- Support every AI provider immediately
- Add battles, quests, items, teams, trading, breeding, achievements, or virtual currency
- Add randomized creature generation
- Add daily login rewards
- Turn the app into a game engine

The evolution mechanic is a visual representation of lifetime usage, not a gamification platform.

---

## 6. The Tokemon Evolution System

### 6.1 Core Rule

The global Tokemon evolves whenever lifetime total tokens cross the next threshold. Thresholds follow powers of ten through 1B, then use a repeating `1–2–5` sequence through the 1T final form.

```text
stage = highest index whose threshold <= lifetime_tokens
```

The threshold table is ordered and append-only: preserve the meaning and asset number of existing stages, and add future forms only at the tail. The displayed stage is derived entirely from stored usage. It is not mutable user state and does not require a separate progression service.

### 6.2 Evolution Thresholds

The initial progression:

| Lifetime tokens | Stage | Working form name |
|---:|---:|---|
| 0–9 | 0 | Egg |
| 10–99 | 1 | Bitling |
| 100–999 | 2 | Bytelet |
| 1,000–9,999 | 3 | Promptling |
| 10,000–99,999 | 4 | Context Cub |
| 100,000–999,999 | 5 | Token Scout |
| 1M–9.99M | 6 | Code Caster |
| 10M–99.99M | 7 | Agent Beast |
| 100M–999.99M | 8 | Context Dragon |
| 1B–1.99B | 9 | Token Titan |
| 2B–4.99B | 10 | Model Eater |
| 5B–9.99B | 11 | Context Deity |
| 10B–19.99B | 12 | Reality Core |
| 20B–49.99B | 13 | Inference Leviathan |
| 50B–99.99B | 14 | Parameter Colossus |
| 100B–199.99B | 15 | World Weaver |
| 200B–499.99B | 16 | Cosmic Architect |
| 500B–999.99B | 17 | Universe Engine |
| 1T+ | 18 | The Singularity |

These names are working copy, not hard product terminology. The visual form and stage number matter more than the labels.

### 6.3 Evolution Progress

The dashboard shows:

- Current form
- Current lifetime tokens
- Previous threshold
- Next threshold
- Tokens remaining
- Percentage progress within the current threshold interval

Example:

```text
TOKEN TITAN · STAGE 9

1,482,938,221 lifetime tokens
48.3% toward the next evolution
517,061,779 tokens remaining
```

Progress is linear within the current threshold interval, not a percentage of all historical usage.

A simple calculation:

```text
lower = thresholds[stage]
upper = thresholds[stage + 1]
progress = (lifetime_tokens - lower) / (upper - lower)
```

### 6.4 Evolution Event

When a newly ingested batch crosses a threshold:

- Mark the response as containing an evolution.
- On the next dashboard load, play one short visual transition.
- Show the previous and current forms.
- Do not play sound.
- Do not use confetti.
- Do not block access to analytics.
- Do not repeatedly replay the event.

The event can be acknowledged locally in the browser using a stored last-seen stage. No server-side notification system is required.

### 6.5 Historical Imports

If imported data skips several stages:

- Set the Tokemon directly to the correct current stage.
- Show `You crossed 4 evolution stages`.
- Do not animate every missed stage sequentially.
- Make previous forms viewable in a simple evolution strip.

### 6.6 Art Implementation

Keep the art system deliberately simple.

- Use original local raster assets.
- Ship one asset per stage.
- Use the same silhouette family so evolution feels coherent.
- Early forms should be small and rounded.
- Later forms should become increasingly elaborate and absurd.
- Maintain a limited palette.
- Avoid dependencies on a canvas engine or animation framework.
- Use a CSS opacity/scale transition between forms.
- Provide a reduced-motion fallback.
- If an asset fails to load, show the stage number and form name instead.

No generated image API is required at runtime.

### 6.7 Brand Guardrail

Tokemon should use wholly original creature designs and interface assets.

Do not use:

- Existing Pokémon creatures
- Poké Balls
- Franchise typefaces
- Franchise badges
- Character names
- Game screenshots
- Copied evolution animations
- Recognizable franchise UI

Before a broad public or commercial launch, treat **Tokemon** as a working title and complete an appropriate name and trademark review.

---

## 7. Recommended Technology

### Language

Use **Go** for the server and agent.

Reasons:

- Easy cross-compilation
- One static binary
- Low memory usage
- Fast startup
- Straightforward concurrency
- Good filesystem support
- Simple deployment
- Easier contribution than a mixed-language stack
- One release artifact for all modes

Rust remains a possible future option for unusually difficult parsers, but it is unnecessary for the MVP.

### Backend

- Go
- SQLite
- Embedded migrations
- REST API
- Embedded HTML templates
- Embedded static assets, including Tokemon SVGs

### Frontend

Prefer:

- Go templates
- HTMX only where it materially helps
- A lightweight charting library
- Minimal custom JavaScript
- CSS transitions for evolution

Do not introduce React, a frontend monorepo, or a large build pipeline unless the dashboard eventually outgrows server-rendered HTML.

### Refresh Model

Do not use live sockets.

Refresh analytics:

- On page load
- When filters change
- Automatically every 60 seconds

That is sufficiently live for token analytics.

---

## 8. Single-Binary CLI

```bash
tokemon serve
tokemon agent
tokemon discover
tokemon inspect
tokemon import usage.jsonl
tokemon export usage.jsonl
tokemon purge --before 2026-01-01
```

### `tokemon serve`

Starts:

- REST API
- Dashboard
- SQLite database
- Embedded default model catalog
- Embedded Tokemon assets

### `tokemon agent`

Starts the local machine agent and periodically scans configured sources.

### `tokemon discover`

Finds supported tools and reports detected paths.

```text
✓ Claude Code detected at ~/.claude
✓ Codex detected at ~/.codex
✓ Hermes Agent detected at ~/.hermes
– Gemini CLI not detected
– OpenCode not detected
– OpenClaw not detected
```

### `tokemon inspect`

Prints exactly what the agent would send to the server.

This is a core privacy feature.

### `tokemon import`

Imports normalized JSONL events.

### `tokemon export`

Exports normalized JSONL events.

### `tokemon purge`

Deletes usage events older than a specified date.

CSV import and export are not required for v0.2.

---

## 9. Deployment

### Central Server

```yaml
services:
  tokemon:
    image: ghcr.io/tokemon/tokemon:latest
    command: serve
    ports:
      - "8080:8080"
    volumes:
      - ./data:/data
      - ./models.yaml:/config/models.yaml:ro
    environment:
      TOKEMON_DATABASE: /data/tokemon.db
      TOKEMON_INGEST_TOKEN: change-me
      TOKEMON_MODEL_CATALOG: /config/models.yaml
    restart: unless-stopped
```

### Machine Agents

Agents can run as:

- Native binaries
- systemd services
- launchd services
- Docker containers with read-only mounts

Native installation is preferred because local dotfiles and application data are easier to access safely.

The supported install flow uses one release binary for macOS and Linux on
arm64 and amd64. A user-level installer verifies the release checksum, writes
the mode-0600 agent configuration, and installs launchd or systemd without
requiring Go, Docker, or root. The same binary contains the built-in adapters;
`TOKEMON_ADAPTERS` selects a per-machine allowlist, while an explicit generic
JSONL path opts the generic adapter in.

### Authentication

For v0.2:

- Require a non-empty shared ingest token on every non-loopback listener.
- Allow an empty token only with the explicit `--dev-loopback` mode and an
  explicit loopback bind (`127.0.0.1`, `::1`, or `localhost`).
- Protect dashboard, settings, analytics, export, machines, and evolution
  routes with Basic auth (`tokemon:<token>`) or Bearer auth. The dashboard
  token defaults to the ingest token and may be separated with
  `TOKEMON_DASHBOARD_TOKEN`.
- A reverse proxy may provide `X-Forwarded-User` only when its source is in
  the configured `TOKEMON_TRUSTED_PROXY_CIDRS`; the proxy must strip incoming
  copies of that header. Do not trust forwarded identity from arbitrary
  clients.
- Keep the service on Tailscale/WireGuard or behind an authenticated proxy;
  direct public-internet exposure is unsupported.
- Generate a stable machine ID for each agent.

Per-agent credentials and revocation can come later.

---

## 10. Architecture

```text
┌─────────────────────┐
│ Laptop              │
│ tokemon agent       │
│ Claude / Codex logs │
└──────────┬──────────┘
           │ normalized usage events
           ▼
┌────────────────────────────┐
│ Tokemon Server             │
│ API + SQLite + Dashboard   │
│ Lifetime total → Evolution │
└──────────▲─────────────────┘
           │
┌──────────┴──────────┐
│ Homelab Machine     │
│ tokemon agent       │
│ Multiple tools      │
└─────────────────────┘
```

### Agent Responsibilities

The agent:

- Discovers supported local tools
- Locates known usage files
- Polls files at a fixed interval
- Reads only new records
- Normalizes provider-specific records
- Sends batches to the server
- Saves a local cursor after successful upload
- Retries failed uploads later
- Reports machine status
- Never sends conversation content

The agent reports build version, operating system, architecture, selected
adapter IDs, source counts, and source-error counts through the authenticated
`/api/v1/agents/heartbeat` endpoint. Heartbeats contain no local paths or
provider record content.

### Server Responsibilities

The server:

- Authenticates ingestion
- Accepts normalized usage events
- Deduplicates records
- Stores usage
- Calculates analytics
- Derives the current evolution stage from lifetime tokens
- Calculates simple estimated cost
- Loads model metadata from YAML
- Serves the dashboard
- Imports and exports JSONL

No special evolution database is required. The current stage is a derived metric.

---

## 11. Polling and Cursor Model

Do not introduce filesystem watchers.

Every 60 seconds, the agent should:

1. Inspect known source paths.
2. Find files supported by an adapter.
3. Read from the last cursor.
4. Normalize new records.
5. Send a batch.
6. Save the new cursor only after a successful upload.

Usage logs already provide durable storage. A separate local event spool is unnecessary.

Local agent state needs only:

- Source path
- Source identity
- Last byte offset or logical cursor
- Last successful sync
- Machine ID

If an upload fails:

- Do not advance the cursor.
- Retry on the next poll.

Deterministic server-side event IDs prevent duplicates.

### File Rotation

Detect:

- File truncation
- File replacement
- Log rotation
- Missing files

A new file identity should reset the cursor safely without duplicating historical events.

---

## 12. Filesystem Discovery

Inspect known locations such as:

```text
~/.claude/
~/.codex/
~/.copilot/session-state/
~/.config/
~/.local/share/
~/Library/Application Support/
%APPDATA%
```

Each adapter defines its own paths and supported file patterns.

Do not recursively scan the entire home directory.

Example configuration:

```yaml
server:
  url: https://tokens.example.com
  token: ${TOKEMON_INGEST_TOKEN}

machine:
  name: mac-server

scan:
  interval: 60s

privacy:
  include_prompt_content: false
  include_response_content: false
  include_repository_paths: false

sources:
  auto_discover: true
  paths:
    - type: claude-code
      path: ~/.claude
    - type: codex
      path: ~/.codex
    - type: generic-jsonl
      path: ~/ai-usage/*.jsonl

state:
  path: ~/.local/share/tokemon/state.db
```

The local state database stores cursors and machine state only.

The native agent enforces this boundary again at the shared outbound client and
the `inspect` command. Unknown fields and sensitive metadata are rejected
before any upload. A working directory may be reduced to a lowercase basename
for project analytics (for example, `/work/SecretClient` becomes
`secretclient`); the basename can still be identifying. Users who do not want
project labels can set `TOKEMON_INCLUDE_PROJECTS=false` or use
`--include-projects=false`. `TOKEMON_ADAPTERS` is an explicit provider
allowlist, and generic JSONL is opt-in through configured paths. Extension
metadata is limited to keys in the `tokemon_` namespace.

---

## 13. Normalized Usage Format

All adapters emit one normalized event.

```json
{
  "schema_version": "1",
  "event_id": "sha256:...",
  "timestamp": "2026-07-11T18:42:00Z",
  "machine_id": "mac-server",
  "session_id": "optional-provider-session-id",
  "project": "carteakey.dev",
  "provider": "anthropic",
  "model": "claude-opus-4.1",
  "tool": "claude-code",
  "input_tokens": 12000,
  "output_tokens": 3400,
  "cache_read_tokens": 8200,
  "cache_write_tokens": 1100,
  "reasoning_tokens": null,
  "total_tokens": 24700,
  "duration_ms": 42000,
  "cost": null,
  "currency": "USD",
  "token_accuracy": "reported",
  "source": {
    "adapter": "claude-code",
    "adapter_version": "1.0.0"
  },
  "metadata": {}
}
```

### Required Fields

- `schema_version`
- `event_id`
- `timestamp`
- `machine_id`
- `project` is optional and contains only a normalized project label, never a repository path
- `provider`
- `model`
- `tool`
- `token_accuracy`

Token and duration fields may be `null`.

### Event Identity

Create a deterministic ID from stable source fields such as:

- Machine ID
- Adapter ID
- Source-file identity
- Provider event ID, when available
- Record offset
- Timestamp
- Session ID

This allows safe rescanning without duplicates.

---

## 14. Token Accuracy

Distinguish:

- **Reported** — explicitly recorded by the provider or tool
- **Derived** — calculated from reported component fields
- **Estimated** — approximated because exact data was unavailable
- **Unknown** — unavailable and not safely inferable

Never blend these silently.

The dashboard should show:

```text
94% reported · 6% estimated
```

Unknown values remain unknown rather than becoming zero.

Only reported, derived, or clearly estimated tokens contribute to evolution. Records with unknown total tokens do not.

---

## 15. Provider Adapters

Use clean internal Go adapters.

Do not implement external runtime plugins in v0.2.

```go
type Adapter interface {
    ID() string
    Discover(ctx context.Context) ([]Source, error)
    Parse(ctx context.Context, source Source, cursor Cursor) ([]UsageEvent, Cursor, error)
    NormalizeModel(rawModel string) string
    Capabilities() Capabilities
}
```

```go
type Capabilities struct {
    InputTokens     bool
    OutputTokens    bool
    CacheTokens     bool
    ReasoningTokens bool
    Cost            bool
    SessionID       bool
    Duration        bool
}
```

Each adapter defines:

- ID
- Display name
- Discovery paths
- Supported file patterns
- Parser
- Cursor strategy
- Deduplication inputs
- Model-name normalization
- Capabilities
- Accuracy level

### v0.2 Adapters

Built-in and extension adapters:

1. Claude Code
2. Codex
3. GitHub Copilot CLI
4. OpenCode
5. Antigravity
6. OpenClaw
7. Hermes Agent
8. Generic JSONL

GitHub Copilot CLI usage is read from `~/.copilot/session-state/*/events.jsonl`. The adapter consumes only durable `session.shutdown.modelMetrics.usage` aggregates and the final directory name from session context. Prompts, responses, tool arguments, titles, repository paths, and modified-file lists are never included in normalized events. One stable snapshot is emitted per model and session; active sessions become visible after Copilot writes their shutdown aggregate.

OpenClaw usage is read from `~/.openclaw/agents/*/sessions/*.jsonl`. The adapter consumes only assistant message usage metadata, provider, model, timestamps, and the session header's normalized project basename. Transcript content, tool calls, tool arguments, trajectory mirrors, and full working-directory paths are never included in normalized events. Zero-token delivery and error records are ignored, and incomplete final records remain eligible for the next poll.

Hermes Agent usage is read from the `sessions` and `session_model_usage` metadata tables in `~/.hermes/state.db` and `~/.hermes/profiles/*/state.db`. The adapter reconciles per-model and auxiliary usage against session aggregates, preserving provider, model, token components, timestamps, costs, and only the normalized working-directory basename. It never queries Hermes' `messages` table, so prompts, responses, reasoning text, tool arguments, conversation titles, credentials, billing endpoints, and full filesystem paths remain local. Stable snapshot IDs and WAL-aware source signatures make repeated polling idempotent while active sessions continue to update.

### Generic JSONL

The generic format is the escape hatch for unsupported tools.

Any script can emit normalized events into a configured JSONL file. This gives Tokemon an extension mechanism without a plugin runtime.

### Later Candidates

- Gemini CLI
- Aider
- Cursor
- Continue
- Cline
- Roo Code
- Windsurf
- OpenRouter
- LiteLLM
- Ollama
- LM Studio

These are roadmap candidates, not promises.

---

## 16. Model Catalog and Tokedex

The **Tokedex** is the model catalog and tier-list view.

The source of truth is a static YAML file bundled with Tokemon. Users may mount an override.

Do not build:

- A catalog editor
- Remote synchronization
- Pricing history
- Weighted ranking categories
- Crowd voting

Example:

```yaml
schema_version: 1

models:
  claude-opus-4:
    provider: anthropic
    display_name: Claude Opus 4
    aliases:
      - claude-opus-4
      - anthropic/claude-opus-4
    tier: S
    input_price_per_million: 15
    output_price_per_million: 75
    cache_read_price_per_million: 1.5
    cache_write_price_per_million: 18.75

  gpt-5-codex:
    provider: openai
    display_name: GPT-5 Codex
    aliases:
      - gpt-5-codex
      - openai/gpt-5-codex
    tier: S
    input_price_per_million: null
    output_price_per_million: null
```

### Model Fields

- Canonical ID
- Provider
- Display name
- Aliases
- Tier
- Input price
- Output price
- Optional cache prices

Usage events retain the original raw model name even if canonical mapping changes.

### Tier List

Use only:

- S
- A
- B
- C
- D
- Unranked

Each model has one default tier in v0.2.

---

## 17. Cost Estimation

Support simple API-style estimation:

```text
input tokens × input price
+ output tokens × output price
+ cache-read tokens × cache-read price
+ cache-write tokens × cache-write price
```

Every calculated value displays an **Estimated** badge.

Unknown model:

```text
Cost unavailable
Model not found in models.yaml
```

Do not implement:

- Currency conversion
- Subscription economics
- Effective cost per million
- Break-even calculations
- Historical prices

Subscription products must not pretend to have exact per-token cost.

---

## 18. Dashboard Information Architecture

Navigation:

```text
Overview | Tokedex | Settings
```

`Settings` may initially be configuration documentation instead of a full interface.

### Overview Order

1. Global filters
2. Lifetime token odometer
3. Current Tokemon and evolution progress
4. Summary metrics
5. Usage timeline
6. Model breakdown
7. Machine status
8. Recent sessions

### Filters

```text
Time Range | Machine | Provider | Model | Tool
```

Time ranges:

- 24H
- 7D
- 30D
- All

Custom ranges can wait.

The Tokemon form is always based on unfiltered lifetime usage. Filters must not make it temporarily devolve.

---

## 19. Dashboard Layout

```text
┌──────────────────────────────────────────────────────────────────────┐
│ TOKEMON                  7D  30D  ALL    ALL MACHINES       TODEX ⚙ │
├──────────────────────────────────────────────────────────────────────┤
│                                                                      │
│      [ CURRENT TOKEMON ]              1,482,938,221                  │
│         TOKEN TITAN                  LIFETIME TOKENS                 │
│                                                                      │
│       Stage 9 · 48.3% toward next evolution · updated 23s ago       │
│       ███░░░░░░░░░░░░░░░░░░░                                        │
│                                                                      │
├───────────────────────────────────────────────┬──────────────────────┤
│ TOKEN USAGE                                   │ FAVOURITE MODEL      │
│                                               │                      │
│             ▁▂▂▃▅▄▆█▅▃                        │ Claude Opus          │
│                                               │ 48.2M tokens         │
├───────────────────────────────────────────────┼──────────────────────┤
│ MODELS                                        │ MACHINES             │
│                                               │                      │
│ Claude Opus      48.2M   51%   $202           │ ● mac-server         │
│ GPT-5 Codex      31.4M   33%   $104           │ ● laptop             │
│ DeepSeek          8.1M    9%   $7             │ ○ old-laptop         │
├───────────────────────────────────────────────┴──────────────────────┤
│ RECENT SESSIONS                                                      │
│ 14:42  laptop      codex     gpt-5-codex       824k    $1.12        │
│ 14:08  mac-server  claude    opus                 1.2M    $3.41      │
└──────────────────────────────────────────────────────────────────────┘
```

---

## 20. Lifetime Counter

The lifetime number remains the strongest visual element.

It should:

- Use tabular or monospace numerals
- Animate from the previously rendered value
- Use a subtle odometer transition
- Add thousands separators
- Avoid continuous animation
- Avoid flashing
- Avoid sound
- Show `updated 23s ago`
- Fit on mobile without truncation

Default label:

> **Lifetime Tokens**

The creature and number should read as one unit: the usage total explains why this form exists.

The counter subtext should give Input and Output equal-width mini split-flap counters using full comma-separated values. Output should use the amber accent so it remains legible even when Input is much larger. Hovering or focusing either value reveals the same exact token total. Cache detail belongs in the Cache hit summary below. Unknown token fields remain preserved in storage and analytics but are intentionally omitted from the overview hero.

---

## 21. Core Analytics

Keep analytics focused:

- Tokens by day
- Tokens by model
- Tokens by provider
- Tokens by machine
- Tokens by privacy-safe project label, merged across machines
- Tokens by tool
- Input versus output tokens
- Cache usage
- Estimated cost
- Sessions per day
- Average tokens per session
- Most-used model
- Largest session
- Highest-token day
- Current evolution stage
- Tokens until next evolution

### Light Tokemon Copy

Use sparingly:

- **Current Form** — current evolution
- **Next Evolution** — next threshold
- **Favourite Model** — most-used model
- **Training Ground** — most active machine
- **Biggest Battle** — largest session
- **Power Level** — lifetime tokens

The default analytics labels should still be understandable without knowing the joke.

---

## 22. Charts and Tables

### Timeline

Use one line or stacked-area chart.

- X-axis: time
- Y-axis: tokens
- Default: total usage
- Optional grouping by model
- Maximum five visible model series
- Group the rest as `Other`
- Use hover tooltips
- Avoid rainbow charts

### Model Breakdown

Prefer a table over a pie chart.

```text
Model             Tier    Tokens    Share    Sessions    Est. Cost
Claude Opus       S       48.2M     51%      412         $202
GPT-5 Codex       S       31.4M     33%      281         $104
DeepSeek          A        8.1M      9%       91           $7
```

### Input and Output Split

```text
Input   ███████████████████░░░░  81%
Output  ████                     19%
```

---

## 23. Recent Sessions

Show:

- Start time
- Machine
- Tool
- Provider
- Model
- Input tokens
- Output tokens
- Total tokens
- Estimated cost
- Duration
- Accuracy indicator

Never show:

- Prompt text
- Response text
- Conversation title
- Repository path
- Source code

---

## 24. Machines

Each machine shows:

- Name
- Operating system
- Architecture
- Agent version
- Last seen
- Detected adapters
- Usage today
- Lifetime usage
- Connection status

Statuses:

- Connected
- Stale
- Offline

Do not build fleet management.

The app has one global Tokemon in v0.2. Per-machine creatures are a later possibility and must not delay the MVP.

---

## 25. Visual Design

The interface is a quiet technical dashboard containing one evolving creature.

Think:

> A polished observability tool that accidentally hatched something.

### Visual Personality

- Dark, nearly black background
- Warm off-white text
- One muted accent per Tokemon stage
- Restrained red for warnings or spend
- Green only for connected machines
- Monospace or tabular numerals
- Clean sans-serif labels
- Thin borders
- Almost no shadows
- Generous spacing
- Minimal animation

Avoid:

- Glassmorphism
- Giant gradients
- Excessive glow
- Nested cards
- Confetti
- Particle explosions
- Battle-screen chrome
- Franchise imitation
- Constantly bouncing mascots
- Sound effects

The later forms may look increasingly alarming, but the dashboard itself should remain calm.

### Typography

- Interface: system sans-serif
- Numbers and code: system monospace

No custom font download should be required.

### Colour Tokens

```css
:root {
  --bg: #11110f;
  --surface: #181815;
  --surface-raised: #1f1f1b;
  --text: #eee9df;
  --text-muted: #9f9a90;
  --border: #333129;
  --accent: #8aa18a;
  --danger: #b85c50;
  --success: #5f8f69;
}
```

The exact values may change. Accessibility and consistency matter more than specific hex codes.

### Creature Placement

- Desktop: beside the lifetime counter
- Mobile: above the counter
- Never obscure analytics
- Never consume more than roughly one-third of the first viewport
- Use a restrained idle state, or none
- Support `prefers-reduced-motion`

---

## 26. Responsive Design

Mobile order:

1. Current Tokemon
2. Lifetime tokens
3. Evolution progress
4. Today and week summary
5. Usage timeline
6. Top models
7. Machines
8. Recent sessions

Tables become stacked rows.

The lifetime number must never overflow.

The evolution asset should remain crisp and lightweight.

---

## 27. Empty and Error States

### No Machines

```text
No Tokemon has hatched yet.

Run this on your first machine:

tokemon agent --server http://your-server:8080
```

### No Supported Tools

```text
No usage logs found.

Checked:
~/.claude
~/.codex

Run tokemon discover --verbose for details.
```

### Unknown Pricing

```text
Cost unavailable
Model not found in models.yaml
```

### Agent Offline

```text
Agent has not checked in for 18 minutes.
Last successful sync: 14:42
```

### Partial Data

```text
Some records do not expose exact token counts.
94% reported · 6% estimated
```

### Zero Tokens

Show the Egg form and:

```text
Your Tokemon is waiting for its first token.
```

Empty charts must always explain what is missing.

---

## 28. Privacy and Security

### Never Collected by Default

- Prompt text
- Response text
- Source code
- Repository contents
- Conversation titles
- User messages
- Repository paths
- Unrelated files

### Collected

- Token counts
- Model
- Provider
- Tool
- Timestamp
- Session identifier
- Machine identifier
- Duration when available
- Cost when provided
- Adapter version
- Accuracy classification

### Requirements

- Shared ingest token
- Hashed token storage where applicable
- Read-only source access
- Configurable source allowlist
- TLS when remotely exposed
- Local-only operation
- Exact outgoing-payload inspection
- Database deletion
- No maintainer telemetry by default

The `inspect` command makes privacy verifiable rather than merely promised.

---

## 29. API

```text
POST /api/v1/events/batch
POST /api/v1/agents/heartbeat

GET  /api/v1/analytics/overview
GET  /api/v1/analytics/timeline
GET  /api/v1/evolution
GET  /api/v1/models
GET  /api/v1/machines
GET  /api/v1/sessions
GET  /api/v1/catalog
```

### Evolution Response

```json
{
  "lifetime_tokens": 1482938221,
  "stage": 9,
  "form": "token-titan",
  "lower_threshold": 1000000000,
  "next_threshold": 2000000000,
  "tokens_remaining": 517061779,
  "progress": 0.482938221
}
```

The server should calculate this from aggregate usage, not persisted progression state.

### Batch Ingestion

Support:

- Shared-token authentication
- JSON
- Optional gzip compression
- Idempotency
- Whole-batch validation before SQLite mutation (a rejected event rejects the
  complete request)
- Maximum batch size
- 10 MiB compressed request, 8 MiB decompressed request, 4 MiB serialized
  event batch, and 1,000 events
- Bounded field/string/metadata sizes; finite non-negative costs; bounded
  timestamps; uppercase three-letter currencies; and consistent known token
  components. Unknown provider values remain valid unknowns.
- Accepted, duplicate, and rejected counts
- Previous and new lifetime totals
- Whether an evolution threshold was crossed

Example:

```json
{
  "accepted": 142,
  "duplicates": 3,
  "rejected": 1,
  "previous_stage": 8,
  "current_stage": 9,
  "evolved": true
}
```

### Agent Heartbeat

The heartbeat is authenticated with the same ingest credential and contains
deployment metadata only:

```json
{
  "machine_id": "linux-box",
  "agent_version": "0.3.0",
  "operating_system": "linux",
  "architecture": "arm64",
  "adapters": [{"id": "codex", "version": "0.6.0"}],
  "source_count": 4,
  "source_error_count": 0
}
```

---

## 30. Database Entities

### `machines`

- `id`
- `name`
- `operating_system`
- `architecture`
- `agent_version`
- `detected_adapters`
- `source_count`
- `source_error_count`
- `first_seen_at`
- `last_seen_at`

### `usage_events`

- `event_id`
- `timestamp`
- `machine_id`
- `provider`
- `raw_model`
- `canonical_model`
- `tool`
- `input_tokens`
- `output_tokens`
- `cache_read_tokens`
- `cache_write_tokens`
- `reasoning_tokens`
- `total_tokens`
- `cost`
- `currency`
- `session_id`
- `duration_ms`
- `token_accuracy`
- `adapter`
- `adapter_version`

### Local `agent_state`

- `source_path`
- `source_identity`
- `adapter`
- `cursor`
- `last_successful_sync`
- `machine_id`

Do not add an `evolutions` table in v0.2. Evolution is derived from lifetime token totals.

The browser may keep the last-seen stage locally to avoid replaying an evolution animation.

---

## 31. Data Retention and Portability

Users can:

- Back up SQLite
- Delete SQLite
- Export normalized JSONL
- Import normalized JSONL
- Purge old records from the CLI

Evolution is recalculated after imports, deletions, or purges.

If deleting historical data lowers the lifetime total, the Tokemon may move to the corresponding lower stage. This is mathematically correct and should not be treated as an error.

Do not build retention-policy screens.

---

## 32. Repository Structure

```text
tokemon/
├── cmd/
│   └── tokemon/
├── internal/
│   ├── adapters/
│   │   ├── claude/
│   │   ├── codex/
│   │   ├── builtin/
│   │   └── genericjsonl/
│   ├── agent/
│   ├── analytics/
│   ├── api/
│   ├── catalog/
│   ├── database/
│   ├── evolution/
│   ├── version/
│   ├── ingestion/
│   ├── privacy/
│   └── server/
├── web/
│   ├── templates/
│   └── static/
│       └── tokemon/
│           ├── stage-00.svg
│           ├── stage-01.svg
│           ├── stage-02.svg
│           └── ...
├── catalog/
│   └── models.yaml
├── migrations/
├── deploy/
│   ├── docker-compose.yml
│   ├── install-agent.sh
│   ├── build-release.sh
│   ├── systemd/
│   └── launchd/
├── docs/
├── go.mod
├── go.sum
└── README.md
```

The `evolution` package should contain pure calculations and form metadata, not a game framework.

---

## 33. v0.2 Acceptance Criteria

Tokemon v0.2 is ready when:

1. The server starts through Docker Compose.
2. The same binary runs as an agent on at least two machines.
3. The agent detects Claude Code and Codex usage locations.
4. Generic JSONL accepts documented normalized events.
5. New records sync incrementally.
6. Failed uploads do not advance cursors.
7. Rescanning does not create duplicates.
8. The dashboard shows lifetime tokens.
9. The current stage is correctly derived from lifetime tokens.
10. Crossing any configured evolution threshold changes the form.
11. The dashboard shows tokens remaining until the next evolution.
12. Historical imports select the correct form without replaying every stage.
13. The current Tokemon uses an original local raster asset.
14. Reduced-motion users receive no forced evolution animation.
15. Filter changes do not alter the global lifetime form.
16. Usage is viewable over 24 hours, 7 days, 30 days, and all time.
17. Usage can be filtered by machine, provider, model, and tool.
18. Models and machines have useful breakdowns.
19. Recent sessions contain no conversation content.
20. Costs are estimated from mounted YAML.
21. Unknown pricing remains visibly unknown.
22. Accuracy is reported as reported, derived, estimated, or unknown.
23. The Tokedex renders from YAML.
24. `tokemon inspect` shows the exact outgoing payload.
25. No prompt or response content leaves the machine by default.
26. Data can be exported and re-imported as JSONL.
27. The complete system remains one server container plus one native binary per machine.

---

## 34. Release Plan

### v0.2 — It Hatched

- Rename product to Tokemon
- Single Go binary
- Server and agent modes
- SQLite
- Claude Code adapter
- Codex adapter
- Generic JSONL adapter
- Incremental polling
- Idempotent ingestion
- One-page overview
- Lifetime token odometer
- Global Tokemon
- Power-of-ten evolution through 1B, followed by `1–2–5` checkpoints
- Evolution progress
- Original stage raster assets
- Usage timeline
- Model breakdown
- Machine status
- Recent sessions
- Static model catalog
- Tokedex tier list
- Estimated API cost
- JSONL import and export
- Privacy inspection command

### v0.3 — A Wild Provider Appears

Only after v0.2 is stable:

- Additional adapters
- Better model-catalog coverage
- CSV export
- Per-agent tokens
- Configurable terminology
- Basic usage budgets
- Usage-spike alerts
- Agent-offline alerts
- Evolution-history strip

### Later — Final Form

Only if users genuinely request them:

- Subscription ROI
- Local-model throughput
- Optional Git metadata
- Privacy-safe share cards
- More tier categories
- Pricing history
- Multi-user access
- External adapter SDK
- Per-machine Tokemon

---

## 35. Nice-to-Have Ideas That Must Not Delay v0.2

### Evolution History

A compact strip of previously unlocked forms.

No encyclopedia, lore database, or achievement engine is needed.

### Share Card

```text
My Tokemon evolved into:
TOKEN TITAN

1,482,938,221 lifetime tokens
Favourite model: Claude Opus

This is probably fine.
```

### Optional Per-Machine Forms

Each machine could eventually have its own Tokemon based on machine-local lifetime usage.

This must not replace the single global Tokemon in v0.2.

### Local Models

Track:

- Generated tokens
- Tokens per second
- GPU time
- Model
- Machine

### Budgets and Alerts

- Daily token budget
- Monthly cost budget
- Usage-spike alerts
- New-model alerts
- Agent-offline alerts

No alert is required for evolution; seeing it on the dashboard is enough.

---

## 36. Final Product Definition

**Tokemon is a tiny, self-hosted, multi-machine, provider-agnostic token analytics dashboard whose mascot evolves at deterministic lifetime-usage checkpoints.**

Its first proper release remains intentionally boring underneath:

- One Go binary
- One server container
- Native polling agents
- SQLite
- Three adapters
- One YAML catalog
- One derived evolution threshold table
- A small set of original raster forms
- One excellent dashboard

No React application. No Postgres. No Redis. No plugin runtime. No user-account system. No live sockets. No game engine.

The product should be trustworthy first, useful immediately, and increasingly unhinged only in the appearance of the creature you have fed.
