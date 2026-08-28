// Explicit networked provenance check. Unlike the normal offline asset build,
// this downloads every pinned npm tarball, verifies its SRI, parses it in memory,
// and compares the declared artifact + license bytes with the committed files.

import {
  loadVendorManifest,
  verifyLocalVendorAssets,
  verifyUpstreamVendorAssets,
} from './vendor-assets.mjs';

const manifest = loadVendorManifest();
verifyLocalVendorAssets(manifest);
const count = await verifyUpstreamVendorAssets(manifest);
console.log(
  `verified ${count} runtime-vendored JavaScript artifact(s) against pinned npm tarballs`,
);
