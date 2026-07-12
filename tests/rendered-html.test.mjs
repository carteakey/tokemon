import assert from "node:assert/strict";
import test from "node:test";

async function render() {
  const workerUrl = new URL("../dist/server/index.js", import.meta.url);
  workerUrl.searchParams.set("test", `${process.pid}-${Date.now()}`);
  const { default: worker } = await import(workerUrl.href);

  return worker.fetch(
    new Request("https://tokemon.example/", { headers: { accept: "text/html" } }),
    { ASSETS: { fetch: async () => new Response("Not found", { status: 404 }) } },
    { waitUntil() {}, passThroughOnException() {} },
  );
}

test("server-renders the Tokemon landing page", async () => {
  const response = await render();
  assert.equal(response.status, 200);
  assert.match(response.headers.get("content-type") ?? "", /^text\/html\b/i);

  const html = await response.text();
  assert.match(html, /<title>Tokemon — Your coding tokens are evolving<\/title>/i);
  assert.match(html, /Your coding tokens are/);
  assert.match(html, /QUICK INSTALL/);
  assert.match(html, /EVOLUTION ARCHIVE/);
  assert.match(html, /PRIVACY BY DEFAULT/);
  assert.match(html, /property="og:image" content="https?:\/\/[^\"]+\/og\.png"/);
  assert.doesNotMatch(html, /codex-preview|react-loading-skeleton/);
});
