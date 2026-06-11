import { test, expect, type Page, type Response } from '@playwright/test';
import { controlURL } from './playwright.config';

// Live mode (Unit 27 / D19), made deterministic by the fakeapi control
// surface (watch-script with delayMs, watch-401, the openWatches snapshot):
//
//   - enabling Live opens the `_stream` SSE fetch (a client-minted ?g=
//     generation on the URL), pulses the topbar livedot, and a scripted pod
//     status change lands as a PUSH -- well under any polling cadence -- and
//     flashes exactly the changed cell through the shared morph pipeline;
//   - the choice persists via the ro_prefs cookie (SSR renders Live + the
//     stream reopens on reload);
//   - a filter-chip commit while a delayMs-held push from the OLD stream is
//     in flight: the stale unfiltered push is DISCARDED at morph time, the
//     stream reopens with a FRESH generation carrying the f= query, and the
//     reopened stream keeps pushing (filter coherence, the review-named
//     failure mode);
//   - `/__control/watch-401` + a scripted EOF make readout's re-watch hit a
//     401 -> the server emits `ro-terminal` (reason auth) -> the stale banner
//     appears, 5s polling resumes, and a later tab-hide/show does NOT reopen
//     the stream (the close-reason taxonomy: terminal fallback is sticky);
//   - document.hidden closes a riding stream (the server-side watch tears
//     down) and visibility return reopens it -- the ONLY visibility-return
//     reopen the taxonomy allows;
//   - a push into the 600-row windowed fixture keeps the window: no
//     duplicate rows, two spacers, stable mid-list scroll, the changed cell
//     flashes (the windowed-push review hole);
//   - multi-type and multi-cluster pages render the Live option DISABLED
//     with the scope title; the single-type page keeps it enabled.

const PODS = '/clusters/e2e/namespaces/default/pods';
const PODS_LIST_PATH = '/api/v1/namespaces/default/pods';
const BIG_PODS = '/clusters/e2e/namespaces/big/pods';
const BIG_PODS_LIST_PATH = '/api/v1/namespaces/big/pods';

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

// The fakeapi hub snapshot names every open ?watch=true connection -- the
// server-side truth about whether a Live stream is riding (each open stream
// holds exactly one upstream watch; nothing else in readout watches).
async function openWatchCount(): Promise<number> {
  const res = await fetch(`${controlURL}/__control/watch-script`);
  if (!res.ok) {
    throw new Error(`watch snapshot: ${res.status}`);
  }
  const body = (await res.json()) as { openWatches?: string[] };
  return (body.openWatches ?? []).length;
}

// A polling tick (or any programmatic re-fetch) marks itself RO-No-Push (the
// refresh.spec.ts pattern) -- how the 5s fallback polling is awaited.
function isTickResponse(r: Response): boolean {
  return r.url().includes('/_table') && r.request().headers()['ro-no-push'] === 'true';
}

function isStreamRequest(url: string): boolean {
  return url.includes('/_stream');
}

function streamGen(url: string): string {
  return new URL(url).searchParams.get('g') ?? '';
}

function rowNames(page: Page) {
  return page.locator('#resource-list-content table.ro-table tbody td.cell-name');
}

function podRow(page: Page, n: number) {
  return page.locator(`tr[data-key="e2e/big/big-pod-${String(n).padStart(4, '0')}"]`);
}

// The navbar dropdown opens on hover; park the cursor afterwards so the open
// menu never intercepts later clicks (the refresh.spec.ts pattern).
async function pickLive(page: Page): Promise<void> {
  await page.locator('#refresh-dropdown').hover();
  await page.locator('.refresh-option[data-interval="Live"]').click();
  await page.mouse.move(200, 400);
}

// Simulate tab visibility for the document.hidden taxonomy: readout.js reads
// document.hidden and listens for visibilitychange, both overridable.
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
    'the Live chrome (refresh dropdown, chips editor, windowing) is a desktop surface (below 760px the card layer replaces the table, D22)'
  );
  await control('/__control/reset');
});

test('Live opens the stream: livedot pulses, a status change lands as a push and flashes, the pick persists', async ({
  page,
}) => {
  await page.goto(PODS);
  await expect(rowNames(page)).toHaveText(['nginx', 'my-app']);

  const streamReq = page.waitForRequest((r) => isStreamRequest(r.url()), { timeout: 10_000 });
  await pickLive(page);
  const gen = streamGen((await streamReq).url());
  expect(gen).not.toBe(''); // the client minted a generation onto the URL
  await expect(page.locator('#refresh-label')).toHaveText('Live');
  await expect(page.locator('.refresh-option[data-interval="Live"]')).toHaveClass(/is-active/);
  await expect(page.locator('#refresh-dropdown .ro-livedot')).toHaveCSS(
    'animation-name',
    'ro-pulse'
  );
  // The server-side watch is riding (fakeapi hub truth).
  await expect.poll(openWatchCount, { timeout: 5_000 }).toBeGreaterThan(0);

  // A scripted change arrives as a PUSH: Live arms NO polling timer, so the
  // morph landing at all (let alone this fast) proves the stream delivered
  // it. The changed STATUS cell flashes; the untouched NAME cell does not.
  await scriptEvents([
    {
      path: PODS_LIST_PATH,
      type: 'MODIFIED',
      object: {
        apiVersion: 'v1',
        kind: 'Pod',
        metadata: { name: 'nginx', namespace: 'default', uid: '00000000-0000-0000-0000-000000000001' },
        status: { phase: 'Running' },
      },
      cells: ['nginx', '0/1', 'CrashLoopBackOff', '3', '10m'],
    },
  ]);
  const nginx = page.locator('tr[data-key="e2e/default/nginx"]');
  await expect(nginx.locator('td:has(span.cell-status)')).toContainText('CrashLoopBackOff', {
    timeout: 2_000,
  });
  await expect(nginx.locator('td:has(span.cell-status)')).toHaveClass(/ro-cell-changed/);
  await expect(nginx.locator('td.cell-name')).not.toHaveClass(/ro-cell-changed/);

  // Persistence (D9): a reload renders Live at SSR from the ro_prefs cookie
  // and the stream reopens by itself (a fresh page init is a fresh attempt).
  const reopened = page.waitForRequest((r) => isStreamRequest(r.url()), { timeout: 10_000 });
  await page.reload();
  await expect(page.locator('#refresh-label')).toHaveText('Live');
  await expect(page.locator('.refresh-option[data-interval="Live"]')).toHaveClass(/is-active/);
  await reopened;
});

test('a filter chip while an old-generation push is held in flight: morph-time discard + fresh-generation reopen', async ({
  page,
}) => {
  await page.goto(PODS);
  const firstStream = page.waitForRequest((r) => isStreamRequest(r.url()), { timeout: 10_000 });
  await pickLive(page);
  const gen1 = streamGen((await firstStream).url());
  await expect.poll(openWatchCount, { timeout: 5_000 }).toBeGreaterThan(0);

  // Hold the upcoming user `_table` commit open for 1.5s -- the in-flight
  // window the held push must land inside (deterministic, no timing race).
  await page.route('**/_table*', async (route) => {
    await new Promise((resolve) => setTimeout(resolve, 1_500));
    await route.continue();
  });
  // Hold the my-app mutation 600ms: fakeapi delays the list-state change AND
  // the stream emission, so the OLD stream pushes an UNFILTERED fragment
  // (my-app CrashLooping) squarely inside the commit's in-flight window.
  await scriptEvents([
    {
      path: PODS_LIST_PATH,
      type: 'MODIFIED',
      delayMs: 600,
      object: {
        apiVersion: 'v1',
        kind: 'Pod',
        metadata: { name: 'my-app', namespace: 'default', uid: '00000000-0000-0000-0000-000000000002' },
        status: { phase: 'Running' },
      },
      cells: ['my-app', '0/1', 'CrashLoopBackOff', '7', '5m'],
    },
  ]);

  const secondStream = page.waitForRequest(
    (r) => isStreamRequest(r.url()) && streamGen(r.url()) !== gen1,
    { timeout: 10_000 }
  );
  // Commit a status:Running chip: my-app (now CrashLooping server-side once
  // the delay fires) is filtered OUT of every subsequent server render.
  await page.locator('#ro-filter-input').click();
  await page.locator('#ro-filter-input').pressSequentially('status:Running');
  await page.locator('#ro-filter-input').press('Enter');

  // t ~= 1.1s: the held push has been delivered and DISCARDED (the commit is
  // still in flight) -- my-app shows its pre-change status, the unfiltered
  // stale fragment never morphed in.
  await page.waitForTimeout(1_100);
  await expect(
    page.locator('tr[data-key="e2e/default/my-app"] td:has(span.cell-status)')
  ).toContainText('Running');

  // The commit lands: the filtered view holds nginx only, and the stream
  // reopened under a FRESH generation carrying the f= query.
  const req2 = await secondStream;
  expect(req2.url()).toContain('f=status');
  await expect(rowNames(page)).toHaveText(['nginx']);
  await page.unroute('**/_table*');

  // The reopened stream is functional: a further nginx change (still
  // Running, so it stays in the filtered view) pushes through.
  await scriptEvents([
    {
      path: PODS_LIST_PATH,
      type: 'MODIFIED',
      object: {
        apiVersion: 'v1',
        kind: 'Pod',
        metadata: { name: 'nginx', namespace: 'default', uid: '00000000-0000-0000-0000-000000000001' },
        status: { phase: 'Running' },
      },
      cells: ['nginx', '1/1', 'Running', '4', '10m'],
    },
  ]);
  const nginx = page.locator('tr[data-key="e2e/default/nginx"]');
  await expect(nginx.locator('td').nth(3)).toContainText('4', { timeout: 3_000 });
});

test('watch-401 terminal: banner appears, 5s polling resumes, tab-hide/show never reopens', async ({
  page,
}) => {
  await page.goto(PODS);
  const stream = page.waitForRequest((r) => isStreamRequest(r.url()), { timeout: 10_000 });
  await pickLive(page);
  await stream;
  await expect.poll(openWatchCount, { timeout: 5_000 }).toBeGreaterThan(0);

  // Arm the one-shot 401, then cleanly EOF the riding watch: readout
  // re-watches, hits the 401, and emits `ro-terminal` reason "auth".
  await control('/__control/watch-401');
  await scriptEvents([{ path: PODS_LIST_PATH, type: 'EOF' }]);

  // The taxonomy: terminal -> the stale banner reveals (the shared
  // markListStale machinery) and 5s polling resumes.
  await expect(page.locator('.ro-stale-banner')).toBeVisible({ timeout: 10_000 });
  const tick = await page.waitForResponse(isTickResponse, { timeout: 15_000 });
  expect(tick.ok()).toBe(true);
  // The first successful poll clears the degradation banner -- the standard
  // stale recovery; the data is fresh again (5s-polling fresh).
  await expect(page.locator('.ro-stale-banner')).toBeHidden();

  // A subsequent tab-hide/show must NOT reopen the stream: the terminal
  // fallback is sticky (only a fresh page init or an explicit re-pick
  // re-attempts). The catch-up _table poll on return is fine -- only
  // /_stream is forbidden.
  const streamRequests: string[] = [];
  page.on('request', (r) => {
    if (isStreamRequest(r.url())) {
      streamRequests.push(r.url());
    }
  });
  await setHidden(page, true);
  await setHidden(page, false);
  await page.waitForTimeout(800);
  expect(streamRequests).toEqual([]);

  // Live stays the selected mode and the livedot keeps pulsing: the fallback
  // is an ACTIVE polling mode (the Unit 21 rule).
  await expect(page.locator('#refresh-label')).toHaveText('Live');
  await expect(page.locator('#refresh-dropdown .ro-livedot')).toHaveCSS(
    'animation-name',
    'ro-pulse'
  );
});

test('document.hidden closes the stream; visibility return reopens it (hidden-close only)', async ({
  page,
}) => {
  await page.goto(PODS);
  const stream = page.waitForRequest((r) => isStreamRequest(r.url()), { timeout: 10_000 });
  await pickLive(page);
  const gen1 = streamGen((await stream).url());
  await expect.poll(openWatchCount, { timeout: 5_000 }).toBeGreaterThan(0);

  // Hide: the client aborts the stream fetch; the server's upstream watch
  // tears down with the request context (fakeapi hub truth).
  await setHidden(page, true);
  await expect.poll(openWatchCount, { timeout: 5_000 }).toBe(0);

  // Show: reopen under a FRESH generation -- the one visibility-return
  // reopen the taxonomy allows.
  const reopened = page.waitForRequest(
    (r) => isStreamRequest(r.url()) && streamGen(r.url()) !== gen1,
    { timeout: 10_000 }
  );
  await setHidden(page, false);
  await reopened;
  await expect.poll(openWatchCount, { timeout: 5_000 }).toBeGreaterThan(0);
});

test('a Live push while windowed keeps the window: no duplicates, stable scroll, honest flash', async ({
  page,
}) => {
  await page.goto(BIG_PODS);
  await expect(podRow(page, 1)).toBeVisible();
  const stream = page.waitForRequest((r) => isStreamRequest(r.url()), { timeout: 10_000 });
  await pickLive(page);
  await stream;
  await expect.poll(openWatchCount, { timeout: 5_000 }).toBeGreaterThan(0);

  // Push a change to an IN-WINDOW row (top of the list): the adopted cell
  // updates and flashes through the virtualizer's identity diff.
  await scriptEvents([
    {
      path: BIG_PODS_LIST_PATH,
      type: 'MODIFIED',
      object: {
        apiVersion: 'v1',
        kind: 'Pod',
        metadata: { name: 'big-pod-0002', namespace: 'big' },
      },
      cells: ['big-pod-0002', '0/1', 'Error', '5', '10m'],
    },
  ]);
  await expect(podRow(page, 2).locator('td').nth(2)).toContainText('Error', { timeout: 3_000 });
  await expect(podRow(page, 2).locator('td').nth(2)).toHaveClass(/ro-cell-changed/);
  await expect(podRow(page, 2).locator('td.cell-name')).not.toHaveClass(/ro-cell-changed/);

  // The window survived the push: far fewer than 600 rows, NO duplicate
  // keys, both spacers, the true total -- 600 rows never rode the morph.
  const keys = await page.evaluate(() =>
    Array.from(
      document.querySelectorAll('#resource-list-content tbody tr[data-key]'),
      (tr) => tr.getAttribute('data-key')
    )
  );
  expect(keys.length).toBeGreaterThan(10);
  expect(keys.length).toBeLessThan(100);
  expect(new Set(keys).size).toBe(keys.length);
  await expect(page.locator('tr.ro-vspacer')).toHaveCount(2);
  await expect(page.locator('.ro-foundline')).toContainText('Found 600 rows');

  // Park mid-list and push a change into the CURRENT window: the scroll
  // position must hold exactly (the adoption pipeline restores it).
  await page.evaluate(() => window.scrollTo(0, 4000));
  await expect
    .poll(() => page.evaluate(() => window.scrollY), { timeout: 5_000 })
    .toBeGreaterThan(3500);
  const scrollBefore = await page.evaluate(() => window.scrollY);
  // The re-window rides a rAF-throttled scroll listener: wait until the
  // rendered slice actually moved before reading it, or the bounds still
  // describe the top-of-list window.
  await expect
    .poll(
      () =>
        page.evaluate(
          () =>
            (window as unknown as { roVirtual: { renderedBounds(): { start: number } } })
              .roVirtual.renderedBounds().start
        ),
      { timeout: 5_000 }
    )
    .toBeGreaterThan(50);
  const bounds = await page.evaluate(
    () =>
      (window as unknown as { roVirtual: { renderedBounds(): { start: number; end: number } } })
        .roVirtual.renderedBounds()
  );
  const inWindowPod = bounds.start + 6; // visible list index i is big-pod-(i+1)
  await scriptEvents([
    {
      path: BIG_PODS_LIST_PATH,
      type: 'MODIFIED',
      object: {
        apiVersion: 'v1',
        kind: 'Pod',
        metadata: { name: `big-pod-${String(inWindowPod + 1).padStart(4, '0')}`, namespace: 'big' },
      },
      cells: [
        `big-pod-${String(inWindowPod + 1).padStart(4, '0')}`,
        '0/1',
        'CrashLoopBackOff',
        '2',
        '10m',
      ],
    },
  ]);
  await expect(podRow(page, inWindowPod + 1).locator('td').nth(2)).toContainText(
    'CrashLoopBackOff',
    { timeout: 3_000 }
  );
  expect(await page.evaluate(() => window.scrollY)).toBe(scrollBefore);
  await expect(page.locator('tr.ro-vspacer')).toHaveCount(2);

  // The spacer offsets stayed exact: the last row is still reachable.
  await page.evaluate(() => window.scrollTo(0, document.documentElement.scrollHeight));
  await expect(podRow(page, 600)).toBeVisible();
});

test('the Live option is disabled on multi-type and multi-cluster pages, enabled on single-type', async ({
  page,
}) => {
  const live = page.locator('.refresh-option[data-interval="Live"]');

  // Multi-type page (plural "all"): disabled with the scope title.
  await page.goto('/clusters/e2e/namespaces/default/all');
  await expect(live).toBeDisabled();
  await expect(live).toHaveAttribute('title', /single-type, single-cluster/);

  // Multi-cluster page (_all union): disabled.
  await page.goto('/clusters/_all/pods');
  await expect(live).toBeDisabled();

  // Single-type, single-cluster list: enabled.
  await page.goto(PODS);
  await expect(live).toBeEnabled();
});
