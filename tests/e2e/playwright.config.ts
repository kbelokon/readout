import { defineConfig, devices } from '@playwright/test';

// The suite runs against the standalone harness (./harness): a fakeapi fake
// apiserver plus the built readout binary launched with a generated
// KUBECONFIG. Specs drive deterministic fixture state via the control surface
// at `${controlURL}/__control/` (fail-lists, watch-script, watch-401, reset).
const readoutPort = process.env.READOUT_E2E_PORT ?? '8090';
const fakeapiPort = process.env.FAKEAPI_E2E_PORT ?? '8091';
const baseURL = `http://127.0.0.1:${readoutPort}`;
export const controlURL = `http://127.0.0.1:${fakeapiPort}`;

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
    {
      name: 'desktop',
      use: { ...devices['Desktop Chrome'], viewport: { width: 1440, height: 900 } },
    },
    // Mobile walk: below the 760px breakpoint, where the card layer plus
    // hamburger replace the table chrome (SPEC deviation D22 keeps it).
    {
      name: 'mobile',
      use: { ...devices['Desktop Chrome'], viewport: { width: 390, height: 844 }, hasTouch: true },
    },
  ],
  webServer: {
    command: 'go run ./harness',
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
