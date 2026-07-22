// Pre/post steps around `docusaurus gen-api-docs` (see package.json "gen-api").
//
//   node scripts/api-docs.mjs pre    # before gen-api-docs
//   node scripts/api-docs.mjs post   # after gen-api-docs
//
// The generated API reference under docs-site/api-docs/ is gitignored and rebuilt every run.
//
// pre  — wipes ./api-docs to a clean, empty folder (which must exist for the /api docs instance to
//        initialize when the CLI loads the Docusaurus config). gen-api-docs will NOT overwrite an
//        existing sidebar.ts, so we must NOT leave a stub behind — a clean folder guarantees it
//        writes a fresh, fully-populated sidebar. sidebars.api.ts tolerates the momentarily-absent
//        sidebar.ts via a guarded require.
//
// post — makes the generated API "info" page the /api index by giving it `slug: /`. gen-api-docs
//        emits it at id `switchboard-http-api` (route /api/switchboard-http-api) with no root doc, so
//        without this /api itself would 404. Re-run every build, so it survives regeneration.

import {
  existsSync, mkdirSync, rmSync, writeFileSync, readFileSync,
} from 'node:fs';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';

const __dirname = dirname(fileURLToPath(import.meta.url));
const API_DIR = join(__dirname, '..', 'api-docs');
const INFO = join(API_DIR, 'switchboard-http-api.info.mdx');

function pre() {
  // Clean slate: an empty (but existing) folder. gen-api-docs writes a fresh sidebar.ts only when
  // none is present, so we must not leave any file behind.
  rmSync(API_DIR, { recursive: true, force: true });
  mkdirSync(API_DIR, { recursive: true });
}

function post() {
  if (!existsSync(INFO)) {
    throw new Error(`api-docs post: expected generated info page at ${INFO}`);
  }
  let src = readFileSync(INFO, 'utf8');
  if (!/^---\n[\s\S]*?\n---/.test(src)) {
    throw new Error('api-docs post: info page has no frontmatter block');
  }
  if (/^slug:\s*/m.test(src)) return; // already patched
  // Insert `slug: /` as the first frontmatter key so the info page becomes the /api index.
  src = src.replace(/^---\n/, '---\nslug: /\n');
  writeFileSync(INFO, src);
}

const cmd = process.argv[2];
if (cmd === 'pre') pre();
else if (cmd === 'post') post();
else {
  console.error('usage: node scripts/api-docs.mjs <pre|post>');
  process.exit(1);
}
