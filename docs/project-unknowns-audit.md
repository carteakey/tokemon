# Project unknowns audit — 2026-11-04

Task 1f471778 · zcodebee2 · docs + findings only (no code changes).
Reviewed at main `8ef69a0`: adapters → usage → database → dashboard templates,
cross-checked against `docs/tokemon-spec-v0.2.md`.

## Spec baseline (what "unknown" is supposed to mean)

`docs/tokemon-spec-v0.2.md:713-730` defines the taxonomy — Reported / Derived /
Estimated / **Unknown** ("unavailable and not safely inferable") — with two hard
rules: "Never blend these silently" and "Unknown values remain unknown rather
than becoming zero." The audit measures the implementation against that bar.

## Findings

### 1. HIGH — Projects breakdown silently drops unknown-project usage

`internal/database/database.go:1306-1309` (`Store.Overview`):

```sql
SELECT project, … FROM usage_events WHERE project <> '' GROUP BY project …
```

Events with an empty project are excluded from the dashboard's Projects table —
not shown as "Unknown project", simply absent. Meanwhile the flagship Claude
adapter **never sets Project at all** (see finding 2), so for Claude Code usage:

- the Projects table omits the majority of real-world usage;
- Share percentages in that table are computed against **lifetime totals that
  include the dropped events** (`{{share .Tokens $.LifetimeTokens}}`), so the
  displayed shares understate and cannot sum to 100%, with no explanation;
- the same events DO appear in the Recent Sessions table as "Unknown project"
  rows — the two surfaces disagree about the same data.

**Recommendation:** bucket empty project as an explicit row (label
"Unknown project") or add a footnote row with the dropped token count; pick one
semantics and document it in the spec's project-field section. Small DB +
template change with a regression test.

### 2. MEDIUM — Claude adapter never reports Project

`internal/adapters/claude/claude.go:312-329`: the emitted `usage.Event` has no
`Project` field. Every other adapter (codex, copilot, opencode, antigravity,
openclaw, hermes, zcode) sets one. Since Claude Code is the primary harness,
most real dashboards will show mostly/only "Unknown project" sessions.

Cheap, privacy-safe improvement: Claude Code stores JSONL under
`~/.claude/projects/<encoded-project>/…` and the adapter already opens that
directory (`claude.go:33`), so the project folder name could be derived from the
source path and normalized via `usage.NormalizeProject`. Folder name only — no
conversation content — consistent with the privacy model. **Needs spec
sign-off** before implementation; this audit only flags it.

### 3. MEDIUM — Unpriced tokens are computed but never surfaced

`database.go` computes and stores `EstimatedCost.PricedTokens` and
`EstimatedCost.UnpricedTokens`. The UI:

- overview stat (`server.go:2128`): shows the money amount only;
- analytics stat (`server.go:3183`): shows "Priced tokens: N" — never the
  unpriced count;
- `UnpricedTokens` appears in **zero** templates.

A cost estimate over partially-priced usage is exactly the silent blending the
spec forbids ("Never blend these silently"). Catalog gaps — models missing from
`catalog/models.yaml` — are invisible to the user.

**Recommendation:** disclose coverage in the stat-meta, e.g.
"Priced 6.4M · unpriced 0.4M" or a percentage; one-line template change plus
whatever test coverage the formatter needs.

### 4. MEDIUM — Literal model "unknown" enters aggregates as a first-class model

`claude.go:310` falls back to `model = "unknown"` when a record's model can't
be parsed. Nothing downstream special-cases it: it groups as a model named
"unknown" in the Models breakdown, appears in Tokedex as if it were a catalog
model, displays via `compactModelName("unknown")` → "unknown", and is
permanently unpriced (not in the catalog).

**Recommendation:** keep the event and its tokens counted (that part follows
the spec), but render it as "Unknown model" in breakdowns and exclude it from
catalog-backed surfaces (Tokedex cards, pricing provenance), with the unpriced
disclosure from finding 3 making the gap visible.

### 5. LOW — Terminology drift for missing values

`server.go` template helpers: `tokenValue(nil)` → "Unknown" (952-957),
`durationValue(nil)` → "Unknown" (972-977), but `costValue(nil)` → "Unavailable"
(958-968). Sessions table: "Unknown project". Analytics note: "unavailable
token totals … remain visible as unknown". Export tooltip: "unknown totals
present". Spec:718 defines **Unknown** as the umbrella term for
"unavailable and not safely inferable".

**Recommendation:** standardize on the spec vocabulary (or document the
distinction between "unknown data" and "unavailable service"); add the chosen
glossary to the spec's dashboard section.

### 6. LOW — Unknown markers are visual-only and undocumented

The dashboard has a coherent visual language for unknowns — amber borders and
markers on heatmap cells, trend columns, share columns (`server.go:1936`,
`2955`, `2971`, `3028`), the unknown-events banner (`3185`), the "?" flag — but
neither README, DEPLOYMENT, SECURITY, nor the spec's dashboard section explains
it. The spec covers the philosophy (713-730) and unknown pricing (1285) but not
the concrete UI affordances.

**Recommendation:** one short paragraph ("How the dashboard marks unknowns")
in the spec's dashboard section.

### 7. INFO — Session naming cross-reference

Sessions without a `sessionId` fall back to `usage.HashSessionID(source.Path)`
(`claude.go:167`), so hashed file-path IDs surface in the sessions table. This
overlaps task 04d474ee (session naming, assigned to zcodebee1) — noted here as
a cross-reference only.

## Catalog gap snapshot

`catalog/models.yaml` (schema v2): 12 models — GPT-5 family (codex, 5.2/5.3,
5.4, 5.4-mini, …) and Claude opus-4/4.5-plus, sonnet-4, haiku-4.5 with alias
families. Anything outside the alias set (version bumps, new releases, the
literal "unknown") displays via `compactModelName` and accumulates into
`UnpricedTokens` invisibly (see findings 3-4). No gap alert exists anywhere.

## Suggested follow-ups (out of scope for this audit)

1. Unknown-project bucket in `Store.Overview` + template + test (finding 1).
2. Claude adapter project derivation from the `.claude` projects path — needs
   spec sign-off (finding 2).
3. Priced/unpriced coverage disclosure on both cost stats (finding 3).
4. Vocabulary standardization + spec glossary (findings 5-6).
