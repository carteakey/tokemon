# Tokedex roadmap — what "enabled" requires

Tokedex (the `/tokedex` model-catalog UI) is gated behind
`TOKEMON_ENABLE_TOKEDEX` / `--enable-tokedex` and ships **off by default**:
the page answers 404 and no navigation link advertises it until the work
below lands. This doc is the audit and enablement checklist behind that
decision (task 328fb6d9; the tier-legend and empty-state slice landed under
task eaca1130). It lists what exists, what is missing, what each gap costs,
and the minimum viable set to flip the flag for a deployment.

## Current state (2026-11-10, verified)

What the page already does:

- Tier-grouped model cards from `catalog/models.yaml` (display name, tier,
  aliases, pricing provenance with verification date and source link).
- A tier legend explaining what S/A/B/C/D/Unranked mean, stating that tiers
  are editorial groupings from the bundled YAML — a maintainer judgment, not
  measured model quality (item 2, shipped 2026-11-10).
- Observed usage context per model (lifetime tokens, event counts) and a
  pricing-coverage header (`priced / observed`, `mode: unknown` disclosure).
  A 2026-11-12 verification pass priced the top observed models against
  provider pages; the live hub's copy now covers 97.3% of observed tokens
  (up from 32.5%) once redeployed (item 1).
- Standardized empty/unknown states (item 4, shipped 2026-11-10): models with
  unpublished rates say "Rates not published yet" instead of rendering empty
  pricing, zero-usage catalog models say "No usage recorded yet" instead of
  zero counts, a fully cataloged deployment states that nothing is
  uncataloged, and an empty catalog explains how to populate it. Nullable
  numeric cells keep the "Unknown, never zero" rule, and the footer repeats
  it.
- An uncataloged-models section for identifiers seen in usage but absent
  from the catalog, with the "how to add a model" help link.
- Copy-ID affordance on model identifiers; catalog help link.
- Read API stays open while the UI is gated: `/api/v1/catalog` and
  `/api/v1/models`.
- Accessibility currently passes the CI bar: the browsercheck axe scan
  (WCAG 2.x A/AA) is clean on the page, and the dependency-free contract
  tests cover the mobile breakpoint.

What it does not do: no per-model detail view, no sorting/filtering/search
on the card grid, no alias management (aliasing lives in Settings and the
two surfaces do not reference each other), and pricing still has a small
`mode: unknown` tail (item 1).

## Roadmap items

Estimates are focused worker sessions: S ≤ half a day, M = 1–2 days,
L = 3+ days. Items marked **[MVP]** are the minimum viable set; everything
else can land after the flag ships.

1. **[MVP] Pricing verification pass (S–M, largely research). — SHIPPED
   2026-11-12.** Every rate added in this pass was read from a provider
   pricing page fetched that day, and each newly priced entry records the
   exact `source` URL plus `verified_at: 2026-11-12`. Ten models moved from
   `mode: unknown` to `mode: standard`:
   - Anthropic (`platform.claude.com/docs/en/about-claude/pricing`):
     Claude Opus 5.5 ($4/$20), Sonnet 5.5 ($2/$10), Sonnet 5 ($2/$10).
   - OpenAI (`developers.openai.com/api/docs/pricing`): GPT-6.1 Sol
     ($2/$10), GPT-6.1 Terra ($1/$5), GPT-6 Luna ($0.10/$0.50), GPT-6 Sol
     ($2/$10), GPT-6 Astra ($10/$50).
   - Z.ai (`docs.z.ai/guides/overview/pricing`): GLM-5.3 Flash
     ($0.15/$0.50).
   - Google (`ai.google.dev/gemini-api/docs/pricing`): Gemini 3.6 Flash
     ($0.75/$3.75).

   Normalization conventions: OpenAI rates are the published short-context
   (≤272K) standard tier — long-context rates are higher and are not encoded;
   Anthropic `cache_write_per_million` is the 5-minute cache-write rate;
   cached-input rates are recorded where the page states them.

   Coverage on the live hub's usage (22.64B observed tokens, read-only copy):
   priced tokens rose from **32.5% (7.36B)** to **97.3% (22.03B)**;
   cataloged-but-unpublished tokens fell from 67.3% to 2.5%; uncataloged
   identifiers are 0.2% (only the documented non-model sentinels).

   Deliberately left `mode: unknown`, with the reason:
   - Claude Haiku 5.5 — tiered by prompt length (≤100K vs >100K tokens), so
     no single input/output rate is honest.
   - DeepSeek V4.1 Flash and V4 Pro — peak/off-peak time-of-day rates (2x
     spread); the schema has one rate.
   - Gemini 3 Flash — the page publishes `gemini-3-flash-preview`, a
     different model ID; no rate for the stable ID.
   - Gemini 3.7 Flash — not listed on the pricing page at all.
   - Nemotron 3.5 Super VL 120B-A12B — NVIDIA publishes model cards, not a
     per-token API rate.
   - Qwen 3.8 Flash Next — no public rate for this exact variant.
   - Codex Auto Review — not a published model with a rate card.
   - DeepSeek V4 Flash (free), Space Bunny (free), Muse Spark 1.2
     (contributor free) — plan/free-tier identifiers, no per-token rate.

   Known drift left for a follow-up: the 2026-07-12 OpenAI rates for
   `gpt-5.6-luna` ($1/$6) and `gpt-5.6-terra` ($2.50/$15) are higher than the
   current page ($0.20/$1.20 and $2/$12), and `gpt-5.6-sol`'s page rate
   ($4/$20) is explicitly promotional through 2026-11-30 while the catalog
   carries $5/$30. These were not rewritten in this pass because encoding
   promotional or newly-cut rates is a policy decision, not a fetch. Gemini
   3.6 Flash also has an announced increase to $1.50/$7.50 on 2027-01-01.

2. **[MVP] Tier explanations (S). — SHIPPED 2026-11-10.** The page now
   renders a legend above the tier sections: one row per populated tier
   (S/A/B/C/D/Unranked) explaining what the letter means, plus a provenance
   line stating that tiers are editorial groupings from the bundled
   `catalog/models.yaml` — a maintainer judgment, not measured model
   quality. Covered by a render test and a browsercheck invariant.

3. **[MVP] Sort / filter / search over the card grid (M).** The grid is
   static; with 35+ catalog models plus uncataloged entries, finding one is
   manual scanning. Client-side only (no new endpoints): sort by tokens /
   events / name, filter by tier / provider / priced-vs-unknown, and a
   free-text search over display names and aliases. Reuse the analytics
   filter-form styling to stay visually consistent.

4. **[MVP] Empty and unknown states (S). — SHIPPED 2026-11-10.**
   `mode: unknown` pricing renders "Rates not published yet · cost estimates
   stay Unknown, not zero" (with the status-check date) instead of an empty
   pricing grid; zero-usage catalog models say "No usage recorded yet"
   instead of `0 tokens · 0 events`; a fully cataloged deployment replaces
   the uncataloged section with an explicit "Every observed model resolves
   to a bundled catalog entry" note; an empty catalog explains how to
   populate it. The copy mirrors the sessions/analytics empty-state
   patterns, and nullable numeric cells keep the "Unknown, never zero" rule
   (the footer repeats it). Covered by render tests for the populated,
   empty, and uncataloged cases plus a browsercheck invariant.

5. **Per-model detail view (L, phase 2).** A `/tokedex/<model>` page with
   usage history (weekly trend), cost estimate and its pricing provenance,
   recent sessions filtered to the model, and machine distribution. This is
   the largest item: one new route, one template, two or three store
   queries, plus browsercheck coverage for the new page. Genuinely useful
   once item 1 makes costs meaningful — not before.

6. **Alias management UX (M, phase 2).** Settings already manages model
   aliases; Tokedex should surface them per model ("you alias
   `provider/raw-model` to this display name") and deep-link to the Settings
   alias form pre-filled, instead of duplicating editing UI on two pages.

7. **Accessibility for every new surface (standing rule, bundled above).**
   The page passes today; anything added must keep it that way: axe-clean
   under `internal/browsercheck`, keyboard-reachable scroll regions, visible
   focus, and labeled controls. Costed inside items 3–6, listed separately
   so it cannot be dropped silently.

8. **Tests for every shipped surface (standing rule, bundled above).**
   Dependency-free contract tests in `internal/api` for markup/behavior and
   browsercheck invariants for rendered pages, matching the existing bar
   (route gating tests, mobile breakpoint, axe scan).

## Enablement checklist

A deployment may flip `TOKEMON_ENABLE_TOKEDEX=true` when:

- [ ] Items 1–4 (**[MVP]** set) are merged and gated. Items 1 (pricing
      verification), 2 (tier legend), and 4 (empty/unknown states) shipped
      2026-11-10/12; item 3 (sort/filter/search) remains.
- [ ] The pricing-coverage header is honest for the deployment's catalog
      copy (remember: the hub loads the catalog from
      `~/Library/Application Support/Tokemon/models.yaml`, not the repo —
      copy the file and restart after changes).
- [ ] `internal/browsercheck` covers the enabled page (it already scans
      `/tokedex` when the flag is on, plus a disabled-mode 404 check).
- [ ] The user has reviewed the MVP result and agreed the page earns its
      nav slot.

Flag default stays off until the checklist is met; the gate code and tests
remain in place regardless, so re-gating later is a one-line change.
