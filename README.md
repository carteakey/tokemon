# Tokemon marketing site

The public landing page for [Tokemon](https://github.com/carteakey/tokemon),
the local-first token garden for coding agents.

The site is a server-rendered vinext app with a deliberately small surface:
product story, privacy promise, evolution archive, installation path, and
links to the source repository and current release.

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
- [Latest Tokemon release](https://github.com/carteakey/tokemon/releases/tag/v0.3.2)
- [Public deployment guide](https://github.com/carteakey/tokemon/blob/main/DEPLOYMENT.md)
- [MIT license](https://github.com/carteakey/tokemon/blob/main/LICENSE)

## Hosting

The site uses the repository's `.openai/hosting.json` and the existing vinext
Cloudflare-compatible build. Keep the hosting metadata in sync with the
configured Sites project and deploy the validated build from the pushed branch.
