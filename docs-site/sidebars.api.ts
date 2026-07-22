// Sidebar for the /api docs instance. `docusaurus gen-api-docs` writes the tag-grouped item array to
// ./api-docs/sidebar.ts. That file is absent on a clean checkout (and momentarily during generation,
// which itself loads this config), so the require is guarded: an empty sidebar until generation runs.
// scripts/api-docs.mjs "pre" clears api-docs/ entirely so gen-api-docs always writes a fresh sidebar
// (it will not overwrite an existing sidebar file).
import type { SidebarsConfig } from '@docusaurus/plugin-content-docs';

let apiItems: SidebarsConfig[string] = [];
try {
  // eslint-disable-next-line @typescript-eslint/no-var-requires, global-require, import/no-unresolved
  const generated = require('./api-docs/sidebar');
  apiItems = (generated.default ?? generated) as SidebarsConfig[string];
} catch {
  apiItems = [];
}

const sidebars: SidebarsConfig = { apiSidebar: apiItems };

export default sidebars;
