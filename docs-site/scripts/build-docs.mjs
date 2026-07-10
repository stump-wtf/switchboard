// Generate docs-site/docs-generated/ from the repo's SDD-canonical design record at build time:
//   docs/adrs/                     -> /decisions   (ADRs, MADR)
//   docs/openspec/specs/{cap}/     -> /specs       (OpenSpec spec.md + design.md pairs)
//   docs/design/NN-slug.md         -> /design      (design language, ADR-0016)
//   docs/reference/*.yaml          -> /reference   (machine-readable contracts)
//   docs/prfaq.md                  -> /prfaq
// These files are the single source of truth; this script only adapts them for Docusaurus
// (sidebar order, status lines, cross-link rewrites). docs-generated/ is gitignored.
//
// Generated spec/design pages are emitted with `format: md` (CommonMark) so machine-authored
// constructs like `<uuid>` or `<agent_id>` are harmless inline HTML rather than MDX JSX errors.
// Mermaid fenced blocks still render as diagrams under CommonMark.
//
// Run: `npm run build-content` (invoked automatically by `npm start` / `npm run build`).

import {
  readFileSync, writeFileSync, mkdirSync, rmSync, readdirSync, copyFileSync, statSync,
} from 'node:fs';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const __dirname = dirname(fileURLToPath(import.meta.url));
const SITE = join(__dirname, '..');
const REPO = join(SITE, '..');
const ADR_SRC = join(REPO, 'docs', 'adrs');
const SPEC_SRC = join(REPO, 'docs', 'openspec', 'specs');
const DESIGN_SRC = join(REPO, 'docs', 'design');
const REF_SRC = join(REPO, 'docs', 'reference');
const OUT = join(SITE, 'docs-generated');
const STATIC_REF = join(SITE, 'static', 'reference');

const GITHUB = 'https://github.com/joestump/switchboard';
const GITHUB_RAW = 'https://raw.githubusercontent.com/joestump/switchboard/main';

// ---- discover sources ----
const adrFiles = readdirSync(ADR_SRC).filter((f) => /^ADR-\d+.*\.md$/.test(f)).sort();
const capDirs = readdirSync(SPEC_SRC)
  .filter((d) => statSync(join(SPEC_SRC, d)).isDirectory())
  .sort();
const refFiles = readdirSync(REF_SRC).filter((f) => /\.ya?ml$/.test(f)).sort();
const designFiles = readdirSync(DESIGN_SRC).filter((f) => /^\d+-.*\.md$/.test(f)).sort();

// ---- clean + scaffold ----
rmSync(OUT, { recursive: true, force: true });
mkdirSync(join(OUT, 'decisions'), { recursive: true });
mkdirSync(join(OUT, 'specs'), { recursive: true });
mkdirSync(join(OUT, 'design'), { recursive: true });
mkdirSync(join(OUT, 'reference'), { recursive: true });
rmSync(STATIC_REF, { recursive: true, force: true });
mkdirSync(STATIC_REF, { recursive: true });

/** Split `---\n..\n---\n<body>` into {fm, body}. */
function splitFrontmatter(text) {
  const m = text.match(/^---\n([\s\S]*?)\n---\n?([\s\S]*)$/);
  return m ? { fm: m[1], body: m[2] } : { fm: '', body: text };
}
function fmValue(fm, key) {
  const m = fm.match(new RegExp(`^${key}:\\s*(.+)$`, 'm'));
  return m ? m[1].trim() : null;
}
// MDX reads `<https://…>` autolinks as JSX and errors. Rewrite them to real markdown links.
// (Only needed for the .mdx landing page + ADR pages; spec/design pages use format: md.)
function sanitizeMdx(s) {
  return s.replace(/<((?:https?):\/\/[^>\s]+)>/g, '[$1]($1)');
}
// docs/README.md (the design-index) has no page in the site; point at GitHub.
function rewriteRepoLinks(s) {
  return s.replace(/\]\(\.\.\/README\.md([^)]*)\)/g, `](${GITHUB}/blob/main/docs/README.md$1)`);
}
// Rewrite links to ADR / spec / reference source files onto their rendered site routes.
// Patterns tolerate any relative prefix (../, ../../../, docs/, etc.).
function rewriteDesignLinks(s) {
  return s
    // ...adrs/ADR-0007-foo.md[#anchor] -> /decisions/ADR-0007-foo[#anchor]
    .replace(/\]\([^)]*?adrs\/(ADR-\d+[a-z0-9-]*)\.md([^)]*)\)/g, '](/decisions/$1$2)')
    // ...openspec/specs/{cap}/spec.md -> /specs/{cap}/spec  (and design.md -> /specs/{cap}/design)
    .replace(/\]\([^)]*?openspec\/specs\/([a-z0-9-]+)\/(spec|design)\.md([^)]*)\)/g, '](/specs/$1/$2$3)')
    // ...reference/openapi.yaml -> /reference/openapi
    .replace(/\]\([^)]*?reference\/(openapi|asyncapi)\.ya?ml([^)]*)\)/g, '](/reference/$1$2)');
}

// ---- landing page ----
writeFileSync(join(OUT, 'intro.mdx'), `---
slug: /
title: Switchboard
sidebar_label: Overview
sidebar_position: 0
hide_title: true
hide_table_of_contents: true
---

import Link from '@docusaurus/Link';

<div className="sb-hero">
  <div className="sb-hero__eyebrow">MCP server · durable todo queue · local web UI</div>
  <h1 className="sb-hero__title">Switchboard</h1>
  <p className="sb-hero__tagline"><em>Many lines come in. The operator verifies each caller, and patches it through.</em></p>
  <p className="sb-hero__lead">Switchboard receives inbound webhooks, verifies each one per source, and turns it into a durable todo that agents claim and complete over scoped, human‑vended MCP endpoints. One box: receive · verify · patch through.</p>
  <div className="sb-hero__cta">
    <Link className="button button--primary button--lg" to="/decisions">Read the decisions →</Link>
    <Link className="button button--secondary button--lg" to="/specs">Browse the specs</Link>
    <Link className="button button--outline button--lg" to="/prfaq">Read the PRFAQ</Link>
  </div>
</div>

<div className="sb-tiles">

  <Link className="sb-tile" to="/decisions/ADR-0014-ingestion-adapters-push-pull">
    <svg className="sb-tile__icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round"><path d="M3 13v5a2 2 0 0 0 2 2h14a2 2 0 0 0 2-2v-5"/><path d="M8 9l4 4 4-4"/><path d="M12 2v11"/></svg>
    <div className="sb-tile__title">Push &amp; pull ingestion adapters</div>
    <p className="sb-tile__body">Push webhooks (GitHub, Stripe, Slack, Docker Hub, generic) and pull queue adapters (Redis, with SQS/NATS/AMQP to follow) normalize into the same todo — each with an enforced trust mode. Pull adapters ack the source only after the todo is durably stored, so nothing is lost at the boundary.</p>
  </Link>

  <Link className="sb-tile" to="/decisions/ADR-0007-todos-as-core-primitive">
    <svg className="sb-tile__icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round"><path d="M9 6h11"/><path d="M9 12h11"/><path d="M9 18h11"/><path d="M4 5.5l1 1 2-2"/><path d="M4 11.5l1 1 2-2"/><path d="M4 17.5l1 1 2-2"/></svg>
    <div className="sb-tile__title">Durable todo work‑queue</div>
    <p className="sb-tile__body">Every event becomes a work‑item with a lifecycle — claimed under a lease, completed with an ack, deduped by idempotency key. A crashed worker's todo re‑surfaces; nothing is read‑once and lost.</p>
  </Link>

  <Link className="sb-tile" to="/decisions/ADR-0008-human-principal-vended-endpoints">
    <svg className="sb-tile__icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round"><circle cx="8" cy="8" r="4"/><path d="M10.8 10.8L20 20"/><path d="M17 17l2-2"/><path d="M14.5 14.5l2-2"/></svg>
    <div className="sb-tile__title">Per‑agent vended MCP endpoints</div>
    <p className="sb-tile__body">Humans are the accountable principals; each agent is vended a scoped MCP endpoint (queues + verb allowlist). The credential is stored hashed in Postgres, short‑lived and revocable — revoke = kill the endpoint.</p>
  </Link>

  <Link className="sb-tile" to="/decisions/ADR-0009-personas-as-scoped-agent-cards">
    <svg className="sb-tile__icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round"><rect x="3" y="7" width="13" height="10" rx="1.5"/><path d="M7.5 4.5H19a1.5 1.5 0 0 1 1.5 1.5v9"/></svg>
    <div className="sb-tile__title">Personas as A2A Agent Cards</div>
    <p className="sb-tile__body">One agent, many least‑privilege faces. A persona is a human‑authored prompt plus a verb subset; its advertised skills are derived from what's actually vended, published as an A2A Agent Card.</p>
  </Link>

  <Link className="sb-tile" to="/decisions/ADR-0010-a2a-discovery-human-vended-friending">
    <svg className="sb-tile__icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round"><circle cx="6" cy="12" r="2.5"/><circle cx="18" cy="12" r="2.5"/><path d="M8.5 12h3"/><path d="M12.5 10.7l1.3 1.3 2.2-2.2"/></svg>
    <div className="sb-tile__title">Human‑approved friending</div>
    <p className="sb-tile__body">Agents discover peers over A2A and send a scoped friend request. Approval lands as a todo in the target human's queue — and approving is the vend. Per‑direction, revocable, non‑transitive.</p>
  </Link>

  <Link className="sb-tile" to="/decisions/ADR-0013-channels-push-delivery">
    <svg className="sb-tile__icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round"><path d="M6 9a6 6 0 0 1 12 0c0 5 2 6 2 6H4s2-1 2-6"/><path d="M10 20.5a2 2 0 0 0 4 0"/></svg>
    <div className="sb-tile__title">Push into your live session</div>
    <p className="sb-tile__body">When a harness is attached, switchboard pushes new todos straight into the session over the open Claude Code Channels standard — a doorbell, not the ledger. Offline? The durable queue keeps the work until it's pulled.</p>
  </Link>

  <Link className="sb-tile" to="/decisions/ADR-0001-web-stack-go-htmx-pico">
    <svg className="sb-tile__icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round"><rect x="3" y="4" width="18" height="16" rx="1.5"/><path d="M3 8h18"/><circle cx="5.8" cy="6" r="0.5"/><circle cx="7.8" cy="6" r="0.5"/></svg>
    <div className="sb-tile__title">Live operator board</div>
    <p className="sb-tile__body">A five‑view operator board — Board, Todos, Endpoints, Personas, Friends — on Go (net/http) + HTMX, wearing the brass‑and‑bakelite Operator design language and updating live over Server‑Sent Events, with the same trust badges the API and MCP surfaces carry.</p>
  </Link>

</div>

<div className="sb-trust">
  <span>Two families — webhook &amp; queue — each event's trust shown:</span>
  <span className="sb-badge sb-badge--signed">signed</span>
  <span className="sb-badge sb-badge--token">token</span>
  <span className="sb-badge sb-badge--open">open</span>
  <span className="sb-badge sb-badge--queue">queue</span>
  <span>— see <Link to="/decisions/ADR-0003-per-provider-ingestion-and-trust-model">ADR‑0003</Link>.</span>
</div>

:::note Design record
This site is the **canonical, SDD‑governed design record** for switchboard — ${adrFiles.length} architecture
decision records and ${capDirs.length} OpenSpec capability specs (each a requirements + design pair),
plus machine‑readable reference contracts. The MVP application code is built from these documents. The
name is the architecture: a manual telephone exchange took many incoming lines, an operator verified
the caller, and patched the line through — which is why these pages wear a switchboard‑era palette of
brass, bakelite, operator‑cream, oxblood, and patch‑cable tones.
:::

## Start here

- **[Decisions (ADRs)](/decisions)** — why switchboard is built the way it is: the stack, the PostgreSQL
  persistence, the trust model, the MCP contract, and the todo/agent‑vending/A2A layer.
- **[Specifications](/specs)** — ${capDirs.length} OpenSpec capabilities (RFC 2119 requirements + Mermaid design):
  ingestion, the durable todo queue, persistence, the MCP + agent tool surfaces, vended endpoints,
  identity, personas, friending, Channels push delivery, and the web UI.
- **[Design](/design)** — the "Operator" design language (ADR‑0016): tokens, components, the five
  operator‑board screens, voice, and the directions explored.
- **[Reference](/reference)** — the OpenAPI (HTTP surface) and AsyncAPI (SSE stream) contracts.
`);

// ---- PRFAQ -> /prfaq ----
{
  const raw = readFileSync(join(REPO, 'docs', 'prfaq.md'), 'utf8');
  const content = sanitizeMdx(rewriteRepoLinks(raw));
  writeFileSync(
    join(OUT, 'prfaq.md'),
    `---\nslug: /prfaq\ntitle: PRFAQ\nsidebar_label: PRFAQ\nsidebar_position: 1\n---\n\n${content}`,
  );
}

// ---- ADRs -> decisions/ ----
for (const f of adrFiles) {
  const num = parseInt(f.match(/ADR-(\d+)/)[1], 10);
  const raw = readFileSync(join(ADR_SRC, f), 'utf8');
  const { fm, body } = splitFrontmatter(raw);
  const status = fmValue(fm, 'status') || 'proposed';
  const date = fmValue(fm, 'date') || '';
  const deciders = fmValue(fm, 'decision-makers') || 'Joe Stump';

  let content = body.replace(
    /\]\(\.\.\/\.\.\/static\/tokens\.css\)/g,
    `](${GITHUB_RAW}/static/tokens.css)`,
  );
  content = rewriteDesignLinks(rewriteRepoLinks(content));
  content = content.replace(
    /^(#\s+.+)$/m,
    `$1\n\n> **Status** · ${status}  ·  **Date** · ${date}  ·  **Deciders** · ${deciders}`,
  );
  content = sanitizeMdx(content);
  const outFm = `---\nsidebar_position: ${num + 1}\n---\n\n`;
  writeFileSync(join(OUT, 'decisions', f), outFm + content);
}
writeFileSync(
  join(OUT, 'decisions', '_category_.json'),
  JSON.stringify(
    {
      label: 'Decisions (ADRs)',
      position: 2,
      link: { type: 'generated-index', slug: '/decisions', title: 'Architecture Decision Records', description: 'The decisions that shape switchboard — MADR format, newest ideas built on the oldest.' },
    },
    null,
    2,
  ),
);

// ---- OpenSpec specs -> specs/{cap}/{spec,design} ----
for (const cap of capDirs) {
  const dir = join(SPEC_SRC, cap);
  const specRaw = readFileSync(join(dir, 'spec.md'), 'utf8');
  const { fm, body } = splitFrontmatter(specRaw);
  const h1 = body.match(/^#\s+(SPEC-\d+):\s*(.+)$/m);
  const specNum = h1 ? parseInt(h1[1].slice(5), 10) : 999;
  const specId = h1 ? h1[1] : 'SPEC-????';
  const title = h1 ? h1[2].trim() : cap;
  const status = fmValue(fm, 'status') || 'draft';
  const date = fmValue(fm, 'date') || '';
  const implps = (fmValue(fm, 'implements') || '').replace(/[[\]]/g, '');
  const requires = (fmValue(fm, 'requires') || '').replace(/[[\]]/g, '');

  mkdirSync(join(OUT, 'specs', cap), { recursive: true });
  writeFileSync(
    join(OUT, 'specs', cap, '_category_.json'),
    JSON.stringify(
      {
        label: `${specId} · ${title}`,
        position: specNum,
        link: { type: 'generated-index', slug: `/specs/${cap}`, title: `${specId}: ${title}`, description: `Requirements and design for the ${title} capability.` },
      },
      null,
      2,
    ),
  );

  // spec.md page (requirements) — format: md for safety with machine-authored <…> constructs.
  let specBody = rewriteDesignLinks(rewriteRepoLinks(body));
  let meta = `> **SPEC** · ${specId}  ·  **Status** · ${status}  ·  **Date** · ${date}`;
  if (implps) meta += `  ·  **Implements** · ${implps}`;
  if (requires) meta += `  ·  **Requires** · ${requires}`;
  specBody = specBody.replace(/^(#\s+.+)$/m, `$1\n\n${meta}`);
  writeFileSync(
    join(OUT, 'specs', cap, 'spec.md'),
    `---\nsidebar_position: 1\nsidebar_label: Requirements\nformat: md\n---\n\n${specBody}`,
  );

  // design.md page (architecture + mermaid)
  const designRaw = readFileSync(join(dir, 'design.md'), 'utf8');
  const designBody = rewriteDesignLinks(rewriteRepoLinks(splitFrontmatter(designRaw).body));
  writeFileSync(
    join(OUT, 'specs', cap, 'design.md'),
    `---\nsidebar_position: 2\nsidebar_label: Design\nformat: md\n---\n\n${designBody}`,
  );
}
writeFileSync(
  join(OUT, 'specs', '_category_.json'),
  JSON.stringify(
    {
      label: 'Specifications',
      position: 3,
      link: { type: 'generated-index', slug: '/specs', title: 'OpenSpec Specifications', description: 'RFC 2119 requirements + Mermaid design for each switchboard capability. Each spec realizes one or more ADRs.' },
    },
    null,
    2,
  ),
);

// ---- Design language (docs/design/NN-slug.md) -> design/ ----
// Numeric filename prefix orders the sidebar; the emitted filename drops it so routes are clean
// (/design/overview, /design/design-language, …). Cross-links between design pages
// (`./NN-slug.md`) are rewritten onto those routes.
function rewriteDesignSectionLinks(s) {
  return s.replace(/\]\((?:\.\/)?\d+-([a-z0-9-]+)\.md([^)]*)\)/g, '](/design/$1$2)');
}
for (const f of designFiles) {
  const pos = parseInt(f.match(/^(\d+)-/)[1], 10);
  const slug = f.replace(/^\d+-/, '').replace(/\.md$/, '');
  const raw = readFileSync(join(DESIGN_SRC, f), 'utf8');
  const { fm, body } = splitFrontmatter(raw);
  const label = fmValue(fm, 'title') || slug;
  const content = rewriteDesignSectionLinks(rewriteDesignLinks(rewriteRepoLinks(body)));
  writeFileSync(
    join(OUT, 'design', `${slug}.md`),
    `---\nsidebar_position: ${pos}\nsidebar_label: ${label}\nformat: md\n---\n\n${content}`,
  );
}
writeFileSync(
  join(OUT, 'design', '_category_.json'),
  JSON.stringify(
    {
      label: 'Design',
      position: 4,
      link: { type: 'generated-index', slug: '/design', title: 'Design', description: 'The "Operator" design language (ADR-0016): tokens, components, screens, voice, and the directions explored.' },
    },
    null,
    2,
  ),
);

// ---- reference YAML contracts -> reference/ ----
const REF_META = {
  'openapi.yaml':  { pos: 1, title: 'OpenAPI — HTTP surface', blurb: 'The webhook ingestion endpoints and the local web-UI routes. Validated against OpenAPI 3.1.' },
  'asyncapi.yaml': { pos: 2, title: 'AsyncAPI — SSE stream', blurb: 'The Server-Sent Events schema for the live web UI: the `event-received` and `status` messages the dashboard and log screens subscribe to.' },
};
for (const file of refFiles) {
  const meta = REF_META[file] || { pos: 100, title: file, blurb: '' };
  const slug = file.replace(/\.ya?ml$/, '');
  const yaml = readFileSync(join(REF_SRC, file), 'utf8');
  copyFileSync(join(REF_SRC, file), join(STATIC_REF, file));
  writeFileSync(
    join(OUT, 'reference', `${slug}.md`),
    `---\nsidebar_position: ${meta.pos}\ntitle: ${meta.title}\nformat: md\n---\n\n# ${meta.title}\n\n${meta.blurb}\n\n` +
      `[⬇ Download the raw \`${file}\`](pathname:///switchboard/reference/${file}) · ` +
      `[view on GitHub](${GITHUB}/blob/main/docs/reference/${file})\n\n` +
      `\`\`\`\`yaml\n${yaml}\n\`\`\`\`\n`,
  );
}
writeFileSync(
  join(OUT, 'reference', '_category_.json'),
  JSON.stringify(
    {
      label: 'Reference',
      position: 5,
      link: { type: 'generated-index', slug: '/reference', title: 'Reference Contracts', description: 'Machine-readable interface contracts: the HTTP surface (OpenAPI) and the live SSE stream (AsyncAPI).' },
    },
    null,
    2,
  ),
);

console.log(`build-docs: ${adrFiles.length} ADRs + ${capDirs.length} specs + ${refFiles.length} reference contracts -> docs-generated/`);
