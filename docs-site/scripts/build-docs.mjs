// Generate docs-site/docs-generated/ from the repo's SDD-canonical design record at build time:
//   docs/getting-started/NN-slug.md -> /getting-started (the zero-to-one path for hosted users)
//   docs/guides/NN-slug.md         -> /guides      (user-facing usage guides)
//   docs/adrs/                     -> /decisions   (ADRs, MADR)
//   docs/openspec/specs/{cap}/     -> /specs       (OpenSpec spec.md + design.md pairs)
//   docs/design/NN-slug.md         -> /design      (design language, ADR-0016)
//   docs/reference/*.yaml          -> /reference   (machine-readable contracts)
//   docs/prfaq.md                  -> /prfaq
// The marketing landing page (route /) is NOT generated here — it is the standalone
// docs-site/src/pages/index.mdx (a page, so it renders with no docs sidebar). The per-endpoint HTTP
// API reference at /api is generated separately by `docusaurus gen-api-docs` (see package.json).
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
const START_SRC = join(REPO, 'docs', 'getting-started');
const GUIDES_SRC = join(REPO, 'docs', 'guides');
const ADR_SRC = join(REPO, 'docs', 'adrs');
const SPEC_SRC = join(REPO, 'docs', 'openspec', 'specs');
const DESIGN_SRC = join(REPO, 'docs', 'design');
const REF_SRC = join(REPO, 'docs', 'reference');
const OUT = join(SITE, 'docs-generated');
const STATIC_REF = join(SITE, 'static', 'reference');

// The source repository is private, so the published site links to nothing in it: a repo URL is a
// dead link (or a login wall) for every reader of the public docs.

// ---- discover sources ----
const startFiles = readdirSync(START_SRC).filter((f) => /^\d+-.*\.md$/.test(f)).sort();
const guideFiles = readdirSync(GUIDES_SRC).filter((f) => /^\d+-.*\.md$/.test(f)).sort();
const adrFiles = readdirSync(ADR_SRC).filter((f) => /^ADR-\d+.*\.md$/.test(f)).sort();
const capDirs = readdirSync(SPEC_SRC)
  .filter((d) => statSync(join(SPEC_SRC, d)).isDirectory())
  .sort();
const refFiles = readdirSync(REF_SRC).filter((f) => /\.ya?ml$/.test(f)).sort();
const designFiles = readdirSync(DESIGN_SRC).filter((f) => /^\d+-.*\.md$/.test(f)).sort();

// ---- clean + scaffold ----
rmSync(OUT, { recursive: true, force: true });
mkdirSync(join(OUT, 'getting-started'), { recursive: true });
mkdirSync(join(OUT, 'guides'), { recursive: true });
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
// docs/README.md (the design-index) has no page in the site; the Decisions index is its published
// equivalent.
function rewriteRepoLinks(s) {
  return s.replace(/\]\(\.\.\/README\.md([^)]*)\)/g, '](/decisions)');
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
    .replace(/\]\([^)]*?reference\/(openapi|asyncapi)\.ya?ml([^)]*)\)/g, '](/reference/$1$2)')
    // ...guides/08-handoff-lanes.md -> /guides/handoff-lanes (the emitted route drops the NN- prefix)
    .replace(/\]\([^)]*?guides\/\d+-([a-z0-9-]+)\.md([^)]*)\)/g, '](/guides/$1$2)');
}
// A guide linking a sibling guide by filename (`08-handoff-lanes.md`, `./08-handoff-lanes.md`).
function rewriteGuideSiblingLinks(s) {
  return s.replace(/\]\((?:\.\/)?\d+-([a-z0-9-]+)\.md([^)]*)\)/g, '](/guides/$1$2)');
}

// ---- getting started (docs/getting-started/NN-slug.md) -> getting-started/ ----
// The zero-to-one path for someone using the hosted service: concepts, first endpoint, connecting
// an agent, first webhook, and how the sibling projects fit. Same shape and rewrites as the guides;
// it sorts above them so a newcomer lands here first.
for (const f of startFiles) {
  const pos = parseInt(f.match(/^(\d+)-/)[1], 10);
  const slug = f.replace(/^\d+-/, '').replace(/\.md$/, '');
  const raw = readFileSync(join(START_SRC, f), 'utf8');
  const { fm, body } = splitFrontmatter(raw);
  const label = fmValue(fm, 'title') || slug;
  const content = rewriteDesignLinks(rewriteRepoLinks(body));
  writeFileSync(
    join(OUT, 'getting-started', `${slug}.md`),
    `---\nsidebar_position: ${pos}\nsidebar_label: ${label}\nformat: md\n---\n\n${content}`,
  );
}
writeFileSync(
  join(OUT, 'getting-started', '_category_.json'),
  JSON.stringify(
    {
      label: 'Getting started',
      position: 0,
      link: { type: 'generated-index', slug: '/getting-started', title: 'Getting started', description: 'From zero to a working agent on the hosted service: the concepts, your first endpoint, connecting an agent over MCP, and your first webhook.' },
    },
    null,
    2,
  ),
);

// ---- user guides (docs/guides/NN-slug.md) -> guides/ ----
// User-facing usage docs. Numeric filename prefix orders the sidebar and the emitted filename drops
// it, so routes are clean (/guides/overview, /guides/vend-an-endpoint, …). Guides are authored with
// site-absolute links already; the standard rewrites run anyway for safety. format: md (CommonMark)
// keeps machine-ish `<host>`/`<slug>` tokens (always inside code spans) harmless.
for (const f of guideFiles) {
  const pos = parseInt(f.match(/^(\d+)-/)[1], 10);
  const slug = f.replace(/^\d+-/, '').replace(/\.md$/, '');
  const raw = readFileSync(join(GUIDES_SRC, f), 'utf8');
  const { fm, body } = splitFrontmatter(raw);
  const label = fmValue(fm, 'title') || slug;
  const content = rewriteGuideSiblingLinks(rewriteDesignLinks(rewriteRepoLinks(body)));
  writeFileSync(
    join(OUT, 'guides', `${slug}.md`),
    `---\nsidebar_position: ${pos}\nsidebar_label: ${label}\nformat: md\n---\n\n${content}`,
  );
}
writeFileSync(
  join(OUT, 'guides', '_category_.json'),
  JSON.stringify(
    {
      label: 'Guides',
      position: 1,
      link: { type: 'generated-index', slug: '/guides', title: 'Guides', description: 'How to use switchboard: vend endpoints, drain the durable todo queue, route events with jq rules, work the queue well, and keep it safe.' },
    },
    null,
    2,
  ),
);

// ---- PRFAQ -> /prfaq ----
{
  const raw = readFileSync(join(REPO, 'docs', 'prfaq.md'), 'utf8');
  const content = sanitizeMdx(rewriteRepoLinks(raw));
  writeFileSync(
    join(OUT, 'prfaq.md'),
    `---\nslug: /prfaq\ntitle: PRFAQ\nsidebar_label: PRFAQ\nsidebar_position: 6\n---\n\n${content}`,
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

  // ADR-0018 decoupling: token links resolve to the docs site's FROZEN Operator token copy
  // (docs-site/static/design-tokens/), never the app's live static/tokens.css — app-token churn
  // must not silently restyle the published design record.
  let content = body.replace(
    /\]\(\.\.\/\.\.\/static\/tokens\.css\)/g,
    '](/design-tokens/tokens-operator.css)',
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
      link: { type: 'generated-index', slug: '/design', title: 'Design', description: 'The charm-web design language (ADR-0018): day/night tokens, components, screens, voice, and the directions explored.' },
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
      `[⬇ Download the raw \`${file}\`](pathname:///docs/reference/${file})\n\n` +
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

// ---- llms.txt -> static/llms.txt (served at <site>/docs/llms.txt) ----
// An agent pointed at this file (llmstxt.org format: H1, blockquote summary, H2 sections of
// `- [Title](url): note` lines) finds the pages that answer "how do I connect, and is it safe?"
// without guessing. Every URL is absolute and public; titles come from each page's front matter.
// static/llms.txt is gitignored — this script is its source.
// TODO: align with the shared approach stump.wtf/harness#475 picks for all three sites.
const SITE_URL = (process.env.DOCS_URL || 'https://switchboard.stump.wtf').replace(/\/+$/, '');
const DOCS_ROOT = `${SITE_URL}/docs`;
function pageTitle(srcPath) {
  const { fm, body } = splitFrontmatter(readFileSync(srcPath, 'utf8'));
  const h1 = body.match(/^#\s+(.+)$/m);
  return fmValue(fm, 'title') || (h1 ? h1[1].trim() : srcPath);
}
function llmsEntry(dir, file, route, note) {
  return `- [${pageTitle(join(dir, file))}](${DOCS_ROOT}${route}): ${note}`;
}
const llms = [
  '# Switchboard',
  '',
  '> Switchboard verifies inbound webhooks and turns them into a durable todo queue that AI agents',
  '> drain over MCP (Streamable HTTP). Agents claim a todo, do the work, and complete or fail it; a',
  '> push "doorbell" is only a hint, and the queue is the record.',
  '',
  'The hosted service is at https://switchboard.stump.wtf. Start with the connect page: it says',
  'exactly what to configure for Claude Code, Crush, or any other MCP client, and how to prove the',
  'connection works.',
  '',
  '## Start here',
  '',
  llmsEntry(START_SRC, '03-connect-an-agent.md', '/getting-started/connect-an-agent',
    'wire an agent to an endpoint (URL + bearer credential or OAuth), turn on push for Claude Code or Crush, or poll with claim_next'),
  '',
  '## Guides',
  '',
  llmsEntry(GUIDES_SRC, '12-security-model.md', '/guides/security-model',
    'what switchboard protects, what it cannot, and what is on the operator; read before giving an agent a credential'),
  llmsEntry(GUIDES_SRC, '14-self-hosting.md', '/guides/self-hosting',
    'run your own instance: one Go binary plus PostgreSQL'),
  llmsEntry(GUIDES_SRC, '06-operator-cli.md', '/guides/operator-cli',
    'log in, then vend, list, and revoke endpoints with the switchboard CLI or the /api/v1 operator API (the hosted web board does the same)'),
  llmsEntry(GUIDES_SRC, '07-routing-rules.md', '/guides/routing-rules',
    'per-webhook jq rules, first match wins, that pick which queue and endpoints a delivery lands on, or drop it'),
  llmsEntry(GUIDES_SRC, '10-routing-cookbook.md', '/guides/routing-cookbook',
    'tested routing-rule recipes'),
  '',
  '## Optional',
  '',
  '- [How Harness, Switchboard and Cairn fit together](https://stump-wtf.github.io/harness/guides/harness-switchboard-cairn/): the canonical page for the whole stack',
  '',
].join('\n');
// The docs are public; a private host in llms.txt is a dead link at best and a leak at worst.
// Fail the build rather than publish one. Public hosts: switchboard.stump.wtf, cairn.stump.wtf,
// stump-wtf.github.io, github.com. Everything under stump.rocks is private.
const privateHost = llms.match(/(?:[a-z0-9-]+\.)*stump\.rocks\b/i);
if (privateHost) {
  throw new Error(`build-docs: llms.txt would publish a private host (${privateHost[0]}); fix the entry or DOCS_URL`);
}
writeFileSync(join(SITE, 'static', 'llms.txt'), llms);

console.log(`build-docs: ${adrFiles.length} ADRs + ${capDirs.length} specs + ${refFiles.length} reference contracts -> docs-generated/`);
