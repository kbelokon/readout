// Offline gate for committed runtime-vendored JavaScript. This intentionally
// performs no network access; use verify:vendor:upstream for npm provenance.

import { loadVendorManifest, verifyLocalVendorAssets } from './vendor-assets.mjs';

const manifest = loadVendorManifest();
const count = verifyLocalVendorAssets(manifest);
console.log(
  `verified ${count} local runtime-vendored JavaScript artifact(s) (offline; upstream not contacted)`,
);
