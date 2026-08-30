import { test, expect, type Page, type Response } from '@playwright/test';
import { controlURL } from './playwright.config';

// ro_prefs cookie write surfaces, end to end with the SERVER FILL as the oracle:
// each spec performs the direct user interaction that writes the cookie, then
// forces a fresh server render (a bare-URL goto / a reload / a different page)
// and asserts the persisted state in the SSR markup -- the same renders
// prefs_test.go pins from hand-built cookies, here driven by the real JS
// writer. Three of the four write surfaces live here:
//
//   - sort click -> a later BARE-url load renders the cookie-filled sort
//     (rows re-ordered + th.sorted) while the URL itself stays clean (the
//     fill is render-only, never materialized into the address bar);
//   - Live toggle -> a reload renders the persisted mode into the topbar
//     (the server-painted aria-pressed on [data-ro-action="toggle-live"]);
//   - namespace switch -> the clusters page's entry link points into the
//     persisted namespace's pods list.
//
// The fourth surface (column toggle) is covered by column-visibility.spec.ts.
// Fixture state is reset per spec; each test gets a fresh browser context, so
// cookie state never bleeds between tests.

const PODS = '/clusters/e2e/namespaces/default/pods';

async function control(path: string): Promise<void> {
  const res = await fetch(controlURL + path);
  if (!res.ok) {
    throw new Error(`control ${path}: ${res.status} ${await res.text()}`);
  }
}

function isUserTableResponse(r: Response): boolean {
  const headers = r.request().headers();
  return r.url().includes('/_table') && headers['ro-no-push'] !== 'true';
}

async function clickSort(page: Page, label: string): Promise<void> {
  const header = page.locator('thead th a', { hasText: label }).first();
  const before = await header.getAttribute('href');
  const swapped = page.waitForResponse(isUserTableResponse);
  await header.click();
  await swapped;
  // The response is not the swap. Wait for the morphed header to publish its
  // NEXT sort target: a second click issued before the morph lands would just
  // re-request the direction that is already applied.
  await expect
    .poll(() => page.locator('thead th a', { hasText: label }).first().getAttribute('href'))
    .not.toBe(before);
}

function rowNames(page: Page) {
  return page.locator('#resource-list-content table.ro-table tbody td.cell-name');
}

const LIVE_TOGGLE = '[data-ro-action="toggle-live"]';

test.beforeEach(async ({}, testInfo) => {
  test.skip(
    testInfo.project.name !== 'desktop',
    'the prefs write surfaces are desktop chrome (below 760px the card layer replaces the sortable table)'
  );
  await control('/__control/reset');
});

test('a sort click persists: a bare-URL load renders the cookie-filled sort', async ({ page }) => {
  await page.goto(PODS);
  await expect(rowNames(page)).toHaveText(['nginx', 'my-app']);

  // The direct interaction write: a sort-header click (rides the v2 loop).
  await clickSort(page, 'Name');
  await expect(page).toHaveURL(/\?sort=Name$/);
  await expect(rowNames(page)).toHaveText(['my-app', 'nginx']);

  // A LATER load of the bare list URL -- no query at all. Only the cookie can
  // order these rows: the server fill renders the persisted sort (rows +
  // th.sorted) while the URL stays clean (the fill is render-only).
  await page.goto(PODS);
  await expect(rowNames(page)).toHaveText(['my-app', 'nginx']);
  await expect(page.locator('th.sorted', { hasText: 'Name' })).toBeVisible();
  expect(new URL(page.url()).search).toBe('');
});

test('the Live toggle persists: a reload renders it pressed from the cookie', async ({ page }) => {
  await page.goto(PODS);
  const toggle = page.locator(LIVE_TOGGLE);
  await expect(toggle).toHaveAttribute('aria-pressed', 'false');

  // The direct interaction write: one click on the topbar Live toggle.
  await toggle.click();
  await expect(toggle).toHaveAttribute('aria-pressed', 'true');

  // A fresh server render carries the persisted mode at SSR -- the cookie is
  // the only carrier across this reload, and the SERVER paints aria-pressed
  // (asserted before any client JS could have re-synced it, via the markup the
  // reload response delivered).
  await page.reload();
  await expect(page.locator(LIVE_TOGGLE)).toHaveAttribute('aria-pressed', 'true');

  // ... and turning it back off persists the same way.
  await page.locator(LIVE_TOGGLE).click();
  await expect(page.locator(LIVE_TOGGLE)).toHaveAttribute('aria-pressed', 'false');
  await page.reload();
  await expect(page.locator(LIVE_TOGGLE)).toHaveAttribute('aria-pressed', 'false');
});

test('a namespace switch persists: the clusters page entry link points into it', async ({
  page,
}) => {
  await page.goto(PODS);

  // The direct interaction write: switch namespaces via the topbar dropdown
  // (the click records the pref, then the boosted navigation proceeds).
  await page.locator('#namespace-dropdown .context-trigger').click();
  const item = page.locator('#namespace-dropdown .namespace-item', {
    hasText: 'kube-system',
  });
  await expect(item).toBeVisible();
  await item.click();
  await expect(page).toHaveURL(/\/namespaces\/kube-system\/pods$/);

  // The consumer surface (href-only): the namespace pref is read only when
  // building cluster-entry hrefs, so the cluster row's entry link on the
  // clusters page now points into the persisted namespace's pods list.
  await page.goto('/clusters');
  await expect(page.locator('td.cl-name a', { hasText: 'e2e' })).toHaveAttribute(
    'href',
    '/clusters/e2e/namespaces/kube-system/pods'
  );
});
