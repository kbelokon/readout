import { test, expect } from '@playwright/test';
import { controlURL } from './playwright.config';

// The SPEC §7.15 long-annotation toggle on the pod detail page: a >120-char
// annotation value renders as a collapsed `key · size` button (`[data-annolong]`)
// with a hidden scrollable <pre class="anno-pre"> payload. The toggle is
// all-new delegated JS (readout.js flips the [hidden] attribute + mirrors
// aria-expanded + rotates the chevron via .open) with no other behavioral
// gate, so the click interaction is proven end to end here. Driven against the
// fakeapi nginx pod fixture, whose last-applied-configuration annotation is
// 218 chars (past the 120 threshold); the short example.com/note annotation
// stays a plain chip alongside it.

const POD = '/clusters/e2e/namespaces/default/pods/nginx';

test.beforeEach(async ({}, testInfo) => {
  test.skip(
    testInfo.project.name !== 'desktop',
    'the long-annotation toggle is viewport-independent; one walk on the desktop chrome suffices'
  );
  const res = await fetch(`${controlURL}/__control/reset`);
  if (!res.ok) {
    throw new Error(`control reset: ${res.status} ${await res.text()}`);
  }
});

test('a long annotation expands to its <pre> payload and collapses back', async ({ page }) => {
  await page.goto(POD);

  const block = page.locator('.anno-long');
  const toggle = block.locator('button[data-annolong]');
  const pre = block.locator('pre.anno-pre');

  // Collapsed render: the toggle names the key, the payload is hidden, and the
  // short annotation still renders as a plain chip next to it.
  await expect(block).toHaveCount(1);
  await expect(toggle.locator('.ck')).toHaveText('kubectl.kubernetes.io/last-applied-configuration');
  await expect(toggle).toHaveAttribute('aria-expanded', 'false');
  await expect(pre).toBeHidden();
  await expect(
    page.locator('span.ro-chip.anno', { hasText: 'example.com/note' })
  ).toBeVisible();

  // Click: the payload reveals IN PLACE (no navigation) with the FULL verbatim
  // value; aria-expanded mirrors the state and the chevron earns .open.
  await toggle.click();
  await expect(pre).toBeVisible();
  await expect(pre).toContainText('"kind":"Pod"');
  await expect(toggle).toHaveAttribute('aria-expanded', 'true');
  await expect(toggle).toHaveClass(/open/);
  expect(new URL(page.url()).pathname).toBe(POD);

  // Second click: collapsed again -- payload hidden, state mirrored back.
  await toggle.click();
  await expect(pre).toBeHidden();
  await expect(toggle).toHaveAttribute('aria-expanded', 'false');
  await expect(toggle).not.toHaveClass(/open/);
});
