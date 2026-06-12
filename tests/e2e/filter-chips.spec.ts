import { test, expect, type Page, type Response } from '@playwright/test';
import { controlURL } from './playwright.config';

// Filters v2 chips editor, end to end against the fakeapi harness:
//
//   - free text live-matches the NAME column entirely client-side: rows narrow
//     with NO network request (request-count assertion) until an operator chip
//     commits;
//   - ⏎ on `field:value` / `field>value` materializes a server-rendered chip,
//     fires the `_table` partial through the v2 loop, and the pushed URL is the
//     CANONICAL list URL carrying the encoded `?f=` chip;
//   - ⌫ on an empty input pops the last chip (riding its server-built removal
//     href) and restores the rows;
//   - an unknown field on ⏎ shows the inline hint and creates NO chip;
//   - clicking a label chip in a namespaces row appends the corresponding
//     `label:key=value` chip and narrows the rows;
//   - a focused draft AND its focus survive a refresh-tick morph (the
//     ignoreActiveValue contract, asserted where the chips editor lives).
//
// Fixture state is scripted through the control surface and reset per spec.

const PODS = '/clusters/e2e/namespaces/default/pods';
const PODS_LIST_PATH = '/api/v1/namespaces/default/pods';
const NAMESPACES = '/clusters/e2e/namespaces';

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

// addPod scripts one extra pod row with explicit Table cells
// ([Name, Ready, Status, Restarts, Age] -- the pods fixture schema).
async function addPod(name: string, cells: string[]): Promise<void> {
  await scriptEvents([
    { path: PODS_LIST_PATH, type: 'ADDED', object: podObject(name, '2026-01-01T00:00:00Z'), cells },
  ]);
}

function isUserTableResponse(r: Response): boolean {
  const headers = r.request().headers();
  return (
    r.url().includes('/_table') &&
    headers['ro-no-push'] !== 'true' &&
    headers['hx-preloaded'] !== 'true'
  );
}

function isTickResponse(r: Response): boolean {
  return r.url().includes('/_table') && r.request().headers()['ro-no-push'] === 'true';
}

function waitForTick(page: Page): Promise<Response> {
  return page.waitForResponse(isTickResponse, { timeout: 15_000 });
}

// The navbar interval menu opens on hover (CSS :hover/:focus-within).
async function pickInterval(page: Page, secs: number): Promise<void> {
  await page.locator('#refresh-dropdown').hover();
  await page.locator(`.refresh-option[data-ro-interval="${secs}"]`).click();
}

const filterInput = (page: Page) => page.locator('#ro-filter-input');
const editorChips = (page: Page) => page.locator('#ro-filter-field .ro-scope-chip');

// The names of the rows the live/server filters left VISIBLE (the live name
// match hides rows with the ro-row-filtered class; server filtering removes
// them from the fragment entirely).
function visibleNames(page: Page) {
  return page.locator(
    '#resource-list-content table.ro-table tbody tr[data-key]:not(.ro-row-filtered) td.cell-name'
  );
}

// Commit the typed draft with ⏎ and await the resulting USER `_table` swap.
async function commitDraft(page: Page): Promise<void> {
  const swapped = page.waitForResponse(isUserTableResponse);
  await filterInput(page).press('Enter');
  await swapped;
}

test.beforeEach(async ({}, testInfo) => {
  test.skip(
    testInfo.project.name !== 'desktop',
    'the chips editor is a desktop surface (below 760px the card layer replaces the table)'
  );
  await control('/__control/reset');
});

test('free text narrows rows live with no network request', async ({ page }) => {
  await addPod('api-server', ['api-server', '1/1', 'Running', '0', '1m']);
  await page.goto(PODS);
  await expect(visibleNames(page)).toHaveText(['nginx', 'my-app', 'api-server']);

  // Count every request from here on (preload warm-ups excluded -- hovering
  // chrome may legitimately warm a link; typing must trigger NOTHING).
  const requests: string[] = [];
  page.on('request', (r) => {
    if ((r.headers()['hx-preloaded'] ?? '') !== 'true') {
      requests.push(r.url());
    }
  });

  await filterInput(page).click();
  await filterInput(page).pressSequentially('api');

  // The live name match applied: only the matching row stays visible, the
  // others are class-hidden (still in the DOM -- no server round-trip).
  await expect(visibleNames(page)).toHaveText(['api-server']);
  await expect(
    page.locator('tr[data-key="e2e/default/nginx"]')
  ).toHaveClass(/ro-row-filtered/);

  // And NOT ONE request was made: the matcher ran on the client row model.
  // The negative window must outlast the canonical htmx active-search debounce
  // (500ms) -- a shorter settle would pass even if typing armed a debounced
  // request that had not fired yet.
  await page.waitForTimeout(750);
  expect(requests).toEqual([]);

  // Clearing the draft restores every row -- still without a request (same
  // post-debounce settle before the recheck, for the same reason).
  await filterInput(page).fill('');
  await expect(visibleNames(page)).toHaveText(['nginx', 'my-app', 'api-server']);
  await page.waitForTimeout(750);
  expect(requests).toEqual([]);
});

test('status:Running ⏎ materializes a chip, pushes the canonical f= URL, filters rows', async ({
  page,
}) => {
  await addPod('crashed', ['crashed', '0/1', 'CrashLoopBackOff', '3', '2m']);
  await page.goto(PODS);
  await expect(visibleNames(page)).toHaveText(['nginx', 'my-app', 'crashed']);

  await filterInput(page).click();
  await filterInput(page).pressSequentially('status:Running');
  await commitDraft(page);

  // The chip rendered SERVER-side inside the editor field, split field/op/value.
  await expect(editorChips(page)).toHaveCount(1);
  await expect(editorChips(page).first()).toContainText('status');
  await expect(editorChips(page).first()).toContainText('Running');
  // The pushed URL is the canonical list URL carrying the encoded chip.
  await expect(page).toHaveURL(/f=status%3ARunning/);
  expect(new URL(page.url()).pathname).not.toContain('_table');
  // Rows filtered server-side; the input cleared for the next chip.
  await expect(visibleNames(page)).toHaveText(['nginx', 'my-app']);
  await expect(filterInput(page)).toHaveValue('');

  // A hard reload of the shared URL lands with the chip visible (server-rendered).
  await page.reload();
  await expect(editorChips(page)).toHaveCount(1);
  await expect(visibleNames(page)).toHaveText(['nginx', 'my-app']);
});

test('restarts>0 ⏎ commits an operator chip', async ({ page }) => {
  await addPod('crashed', ['crashed', '0/1', 'CrashLoopBackOff', '3', '2m']);
  await page.goto(PODS);

  await filterInput(page).click();
  await filterInput(page).pressSequentially('restarts>0');
  await commitDraft(page);

  await expect(page).toHaveURL(/f=restarts%3E0/);
  await expect(editorChips(page)).toHaveCount(1);
  await expect(editorChips(page).first()).toContainText('restarts');
  await expect(visibleNames(page)).toHaveText(['crashed']);
});

test('a second chip ANDs with the first; ⌫ pops only the last chip', async ({ page }) => {
  await addPod('crashed', ['crashed', '0/1', 'CrashLoopBackOff', '3', '2m']);
  await addPod('restarted', ['restarted', '1/1', 'Running', '2', '5m']);
  await page.goto(PODS);
  await expect(visibleNames(page)).toHaveText(['nginx', 'my-app', 'crashed', 'restarted']);

  await filterInput(page).click();
  await filterInput(page).pressSequentially('status:Running');
  await commitDraft(page);
  await expect(visibleNames(page)).toHaveText(['nginx', 'my-app', 'restarted']);

  // The second chip AND-combines: only the row matching BOTH
  // survives, and the canonical URL carries both repeatable f= params.
  await filterInput(page).click();
  await filterInput(page).pressSequentially('restarts>0');
  await commitDraft(page);
  await expect(editorChips(page)).toHaveCount(2);
  await expect(visibleNames(page)).toHaveText(['restarted']);
  await expect(page).toHaveURL(/f=status%3ARunning/);
  await expect(page).toHaveURL(/f=restarts%3E0/);

  // ⌫ pops only the LAST chip: the first keeps filtering.
  await filterInput(page).click();
  const swapped = page.waitForResponse(isUserTableResponse);
  await filterInput(page).press('Backspace');
  await swapped;
  await expect(editorChips(page)).toHaveCount(1);
  await expect(editorChips(page).first()).toContainText('status');
  await expect(visibleNames(page)).toHaveText(['nginx', 'my-app', 'restarted']);
  await expect(page).toHaveURL(/f=status%3ARunning/);
  await expect(page).not.toHaveURL(/f=restarts%3E0/);
});

test('⌫ on empty input pops the chip and restores the rows', async ({ page }) => {
  await addPod('crashed', ['crashed', '0/1', 'CrashLoopBackOff', '3', '2m']);
  await page.goto(`${PODS}?f=status%3ARunning`);
  await expect(editorChips(page)).toHaveCount(1);
  await expect(visibleNames(page)).toHaveText(['nginx', 'my-app']);

  await filterInput(page).click();
  await expect(filterInput(page)).toHaveValue('');
  const swapped = page.waitForResponse(isUserTableResponse);
  await filterInput(page).press('Backspace');
  await swapped;

  await expect(editorChips(page)).toHaveCount(0);
  await expect(visibleNames(page)).toHaveText(['nginx', 'my-app', 'crashed']);
  await expect(page).not.toHaveURL(/f=/);
});

test('unknown field on ⏎ shows the inline hint and creates no chip', async ({ page }) => {
  await page.goto(PODS);

  const requests: string[] = [];
  page.on('request', (r) => {
    if (r.url().includes('/_table')) {
      requests.push(r.url());
    }
  });

  await filterInput(page).click();
  await filterInput(page).pressSequentially('bogus:x');
  await filterInput(page).press('Enter');

  const hint = page.locator('#ro-filter-error');
  await expect(hint).toBeVisible();
  await expect(hint).toContainText('no such field — try');
  await expect(editorChips(page)).toHaveCount(0);
  // No chip request fired and the draft stays editable.
  expect(requests).toEqual([]);
  await expect(filterInput(page)).toHaveValue('bogus:x');
  await expect(page).not.toHaveURL(/f=/);

  // The next keystroke clears the hint.
  await filterInput(page).press('Backspace');
  await expect(hint).toBeHidden();
});

test('clicking a label chip in a namespaces row appends the label chip and narrows rows', async ({
  page,
}) => {
  await page.goto(NAMESPACES);
  await expect(visibleNames(page)).toHaveText(['default', 'kube-system', 'my-app']);

  // The default namespace fixture carries team=core; its chip is a
  // click-to-filter anchor built server-side.
  await page.locator('tr[data-key="e2e/default"] a.ro-chip', { hasText: 'team' }).click();

  await expect(page).toHaveURL(/f=label%3Ateam%3Dcore/);
  await expect(editorChips(page)).toHaveCount(1);
  await expect(editorChips(page).first()).toContainText('label');
  await expect(editorChips(page).first()).toContainText('team=core');
  await expect(visibleNames(page)).toHaveText(['default']);
});

test('a focused draft and its focus survive a refresh-tick morph', async ({ page }) => {
  await page.goto(PODS);
  await pickInterval(page, 5);

  await filterInput(page).click();
  await filterInput(page).pressSequentially('ngi');
  await expect(visibleNames(page)).toHaveText(['nginx']);

  // Change cluster state and let one tick morph the fragment under the draft.
  await addPod('omega', ['omega', '1/1', 'Running', '0', '1y']);
  await waitForTick(page);
  // The morph applied: the new row is in the DOM (hidden by the live filter).
  await expect(page.locator('tr[data-key="e2e/default/omega"]')).toBeAttached();

  // The ignoreActiveValue contract where the editor lives: the draft text AND
  // the focus survived the morph, and the live narrowing was re-applied.
  await expect(filterInput(page)).toHaveValue('ngi');
  await expect(filterInput(page)).toBeFocused();
  await expect(visibleNames(page)).toHaveText(['nginx']);
});
