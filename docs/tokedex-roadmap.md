# Tokedex roadmap — what "enabled" requires

Tokedex (the `/tokedex` model-catalog UI) is gated behind
`TOKEMON_ENABLE_TOKEDEX` / `--enable-tokedex` and ships **off by default**:
the page answers 404 and no navigation link advertises it until the work
below lands. This doc is the audit and enablement checklist behind that
decision (task 328fb6d9). It lists what exists, what is missing, what each
gap costs, and the minimum viable set to flip the flag for a deployment.

## Current state (2026-11-07, verified)

What the page already does:

- Tier-grouped model cards from `catalog/models.yaml` (display name, tier,
  aliases, pricing provenance with verification date and source link).
- Observed usage context per model (lifetime tokens, event counts) and a
  pricing-coverage header (`priced / observed`, `mode: unknown` disclosure).
- An uncataloged-models section for identifiers seen in usage but absent
  from the catalog, with the "how to add a model" help link.
- Copy-ID affordance on model identifiers; catalog help link.
- Read API stays open while the UI is gated: `/api/v1/catalog` and
  `/api/v1/models`.
- Accessibility currently passes the CI bar: the browsercheck axe scan
  (WCAG 2.x A/AA) is clean on the page, and the dependency-free contract
  tests cover the mobile breakpoint.

What it does not do: no per-model detail view, no sorting/filtering/search
on the card grid, no explanation of what tiers mean, no alias management
(aliasing lives in Settings and the two surfaces do not reference each
other), and thin empty/unknown states for partially-priced models.

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

2. **[MVP] Tier explanations (S).** Tier letters (S/A/B/…) render bare.
   Add one line per tier (or a legend) stating what the tier means and
   where it comes from (bundled YAML judgment, not measured quality). Cheap,
   and the single biggest comprehension win on the page.

3. **[MVP] Sort / filter / search over the card grid (M).** The grid is
   static; with 35+ catalog models plus uncataloged entries, finding one is
   manual scanning. Client-side only (no new endpoints): sort by tokens /
   events / name, filter by tier / provider / priced-vs-unknown, and a
   free-text search over display names and aliases. Reuse the analytics
   filter-form styling to stay visually consistent.

4. **[MVP] Empty and unknown states (S).** Standardize how the page renders
   `mode: unknown` pricing ("rates not published yet"), zero-usage catalog
   models, and a catalog with no uncataloged models at all. Mirror the
   sessions/analytics empty-state copy patterns so the pages feel like one
   product. Numeric cells must keep the "Unknown, never zero" rule for
   nullable fields.

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

- [ ] Items 1–4 (**[MVP]** set) are merged and gated.
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
