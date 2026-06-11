// build-assets.mjs -- the frontend asset pipeline (strangler skeleton).
//
// esbuild bundles internal/assets/src/js/readout.ts -> static/readout.js as a
// single classic IIFE (no module loader, no SPA, no CDN). Lightning CSS bundles
// internal/assets/src/css/readout.css -> static/readout.css, inlining @imports
// and emitting cascade @layer blocks in their declared order.
//
// Both tools are driven through their JS API (NOT the CLIs) so the OUTPUT can be
// validated IN-PROCESS, before it touches disk: if an invariant fails we throw
// and write nothing, so a broken build can never half-overwrite a committed
// artifact. The Go binary embeds the static/ files and builds with NO Node, so
// this script is dev/CI-only -- it regenerates committed artifacts, it is not a
// runtime dependency.
//
// Deliberately NO minify and NO source maps (debuggable shipped assets), NO
// Lightning CSS `targets` (targets would downlevel oklch()->hex and unwrap the
// native CSS nesting we want to keep), and the output paths/names are fixed
// (static/readout.{js,css}) because go:embed + the ?v=<hash> cache-buster key
// off them.

import { readFileSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import * as esbuild from 'esbuild';
import { bundle as lightningBundle } from 'lightningcss';

const repoRoot = join(dirname(fileURLToPath(import.meta.url)), '..');
const SRC_JS = join(repoRoot, 'internal/assets/src/js/readout.ts');
const SRC_CSS = join(repoRoot, 'internal/assets/src/css/readout.css');
const OUT_JS = join(repoRoot, 'internal/assets/static/readout.js');
const OUT_CSS = join(repoRoot, 'internal/assets/static/readout.css');

function fail(msg) {
  throw new Error(`build-assets invariant failed: ${msg}`);
}

// --- JS: esbuild -> classic IIFE ------------------------------------------
async function buildJS() {
  const result = await esbuild.build({
    entryPoints: [SRC_JS],
    bundle: true,
    format: 'iife',
    target: 'es2022',
    minify: false,
    sourcemap: false,
    legalComments: 'none',
    charset: 'utf8',
    write: false,
  });
  const out = result.outputFiles[0].text;

  // (strict-mode) the legacy bundle assumes 'use strict'; the IIFE wrapper must
  // preserve it so the strict-only global-assignment semantics still hold.
  if (!/^"use strict";/.test(out)) {
    fail('JS output does not start with "use strict";');
  }
  // (IIFE) it must be a single self-invoking scope, not bare module statements.
  if (!out.includes('(() => {')) {
    fail('JS output is not wrapped in an IIFE');
  }
  // (no module leak) no import/export survived the bundle.
  if (/^\s*(import|export)\s/m.test(out)) {
    fail('JS output still contains import/export statements');
  }
  return out;
}

// --- CSS: Lightning CSS -> bundled, layered --------------------------------
function buildCSS() {
  const { code } = lightningBundle({
    filename: SRC_CSS,
    minify: false,
    sourceMap: false,
  });
  const out = code.toString('utf8');

  // (1) cascade layer order: theme MUST be emitted before app. Lightning CSS
  // absorbs the `@layer theme, app;` pre-declaration and re-emits the blocks in
  // the declared cascade order, so we assert BLOCK ORDER (the `@layer theme {`
  // and `@layer app {` openers), NOT the literal first statement.
  const themeAt = out.indexOf('@layer theme {');
  const appAt = out.indexOf('@layer app {');
  if (themeAt === -1) fail('CSS output has no `@layer theme {` block');
  if (appAt === -1) fail('CSS output has no `@layer app {` block');
  if (!(themeAt < appAt)) {
    fail(`CSS layer order wrong: @layer app ({${appAt}}) before @layer theme ({${themeAt}})`);
  }

  // (2) no @import survived bundling (everything is inlined).
  if (out.includes('@import')) {
    fail('CSS output still contains an @import');
  }

  // (3) no remote url(http...) crept in -- the CSP is `default-src 'self'`, so
  // every asset reference must stay same-origin/relative.
  if (/url\(\s*['"]?https?:/i.test(out)) {
    fail('CSS output contains a remote url(http...)');
  }
  return out;
}

const [js, css] = await Promise.all([buildJS(), Promise.resolve(buildCSS())]);

// (4) byte-for-byte determinism: a rebuild must reproduce the committed bytes.
// Compare against what is already on disk and only rewrite on an actual change,
// so `node build && node build && git diff` is empty on the second run.
function writeIfChanged(path, next) {
  let prev = null;
  try {
    prev = readFileSync(path, 'utf8');
  } catch {
    prev = null;
  }
  if (prev === next) {
    console.log(`unchanged: ${path}`);
    return;
  }
  writeFileSync(path, next, 'utf8');
  console.log(`wrote: ${path} (${Buffer.byteLength(next, 'utf8')} bytes)`);
}

writeIfChanged(OUT_JS, js);
writeIfChanged(OUT_CSS, css);
