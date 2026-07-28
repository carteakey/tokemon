const forms = [
  { stage: "00", name: "The first signal", image: "/tokemon/stage-00.png" },
  { stage: "08", name: "A serious appetite", image: "/tokemon/stage-08.png" },
  { stage: "13", name: "Inference Leviathan", image: "/tokemon/stage-13.png" },
  { stage: "14", name: "Parameter Colossus", image: "/tokemon/stage-14.png" },
  { stage: "15", name: "World Weaver", image: "/tokemon/stage-15.png" },
  { stage: "16", name: "Cosmic Architect", image: "/tokemon/stage-16.png" },
  { stage: "17", name: "Universe Engine", image: "/tokemon/stage-17.png" },
  { stage: "18", name: "The Singularity", image: "/tokemon/stage-18.png" },
];

const providers = [
  "CLAUDE CODE",
  "CODEX",
  "GITHUB COPILOT CLI",
  "OPENCODE",
  "ANTIGRAVITY",
  "OPENCLAW",
];

const features = [
  {
    number: "01",
    title: "See the shape of your usage",
    copy: "Lifetime totals, token mix, activity, providers, models, projects, and machines in one quiet instrument panel.",
  },
  {
    number: "02",
    title: "Keep the conversation yours",
    copy: "The default event payload contains usage metadata only. Prompts, responses, source code, and repository contents stay local.",
  },
  {
    number: "03",
    title: "Grow across your machines",
    copy: "Run a persistent SQLite hub and authenticated agents on macOS or Linux with durable cursors and checksum-verified installs.",
  },
];

export default function Home() {
  return (
    <main>
      <nav className="nav shell" aria-label="Primary navigation">
        <a className="brand" href="#top" aria-label="Tokemon home">
          <img src="/tokemon/token-dex.png" alt="" width="36" height="36" />
          <span>TOKEMON</span>
        </a>
        <div className="navLinks">
          <a href="#install">Install</a>
          <a href="#features">Features</a>
          <a href="#artifacts">Forms</a>
          <a href="#about">About</a>
        </div>
        <a className="navCta" href="https://github.com/carteakey/tokemon/releases/tag/v0.3.2">
          v0.3.2 <span>↗</span>
        </a>
      </nav>

      <section className="hero shell" id="top">
        <div className="heroCopy">
          <p className="eyebrow"><span className="pulse" /> LOCAL-FIRST TOKEN GARDEN</p>
          <h1>Your coding tokens are <em>evolving.</em></h1>
          <p className="lede">Tokemon turns coding-agent usage metadata into one strange little creature that grows with every token—without turning your private work into telemetry.</p>
          <div className="heroActions">
            <a className="primary" href="#install">Start the hatch <span>→</span></a>
            <a className="secondary" href="#artifacts">Meet the forms</a>
          </div>
          <div className="trustRow" aria-label="Product qualities">
            <span>● SQLite</span><span>● Six adapters</span><span>● MIT licensed</span>
          </div>
        </div>
        <div className="heroArtifact" aria-label="Featured Tokemon artifact">
          <div className="scanline" />
          <div className="artifactTop"><span>SPECIMEN / 08</span><span>LIVE SIGNAL</span></div>
          <img src="/tokemon/stage-08.png" alt="An original evolved Tokemon creature" width="640" height="640" />
          <div className="artifactBottom">
            <div><small>CURRENT FORM</small><strong>UNNAMED_08</strong></div>
            <div className="level"><small>LIFETIME TOKENS</small><strong>10,000,000</strong></div>
          </div>
        </div>
      </section>

      <section className="ticker" aria-label="Product summary"><div>FEED IT TOKENS <i>◆</i> WATCH IT EVOLVE <i>◆</i> KEEP YOUR CONTENT PRIVATE <i>◆</i> FEED IT TOKENS <i>◆</i> WATCH IT EVOLVE</div></section>

      <section className="section shell" id="install">
        <div className="sectionIntro">
          <p className="eyebrow">01 / QUICK INSTALL</p>
          <h2>From zero to <em>hatched.</em></h2>
          <p>Run the garden locally, connect an agent, and let usage metadata do the rest. Use the release installer for a managed macOS or Linux setup.</p>
        </div>
        <div className="steps">
          <article className="step"><span className="stepNo">01</span><h3>Start the garden</h3><p>Run the server with a local SQLite database.</p><code><b>$</b> go run ./cmd/tokemon serve<br />&nbsp;&nbsp;--database ./data/tokemon.db</code></article>
          <article className="step"><span className="stepNo">02</span><h3>Connect a machine</h3><p>Point the agent at your local server.</p><code><b>$</b> go run ./cmd/tokemon agent<br />&nbsp;&nbsp;--server http://127.0.0.1:8080</code></article>
          <article className="step"><span className="stepNo">03</span><h3>Watch it grow</h3><p>Open the quiet dashboard and meet your current form.</p><code><b>↗</b> http://localhost:8080</code></article>
        </div>
        <div className="installLinks">
          <a className="textLink" href="https://github.com/carteakey/tokemon/blob/main/DEPLOYMENT.md">Read deployment guide <span>↗</span></a>
          <a className="textLink" href="https://github.com/carteakey/tokemon/releases/tag/v0.3.2">Download v0.3.2 <span>↗</span></a>
        </div>
        <p className="requirement">Requires Go 1.26+ <span>·</span> macOS and Linux deployment paths included <span>·</span> Keep ingest private or set a token</p>
      </section>

      <section className="featureSection" id="features">
        <div className="section shell">
          <div className="sectionIntro"><p className="eyebrow">02 / THE SIGNAL</p><h2>Useful telemetry.<br /><em>Human scale.</em></h2></div>
          <div className="featureGrid">
            {features.map((feature) => <article className="feature" key={feature.number}>
              <span className="featureNo">{feature.number}</span>
              <h3>{feature.title}</h3>
              <p>{feature.copy}</p>
            </article>)}
          </div>
        </div>
      </section>

      <section className="artifactSection" id="artifacts">
        <div className="section shell">
          <div className="sectionIntro splitIntro">
            <div><p className="eyebrow">03 / EVOLUTION ARCHIVE</p><h2>One signal.<br /><em>Nineteen forms.</em></h2></div>
            <p>Powers of ten carry the creature to one billion tokens. After that, denser 1–2–5 checkpoints keep the late forms moving: leviathan, colossus, weaver, architect, engine, singularity.</p>
          </div>
          <div className="forms">
            {forms.map((form) => <article className="form" key={form.stage}>
              <div className="formMeta"><span>STAGE {form.stage}</span><span>{form.name}</span></div>
              <img src={form.image} alt={`Original Tokemon evolution artifact, stage ${form.stage}`} width="640" height="640" />
              <div className="formTicks">+ + + + + + + + + + + +</div>
            </article>)}
          </div>
          <p className="archiveNote">Eight signals recovered from the complete 19-form archive · final form at 1T tokens.</p>
        </div>
      </section>

      <section className="section shell principles">
        <div className="privacyBlock">
          <p className="eyebrow">04 / PRIVACY BY DEFAULT</p>
          <h2>It counts the fuel.<br /><em>Not the conversation.</em></h2>
          <p>Tokemon stores the usage metadata needed to grow your creature and power the dashboard. Your creative work stays yours.</p>
        </div>
        <div className="dataGrid">
          <article className="kept"><span>COLLECTED</span><ul><li>Token totals</li><li>Model identifiers</li><li>Timestamps</li><li>Machine aliases</li></ul></article>
          <article className="never"><span>NEVER BY DEFAULT</span><ul><li>Prompts or responses</li><li>Source code</li><li>Repository contents</li><li>Conversation titles</li></ul></article>
        </div>
      </section>

      <section className="compat shell">
        <p>ADAPTERS IN THE FIELD</p>
        <div>{providers.map((provider) => <strong key={provider}>{provider}</strong>)}</div>
      </section>

      <section className="section shell about" id="about">
        <div>
          <p className="eyebrow">05 / ABOUT THE EXPERIMENT</p>
          <h2>A tiny observability tool that <em>accidentally hatched something.</em></h2>
        </div>
        <div className="aboutCopy">
          <p>Tokemon is an independent, MIT-licensed experiment in making invisible coding-agent usage feel tangible—without turning private work into telemetry.</p>
          <div className="resourceLinks" aria-label="Tokemon resources">
            <a href="https://github.com/carteakey/tokemon">Source repository <span>↗</span></a>
            <a href="https://github.com/carteakey/tokemon/releases/tag/v0.3.2">Latest release <span>↗</span></a>
            <a href="https://github.com/carteakey/tokemon/blob/main/docs/tokemon-spec-v0.2.md">Product spec <span>↗</span></a>
          </div>
          <p className="aboutNote">Independent and just for fun. Not affiliated with, endorsed by, or connected to Pokémon or The Pokémon Company.</p>
          <a className="textLink" href="#install">Run it locally <span>→</span></a>
        </div>
      </section>

      <footer>
        <div className="shell footerInner">
          <div className="footerBrand"><img src="/tokemon/token-dex.png" alt="" width="46" height="46" /><div><strong>TOKEMON</strong><span>YOUR TOKENS ARE EVOLVING</span></div></div>
          <p>Independent, open source, and MIT licensed. Not affiliated with, endorsed by, or connected to Pokémon or The Pokémon Company.</p>
          <a href="#top">BACK TO TOP ↑</a>
        </div>
      </footer>
    </main>
  );
}
