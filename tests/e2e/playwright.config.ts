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
// e2e-visual-update targets set it). A bare `npx playwright test` therefore
// runs the behavioural projects only and never the screenshot walk. testIgnore
// on the behavioural projects is the second guard — even with RO_VISUAL=1 they
// never pick up visual.spec.ts.
//
// Contract: the grid is a HOST tool on a SINGLE developer machine. The committed
// baselines are that machine's own Chromium render, so a same-machine strict
// compare is honest. The PNGs are NOT portable — Chromium glyph rasterization
// differs across machines (and under emulation) — so CI does NOT run the visual
// grid. Regenerate the baselines with `make e2e-visual-update` whenever the dev
// mac or its macOS version changes.
const visualEnabled = process.env.RO_VISUAL === '1';

// Per-comparison maxDiffPixels, read from RO_VISUAL_MAXDIFF — DEFAULT 0 (strict,
// zero tolerance). There is NO unconditional tolerance baked into the spec.
//
// The knob exists as an escape hatch: if a measured same-machine noise floor
// ever demands a small budget, set RO_VISUAL_MAXDIFF (and the dense fallback
// RO_VISUAL_MAXDIFF_DENSE for the text-heavy frames) rather than editing the
// baselines. The default stays 0 so the grid compares pixel-exact unless a knob
// is set explicitly.
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
  // Screenshot baselines for the visual-regression walk live next to the specs,
  // keyed by project (viewport) name.
  snapshotPathTemplate: '{testDir}/__screenshots__/{projectName}/{testFileName}/{arg}{ext}',
  use: {
    baseURL,
    screenshot: 'only-on-failure',
    trace: 'retain-on-failure',
  },
  projects: [
    // Desktop walk: the full chrome (sidebar + topbar + table layer).
    // testIgnore keeps the visual baselines OFF this project: the behavioural
    // `make e2e` must never run (or require) the screenshot spec, which lives
    // in its own RO_VISUAL-gated `visual` project.
    {
      name: 'desktop',
      testIgnore: /visual\.spec\.ts/,
      use: { ...devices['Desktop Chrome'], viewport: { width: 1440, height: 900 } },
    },
    // Mobile walk: below the 760px breakpoint, where the card layer plus
    // hamburger replace the table chrome (the mobile card layer is kept).
    {
      name: 'mobile',
      testIgnore: /visual\.spec\.ts/,
      use: { ...devices['Desktop Chrome'], viewport: { width: 390, height: 844 }, hasTouch: true },
    },
    // Visual baselines: the screenshot walk, host-only,
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
