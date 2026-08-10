# Evolution art QA and release checklist

This checklist is the public release contract for the original Tokemon
evolution art under `web/static/tokemon/`. It is intentionally reproducible
without a browser service or an image-processing dependency.

## Automated gate

Run these commands from the repository root:

```bash
go run ./cmd/tokemon art validate --dir web/static/tokemon
go test ./internal/art ./internal/evolution ./internal/api ./cmd/tokemon
go test ./...
go vet ./...
go test -race ./...
```

`art validate` checks all of the following before a release can proceed:

- every numbered manifest stage exists exactly once, and every `stage-*.png`
  is listed in `manifest.json`;
- stable `/static/tokemon/stage-NN.png` paths and non-empty form names;
- the expected SHA-256 digest for each checked-in file;
- manifest dimensions and the 640px maximum edge;
- an 8-bit RGBA PNG with at least one transparent pixel for every stage.

The manifest's `sha256`, `width`, `height`, `rgba`, and `has_alpha` fields are
release checksums and format declarations, not runtime data.

## Visual and rendering gate

- Inspect every stage from `stage-00.png` through `stage-18.png` at native size
  with nearest-neighbour/pixelated rendering. Confirm one coherent limited
  palette, readable silhouette progression, clean transparent edges, and no
  franchise creatures, badges, typefaces, or interface motifs.
- Render the dashboard with an empty database, a populated database, and
  token totals immediately below and at a late-stage threshold. Confirm the
  form name, stage chip, image path, progress, and final-form state agree.
- Request a deliberately absent stage URL and confirm the dashboard's image
  error handler hides the broken image and reveals the labelled `STAGE NN`
  fallback. The deterministic HTTP contract is covered by
  `TestDashboardMissingAssetFallbackRuntimeContract`; live browser QA is an
  additional release check when a browser runtime is available.
- Check `prefers-reduced-motion: reduce`: no forced odometer/evolution motion,
  no layout shift, and the form/stage text remains available without the art.
- Check desktop and narrow viewport rendering for no horizontal page overflow;
  the creature remains subordinate to the lifetime total and retains a visible
  focus/alt-text contract.

## Provenance and release evidence

- Keep the provenance note in `web/static/tokemon/README.md` with the source
  workflow, chroma-key conversion, resize limit, stable filenames, and font
  license.
- Preserve the existing art unless a real defect is found; record any
  replacement as a manifest hash change and repeat the full gate.
- Attach the command output, inspected stage list, rendered states, and any
  browser/runtime limitation to the CAR-66 release comment. Do not include
  private project notes, local data, credentials, or generated prompts.
