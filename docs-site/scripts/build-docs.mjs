// Generate docs-site/docs-generated/ from the repo's docs/adr and docs/specs at build time.
// The ADRs and specs are the single source of truth; this script only adapts them for Docusaurus
// (adds sidebar order, a status line, and rewrites a few cross-links). docs-generated/ is gitignored.
//
// Run: `npm run build-content` (invoked automatically by `npm start` / `npm run build`).

import {
  readFileSync, writeFileSync, mkdirSync, rmSync, readdirSync, copyFileSync,
} from 'node:fs';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const __dirname = dirname(fileURLToPath(import.meta.url));
const SITE = join(__dirname, '..');
const REPO = join(SITE, '..');
const ADR_SRC = join(REPO, 'docs', 'adr');
const SPEC_SRC = join(REPO, 'docs', 'specs');
const OUT = join(SITE, 'docs-generated');
const STATIC_SPECS = join(SITE, 'static', 'specs');

const GITHUB = 'https://github.com/joestump/switchboard';
const GITHUB_RAW = 'https://raw.githubusercontent.com/joestump/switchboard/main';

// ---- discover sources (also used for the intro counts) ----
const adrFiles = readdirSync(ADR_SRC).filter((f) => /^ADR-\d+.*\.md$/.test(f)).sort();
const yamlSpecs = ['openapi.yaml', 'asyncapi.yaml'];
const specMdFiles = readdirSync(SPEC_SRC).filter((f) => f.endsWith('.md')).sort();
const specCount = yamlSpecs.length + specMdFiles.length;

// ---- clean + scaffold ----
rmSync(OUT, { recursive: true, force: true });
mkdirSync(join(OUT, 'decisions'), { recursive: true });
mkdirSync(join(OUT, 'specs'), { recursive: true });
rmSync(STATIC_SPECS, { recursive: true, force: true });
mkdirSync(STATIC_SPECS, { recursive: true });

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
// (Fenced code blocks are left alone — MDX treats fence contents as literal.)
function sanitizeMdx(s) {
  return s.replace(/<((?:https?):\/\/[^>\s]+)>/g, '[$1]($1)');
}
// docs/README.md (the ADR/spec index + open-questions) has no page in the site; point at GitHub.
function rewriteRepoLinks(s) {
  return s.replace(/\]\(\.\.\/README\.md([^)]*)\)/g, `](${GITHUB}/blob/main/docs/README.md$1)`);
}

// ---- landing page ----
writeFileSync(join(OUT, 'intro.mdx'), `---
slug: /
title: Switchboard
sidebar_label: Overview
sidebar_position: 0
hide_table_of_contents: true
---

import Link from '@docusaurus/Link';

<div className="sb-hero">
  <div className="sb-hero__eyebrow">MCP server · local web UI · Redis queue</div>
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

  <Link className="sb-tile" to="/decisions/ADR-014-ingestion-adapters-push-pull">
    <svg className="sb-tile__icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round"><path d="M3 13v5a2 2 0 0 0 2 2h14a2 2 0 0 0 2-2v-5"/><path d="M8 9l4 4 4-4"/><path d="M12 2v11"/></svg>
    <div className="sb-tile__title">Push &amp; pull ingestion adapters</div>
    <p className="sb-tile__body">Push webhooks (GitHub, Stripe, Slack, Docker Hub, generic) and pull queue adapters (Redis, with SQS/NATS/AMQP to follow) normalize into the same todo — each with an enforced trust mode. Pull adapters ack the source only after the todo is durably stored, so nothing is lost at the boundary.</p>
  </Link>

  <Link className="sb-tile" to="/decisions/ADR-007-todos-as-core-primitive">
    <svg className="sb-tile__icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round"><path d="M9 6h11"/><path d="M9 12h11"/><path d="M9 18h11"/><path d="M4 5.5l1 1 2-2"/><path d="M4 11.5l1 1 2-2"/><path d="M4 17.5l1 1 2-2"/></svg>
    <div className="sb-tile__title">Durable todo work‑queue</div>
    <p className="sb-tile__body">Every event becomes a work‑item with a lifecycle — claimed under a lease, completed with an ack, deduped by idempotency key. A crashed worker's todo re‑surfaces; nothing is read‑once and lost.</p>
  </Link>

  <Link className="sb-tile" to="/decisions/ADR-008-human-principal-vended-endpoints">
    <svg className="sb-tile__icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round"><circle cx="8" cy="8" r="4"/><path d="M10.8 10.8L20 20"/><path d="M17 17l2-2"/><path d="M14.5 14.5l2-2"/></svg>
    <div className="sb-tile__title">Per‑agent vended MCP endpoints</div>
    <p className="sb-tile__body">Humans are the accountable principals; each agent is vended a scoped MCP endpoint (queues + verb allowlist). The credential is stored hashed in Postgres, short‑lived and revocable — revoke = kill the endpoint.</p>
  </Link>

  <Link className="sb-tile" to="/decisions/ADR-009-personas-as-scoped-agent-cards">
    <svg className="sb-tile__icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round"><rect x="3" y="7" width="13" height="10" rx="1.5"/><path d="M7.5 4.5H19a1.5 1.5 0 0 1 1.5 1.5v9"/></svg>
    <div className="sb-tile__title">Personas as A2A Agent Cards</div>
    <p className="sb-tile__body">One agent, many least‑privilege faces. A persona is a human‑authored prompt plus a verb subset; its advertised skills are derived from what's actually vended, published as an A2A Agent Card.</p>
  </Link>

  <Link className="sb-tile" to="/decisions/ADR-010-a2a-discovery-human-vended-friending">
    <svg className="sb-tile__icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round"><circle cx="6" cy="12" r="2.5"/><circle cx="18" cy="12" r="2.5"/><path d="M8.5 12h3"/><path d="M12.5 10.7l1.3 1.3 2.2-2.2"/></svg>
    <div className="sb-tile__title">Human‑approved friending</div>
    <p className="sb-tile__body">Agents discover peers over A2A and send a scoped friend request. Approval lands as a todo in the target human's queue — and approving is the vend. Per‑direction, revocable, non‑transitive.</p>
  </Link>

  <Link className="sb-tile" to="/decisions/ADR-013-channels-push-delivery">
    <svg className="sb-tile__icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round"><path d="M6 9a6 6 0 0 1 12 0c0 5 2 6 2 6H4s2-1 2-6"/><path d="M10 20.5a2 2 0 0 0 4 0"/></svg>
    <div className="sb-tile__title">Push into your live session</div>
    <p className="sb-tile__body">When a harness is attached, switchboard pushes new todos straight into the session over the open Claude Code Channels standard — a doorbell, not the ledger. Offline? The durable queue keeps the work until it's pulled.</p>
  </Link>

  <Link className="sb-tile" to="/decisions/ADR-001-web-stack-go-htmx-pico">
    <svg className="sb-tile__icon" viewBox="0 0 24 24" fill="none" stroke="currentColor" strokeWidth="1.6" strokeLinecap="round" strokeLinejoin="round"><rect x="3" y="4" width="18" height="16" rx="1.5"/><path d="M3 8h18"/><circle cx="5.8" cy="6" r="0.5"/><circle cx="7.8" cy="6" r="0.5"/></svg>
    <div className="sb-tile__title">Local live web UI</div>
    <p className="sb-tile__body">A small, local‑only operator board — four screens on Go (net/http) + HTMX + Pico.css, updating live over Server‑Sent Events, with the same trust badges the API and MCP surfaces carry.</p>
  </Link>

</div>

<div className="sb-trust">
  <span>Two families — webhook &amp; queue — each event's trust shown:</span>
  <span className="sb-badge sb-badge--signed">signed</span>
  <span className="sb-badge sb-badge--token">token</span>
  <span className="sb-badge sb-badge--open">open</span>
  <span className="sb-badge sb-badge--queue">queue</span>
  <span>— see <Link to="/decisions/ADR-003-per-provider-ingestion-and-trust-model">ADR‑003</Link>.</span>
</div>

:::note Docs-first bootstrap
This site is the **canonical design record** for switchboard — ${adrFiles.length} architecture decision
records and ${specCount} API/stream/tool specs. Application code is written *fresh from these documents*
in a follow‑up session; there is no runnable build yet. The name is the architecture: a manual
telephone exchange took many incoming lines, an operator verified the caller, and patched the line
through — which is why these pages wear a switchboard‑era palette of brass, bakelite, operator‑cream,
oxblood, and patch‑cable tones.
:::

## Start here

- **[Decisions (ADRs)](/decisions)** — why switchboard is built the way it is: the stack, the PostgreSQL
  persistence, the trust model, the MCP contract,
  and the todo/agent‑vending/A2A layer.
- **[Specifications](/specs)** — the HTTP surface (OpenAPI), the live SSE stream (AsyncAPI), the MCP
  tool + resource contracts, the todo object/state‑machine, webhook ingestion, personas/Agent Cards,
  friending, and the account/vended‑endpoint model.
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

  // Rewrite the one app-asset link (tokens.css lives in the app, not the docs).
  let content = body.replace(
    /\]\(\.\.\/\.\.\/static\/tokens\.css\)/g,
    `](${GITHUB_RAW}/static/tokens.css)`,
  );
  content = rewriteRepoLinks(content);

  // Inject a status line right after the H1 (plain markdown blockquote — safe under MDX).
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

// ---- markdown specs -> specs/ ----
// Every *.md in docs/specs becomes a page. Known specs get an explicit order + title;
// any new one falls back to alphabetical order after the known set and an H1-derived title.
const SPEC_META = {
  'mcp-tools.md':                { pos: 3, title: 'MCP tools & resources' },
  'todos.md':                    { pos: 4, title: 'Todos — object & state machine' },
  'agent-mcp-tools.md':          { pos: 5, title: 'Agent MCP tool surface' },
  'ingestion-adapters.md':       { pos: 6, title: 'Ingestion adapters (push & pull)' },
  'personas-and-agent-cards.md': { pos: 7, title: 'Personas & Agent Cards' },
  'friend-requests.md':          { pos: 8, title: 'Friend-request & approval flow' },
  'accounts-and-endpoints.md':   { pos: 9, title: 'Accounts & vended endpoints' },
};
function deriveTitle(raw, file) {
  const h1 = raw.match(/^#\s+(.+)$/m);
  return h1 ? h1[1].replace(/^switchboard\s+[—-]\s+/i, '').trim() : file.replace(/\.md$/, '');
}
specMdFiles.forEach((file, i) => {
  const raw = readFileSync(join(SPEC_SRC, file), 'utf8');
  const meta = SPEC_META[file] || { pos: 100 + i, title: deriveTitle(raw, file) };
  const content = sanitizeMdx(
    rewriteRepoLinks(raw)
      .replace(/\]\(\.\.\/adr\//g, '](../decisions/')
      .replace(/\]\(openapi\.yaml\)/g, '](./openapi)')
      .replace(/\]\(asyncapi\.yaml\)/g, '](./asyncapi)'),
  );
  writeFileSync(
    join(OUT, 'specs', file),
    `---\nsidebar_position: ${meta.pos}\ntitle: ${meta.title}\n---\n\n${content}`,
  );
});

// openapi.yaml / asyncapi.yaml: copy raw into static/ (downloadable) and wrap each in a page.
function specPage(slug, title, pos, file, blurb) {
  const yaml = readFileSync(join(SPEC_SRC, file), 'utf8');
  copyFileSync(join(SPEC_SRC, file), join(STATIC_SPECS, file));
  // 4-backtick fence so nothing inside can close it; code fences are literal under MDX.
  writeFileSync(
    join(OUT, 'specs', `${slug}.md`),
    `---\nsidebar_position: ${pos}\ntitle: ${title}\n---\n\n# ${title}\n\n${blurb}\n\n` +
      `[⬇ Download the raw \`${file}\`](pathname:///switchboard/specs/${file}) · ` +
      `[view on GitHub](${GITHUB}/blob/main/docs/specs/${file})\n\n` +
      `\`\`\`\`yaml\n${yaml}\n\`\`\`\`\n`,
  );
}
specPage(
  'openapi', 'OpenAPI — HTTP surface', 1, 'openapi.yaml',
  'The webhook ingestion endpoints and the local web-UI routes. Hand-authored (the endpoints are webhook receivers and server-rendered HTML, not model-driven). Validated against OpenAPI 3.1.',
);
specPage(
  'asyncapi', 'AsyncAPI — SSE stream', 2, 'asyncapi.yaml',
  'The Server-Sent Events schema for the live web UI: the `event-received` and `status` messages the dashboard and log screens subscribe to.',
);

writeFileSync(
  join(OUT, 'specs', '_category_.json'),
  JSON.stringify(
    {
      label: 'Specifications',
      position: 3,
      link: { type: 'generated-index', slug: '/specs', title: 'Specifications', description: 'The HTTP surface (OpenAPI), the live SSE stream (AsyncAPI), the MCP tool/resource contracts, and the todo/agent/A2A specs.' },
    },
    null,
    2,
  ),
);

console.log(`build-docs: ${adrFiles.length} ADRs + ${specCount} specs -> docs-generated/`);
