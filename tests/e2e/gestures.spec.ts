import { test, expect, type Page, type Response } from '@playwright/test';
import { controlURL } from './playwright.config';

// Row gestures (Unit 16 / D10) on single-type pages, end to end:
//
//   - the three gestures stay DISTINCT (SPEC §5): name-click opens (the
//     anchor), row-click toggles selection, right-click opens the context
//     menu bound to that row's server-resolved data attributes;
//   - menu composition is per-kind: pods get 5 actions (View logs included),
//     every other kind 4;
//   - the bulk bar (SPEC §6.4) appears at >=1 selected with "N selected",
//     Copy names joins the FULL names with newlines (inline "Copied", no
//     toast), ✕ clears;
//   - THE identity proof: selection is keyed by data-key, not row position --
//     after a sort reorder AND a refresh-tick morph the SAME keys stay
//     selected, so a bulk action can never hit the wrong objects;
//   - an hx-boost navigation to another kind clears the selection.
//
// Driven against the fakeapi harness; clipboard runs against the real async
// clipboard API (127.0.0.1 is a secure context; permissions granted below).

const PODS = '/clusters/e2e/namespaces/default/pods';
const SERVICES = '/clusters/e2e/namespaces/default/services';
const PODS_LIST_PATH = '/api/v1/namespaces/default/pods';
const POD_BASE = '/clusters/e2e/namespaces/default/pods/nginx';

test.use({ permissions: ['clipboard-read', 'clipboard-write'] });

async function control(path: string): Promise<void> {
  const res = await fetch(controlURL + path);
  if (!res.ok) {
    throw new Error(`control ${path}: ${res.status} ${await res.text()}`);
  }
}

async function scriptEvents(events: object[]): Promise<void> {
  const res = await fetch(`${controlURL}/__control/watch-script`, {
    method: 'POST',
    headers: { 'content-type': 'application/json' },
    body: JSON.stringify({ events }),
  });
  if (!res.ok) {
    throw new Error(`watch-script: ${res.status} ${await res.text()}`);
  }
}

function podObject(name: string, created: string) {
  return {
    apiVersion: 'v1',
    kind: 'Pod',
    metadata: { name, namespace: 'default', creationTimestamp: created, uid: `uid-${name}` },
    status: { phase: 'Running' },
  };
}

// seedThird adds a third pod so three-row selection and reorder scenarios have
// distinguishable natural vs sorted orders (zeta sorts last by name).
async function seedThird(): Promise<void> {
  await scriptEvents([
    {
      path: PODS_LIST_PATH,
      type: 'ADDED',
      object: podObject('zeta', '2025-01-01T00:00:00Z'),
      cells: ['zeta', '1/1', 'Running', '0', '1y'],
    },
  ]);
}

function isTickResponse(r: Response): boolean {
  return r.url().includes('/_table') && r.request().headers()['ro-no-push'] === 'true';
}

function isUserTableResponse(r: Response): boolean {
  const headers = r.request().headers();
  return (
    r.url().includes('/_table') &&
    headers['ro-no-push'] !== 'true' &&
    headers['hx-preloaded'] !== 'true'
  );
}

function waitForTick(page: Page): Promise<Response> {
  return page.waitForResponse(isTickResponse, { timeout: 15_000 });
}

async function clickSort(page: Page, label: string): Promise<void> {
  const swapped = page.waitForResponse(isUserTableResponse);
  await page.locator('thead th a', { hasText: label }).first().click();
  await swapped;
}

// The navbar interval menu opens on hover (CSS :hover/:focus-within).
async function pickInterval(page: Page, secs: number): Promise<void> {
  await page.locator('#refresh-dropdown').hover();
  await page.locator(`.refresh-option[data-interval="${secs}"]`).click();
}

// selectRow toggles a row through the row-click gesture: the Ready cell
// (td #2) is plain text, so the click cannot hit the name anchor or any other
// interactive descendant.
async function selectRow(page: Page, key: string): Promise<void> {
  await page.locator(`tr[data-key="${key}"] td`).nth(1).click();
}

function selectedKeys(page: Page): Promise<string[]> {
  return page.evaluate(() =>
    Array.from(document.querySelectorAll('#resource-list-content tr.is-selected'), (tr) =>
      (tr as HTMLElement).dataset.key ?? ''
    )
  );
}

const menuItems = '#ro-ctxmenu [data-ro-action]:not([hidden])';

test.beforeEach(async ({}, testInfo) => {
  test.skip(
    testInfo.project.name !== 'desktop',
    'row gestures are a desktop surface (below 760px the card layer replaces the table, D22)'
  );
  await control('/__control/reset');
});

test('right-click on a pod row opens the 5-action menu bound to that row; esc closes; Open navigates', async ({
  page,
}) => {
  await page.goto(PODS);
  const row = page.locator('tr[data-key="e2e/default/nginx"]');
  const menu = page.locator('#ro-ctxmenu');

  await row.click({ button: 'right' });
  await expect(menu).toHaveClass(/is-open/);

  // Pods compose all five actions -- View logs included -- in the D10 order,
  // each bound to THIS row's server-resolved target.
  await expect(page.locator(menuItems)).toHaveCount(5);
  await expect(page.locator(menuItems)).toHaveText([
    /Open/,
    /Copy name/,
    /View YAML/,
    /View logs/,
    /Download YAML/,
  ]);
  await expect(page.locator('[data-ro-action="open"]')).toHaveAttribute('data-href', POD_BASE);
  await expect(page.locator('[data-ro-action="yaml"]')).toHaveAttribute('data-href', `${POD_BASE}?view=yaml`);
  await expect(page.locator('[data-ro-action="logs"]')).toHaveAttribute('data-href', `${POD_BASE}/logs`);
  await expect(page.locator('[data-ro-action="download"]')).toHaveAttribute(
    'data-href',
    `${POD_BASE}?download=yaml`
  );

  // esc closes without acting.
  await page.keyboard.press('Escape');
  await expect(menu).not.toHaveClass(/is-open/);

  // Re-open and activate Open: lands on the pod detail page.
  await row.click({ button: 'right' });
  await page.locator('[data-ro-action="open"]').click();
  await page.waitForURL(`**${POD_BASE}`);
  await expect(page.locator('.ro-topbar')).toBeVisible();
});

test('name click opens the detail page (the open gesture, not the toggle)', async ({ page }) => {
  await page.goto(PODS);

  // The OPEN gesture (SPEC §5): the sticky name cell's anchor lands on the
  // object detail. (Its distinctness from the row-click TOGGLE is carried by
  // the selection tests: their row clicks hit a plain td and select without
  // navigating; this one navigates.)
  await page.locator('tr[data-key="e2e/default/nginx"] td.cell-name a').click();
  await page.waitForURL(`**${POD_BASE}`);
  await expect(page.locator('.ro-topbar')).toBeVisible();
  await expect(page.locator('.ro-detail-title .pn-head')).toHaveText('nginx');
  // A boosted navigation, and the detail page mounts no bulk bar.
  await expect(page.locator('#ro-bulkbar')).toHaveCount(0);
});

test('right-click on a service row shows 4 actions -- View logs is pods-only', async ({ page }) => {
  await page.goto(SERVICES);
  await page.locator('tr[data-key="e2e/default/frontend"]').click({ button: 'right' });

  await expect(page.locator('#ro-ctxmenu')).toHaveClass(/is-open/);
  await expect(page.locator(menuItems)).toHaveCount(4);
  await expect(page.locator('[data-ro-action="logs"]')).toBeHidden();
  const base = '/clusters/e2e/namespaces/default/services/frontend';
  await expect(page.locator('[data-ro-action="open"]')).toHaveAttribute('data-href', base);
  await expect(page.locator('[data-ro-action="yaml"]')).toHaveAttribute('data-href', `${base}?view=yaml`);
});

test('row clicks toggle selection and the bulk bar counts; ✕ clears', async ({ page }) => {
  await seedThird();
  await page.goto(PODS);
  const bar = page.locator('#ro-bulkbar');
  await expect(bar).not.toHaveClass(/is-open/);

  await selectRow(page, 'e2e/default/nginx');
  await selectRow(page, 'e2e/default/my-app');
  await selectRow(page, 'e2e/default/zeta');
  await expect(bar).toHaveClass(/is-open/);
  await expect(page.locator('#ro-bulk-count')).toHaveText('3 selected');
  await expect(page.locator('tr.is-selected')).toHaveCount(3);

  // A second click on a selected row DEselects it (toggle, not accumulate).
  await selectRow(page, 'e2e/default/my-app');
  await expect(page.locator('#ro-bulk-count')).toHaveText('2 selected');
  expect(await selectedKeys(page)).toEqual(['e2e/default/nginx', 'e2e/default/zeta']);

  // ✕ clears everything and the bar fades out.
  await page.locator('#ro-bulk-clear').click();
  await expect(bar).not.toHaveClass(/is-open/);
  await expect(page.locator('tr.is-selected')).toHaveCount(0);
});

test('selection is keyed by name, not position: sort reorder + tick keep the SAME keys', async ({
  page,
}) => {
  await seedThird();
  await page.goto(PODS);

  // Natural order: nginx(0), my-app(1), zeta(2). Select positions 0 and 2 BY
  // NAME (their data-keys).
  await expect(page.locator('#resource-list-content td.cell-name')).toHaveText([
    'nginx',
    'my-app',
    'zeta',
  ]);
  await selectRow(page, 'e2e/default/nginx');
  await selectRow(page, 'e2e/default/zeta');
  expect(await selectedKeys(page)).toEqual(['e2e/default/nginx', 'e2e/default/zeta']);

  // Sort by Name: the rows REORDER to my-app(0), nginx(1), zeta(2). If
  // selection were positional, position 0 (now my-app) would be selected;
  // identity keeps the SAME two keys on their new positions.
  await clickSort(page, 'Name');
  await expect(page.locator('#resource-list-content td.cell-name')).toHaveText([
    'my-app',
    'nginx',
    'zeta',
  ]);
  expect((await selectedKeys(page)).sort()).toEqual(['e2e/default/nginx', 'e2e/default/zeta']);
  await expect(page.locator('tr[data-key="e2e/default/my-app"]')).not.toHaveClass(/is-selected/);

  // Force a refresh tick that lands a NEW object mid-list (omega sorts between
  // nginx and zeta) -- a full server fragment morphs in and the positions
  // shift again. The same two keys MUST come out selected: this is the proof
  // bulk actions can never hit wrong objects after reorders.
  await pickInterval(page, 5);
  await scriptEvents([
    {
      path: PODS_LIST_PATH,
      type: 'ADDED',
      object: podObject('omega', '2025-06-01T00:00:00Z'),
      cells: ['omega', '1/1', 'Running', '0', '1y'],
    },
  ]);
  await waitForTick(page);
  await expect(page.locator('#resource-list-content td.cell-name')).toHaveText([
    'my-app',
    'nginx',
    'omega',
    'zeta',
  ]);
  expect((await selectedKeys(page)).sort()).toEqual(['e2e/default/nginx', 'e2e/default/zeta']);
  await expect(page.locator('#ro-bulk-count')).toHaveText('2 selected');
  await expect(page.locator('tr[data-key="e2e/default/omega"]')).not.toHaveClass(/is-selected/);
});

test('Copy names yields newline-joined full names (live-filtered selections included); Copy name copies one', async ({
  page,
}) => {
  await page.goto(PODS);
  await selectRow(page, 'e2e/default/nginx');
  await selectRow(page, 'e2e/default/my-app');

  // Narrow the LIVE free-text filter to nginx: my-app's row hides client-side
  // but stays selected in the store -- PINNED: Copy names still includes it
  // (selection is explicit user intent; the store, not the DOM, feeds bulk).
  await page.locator('#ro-filter-input').fill('nginx');
  await expect(page.locator('tr[data-key="e2e/default/my-app"]')).toHaveClass(/ro-row-filtered/);

  await page.locator('#ro-bulk-copy').click();
  // Inline "Copied" feedback on the button itself (no toast), then reverts.
  await expect(page.locator('#ro-bulk-copy span:last-child')).toHaveText('Copied');
  expect(await page.evaluate(() => navigator.clipboard.readText())).toBe('nginx\nmy-app');
  await expect(page.locator('#ro-bulk-copy span:last-child')).toHaveText('Copy names');

  // Context-menu Copy name: the single FULL row name.
  await page.locator('tr[data-key="e2e/default/nginx"]').click({ button: 'right' });
  await page.locator('[data-ro-action="copy"]').click();
  await expect(page.locator('#ro-ctxmenu')).not.toHaveClass(/is-open/);
  await expect
    .poll(() => page.evaluate(() => navigator.clipboard.readText()))
    .toBe('nginx');
});

test('an hx-boost navigation to another kind clears the selection', async ({ page }) => {
  await page.goto(PODS);
  await selectRow(page, 'e2e/default/nginx');
  await selectRow(page, 'e2e/default/my-app');
  await expect(page.locator('#ro-bulk-count')).toHaveText('2 selected');

  // Boosted sidebar navigation to Services = the screen change (SPEC §6.4).
  await page.locator('.ro-sidebar a', { hasText: 'Services' }).click();
  await page.waitForURL(`**${SERVICES}`);
  await expect(page.locator('#ro-bulkbar')).not.toHaveClass(/is-open/);
  expect(
    await page.evaluate(() => (window as any).roRowState.selectedKeys() as string[])
  ).toEqual([]);

  // And back on pods nothing is decorated stale.
  await page.locator('.ro-sidebar a', { hasText: 'Pods' }).first().click();
  await page.waitForURL(`**${PODS}`);
  await expect(page.locator('tr[data-key="e2e/default/nginx"]')).toBeVisible();
  await expect(page.locator('tr.is-selected')).toHaveCount(0);
  await expect(page.locator('#ro-bulkbar')).not.toHaveClass(/is-open/);
});
