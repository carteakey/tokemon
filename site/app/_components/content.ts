// Facts for the landing page. Every value here is sourced from the repository:
// thresholds and names from internal/evolution/evolution.go, adapters from
// internal/adapters/builtin, release details from CHANGELOG.md (0.4.0) and
// DEPLOYMENT.md, and the event shape from docs/tokemon-spec-v0.2.md §13.

export const links = {
  repo: "https://github.com/carteakey/tokemon",
  release: "https://github.com/carteakey/tokemon/releases/tag/v0.4.0",
  deployment: "https://github.com/carteakey/tokemon/blob/main/DEPLOYMENT.md",
  license: "https://github.com/carteakey/tokemon/blob/main/LICENSE",
  spec: "https://github.com/carteakey/tokemon/blob/main/docs/tokemon-spec-v0.2.md",
  changelog: "https://github.com/carteakey/tokemon/blob/main/CHANGELOG.md",
  security: "https://github.com/carteakey/tokemon/blob/main/SECURITY.md",
  catalog: "https://github.com/carteakey/tokemon/blob/main/catalog/models.yaml",
} as const;

export const version = "0.4.0";

export type Specimen = {
  stage: string;
  name: string;
  threshold: string;
  thresholdFull: string;
  image: string;
  blurb: string;
};

// Only these four forms are public. Sealed stages are rendered from coarse
// silhouettes; their art and names never appear on the page.
export const specimens: Specimen[] = [
  { stage: "00", name: "Egg", threshold: "0", thresholdFull: "0 tokens", image: "/tokemon/stage-00.webp", blurb: "Waiting for its first token." },
  { stage: "08", name: "Context Dragon", threshold: "100M", thresholdFull: "100,000,000 tokens", image: "/tokemon/stage-08.webp", blurb: "Ten to the eighth tokens in, and it has wings." },
  { stage: "13", name: "Inference Leviathan", threshold: "20B", thresholdFull: "20,000,000,000 tokens", image: "/tokemon/stage-13.webp", blurb: "Deep in the 1–2–5 checkpoints." },
  { stage: "18", name: "The Singularity", threshold: "1T", thresholdFull: "1,000,000,000,000 tokens", image: "/tokemon/stage-18.webp", blurb: "The final form. One trillion tokens." },
];

export type SealedStage = { stage: string; threshold: string };
export type SealedGroup = { range: string; label: string; thresholdRange: string; copy: string; stages: SealedStage[] };

export const sealedGroups: SealedGroup[] = [
  {
    range: "01–07",
    label: "Early dossier",
    thresholdRange: "10 → 10M",
    copy: "The first mutations stay sealed.",
    stages: [
      { stage: "01", threshold: "10" },
      { stage: "02", threshold: "100" },
      { stage: "03", threshold: "1K" },
      { stage: "04", threshold: "10K" },
      { stage: "05", threshold: "100K" },
      { stage: "06", threshold: "1M" },
      { stage: "07", threshold: "10M" },
    ],
  },
  {
    range: "09–12",
    label: "Titan dossier",
    thresholdRange: "1B → 10B",
    copy: "The appetite gets stranger from here.",
    stages: [
      { stage: "09", threshold: "1B" },
      { stage: "10", threshold: "2B" },
      { stage: "11", threshold: "5B" },
      { stage: "12", threshold: "10B" },
    ],
  },
  {
    range: "14–17",
    label: "Cosmic dossier",
    thresholdRange: "50B → 500B",
    copy: "Four late forms are not public yet.",
    stages: [
      { stage: "14", threshold: "50B" },
      { stage: "15", threshold: "100B" },
      { stage: "16", threshold: "200B" },
      { stage: "17", threshold: "500B" },
    ],
  },
];

export type Adapter = { no: string; name: string; id: string; habitat: string; format: string; enabled: "Default on" | "Opt-in" };

export const adapters: Adapter[] = [
  { no: "01", name: "Claude Code", id: "claude-code", habitat: "~/.claude/projects", format: "JSONL", enabled: "Default on" },
  { no: "02", name: "Codex", id: "codex", habitat: "~/.codex/sessions", format: "JSONL + SQLite", enabled: "Default on" },
  { no: "03", name: "GitHub Copilot CLI", id: "copilot-cli", habitat: "~/.copilot/session-state", format: "JSONL", enabled: "Default on" },
  { no: "04", name: "OpenCode", id: "opencode", habitat: "opencode.db", format: "SQLite", enabled: "Default on" },
  { no: "05", name: "Antigravity", id: "antigravity", habitat: "~/.gemini/antigravity*", format: "SQLite", enabled: "Default on" },
  { no: "06", name: "OpenClaw", id: "openclaw", habitat: "~/.openclaw/agents", format: "JSONL", enabled: "Default on" },
  { no: "07", name: "Hermes Agent", id: "hermes-agent", habitat: "~/.hermes/state.db", format: "SQLite", enabled: "Default on" },
  { no: "08", name: "Generic JSONL", id: "generic-jsonl", habitat: "Paths you configure", format: "Normalized JSONL", enabled: "Opt-in" },
];

export const releaseNotes = [
  { tag: "AUTH", title: "Locked dashboard", copy: "Dashboard pages, read APIs, and /metrics require Basic or Bearer auth. /healthz stays open for liveness checks." },
  { tag: "SIGNED", title: "Signed release assets", copy: "Every archive, the checksums, and the SPDX SBOM are signed with keyless Cosign. The installer refuses anything unsigned." },
  { tag: "GUARD", title: "Outbound privacy guard", copy: "One guard for tokemon inspect and every upload rejects content, credentials, tool arguments, full paths, and titles before a request opens." },
  { tag: "/TOKEDEX", title: "Model catalog page", copy: "Bundled YAML model tiers, aliases, pricing provenance, and observed usage context." },
  { tag: "/SESSIONS", title: "Session timeline", copy: "A filterable, metadata-only session timeline with export." },
  { tag: "SHARE", title: "Usage share over time", copy: "Analytics shows the top five harnesses, providers, models, or machines per window, plus aggregated Others." },
  { tag: "BACKUP", title: "Backups that restore", copy: "tokemon backup create, verify, and restore: WAL-safe versioned SQLite snapshots with retention." },
];

export const trust = [
  { mark: "SIG", title: "Cosign keyless", copy: "Archives, checksums, SBOM, and the image are signed by the release workflow identity." },
  { mark: "SBOM", title: "SPDX SBOM", copy: "A software bill of materials ships with every release." },
  { mark: "SHA", title: "checksums.txt", copy: "The installer verifies the archive hash and embedded version before installing." },
  { mark: "OCI", title: "Pinned images", copy: "ghcr.io/carteakey/tokemon by semver and digest. There is no latest tag." },
];

export const neverLeaves = [
  "Prompt text",
  "Response text",
  "Source code",
  "Repository contents",
  "Conversation titles",
  "Full repository paths",
  "Tool arguments",
  "Credentials",
];

export const hubCommand = `export TOKEMON_INGEST_TOKEN="$(openssl rand -hex 32)"
export TOKEMON_UID="$(id -u)"
export TOKEMON_GID="$(id -g)"

docker compose -f deploy/docker-compose.yml up -d --build
curl --fail http://localhost:18787/healthz`;

export const agentCommand = `bash deploy/install-agent.sh \\
  --version 0.4.0 \\
  --server https://tokemon.example.ts.net \\
  --machine-id laptop \\
  --adapters claude-code,codex \\
  --token-stdin <<'TOKEN'
replace-with-a-generated-secret
TOKEN`;

export const cloneCommand = `git clone --branch v0.4.0 https://github.com/carteakey/tokemon.git
cd tokemon`;

export const localCommand = `go run ./cmd/tokemon serve --addr 127.0.0.1:8080 \\
  --dev-loopback --database ./data/tokemon.db
go run ./cmd/tokemon agent --server http://127.0.0.1:8080 --once`;
