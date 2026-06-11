import { test, expect, type Page, type Response } from '@playwright/test';
import { controlURL } from './playwright.config';

// ro_prefs write surfaces (D9), end to end with the SERVER FILL as the oracle:
// each spec performs the direct user interaction that writes the cookie, then
// forces a fresh server render (a bare-URL goto / a reload / a different page)
// and asserts the persisted state in the SSR markup -- the same renders
// prefs_test.go pins from hand-built cookies, here driven by the real JS
// writer. Three of the four write surfaces live here:
//
//   - sort click -> a later BARE-url load renders the cookie-filled sort
//     (rows re-ordered + th.sorted) while the URL itself stays clean (the
//     fill is render-only, never materialized into the address bar);
//   - interval pick -> a reload renders the persisted mode into the topbar
//     (#refresh-label text + the active .refresh-option);
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
  return (
    r.url().includes('/_table') &&
    headers['ro-no-push'] !== 'true' &&
    headers['hx-preloaded'] !== 'true'
  );
}

async function clickSort(page: Page, label: string): Promise<void> {
  const swapped = page.waitForResponse(isUserTableResponse);
  await page.locator('thead th a', { hasText: label }).first().click();
  await swapped;
}

function rowNames(page: Page) {
  return page.locator('#resource-list-content table.ro-table tbody td.cell-name');
}

// The navbar interval menu opens on hover (CSS :hover/:focus-within).
async function pickInterval(page: Page, secs: number): Promise<void> {
  await page.locator('#refresh-dropdown').hover();
  await page.locator(`.refresh-option[data-ro-interval="${secs}"]`).click();
}

test.beforeEach(async ({}, testInfo) => {
  test.skip(
    testInfo.project.name !== 'desktop',
    'the prefs write surfaces are desktop chrome (below 760px the card layer replaces the sortable table, D22)'
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

test('an interval pick persists: a reload renders 30s active from the cookie', async ({ page }) => {
  await page.goto(PODS);

  // The direct interaction write: pick 30s in the topbar dropdown.
  await pickInterval(page, 30);
  await expect(page.locator('#refresh-label')).toHaveText('30s');

  // A fresh server render carries the persisted mode at SSR: the label text,
  // the active option, and ONLY that option active.
  await page.reload();
  await expect(page.locator('#refresh-label')).toHaveText('30s');
  await expect(page.locator('.refresh-option[data-ro-interval="30"]')).toHaveClass(
    /is-active/
  );
  await expect(page.locator('.refresh-option[data-ro-interval="0"]')).not.toHaveClass(
    /is-active/
  );
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

  // The consumer surface (href-only, D9): the cluster row's entry link on the
  // clusters page now points into the persisted namespace's pods list.
  await page.goto('/clusters');
  await expect(page.locator('td.cl-name a', { hasText: 'e2e' })).toHaveAttribute(
    'href',
    '/clusters/e2e/namespaces/kube-system/pods'
  );
});
