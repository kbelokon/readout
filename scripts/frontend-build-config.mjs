export const frontendJavaScriptEntryRelativePath = 'internal/assets/src/js/readout.ts';

// Keep the production bundle recipe in one place so asset generation and the
// mutation runtime-graph proof cannot silently drift apart.
export function frontendJavaScriptBuildOptions(repoRoot, overrides = {}) {
  return {
    absWorkingDir: repoRoot,
    entryPoints: [frontendJavaScriptEntryRelativePath],
    bundle: true,
    format: 'iife',
    target: 'es2022',
    minify: false,
    sourcemap: false,
    legalComments: 'none',
    charset: 'utf8',
    write: false,
    ...overrides,
  };
}
