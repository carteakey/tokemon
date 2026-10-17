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

let htmlPromise;
function html() {
  htmlPromise ??= render().then(async (response) => {
    assert.equal(response.status, 200);
    assert.match(response.headers.get("content-type") ?? "", /^text\/html\b/i);
    return response.text();
  });
  return htmlPromise;
}

test("server-renders the Tokemon landing page", async () => {
  const page = await html();
  assert.match(page, /<title>Tokemon — Your coding tokens are evolving<\/title>/i);
  assert.match(page, /Your coding tokens are/);
  assert.match(page, /HOW IT GROWS/);
  assert.match(page, /QUICK INSTALL/);
  assert.match(page, /PRIVACY BY DEFAULT/);
  assert.match(page, /TOKEDEX \+ ESTIMATES/);
  assert.match(page, /static YAML model catalog/);
  assert.match(page, /usage share/);
  assert.match(page, /Representative sample data/);
  assert.match(page, /property="og:image" content="https?:\/\/[^"]+\/og\.png"/);
  assert.doesNotMatch(page, /codex-preview|react-loading-skeleton/);
});

test("keeps the evolution archive a teaser", async () => {
  const page = await html();
  assert.match(page, /EVOLUTION ARCHIVE/);
  assert.match(page, /Nineteen forms/);
  assert.match(page, /1–2–5 checkpoints/);
  for (const name of ["Egg", "Context Dragon", "Inference Leviathan", "The Singularity"]) {
    assert.match(page, new RegExp(name));
  }
  assert.match(page, /CLASSIFIED/);
  assert.match(page, /fifteen forms remain sealed/);
  // Sealed art and names must never reach the page.
  assert.doesNotMatch(page, /stage-14\.png/);
  assert.doesNotMatch(page, /stage-(0[1-7]|09|1[0-2]|1[4-7])\.(png|webp)/);
  assert.doesNotMatch(page, /Parameter Colossus|World Weaver|Cosmic Architect|Universe Engine|Agent Beast|Token Titan/);
  assert.doesNotMatch(page, /tokemon-dashboard\.png/);
});

test("guards product facts", async () => {
  const page = await html();
  for (const adapter of ["Claude Code", "Codex", "GitHub Copilot CLI", "OpenCode", "Antigravity", "OpenClaw", "Hermes Agent", "Generic JSONL"]) {
    assert.match(page, new RegExp(adapter, "i"), `missing adapter ${adapter}`);
  }
  assert.match(page, /MIT licensed/);
  assert.match(page, /Latest release/);
  assert.match(page, /https:\/\/github\.com\/carteakey\/tokemon\/releases\/tag\/v0\.4\.0/);
  assert.match(page, /https:\/\/github\.com\/carteakey\/tokemon\/blob\/main\/DEPLOYMENT\.md/);
  assert.match(page, /https:\/\/github\.com\/carteakey\/tokemon\/blob\/main\/LICENSE/);
  assert.match(page, /v0\.4\.0/);
  assert.doesNotMatch(page, /v0\.3\.3|v0\.3\.2/);
  assert.doesNotMatch(page, /Six adapters|UNNAMED_08/);
  assert.match(page, /Not affiliated with, endorsed by, or connected to Pokémon or The Pokémon Company/);
});

test("shows the real installer flags and release trust signals", async () => {
  const page = await html();
  assert.match(page, /install-agent\.sh/);
  assert.match(page, /--version<\/span> 0\.4\.0/);
  assert.match(page, /--token-stdin/);
  assert.doesNotMatch(page, /--token [^-]/);
  assert.match(page, /Cosign/);
  assert.match(page, /SBOM/);
  assert.match(page, /checksums\.txt/);
});

test("shows the metadata event and what never leaves", async () => {
  const page = await html();
  for (const field of ["schema_version", "event_id", "input_tokens", "output_tokens", "token_accuracy"]) {
    assert.match(page, new RegExp(`&quot;${field}&quot;|"${field}"`), `missing event field ${field}`);
  }
  for (const item of ["Prompt text", "Response text", "Source code", "Repository contents", "Conversation titles"]) {
    assert.match(page, new RegExp(item));
  }
  assert.match(page, /tokemon inspect/);
});
