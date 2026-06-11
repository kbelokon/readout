import { test, expect } from '@playwright/test';
import { controlURL } from './playwright.config';

// The SPEC §4.9/§4.10 in-cell overflow toggle on a configmap data-keys cell
// (Unit 11): the `+N keys` button is all-new delegated JS with no other
// behavioral gate, so the click interaction is proven end to end here --
// extra chips hidden at render, revealed in place by a click (button face
// flips to "less"), hidden again by a second click. Driven against the
// fakeapi configmaps fixture: app-config carries 5 keys (4 data + 1
// binaryData), 2 past the keysCellMax=3 threshold.

const CONFIGMAPS = '/clusters/e2e/namespaces/default/configmaps';

test.beforeEach(async ({}, testInfo) => {
  test.skip(
    testInfo.project.name !== 'desktop',
    'the keys cell rides the table layer; below 760px the card layer replaces it (D22)'
  );
  const res = await fetch(`${controlURL}/__control/reset`);
  if (!res.ok) {
    throw new Error(`control reset: ${res.status} ${await res.text()}`);
  }
});

test('+N keys expands the data chips in-cell and collapses back', async ({ page }) => {
  await page.goto(CONFIGMAPS);

  const strip = page.locator(
    'table.ro-table tr[data-key="e2e/default/app-config"] .ro-chips'
  );
  const chips = strip.locator('.ro-chip:not(.more)');
  const extras = strip.locator('.ro-chip.xtra');
  const more = strip.locator('button.ro-chip.more[data-ro-more]');

  // Collapsed render: all 5 key chips are in the DOM, the 2 past the
  // threshold are hidden, and the button face reads "+2 keys".
  await expect(chips).toHaveCount(5);
  await expect(extras).toHaveCount(2);
  for (const extra of await extras.all()) {
    await expect(extra).toBeHidden();
  }
  await expect(more).toHaveAttribute('aria-expanded', 'false');
  await expect(more.locator('.more-n')).toBeVisible();
  await expect(more.locator('.more-n')).toHaveText('+2 keys');
  await expect(more.locator('.more-less')).toBeHidden();

  // Click: the strip expands IN-CELL -- the extra chips become visible right
  // where they are (no navigation, no overlay) and the button reads "less".
  await more.click();
  await expect(strip).toHaveClass(/expanded/);
  for (const extra of await extras.all()) {
    await expect(extra).toBeVisible();
  }
  await expect(more).toHaveAttribute('aria-expanded', 'true');
  await expect(more.locator('.more-less')).toBeVisible();
  await expect(more.locator('.more-less')).toHaveText('less');
  await expect(more.locator('.more-n')).toBeHidden();
  // Still the same page: the toggle is a peek, not a navigation.
  expect(new URL(page.url()).pathname).toBe(CONFIGMAPS);

  // Second click: collapsed again -- extras hidden, face back to "+2 keys".
  await more.click();
  await expect(strip).not.toHaveClass(/expanded/);
  for (const extra of await extras.all()) {
    await expect(extra).toBeHidden();
  }
  await expect(more).toHaveAttribute('aria-expanded', 'false');
  await expect(more.locator('.more-n')).toBeVisible();
});
