# Tokemon visual style guide

Tokemon is a dense, quiet instrument panel with the charm of an original 16-bit field guide. It should feel sturdy and tactile, not cute, glossy, or overloaded.

## Principles

1. **Show the value first.** Labels identify; values explain. Remove prose that restates the chart, meter, or number beside it.
2. **Dense, not cramped.** Prefer compact panels, short labels, aligned values, and deliberate gaps over large empty cards.
3. **One strong moment.** The creature and lifetime counter carry the page. Supporting data stays visually quieter.
4. **Thick enough to feel built.** Use visible one-pixel borders, strong icon silhouettes, and substantial meters. Avoid hairline-only or glass effects.
5. **Local and restrained.** The interface should feel like a private tool, not a growth dashboard or billing product.

## Palette

| Role | Token | Value |
| --- | --- | --- |
| Canvas | `--bg` | `#10110f` |
| Panel | `--surface` | `#171916` |
| Raised panel | `--surface-raised` | `#1d201b` |
| Main text | `--text` | `#f0ede5` |
| Secondary text | `--muted` | `#a2a69b` |
| Quiet text | `--faint` | `#6f766b` |
| Border | `--line` | `#30352d` |
| Strong border | `--line-bright` | `#485044` |
| Sage accent | `--accent` | `#9bbba0` |
| Moss accent | `--accent-dim` | `#607864` |
| Amber accent | `--warm` | `#d2a477` |
| Error only | `--danger` | `#bd7164` |

Sage is structural: navigation state, headings, progress, and input/cache data. Amber is scarce: output, share values, milestones, and one meaningful highlight. Do not introduce new semantic colors when sage, amber, text, or muted text can do the job.

## Type

- Use Pixelify Sans for the brand, creature names, and rare display headings.
- Use the system monospace stack for counters, values, table data, labels, and code.
- Use the system sans-serif stack for sentences and help text.
- Use uppercase labels at 9–12px with `0.06em–0.10em` tracking.
- Use tabular figures for changing or compared numbers.
- Never use novelty type for body copy.

## Shape and depth

- Main frame: 12px radius.
- Panels: 7–8px radius and a visible `--line-bright` border.
- Controls and meter tracks: 4–5px radius.
- Borders are normally 1px. Use 2px only for focus or a temporary overlay.
- Depth comes from nested dark surfaces and restrained inset shading, not blurred shadows.
- Avoid glassmorphism, gradients behind ordinary cards, glowing borders, and floating white surfaces.

## Spacing and density

Use a compact 2px / 4px / 6px / 8px / 10px / 12px / 16px / 20px rhythm.

- Dashboard gaps: 10px.
- Panel padding: 12–18px.
- Table rows: about 7px vertical padding.
- Summary cards: 76px minimum height.
- Prefer five compact metrics in one desktop row over large independent cards.
- At narrow widths, preserve hierarchy and stack the layout; never shrink data below legibility.

## Icons and pixel art

- Use original pixel art with sturdy silhouettes, dark outlines, limited shading, and no franchise references.
- Summary-card icons render at 36px on desktop and 30px on phones.
- Table-row icons render at 17px.
- Source icons are transparent 128px PNGs under `web/static/tokemon/icons/`.
- Use `image-rendering: pixelated`, `object-fit: contain`, explicit dimensions, and empty alt text when the adjacent label already names the item.
- Keep a consistent visual mass across a set. Do not mix line icons, emoji, smooth vectors, and pixel sprites.
- Tiny blue, violet, or green accents may distinguish project, model, and machine categories. Structural UI remains sage and amber.

## Data presentation

- Lifetime tokens use the split-flap counter and remain the dominant number.
- Composition is one thick segmented meter with compact percentages or totals beneath it.
- Evolution progress appears once: remaining amount, percentage, and one progress bar.
- Activity uses the compact daily pixel field. Details belong on hover or focus.
- Tables show at most the most useful rows and align numeric columns to the right.
- Unknown values stay unknown; never silently convert them to zero.

## Voice and copy

Write labels, not explanations. Prefer “Cache hit,” “Threads,” and “Active days.” Avoid copy such as “distinct provider sessions,” “merged across machines,” or “this is not your bill” when the interface already makes the context clear.

Keep a sentence only when it changes what the user should do, prevents a real misunderstanding, or carries a privacy/safety guarantee. Put technical detail in documentation or a data view, not the overview.

## Accessibility and motion

- Maintain readable contrast for text and controls.
- Do not rely on color alone for meaning; pair it with position, labels, or values.
- Decorative icons use empty alt text. Meaningful creature art names the form and stage.
- Keyboard focus must be visible with a 2px outline.
- Respect `prefers-reduced-motion`; the counter must remain understandable without animation.
- Test at desktop and phone widths with no horizontal page overflow.

## New UI checklist

- Does every visible sentence earn its space?
- Is the primary number or action obvious in one glance?
- Can an existing token, component, icon, or spacing value be reused?
- Are borders, radii, icon weight, and data alignment consistent with the dashboard?
- Does the mobile layout preserve the same hierarchy?
- Are empty, unknown, loading, focus, and reduced-motion states covered?
