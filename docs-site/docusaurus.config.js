// @ts-check
// Docusaurus config for the switchboard docs site.
// Renders the ADRs (docs/adr) and specs (docs/specs) — transformed into docs-generated/ by
// scripts/build-docs.mjs at build time — with mermaid diagrams and the switchboard-era theme.
//
// Published to GitHub Pages at https://joestump.github.io/switchboard/ (baseUrl /switchboard/).

const { themes } = require('prism-react-renderer');

const SITE_URL = process.env.DOCS_URL || 'https://joestump.github.io';
const BASE_URL = '/switchboard/';
const GITHUB_URL = 'https://github.com/joestump/switchboard';

/** @type {import('@docusaurus/types').Config} */
const config = {
  title: 'Switchboard',
  tagline: 'The operator’s board for inbound webhooks — receive, verify, patch through.',
  favicon: 'img/favicon.svg',

  future: { v4: true },

  url: SITE_URL,
  baseUrl: BASE_URL,
  organizationName: 'joestump',
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
          { to: '/decisions', label: 'Decisions', position: 'left' },
          { to: '/specs', label: 'Specs', position: 'left' },
          { href: GITHUB_URL, label: 'GitHub', position: 'right' },
        ],
      },
      footer: {
        style: 'dark',
        links: [
          {
            title: 'Docs',
            items: [
              { label: 'Decisions (ADRs)', to: '/decisions' },
              { label: 'Specifications', to: '/specs' },
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
