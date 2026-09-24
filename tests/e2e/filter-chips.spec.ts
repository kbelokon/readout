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
//   - a focused draft AND its focus survive a Refresh morph (the
//     ignoreActiveValue contract, asserted where the chips editor lives);
//   - an UNFOCUSED draft survives every full re-render too: a Refresh click, a
//     Live reconnect after going offline, and a Live reopen on returning to the
//     tab;
//   - the draft rides the page URL as `q` (replaceState: no request, no history
//     entry), so it survives a reload, history navigation and a sort push;
//   - ⏎ on plain text pins it as a `name:` chip where the table has a Name
//     column, and keeps it as live text where it has none (Events).
//
// Fixture state is scripted through the control surface and reset per spec.

const PODS = '/clusters/e2e/namespaces/default/pods';
const PODS_LIST_PATH = '/api/v1/namespaces/default/pods';
const NAMESPACES = '/clusters/e2e/namespaces';
const EVENTS = '/clusters/e2e/namespaces/default/events';
const LIVE_TOGGLE = '[data-ro-action="toggle-live"]';
const REFRESH_NOW = '[data-ro-action="refresh-now"]';

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
  return r.url().includes('/_table') && headers['ro-no-push'] !== 'true';
}

function isTickResponse(r: Response): boolean {
  return r.url().includes('/_table') && r.request().headers()['ro-no-push'] === 'true';
}

function waitForTick(page: Page): Promise<Response> {
  return page.waitForResponse(isTickResponse, { timeout: 15_000 });
}

// requestListRefresh is the production container-owned re-fetch -- exactly what
// the topbar's Refresh button calls. It is driven through the window seam here
// rather than by clicking the button, because the test below asserts that the
// filter draft keeps DOM FOCUS across the morph, and a button click would move
// that focus itself.
async function refreshList(page: Page): Promise<Response> {
  const tick = waitForTick(page);
  await page.evaluate(() =>
    (window as unknown as { requestListRefresh(): void }).requestListRefresh()
  );
  return tick;
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

// typeDraft types free text into the editor and waits for the live match.
async function typeDraft(page: Page, text: string): Promise<void> {
  await filterInput(page).click();
  await filterInput(page).pressSequentially(text);
}

// The `q` param of the page URL, decoded; null when absent.
function urlDraft(page: Page): string | null {
  return new URL(page.url()).searchParams.get('q');
}

interface LiveStats {
  state: string;
  connections: number;
  v2Snapshots: number;
}

function liveStats(page: Page): Promise<LiveStats> {
  return page.evaluate(() => {
    const stats = (window as unknown as { roLive: { stats(): LiveStats } }).roLive.stats();
    return { state: stats.state, connections: stats.connections, v2Snapshots: stats.v2Snapshots };
  });
}

// enableLive turns the topbar toggle on and waits for the first committed
// snapshot: `open` is the only state whose rows came off the stream.
async function enableLive(page: Page): Promise<void> {
  await page.locator(LIVE_TOGGLE).click();
  await expect(page.locator(LIVE_TOGGLE)).toHaveAttribute('aria-pressed', 'true');
  await expect.poll(async () => (await liveStats(page)).state, { timeout: 10_000 }).toBe('open');
}

// Simulated tab visibility: live.ts reads document.hidden and listens for
// visibilitychange, both overridable.
async function setHidden(page: Page, hidden: boolean): Promise<void> {
  await page.evaluate((h) => {
    Object.defineProperty(document, 'hidden', { configurable: true, get: () => h });
    Object.defineProperty(document, 'visibilityState', {
      configurable: true,
      get: () => (h ? 'hidden' : 'visible'),
    });
    document.dispatchEvent(new Event('visibilitychange'));
  }, hidden);
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

  // Count every request from here on: typing must trigger NOTHING.
  const requests: string[] = [];
  page.on('request', (r) => {
    requests.push(r.url());
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

test('a focused draft and its focus survive a Refresh morph', async ({ page }) => {
  await page.goto(PODS);

  await filterInput(page).click();
  await filterInput(page).pressSequentially('ngi');
  await expect(visibleNames(page)).toHaveText(['nginx']);

  // Change cluster state and let one refresh morph the fragment under the draft.
  await addPod('omega', ['omega', '1/1', 'Running', '0', '1y']);
  await refreshList(page);
  // The morph applied: the new row is in the DOM (hidden by the live filter).
  await expect(page.locator('tr[data-key="e2e/default/omega"]')).toBeAttached();

  // The ignoreActiveValue contract where the editor lives: the draft text AND
  // the focus survived the morph, and the live narrowing was re-applied.
  await expect(filterInput(page)).toHaveValue('ngi');
  await expect(filterInput(page)).toBeFocused();
  await expect(visibleNames(page)).toHaveText(['nginx']);
});

test('an unfocused draft survives a Refresh click', async ({ page }) => {
  await page.goto(PODS);
  await typeDraft(page, 'ngi');
  await expect(visibleNames(page)).toHaveText(['nginx']);

  // The click itself takes the focus off the input -- the reported case.
  await addPod('omega', ['omega', '1/1', 'Running', '0', '1y']);
  const tick = waitForTick(page);
  await page.locator(REFRESH_NOW).click();
  await tick;
  await expect(page.locator('tr[data-key="e2e/default/omega"]')).toBeAttached();

  await expect(filterInput(page)).not.toBeFocused();
  await expect(filterInput(page)).toHaveValue('ngi');
  await expect(visibleNames(page)).toHaveText(['nginx']);
});

test('an unfocused draft survives a Live reconnect after going offline', async ({ page }) => {
  await page.goto(PODS);
  await enableLive(page);
  await typeDraft(page, 'ngi');
  await filterInput(page).blur();
  const before = await liveStats(page);

  await page.context().setOffline(true);
  await expect.poll(async () => (await liveStats(page)).state, { timeout: 10_000 }).toBe('offline');
  await page.context().setOffline(false);
  // The reconnect commits a fresh full snapshot: the whole list morphs.
  await expect
    .poll(async () => (await liveStats(page)).v2Snapshots, { timeout: 10_000 })
    .toBeGreaterThan(before.v2Snapshots);

  await expect(filterInput(page)).toHaveValue('ngi');
  await expect(visibleNames(page)).toHaveText(['nginx']);
});

test('an unfocused draft survives Live reopening when the tab returns', async ({ page }) => {
  await page.goto(PODS);
  await enableLive(page);
  await typeDraft(page, 'ngi');
  await filterInput(page).blur();
  const before = await liveStats(page);

  await setHidden(page, true);
  await expect.poll(async () => (await liveStats(page)).state, { timeout: 10_000 }).toBe('hidden');
  await setHidden(page, false);
  await expect
    .poll(async () => (await liveStats(page)).v2Snapshots, { timeout: 10_000 })
    .toBeGreaterThan(before.v2Snapshots);

  await expect(filterInput(page)).toHaveValue('ngi');
  await expect(visibleNames(page)).toHaveText(['nginx']);
});

test('the draft rides the URL as q and survives a reload', async ({ page }) => {
  await page.goto(PODS);
  await typeDraft(page, 'ngi');
  await expect.poll(() => urlDraft(page)).toBe('ngi');

  await page.reload();
  await expect(filterInput(page)).toHaveValue('ngi');
  await expect(visibleNames(page)).toHaveText(['nginx']);

  // A chip in progress narrows nothing, so it never reaches the URL.
  await filterInput(page).fill('status:Run');
  await expect.poll(() => urlDraft(page)).toBeNull();
  await expect(visibleNames(page)).toHaveText(['nginx', 'my-app']);
});

test('the draft follows history across a sort push and a detail page', async ({ page }) => {
  await page.goto(PODS);
  await typeDraft(page, 'ngi');
  await expect.poll(() => urlDraft(page)).toBe('ngi');

  // The sort header was rendered before the draft existed, so its link carries
  // no q -- the pushed URL must still carry the field's text.
  const sorted = page.waitForResponse(isUserTableResponse);
  await page.locator('table.ro-table thead th a', { hasText: 'Status' }).click();
  await sorted;
  await expect(page).toHaveURL(/[?&]sort=/);
  expect(urlDraft(page)).toBe('ngi');
  await expect(filterInput(page)).toHaveValue('ngi');
  await expect(visibleNames(page)).toHaveText(['nginx']);

  await page.locator('tr[data-key="e2e/default/nginx"] td.cell-name a').click();
  await expect(page).toHaveURL(/\/pods\/nginx$/);
  await page.goBack();
  await expect(page).toHaveURL(/[?&]sort=/);
  await expect(filterInput(page)).toHaveValue('ngi');
  await expect(visibleNames(page)).toHaveText(['nginx']);

  // One more step back is the entry from before the sort push.
  await page.goBack();
  await expect(page).not.toHaveURL(/[?&]sort=/);
  await expect(filterInput(page)).toHaveValue('ngi');
  await expect(visibleNames(page)).toHaveText(['nginx']);

  await page.goForward();
  await expect(page).toHaveURL(/[?&]sort=/);
  await expect(filterInput(page)).toHaveValue('ngi');
  await expect(visibleNames(page)).toHaveText(['nginx']);
});

test('⏎ on plain text pins it as a name: chip with its commas kept literal', async ({ page }) => {
  await addPod('api-server', ['api-server', '1/1', 'Running', '0', '1m']);
  await page.goto(PODS);
  await typeDraft(page, 'api');
  await expect(visibleNames(page)).toHaveText(['api-server']);
  await commitDraft(page);

  await expect(editorChips(page)).toHaveCount(1);
  await expect(editorChips(page).first()).toContainText('name');
  await expect(editorChips(page).first()).toContainText('api');
  await expect(page).toHaveURL(/[?&]f=name%3Aapi(?:&|$)/);
  // The draft became the chip; it does not linger as q as well.
  expect(urlDraft(page)).toBeNull();
  await expect(filterInput(page)).toHaveValue('');
  await expect(visibleNames(page)).toHaveText(['api-server']);

  // Free text matches one literal substring, so a comma must not turn into
  // the chip grammar's OR: `nginx,my` matches no name either way.
  await filterInput(page).click();
  await filterInput(page).press('Backspace');
  await expect(editorChips(page)).toHaveCount(0);
  await typeDraft(page, 'nginx,my');
  await expect(visibleNames(page)).toHaveCount(0);
  await commitDraft(page);
  await expect(page).toHaveURL(/[?&]f=name%3Anginx%2Cmy(?:&|$)/);
  await expect(visibleNames(page)).toHaveCount(0);
});

test('⏎ on plain text in a table without Name keeps it as live text', async ({ page }) => {
  await page.goto(EVENTS);
  const requests: string[] = [];
  page.on('request', (r) => {
    if (r.url().includes('/_table')) {
      requests.push(r.url());
    }
  });

  await typeDraft(page, 'nginx');
  await filterInput(page).press('Enter');

  await expect.poll(() => urlDraft(page)).toBe('nginx');
  await expect(filterInput(page)).toHaveValue('nginx');
  await expect(editorChips(page)).toHaveCount(0);
  await page.waitForTimeout(750);
  expect(requests).toEqual([]);
  await expect(page).not.toHaveURL(/[?&]f=/);
});

test('typing adds no history entries, requests or Live reconnects', async ({ page }) => {
  await page.goto(PODS);
  await enableLive(page);
  const before = await page.evaluate(() => ({
    entries: window.history.length,
    cookie: document.cookie,
  }));
  const live = await liveStats(page);
  const requests: string[] = [];
  page.on('request', (r) => {
    requests.push(r.url());
  });

  // Bursts with pauses longer than any reasonable URL-write delay, so every
  // settled draft has had its chance to reach the address bar.
  await filterInput(page).click();
  for (const chunk of ['my', '-a', 'pp']) {
    await filterInput(page).pressSequentially(chunk);
    await page.waitForTimeout(800);
  }
  await filterInput(page).press('Backspace');
  await page.waitForTimeout(800);

  expect(
    await page.evaluate(() => ({ entries: window.history.length, cookie: document.cookie }))
  ).toEqual(before);
  expect(requests).toEqual([]);
  const after = await liveStats(page);
  expect(after.connections).toBe(live.connections);
  expect(after.state).toBe('open');
});
