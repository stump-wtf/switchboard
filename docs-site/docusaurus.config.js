// @ts-check
// Docusaurus config for the switchboard docs site.
// Renders the ADRs (docs/adrs), OpenSpec specs (docs/openspec/specs), and reference contracts
// (docs/reference) — transformed into docs-generated/ by scripts/build-docs.mjs at build time —
// with mermaid diagrams and the switchboard-era theme.
//
// Served as compiled static files by the front Caddy at https://switchboard.stump.wtf/docs/
// (baseUrl /docs/). Repo of record: gitea.stump.rocks/stump.wtf/switchboard.

const { themes } = require('prism-react-renderer');

const SITE_URL = process.env.DOCS_URL || 'https://switchboard.stump.wtf';
const BASE_URL = '/docs/';
const GITHUB_URL = 'https://gitea.stump.rocks/stump.wtf/switchboard';

/** @type {import('@docusaurus/types').Config} */
const config = {
  title: 'Switchboard',
  tagline: 'The operator’s board for inbound webhooks — receive, verify, patch through.',
  favicon: 'img/favicon.svg',

  future: { v4: true },

  url: SITE_URL,
  baseUrl: BASE_URL,
  organizationName: 'stump-wtf',
  projectName: 'switchboard',

  onBrokenLinks: 'warn',
  onBrokenMarkdownLinks: 'warn',

  // MDX everywhere (default) so mermaid ```mermaid blocks in the ADRs render as diagrams. The
  // build script sanitizes the one MDX-hostile construct in the source (autolinks) — see build-docs.mjs.
  markdown: { mermaid: true },
  themes: ['@docusaurus/theme-mermaid'],

  i18n: { defaultLocale: 'en', locales: ['en'] },

  presets: [
    [
      'classic',
      /** @type {import('@docusaurus/preset-classic').Options} */
      ({
        docs: {
          path: 'docs-generated',
          routeBasePath: '/',
          sidebarPath: require.resolve('./sidebars.js'),
        },
        blog: false,
        theme: {
          customCss: require.resolve('./src/css/custom.css'),
        },
      }),
    ],
  ],

  themeConfig:
    /** @type {import('@docusaurus/preset-classic').ThemeConfig} */
    ({
      colorMode: { defaultMode: 'dark', respectPrefersColorScheme: true },
      navbar: {
        title: 'Switchboard',
        logo: { alt: 'Switchboard jack', src: 'img/logo.svg' },
        items: [
          { type: 'docSidebar', sidebarId: 'docs', position: 'left', label: 'Documentation' },
          { to: '/prfaq', label: 'PRFAQ', position: 'left' },
          { to: '/decisions', label: 'Decisions', position: 'left' },
          { to: '/specs', label: 'Specs', position: 'left' },
          { to: '/design', label: 'Design', position: 'left' },
          { to: '/reference', label: 'Reference', position: 'left' },
          { href: GITHUB_URL, label: 'GitHub', position: 'right' },
        ],
      },
      footer: {
        style: 'dark',
        links: [
          {
            title: 'Docs',
            items: [
              { label: 'PRFAQ', to: '/prfaq' },
              { label: 'Decisions (ADRs)', to: '/decisions' },
              { label: 'Specifications', to: '/specs' },
              { label: 'Design', to: '/design' },
              { label: 'Reference', to: '/reference' },
            ],
          },
          {
            title: 'Source',
            items: [
              { label: 'GitHub', href: GITHUB_URL },
            ],
          },
        ],
        copyright: `Switchboard — © 2026 Joe Stump. MIT. “Many lines come in; the operator patches each through.”`,
      },
      prism: {
        theme: themes.oneLight,
        darkTheme: themes.oneDark,
        additionalLanguages: ['bash', 'json', 'yaml', 'python', 'toml'],
      },
      mermaid: { theme: { light: 'neutral', dark: 'dark' } },
    }),
};

module.exports = config;
