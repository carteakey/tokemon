# Tokemon evolution art

These are original raster pixel-art assets for the Tokemon evolution forms. They are generated as a starting art direction for CAR-66 and must remain free of Pokémon or other franchise references.

The `token-dex.png` asset is the project mark: a small handheld token field guide in the same pixel-art language as the evolution forms. It is used by the dashboard as its favicon and header icon.

The `icons/` directory contains the dashboard's original utility icon family. The five summary-card icons represent top machine, estimated API cost, cache hit, threads, and active days. The project, model, and machine icons identify rows in the usage tables. Keep these decorative in markup (`alt=""`); nearby labels carry the meaning.

Provenance: the evolution forms were generated with the built-in image-generation tool using local stage references, then converted from a flat `#00ff00` chroma-key background with the image-generation chroma-key helper and resized to a 640px maximum edge. The token-dex mark follows the same generated pixel-art direction and chroma-key workflow, resized to 512px. The utility icons were generated as one original eight-icon sprite atlas on flat `#ff00ff`, converted with the same chroma-key helper, split into stable semantic filenames, and resized to 128px transparent PNGs.

The stage filenames are stable and intentionally use the evolution stage number. Keep the dashboard fallback text available if an asset is missing.

The art pass contains one asset for every stage from `stage-00.png` through `stage-18.png`. Stages 13–18 extend the original silhouette family at the tail; existing assets were not renumbered or replaced. The manifest is the stable lookup contract used by the dashboard.

The dashboard self-hosts the Latin subset of Pixelify Sans at `fonts/pixelify-sans-latin.woff2` for creature names, stage labels, and display headings. Body copy remains system sans-serif, while token values and code remain system monospace. The font is distributed under the SIL Open Font License 1.1; see `fonts/OFL.txt`.
