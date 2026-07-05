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

const GITEA = 'https://gitea.stump.rocks/joestump/switchboard';
const GITEA_RAW = `${GITEA}/raw/branch/main`;

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

// ---- landing page ----
writeFileSync(join(OUT, 'intro.mdx'), `---
slug: /
title: Switchboard
sidebar_label: Overview
sidebar_position: 0
---

# Switchboard

*Many lines come in. The operator verifies each caller, and patches it through.*

**Switchboard** is the operator's board for your inbound webhooks. It receives events from external
providers (GitHub, Stripe, Slack, Docker Hub, and self-hosted/homelab senders), verifies and
normalizes each one, stores them in SQLite, and patches them through to two consumers of the same
backend — **MCP clients** (Claude Code and other agents) and a **human** at a small, local-only web
UI that updates live over Server-Sent Events. A fourth incoming line, a **Redis queue consumer**,
feeds the same pipeline with no HTTP endpoint at all.

The name is the architecture: a manual telephone exchange took many incoming lines, an operator
verified the caller, and patched the line through to its destination. That is exactly this — and it
is why these pages wear a switchboard-era palette of brass, bakelite, operator-cream, oxblood, and
patch-cable tones.

:::note Docs-first bootstrap
This site documents the **design** of switchboard — seven architecture decision records and three
API/stream/tool specs. Application code is written *fresh from these documents* in a follow-up
session; there is no runnable build yet.
:::

## Start here

- **[Decisions (ADRs)](/decisions)** — why switchboard is built the way it is: the stack, the SQLite
  persistence, the three-mode trust model, secrets via OpenBao, the MCP contract, and the repo/CI setup.
- **[Specifications](/specs)** — the HTTP surface (OpenAPI), the live SSE stream (AsyncAPI), and the
  exact MCP tool + resource contract.

## The trust model in one glance

Every event's trust story is explicit, per-provider, and shown — never assumed.

- <span className="sb-badge sb-badge--signed">signed</span> GitHub / Stripe / Slack — mandatory HMAC verification; a bad signature is a **401** and the payload is not stored.
- <span className="sb-badge sb-badge--unverified">unverified</span> Docker Hub + homelab — a generic endpoint with no signature scheme; opt-in, disabled by default, always labeled unverified.
- <span className="sb-badge sb-badge--redis">redis</span> the queue cord — no HTTP signature; trust is the Redis connection's ACL/TLS.

See **[ADR-003](/decisions/ADR-003-per-provider-ingestion-and-trust-model)** for the full model.
`);

// ---- ADRs -> decisions/ ----
const adrFiles = readdirSync(ADR_SRC).filter((f) => /^ADR-\d+.*\.md$/.test(f)).sort();
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
    `](${GITEA_RAW}/static/tokens.css)`,
  );

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

// ---- specs -> specs/ ----
// mcp-tools.md (has no frontmatter): copy with sidebar order + cross-link rewrites.
{
  const raw = readFileSync(join(SPEC_SRC, 'mcp-tools.md'), 'utf8');
  const content = sanitizeMdx(
    raw
      .replace(/\]\(\.\.\/adr\//g, '](../decisions/')
      .replace(/\]\(openapi\.yaml\)/g, '](./openapi)')
      .replace(/\]\(asyncapi\.yaml\)/g, '](./asyncapi)'),
  );
  writeFileSync(join(OUT, 'specs', 'mcp-tools.md'), `---\nsidebar_position: 3\ntitle: MCP tools & resources\n---\n\n${content}`);
}

// openapi.yaml / asyncapi.yaml: copy raw into static/ (downloadable) and wrap each in a page.
function specPage(slug, title, pos, file, blurb) {
  const yaml = readFileSync(join(SPEC_SRC, file), 'utf8');
  copyFileSync(join(SPEC_SRC, file), join(STATIC_SPECS, file));
  // 4-backtick fence so nothing inside can close it; code fences are literal under MDX.
  writeFileSync(
    join(OUT, 'specs', `${slug}.md`),
    `---\nsidebar_position: ${pos}\ntitle: ${title}\n---\n\n# ${title}\n\n${blurb}\n\n` +
      `[⬇ Download the raw \`${file}\`](pathname:///switchboard/specs/${file}) · ` +
      `[view on Gitea](${GITEA}/src/branch/main/docs/specs/${file})\n\n` +
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
      link: { type: 'generated-index', slug: '/specs', title: 'Specifications', description: 'The HTTP surface (OpenAPI), the live SSE stream (AsyncAPI), and the MCP tool/resource contract.' },
    },
    null,
    2,
  ),
);

console.log(`build-docs: ${adrFiles.length} ADRs + 3 specs -> docs-generated/`);
