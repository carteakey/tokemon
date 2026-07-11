# Token Bandit

> **Your coding agents have a gambling problem.**

A lightweight, self-hosted dashboard for tracking token usage across AI coding tools, models, providers, and machines.

Token Bandit discovers local usage records from tools such as Claude Code and Codex, normalizes them into a common format, and sends usage metadata from multiple machines to one central dashboard.

The application should be:

- Self-hosted
- Privacy-first
- Lightweight
- Easy to deploy
- Useful with almost no configuration
- Extensible across providers
- Slightly casino-themed without becoming visually obnoxious

Token Bandit must never collect prompt text, response text, source code, or repository contents by default. It processes usage metadata only.

---

## 1. Product Positioning

Token Bandit is a tiny, self-hosted analytics application that answers four questions:

1. How many tokens am I using?
2. Where are they being used?
3. Which models are consuming the most?
4. Exactly how bad has my AI coding habit become?

The product should feel like:

> Plausible Analytics for AI coding usage, with one absurd casino centrepiece.

Its humour should come primarily from restrained copy, not decorative excess.

---

## 2. Product Principles

1. **Metadata, not conversations.**
2. **One binary should be enough.**
3. **SQLite until SQLite is genuinely insufficient.**
4. **Provider quirks belong inside adapters.**
5. **Unknown values remain unknown.**
6. **Every number should explain where it came from.**
7. **The dashboard should be fun, but the data should be serious.**
8. **Adding a provider should not require redesigning the system.**
9. **Boring infrastructure is a feature.**
10. **No component should exist merely because it may be useful later.**

---

## 3. Strict MVP Scope

The first release consists of:

- One Go binary
- `serve`, `agent`, `discover`, `inspect`, `import`, and `export` commands
- One Docker Compose service for the server
- Native agents on each machine
- SQLite
- Claude Code adapter
- Codex adapter
- Generic JSONL adapter
- Incremental polling
- Idempotent ingestion
- One-page dashboard
- All-time token counter
- Daily usage chart
- Usage by model
- Usage by machine
- Recent sessions
- Static YAML model catalog
- Simple model tier list
- Simple estimated API cost
- No prompt or response collection

The MVP should feel like:

> One server, one tiny agent, three adapters, one database, and one excellent dashboard.

---

## 4. Non-Goals

The MVP will not:

- Proxy model requests
- Replace LiteLLM
- Inspect source code
- Store prompts or responses
- Evaluate response quality
- Score developers
- Track employee productivity
- Require Kubernetes
- Require Postgres
- Require Redis
- Require a JavaScript-heavy frontend
- Implement WebSockets or Server-Sent Events
- Implement external runtime plugins
- Implement user accounts
- Implement organization-level billing
- Implement role-based access control
- Calculate exact subscription-token costs
- Perform currency conversion
- Maintain pricing history
- Provide subscription ROI calculations
- Provide a model-catalog administration interface
- Synchronize the model catalog remotely
- Recursively scan a user's entire home directory
- Provide a retention-management interface
- Support every AI provider in the first release

---

## 5. Recommended Technology

### Language

Use **Go** for both the server and agent.

Reasons:

- Easy cross-compilation
- Single static binary
- Low memory usage
- Fast startup
- Straightforward concurrency
- Good filesystem support
- Simple deployment
- Easier contributor experience than Rust
- One language across the whole project

Rust remains a valid future option for specialized parsers, but Go is the better MVP choice.

### Backend

- Go
- SQLite
- Embedded database migrations
- REST API
- Embedded static assets and HTML templates

### Frontend

Prefer:

- Go templates
- HTMX only where interaction materially benefits
- A lightweight charting library
- Minimal custom JavaScript

Do not introduce React, a frontend monorepo, or a complex build pipeline unless the dashboard eventually outgrows server-rendered HTML.

### Refresh Model

Do not use live sockets.

Refresh analytics:

- On page load
- When filters change
- Automatically every 60 seconds

That is sufficiently live for token analytics.

---

## 6. Single-Binary CLI

Use one binary for all server and agent functions.

```bash
token-bandit serve
token-bandit agent
token-bandit discover
token-bandit inspect
token-bandit import usage.jsonl
token-bandit export usage.jsonl
```

### Commands

#### `token-bandit serve`

Starts:

- REST API
- Dashboard
- SQLite database
- Embedded model catalog

#### `token-bandit agent`

Starts the machine agent and periodically scans configured sources.

#### `token-bandit discover`

Finds supported local tools and reports their detected paths.

Example:

```text
✓ Claude Code detected at ~/.claude
✓ Codex detected at ~/.codex
– Gemini CLI not detected
– OpenCode not detected
```

#### `token-bandit inspect`

Prints exactly what the agent would send to the server.

This is a core privacy feature.

#### `token-bandit import`

Imports normalized JSONL usage events.

#### `token-bandit export`

Exports normalized JSONL usage events.

CSV import and export are not required for the MVP.

---

## 7. Deployment

### Central Server

The server should run as a single Docker container.

```yaml
services:
  token-bandit:
    image: ghcr.io/token-bandit/token-bandit:latest
    command: serve
    ports:
      - "8080:8080"
    volumes:
      - ./data:/data
      - ./models.yaml:/config/models.yaml:ro
    environment:
      TOKEN_BANDIT_DATABASE: /data/token-bandit.db
      TOKEN_BANDIT_INGEST_TOKEN: change-me
      TOKEN_BANDIT_MODEL_CATALOG: /config/models.yaml
    restart: unless-stopped
```

### Machine Agents

Agents can run as:

- A native binary
- A systemd service
- A launchd service
- A Docker container with read-only mounts

Native installation should be preferred because local dotfiles and application data are easier to access safely.

### Authentication

For the MVP:

- Use one shared ingest token for all agents.
- Do not implement dashboard accounts.
- Recommend Tailscale, Cloudflare Access, or reverse-proxy authentication when the dashboard is exposed beyond a trusted network.

A generated machine ID is sufficient for identifying agents.

Per-agent credentials and revocation can be added later.

---

## 8. Architecture

```text
┌─────────────────────┐
│ Laptop              │
│ token-bandit agent  │
│ Claude / Codex logs │
└──────────┬──────────┘
           │ normalized usage events
           ▼
┌─────────────────────┐
│ Token Bandit Server │
│ API + SQLite + UI   │
└──────────▲──────────┘
           │
┌──────────┴──────────┐
│ Homelab Machine     │
│ token-bandit agent  │
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

### Server Responsibilities

The server:

- Accepts normalized usage events
- Authenticates ingestion requests
- Deduplicates records
- Stores usage data
- Calculates simple estimated costs
- Loads model metadata from YAML
- Serves the dashboard
- Supports JSONL import and export

---

## 9. Polling and Cursor Model

Do not introduce filesystem watchers in the MVP.

Every 60 seconds, the agent should:

1. Inspect known source paths.
2. Find files supported by an adapter.
3. Read from the last recorded cursor.
4. Normalize new records.
5. Send a batch to the server.
6. Save the new cursor only after a successful upload.

Usage logs already act as durable storage, so the MVP does not need a separate local event spool.

The local agent state only needs:

- File path
- File identity
- Last byte offset or logical cursor
- Last successful sync
- Machine ID

If an upload fails:

- Do not advance the cursor.
- Retry on the next poll.

Server-side deterministic event IDs prevent duplicate ingestion.

### File Rotation

The cursor implementation should detect:

- File truncation
- File replacement
- Log rotation
- Missing source files

A new file identity should reset the cursor safely without duplicating old events.

---

## 10. Filesystem Discovery

The agent should inspect known locations such as:

```text
~/.claude/
~/.codex/
~/.config/
~/.local/share/
~/Library/Application Support/
%APPDATA%
```

Each adapter defines its own discovery paths and supported file patterns.

The agent must not recursively scan the entire home directory by default.

Users can add explicit paths:

```yaml
server:
  url: https://tokens.example.com
  token: ${TOKEN_BANDIT_INGEST_TOKEN}

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
  path: ~/.local/share/token-bandit/state.db
```

The local state database stores cursors and machine state only, not a second copy of every usage event.

---

## 11. Normalized Usage Format

All adapters emit the same normalized event format.

```json
{
  "schema_version": "1",
  "event_id": "sha256:...",
  "timestamp": "2026-07-11T18:42:00Z",
  "machine_id": "mac-server",
  "session_id": "optional-provider-session-id",
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
- `provider`
- `model`
- `tool`
- `token_accuracy`

Token and duration fields may be `null` when a provider does not expose them.

### Event Identity

Each event must have a deterministic ID based on stable source fields.

A typical input might include:

- Machine ID
- Adapter ID
- Source-file identity
- Provider event ID, when available
- Record offset
- Timestamp
- Session ID

This allows safe rescanning without duplicate records.

---

## 12. Token Accuracy

Token Bandit must distinguish three kinds of values:

- **Reported** — explicitly recorded by the provider or tool
- **Derived** — calculated from provider-reported component fields
- **Estimated** — approximated because exact data was unavailable

Never blend these silently.

The dashboard should show a compact data-quality indicator:

```text
94% reported · 6% estimated
```

Possible per-record values:

```text
reported
derived
estimated
unknown
```

Unknown fields must remain unknown rather than being replaced with zero.

---

## 13. Provider Adapter System

Provider support should use clean, internal Go adapters.

Do not implement external runtime plugins in the MVP. External plugins create distribution, compatibility, versioning, and security problems too early.

Conceptual interface:

```go
type Adapter interface {
    ID() string
    Discover(ctx context.Context) ([]Source, error)
    Parse(ctx context.Context, source Source, cursor Cursor) ([]UsageEvent, Cursor, error)
    NormalizeModel(rawModel string) string
    Capabilities() Capabilities
}
```

Capabilities:

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

- Adapter ID
- Display name
- Discovery paths
- Supported file patterns
- Parser
- Cursor strategy
- Deduplication inputs
- Model-name normalization
- Exposed capabilities
- Token-accuracy level

### MVP Adapters

Ship only:

1. Claude Code
2. Codex
3. Generic JSONL

### Generic JSONL Adapter

The generic JSONL schema is the escape hatch for unsupported tools.

Any external script can produce normalized events and place them in a configured file.

This keeps Token Bandit extensible without building a plugin runtime.

### Compatibility Roadmap

Potential later adapters:

- Gemini CLI
- OpenCode
- Aider
- Cursor
- Continue
- GitHub Copilot CLI
- Cline
- Roo Code
- Windsurf
- OpenRouter
- LiteLLM
- Ollama
- LM Studio

These are roadmap candidates, not MVP promises.

---

## 14. Model Catalog

The model catalog is a static YAML file bundled with the application.

Users may mount their own file to override it.

Do not build:

- A catalog editor
- Remote catalog synchronization
- Pricing history
- Complex ranking categories

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

### MVP Model Fields

Each model needs only:

- Canonical ID
- Provider
- Display name
- Aliases
- Tier
- Input price
- Output price
- Optional cache prices

Usage records must retain the original raw model name even when the canonical mapping changes.

---

## 15. Model Tier List

The tier list should remain deliberately simple:

- S
- A
- B
- C
- D
- Unranked

For the MVP:

- Each model has one default tier.
- The tier list renders directly from `models.yaml`.
- No drag-and-drop editor is required.
- No weighted scoring or category system is required.
- No crowdsourced ranking is required.

The tier-list page is the only page besides the main dashboard.

More specific categories such as agentic ability, value, local models, or raw coding ability can be added later if they prove useful.

---

## 16. Cost Estimation

The MVP should support only simple API-style cost estimation:

```text
input tokens × input price
+ output tokens × output price
+ cache-read tokens × cache-read price
+ cache-write tokens × cache-write price
```

Every calculated cost should show an **Estimated** badge.

When a model is missing from the catalog:

```text
Cost unavailable
Model not found in models.yaml
```

Do not implement:

- Currency conversion
- Subscription-plan economics
- Effective cost per million
- Break-even calculations
- Monthly utilization scoring
- Historical pricing changes

Subscription products should not pretend to have exact per-token costs.

---

## 17. Dashboard Information Architecture

The MVP has three navigation items:

```text
Overview | Tier List | Settings
```

`Settings` may initially be configuration-file documentation rather than a full settings interface.

### One-Page Overview

Avoid separate pages for models, machines, providers, sessions, and overview.

The overview should contain:

1. Global filters
2. Jackpot counter
3. Summary metrics
4. Usage timeline
5. Model breakdown
6. Machine status
7. Recent sessions

### Global Filters

```text
Time Range | Machine | Provider | Model | Tool
```

Suggested time ranges:

- 24H
- 7D
- 30D
- All

Custom date ranges can wait.

---

## 18. Dashboard Layout

```text
┌────────────────────────────────────────────────────────────────────┐
│ TOKEN BANDIT                 7D  30D  ALL    ALL MACHINES      ⚙   │
├────────────────────────────────────────────────────────────────────┤
│                                                                    │
│                       1,482,938,221                                │
│                      ALL-TIME TOKENS                               │
│                                                                    │
│              $418 estimated     +18% this week                     │
│                                                                    │
├───────────────────────────────────────────────┬────────────────────┤
│ TOKEN USAGE                                   │ HOUSE FAVOURITE    │
│                                               │                    │
│            ▁▂▂▃▅▄▆█▅▃                         │ Claude Opus        │
│                                               │ 48.2M tokens       │
├───────────────────────────────────────────────┼────────────────────┤
│ MODELS                                        │ MACHINES           │
│                                               │                    │
│ Claude Opus      48.2M   51%   $202           │ ● mac-server       │
│ GPT-5 Codex      31.4M   33%   $104           │ ● laptop           │
│ DeepSeek          8.1M    9%   $7             │ ○ old-laptop       │
├───────────────────────────────────────────────┴────────────────────┤
│ RECENT SESSIONS                                                    │
│ 14:42  laptop      codex     gpt-5-codex       824k    $1.12      │
│ 14:08  mac-server  claude    opus                 1.2M    $3.41    │
└────────────────────────────────────────────────────────────────────┘
```

---

## 19. The Jackpot Counter

The all-time token counter is the signature element.

It should:

- Use tabular or monospace numerals
- Be the strongest visual element on the page
- Animate from the previous value when the page loads
- Use a subtle mechanical odometer transition
- Add thousands separators
- Avoid continuous spinning
- Avoid flashing
- Avoid sound
- Show a small timestamp such as `updated 23s ago`
- Fit on mobile without truncation

Default label:

> **All-Time Tokens Burned**

Other copy may appear elsewhere:

- The House Total
- Lifetime Wager
- Context Consumed
- Tokens Sent to the Void

Do not recreate a literal slot machine.

---

## 20. Core Analytics

Keep the initial analytics focused:

- Tokens by day
- Tokens by model
- Tokens by provider
- Tokens by machine
- Tokens by tool
- Input versus output tokens
- Cache usage when available
- Estimated cost
- Sessions per day
- Average tokens per session
- Most-used model
- Highest-token session
- Highest-token day

### Casino-Flavoured Labels

Use sparingly:

- **House Favourite** — most-used model
- **Biggest Bet** — largest session
- **High Roller** — most expensive model
- **Jackpot Day** — highest-token day
- **Biggest Whale** — machine with the most usage
- **Lucky Streak** — consecutive active days
- **Burn Rate** — average tokens per hour

A later Professional Mode may replace these labels with conventional analytics terminology.

---

## 21. Charts and Tables

### Main Timeline

Use one line or stacked-area chart.

- X-axis: time
- Y-axis: tokens
- Default: total token usage
- Optional grouping: model
- Maximum five visible model series
- Group the rest as `Other`
- Use hover tooltips
- Avoid legends with dozens of entries
- Avoid rainbow charts

### Model Breakdown

Prefer a table over a pie chart.

```text
Model             Tier    Tokens    Share    Sessions    Est. Cost
Claude Opus       S       48.2M     51%      412         $202
GPT-5 Codex       S       31.4M     33%      281         $104
DeepSeek          A        8.1M      9%       91           $7
```

Sparklines can be added later.

### Input and Output Split

Use a simple horizontal comparison rather than another chart:

```text
Input   ███████████████████░░░░  81%
Output  ████                     19%
```

---

## 22. Recent Sessions

The recent sessions table should contain:

- Start time
- Machine
- Tool
- Provider
- Model
- Input tokens
- Output tokens
- Total tokens
- Estimated cost
- Duration, when available
- Token-accuracy indicator

No prompt, response, conversation title, repository path, or source-code content should appear.

---

## 23. Machines

Each machine displays:

- Machine name
- Operating system
- Architecture
- Agent version
- Last seen
- Detected adapters
- Usage today
- Usage all time
- Connection status

Suggested statuses:

- Connected
- Stale
- Offline

Do not build a complex fleet-management interface.

---

## 24. Visual Design

The dashboard should be a quiet technical interface with one absurd casino centrepiece.

Think:

> Plausible Analytics meets a vintage slot-machine counter, designed by someone embarrassed about enjoying slot machines.

### Visual Personality

- Dark, nearly black background
- Warm off-white text instead of pure white
- Muted brass or gold accent
- Restrained red for warnings or spending
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
- Nested cards inside cards
- Animated coins
- Playing-card decorations
- Roulette-wheel loaders
- Confetti
- Slot-machine sound effects
- Flashing numbers

The product is funniest when it treats a ridiculous statistic with complete seriousness.

### Typography

Suggested pairing:

- Interface: system sans-serif
- Numbers and code: system monospace

Do not make users download custom fonts merely to run the application.

### Colour Approach

Use CSS variables so the palette remains easy to change:

```css
:root {
  --bg: #11110f;
  --surface: #181815;
  --surface-raised: #1f1f1b;
  --text: #eee9df;
  --text-muted: #9f9a90;
  --border: #333129;
  --accent: #b89a55;
  --danger: #b85c50;
  --success: #5f8f69;
}
```

The exact values can evolve during implementation. The design principle matters more than any specific hex code.

---

## 25. Responsive Design

Do not attempt to preserve the full desktop layout on mobile.

Mobile order:

1. Jackpot
2. Today and week summary
3. Usage timeline
4. Top models
5. Machines
6. Recent sessions

Tables should become compact stacked rows.

The jackpot number should use responsive typography and never overflow horizontally.

---

## 26. Empty and Error States

Self-hosted tools need useful empty states.

### No Machines

```text
No bets have been placed.

Run this on your first machine:

token-bandit agent --server http://your-server:8080
```

### No Supported Tools

```text
No usage logs found.

Checked:
~/.claude
~/.codex

Run token-bandit discover --verbose for details.
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
Some usage records do not expose exact token counts.
94% reported · 6% estimated
```

Empty charts should never appear without an explanation.

---

## 27. Privacy and Security

Privacy is a core product requirement.

### Never Collected by Default

- Prompt text
- Response text
- Source code
- Repository contents
- Conversation titles
- File contents unrelated to usage records
- User messages
- Repository paths
- Conversation filenames

### Collected

- Token counts
- Model name
- Provider
- Tool
- Timestamp
- Session identifier
- Machine identifier
- Duration, when available
- Cost, when explicitly provided
- Adapter version
- Token-accuracy classification

### Security Requirements

- Shared ingest token for the MVP
- Hashed token storage where applicable
- Read-only access to source paths
- Configurable source allowlist
- TLS expected when exposed remotely
- Local-only operation supported
- Ability to inspect outgoing payloads
- Ability to delete the SQLite database
- No analytics telemetry sent to the Token Bandit maintainers by default

The `inspect` command must make privacy verifiable rather than merely promised.

---

## 28. API

Minimum endpoints:

```text
POST /api/v1/events/batch
POST /api/v1/agents/heartbeat

GET  /api/v1/analytics/overview
GET  /api/v1/analytics/timeline
GET  /api/v1/models
GET  /api/v1/machines
GET  /api/v1/sessions
GET  /api/v1/catalog
```

### Batch Ingestion

The batch endpoint should support:

- Shared-token authentication
- JSON payloads
- Optional gzip compression
- Idempotent event ingestion
- Partial validation errors
- A maximum batch size
- Clear success and failure counts

A response may resemble:

```json
{
  "accepted": 142,
  "duplicates": 3,
  "rejected": 1
}
```

---

## 29. Database Entities

### `machines`

- `id`
- `name`
- `operating_system`
- `architecture`
- `agent_version`
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

### `agent_state`

Local to each machine:

- `source_path`
- `source_identity`
- `adapter`
- `cursor`
- `last_successful_sync`
- `machine_id`

The server does not need a separate ingestion-cursor system for the MVP.

---

## 30. Data Retention and Portability

For the MVP, users can:

- Back up the SQLite database
- Delete the database
- Export normalized JSONL
- Import normalized JSONL
- Purge old records with a CLI command

Example:

```bash
token-bandit purge --before 2026-01-01
```

Do not build retention-policy screens yet.

Token Bandit should never lock users into its internal database.

---

## 31. Repository Structure

```text
token-bandit/
├── cmd/
│   └── token-bandit/
├── internal/
│   ├── adapters/
│   │   ├── claude/
│   │   ├── codex/
│   │   └── genericjsonl/
│   ├── agent/
│   ├── analytics/
│   ├── api/
│   ├── catalog/
│   ├── database/
│   ├── ingestion/
│   ├── privacy/
│   └── server/
├── web/
│   ├── templates/
│   └── static/
├── catalog/
│   └── models.yaml
├── migrations/
├── deploy/
│   ├── docker-compose.yml
│   ├── systemd/
│   └── launchd/
├── docs/
├── go.mod
├── go.sum
└── README.md
```

---

## 32. MVP Acceptance Criteria

Token Bandit v0.1 is ready when:

1. A user can start the server with Docker Compose.
2. A user can run the same binary as an agent on at least two machines.
3. The agent can detect Claude Code and Codex usage locations.
4. The generic JSONL adapter accepts documented normalized events.
5. New usage records sync incrementally.
6. Failed uploads do not advance local cursors.
7. Rescanning does not create duplicate events.
8. The dashboard shows an all-time token counter.
9. The dashboard shows usage over 24 hours, 7 days, 30 days, and all time.
10. Usage can be filtered by machine, provider, model, and tool.
11. The dashboard shows model and machine breakdowns.
12. Recent sessions are visible without conversation content.
13. Costs are estimated from a mounted YAML catalog.
14. Unknown prices remain visibly unknown.
15. Token accuracy is identified as reported, derived, estimated, or unknown.
16. The tier-list page renders from YAML.
17. `token-bandit inspect` shows the exact outgoing payload.
18. No prompt or response content leaves a machine by default.
19. Usage data can be exported and re-imported as JSONL.
20. The complete system remains usable with one server container and one native binary per machine.

---

## 33. Release Plan

### v0.1 — The First Bet

- Single Go binary
- Server and agent modes
- SQLite
- Claude Code adapter
- Codex adapter
- Generic JSONL adapter
- Incremental polling
- Idempotent ingestion
- One-page overview
- All-time token jackpot
- Usage timeline
- Model breakdown
- Machine status
- Recent sessions
- Static model catalog
- Simple tier list
- Estimated API cost
- JSONL import and export
- Privacy inspection command

### v0.2 — Know When to Fold

Only after the MVP is stable:

- Additional adapters
- Better catalog coverage
- CSV export
- Per-agent tokens
- Configurable dashboard terminology
- Basic usage budgets
- Usage-spike alerts
- Agent-offline alerts

### Later — High Roller

Only if actual users request them:

- Subscription ROI
- Local model throughput
- Git metadata
- Public share cards
- More advanced tier categories
- Pricing history
- Multi-user access
- External adapter SDK

---

## 34. Nice-to-Have Ideas That Must Not Delay the MVP

These are explicitly outside v0.1:

### Budgets and Alerts

- Daily token budget
- Monthly cost budget
- Usage-spike alerts
- New-model alerts
- Agent-offline alerts

### Optional Git Metadata

Associate sessions with:

- Repository
- Branch
- Commit

This must remain opt-in and must not send repository paths by default.

### Session Comparison

Compare:

- Models used
- Token consumption
- Duration
- Estimated cost

Token Bandit must not imply that fewer tokens automatically means better work.

### Local Models

Track:

- Generated tokens
- Tokens per second
- GPU time
- Model
- Machine

### Privacy-Safe Share Cards

Example:

```text
This month I burned:
482,918,221 tokens

Top model: Claude Opus
Biggest day: July 8

The house always wins.
```

---

## 35. Final Product Definition

**Token Bandit is a tiny, self-hosted, multi-machine, provider-agnostic token-usage dashboard for AI coding tools.**

Its first version should be intentionally boring under the hood:

- One Go binary
- One server container
- Native polling agents
- SQLite
- Three adapters
- One YAML catalog
- One excellent dashboard

No React application. No Postgres. No Redis. No plugin runtime. No user-account system. No live sockets. No remote configuration platform.

The product should be trustworthy first, useful immediately, and funny only where the joke improves the experience.
