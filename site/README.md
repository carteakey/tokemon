# Tokemon marketing site

The public landing page for [Tokemon](https://github.com/carteakey/tokemon),
the local-first token garden for coding agents.

The site is a server-rendered vinext app with a deliberately small surface:
product story, evolution archive, dashboard showcase, Tokedex summary, privacy
promise, adapter roster, release notes, installation path, and links to the
source repository and current release. Animation is CSS-only (and disabled
under `prefers-reduced-motion`); the only client JavaScript is the progressive
copy button on the install commands.

Page facts live in `app/_components/content.ts`. The evolution archive is a
teaser: only stages 00, 08, 13, and 18 are shown. Sealed forms render from the
coarse 16×16 silhouettes in `app/_components/silhouettes.ts`, so the page never
references a sealed stage image or name (the rendered-HTML test enforces this).
`public/tokemon/dashboard-sample.webp` is `docs/tokemon-dashboard.png` with the
sealed creature panel replaced by its silhouette.

## Run locally

Requires Node.js `>=22.13.0`.

```bash
npm install
npm run dev
```

Build and test the production output:

```bash
npm run build
npm test
npm run lint
```

## Release links

- [Tokemon source repository](https://github.com/carteakey/tokemon)
- [Latest Tokemon release](https://github.com/carteakey/tokemon/releases/tag/v0.4.0)
- [Public deployment guide](https://github.com/carteakey/tokemon/blob/main/DEPLOYMENT.md)
- [MIT license](https://github.com/carteakey/tokemon/blob/main/LICENSE)

## Hosting

The site uses the repository's `.openai/hosting.json` and the existing vinext
Cloudflare-compatible build. Keep the hosting metadata in sync with the
configured Sites project and deploy the validated build from the pushed branch.
