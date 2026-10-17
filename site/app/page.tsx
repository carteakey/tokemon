/* eslint-disable @next/next/no-img-element -- Pixel art and the redacted screenshot are
   pre-sized static WebP files with explicit dimensions; next/image would route them through
   the Cloudflare Images optimizer, which this static landing page does not use. */
import type { CSSProperties } from "react";
import { CopyButton } from "./_components/CopyButton";
import { JsonBlock, ShellBlock, YamlBlock } from "./_components/Highlight";
import { Odometer } from "./_components/Odometer";
import { silhouettes } from "./_components/silhouettes";
import {
  adapters,
  agentCommand,
  cloneCommand,
  hubCommand,
  links,
  localCommand,
  neverLeaves,
  releaseNotes,
  sealedGroups,
  specimens,
  trust,
  version,
} from "./_components/content";

// docs/tokemon-spec-v0.2.md §13: the normalized event every adapter emits.
const sampleEvent = {
  schema_version: "1",
  event_id: "sha256:...",
  timestamp: "2026-07-11T18:42:00Z",
  machine_id: "mac-server",
  session_id: "optional-provider-session-id",
  project: "carteakey.dev",
  provider: "anthropic",
  model: "claude-opus-4.1",
  tool: "claude-code",
  input_tokens: 12000,
  output_tokens: 3400,
  cache_read_tokens: 8200,
  cache_write_tokens: 1100,
  reasoning_tokens: null,
  total_tokens: 24700,
  duration_ms: 42000,
  cost: null,
  currency: "USD",
  token_accuracy: "reported",
  source: { adapter: "claude-code", adapter_version: "1.0.0" },
  metadata: {},
};

// Selected lines, copied verbatim, from the claude-haiku-4.5 entry in catalog/models.yaml.
const catalogExcerpt = `claude-haiku-4.5:
  provider: anthropic
  display_name: Claude Haiku 4.5
  aliases: [claude-haiku-4-5, claude-haiku-4-5-20251001]
  tier: B
  pricing:
    currency: USD
    mode: standard
    verified_at: 2026-07-12
    input_per_million: 1
    cached_input_per_million: 0.1
    output_per_million: 5`;

// Demo reading for the hero device: a Context Dragon between 100M and 1B.
const DEMO_TOKENS = 412_906_318;
const DEMO_PROGRESS = Math.round(((DEMO_TOKENS - 100_000_000) / 900_000_000) * 100);

const particles = [
  { x: 8, s: 6, d: 0, t: 9, c: "a" }, { x: 17, s: 4, d: 3.2, t: 11, c: "w" }, { x: 26, s: 8, d: 6.1, t: 13, c: "c" },
  { x: 34, s: 4, d: 1.4, t: 8, c: "a" }, { x: 43, s: 6, d: 4.8, t: 12, c: "w" }, { x: 52, s: 4, d: 7.5, t: 10, c: "a" },
  { x: 61, s: 8, d: 2.2, t: 14, c: "w" }, { x: 69, s: 4, d: 5.6, t: 9, c: "c" }, { x: 77, s: 6, d: 0.8, t: 11, c: "a" },
  { x: 85, s: 4, d: 3.9, t: 10, c: "w" }, { x: 92, s: 6, d: 6.8, t: 12, c: "a" }, { x: 22, s: 4, d: 8.4, t: 10, c: "w" },
  { x: 47, s: 4, d: 9.1, t: 9, c: "c" }, { x: 73, s: 4, d: 10.2, t: 13, c: "a" },
];

const ticker = ["Feed it tokens", "Watch it evolve", "Keep your content private", "Metadata only", "Nineteen forms", "One trillion tokens to the end"];

function Silhouette({ stage }: { stage: string }) {
  return (
    <svg className="silhouette" viewBox="0 0 16 16" width="16" height="16" aria-hidden="true" shapeRendering="crispEdges">
      <path d={silhouettes[stage]} />
    </svg>
  );
}

function Arrow() {
  return <span className="arrow" aria-hidden="true">→</span>;
}

function Ext() {
  return <span className="arrow" aria-hidden="true">↗</span>;
}

export default function Home() {
  return (
    <>
      <a className="skipLink" href="#main">Skip to content</a>
      <header className="siteHeader">
        <nav className="nav shell" aria-label="Primary">
          <a className="brand" href="#top" aria-label="Tokemon home">
            <img src="/tokemon/token-dex-96.webp" alt="" width="34" height="34" />
            <span>TOKEMON</span>
          </a>
          <div className="navLinks">
            <a href="#how">How it works</a>
            <a href="#forms">Forms</a>
            <a href="#dashboard">Dashboard</a>
            <a href="#privacy">Privacy</a>
            <a href="#adapters">Adapters</a>
            <a href="#install">Install</a>
          </div>
          <a className="navCta" href={links.release}>
            <span className="liveDot" aria-hidden="true" />v{version}<span className="srOnly"> release notes</span> <Ext />
          </a>
        </nav>
      </header>

      <main id="main">
        {/* ───────────── HERO ───────────── */}
        <section className="hero" id="top" aria-labelledby="hero-title">
          <div className="heroBackdrop" aria-hidden="true">
            <div className="heroGrid" />
            <div className="heroGlow" />
            <div className="particles">
              {particles.map((p, i) => (
                <i key={i} className={`particle p-${p.c}`} style={{ "--x": `${p.x}%`, "--s": `${p.s}px`, "--d": `${p.d}s`, "--t": `${p.t}s` } as CSSProperties} />
              ))}
            </div>
          </div>

          <div className="shell heroLayout">
            <div className="heroCopy">
              <a className="releasePill" href={links.release}>
                <span className="pillTag">NEW</span>
                <span>v{version} · signed releases, locked dashboard</span>
                <Ext />
              </a>
              <p className="eyebrow">LOCAL-FIRST TOKEN GARDEN</p>
              <h1 id="hero-title">Your coding tokens are <em>evolving.</em></h1>
              <p className="lede">
                Tokemon reads your coding agents&rsquo; local usage logs, keeps only the metadata, and grows one strange little creature on a private hub you run. Token counts go in. Your prompts, code, and conversations never leave the machine.
              </p>
              <div className="heroActions">
                <a className="button buttonPrimary" href="#install">Install v{version} <Arrow /></a>
                <a className="button buttonGhost" href="#forms">Meet the forms</a>
              </div>
              <ul className="heroFacts" aria-label="Product facts">
                <li><b>Metadata</b> only</li>
                <li><b>Go</b> + SQLite</li>
                <li><b>8</b> adapters</li>
                <li><b>MIT</b> licensed</li>
              </ul>
            </div>

            <figure className="device" aria-labelledby="device-caption">
              <div className="deviceBezel">
                <div className="deviceTop" aria-hidden="true">
                  <span className="powerLed" />
                  <span>TOKEDEX</span>
                  <span className="grille"><i /><i /><i /></span>
                </div>
                <div className="screen">
                  <div className="screenHud" aria-hidden="true">
                    <span>FORM 08 / 18</span>
                    <span className="hudLive"><span className="liveDot" />DEMO FEED</span>
                  </div>
                  <div className="screenHalo" aria-hidden="true" />
                  <div className="creature">
                    <img src="/tokemon/stage-08.webp" alt="Context Dragon, Tokemon evolution stage 08: a pixel-art serpent of green circuit blocks with cream wings" width="640" height="640" fetchPriority="high" />
                  </div>
                  <div className="creatureShadow" aria-hidden="true" />
                  <div className="screenScan" aria-hidden="true" />
                  <div className="screenName">
                    <strong>Context Dragon</strong>
                    <span>STAGE 08 · UNLOCKED AT 100M</span>
                  </div>
                </div>
                <div className="readout">
                  <div className="readoutLabel"><span>LIFETIME TOKENS</span><span className="sampleTag">SAMPLE DATA</span></div>
                  <Odometer value={DEMO_TOKENS} label="Sample lifetime token counter" />
                  <div className="nextForm">
                    <span>NEXT · STAGE 09 · <span className="redacted" aria-label="classified">CLASSIFIED</span></span>
                    <span>AT 1B</span>
                  </div>
                  <div className="evoBar" role="img" aria-label={`Sample progress to the next form: ${DEMO_PROGRESS}%`}>
                    <i style={{ "--p": `${DEMO_PROGRESS}%` } as CSSProperties} />
                  </div>
                </div>
                <div className="controls" aria-hidden="true">
                  <span className="dpad"><i /><i /></span>
                  <span className="pills"><i /><i /></span>
                  <span className="abButtons"><i /><i /></span>
                </div>
              </div>
              <figcaption id="device-caption" className="srOnly">A sample Tokemon reading: the Context Dragon with a demo lifetime token counter.</figcaption>
            </figure>
          </div>
        </section>

        <div className="ticker" aria-hidden="true">
          <div className="tickerTrack">
            {[0, 1].map((copy) => (
              <span key={copy}>
                {ticker.map((item) => <span key={item}>{item}<i>◆</i></span>)}
              </span>
            ))}
          </div>
        </div>

        {/* ───────────── HOW IT WORKS ───────────── */}
        <section className="section shell" id="how" aria-labelledby="how-title">
          <div className="sectionHead">
            <p className="eyebrow">01 / HOW IT GROWS</p>
            <h2 id="how-title">Three moving parts. <em>No conversation content.</em></h2>
            <p>Agents are outbound-only. The hub is the only component that needs a reachable port, and it belongs on Tailscale, WireGuard, or behind an authenticated proxy.</p>
          </div>
          <ol className="flow">
            <li className="flowNode">
              <span className="flowNo">A</span>
              <h3>Agents read locally</h3>
              <p>On each macOS or Linux machine, a small agent reads the usage logs your coding tools already write, with durable cursors so nothing is counted twice.</p>
              <ul className="chips" aria-label="Example sources"><li>claude-code</li><li>codex</li><li>+6 more</li></ul>
            </li>
            <li className="flowPipe" aria-hidden="true">
              <span className="packet" /><span className="packet" /><span className="packet" />
              <b>METADATA ONLY</b>
            </li>
            <li className="flowNode">
              <span className="flowNo">B</span>
              <h3>The hub remembers</h3>
              <p>One Go binary with SQLite and a server-rendered dashboard. It stores usage metadata only: token counts, models, providers, timestamps, and machine health.</p>
              <ul className="chips" aria-label="Hub stack"><li>Go</li><li>SQLite</li><li>auth required</li></ul>
            </li>
            <li className="flowPipe" aria-hidden="true">
              <span className="packet" /><span className="packet" /><span className="packet" />
              <b>LIFETIME TOTAL</b>
            </li>
            <li className="flowNode flowNodeLive">
              <span className="flowNo">C</span>
              <h3>The creature grows</h3>
              <p>Evolution is derived from your lifetime token count, deterministically. Cross a threshold and your Tokemon takes its next form.</p>
              <ul className="chips" aria-label="Evolution"><li>19 forms</li><li>0 → 1T</li></ul>
            </li>
          </ol>
        </section>

        {/* ───────────── EVOLUTION ───────────── */}
        <section className="evolution" id="forms" aria-labelledby="forms-title">
          <div className="section shell">
            <div className="sectionHead sectionHeadSplit">
              <div>
                <p className="eyebrow">02 / EVOLUTION ARCHIVE</p>
                <h2 id="forms-title">One signal. <em>Nineteen forms.</em></h2>
              </div>
              <p>Powers of ten carry the creature through one billion tokens. After that, denser 1–2–5 checkpoints keep the late forms moving all the way to one trillion. Four specimens are public. The rest you meet on your own dashboard.</p>
            </div>

            <ol className="ladder">
              {specimens.map((specimen, index) => {
                const group = sealedGroups[index];
                return [
                  <li className="specimen" key={specimen.stage}>
                    <div className="specimenFrame">
                      <div className="specimenMeta"><span>No.{specimen.stage}</span><span>{specimen.threshold}</span></div>
                      <div className="specimenArt">
                        <img src={specimen.image} alt={`${specimen.name}, Tokemon evolution stage ${specimen.stage}`} width="640" height="640" loading="lazy" decoding="async" />
                      </div>
                      <h3>{specimen.name}</h3>
                      <p>{specimen.blurb}</p>
                      <span className="specimenUnlock">UNLOCKS AT {specimen.thresholdFull.toUpperCase()}</span>
                    </div>
                  </li>,
                  group ? (
                    <li className="vault" key={group.range} aria-label={`Stages ${group.range}: ${group.label}, classified. Thresholds ${group.thresholdRange} tokens.`}>
                      <div className="vaultHead">
                        <span className="stamp">CLASSIFIED</span>
                        <strong>{group.label}</strong>
                        <span className="vaultRange">STAGES {group.range}</span>
                      </div>
                      <ul className="vaultSlots" aria-hidden="true">
                        {group.stages.map((sealed) => (
                          <li key={sealed.stage}>
                            <Silhouette stage={sealed.stage} />
                            <span className="slotNo">{sealed.stage}</span>
                            <span className="slotAt">{sealed.threshold}</span>
                          </li>
                        ))}
                      </ul>
                      <p className="vaultCopy">{group.copy}</p>
                    </li>
                  ) : null,
                ];
              })}
            </ol>
            <p className="archiveNote"><span>4 / 19 DOCUMENTED</span> Four specimens shown · fifteen forms remain sealed as a teaser from the complete 19-form archive.</p>
          </div>
        </section>

        {/* ───────────── DASHBOARD ───────────── */}
        <section className="section shell" id="dashboard" aria-labelledby="dash-title">
          <div className="sectionHead sectionHeadCenter">
            <p className="eyebrow">03 / THE INSTRUMENT PANEL</p>
            <h2 id="dash-title">See exactly where <em>your tokens go.</em></h2>
            <p>Lifetime totals, token mix, a 53-week activity field, machine health, and breakdowns by project, harness, model, and machine. Quiet by default, detailed on demand.</p>
          </div>

          <figure className="showcase">
            <div className="browser">
              <div className="browserBar" aria-hidden="true">
                <span className="dots"><i /><i /><i /></span>
                <span className="url">tokemon.example.ts.net</span>
                <span className="lock">AUTH</span>
              </div>
              <div className="browserView">
                <img src="/tokemon/dashboard-sample.webp" alt="The Tokemon dashboard with representative sample data: lifetime token counter, input and output split, next-evolution progress, summary cards, a 53-week token activity field, and tables for projects, harnesses, models, and machines. The creature panel is redacted with a sealed silhouette." width="1280" height="1039" loading="lazy" decoding="async" />
                <span className="redactStamp" aria-hidden="true">SEALED</span>
                <span className="pin" style={{ "--px": "50.5%", "--py": "11.2%" } as CSSProperties} aria-hidden="true">1</span>
                <span className="pin" style={{ "--px": "49.4%", "--py": "36.7%" } as CSSProperties} aria-hidden="true">2</span>
                <span className="pin" style={{ "--px": "14.6%", "--py": "55.6%" } as CSSProperties} aria-hidden="true">3</span>
                <span className="pin" style={{ "--px": "10.8%", "--py": "73%" } as CSSProperties} aria-hidden="true">4</span>
              </div>
            </div>
            <figcaption>Representative sample data. The creature panel is sealed here; on your hub it shows your current form.</figcaption>
          </figure>

          <ol className="legend">
            <li><span>1</span><div><h3>Lifetime counter</h3><p>One global number with its token mix. Filters never change it.</p></div></li>
            <li><span>2</span><div><h3>Next evolution</h3><p>Tokens to go and progress toward the next threshold.</p></div></li>
            <li><span>3</span><div><h3>Activity field</h3><p>53 weeks of daily usage at a glance.</p></div></li>
            <li><span>4</span><div><h3>Breakdowns</h3><p>Projects, harnesses, models, and machines with share.</p></div></li>
          </ol>

          <ul className="routes" aria-label="Dashboard pages">
            <li><code>/analytics</code><span>Trends, comparisons, filters, usage share over time, and export.</span></li>
            <li><code>/tokedex</code><span>Model tiers, aliases, pricing provenance, observed usage.</span></li>
            <li><code>/sessions</code><span>A filterable, metadata-only session timeline with export.</span></li>
            <li><code>/insights</code><span>Deterministic, evidence-backed aggregate cards. Local by default.</span></li>
          </ul>
        </section>

        {/* ───────────── TOKEDEX ───────────── */}
        <section className="tokedex" id="tokedex" aria-labelledby="dex-title">
          <div className="section shell dexLayout">
            <div className="sectionHead">
              <p className="eyebrow">04 / TOKEDEX + ESTIMATES</p>
              <h2 id="dex-title">A catalog, not a <em>leaderboard.</em></h2>
              <p>The Tokedex is a static YAML model catalog: canonical names, aliases, provider, and a simple S–D or Unranked tier. Known models can show an estimated API-style cost with its pricing source. Unknown models stay unknown.</p>
              <ul className="tiers" aria-label="Tier scale">
                <li className="tierS">S</li><li className="tierA">A</li><li className="tierB">B</li><li className="tierC">C</li><li className="tierD">D</li><li className="tierU"><span aria-hidden="true">?</span><span className="srOnly">Unranked</span></li>
              </ul>
              <ul className="dexPoints">
                <li><span><b>Aliases resolve</b> without rewriting the raw event.</span></li>
                <li><span><b>No invented prices</b> and no subscription math.</span></li>
                <li><span><b>Estimated cost</b> is always labeled as an estimate.</span></li>
              </ul>
            </div>
            <figure className="codeCard dexCard">
              <div className="codeCardBar"><span>catalog/models.yaml</span><span className="tierBadge">TIER B</span></div>
              <pre><YamlBlock source={catalogExcerpt} /></pre>
              <figcaption>Excerpt from the bundled catalog. <a href={links.catalog}>View the file <Ext /></a></figcaption>
            </figure>
          </div>
        </section>

        {/* ───────────── PRIVACY ───────────── */}
        <section className="section shell" id="privacy" aria-labelledby="privacy-title">
          <div className="sectionHead sectionHeadSplit">
            <div>
              <p className="eyebrow">05 / PRIVACY BY DEFAULT</p>
              <h2 id="privacy-title">It counts the fuel. <em>Not the conversation.</em></h2>
            </div>
            <p>One outbound guard runs before every upload. Unknown fields, content, credentials, and full paths are rejected before a request opens, and a rejected batch never advances the local cursor.</p>
          </div>

          <div className="customs">
            <figure className="codeCard leaves">
              <div className="codeCardBar"><span className="okDot" aria-hidden="true" /><span>WHAT LEAVES YOUR MACHINE</span><span className="muted">one normalized event</span></div>
              <pre><JsonBlock value={sampleEvent} /></pre>
              <figcaption>The normalized event shape from the product spec. Projects are lowercase basenames, never paths.</figcaption>
            </figure>

            <div className="guardLine" aria-hidden="true"><span>PRIVACY GUARD</span></div>

            <div className="never">
              <div className="neverHead"><span className="noDot" aria-hidden="true" /><h3>What never does</h3></div>
              <ul>
                {neverLeaves.map((item) => (
                  <li key={item}><span className="neverX" aria-hidden="true">×</span><span className="neverText">{item}</span><span className="neverBar" aria-hidden="true" /></li>
                ))}
              </ul>
              <div className="inspect">
                <p>Don&rsquo;t take our word for it. Print the exact outbound payload:</p>
                <pre className="inlineTerm"><code><b>$</b> tokemon inspect</code></pre>
                <p className="small">Set <code>TOKEMON_INCLUDE_PROJECTS=false</code> to drop even project basenames.</p>
              </div>
            </div>
          </div>
        </section>

        {/* ───────────── ADAPTERS ───────────── */}
        <section className="adaptersSection" id="adapters" aria-labelledby="adapters-title">
          <div className="section shell">
            <div className="sectionHead sectionHeadSplit">
              <div>
                <p className="eyebrow">06 / ADAPTERS IN THE FIELD</p>
                <h2 id="adapters-title">Eight adapters. <em>One creature.</em></h2>
              </div>
              <p>Provider quirks stay inside each adapter. Pick a subset with <code>TOKEMON_ADAPTERS</code>; anything without a native adapter can feed the generic JSONL path.</p>
            </div>
            <ul className="cards">
              {adapters.map((adapter) => (
                <li className={adapter.enabled === "Opt-in" ? "card cardOptIn" : "card"} key={adapter.id}>
                  <div className="cardTop"><span>#{adapter.no}</span><span className="cardFormat">{adapter.format}</span></div>
                  <h3>{adapter.name}</h3>
                  <code className="cardId">{adapter.id}</code>
                  <dl>
                    <div><dt>Reads</dt><dd>{adapter.habitat}</dd></div>
                    <div><dt>Status</dt><dd className="cardStatus">{adapter.enabled}</dd></div>
                  </dl>
                </li>
              ))}
            </ul>
          </div>
        </section>

        {/* ───────────── RELEASE ───────────── */}
        <section className="section shell" id="release" aria-labelledby="release-title">
          <div className="releaseLayout">
            <div className="sectionHead">
              <p className="eyebrow">07 / PATCH NOTES</p>
              <h2 id="release-title">v{version}: locked, signed, <em>catalogued.</em></h2>
              <p>The first signed Tokemon release. It also ships the 0.3.3 changes, which were tagged but never published.</p>
              <a className="textLink" href={links.release}>Latest release <Ext /></a>
              <a className="textLink" href={links.changelog}>Full changelog <Ext /></a>
            </div>
            <ul className="notes">
              {releaseNotes.map((note) => (
                <li key={note.title}>
                  <span className="noteTag">{note.tag}</span>
                  <div><h3>{note.title}</h3><p>{note.copy}</p></div>
                </li>
              ))}
            </ul>
          </div>

          <div className="trust" aria-labelledby="trust-title">
            <div className="trustHead">
              <h3 id="trust-title" className="trustTitle"><span className="seal" aria-hidden="true">✓</span>Signed releases</h3>
              <p>Installers reject unsigned or tampered assets. Missing signatures are a hard failure.</p>
            </div>
            <ul>
              {trust.map((item) => (
                <li key={item.title}><span className="trustMark">{item.mark}</span><div><strong>{item.title}</strong><p>{item.copy}</p></div></li>
              ))}
            </ul>
          </div>
        </section>

        {/* ───────────── INSTALL ───────────── */}
        <section className="install" id="install" aria-labelledby="install-title">
          <div className="section shell">
            <div className="sectionHead sectionHeadCenter">
              <p className="eyebrow">08 / QUICK INSTALL</p>
              <h2 id="install-title">From zero to <em>hatched.</em></h2>
              <p>Raise a hub with Docker Compose, then enroll each machine with the signed native installer. Run both from a checkout of the release tag.</p>
            </div>

            <div className="terminalLead">
              <div className="terminal terminalSmall">
                <div className="terminalBar"><span className="dots" aria-hidden="true"><i /><i /><i /></span><span className="terminalTitle">00 · get the release</span><CopyButton text={cloneCommand} label="clone command" /></div>
                <pre><ShellBlock source={cloneCommand} /></pre>
              </div>
            </div>

            <div className="terminals">
              <div className="step">
                <div className="stepHead"><span className="stepNo">01</span><div><h3>Raise the hub</h3><p>A persistent dashboard with SQLite under <code>./data</code>. Keep it on a private network.</p></div></div>
                <div className="terminal">
                  <div className="terminalBar"><span className="dots" aria-hidden="true"><i /><i /><i /></span><span className="terminalTitle">hub · docker compose</span><CopyButton text={hubCommand} label="hub commands" /></div>
                  <pre><ShellBlock source={hubCommand} /></pre>
                </div>
              </div>
              <div className="step">
                <div className="stepHead"><span className="stepNo">02</span><div><h3>Enroll a machine</h3><p>Downloads v{version}, verifies Cosign signatures and checksums, and installs a launchd or systemd user service.</p></div></div>
                <div className="terminal">
                  <div className="terminalBar"><span className="dots" aria-hidden="true"><i /><i /><i /></span><span className="terminalTitle">agent · install-agent.sh</span><CopyButton text={agentCommand} label="agent install command" /></div>
                  <pre><ShellBlock source={agentCommand} /></pre>
                </div>
              </div>
            </div>

            <div className="installFoot">
              <details className="localTry">
                <summary>Just kicking the tires? Run it from source on loopback</summary>
                <div className="terminal terminalSmall">
                  <div className="terminalBar"><span className="dots" aria-hidden="true"><i /><i /><i /></span><span className="terminalTitle">local · go run</span><CopyButton text={localCommand} label="local run commands" /></div>
                  <pre><ShellBlock source={localCommand} /></pre>
                </div>
                <p><code>--dev-loopback</code> permits an empty ingest token only on a loopback listener. Then open <code>localhost:8080</code>.</p>
              </details>
              <ul className="requirements" aria-label="Requirements">
                <li>Hub: Docker with Compose</li>
                <li>Agents: macOS or Linux, arm64 or amd64</li>
                <li>Installer requires Cosign</li>
                <li>From source: Go 1.26.9+</li>
              </ul>
              <div className="installLinks">
                <a className="button buttonPrimary" href={links.release}>Download v{version} <Ext /></a>
                <a className="button buttonGhost" href={links.deployment}>Read the deployment guide <Ext /></a>
              </div>
            </div>
          </div>
        </section>

        {/* ───────────── ABOUT ───────────── */}
        <section className="section shell about" id="about" aria-labelledby="about-title">
          <div className="aboutArt" aria-hidden="true">
            <img src="/tokemon/stage-00.webp" alt="" width="640" height="640" loading="lazy" decoding="async" />
          </div>
          <div>
            <p className="eyebrow">09 / ABOUT THE EXPERIMENT</p>
            <h2 id="about-title">A tiny observability tool that <em>accidentally hatched something.</em></h2>
            <p className="aboutCopy">Tokemon is an independent, MIT-licensed experiment in making invisible coding-agent usage feel tangible, without turning private work into telemetry. No maintainer telemetry by default, no hosted service, no sign-up. Your hub, your data.</p>
            <div className="resourceLinks" aria-label="Tokemon resources">
              <a href={links.repo}><span>Source repository</span><Ext /></a>
              <a href={links.release}><span>Latest release · v{version}</span><Ext /></a>
              <a href={links.deployment}><span>Deployment guide</span><Ext /></a>
              <a href={links.spec}><span>Product spec</span><Ext /></a>
              <a href={links.license}><span>MIT license</span><Ext /></a>
            </div>
          </div>
        </section>
      </main>

      <footer className="footer">
        <div className="shell">
          <div className="footerHero">
            <p className="footerWord" aria-hidden="true">TOKEMON</p>
            <p className="footerTag">Your coding tokens are evolving.</p>
            <a className="button buttonPrimary" href="#install">Start the hatch <Arrow /></a>
          </div>
          <div className="footerGrid">
            <div className="footerBrand">
              <img src="/tokemon/token-dex-96.webp" alt="" width="40" height="40" loading="lazy" />
              <div><strong>TOKEMON</strong><span>LOCAL-FIRST TOKEN GARDEN</span></div>
            </div>
            <nav aria-label="Footer">
              <a href={links.repo}>GitHub</a>
              <a href={links.release}>Release v{version}</a>
              <a href={links.deployment}>Deployment</a>
              <a href={links.security}>Security</a>
              <a href={links.license}>License</a>
              <a href="#top">Back to top ↑</a>
            </nav>
          </div>
          <p className="disclaimer">Independent, open source, and MIT licensed. Made just for fun. Not affiliated with, endorsed by, or connected to Pokémon or The Pokémon Company.</p>
        </div>
      </footer>
    </>
  );
}
