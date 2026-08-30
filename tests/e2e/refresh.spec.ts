import { test, expect, type Page, type Response } from '@playwright/test';
import { controlURL } from './playwright.config';
import { resetFixture } from './hub';

// The two update controls, end to end. readout has exactly ONE automatic update
// path (the Live SSE stream) and exactly ONE manual one (the Refresh button).
// There is no interval picker, no countdown, no polling timer anywhere -- so
// the load-bearing claims here are as much about what does NOT happen:
//
//   - a fresh profile renders the Live toggle OFF and issues no automatic
//     request at all, however long the page sits there (a fake clock advances
//     an hour and nothing leaves the browser);
//   - ONE toggle click opens exactly one `_stream`; a second click closes it
//     and, again, nothing follows -- neither a stream nor a `_table`;
//   - ONE Refresh click makes exactly one `_table` request and disables the
//     button for the flight, so a double click cannot stack two;
//   - a cookie written by an older build (a numeric polling interval) renders
//     OFF and never polls -- the whole migration is "anything but the literal
//     Live is off";
//   - Refresh is available on pages the Live toggle is not (a multi-cluster
//     union), because a manual re-fetch always applies;
//   - a scripted LIST mutation flashes the changed cell on the next Refresh
//     morph: the non-stream flash net, so severing Live can never sever the
//     only flash coverage;
//   - the topbar livedot pulses brand only while Live is on and is static
//     GHOST-GREY otherwise (per the colour law, brand-green is a live-health
//     signal -- a green dot with no stream would be a false one).

const PODS = '/clusters/e2e/namespaces/default/pods';
const PODS_LIST_PATH = '/api/v1/namespaces/default/pods';
const ALL_CLUSTERS_PODS = '/clusters/_all/namespaces/default/pods';

const LIVE_TOGGLE = '[data-ro-action="toggle-live"]';
const REFRESH_NOW = '[data-ro-action="refresh-now"]';

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

// A Refresh click (or any programmatic re-fetch) marks itself RO-No-Push;
// matching on the request header keeps it awaitable apart from user sorts (the
// list-loop.spec.ts pattern).
function isTickResponse(r: Response): boolean {
  return r.url().includes('/_table') && r.request().headers()['ro-no-push'] === 'true';
}

function rowNames(page: Page) {
  return page.locator('#resource-list-content table.ro-table tbody td.cell-name');
}

// clickRefresh performs the one-shot re-fetch and resolves on its response.
async function clickRefresh(page: Page): Promise<Response> {
  const tick = page.waitForResponse(isTickResponse, { timeout: 15_000 });
  await page.locator(REFRESH_NOW).click();
  return tick;
}

// liveState reads the transport's own state name off the roLive debug seam --
// the client-side truth about whether a stream is held. The server-side watch
// is NOT a proxy for it any more: the pod-local WatchHub retains a source for
// 30s after its last subscriber leaves, so an upstream watch can outlive the
// browser that opened it.
function liveState(page: Page): Promise<string> {
  return page.evaluate(
    () => (window as unknown as { roLive: { stats(): { state: string } } }).roLive.stats().state
  );
}

// updateTraffic records every request that could only come from an update path:
// the list partial and the Live stream. The returned reader is what the
// "nothing happens" assertions read.
function updateTraffic(page: Page): { table: string[]; stream: string[] } {
  const seen = { table: [] as string[], stream: [] as string[] };
  page.on('request', (request) => {
    const path = new URL(request.url()).pathname;
    if (path.endsWith('/_table')) seen.table.push(request.url());
    if (path.endsWith('/_stream')) seen.stream.push(request.url());
  });
  return seen;
}

// The ro_prefs wire format is `v1.<base64url(JSON)>` (internal/web/prefs.go).
// Writing one directly is how a profile from an older build is reproduced.
function prefsCookieValue(payload: object): string {
  return `v1.${Buffer.from(JSON.stringify(payload), 'utf8').toString('base64url')}`;
}

// Resolve a CSS custom property to the computed rgb() serialization toHaveCSS
// compares against (the raw token is a hex literal; themes differ, so the
// expected colour is read from the page itself, never hardcoded).
function resolvedToken(page: Page, token: string): Promise<string> {
  return page.evaluate((t) => {
    const probe = document.createElement('span');
    probe.style.color = `var(${t})`;
    document.body.appendChild(probe);
    const rgb = getComputedStyle(probe).color;
    probe.remove();
    return rgb;
  }, token);
}

test.beforeEach(async ({}, testInfo) => {
  test.skip(
    testInfo.project.name !== 'desktop',
    'the update controls are a desktop surface (below 760px the card layer replaces the table)'
  );
  await resetFixture();
});

test('a fresh profile renders Live off and issues no automatic request for an hour', async ({
  page,
}) => {
  const traffic = updateTraffic(page);
  await page.goto(PODS);
  await expect(rowNames(page)).toHaveText(['nginx', 'my-app']);
  await expect(page.locator(LIVE_TOGGLE)).toHaveAttribute('aria-pressed', 'false');
  await expect(page.locator(REFRESH_NOW)).toBeEnabled();

  // An hour of page-owned time. Every timer this build can arm would have
  // fired many times over; there is no timer to arm, so nothing leaves the
  // browser. (The clock is installed after load so the document's own
  // fetches are already settled.)
  await page.clock.install();
  await page.clock.fastForward(3_600_000);
  await page.waitForTimeout(250);
  expect(traffic).toEqual({ table: [], stream: [] });
  await expect(rowNames(page)).toHaveText(['nginx', 'my-app']);
});

test('one toggle click opens exactly one stream; a second closes it and nothing follows', async ({
  page,
}) => {
  const traffic = updateTraffic(page);
  await page.goto(PODS);
  const toggle = page.locator(LIVE_TOGGLE);

  const opened = page.waitForRequest(
    (r) => new URL(r.url()).pathname.endsWith('/_stream'),
    { timeout: 10_000 }
  );
  await toggle.click();
  const request = await opened;
  // The v2 negotiation headers the server keys the stream on.
  expect(request.headers()['ro-live-version']).toBe('2');
  expect(request.headers()['ro-live-generation']).toBeTruthy();
  await expect(toggle).toHaveAttribute('aria-pressed', 'true');
  // `open` is reached only once a full snapshot has been committed, so this is
  // the causal barrier for "the stream is carrying data", not just "a request
  // left the browser".
  await expect.poll(() => liveState(page), { timeout: 10_000 }).toBe('open');
  expect(traffic.stream).toHaveLength(1);

  // Off: the transport tears down with no request of its own, and the stored
  // preference flips back.
  await toggle.click();
  await expect(toggle).toHaveAttribute('aria-pressed', 'false');
  expect(await liveState(page)).toBe('off');

  // ... and it STAYS torn down. An hour of page time arms nothing.
  await page.clock.install();
  await page.clock.fastForward(3_600_000);
  await page.waitForTimeout(250);
  expect(traffic.stream).toHaveLength(1);
  expect(traffic.table).toEqual([]);
});

test('one Refresh click makes exactly one request and disables the button in flight', async ({
  page,
}) => {
  const traffic = updateTraffic(page);
  await page.goto(PODS);
  const refresh = page.locator(REFRESH_NOW);

  // Hold the request at the browser boundary so the in-flight window is a
  // state to assert against rather than a race to catch.
  let markStarted!: () => void;
  let release!: () => void;
  const started = new Promise<void>((resolve) => {
    markStarted = resolve;
  });
  const released = new Promise<void>((resolve) => {
    release = resolve;
  });
  // Self-retiring rather than `{ times: 1 }`: an exhausted times-limited route
  // is torn down at the instant its last request resolves, and a request the
  // page issues in that same turn can be dropped as interception unwinds.
  let held = false;
  await page.route('**/_table*', async (route) => {
    if (held) {
      await route.fallback();
      return;
    }
    held = true;
    markStarted();
    await released;
    await route.continue();
  });

  const settled = page.waitForResponse(isTickResponse, { timeout: 15_000 });
  await refresh.click();
  await started;
  await expect(refresh).toBeDisabled();

  // A second click while the tracker is occupied cannot stack a request: the
  // button is disabled, and the handler re-checks the tracker anyway.
  await refresh.click({ force: true });
  expect(traffic.table).toHaveLength(1);

  release();
  expect((await settled).status()).toBe(200);
  await expect(refresh).toBeEnabled();
  expect(traffic.table).toHaveLength(1);
  expect(traffic.stream).toEqual([]);
  await expect(rowNames(page)).toHaveText(['nginx', 'my-app']);
});

test('a legacy numeric refresh preference renders off and never polls', async ({ page, context }) => {
  await context.addCookies([
    { name: 'ro_prefs', value: prefsCookieValue({ refresh: '5' }), domain: '127.0.0.1', path: '/' },
  ]);
  const traffic = updateTraffic(page);
  await page.goto(PODS);

  // The server renders it off (only the literal "Live" presses the toggle) and
  // the client agrees -- no migration, no resurrected poll loop.
  await expect(page.locator(LIVE_TOGGLE)).toHaveAttribute('aria-pressed', 'false');
  expect(await liveState(page)).toBe('off');
  await page.clock.install();
  await page.clock.fastForward(3_600_000);
  await page.waitForTimeout(250);
  expect(traffic).toEqual({ table: [], stream: [] });
});

test('Refresh works on a multi-cluster page, which offers no Live toggle', async ({ page }) => {
  const traffic = updateTraffic(page);
  await page.goto(ALL_CLUSTERS_PODS);

  // `_stream` does not serve the cluster union, so the server renders no
  // toggle there -- but a manual re-fetch always applies.
  await expect(page.locator(LIVE_TOGGLE)).toHaveCount(0);
  await expect(page.locator(REFRESH_NOW)).toBeEnabled();

  const response = await clickRefresh(page);
  expect(response.status()).toBe(200);
  expect(traffic.table).toHaveLength(1);
  expect(traffic.stream).toEqual([]);
  await expect(rowNames(page)).toHaveText(['nginx', 'my-app']);
});

test('a scripted status change flashes the changed cell on the next Refresh morph', async ({
  page,
}) => {
  await page.goto(PODS);
  await expect(rowNames(page)).toHaveText(['nginx', 'my-app']);

  // Mutate the fixture's LIST state per the fakeapi contract (control-applied
  // changes alter subsequent LIST responses): nginx's READY/
  // STATUS/RESTARTS cells change; NAME and AGE stay byte-identical.
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
  await clickRefresh(page);

  // The morph surfaced the change honestly: the STATUS cell flashes
  // ro-cell-changed, the unchanged NAME cell does not, and the untouched
  // my-app row carries no flash at all.
  const nginx = page.locator('tr[data-key="e2e/default/nginx"]');
  await expect(nginx.locator('td:has(span.cell-status)')).toHaveClass(/ro-cell-changed/);
  await expect(nginx.locator('td.cell-name')).not.toHaveClass(/ro-cell-changed/);
  await expect(page.locator('tr[data-key="e2e/default/my-app"] td.ro-cell-changed')).toHaveCount(0);
});

test('the livedot pulses brand while Live is on and is static ghost when off', async ({ page }) => {
  await page.goto(PODS);
  const toggle = page.locator(LIVE_TOGGLE);
  const dot = toggle.locator('.ro-livedot');
  const ghost = await resolvedToken(page, '--text-ghost');
  const brand = await resolvedToken(page, '--brand');
  expect(ghost).not.toBe(brand); // the colour assertions below must tell them apart

  // Off (the default): a static GHOST dot -- no pulse AND no brand green.
  await expect(dot).toHaveCSS('animation-name', 'none');
  await expect(dot).toHaveCSS('background-color', ghost);

  // Live on: brand colour + the pulse, both hanging off the ONE state owner
  // (aria-pressed, rendered at SSR and flipped by the toggle handler).
  await toggle.click();
  await expect(toggle).toHaveAttribute('aria-pressed', 'true');
  await expect(dot).toHaveCSS('animation-name', 'ro-pulse');
  await expect(dot).toHaveCSS('background-color', brand);

  // Back off: the dot drops the pulse AND the green.
  await toggle.click();
  await expect(toggle).toHaveAttribute('aria-pressed', 'false');
  await expect(dot).toHaveCSS('animation-name', 'none');
  await expect(dot).toHaveCSS('background-color', ghost);
});
