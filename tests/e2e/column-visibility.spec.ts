import { test, expect, type Page, type Response } from '@playwright/test';
import { controlURL } from './playwright.config';

// Column-visibility popover, end to end against the fakeapi harness:
//
//   - the shipped v2 default-hidden set applies: the nodes page hides
//     External-IP (and friends) on a plain load;
//   - toggling a column on in the ⊞ popover shows it WITHOUT a full page load
//     (window-marker + zero history delta + unchanged URL: a toggle is
//     cookie-state riding the RO-No-Push partial loop, never URL-state) and
//     the choice survives a hard reload (ro_prefs cookie persisted);
//   - the identity/name column renders checked + disabled, and a forced
//     ?hidecols=Name deep link still shows the name column (ignored
//     server-side);
//   - the title row keeps the TSV download + search-this-type buttons, and the
//     absorbed label-selector / labels-as-columns inputs are reachable inside
//     the popover;
//   - a pasted ?sort= deep link never writes the ro_prefs cookie (only direct
//     user interactions write the cookie, cookie-unchanged assertion).
//
// Fixture state is reset per spec; Playwright gives each test a fresh browser
// context, so cookie state never bleeds between tests.

const NODES = '/clusters/e2e/nodes';
const PODS = '/clusters/e2e/namespaces/default/pods';

async function control(path: string): Promise<void> {
  const res = await fetch(controlURL + path);
  if (!res.ok) {
    throw new Error(`control ${path}: ${res.status} ${await res.text()}`);
  }
}

// A column toggle rides the container's own programmatic path: the request is
// marked RO-No-Push (so the server never answers with HX-Push-Url).
function isNoPushTableResponse(r: Response): boolean {
  return r.url().includes('/_table') && r.request().headers()['ro-no-push'] === 'true';
}

// A user-initiated table request (a chip commit, the popover form submit):
// no RO-No-Push marker, no preload warm-up.
function isUserTableResponse(r: Response): boolean {
  const headers = r.request().headers();
  return (
    r.url().includes('/_table') &&
    headers['ro-no-push'] !== 'true' &&
    headers['hx-preloaded'] !== 'true'
  );
}

function headers(page: Page) {
  return page.locator('#resource-list-content table.ro-table thead th');
}

function header(page: Page, name: string) {
  return headers(page).filter({ hasText: name });
}

async function openColsPopover(page: Page): Promise<void> {
  await page.locator('#ro-cols-btn').click();
  await expect(page.locator('#ro-cols-pop')).toBeVisible();
}

// Click a popover entry and await the resulting no-push partial re-render.
async function toggleColumn(page: Page, name: string): Promise<void> {
  const swapped = page.waitForResponse(isNoPushTableResponse);
  await page.locator(`#ro-cols-pop .col-toggle[data-col="${name}"]`).click();
  await swapped;
}

async function roPrefsCookie(page: Page): Promise<string | undefined> {
  const cookies = await page.context().cookies();
  return cookies.find((c) => c.name === 'ro_prefs')?.value;
}

test.beforeEach(async ({}, testInfo) => {
  test.skip(
    testInfo.project.name !== 'desktop',
    'the columns popover is a desktop surface (below 760px the card layer replaces the table)'
  );
  await control('/__control/reset');
});

test('v2 defaults hide nodes noise; a toggle shows the column with no page load and persists', async ({
  page,
}) => {
  await page.goto(NODES);

  // The shipped DefaultHiddenColumns set applies on a plain load: the noise
  // columns are absent while their neighbours render.
  await expect(header(page, 'Internal-IP')).toHaveCount(1);
  for (const hidden of ['External-IP', 'OS-Image', 'Kernel-Version', 'Created']) {
    await expect(header(page, hidden)).toHaveCount(0);
  }

  // Plant a marker, record history depth + URL: the toggle must change NONE
  // of them (cookie-state, partial morph -- not a navigation, not a push).
  await page.evaluate(() => {
    (window as unknown as { __noReload: boolean }).__noReload = true;
  });
  const historyBefore = await page.evaluate(() => window.history.length);
  const urlBefore = page.url();

  await openColsPopover(page);
  // The popover offers the hidden column back, unchecked.
  await expect(
    page.locator('#ro-cols-pop .col-toggle[data-col="External-IP"] .ro-check')
  ).not.toBeChecked();
  await toggleColumn(page, 'External-IP');

  // The column appeared (header + the fixture's cell value)...
  await expect(header(page, 'External-IP')).toHaveCount(1);
  await expect(page.locator('#resource-list-content tbody')).toContainText('203.0.113.7');
  // ...the popover stayed open across the morph with the entry now checked...
  await expect(page.locator('#ro-cols-pop')).toBeVisible();
  await expect(
    page.locator('#ro-cols-pop .col-toggle[data-col="External-IP"] .ro-check')
  ).toBeChecked();
  // ...and NO full page load, NO history entry, NO URL mutation happened.
  expect(
    await page.evaluate(() => (window as unknown as { __noReload: boolean }).__noReload)
  ).toBe(true);
  expect(await page.evaluate(() => window.history.length)).toBe(historyBefore);
  expect(page.url()).toBe(urlBefore);

  // The complete hidden set was written to the cookie: a hard reload renders
  // External-IP shown while the other defaults stay hidden.
  await page.reload();
  await expect(header(page, 'External-IP')).toHaveCount(1);
  for (const hidden of ['OS-Image', 'Kernel-Version', 'Created']) {
    await expect(header(page, hidden)).toHaveCount(0);
  }
});

test('the identity column is locked on and ?hidecols=Name is ignored server-side', async ({
  page,
}) => {
  await page.goto(`${NODES}?hidecols=Name`);

  // The deep link could not hide the name column: header + sticky name cell.
  await expect(header(page, 'Name')).toHaveCount(1);
  await expect(page.locator('td.cell-name a', { hasText: 'worker-1' })).toBeVisible();

  // Its popover entry renders checked + disabled (not a toggle surface).
  await openColsPopover(page);
  const nameCheck = page.locator('#ro-cols-pop .col-toggle[data-col="Name"] .ro-check');
  await expect(nameCheck).toBeChecked();
  await expect(nameCheck).toBeDisabled();
  await expect(page.locator('#ro-cols-pop .col-toggle[data-col="Name"]')).toBeDisabled();
});

test('title-row survivors: TSV + search buttons stay, labelcols/selector live in the popover', async ({
  page,
}) => {
  await page.goto(NODES);

  await expect(
    page.locator('.ro-title-actions a[title="Download resource list as Tab-Separated-Values (TSV)"]')
  ).toBeVisible();
  await expect(page.locator('.ro-title-actions a[title="Search this type"]')).toBeVisible();

  // The absorbed tools-form inputs are reachable inside the popover; the old
  // toggle-tools form chrome is gone from single-type pages.
  await openColsPopover(page);
  await expect(page.locator('#ro-cols-labelcols')).toBeVisible();
  await expect(page.locator('#ro-cols-selector')).toBeVisible();
  await expect(page.locator('form.tools-form')).toHaveCount(0);

  // The placeholders must be READABLE: the example text fits the input's
  // content box un-clipped (a cut-off hint is no hint -- field report
  // 2026-06-11), and the full explanation rides the title tooltip.
  for (const id of ['ro-cols-labelcols', 'ro-cols-selector']) {
    const fits = await page.locator(`#${id}`).evaluate((el) => {
      const input = el as HTMLInputElement;
      const cs = getComputedStyle(input);
      const ctx = document.createElement('canvas').getContext('2d')!;
      ctx.font = `${cs.fontStyle} ${cs.fontWeight} ${cs.fontSize} ${cs.fontFamily}`;
      const text = ctx.measureText(input.placeholder).width;
      const room =
        input.clientWidth - parseFloat(cs.paddingLeft) - parseFloat(cs.paddingRight);
      return { text, room, ok: text <= room };
    });
    expect(fits.ok, `#${id} placeholder ${fits.text}px must fit ${fits.room}px`).toBe(true);
    await expect(page.locator(`#${id}`)).toHaveAttribute('title', /.{20,}/);
  }
});

test('a popover selector submit keeps the active filter chip (f= merge)', async ({ page }) => {
  await page.goto(PODS);

  // Commit a status chip through the editor: the URL gains the encoded f= pair.
  const input = page.locator('#ro-filter-input');
  await input.click();
  await input.pressSequentially('status:Running');
  const chipSwap = page.waitForResponse(isUserTableResponse);
  await input.press('Enter');
  await chipSwap;
  await expect(page).toHaveURL(/f=status%3ARunning/);
  await expect(page.locator('#ro-filter-field .ro-scope-chip')).toHaveCount(1);

  // Submit a selector from the popover's absorbed form. The intercepted
  // submit must MERGE into the live query -- the regression was a native GET
  // rebuilt from the round-trip hidden inputs, which wiped the f= chip.
  await openColsPopover(page);
  await page.locator('#ro-cols-selector').fill('app=nginx');
  const submitSwap = page.waitForResponse(isUserTableResponse);
  await page.locator('#ro-cols-pop form.ro-pop-form button[type="submit"]').click();
  await submitSwap;

  // The chip survived in the URL (byte-exact) AND in the editor UI, with the
  // selector applied alongside it -- and no full navigation happened (the
  // canonical URL came from HX-Push-Url, the document path stayed the list).
  await expect(page).toHaveURL(/f=status%3ARunning/);
  await expect(page).toHaveURL(/selector=app%3Dnginx/);
  expect(new URL(page.url()).pathname).not.toContain('_table');
  const chip = page.locator('#ro-filter-field .ro-scope-chip');
  await expect(chip).toHaveCount(1);
  await expect(chip.first()).toContainText('status');
  await expect(chip.first()).toContainText('Running');
  // The server echoed the selector back into the re-rendered popover form --
  // it SAW the param (the merge reached SSR, not just the address bar).
  await expect(page.locator('#ro-cols-selector')).toHaveValue('app=nginx');
});

test('a pasted ?sort= deep link does not write the ro_prefs cookie', async ({ page }) => {
  expect(await roPrefsCookie(page)).toBeUndefined();

  await page.goto(`${NODES}?sort=Version`);
  await expect(page.locator('th.sorted', { hasText: 'Version' })).toBeVisible();

  // Writes happen only on direct popover/sort-header interactions -- merely
  // ARRIVING with explicit params must leave the cookie untouched (absent).
  await page.waitForTimeout(400);
  expect(await roPrefsCookie(page)).toBeUndefined();
});
