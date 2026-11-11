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
two surfaces do not reference each other), and pricing is still mostly
`mode: unknown` (item 1).

## Roadmap items

Estimates are focused worker sessions: S ≤ half a day, M = 1–2 days,
L = 3+ days. Items marked **[MVP]** are the minimum viable set; everything
else can land after the flag ships.

1. **[MVP] Pricing verification pass (S–M, largely research).** The catalog
   is honest but mostly unpriced: recent entries use `mode: unknown`, and
   the live hub's coverage line confirms most observed tokens carry no cost.
   Verify input/output rates against provider pricing pages for the top
   observed models, set `mode: standard` with `verified_at` + `source`, and
   keep `mode: unknown` for the rest. Depends on the pricing task's schema
   decisions. Without this, a "dex" about models shows little more than
   names and raw token counts.

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

- [ ] Items 1–4 (**[MVP]** set) are merged and gated. Items 2 (tier
      legend) and 4 (empty/unknown states) shipped 2026-11-10; items 1
      (pricing verification) and 3 (sort/filter/search) remain.
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
