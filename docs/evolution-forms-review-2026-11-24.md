# Evolution Forms Review & Art Upgrade Plan (2026-11-24)

Full visual, technical, and naming audit of all 19 Tokemon evolution forms
(`stage-00` through `stage-18`) in `web/static/tokemon/` and
`internal/evolution/evolution.go`.

## Summary

The shared charcoal, bronze/amber (`#d2a477`), cream-bone, and sage-circuit
(`#9bbba0`) palette holds together well across the ladder—stages `00`, `02`,
`05`, `06`, `07`, `08`, `11`, `13`, `15`, and `17` are strong anchors. The
audit surfaced two buckets of improvements:

1. **Obvious fixes (applied in this pass):**
   - Cleaned 17 stray pale-green chroma-key speckle artifacts and outer green
     fringe pixels on `stage-09.png` (`Token Titan`) and padded it from
     `568×640` to `640×640` so all 19 sprites share a uniform square canvas.
   - Nearest-neighbor scaled `stage-18.png` (`The Singularity`, `1T`) from
     `284×318` (`6.7%` fill) to `403×452` (`13.5%` fill) and updated
     `site/public/tokemon/stage-18.webp` so the final form carries real weight
     while keeping its quiet teardrop minimalism mirroring `00 Egg`.
   - Gently nearest-neighbor scaled `stage-01.png` (`Bitling`, `10`) from
     `210×228` (`8.1%` fill) to `262×285` (`12.6%` fill) for better readability
     on small cards while staying noticeably smaller than `Egg` and `Bytelet`.
   - Eliminated repeated prefixes (`Context` ×3, `Token` ×2, `-ling` ×2) and
     late-game generic sci-fi drift (`12`, `15`, `16`, `17`) across sealed
     forms while leaving the 4 public marketing-site forms (`00 Egg`,
     `08 Context Dragon`, `13 Inference Leviathan`, `18 The Singularity`)
     untouched.
2. **Future art redraws (tracked below for a dedicated art pass):**
   - `stage-04.png` (`Cache Cub`): pixel-art style alignment with `03`/`05`.
   - `stage-10.png` (`Model Eater`) and `stage-12.png` (`Latent Core`): break
     the near-duplicate "almond eye + 4 radial fins" silhouette trio (`10/11/12`).
   - `stage-16.png` (`Lattice Spire`): optional entity redraw if we ever want
     an *Architect* character rather than a living citadel monument.

## Full 19-Form Roster Audit

| Stage | Threshold | Previous Name | Current Name | Public? | Canvas / BBox (after pass) | Visual & Naming Notes |
| ---: | ---: | --- | --- | --- | --- | --- |
| 00 | 0 | Egg | **Egg** | Public | `640×640` (`330×375`) | Iconic opener; keep locked. |
| 01 | 10 | Bitling | **Bitling** | Sealed | `640×640` (`262×285`) | Scaled `1.25×` via nearest-neighbor for mobile legibility. |
| 02 | 100 | Bytelet | **Bytelet** | Sealed | `640×640` (`327×316`) | Strong pixel-beetle sprite and name. |
| 03 | 1K | Promptling | **Prompt Imp** | Sealed | `640×640` (`399×430`) | Removed `-ling` repeat after `Bitling`; fits the big-eared imp sprite. |
| 04 | 10K | Context Cub | **Cache Cub** | Sealed | `640×640` (`385×397`) | Removed 1st duplicate `Context`. **Art redraw candidate** (origami style break). |
| 05 | 100K | Token Scout | **Vector Lynx** | Sealed | `640×640` (`391×387`) | Removed duplicate `Token`; matches the sleek feline-circuit sprite. |
| 06 | 1M | Code Caster | **Code Caster** | Sealed | `640×640` (`463×490`) | Great match for the 4-armed floating rune caster. |
| 07 | 10M | Agent Beast | **Agent Hound** | Sealed | `640×640` (`531×515`) | Sharper than generic `Beast`; matches the armored circuit-hound sprite. |
| 08 | 100M | Context Dragon | **Context Dragon** | Public | `640×640` (`363×561`) | Standout sprite and hero mascot; keep locked. |
| 09 | 1B | Token Titan | **Token Titan** | Sealed | `640×640` (`529×612`) | Now the sole `Token` name; cleaned 17 chroma speckles + green fringe, padded to `640×640`. |
| 10 | 2B | Model Eater | **Model Eater** | Sealed | `640×640` (`441×504`) | Strong name. **Art redraw candidate** (currently a passive eye-shield, duplicate of `12`). |
| 11 | 5B | Context Deity | **Attention Seraph** | Sealed | `640×640` (`481×533`) | Removed 3rd duplicate `Context`; nails the multi-eyed 6-window seraph art. |
| 12 | 10B | Reality Core | **Latent Core** | Sealed | `640×640` (`538×528`) | Anchored back to AI theme. **Art redraw candidate** (breaks `10/11/12` silhouette repeat). |
| 13 | 20B | Inference Leviathan | **Inference Leviathan** | Public | `640×640` (`575×545`) | S-tier cosmic serpent and name; keep locked. |
| 14 | 50B | Parameter Colossus | **Parameter Colossus** | Sealed | `640×640` (`571×603`) | Strong bronze golem and LLM name (also featured in `site/public/og.png`). |
| 15 | 100B | World Weaver | **Tensor Weaver** | Sealed | `640×640` (`422×588`) | Replaced generic sci-fi name; matches the glowing-tendril celestial jellyfish. |
| 16 | 200B | Cosmic Architect | **Lattice Spire** | Sealed | `640×640` (`402×625`) | Aligned name with the living obsidian-circuit cathedral spire art. |
| 17 | 500B | Universe Engine | **Gradient Engine** | Sealed | `640×640` (`551×586`) | Replaced generic sci-fi name; matches the spinning cosmic dynamo wheel. |
| 18 | 1T | The Singularity | **The Singularity** | Public | `640×640` (`403×452`) | Scaled `1.42×` via nearest-neighbor so 1T feels monumental while echoing `00 Egg`. |

Every stage now owns a distinct computing/AI root with zero repeated prefixes:
`Egg`, `Bit`, `Byte`, `Prompt`, `Cache`, `Vector`, `Code`, `Agent`, `Context`,
`Token`, `Model`, `Attention`, `Latent`, `Inference`, `Parameter`, `Tensor`,
`Lattice`, `Gradient`, `Singularity`.

## Future Art Redraw Briefs

Keep these style invariants for any replacement sprite (see also `STYLE_GUIDE.md`
and `docs/evolution-art-qa.md`):

- **Palette:** charcoal obsidian body (`#1d201b`–`#2f332c`), muted sage
  circuitry/glow (`#9bbba0` / `#5f7a63`), weathered copper-amber trim
  (`#d2a477` / `#9a6a43`), cream-bone highlights (`#f0ede5`). No neon cyan,
  magenta, or bright primary yellow.
- **Rendering:** crisp chunky pixel clusters with clean 1px dark outlines, flat
  `#00ff00` chroma-key background converted via `tools/chromakey`, centered on a
  `640×640` 8-bit RGBA transparent canvas with zero semi-transparent green
  fringe.
- **Originality:** strictly original field-guide forms—zero franchise creatures,
  badges, or interface motifs.

### Priority 1: `stage-04.png` — Cache Cub (10K)

- **Problem:** Only sprite rendered in smooth-shaded low-poly/origami style;
  missing copper/amber body trim; harsh bright-yellow floating square above the
  head; anime-style puppy eyes that break continuity between `03 Prompt Imp` and
  `05 Vector Lynx`.
- **Target design:** Stocky quadruped pixel-art cub built from weathered
  charcoal plates, warm copper collar/ankle trim, sage-green etched circuit
  plates along its back (visual bridge from `02 Bytelet`'s shell and `03`'s
  pixel face into `05 Vector Lynx`), with crisp pixel eyes matching `05`.

### Priority 2: `stage-10.png` — Model Eater (2B)

- **Problem:** Shares the exact vertical-almond + central-eye + 4-fin silhouette
  with `11 Attention Seraph` and `12 Latent Core`, and looks passive rather than
  predatory.
- **Target design:** A predatory cosmic/abyssal creature or armored ringed maw
  with segmented bronze jaws opening around a dark sage singularity throat,
  pulling fractured token cubes and model shards inward. Distinct horizontal or
  angled predatory silhouette between upright `09 Token Titan` and symmetrical
  `11 Attention Seraph`.

### Priority 3: `stage-12.png` — Latent Core (10B)

- **Problem:** Near-duplicate of `stage-10.png` (vertical eye almond + 4 corner
  blades + orbiting cubes).
- **Target design:** Interlocking nested gyroscopic astrolabe rings (weathered
  bone and copper) surrounding a suspended geometric icosahedral/octahedral sage
  crystal core—unmistakably a *Core* and visually distinct from `11 Attention
  Seraph`'s vertical eye-and-tablets silhouette.

### Optional: `stage-16.png` — Lattice Spire (200B)

- **Current state:** Renaming `Cosmic Architect` to `Lattice Spire` resolves the
  art/name mismatch immediately.
- **Optional future direction:** If we later prefer an entity over a living
  monument at 200B, redraw as an elongated celestial cloaked/geometric builder
  weaving a floating wireframe lattice (evolutionary echo of `09 Token Titan`)
  and rename to `Lattice Architect` or `Epoch Architect`.

## Post-Redraw Verification Checklist

After replacing any `web/static/tokemon/stage-NN.png`:

1. Update SHA-256 and dimensions in `web/static/tokemon/manifest.json`.
2. Regenerate the matching 16×16 silhouette in `site/app/_components/silhouettes.ts`
   if a sealed stage's outline changed (and regenerate `site/public/tokemon/stage-NN.webp`
   only for the 4 public forms `00`, `08`, `13`, `18`).
3. Run the art and test gates:

```bash
go run ./cmd/tokemon art validate --dir web/static/tokemon
go test ./internal/art ./internal/evolution ./internal/api ./internal/system -count=1
TOKEMON_BROWSER_SMOKE=1 go test ./internal/browsercheck -v -count=1
cd site && npm run lint && npm test
```
