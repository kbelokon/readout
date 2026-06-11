import { test, expect, type Page, type Response } from '@playwright/test';
import { controlURL } from './playwright.config';

// The v2 list interaction loop (D6) on a single-type page, end to end:
//
//   - a sort-header click is a `_table` partial request morphed into the
//     persistent container, and the HISTORY entry it creates is the CANONICAL
//     list URL (never the partial URL -- a reload of the pushed entry must
//     render a full page);
//   - the refresh tick derives its request URL from location.href at fire
//     time, so it can never revert a pushed sort (the exact regression the
//     old render-time-baked PartialURL contract produced);
//   - ticks are programmatic (RO-No-Push): history.length stays flat no
//     matter how many fire;
//   - the morph path is CSP-safe (config delivered as a JS object inside the
//     ro-morph extension -- no securitypolicyviolation, ever);
//   - rows carry data-key/id object identity: idiomorph keeps the same DOM
//     nodes across morphs and readout.js re-keys selection state onto them.
//
// Driven against the fakeapi harness; fixture state is scripted through the
// control surface and reset before every spec.

const PODS = '/clusters/e2e/namespaces/default/pods';
const PODS_LIST_PATH = '/api/v1/namespaces/default/pods';

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

// seedAges stamps creationTimestamps onto the two fixture pods (MODIFIED keeps
// their Table cells) and adds a third, oldest pod -- sort=Age needs distinct
// object timestamps (kube.SortTable reads metadata.creationTimestamp, not the
// display cell), and three rows make sorted-vs-natural order distinguishable.
async function seedAges(): Promise<void> {
  await scriptEvents([
    { path: PODS_LIST_PATH, type: 'MODIFIED', object: podObject('nginx', '2026-01-01T00:00:00Z') },
    { path: PODS_LIST_PATH, type: 'MODIFIED', object: podObject('my-app', '2026-03-01T00:00:00Z') },
    {
      path: PODS_LIST_PATH,
      type: 'ADDED',
      object: podObject('zeta', '2025-01-01T00:00:00Z'),
      cells: ['zeta', '1/1', 'Running', '0', '1y'],
    },
  ]);
}

// A tick (or any programmatic re-fetch) marks itself RO-No-Push; a user sort
// click does not. Matching on the REQUEST header keeps the two awaitable
// independently (and ignores preload warm-ups, which carry HX-Preloaded).
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
    'the v2 list loop is a desktop surface (below 760px the card layer replaces the sortable table, D22)'
  );
  await control('/__control/reset');
});

test('sort click morphs the table and pushes the canonical URL', async ({ page }) => {
  await page.goto(PODS);
  await expect(rowNames(page)).toHaveText(['nginx', 'my-app']);

  await clickSort(page, 'Name');

  // The pushed URL is the canonical list page + query -- never the partial.
  await expect(page).toHaveURL(/\?sort=Name$/);
  expect(new URL(page.url()).pathname).not.toContain('_table');
  // No navigation happened: the chrome is intact and only the rows re-sorted.
  await expect(page.locator('.ro-topbar')).toBeVisible();
  await expect(rowNames(page)).toHaveText(['my-app', 'nginx']);

  // A hard reload of the pushed entry renders a FULL page (topbar present),
  // not a bare fragment -- the push header carried the canonical URL.
  await page.reload();
  await expect(page.locator('.ro-topbar')).toBeVisible();
  await expect(rowNames(page)).toHaveText(['my-app', 'nginx']);
});

test('refresh tick derives its URL from location and keeps the user sort', async ({ page }) => {
  await seedAges();
  await page.goto(PODS);
  await expect(rowNames(page)).toHaveText(['nginx', 'my-app', 'zeta']);

  // Two header clicks: Age ascending, then the freshly-morphed header's
  // descending toggle. sort=Age:desc lists oldest first.
  await clickSort(page, 'Age');
  await clickSort(page, 'Age');
  await expect(page).toHaveURL(/sort=Age%3Adesc/);
  await expect(rowNames(page)).toHaveText(['zeta', 'nginx', 'my-app']);

  // Arm a 5s interval, change cluster state, and await ONE tick. The tick must
  // re-fetch the LIVE URL (sort=Age:desc): under the old baked-PartialURL
  // contract it re-fetched the page's load-time query and reverted the sort.
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

  // Row order AND the URL still reflect age:desc; the new object landed in
  // its sorted position (the morph applied a server-sorted fragment).
  await expect(rowNames(page)).toHaveText(['zeta', 'omega', 'nginx', 'my-app']);
  await expect(page).toHaveURL(/sort=Age%3Adesc/);
});

test('history.length is unchanged across two awaited ticks', async ({ page }) => {
  await page.goto(PODS);
  await pickInterval(page, 5);

  const before = await page.evaluate(() => window.history.length);
  await waitForTick(page);
  await waitForTick(page);
  const after = await page.evaluate(() => window.history.length);

  expect(after).toBe(before);
  // And the document URL never became the partial.
  expect(new URL(page.url()).pathname).not.toContain('_table');
});

test('no securitypolicyviolation fires during user and tick morphs', async ({ page }) => {
  await page.addInitScript(() => {
    (window as unknown as { __cspViolations: string[] }).__cspViolations = [];
    document.addEventListener('securitypolicyviolation', (e) => {
      const ev = e as SecurityPolicyViolationEvent;
      (window as unknown as { __cspViolations: string[] }).__cspViolations.push(
        `${ev.violatedDirective} ${ev.blockedURI}`
      );
    });
  });
  await page.goto(PODS);

  // One user-initiated morph (sort click) ...
  await clickSort(page, 'Name');
  await expect(rowNames(page)).toHaveText(['my-app', 'nginx']);
  // ... and one programmatic tick morph.
  await pickInterval(page, 5);
  await waitForTick(page);

  // Both morphs ran through the JS-config path; an attribute-spec morph config
  // would have tripped script-src 'self' (Function() eval) right here.
  expect(
    await page.evaluate(() => (window as unknown as { __cspViolations: string[] }).__cspViolations)
  ).toEqual([]);
});

test('row identity is stable across morphs and selection state re-keys onto it', async ({ page }) => {
  await page.goto(PODS);
  const key = 'e2e/default/nginx';
  const row = page.locator(`tr[data-key="${key}"]`);

  // The D6 identity contract: data-key plus the id derived from it.
  await expect(row).toHaveAttribute('id', 'row-e2e/default/nginx');

  // Select the row through the identity-keyed store (the Unit-16 gesture seam)
  // and remember the DOM node itself.
  await page.evaluate((k) => {
    const w = window as unknown as {
      __nginxRow: Element | null;
      roRowState: { setSelected(key: string, on: boolean): void };
    };
    w.__nginxRow = document.querySelector(`tr[data-key="${k}"]`);
    w.roRowState.setSelected(k, true);
  }, key);
  await expect(row).toHaveClass(/is-selected/);

  // Change cluster state and let a tick morph the fragment.
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
  await expect(page.locator('tr[data-key="e2e/default/omega"]')).toBeVisible();

  // idiomorph matched the row by id: the SAME DOM node survived the morph, and
  // the morph-wiped selection class was re-applied from the store.
  expect(
    await page.evaluate(
      (k) =>
        (window as unknown as { __nginxRow: Element | null }).__nginxRow ===
        document.querySelector(`tr[data-key="${k}"]`),
      key
    )
  ).toBe(true);
  await expect(row).toHaveClass(/is-selected/);
});
