import { test, expect } from '@playwright/test';
import { spawn, type ChildProcess } from 'node:child_process';
import fs from 'node:fs';
import os from 'node:os';
import path from 'node:path';

// First-run screen against the REAL binary: readout launched with a
// KUBECONFIG that resolves to ZERO contexts must SURVIVE startup (the
// reachability prerequisite -- it used to exit before binding the listener)
// and render the instruction screen: the literal headline, the command block
// with the binary's REAL config surface (KUBECONFIG + --config; the
// prototype's nonexistent --kubeconfig flag must not ship), Setup docs, and a
// Re-check that is a plain read-only GET reload. There is NO login UI
// anywhere.
//
// The shared Playwright webServer harness boots readout against a populated
// fakeapi kubeconfig, so this spec spawns its OWN readout process on a
// dedicated port with a zero-context kubeconfig written to a temp dir.

const FIRSTRUN_PORT = 8094;
const FIRSTRUN_URL = `http://127.0.0.1:${FIRSTRUN_PORT}`;

let readout: ChildProcess | undefined;
let tmpDir: string | undefined;

function readoutBinary(): string {
  return process.env.READOUT_BIN ?? path.resolve(__dirname, '..', '..', 'readout');
}

async function waitForReady(url: string, timeoutMs: number): Promise<void> {
  const deadline = Date.now() + timeoutMs;
  let lastError: unknown;
  while (Date.now() < deadline) {
    try {
      const res = await fetch(url);
      if (res.ok) {
        return;
      }
      lastError = new Error(`status ${res.status}`);
    } catch (err) {
      lastError = err;
    }
    await new Promise((resolve) => setTimeout(resolve, 250));
  }
  throw new Error(`readout (first-run) never became ready at ${url}: ${lastError}`);
}

test.beforeAll(async () => {
  tmpDir = fs.mkdtempSync(path.join(os.tmpdir(), 'readout-firstrun-'));
  const kubeconfig = path.join(tmpDir, 'kubeconfig');
  // A valid kubeconfig with ZERO contexts: the loader succeeds, discovery
  // yields nothing, and the manager must come up empty instead of failing.
  fs.writeFileSync(kubeconfig, 'apiVersion: v1\nkind: Config\n', { mode: 0o600 });

  readout = spawn(readoutBinary(), ['--port', String(FIRSTRUN_PORT)], {
    env: {
      ...process.env,
      KUBECONFIG: kubeconfig,
      // Blank the in-cluster fallback so only the zero-context file counts.
      KUBERNETES_SERVICE_HOST: '',
      KUBERNETES_SERVICE_PORT: '',
    },
    stdio: ['ignore', 'inherit', 'inherit'],
  });
  readout.on('exit', (code, signal) => {
    // Surfaces the old fatal-startup regression loudly: a zero-context boot
    // that exits kills the suite with this message instead of a vague timeout.
    if (code !== null && code !== 0) {
      console.error(`readout (first-run) exited early: code=${code} signal=${signal}`);
    }
  });
  await waitForReady(`${FIRSTRUN_URL}/clusters`, 30_000);
});

test.afterAll(async () => {
  if (readout && !readout.killed) {
    readout.kill('SIGTERM');
  }
  if (tmpDir) {
    fs.rmSync(tmpDir, { recursive: true, force: true });
  }
});

test.beforeEach(async ({}, testInfo) => {
  test.skip(
    testInfo.project.name !== 'desktop',
    'the first-run screen is viewport-independent; one boot is enough'
  );
});

test('zero-context kubeconfig boots into the first-run screen, not an exit', async ({ page }) => {
  await page.goto(`${FIRSTRUN_URL}/clusters`);

  const card = page.locator('.ro-firstrun');
  await expect(card.locator('h3')).toHaveText('No clusters configured');
  // The zero count still renders in the title row.
  await expect(page.locator('.ro-title-row .ro-count')).toHaveText('0');
  // The command block: the binary's REAL config surface.
  const detail = card.locator('.errdetail');
  await expect(detail).toContainText('KUBECONFIG=~/.kube/config readout');
  await expect(detail).toContainText('--config');
  await expect(detail).not.toContainText('--kubeconfig');
  // Setup docs + Re-check.
  await expect(card.locator('.ro-empty-actions a', { hasText: 'Setup docs' })).toBeVisible();
  const recheck = card.locator('.ro-empty-actions a', { hasText: 'Re-check' });
  await expect(recheck).toHaveAttribute('href', '/clusters');

  // Re-check is a plain GET reload -- it works BECAUSE the server is running.
  await recheck.click();
  await expect(card.locator('h3')).toHaveText('No clusters configured');
  expect(new URL(page.url()).pathname).toBe('/clusters');

  // No login UI anywhere: no token input, no SSO card.
  await expect(page.locator('input[type="password"]')).toHaveCount(0);
  await expect(page.locator('.login-card')).toHaveCount(0);
});
