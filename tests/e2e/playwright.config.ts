import { defineConfig, devices } from '@playwright/test';

// The suite runs against the standalone harness (./harness): a fakeapi fake
// apiserver plus the built readout binary launched with a generated
// KUBECONFIG. Specs drive deterministic fixture state via the control surface
// at `${controlURL}/__control/` (fail-lists, watch-script, watch-401, reset).
const readoutPort = process.env.READOUT_E2E_PORT ?? '8090';
const fakeapiPort = process.env.FAKEAPI_E2E_PORT ?? '8091';
const baseURL = `http://127.0.0.1:${readoutPort}`;
export const controlURL = `http://127.0.0.1:${fakeapiPort}`;

// The visual baselines run ONLY when RO_VISUAL=1 (the Makefile e2e-visual /
// e2e-visual-update targets set it inside the container). A bare
// `npx playwright test` on the host therefore runs the behavioural projects
// only and never the screenshot walk: the arm64-macOS host renderer does not
// match the linux/amd64 container, so host-side baseline compares are
// meaningless. testIgnore on the behavioural projects is the second guard —
// even with RO_VISUAL=1 they never pick up visual.spec.ts.
const visualEnabled = process.env.RO_VISUAL === '1';

// Per-comparison maxDiffPixels, read from RO_VISUAL_MAXDIFF — DEFAULT 0 (strict,
// zero tolerance). There is NO unconditional tolerance baked into the spec.
//
// Why the env knob exists: the CANONICAL baselines are generated and verified
// STRICTLY (no env => 0) by the visual CI job on NATIVE linux/amd64, where
// Chromium glyph rasterization is deterministic. Locally the only runner is the
// arm64-macOS Docker daemon emulating linux/amd64 under Rosetta, whose glyph
// rasterization is nondeterministic ACROSS browser-process launches: glyphs
// re-rasterize with sub-pixel position shifts along their edges (not any single
// dynamic element).
//
// That noise is BIMODAL under Playwright's comparator: ~100 differing pixels or
// fewer on 32 of the 34 frames (the count fluctuates run to run), but ~6326 on
// the two busiest text-dense frames (the nodes table, light + dark). A single
// budget wide enough for nodes (10000) would needlessly slacken the 32 clean
// frames and is already the scale of a small real regression. So there are TWO
// budgets: the default RO_VISUAL_MAXDIFF covers the clean frames, and
// RO_VISUAL_MAXDIFF_DENSE (the spec applies it to the two nodes frames; falls
// back to RO_VISUAL_MAXDIFF) covers the dense outliers. `make e2e-visual` sets
// RO_VISUAL_MAXDIFF=300 (observed clean peak ~101 px, with margin) and
// RO_VISUAL_MAXDIFF_DENSE=10000 (~6326 x1.6). CI runs WITHOUT either env =>
// both default to strict zero.
const visualMaxDiff = Number(process.env.RO_VISUAL_MAXDIFF ?? 0);

const visualProject = {
  name: 'visual',
  testMatch: /visual\.spec\.ts/,
  expect: {
    toHaveScreenshot: { maxDiffPixels: visualMaxDiff },
  },
  use: {
    ...devices['Desktop Chrome'],
    viewport: { width: 1440, height: 900 },
    // reducedMotion lives HERE and NOT in the global use{}: it freezes the
    // cell-flash and the live-dot pulse for stable captures, but those
    // animations are asserted by the behavioural specs, which must keep
    // running at full motion.
    reducedMotion: 'reduce' as const,
  },
};

export default defineConfig({
  testDir: '.',
  outputDir: './test-results',
  // One shared harness carries mutable fixture state: specs run serially so
  // control-surface toggles cannot bleed between tests.
  fullyParallel: false,
  workers: 1,
  forbidOnly: !!process.env.CI,
  retries: 0,
  reporter: [['list']],
  timeout: 30_000,
  // Screenshot baselines for the SPEC §9 visual walk live next to the specs,
  // keyed by project (viewport) name.
  snapshotPathTemplate: '{testDir}/__screenshots__/{projectName}/{testFileName}/{arg}{ext}',
  use: {
    baseURL,
    screenshot: 'only-on-failure',
    trace: 'retain-on-failure',
  },
  projects: [
    // Desktop walk: the full chrome (sidebar + topbar + table layer).
    // testIgnore keeps the visual baselines OFF this project: the host
    // renderer does not match the linux/amd64 container, so a host `make e2e`
    // must never run (or require) the screenshot spec.
    {
      name: 'desktop',
      testIgnore: /visual\.spec\.ts/,
      use: { ...devices['Desktop Chrome'], viewport: { width: 1440, height: 900 } },
    },
    // Mobile walk: below the 760px breakpoint, where the card layer plus
    // hamburger replace the table chrome (SPEC deviation D22 keeps it).
    {
      name: 'mobile',
      testIgnore: /visual\.spec\.ts/,
      use: { ...devices['Desktop Chrome'], viewport: { width: 390, height: 844 }, hasTouch: true },
    },
    // Visual baselines (SPEC §9 / Unit 2): the screenshot walk, container-only,
    // included only under RO_VISUAL=1. testMatch pins it to visual.spec.ts
    // alone, so it never runs the behavioural specs.
    ...(visualEnabled ? [visualProject] : []),
  ],
  webServer: {
    // Host flow runs the harness via `go run`; the containerized flow has no Go
    // toolchain, so HARNESS_BIN points at a prebuilt linux/amd64 binary.
    command: process.env.HARNESS_BIN ?? 'go run ./harness',
    url: `${baseURL}/clusters`,
    // Never reuse a stray server: stale fixture state would make runs
    // nondeterministic, which is the whole point of the harness.
    reuseExistingServer: false,
    timeout: 60_000,
    stdout: 'pipe',
    stderr: 'pipe',
    env: {
      ...process.env,
      READOUT_E2E_PORT: readoutPort,
      FAKEAPI_E2E_PORT: fakeapiPort,
    },
  },
});
